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
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// The sweeper's half of docs/DECISIONS.md: the judge window and the backlog
// (§4.4), the decision conflicts an abandoned session settles (§4.5), and the
// one place this feature interrupts a person (§4.7).
//
// Every name here is a test name. Ana and Bob are the two people; the decision
// is #auth-jwt-cookie and the plan is INT-91, exactly as the spec's examples
// use them.
//
// These run against real MySQL through the same harness as the rest of the
// package (see integration_test.go for the DSN and the schema).

const (
	testDecisionKey = "#auth-jwt-cookie"
	testIntentKey   = "INT-91"
	testConflictKey = "CF-70"
)

// ─────────────────────────────────────────────
// Fixtures
// ─────────────────────────────────────────────

// seedPerson adds a second person to the team: their account, their member row
// and one agent of theirs. The harness's own person is Ana; Bob is everybody
// else.
//
// The account is theirs alone, because a member is unique per (account, team):
// two members against one account is one person joining twice, which is not
// what two people look like. token_hash is a fixture string, the way the
// harness's own is — this package never reads that column, and no test here
// creates a reason to hold a real one.
func (h *harness) seedPerson(memberKey, agentKey, name, label string) (member, agent uuid.UUID) {
	h.t.Helper()
	account := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
		account.String(), "acct-"+account.String()[:8], name, "fixture-hash", 1, int64(enums.RECORD_STATUS_ACTIVE))

	member = uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `member` (`id`,`team_uuid`,`key`,`display_name`,`role`,`status`,`account_uuid`) VALUES (?,?,?,?,?,?,?)",
		member.String(), h.teamUUID.String(), memberKey, name, int64(enums.MEMBER_ROLE_MEMBER),
		int64(enums.RECORD_STATUS_ACTIVE), account.String())

	agent = uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `agent` (`id`,`key`,`label`,`client_key`,`status`,`account_uuid`) VALUES (?,?,?,?,?,?)",
		agent.String(), agentKey, label, "client-"+agentKey, int64(enums.AGENT_STATUS_ACTIVE), account.String())
	return member, agent
}

// seedSessionFor is seedSession with a chosen agent and member, so a test can
// put Ana and Bob on two sides of one conflict.
func (h *harness) seedSessionFor(key string, agentUUID, memberUUID uuid.UUID, status enums.SessionStatus, heartbeat time.Time) uuid.UUID {
	h.t.Helper()
	sid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`branch`,`status`,`started_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		sid.String(), h.teamUUID.String(), h.projectUUID.String(), agentUUID.String(), memberUUID.String(),
		key, "feat/auth", int64(status), heartbeat, heartbeat)
	return sid
}

// beat keeps a session alive as a test clock moves forward, the way a working
// agent would. Without it, a test that steps past the abandon threshold would
// be testing session expiry rather than the judge window.
func (h *harness) beat(session uuid.UUID, at time.Time) {
	h.t.Helper()
	h.exec("UPDATE `session` SET `last_heartbeat_at` = ?, `status` = ? WHERE `id` = ?",
		at, int64(enums.SESSION_STATUS_LIVE), session.String())
}

func (h *harness) seedDecision(key string, status enums.DecisionStatus, revision int64, recordedBy uuid.UUID) uuid.UUID {
	h.t.Helper()
	did := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`status`,`always_show`,"+
		"`revision`,`decided_by_member_uuid`,`decided_at`,`recorded_by_session_uuid`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		did.String(), h.teamUUID.String(), h.projectUUID.String(), key,
		"Session tokens live in an httpOnly cookie",
		"The session token is set as an httpOnly cookie and never read from JavaScript.",
		int64(status), 0, revision, h.memberUUID.String(), time.Now().UTC(), uuidOrNil(recordedBy))
	return did
}

func (h *harness) seedIntent(session, member uuid.UUID, key, summary string, status enums.IntentStatus, revision int64) uuid.UUID {
	h.t.Helper()
	iid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,`revision`,`declared_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		iid.String(), h.teamUUID.String(), h.projectUUID.String(), session.String(), member.String(),
		key, summary, int64(enums.INTENT_KIND_IMPLEMENT), int64(status), revision, time.Now().UTC())
	return iid
}

// seedPair writes one row of the ledger: a decision and a plan put in front of
// a judge. judge nil leaves it in the backlog, the way the reviewer leaves a
// pair it minted past the per-minute cap.
func (h *harness) seedPair(pairKey string, decision uuid.UUID, decRev int64, intent uuid.UUID, intRev int64,
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
		jid.String(), h.teamUUID.String(), pairKey, int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION),
		int64(enums.SUBJECT_KIND_DECISION), decision.String(), decRev,
		int64(enums.SUBJECT_KIND_INTENT), intent.String(), intRev,
		int64(enums.JUDGEMENT_STATUS_PENDING), judgeArg, expiresArg, count, updatedAt, updatedAt)
	return jid
}

type pairState struct {
	status  enums.JudgementStatus
	judge   string
	expires sql.NullTime
	count   int64
}

func (h *harness) pairState(pair uuid.UUID) pairState {
	h.t.Helper()
	var (
		p  pairState
		st int64
	)
	if err := h.db.QueryRow("SELECT `status`, COALESCE(`judge_session_uuid`, ''), `judging_expires_at`, `assignment_count` "+
		"FROM `judgement` WHERE `id` = ?", pair.String()).Scan(&st, &p.judge, &p.expires, &p.count); err != nil {
		h.t.Fatalf("reading the pair: %v", err)
	}
	p.status = enums.JudgementStatus(st)
	return p
}

// decisionConflict is one open decision_contradiction conflict with the two
// participant rows report_judgement would have attached: the plan's owner
// (subject: the intent) and the decision's author (subject: the decision).
type decisionConflict struct {
	uuid     uuid.UUID
	ownerRow uuid.UUID
}

