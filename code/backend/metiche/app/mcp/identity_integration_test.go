package mcp

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/mklfarha/metiche/backend/enums"
)

// v4: the token names the agent. These tests run against the real MySQL the
// rest of the integration suite uses, and the first one runs through the REAL
// transport — the SDK's streamable HTTP client talking to mcp.Register's
// wiring behind httptest — because every earlier identity failure lived in
// what a client actually put on the wire, and a test that calls handler
// methods directly cannot see that.

// ─────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────

func agentTokenHash(t *testing.T, hs *harness, agentID uuid.UUID) string {
	t.Helper()
	var h sql.NullString
	if err := hs.core.DB().QueryRow("SELECT `token_hash` FROM `agent` WHERE `id` = ?", agentID.String()).Scan(&h); err != nil {
		t.Fatalf("reading agent.token_hash: %v", err)
	}
	return h.String
}

func accountTokenHash(t *testing.T, hs *harness, accountID uuid.UUID) string {
	t.Helper()
	var h string
	if err := hs.core.DB().QueryRow("SELECT `token_hash` FROM `account` WHERE `id` = ?", accountID.String()).Scan(&h); err != nil {
		t.Fatalf("reading account.token_hash: %v", err)
	}
	return h
}

func (hs *harness) teamSlug(t *testing.T) string {
	t.Helper()
	team, err := hs.h.teamByID(context.Background(), hs.teamID)
	if err != nil {
		t.Fatal(err)
	}
	return team.Slug
}

func (hs *harness) inviteUses(t *testing.T) int {
	t.Helper()
	return countRows(t, hs.core.DB(), "SELECT `uses` FROM `invite` WHERE `code` = ?", hs.code)
}

// bearerOnly is an MCP client's HTTP transport that adds exactly ONE thing to
// every request: Authorization. No X-Metiche-Client-Key, no other identity.
// It is what Claude Code actually delivers from a config file, per the server
// log that started v4.
type bearerOnly struct{ token string }

func (b bearerOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// realServer mounts the production wiring — mcp.Register on a chi router,
// which is authMiddleware around the SDK's streamable handler, with authTool
// around every tool — behind httptest. It records whether any request ever
// arrived carrying X-Metiche-Client-Key, so a test can prove it did not need
// one.
func realServer(t *testing.T, hs *harness) (string, *atomic.Bool) {
	t.Helper()
	t.Setenv("METICHE_ROLE", "all")
	t.Setenv("METICHE_JOIN_PER_HOUR", "1000")
	r := chi.NewRouter()
	Register(r, hs.core, zap.NewNop())
	saw := &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Metiche-Client-Key") != "" {
			saw.Store(true)
		}
		r.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/mcp", saw
}

func dialAs(endpoint, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "metiche-identity-test", Version: "0"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: bearerOnly{token: token}, Timeout: 15 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
}

func connectAs(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	cs, err := dialAs(endpoint, token)
	if err != nil {
		t.Fatalf("connecting over the streamable transport: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s over the transport: %v", name, err)
	}
	return res, resultText(t, res)
}

func sessionAgent(t *testing.T, hs *harness, sessionKey string) (agentUUID, clientKey string) {
	t.Helper()
	if err := hs.core.DB().QueryRow(
		"SELECT s.`agent_uuid`, a.`client_key` FROM `session` s JOIN `agent` a ON a.`id` = s.`agent_uuid` "+
			"WHERE s.`team_uuid` = ? AND s.`key` = ?", hs.teamID.String(), sessionKey).Scan(&agentUUID, &clientKey); err != nil {
		t.Fatalf("reading session %s: %v", sessionKey, err)
	}
	return agentUUID, clientKey
}

// ─────────────────────────────────────────────
// the end-to-end proof
// ─────────────────────────────────────────────

