package sweeper

// duplicates.go is the sweeper's half of duplicate work (docs/DUPLICATES.md):
// the duplicate pairs' backlog and judge window (§3.1 step 7, the decisions
// rule of DECISIONS.md §4.4 applied to a second kind), the incumbent's notice
// after the grace (§4.5), and the duplicate side of escalation (§4.8). The
// conflicts an abandoned session settles live with their siblings in
// conflicts.go.
//
// # Quiet, again
//
// A pair nobody answered expires with no event and no notice, exactly as a
// decision pair does. Staleness is by wording_revision: a pair minted against
// wording one side has since changed is a question about a plan nobody holds
// any more, so it is expired rather than re-armed.
//
// # The plan's own session, always
//
// A duplicate pair is only ever assigned or re-armed to subject b's session:
// the judge's plan (§4.3). The assignment, the re-arm and the expiry are the
// decision pass's own statements (assignPair, rearmPair, expirePair), so the
// guards that make them safe — `judge_session_uuid IS NULL` on assignment, the
// pair's own `judge_session_uuid` on a re-arm — are the same lines for both
// kinds.
//
// # The incumbent hears only if it is still standing
//
// The judge is told in its own response. The incumbent (subject a) is told by
// this pass and nobody else, only once the conflict has outlived
// coordination.DuplicateNoticeGrace — a judge that yields in time interrupts
// nobody. The decision is made twice, outside the lock to skip what is not
// due and inside appendEvent's transaction, where it counts.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/guregu/null/v6"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/app/mcp"
	conflict_evidence_entity "github.com/mklfarha/metiche/backend/entity/conflict_evidence"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

const (
	// duplicateNoticeFloor is §4.4's notify floor: below medium a duplicate is
	// on the board and interrupts nobody.
	duplicateNoticeFloor = enums.CONFLICT_SEVERITY_MEDIUM

	// duplicateNoticesPerPass bounds the notice scan (§4.5's LIMIT 16).
	duplicateNoticesPerPass = 16

	// duplicateNoticeTTL is how long the incumbent's notice stays worth
	// reading (§4.5 step 6).
	duplicateNoticeTTL = 4 * time.Hour

	// duplicatePathNoticesScanned bounds the pending notices read when
	// folding a path notice into the duplicate one (§4.5 step 5).
	duplicatePathNoticesScanned = 8

	// duplicateSessionIntentsLimit bounds one session's live plans read.
	duplicateSessionIntentsLimit = 64
)

// ─────────────────────────────────────────────
// (a) The pairs: backlog, re-arming, expiry
// ─────────────────────────────────────────────

// duplicatePair is one row of the open duplicate pairs scan: the judgement,
// plus enough of both plans to say whether the question is still worth asking.
type duplicatePair struct {
	id        string
	aRevision int64 // subject a's wording_revision when the pair was minted
	bRevision int64 // subject b's, likewise
	// judgeSession is empty for a pair in the backlog.
	judgeSession    string
	assignmentCount int64

	aStatus     enums.IntentStatus
	aCurrent    int64
	aSessionAge enums.SessionStatus

	bStatus     enums.IntentStatus
	bCurrent    int64
	bSession    string
	bSessionAge enums.SessionStatus
}

// planLive is "declared or active".
func planLive(s enums.IntentStatus) bool {
	return s == enums.INTENT_STATUS_DECLARED || s == enums.INTENT_STATUS_ACTIVE
}

// sessionWorking is "live or stale": a missed heartbeat is not a finished plan.
func sessionWorking(s enums.SessionStatus) bool {
	return s == enums.SESSION_STATUS_LIVE || s == enums.SESSION_STATUS_STALE
}

