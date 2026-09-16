package webapi

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/authz"
	"github.com/mklfarha/metiche/backend/app/browser"
	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/app/stream"
	"github.com/mklfarha/metiche/backend/enums"
)

// The read API is SQL over the generated schema, so every one of these tests
// needs a real MySQL holding create.sql. Point METICHE_TEST_MYSQL_DSN at one:
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&interpolateParams=true' go test ./app/webapi/
//
// interpolateParams=true is not decoration: it is what production runs
// (config/base.yaml recommends it), and it changes how []byte arguments reach
// MySQL. Without it, a []byte bound to a JSON column works; with it, the
// driver sends a binary literal and MySQL rejects it with error 3144. Leaving
// it off here once hid exactly that bug until a real deployment found it.
//
// Without it every test here SKIPS with that reason rather than being deleted:
// there is nothing in this package that can be meaningfully faked, because
// what is being tested IS the SQL and the isolation level it runs under.
const dsnEnv = "METICHE_TEST_MYSQL_DSN"

// Sentinels for the two secrets the schema holds. Every response body is
// checked against them; neither is ever a real credential.
const (
	fakeJoinCode  = "JOINCODESECRET"
	fakeTokenHash = "TOKENHASHSECRET"
)

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

func newUUID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
}

func mustExec(t *testing.T, db execer, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// seeded is the fixture: one team with two people, three sessions, live
// intents, one held claim and one expired one, an open conflict and a closed
// one, a mismatching contract and an unclaimed one, a decision, and an event
// log.
type seeded struct {
	teamID string
	slug   string

	// memberToken authenticates Ana, who IS a member of this team.
	// outsiderToken authenticates a perfectly valid account that is NOT.
	// Both are minted from crypto/rand at run time by the real MintToken, so
	// no credential is ever written down in this repository, and the pair is
	// what separates "a bad token" from "a good token for the wrong person" —
	// the case a naive membership check gets wrong.
	memberToken     string
	memberTokenHash string
	outsiderToken   string

	// Browser sessions (docs/BOARD_LOGIN.md §4.1), minted at run time by
	// browser.MintSessionSecret and stored as their hash only:
	// ownerSession is Ana (OWNER), memberSession is Beto (MEMBER), and
	// outsiderSession is a valid session for the account that is a member of
	// nothing.
	ownerSession    string
	memberSession   string
	outsiderSession string
}

// seedBoard seeds the fixture as a PUBLIC team.
//
// Public because everything below it is about the SQL — the joins, the
// isolation level, the lazy expiry filter — and a gate returning 404 would
// just get in the way of testing any of that. The gate is not untested as a
// result: it has its own cases at the bottom of this file, on a private team
// seeded by seedBoardVisibility, plus the decision's own unit tests in
// app/authz.
func seedBoard(t *testing.T, db *sql.DB) seeded {
	t.Helper()
	return seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PUBLIC)
}