func (h *harness) seedDecisionConflict(key, decisionKey string, severity enums.ConflictSeverity,
	owner, ownerAgent, ownerMember, intent uuid.UUID,
	decider, deciderAgent, deciderMember, decision uuid.UUID,
	detectedAt, notifiedAt time.Time) decisionConflict {
	h.t.Helper()
	cid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`detector_rule`,`evidence`,`suggested_action`,`occurrence_count`,`first_detected_at`,`last_detected_at`,"+
		"`notified_at`,`max_severity_notified`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		cid.String(), h.teamUUID.String(), h.projectUUID.String(), key,
		int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION), "dedupe-"+cid.String()[:8],
		int64(severity), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.DETECTED_BY_AGENT),
		"decision_contradiction.judged",
		// The decision is found through evidence.overlap_path — the decider's
		// participant row may not exist at all, so this is the real link.
		`{"overlap_path":"`+decisionKey+`"}`,
		"Change the plan to follow it and update_intent with the new summary.",
		1, detectedAt, detectedAt, notifiedAt, int64(severity))

	ownerRow := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
		"`subject_kind`,`subject_uuid`,`role`,`notified_at`) VALUES (?,?,?,?,?,?,?,?,?,?)",
		ownerRow.String(), cid.String(), h.teamUUID.String(), owner.String(), ownerAgent.String(), ownerMember.String(),
		int64(enums.SUBJECT_KIND_INTENT), intent.String(), int64(enums.PARTICIPANT_ROLE_INITIATOR), notifiedAt)

	if !decider.IsNil() {
		h.exec("INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
			"`subject_kind`,`subject_uuid`,`role`,`notified_at`) VALUES (?,?,?,?,?,?,?,?,?,?)",
			id(), cid.String(), h.teamUUID.String(), decider.String(), deciderAgent.String(), deciderMember.String(),
			int64(enums.SUBJECT_KIND_DECISION), decision.String(), int64(enums.PARTICIPANT_ROLE_INCUMBENT), notifiedAt)
	}
	return decisionConflict{uuid: cid, ownerRow: ownerRow}
}

func (h *harness) conflictEscalatedAt(conflict uuid.UUID) sql.NullTime {
	h.t.Helper()
	var at sql.NullTime
	if err := h.db.QueryRow("SELECT `escalated_at` FROM `conflict` WHERE `id` = ?", conflict.String()).Scan(&at); err != nil {
		h.t.Fatalf("reading escalated_at: %v", err)
	}
	return at
}

type questionRow struct {
	key            string
	body           string
	kind           enums.InstructionKind
	requiresReport bool
	refUUID        string
	targetSession  string
	expiresAt      sql.NullTime
}