// subjectsValid is the duplicate pair's "still worth asking": both plans
// declared or active at the wording they were paired at, and both sessions
// still working. A pair whose other side's session is gone is not a
// duplicate of anything any more.
func (p duplicatePair) subjectsValid() bool {
	if !planLive(p.aStatus) || !planLive(p.bStatus) {
		return false
	}
	if p.aCurrent != p.aRevision || p.bCurrent != p.bRevision {
		return false
	}
	if p.bSession == "" {
		return false
	}
	return sessionWorking(p.aSessionAge) && sessionWorking(p.bSessionAge)
}

// pending is the pair as the decision pass's shared statements read it. The
// session it may be assigned to is subject b's, and nobody else's.
func (p duplicatePair) pending() pendingPair {
	return pendingPair{
		id:              p.id,
		judgeSession:    p.judgeSession,
		assignmentCount: p.assignmentCount,
		intentSession:   p.bSession,
	}
}

// sweepDuplicatePairs hands out the duplicate backlog, re-arms what lapsed and
// expires what is past asking. It writes no events and takes no team lock.
//
// The per-minute cap is the one decisions use: reviewsAssignedRecently counts
// a session's armed pairs of every kind, so the decision pass that ran just
// before this one has already spent its share.
func (s *Sweeper) sweepDuplicatePairs(ctx context.Context, t teamRow, now time.Time, rep *Report) error {
	pairs, err := s.loadOpenDuplicatePairs(ctx, t, now)
	if err != nil {
		return err
	}
	recent := map[string]int{}
	for _, p := range pairs {
		valid := p.subjectsValid()
		if p.judgeSession != "" {
			if valid && p.assignmentCount < judgeMaxAssignments {
				if err := s.rearmPair(ctx, p.pending(), t.judgeWindow, now, rep); err != nil {
					return err
				}
				continue
			}
			if err := s.expirePair(ctx, p.pending(), now, rep); err != nil {
				return err
			}
			continue
		}
		if !valid {
			if err := s.expirePair(ctx, p.pending(), now, rep); err != nil {
				return err
			}
			continue
		}
		n, seen := recent[p.bSession]
		if !seen {
			n, err = s.reviewsAssignedRecently(ctx, p.bSession, now, t.maxReviewsPerMinute)
			if err != nil {
				return err
			}
		}
		if n >= t.maxReviewsPerMinute {
			// Over the cap: left exactly as it is for a later pass.
			recent[p.bSession] = n
			continue
		}
		assigned, err := s.assignPair(ctx, p.pending(), t.judgeWindow, now, rep)
		if err != nil {
			return err
		}
		if assigned {
			n++
		}
		recent[p.bSession] = n
	}
	return nil
}

