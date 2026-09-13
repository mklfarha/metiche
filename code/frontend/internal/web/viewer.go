package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// Login configures signing in to the board with a link minted from a terminal
// (docs/BOARD_LOGIN.md). The board holds no credential of its own: a browser
// session is exchanged for a link by the backend, carried by a cookie, and
// relayed to the backend as a header on every read made for that viewer.
type Login struct {
	// Backend is the client for /v1/browser/* and /v1/teams/{slug}/access.
	// Required.
	Backend *feed.BrowserClient
	// NewViewerFeed builds the feed for one viewer's private board: a live
	// feed that sends session as X-Metiche-Browser-Session. Required.
	NewViewerFeed func(slug, session string) feed.Feed

	// BaseURL is the board's own public base URL. Its origin is the only one a
	// POST is accepted from. Required; https, except http://localhost and
	// http://127.0.0.1.
	BaseURL string
	// InsecureCookie names the cookie metiche_session and drops Secure, for
	// local development over plain http. Refused unless BaseURL is
	// http://localhost or http://127.0.0.1.
	InsecureCookie bool
	// TrustProxyHops is how many proxies in front of the board append to
	// X-Forwarded-For. 0 uses the connection's address; N takes the Nth entry
	// from the RIGHT, never the leftmost, which the client writes.
	TrustProxyHops int

	// Reauth is how often an open private-board stream re-checks its viewer's
	// session and access. Default 60s.
	Reauth time.Duration
	// SessionCacheTTL is how long a positive session check is believed.
	// Default 15s. Access to a team is never cached.
	SessionCacheTTL time.Duration
	// RefusalRecheck is the least time between two uncached re-validations of
	// one session made because the backend refused a viewer the cache had
	// signed in (see recheckRefused). Default 5s.
	RefusalRecheck time.Duration
	// MaxViewerBoards caps private boards held open for viewers. Default 200.
	MaxViewerBoards int
	// IdleGrace is how long a private board with no subscriber is kept, which
	// covers the gap between a page load and its stream connecting. Default 2m.
	IdleGrace time.Duration
	// SigninLimit sign-in attempts are allowed per client address per
	// SigninWindow. Default 30 per 10 minutes.
	SigninLimit  int
	SigninWindow time.Duration

	// Now is the clock for the session cache and the sign-in limiter. Default
	// time.Now.
	Now func() time.Time
}

const (
	// sessionCookieName is host-only by construction: a browser refuses a
	// __Host- cookie that is not Secure, not Path=/, or has a Domain.
	sessionCookieName = "__Host-metiche_session"
	// devSessionCookieName is the -dev-insecure-cookie spelling.
	devSessionCookieName = "metiche_session"
	// maxSessionAge is the absolute session lifetime (decision 2).
	maxSessionAge = 30 * 24 * time.Hour
	// maxCachedViewers bounds the session cache.
	maxCachedViewers = 10000
)

// Viewer is a signed-in browser. The secret is the session cookie's value: it
// is never logged, never rendered, and never part of a feed's Name.
type Viewer struct {
	SessionKey  string
	AccountKey  string
	DisplayName string

	secret string
	hash   string // hex sha256 of secret; the only form used as a map key
	// cached is true when this request was signed in by the session cache
	// rather than by a backend answer made for it.
	cached bool
}

// String keeps the secret out of any %v.
func (v *Viewer) String() string {
	return fmt.Sprintf("Viewer{session_key:%s account_key:%s}", v.SessionKey, v.AccountKey)
}

// CSRFToken is this viewer's synchronizer token (§6.3).
func (v *Viewer) CSRFToken() string { return csrfToken(v.secret) }

type viewerStatus int

const (
	viewerAnonymous   viewerStatus = iota // no cookie, or one the backend says is invalid
	viewerSignedIn                        // a valid session
	viewerUnavailable                     // a cookie the backend could not be asked about
)

type cachedViewer struct {
	v     Viewer
	until time.Time
}

type login struct {
	cfg        Login
	origin     string
	cookieName string
	secure     bool
	log        *slog.Logger

	mu    sync.Mutex
	cache map[string]cachedViewer // by hex sha256 of the secret, never the secret
	// rechecked is when each session was last re-validated by recheckRefused,
	// by the same hash.
	rechecked map[string]time.Time

	limiter *windowLimiter
	boards  *viewerBoards
}

