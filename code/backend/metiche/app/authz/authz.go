// Package authz answers the one question the board's HTTP surface must ask
// before it does anything else: may THIS request read THIS team?
//
// app/webapi and app/stream are mounted on the backend's root router. A team
// board carries file paths, branch names, goals, status lines and recorded
// decisions — it is the single most sensitive thing this system stores, and
// `team.visibility` exists precisely so that a board is private unless somebody
// said otherwise. The column defaults to private (create.sql: `visibility` INT
// NOT NULL DEFAULT 1) and create_team writes private explicitly. This package
// is what reads it.
//
// # The decision, in full
//
//	public team   -> anyone, with no credential at all. The credential is not
//	                 even looked at, so a stale or garbage one never refuses a
//	                 public board. That is what the field is for and what the
//	                 demo board depends on.
//	private team  -> ONE principal whose ACCOUNT is a live member of that team:
//	                 either a bearer token (an agent's, or a legacy account's)
//	                 or a browser session (docs/BOARD_LOGIN.md §4.1). Both at
//	                 once is refused: a request is one principal. Everything
//	                 else is denied.
//
// Membership is read on every request and never cached. A cache here would
// outlive a membership revocation, which is exactly the moment the answer has
// to change.
//
// # Denial is 404, never 403
//
// A 403 is an admission that the team exists. Slugs are short, human-chosen
// and guessable, so an endpoint that answers 403 for "real but not yours" and
// 404 for "no such team" hands out a free oracle for enumerating the slug
// namespace. So every refusal here is the SAME refusal: ErrDenied, rendered as
// the caller package's own byte-identical "no such team" response. An unknown
// slug, a private team with no credential, a garbage session, a valid session
// for a non-member, and a bearer and a session sent together are
// indistinguishable.
//
// # An outage is 503, never 404
//
// Any error that is not ErrDenied means the decision could not be made (the
// database is unreachable). That is rendered as the caller's "unavailable"
// response, not as a refusal: the board must not tell a signed-in member "no
// such team" (and forget the team) because MySQL restarted, and it must not
// clear a good cookie. This is not an oracle: the team row is read first, so an
// outage answers 503 for an unknown slug just as it does for a real one
// (docs/BOARD_LOGIN.md §1, "DB outage").
//
// # Reuse, not a second scheme
//
// The bearer path is app/mcp's, unchanged: mcp.BearerFromHeader parses the
// header and mcp.AccountByToken delegates to mcp.IdentityByToken. The browser
// session path is app/browser's (browser.AccountBySession), the same predicate
// GET /v1/browser/session answers with. There is exactly one definition of each
// credential in this repository, and this package calls them rather than
// restating them. Import direction: authz -> mcp and authz -> browser; neither
// imports this package.
//
// # A credential never comes from the URL
//
// extractCredential below is the ONLY place a credential may enter this
// package, and it reads request HEADERS. Not the query string, and not a
// cookie — ever. A credential in a URL is a credential in the access log (chi's
// request logger logs the query string), the Referer header, the browser's
// history and whatever proxy sits in front of the pod. A backend credential on
// a cookie would be sent by any browser to a host that answers
// Access-Control-Allow-Origin: *. The board holds the viewer's cookie and
// relays it to the backend as the X-Metiche-Browser-Session header.
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

	"github.com/mklfarha/metiche/backend/app/browser"
	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/enums"
)

// HeaderBrowserSession is the header a browser session travels in, from the
// board to the backend ("X-Metiche-Browser-Session"). It is the only way a
// session enters this package. It is app/browser's constant, so the two
// packages cannot drift apart.
const HeaderBrowserSession = browser.HeaderSession

// Team is the little a granted decision hands back.
//
// Deliberately not the team row: this type must never be able to carry a
// secret, for the same reason stream.TeamRef and webapi.teamRef cannot.
type Team struct {
	UUID   uuid.UUID
	Slug   string
	Public bool

	// Role is the viewer's live membership role when the grant was decided by
	// membership (a private team). It is MEMBER_ROLE_INVALID for a public team,
	// whose grant never looks at a credential.
	Role enums.MemberRole
}

// Credential is what the caller presented, already extracted from HEADERS.
//
// Its String and GoString never print the values, so a Credential handed to a
// logger or a %v by mistake does not become a leaked secret.
type Credential struct {
	// Bearer is an agent (or legacy account) token: Authorization: Bearer, or
	// X-Metiche-Token.
	Bearer string
	// BrowserSession is a browser session secret: X-Metiche-Browser-Session.
	BrowserSession string
}

// String implements fmt.Stringer without revealing either value.
func (c Credential) String() string {
	return "authz.Credential{bearer:" + presence(c.Bearer) + ", browser_session:" + presence(c.BrowserSession) + "}"
}

// GoString implements fmt.GoStringer without revealing either value.
func (c Credential) GoString() string { return c.String() }

func presence(s string) string {
	if strings.TrimSpace(s) == "" {
		return "absent"
	}
	return "present"
}