func (h *harness) questions() []questionRow {
	h.t.Helper()
	rows, err := h.db.Query("SELECT `key`, `body`, `kind`, `requires_report`, COALESCE(`ref_uuid`, ''), " +
		"COALESCE(`target_session_uuid`, ''), `expires_at` FROM `instruction` ORDER BY `key`")
	if err != nil {
		h.t.Fatalf("reading instructions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []questionRow
	for rows.Next() {
		var (
			q    questionRow
			kind int64
		)
		if err := rows.Scan(&q.key, &q.body, &kind, &q.requiresReport, &q.refUUID, &q.targetSession, &q.expiresAt); err != nil {
			h.t.Fatalf("reading instructions: %v", err)
		}
		q.kind = enums.InstructionKind(kind)
		out = append(out, q)
	}
	return out
}

func uuidOrNil(u uuid.UUID) any {
	if u.IsNil() {
		return nil
	}
	return u.String()
}

// atClock builds Options whose clock is fixed, so a test can stand at an exact
// moment relative to a budget rather than racing a real one.
func atClock(at *time.Time) Options {
	return Options{Clock: func() time.Time { return *at }}
}

// ─────────────────────────────────────────────
// (a) The backlog and the judge window (§4.4)
// ─────────────────────────────────────────────

// TestIntegrationDecisionBacklogIsAssignedUpToTheCap is the per-minute cap
// doing its job in both directions: the pairs under it are handed to the
// plan's own agent, and the ones over it are left exactly as they are for a
// later pass rather than dropped or dumped on somebody.
func TestIntegrationDecisionBacklogIsAssignedUpToTheCap(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := now

	bob, bobAgent := h.seedPerson("M-2", "A-2", "Bob", "codex")
	session := h.seedSessionFor("S-22", bobAgent, bob, enums.SESSION_STATUS_LIVE, now)
	intent := h.seedIntent(session, bob, testIntentKey, "store the session token in localStorage after login",
		enums.INTENT_STATUS_ACTIVE, 1)

	// Five candidate pairs in the backlog: unassigned, which is how the
	// reviewer leaves a pair it minted past the cap.
	var pairs []uuid.UUID
	for i := 0; i < 5; i++ {
		decision := h.seedDecision(fmt.Sprintf("#test-decision-%d", i), enums.DECISION_STATUS_ACCEPTED, 1, uuid.Nil)
		pairs = append(pairs, h.seedPair(fmt.Sprintf("pair-backlog-%d", i), decision, 1, intent, 1,
			nil, nil, 0, now.Add(-5*time.Minute)))
	}

	rep := h.runOnce(h.sweeper(atClock(&clock)))

	if rep.JudgementsAssigned != defaultMaxReviewsPerMinute {
		t.Fatalf("the pass assigned %d pairs, want %d — the per-minute cap is what bounds how much judging one agent is asked for at once",
			rep.JudgementsAssigned, defaultMaxReviewsPerMinute)
	}
	assigned, backlog := 0, 0
	for _, p := range pairs {
		st := h.pairState(p)
		if st.status != enums.JUDGEMENT_STATUS_PENDING {
			t.Fatalf("a pair is %v; nothing here may leave pending", st.status)
		}
		if st.judge == "" {
			backlog++
			if st.count != 0 {
				t.Errorf("an unassigned pair carries assignment_count %d, want 0", st.count)
			}
			continue
		}
		assigned++
		if st.judge != session.String() {
			t.Errorf("a pair was assigned to %s, but only the plan's own session %s may ever judge it", st.judge, session)
		}
		if st.count != 1 {
			t.Errorf("a freshly assigned pair carries assignment_count %d, want 1", st.count)
		}
		if want := now.Add(decisionJudgeWindow); !st.expires.Valid || !st.expires.Time.UTC().Equal(want) {
			t.Errorf("judging_expires_at = %v, want %v (now + the 15m judge window)", st.expires.Time, want)
		}
	}
	if assigned != 3 || backlog != 2 {
		t.Errorf("%d assigned and %d left in the backlog, want 3 and 2", assigned, backlog)
	}

	// §4.4 is silent: handing out work tells nobody anything.
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("the backlog pass wrote %d events, want 0", n)
	}

	// A second pass in the same minute stays at the cap.
	rep2 := h.runOnce(h.sweeper(atClock(&clock)))
	if rep2.JudgementsAssigned != 0 {
		t.Errorf("a second pass inside the same minute assigned %d more pairs, want 0", rep2.JudgementsAssigned)
	}

	// A minute later the cap has moved on and the rest go out.
	clock = now.Add(judgeRateWindow + time.Second)
	h.beat(session, clock)
	rep3 := h.runOnce(h.sweeper(atClock(&clock)))
	if rep3.JudgementsAssigned != 2 {
		t.Errorf("after the rate window the pass assigned %d pairs, want the 2 that were waiting", rep3.JudgementsAssigned)
	}
}

// TestIntegrationLapsedPairIsReArmedThenExpires walks one pair through every
// state §4.4 gives it: armed three times, then given up on — quietly.
//
// The quiet is the point. An agent that never answered has not said the plan
// contradicts the decision; it has said nothing. Recording a verdict here, or
// telling anybody, would put a conclusion on the board that no model reached.
func TestIntegrationLapsedPairIsReArmedThenExpires(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base

	bob, bobAgent := h.seedPerson("M-2", "A-2", "Bob", "codex")
	session := h.seedSessionFor("S-22", bobAgent, bob, enums.SESSION_STATUS_LIVE, base)
	decision := h.seedDecision(testDecisionKey, enums.DECISION_STATUS_ACCEPTED, 1, uuid.Nil)
	intent := h.seedIntent(session, bob, testIntentKey, "read the session from localStorage", enums.INTENT_STATUS_ACTIVE, 1)

	lapsed := base.Add(-time.Minute)
	pair := h.seedPair("pair-rearm", decision, 1, intent, 1, &session, &lapsed, 1, lapsed)

	// 1 → 2 → 3, one pass each, the window lapsing in between.
	for want := int64(2); want <= judgeMaxAssignments; want++ {
		h.beat(session, clock)
		rep := h.runOnce(h.sweeper(atClock(&clock)))
		if rep.JudgementsReArmed != 1 {
			t.Fatalf("assignment_count %d: the pass re-armed %d pairs, want 1", want, rep.JudgementsReArmed)
		}
		st := h.pairState(pair)
		if st.count != want {
			t.Fatalf("assignment_count = %d, want %d", st.count, want)
		}
		if st.status != enums.JUDGEMENT_STATUS_PENDING {
			t.Fatalf("a re-armed pair is %v, want pending", st.status)
		}
		if st.judge != session.String() {
			t.Fatalf("re-arming moved the pair to %s; it may only ever sit with the plan's own session %s", st.judge, session)
		}
		if exp := clock.Add(decisionJudgeWindow); !st.expires.Valid || !st.expires.Time.UTC().Equal(exp) {
			t.Fatalf("judging_expires_at = %v, want %v", st.expires.Time, exp)
		}
		clock = clock.Add(decisionJudgeWindow + time.Minute)
	}

	// The third silence is the last one.
	h.beat(session, clock)
	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.JudgementsExpired != 1 {
		t.Fatalf("the pass expired %d pairs, want 1 — at assignment_count 3 the sweeper gives up", rep.JudgementsExpired)
	}
	if rep.JudgementsReArmed != 0 {
		t.Errorf("the pass re-armed %d pairs past the cap of %d", rep.JudgementsReArmed, judgeMaxAssignments)
	}
	st := h.pairState(pair)
	if st.status != enums.JUDGEMENT_STATUS_EXPIRED {
		t.Fatalf("the pair is %v after three unanswered windows, want expired", st.status)
	}
	if st.count != judgeMaxAssignments {
		t.Errorf("assignment_count = %d, want %d", st.count, judgeMaxAssignments)
	}

	// Not one event, not one instruction, not one conflict, over the whole run.
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("expiring a pair wrote %d events; unjudged is not a contradiction, so it must say nothing", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("expiring a pair raised %d instructions, want 0 — it asks nobody", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("expiring a pair raised %d conflicts, want 0", n)
	}
}

// TestIntegrationStaleSubjectsExpireWithoutReArming: a pair against wording
// nobody stands behind any more is not worth an agent's attention, however
// many turns it has left.
func TestIntegrationStaleSubjectsExpireWithoutReArming(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base

	bob, bobAgent := h.seedPerson("M-2", "A-2", "Bob", "codex")
	session := h.seedSessionFor("S-22", bobAgent, bob, enums.SESSION_STATUS_LIVE, base)
	intent := h.seedIntent(session, bob, testIntentKey, "read the session from localStorage", enums.INTENT_STATUS_ACTIVE, 1)
	lapsed := base.Add(-time.Minute)

	// (a) the decision was superseded; (b) the decision moved to revision 2
	// under a pair minted against revision 1; (c) the plan is done.
	superseded := h.seedDecision("#test-superseded", enums.DECISION_STATUS_SUPERSEDED, 1, uuid.Nil)
	revised := h.seedDecision("#test-revised", enums.DECISION_STATUS_ACCEPTED, 2, uuid.Nil)
	live := h.seedDecision("#test-live", enums.DECISION_STATUS_ACCEPTED, 1, uuid.Nil)
	doneIntent := h.seedIntent(session, bob, "INT-92", "already finished", enums.INTENT_STATUS_DONE, 1)

	a := h.seedPair("pair-superseded", superseded, 1, intent, 1, &session, &lapsed, 1, lapsed)
	b := h.seedPair("pair-revised", revised, 1, intent, 1, &session, &lapsed, 1, lapsed)
	c := h.seedPair("pair-doneplan", live, 1, doneIntent, 1, &session, &lapsed, 1, lapsed)

	h.beat(session, clock)
	rep := h.runOnce(h.sweeper(atClock(&clock)))

	if rep.JudgementsExpired != 3 {
		t.Fatalf("the pass expired %d pairs, want 3", rep.JudgementsExpired)
	}
	if rep.JudgementsReArmed != 0 {
		t.Fatalf("the pass re-armed %d pairs whose subjects are stale, want 0", rep.JudgementsReArmed)
	}
	for name, pair := range map[string]uuid.UUID{"superseded decision": a, "revised decision": b, "finished plan": c} {
		st := h.pairState(pair)
		if st.status != enums.JUDGEMENT_STATUS_EXPIRED {
			t.Errorf("the pair against a %s is %v, want expired", name, st.status)
		}
		if st.count != 1 {
			t.Errorf("the pair against a %s was re-armed to %d; a stale pair gets no more turns", name, st.count)
		}
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("expiring stale pairs wrote %d events, want 0", n)
	}
}

// ─────────────────────────────────────────────
// (b) An abandoned session (§4.5)
// ─────────────────────────────────────────────

// TestIntegrationAbandonedSessionSettlesItsDecisionConflicts: nobody is
// running the plan any more, so it cannot break anything. The DECISION
// survives — it is a standing agreement, not one agent's opinion.
func TestIntegrationAbandonedSessionSettlesItsDecisionConflicts(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := now

	bob, bobAgent := h.seedPerson("M-2", "A-2", "Bob", "codex")
	// No heartbeat for 20 minutes: past the 600s abandon threshold.
	owner := h.seedSessionFor("S-22", bobAgent, bob, enums.SESSION_STATUS_LIVE, now.Add(-20*time.Minute))
	ana := h.seedSessionFor("S-17", h.agentUUID, h.memberUUID, enums.SESSION_STATUS_LIVE, now)

	decision := h.seedDecision(testDecisionKey, enums.DECISION_STATUS_ACCEPTED, 1, ana)
	intent := h.seedIntent(owner, bob, testIntentKey, "store the session token in localStorage after login",
		enums.INTENT_STATUS_ACTIVE, 1)
	cf := h.seedDecisionConflict(testConflictKey, testDecisionKey, enums.CONFLICT_SEVERITY_MEDIUM,
		owner, bobAgent, bob, intent, ana, h.agentUUID, h.memberUUID, decision,
		now.Add(-30*time.Minute), now.Add(-30*time.Minute))

	// A pair this session was still meant to judge dies with it.
	armed := now.Add(10 * time.Minute)
	pair := h.seedPair("pair-abandoned", decision, 1, intent, 1, &owner, &armed, 1, now.Add(-time.Minute))

	rep := h.runOnce(h.sweeper(atClock(&clock)))

	if rep.SessionsAbandoned != 1 {
		t.Fatalf("the pass abandoned %d sessions, want 1", rep.SessionsAbandoned)
	}
	if rep.ConflictsResolved != 1 {
		t.Fatalf("the pass resolved %d conflicts, want 1", rep.ConflictsResolved)
	}

	var (
		status, resolution int64
		note               sql.NullString
		resolvedAt         sql.NullTime
		resolvedBy         sql.NullString
	)
	if err := h.db.QueryRow("SELECT `status`, COALESCE(`resolution`, 0), `resolution_note`, `resolved_at`, `resolved_by_member_uuid` "+
		"FROM `conflict` WHERE `id` = ?", cf.uuid.String()).Scan(&status, &resolution, &note, &resolvedAt, &resolvedBy); err != nil {
		t.Fatal(err)
	}
	if enums.ConflictStatus(status) != enums.CONFLICT_STATUS_RESOLVED {
		t.Fatalf("%s is %v after its plan's session was abandoned, want resolved", testConflictKey, enums.ConflictStatus(status))
	}
	if enums.ConflictResolution(resolution) != enums.CONFLICT_RESOLUTION_SUPERSEDED {
		t.Errorf("resolution = %v, want superseded", enums.ConflictResolution(resolution))
	}
	if !resolvedAt.Valid {
		t.Error("resolved_at is not set")
	}
	if resolvedBy.Valid {
		t.Error("resolved_by_member_uuid is set; a conflict metiche closes was settled by nobody")
	}
	// §4.8's abandoned wording, exactly.
	for _, want := range []string{"Cleared by metiche:", "S-22 (codex)", "was abandoned at", "after no heartbeat", "ending " + testIntentKey} {
		if !strings.Contains(note.String, want) {
			t.Errorf("resolution_note is missing %q:\n%s", want, note.String)
		}
	}
	t.Logf("abandoned resolution_note: %s", note.String)

	// The decision itself is untouched.
	var decisionStatus int64
	if err := h.db.QueryRow("SELECT `status` FROM `decision` WHERE `id` = ?", decision.String()).Scan(&decisionStatus); err != nil {
		t.Fatal(err)
	}
	if enums.DecisionStatus(decisionStatus) != enums.DECISION_STATUS_ACCEPTED {
		t.Errorf("the decision is %v; a session going quiet must never un-decide a standing agreement",
			enums.DecisionStatus(decisionStatus))
	}

	// Its pending pair expired, quietly.
	if st := h.pairState(pair); st.status != enums.JUDGEMENT_STATUS_EXPIRED {
		t.Errorf("the abandoned session's pair is %v, want expired", st.status)
	}

	var structural bool
	if err := h.db.QueryRow("SELECT `structural` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_RESOLVED), cf.uuid.String()).Scan(&structural); err != nil {
		t.Fatalf("no conflict_resolved event for %s: %v", testConflictKey, err)
	}
	if !structural {
		t.Error("conflict_resolved is not structural; the board would not repaint")
	}

	// Idempotent.
	rep2 := h.runOnce(h.sweeper(atClock(&clock)))
	if rep2.ConflictsResolved != 0 {
		t.Errorf("a second pass resolved %d conflicts again, want 0", rep2.ConflictsResolved)
	}
}

// ─────────────────────────────────────────────
// (c) Asking a person (§4.7)
// ─────────────────────────────────────────────

// escalationWorld is the standing setup for the escalation tests: Bob's plan
// breaks Ana's decision, both agents are live, and nothing has moved since the
// verdict.
type escalationWorld struct {
	ana, bob             uuid.UUID
	anaAgent, bobAgent   uuid.UUID
	anaSession, bobSess  uuid.UUID
	decision, intent     uuid.UUID
	conflict             decisionConflict
	base                 time.Time
	severity             enums.ConflictSeverity
	intentStatus         enums.IntentStatus
	withLiveDeciderAgent bool
}

func (h *harness) seedEscalation(base time.Time, severity enums.ConflictSeverity,
	intentStatus enums.IntentStatus, openSince time.Time) escalationWorld {
	h.t.Helper()
	w := escalationWorld{base: base, severity: severity, intentStatus: intentStatus, withLiveDeciderAgent: true}
	w.ana, w.anaAgent = h.memberUUID, h.agentUUID
	w.bob, w.bobAgent = h.seedPerson("M-2", "A-2", "Bob", "codex")
	w.anaSession = h.seedSessionFor("S-17", w.anaAgent, w.ana, enums.SESSION_STATUS_LIVE, base)
	w.bobSess = h.seedSessionFor("S-22", w.bobAgent, w.bob, enums.SESSION_STATUS_LIVE, base)
	w.decision = h.seedDecision(testDecisionKey, enums.DECISION_STATUS_ACCEPTED, 1, w.anaSession)
	w.intent = h.seedIntent(w.bobSess, w.bob, testIntentKey,
		"store the session token in localStorage after login", intentStatus, 1)
	w.conflict = h.seedDecisionConflict(testConflictKey, testDecisionKey, severity,
		w.bobSess, w.bobAgent, w.bob, w.intent,
		w.anaSession, w.anaAgent, w.ana, w.decision,
		openSince, openSince)
	return w
}

// TestIntegrationDecisionConflictEscalatesExactlyOnce is §4.7 end to end: past
// the budget, above the floor, with the plan still live, metiche asks both
// people — once, and never again.
func TestIntegrationDecisionConflictEscalatesExactlyOnce(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base

	// The project's cadence is hackathon, so the budget is 10 minutes.
	w := h.seedEscalation(base, enums.CONFLICT_SEVERITY_HIGH, enums.INTENT_STATUS_ACTIVE, base.Add(-11*time.Minute))

	rep := h.runOnce(h.sweeper(atClock(&clock)))

	if rep.ConflictsEscalated != 1 {
		t.Fatalf("the pass escalated %d conflicts, want 1", rep.ConflictsEscalated)
	}
	at := h.conflictEscalatedAt(w.conflict.uuid)
	if !at.Valid || !at.Time.UTC().Equal(clock) {
		t.Fatalf("escalated_at = %v, want %v", at.Time, clock)
	}

	// One event, structural, worded as §4.7 says.
	var (
		summary    sql.NullString
		payload    sql.NullString
		structural bool
		subjectKey sql.NullString
	)
	if err := h.db.QueryRow("SELECT `summary`, `payload`, `structural`, `subject_key` FROM `team_event` "+
		"WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_ESCALATED), w.conflict.uuid.String()).
		Scan(&summary, &payload, &structural, &subjectKey); err != nil {
		t.Fatalf("no conflict_escalated event: %v", err)
	}
	if !structural {
		t.Error("conflict_escalated is not structural; the board would not repaint the conflict card")
	}
	if want := testConflictKey + ": a person was asked about " + testDecisionKey; summary.String != want {
		t.Errorf("summary = %q, want %q", summary.String, want)
	}
	if subjectKey.String != testConflictKey {
		t.Errorf("subject_key = %q, want %q", subjectKey.String, testConflictKey)
	}
	// Decoded through the entity rather than string-matched: the JSON column
	// reformats what it stores, and what matters is that a board reading the
	// event back gets the severity, the conflict and the question itself.
	p := payload_entity.EventPayloadFromJSON([]byte(payload.String))
	if p.Severity != enums.CONFLICT_SEVERITY_HIGH {
		t.Errorf("payload severity = %v, want high", p.Severity)
	}
	if p.ConflictUUID == nil || p.ConflictUUID.String() != w.conflict.uuid.String() {
		t.Errorf("payload points at %v, want the conflict %s", p.ConflictUUID, w.conflict.uuid)
	}
	if !strings.Contains(p.Message.String, "report_back") {
		t.Errorf("payload message is not the question that was asked:\n%s", p.Message.String)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_ESCALATED)); n != 1 {
		t.Errorf("%d conflict_escalated events, want exactly 1", n)
	}

	// One question to the plan's owner and one to the live decider.
	qs := h.questions()
	if len(qs) != 2 {
		t.Fatalf("%d instructions raised, want 2 (the plan owner and the decider)", len(qs))
	}
	if rep.InstructionsRaised != 2 {
		t.Errorf("the report counted %d instructions, want 2", rep.InstructionsRaised)
	}
	byTarget := map[string]questionRow{}
	for _, q := range qs {
		byTarget[q.targetSession] = q
		if q.kind != enums.INSTRUCTION_KIND_QUESTION {
			t.Errorf("instruction %s is %v, want question", q.key, q.kind)
		}
		if !q.requiresReport {
			t.Errorf("instruction %s does not require a report; the whole point is that the agent comes back with the answer", q.key)
		}
		if q.refUUID != w.conflict.uuid.String() {
			t.Errorf("instruction %s points at %q, want the conflict", q.key, q.refUUID)
		}
		if n := utf8.RuneCountInString(q.body); n > escalationQuestionChars {
			t.Errorf("instruction %s is %d characters; get_instructions shows at most %d:\n%s",
				q.key, n, escalationQuestionChars, q.body)
		}
		if !strings.HasSuffix(q.body, "Then report_back their answer.") {
			t.Errorf("instruction %s lost the one sentence that says what to do:\n%s", q.key, q.body)
		}
		if want := clock.Add(escalationInstructionTTL); !q.expiresAt.Valid || !q.expiresAt.Time.UTC().Equal(want) {
			t.Errorf("instruction %s expires at %v, want %v", q.key, q.expiresAt.Time, want)
		}
	}
	owner, ok := byTarget[w.bobSess.String()]
	if !ok {
		t.Fatal("the plan's owner was not asked anything")
	}
	for _, want := range []string{testConflictKey, testDecisionKey, "unsettled 10m", "follow the decision", "agree with Ana", "Plan: \""} {
		if !strings.Contains(owner.body, want) {
			t.Errorf("the plan owner's question is missing %q:\n%s", want, owner.body)
		}
	}
	t.Logf("plan owner question (%d chars): %s", utf8.RuneCountInString(owner.body), owner.body)

	decider, ok := byTarget[w.anaSession.String()]
	if !ok {
		t.Fatal("the decider's agent was not asked anything")
	}
	for _, want := range []string{testConflictKey, testDecisionKey, "unsettled 10m", "keep the decision", "Bob's plan"} {
		if !strings.Contains(decider.body, want) {
			t.Errorf("the decider's question is missing %q:\n%s", want, decider.body)
		}
	}
	t.Logf("decider question (%d chars): %s", utf8.RuneCountInString(decider.body), decider.body)

	// Once. A second pass, further past the budget, writes nothing at all.
	clock = base.Add(time.Hour)
	h.beat(w.anaSession, clock)
	h.beat(w.bobSess, clock)
	rep2 := h.runOnce(h.sweeper(atClock(&clock)))
	if rep2.ConflictsEscalated != 0 {
		t.Errorf("a second pass escalated %d conflicts again, want 0", rep2.ConflictsEscalated)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_ESCALATED)); n != 1 {
		t.Errorf("%d conflict_escalated events after two passes, want 1", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 2 {
		t.Errorf("%d instructions after two passes, want 2 — a person is asked once", n)
	}
	if at2 := h.conflictEscalatedAt(w.conflict.uuid); !at2.Time.UTC().Equal(at.Time.UTC()) {
		t.Errorf("escalated_at moved from %v to %v", at.Time, at2.Time)
	}
}

// TestIntegrationDecisionConflictBelowTheHumanFloorIsNeverEscalated: below the
// floor a conflict lives on the board and interrupts nobody, forever.
func TestIntegrationDecisionConflictBelowTheHumanFloorIsNeverEscalated(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base

	// Medium, under the default human floor of high, and long past the budget.
	w := h.seedEscalation(base, enums.CONFLICT_SEVERITY_MEDIUM, enums.INTENT_STATUS_ACTIVE, base.Add(-24*time.Hour))

	rep := h.runOnce(h.sweeper(atClock(&clock)))

	if rep.ConflictsEscalated != 0 {
		t.Fatalf("a medium conflict escalated %d times; the human floor is high", rep.ConflictsEscalated)
	}
	if at := h.conflictEscalatedAt(w.conflict.uuid); at.Valid {
		t.Errorf("escalated_at = %v on a conflict below the floor", at.Time)
	}
	if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("%d people were interrupted below the human floor, want 0", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_ESCALATED)); n != 0 {
		t.Errorf("%d conflict_escalated events below the floor, want 0", n)
	}
}

// TestIntegrationDecisionConflictWaitsForTheWholeBudget: one second early is
// early. The budget is the agents' turn to settle it themselves.
func TestIntegrationDecisionConflictWaitsForTheWholeBudget(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	budget := coordination.EscalationBudget(enums.ProjectCadence(enums.PROJECT_CADENCE_HACKATHON).String())

	// Everything happened exactly one second short of a full budget ago.
	clock := base
	w := h.seedEscalation(base, enums.CONFLICT_SEVERITY_HIGH, enums.INTENT_STATUS_ACTIVE, base.Add(-budget+time.Second))

	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.ConflictsEscalated != 0 {
		t.Fatalf("a conflict one second short of its %s budget escalated %d times, want 0", budget, rep.ConflictsEscalated)
	}
	if at := h.conflictEscalatedAt(w.conflict.uuid); at.Valid {
		t.Fatal("escalated_at is set before the budget elapsed")
	}

	// One second later it is exactly at the budget, and inclusive.
	clock = base.Add(time.Second)
	h.beat(w.anaSession, clock)
	h.beat(w.bobSess, clock)
	rep2 := h.runOnce(h.sweeper(atClock(&clock)))
	if rep2.ConflictsEscalated != 1 {
		t.Fatalf("at exactly the %s budget the pass escalated %d conflicts, want 1", budget, rep2.ConflictsEscalated)
	}
}

// TestIntegrationFinishedPlanIsNeverEscalated: there is nothing to ask a
// person about when the plan that broke the decision is over.
func TestIntegrationFinishedPlanIsNeverEscalated(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base

	w := h.seedEscalation(base, enums.CONFLICT_SEVERITY_HIGH, enums.INTENT_STATUS_DONE, base.Add(-time.Hour))

	rep := h.runOnce(h.sweeper(atClock(&clock)))

	if rep.ConflictsEscalated != 0 {
		t.Fatalf("a conflict whose plan is done escalated %d times, want 0", rep.ConflictsEscalated)
	}
	if at := h.conflictEscalatedAt(w.conflict.uuid); at.Valid {
		t.Errorf("escalated_at = %v on a conflict whose plan is finished", at.Time)
	}
	if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 0 {
		t.Errorf("%d people were asked about a finished plan, want 0", n)
	}
}

// TestIntegrationReopenedConflictCanBeAskedAboutOnceMore: reopening clears
// escalated_at (§4.6), and the idempotency key carries occurrence_count, so
// the second round of disagreement earns its own question rather than being
// collapsed into the first one's event.
func TestIntegrationReopenedConflictCanBeAskedAboutOnceMore(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base

	w := h.seedEscalation(base, enums.CONFLICT_SEVERITY_HIGH, enums.INTENT_STATUS_ACTIVE, base.Add(-11*time.Minute))

	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.ConflictsEscalated != 1 {
		t.Fatalf("the first escalation did not fire: %d", rep.ConflictsEscalated)
	}

	// What report_judgement does when a fresh conflict verdict reopens a
	// conflict metiche had closed: status open, escalated_at cleared, the
	// occurrence counted.
	reopened := base.Add(time.Hour)
	h.exec("UPDATE `conflict` SET `status` = ?, `resolution` = NULL, `resolution_note` = NULL, `resolved_at` = NULL, "+
		"`escalated_at` = NULL, `occurrence_count` = 2, `last_detected_at` = ? WHERE `id` = ?",
		int64(enums.CONFLICT_STATUS_OPEN), reopened, w.conflict.uuid.String())
	h.exec("UPDATE `conflict_participant` SET `notified_at` = ? WHERE `conflict_uuid` = ?",
		reopened, w.conflict.uuid.String())

	// Still inside the new budget: the agents get a full turn after their last
	// exchange, every time.
	clock = reopened.Add(9 * time.Minute)
	h.beat(w.anaSession, clock)
	h.beat(w.bobSess, clock)
	if rep := h.runOnce(h.sweeper(atClock(&clock))); rep.ConflictsEscalated != 0 {
		t.Fatalf("the reopened conflict escalated %d times before its new budget elapsed, want 0", rep.ConflictsEscalated)
	}

	clock = reopened.Add(11 * time.Minute)
	h.beat(w.anaSession, clock)
	h.beat(w.bobSess, clock)
	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.ConflictsEscalated != 1 {
		t.Fatalf("the reopened conflict escalated %d times past its new budget, want 1", rep.ConflictsEscalated)
	}
	if at := h.conflictEscalatedAt(w.conflict.uuid); !at.Valid || !at.Time.UTC().Equal(clock) {
		t.Errorf("escalated_at = %v, want %v", at.Time, clock)
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_ESCALATED)); n != 2 {
		t.Errorf("%d conflict_escalated events, want 2 — the second occurrence has its own idempotency key", n)
	}
	if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", h.teamUUID.String()); n != 4 {
		t.Errorf("%d instructions after two escalations, want 4", n)
	}
}