func seedBoardVisibility(t *testing.T, db *sql.DB, visibility enums.TeamVisibility) seeded {
	t.Helper()

	teamID := newUUID(t)
	slug := "board-" + teamID[:8]
	mustExec(t, db, "INSERT INTO `team` (`id`,`name`,`slug`,`sequence`,`board_revision`,`status`,`visibility`) VALUES (?,?,?,?,?,?,?)",
		teamID, "Board Demo", slug, 42, 7, 1, visibility.ToInt64())
	// The join code moved off `team` and onto `invite` in v3. Seed one anyway:
	// assertNoSecrets below is only worth running if the secret it looks for is
	// actually in the database, one join away from what these endpoints read.
	// Deleting the secret to make the test compile would have deleted the test.
	inviteID := newUUID(t)
	mustExec(t, db, "INSERT INTO `invite` (`id`,`team_uuid`,`code`,`uses`,`status`) VALUES (?,?,?,?,?)",
		inviteID, teamID, fakeJoinCode, 0, 1)
	t.Cleanup(func() { mustExec(t, db, "DELETE FROM `team` WHERE `id` = ?", teamID) })

	// v3: the token hash lives on `account`, not `agent`, and `member` is the
	// join between an account and a team. fakeTokenHash stays on the account
	// so assertNoSecrets still has a real secret to hunt for.
	// Ana gets a REAL minted token, because the authorization tests need one
	// that the production lookup path will actually resolve. Beto keeps
	// fakeTokenHash so assertNoSecrets still has a genuine secret to hunt for,
	// sitting on a row one join away from everything these endpoints read.
	memberToken, memberHash, err := metichemcp.MintToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	outsiderToken, outsiderHash, err := metichemcp.MintToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	acctA, acctB := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
		acctA, "acct-"+acctA[:8], "Ana", memberHash, 1, 1)
	mustExec(t, db, "INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
		acctB, "acct-"+acctB[:8], "Beto", fakeTokenHash, 1, 1)

	// A third account that exists, is active, and holds a token this server
	// will happily resolve — and is a member of NOTHING. "Authenticated" is
	// not "authorized", and this row is what proves the gate knows that.
	acctOutsider := newUUID(t)
	mustExec(t, db, "INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
		acctOutsider, "acct-"+acctOutsider[:8], "Outsider", outsiderHash, 1, 1)
	t.Cleanup(func() {
		mustExec(t, db, "DELETE FROM `account` WHERE `id` IN (?,?,?)", acctA, acctB, acctOutsider)
	})

	ana, beto := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `member` (`id`,`account_uuid`,`team_uuid`,`key`,`display_name`,`role`,`status`) VALUES (?,?,?,?,?,?,?)",
		ana, acctA, teamID, "M-1", "Ana", 1, 1)
	mustExec(t, db, "INSERT INTO `member` (`id`,`account_uuid`,`team_uuid`,`key`,`display_name`,`role`,`status`) VALUES (?,?,?,?,?,?,?)",
		beto, acctB, teamID, "M-2", "Beto", 2, 1)

	agentA, agentB := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_kind`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?)",
		agentA, acctA, "A-1", "claude-1", "claude-code", "client-a", 1)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_kind`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?)",
		agentB, acctB, "A-2", "claude-2", "claude-code", "client-b", 1)

	// A third agent, for the outsider, so each session names the ACTIVE agent
	// it was created from, as a TERMINAL_LINK session does.
	agentOutsider := newUUID(t)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_kind`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?)",
		agentOutsider, acctOutsider, "A-3", "claude-3", "claude-code", "client-o", 1)
	ownerSession := seedBrowserSession(t, db, acctA, agentA)
	memberSession := seedBrowserSession(t, db, acctB, agentB)
	outsiderSession := seedBrowserSession(t, db, acctOutsider, agentOutsider)

	project := newUUID(t)
	mustExec(t, db, "INSERT INTO `project` (`id`,`team_uuid`,`key`,`name`,`status`) VALUES (?,?,?,?,?)",
		project, teamID, "api", "API", 1)

	s1, s2, s3 := newUUID(t), newUUID(t), newUUID(t)
	// live, stale, ended. The ended one must not appear on the board.
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`branch`,`goal`,`status`,`status_line`,`started_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,NOW(),NOW())",
		s1, teamID, project, agentA, ana, "S-1", "feat/auth", "ship login", 1, "writing the login handler")
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`branch`,`status`,`status_line`,`started_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,NOW(),NOW())",
		s2, teamID, project, agentB, beto, "S-2", "feat/ui", 2, "building the login form")
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`status`,`outcome`,`ended_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,NOW())",
		s3, teamID, project, agentA, ana, "S-3", 3, 1)

	i1, i2, i3 := newUUID(t), newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,`external_ref`,`revision`,`declared_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,NOW())",
		i1, teamID, project, s1, ana, "INT-1", "add POST /api/login", 1, 1, "GH-12", 2)
	mustExec(t, db, "INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,`revision`,`declared_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,NOW())",
		i2, teamID, project, s2, beto, "INT-2", "build the login form", 1, 2, 1)
	// On the ended session: must not show up on the board.
	mustExec(t, db, "INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,`revision`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		i3, teamID, project, s3, ana, "INT-3", "old work", 1, 3, 1)
	mustExec(t, db, "UPDATE `session` SET `current_intent_uuid` = ? WHERE `id` = ?", i1, s1)

	// One live claim and one that has expired. The expired one must not be
	// drawn as held: the lazy filter is the authority, not the status column.
	c1, c2 := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `claim` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`intent_uuid`,`key`,`mode`,`status`,`expires_at`,`hard_expires_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,DATE_ADD(NOW(), INTERVAL 1 HOUR),DATE_ADD(NOW(), INTERVAL 4 HOUR))",
		c1, teamID, project, s1, ana, i1, "CL-1", 2, 1)
	mustExec(t, db, "INSERT INTO `claim` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`mode`,`status`,`expires_at`,`hard_expires_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,DATE_SUB(NOW(), INTERVAL 1 HOUR),DATE_ADD(NOW(), INTERVAL 1 HOUR))",
		c2, teamID, project, s2, beto, "CL-2", 2, 1)
	for _, p := range []string{"app/auth.go", "app/session/**"} {
		mustExec(t, db, "INSERT INTO `claim_path` (`id`,`claim_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`mode`,`status`,`expires_at`,`pattern`,`pattern_norm`,`kind`,`prefix`,`depth`) "+
			"VALUES (?,?,?,?,?,?,?,DATE_ADD(NOW(), INTERVAL 1 HOUR),?,?,?,?,?)",
			newUUID(t), c1, project, s1, ana, 2, 1, p, p, 1, "app/", 2)
	}
	mustExec(t, db, "INSERT INTO `claim_path` (`id`,`claim_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`mode`,`status`,`expires_at`,`pattern`,`pattern_norm`,`kind`,`prefix`,`depth`) "+
		"VALUES (?,?,?,?,?,?,?,DATE_SUB(NOW(), INTERVAL 1 HOUR),?,?,?,?,?)",
		newUUID(t), c2, project, s2, beto, 2, 1, "web/login.tsx", "web/login.tsx", 1, "web/", 2)

	// An open conflict with two participants, and a resolved one.
	cf1, cf2 := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,`detector_rule`,`suggested_action`,`occurrence_count`,`first_detected_at`,`last_detected_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,NOW(),NOW())",
		cf1, teamID, project, "CF-1", 1, "dedupe-"+cf1[:8], 3, 1, 1, "path_overlap",
		"Ana holds app/auth.go (write, 4m, feat/auth); consume her POST /api/login contract instead", 2)
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,`resolution`,`resolved_at`,`occurrence_count`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,NOW(),?)",
		cf2, teamID, project, "CF-2", 2, "dedupe-"+cf2[:8], 1, 4, 1, 4, 1)
	mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
		newUUID(t), cf1, teamID, s1, agentA, ana, 3, c1, 2)
	mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
		newUUID(t), cf1, teamID, s2, agentB, beto, 3, c2, 1)

	// Two contracts: one with a produces and a consumes that disagree, one
	// consumed with nobody producing it.
	ct1, ct2 := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `contract` (`id`,`team_uuid`,`project_uuid`,`key`,`key_norm`,`kind`,`status`,`title`) VALUES (?,?,?,?,?,?,?,?)",
		ct1, teamID, project, "POST /api/login", "post:/api/login", 1, 2, "Login")
	mustExec(t, db, "INSERT INTO `contract` (`id`,`team_uuid`,`project_uuid`,`key`,`key_norm`,`kind`,`status`,`title`) VALUES (?,?,?,?,?,?,?,?)",
		ct2, teamID, project, "GET /api/me", "get:/api/me", 1, 2, "Me")

	a1, a2, a3 := newUUID(t), newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `contract_assertion` (`id`,`contract_uuid`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`role`,`shape_hash`,`shape_fingerprint`,`status`,`revision`,`active_marker`,`asserted_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,NOW())",
		a1, ct1, teamID, project, s1, ana, 1, "hash-produces", "fp-a", 1, 1, 1)
	mustExec(t, db, "INSERT INTO `contract_assertion` (`id`,`contract_uuid`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`role`,`shape_hash`,`shape_fingerprint`,`status`,`revision`,`active_marker`,`asserted_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,NOW())",
		a2, ct1, teamID, project, s2, beto, 2, "hash-consumes", "fp-b", 1, 1, 1)
	mustExec(t, db, "INSERT INTO `contract_assertion` (`id`,`contract_uuid`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`role`,`shape_hash`,`shape_fingerprint`,`status`,`revision`,`active_marker`,`asserted_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,NOW())",
		a3, ct2, teamID, project, s2, beto, 2, "hash-me", "fp-c", 1, 1, 1)
	for _, f := range []string{"token", "expires_at"} {
		mustExec(t, db, "INSERT INTO `contract_field` (`id`,`assertion_uuid`,`contract_uuid`,`path`,`path_snake`,`type`,`required`,`direction`) VALUES (?,?,?,?,?,?,?,?)",
			newUUID(t), a1, ct1, f, f, 1, 1, 2)
	}

	d1 := newUUID(t)
	mustExec(t, db, "INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`status`,`always_show`,`revision`,`decided_by_member_uuid`,`decided_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,NOW())",
		d1, teamID, project, "#auth-jwt-cookie", "Auth is a cookie",
		"auth is a JWT in an httpOnly cookie, never localStorage", 2, 1, 1, ana)
	mustExec(t, db, "INSERT INTO `decision_path` (`id`,`decision_uuid`,`team_uuid`,`project_uuid`,`pattern`,`pattern_norm`,`kind`,`prefix`,`depth`) VALUES (?,?,?,?,?,?,?,?,?)",
		newUUID(t), d1, teamID, project, "app/auth/**", "app/auth/**", 3, "app/auth/", 2)

	// The event log. Three of the five belong to S-1.
	events := []struct {
		seq        int64
		session    any
		kind       int64
		structural int
		subjectKey string
		summary    string
	}{
		{38, nil, 1, 1, "M-1", "Ana joined"},
		{39, s1, 3, 1, "S-1", "S-1 started"},
		{40, s1, 6, 0, "INT-1", "INT-1 declared"},
		{41, s2, 6, 0, "INT-2", "INT-2 declared"},
		{42, s1, 17, 0, "CF-1", "CF-1 raised"},
	}
	for _, e := range events {
		mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`project_uuid`,`session_uuid`,`kind`,`structural`,`subject_key`,`summary`,`idempotency_key`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?)",
			newUUID(t), teamID, e.seq, project, e.session, e.kind, e.structural, e.subjectKey, e.summary,
			"seed-"+teamID[:8]+"-"+e.summary)
	}

	return seeded{
		teamID:          teamID,
		slug:            slug,
		memberToken:     memberToken,
		memberTokenHash: memberHash,
		outsiderToken:   outsiderToken,
		ownerSession:    ownerSession,
		memberSession:   memberSession,
		outsiderSession: outsiderSession,
	}
}

