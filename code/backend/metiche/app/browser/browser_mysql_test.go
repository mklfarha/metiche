package browser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

// These tests are SQL against the generated schema. Point METICHE_TEST_MYSQL_DSN
// at a MySQL holding create.sql, with parseTime=true&interpolateParams=true
// (what production runs), and run with -p 1: they create and delete rows.
const dsnEnv = "METICHE_TEST_MYSQL_DSN"

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("skipped: %s is not set, so there is no MySQL to run against", dsnEnv)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}

type person struct {
	account, agent, key, name string
}

func (p person) accountUUID() uuid.UUID { return uuid.FromStringOrNil(p.account) }
func (p person) agentUUID() uuid.UUID   { return uuid.FromStringOrNil(p.agent) }

// seedPerson creates an ACTIVE account with one ACTIVE agent. Deleting the
// account cascades to agents, links and sessions.
func seedPerson(t *testing.T, db *sql.DB, name string) person {
	t.Helper()
	p := person{account: newID(t), agent: newID(t), name: name}
	p.key = "acct-" + p.account[:8]
	exec(t, db, "INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
		p.account, p.key, name, Hash("fake-token-"+p.account), 1, enums.RECORD_STATUS_ACTIVE)
	exec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_kind`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?)",
		p.agent, p.account, "A-"+p.agent[:6], "claude-1", "claude-code", "client-"+p.agent[:8], enums.AGENT_STATUS_ACTIVE)
	t.Cleanup(func() { exec(t, db, "DELETE FROM `account` WHERE `id` = ?", p.account) })
	return p
}

func mint(t *testing.T, db *sql.DB, p person, redirect string) MintedLink {
	t.Helper()
	m, err := MintLink(context.Background(), db, MintLinkRequest{
		AccountUUID: p.accountUUID(), AgentUUID: p.agentUUID(), RedirectPath: redirect,
		RequestedVia: enums.BOARD_LINK_SOURCE_INSTALLER, BoardBaseURL: "http://localhost:8787",
	})
	if err != nil {
		t.Fatalf("MintLink: %v", err)
	}
	return m
}

func newServer(t *testing.T, db *sql.DB) (*httptest.Server, *API) {
	t.Helper()
	api := NewAPI(db, nil)
	r := chi.NewRouter()
	api.RegisterOn(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, api
}

type resp struct {
	code   int
	body   string
	header http.Header
}

func call(t *testing.T, method, url, session, body string, extra ...string) resp {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if session != "" {
		req.Header.Set(HeaderSession, session)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, string(b), res.Header}
}

func exchangeBody(secret string) string {
	b, _ := json.Marshal(ExchangeRequest{LinkSecret: secret, UserAgent: "Mozilla/5.0 (test)", IPHint: "203.0.113.77"})
	return string(b)
}

func exchange(t *testing.T, srv *httptest.Server, secret string) resp {
	t.Helper()
	return call(t, http.MethodPost, srv.URL+PathSessions, "", exchangeBody(secret))
}

func mustExchange(t *testing.T, srv *httptest.Server, secret string) Exchanged {
	t.Helper()
	r := exchange(t, srv, secret)
	if r.code != http.StatusCreated {
		t.Fatalf("exchange = %d %s", r.code, r.body)
	}
	var out Exchanged
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The one refusal body, as the unknown link produces it.
func unknownLinkRefusal(t *testing.T, srv *httptest.Server) resp {
	t.Helper()
	r := exchange(t, srv, "mbl_obviously-fake-unknown-link")
	if r.code != http.StatusNotFound {
		t.Fatalf("unknown link = %d %s", r.code, r.body)
	}
	return r
}

func assertSameRefusal(t *testing.T, what string, got, want resp) {
	t.Helper()
	if got.code != want.code || got.body != want.body || got.header.Get("Content-Type") != want.header.Get("Content-Type") {
		t.Errorf("%s = %d %q (%s); want the one refusal %d %q (%s)", what,
			got.code, got.body, got.header.Get("Content-Type"), want.code, want.body, want.header.Get("Content-Type"))
	}
	if got.header.Get("Set-Cookie") != "" {
		t.Errorf("%s set a cookie", what)
	}
}

func consumedAt(t *testing.T, db *sql.DB, secret string) sql.NullTime {
	t.Helper()
	var c sql.NullTime
	if err := db.QueryRow("SELECT `consumed_at` FROM `board_login_link` WHERE `secret_hash` = ?", Hash(secret)).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// ── exchange ────────────────────────────────────────────────────────────────

func TestExchangeCreatesSessionAndStoresOnlyHashes(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	link := mint(t, db, ana, "/t/taqueria-tracker")

	if !strings.HasPrefix(link.Secret, LinkPrefix) || link.LoginURL != "http://localhost:8787/signin#"+link.Secret ||
		link.BoardURL != "http://localhost:8787/t/taqueria-tracker" || time.Until(link.ExpiresAt) > LinkTTL+time.Second {
		t.Fatalf("minted link shape: %s", link)
	}
	if strings.Contains(link.String(), link.Secret) || strings.Contains(fmt.Sprintf("%v", link), link.Secret) {
		t.Fatal("MintedLink formats its secret")
	}

	r := exchange(t, srv, link.Secret)
	if r.code != http.StatusCreated || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange = %d cache %q %s", r.code, r.header.Get("Cache-Control"), r.body)
	}
	var out Exchanged
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.SessionSecret, SessionPrefix) || !strings.HasPrefix(out.SessionKey, "BS-") || len(out.SessionKey) != 13 ||
		out.RedirectPath != "/t/taqueria-tracker" || out.AccountKey != ana.key || out.DisplayName != "Ana" {
		t.Fatalf("exchange body: %+v", out)
	}
	if d := time.Until(out.ExpiresAt); d < SessionAbsoluteTTL-time.Minute || d > SessionAbsoluteTTL+time.Minute {
		t.Fatalf("expires_at %v is not now+30d", out.ExpiresAt)
	}
	for _, k := range []string{"session_secret", "session_key", "expires_at", "redirect_path", "account_key", "display_name"} {
		if !strings.Contains(r.body, `"`+k+`"`) {
			t.Errorf("exchange body lacks %q", k)
		}
	}

	if !consumedAt(t, db, link.Secret).Valid {
		t.Error("link not consumed")
	}
	var (
		hash, agent, ua, ip string
		method              int64
		lastSeen            sql.NullTime
	)
	if err := db.QueryRow("SELECT `secret_hash`, `created_from_agent_uuid`, `auth_method`, `user_agent`, `ip_hint`, `last_seen_at` FROM `browser_session` WHERE `key` = ?",
		out.SessionKey).Scan(&hash, &agent, &method, &ua, &ip, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if hash != Hash(out.SessionSecret) || agent != ana.agent || enums.BrowserAuthMethod(method) != enums.BROWSER_AUTH_METHOD_TERMINAL_LINK ||
		ua != "Mozilla/5.0 (test)" || ip != "203.0.113.0/24" || !lastSeen.Valid {
		t.Fatalf("session row: hash ok=%v agent=%s method=%d ua=%q ip=%q lastSeen=%v", hash == Hash(out.SessionSecret), agent, method, ua, ip, lastSeen)
	}
	// Canary: neither plaintext is in either table, in any column.
	for _, table := range []string{"board_login_link", "browser_session"} {
		rows, err := db.Query("SELECT * FROM `"+table+"` WHERE `account_uuid` = ?", ana.account)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]sql.RawBytes, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if bytes.Contains(v, []byte(link.Secret)) || bytes.Contains(v, []byte(out.SessionSecret)) ||
					bytes.Contains(v, []byte(LinkPrefix)) || bytes.Contains(v, []byte(SessionPrefix)) {
					t.Errorf("%s.%s holds a plaintext secret", table, cols[i])
				}
			}
		}
		_ = rows.Close()
	}

	cur := call(t, http.MethodGet, srv.URL+PathSession, out.SessionSecret, "")
	if cur.code != http.StatusOK || !strings.Contains(cur.body, out.SessionKey) || !strings.Contains(cur.body, `"display_name":"Ana"`) ||
		strings.Contains(cur.body, out.SessionSecret) || cur.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET session = %d %s", cur.code, cur.body)
	}
}

func TestExchangeReuseRefused(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	link := mint(t, db, ana, "/")
	mustExchange(t, srv, link.Secret)
	assertSameRefusal(t, "immediate second exchange of one link", exchange(t, srv, link.Secret), unknownLinkRefusal(t, srv))
	// Replay it a minute later. This step is what makes the test able to fail:
	// MySQL's RowsAffected counts CHANGED rows, so a replay in the same second
	// would write an identical consumed_at, affect 0 rows and be refused even
	// with `consumed_at IS NULL` missing from the UPDATE. A real replay comes
	// later, when consumed_at would change.
	exec(t, db, "UPDATE `board_login_link` SET `consumed_at` = ? WHERE `secret_hash` = ?", dbNow().Add(-time.Minute), Hash(link.Secret))
	assertSameRefusal(t, "second exchange of one link a minute later", exchange(t, srv, link.Secret), unknownLinkRefusal(t, srv))
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM `browser_session` WHERE `account_uuid` = ?", ana.account).Scan(&n)
	if n != 1 {
		t.Errorf("%d sessions after a reused link, want 1", n)
	}
}

func TestExchangeExpiredRefused(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	link := mint(t, db, ana, "/teams")
	exec(t, db, "UPDATE `board_login_link` SET `expires_at` = ? WHERE `secret_hash` = ?", dbNow().Add(-time.Second), Hash(link.Secret))
	assertSameRefusal(t, "expired link", exchange(t, srv, link.Secret), unknownLinkRefusal(t, srv))
	if consumedAt(t, db, link.Secret).Valid {
		t.Error("a refused expired link was consumed")
	}
}

func TestExchangeRetiredAgentRefused(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	link := mint(t, db, ana, "/")
	exec(t, db, "UPDATE `agent` SET `status` = ? WHERE `id` = ?", enums.AGENT_STATUS_RETIRED, ana.agent)
	assertSameRefusal(t, "retired agent's link", exchange(t, srv, link.Secret), unknownLinkRefusal(t, srv))
	if consumedAt(t, db, link.Secret).Valid {
		t.Error("the refused exchange was not rolled back: link consumed")
	}
}

func TestExchangeInactiveAccountRefused(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	link := mint(t, db, ana, "/")
	exec(t, db, "UPDATE `account` SET `status` = ? WHERE `id` = ?", enums.RECORD_STATUS_INACTIVE, ana.account)
	assertSameRefusal(t, "inactive account's link", exchange(t, srv, link.Secret), unknownLinkRefusal(t, srv))
}

func TestExchangeMalformedIsTheSameRefusal(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	want := unknownLinkRefusal(t, srv)
	for _, body := range []string{"", "{", `{"link_secret":""}`, `{"link_secret":42}`, `{"link_secret":"mbs_session-not-link"}`, strings.Repeat("x", 20000)} {
		assertSameRefusal(t, "malformed "+body[:min(len(body), 20)], call(t, http.MethodPost, srv.URL+PathSessions, "", body), want)
	}
}

// Two (here: 24) concurrent exchanges of one link: exactly one 201.
func TestExchangeConcurrentExactlyOneWins(t *testing.T) {
	db := testDB(t)
	db.SetMaxOpenConns(32)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	for round := 0; round < 5; round++ {
		link := mint(t, db, ana, "/")
		const racers = 24
		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			codes = make([]int, racers)
		)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				res, err := http.Post(srv.URL+PathSessions, "application/json", strings.NewReader(exchangeBody(link.Secret)))
				if err != nil {
					codes[i] = -1
					return
				}
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				codes[i] = res.StatusCode
			}(i)
		}
		close(start)
		wg.Wait()
		counts := map[int]int{}
		for _, c := range codes {
			counts[c]++
		}
		t.Logf("round %d: %d concurrent exchanges of one link -> %v", round, racers, counts)
		if counts[http.StatusCreated] != 1 || counts[http.StatusNotFound] != racers-1 {
			t.Fatalf("round %d: want exactly one 201 and %d 404, got %v", round, racers-1, counts)
		}
	}
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM `browser_session` WHERE `account_uuid` = ?", ana.account).Scan(&n)
	if n != 5 {
		t.Fatalf("%d sessions for 5 links", n)
	}
}

// A database failure inside the exchange transaction (after the UPDATE) is 503
// and consumes nothing; the link then works.
func TestExchangeDatabaseFailureConsumesNothing(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	link := mint(t, db, ana, "/")
	exec(t, db, "RENAME TABLE `browser_session` TO `browser_session_moved_by_test`")
	restored := false
	restore := func() {
		if !restored {
			exec(t, db, "RENAME TABLE `browser_session_moved_by_test` TO `browser_session`")
			restored = true
		}
	}
	t.Cleanup(restore)

	r := exchange(t, srv, link.Secret)
	if r.code != http.StatusServiceUnavailable || strings.Contains(r.body, "browser_session") || strings.Contains(strings.ToLower(r.body), "table") {
		t.Fatalf("exchange with the session table gone = %d %s; want an opaque 503", r.code, r.body)
	}
	restore()
	if consumedAt(t, db, link.Secret).Valid {
		t.Fatal("a failed exchange consumed the link")
	}
	mustExchange(t, srv, link.Secret)
}

func TestExchangeBackstopIs503WithRetryAfter(t *testing.T) {
	db := testDB(t)
	srv, api := newServer(t, db)
	api.exchangeLimit = newWindowLimiter(1, time.Hour)
	_ = unknownLinkRefusal(t, srv)
	r := exchange(t, srv, "mbl_obviously-fake")
	if r.code != http.StatusServiceUnavailable || r.header.Get("Retry-After") == "" {
		t.Fatalf("over the backstop = %d retry %q", r.code, r.header.Get("Retry-After"))
	}
}

// ── minting ─────────────────────────────────────────────────────────────────

func TestMintLinkRefusalsAndCap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ana := seedPerson(t, db, "Ana")
	beto := seedPerson(t, db, "Beto")

	req := func(p person, agent uuid.UUID, redirect string) MintLinkRequest {
		return MintLinkRequest{AccountUUID: p.accountUUID(), AgentUUID: agent, RedirectPath: redirect}
	}
	if _, err := MintLink(ctx, db, req(ana, beto.agentUUID(), "/")); !errors.Is(err, ErrMintRefused) {
		t.Errorf("another account's agent: %v", err)
	}
	if _, err := MintLink(ctx, db, req(ana, ana.agentUUID(), "//evil.example")); !errors.Is(err, ErrInvalidRedirect) {
		t.Errorf("bad redirect: %v", err)
	}
	if _, err := MintLink(ctx, db, MintLinkRequest{AccountUUID: ana.accountUUID(), AgentUUID: ana.agentUUID(), RedirectPath: "/", BoardBaseURL: "http://metiche.xyz"}); !errors.Is(err, ErrInvalidBoardBaseURL) {
		t.Errorf("bad base: %v", err)
	}
	m, err := MintLink(ctx, db, req(ana, ana.agentUUID(), "/"))
	if err != nil || !strings.HasPrefix(m.LoginURL, DefaultBoardBaseURL+"/signin#mbl_") {
		t.Fatalf("default base: %v", err)
	}
	var via int64
	_ = db.QueryRow("SELECT `requested_via` FROM `board_login_link` WHERE `secret_hash` = ?", Hash(m.Secret)).Scan(&via)
	if enums.BoardLinkSource(via) != enums.BOARD_LINK_SOURCE_AGENT {
		t.Errorf("requested_via default = %d", via)
	}

	// Concurrent mints on one account: the cap is exact (5 outstanding). One
	// is already outstanding above.
	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		ok, capped, bad int
	)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := MintLink(ctx, db, req(ana, ana.agentUUID(), "/teams"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrTooManyLinks):
				capped++
			default:
				bad++
				t.Logf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	t.Logf("12 concurrent mints with 1 outstanding: %d minted, %d capped", ok, capped)
	if ok != MaxOutstandingLinks-1 || capped != 12-ok || bad != 0 {
		t.Fatalf("cap not exact: ok=%d capped=%d other=%d", ok, capped, bad)
	}
	// A consumed or expired link no longer counts.
	exec(t, db, "UPDATE `board_login_link` SET `expires_at` = ? WHERE `secret_hash` = ?", dbNow().Add(-time.Second), Hash(m.Secret))
	if _, err := MintLink(ctx, db, req(ana, ana.agentUUID(), "/")); err != nil {
		t.Errorf("after one expired: %v", err)
	}
	// At the cap again; a consumed link no longer counts either.
	if _, err := MintLink(ctx, db, req(ana, ana.agentUUID(), "/")); !errors.Is(err, ErrTooManyLinks) {
		t.Fatalf("back at the cap: %v", err)
	}
	exec(t, db, "UPDATE `board_login_link` SET `consumed_at` = ? WHERE `account_uuid` = ? AND `consumed_at` IS NULL AND `expires_at` > ? LIMIT 1",
		dbNow(), ana.account, dbNow())
	if _, err := MintLink(ctx, db, req(ana, ana.agentUUID(), "/")); err != nil {
		t.Errorf("after one consumed: %v", err)
	}

	exec(t, db, "UPDATE `agent` SET `status` = ? WHERE `id` = ?", enums.AGENT_STATUS_RETIRED, beto.agent)
	if _, err := MintLink(ctx, db, req(beto, beto.agentUUID(), "/")); !errors.Is(err, ErrMintRefused) {
		t.Errorf("retired agent: %v", err)
	}
	exec(t, db, "UPDATE `agent` SET `status` = ? WHERE `id` = ?", enums.AGENT_STATUS_ACTIVE, beto.agent)
	exec(t, db, "UPDATE `account` SET `status` = ? WHERE `id` = ?", enums.RECORD_STATUS_INACTIVE, beto.account)
	if _, err := MintLink(ctx, db, req(beto, beto.agentUUID(), "/")); !errors.Is(err, ErrMintRefused) {
		t.Errorf("inactive account: %v", err)
	}
}

// ── session validation ──────────────────────────────────────────────────────

func signIn(t *testing.T, db *sql.DB, srv *httptest.Server, p person) Exchanged {
	t.Helper()
	return mustExchange(t, srv, mint(t, db, p, "/").Secret)
}

func assert401(t *testing.T, srv *httptest.Server, what, secret string, extra ...string) {
	t.Helper()
	r := call(t, http.MethodGet, srv.URL+PathSession, secret, "", extra...)
	want := `{"detail":"no such browser session","status":401,"title":"unauthorized","type":"about:blank"}` + "\n"
	if r.code != http.StatusUnauthorized || r.body != want || r.header.Get("Content-Type") != "application/problem+json" {
		t.Errorf("%s: GET session = %d %q; want the one 401", what, r.code, r.body)
	}
}

func assert200(t *testing.T, srv *httptest.Server, what, secret string) {
	t.Helper()
	if r := call(t, http.MethodGet, srv.URL+PathSession, secret, ""); r.code != http.StatusOK {
		t.Errorf("%s: GET session = %d %s; want 200", what, r.code, r.body)
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	s := signIn(t, db, srv, seedPerson(t, db, "Ana"))
	exec(t, db, "UPDATE `browser_session` SET `last_seen_at` = ? WHERE `key` = ?", dbNow().Add(-SessionIdleTTL+time.Hour), s.SessionKey)
	assert200(t, srv, "seen 6d23h ago", s.SessionSecret)
	exec(t, db, "UPDATE `browser_session` SET `last_seen_at` = ?, `created_at` = ? WHERE `key` = ?",
		dbNow().Add(-SessionIdleTTL-time.Minute), dbNow().Add(-8*24*time.Hour), s.SessionKey)
	assert401(t, srv, "idle 7d1m", s.SessionSecret)
}

func TestSessionAbsoluteExpiry(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	s := signIn(t, db, srv, seedPerson(t, db, "Ana"))
	exec(t, db, "UPDATE `browser_session` SET `expires_at` = ? WHERE `key` = ?", dbNow().Add(-time.Second), s.SessionKey)
	assert401(t, srv, "absolutely expired", s.SessionSecret)
}

func TestSessionRevoked(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	s := signIn(t, db, srv, seedPerson(t, db, "Ana"))
	exec(t, db, "UPDATE `browser_session` SET `revoked_at` = ?, `end_reason` = ? WHERE `key` = ?", dbNow(), enums.BROWSER_SESSION_END_REASON_REVOKED, s.SessionKey)
	assert401(t, srv, "revoked", s.SessionSecret)
}

func TestSessionOriginAgentRetired(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	s := signIn(t, db, srv, ana)
	assert200(t, srv, "agent active", s.SessionSecret)
	exec(t, db, "UPDATE `agent` SET `status` = ? WHERE `id` = ?", enums.AGENT_STATUS_RETIRED, ana.agent)
	assert401(t, srv, "origin agent retired", s.SessionSecret)
	if _, err := AccountBySession(context.Background(), db, s.SessionSecret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("AccountBySession after retire = %v", err)
	}
	exec(t, db, "UPDATE `agent` SET `status` = ? WHERE `id` = ?", enums.AGENT_STATUS_ACTIVE, ana.agent)
	assert200(t, srv, "agent reactivated", s.SessionSecret)
}

func TestSessionInactiveAccountAndOtherRefusals(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana := seedPerson(t, db, "Ana")
	s := signIn(t, db, srv, ana)

	acct, err := AccountBySession(context.Background(), db, s.SessionSecret)
	if err != nil || acct.ID.String() != ana.account || acct.Status != enums.RECORD_STATUS_ACTIVE || acct.TokenHash != "" {
		t.Fatalf("AccountBySession = %+v, %v", acct, err)
	}

	assert401(t, srv, "no header", "")
	assert401(t, srv, "garbage", "mbs_obviously-fake")
	assert401(t, srv, "wrong prefix", "mtk_obviously-fake")
	assert401(t, srv, "the hash itself", Hash(s.SessionSecret))
	assert401(t, srv, "valid session plus a bearer", s.SessionSecret, "Authorization", "Bearer mtk_obviously-fake")
	assert401(t, srv, "valid session plus X-Metiche-Token", s.SessionSecret, "X-Metiche-Token", "mtk_obviously-fake")
	if r := call(t, http.MethodGet, srv.URL+PathSession+"?session="+s.SessionSecret, "", ""); r.code != http.StatusUnauthorized {
		t.Errorf("session in the query string was accepted: %d", r.code)
	}
	assert200(t, srv, "still valid", s.SessionSecret)

	exec(t, db, "UPDATE `account` SET `status` = ? WHERE `id` = ?", enums.RECORD_STATUS_INACTIVE, ana.account)
	assert401(t, srv, "inactive account", s.SessionSecret)
}

func TestLastSeenWrittenAtMostEveryTenMinutes(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	s := signIn(t, db, srv, seedPerson(t, db, "Ana"))
	lastSeen := func() time.Time {
		var ts time.Time
		if err := db.QueryRow("SELECT `last_seen_at` FROM `browser_session` WHERE `key` = ?", s.SessionKey).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts.UTC()
	}
	fiveAgo := dbNow().Add(-5 * time.Minute)
	exec(t, db, "UPDATE `browser_session` SET `last_seen_at` = ? WHERE `key` = ?", fiveAgo, s.SessionKey)
	for i := 0; i < 3; i++ {
		assert200(t, srv, "5 min", s.SessionSecret)
	}
	if got := lastSeen(); !got.Equal(fiveAgo) {
		t.Fatalf("last_seen_at rewritten within 10 min: %v -> %v", fiveAgo, got)
	}
	elevenAgo := dbNow().Add(-11 * time.Minute)
	exec(t, db, "UPDATE `browser_session` SET `last_seen_at` = ? WHERE `key` = ?", elevenAgo, s.SessionKey)
	assert200(t, srv, "11 min", s.SessionSecret)
	if got := lastSeen(); time.Since(got) > 5*time.Second {
		t.Fatalf("last_seen_at not refreshed after 11 min: %v", got)
	}
}

// DB outage: 503, never 401.
func TestDatabaseClosedIs503(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	s := signIn(t, db, srv, seedPerson(t, db, "Ana"))

	closed, err := sql.Open("mysql", os.Getenv(dsnEnv))
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	down, _ := newServer(t, closed)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, PathSession, ""},
		{http.MethodDelete, PathSession, ""},
		{http.MethodGet, PathSessions, ""},
		{http.MethodDelete, PathSessions, ""},
		{http.MethodDelete, "/v1/browser/sessions/BS-0000000000", ""},
		{http.MethodGet, PathTeams, ""},
	} {
		r := call(t, c.method, down.URL+c.path, s.SessionSecret, c.body)
		if r.code != http.StatusServiceUnavailable || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("DB closed: %s %s = %d %s; want 503", c.method, c.path, r.code, r.body)
		}
	}
	if r := exchange(t, down, mint(t, db, seedPerson(t, db, "Beto"), "/").Secret); r.code != http.StatusServiceUnavailable {
		t.Errorf("DB closed: exchange = %d; want 503", r.code)
	}
	if _, err := ValidateSession(context.Background(), closed, s.SessionSecret); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("ValidateSession on a closed DB = %v; want ErrUnavailable", err)
	}
	assert200(t, srv, "same cookie, DB back", s.SessionSecret)
}

// ── sign out and revocation ─────────────────────────────────────────────────

func endReason(t *testing.T, db *sql.DB, key string) enums.BrowserSessionEndReason {
	t.Helper()
	var r sql.NullInt64
	if err := db.QueryRow("SELECT `end_reason` FROM `browser_session` WHERE `key` = ?", key).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return enums.BrowserSessionEndReason(r.Int64)
}

func TestSignOutEverywhereAndRevokeByKey(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana, beto := seedPerson(t, db, "Ana"), seedPerson(t, db, "Beto")
	a1, a2, a3 := signIn(t, db, srv, ana), signIn(t, db, srv, ana), signIn(t, db, srv, ana)
	b1 := signIn(t, db, srv, beto)

	// Sign out (this browser).
	if r := call(t, http.MethodDelete, srv.URL+PathSession, a1.SessionSecret, ""); r.code != http.StatusOK || r.body != `{"revoked":1}`+"\n" {
		t.Fatalf("sign out = %d %s", r.code, r.body)
	}
	assert401(t, srv, "after sign out", a1.SessionSecret)
	if endReason(t, db, a1.SessionKey) != enums.BROWSER_SESSION_END_REASON_SIGNED_OUT {
		t.Error("sign out end_reason")
	}
	if r := call(t, http.MethodDelete, srv.URL+PathSession, a1.SessionSecret, ""); r.code != http.StatusUnauthorized {
		t.Errorf("second sign out = %d", r.code)
	}

	// Revoke another account's key: the same 404 as a key that does not exist.
	other := call(t, http.MethodDelete, srv.URL+"/v1/browser/sessions/"+b1.SessionKey, a2.SessionSecret, "")
	none := call(t, http.MethodDelete, srv.URL+"/v1/browser/sessions/BS-NOSUCHKEY0", a2.SessionSecret, "")
	if other.code != http.StatusNotFound || other.body != none.body || none.code != http.StatusNotFound {
		t.Errorf("revoke another account's key = %d %q; unknown key = %d %q", other.code, other.body, none.code, none.body)
	}
	assert200(t, srv, "Beto untouched", b1.SessionSecret)
	if n, err := Revoke(context.Background(), db, ana.accountUUID(), b1.SessionKey, enums.BROWSER_SESSION_END_REASON_REVOKED_BY_AGENT); !errors.Is(err, ErrSessionNotFound) || n != 0 {
		t.Errorf("package Revoke of another account's key = %d, %v", n, err)
	}

	// Revoke one of my own other browsers.
	if r := call(t, http.MethodDelete, srv.URL+"/v1/browser/sessions/"+strings.ToLower(a3.SessionKey), a2.SessionSecret, ""); r.code != http.StatusOK || r.body != `{"revoked":1}`+"\n" {
		t.Fatalf("revoke own key = %d %s", r.code, r.body)
	}
	assert401(t, srv, "revoked by key", a3.SessionSecret)
	assert200(t, srv, "the revoking browser", a2.SessionSecret)
	if endReason(t, db, a3.SessionKey) != enums.BROWSER_SESSION_END_REASON_REVOKED {
		t.Error("revoke end_reason")
	}
	if n, err := Revoke(context.Background(), db, ana.accountUUID(), a3.SessionKey, enums.BROWSER_SESSION_END_REASON_REVOKED); err != nil || n != 0 {
		t.Errorf("revoking an already ended own key = %d, %v; want 0, nil", n, err)
	}

	// Sign out everywhere: every live session of Ana's, Beto's untouched.
	a4 := signIn(t, db, srv, ana)
	r := call(t, http.MethodDelete, srv.URL+PathSessions, a2.SessionSecret, "")
	if r.code != http.StatusOK || r.body != `{"revoked":2}`+"\n" {
		t.Fatalf("sign out everywhere = %d %s", r.code, r.body)
	}
	assert401(t, srv, "everywhere a2", a2.SessionSecret)
	assert401(t, srv, "everywhere a4", a4.SessionSecret)
	assert200(t, srv, "Beto after Ana signs out everywhere", b1.SessionSecret)
	if endReason(t, db, a4.SessionKey) != enums.BROWSER_SESSION_END_REASON_SIGNED_OUT_EVERYWHERE || endReason(t, db, a1.SessionKey) != enums.BROWSER_SESSION_END_REASON_SIGNED_OUT {
		t.Error("sign out everywhere end_reason (or it overwrote an earlier one)")
	}

	// The agent path (sign_out_browsers): all of Beto's.
	b2 := signIn(t, db, srv, beto)
	if n, err := Revoke(context.Background(), db, beto.accountUUID(), "", enums.BROWSER_SESSION_END_REASON_REVOKED_BY_AGENT); err != nil || n != 2 {
		t.Fatalf("Revoke all by agent = %d, %v", n, err)
	}
	assert401(t, srv, "revoked by agent", b2.SessionSecret)
	if endReason(t, db, b2.SessionKey) != enums.BROWSER_SESSION_END_REASON_REVOKED_BY_AGENT {
		t.Error("revoked by agent end_reason")
	}
}

func TestListSessionsAndTeams(t *testing.T) {
	db := testDB(t)
	srv, _ := newServer(t, db)
	ana, beto := seedPerson(t, db, "Ana"), seedPerson(t, db, "Beto")
	old := signIn(t, db, srv, ana)
	cur := signIn(t, db, srv, ana)
	bs := signIn(t, db, srv, beto)
	exec(t, db, "UPDATE `browser_session` SET `created_at` = ? WHERE `key` = ?", dbNow().Add(-time.Hour), old.SessionKey)
	if _, err := Revoke(context.Background(), db, ana.accountUUID(), old.SessionKey, enums.BROWSER_SESSION_END_REASON_REVOKED); err != nil {
		t.Fatal(err)
	}

	r := call(t, http.MethodGet, srv.URL+PathSessions, cur.SessionSecret, "")
	if r.code != http.StatusOK {
		t.Fatalf("list = %d %s", r.code, r.body)
	}
	for _, secret := range []string{cur.SessionSecret, old.SessionSecret, Hash(cur.SessionSecret), Hash(old.SessionSecret), bs.SessionKey, "secret_hash"} {
		if strings.Contains(r.body, secret) {
			t.Errorf("session list contains %q", secret[:min(len(secret), 12)])
		}
	}
	var list struct{ Sessions []SessionInfo }
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 2 || list.Sessions[0].Key != cur.SessionKey || !list.Sessions[0].Current || list.Sessions[0].State != "live" ||
		list.Sessions[0].UserAgent != "Mozilla/5.0 (test)" || list.Sessions[0].IPHint != "203.0.113.0/24" || list.Sessions[0].AuthMethod != "terminal_link" ||
		list.Sessions[1].Key != old.SessionKey || list.Sessions[1].Current || list.Sessions[1].State != "ended" || list.Sessions[1].EndReason != "revoked" ||
		list.Sessions[1].RevokedAt == nil {
		t.Fatalf("list = %s", r.body)
	}

	// Teams: a live membership (private), a public one, a revoked one, and a
	// team Ana is not on.
	team := func(name string, vis enums.TeamVisibility) string {
		id := newID(t)
		exec(t, db, "INSERT INTO `team` (`id`,`name`,`slug`,`sequence`,`board_revision`,`status`,`visibility`) VALUES (?,?,?,?,?,?,?)",
			id, name, strings.ToLower(name)+"-"+id[:6], 0, 0, 1, vis.ToInt64())
		t.Cleanup(func() { exec(t, db, "DELETE FROM `team` WHERE `id` = ?", id) })
		return id
	}
	member := func(p person, teamID string, role enums.MemberRole, revoked bool) {
		id := newID(t)
		exec(t, db, "INSERT INTO `member` (`id`,`account_uuid`,`team_uuid`,`key`,`display_name`,`role`,`status`) VALUES (?,?,?,?,?,?,?)",
			id, p.account, teamID, "M-"+id[:4], p.name, role.ToInt64(), 1)
		if revoked {
			exec(t, db, "UPDATE `member` SET `revoked_at` = ? WHERE `id` = ?", dbNow(), id)
		}
	}
	tPriv, tPub, tGone, tOther := team("Alpha", enums.TEAM_VISIBILITY_PRIVATE), team("Bravo", enums.TEAM_VISIBILITY_PUBLIC), team("Charlie", enums.TEAM_VISIBILITY_PRIVATE), team("Delta", enums.TEAM_VISIBILITY_PRIVATE)
	member(ana, tPriv, enums.MEMBER_ROLE_OWNER, false)
	member(ana, tPub, enums.MEMBER_ROLE_MEMBER, false)
	member(ana, tGone, enums.MEMBER_ROLE_MEMBER, true)
	member(beto, tOther, enums.MEMBER_ROLE_OWNER, false)

	tr := call(t, http.MethodGet, srv.URL+PathTeams, cur.SessionSecret, "")
	var teams struct{ Teams []TeamSummary }
	if err := json.Unmarshal([]byte(tr.body), &teams); err != nil || tr.code != http.StatusOK {
		t.Fatalf("teams = %d %s", tr.code, tr.body)
	}
	if len(teams.Teams) != 2 || teams.Teams[0].Name != "Alpha" || teams.Teams[0].Visibility != "private" || teams.Teams[0].Role != "owner" ||
		teams.Teams[1].Name != "Bravo" || teams.Teams[1].Visibility != "public" || teams.Teams[1].Role != "member" {
		t.Fatalf("teams = %s", tr.body)
	}
	if strings.Contains(tr.body, "Charlie") || strings.Contains(tr.body, "Delta") {
		t.Fatal("teams lists a revoked membership or another account's team")
	}
}