// loadOpenDuplicatePairs is the decision scan with the kind flipped, off
// idx_judgement_team_open: pending duplicate pairs that are unassigned or
// whose window lapsed. The LEFT JOINs read both plans and both sessions in the
// one query; a missing row comes back as a zero status, which is not valid.
func (s *Sweeper) loadOpenDuplicatePairs(ctx context.Context, t teamRow, now time.Time) ([]duplicatePair, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT j.`id`, j.`subject_a_revision`, j.`subject_b_revision`, COALESCE(j.`judge_session_uuid`, ''), j.`assignment_count`, "+
			"COALESCE(ia.`status`, 0), COALESCE(ia.`wording_revision`, 0), COALESCE(sa.`status`, 0), "+
			"COALESCE(ib.`status`, 0), COALESCE(ib.`wording_revision`, 0), COALESCE(ib.`session_uuid`, ''), COALESCE(sb.`status`, 0) "+
			"FROM `judgement` j "+
			"LEFT JOIN `intent` ia ON ia.`id` = j.`subject_a_uuid` "+
			"LEFT JOIN `session` sa ON sa.`id` = ia.`session_uuid` "+
			"LEFT JOIN `intent` ib ON ib.`id` = j.`subject_b_uuid` "+
			"LEFT JOIN `session` sb ON sb.`id` = ib.`session_uuid` "+
			"WHERE j.`team_uuid` = ? AND j.`status` = ? AND j.`kind` = ? "+
			"AND (j.`judging_expires_at` IS NULL OR j.`judging_expires_at` < ?) "+
			"ORDER BY j.`judging_expires_at` LIMIT ?",
		t.uuid.String(), int64(enums.JUDGEMENT_STATUS_PENDING), int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		now, s.opts.MaxJudgementsPerPass)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []duplicatePair
	for rows.Next() {
		var (
			p                          duplicatePair
			aSt, aSess, bSt, bSessStat int64
		)
		if err := rows.Scan(&p.id, &p.aRevision, &p.bRevision, &p.judgeSession, &p.assignmentCount,
			&aSt, &p.aCurrent, &aSess, &bSt, &p.bCurrent, &p.bSession, &bSessStat); err != nil {
			return nil, err
		}
		p.aStatus, p.aSessionAge = enums.IntentStatus(aSt), enums.SessionStatus(aSess)
		p.bStatus, p.bSessionAge = enums.IntentStatus(bSt), enums.SessionStatus(bSessStat)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────
// (b) One duplicate conflict, as the notice and escalation read it
// ─────────────────────────────────────────────

// duplicateSide is one plan of a duplicate conflict and the agent behind it.
type duplicateSide struct {
	escalationSide
	found        bool
	sessionFound bool
	key          string
	summary      string
	intentStatus enums.IntentStatus
}

// working is "declared or active, on a session that is still working".
func (d duplicateSide) working() bool {
	return d.found && planLive(d.intentStatus) && d.sessionFound && d.live()
}

// duplicateConflictState is one open duplicate_work conflict with both plans.
// b is the yield side (the suggested yield session's plan), a the incumbent.
type duplicateConflictState struct {
	found       bool
	plansOK     bool
	conflictKey string
	projectUUID string
	severity    enums.ConflictSeverity
	notifiedAt  sql.NullTime
	maxNotified enums.ConflictSeverity
	escalatedAt sql.NullTime
	occurrence  int64
	cadence     enums.ProjectCadence
	// detected is COALESCE(last_detected_at, first_detected_at): a fresh
	// verdict or a reopen restarts the grace.
	detected time.Time
	// openSince adds the participants' notified_at, for escalation (§4.8).
	openSince  time.Time
	fieldIssue string

	a, b         duplicateSide
	participants []participantSide
}

// attached reports whether a session is a participant of the conflict.
func (st duplicateConflictState) attached(sessionUUID string) bool {
	for _, p := range st.participants {
		if p.sessionUUID == sessionUUID {
			return true
		}
	}
	return false
}

// loadDuplicateConflictState reads one open or acknowledged duplicate_work
// conflict, both of its plans by uq_intent_team_key, and its participants.
// Every read is a point lookup or LIMITed, so it is safe inside the lock.
func loadDuplicateConflictState(ctx context.Context, q decisionQueryer, teamUUID, conflictID uuid.UUID) (duplicateConflictState, error) {
	var (
		st          duplicateConflictState
		cadence     int64
		maxNotified sql.NullInt64
		detected    sql.NullTime
		yield       string
		evidence    string
	)
	err := q.QueryRowContext(ctx,
		"SELECT c.`key`, c.`project_uuid`, c.`severity`, c.`notified_at`, c.`max_severity_notified`, c.`escalated_at`, "+
			"c.`occurrence_count`, COALESCE(c.`last_detected_at`, c.`first_detected_at`), COALESCE(p.`cadence`, 0), "+
			"COALESCE(c.`suggested_yield_session_uuid`, ''), COALESCE(c.`evidence`, '{}') "+
			"FROM `conflict` c LEFT JOIN `project` p ON p.`id` = c.`project_uuid` "+
			"WHERE c.`id` = ? AND c.`team_uuid` = ? AND c.`kind` = ? AND c.`status` IN (?, ?)",
		conflictID.String(), teamUUID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED)).
		Scan(&st.conflictKey, &st.projectUUID, &st.severity, &st.notifiedAt, &maxNotified, &st.escalatedAt,
			&st.occurrence, &detected, &cadence, &yield, &evidence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return st, nil
	case err != nil:
		return st, err
	}
	st.found = true
	st.cadence = enums.ProjectCadence(cadence)
	if maxNotified.Valid {
		st.maxNotified = enums.ConflictSeverity(maxNotified.Int64)
	}
	if detected.Valid {
		st.detected = detected.Time.UTC()
	}
	st.openSince = st.detected

	ev := conflict_evidence_entity.ConflictEvidenceFromJSON([]byte(evidence))
	if len(ev.FieldIssues) > 0 {
		st.fieldIssue = ev.FieldIssues[0]
	}
	aKey, bKey, ok := mcp.DuplicatePlanKeys(ev.Adjusters)
	if !ok {
		// Nothing truthful can be said about which plans these are.
		return st, nil
	}
	if st.a, err = loadDuplicateSide(ctx, q, teamUUID, aKey); err != nil {
		return st, err
	}
	if st.b, err = loadDuplicateSide(ctx, q, teamUUID, bKey); err != nil {
		return st, err
	}
	// b is the yield side: the plan whose session is suggested_yield_session_uuid.
	if yield != "" && st.a.sessionUUID == yield && st.b.sessionUUID != yield {
		st.a, st.b = st.b, st.a
	}
	st.plansOK = st.a.found && st.b.found

	if st.participants, err = loadEscalationSides(ctx, q, conflictID); err != nil {
		return st, err
	}
	for _, p := range st.participants {
		if p.notifiedAt.Valid && p.notifiedAt.Time.UTC().After(st.openSince) {
			st.openSince = p.notifiedAt.Time.UTC()
		}
	}
	return st, nil
}

