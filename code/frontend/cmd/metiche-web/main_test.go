package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	frontend "github.com/mklfarha/metiche/frontend"
	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/web"
)

// backend answers for the slugs it holds and 404s the rest.
type backend struct {
	mu    sync.Mutex
	teams map[string]string
	hits  map[string]int
}

func (b *backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, _ := strings.CutPrefix(r.URL.Path, "/v1/teams/")
	slug, sub, _ := strings.Cut(rest, "/")
	b.mu.Lock()
	name, ok := b.teams[slug]
	if sub == "" {
		b.hits[slug]++
	}
	b.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch sub {
	case "":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sequence": 1, "board_revision": 1,
			"team":     map[string]any{"key": slug, "name": name},
			"sessions": []any{}, "conflicts": []any{},
		})
	case "contracts":
		_, _ = io.WriteString(w, `{"contracts":[]}`)
	case "decisions":
		_, _ = io.WriteString(w, `{"decisions":[]}`)
	case "stream":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
}

func setup(t *testing.T) (*backend, *httptest.Server, context.Context, *slog.Logger) {
	t.Helper()
	b := &backend{teams: map[string]string{"acme-live": "Acme", "beta-live": "Beta"}, hits: map[string]int{}}
	stub := httptest.NewServer(b)
	t.Cleanup(stub.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); stub.CloseClientConnections() })
	return b, stub, ctx, slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestMixedModeRegistersBoth: with -backend set, the embedded recordings are
// registered as looping demo boards alongside the pre-warmed live team, a
// pre-warm over a demo slug is refused, and discovery serves a team that was
// never named.
func TestMixedModeRegistersBoth(t *testing.T) {
	b, stub, ctx, log := setup(t)
	srv := web.NewServer(frontend.Static(), log)
	err := registerTeams(ctx, srv, options{
		backend: stub.URL, client: stub.Client(), backfill: 0,
		fixtures: frontend.Fixtures(), speed: 6, warmup: 30, loop: false, demo: true,
		teams:    []string{"acme-live", "demo"},
		discover: true, maxTeams: 10,
	}, log)
	if err != nil {
		t.Fatal(err)
	}

	for _, slug := range []string{"demo", "demo-tidewater"} {
		tm, ok := srv.Lookup(slug)
		if !ok || !tm.Demo {
			t.Fatalf("%s: registered=%v, want a demo team", slug, ok)
		}
		fx, isFixture := tm.Feed.(*feed.Fixture)
		if !isFixture || !fx.Loop {
			t.Fatalf("%s: want a looping fixture feed alongside a backend", slug)
		}
	}
	live, ok := srv.Lookup("acme-live")
	if !ok || live.Demo || live.Discovered {
		t.Fatalf("acme-live: registered=%v demo=%v discovered=%v, want a pre-warmed live team", ok, live != nil && live.Demo, live != nil && live.Discovered)
	}
	if _, isLive := live.Feed.(*feed.Live); !isLive {
		t.Fatalf("acme-live has feed %T", live.Feed)
	}
	if b.hits["demo"] != 0 {
		t.Fatalf("the backend was asked about the demo slug")
	}

	h := srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/beta-live", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("discovered team: %d", rec.Code)
	}
	if tm, ok := srv.Lookup("beta-live"); !ok || tm.Demo || !tm.Discovered {
		t.Fatalf("beta-live not registered as a discovered live team")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/demo", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-demo="recording"`) {
		t.Fatalf("/t/demo: %d, labelled=%v", rec.Code, strings.Contains(rec.Body.String(), `data-demo="recording"`))
	}
}

// TestDemoOffWithBackendRegistersOnlyLive: -demo=false leaves no recording up.
func TestDemoOffWithBackendRegistersOnlyLive(t *testing.T) {
	_, stub, ctx, log := setup(t)
	srv := web.NewServer(frontend.Static(), log)
	if err := registerTeams(ctx, srv, options{
		backend: stub.URL, client: stub.Client(), fixtures: frontend.Fixtures(),
		demo: false, teams: []string{"acme-live"},
	}, log); err != nil {
		t.Fatal(err)
	}
	teams := srv.Teams()
	if len(teams) != 1 || teams[0].Slug != "acme-live" || teams[0].Demo {
		t.Fatalf("teams = %+v", teams)
	}
}

// TestFixtureOnlyModeHasNoDiscovery: with no backend, the recordings are all
// there is, and an unknown slug is a plain 404.
func TestFixtureOnlyModeHasNoDiscovery(t *testing.T) {
	_, _, ctx, log := setup(t)
	srv := web.NewServer(frontend.Static(), log)
	if err := registerTeams(ctx, srv, options{
		fixtures: frontend.Fixtures(), speed: 6, warmup: 30, demo: false, teams: []string{"acme-live"},
	}, log); err != nil {
		t.Fatal(err)
	}
	for _, tm := range srv.Teams() {
		if !tm.Demo {
			t.Fatalf("fixture mode registered a live team %s", tm.Slug)
		}
	}
	if len(srv.Teams()) != 2 {
		t.Fatalf("want the 2 embedded recordings, have %d", len(srv.Teams()))
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/acme-live", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestDemoSlug(t *testing.T) {
	for in, want := range map[string]string{"demo": "demo", "tidewater": "demo-tidewater", "demo-x": "demo-x"} {
		if got := demoSlug(in); got != want {
			t.Errorf("demoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
