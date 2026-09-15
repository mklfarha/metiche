package web

import (
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
	"github.com/mklfarha/metiche/frontend/internal/view"
)

// The rest of a board's history, next to the Runs pages in runs.go.
//
//	GET /t/{slug}/conflicts[?status=&kind=&cursor=]  open conflicts (hub), then past ones (backend)
//	GET /t/{slug}/activity[?kind=&session=&before=]  the event log, newest first
//	GET /t/{slug}/timeline?before=N                  the rail's older rows (htmx)
//	GET /t/{slug}/graph[?window=24h|7d]              live (hub), or a past window (backend)
//
// The same rules as runs.go. Every request has already been through s.team:
// the same resolution, the same /access check for a signed-in viewer and the
// same 404 as every other board page. The history is read through the team's
// own feed, so the backend sees the principal it sees for the board itself. A
// demo board's feed is a recording and is never asked: its pages are what the
// recording holds, and nothing on them offers to load more.
//
// Contracts and Decisions have no history here: nothing writes those tables
// yet, and their pages keep their empty states.
//
// Every one of these is a read, so "Load older" is a plain GET with no CSRF
// token. A cursor in a URL is the backend's opaque page marker and no secret.

const (
	conflictHistoryPageSize = 50
	activityPageSize        = 100
	railPageSize            = 100
)

var (
	// filterWordPattern bounds a status or kind taken from a URL before it
	// is forwarded; the backend decides whether it is one it knows.
	filterWordPattern = regexp.MustCompile(`^[a-z_]{1,40}$`)
	// sequencePattern bounds a before-cursor.
	sequencePattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
)

// boardHistoryOf is the team's history reader, or nil for a demo board and for
// any feed that cannot read one.
func boardHistoryOf(t *Team) feed.BoardHistory {
	if t == nil || t.Demo {
		return nil
	}
	h, _ := t.Feed.(feed.BoardHistory)
	return h
}

func optional(re *regexp.Regexp, v string) bool { return v == "" || re.MatchString(v) }

// historyRefused reports whether the backend no longer lets this reader see
// the team.
func historyRefused(err error) bool {
	return errors.Is(err, feed.ErrNotFound) || errors.Is(err, feed.ErrSessionInvalid)
}

// historyNotFound is the answer an unknown team gets.
func (s *Server) historyNotFound(w http.ResponseWriter, r *http.Request, t *Team) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, view.NotFound(t.Slug))
}

func isFragment(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// ---------------------------------------------------------------- conflicts

func (s *Server) conflicts(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	h := boardHistoryOf(t)
	if h == nil {
		s.page(w, r, t, snap, view.TabConflicts, view.ConflictsPage(snap))
		return
	}
	if t.viewer {
		privateHeaders(w)
	}
	w.Header().Add("Vary", "HX-Request")

	q := r.URL.Query()
	status, kind, cursor := q.Get("status"), q.Get("kind"), q.Get("cursor")
	if status == "all" {
		status = ""
	}
	if !optional(filterWordPattern, status) || !optional(filterWordPattern, kind) || !optional(runCursorPattern, cursor) {
		http.Error(w, "that page of conflicts does not exist", http.StatusBadRequest)
		return
	}
	fragment := cursor != "" && isFragment(r)

	p := view.ConflictHistoryParams{Slug: t.Slug, Now: snap.Now, Status: status, Kind: kind, Older: cursor != ""}
	page, err := h.ConflictHistory(r.Context(), model.ConflictQuery{
		Status: status, Kind: kind, Cursor: cursor, Limit: conflictHistoryPageSize,
	})
	switch {
	case err == nil:
		p.Rows, p.Statuses, p.Kinds = page.Conflicts, page.Statuses, page.Kinds
		if page.NextCursor != "" {
			p.OlderURL = view.ConflictHistoryURL(t.Slug, status, kind, page.NextCursor)
		}
	case historyRefused(err):
		s.historyNotFound(w, r, t)
		return
	case errors.Is(err, feed.ErrBadRequest):
		http.Error(w, "that page of conflicts does not exist", http.StatusBadRequest)
		return
	default:
		s.log.Warn("reading the conflict history failed", "err", err)
		if fragment {
			http.Error(w, "older conflicts are unavailable right now; try again shortly", http.StatusServiceUnavailable)
			return
		}
		p.Unavailable, p.Older = true, false
	}

	if fragment {
		s.render(w, r, view.ConflictHistoryRows(p))
		return
	}
	s.page(w, r, t, snap, view.TabConflicts, view.ConflictsHistoryPage(snap, p))
}

// ---------------------------------------------------------------- activity

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	kind, session, beforeRaw := q.Get("kind"), strings.TrimSpace(q.Get("session")), q.Get("before")
	if !optional(filterWordPattern, kind) || !optional(sessionKeyPattern, session) || !optional(sequencePattern, beforeRaw) {
		http.Error(w, "that page of activity does not exist", http.StatusBadRequest)
		return
	}
	before, _ := strconv.ParseInt(beforeRaw, 10, 64)

	p := view.ActivityParams{Slug: t.Slug, Kind: kind, Session: session}
	h := boardHistoryOf(t)
	if h == nil {
		p.Demo = true
		p.Rows, p.Kinds = hubActivity(snap, kind, session)
		s.page(w, r, t, snap, view.TabActivity, view.ActivityPage(snap, p))
		return
	}
	if t.viewer {
		privateHeaders(w)
	}
	w.Header().Add("Vary", "HX-Request")
	fragment := before > 0 && isFragment(r)
	p.Older = before > 0

	page, err := h.Events(r.Context(), model.EventQuery{Before: before, Kind: kind, Session: session, Limit: activityPageSize})
	switch {
	case err == nil:
		p.Rows, p.Kinds = page.Events, page.Kinds
		if page.NextBefore > 0 {
			p.OlderURL = view.ActivityURL(t.Slug, page.NextBefore, kind, session)
		}
	case historyRefused(err):
		s.historyNotFound(w, r, t)
		return
	case errors.Is(err, feed.ErrBadRequest):
		http.Error(w, "that page of activity does not exist", http.StatusBadRequest)
		return
	default:
		s.log.Warn("reading the event history failed", "err", err)
		if fragment {
			http.Error(w, "older events are unavailable right now; try again shortly", http.StatusServiceUnavailable)
			return
		}
		p.Unavailable, p.Older = true, false
		p.Rows, p.Kinds = hubActivity(snap, kind, session)
	}

	if fragment {
		s.render(w, r, view.ActivityRows(p))
		return
	}
	s.page(w, r, t, snap, view.TabActivity, view.ActivityPage(snap, p))
}

