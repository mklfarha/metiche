package browser

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/core"
	"github.com/mklfarha/metiche/backend/enums"
)

// Route patterns, for app/rest.go's AllowedRoutes. Board only; NOT routed by
// any ingress: the board (metiche-web) reaches them in-cluster, exactly as it
// reaches /v1/teams/{slug}.
const (
	// PathSessions: POST exchange, GET list, DELETE sign out everywhere.
	PathSessions = "/v1/browser/sessions"
	// PathSession: GET the current session, DELETE sign out.
	PathSession = "/v1/browser/session"
	// PathSessionKey: DELETE one session of the caller's account.
	PathSessionKey = "/v1/browser/sessions/{key}"
	// PathTeams: GET the caller's teams.
	PathTeams = "/v1/browser/teams"
)

const (
	// ExchangePerHourEnv is the process-wide exchange backstop (§2.4).
	ExchangePerHourEnv     = "METICHE_BROWSER_EXCHANGE_PER_HOUR"
	defaultExchangePerHour = 600
	// maxExchangeBody bounds the exchange request body. A link secret is 47
	// bytes and the user agent is truncated to 200 characters anyway.
	maxExchangeBody = 8 << 10
)

// API serves /v1/browser/*.
//
// # HTTP shapes
//
// Every response carries Cache-Control: no-store. Errors are RFC 7807
// problem+json, like app/webapi.
//
//	POST   /v1/browser/sessions        body {"link_secret","user_agent","ip_hint"}
//	       201 {"session_secret","session_key","expires_at","redirect_path","account_key","display_name"}
//	       404 {"title":"not found","detail":"no such sign-in link"}  (unknown, used, expired, retired agent, inactive account, malformed body)
//	       503 database failure, or the process-wide backstop (then with Retry-After)
//
// Every other route authenticates with X-Metiche-Browser-Session: <mbs_...>
// and no other credential header (a request carrying Authorization or
// X-Metiche-Token as well is refused: one request, one principal):
//
//	GET    /v1/browser/session         200 {"session_key","account_key","display_name","expires_at"}
//	DELETE /v1/browser/session         200 {"revoked":1}                     end_reason signed_out
//	GET    /v1/browser/sessions        200 {"sessions":[SessionInfo...]}
//	DELETE /v1/browser/sessions        200 {"revoked":N}                     end_reason signed_out_everywhere (includes this one)
//	DELETE /v1/browser/sessions/{key}  200 {"revoked":0|1}                   end_reason revoked
//	                                   404 {"title":"not found","detail":"no such browser session key"}  (not this account's)
//	GET    /v1/browser/teams           200 {"teams":[TeamSummary...]}
//
//	401 {"title":"unauthorized","detail":"no such browser session"}  every session refusal, one body
//	503 {"title":"unavailable","detail":"browser sign-in is unavailable"}  database failure; the board keeps the cookie
type API struct {
	db     *sql.DB
	logger *zap.Logger

	// exchangeLimit is the process-wide backstop on exchanges. A field so a
	// test can shrink it.
	exchangeLimit *windowLimiter
}

// NewAPI builds the handlers over a database handle.
func NewAPI(db *sql.DB, logger *zap.Logger) *API {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &API{
		db:            db,
		logger:        logger,
		exchangeLimit: newWindowLimiter(envInt(ExchangePerHourEnv, defaultExchangePerHour), time.Hour),
	}
}

// RegisterOn mounts the routes on the ROOT router with full /v1 paths (see the
// contract in app/rest.go: no r.Route("/v1"), no r.Use).
func (a *API) RegisterOn(r chi.Router) {
	r.Post(PathSessions, noStore(a.handleExchange))
	r.Get(PathSessions, noStore(a.withViewer(a.handleList)))
	r.Delete(PathSessions, noStore(a.withViewer(a.handleSignOutEverywhere)))
	r.Get(PathSession, noStore(a.withViewer(a.handleCurrent)))
	r.Delete(PathSession, noStore(a.handleSignOut))
	r.Delete(PathSessionKey, noStore(a.withViewer(a.handleRevokeKey)))
	r.Get(PathTeams, noStore(a.withViewer(a.handleTeams)))
}

// Register wires the API into the REST server.
func Register(r chi.Router, coreImpl *core.Implementation, logger *zap.Logger) *API {
	a := NewAPI(coreImpl.DB(), logger)
	a.RegisterOn(r)
	return a
}

func noStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

// ── exchange ────────────────────────────────────────────────────────────────

