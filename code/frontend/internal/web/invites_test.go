package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The Invites page (docs/BOARD_LOGIN.md §10.10) against the stub backend's
// invite routes (invites_stub_test.go). Every code here is an obvious fake.

const (
	ownerSecret  = "mbs_fake-session-owner-0001"
	ownerKey     = "BS-OWNER00001"
	ownerAccount = "AC-owner"
	ownerName    = "Ana Owner"
)

// newInvitesHarness is world() plus an owner of the private team.
func newInvitesHarness(t *testing.T) *harness {
	t.Helper()
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	x.backend.addSession(ownerSecret, ownerKey, ownerAccount, ownerName)
	x.backend.makeOwner(privSlug, ownerAccount)
	return x
}

func invitesPath(slug string) string { return "/t/" + slug + "/invites" }

func revokePath(slug, id string) string { return invitesPath(slug) + "/" + id + "/revoke" }

func withCSRF(secret string, v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = vs
	}
	out.Set("csrf", csrfToken(secret))
	return out
}

// postAs is a same-origin browser POST, with the session cookie when secret is
// set. Later options override the defaults.
func postAs(x *harness, secret, path string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	base := []reqOpt{withHeader("Origin", testOrigin), withHeader("Sec-Fetch-Site", "same-origin")}
	if secret != "" {
		base = append(base, withCookie(secret))
	}
	return x.do(http.MethodPost, path, form.Encode(), append(base, opts...)...)
}

func norm(body, slug string) string { return strings.ReplaceAll(body, slug, "SLUG") }

// ---------------------------------------------------------------- the tab

func TestInvitesTabOnlyForSignedInMembers(t *testing.T) {
	x := newInvitesHarness(t)
	x.backend.addPublicMember(pubSlug, memberAccount)

	for _, c := range []struct {
		name, secret, path, slug string
		want                     bool
	}{
		{"member, private board", memberSecret, "/t/" + privSlug, privSlug, true},
		{"member, private graph", memberSecret, "/t/" + privSlug + "/graph", privSlug, true},
		{"owner, private conflicts", ownerSecret, "/t/" + privSlug + "/conflicts", privSlug, true},
		{"member of a public team", memberSecret, "/t/" + pubSlug, pubSlug, true},
		{"signed-in non-member, public team", outsiderSecret, "/t/" + pubSlug, pubSlug, false},
		{"anonymous, public team", "", "/t/" + pubSlug, pubSlug, false},
		{"member, demo board", memberSecret, "/t/demo", "demo", false},
		{"anonymous, demo board", "", "/t/demo/runs", "demo", false},
	} {
		rec := x.getAs(c.secret, c.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET %s = %d", c.name, c.path, rec.Code)
		}
		body := rec.Body.String()
		if got := strings.Contains(body, `href="`+invitesPath(c.slug)+`"`); got != c.want {
			t.Errorf("%s: Invites tab present=%v, want %v", c.name, got, c.want)
		}
		if !c.want && strings.Contains(body, ">Invites") {
			t.Errorf("%s: an Invites label is on the page", c.name)
		}
	}

	rec := x.getAs(memberSecret, invitesPath(privSlug))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `href="`+invitesPath(privSlug)+`" class="on"`) {
		t.Fatalf("the member's invites page: %d, active tab present=%v", rec.Code,
			strings.Contains(rec.Body.String(), `href="`+invitesPath(privSlug)+`" class="on"`))
	}
}

// ---------------------------------------------------------------- 404s

