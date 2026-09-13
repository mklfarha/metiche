package browser

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

const (
	// LinkTTL is how long a sign-in link can be exchanged (decision 1, §9).
	LinkTTL = 10 * time.Minute
	// MaxOutstandingLinks is how many unconsumed, unexpired links one account
	// may hold at once; minting another is ErrTooManyLinks.
	MaxOutstandingLinks = 5

	// RedirectHome and RedirectTeams are the two fixed redirect paths; the
	// third shape is TeamRedirectPath(slug).
	RedirectHome  = "/"
	RedirectTeams = "/teams"

	// DefaultBoardBaseURL is used when METICHE_BOARD_BASE_URL is unset.
	DefaultBoardBaseURL = "https://metiche.xyz"
	// BoardBaseURLEnv names the backend's board base URL setting.
	BoardBaseURLEnv = "METICHE_BOARD_BASE_URL"

	// userAgentMaxRunes and ipHintMaxLen are the column widths.
	userAgentMaxRunes = 200
	ipHintMaxLen      = 64
)

var (
	// ErrTooManyLinks: the account already holds MaxOutstandingLinks live
	// links. app/mcp renders it as "rate_limited: ...".
	ErrTooManyLinks = errors.New("too many outstanding sign-in links for this account; use one or wait for them to expire")
	// ErrMintRefused: the account is not ACTIVE, or the agent is not ACTIVE or
	// does not belong to the account. One error for all three.
	ErrMintRefused = errors.New("this agent cannot mint a sign-in link")
	// ErrInvalidRedirect: the redirect path is not "/", "/teams" or
	// "/t/<slug>" with a valid slug.
	ErrInvalidRedirect = errors.New("invalid board redirect path")
	// ErrInvalidBoardBaseURL: the board base URL is not https:// (or
	// http://localhost / http://127.0.0.1), or carries a path, query,
	// fragment or userinfo.
	ErrInvalidBoardBaseURL = errors.New("invalid board base URL")
	// ErrLinkNotUsable is the ONE refusal of an exchange: unknown, already
	// used, expired, retired agent, inactive account.
	ErrLinkNotUsable = errors.New("no such sign-in link")
	// ErrUnavailable wraps every database failure. Callers map it to 503.
	ErrUnavailable = errors.New("browser sign-in is unavailable")
)

// unavailable wraps a database failure. The driver text stays in the chain for
// the server log; no handler in this package ever writes an error's text into
// a response.
func unavailable(err error) error { return fmt.Errorf("%w: %v", ErrUnavailable, err) }

// MintLinkRequest is what open_board passes after it has authenticated the
// agent token and chosen the team.
type MintLinkRequest struct {
	// AccountUUID is the account the browser will be signed in as.
	AccountUUID uuid.UUID
	// AgentUUID is the agent whose token is minting. It must be ACTIVE and
	// belong to AccountUUID, now and again at exchange.
	AgentUUID uuid.UUID
	// RedirectPath must satisfy ValidRedirectPath: RedirectHome,
	// RedirectTeams or TeamRedirectPath(slug).
	RedirectPath string
	// RequestedVia is client-reported (installer, cli, agent). INVALID is
	// stored as AGENT.
	RequestedVia enums.BoardLinkSource
	// BoardBaseURL is the board origin, e.g. from BoardBaseURLFromEnv. Empty
	// means DefaultBoardBaseURL. It is validated with ParseBoardBaseURL.
	BoardBaseURL string
}

// MintedLink is the result of MintLink. Secret and LoginURL are shown ONCE and
// never stored, logged or put into an error.
type MintedLink struct {
	Secret       string    // "mbl_..."
	LoginURL     string    // <board base>/signin#<Secret>
	BoardURL     string    // <board base><RedirectPath>, no secret
	RedirectPath string    // as stored
	ExpiresAt    time.Time // UTC, now + LinkTTL
}

// String never prints the secret, so a stray %v cannot log one.
func (m MintedLink) String() string {
	return fmt.Sprintf("MintedLink{BoardURL:%s RedirectPath:%s ExpiresAt:%s}", m.BoardURL, m.RedirectPath, m.ExpiresAt.Format(time.RFC3339))
}

