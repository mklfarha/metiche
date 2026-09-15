package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/model"
)

// stubBackend is the metiche read API as far as discovery cares: a set of
// readable teams (200), a set that fails (500), and 404 for everything else —
// which, as in app/authz, covers both "no such team" and "private".
type stubBackend struct {
	mu      sync.Mutex
	public  map[string]string // slug -> team name
	failing map[string]bool
	hits    map[string]int // GET /v1/teams/{slug}, per slug
	streams map[string]int // GET /v1/teams/{slug}/stream answered 200, per slug
	total   int            // every request of any kind

	// gate, when non-nil, holds the FIRST snapshot read until it is closed.
	gate chan struct{}

	// sessions and conflicts, when non-nil, are what every snapshot carries.
	sessions  []any
	conflicts []any

	// login is the browser-session half of the backend (login_stub_test.go).
	login stubLogin
}

func newStubBackend() *stubBackend {
	return &stubBackend{public: map[string]string{}, failing: map[string]bool{},
		hits: map[string]int{}, streams: map[string]int{}, login: newStubLogin()}
}

func (b *stubBackend) setPublic(slug, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.public[slug] = name
}

// makePrivate is the visibility flip: from now on every read of slug is the
// backend's 404. Connections already open are left alone, as the real
// backend leaves them.
func (b *stubBackend) makePrivate(slug string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.public, slug)
}

func (b *stubBackend) streamsFor(slug string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.streams[slug]
}

func (b *stubBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/browser/") {
		b.serveBrowser(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/teams/")
	b.mu.Lock()
	b.total++
	b.login.observe(r)
	if !ok {
		b.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	slug, sub, _ := strings.Cut(rest, "/")
	if sub == "access" {
		b.mu.Unlock()
		b.serveAccess(w, r, slug)
		return
	}
	if sub == "invites" || strings.HasPrefix(sub, "invites/") {
		b.total-- // serveInvites counts it
		b.mu.Unlock()
		b.serveInvites(w, r, slug, sub)
		return
	}
	name, public := b.public[slug]
	if !public {
		name, public = b.readableLocked(slug, r)
	}
	failing := b.failing[slug]
	gate := b.gate
	if sub == "" {
		b.hits[slug]++
	}
	if sub == "stream" && public && !failing {
		b.streams[slug]++
	}
	sessions, conflicts := b.sessions, b.conflicts
	b.mu.Unlock()
	if sessions == nil {
		sessions = []any{}
	}
	if conflicts == nil {
		conflicts = []any{}
	}

	if failing {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if !public {
		http.NotFound(w, r)
		return
	}
	if sub == "sessions" || strings.HasPrefix(sub, "sessions/") {
		b.serveRuns(w, r, slug, sub)
		return
	}
	if sub == "conflicts/history" || sub == "events" || sub == "graph" {
		b.serveBoardHistory(w, r, slug, sub)
		return
	}
	switch sub {
	case "":
		if gate != nil {
			<-gate
		}
		writeJSON(w, map[string]any{
			"sequence": 3, "board_revision": 1,
			"team":     map[string]any{"key": slug, "name": name, "sequence": 3, "board_revision": 1},
			"sessions": sessions, "conflicts": conflicts,
		})
	case "contracts":
		writeJSON(w, map[string]any{"contracts": []any{}})
	case "decisions":
		writeJSON(w, map[string]any{"decisions": []any{}})
	case "stream":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	default:
		http.NotFound(w, r)
	}
}

func (b *stubBackend) hitsFor(slug string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits[slug]
}

func (b *stubBackend) requests() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// idleFeed stands in for a fixture: it has a name and never says anything.
type idleFeed struct{}

func (idleFeed) Name() string { return "fixture:test" }
func (idleFeed) Stream(ctx context.Context, _ int64) (<-chan model.Event, error) {
	ch := make(chan model.Event)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

type harness struct {
	srv     *Server
	h       http.Handler
	backend *stubBackend
	stub    *httptest.Server
	ctx     context.Context

	probes   atomic.Int64
	newFeeds atomic.Int64
	clock    atomic.Int64 // unix nanos
}

// newHarness builds a server with discovery on against a stub backend. Cleanup
// cancels every feed before the stub closes, so no stream is left open.
func newHarness(t *testing.T, cfg Discovery) *harness {
	t.Helper()
	x := &harness{backend: newStubBackend()}
	x.stub = httptest.NewServer(x.backend)
	t.Cleanup(x.stub.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); x.stub.CloseClientConnections() })
	x.ctx = ctx

	x.clock.Store(time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC).UnixNano())
	x.srv = NewServer(nil, quiet())
	cfg.Probe = func(ctx context.Context, slug string) (feed.ProbeResult, error) {
		x.probes.Add(1)
		return feed.ProbeTeam(ctx, x.stub.Client(), x.stub.URL, slug)
	}
	cfg.NewFeed = func(slug string) feed.Feed {
		x.newFeeds.Add(1)
		return &feed.Live{BaseURL: x.stub.URL, Slug: slug, Client: x.stub.Client(), Logger: quiet()}
	}
	cfg.Now = func() time.Time { return time.Unix(0, x.clock.Load()) }
	x.srv.EnableDiscovery(ctx, cfg)
	x.h = x.srv.Handler()
	return x
}

func (x *harness) get(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func (x *harness) advance(d time.Duration) { x.clock.Add(int64(d)) }

func liveTeams(s *Server) []*Team {
	var out []*Team
	for _, t := range s.Teams() {
		if !t.Demo {
			out = append(out, t)
		}
	}
	return out
}

// TestDiscoverySingleflight: many concurrent first requests for one slug make
// ONE probe and ONE feed, and every one of them gets the board.
func TestDiscoverySingleflight(t *testing.T) {
	x := newHarness(t, Discovery{})
	x.backend.public["acme-live"] = "Acme"
	gate := make(chan struct{})
	x.backend.gate = gate

	const n = 32
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = x.get("/t/acme-live").Code
		}(i)
	}
	// Let the probe get stuck on the gate and the rest pile up behind it.
	deadline := time.Now().Add(5 * time.Second)
	for x.probes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	x.backend.mu.Lock()
	x.backend.gate = nil
	x.backend.mu.Unlock()
	close(gate)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, c)
		}
	}
	if got := x.probes.Load(); got != 1 {
		t.Errorf("probes = %d, want 1", got)
	}
	if got := x.newFeeds.Load(); got != 1 {
		t.Errorf("feeds created = %d, want 1", got)
	}
	if got := len(liveTeams(x.srv)); got != 1 {
		t.Errorf("live teams registered = %d, want 1", got)
	}
	// And once registered, the backend is not asked again.
	before := x.probes.Load()
	for i := 0; i < 5; i++ {
		x.get("/t/acme-live")
	}
	if x.probes.Load() != before {
		t.Errorf("a registered team was probed again")
	}
}

