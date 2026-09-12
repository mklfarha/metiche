package webapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/stream"
)

// The read API is SQL over the generated schema, so every one of these tests
// needs a real MySQL holding create.sql. Point METICHE_TEST_MYSQL_DSN at one:
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true' go test ./app/webapi/
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
}

func seedBoard(t *testing.T, db *sql.DB) seeded {
	t.Helper()

	teamID := newUUID(t)
	slug := "board-" + teamID[:8]
	mustExec(t, db, "INSERT INTO `team` (`id`,`name`,`slug`,`join_code`,`sequence`,`board_revision`,`status`) VALUES (?,?,?,?,?,?,?)",
		teamID, "Board Demo", slug, fakeJoinCode, 42, 7, 1)
	t.Cleanup(func() { mustExec(t, db, "DELETE FROM `team` WHERE `id` = ?", teamID) })

	ana, beto := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `member` (`id`,`team_uuid`,`key`,`display_name`,`role`,`status`) VALUES (?,?,?,?,?,?)",
		ana, teamID, "M-1", "Ana", 1, 1)
	mustExec(t, db, "INSERT INTO `member` (`id`,`team_uuid`,`key`,`display_name`,`role`,`status`) VALUES (?,?,?,?,?,?)",
		beto, teamID, "M-2", "Beto", 2, 1)

	agentA, agentB := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`team_uuid`,`member_uuid`,`key`,`label`,`client_kind`,`token_hash`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?,?,?)",
		agentA, teamID, ana, "A-1", "claude-1", "claude-code", fakeTokenHash, "client-a", 1)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`team_uuid`,`member_uuid`,`key`,`label`,`client_kind`,`token_hash`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?,?,?)",
		agentB, teamID, beto, "A-2", "claude-2", "claude-code", fakeTokenHash, "client-b", 1)

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

	return seeded{teamID: teamID, slug: slug}
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

	for _, p := range []string{"", "/conflicts", "/contracts", "/decisions", "/sessions/S-1"} {
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
func assertNoSecrets(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{fakeJoinCode, fakeTokenHash} {
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
	stream.NewServer(hub, stream.NewDBTeamLookup(db), nil).RegisterOn(r)

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