// TestIntegrationEscalationIsClaimedOnce drives the race the conditional
// UPDATE exists for, deterministically: two claims on one conflict, the second
// arriving after the first has landed. Exactly one may win.
//
// This is what stands between a person and being asked twice about the same
// disagreement when two pods sweep the same team in the same instant.
func TestIntegrationEscalationIsClaimedOnce(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	ctx := context.Background()

	w := h.seedEscalation(base, enums.CONFLICT_SEVERITY_HIGH, enums.INTENT_STATUS_ACTIVE, base.Add(-11*time.Minute))

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	first, err := claimEscalation(ctx, tx, w.conflict.uuid, base)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("the first claim on an un-escalated conflict was refused; nobody would ever be asked")
	}

	later := base.Add(time.Minute)
	second, err := claimEscalation(ctx, tx, w.conflict.uuid, later)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("a second claim on an already-escalated conflict succeeded; the person is asked twice about one disagreement")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	at := h.conflictEscalatedAt(w.conflict.uuid)
	if !at.Valid {
		t.Fatal("escalated_at is not set after a winning claim")
	}
	if !at.Time.UTC().Equal(base) {
		t.Errorf("escalated_at = %v, want %v — the losing claim moved the clock", at.Time, base)
	}
}

// TestIntegrationDecisionConflictEscalatesOnlyAtOrAboveTheHumanFloor walks the
// whole severity ladder against the default floor of high.
//
// The two halves matter for different reasons. Below the floor, silence is the
// feature: most disagreements are worth recording and worth nobody's evening.
// At and ABOVE it, the scan has to reach them — a floor written as a range
// with the wrong end open would quietly drop the most serious conflicts there
// are, which is the one failure nobody would notice from the outside.
func TestIntegrationDecisionConflictEscalatesOnlyAtOrAboveTheHumanFloor(t *testing.T) {
	cases := []struct {
		severity enums.ConflictSeverity
		want     int
	}{
		{enums.CONFLICT_SEVERITY_LOW, 0},
		{enums.CONFLICT_SEVERITY_MEDIUM, 0},
		{enums.CONFLICT_SEVERITY_HIGH, 1},
		{enums.CONFLICT_SEVERITY_CRITICAL, 1},
	}
	for _, c := range cases {
		t.Run(c.severity.String(), func(t *testing.T) {
			h := newHarness(t)
			base := time.Now().UTC().Truncate(time.Second)
			clock := base

			// An hour past a ten-minute budget: time is never the reason here.
			w := h.seedEscalation(base, c.severity, enums.INTENT_STATUS_ACTIVE, base.Add(-time.Hour))

			rep := h.runOnce(h.sweeper(atClock(&clock)))
			if rep.ConflictsEscalated != c.want {
				t.Fatalf("a %v conflict escalated %d times, want %d (the human floor is high)",
					c.severity, rep.ConflictsEscalated, c.want)
			}
			if got := h.conflictEscalatedAt(w.conflict.uuid).Valid; got != (c.want == 1) {
				t.Errorf("escalated_at set = %v for a %v conflict, want %v", got, c.severity, c.want == 1)
			}
			wantInstructions := 0
			if c.want == 1 {
				wantInstructions = 2
			}
			if n := h.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", h.teamUUID.String()); n != wantInstructions {
				t.Errorf("%d people were asked about a %v conflict, want %d", n, c.severity, wantInstructions)
			}
		})
	}
}

