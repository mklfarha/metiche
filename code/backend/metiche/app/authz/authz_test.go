package authz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/browser"
	"github.com/mklfarha/metiche/backend/enums"
)

// These cover the parts of the decision that need no database: the wrapper's
// behaviour around a decision, and — the one that matters most — where a
// credential is allowed to come from.
//
// The database-backed cases live in session_mysql_test.go and
// identity_mysql_test.go here (the Guard itself), and in app/webapi's mysql
// test, which mounts both gated halves on one router against a real schema.

// recorder is a "next" handler that records whether it ran and what the gate
// put on its context.
type recorder struct {
	ran  bool
	team Team
	cred Credential
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.ran = true
	rec.team, _ = TeamFromContext(r.Context())
	rec.cred = CredentialFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "the board")
}

// serve mounts one gated route and issues req against it.
func serve(t *testing.T, a Authorizer, req *http.Request) (*httptest.ResponseRecorder, *recorder) {
	t.Helper()
	next := &recorder{}
	m := Middleware{
		Authorizer: a,
		NotFound: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no such team", http.StatusNotFound)
		},
	}
	r := chi.NewRouter()
	r.Get("/v1/teams/{slug}", m.Wrap(next.ServeHTTP))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, next
}

func TestGrantedRequestReachesTheHandler(t *testing.T) {
	allow := AuthorizerFunc(func(_ context.Context, teamRef string, _ Credential) (Team, error) {
		if teamRef != "demo" {
			t.Fatalf("the wrapper passed the wrong team ref: %q", teamRef)
		}
		return Team{Slug: teamRef, Public: true}, nil
	})

	w, next := serve(t, allow, httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil))
	if !next.ran {
		t.Fatal("an authorized request never reached the handler")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestGrantAndCredentialReachTheHandlerContext: /access reads the granted Team,
// and the stream re-runs the decision with the same Credential later.
func TestGrantAndCredentialReachTheHandlerContext(t *testing.T) {
	id := uuid.Must(uuid.NewV4())
	allow := AuthorizerFunc(func(_ context.Context, teamRef string, _ Credential) (Team, error) {
		return Team{UUID: id, Slug: teamRef, Role: enums.MEMBER_ROLE_OWNER}, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set(HeaderBrowserSession, "session-value")

	_, next := serve(t, allow, req)
	if next.team.UUID != id || next.team.Role != enums.MEMBER_ROLE_OWNER {
		t.Fatalf("TeamFromContext = %+v", next.team)
	}
	if next.cred.BrowserSession != "session-value" || next.cred.Bearer != "" {
		t.Fatal("CredentialFromContext did not carry the credential the request was authorized with")
	}
	if _, ok := TeamFromContext(context.Background()); ok {
		t.Fatal("TeamFromContext reported a grant outside a wrapped handler")
	}
}

// TestDeniedRequestNeverReachesTheHandler is the property the whole design
// rests on: the check runs BEFORE the handler does any work — no transaction
// opened, no row read, and for the stream no byte written.
func TestDeniedRequestNeverReachesTheHandler(t *testing.T) {
	w, next := serve(t, denyAll(), httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil))
	if next.ran {
		t.Fatal("a denied request reached the handler anyway")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if body := w.Body.String(); body == "the board" {
		t.Fatalf("the refusal carried the handler's output: %q", body)
	}
}

// TestAnInfrastructureFailureIs503NotARefusal: a decision that could not be
// made (the database is down) must not be rendered as "no such team". The
// board would tell a signed-in member their team does not exist and clear a
// good cookie (docs/BOARD_LOGIN.md §1, DB outage). It still fails CLOSED — the
// handler never runs — and it echoes nothing about the failure.
func TestAnInfrastructureFailureIs503NotARefusal(t *testing.T) {
	boom := AuthorizerFunc(func(context.Context, string, Credential) (Team, error) {
		return Team{}, errors.New("the database fell over at dsn user:hunter2@tcp")
	})

	w, next := serve(t, boom, httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil))
	if next.ran {
		t.Fatal("a request failed open when authorization errored")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — an outage is not a refusal", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "fell over") || strings.Contains(body, "hunter2") {
		t.Fatalf("the 503 echoed the failure: %q", body)
	}

	// A wrapped ErrDenied is still a refusal.
	wrapped := AuthorizerFunc(func(context.Context, string, Credential) (Team, error) {
		return Team{}, fmt.Errorf("context: %w", ErrDenied)
	})
	if w, _ := serve(t, wrapped, httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)); w.Code != http.StatusNotFound {
		t.Fatalf("a wrapped ErrDenied: status = %d, want 404", w.Code)
	}
}

// TestANilAuthorizerRefusesEverything: a wiring bug on a public route must
// close the door, not open it.
func TestANilAuthorizerRefusesEverything(t *testing.T) {
	w, next := serve(t, nil, httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil))
	if next.ran {
		t.Fatal("a nil authorizer let the request through")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestTheCredentialComesFromHeadersAndNeverFromTheURL is a standing guard
// against the shortcut this surface invites.
//
// A browser's EventSource cannot set headers, so the temptation on the SSE
// route is ?token= or ?session=. That would put a live credential in the access
// log (chi's request logger logs the query string), the Referer header, the
// browser history and every proxy in between. A cookie is refused too: the
// backend answers Access-Control-Allow-Origin: *, and the board relays the
// viewer's cookie as a header. The extractor reads the three headers only, and
// this test fails the moment somebody teaches it otherwise.
func TestTheCredentialComesFromHeadersAndNeverFromTheURL(t *testing.T) {
	const presented = "not-a-real-credential"

	var seen Credential
	capture := AuthorizerFunc(func(_ context.Context, _ string, cred Credential) (Team, error) {
		seen = cred
		return Team{}, ErrDenied
	})

	// In the query string, under every name anyone would reach for.
	for _, q := range []string{
		"token", "access_token", "auth", "bearer", "api_key",
		"session", "browser_session", "session_secret", "metiche_session", "__Host-metiche_session", "mbs", "s",
	} {
		seen = Credential{}
		req := httptest.NewRequest(http.MethodGet, "/v1/teams/demo?"+q+"="+presented, nil)
		if _, _ = serve(t, capture, req); seen != (Credential{}) {
			t.Fatalf("a credential was accepted from the query parameter %q", q)
		}
	}

	// In a cookie, under the board's own cookie names.
	for _, name := range []string{"__Host-metiche_session", "metiche_session", "session"} {
		seen = Credential{}
		req := httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
		req.AddCookie(&http.Cookie{Name: name, Value: presented})
		if _, _ = serve(t, capture, req); seen != (Credential{}) {
			t.Fatalf("a credential was accepted from the cookie %q", name)
		}
	}

	// In the Authorization header, which is the supported bearer form.
	seen = Credential{}
	req := httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set("Authorization", "Bearer "+presented)
	if _, _ = serve(t, capture, req); seen != (Credential{Bearer: presented}) {
		t.Fatal("the bearer token did not reach the decision as Bearer")
	}

	// The X-Metiche-Token fallback app/mcp's middleware also accepts.
	seen = Credential{}
	req = httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set("X-Metiche-Token", presented)
	if _, _ = serve(t, capture, req); seen != (Credential{Bearer: presented}) {
		t.Fatal("the X-Metiche-Token fallback did not reach the decision as Bearer")
	}

	// The browser session header, and nothing else, fills BrowserSession.
	seen = Credential{}
	req = httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set("X-Metiche-Browser-Session", presented)
	if _, _ = serve(t, capture, req); seen != (Credential{BrowserSession: presented}) {
		t.Fatal("the X-Metiche-Browser-Session header did not reach the decision as BrowserSession")
	}

	// Both at once are passed through as both; the Guard refuses the pair.
	seen = Credential{}
	req = httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set("Authorization", "Bearer "+presented)
	req.Header.Set("X-Metiche-Browser-Session", presented)
	if _, _ = serve(t, capture, req); seen != (Credential{Bearer: presented, BrowserSession: presented}) {
		t.Fatal("a request with both credentials lost one of them before the decision")
	}
}

// TestTheHeaderNameIsTheOneTheBrowserPackageUses: one name, two constants, and
// they must never drift, or the board's session silently stops authorizing.
func TestTheHeaderNameIsTheOneTheBrowserPackageUses(t *testing.T) {
	if HeaderBrowserSession != "X-Metiche-Browser-Session" {
		t.Fatalf("HeaderBrowserSession = %q", HeaderBrowserSession)
	}
	if HeaderBrowserSession != browser.HeaderSession {
		t.Fatalf("authz and app/browser disagree on the session header: %q vs %q",
			HeaderBrowserSession, browser.HeaderSession)
	}
}

// TestACredentialNeverPrintsItsValues: a Credential handed to a logger or a
// format verb by mistake must not become a leaked secret.
func TestACredentialNeverPrintsItsValues(t *testing.T) {
	c := Credential{Bearer: "mtk_secretbearer", BrowserSession: "mbs_secretsession"}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, c)
		if strings.Contains(out, "secretbearer") || strings.Contains(out, "secretsession") {
			t.Fatalf("%s printed a credential: %s", verb, out)
		}
	}
}

