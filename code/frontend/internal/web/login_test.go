package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// Board sign-in and private boards (docs/BOARD_LOGIN.md §2, §4, §6) against
// the stub backend's browser-session API (login_stub_test.go). Every secret
// here is an obvious fake.

const (
	testOrigin = "https://board.test"

	privSlug = "hidden-crew"
	privName = "Hidden Crew"
	pubSlug  = "open-crew"
	pubName  = "Open Crew"
	missing  = "no-such-team-7q"

	memberSecret   = "mbs_fake-session-member-0001"
	memberKey      = "BS-MEMBER0001"
	memberAccount  = "AC-member"
	memberName     = "Mara Member"
	outsiderSecret = "mbs_fake-session-outsider-0001"
	outsiderKey    = "BS-OUTSIDE001"
	outsiderName   = "Olek Outsider"

	fakeLink   = "mbl_fake-link-0000000001"
	mintedSess = "mbs_fake-session-minted-0001"
)

func newLoginHarness(t *testing.T, disc Discovery, lg Login) *harness {
	t.Helper()
	x := newHarness(t, disc)
	clock := func() time.Time { return time.Unix(0, x.clock.Load()) }
	x.backend.mu.Lock()
	x.backend.login.now = clock
	x.backend.mu.Unlock()
	lg.Backend = &feed.BrowserClient{BaseURL: x.stub.URL, Client: x.stub.Client()}
	lg.NewViewerFeed = func(slug, session string) feed.Feed {
		return &feed.Live{BaseURL: x.stub.URL, Slug: slug, BrowserSession: session, Client: x.stub.Client(),
			Logger: quiet(), Backoff: 5 * time.Millisecond}
	}
	if lg.BaseURL == "" {
		lg.BaseURL = testOrigin
	}
	if lg.Now == nil {
		lg.Now = clock
	}
	if err := x.srv.EnableLogin(x.ctx, lg); err != nil {
		t.Fatal(err)
	}
	return x
}

// world registers a demo, makes a public team and a private team with one
// member, and gives the member and an outsider a session each.
func (x *harness) world(t *testing.T) {
	t.Helper()
	if _, err := x.srv.AddDemoTeam(x.ctx, "demo", "Orbital Freight", "", idleFeed{}); err != nil {
		t.Fatal(err)
	}
	x.backend.setPublic(pubSlug, pubName)
	x.backend.makePrivateTeam(privSlug, privName, memberAccount)
	x.backend.addSession(memberSecret, memberKey, memberAccount, memberName)
	x.backend.addSession(outsiderSecret, outsiderKey, "AC-outsider", outsiderName)
}

func (b *stubBackend) revoke(secret string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.sessions[secret].revoked = true
}

type reqOpt func(*http.Request)

func withCookie(secret string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: secret}) }
}

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func (x *harness) do(method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, o := range opts {
		o(req)
	}
	// A stream that is (wrongly) served must not hang the test.
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func (x *harness) getAs(secret, path string) *httptest.ResponseRecorder {
	if secret == "" {
		return x.do(http.MethodGet, path, "")
	}
	return x.do(http.MethodGet, path, "", withCookie(secret))
}

func (x *harness) boardCount() int { return x.srv.login.boards.count() }

func (x *harness) viewerTeam(t *testing.T, slug string) *Team {
	t.Helper()
	vb := x.srv.login.boards
	vb.mu.Lock()
	defer vb.mu.Unlock()
	for k, b := range vb.boards {
		if k.slug == slug {
			return b.team
		}
	}
	t.Fatalf("no private board for %s", slug)
	return nil
}

// cookie is one parsed Set-Cookie line.
type cookie struct {
	name, value string
	attrs       map[string]string // lower-cased attribute name -> value ("" for flags)
}

func setCookies(rec *httptest.ResponseRecorder) []cookie {
	var out []cookie
	for _, line := range rec.Result().Header.Values("Set-Cookie") {
		parts := strings.Split(line, ";")
		name, value, _ := strings.Cut(strings.TrimSpace(parts[0]), "=")
		c := cookie{name: name, value: value, attrs: map[string]string{}}
		for _, p := range parts[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			c.attrs[strings.ToLower(k)] = v
		}
		out = append(out, c)
	}
	return out
}

func assertNoSetCookie(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if v := rec.Result().Header.Values("Set-Cookie"); len(v) != 0 {
		t.Fatalf("%s: Set-Cookie %q", what, v)
	}
}

func assertCookieCleared(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	cs := setCookies(rec)
	if len(cs) != 1 || cs[0].name != sessionCookieName || cs[0].value != "" || cs[0].attrs["max-age"] != "0" {
		t.Fatalf("%s: want the session cookie cleared, got %+v", what, cs)
	}
}

func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func signinPost(x *harness, link string, opts ...reqOpt) *httptest.ResponseRecorder {
	opts = append([]reqOpt{withHeader("Origin", testOrigin), withHeader("Sec-Fetch-Site", "same-origin")}, opts...)
	return x.do(http.MethodPost, "/signin", "link="+link, opts...)
}

// openStreamAs connects a real SSE client with a session cookie.
func openStreamAs(t *testing.T, base, path, secret string) (*http.Response, <-chan struct{}) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	if secret != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: secret})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream %s: %d", path, resp.StatusCode)
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
	return resp, done
}

// ---------------------------------------------------------------- §4.5