// TestIntegrationTwoAgentsTwoTokensNoHints is the demo failure, fixed, through
// the real transport. One person, two clients on one laptop. Each connection
// carries only its own Authorization header — no client_key argument, no
// X-Metiche-Client-Key — and each start_session lands on its own agent. Then
// one client's token is refused on the other client's session.
func TestIntegrationTwoAgentsTwoTokensNoHints(t *testing.T) {
	hs := newHarness(t)
	endpoint, sawClientKeyHeader := realServer(t, hs)
	slug := hs.teamSlug(t)

	// The first client has no token: it redeems the invite and gets token A.
	anon := connectAs(t, endpoint, "")
	res, text := callTool(t, anon, "join_team", map[string]any{
		"join_code": hs.code, "member_name": "Maykel", "agent_label": "claude on laptop",
		"client_key": "laptop-claude", "client_kind": "claude",
	})
	if res.IsError {
		t.Fatalf("join_team with the code: %s", redactSecrets(text))
	}
	var joinedA JoinTeamResult
	decodeResult(t, res, &joinedA)
	tokenA := joinedA.Token
	if tokenA == "" || joinedA.TokenScope != "agent" || joinedA.ClientKey != "laptop-claude" {
		t.Fatalf("first join: token present=%v scope=%q client_key=%q", tokenA != "", joinedA.TokenScope, joinedA.ClientKey)
	}
	usesAfterCode := hs.inviteUses(t)

	// The second client attaches by slug, carrying A. No join code.
	withA := connectAs(t, endpoint, tokenA)
	res, text = callTool(t, withA, "join_team", map[string]any{
		"team_slug": slug, "member_name": "Maykel", "agent_label": "codex on laptop",
		"client_key": "laptop-codex", "client_kind": "codex",
	})
	if res.IsError {
		t.Fatalf("join_team by slug: %s", redactSecrets(text))
	}
	var joinedB JoinTeamResult
	decodeResult(t, res, &joinedB)
	tokenB := joinedB.Token
	if tokenB == "" || tokenB == tokenA || joinedB.TokenKept {
		t.Fatalf("second client: got a token=%v, same as A=%v, kept=%v; want a distinct new token",
			tokenB != "", tokenB == tokenA, joinedB.TokenKept)
	}
	if uses := hs.inviteUses(t); uses != usesAfterCode {
		t.Errorf("join by slug spent an invite use: %d -> %d", usesAfterCode, uses)
	}

	// Each token, alone, starts a session.
	connA := connectAs(t, endpoint, tokenA)
	res, text = callTool(t, connA, "start_session", map[string]any{"project_key": "metiche", "branch": "feat/a"})
	if res.IsError {
		t.Fatalf("start_session with only token A: %s", text)
	}
	var envA Envelope
	decodeResult(t, res, &envA)

	connB := connectAs(t, endpoint, tokenB)
	res, text = callTool(t, connB, "start_session", map[string]any{"project_key": "metiche", "branch": "feat/b"})
	if res.IsError {
		t.Fatalf("start_session with only token B: %s", text)
	}
	var envB Envelope
	decodeResult(t, res, &envB)

	if envA.Key == "" || envB.Key == "" || envA.Key == envB.Key {
		t.Fatalf("session keys %q and %q, want two distinct sessions", envA.Key, envB.Key)
	}
	agentOfA, clientOfA := sessionAgent(t, hs, envA.Key)
	agentOfB, clientOfB := sessionAgent(t, hs, envB.Key)
	t.Logf("session %s -> agent %s (%s); session %s -> agent %s (%s)",
		envA.Key, agentOfA, clientOfA, envB.Key, agentOfB, clientOfB)
	if agentOfA == agentOfB {
		t.Fatal("both sessions landed on one agent")
	}
	if clientOfA != "laptop-claude" || clientOfB != "laptop-codex" {
		t.Fatalf("sessions landed on %q and %q, want laptop-claude and laptop-codex", clientOfA, clientOfB)
	}

	// A's token cannot act on B's session...
	res, text = callTool(t, withA, "end_session", map[string]any{"session_key": envB.Key})
	if !res.IsError {
		t.Fatalf("token A ended B's session: %s", text)
	}
	if !strings.Contains(text, "belongs to another of your agents") {
		t.Fatalf("token A on B's session was refused for the wrong reason: %s", text)
	}
	// ...and B's session is still B's to use.
	res, text = callTool(t, connB, "heartbeat", map[string]any{"session_key": envB.Key})
	if res.IsError {
		t.Fatalf("B's own heartbeat after A's refused attempt: %s", text)
	}

	if sawClientKeyHeader.Load() {
		t.Fatal("a request carried X-Metiche-Client-Key; this test must prove identity without it")
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `account`"); n != 1 {
		t.Errorf("%d accounts, want 1: two clients of one person", n)
	}
}