// Anonymous, a signed-in non-member (of a private or a public team) and a demo
// board all get the page an unknown team gets — GET and both POSTs — and the
// backend never sees a create.
func TestInvitesAre404LikeAnUnknownTeamForEverybodyElse(t *testing.T) {
	x := newInvitesHarness(t)
	create := url.Values{"label": {"nope"}}

	type probe struct {
		name string
		rec  func() *httptest.ResponseRecorder
		slug string
	}
	check := func(viewer string, ref *httptest.ResponseRecorder, probes []probe) {
		t.Helper()
		want := norm(ref.Body.String(), missing)
		if ref.Code != http.StatusNotFound || !strings.Contains(want, "No team called") {
			t.Fatalf("%s: the unknown team's page is %d", viewer, ref.Code)
		}
		for _, p := range probes {
			rec := p.rec()
			if rec.Code != http.StatusNotFound || norm(rec.Body.String(), p.slug) != want {
				t.Errorf("%s %s: %d, and the page differs from an unknown team's:\n%.300q\n%.300q",
					viewer, p.name, rec.Code, norm(rec.Body.String(), p.slug), want)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("%s %s: Location %q", viewer, p.name, loc)
			}
			assertNoSetCookie(t, viewer+" "+p.name, rec)
		}
	}

	// Anonymous: the board route of a slug that does not exist is the reference.
	check("anonymous", x.getAs("", "/t/"+missing), []probe{
		{"GET private", func() *httptest.ResponseRecorder { return x.getAs("", invitesPath(privSlug)) }, privSlug},
		{"GET public", func() *httptest.ResponseRecorder { return x.getAs("", invitesPath(pubSlug)) }, pubSlug},
		{"GET demo", func() *httptest.ResponseRecorder { return x.getAs("", invitesPath("demo")) }, "demo"},
		{"GET unknown", func() *httptest.ResponseRecorder { return x.getAs("", invitesPath(missing)) }, missing},
		{"POST create", func() *httptest.ResponseRecorder { return postAs(x, "", invitesPath(privSlug), create) }, privSlug},
		{"POST revoke", func() *httptest.ResponseRecorder {
			return postAs(x, "", revokePath(privSlug, "00000000-0000-4000-8000-000000000001"), url.Values{})
		}, privSlug},
	})

	// A signed-in non-member, with a valid CSRF token of their own.
	check("non-member", x.getAs(outsiderSecret, "/t/"+missing), []probe{
		{"GET private", func() *httptest.ResponseRecorder { return x.getAs(outsiderSecret, invitesPath(privSlug)) }, privSlug},
		{"GET public", func() *httptest.ResponseRecorder { return x.getAs(outsiderSecret, invitesPath(pubSlug)) }, pubSlug},
		{"GET demo", func() *httptest.ResponseRecorder { return x.getAs(outsiderSecret, invitesPath("demo")) }, "demo"},
		{"GET unknown", func() *httptest.ResponseRecorder { return x.getAs(outsiderSecret, invitesPath(missing)) }, missing},
		{"POST create, private", func() *httptest.ResponseRecorder {
			return postAs(x, outsiderSecret, invitesPath(privSlug), withCSRF(outsiderSecret, create))
		}, privSlug},
		{"POST create, public", func() *httptest.ResponseRecorder {
			return postAs(x, outsiderSecret, invitesPath(pubSlug), withCSRF(outsiderSecret, create))
		}, pubSlug},
		{"POST revoke", func() *httptest.ResponseRecorder {
			return postAs(x, outsiderSecret, revokePath(privSlug, "00000000-0000-4000-8000-000000000001"), withCSRF(outsiderSecret, url.Values{}))
		}, privSlug},
	})

	// A member of the private team is a non-member of the public one.
	check("member elsewhere", x.getAs(memberSecret, "/t/"+missing), []probe{
		{"GET public", func() *httptest.ResponseRecorder { return x.getAs(memberSecret, invitesPath(pubSlug)) }, pubSlug},
		{"GET demo", func() *httptest.ResponseRecorder { return x.getAs(memberSecret, invitesPath("demo")) }, "demo"},
	})

	if c := x.backend.inviteCreates(); len(c) != 0 {
		t.Fatalf("the backend saw %d creates", len(c))
	}
}

// ---------------------------------------------------------------- create

func TestCreateInviteNeedsCSRFAndSameOrigin(t *testing.T) {
	x := newInvitesHarness(t)
	path := invitesPath(privSlug)
	form := url.Values{"label": {"for Ana"}}

	for _, c := range []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"no token", postAs(x, memberSecret, path, form)},
		{"another session's token", postAs(x, memberSecret, path, withCSRF(outsiderSecret, form))},
		{"a foreign Origin", postAs(x, memberSecret, path, withCSRF(memberSecret, form), withHeader("Origin", "https://evil.test"))},
		{"a cross-site fetch", postAs(x, memberSecret, path, withCSRF(memberSecret, form), withHeader("Sec-Fetch-Site", "cross-site"))},
	} {
		if c.rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", c.name, c.rec.Code)
		}
		if strings.Contains(c.rec.Body.String(), "FAKEINV") {
			t.Errorf("%s: a code is in the answer", c.name)
		}
	}
	if n := len(x.backend.inviteCreates()); n != 0 {
		t.Fatalf("refused POSTs reached the backend %d times", n)
	}
	if rec := postAs(x, memberSecret, path, withCSRF(memberSecret, form)); rec.Code != http.StatusOK {
		t.Fatalf("with the token and the right origin: %d", rec.Code)
	}
}

