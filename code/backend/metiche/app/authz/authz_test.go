package authz

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// These cover the parts of the decision that need no database: the wrapper's
// behaviour around a decision, and — the one that matters most — where a
// credential is allowed to come from.
//
// The database-backed end-to-end cases (a public team readable with no token,
// a private team 404ing for no token / a non-member token, 200 for a member)
// live in app/webapi's mysql test, which mounts both gated halves on one
// router against a real schema.

// recorder is a "next" handler that records whether it ran.
type recorder struct{ ran bool }

func (rec *recorder) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	rec.ran = true
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
	allow := AuthorizerFunc(func(_ context.Context, teamRef, _ string) (Team, error) {
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
	if w.Code == http.StatusForbidden {
		t.Fatal("403 confirms the team exists; the refusal must be 404")
	}
	if body := w.Body.String(); body == "the board" {
		t.Fatalf("the refusal carried the handler's output: %q", body)
	}
}

// TestAnInfrastructureFailureFailsClosed: a database that is unhappy must not
// answer 500 on a private team and 404 on an unknown one. The difference
// would be the oracle this design exists to remove, so both are 404.
func TestAnInfrastructureFailureFailsClosed(t *testing.T) {
	boom := AuthorizerFunc(func(context.Context, string, string) (Team, error) {
		return Team{}, errors.New("the database fell over")
	})

	w, next := serve(t, boom, httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil))
	if next.ran {
		t.Fatal("a request failed open when authorization errored")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — an error must not be distinguishable from a refusal", w.Code)
	}
	if body := w.Body.String(); body != "no such team\n" {
		t.Fatalf("the refusal echoed something about the failure: %q", body)
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

// TestTheTokenComesFromAHeaderAndNeverFromTheURL is a standing guard against
// the shortcut this surface invites.
//
// A browser's EventSource cannot set headers, so the temptation on the SSE
// route is ?token=. That would put a live credential in the access log, the
// Referer header, the browser history and every proxy in between. The
// extractor reads headers only, and this test fails the moment somebody
// teaches it otherwise.
func TestTheTokenComesFromAHeaderAndNeverFromTheURL(t *testing.T) {
	const presented = "not-a-real-credential"

	var seen string
	capture := AuthorizerFunc(func(_ context.Context, _, token string) (Team, error) {
		seen = token
		return Team{}, ErrDenied
	})

	// In the query string, under every name anyone would reach for.
	for _, q := range []string{"token", "access_token", "auth", "bearer", "api_key"} {
		seen = ""
		req := httptest.NewRequest(http.MethodGet, "/v1/teams/demo?"+q+"="+presented, nil)
		if _, _ = serve(t, capture, req); seen != "" {
			t.Fatalf("a credential was accepted from the query parameter %q: %q", q, seen)
		}
	}

	// In the Authorization header, which is the supported way.
	seen = ""
	req := httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set("Authorization", "Bearer "+presented)
	if _, _ = serve(t, capture, req); seen != presented {
		t.Fatalf("the bearer token did not reach the decision: %q", seen)
	}

	// And in the X-Metiche-Token fallback, the same one app/mcp's middleware
	// accepts for clients that cannot set Authorization. Same scheme, not a
	// second one.
	seen = ""
	req = httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set("X-Metiche-Token", presented)
	if _, _ = serve(t, capture, req); seen != presented {
		t.Fatalf("the X-Metiche-Token fallback did not reach the decision: %q", seen)
	}
}

// TestAPublicTeamIsDecidedWithoutLookingAtTheToken pins the short-circuit: a
// public board costs one query and must never be refused for a stale or
// malformed credential someone happens to be carrying.
func TestAPublicTeamIsDecidedWithoutLookingAtTheToken(t *testing.T) {
	allow := AuthorizerFunc(func(_ context.Context, teamRef, token string) (Team, error) {
		if token != "garbage" {
			t.Fatalf("token = %q", token)
		}
		return Team{Slug: teamRef, Public: true}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/teams/demo", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	w, next := serve(t, allow, req)
	if !next.ran || w.Code != http.StatusOK {
		t.Fatalf("a public team was refused: ran=%v status=%d", next.ran, w.Code)
	}
}

// TestAnEmptyTeamRefIsDenied: the guard must not treat a missing {slug} as a
// wildcard.
func TestAnEmptyTeamRefIsDenied(t *testing.T) {
	g := &Guard{} // no database is reached: the empty ref short-circuits first.
	if _, err := g.Authorize(context.Background(), "   ", ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("an empty team ref returned %v, want ErrDenied", err)
	}
}

func denyAll() Authorizer {
	return AuthorizerFunc(func(context.Context, string, string) (Team, error) {
		return Team{}, ErrDenied
	})
}
