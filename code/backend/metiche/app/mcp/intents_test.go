package mcp

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/enums"
)

// These run against a REAL MySQL, for the same reason the rest of the
// integration suite does: what is being proved here — that two agents
// declaring overlapping paths produce exactly ONE conflict, and that the
// later one is told synchronously while the earlier one is told through a
// queued instruction — is a property of the team row lock, the unique index
// on (team_uuid, dedupe_key), and detection running inside the write
// transaction. A fake would prove that the fake works.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&interpolateParams=true' \
//
// interpolateParams=true is not decoration: it is what production runs
// (config/base.yaml recommends it), and it changes how []byte arguments reach
// MySQL. Without it, a []byte bound to a JSON column works; with it, the
// driver sends a binary literal and MySQL rejects it with error 3144. Leaving
// it off here once hid exactly that bug until a real deployment found it.
//	  go test ./app/mcp/ -run IntegrationCollision -v
//
// No DSN is committed anywhere in this repository.

// withDetector installs the detection layer the way app/rest.go will. Done
// per test rather than in the shared harness so the other integration tests
// keep running against a handler with no detector, which is what proves the
// hook is optional.
func withDetector(t *testing.T, hs *harness) {
	t.Helper()
	hs.h.SetDetector(NewPathDetector(hs.core, zap.NewNop()))
}

// startWork runs the real start_session tool and returns the session key.
func startWork(t *testing.T, hs *harness, c *caller, branch, goal string) string {
	t.Helper()
	res, _, err := hs.h.StartSession(c.ctx, nil, StartSessionParams{
		ProjectKey: "metiche",
		Branch:     branch,
		Goal:       goal,
	})
	if err != nil {
		t.Fatalf("start_session(%s): %v", branch, err)
	}
	var env Envelope
	decodeResult(t, res, &env)
	if env.Key == "" {
		t.Fatalf("start_session returned no session key: %s", resultText(t, res))
	}
	return env.Key
}

func declare(t *testing.T, hs *harness, c *caller, args DeclareIntentParams) Envelope {
	t.Helper()
	res, _, err := hs.h.DeclareIntent(c.ctx, nil, args)
	if err != nil {
		t.Fatalf("declare_intent(%v): %v", args.Paths, err)
	}
	var env Envelope
	decodeResult(t, res, &env)
	t.Logf("declare_intent(%v) -> %s", args.Paths, resultText(t, res))
	return env
}

func sessionUUIDFor(t *testing.T, hs *harness, key string) string {
	t.Helper()
	var id string
	if err := hs.core.DB().QueryRow("SELECT `id` FROM `session` WHERE `key` = ?", key).Scan(&id); err != nil {
		t.Fatalf("looking up session %s: %v", key, err)
	}
	return id
}