// The code is in the POST's own answer — 200, no-store, no Location, in no
// header — and a reload of the page does not have it.
func TestCreateInviteShowsTheCodeOnce(t *testing.T) {
	x := newInvitesHarness(t)
	path := invitesPath(privSlug)
	rec := postAs(x, memberSecret, path, withCSRF(memberSecret, url.Values{
		"label": {"for Ana"}, "max_uses": {"2"}, "expires_in_hours": {"72"}}))
	codes := x.backend.inviteCodes()
	if rec.Code != http.StatusOK || len(codes) != 1 {
		t.Fatalf("create: %d, %d codes minted", rec.Code, len(codes))
	}
	code, body := codes[0], rec.Body.String()
	for _, want := range []string{
		`<pre id="invite-code">` + code + `</pre>`,
		`data-copy="invite-code"`,
		`<pre id="invite-install">curl -fsSL https://metiche.xyz/install.sh | sh</pre>`,
		"Share this code like a door code",
		"chooses <b>join</b> and pastes the code",
		`src="/static/landing.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the create answer lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("the page has an inline script")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("the create answer redirects: Location %q", loc)
	}
	for k, vs := range rec.Header() {
		for _, v := range vs {
			if strings.Contains(v, code) {
				t.Errorf("header %s carries the code", k)
			}
		}
	}
	assertNoSetCookie(t, "create", rec)
	if got := x.backend.inviteCreates()[0]; got["label"] != "for Ana" || got["max_uses"] != 2 || got["expires_in_hours"] != 72 {
		t.Errorf("the backend was asked for %v", got)
	}

	for _, p := range []string{path, "/t/" + privSlug} {
		again := x.getAs(memberSecret, p)
		if again.Code != http.StatusOK || strings.Contains(again.Body.String(), code) || strings.Contains(again.Body.String(), "invite-code") {
			t.Errorf("GET %s after the create: %d, code present=%v", p, again.Code, strings.Contains(again.Body.String(), code))
		}
	}
	reload := x.getAs(memberSecret, path).Body.String()
	id, _ := x.backend.inviteByLabel(privSlug, "for Ana")
	for _, want := range []string{"for Ana", `data-state="active"`, "created by you", "uses 0 / 2", revokePath(privSlug, id)} {
		if !strings.Contains(reload, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
}

func TestCreateInviteRefusalsShowTheServerMessage(t *testing.T) {
	x := newInvitesHarness(t)
	path := invitesPath(privSlug)

	// The form offers each role its own caps; the backend enforces them.
	if b := x.getAs(memberSecret, path).Body.String(); !strings.Contains(b, `max="25"`) || strings.Contains(b, "30 days") {
		t.Error("the member's form does not show the member caps")
	}
	if b := x.getAs(ownerSecret, path).Body.String(); !strings.Contains(b, `max="100"`) || !strings.Contains(b, `value="720"`) {
		t.Error("the owner's form does not show the owner caps")
	}

	for _, c := range []struct {
		name, secret string
		form         url.Values
		status       int
		want         string
	}{
		{"member over the cap", memberSecret, url.Values{"label": {"big one"}, "max_uses": {"26"}}, http.StatusUnprocessableEntity,
			"Members may create invites with at most 25 uses and 168 hours (7 days); the team&#39;s owner can make larger ones"},
		{"member past 7 days", memberSecret, url.Values{"expires_in_hours": {"720"}}, http.StatusUnprocessableEntity, "at most 25 uses and 168 hours"},
		{"owner over the ceiling", ownerSecret, url.Values{"max_uses": {"101"}}, http.StatusUnprocessableEntity, "Max_uses may be at most 100"},
		{"not a number", memberSecret, url.Values{"max_uses": {"lots"}}, http.StatusUnprocessableEntity, "must be whole numbers"},
	} {
		rec := postAs(x, c.secret, path, withCSRF(c.secret, c.form))
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d, message present=%v", c.name, rec.Code, strings.Contains(rec.Body.String(), c.want))
		}
		if strings.Contains(rec.Body.String(), "FAKEINV") || strings.Contains(rec.Body.String(), "invite-code") {
			t.Errorf("%s: a code panel is on a refusal", c.name)
		}
	}
	if b := postAs(x, memberSecret, path, withCSRF(memberSecret, url.Values{"label": {"big one"}, "max_uses": {"26"}})).Body.String(); !strings.Contains(b, `value="big one"`) || !strings.Contains(b, `value="26"`) {
		t.Error("a refused create does not keep what was typed")
	}
	if n := len(x.backend.inviteCodes()); n != 0 {
		t.Fatalf("refusals minted %d codes", n)
	}

	x.backend.setInvitesLimited(true)
	rec := postAs(x, memberSecret, path, withCSRF(memberSecret, url.Values{}))
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "Too many invites created by this account in the last hour; try again in 59m0s") {
		t.Errorf("rate limited: %d", rec.Code)
	}
	x.backend.setInvitesLimited(false)

	// An outage is 503 and never clears the cookie.
	x.backend.setInvitesDown(true)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"GET":  x.getAs(memberSecret, path),
		"POST": postAs(x, memberSecret, path, withCSRF(memberSecret, url.Values{})),
		"revoke": postAs(x, memberSecret, revokePath(privSlug, "00000000-0000-4000-8000-000000000001"),
			withCSRF(memberSecret, url.Values{})),
	} {
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s during an outage: %d, want 503", name, rec.Code)
		}
		assertNoSetCookie(t, name+" during an outage", rec)
	}
	x.backend.setInvitesDown(false)
	x.backend.setSessionDown(true)
	x.srv.login.forget(func(Viewer) bool { return true })
	rec = x.getAs(memberSecret, path)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET with the session check down: %d, want 503", rec.Code)
	}
	assertNoSetCookie(t, "session check down", rec)
	x.backend.setSessionDown(false)
	if rec := x.getAs(memberSecret, path); rec.Code != http.StatusOK {
		t.Errorf("after the outage: %d", rec.Code)
	}
}

// ---------------------------------------------------------------- revoke

func TestRevokeInviteNeedsCSRFAndWorks(t *testing.T) {
	x := newInvitesHarness(t)
	path := invitesPath(privSlug)
	for _, c := range []struct{ secret, label string }{{memberSecret, "mine"}, {ownerSecret, "the owner's"}} {
		if rec := postAs(x, c.secret, path, withCSRF(c.secret, url.Values{"label": {c.label}})); rec.Code != http.StatusOK {
			t.Fatalf("seeding %q: %d", c.label, rec.Code)
		}
	}
	mine, _ := x.backend.inviteByLabel(privSlug, "mine")
	owners, _ := x.backend.inviteByLabel(privSlug, "the owner's")

	memberPage := x.getAs(memberSecret, path).Body.String()
	if !strings.Contains(memberPage, revokePath(privSlug, mine)) || strings.Contains(memberPage, owners) {
		t.Errorf("the member's page: own revoke form=%v, owner's invite listed=%v",
			strings.Contains(memberPage, revokePath(privSlug, mine)), strings.Contains(memberPage, owners))
	}
	ownerPage := x.getAs(ownerSecret, path).Body.String()
	if !strings.Contains(ownerPage, revokePath(privSlug, mine)) || !strings.Contains(ownerPage, revokePath(privSlug, owners)) {
		t.Error("the owner's page lacks a revoke form")
	}

	state := func(label string) string { _, s := x.backend.inviteByLabel(privSlug, label); return s }
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"no token":         postAs(x, memberSecret, revokePath(privSlug, mine), url.Values{}),
		"a wrong token":    postAs(x, memberSecret, revokePath(privSlug, mine), withCSRF(outsiderSecret, url.Values{})),
		"a foreign Origin": postAs(x, memberSecret, revokePath(privSlug, mine), withCSRF(memberSecret, url.Values{}), withHeader("Origin", "https://evil.test")),
	} {
		if rec.Code != http.StatusForbidden || state("mine") != "active" {
			t.Errorf("revoke with %s: %d, state %q", name, rec.Code, state("mine"))
		}
	}

	// A member cannot revoke the owner's invite.
	rec := postAs(x, memberSecret, revokePath(privSlug, owners), withCSRF(memberSecret, url.Values{}))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not one you can revoke") || state("the owner's") != "active" {
		t.Errorf("member revoking the owner's: %d, state %q", rec.Code, state("the owner's"))
	}

	rec = postAs(x, memberSecret, revokePath(privSlug, mine), withCSRF(memberSecret, url.Values{}))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path || state("mine") != "revoked" {
		t.Fatalf("revoke: %d Location %q, state %q", rec.Code, rec.Header().Get("Location"), state("mine"))
	}
	after := x.getAs(memberSecret, path).Body.String()
	if !strings.Contains(after, `data-state="revoked"`) || strings.Contains(after, revokePath(privSlug, mine)) {
		t.Error("a revoked invite still has a Revoke form, or is not shown as revoked")
	}

	// The owner may revoke a member's invite; here, their own.
	if rec := postAs(x, ownerSecret, revokePath(privSlug, owners), withCSRF(ownerSecret, url.Values{})); rec.Code != http.StatusSeeOther || state("the owner's") != "revoked" {
		t.Errorf("owner revoking: %d, state %q", rec.Code, state("the owner's"))
	}
}
