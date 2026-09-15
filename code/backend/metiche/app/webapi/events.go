package webapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

// The team's event log, read backwards: the board's timeline rail loads older
// events from here, and its Activity page filters them by kind and session.
//
// The stream (app/stream) reads forwards from a cursor and carries each
// event's payload for the board's live fold; the session page reads one
// session's events forwards. Neither is reused, on purpose: this read goes the
// other way, across sessions, and returns less. team_event holds three columns
// a response must never carry — payload, idempotency_key and
// response_snapshot (a tool's whole answer, which can hold a token) — and the
// explicit column list below is what keeps them out.

const (
	defaultEventsPage = 100
	maxEventsPage     = 200
)

// historyEventWire is one event as the history returns it. Sequence is the
// paging unit and structural decides whether the rail bolds the row; the rest
// is what a person reads.
type historyEventWire struct {
	Sequence   int64   `json:"sequence"`
	Kind       string  `json:"kind"`
	Structural bool    `json:"structural"`
	SubjectKey string  `json:"subject_key,omitempty"`
	Summary    string  `json:"summary,omitempty"`
	SessionKey string  `json:"session_key,omitempty"`
	MemberName string  `json:"member_name,omitempty"`
	AgentLabel string  `json:"agent_label,omitempty"`
	OccurredAt *string `json:"occurred_at,omitempty"`
}

type eventsResponse struct {
	cursors
	Team teamRef `json:"team"`

	// Kind and SessionKey echo the filters; "" is no filter.
	Kind       string `json:"kind"`
	SessionKey string `json:"session_key"`

	// Events are newest first.
	Events []historyEventWire `json:"events"`

	// NextBefore is present only when there are older events: pass it back
	// as ?before=. It is a sequence, the unit the stream's ?after= and the
	// session page's next_after already use, so there is one paging
	// vocabulary for the event log in either direction.
	NextBefore *int64 `json:"next_before,omitempty"`

	// Kinds are the values ?kind= accepts.
	Kinds []string `json:"kinds"`
}

// beforePattern is a sequence as this endpoint issues one: a positive
// integer that fits an int64 with room to spare. Anything else is refused
// rather than read as "no cursor".
var beforePattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// eventColumns and eventJoins are the history's explicit column list. An event
// names its member and agent directly when the writer set them; otherwise the
// session's own member and agent name it.
const eventColumns = "e.`sequence`, e.`kind`, e.`structural`, e.`subject_key`, e.`summary`, e.`occurred_at`, " +
	"s.`key`, COALESCE(m.`display_name`, sm.`display_name`), COALESCE(a.`label`, sa.`label`)"

const eventJoins = "FROM `team_event` e " +
	"LEFT JOIN `session` s ON s.`id` = e.`session_uuid` " +
	"LEFT JOIN `member` m ON m.`id` = e.`member_uuid` " +
	"LEFT JOIN `member` sm ON sm.`id` = s.`member_uuid` " +
	"LEFT JOIN `agent` a ON a.`id` = e.`agent_uuid` " +
	"LEFT JOIN `agent` sa ON sa.`id` = s.`agent_uuid` "

// handleEvents serves GET /v1/teams/{slug}/events?before=N&kind=K&session=S&limit=M.
func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	q := r.URL.Query()
	limit := intQuery(r, "limit", defaultEventsPage, 1, maxEventsPage)

	var before int64
	if raw := q.Get("before"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if !beforePattern.MatchString(raw) || err != nil {
			writeProblem(w, http.StatusBadRequest, "bad request", "before is not a sequence this endpoint issued")
			return
		}
		before = n
	}
	kind := strings.ToLower(strings.TrimSpace(q.Get("kind")))
	var kindArg any
	if kind != "" {
		k := enums.EventKindFromString(kind)
		if k == enums.EVENT_KIND_INVALID {
			writeProblem(w, http.StatusBadRequest, "bad request", "kind is not an event kind")
			return
		}
		kindArg = k.ToInt64()
	}
	sessionKey := strings.TrimSpace(q.Get("session"))
	if sessionKey != "" && !cursorKeyPattern.MatchString(sessionKey) {
		writeProblem(w, http.StatusBadRequest, "bad request", "session is not a session key")
		return
	}

	out := eventsResponse{Kind: kind, SessionKey: sessionKey, Events: []historyEventWire{}, Kinds: eventKindNames()}
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		sessionUUID := ""
		if sessionKey != "" {
			err := tx.QueryRowContext(r.Context(),
				"SELECT `id` FROM `session` WHERE `team_uuid` = ? AND `key` = ? LIMIT 1", team.UUID, sessionKey).Scan(&sessionUUID)
			if errors.Is(err, sql.ErrNoRows) {
				// A session this team never had has no events. Not a 404:
				// the team itself is readable, and a 404 here would be a
				// second, different "not found" for the board to tell apart.
				return nil
			}
			if err != nil {
				return err
			}
		}

		events, next, err := loadEventPage(r.Context(), tx, team.UUID, before, kindArg, sessionUUID, limit)
		if err != nil {
			return err
		}
		out.Events, out.NextBefore = events, next
		return nil
	})
	if err != nil {
		a.fail(w, r, err, "read the events")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// loadEventPage reads one page newest first, and the before-cursor for the
// next page or nil.
//
// Unfiltered and by session it is an index range scan backwards: the
// (team_uuid, sequence) unique index, or (session_uuid, sequence). By kind it
// walks the team's index backwards filtering on kind, which for a rare kind on
// a long log reads far more rows than it returns (see the report's index
// follow-up).
func loadEventPage(ctx context.Context, tx *sql.Tx, teamUUID string, before int64, kind any, sessionUUID string, limit int) ([]historyEventWire, *int64, error) {
	q := "SELECT " + eventColumns + " " + eventJoins + "WHERE e.`team_uuid` = ?"
	args := []any{teamUUID}
	if before > 0 {
		q += " AND e.`sequence` < ?"
		args = append(args, before)
	}
	if kind != nil {
		q += " AND e.`kind` = ?"
		args = append(args, kind)
	}
	if sessionUUID != "" {
		q += " AND e.`session_uuid` = ?"
		args = append(args, sessionUUID)
	}
	q += " ORDER BY e.`sequence` DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]historyEventWire, 0, limit)
	for rows.Next() {
		if len(out) == limit {
			next := out[len(out)-1].Sequence
			return out, &next, nil
		}
		var (
			e                               historyEventWire
			kindRaw                         int64
			subjectKey, summary, sessionKey sql.NullString
			memberName, agentLabel          sql.NullString
			occurred                        sql.NullTime
		)
		if err := rows.Scan(&e.Sequence, &kindRaw, &e.Structural, &subjectKey, &summary, &occurred,
			&sessionKey, &memberName, &agentLabel); err != nil {
			return nil, nil, err
		}
		e.Kind = enums.EventKind(kindRaw).String()
		e.SubjectKey, e.Summary, e.SessionKey = subjectKey.String, summary.String, sessionKey.String
		e.MemberName, e.AgentLabel = memberName.String, agentLabel.String
		e.OccurredAt = rfc3339(occurred)
		out = append(out, e)
	}
	return out, nil, rows.Err()
}

// eventKindNames lists every event kind by name, in the enum's order.
func eventKindNames() []string {
	out := []string{}
	for i := int64(1); ; i++ {
		name := enums.EventKind(i).String()
		if name == enums.EventKind(enums.EVENT_KIND_INVALID).String() {
			return out
		}
		out = append(out, name)
	}
}