// TestDiscoveryNegativeCache: a 404 is remembered, so hammering a junk slug
// reaches the backend once per TTL.
func TestDiscoveryNegativeCache(t *testing.T) {
	x := newHarness(t, Discovery{NotFoundTTL: 30 * time.Second})

	for i := 0; i < 5; i++ {
		if rec := x.get("/t/random-junk"); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %d: status %d, want 404", i, rec.Code)
		}
	}
	if got := x.backend.hitsFor("random-junk"); got != 1 {
		t.Fatalf("backend was asked %d times for a remembered 404, want 1", got)
	}

	x.advance(29 * time.Second)
	x.get("/t/random-junk")
	if got := x.backend.hitsFor("random-junk"); got != 1 {
		t.Fatalf("asked again inside the TTL: %d", got)
	}

	// Past the TTL it is asked again — a team created meanwhile must appear.
	x.backend.public["random-junk"] = "Created Later"
	x.advance(2 * time.Second)
	if rec := x.get("/t/random-junk"); rec.Code != http.StatusOK {
		t.Fatalf("after TTL: status %d, want 200", rec.Code)
	}
	if got := x.probes.Load(); got != 2 {
		t.Fatalf("probes = %d, want 2", got)
	}
}

// TestDiscoveryBackendErrorIsNotANotFound: a 500 is a 503 to the viewer, is
// remembered only briefly, and creates nothing.
func TestDiscoveryBackendErrorIsNotANotFound(t *testing.T) {
	x := newHarness(t, Discovery{ErrorTTL: 5 * time.Second})
	x.backend.failing["flaky-team"] = true

	for i := 0; i < 3; i++ {
		if rec := x.get("/t/flaky-team"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", rec.Code)
		}
	}
	if got := x.backend.hitsFor("flaky-team"); got != 1 {
		t.Fatalf("backend asked %d times during the error TTL, want 1", got)
	}
	x.advance(6 * time.Second)
	x.get("/t/flaky-team")
	if got := x.backend.hitsFor("flaky-team"); got != 2 {
		t.Fatalf("backend asked %d times after the error TTL, want 2", got)
	}
}