// ErrDenied is the only error a refused read ever produces.
//
// One error for every reason — no such slug, private with no credential, a
// credential that does not resolve, a valid one belonging to somebody who is
// not a member, membership revoked, two credentials at once — because a caller
// that can tell those apart can enumerate teams. See the package comment.
var ErrDenied = errors.New("no such team")

// Authorizer is the decision, as an interface so a test can drive the
// branches without a database and so the hub-and-fake-source tests in
// app/stream keep working with no MySQL in sight.
type Authorizer interface {
	// Authorize takes the team as it appeared in the URL and the credential
	// ALREADY EXTRACTED FROM HEADERS — never a raw request, so that no
	// implementation can quietly start accepting a credential from the query
	// string. A zero Credential means "the caller presented none".
	//
	// It returns ErrDenied for every refusal, and any other error only when
	// the decision could not be made.
	Authorize(ctx context.Context, teamRef string, cred Credential) (Team, error)
}

// AuthorizerFunc adapts a plain function to Authorizer.
type AuthorizerFunc func(ctx context.Context, teamRef string, cred Credential) (Team, error)

// Authorize implements Authorizer.
func (f AuthorizerFunc) Authorize(ctx context.Context, teamRef string, cred Credential) (Team, error) {
	return f(ctx, teamRef, cred)
}

// Guard is the production Authorizer: indexed point reads against the
// database, and nothing cached.
type Guard struct{ db *sql.DB }

// NewGuard builds the database-backed decision.
func NewGuard(db *sql.DB) *Guard { return &Guard{db: db} }

// Authorize implements Authorizer.
//
// Order matters: resolve the team FIRST, and short-circuit on public before
// either credential is even looked at. A public board must cost one query and
// must never reject a caller for holding a stale or garbage credential.
func (g *Guard) Authorize(ctx context.Context, teamRef string, cred Credential) (Team, error) {
	team, err := g.resolveTeam(ctx, teamRef)
	if err != nil {
		return Team{}, err
	}
	if team.Public {
		return team, nil
	}

	// Private from here down. Every refusal is ErrDenied.
	accountID, err := g.accountFor(ctx, cred)
	if err != nil {
		return Team{}, err
	}

	role, member, err := g.isLiveMember(ctx, accountID, team.UUID)
	if err != nil {
		return Team{}, err
	}
	if !member {
		return Team{}, ErrDenied
	}
	team.Role = role
	return team, nil
}

// ViewerRole answers, for a team the caller has ALREADY been granted, the
// presented principal's live membership role — MEMBER_ROLE_INVALID when there
// is no credential, the credential does not resolve, both kinds were sent, or
// the account is not a live member.
//
// It exists for GET /v1/teams/{slug}/access on a PUBLIC team, where the grant
// deliberately never looked at the credential. It never refuses: a garbage
// session on a public team is a viewer with no role, not an error. Only an
// infrastructure failure returns an error.
func (g *Guard) ViewerRole(ctx context.Context, team Team, cred Credential) (enums.MemberRole, error) {
	if team.Role != enums.MEMBER_ROLE_INVALID {
		return team.Role, nil
	}
	accountID, err := g.accountFor(ctx, cred)
	if errors.Is(err, ErrDenied) {
		return enums.MEMBER_ROLE_INVALID, nil
	}
	if err != nil {
		return enums.MEMBER_ROLE_INVALID, err
	}
	role, member, err := g.isLiveMember(ctx, accountID, team.UUID)
	if err != nil || !member {
		return enums.MEMBER_ROLE_INVALID, err
	}
	return role, nil
}

// accountFor resolves the ONE principal a request presents to its account.
//
// ErrDenied for no credential, two credentials, or one that does not resolve
// to an ACTIVE account. Any other error means the answer is unknown.
func (g *Guard) accountFor(ctx context.Context, cred Credential) (uuid.UUID, error) {
	bearer := strings.TrimSpace(cred.Bearer)
	session := strings.TrimSpace(cred.BrowserSession)

	switch {
	case bearer != "" && session != "":
		// A request is one principal. The board never sends a bearer, and an
		// agent never holds a browser session, so both at once is a confused
		// or crafted request; refusing it keeps "whose read is this?" a
		// question with one answer.
		return uuid.Nil, ErrDenied

	case bearer != "":
		acct, err := metichemcp.AccountByToken(ctx, g.db, bearer)
		if err != nil {
			// mcp.AccountByToken already collapses "no such token" and "the
			// database is unhappy" into one opaque error.
			return uuid.Nil, ErrDenied
		}
		if acct.Status != enums.RECORD_STATUS_ACTIVE {
			return uuid.Nil, ErrDenied
		}
		return acct.ID, nil

	case session != "":
		acct, err := browser.AccountBySession(ctx, g.db, session)
		if errors.Is(err, browser.ErrUnauthenticated) {
			return uuid.Nil, ErrDenied
		}
		if err != nil {
			// Unknown, not refused (browser.ErrUnavailable): the board shows
			// "unavailable" and keeps the cookie instead of signing the
			// person out. It never grants.
			return uuid.Nil, err
		}
		if acct.Status != enums.RECORD_STATUS_ACTIVE || acct.ID == uuid.Nil {
			return uuid.Nil, ErrDenied
		}
		return acct.ID, nil
	}
	return uuid.Nil, ErrDenied
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
// not revoked, still active. It returns the member's role alongside.
//
// revoked_at is tested as well as status because revocation is the SOFT
// removal in this schema — the row stays for the history that references it —
// so a check on status alone would keep letting an ejected member read the
// board. Same predicate as mcp's liveMemberOf.
func (g *Guard) isLiveMember(ctx context.Context, accountUUID, teamUUID uuid.UUID) (enums.MemberRole, bool, error) {
	var role int64
	err := g.db.QueryRowContext(ctx,
		"SELECT `role` FROM `member` WHERE `account_uuid` = ? AND `team_uuid` = ? "+
			"AND `revoked_at` IS NULL AND `status` = ? LIMIT 1",
		accountUUID.String(), teamUUID.String(), enums.RECORD_STATUS_ACTIVE).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return enums.MEMBER_ROLE_INVALID, false, nil
	}
	if err != nil {
		return enums.MEMBER_ROLE_INVALID, false, err
	}
	return enums.MemberRole(role), true, nil
}

