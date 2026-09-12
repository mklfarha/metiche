// Package authz answers the one question the board's HTTP surface must ask
// before it does anything else: may THIS request read THIS team?
//
// app/webapi and app/stream are mounted on the PUBLIC router and the deploy
// target is a public host. A team board carries file paths, branch names,
// goals, status lines and recorded decisions — it is the single most sensitive
// thing this system stores, and `team.visibility` exists precisely so that a
// board is private unless somebody said otherwise. The column defaults to
// private (create.sql: `visibility` INT NOT NULL DEFAULT 1) and create_team
// writes private explicitly, so "nothing reads visibility" meant "every board
// was world-readable to anyone who guessed a slug". This package is what reads
// it.
//
// # The decision, in full
//
//	public team   -> anyone, with no token at all. That is what the field is
//	                 for and what the demo board depends on.
//	private team  -> a bearer token whose ACCOUNT is a live member of that
//	                 team. Everything else is denied.
//
// # Denial is 404, never 403
//
// A 403 is an admission that the team exists. Slugs are short, human-chosen
// and guessable, so an endpoint that answers 403 for "real but not yours" and
// 404 for "no such team" hands out a free oracle for enumerating the slug
// namespace — and the mere existence of a team, with its name-shaped slug, is
// itself information about who is building what. So every refusal here is the
// SAME refusal: ErrDenied, rendered as the caller package's own byte-identical
// "no such team" response. A stranger cannot tell an unknown slug from a
// private one from one whose token was wrong, and neither can a script.
//
// This is the same reasoning app/mcp/auth.go applies to invites
// (ErrInviteNotUsable is one error for every reason) and that
// stream.ErrTeamNotFound already documents for the lookup.
//
// # Reuse, not a second scheme
//
// The token path is app/mcp's, unchanged: mcp.BearerFromHeader parses the
// header, mcp.AccountByToken hashes it with mcp.HashToken and re-checks it in
// constant time with mcp.VerifyToken against account.token_hash. There is
// exactly ONE definition in this repository of "what a metiche token is and
// who it belongs to", and this package calls it rather than restating it. The
// import direction (authz -> mcp) is deliberate: mcp is where identity lives,
// and nothing in mcp imports this package, so there is no cycle.
//
// # A token never comes from the URL
//
// extractToken below is the ONLY place a credential may enter this package,
// and it reads request HEADERS. Not the query string — ever. A token in a URL
// is a token in the access log, in the Referer header, in the browser's
// history and in whatever proxy sits in front of the pod. The consequence is
// real and is not worked around here: a browser's EventSource cannot set
// headers, so a private team's SSE stream needs fetch-based SSE, a cookie
// session, or a short-lived ticket — a design decision, not something to
// paper over with ?token=.
package authz

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/enums"
)

// Team is the little a granted decision hands back.
//
// Deliberately not the team row: this type must never be able to carry a
// secret, for the same reason stream.TeamRef and webapi.teamRef cannot.
type Team struct {
	UUID   uuid.UUID
	Slug   string
	Public bool
}

// ErrDenied is the only error a refused read ever produces.
//
// One error for every reason — no such slug, private with no token, private
// with a token that does not resolve, private with a token belonging to
// somebody who is not a member, membership revoked — because a caller that can
// tell those apart can enumerate teams. See the package comment.
var ErrDenied = errors.New("no such team")

// Authorizer is the decision, as an interface so a test can drive the two
// branches without a database and so the hub-and-fake-source tests in
// app/stream keep working with no MySQL in sight.
type Authorizer interface {
	// Authorize takes the team as it appeared in the URL and the bearer token
	// ALREADY EXTRACTED FROM A HEADER — never a raw request, so that no
	// implementation can quietly start accepting a credential from the query
	// string. An empty token means "the caller presented none".
	Authorize(ctx context.Context, teamRef, token string) (Team, error)
}

// AuthorizerFunc adapts a plain function to Authorizer.
type AuthorizerFunc func(ctx context.Context, teamRef, token string) (Team, error)

