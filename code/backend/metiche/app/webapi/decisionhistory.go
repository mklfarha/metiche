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

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/enums"
)

// A team's decision history: the decisions that are over — superseded or
// revoked — newest first, a page at a time, and one decision's earlier
// wordings.
//
// It is a sibling of GET /v1/teams/{slug}/decisions for the same reason the
// conflict history is a sibling of /conflicts: the two answer different
// questions and sort differently. /decisions is "what is in force", always-show
// first so a load-bearing rule is never buried, bounded and unpaged. A history
// is read by time, and a cursor over an always-show-first order would hand a
// page boundary to whichever decision got flagged next.
//
// Nothing here selects a column by wildcard. team_event carries three columns
// a response must never hold — the raw payload, the idempotency key and
// response_snapshot, which is a tool's whole answer and can hold a token — and
// the revisions query reads exactly one key out of the payload, by name.

// PathDecisionHistory is the decision history route.
const PathDecisionHistory = "/v1/teams/{slug}/decisions/history"

const (
	defaultDecisionHistoryPage = 50
	maxDecisionHistoryPage     = 100

	// maxDecisionRevisions bounds ?key=: the wordings a decision has had,
	// newest first. Older ones age out with event retention (§10, decision 5),
	// so this is a page of what still exists, not a guarantee.
	maxDecisionRevisions = 20
)

// pastDecisionStatuses is what "history" means here: a decision that is no
// longer in force. proposed is unused in v1 (§10, decision 6) and accepted is
// what /decisions serves.
var pastDecisionStatuses = []enums.DecisionStatus{
	enums.DECISION_STATUS_SUPERSEDED,
	enums.DECISION_STATUS_REVOKED,
}

type decisionHistoryResponse struct {
	cursors
	Team teamRef `json:"team"`

	// Status echoes the filter applied; "" means every past status.
	Status string `json:"status"`

	// Statuses are the values ?status= accepts, so a client never hard-codes
	// the enum.
	Statuses []string `json:"statuses"`

	Decisions []decisionWire `json:"decisions"`

	// NextCursor is present only when there is an older page. Opaque.
	NextCursor string `json:"next_cursor,omitempty"`

	// Revisions is present only for ?key=: that decision's earlier wordings,
	// newest first.
	Revisions []decisionRevisionWire `json:"revisions,omitempty"`
}

