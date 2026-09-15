package webapi

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

// What was entangled over a past window: the sessions that were active in it,
// every path each of them held during it with when the hold began and ended,
// and the conflicts that were raised or still open in it.
//
// It returns intervals, not a verdict. Whether two sessions in the same area
// held it AT THE SAME TIME is the board's to compute from these intervals,
// and a graph that drew every pair in one area as a crossing would invent
// overlaps that never happened. What became a real collision is in
// Conflicts, straight from the conflict table.
//
// # Where the intervals come from, and how far to trust them
//
//	held from   claim_path.created_at: the row is written when the path is
//	            claimed (declare_intent, add_paths).
//	held until  still held: LEAST(claim_path.expires_at, claim.hard_expires_at),
//	            the lazy expiry every detection already trusts; the hold is
//	            reported open-ended while that is in the future.
//	            no longer held: LEAST(claim_path.updated_at, expires_at,
//	            hard_expires_at). Every writer that moves a path off "held" —
//	            drop_paths, done, end_session, the sweeper's expiry and
//	            abandonment — stamps updated_at in the same statement, and
//	            nothing touches a path after it leaves "held" (the heartbeat's
//	            expiry mirror is conditional on status = held). The LEAST
//	            covers the sweeper running after a TTL had already lapsed: the
//	            hold ended at its expiry, not at the sweep.
//
// Both ends are DATETIME, to the second. A team with retention on has
// deleted closed sessions older than its policy, and their claims with them,
// so a window reaching past that shows less than happened.
//
// This endpoint takes a window instead of a cursor. A graph is drawn whole; a
// page of one would draw a false picture of the rest. The window is one of
// two fixed values, the session count is bounded by limit, the holds and the
// conflicts by fixed caps, and Truncated says when any bound was hit.

const (
	defaultGraphSessions = 100
	maxGraphSessions     = 200
	maxGraphHolds        = 2000
	maxGraphConflicts    = 200
)

// graphWindows are the windows ?window= accepts.
var graphWindows = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

type graphHoldWire struct {
	SessionKey string `json:"session_key"`
	ClaimKey   string `json:"claim_key"`
	Mode       string `json:"mode"`
	Path       string `json:"path"`
	// Status is the path's status, with a hold whose expiry has passed
	// reported as expired whether or not the sweeper has got to it.
	Status   string  `json:"status"`
	HeldFrom *string `json:"held_from,omitempty"`
	// HeldUntil is absent while the path is still held.
	HeldUntil *string `json:"held_until,omitempty"`
}

type graphResponse struct {
	cursors
	Team teamRef `json:"team"`

	Window string `json:"window"`
	From   string `json:"from"`
	To     string `json:"to"`

	// Sessions active in the window, most recently started first.
	Sessions []sessionCore `json:"sessions"`
	// Holds are the paths those sessions held during the window, oldest
	// first.
	Holds []graphHoldWire `json:"holds"`
	// Conflicts raised before the window ended that were still open or ended
	// inside it, worst first.
	Conflicts []conflictWire `json:"conflicts"`

	Truncated bool `json:"truncated"`
}

// graphSessionStart and graphSessionEnd bound a session's life. A live or
// stale session is still going; an ended or abandoned one ended at ended_at,
// or failing that its last sign of life.
const (
	graphSessionStart = "COALESCE(s.`started_at`, s.`created_at`)"
	graphSessionEnd   = "COALESCE(s.`ended_at`, s.`last_heartbeat_at`, s.`started_at`, s.`created_at`)"
)

// graphHoldEnd is when a path stopped being held; see the file comment. Its
// one placeholder is the held status.
const graphHoldEnd = "(CASE WHEN cp.`status` = ? THEN LEAST(cp.`expires_at`, c.`hard_expires_at`) " +
	"ELSE LEAST(cp.`updated_at`, cp.`expires_at`, c.`hard_expires_at`) END)"