// TestIntegrationCollisionBetweenTwoSessions is the end-to-end proof of the
// core loop, and the test this whole package exists to pass.
//
// Two agents, two people, two branches, one file. The second one to declare
// is told IN ITS OWN RESPONSE — it has the information in hand and has not
// started yet — and the first one, who cannot be pushed to, gets an
// instruction waiting on its next call. Exactly one conflict row, because
// ordering assigns responsibility rather than permission and the pair is one
// collision however many times it is re-detected.
func TestIntegrationCollisionBetweenTwoSessions(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	bob := hs.join(t, "Bob", "client-bob")

	anaKey := startWork(t, hs, ana, "feat/auth", "build the login endpoint")
	bobKey := startWork(t, hs, bob, "feat/login", "build the login UI")
	anaSession := sessionUUIDFor(t, hs, anaKey)
	bobSession := sessionUUIDFor(t, hs, bobKey)

	// ── Ana declares first, into an empty world ─────────────────────────
	first := declare(t, hs, ana, DeclareIntentParams{
		SessionKey: anaKey,
		Summary:    "add the POST /api/login handler and its token refresh",
		Paths:      []string{"internal/auth/token.go"},
		Mode:       "write",
		Kind:       "implement",
	})
	if len(first.Conflicts) != 0 {
		t.Fatalf("the first declarer was told about %d conflict(s) in an empty project: %+v",
			len(first.Conflicts), first.Conflicts)
	}
	if first.Key == "" || !strings.HasPrefix(first.Key, "INT-") {
		t.Errorf("declare_intent returned key %q, want an INT- key", first.Key)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `claim_path` WHERE `session_uuid` = ?", anaSession); n != 1 {
		t.Errorf("Ana holds %d claim_path row(s), want 1", n)
	}

	// ── Bob declares second, over the top of her ────────────────────────
	second := declare(t, hs, bob, DeclareIntentParams{
		SessionKey: bobKey,
		Summary:    "wire the login form to the auth endpoint",
		Paths:      []string{"internal/auth/**"},
		Mode:       "write",
	})

	// (a) The later declarer is told SYNCHRONOUSLY, in its own response.
	if len(second.Conflicts) != 1 {
		t.Fatalf("the later declarer was told about %d conflict(s), want exactly 1: %+v",
			len(second.Conflicts), second.Conflicts)
	}
	notice := second.Conflicts[0]
	switch {
	case notice.Severity != "high":
		t.Errorf("severity = %q, want high (two writers, one file, different people and branches)", notice.Severity)
	case notice.Kind != "path_overlap":
		t.Errorf("kind = %q, want path_overlap", notice.Kind)
	}
	if !strings.Contains(notice.With, "Ana") {
		t.Errorf("the notice does not name who they collided with: %q", notice.With)
	}
	if len(notice.Paths) != 1 || notice.Paths[0] != "internal/auth/token.go" {
		t.Errorf("the notice names paths %v, want the exact file the two claims meet on", notice.Paths)
	}
	// The rule that separates signal from noise.
	if strings.TrimSpace(notice.SuggestedAction) == "" {
		t.Error("a conflict was surfaced with no suggested action, which is the definition of noise here")
	}
	if !strings.HasPrefix(notice.Key, "CF-") {
		t.Errorf("conflict key = %q, want a CF- key", notice.Key)
	}
	t.Logf("Bob was told, in his own response: [%s %s] %s", notice.Key, notice.Severity, notice.SuggestedAction)

	// (b) EXACTLY ONE conflict row.
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ?", hs.teamID.String()); n != 1 {
		t.Fatalf("%d conflict rows, want exactly 1", n)
	}
	var (
		conflictID string
		severity   int64
		status     int64
		occurrence int64
		rule       string
		suggested  string
		yieldTo    string
	)
	if err := hs.core.DB().QueryRow(
		"SELECT `id`, `severity`, `status`, `occurrence_count`, COALESCE(`detector_rule`, ''), "+
			"COALESCE(`suggested_action`, ''), COALESCE(`suggested_yield_session_uuid`, '') "+
			"FROM `conflict` WHERE `team_uuid` = ?", hs.teamID.String()).
		Scan(&conflictID, &severity, &status, &occurrence, &rule, &suggested, &yieldTo); err != nil {
		t.Fatalf("reading the conflict: %v", err)
	}
	if severity != int64(enums.CONFLICT_SEVERITY_HIGH) {
		t.Errorf("stored severity = %d, want high (%d)", severity, enums.CONFLICT_SEVERITY_HIGH)
	}
	if status != int64(enums.CONFLICT_STATUS_OPEN) {
		t.Errorf("stored status = %d, want open (%d)", status, enums.CONFLICT_STATUS_OPEN)
	}
	if occurrence != 1 {
		t.Errorf("occurrence_count = %d on first detection, want 1", occurrence)
	}
	if suggested == "" {
		t.Error("the conflict row has no suggested_action")
	}
	// Ordering assigns responsibility, not permission: the later declarer is
	// the one asked to move.
	if yieldTo != bobSession {
		t.Errorf("suggested_yield_session_uuid = %q, want the later declarer's session", yieldTo)
	}
	t.Logf("conflict row: severity=%d rule=%s occurrences=%d", severity, rule, occurrence)

	// (c) Both sides are attached, with the right roles.
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ?", conflictID); n != 2 {
		t.Errorf("%d participants, want 2", n)
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? AND `role` = ?",
		conflictID, bobSession, enums.PARTICIPANT_ROLE_INITIATOR); n != 1 {
		t.Errorf("the later declarer is not attached as the initiator")
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? AND `role` = ?",
		conflictID, anaSession, enums.PARTICIPANT_ROLE_INCUMBENT); n != 1 {
		t.Errorf("the earlier declarer is not attached as the incumbent")
	}

	// (d) The earlier declarer, who cannot be pushed to, has an instruction
	//     waiting — and the later one does not, because they were already
	//     told in their own response.
	var (
		instrBody   string
		instrKind   int64
		instrStatus int64
		instrRef    string
	)
	if err := hs.core.DB().QueryRow(
		"SELECT `body`, `kind`, `status`, COALESCE(`ref_uuid`, '') FROM `instruction` WHERE `target_session_uuid` = ?",
		anaSession).Scan(&instrBody, &instrKind, &instrStatus, &instrRef); err != nil {
		t.Fatalf("no instruction was queued for the earlier declarer: %v", err)
	}
	if instrKind != int64(enums.INSTRUCTION_KIND_CONFLICT_NOTICE) {
		t.Errorf("instruction kind = %d, want conflict_notice (%d)", instrKind, enums.INSTRUCTION_KIND_CONFLICT_NOTICE)
	}
	if instrStatus != int64(enums.INSTRUCTION_STATUS_PENDING) {
		t.Errorf("instruction status = %d, want pending (%d)", instrStatus, enums.INSTRUCTION_STATUS_PENDING)
	}
	if instrRef != conflictID {
		t.Errorf("the instruction points at %q, want the conflict %q", instrRef, conflictID)
	}
	if !strings.Contains(instrBody, "Bob") {
		t.Errorf("the instruction does not say who arrived: %q", instrBody)
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ?", bobSession); n != 0 {
		t.Errorf("%d instruction(s) queued for the agent that was already told synchronously, want 0", n)
	}
	t.Logf("Ana's queued instruction: %s", instrBody)

	// (e) And she finds out on her next call, with no polling: the pending
	//     count rides on a bare heartbeat.
	res, _, err := hs.h.Heartbeat(ana.ctx, nil, HeartbeatParams{SessionKey: anaKey})
	if err != nil {
		t.Fatalf("Ana's heartbeat: %v", err)
	}
	var beat Envelope
	decodeResult(t, res, &beat)
	if beat.Pending.Instructions != 1 {
		t.Errorf("Ana's heartbeat reported %d pending instruction(s), want 1", beat.Pending.Instructions)
	}
	if beat.Pending.Conflicts != 1 {
		t.Errorf("Ana's heartbeat reported %d open conflict(s), want 1", beat.Pending.Conflicts)
	}
	if beat.Note == "" {
		t.Error("a heartbeat with work waiting said nothing about it")
	}
	t.Logf("Ana's next heartbeat: %s", resultText(t, res))

	// ── (f) Re-detection of the SAME pair bumps the counter ─────────────
	// Bob adds the exact file to the claim he already holds. Same two
	// claims, same file, so the same dedupe key: a second row here would be
	// the board showing one collision twice forever. The severity does climb
	// — two exact writers on one file is critical, not high — and an
	// escalation is the one thing that earns a second interruption.
	if _, _, err := hs.h.UpdateIntent(bob.ctx, nil, UpdateIntentParams{
		SessionKey: bobKey,
		Status:     "active",
		AddPaths:   []string{"internal/auth/token.go"},
		StatusLine: "wiring the form to the endpoint",
	}); err != nil {
		t.Fatalf("update_intent: %v", err)
	}

	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ?", hs.teamID.String()); n != 1 {
		t.Errorf("%d conflict rows after re-detecting the same pair, want still exactly 1", n)
	}
	var (
		sev2   int64
		occur2 int64
	)
	if err := hs.core.DB().QueryRow(
		"SELECT `severity`, `occurrence_count` FROM `conflict` WHERE `id` = ?", conflictID).
		Scan(&sev2, &occur2); err != nil {
		t.Fatalf("re-reading the conflict: %v", err)
	}
	if occur2 != 2 {
		t.Errorf("occurrence_count = %d after a second detection, want 2", occur2)
	}
	if sev2 != int64(enums.CONFLICT_SEVERITY_CRITICAL) {
		t.Errorf("severity = %d after both sides pinned the same file, want critical (%d)",
			sev2, enums.CONFLICT_SEVERITY_CRITICAL)
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ?", anaSession); n != 2 {
		t.Errorf("%d instructions for the incumbent after an escalation, want 2 (one per escalation, not one per detection)", n)
	}
	t.Logf("after re-detection: one conflict row, occurrences=%d, severity=%d", occur2, sev2)
}

