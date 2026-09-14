package webapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

// A team's run history: every session it has ever run, not only the live and
// stale ones the snapshot carries — and, for one session, the whole story of
// it: what it declared, what it claimed, and which conflicts it was in.
//
// Nothing here selects a column by wildcard. team_event also carries
// response_snapshot (a tool's whole answer, which can hold a token), an
// idempotency_key and a raw payload. None of them is read by this file, and
// the explicit column lists are what keeps it that way.

const (
	defaultSessionsPage = 50
	maxSessionsPage     = 100

	// maxRunRows bounds each list on one run's detail: its intents, its
	// (claim, path) rows and its conflicts.
	maxRunRows = 500
)

type sessionsResponse struct {
	cursors
	Team     teamRef              `json:"team"`
	Sessions []sessionSummaryWire `json:"sessions"`

	// NextCursor is present only when there is an older page. It is opaque:
	// a client hands it back as ?cursor= and never looks inside.
	NextCursor string `json:"next_cursor,omitempty"`
}

// handleSessions serves GET /v1/teams/{slug}/sessions?cursor=C&limit=N.
//
// Newest first, every status (live, stale, ended, abandoned), a page of at
// most maxSessionsPage. Each row is the same session the snapshot and the
// session page describe, plus three counts instead of the live holdings: its
// intents, the distinct paths it claimed, and the conflicts it took part in.
func (a *API) handleSessions(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	limit := intQuery(r, "limit", defaultSessionsPage, 1, maxSessionsPage)

	var after *sessionCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeSessionCursor(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad request", "cursor is not one this endpoint issued")
			return
		}
		after = &c
	}

	var out sessionsResponse
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		sessions, next, err := loadSessionPage(r.Context(), tx, team.UUID, after, limit)
		if err != nil {
			return err
		}
		out.Sessions = sessions
		if next != nil {
			out.NextCursor = next.encode()
		}
		return nil
	})
	if err != nil {
		a.fail(w, r, err, "read the sessions")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- cursor

// sessionCursor is the last row of a page: where it sorts, and its key.
//
// The key rather than the row's id, because ids never leave this package
// (see wire.go). The id is still the tiebreak — the query looks it up from
// the key — so two sessions started in the same second keep one fixed order.
type sessionCursor struct {
	At  string // sessionPageOrder of the row, as the database wrote it
	Key string
}

const (
	sessionCursorVersion = "s1"
	cursorTimeLayout     = "2006-01-02 15:04:05"
)

var errBadCursor = errors.New("bad cursor")

func (c sessionCursor) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(sessionCursorVersion + "|" + c.At + "|" + c.Key))
}

func decodeSessionCursor(raw string) (sessionCursor, error) {
	if len(raw) > 256 {
		return sessionCursor{}, errBadCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return sessionCursor{}, errBadCursor
	}
	parts := strings.SplitN(string(b), "|", 3)
	if len(parts) != 3 || parts[0] != sessionCursorVersion || parts[2] == "" || len(parts[2]) > 32 {
		return sessionCursor{}, errBadCursor
	}
	if _, err := time.Parse(cursorTimeLayout, parts[1]); err != nil {
		return sessionCursor{}, errBadCursor
	}
	return sessionCursor{At: parts[1], Key: parts[2]}, nil
}

// ---------------------------------------------------------------- the page

// sessionPageOrder is what a session sorts by. started_at is nullable in the
// schema and created_at is not, so every row has exactly one place.
//
// A session that starts while somebody is paging sorts AHEAD of any cursor
// they hold, so it can never push a row across a page boundary they have not
// reached: no duplicates and no gaps, only a newer first page next time.
const sessionPageOrder = "COALESCE(s.`started_at`, s.`created_at`)"

