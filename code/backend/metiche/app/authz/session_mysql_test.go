package authz

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/browser"
	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/enums"
)

// The browser-session branch of the Guard (docs/BOARD_LOGIN.md §4.1), through
// the production decision on a real MySQL. The HTTP-level proofs (byte-identical
// 404s, /access bodies, the stream) are in app/webapi's mysql test.
//
// Needs METICHE_TEST_MYSQL_DSN (parseTime=true&interpolateParams=true) at a
// database holding create.sql. No DSN is committed anywhere.

type sessionFixture struct {
	db                    *sql.DB
	privateSlug           string
	publicSlug            string
	ownerSession          string // Ana, OWNER of both teams
	memberSession         string // Beto, MEMBER of the private team
	outsiderSession       string // an ACTIVE account that is a member of nothing
	ownerToken            string // Ana's agent token
	anaAccount, betoAgent string
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("METICHE_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("skipped: METICHE_TEST_MYSQL_DSN is not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("opening MySQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("skipped: METICHE_TEST_MYSQL_DSN is set but the database is unreachable: %v", err)
	}
	return db
}

func seedSessions(t *testing.T) sessionFixture {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	id := func() string { return uuid.Must(uuid.NewV4()).String() }

	privateTeam, publicTeam := id(), id()
	fx := sessionFixture{db: db, privateSlug: "priv-" + privateTeam[:8], publicSlug: "pub-" + publicTeam[:8]}
	exec("INSERT INTO `team` (`id`,`name`,`slug`,`status`,`visibility`) VALUES (?,?,?,?,?)",
		privateTeam, "Private", fx.privateSlug, enums.RECORD_STATUS_ACTIVE, enums.TEAM_VISIBILITY_PRIVATE)
	exec("INSERT INTO `team` (`id`,`name`,`slug`,`status`,`visibility`) VALUES (?,?,?,?,?)",
		publicTeam, "Public", fx.publicSlug, enums.RECORD_STATUS_ACTIVE, enums.TEAM_VISIBILITY_PUBLIC)

	account := func(name string) string {
		acct := id()
		_, unheld, err := metichemcp.MintToken()
		if err != nil {
			t.Fatal(err)
		}
		exec("INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
			acct, strings.ReplaceAll(acct, "-", ""), name, unheld, enums.IDENTITY_PROVIDER_NONE, enums.RECORD_STATUS_ACTIVE)
		return acct
	}
	agent := func(acct, key string) (agentID, token string) {
		agentID = id()
		token, hash, err := metichemcp.MintToken()
		if err != nil {
			t.Fatal(err)
		}
		exec("INSERT INTO `agent` (`id`,`key`,`label`,`client_key`,`status`,`account_uuid`,`token_hash`) VALUES (?,?,?,?,?,?,?)",
			agentID, key, "claude", "client-"+agentID[:8], enums.AGENT_STATUS_ACTIVE, acct, hash)
		return agentID, token
	}
	member := func(team, acct, key string, role enums.MemberRole) {
		exec("INSERT INTO `member` (`id`,`team_uuid`,`key`,`display_name`,`role`,`status`,`account_uuid`) VALUES (?,?,?,?,?,?,?)",
			id(), team, key, key, role, enums.RECORD_STATUS_ACTIVE, acct)
	}
	session := func(acct, agentID string) string {
		secret, hash, err := browser.MintSessionSecret()
		if err != nil {
			t.Fatal(err)
		}
		exec("INSERT INTO `browser_session` (`id`,`key`,`account_uuid`,`secret_hash`,`auth_method`,`created_from_agent_uuid`,`expires_at`,`last_seen_at`) "+
			"VALUES (?,?,?,?,?,?,DATE_ADD(UTC_TIMESTAMP(), INTERVAL 30 DAY),UTC_TIMESTAMP())",
			id(), "BS-"+strings.ToUpper(hash[:10]), acct, hash, enums.BROWSER_AUTH_METHOD_TERMINAL_LINK, agentID)
		return secret
	}

	ana, beto, outsider := account("Ana"), account("Beto"), account("Outsider")
	fx.anaAccount = ana
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM `team` WHERE `id` IN (?,?)", privateTeam, publicTeam)
		_, _ = db.Exec("DELETE FROM `account` WHERE `id` IN (?,?,?)", ana, beto, outsider)
	})

	anaAgent, anaToken := agent(ana, "A-1")
	betoAgent, _ := agent(beto, "A-2")
	outsiderAgent, _ := agent(outsider, "A-3")
	fx.ownerToken, fx.betoAgent = anaToken, betoAgent

	member(privateTeam, ana, "M-1", enums.MEMBER_ROLE_OWNER)
	member(privateTeam, beto, "M-2", enums.MEMBER_ROLE_MEMBER)
	member(publicTeam, ana, "M-1", enums.MEMBER_ROLE_OWNER)

	fx.ownerSession = session(ana, anaAgent)
	fx.memberSession = session(beto, betoAgent)
	fx.outsiderSession = session(outsider, outsiderAgent)
	return fx
}