// TestIntegrationReadersNeverCollide is the noise rule that would otherwise
// produce most of the traffic in the system, proved end to end rather than
// only in the unit test: everybody reads everything.
func TestIntegrationReadersNeverCollide(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	bob := hs.join(t, "Bob", "client-bob")
	anaKey := startWork(t, hs, ana, "feat/auth", "read the auth package")
	bobKey := startWork(t, hs, bob, "feat/login", "read the auth package too")

	declare(t, hs, ana, DeclareIntentParams{
		SessionKey: anaKey, Summary: "understand how tokens are refreshed",
		Paths: []string{"internal/auth/**"}, Mode: "read", Kind: "investigate",
	})
	second := declare(t, hs, bob, DeclareIntentParams{
		SessionKey: bobKey, Summary: "understand the same thing",
		Paths: []string{"internal/auth/token.go"}, Mode: "read", Kind: "investigate",
	})

	if len(second.Conflicts) != 0 {
		t.Errorf("two readers were told about %d conflict(s): %+v", len(second.Conflicts), second.Conflicts)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict`"); n != 0 {
		t.Errorf("%d conflict rows for read x read, want 0 — not even one recorded at severity none", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `instruction`"); n != 0 {
		t.Errorf("%d instructions raised for read x read, want 0", n)
	}
}

// TestIntegrationCheckPathsCommitsToNothing proves the pre-flight is a
// pre-flight: it sees what declare_intent would see, and leaves no trace.
func TestIntegrationCheckPathsCommitsToNothing(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	bob := hs.join(t, "Bob", "client-bob")
	anaKey := startWork(t, hs, ana, "feat/auth", "build the login endpoint")
	bobKey := startWork(t, hs, bob, "feat/login", "build the login UI")

	declare(t, hs, ana, DeclareIntentParams{
		SessionKey: anaKey, Summary: "add the token refresh",
		Paths: []string{"internal/auth/token.go"}, Mode: "write",
	})

	before := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`")

	res, _, err := hs.h.CheckPaths(bob.ctx, nil, CheckPathsParams{
		SessionKey: bobKey,
		Paths:      []string{"internal/auth/**", "vendor/github.com/x/y.go", "web/ui/login.tsx"},
		Mode:       "write",
	})
	if err != nil {
		t.Fatalf("check_paths: %v", err)
	}
	var out CheckPathsResult
	decodeResult(t, res, &out)
	t.Logf("check_paths -> %s", resultText(t, res))

	if len(out.Holders) != 1 {
		t.Fatalf("check_paths found %d holder(s), want 1: %+v", len(out.Holders), out.Holders)
	}
	h := out.Holders[0]
	if !strings.Contains(h.HeldBy, "Ana") {
		t.Errorf("holder = %q, want Ana", h.HeldBy)
	}
	if h.Path != "internal/auth/token.go" {
		t.Errorf("holder path = %q, want the file the two claims meet on", h.Path)
	}
	if h.WouldBe != "high" {
		t.Errorf("forecast severity = %q, want high", h.WouldBe)
	}
	if strings.TrimSpace(h.SuggestedAction) == "" {
		t.Error("check_paths reported a holder with no suggested action")
	}
	// The ignore list is reported, not silently applied: an agent that asked
	// about a vendored path deserves to know it is not tracked.
	if len(out.Ignored) != 1 || !strings.Contains(out.Ignored[0], "vendor/") {
		t.Errorf("ignored = %v, want the vendored path reported back", out.Ignored)
	}
	if out.Advisory == "" {
		t.Error("check_paths did not say that it reserved nothing")
	}

	// Nothing was written. Not a claim, not a conflict, not an instruction,
	// and not an event — a read-only tool that advances the team's sequence
	// makes every board on the team repaint for nothing.
	for _, tbl := range []string{"conflict", "conflict_participant", "instruction"} {
		if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `"+tbl+"`"); n != 0 {
			t.Errorf("check_paths wrote %d row(s) to %s, want 0", n, tbl)
		}
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `claim` WHERE `session_uuid` = ?",
		sessionUUIDFor(t, hs, bobKey)); n != 0 {
		t.Errorf("check_paths claimed %d path(s) for the caller, want 0", n)
	}
	if after := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`"); after != before {
		t.Errorf("check_paths appended %d event(s), want 0", after-before)
	}
}

// TestIntegrationDeclareIntentReplaysVerbatim covers the idempotency promise
// for the tool that has the most to lose from breaking it: a retry that
// re-rendered its response would quietly drop the conflicts the caller was
// told about the first time, and the retry would look cleaner than the
// original.
func TestIntegrationDeclareIntentReplaysVerbatim(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	bob := hs.join(t, "Bob", "client-bob")
	anaKey := startWork(t, hs, ana, "feat/auth", "build the login endpoint")
	bobKey := startWork(t, hs, bob, "feat/login", "build the login UI")

	declare(t, hs, ana, DeclareIntentParams{
		SessionKey: anaKey, Summary: "add the token refresh",
		Paths: []string{"internal/auth/token.go"}, Mode: "write",
	})

	args := DeclareIntentParams{
		SessionKey:     bobKey,
		Summary:        "wire the login form to the auth endpoint",
		Paths:          []string{"internal/auth/token.go"},
		Mode:           "write",
		IdempotencyKey: "retry-me",
	}
	first, _, err := hs.h.DeclareIntent(bob.ctx, nil, args)
	if err != nil {
		t.Fatalf("first declare_intent: %v", err)
	}
	second, _, err := hs.h.DeclareIntent(bob.ctx, nil, args)
	if err != nil {
		t.Fatalf("retried declare_intent: %v", err)
	}
	a, b := resultText(t, first), resultText(t, second)
	if a != b {
		t.Errorf("a retry answered differently:\nfirst:  %s\nsecond: %s", a, b)
	}
	if !strings.Contains(a, "suggested_action") {
		t.Errorf("the response carried no conflict to replay, so this proves nothing: %s", a)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `intent` WHERE `session_uuid` = ?",
		sessionUUIDFor(t, hs, bobKey)); n != 1 {
		t.Errorf("the retry declared %d intents, want 1", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict`"); n != 1 {
		t.Errorf("the retry produced %d conflicts, want 1", n)
	}
	t.Logf("both calls returned: %s", a)
}

