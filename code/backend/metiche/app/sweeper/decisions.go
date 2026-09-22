package sweeper

// decisions.go is the sweeper's half of the decisions feature: the judge
// window, the review backlog and re-arming (docs/DECISIONS.md §4.4), and the
// one moment in this whole feature that is allowed to interrupt a person
// (§4.7). The decision conflicts an abandoned session settles live next to
// their contract siblings in conflicts.go.
//
// Three rules shape everything here.
//
// # Quiet
//
// A pair nobody answered expires with no event and no notice. "Unjudged" is
// not "contradicts" — the server never decides that (§4.1) — so an agent that
// went away leaves a question unanswered, not a verdict. Nothing on the board
// changes, nobody is told, and the only trace is the row's own status.
//
// # Once
//
// A conflict is escalated at most once, and the guard is the database's
// (`escalated_at IS NULL` on the UPDATE), not a flag this process remembers.
// Two pods sweeping the same team in the same instant ask the person once,
// and the second one's event is collapsed by the idempotency key as well. The
// key carries occurrence_count, so a conflict that metiche closed and that
// later reopened — which clears escalated_at (§4.6) — can earn one more.
//
// # The plan's own session, always
//
// A pair is only ever assigned to the session whose plan it is. Re-arming
// therefore never names a judge: it extends the window of the pair that is
// already the plan owner's. Handing somebody else's plan to a third agent to
// judge is exactly the thing §4.1 refuses to do.
//
// # Locks
//
// The backlog pass takes no team lock at all. Every write there is one
// conditional UPDATE carrying the state it read, so a race costs at most one
// pair over the per-minute cap, which §4.4 accepts. Escalation does take the
// lock, because the conflict's flip, the two instructions and the event have
// to be one atomic step — and it takes it through appendEvent, for the length
// of those statements and nothing more.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/uuid"
	"github.com/guregu/null/v6"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/app/mcp"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	team_settings_entity "github.com/mklfarha/metiche/backend/entity/team_settings"
	"github.com/mklfarha/metiche/backend/enums"
)

// The numbers §4.4 and §4.7 fix. app/mcp holds the first four unexported, for
// the tool side of the same feature; they are restated here rather than
// exported from there, because widening a frozen package's surface (§9.1) to
// share four constants the spec already states is the larger change. The
// per-team overrides come from team_settings, read by applyDecisionSettings
// below, exactly as app/mcp's loadDecisionSettings reads them.
const (
	// decisionJudgeWindow is how long a pair waits for its judge before the
	// window lapses. team_settings.judge_window_seconds overrides it.
	decisionJudgeWindow = 15 * time.Minute

	// judgeMaxAssignments is how many times one pair is armed before the
	// sweeper gives up on it (§4.4). The third silence is the last.
	judgeMaxAssignments = 3

	// judgeRateWindow and defaultMaxReviewsPerMinute are the per-minute cap on
	// how much judging one session is asked for.
	// team_settings.max_reviews_per_minute overrides the count.
	judgeRateWindow            = 60 * time.Second
	defaultMaxReviewsPerMinute = 3

	// defaultHumanNotifyFloor is §4.7's default for
	// team_settings.human_notify_floor: below high, a conflict is on the board
	// and interrupts nobody, forever.
	defaultHumanNotifyFloor = enums.CONFLICT_SEVERITY_HIGH

	// escalationInstructionTTL is how long the question waits for an answer
	// before it stops being worth asking.
	escalationInstructionTTL = 4 * time.Hour

	// escalationQuestionChars is what get_instructions actually shows (F7).
	// A question that does not fit is a question the agent never finishes
	// reading.
	escalationQuestionChars = 200

	// escalationSummaryChars is §4.8's clip on the quoted plan summary: the
	// part of the question that gives way when there is no room.
	escalationSummaryChars = 40

	// escalationNameChars and escalationKeyChars bound the other substitutions,
	// so a team that records an 80-character decision key cannot push
	// "Then report_back their answer." off the end of the body.
	escalationNameChars = 24
	escalationKeyChars  = 60

	// escalationMaxParticipants bounds the participants read per conflict. A
	// decision conflict has two sides; eight is a different problem.
	escalationMaxParticipants = 8
)

