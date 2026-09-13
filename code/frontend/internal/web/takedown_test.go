package web

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// A board discovered while its team was public must stop being served when the
// team goes private or away. These tests flip the stub backend to 404 under an
// open board and assert the whole takedown: registry, slot, negative cache,
// SSE streams, feed goroutine, re-probe goroutine.

// goroutinesRunning counts live goroutines whose stack contains fn.
func goroutinesRunning(fn string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, fn) {
			count++
		}
	}
	return count
}

const (
	liveStreamLoop = "feed.(*Live).Stream.func1"
	recheckLoop    = "web.(*discovery).recheck"
)

// openStream connects a real SSE client to a board and returns a channel that
// closes when the server ends the response.
func openStream(t *testing.T, base, slug string) (ended <-chan struct{}) {
	t.Helper()
	resp, err := http.Get(base + "/t/" + slug + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream %s: %d", slug, resp.StatusCode)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
		}
	}()
	t.Cleanup(func() { _ = resp.Body.Close() })
	return done
}

func (x *harness) discoveredSlots() int {
	x.srv.disc.mu.Lock()
	defer x.srv.disc.mu.Unlock()
	return x.srv.disc.discovered
}

func (x *harness) remembered404(slug string) bool {
	x.srv.disc.mu.Lock()
	defer x.srv.disc.mu.Unlock()
	m, ok := x.srv.disc.negative[slug]
	return ok && m.result == resolveNotFound
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	ts := httptest.NewServer(h)
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close() })
	return ts
}

// assertTakenDown checks everything a takedown promises for tm.
func assertTakenDown(t *testing.T, x *harness, ts *httptest.Server, tm *Team, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the open SSE stream was not closed")
	}
	select {
	case <-tm.Hub.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the hub's feed goroutine did not exit")
	}
	waitUntil(t, "the live feed's stream goroutine to exit", func() bool { return goroutinesRunning(liveStreamLoop) == 0 })
	waitUntil(t, "the re-probe goroutine to exit", func() bool { return goroutinesRunning(recheckLoop) == 0 })

	if _, ok := x.srv.Lookup(tm.Slug); ok {
		t.Fatal("the team is still registered")
	}
	if got := x.discoveredSlots(); got != 0 {
		t.Fatalf("discovery slots in use = %d, want 0", got)
	}
	if !x.remembered404(tm.Slug) {
		t.Fatal("the 404 is not in the negative cache")
	}
	if !tm.Hub.Closed() {
		t.Fatal("the hub was not closed")
	}

	probes := x.probes.Load()
	rec := x.get("/t/" + tm.Slug)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no such team") {
		t.Fatalf("next page load: %d, want the 404 page", rec.Code)
	}
	if x.probes.Load() != probes {
		t.Fatal("the next page load probed the backend despite the remembered 404")
	}
	resp, err := http.Get(ts.URL + "/t/" + tm.Slug + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reconnecting stream: %d, want 404", resp.StatusCode)
	}
}

// TestDiscoveredTeamIsTakenDownWhenTheFeedReads404 is (c). The re-probe is
// set to an hour so only the feed can notice: the backend drops the stream
// (as a deploy or an idle proxy would), the feed reconnects into a 404, and
// the board comes down. The freed slot is then usable by another team.
func TestDiscoveredTeamIsTakenDownWhenTheFeedReads404(t *testing.T) {
	waitUntil(t, "no live feed goroutines from earlier tests", func() bool {
		return goroutinesRunning(liveStreamLoop) == 0 && goroutinesRunning(recheckLoop) == 0
	})
	x := newHarness(t, Discovery{MaxTeams: 1, RecheckInterval: time.Hour})
	ts := serve(t, x.h)
	const slug = "going-private"
	x.backend.setPublic(slug, "Going Private")

	if rec := x.get("/t/" + slug); rec.Code != http.StatusOK {
		t.Fatalf("discover: %d", rec.Code)
	}
	tm, _ := x.srv.Lookup(slug)
	ended := openStream(t, ts.URL, slug)
	waitUntil(t, "the viewer to subscribe", func() bool { return tm.Hub.Subscribers() == 1 })
	if got := goroutinesRunning(liveStreamLoop); got != 1 {
		t.Fatalf("live feed goroutines = %d, want 1", got)
	}
	if got := goroutinesRunning(recheckLoop); got != 1 {
		t.Fatalf("re-probe goroutines = %d, want 1", got)
	}
	if x.discoveredSlots() != 1 {
		t.Fatal("setup: slot not taken")
	}
	probes := x.probes.Load()

	x.backend.makePrivate(slug)
	x.stub.CloseClientConnections()

	assertTakenDown(t, x, ts, tm, ended)
	if x.probes.Load() != probes {
		t.Fatal("the takedown came from a probe; this test is about the feed")
	}

	// The slot is free: with MaxTeams 1 another team can be discovered.
	x.backend.setPublic("next-team", "Next")
	if rec := x.get("/t/next-team"); rec.Code != http.StatusOK {
		t.Fatalf("discovering into the freed slot: %d", rec.Code)
	}
}

