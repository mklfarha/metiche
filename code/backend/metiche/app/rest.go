package app

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/browser"
	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/app/stream"
	"github.com/mklfarha/metiche/backend/app/webapi"
	"github.com/mklfarha/metiche/backend/core"
	restserver "github.com/mklfarha/metiche/backend/rest/server"
)

// ProvideCustomRoutes is wired in main.go; it hands your custom REST routes to
// the generated REST server, which mounts them after the generated CRUD routes
// (so you can add new endpoints or override generated ones on the same path).
//
// Add your routes below — this file is generated ONCE and will not be
// overwritten. Adding a REST endpoint needs no code generation.
//
// THE CONTRACT — `r` is the server's ROOT chi router, and you get it AFTER the
// generated routes are mounted and AFTER the generated middleware (request id,
// recoverer, logger, CORS) is installed. chi forbids two things on a
// mux that already has routes, and it enforces both with a PANIC raised while
// the router is being built — so the offending code compiles, ships, and then
// crash-loops the container on start.
//
// First: spell the FULL path, including the /v1 prefix. Do not re-mount it.
//
//	r.Get("/v1/custom/ping", h)     // correct
//	r.Get("/custom/ping", h)          // compiles, but registers at the ROOT
//	r.Route("/v1", func(sub chi.Router) { ... })
//	// PANIC: attempting to Mount() a handler on an existing path, '/v1'
//	// — the generated CRUD routes already occupy it
//
// Second: scope middleware with r.Group, never r.Use.
//
//	r.Use(mw)
//	// PANIC: all middlewares must be defined before routes on a mux
//
//	r.Group(func(g chi.Router) {      // correct
//		g.Use(mw)
//		g.Get("/v1/custom/ping", h)
//	})
//
// What you get for free: the generated middleware chain already applies to your
// routes, and a
// custom route on a generated path takes precedence over the generated handler
// (chi matches a static route before a mount).
//
// Response helpers: restserver.ListEnvelope[T] is exported and is the shape the
// generated list endpoints return. The rest — writeJSON, writeProblem,
// parseListParams, <entity>Declarations — are unexported, so a custom handler
// writes its own JSON. Match the generated error shape (RFC 7807 problem+json:
// type/title/status/detail) if you want one error format across the API.
//
// # WHAT METICHE MOUNTS HERE
//
// app/mcp.Register installs the agent-facing surface, gated on METICHE_ROLE:
//
//	/v1/mcp, /v1/mcp/*   the MCP endpoint over streamable HTTP (roles: mcp, all)
//	/v1/metrics/mcp      the team-lock-hold histogram, timings only (roles: api, all)
//
// The MCP server runs IN THIS PROCESS rather than as its own service for one
// load-bearing reason: a mutating tool has to append a team_event and advance
// team.sequence inside a single transaction that holds the team row lock, and
// only in-process access to core.Implementation gives us that transaction.
//
// METICHE_ROLE is "all" for v1 — one pod serving both halves — so the
// in-process fan-out on commit covers every write. The api/mcp split exists
// for when agent traffic needs isolating from the board's, and not before. An
// unset or unrecognised value serves everything.
//
// # THE GENERATED CRUD IS NEVER SERVED (DenyUnlisted)
//
// The generated server mounts unauthenticated create/read/update/delete for
// EVERY entity under /v1 (accounts with token hashes, invites with join codes,
// sessions, ...). None of it is behind app/authz, and in production it was
// reachable from the internet. So the last thing this function does is
// DenyUnlisted, which turns the router into default-deny:
//
//  1. Every route registered on the ROOT router whose pattern is not in
//     AllowedRoutes is re-registered, for all methods, with a plain 404. That
//     includes the generated mount itself: r.Route("/v1", ...) puts ONE
//     catch-all node "/v1/*" (plus its "/v1" and "/v1/" stubs) in the root
//     tree, and every generated entity route is reachable only through it.
//     Re-registering a pattern on a chi mux replaces its handlers in place —
//     it is not a Mount, so it does not panic — and the subrouter behind it
//     becomes unreachable as a whole.
//  2. "/*" at the root, NotFound and MethodNotAllowed all answer the same 404,
//     so an unknown path, a denied generated path and a wrong method on an
//     allowed path are byte-for-byte indistinguishable. 404, never 401/403/405:
//     nothing here confirms that an API exists.
//
// Why this cannot silently reopen when codegen adds an entity: nothing is
// enumerated from the generated code. A new entity lives under /v1/*, which is
// denied as a unit; a new top-level generated route or mount appears in
// r.Routes() and is not in AllowedRoutes, so it is denied too. The allowlist is
// the list of hand-written routes that MAY answer, and it is exhaustive for
// hand-written code as well: a route added in app/mcp, app/webapi or app/stream
// without a matching AllowedRoutes entry is denied and logged at startup, so
// exposure is always an edit to this file, in review.
//
// Allowed routes still get a 404 for methods they do not register (e.g.
// POST /v1/teams/{slug}): chi falls through to "/v1/*", which is denied. That
// fall-through is exactly how PATCH/DELETE /v1/teams/{uuid} used to reach the
// generated handlers, even though GET was shadowed by the board route.
//
// app/rest_deny_test.go builds the real server, walks it with chi.Walk and
// asserts every generated route answers 404 for every method.
func ProvideCustomRoutes(coreImpl *core.Implementation, logger *zap.Logger) restserver.CustomRoutesFn {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(r chi.Router) {
		handler := metichemcp.Register(r, coreImpl, logger)

		// THE DETECTOR IS INSTALLED, NOT CALLED. NewPathDetector returns a
		// hook that commit() runs from inside the transaction that already
		// holds the team row lock. Registering it on the handler is the only
		// way to get it there: check overlap around commit() instead and two
		// agents both see a clean world and both insert, which is precisely
		// the race the lock exists to remove.
		//
		// Without this line every tool still works and no conflict is ever
		// found — a silent, green, useless build. It has no test of its own
		// because the thing it would assert is "the wiring is wired"; what
		// proves it is the two-session integration test in app/mcp, which
		// fails outright if detection is absent.
		// The decision reviewer runs second, on the same transaction: it
		// pairs a declared or updated plan with the recorded decisions it
		// touches and writes the inline review block (docs/DECISIONS.md §3.2).
		// The duplicate reviewer runs before it and takes the shared review
		// cap first; the renderer writes both reviewers' pairs into one block,
		// last (docs/DUPLICATES.md §3.1).
		handler.SetDetector(metichemcp.ChainDetectors(
			metichemcp.NewPathDetector(coreImpl, logger),
			metichemcp.NewDuplicateReviewer(coreImpl, logger),
			metichemcp.NewDecisionReviewer(coreImpl, logger),
			metichemcp.NewReviewRenderer(),
		))

		// The board's half of the surface. Same role gate as the MCP
		// endpoint above: these are what the frontend reads, so they belong
		// to the api role, and an mcp-only pod does not serve them.
		//
		// Both take the root router and spell full /v1 paths themselves, for
		// the two chi reasons in the contract above.
		if metichemcp.RoleFromEnv(logger).ServesAPI() {
			webapi.Register(r, coreImpl, logger) // GET /v1/teams/{slug}, /conflicts, /contracts, /decisions, /sessions, /sessions/{key}
			stream.Register(r, coreImpl, logger) // GET /v1/teams/{slug}/stream (SSE)
			// Board sign-in (docs/BOARD_LOGIN.md §2.4, §2.7, §2.8): the
			// exchange, the session check, sign out and the "your teams"
			// list. In-cluster only, like the two above; see AllowedRoutes.
			browser.Register(r, coreImpl, logger) // /v1/browser/sessions, /session, /sessions/{key}, /teams
			// Invite management for a signed-in member (docs/BOARD_LOGIN.md
			// §10.10). It is handed the MCP endpoint's own Handler, so the
			// rules are create_invite's, and the per-account create budget is
			// ONE budget whichever surface spends it. In-cluster only.
			webapi.RegisterInvites(r, coreImpl, handler, logger) // /v1/teams/{slug}/invites, /invites/{invite_id}
		}

		// MUST STAY LAST: it only allows what is already registered, and it
		// denies everything else that is registered. See the doc comment.
		DenyUnlisted(r, logger)
	}
}