// seedBrowserSession inserts a live browser_session for an account and returns
// its secret. Only the hash is stored, exactly as the exchange stores it.
func seedBrowserSession(t *testing.T, db *sql.DB, account, agent string) string {
	t.Helper()
	secret, hash, err := browser.MintSessionSecret()
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	mustExec(t, db, "INSERT INTO `browser_session` (`id`,`key`,`account_uuid`,`secret_hash`,`auth_method`,`created_from_agent_uuid`,`expires_at`,`last_seen_at`) "+
		"VALUES (?,?,?,?,?,?,DATE_ADD(UTC_TIMESTAMP(), INTERVAL 30 DAY),UTC_TIMESTAMP())",
		newUUID(t), "BS-"+strings.ToUpper(hash[:10]), account, hash, enums.BROWSER_AUTH_METHOD_TERMINAL_LINK, agent)
	return secret
}

func newBoardServer(t *testing.T, db *sql.DB) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	NewAPI(db, nil).RegisterOn(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string, into any) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status %d, body %s", url, resp.StatusCode, body)
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("decode %s: %v (body %s)", url, err, body)
		}
	}
	return string(body)
}

// TestSnapshotIsOneCoherentRead covers the board's first paint end to end.
func TestSnapshotIsOneCoherentRead(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	srv := newBoardServer(t, db)

	var out snapshotResponse
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug, &out)

	if out.Sequence != 42 || out.BoardRevision != 7 {
		t.Fatalf("cursors = %d/%d, want 42/7", out.Sequence, out.BoardRevision)
	}
	if out.Team.Key != fx.slug || out.Team.Name != "Board Demo" {
		t.Fatalf("team = %+v", out.Team)
	}

	// Live and stale, never ended.
	if len(out.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 (live + stale, not the ended one)", len(out.Sessions))
	}
	byKey := map[string]sessionWire{}
	for _, s := range out.Sessions {
		byKey[s.Key] = s
	}
	s1, ok := byKey["S-1"]
	if !ok {
		t.Fatalf("S-1 missing: %+v", out.Sessions)
	}
	if s1.Status != "live" || s1.MemberName != "Ana" || s1.AgentLabel != "claude-1" ||
		s1.Branch != "feat/auth" || s1.StatusLine != "writing the login handler" ||
		s1.ProjectKey != "api" || s1.CurrentIntentKey != "INT-1" {
		t.Fatalf("S-1 = %+v", s1)
	}
	if len(s1.Intents) != 1 || s1.Intents[0].Key != "INT-1" ||
		s1.Intents[0].Status != "declared" || s1.Intents[0].ExternalRef != "GH-12" ||
		s1.Intents[0].Revision != 2 {
		t.Fatalf("S-1 intents = %+v", s1.Intents)
	}
	if len(s1.Claims) != 1 || s1.Claims[0].Key != "CL-1" || s1.Claims[0].Mode != "write" ||
		len(s1.Claims[0].Paths) != 2 {
		t.Fatalf("S-1 claims = %+v", s1.Claims)
	}

	s2 := byKey["S-2"]
	if s2.Status != "stale" {
		t.Fatalf("S-2 status = %q, want stale", s2.Status)
	}
	// Its claim has expired, and the lazy filter — not the status column — is
	// what decides that.
	if len(s2.Claims) != 0 {
		t.Fatalf("S-2 should hold nothing; an expired claim was drawn as held: %+v", s2.Claims)
	}

	if len(out.Conflicts) != 1 || out.Conflicts[0].Key != "CF-1" {
		t.Fatalf("conflicts = %+v, want just the open CF-1", out.Conflicts)
	}
	cf := out.Conflicts[0]
	if cf.Severity != "high" || cf.Kind != "path_overlap" || cf.Status != "open" ||
		cf.OccurrenceCount != 2 || cf.SuggestedAction == "" {
		t.Fatalf("CF-1 = %+v", cf)
	}
	if len(cf.Participants) != 2 {
		t.Fatalf("CF-1 participants = %+v, want 2", cf.Participants)
	}

	if out.Counts.LiveSessions != 2 || out.Counts.OpenConflicts != 1 || out.Counts.HeldClaims != 1 {
		t.Fatalf("counts = %+v", out.Counts)
	}

	assertNoSecrets(t, raw)
}

// TestSnapshotDoesNotSeeAPartiallyAppliedTransaction is the isolation proof.
//
// It opens the read transaction the endpoints use, takes its first read, then
// commits a write from a DIFFERENT connection — the write path bumping the
// sequence and appending an event, the same pair of statements a real tool
// call makes under the team lock. The read transaction must continue to see
// the world as it was: same sequence, same event count.
//
// Under READ COMMITTED the second read would pick up the committed change and
// the response would mix two instants — a cursor from before the write beside
// rows from after it, which is exactly the torn board this promises not to
// serve.
func TestSnapshotDoesNotSeeAPartiallyAppliedTransaction(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	ctx := context.Background()
	a := NewAPI(db, nil)

	tx, err := a.beginRead(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The first read establishes the snapshot.
	before, err := resolveTeam(ctx, tx, fx.slug)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	beforeEvents := countEvents(t, tx, fx.teamID)

	// Another connection commits a write while the read transaction is open.
	wtx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin write: %v", err)
	}
	if _, err := wtx.Exec("UPDATE `team` SET `sequence` = `sequence` + 1, `board_revision` = `board_revision` + 1 WHERE `id` = ?", fx.teamID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := wtx.Exec("INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`kind`,`structural`,`summary`,`idempotency_key`) VALUES (?,?,?,?,?,?,?)",
		newUUID(t), fx.teamID, 43, 4, 1, "S-1 ended", "iso-"+fx.teamID[:8]); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := wtx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The read transaction is still on its snapshot.
	after, err := resolveTeam(ctx, tx, fx.slug)
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if after.Sequence != before.Sequence || after.BoardRevision != before.BoardRevision {
		t.Fatalf("the read transaction saw a concurrent commit: %d/%d became %d/%d",
			before.Sequence, before.BoardRevision, after.Sequence, after.BoardRevision)
	}
	if now := countEvents(t, tx, fx.teamID); now != beforeEvents {
		t.Fatalf("the read transaction saw %d events, then %d — the snapshot is not stable",
			beforeEvents, now)
	}

	// And once the snapshot is released, the new state is of course visible.
	_ = tx.Rollback()
	fresh, err := a.beginRead(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = fresh.Rollback() }()
	next, err := resolveTeam(ctx, fresh, fx.slug)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if next.Sequence != before.Sequence+1 {
		t.Fatalf("a new read did not see the commit: sequence = %d, want %d",
			next.Sequence, before.Sequence+1)
	}
}

// TestReadCommittedWouldSeeTheConcurrentCommit is the counterfactual that
// makes the test above mean something.
//
// It runs the identical scenario at READ COMMITTED and asserts the read DOES
// tear — a second read inside the same transaction picking up a commit that
// landed between the two. That is the behaviour beginRead exists to avoid, and
// pinning it here means changing the isolation level breaks a test instead of
// quietly making every board read racy.
func TestReadCommittedWouldSeeTheConcurrentCommit(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	before, err := resolveTeam(ctx, tx, fx.slug)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, err := db.Exec("UPDATE `team` SET `sequence` = `sequence` + 1 WHERE `id` = ?", fx.teamID); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, err := resolveTeam(ctx, tx, fx.slug)
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if after.Sequence == before.Sequence {
		t.Fatal("READ COMMITTED did not tear, so the REPEATABLE READ test above proves nothing on this server")
	}
}

func countEvents(t *testing.T, tx *sql.Tx, teamID string) int64 {
	t.Helper()
	var n int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", teamID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestConflictsEndpoint(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	srv := newBoardServer(t, db)

	var open conflictsResponse
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts", &open)
	if open.Sequence != 42 || open.BoardRevision != 7 {
		t.Fatalf("cursors = %d/%d", open.Sequence, open.BoardRevision)
	}
	if len(open.Conflicts) != 1 || open.Conflicts[0].Key != "CF-1" {
		t.Fatalf("default status should be open: %+v", open.Conflicts)
	}
	assertNoSecrets(t, raw)

	var all conflictsResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts?status=all", &all)
	if len(all.Conflicts) != 2 {
		t.Fatalf("status=all should include the resolved one: %+v", all.Conflicts)
	}
	// Severity first, so the critical/high one is never buried.
	if all.Conflicts[0].Key != "CF-1" {
		t.Fatalf("conflicts are not ordered by severity: %+v", all.Conflicts)
	}
	var resolved *conflictWire
	for i := range all.Conflicts {
		if all.Conflicts[i].Key == "CF-2" {
			resolved = &all.Conflicts[i]
		}
	}
	if resolved == nil || resolved.Status != "resolved" || resolved.Resolution != "converged" {
		t.Fatalf("CF-2 = %+v", resolved)
	}

	resp, err := http.Get(srv.URL + "/v1/teams/" + fx.slug + "/conflicts?status=banana")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad status filter: got %d, want 400", resp.StatusCode)
	}
}