// Authorize implements Authorizer.
func (f AuthorizerFunc) Authorize(ctx context.Context, teamRef, token string) (Team, error) {
	return f(ctx, teamRef, token)
}

// Guard is the production Authorizer: two indexed point reads against the
// database, and nothing cached.
//
// Nothing cached on purpose. A cache here would outlive a membership
// revocation, which is exactly the moment the answer has to change — the same
// reasoning app/mcp/auth.go gives for reading account.token_hash straight from
// the table on every call.
type Guard struct{ db *sql.DB }

// NewGuard builds the database-backed decision.
func NewGuard(db *sql.DB) *Guard { return &Guard{db: db} }

// Authorize implements Authorizer.
//
// Order matters: resolve the team FIRST, and short-circuit on public before
// the token is even looked at. A public board must cost one query and must
// never reject a caller for holding a stale token.
func (g *Guard) Authorize(ctx context.Context, teamRef, token string) (Team, error) {
	team, err := g.resolveTeam(ctx, teamRef)
	if err != nil {
		return Team{}, err
	}
	if team.Public {
		return team, nil
	}

	// Private from here down. Every path out is ErrDenied.
	if strings.TrimSpace(token) == "" {
		return Team{}, ErrDenied
	}
	acct, err := metichemcp.AccountByToken(ctx, g.db, token)
	if err != nil {
		// mcp.AccountByToken already collapses "no such token" and "the
		// database is unhappy" into one opaque error, for the same reason
		// this function collapses everything into ErrDenied.
		return Team{}, ErrDenied
	}
	if acct.Status != enums.RECORD_STATUS_ACTIVE {
		return Team{}, ErrDenied
	}

	member, err := g.isLiveMember(ctx, acct.ID, team.UUID)
	if err != nil {
		return Team{}, err
	}
	if !member {
		return Team{}, ErrDenied
	}
	return team, nil
}

// resolveTeam looks a team up by slug, falling back to uuid.
//
// The uuid fallback is not optional: both calling packages sit at
// /v1/teams/{slug} on the ROOT router, chi matches that ahead of the generated
// /v1/teams/{id} mount, and both of their own resolvers already accept a uuid.
// A guard that only understood slugs would 404 every uuid-addressed request
// before the handler that handles it fine ever ran.
func (g *Guard) resolveTeam(ctx context.Context, ref string) (Team, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Team{}, ErrDenied
	}

	// col is chosen from the two literals at the call sites below, never from
	// user input; the value is always a bound parameter.
	scan := func(col, val string) (Team, error) {
		var (
			id, slug   string
			visibility int64
		)
		err := g.db.QueryRowContext(ctx,
			"SELECT `id`, `slug`, `visibility` FROM `team` WHERE `"+col+"` = ? LIMIT 1",
			val).Scan(&id, &slug, &visibility)
		if err != nil {
			return Team{}, err
		}
		parsed, err := uuid.FromString(id)
		if err != nil {
			return Team{}, err
		}
		// Anything that is not exactly "public" is private. An unset,
		// invalid or newly added visibility value therefore fails CLOSED,
		// which is the only safe direction for a field whose whole job is to
		// keep a board off the open internet.
		return Team{
			UUID:   parsed,
			Slug:   slug,
			Public: enums.TeamVisibility(visibility) == enums.TEAM_VISIBILITY_PUBLIC,
		}, nil
	}

	t, err := scan("slug", ref)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Team{}, err
	}
	if _, perr := uuid.FromString(ref); perr != nil {
		return Team{}, ErrDenied
	}
	t, err = scan("id", ref)
	if errors.Is(err, sql.ErrNoRows) {
		return Team{}, ErrDenied
	}
	if err != nil {
		return Team{}, err
	}
	return t, nil
}

