package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/config"
	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/browser"
	"github.com/mklfarha/metiche/backend/app/webapi"
	"github.com/mklfarha/metiche/backend/core"
	restserver "github.com/mklfarha/metiche/backend/rest/server"
)

// These tests need no database. core.New only sql.Open()s, which does not
// connect; the address below is a closed local port, so any handler that does
// reach the database fails fast instead of hanging. There is no user or
// password in it because nothing ever authenticates.

const denyMarker = "X-Metiche-Test-Denied"

var allMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// markDenials swaps the production deny handler for one that also sets a
// header, so a 404 from the deny layer can be told apart from a 404 written by
// a hand-written handler (app/authz answers 404 on purpose).
func markDenials(t *testing.T) {
	t.Helper()
	prev := denyNotFound
	denyNotFound = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(denyMarker, "1")
		prev(w, r)
	}
	t.Cleanup(func() { denyNotFound = prev })
}

func testProvider(t *testing.T) config.Provider {
	t.Helper()
	p, err := config.NewStaticProvider(map[string]any{
		"ports": map[string]any{"http": "0"},
		"db": []map[string]any{{
			"name": "deny_test_no_database", "host": "127.0.0.1", "port": "1",
			"driver": "mysql", "params": "timeout=200ms",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// buildServer builds the REST server exactly as main.go's fx graph does —
// restserver.New with core.New and (optionally) app.ProvideCustomRoutes —
// without starting its listener.
func buildServer(t *testing.T, withCustom bool) chi.Router {
	t.Helper()
	t.Setenv("METICHE_ROLE", "all")
	provider := testProvider(t)
	logger := zap.NewNop()
	impl, err := core.New(core.Params{Provider: provider, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	params := restserver.Params{
		Lifecycle: fxtest.NewLifecycle(t),
		Config:    provider,
		Core:      impl,
		Logger:    logger,
	}
	if withCustom {
		params.CustomRoutes = ProvideCustomRoutes(impl, logger)
	}
	srv := restserver.New(params)
	r, ok := srv.Handler.(chi.Router)
	if !ok {
		t.Fatalf("server handler is %T, not a chi.Router", srv.Handler)
	}
	return r
}

type route struct{ method, pattern string }

func walk(t *testing.T, r chi.Router) []route {
	t.Helper()
	var out []route
	err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, route{method, pattern})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pattern != out[j].pattern {
			return out[i].pattern < out[j].pattern
		}
		return out[i].method < out[j].method
	})
	return out
}

var paramRE = regexp.MustCompile(`\{[^}]+\}`)

// concrete turns a chi pattern into request paths: params become a uuid, a
// trailing wildcard becomes a segment, and both the with- and without-slash
// spellings are produced because a chi mount answers both.
func concrete(pattern string) []string {
	p := paramRE.ReplaceAllString(pattern, "0b6b2f2e-8f6e-4d7a-9a51-2b6f0d1c9e11")
	p = strings.ReplaceAll(p, "*", "x")
	if p == "/" {
		return []string{"/"}
	}
	trimmed := strings.TrimSuffix(p, "/")
	return []string{trimmed, trimmed + "/"}
}

// allowedMatcher answers "would a hand-written, allowlisted route serve this
// method+path?" by routing it through a router holding only those routes.
func allowedMatcher(real []route) *chi.Mux {
	m := chi.NewRouter()
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, rt := range real {
		if _, ok := AllowedRoutes[rt.pattern]; ok {
			m.Method(rt.method, rt.pattern, noop)
		}
	}
	return m
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func denied(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusNotFound && rec.Header().Get(denyMarker) == "1"
}

// TestGeneratedCRUDIsDenied walks the generated server (no custom routes) to
// enumerate every route codegen registers, then walks the real server, and
// asserts every non-allowlisted route answers 404 from the deny layer for
// every method.
func TestGeneratedCRUDIsDenied(t *testing.T) {
	markDenials(t)

	generated := walk(t, buildServer(t, false))
	realRouter := buildServer(t, true)
	real := walk(t, realRouter)

	genPatterns := map[string]bool{}
	for _, rt := range generated {
		genPatterns[rt.pattern] = true
	}
	t.Logf("generated server registers %d (method, pattern) routes over %d patterns", len(generated), len(genPatterns))
	for _, must := range []string{"/v1/accounts/", "/v1/accounts/{id}/", "/v1/invites/", "/v1/invites/{id}/", "/v1/sessions/", "/v1/teams/{id}/", "/healthz", "/"} {
		if !genPatterns[must] {
			t.Fatalf("walk of the generated server did not find %q; the enumeration is broken, not the deny layer", must)
		}
	}

	matcher := allowedMatcher(real)
	all := map[string]bool{}
	for _, rt := range append(generated, real...) {
		all[rt.pattern] = true
	}
	patterns := make([]string, 0, len(all))
	for p := range all {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)

	var exposed []string
	checked, shadowed := 0, 0
	for _, p := range patterns {
		if _, ok := AllowedRoutes[p]; ok {
			continue
		}
		for _, path := range concrete(p) {
			for _, method := range allMethods {
				if matcher.Match(chi.NewRouteContext(), method, path) {
					// A hand-written allowlisted handler owns this exact
					// request (e.g. GET /v1/teams/{uuid} is the board
					// snapshot, which accepts a uuid). Not generated CRUD.
					shadowed++
					t.Logf("served by allowlisted route, not generated: %s %s", method, path)
					continue
				}
				checked++
				rec := do(realRouter, method, path)
				if !denied(rec) {
					exposed = append(exposed, method+" "+path+" -> "+http.StatusText(rec.Code))
				}
			}
		}
	}
	t.Logf("checked %d non-allowlisted requests (%d patterns x paths x methods), %d owned by allowlisted routes", checked, len(patterns), shadowed)
	if len(exposed) > 0 {
		t.Fatalf("%d requests were NOT denied with 404:\n  %s", len(exposed), strings.Join(exposed, "\n  "))
	}
}

// TestAllowlistedRoutesStillReachable asserts every allowlisted route that the
// real server registers is served by its own handler, not by the deny layer.
func TestAllowlistedRoutesStillReachable(t *testing.T) {
	markDenials(t)
	r := buildServer(t, true)

	seen := map[string]bool{}
	for _, rt := range walk(t, r) {
		if _, ok := AllowedRoutes[rt.pattern]; !ok {
			continue
		}
		// The MCP handler is registered for every method; one GET and one
		// POST prove it is reachable without flooding the log.
		if rt.pattern == "/v1/mcp" || rt.pattern == "/v1/mcp/*" {
			if rt.method != http.MethodGet && rt.method != http.MethodPost {
				continue
			}
		}
		seen[rt.pattern] = true
		path := concrete(rt.pattern)[0]
		rec := do(r, rt.method, path)
		if denied(rec) {
			t.Errorf("allowlisted %s %s was denied by the deny layer", rt.method, rt.pattern)
			continue
		}
		t.Logf("reachable: %-6s %-36s -> %d (own handler)", rt.method, rt.pattern, rec.Code)
	}
	for p := range AllowedRoutes {
		if !seen[p] {
			t.Errorf("allowlisted pattern %q is not registered by the real server (role all); stale allowlist entry?", p)
		}
	}
	if rec := do(r, http.MethodGet, "/v1/metrics/mcp"); rec.Code != http.StatusOK {
		t.Errorf("GET /v1/metrics/mcp = %d, want 200", rec.Code)
	}
	if rec := do(r, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

// TestDeniedLooksLikeUnknownPath: the production deny response is identical to
// a path that never existed, so it confirms nothing.
func TestDeniedLooksLikeUnknownPath(t *testing.T) {
	r := buildServer(t, true)
	unknown := do(r, http.MethodGet, "/no-such-thing-at-all")
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/accounts"},
		{http.MethodPost, "/v1/invites"},
		{http.MethodDelete, "/v1/sessions/0b6b2f2e-8f6e-4d7a-9a51-2b6f0d1c9e11"},
		{http.MethodPatch, "/v1/teams/0b6b2f2e-8f6e-4d7a-9a51-2b6f0d1c9e11"},
		{http.MethodPost, "/v1/metrics/mcp"},
		{http.MethodGet, "/v1/openapi.yaml"},
		{http.MethodGet, "/"},
	} {
		rec := do(r, c.method, c.path)
		body, _ := io.ReadAll(rec.Body)
		ubody := unknown.Body.String()
		if rec.Code != http.StatusNotFound || string(body) != ubody ||
			rec.Header().Get("Content-Type") != unknown.Header().Get("Content-Type") {
			t.Errorf("%s %s = %d %q (%s); want exactly the unknown-path response 404 %q (%s)",
				c.method, c.path, rec.Code, body, rec.Header().Get("Content-Type"), ubody, unknown.Header().Get("Content-Type"))
		}
	}
}

// TestFutureCodegenIsDeniedByDefault simulates a codegen run that adds a new
// entity under /v1, a new /v2 mount and a new root route, plus a hand-written
// route someone forgot to allowlist. None of them is in any list.
func TestFutureCodegenIsDeniedByDefault(t *testing.T) {
	markDenials(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "LEAK") })

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Route("/future-widgets", func(r chi.Router) {
			r.Get("/", ok)
			r.Post("/", ok)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", ok)
				r.Put("/", ok)
				r.Patch("/", ok)
				r.Delete("/", ok)
			})
		})
	})
	r.Route("/v2", func(r chi.Router) { r.Get("/things", ok) })
	r.Get("/swagger", ok)
	r.Get("/healthz", ok)
	// hand-written, allowlisted vs not
	r.Get("/v1/metrics/mcp", ok)
	r.Get("/v1/forgot-to-allowlist", ok)

	DenyUnlisted(r, zap.NewNop())

	for _, path := range []string{
		"/v1", "/v1/", "/v1/future-widgets", "/v1/future-widgets/",
		"/v1/future-widgets/abc", "/v1/future-widgets/abc/",
		"/v2", "/v2/things", "/swagger", "/v1/forgot-to-allowlist",
		"/v1/entity-codegen-will-add-next-year", "/totally/unknown",
	} {
		for _, method := range allMethods {
			rec := do(r, method, path)
			if !denied(rec) || strings.Contains(rec.Body.String(), "LEAK") {
				t.Errorf("%s %s = %d %q; want 404 from the deny layer", method, path, rec.Code, rec.Body.String())
			}
		}
	}
	for _, path := range []string{"/healthz", "/v1/metrics/mcp"} {
		if rec := do(r, http.MethodGet, path); rec.Code != http.StatusOK || rec.Body.String() != "LEAK" {
			t.Errorf("GET %s = %d; allowlisted route must stay reachable", path, rec.Code)
		}
	}

	// And against the real server: an entity path that does not exist yet.
	real := buildServer(t, true)
	for _, path := range []string{"/v1/future-widgets", "/v1/future-widgets/0b6b2f2e-8f6e-4d7a-9a51-2b6f0d1c9e11"} {
		for _, method := range allMethods {
			if rec := do(real, method, path); !denied(rec) {
				t.Errorf("real server: %s %s = %d; want 404 from the deny layer", method, path, rec.Code)
			}
		}
	}
}