// TestViewerMatrix is the §4.5 table: anonymous, a member and a signed-in
// non-member, against a demo, a public live team, a private team and a slug
// that does not exist. For one viewer, every 404 is the same page whatever
// its cause.
func TestViewerMatrix(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)

	viewers := []struct {
		name, secret, display string
		want                  map[string]int
	}{
		{"anonymous", "", "", map[string]int{"demo": 200, pubSlug: 200, privSlug: 404, missing: 404}},
		{"member", memberSecret, memberName, map[string]int{"demo": 200, pubSlug: 200, privSlug: 200, missing: 404}},
		{"non-member", outsiderSecret, outsiderName, map[string]int{"demo": 200, pubSlug: 200, privSlug: 404, missing: 404}},
	}
	for _, v := range viewers {
		signedIn := v.secret != ""
		for _, slug := range []string{"demo", pubSlug, privSlug, missing} {
			rec := x.getAs(v.secret, "/t/"+slug)
			body := rec.Body.String()
			if rec.Code != v.want[slug] {
				t.Fatalf("%s GET /t/%s: %d, want %d", v.name, slug, rec.Code, v.want[slug])
			}
			if got := rec.Header().Get("Cache-Control") == "private, no-store"; got != signedIn {
				t.Errorf("%s GET /t/%s: Cache-Control %q", v.name, slug, rec.Header().Get("Cache-Control"))
			}
			for _, name := range []string{memberName, outsiderName} {
				if strings.Contains(body, name) != (name == v.display) {
					t.Errorf("%s GET /t/%s: indicator for %q present=%v", v.name, slug, name, strings.Contains(body, name))
				}
			}
			if slug == "demo" && !strings.Contains(body, "DEMO · a recording.") {
				t.Errorf("%s: the demo is not labelled", v.name)
			}
			if strings.Contains(body, privName) != (slug == privSlug && rec.Code == 200) {
				t.Errorf("%s GET /t/%s: private team name present=%v", v.name, slug, strings.Contains(body, privName))
			}
			assertNoSetCookie(t, v.name+" /t/"+slug, rec)
		}
		if v.name == "member" {
			continue
		}
		// The private team and the nonexistent slug are one page for this
		// viewer (the page names the slug that was asked for, nothing else).
		for _, sub := range []string{"", "/graph", "/stream"} {
			a := x.getAs(v.secret, "/t/"+privSlug+sub)
			b := x.getAs(v.secret, "/t/"+missing+sub)
			na := strings.ReplaceAll(a.Body.String(), privSlug, "SLUG")
			nb := strings.ReplaceAll(b.Body.String(), missing, "SLUG")
			if a.Code != 404 || b.Code != 404 || na != nb {
				t.Errorf("%s %s: private %d and nonexistent %d differ:\n%.300q\n%.300q", v.name, sub, a.Code, b.Code, na, nb)
			}
		}
	}

	if _, ok := x.srv.Lookup(privSlug); ok {
		t.Fatal("the private team is in the shared registry")
	}
	for _, tm := range x.srv.Teams() {
		if tm.Slug == privSlug || tm.viewer {
			t.Fatalf("Teams() lists %s (viewer=%v)", tm.Slug, tm.viewer)
		}
	}
	if x.boardCount() != 1 {
		t.Fatalf("private boards = %d, want 1 (the member's)", x.boardCount())
	}
	if tm := x.viewerTeam(t, privSlug); !tm.viewer || tm.Name != privName {
		t.Fatalf("member's board: viewer=%v name=%q", tm.viewer, tm.Name)
	}
}

// TestPrivateBoardIsNeverServedToAnybodyElse: once a member has a private
// board open, an anonymous visitor and a signed-in non-member still get 404
// for it, and it is in no shared list. (Mutation: register the viewer hub in
// Server.teams → anonymous gets 200.)
func TestPrivateBoardIsNeverServedToAnybodyElse(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 200 || !strings.Contains(rec.Body.String(), privName) {
		t.Fatalf("member: %d", rec.Code)
	}
	for _, who := range []struct{ name, secret string }{{"anonymous", ""}, {"non-member", outsiderSecret}} {
		for _, p := range []string{"/t/" + privSlug, "/t/" + privSlug + "/graph", "/t/" + privSlug + "/runs", "/t/" + privSlug + "/stream"} {
			rec := x.getAs(who.secret, p)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s GET %s after the member opened it: %d, want 404", who.name, p, rec.Code)
			}
			if strings.Contains(rec.Body.String(), privName) {
				t.Fatalf("%s GET %s: body names the private team", who.name, p)
			}
		}
	}
	if _, ok := x.srv.Lookup(privSlug); ok {
		t.Fatal("Lookup finds the private team")
	}
	for _, tm := range x.srv.Teams() {
		if tm.viewer || tm.Slug == privSlug {
			t.Fatal("Teams() lists a private board")
		}
	}
	var h struct {
		LiveTeams int `json:"live_teams"`
	}
	rec := x.get("/healthz")
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil || h.LiveTeams != 0 || strings.Contains(rec.Body.String(), privSlug) {
		t.Fatalf("/healthz counts or names the private board: %s", rec.Body.String())
	}
}

