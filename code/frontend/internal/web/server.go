// Package web is the HTTP surface: five pages, one SSE stream, two controls.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/hub"
	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
	"github.com/mklfarha/metiche/frontend/internal/view"
)

// Team is one board this service serves.
type Team struct {
	Slug     string
	Name     string
	JoinCode string
	Hub      *hub.Hub
	Feed     feed.Feed
}

// Server holds the teams and the static assets.
type Server struct {
	teams  map[string]*Team
	order  []string
	static fs.FS
	log    *slog.Logger
}

// NewServer builds the server. Teams are registered before Handler is called.
func NewServer(static fs.FS, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{teams: map[string]*Team{}, static: static, log: log}
}

// AddTeam registers a team and starts its feed.
func (s *Server) AddTeam(ctx context.Context, slug, name, joinCode string, f feed.Feed) (*Team, error) {
	h := hub.New(slug, name, view.Renderer{}, s.log)
	if err := h.Run(ctx, f); err != nil {
		return nil, fmt.Errorf("start feed for %s: %w", slug, err)
	}
	t := &Team{Slug: slug, Name: name, JoinCode: joinCode, Hub: h, Feed: f}
	s.teams[slug] = t
	s.order = append(s.order, slug)
	return t, nil
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(recoverer(s.log))

	r.Handle("/static/*", http.StripPrefix("/static/", cacheless(http.FileServer(http.FS(s.static)))))
	r.Get("/healthz", s.healthz)
	r.Get("/", s.join)
	r.Get("/join", s.joinCode)

	r.Route("/t/{slug}", func(r chi.Router) {
		r.Get("/", s.board)
		r.Get("/stream", s.stream)
		r.Get("/graph", s.graph)
		r.Get("/conflicts", s.conflicts)
		r.Get("/contracts", s.contracts)
		r.Get("/decisions", s.decisions)
		r.Get("/runs", s.runs)
		r.Get("/runs/{session}", s.run)
		r.Post("/conflicts/{key}/resolve", s.resolve)
		r.Post("/sessions/{session}/nudge", s.nudge)
		r.Post("/cadence", s.cadence)
	})
	return r
}

// ---------------------------------------------------------------- pages

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	cards := make([]view.TeamCard, 0, len(s.order))
	for _, slug := range s.order {
		t := s.teams[slug]
		snap := t.Hub.Snapshot()
		cards = append(cards, view.TeamCard{
			Slug: t.Slug, Name: t.Name,
			Live: snap.LiveSessions(), Members: len(snap.Members),
			Open: len(snap.OpenConflicts()), Severity: firstNonEmpty(snap.WorstOpen(), "low"),
		})
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].Live > cards[j].Live })
	s.render(w, r, view.JoinPage(cards))
}

func (s *Server) joinCode(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	for _, slug := range s.order {
		if strings.EqualFold(s.teams[slug].JoinCode, code) {
			http.Redirect(w, r, "/t/"+slug, http.StatusSeeOther)
			return
		}
	}
	// Wrong codes must not be distinguishable from unknown teams, and this
	// service does not mint tokens — the backend will.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	t, snap, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabBoard, view.BoardPage(snap))
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	t, snap, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabGraph, view.GraphPage(snap))
}

func (s *Server) conflicts(w http.ResponseWriter, r *http.Request) {
	t, snap, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabConflicts, view.ConflictsPage(snap))
}

func (s *Server) contracts(w http.ResponseWriter, r *http.Request) {
	t, snap, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabContracts, view.ContractsPage(snap))
}

func (s *Server) decisions(w http.ResponseWriter, r *http.Request) {
	t, snap, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabDecisions, view.DecisionsPage(snap))
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	t, snap, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabRuns, view.RunsPage(snap))
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	t, snap, ok := s.team(w, r)
	if !ok {
		return
	}
	sess := snap.Session(chi.URLParam(r, "session"))
	if sess == nil {
		w.WriteHeader(http.StatusNotFound)
		s.page(w, r, t, snap, view.TabRuns, view.RunsPage(snap))
		return
	}
	s.page(w, r, t, snap, view.TabRuns, view.RunPage(snap, sess))
}

