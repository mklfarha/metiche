package view

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

// Test fixtures only: obvious fakes.
const (
	testCSRF = "csrf-TESTONLY-not-a-real-token"
	testName = "Ada Tester"
)

func signedIn() context.Context {
	return WithViewer(context.Background(), Viewer{SignedIn: true, DisplayName: testName, CSRFToken: testCSRF})
}

func render(t *testing.T, ctx context.Context, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func boardPage(ctx context.Context, t *testing.T) string {
	s := twoTerminals()
	return render(t, ctx, Layout(s, TabBoard, "/t/demo/stream?after=0", BoardPage(s)))
}

var (
	scriptTag = regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`)
	onHandler = regexp.MustCompile(`(?i)<[^>]*\son[a-z]+\s*=`)
	postForm  = regexp.MustCompile(`(?s)<form\b[^>]*method="post"[^>]*>.*?</form>`)
)

// assertNoInlineScript: every <script> has a src and an empty body, and no
// element carries an on*= handler. The sign-in page's CSP is
// script-src 'self'; the rest of the board keeps to the same rule.
func assertNoInlineScript(t *testing.T, name, html string) {
	t.Helper()
	for _, m := range scriptTag.FindAllStringSubmatch(html, -1) {
		if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
			t.Errorf("%s: inline script: %q", name, m[0])
		}
	}
	if m := onHandler.FindString(html); m != "" {
		t.Errorf("%s: inline event handler: %q", name, m)
	}
}

func TestIndicatorSignedIn(t *testing.T) {
	html := boardPage(signedIn(), t)
	for _, want := range []string{
		`<meta name="csrf-token" content="` + testCSRF + `">`,
		`hx-headers="{&#34;X-CSRF-Token&#34;:&#34;` + testCSRF + `&#34;}"`,
		`<div class="who" data-viewer="signed-in">`,
		`<a class="who-name" href="/account"`,
		`<b>` + testName + `</b>`,
		`<form class="who-out" method="post" action="/signout"><input type="hidden" name="csrf" value="` + testCSRF + `">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("signed-in board lacks %q", want)
		}
	}
	if strings.Contains(html, `href="/signin"`) {
		t.Errorf("signed-in board offers Sign in")
	}
	assertNoInlineScript(t, "board (signed in)", html)
}

func TestIndicatorAnonymous(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no viewer":             context.Background(),
		"explicitly anonymous":  WithViewer(context.Background(), Viewer{}),
		"not signed in, filled": WithViewer(context.Background(), Viewer{DisplayName: testName, CSRFToken: testCSRF}),
	} {
		html := boardPage(ctx, t)
		if !strings.Contains(html, `<div class="who" data-viewer="anonymous"><a class="who-btn" href="/signin">Sign in</a></div>`) {
			t.Errorf("%s: no Sign in link", name)
		}
		for _, bad := range []string{"csrf", "hx-headers", testName, "/signout", "/account"} {
			if strings.Contains(html, bad) {
				t.Errorf("%s: anonymous board contains %q", name, bad)
			}
		}
		assertNoInlineScript(t, "board (anonymous)", html)
	}
}

// The indicator lives inside the topbar, so the demo banner is still the
// .app grid's second child and nothing is added between them.
func TestIndicatorLeavesDemoBannerRow(t *testing.T) {
	html := boardPage(WithDemo(signedIn()), t)
	if !regexp.MustCompile(`(?s)<div class="app"><header class="topbar">.*?</header><div class="demo-banner"`).MatchString(html) {
		t.Fatalf("demo banner is no longer directly after the topbar")
	}
}

func TestDisplayNameIsEscaped(t *testing.T) {
	ctx := WithViewer(context.Background(), Viewer{SignedIn: true, DisplayName: `<img src=x onerror=alert(1)>`, CSRFToken: `"><x`})
	html := boardPage(ctx, t)
	if strings.Contains(html, "<img src=x") || strings.Contains(html, `"><x`) {
		t.Fatalf("display name or token not escaped")
	}
}

