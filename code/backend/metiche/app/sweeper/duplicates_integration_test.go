package sweeper

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/app/mcp"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// The sweeper's half of docs/DUPLICATES.md (§7.3): the duplicate pairs'
// backlog and judge window, the incumbent's notice after the grace (§4.5),
// the path notice it folds in (§4.6), settlement when a side is abandoned
// (§4.7), and escalation (§4.8).
//
// Every name here is a test name. Ana's agent (S-17, the harness's own
// person) declared INT-83 "add login page" first; Bob's agent (S-22)
// declared INT-92 "build the login screen", judged it the same work, and is
// the yield side of CF-44 — exactly the spec's example. The project's cadence
// is hackathon: grace 2m, escalation budget 10m.
//
// Real MySQL, through the same harness as the rest of the package.

const (
	dupIncumbentKey = "INT-83"
	dupYieldKey     = "INT-92"
	dupConflictKey  = "CF-44"
	dupRationale    = "both build the login page; INT-83 already has the route"
)

// ─────────────────────────────────────────────
// Fixtures
// ─────────────────────────────────────────────

// seedWordedIntent is a plan at a chosen wording_revision.
func (h *harness) seedWordedIntent(session, member uuid.UUID, key, summary string, status enums.IntentStatus, wording int64) uuid.UUID {
	h.t.Helper()
	iid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,"+
		"`revision`,`wording_revision`,`declared_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		iid.String(), h.teamUUID.String(), h.projectUUID.String(), session.String(), member.String(),
		key, summary, int64(enums.INTENT_KIND_IMPLEMENT), int64(status), 1, wording, time.Now().UTC())
	return iid
}

// seedDuplicatePair writes one duplicate_work judgement the way the reviewer
// does: a = the other plan, b = the judge's plan, both at wording_revision.
// judge nil leaves it in the backlog.
func (h *harness) seedDuplicatePair(a uuid.UUID, aRev int64, b uuid.UUID, bRev int64,
	judge *uuid.UUID, expires *time.Time, count int, updatedAt time.Time) uuid.UUID {
	h.t.Helper()
	jid := uuid.Must(uuid.NewV4())
	var judgeArg, expiresArg any
	if judge != nil {
		judgeArg = judge.String()
	}
	if expires != nil {
		expiresArg = *expires
	}
	h.exec("INSERT INTO `judgement` (`id`,`team_uuid`,`pair_key`,`kind`,`subject_a_kind`,`subject_a_uuid`,`subject_a_revision`,"+
		"`subject_b_kind`,`subject_b_uuid`,`subject_b_revision`,`status`,`judge_session_uuid`,`judging_expires_at`,"+
		"`assignment_count`,`pinned`,`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?)",
		jid.String(), h.teamUUID.String(), "dup-"+jid.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		int64(enums.SUBJECT_KIND_INTENT), a.String(), aRev,
		int64(enums.SUBJECT_KIND_INTENT), b.String(), bRev,
		int64(enums.JUDGEMENT_STATUS_PENDING), judgeArg, expiresArg, count, updatedAt, updatedAt)
	return jid
}

// dupWorld is Ana's INT-83 and Bob's INT-92, both live.
type dupWorld struct {
	bob, bobAgent        uuid.UUID
	anaSession, bobSess  uuid.UUID
	incumbent, yieldPlan uuid.UUID
}

func (h *harness) seedDupWorld(base time.Time) dupWorld {
	h.t.Helper()
	var w dupWorld
	w.bob, w.bobAgent = h.seedPerson("M-2", "A-2", "Bob", "frontend")
	w.anaSession = h.seedSessionFor("S-17", h.agentUUID, h.memberUUID, enums.SESSION_STATUS_LIVE, base)
	w.bobSess = h.seedSessionFor("S-22", w.bobAgent, w.bob, enums.SESSION_STATUS_LIVE, base)
	w.incumbent = h.seedWordedIntent(w.anaSession, h.memberUUID, dupIncumbentKey, "add login page", enums.INTENT_STATUS_ACTIVE, 1)
	w.yieldPlan = h.seedWordedIntent(w.bobSess, w.bob, dupYieldKey, "build the login screen", enums.INTENT_STATUS_ACTIVE, 1)
	return w
}

// beatBoth keeps both agents working as the test clock moves.
func (h *harness) beatBoth(w dupWorld, at time.Time) {
	h.t.Helper()
	h.beat(w.anaSession, at)
	h.beat(w.bobSess, at)
}

// seedDuplicateConflict is CF-44 as report_judgement raises it (§3.3): the
// judge's session (Bob's) is the yield side and the only participant, notified
// in its own response; the incumbent is not attached; the verdict's judgement
// row links to the conflict.
func (h *harness) seedDuplicateConflict(w dupWorld, severity enums.ConflictSeverity, detectedAt time.Time) uuid.UUID {
	h.t.Helper()
	cid := uuid.Must(uuid.NewV4())
	evidence := `{"overlap_path":null,"a_label":"Ana (claude)","a_summary":"add login page","b_label":"Bob (frontend)",` +
		`"b_summary":"build the login screen","adjusters":["plans:` + dupIncumbentKey + `,` + dupYieldKey + `","words:login,screen"],` +
		`"field_issues":["S-22's model (0.85): ` + dupRationale + `"],"detail":"duplicate_work.words"}`
	h.exec("INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`detector_rule`,`confidence`,`evidence`,`suggested_action`,`suggested_yield_session_uuid`,`suggested_yield_reason`,"+
		"`occurrence_count`,`first_detected_at`,`last_detected_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		cid.String(), h.teamUUID.String(), h.projectUUID.String(), dupConflictKey, int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		mcp.DuplicateWorkDedupeKey(w.incumbent.String(), w.yieldPlan.String()), int64(severity), int64(enums.CONFLICT_STATUS_OPEN),
		int64(enums.DETECTED_BY_AGENT), mcp.RuleDuplicateWords, 0.85, evidence,
		"INT-83 (Ana (claude), active 4m) is already building this: \"add login page\". Stop before you edit.",
		w.bobSess.String(), "same work as "+dupIncumbentKey, 1, detectedAt, detectedAt)
	h.exec("INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
		"`subject_kind`,`subject_uuid`,`role`,`notified_at`) VALUES (?,?,?,?,?,?,?,?,?,?)",
		id(), cid.String(), h.teamUUID.String(), w.bobSess.String(), w.bobAgent.String(), w.bob.String(),
		int64(enums.SUBJECT_KIND_INTENT), w.yieldPlan.String(), int64(enums.PARTICIPANT_ROLE_INITIATOR), detectedAt)
	h.exec("INSERT INTO `judgement` (`id`,`team_uuid`,`pair_key`,`kind`,`subject_a_kind`,`subject_a_uuid`,`subject_a_revision`,"+
		"`subject_b_kind`,`subject_b_uuid`,`subject_b_revision`,`status`,`verdict`,`severity`,`confidence`,`rationale`,"+
		"`judge_session_uuid`,`judged_at`,`assignment_count`,`pinned`,`conflict_uuid`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,0,?)",
		id(), h.teamUUID.String(), "dup-judged-"+cid.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		int64(enums.SUBJECT_KIND_INTENT), w.incumbent.String(), 1, int64(enums.SUBJECT_KIND_INTENT), w.yieldPlan.String(), 1,
		int64(enums.JUDGEMENT_STATUS_JUDGED), int64(enums.JUDGEMENT_VERDICT_CONFLICT), int64(severity), 0.85, dupRationale,
		w.bobSess.String(), detectedAt, cid.String())
	return cid
}

