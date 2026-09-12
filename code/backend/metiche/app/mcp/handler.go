// Package mcp is metiche's MCP server: the surface every teammate's coding
// agent calls to say what it is about to do and what it is doing right now.
//
// It mounts INTO the generated API process (see app/rest.go) rather than
// running as its own service, and that co-location is load-bearing rather than
// convenient. Every mutating tool must append a team_event and advance
// team.sequence inside ONE transaction that also holds the team's row lock —
// otherwise the sequence the board resumes from develops holes, and collision
// detection races: two agents both read a clean world and both insert. Only
// in-process access to core.Implementation gives us that transaction.
//
// The shape of the package:
//
//	sequence.go  the transactional core. One function — commit — that every
//	             mutating tool goes through. Lock, idempotency replay, the
//	             caller's snapshot change, the detection hook, the event, the
//	             two cursors.
//	envelope.go  the one response shape every tool returns, and the pending
//	             counts that ride on it. That count is the entire push
//	             mechanism: MCP cannot push.
//	auth.go      join code -> per-agent bearer token; the middleware that
//	             resolves a token to an agent on every request.
//	safetool.go  the wrapper that turns a panicking tool into a failed call
//	             rather than a dead process.
//	server.go    role split, tool registration, annotations.
package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/guregu/null/v6"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/core"
	agentmod "github.com/mklfarha/metiche/backend/core/module/agent"
	agent_types "github.com/mklfarha/metiche/backend/core/module/agent/types"
	membermod "github.com/mklfarha/metiche/backend/core/module/member"
	member_types "github.com/mklfarha/metiche/backend/core/module/member/types"
	projectmod "github.com/mklfarha/metiche/backend/core/module/project"
	project_types "github.com/mklfarha/metiche/backend/core/module/project/types"
	sessionmod "github.com/mklfarha/metiche/backend/core/module/session"
	session_types "github.com/mklfarha/metiche/backend/core/module/session/types"
	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	agent_entity "github.com/mklfarha/metiche/backend/entity/agent"
	member_entity "github.com/mklfarha/metiche/backend/entity/member"
	project_entity "github.com/mklfarha/metiche/backend/entity/project"
	session_entity "github.com/mklfarha/metiche/backend/entity/session"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// ProtocolVersion is stamped on the server implementation so a client can tell
// which tool contract it is talking to. Bump it when the meaning of a tool
// changes, not when one is added.
const ProtocolVersion = "1.0"

// Handler holds everything a tool needs. Nothing more: no HTTP client, no
// outbound anything. metiche must run with no third-party credential, and the
// cheapest way to keep that true is for the server to have nothing to call.
type Handler struct {
	core   *core.Implementation
	logger *zap.Logger

	// lockHold records how long each mutating call HELD the team row lock, and
	// lockWait how long it queued to get it. PLAN.md calls the first the thing
	// that protects the headroom, so both ship from day one rather than after
	// the first incident.
	lockHold *Histogram
	lockWait *Histogram

	// joinLimit and createLimit bound the two tools that answer without a
	// token. metiche has no login by design — the join code is the credential
	// — so "who is allowed to do this" is answered by a rate limit and by
	// plan limits rather than by an account.
	joinLimit   *RateLimiter
	createLimit *RateLimiter

	// detect is the seam the detection layer (wave C) plugs into. It runs
	// INSIDE the team lock, before the event is inserted, so a check and the
	// insert it authorises cannot be separated by another agent's write. See
	// DetectHook.
	detect DetectHook
}

func NewHandler(coreImpl *core.Implementation, logger *zap.Logger) *Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Handler{
		core:        coreImpl,
		logger:      logger,
		lockHold:    NewHistogram(MetricLockHold),
		lockWait:    NewHistogram(MetricLockWait),
		joinLimit:   NewRateLimiter(envInt("METICHE_JOIN_PER_HOUR", defaultJoinTeamPerHour), rateWindow),
		createLimit: NewRateLimiter(envInt("METICHE_CREATE_TEAM_PER_HOUR", defaultCreateTeamPerHour), rateWindow),
	}
}