func TestContractsEndpointShowsTheBottleneck(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	srv := newBoardServer(t, db)

	var out contractsResponse
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/contracts", &out)
	if out.Sequence != 42 || out.BoardRevision != 7 {
		t.Fatalf("cursors = %d/%d", out.Sequence, out.BoardRevision)
	}
	if len(out.Contracts) != 2 {
		t.Fatalf("got %d contracts, want 2", len(out.Contracts))
	}

	byKey := map[string]contractWire{}
	for _, c := range out.Contracts {
		byKey[c.Key] = c
	}
	login := byKey["POST /api/login"]
	if login.Agreement != "mismatch" {
		t.Fatalf("a produces and a consumes with different shape hashes should be a mismatch: %+v", login)
	}
	if len(login.Produces) != 1 || login.Produces[0].SessionKey != "S-1" || login.Produces[0].FieldCount != 2 {
		t.Fatalf("produces = %+v", login.Produces)
	}
	if len(login.Consumes) != 1 || login.Consumes[0].SessionKey != "S-2" {
		t.Fatalf("consumes = %+v", login.Consumes)
	}

	me := byKey["GET /api/me"]
	if me.Agreement != "unclaimed" {
		t.Fatalf("a consumes with no producer is the unclaimed signal: %+v", me)
	}
	assertNoSecrets(t, raw)
}

func TestDecisionsEndpoint(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	srv := newBoardServer(t, db)

	var out decisionsResponse
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions", &out)
	if out.Sequence != 42 || out.BoardRevision != 7 {
		t.Fatalf("cursors = %d/%d", out.Sequence, out.BoardRevision)
	}
	if len(out.Decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(out.Decisions))
	}
	d := out.Decisions[0]
	if d.Key != "#auth-jwt-cookie" || !d.AlwaysShow || d.Status != "accepted" ||
		d.DecidedBy != "Ana" || len(d.Scope) != 1 || d.Scope[0] != "app/auth/**" {
		t.Fatalf("decision = %+v", d)
	}
	assertNoSecrets(t, raw)
}

// TestSessionHistoryIsTheEventLogFilteredBySession pins the model's claim that
// a run's history is not a separate structure.
func TestSessionHistoryIsTheEventLogFilteredBySession(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	srv := newBoardServer(t, db)

	var out sessionResponse
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/sessions/S-1", &out)
	if out.Sequence != 42 || out.BoardRevision != 7 {
		t.Fatalf("cursors = %d/%d", out.Sequence, out.BoardRevision)
	}
	if out.Session.Key != "S-1" || out.Session.MemberName != "Ana" || out.Session.Status != "live" {
		t.Fatalf("session = %+v", out.Session)
	}
	if len(out.Session.Intents) != 1 || len(out.Session.Claims) != 1 {
		t.Fatalf("session lost its intents/claims: %+v", out.Session)
	}

	wantSeqs := []int64{39, 40, 42}
	if len(out.Events) != len(wantSeqs) {
		t.Fatalf("got %d events, want %d: %+v", len(out.Events), len(wantSeqs), out.Events)
	}
	for i, want := range wantSeqs {
		if out.Events[i].Sequence != want {
			t.Fatalf("events = %+v, want sequences %v", out.Events, wantSeqs)
		}
	}
	if out.Events[0].Kind != "session_started" || !out.Events[0].Structural {
		t.Fatalf("first event = %+v", out.Events[0])
	}
	if out.Events[1].Structural {
		t.Fatalf("intent_declared should not be structural: %+v", out.Events[1])
	}
	assertNoSecrets(t, raw)

	// The cursor is the same unit as everywhere else, and exclusive.
	var page sessionResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/sessions/S-1?after=39", &page)
	if len(page.Events) != 2 || page.Events[0].Sequence != 40 {
		t.Fatalf("after=39 returned %+v", page.Events)
	}

	resp, err := http.Get(srv.URL + "/v1/teams/" + fx.slug + "/sessions/S-404")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: got %d, want 404", resp.StatusCode)
	}
}

func TestUnknownTeamIs404(t *testing.T) {
	db := testDB(t)
	srv := newBoardServer(t, db)

	for _, p := range []string{"", "/conflicts", "/contracts", "/decisions", "/sessions", "/sessions/S-1"} {
		resp, err := http.Get(srv.URL + "/v1/teams/no-such-team" + p)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code != http.StatusNotFound {
			t.Fatalf("%q: got %d, want 404", p, code)
		}
	}
}

// TestTeamResolvesByUUIDToo covers the consequence of shadowing the generated
// /v1/teams/{id}: addressing a team by its uuid must keep working.
func TestTeamResolvesByUUIDToo(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	srv := newBoardServer(t, db)

	var out snapshotResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.teamID, &out)
	if out.Team.Key != fx.slug {
		t.Fatalf("uuid lookup returned %+v", out.Team)
	}
}

// assertNoSecrets is run against every response body in this file. The join
// code and the token hash both live one join away from what these endpoints
// read, and "we were careful" is not a guarantee — this is.
func assertNoSecrets(t *testing.T, body string, extra ...string) {
	t.Helper()
	for _, secret := range append([]string{fakeJoinCode, fakeTokenHash}, extra...) {
		if secret == "" {
			t.Fatal("assertNoSecrets was handed an empty needle, so it is checking nothing")
		}
		if strings.Contains(body, secret) {
			t.Fatalf("a response carried a secret (%s): %s", secret, body)
		}
	}
}