// dbNow is the one clock: UTC, truncated to the second, because every
// timestamp column here is DATETIME(0) and MySQL ROUNDS fractional seconds on
// insert. Truncating in Go keeps "stored" and "compared" the same instant.
func dbNow() time.Time { return time.Now().UTC().Truncate(time.Second) }

// MintLink stores a new sign-in link (§2.3) and returns its secret.
//
// In one transaction it locks the account row, requires the account ACTIVE and
// the agent ACTIVE and owned by that account (else ErrMintRefused), counts the
// account's unconsumed unexpired links (>= MaxOutstandingLinks is
// ErrTooManyLinks), and inserts the row with only Hash(secret). Database
// failures wrap ErrUnavailable. The per-account hourly rate limit is the
// caller's (app/mcp openBoardLimit).
//
// The account row lock (SELECT ... FOR UPDATE) is what makes the cap exact:
// without it two concurrent mints both count 4 and both insert.
func MintLink(ctx context.Context, db *sql.DB, req MintLinkRequest) (MintedLink, error) {
	if db == nil {
		return MintedLink{}, unavailable(errors.New("no database"))
	}
	if req.AccountUUID == uuid.Nil || req.AgentUUID == uuid.Nil {
		return MintedLink{}, ErrMintRefused
	}
	if !ValidRedirectPath(req.RedirectPath) {
		return MintedLink{}, ErrInvalidRedirect
	}
	base := req.BoardBaseURL
	if base == "" {
		base = DefaultBoardBaseURL
	}
	base, err := ParseBoardBaseURL(base)
	if err != nil {
		return MintedLink{}, err
	}
	via := req.RequestedVia
	switch via {
	case enums.BOARD_LINK_SOURCE_INSTALLER, enums.BOARD_LINK_SOURCE_CLI, enums.BOARD_LINK_SOURCE_AGENT:
	default:
		via = enums.BOARD_LINK_SOURCE_AGENT
	}

	secret, hash, err := MintLinkSecret()
	if err != nil {
		return MintedLink{}, unavailable(err)
	}
	id, err := uuid.NewV4()
	if err != nil {
		return MintedLink{}, unavailable(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MintedLink{}, unavailable(err)
	}
	defer func() { _ = tx.Rollback() }()

	now := dbNow()
	expires := now.Add(LinkTTL)
	account, agent := req.AccountUUID.String(), req.AgentUUID.String()

	var one int
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM `account` c JOIN `agent` a ON a.`account_uuid` = c.`id` "+
			"WHERE c.`id` = ? AND a.`id` = ? AND c.`status` = ? AND a.`status` = ? FOR UPDATE",
		account, agent, enums.RECORD_STATUS_ACTIVE, enums.AGENT_STATUS_ACTIVE).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return MintedLink{}, ErrMintRefused
	}
	if err != nil {
		return MintedLink{}, unavailable(err)
	}

	var outstanding int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `board_login_link` WHERE `account_uuid` = ? AND `consumed_at` IS NULL AND `expires_at` > ?",
		account, now).Scan(&outstanding); err != nil {
		return MintedLink{}, unavailable(err)
	}
	if outstanding >= MaxOutstandingLinks {
		return MintedLink{}, ErrTooManyLinks
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO `board_login_link` (`id`,`account_uuid`,`agent_uuid`,`secret_hash`,`redirect_path`,`requested_via`,`expires_at`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?)",
		id.String(), account, agent, hash, req.RedirectPath, via.ToInt64(), expires, now, now); err != nil {
		return MintedLink{}, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return MintedLink{}, unavailable(err)
	}
	return MintedLink{
		Secret:       secret,
		LoginURL:     base + "/signin#" + secret,
		BoardURL:     base + req.RedirectPath,
		RedirectPath: req.RedirectPath,
		ExpiresAt:    expires,
	}, nil
}

// ExchangeRequest is the body of POST /v1/browser/sessions.
type ExchangeRequest struct {
	LinkSecret string `json:"link_secret"`
	// UserAgent is sanitized (printable only) and truncated to 200 runes.
	UserAgent string `json:"user_agent"`
	// IPHint is reduced to its IPv4 /24 or IPv6 /48 prefix; unparseable is
	// stored as NULL.
	IPHint string `json:"ip_hint"`
}

