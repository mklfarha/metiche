package web

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/hub"
)

// The board used to have three controls — resolve a conflict, nudge a session,
// change the pace — that anybody could POST to. Each applied a made-up event
// to the board's store and broadcast it to every viewer. These tests pin that
// the paths are now the unknown-slug 404 for every kind of team, and that
// nothing reaches a store or a subscriber.

// control is one old control request, with the form it used to send.
type control struct {
	path string // under /t/{slug}
	form url.Values
}

var controls = []control{
	{"/conflicts/C-1/resolve", url.Values{"status": {"resolved"}}},
	{"/conflicts/C-1/resolve", url.Values{"status": {"dismissed"}}},
	{"/sessions/S-1/nudge", url.Values{"nudge": {"ask"}}},
	{"/cadence", url.Values{"cadence": {"hackathon"}}},
}

func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true") // what htmx sends; it must not matter
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// unknownSlugPage is what this server says for a board that does not exist:
// a GET for slug against a backend that answers 404 for it.
func unknownSlugPage(t *testing.T, slug string) string {
	t.Helper()
	y := newHarness(t, Discovery{})
	rec := y.get("/t/" + slug)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reference unknown-slug page for %s: status %d", slug, rec.Code)
	}
	return rec.Body.String()
}

type storeMark struct {
	seq, rev int64
	events   int
}

func markOf(tm *Team) storeMark {
	s := tm.Hub.Snapshot()
	return storeMark{seq: tm.Hub.Store().Sequence(), rev: s.Team.BoardRevision, events: len(s.Events)}
}

// assertSilent fails if sub receives anything within d.
func assertSilent(t *testing.T, who string, sub *hub.Subscriber, d time.Duration) {
	t.Helper()
	select {
	case f, ok := <-sub.C:
		if !ok {
			t.Fatalf("%s: subscription closed", who)
		}
		t.Fatalf("%s: a %q frame (id %d) was broadcast", who, f.Name, f.ID)
	case <-time.After(d):
	}
}

// TestControlsOnLiveTeamsAre404AndInjectNothing is (a): for a discovered and a
// pre-registered live team, every old control path is byte-for-byte the
// unknown-slug page, the store does not move, and a connected subscriber
// receives no frame.
func TestControlsOnLiveTeamsAre404AndInjectNothing(t *testing.T) {
	x := newHarness(t, Discovery{})
	const discovered, prewarmed = "acme-live", "beta-prewarmed"
	x.backend.setPublic(discovered, "Acme")
	x.backend.setPublic(prewarmed, "Beta")

	if rec := x.get("/t/" + discovered); rec.Code != http.StatusOK {
		t.Fatalf("discovering: %d", rec.Code)
	}
	f := &feed.Live{BaseURL: x.stub.URL, Slug: prewarmed, Client: x.stub.Client(), Logger: quiet()}
	if _, err := x.srv.AddLiveTeam(x.ctx, prewarmed, f); err != nil {
		t.Fatal(err)
	}

	for _, slug := range []string{discovered, prewarmed} {
		tm, ok := x.srv.Lookup(slug)
		if !ok || tm.Demo {
			t.Fatalf("%s: not registered as a live team", slug)
		}
		want := unknownSlugPage(t, slug)
		sub := tm.Hub.Subscribe()
		before := markOf(tm)
		probes := x.probes.Load()

		for _, c := range controls {
			path := "/t/" + slug + c.path
			rec := postForm(x.h, path, c.form)
			if rec.Code != http.StatusNotFound {
				t.Errorf("POST %s: status %d, want 404", path, rec.Code)
			}
			if got := rec.Body.String(); got != want {
				t.Errorf("POST %s: body is not the unknown-slug page\n got: %.200q\nwant: %.200q", path, got, want)
			}
			if rec.Header().Get("HX-Refresh") != "" {
				t.Errorf("POST %s: still tells htmx to refresh", path)
			}
		}

		if after := markOf(tm); after != before {
			t.Errorf("%s: store moved from %+v to %+v", slug, before, after)
		}
		if x.probes.Load() != probes {
			t.Errorf("%s: a control request reached the backend", slug)
		}
		assertSilent(t, slug, sub, 200*time.Millisecond)
		tm.Hub.Unsubscribe(sub)
	}
}

