package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	sessionmod "github.com/mklfarha/metiche/backend/core/module/session"
	session_types "github.com/mklfarha/metiche/backend/core/module/session/types"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
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
	ProjectKey     string `json:"project_key,omitempty" jsonschema:"The name of the git root folder: the basename of 'git rev-parse --show-toplevel', run in the repository - never a name you make up, and never your working directory's name when you were started above or inside the repo. Optional when repo_url is given (derived from it); required when there is no remote. Claims are scoped to the project, so every agent in one repository must land on the same one. Created on first use."`
	TeamSlug       string `json:"team_slug,omitempty" jsonschema:"Which team this work is for, by slug. Omit it if you are only on one team; required if you are on several, because guessing would put your work on the wrong board."`
	ClientKey      string `json:"client_key,omitempty" jsonschema:"Your token identifies this agent. You never need client_key; omit it. Several terminals of one client are several sessions of one agent — start_session tells you about your other live sessions."`
	Branch         string `json:"branch,omitempty" jsonschema:"The output of 'git branch --show-current', run in the repository. Omit it on a detached HEAD. Self-reported: metiche never runs git."`
	BaseCommit     string `json:"base_commit,omitempty" jsonschema:"The commit you branched from, full or short sha."`
	Goal           string `json:"goal,omitempty" jsonschema:"One sentence on what this whole session is for, written for a teammate skimming the board. Max 280 characters."`
	StatusLine     string `json:"status_line,omitempty" jsonschema:"The 'what I am doing right now' line shown on the board. Max 120 characters. Update it with heartbeat."`
	ProjectName    string `json:"project_name,omitempty" jsonschema:"Human-readable repository name, used only when the project is created on this call."`
	RepoURL        string `json:"repo_url,omitempty" jsonschema:"The output of 'git remote get-url origin', run in the repository; omit it only if there is no remote. This is how metiche knows two agents are in the SAME repository: https and ssh forms, .git and letter case are normalized away, and an existing project for this repository is used whatever project_key says. Credentials in the URL are stripped and never stored."`
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

	// The repository is the project's identity; the key is only its name.
	// Canonicalized here, before anything is logged or written, so a
	// credential in the remote never gets further than this line. A remote
	// with no host (a local path) has no canonical URL and identifies nothing:
	// the key alone decides, as it always did.
	repoURL := truncate(canonicalRepoURL(args.RepoURL), 512)
	projectKey := truncate(args.ProjectKey, 512)
	keyDerived := false
	if projectKey == "" && repoURL != "" {
		projectKey = truncate(projectKeyFromRepoURL(repoURL), 512)
		keyDerived = projectKey != ""
	}
	if projectKey == "" {
		return nil, nil, errors.New("project_key is required when there is no repo_url — claims are scoped to a repository, and an unscoped claim collides with every other repo the team owns; " +
			"send repo_url (git remote get-url origin) or project_key (the basename of git rev-parse --show-toplevel)")
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
		sessionKey string
		sessionID  uuid.UUID
		projectID  uuid.UUID
	)

	response, err := h.commit(ctx, Mutation{
		TeamUUID:       team.ID,
		IdempotencyKey: idem,
		Kind:           enums.EVENT_KIND_SESSION_STARTED,
		Structural:     true,
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(member.ID),
		SubjectKind:    enums.SUBJECT_KIND_SESSION,
		// No Summary here: it names the project, and which project is only
		// decided inside Apply. Apply sets tc.Summary.
		Payload: payload_entity.EventPayload{Message: nullString(truncate(args.Goal, 280))},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			// Counted HERE, under the team lock, so the cap is exact: two
			// terminals racing to open the ninth session serialize on the team
			// row, and the second one sees the first one's insert.
			open, err := openSessionsOfAgent(ctx, tc.Tx, team.ID, ag.ID, maxLiveSessionsPerAgent)
			if err != nil {
				return err
			}
			if len(open) >= maxLiveSessionsPerAgent {
				return fmt.Errorf(
					"this agent already has %d open sessions, the most one agent may hold: %s — "+
						"end the ones that are not this terminal with end_session, then call start_session again",
					len(open), describeOpenSessions(open, tc.Now))
			}

			// Resolved under the lock this Apply already holds, so two agents
			// racing to open the first session in one repository cannot each
			// create a project for it. See resolveProject for the order.
			in := projectInput{
				Key:        projectKey,
				KeyDerived: keyDerived,
				RepoURL:    repoURL,
				Name:       args.ProjectName,
				Branch:     args.Branch,
			}
			resolved, err := h.resolveProject(ctx, tc, team.ID, in)
			if err != nil {
				return err
			}
			proj := resolved.Project
			projectID = proj.ID
			// The board's timeline names the project the session is on, not
			// the name the agent happened to send for it.
			tc.Summary = fmt.Sprintf("%s started work on %s", ag.Label, proj.Key)

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
			// The note about this agent's other sessions is built here too, from
			// the rows counted under the lock, so a replay repeats the world as
			// it was when the session started rather than as it is now.
			env.Key = sessionKey
			env.ProjectKey = proj.Key
			env.Note = "heartbeat every ~60s with this session_key, or your claims lapse"
			if len(open) > 0 {
				env.Note = otherSessionsNote(open, sessionKey, tc.Now) + "; " + env.Note
			}
			if resolved.Created {
				env.Note = newProjectNote(proj.Key, resolved.Others, sessionKey) + "; " + env.Note
			}
			if override := keyOverrideNote(in, resolved); override != "" {
				env.Note = override + "; " + env.Note
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

// maxLiveSessionsPerAgent bounds how many sessions one agent may hold open
// (live or stale) on one team at once.
//
// Every terminal of a client shares one token, so a terminal is a SESSION and
// an agent may legitimately hold several. What it must not do is pile them up:
// a terminal that vanished without end_session leaves a lane on the board
// until the sweeper abandons it, and a client restarted in a loop would fill
// the board with ghosts. Eight is more terminals than a person drives, and few
// enough that the refusal can list every one of them.
const maxLiveSessionsPerAgent = 8

// openSession is one of an agent's sessions that has not ended.
type openSession struct {
	Key       string
	Branch    string
	Status    enums.SessionStatus
	StartedAt time.Time
}

// openSessionsOfAgent lists an agent's live and stale sessions on one team,
// oldest first, at most limit of them.
//
// Scoped to the team because session keys are: S-7 names a session only
// within its team, so a key from another team would be one end_session could
// not act on. It runs inside the team lock, so it is bounded twice — by the
// team's open sessions (idx_session_liveness) and by limit.
func openSessionsOfAgent(ctx context.Context, tx *sql.Tx, teamUUID, agentUUID uuid.UUID, limit int) ([]openSession, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT `key`, COALESCE(`branch`, ''), `status`, COALESCE(`started_at`, `created_at`) FROM `session` "+
			"WHERE `team_uuid` = ? AND `status` IN (?, ?) AND `agent_uuid` = ? "+
			"ORDER BY COALESCE(`started_at`, `created_at`) ASC, CHAR_LENGTH(`key`) ASC, `key` ASC LIMIT ?",
		teamUUID.String(), enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE, agentUUID.String(), limit)
	if err != nil {
		return nil, retryable(err, "counting this agent's open sessions")
	}
	defer func() { _ = rows.Close() }()

	var out []openSession
	for rows.Next() {
		var s openSession
		var status int64
		if err := rows.Scan(&s.Key, &s.Branch, &status, &s.StartedAt); err != nil {
			return nil, err
		}
		s.Status = enums.SessionStatus(status)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "counting this agent's open sessions")
	}
	return out, nil
}

// describeOpenSessions renders "S-7 live on feat/a (12m ago), S-8 stale on
// feat/b (3h ago)".
func describeOpenSessions(open []openSession, now time.Time) string {
	parts := make([]string, 0, len(open))
	for _, s := range open {
		where := "with no branch"
		if s.Branch != "" {
			where = "on " + s.Branch
		}
		parts = append(parts, fmt.Sprintf("%s %s %s (%s)", s.Key, s.Status.String(), where, sessionAge(now.Sub(s.StartedAt))))
	}
	return strings.Join(parts, ", ")
}

// otherSessionsNote is what a second terminal is told about the first.
func otherSessionsNote(open []openSession, thisKey string, now time.Time) string {
	which := "one of those"
	if len(open) == 1 {
		which = open[0].Key
	}
	return fmt.Sprintf("you already have %s; this is %s — if %s was this terminal's earlier run, end_session it",
		describeOpenSessions(open, now), thisKey, which)
}

// sessionAge is a coarse, model-readable age. Coarse on purpose: the note is
// stored for replay, and a precise age would be precisely wrong on a retry.
func sessionAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		// Not "<1m": encoding/json escapes '<', and the note is read by a model.
		return "under a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
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