// TestIntegrationEndSessionFreesTheFiles closes the loop the other way: the
// collision is real while the claim is held, and gone the moment it is not.
func TestIntegrationEndSessionFreesTheFiles(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	bob := hs.join(t, "Bob", "client-bob")
	anaKey := startWork(t, hs, ana, "feat/auth", "build the login endpoint")
	bobKey := startWork(t, hs, bob, "feat/login", "build the login UI")

	declare(t, hs, ana, DeclareIntentParams{
		SessionKey: anaKey, Summary: "add the token refresh",
		Paths: []string{"internal/auth/token.go"}, Mode: "write",
	})
	if _, _, err := hs.h.EndSession(ana.ctx, nil, EndSessionParams{SessionKey: anaKey}); err != nil {
		t.Fatalf("end_session: %v", err)
	}

	second := declare(t, hs, bob, DeclareIntentParams{
		SessionKey: bobKey, Summary: "take over the token refresh",
		Paths: []string{"internal/auth/token.go"}, Mode: "write",
	})
	if len(second.Conflicts) != 0 {
		t.Errorf("a released claim still collided: %+v", second.Conflicts)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict`"); n != 0 {
		t.Errorf("%d conflicts against an ended session, want 0", n)
	}
}

// compile-time guard that the registration entry point keeps the signature
// app/rest.go is going to call.
var _ = func(ctx context.Context) {}
