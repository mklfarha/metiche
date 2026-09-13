package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/config"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/core"
	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	agent_entity "github.com/mklfarha/metiche/backend/entity/agent"
	member_entity "github.com/mklfarha/metiche/backend/entity/member"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// These tests run against a REAL MySQL. There is no sqlite fallback and no
// mock, deliberately: the two things they prove — that a retry replays instead
// of re-applying, and that concurrent writers produce a gapless sequence —
// are properties of InnoDB row locks and unique indexes. A fake would prove
// that the fake works.
//
// Run them with:
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&interpolateParams=true' \
//
// interpolateParams=true is not decoration: it is what production runs
// (config/base.yaml recommends it), and it changes how []byte arguments reach
// MySQL. Without it, a []byte bound to a JSON column works; with it, the
// driver sends a binary literal and MySQL rejects it with error 3144. Leaving
// it off here once hid exactly that bug until a real deployment found it.
//
//	go test ./app/mcp/ -run Integration -v
//
// The database must already have core/repository/sql/schema/create.sql
// applied. No DSN is committed anywhere in this repository — a connection
// string in a public repo is the security gate this project set for itself.
const dsnEnv = "METICHE_TEST_MYSQL_DSN"

type harness struct {
	h      *Handler
	core   *core.Implementation
	teamID uuid.UUID
	planID uuid.UUID
	// code is the team's INVITE code. v3 removed team.join_code, so the
	// harness seeds an invite row instead of a column.
	code string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv(dsnEnv))
	if dsn == "" {
		t.Skipf("skipped: %s is not set, so there is no MySQL to run the write path against. "+
			"Apply core/repository/sql/schema/create.sql to a database and set %s to its DSN.", dsnEnv, dsnEnv)
	}

	name, rest, ok := splitDSN(dsn)
	if !ok {
		t.Fatalf("%s does not look like a MySQL DSN (user:pass@tcp(host:port)/dbname?params)", dsnEnv)
	}
	yaml := fmt.Sprintf("db:\n  - name: %q\n    host: %q\n    port: %q\n    user: %q\n    pswd: %q\n    params: %q\n    driver: \"mysql\"\n",
		name.db, name.host, name.port, name.user, name.pass, rest)

	provider, err := config.NewYAML(config.Source(strings.NewReader(yaml)))
	if err != nil {
		t.Fatalf("building the test config: %v", err)
	}
	impl, err := core.New(core.Params{Provider: provider, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("connecting to MySQL: %v", err)
	}
	t.Cleanup(impl.Destroy)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := impl.DB().PingContext(ctx); err != nil {
		t.Skipf("skipped: %s is set but the database is unreachable: %v", dsnEnv, err)
	}

	truncateAll(t, impl.DB())

	hh := NewHandler(impl, zap.NewNop())
	hs := &harness{h: hh, core: impl, code: "JOIN" + strings.ToUpper(uuid.Must(uuid.NewV4()).String()[:6])}

	// A plan with is_instance_default. Seeded rather than assumed because
	// create_team REFUSES to make a team with no plan — a team with no plan
	// is a team with no limits — and an empty `plan` table is exactly the
	// state a fresh database is in.
	hs.planID = uuid.Must(uuid.NewV4())
	if _, err := impl.DB().Exec(
		"INSERT INTO `plan` (`id`,`key`,`name`,`max_concurrent_agents`,`retention_days`,`max_members`,"+
			"`max_projects`,`is_instance_default`,`sort_order`,`status`) VALUES (?,?,?,?,?,?,?,?,?,?)",
		hs.planID.String(), "free", "Free", 5, 30, 10, 5, true, 0, enums.RECORD_STATUS_ACTIVE); err != nil {
		t.Fatalf("seeding the instance default plan: %v", err)
	}

	hs.teamID = uuid.Must(uuid.NewV4())
	planID := hs.planID
	if _, err := impl.Team().Insert(context.Background(), team_types.UpsertRequest{
		Team: team_entity.Team{
			ID:                      hs.teamID,
			Name:                    "Test team",
			Slug:                    "test-" + hs.teamID.String()[:8],
			Status:                  enums.RECORD_STATUS_ACTIVE,
			PlanUUID:                &planID,
			PlanSource:              enums.PLAN_SOURCE_INSTANCE_DEFAULT,
			Visibility:              enums.TEAM_VISIBILITY_PRIVATE,
			RequiresClaimedAccounts: false,
		},
	}, teammod.WithSkipCache()); err != nil {
		t.Fatalf("seeding the team: %v", err)
	}

	// The invite that replaced team.join_code: uncapped and unexpiring, so a
	// test can redeem it as many times as it likes.
	if _, err := impl.DB().Exec(
		"INSERT INTO `invite` (`id`,`team_uuid`,`code`,`label`,`uses`,`status`) VALUES (?,?,?,?,?,?)",
		uuid.Must(uuid.NewV4()).String(), hs.teamID.String(), hs.code, "harness", 0,
		enums.INVITE_STATUS_ACTIVE); err != nil {
		t.Fatalf("seeding the invite: %v", err)
	}
	return hs
}

type dsnParts struct{ user, pass, host, port, db string }

// splitDSN pulls a go-sql-driver DSN apart, because core.New takes the pieces
// rather than the string.
func splitDSN(dsn string) (dsnParts, string, bool) {
	var p dsnParts
	at := strings.LastIndex(dsn, "@tcp(")
	if at < 0 {
		return p, "", false
	}
	creds := dsn[:at]
	if i := strings.Index(creds, ":"); i >= 0 {
		p.user, p.pass = creds[:i], creds[i+1:]
	} else {
		p.user = creds
	}
	rest := dsn[at+len("@tcp("):]
	close := strings.Index(rest, ")")
	if close < 0 {
		return p, "", false
	}
	hostport := rest[:close]
	p.host, p.port = hostport, "3306"
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		p.host, p.port = hostport[:i], hostport[i+1:]
	}
	tail := strings.TrimPrefix(rest[close+1:], "/")
	params := ""
	if i := strings.Index(tail, "?"); i >= 0 {
		p.db, params = tail[:i], tail[i+1:]
	} else {
		p.db = tail
	}
	if !strings.Contains(params, "parseTime") {
		if params != "" {
			params += "&"
		}
		params += "parseTime=true"
	}
	return p, params, p.db != ""
}

// truncateAll gives every test a clean world. FK checks are dropped around it
// because the tables reference each other and there is no order that satisfies
// all of them.
func truncateAll(t *testing.T, db *sql.DB) {
	t.Helper()
	tables := []string{
		"team_event", "instruction", "judgement", "conflict_participant", "conflict",
		"contract_field", "contract_assertion", "contract", "decision_token", "decision_path",
		"decision", "intent_token", "claim_path", "claim", "intent", "session", "project",
		"invite", "notification_channel", "limit_event",
		"agent", "member", "team", "account", "plan",
	}
	if _, err := db.Exec("SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("disabling FK checks: %v", err)
	}
	for _, tbl := range tables {
		if _, err := db.Exec("TRUNCATE TABLE `" + tbl + "`"); err != nil {
			t.Fatalf("truncating %s: %v", tbl, err)
		}
	}
	if _, err := db.Exec("SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		t.Fatalf("re-enabling FK checks: %v", err)
	}
}

