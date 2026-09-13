package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// BrowserSessionHeader carries a signed-in viewer's browser session from the
// board to the backend. The backend reads credentials from headers only; the
// session never travels in a URL, where access logs would keep it.
const BrowserSessionHeader = "X-Metiche-Browser-Session"

var (
	// ErrSessionInvalid means the backend answered 401 for a browser session:
	// unknown, expired, revoked, its origin agent retired, or its account
	// inactive. It is the ONLY answer that may clear a viewer's cookie.
	ErrSessionInvalid = errors.New("browser session is not valid")
	// ErrLinkRefused means the backend refused a sign-in link. Unknown, used,
	// expired, retired agent and inactive account are one answer.
	ErrLinkRefused = errors.New("sign-in link refused")
)

// BrowserClient talks to the backend's in-cluster browser-session API
// (/v1/browser/*) and to /v1/teams/{slug}/access.
//
// Every other status than the ones each method names is returned as a plain
// error, which callers treat as "unavailable" — an outage is not a verdict
// about a session or a team. No error ever carries a response body, a link
// secret or a session secret.
type BrowserClient struct {
	// BaseURL is the backend root, with or without the /v1 suffix.
	BaseURL string
	Client  *http.Client
	// Timeout bounds each call. Default 5s.
	Timeout time.Duration
}

// Exchanged is the backend's answer to a redeemed sign-in link.
type Exchanged struct {
	SessionSecret string    `json:"session_secret"`
	SessionKey    string    `json:"session_key"`
	ExpiresAt     time.Time `json:"expires_at"`
	RedirectPath  string    `json:"redirect_path"`
	AccountKey    string    `json:"account_key"`
	DisplayName   string    `json:"display_name"`
}

// String keeps the secret out of any %v a caller might log.
func (e Exchanged) String() string {
	return fmt.Sprintf("Exchanged{session_key:%s account_key:%s}", e.SessionKey, e.AccountKey)
}

// BrowserSession is GET /v1/browser/session: who a valid session belongs to.
type BrowserSession struct {
	SessionKey  string    `json:"session_key"`
	AccountKey  string    `json:"account_key"`
	DisplayName string    `json:"display_name"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// TeamAccess is GET /v1/teams/{slug}/access.
type TeamAccess struct {
	Visibility string `json:"visibility"` // "public" | "private"
	Role       string `json:"role"`       // "owner" | "member" | "" (null)
}

// Private reports whether the team is private.
func (a TeamAccess) Private() bool { return a.Visibility == "private" }

// SessionListing is one row of GET /v1/browser/sessions. Never a secret or a
// hash: the backend does not send them, and this type has nowhere to put one.
type SessionListing struct {
	Key        string     `json:"key"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UserAgent  string     `json:"user_agent"`
	IPHint     string     `json:"ip_hint"`
	AuthMethod string     `json:"auth_method"`
	Current    bool       `json:"current"`
}

// BrowserTeam is one row of GET /v1/browser/teams.
type BrowserTeam struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	Role       string `json:"role"`
}

