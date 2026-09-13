package sweeper

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	"go.uber.org/config"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/core"
	"github.com/mklfarha/metiche/backend/enums"
)

// These tests run against a REAL MySQL. There is no sqlite fallback and no
// mock, on purpose: what they prove — that a DELETE really removed rows, that
// ON DELETE CASCADE took the intents with a session and left the contracts
// alone, that the retention floor equals an actual MIN(sequence) — are
// properties of the schema. A fake would prove the fake works.
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
//	go test ./app/sweeper/ -run Integration -v
//
// The database must already have core/repository/sql/schema/create.sql
// applied (25 tables). No DSN is committed anywhere in this repository.
const dsnEnv = "METICHE_TEST_MYSQL_DSN"

// ─────────────────────────────────────────────
// Harness
// ─────────────────────────────────────────────

type harness struct {
	t    *testing.T
	core *core.Implementation
	db   *sql.DB

	teamUUID    uuid.UUID
	planUUID    uuid.UUID
	accountUUID uuid.UUID
	memberUUID  uuid.UUID
	agentUUID   uuid.UUID
	projectUUID uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv(dsnEnv))
	if dsn == "" {
		t.Skipf("skipped: %s is not set, so there is no MySQL to sweep. "+
			"Apply core/repository/sql/schema/create.sql to a database and set %s to its DSN.", dsnEnv, dsnEnv)
	}
	parts, params, ok := splitDSN(dsn)
	if !ok {
		t.Fatalf("%s does not look like a MySQL DSN (user:pass@tcp(host:port)/dbname?params)", dsnEnv)
	}
	yaml := fmt.Sprintf("db:\n  - name: %q\n    host: %q\n    port: %q\n    user: %q\n    pswd: %q\n    params: %q\n    driver: \"mysql\"\n",
		parts.db, parts.host, parts.port, parts.user, parts.pass, params)

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

	h := &harness{t: t, core: impl, db: impl.DB()}
	h.truncateAll()
	h.seedTeam()
	return h
}

type dsnParts struct{ user, pass, host, port, db string }

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
	closeParen := strings.Index(rest, ")")
	if closeParen < 0 {
		return p, "", false
	}
	hostport := rest[:closeParen]
	p.host, p.port = hostport, "3306"
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		p.host, p.port = hostport[:i], hostport[i+1:]
	}
	tail := strings.TrimPrefix(rest[closeParen+1:], "/")
	params := ""
	if i := strings.Index(tail, "?"); i >= 0 {
		p.db, params = tail[:i], tail[i+1:]
	} else {
		p.db = tail
	}
	// parseTime is not optional: every assertion in this file compares a
	// DATETIME against a time.Time.
	if !strings.Contains(params, "parseTime") {
		if params != "" {
			params += "&"
		}
		params += "parseTime=true"
	}
	return p, params, p.db != ""
}

// truncateAll gives every test a clean world. All 25 tables, with FK checks
// dropped around them because they reference each other and no order
// satisfies every constraint.
func (h *harness) truncateAll() {
	h.t.Helper()
	tables := []string{
		"limit_event", "team_event", "instruction", "judgement", "conflict_participant", "conflict",
		"contract_field", "contract_assertion", "contract", "decision_token", "decision_path",
		"decision", "intent_token", "claim_path", "claim", "intent", "session", "notification_channel",
		"invite", "project", "agent", "member", "team", "account", "plan",
	}
	if _, err := h.db.Exec("SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		h.t.Fatalf("disabling FK checks: %v", err)
	}
	for _, tbl := range tables {
		if _, err := h.db.Exec("TRUNCATE TABLE `" + tbl + "`"); err != nil {
			h.t.Fatalf("truncating %s: %v", tbl, err)
		}
	}
	if _, err := h.db.Exec("SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		h.t.Fatalf("re-enabling FK checks: %v", err)
	}
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(query, args...); err != nil {
		h.t.Fatalf("%s: %v", firstLine(query), err)
	}
}

func firstLine(q string) string {
	if i := strings.Index(q, "("); i > 0 {
		return strings.TrimSpace(q[:i])
	}
	return q
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatalf("%s: %v", firstLine(query), err)
	}
	return n
}

func id() string { return uuid.Must(uuid.NewV4()).String() }

// seedTeam creates the fixed cast: a plan, a team, an account, a member, an
// agent and a project. Nothing here writes a token, a join code or a webhook
// URL — the sweeper never reads those columns and the fixtures do not create
// a reason to.
func (h *harness) seedTeam() {
	h.t.Helper()
	h.planUUID = uuid.Must(uuid.NewV4())
	h.teamUUID = uuid.Must(uuid.NewV4())
	h.accountUUID = uuid.Must(uuid.NewV4())
	h.memberUUID = uuid.Must(uuid.NewV4())
	h.agentUUID = uuid.Must(uuid.NewV4())
	h.projectUUID = uuid.Must(uuid.NewV4())

	// retention_days NULL: forever, until a test says otherwise.
	h.exec("INSERT INTO `plan` (`id`,`key`,`name`,`is_instance_default`,`sort_order`,`status`) VALUES (?,?,?,?,?,?)",
		h.planUUID.String(), "test-plan", "Test plan", 0, 0, int64(enums.RECORD_STATUS_ACTIVE))

	h.exec("INSERT INTO `team` (`id`,`name`,`slug`,`sequence`,`board_revision`,`status`,`plan_uuid`) VALUES (?,?,?,?,?,?,?)",
		h.teamUUID.String(), "Test team", "test-"+h.teamUUID.String()[:8], 0, 0,
		int64(enums.RECORD_STATUS_ACTIVE), h.planUUID.String())

	h.exec("INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
		h.accountUUID.String(), "acct1", "Ana", "fixture-hash", 1, int64(enums.RECORD_STATUS_ACTIVE))

	h.exec("INSERT INTO `member` (`id`,`team_uuid`,`key`,`display_name`,`role`,`status`,`account_uuid`) VALUES (?,?,?,?,?,?,?)",
		h.memberUUID.String(), h.teamUUID.String(), "M-1", "Ana", int64(enums.MEMBER_ROLE_MEMBER),
		int64(enums.RECORD_STATUS_ACTIVE), h.accountUUID.String())

	h.exec("INSERT INTO `agent` (`id`,`key`,`label`,`client_key`,`status`,`account_uuid`) VALUES (?,?,?,?,?,?)",
		h.agentUUID.String(), "A-1", "claude", "client-a", int64(enums.AGENT_STATUS_ACTIVE), h.accountUUID.String())

	h.exec("INSERT INTO `project` (`id`,`team_uuid`,`key`,`name`,`status`,`cadence`) VALUES (?,?,?,?,?,?)",
		h.projectUUID.String(), h.teamUUID.String(), "metiche", "metiche",
		int64(enums.RECORD_STATUS_ACTIVE), int64(enums.PROJECT_CADENCE_HACKATHON))
}

