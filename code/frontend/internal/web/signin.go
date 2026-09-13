package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/view"
)

// ---------------------------------------------------------------- pages

// withViewer marks r's render as signed in when v is. The topbar indicator,
// the sign-out form, the CSRF meta tag and NotFound's indicator all read it
// (view.ViewerFrom). The session secret never reaches a view; only the token
// derived from it does.
func (s *Server) withViewer(r *http.Request, v *Viewer) *http.Request {
	if v == nil {
		return r
	}
	return r.WithContext(view.WithViewer(r.Context(), view.Viewer{
		SignedIn: true, DisplayName: v.DisplayName, CSRFToken: v.CSRFToken(),
	}))
}

// accountSessions adapts the backend's session list for the account page and
// marks this browser's row.
func accountSessions(list []feed.SessionListing, current string) []view.AccountSession {
	out := make([]view.AccountSession, 0, len(list))
	for _, l := range list {
		out = append(out, view.AccountSession{
			Key: l.Key, UserAgent: l.UserAgent, IPHint: l.IPHint,
			CreatedAt: l.CreatedAt, LastSeenAt: l.LastSeenAt, ExpiresAt: l.ExpiresAt,
			Current: l.Current || l.Key == current,
		})
	}
	return out
}

// yourTeams adapts GET /v1/browser/teams for /teams.
func yourTeams(list []feed.BrowserTeam) []view.YourTeam {
	out := make([]view.YourTeam, 0, len(list))
	for _, t := range list {
		out = append(out, view.YourTeam{Slug: t.Slug, Name: t.Name, Visibility: t.Visibility, Role: t.Role})
	}
	return out
}

// ---------------------------------------------------------------- CSRF

// csrfToken is base64url(HMAC-SHA256(key = session secret, msg =
// "metiche-csrf-v1")): stateless, and it dies with the session (§6.3).
func csrfToken(secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("metiche-csrf-v1"))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// csrfOK compares the request's token — X-CSRF-Token (htmx) or the csrf form
// field — in constant time.
func csrfOK(r *http.Request, secret string) bool {
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.PostFormValue("csrf")
	}
	want := csrfToken(secret)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ---------------------------------------------------------------- headers

// signinHeaders are the /signin headers (§2.5, §6.4). The page loads only
// same-origin script, so a link secret it reads cannot reach anybody else.
func signinHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
}

// baseHeaders go on every board response (§6.4): board URLs name slugs.
func baseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- /signin

// linkPattern is what open_board mints: "mbl_" + base64url(32 bytes).
var linkPattern = regexp.MustCompile(`^mbl_[A-Za-z0-9_-]{16,128}$`)

// sessionKeyPattern bounds a session key taken from a URL.
var sessionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)

// safeRedirect accepts only what the backend may store: "/", "/teams" or
// "/t/<slug>". Anything else goes to "/", so a stored path can never become
// an open redirect.
func safeRedirect(p string) string {
	if p == "/" || p == "/teams" {
		return p
	}
	if slug, ok := strings.CutPrefix(p, "/t/"); ok && ValidSlug(slug) {
		return p
	}
	return "/"
}

func (s *Server) signinPage(w http.ResponseWriter, r *http.Request) {
	signinHeaders(w)
	s.render(w, r, view.SignInPage())
}

// signinPost redeems a sign-in link (§2.5). The secret comes from the body
// only — never the query string — and goes to the backend in a JSON body.
func (s *Server) signinPost(w http.ResponseWriter, r *http.Request) {
	signinHeaders(w)
	l := s.login
	if l == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	if !l.sameOrigin(r, true) {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if !l.limiter.allow(clientIP(r, l.cfg.TrustProxyHops)) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	link := r.PostFormValue("link")
	if !linkPattern.MatchString(link) {
		writeJSONStatus(w, http.StatusOK, map[string]string{"error": "link"})
		return
	}
	ex, err := l.cfg.Backend.Exchange(r.Context(), link, userAgentHint(r.UserAgent()), ipHint(r, l.cfg.TrustProxyHops))
	switch {
	case errors.Is(err, feed.ErrLinkRefused):
		writeJSONStatus(w, http.StatusOK, map[string]string{"error": "link"})
		return
	case err != nil:
		s.log.Warn("sign-in exchange failed", "err", err)
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}

	// A cookie this browser already had is never reused (session fixation):
	// the new session replaces it, and the old one is revoked best-effort.
	if old := l.cookieValue(r); old != "" && old != ex.SessionSecret && plausibleSecret(old) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		_ = l.cfg.Backend.SignOut(ctx, old)
		cancel()
		l.sessionEnded(secretHash(old))
	}

	l.setCookie(w, ex.SessionSecret, ex.ExpiresAt)
	l.remember(&Viewer{SessionKey: ex.SessionKey, AccountKey: ex.AccountKey, DisplayName: ex.DisplayName,
		secret: ex.SessionSecret, hash: secretHash(ex.SessionSecret)})
	writeJSONStatus(w, http.StatusOK, map[string]string{"redirect": safeRedirect(ex.RedirectPath)})
}