// ---------------------------------------------------------------- controls

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.team(w, r)
	if !ok {
		return
	}
	key := chi.URLParam(r, "key")
	status := firstNonEmpty(r.FormValue("status"), "resolved")

	payload, _ := json.Marshal(map[string]string{
		"conflict_key": key, "status": status, "resolution": status + " from the board",
	})
	t.Hub.Inject(model.Event{
		Kind:       "conflict_resolved",
		Summary:    fmt.Sprintf("%s marked %s from the board", key, status),
		OccurredAt: time.Now(),
		Structural: true,
		Payload:    payload,
	})
	// The page repaints from the board frame the injection just broadcast, so
	// the response body has nothing to say.
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// cadence changes the project's pace from the board. It is structural: every
// suggested action on the page is written in that register, so the board has
// to repaint.
func (s *Server) cadence(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.team(w, r)
	if !ok {
		return
	}
	next := model.Cadence(strings.TrimSpace(r.FormValue("cadence")))
	if !next.Valid() {
		http.Error(w, "unknown cadence", http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(map[string]string{"cadence": string(next)})
	t.Hub.Inject(model.Event{
		Kind:       "project_cadence_changed",
		Summary:    fmt.Sprintf("project pace set to %s from the board", next.Label()),
		OccurredAt: time.Now(),
		Structural: true,
		Payload:    payload,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) nudge(w http.ResponseWriter, r *http.Request) {
	t, snap, ok := s.team(w, r)
	if !ok {
		return
	}
	sessionKey := chi.URLParam(r, "session")
	kind := firstNonEmpty(r.FormValue("nudge"), "ask")
	payload, _ := json.Marshal(map[string]string{"session_key": sessionKey, "kind": kind})
	// A nudge is deliberately NOT structural: it belongs in the log, and
	// repainting everybody's board because one human poked one agent is
	// exactly the noise the two-cursor split exists to avoid.
	t.Hub.Inject(model.Event{
		Kind:       "instruction_delivered",
		Summary:    fmt.Sprintf("nudge (%s) queued for %s — delivered on its next call", kind, snap.SessionLabel(sessionKey)),
		SessionKey: sessionKey,
		OccurredAt: time.Now(),
		Structural: false,
		Payload:    payload,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- stream

// stream is the SSE endpoint. It emits exactly two named event types, both
// carrying HTML:
//
//	event: timeline   one <li>, appended        (every event)
//	event: board      the board container       (structural events only)
//
// Cursor handling is the part that has to be right. A client reconnecting
// must not miss an event and must not apply one twice:
//
//   - the cursor comes from Last-Event-ID when the browser supplies it (it
//     always does on an automatic EventSource reconnect) and from ?after=
//     otherwise, so a cold load can ask for a specific point too;
//   - we subscribe BEFORE reading the backlog, so an event committed during
//     the replay is queued rather than lost;
//   - frames already covered by the replay are then skipped by sequence, so
//     that same event is not delivered twice;
//   - the board frame is sent last and is idempotent by construction, since
//     it replaces the container outright.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	t, ok := s.teams[chi.URLParam(r, "slug")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx would otherwise buffer this to death
	w.WriteHeader(http.StatusOK)

	sub := t.Hub.Subscribe()
	defer t.Hub.Unsubscribe(sub)

	// Tell the client how long to wait before reconnecting, then replay.
	fmt.Fprint(w, "retry: 1000\n\n")

	var replayed int64
	for _, f := range t.Hub.Replay(after) {
		writeFrame(w, f)
		if f.Name == "timeline" && f.ID > replayed {
			replayed = f.ID
		}
	}
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case f, open := <-sub.C:
			if !open {
				return
			}
			if f.Name == "timeline" && f.ID <= replayed {
				continue // already covered by the replay
			}
			writeFrame(w, f)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// writeFrame serialises one frame. HTML fragments contain newlines, and a bare
// newline would end the SSE message, so every line gets its own data: field —
// the browser rejoins them with \n, which is exactly what the HTML needs.
func writeFrame(w http.ResponseWriter, f hub.Frame) {
	fmt.Fprintf(w, "id: %d\nevent: %s\n", f.ID, f.Name)
	for _, line := range strings.Split(f.HTML, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

// ---------------------------------------------------------------- plumbing

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	type teamHealth struct {
		Slug          string `json:"slug"`
		Feed          string `json:"feed"`
		Sequence      int64  `json:"sequence"`
		BoardRevision int64  `json:"board_revision"`
		Subscribers   int    `json:"subscribers"`
	}
	out := struct {
		OK    bool         `json:"ok"`
		Teams []teamHealth `json:"teams"`
	}{OK: true}
	for _, slug := range s.order {
		t := s.teams[slug]
		snap := t.Hub.Snapshot()
		out.Teams = append(out.Teams, teamHealth{
			Slug: slug, Feed: t.Feed.Name(),
			Sequence: snap.Team.Sequence, BoardRevision: snap.Team.BoardRevision,
			Subscribers: t.Hub.Subscribers(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) team(w http.ResponseWriter, r *http.Request) (*Team, state.Snapshot, bool) {
	slug := chi.URLParam(r, "slug")
	t, ok := s.teams[slug]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, r, view.NotFound(slug))
		return nil, state.Snapshot{}, false
	}
	return t, t.Hub.Snapshot(), true
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, t *Team, snap state.Snapshot, tab view.Tab, body templ.Component) {
	streamURL := fmt.Sprintf("/t/%s/stream?after=%d", t.Slug, snap.Team.Sequence)
	s.render(w, r, view.Layout(snap, tab, streamURL, body))
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		s.log.Error("render failed", "path", r.URL.Path, "err", err)
	}
}

func cacheless(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic serving request", "path", r.URL.Path, "panic", v)
					http.Error(w, "internal error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