// caller is one authenticated person, with the three v3 identities that used
// to be collapsed into one agent row: the account (the person, and what the
// token resolves to), the member (that person on this team) and the agent
// (one client process of theirs).
type caller struct {
	ctx     context.Context
	token   string
	account account_entity.Account
	member  member_entity.Member
	agent   agent_entity.Agent
}

// join runs the real join_team tool as a BRAND NEW anonymous person and
// returns their authenticated context.
//
// New, because that is what a request with no token means in v3: first
// contact mints an account. Re-joining as somebody who already exists means
// presenting their token, which is what rejoin does.
func (hs *harness) join(t *testing.T, memberName, clientKey string) *caller {
	t.Helper()
	return hs.joinAsCtx(t, context.Background(), "", memberName, clientKey)
}

// rejoin re-runs join_team carrying an existing person's token, which is how
// a restarted agent comes back as itself.
func (hs *harness) rejoin(t *testing.T, prev *caller, memberName, clientKey string) *caller {
	t.Helper()
	return hs.joinAsCtx(t, WithAccount(context.Background(), prev.account), prev.token, memberName, clientKey)
}

func (hs *harness) joinAsCtx(t *testing.T, base context.Context, carried, memberName, clientKey string) *caller {
	t.Helper()
	res, _, err := hs.h.JoinTeam(base, nil, JoinTeamParams{
		JoinCode:   hs.code,
		MemberName: memberName,
		AgentLabel: "test",
		ClientKey:  clientKey,
	})
	if err != nil {
		t.Fatalf("join_team: %v", err)
	}
	var out JoinTeamResult
	decodeResult(t, res, &out)

	token := out.Token
	switch {
	case carried == "" && token == "":
		t.Fatal("a first contact must mint a token")
	case carried != "" && token != "":
		t.Fatal("join_team minted a second token for a caller that already had one")
	case token == "":
		token = carried
	}
	return hs.callerFor(t, token, out.AgentKey)
}

// ctxForToken is the cheap half of callerFor: an authenticated context and
// nothing else, for the calls that only need to be somebody.
func (hs *harness) ctxForToken(t *testing.T, token string) context.Context {
	t.Helper()
	acct, err := hs.h.resolveToken(context.Background(), token)
	if err != nil {
		t.Fatalf("the token does not resolve: %v", err)
	}
	return WithAccount(context.Background(), acct)
}

// callerFor resolves a token all the way to the three identities behind it.
func (hs *harness) callerFor(t *testing.T, token, agentKey string) *caller {
	t.Helper()
	acct, err := hs.h.resolveToken(context.Background(), token)
	if err != nil {
		t.Fatalf("the token does not resolve: %v", err)
	}
	ctx := WithAccount(context.Background(), acct)

	member, found, err := hs.h.memberByAccount(ctx, nil, acct.ID, hs.teamID)
	if err != nil || !found {
		t.Fatalf("no membership for the account that just joined: found=%v err=%v", found, err)
	}
	agents, err := hs.h.activeAgents(ctx, acct.ID)
	if err != nil {
		t.Fatalf("listing the account's agents: %v", err)
	}
	var ag agent_entity.Agent
	for _, a := range agents {
		if agentKey == "" || a.Key == agentKey {
			ag = a
			break
		}
	}
	if ag.ID.IsNil() {
		t.Fatalf("no agent %q for the account that just joined", agentKey)
	}
	return &caller{ctx: ctx, token: token, account: acct, member: member, agent: ag}
}

func decodeResult(t *testing.T, res *mcp.CallToolResult, into any) {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("tool returned no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tool returned %T, want text content", res.Content[0])
	}
	if err := json.Unmarshal([]byte(text.Text), into); err != nil {
		t.Fatalf("tool result is not JSON: %v\n%s", err, text.Text)
	}
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("tool returned no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tool returned %T, want text content", res.Content[0])
	}
	return text.Text
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// ─────────────────────────────────────────────
// (a) Idempotency
// ─────────────────────────────────────────────

// TestIntegrationIdempotentReplay is the first half of the proof PLAN.md asks
// for: the same call twice returns byte-identical responses and writes exactly
// one event.
//
// Byte-identical, not merely equivalent. A retry that re-renders its response
// would quietly drop the conflicts the caller was told about the first time,
// and the retry would look cleaner than the original — which is the worst
// possible direction for this particular lie.
func TestIntegrationIdempotentReplay(t *testing.T) {
	hs := newHarness(t)
	ctx := hs.join(t, "Ana", "client-a").ctx

	args := StartSessionParams{
		ProjectKey:     "metiche",
		Branch:         "feat/auth",
		BaseCommit:     "abc1234",
		Goal:           "build the login endpoint",
		IdempotencyKey: "retry-me",
	}

	first, _, err := hs.h.StartSession(ctx, nil, args)
	if err != nil {
		t.Fatalf("first start_session: %v", err)
	}
	second, _, err := hs.h.StartSession(ctx, nil, args)
	if err != nil {
		t.Fatalf("retried start_session: %v", err)
	}

	a, b := resultText(t, first), resultText(t, second)
	if a != b {
		t.Errorf("a retry answered differently:\nfirst:  %s\nsecond: %s", a, b)
	}
	t.Logf("both calls returned: %s", a)

	events := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), enums.EVENT_KIND_SESSION_STARTED)
	if events != 1 {
		t.Errorf("the retry wrote %d session_started events, want exactly 1", events)
	}
	sessions := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ?", hs.teamID.String())
	if sessions != 1 {
		t.Errorf("the retry created %d sessions, want exactly 1", sessions)
	}

	// And the stored snapshot IS the bytes, not a reconstruction of them.
	var stored []byte
	if err := hs.core.DB().QueryRow(
		"SELECT `response_snapshot` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), enums.EVENT_KIND_SESSION_STARTED).Scan(&stored); err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}
	// The first response is the envelope plus the session key the tool
	// attached after commit; the snapshot is the envelope commit rendered.
	// What must hold is that the REPLAY equals the FIRST RESPONSE, which is
	// asserted above; here we only check a snapshot was stored at all.
	if len(stored) == 0 {
		t.Error("no response_snapshot was stored, so nothing could be replayed")
	}
}

// TestIntegrationJoinTeamNeverPersistsAToken is the security half of the same
// mechanism. A replayable response is a persisted response, so join_team
// deliberately opts out — and this proves the opt-out actually holds.
func TestIntegrationJoinTeamNeverPersistsAToken(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	rows, err := hs.core.DB().Query("SELECT COALESCE(`response_snapshot`, ''), COALESCE(`summary`, ''), COALESCE(CAST(`payload` AS CHAR), '') FROM `team_event`")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var snap, summary, payload string
		if err := rows.Scan(&snap, &summary, &payload); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{snap, summary, payload} {
			if strings.Contains(field, tokenPrefix) {
				t.Errorf("a bearer token reached the event log: %s", field)
			}
		}
	}

	// v3 moved the hash off the agent and onto the ACCOUNT, and it is still
	// only ever a hash. The agent table no longer has the column at all,
	// which is asserted here so a regeneration that brought it back would be
	// caught rather than quietly re-splitting the credential.
	var stored string
	if err := hs.core.DB().QueryRow("SELECT `token_hash` FROM `account` WHERE `id` = ?", ana.account.ID.String()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 64 || strings.HasPrefix(stored, tokenPrefix) {
		t.Errorf("account.token_hash = %q, want a 64-character sha256 digest", stored)
	}
	var cols int
	if err := hs.core.DB().QueryRow(
		"SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() " +
			"AND table_name = 'agent' AND column_name = 'token_hash'").Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Error("agent.token_hash is back; the token belongs to the account in v3")
	}
}

