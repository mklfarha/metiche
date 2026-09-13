package mcp

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

// The integration half of these runs against a REAL MySQL, for the same
// reason the rest of this suite does: what is being proved — that reading an
// instruction is what marks it delivered, that a second read does not hand it
// over again, and that a report is one event however many times it is sent —
// is a property of a transaction, a row lock and the unique index on
// (team_uuid, idempotency_key). A fake would prove that the fake works.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&multiStatements=true&interpolateParams=true' \
//	  go test ./app/mcp/ -run Instruction -v -p 1
//
// No DSN is committed anywhere in this repository.

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

// responseTokenBudgetChars is PLAN.md's hard cap of 700 tokens on any
// response, in characters at the conventional ~4 chars/token. Asserted on the
// bytes the agent actually receives, envelope included.
const responseTokenBudgetChars = 700 * 4

func getInstructions(t *testing.T, hs *harness, c *caller, args GetInstructionsParams) (InstructionsResult, string) {
	t.Helper()
	res, _, err := hs.h.GetInstructions(c.ctx, nil, args)
	if err != nil {
		t.Fatalf("get_instructions(%s): %v", args.SessionKey, err)
	}
	var out InstructionsResult
	decodeResult(t, res, &out)
	return out, resultText(t, res)
}

// seedInstruction writes one instruction straight into the table, the way the
// board's nudge endpoint and the other detectors will. Used where the test
// needs MORE notices than one collision produces.
func seedInstruction(t *testing.T, hs *harness, sessionUUID string, agentUUID uuid.UUID, key, body string, at time.Time) {
	t.Helper()
	if _, err := hs.core.DB().Exec(
		"INSERT INTO `instruction` (`id`,`team_uuid`,`target_session_uuid`,`target_agent_uuid`,`key`,"+
			"`source`,`kind`,`body`,`requires_report`,`status`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		uuid.Must(uuid.NewV4()).String(), hs.teamID.String(), sessionUUID, agentUUID.String(), key,
		enums.INSTRUCTION_SOURCE_SERVER, enums.INSTRUCTION_KIND_FYI, body, true,
		enums.INSTRUCTION_STATUS_PENDING, at, at); err != nil {
		t.Fatalf("seeding instruction %s: %v", key, err)
	}
}

func instructionRow(t *testing.T, hs *harness, key string) (status int64, action sql.NullInt64, note sql.NullString, deliveredAt sql.NullTime, actedAt sql.NullTime) {
	t.Helper()
	if err := hs.core.DB().QueryRow(
		"SELECT `status`, `action`, `action_note`, `delivered_at`, `acted_at` FROM `instruction` "+
			"WHERE `team_uuid` = ? AND `key` = ?", hs.teamID.String(), key).
		Scan(&status, &action, &note, &deliveredAt, &actedAt); err != nil {
		t.Fatalf("reading instruction %s: %v", key, err)
	}
	return
}

// collide runs the real loop up to the point where one agent has a conflict
// notice waiting: Ana declares, Bob declares over the top of her, and Ana —
// who cannot be pushed to — has an instruction pending.
func collide(t *testing.T, hs *harness) (ana, bob *caller, anaKey, bobKey string) {
	t.Helper()
	withDetector(t, hs)
	ana = hs.join(t, "Ana", "client-ana")
	bob = hs.join(t, "Bob", "client-bob")
	anaKey = startWork(t, hs, ana, "feat/auth", "build the login endpoint")
	bobKey = startWork(t, hs, bob, "feat/login", "build the login UI")

	declare(t, hs, ana, DeclareIntentParams{
		SessionKey: anaKey,
		Summary:    "add the POST /api/login handler and its token refresh",
		Paths:      []string{"internal/auth/token.go"},
		Mode:       "write",
	})
	second := declare(t, hs, bob, DeclareIntentParams{
		SessionKey: bobKey,
		Summary:    "wire the login form to the auth endpoint",
		Paths:      []string{"internal/auth/**"},
		Mode:       "write",
	})
	if len(second.Conflicts) != 1 {
		t.Fatalf("the collision did not happen: %+v", second.Conflicts)
	}
	return ana, bob, anaKey, bobKey
}