// loadDuplicateSide reads one plan and its session, agent and member.
func loadDuplicateSide(ctx context.Context, q decisionQueryer, teamUUID uuid.UUID, key string) (duplicateSide, error) {
	d := duplicateSide{key: key}
	var status, sessionStatus int64
	err := q.QueryRowContext(ctx,
		"SELECT i.`id`, i.`summary`, i.`status`, i.`session_uuid`, COALESCE(sn.`key`, ''), COALESCE(sn.`status`, 0), "+
			"COALESCE(sn.`agent_uuid`, ''), COALESCE(sn.`member_uuid`, ''), COALESCE(a.`label`, ''), COALESCE(m.`display_name`, '') "+
			"FROM `intent` i "+
			"LEFT JOIN `session` sn ON sn.`id` = i.`session_uuid` "+
			"LEFT JOIN `agent` a ON a.`id` = sn.`agent_uuid` "+
			"LEFT JOIN `member` m ON m.`id` = sn.`member_uuid` "+
			"WHERE i.`team_uuid` = ? AND i.`key` = ?",
		teamUUID.String(), key).
		Scan(&d.subjectUUID, &d.summary, &status, &d.sessionUUID, &d.sessionKey, &sessionStatus,
			&d.agentUUID, &d.memberUUID, &d.agentLabel, &d.memberName)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return d, nil
	case err != nil:
		return d, err
	}
	d.found = true
	d.intentStatus = enums.IntentStatus(status)
	d.status = enums.SessionStatus(sessionStatus)
	d.sessionFound = d.sessionKey != ""
	return d, nil
}

// ─────────────────────────────────────────────
// (c) The incumbent's notice (§4.5)
// ─────────────────────────────────────────────

// noticeDue is §4.5 steps 1 to 3 in one place: still open (the loader only
// finds open or acknowledged rows), not yet told at this severity, at or above
// the notify floor, past the grace, and both plans still standing — the
// incumbent's on a working session.
func noticeDue(st duplicateConflictState, now time.Time) bool {
	if !st.found || !st.plansOK {
		return false
	}
	if st.severity < duplicateNoticeFloor {
		return false
	}
	if st.notifiedAt.Valid && st.maxNotified >= st.severity {
		return false
	}
	if now.Sub(st.detected) < coordination.DuplicateNoticeGrace(st.cadence.String()) {
		return false
	}
	return st.a.working() && st.b.found && planLive(st.b.intentStatus)
}

