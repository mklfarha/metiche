package feed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const (
	fakeLink    = "mbl_fake-link-0000000001"
	fakeSession = "mbs_fake-session-0001"
)

type seen struct {
	method, path, query, session, auth, contentType, body string
}

// browserStub answers each browser endpoint with the status in statuses (200
// or 201 by default) and records every request.
type browserStub struct {
	mu       sync.Mutex
	requests []seen
	statuses map[string]int // "METHOD path" -> status
}

func (b *browserStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.requests = append(b.requests, seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery,
		r.Header.Get(BrowserSessionHeader), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)})
	status, ok := b.statuses[r.Method+" "+r.URL.EscapedPath()]
	b.mu.Unlock()
	if ok && status >= 300 {
		http.Error(w, `{"detail":"database password is hunter2 for `+fakeSession+`"}`, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.EscapedPath() {
	case "POST /v1/browser/sessions":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"session_secret":"`+fakeSession+`","session_key":"BS-ABCDEFGHIJ","expires_at":"2026-10-12T10:00:00Z","redirect_path":"/t/acme","account_key":"AC-1","display_name":"Mara"}`)
	case "GET /v1/browser/session":
		_, _ = io.WriteString(w, `{"session_key":"BS-ABCDEFGHIJ","account_key":"AC-1","display_name":"Mara","expires_at":"2026-10-12T10:00:00Z"}`)
	case "GET /v1/teams/acme-live/access":
		_, _ = io.WriteString(w, `{"visibility":"private","role":"member"}`)
	case "GET /v1/teams/open-team/access":
		_, _ = io.WriteString(w, `{"visibility":"public","role":null}`)
	case "GET /v1/browser/sessions":
		_, _ = io.WriteString(w, `{"sessions":[{"key":"BS-ABCDEFGHIJ","created_at":"2026-09-12T10:00:00Z","last_seen_at":null,"expires_at":"2026-10-12T10:00:00Z","user_agent":"Firefox","state":"live","current":true,"revoked_at":null,"end_reason":""},`+
			`{"key":"BS-ENDED00001","created_at":"2026-09-11T10:00:00Z","last_seen_at":"2026-09-11T11:00:00Z","expires_at":"2026-10-11T10:00:00Z","user_agent":"Safari","state":"ended","current":false,"revoked_at":"2026-09-11T12:00:00Z","end_reason":"revoked_by_agent"}]}`)
	case "GET /v1/browser/teams":
		_, _ = io.WriteString(w, `{"teams":[{"slug":"acme-live","name":"Acme","visibility":"private","role":"owner"}]}`)
	default:
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}
}

func (b *browserStub) last() seen {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests[len(b.requests)-1]
}

// TestBrowserClientShapes pins every call the board makes to the backend's
// browser API: method, path, where the credential goes, and what is decoded.
func TestBrowserClientShapes(t *testing.T) {
	stub := &browserStub{statuses: map[string]int{}}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	c := &BrowserClient{BaseURL: srv.URL + "/v1/", Client: srv.Client()}
	ctx := context.Background()

	ex, err := c.Exchange(ctx, fakeLink, "Firefox", "203.0.113.0/24")
	if err != nil {
		t.Fatal(err)
	}
	got := stub.last()
	if got.method != "POST" || got.path != "/v1/browser/sessions" || got.query != "" || got.session != "" {
		t.Fatalf("exchange request = %+v", got)
	}
	if got.contentType != "application/json" {
		t.Fatalf("exchange content type %q", got.contentType)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatal(err)
	}
	if body["link_secret"] != fakeLink || body["user_agent"] != "Firefox" || body["ip_hint"] != "203.0.113.0/24" || len(body) != 3 {
		t.Fatalf("exchange body = %v", body)
	}
	if ex.SessionSecret != fakeSession || ex.SessionKey != "BS-ABCDEFGHIJ" || ex.RedirectPath != "/t/acme" ||
		ex.AccountKey != "AC-1" || ex.DisplayName != "Mara" || !ex.ExpiresAt.Equal(time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("exchanged = %+v", ex)
	}
	if strings.Contains(ex.String(), fakeSession) {
		t.Fatal("Exchanged.String prints the secret")
	}

	bs, err := c.Session(ctx, fakeSession)
	if err != nil || bs.SessionKey != "BS-ABCDEFGHIJ" || bs.DisplayName != "Mara" {
		t.Fatalf("session = %+v, %v", bs, err)
	}
	if got := stub.last(); got.method != "GET" || got.path != "/v1/browser/session" || got.session != fakeSession || got.query != "" {
		t.Fatalf("session request = %+v", got)
	}

	acc, err := c.Access(ctx, fakeSession, "acme-live")
	if err != nil || !acc.Private() || acc.Role != "member" {
		t.Fatalf("access = %+v, %v", acc, err)
	}
	if got := stub.last(); got.path != "/v1/teams/acme-live/access" || got.session != fakeSession {
		t.Fatalf("access request = %+v", got)
	}
	if acc, err := c.Access(ctx, fakeSession, "open-team"); err != nil || acc.Private() || acc.Role != "" {
		t.Fatalf("public access = %+v, %v", acc, err)
	}
	if _, err := c.Access(ctx, fakeSession, "no-such"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access 404: %v", err)
	}

	list, err := c.Sessions(ctx, fakeSession)
	if err != nil || len(list) != 2 || list[0].Key != "BS-ABCDEFGHIJ" || list[0].LastSeenAt != nil ||
		list[0].State != SessionLive || !list[0].Current || list[0].RevokedAt != nil || list[0].EndReason != "" {
		t.Fatalf("sessions = %+v, %v", list, err)
	}
	if e := list[1]; e.State != SessionEnded || e.EndReason != "revoked_by_agent" || e.RevokedAt == nil ||
		!e.RevokedAt.Equal(time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("ended session = %+v", e)
	}
	teams, err := c.Teams(ctx, fakeSession)
	if err != nil || len(teams) != 1 || teams[0].Slug != "acme-live" || teams[0].Visibility != "private" {
		t.Fatalf("teams = %+v, %v", teams, err)
	}

	for _, tc := range []struct {
		call   func() error
		method string
		path   string
	}{
		{func() error { return c.SignOut(ctx, fakeSession) }, "DELETE", "/v1/browser/session"},
		{func() error { return c.SignOutEverywhere(ctx, fakeSession) }, "DELETE", "/v1/browser/sessions"},
		{func() error { return c.Revoke(ctx, fakeSession, "BS-OTHER") }, "DELETE", "/v1/browser/sessions/BS-OTHER"},
	} {
		if err := tc.call(); err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if got := stub.last(); got.method != tc.method || got.path != tc.path || got.session != fakeSession {
			t.Fatalf("request = %+v, want %s %s", got, tc.method, tc.path)
		}
	}

	stub.mu.Lock()
	for _, r := range stub.requests {
		if r.auth != "" {
			t.Errorf("%s %s carried Authorization", r.method, r.path)
		}
		if strings.Contains(r.path+r.query, "mb") {
			t.Errorf("%s %s%s: a secret in the URL", r.method, r.path, r.query)
		}
	}
	stub.mu.Unlock()
}

// TestBrowserClientStatuses: 401 is the only answer that says a session is
// invalid, 404 the only one that refuses a link, and nothing else is either —
// an outage must never read as a verdict. No error carries the body or a
// secret.
func TestBrowserClientStatuses(t *testing.T) {
	stub := &browserStub{statuses: map[string]int{}}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	c := &BrowserClient{BaseURL: srv.URL, Client: srv.Client()}
	ctx := context.Background()
	set := func(k string, v int) { stub.mu.Lock(); stub.statuses[k] = v; stub.mu.Unlock() }

	noLeak := func(err error) {
		t.Helper()
		if err != nil && (strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "mbs_") || strings.Contains(err.Error(), "mbl_")) {
			t.Fatalf("error leaks: %v", err)
		}
	}

	set("POST /v1/browser/sessions", 404)
	_, err := c.Exchange(ctx, fakeLink, "", "")
	noLeak(err)
	if !errors.Is(err, ErrLinkRefused) {
		t.Fatalf("exchange 404: %v", err)
	}
	set("POST /v1/browser/sessions", 503)
	_, err = c.Exchange(ctx, fakeLink, "", "")
	noLeak(err)
	if err == nil || errors.Is(err, ErrLinkRefused) {
		t.Fatalf("exchange 503: %v", err)
	}

	set("GET /v1/browser/session", 401)
	_, err = c.Session(ctx, fakeSession)
	noLeak(err)
	if !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("session 401: %v", err)
	}
	for _, st := range []int{403, 404, 500, 503} {
		set("GET /v1/browser/session", st)
		_, err = c.Session(ctx, fakeSession)
		noLeak(err)
		if err == nil || errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("session %d: %v", st, err)
		}
	}

	set("GET /v1/teams/acme-live/access", 503)
	_, err = c.Access(ctx, fakeSession, "acme-live")
	noLeak(err)
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("access 503: %v", err)
	}
	if strings.Contains(err.Error(), "acme-live") {
		t.Fatalf("access error names the slug: %v", err)
	}

	set("DELETE /v1/browser/sessions/BS-OTHER", 404)
	if err := c.Revoke(ctx, fakeSession, "BS-OTHER"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke 404: %v", err)
	}
	set("DELETE /v1/browser/session", 503)
	if err := c.SignOut(ctx, fakeSession); err == nil || errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("sign out 503: %v", err)
	}

	// Unreachable is an error too, and names no URL.
	dead := &BrowserClient{BaseURL: "http://127.0.0.1:1", Timeout: time.Second}
	_, err = dead.Session(ctx, fakeSession)
	noLeak(err)
	if err == nil || errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("unreachable: %v", err)
	}
}

// TestLiveSendsTheBrowserSessionAndNeverABearerWithIt: a request is one
// principal. A feed with a browser session sends it on every read, and never
// an Authorization header next to it.
func TestLiveSendsTheBrowserSessionAndNeverABearerWithIt(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 2, events: seedEvents(3)}
	live, _ := newLive(t, b)
	live.BrowserSession = fakeSession // Token is also set by newLive

	var mu sync.Mutex
	var sessions []string
	inner := b.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sessions = append(sessions, r.Header.Get(BrowserSessionHeader))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	live.BaseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := live.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	ch, err := live.Stream(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch, 1)

	b.mu.Lock()
	defer b.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	// snapshot, contracts, decisions, the best-effort settled conflicts read
	// (which this fake answers 404) and the stream: five reads, one principal.
	if len(sessions) != 5 {
		t.Fatalf("requests = %d, want 5", len(sessions))
	}
	for i, got := range sessions {
		if got != fakeSession {
			t.Fatalf("request %d carried session %q", i, got)
		}
	}
	for i, got := range b.authSeen {
		if got != "" {
			t.Fatalf("request %d carried Authorization %q next to a browser session", i, got)
		}
		if sessions[i] != fakeSession {
			t.Fatalf("request %d carried session %q", i, sessions[i])
		}
	}
	if strings.Contains(live.Name(), fakeSession) {
		t.Fatal("Name() contains the session")
	}
}
