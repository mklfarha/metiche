package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Tool: sign_out_browsers
// ─────────────────────────────────────────────
//
// The agent's half of browser-session revocation (docs/BOARD_LOGIN.md §2.8,
// §5.4). A browser signed in with a link from open_board holds a session for
// the whole ACCOUNT, and a person who asks their assistant "sign my browsers
// out" should not have to find the board's /account page to do it.
//
// Account-scoped, like list_teams: requireAccount and nothing further, because
// a browser session belongs to a person and names no team. The isolation that
// replaces a membership check is in every WHERE clause below — each statement
// is rooted at `account_uuid = <the caller's account>`, so a session key that
// belongs to somebody else matches no row and gets the same not_found answer as
// a key that never existed. There is no statement in this file that can reach
// another account's sessions.
//
// Direct SQL rather than app/browser, as §2.8 specifies: two statements, both
// bounded by the account index, and no reason to widen that package's surface
// for them.

const (
	// browserSessionIdle is the idle expiry (decision 2, §9). A session whose
	// last_seen_at is older no longer validates, so it is not listed as
	// signed in and not counted as signed out.
	browserSessionIdle = 7 * 24 * time.Hour

	// browserSessionKeyMax is browser_session.key's width.
	browserSessionKeyMax = 32

	// signOutBrowsersListMax is a runaway guard on the listing, not a product
	// limit: nobody has a hundred signed-in browsers.
	signOutBrowsersListMax = 100
)

// SignOutBrowsersParams: no argument lists, session_key revokes one, all
// revokes every one.
//
// Listing is the empty call on purpose. The tool's name says "sign out", and
// a model that calls it bare to find out what is there must not sign the
// person out of every browser as a side effect. Signing out everywhere is a
// flag the caller has to set.
type SignOutBrowsersParams struct {
	SessionKey string `json:"session_key,omitempty" jsonschema:"the key of ONE of your signed-in browsers (BS-...), as listed by calling this tool with no arguments. Signs that browser out"`
	All        bool   `json:"all,omitempty" jsonschema:"true signs out EVERY browser signed in to your account. Omit it (and session_key) to only list them"`
}

