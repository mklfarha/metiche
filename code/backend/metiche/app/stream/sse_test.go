package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/authz"
	"github.com/mklfarha/metiche/backend/app/stream/publish"
)

// sseEvent is one parsed frame off the wire.
type sseEvent struct {
	id    int64
	name  string
	frame Frame
}

// readSSE parses frames off a live response body until it has n of them or the
// deadline passes. Comment lines (the keepalive) are counted separately, and
// both counts are reported even when the deadline wins — an idle stream never
// reaches n and the keepalive count is the whole point of watching it.
func readSSE(t *testing.T, body io.Reader, n int, within time.Duration) ([]sseEvent, int) {
	t.Helper()

	var (
		mu        sync.Mutex
		events    []sseEvent
		keepalive int
	)
	done := make(chan struct{})

	go func() {
		defer close(done)
		sc := bufio.NewScanner(body)
		var (
			cur    sseEvent
			haveID bool
		)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, ":"):
				mu.Lock()
				keepalive++
				mu.Unlock()
			case strings.HasPrefix(line, "id: "):
				v, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
				if err != nil {
					return
				}
				cur.id = v
				haveID = true
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.frame); err != nil {
					return
				}
			case line == "":
				if haveID {
					mu.Lock()
					events = append(events, cur)
					enough := len(events) >= n
					mu.Unlock()
					cur, haveID = sseEvent{}, false
					if enough {
						return
					}
				}
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(within):
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]sseEvent(nil), events...), keepalive
}

// allowPublic is the authorizer these tests run under: it says "this team is
// public", which is the decision app/authz reaches for a real team whose
// visibility column says public.
//
// Everything in this file is about the STREAM — ordering, resume, keepalive,
// teardown — and none of it should be re-testing the gate. The gate has its
// own tests: the decision itself in app/authz, and the database-backed
// end-to-end cases (private team, no token / wrong account / member) in
// app/webapi's mysql test, which mounts both halves on one router. What this
// file does own is TestStreamRefusesBeforeTheFirstByte below.
var allowPublic = authz.AuthorizerFunc(func(_ context.Context, teamRef string, _ authz.Credential) (authz.Team, error) {
	if teamRef == "" {
		return authz.Team{}, authz.ErrDenied
	}
	return authz.Team{Slug: teamRef, Public: true}, nil
})

// denyAll refuses everything, the way the real guard refuses a private team to
// a stranger.
var denyAll = authz.AuthorizerFunc(func(context.Context, string, authz.Credential) (authz.Team, error) {
	return authz.Team{}, authz.ErrDenied
})

// newStreamServer stands up the real chi route and the real handler over a
// faked event source.
func newStreamServer(t *testing.T, src Source, team uuid.UUID) (*httptest.Server, *Hub) {
	t.Helper()
	return newStreamServerKeepalive(t, src, team, keepaliveInterval)
}

func newStreamServerKeepalive(t *testing.T, src Source, team uuid.UUID, keepalive time.Duration) (*httptest.Server, *Hub) {
	t.Helper()
	return newStreamServerAuth(t, src, team, keepalive, allowPublic)
}

func newStreamServerAuth(t *testing.T, src Source, team uuid.UUID, keepalive time.Duration, a authz.Authorizer) (*httptest.Server, *Hub) {
	t.Helper()
	return newStreamServerReauth(t, src, team, keepalive, reauthInterval, a)
}

func newStreamServerReauth(t *testing.T, src Source, team uuid.UUID, keepalive, reauth time.Duration, a authz.Authorizer) (*httptest.Server, *Hub) {
	t.Helper()

	hub := NewHub(src, nil)
	lookup := func(_ context.Context, slug string) (TeamRef, error) {
		if slug != "demo" {
			return TeamRef{}, ErrTeamNotFound
		}
		return TeamRef{UUID: team, Slug: "demo"}, nil
	}

	r := chi.NewRouter()
	srvHandler := NewServer(hub, lookup, a, nil)
	srvHandler.keepalive = keepalive
	srvHandler.SetReauthInterval(reauth)
	srvHandler.RegisterOn(r)

	srv := httptest.NewServer(r)
	// CloseClientConnections before Close: Close waits for outstanding
	// requests, and an SSE handler is outstanding until its client hangs up —
	// so a test that fails before closing its body would hang the suite here
	// rather than report the failure.
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		hub.Close()
	})
	return srv, hub
}