// TestMemberIsNeverShownTheAnonymousNegativeCache: an anonymous 404 for a
// private team is remembered, and the member asking next still gets the
// board. A signed-in 404 is remembered nowhere. (Mutation: consult the
// negative cache by slug for signed-in requests → the member gets the cached
// 404.)
func TestMemberIsNeverShownTheAnonymousNegativeCache(t *testing.T) {
	x := newLoginHarness(t, Discovery{NotFoundTTL: time.Hour}, Login{})
	x.world(t)

	if rec := x.getAs("", "/t/"+privSlug); rec.Code != 404 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	if !x.remembered404(privSlug) {
		t.Fatal("setup: the anonymous 404 is not remembered")
	}
	if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 200 {
		t.Fatalf("member after an anonymous 404: %d, want 200 — a cached anonymous answer was applied to a signed-in request", rec.Code)
	}
	probes := x.probes.Load()
	if rec := x.getAs("", "/t/"+privSlug); rec.Code != 404 || x.probes.Load() != probes {
		t.Fatalf("anonymous again: %d, probes +%d", rec.Code, x.probes.Load()-probes)
	}

	// A signed-in 404 teaches the anonymous cache nothing.
	x.backend.makePrivateTeam("other-crew", "Other Crew")
	for _, slug := range []string{"other-crew", "never-was-7q"} {
		if rec := x.getAs(outsiderSecret, "/t/"+slug); rec.Code != 404 {
			t.Fatalf("non-member %s: %d", slug, rec.Code)
		}
		if x.remembered404(slug) {
			t.Fatalf("a signed-in 404 for %s was remembered", slug)
		}
	}

	// And a remembered anonymous 404 for a team made public since does not
	// hide it from a signed-in viewer either.
	if rec := x.getAs("", "/t/late-crew"); rec.Code != 404 || !x.remembered404("late-crew") {
		t.Fatalf("setup late-crew: %d", rec.Code)
	}
	x.backend.setPublic("late-crew", "Late Crew")
	if rec := x.getAs(outsiderSecret, "/t/late-crew"); rec.Code != 200 {
		t.Fatalf("signed-in viewer of a newly public team: %d", rec.Code)
	}
	if tm, ok := x.srv.Lookup("late-crew"); !ok || tm.viewer {
		t.Fatal("a public team reached by a signed-in viewer is not the shared board")
	}
}

// ---------------------------------------------------------------- /signin

// TestSessionCookieAttributes asserts the cookie one attribute at a time
// (§2.6). (Mutation: drop HttpOnly or SameSite.)
func TestSessionCookieAttributes(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	x.backend.addLink(fakeLink, stubSession{secret: mintedSess, key: "BS-MINTED0001", account: memberAccount,
		name: memberName, redirect: "/t/" + privSlug})

	ua := "Mozilla/5.0 (Test)" + strings.Repeat("x", 300)
	rec := signinPost(x, fakeLink, withHeader("User-Agent", ua))
	if rec.Code != 200 || jsonBody(t, rec)["redirect"] != "/t/"+privSlug {
		t.Fatalf("sign-in: %d %s", rec.Code, rec.Body.String())
	}
	cs := setCookies(rec)
	if len(cs) != 1 {
		t.Fatalf("Set-Cookie lines = %d", len(cs))
	}
	c := cs[0]
	if c.name != "__Host-metiche_session" {
		t.Errorf("name = %q", c.name)
	}
	if c.value != mintedSess {
		t.Errorf("value is not the minted session")
	}
	if v, ok := c.attrs["path"]; !ok || v != "/" {
		t.Errorf("Path = %q (present %v), want /", v, ok)
	}
	if _, ok := c.attrs["secure"]; !ok {
		t.Error("not Secure")
	}
	if _, ok := c.attrs["httponly"]; !ok {
		t.Error("not HttpOnly")
	}
	if v := c.attrs["samesite"]; v != "Lax" {
		t.Errorf("SameSite = %q, want Lax", v)
	}
	if _, ok := c.attrs["domain"]; ok {
		t.Error("has a Domain")
	}
	if age, err := strconv.Atoi(c.attrs["max-age"]); err != nil || age != 30*24*3600 {
		t.Errorf("Max-Age = %q, want %d", c.attrs["max-age"], 30*24*3600)
	}
	if len(c.attrs) != 5 {
		t.Errorf("unexpected attributes: %v", c.attrs)
	}

	x.backend.mu.Lock()
	ex := x.backend.login.exchanges[0]
	x.backend.mu.Unlock()
	if ex["link_secret"] != fakeLink || len(ex["user_agent"]) != 200 || ex["ip_hint"] != "" {
		t.Fatalf("exchange body: link ok=%v ua=%d ip=%q", ex["link_secret"] == fakeLink, len(ex["user_agent"]), ex["ip_hint"])
	}

	// The cookie works, and the exchange primed the session cache.
	checks, _, _ := x.backend.loginCounts()
	if rec := x.getAs(mintedSess, "/t/"+privSlug); rec.Code != 200 {
		t.Fatalf("signed-in board: %d", rec.Code)
	}
	if after, _, _ := x.backend.loginCounts(); after != checks {
		t.Fatalf("session checks after sign-in: %d, want %d", after, checks)
	}

	t.Run("dev insecure cookie", func(t *testing.T) {
		y := newLoginHarness(t, Discovery{}, Login{BaseURL: "http://localhost:8787", InsecureCookie: true})
		y.backend.addLink(fakeLink, stubSession{secret: mintedSess, key: "BS-MINTED0001", account: memberAccount, name: memberName, redirect: "/teams"})
		rec := y.do(http.MethodPost, "/signin", "link="+fakeLink, withHeader("Origin", "http://localhost:8787"))
		cs := setCookies(rec)
		if rec.Code != 200 || len(cs) != 1 {
			t.Fatalf("%d %v", rec.Code, cs)
		}
		c := cs[0]
		_, secure := c.attrs["secure"]
		_, httpOnly := c.attrs["httponly"]
		_, domain := c.attrs["domain"]
		if c.name != "metiche_session" || secure || !httpOnly || c.attrs["samesite"] != "Lax" || c.attrs["path"] != "/" || domain {
			t.Fatalf("dev cookie = %+v", c)
		}
	})
}