// TestIntegrationRejoinIsSameAgent: uq_agent_account_client is what keeps a
// restarted agent from appearing twice on the board.
//
// v3 changed both halves of this. The uniqueness moved from
// (member, client_key) to (account, client_key), and the token no longer
// rotates on a re-join — it lives on the ACCOUNT now, one per person, and
// rotating it because one of that person's three agents restarted would log
// the other two out.
func TestIntegrationRejoinIsSameAgent(t *testing.T) {
	hs := newHarness(t)
	first := hs.join(t, "Ana", "client-a")
	second := hs.rejoin(t, first, "Ana", "client-a")

	if first.agent.ID != second.agent.ID {
		t.Errorf("re-joining created a second agent: %s then %s", first.agent.ID, second.agent.ID)
	}
	if first.account.ID != second.account.ID {
		t.Errorf("re-joining created a second account: %s then %s", first.account.ID, second.account.ID)
	}
	if first.member.ID != second.member.ID {
		t.Errorf("re-joining created a second membership: %s then %s", first.member.ID, second.member.ID)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `account`"); n != 1 {
		t.Errorf("%d accounts, want 1", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `member`"); n != 1 {
		t.Errorf("%d members, want 1", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `agent`"); n != 1 {
		t.Errorf("%d agents, want 1", n)
	}

	// The original token still works: one person, one credential.
	if _, err := hs.h.resolveToken(context.Background(), first.token); err != nil {
		t.Errorf("re-joining invalidated the caller's own token: %v", err)
	}
	if first.account.TokenHash != second.account.TokenHash {
		t.Error("re-joining rotated the account token; that would log the person's other agents out")
	}
}

// TestIntegrationOnePersonManyTeams is the property v3 exists for: an account
// is a person ACROSS teams, so one token reaches both boards and the team is
// a per-call scope rather than something baked into the credential.
func TestIntegrationOnePersonManyTeams(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	// A second team, with its own invite, joined with the SAME token.
	secondTeam := uuid.Must(uuid.NewV4())
	planID := hs.planID
	if _, err := hs.core.Team().Insert(context.Background(), team_types.UpsertRequest{
		Team: team_entity.Team{
			ID: secondTeam, Name: "Other team", Slug: "other-" + secondTeam.String()[:8],
			Status: enums.RECORD_STATUS_ACTIVE, PlanUUID: &planID,
			PlanSource: enums.PLAN_SOURCE_INSTANCE_DEFAULT, Visibility: enums.TEAM_VISIBILITY_PRIVATE,
		},
	}, teammod.WithSkipCache()); err != nil {
		t.Fatal(err)
	}
	otherCode := "OTHR" + strings.ToUpper(secondTeam.String()[:6])
	if _, err := hs.core.DB().Exec(
		"INSERT INTO `invite` (`id`,`team_uuid`,`code`,`uses`,`status`) VALUES (?,?,?,?,?)",
		uuid.Must(uuid.NewV4()).String(), secondTeam.String(), otherCode, 0, enums.INVITE_STATUS_ACTIVE); err != nil {
		t.Fatal(err)
	}

	res, _, err := hs.h.JoinTeam(ana.ctx, nil, JoinTeamParams{
		JoinCode: otherCode, MemberName: "Ana", AgentLabel: "test", ClientKey: "client-a",
	})
	if err != nil {
		t.Fatalf("joining a second team with the same token: %v", err)
	}
	var out JoinTeamResult
	decodeResult(t, res, &out)
	if out.Token != "" {
		t.Error("joining a second team minted a second token; one person has one credential")
	}

	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `account`"); n != 1 {
		t.Errorf("%d accounts, want 1 — the same person joined twice", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `member`"); n != 2 {
		t.Errorf("%d memberships, want 2 — one per team", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `agent`"); n != 1 {
		t.Errorf("%d agents, want 1 — an agent belongs to a person, not a team", n)
	}

	// With two live memberships the team is genuinely ambiguous, so a
	// team-scoped call must refuse rather than guess which board to write to.
	if _, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{ProjectKey: "metiche"}); err == nil {
		t.Error("start_session picked a team for a caller who is on two")
	} else if !strings.Contains(err.Error(), "team_slug") {
		t.Errorf("the refusal should name the missing argument: %v", err)
	}

	teamA, err := hs.h.teamByID(context.Background(), hs.teamID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{
		ProjectKey: "metiche", TeamSlug: teamA.Slug}); err != nil {
		t.Fatalf("start_session with an explicit team_slug: %v", err)
	}
}

// TestIntegrationInviteRedemption covers the table that replaced
// team.join_code: every reason an invite stops working, and the use counter.
func TestIntegrationInviteRedemption(t *testing.T) {
	hs := newHarness(t)

	seed := func(code string, maxUses any, expiresAt any, revokedAt any, status int) {
		t.Helper()
		if _, err := hs.core.DB().Exec(
			"INSERT INTO `invite` (`id`,`team_uuid`,`code`,`max_uses`,`uses`,`expires_at`,`revoked_at`,`status`) "+
				"VALUES (?,?,?,?,?,?,?,?)",
			uuid.Must(uuid.NewV4()).String(), hs.teamID.String(), code, maxUses, 0, expiresAt, revokedAt, status); err != nil {
			t.Fatalf("seeding invite %s: %v", code, err)
		}
	}
	past := time.Now().UTC().Add(-time.Hour)
	seed("CAPPED0001", 1, nil, nil, int(enums.INVITE_STATUS_ACTIVE))
	seed("EXPIRED001", nil, past, nil, int(enums.INVITE_STATUS_ACTIVE))
	seed("REVOKED001", nil, nil, past, int(enums.INVITE_STATUS_ACTIVE))
	seed("RETIRED001", nil, nil, nil, int(enums.INVITE_STATUS_REVOKED))

	for _, code := range []string{"NOSUCHCODE", "EXPIRED001", "REVOKED001", "RETIRED001"} {
		if _, _, err := hs.h.redeemInvite(context.Background(), code); err == nil {
			t.Errorf("redeemed %s, which is not usable", code)
		} else if strings.Contains(err.Error(), code) {
			t.Errorf("the refusal echoed the code back: %v", err)
		}
	}

	// The use cap is enforced, and the counter and status move with it.
	inv, team, err := hs.h.redeemInvite(context.Background(), "capped0001")
	if err != nil {
		t.Fatalf("redeeming a usable invite: %v", err)
	}
	if team.ID != hs.teamID {
		t.Errorf("redeemed onto team %s, want %s", team.ID, hs.teamID)
	}
	if inv.Uses != 1 {
		t.Errorf("uses = %d after one redemption, want 1", inv.Uses)
	}
	var uses, status int64
	var lastUsed sql.NullTime
	if err := hs.core.DB().QueryRow(
		"SELECT `uses`, `status`, `last_used_at` FROM `invite` WHERE `code` = ?", "CAPPED0001").
		Scan(&uses, &status, &lastUsed); err != nil {
		t.Fatal(err)
	}
	if uses != 1 {
		t.Errorf("invite.uses = %d, want 1", uses)
	}
	if !lastUsed.Valid {
		t.Error("invite.last_used_at was not set")
	}
	if status != int64(enums.INVITE_STATUS_EXHAUSTED) {
		t.Errorf("invite.status = %d, want exhausted (%d) once max_uses was reached", status, enums.INVITE_STATUS_EXHAUSTED)
	}
	if _, _, err := hs.h.redeemInvite(context.Background(), "CAPPED0001"); err == nil {
		t.Error("a max_uses=1 invite was redeemed twice")
	}
}

// ─────────────────────────────────────────────
// (b) Concurrency — this is the test that proves the lock
// ─────────────────────────────────────────────

// TestIntegrationConcurrentWritesAreGapless fires N goroutines at the write
// path at once and demands a sequence with no holes, no duplicates and no
// lost updates.
//
// Without SELECT ... FOR UPDATE on the team row this fails in two ways at
// once: two writers read the same last sequence and try to write the same next
// one — one of them dies on uq_team_event_sequence and takes its snapshot
// change down with it — and team.sequence ends up behind the log, because the
// last writer to commit writes back a value it read before the others landed.
func TestIntegrationConcurrentWritesAreGapless(t *testing.T) {
	hs := newHarness(t)
	// Four agents rather than one: an agent may hold at most
	// maxLiveSessionsPerAgent open sessions, and this test is about the lock,
	// not that cap. Four people on one team still serialize on one team row.
	names := []string{"Ana", "Bob", "Cai", "Dee"}
	ctxs := make([]context.Context, len(names))
	for i, name := range names {
		ctxs[i] = hs.join(t, name, fmt.Sprintf("client-%d", i)).ctx
	}

	const n = 24
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		seqs []int64
	)
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them all at the same instant
			res, _, err := hs.h.StartSession(ctxs[i%len(ctxs)], nil, StartSessionParams{
				ProjectKey:     "metiche",
				Branch:         fmt.Sprintf("feat/%d", i),
				Goal:           fmt.Sprintf("piece of work %d", i),
				IdempotencyKey: fmt.Sprintf("concurrent-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			var env Envelope
			decodeResult(t, res, &env)
			seqs = append(seqs, env.Sequence)
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("a concurrent start_session failed: %v", err)
	}
	if len(seqs) != n {
		t.Fatalf("%d of %d calls returned a sequence", len(seqs), n)
	}

	// Every caller got a distinct sequence.
	seen := map[int64]bool{}
	for _, s := range seqs {
		if seen[s] {
			t.Errorf("sequence %d was handed to two callers", s)
		}
		seen[s] = true
	}

	// The log itself is gapless. Each join_team wrote member_joined and
	// agent_joined first, so the log is 1..n+2*agents with nothing missing.
	rows, err := hs.core.DB().Query(
		"SELECT `sequence` FROM `team_event` WHERE `team_uuid` = ? ORDER BY `sequence` ASC", hs.teamID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var log []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		log = append(log, s)
	}
	wantLen := n + 2*len(names) // member_joined + agent_joined, per agent
	if len(log) != wantLen {
		t.Fatalf("the log has %d events, want %d", len(log), wantLen)
	}
	for i, s := range log {
		if s != int64(i+1) {
			t.Fatalf("the sequence has a hole: event %d carries sequence %d", i, s)
		}
	}

	// No lost update: the team's cursor matches the log's last sequence.
	var teamSeq, teamRev int64
	if err := hs.core.DB().QueryRow(
		"SELECT `sequence`, `board_revision` FROM `team` WHERE `id` = ?", hs.teamID.String()).
		Scan(&teamSeq, &teamRev); err != nil {
		t.Fatal(err)
	}
	if teamSeq != int64(wantLen) {
		t.Errorf("team.sequence = %d but the log ends at %d — an update was lost", teamSeq, wantLen)
	}
	// Every event here is structural, so the two cursors moved together.
	if teamRev != int64(wantLen) {
		t.Errorf("team.board_revision = %d, want %d (every event in this test is structural)", teamRev, wantLen)
	}

	// And N sessions exist, one per caller: nobody's Apply was rolled back.
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`"); got != n {
		t.Errorf("%d sessions, want %d — a snapshot change was rolled back", got, n)
	}
	// Exactly one project, though all N raced to create it.
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `project`"); got != 1 {
		t.Errorf("%d projects, want 1 — the create-on-first-use raced", got)
	}

	stats := hs.h.LockHoldStats()
	t.Logf("%s: count=%d p50=%.0f p95=%.0f p99=%.0f max=%.2f mean=%.2f",
		stats.Name, stats.Count, stats.P50MS, stats.P95MS, stats.P99MS, stats.MaxMS, stats.MeanMS)
	if stats.Count == 0 {
		t.Error("the lock-hold histogram recorded nothing")
	}
	if stats.P99MS > 25 {
		t.Errorf("%s p99 = %.0fms, over the 25ms tripwire", stats.Name, stats.P99MS)
	}
}

// TestIntegrationConcurrentRetriesOfOneCall is the other half of the race:
// N goroutines retrying the SAME call must produce one event and one answer.
func TestIntegrationConcurrentRetriesOfOneCall(t *testing.T) {
	hs := newHarness(t)
	ctx := hs.join(t, "Ana", "client-a").ctx

	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		answers []string
		errs    []error
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, _, err := hs.h.StartSession(ctx, nil, StartSessionParams{
				ProjectKey:     "metiche",
				Goal:           "the one piece of work",
				IdempotencyKey: "the-same-key",
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			answers = append(answers, resultText(t, res))
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("a concurrent retry failed: %v", err)
	}
	if len(answers) != n {
		t.Fatalf("%d of %d retries answered", len(answers), n)
	}
	for i, a := range answers[1:] {
		if a != answers[0] {
			t.Errorf("retry %d answered differently:\n%s\n%s", i+1, answers[0], a)
		}
	}
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`"); got != 1 {
		t.Errorf("%d sessions, want 1", got)
	}
	if got := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `team_event` WHERE `kind` = ?", enums.EVENT_KIND_SESSION_STARTED); got != 1 {
		t.Errorf("%d session_started events, want 1", got)
	}
}

// ─────────────────────────────────────────────
// The two cursors, end to end
// ─────────────────────────────────────────────

// TestIntegrationTwoCursors proves the distinction is real in the database and
// not just in a comment: ending a session moves sequence and leaves
// board_revision where it was, because the lane is already on the board.
func TestIntegrationTwoCursors(t *testing.T) {
	hs := newHarness(t)
	ctx := hs.join(t, "Ana", "client-a").ctx

	res, _, err := hs.h.StartSession(ctx, nil, StartSessionParams{ProjectKey: "metiche", Goal: "work"})
	if err != nil {
		t.Fatal(err)
	}
	var started Envelope
	decodeResult(t, res, &started)
	t.Logf("start_session  -> %s", resultText(t, res))

	res, _, err = hs.h.Heartbeat(ctx, nil, HeartbeatParams{SessionKey: started.Key, StatusLine: "editing auth.go"})
	if err != nil {
		t.Fatal(err)
	}
	var beat Envelope
	decodeResult(t, res, &beat)
	t.Logf("heartbeat      -> %s", resultText(t, res))

	// A heartbeat takes no lock and writes no event, so NEITHER cursor moves.
	if beat.Sequence != started.Sequence || beat.Revision != started.Revision {
		t.Errorf("heartbeat moved a cursor: %d/%d -> %d/%d",
			started.Sequence, started.Revision, beat.Sequence, beat.Revision)
	}
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`"); got != 3 {
		t.Errorf("%d events after a heartbeat, want 3 (member_joined, agent_joined, session_started)", got)
	}

	res, _, err = hs.h.EndSession(ctx, nil, EndSessionParams{SessionKey: started.Key})
	if err != nil {
		t.Fatal(err)
	}
	var ended Envelope
	decodeResult(t, res, &ended)
	t.Logf("end_session    -> %s", resultText(t, res))

	if ended.Sequence != started.Sequence+1 {
		t.Errorf("sequence = %d, want %d: every event advances it", ended.Sequence, started.Sequence+1)
	}
	if ended.Revision != started.Revision {
		t.Errorf("board_revision = %d, want %d: ending a session is a repaint, not a re-layout",
			ended.Revision, started.Revision)
	}
}

// TestIntegrationHeartbeatExtendsClaimsWithoutTheLock covers the claim TTL
// side of the most frequent call, including the hard cap a heartbeat may not
// push past.
func TestIntegrationHeartbeatExtendsClaimsWithoutTheLock(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")
	ctx := ana.ctx

	res, _, err := hs.h.StartSession(ctx, nil, StartSessionParams{ProjectKey: "metiche", Goal: "work"})
	if err != nil {
		t.Fatal(err)
	}
	var started Envelope
	decodeResult(t, res, &started)

	who, err := hs.h.RequireSession(ctx, started.Key)
	if err != nil {
		t.Fatal(err)
	}
	sess := who.Session

	// One claim expiring soon, with a hard cap only a minute out. Inserted
	// directly: declare_intent, which is what creates claims in the product,
	// is a later wave.
	now := time.Now().UTC()
	claimID := uuid.Must(uuid.NewV4())
	if _, err := hs.core.DB().Exec(
		"INSERT INTO `claim` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`mode`,`status`,"+
			"`ttl_seconds`,`expires_at`,`hard_expires_at`,`breadth_score`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		claimID.String(), hs.teamID.String(), sess.ProjectUUID.String(), sess.ID.String(), ana.member.ID.String(),
		"C-1", enums.CLAIM_MODE_WRITE, enums.CLAIM_STATUS_HELD, 900,
		now.Add(30*time.Second), now.Add(time.Minute), 0, now, now); err != nil {
		t.Fatalf("seeding a claim: %v", err)
	}

	if _, _, err := hs.h.Heartbeat(ctx, nil, HeartbeatParams{SessionKey: started.Key}); err != nil {
		t.Fatal(err)
	}

	var expires, hard time.Time
	if err := hs.core.DB().QueryRow(
		"SELECT `expires_at`, `hard_expires_at` FROM `claim` WHERE `id` = ?", claimID.String()).
		Scan(&expires, &hard); err != nil {
		t.Fatal(err)
	}
	if !expires.After(now.Add(30 * time.Second)) {
		t.Errorf("the heartbeat did not extend the claim: expires_at = %v", expires)
	}
	if expires.After(hard) {
		t.Errorf("the heartbeat pushed a claim past its hard cap: %v > %v", expires, hard)
	}

	// And no event was written for it: the heartbeat is outside the log.
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`"); got != 3 {
		t.Errorf("%d events, want 3 — heartbeat must not write one", got)
	}

	// end_session releases it, so a teammate is unblocked now rather than in
	// fifteen minutes.
	if _, _, err := hs.h.EndSession(ctx, nil, EndSessionParams{SessionKey: started.Key}); err != nil {
		t.Fatal(err)
	}
	var status int64
	if err := hs.core.DB().QueryRow("SELECT `status` FROM `claim` WHERE `id` = ?", claimID.String()).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != int64(enums.CLAIM_STATUS_RELEASED) {
		t.Errorf("claim status = %d, want released (%d)", status, enums.CLAIM_STATUS_RELEASED)
	}
}

