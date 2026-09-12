package stream

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"
)

// The database-backed half of this package: proof that the SQL in source.go
// produces the frames the hub and the SSE handler are tested against.
//
// It runs against a real MySQL holding the generated schema
// (core/repository/sql/schema/create.sql), pointed at by
// METICHE_TEST_MYSQL_DSN. Without that variable the test SKIPS with a reason
// rather than being deleted: the ordering and reconnect logic are covered
// without a database in hub_test.go and sse_test.go, and this is the part that
// genuinely cannot be.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true' go test ./app/stream/
//
// parseTime=true is required — occurred_at is scanned into a time.Time, as the
// generated repository also does.
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

func newUUID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}

// seedStreamTeam creates the minimum rows a frame joins against and returns
// the team uuid and slug.
func seedStreamTeam(t *testing.T, db *sql.DB) (uuid.UUID, string) {
	t.Helper()

	teamID := newUUID(t)
	slug := "stream-" + teamID[:8]

	mustExec(t, db, "INSERT INTO `team` (`id`,`name`,`slug`,`sequence`,`board_revision`,`status`) VALUES (?,?,?,?,?,?)",
		teamID, "Stream Test", slug, 3, 2, 1)
	t.Cleanup(func() { mustExec(t, db, "DELETE FROM `team` WHERE `id` = ?", teamID) })

	accountID := newUUID(t)
	mustExec(t, db, "INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
		accountID, "acct-"+accountID[:8], "Ana", "not-a-real-hash", 1, 1)
	t.Cleanup(func() { mustExec(t, db, "DELETE FROM `account` WHERE `id` = ?", accountID) })
	memberID := newUUID(t)
	mustExec(t, db, "INSERT INTO `member` (`id`,`account_uuid`,`team_uuid`,`key`,`display_name`,`role`,`status`) VALUES (?,?,?,?,?,?,?)",
		memberID, accountID, teamID, "M-1", "Ana", 1, 1)
	agentID := newUUID(t)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_key`,`status`) VALUES (?,?,?,?,?,?)",
		agentID, accountID, "A-1", "claude-1", "client-1", 1)
	projectID := newUUID(t)
	mustExec(t, db, "INSERT INTO `project` (`id`,`team_uuid`,`key`,`name`,`status`) VALUES (?,?,?,?,?)",
		projectID, teamID, "api", "API", 1)
	sessionID := newUUID(t)
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`status`) VALUES (?,?,?,?,?,?,?)",
		sessionID, teamID, projectID, agentID, memberID, "S-1", 1)

	// Three events: one team-level (no session), two session-scoped, one of
	// them structural.
	mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`kind`,`structural`,`summary`,`idempotency_key`) VALUES (?,?,?,?,?,?,?)",
		newUUID(t), teamID, 1, 1 /* member_joined */, 1, "Ana joined", "seed-1")
	mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`project_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`kind`,`structural`,`subject_kind`,`subject_key`,`summary`,`payload`,`idempotency_key`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		newUUID(t), teamID, 2, projectID, sessionID, agentID, memberID, 3 /* session_started */, 1,
		1 /* session */, "S-1", "session started", `{"message":"hello"}`, "seed-2")
	mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`project_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`kind`,`structural`,`subject_kind`,`subject_key`,`summary`,`payload`,`idempotency_key`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		newUUID(t), teamID, 3, projectID, sessionID, agentID, memberID, 6 /* intent_declared */, 0,
		2 /* intent */, "INT-1", "declared an intent", `{"paths":["app/auth.go"],"message":"auth"}`, "seed-3")

	parsed, err := uuid.FromString(teamID)
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return parsed, slug
}

func TestDBSourceFramesAfter(t *testing.T) {
	db := testDB(t)
	teamUUID, _ := seedStreamTeam(t, db)

	src := NewDBSource(db)

	all, err := src.FramesAfter(context.Background(), teamUUID, 0, batchLimit)
	if err != nil {
		t.Fatalf("FramesAfter: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d frames, want 3: %v", len(all), seqsOf(all))
	}

	// A team-level event has no session and must still be delivered — an inner
	// join here would leave a permanent hole at sequence 1.
	if all[0].Sequence != 1 || all[0].Kind != "member_joined" || all[0].SessionKey != "" {
		t.Fatalf("team-level frame wrong: %+v", all[0])
	}
	if !all[0].Structural {
		t.Fatal("member_joined should be marked structural in the seed")
	}
	if all[0].BoardRevision != 2 {
		t.Fatalf("board_revision = %d, want the team's current 2", all[0].BoardRevision)
	}

	if all[1].SessionKey != "S-1" || all[1].AgentLabel != "claude-1" ||
		all[1].MemberName != "Ana" || all[1].MemberKey != "M-1" || all[1].ProjectKey != "api" {
		t.Fatalf("session-scoped frame lost its joined keys: %+v", all[1])
	}
	if all[1].SubjectKind != "session" || all[1].SubjectKey != "S-1" {
		t.Fatalf("subject wrong: %+v", all[1])
	}
	if all[1].Payload.Message != "hello" {
		t.Fatalf("payload not decoded: %+v", all[1].Payload)
	}

	if all[2].Kind != "intent_declared" || all[2].Structural {
		t.Fatalf("intent_declared should not be structural: %+v", all[2])
	}
	if len(all[2].Payload.Paths) != 1 || all[2].Payload.Paths[0] != "app/auth.go" {
		t.Fatalf("payload paths not decoded: %+v", all[2].Payload)
	}

	// The cursor is exclusive.
	after2, err := src.FramesAfter(context.Background(), teamUUID, 2, batchLimit)
	if err != nil {
		t.Fatalf("FramesAfter: %v", err)
	}
	if len(after2) != 1 || after2[0].Sequence != 3 {
		t.Fatalf("after=2 returned %v, want just [3]", seqsOf(after2))
	}

	// And nothing leaks: the join code is in the team row this frame joined
	// against, and it must not have come out with it.
	for _, f := range all {
		if got := f.Summary + f.Payload.Message + f.Payload.Detail; strings.Contains(got, "JOINCODE") {
			t.Fatalf("a frame carried the join code: %+v", f)
		}
	}
}

func TestDBTeamLookup(t *testing.T) {
	db := testDB(t)
	teamUUID, slug := seedStreamTeam(t, db)

	lookup := NewDBTeamLookup(db)

	ref, err := lookup(context.Background(), slug)
	if err != nil {
		t.Fatalf("lookup by slug: %v", err)
	}
	if ref.UUID != teamUUID || ref.Sequence != 3 || ref.BoardRevision != 2 {
		t.Fatalf("lookup by slug returned %+v", ref)
	}

	// The route shadows the generated /v1/teams/{id}, so a uuid must resolve
	// here too.
	byID, err := lookup(context.Background(), teamUUID.String())
	if err != nil {
		t.Fatalf("lookup by uuid: %v", err)
	}
	if byID.Slug != slug {
		t.Fatalf("lookup by uuid returned %+v", byID)
	}

	if _, err := lookup(context.Background(), "definitely-not-a-team"); !errors.Is(err, ErrTeamNotFound) {
		t.Fatalf("unknown slug returned %v, want ErrTeamNotFound", err)
	}
}