// TestEnableLoginRefusesUnsafeConfigurations: the dev cookie only on
// localhost, https everywhere else.
func TestEnableLoginRefusesUnsafeConfigurations(t *testing.T) {
	ok := func(base string, insecure bool) error {
		s := NewServer(nil, quiet())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		return s.EnableLogin(ctx, Login{Backend: &feed.BrowserClient{}, BaseURL: base, InsecureCookie: insecure,
			NewViewerFeed: func(string, string) feed.Feed { return idleFeed{} }})
	}
	for _, c := range []struct {
		base     string
		insecure bool
		good     bool
	}{
		{"https://metiche.xyz", false, true},
		{"http://localhost:8787", false, true},
		{"http://localhost:8787", true, true},
		{"http://127.0.0.1:8787", true, true},
		{"https://metiche.xyz", true, false},
		{"http://metiche.xyz", false, false},
		{"http://metiche.xyz", true, false},
		{"http://localhost.evil.test", true, false},
		{"https://metiche.xyz/board", false, false},
		{"https://metiche.xyz?x=1", false, false},
		{"", false, false},
	} {
		if err := ok(c.base, c.insecure); (err == nil) != c.good {
			t.Errorf("base %q insecure %v: err = %v", c.base, c.insecure, err)
		}
	}
}

// TestSigninHeaders: §2.5 and §6.4, on the page and on the POST.
func TestSigninHeaders(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	want := map[string]string{
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	}
	get := x.get("/signin")
	if get.Code != 200 || !strings.Contains(get.Body.String(), `src="/static/signin.js"`) {
		t.Fatalf("GET /signin: %d", get.Code)
	}
	post := signinPost(x, "mbl_nope")
	for name, rec := range map[string]*httptest.ResponseRecorder{"GET": get, "POST": post} {
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s /signin %s = %q, want %q", name, k, got, v)
			}
		}
	}
}

// TestSigninRefusesOtherOrigins: no Origin, a foreign one, "null", or a
// cross-site fetch is a 403 that reaches no backend. A link in the query
// string is never read.
func TestSigninRefusesOtherOrigins(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.backend.addLink(fakeLink, stubSession{secret: mintedSess, key: "BS-MINTED0001", account: memberAccount, name: memberName, redirect: "/"})
	for _, c := range []struct {
		name string
		opts []reqOpt
	}{
		{"no Origin", nil},
		{"foreign Origin", []reqOpt{withHeader("Origin", "https://evil.test")}},
		{"null Origin", []reqOpt{withHeader("Origin", "null")}},
		{"http twin of the origin", []reqOpt{withHeader("Origin", "http://board.test")}},
		{"cross-site fetch", []reqOpt{withHeader("Origin", testOrigin), withHeader("Sec-Fetch-Site", "cross-site")}},
		{"same-site fetch", []reqOpt{withHeader("Origin", testOrigin), withHeader("Sec-Fetch-Site", "same-site")}},
	} {
		rec := x.do(http.MethodPost, "/signin", "link="+fakeLink, c.opts...)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", c.name, rec.Code)
		}
		assertNoSetCookie(t, c.name, rec)
	}
	rec := x.do(http.MethodPost, "/signin?link="+fakeLink, "", withHeader("Origin", testOrigin))
	if rec.Code != 200 || jsonBody(t, rec)["error"] != "link" {
		t.Fatalf("link in the query: %d %s", rec.Code, rec.Body.String())
	}
	if _, _, ex := x.backend.loginCounts(); ex != 0 {
		t.Fatalf("exchanges = %d, want 0", ex)
	}
	// The same link, properly sent, still works: nothing above spent it.
	if rec := signinPost(x, fakeLink); jsonBody(t, rec)["redirect"] != "/" {
		t.Fatalf("proper sign-in: %s", rec.Body.String())
	}
}

// TestSigninFailuresAndOutage: a used or unknown link is the one failure
// answer; an outage is "unavailable", sets no cookie and spends nothing; the
// per-client limit holds.
func TestSigninFailuresAndOutage(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{SigninLimit: 4})
	x.backend.addLink(fakeLink, stubSession{secret: mintedSess, key: "BS-MINTED0001", account: memberAccount, name: memberName, redirect: "https://evil.test/"})

	x.backend.setDown(true)
	rec := signinPost(x, fakeLink)
	if rec.Code != http.StatusServiceUnavailable || jsonBody(t, rec)["error"] != "unavailable" {
		t.Fatalf("outage: %d %s", rec.Code, rec.Body.String())
	}
	assertNoSetCookie(t, "outage", rec)
	x.backend.setDown(false)

	rec = signinPost(x, fakeLink)
	if rec.Code != 200 || jsonBody(t, rec)["redirect"] != "/" {
		t.Fatalf("after the outage (and a stored redirect that is not a board path): %d %s", rec.Code, rec.Body.String())
	}
	for _, link := range []string{fakeLink, "mbl_fake-link-unknown-0001"} {
		rec = signinPost(x, link)
		if rec.Code != 200 || jsonBody(t, rec)["error"] != "link" || len(jsonBody(t, rec)) != 1 {
			t.Fatalf("refused link: %d %s", rec.Code, rec.Body.String())
		}
		assertNoSetCookie(t, "refused link", rec)
	}
	if rec = signinPost(x, "mbl_fake-link-unknown-0002"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("5th attempt with a limit of 4: %d, want 429", rec.Code)
	}
}

