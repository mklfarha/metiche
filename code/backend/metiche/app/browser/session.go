package browser

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/gofrs/uuid"

	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	"github.com/mklfarha/metiche/backend/enums"
)

const (
	// SessionAbsoluteTTL is a session's absolute lifetime (decision 2, §9).
	SessionAbsoluteTTL = 30 * 24 * time.Hour
	// SessionIdleTTL is how long a session survives without being seen.
	SessionIdleTTL = 7 * 24 * time.Hour
	// LastSeenWriteInterval bounds last_seen_at writes to one per session per
	// interval, so browsing does not write a row per request.
	LastSeenWriteInterval = 10 * time.Minute

	// HeaderSession carries the session secret from the board to the backend.
	// It is the only place a session secret is ever read from.
	HeaderSession = "X-Metiche-Browser-Session"

	listSessionsMax = 100
	listTeamsMax    = 200
)

var (
	// ErrUnauthenticated is the ONE refusal of a session check: unknown,
	// hash mismatch, revoked, absolutely or idle expired, inactive account,
	// origin agent not ACTIVE. Rendered as 401 by this package's REST handlers.
	ErrUnauthenticated = errors.New("no such browser session")
	// ErrSessionNotFound: a session key that does not name a session of the
	// caller's account. Rendered as 404; app/mcp renders it as "not_found:".
	ErrSessionNotFound = errors.New("no such browser session key")
)

// Viewer is a validated browser session: who is looking at the board.
// It never holds the secret or its hash.
type Viewer struct {
	SessionUUID uuid.UUID
	SessionKey  string
	AccountUUID uuid.UUID
	AccountKey  string
	DisplayName string
	// ExpiresAt is the absolute expiry (UTC).
	ExpiresAt time.Time
}

// ValidateSession resolves a session secret to its Viewer (§2.7): lookup by
// Hash(secret), constant-time Verify, revoked_at IS NULL, expires_at > now,
// last seen (or created) within SessionIdleTTL, account ACTIVE, and, when the
// session was created from an agent, that agent ACTIVE and owned by the same
// account. It then writes last_seen_at if it is older than
// LastSeenWriteInterval (best effort).
//
// Refusals are ErrUnauthenticated; database failures wrap ErrUnavailable.
//
// Every rule is in the WHERE clause, not in Go: the query either returns the
// session or nothing, so there is no second place a rule can be forgotten, and
// no refusal reason exists anywhere to leak. Expiry is enforced here and never
// relies on the sweeper (§3.3).
func ValidateSession(ctx context.Context, db *sql.DB, secret string) (Viewer, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" || !strings.HasPrefix(secret, SessionPrefix) {
		return Viewer{}, ErrUnauthenticated
	}
	if db == nil {
		return Viewer{}, unavailable(errors.New("no database"))
	}
	hash := Hash(secret)
	now := dbNow()

	var (
		sessionID, key, storedHash, accountID, accountKey, displayName string
		expires                                                        time.Time
		lastSeen                                                       sql.NullTime
	)
	err := db.QueryRowContext(ctx,
		"SELECT s.`id`, s.`key`, s.`secret_hash`, s.`expires_at`, s.`last_seen_at`, c.`id`, c.`key`, c.`display_name` "+
			"FROM `browser_session` s "+
			"JOIN `account` c ON c.`id` = s.`account_uuid` "+
			"LEFT JOIN `agent` a ON a.`id` = s.`created_from_agent_uuid` "+
			"WHERE s.`secret_hash` = ? "+
			"AND s.`revoked_at` IS NULL "+
			"AND s.`expires_at` > ? "+
			"AND COALESCE(s.`last_seen_at`, s.`created_at`) > ? "+
			"AND c.`status` = ? "+
			// The origin agent. A TERMINAL_LINK session is valid only while the
			// agent that minted its link is ACTIVE and still this account's;
			// retiring the agent ends the session with no write (decision 4).
			// Only a session that never had an agent (GITHUB, reserved) skips it.
			"AND ((s.`created_from_agent_uuid` IS NULL AND s.`auth_method` = ?) "+
			"OR (a.`status` = ? AND a.`account_uuid` = s.`account_uuid`)) "+
			"LIMIT 1",
		hash, now, now.Add(-SessionIdleTTL), enums.RECORD_STATUS_ACTIVE,
		enums.BROWSER_AUTH_METHOD_GITHUB, enums.AGENT_STATUS_ACTIVE).
		Scan(&sessionID, &key, &storedHash, &expires, &lastSeen, &accountID, &accountKey, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return Viewer{}, ErrUnauthenticated
	}
	if err != nil {
		return Viewer{}, unavailable(err)
	}
	if !Verify(secret, storedHash) {
		return Viewer{}, ErrUnauthenticated
	}
	v := Viewer{SessionKey: key, AccountKey: accountKey, DisplayName: displayName, ExpiresAt: expires.UTC()}
	if v.SessionUUID, err = uuid.FromString(sessionID); err != nil {
		return Viewer{}, ErrUnauthenticated
	}
	if v.AccountUUID, err = uuid.FromString(accountID); err != nil {
		return Viewer{}, ErrUnauthenticated
	}

	if !lastSeen.Valid || !lastSeen.Time.After(now.Add(-LastSeenWriteInterval)) {
		// Best effort, and conditional so concurrent requests write once. A
		// failure here does not un-validate a session the read just proved.
		_, _ = db.ExecContext(ctx,
			"UPDATE `browser_session` SET `last_seen_at` = ?, `updated_at` = ? "+
				"WHERE `id` = ? AND (`last_seen_at` IS NULL OR `last_seen_at` <= ?)",
			now, now, sessionID, now.Add(-LastSeenWriteInterval))
	}
	return v, nil
}