// Exchange redeems a sign-in link: POST /v1/browser/sessions with the secret
// in a JSON body, never a query string.
func (c *BrowserClient) Exchange(ctx context.Context, linkSecret, userAgent, ipHint string) (Exchanged, error) {
	body := map[string]string{"link_secret": linkSecret, "user_agent": userAgent, "ip_hint": ipHint}
	status, raw, err := c.call(ctx, http.MethodPost, "/browser/sessions", "", body)
	if err != nil {
		return Exchanged{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		var out Exchanged
		if err := json.Unmarshal(raw, &out); err != nil || out.SessionSecret == "" {
			return Exchanged{}, errors.New("POST /v1/browser/sessions: malformed answer")
		}
		return out, nil
	case http.StatusNotFound:
		return Exchanged{}, ErrLinkRefused
	default:
		return Exchanged{}, statusError(http.MethodPost, "/v1/browser/sessions", status)
	}
}

// Session validates a session: GET /v1/browser/session.
func (c *BrowserClient) Session(ctx context.Context, secret string) (BrowserSession, error) {
	var out BrowserSession
	err := c.getJSON(ctx, "/browser/session", secret, &out, nil)
	return out, err
}

// Access asks whether this viewer may read slug: GET /v1/teams/{slug}/access.
// 404 is ErrNotFound (no such team, or not for this viewer — one answer).
func (c *BrowserClient) Access(ctx context.Context, secret, slug string) (TeamAccess, error) {
	var out TeamAccess
	err := c.getJSON(ctx, "/teams/"+url.PathEscape(slug)+"/access", secret, &out, ErrNotFound)
	return out, err
}

// Sessions lists the account's live browser sessions: GET /v1/browser/sessions.
func (c *BrowserClient) Sessions(ctx context.Context, secret string) ([]SessionListing, error) {
	var out struct {
		Sessions []SessionListing `json:"sessions"`
	}
	err := c.getJSON(ctx, "/browser/sessions", secret, &out, nil)
	return out.Sessions, err
}

// Teams lists the teams the account is a live member of: GET /v1/browser/teams.
func (c *BrowserClient) Teams(ctx context.Context, secret string) ([]BrowserTeam, error) {
	var out struct {
		Teams []BrowserTeam `json:"teams"`
	}
	err := c.getJSON(ctx, "/browser/teams", secret, &out, nil)
	return out.Teams, err
}

// SignOut revokes this session: DELETE /v1/browser/session.
func (c *BrowserClient) SignOut(ctx context.Context, secret string) error {
	return c.del(ctx, "/browser/session", secret, nil)
}

// SignOutEverywhere revokes every session of the account: DELETE
// /v1/browser/sessions.
func (c *BrowserClient) SignOutEverywhere(ctx context.Context, secret string) error {
	return c.del(ctx, "/browser/sessions", secret, nil)
}

// Revoke revokes one session of the caller's account by its public key:
// DELETE /v1/browser/sessions/{key}. Another account's key is ErrNotFound.
func (c *BrowserClient) Revoke(ctx context.Context, secret, key string) error {
	return c.del(ctx, "/browser/sessions/"+url.PathEscape(key), secret, ErrNotFound)
}

func (c *BrowserClient) getJSON(ctx context.Context, path, secret string, v any, on404 error) error {
	status, raw, err := c.call(ctx, http.MethodGet, path, secret, nil)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusOK:
		if err := json.Unmarshal(raw, v); err != nil {
			return fmt.Errorf("GET /v1%s: malformed answer", redactPath(path))
		}
		return nil
	case status == http.StatusUnauthorized:
		return ErrSessionInvalid
	case status == http.StatusNotFound && on404 != nil:
		return on404
	default:
		return statusError(http.MethodGet, "/v1"+redactPath(path), status)
	}
}

func (c *BrowserClient) del(ctx context.Context, path, secret string, on404 error) error {
	status, _, err := c.call(ctx, http.MethodDelete, path, secret, nil)
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized:
		return ErrSessionInvalid
	case status == http.StatusNotFound && on404 != nil:
		return on404
	default:
		return statusError(http.MethodDelete, "/v1"+redactPath(path), status)
	}
}

// call makes one request and reads the whole (bounded) body inside the
// timeout. The body is returned for decoding only — never for an error.
func (c *BrowserClient) call(ctx context.Context, method, path, secret string, in any) (int, []byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	}
	root := (&Live{BaseURL: c.BaseURL}).root()
	req, err := http.NewRequestWithContext(ctx, method, root+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("%s /v1%s: bad request", method, redactPath(path))
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set(BrowserSessionHeader, secret)
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// A url.Error names the URL, which holds no secret here; keep only
		// the method and path anyway.
		return 0, nil, fmt.Errorf("%s /v1%s: backend unreachable", method, redactPath(path))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%s /v1%s: reading the answer failed", method, redactPath(path))
	}
	return resp.StatusCode, raw, nil
}

// redactPath drops the per-team and per-session segments: a slug may name a
// private team, and neither belongs in a log line.
func redactPath(path string) string {
	switch {
	case len(path) > len("/teams/") && path[:len("/teams/")] == "/teams/":
		return "/teams/{slug}/access"
	case len(path) > len("/browser/sessions/") && path[:len("/browser/sessions/")] == "/browser/sessions/":
		return "/browser/sessions/{key}"
	}
	return path
}

func statusError(method, path string, status int) error {
	return fmt.Errorf("%s %s: backend answered %d", method, path, status)
}
