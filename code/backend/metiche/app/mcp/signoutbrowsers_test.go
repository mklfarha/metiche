package mcp

import (
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// sign_out_browsers, through the real transport. The property that matters is
// that the tool is account-scoped in every statement: another account's
// session key is exactly as unknown as a key that never existed, and nothing
// the caller sends can revoke, or reveal, somebody else's browser.

type seededSession struct {
	key      string
	expires  time.Time
	lastSeen time.Time
}

func seedBrowserSession(t *testing.T, hs *harness, c *caller, s seededSession) {
	t.Helper()
	secret, hash, err := MintToken()
	if err != nil || secret == "" {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if s.expires.IsZero() {
		s.expires = now.Add(30 * 24 * time.Hour)
	}
	if s.lastSeen.IsZero() {
		s.lastSeen = now
	}
	if _, err := hs.core.DB().Exec(
		"INSERT INTO `browser_session` (`id`,`key`,`account_uuid`,`secret_hash`,`auth_method`,`created_from_agent_uuid`,"+
			"`user_agent`,`expires_at`,`last_seen_at`) VALUES (?,?,?,?,?,?,?,?,?)",
		uuid.Must(uuid.NewV4()).String(), s.key, c.account.ID.String(), hash, enums.BROWSER_AUTH_METHOD_TERMINAL_LINK,
		c.agent.ID.String(), "Mozilla/5.0 test", s.expires, s.lastSeen); err != nil {
		t.Fatalf("seeding browser session %s: %v", s.key, err)
	}
}

// sessionState is (revoked, end_reason) for one key.
func sessionState(t *testing.T, hs *harness, key string) (bool, int64) {
	t.Helper()
	var revoked sql.NullTime
	var reason sql.NullInt64
	if err := hs.core.DB().QueryRow(
		"SELECT `revoked_at`, `end_reason` FROM `browser_session` WHERE `key` = ?", key).Scan(&revoked, &reason); err != nil {
		t.Fatalf("reading browser session %s: %v", key, err)
	}
	return revoked.Valid, reason.Int64
}

func sessionKeys(out SignOutBrowsersResult) []string {
	keys := make([]string, 0, len(out.Sessions))
	for _, s := range out.Sessions {
		keys = append(keys, s.Key)
	}
	sort.Strings(keys)
	return keys
}

func TestIntegrationSignOutBrowsersCannotTouchAnotherAccount(t *testing.T) {
	hs := loginHarness(t)
	endpoint, _ := loginServer(t, hs)
	ana := hs.join(t, "Ana", "ana-laptop")
	bob := hs.join(t, "Bob", "bob-laptop")
	if ana.account.ID == bob.account.ID {
		t.Fatal("setup: Ana and Bob must be two accounts")
	}
	now := time.Now().UTC()
	seedBrowserSession(t, hs, ana, seededSession{key: "BS-ANA0000001"})
	seedBrowserSession(t, hs, ana, seededSession{key: "BS-ANA0000002"})
	seedBrowserSession(t, hs, ana, seededSession{key: "BS-ANAEXPIRED", expires: now.Add(-time.Minute)})
	seedBrowserSession(t, hs, ana, seededSession{key: "BS-ANAIDLE001", lastSeen: now.Add(-8 * 24 * time.Hour)})
	seedBrowserSession(t, hs, bob, seededSession{key: "BS-BOB0000001"})

	asAna := connectAs(t, endpoint, ana.token)
	asBob := connectAs(t, endpoint, bob.token)

	signOut := func(cs *mcp.ClientSession, args map[string]any) SignOutBrowsersResult {
		t.Helper()
		res, text := callTool(t, cs, "sign_out_browsers", args)
		if res.IsError {
			t.Fatalf("sign_out_browsers %v: %s", args, text)
		}
		var out SignOutBrowsersResult
		decodeResult(t, res, &out)
		return out
	}

	// The bare call lists Ana's live browsers and changes nothing.
	out := signOut(asAna, map[string]any{})
	t.Logf("Ana lists: revoked=%d keys=%v", out.Revoked, sessionKeys(out))
	if got := strings.Join(sessionKeys(out), ","); got != "BS-ANA0000001,BS-ANA0000002" || out.Revoked != 0 {
		t.Fatalf("Ana's listing = %s (revoked %d), want only her two live sessions and nothing revoked", got, out.Revoked)
	}
	for _, s := range out.Sessions {
		if !s.FromThisAgent || s.AuthMethod != "terminal_link" {
			t.Errorf("session %s: from_this_agent=%v auth_method=%q", s.Key, s.FromThisAgent, s.AuthMethod)
		}
	}

	// Ana names Bob's key: refused, and refused with the SAME words as a key
	// that does not exist.
	res, foreign := callTool(t, asAna, "sign_out_browsers", map[string]any{"session_key": "BS-BOB0000001"})
	if !res.IsError || !strings.Contains(foreign, "not_found:") {
		t.Fatalf("Ana revoking Bob's session: error=%v %q, want not_found", res.IsError, foreign)
	}
	_, unknown := callTool(t, asAna, "sign_out_browsers", map[string]any{"session_key": "BS-NOSUCH0001"})
	if strings.ReplaceAll(foreign, "BS-BOB0000001", "K") != strings.ReplaceAll(unknown, "BS-NOSUCH0001", "K") {
		t.Errorf("another account's key is distinguishable from an unknown one:\n%s\n%s", foreign, unknown)
	}
	t.Logf("Ana -> Bob's key: %s", foreign)
	if revoked, _ := sessionState(t, hs, "BS-BOB0000001"); revoked {
		t.Fatal("Ana's call revoked Bob's browser session")
	}

	// Ana signs out everywhere: her two live sessions, never Bob's.
	out = signOut(asAna, map[string]any{"all": true})
	t.Logf("Ana all=true: revoked=%d still=%v", out.Revoked, sessionKeys(out))
	if out.Revoked != 2 || len(out.Sessions) != 0 {
		t.Fatalf("Ana all=true: revoked %d, %d still listed; want 2 and 0", out.Revoked, len(out.Sessions))
	}
	for _, k := range []string{"BS-ANA0000001", "BS-ANA0000002"} {
		if revoked, reason := sessionState(t, hs, k); !revoked || reason != enums.BROWSER_SESSION_END_REASON_REVOKED_BY_AGENT {
			t.Errorf("%s: revoked=%v end_reason=%d, want revoked_by_agent", k, revoked, reason)
		}
	}
	if revoked, _ := sessionState(t, hs, "BS-BOB0000001"); revoked {
		t.Fatal("Ana's all=true revoked Bob's browser session")
	}

	// Bob still sees and controls his own.
	if got := strings.Join(sessionKeys(signOut(asBob, map[string]any{})), ","); got != "BS-BOB0000001" {
		t.Fatalf("Bob's listing = %s, want BS-BOB0000001", got)
	}
	if out := signOut(asBob, map[string]any{"session_key": "BS-BOB0000001"}); out.Revoked != 1 {
		t.Fatalf("Bob revoking his own session: revoked %d, want 1", out.Revoked)
	}
	// Idempotent: the same call again is ok and revokes nothing.
	if out := signOut(asBob, map[string]any{"session_key": "BS-BOB0000001"}); out.Revoked != 0 {
		t.Fatalf("repeating the revocation revoked %d, want 0", out.Revoked)
	}
	// And a revoked key of Bob's is still not_found to Ana.
	if res, text := callTool(t, asAna, "sign_out_browsers", map[string]any{"session_key": "BS-BOB0000001"}); !res.IsError || !strings.Contains(text, "not_found:") {
		t.Fatalf("Ana on Bob's revoked key: %q", text)
	}

	// Both selectors at once is a mistake, not a choice.
	if res, text := callTool(t, asAna, "sign_out_browsers", map[string]any{"session_key": "BS-ANA0000001", "all": true}); !res.IsError || !strings.Contains(text, "not both") {
		t.Fatalf("session_key with all=true: %q", text)
	}
}

// TestIntegrationSignOutBrowsersNeedsAnAccount: no token, no answer.
func TestIntegrationSignOutBrowsersNeedsAnAccount(t *testing.T) {
	hs := loginHarness(t)
	endpoint, _ := loginServer(t, hs)
	anon := connectAs(t, endpoint, "")
	res, text := callTool(t, anon, "sign_out_browsers", map[string]any{"all": true})
	if !res.IsError || !strings.Contains(text, "needs your metiche token") {
		t.Fatalf("sign_out_browsers with no token: %q", text)
	}
}