func (h *harness) seedSession(key string, status enums.SessionStatus, heartbeat time.Time) uuid.UUID {
	h.t.Helper()
	sid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`branch`,`status`,`started_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		sid.String(), h.teamUUID.String(), h.projectUUID.String(), h.agentUUID.String(), h.memberUUID.String(),
		key, "feat/auth", int64(status), heartbeat, heartbeat)
	return sid
}

// seedClaim writes a held claim and its one denormalized claim_path row, the
// way declare_intent would.
func (h *harness) seedClaim(sessionUUID uuid.UUID, key, pattern string, expiresAt time.Time) uuid.UUID {
	h.t.Helper()
	cid := uuid.Must(uuid.NewV4())
	prefix := pattern
	if i := strings.Index(pattern, "*"); i >= 0 {
		prefix = pattern[:strings.LastIndex(pattern[:i+1], "/")+1]
	}
	h.exec("INSERT INTO `claim` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`mode`,`status`,`ttl_seconds`,`expires_at`,`hard_expires_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		cid.String(), h.teamUUID.String(), h.projectUUID.String(), sessionUUID.String(), h.memberUUID.String(),
		key, int64(enums.CLAIM_MODE_WRITE), int64(enums.CLAIM_STATUS_HELD), 900,
		expiresAt, expiresAt.Add(4*time.Hour))

	h.exec("INSERT INTO `claim_path` (`id`,`claim_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`mode`,`status`,`expires_at`,`pattern`,`pattern_norm`,`kind`,`prefix`,`depth`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id(), cid.String(), h.projectUUID.String(), sessionUUID.String(), h.memberUUID.String(),
		int64(enums.CLAIM_MODE_WRITE), int64(enums.CLAIM_STATUS_HELD), expiresAt,
		pattern, pattern, int64(enums.CLAIM_PATH_KIND_EXACT), prefix, 2)
	return cid
}

func (h *harness) seedEvent(seq int64, occurredAt time.Time) {
	h.t.Helper()
	h.exec("INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`kind`,`summary`,`idempotency_key`,`occurred_at`) VALUES (?,?,?,?,?,?,?)",
		id(), h.teamUUID.String(), seq, int64(enums.EVENT_KIND_INTENT_DECLARED),
		fmt.Sprintf("seeded event %d", seq), fmt.Sprintf("seed-%d", seq), occurredAt)
	h.exec("UPDATE `team` SET `sequence` = GREATEST(`sequence`, ?) WHERE `id` = ?", seq, h.teamUUID.String())
}

func (h *harness) sweeper(opts Options) *Sweeper {
	h.t.Helper()
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	return New(h.core, zap.NewNop(), opts)
}

func (h *harness) runOnce(s *Sweeper) Report {
	h.t.Helper()
	rep, err := s.RunOnce(context.Background())
	if err != nil {
		h.t.Fatalf("RunOnce: %v", err)
	}
	if len(rep.Errors) > 0 {
		h.t.Fatalf("RunOnce reported errors: %v", rep.Errors)
	}
	if rep.Skipped {
		h.t.Fatal("RunOnce was skipped; another process holds the lease")
	}
	return rep
}

func (h *harness) claimStatus(cid uuid.UUID) enums.ClaimStatus {
	h.t.Helper()
	var st int64
	if err := h.db.QueryRow("SELECT `status` FROM `claim` WHERE `id` = ?", cid.String()).Scan(&st); err != nil {
		h.t.Fatalf("reading claim status: %v", err)
	}
	return enums.ClaimStatus(st)
}

func (h *harness) sessionStatus(sid uuid.UUID) enums.SessionStatus {
	h.t.Helper()
	var st int64
	if err := h.db.QueryRow("SELECT `status` FROM `session` WHERE `id` = ?", sid.String()).Scan(&st); err != nil {
		h.t.Fatalf("reading session status: %v", err)
	}
	return enums.SessionStatus(st)
}

// detectionHits is the shape of the query that runs inside every
// declare_intent: one index scan over claim_path filtered on project, status,
// prefix and — the part that matters here — expires_at.
func (h *harness) detectionHits(prefix string, now time.Time) int {
	h.t.Helper()
	return h.count(
		"SELECT COUNT(*) FROM `claim_path` WHERE `project_uuid` = ? AND `status` = ? "+
			"AND (`prefix` = ? OR `prefix` LIKE CONCAT(?, '%')) AND `expires_at` > ?",
		h.projectUUID.String(), int64(enums.CLAIM_STATUS_HELD), prefix, prefix, now)
}

// boardHits is what a consumer that trusts the STORED status sees — the
// board, a state read, anything that has no clock of its own. This is the
// number the sweeper exists to correct.
func (h *harness) boardHits(prefix string) int {
	h.t.Helper()
	return h.count(
		"SELECT COUNT(*) FROM `claim_path` WHERE `project_uuid` = ? AND `status` = ? AND `prefix` = ?",
		h.projectUUID.String(), int64(enums.CLAIM_STATUS_HELD), prefix)
}

// ─────────────────────────────────────────────
// (a) Claims
// ─────────────────────────────────────────────

// TestIntegrationClaimPastTTLExpiresAndLeavesDetection proves both halves of
// PLAN.md's claim about TTLs at once:
//
//   - the lapsed claim is ALREADY invisible to detection before the sweeper
//     runs, because the detection query filters on expires_at. Correctness
//     does not depend on this package running.
//   - after the pass, the stored status agrees, so the board and every reader
//     with no clock of its own sees the same thing.
func TestIntegrationClaimPastTTLExpiresAndLeavesDetection(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()

	sess := h.seedSession("S-1", enums.SESSION_STATUS_LIVE, now.Add(-10*time.Second))
	dead := h.seedClaim(sess, "C-1", "src/auth.go", now.Add(-time.Minute)) // lapsed a minute ago
	live := h.seedClaim(sess, "C-2", "src/live.go", now.Add(time.Hour))    // still held

	// Before the sweeper: the stored status still says held…
	if got := h.claimStatus(dead); got != enums.CLAIM_STATUS_HELD {
		t.Fatalf("fixture is wrong: lapsed claim starts as %v", got)
	}
	// …and yet detection already ignores it. This is the property PLAN.md
	// insists on: "lazy filter is authoritative; sweeper is for visibility".
	if n := h.detectionHits("src/auth.go", now); n != 0 {
		t.Fatalf("a claim past its TTL was still a detection candidate before the sweep (%d hits); "+
			"correctness must not depend on the sweeper running", n)
	}
	if n := h.detectionHits("src/live.go", now); n != 1 {
		t.Fatalf("the live claim was not a detection candidate (%d hits); the query under test is wrong", n)
	}
	// The board, which has no clock, still shows the dead hold.
	if n := h.boardHits("src/auth.go"); n != 1 {
		t.Fatalf("fixture is wrong: the board shows %d held paths for the lapsed claim, want 1", n)
	}

	rep := h.runOnce(h.sweeper(Options{}))

	if rep.ClaimsExpired != 1 {
		t.Errorf("report says %d claims expired, want 1", rep.ClaimsExpired)
	}
	if rep.ClaimPathsExpired != 1 {
		t.Errorf("report says %d claim paths expired, want 1", rep.ClaimPathsExpired)
	}
	if got := h.claimStatus(dead); got != enums.CLAIM_STATUS_EXPIRED {
		t.Errorf("lapsed claim is %v after the sweep, want expired", got)
	}
	if got := h.claimStatus(live); got != enums.CLAIM_STATUS_HELD {
		t.Errorf("the live claim was expired too (%v); the sweeper must only touch lapsed holds", got)
	}
	if n := h.boardHits("src/auth.go"); n != 0 {
		t.Errorf("the board still shows %d held paths for the expired claim; the denormalized copy on claim_path was not updated", n)
	}
	if n := h.boardHits("src/live.go"); n != 1 {
		t.Errorf("the live claim vanished from the board (%d held paths)", n)
	}

	events := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CLAIM_EXPIRED), dead.String())
	if events != 1 {
		t.Errorf("%d claim_expired events for the lapsed claim, want 1 — the board learns from the log", events)
	}

	// Idempotent: a second pass changes nothing and says nothing twice.
	before := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String())
	rep2 := h.runOnce(h.sweeper(Options{}))
	if rep2.ClaimsExpired != 0 {
		t.Errorf("the second pass expired %d claims, want 0", rep2.ClaimsExpired)
	}
	after := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String())
	if before != after {
		t.Errorf("the second pass wrote %d extra events; the sweeper must be idempotent", after-before)
	}
}