// handleDecisionHistory serves
// GET /v1/teams/{slug}/decisions/history?status=S&cursor=C&limit=N&key=K.
//
// ?key= is the other question this endpoint answers: one decision in ANY
// status, with the wordings it has had. It is here rather than on /decisions
// because it is a read of the past — the current wording is on the card
// already — and because it pages the event log, which nothing on /decisions
// touches.
func (a *API) handleDecisionHistory(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	q := r.URL.Query()
	limit := intQuery(r, "limit", defaultDecisionHistoryPage, 1, maxDecisionHistoryPage)

	statuses := make([]any, 0, len(pastDecisionStatuses))
	for _, s := range pastDecisionStatuses {
		statuses = append(statuses, s.ToInt64())
	}
	status := strings.ToLower(strings.TrimSpace(q.Get("status")))
	switch status {
	case "", "all":
		status = ""
	default:
		s := enums.DecisionStatusFromString(status)
		if !isPastDecisionStatus(s) {
			writeProblem(w, http.StatusBadRequest, "bad request", "status must be superseded or revoked")
			return
		}
		statuses = []any{s.ToInt64()}
	}

	// One decision by key, in any status. Normalized the same way
	// record_decision normalizes it, so #auth-jwt-cookie and auth-jwt-cookie
	// are the same request.
	key := ""
	if raw := strings.TrimSpace(q.Get("key")); raw != "" {
		k, err := coordination.NormalizeDecisionKey(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad request", "key is not a decision key")
			return
		}
		key = k
	}

	var after *decisionCursor
	if raw := q.Get("cursor"); raw != "" {
		c, err := decodeDecisionCursor(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad request", "cursor is not one this endpoint issued")
			return
		}
		after = &c
	}

	out := decisionHistoryResponse{Status: status, Statuses: pastDecisionStatusNames(), Decisions: []decisionWire{}}
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		if key != "" {
			one, err := loadDecisionByKey(r.Context(), tx, team.UUID, key)
			if err != nil {
				return err
			}
			out.Decisions = one
			out.Revisions, err = loadDecisionRevisions(r.Context(), tx, team.UUID, key)
			return err
		}

		page, next, err := loadDecisionHistoryPage(r.Context(), tx, team.UUID, statuses, after, limit)
		if err != nil {
			return err
		}
		out.Decisions = page
		if next != nil {
			out.NextCursor = next.encode()
		}
		return nil
	})
	if err != nil {
		a.fail(w, r, err, "read the decision history")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func isPastDecisionStatus(s enums.DecisionStatus) bool {
	for _, p := range pastDecisionStatuses {
		if s == p {
			return true
		}
	}
	return false
}

func pastDecisionStatusNames() []string {
	out := make([]string, 0, len(pastDecisionStatuses))
	for _, s := range pastDecisionStatuses {
		out = append(out, s.String())
	}
	return out
}

// ---------------------------------------------------------------- cursor

// decisionCursor is the last row of a page: when it was last written, and its
// key. The key is unique per team (uq_decision_team_key), so it is the
// tiebreak for decisions that ended in the same second.
type decisionCursor struct {
	At  string // RFC 3339 UTC, the wire's own time format
	Key string
}

const decisionCursorVersion = "d1"

// decisionKeyPattern is a decision key as coordination.NormalizeDecisionKey
// produces one: a leading # and 3-60 lowercase letters, digits and dashes.
var decisionKeyPattern = regexp.MustCompile(`^#[a-z0-9-]{3,60}$`)

func (c decisionCursor) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(decisionCursorVersion + "|" + c.At + "|" + c.Key))
}

func decodeDecisionCursor(raw string) (decisionCursor, error) {
	if len(raw) > 256 {
		return decisionCursor{}, errBadCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return decisionCursor{}, errBadCursor
	}
	parts := strings.SplitN(string(b), "|", 3)
	if len(parts) != 3 || parts[0] != decisionCursorVersion || !decisionKeyPattern.MatchString(parts[2]) {
		return decisionCursor{}, errBadCursor
	}
	if _, err := time.Parse(time.RFC3339, parts[1]); err != nil {
		return decisionCursor{}, errBadCursor
	}
	return decisionCursor{At: parts[1], Key: parts[2]}, nil
}

// sqlTime turns the cursor's RFC 3339 instant back into the literal the
// database compares against. The cursor is RFC 3339 because that is the one
// time format this API speaks on the wire; the column is a DATETIME written
// in UTC, so the comparison is against the same instant either way.
func (c decisionCursor) sqlTime() (string, error) {
	t, err := time.Parse(time.RFC3339, c.At)
	if err != nil {
		return "", errBadCursor
	}
	return t.UTC().Format(cursorTimeLayout), nil
}

// ---------------------------------------------------------------- the page