// TestDiscoveryCap: at the cap nothing new is probed, and a 404 does not use
// up a slot.
func TestDiscoveryCap(t *testing.T) {
	x := newHarness(t, Discovery{MaxTeams: 1})
	x.backend.public["acme-live"] = "Acme"
	x.backend.public["beta-live"] = "Beta"

	if rec := x.get("/t/nope-nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("junk: %d", rec.Code)
	}
	if rec := x.get("/t/acme-live"); rec.Code != http.StatusOK {
		t.Fatalf("first team: status %d, want 200 (a 404 must not consume the only slot)", rec.Code)
	}
	if rec := x.get("/t/beta-live"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("over the cap: status %d, want 503", rec.Code)
	}
	if got := x.backend.hitsFor("beta-live"); got != 0 {
		t.Fatalf("the backend was asked about a slug over the cap (%d)", got)
	}
	if rec := x.get("/t/beta-live/stream"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stream over the cap: status %d, want 503", rec.Code)
	}
	if got := x.newFeeds.Load(); got != 1 {
		t.Fatalf("feeds = %d, want 1", got)
	}
	// The team already registered keeps working.
	if rec := x.get("/t/acme-live"); rec.Code != http.StatusOK {
		t.Fatalf("registered team at the cap: %d", rec.Code)
	}
}

// TestDiscoveryRejectsImplausibleSlugs: anything that is not slug-shaped is a
// 404 without a single backend request.
func TestDiscoveryRejectsImplausibleSlugs(t *testing.T) {
	x := newHarness(t, Discovery{})
	paths := []string{
		"/t/Acme-Live",
		"/t/acme_live",
		"/t/-acme",
		"/t/acme-",
		"/t/acme.live",
		"/t/acme%20live",
		"/t/%2e%2e",
		"/t/" + strings.Repeat("a", 65),
		"/t/acme%2Flive",
		"/t/ac%00me",
	}
	for _, p := range paths {
		rec := x.get(p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, rec.Code)
		}
	}
	if got := x.backend.requests(); got != 0 {
		t.Fatalf("backend received %d requests for implausible slugs, want 0", got)
	}

	for slug, want := range map[string]bool{
		"a": true, "acme": true, "acme-live-3f9a2c": true, strings.Repeat("a", 64): true,
		"6d657469-6368-4520-9465-616d73000001": true, // a team uuid, which the backend also accepts
		"":                                     false, "A": false, "acme--": false, "acme_live": false, strings.Repeat("a", 65): false,
		"../etc": false, "acme live": false,
	} {
		if got := ValidSlug(slug); got != want {
			t.Errorf("ValidSlug(%q) = %v, want %v", slug, got, want)
		}
	}
}

// TestDiscoveryNeverCreatesAFeedForAnUnconfirmedSlug: private (404), missing
// (404) and failing (500) slugs register nothing and build no feed.
func TestDiscoveryNeverCreatesAFeedForAnUnconfirmedSlug(t *testing.T) {
	x := newHarness(t, Discovery{})
	x.backend.failing["broken-team"] = true
	// "private-team" is simply absent from public: the backend's 404.

	for _, p := range []string{
		"/t/private-team", "/t/private-team/stream", "/t/private-team/graph",
		"/t/does-not-exist", "/t/broken-team",
	} {
		rec := x.get(p)
		if rec.Code == http.StatusOK {
			t.Fatalf("GET %s: 200 for an unconfirmed slug", p)
		}
	}
	if got := x.newFeeds.Load(); got != 0 {
		t.Fatalf("feeds created = %d, want 0", got)
	}
	for _, slug := range []string{"private-team", "does-not-exist", "broken-team"} {
		if _, ok := x.srv.Lookup(slug); ok {
			t.Fatalf("%s was registered", slug)
		}
	}
	if len(x.srv.Teams()) != 0 {
		t.Fatalf("teams registered: %d", len(x.srv.Teams()))
	}
}

// TestDemoSlugIsNeverLookedUpOnTheBackend: a registered demo holds its slug,
// even when a real team by that name exists.
func TestDemoSlugIsNeverLookedUpOnTheBackend(t *testing.T) {
	x := newHarness(t, Discovery{})
	x.backend.public["demo"] = "A Real Team Called Demo"
	if _, err := x.srv.AddDemoTeam(x.ctx, "demo", "Orbital Freight", "", idleFeed{}); err != nil {
		t.Fatal(err)
	}
	rec := x.get("/t/demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := x.backend.hitsFor("demo"); got != 0 {
		t.Fatalf("backend asked about a demo slug %d times", got)
	}
	if strings.Contains(rec.Body.String(), "A Real Team Called Demo") {
		t.Fatal("the backend team shadowed the demo")
	}
	if _, err := x.srv.AddLiveTeam(x.ctx, "demo", idleFeed{}); err == nil {
		t.Fatal("a live team was registered over a demo slug")
	}
}

func ExampleValidSlug() {
	fmt.Println(ValidSlug("acme-live"), ValidSlug("../admin"))
	// Output: true false
}