// TestIntegrationClaimPastHardCeilingExpires covers the 4h cap a heartbeat
// cannot push past: expires_at is still in the future, hard_expires_at is not.
func TestIntegrationClaimPastHardCeilingExpires(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	sess := h.seedSession("S-1", enums.SESSION_STATUS_LIVE, now)

	cid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `claim` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`mode`,`status`,`ttl_seconds`,`expires_at`,`hard_expires_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		cid.String(), h.teamUUID.String(), h.projectUUID.String(), sess.String(), h.memberUUID.String(),
		"C-9", int64(enums.CLAIM_MODE_WRITE), int64(enums.CLAIM_STATUS_HELD), 900,
		now.Add(10*time.Minute), // a heartbeat pushed the TTL forward…
		now.Add(-time.Minute))   // …but the 4h ceiling passed a minute ago

	rep := h.runOnce(h.sweeper(Options{}))
	if rep.ClaimsExpired != 1 {
		t.Fatalf("report says %d claims expired, want 1: a heartbeat must not extend a claim past the hard ceiling", rep.ClaimsExpired)
	}
	if got := h.claimStatus(cid); got != enums.CLAIM_STATUS_EXPIRED {
		t.Fatalf("claim past its hard ceiling is %v, want expired", got)
	}
}

// ─────────────────────────────────────────────
// (b) Sessions
// ─────────────────────────────────────────────

// TestIntegrationSessionGoesStaleThenAbandoned walks PLAN.md's two
// thresholds: 180s without a heartbeat is stale, 600s is abandoned.
func TestIntegrationSessionGoesStaleThenAbandoned(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()

	sess := h.seedSession("S-1", enums.SESSION_STATUS_LIVE, now.Add(-200*time.Second))
	claim := h.seedClaim(sess, "C-1", "src/auth.go", now.Add(2*time.Hour))
	alive := h.seedSession("S-2", enums.SESSION_STATUS_LIVE, now.Add(-5*time.Second))

	// ── 200s: past 180, short of 600 ────────────────────────────────────────
	rep := h.runOnce(h.sweeper(Options{}))
	if rep.SessionsStale != 1 {
		t.Fatalf("report says %d sessions went stale, want 1", rep.SessionsStale)
	}
	if rep.SessionsAbandoned != 0 {
		t.Fatalf("report abandoned %d sessions at 200s; the threshold is 600s", rep.SessionsAbandoned)
	}
	if got := h.sessionStatus(sess); got != enums.SESSION_STATUS_STALE {
		t.Fatalf("session with a 200s-old heartbeat is %v, want stale", got)
	}
	if got := h.sessionStatus(alive); got != enums.SESSION_STATUS_LIVE {
		t.Fatalf("the session that heartbeated five seconds ago is %v, want live", got)
	}
	if got := h.claimStatus(claim); got != enums.CLAIM_STATUS_HELD {
		t.Fatalf("a stale session's claim is %v; going stale is reversible and must not drop holds", got)
	}

	// ── 700s: past 600 ──────────────────────────────────────────────────────
	h.exec("UPDATE `session` SET `last_heartbeat_at` = ?, `started_at` = ? WHERE `id` = ?",
		now.Add(-700*time.Second), now.Add(-700*time.Second), sess.String())

	rep = h.runOnce(h.sweeper(Options{}))
	if rep.SessionsAbandoned != 1 {
		t.Fatalf("report says %d sessions were abandoned, want 1", rep.SessionsAbandoned)
	}
	if got := h.sessionStatus(sess); got != enums.SESSION_STATUS_ABANDONED {
		t.Fatalf("session with a 700s-old heartbeat is %v, want abandoned", got)
	}
	var ended sql.NullTime
	var outcome sql.NullInt64
	if err := h.db.QueryRow("SELECT `ended_at`, `outcome` FROM `session` WHERE `id` = ?", sess.String()).Scan(&ended, &outcome); err != nil {
		t.Fatalf("reading the abandoned session: %v", err)
	}
	if !ended.Valid {
		t.Error("an abandoned session has no ended_at; its run record never closes")
	}
	if !outcome.Valid || enums.SessionOutcome(outcome.Int64) != enums.SESSION_OUTCOME_ABANDONED {
		t.Errorf("abandoned session's outcome is %v, want abandoned", outcome)
	}
	if got := h.claimStatus(claim); got != enums.CLAIM_STATUS_EXPIRED {
		t.Errorf("a written-off session still holds a claim (%v); its two-hour TTL would keep a dead agent on the board", got)
	}
	if n := h.boardHits("src/auth.go"); n != 0 {
		t.Errorf("the board still shows %d held paths for an abandoned session", n)
	}

	events := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_SESSION_ABANDONED), sess.String())
	if events != 1 {
		t.Errorf("%d session_abandoned events, want 1", events)
	}
	var rev int64
	if err := h.db.QueryRow("SELECT `board_revision` FROM `team` WHERE `id` = ?", h.teamUUID.String()).Scan(&rev); err != nil {
		t.Fatalf("reading board_revision: %v", err)
	}
	if rev < 1 {
		t.Errorf("board_revision is %d; a session leaving the board is a structural change and must ask for a re-layout", rev)
	}

	// Idempotent.
	before := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String())
	rep = h.runOnce(h.sweeper(Options{}))
	if rep.SessionsAbandoned != 0 || rep.SessionsStale != 0 {
		t.Errorf("the second pass moved %d/%d sessions, want 0", rep.SessionsStale, rep.SessionsAbandoned)
	}
	if after := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); after != before {
		t.Errorf("the second pass wrote %d extra events", after-before)
	}
}

// TestIntegrationSessionWithNoHeartbeatEverIsSwept covers the agent that
// crashed on its first loop: last_heartbeat_at is NULL, and a comparison
// against NULL is NULL, so a naive query would leave it "live" forever.
func TestIntegrationSessionWithNoHeartbeatEverIsSwept(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()

	sid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`status`,`started_at`) VALUES (?,?,?,?,?,?,?,?)",
		sid.String(), h.teamUUID.String(), h.projectUUID.String(), h.agentUUID.String(), h.memberUUID.String(),
		"S-9", int64(enums.SESSION_STATUS_LIVE), now.Add(-time.Hour))

	h.runOnce(h.sweeper(Options{}))
	if got := h.sessionStatus(sid); got != enums.SESSION_STATUS_ABANDONED {
		t.Fatalf("a session that never heartbeated and started an hour ago is %v, want abandoned", got)
	}
}

// ─────────────────────────────────────────────
// (c) contract_unclaimed
// ─────────────────────────────────────────────

func (h *harness) seedContract(key string) uuid.UUID {
	h.t.Helper()
	cid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `contract` (`id`,`team_uuid`,`project_uuid`,`key`,`key_norm`,`kind`,`status`,`title`) VALUES (?,?,?,?,?,?,?,?)",
		cid.String(), h.teamUUID.String(), h.projectUUID.String(), key, strings.ToLower(key),
		int64(enums.CONTRACT_KIND_HTTP_ENDPOINT), int64(enums.CONTRACT_STATUS_PUBLISHED), key)
	return cid
}

func (h *harness) seedAssertion(contractUUID, sessionUUID uuid.UUID, role enums.AssertionRole, assertedAt time.Time) uuid.UUID {
	h.t.Helper()
	aid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `contract_assertion` (`id`,`contract_uuid`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,"+
		"`role`,`shape_hash`,`shape_fingerprint`,`status`,`active_marker`,`asserted_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		aid.String(), contractUUID.String(), h.teamUUID.String(), h.projectUUID.String(), sessionUUID.String(),
		h.memberUUID.String(), int64(role), "hash-"+aid.String()[:8], "fp-"+aid.String()[:8],
		int64(enums.ASSERTION_STATUS_ACTIVE), 1, assertedAt)
	return aid
}