// ---------------------------------------------------------------- sign out

// postGuard runs the checks every session POST shares: login on, same origin,
// a session cookie, and its CSRF token. It returns the cookie's secret, or ""
// after writing the response.
func (s *Server) postGuard(w http.ResponseWriter, r *http.Request) string {
	l := s.login
	if l == nil {
		http.NotFound(w, r)
		return ""
	}
	privateHeaders(w)
	if !l.sameOrigin(r, false) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return ""
	}
	secret := l.cookieValue(r)
	if secret == "" {
		// Nothing to sign out of, and nothing a forged request could do.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return ""
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !csrfOK(r, secret) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return ""
	}
	return secret
}

func redirectAfterPost(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", to)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// signout is POST /signout: revoke this session, clear the cookie, go home.
func (s *Server) signout(w http.ResponseWriter, r *http.Request) {
	secret := s.postGuard(w, r)
	if secret == "" {
		return
	}
	l := s.login
	if err := l.cfg.Backend.SignOut(r.Context(), secret); err != nil && !errors.Is(err, feed.ErrSessionInvalid) {
		s.log.Warn("sign-out failed; the cookie is kept", "err", err)
		http.Error(w, "metiche is unavailable right now; try again shortly", http.StatusServiceUnavailable)
		return
	}
	l.sessionEnded(secretHash(secret))
	l.clearCookie(w)
	redirectAfterPost(w, r, "/")
}

// account is GET /account.
func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	if s.login == nil {
		http.NotFound(w, r)
		return
	}
	v, vs := s.viewer(w, r)
	switch vs {
	case viewerUnavailable:
		http.Error(w, "metiche is unavailable right now; try again shortly", http.StatusServiceUnavailable)
		return
	case viewerAnonymous:
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
		return
	}
	sessions, err := s.login.cfg.Backend.Sessions(r.Context(), v.secret)
	switch {
	case errors.Is(err, feed.ErrSessionInvalid):
		s.login.sessionEnded(v.hash)
		s.login.clearCookie(w)
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
		return
	case err != nil:
		s.log.Warn("listing browser sessions failed", "err", err)
		http.Error(w, "metiche is unavailable right now; try again shortly", http.StatusServiceUnavailable)
		return
	}
	r = s.withViewer(r, v)
	s.render(w, r, view.AccountPage(view.AccountParams{Sessions: accountSessions(sessions, v.SessionKey)}))
}

// signoutAll is POST /account/signout-all.
func (s *Server) signoutAll(w http.ResponseWriter, r *http.Request) {
	secret := s.postGuard(w, r)
	if secret == "" {
		return
	}
	l := s.login
	v, vs := s.viewer(w, r)
	switch vs {
	case viewerUnavailable:
		http.Error(w, "metiche is unavailable right now; try again shortly", http.StatusServiceUnavailable)
		return
	case viewerAnonymous:
		redirectAfterPost(w, r, "/")
		return
	}
	if err := l.cfg.Backend.SignOutEverywhere(r.Context(), secret); err != nil && !errors.Is(err, feed.ErrSessionInvalid) {
		s.log.Warn("sign-out everywhere failed; the cookie is kept", "err", err)
		http.Error(w, "metiche is unavailable right now; try again shortly", http.StatusServiceUnavailable)
		return
	}
	account := v.AccountKey
	l.forget(func(c Viewer) bool { return c.AccountKey == account })
	l.boards.evictWhere("signed out everywhere", func(b *viewerBoard) bool { return b.account == account })
	l.clearCookie(w)
	redirectAfterPost(w, r, "/")
}

// revokeSession is POST /account/sessions/{key}/revoke.
func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) {
	secret := s.postGuard(w, r)
	if secret == "" {
		return
	}
	l := s.login
	key := chi.URLParam(r, "key")
	if !sessionKeyPattern.MatchString(key) {
		http.NotFound(w, r)
		return
	}
	v, vs := s.viewer(w, r)
	switch vs {
	case viewerUnavailable:
		http.Error(w, "metiche is unavailable right now; try again shortly", http.StatusServiceUnavailable)
		return
	case viewerAnonymous:
		redirectAfterPost(w, r, "/signin")
		return
	}
	switch err := l.cfg.Backend.Revoke(r.Context(), secret, key); {
	case errors.Is(err, feed.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, feed.ErrSessionInvalid):
		l.sessionEnded(v.hash)
		l.clearCookie(w)
		redirectAfterPost(w, r, "/signin")
		return
	case err != nil:
		s.log.Warn("revoking a browser session failed", "err", err)
		http.Error(w, "metiche is unavailable right now; try again shortly", http.StatusServiceUnavailable)
		return
	}
	l.forget(func(c Viewer) bool { return c.SessionKey == key })
	l.boards.evictWhere("session revoked", func(b *viewerBoard) bool { return b.key.session == key })
	if key == v.SessionKey {
		l.clearCookie(w)
		redirectAfterPost(w, r, "/")
		return
	}
	redirectAfterPost(w, r, "/account")
}
