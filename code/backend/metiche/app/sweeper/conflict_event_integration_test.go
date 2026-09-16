package sweeper

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// What a settled conflict SAYS, not merely that it was settled.
//
// The three settle paths in conflicts.go each hand appendEvent a finish
// callback carrying the explanation that mcp.SettleConflict and its siblings
// worked out under the team lock. These tests read the stored team_event row
// back and hold it to what app/mcp's own appendConflictResolvedEvent writes
// for the same situation: a conflict settled by an agent and one settled by
// the sweeper must read identically on the board and in the event feed.
//
// Real MySQL, through the same harness as the rest of the package.

// resolvedEvent is the stored conflict_resolved row for one conflict — every
// column the board and the feed read off it.
type resolvedEvent struct {
	summary        sql.NullString
	subjectKey     sql.NullString
	payload        sql.NullString
	subjectKind    sql.NullInt64
	structural     bool
	sequence       int64
	idempotencyKey string
}

func (h *harness) resolvedEvent(conflict uuid.UUID) resolvedEvent {
	h.t.Helper()
	var e resolvedEvent
	if err := h.db.QueryRow(
		"SELECT `summary`, `subject_key`, `payload`, `subject_kind`, `structural`, `sequence`, `idempotency_key` "+
			"FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_RESOLVED), conflict.String()).
		Scan(&e.summary, &e.subjectKey, &e.payload, &e.subjectKind, &e.structural, &e.sequence, &e.idempotencyKey); err != nil {
		h.t.Fatalf("no conflict_resolved event for %s: %v", conflict, err)
	}
	return e
}

// resolutionNote is what the settle wrote on the conflict itself. The event's
// payload.message must be the same sentence — that is the whole point of
// carrying it out of the locked transaction.
func (h *harness) resolutionNote(conflict uuid.UUID) string {
	h.t.Helper()
	var note sql.NullString
	if err := h.db.QueryRow("SELECT `resolution_note` FROM `conflict` WHERE `id` = ?", conflict.String()).Scan(&note); err != nil {
		h.t.Fatalf("reading the resolution note: %v", err)
	}
	return note.String
}

// checkResolvedEvent is the shared assertion: the summary, the subject key and
// every payload field app/mcp fills in, plus the structural flag and the
// idempotency key, which this change must leave exactly as they were.
func checkResolvedEvent(t *testing.T, h *harness, conflict uuid.UUID, wantSummary, wantKey string,
	wantResolution enums.ConflictResolution, wantSeverity enums.ConflictSeverity, wantPaths []string) {
	t.Helper()
	ev := h.resolvedEvent(conflict)

	t.Logf("stored team_event for %s:\n  summary     = %s\n  subject_key = %s\n  payload     = %s",
		wantKey, nullText(ev.summary), nullText(ev.subjectKey), nullText(ev.payload))

	if !ev.summary.Valid || ev.summary.String == "" {
		t.Errorf("summary is NULL; the board and the feed show a settled conflict with no explanation at all")
	} else if ev.summary.String != wantSummary {
		t.Errorf("summary =\n  %q\nwant\n  %q", ev.summary.String, wantSummary)
	}
	if !ev.subjectKey.Valid || ev.subjectKey.String != wantKey {
		t.Errorf("subject_key = %s, want %q", nullText(ev.subjectKey), wantKey)
	}
	if !ev.payload.Valid || ev.payload.String == "" {
		t.Fatalf("payload is NULL; the resolution, the status change and the note never reach the feed")
	}

	p := payload_entity.EventPayloadFromJSON([]byte(ev.payload.String))
	if note := h.resolutionNote(conflict); p.Message.String != note {
		t.Errorf("payload.message =\n  %q\nwant the conflict's own resolution note\n  %q", p.Message.String, note)
	}
	if p.PreviousStatus.String != enums.ConflictStatus(enums.CONFLICT_STATUS_OPEN).String() {
		t.Errorf("payload.previous_status = %q, want %q", p.PreviousStatus.String,
			enums.ConflictStatus(enums.CONFLICT_STATUS_OPEN).String())
	}
	if p.NewStatus.String != enums.ConflictStatus(enums.CONFLICT_STATUS_RESOLVED).String() {
		t.Errorf("payload.new_status = %q, want %q", p.NewStatus.String,
			enums.ConflictStatus(enums.CONFLICT_STATUS_RESOLVED).String())
	}
	if p.Detail.String != wantResolution.String() {
		t.Errorf("payload.detail = %q, want the resolution %q", p.Detail.String, wantResolution.String())
	}
	if p.Severity != wantSeverity {
		t.Errorf("payload.severity = %v, want %v", p.Severity, wantSeverity)
	}
	if p.ConflictUUID == nil || *p.ConflictUUID != conflict {
		t.Errorf("payload.conflict_uuid = %v, want %s", p.ConflictUUID, conflict)
	}
	if strings.Join(p.Paths, ",") != strings.Join(wantPaths, ",") {
		t.Errorf("payload.paths = %v, want %v", p.Paths, wantPaths)
	}

	// Untouched by the summary fix, and asserted here so it stays that way.
	if !ev.structural {
		t.Error("conflict_resolved is not structural; the board would not repaint")
	}
	if ev.subjectKind.Int64 != int64(enums.SUBJECT_KIND_CONFLICT) {
		t.Errorf("subject_kind = %d, want conflict", ev.subjectKind.Int64)
	}
	if want := "sweep:conflict_resolved:" + conflict.String(); ev.idempotencyKey != want {
		t.Errorf("idempotency_key = %q, want %q", ev.idempotencyKey, want)
	}
	h.checkGapless()
}

// checkGapless is the log invariant every event in this package must keep: one
// event per sequence number, and team.sequence at the top of them.
func (h *harness) checkGapless() {
	h.t.Helper()
	var events, maxSeq, teamSeq int64
	if err := h.db.QueryRow("SELECT COUNT(*), COALESCE(MAX(`sequence`),0) FROM `team_event` WHERE `team_uuid` = ?",
		h.teamUUID.String()).Scan(&events, &maxSeq); err != nil {
		h.t.Fatal(err)
	}
	if err := h.db.QueryRow("SELECT `sequence` FROM `team` WHERE `id` = ?", h.teamUUID.String()).Scan(&teamSeq); err != nil {
		h.t.Fatal(err)
	}
	if events != maxSeq || teamSeq != maxSeq {
		h.t.Errorf("event log has a gap: %d events, max sequence %d, team.sequence %d", events, maxSeq, teamSeq)
	}
}

func nullText(s sql.NullString) string {
	if !s.Valid {
		return "NULL"
	}
	return s.String
}

// seedContractConflict writes an open contract conflict between two sessions'
// assertions, with the participant rows detection would have attached.
func (h *harness) seedContractConflict(key, contractKey string, kind enums.ConflictKind, rule string,
	aSession, aAgent, aMember, aAssertion uuid.UUID,
	bSession, bAgent, bMember, bAssertion uuid.UUID, detectedAt time.Time) uuid.UUID {
	h.t.Helper()
	cid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`detector_rule`,`evidence`,`suggested_action`,`occurrence_count`,`first_detected_at`,`last_detected_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		cid.String(), h.teamUUID.String(), h.projectUUID.String(), key, int64(kind),
		"dedupe-"+cid.String()[:8], int64(enums.CONFLICT_SEVERITY_HIGH), int64(enums.CONFLICT_STATUS_OPEN),
		int64(enums.DETECTED_BY_SERVER), rule, `{"overlap_path":"`+contractKey+`"}`,
		"agree on the shape", 1, detectedAt, detectedAt)

	for _, p := range []struct {
		session, agent, member, assertion uuid.UUID
		role                              enums.ParticipantRole
	}{
		{aSession, aAgent, aMember, aAssertion, enums.PARTICIPANT_ROLE_INITIATOR},
		{bSession, bAgent, bMember, bAssertion, enums.PARTICIPANT_ROLE_INCUMBENT},
	} {
		h.exec("INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
			"`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
			id(), cid.String(), h.teamUUID.String(), p.session.String(), p.agent.String(), p.member.String(),
			int64(enums.SUBJECT_KIND_CONTRACT_ASSERTION), p.assertion.String(), int64(p.role))
	}
	return cid
}

