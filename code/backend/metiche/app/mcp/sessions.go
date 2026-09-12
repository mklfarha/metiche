package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/app/coordination"
	projectmod "github.com/mklfarha/metiche/backend/core/module/project"
	project_types "github.com/mklfarha/metiche/backend/core/module/project/types"
	sessionmod "github.com/mklfarha/metiche/backend/core/module/session"
	session_types "github.com/mklfarha/metiche/backend/core/module/session/types"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	project_entity "github.com/mklfarha/metiche/backend/entity/project"
	session_entity "github.com/mklfarha/metiche/backend/entity/session"
	"github.com/mklfarha/metiche/backend/enums"
)

// DefaultClaimTTLSeconds is how long a claim survives without a heartbeat.
// Claims are advisory and expiring by design: an agent thinking for twenty
// minutes should drop its holds, not its intent.
const DefaultClaimTTLSeconds = 900

// ─────────────────────────────────────────────
// Tool: start_session
// ─────────────────────────────────────────────

type StartSessionParams struct {
	ProjectKey     string `json:"project_key" jsonschema:"A short stable key for the repository you are working in - the repo name is the obvious choice. Claims are scoped to it, so a team working across several repos does not collide with itself. Created on first use."`
	TeamSlug       string `json:"team_slug,omitempty" jsonschema:"Which team this work is for, by slug. Omit it if you are only on one team; required if you are on several, because guessing would put your work on the wrong board."`
	ClientKey      string `json:"client_key,omitempty" jsonschema:"The same stable client_key you passed to join_team, identifying WHICH of your agents is starting this session. Omit it if you only run one."`
	Branch         string `json:"branch,omitempty" jsonschema:"The git branch you are working on, exactly as git reports it. Self-reported: metiche never runs git."`
	BaseCommit     string `json:"base_commit,omitempty" jsonschema:"The commit you branched from, full or short sha."`
	Goal           string `json:"goal,omitempty" jsonschema:"One sentence on what this whole session is for, written for a teammate skimming the board. Max 280 characters."`
	StatusLine     string `json:"status_line,omitempty" jsonschema:"The 'what I am doing right now' line shown on the board. Max 120 characters. Update it with heartbeat."`
	ProjectName    string `json:"project_name,omitempty" jsonschema:"Human-readable repository name, used only when the project is created on this call."`
	RepoURL        string `json:"repo_url,omitempty" jsonschema:"Repository URL, used only when the project is created on this call."`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Pass a key of your own and retrying this exact call returns the exact same answer instead of starting a second session. Omit it and every call starts a new session."`
}