// TestBoardAndStreamRoutesCoexist mounts BOTH halves of this work — the read
// API and the SSE stream — on one router underneath a stand-in for the
// generated /v1 mount, which is exactly how they will be wired in app/rest.go.
//
// chi enforces its mount rules with a PANIC raised while the router is being
// built, so a bad combination compiles, ships, and crash-loops the container
// on start. This is the test that catches that on a laptop instead.
func TestBoardAndStreamRoutesCoexist(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)

	r := chi.NewRouter()
	// Stand-in for the generated CRUD tree, mounted first.
	r.Route("/v1", func(v1 chi.Router) {
		v1.Route("/teams", func(teams chi.Router) {
			teams.Get("/", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("generated-list"))
			})
			teams.Route("/{id}", func(one chi.Router) {
				one.Get("/", func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte("generated-get"))
				})
			})
		})
	})

	NewAPI(db, nil).RegisterOn(r)
	hub := stream.NewHub(stream.NewDBSource(db), nil)
	defer hub.Close()
	stream.NewServer(hub, stream.NewDBTeamLookup(db), authz.NewGuard(db), nil).RegisterOn(r)

	srv := httptest.NewServer(r)
	defer func() {
		srv.CloseClientConnections()
		srv.Close()
	}()

	// The generated collection route is untouched.
	resp, err := http.Get(srv.URL + "/v1/teams/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "generated-list" {
		t.Fatalf("the generated list route was shadowed: %q", body)
	}

	// The board's snapshot resolves.
	var snap snapshotResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug, &snap)
	if snap.Team.Key != fx.slug {
		t.Fatalf("snapshot route did not resolve: %+v", snap.Team)
	}

	// And so does the stream, on the same router.
	sresp, err := http.Get(srv.URL + "/v1/teams/" + fx.slug + "/stream?after=41")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	ct := sresp.Header.Get("Content-Type")
	_ = sresp.Body.Close()
	if ct != "text/event-stream" {
		t.Fatalf("stream route did not resolve: Content-Type = %q", ct)
	}
}

// ─────────────────────────────────────────────
// Authorization: team.visibility, and membership
// ─────────────────────────────────────────────
//
// These are the end-to-end proofs for app/authz over a real database, and
// they cover BOTH halves of the board's surface — the JSON reads in this
// package and the SSE stream in app/stream — because they are mounted on the
// same public router, gated by the same decision, and a hole in either is the
// same hole.
//
// The rule being pinned:
//
//	public team   -> readable by anyone, no token.
//	private team  -> a bearer token whose account is a live member. Anything
//	                 else is 404, NEVER 403. A 403 would confirm the team
//	                 exists, which is exactly how the slug namespace leaks.

// newGuardedServer mounts both halves behind the real database-backed guard,
// the way app/rest.go does in production.
func newGuardedServer(t *testing.T, db *sql.DB) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	NewAPI(db, nil).RegisterOn(r)
	hub := stream.NewHub(stream.NewDBSource(db), nil)
	stream.NewServer(hub, stream.NewDBTeamLookup(db), authz.NewGuard(db), nil).RegisterOn(r)
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		// CloseClientConnections before Close: Close waits on outstanding
		// requests and an SSE handler is outstanding until its client hangs
		// up, so a failing test would hang the suite instead of reporting.
		srv.CloseClientConnections()
		srv.Close()
		hub.Close()
	})
	return srv
}

// boardPaths is every read this package and app/stream expose for a team.
// Each new case below walks the whole list: a gate that covers four routes
// out of five is not a gate.
func boardPaths() []string {
	return []string{"", "/conflicts", "/contracts", "/decisions", "/sessions", "/sessions/S-1",
		"/conflicts/history", "/decisions/history", "/events", "/graph", "/stream?after=41"}
}

// get issues a request, optionally bearing a token, and returns the status
// and the body. The token always rides in the Authorization HEADER — never in
// the query string, which is where it would end up in the access log, the
// Referer and the browser history.
func get(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// A stream that is allowed through never ends on its own, so every read
	// here is bounded and the body is closed immediately after.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// An allowed SSE response never ends on its own, so reading it to EOF
	// would just burn the client timeout. Take the first frame instead —
	// which also turns "the stream was allowed" into "the stream delivered",
	// a stronger thing to assert.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp.StatusCode, readOneSSEFrame(t, resp.Body, 5*time.Second)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body)
}

// TestPublicTeamIsReadableWithNoToken is the property the demo board depends
// on: visibility=public means public, on every route including the stream.
func TestPublicTeamIsReadableWithNoToken(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PUBLIC)
	srv := newGuardedServer(t, db)

	for _, p := range boardPaths() {
		url := srv.URL + "/v1/teams/" + fx.slug + p
		code, body := get(t, url, "")
		if code != http.StatusOK {
			t.Fatalf("%q on a PUBLIC team with no token: got %d, want 200 (body %s)", p, code, body)
		}
		if p == "/stream?after=41" && !strings.Contains(body, "id: 42") {
			t.Fatalf("the public stream did not deliver the event after the cursor; got:\n%s", body)
		}
		assertNoSecrets(t, body, fx.memberToken, fx.memberTokenHash, fx.outsiderToken)
	}
}

// TestPrivateTeamWithNoTokenIs404 is the hole this work closes. Before the
// guard existed, every one of these returned 200 and the whole board with it.
func TestPrivateTeamWithNoTokenIs404(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	for _, p := range boardPaths() {
		code, body := get(t, srv.URL+"/v1/teams/"+fx.slug+p, "")
		if code != http.StatusNotFound {
			t.Fatalf("%q on a PRIVATE team with no token: got %d, want 404 (body %s)", p, code, body)
		}
		// Nothing about the team may come back with the refusal.
		if strings.Contains(body, "Board Demo") || strings.Contains(body, "feat/auth") ||
			strings.Contains(body, "#auth-jwt-cookie") {
			t.Fatalf("%q leaked board content in its refusal: %s", p, body)
		}
	}
}

// TestPrivateTeamWithANonMemberTokenIs404 is the case a check that only asks
// "is this a valid token?" gets wrong. The token is real, the account is real
// and active, and it is a member of nothing.
func TestPrivateTeamWithANonMemberTokenIs404(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	// Sanity: the token really does resolve to an account, so a 404 below is
	// the MEMBERSHIP check talking and not a broken credential.
	if _, err := metichemcp.AccountByToken(context.Background(), db, fx.outsiderToken); err != nil {
		t.Fatalf("the outsider's token should resolve to an account: %v", err)
	}

	for _, p := range boardPaths() {
		code, body := get(t, srv.URL+"/v1/teams/"+fx.slug+p, fx.outsiderToken)
		if code != http.StatusNotFound {
			t.Fatalf("%q on a PRIVATE team with a NON-MEMBER token: got %d, want 404 (body %s)", p, code, body)
		}
	}
}

