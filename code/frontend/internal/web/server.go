// Package web is the HTTP surface: five pages, one SSE stream, no controls,
// and — against a real backend — on-demand registration of the teams people
// ask for (discovery.go), sign-in with a link minted from a terminal
// (signin.go, viewer.go), and private boards read with each signed-in viewer's
// own session (viewerboards.go).
//
// Every board is READ-ONLY. Signing in changes what a viewer may SEE, never
// what anybody can do: nothing this service serves changes a board.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/hub"
	"github.com/mklfarha/metiche/frontend/internal/state"
	"github.com/mklfarha/metiche/frontend/internal/view"
)

// Team is one board this service serves.
type Team struct {
	Slug string
	Name string
	// JoinCode is only ever set on a demo team, whose recording carries a
	// made-up one. A live team's invite codes are the backend's business and
	// this service never learns them.
	JoinCode string
	// Demo is true for a recording replayed from a fixture and false for a
	// real team read from the backend. It is decided once, by whoever
	// registered the team, and every public surface filters on it: a live
	// team never appears on /, /teams, /join or /healthz, and a demo board
	// always says it is a recording.
	Demo bool
	// Discovered is true for a live team registered on demand, because
	// somebody asked for its board and the backend confirmed it, rather than
	// named up front with -teams.
	Discovered bool
	Hub        *hub.Hub
	Feed       feed.Feed

	// viewer is true for a private board held for ONE signed-in viewer and
	// read with that viewer's session. Such a team lives in viewerBoards and
	// is never in Server.teams, never in Teams(), and never resolved for
	// anybody else.
	viewer bool

	// ctx is the team's own lifetime: cancel ends it, which stops the feed and
	// anything else discovery runs for this team.
	ctx    context.Context
	cancel context.CancelFunc
}

// Server holds the teams and the static assets.
//
// teams is written after Handler is serving — discovery registers boards on
// demand — so every read and write goes through mu.
type Server struct {
	mu     sync.RWMutex
	teams  map[string]*Team
	order  []string
	static fs.FS
	log    *slog.Logger

	disc  *discovery
	login *login
}

// NewServer builds the server. Demo and pre-warmed teams are registered before
// Handler is called; discovered ones are registered while it serves.
func NewServer(static fs.FS, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{teams: map[string]*Team{}, static: static, log: log}
}

// AddDemoTeam registers a recording. It is labelled a demo on every page, it
// is the only kind of team the public pages list, and its slug is reserved:
// once a demo holds a slug, the backend is never asked about that slug and a
// live team cannot be registered over it.
func (s *Server) AddDemoTeam(ctx context.Context, slug, name, joinCode string, f feed.Feed) (*Team, error) {
	return s.addTeam(ctx, slug, name, joinCode, true, false, f)
}

// AddLiveTeam registers a real team read from the backend. It carries no join
// code and is never listed on a public page.
func (s *Server) AddLiveTeam(ctx context.Context, slug string, f feed.Feed) (*Team, error) {
	return s.addTeam(ctx, slug, slug, "", false, false, f)
}

func (s *Server) addTeam(ctx context.Context, slug, name, joinCode string, demo, discovered bool, f feed.Feed) (*Team, error) {
	if existing, ok := s.Lookup(slug); ok {
		kind := "live"
		if existing.Demo {
			kind = "demo"
		}
		return nil, fmt.Errorf("team %q is already registered as a %s board", slug, kind)
	}
	t, err := s.startTeam(ctx, slug, name, joinCode, demo, discovered, f)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.teams[slug]; ok {
		t.cancel()
		kind := "live"
		if existing.Demo {
			kind = "demo"
		}
		return nil, fmt.Errorf("team %q is already registered as a %s board", slug, kind)
	}
	s.teams[slug] = t
	s.order = append(s.order, slug)
	return t, nil
}

// startTeam runs a feed into a new hub. It registers nothing: addTeam puts the
// result in the shared registry, viewerBoards keeps it for one viewer.
func (s *Server) startTeam(ctx context.Context, slug, name, joinCode string, demo, discovered bool, f feed.Feed) (*Team, error) {
	// Each team gets its own context so that one which loses a registration
	// race can be stopped without touching anybody else's feed.
	ctx, cancel := context.WithCancel(ctx)
	h := hub.New(slug, name, view.Renderer{}, s.log)
	if err := h.Run(ctx, f); err != nil {
		cancel()
		return nil, fmt.Errorf("start feed: %w", err)
	}
	// A live feed learns the team's real name from its snapshot during Run;
	// a fixture was named by its caller. Take whichever is better.
	if loaded := h.Snapshot().Team.Name; loaded != "" {
		name = loaded
	}
	return &Team{Slug: slug, Name: name, JoinCode: joinCode, Demo: demo, Discovered: discovered,
		Hub: h, Feed: f, ctx: ctx, cancel: cancel}, nil
}

