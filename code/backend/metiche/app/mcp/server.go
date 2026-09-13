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
  1. join_team once per client, with the team's join code. Save the token it returns; it is shown once.
  2. start_session once, when you begin a piece of work.
  3. heartbeat about every 60 seconds while you work. Claims lapse without it.
  4. end_session when you are done, so your holds are released immediately.

Name the repository from git, never from your own guess, and run these inside the repository
whatever folder you were started in:
  repo_url    = the output of: git remote get-url origin   (omit it only if there is no remote)
  project_key = the basename of: git rev-parse --show-toplevel
  branch      = the output of: git branch --show-current
metiche matches projects by repo_url first, so every agent in one repository lands on one project -
which is the only way their claims can collide. Every path you send to declare_intent, update_intent
and check_paths is relative to that git root, NOT to your working directory: started in the parent
folder or a subfolder, the file is still app/rest.go.

Your token identifies THIS agent - this client, on this machine. Every client gets its own, and the
Authorization header is the only thing any call needs. You never need client_key; omit it, and never
guess one. Several terminals of one client are several sessions of one agent: call start_session in
each, and it tells you about your other live sessions.

Send your token on every call, including a later join_team for a second team - joining again with
your own token keeps it and adds a membership rather than a second identity. A team is a per-call
scope: pass team_slug when you are on more than one. A join code is an invite, so it can be revoked,
expired or used up; ask for a fresh one rather than retrying a dead one.
To let a teammate join, call create_invite and give them the code; they run the metiche installer and paste it.

Every response carries "pending": counts of instructions, conflicts and reviews waiting for you.
MCP cannot push, so that count is how you find out anything. When it is non-zero, fetch the
contents with get_instructions - reading them is what marks them delivered, so you get each one
once - and say what you did with report_back. When it is zero, carry on.

Two cursors ride on every response. "sequence" advances on every event on the team - if it jumped
by more than you expected, you missed something. "revision" advances only when the board's shape
changed.

Responses are small on purpose. metiche will never hand you the whole board or everybody's claims;
ask for what you need with get_team_state.