// SetDetector installs the deterministic detection layer.
//
// THIS IS THE SEAM. Detection must be able to run inside the same transaction
// as the sequence lock and the event insert — check-then-insert outside the
// lock is precisely the TOCTOU race where two agents both observe a clean world
// and both commit. The hook is handed the open transaction and the sequence the
// pending event will carry, and may both query and insert on it.
//
// Called once at wiring time, before any request is served.
func (h *Handler) SetDetector(d DetectHook) { h.detect = d }

// LockHoldStats exposes the team-lock histogram for the metrics endpoint and
// for tests that assert the p99 tripwire.
func (h *Handler) LockHoldStats() HistogramSnapshot { return h.lockHold.Snapshot() }

// LockWaitStats exposes the queueing histogram alongside it.
func (h *Handler) LockWaitStats() HistogramSnapshot { return h.lockWait.Snapshot() }

// ─────────────────────────────────────────────
// Resolvers
//
// Limit: 1 on every generated fetch-by-index is NOT optional. The generated
// query ends in "LIMIT ?, ?" and takes the limit straight from the request, so
// a zero value is a literal LIMIT 0 — the row is found and then discarded, and
// the caller sees "no such member" for a row that plainly exists.
//
// Every read here skips the module cache. The generated modules cache for 30s,
// which is right for a REST read path and wrong here: an agent calls
// start_session and heartbeat seconds apart, and a stale team row would hand
// back a stale sequence — the one number a client uses to detect gaps.
// ─────────────────────────────────────────────

func (h *Handler) teamByID(ctx context.Context, id uuid.UUID) (team_entity.Team, error) {
	res, err := h.core.Team().FetchTeamByID(ctx,
		team_types.FetchTeamByIDRequest{ID: id}, teammod.WithSkipCache())
	if err != nil {
		return team_entity.Team{}, retryable(err, "looking up the team")
	}
	if len(res.Results) == 0 {
		return team_entity.Team{}, fmt.Errorf("team %s no longer exists", id)
	}
	return res.Results[0], nil
}

func (h *Handler) teamByJoinCode(ctx context.Context, code string) (team_entity.Team, error) {
	code = normalizeJoinCode(code)
	if code == "" {
		return team_entity.Team{}, errors.New("join_code is required")
	}
	res, err := h.core.Team().FetchTeamByJoinCode(ctx,
		team_types.FetchTeamByJoinCodeRequest{JoinCode: code, Limit: 1}, teammod.WithSkipCache())
	if err != nil {
		return team_entity.Team{}, retryable(err, "looking up the join code")
	}
	if len(res.Results) == 0 || res.Results[0].Status != enums.RECORD_STATUS_ACTIVE {
		// Deliberately vague and identical for "no such code" and "retired
		// code": a join code is a credential, and a distinguishable answer
		// turns this tool into an oracle for enumerating teams.
		return team_entity.Team{}, errors.New("that join code is not valid for any active team")
	}
	return res.Results[0], nil
}

func (h *Handler) memberByKey(ctx context.Context, tx *sql.Tx, teamUUID uuid.UUID, key string) (member_entity.Member, bool, error) {
	opts := []membermod.Option{membermod.WithSkipCache()}
	if tx != nil {
		opts = append(opts, membermod.WithSQLTransaction(tx))
	}
	res, err := h.core.Member().FetchMemberByTeamUUIDAndKey(ctx,
		member_types.FetchMemberByTeamUUIDAndKeyRequest{TeamUUID: teamUUID, Key: key, Limit: 1}, opts...)
	if err != nil {
		return member_entity.Member{}, false, retryable(err, "looking up the member")
	}
	if len(res.Results) == 0 {
		return member_entity.Member{}, false, nil
	}
	return res.Results[0], true, nil
}