// TestPrivateTeamWithAMemberTokenIs200AndStreams is the other half: the gate
// has to let the right person through, and the stream has to actually stream.
func TestPrivateTeamWithAMemberTokenIs200AndStreams(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	for _, p := range []string{"", "/conflicts", "/contracts", "/decisions", "/sessions", "/sessions/S-1"} {
		code, body := get(t, srv.URL+"/v1/teams/"+fx.slug+p, fx.memberToken)
		if code != http.StatusOK {
			t.Fatalf("%q on a PRIVATE team with a MEMBER token: got %d, want 200 (body %s)", p, code, body)
		}
		assertNoSecrets(t, body, fx.memberToken, fx.memberTokenHash, fx.outsiderToken)
	}

	// The stream, for real: 200, the SSE content type, and a frame off the
	// wire. ?after=41 leaves exactly one seeded event (sequence 42) to deliver,
	// which is what makes "it streamed" observable rather than assumed.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/teams/"+fx.slug+"/stream?after=41", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.memberToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream with a member token: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream Content-Type = %q, want text/event-stream", ct)
	}

	frame := readOneSSEFrame(t, resp.Body, 5*time.Second)
	if !strings.Contains(frame, "id: 42") {
		t.Fatalf("the stream did not deliver the event after the cursor; got:\n%s", frame)
	}
	assertNoSecrets(t, frame, fx.memberToken, fx.memberTokenHash, fx.outsiderToken)
}

// TestPrivateTeamRefusalIsIndistinguishableFromAnUnknownTeam is the 404-not-403
// rule, stated as an assertion instead of as a comment.
//
// If the refusal for a team that EXISTS differed from the refusal for a slug
// that does not — in status, in Content-Type, or in a single byte of body —
// then that difference would be a 403 in all but name, and a script could walk
// the slug namespace with it.
func TestPrivateTeamRefusalIsIndistinguishableFromAnUnknownTeam(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	for _, p := range boardPaths() {
		realCode, realBody := get(t, srv.URL+"/v1/teams/"+fx.slug+p, "")
		fakeCode, fakeBody := get(t, srv.URL+"/v1/teams/no-such-team-at-all"+p, "")
		if realCode != fakeCode || realBody != fakeBody {
			t.Fatalf("%q distinguishes a private team from a missing one: %d %q vs %d %q",
				p, realCode, realBody, fakeCode, fakeBody)
		}
		// And with a valid token belonging to somebody else, too.
		outCode, outBody := get(t, srv.URL+"/v1/teams/"+fx.slug+p, fx.outsiderToken)
		if outCode != fakeCode || outBody != fakeBody {
			t.Fatalf("%q distinguishes \"not your team\" from \"no such team\": %d %q vs %d %q",
				p, outCode, outBody, fakeCode, fakeBody)
		}
	}
}

// TestRevokedMemberLosesTheBoard covers the soft-removal case: member rows are
// revoked, not deleted, so a check that only looked at status would keep
// letting an ejected teammate read everything.
func TestRevokedMemberLosesTheBoard(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	if code, _ := get(t, srv.URL+"/v1/teams/"+fx.slug, fx.memberToken); code != http.StatusOK {
		t.Fatalf("the member should start with access: got %d", code)
	}

	mustExec(t, db, "UPDATE `member` SET `revoked_at` = NOW() WHERE `team_uuid` = ? AND `key` = ?",
		fx.teamID, "M-1")

	for _, p := range boardPaths() {
		if code, body := get(t, srv.URL+"/v1/teams/"+fx.slug+p, fx.memberToken); code != http.StatusNotFound {
			t.Fatalf("%q after revocation: got %d, want 404 (body %s)", p, code, body)
		}
	}
}

// TestPrivateTeamByUUIDIsAlsoGated closes the obvious way around a slug gate.
//
// These routes shadow the generated /v1/teams/{id}, so both resolvers accept a
// uuid — and a guard that only understood slugs would be bypassed by anyone
// who had ever seen the team's id.
func TestPrivateTeamByUUIDIsAlsoGated(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	if code, body := get(t, srv.URL+"/v1/teams/"+fx.teamID, ""); code != http.StatusNotFound {
		t.Fatalf("uuid-addressed private team with no token: got %d, want 404 (body %s)", code, body)
	}
	if code, _ := get(t, srv.URL+"/v1/teams/"+fx.teamID, fx.memberToken); code != http.StatusOK {
		t.Fatalf("uuid-addressed private team with a member token: got %d, want 200", code)
	}
}

// readOneSSEFrame reads until the first blank-line-terminated frame, or the
// deadline. It exists because an allowed SSE response never ends on its own.
func readOneSSEFrame(t *testing.T, body io.Reader, within time.Duration) string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		var b strings.Builder
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			line := sc.Text()
			b.WriteString(line)
			b.WriteString("\n")
			if line == "" && strings.Contains(b.String(), "id: ") {
				break
			}
		}
		out <- b.String()
	}()
	select {
	case s := <-out:
		return s
	case <-time.After(within):
		t.Fatal("no SSE frame arrived before the deadline")
		return ""
	}
}

// TestTheLeakTestHasARealSecretToHuntFor keeps assertNoSecrets honest.
//
// A leak test is only worth the line it occupies if the string it looks for is
// genuinely in the database, on a row one join away from what these endpoints
// read. The easy way to make a leak test green is to stop seeding the secret,
// and that is indistinguishable from passing — so this asserts the needles are
// really there, and fails loudly if a future refactor of the fixture quietly
// removes them.
func TestTheLeakTestHasARealSecretToHuntFor(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)

	var code string
	if err := db.QueryRow("SELECT `code` FROM `invite` WHERE `team_uuid` = ?", fx.teamID).Scan(&code); err != nil {
		t.Fatalf("the fixture no longer seeds an invite code for assertNoSecrets to hunt: %v", err)
	}
	if code != fakeJoinCode {
		t.Fatalf("invite.code = %q, but assertNoSecrets hunts for %q", code, fakeJoinCode)
	}

	var n int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM `account` a JOIN `member` m ON m.`account_uuid` = a.`id` "+
			"WHERE m.`team_uuid` = ? AND a.`token_hash` = ?", fx.teamID, fakeTokenHash).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n == 0 {
		t.Fatalf("no member account carries %q, so assertNoSecrets is hunting for nothing", fakeTokenHash)
	}

	// And the minted credentials the authorization tests use are real rows
	// too — the same needles, passed to assertNoSecrets as extras.
	var hash string
	if err := db.QueryRow("SELECT `token_hash` FROM `account` a JOIN `member` m ON m.`account_uuid` = a.`id` "+
		"WHERE m.`team_uuid` = ? AND m.`key` = ?", fx.teamID, "M-1").Scan(&hash); err != nil {
		t.Fatalf("the member account is missing: %v", err)
	}
	if hash != fx.memberTokenHash || fx.memberToken == "" {
		t.Fatal("the member's minted token is not the one on the row the guard will read")
	}

	// Belt and braces: the needle finder actually finds.
	if !strings.Contains("prefix "+fakeJoinCode+" suffix", fakeJoinCode) {
		t.Fatal("unreachable")
	}
}

// ─────────────────────────────────────────────
// Browser sessions, /access, and the open stream (docs/BOARD_LOGIN.md §4)
// ─────────────────────────────────────────────

// response is one observed HTTP answer, everything a probe could compare.
type response struct {
	code        int
	contentType string
	body        string
}