// judgeRationale is the verdict's rationale, read back from the evidence's
// field_issues[0] ("S-22's model (0.85): <rationale>").
func judgeRationale(fieldIssue string) string {
	if _, rest, ok := strings.Cut(fieldIssue, "): "); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

// incumbentNoticeBody is §4.5 step 6's body: "%s (%s) on %s: %s", so
// get_instructions' splitNoticeBody separates what happened from the action.
// The judge is the yield side's session: b is the plan whose agent judged.
func incumbentNoticeBody(st duplicateConflictState, pathConflictKey string) string {
	action := mcp.DuplicateIncumbentAction(st.b.name(), st.b.key, judgeRationale(st.fieldIssue), st.b.sessionKey, pathConflictKey)
	return fmt.Sprintf("%s (%s) on %s: %s", st.conflictKey, st.severity.String(), st.a.key, action)
}

// noticeDuplicateIncumbents is §4.5's pass, per team, after the pair pass.
func (s *Sweeper) noticeDuplicateIncumbents(ctx context.Context, t teamRow, now time.Time, rep *Report) error {
	rows, err := s.db.QueryContext(ctx,
		"SELECT c.`id` FROM `conflict` c "+
			"WHERE c.`team_uuid` = ? AND c.`status` IN (?, ?) AND c.`severity` >= ? AND c.`kind` = ? "+
			"AND (c.`notified_at` IS NULL OR c.`max_severity_notified` < c.`severity`) "+
			"ORDER BY c.`first_detected_at` LIMIT ?",
		t.uuid.String(), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED),
		int64(duplicateNoticeFloor), int64(enums.CONFLICT_KIND_DUPLICATE_WORK), duplicateNoticesPerPass)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		if id, err := uuid.FromString(raw); err == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, id := range ids {
		s.noticeDuplicateIncumbent(ctx, t, id, now, rep)
	}
	return nil
}