// loadSessionPage reads one page and returns the cursor for the next, or nil.
func loadSessionPage(ctx context.Context, tx *sql.Tx, teamUUID string, after *sessionCursor, limit int) ([]sessionSummaryWire, *sessionCursor, error) {
	q := "SELECT " + sessionColumns + ", " + sessionPageOrder + " " + sessionJoins + "WHERE s.`team_uuid` = ?"
	args := []any{teamUUID}
	if after != nil {
		// A cursor whose key no longer resolves makes the tie comparison NULL,
		// which skips only the rows sharing its second; nothing older is lost.
		q += " AND (" + sessionPageOrder + " < ? OR (" + sessionPageOrder + " = ? AND s.`id` > " +
			"(SELECT c.`id` FROM `session` c WHERE c.`team_uuid` = ? AND c.`key` = ?)))"
		args = append(args, after.At, after.At, teamUUID, after.Key)
	}
	q += " ORDER BY " + sessionPageOrder + " DESC, s.`id` LIMIT ?"
	args = append(args, limit+1) // one extra row says whether there is a next page

	out, ids, lastAt, more, err := scanSessionPage(ctx, tx, q, args, limit)
	if err != nil {
		return nil, nil, err
	}
	if err := attachSessionCounts(ctx, tx, teamUUID, ids, out); err != nil {
		return nil, nil, err
	}
	if !more || len(out) == 0 {
		return out, nil, nil
	}
	return out, &sessionCursor{At: lastAt.Format(cursorTimeLayout), Key: out[len(out)-1].Key}, nil
}

// scanSessionPage runs the page query and closes its rows before returning,
// so the count queries that follow can use the same transaction.
func scanSessionPage(ctx context.Context, tx *sql.Tx, q string, args []any, limit int) ([]sessionSummaryWire, []string, time.Time, bool, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, time.Time{}, false, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]sessionSummaryWire, 0, limit)
	ids := make([]string, 0, limit)
	var lastAt time.Time
	for rows.Next() {
		if len(out) == limit {
			return out, ids, lastAt, true, nil
		}
		var at sql.NullTime
		id, core, err := scanSessionCore(rows, &at)
		if err != nil {
			return nil, nil, time.Time{}, false, err
		}
		out = append(out, sessionSummaryWire{sessionCore: core})
		ids = append(ids, id)
		// Formatted later exactly as the driver read it, so the cursor
		// compares against the stored value whatever the connection's loc.
		lastAt = at.Time
	}
	return out, ids, lastAt, false, rows.Err()
}

// attachSessionCounts fills the three counts for a page: three grouped
// queries over at most maxSessionsPage sessions, not three per row.
func attachSessionCounts(ctx context.Context, tx *sql.Tx, teamUUID string, ids []string, out []sessionSummaryWire) error {
	if len(ids) == 0 {
		return nil
	}
	byID := make(map[string]*sessionCountsWire, len(ids))
	for i, id := range ids {
		byID[id] = &out[i].Counts
	}
	in := placeholders(len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, teamUUID)
	for _, id := range ids {
		args = append(args, id)
	}

	count := func(q string, set func(*sessionCountsWire, int64)) error {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				sessionUUID string
				n           int64
			)
			if err := rows.Scan(&sessionUUID, &n); err != nil {
				return err
			}
			if c, ok := byID[sessionUUID]; ok {
				set(c, n)
			}
		}
		return rows.Err()
	}

	if err := count("SELECT i.`session_uuid`, COUNT(*) FROM `intent` i "+
		"WHERE i.`team_uuid` = ? AND i.`session_uuid` IN ("+in+") GROUP BY i.`session_uuid`",
		func(c *sessionCountsWire, n int64) { c.Intents = n }); err != nil {
		return err
	}
	if err := count("SELECT cp.`session_uuid`, COUNT(DISTINCT cp.`pattern_norm`) FROM `claim_path` cp "+
		"JOIN `claim` c ON c.`id` = cp.`claim_uuid` "+
		"WHERE c.`team_uuid` = ? AND cp.`session_uuid` IN ("+in+") GROUP BY cp.`session_uuid`",
		func(c *sessionCountsWire, n int64) { c.ClaimedPaths = n }); err != nil {
		return err
	}
	if err := count("SELECT p.`session_uuid`, COUNT(DISTINCT p.`conflict_uuid`) FROM `conflict_participant` p "+
		"WHERE p.`team_uuid` = ? AND p.`session_uuid` IN ("+in+") GROUP BY p.`session_uuid`",
		func(c *sessionCountsWire, n int64) { c.Conflicts = n }); err != nil {
		return err
	}
	// Served by idx_session_parent: the page's sessions as parents.
	return count("SELECT k.`parent_session_uuid`, COUNT(*) FROM `session` k "+
		"WHERE k.`team_uuid` = ? AND k.`parent_session_uuid` IN ("+in+") GROUP BY k.`parent_session_uuid`",
		func(c *sessionCountsWire, n int64) { c.Subagents = n })
}

