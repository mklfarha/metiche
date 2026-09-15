package webapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

// A team's conflict history: every conflict that is over, newest first, a page
// at a time.
//
// It is a sibling of GET /v1/teams/{slug}/conflicts rather than a cursor bolted
// onto it, because the two answer different questions and sort differently.
// /conflicts is "what still wants a person": severity first, so a critical one
// is never buried under noise, bounded and unpaged, and the board snapshot and
// its Settled read depend on exactly that. A history is read by time, and a
// cursor over a severity-first order would hand a page boundary to whichever
// conflict's severity climbed next. Keeping them apart leaves the snapshot's
// contract untouched.
//
// Like every read in this package, nothing here selects a column by wildcard.
// conflict.evidence is read only through the three path keys conflictColumns
// names; the rest of it (the intent summaries an agent wrote) never leaves.

// PathConflictHistory is the conflict history route.
const PathConflictHistory = "/v1/teams/{slug}/conflicts/history"

const (
	defaultConflictHistoryPage = 50
	maxConflictHistoryPage     = 100
)

// pastConflictStatuses is what "history" means: the complement of
// openConflictStatuses.
var pastConflictStatuses = []enums.ConflictStatus{
	enums.CONFLICT_STATUS_RESOLVED,
	enums.CONFLICT_STATUS_DISMISSED,
	enums.CONFLICT_STATUS_EXPIRED,
}

type conflictHistoryResponse struct {
	cursors
	Team teamRef `json:"team"`

	// Status and Kind echo the filters applied; "" means every past status,
	// every kind.
	Status string `json:"status"`
	Kind   string `json:"kind"`

	Conflicts []conflictWire `json:"conflicts"`

	// NextCursor is present only when there is an older page. Opaque.
	NextCursor string `json:"next_cursor,omitempty"`

	// Statuses and Kinds are the values the two filters accept, so a client
	// never hard-codes the enums.
	Statuses []string `json:"statuses"`
	Kinds    []string `json:"kinds"`
}

// conflictHistoryOrder is what a past conflict sorts by: when it ended, else
// when it was first seen, else when the row was written.
//
// Every term is fixed once set. last_detected_at is deliberately NOT one of
// them: a re-detection of a settled pair bumps it (the upsert in the detector
// leaves the status alone), and a sort key that moves would move a row across
// a page boundary somebody is paging over.
const conflictHistoryOrder = "COALESCE(c.`resolved_at`, c.`first_detected_at`, c.`created_at`)"

// handleConflictHistory serves
// GET /v1/teams/{slug}/conflicts/history?status=S&kind=K&cursor=C&limit=N.
func (a *API) handleConflictHistory(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	q := r.URL.Query()
	limit := intQuery(r, "limit", defaultConflictHistoryPage, 1, maxConflictHistoryPage)

	statuses := make([]any, 0, len(pastConflictStatuses))
	for _, s := range pastConflictStatuses {
		statuses = append(statuses, s.ToInt64())
	}
	status := strings.ToLower(strings.TrimSpace(q.Get("status")))
	switch status {
	case "", "all":
		status = ""
	default:
		s := enums.ConflictStatusFromString(status)
		if !isPastConflictStatus(s) {
			writeProblem(w, http.StatusBadRequest, "bad request", "status must be resolved, dismissed or expired")
			return
		}
		statuses = []any{s.ToInt64()}
	}

	kind := strings.ToLower(strings.TrimSpace(q.Get("kind")))
	var kindArg any
	if kind != "" {
		k := enums.ConflictKindFromString(kind)
		if k == enums.CONFLICT_KIND_INVALID {
			writeProblem(w, http.StatusBadRequest, "bad request", "kind is not a conflict kind")
			return
		}
		kindArg = k.ToInt64()
	}

	var after *conflictCursor
	if raw := q.Get("cursor"); raw != "" {
		c, err := decodeConflictCursor(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad request", "cursor is not one this endpoint issued")
			return
		}
		after = &c
	}

	out := conflictHistoryResponse{Status: status, Kind: kind, Statuses: pastConflictStatusNames(), Kinds: conflictKindNames()}
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		page, next, err := loadConflictHistoryPage(r.Context(), tx, team.UUID, statuses, kindArg, after, limit)
		if err != nil {
			return err
		}
		out.Conflicts = page
		if next != nil {
			out.NextCursor = next.encode()
		}
		return nil
	})
	if err != nil {
		a.fail(w, r, err, "read the conflict history")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func isPastConflictStatus(s enums.ConflictStatus) bool {
	for _, p := range pastConflictStatuses {
		if s == p {
			return true
		}
	}
	return false
}

func pastConflictStatusNames() []string {
	out := make([]string, 0, len(pastConflictStatuses))
	for _, s := range pastConflictStatuses {
		out = append(out, s.String())
	}
	return out
}

// liveConflictKinds are the kinds something actually detects today, in the
// enum's order. The enum also names decision_contradiction, duplicate_work
// and stale_base, which nothing raises yet; offering them as filters would
// advertise detections that do not exist.
var liveConflictKinds = []enums.ConflictKind{
	enums.CONFLICT_KIND_PATH_OVERLAP,
	enums.CONFLICT_KIND_CONTRACT_MISMATCH,
	enums.CONFLICT_KIND_CONTRACT_UNCLAIMED,
	enums.CONFLICT_KIND_CONTRACT_NAMING_VARIANT,
}

// conflictKindNames lists the live conflict kinds by name.
func conflictKindNames() []string {
	out := make([]string, 0, len(liveConflictKinds))
	for _, k := range liveConflictKinds {
		out = append(out, k.String())
	}
	return out
}

// ---------------------------------------------------------------- cursor

// conflictCursor is the last row of a page: where it sorts, and its key. The
// key is unique per team (uq_conflict_team_key), so it is the tiebreak.
type conflictCursor struct {
	At  string
	Key string
}

const conflictCursorVersion = "c1"

var cursorKeyPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)