// AccountBySession is ValidateSession for app/authz.Guard: the same predicate,
// returning the account. Only ID, Key, DisplayName and Status (always
// RECORD_STATUS_ACTIVE on success) are populated; TokenHash is never set.
// Refusals are ErrUnauthenticated; database failures wrap ErrUnavailable.
func AccountBySession(ctx context.Context, db *sql.DB, secret string) (account_entity.Account, error) {
	v, err := ValidateSession(ctx, db, secret)
	if err != nil {
		return account_entity.Account{}, err
	}
	return account_entity.Account{
		ID:          v.AccountUUID,
		Key:         v.AccountKey,
		DisplayName: v.DisplayName,
		Status:      enums.RECORD_STATUS_ACTIVE,
	}, nil
}

// SignOut revokes the session this secret names (end_reason SIGNED_OUT).
// A secret that does not validate is ErrUnauthenticated.
func SignOut(ctx context.Context, db *sql.DB, secret string) error {
	v, err := ValidateSession(ctx, db, secret)
	if err != nil {
		return err
	}
	now := dbNow()
	if _, err := db.ExecContext(ctx,
		"UPDATE `browser_session` SET `revoked_at` = ?, `end_reason` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `revoked_at` IS NULL",
		now, enums.BROWSER_SESSION_END_REASON_SIGNED_OUT, now, v.SessionUUID.String()); err != nil {
		return unavailable(err)
	}
	return nil
}