func (h *Handler) agentByClientKey(ctx context.Context, tx *sql.Tx, memberUUID uuid.UUID, clientKey string) (agent_entity.Agent, bool, error) {
	opts := []agentmod.Option{agentmod.WithSkipCache()}
	if tx != nil {
		opts = append(opts, agentmod.WithSQLTransaction(tx))
	}
	res, err := h.core.Agent().FetchAgentByMemberUUIDAndClientKey(ctx,
		agent_types.FetchAgentByMemberUUIDAndClientKeyRequest{MemberUUID: memberUUID, ClientKey: clientKey, Limit: 1}, opts...)
	if err != nil {
		return agent_entity.Agent{}, false, retryable(err, "looking up the agent")
	}
	if len(res.Results) == 0 {
		return agent_entity.Agent{}, false, nil
	}
	return res.Results[0], true, nil
}

func (h *Handler) projectByKey(ctx context.Context, tx *sql.Tx, teamUUID uuid.UUID, key string) (project_entity.Project, bool, error) {
	opts := []projectmod.Option{projectmod.WithSkipCache()}
	if tx != nil {
		opts = append(opts, projectmod.WithSQLTransaction(tx))
	}
	res, err := h.core.Project().FetchProjectByTeamUUIDAndKey(ctx,
		project_types.FetchProjectByTeamUUIDAndKeyRequest{TeamUUID: teamUUID, Key: key, Limit: 1}, opts...)
	if err != nil {
		return project_entity.Project{}, false, retryable(err, "looking up the project")
	}
	if len(res.Results) == 0 {
		return project_entity.Project{}, false, nil
	}
	return res.Results[0], true, nil
}

// sessionByKey resolves an agent-supplied session key, and refuses a session
// that belongs to another agent.
//
// The ownership check is not paranoia: session keys are short and predictable
// (S-7), the endpoint is public, and without it any authenticated agent on any
// team could end any other agent's session by guessing.
func (h *Handler) sessionByKey(ctx context.Context, ag agent_entity.Agent, key string) (session_entity.Session, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return session_entity.Session{}, errors.New("session_key is required")
	}
	res, err := h.core.Session().FetchSessionByTeamUUIDAndKey(ctx,
		session_types.FetchSessionByTeamUUIDAndKeyRequest{TeamUUID: ag.TeamUUID, Key: key, Limit: 1},
		sessionmod.WithSkipCache())
	if err != nil {
		return session_entity.Session{}, retryable(err, "looking up the session")
	}
	if len(res.Results) == 0 {
		return session_entity.Session{}, fmt.Errorf(
			"no session with key %q on this team — call start_session first (session keys are per team and case-sensitive)", key)
	}
	s := res.Results[0]
	if s.AgentUUID != ag.ID {
		return session_entity.Session{}, fmt.Errorf("session %q belongs to another agent", key)
	}
	return s, nil
}

// ─────────────────────────────────────────────
// Small helpers
// ─────────────────────────────────────────────

func nullString(s string) null.String {
	if s == "" {
		return null.String{}
	}
	return null.StringFrom(s)
}

func nullTime(t time.Time) null.Time {
	if t.IsZero() {
		return null.Time{}
	}
	return null.TimeFrom(t)
}

func uuidPtr(id uuid.UUID) *uuid.UUID {
	if id.IsNil() {
		return nil
	}
	v := id
	return &v
}

// truncate clips a string to a column's width in RUNES, not bytes, so a
// multi-byte character is never split into invalid UTF-8 on the way to a
// VARCHAR that counts characters.
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max]))
}

// slugKey turns a display name into the stable per-team member key.
//
// The key, not a uuid, is what makes "join as Ana twice" land on one member
// row: uq_member_team_key is the only thing standing between a restarted agent
// and a duplicate person on the board.
func slugKey(s string, max int) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > max {
		out = strings.Trim(out[:max], "-")
	}
	return out
}

func normalizeJoinCode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