// AllowedRoutes is the complete set of ROOT-router patterns that may answer a
// request. Anything else registered on the router answers 404. Patterns must
// match the registration exactly (chi pattern syntax, including param names).
//
// Whether a pattern is actually served still depends on METICHE_ROLE; being
// listed here only means it is not denied.
var AllowedRoutes = map[string]string{
	// Liveness/readiness/startup probes in deploy/.helm/metiche. Generated,
	// static, touches nothing.
	"/healthz": "generated health probe",

	// app/mcp.Register — role mcp.
	"/v1/mcp":   "MCP streamable HTTP",
	"/v1/mcp/*": "MCP streamable HTTP",

	// app/mcp.Register — role api. Timings only.
	"/v1/metrics/mcp": "team-lock-hold histogram",

	// app/webapi.RegisterOn — role api, every route behind app/authz.
	webapi.PathSnapshot:  "board snapshot",
	webapi.PathConflicts: "board conflicts",
	webapi.PathContracts: "board contracts",
	webapi.PathDecisions: "board decisions",
	webapi.PathSession:   "board session",
	// The run history (GET). Same guard as the four above, and like them not
	// routed by any ingress: TestBrowserRoutesAreNotRoutedByAnyIngress pins it.
	webapi.PathSessions: "board run history",
	// The rest of the board's history (GET): past conflicts, the event log
	// read backwards, and a past window of the graph. Same guard, same
	// footing: not routed by any ingress, pinned by the same test.
	webapi.PathConflictHistory: "board conflict history",
	webapi.PathEvents:          "board event history",
	webapi.PathGraph:           "board graph over a past window",
	// Past decisions: the superseded and revoked ones GET /decisions no
	// longer returns, and one decision's earlier wordings from the event log
	// (docs/DECISIONS.md §5.1). Same guard and same footing as the three
	// above.
	webapi.PathDecisionHistory: "board decision history",

	// Board sign-in (docs/BOARD_LOGIN.md §6.1). BOARD ONLY; NOT ROUTED BY ANY
	// INGRESS. They must be listed here or this layer would 404 the board's
	// own in-cluster calls; being listed does not make them public, because
	// exposure is decided by the ingress, and deploy/.helm/metiche routes
	// only /v1/metrics/mcp (Exact) and /v1/mcp (Prefix) —
	// TestBrowserRoutesAreNotRoutedByAnyIngress in rest_deny_test.go pins
	// that. They are also safe if that ever regressed: every one of them
	// needs a 256-bit secret (a link in the POST body, or a session in
	// X-Metiche-Browser-Session), refuses with one opaque body, and never
	// reads a credential from the URL. Only the methods each registers
	// answer; every other method falls through to the deny layer.
	webapi.PathAccess:      "board: may this viewer read this team (GET)",
	browser.PathSessions:   "board: sign-in exchange (POST), session list (GET), sign out everywhere (DELETE)",
	browser.PathSession:    "board: current browser session (GET), sign out (DELETE)",
	browser.PathSessionKey: "board: revoke one browser session of the caller's account (DELETE)",
	browser.PathTeams:      "board: the signed-in viewer's teams (GET)",
	// Board invites (§10.10). Same footing as the four above: board only, not
	// routed by any ingress, a browser session in X-Metiche-Browser-Session
	// and never a bearer, and every refusal the same 404 as an unknown team.
	webapi.PathInvites: "board: a signed-in member's invites (GET list, POST create)",
	webapi.PathInvite:  "board: revoke one invite (DELETE)",

	// app/stream.Register — role api, behind app/authz.
	stream.StreamPath: "board SSE stream",
}

