package webapi

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

// maxConflicts bounds the conflict list on a snapshot. A board shows the worst
// ones; a team with three hundred open conflicts has a different problem and a
// dedicated page for it.
const maxConflicts = 200

// snapshotResponse is what a board loads before it starts streaming.
type snapshotResponse struct {
	cursors
	Team      teamRef        `json:"team"`
	Sessions  []sessionWire  `json:"sessions"`
	Conflicts []conflictWire `json:"conflicts"`
	Counts    snapshotCounts `json:"counts"`
}

type snapshotCounts struct {
	LiveSessions  int `json:"live_sessions"`
	OpenConflicts int `json:"open_conflicts"`
	HeldClaims    int `json:"held_claims"`
}

// handleSnapshot serves GET /v1/teams/{slug}.
//
// ONE coherent read, not a dozen. The board's whole first paint — who is live,
// what each of them is doing, what they are holding, what is on fire, and the
// two cursors — comes back from a single read-only REPEATABLE READ
// transaction, so every part of it describes the same instant.
//
// The sequence it returns is the cursor to hand straight to
// GET /v1/teams/{slug}/stream?after=N. Because the snapshot was taken inside
// one MVCC snapshot, that number is exactly the boundary: everything at or
// below it is already drawn, everything above it arrives on the stream. No
// gap, no double-application.
func (a *API) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	var out snapshotResponse
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		sessions, byUUID, err := loadLiveSessions(r.Context(), tx, team.UUID)
		if err != nil {
			return err
		}
		if err := attachIntents(r.Context(), tx, team.UUID, byUUID); err != nil {
			return err
		}
		held, err := attachClaims(r.Context(), tx, team.UUID, byUUID)
		if err != nil {
			return err
		}

		conflicts, err := loadConflicts(r.Context(), tx, team.UUID, openConflictStatuses, maxConflicts)
		if err != nil {
			return err
		}

		out.Sessions = sessions
		out.Conflicts = conflicts
		out.Counts = snapshotCounts{
			LiveSessions:  len(sessions),
			OpenConflicts: len(conflicts),
			HeldClaims:    held,
		}
		return nil
	})
	if err != nil {
		a.fail(w, r, err, "read the team")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// openConflictStatuses is what "open" means on the board: anything a person
// still has to deal with. Resolved and dismissed are history, expired is
// noise.
var openConflictStatuses = []any{
	enums.CONFLICT_STATUS_OPEN,
	enums.CONFLICT_STATUS_ACKNOWLEDGED,
	enums.CONFLICT_STATUS_RESOLVING,
}

const liveSessionsQuery = "SELECT s.`id`, s.`key`, s.`branch`, s.`goal`, s.`status`, s.`status_line`, " +
	"s.`started_at`, s.`last_heartbeat_at`, s.`ended_at`, s.`outcome`, " +
	"p.`key`, m.`key`, m.`display_name`, a.`label`, a.`client_kind`, ci.`key` " +
	"FROM `session` s " +
	"JOIN `project` p ON p.`id` = s.`project_uuid` " +
	"JOIN `member` m ON m.`id` = s.`member_uuid` " +
	"JOIN `agent` a ON a.`id` = s.`agent_uuid` " +
	"LEFT JOIN `intent` ci ON ci.`id` = s.`current_intent_uuid` " +
	"WHERE s.`team_uuid` = ? AND s.`status` IN (?, ?) " +
	"ORDER BY m.`display_name`, a.`label`, s.`key`"

// loadLiveSessions returns the board's lanes: one per session that is live or
// stale.
//
// Stale is included deliberately. A session whose heartbeat has lapsed is the
// single most useful thing on the board — it is an agent that stopped talking
// while still holding claims — and dropping it would make the holder of a
// contended path silently disappear.
//
// The second return value indexes the slice by session uuid so the follow-up
// queries can attach to it without a second pass over the rows. It is the only
// place a uuid appears, and it never leaves this file.
func loadLiveSessions(ctx context.Context, tx *sql.Tx, teamUUID string) ([]sessionWire, map[string]*sessionWire, error) {
	rows, err := tx.QueryContext(ctx, liveSessionsQuery, teamUUID,
		enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	// The slice is allocated up front and indexed by pointer, so the map and
	// the response are the same objects rather than two copies to keep in sync.
	out := make([]sessionWire, 0, 16)
	ids := make([]string, 0, 16)
	for rows.Next() {
		var (
			id                                                      string
			s                                                       sessionWire
			branch, goal, statusLine                                sql.NullString
			started, heartbeat, ended                               sql.NullTime
			status, outcome                                         sql.NullInt64
			projectKey, memberKey, memberName, agentLabel, clientKd sql.NullString
			currentIntent                                           sql.NullString
		)
		if err := rows.Scan(&id, &s.Key, &branch, &goal, &status, &statusLine,
			&started, &heartbeat, &ended, &outcome,
			&projectKey, &memberKey, &memberName, &agentLabel, &clientKd, &currentIntent); err != nil {
			return nil, nil, err
		}
		s.Branch, s.Goal, s.StatusLine = branch.String, goal.String, statusLine.String
		s.Status = enums.SessionStatus(status.Int64).String()
		if outcome.Valid && enums.SessionOutcome(outcome.Int64) != enums.SESSION_OUTCOME_INVALID {
			s.Outcome = enums.SessionOutcome(outcome.Int64).String()
		}
		s.StartedAt, s.LastHeartbeatAt, s.EndedAt = rfc3339(started), rfc3339(heartbeat), rfc3339(ended)
		s.ProjectKey, s.MemberKey, s.MemberName = projectKey.String, memberKey.String, memberName.String
		s.AgentLabel, s.ClientKind = agentLabel.String, clientKd.String
		s.CurrentIntentKey = currentIntent.String
		s.Intents = []intentWire{}
		s.Claims = []claimWire{}
		out = append(out, s)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	byUUID := make(map[string]*sessionWire, len(out))
	for i := range out {
		byUUID[ids[i]] = &out[i]
	}
	return out, byUUID, nil
}

const liveIntentsQuery = "SELECT i.`session_uuid`, i.`key`, i.`summary`, i.`kind`, i.`status`, " +
	"i.`external_ref`, i.`revision`, i.`declared_at` " +
	"FROM `intent` i JOIN `session` s ON s.`id` = i.`session_uuid` " +
	"WHERE i.`team_uuid` = ? AND s.`status` IN (?, ?) AND i.`status` IN (?, ?) " +
	"ORDER BY i.`declared_at`, i.`key`"

// attachIntents hangs each live session's declared and active intents off it.
//
// One query for the whole board, not one per lane: five people with two agents
// each is ten sessions, and ten round trips to draw one screen is how a board
// that felt instant at three people stops feeling instant at ten.
func attachIntents(ctx context.Context, tx *sql.Tx, teamUUID string, byUUID map[string]*sessionWire) error {
	if len(byUUID) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, liveIntentsQuery, teamUUID,
		enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE,
		enums.INTENT_STATUS_DECLARED, enums.INTENT_STATUS_ACTIVE)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			sessionUUID       string
			in                intentWire
			kind, status      sql.NullInt64
			externalRef       sql.NullString
			declared          sql.NullTime
			revision          sql.NullInt64
			summaryFromColumn string
		)
		if err := rows.Scan(&sessionUUID, &in.Key, &summaryFromColumn, &kind, &status,
			&externalRef, &revision, &declared); err != nil {
			return err
		}
		in.Summary = summaryFromColumn
		if kind.Valid && enums.IntentKind(kind.Int64) != enums.INTENT_KIND_INVALID {
			in.Kind = enums.IntentKind(kind.Int64).String()
		}
		in.Status = enums.IntentStatus(status.Int64).String()
		in.ExternalRef = externalRef.String
		in.Revision = revision.Int64
		in.DeclaredAt = rfc3339(declared)

		if s, ok := byUUID[sessionUUID]; ok {
			s.Intents = append(s.Intents, in)
		}
	}
	return rows.Err()
}

