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
	"strings"
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
//
// The list includes sessions that already ended or expired, until the sweeper
// deletes them, so "why was I signed out" has an answer. Only State "live" is
// a signed-in browser.
type SessionListing struct {
	Key        string     `json:"key"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UserAgent  string     `json:"user_agent"`
	IPHint     string     `json:"ip_hint"`
	AuthMethod string     `json:"auth_method"`
	Current    bool       `json:"current"`
	// State is "live", "ended" (revoked_at is set) or "expired".
	State string `json:"state"`
	// RevokedAt is when an ended session was ended; nil otherwise.
	RevokedAt *time.Time `json:"revoked_at"`
	// EndReason is why it ended: "signed_out", "signed_out_everywhere",
	// "revoked", "revoked_by_agent", "replaced", or "" when it has not.
	EndReason string `json:"end_reason"`
}

// Session states in GET /v1/browser/sessions.
const (
	SessionLive    = "live"
	SessionEnded   = "ended"
	SessionExpired = "expired"
)

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

// Sessions lists the account's browser sessions, newest first, including the
// ended and expired ones still in the table: GET /v1/browser/sessions.
// Callers filter on State.
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

// ── invites (docs/BOARD_LOGIN.md §10.10) ────────────────────────────────────

// ErrInviteNotFound is the backend's 404 for an invite this viewer may not
// revoke: another member's, another team's, one that never existed. It is
// told apart from ErrNotFound (the team, for this viewer) by the problem's
// title, which the backend sets to the refusal code "not_found".
var ErrInviteNotFound = errors.New("no invite with that id that you can revoke")

// InviteRefused is a create or revoke the backend refused on its merits: a cap,
// a bad value, the per-account rate limit. Message is the backend's own text,
// for the page; Error() is only the code, for a log line.
type InviteRefused struct {
	Status  int
	Code    string // "invalid_argument" | "not_permitted" | "rate_limited"
	Message string
}

func (e *InviteRefused) Error() string { return "invite refused: " + e.Code }

// Invite is one row of GET /v1/teams/{slug}/invites. It has no field for a
// code: the backend never sends one there.
type Invite struct {
	InviteID     string     `json:"invite_id"`
	Label        string     `json:"label"`
	CreatedBy    string     `json:"created_by"`
	CreatedByYou bool       `json:"created_by_you"`
	Uses         int64      `json:"uses"`
	MaxUses      *int64     `json:"max_uses"`   // nil: uncapped
	ExpiresAt    *time.Time `json:"expires_at"` // nil: never
	State        string     `json:"state"`      // active | expired | exhausted | revoked
	LastUsedAt   *time.Time `json:"last_used_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

// InviteList is GET /v1/teams/{slug}/invites.
type InviteList struct {
	TeamSlug string   `json:"team_slug"`
	Scope    string   `json:"scope"` // "team" (the owner) | "created_by_you" (a member)
	Invites  []Invite `json:"invites"`
	More     bool     `json:"more"`
}

// Owner reports whether the list is the whole team's, which only the owner
// gets.
func (l InviteList) Owner() bool { return l.Scope == "team" }

// InviteRequest is POST /v1/teams/{slug}/invites. Zero means the default.
type InviteRequest struct {
	Label          string `json:"label"`
	MaxUses        int    `json:"max_uses"`
	ExpiresInHours int    `json:"expires_in_hours"`
}

// CreatedInvite is the POST's answer: the only place a code ever appears.
type CreatedInvite struct {
	InviteID  string    `json:"invite_id"`
	Label     string    `json:"label"`
	Code      string    `json:"code"`
	MaxUses   int64     `json:"max_uses"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	ShareNote string    `json:"share_note"`
}

// String keeps the code out of any %v a caller might log.
func (c CreatedInvite) String() string {
	return fmt.Sprintf("CreatedInvite{invite_id:%s max_uses:%d}", c.InviteID, c.MaxUses)
}

// Invites lists what this viewer may see of a team's invites. 404 is
// ErrNotFound: no such team, or this viewer is not a live member (one answer).
func (c *BrowserClient) Invites(ctx context.Context, secret, slug string) (InviteList, error) {
	var out InviteList
	err := c.getJSON(ctx, "/teams/"+url.PathEscape(slug)+"/invites", secret, &out, ErrNotFound)
	return out, err
}

// CreateInvite mints an invite. The code is in the answer and nowhere else.
func (c *BrowserClient) CreateInvite(ctx context.Context, secret, slug string, in InviteRequest) (CreatedInvite, error) {
	path := "/teams/" + url.PathEscape(slug) + "/invites"
	status, raw, err := c.call(ctx, http.MethodPost, path, secret, in)
	if err != nil {
		return CreatedInvite{}, err
	}
	if status == http.StatusCreated {
		var out CreatedInvite
		if err := json.Unmarshal(raw, &out); err != nil || out.Code == "" {
			return CreatedInvite{}, fmt.Errorf("POST /v1%s: malformed answer", redactPath(path))
		}
		return out, nil
	}
	return CreatedInvite{}, inviteError(http.MethodPost, path, status, raw)
}

// RevokeInvite ends one invite.
func (c *BrowserClient) RevokeInvite(ctx context.Context, secret, slug, inviteID string) error {
	path := "/teams/" + url.PathEscape(slug) + "/invites/" + url.PathEscape(inviteID)
	status, raw, err := c.call(ctx, http.MethodDelete, path, secret, nil)
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return inviteError(http.MethodDelete, path, status, raw)
}

// inviteError maps a non-2xx invite answer. Only a problem whose title is a
// refusal code is a verdict; anything else is "unavailable" to the caller.
func inviteError(method, path string, status int, raw []byte) error {
	var p struct{ Title, Detail string }
	_ = json.Unmarshal(raw, &p)
	switch {
	case status == http.StatusUnauthorized:
		return ErrSessionInvalid
	case status == http.StatusNotFound && p.Title == "not_found":
		return ErrInviteNotFound
	case status == http.StatusNotFound:
		return ErrNotFound
	case (status == http.StatusBadRequest || status == http.StatusForbidden || status == http.StatusTooManyRequests) &&
		p.Title != "" && p.Detail != "":
		return &InviteRefused{Status: status, Code: p.Title, Message: p.Detail}
	}
	return statusError(method, "/v1"+redactPath(path), status)
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
		_, sub, _ := strings.Cut(path[len("/teams/"):], "/")
		switch {
		case sub == "invites":
			return "/teams/{slug}/invites"
		case strings.HasPrefix(sub, "invites/"):
			return "/teams/{slug}/invites/{invite_id}"
		}
		return "/teams/{slug}/access"
	case len(path) > len("/browser/sessions/") && path[:len("/browser/sessions/")] == "/browser/sessions/":
		return "/browser/sessions/{key}"
	}
	return path
}

func statusError(method, path string, status int) error {
	return fmt.Errorf("%s %s: backend answered %d", method, path, status)
}
