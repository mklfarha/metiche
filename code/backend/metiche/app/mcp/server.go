package mcp

import (
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/core"
)

func boolPtr(b bool) *bool { return &b }

// Role selects which half of the surface this process serves, so the agent
// endpoint and the human-facing API can later run as two deployments from ONE
// image.
//
// The point is blast-radius isolation: agents are unpredictable callers, and a
// burst of MCP traffic must not slow the board the humans are watching.
//
// v1 runs METICHE_ROLE=all in one pod, deliberately. PLAN.md wants the
// in-process fan-out on commit to cover 100% of writes, which it only does
// while both halves share a process. The split exists for when agent load
// needs isolating, and not before.
type Role string

const (
	RoleAll Role = "all"
	RoleAPI Role = "api"
	RoleMCP Role = "mcp"
)

// ParseRole is the whole of the role decision, factored out so it can be
// tested without an environment.
//
// An unset or unrecognised value serves everything, which is the safe default
// in the only direction that matters: a misconfigured pod that serves too much
// is a performance problem, one that serves too little is an outage.
func ParseRole(v string) (Role, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(RoleAPI):
		return RoleAPI, true
	case string(RoleMCP):
		return RoleMCP, true
	case "", string(RoleAll):
		return RoleAll, true
	default:
		return RoleAll, false
	}
}

// ServesMCP reports whether this role answers the agent endpoint.
func (r Role) ServesMCP() bool { return r == RoleMCP || r == RoleAll }

// ServesAPI reports whether this role answers the board's endpoints.
func (r Role) ServesAPI() bool { return r == RoleAPI || r == RoleAll }

// RoleFromEnv reads METICHE_ROLE.
func RoleFromEnv(logger *zap.Logger) Role {
	raw := os.Getenv("METICHE_ROLE")
	role, ok := ParseRole(raw)
	if !ok && logger != nil {
		logger.Warn("METICHE_ROLE is not one of api, mcp, all; serving everything",
			zap.String("value", raw))
	}
	return role
}

// Register mounts this process's share of the surface onto the generated
// server's router.
//
// Returns the Handler so the wiring in app/rest.go can install the detection
// layer (SetDetector) and so later waves can reach the same instance.
func Register(r chi.Router, coreImpl *core.Implementation, logger *zap.Logger) *Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	role := RoleFromEnv(logger)
	handler := NewHandler(coreImpl, logger)

	if role.ServesMCP() {
		streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			return newServer(handler, logger)
		}, nil)
		// authMiddleware wraps the handler directly rather than being
		// installed with r.Use: chi panics if middleware is added to a mux
		// that already has routes, and the generated CRUD routes are mounted
		// before this runs.
		authed := handler.authMiddleware(streamable)

		// Full paths including /v1 — the generated CRUD already owns that
		// mount point, and r.Route("/v1", ...) here would panic while the
		// router is being built.
		r.Handle("/v1/mcp", authed)
		r.Handle("/v1/mcp/*", authed)
	}

	if role.ServesAPI() {
		// Timings only: no team, session or agent identifier, so this is safe
		// on a public endpoint. Outside /v1/mcp so it cannot shadow a
		// protocol path.
		r.Get("/v1/metrics/mcp", handler.metricsHandler)
	}

	logger.Info("metiche custom routes mounted", zap.String("role", string(role)))
	return handler
}

// serverInstructions is what an MCP client shows its model about this server
// as a whole. It is the only place to say the things that are true of every
// tool at once.
func serverInstructions() string {
	return strings.TrimSpace(`
metiche is your team's shared awareness layer. Other people's agents are editing the same
repository right now, and nobody can see anyone else's work until it lands in git. You fix that by
saying what you are ABOUT TO DO before you do it.

The loop:
  1. join_team once, with the team's join code. Save the token it returns; it is shown once.
  2. start_session once, when you begin a piece of work.
  3. heartbeat about every 60 seconds while you work. Claims lapse without it.
  4. end_session when you are done, so your holds are released immediately.

Every response carries "pending": counts of instructions, conflicts and reviews waiting for you.
MCP cannot push, so that count is how you find out anything. When it is non-zero, fetch the
contents; when it is zero, carry on.

Two cursors ride on every response. "sequence" advances on every event on the team - if it jumped
by more than you expected, you missed something. "revision" advances only when the board's shape
changed.

Responses are small on purpose. metiche will never hand you the whole board or everybody's claims;
ask for what you need with get_team_state.

Retries are safe: pass the same idempotency_key and you get the same answer back, applied once.
`)
}