// ─────────────────────────────────────────────
// (1) Delivery, and only to the right session
// ─────────────────────────────────────────────

// TestIntegrationInstructionReachesOnlyItsOwnSession is the whole point of
// the tool: the agent a conflict was raised AGAINST collects it, the agent
// that caused it (and was already told synchronously) collects nothing, and
// what comes back is actionable rather than merely true.
func TestIntegrationInstructionReachesOnlyItsOwnSession(t *testing.T) {
	hs := newHarness(t)
	ana, bob, anaKey, bobKey := collide(t, hs)

	out, raw := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey})
	t.Logf("Ana's get_instructions -> %s", raw)

	if len(out.Instructions) != 1 {
		t.Fatalf("Ana was handed %d instruction(s), want exactly 1", len(out.Instructions))
	}
	got := out.Instructions[0]
	if !strings.HasPrefix(got.Key, "IN-") {
		t.Errorf("instruction key = %q, want an IN- key", got.Key)
	}
	if got.Kind != "conflict_notice" {
		t.Errorf("kind = %q, want conflict_notice", got.Kind)
	}
	// What happened, who is involved, and what to do about it — PLAN.md's
	// rule is that the last of those is never absent.
	if strings.TrimSpace(got.What) == "" {
		t.Error("the instruction does not say what happened")
	}
	if !strings.Contains(got.With, "Bob") {
		t.Errorf("with = %q, want it to name the other agent", got.With)
	}
	if !strings.Contains(got.With, "feat/login") {
		t.Errorf("with = %q, want it to name their branch, which is what makes it actionable", got.With)
	}
	if strings.TrimSpace(got.SuggestedAction) == "" {
		t.Fatal("a conflict was surfaced with no suggested next action, which is the definition of noise here")
	}
	if got.Severity != "high" {
		t.Errorf("severity = %q, want high", got.Severity)
	}
	if len(got.Paths) != 1 || got.Paths[0] != "internal/auth/token.go" {
		t.Errorf("paths = %v, want the one file the two claims meet on", got.Paths)
	}
	if !strings.HasPrefix(got.Ref, "CF-") {
		t.Errorf("ref = %q, want the conflict key to act on", got.Ref)
	}
	if got.From != "metiche" {
		t.Errorf("from = %q, want metiche for something the server detected", got.From)
	}
	t.Logf("what:   %s", got.What)
	t.Logf("with:   %s", got.With)
	t.Logf("action: %s", got.SuggestedAction)

	// The envelope still rides along, with the counts now reflecting the
	// delivery: nothing left pending, but the conflict is still open.
	if out.Pending.Instructions != 0 {
		t.Errorf("pending.instructions = %d after delivery, want 0", out.Pending.Instructions)
	}
	if out.Pending.Conflicts != 1 {
		t.Errorf("pending.conflicts = %d, want 1 — delivery does not resolve the conflict", out.Pending.Conflicts)
	}
	if out.Sequence == 0 {
		t.Error("the response carries no sequence")
	}
	if out.MoreWaiting != 0 || out.Truncated {
		t.Errorf("more_waiting = %d truncated = %v, want nothing left", out.MoreWaiting, out.Truncated)
	}

	// PLAN.md budgets a hard cap of ~700 tokens on any response.
	if len(raw) > responseTokenBudgetChars {
		t.Errorf("the response is %d characters, over the ~700 token budget", len(raw))
	}
	t.Logf("response size: %d characters (~%d tokens), budget ~700 tokens", len(raw), len(raw)/4)

	// Reading is the delivery receipt.
	status, _, _, delivered, _ := instructionRow(t, hs, got.Key)
	if status != int64(enums.INSTRUCTION_STATUS_DELIVERED) {
		t.Errorf("instruction status = %d after being read, want delivered (%d)", status, enums.INSTRUCTION_STATUS_DELIVERED)
	}
	if !delivered.Valid {
		t.Error("delivered_at was not stamped, so nobody can see the notice reached the agent")
	}

	// ── And the other agent gets NOTHING ────────────────────────────────
	other, otherRaw := getInstructions(t, hs, bob, GetInstructionsParams{SessionKey: bobKey})
	if len(other.Instructions) != 0 {
		t.Fatalf("the other session was handed %d instruction(s) that were not addressed to it: %s",
			len(other.Instructions), otherRaw)
	}
	if other.Pending.Instructions != 0 {
		t.Errorf("the other session has %d pending instruction(s), want 0", other.Pending.Instructions)
	}
	t.Logf("Bob's get_instructions -> %s", otherRaw)
}