// TestIntegrationContractUnclaimedIsRaisedOnceAndNotified is PLAN.md's
// highest-value signal: somebody is coding against an endpoint nobody is
// building.
func TestIntegrationContractUnclaimedIsRaisedOnceAndNotified(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()

	consumer := h.seedSession("S-1", enums.SESSION_STATUS_LIVE, now.Add(-5*time.Second))
	contract := h.seedContract("POST /api/login")

	// Three minutes: short of the five-minute hackathon threshold.
	assertion := h.seedAssertion(contract, consumer, enums.ASSERTION_ROLE_CONSUMES, now.Add(-3*time.Minute))
	rep := h.runOnce(h.sweeper(Options{}))
	if rep.UnclaimedRaised != 0 {
		t.Fatalf("raised %d conflicts after three minutes; the hackathon threshold is five", rep.UnclaimedRaised)
	}

	// Ten minutes: past it.
	h.exec("UPDATE `contract_assertion` SET `asserted_at` = ? WHERE `id` = ?", now.Add(-10*time.Minute), assertion.String())

	rep = h.runOnce(h.sweeper(Options{}))
	if rep.UnclaimedRaised != 1 {
		t.Fatalf("raised %d contract_unclaimed conflicts, want 1", rep.UnclaimedRaised)
	}

	var (
		kind, severity, status int64
		rule                   sql.NullString
		suggested              sql.NullString
		occurrences            int64
		conflictUUID           string
	)
	if err := h.db.QueryRow(
		"SELECT `id`,`kind`,`severity`,`status`,`detector_rule`,`suggested_action`,`occurrence_count` "+
			"FROM `conflict` WHERE `team_uuid` = ?", h.teamUUID.String()).
		Scan(&conflictUUID, &kind, &severity, &status, &rule, &suggested, &occurrences); err != nil {
		t.Fatalf("reading the conflict: %v", err)
	}
	if enums.ConflictKind(kind) != enums.CONFLICT_KIND_CONTRACT_UNCLAIMED {
		t.Errorf("conflict kind is %v, want contract_unclaimed", enums.ConflictKind(kind))
	}
	if enums.ConflictStatus(status) != enums.CONFLICT_STATUS_OPEN {
		t.Errorf("conflict status is %v, want open", enums.ConflictStatus(status))
	}
	if rule.String != DetectorRuleUnclaimed {
		t.Errorf("detector_rule is %q, want %q", rule.String, DetectorRuleUnclaimed)
	}
	// PLAN.md, noise control: "never surface a conflict without a suggested
	// next action".
	if !suggested.Valid || strings.TrimSpace(suggested.String) == "" {
		t.Error("the conflict carries no suggested action, which makes it noise rather than signal")
	}
	if !strings.Contains(suggested.String, "POST /api/login") {
		t.Errorf("the suggested action does not name the contract: %q", suggested.String)
	}

	if n := h.count("SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
		conflictUUID, consumer.String()); n != 1 {
		t.Errorf("%d participants for the consumer session, want 1", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ? AND `status` = ?",
		consumer.String(), int64(enums.INSTRUCTION_STATUS_PENDING)); n != 1 {
		t.Errorf("%d pending instructions for the consumer, want 1 — the instruction IS the push", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_RAISED)); n != 1 {
		t.Errorf("%d conflict_raised events, want 1", n)
	}

	// ── Re-detection bumps a counter, it does not spam ──────────────────────
	rep = h.runOnce(h.sweeper(Options{}))
	if rep.UnclaimedRaised != 0 {
		t.Errorf("the second pass raised %d new conflicts for the same pair, want 0", rep.UnclaimedRaised)
	}
	if rep.UnclaimedReDetected != 1 {
		t.Errorf("the second pass re-detected %d, want 1", rep.UnclaimedReDetected)
	}
	if n := h.count("SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 1 {
		t.Errorf("%d conflict rows after two passes, want 1", n)
	}
	if n := h.count("SELECT `occurrence_count` FROM `conflict` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 2 {
		t.Errorf("occurrence_count is %d after two detections, want 2", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_RAISED)); n != 1 {
		t.Errorf("%d conflict_raised events after two passes, want 1", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 1 {
		t.Errorf("%d instructions after two passes, want 1 — re-detection must not re-interrupt", n)
	}

	// ── A producer appears: nothing new is raised ───────────────────────────
	producer := h.seedSession("S-2", enums.SESSION_STATUS_LIVE, now.Add(-5*time.Second))
	h.seedAssertion(contract, producer, enums.ASSERTION_ROLE_PRODUCES, now.Add(-time.Minute))

	before := h.count("SELECT `occurrence_count` FROM `conflict` WHERE `team_uuid` = ?", h.teamUUID.String())
	rep = h.runOnce(h.sweeper(Options{}))
	if rep.UnclaimedRaised != 0 || rep.UnclaimedReDetected != 0 {
		t.Errorf("with a producer present the sweeper still fired (raised=%d redetected=%d)",
			rep.UnclaimedRaised, rep.UnclaimedReDetected)
	}
	if after := h.count("SELECT `occurrence_count` FROM `conflict` WHERE `team_uuid` = ?", h.teamUUID.String()); after != before {
		t.Errorf("occurrence_count moved from %d to %d after a producer appeared", before, after)
	}
}

// TestIntegrationContractUnclaimedRespectsCadence: the same fixture on a
// steady project says nothing, because a day has not passed.
func TestIntegrationContractUnclaimedRespectsCadence(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.exec("UPDATE `project` SET `cadence` = ? WHERE `id` = ?",
		int64(enums.PROJECT_CADENCE_STEADY), h.projectUUID.String())

	consumer := h.seedSession("S-1", enums.SESSION_STATUS_LIVE, now.Add(-5*time.Second))
	contract := h.seedContract("POST /api/login")
	h.seedAssertion(contract, consumer, enums.ASSERTION_ROLE_CONSUMES, now.Add(-2*time.Hour))

	rep := h.runOnce(h.sweeper(Options{}))
	if rep.UnclaimedRaised != 0 {
		t.Fatalf("raised %d conflicts two hours in on a STEADY project, where the threshold is a day", rep.UnclaimedRaised)
	}
	if n := h.count("SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Fatalf("%d conflict rows, want 0", n)
	}
}

// ─────────────────────────────────────────────
// (d) Retention
// ─────────────────────────────────────────────

func (h *harness) setRetentionDays(days int) {
	h.t.Helper()
	h.exec("UPDATE `plan` SET `retention_days` = ? WHERE `id` = ?", days, h.planUUID.String())
}

func (h *harness) teamFloor() int64 {
	h.t.Helper()
	var floor int64
	if err := h.db.QueryRow("SELECT `retention_floor_sequence` FROM `team` WHERE `id` = ?", h.teamUUID.String()).Scan(&floor); err != nil {
		h.t.Fatalf("reading retention_floor_sequence: %v", err)
	}
	return floor
}

// actualMinSequence is what is REALLY on disk. Every floor assertion in this
// file compares against this rather than against a number the test expected,
// because the whole point of the floor is that it describes the disk.
func (h *harness) actualMinSequence() (int64, bool) {
	h.t.Helper()
	var min sql.NullInt64
	if err := h.db.QueryRow("SELECT MIN(`sequence`) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()).Scan(&min); err != nil {
		h.t.Fatalf("reading MIN(sequence): %v", err)
	}
	return min.Int64, min.Valid
}

func retentionOn(batch int) Options {
	return Options{
		RetentionEnabled:  true,
		RetentionInterval: 0, // every pass, which is what an operator running it by hand wants
		BatchSize:         batch,
	}
}

// TestIntegrationRetentionDeletesEventsAndAdvancesFloor is the test that
// matters most in this file.
//
// The floor is asserted against a real MIN(sequence) taken after the delete,
// not against the number the test was expecting. A floor that matches an
// expectation but not the disk is precisely the bug it exists to prevent: an
// SSE client reconnecting with ?after=N below the floor has missed events
// that no longer exist and must be told to reload rather than handed a silent
// hole.
func TestIntegrationRetentionDeletesEventsAndAdvancesFloor(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.setRetentionDays(7)

	for seq := int64(1); seq <= 5; seq++ {
		h.seedEvent(seq, now.AddDate(0, 0, -30)) // thirty days old
	}
	for seq := int64(6); seq <= 8; seq++ {
		h.seedEvent(seq, now.Add(-time.Hour)) // an hour old
	}
	if h.teamFloor() != 0 {
		t.Fatalf("fixture is wrong: the floor starts at %d", h.teamFloor())
	}

	// BatchSize 2 so the five old events take three bounded DELETEs rather
	// than one statement locking the whole range.
	rep := h.runOnce(h.sweeper(retentionOn(2)))

	if !rep.Retention.Enabled {
		t.Fatal("the report says retention was disabled")
	}
	if rep.Retention.EventsDeleted != 5 {
		t.Errorf("deleted %d events, want 5", rep.Retention.EventsDeleted)
	}
	if rep.Retention.Batches < 3 {
		t.Errorf("used %d batches for five rows at a batch size of two; the delete was not bounded", rep.Retention.Batches)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 3 {
		t.Errorf("%d events remain, want 3", n)
	}

	min, ok := h.actualMinSequence()
	if !ok {
		t.Fatal("no events remain at all; the fixture's recent events were deleted too")
	}
	if got := h.teamFloor(); got != min {
		t.Errorf("retention_floor_sequence is %d but the lowest sequence actually on disk is %d; "+
			"a client reconnecting at %d would be handed a silent hole", got, min, got)
	}
	if rep.Retention.FloorsAdvanced != 1 {
		t.Errorf("the report says %d floors advanced, want 1", rep.Retention.FloorsAdvanced)
	}

	var sweptAt sql.NullTime
	if err := h.db.QueryRow("SELECT `last_retention_sweep_at` FROM `team` WHERE `id` = ?", h.teamUUID.String()).Scan(&sweptAt); err != nil {
		t.Fatalf("reading last_retention_sweep_at: %v", err)
	}
	if !sweptAt.Valid {
		t.Error("last_retention_sweep_at is NULL after a sweep; a policy that has stopped running would be invisible")
	}

	// A second pass deletes nothing more and leaves the floor where it is.
	rep2 := h.runOnce(h.sweeper(retentionOn(2)))
	if rep2.Retention.EventsDeleted != 0 {
		t.Errorf("the second pass deleted %d more events, want 0", rep2.Retention.EventsDeleted)
	}
	if got := h.teamFloor(); got != min {
		t.Errorf("the floor moved to %d on an empty pass, want %d", got, min)
	}
}

// TestIntegrationRetentionDeletesClosedSessionsOnly proves the other half of
// what is deletable, and that live work is not.
func TestIntegrationRetentionDeletesClosedSessionsOnly(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.setRetentionDays(7)
	old := now.AddDate(0, 0, -30)

	ended := h.seedSession("S-1", enums.SESSION_STATUS_ENDED, old)
	h.exec("UPDATE `session` SET `ended_at` = ?, `updated_at` = ? WHERE `id` = ?", old, old, ended.String())
	abandoned := h.seedSession("S-2", enums.SESSION_STATUS_ABANDONED, old)
	h.exec("UPDATE `session` SET `ended_at` = ?, `updated_at` = ? WHERE `id` = ?", old, old, abandoned.String())
	// Still running, and old enough to be caught by a careless cutoff.
	live := h.seedSession("S-3", enums.SESSION_STATUS_LIVE, now)
	h.exec("UPDATE `session` SET `updated_at` = ? WHERE `id` = ?", old, live.String())
	// A claim on the ended session: it must go with it, through the schema's
	// own cascade rather than through a second DELETE here.
	h.seedClaim(ended, "C-1", "src/auth.go", old)

	rep := h.runOnce(h.sweeper(retentionOn(500)))

	if rep.Retention.SessionsDeleted != 2 {
		t.Errorf("deleted %d sessions, want 2 (the ended one and the abandoned one)", rep.Retention.SessionsDeleted)
	}
	if n := h.count("SELECT COUNT(*) FROM `session` WHERE `id` = ?", live.String()); n != 1 {
		t.Error("a LIVE session was deleted; current state is not history")
	}
	if n := h.count("SELECT COUNT(*) FROM `claim` WHERE `session_uuid` = ?", ended.String()); n != 0 {
		t.Errorf("%d claims survived their deleted session; the cascade did not fire", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `claim_path` WHERE `session_uuid` = ?", ended.String()); n != 0 {
		t.Errorf("%d claim_paths survived their deleted session", n)
	}
}

// TestIntegrationRetentionSparesDecisionsAndContracts is the guard on the
// rule that matters most.
//
// Decisions and contracts are standing agreements, not history. "auth is a
// JWT in an httpOnly cookie" does not become less true because it was agreed
// ninety days ago, and if it silently disappeared the team would go on
// believing it had been agreed while every new agent saw nothing. The
// retention pass deletes the events around them and must leave them entirely
// alone.
func TestIntegrationRetentionSparesDecisionsAndContracts(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.setRetentionDays(1)
	old := now.AddDate(0, 0, -30)

	// A closed session from a month ago, which retention WILL delete…
	sess := h.seedSession("S-1", enums.SESSION_STATUS_ENDED, old)
	h.exec("UPDATE `session` SET `ended_at` = ?, `updated_at` = ? WHERE `id` = ?", old, old, sess.String())

	// …and the agreements that session left behind, which it must not.
	decisionUUID := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`status`,`always_show`,`decided_at`,`created_at`,`updated_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		decisionUUID.String(), h.teamUUID.String(), h.projectUUID.String(), "#auth-jwt-cookie",
		"Auth is a JWT in an httpOnly cookie", "Auth is a JWT in an httpOnly cookie, never localStorage.",
		int64(enums.DECISION_STATUS_ACCEPTED), 1, old, old, old)
	h.exec("INSERT INTO `decision_path` (`id`,`decision_uuid`,`team_uuid`,`project_uuid`,`pattern`,`pattern_norm`,`kind`,`prefix`,`created_at`,`updated_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		id(), decisionUUID.String(), h.teamUUID.String(), h.projectUUID.String(), "src/auth/**", "src/auth/**",
		int64(enums.CLAIM_PATH_KIND_GLOB), "src/auth/", old, old)
	h.exec("INSERT INTO `decision_token` (`id`,`decision_uuid`,`team_uuid`,`project_uuid`,`token`,`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?)",
		id(), decisionUUID.String(), h.teamUUID.String(), h.projectUUID.String(), "jwt", old, old)

	contract := h.seedContract("POST /api/login")
	h.exec("UPDATE `contract` SET `created_at` = ?, `updated_at` = ? WHERE `id` = ?", old, old, contract.String())
	assertion := h.seedAssertion(contract, sess, enums.ASSERTION_ROLE_PRODUCES, old)
	h.exec("UPDATE `contract_assertion` SET `created_at` = ?, `updated_at` = ? WHERE `id` = ?", old, old, assertion.String())
	h.exec("INSERT INTO `contract_field` (`id`,`assertion_uuid`,`contract_uuid`,`path`,`path_snake`,`type`,`required`,`direction`,`created_at`,`updated_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		id(), assertion.String(), contract.String(), "response.token", "response.token",
		int64(enums.CONTRACT_FIELD_TYPE_STRING), 1, int64(enums.FIELD_DIRECTION_OUT), old, old)

	// Events around them, which retention WILL delete.
	for seq := int64(1); seq <= 4; seq++ {
		h.seedEvent(seq, old)
	}
	h.seedEvent(5, now.Add(-time.Minute))

	rep := h.runOnce(h.sweeper(retentionOn(500)))

	if rep.Retention.EventsDeleted != 4 {
		t.Fatalf("deleted %d events, want 4 — the pass under test has to actually delete around the agreements", rep.Retention.EventsDeleted)
	}
	if rep.Retention.SessionsDeleted != 1 {
		t.Fatalf("deleted %d sessions, want 1", rep.Retention.SessionsDeleted)
	}

	survivors := []struct {
		what  string
		query string
		args  []any
	}{
		{"decision", "SELECT COUNT(*) FROM `decision` WHERE `id` = ?", []any{decisionUUID.String()}},
		{"decision_path", "SELECT COUNT(*) FROM `decision_path` WHERE `decision_uuid` = ?", []any{decisionUUID.String()}},
		{"decision_token", "SELECT COUNT(*) FROM `decision_token` WHERE `decision_uuid` = ?", []any{decisionUUID.String()}},
		{"contract", "SELECT COUNT(*) FROM `contract` WHERE `id` = ?", []any{contract.String()}},
		{"contract_assertion", "SELECT COUNT(*) FROM `contract_assertion` WHERE `id` = ?", []any{assertion.String()}},
		{"contract_field", "SELECT COUNT(*) FROM `contract_field` WHERE `assertion_uuid` = ?", []any{assertion.String()}},
	}
	for _, s := range survivors {
		if n := h.count(s.query, s.args...); n != 1 {
			t.Errorf("%s did not survive the retention pass (%d rows) — a standing agreement was deleted as if it were history", s.what, n)
		}
	}

	// And the floor still describes what is actually there.
	min, ok := h.actualMinSequence()
	if !ok {
		t.Fatal("every event was deleted; the recent one should have survived")
	}
	if got := h.teamFloor(); got != min {
		t.Errorf("retention_floor_sequence is %d, the real MIN(sequence) is %d", got, min)
	}
}

// TestIntegrationRetentionDisabledDeletesNothing is the posture test.
//
// Same fixture, same ages, same plan — only the operator's switch is off. A
// self-hoster who never asked for a retention policy must find every row
// where they left it.
func TestIntegrationRetentionDisabledDeletesNothing(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.setRetentionDays(1) // a policy exists…
	old := now.AddDate(0, 0, -30)

	for seq := int64(1); seq <= 5; seq++ {
		h.seedEvent(seq, old)
	}
	sess := h.seedSession("S-1", enums.SESSION_STATUS_ENDED, old)
	h.exec("UPDATE `session` SET `ended_at` = ?, `updated_at` = ? WHERE `id` = ?", old, old, sess.String())

	// …and enforcement is off, which is the default.
	opts := Options{}
	if opts.RetentionEnabled {
		t.Fatal("the zero Options enabled retention")
	}
	rep := h.runOnce(h.sweeper(opts))

	if rep.Retention.Enabled {
		t.Error("the report claims retention was enabled")
	}
	if rep.Retention.EventsDeleted != 0 || rep.Retention.SessionsDeleted != 0 {
		t.Fatalf("with enforcement disabled the pass deleted %d events and %d sessions, want 0 and 0",
			rep.Retention.EventsDeleted, rep.Retention.SessionsDeleted)
	}
	if rep.Retention.TeamsConsidered != 0 {
		t.Errorf("the disabled pass considered %d teams; it must not even look", rep.Retention.TeamsConsidered)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 5 {
		t.Errorf("%d events remain, want all 5", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `session` WHERE `id` = ?", sess.String()); n != 1 {
		t.Error("a closed session was deleted with enforcement disabled")
	}
	if got := h.teamFloor(); got != 0 {
		t.Errorf("the floor moved to %d with enforcement disabled", got)
	}
	var sweptAt sql.NullTime
	if err := h.db.QueryRow("SELECT `last_retention_sweep_at` FROM `team` WHERE `id` = ?", h.teamUUID.String()).Scan(&sweptAt); err != nil {
		t.Fatalf("reading last_retention_sweep_at: %v", err)
	}
	if sweptAt.Valid {
		t.Error("last_retention_sweep_at was stamped by a pass that enforced nothing; it must stay NULL so an operator can see the policy never ran")
	}
}

// TestIntegrationRetentionWithNoPolicyKeepsEverything: enforcement is ON, the
// plan says nothing. NULL retention_days means forever.
func TestIntegrationRetentionWithNoPolicyKeepsEverything(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -400)
	for seq := int64(1); seq <= 3; seq++ {
		h.seedEvent(seq, old)
	}

	rep := h.runOnce(h.sweeper(retentionOn(500)))
	if rep.Retention.EventsDeleted != 0 {
		t.Fatalf("deleted %d events under a plan with no retention_days; NULL means forever", rep.Retention.EventsDeleted)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 3 {
		t.Fatalf("%d events remain, want 3", n)
	}
	if got := h.teamFloor(); got != 0 {
		t.Errorf("the floor moved to %d for a team with no policy", got)
	}
}

// TestIntegrationRetentionIsRateLimitedByTheDatabase: the second pass inside
// the interval does nothing, because last_retention_sweep_at says the first
// one just ran. That is what keeps three pods on a 30s loop from re-scanning
// every team every half minute.
func TestIntegrationRetentionIsRateLimitedByTheDatabase(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.setRetentionDays(7)
	for seq := int64(1); seq <= 4; seq++ {
		h.seedEvent(seq, now.AddDate(0, 0, -30))
	}
	h.seedEvent(5, now)

	opts := retentionOn(500)
	opts.RetentionInterval = time.Hour

	first := h.runOnce(h.sweeper(opts))
	if first.Retention.TeamsEnforced != 1 {
		t.Fatalf("the first pass enforced %d teams, want 1", first.Retention.TeamsEnforced)
	}
	second := h.runOnce(h.sweeper(opts))
	if second.Retention.TeamsEnforced != 0 {
		t.Fatalf("the second pass inside the interval enforced %d teams, want 0", second.Retention.TeamsEnforced)
	}
}

// TestIntegrationRetentionEmptyLogLeavesAServeableFloor covers the edge the
// COALESCE exists for: every event is gone, so the first cursor that can
// still be served is the next sequence that will be written.
func TestIntegrationRetentionEmptyLogLeavesAServeableFloor(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.setRetentionDays(1)
	for seq := int64(1); seq <= 3; seq++ {
		h.seedEvent(seq, now.AddDate(0, 0, -30))
	}

	rep := h.runOnce(h.sweeper(retentionOn(500)))
	if rep.Retention.EventsDeleted != 3 {
		t.Fatalf("deleted %d events, want 3", rep.Retention.EventsDeleted)
	}
	if _, ok := h.actualMinSequence(); ok {
		t.Fatal("events remain; the fixture did not empty the log")
	}
	var teamSeq int64
	if err := h.db.QueryRow("SELECT `sequence` FROM `team` WHERE `id` = ?", h.teamUUID.String()).Scan(&teamSeq); err != nil {
		t.Fatalf("reading team.sequence: %v", err)
	}
	if got := h.teamFloor(); got != teamSeq+1 {
		t.Errorf("with an empty log the floor is %d, want sequence+1 = %d; "+
			"every cursor at or below %d has missed events that no longer exist", got, teamSeq+1, teamSeq)
	}
}

// TestIntegrationLeaseSkipsTheSecondPod: with the lease on, a pass that
// cannot take the advisory lock returns immediately having done nothing,
// rather than duplicating the first pod's work.
func TestIntegrationLeaseSkipsTheSecondPod(t *testing.T) {
	h := newHarness(t)

	opts := Options{Lease: true, LeaseName: "metiche:sweeper:test:" + h.teamUUID.String()[:8]}
	s := h.sweeper(opts)

	release, got, err := s.acquireLease(context.Background())
	if err != nil {
		t.Fatalf("taking the lease by hand: %v", err)
	}
	if !got {
		t.Fatal("could not take a freshly named lease")
	}
	defer release()

	other := h.sweeper(opts)
	rep, err := other.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("the second sweeper failed instead of skipping: %v", err)
	}
	if !rep.Skipped {
		t.Fatal("a second pod ran the pass while another held the lease")
	}
}

// ─────────────────────────────────────────────
// (e) Login sweep: expired sign-in links and browser sessions
// ─────────────────────────────────────────────
//
// BOARD_LOGIN.md §3.3. These rows are credentials, not history, so the sweep
// runs with RetentionEnabled=false (the zero Options), and its own switch,
// LoginSweepEnabled, defaults to on even when the YAML omits the key.

// newLoginHarness is newHarness plus a clean slate for the two login tables.
// truncateAll predates them and does not list them, so rows from a previous
// test would otherwise survive.
func newLoginHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.exec("DELETE FROM `board_login_link`")
	h.exec("DELETE FROM `browser_session`")
	return h
}

type loginLinkSeed struct {
	expiresAt  time.Time
	consumedAt *time.Time
}

// seedLoginLinks inserts the links in multi-row INSERTs of at most 200 rows
// and returns their ids in order.
func (h *harness) seedLoginLinks(links []loginLinkSeed) []string {
	h.t.Helper()
	ids := make([]string, 0, len(links))
	for start := 0; start < len(links); start += 200 {
		end := start + 200
		if end > len(links) {
			end = len(links)
		}
		var (
			rows []string
			args []any
		)
		for _, l := range links[start:end] {
			lid := id()
			ids = append(ids, lid)
			var consumed any
			if l.consumedAt != nil {
				consumed = *l.consumedAt
			}
			rows = append(rows, "(?,?,?,?,?,?,?,?)")
			args = append(args, lid, h.accountUUID.String(), h.agentUUID.String(),
				strings.ReplaceAll(lid, "-", ""), "/t/test", int64(enums.BOARD_LINK_SOURCE_CLI),
				l.expiresAt, consumed)
		}
		h.exec("INSERT INTO `board_login_link` (`id`,`account_uuid`,`agent_uuid`,`secret_hash`,`redirect_path`,`requested_via`,`expires_at`,`consumed_at`) VALUES "+
			strings.Join(rows, ","), args...)
	}
	return ids
}

type browserSessionSeed struct {
	expiresAt  time.Time
	lastSeenAt *time.Time
	revokedAt  *time.Time
}

// seedBrowserSessions inserts the sessions in multi-row INSERTs of at most 200
// rows and returns their ids in order.
func (h *harness) seedBrowserSessions(sessions []browserSessionSeed) []string {
	h.t.Helper()
	ids := make([]string, 0, len(sessions))
	for start := 0; start < len(sessions); start += 200 {
		end := start + 200
		if end > len(sessions) {
			end = len(sessions)
		}
		var (
			rows []string
			args []any
		)
		for _, s := range sessions[start:end] {
			sid := id()
			ids = append(ids, sid)
			var lastSeen, revoked, reason any
			if s.lastSeenAt != nil {
				lastSeen = *s.lastSeenAt
			}
			if s.revokedAt != nil {
				revoked = *s.revokedAt
				reason = int64(enums.BROWSER_SESSION_END_REASON_SIGNED_OUT)
			}
			rows = append(rows, "(?,?,?,?,?,?,?,?,?,?)")
			args = append(args, sid, strings.ReplaceAll(sid, "-", ""), h.accountUUID.String(),
				"h"+strings.ReplaceAll(sid, "-", ""), int64(enums.BROWSER_AUTH_METHOD_TERMINAL_LINK),
				h.agentUUID.String(), s.expiresAt, lastSeen, revoked, reason)
		}
		h.exec("INSERT INTO `browser_session` (`id`,`key`,`account_uuid`,`secret_hash`,`auth_method`,`created_from_agent_uuid`,`expires_at`,`last_seen_at`,`revoked_at`,`end_reason`) VALUES "+
			strings.Join(rows, ","), args...)
	}
	return ids
}

func (h *harness) rowExists(table, rowID string) bool {
	h.t.Helper()
	return h.count("SELECT COUNT(*) FROM `"+table+"` WHERE `id` = ?", rowID) == 1
}

func timePtr(t time.Time) *time.Time { return &t }

// TestLoginSweepIsOnByDefault covers the zero-value config. It needs no
// database. The switch must be on for the zero Options, after withDefaults,
// and after Populate from a sweeper YAML block that omits the key, which is
// the path app/worker.go takes. Only an explicit false turns it off.
func TestLoginSweepIsOnByDefault(t *testing.T) {
	if !(Options{}).loginSweepOn() {
		t.Fatal("the zero Options turned the login sweep off")
	}
	d := Options{}.withDefaults()
	if d.LoginSweepEnabled == nil || !*d.LoginSweepEnabled {
		t.Fatal("withDefaults left LoginSweepEnabled unset or false; it must default to true")
	}
	if d.RetentionEnabled {
		t.Fatal("withDefaults turned retention on while defaulting the login sweep")
	}

	populate := func(yaml string) Options {
		t.Helper()
		provider, err := config.NewYAML(config.Source(strings.NewReader(yaml)))
		if err != nil {
			t.Fatalf("building config: %v", err)
		}
		opts := Options{}
		if err := provider.Get("sweeper").Populate(&opts); err != nil {
			t.Fatalf("populating Options: %v", err)
		}
		return opts
	}

	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"no sweeper block", "other:\n  x: 1\n", true},
		{"block omits the key", "sweeper:\n  retentionenabled: false\n  batchsize: 250\n", true},
		{"explicit true", "sweeper:\n  loginsweepenabled: true\n", true},
		{"explicit false", "sweeper:\n  loginsweepenabled: false\n", false},
	}
	for _, c := range cases {
		opts := populate(c.yaml)
		if got := opts.loginSweepOn(); got != c.want {
			t.Errorf("%s: loginSweepOn() = %v after Populate, want %v", c.name, got, c.want)
		}
		eff := New(&core.Implementation{}, zap.NewNop(), opts).Options()
		if got := *eff.LoginSweepEnabled; got != c.want {
			t.Errorf("%s: effective LoginSweepEnabled = %v, want %v", c.name, got, c.want)
		}
		if eff.RetentionEnabled {
			t.Errorf("%s: retention came out enabled", c.name)
		}
	}
}

// TestIntegrationLoginSweepLiveSessionSurvives is the §8 mutation target.
// With the cutoff sign flipped (now + tail instead of now - tail), the live
// session and the unexpired link fall inside the delete and this test fails.
func TestIntegrationLoginSweepLiveSessionSurvives(t *testing.T) {
	h := newLoginHarness(t)
	now := time.Now().UTC()

	live := h.seedBrowserSessions([]browserSessionSeed{{
		expiresAt:  now.Add(29 * 24 * time.Hour),
		lastSeenAt: timePtr(now.Add(-time.Minute)),
	}})[0]
	unexpired := h.seedLoginLinks([]loginLinkSeed{{expiresAt: now.Add(10 * time.Minute)}})[0]

	opts := Options{} // RetentionEnabled=false, LoginSweepEnabled unset
	rep := h.runOnce(h.sweeper(opts))

	if rep.Retention.Enabled {
		t.Fatal("fixture is wrong: retention is enabled")
	}
	if !rep.Logins.Enabled {
		t.Error("the report says the login sweep was disabled under the zero Options")
	}
	if !h.rowExists("browser_session", live) {
		t.Error("a live browser session was deleted by the sweeper")
	}
	if !h.rowExists("board_login_link", unexpired) {
		t.Error("an unexpired sign-in link was deleted by the sweeper")
	}
	if rep.Logins.SessionsDeleted != 0 || rep.Logins.LinksDeleted != 0 {
		t.Errorf("deleted %d sessions and %d links, want 0 and 0", rep.Logins.SessionsDeleted, rep.Logins.LinksDeleted)
	}
}

// TestIntegrationLoginSweepDeletesLinksPastTheTailInBatches seeds more than
// one batch of dead links, with RetentionEnabled=false. Every link expired over
// 24h ago goes, consumed or not, across several LIMIT 500 statements. Links
// inside the tail survive, and so does the unexpired one.
func TestIntegrationLoginSweepDeletesLinksPastTheTailInBatches(t *testing.T) {
	h := newLoginHarness(t)
	now := time.Now().UTC()

	const dead = 1203
	var seeds []loginLinkSeed
	for i := 0; i < dead; i++ {
		s := loginLinkSeed{expiresAt: now.Add(-25*time.Hour - time.Duration(i)*time.Minute)}
		if i%2 == 0 {
			s.consumedAt = timePtr(s.expiresAt.Add(-5 * time.Minute))
		}
		seeds = append(seeds, s)
	}
	h.seedLoginLinks(seeds)

	survivors := h.seedLoginLinks([]loginLinkSeed{
		{expiresAt: now.Add(10 * time.Minute)},                                                 // unexpired
		{expiresAt: now.Add(-23 * time.Hour)},                                                  // expired, inside the tail
		{expiresAt: now.Add(-2 * time.Hour), consumedAt: timePtr(now.Add(-130 * time.Minute))}, // consumed, inside the tail
	})

	opts := Options{}
	if opts.RetentionEnabled {
		t.Fatal("the zero Options enabled retention")
	}
	rep := h.runOnce(h.sweeper(opts))

	if rep.Logins.LinksDeleted != dead {
		t.Errorf("deleted %d links, want %d", rep.Logins.LinksDeleted, dead)
	}
	// 1203 links at 500 a batch is 500+500+203: three link batches, plus at
	// least one (empty) session batch.
	if rep.Logins.Batches < 4 {
		t.Errorf("used %d batches for %d links at LIMIT 500; the delete was not bounded", rep.Logins.Batches, dead)
	}
	for i, sid := range survivors {
		if !h.rowExists("board_login_link", sid) {
			t.Errorf("survivor %d was deleted", i)
		}
	}
	if n := h.count("SELECT COUNT(*) FROM `board_login_link`"); n != len(survivors) {
		t.Errorf("%d links remain, want %d", n, len(survivors))
	}

	// A second pass has nothing left to delete.
	if rep2 := h.runOnce(h.sweeper(opts)); rep2.Logins.LinksDeleted != 0 {
		t.Errorf("the second pass deleted %d more links, want 0", rep2.Logins.LinksDeleted)
	}
}

// TestIntegrationLoginSweepDeletesSessionsPastTheTail covers each of the three
// clauses with a row just past the tail and a row just inside it, plus more
// than one batch of absolutely expired sessions. RetentionEnabled is false
// throughout.
func TestIntegrationLoginSweepDeletesSessionsPastTheTail(t *testing.T) {
	h := newLoginHarness(t)
	now := time.Now().UTC()
	day := 24 * time.Hour
	future := now.Add(20 * day)

	const bulk = 612
	var seeds []browserSessionSeed
	for i := 0; i < bulk; i++ {
		seeds = append(seeds, browserSessionSeed{expiresAt: now.Add(-8*day - time.Duration(i)*time.Minute)})
	}
	h.seedBrowserSessions(seeds)

	gone := h.seedBrowserSessions([]browserSessionSeed{
		{expiresAt: now.Add(-8 * day), lastSeenAt: timePtr(now.Add(-9 * day))},  // absolute expiry 8d ago
		{expiresAt: future, revokedAt: timePtr(now.Add(-8 * day))},              // revoked 8d ago
		{expiresAt: future, lastSeenAt: timePtr(now.Add(-15 * day))},            // idle 15d
		{expiresAt: now.Add(-30 * day), revokedAt: timePtr(now.Add(-31 * day))}, // all at once
	})
	kept := h.seedBrowserSessions([]browserSessionSeed{
		{expiresAt: future, lastSeenAt: timePtr(now.Add(-time.Minute))},        // live
		{expiresAt: now.Add(-6 * day), lastSeenAt: timePtr(now.Add(-6 * day))}, // expired 6d ago, inside the tail
		{expiresAt: future, revokedAt: timePtr(now.Add(-6 * day))},             // revoked 6d ago, inside the tail
		{expiresAt: future, lastSeenAt: timePtr(now.Add(-13 * day))},           // idle 13d, inside 7d+7d
		{expiresAt: future}, // never seen, not expired
	})

	rep := h.runOnce(h.sweeper(Options{}))

	if want := bulk + len(gone); rep.Logins.SessionsDeleted != want {
		t.Errorf("deleted %d sessions, want %d", rep.Logins.SessionsDeleted, want)
	}
	// 616 sessions at 500 a batch is two session batches, plus at least one
	// (empty) link batch.
	if rep.Logins.Batches < 3 {
		t.Errorf("used %d batches for %d sessions at LIMIT 500; the delete was not bounded", rep.Logins.Batches, bulk+len(gone))
	}
	for i, sid := range gone {
		if h.rowExists("browser_session", sid) {
			t.Errorf("session %d past the tail survived", i)
		}
	}
	for i, sid := range kept {
		if !h.rowExists("browser_session", sid) {
			t.Errorf("session %d inside the tail was deleted", i)
		}
	}
	if n := h.count("SELECT COUNT(*) FROM `browser_session`"); n != len(kept) {
		t.Errorf("%d sessions remain, want %d", n, len(kept))
	}
}

// TestIntegrationLoginSweepSwitchOffDeletesNothing: an explicit
// LoginSweepEnabled=false is the only way to stop the sweep.
func TestIntegrationLoginSweepSwitchOffDeletesNothing(t *testing.T) {
	h := newLoginHarness(t)
	now := time.Now().UTC()
	link := h.seedLoginLinks([]loginLinkSeed{{expiresAt: now.Add(-48 * time.Hour)}})[0]
	sess := h.seedBrowserSessions([]browserSessionSeed{{expiresAt: now.Add(-30 * 24 * time.Hour)}})[0]

	off := false
	rep := h.runOnce(h.sweeper(Options{LoginSweepEnabled: &off}))

	if rep.Logins.Enabled {
		t.Error("the report says the login sweep ran with the switch off")
	}
	if rep.Logins.LinksDeleted != 0 || rep.Logins.SessionsDeleted != 0 || rep.Logins.Batches != 0 {
		t.Errorf("with the switch off: %+v, want all zero", rep.Logins)
	}
	if !h.rowExists("board_login_link", link) || !h.rowExists("browser_session", sess) {
		t.Error("a row was deleted with LoginSweepEnabled=false")
	}
}