// TestSigninReplacesAnExistingSession: a cookie already present is never
// reused, and its session is revoked.
func TestSigninReplacesAnExistingSession(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	x.backend.addLink(fakeLink, stubSession{secret: mintedSess, key: "BS-MINTED0001", account: memberAccount, name: memberName, redirect: "/teams"})
	rec := signinPost(x, fakeLink, withCookie(memberSecret))
	cs := setCookies(rec)
	if len(cs) != 1 || cs[0].value != mintedSess {
		t.Fatalf("cookie: %+v", cs)
	}
	if !x.backend.revoked(memberSecret) {
		t.Fatal("the replaced session was not revoked")
	}
}

// ---------------------------------------------------------------- CSRF

// TestSignoutRequiresCSRFAndSameOrigin (§6.3). (Mutation: skip the CSRF
// compare → the tokenless POST signs the viewer out.)
func TestSignoutRequiresCSRFAndSameOrigin(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 200 {
		t.Fatalf("member: %d", rec.Code)
	}
	token := csrfToken(memberSecret)
	for _, c := range []struct {
		name string
		body string
		opts []reqOpt
	}{
		{"no token", "", []reqOpt{withHeader("Origin", testOrigin)}},
		{"no token, no Origin", "", nil},
		{"another session's token", "", []reqOpt{withHeader("X-CSRF-Token", csrfToken(outsiderSecret))}},
		{"another session's token in the form", "csrf=" + csrfToken(outsiderSecret), nil},
		{"token with a tail", "", []reqOpt{withHeader("X-CSRF-Token", token+"x")}},
		{"foreign Origin", "", []reqOpt{withHeader("Origin", "https://evil.test"), withHeader("X-CSRF-Token", token)}},
		{"null Origin", "", []reqOpt{withHeader("Origin", "null"), withHeader("X-CSRF-Token", token)}},
		{"cross-site fetch", "", []reqOpt{withHeader("Sec-Fetch-Site", "cross-site"), withHeader("X-CSRF-Token", token)}},
	} {
		rec := x.do(http.MethodPost, "/signout", c.body, append([]reqOpt{withCookie(memberSecret)}, c.opts...)...)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d, want 403", c.name, rec.Code)
		}
		assertNoSetCookie(t, c.name, rec)
		if x.backend.revoked(memberSecret) || x.boardCount() != 1 {
			t.Fatalf("%s: the session was ended", c.name)
		}
	}
	if rec := x.getAs(memberSecret, "/signout"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /signout: %d, want 404", rec.Code)
	}

	rec := x.do(http.MethodPost, "/signout", "", withCookie(memberSecret), withHeader("Origin", testOrigin),
		withHeader("Sec-Fetch-Site", "same-origin"), withHeader("X-CSRF-Token", token))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("sign out: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	assertCookieCleared(t, "sign out", rec)
	c := setCookies(rec)[0]
	_, secure := c.attrs["secure"]
	_, httpOnly := c.attrs["httponly"]
	if !secure || !httpOnly || c.attrs["samesite"] != "Lax" || c.attrs["path"] != "/" {
		t.Fatalf("the clearing cookie does not match the one set: %+v", c)
	}
	if !x.backend.revoked(memberSecret) || x.boardCount() != 0 {
		t.Fatalf("sign out: revoked=%v boards=%d", x.backend.revoked(memberSecret), x.boardCount())
	}

	replay := x.getAs(memberSecret, "/t/"+privSlug)
	if replay.Code != http.StatusNotFound {
		t.Fatalf("replayed cookie: %d, want 404", replay.Code)
	}
	assertCookieCleared(t, "replayed cookie", replay)

	// A plain form, as the topbar sends it.
	rec = x.do(http.MethodPost, "/signout", "csrf="+csrfToken(outsiderSecret), withCookie(outsiderSecret), withHeader("Origin", testOrigin))
	if rec.Code != http.StatusSeeOther || !x.backend.revoked(outsiderSecret) {
		t.Fatalf("form sign out: %d", rec.Code)
	}
}