// ─────────────────────────────────────────────
// (2) Once, and only once
// ─────────────────────────────────────────────

// TestIntegrationGetInstructionsDoesNotRedeliver proves the receipt actually
// consumes. A notice handed over twice is indistinguishable from two notices,
// and an agent that acts on the second one redoes work it already did.
func TestIntegrationGetInstructionsDoesNotRedeliver(t *testing.T) {
	hs := newHarness(t)
	ana, _, anaKey, _ := collide(t, hs)

	first, _ := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey})
	if len(first.Instructions) != 1 {
		t.Fatalf("first call handed over %d, want 1", len(first.Instructions))
	}
	key := first.Instructions[0].Key
	_, _, _, firstDelivered, _ := instructionRow(t, hs, key)

	second, raw := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey})
	if len(second.Instructions) != 0 {
		t.Fatalf("the second call re-delivered %d instruction(s): %s", len(second.Instructions), raw)
	}
	if second.Pending.Instructions != 0 {
		t.Errorf("pending.instructions = %d on the second call, want 0", second.Pending.Instructions)
	}
	if !strings.Contains(second.Note, "nothing waiting") {
		t.Errorf("note = %q, want it to say plainly that there is nothing waiting", second.Note)
	}
	t.Logf("second get_instructions -> %s", raw)

	// The escape hatch for the failure this design deliberately accepts: a
	// response lost on the way back leaves an instruction marked delivered
	// that the agent never saw, and include_delivered is how it gets it.
	again, againRaw := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey, IncludeDelivered: true})
	if len(again.Instructions) != 1 || again.Instructions[0].Key != key {
		t.Fatalf("include_delivered did not hand back the delivered instruction: %s", againRaw)
	}
	_, _, _, againDelivered, _ := instructionRow(t, hs, key)
	if !againDelivered.Time.Equal(firstDelivered.Time) {
		t.Errorf("delivered_at moved on a re-read (%v -> %v); it must keep saying when the agent FIRST got it",
			firstDelivered.Time, againDelivered.Time)
	}
	t.Logf("include_delivered re-read -> %s", againRaw)
}

// ─────────────────────────────────────────────
// (3) The cap and the "more remain" signal
// ─────────────────────────────────────────────

