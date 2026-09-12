package app

import (
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
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
func ProvideCustomRoutes(coreImpl *core.Implementation, logger *zap.Logger) restserver.CustomRoutesFn {
	return func(r chi.Router) {
		// Returns the handler so a later wave can install the detection layer
		// with SetDetector — it must run inside the team lock, which means it
		// has to be registered on this instance rather than called around it.
		_ = metichemcp.Register(r, coreImpl, logger)

		// SEAM — the board's own endpoints (app/webapi) and the SSE stream
		// (app/stream) mount here too, on this same root router, with full
		// paths including the /v1 prefix. Two chi rules apply to them exactly
		// as they do above: never r.Route("/v1", ...) — the generated CRUD
		// already owns that mount point and re-mounting panics while the
		// router builds — and never r.Use here, because middleware cannot be
		// added to a mux that already has routes. Wrap the handler itself, or
		// scope it with r.Group.
	}
}