// ─────────────────────────────────────────────
// (a) A path overlap a lapsed claim settled
// ─────────────────────────────────────────────

// TestIntegrationSettledPathOverlapEventSaysWhatHappened: the claim lapsed,
// the conflict closed — and the event the board reads has to carry the same
// sentence app/mcp writes when drop_paths closes the same conflict.
func TestIntegrationSettledPathOverlapEventSaysWhatHappened(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()

	ana := h.seedSession("S-19", enums.SESSION_STATUS_LIVE, now.Add(-10*time.Second))
	bob := h.seedSession("S-20", enums.SESSION_STATUS_LIVE, now.Add(-10*time.Second))
	lapsed := h.seedClaim(ana, "C-1", "app/rest.go", now.Add(-time.Minute))
	held := h.seedClaim(bob, "C-2", "app/rest.go", now.Add(time.Hour))
	cf := h.seedPathConflict("CF-23", "app/rest.go", ana, lapsed, bob, held)

	rep := h.runOnce(h.sweeper(Options{}))
	if rep.ConflictsResolved != 1 {
		t.Fatalf("the pass resolved %d conflicts, want 1", rep.ConflictsResolved)
	}

	checkResolvedEvent(t, h, cf,
		"CF-23 settled (yielded): the overlap on app/rest.go cleared",
		"CF-23", enums.CONFLICT_RESOLUTION_YIELDED, enums.CONFLICT_SEVERITY_HIGH,
		[]string{"app/rest.go"})
}