const heldClaimsQuery = "SELECT c.`session_uuid`, c.`key`, c.`mode`, c.`expires_at`, cp.`pattern_norm` " +
	"FROM `claim` c " +
	"JOIN `claim_path` cp ON cp.`claim_uuid` = c.`id` " +
	"WHERE c.`team_uuid` = ? AND c.`status` = ? AND c.`expires_at` > NOW() " +
	"ORDER BY c.`created_at`, c.`key`, cp.`pattern_norm`"

// attachClaims hangs each session's still-valid holds off it and returns how
// many distinct claims were attached.
//
// expires_at > NOW() is not belt-and-braces over the sweeper — it is the
// authority. PLAN.md is explicit that the lazy filter decides what is held and
// the sweeper exists only so the expiry becomes VISIBLE. A board that trusted
// status alone would draw claims that detection itself no longer counts, and
// would show a file as contended minutes after it was free.
func attachClaims(ctx context.Context, tx *sql.Tx, teamUUID string, byUUID map[string]*sessionWire) (int, error) {
	if len(byUUID) == 0 {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, heldClaimsQuery, teamUUID, enums.CLAIM_STATUS_HELD)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	// One row per (claim, path); the ORDER BY groups a claim's paths together
	// so they fold into one claimWire without a map.
	held := 0
	type cursor struct {
		session *sessionWire
		key     string
	}
	var current cursor
	for rows.Next() {
		var (
			sessionUUID string
			key         string
			mode        sql.NullInt64
			expires     sql.NullTime
			pattern     string
		)
		if err := rows.Scan(&sessionUUID, &key, &mode, &expires, &pattern); err != nil {
			return 0, err
		}
		s, ok := byUUID[sessionUUID]
		if !ok {
			continue
		}
		if current.session != s || current.key != key {
			s.Claims = append(s.Claims, claimWire{
				Key:       key,
				Mode:      enums.ClaimMode(mode.Int64).String(),
				ExpiresAt: rfc3339(expires),
				Paths:     []string{},
			})
			current = cursor{session: s, key: key}
			held++
		}
		last := &s.Claims[len(s.Claims)-1]
		last.Paths = append(last.Paths, pattern)
	}
	return held, rows.Err()
}