// noticeDuplicateIncumbent tells one incumbent, when §4.5 says it is due.
func (s *Sweeper) noticeDuplicateIncumbent(ctx context.Context, t teamRow, conflictID uuid.UUID, now time.Time, rep *Report) {
	st, err := loadDuplicateConflictState(ctx, s.db, t.uuid, conflictID)
	if err != nil {
		rep.addErr("reading duplicate conflict "+conflictID.String(), err)
		return
	}
	if !noticeDue(st, now) {
		// Inside the grace, or already settled in all but name. Checked
		// again under the lock; this read only spares the lock.
		return
	}

	project := uuid.FromStringOrNil(st.projectUUID)
	session := uuid.FromStringOrNil(st.a.sessionUUID)
	agent := uuid.FromStringOrNil(st.a.agentUUID)
	member := uuid.FromStringOrNil(st.a.memberUUID)

	var (
		body        string
		instruction uuid.UUID
		severity    enums.ConflictSeverity
		key         string
		told        string
		yieldKey    string
	)
	wrote, err := s.appendEvent(ctx, sweepEvent{
		teamUUID: t.uuid,
		// occurrence_count and the severity are in the key: a reopened
		// conflict earns a fresh notice after a fresh grace, and a severity
		// rise re-notifies once.
		idempotencyKey: fmt.Sprintf("sweep:duplicate_notice:%s:%d:%d", conflictID.String(), st.occurrence, int64(st.severity)),
		kind:           enums.EVENT_KIND_INSTRUCTION_RAISED,
		// Structural: the incumbent's lane gains a conflict badge.
		structural:  true,
		projectUUID: &project,
		sessionUUID: &session,
		agentUUID:   &agent,
		memberUUID:  &member,
		subjectKind: enums.SUBJECT_KIND_CONFLICT,
		subjectUUID: &conflictID,
		extra: func(ctx context.Context, tx *sql.Tx, seq int64, at time.Time) error {
			fresh, err := loadDuplicateConflictState(ctx, tx, t.uuid, conflictID)
			if err != nil {
				return err
			}
			if !noticeDue(fresh, at) {
				// The judge yielded, re-scoped or ended in the meantime, or
				// another pod told the incumbent already.
				return errNothingToSay
			}
			if err := attachIncumbent(ctx, tx, t.uuid, conflictID, fresh.a, at); err != nil {
				return err
			}
			folded, err := foldPathNotice(ctx, tx, fresh.a.sessionUUID, fresh.b.sessionUUID, fresh.conflictKey, at)
			if err != nil {
				return err
			}
			body = incumbentNoticeBody(fresh, folded)
			if instruction, err = insertDuplicateNotice(ctx, tx, t.uuid, conflictID, fresh.a, fmt.Sprintf("I-%d", seq), body, at); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx,
				"UPDATE `conflict` SET `notified_at` = ?, `max_severity_notified` = ?, `updated_at` = ? "+
					"WHERE `id` = ? AND `status` IN (?, ?) AND (`notified_at` IS NULL OR `max_severity_notified` < ?)",
				at, int64(fresh.severity), at, conflictID.String(),
				int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED), int64(fresh.severity))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errNothingToSay
			}
			severity, key, yieldKey = fresh.severity, fresh.conflictKey, fresh.b.key
			told = firstText(fresh.a.agentLabel, fresh.a.sessionKey, "the other agent")
			return nil
		},
		finish: func() (string, string, []byte) {
			payload := payload_entity.EventPayload{
				ConflictUUID:    &conflictID,
				Severity:        severity,
				InstructionUUID: &instruction,
				Message:         null.StringFrom(body),
			}
			return fmt.Sprintf("%s: %s told about %s", key, told, yieldKey), key, payload.ToJSON()
		},
	})
	switch {
	case err != nil:
		rep.addErr("telling the incumbent of duplicate conflict "+conflictID.String(), err)
	case wrote:
		rep.DuplicateNoticesSent++
		rep.InstructionsRaised++
		rep.EventsEmitted++
	}
}

// attachIncumbent makes subject a's session a participant (subject_kind
// intent, role incumbent), notified now: from here on it carries the
// conflict in pending.conflicts. app/mcp's upsertDecisionParticipant is
// unexported, so this is the sweeper's own, with the same check-then-write
// shape.
func attachIncumbent(ctx context.Context, tx *sql.Tx, teamUUID, conflictID uuid.UUID, a duplicateSide, now time.Time) error {
	var exists int
	err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? LIMIT 1",
		conflictID.String(), a.sessionUUID).Scan(&exists)
	switch {
	case err == nil:
		_, err := tx.ExecContext(ctx,
			"UPDATE `conflict_participant` SET `subject_uuid` = ?, `notified_at` = ?, `updated_at` = ? "+
				"WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
			a.subjectUUID, now, now, conflictID.String(), a.sessionUUID)
		return err
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	participant, err := uuid.NewV4()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
			"`subject_kind`,`subject_uuid`,`role`,`notified_at`,`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		participant.String(), conflictID.String(), teamUUID.String(), a.sessionUUID, a.agentUUID, a.memberUUID,
		int64(enums.SUBJECT_KIND_INTENT), a.subjectUUID, int64(enums.PARTICIPANT_ROLE_INCUMBENT), now, now, now)
	return err
}