// ─────────────────────────────────────────────
// the token lifecycle
// ─────────────────────────────────────────────

// TestIntegrationRotateOneAgentNotThePerson: a join for agent X that does not
// carry X's own token issues X a new token and kills X's old one — and touches
// nothing else. Not the token it carried, not the person, not the other agent.
func TestIntegrationRotateOneAgentNotThePerson(t *testing.T) {
	hs := newHarness(t)
	claude := hs.join(t, "Ana", "laptop-claude")
	codex := hs.rejoin(t, claude, "Ana", "laptop-codex")
	if codex.agent.ID == claude.agent.ID || codex.account.ID != claude.account.ID {
		t.Fatalf("setup: want two agents of one account, got agents %s/%s accounts %s/%s",
			claude.agent.ID, codex.agent.ID, claude.account.ID, codex.account.ID)
	}
	accountHash := accountTokenHash(t, hs, claude.account.ID)
	claudeHash := agentTokenHash(t, hs, claude.agent.ID)

	// Rotate codex, carrying CLAUDE's token rather than codex's own.
	rotated := hs.rejoin(t, claude, "Ana", "laptop-codex")
	if rotated.joined.TokenKept || rotated.token == codex.token {
		t.Fatalf("a join for codex carrying claude's token must rotate codex's: kept=%v same=%v",
			rotated.joined.TokenKept, rotated.token == codex.token)
	}
	if rotated.agent.ID != codex.agent.ID {
		t.Fatalf("rotation created agent %s instead of re-using %s", rotated.agent.ID, codex.agent.ID)
	}

	if _, err := hs.h.resolveToken(context.Background(), codex.token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("codex's old token still resolves after rotation: %v", err)
	}
	if id, err := hs.h.resolveToken(context.Background(), rotated.token); err != nil || id.Agent == nil || id.Agent.ID != codex.agent.ID {
		t.Errorf("codex's new token does not name codex: err=%v", err)
	}
	if id, err := hs.h.resolveToken(context.Background(), claude.token); err != nil || id.Agent == nil || id.Agent.ID != claude.agent.ID {
		t.Errorf("the token the rotation CARRIED stopped naming claude: err=%v", err)
	}
	if got := agentTokenHash(t, hs, claude.agent.ID); got != claudeHash {
		t.Error("rotating codex changed claude's token hash")
	}
	if got := accountTokenHash(t, hs, claude.account.ID); got != accountHash {
		t.Error("rotating one agent changed the account's token hash")
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `account`"); n != 1 {
		t.Errorf("%d accounts, want 1", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `agent`"); n != 2 {
		t.Errorf("%d agents, want 2", n)
	}
}

// TestIntegrationLegacyAccountTokenStillWorks: installs made before v4 hold an
// account token and must keep working until the installer is re-run.
func TestIntegrationLegacyAccountTokenStillWorks(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "laptop-claude")

	// Make Ana a pre-v4 install: a usable ACCOUNT token, and an agent with no
	// token of its own.
	legacy, legacyHash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.core.DB().Exec("UPDATE `account` SET `token_hash` = ? WHERE `id` = ?", legacyHash, ana.account.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.core.DB().Exec("UPDATE `agent` SET `token_hash` = NULL WHERE `id` = ?", ana.agent.ID.String()); err != nil {
		t.Fatal(err)
	}

	id, err := hs.h.resolveToken(context.Background(), legacy)
	if err != nil {
		t.Fatalf("a legacy account token no longer resolves: %v", err)
	}
	if id.Agent != nil || id.Account.ID != ana.account.ID {
		t.Fatalf("legacy token resolved to account %s agent-present=%v, want account %s and no agent",
			id.Account.ID, id.Agent != nil, ana.account.ID)
	}
	if acct, err := AccountByToken(context.Background(), hs.core.DB(), legacy); err != nil || acct.ID != ana.account.ID {
		t.Fatalf("the board's entry point refused a legacy token: %v", err)
	}

	// Through the real transport with only the legacy bearer: a person with
	// one agent needs nothing else, exactly as before v4.
	endpoint, _ := realServer(t, hs)
	conn := connectAs(t, endpoint, legacy)
	res, text := callTool(t, conn, "start_session", map[string]any{"project_key": "metiche"})
	if res.IsError {
		t.Fatalf("start_session with a legacy token: %s", text)
	}
	var env Envelope
	decodeResult(t, res, &env)
	if _, client := sessionAgent(t, hs, env.Key); client != "laptop-claude" {
		t.Fatalf("legacy session landed on %q, want laptop-claude", client)
	}

	// A join carrying the legacy token mints an AGENT token for the new client
	// and leaves the account token exactly as it was.
	joinRes, _, err := hs.h.JoinTeam(hs.ctxForToken(t, legacy), nil, JoinTeamParams{
		JoinCode: hs.code, MemberName: "Ana", AgentLabel: "codex", ClientKey: "laptop-codex",
	})
	if err != nil {
		t.Fatalf("join_team carrying a legacy token: %v", err)
	}
	var out JoinTeamResult
	decodeResult(t, joinRes, &out)
	if out.Token == "" || out.TokenKept || out.TokenScope != "agent" {
		t.Fatalf("legacy join: token present=%v kept=%v scope=%q; want a fresh agent token", out.Token != "", out.TokenKept, out.TokenScope)
	}
	if got := accountTokenHash(t, hs, ana.account.ID); got != legacyHash {
		t.Error("a join carrying the legacy token rotated the account token")
	}
	if _, err := hs.h.resolveToken(context.Background(), legacy); err != nil {
		t.Errorf("the legacy token stopped working after minting an agent token: %v", err)
	}
	if id, err := hs.h.resolveToken(context.Background(), out.Token); err != nil || id.Agent == nil || id.Agent.ClientKey != "laptop-codex" {
		t.Errorf("the minted token does not name laptop-codex: err=%v", err)
	}

	// Two agents now, and the legacy token names neither: the one failure
	// left is the legacy one, and it points at the installer.
	if _, err := hs.h.RequireAgent(hs.ctxForToken(t, legacy), "", ""); err == nil ||
		!strings.Contains(err.Error(), "Re-run the metiche installer") {
		t.Errorf("legacy token with two agents: %v, want the installer hint", err)
	}
}

// TestIntegrationRetiredAgentTokenIsDead is the MCP half: a retired agent's
// token resolves to nobody — at the HTTP edge, in authTool, at the entry point
// the board uses, and on the real transport. The board gate's own half is
// TestRetiredAgentTokenIsDeadAtTheBoardGate in app/authz.
func TestIntegrationRetiredAgentTokenIsDead(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "laptop-claude")
	if _, err := hs.h.resolveToken(context.Background(), ana.token); err != nil {
		t.Fatalf("sanity: the active agent's token should resolve: %v", err)
	}

	if _, err := hs.core.DB().Exec("UPDATE `agent` SET `status` = ? WHERE `id` = ?",
		enums.AGENT_STATUS_RETIRED, ana.agent.ID.String()); err != nil {
		t.Fatal(err)
	}

	if _, err := hs.h.resolveToken(context.Background(), ana.token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a retired agent's token resolved: %v", err)
	}
	if _, err := AccountByToken(context.Background(), hs.core.DB(), ana.token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a retired agent's token resolved to its account for the board: %v", err)
	}

	// The HTTP edge.
	reached := false
	mw := hs.h.authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+ana.token)
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("retired token at the edge: status %d reached=%v, want 401", rec.Code, reached)
	}

	// authTool.
	var sawIdentity bool
	probe := func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		_, sawIdentity = IdentityFromContext(ctx)
		return nil, struct{}{}, nil
	}
	callReq := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
	callReq.Extra.Header.Set("Authorization", "Bearer "+ana.token)
	if _, _, err := authTool[struct{}, struct{}](hs.h, probe)(context.Background(), callReq, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if sawIdentity {
		t.Fatal("authTool put an identity on the context for a retired agent's token")
	}

	// The real transport: the connection itself is refused.
	endpoint, _ := realServer(t, hs)
	if cs, err := dialAs(endpoint, ana.token); err == nil {
		_ = cs.Close()
		t.Fatal("a retired agent's token opened an MCP session")
	}
}