// EnableLogin turns sign-in and private boards on. ctx is the lifetime of
// every private board. It refuses a configuration that would set a cookie a
// browser drops, or that would take POSTs from anywhere.
func (s *Server) EnableLogin(ctx context.Context, cfg Login) error {
	if cfg.Backend == nil || cfg.NewViewerFeed == nil {
		return errors.New("login needs a backend client and a viewer feed")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return fmt.Errorf("board base URL %q must be a bare scheme://host[:port]", cfg.BaseURL)
	}
	host := strings.ToLower(u.Hostname())
	local := host == "localhost" || host == "127.0.0.1"
	switch {
	case u.Scheme == "https":
		if cfg.InsecureCookie {
			return errors.New("-dev-insecure-cookie is refused unless the board's base URL is http://localhost or http://127.0.0.1")
		}
	case u.Scheme == "http" && local:
	case u.Scheme == "http" && cfg.InsecureCookie:
		return errors.New("-dev-insecure-cookie is refused unless the board's base URL is http://localhost or http://127.0.0.1")
	default:
		return fmt.Errorf("board base URL %q must be https (plain http only for localhost)", cfg.BaseURL)
	}
	if cfg.TrustProxyHops < 0 {
		return errors.New("trust-proxy-hops must not be negative")
	}
	if cfg.Reauth <= 0 {
		cfg.Reauth = 60 * time.Second
	}
	if cfg.SessionCacheTTL <= 0 {
		cfg.SessionCacheTTL = 15 * time.Second
	}
	if cfg.RefusalRecheck <= 0 {
		cfg.RefusalRecheck = 5 * time.Second
	}
	if cfg.MaxViewerBoards <= 0 {
		cfg.MaxViewerBoards = 200
	}
	if cfg.IdleGrace <= 0 {
		cfg.IdleGrace = 2 * time.Minute
	}
	if cfg.SigninLimit <= 0 {
		cfg.SigninLimit = 30
	}
	if cfg.SigninWindow <= 0 {
		cfg.SigninWindow = 10 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	l := &login{
		cfg:        cfg,
		origin:     u.Scheme + "://" + strings.ToLower(u.Host),
		cookieName: sessionCookieName,
		secure:     true,
		log:        s.log,
		cache:      map[string]cachedViewer{},
		rechecked:  map[string]time.Time{},
		limiter:    newWindowLimiter(cfg.SigninLimit, cfg.SigninWindow, cfg.Now),
	}
	if cfg.InsecureCookie {
		l.cookieName, l.secure = devSessionCookieName, false
	}
	l.boards = newViewerBoards(ctx, s, l)
	s.login = l
	go l.boards.reap(cfg.IdleGrace)
	return nil
}

// viewer resolves the request's viewer (§2.7 step 1).
//
//   - no cookie: anonymous;
//   - a cached positive answer younger than SessionCacheTTL: signed in;
//   - the backend says 200: signed in, and cached;
//   - the backend says 401: the cookie is cleared and the request continues
//     anonymously;
//   - anything else: unavailable, and the cookie is KEPT — an outage is not a
//     verdict about the session.
func (s *Server) viewer(w http.ResponseWriter, r *http.Request) (*Viewer, viewerStatus) {
	l := s.login
	if l == nil {
		return nil, viewerAnonymous
	}
	// What this response says depends on the cookie, whatever it turns out
	// to be.
	w.Header().Set("Vary", "Cookie")
	secret := l.cookieValue(r)
	if secret == "" {
		return nil, viewerAnonymous
	}
	// A request carrying a session is never cacheable by anybody but the
	// browser, and not by that either.
	privateHeaders(w)
	if !plausibleSecret(secret) {
		l.clearCookie(w)
		return nil, viewerAnonymous
	}
	hash := secretHash(secret)
	if v, ok := l.cached(hash); ok {
		v.cached = true
		return v, viewerSignedIn
	}
	bs, err := l.cfg.Backend.Session(r.Context(), secret)
	switch {
	case err == nil:
		v := &Viewer{SessionKey: bs.SessionKey, AccountKey: bs.AccountKey, DisplayName: bs.DisplayName,
			secret: secret, hash: hash}
		l.remember(v)
		return v, viewerSignedIn
	case errors.Is(err, feed.ErrSessionInvalid):
		l.sessionEnded(hash)
		l.clearCookie(w)
		return nil, viewerAnonymous
	default:
		s.log.Warn("could not validate a browser session; the viewer is unavailable, the cookie is kept", "err", err)
		return nil, viewerUnavailable
	}
}

func (l *login) cookieValue(r *http.Request) string {
	c, err := r.Cookie(l.cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// setCookie sets the session cookie (§2.6), one attribute at a time on
// purpose: each is load-bearing.
func (l *login) setCookie(w http.ResponseWriter, secret string, expires time.Time) {
	age := maxSessionAge
	if !expires.IsZero() {
		if until := expires.Sub(l.cfg.Now()); until < age {
			age = until
		}
	}
	if age < time.Second {
		age = time.Second
	}
	http.SetCookie(w, &http.Cookie{
		Name:     l.cookieName,
		Value:    secret,
		Path:     "/",
		Secure:   l.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(age / time.Second),
	})
}

// clearCookie deletes the session cookie (Max-Age=0) with the same attributes
// it was set with; a __Host- cookie is only replaced by one that qualifies.
func (l *login) clearCookie(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{
		Name:     l.cookieName,
		Value:    "",
		Path:     "/",
		Secure:   l.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (l *login) cached(hash string) (*Viewer, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.cache[hash]
	if !ok {
		return nil, false
	}
	if !l.cfg.Now().Before(c.until) {
		delete(l.cache, hash)
		return nil, false
	}
	v := c.v
	return &v, true
}

func (l *login) remember(v *Viewer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	if len(l.cache) >= maxCachedViewers {
		for k, c := range l.cache {
			if !now.Before(c.until) {
				delete(l.cache, k)
			}
		}
		if len(l.cache) >= maxCachedViewers {
			l.cache = map[string]cachedViewer{}
		}
	}
	l.cache[v.hash] = cachedViewer{v: *v, until: now.Add(l.cfg.SessionCacheTTL)}
}

// forget drops every cached session match selects, so a revocation made
// through this board is not outlived by the cache.
func (l *login) forget(match func(Viewer) bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, c := range l.cache {
		if match(c.v) {
			delete(l.cache, k)
		}
	}
}

// sessionEnded forgets a session and closes its private boards.
func (l *login) sessionEnded(hash string) {
	l.forget(func(v Viewer) bool { return v.hash == hash })
	l.boards.evictWhere("session ended", func(b *viewerBoard) bool { return b.hash == hash })
}

// refusal is what re-checking a refused viewer's session concluded.
type refusal int

const (
	refusalStands      refusal = iota // the session is good: the refusal is about the team
	refusalSessionOver                // the backend answered 401 for the session
	refusalUnavailable                // the session could not be checked
)

// recheckRefused decides what a refusal from the backend — a 404 or 401 from
// /access, or a 404 from a private board's first read — means for a viewer.
//
// The backend answers 404, not 401, to /access for a session that is no longer
// valid (app/authz folds every refusal into one answer), so a viewer the
// SESSION CACHE signed in may be refused because the session itself is over.
// Believing the cache would serve that 404 with the cookie kept until the
// cache entry expires. So the cache entry is dropped and the session is asked
// about once, uncached:
//
//   - 401: the session is over. It is forgotten, its private boards are
//     closed, and the caller clears the cookie in this same response;
//   - 200: the refusal stands and the cookie is kept (a non-member, or a
//     member removed from the team). The fresh answer is cached again;
//   - anything else: unavailable, and the cookie is kept.
//
// A viewer validated by the backend during this request is not re-checked
// for a 404: that answer is already fresh. A 401 is re-checked either way.
//
// It is the refusal path only, and it is rated per session: at most one
// re-validation per RefusalRecheck for one session, whatever the number of
// requests, slugs or tabs. A request refused inside that window keeps the
// verdict of the re-check that just ran (which re-cached the session), so it
// makes no backend call of its own; and a re-check never leads to another
// re-check, so there is no loop to amplify.
func (s *Server) recheckRefused(ctx context.Context, v *Viewer, status401 bool) refusal {
	l := s.login
	if !v.cached && !status401 {
		return refusalStands
	}
	if !l.mayRecheck(v.hash) {
		if status401 {
			// As before: the session is forgotten, so the very next request
			// asks the backend uncached and clears the cookie on its 401.
			l.sessionEnded(v.hash)
		}
		return refusalStands
	}
	l.forget(func(c Viewer) bool { return c.hash == v.hash })
	bs, err := l.cfg.Backend.Session(ctx, v.secret)
	switch {
	case err == nil:
		l.remember(&Viewer{SessionKey: bs.SessionKey, AccountKey: bs.AccountKey, DisplayName: bs.DisplayName,
			secret: v.secret, hash: v.hash})
		return refusalStands
	case errors.Is(err, feed.ErrSessionInvalid):
		l.sessionEnded(v.hash)
		return refusalSessionOver
	default:
		s.log.Warn("could not re-validate a refused browser session; the viewer is unavailable, the cookie is kept", "err", err)
		return refusalUnavailable
	}
}

// mayRecheck reports whether hash's session may be re-validated now, and if so
// records that it is.
func (l *login) mayRecheck(hash string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	if last, ok := l.rechecked[hash]; ok && now.Sub(last) < l.cfg.RefusalRecheck {
		return false
	}
	if len(l.rechecked) >= maxCachedViewers {
		for k, last := range l.rechecked {
			if now.Sub(last) >= l.cfg.RefusalRecheck {
				delete(l.rechecked, k)
			}
		}
		if len(l.rechecked) >= maxCachedViewers {
			l.rechecked = map[string]time.Time{}
		}
	}
	l.rechecked[hash] = now
	return true
}

// sameOrigin is the Origin / Sec-Fetch-Site check every state-changing POST
// passes (§6.3). With requireOrigin (a pre-session POST) a missing Origin is a
// refusal too.
func (l *login) sameOrigin(r *http.Request, requireOrigin bool) bool {
	switch o := r.Header.Get("Origin"); {
	case o == "":
		if requireOrigin {
			return false
		}
	case !strings.EqualFold(o, l.origin):
		return false
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		return false
	}
	return true
}

// reauthorize re-runs session validation and /access for a viewer's open
// private stream (§4.4). Anything but a valid session and a 200 for a private
// team ends the stream. A verdict (401, 404, or the team turned public)
// also closes the board; an outage only ends this stream.
func (s *Server) reauthorize(ctx context.Context, v *Viewer, t *Team) bool {
	l := s.login
	bs, err := l.cfg.Backend.Session(ctx, v.secret)
	if errors.Is(err, feed.ErrSessionInvalid) {
		l.sessionEnded(v.hash)
		return false
	}
	if err != nil {
		return false
	}
	l.remember(&Viewer{SessionKey: bs.SessionKey, AccountKey: bs.AccountKey, DisplayName: bs.DisplayName,
		secret: v.secret, hash: v.hash})
	acc, err := l.cfg.Backend.Access(ctx, v.secret, t.Slug)
	if err != nil && !errors.Is(err, feed.ErrNotFound) && !errors.Is(err, feed.ErrSessionInvalid) {
		return false
	}
	if err != nil || !acc.Private() {
		l.boards.evictWhere("access re-check refused", func(b *viewerBoard) bool { return b.team == t })
		return false
	}
	return true
}

// privateHeaders marks a response that depends on who is signed in.
func privateHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "private, no-store")
	h.Set("Vary", "Cookie")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
}

func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// plausibleSecret rejects, without a backend call, a cookie no backend could
// have minted.
func plausibleSecret(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// clientIP is the address sign-in limits key on. With hops = 0 it is the
// connection's peer. With hops = N it is the Nth X-Forwarded-For entry from
// the right — the one the Nth trusted proxy appended — and never the leftmost,
// which whoever sent the request wrote. Too few entries, or an entry that is
// not an address, falls back to the peer.
func clientIP(r *http.Request, hops int) string {
	peer := r.RemoteAddr
	if h, _, err := net.SplitHostPort(peer); err == nil {
		peer = h
	}
	if hops <= 0 {
		return peer
	}
	var entries []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) < hops {
		return peer
	}
	ip := net.ParseIP(entries[len(entries)-hops])
	if ip == nil {
		return peer
	}
	return ip.String()
}

// ipHint is the truncated prefix stored with a session (decision 6): IPv4 /24,
// IPv6 /48. It is empty unless the board knows the real client address, which
// is only when it trusts a proxy hop.
func ipHint(r *http.Request, hops int) string {
	if hops <= 0 {
		return ""
	}
	ip := net.ParseIP(clientIP(r, hops))
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return (&net.IPNet{IP: v4.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}).String()
	}
	return (&net.IPNet{IP: ip.Mask(net.CIDRMask(48, 128)), Mask: net.CIDRMask(48, 128)}).String()
}

// userAgentHint is the user agent truncated to 200 printable characters.
func userAgentHint(ua string) string {
	var b strings.Builder
	for _, c := range ua {
		if b.Len() >= 200 {
			break
		}
		if c >= 0x20 && c < 0x7f {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// windowLimiter is a fixed-window counter per key, bounded in size.
type windowLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	hits   map[string]windowCount
}

type windowCount struct {
	start time.Time
	n     int
}

func newWindowLimiter(limit int, window time.Duration, now func() time.Time) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, now: now, hits: map[string]windowCount{}}
}

func (l *windowLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	c, ok := l.hits[key]
	if !ok || now.Sub(c.start) >= l.window {
		if len(l.hits) >= maxCachedViewers {
			for k, old := range l.hits {
				if now.Sub(old.start) >= l.window {
					delete(l.hits, k)
				}
			}
			if len(l.hits) >= maxCachedViewers {
				l.hits = map[string]windowCount{}
			}
		}
		c = windowCount{start: now}
	}
	if c.n >= l.limit {
		l.hits[key] = c
		return false
	}
	c.n++
	l.hits[key] = c
	return true
}