// TestRecheckTakesDownAHealthyBoardMadePrivate is (d). The backend keeps the
// feed's stream open after the flip — the real one authorizes a stream only
// when it is opened — so only the periodic re-probe can notice, and it must
// within about one interval.
func TestRecheckTakesDownAHealthyBoardMadePrivate(t *testing.T) {
	waitUntil(t, "no live feed goroutines from earlier tests", func() bool {
		return goroutinesRunning(liveStreamLoop) == 0 && goroutinesRunning(recheckLoop) == 0
	})
	const interval = 150 * time.Millisecond
	x := newHarness(t, Discovery{RecheckInterval: interval})
	ts := serve(t, x.h)
	const slug = "quietly-private"
	x.backend.setPublic(slug, "Quietly Private")

	if rec := x.get("/t/" + slug); rec.Code != http.StatusOK {
		t.Fatalf("discover: %d", rec.Code)
	}
	tm, _ := x.srv.Lookup(slug)
	ended := openStream(t, ts.URL, slug)
	waitUntil(t, "the feed's stream to open", func() bool { return x.backend.streamsFor(slug) == 1 })

	// While public, re-probes keep the board up.
	time.Sleep(3 * interval)
	if _, ok := x.srv.Lookup(slug); !ok {
		t.Fatal("a public team was taken down")
	}
	if x.probes.Load() < 2 {
		t.Fatalf("probes = %d; the re-probe is not running", x.probes.Load())
	}

	flipped := time.Now()
	x.backend.makePrivate(slug)
	select {
	case <-ended:
	case <-time.After(10 * interval):
		t.Fatalf("still streaming %v after the flip (interval %v)", time.Since(flipped), interval)
	}
	if took := time.Since(flipped); took > interval+100*time.Millisecond {
		t.Errorf("takedown took %v, want within about one interval (%v)", took, interval)
	}
	if got := x.backend.streamsFor(slug); got != 1 {
		t.Fatalf("the feed opened %d streams; it must have stayed healthy for this test to mean anything", got)
	}
	assertTakenDown(t, x, ts, tm, ended)
}

// TestDemoAndPreRegisteredTeamsAreNeverTakenDown is (e). With a fast re-probe
// and a backend that 404s everything, a discovered team comes down — proving
// the machinery is running in this test — while the demo and the pre-warmed
// live team stay, and unregister refuses them outright.
func TestDemoAndPreRegisteredTeamsAreNeverTakenDown(t *testing.T) {
	x := newHarness(t, Discovery{RecheckInterval: 20 * time.Millisecond})
	ts := serve(t, x.h)

	demo, err := x.srv.AddDemoTeam(x.ctx, "demo", "Orbital Freight", "", idleFeed{})
	if err != nil {
		t.Fatal(err)
	}
	x.backend.setPublic("pre-warmed", "Pre Warmed")
	pf := &feed.Live{BaseURL: x.stub.URL, Slug: "pre-warmed", Client: x.stub.Client(), Logger: quiet(), Backoff: 5 * time.Millisecond}
	pre, err := x.srv.AddLiveTeam(x.ctx, "pre-warmed", pf)
	if err != nil {
		t.Fatal(err)
	}
	x.backend.setPublic("found-team", "Found")
	if rec := x.get("/t/found-team"); rec.Code != http.StatusOK {
		t.Fatalf("discover: %d", rec.Code)
	}
	demoEnded := openStream(t, ts.URL, "demo")
	preEnded := openStream(t, ts.URL, "pre-warmed")

	// Everything is a 404 now, and every open backend connection drops, so
	// the pre-warmed feed reconnects into 404s every 5ms.
	x.backend.makePrivate("pre-warmed")
	x.backend.makePrivate("found-team")
	x.stub.CloseClientConnections()

	waitUntil(t, "the discovered team to come down", func() bool {
		_, ok := x.srv.Lookup("found-team")
		return !ok
	})
	time.Sleep(300 * time.Millisecond) // ~15 re-probe intervals, ~60 feed 404s

	for _, tm := range []*Team{demo, pre} {
		if got, ok := x.srv.Lookup(tm.Slug); !ok || got != tm {
			t.Fatalf("%s was unregistered", tm.Slug)
		}
		if tm.Hub.Closed() {
			t.Fatalf("%s: hub closed", tm.Slug)
		}
		if x.srv.disc.unregister(x.srv, tm, "test") {
			t.Fatalf("unregister accepted %s", tm.Slug)
		}
		if _, ok := x.srv.Lookup(tm.Slug); !ok {
			t.Fatalf("%s was unregistered by a refused call", tm.Slug)
		}
		if rec := x.get("/t/" + tm.Slug); rec.Code != http.StatusOK {
			t.Fatalf("GET /t/%s: %d", tm.Slug, rec.Code)
		}
	}
	for name, ended := range map[string]<-chan struct{}{"demo": demoEnded, "pre-warmed": preEnded} {
		select {
		case <-ended:
			t.Fatalf("%s: its viewer's stream was closed", name)
		default:
		}
	}
	select {
	case <-demo.Hub.Done():
		t.Fatal("the demo's feed stopped")
	default:
	}
	if got := x.backend.hitsFor("demo"); got != 0 {
		t.Fatalf("the backend was asked about the demo slug %d times", got)
	}
}