// hubActivity is the log this board holds, newest first and filtered, with
// the kinds it contains: a demo board's whole Activity page, and a live
// board's when the backend cannot be read.
func hubActivity(snap state.Snapshot, kind, session string) ([]model.HistoryEvent, []string) {
	var rows []model.HistoryEvent
	seen := map[string]bool{}
	kinds := []string{}
	for i := len(snap.Events) - 1; i >= 0; i-- {
		ev := snap.Events[i]
		if !seen[ev.Kind] {
			seen[ev.Kind] = true
			kinds = append(kinds, ev.Kind)
		}
		if (kind != "" && ev.Kind != kind) || (session != "" && ev.SessionKey != session) {
			continue
		}
		row := model.HistoryEvent{Sequence: ev.Sequence, Kind: ev.Kind, Structural: ev.Structural,
			Summary: ev.Summary, SessionKey: ev.SessionKey, OccurredAt: ev.OccurredAt}
		if label := snap.SessionLabel(ev.SessionKey); ev.SessionKey != "" && label != ev.SessionKey {
			row.MemberName = label
		}
		rows = append(rows, row)
	}
	sort.Strings(kinds)
	return rows, kinds
}

// ---------------------------------------------------------------- the rail

// timeline answers the rail's "Load older events" with the rows before a
// sequence, oldest first, and the control for the page before those.
func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	t, _, r, ok := s.team(w, r)
	if !ok {
		return
	}
	h := boardHistoryOf(t)
	if h == nil {
		// A demo board's rail has no such control, and a recording is never
		// asked for more.
		s.historyNotFound(w, r, t)
		return
	}
	raw := r.URL.Query().Get("before")
	if !sequencePattern.MatchString(raw) {
		http.Error(w, "those events do not exist", http.StatusBadRequest)
		return
	}
	before, _ := strconv.ParseInt(raw, 10, 64)
	if t.viewer {
		privateHeaders(w)
	}

	page, err := h.Events(r.Context(), model.EventQuery{Before: before, Limit: railPageSize})
	switch {
	case err == nil:
	case historyRefused(err):
		s.historyNotFound(w, r, t)
		return
	case errors.Is(err, feed.ErrBadRequest):
		http.Error(w, "those events do not exist", http.StatusBadRequest)
		return
	default:
		s.log.Warn("reading older events failed", "err", err)
		http.Error(w, "older events are unavailable right now; try again shortly", http.StatusServiceUnavailable)
		return
	}
	p := view.RailOlderParams{Slug: t.Slug, Next: page.NextBefore}
	for i := len(page.Events) - 1; i >= 0; i-- {
		p.Rows = append(p.Rows, page.Events[i])
	}
	s.render(w, r, view.RailOlderRows(p))
}

// ---------------------------------------------------------------- the graph

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	window := r.URL.Query().Get("window")
	h := boardHistoryOf(t)
	if h == nil || window == "" || window == "live" {
		// Live, as it always was. A demo board is always live: its recording
		// has no past windows to draw.
		s.page(w, r, t, snap, view.TabGraph, view.GraphPage(snap))
		return
	}
	if window != "24h" && window != "7d" {
		http.Error(w, "there is no such time range", http.StatusBadRequest)
		return
	}
	if t.viewer {
		privateHeaders(w)
	}

	p := view.GraphParams{Slug: t.Slug, Window: window}
	data, err := h.GraphWindow(r.Context(), window)
	switch {
	case err == nil:
		p.Data, p.Graph = data, state.WindowGraph(t.Slug, data)
	case historyRefused(err):
		s.historyNotFound(w, r, t)
		return
	default:
		s.log.Warn("reading a past graph window failed", "err", err)
		p.Unavailable = true
	}
	s.page(w, r, t, snap, view.TabGraph, view.GraphWindowPage(snap, p))
}