// TestIntegrationInstructionCapAndMoreRemain: a session with a backlog gets a
// bounded batch, is told how many are left, and the ones it did not get stay
// PENDING rather than being quietly marked delivered.
func TestIntegrationInstructionCapAndMoreRemain(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)
	ana := hs.join(t, "Ana", "client-ana")
	anaKey := startWork(t, hs, ana, "feat/auth", "build the login endpoint")
	anaSession := sessionUUIDFor(t, hs, anaKey)

	base := time.Now().UTC().Add(-time.Hour)
	const seeded = 7
	for i := 0; i < seeded; i++ {
		seedInstruction(t, hs, anaSession, ana.agent.ID,
			fmt.Sprintf("IN-90%d", i),
			fmt.Sprintf("notice number %d: the sweeper found nobody producing the contract you consume", i),
			base.Add(time.Duration(i)*time.Minute))
	}

	// Explicit limit.
	out, raw := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey, Limit: 2})
	if len(out.Instructions) != 2 {
		t.Fatalf("limit=2 handed over %d instruction(s): %s", len(out.Instructions), raw)
	}
	if out.MoreWaiting != seeded-2 {
		t.Errorf("more_waiting = %d, want %d", out.MoreWaiting, seeded-2)
	}
	if !out.Truncated {
		t.Error("truncated = false while 5 remain")
	}
	if !strings.Contains(out.Note, "more waiting") {
		t.Errorf("note = %q, want it to tell the agent to call again", out.Note)
	}
	// Oldest first, so nothing starves behind a drip of new ones.
	if out.Instructions[0].Key != "IN-900" || out.Instructions[1].Key != "IN-901" {
		t.Errorf("got %s, %s; want the two oldest", out.Instructions[0].Key, out.Instructions[1].Key)
	}
	// Every one of them still carries an action, including a plain FYI.
	for _, in := range out.Instructions {
		if strings.TrimSpace(in.SuggestedAction) == "" {
			t.Errorf("%s was delivered with no suggested action", in.Key)
		}
	}
	t.Logf("limit=2 -> %s", raw)

	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ? AND `status` = ?",
		anaSession, enums.INSTRUCTION_STATUS_PENDING); n != seeded-2 {
		t.Errorf("%d instructions still pending, want %d — the undelivered ones must not be consumed", n, seeded-2)
	}

	// The default cap, with no limit given.
	next, nextRaw := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey})
	if len(next.Instructions) != InstructionsDefaultLimit {
		t.Errorf("the default handed over %d, want %d", len(next.Instructions), InstructionsDefaultLimit)
	}
	if next.MoreWaiting != seeded-2-InstructionsDefaultLimit {
		t.Errorf("more_waiting = %d, want %d", next.MoreWaiting, seeded-2-InstructionsDefaultLimit)
	}
	if len(nextRaw) > responseTokenBudgetChars {
		t.Errorf("a full default batch is %d characters, over the ~700 token budget", len(nextRaw))
	}
	t.Logf("default batch -> %d instruction(s), %d more waiting, %d characters (~%d tokens)",
		len(next.Instructions), next.MoreWaiting, len(nextRaw), len(nextRaw)/4)

	// The ceiling holds even when the caller asks for more than it.
	last, _ := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey, Limit: 500})
	if len(last.Instructions) != seeded-2-InstructionsDefaultLimit {
		t.Errorf("limit=500 handed over %d, want the %d that were left", len(last.Instructions), seeded-2-InstructionsDefaultLimit)
	}
	if last.MoreWaiting != 0 || last.Truncated {
		t.Errorf("more_waiting = %d truncated = %v, want the backlog cleared", last.MoreWaiting, last.Truncated)
	}
}

// TestIntegrationInstructionBudgetCutsTheBatch: the cut order's last step —
// items past the token budget are not delivered at all rather than delivered
// truncated. They stay PENDING and are reported as more_waiting.
func TestIntegrationInstructionBudgetCutsTheBatch(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)
	ana := hs.join(t, "Ana", "client-ana")
	anaKey := startWork(t, hs, ana, "feat/auth", "build the login endpoint")
	anaSession := sessionUUIDFor(t, hs, anaKey)

	// Ten long notices: each one renders to ~250 characters, so the budget
	// bites before the limit does.
	long := strings.Repeat("a contract nobody is producing and a very long explanation of it ", 9)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 10; i++ {
		seedInstruction(t, hs, anaSession, ana.agent.ID, fmt.Sprintf("IN-95%d", i), long, base.Add(time.Duration(i)*time.Minute))
	}

	out, raw := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey, Limit: InstructionsMaxLimit})
	if len(out.Instructions) == 0 {
		t.Fatal("nothing was delivered at all")
	}
	if len(out.Instructions) >= 10 {
		t.Errorf("the budget did not cut the batch: %d delivered", len(out.Instructions))
	}
	if out.MoreWaiting != 10-len(out.Instructions) {
		t.Errorf("more_waiting = %d, want %d", out.MoreWaiting, 10-len(out.Instructions))
	}
	if len(raw) > responseTokenBudgetChars {
		t.Errorf("the response is %d characters, over the ~700 token budget", len(raw))
	}
	// Nothing was delivered half: every body is whole, up to the field cap.
	for _, in := range out.Instructions {
		if len([]rune(in.What)) > instructionTextChars {
			t.Errorf("%s: what is %d runes, over the field cap", in.Key, len([]rune(in.What)))
		}
	}
	t.Logf("budget cut a 10-notice backlog to %d, %d more waiting, %d characters (~%d tokens)",
		len(out.Instructions), out.MoreWaiting, len(raw), len(raw)/4)
}