// Lookup returns a registered team. It never asks the backend.
func (s *Server) Lookup(slug string) (*Team, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.teams[slug]
	return t, ok
}

// Teams returns every registered team in registration order.
func (s *Server) Teams() []*Team {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Team, 0, len(s.order))
	for _, slug := range s.order {
		out = append(out, s.teams[slug])
	}
	return out
}

// demoTeams is the only list a public page may be built from.
func (s *Server) demoTeams() []*Team {
	var out []*Team
	for _, t := range s.Teams() {
		if t.Demo {
			out = append(out, t)
		}
	}
	return out
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(recoverer(s.log))
	r.Use(baseHeaders)
	// A GET is never state-changing, and a method a path does not take is the
	// same 404 as a path that does not exist.
	r.MethodNotAllowed(http.NotFound)

	r.Handle("/static/*", http.StripPrefix("/static/", cacheless(http.FileServer(http.FS(s.static)))))

	// THE LANDING PAGE'S ONE COMMAND POINTS HERE, so this route is not a
	// convenience: `curl -fsSL https://metiche.xyz/install.sh | sh` is the
	// headline instruction on the page this same server renders, and without
	// it the first thing anyone does returns 404.
	//
	// Served from the embedded copy at static/install.sh, which
	// install_sh_test.go pins byte-for-byte against the canonical script at
	// the repository root. text/plain rather than a shell media type: the
	// browsers people paste this into should show it, because reading a
	// script before piping it to sh is a thing to encourage.
	r.Get("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(s.static, "install.sh")
		if err != nil {
			http.Error(w, "install.sh is not available", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	r.Get("/healthz", s.healthz)
	// The landing page is the front door: it explains the idea to somebody who
	// has never heard of it, and it deliberately shows no team data. The team
	// picker moved to /teams, which is only useful once you already know what
	// this is.
	r.Get("/", s.landing)
	r.Get("/teams", s.join)
	r.Get("/join", s.joinCode)

	// Signing in (signin.go). /signin is served even without a backend, so the
	// page that explains how to sign in always exists; everything else answers
	// only when login is on.
	r.Get("/signin", s.signinPage)
	r.Post("/signin", s.signinPost)
	r.Post("/signout", s.signout)
	r.Get("/account", s.account)
	r.Post("/account/signout-all", s.signoutAll)
	r.Post("/account/sessions/{key}/revoke", s.revokeSession)

	r.Route("/t/{slug}", func(r chi.Router) {
		r.Get("/", s.board)
		r.Get("/stream", s.stream)
		r.Get("/graph", s.graph)
		r.Get("/conflicts", s.conflicts)
		r.Get("/contracts", s.contracts)
		r.Get("/decisions", s.decisions)
		r.Get("/runs", s.runs)
		r.Get("/runs/{session}", s.run)
		// Invites (invites.go): the board's only writes, for a signed-in
		// member with the CSRF token. Everybody else gets the NotFound page.
		r.Get("/invites", s.invites)
		r.Post("/invites", s.createInvite)
		r.Post("/invites/{invite}/revoke", s.revokeInvite)
		// These three paths were board controls. They had no authentication,
		// and each applied a made-up event to the board and broadcast it to
		// every viewer — of a real team, or of the shared demo. They now answer
		// exactly what an unknown slug answers, for every team, without looking
		// the team up: nothing about the response says whether the slug exists.
		// They are spelled out, rather than left to the router's default 404,
		// only so the page is the same NotFound page a board URL gets.
		r.Post("/conflicts/{key}/resolve", s.noControls)
		r.Post("/sessions/{session}/nudge", s.noControls)
		r.Post("/cadence", s.noControls)
	})
	return r
}

// ---------------------------------------------------------------- pages

// join serves /teams. It lists DEMO teams only — plus, for a signed-in viewer,
// that viewer's own teams as the backend lists them.
//
// In fixture mode that was every team, and harmless. Against a real backend a
// directory of registered teams with their live-session counts is a public
// index of who is building what — and with discovery, of every public team
// anybody has ever typed into the address bar. A real team's board is reached
// by its URL, which the page says, never by browsing. "Your teams" comes from
// the backend for this viewer's session, never from the registry.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var yours []feed.BrowserTeam
	if v, vs := s.viewer(w, r); vs == viewerSignedIn {
		teams, err := s.login.cfg.Backend.Teams(r.Context(), v.secret)
		switch {
		case errors.Is(err, feed.ErrSessionInvalid):
			s.login.sessionEnded(v.hash)
			s.login.clearCookie(w)
		case err != nil:
			s.log.Warn("listing a viewer's teams failed", "err", err)
			r = s.withViewer(r, v)
		default:
			yours = teams
			if yours == nil {
				yours = []feed.BrowserTeam{}
			}
			r = s.withViewer(r, v)
		}
	}
	demos := s.demoTeams()
	cards := make([]view.TeamCard, 0, len(demos))
	for _, t := range demos {
		snap := t.Hub.Snapshot()
		cards = append(cards, view.TeamCard{
			Slug: t.Slug, Name: t.Name,
			Live: snap.LiveSessions(), Members: len(snap.Members),
			Open: len(snap.OpenConflicts()), Severity: firstNonEmpty(snap.WorstOpen(), "low"),
		})
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].Live > cards[j].Live })
	s.render(w, r, view.TeamsPage(view.TeamsParams{Demos: cards, Mine: yourTeams(yours)}))
}

