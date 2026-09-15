package view

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
)

// TabActivity is the Activity nav item: the team's event log.
const TabActivity Tab = "activity"

type historyKey struct{}

// WithHistory marks a render as a board that can read its history from the
// backend. A demo board never is, so nothing on it offers to load more.
func WithHistory(ctx context.Context) context.Context {
	return context.WithValue(ctx, historyKey{}, true)
}

// HasHistory reports whether WithHistory marked this render.
func HasHistory(ctx context.Context) bool {
	v, _ := ctx.Value(historyKey{}).(bool)
	return v
}

// ConflictHistoryParams is the History section of the Conflicts page.
type ConflictHistoryParams struct {
	Slug string
	Now  time.Time

	// Status and Kind are the filters applied; "" is every value.
	Status, Kind string
	// Statuses and Kinds are what the filters accept, as the backend says.
	Statuses, Kinds []string

	Rows []*model.Conflict // newest first
	// OlderURL loads the next, older page; "" when there is none.
	OlderURL string
	// Older is true when the page starts from a cursor (no JavaScript).
	Older bool
	// Unavailable is true when the backend could not be read.
	Unavailable bool
}

// Filtered reports whether a filter is applied.
func (p ConflictHistoryParams) Filtered() bool { return p.Status != "" || p.Kind != "" }

// ActivityParams is the Activity page.
type ActivityParams struct {
	Slug string

	Kind, Session string
	Kinds         []string

	Rows     []model.HistoryEvent // newest first
	OlderURL string
	Older    bool
	// Unavailable: the backend could not be read; Rows are what the board holds.
	Unavailable bool
	// Demo: Rows are the recording's, and there are no older pages.
	Demo bool
}

// Filtered reports whether a filter is applied.
func (p ActivityParams) Filtered() bool { return p.Kind != "" || p.Session != "" }

// RailOlderParams is the answer to the rail's "Load older events".
type RailOlderParams struct {
	Slug string
	Rows []model.HistoryEvent // oldest first
	// Next is the before-cursor for the page older than Rows; 0 when none.
	Next int64
}

// GraphParams is the graph over a past window.
type GraphParams struct {
	Slug        string
	Window      string // 24h | 7d
	Data        model.GraphWindow
	Graph       state.Graph
	Unavailable bool
}

// graphWindowChoices are the time range selector's choices; "" is live.
var graphWindowChoices = []struct{ Window, Label string }{
	{"", "Live"}, {"24h", "Last 24 hours"}, {"7d", "Last 7 days"},
}

func windowPhrase(w string) string {
	switch w {
	case "24h":
		return "the last 24 hours"
	case "7d":
		return "the last 7 days"
	}
	return "this window"
}

func teamURL(slug, sub string) templ.SafeURL {
	return templ.SafeURL("/t/" + url.PathEscape(slug) + "/" + sub)
}

func graphWindowURL(slug, window string) templ.SafeURL {
	if window == "" {
		return teamURL(slug, "graph")
	}
	return templ.SafeURL("/t/" + url.PathEscape(slug) + "/graph?window=" + url.QueryEscape(window))
}

func withQuery(path string, v url.Values) string {
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// ConflictHistoryURL is the Conflicts page with these filters, from cursor.
func ConflictHistoryURL(slug, status, kind, cursor string) string {
	v := url.Values{}
	for k, val := range map[string]string{"status": status, "kind": kind, "cursor": cursor} {
		if val != "" {
			v.Set(k, val)
		}
	}
	return withQuery("/t/"+url.PathEscape(slug)+"/conflicts", v)
}

// ActivityURL is the Activity page with these filters, before a sequence.
func ActivityURL(slug string, before int64, kind, session string) string {
	v := url.Values{}
	if before > 0 {
		v.Set("before", strconv.FormatInt(before, 10))
	}
	if kind != "" {
		v.Set("kind", kind)
	}
	if session != "" {
		v.Set("session", session)
	}
	return withQuery("/t/"+url.PathEscape(slug)+"/activity", v)
}

func railFragmentURL(slug string) string { return "/t/" + url.PathEscape(slug) + "/timeline" }

// railVals is the control's default before; board.js replaces it with the
// oldest row on screen when the control is clicked.
func railVals(before int64) string { return fmt.Sprintf(`{"before":"%d"}`, before) }

// RailBefore is where the rail's older events start: before its oldest row,
// or, for an empty rail, before whatever the next event will be.
func RailBefore(s state.Snapshot) int64 {
	if ev := s.RecentEvents(120); len(ev) > 0 {
		return ev[0].Sequence
	}
	return s.Team.Sequence + 1
}

func participantWho(p model.Participant) string {
	switch {
	case p.MemberName == "":
		return p.AgentLabel
	case p.AgentLabel == "":
		return p.MemberName
	}
	return p.MemberName + " · " + p.AgentLabel
}

func kindOrEvery(kind string) string {
	if kind == "" {
		return "every kind"
	}
	return kind
}

// activityOnlyURL is a row's "only this run" filter, or "" when the page is
// already one session's or the event belongs to none.
func activityOnlyURL(p ActivityParams, ev model.HistoryEvent) string {
	if p.Session != "" || ev.SessionKey == "" {
		return ""
	}
	return ActivityURL(p.Slug, 0, p.Kind, ev.SessionKey)
}

func pastStatusBadge(status string) string {
	if status == "resolved" {
		return "good"
	}
	return "plain"
}