// TestReconnectByCursorIsExact is the contract the board depends on.
//
// Publish a known sequence of events, connect with ?after=K, and receive
// precisely the events after K — once each, in order, with no gap and no
// duplicate. Then disconnect, let more events land while nobody is attached,
// and reconnect with the last sequence seen: the second connection must
// deliver exactly what was missed and nothing that was already delivered.
func TestReconnectByCursorIsExact(t *testing.T) {
	team := testTeam(t)
	src := &fakeSource{}
	src.append(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)

	srv, hub := newStreamServer(t, src, team)

	// --- first connection: resume from 4 -------------------------------
	resp, err := http.Get(srv.URL + "/v1/teams/demo/stream?after=4")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if nb := resp.Header.Get("X-Accel-Buffering"); nb != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", nb)
	}

	events, _ := readSSE(t, resp.Body, 6, 5*time.Second)
	wantFirst := []int64{5, 6, 7, 8, 9, 10}
	assertSequences(t, events, wantFirst, "first connection")
	for _, e := range events {
		if e.name != "intent_declared" {
			t.Fatalf("frame %d has event name %q, want the event kind", e.id, e.name)
		}
		if e.id != e.frame.Sequence {
			t.Fatalf("SSE id %d does not match the frame's sequence %d", e.id, e.frame.Sequence)
		}
	}

	// --- disconnect; events keep landing while nobody is attached ------
	last := events[len(events)-1].id
	_ = resp.Body.Close()

	// The subscription and its tailing goroutine must go away with the
	// connection, not linger.
	waitForNoWatchers(t, hub, team)

	src.append(11, 12, 13)
	hub.Publish(publish.Event{TeamUUID: team, Sequence: 13})

	// --- reconnect at the last sequence seen ---------------------------
	resp2, err := http.Get(srv.URL + "/v1/teams/demo/stream?after=" + strconv.FormatInt(last, 10))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()

	events2, _ := readSSE(t, resp2.Body, 3, 5*time.Second)
	assertSequences(t, events2, []int64{11, 12, 13}, "after reconnect")
}

// TestReconnectViaLastEventIDHeader covers the browser's own automatic
// reconnect: EventSource resends the last id it saw as Last-Event-ID without
// being asked, and honouring it means a dropped socket recovers exactly even
// for a client with no reconnect logic of its own.
func TestReconnectViaLastEventIDHeader(t *testing.T) {
	team := testTeam(t)
	src := &fakeSource{}
	src.append(1, 2, 3, 4, 5)
	srv, _ := newStreamServer(t, src, team)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/teams/demo/stream", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Last-Event-ID", "3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	events, _ := readSSE(t, resp.Body, 2, 5*time.Second)
	assertSequences(t, events, []int64{4, 5}, "Last-Event-ID resume")
}

// TestLiveEventsFollowTheReplayWithoutAGap is the subscribe-before-replay
// property. An event committed WHILE the replay is running belongs to neither
// the replay query nor a subscription taken afterwards — which is the hole
// that ordering exists to close. Here the client resumes at 0, the backlog
// replays, and a new event lands immediately after; it must arrive exactly
// once, on the same connection.
func TestLiveEventsFollowTheReplayWithoutAGap(t *testing.T) {
	team := testTeam(t)
	src := &fakeSource{}
	src.append(1, 2, 3)
	srv, hub := newStreamServer(t, src, team)

	resp, err := http.Get(srv.URL + "/v1/teams/demo/stream?after=0")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	go func() {
		// Land a new event while the connection is settling.
		time.Sleep(20 * time.Millisecond)
		src.append(4)
		hub.Publish(publish.Event{TeamUUID: team, Sequence: 4})
	}()

	events, _ := readSSE(t, resp.Body, 4, 5*time.Second)
	assertSequences(t, events, []int64{1, 2, 3, 4}, "replay then live")
}