// ─────────────────────────────────────────────
// (4) report_back, twice
// ─────────────────────────────────────────────

// TestIntegrationReportBackTwiceWritesOneEvent is the idempotency proof. The
// most likely double report is a retry after a response the agent never
// received, so the promise cannot depend on the agent sending a key.
func TestIntegrationReportBackTwiceWritesOneEvent(t *testing.T) {
	hs := newHarness(t)
	ana, _, anaKey, _ := collide(t, hs)

	out, _ := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey})
	if len(out.Instructions) != 1 {
		t.Fatalf("expected one instruction to report on, got %d", len(out.Instructions))
	}
	key := out.Instructions[0].Key

	args := ReportBackParams{
		SessionKey:     anaKey,
		InstructionKey: key,
		Outcome:        "done",
		Note:           "dropped internal/auth/token.go; Bob has it",
	}
	first, _, err := hs.h.ReportBack(ana.ctx, nil, args)
	if err != nil {
		t.Fatalf("report_back: %v", err)
	}
	second, _, err := hs.h.ReportBack(ana.ctx, nil, args)
	if err != nil {
		t.Fatalf("report_back retried: %v", err)
	}
	a, b := resultText(t, first), resultText(t, second)
	if a != b {
		t.Errorf("the retry answered differently:\nfirst:  %s\nsecond: %s", a, b)
	}
	t.Logf("both report_back calls returned: %s", a)

	events := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), enums.EVENT_KIND_INSTRUCTION_ACTED)
	if events != 1 {
		t.Fatalf("reporting the same outcome twice wrote %d events, want exactly 1", events)
	}

	status, action, note, delivered, acted := instructionRow(t, hs, key)
	if status != int64(enums.INSTRUCTION_STATUS_ACTED) {
		t.Errorf("status = %d, want acted (%d)", status, enums.INSTRUCTION_STATUS_ACTED)
	}
	if action.Int64 != int64(enums.INSTRUCTION_ACTION_COMPLIED) {
		t.Errorf("action = %d, want complied (%d)", action.Int64, enums.INSTRUCTION_ACTION_COMPLIED)
	}
	if !strings.HasPrefix(note.String, "done:") {
		t.Errorf("action_note = %q, want the exact outcome word kept on the front of it", note.String)
	}
	if !acted.Valid || !delivered.Valid {
		t.Errorf("acted_at valid = %v, delivered_at valid = %v; both must be stamped", acted.Valid, delivered.Valid)
	}
	t.Logf("instruction %s: status=acted action=complied note=%q", key, note.String)

	// A DIFFERENT outcome later is news, and is a second event on purpose.
	if _, _, err := hs.h.ReportBack(ana.ctx, nil, ReportBackParams{
		SessionKey: anaKey, InstructionKey: key, Outcome: "blocked",
		Note: "Bob has not landed his change yet",
	}); err != nil {
		t.Fatalf("changing the outcome: %v", err)
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), enums.EVENT_KIND_INSTRUCTION_ACTED); n != 2 {
		t.Errorf("%d instruction_acted events after a genuinely new outcome, want 2", n)
	}

	// And a refusal with nothing said is refused: it leaves the person who
	// raised it exactly as stuck as silence would.
	if _, _, err := hs.h.ReportBack(ana.ctx, nil, ReportBackParams{
		SessionKey: anaKey, InstructionKey: key, Outcome: "refused",
	}); err == nil {
		t.Error("report_back accepted a refusal with no reason")
	} else {
		t.Logf("refusal with no note: %v", err)
	}
}

