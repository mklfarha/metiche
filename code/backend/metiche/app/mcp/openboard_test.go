package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// open_board, through the REAL transport: mcp.Register on a chi router behind
// httptest, and the SDK's streamable client carrying only an Authorization
// header — the path identity_integration_test.go established, because every
// identity failure so far lived in what actually crossed the wire.
//
// A test here never prints a login_url or a result text that holds one.

const testBoardBase = "https://board.example.test"

// loginHarness is newHarness plus the two browser-login tables, which the
// shared truncateAll predates.
func loginHarness(t *testing.T) *harness {
	t.Helper()
	hs := newHarness(t)
	for _, tbl := range []string{"browser_session", "board_login_link"} {
		if _, err := hs.core.DB().Exec("DELETE FROM `" + tbl + "`"); err != nil {
			t.Fatalf("clearing %s: %v", tbl, err)
		}
	}
	return hs
}

// loginServer is realServer with every log line captured, so a test can prove
// what never reached one.
func loginServer(t *testing.T, hs *harness) (string, *observer.ObservedLogs) {
	t.Helper()
	t.Setenv("METICHE_ROLE", "all")
	t.Setenv("METICHE_JOIN_PER_HOUR", "1000")
	t.Setenv("METICHE_BOARD_BASE_URL", testBoardBase)
	obs, logs := observer.New(zapcore.DebugLevel)
	r := chi.NewRouter()
	Register(r, hs.core, zap.New(obs))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/mcp", logs
}

func openBoard(t *testing.T, cs *mcp.ClientSession, args map[string]any) (OpenBoardResult, string) {
	t.Helper()
	res, text := callTool(t, cs, "open_board", args)
	if res.IsError {
		t.Fatalf("open_board %v refused: %s", args, text)
	}
	var out OpenBoardResult
	decodeResult(t, res, &out)
	_, secret, ok := strings.Cut(out.LoginURL, "#")
	if !ok || !strings.HasPrefix(secret, "mbl_") {
		t.Fatal("login_url carries no #mbl_ secret (value withheld)")
	}
	return out, secret
}

func openBoardRefused(t *testing.T, cs *mcp.ClientSession, args map[string]any) string {
	t.Helper()
	res, text := callTool(t, cs, "open_board", args)
	if !res.IsError {
		t.Fatalf("open_board %v succeeded; want a refusal", args)
	}
	return text
}

type storedLink struct {
	account, agent, hash, redirect string
	via                            int64
	expires                        time.Time
	consumed                       bool
}

func linkFor(t *testing.T, hs *harness, secret string) storedLink {
	t.Helper()
	var l storedLink
	var consumed sql.NullTime
	if err := hs.core.DB().QueryRow(
		"SELECT `account_uuid`, `agent_uuid`, `secret_hash`, `redirect_path`, `requested_via`, `expires_at`, `consumed_at` "+
			"FROM `board_login_link` WHERE `secret_hash` = ?", HashToken(secret)).
		Scan(&l.account, &l.agent, &l.hash, &l.redirect, &l.via, &l.expires, &consumed); err != nil {
		t.Fatalf("no board_login_link row under the secret's hash: %v", err)
	}
	l.consumed = consumed.Valid
	return l
}

func linkCount(t *testing.T, hs *harness, account uuid.UUID) int {
	t.Helper()
	return countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `board_login_link` WHERE `account_uuid` = ?", account.String())
}

// rowsContaining counts the rows of table in which ANY column, cast to text,
// contains needle. Columns come from information_schema, so a column added
// later is scanned too.
func rowsContaining(t *testing.T, db *sql.DB, table, needle string) int {
	t.Helper()
	rows, err := db.Query("SELECT `COLUMN_NAME` FROM information_schema.`COLUMNS` "+
		"WHERE `TABLE_SCHEMA` = DATABASE() AND `TABLE_NAME` = ? ORDER BY `ORDINAL_POSITION`", table)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, "COALESCE(CAST(`"+c+"` AS CHAR), '')")
	}
	_ = rows.Close()
	if len(cols) == 0 {
		t.Fatalf("table %s has no columns in this database", table)
	}
	return countRows(t, db, "SELECT COUNT(*) FROM `"+table+"` WHERE INSTR(CONCAT_WS('|', "+strings.Join(cols, ", ")+"), ?) > 0", needle)
}