// BrowserSessionInfo is one signed-in browser. Never a secret or a hash: the
// key is a public handle that authenticates nothing.
type BrowserSessionInfo struct {
	Key        string     `json:"key"`
	AuthMethod string     `json:"auth_method"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UserAgent  string     `json:"user_agent,omitempty"`
	// FromThisAgent says the link that created it was minted with the token
	// on this very call.
	FromThisAgent bool `json:"from_this_agent"`
}

// SignOutBrowsersResult is not an Envelope for the reason ListTeamsResult is
// not: there is no team, so there are no cursors, and zeros would read as
// "you missed every event".
type SignOutBrowsersResult struct {
	OK      bool `json:"ok"`
	Revoked int  `json:"revoked"`
	// Sessions is what is still signed in AFTER this call.
	Sessions []BrowserSessionInfo `json:"sessions"`
	Note     string               `json:"note"`
}

// liveBrowserSessionWhere is "a session that would validate right now", for
// one account: not revoked, not past its absolute or idle expiry, and — when
// an agent created it — that agent still active. The account condition comes
// first and is not optional.
//
// Arguments, in order: account uuid, now, now - idle, AGENT_STATUS_ACTIVE.
const liveBrowserSessionWhere = "`account_uuid` = ? AND `revoked_at` IS NULL AND `expires_at` > ? " +
	"AND (`last_seen_at` IS NULL OR `last_seen_at` > ?) " +
	"AND (`created_from_agent_uuid` IS NULL OR `created_from_agent_uuid` IN " +
	"(SELECT `id` FROM `agent` WHERE `status` = ?))"

func liveBrowserSessionArgs(account uuid.UUID, now time.Time) []any {
	return []any{account.String(), now, now.Add(-browserSessionIdle), enums.AGENT_STATUS_ACTIVE}
}

// SignOutBrowsers lists, or revokes, the caller's own browser sessions.
func (h *Handler) SignOutBrowsers(ctx context.Context, _ *mcp.CallToolRequest, in SignOutBrowsersParams) (*mcp.CallToolResult, any, error) {
	acct, err := h.requireAccount(ctx)
	if err != nil {
		return nil, nil, err
	}
	key := strings.TrimSpace(in.SessionKey)
	if key != "" && in.All {
		return nil, nil, errors.New(
			"invalid_request: pass session_key to sign out one browser, or all=true to sign out every browser, not both")
	}
	if ok, wait := h.accountLimiters().signOutBrowsers.Allow(acct.ID.String()); !ok {
		return nil, nil, fmt.Errorf("rate_limited: too many sign_out_browsers calls for this account; try again in %s", wait)
	}

	db := h.core.DB()
	now := time.Now().UTC()
	out := SignOutBrowsersResult{OK: true}
	setRevoked := "UPDATE `browser_session` SET `revoked_at` = ?, `end_reason` = ?, `updated_at` = ? WHERE "
	revokedArgs := []any{now, enums.BROWSER_SESSION_END_REASON_REVOKED_BY_AGENT, now}

	switch {
	case in.All:
		res, err := db.ExecContext(ctx, setRevoked+liveBrowserSessionWhere,
			append(revokedArgs, liveBrowserSessionArgs(acct.ID, now)...)...)
		if err != nil {
			return nil, nil, retryable(err, "signing out your browsers")
		}
		n, _ := res.RowsAffected()
		out.Revoked = int(n)

	case key != "":
		notFound := fmt.Errorf(
			"not_found: no browser with session key %q is signed in to your account. "+
				"Call sign_out_browsers with no arguments to list yours", truncate(key, browserSessionKeyMax))
		if len([]rune(key)) > browserSessionKeyMax {
			return nil, nil, notFound
		}
		res, err := db.ExecContext(ctx, setRevoked+"`key` = ? AND "+liveBrowserSessionWhere,
			append(append(revokedArgs, key), liveBrowserSessionArgs(acct.ID, now)...)...)
		if err != nil {
			return nil, nil, retryable(err, "signing out that browser")
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			// Already signed out, or expired — or not this account's at all.
			// Only the first two are worth telling apart, and the check is
			// scoped to this account, so a key belonging to someone else is
			// exactly as unknown as a key that never existed.
			var mine int
			if err := db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM `browser_session` WHERE `account_uuid` = ? AND `key` = ?",
				acct.ID.String(), key).Scan(&mine); err != nil {
				return nil, nil, retryable(err, "looking up that browser")
			}
			if mine == 0 {
				return nil, nil, notFound
			}
		}
		out.Revoked = int(n)
	}

	var thisAgent uuid.UUID
	if id, ok := IdentityFromContext(ctx); ok && id.Agent != nil {
		thisAgent = id.Agent.ID
	}
	sessions, err := h.liveBrowserSessions(ctx, acct.ID, thisAgent, now)
	if err != nil {
		return nil, nil, err
	}
	out.Sessions = sessions
	out.Note = signOutBrowsersNote(in, key, out)
	return jsonValue(out)
}

// liveBrowserSessions lists the account's signed-in browsers, newest first.
func (h *Handler) liveBrowserSessions(ctx context.Context, account, thisAgent uuid.UUID, now time.Time) ([]BrowserSessionInfo, error) {
	rows, err := h.core.DB().QueryContext(ctx,
		"SELECT `key`, `auth_method`, `created_at`, `last_seen_at`, `expires_at`, "+
			"COALESCE(`user_agent`, ''), COALESCE(`created_from_agent_uuid`, '') "+
			"FROM `browser_session` WHERE "+liveBrowserSessionWhere+" "+
			"ORDER BY `created_at` DESC, `key` ASC LIMIT ?",
		append(liveBrowserSessionArgs(account, now), signOutBrowsersListMax)...)
	if err != nil {
		return nil, retryable(err, "listing your browsers")
	}
	defer func() { _ = rows.Close() }()

	out := []BrowserSessionInfo{}
	for rows.Next() {
		var (
			s         BrowserSessionInfo
			method    int64
			lastSeen  sql.NullTime
			fromAgent string
		)
		if err := rows.Scan(&s.Key, &method, &s.CreatedAt, &lastSeen, &s.ExpiresAt, &s.UserAgent, &fromAgent); err != nil {
			return nil, err
		}
		s.AuthMethod = enums.BrowserAuthMethod(method).String()
		if lastSeen.Valid {
			t := lastSeen.Time.UTC()
			s.LastSeenAt = &t
		}
		s.CreatedAt, s.ExpiresAt = s.CreatedAt.UTC(), s.ExpiresAt.UTC()
		s.FromThisAgent = !thisAgent.IsNil() && fromAgent == thisAgent.String()
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "listing your browsers")
	}
	return out, nil
}

func signOutBrowsersNote(in SignOutBrowsersParams, key string, out SignOutBrowsersResult) string {
	still := fmt.Sprintf("%d browser(s) are still signed in to this account.", len(out.Sessions))
	switch {
	case in.All && out.Revoked == 0:
		return "No browser was signed in to this account; nothing changed."
	case in.All, key != "" && out.Revoked > 0:
		return fmt.Sprintf("Signed out %d browser(s). Each loses access on its next page load, within about 15 seconds, "+
			"and an open board closes within a minute. %s", out.Revoked, still)
	case key != "":
		return "That browser was already signed out or had expired; nothing changed. " + still
	default:
		return still + " Nothing was changed. To sign one out, call sign_out_browsers with its session_key; " +
			"all=true signs out every one. Only do either when the person asked for it."
	}
}