// TestAccountPagesAndPosts: /account is signed-in only; sign out everywhere
// and revoke need the token, and revoke cannot reach another account.
func TestAccountPagesAndPosts(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	const second = "mbs_fake-session-member-0002"
	x.backend.addSession(second, "BS-MEMBER0002", memberAccount, memberName)
	token := csrfToken(memberSecret)
	post := func(path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
		return x.do(http.MethodPost, path, body, append([]reqOpt{withCookie(memberSecret), withHeader("Origin", testOrigin)}, opts...)...)
	}

	if rec := x.get("/account"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/signin" {
		t.Fatalf("anonymous /account: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	rec := x.getAs(memberSecret, "/account")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, memberKey) || !strings.Contains(body, "BS-MEMBER0002") || strings.Contains(body, outsiderKey) {
		t.Fatalf("/account: %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(body, token) {
		t.Fatalf("/account headers or token: %q", rec.Header().Get("Cache-Control"))
	}
	if strings.Contains(body, memberSecret) || strings.Contains(body, second) {
		t.Fatal("/account renders a session secret")
	}

	if rec := post("/account/signout-all", ""); rec.Code != 403 || x.backend.revoked(memberSecret) {
		t.Fatalf("signout-all without a token: %d", rec.Code)
	}
	if rec := post("/account/sessions/BS-MEMBER0002/revoke", ""); rec.Code != 403 || x.backend.revoked(second) {
		t.Fatalf("revoke without a token: %d", rec.Code)
	}
	if rec := post("/account/sessions/"+outsiderKey+"/revoke", "", withHeader("X-CSRF-Token", token)); rec.Code != 404 || x.backend.revoked(outsiderSecret) {
		t.Fatalf("revoke another account's session: %d", rec.Code)
	}
	rec = post("/account/sessions/BS-MEMBER0002/revoke", "csrf="+token)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account" || !x.backend.revoked(second) || x.backend.revoked(memberSecret) {
		t.Fatalf("revoke: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	assertNoSetCookie(t, "revoking another browser", rec)

	rec = post("/account/signout-all", "csrf="+token)
	if rec.Code != http.StatusSeeOther || !x.backend.revoked(memberSecret) {
		t.Fatalf("signout-all: %d", rec.Code)
	}
	assertCookieCleared(t, "signout-all", rec)
}

// ---------------------------------------------------------------- semantics

// TestOutageIs503AndKeepsTheCookie: a backend that cannot answer is never a
// 404 and never clears a cookie; demo and registered public boards keep
// serving. A session the backend says is invalid is cleared.
func TestOutageIs503AndKeepsTheCookie(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	if rec := x.get("/t/" + pubSlug); rec.Code != 200 {
		t.Fatalf("discover the public team: %d", rec.Code)
	}

	check := func(phase string) {
		t.Helper()
		for _, p := range []string{"/t/" + privSlug, "/t/" + privSlug + "/graph", "/t/" + privSlug + "/stream", "/t/" + missing, "/account"} {
			rec := x.getAs(memberSecret, p)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s: GET %s: %d, want 503", phase, p, rec.Code)
			}
			assertNoSetCookie(t, phase+" "+p, rec)
		}
		for _, p := range []string{"/t/demo", "/t/" + pubSlug} {
			rec := x.getAs(memberSecret, p)
			if rec.Code != 200 {
				t.Fatalf("%s: GET %s: %d, want 200", phase, p, rec.Code)
			}
			assertNoSetCookie(t, phase+" "+p, rec)
		}
	}

	x.backend.setDown(true)
	check("session check down")
	rec := x.do(http.MethodPost, "/signout", "", withCookie(memberSecret), withHeader("X-CSRF-Token", csrfToken(memberSecret)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("sign out during the outage: %d", rec.Code)
	}
	assertNoSetCookie(t, "sign out during the outage", rec)

	x.backend.setDown(false)
	if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 200 {
		t.Fatalf("back up: %d", rec.Code)
	}
	// Now the session is cached and only /access fails.
	x.backend.setDown(true)
	check("access down, session cached")
	x.backend.setDown(false)

	x.backend.revoke(memberSecret)
	x.advance(16 * time.Second)
	rec = x.getAs(memberSecret, "/t/"+privSlug)
	if rec.Code != 404 {
		t.Fatalf("revoked session: %d", rec.Code)
	}
	assertCookieCleared(t, "revoked session", rec)
}

// TestSessionCheckIsCachedAndAccessIsNot: 15s for "who is this", never for
// "may they read this team".
func TestSessionCheckIsCachedAndAccessIsNot(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	for i := 0; i < 2; i++ {
		if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 200 {
			t.Fatalf("GET %d: %d", i, rec.Code)
		}
	}
	if s, a, _ := x.backend.loginCounts(); s != 1 || a != 2 {
		t.Fatalf("session checks %d, access checks %d; want 1 and 2", s, a)
	}
	x.advance(14 * time.Second)
	x.getAs(memberSecret, "/t/"+privSlug)
	if s, _, _ := x.backend.loginCounts(); s != 1 {
		t.Fatalf("session checked again inside 15s: %d", s)
	}
	x.advance(2 * time.Second)
	x.getAs(memberSecret, "/t/"+privSlug)
	if s, _, _ := x.backend.loginCounts(); s != 2 {
		t.Fatalf("session not re-checked after 15s: %d", s)
	}
	x.backend.removeMember(privSlug, memberAccount)
	if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 404 {
		t.Fatalf("removed member, next page: %d, want 404", rec.Code)
	}
}

// TestPrivateStreamEndsWhenAccessIsLost (§4.4). The stub keeps the board's
// upstream stream open after the change, as a backend without its own re-auth
// would, so only the board's re-check can end the browser's stream.
// (Mutation: remove the board-side re-auth tick.)
func TestPrivateStreamEndsWhenAccessIsLost(t *testing.T) {
	for _, how := range []string{"membership removed", "session revoked"} {
		t.Run(how, func(t *testing.T) {
			const reauth = 100 * time.Millisecond
			x := newLoginHarness(t, Discovery{}, Login{Reauth: reauth})
			x.world(t)
			ts := serve(t, x.h)

			resp, ended := openStreamAs(t, ts.URL, "/t/"+privSlug+"/stream", memberSecret)
			if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") || !strings.Contains(cc, "no-store") {
				t.Fatalf("stream Cache-Control %q", cc)
			}
			waitUntil(t, "the upstream stream to open", func() bool { return x.backend.streamsFor(privSlug) == 1 })
			time.Sleep(4 * reauth)
			select {
			case <-ended:
				t.Fatal("the stream ended while the member still had access")
			default:
			}

			if how == "membership removed" {
				x.backend.removeMember(privSlug, memberAccount)
			} else {
				x.backend.revoke(memberSecret)
			}
			changed := time.Now()
			select {
			case <-ended:
			case <-time.After(20 * reauth):
				t.Fatalf("still streaming %v after access was lost (re-auth every %v)", time.Since(changed), reauth)
			}
			if got := x.backend.streamsFor(privSlug); got != 1 {
				t.Fatalf("upstream streams = %d; the upstream must stay healthy for this test to mean anything", got)
			}
			waitUntil(t, "the private board to close", func() bool { return x.boardCount() == 0 })
			if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 404 {
				t.Fatalf("next page: %d", rec.Code)
			}
		})
	}
}

// TestViewerBoardEviction: idle, upstream 404, and the cap.
func TestViewerBoardEviction(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		x := newLoginHarness(t, Discovery{}, Login{IdleGrace: 60 * time.Millisecond})
		x.world(t)
		if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 200 {
			t.Fatalf("%d", rec.Code)
		}
		tm := x.viewerTeam(t, privSlug)
		waitUntil(t, "the idle board to close", func() bool { return x.boardCount() == 0 })
		select {
		case <-tm.Hub.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the idle board's feed kept running")
		}
	})
	t.Run("upstream 404", func(t *testing.T) {
		x := newLoginHarness(t, Discovery{}, Login{Reauth: time.Hour})
		x.world(t)
		ts := serve(t, x.h)
		_, ended := openStreamAs(t, ts.URL, "/t/"+privSlug+"/stream", memberSecret)
		waitUntil(t, "the upstream stream", func() bool { return x.backend.streamsFor(privSlug) == 1 })
		x.backend.removeMember(privSlug, memberAccount)
		x.stub.CloseClientConnections()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("the stream survived the upstream 404")
		}
		waitUntil(t, "the board to close", func() bool { return x.boardCount() == 0 })
	})
	t.Run("cap", func(t *testing.T) {
		x := newLoginHarness(t, Discovery{}, Login{MaxViewerBoards: 1})
		x.world(t)
		const other = "mbs_fake-session-member-b-0001"
		x.backend.makePrivateTeam(privSlug, privName, "AC-member-b")
		x.backend.addSession(other, "BS-MEMBERB001", "AC-member-b", "Bo")
		ts := serve(t, x.h)
		resp, _ := openStreamAs(t, ts.URL, "/t/"+privSlug+"/stream", memberSecret)
		first := x.viewerTeam(t, privSlug)
		waitUntil(t, "a subscriber", func() bool { return first.Hub.Subscribers() == 1 })
		if rec := x.getAs(other, "/t/"+privSlug); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("over the cap with no idle board: %d, want 503", rec.Code)
		}
		_ = resp.Body.Close()
		waitUntil(t, "the subscriber to leave", func() bool { return first.Hub.Subscribers() == 0 })
		if rec := x.getAs(other, "/t/"+privSlug); rec.Code != 200 {
			t.Fatalf("at the cap with an idle board: %d, want 200", rec.Code)
		}
		if x.boardCount() != 1 || !first.Hub.Closed() {
			t.Fatalf("boards=%d, least recently used closed=%v", x.boardCount(), first.Hub.Closed())
		}
	})
}