// decisionQueryer is the little of *sql.DB and *sql.Tx this file needs, so the
// same loader serves the read outside the lock and the re-read inside it.
type decisionQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// applyDecisionSettings reads the decisions half of team_settings through the
// generated entity, so a field renamed in nuzur breaks the build here instead
// of silently reverting a team to the defaults.
func applyDecisionSettings(t *teamRow, raw []byte) {
	if len(raw) == 0 {
		return
	}
	e := team_settings_entity.TeamSettingsFromJSON(raw)
	if e.JudgeWindowSeconds.Valid && e.JudgeWindowSeconds.Int64 > 0 {
		t.judgeWindow = time.Duration(e.JudgeWindowSeconds.Int64) * time.Second
	}
	if e.MaxReviewsPerMinute.Valid && e.MaxReviewsPerMinute.Int64 > 0 {
		t.maxReviewsPerMinute = int(e.MaxReviewsPerMinute.Int64)
	}
	if e.HumanNotifyFloor != enums.CONFLICT_SEVERITY_INVALID {
		t.humanFloor = e.HumanNotifyFloor
	}
}

// severityOf converts the stored enum to coordination's pure one.
//
// The two ladders are numbered identically — invalid/none 0, low 1, medium 2,
// high 3, critical 4 — and this function is the one place that says so, rather
// than a bare cast at each call site that would quietly go wrong if either
// ladder ever gained a rung.
func severityOf(s enums.ConflictSeverity) coordination.Severity {
	switch s {
	case enums.CONFLICT_SEVERITY_LOW:
		return coordination.SeverityLow
	case enums.CONFLICT_SEVERITY_MEDIUM:
		return coordination.SeverityMedium
	case enums.CONFLICT_SEVERITY_HIGH:
		return coordination.SeverityHigh
	case enums.CONFLICT_SEVERITY_CRITICAL:
		return coordination.SeverityCritical
	}
	return coordination.SeverityNone
}

// ─────────────────────────────────────────────
// (a) The backlog and the judge window (§4.4)
// ─────────────────────────────────────────────

// pendingPair is one row of the open-pairs scan: the judgement, plus enough of
// its two subjects to say whether the question is still worth asking.
type pendingPair struct {
	id               string
	decisionRevision int64
	intentRevision   int64
	// judgeSession is empty for a pair in the backlog — one the reviewer minted
	// past the rate cap and left for this pass to hand out.
	judgeSession    string
	expiresAt       sql.NullTime
	assignmentCount int64

	decisionStatus   enums.DecisionStatus
	decisionCurrent  int64
	intentStatus     enums.IntentStatus
	intentCurrent    int64
	intentSession    string
	intentSessionAge enums.SessionStatus
}

// subjectsValid is §4.4's "subjects still valid": the decision accepted at the
// revision the pair was minted against, the plan declared or active at its
// own, and the plan's session still working.
//
// The revision equality is the part that matters. A pair against wording that
// has since been revised is a question about a sentence nobody stands behind
// any more; re-arming it would spend an agent's attention on history.
func (p pendingPair) subjectsValid() bool {
	if p.decisionStatus != enums.DECISION_STATUS_ACCEPTED || p.decisionCurrent != p.decisionRevision {
		return false
	}
	switch p.intentStatus {
	case enums.INTENT_STATUS_DECLARED, enums.INTENT_STATUS_ACTIVE:
	default:
		return false
	}
	if p.intentCurrent != p.intentRevision || p.intentSession == "" {
		return false
	}
	switch p.intentSessionAge {
	case enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE:
		return true
	}
	return false
}

// sweepJudgements is §4.4's pass: hand out the backlog, re-arm what lapsed,
// expire what is past asking. It writes no events, by design.
func (s *Sweeper) sweepJudgements(ctx context.Context, t teamRow, now time.Time, rep *Report) error {
	pairs, err := s.loadOpenPairs(ctx, t, now)
	if err != nil {
		return err
	}

	// The per-minute cap is counted once per session and then carried in this
	// map, so ten backlogged pairs for one session cost one COUNT rather than
	// ten, and the cap is honoured across the whole pass rather than per row.
	recent := map[string]int{}

	for _, p := range pairs {
		valid := p.subjectsValid()

		if p.judgeSession != "" {
			// Assigned, and its window lapsed.
			if valid && p.assignmentCount < judgeMaxAssignments {
				if err := s.rearmPair(ctx, p, t.judgeWindow, now, rep); err != nil {
					return err
				}
				continue
			}
			if err := s.expirePair(ctx, p, now, rep); err != nil {
				return err
			}
			continue
		}

		// Unassigned: the reviewer minted it past the rate cap.
		if !valid {
			if err := s.expirePair(ctx, p, now, rep); err != nil {
				return err
			}
			continue
		}
		n, seen := recent[p.intentSession]
		if !seen {
			n, err = s.reviewsAssignedRecently(ctx, p.intentSession, now, t.maxReviewsPerMinute)
			if err != nil {
				return err
			}
		}
		if n >= t.maxReviewsPerMinute {
			// Still over the cap. Left exactly as it is, for the next pass:
			// the backlog is the one thing here that is neither expired nor
			// assigned, and a pass that dropped it would lose the question.
			recent[p.intentSession] = n
			continue
		}
		assigned, err := s.assignPair(ctx, p, t.judgeWindow, now, rep)
		if err != nil {
			return err
		}
		if assigned {
			n++
		}
		recent[p.intentSession] = n
	}
	return nil
}