func (a *API) handleExchange(w http.ResponseWriter, r *http.Request) {
	if ok, retry := a.exchangeLimit.allow("browser-exchange"); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retry.Seconds()))))
		a.unavailable(w)
		return
	}
	var req ExchangeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxExchangeBody))
	if err := dec.Decode(&req); err != nil {
		// A malformed body is the same refusal as a bad link: nothing about
		// the request's shape is worth an oracle.
		a.linkNotFound(w)
		return
	}
	out, err := Exchange(r.Context(), a.db, req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, out)
	case errors.Is(err, ErrLinkNotUsable):
		a.linkNotFound(w)
	default:
		a.fail(w, r, err, "exchanging a sign-in link")
	}
}

// ── session-authenticated routes ────────────────────────────────────────────

type viewerHandler func(w http.ResponseWriter, r *http.Request, v Viewer)

// sessionSecret is the only door a session secret comes through: one header.
// A request that also carries a bearer is refused (§4.1: one principal).
func sessionSecret(r *http.Request) string {
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" || strings.TrimSpace(r.Header.Get("X-Metiche-Token")) != "" {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(HeaderSession))
}

func (a *API) withViewer(next viewerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := ValidateSession(r.Context(), a.db, sessionSecret(r))
		if err != nil {
			a.sessionError(w, r, err)
			return
		}
		next(w, r, v)
	}
}

func (a *API) handleCurrent(w http.ResponseWriter, _ *http.Request, v Viewer) {
	writeJSON(w, http.StatusOK, map[string]any{
		"session_key":  v.SessionKey,
		"account_key":  v.AccountKey,
		"display_name": v.DisplayName,
		"expires_at":   v.ExpiresAt,
	})
}

func (a *API) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if err := SignOut(r.Context(), a.db, sessionSecret(r)); err != nil {
		a.sessionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": 1})
}

func (a *API) handleSignOutEverywhere(w http.ResponseWriter, r *http.Request, v Viewer) {
	n, err := Revoke(r.Context(), a.db, v.AccountUUID, "", enums.BROWSER_SESSION_END_REASON_SIGNED_OUT_EVERYWHERE)
	if err != nil {
		a.fail(w, r, err, "signing out everywhere")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

func (a *API) handleRevokeKey(w http.ResponseWriter, r *http.Request, v Viewer) {
	n, err := Revoke(r.Context(), a.db, v.AccountUUID, chi.URLParam(r, "key"), enums.BROWSER_SESSION_END_REASON_REVOKED)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
	case errors.Is(err, ErrSessionNotFound):
		writeProblem(w, http.StatusNotFound, "not found", ErrSessionNotFound.Error())
	default:
		a.fail(w, r, err, "revoking a browser session")
	}
}

func (a *API) handleList(w http.ResponseWriter, r *http.Request, v Viewer) {
	list, err := ListSessions(r.Context(), a.db, v.AccountUUID, v.SessionUUID)
	if err != nil {
		a.fail(w, r, err, "listing browser sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (a *API) handleTeams(w http.ResponseWriter, r *http.Request, v Viewer) {
	teams, err := ListTeams(r.Context(), a.db, v.AccountUUID)
	if err != nil {
		a.fail(w, r, err, "listing teams")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
}

// ── responses ───────────────────────────────────────────────────────────────

func (a *API) linkNotFound(w http.ResponseWriter) {
	writeProblem(w, http.StatusNotFound, "not found", ErrLinkNotUsable.Error())
}

// sessionError renders a session refusal as the one 401, and anything else
// (a database failure) as 503 — never 401, or the board clears a good cookie.
func (a *API) sessionError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnauthenticated) {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", ErrUnauthenticated.Error())
		return
	}
	a.fail(w, r, err, "checking a browser session")
}

// fail is every non-refusal error: 503, logged, never echoed (driver text can
// carry a DSN, and no error in this package carries a secret).
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error, what string) {
	if r.Context().Err() == nil {
		a.logger.Warn("browser session request failed", zap.String("what", what), zap.Error(err))
	}
	a.unavailable(w)
}

func (a *API) unavailable(w http.ResponseWriter) {
	writeProblem(w, http.StatusServiceUnavailable, "unavailable", ErrUnavailable.Error())
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// writeProblem matches app/webapi's (and the generated handlers') RFC 7807
// shape.
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

// ── the exchange backstop ───────────────────────────────────────────────────

// windowLimiter is a fixed-window counter, the same shape as
// app/mcp.RateLimiter, restated here because this package must not import
// app/mcp. It has one key in production.
type windowLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	count  map[string]int
	reset  map[string]time.Time
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, count: map[string]int{}, reset: map[string]time.Time{}}
}

func (l *windowLimiter) allow(key string) (bool, time.Duration) {
	if l == nil || l.limit <= 0 {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.reset[key]) {
		l.count[key], l.reset[key] = 0, now.Add(l.window)
	}
	if l.count[key] >= l.limit {
		return false, l.reset[key].Sub(now)
	}
	l.count[key]++
	return true, 0
}

// envInt reads a non-negative integer from the environment; 0 disables the
// limit, as in app/mcp.
func envInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}