func (c conflictCursor) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(conflictCursorVersion + "|" + c.At + "|" + c.Key))
}

func decodeConflictCursor(raw string) (conflictCursor, error) {
	if len(raw) > 256 {
		return conflictCursor{}, errBadCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return conflictCursor{}, errBadCursor
	}
	parts := strings.SplitN(string(b), "|", 3)
	if len(parts) != 3 || parts[0] != conflictCursorVersion || !cursorKeyPattern.MatchString(parts[2]) {
		return conflictCursor{}, errBadCursor
	}
	if _, err := time.Parse(cursorTimeLayout, parts[1]); err != nil {
		return conflictCursor{}, errBadCursor
	}
	return conflictCursor{At: parts[1], Key: parts[2]}, nil
}

// ---------------------------------------------------------------- the page

// loadConflictHistoryPage reads one page of past conflicts with their
// participants and paths, and the cursor for the next page or nil.
//
// Served by idx_conflict_open's (team_uuid, status) prefix for the filter;
// the order is a filesort over one team's closed conflicts. See the report's
// index follow-up.
func loadConflictHistoryPage(ctx context.Context, tx *sql.Tx, teamUUID string, statuses []any, kind any,
	after *conflictCursor, limit int) ([]conflictWire, *conflictCursor, error) {
	q := "SELECT " + conflictColumns + ", " + conflictHistoryOrder + " FROM `conflict` c " +
		"WHERE c.`team_uuid` = ? AND c.`status` IN (" + placeholders(len(statuses)) + ")"
	args := append([]any{teamUUID}, statuses...)
	if kind != nil {
		q += " AND c.`kind` = ?"
		args = append(args, kind)
	}
	if after != nil {
		q += " AND (" + conflictHistoryOrder + " < ? OR (" + conflictHistoryOrder + " = ? AND c.`key` > ?))"
		args = append(args, after.At, after.At, after.Key)
	}
	q += " ORDER BY " + conflictHistoryOrder + " DESC, c.`key` LIMIT ?"
	args = append(args, limit+1)

	out, ids, lastAt, more, err := queryConflicts(ctx, tx, q, args, limit, true)
	if err != nil {
		return nil, nil, err
	}
	if err := attachParticipants(ctx, tx, teamUUID, ids, out); err != nil {
		return nil, nil, err
	}
	if !more || len(out) == 0 {
		return out, nil, nil
	}
	return out, &conflictCursor{At: lastAt.Format(cursorTimeLayout), Key: out[len(out)-1].Key}, nil
}