// landing serves the public page. It is handed at most one board URL, for the
// "see a live demo" link, and that URL is only ever a DEMO board: the landing
// page is shown to every visitor, and pointing it at the first registered
// team advertised a real team to all of them the moment the board ran against
// a backend. With no demo registered it gets "" and the link is omitted
// rather than pointing at a 404 — or at somebody's team.
func (s *Server) landing(w http.ResponseWriter, r *http.Request) {
	boardURL := ""
	if demos := s.demoTeams(); len(demos) > 0 {
		boardURL = "/t/" + demos[0].Slug
	}
	s.render(w, r, view.Landing(boardURL))
}

// joinCode matches demo join codes only. A live team's invite codes live in
// the backend and are redeemed by an agent there; the board never holds one,
// so there is nothing to compare against.
func (s *Server) joinCode(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	for _, t := range s.demoTeams() {
		if code != "" && t.JoinCode != "" && strings.EqualFold(t.JoinCode, code) {
			http.Redirect(w, r, "/t/"+t.Slug, http.StatusSeeOther)
			return
		}
	}
	// Wrong codes must not be distinguishable from unknown teams, and this
	// service does not mint tokens — the backend will.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabBoard, view.BoardPage(snap))
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabGraph, view.GraphPage(snap))
}

func (s *Server) conflicts(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabConflicts, view.ConflictsPage(snap))
}

func (s *Server) contracts(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabContracts, view.ContractsPage(snap))
}

func (s *Server) decisions(w http.ResponseWriter, r *http.Request) {
	t, snap, r, ok := s.team(w, r)
	if !ok {
		return
	}
	s.page(w, r, t, snap, view.TabDecisions, view.DecisionsPage(snap))
}

// runs and run, the Runs page and one run, are in runs.go.

// ---------------------------------------------------------------- controls