// loadDecisionHistoryPage reads one page of past decisions, newest first, with
// their scope, judged counts and open conflicts, and the cursor for the next
// page or nil.
//
// The order is (updated_at DESC, key ASC). updated_at is fixed once a decision
// is terminal — a superseded or revoked decision is never written again — so a
// row cannot move across a page boundary somebody is paging over. A decision
// that ENDS mid-walk sorts ahead of every cursor already issued, so it appears
// on a later first page and never as a gap or a duplicate.
func loadDecisionHistoryPage(ctx context.Context, tx *sql.Tx, teamUUID string, statuses []any,
	after *decisionCursor, limit int) ([]decisionWire, *decisionCursor, error) {
	q := "SELECT " + decisionColumns + " " + decisionJoins +
		"WHERE d.`team_uuid` = ? AND d.`status` IN (" + placeholders(len(statuses)) + ")"
	args := append([]any{teamUUID}, statuses...)
	if after != nil {
		at, err := after.sqlTime()
		if err != nil {
			return nil, nil, err
		}
		q += " AND (d.`updated_at` < ? OR (d.`updated_at` = ? AND d.`key` > ?))"
		args = append(args, at, at, after.Key)
	}
	q += " ORDER BY d.`updated_at` DESC, d.`key` LIMIT ?"
	args = append(args, limit+1) // one extra row says whether there is a next page

	out, ids, lastAt, more, err := queryDecisions(ctx, tx, q, args, limit)
	if err != nil {
		return nil, nil, err
	}
	if err := attachDecisionDetail(ctx, tx, teamUUID, ids, out); err != nil {
		return nil, nil, err
	}
	if !more || len(out) == 0 {
		return out, nil, nil
	}
	return out, &decisionCursor{At: lastAt.UTC().Format(time.RFC3339), Key: out[len(out)-1].Key}, nil
}

// loadDecisionByKey reads one decision in any status. An empty result is not a
// 404: the team itself is readable, and a second, different "not found" is one
// more thing for the board to tell apart from the gate's.
func loadDecisionByKey(ctx context.Context, tx *sql.Tx, teamUUID, key string) ([]decisionWire, error) {
	q := "SELECT " + decisionColumns + " " + decisionJoins +
		"WHERE d.`team_uuid` = ? AND d.`key` = ? LIMIT ?"
	out, ids, _, _, err := queryDecisions(ctx, tx, q, []any{teamUUID, key, 1}, 1)
	if err != nil {
		return nil, err
	}
	return out, attachDecisionDetail(ctx, tx, teamUUID, ids, out)
}

// decisionRevisionColumns reads the event log's account of one decision. The
// payload is read through ONE key by name — message, which the writer sets to
// the wording as it was recorded — so the rest of the document, and the two
// secret-bearing columns beside it, never reach a response.
const decisionRevisionColumns = "e.`sequence`, e.`kind`, e.`summary`, " +
	"JSON_VALUE(e.`payload`, '$.message'), e.`occurred_at`"

// loadDecisionRevisions reads the wordings one decision has had, newest first.
//
// This walks the team's (team_uuid, sequence) index backwards filtering on
// kind and subject_key, which for an old decision on a long log reads more
// rows than it returns — the same cost profile events.go already accepts for a
// kind filter, bounded here by a LIMIT of 20.
func loadDecisionRevisions(ctx context.Context, tx *sql.Tx, teamUUID, key string) ([]decisionRevisionWire, error) {
	q := "SELECT " + decisionRevisionColumns + " FROM `team_event` e " +
		"WHERE e.`team_uuid` = ? AND e.`kind` IN (?, ?) AND e.`subject_key` = ? " +
		"ORDER BY e.`sequence` DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, q, teamUUID,
		int64(enums.EVENT_KIND_DECISION_RECORDED), int64(enums.EVENT_KIND_DECISION_SUPERSEDED),
		key, maxDecisionRevisions)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]decisionRevisionWire, 0, maxDecisionRevisions)
	for rows.Next() {
		var (
			rev                decisionRevisionWire
			kind               int64
			summary, statement sql.NullString
			occurred           sql.NullTime
		)
		if err := rows.Scan(&rev.Sequence, &kind, &summary, &statement, &occurred); err != nil {
			return nil, err
		}
		rev.Kind = enums.EventKind(kind).String()
		rev.Summary = summary.String
		// An earlier wording was written by an agent, like the rationale, and
		// an older binary's row was never masked on the way in.
		rev.Statement = boardText(statement.String, maxDecisionRevisionChars)
		rev.OccurredAt = rfc3339(occurred)
		out = append(out, rev)
	}
	return out, rows.Err()
}

// maxDecisionRevisionChars is the statement limit record_decision enforces
// (app/mcp's MaxDecisionStatementChars), so a wording that fitted when it was
// recorded is served whole.
const maxDecisionRevisionChars = 400