func TestNotFoundHint(t *testing.T) {
	const hint = `<div class="hint nf-signin" data-hint="signin">If this is your team&#39;s board, sign in from a terminal where metiche is installed: <code>metiche open</code>, or ask your assistant to open the metiche board.</div>`
	anon := render(t, context.Background(), NotFound("no-such-team-7q"))
	in := render(t, signedIn(), NotFound("no-such-team-7q"))
	for name, html := range map[string]string{"anonymous": anon, "signed in": in} {
		if !strings.Contains(html, hint) {
			t.Errorf("%s 404 lacks the hint line", name)
		}
		assertNoInlineScript(t, "404 "+name, html)
	}
	if strings.Contains(anon, `class="who"`) {
		t.Errorf("anonymous 404 shows an indicator")
	}
	if !strings.Contains(in, `<div class="nf-who"><div class="who" data-viewer="signed-in">`) {
		t.Errorf("signed-in 404 lacks the indicator")
	}
	// Same inputs, same bytes: nothing else feeds this page.
	if render(t, signedIn(), NotFound("no-such-team-7q")) != in {
		t.Errorf("404 is not deterministic for the same viewer")
	}
}

func TestTeamsPageYourTeams(t *testing.T) {
	p := TeamsParams{
		Demos: []TeamCard{{Slug: "demo", Name: "Demo", Live: 2, Members: 3}},
		Mine: []YourTeam{
			{Slug: "rocket-TESTONLY", Name: "Rocket Test Team", Visibility: "private", Role: "owner"},
			{Slug: "open-TESTONLY", Name: "Open Test Team", Visibility: "public", Role: "member"},
		},
	}
	in := render(t, signedIn(), TeamsPage(p))
	for _, want := range []string{
		`<h2 id="yours-h">Your teams</h2>`,
		`<a class="teamlink" href="/t/rocket-TESTONLY">`,
		`/t/rocket-TESTONLY · private · owner`,
		`<a class="teamlink" href="/t/open-TESTONLY">`,
		`<a class="teamlink" href="/t/demo">`,
		`<b>` + testName + `</b>`,
	} {
		if !strings.Contains(in, want) {
			t.Errorf("signed-in /teams lacks %q", want)
		}
	}
	assertNoInlineScript(t, "/teams signed in", in)

	// Anonymous: Mine is never rendered, and the page is JoinPage's.
	anon := render(t, context.Background(), TeamsPage(p))
	for _, bad := range []string{"TESTONLY", "Your teams", "csrf"} {
		if strings.Contains(anon, bad) {
			t.Errorf("anonymous /teams contains %q", bad)
		}
	}
	if anon != render(t, context.Background(), JoinPage(p.Demos)) {
		t.Errorf("anonymous TeamsPage differs from JoinPage")
	}

	empty := render(t, signedIn(), TeamsPage(TeamsParams{Demos: p.Demos}))
	if !strings.Contains(empty, "not a member of any team yet") {
		t.Errorf("signed-in /teams with no teams says nothing")
	}
}

func accountFixture() AccountParams {
	created := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	seen := created.Add(90 * time.Minute)
	return AccountParams{Sessions: []AccountSession{
		{Key: "BS-TESTONLY-1", UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36",
			IPHint: "203.0.113.0/24", CreatedAt: created, LastSeenAt: &seen, Current: true},
		{Key: "BS-TESTONLY/2", UserAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
			IPHint: "2001:db8:1::/48", CreatedAt: created.Add(-24 * time.Hour)},
	}, Ended: []AccountSession{
		{Key: "BS-TESTENDED1", UserAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0",
			CreatedAt: created.Add(-48 * time.Hour), LastSeenAt: &seen, EndedAt: &seen, EndReason: "revoked_by_agent"},
		{Key: "BS-TESTEXPIRD", UserAgent: "curl/8.7.1", CreatedAt: created.Add(-72 * time.Hour), EndReason: SessionExpiredReason},
	}}
}