// TestVisibilityFlipWithSignIn (F2): a public team discovered anonymously
// and then made private comes down for everybody else, and its member keeps
// seeing it on a board of their own.
func TestVisibilityFlipWithSignIn(t *testing.T) {
	x := newLoginHarness(t, Discovery{RecheckInterval: time.Hour}, Login{})
	x.backend.addSession(memberSecret, memberKey, memberAccount, memberName)
	x.backend.setPublic("flip-crew", "Flip Crew")
	if rec := x.get("/t/flip-crew"); rec.Code != 200 {
		t.Fatalf("anonymous while public: %d", rec.Code)
	}
	if rec := x.getAs(memberSecret, "/t/flip-crew"); rec.Code != 200 || x.boardCount() != 0 {
		t.Fatalf("member while public: %d, private boards %d (want the shared board)", rec.Code, x.boardCount())
	}
	x.backend.makePrivateTeam("flip-crew", "Flip Crew", memberAccount)
	x.stub.CloseClientConnections()
	waitUntil(t, "the shared board to come down", func() bool { _, ok := x.srv.Lookup("flip-crew"); return !ok })
	if rec := x.get("/t/flip-crew"); rec.Code != 404 {
		t.Fatalf("anonymous after the flip: %d", rec.Code)
	}
	if rec := x.getAs(memberSecret, "/t/flip-crew"); rec.Code != 200 || x.boardCount() != 1 {
		t.Fatalf("member after the flip: %d, private boards %d", rec.Code, x.boardCount())
	}
}

// TestControlsStay404ForSignedInViewers (F1): a member with a valid CSRF
// token gets the same 404 from the old control paths, on their private board
// and on the demo, and nothing moves.
func TestControlsStay404ForSignedInViewers(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	if rec := x.getAs(memberSecret, "/t/"+privSlug); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	priv := x.viewerTeam(t, privSlug)
	demo, _ := x.srv.Lookup("demo")
	marks := map[*Team]storeMark{priv: markOf(priv), demo: markOf(demo)}
	for _, slug := range []string{privSlug, "demo"} {
		for _, c := range controls {
			rec := x.do(http.MethodPost, "/t/"+slug+c.path, c.form.Encode(), withCookie(memberSecret),
				withHeader("Origin", testOrigin), withHeader("X-CSRF-Token", csrfToken(memberSecret)))
			if rec.Code != http.StatusNotFound {
				t.Errorf("POST /t/%s%s: %d, want 404", slug, c.path, rec.Code)
			}
		}
	}
	for tm, before := range marks {
		if after := markOf(tm); after != before {
			t.Errorf("%s: store moved", tm.Slug)
		}
	}
}