// attachAna makes the incumbent a participant, as the notice pass would.
func (h *harness) attachAna(w dupWorld, cid uuid.UUID, notifiedAt time.Time) {
	h.t.Helper()
	h.exec("INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
		"`subject_kind`,`subject_uuid`,`role`,`notified_at`) VALUES (?,?,?,?,?,?,?,?,?,?)",
		id(), cid.String(), h.teamUUID.String(), w.anaSession.String(), h.agentUUID.String(), h.memberUUID.String(),
		int64(enums.SUBJECT_KIND_INTENT), w.incumbent.String(), int64(enums.PARTICIPANT_ROLE_INCUMBENT), notifiedAt)
}

type noticeRow struct {
	id, key, body, target, ref string
	kind                       enums.InstructionKind
	status                     enums.InstructionStatus
	requiresReport             bool
	actionNote                 sql.NullString
	expiresAt                  sql.NullTime
}

func (h *harness) instructionsTo(session uuid.UUID) []noticeRow {
	h.t.Helper()
	rows, err := h.db.Query("SELECT `id`, `key`, `body`, COALESCE(`target_session_uuid`, ''), COALESCE(`ref_uuid`, ''), `kind`, `status`, "+
		"`requires_report`, `action_note`, `expires_at` FROM `instruction` WHERE `target_session_uuid` = ? ORDER BY `created_at`, `key`",
		session.String())
	if err != nil {
		h.t.Fatalf("reading instructions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []noticeRow
	for rows.Next() {
		var (
			n            noticeRow
			kind, status int64
		)
		if err := rows.Scan(&n.id, &n.key, &n.body, &n.target, &n.ref, &kind, &status, &n.requiresReport, &n.actionNote, &n.expiresAt); err != nil {
			h.t.Fatalf("reading instructions: %v", err)
		}
		n.kind, n.status = enums.InstructionKind(kind), enums.InstructionStatus(status)
		out = append(out, n)
	}
	return out
}

func (h *harness) countEvents(kind enums.EventKind) int {
	h.t.Helper()
	return h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?", h.teamUUID.String(), int64(kind))
}

func (h *harness) incumbentParticipants(cid, session uuid.UUID) int {
	h.t.Helper()
	return h.count("SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ?", cid.String(), session.String())
}

// splitNotice is get_instructions' split (app/mcp splitNoticeBody): what
// happened before the first ": ", the action after it.
func splitNotice(body string) (what, action string, ok bool) {
	what, action, ok = strings.Cut(body, ": ")
	return strings.TrimSpace(what), strings.TrimSpace(action), ok && strings.TrimSpace(action) != ""
}

// ─────────────────────────────────────────────
// (a) The pairs
// ─────────────────────────────────────────────

// TestIntegrationDuplicateBacklogIsAssignedToTheJudgeUpToTheCap: backlogged
// duplicate pairs go to subject b's session only, under the one per-minute cap
// decisions share. A decision pair armed seconds ago already takes a slot.
func TestIntegrationDuplicateBacklogIsAssignedToTheJudgeUpToTheCap(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	w := h.seedDupWorld(now)

	// One decision pair armed for Bob 10 seconds ago: the cap is shared.
	decision := h.seedDecision("#test-decision", enums.DECISION_STATUS_ACCEPTED, 1, uuid.Nil)
	armed := now.Add(10 * time.Minute)
	h.seedPair("pair-decision", decision, 1, w.yieldPlan, 1, &w.bobSess, &armed, 1, now.Add(-10*time.Second))

	// Five backlogged duplicate pairs: Ana's plans (a) against Bob's (b).
	var pairs []uuid.UUID
	for i := 0; i < 5; i++ {
		other := h.seedWordedIntent(w.anaSession, h.memberUUID, fmt.Sprintf("INT-%d", 100+i), "add login page", enums.INTENT_STATUS_ACTIVE, 1)
		pairs = append(pairs, h.seedDuplicatePair(other, 1, w.yieldPlan, 1, nil, nil, 0, now.Add(-5*time.Minute)))
	}

	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.JudgementsAssigned != defaultMaxReviewsPerMinute-1 {
		t.Fatalf("over the cap: the pass assigned %d duplicate pairs, want %d (cap %d, one slot already spent on a decision pair)",
			rep.JudgementsAssigned, defaultMaxReviewsPerMinute-1, defaultMaxReviewsPerMinute)
	}
	assigned, backlog := 0, 0
	for _, p := range pairs {
		st := h.pairState(p)
		if st.status != enums.JUDGEMENT_STATUS_PENDING {
			t.Fatalf("a pair is %v; handing out work never closes a pair", st.status)
		}
		if st.judge == "" {
			backlog++
			if st.count != 0 {
				t.Errorf("a backlogged pair carries assignment_count %d, want 0", st.count)
			}
			continue
		}
		assigned++
		if st.judge != w.bobSess.String() {
			t.Errorf("a pair was assigned to %s; only subject b's session %s (the judge's plan) may judge it", st.judge, w.bobSess)
		}
		if st.count != 1 {
			t.Errorf("assignment_count = %d, want 1", st.count)
		}
		if want := now.Add(decisionJudgeWindow); !st.expires.Valid || !st.expires.Time.UTC().Equal(want) {
			t.Errorf("judging_expires_at = %v, want %v", st.expires.Time, want)
		}
	}
	if assigned != 2 || backlog != 3 {
		t.Errorf("%d assigned and %d in the backlog, want 2 and 3", assigned, backlog)
	}
	if n := h.count("SELECT COUNT(*) FROM `judgement` WHERE `judge_session_uuid` = ?", w.anaSession.String()); n != 0 {
		t.Errorf("%d pairs were handed to Ana's session, subject a's; want 0", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("the backlog pass wrote %d events, want 0", n)
	}

	// Still inside the minute: nothing more.
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.JudgementsAssigned != 0 {
		t.Errorf("a second pass inside the minute assigned %d more, want 0", rep.JudgementsAssigned)
	}

	// Under the cap: a minute on, the three waiting pairs all go out.
	clock = now.Add(judgeRateWindow + time.Second)
	h.beatBoth(w, clock)
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.JudgementsAssigned != 3 {
		t.Errorf("under the cap the pass assigned %d pairs, want the 3 that were waiting", rep.JudgementsAssigned)
	}
	for _, p := range pairs {
		if st := h.pairState(p); st.judge != w.bobSess.String() {
			t.Errorf("a pair sits with %q, want Bob's session", st.judge)
		}
	}
}

// TestIntegrationDuplicatePairIsReArmedThenExpiresSilently: armed 1 → 2 → 3,
// then given up on, with no event, no instruction and no conflict.
func TestIntegrationDuplicatePairIsReArmedThenExpiresSilently(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base
	w := h.seedDupWorld(base)

	lapsed := base.Add(-time.Minute)
	pair := h.seedDuplicatePair(w.incumbent, 1, w.yieldPlan, 1, &w.bobSess, &lapsed, 1, lapsed)

	for want := int64(2); want <= judgeMaxAssignments; want++ {
		h.beatBoth(w, clock)
		rep := h.runOnce(h.sweeper(atClock(&clock)))
		if rep.JudgementsReArmed != 1 {
			t.Fatalf("assignment_count %d: the pass re-armed %d pairs, want 1", want, rep.JudgementsReArmed)
		}
		st := h.pairState(pair)
		if st.count != want || st.status != enums.JUDGEMENT_STATUS_PENDING || st.judge != w.bobSess.String() {
			t.Fatalf("after re-arm: count=%d status=%v judge=%s, want %d pending %s", st.count, st.status, st.judge, want, w.bobSess)
		}
		if exp := clock.Add(decisionJudgeWindow); !st.expires.Valid || !st.expires.Time.UTC().Equal(exp) {
			t.Fatalf("judging_expires_at = %v, want %v", st.expires.Time, exp)
		}
		t.Logf("pass at +%s: re-armed to assignment_count %d, judge S-22", clock.Sub(base), st.count)
		clock = clock.Add(decisionJudgeWindow + time.Minute)
	}

	h.beatBoth(w, clock)
	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.JudgementsExpired != 1 || rep.JudgementsReArmed != 0 {
		t.Fatalf("at assignment_count 3: expired %d, re-armed %d; want 1 and 0", rep.JudgementsExpired, rep.JudgementsReArmed)
	}
	if st := h.pairState(pair); st.status != enums.JUDGEMENT_STATUS_EXPIRED || st.count != judgeMaxAssignments {
		t.Fatalf("the pair is %v at count %d, want expired at %d", st.status, st.count, judgeMaxAssignments)
	}
	t.Logf("pass at +%s: expired after three silences", clock.Sub(base))
	for table, want := range map[string]int{"team_event": 0, "instruction": 0, "conflict": 0} {
		if n := h.count("SELECT COUNT(*) FROM `"+table+"` WHERE `team_uuid` = ?", h.teamUUID.String()); n != want {
			t.Errorf("expiring a duplicate pair wrote %d %s rows, want %d — unjudged is not a duplicate, and asks nobody", n, table, want)
		}
	}
}

// TestIntegrationStaleDuplicatePairsExpireWithoutReArming: a pair against a
// wording either side has since changed, or whose other side's session is
// gone, is expired — assigned or still in the backlog — and never re-armed.
func TestIntegrationStaleDuplicatePairsExpireWithoutReArming(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base
	w := h.seedDupWorld(base)
	lapsed := base.Add(-time.Minute)

	// Ana reworded (wording 2), Bob's plan reworded, Ana's other session ended.
	anaReworded := h.seedWordedIntent(w.anaSession, h.memberUUID, "INT-84", "add login page with remember me", enums.INTENT_STATUS_ACTIVE, 2)
	bobReworded := h.seedWordedIntent(w.bobSess, w.bob, "INT-93", "wire the login page to POST /api/login", enums.INTENT_STATUS_ACTIVE, 2)
	ended := h.seedSessionFor("S-18", h.agentUUID, h.memberUUID, enums.SESSION_STATUS_ENDED, base)
	onEnded := h.seedWordedIntent(ended, h.memberUUID, "INT-85", "add login page", enums.INTENT_STATUS_ACTIVE, 1)

	pairs := map[string]uuid.UUID{
		"a's wording moved (assigned)":   h.seedDuplicatePair(anaReworded, 1, w.yieldPlan, 1, &w.bobSess, &lapsed, 1, lapsed),
		"b's wording moved (assigned)":   h.seedDuplicatePair(w.incumbent, 1, bobReworded, 1, &w.bobSess, &lapsed, 1, lapsed),
		"a's session is gone (assigned)": h.seedDuplicatePair(onEnded, 1, w.yieldPlan, 1, &w.bobSess, &lapsed, 1, lapsed),
		"a's wording moved (backlog)":    h.seedDuplicatePair(anaReworded, 1, bobReworded, 2, nil, nil, 0, lapsed),
		"a's session is gone (backlog)":  h.seedDuplicatePair(onEnded, 1, bobReworded, 2, nil, nil, 0, lapsed),
	}

	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.JudgementsExpired != len(pairs) || rep.JudgementsReArmed != 0 || rep.JudgementsAssigned != 0 {
		t.Fatalf("expired %d, re-armed %d, assigned %d; want %d, 0, 0",
			rep.JudgementsExpired, rep.JudgementsReArmed, rep.JudgementsAssigned, len(pairs))
	}
	for name, p := range pairs {
		st := h.pairState(p)
		if st.status != enums.JUDGEMENT_STATUS_EXPIRED {
			t.Errorf("%s: the pair is %v, want expired", name, st.status)
		}
		if strings.HasSuffix(name, "(backlog)") && (st.judge != "" || st.count != 0) {
			t.Errorf("%s: a stale backlogged pair was assigned (judge %q, count %d) before expiring", name, st.judge, st.count)
		}
		if strings.HasSuffix(name, "(assigned)") && st.count != 1 {
			t.Errorf("%s: re-armed to %d; a stale pair gets no more turns", name, st.count)
		}
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("expiring stale pairs wrote %d events, want 0", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("expiring stale pairs asked %d agents, want 0", n)
	}
}

// TestIntegrationDuplicateReArmOnlyEverExtendsThePairsOwnJudge is the duplicate
// twin of the decisions test: one pass re-arms each pair where it sits, and a
// stale read naming the wrong session moves nothing.
func TestIntegrationDuplicateReArmOnlyEverExtendsThePairsOwnJudge(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base
	w := h.seedDupWorld(base)

	anaOther := h.seedWordedIntent(w.anaSession, h.memberUUID, "INT-86", "build the signup screen", enums.INTENT_STATUS_ACTIVE, 1)
	bobOther := h.seedWordedIntent(w.bobSess, w.bob, "INT-94", "add signup page", enums.INTENT_STATUS_ACTIVE, 1)

	lapsed := base.Add(-time.Minute)
	bobPair := h.seedDuplicatePair(w.incumbent, 1, w.yieldPlan, 1, &w.bobSess, &lapsed, 1, lapsed) // Bob judges INT-92
	anaPair := h.seedDuplicatePair(bobOther, 1, anaOther, 1, &w.anaSession, &lapsed, 1, lapsed)    // Ana judges INT-86

	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.JudgementsReArmed != 2 {
		t.Fatalf("the pass re-armed %d pairs, want 2", rep.JudgementsReArmed)
	}
	for _, c := range []struct {
		name        string
		pair, judge uuid.UUID
	}{{"Bob's", bobPair, w.bobSess}, {"Ana's", anaPair, w.anaSession}} {
		if st := h.pairState(c.pair); st.judge != c.judge.String() || st.count != 2 {
			t.Errorf("%s pair: judge %s count %d, want %s and 2", c.name, st.judge, st.count, c.judge)
		}
	}

	// The stale read: this pass believes Ana's session holds Bob's pair.
	before := h.pairState(bobPair)
	stale := duplicatePair{id: bobPair.String(), judgeSession: w.anaSession.String(), assignmentCount: before.count, bSession: w.bobSess.String()}
	var out Report
	if err := h.sweeper(atClock(&clock)).rearmPair(context.Background(), stale.pending(), decisionJudgeWindow, clock.Add(time.Minute), &out); err != nil {
		t.Fatalf("rearmPair: %v", err)
	}
	if out.JudgementsReArmed != 0 {
		t.Errorf("re-arming reported %d pairs extended for a judge the row does not have, want 0", out.JudgementsReArmed)
	}
	after := h.pairState(bobPair)
	if after.judge != w.bobSess.String() {
		t.Fatalf("Bob's duplicate pair now sits with %s, want %s — re-arming handed one agent's plan to another", after.judge, w.bobSess)
	}
	if after.count != before.count || !after.expires.Time.Equal(before.expires.Time) {
		t.Errorf("an UPDATE naming the wrong judge moved count %d → %d, expiry %v → %v",
			before.count, after.count, before.expires.Time, after.expires.Time)
	}
	if st := h.pairState(anaPair); st.judge != w.anaSession.String() || st.count != 2 {
		t.Errorf("Ana's pair is now judge=%s count=%d, want %s and 2", st.judge, st.count, w.anaSession)
	}
}

// ─────────────────────────────────────────────
// (b) The incumbent's notice (§4.5, §4.6)
// ─────────────────────────────────────────────

// TestIntegrationIncumbentIsToldOnlyAfterTheGrace: nothing at grace − 1s; at
// the grace, once; the incumbent attached; the event structural and worded;
// the body split by get_instructions with both halves ≤ 200. Then a severity
// rise re-notifies once, and a reopened conflict waits a fresh grace.
func TestIntegrationIncumbentIsToldOnlyAfterTheGrace(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	w := h.seedDupWorld(base)
	cf := h.seedDuplicateConflict(w, enums.CONFLICT_SEVERITY_MEDIUM, base)

	grace := coordination.DuplicateNoticeGrace("hackathon")
	if grace != 2*time.Minute {
		t.Fatalf("hackathon grace = %v, want 2m (EscalationBudget/5)", grace)
	}

	// Inside the grace: the judge may still yield, so the incumbent hears nothing.
	clock := base.Add(grace - time.Second)
	h.beatBoth(w, clock)
	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.DuplicateNoticesSent != 0 || len(h.instructionsTo(w.anaSession)) != 0 {
		t.Fatalf("at grace − 1s the incumbent was told (%d notices); it hears only if the duplicate outlives the grace", rep.DuplicateNoticesSent)
	}
	if n := h.incumbentParticipants(cf, w.anaSession); n != 0 {
		t.Fatalf("the incumbent was attached inside the grace (%d rows); attaching is itself an interruption", n)
	}
	if n := h.countEvents(enums.EVENT_KIND_INSTRUCTION_RAISED); n != 0 {
		t.Fatalf("%d instruction_raised events inside the grace, want 0", n)
	}
	t.Logf("at +%s (grace − 1s): no notice, no participant, no event", clock.Sub(base))

	// At the grace: told, once.
	clock = base.Add(grace)
	h.beatBoth(w, clock)
	rep = h.runOnce(h.sweeper(atClock(&clock)))
	if rep.DuplicateNoticesSent != 1 {
		t.Fatalf("at the grace the pass sent %d notices, want 1", rep.DuplicateNoticesSent)
	}
	notices := h.instructionsTo(w.anaSession)
	if len(notices) != 1 {
		t.Fatalf("Ana's session has %d instructions, want 1", len(notices))
	}
	n := notices[0]
	t.Logf("notice %s to S-17: %s", n.key, n.body)
	if n.kind != enums.INSTRUCTION_KIND_CONFLICT_NOTICE || n.status != enums.INSTRUCTION_STATUS_PENDING || n.requiresReport {
		t.Errorf("notice kind=%v status=%v requires_report=%v, want conflict_notice, pending, false", n.kind, n.status, n.requiresReport)
	}
	if n.ref != cf.String() {
		t.Errorf("notice ref = %s, want the conflict %s", n.ref, cf)
	}
	if want := clock.Add(4 * time.Hour); !n.expiresAt.Valid || !n.expiresAt.Time.UTC().Equal(want) {
		t.Errorf("expires_at = %v, want now + 4h", n.expiresAt.Time)
	}
	wantAction := mcp.DuplicateIncumbentAction("S-22 (frontend)", dupYieldKey, dupRationale, "S-22", "")
	if want := dupConflictKey + " (medium) on " + dupIncumbentKey + ": " + wantAction; n.body != want {
		t.Errorf("body =\n  %q\nwant\n  %q", n.body, want)
	}
	what, action, ok := splitNotice(n.body)
	if !ok || what != dupConflictKey+" (medium) on "+dupIncumbentKey {
		t.Errorf("get_instructions would split the body as what=%q ok=%v", what, ok)
	}
	if utf8.RuneCountInString(what) > 200 || utf8.RuneCountInString(action) > 200 {
		t.Errorf("what is %d and action %d characters; get_instructions shows at most 200 of each",
			utf8.RuneCountInString(what), utf8.RuneCountInString(action))
	}
	for _, want := range []string{"S-22 (frontend) judged its plan INT-92 is the same work as yours", "was told to stop", "settle the split with S-22"} {
		if !strings.Contains(action, want) {
			t.Errorf("action is missing %q:\n%s", want, action)
		}
	}

	// The incumbent is attached now, and the conflict marked told.
	var (
		role, subjectKind int64
		subject           string
		notifiedAt        sql.NullTime
	)
	if err := h.db.QueryRow("SELECT `role`, `subject_kind`, `subject_uuid`, `notified_at` FROM `conflict_participant` "+
		"WHERE `conflict_uuid` = ? AND `session_uuid` = ?", cf.String(), w.anaSession.String()).
		Scan(&role, &subjectKind, &subject, &notifiedAt); err != nil {
		t.Fatalf("the incumbent is not attached after the notice: %v", err)
	}
	if enums.ParticipantRole(role) != enums.PARTICIPANT_ROLE_INCUMBENT || enums.SubjectKind(subjectKind) != enums.SUBJECT_KIND_INTENT ||
		subject != w.incumbent.String() || !notifiedAt.Valid {
		t.Errorf("participant role=%v kind=%v subject=%s notified=%v, want incumbent, intent, INT-83, set",
			enums.ParticipantRole(role), enums.SubjectKind(subjectKind), subject, notifiedAt.Valid)
	}
	var (
		cNotified sql.NullTime
		cMaxSev   sql.NullInt64
	)
	if err := h.db.QueryRow("SELECT `notified_at`, `max_severity_notified` FROM `conflict` WHERE `id` = ?", cf.String()).
		Scan(&cNotified, &cMaxSev); err != nil {
		t.Fatal(err)
	}
	if !cNotified.Valid || !cNotified.Time.UTC().Equal(clock) || enums.ConflictSeverity(cMaxSev.Int64) != enums.CONFLICT_SEVERITY_MEDIUM {
		t.Errorf("conflict notified_at=%v max_severity_notified=%v, want %v and medium", cNotified.Time, cMaxSev.Int64, clock)
	}

	// The stored event, column by column.
	var (
		summary, subjectKey, payload sql.NullString
		structural                   bool
		evSession, idem              string
	)
	if err := h.db.QueryRow("SELECT `summary`, `subject_key`, `payload`, `structural`, COALESCE(`session_uuid`, ''), `idempotency_key` "+
		"FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_INSTRUCTION_RAISED), cf.String()).
		Scan(&summary, &subjectKey, &payload, &structural, &evSession, &idem); err != nil {
		t.Fatalf("no instruction_raised event: %v", err)
	}
	t.Logf("stored event: summary=%q subject_key=%q idempotency_key=%q", summary.String, subjectKey.String, idem)
	if !structural {
		t.Error("instruction_raised is not structural; the incumbent's lane would not gain its badge")
	}
	if want := dupConflictKey + ": claude told about " + dupYieldKey; summary.String != want {
		t.Errorf("summary = %q, want %q", summary.String, want)
	}
	if subjectKey.String != dupConflictKey || evSession != w.anaSession.String() {
		t.Errorf("subject_key=%q session=%s, want %s and Ana's session", subjectKey.String, evSession, dupConflictKey)
	}
	if want := fmt.Sprintf("sweep:duplicate_notice:%s:1:%d", cf, int64(enums.CONFLICT_SEVERITY_MEDIUM)); idem != want {
		t.Errorf("idempotency_key = %q, want %q", idem, want)
	}
	p := payload_entity.EventPayloadFromJSON([]byte(payload.String))
	if p.ConflictUUID == nil || *p.ConflictUUID != cf || p.Severity != enums.CONFLICT_SEVERITY_MEDIUM ||
		p.InstructionUUID == nil || p.InstructionUUID.String() != n.id || p.Message.String != n.body {
		t.Errorf("payload = %s; want the conflict, medium, the instruction %s and the body", payload.String, n.id)
	}

	// A second pass writes nothing.
	clock = clock.Add(time.Minute)
	h.beatBoth(w, clock)
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.DuplicateNoticesSent != 0 || rep.EventsEmitted != 0 {
		t.Errorf("a second pass sent %d notices and %d events, want 0", rep.DuplicateNoticesSent, rep.EventsEmitted)
	}

	// A severity rise (a fresh verdict: occurrence and last_detected move)
	// re-notifies once, after its own grace.
	rise := clock
	h.exec("UPDATE `conflict` SET `severity` = ?, `occurrence_count` = 2, `last_detected_at` = ? WHERE `id` = ?",
		int64(enums.CONFLICT_SEVERITY_HIGH), rise, cf.String())
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.DuplicateNoticesSent != 0 {
		t.Errorf("a severity rise re-notified inside its grace")
	}
	clock = rise.Add(grace)
	h.beatBoth(w, clock)
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.DuplicateNoticesSent != 1 {
		t.Fatalf("a severity rise past the grace sent %d notices, want 1", rep.DuplicateNoticesSent)
	}
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.DuplicateNoticesSent != 0 {
		t.Errorf("a severity rise re-notified twice")
	}
	if got := len(h.instructionsTo(w.anaSession)); got != 2 {
		t.Errorf("Ana has %d notices after one severity rise, want 2", got)
	}

	// Reopened by metiche (notified_at cleared, occurrence 3): a fresh grace.
	reopen := clock.Add(time.Minute)
	h.exec("UPDATE `conflict` SET `notified_at` = NULL, `max_severity_notified` = NULL, `occurrence_count` = 3, `last_detected_at` = ? WHERE `id` = ?",
		reopen, cf.String())
	clock = reopen.Add(grace - time.Second)
	h.beatBoth(w, clock)
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.DuplicateNoticesSent != 0 {
		t.Errorf("a reopened conflict re-notified before a fresh grace")
	}
	clock = reopen.Add(grace)
	h.beatBoth(w, clock)
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.DuplicateNoticesSent != 1 {
		t.Errorf("a reopened conflict past a fresh grace sent %d notices, want 1", rep.DuplicateNoticesSent)
	}
	h.checkGapless()
}

// TestIntegrationNoNoticeWhenTheJudgeYieldedInsideTheGrace: Bob marks INT-92
// superseded a minute in. update_intent settles CF-44 as YIELDED (the exported
// settle the tool calls); past the grace the incumbent hears nothing at all.
// And a plan that yielded without its conflict being closed yet is not
// announced either.
func TestIntegrationNoNoticeWhenTheJudgeYieldedInsideTheGrace(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	w := h.seedDupWorld(base)
	cf := h.seedDuplicateConflict(w, enums.CONFLICT_SEVERITY_MEDIUM, base)

	yieldAt := base.Add(time.Minute)
	h.exec("UPDATE `intent` SET `status` = ?, `ended_at` = ? WHERE `id` = ?", int64(enums.INTENT_STATUS_SUPERSEDED), yieldAt, w.yieldPlan.String())
	tx, err := h.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	settled, ok, err := mcp.SettleDuplicateConflict(context.Background(), tx, cf, mcp.DuplicateRelease{
		TeamUUID: h.teamUUID, SessionUUID: w.bobSess, Kind: mcp.DuplicateReleaseIntentEnded, At: yieldAt})
	if err != nil || !ok {
		_ = tx.Rollback()
		t.Fatalf("settling after the yield: ok=%v err=%v", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if settled.Resolution != enums.CONFLICT_RESOLUTION_YIELDED {
		t.Fatalf("resolution = %v, want yielded", settled.Resolution)
	}

	clock := base.Add(coordination.DuplicateNoticeGrace("hackathon") + time.Minute)
	h.beatBoth(w, clock)
	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.DuplicateNoticesSent != 0 || len(h.instructionsTo(w.anaSession)) != 0 || h.incumbentParticipants(cf, w.anaSession) != 0 {
		t.Fatalf("the incumbent was told about a duplicate the judge yielded inside the grace")
	}

	// The same yield, with the conflict still open (the settle not yet run).
	w2Plan := h.seedWordedIntent(w.bobSess, w.bob, "INT-95", "make the login screen", enums.INTENT_STATUS_SUPERSEDED, 1)
	cf2 := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`detector_rule`,`evidence`,`suggested_yield_session_uuid`,`occurrence_count`,`first_detected_at`,`last_detected_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)",
		cf2.String(), h.teamUUID.String(), h.projectUUID.String(), "CF-45", int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		mcp.DuplicateWorkDedupeKey(w.incumbent.String(), w2Plan.String()), int64(enums.CONFLICT_SEVERITY_MEDIUM),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.DETECTED_BY_AGENT), mcp.RuleDuplicateWords,
		`{"adjusters":["plans:INT-83,INT-95"],"field_issues":["S-22's model (0.90): same page"]}`, w.bobSess.String(), base, base)
	rep = h.runOnce(h.sweeper(atClock(&clock)))
	if rep.DuplicateNoticesSent != 0 || len(h.instructionsTo(w.anaSession)) != 0 {
		t.Errorf("the incumbent was told about a plan that already yielded")
	}
}

// TestIntegrationPendingPathNoticeIsFoldedIntoTheDuplicateNotice (§4.6): a path
// notice between the same two sessions that Ana has not read yet is dismissed
// as superseded by CF-44 and named in the new body; a delivered one, and one
// about somebody else, are left alone.
func TestIntegrationPendingPathNoticeIsFoldedIntoTheDuplicateNotice(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	w := h.seedDupWorld(base)
	cf := h.seedDuplicateConflict(w, enums.CONFLICT_SEVERITY_MEDIUM, base)
	other := h.seedSessionFor("S-23", w.bobAgent, w.bob, enums.SESSION_STATUS_LIVE, base)

	pathWithBob := h.seedPathConflict("CF-40", "web/src/auth/session.ts", w.bobSess, uuid.Must(uuid.NewV4()), w.anaSession, uuid.Must(uuid.NewV4()))
	pathWithOther := h.seedPathConflict("CF-41", "web/src/nav.tsx", other, uuid.Must(uuid.NewV4()), w.anaSession, uuid.Must(uuid.NewV4()))

	notice := func(key string, conflict uuid.UUID, status enums.InstructionStatus, at time.Time) string {
		nid := id()
		h.exec("INSERT INTO `instruction` (`id`,`team_uuid`,`target_session_uuid`,`target_agent_uuid`,`key`,`source`,`kind`,`body`,"+
			"`ref_kind`,`ref_uuid`,`requires_report`,`status`,`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?,?)",
			nid, h.teamUUID.String(), w.anaSession.String(), h.agentUUID.String(), key,
			int64(enums.INSTRUCTION_SOURCE_SERVER), int64(enums.INSTRUCTION_KIND_CONFLICT_NOTICE),
			key+" (high) on web/src: settle it between you", int64(enums.SUBJECT_KIND_CONFLICT), conflict.String(),
			int64(status), at, at)
		return nid
	}
	pending := notice("I-901", pathWithBob, enums.INSTRUCTION_STATUS_PENDING, base)
	delivered := notice("I-902", pathWithBob, enums.INSTRUCTION_STATUS_DELIVERED, base.Add(-time.Minute))
	elsewhere := notice("I-903", pathWithOther, enums.INSTRUCTION_STATUS_PENDING, base)

	clock := base.Add(coordination.DuplicateNoticeGrace("hackathon"))
	h.beatBoth(w, clock)
	h.beat(other, clock)
	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.DuplicateNoticesSent != 1 {
		t.Fatalf("the pass sent %d duplicate notices, want 1", rep.DuplicateNoticesSent)
	}

	byID := map[string]noticeRow{}
	var dup noticeRow
	for _, n := range h.instructionsTo(w.anaSession) {
		byID[n.id] = n
		if n.ref == cf.String() {
			dup = n
		}
	}
	if got := byID[pending]; got.status != enums.INSTRUCTION_STATUS_DISMISSED || got.actionNote.String != "superseded by "+dupConflictKey {
		t.Errorf("the unread path notice is %v with action_note %q, want dismissed and %q",
			got.status, got.actionNote.String, "superseded by "+dupConflictKey)
	}
	if got := byID[delivered]; got.status != enums.INSTRUCTION_STATUS_DELIVERED || got.actionNote.Valid {
		t.Errorf("the delivered path notice became %v (%q); a notice already read is left alone", got.status, got.actionNote.String)
	}
	if got := byID[elsewhere]; got.status != enums.INSTRUCTION_STATUS_PENDING || got.actionNote.Valid {
		t.Errorf("the path notice about another session became %v (%q); only the two sessions' own is folded", got.status, got.actionNote.String)
	}
	t.Logf("folded I-901 → %v, action_note %q; new notice: %s", byID[pending].status, byID[pending].actionNote.String, dup.body)
	if !strings.HasSuffix(dup.body, " (also CF-40)") {
		t.Errorf("the duplicate notice does not name the path conflict it replaced:\n%s", dup.body)
	}
	if _, action, _ := splitNotice(dup.body); utf8.RuneCountInString(action) > 200 {
		t.Errorf("the action is %d characters, want ≤ 200", utf8.RuneCountInString(action))
	}
}

// ─────────────────────────────────────────────
// (c) Abandonment (§4.7)
// ─────────────────────────────────────────────

// TestIntegrationAbandonedSideSettlesTheDuplicateConflict: either side going
// quiet past the abandon threshold closes CF-44 as SUPERSEDED with §4.10's
// "Cleared by metiche" note, one structural conflict_resolved event whose
// summary, subject key and payload are stored. The incumbent case reaches a
// conflict it is not yet a participant of, through the judgement row.
func TestIntegrationAbandonedSideSettlesTheDuplicateConflict(t *testing.T) {
	for _, side := range []string{"yield side", "incumbent"} {
		t.Run(side, func(t *testing.T) {
			h := newHarness(t)
			now := time.Now().UTC().Truncate(time.Second)
			clock := now
			w := h.seedDupWorld(now)
			cf := h.seedDuplicateConflict(w, enums.CONFLICT_SEVERITY_MEDIUM, now.Add(-30*time.Minute))

			quiet, quietPlan, quietName := w.bobSess, dupYieldKey, "S-22 (frontend)"
			if side == "incumbent" {
				quiet, quietPlan, quietName = w.anaSession, dupIncumbentKey, "S-17 (claude)"
			}
			// No heartbeat for 20 minutes: past the 600s threshold.
			h.exec("UPDATE `session` SET `last_heartbeat_at` = ? WHERE `id` = ?", now.Add(-20*time.Minute), quiet.String())

			// A pending pair another agent was asked about the quiet side's plan.
			asker := h.seedSessionFor("S-30", w.bobAgent, w.bob, enums.SESSION_STATUS_LIVE, now)
			askerPlan := h.seedWordedIntent(asker, w.bob, "INT-99", "add login page", enums.INTENT_STATUS_ACTIVE, 1)
			quietIntent := w.yieldPlan
			if side == "incumbent" {
				quietIntent = w.incumbent
			}
			armed := now.Add(10 * time.Minute)
			orphan := h.seedDuplicatePair(quietIntent, 1, askerPlan, 1, &asker, &armed, 1, now)

			rep := h.runOnce(h.sweeper(atClock(&clock)))
			if rep.SessionsAbandoned != 1 || rep.ConflictsResolved != 1 {
				t.Fatalf("abandoned %d sessions and resolved %d conflicts, want 1 and 1", rep.SessionsAbandoned, rep.ConflictsResolved)
			}

			var (
				status, resolution int64
				resolvedBy         sql.NullString
			)
			if err := h.db.QueryRow("SELECT `status`, COALESCE(`resolution`, 0), `resolved_by_member_uuid` FROM `conflict` WHERE `id` = ?",
				cf.String()).Scan(&status, &resolution, &resolvedBy); err != nil {
				t.Fatal(err)
			}
			if enums.ConflictStatus(status) != enums.CONFLICT_STATUS_RESOLVED || enums.ConflictResolution(resolution) != enums.CONFLICT_RESOLUTION_SUPERSEDED || resolvedBy.Valid {
				t.Fatalf("CF-44 is %v/%v resolved_by=%v, want resolved/superseded by nobody",
					enums.ConflictStatus(status), enums.ConflictResolution(resolution), resolvedBy.Valid)
			}
			wantNote := fmt.Sprintf("Cleared by metiche: %s was abandoned at %s after no heartbeat, ending %s.",
				quietName, clock.UTC().Format("15:04")+" UTC", quietPlan)
			if note := h.resolutionNote(cf); note != wantNote {
				t.Errorf("resolution_note =\n  %q\nwant\n  %q", note, wantNote)
			}
			t.Logf("resolution_note: %s", h.resolutionNote(cf))

			checkResolvedEvent(t, h, cf, "CF-44 settled (superseded): INT-92 no longer duplicates INT-83", dupConflictKey,
				enums.CONFLICT_RESOLUTION_SUPERSEDED, enums.CONFLICT_SEVERITY_MEDIUM, nil)

			if st := h.pairState(orphan); st.status != enums.JUDGEMENT_STATUS_EXPIRED {
				t.Errorf("the pair about the abandoned side's plan is %v, want expired", st.status)
			}
			if rep2 := h.runOnce(h.sweeper(atClock(&clock))); rep2.ConflictsResolved != 0 {
				t.Errorf("a second pass resolved %d again, want 0", rep2.ConflictsResolved)
			}
		})
	}
}

// ─────────────────────────────────────────────
// (d) Escalation (§4.8)
// ─────────────────────────────────────────────

// TestIntegrationDuplicateConflictEscalatesExactlyOnce: high, both plans live,
// Ana told 11 minutes ago, nothing settled: both people are asked, once. The
// guard is claimEscalation's escalated_at IS NULL, exercised directly after.
func TestIntegrationDuplicateConflictEscalatesExactlyOnce(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base
	w := h.seedDupWorld(base)
	since := base.Add(-11 * time.Minute)
	cf := h.seedDuplicateConflict(w, enums.CONFLICT_SEVERITY_HIGH, since)
	h.attachAna(w, cf, since)
	h.exec("UPDATE `conflict` SET `notified_at` = ?, `max_severity_notified` = ? WHERE `id` = ?", since, int64(enums.CONFLICT_SEVERITY_HIGH), cf.String())

	// One second short of the budget since the last notice: nothing.
	h.exec("UPDATE `conflict_participant` SET `notified_at` = ? WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
		base.Add(-10*time.Minute+time.Second), cf.String(), w.anaSession.String())
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.ConflictsEscalated != 0 {
		t.Fatalf("escalated %d a second before the budget since the incumbent's notice", rep.ConflictsEscalated)
	}
	h.exec("UPDATE `conflict_participant` SET `notified_at` = ? WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
		since, cf.String(), w.anaSession.String())

	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.ConflictsEscalated != 1 || rep.InstructionsRaised != 2 {
		t.Fatalf("escalated %d conflicts with %d questions, want 1 and 2", rep.ConflictsEscalated, rep.InstructionsRaised)
	}
	if at := h.conflictEscalatedAt(cf); !at.Valid || !at.Time.UTC().Equal(clock) {
		t.Fatalf("escalated_at = %v, want %v", at.Time, clock)
	}

	wantYield, wantIncumbent := mcp.DuplicateEscalationQuestions(dupConflictKey, dupYieldKey, dupIncumbentKey, "Bob", "Ana", "add login page", "10m")
	var gotYield, gotIncumbent []questionRow
	for _, q := range h.questions() {
		if q.kind != enums.INSTRUCTION_KIND_QUESTION || !q.requiresReport || q.refUUID != cf.String() {
			t.Errorf("question %s: kind=%v requires_report=%v ref=%s", q.key, q.kind, q.requiresReport, q.refUUID)
		}
		if n := utf8.RuneCountInString(q.body); n > 200 {
			t.Errorf("question %s is %d characters, want ≤ 200", q.key, n)
		}
		switch q.targetSession {
		case w.bobSess.String():
			gotYield = append(gotYield, q)
		case w.anaSession.String():
			gotIncumbent = append(gotIncumbent, q)
		}
		t.Logf("question %s to %s (%d chars): %s", q.key, q.targetSession[:8], utf8.RuneCountInString(q.body), q.body)
	}
	if len(gotYield) != 1 || gotYield[0].body != wantYield {
		t.Errorf("Bob's question = %+v, want exactly %q", gotYield, wantYield)
	}
	if len(gotIncumbent) != 1 || gotIncumbent[0].body != wantIncumbent {
		t.Errorf("Ana's question = %+v, want exactly %q", gotIncumbent, wantIncumbent)
	}

	var (
		summary, subjectKey sql.NullString
		structural          bool
	)
	if err := h.db.QueryRow("SELECT `summary`, `subject_key`, `structural` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_ESCALATED), cf.String()).Scan(&summary, &subjectKey, &structural); err != nil {
		t.Fatalf("no conflict_escalated event: %v", err)
	}
	if want := "CF-44: a person was asked about INT-92 / INT-83"; summary.String != want || subjectKey.String != dupConflictKey || !structural {
		t.Errorf("event summary=%q subject_key=%q structural=%v, want %q, CF-44, true", summary.String, subjectKey.String, structural, want)
	}

	// Never again: a later pass, and the claim itself.
	clock = clock.Add(time.Hour)
	h.beatBoth(w, clock)
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.ConflictsEscalated != 0 {
		t.Errorf("a later pass escalated %d more, want 0", rep.ConflictsEscalated)
	}
	tx, err := h.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	again, err := claimEscalation(context.Background(), tx, cf, clock)
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("a second claim on an escalated duplicate conflict succeeded; the people would be asked twice")
	}
	if n := h.countEvents(enums.EVENT_KIND_CONFLICT_ESCALATED); n != 1 {
		t.Errorf("%d conflict_escalated events, want 1", n)
	}
	if got := len(h.questions()); got != 2 {
		t.Errorf("%d questions in all, want 2", got)
	}
}

// TestIntegrationDuplicateConflictIsNeverEscalatedBelowTheFloor walks the
// ladder against the default human floor (high), an hour past the budget, and
// the cases where a plan ended or the incumbent was never told.
func TestIntegrationDuplicateConflictIsNeverEscalatedBelowTheFloor(t *testing.T) {
	cases := []struct {
		name      string
		severity  enums.ConflictSeverity
		attached  bool
		endedPlan bool
		want      int // conflicts escalated
		questions int
	}{
		{"low", enums.CONFLICT_SEVERITY_LOW, true, false, 0, 0},
		{"medium", enums.CONFLICT_SEVERITY_MEDIUM, true, false, 0, 0},
		{"high", enums.CONFLICT_SEVERITY_HIGH, true, false, 1, 2},
		{"critical", enums.CONFLICT_SEVERITY_CRITICAL, true, false, 1, 2},
		{"high, plan done", enums.CONFLICT_SEVERITY_HIGH, true, true, 0, 0},
		// Never told before this pass: the notice step, which runs first, tells
		// the incumbent now, and a fresh notice restarts the budget (§4.8's
		// openSince), so nobody's person is asked in the same breath.
		{"high, incumbent told only now", enums.CONFLICT_SEVERITY_HIGH, false, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			base := time.Now().UTC().Truncate(time.Second)
			clock := base
			w := h.seedDupWorld(base)
			cf := h.seedDuplicateConflict(w, c.severity, base.Add(-time.Hour))
			if c.attached {
				h.attachAna(w, cf, base.Add(-time.Hour))
				h.exec("UPDATE `conflict` SET `notified_at` = ?, `max_severity_notified` = `severity` WHERE `id` = ?", base.Add(-time.Hour), cf.String())
			}
			if c.endedPlan {
				h.exec("UPDATE `intent` SET `status` = ? WHERE `id` = ?", int64(enums.INTENT_STATUS_DONE), w.incumbent.String())
			}
			rep := h.runOnce(h.sweeper(atClock(&clock)))
			if rep.ConflictsEscalated != c.want {
				t.Fatalf("%s: escalated %d, want %d (human floor high)", c.name, rep.ConflictsEscalated, c.want)
			}
			if got := h.conflictEscalatedAt(cf).Valid; got != (c.want == 1) {
				t.Errorf("escalated_at set = %v, want %v", got, c.want == 1)
			}
			if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ? AND `kind` = ?",
				h.teamUUID.String(), int64(enums.INSTRUCTION_KIND_QUESTION)); n != c.questions {
				t.Errorf("%d questions, want %d", n, c.questions)
			}
		})
	}
}