// TestIntegrationDetectionRunsInsideTheLock is the seam's proof.
//
// The hook is handed the open transaction and the sequence the pending event
// will carry, and what it INSERTS there lands atomically with the event — a
// conflict raised by a call and the event that caused it can never be half
// present. It also shows the counts on the response are taken after the hook,
// so a conflict this call raised is visible in its own answer rather than one
// call later.
func TestIntegrationDetectionRunsInsideTheLock(t *testing.T) {
	hs := newHarness(t)
	ctx := hs.join(t, "Ana", "client-a").ctx

	res, _, err := hs.h.StartSession(ctx, nil, StartSessionParams{ProjectKey: "metiche", Goal: "work"})
	if err != nil {
		t.Fatal(err)
	}
	var started Envelope
	decodeResult(t, res, &started)
	who, err := hs.h.RequireSession(ctx, started.Key)
	if err != nil {
		t.Fatal(err)
	}
	sess := who.Session

	var sawSequence int64
	var sawUncommittedEvent int
	hs.h.SetDetector(func(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
		sawSequence = tc.Sequence

		// The event this hook is running for does not exist yet: detection
		// happens BEFORE the insert, which is what lets it decide what the
		// insert should say.
		if err := tc.Tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `sequence` = ?",
			tc.TeamUUID.String(), tc.Sequence).Scan(&sawUncommittedEvent); err != nil {
			return nil, err
		}

		// Insert on the same transaction, under the same row lock. This is
		// the whole point of the hook: check and insert cannot be separated.
		id := uuid.Must(uuid.NewV4())
		if _, err := tc.Tx.ExecContext(ctx,
			"INSERT INTO `instruction` (`id`,`team_uuid`,`target_session_uuid`,`key`,`source`,`kind`,`body`,"+
				"`requires_report`,`status`,`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			id.String(), tc.TeamUUID.String(), sess.ID.String(), tc.Key("INT"),
			enums.INSTRUCTION_SOURCE_SERVER, enums.INSTRUCTION_KIND_CONFLICT_NOTICE,
			"Ana holds auth.go (write, feat/auth); consume her POST /api/login contract instead",
			false, enums.INSTRUCTION_STATUS_PENDING, tc.Now, tc.Now); err != nil {
			return nil, err
		}

		return []ConflictNotice{{
			Key: tc.Key("CF"), Kind: "path_overlap", Severity: "high",
			With:            "ana/backend",
			Paths:           []string{"auth.go"},
			SuggestedAction: "consume her POST /api/login contract instead",
		}}, nil
	})
	t.Cleanup(func() { hs.h.SetDetector(nil) })

	res, _, err = hs.h.EndSession(ctx, nil, EndSessionParams{SessionKey: started.Key})
	if err != nil {
		t.Fatal(err)
	}
	body := resultText(t, res)
	t.Logf("end_session with a detector -> %s", body)

	var env Envelope
	decodeResult(t, res, &env)

	if sawSequence != started.Sequence+1 {
		t.Errorf("the hook saw sequence %d, want %d", sawSequence, started.Sequence+1)
	}
	if sawUncommittedEvent != 0 {
		t.Error("the hook ran after the event was inserted; it must run before")
	}
	if len(env.Conflicts) != 1 {
		t.Fatalf("the hook's conflict did not reach the response: %s", body)
	}
	if env.Conflicts[0].SuggestedAction == "" {
		t.Error("a conflict without a suggested action is noise")
	}
	// Counted after the hook: the instruction it inserted is already visible.
	if env.Pending.Instructions != 1 {
		t.Errorf("pending.instructions = %d, want 1 — the counts must be taken after detection",
			env.Pending.Instructions)
	}
	// And it committed with the event, atomically.
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `instruction`"); got != 1 {
		t.Errorf("%d instructions, want 1", got)
	}
}