// ---------------------------------------------------------------- one run

const runIntentsQuery = "SELECT i.`key`, i.`summary`, i.`kind`, i.`status`, i.`external_ref`, i.`revision`, " +
	"i.`declared_at`, i.`started_at`, i.`ended_at` " +
	"FROM `intent` i WHERE i.`team_uuid` = ? AND i.`session_uuid` = ? " +
	"ORDER BY COALESCE(i.`declared_at`, i.`created_at`), i.`key` LIMIT ?"

const runClaimsQuery = "SELECT c.`key`, c.`mode`, c.`status`, c.`created_at`, c.`expires_at`, c.`released_at`, " +
	"cp.`pattern_norm` " +
	"FROM `claim` c JOIN `claim_path` cp ON cp.`claim_uuid` = c.`id` " +
	"WHERE c.`team_uuid` = ? AND c.`session_uuid` = ? " +
	"ORDER BY c.`created_at`, c.`key`, cp.`pattern_norm` LIMIT ?"

// loadRunHistory reads what one session did over its whole life, whatever
// its status now: every intent, every claim with its paths, and every
// conflict it was a participant in, settled ones with how they were settled.
//
// It is deliberately separate from attachIntents and attachClaims, which
// answer "what does this session hold RIGHT NOW" and are empty for a session
// that has ended.
func loadRunHistory(ctx context.Context, tx *sql.Tx, teamUUID, sessionUUID string) (runHistoryWire, error) {
	out := runHistoryWire{Intents: []runIntentWire{}, Claims: []runClaimWire{}}

	if err := func() error {
		rows, err := tx.QueryContext(ctx, runIntentsQuery, teamUUID, sessionUUID, maxRunRows)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				in                         runIntentWire
				kind, status, revision     sql.NullInt64
				externalRef                sql.NullString
				declared, started, endedAt sql.NullTime
			)
			if err := rows.Scan(&in.Key, &in.Summary, &kind, &status, &externalRef, &revision,
				&declared, &started, &endedAt); err != nil {
				return err
			}
			if kind.Valid && enums.IntentKind(kind.Int64) != enums.INTENT_KIND_INVALID {
				in.Kind = enums.IntentKind(kind.Int64).String()
			}
			in.Status = enums.IntentStatus(status.Int64).String()
			in.ExternalRef = externalRef.String
			in.Revision = revision.Int64
			in.DeclaredAt, in.StartedAt, in.EndedAt = rfc3339(declared), rfc3339(started), rfc3339(endedAt)
			out.Intents = append(out.Intents, in)
		}
		return rows.Err()
	}(); err != nil {
		return runHistoryWire{}, err
	}

	if err := func() error {
		rows, err := tx.QueryContext(ctx, runClaimsQuery, teamUUID, sessionUUID, maxRunRows)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		// One row per (claim, path); the ORDER BY keeps a claim's paths
		// together so they fold into one entry.
		for rows.Next() {
			var (
				key                         string
				mode, status                sql.NullInt64
				created, expires, releaseAt sql.NullTime
				pattern                     string
			)
			if err := rows.Scan(&key, &mode, &status, &created, &expires, &releaseAt, &pattern); err != nil {
				return err
			}
			if n := len(out.Claims); n == 0 || out.Claims[n-1].Key != key {
				out.Claims = append(out.Claims, runClaimWire{
					claimWire: claimWire{
						Key:       key,
						Mode:      enums.ClaimMode(mode.Int64).String(),
						ExpiresAt: rfc3339(expires),
						Paths:     []string{},
					},
					Status:     enums.ClaimStatus(status.Int64).String(),
					ClaimedAt:  rfc3339(created),
					ReleasedAt: rfc3339(releaseAt),
				})
			}
			last := &out.Claims[len(out.Claims)-1]
			last.Paths = append(last.Paths, pattern)
		}
		return rows.Err()
	}(); err != nil {
		return runHistoryWire{}, err
	}

	conflicts, err := loadSessionConflicts(ctx, tx, teamUUID, sessionUUID, maxRunRows)
	if err != nil {
		return runHistoryWire{}, err
	}
	out.Conflicts = conflicts
	return out, nil
}
