package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/config"
	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/mklfarha/metiche/backend/app/browser"
	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/core"
	"github.com/mklfarha/metiche/backend/enums"
	restserver "github.com/mklfarha/metiche/backend/rest/server"
)

// The board's invite routes (docs/BOARD_LOGIN.md §10.10), through the REAL
// wiring: restserver.New with ProvideCustomRoutes, so the REST routes, the
// deny layer, the chi request logger and the MCP endpoint are the ones
// production mounts, sharing one mcp.Handler. People join with the real
// join_team and get real agent tokens; browser sessions are seeded the way
// app/webapi's tests seed them. Every name is a test name and every secret is
// minted at run time.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&interpolateParams=true' go test -p 1 ./app/
//
// Rows are seeded under fresh ids and deleted at the end, so packages that
// run later against the same database see nothing of them.

type inviteWorld struct {
	t    *testing.T
	db   *sql.DB
	core *core.Implementation
	srv  *httptest.Server
	logs *observer.ObservedLogs
	reqs *lockedBuffer // the chi request logger's output

	plan  string
	teams []string
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func newID(t *testing.T) string {
	t.Helper()
	h := randHex(t, 16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

// dsnProvider turns a go-sql-driver DSN into the config core.New reads.
func dsnProvider(t *testing.T, dsn string) config.Provider {
	t.Helper()
	at := strings.LastIndex(dsn, "@tcp(")
	closeAt := strings.Index(dsn[at+5:], ")")
	if at < 0 || closeAt < 0 {
		t.Fatalf("METICHE_TEST_MYSQL_DSN does not look like user:pass@tcp(host:port)/db?params")
	}
	user, pass, _ := strings.Cut(dsn[:at], ":")
	host, port, _ := strings.Cut(dsn[at+5:at+5+closeAt], ":")
	dbName, params, _ := strings.Cut(strings.TrimPrefix(dsn[at+5+closeAt+1:], "/"), "?")
	p, err := config.NewStaticProvider(map[string]any{
		"ports": map[string]any{"http": "0"},
		"db": []map[string]any{{
			"name": dbName, "host": host, "port": port, "user": user, "pswd": pass,
			"params": params, "driver": "mysql",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newInviteWorld(t *testing.T) *inviteWorld {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("METICHE_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("skipped: METICHE_TEST_MYSQL_DSN is not set, so there is no MySQL to run the invite routes against")
	}
	t.Setenv("METICHE_ROLE", "all")
	t.Setenv("METICHE_JOIN_PER_HOUR", "1000")

	obs, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(obs)
	provider := dsnProvider(t, dsn)
	impl, err := core.New(core.Params{Provider: provider, Logger: logger})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := impl.DB().PingContext(ctx); err != nil {
		t.Skipf("skipped: METICHE_TEST_MYSQL_DSN is set but the database is unreachable: %v", err)
	}

	// The generated server's request logger writes to its own stdout logger;
	// capture it, so the canary test can read every request line.
	reqs := &lockedBuffer{}
	prevLogger := middleware.DefaultLogger
	middleware.DefaultLogger = middleware.RequestLogger(&middleware.DefaultLogFormatter{
		Logger: log.New(reqs, "", log.LstdFlags), NoColor: true})
	t.Cleanup(func() { middleware.DefaultLogger = prevLogger })

	srv := restserver.New(restserver.Params{
		Lifecycle:    fxtest.NewLifecycle(t),
		Config:       provider,
		Core:         impl,
		Logger:       logger,
		CustomRoutes: ProvideCustomRoutes(impl, logger),
	})
	w := &inviteWorld{t: t, db: impl.DB(), core: impl, logs: logs, reqs: reqs}
	w.srv = httptest.NewServer(srv.Handler)
	t.Cleanup(w.srv.Close)

	w.plan = newID(t)
	w.exec("INSERT INTO `plan` (`id`,`key`,`name`,`max_concurrent_agents`,`retention_days`,`max_members`,"+
		"`max_projects`,`is_instance_default`,`sort_order`,`status`) VALUES (?,?,?,?,?,?,?,?,?,?)",
		w.plan, "invrest-"+randHex(t, 4), "Test plan", 20, 30, 50, 5, false, 0, enums.RECORD_STATUS_ACTIVE)
	t.Cleanup(w.cleanup)
	return w
}

func (w *inviteWorld) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.db.Exec(q, args...); err != nil {
		w.t.Fatalf("exec %s: %v", q, err)
	}
}

func (w *inviteWorld) count(q string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(q, args...).Scan(&n); err != nil {
		w.t.Fatalf("count %s: %v", q, err)
	}
	return n
}

// cleanup deletes every row this world made: its teams, everything that
// names them, the accounts that joined them and everything that names those.
func (w *inviteWorld) cleanup() {
	ctx := context.Background()
	conn, err := w.db.Conn(ctx)
	if err != nil {
		w.t.Logf("cleanup: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	in := func(ids []string) (string, []any) {
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
	}
	columnTables := func(col string) []string {
		rows, err := conn.QueryContext(ctx,
			"SELECT TABLE_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND COLUMN_NAME = ?", col)
		if err != nil {
			return nil
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				out = append(out, s)
			}
		}
		return out
	}
	_, _ = conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0")
	defer func() { _, _ = conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1") }()
	if len(w.teams) == 0 {
		_, _ = conn.ExecContext(ctx, "DELETE FROM `plan` WHERE `id` = ?", w.plan)
		return
	}
	marks, teamArgs := in(w.teams)
	// contract_field has no team_uuid; it goes with its team's contracts.
	_, _ = conn.ExecContext(ctx, "DELETE FROM `contract_field` WHERE `contract_uuid` IN (SELECT `id` FROM `contract` WHERE `team_uuid` IN ("+marks+"))", teamArgs...)
	var accounts []string
	if rows, err := conn.QueryContext(ctx, "SELECT DISTINCT `account_uuid` FROM `member` WHERE `team_uuid` IN ("+marks+")", teamArgs...); err == nil {
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				accounts = append(accounts, s)
			}
		}
		_ = rows.Close()
	}
	for _, tbl := range columnTables("team_uuid") {
		_, _ = conn.ExecContext(ctx, "DELETE FROM `"+tbl+"` WHERE `team_uuid` IN ("+marks+")", teamArgs...)
	}
	if len(accounts) > 0 {
		am, aargs := in(accounts)
		for _, tbl := range columnTables("account_uuid") {
			_, _ = conn.ExecContext(ctx, "DELETE FROM `"+tbl+"` WHERE `account_uuid` IN ("+am+")", aargs...)
		}
		_, _ = conn.ExecContext(ctx, "DELETE FROM `account` WHERE `id` IN ("+am+")", aargs...)
	}
	_, _ = conn.ExecContext(ctx, "DELETE FROM `team` WHERE `id` IN ("+marks+")", teamArgs...)
	_, _ = conn.ExecContext(ctx, "DELETE FROM `plan` WHERE `id` = ?", w.plan)
}

type testTeam struct{ id, slug, code string }

// team seeds a team with one uncapped, unexpiring invite to join it by.
func (w *inviteWorld) team(visibility enums.TeamVisibility) testTeam {
	w.t.Helper()
	tm := testTeam{id: newID(w.t)}
	tm.slug = "invrest-" + tm.id[:8]
	tm.code = "JOIN" + strings.ToUpper(randHex(w.t, 3))
	w.teams = append(w.teams, tm.id)
	w.exec("INSERT INTO `team` (`id`,`name`,`slug`,`sequence`,`board_revision`,`status`,`visibility`,`plan_uuid`,`plan_source`) VALUES (?,?,?,?,?,?,?,?,?)",
		tm.id, "Test team", tm.slug, 0, 0, enums.RECORD_STATUS_ACTIVE, visibility, w.plan, enums.PLAN_SOURCE_INSTANCE_DEFAULT)
	w.exec("INSERT INTO `invite` (`id`,`team_uuid`,`code`,`label`,`uses`,`status`) VALUES (?,?,?,?,?,?)",
		newID(w.t), tm.id, tm.code, "harness", 0, enums.INVITE_STATUS_ACTIVE)
	return tm
}

// person is somebody who joined a team with the real join_team: an agent
// token for MCP, and a browser session for the board.
type person struct {
	name, token, session string
	account, member      string
}

func (w *inviteWorld) join(tm testTeam, name string) person {
	w.t.Helper()
	anon := w.connect("")
	isErr, text := w.tool(anon, "join_team", map[string]any{
		"join_code": tm.code, "member_name": name, "agent_label": "test",
		"client_key": strings.ToLower(strings.ReplaceAll(name, " ", "-")) + "-" + randHex(w.t, 3),
	})
	if isErr {
		w.t.Fatalf("%s could not join: %.200s", name, text)
	}
	var out metichemcp.JoinTeamResult
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Token == "" {
		w.t.Fatalf("join_team for %s returned no token (%v)", name, err)
	}
	p := person{name: name, token: out.Token}
	if err := w.db.QueryRow("SELECT `id`, `account_uuid` FROM `member` WHERE `team_uuid` = ? AND `display_name` = ?",
		tm.id, name).Scan(&p.member, &p.account); err != nil {
		w.t.Fatalf("member row for %s: %v", name, err)
	}
	var agent string
	if err := w.db.QueryRow("SELECT `id` FROM `agent` WHERE `account_uuid` = ? LIMIT 1", p.account).Scan(&agent); err != nil {
		w.t.Fatalf("agent row for %s: %v", name, err)
	}
	secret, hash, err := browser.MintSessionSecret()
	if err != nil {
		w.t.Fatal(err)
	}
	w.exec("INSERT INTO `browser_session` (`id`,`key`,`account_uuid`,`secret_hash`,`auth_method`,`created_from_agent_uuid`,`expires_at`,`last_seen_at`) "+
		"VALUES (?,?,?,?,?,?,DATE_ADD(UTC_TIMESTAMP(), INTERVAL 30 DAY),UTC_TIMESTAMP())",
		newID(w.t), "BS-"+strings.ToUpper(hash[:10]), p.account, hash, enums.BROWSER_AUTH_METHOD_TERMINAL_LINK, agent)
	p.session = secret
	return p
}

func (w *inviteWorld) promote(p person) {
	w.t.Helper()
	w.exec("UPDATE `member` SET `role` = ? WHERE `id` = ?", enums.MEMBER_ROLE_OWNER, p.member)
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (w *inviteWorld) connect(token string) *mcp.ClientSession {
	w.t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "invite-rest-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             w.srv.URL + "/v1/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token}, Timeout: 15 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		w.t.Fatalf("connecting to /v1/mcp: %v", err)
	}
	w.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func (w *inviteWorld) tool(cs *mcp.ClientSession, name string, args map[string]any) (bool, string) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		w.t.Fatalf("%s: %v", name, err)
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	return res.IsError, text
}

type restResp struct {
	status int
	body   string
	header http.Header
}

// problem is the detail of a problem+json body.
func (r restResp) problem() (title, detail string) {
	var p struct{ Title, Detail string }
	_ = json.Unmarshal([]byte(r.body), &p)
	return p.Title, p.Detail
}

func (w *inviteWorld) call(method, path, body string, headers map[string]string) restResp {
	w.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rdr)
	if err != nil {
		w.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := w.srv.Client().Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return restResp{status: resp.StatusCode, body: string(b), header: resp.Header}
}

func sessionHeader(p person) map[string]string {
	return map[string]string{browser.HeaderSession: p.session}
}

func invitesPath(slug string) string { return "/v1/teams/" + slug + "/invites" }

func (w *inviteWorld) create(p person, slug, body string) (metichemcp.CreateInviteResult, restResp) {
	w.t.Helper()
	r := w.call(http.MethodPost, invitesPath(slug), body, sessionHeader(p))
	var out metichemcp.CreateInviteResult
	if r.status == http.StatusCreated {
		if err := json.Unmarshal([]byte(r.body), &out); err != nil {
			w.t.Fatalf("create answer: %v", err)
		}
	}
	return out, r
}

func (w *inviteWorld) mustCreate(p person, slug, body string) metichemcp.CreateInviteResult {
	w.t.Helper()
	out, r := w.create(p, slug, body)
	if r.status != http.StatusCreated {
		title, detail := r.problem()
		w.t.Fatalf("%s POST %s %s = %d %s: %s", p.name, invitesPath(slug), body, r.status, title, detail)
	}
	return out
}

func (w *inviteWorld) list(p person, slug string) (metichemcp.ListInvitesResult, restResp) {
	w.t.Helper()
	r := w.call(http.MethodGet, invitesPath(slug), "", sessionHeader(p))
	if r.status != http.StatusOK {
		w.t.Fatalf("%s GET %s = %d %s", p.name, invitesPath(slug), r.status, r.body)
	}
	var out metichemcp.ListInvitesResult
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		w.t.Fatalf("list answer: %v", err)
	}
	return out, r
}

func (w *inviteWorld) inviteCount(tm testTeam) int {
	return w.count("SELECT COUNT(*) FROM `invite` WHERE `team_uuid` = ?", tm.id)
}

func (w *inviteWorld) revokedAt(inviteID string) sql.NullTime {
	w.t.Helper()
	var at sql.NullTime
	if err := w.db.QueryRow("SELECT `revoked_at` FROM `invite` WHERE `id` = ?", inviteID).Scan(&at); err != nil {
		w.t.Fatalf("invite %s: %v", inviteID, err)
	}
	return at
}

func assertNoStore(t *testing.T, what string, r restResp) {
	t.Helper()
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control %q, want no-store", what, got)
	}
}

// ─────────────────────────────────────────────
// tests
// ─────────────────────────────────────────────

// An owner lists and revokes every invite of the team; a member sees and
// revokes only their own, and every other invite is one not_found — the same
// text the MCP tool gives for the same id.
func TestInviteRESTOwnerAndMemberScope(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	w.promote(ana)
	bob := w.join(tm, "Bob")
	tess := w.join(tm, "Test Member")
	other := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	otherInvite := newID(t)
	w.exec("INSERT INTO `invite` (`id`,`team_uuid`,`code`,`label`,`uses`,`status`) VALUES (?,?,?,?,?,?)",
		otherInvite, other.id, "FAKEOTHER"+strings.ToUpper(randHex(t, 2)), "other team", 0, enums.INVITE_STATUS_ACTIVE)

	before := time.Now().UTC()
	anaInv, r := w.create(ana, tm.slug, `{"label":"for a teammate"}`)
	if r.status != http.StatusCreated {
		t.Fatalf("ana's create: %d %s", r.status, r.body)
	}
	assertNoStore(t, "create", r)
	if !anaInv.Created || anaInv.State != "active" || anaInv.MaxUses != 1 || anaInv.Label != "for a teammate" ||
		len(anaInv.Code) != 10 || !strings.Contains(anaInv.ShareNote, "curl -fsSL https://metiche.xyz/install.sh | sh") {
		t.Fatalf("ana's invite: created=%v state=%q max=%d label=%q code length=%d", anaInv.Created, anaInv.State, anaInv.MaxUses, anaInv.Label, len(anaInv.Code))
	}
	if d := anaInv.ExpiresAt.Sub(before.Add(7 * 24 * time.Hour)); d < -2*time.Second || d > time.Minute {
		t.Errorf("default expiry %s, want about 7 days after %s", anaInv.ExpiresAt, before)
	}
	bobInv := w.mustCreate(bob, tm.slug, `{"label":"bob's","max_uses":3}`)
	tessInv := w.mustCreate(tess, tm.slug, `{"label":"tess's"}`)

	bobList, bobRaw := w.list(bob, tm.slug)
	assertNoStore(t, "list", bobRaw)
	if bobList.Scope != "created_by_you" || len(bobList.Invites) != 1 || bobList.Invites[0].InviteID != bobInv.InviteID ||
		!bobList.Invites[0].CreatedByYou || bobList.Invites[0].CreatedBy != "Bob" {
		t.Fatalf("bob's list: %s", bobRaw.body)
	}
	if strings.Contains(bobRaw.body, anaInv.InviteID) || strings.Contains(bobRaw.body, tessInv.InviteID) {
		t.Fatal("bob's list shows somebody else's invite")
	}
	anaList, _ := w.list(ana, tm.slug)
	if anaList.Scope != "team" || len(anaList.Invites) != 4 { // three here and the harness invite
		t.Fatalf("ana's list: scope=%q %d invites, want team and 4", anaList.Scope, len(anaList.Invites))
	}

	// The REST list is list_invites' answer, from the same function.
	isErr, mcpText := w.tool(w.connect(bob.token), "list_invites", map[string]any{"team_slug": tm.slug})
	var mcpList metichemcp.ListInvitesResult
	if isErr || json.Unmarshal([]byte(mcpText), &mcpList) != nil || mcpList.Scope != bobList.Scope ||
		len(mcpList.Invites) != 1 || mcpList.Invites[0].InviteID != bobInv.InviteID || mcpList.Note != bobList.Note {
		t.Errorf("list_invites and GET disagree: %s vs %s", mcpText, bobRaw.body)
	}

	revoke := func(p person, id string) restResp {
		return w.call(http.MethodDelete, invitesPath(tm.slug)+"/"+id, "", sessionHeader(p))
	}
	unknown := newID(t)
	want := revoke(bob, unknown)
	title, detail := want.problem()
	if want.status != http.StatusNotFound || title != "not_found" || !strings.HasPrefix(detail, "not_found: no invite with that invite_id on team "+tm.slug) {
		t.Fatalf("bob revoking an unknown id: %d %s", want.status, want.body)
	}
	assertNoStore(t, "revoke refusal", want)
	for name, id := range map[string]string{
		"ana's invite": anaInv.InviteID, "tess's invite": tessInv.InviteID,
		"another team's": otherInvite, "a malformed id": "not-a-uuid",
	} {
		if got := revoke(bob, id); got.status != want.status || got.body != want.body {
			t.Errorf("bob revoking %s: %d %q, want the unknown id's %d %q", name, got.status, got.body, want.status, want.body)
		}
	}
	for _, id := range []string{anaInv.InviteID, tessInv.InviteID, otherInvite} {
		if w.revokedAt(id).Valid {
			t.Errorf("invite %s was revoked by a refused call", id)
		}
	}
	// The MCP tool refuses the same id with the same words.
	if isErr, text := w.tool(w.connect(bob.token), "revoke_invite", map[string]any{"team_slug": tm.slug, "invite_id": unknown}); !isErr || text != detail {
		t.Errorf("revoke_invite's not_found %q differs from the REST detail %q", text, detail)
	}

	var out metichemcp.RevokeInviteResult
	if r := revoke(bob, bobInv.InviteID); r.status != http.StatusOK || json.Unmarshal([]byte(r.body), &out) != nil || out.State != "revoked" || out.AlreadyRevoked {
		t.Errorf("bob revoking his own: %d %s", r.status, r.body)
	}
	if r := revoke(ana, tessInv.InviteID); r.status != http.StatusOK || json.Unmarshal([]byte(r.body), &out) != nil || out.State != "revoked" {
		t.Errorf("the owner revoking a member's: %d %s", r.status, r.body)
	}
	if !w.revokedAt(tessInv.InviteID).Valid || !w.revokedAt(bobInv.InviteID).Valid {
		t.Error("an accepted revoke did not revoke")
	}
	anaUnknown := revoke(ana, unknown)
	if got := revoke(ana, otherInvite); got.status != http.StatusNotFound || got.body != anaUnknown.body {
		t.Errorf("the owner revoking another team's invite: %d %q, want %q", got.status, got.body, anaUnknown.body)
	}
}

// The caps are create_invite's, enforced by the same function, with the same
// words, and a refused create writes nothing.
func TestInviteRESTCapsEnforced(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	w.promote(ana)
	bob := w.join(tm, "Bob")

	before := w.inviteCount(tm)
	for _, c := range []struct {
		who          person
		body         string
		status       int
		detailPrefix string
	}{
		{bob, `{"max_uses":26}`, http.StatusForbidden, "not_permitted: members may create invites with at most 25 uses and 168 hours (7 days)"},
		{bob, `{"expires_in_hours":169}`, http.StatusForbidden, "not_permitted: members may create invites with at most 25 uses and 168 hours (7 days)"},
		{bob, `{"max_uses":100,"expires_in_hours":720}`, http.StatusForbidden, "not_permitted:"},
		{ana, `{"max_uses":101}`, http.StatusBadRequest, "invalid_argument: max_uses may be at most 100"},
		{ana, `{"expires_in_hours":721}`, http.StatusBadRequest, "invalid_argument: expires_in_hours may be at most 720 (30 days)"},
		{ana, `{"max_uses":-1}`, http.StatusBadRequest, "invalid_argument: max_uses and expires_in_hours must be positive"},
		{ana, `{"max_uses":"lots"}`, http.StatusBadRequest, "invalid_argument:"},
		{ana, `not json`, http.StatusBadRequest, "invalid_argument:"},
	} {
		_, r := w.create(c.who, tm.slug, c.body)
		_, detail := r.problem()
		if r.status != c.status || !strings.HasPrefix(detail, c.detailPrefix) {
			t.Errorf("%s POST %s = %d %q, want %d %q…", c.who.name, c.body, r.status, detail, c.status, c.detailPrefix)
		}
		assertNoStore(t, "a refused create", r)
	}
	if after := w.inviteCount(tm); after != before {
		t.Fatalf("refused creates wrote %d invites", after-before)
	}

	// The MCP tool says exactly what the REST detail says.
	_, r := w.create(bob, tm.slug, `{"max_uses":26}`)
	_, detail := r.problem()
	if isErr, text := w.tool(w.connect(bob.token), "create_invite", map[string]any{"team_slug": tm.slug, "max_uses": 26}); !isErr || text != detail {
		t.Errorf("create_invite %q, REST %q", text, detail)
	}

	if inv := w.mustCreate(bob, tm.slug, `{"max_uses":25,"expires_in_hours":168}`); inv.MaxUses != 25 {
		t.Errorf("a member at the cap: max_uses=%d", inv.MaxUses)
	}
	if inv := w.mustCreate(ana, tm.slug, `{"max_uses":100,"expires_in_hours":720}`); inv.MaxUses != 100 || inv.ExpiresAt.Sub(time.Now().UTC()) < 719*time.Hour {
		t.Errorf("the owner at the ceiling: max_uses=%d expires_at=%s", inv.MaxUses, inv.ExpiresAt)
	}
}

// A bearer token, no credential, a session that does not validate, a
// non-member, a revoked member and a public team's non-member all get the
// bytes an unknown team gets, on all three routes, and change nothing.
func TestInviteRESTRefusalsAreAnUnknownTeam(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	w.promote(ana)
	removed := w.join(tm, "Removed Member")
	w.exec("UPDATE `member` SET `revoked_at` = UTC_TIMESTAMP() WHERE `id` = ?", removed.member)
	pub := w.team(enums.TEAM_VISIBILITY_PUBLIC)
	elsewhere := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	outsider := w.join(elsewhere, "Bob")
	anaInv := w.mustCreate(ana, tm.slug, `{"label":"must survive"}`)
	before := w.inviteCount(tm) + w.inviteCount(pub)

	const missing = "no-such-team-q7z"
	routes := []struct{ method, sub, body string }{
		{http.MethodGet, "", ""},
		{http.MethodPost, "", `{"label":"nope"}`},
		{http.MethodDelete, "/" + anaInv.InviteID, ""},
	}
	// The package's unknown-slug answer on the board's snapshot route.
	snapshot := w.call(http.MethodGet, "/v1/teams/"+missing, "", nil)

	for _, rt := range routes {
		base := w.call(rt.method, invitesPath(missing)+rt.sub, rt.body, sessionHeader(ana))
		if base.status != http.StatusNotFound || base.body != snapshot.body ||
			base.header.Get("Content-Type") != snapshot.header.Get("Content-Type") {
			t.Fatalf("%s unknown team: %d %q (%s), want the snapshot route's 404 %q (%s)", rt.method, base.status, base.body,
				base.header.Get("Content-Type"), snapshot.body, snapshot.header.Get("Content-Type"))
		}
		assertNoStore(t, rt.method+" unknown team", base)
		cases := []struct {
			name, slug string
			headers    map[string]string
		}{
			{"no credential", tm.slug, nil},
			{"a garbage session", tm.slug, map[string]string{browser.HeaderSession: "mbs_not-a-real-session-0000000000000000"}},
			{"a member of another team", tm.slug, sessionHeader(outsider)},
			{"a revoked member", tm.slug, sessionHeader(removed)},
			{"the owner's bearer token", tm.slug, map[string]string{"Authorization": "Bearer " + ana.token}},
			{"the owner's X-Metiche-Token", tm.slug, map[string]string{"X-Metiche-Token": ana.token}},
			{"the owner's session AND bearer", tm.slug, map[string]string{browser.HeaderSession: ana.session, "Authorization": "Bearer " + ana.token}},
			{"a public team's non-member", pub.slug, sessionHeader(outsider)},
			{"a public team, no credential", pub.slug, nil},
		}
		for _, c := range cases {
			got := w.call(rt.method, invitesPath(c.slug)+rt.sub, rt.body, c.headers)
			for _, h := range []string{"Content-Type", "Cache-Control", "Retry-After"} {
				if got.header.Get(h) != base.header.Get(h) {
					t.Errorf("%s %s: header %s %q, unknown team %q", rt.method, c.name, h, got.header.Get(h), base.header.Get(h))
				}
			}
			if got.status != base.status || got.body != base.body {
				t.Errorf("%s %s: %d %q, want the unknown team's %d %q", rt.method, c.name, got.status, got.body, base.status, base.body)
			}
		}
	}
	if after := w.inviteCount(tm) + w.inviteCount(pub); after != before {
		t.Errorf("refused requests wrote %d invites", after-before)
	}
	if w.revokedAt(anaInv.InviteID).Valid {
		t.Error("a refused request revoked the owner's invite")
	}
	// And the owner, with only her session, is served.
	if l, _ := w.list(ana, tm.slug); l.Scope != "team" {
		t.Errorf("the owner's own list: scope %q", l.Scope)
	}
}

// The code a POST returns is in that response and nowhere else: not in a
// list, not in any team_event row (joining with it writes several), not in a
// zap log line and not in the request log.
func TestInviteRESTCodeNeverInListEventsOrLogs(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	w.promote(ana)
	bob := w.join(tm, "Bob")

	first, r := w.create(ana, tm.slug, `{"label":"canary","max_uses":5}`)
	if r.status != http.StatusCreated || first.Code == "" || !strings.Contains(r.body, first.Code) {
		t.Fatalf("setup: create %d", r.status)
	}
	second := w.mustCreate(bob, tm.slug, `{"label":"second canary"}`)
	codes := []string{first.Code, second.Code, tm.code}
	has := func(s string) bool {
		ls := strings.ToLower(s)
		for _, c := range codes {
			if strings.Contains(ls, strings.ToLower(c)) {
				return true
			}
		}
		return false
	}

	for _, p := range []person{ana, bob} {
		_, raw := w.list(p, tm.slug)
		if has(raw.body) || strings.Contains(raw.body, `"code"`) {
			t.Errorf("%s's list carries a code", p.name)
		}
	}
	// Redeem the canary: member_joined and agent_joined events are the rows
	// most likely to pick up what the join saw.
	w.join(tm, "Canary Redeemer")
	if r := w.call(http.MethodDelete, invitesPath(tm.slug)+"/"+second.InviteID, "", sessionHeader(bob)); r.status != http.StatusOK || has(r.body) {
		t.Errorf("revoke: %d, code present=%v", r.status, has(r.body))
	}
	_, after := w.list(ana, tm.slug)
	if has(after.body) {
		t.Error("a list after the redemption carries a code")
	}

	rows, err := w.db.Query("SELECT CONCAT_WS('|', `id`, `kind`, IFNULL(`subject_key`, ''), IFNULL(`summary`, ''), "+
		"IFNULL(CAST(`payload` AS CHAR), ''), `idempotency_key`, IFNULL(CAST(`response_snapshot` AS CHAR), '')) FROM `team_event` WHERE `team_uuid` = ?", tm.id)
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		scanned++
		if has(line) {
			t.Error("a team_event row carries a code")
		}
	}
	_ = rows.Close()
	if scanned == 0 {
		t.Fatal("no team_event rows for the team, so the scan proves nothing")
	}

	zapLines := 0
	for _, e := range w.logs.All() {
		zapLines++
		if has(e.Message + " " + fmt.Sprint(e.ContextMap())) {
			t.Error("a zap log line carries a code")
		}
	}
	reqLog := w.reqs.String()
	if !strings.Contains(reqLog, "POST "+w.srv.URL+invitesPath(tm.slug)) && !strings.Contains(reqLog, invitesPath(tm.slug)) {
		t.Fatal("the request log never saw the create, so its scan proves nothing")
	}
	if has(reqLog) {
		t.Error("the request log carries a code")
	}
	if w.count("SELECT COUNT(*) FROM `invite` WHERE `code` = ?", first.Code) != 1 {
		t.Error("the canary is not on exactly one invite row (redemption looks it up there)")
	}
	t.Logf("scanned %d team_event rows, %d zap lines and %d request-log lines", scanned, zapLines, strings.Count(reqLog, "\n"))
}

// One budget per account, whichever surface spends it.
func TestInviteRESTRateLimitIsSharedWithMCP(t *testing.T) {
	t.Setenv("METICHE_CREATE_INVITE_PER_HOUR", "3")
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	bob := w.join(tm, "Bob")
	anaMCP, bobMCP := w.connect(ana.token), w.connect(bob.token)
	args := map[string]any{"team_slug": tm.slug}

	// Ana: MCP, REST, MCP — three of three — then both surfaces refuse.
	if isErr, text := w.tool(anaMCP, "create_invite", args); isErr {
		t.Fatalf("ana's first create_invite: %s", text)
	}
	w.mustCreate(ana, tm.slug, `{}`)
	if isErr, text := w.tool(anaMCP, "create_invite", args); isErr {
		t.Fatalf("ana's third create: %s", text)
	}
	before := w.inviteCount(tm)
	_, r := w.create(ana, tm.slug, `{}`)
	title, detail := r.problem()
	retry, _ := strconv.Atoi(r.header.Get("Retry-After"))
	if r.status != http.StatusTooManyRequests || title != "rate_limited" || retry < 1 ||
		!strings.HasPrefix(detail, "rate_limited: too many invites created by this account in the last hour; try again in ") {
		t.Fatalf("ana's REST create over budget: %d Retry-After=%q %s", r.status, r.header.Get("Retry-After"), r.body)
	}
	assertNoStore(t, "rate limited", r)
	if isErr, text := w.tool(anaMCP, "create_invite", args); !isErr || !strings.HasPrefix(text, "rate_limited: too many invites created by this account in the last hour") {
		t.Errorf("ana's create_invite over budget: error=%v %s", isErr, text)
	}
	if w.inviteCount(tm) != before {
		t.Error("a rate-limited create wrote an invite")
	}

	// Bob's budget is his own, and REST spending it exhausts create_invite.
	for i := 0; i < 3; i++ {
		w.mustCreate(bob, tm.slug, `{}`)
	}
	if isErr, text := w.tool(bobMCP, "create_invite", args); !isErr || !strings.HasPrefix(text, "rate_limited:") {
		t.Errorf("bob's create_invite after three REST creates: error=%v %s", isErr, text)
	}
}