// TestIntegrationAuthIsRequired: the endpoint is public, the team's board is
// not.
func TestIntegrationAuthIsRequired(t *testing.T) {
	hs := newHarness(t)
	anonymous := context.Background()

	if _, _, err := hs.h.StartSession(anonymous, nil, StartSessionParams{ProjectKey: "metiche"}); err == nil {
		t.Error("start_session ran without a token")
	}
	if _, _, err := hs.h.Heartbeat(anonymous, nil, HeartbeatParams{SessionKey: "S-1"}); err == nil {
		t.Error("heartbeat ran without a token")
	}
	if _, _, err := hs.h.GetTeamState(anonymous, nil, GetTeamStateParams{}); err == nil {
		t.Error("get_team_state ran without a token")
	}
	// health is deliberately open: "your token was rejected" is not an answer
	// to "is the server up?".
	if _, _, err := hs.h.Health(anonymous, nil, HealthParams{}); err != nil {
		t.Errorf("health should answer without a token: %v", err)
	}

	// A bad join code does not reveal whether the team exists.
	if _, _, err := hs.h.JoinTeam(anonymous, nil, JoinTeamParams{
		JoinCode: "NOPE", MemberName: "Mallory", ClientKey: "x",
	}); err == nil {
		t.Error("join_team accepted a join code that belongs to no team")
	}

	// A token that is not ours resolves to nobody.
	if _, err := hs.h.resolveToken(anonymous, "mtk_not-a-real-token"); err == nil {
		t.Error("resolveToken accepted a forged token")
	}

	// Another agent cannot touch this agent's session.
	ctxA := hs.join(t, "Ana", "client-a").ctx
	res, _, err := hs.h.StartSession(ctxA, nil, StartSessionParams{ProjectKey: "metiche"})
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	decodeResult(t, res, &env)

	ctxB := hs.join(t, "Bob", "client-b").ctx
	if _, _, err := hs.h.EndSession(ctxB, nil, EndSessionParams{SessionKey: env.Key}); err == nil {
		t.Error("one agent ended another agent's session")
	}
}