// fetch issues a GET with the given headers and reads a bounded body (the
// first frame of an allowed stream).
func fetch(t *testing.T, url string, headers map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := response{code: resp.StatusCode, contentType: resp.Header.Get("Content-Type")}
	if strings.HasPrefix(out.contentType, "text/event-stream") {
		out.body = readOneSSEFrame(t, resp.Body, 5*time.Second)
		return out
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	out.body = string(b)
	return out
}

func session(secret string) map[string]string {
	return map[string]string{authz.HeaderBrowserSession: secret}
}

// gatedPaths is boardPaths plus the new /access endpoint.
func gatedPaths() []string { return append(boardPaths(), "/access") }

// TestPrivateTeamWithAMemberSessionIs200AndStreams: a live member's valid
// browser session reads every route of a private team, the stream included.
func TestPrivateTeamWithAMemberSessionIs200AndStreams(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	for _, secret := range []string{fx.ownerSession, fx.memberSession} {
		for _, p := range gatedPaths() {
			got := fetch(t, srv.URL+"/v1/teams/"+fx.slug+p, session(secret))
			if got.code != http.StatusOK {
				t.Fatalf("%q on a PRIVATE team with a MEMBER's session: got %d, want 200 (body %s)", p, got.code, got.body)
			}
			if p == "/stream?after=41" && !strings.Contains(got.body, "id: 42") {
				t.Fatalf("the member's session stream did not deliver; got:\n%s", got.body)
			}
			assertNoSecrets(t, got.body, fx.ownerSession, fx.memberSession, fx.outsiderSession,
				browser.Hash(fx.ownerSession), fx.memberToken)
		}
	}
}

// TestPrivateTeamSessionRefusalsAreByteIdenticalToAnUnknownTeam is §4.3 and the
// §8 B proof: a non-member's session, a garbage session, a bearer and a session
// together, and no credential at all, on a private team that EXISTS, all get
// the same status, Content-Type and body as a slug that does not exist, on
// every route including /access and the stream. Also compared for the same
// viewer: the non-member's session on the unknown slug.
func TestPrivateTeamSessionRefusalsAreByteIdenticalToAnUnknownTeam(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	const unknown = "no-such-team-7q"
	cases := []struct {
		name    string
		slug    string
		headers map[string]string
	}{
		{"non-member session", fx.slug, session(fx.outsiderSession)},
		{"garbage session", fx.slug, session("mbs_" + strings.Repeat("A", 43))},
		{"bearer + session together (both the member's own)", fx.slug, map[string]string{
			"Authorization": "Bearer " + fx.memberToken, authz.HeaderBrowserSession: fx.ownerSession}},
		{"no credential", fx.slug, nil},
		{"non-member session, unknown slug", unknown, session(fx.outsiderSession)},
	}

	for _, p := range gatedPaths() {
		want := fetch(t, srv.URL+"/v1/teams/"+unknown+p, nil)
		if want.code != http.StatusNotFound {
			t.Fatalf("%q unknown slug, anonymous: got %d, want 404", p, want.code)
		}
		t.Logf("%-18s unknown slug, anonymous -> %d %q %q", p, want.code, want.contentType, want.body)
		for _, c := range cases {
			got := fetch(t, srv.URL+"/v1/teams/"+c.slug+p, c.headers)
			if got != want {
				t.Fatalf("%q %s distinguishes itself from an unknown slug:\n got  %d %q %q\n want %d %q %q",
					p, c.name, got.code, got.contentType, got.body, want.code, want.contentType, want.body)
			}
			t.Logf("%-18s %-50s -> %d %q %q  identical=%v", p, c.name, got.code, got.contentType, got.body, got == want)
		}
	}
}

// TestPublicTeamIgnoresAGarbageSession: the public short-circuit never looks at
// a credential, so a stale or garbage session (or two credentials) must not
// make a public board disappear.
func TestPublicTeamIgnoresAGarbageSession(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PUBLIC)
	srv := newGuardedServer(t, db)

	for name, h := range map[string]map[string]string{
		"garbage session":    session("mbs_garbage"),
		"bearer and session": {"Authorization": "Bearer mtk_garbage", authz.HeaderBrowserSession: "mbs_garbage"},
	} {
		for _, p := range gatedPaths() {
			if got := fetch(t, srv.URL+"/v1/teams/"+fx.slug+p, h); got.code != http.StatusOK {
				t.Fatalf("%q on a PUBLIC team with a %s: got %d, want 200 (body %s)", p, name, got.code, got.body)
			}
		}
	}
}

// TestAccessEndpointAnswersPerViewer pins GET /v1/teams/{slug}/access, exact
// bytes, for every viewer in the §4.5 matrix that gets a 200.
func TestAccessEndpointAnswersPerViewer(t *testing.T) {
	db := testDB(t)
	// One fixture (its invite code is a unique constant): the private cases
	// run first, then the same team is made public for the public cases.
	priv := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	pub := priv
	srv := newGuardedServer(t, db)

	type accessCase struct {
		name    string
		slug    string
		headers map[string]string
		want    string
	}
	check := func(c accessCase) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/teams/"+c.slug+"/access", nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != c.want {
			t.Fatalf("%s: %d %s, want 200 %s", c.name, resp.StatusCode, body, c.want)
		}
		if ct, cc := resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"); ct != "application/json" || cc != "no-store" {
			t.Fatalf("%s: Content-Type %q Cache-Control %q", c.name, ct, cc)
		}
		t.Logf("%-32s -> %d %s", c.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	for _, c := range []accessCase{
		{"private, owner session", priv.slug, session(priv.ownerSession), `{"visibility":"private","role":"owner"}`},
		{"private, member session", priv.slug, session(priv.memberSession), `{"visibility":"private","role":"member"}`},
		{"private, owner bearer", priv.slug, map[string]string{"Authorization": "Bearer " + priv.memberToken}, `{"visibility":"private","role":"owner"}`},
		{"private by uuid, owner session", priv.teamID, session(priv.ownerSession), `{"visibility":"private","role":"owner"}`},
	} {
		check(c)
	}

	mustExec(t, db, "UPDATE `team` SET `visibility` = ? WHERE `id` = ?", int64(enums.TEAM_VISIBILITY_PUBLIC), pub.teamID)
	for _, c := range []accessCase{
		{"public, anonymous", pub.slug, nil, `{"visibility":"public","role":null}`},
		{"public, garbage session", pub.slug, session("mbs_garbage"), `{"visibility":"public","role":null}`},
		{"public, non-member session", pub.slug, session(pub.outsiderSession), `{"visibility":"public","role":null}`},
		{"public, owner session", pub.slug, session(pub.ownerSession), `{"visibility":"public","role":"owner"}`},
		{"public, member session", pub.slug, session(pub.memberSession), `{"visibility":"public","role":"member"}`},
		{"public, bearer and session", pub.slug, map[string]string{
			"Authorization": "Bearer " + pub.memberToken, authz.HeaderBrowserSession: pub.ownerSession}, `{"visibility":"public","role":null}`},
	} {
		check(c)
	}
}