// StartSession opens one bounded piece of work by one agent on one branch.
//
// Structural: a session is a new lane on the board, so board_revision moves
// and clients re-layout rather than repaint.
func (h *Handler) StartSession(ctx context.Context, _ *mcp.CallToolRequest, args StartSessionParams) (*mcp.CallToolResult, any, error) {
	// v3: the token is the PERSON, so the team and the agent are per-call
	// scope rather than properties of the credential. RequireAgent pins both
	// and checks the membership behind them.
	who, err := h.RequireAgent(ctx, args.TeamSlug, args.ClientKey)
	if err != nil {
		return nil, nil, err
	}
	ag, team, member := who.Agent, who.Team, who.Member

	projectKey := truncate(args.ProjectKey, 512)
	if projectKey == "" {
		return nil, nil, errors.New("project_key is required — claims are scoped to a repository, and an unscoped claim collides with every other repo the team owns")
	}

	idem := strings.TrimSpace(args.IdempotencyKey)
	if idem == "" {
		nonce, err := uuid.NewV4()
		if err != nil {
			return nil, nil, err
		}
		idem = "session_started:" + nonce.String()
	} else {
		// Namespaced by agent so two agents that happen to pick the same key
		// do not replay each other's session.
		idem = "session_started:" + ag.ID.String() + ":" + idem
	}

	var (
		sessionKey  string
		sessionID   uuid.UUID
		projectID   uuid.UUID
		projectMade bool
	)

	response, err := h.commit(ctx, Mutation{
		TeamUUID:       team.ID,
		IdempotencyKey: idem,
		Kind:           enums.EVENT_KIND_SESSION_STARTED,
		Structural:     true,
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(member.ID),
		SubjectKind:    enums.SUBJECT_KIND_SESSION,
		Summary:        fmt.Sprintf("%s started work on %s", ag.Label, projectKey),
		Payload:        payload_entity.EventPayload{Message: nullString(truncate(args.Goal, 280))},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			proj, found, err := h.projectByKey(ctx, tc.Tx, team.ID, projectKey)
			if err != nil {
				return err
			}
			if !found {
				// Created on first use so joining a team and starting work is
				// one call each, not three. The defaults matter more here than
				// anywhere: nuzur generates most of the Go in this repo, and
				// without the generated-code ignore patterns every agent would
				// collide with every other agent on files nobody hand-edits.
				id, err := uuid.NewV4()
				if err != nil {
					return err
				}
				proj = project_entity.Project{
					ID:                   id,
					TeamUUID:             team.ID,
					Key:                  projectKey,
					Name:                 truncate(firstNonEmpty(args.ProjectName, projectKey), 120),
					RepoURL:              nullString(truncate(args.RepoURL, 512)),
					DefaultBranch:        nullString(truncate(args.Branch, 120)),
					IgnorePatterns:       coordination.DefaultIgnorePatterns(),
					HotspotPatterns:      coordination.DefaultHotspotPatterns(),
					CaseInsensitivePaths: true,
					Cadence:              enums.PROJECT_CADENCE_HACKATHON,
					Status:               enums.RECORD_STATUS_ACTIVE,
				}
				if _, err := h.core.Project().Insert(ctx,
					project_types.UpsertRequest{Project: proj}, projectmod.WithSQLTransaction(tc.Tx)); err != nil {
					return err
				}
				projectMade = true
			}
			projectID = proj.ID

			id, err := uuid.NewV4()
			if err != nil {
				return err
			}
			sessionID = id
			// Minted from the sequence this event will carry, which is unique
			// per team by construction — no counter table, no extra round
			// trip, and no way for a concurrent caller to pick the same one.
			sessionKey = tc.Key("S")

			if _, err := h.core.Session().Insert(ctx, session_types.UpsertRequest{
				Session: session_entity.Session{
					ID:              id,
					TeamUUID:        team.ID,
					ProjectUUID:     proj.ID,
					AgentUUID:       ag.ID,
					MemberUUID:      member.ID,
					Key:             sessionKey,
					Branch:          nullString(truncate(args.Branch, 200)),
					BaseCommit:      nullString(truncate(args.BaseCommit, 64)),
					Goal:            nullString(truncate(args.Goal, 280)),
					Status:          enums.SESSION_STATUS_LIVE,
					StatusLine:      nullString(truncate(args.StatusLine, 120)),
					StartedAt:       nullTime(tc.Now),
					LastHeartbeatAt: nullTime(tc.Now),
				},
			}, sessionmod.WithSQLTransaction(tc.Tx)); err != nil {
				return err
			}

			// Written onto the envelope HERE, inside the transaction, not
			// after commit returns. The bytes commit renders are the bytes it
			// stores for a replay, so anything added afterwards would be
			// present in the first answer and missing from the retry.
			env.Key = sessionKey
			env.Note = "heartbeat every ~60s with this session_key, or your claims lapse"
			if projectMade {
				env.Note = fmt.Sprintf("project %q created; ", projectKey) + env.Note
			}
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	_ = sessionID
	_ = projectID
	return jsonResult(response)
}

// ─────────────────────────────────────────────
// Tool: end_session
// ─────────────────────────────────────────────

type EndSessionParams struct {
	SessionKey string `json:"session_key" jsonschema:"The session_key start_session gave you."`
	Outcome    string `json:"outcome,omitempty" jsonschema:"How it ended: succeeded, failed, or abandoned. Defaults to succeeded."`
	Note       string `json:"note,omitempty" jsonschema:"One closing line - for a failure, what went wrong. Max 400 characters."`
}

// EndSession closes a session and releases everything it was holding.
//
// NOT structural, and that is the clearest example of why there are two
// cursors: the lane is already on the board and stays there, so this is a
// repaint (sequence moves) and not a re-layout (board_revision does not).
//
// Releasing the claims here rather than waiting for the TTL is the difference
// between a teammate being unblocked now and being unblocked in fifteen
// minutes.
func (h *Handler) EndSession(ctx context.Context, _ *mcp.CallToolRequest, args EndSessionParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSession(ctx, args.SessionKey)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session

	outcome := enums.SessionOutcome(enums.SESSION_OUTCOME_SUCCEEDED)
	switch strings.ToLower(strings.TrimSpace(args.Outcome)) {
	case "", "succeeded", "success", "done":
		outcome = enums.SESSION_OUTCOME_SUCCEEDED
	case "failed", "failure":
		outcome = enums.SESSION_OUTCOME_FAILED
	case "abandoned", "cancelled", "canceled":
		outcome = enums.SESSION_OUTCOME_ABANDONED
	default:
		return nil, nil, fmt.Errorf("outcome must be one of succeeded, failed, abandoned (got %q)", args.Outcome)
	}

	response, err := h.commit(ctx, Mutation{
		TeamUUID: who.Team.ID,
		// Keyed on the session, so a retry of end_session is inherently a
		// replay rather than a second event.
		IdempotencyKey: "session_ended:" + sess.ID.String(),
		Kind:           enums.EVENT_KIND_SESSION_ENDED,
		Structural:     false,
		ProjectUUID:    uuidPtr(sess.ProjectUUID),
		SessionUUID:    uuidPtr(sess.ID),
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(who.Member.ID),
		SubjectKind:    enums.SUBJECT_KIND_SESSION,
		SubjectUUID:    uuidPtr(sess.ID),
		SubjectKey:     sess.Key,
		Summary:        fmt.Sprintf("%s ended session %s (%s)", ag.Label, sess.Key, outcome.String()),
		Payload: payload_entity.EventPayload{
			Message:        nullString(truncate(args.Note, 400)),
			PreviousStatus: nullString(sess.Status.String()),
			NewStatus:      nullString(enums.SessionStatus(enums.SESSION_STATUS_ENDED).String()),
		},
		Envelope: Envelope{Key: sess.Key},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			n, err := releaseSessionClaims(ctx, tc.Tx, sess.ID, tc.Now, "session ended")
			if err != nil {
				return err
			}
			if n > 0 {
				env.Note = fmt.Sprintf("released %d claim(s)", n)
			}

			// A targeted UPDATE rather than the generated full-row update:
			// writing back every column would clobber a status_line a
			// concurrent heartbeat had just set, and the guard on status makes
			// ending an already-ended session a no-op instead of a resurrection.
			_, err = tc.Tx.ExecContext(ctx,
				"UPDATE `session` SET `status` = ?, `outcome` = ?, `outcome_note` = ?, `ended_at` = ?, `updated_at` = ? "+
					"WHERE `id` = ? AND `status` IN (?, ?)",
				enums.SESSION_STATUS_ENDED, outcome, truncate(args.Note, 400), tc.Now, tc.Now,
				sess.ID.String(), enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE)
			return err
		},
	})
	if err != nil {
		return nil, nil, err
	}

	return jsonResult(response)
}

// releaseSessionClaims drops a session's advisory holds and mirrors the change
// onto claim_path.
//
// Two statements, both bounded by one session's own claims (a handful), both
// index-driven — which is what makes them safe to run inside the team lock.
// claim_path repeats status and expires_at from claim on purpose: the overlap
// scan that runs inside every declaration must be one index scan with no
// joins, and the price of that is keeping the copy honest right here.
func releaseSessionClaims(ctx context.Context, q queryer, sessionUUID uuid.UUID, now time.Time, reason string) (int64, error) {
	res, err := q.ExecContext(ctx,
		"UPDATE `claim` SET `status` = ?, `released_at` = ?, `release_reason` = ?, `updated_at` = ? "+
			"WHERE `session_uuid` = ? AND `status` = ?",
		enums.CLAIM_STATUS_RELEASED, now, truncate(reason, 200), now,
		sessionUUID.String(), enums.CLAIM_STATUS_HELD)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()

	if _, err := q.ExecContext(ctx,
		"UPDATE `claim_path` SET `status` = ?, `updated_at` = ? WHERE `session_uuid` = ? AND `status` = ?",
		enums.CLAIM_STATUS_RELEASED, now, sessionUUID.String(), enums.CLAIM_STATUS_HELD); err != nil {
		return n, err
	}
	return n, nil
}

// ─────────────────────────────────────────────
// Tool: heartbeat
// ─────────────────────────────────────────────

type HeartbeatParams struct {
	SessionKey string `json:"session_key" jsonschema:"The session_key start_session gave you."`
	StatusLine string `json:"status_line,omitempty" jsonschema:"What you are doing RIGHT NOW, one line, max 120 characters - 'rewriting the token refresh in auth.go'. This is what a teammate sees on the board."`
	HeadCommit string `json:"head_commit,omitempty" jsonschema:"Your current HEAD sha, if it moved."`
}

// Heartbeat is the most frequent call in the system and the only one with a
// time obligation: an agent can edit for twenty minutes without calling
// anything else, and its claims would lapse.
//
// IT TAKES NO TEAM LOCK AND WRITES NO EVENT. That is the whole design of it.
// The team row is the single serialization point for the entire team, and
// routing the most frequent call through it would spend the write-path
// headroom on a call that changes nothing anyone needs ordered. Every write
// here is scoped to this one session's own rows, so two agents heartbeating
// at the same instant never touch the same row.
//
// What it does carry is the pending counts — which makes it the cheapest
// possible delivery vehicle for the push-that-cannot-push: a teammate's nudge
// reaches an agent on its next bare heartbeat, with no polling and no event.
func (h *Handler) Heartbeat(ctx context.Context, _ *mcp.CallToolRequest, args HeartbeatParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSession(ctx, args.SessionKey)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session

	now := time.Now().UTC()
	db := h.core.DB()

	var statusLine any
	if s := truncate(args.StatusLine, 120); s != "" {
		statusLine = s
	}
	var headCommit any
	if s := truncate(args.HeadCommit, 64); s != "" {
		headCommit = s
	}

	// Refuse a session that is already closed. Checked on the row we just
	// read rather than on the UPDATE's RowsAffected, because
	// session.last_heartbeat_at is a DATETIME with SECOND granularity: two
	// heartbeats inside one second write identical values, MySQL reports
	// zero rows changed, and treating that as "the session is gone" would
	// reject the most frequent call in the system roughly whenever an agent
	// retried promptly. The WHERE clause below still guards the race.
	switch sess.Status {
	case enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE:
	default:
		return nil, nil, fmt.Errorf(
			"session %s is already %s — call start_session to begin a new one", sess.Key, sess.Status.String())
	}

	// COALESCE so an omitted field keeps its previous value instead of
	// blanking the board, and the explicit status write quietly revives a
	// session that had drifted to stale.
	_, err = db.ExecContext(ctx,
		"UPDATE `session` SET `last_heartbeat_at` = ?, `status_line` = COALESCE(?, `status_line`), "+
			"`head_commit` = COALESCE(?, `head_commit`), `status` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` IN (?, ?)",
		now, statusLine, headCommit, enums.SESSION_STATUS_LIVE, now,
		sess.ID.String(), enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE)
	if err != nil {
		return nil, nil, retryable(err, "recording the heartbeat")
	}

	// Extend the session's claims, capped by hard_expires_at. The cap is what
	// stops a forgotten agent holding half the repo for a day: a heartbeat can
	// push a claim out, but never past the 4h ceiling the claim was born with.
	extended, err := extendSessionClaims(ctx, db, sess.ID, now)
	if err != nil {
		return nil, nil, retryable(err, "extending the claims")
	}

	if _, err := db.ExecContext(ctx,
		"UPDATE `agent` SET `last_seen_at` = ?, `updated_at` = ? WHERE `id` = ?",
		now, now, ag.ID.String()); err != nil {
		return nil, nil, retryable(err, "recording agent liveness")
	}

	pending, err := h.pendingCounts(ctx, db, uuidPtr(sess.ID))
	if err != nil {
		return nil, nil, retryable(err, "counting pending work")
	}

	// Read the cursors without locking. A heartbeat is a reader here; taking
	// the lock to report a number it does not change would serialize the most
	// frequent call in the system behind every write on the team.
	var seq, rev int64
	if err := db.QueryRowContext(ctx,
		"SELECT `sequence`, `board_revision` FROM `team` WHERE `id` = ?", who.Team.ID.String()).
		Scan(&seq, &rev); err != nil {
		return nil, nil, retryable(err, "reading the team cursors")
	}

	env := Envelope{OK: true, Key: sess.Key, Sequence: seq, Revision: rev}.withPending(pending)
	if env.Note == "" && extended > 0 {
		env.Note = fmt.Sprintf("%d claim(s) extended", extended)
	}
	out, err := marshalEnvelope(env)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(out)
}

// extendSessionClaims pushes this session's claims out by their own TTL,
// never past hard_expires_at.
func extendSessionClaims(ctx context.Context, q queryer, sessionUUID uuid.UUID, now time.Time) (int64, error) {
	res, err := q.ExecContext(ctx,
		"UPDATE `claim` SET `expires_at` = LEAST(`hard_expires_at`, DATE_ADD(?, INTERVAL `ttl_seconds` SECOND)), `updated_at` = ? "+
			"WHERE `session_uuid` = ? AND `status` = ?",
		now, now, sessionUUID.String(), enums.CLAIM_STATUS_HELD)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, nil
	}

	// Mirror onto the denormalized copy the detection scan reads, or the scan
	// would keep filtering on a stale expiry.
	if _, err := q.ExecContext(ctx,
		"UPDATE `claim_path` cp JOIN `claim` c ON c.`id` = cp.`claim_uuid` "+
			"SET cp.`expires_at` = c.`expires_at`, cp.`updated_at` = ? "+
			"WHERE cp.`session_uuid` = ? AND cp.`status` = ?",
		now, sessionUUID.String(), enums.CLAIM_STATUS_HELD); err != nil {
		return n, err
	}
	return n, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