// isLiveMember is the membership half of the check: this account, this team,
// not revoked, still active.
//
// revoked_at is tested as well as status because revocation is the SOFT
// removal in this schema — the row stays for the history that references it —
// so a check on status alone would keep letting an ejected member read the
// board. Same predicate as mcp's liveMemberOf.
func (g *Guard) isLiveMember(ctx context.Context, accountUUID, teamUUID uuid.UUID) (bool, error) {
	var one int
	err := g.db.QueryRowContext(ctx,
		"SELECT 1 FROM `member` WHERE `account_uuid` = ? AND `team_uuid` = ? "+
			"AND `revoked_at` IS NULL AND `status` = ? LIMIT 1",
		accountUUID.String(), teamUUID.String(), enums.RECORD_STATUS_ACTIVE).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ─────────────────────────────────────────────
// The middleware
// ─────────────────────────────────────────────

// Middleware turns the decision into a wrapper around one route's handler.
//
// It is a WRAPPER rather than router middleware for two reasons, one of them a
// crash and one of them correctness:
//
//   - chi panics — while the router is being built, so the container compiles,
//     ships and then crash-loops — if r.Use is called on a mux that already
//     has routes, and both of these packages are handed the root router AFTER
//     the generated CRUD is mounted on it. Wrapping the handler is the form
//     that is always legal.
//   - chi populates the URL parameters during routing, which happens after any
//     r.Use middleware and before the endpoint handler. A wrapper on the
//     handler can therefore read chi.URLParam(r, "slug"); an r.Use middleware
//     could not, and would have to re-parse the path by hand.
type Middleware struct {
	// Authorizer makes the decision.
	Authorizer Authorizer

	// NotFound renders the refusal, and MUST be byte-identical to the response
	// the wrapped package already returns for a slug that does not exist —
	// same status, same Content-Type, same body. If the two differ in any
	// observable way then the difference itself is the 403 this design went to
	// the trouble of not sending: a probe would simply read "which kind of 404
	// did I get?" instead.
	NotFound http.HandlerFunc

	// Logger is optional.
	Logger *zap.Logger
}

// Wrap returns next, gated.
//
// For app/stream this is the whole reason the check is a wrapper and not
// something the handler does itself: an SSE handler writes 200 and flushes the
// headers before it knows anything, and once the stream is open the status
// code is spent. The client would see a healthy connection that dies, which is
// indistinguishable from a network blip and which every EventSource on earth
// retries forever. Refusing here means the caller gets an HTTP STATUS.
func (m Middleware) Wrap(next http.HandlerFunc) http.HandlerFunc {
	if m.Authorizer == nil {
		// A nil authorizer is a wiring bug, and the safe reading of a wiring
		// bug on this surface is "nobody gets in".
		return func(w http.ResponseWriter, r *http.Request) { m.deny(w, r) }
	}
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := m.Authorizer.Authorize(r.Context(), chi.URLParam(r, "slug"), extractToken(r))
		if err != nil {
			if !errors.Is(err, ErrDenied) && r.Context().Err() == nil {
				// An infrastructure failure fails CLOSED and is logged here,
				// because the caller is told nothing: answering 500 for a
				// database hiccup on a private team and 404 for an unknown one
				// would leak the distinction this package exists to hide. The
				// error is logged, never echoed — driver text can carry a DSN,
				// and a DSN carries a password.
				m.logger().Warn("authorizing a board read failed; denying",
					zap.String("path", r.URL.Path), zap.Error(err))
			}
			m.deny(w, r)
			return
		}
		next(w, r)
	}
}

func (m Middleware) deny(w http.ResponseWriter, r *http.Request) {
	if m.NotFound != nil {
		m.NotFound(w, r)
		return
	}
	http.Error(w, "no such team", http.StatusNotFound)
}

func (m Middleware) logger() *zap.Logger {
	if m.Logger == nil {
		return zap.NewNop()
	}
	return m.Logger
}

// extractToken is the only door a credential comes through.
//
// Headers only. The two spellings are exactly the two app/mcp's own middleware
// accepts — Authorization: Bearer, and the X-Metiche-Token fallback for
// clients that cannot set Authorization — so this is the same scheme, not a
// second one. The query string is deliberately not consulted; see the package
// comment for what that costs and why it is still right.
func extractToken(r *http.Request) string {
	if t := metichemcp.BearerFromHeader(r.Header.Get("Authorization")); t != "" {
		return t
	}
	return strings.TrimSpace(r.Header.Get("X-Metiche-Token"))
}