// ─────────────────────────────────────────────
// (5) Somebody else's instructions
// ─────────────────────────────────────────────

// TestIntegrationAgentCannotReachAnotherSessionsInstructions is the security
// case. Instruction and session keys are short and guessable (IN-3, S-2) and
// the endpoint is public, so the scope has to be enforced on every call, not
// assumed from the key.
func TestIntegrationAgentCannotReachAnotherSessionsInstructions(t *testing.T) {
	hs := newHarness(t)
	ana, bob, anaKey, bobKey := collide(t, hs)

	var anaInstruction string
	if err := hs.core.DB().QueryRow(
		"SELECT `key` FROM `instruction` WHERE `target_session_uuid` = ?",
		sessionUUIDFor(t, hs, anaKey)).Scan(&anaInstruction); err != nil {
		t.Fatalf("no instruction was queued for Ana: %v", err)
	}

	// (a) Bob cannot point get_instructions at Ana's session.
	if _, _, err := hs.h.GetInstructions(bob.ctx, nil, GetInstructionsParams{SessionKey: anaKey}); err == nil {
		t.Fatal("Bob read another session's instructions")
	} else {
		t.Logf("Bob calling get_instructions on Ana's session: %v", err)
	}

	// (b) Nor answer, and so silence, an instruction addressed to her.
	if _, _, err := hs.h.ReportBack(bob.ctx, nil, ReportBackParams{
		SessionKey: bobKey, InstructionKey: anaInstruction, Outcome: "done", Note: "not mine to close",
	}); err == nil {
		t.Fatal("Bob reported back on an instruction addressed to another session")
	} else if !strings.Contains(err.Error(), "not sent to this session") {
		t.Errorf("the refusal should say why: %v", err)
	} else {
		t.Logf("Bob calling report_back on Ana's instruction: %v", err)
	}

	// (c) Neither attempt consumed it, and neither wrote an event.
	status, _, _, delivered, _ := instructionRow(t, hs, anaInstruction)
	if status != int64(enums.INSTRUCTION_STATUS_PENDING) {
		t.Errorf("Ana's instruction is %d after two attempts by somebody else, want still pending (%d)",
			status, enums.INSTRUCTION_STATUS_PENDING)
	}
	if delivered.Valid {
		t.Error("Ana's instruction was stamped delivered by somebody else's call")
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), enums.EVENT_KIND_INSTRUCTION_ACTED); n != 0 {
		t.Errorf("%d instruction_acted events, want 0", n)
	}

	// And it is still there for its actual owner.
	out, _ := getInstructions(t, hs, ana, GetInstructionsParams{SessionKey: anaKey})
	if len(out.Instructions) != 1 || out.Instructions[0].Key != anaInstruction {
		t.Fatalf("Ana can no longer collect her own instruction: %+v", out.Instructions)
	}
	t.Logf("Ana still collects %s herself", anaInstruction)
}

// ─────────────────────────────────────────────
// Unit: the rule that a notice always carries an action
// ─────────────────────────────────────────────