// TestKeepaliveAndDisconnect checks the two operational details: a comment
// frame keeps an idle stream alive, and hanging up releases the subscription
// rather than leaking it.
//
// The keepalive interval is shortened here; in production it is 20s.
func TestKeepaliveAndDisconnect(t *testing.T) {
	team := testTeam(t)
	src := &fakeSource{}
	srv, hub := newStreamServerKeepalive(t, src, team, 20*time.Millisecond)

	resp, err := http.Get(srv.URL + "/v1/teams/demo/stream")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Wait until the hub has actually registered the subscriber.
	waitFor(t, time.Second, func() bool { return watcherCount(hub, team) == 1 })

	// No events will ever arrive, so readSSE returns on its deadline; what it
	// reports is how many comment frames were written meanwhile.
	_, keepalives := readSSE(t, resp.Body, 1, 300*time.Millisecond)
	if keepalives == 0 {
		t.Fatal("an idle stream wrote no keepalive comment")
	}

	_ = resp.Body.Close()
	waitForNoWatchers(t, hub, team)
}

func TestUnknownTeamIs404AndBadCursorIs400(t *testing.T) {
	team := testTeam(t)
	srv, _ := newStreamServer(t, &fakeSource{}, team)

	resp, err := http.Get(srv.URL + "/v1/teams/nope/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown team: status = %d, want 404", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/v1/teams/demo/stream?after=banana")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cursor: status = %d, want 400", resp2.StatusCode)
	}
}

// TestStreamRouteCoexistsWithTheGeneratedCRUDMount reproduces how this package
// is actually mounted: the generated router owns /v1 as a chi mount, and these
// routes are registered on the ROOT router afterwards. chi panics on some
// combinations of mount and route, and it does it while the router is being
// built — so the failure mode is a container that compiles, ships and then
// crash-loops. This asserts the shape is legal and that the stream route wins.
func TestStreamRouteCoexistsWithTheGeneratedCRUDMount(t *testing.T) {
	r := chi.NewRouter()
	// Stand-in for the generated tree.
	r.Route("/v1", func(v1 chi.Router) {
		v1.Route("/teams", func(teams chi.Router) {
			teams.Get("/", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, "generated-list")
			})
			teams.Route("/{id}", func(one chi.Router) {
				one.Get("/", func(w http.ResponseWriter, _ *http.Request) {
					_, _ = fmt.Fprint(w, "generated-get")
				})
			})
		})
	})

	team := testTeam(t)
	hub := NewHub(&fakeSource{}, nil)
	defer hub.Close()
	lookup := func(context.Context, string) (TeamRef, error) { return TeamRef{UUID: team}, nil }
	NewServer(hub, lookup, allowPublic, nil).RegisterOn(r)

	srv := httptest.NewServer(r)
	defer srv.Close()

	// The generated collection route still works.
	resp, err := http.Get(srv.URL + "/v1/teams/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "generated-list" {
		t.Fatalf("generated list route was shadowed: %q", body)
	}

	// And the stream route resolves.
	resp2, err := http.Get(srv.URL + "/v1/teams/demo/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	ct := resp2.Header.Get("Content-Type")
	_ = resp2.Body.Close()
	if ct != "text/event-stream" {
		t.Fatalf("stream route did not resolve under the mount: Content-Type = %q", ct)
	}
}

func assertSequences(t *testing.T, events []sseEvent, want []int64, label string) {
	t.Helper()
	got := make([]int64, 0, len(events))
	for _, e := range events {
		got = append(got, e.id)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got sequences %v, want exactly %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got sequences %v, want exactly %v", label, got, want)
		}
	}
}

func watcherCount(h *Hub, team uuid.UUID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	w, ok := h.teams[team]
	if !ok {
		return 0
	}
	return len(w.chans)
}

func waitForNoWatchers(t *testing.T, h *Hub, team uuid.UUID) {
	t.Helper()
	waitFor(t, 3*time.Second, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		_, ok := h.teams[team]
		return !ok
	})
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}