// assertCanaryAbsent: the secret a real mint returned is in no stored row and
// no log line — only its hash is stored, and the scan finds THAT (the positive
// control that proves the scan works).
func assertCanaryAbsent(t *testing.T, hs *harness, logs *observer.ObservedLogs, secret string) {
	t.Helper()
	body := strings.TrimPrefix(secret, "mbl_")
	if len(body) < 40 {
		t.Fatalf("canary is %d chars; a real link secret is 43", len(body))
	}
	db := hs.core.DB()
	for _, table := range []string{"board_login_link", "browser_session", "team_event"} {
		if n := rowsContaining(t, db, table, body); n != 0 {
			t.Errorf("the link secret is stored in the clear in %d %s row(s)", n, table)
		}
	}
	assertNoSecretsInEventLog(t, hs, secret, body)
	if n := rowsContaining(t, db, "board_login_link", HashToken(secret)); n != 1 {
		t.Errorf("positive control: the secret's hash is in %d board_login_link rows, want 1", n)
	}

	if logs.FilterField(zap.String("tool", "open_board")).Len() == 0 {
		t.Error("positive control: no open_board log line was captured, so the log check proves nothing")
	}
	for _, e := range logs.All() {
		if strings.Contains(e.Message+" "+fmt.Sprint(e.ContextMap()), body) {
			t.Errorf("the link secret reached a log line: %q", e.Message)
		}
	}
}

// ─────────────────────────────────────────────
// minting
// ─────────────────────────────────────────────

func TestIntegrationOpenBoardAgentTokenMints(t *testing.T) {
	hs := loginHarness(t)
	endpoint, logs := loginServer(t, hs)
	slug := hs.teamSlug(t)
	ana := hs.join(t, "Ana", "laptop-claude")
	eventsBefore := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`")

	cs := connectAs(t, endpoint, ana.token)
	before := time.Now().UTC()
	out, secret := openBoard(t, cs, map[string]any{"requested_via": "installer"})

	if !out.OK || !out.SingleUse {
		t.Errorf("ok=%v single_use=%v", out.OK, out.SingleUse)
	}
	if !strings.HasPrefix(out.LoginURL, testBoardBase+"/signin#mbl_") {
		t.Errorf("login_url is not %s/signin#mbl_… (value withheld)", testBoardBase)
	}
	if out.BoardURL != testBoardBase+"/t/"+slug {
		t.Errorf("board_url = %q, want %s/t/%s", out.BoardURL, testBoardBase, slug)
	}
	if out.Team == nil || out.Team.Slug != slug || out.Team.Name != "Test team" || out.Team.Visibility != "private" {
		t.Errorf("team = %+v", out.Team)
	}
	if d := out.LoginExpiresAt.Sub(before); d < 10*time.Minute-3*time.Second || d > 10*time.Minute+3*time.Second {
		t.Errorf("login_expires_at is %s after the call, want 10m", d)
	}
	if !strings.Contains(out.Note, "works once") {
		t.Errorf("note = %q", out.Note)
	}
	t.Logf("minted: board_url=%s redirect team=%s expires_in=%s login_url=<redacted>", out.BoardURL, out.Team.Slug,
		out.LoginExpiresAt.Sub(before).Round(time.Second))

	l := linkFor(t, hs, secret)
	if l.account != ana.account.ID.String() || l.agent != ana.agent.ID.String() {
		t.Errorf("stored link is for account %s agent %s, want Ana's", l.account, l.agent)
	}
	if l.redirect != "/t/"+slug || l.via != enums.BOARD_LINK_SOURCE_INSTALLER || l.consumed {
		t.Errorf("stored redirect=%q via=%d consumed=%v", l.redirect, l.via, l.consumed)
	}
	if len(l.hash) != 64 || l.hash != HashToken(secret) || strings.Contains(l.hash, strings.TrimPrefix(secret, "mbl_")) {
		t.Errorf("secret_hash is not sha256(secret)")
	}

	assertCanaryAbsent(t, hs, logs, secret)
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`"); n != eventsBefore {
		t.Errorf("open_board wrote %d team_event row(s); a sign-in link is not board state", n-eventsBefore)
	}

	// Every call is a new link.
	if _, again := openBoard(t, cs, map[string]any{}); again == secret {
		t.Error("a second open_board returned the same secret")
	}
	if n := linkCount(t, hs, ana.account.ID); n != 2 {
		t.Errorf("%d links stored, want 2", n)
	}
}