// Exchanged is a freshly created browser session. SessionSecret is the cookie
// value and is returned exactly once.
type Exchanged struct {
	SessionSecret string    `json:"session_secret"`
	SessionKey    string    `json:"session_key"`
	ExpiresAt     time.Time `json:"expires_at"`
	RedirectPath  string    `json:"redirect_path"`
	AccountKey    string    `json:"account_key"`
	DisplayName   string    `json:"display_name"`
}

// Exchange consumes a link and creates a browser session, in ONE transaction
// (§2.4): the conditional UPDATE (unconsumed AND unexpired) must affect one
// row, the link's agent and account must be ACTIVE, and the session is
// inserted with auth_method TERMINAL_LINK. Any refusal is ErrLinkNotUsable;
// any database failure wraps ErrUnavailable and consumes nothing. Of two
// concurrent exchanges of one link exactly one succeeds.
//
// Why the UPDATE comes first: it takes the row lock on the link. A second,
// concurrent exchange of the same link blocks on that lock, and when the first
// commits InnoDB re-evaluates its WHERE against the committed row, which is no
// longer `consumed_at IS NULL`, so it affects zero rows. That is the whole
// single-use guarantee; nothing in Go re-checks consumed_at or expires_at, so
// the SQL predicate cannot silently become decorative.
func Exchange(ctx context.Context, db *sql.DB, req ExchangeRequest) (Exchanged, error) {
	linkSecret := strings.TrimSpace(req.LinkSecret)
	if linkSecret == "" || !strings.HasPrefix(linkSecret, LinkPrefix) {
		return Exchanged{}, ErrLinkNotUsable
	}
	if db == nil {
		return Exchanged{}, unavailable(errors.New("no database"))
	}
	hash := Hash(linkSecret)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Exchanged{}, unavailable(err)
	}
	// Every return before Commit rolls back, so a refusal after the UPDATE
	// (retired agent, inactive account) does not burn the link, and a database
	// failure consumes nothing.
	defer func() { _ = tx.Rollback() }()

	now := dbNow()
	res, err := tx.ExecContext(ctx,
		"UPDATE `board_login_link` SET `consumed_at` = ?, `updated_at` = ? "+
			"WHERE `secret_hash` = ? AND `consumed_at` IS NULL AND `expires_at` > ?",
		now, now, hash, now)
	if err != nil {
		return Exchanged{}, unavailable(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Exchanged{}, unavailable(err)
	}
	if n != 1 {
		return Exchanged{}, ErrLinkNotUsable
	}

	var (
		storedHash, accountID, agentID, redirect, accountKey, displayName string
	)
	err = tx.QueryRowContext(ctx,
		"SELECT l.`secret_hash`, l.`account_uuid`, l.`agent_uuid`, l.`redirect_path`, c.`key`, c.`display_name` "+
			"FROM `board_login_link` l "+
			"JOIN `agent` a ON a.`id` = l.`agent_uuid` AND a.`account_uuid` = l.`account_uuid` "+
			"JOIN `account` c ON c.`id` = l.`account_uuid` "+
			"WHERE l.`secret_hash` = ? AND a.`status` = ? AND c.`status` = ? LIMIT 1",
		hash, enums.AGENT_STATUS_ACTIVE, enums.RECORD_STATUS_ACTIVE).
		Scan(&storedHash, &accountID, &agentID, &redirect, &accountKey, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return Exchanged{}, ErrLinkNotUsable
	}
	if err != nil {
		return Exchanged{}, unavailable(err)
	}
	// The SQL equality found the row; the constant-time compare is the
	// security boundary, as in app/mcp.IdentityByToken.
	if !Verify(linkSecret, storedHash) || !ValidRedirectPath(redirect) {
		return Exchanged{}, ErrLinkNotUsable
	}

	sessionSecret, sessionHash, err := MintSessionSecret()
	if err != nil {
		return Exchanged{}, unavailable(err)
	}
	sessionID, err := uuid.NewV4()
	if err != nil {
		return Exchanged{}, unavailable(err)
	}
	expires := now.Add(SessionAbsoluteTTL)
	ua, ip := sanitizeUserAgent(req.UserAgent), ipPrefix(req.IPHint)

	var key string
	// The key is 50 random bits; a collision is astronomically unlikely, but a
	// duplicate-key failure is a statement rollback in InnoDB, not a
	// transaction abort, so retrying inside the transaction is safe.
	for attempt := 0; ; attempt++ {
		if key, err = mintSessionKey(); err != nil {
			return Exchanged{}, unavailable(err)
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO `browser_session` (`id`,`key`,`account_uuid`,`secret_hash`,`auth_method`,`created_from_agent_uuid`,"+
				"`user_agent`,`ip_hint`,`expires_at`,`last_seen_at`,`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			sessionID.String(), key, accountID, sessionHash, enums.BROWSER_AUTH_METHOD_TERMINAL_LINK, agentID,
			ua, ip, expires, now, now, now)
		if err == nil {
			break
		}
		var me *mysql.MySQLError
		if attempt < 2 && errors.As(err, &me) && me.Number == 1062 {
			continue
		}
		return Exchanged{}, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return Exchanged{}, unavailable(err)
	}
	return Exchanged{
		SessionSecret: sessionSecret,
		SessionKey:    key,
		ExpiresAt:     expires,
		RedirectPath:  redirect,
		AccountKey:    accountKey,
		DisplayName:   displayName,
	}, nil
}

// sessionKeyAlphabet is Crockford base32, as app/mcp's join codes.
const sessionKeyAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// mintSessionKey returns "BS-" + 10 base32 characters. A public handle, not a
// credential.
func mintSessionKey() (string, error) {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = sessionKeyAlphabet[int(b)%len(sessionKeyAlphabet)]
	}
	return "BS-" + string(out), nil
}