// newServer builds the tool surface.
func newServer(h *Handler, logger *zap.Logger) *mcp.Server {
	registered = nil

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "metiche",
		Version: ProtocolVersion,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions(),
	})

	// Annotations are not decoration. MCP's destructiveHint DEFAULTS TO TRUE
	// when a tool omits its annotations, so an unannotated surface advertises
	// itself as entirely destructive and a well-behaved client gates every
	// call behind a confirmation. Nothing here destroys anything: these tools
	// append to a log and update a status line.
	//
	//	readOnly    reads nothing but reads
	//	idempotent  calling it twice with the same arguments changes nothing
	//	            more than calling it once
	//	additive    it writes, and two identical calls are two real writes
	//
	// OpenWorldHint is false throughout: metiche talks to its own database and
	// nothing else. That is a product constraint, not an implementation
	// detail — no third-party credential may ever be required to run it.
	var (
		readOnly   = &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
		idempotent = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
		additive   = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
	)

	addTool(server, logger, &mcp.Tool{
		Name: "create_team",
		Description: "Create a new metiche team and join it in the same call. Use this when nobody has set the team up yet; if you were given a join code, use join_team instead. " +
			"Returns the team's join_code — the shared secret your teammates need — and your own bearer token. " +
			"No account or login is involved anywhere in metiche: the join code is the credential, so anyone can create a team and abuse is bounded by per-address rate limiting and plan limits rather than by a sign-in. " +
			"Pass an idempotency_key of at least 8 characters (a uuid is ideal): retrying with the same key returns the same team instead of creating a second one.",
		// Additive rather than idempotent: two calls with two different
		// idempotency keys are two real teams. The key makes a RETRY safe,
		// which is not the same promise.
		Annotations: additive,
	}, h.CreateTeam)

	addTool(server, logger, &mcp.Tool{
		Name: "join_team",
		Description: "Join a team with its join code and get your own bearer token. Call this ONCE, before anything else, then send the token as an Authorization: Bearer header on every later call. " +
			"Pass a stable client_key (your session id, or the working directory plus your label) so that restarting re-joins you as the same agent instead of adding a duplicate to the board. " +
			"The token is shown once and cannot be recovered; re-joining mints a new one and retires the old.",
		// Idempotent on identity: re-joining with the same (member, client_key)
		// lands on the same agent row rather than creating a second one.
		Annotations: idempotent,
	}, h.JoinTeam)

	addTool(server, logger, &mcp.Tool{
		Name: "start_session",
		Description: "Begin one bounded piece of work: which repo, which branch, what commit you are starting from, and what you are trying to achieve. Returns a session_key you pass to every later call. " +
			"Start a new session per piece of work, not per message.",
		Annotations: idempotent,
	}, h.StartSession)

	addTool(server, logger, &mcp.Tool{
		Name:        "end_session",
		Description: "Close a session and immediately release the files it was holding. Always call this, especially when things went badly — an abandoned session keeps its claims until they time out, and blocks nobody but confuses everybody.",
		Annotations: idempotent,
	}, h.EndSession)

	addTool(server, logger, &mcp.Tool{
		Name: "heartbeat",
		Description: "Say you are still alive, extend your file claims, update the one-line 'what I am doing right now' the board shows, and pick up the count of anything waiting for you. " +
			"Call it about every 60 seconds while you work: it is cheap, it is the only call with a time obligation, and your claims lapse without it. " +
			"If the pending counts come back non-zero, act on them.",
		Annotations: idempotent,
	}, h.Heartbeat)

	addTool(server, logger, &mcp.Tool{
		Name: "get_team_state",
		Description: "See who else is working and on what: their branch, their goal, and their current status line. Call it once at the start of a session to orient yourself, and again when the sequence on your responses jumped further than you expected. " +
			"Scoped and paginated; it will never hand you the whole board.",
		Annotations: readOnly,
	}, h.GetTeamState)

	addTool(server, logger, &mcp.Tool{
		Name:        "health",
		Description: "Check whether metiche itself is reachable and its database is up. Call this when a tool fails with a transport error, to tell 'metiche is down, retry later' apart from 'my session is broken'. Never abandon a session on a single failed call without checking here first.",
		Annotations: readOnly,
	}, h.Health)

	// ── the seam ────────────────────────────────────────────────────────────
	// Tools 5–14 from PLAN.md (declare_intent, update_intent, check_paths,
	// publish_contract, record_decision, get_review_context, report_judgement,
	// resolve_conflict, get_instructions, report_back) register here, exactly
	// like the six above. Each one:
	//
	//   - takes its agent from h.requireAgent(ctx);
	//   - routes every write through h.commit(Mutation{...}), which is the
	//     only thing in this package that writes an event;
	//   - does its detection in the Mutation's Detect hook or in the handler-
	//     wide one installed with SetDetector, so the check and the insert it
	//     authorises happen under the same row lock;
	//   - returns an Envelope, so the pending counts ride along;
	//   - declares readOnly / idempotent / additive, explicitly.
	//
	// get_instructions is the one that is NOT readOnly, however much it looks
	// like it: reading an instruction is what marks it delivered.

	return server
}