// loadOpenPairs is §4.4's scan, off idx_judgement_team_open (team_uuid,
// status, judging_expires_at): pending pairs that are unassigned (NULL expiry,
// which sorts first) or whose window has lapsed.
//
// The three LEFT JOINs answer "are the subjects still valid" for the whole
// batch in the one query rather than three point lookups per row, and a
// subject whose row is gone comes back as a zero status, which is not valid —
// the right answer for a plan whose session was deleted by retention.
func (s *Sweeper) loadOpenPairs(ctx context.Context, t teamRow, now time.Time) ([]pendingPair, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT j.`id`, j.`subject_a_revision`, j.`subject_b_revision`, COALESCE(j.`judge_session_uuid`, ''), "+
			"j.`judging_expires_at`, j.`assignment_count`, "+
			"COALESCE(d.`status`, 0), COALESCE(d.`revision`, 0), "+
			"COALESCE(i.`status`, 0), COALESCE(i.`revision`, 0), COALESCE(i.`session_uuid`, ''), "+
			"COALESCE(sn.`status`, 0) "+
			"FROM `judgement` j "+
			"LEFT JOIN `decision` d ON d.`id` = j.`subject_a_uuid` "+
			"LEFT JOIN `intent` i ON i.`id` = j.`subject_b_uuid` "+
			"LEFT JOIN `session` sn ON sn.`id` = i.`session_uuid` "+
			"WHERE j.`team_uuid` = ? AND j.`status` = ? AND j.`kind` = ? "+
			"AND (j.`judging_expires_at` IS NULL OR j.`judging_expires_at` < ?) "+
			"ORDER BY j.`judging_expires_at` LIMIT ?",
		t.uuid.String(), int64(enums.JUDGEMENT_STATUS_PENDING),
		// The kind filter is not in §4.4's predicate, and today it changes
		// nothing: every judgement row is a decision pair. It is here so that a
		// later pair kind with its own window cannot be silently re-armed by
		// this rule.
		int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION),
		now, s.opts.MaxJudgementsPerPass)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []pendingPair
	for rows.Next() {
		var (
			p                        pendingPair
			dStatus, iStatus, sState int64
		)
		if err := rows.Scan(&p.id, &p.decisionRevision, &p.intentRevision, &p.judgeSession,
			&p.expiresAt, &p.assignmentCount,
			&dStatus, &p.decisionCurrent, &iStatus, &p.intentCurrent, &p.intentSession, &sState); err != nil {
			return nil, err
		}
		p.decisionStatus = enums.DecisionStatus(dStatus)
		p.intentStatus = enums.IntentStatus(iStatus)
		p.intentSessionAge = enums.SessionStatus(sState)
		out = append(out, p)
	}
	return out, rows.Err()
}

// assignPair hands a backlogged pair to the plan's OWN session — never to
// anybody else, because judging somebody else's plan is not a thing this
// system asks of an agent (§4.1).
//
// The `judge_session_uuid IS NULL` guard is what makes two pods racing this
// row assign it once.
func (s *Sweeper) assignPair(ctx context.Context, p pendingPair, window time.Duration, now time.Time, rep *Report) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE `judgement` SET `judge_session_uuid` = ?, `judging_expires_at` = ?, `assignment_count` = 1, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` = ? AND `judge_session_uuid` IS NULL AND `assignment_count` = ?",
		p.intentSession, now.Add(window), now,
		p.id, int64(enums.JUDGEMENT_STATUS_PENDING), p.assignmentCount)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		rep.JudgementsAssigned++
		return true, nil
	}
	return false, nil
}

// rearmPair gives the pair's window another turn. It deliberately does not
// name a judge: the row already carries the plan's own session, and an UPDATE
// that set it would be the one way this sweeper could hand a plan to the
// wrong agent.
//
// `assignment_count = ?` is the conditional part: two pods that both read 1
// produce 2, not 3.
func (s *Sweeper) rearmPair(ctx context.Context, p pendingPair, window time.Duration, now time.Time, rep *Report) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE `judgement` SET `judging_expires_at` = ?, `assignment_count` = `assignment_count` + 1, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` = ? AND `assignment_count` = ? AND `judge_session_uuid` = ?",
		now.Add(window), now,
		p.id, int64(enums.JUDGEMENT_STATUS_PENDING), p.assignmentCount, p.judgeSession)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		rep.JudgementsReArmed++
	}
	return nil
}