// TestStreamRefusesBeforeTheFirstByte is the reason the gate is a wrapper
// around the handler rather than a check inside it.
//
// An SSE handler writes 200 and flushes its headers almost immediately, and
// after that the status code is spent: a refusal decided any later can only
// be expressed by hanging up. To the client that is a healthy stream that
// died, which is indistinguishable from a network blip and which every
// EventSource on earth retries forever — a denied board would look like a
// flaky one, and would keep knocking.
//
// So this asserts the client sees an HTTP STATUS: 404, not text/event-stream,
// a complete finite body, and a response that is OVER — io.ReadAll returns,
// which it never would on an open stream.
func TestStreamRefusesBeforeTheFirstByte(t *testing.T) {
	team := testTeam(t)
	src := &fakeSource{}
	src.append(1, 2, 3)
	srv, hub := newStreamServerAuth(t, src, team, keepaliveInterval, denyAll)

	resp, err := http.Get(srv.URL + "/v1/teams/demo/stream?after=0")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("denied stream: status = %d, want 404", resp.StatusCode)
	}
	// 404, never 403: a 403 would confirm the team exists.
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("the stream answered 403, which confirms the team exists")
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("the stream opened before it was authorized: Content-Type = %q", ct)
	}

	// The response completes. On an open-then-dead stream this would block
	// until the test binary timed out.
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(resp.Body)
		done <- string(b)
	}()
	select {
	case body := <-done:
		if strings.Contains(body, "data: ") || strings.Contains(body, "id: ") {
			t.Fatalf("a refused stream still wrote frames: %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a refused stream left the connection open instead of returning a status")
	}

	// And nothing was subscribed: a refused request must not reach the hub,
	// or a stranger could keep a team's tailing goroutine alive by knocking.
	if n := watcherCount(hub, team); n != 0 {
		t.Fatalf("a refused request subscribed to the hub anyway: %d watchers", n)
	}
}