// TestDemoControlsReachNoOtherViewer is (b). The demo is one shared hub, so
// its controls are gone too: visitor A's POSTs are 404s, the store does not
// move, and visitor B — a real SSE connection to the same demo — receives
// nothing after its replay. The demo board and the landing page still work.
func TestDemoControlsReachNoOtherViewer(t *testing.T) {
	x := newHarness(t, Discovery{})
	demo, err := x.srv.AddDemoTeam(x.ctx, "demo", "Orbital Freight", "", idleFeed{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(x.h)
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close() })

	// Visitor B opens the stream and reads the replay (retry + board frame).
	resp, err := http.Get(ts.URL + "/t/demo/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	lines := make(chan string, 1024)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	sawBoard := false
	for !sawBoard {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream ended during replay")
			}
			sawBoard = l == "event: board"
		case <-time.After(5 * time.Second):
			t.Fatal("no replay")
		}
	}
	waitUntil(t, "visitor B subscribed", func() bool { return demo.Hub.Subscribers() == 1 })
	// A second, hub-level viewer as well, asserting on frames directly.
	sub := demo.Hub.Subscribe()
	defer demo.Hub.Unsubscribe(sub)
	before := markOf(demo)

	// Visitor A pokes every control.
	for _, c := range controls {
		rec := postForm(x.h, "/t/demo"+c.path, c.form)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST /t/demo%s: status %d, want 404", c.path, rec.Code)
		}
	}

	if after := markOf(demo); after != before {
		t.Errorf("demo store moved from %+v to %+v", before, after)
	}
	assertSilent(t, "hub viewer", sub, 200*time.Millisecond)
	// Drain what is left of the replay message, then B must hear nothing
	// but (at most) a keepalive comment.
	deadline := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("visitor B's stream ended")
			}
			if strings.HasPrefix(l, "event:") {
				t.Fatalf("visitor B received %q after visitor A's POSTs", l)
			}
		case <-deadline:
			done = true
		}
	}

	// The demo is still a working demo.
	rec := x.get("/t/demo")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-demo="recording"`) {
		t.Fatalf("demo board: %d, labelled=%v", rec.Code, strings.Contains(rec.Body.String(), `data-demo="recording"`))
	}
	if body := x.get("/").Body.String(); !strings.Contains(body, `href="/t/demo"`) {
		t.Fatal("landing no longer links the demo")
	}
}

// TestBoardHTMLHasNoControls is the render check: every page of a live board
// — with a live session and an open conflict, which is exactly when the
// controls used to render — and of a demo board carries no form, no hx-post
// and no control path.
func TestBoardHTMLHasNoControls(t *testing.T) {
	x := newHarness(t, Discovery{})
	const slug = "acme-live"
	x.backend.setPublic(slug, "Acme")
	now := time.Now().UTC().Format(time.RFC3339)
	x.backend.sessions = []any{map[string]any{
		"key": "S-17", "project_key": "web", "member_key": "M-1", "member_name": "Mara",
		"agent_label": "api", "status": "live", "status_line": "writing the handler",
		"branch": "feat/api", "started_at": now, "last_heartbeat_at": now,
		"intents": []any{}, "claims": []any{},
	}}
	x.backend.conflicts = []any{map[string]any{
		"key": "C-9", "kind": "claim_overlap", "severity": "high", "status": "open",
		"suggested_action": "talk to each other", "first_detected_at": now,
		"participants": []any{map[string]any{"session_key": "S-17", "role": "holder", "subject_kind": "claim"}},
	}}
	if _, err := x.srv.AddDemoTeam(x.ctx, "demo", "Orbital Freight", "", idleFeed{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(x.h)
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close() })

	get := func(path string) string {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
		var b strings.Builder
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			b.WriteString(sc.Text())
			b.WriteByte('\n')
		}
		return b.String()
	}

	// The fixture is not vacuous: the session is live on the board and the
	// conflict is open on the conflicts page.
	if body := get("/t/" + slug); !strings.Contains(body, "S-17") {
		t.Fatal("setup: the live session is not on the board")
	}
	if body := get("/t/" + slug + "/conflicts"); !strings.Contains(body, "C-9") {
		t.Fatal("setup: the open conflict is not on the conflicts page")
	}

	forbidden := []string{"hx-post", "<form", "<select", "/resolve", "/nudge", "/cadence", ">nudge<", "Mark resolved", "Acknowledge<", "Dismiss as false positive"}
	for _, board := range []string{slug, "demo"} {
		for _, page := range []string{"", "/graph", "/conflicts", "/contracts", "/decisions", "/runs", "/runs/S-17", "/activity"} {
			path := "/t/" + board + page
			if board == "demo" && page == "/runs/S-17" {
				continue // no such session in the demo
			}
			body := get(path)
			for _, bad := range forbidden {
				if strings.Contains(body, bad) {
					t.Errorf("GET %s: contains %q", path, bad)
				}
			}
			if !strings.Contains(body, `class="cadence"`) {
				t.Errorf("GET %s: the pace is no longer stated", path)
			}
		}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
