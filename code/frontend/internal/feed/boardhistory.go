package feed

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// BoardHistory reads the rest of a team's history from the backend: past
// conflicts, the event log backwards, and a past window of the graph.
//
// Like RunHistory, a Live implements it and reads with exactly the principal
// it reads the board with; a Fixture does not, so a demo board has no way to
// ask the backend anything.
type BoardHistory interface {
	// ConflictHistory reads one page of past conflicts, newest first.
	ConflictHistory(ctx context.Context, q model.ConflictQuery) (model.ConflictPage, error)
	// Events reads one page of the event log, newest first.
	Events(ctx context.Context, q model.EventQuery) (model.EventPage, error)
	// GraphWindow reads what was entangled over a past window: "24h" or "7d".
	GraphWindow(ctx context.Context, window string) (model.GraphWindow, error)
}

var _ BoardHistory = (*Live)(nil)

// ErrBadRequest means the backend refused a history read's parameters (a
// filter value or cursor it does not accept) with a 400. It is never a
// verdict about the team.
var ErrBadRequest = errors.New("the backend refused the request's parameters")

// ---------------------------------------------------------------- wire

type conflictHistoryWire struct {
	Conflicts  []conflictJSON `json:"conflicts"`
	NextCursor string         `json:"next_cursor"`
	Statuses   []string       `json:"statuses"`
	Kinds      []string       `json:"kinds"`
}

type historyEventJSON struct {
	Sequence   int64   `json:"sequence"`
	Kind       string  `json:"kind"`
	Structural bool    `json:"structural"`
	SubjectKey string  `json:"subject_key"`
	Summary    string  `json:"summary"`
	SessionKey string  `json:"session_key"`
	MemberName string  `json:"member_name"`
	AgentLabel string  `json:"agent_label"`
	OccurredAt *string `json:"occurred_at"`
}

type eventsWire struct {
	Events     []historyEventJSON `json:"events"`
	NextBefore *int64             `json:"next_before"`
	Kinds      []string           `json:"kinds"`
}

type graphHoldJSON struct {
	SessionKey string  `json:"session_key"`
	ClaimKey   string  `json:"claim_key"`
	Mode       string  `json:"mode"`
	Path       string  `json:"path"`
	Status     string  `json:"status"`
	HeldFrom   *string `json:"held_from"`
	HeldUntil  *string `json:"held_until"`
}

type graphWindowWire struct {
	Window    string               `json:"window"`
	From      string               `json:"from"`
	To        string               `json:"to"`
	Sessions  []historySessionJSON `json:"sessions"`
	Holds     []graphHoldJSON      `json:"holds"`
	Conflicts []conflictJSON       `json:"conflicts"`
	Truncated bool                 `json:"truncated"`
}

func (l *Live) teamURL(sub string, q url.Values) string {
	target := l.root() + "/teams/" + url.PathEscape(l.Slug) + "/" + sub
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	return target
}

// ---------------------------------------------------------------- reads

// ConflictHistory implements BoardHistory: GET /v1/teams/{slug}/conflicts/history.
func (l *Live) ConflictHistory(ctx context.Context, in model.ConflictQuery) (model.ConflictPage, error) {
	q := url.Values{}
	setIf(q, "status", in.Status)
	setIf(q, "kind", in.Kind)
	setIf(q, "cursor", in.Cursor)
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	var wire conflictHistoryWire
	if err := l.readHistory(ctx, l.teamURL("conflicts/history", q), &wire); err != nil {
		return model.ConflictPage{}, err
	}
	out := model.ConflictPage{NextCursor: wire.NextCursor, Statuses: wire.Statuses, Kinds: wire.Kinds,
		Conflicts: make([]*model.Conflict, 0, len(wire.Conflicts))}
	for _, c := range wire.Conflicts {
		out.Conflicts = append(out.Conflicts, c.conflict(nil))
	}
	return out, nil
}

// Events implements BoardHistory: GET /v1/teams/{slug}/events.
func (l *Live) Events(ctx context.Context, in model.EventQuery) (model.EventPage, error) {
	q := url.Values{}
	if in.Before > 0 {
		q.Set("before", strconv.FormatInt(in.Before, 10))
	}
	setIf(q, "kind", in.Kind)
	setIf(q, "session", in.Session)
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	var wire eventsWire
	if err := l.readHistory(ctx, l.teamURL("events", q), &wire); err != nil {
		return model.EventPage{}, err
	}
	out := model.EventPage{Kinds: wire.Kinds, Events: make([]model.HistoryEvent, 0, len(wire.Events))}
	if wire.NextBefore != nil {
		out.NextBefore = *wire.NextBefore
	}
	for _, e := range wire.Events {
		out.Events = append(out.Events, model.HistoryEvent{
			Sequence: e.Sequence, Kind: e.Kind, Structural: e.Structural, SubjectKey: e.SubjectKey,
			Summary: e.Summary, SessionKey: e.SessionKey, MemberName: e.MemberName, AgentLabel: e.AgentLabel,
			OccurredAt: parseTime(e.OccurredAt),
		})
	}
	return out, nil
}

// GraphWindow implements BoardHistory: GET /v1/teams/{slug}/graph.
func (l *Live) GraphWindow(ctx context.Context, window string) (model.GraphWindow, error) {
	var wire graphWindowWire
	if err := l.readHistory(ctx, l.teamURL("graph", url.Values{"window": {window}}), &wire); err != nil {
		return model.GraphWindow{}, err
	}
	out := model.GraphWindow{
		Window: wire.Window, From: parseTime(&wire.From), To: parseTime(&wire.To), Truncated: wire.Truncated,
	}
	for _, s := range wire.Sessions {
		out.Sessions = append(out.Sessions, s.summary())
	}
	for _, h := range wire.Holds {
		out.Holds = append(out.Holds, model.GraphHold{
			SessionKey: h.SessionKey, ClaimKey: h.ClaimKey, Mode: h.Mode, Path: h.Path, Status: h.Status,
			From: parseTime(h.HeldFrom), Until: parseTime(h.HeldUntil),
		})
	}
	for _, c := range wire.Conflicts {
		out.Conflicts = append(out.Conflicts, c.conflict(nil))
	}
	return out, nil
}

func setIf(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}
