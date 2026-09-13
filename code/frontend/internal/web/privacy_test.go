package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// The public pages — /, /teams, /join and /healthz — are served to every
// visitor on the public host. Against a real backend they must never name a
// real team: only the demo recordings.

const (
	liveSlug = "acme-secret-squad"
	liveName = "Acme Secret Squad"
	liveCode = "LIVE-CODE-0001"

	discoveredSlug = "beta-hidden-crew"
	discoveredName = "Beta Hidden Crew"
)

// mixed registers a demo, a pre-warmed live team (with a join code forced onto
// it, which production never does, to prove the filter rather than the absence
// of data) and a discovered live team.
func mixed(t *testing.T, liveFirst bool) *harness {
	t.Helper()
	x := newHarness(t, Discovery{})
	x.backend.public[liveSlug] = liveName
	x.backend.public[discoveredSlug] = discoveredName

	addLive := func() {
		f := &feed.Live{BaseURL: x.stub.URL, Slug: liveSlug, Client: x.stub.Client(), Logger: quiet()}
		if _, err := x.srv.addTeam(x.ctx, liveSlug, liveSlug, liveCode, false, false, f); err != nil {
			t.Fatal(err)
		}
	}
	if liveFirst {
		addLive()
	}
	if _, err := x.srv.AddDemoTeam(x.ctx, "demo", "Orbital Freight", "DEMO-CODE-0001", idleFeed{}); err != nil {
		t.Fatal(err)
	}
	if !liveFirst {
		addLive()
	}
	if rec := x.get("/t/" + discoveredSlug); rec.Code != http.StatusOK {
		t.Fatalf("discovering %s: %d", discoveredSlug, rec.Code)
	}
	if len(liveTeams(x.srv)) != 2 {
		t.Fatalf("setup: want 2 live teams, have %d", len(liveTeams(x.srv)))
	}
	return x
}

func assertNoLiveTeam(t *testing.T, path string, rec interface {
	Header() http.Header
}, body string) {
	t.Helper()
	for _, secret := range []string{liveSlug, liveName, liveCode, discoveredSlug, discoveredName} {
		if strings.Contains(body, secret) {
			t.Errorf("GET %s: body contains %q", path, secret)
		}
		if strings.Contains(rec.Header().Get("Location"), secret) {
			t.Errorf("GET %s: redirects to %q", path, secret)
		}
	}
}

// TestLiveTeamNeverAppearsOnPublicPages renders /, /teams, /join and /healthz
// with live and demo teams registered, and asserts no live slug, name or code
// is in any of them.
func TestLiveTeamNeverAppearsOnPublicPages(t *testing.T) {
	x := mixed(t, true)
	for _, p := range []string{"/", "/teams", "/join", "/join?code=", "/join?code=NOPE", "/healthz"} {
		rec := x.get(p)
		assertNoLiveTeam(t, p, rec, rec.Body.String())
	}
	// The demo is still offered, so the pages are not merely empty.
	if body := x.get("/teams").Body.String(); !strings.Contains(body, "/t/demo") {
		t.Errorf("/teams does not list the demo")
	}
	if body := x.get("/teams").Body.String(); !strings.Contains(body, "/t/&lt;your-team-slug&gt;") {
		t.Errorf("/teams does not say how to reach your own board")
	}
}

// TestLandingLinkTargetsDemoSlug: even when a live team was registered first,
// the landing page's board link is the demo.
func TestLandingLinkTargetsDemoSlug(t *testing.T) {
	x := mixed(t, true)
	body := x.get("/").Body.String()
	if !strings.Contains(body, `href="/t/demo"`) {
		t.Fatalf("landing does not link the demo board")
	}
	if !strings.Contains(body, "See a live demo") {
		t.Fatalf("landing has no 'See a live demo' link")
	}
	assertNoLiveTeam(t, "/", x.get("/"), body)

	// With no demo registered the link is omitted rather than pointing at a
	// live team.
	y := newHarness(t, Discovery{})
	y.backend.public[liveSlug] = liveName
	f := &feed.Live{BaseURL: y.stub.URL, Slug: liveSlug, Client: y.stub.Client(), Logger: quiet()}
	if _, err := y.srv.AddLiveTeam(y.ctx, liveSlug, f); err != nil {
		t.Fatal(err)
	}
	body = y.get("/").Body.String()
	if strings.Contains(body, `href="/t/`) || strings.Contains(body, "See a live demo") {
		t.Fatalf("landing links a board with no demo registered")
	}
}

// TestJoinCodeNeverRedirectsToLiveTeam: a live team's code — were the board
// ever to hold one — is treated exactly like a wrong code.
func TestJoinCodeNeverRedirectsToLiveTeam(t *testing.T) {
	x := mixed(t, true)

	rec := x.get("/join?code=" + liveCode)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("live code: %d → %q, want 303 → /", rec.Code, rec.Header().Get("Location"))
	}
	wrong := x.get("/join?code=WRONG-CODE-9999")
	if wrong.Code != rec.Code || wrong.Header().Get("Location") != rec.Header().Get("Location") {
		t.Fatalf("a live code is distinguishable from a wrong one")
	}
	demo := x.get("/join?code=demo-code-0001")
	if demo.Header().Get("Location") != "/t/demo" {
		t.Fatalf("demo code: → %q, want /t/demo", demo.Header().Get("Location"))
	}
}

// TestDemoBoardIsLabelled: every page of a demo board says it is a recording;
// a live board does not.
func TestDemoBoardIsLabelled(t *testing.T) {
	x := mixed(t, false)
	for _, p := range []string{"/t/demo", "/t/demo/graph", "/t/demo/conflicts", "/t/demo/runs"} {
		body := x.get(p).Body.String()
		if !strings.Contains(body, `data-demo="recording"`) || !strings.Contains(body, "DEMO · a recording.") {
			t.Errorf("GET %s: no demo label", p)
		}
		if !strings.Contains(body, "<title>demo · ") {
			t.Errorf("GET %s: title does not say demo", p)
		}
	}
	for _, p := range []string{"/t/" + liveSlug, "/t/" + discoveredSlug} {
		rec := x.get(p)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `data-demo="recording"`) {
			t.Errorf("GET %s: a live board is labelled as a demo", p)
		}
	}
}