// ─────────────────────────────────────────────
// Pure: the numbers and the wording
// ─────────────────────────────────────────────

// TestEscalationBudgetsAreTheSpecNumbers pins this package's documented
// budgets to coordination's, which is the one that actually decides. If the
// two ever disagree, the comment in sweeper.go is lying to its reader.
func TestEscalationBudgetsAreTheSpecNumbers(t *testing.T) {
	cases := []struct {
		cadence enums.ProjectCadence
		want    time.Duration
	}{
		{enums.PROJECT_CADENCE_HACKATHON, DefaultEscalationHackathon},
		{enums.PROJECT_CADENCE_SPRINT, DefaultEscalationSprint},
		{enums.PROJECT_CADENCE_STEADY, DefaultEscalationSteady},
		// An unrecognised cadence lands on sprint, never on the impatient end.
		{enums.ProjectCadence(99), DefaultEscalationSprint},
	}
	for _, c := range cases {
		if got := coordination.EscalationBudget(c.cadence.String()); got != c.want {
			t.Errorf("cadence %v: budget %s, want %s", c.cadence, got, c.want)
		}
	}
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{10 * time.Minute, "10m"}, {30 * time.Minute, "30m"}, {2 * time.Hour, "2h"}} {
		if got := renderBudget(c.d); got != c.want {
			t.Errorf("renderBudget(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestEscalationQuestionsAlwaysFitAndKeepTheirInstruction is the property that
// matters about the wording: whatever a team names its decisions and however
// long a plan's summary is, the question fits what get_instructions shows and
// still ends with what the agent must do.
func TestEscalationQuestionsAlwaysFitAndKeepTheirInstruction(t *testing.T) {
	long := strings.Repeat("a", 200)
	cases := []struct {
		name string
		st   escalationState
	}{
		{"the spec's own example", escalationState{
			conflictKey: testConflictKey, decisionKey: testDecisionKey,
			intentSummary: "store the session token in localStorage after login",
			owner:         escalationSide{sessionKey: "S-22", agentLabel: "codex", memberName: "Bob"},
			decider:       escalationSide{sessionKey: "S-17", agentLabel: "claude", memberName: "Ana"},
		}},
		{"everything at its column limit", escalationState{
			conflictKey: strings.Repeat("C", 32), decisionKey: "#" + strings.Repeat("k", 79),
			intentSummary: long,
			owner:         escalationSide{sessionKey: strings.Repeat("S", 32), agentLabel: long, memberName: long},
			decider:       escalationSide{sessionKey: strings.Repeat("D", 32), agentLabel: long, memberName: long},
		}},
		{"nothing known at all", escalationState{}},
		{"a summary that would close its own quotation", escalationState{
			conflictKey: testConflictKey, decisionKey: testDecisionKey,
			intentSummary: "use \"localStorage\"\n\tfor  the token",
			owner:         escalationSide{sessionKey: "S-22", memberName: "Bob"},
			decider:       escalationSide{sessionKey: "S-17", memberName: "Ana"},
		}},
	}
	for _, c := range cases {
		for _, budget := range []time.Duration{10 * time.Minute, 30 * time.Minute, 2 * time.Hour} {
			for who, body := range map[string]string{
				"plan owner": planOwnerQuestion(c.st, budget),
				"decider":    deciderQuestion(c.st, budget),
			} {
				if n := utf8.RuneCountInString(body); n > escalationQuestionChars {
					t.Errorf("%s / %s / %s: %d characters, max %d:\n%s", c.name, who, budget, n, escalationQuestionChars, body)
				}
				if !strings.HasSuffix(body, "Then report_back their answer.") {
					t.Errorf("%s / %s / %s: lost its instruction:\n%s", c.name, who, budget, body)
				}
				if strings.Count(body, `"`) != 2 {
					t.Errorf("%s / %s / %s: the quoted plan does not close cleanly:\n%s", c.name, who, budget, body)
				}
				if strings.ContainsAny(body, "\n\t") {
					t.Errorf("%s / %s / %s: a newline reached the body:\n%s", c.name, who, budget, body)
				}
			}
		}
	}
}

// TestIntegrationReArmOnlyEverExtendsThePairsOwnJudge holds the ownership
// clause on rearmPair's UPDATE, which is the one thing standing between §4.1
// and handing one agent's plan to another agent to judge.
//
// (a) Two live sessions, each with a pair of its own whose window lapsed: one
// pass extends both and moves neither judge.
//
// (b) The case the clause exists for. The backlog is read OUTSIDE any lock, so
// between that scan and this UPDATE another pod can re-assign or expire the
// row. This recreates that divergence directly — a pendingPair carrying a
// judge the stored row no longer has — and the write must refuse it. Without
// `AND judge_session_uuid = ?` the UPDATE lands on `id` alone and quietly
// moves Bob's pair onto Ana's session with a fresh window.
func TestIntegrationReArmOnlyEverExtendsThePairsOwnJudge(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	clock := base

	bob, bobAgent := h.seedPerson("M-2", "A-2", "Bob", "codex")
	bobSession := h.seedSessionFor("S-22", bobAgent, bob, enums.SESSION_STATUS_LIVE, base)
	anaSession := h.seedSessionFor("S-17", h.agentUUID, h.memberUUID, enums.SESSION_STATUS_LIVE, base)

	bobDecision := h.seedDecision("#test-bob", enums.DECISION_STATUS_ACCEPTED, 1, uuid.Nil)
	anaDecision := h.seedDecision("#test-ana", enums.DECISION_STATUS_ACCEPTED, 1, uuid.Nil)
	bobIntent := h.seedIntent(bobSession, bob, testIntentKey, "read the session from localStorage",
		enums.INTENT_STATUS_ACTIVE, 1)
	anaIntent := h.seedIntent(anaSession, h.memberUUID, "INT-92", "set the cookie on login",
		enums.INTENT_STATUS_ACTIVE, 1)

	lapsed := base.Add(-time.Minute)
	bobPair := h.seedPair("pair-bob", bobDecision, 1, bobIntent, 1, &bobSession, &lapsed, 1, lapsed)
	anaPair := h.seedPair("pair-ana", anaDecision, 1, anaIntent, 1, &anaSession, &lapsed, 1, lapsed)

	// (a) One pass, two re-arms, each staying where it belongs.
	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.JudgementsReArmed != 2 {
		t.Fatalf("the pass re-armed %d pairs, want 2", rep.JudgementsReArmed)
	}
	for _, c := range []struct {
		name  string
		pair  uuid.UUID
		judge uuid.UUID
	}{{"Bob's", bobPair, bobSession}, {"Ana's", anaPair, anaSession}} {
		st := h.pairState(c.pair)
		if st.judge != c.judge.String() {
			t.Errorf("%s pair now sits with %s; a pair only ever belongs to the plan's own session %s",
				c.name, st.judge, c.judge)
		}
		if st.count != 2 {
			t.Errorf("%s pair is at assignment_count %d, want 2", c.name, st.count)
		}
	}

	// (b) The stale read: this pass believes Ana's session holds Bob's pair.
	// The row says otherwise, and the row wins.
	before := h.pairState(bobPair)
	var out Report
	stale := pendingPair{
		id:              bobPair.String(),
		judgeSession:    anaSession.String(),
		assignmentCount: before.count,
	}
	if err := h.sweeper(atClock(&clock)).rearmPair(context.Background(), stale, decisionJudgeWindow, clock, &out); err != nil {
		t.Fatalf("rearmPair: %v", err)
	}
	if out.JudgementsReArmed != 0 {
		t.Errorf("re-arming reported %d pairs extended for a judge the row does not have, want 0", out.JudgementsReArmed)
	}
	after := h.pairState(bobPair)
	if after.judge != bobSession.String() {
		t.Fatalf("Bob's pair now sits with %s, want its own session %s — re-arming handed one agent's plan to another",
			after.judge, bobSession)
	}
	if after.count != before.count {
		t.Errorf("assignment_count moved %d → %d for an UPDATE that named the wrong judge", before.count, after.count)
	}
	if !before.expires.Valid || !after.expires.Valid || !after.expires.Time.Equal(before.expires.Time) {
		t.Errorf("judging_expires_at moved %v → %v for an UPDATE that named the wrong judge",
			before.expires.Time, after.expires.Time)
	}

	// Ana's pair was never in this UPDATE's sights and must be untouched too.
	if st := h.pairState(anaPair); st.judge != anaSession.String() || st.count != 2 {
		t.Errorf("Ana's pair is now judge=%s count=%d, want %s and 2", st.judge, st.count, anaSession)
	}
}

// TestSubjectsValidIsBothRevisionsAndBothLives walks the table in §4.4 without
// a database, because every one of these is a reason not to spend an agent's
// attention and each deserves to be named.
func TestSubjectsValidIsBothRevisionsAndBothLives(t *testing.T) {
	ok := pendingPair{
		decisionRevision: 2, intentRevision: 5,
		decisionStatus: enums.DECISION_STATUS_ACCEPTED, decisionCurrent: 2,
		intentStatus: enums.INTENT_STATUS_ACTIVE, intentCurrent: 5,
		intentSession: "a-session", intentSessionAge: enums.SESSION_STATUS_LIVE,
	}
	if !ok.subjectsValid() {
		t.Fatal("a live pair at both current revisions is not valid")
	}
	cases := map[string]func(p *pendingPair){
		"the decision was superseded":   func(p *pendingPair) { p.decisionStatus = enums.DECISION_STATUS_SUPERSEDED },
		"the decision was revoked":      func(p *pendingPair) { p.decisionStatus = enums.DECISION_STATUS_REVOKED },
		"the decision row is gone":      func(p *pendingPair) { p.decisionStatus = 0; p.decisionCurrent = 0 },
		"the decision has been revised": func(p *pendingPair) { p.decisionCurrent = 3 },
		"the plan has been revised":     func(p *pendingPair) { p.intentCurrent = 6 },
		"the plan is done":              func(p *pendingPair) { p.intentStatus = enums.INTENT_STATUS_DONE },
		"the plan was abandoned":        func(p *pendingPair) { p.intentStatus = enums.INTENT_STATUS_ABANDONED },
		"the plan was superseded":       func(p *pendingPair) { p.intentStatus = enums.INTENT_STATUS_SUPERSEDED },
		"the plan row is gone":          func(p *pendingPair) { p.intentStatus = 0; p.intentSession = "" },
		"its session ended":             func(p *pendingPair) { p.intentSessionAge = enums.SESSION_STATUS_ENDED },
		"its session was abandoned":     func(p *pendingPair) { p.intentSessionAge = enums.SESSION_STATUS_ABANDONED },
		"its session row is gone":       func(p *pendingPair) { p.intentSessionAge = 0 },
	}
	for name, mutate := range cases {
		p := ok
		mutate(&p)
		if p.subjectsValid() {
			t.Errorf("a pair is still valid when %s", name)
		}
	}

	// A stale session is not a dead one: an agent between heartbeats is still
	// working, and its pair still deserves its window.
	stale := ok
	stale.intentSessionAge = enums.SESSION_STATUS_STALE
	if !stale.subjectsValid() {
		t.Error("a pair on a stale session is not valid; a missed heartbeat is not a finished plan")
	}
	declared := ok
	declared.intentStatus = enums.INTENT_STATUS_DECLARED
	if !declared.subjectsValid() {
		t.Error("a pair on a declared plan is not valid")
	}
}