// Revoke ends browser sessions of one account and returns how many rows it
// ended.
//
//   - sessionKey == "": every live (unrevoked, unexpired) session of the
//     account.
//   - otherwise: that one session. A key that does not name a session of this
//     account is ErrSessionNotFound; a key of this account's that has already
//     ended is (0, nil).
//
// reason is the end_reason written: SIGNED_OUT_EVERYWHERE (board "sign out
// everywhere"), REVOKED (board revoke one), REVOKED_BY_AGENT (MCP
// sign_out_browsers). Database failures wrap ErrUnavailable.
func Revoke(ctx context.Context, db *sql.DB, accountUUID uuid.UUID, sessionKey string, reason enums.BrowserSessionEndReason) (int64, error) {
	if accountUUID == uuid.Nil {
		return 0, ErrSessionNotFound
	}
	switch reason {
	case enums.BROWSER_SESSION_END_REASON_SIGNED_OUT, enums.BROWSER_SESSION_END_REASON_SIGNED_OUT_EVERYWHERE,
		enums.BROWSER_SESSION_END_REASON_REVOKED, enums.BROWSER_SESSION_END_REASON_REVOKED_BY_AGENT,
		enums.BROWSER_SESSION_END_REASON_REPLACED:
	default:
		reason = enums.BROWSER_SESSION_END_REASON_REVOKED
	}
	if db == nil {
		return 0, unavailable(errors.New("no database"))
	}
	now := dbNow()
	account := accountUUID.String()

	sessionKey = strings.ToUpper(strings.TrimSpace(sessionKey))
	if sessionKey == "" {
		res, err := db.ExecContext(ctx,
			"UPDATE `browser_session` SET `revoked_at` = ?, `end_reason` = ?, `updated_at` = ? "+
				"WHERE `account_uuid` = ? AND `revoked_at` IS NULL AND `expires_at` > ?",
			now, reason.ToInt64(), now, account, now)
		if err != nil {
			return 0, unavailable(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, unavailable(err)
		}
		return n, nil
	}

	// The account is in the WHERE of both statements: another account's key
	// has no row to find, and so is indistinguishable from no key at all.
	var id string
	err := db.QueryRowContext(ctx,
		"SELECT `id` FROM `browser_session` WHERE `key` = ? AND `account_uuid` = ? LIMIT 1",
		sessionKey, account).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, unavailable(err)
	}
	res, err := db.ExecContext(ctx,
		"UPDATE `browser_session` SET `revoked_at` = ?, `end_reason` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `account_uuid` = ? AND `revoked_at` IS NULL AND `expires_at` > ?",
		now, reason.ToInt64(), now, id, account, now)
	if err != nil {
		return 0, unavailable(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, unavailable(err)
	}
	return n, nil
}

// SessionInfo is one row of the account page's session list. Never a secret or
// a hash.
type SessionInfo struct {
	Key        string     `json:"key"`
	AuthMethod string     `json:"auth_method"` // "terminal_link" | "github"
	State      string     `json:"state"`       // "live" | "ended" | "expired"
	Current    bool       `json:"current"`     // the session making this request
	UserAgent  string     `json:"user_agent"`  // "" when unknown
	IPHint     string     `json:"ip_hint"`     // "" when unknown
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	EndReason  string     `json:"end_reason"` // "" | "signed_out" | "signed_out_everywhere" | "revoked" | "revoked_by_agent" | "replaced"
}

// ListSessions lists an account's browser sessions, newest first (at most
// 100), including ended and expired ones still in the table so "why was I
// signed out" is answerable. current marks the caller's own session.
//
// "live" here is revoked/expiry/idle only; a session whose origin agent was
// retired shows as live but no longer validates.
func ListSessions(ctx context.Context, db *sql.DB, accountUUID, current uuid.UUID) ([]SessionInfo, error) {
	if db == nil {
		return nil, unavailable(errors.New("no database"))
	}
	now := dbNow()
	rows, err := db.QueryContext(ctx,
		"SELECT `id`, `key`, `auth_method`, `user_agent`, `ip_hint`, `created_at`, `last_seen_at`, `expires_at`, `revoked_at`, `end_reason` "+
			"FROM `browser_session` WHERE `account_uuid` = ? ORDER BY `created_at` DESC, `key` LIMIT ?",
		accountUUID.String(), listSessionsMax)
	if err != nil {
		return nil, unavailable(err)
	}
	defer func() { _ = rows.Close() }()

	out := []SessionInfo{}
	for rows.Next() {
		var (
			id, key             string
			method              int64
			ua, ip              sql.NullString
			created, expires    time.Time
			lastSeen, revokedAt sql.NullTime
			endReason           sql.NullInt64
		)
		if err := rows.Scan(&id, &key, &method, &ua, &ip, &created, &lastSeen, &expires, &revokedAt, &endReason); err != nil {
			return nil, unavailable(err)
		}
		info := SessionInfo{
			Key:        key,
			AuthMethod: enums.BrowserAuthMethod(method).String(),
			Current:    current != uuid.Nil && id == current.String(),
			UserAgent:  ua.String,
			IPHint:     ip.String,
			CreatedAt:  created.UTC(),
			ExpiresAt:  expires.UTC(),
		}
		seen := created
		if lastSeen.Valid {
			t := lastSeen.Time.UTC()
			info.LastSeenAt, seen = &t, t
		}
		if revokedAt.Valid {
			t := revokedAt.Time.UTC()
			info.RevokedAt = &t
		}
		if endReason.Valid && endReason.Int64 != 0 {
			info.EndReason = enums.BrowserSessionEndReason(endReason.Int64).String()
		}
		switch {
		case revokedAt.Valid:
			info.State = "ended"
		case !expires.After(now) || !seen.After(now.Add(-SessionIdleTTL)):
			info.State = "expired"
		default:
			info.State = "live"
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// TeamSummary is one of "Your teams" (§4.5).
type TeamSummary struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"` // "public" | "private"
	Role       string `json:"role"`       // "owner" | "member"
}

// ListTeams lists the active teams the account is a live member of (member
// not revoked and ACTIVE, team ACTIVE), by name, at most 200.
//
// Rooted at this account's own member rows, as app/mcp list_teams: a team the
// account is not a live member of has no row to produce.
func ListTeams(ctx context.Context, db *sql.DB, accountUUID uuid.UUID) ([]TeamSummary, error) {
	if db == nil {
		return nil, unavailable(errors.New("no database"))
	}
	rows, err := db.QueryContext(ctx,
		"SELECT t.`slug`, t.`name`, t.`visibility`, m.`role` FROM `member` m JOIN `team` t ON t.`id` = m.`team_uuid` "+
			"WHERE m.`account_uuid` = ? AND m.`revoked_at` IS NULL AND m.`status` = ? AND t.`status` = ? "+
			"ORDER BY t.`name`, t.`slug` LIMIT ?",
		accountUUID.String(), enums.RECORD_STATUS_ACTIVE, enums.RECORD_STATUS_ACTIVE, listTeamsMax)
	if err != nil {
		return nil, unavailable(err)
	}
	defer func() { _ = rows.Close() }()
	out := []TeamSummary{}
	for rows.Next() {
		var (
			slug, name       string
			visibility, role int64
		)
		if err := rows.Scan(&slug, &name, &visibility, &role); err != nil {
			return nil, unavailable(err)
		}
		vis := "private" // anything not exactly public fails closed, as authz
		if enums.TeamVisibility(visibility) == enums.TEAM_VISIBILITY_PUBLIC {
			vis = "public"
		}
		out = append(out, TeamSummary{Slug: slug, Name: name, Visibility: vis, Role: enums.MemberRole(role).String()})
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}