// denyNotFound is the single response for everything that is not allowed.
// http.NotFound is also chi's default for an unknown path, so a denied route
// looks exactly like a path that was never registered.
//
// A variable only so the tests can tell "denied by this layer" apart from a
// hand-written handler that legitimately answers 404 (app/authz does, for a
// team you cannot see). Nothing outside tests assigns it.
var denyNotFound http.HandlerFunc = http.NotFound

// DenyUnlisted makes r default-deny. It must run after every route that may be
// allowed has been registered. It returns the patterns it denied, for tests
// and for the startup log.
func DenyUnlisted(r chi.Router, logger *zap.Logger) []string {
	if logger == nil {
		logger = zap.NewNop()
	}
	deny := http.HandlerFunc(denyNotFound)

	var denied []string
	denyPattern := func(p string) {
		r.Handle(p, deny)
		denied = append(denied, p)
	}

	for _, rt := range r.Routes() {
		if _, ok := AllowedRoutes[rt.Pattern]; ok {
			continue
		}
		if rt.SubRoutes == nil && !isGeneratedLooking(rt.Pattern) {
			// A root-level route that is neither generated-shaped nor on the
			// allowlist: almost certainly a hand-written route someone forgot
			// to list. Deny it anyway, loudly.
			logger.Error("route is not in app.AllowedRoutes and is DENIED; add it there if it must be public",
				zap.String("pattern", rt.Pattern))
		}
		denyPattern(rt.Pattern)
		// A Mount registers "/x", "/x/" and "/x/*"; r.Routes() reports only
		// the catch-all. Deny the two stubs as well, or "/v1" and "/v1/"
		// would still hand the request to the generated subrouter.
		if base, ok := strings.CutSuffix(rt.Pattern, "/*"); ok && base != "" {
			if _, allowed := AllowedRoutes[base]; !allowed {
				denyPattern(base)
			}
			if _, allowed := AllowedRoutes[base+"/"]; !allowed {
				denyPattern(base + "/")
			}
		}
	}

	// Anything with no node at all, and any method an allowed node does not
	// register, gets the same 404.
	if _, ok := AllowedRoutes["/*"]; !ok {
		denyPattern("/*")
	}
	r.NotFound(deny)
	r.MethodNotAllowed(deny)

	logger.Info("default-deny installed on the REST router",
		zap.Int("denied_patterns", len(denied)),
		zap.Int("allowed_patterns", len(AllowedRoutes)))
	return denied
}

// isGeneratedLooking reports whether a root pattern is one the generated
// server is known to register (its /v1 mount and its root handlers), used only
// to decide whether a denial deserves an error log.
func isGeneratedLooking(p string) bool {
	return p == "/" || p == "/*" || p == "/v1" || p == "/v1/" || strings.HasPrefix(p, "/v1/*")
}
