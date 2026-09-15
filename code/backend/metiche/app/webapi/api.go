// Package webapi is the board's read API: purpose-built endpoints that return
// a team the way a board wants to draw it.
//
// It exists alongside the generated CRUD rather than instead of it. The
// generated endpoints address a row by uuid and scope a child list with an AIP
// filter expression; everything else in metiche — the tool surface, the prose,
// the board's own URLs — addresses things by short key (a team slug, S-17,
// CF-14). A board built on the generated CRUD would have to resolve a slug to
// a uuid, then issue a dozen filtered list calls and hope they agreed with each
// other. These endpoints are the same reads done once, coherently, in the
// vocabulary the rest of the system already speaks.
//
// # Isolation
//
// Every read here runs in a READ-ONLY, REPEATABLE READ transaction. That is
// the whole answer to "reads must never see a partially applied transaction",
// and the reasoning is in beginRead below.
//
// # Two cursors, never conflated
//
// Every response carries both:
//
//   - sequence — advances on EVERY event. "Something changed; repaint that one
//     thing." It is also the resume cursor: pass it to the SSE stream as
//     ?after=N and the stream picks up exactly where the snapshot ended.
//   - board_revision — advances ONLY on a structural change. "The shape of the
//     board changed; rebuild the layout."
//
// A client that re-lays-out on a sequence bump flickers on every status-line
// edit. A client that only watches board_revision never repaints at all. They
// are different numbers meaning different things and they are both here for
// that reason.
package webapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/authz"
	"github.com/mklfarha/metiche/backend/core"
)

// API serves the board's reads.
type API struct {
	db     *sql.DB
	logger *zap.Logger

	// guard is the visibility-and-membership check every route in this
	// package runs BEFORE its handler. It is a field rather than something a
	// caller passes in because there must be no way to mount these endpoints
	// unguarded: RegisterOn is the only way in, and it wraps every route.
	guard authz.Middleware

	// roles answers GET /v1/teams/{slug}/access's "role" for a public team,
	// whose grant never looked at the credential. See access.go.
	roles *authz.Guard
}

// NewAPI builds the read API over a database handle.
//
// It takes *sql.DB rather than *core.Implementation on purpose. These reads
// compose a dozen joins into one coherent answer and must all observe the same
// snapshot, which means one transaction — and the generated entity modules
// read through a shared in-process cache that no transaction covers. A cached
// row from before a write mixed with a fresh row from after it is precisely the
// torn read this package promises not to return.
func NewAPI(db *sql.DB, logger *zap.Logger) *API {
	if logger == nil {
		logger = zap.NewNop()
	}
	guard := authz.NewGuard(db)
	a := &API{db: db, logger: logger, roles: guard}
	a.guard = authz.Middleware{
		Authorizer: guard,
		// The refusal is notFound, which is the SAME call writeProblem makes
		// for errTeamNotFound below. Byte-identical on purpose: a private
		// team must be indistinguishable from one that does not exist, and a
		// difference in status, Content-Type or body would be the 403 this
		// deliberately does not send. 404, never 403 — a 403 confirms the
		// team exists and hands out a free oracle for the slug namespace.
		NotFound: a.notFound,
		// A decision that could not be made (the database is unreachable) is
		// 503, never the 404 above: the board must show "unavailable" to a
		// signed-in member, not "no such team", and must keep their cookie.
		Unavailable: a.unavailable,
		Logger:      logger,
	}
	return a
}

// notFound is this package's one "no such team" response, used both by the
// guard and by fail. See NewAPI for why they must be the same bytes.
func (a *API) notFound(w http.ResponseWriter, _ *http.Request) {
	writeProblem(w, http.StatusNotFound, "not found", "no such team")
}

// unavailable is this package's one "could not decide right now" response.
func (a *API) unavailable(w http.ResponseWriter, _ *http.Request) {
	writeProblem(w, http.StatusServiceUnavailable, "unavailable", "the board is unavailable right now")
}

// Route paths, exported so whoever mounts them does not retype them.
const (
	PathSnapshot  = "/v1/teams/{slug}"
	PathConflicts = "/v1/teams/{slug}/conflicts"
	PathContracts = "/v1/teams/{slug}/contracts"
	PathDecisions = "/v1/teams/{slug}/decisions"
	PathSession   = "/v1/teams/{slug}/sessions/{key}"

	// PathSessions is the team's run history: every session, newest first,
	// paged with an opaque cursor (history.go).
	PathSessions = "/v1/teams/{slug}/sessions"

	// PathConflictHistory (conflicthistory.go), PathEvents (events.go) and
	// PathGraph (graphhistory.go) are the rest of the board's history: past
	// conflicts, the event log backwards, and what was entangled over a past
	// window. Board only, like every read here.
	PathEvents = "/v1/teams/{slug}/events"
	PathGraph  = "/v1/teams/{slug}/graph"

	// PathAccess answers "may this viewer read this team, and as what?" for
	// the board (docs/BOARD_LOGIN.md §4.2). Board only; NOT routed by any
	// ingress.
	PathAccess = "/v1/teams/{slug}/access"
)