// TestHeaders: §6.4.
func TestHeaders(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	for _, p := range []string{"/", "/teams", "/t/demo", "/t/" + missing, "/healthz"} {
		rec := x.get(p)
		if rec.Header().Get("Referrer-Policy") != "same-origin" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("anonymous %s: Referrer-Policy %q nosniff %q", p, rec.Header().Get("Referrer-Policy"), rec.Header().Get("X-Content-Type-Options"))
		}
		if strings.Contains(rec.Header().Get("Cache-Control"), "private") {
			t.Errorf("anonymous %s: Cache-Control %q", p, rec.Header().Get("Cache-Control"))
		}
	}
	for _, p := range []string{"/t/" + privSlug, "/t/demo", "/t/" + pubSlug, "/t/" + missing, "/teams", "/account"} {
		rec := x.getAs(memberSecret, p)
		h := rec.Header()
		if h.Get("Cache-Control") != "private, no-store" || !strings.Contains(h.Get("Vary"), "Cookie") ||
			h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "same-origin" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("signed-in %s: %v", p, h)
		}
	}
	// "Your teams" is the member's alone.
	if body := x.getAs(memberSecret, "/teams").Body.String(); !strings.Contains(body, privName) {
		t.Error("signed-in /teams does not list the member's team")
	}
	if body := x.get("/teams").Body.String(); strings.Contains(body, privName) || strings.Contains(body, privSlug) {
		t.Error("anonymous /teams names a private team")
	}
}

// TestBackendSeesOneCredentialPerRequest: no bearer ever; anonymous reads
// carry no session; private reads carry the viewer's.
func TestBackendSeesOneCredentialPerRequest(t *testing.T) {
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	x.get("/t/" + pubSlug)
	x.getAs(memberSecret, "/t/"+pubSlug)
	x.getAs(memberSecret, "/t/"+privSlug)
	x.getAs(outsiderSecret, "/t/"+privSlug)
	x.backend.mu.Lock()
	defer x.backend.mu.Unlock()
	if x.backend.login.sawBearer {
		t.Fatal("the backend received a bearer")
	}
	if n := x.backend.login.sessionReads[pubSlug]; n != 0 {
		t.Fatalf("public team reads with a session: %d", n)
	}
	if n := x.backend.login.sessionReads[privSlug]; n < 3 {
		t.Fatalf("private team reads with a session: %d, want the snapshot's three and more", n)
	}
}

// ---------------------------------------------------------------- units

func TestClientIPAndHint(t *testing.T) {
	req := func(xff ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.9:5555"
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	for _, c := range []struct {
		r    *http.Request
		hops int
		want string
	}{
		{req("198.51.100.1"), 0, "10.0.0.9"},
		{req("6.6.6.6, 203.0.113.7"), 1, "203.0.113.7"},
		{req("6.6.6.6", "203.0.113.7"), 1, "203.0.113.7"},
		{req("6.6.6.6, 203.0.113.7, 10.0.0.1"), 2, "203.0.113.7"},
		{req(), 1, "10.0.0.9"},
		{req("not-an-address"), 1, "10.0.0.9"},
		{req("203.0.113.7"), 2, "10.0.0.9"},
	} {
		if got := clientIP(c.r, c.hops); got != c.want {
			t.Errorf("clientIP(%v, %d) = %q, want %q", c.r.Header.Values("X-Forwarded-For"), c.hops, got, c.want)
		}
	}
	if got := ipHint(req("203.0.113.7"), 1); got != "203.0.113.0/24" {
		t.Errorf("v4 hint %q", got)
	}
	if got := ipHint(req("2001:db8:abcd:12::1"), 1); got != "2001:db8:abcd::/48" {
		t.Errorf("v6 hint %q", got)
	}
	if got := ipHint(req("203.0.113.7"), 0); got != "" {
		t.Errorf("hint without a trusted hop %q", got)
	}
}

func TestUserAgentHintAndRedirects(t *testing.T) {
	if got := userAgentHint("Firefox\x00\n/1.0 ✓" + strings.Repeat("y", 300)); len(got) != 200 || !strings.HasPrefix(got, "Firefox/1.0 y") {
		t.Errorf("user agent hint %q", got)
	}
	for in, want := range map[string]string{
		"/": "/", "/teams": "/teams", "/t/acme-live": "/t/acme-live",
		"//evil.test": "/", "https://evil.test/t/x": "/", "/t/../admin": "/", "/account": "/", "": "/", "/t/Acme": "/",
	} {
		if got := safeRedirect(in); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCSRFToken(t *testing.T) {
	a, b := csrfToken(memberSecret), csrfToken(outsiderSecret)
	if a == b || a != csrfToken(memberSecret) || len(a) != 43 || strings.ContainsAny(a, "+/=") {
		t.Fatalf("tokens %q %q", a, b)
	}
	if strings.Contains(a, memberSecret) {
		t.Fatal("the token contains the secret")
	}
}