If the person asks to see the board, call open_board.

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

	addTool(server, h, logger, &mcp.Tool{
		Name: "create_team",
		Description: "Create a new metiche team and join it in the same call. Use this when nobody has set the team up yet; if you were given a join code, use join_team instead. " +
			"Returns the team's first join_code — the shared secret your teammates need — and, unless you sent this agent's own token, this agent's bearer token. " +
			"There is no signup anywhere in metiche: a first call with no token mints you an anonymous identity and hands you this agent's token once, so abuse is bounded by per-address rate limiting and the team's plan rather than by a sign-in. " +
			"Pass an idempotency_key of at least 8 characters (a uuid is ideal): retrying with the same key, carrying your token, returns the same team and the same join code instead of creating a second one.",
		// Additive rather than idempotent: two calls with two different
		// idempotency keys are two real teams. The key makes a RETRY safe,
		// which is not the same promise.
		Annotations: additive,
	}, h.CreateTeam)

	addTool(server, h, logger, &mcp.Tool{
		Name: "join_team",
		Description: "Join a team and get this agent's bearer token. Call this before anything else, then send the token as an Authorization: Bearer header on every later call — it is the only thing later calls need. " +
			"Admission is a join_code, or — to attach another of your clients to a team you are already on — team_slug plus any of your tokens, which spends no invite. " +
			"Pass a stable client_key so that restarting re-joins you as the same agent instead of adding a duplicate to the board. " +
			"The token identifies THIS agent, not the person: every client gets its own, it is shown once and cannot be recovered. Re-joining with this agent's own token keeps it (token_kept); a join without it issues this agent a new token.",
		// Idempotent on identity: re-joining with the same (member, client_key)
		// lands on the same agent row rather than creating a second one.
		Annotations: idempotent,
	}, h.JoinTeam)

	addTool(server, h, logger, &mcp.Tool{
		Name: "start_session",
		Description: "Begin one bounded piece of work: which repo, which branch, what commit you are starting from, and what you are trying to achieve. Returns a session_key you pass to every later call. " +
			"Start a new session per piece of work, not per message. " +
			"Identify the repository from git, run inside it: repo_url = 'git remote get-url origin' (omit if there is no remote), project_key = the basename of 'git rev-parse --show-toplevel', branch = 'git branch --show-current'. " +
			"Every path you later claim or check is relative to that git root, not to your working directory. " +
			"If you are on more than one team, pass team_slug so the work lands on the right board. You never need client_key: your token already names this agent. " +
			"Several terminals of one client are several sessions of one agent; start_session tells you about your other live sessions.",
		Annotations: idempotent,
	}, h.StartSession)

	addTool(server, h, logger, &mcp.Tool{
		Name:        "end_session",
		Description: "Close a session and immediately release the files it was holding. Always call this, especially when things went badly — an abandoned session keeps its claims until they time out, and blocks nobody but confuses everybody.",
		Annotations: idempotent,
	}, h.EndSession)

	addTool(server, h, logger, &mcp.Tool{
		Name: "heartbeat",
		Description: "Say you are still alive, extend your file claims, update the one-line 'what I am doing right now' the board shows, and pick up the count of anything waiting for you. " +
			"Call it about every 60 seconds while you work: it is cheap, it is the only call with a time obligation, and your claims lapse without it. " +
			"If the pending counts come back non-zero, act on them.",
		Annotations: idempotent,
	}, h.Heartbeat)

	addTool(server, h, logger, &mcp.Tool{
		Name: "get_team_state",
		Description: "See who else is working and on what: their branch, their goal, and their current status line. Call it once at the start of a session to orient yourself, and again when the sequence on your responses jumped further than you expected. " +
			"Scoped and paginated; it will never hand you the whole board. Pass team_slug if you are on more than one team.",
		Annotations: readOnly,
	}, h.GetTeamState)

	addTool(server, h, logger, &mcp.Tool{
		Name: "list_teams",
		Description: "Which teams am I on? Answers 'which team does this repository belong to' when you do not already know, and it is the ONLY way to find out — never guess a team from the directory name, the repo name or the git remote. " +
			"Read a .metiche file first: if the repo or any parent directory has one, the team it names WINS over this list and you should not call this tool at all. " +
			"Otherwise call this before start_session, read the note it returns, and do what it says — with one team you bind to it and tell the person once; with more than one you stop and ask the person which, because putting private work on the wrong team's board cannot be undone. " +
			"Takes no arguments and needs only your bearer token: it is the one call you can make before you have a team or a session. Returns each team's slug (what you pass as team_slug), its name, your role, how many members it has, and whether you already have a session running there.",
		Annotations: readOnly,
	}, h.ListTeams)

	addTool(server, h, logger, &mcp.Tool{
		Name: "open_board",
		Description: "Open the metiche board for the person. Returns login_url, a sign-in link that signs ONE browser in as this person, works once and expires in 10 minutes. " +
			"If the person asked you to open the board and you can run a command, write it into a private temporary file and open that file with the OS opener — never put the link itself on a command line. " +
			"Otherwise show it to them once. Never write it into the repository, a commit, an issue, a PR or a chat channel. board_url is the plain address to share.",
		// Additive: every call stores a new single-use link. No idempotency
		// key, on purpose: a replay would have to hand back the same secret,
		// and the only way to do that is to store it.
		Annotations: additive,
	}, h.OpenBoard)

	addTool(server, h, logger, &mcp.Tool{
		Name: "sign_out_browsers",
		Description: "List or sign out the browsers signed in to the person's metiche board. With no arguments it only lists them (key, when created and last seen, user agent) and changes nothing. " +
			"session_key signs out that one browser; all=true signs out every browser on this account. Only ever this account's own browsers. " +
			"Never sign anything out unless the person asked you to.",
		// Destructive: it ends a person's signed-in browsers (revoke_invite is
		// the other destructive tool). Idempotent: signing out a browser that is
		// already signed out changes nothing.
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)},
	}, h.SignOutBrowsers)

	// Invites (docs/CLI.md §1.6, §4.3-4.5). The code is shown once, by
	// create_invite, and by nothing after it.
	addTool(server, h, logger, &mcp.Tool{
		Name: "create_invite",
		Description: "Make a join code so a teammate can join this team. Returns code, shown ONLY on this call: give it to the person, who runs the metiche installer (curl -fsSL https://metiche.xyz/install.sh | sh), chooses join and pastes it. " +
			"Defaults to 1 use and 7 days; owners may allow up to 100 uses and 30 days, members up to 25 uses and 7 days. " +
			"Treat the code like a door code: never write it to the repository, a commit, an issue, a PR or a chat channel. " +
			"Pass team_slug if you are on more than one team. An idempotency_key makes a retry return the same invite, without its code.",
		// Additive: two calls are two invites. The optional key makes a RETRY
		// safe, which is a different promise.
		Annotations: additive,
	}, h.CreateInvite)

	addTool(server, h, logger, &mcp.Tool{
		Name: "list_invites",
		Description: "List this team's invites: label, who made each, uses and max_uses, expiry, state (active, expired, exhausted, revoked) and when it was last used. Never the codes. " +
			"The team's owner sees every invite; a member sees only the ones they created. Pass team_slug if you are on more than one team.",
		Annotations: readOnly,
	}, h.ListInvites)

	addTool(server, h, logger, &mcp.Tool{
		Name: "revoke_invite",
		Description: "Revoke an invite by its invite_id, so nobody new can join with its code. People who already joined keep their access. " +
			"The team's owner can revoke any invite; a member only their own. Revoking an already-revoked invite changes nothing. " +
			"Never revoke an invite unless the person asked you to.",
		// Destructive: anyone holding the code loses the way in, and nothing
		// in the tool surface can un-revoke it. Idempotent: a second revoke
		// changes nothing.
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)},
	}, h.RevokeInvite)

	addTool(server, h, logger, &mcp.Tool{
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
	//   - takes its caller from h.RequireSession(ctx, session_key) — or
	//     h.RequireTeam / h.RequireAgent when it names no session — which
	//     returns a Resolved: the account, member, agent, session and team,
	//     with the membership already checked;
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

	// Tools 5-7: declare_intent, update_intent, check_paths. The detection
	// they run is NOT in this file -- it is the handler-wide DetectHook
	// installed by app/rest.go, which is what puts the overlap check inside
	// the same row lock as the insert it authorises.
	RegisterWorkTools(server, h, logger)

	// Tools 13-14: get_instructions, report_back. The other end of the
	// pending counts above -- the count says something is waiting,
	// get_instructions is how it is collected, and report_back is how the
	// person who raised it finds out what happened. get_instructions is the
	// one that is NOT readOnly: reading is the delivery receipt.
	RegisterInstructionTools(server, h, logger)

	return server
}