// TestIntegrationGetTeamState checks the read-only tool stays scoped, capped
// and free of anything that grows with the team.
func TestIntegrationGetTeamState(t *testing.T) {
	hs := newHarness(t)
	ctxA := hs.join(t, "Ana", "client-a").ctx
	ctxB := hs.join(t, "Bob", "client-b").ctx

	for i := 0; i < 3; i++ {
		if _, _, err := hs.h.StartSession(ctxA, nil, StartSessionParams{
			ProjectKey: "metiche", Goal: fmt.Sprintf("ana %d", i), StatusLine: "working"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := hs.h.StartSession(ctxB, nil, StartSessionParams{ProjectKey: "metiche", Goal: "bob"}); err != nil {
		t.Fatal(err)
	}

	res, _, err := hs.h.GetTeamState(ctxA, nil, GetTeamStateParams{Scope: "sessions", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	var page1 TeamStateResult
	decodeResult(t, res, &page1)
	t.Logf("get_team_state page 1 -> %s", resultText(t, res))
	if len(page1.Sessions) != 2 {
		t.Fatalf("page 1 has %d sessions, want 2", len(page1.Sessions))
	}
	if page1.NextCursor == "" {
		t.Fatal("page 1 should offer a cursor; there are 4 sessions")
	}

	res, _, err = hs.h.GetTeamState(ctxA, nil, GetTeamStateParams{Scope: "sessions", Limit: 2, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	var page2 TeamStateResult
	decodeResult(t, res, &page2)
	if len(page2.Sessions) != 2 {
		t.Fatalf("page 2 has %d sessions, want 2", len(page2.Sessions))
	}
	for _, a := range page1.Sessions {
		for _, b := range page2.Sessions {
			if a.Key == b.Key {
				t.Errorf("session %s appeared on both pages", a.Key)
			}
		}
	}

	// scope=me is only mine.
	res, _, err = hs.h.GetTeamState(ctxB, nil, GetTeamStateParams{Scope: "me"})
	if err != nil {
		t.Fatal(err)
	}
	var mine TeamStateResult
	decodeResult(t, res, &mine)
	if len(mine.Sessions) != 1 {
		t.Errorf("scope=me returned %d sessions, want 1", len(mine.Sessions))
	}
	for _, s := range mine.Sessions {
		if !s.Mine {
			t.Errorf("scope=me returned somebody else's session %s", s.Key)
		}
	}

	// No claims, no paths, ever.
	if strings.Contains(resultText(t, res), `"claims"`) || strings.Contains(resultText(t, res), `"paths"`) {
		t.Error("get_team_state must not hand back claims or paths")
	}

	// events replay in order from a cursor.
	res, _, err = hs.h.GetTeamState(ctxA, nil, GetTeamStateParams{Scope: "events", Since: 0, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var events TeamStateResult
	decodeResult(t, res, &events)
	if len(events.Events) == 0 {
		t.Fatal("scope=events returned nothing")
	}
	for i := 1; i < len(events.Events); i++ {
		if events.Events[i].Sequence <= events.Events[i-1].Sequence {
			t.Errorf("events are out of order at %d", i)
		}
	}

	// A limit over the cap is clamped rather than honoured.
	res, _, err = hs.h.GetTeamState(ctxA, nil, GetTeamStateParams{Scope: "events", Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var big TeamStateResult
	decodeResult(t, res, &big)
	if len(big.Events) > stateMaxLimit {
		t.Errorf("limit was not clamped: %d rows", len(big.Events))
	}
}

// TestIntegrationHealth is the tool an agent calls when everything else is
// failing, so it has to work with no token and it has to actually touch the
// database.
func TestIntegrationHealth(t *testing.T) {
	hs := newHarness(t)
	res, _, err := hs.h.Health(context.Background(), nil, HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	var out HealthResult
	decodeResult(t, res, &out)
	t.Logf("health -> %s", resultText(t, res))
	if !out.OK || out.Database != "reachable" {
		t.Errorf("health reported %+v against a live database", out)
	}
	if out.Authenticated {
		t.Error("health reported authenticated on an anonymous call")
	}
}

// ─────────────────────────────────────────────
// create_team
// ─────────────────────────────────────────────

// TestIntegrationCreateTeam covers the bootstrap: before this tool there was
// no way for the first person to get a join code, so join_team assumed a world
// nothing could produce.
func TestIntegrationCreateTeam(t *testing.T) {
	hs := newHarness(t)

	args := CreateTeamParams{
		TeamName:       "Hack Night",
		MemberName:     "Ana",
		AgentLabel:     "backend",
		ClientKey:      "client-a",
		IdempotencyKey: uuid.Must(uuid.NewV4()).String(),
	}

	res, _, err := hs.h.CreateTeam(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("create_team: %v", err)
	}
	var first CreateTeamResult
	decodeResult(t, res, &first)
	t.Logf("create_team -> %s", redactSecrets(resultText(t, res)))

	if !first.Created {
		t.Error("the first create_team should report created=true")
	}
	if first.JoinCode == "" || len(first.JoinCode) != joinCodeLength {
		t.Errorf("join_code = %q, want %d characters", first.JoinCode, joinCodeLength)
	}
	if first.Token == "" || !strings.HasPrefix(first.Token, tokenPrefix) {
		t.Error("create_team should join the creator and return their token")
	}
	if first.TeamSlug == "" {
		t.Error("create_team should return a slug for the board URL")
	}
	if first.AgentKey == "" || first.MemberKey != "ana" {
		t.Errorf("creator not joined properly: member=%q agent=%q", first.MemberKey, first.AgentKey)
	}

	// The team got the instance default plan, and it got it explicitly. A
	// team with no plan is a team with no limits, which is the thing
	// ensureTeam refuses to create.
	var planUUID sql.NullString
	var planSource int64
	var visibility int64
	if err := hs.core.DB().QueryRow(
		"SELECT `plan_uuid`, `plan_source`, `visibility` FROM `team` WHERE `slug` = ?", first.TeamSlug).
		Scan(&planUUID, &planSource, &visibility); err != nil {
		t.Fatal(err)
	}
	if !planUUID.Valid || planUUID.String != hs.planID.String() {
		t.Errorf("team.plan_uuid = %v, want the instance default %s", planUUID, hs.planID)
	}
	if planSource != int64(enums.PLAN_SOURCE_INSTANCE_DEFAULT) {
		t.Errorf("team.plan_source = %d, want instance_default (%d)", planSource, enums.PLAN_SOURCE_INSTANCE_DEFAULT)
	}
	if visibility != int64(enums.TEAM_VISIBILITY_PRIVATE) {
		t.Errorf("team.visibility = %d, want private (%d)", visibility, enums.TEAM_VISIBILITY_PRIVATE)
	}
	if first.Plan == "" {
		t.Error("create_team should say which plan the team got")
	}

	// The join code is an INVITE row now, owned by the creator's membership.
	var inviteTeam, createdBy sql.NullString
	var inviteStatus, inviteUses int64
	if err := hs.core.DB().QueryRow(
		"SELECT `team_uuid`, `created_by_member_uuid`, `status`, `uses` FROM `invite` WHERE `code` = ?",
		first.JoinCode).Scan(&inviteTeam, &createdBy, &inviteStatus, &inviteUses); err != nil {
		t.Fatalf("create_team did not mint an invite: %v", err)
	}
	if inviteStatus != int64(enums.INVITE_STATUS_ACTIVE) {
		t.Errorf("the first invite is status %d, want active", inviteStatus)
	}
	if !createdBy.Valid {
		t.Error("the first invite has no created_by_member_uuid")
	}

	// The creator's token works immediately: no second call needed.
	creator := hs.ctxForToken(t, first.Token)
	if _, _, err := hs.h.StartSession(creator, nil,
		StartSessionParams{ProjectKey: "metiche", Goal: "work", TeamSlug: first.TeamSlug}); err != nil {
		t.Fatalf("start_session with the creator's token: %v", err)
	}

	// A retry does not create a second team. A real retry carries the token
	// the first attempt returned — without it the caller is, by definition, a
	// different anonymous person.
	res, _, err = hs.h.CreateTeam(creator, nil, args)
	if err != nil {
		t.Fatalf("retried create_team: %v", err)
	}
	var retry CreateTeamResult
	decodeResult(t, res, &retry)
	if retry.Created {
		t.Error("the retry reported created=true")
	}
	if retry.JoinCode != first.JoinCode {
		t.Error("the retry returned a different join code, so a second door key was quietly issued")
	}
	if retry.TeamSlug != first.TeamSlug {
		t.Error("the retry landed on a different team")
	}
	if retry.Token != "" {
		t.Error("the retry minted a second token for a caller who already had one")
	}
	// newHarness seeds one team of its own, so the count is 2, not 1.
	if got := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team`"); got != 2 {
		t.Errorf("%d teams exist, want 2 (the harness's plus this one) — the retry created another", got)
	}

	// A different idempotency key IS a different team. Retry safety is not
	// the same promise as "one team ever".
	args2 := args
	args2.IdempotencyKey = uuid.Must(uuid.NewV4()).String()
	res, _, err = hs.h.CreateTeam(creator, nil, args2)
	if err != nil {
		t.Fatal(err)
	}
	var second CreateTeamResult
	decodeResult(t, res, &second)
	if !second.Created || second.TeamSlug == first.TeamSlug {
		t.Error("a fresh idempotency_key should create a distinct team")
	}
	// Two teams called "Hack Night": the slug has to disambiguate.
	if second.TeamSlug == "" || second.TeamSlug == first.TeamSlug {
		t.Errorf("slug collision: %q and %q", first.TeamSlug, second.TeamSlug)
	}

	// And a teammate can actually join with the code that came back.
	res, _, err = hs.h.JoinTeam(context.Background(), nil, JoinTeamParams{
		JoinCode: first.JoinCode, MemberName: "Bob", AgentLabel: "ui", ClientKey: "client-b",
	})
	if err != nil {
		t.Fatalf("join_team with the code create_team returned: %v", err)
	}
	var bob JoinTeamResult
	decodeResult(t, res, &bob)
	if bob.Token == "" {
		t.Error("the teammate got no token")
	}

	// The join code is a secret too: it must not reach the event log.
	assertNoSecretsInEventLog(t, hs, first.JoinCode, first.Token, bob.Token)
}

// TestIntegrationCreateTeamWithNoDefaultPlan is the guard the v3 plan model
// is worth nothing without.
//
// A team whose plan_uuid is NULL has no ceiling on agents, members, projects
// or retention. On a hosted instance that is an unbounded tenant, and the
// first anyone would hear of it is the bill or the outage — so creation fails
// loudly here rather than defaulting to "no limits" silently.
func TestIntegrationCreateTeamWithNoDefaultPlan(t *testing.T) {
	hs := newHarness(t)
	if _, err := hs.core.DB().Exec("UPDATE `plan` SET `is_instance_default` = 0"); err != nil {
		t.Fatal(err)
	}

	_, _, err := hs.h.CreateTeam(context.Background(), nil, CreateTeamParams{
		TeamName: "Unbounded", MemberName: "Ana", AgentLabel: "backend",
		ClientKey: "client-a", IdempotencyKey: uuid.Must(uuid.NewV4()).String(),
	})
	if err == nil {
		t.Fatal("create_team made a team with no plan")
	}
	if !errors.Is(err, ErrNoDefaultPlan) {
		t.Errorf("the refusal should be ErrNoDefaultPlan, got: %v", err)
	}
	// And it failed BEFORE writing anything: no half-made team, no account.
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team`"); n != 1 {
		t.Errorf("%d teams, want only the harness's — a team was half-created", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `account`"); n != 0 {
		t.Errorf("%d accounts, want 0 — an identity was minted for a creation that failed", n)
	}
}

func TestIntegrationCreateTeamValidation(t *testing.T) {
	hs := newHarness(t)
	base := CreateTeamParams{
		TeamName: "Hack Night", MemberName: "Ana", AgentLabel: "backend",
		ClientKey: "client-a", IdempotencyKey: uuid.Must(uuid.NewV4()).String(),
	}
	cases := []struct {
		name  string
		muton func(*CreateTeamParams)
	}{
		{"no team name", func(p *CreateTeamParams) { p.TeamName = "" }},
		{"unsluggable team name", func(p *CreateTeamParams) { p.TeamName = "!!!" }},
		{"no member name", func(p *CreateTeamParams) { p.MemberName = "" }},
		{"no client key", func(p *CreateTeamParams) { p.ClientKey = "" }},
		{"no idempotency key", func(p *CreateTeamParams) { p.IdempotencyKey = "" }},
		{"short idempotency key", func(p *CreateTeamParams) { p.IdempotencyKey = "abc" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := base
			tc.muton(&args)
			if _, _, err := hs.h.CreateTeam(context.Background(), nil, args); err == nil {
				t.Error("accepted an invalid request")
			}
		})
	}
}

// TestIntegrationCreateTeamRateLimit: creation is open, so the budget is the
// only thing bounding it.
func TestIntegrationCreateTeamRateLimit(t *testing.T) {
	t.Setenv("METICHE_CREATE_TEAM_PER_HOUR", "2")
	t.Setenv("METICHE_JOIN_PER_HOUR", "3")
	hs := newHarness(t)

	ctx := WithClientIP(context.Background(), "203.0.113.9")
	for i := 0; i < 2; i++ {
		if _, _, err := hs.h.CreateTeam(ctx, nil, CreateTeamParams{
			TeamName: fmt.Sprintf("Team %d", i), MemberName: "Ana", AgentLabel: "a",
			ClientKey: "c", IdempotencyKey: uuid.Must(uuid.NewV4()).String(),
		}); err != nil {
			t.Fatalf("create %d inside the budget failed: %v", i, err)
		}
	}
	_, _, err := hs.h.CreateTeam(ctx, nil, CreateTeamParams{
		TeamName: "Team 3", MemberName: "Ana", AgentLabel: "a",
		ClientKey: "c", IdempotencyKey: uuid.Must(uuid.NewV4()).String(),
	})
	if err == nil {
		t.Fatal("the third create_team was allowed past a budget of 2")
	}
	if !strings.Contains(err.Error(), "try again in") {
		t.Errorf("the rejection should say when to retry: %v", err)
	}

	// Another address has its own budget.
	if _, _, err := hs.h.CreateTeam(WithClientIP(context.Background(), "198.51.100.4"), nil, CreateTeamParams{
		TeamName: "Other", MemberName: "Bob", AgentLabel: "a",
		ClientKey: "c", IdempotencyKey: uuid.Must(uuid.NewV4()).String(),
	}); err != nil {
		t.Errorf("a different address was rate limited: %v", err)
	}

	// join_team is limited too: it is the only place a join code can be
	// guessed at.
	for i := 0; i < 3; i++ {
		_, _, _ = hs.h.JoinTeam(ctx, nil, JoinTeamParams{
			JoinCode: "WRONGCODE1", MemberName: "Mallory", ClientKey: "m",
		})
	}
	_, _, err = hs.h.JoinTeam(ctx, nil, JoinTeamParams{
		JoinCode: hs.code, MemberName: "Mallory", ClientKey: "m",
	})
	if err == nil || !strings.Contains(err.Error(), "too many join attempts") {
		t.Errorf("join_team was not rate limited: %v", err)
	}
}

// TestIntegrationAuthMiddleware drives the real HTTP edge, because the
// middleware is where "resolve, do not reject" has to be exactly right: reject
// too eagerly and nobody can ever call create_team or join_team; reject too
// late and a wrong token looks like no token.
func TestIntegrationAuthMiddleware(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	var seen account_entity.Account
	var sawAgent bool
	var sawIP string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, sawAgent = AgentFromContext(r.Context())
		sawIP = ClientIPFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	mw := hs.h.authMiddleware(next)

	// No token: passes through anonymously so create_team and join_team work.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/mcp", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("anonymous request got %d, want 200 — join_team would be unreachable", rec.Code)
	}
	if sawAgent {
		t.Error("an anonymous request resolved to an account")
	}
	if sawIP != "203.0.113.9" {
		t.Errorf("client ip = %q, want 203.0.113.9", sawIP)
	}

	// A bad token is rejected at the edge, not passed through as anonymous.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/mcp", nil)
	req.Header.Set("Authorization", "Bearer mtk_definitely-not-real")
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a forged token got %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("a 401 should say what scheme it wants")
	}
	if strings.Contains(rec.Body.String(), "mtk_definitely-not-real") {
		t.Error("the rejection echoed the presented token back")
	}

	// A good token resolves to the right agent.
	token, hash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.core.DB().Exec("UPDATE `account` SET `token_hash` = ? WHERE `id` = ?", hash, ana.account.ID.String()); err != nil {
		t.Fatal(err)
	}
	sawAgent = false
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid token got %d", rec.Code)
	}
	if !sawAgent || seen.ID != ana.account.ID {
		t.Errorf("resolved to %v, want account %s", seen.ID, ana.account.ID)
	}
}

// assertNoSecretsInEventLog is the one assertion worth repeating: neither a
// bearer token nor a join code may ever reach an append-only table.
func assertNoSecretsInEventLog(t *testing.T, hs *harness, secrets ...string) {
	t.Helper()
	rows, err := hs.core.DB().Query(
		"SELECT COALESCE(CAST(`response_snapshot` AS CHAR), ''), COALESCE(`summary`, ''), COALESCE(CAST(`payload` AS CHAR), ''), COALESCE(`subject_key`, '') FROM `team_event`")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var snap, summary, payload, subject string
		if err := rows.Scan(&snap, &summary, &payload, &subject); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{snap, summary, payload, subject} {
			for _, secret := range secrets {
				if secret != "" && strings.Contains(field, secret) {
					t.Errorf("a secret reached the event log: %s", field)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// redactSecrets keeps a test log from being the thing that leaks a credential
// into CI output.
func redactSecrets(s string) string {
	out := secretPattern.ReplaceAllString(s, `"$1":"<redacted>"`)
	return out
}

var secretPattern = regexp.MustCompile(`"(token|join_code|auth_header)":"[^"]*"`)