// foldPathNotice is §4.5 step 5 and §4.6: a path-overlap notice to the
// incumbent that is still pending (never delivered), about an open path
// conflict the yield side's session takes part in, is dismissed as superseded
// by this conflict, so the incumbent is interrupted once and not twice. A
// notice already delivered is left alone: the duplicate notice then carries
// news the first did not. Returns the first folded path conflict's key, which
// the new body names.
func foldPathNotice(ctx context.Context, tx *sql.Tx, incumbentSession, yieldSession, conflictKey string, now time.Time) (string, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT i.`id`, c.`id`, c.`key` FROM `instruction` i JOIN `conflict` c ON c.`id` = i.`ref_uuid` "+
			"WHERE i.`target_session_uuid` = ? AND i.`status` = ? AND i.`kind` = ? AND i.`ref_kind` = ? "+
			"AND c.`kind` = ? AND c.`status` IN (?, ?) ORDER BY i.`created_at`, i.`key` LIMIT ?",
		incumbentSession, int64(enums.INSTRUCTION_STATUS_PENDING), int64(enums.INSTRUCTION_KIND_CONFLICT_NOTICE),
		int64(enums.SUBJECT_KIND_CONFLICT), int64(enums.CONFLICT_KIND_PATH_OVERLAP),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED), duplicatePathNoticesScanned)
	if err != nil {
		return "", err
	}
	type pathNotice struct{ instruction, conflict, key string }
	var notices []pathNotice
	for rows.Next() {
		var n pathNotice
		if err := rows.Scan(&n.instruction, &n.conflict, &n.key); err != nil {
			_ = rows.Close()
			return "", err
		}
		notices = append(notices, n)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	_ = rows.Close()

	folded := ""
	for _, n := range notices {
		var exists int
		err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? LIMIT 1",
			n.conflict, yieldSession).Scan(&exists)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue // a path conflict with somebody else
		case err != nil:
			return "", err
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE `instruction` SET `status` = ?, `action_note` = ?, `updated_at` = ? WHERE `id` = ? AND `status` = ?",
			int64(enums.INSTRUCTION_STATUS_DISMISSED), truncate("superseded by "+conflictKey, 400), now,
			n.instruction, int64(enums.INSTRUCTION_STATUS_PENDING))
		if err != nil {
			return "", err
		}
		if c, _ := res.RowsAffected(); c == 1 && folded == "" {
			folded = n.key
		}
	}
	return folded, nil
}

// insertDuplicateNotice queues the incumbent's conflict_notice (§4.5 step 6):
// a notice, not a question, so requires_report is 0.
func insertDuplicateNotice(ctx context.Context, tx *sql.Tx, teamUUID, conflictID uuid.UUID,
	side duplicateSide, key, body string, now time.Time) (uuid.UUID, error) {
	instructionUUID, err := uuid.NewV4()
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO `instruction` (`id`,`team_uuid`,`target_session_uuid`,`target_agent_uuid`,`key`,`source`,`kind`,`body`,"+
			"`ref_kind`,`ref_uuid`,`requires_report`,`status`,`expires_at`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		instructionUUID.String(), teamUUID.String(), side.sessionUUID, nullableString(side.agentUUID), key,
		int64(enums.INSTRUCTION_SOURCE_SERVER), int64(enums.INSTRUCTION_KIND_CONFLICT_NOTICE),
		truncate(body, 600),
		int64(enums.SUBJECT_KIND_CONFLICT), conflictID.String(),
		false, int64(enums.INSTRUCTION_STATUS_PENDING),
		now.Add(duplicateNoticeTTL), now, now)
	return instructionUUID, err
}

// ─────────────────────────────────────────────
// (d) Asking a person about a duplicate (§4.8)
// ─────────────────────────────────────────────

// duplicatePlansLive is §4.8's "intent live" for duplicates: both plans
// declared or active and both sessions live or stale.
func (st duplicateConflictState) duplicatePlansLive() bool {
	return st.plansOK && st.a.working() && st.b.working()
}