// TestIntegrationAgentTokenWithForeignClientKeyIsRefused: an agent token may
// not act as another agent, by argument or by header.
func TestIntegrationAgentTokenWithForeignClientKeyIsRefused(t *testing.T) {
	hs := newHarness(t)
	claude := hs.join(t, "Ana", "laptop-claude")
	_ = hs.rejoin(t, claude, "Ana", "laptop-codex")

	// Its own client_key is redundant but harmless.
	if res, err := hs.h.RequireAgent(claude.ctx, "", "laptop-claude"); err != nil || res.Agent.ID != claude.agent.ID {
		t.Fatalf("agent token with its own client_key: %v", err)
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
		arg  string
	}{
		{"another agent by argument", claude.ctx, "laptop-codex"},
		{"another agent by header", WithClientKey(claude.ctx, "laptop-codex"), ""},
		{"an unknown client_key", claude.ctx, "not-an-agent"},
	} {
		_, err := hs.h.RequireAgent(c.ctx, "", c.arg)
		if err == nil {
			t.Fatalf("%s: an agent token acted as a different client_key", c.name)
		}
		if !strings.Contains(err.Error(), "this connection is agent") {
			t.Fatalf("%s: refused for the wrong reason: %v", c.name, err)
		}
	}

	// And on a real tool: no session is started under the other agent.
	if _, _, err := hs.h.StartSession(claude.ctx, nil, StartSessionParams{ProjectKey: "metiche", ClientKey: "laptop-codex"}); err == nil {
		t.Fatal("start_session with claude's token and codex's client_key succeeded")
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`"); n != 0 {
		t.Fatalf("%d sessions exist after a refused start_session, want 0", n)
	}
}

// ─────────────────────────────────────────────
// join by team slug
// ─────────────────────────────────────────────

// TestIntegrationJoinBySlugNeedsAToken: a slug is not a credential. With no
// token, a real slug and a made-up one get the same answer an unusable invite
// gets, nothing is created, and the attempt still costs join budget.
func TestIntegrationJoinBySlugNeedsAToken(t *testing.T) {
	t.Setenv("METICHE_JOIN_PER_HOUR", "2")
	hs := newHarness(t)
	slug := hs.teamSlug(t)
	ctx := WithClientIP(context.Background(), "198.51.100.20")

	for _, s := range []string{slug, "no-such-team-anywhere"} {
		_, _, err := hs.h.JoinTeam(ctx, nil, JoinTeamParams{
			TeamSlug: s, MemberName: "Mallory", AgentLabel: "x", ClientKey: "mallory-1",
		})
		if err == nil || err.Error() != ErrInviteNotUsable.Error() {
			t.Fatalf("join by slug %q with no token: %v, want exactly ErrInviteNotUsable", s, err)
		}
	}
	for _, tbl := range []string{"account", "agent", "member"} {
		if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `"+tbl+"`"); n != 0 {
			t.Errorf("a refused slug join created %d %s rows", n, tbl)
		}
	}
	if uses := hs.inviteUses(t); uses != 0 {
		t.Errorf("a refused slug join touched the invite: uses=%d", uses)
	}

	// Rate limited either way: those two refusals spent this address's budget,
	// so even a valid join code is now refused from it.
	_, _, err := hs.h.JoinTeam(ctx, nil, JoinTeamParams{
		JoinCode: hs.code, MemberName: "Mallory", AgentLabel: "x", ClientKey: "mallory-1",
	})
	if err == nil || !strings.Contains(err.Error(), "too many join attempts") {
		t.Fatalf("slug attempts did not count against the join budget: %v", err)
	}
}

// TestIntegrationJoinBySlugNeedsMembership: a token is necessary, not
// sufficient. The person behind it must be a live member of the team.
func TestIntegrationJoinBySlugNeedsMembership(t *testing.T) {
	hs := newHarness(t)
	slug := hs.teamSlug(t)
	ana := hs.join(t, "Ana", "laptop-claude")

	// An outsider: a real, active person with a real token, on another team.
	_, otherCode := seedTeam(t, hs, "Elsewhere")
	outsider, _ := joinWithCode(t, hs, context.Background(), "", otherCode, "Olga", "olga-laptop")
	if _, _, err := hs.h.JoinTeam(outsider, nil, JoinTeamParams{
		TeamSlug: slug, MemberName: "Olga", AgentLabel: "x", ClientKey: "olga-2",
	}); err == nil || err.Error() != ErrInviteNotUsable.Error() {
		t.Fatalf("a non-member joined by slug: %v, want exactly ErrInviteNotUsable", err)
	}
	// A member naming a slug that does not exist gets the same answer, so
	// "not your team" and "no such team" cannot be told apart.
	if _, _, err := hs.h.JoinTeam(ana.ctx, nil, JoinTeamParams{
		TeamSlug: "no-such-team-anywhere", MemberName: "Ana", AgentLabel: "x", ClientKey: "laptop-codex",
	}); err == nil || err.Error() != ErrInviteNotUsable.Error() {
		t.Fatalf("an unknown slug: %v, want exactly ErrInviteNotUsable", err)
	}

	// A live member attaches a second client by slug, spending nothing.
	usesBefore := hs.inviteUses(t)
	res, _, err := hs.h.JoinTeam(ana.ctx, nil, JoinTeamParams{
		TeamSlug: slug, MemberName: "Ana", AgentLabel: "codex", ClientKey: "laptop-codex",
	})
	if err != nil {
		t.Fatalf("a live member joining by slug: %v", err)
	}
	var out JoinTeamResult
	decodeResult(t, res, &out)
	if out.Token == "" || out.Token == ana.token || out.TeamSlug != slug {
		t.Fatalf("slug join: token present=%v same-as-carried=%v slug=%q", out.Token != "", out.Token == ana.token, out.TeamSlug)
	}
	if uses := hs.inviteUses(t); uses != usesBefore {
		t.Errorf("join by slug spent an invite use: %d -> %d", usesBefore, uses)
	}

	// A revoked membership is refused with the same opaque answer.
	if _, err := hs.core.DB().Exec(
		"UPDATE `member` SET `revoked_at` = UTC_TIMESTAMP() WHERE `account_uuid` = ? AND `team_uuid` = ?",
		ana.account.ID.String(), hs.teamID.String()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hs.h.JoinTeam(ana.ctx, nil, JoinTeamParams{
		TeamSlug: slug, MemberName: "Ana", AgentLabel: "cursor", ClientKey: "laptop-cursor",
	}); err == nil || err.Error() != ErrInviteNotUsable.Error() {
		t.Fatalf("a revoked member joined by slug: %v, want exactly ErrInviteNotUsable", err)
	}
}

// ─────────────────────────────────────────────
// the diagnostic gate
// ─────────────────────────────────────────────

// TestIntegrationMissingHeaderLogIsGatedOnAccountTokens: the "no agent identity
// header" line fires for a legacy account token with no client-key header, and
// for nothing else — above all not for an agent token, which is every call from
// every new install.
func TestIntegrationMissingHeaderLogIsGatedOnAccountTokens(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "laptop-claude")
	legacy, legacyHash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.core.DB().Exec("UPDATE `account` SET `token_hash` = ? WHERE `id` = ?", legacyHash, ana.account.ID.String()); err != nil {
		t.Fatal(err)
	}

	obs, logs := observer.New(zapcore.InfoLevel)
	hs.h.logger = zap.New(obs)
	wrapped := authTool[struct{}, struct{}](hs.h, passThrough)
	call := func(bearer, clientKey string) int {
		req := &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{Name: "start_session"},
			Extra:  &mcp.RequestExtra{Header: http.Header{}},
		}
		req.Extra.Header.Set("Authorization", "Bearer "+bearer)
		if clientKey != "" {
			req.Extra.Header.Set("X-Metiche-Client-Key", clientKey)
		}
		if _, _, err := wrapped(context.Background(), req, struct{}{}); err != nil {
			t.Fatal(err)
		}
		return logs.FilterMessage(noIdentityHeaderMsg).Len()
	}

	if n := call(ana.token, ""); n != 0 {
		t.Fatalf("an AGENT token with no header logged the diagnostic (%d lines); that is the normal case", n)
	}
	if n := call(legacy, "laptop-claude"); n != 0 {
		t.Fatalf("a legacy token WITH the header logged the diagnostic (%d lines)", n)
	}
	if n := call(legacy, ""); n != 1 {
		t.Fatalf("a legacy token with no header logged %d lines, want exactly 1", n)
	}
	assertNoHeaderValues(t, logs, ana.token, legacy, "Bearer ")
}