// noControls answers the old control paths — for a live team, a demo, or a
// slug nobody has heard of — with the same 404 page an unknown board gets.
//
// Demo boards get no special case. A demo is ONE shared hub per recording,
// so anything applied server-side is seen by every visitor watching it;
// keeping a per-viewer copy would need per-viewer state that the next shared
// board frame repaints over, and a CSRF defence for a POST that has nothing
// real behind it. The recording already resolves its own conflicts on screen,
// which is what the demo is there to show.
//
// It never resolves the team, so it never registers one through discovery,
// never asks the backend, and never touches a hub.
func (s *Server) noControls(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, view.NotFound(chi.URLParam(r, "slug")))
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
	// The stream resolves exactly as the page does. After a restart a browser
	// still holding a discovered team's page reconnects HERE first, and must
	// get the board back rather than a 404 it would retry forever.
	v, vs := s.viewer(w, r)
	t, res := s.resolveTeam(r.Context(), v, vs, chi.URLParam(r, "slug"))
	switch res {
	case resolveFound:
	case resolveUnavailable, resolveFull:
		http.Error(w, "board unavailable, try again shortly", http.StatusServiceUnavailable)
		return
	case resolveSessionOver:
		s.login.clearCookie(w)
		http.NotFound(w, r)
		return
	default:
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Subscribe before the status is spent. A team taken down between
	// resolveTeam and here has a closed hub, and its browsers get the same 404
	// as the next page load rather than one last replay of the board.
	sub := t.Hub.Subscribe()
	defer t.Hub.Unsubscribe(sub)
	if t.Hub.Closed() {
		http.NotFound(w, r)
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
	if t.viewer {
		h.Set("Cache-Control", "private, no-store, no-cache, no-transform")
	} else {
		h.Set("Cache-Control", "no-cache, no-transform")
	}
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx would otherwise buffer this to death
	w.WriteHeader(http.StatusOK)

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

	// A private board's stream re-asks the backend whether this viewer may
	// still read it, every Reauth. Without this a member removed from the
	// team, or a revoked session, keeps receiving events for as long as the
	// tab stays open.
	var reauth <-chan time.Time
	if t.viewer {
		tick := time.NewTicker(s.login.cfg.Reauth)
		defer tick.Stop()
		reauth = tick.C
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-reauth:
			if !s.reauthorize(r.Context(), v, t) {
				return
			}
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

// healthz is served on the public host (the ingress routes everything here),
// so it names demo teams only. Live teams are counted, never named: the probe
// needs a 200, and an operator who needs a live team's cursors has the logs.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	type teamHealth struct {
		Slug          string `json:"slug"`
		Feed          string `json:"feed"`
		Sequence      int64  `json:"sequence"`
		BoardRevision int64  `json:"board_revision"`
		Subscribers   int    `json:"subscribers"`
	}
	out := struct {
		OK              bool         `json:"ok"`
		Teams           []teamHealth `json:"teams"`
		LiveTeams       int          `json:"live_teams"`
		DiscoveredTeams int          `json:"discovered_teams"`
	}{OK: true}
	for _, t := range s.Teams() {
		if !t.Demo {
			out.LiveTeams++
			if t.Discovered {
				out.DiscoveredTeams++
			}
			continue
		}
		snap := t.Hub.Snapshot()
		out.Teams = append(out.Teams, teamHealth{
			Slug: t.Slug, Feed: t.Feed.Name(),
			Sequence: snap.Team.Sequence, BoardRevision: snap.Team.BoardRevision,
			Subscribers: t.Hub.Subscribers(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// team resolves a board request for its viewer. The returned request carries
// the signed-in mark for the render.
func (s *Server) team(w http.ResponseWriter, r *http.Request) (*Team, state.Snapshot, *http.Request, bool) {
	slug := chi.URLParam(r, "slug")
	v, vs := s.viewer(w, r)
	anon := r
	r = s.withViewer(r, v)
	t, res := s.resolveTeam(r.Context(), v, vs, slug)
	switch res {
	case resolveFound:
		// The Invites tab is for a live member only (invites.go).
		if vs == viewerSignedIn && s.viewerIsMember(r.Context(), v, t) {
			r = memberRequest(r)
		}
		return t, t.Hub.Snapshot(), r, true
	case resolveUnavailable, resolveFull:
		// Not a verdict about the team — the backend is unreachable or this
		// process is at its cap — so it must not read as "no such team".
		http.Error(w, "this board is unavailable right now; try again shortly", http.StatusServiceUnavailable)
	case resolveSessionOver:
		// The session ended between the cache's answer and this request: the
		// cookie goes now, and the page is the one an anonymous request gets,
		// as it is when viewer itself hears the 401.
		s.login.clearCookie(w)
		r = anon
		w.WriteHeader(http.StatusNotFound)
		s.render(w, r, view.NotFound(slug))
	default:
		w.WriteHeader(http.StatusNotFound)
		s.render(w, r, view.NotFound(slug))
	}
	return nil, state.Snapshot{}, r, false
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, t *Team, snap state.Snapshot, tab view.Tab, body templ.Component) {
	streamURL := fmt.Sprintf("/t/%s/stream?after=%d", t.Slug, snap.Team.Sequence)
	if t.Demo {
		r = r.WithContext(view.WithDemo(r.Context()))
	}
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