func TestSessionEndText(t *testing.T) {
	for reason, want := range map[string]string{
		"signed_out": "signed out", "signed_out_everywhere": "signed out everywhere", "revoked": "revoked",
		"revoked_by_agent": "signed out by an agent", "replaced": "replaced by a newer sign-in",
		SessionExpiredReason: "expired", "": "ended", "something_new": "ended",
	} {
		if got := SessionEndText(reason); got != want {
			t.Errorf("SessionEndText(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestAccountPage(t *testing.T) {
	html := render(t, signedIn(), AccountPage(accountFixture()))
	for _, want := range []string{
		`Signed in as <b data-viewer-name>` + testName + `</b>`,
		`Chrome on macOS`, `Safari on iPhone`, `this browser`,
		`BS-TESTONLY-1 · 203.0.113.0/24`,
		`<time datetime="2026-09-12T10:30:00Z">12 Sep 2026, 10:30 UTC</time>`,
		`not yet`,
		`action="/account/sessions/BS-TESTONLY%2F2/revoke"`,
		`action="/signout"`,
		`action="/account/signout-all"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	// The current browser is ended with Sign out, not a revoke button.
	if strings.Contains(html, "BS-TESTONLY-1/revoke") {
		t.Errorf("current session offers revoke")
	}
	// Ended sessions are read-only, in their own list, with why they ended.
	_, endedList, ok := strings.Cut(html, `<ul class="sessions ended">`)
	if !ok {
		t.Fatalf("account page has no ended list")
	}
	for _, want := range []string{
		`data-session="BS-TESTENDED1" data-end-reason="revoked_by_agent"`, `<span class="badge plain">signed out by an agent</span>`,
		`ended <time datetime="2026-09-12T10:30:00Z">`,
		`data-session="BS-TESTEXPIRD" data-end-reason="expired"`, `<span class="badge plain">expired</span>`,
	} {
		if !strings.Contains(endedList, want) {
			t.Errorf("ended list lacks %q", want)
		}
	}
	if strings.Contains(endedList, "BS-TESTONLY") || strings.Contains(html, "BS-TESTENDED1/revoke") || strings.Contains(html, "BS-TESTEXPIRD/revoke") {
		t.Errorf("an ended session is listed as live or offers revoke")
	}
	forms := postForm.FindAllString(html, -1)
	if len(forms) != 3 {
		t.Fatalf("POST forms = %d, want 3 (revoke, sign out, sign out everywhere)", len(forms))
	}
	for _, f := range forms {
		if !strings.Contains(f, `<input type="hidden" name="csrf" value="`+testCSRF+`">`) {
			t.Errorf("form without the CSRF field: %s", f)
		}
	}
	if regexp.MustCompile(`<form\b[^>]*method="get"`).MatchString(html) {
		t.Errorf("account page has a GET form")
	}
	assertNoInlineScript(t, "/account", html)
}

func TestSignInPage(t *testing.T) {
	html := render(t, context.Background(), SignInPage())
	if got := scriptTag.FindAllString(html, -1); len(got) != 1 || got[0] != `<script src="/static/signin.js"></script>` {
		t.Errorf("sign-in scripts = %q, want exactly /static/signin.js", got)
	}
	assertNoInlineScript(t, "/signin", html)
	// CSP default-src 'self' also forbids inline style.
	if strings.Contains(html, "style=") || strings.Contains(html, "<style") {
		t.Errorf("sign-in page has inline style")
	}
	for _, want := range []string{
		`<noscript>`,
		`id="signin-explain"`,
		`id="signin-progress" data-state="progress" role="status" hidden>`,
		`id="signin-failed" data-state="failed" role="alert" hidden>`,
		`id="signin-unavailable" data-state="unavailable" role="alert" hidden>`,
		`id="signin-rate-limited" data-state="rate-limited" role="alert" hidden>`,
		`id="signin-forbidden" data-state="forbidden" role="alert" hidden>`,
		"This sign-in link has already been used or has expired. Links work once, for 10 minutes.",
		SignInRateLimitedMessage, SignInForbiddenMessage,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("sign-in page lacks %q", want)
		}
	}
	// The page is static: a signed-in viewer gets the same bytes.
	if render(t, signedIn(), SignInPage()) != html {
		t.Errorf("sign-in page varies with the viewer")
	}
}

// TestSignInScriptStripsFragmentFirst is the cheap guard; the headless-browser
// recorder run is the real proof. replaceState must come before the only
// network call, and the link must travel in the body, never the URL.
func TestSignInScriptStripsFragmentFirst(t *testing.T) {
	src, err := os.ReadFile("../../static/signin.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	strip := strings.Index(js, "history.replaceState(null, '', '/signin')")
	fetch := strings.Index(js, "fetch(")
	if strip < 0 || fetch < 0 || strip > fetch {
		t.Fatalf("replaceState at %d, fetch at %d: the fragment must be removed before the request", strip, fetch)
	}
	if strings.Count(js, "fetch(") != 1 || strings.Contains(js, "XMLHttpRequest") || strings.Contains(js, "sendBeacon") {
		t.Fatalf("signin.js must make exactly one request")
	}
	if !strings.Contains(js, "body: 'link=' + encodeURIComponent(secret)") {
		t.Fatalf("the link is not sent in the POST body")
	}
}

func TestBrowserHint(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36 Edg/140.0": "Edge on Windows",
		"Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0":                                                "Firefox on Linux",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Mobile Safari/537.36":              "Chrome on Android",
		"curl/8.7.1": "curl",
		"":           "Unknown browser",
	} {
		if got := browserHint(ua); got != want {
			t.Errorf("browserHint(%q) = %q, want %q", ua, got, want)
		}
	}
}