// expirePair gives up on a pair.
//
// No event, no instruction, no conflict: §4.4's "quiet". An unanswered
// question is not a contradiction, and recording one here would put a verdict
// on the board that no model ever gave.
func (s *Sweeper) expirePair(ctx context.Context, p pendingPair, now time.Time, rep *Report) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE `judgement` SET `status` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` = ? AND `assignment_count` = ?",
		int64(enums.JUDGEMENT_STATUS_EXPIRED), now,
		p.id, int64(enums.JUDGEMENT_STATUS_PENDING), p.assignmentCount)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		rep.JudgementsExpired++
	}
	return nil
}

// reviewsAssignedRecently counts a session's pairs armed in the last
// judgeRateWindow, up to limit. idx_judgement_assignment, capped — the count
// only ever has to answer "is it at the cap", so it never scans past it.
func (s *Sweeper) reviewsAssignedRecently(ctx context.Context, sessionUUID string, now time.Time, limit int) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM (SELECT 1 FROM `judgement` WHERE `judge_session_uuid` = ? AND `status` = ? "+
			"AND `updated_at` >= ? LIMIT ?) recent",
		sessionUUID, int64(enums.JUDGEMENT_STATUS_PENDING), now.Add(-judgeRateWindow), limit).Scan(&n)
	return n, err
}

// ─────────────────────────────────────────────
// (b) Asking a person (§4.7)
// ─────────────────────────────────────────────

// escalationSide is one participant of a decision conflict, with the names the
// question is worded from.
type escalationSide struct {
	sessionUUID string
	agentUUID   string
	memberUUID  string
	sessionKey  string
	agentLabel  string
	memberName  string
	subjectUUID string
	status      enums.SessionStatus
	notifiedAt  sql.NullTime
}

// live reports an agent still there to be asked.
func (e escalationSide) live() bool {
	switch e.status {
	case enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE:
		return true
	}
	return false
}

// name is §4.8's session name: "S-22 (ui)".
func (e escalationSide) name() string {
	key := e.sessionKey
	if key == "" {
		return ""
	}
	if e.agentLabel != "" {
		return fmt.Sprintf("%s (%s)", key, e.agentLabel)
	}
	return key
}

// escalationState is one conflict as §4.7 reads it.
type escalationState struct {
	found       bool
	conflictKey string
	projectUUID string
	severity    enums.ConflictSeverity
	escalatedAt sql.NullTime
	decisionKey string
	occurrence  int64
	cadence     enums.ProjectCadence

	hasOwner   bool
	owner      escalationSide
	hasDecider bool
	decider    escalationSide

	intentKey     string
	intentSummary string
	intentStatus  enums.IntentStatus

	// openSince is GREATEST(COALESCE(last_detected_at, first_detected_at),
	// MAX(participant.notified_at)): a fresh verdict or a fresh notice restarts
	// the clock, so both agents always get a full budget after their last turn.
	openSince time.Time
}

// intentLive is §4.7's "the plan's intent is still live": declared or active,
// on a session that is still working.
func (st escalationState) intentLive() bool {
	switch st.intentStatus {
	case enums.INTENT_STATUS_DECLARED, enums.INTENT_STATUS_ACTIVE:
	default:
		return false
	}
	return st.hasOwner && st.owner.live()
}

// escalateDecisionConflicts is §4.7, run per team per pass after the backlog
// step.
//
// The candidates are read outside the lock and the decision is made twice: once
// here, to avoid taking the team lock for a conflict that is nowhere near its
// budget, and again inside appendEvent's transaction, which is the one that
// counts. The wording is built from the outer read — the keys and names in it
// do not change between the two — while every write is guarded by the state
// the inner read found.
func (s *Sweeper) escalateDecisionConflicts(ctx context.Context, t teamRow, now time.Time, rep *Report) error {
	candidates, err := s.loadEscalationCandidates(ctx, t)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		conflictID := c.id
		if c.kind == enums.CONFLICT_KIND_DUPLICATE_WORK {
			// Same query, same rule, same "once" guard, its own wording and
			// its own two sides (docs/DUPLICATES.md §4.8): duplicates.go.
			s.escalateDuplicateConflict(ctx, t, conflictID, now, rep)
			continue
		}
		st, err := loadEscalationState(ctx, s.db, t.uuid, conflictID)
		if err != nil {
			return err
		}
		if !st.found || !shouldEscalate(t, st, now) {
			continue
		}

		budget := coordination.EscalationBudget(st.cadence.String())
		ownerBody := planOwnerQuestion(st, budget)
		deciderBody := deciderQuestion(st, budget)

		project := uuid.FromStringOrNil(st.projectUUID)
		ownerSession := uuid.FromStringOrNil(st.owner.sessionUUID)
		ownerAgent := uuid.FromStringOrNil(st.owner.agentUUID)
		ownerMember := uuid.FromStringOrNil(st.owner.memberUUID)

		payload := payload_entity.EventPayload{
			ConflictUUID: &conflictID,
			Severity:     st.severity,
			// The plan owner's question, so the timeline says what was asked
			// and not merely that something was.
			Message: null.StringFrom(ownerBody),
		}

		raised := 0
		wrote, err := s.appendEvent(ctx, sweepEvent{
			teamUUID: t.uuid,
			// occurrence_count is in the key on purpose: a conflict metiche
			// closed and that later reopened clears escalated_at (§4.6), and
			// this is what lets the second escalation write its own event.
			idempotencyKey: fmt.Sprintf("sweep:conflict_escalated:%s:%d", conflictID.String(), st.occurrence),
			kind:           enums.EVENT_KIND_CONFLICT_ESCALATED,
			// Structural: "a person was asked" changes the conflict card and
			// the badges, so the board has to repaint rather than merely
			// append a line.
			structural:  true,
			projectUUID: &project,
			sessionUUID: &ownerSession,
			agentUUID:   &ownerAgent,
			memberUUID:  &ownerMember,
			subjectKind: enums.SUBJECT_KIND_CONFLICT,
			subjectUUID: &conflictID,
			subjectKey:  st.conflictKey,
			summary:     fmt.Sprintf("%s: a person was asked about %s", st.conflictKey, st.decisionKey),
			payload:     payload.ToJSON(),
			extra: func(ctx context.Context, tx *sql.Tx, seq int64, at time.Time) error {
				// Re-read under the lock. Between the scan and here an agent
				// may have judged it no_conflict, ended its plan, or answered
				// the notice — any of which means the agents settled it after
				// all, and §4.7 is only for the case where they did not.
				fresh, err := loadEscalationState(ctx, tx, t.uuid, conflictID)
				if err != nil {
					return err
				}
				if !fresh.found || !shouldEscalate(t, fresh, at) {
					return errNothingToSay
				}
				claimed, err := claimEscalation(ctx, tx, conflictID, at)
				if err != nil {
					return err
				}
				if !claimed {
					// Somebody else asked in the microsecond we were deciding.
					return errNothingToSay
				}

				// The plan's owner always: it is the side that must act.
				if err := insertQuestion(ctx, tx, t.uuid, conflictID, fresh.owner,
					escalationInstructionKey(seq, 0), ownerBody, at); err != nil {
					return err
				}
				raised = 1
				// The decider's agent only when there is one still there, and
				// only when it is not the same session.
				if fresh.hasDecider && fresh.decider.live() && fresh.decider.sessionUUID != fresh.owner.sessionUUID {
					if err := insertQuestion(ctx, tx, t.uuid, conflictID, fresh.decider,
						escalationInstructionKey(seq, 1), deciderBody, at); err != nil {
						return err
					}
					raised = 2
				}
				return nil
			},
		})
		switch {
		case err != nil:
			rep.addErr("escalating decision conflict "+conflictID.String(), err)
		case wrote:
			rep.ConflictsEscalated++
			rep.InstructionsRaised += raised
			rep.EventsEmitted++
		}
	}
	return nil
}

// shouldEscalate is the §4.7 rule, and the rule itself is
// coordination.ShouldEscalate — pure, tested without a database, and the same
// function the spec names. Everything here is only the plumbing that feeds it.
func shouldEscalate(t teamRow, st escalationState, now time.Time) bool {
	return coordination.ShouldEscalate(
		severityOf(st.severity), severityOf(t.humanFloor),
		st.openSince, now, st.cadence.String(),
		st.intentLive(), st.escalatedAt.Valid)
}

// escalationCandidate is one row of the escalation scan: the conflict, and its
// kind, which says whose wording and whose sides it is asked with.
type escalationCandidate struct {
	id   uuid.UUID
	kind enums.ConflictKind
}

// loadEscalationCandidates is §4.7's query, off idx_conflict_open (team_uuid,
// status, severity), for both kinds a person can be asked about: decision
// contradictions and duplicate work (docs/DUPLICATES.md §4.8). Oldest first,
// so a backlog is worked through in the order the conflicts appeared rather
// than at random.
func (s *Sweeper) loadEscalationCandidates(ctx context.Context, t teamRow) ([]escalationCandidate, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT `id`, `kind` FROM `conflict` WHERE `team_uuid` = ? AND `status` IN (?, ?) AND `severity` >= ? "+
			"AND `kind` IN (?, ?) AND `escalated_at` IS NULL ORDER BY `first_detected_at` LIMIT ?",
		t.uuid.String(), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED),
		int64(t.humanFloor), int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION), int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		s.opts.MaxEscalationsPerPass)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []escalationCandidate
	for rows.Next() {
		var (
			raw  string
			kind int64
		)
		if err := rows.Scan(&raw, &kind); err != nil {
			return nil, err
		}
		id, err := uuid.FromString(raw)
		if err != nil {
			continue
		}
		out = append(out, escalationCandidate{id: id, kind: enums.ConflictKind(kind)})
	}
	return out, rows.Err()
}

// loadEscalationState reads one conflict, its participants and its plan.
//
// The decision is found through evidence.overlap_path, which is where a
// decision conflict keeps its key (§2.1) — the decider's participant row may
// not exist at all, so the evidence is the authoritative link, and this reads
// it the same way app/mcp's settlement does.
func loadEscalationState(ctx context.Context, q decisionQueryer, teamUUID, conflictID uuid.UUID) (escalationState, error) {
	var (
		st       escalationState
		cadence  int64
		detected sql.NullTime
	)
	err := q.QueryRowContext(ctx,
		"SELECT c.`key`, c.`project_uuid`, c.`severity`, c.`escalated_at`, c.`occurrence_count`, "+
			"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(c.`evidence`, '$.overlap_path')), ''), "+
			"COALESCE(c.`last_detected_at`, c.`first_detected_at`), COALESCE(p.`cadence`, 0) "+
			"FROM `conflict` c LEFT JOIN `project` p ON p.`id` = c.`project_uuid` "+
			"WHERE c.`id` = ? AND c.`team_uuid` = ? AND c.`kind` = ? AND c.`status` IN (?, ?)",
		conflictID.String(), teamUUID.String(), int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED)).
		Scan(&st.conflictKey, &st.projectUUID, &st.severity, &st.escalatedAt, &st.occurrence,
			&st.decisionKey, &detected, &cadence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Closed, dismissed, or never a decision conflict. Nothing to ask.
		return st, nil
	case err != nil:
		return st, err
	}
	st.found = true
	st.cadence = enums.ProjectCadence(cadence)
	if st.decisionKey == "null" {
		st.decisionKey = ""
	}
	if detected.Valid {
		st.openSince = detected.Time.UTC()
	}

	sides, err := loadEscalationSides(ctx, q, conflictID)
	if err != nil {
		return st, err
	}
	for _, side := range sides {
		// A notice restarts the budget: the agent that was just told deserves
		// a full turn before a person is pulled in.
		if side.notifiedAt.Valid && side.notifiedAt.Time.UTC().After(st.openSince) {
			st.openSince = side.notifiedAt.Time.UTC()
		}
	}
	for _, side := range sides {
		switch {
		case side.kind == enums.SUBJECT_KIND_INTENT && !st.hasOwner:
			st.owner, st.hasOwner = side.escalationSide, true
		case side.kind == enums.SUBJECT_KIND_DECISION && !st.hasDecider:
			st.decider, st.hasDecider = side.escalationSide, true
		}
	}

	if st.hasOwner && st.owner.subjectUUID != "" {
		var iStatus int64
		err := q.QueryRowContext(ctx,
			"SELECT `key`, `summary`, `status` FROM `intent` WHERE `id` = ?", st.owner.subjectUUID).
			Scan(&st.intentKey, &st.intentSummary, &iStatus)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The plan is gone; settlement will close the conflict. Not live,
			// so nothing is escalated.
		case err != nil:
			return st, err
		default:
			st.intentStatus = enums.IntentStatus(iStatus)
		}
	}
	return st, nil
}

// participantSide is one row of the participants read, with the subject kind
// that says which side of the conflict it is.
type participantSide struct {
	escalationSide
	kind enums.SubjectKind
}

func loadEscalationSides(ctx context.Context, q decisionQueryer, conflictID uuid.UUID) ([]participantSide, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT p.`subject_kind`, p.`session_uuid`, p.`agent_uuid`, p.`member_uuid`, p.`subject_uuid`, p.`notified_at`, "+
			"COALESCE(sn.`key`, ''), COALESCE(sn.`status`, 0), COALESCE(a.`label`, ''), COALESCE(m.`display_name`, '') "+
			"FROM `conflict_participant` p "+
			"LEFT JOIN `session` sn ON sn.`id` = p.`session_uuid` "+
			"LEFT JOIN `agent` a ON a.`id` = p.`agent_uuid` "+
			"LEFT JOIN `member` m ON m.`id` = p.`member_uuid` "+
			"WHERE p.`conflict_uuid` = ? ORDER BY p.`created_at`, p.`session_uuid` LIMIT ?",
		conflictID.String(), escalationMaxParticipants)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []participantSide
	for rows.Next() {
		var (
			side         participantSide
			kind, status int64
		)
		if err := rows.Scan(&kind, &side.sessionUUID, &side.agentUUID, &side.memberUUID, &side.subjectUUID,
			&side.notifiedAt, &side.sessionKey, &status, &side.agentLabel, &side.memberName); err != nil {
			return nil, err
		}
		side.kind = enums.SubjectKind(kind)
		side.status = enums.SessionStatus(status)
		out = append(out, side)
	}
	return out, rows.Err()
}

// claimEscalation is the moment one conflict becomes "asked about", and the
// `escalated_at IS NULL` in it is the whole of §4.7's "at most once".
//
// It is a conditional UPDATE rather than a check followed by a write because
// the check and the write have to be the same statement. Two pods that both
// read NULL a microsecond apart would otherwise both go on to ask, and the
// person would be interrupted twice about one disagreement — the exact thing
// this feature is supposed to be careful about. Whoever's UPDATE touches the
// row earns the right to ask; everyone else is told false and writes nothing.
func claimEscalation(ctx context.Context, tx *sql.Tx, conflictID uuid.UUID, at time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx,
		"UPDATE `conflict` SET `escalated_at` = ?, `updated_at` = ? WHERE `id` = ? AND `escalated_at` IS NULL",
		at, at, conflictID.String())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// insertQuestion queues one §4.7 question.
//
// requires_report is 1: the whole point is that the agent comes back with what
// its person said. Without it the question is a notice, and a notice is what
// the agents already had and did not settle it with.
func insertQuestion(ctx context.Context, tx *sql.Tx, teamUUID, conflictID uuid.UUID,
	side escalationSide, key, body string, now time.Time) error {
	instructionUUID, err := uuid.NewV4()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO `instruction` (`id`,`team_uuid`,`target_session_uuid`,`target_agent_uuid`,`key`,`source`,`kind`,`body`,"+
			"`ref_kind`,`ref_uuid`,`requires_report`,`status`,`expires_at`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		instructionUUID.String(), teamUUID.String(), side.sessionUUID, nullableString(side.agentUUID), key,
		int64(enums.INSTRUCTION_SOURCE_SERVER), int64(enums.INSTRUCTION_KIND_QUESTION),
		truncate(body, 600),
		int64(enums.SUBJECT_KIND_CONFLICT), conflictID.String(),
		true, int64(enums.INSTRUCTION_STATUS_PENDING),
		now.Add(escalationInstructionTTL), now, now)
	return err
}

// escalationInstructionKey mints the instruction's short key from the event's
// own sequence, the way the unclaimed detector does. Both questions of one
// escalation share a sequence, so the second carries a suffix.
func escalationInstructionKey(seq int64, n int) string {
	if n == 0 {
		return fmt.Sprintf("I-%d", seq)
	}
	return fmt.Sprintf("I-%d.%d", seq, n+1)
}

// ─────────────────────────────────────────────
// (c) The wording (§4.8)
// ─────────────────────────────────────────────

// planOwnerQuestion is §4.8's escalation question for the plan's owner.
//
// The quoted plan summary is what gives way, never the sentence: a question cut
// off before "Then report_back their answer." tells an agent that something is
// wrong and not what to do about it, which is the one thing a question must
// never do. So the summary gets the room that is left, at most §4.8's 40.
func planOwnerQuestion(st escalationState, budget time.Duration) string {
	const shape = "%s on %s, unsettled %s. Ask your person: follow the decision, or agree with %s to change it? Plan: \"%s\". Then report_back their answer."
	decider := firstText(st.decider.memberName, st.decider.name(), "the decider")
	return fitQuestion(shape, clipText(st.conflictKey, escalationKeyChars), clipText(st.decisionKey, escalationKeyChars),
		renderBudget(budget), clipText(decider, escalationNameChars), st.intentSummary)
}

// deciderQuestion is the same question from the other side: the decider's
// person is the one who can say the decision may change.
func deciderQuestion(st escalationState, budget time.Duration) string {
	const shape = "%s on %s, unsettled %s. Ask your person: keep the decision, or change it for %s's plan \"%s\"? Then report_back their answer."
	owner := firstText(st.owner.memberName, st.owner.name(), "the plan owner")
	return fitQuestion(shape, clipText(st.conflictKey, escalationKeyChars), clipText(st.decisionKey, escalationKeyChars),
		renderBudget(budget), clipText(owner, escalationNameChars), st.intentSummary)
}

// duplicateQuestions are docs/DUPLICATES.md §4.10's two escalation questions
// for a duplicate_work conflict: the yield side's (b) and the incumbent's (a).
// The wording is single-sourced in app/mcp, which clips each body to the 200
// characters get_instructions shows; the sweeper only supplies the names.
func duplicateQuestions(st duplicateConflictState, budget time.Duration) (yieldSide, incumbent string) {
	return mcp.DuplicateEscalationQuestions(st.conflictKey, st.b.key, st.a.key,
		firstText(st.b.memberName, st.b.name()), firstText(st.a.memberName, st.a.name()),
		st.a.summary, renderBudget(budget))
}

// fitQuestion renders shape inside escalationQuestionChars, and it is the
// sentence that is protected rather than the substitutions.
//
// §4.8's own arithmetic assumes the keys are the short ones a team actually
// writes ("CF-23", "#auth-jwt-cookie"). The columns allow much longer, and a
// body built from the long end and then simply truncated would lose "Then
// report_back their answer." — the only part that tells the agent what to do.
//
// So the variable parts give way in order of how much the reader loses: the
// quoted plan summary first, then the person's name (still recognisable
// shortened), then the decision key, then the conflict key. The first
// combination that leaves the whole sentence standing wins.
func fitQuestion(shape, conflict, decision, budget, who, summary string) string {
	for _, fit := range []struct{ name, key int }{
		{escalationNameChars, escalationKeyChars},
		{escalationNameChars, 28},
		{12, 28},
		{8, 24},
		{8, 12},
	} {
		c, d, n := clipText(conflict, fit.key), clipText(decision, fit.key), clipText(who, fit.name)
		room := escalationQuestionChars - utf8.RuneCountInString(fmt.Sprintf(shape, c, d, budget, n, ""))
		if room < 0 {
			continue
		}
		if room > escalationSummaryChars {
			room = escalationSummaryChars
		}
		return fmt.Sprintf(shape, c, d, budget, n, clipText(plainText(summary), room))
	}
	// Unreachable with any key the schema can hold; the backstop keeps the
	// promise anyway.
	return clipText(fmt.Sprintf(shape, clipText(conflict, 12), clipText(decision, 12), budget, clipText(who, 8), ""),
		escalationQuestionChars)
}

// renderBudget is §4.8's "10m", "30m", "2h".
func renderBudget(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

// firstText is the first value with something in it.
func firstText(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// clipText shortens to max runes, marking the cut with an ellipsis. Runes, not
// bytes: truncate() would cut a multi-byte character in half, and the column
// these end up in counts characters.
func clipText(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	if max <= 1 {
		return string(r[:max])
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

// plainText makes an agent's own words safe to quote inside a question: one
// line, and no double quote that could close the quotation it sits in.
func plainText(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), `"`, "'")
}