// TestAPublicTeamIsDecidedWithoutLookingAtTheCredential pins the short-circuit
// at the wrapper: a public board must never be refused for a stale or garbage
// credential someone happens to be carrying.
func TestAPublicTeamIsDecidedWithoutLookingAtTheCredential(t *testing.T) {
	allow := AuthorizerFunc(func(_ context.Context, teamRef string, cred Credential) (Team, error) {
		if cred.BrowserSession != "garbage" {
			t.Fatalf("the session did not reach the decision")
		}
		return Team{Slug: teamRef, Public: true}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set(HeaderBrowserSession, "garbage")
	w, next := serve(t, allow, req)
	if !next.ran || w.Code != http.StatusOK {
		t.Fatalf("a public team was refused: ran=%v status=%d", next.ran, w.Code)
	}
}

// TestAnEmptyTeamRefIsDenied: the guard must not treat a missing {slug} as a
// wildcard.
func TestAnEmptyTeamRefIsDenied(t *testing.T) {
	g := &Guard{} // no database is reached: the empty ref short-circuits first.
	if _, err := g.Authorize(context.Background(), "   ", Credential{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("an empty team ref returned %v, want ErrDenied", err)
	}
}

// TestOnePrincipalPerRequest: no credential, and a bearer and a session
// together, are both refused before any lookup. A request is one principal.
func TestOnePrincipalPerRequest(t *testing.T) {
	g := &Guard{} // no database: neither case may reach one.
	for name, cred := range map[string]Credential{
		"none":          {},
		"whitespace":    {Bearer: "  ", BrowserSession: "\t"},
		"both at once":  {Bearer: "mtk_x", BrowserSession: "mbs_y"},
		"both, spaced ": {Bearer: " mtk_x ", BrowserSession: " mbs_y "},
	} {
		if _, err := g.accountFor(context.Background(), cred); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s: accountFor returned %v, want ErrDenied", name, err)
		}
	}
}

func denyAll() Authorizer {
	return AuthorizerFunc(func(context.Context, string, Credential) (Team, error) {
		return Team{}, ErrDenied
	})
}