// TestMemberSessionLosesTheBoardOnTheNextRequest: membership is read on every
// request, uncached, and a revoked session is refused the same way.
func TestMemberSessionLosesTheBoardOnTheNextRequest(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	if got := fetch(t, srv.URL+"/v1/teams/"+fx.slug+"/access", session(fx.ownerSession)); got.code != http.StatusOK {
		t.Fatalf("the owner should start with access: %d", got.code)
	}
	mustExec(t, db, "UPDATE `member` SET `revoked_at` = NOW() WHERE `team_uuid` = ? AND `key` = ?", fx.teamID, "M-1")
	for _, p := range gatedPaths() {
		if got := fetch(t, srv.URL+"/v1/teams/"+fx.slug+p, session(fx.ownerSession)); got.code != http.StatusNotFound {
			t.Fatalf("%q the request after the membership was revoked: got %d, want 404", p, got.code)
		}
	}

	if got := fetch(t, srv.URL+"/v1/teams/"+fx.slug, session(fx.memberSession)); got.code != http.StatusOK {
		t.Fatalf("Beto should still have access: %d", got.code)
	}
	mustExec(t, db, "UPDATE `browser_session` SET `revoked_at` = UTC_TIMESTAMP(), `end_reason` = ? WHERE `secret_hash` = ?",
		enums.BROWSER_SESSION_END_REASON_SIGNED_OUT, browser.Hash(fx.memberSession))
	if got := fetch(t, srv.URL+"/v1/teams/"+fx.slug, session(fx.memberSession)); got.code != http.StatusNotFound {
		t.Fatalf("a signed-out session: got %d, want 404", got.code)
	}
}

// newReauthServer is newGuardedServer with the stream's re-authorization
// interval shortened to reauth.
func newReauthServer(t *testing.T, db *sql.DB, reauth time.Duration) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	NewAPI(db, nil).RegisterOn(r)
	hub := stream.NewHub(stream.NewDBSource(db), nil)
	s := stream.NewServer(hub, stream.NewDBTeamLookup(db), authz.NewGuard(db), nil)
	s.SetReauthInterval(reauth)
	s.RegisterOn(r)
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		hub.Close()
	})
	return srv
}

// openStream connects, reads the seeded frame after 41, and then drains the
// body on ONE goroutine (two readers on one body corrupt the connection). The
// returned channel closes when the stream ends.
func openStream(t *testing.T, url string, headers map[string]string) <-chan struct{} {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/stream?after=41", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: got %d, want 200", resp.StatusCode)
	}
	if frame := readOneSSEFrame(t, resp.Body, 5*time.Second); !strings.Contains(frame, "id: 42") {
		t.Fatalf("stream did not deliver: %s", frame)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	return done
}

// ends reports whether the stream has ended, waiting at most d, and how long
// it waited.
func ends(done <-chan struct{}, d time.Duration) (bool, time.Duration) {
	start := time.Now()
	select {
	case <-done:
		return true, time.Since(start)
	case <-time.After(d):
		return false, d
	}
}

// TestARevokedMembersOpenStreamEndsWithinTheReauthInterval is §4.4 on a real
// database: the gate admitted the member's session once; the membership is
// then revoked while the stream is open, and the stream must end within the
// re-auth interval (50 ms here, 60 s in production) instead of streaming on.
func TestARevokedMembersOpenStreamEndsWithinTheReauthInterval(t *testing.T) {
	const reauth = 50 * time.Millisecond
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newReauthServer(t, db, reauth)
	base := srv.URL + "/v1/teams/" + fx.slug

	body := openStream(t, base, session(fx.ownerSession))
	if done, _ := ends(body, 6*reauth); done {
		t.Fatal("a live member's stream was ended by re-authorization")
	}

	mustExec(t, db, "UPDATE `member` SET `revoked_at` = NOW() WHERE `team_uuid` = ? AND `key` = ?", fx.teamID, "M-1")
	done, took := ends(body, 40*reauth)
	if !done {
		t.Fatalf("a revoked member's open stream was still open after %v (re-auth every %v)", 40*reauth, reauth)
	}
	t.Logf("the revoked member's stream ended %v after the revocation (reauthInterval %v)", took.Round(time.Millisecond), reauth)

	if got := fetch(t, base+"/stream?after=41", session(fx.ownerSession)); got.code != http.StatusNotFound {
		t.Fatalf("the reconnect: got %d, want 404", got.code)
	}
}

// TestAnAnonymousStreamEndsWhenTheTeamGoesPrivate: F2's backend half. An
// anonymous stream on a public team must not keep streaming after the team is
// made private.
func TestAnAnonymousStreamEndsWhenTheTeamGoesPrivate(t *testing.T) {
	const reauth = 50 * time.Millisecond
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PUBLIC)
	srv := newReauthServer(t, db, reauth)
	base := srv.URL + "/v1/teams/" + fx.slug

	body := openStream(t, base, nil)
	mustExec(t, db, "UPDATE `team` SET `visibility` = ? WHERE `id` = ?", int64(enums.TEAM_VISIBILITY_PRIVATE), fx.teamID)
	if done, _ := ends(body, 40*reauth); !done {
		t.Fatal("an anonymous stream kept streaming after its team was made private")
	}
}

// TestASessionLookupOutageIs503NeverA404 is the DB-outage row of the threat
// model at the gate: when the session cannot be checked, a signed-in member
// gets 503 — not the 404 that would make the board forget the team, and not a
// 401 that would clear the cookie. A public team is unaffected.
func TestASessionLookupOutageIs503NeverA404(t *testing.T) {
	db := testDB(t)
	priv := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	mustExec(t, db, "RENAME TABLE `browser_session` TO `browser_session_outage_test`")
	t.Cleanup(func() { mustExec(t, db, "RENAME TABLE `browser_session_outage_test` TO `browser_session`") })

	for _, p := range gatedPaths() {
		got := fetch(t, srv.URL+"/v1/teams/"+priv.slug+p, session(priv.ownerSession))
		if got.code != http.StatusServiceUnavailable {
			t.Fatalf("%q private team, member session, session table unreachable: got %d, want 503 (body %s)", p, got.code, got.body)
		}
		if strings.Contains(got.body, "browser_session") || strings.Contains(got.body, "Error ") {
			t.Fatalf("%q the 503 echoed driver text: %s", p, got.body)
		}
	}

	// The same team made public never looks at the session, so the outage of
	// the session table does not touch it.
	mustExec(t, db, "UPDATE `team` SET `visibility` = ? WHERE `id` = ?", int64(enums.TEAM_VISIBILITY_PUBLIC), priv.teamID)
	for _, p := range gatedPaths() {
		if got := fetch(t, srv.URL+"/v1/teams/"+priv.slug+p, session(priv.ownerSession)); got.code != http.StatusOK {
			t.Fatalf("%q public team during a session-table outage: got %d, want 200", p, got.code)
		}
	}
}

// TestADatabaseOutageIs503ForEverySlug: with the database gone entirely the
// gate answers 503 for a private team AND for an unknown slug, byte-identical —
// the team row is read first, so an outage is not an oracle either.
func TestADatabaseOutageIs503ForEverySlug(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)

	dead, err := sql.Open("mysql", os.Getenv(dsnEnv))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = dead.Close()
	srv := newGuardedServer(t, dead)

	for _, p := range gatedPaths() {
		want := fetch(t, srv.URL+"/v1/teams/no-such-team-7q"+p, session(fx.ownerSession))
		if want.code != http.StatusServiceUnavailable {
			t.Fatalf("%q unknown slug with the database down: got %d, want 503", p, want.code)
		}
		for name, h := range map[string]map[string]string{
			"member session": session(fx.ownerSession),
			"anonymous":      nil,
		} {
			if got := fetch(t, srv.URL+"/v1/teams/"+fx.slug+p, h); got != want {
				t.Fatalf("%q %s with the database down: %d %q, want %d %q", p, name, got.code, got.body, want.code, want.body)
			}
		}
	}
}