// shouldEscalateDuplicate feeds coordination.ShouldEscalate, the same pure
// rule decisions use: at or above the human floor, after a full budget since
// openSince, while both plans stand, and only when escalated_at is unset.
func shouldEscalateDuplicate(t teamRow, st duplicateConflictState, now time.Time) bool {
	return coordination.ShouldEscalate(
		severityOf(st.severity), severityOf(t.humanFloor),
		st.openSince, now, st.cadence.String(),
		st.duplicatePlansLive(), st.escalatedAt.Valid)
}

// escalateDuplicateConflict asks both people about one duplicate conflict the
// agents did not settle: the yield side always, the incumbent when it is
// attached (it was told) and still there. The "once" guard is the decisions
// one: claimEscalation's `escalated_at IS NULL`.
func (s *Sweeper) escalateDuplicateConflict(ctx context.Context, t teamRow, conflictID uuid.UUID, now time.Time, rep *Report) {
	st, err := loadDuplicateConflictState(ctx, s.db, t.uuid, conflictID)
	if err != nil {
		rep.addErr("reading duplicate conflict "+conflictID.String(), err)
		return
	}
	if !st.found || !shouldEscalateDuplicate(t, st, now) {
		return
	}

	budget := coordination.EscalationBudget(st.cadence.String())
	yieldBody, incumbentBody := duplicateQuestions(st, budget)

	project := uuid.FromStringOrNil(st.projectUUID)
	yieldSession := uuid.FromStringOrNil(st.b.sessionUUID)
	yieldAgent := uuid.FromStringOrNil(st.b.agentUUID)
	yieldMember := uuid.FromStringOrNil(st.b.memberUUID)

	payload := payload_entity.EventPayload{
		ConflictUUID: &conflictID,
		Severity:     st.severity,
		Message:      null.StringFrom(yieldBody),
	}

	raised := 0
	wrote, err := s.appendEvent(ctx, sweepEvent{
		teamUUID:       t.uuid,
		idempotencyKey: fmt.Sprintf("sweep:conflict_escalated:%s:%d", conflictID.String(), st.occurrence),
		kind:           enums.EVENT_KIND_CONFLICT_ESCALATED,
		structural:     true,
		projectUUID:    &project,
		sessionUUID:    &yieldSession,
		agentUUID:      &yieldAgent,
		memberUUID:     &yieldMember,
		subjectKind:    enums.SUBJECT_KIND_CONFLICT,
		subjectUUID:    &conflictID,
		subjectKey:     st.conflictKey,
		summary:        fmt.Sprintf("%s: a person was asked about %s / %s", st.conflictKey, st.b.key, st.a.key),
		payload:        payload.ToJSON(),
		extra: func(ctx context.Context, tx *sql.Tx, seq int64, at time.Time) error {
			fresh, err := loadDuplicateConflictState(ctx, tx, t.uuid, conflictID)
			if err != nil {
				return err
			}
			if !fresh.found || !shouldEscalateDuplicate(t, fresh, at) {
				return errNothingToSay
			}
			claimed, err := claimEscalation(ctx, tx, conflictID, at)
			if err != nil {
				return err
			}
			if !claimed {
				return errNothingToSay
			}
			if err := insertQuestion(ctx, tx, t.uuid, conflictID, fresh.b.escalationSide,
				escalationInstructionKey(seq, 0), yieldBody, at); err != nil {
				return err
			}
			raised = 1
			if fresh.attached(fresh.a.sessionUUID) && fresh.a.live() && fresh.a.sessionUUID != fresh.b.sessionUUID {
				if err := insertQuestion(ctx, tx, t.uuid, conflictID, fresh.a.escalationSide,
					escalationInstructionKey(seq, 1), incumbentBody, at); err != nil {
					return err
				}
				raised = 2
			}
			return nil
		},
	})
	switch {
	case err != nil:
		rep.addErr("escalating duplicate conflict "+conflictID.String(), err)
	case wrote:
		rep.ConflictsEscalated++
		rep.InstructionsRaised += raised
		rep.EventsEmitted++
	}
}