// RegisterOn mounts the read API on r, with every route gated.
//
// Spell the full path including /v1: r is the server's ROOT router and the
// generated CRUD already occupies /v1, so re-mounting it would panic at
// startup. chi matches a root-level route ahead of a mount, so
// GET /v1/teams/{slug} here takes precedence over the generated
// GET /v1/teams/{id}. That is intended — it is the board's endpoint — and
// resolveTeam accepts a uuid as well as a slug so addressing a team by its id
// keeps working.
//
// a.guard.Wrap is on EVERY route, and it wraps the handler rather than being
// installed with r.Use: r is the root router and chi panics — while the router
// is being built — if middleware is added to a mux that already has routes.
// Wrapping also puts the check after chi has populated {slug} and before the
// handler opens its read transaction, so a refused request touches nothing.
func (a *API) RegisterOn(r chi.Router) {
	r.Get(PathSnapshot, a.guard.Wrap(a.handleSnapshot))
	r.Get(PathConflicts, a.guard.Wrap(a.handleConflicts))
	r.Get(PathContracts, a.guard.Wrap(a.handleContracts))
	r.Get(PathDecisions, a.guard.Wrap(a.handleDecisions))
	r.Get(PathSession, a.guard.Wrap(a.handleSession))
	r.Get(PathSessions, a.guard.Wrap(a.handleSessions))
	r.Get(PathConflictHistory, a.guard.Wrap(a.handleConflictHistory))
	r.Get(PathEvents, a.guard.Wrap(a.handleEvents))
	r.Get(PathGraph, a.guard.Wrap(a.handleGraph))
	r.Get(PathAccess, a.guard.Wrap(a.handleAccess))
}

// Register wires the read API into the REST server.
func Register(r chi.Router, coreImpl *core.Implementation, logger *zap.Logger) *API {
	a := NewAPI(coreImpl.DB(), logger)
	a.RegisterOn(r)
	return a
}

// beginRead opens the transaction every endpoint in this package reads inside.
//
// REPEATABLE READ, read-only, and the choice matters:
//
// A board snapshot is five or six queries — the team's cursors, its live
// sessions, their intents, the claims they hold, the open conflicts. Under
// InnoDB's REPEATABLE READ the first read establishes a consistent MVCC
// snapshot and every later read in the transaction sees that same snapshot, so
// those queries observe one single instant in the team's history. A write
// transaction that commits halfway through — a declare_intent inserting an
// intent, its claims, its claim_paths and a conflict under the team lock — is
// either entirely visible or entirely invisible. It can never be half of both.
//
// READ COMMITTED, MySQL's other common choice, takes a FRESH snapshot per
// statement. That is exactly the torn read this promises not to return: the
// board would report sequence 417 from its first query and then list the
// claims as of 419, showing a cursor behind rows the client has already been
// told about — or worse, an intent whose claims query ran a millisecond too
// early and came back empty. The board would render a session holding nothing.
//
// SERIALIZABLE is the wrong direction. InnoDB implements it by turning plain
// SELECTs into locking reads, which would put a browser refresh in contention
// with the team lock that PLAN.md budgets at 5–15ms and monitors at a 25ms p99.
// A read path that can block the write path is not worth the ordering
// guarantee, and MVCC already gives the consistency without it.
//
// ReadOnly is set as well: InnoDB skips transaction-id allocation for a
// read-only transaction, and it makes any accidental write in this package a
// runtime error rather than a silent mutation on a read endpoint.
func (a *API) beginRead(ctx context.Context) (*sql.Tx, error) {
	return a.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
}

// read runs fn inside a read transaction and always rolls back — there is
// nothing to commit, and a rollback releases the snapshot immediately.
func (a *API) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := a.beginRead(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// teamRef is the team as the read API needs it. join_code is not a field here
// and is never selected anywhere in this package: a type that can hold a secret
// is a type that eventually logs one.
type teamRef struct {
	UUID          string `json:"-"`
	Key           string `json:"key"`
	Name          string `json:"name"`
	Sequence      int64  `json:"sequence"`
	BoardRevision int64  `json:"board_revision"`
}

var errTeamNotFound = errors.New("no such team")

// resolveTeam looks a team up by slug, falling back to uuid.
//
// The fallback exists because this package's routes shadow the generated
// /v1/teams/{id}: accepting both keeps a uuid-addressed request working.
func resolveTeam(ctx context.Context, tx *sql.Tx, slug string) (teamRef, error) {
	// col is chosen from two literals below, never from user input; the value
	// is always a bound parameter.
	scan := func(col, val string) (teamRef, error) {
		var t teamRef
		err := tx.QueryRowContext(ctx,
			"SELECT `id`, `slug`, `name`, `sequence`, `board_revision` FROM `team` WHERE `"+col+"` = ? LIMIT 1",
			val).Scan(&t.UUID, &t.Key, &t.Name, &t.Sequence, &t.BoardRevision)
		return t, err
	}

	t, err := scan("slug", slug)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return teamRef{}, err
	}
	t, err = scan("id", slug)
	if errors.Is(err, sql.ErrNoRows) {
		return teamRef{}, errTeamNotFound
	}
	if err != nil {
		return teamRef{}, err
	}
	return t, nil
}

// fail turns an internal error into a response without ever echoing the
// database's own message: driver text can carry a DSN, and a DSN carries a
// password.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error, what string) {
	if errors.Is(err, errTeamNotFound) {
		a.notFound(w, r)
		return
	}
	if r.Context().Err() != nil {
		// The client hung up mid-read. Nothing to report and nothing to log.
		return
	}
	a.logger.Warn("board read failed", zap.String("what", what), zap.Error(err))
	writeProblem(w, http.StatusInternalServerError, "internal error", "could not "+what)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// writeProblem matches the generated handlers' RFC 7807 error shape, so the
// API has one error format rather than two.
func writeProblem(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  title,
		"status": code,
		"detail": detail,
	})
}

// intQuery reads a bounded integer query parameter, falling back to def on
// anything it does not like. A board should not get a 400 for a stale bookmark.
func intQuery(r *http.Request, name string, def, min, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func int64Query(r *http.Request, name string) int64 {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