// ─────────────────────────────────────────────
// The grant, on the request context
// ─────────────────────────────────────────────

type ctxKey int

const (
	teamKey ctxKey = iota
	credentialKey
)

// TeamFromContext returns the team Middleware.Wrap granted this request.
// ok is false outside a wrapped handler.
func TeamFromContext(ctx context.Context) (Team, bool) {
	t, ok := ctx.Value(teamKey).(Team)
	return t, ok
}

// CredentialFromContext returns the credential Middleware.Wrap authorized
// this request with, so a long-lived handler (the SSE stream) can re-run the
// same decision later. Zero outside a wrapped handler.
func CredentialFromContext(ctx context.Context) Credential {
	c, _ := ctx.Value(credentialKey).(Credential)
	return c
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
	// the trouble of not sending.
	NotFound http.HandlerFunc

	// Unavailable renders "the decision could not be made" (503). Optional;
	// the default is a plain-text 503.
	Unavailable http.HandlerFunc

	// Logger is optional.
	Logger *zap.Logger
}

// Wrap returns next, gated. A granted request reaches next with the Team and
// the Credential on its context (TeamFromContext, CredentialFromContext).
//
// For app/stream this is the whole reason the check is a wrapper and not
// something the handler does itself: an SSE handler writes 200 and flushes the
// headers before it knows anything, and once the stream is open the status
// code is spent. Refusing here means the caller gets an HTTP STATUS.
func (m Middleware) Wrap(next http.HandlerFunc) http.HandlerFunc {
	if m.Authorizer == nil {
		// A nil authorizer is a wiring bug, and the safe reading of a wiring
		// bug on this surface is "nobody gets in".
		return func(w http.ResponseWriter, r *http.Request) { m.deny(w, r) }
	}
	return func(w http.ResponseWriter, r *http.Request) {
		cred := extractCredential(r)
		team, err := m.Authorizer.Authorize(r.Context(), chi.URLParam(r, "slug"), cred)
		if err != nil {
			if errors.Is(err, ErrDenied) {
				m.deny(w, r)
				return
			}
			if r.Context().Err() == nil {
				// Logged, never echoed — driver text can carry a DSN, and a
				// DSN carries a password. The credential is not logged.
				m.logger().Warn("authorizing a board read failed; answering unavailable",
					zap.String("path", r.URL.Path), zap.Error(err))
			}
			m.unavailable(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), teamKey, team)
		ctx = context.WithValue(ctx, credentialKey, cred)
		next(w, r.WithContext(ctx))
	}
}

func (m Middleware) deny(w http.ResponseWriter, r *http.Request) {
	if m.NotFound != nil {
		m.NotFound(w, r)
		return
	}
	http.Error(w, "no such team", http.StatusNotFound)
}

func (m Middleware) unavailable(w http.ResponseWriter, r *http.Request) {
	if m.Unavailable != nil {
		m.Unavailable(w, r)
		return
	}
	http.Error(w, "unavailable", http.StatusServiceUnavailable)
}

func (m Middleware) logger() *zap.Logger {
	if m.Logger == nil {
		return zap.NewNop()
	}
	return m.Logger
}

// extractCredential is the only door a credential comes through.
//
// Headers only:
//   - Authorization: Bearer, then the X-Metiche-Token fallback -> Bearer.
//     Exactly the two spellings app/mcp's own middleware accepts.
//   - X-Metiche-Browser-Session -> BrowserSession.
//
// The query string and cookies are deliberately not consulted; see the package
// comment.
func extractCredential(r *http.Request) Credential {
	bearer := metichemcp.BearerFromHeader(r.Header.Get("Authorization"))
	if bearer == "" {
		bearer = strings.TrimSpace(r.Header.Get("X-Metiche-Token"))
	}
	return Credential{
		Bearer:         bearer,
		BrowserSession: strings.TrimSpace(r.Header.Get(HeaderBrowserSession)),
	}
}
