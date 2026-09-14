package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
	"github.com/mklfarha/metiche/frontend/internal/view"
)

// The Runs page and one run's page.
//
//	GET /t/{slug}/runs                      the live sessions (hub), then the history (backend)
//	GET /t/{slug}/runs?cursor=C&live=K,K    an older page; with HX-Request, only its rows
//	GET /t/{slug}/runs/{session}            one run: the hub's, else the backend's story
//
// Every request has already been through s.team: the same resolution, the
// same /access check for a signed-in viewer and the same 404 as every other
// board page. The history is then read through the team's own feed, so the
// backend sees the principal it sees for the board itself — no credential for
// a public team, the viewer's browser session (a header, never a URL) for a
// private board. A demo board's feed is a recording and is never asked: its
// pages are exactly what they were before there was a history.
//
// Both are reads, so "Load older" is a plain GET with no CSRF token. The
// cursor in its URL is the backend's opaque page marker and no secret.

const (
	runsPageSize = 50
	// maxLiveKeys bounds the live= list an older page is asked to leave out.
	maxLiveKeys = 100
)

// runCursorPattern bounds a cursor taken from a URL before it is forwarded.
var runCursorPattern = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,256}$`)

// runHistoryOf is the team's history reader, or nil for a demo board and for
// any feed that cannot read one.
func runHistoryOf(t *Team) feed.RunHistory {
	if t == nil || t.Demo {
		return nil
	}
	h, _ := t.Feed.(feed.RunHistory)
	return h
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	h := runHistoryOf(t)
	if h == nil {
		s.page(w, r, t, snap, view.TabRuns, view.RunsPage(snap))
		return
	}
	if t.viewer {
		privateHeaders(w)
	}
	w.Header().Add("Vary", "HX-Request")

	q := r.URL.Query()
	cursor := q.Get("cursor")
	if cursor != "" && !runCursorPattern.MatchString(cursor) {
		http.Error(w, "that page of runs does not exist", http.StatusBadRequest)
		return
	}
	fragment := cursor != "" && r.Header.Get("HX-Request") == "true"

	p := view.RunsParams{Slug: t.Slug, Now: snap.Now, Older: cursor != ""}

	// A session is drawn once. The live section is the hub's; a later page's
	// request carries the keys that section showed, so a run that was live
	// when the page loaded is not drawn again below it.
	shown := map[string]bool{}
	if fragment {
		for _, k := range strings.Split(q.Get("live"), ",") {
			if len(shown) < maxLiveKeys && sessionKeyPattern.MatchString(k) {
				shown[k] = true
			}
		}
	} else {
		for _, sess := range snap.Runs() {
			if sess.Live() {
				p.Live = append(p.Live, hubRunRow(snap, sess))
				shown[sess.Key] = true
			}
		}
	}

	page, err := h.Runs(r.Context(), cursor, runsPageSize)
	switch {
	case err == nil:
		for _, run := range page.Runs {
			if !shown[run.Key] {
				p.History = append(p.History, historyRunRow(run))
			}
		}
		if page.NextCursor != "" {
			p.OlderURL = olderRunsURL(t.Slug, page.NextCursor, shown)
		}
	case errors.Is(err, feed.ErrNotFound), errors.Is(err, feed.ErrSessionInvalid):
		// The backend no longer lets this reader see the team: the answer an
		// unknown team gets.
		w.WriteHeader(http.StatusNotFound)
		s.render(w, r, view.NotFound(t.Slug))
		return
	default:
		s.log.Warn("reading the run history failed", "err", err)
		if fragment {
			http.Error(w, "older runs are unavailable right now; try again shortly", http.StatusServiceUnavailable)
			return
		}
		p.HistoryUnavailable = true
		p.Older = false
		for _, sess := range snap.Runs() {
			if !sess.Live() {
				p.History = append(p.History, hubRunRow(snap, sess))
			}
		}
	}

	if fragment {
		s.render(w, r, view.RunsHistoryRows(p))
		return
	}
	s.page(w, r, t, snap, view.TabRuns, view.RunsHistoryPage(p))
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	key := chi.URLParam(r, "session")
	if sess := snap.Session(key); sess != nil {
		s.page(w, r, t, snap, view.TabRuns, view.RunPage(snap, sess))
		return
	}
	h := runHistoryOf(t)
	if h == nil || !sessionKeyPattern.MatchString(key) {
		s.runNotFound(w, r, t, snap)
		return
	}
	if t.viewer {
		privateHeaders(w)
	}
	d, err := h.Run(r.Context(), key)
	switch {
	case err == nil:
		s.page(w, r, t, snap, view.TabRuns, view.HistoricalRunPage(snap, view.RunDetailParams{Slug: t.Slug, Detail: d}))
	case errors.Is(err, feed.ErrNotFound), errors.Is(err, feed.ErrSessionInvalid):
		s.runNotFound(w, r, t, snap)
	default:
		s.log.Warn("reading a run failed", "err", err)
		http.Error(w, "this run is unavailable right now; try again shortly", http.StatusServiceUnavailable)
	}
}

// runNotFound is what an unknown session has always been: the Runs page of
// what the board holds, with a 404.
func (s *Server) runNotFound(w http.ResponseWriter, r *http.Request, t *Team, snap state.Snapshot) {
	w.WriteHeader(http.StatusNotFound)
	s.page(w, r, t, snap, view.TabRuns, view.RunsPage(snap))
}

func hubRunRow(s state.Snapshot, sess *model.Session) view.RunRow {
	intents, paths, conflicts := s.RunCounts(sess.Key)
	return view.RunRow{
		Key: sess.Key, Agent: s.SessionLabel(sess.Key), Project: s.Project.Key,
		Headline: firstNonEmpty(sess.Goal, sess.StatusLine), Status: sess.Status, Live: sess.Live(),
		StartedAt: sess.StartedAt, EndedAt: sess.EndedAt,
		Intents: intents, Paths: paths, Conflicts: conflicts,
		ParentKey: sess.ParentSessionKey, Subagents: len(s.SubagentsOf(sess.Key)),
	}
}

func historyRunRow(run model.RunSummary) view.RunRow { return view.RunRowFromSummary(run) }

func olderRunsURL(slug, cursor string, live map[string]bool) string {
	v := url.Values{}
	v.Set("cursor", cursor)
	if len(live) > 0 {
		keys := make([]string, 0, len(live))
		for k := range live {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		v.Set("live", strings.Join(keys, ","))
	}
	return "/t/" + url.PathEscape(slug) + "/runs?" + v.Encode()
}