func TestIntegrationOpenBoardRefusesALegacyAccountToken(t *testing.T) {
	hs := loginHarness(t)
	endpoint, _ := loginServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")

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

	cs := connectAs(t, endpoint, legacy)
	if res, text := callTool(t, cs, "list_teams", map[string]any{}); res.IsError {
		t.Fatalf("sanity: the legacy token must authenticate: %s", text)
	}
	text := openBoardRefused(t, cs, map[string]any{"team_slug": hs.teamSlug(t)})
	t.Logf("legacy token -> %s", text)
	if !strings.Contains(text, "not_permitted: open_board needs this client's own token; re-run the metiche installer") {
		t.Fatalf("legacy token refused for the wrong reason: %s", text)
	}
	if n := linkCount(t, hs, ana.account.ID); n != 0 {
		t.Fatalf("a refused legacy mint stored %d link(s)", n)
	}
}

func TestIntegrationOpenBoardRetiredAgentIsRefusedAtTheEdge(t *testing.T) {
	hs := loginHarness(t)
	endpoint, _ := loginServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")

	cs := connectAs(t, endpoint, ana.token)
	openBoard(t, cs, map[string]any{})

	if _, err := hs.core.DB().Exec("UPDATE `agent` SET `status` = ? WHERE `id` = ?",
		enums.AGENT_STATUS_RETIRED, ana.agent.ID.String()); err != nil {
		t.Fatal(err)
	}

	// The HTTP edge: 401 before any tool runs.
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"open_board","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+ana.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	t.Logf("retired agent, raw tools/call open_board -> HTTP %d", resp.StatusCode)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("retired agent's open_board: HTTP %d, want 401", resp.StatusCode)
	}

	// A new connection is refused, and so is the next call on the open one.
	if c, err := dialAs(endpoint, ana.token); err == nil {
		_ = c.Close()
		t.Fatal("a retired agent's token opened an MCP session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "open_board", Arguments: map[string]any{}})
	if err == nil && !res.IsError {
		t.Fatal("open_board succeeded on a connection opened before the agent was retired")
	}
	if n := linkCount(t, hs, ana.account.ID); n != 1 {
		t.Fatalf("%d links after retirement, want the 1 minted before it", n)
	}
}

func TestIntegrationOpenBoardSixthOutstandingLinkIsRefused(t *testing.T) {
	hs := loginHarness(t)
	endpoint, _ := loginServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")
	bob := hs.join(t, "Bob", "bob-laptop")
	asAna := connectAs(t, endpoint, ana.token)

	var secrets []string
	for i := 0; i < 5; i++ {
		_, s := openBoard(t, asAna, map[string]any{})
		secrets = append(secrets, s)
	}
	text := openBoardRefused(t, asAna, map[string]any{})
	t.Logf("6th outstanding -> %s", text)
	if !strings.HasPrefix(strings.TrimSpace(text), "rate_limited:") {
		t.Fatalf("6th outstanding link refused for the wrong reason: %s", text)
	}
	if n := linkCount(t, hs, ana.account.ID); n != 5 {
		t.Fatalf("%d links stored, want 5", n)
	}

	// Another account's budget is its own.
	openBoard(t, connectAs(t, endpoint, bob.token), map[string]any{})

	// A consumed link frees its slot, and only its slot.
	if _, err := hs.core.DB().Exec("UPDATE `board_login_link` SET `consumed_at` = ? WHERE `secret_hash` = ?",
		time.Now().UTC(), HashToken(secrets[0])); err != nil {
		t.Fatal(err)
	}
	openBoard(t, asAna, map[string]any{})
	if text := openBoardRefused(t, asAna, map[string]any{}); !strings.Contains(text, "rate_limited:") {
		t.Fatalf("after refilling the freed slot: %s", text)
	}

	// Expired links are not outstanding.
	if _, err := hs.core.DB().Exec("UPDATE `board_login_link` SET `expires_at` = ? WHERE `account_uuid` = ?",
		time.Now().UTC().Add(-time.Minute), ana.account.ID.String()); err != nil {
		t.Fatal(err)
	}
	openBoard(t, asAna, map[string]any{})
}

func TestIntegrationOpenBoardHourlyLimit(t *testing.T) {
	t.Setenv("METICHE_OPEN_BOARD_PER_HOUR", "2")
	hs := loginHarness(t)
	endpoint, _ := loginServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")
	cs := connectAs(t, endpoint, ana.token)
	openBoard(t, cs, map[string]any{})
	openBoard(t, cs, map[string]any{})
	text := openBoardRefused(t, cs, map[string]any{})
	if !strings.Contains(text, "rate_limited: too many sign-in links for this account in the last hour") {
		t.Fatalf("3rd mint with a budget of 2: %s", text)
	}
	if n := linkCount(t, hs, ana.account.ID); n != 2 {
		t.Fatalf("%d links, want 2", n)
	}
}

func TestIntegrationOpenBoardRedirectFollowsMemberships(t *testing.T) {
	hs := loginHarness(t)
	endpoint, _ := loginServer(t, hs)
	slug := hs.teamSlug(t)
	ana := hs.join(t, "Ana", "laptop-claude")
	asAna := connectAs(t, endpoint, ana.token)

	teamSlugByID := func(id uuid.UUID) string {
		var s string
		if err := hs.core.DB().QueryRow("SELECT `slug` FROM `team` WHERE `id` = ?", id.String()).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	check := func(name string, out OpenBoardResult, secret, wantPath string, wantTeam bool) {
		t.Helper()
		got := linkFor(t, hs, secret).redirect
		t.Logf("%-28s redirect_path=%-32s board_url=%s team=%v", name, got, out.BoardURL, out.Team != nil)
		if got != wantPath {
			t.Errorf("%s: stored redirect_path %q, want %q", name, got, wantPath)
		}
		if out.BoardURL != testBoardBase+wantPath {
			t.Errorf("%s: board_url %q, want %q", name, out.BoardURL, testBoardBase+wantPath)
		}
		if (out.Team != nil) != wantTeam {
			t.Errorf("%s: team present=%v, want %v", name, out.Team != nil, wantTeam)
		}
	}

	// Exactly one team.
	out, s := openBoard(t, asAna, map[string]any{})
	check("one team", out, s, "/t/"+slug, true)

	// Several teams.
	secondID, code2 := seedTeam(t, hs, "Second Team")
	if _, tok := joinWithCode(t, hs, ana.ctx, ana.token, code2, "Ana", "laptop-claude"); tok != ana.token {
		t.Fatal("setup: joining a second team must keep Ana's token")
	}
	out, s = openBoard(t, asAna, map[string]any{})
	check("several teams", out, s, "/teams", false)

	// Several teams, one named.
	slug2 := teamSlugByID(secondID)
	out, s = openBoard(t, asAna, map[string]any{"team_slug": slug2})
	check("several teams, slug given", out, s, "/t/"+slug2, true)

	// A team she is not on: refused, nothing stored.
	thirdID, _ := seedTeam(t, hs, "Not Anas")
	before := linkCount(t, hs, ana.account.ID)
	if text := openBoardRefused(t, asAna, map[string]any{"team_slug": teamSlugByID(thirdID)}); !strings.Contains(text, "not a member") {
		t.Errorf("a non-member's slug: %s", text)
	}
	if n := linkCount(t, hs, ana.account.ID); n != before {
		t.Errorf("a refused slug stored a link")
	}

	// No live membership at all.
	carol := hs.join(t, "Carol", "carol-laptop")
	if _, err := hs.core.DB().Exec("UPDATE `member` SET `revoked_at` = ? WHERE `account_uuid` = ?",
		time.Now().UTC(), carol.account.ID.String()); err != nil {
		t.Fatal(err)
	}
	out, s = openBoard(t, connectAs(t, endpoint, carol.token), map[string]any{})
	check("no teams", out, s, "/teams", false)
}

func TestIntegrationOpenBoardRequestedViaAndConfig(t *testing.T) {
	hs := loginHarness(t)
	endpoint, _ := loginServer(t, hs)
	ana := hs.join(t, "Ana", "laptop-claude")
	cs := connectAs(t, endpoint, ana.token)

	_, s := openBoard(t, cs, map[string]any{})
	if via := linkFor(t, hs, s).via; via != enums.BOARD_LINK_SOURCE_AGENT {
		t.Errorf("default requested_via stored %d, want agent", via)
	}
	_, s = openBoard(t, cs, map[string]any{"requested_via": "cli"})
	if via := linkFor(t, hs, s).via; via != enums.BOARD_LINK_SOURCE_CLI {
		t.Errorf("requested_via=cli stored %d", via)
	}
	if text := openBoardRefused(t, cs, map[string]any{"requested_via": "browser"}); !strings.Contains(text, "requested_via") {
		t.Errorf("an unknown requested_via: %s", text)
	}

	// A misconfigured board address refuses rather than minting a link to it.
	t.Setenv("METICHE_BOARD_BASE_URL", "http://board.example.test")
	if text := openBoardRefused(t, cs, map[string]any{}); !strings.HasPrefix(strings.TrimSpace(text), "unavailable:") {
		t.Errorf("a plain-http board base URL: %s", text)
	}
	if n := linkCount(t, hs, ana.account.ID); n != 2 {
		t.Errorf("%d links, want 2", n)
	}
}

// ─────────────────────────────────────────────
// no database
// ─────────────────────────────────────────────

func TestParseRequestedVia(t *testing.T) {
	for in, want := range map[string]enums.BoardLinkSource{
		"": enums.BOARD_LINK_SOURCE_AGENT, "agent": enums.BOARD_LINK_SOURCE_AGENT, " CLI ": enums.BOARD_LINK_SOURCE_CLI,
		"installer": enums.BOARD_LINK_SOURCE_INSTALLER,
	} {
		if got, err := parseRequestedVia(in); err != nil || got != want {
			t.Errorf("parseRequestedVia(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseRequestedVia("invalid"); err == nil {
		t.Error(`parseRequestedVia("invalid") accepted`)
	}
}

// TestBoardLoginToolAnnotations pins §5.4's annotations. sign_out_browsers is
// the one tool on this surface that declares itself destructive: it ends a
// person's signed-in browsers.
func TestBoardLoginToolAnnotations(t *testing.T) {
	newServer(NewHandler(nil, zap.NewNop()), zap.NewNop())
	found := map[string]*mcp.Tool{}
	for _, tool := range registered {
		found[tool.Name] = tool
	}
	ob, so := found["open_board"], found["sign_out_browsers"]
	if ob == nil || so == nil {
		t.Fatalf("open_board registered=%v sign_out_browsers registered=%v", ob != nil, so != nil)
	}
	a := ob.Annotations
	if a.ReadOnlyHint || a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Errorf("open_board annotations = %+v, want additive (not read-only, not idempotent, not destructive, closed world)", a)
	}
	a = so.Annotations
	if a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || !*a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Errorf("sign_out_browsers annotations = %+v, want destructive and idempotent, closed world", a)
	}
	for _, tool := range []*mcp.Tool{ob, so} {
		if !strings.Contains(tool.Description, "never") && !strings.Contains(tool.Description, "Never") {
			t.Errorf("%s description does not say what never to do with it", tool.Name)
		}
	}
	if !strings.Contains(serverInstructions(), "call open_board") {
		t.Error("server instructions do not tell the model to call open_board")
	}
}
