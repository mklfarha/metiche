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
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid"

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

// newStreamServer stands up the real chi route and the real handler over a
// faked event source.
func newStreamServer(t *testing.T, src Source, team uuid.UUID) (*httptest.Server, *Hub) {
	t.Helper()
	return newStreamServerKeepalive(t, src, team, keepaliveInterval)
}

func newStreamServerKeepalive(t *testing.T, src Source, team uuid.UUID, keepalive time.Duration) (*httptest.Server, *Hub) {
	t.Helper()

	hub := NewHub(src, nil)
	lookup := func(_ context.Context, slug string) (TeamRef, error) {
		if slug != "demo" {
			return TeamRef{}, ErrTeamNotFound
		}
		return TeamRef{UUID: team, Slug: "demo"}, nil
	}

	r := chi.NewRouter()
	srvHandler := NewServer(hub, lookup, nil)
	srvHandler.keepalive = keepalive
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
	NewServer(hub, lookup, nil).RegisterOn(r)

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