// handleGraph serves GET /v1/teams/{slug}/graph?window=24h|7d&limit=N.
func (a *API) handleGraph(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	window := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("window")))
	if window == "" {
		window = "24h"
	}
	span, ok := graphWindows[window]
	if !ok {
		writeProblem(w, http.StatusBadRequest, "bad request", "window must be 24h or 7d")
		return
	}
	limit := intQuery(r, "limit", defaultGraphSessions, 1, maxGraphSessions)
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-span)

	out := graphResponse{Window: window, From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)}
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision
		return loadGraphWindow(r.Context(), tx, team.UUID, from, to, limit, &out)
	})
	if err != nil {
		a.fail(w, r, err, "read the graph")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// loadGraphWindow fills out's sessions, holds and conflicts for [from, to].
func loadGraphWindow(ctx context.Context, tx *sql.Tx, teamUUID string, from, to time.Time, limit int, out *graphResponse) error {
	ids, byID, err := loadGraphSessions(ctx, tx, teamUUID, from, to, limit, out)
	if err != nil {
		return err
	}
	if err := loadGraphHolds(ctx, tx, teamUUID, ids, byID, from, to, out); err != nil {
		return err
	}
	conflicts, err := loadConflictsWhere(ctx, tx, teamUUID,
		" AND COALESCE(c.`first_detected_at`, c.`created_at`) <= ? AND (c.`status` IN (?,?,?) OR "+conflictHistoryOrder+" >= ?)",
		[]any{to, enums.CONFLICT_STATUS_OPEN, enums.CONFLICT_STATUS_ACKNOWLEDGED, enums.CONFLICT_STATUS_RESOLVING, from},
		maxGraphConflicts+1)
	if err != nil {
		return err
	}
	if len(conflicts) > maxGraphConflicts {
		conflicts, out.Truncated = conflicts[:maxGraphConflicts], true
	}
	out.Conflicts = conflicts
	return nil
}

func loadGraphSessions(ctx context.Context, tx *sql.Tx, teamUUID string, from, to time.Time, limit int, out *graphResponse) ([]string, map[string]string, error) {
	q := "SELECT " + sessionColumns + " " + sessionJoins +
		"WHERE s.`team_uuid` = ? AND " + graphSessionStart + " <= ? " +
		"AND (s.`status` IN (?,?) OR " + graphSessionEnd + " >= ?) " +
		"ORDER BY " + graphSessionStart + " DESC, s.`id` LIMIT ?"
	rows, err := tx.QueryContext(ctx, q, teamUUID, to,
		enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE, from, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	out.Sessions = []sessionCore{}
	ids := []string{}
	byID := map[string]string{}
	for rows.Next() {
		if len(out.Sessions) == limit {
			out.Truncated = true
			break
		}
		id, core, err := scanSessionCore(rows)
		if err != nil {
			return nil, nil, err
		}
		out.Sessions = append(out.Sessions, core)
		ids = append(ids, id)
		byID[id] = core.Key
	}
	return ids, byID, rows.Err()
}

func loadGraphHolds(ctx context.Context, tx *sql.Tx, teamUUID string, ids []string, byID map[string]string, from, to time.Time, out *graphResponse) error {
	out.Holds = []graphHoldWire{}
	if len(ids) == 0 {
		return nil
	}
	held := enums.CLAIM_STATUS_HELD
	q := "SELECT cp.`session_uuid`, c.`key`, c.`mode`, cp.`pattern_norm`, cp.`status`, cp.`created_at`, " + graphHoldEnd + " " +
		"FROM `claim_path` cp JOIN `claim` c ON c.`id` = cp.`claim_uuid` " +
		"WHERE c.`team_uuid` = ? AND cp.`session_uuid` IN (" + placeholders(len(ids)) + ") " +
		"AND cp.`created_at` <= ? AND " + graphHoldEnd + " >= ? " +
		"ORDER BY cp.`created_at`, c.`key`, cp.`pattern_norm` LIMIT ?"
	args := make([]any, 0, len(ids)+6)
	args = append(args, held, teamUUID)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, to, held, from, maxGraphHolds+1)

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if len(out.Holds) == maxGraphHolds {
			out.Truncated = true
			break
		}
		var (
			sessionUUID, claimKey, pattern string
			mode, status                   sql.NullInt64
			created, until                 sql.NullTime
		)
		if err := rows.Scan(&sessionUUID, &claimKey, &mode, &pattern, &status, &created, &until); err != nil {
			return err
		}
		h := graphHoldWire{
			SessionKey: byID[sessionUUID], ClaimKey: claimKey, Path: pattern,
			Mode:     enums.ClaimMode(mode.Int64).String(),
			Status:   enums.ClaimStatus(status.Int64).String(),
			HeldFrom: rfc3339(created),
		}
		stillHeld := enums.ClaimStatus(status.Int64) == enums.CLAIM_STATUS_HELD
		switch {
		case stillHeld && until.Valid && until.Time.After(to):
			// Open-ended: held now, and for as long as its session renews it.
		case stillHeld:
			h.Status = enums.ClaimStatus(enums.CLAIM_STATUS_EXPIRED).String()
			h.HeldUntil = rfc3339(until)
		default:
			h.HeldUntil = rfc3339(until)
		}
		out.Holds = append(out.Holds, h)
	}
	return rows.Err()
}