// sanitizeUserAgent keeps printable characters only and truncates to the
// column width. Empty is stored as NULL.
func sanitizeUserAgent(ua string) any {
	var b strings.Builder
	runes := 0
	for _, r := range ua {
		if runes >= userAgentMaxRunes {
			break
		}
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
		runes++
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		return nil
	}
	return s
}

// ipPrefix reduces an address (or a prefix) to IPv4 /24 or IPv6 /48
// (decision 6, §9). The board is expected to send a prefix already; this makes
// the backend enforce it rather than trust it. Unparseable is NULL.
func ipPrefix(hint string) any {
	hint = strings.TrimSpace(hint)
	if hint == "" || len(hint) > ipHintMaxLen*2 {
		return nil
	}
	var addr netip.Addr
	if p, err := netip.ParsePrefix(hint); err == nil {
		addr = p.Addr()
	} else if a, err := netip.ParseAddr(hint); err == nil {
		addr = a
	} else {
		return nil
	}
	addr = addr.Unmap().WithZone("")
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return nil
	}
	s := p.String()
	if len(s) > ipHintMaxLen {
		return nil
	}
	return s
}

// slugPattern is the board's ValidSlug (code/frontend/internal/web/discovery.go).
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// ValidRedirectPath reports whether p is "/", "/teams" or "/t/<slug>" where
// slug matches the board's ValidSlug pattern ^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$.
func ValidRedirectPath(p string) bool {
	switch p {
	case RedirectHome, RedirectTeams:
		return true
	}
	slug, ok := strings.CutPrefix(p, "/t/")
	return ok && slugPattern.MatchString(slug)
}

// TeamRedirectPath returns "/t/<slug>", or ErrInvalidRedirect for a slug the
// board would not accept.
func TeamRedirectPath(slug string) (string, error) {
	if !slugPattern.MatchString(slug) {
		return "", ErrInvalidRedirect
	}
	return "/t/" + slug, nil
}

// ParseBoardBaseURL validates and normalizes a board base URL: scheme https
// (or http with host exactly localhost / 127.0.0.1, any port), no userinfo,
// query or fragment, and no path beyond "/". The result has no trailing slash.
func ParseBoardBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.Contains(raw, "#") || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return "", ErrInvalidBoardBaseURL
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
			return "", ErrInvalidBoardBaseURL
		}
	default:
		return "", ErrInvalidBoardBaseURL
	}
	return u.Scheme + "://" + u.Host, nil
}

// BoardBaseURLFromEnv reads METICHE_BOARD_BASE_URL (default
// DefaultBoardBaseURL) through ParseBoardBaseURL.
func BoardBaseURLFromEnv() (string, error) {
	v := strings.TrimSpace(os.Getenv(BoardBaseURLEnv))
	if v == "" {
		v = DefaultBoardBaseURL
	}
	return ParseBoardBaseURL(v)
}