// TestStreamWithANilAuthorizerRefusesEverything pins the fail-closed default.
// A nil authorizer is a wiring mistake, and the safe reading of a wiring
// mistake on a public route is "nobody gets in", not "everybody does".
func TestStreamWithANilAuthorizerRefusesEverything(t *testing.T) {
	team := testTeam(t)
	srv, _ := newStreamServerAuth(t, &fakeSource{}, team, keepaliveInterval, nil)

	resp, err := http.Get(srv.URL + "/v1/teams/demo/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	code := resp.StatusCode
	_ = resp.Body.Close()
	if code != http.StatusNotFound {
		t.Fatalf("nil authorizer: status = %d, want 404", code)
	}
}

// ─────────────────────────────────────────────
// Re-authorization of an OPEN stream (docs/BOARD_LOGIN.md §4.4)
// ─────────────────────────────────────────────

// testReauth is the re-authorization interval these tests run with; 60s in
// production.
const testReauth = 50 * time.Millisecond

// switchable is an authorizer whose answer a test can change while a stream is
// open, and which records every credential it was asked about.
type switchable struct {
	mode  atomic.Int32 // 0 grant, 1 refuse, 2 outage
	calls atomic.Int32

	mu    sync.Mutex
	creds []authz.Credential
}

func (s *switchable) Authorize(_ context.Context, teamRef string, cred authz.Credential) (authz.Team, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.creds = append(s.creds, cred)
	s.mu.Unlock()
	switch s.mode.Load() {
	case 1:
		return authz.Team{}, authz.ErrDenied
	case 2:
		return authz.Team{}, fmt.Errorf("the database fell over")
	}
	return authz.Team{Slug: teamRef}, nil
}

// endWatch reads a response body to EOF on ONE goroutine and reports when it
// got there. A single reader per body matters: two goroutines reading the same
// body corrupt the client connection. On an open stream it never ends.
type endWatch struct{ done chan struct{} }

func watchEnd(body io.Reader) *endWatch {
	e := &endWatch{done: make(chan struct{})}
	go func() {
		_, _ = io.Copy(io.Discard, body)
		close(e.done)
	}()
	return e
}

// within reports whether the stream has ended, waiting at most d.
func (e *endWatch) within(d time.Duration) bool {
	select {
	case <-e.done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestAnOpenStreamEndsWhenItsViewerLosesAccess is the gap §4.4 closes: the
// gate decides once, before the first byte, and a stream lives for hours. A
// member removed from a private team (or an anonymous viewer of a team just
// made private) must stop receiving events within the re-auth interval, not
// whenever they happen to reconnect.
func TestAnOpenStreamEndsWhenItsViewerLosesAccess(t *testing.T) {
	team := testTeam(t)
	src := &fakeSource{}
	src.append(1, 2)
	auth := &switchable{}
	srv, hub := newStreamServerReauth(t, src, team, keepaliveInterval, testReauth, auth)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/teams/demo/stream?after=0", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set(authz.HeaderBrowserSession, "session-under-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	events, _ := readSSE(t, resp.Body, 2, 5*time.Second)
	assertSequences(t, events, []int64{1, 2}, "before revocation")
	end := watchEnd(resp.Body)

	// Still allowed: several re-auth ticks pass and the stream stays open.
	if end.within(6 * testReauth) {
		t.Fatal("a stream whose viewer still has access was ended by re-authorization")
	}
	if n := auth.calls.Load(); n < 3 {
		t.Fatalf("the open stream re-authorized %d times in %v; want it on every %v tick", n, 6*testReauth, testReauth)
	}

	// Access is lost. The stream must end within a few intervals.
	auth.mode.Store(1)
	if !end.within(20 * testReauth) {
		t.Fatalf("a stream whose viewer lost access was still open after %v (re-auth every %v)", 20*testReauth, testReauth)
	}
	waitForNoWatchers(t, hub, team)

	// Every decision, the gate's and every tick's, was made with the credential
	// the request came with.
	auth.mu.Lock()
	creds := append([]authz.Credential(nil), auth.creds...)
	auth.mu.Unlock()
	for i, c := range creds {
		if c != (authz.Credential{BrowserSession: "session-under-test"}) {
			t.Fatalf("decision %d was made with a different credential than the request carried", i)
		}
	}

	// And the reconnect meets the gate: a status, not a stream.
	resp2, err := http.DefaultClient.Do(req.Clone(context.Background()))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("reconnect after revocation: status = %d, want 404", resp2.StatusCode)
	}
}

// TestAnOutageDuringReauthEndsTheStream: an open stream that can no longer be
// vouched for fails closed. The reconnect is exact (?after=), so ending it
// costs a client nothing but a round trip.
func TestAnOutageDuringReauthEndsTheStream(t *testing.T) {
	team := testTeam(t)
	auth := &switchable{}
	srv, hub := newStreamServerReauth(t, &fakeSource{}, team, keepaliveInterval, testReauth, auth)

	resp, err := http.Get(srv.URL + "/v1/teams/demo/stream")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	waitFor(t, time.Second, func() bool { return watcherCount(hub, team) == 1 })
	end := watchEnd(resp.Body)

	auth.mode.Store(2)
	if !end.within(20 * testReauth) {
		t.Fatal("a stream stayed open although its re-authorization could not be decided")
	}

	// And at the gate an outage is 503, never the 404 of a refusal.
	resp2, err := http.Get(srv.URL + "/v1/teams/demo/stream")
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("gate during an outage: status = %d, want 503", resp2.StatusCode)
	}
}

// TestAStreamEndsWhenItsSlugNowNamesAnotherTeam: a team deleted and re-created
// under the same slug is not the stream the client was admitted to.
func TestAStreamEndsWhenItsSlugNowNamesAnotherTeam(t *testing.T) {
	team := testTeam(t)
	var other atomic.Bool
	auth := authz.AuthorizerFunc(func(_ context.Context, teamRef string, _ authz.Credential) (authz.Team, error) {
		if other.Load() {
			return authz.Team{UUID: uuid.Must(uuid.NewV4()), Slug: teamRef, Public: true}, nil
		}
		return authz.Team{UUID: team, Slug: teamRef, Public: true}, nil
	})
	srv, hub := newStreamServerReauth(t, &fakeSource{}, team, keepaliveInterval, testReauth, auth)

	resp, err := http.Get(srv.URL + "/v1/teams/demo/stream")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	waitFor(t, time.Second, func() bool { return watcherCount(hub, team) == 1 })
	end := watchEnd(resp.Body)
	if end.within(4 * testReauth) {
		t.Fatal("a stream for the same team was ended")
	}
	other.Store(true)
	if !end.within(20 * testReauth) {
		t.Fatal("a stream stayed open after its slug came to name a different team")
	}
}

// TestTheDefaultReauthIntervalIsAMinute pins decision §9 / §4.4 and that a
// non-positive override restores it.
func TestTheDefaultReauthIntervalIsAMinute(t *testing.T) {
	s := NewServer(NewHub(&fakeSource{}, nil), nil, allowPublic, nil)
	defer s.hub.Close()
	if s.reauth != time.Minute || reauthInterval != time.Minute {
		t.Fatalf("reauth = %v, want 1m", s.reauth)
	}
	s.SetReauthInterval(testReauth)
	if s.reauth != testReauth {
		t.Fatalf("SetReauthInterval did not take: %v", s.reauth)
	}
	s.SetReauthInterval(0)
	if s.reauth != reauthInterval {
		t.Fatalf("SetReauthInterval(0) = %v, want the default", s.reauth)
	}
}