// TestSplitNoticeBodyFindsTheIncumbentsAction covers the one coupling in this
// file: the body format detector.go writes. The incumbent's action exists
// nowhere else — conflict.suggested_action holds the OTHER side's action — so
// this parse is load-bearing, and its failure mode has to be soft.
func TestSplitNoticeBodyFindsTheIncumbentsAction(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantWhat   string
		wantAction string
		wantOK     bool
	}{
		{
			name:       "detector body with their summary",
			body:       "CF-3 (high) on internal/auth/token.go: Bob (claude-bob) is now in internal/auth/** on feat/login; narrow your claim or agree who takes the file. They said they are about to: wire the login form",
			wantWhat:   "CF-3 (high) on internal/auth/token.go — they are about to: wire the login form",
			wantAction: "Bob (claude-bob) is now in internal/auth/** on feat/login; narrow your claim or agree who takes the file.",
			wantOK:     true,
		},
		{
			name:       "detector body without one",
			body:       "CF-9 (medium) on go.mod: regenerate after merge, do not hand-resolve",
			wantWhat:   "CF-9 (medium) on go.mod",
			wantAction: "regenerate after merge, do not hand-resolve",
			wantOK:     true,
		},
		{
			name:     "a human's prose, which has no shape at all",
			body:     "stop what you are doing and come talk to me",
			wantWhat: "stop what you are doing and come talk to me",
			wantOK:   false,
		},
		{
			name:     "a colon with nothing after it",
			body:     "CF-1 (low) on x.go: ",
			wantWhat: "CF-1 (low) on x.go: ",
			wantOK:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			what, action, ok := splitNoticeBody(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (what=%q action=%q)", ok, tc.wantOK, what, action)
			}
			if what != tc.wantWhat {
				t.Errorf("what  = %q\nwant    %q", what, tc.wantWhat)
			}
			if action != tc.wantAction {
				t.Errorf("action = %q\nwant     %q", action, tc.wantAction)
			}
		})
	}
}

// TestEveryInstructionKindHasAnAction: the guarantee behind "never surface a
// conflict without a suggested next action". A kind added later with no case
// here still gets the default, and the default names a tool.
func TestEveryInstructionKindHasAnAction(t *testing.T) {
	kinds := []enums.InstructionKind{
		enums.INSTRUCTION_KIND_STOP,
		enums.INSTRUCTION_KIND_STEER,
		enums.INSTRUCTION_KIND_CONFLICT_NOTICE,
		enums.INSTRUCTION_KIND_JUDGE_REQUEST,
		enums.INSTRUCTION_KIND_QUESTION,
		enums.INSTRUCTION_KIND_FYI,
		enums.InstructionKind(99),
	}
	for _, k := range kinds {
		got := fallbackAction(k, "IN-4", "")
		if strings.TrimSpace(got) == "" {
			t.Errorf("kind %d has no fallback action", k)
			continue
		}
		if !strings.Contains(got, "IN-4") {
			t.Errorf("kind %d: %q does not name the instruction to answer", k, got)
		}
		// report_back is the only tool every kind can be answered with today;
		// an action naming a tool the server does not register is worse than
		// none.
		if !strings.Contains(got, "report_back") {
			t.Errorf("kind %d: %q names no tool, so it is a sentence rather than an action", k, got)
		}
	}
}

// TestParseReportOutcome covers the mapping onto the four values the column
// can hold, including the two words that share one of them.
func TestParseReportOutcome(t *testing.T) {
	cases := []struct {
		in     string
		word   string
		action enums.InstructionAction
	}{
		{"done", "done", enums.INSTRUCTION_ACTION_COMPLIED},
		{"COMPLIED", "done", enums.INSTRUCTION_ACTION_COMPLIED},
		{" acknowledged ", "acknowledged", enums.INSTRUCTION_ACTION_PARTIAL},
		{"refused", "refused", enums.INSTRUCTION_ACTION_REFUSED},
		{"blocked", "blocked", enums.INSTRUCTION_ACTION_PARTIAL},
		{"n/a", "not_applicable", enums.INSTRUCTION_ACTION_NOT_APPLICABLE},
	}
	for _, tc := range cases {
		word, action, err := parseReportOutcome(tc.in)
		if err != nil {
			t.Errorf("parseReportOutcome(%q): %v", tc.in, err)
			continue
		}
		if word != tc.word || action != tc.action {
			t.Errorf("parseReportOutcome(%q) = %q/%d, want %q/%d", tc.in, word, action, tc.word, tc.action)
		}
	}
	// "blocked" and "acknowledged" share INSTRUCTION_ACTION_PARTIAL, which is
	// exactly why the word itself is kept on the note.
	if _, _, err := parseReportOutcome(""); err == nil {
		t.Error("an empty outcome was accepted")
	}
	if _, _, err := parseReportOutcome("maybe"); err == nil {
		t.Error("an unknown outcome was accepted")
	}
}