// boardOnlyRoutes are the in-cluster board routes added for sign-in
// (docs/BOARD_LOGIN.md §6.1), with the only methods each may answer.
var boardOnlyRoutes = map[string][]string{
	webapi.PathAccess:      {http.MethodGet},
	browser.PathSessions:   {http.MethodPost, http.MethodGet, http.MethodDelete},
	browser.PathSession:    {http.MethodGet, http.MethodDelete},
	browser.PathSessionKey: {http.MethodDelete},
	browser.PathTeams:      {http.MethodGet},
}

// TestBrowserRoutesAnswerOnlyRegisteredMethods: each new board route is on the
// allowlist, is served by its own handler for exactly its registered methods,
// and every other method is the deny layer's 404.
func TestBrowserRoutesAnswerOnlyRegisteredMethods(t *testing.T) {
	markDenials(t)
	r := buildServer(t, true)

	registered := map[string]map[string]bool{}
	for _, rt := range walk(t, r) {
		if _, ok := boardOnlyRoutes[rt.pattern]; ok {
			if registered[rt.pattern] == nil {
				registered[rt.pattern] = map[string]bool{}
			}
			registered[rt.pattern][rt.method] = true
		}
	}
	for pattern, methods := range boardOnlyRoutes {
		if _, ok := AllowedRoutes[pattern]; !ok {
			t.Errorf("%s is not in AllowedRoutes", pattern)
		}
		want := map[string]bool{}
		for _, m := range methods {
			want[m] = true
			if !registered[pattern][m] {
				t.Errorf("%s %s is not registered", m, pattern)
			}
		}
		for m := range registered[pattern] {
			if !want[m] {
				t.Errorf("%s %s is registered but not expected", m, pattern)
			}
		}
		for _, path := range concrete(pattern) {
			for _, method := range allMethods {
				rec := do(r, method, path)
				switch {
				case want[method] && path == concrete(pattern)[0] && denied(rec):
					t.Errorf("%s %s was denied; its own handler must answer", method, path)
				case !want[method] && !denied(rec):
					t.Errorf("%s %s = %d, not the deny layer's 404", method, path, rec.Code)
				}
				if want[method] && path == concrete(pattern)[0] {
					t.Logf("own handler: %-6s %-40s -> %d", method, path, rec.Code)
				}
			}
		}
	}
	// No credential, no database behind this server: the handlers refuse
	// before anything else, with their own opaque answers.
	if rec := do(r, http.MethodGet, browser.PathSession); rec.Code != http.StatusUnauthorized || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("GET %s without a session = %d (%q), want 401 no-store", browser.PathSession, rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := do(r, http.MethodPost, browser.PathSessions); rec.Code != http.StatusNotFound || denied(rec) {
		t.Errorf("POST %s with no link = %d (denied=%v), want the handler's own 404", browser.PathSessions, rec.Code, denied(rec))
	}
}

// ingressPathRE finds `- path: X` entries; the next pathType line belongs to it.
var ingressPathRE = regexp.MustCompile(`^\s*-\s*path:\s*"?([^"\s]+)"?\s*$`)
var ingressTypeRE = regexp.MustCompile(`^\s*pathType:\s*"?(\w+)"?\s*$`)

// TestBrowserRoutesAreNotRoutedByAnyIngress: being on AllowedRoutes is safe
// only because no ingress routes these paths (§6.1, F7). This reads the
// backend chart's values files and asserts no listed ingress path (Exact, or
// Prefix matched per path element as networking.k8s.io/v1 does) reaches a
// board-only route. The deploy agent's `helm template` check is the rendered
// proof; this one fails in `go test` the moment someone adds such a path.
func TestBrowserRoutesAreNotRoutedByAnyIngress(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "..", "..", "deploy", ".helm", "metiche", "values*.yaml"))
	if len(files) == 0 {
		t.Skip("deploy/.helm/metiche not present in this checkout")
	}
	type ingressPath struct{ file, path, kind string }
	var paths []ingressPath
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			m := ingressPathRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			kind := ""
			if i+1 < len(lines) {
				if tm := ingressTypeRE.FindStringSubmatch(lines[i+1]); tm != nil {
					kind = tm[1]
				}
			}
			paths = append(paths, ingressPath{filepath.Base(f), m[1], kind})
		}
	}
	if len(paths) == 0 {
		t.Fatal("found no ingress paths in the chart values; the scan is broken, not the chart")
	}
	routes := func(ip ingressPath, reqPath string) bool {
		if ip.kind == "Exact" {
			return reqPath == ip.path
		}
		base := strings.TrimSuffix(ip.path, "/")
		return base == "" || reqPath == base || strings.HasPrefix(reqPath, base+"/")
	}
	for _, ip := range paths {
		t.Logf("ingress path in %s: %s (%s)", ip.file, ip.path, ip.kind)
		for pattern := range boardOnlyRoutes {
			for _, p := range concrete(pattern) {
				if routes(ip, p) {
					t.Errorf("%s: ingress path %s (%s) routes board-only %s", ip.file, ip.path, ip.kind, p)
				}
			}
		}
	}
}