// ─────────────────────────────────────────────
// (b) A contract conflict an abandoned session settled
// ─────────────────────────────────────────────

// TestIntegrationSettledContractConflictEventSaysWhatHappened: the consumer
// went quiet, its assertion stopped counting, and the mismatch closed. The
// feed must say so rather than showing a blank line.
func TestIntegrationSettledContractConflictEventSaysWhatHappened(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := now

	bob, bobAgent := h.seedPerson("M-2", "A-2", "Bob", "codex")
	// No heartbeat for 20 minutes: past the 600s abandon threshold.
	consumer := h.seedSessionFor("S-22", bobAgent, bob, enums.SESSION_STATUS_LIVE, now.Add(-20*time.Minute))
	producer := h.seedSessionFor("S-17", h.agentUUID, h.memberUUID, enums.SESSION_STATUS_LIVE, now)

	contract := h.seedContract("POST /api/login")
	consumes := h.seedAssertion(contract, consumer, enums.ASSERTION_ROLE_CONSUMES, now.Add(-time.Hour))
	produces := h.seedAssertion(contract, producer, enums.ASSERTION_ROLE_PRODUCES, now.Add(-time.Hour))

	cf := h.seedContractConflict("CF-40", "POST /api/login", enums.CONFLICT_KIND_CONTRACT_MISMATCH,
		"contract_mismatch.shape",
		consumer, bobAgent, bob, consumes,
		producer, h.agentUUID, h.memberUUID, produces, now.Add(-30*time.Minute))

	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.SessionsAbandoned != 1 {
		t.Fatalf("the pass abandoned %d sessions, want 1", rep.SessionsAbandoned)
	}
	if rep.ConflictsResolved != 1 {
		t.Fatalf("the pass resolved %d conflicts, want 1", rep.ConflictsResolved)
	}

	checkResolvedEvent(t, h, cf,
		"CF-40 settled (superseded): the contract mismatch on POST /api/login",
		"CF-40", enums.CONFLICT_RESOLUTION_SUPERSEDED, enums.CONFLICT_SEVERITY_HIGH,
		[]string{"POST /api/login"})
}

// ─────────────────────────────────────────────
// (c) A decision conflict an abandoned session settled
// ─────────────────────────────────────────────

// TestIntegrationSettledDecisionConflictEventSaysWhatHappened: §4.5's
// abandoned case. The decision survives; the conflict closes; and the event
// names the decision the plan no longer contradicts.
func TestIntegrationSettledDecisionConflictEventSaysWhatHappened(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := now

	bob, bobAgent := h.seedPerson("M-2", "A-2", "Bob", "codex")
	owner := h.seedSessionFor("S-22", bobAgent, bob, enums.SESSION_STATUS_LIVE, now.Add(-20*time.Minute))
	ana := h.seedSessionFor("S-17", h.agentUUID, h.memberUUID, enums.SESSION_STATUS_LIVE, now)

	decision := h.seedDecision(testDecisionKey, enums.DECISION_STATUS_ACCEPTED, 1, ana)
	intent := h.seedIntent(owner, bob, testIntentKey, "store the session token in localStorage after login",
		enums.INTENT_STATUS_ACTIVE, 1)
	cf := h.seedDecisionConflict(testConflictKey, testDecisionKey, enums.CONFLICT_SEVERITY_MEDIUM,
		owner, bobAgent, bob, intent, ana, h.agentUUID, h.memberUUID, decision,
		now.Add(-30*time.Minute), now.Add(-30*time.Minute))

	rep := h.runOnce(h.sweeper(atClock(&clock)))
	if rep.ConflictsResolved != 1 {
		t.Fatalf("the pass resolved %d conflicts, want 1", rep.ConflictsResolved)
	}

	checkResolvedEvent(t, h, cf.uuid,
		testConflictKey+" settled (superseded): the plan no longer contradicts "+testDecisionKey,
		testConflictKey, enums.CONFLICT_RESOLUTION_SUPERSEDED, enums.CONFLICT_SEVERITY_MEDIUM,
		[]string{testDecisionKey})
}