func TestGuardBrowserSessionOnAPrivateTeam(t *testing.T) {
	fx := seedSessions(t)
	g := NewGuard(fx.db)
	ctx := context.Background()

	owner, err := g.Authorize(ctx, fx.privateSlug, Credential{BrowserSession: fx.ownerSession})
	if err != nil {
		t.Fatalf("a live OWNER's valid session on the private team was denied: %v", err)
	}
	if owner.Public || owner.Role != enums.MEMBER_ROLE_OWNER {
		t.Fatalf("owner grant = %+v, want private with role owner", owner)
	}
	member, err := g.Authorize(ctx, fx.privateSlug, Credential{BrowserSession: fx.memberSession})
	if err != nil || member.Role != enums.MEMBER_ROLE_MEMBER {
		t.Fatalf("a live MEMBER's session: %+v, %v", member, err)
	}

	// Every refusal is exactly ErrDenied, nothing more specific.
	for name, cred := range map[string]Credential{
		"non-member session": {BrowserSession: fx.outsiderSession},
		"garbage session":    {BrowserSession: "mbs_garbage-that-names-no-session"},
		"no credential":      {},
		"bearer and session": {Bearer: fx.ownerToken, BrowserSession: fx.ownerSession},
	} {
		if _, err := g.Authorize(ctx, fx.privateSlug, cred); err != ErrDenied { //nolint:errorlint // exact sentinel, on purpose
			t.Fatalf("%s on a private team: %v, want exactly ErrDenied", name, err)
		}
	}

	// The same account's bearer alone still works: the pair was refused for
	// being two principals, not for either credential.
	if _, err := g.Authorize(ctx, fx.privateSlug, Credential{Bearer: fx.ownerToken}); err != nil {
		t.Fatalf("the owner's bearer alone was denied: %v", err)
	}
}

// TestGuardMembershipIsReadOnEveryRequest: nothing is cached, so the request
// after a revocation is refused.
func TestGuardMembershipIsReadOnEveryRequest(t *testing.T) {
	fx := seedSessions(t)
	g := NewGuard(fx.db)
	ctx := context.Background()
	cred := Credential{BrowserSession: fx.ownerSession}

	if _, err := g.Authorize(ctx, fx.privateSlug, cred); err != nil {
		t.Fatalf("before revocation: %v", err)
	}
	if _, err := fx.db.Exec("UPDATE `member` SET `revoked_at` = UTC_TIMESTAMP() WHERE `account_uuid` = ?", fx.anaAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Authorize(ctx, fx.privateSlug, cred); !errors.Is(err, ErrDenied) {
		t.Fatalf("the request right after the membership was revoked: %v, want ErrDenied", err)
	}
}

// TestGuardPublicTeamNeverLooksAtTheCredential: a garbage session, and two
// credentials at once, are both fine on a public board.
func TestGuardPublicTeamNeverLooksAtTheCredential(t *testing.T) {
	fx := seedSessions(t)
	g := NewGuard(fx.db)
	ctx := context.Background()

	for name, cred := range map[string]Credential{
		"garbage session":    {BrowserSession: "mbs_garbage"},
		"bearer and session": {Bearer: "mtk_garbage", BrowserSession: "mbs_garbage"},
		"non-member session": {BrowserSession: fx.outsiderSession},
	} {
		team, err := g.Authorize(ctx, fx.publicSlug, cred)
		if err != nil || !team.Public || team.Role != enums.MEMBER_ROLE_INVALID {
			t.Fatalf("%s on a public team: %+v, %v", name, team, err)
		}
	}

	// ViewerRole is best-effort on a public team: a role for a member, none for
	// anyone else, and never an error for a credential that does not resolve.
	team, _ := g.Authorize(ctx, fx.publicSlug, Credential{})
	for name, tc := range map[string]struct {
		cred Credential
		want enums.MemberRole
	}{
		"owner session":      {Credential{BrowserSession: fx.ownerSession}, enums.MEMBER_ROLE_OWNER},
		"owner bearer":       {Credential{Bearer: fx.ownerToken}, enums.MEMBER_ROLE_OWNER},
		"non-member session": {Credential{BrowserSession: fx.outsiderSession}, enums.MEMBER_ROLE_INVALID},
		"garbage session":    {Credential{BrowserSession: "mbs_garbage"}, enums.MEMBER_ROLE_INVALID},
		"nothing":            {Credential{}, enums.MEMBER_ROLE_INVALID},
		"both":               {Credential{Bearer: fx.ownerToken, BrowserSession: fx.ownerSession}, enums.MEMBER_ROLE_INVALID},
	} {
		role, err := g.ViewerRole(ctx, team, tc.cred)
		if err != nil || role != tc.want {
			t.Fatalf("ViewerRole(%s) = %v, %v; want %v", name, role, err, tc.want)
		}
	}
}

// TestGuardSessionLookupOutageIsNotARefusal: when the session cannot be checked
// the Guard returns an error that is NOT ErrDenied, so the gate answers 503 and
// the board keeps the cookie. A public team is unaffected: it never looks.
func TestGuardSessionLookupOutageIsNotARefusal(t *testing.T) {
	fx := seedSessions(t)
	g := NewGuard(fx.db)
	ctx := context.Background()

	breakSessionTable(t, fx.db)

	_, err := g.Authorize(ctx, fx.privateSlug, Credential{BrowserSession: fx.ownerSession})
	if err == nil || errors.Is(err, ErrDenied) {
		t.Fatalf("a session check that could not reach its table returned %v; want an error that is not ErrDenied", err)
	}
	if _, err := g.Authorize(ctx, fx.publicSlug, Credential{BrowserSession: fx.ownerSession}); err != nil {
		t.Fatalf("a public team during a session-table outage: %v", err)
	}
}

// breakSessionTable renames browser_session away for the rest of the test, so
// the session lookup fails with a real driver error while the team and member
// reads keep working.
func breakSessionTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("RENAME TABLE `browser_session` TO `browser_session_outage_test`"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec("RENAME TABLE `browser_session_outage_test` TO `browser_session`"); err != nil {
			t.Errorf("restoring browser_session: %v", err)
		}
	})
}
