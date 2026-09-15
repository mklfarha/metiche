package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/coordination"
	team_settings_entity "github.com/mklfarha/metiche/backend/entity/team_settings"
	"github.com/mklfarha/metiche/backend/enums"
)

// decisionresolve.go closes decision_contradiction conflicts once they are no
// longer true, and holds the ledger plumbing record_decision, the reviewer and
// report_judgement share (docs/DECISIONS.md §3.0, §4.4, §4.5, §4.8, §9.1).
//
// # The rules (§4.5), first that fits wins
//
//  1. the decision is superseded or revoked → SUPERSEDED;
//  2. the plan (intent) is done, abandoned or superseded, or gone → SUPERSEDED;
//  3. the plan's session is ended or abandoned, or gone → SUPERSEDED;
//  4. the pair at the CURRENT revisions of both subjects is judged no_conflict
//     → CONVERGED;
//
// anything else stays open. A chosen resolution becomes COORDINATED when a
// participant answered the conflict's notice with report_back done or
// acknowledged and a note, unless metiche is writing off an abandoned session.
//
// Revisions close nothing on their own: a revised plan or decision earns a new
// pair for the plan's owner, and that verdict converges or keeps the conflict.
//
// Everything here runs on the transaction that holds the team lock (or, for
// the sweeper, inside appendEvent's locked hook), so every query is a point
// lookup or a LIMITed index range scan.

// The pair kind and detector rules for decision conflicts.
const (
	decisionPairKind      = "decision_contradiction"
	RuleDecisionJudged    = "decision_contradiction.judged"
	RuleDecisionUnsure    = "decision_contradiction.unsure"
	decisionSubjectKind   = "decision"
	intentSubjectKind     = "intent"
	decisionNoteRationale = 160
)

// DecisionContradictionDedupeKey is one conflict per (decision, intent), not
// per revision: one disagreement stays one row, which is what lets a later
// no_conflict close it.
func DecisionContradictionDedupeKey(decisionUUID, intentUUID string) string {
	sum := sha256.Sum256([]byte("decision_contradiction|" + strings.ToLower(decisionUUID) + "|" + strings.ToLower(intentUUID)))
	return hex.EncodeToString(sum[:])
}

// decisionPairKey is §3.0's pair key: subject a is always the decision.
func decisionPairKey(decisionUUID string, decisionRev int64, intentUUID string, intentRev int64) string {
	return coordination.PairKey(decisionPairKind,
		coordination.PairSubject{Kind: decisionSubjectKind, UUID: decisionUUID, Revision: int(decisionRev)},
		coordination.PairSubject{Kind: intentSubjectKind, UUID: intentUUID, Revision: int(intentRev)})
}

// ─────────────────────────────────────────────
// The frozen interface (§9.1)
// ─────────────────────────────────────────────

// DecisionReleaseKind says what happened that may have settled a decision
// conflict.
type DecisionReleaseKind int

const (
	DecisionReleaseJudged           DecisionReleaseKind = iota + 1 // report_judgement no_conflict
	DecisionReleaseDecisionChanged                                 // record_decision supersede or revoke
	DecisionReleaseIntentEnded                                     // update_intent done/abandoned/superseded
	DecisionReleaseSessionEnded                                    // end_session
	DecisionReleaseSessionAbandoned                                // the sweeper writing a session off
)

// DecisionRelease is one such event.
type DecisionRelease struct {
	TeamUUID    uuid.UUID
	SessionUUID uuid.UUID // whose call or lapse caused it
	Kind        DecisionReleaseKind
	At          time.Time // the transaction's clock
}

// OpenDecisionConflictsOfSession lists the open or acknowledged decision_contradiction
// conflicts a session takes part in, oldest first, at most settleMaxConflicts.
// Driven by idx_participant_session.
func OpenDecisionConflictsOfSession(ctx context.Context, q queryer, teamUUID, sessionUUID uuid.UUID) ([]uuid.UUID, error) {
	return scanConflictIDs(ctx, q,
		"SELECT c.`id` FROM `conflict_participant` p JOIN `conflict` c ON c.`id` = p.`conflict_uuid` "+
			"WHERE p.`session_uuid` = ? AND c.`team_uuid` = ? AND c.`kind` = ? AND c.`status` IN (?, ?) "+
			"GROUP BY c.`id`, c.`created_at`, c.`key` ORDER BY c.`created_at`, c.`key` LIMIT ?",
		sessionUUID.String(), teamUUID.String(), int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED), settleMaxConflicts)
}

// openDecisionConflictsOfDecision lists the open decision conflicts on one
// decision, by the key its evidence names (the decider participant may not
// exist, so the evidence is authoritative). idx_conflict_open (team_uuid,
// status, severity) bounds the scan to the team's open conflicts.
func openDecisionConflictsOfDecision(ctx context.Context, q queryer, teamUUID uuid.UUID, decisionKey string) ([]uuid.UUID, error) {
	return scanConflictIDs(ctx, q,
		"SELECT `id` FROM `conflict` WHERE `team_uuid` = ? AND `status` IN (?, ?) AND `kind` = ? "+
			"AND JSON_UNQUOTE(JSON_EXTRACT(`evidence`, '$.overlap_path')) = ? ORDER BY `created_at`, `key` LIMIT ?",
		teamUUID.String(), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED),
		int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION), decisionKey, settleMaxConflicts)
}

// ExpireJudgementsOfSession marks every pending judgement assigned to the session expired.
// Driven by idx_judgement_assignment. No event.
func ExpireJudgementsOfSession(ctx context.Context, q queryer, sessionUUID uuid.UUID, now time.Time) (int64, error) {
	res, err := q.ExecContext(ctx,
		"UPDATE `judgement` SET `status` = ?, `updated_at` = ? WHERE `judge_session_uuid` = ? AND `status` = ?",
		int64(enums.JUDGEMENT_STATUS_EXPIRED), now, sessionUUID.String(), int64(enums.JUDGEMENT_STATUS_PENDING))
	if err != nil {
		return 0, retryable(err, "expiring the session's pairs to judge")
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// expireIntentJudgements expires an intent's pending judgements assigned to its
// session: all of them (belowRevision 0), or only those judged against an
// older revision of the plan. Driven by idx_judgement_assignment.
func expireIntentJudgements(ctx context.Context, q queryer, sessionUUID, intentUUID uuid.UUID, belowRevision int64, now time.Time) error {
	query := "UPDATE `judgement` SET `status` = ?, `updated_at` = ? WHERE `judge_session_uuid` = ? AND `status` = ? AND `subject_b_uuid` = ?"
	args := []any{int64(enums.JUDGEMENT_STATUS_EXPIRED), now, sessionUUID.String(), int64(enums.JUDGEMENT_STATUS_PENDING), intentUUID.String()}
	if belowRevision > 0 {
		query += " AND `subject_b_revision` < ?"
		args = append(args, belowRevision)
	}
	_, err := q.ExecContext(ctx, query, args...)
	return retryable(err, "expiring the plan's pairs to judge")
}

// expireDecisionJudgements expires the pending judgements on one decision: all
// of them (belowRevision 0), or those against an older wording. Driven by
// idx_judgement_subject (team_uuid, subject_a_uuid, …).
func expireDecisionJudgements(ctx context.Context, q queryer, teamUUID, decisionUUID uuid.UUID, belowRevision int64, now time.Time) error {
	query := "UPDATE `judgement` SET `status` = ?, `updated_at` = ? WHERE `team_uuid` = ? AND `subject_a_uuid` = ? AND `status` = ?"
	args := []any{int64(enums.JUDGEMENT_STATUS_EXPIRED), now, teamUUID.String(), decisionUUID.String(), int64(enums.JUDGEMENT_STATUS_PENDING)}
	if belowRevision > 0 {
		query += " AND `subject_a_revision` < ?"
		args = append(args, belowRevision)
	}
	_, err := q.ExecContext(ctx, query, args...)
	return retryable(err, "expiring the decision's pairs to judge")
}

// SettleDecisionConflict re-evaluates one decision_contradiction conflict (§4.5) and closes it
// when a rule fits. It returns false and writes nothing when the conflict is closed already or
// still stands. Must run on the transaction holding the team lock. The caller appends the
// conflict_resolved event (ConflictResolvedSummary / ConflictResolvedPayload handle the kind).
func SettleDecisionConflict(ctx context.Context, q queryer, conflictID uuid.UUID, rel DecisionRelease) (SettledConflict, bool, error) {
	out := SettledConflict{ID: conflictID, Kind: enums.CONFLICT_KIND_DECISION_CONTRADICTION}
	var (
		project     string
		sev, status int64
		escalated   sql.NullTime
	)
	err := q.QueryRowContext(ctx,
		"SELECT `key`, `project_uuid`, `severity`, `status`, `escalated_at`, "+
			"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(`evidence`, '$.overlap_path')), '') "+
			"FROM `conflict` WHERE `id` = ? AND `team_uuid` = ? AND `kind` = ? AND `status` IN (?, ?)",
		conflictID.String(), rel.TeamUUID.String(), int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED)).
		Scan(&out.Key, &project, &sev, &status, &escalated, &out.OverlapPath)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, false, nil
	case err != nil:
		return out, false, retryable(err, "reading the decision conflict to re-evaluate")
	}
	if out.OverlapPath == "null" {
		out.OverlapPath = ""
	}
	out.ProjectUUID, _ = uuid.FromString(project)
	out.Severity = enums.ConflictSeverity(sev)
	out.Previous = enums.ConflictStatus(status)

	st, err := loadDecisionSettleState(ctx, q, rel, conflictID, out.OverlapPath)
	if err != nil {
		return out, false, err
	}
	st.EscalatedAt = escalated

	resolution, rule, ok := decideDecisionSettlement(st, rel.Kind)
	if !ok {
		return out, false, nil
	}
	out.Resolution = resolution
	out.Note = buildDecisionResolutionNote(st, rule, resolution, rel)

	res, err := q.ExecContext(ctx,
		"UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolution_note` = ?, `resolved_at` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` IN (?, ?)",
		int64(enums.CONFLICT_STATUS_RESOLVED), int64(out.Resolution), out.Note, rel.At, rel.At,
		conflictID.String(), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED))
	if err != nil {
		return out, false, retryable(err, "closing the settled decision conflict")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return out, false, nil
	}
	return out, true, nil
}

// ─────────────────────────────────────────────
// The rule — pure
// ─────────────────────────────────────────────

// decisionSettleState is everything the rule and the note read.
type decisionSettleState struct {
	DecisionFound    bool
	DecisionKey      string
	DecisionStatus   enums.DecisionStatus
	DecisionRevision int64
	DecisionUpdated  sql.NullTime
	SupersededByKey  string
	RecorderName     string

	IntentFound  bool
	IntentKey    string
	IntentStatus enums.IntentStatus
	IntentEnded  sql.NullTime

	SessionFound bool
	Owner        settleSide

	// Current is the judgement on the pair at both subjects' current
	// revisions; CurrentVerdict is INVALID when it is not judged.
	CurrentVerdict enums.JudgementVerdict
	JudgeName      string
	Rationale      string
	// DecisionRevised is true when the latest conflict verdict on the pair was
	// against an older wording of the decision.
	DecisionRevised bool

	ActorName   string
	Notes       []settleNote
	EscalatedAt sql.NullTime
}

// Which §4.5 rule settled it; the note is worded for it.
const (
	decisionRuleDecisionEnded = 1
	decisionRuleIntentEnded   = 2
	decisionRuleSessionEnded  = 3
	decisionRuleConverged     = 4
)

// decideDecisionSettlement is §4.5, and it is pure. It returns the resolution,
// the rule that fired, and false to keep the conflict open.
func decideDecisionSettlement(st decisionSettleState, kind DecisionReleaseKind) (enums.ConflictResolution, int, bool) {
	var (
		res  enums.ConflictResolution
		rule int
	)
	switch {
	case !st.DecisionFound || st.DecisionStatus == enums.DECISION_STATUS_SUPERSEDED || st.DecisionStatus == enums.DECISION_STATUS_REVOKED:
		res, rule = enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleDecisionEnded
	case !st.IntentFound || st.IntentStatus == enums.INTENT_STATUS_DONE ||
		st.IntentStatus == enums.INTENT_STATUS_ABANDONED || st.IntentStatus == enums.INTENT_STATUS_SUPERSEDED:
		res, rule = enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleIntentEnded
	case !st.SessionFound || st.Owner.Status == enums.SESSION_STATUS_ENDED || st.Owner.Status == enums.SESSION_STATUS_ABANDONED:
		res, rule = enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleSessionEnded
	case st.CurrentVerdict == enums.JUDGEMENT_VERDICT_NO_CONFLICT:
		res, rule = enums.CONFLICT_RESOLUTION_CONVERGED, decisionRuleConverged
	default:
		return enums.CONFLICT_RESOLUTION_INVALID, 0, false
	}
	if kind != DecisionReleaseSessionAbandoned {
		for _, n := range st.Notes {
			if n.Agrees {
				return enums.CONFLICT_RESOLUTION_COORDINATED, rule, true
			}
		}
	}
	return res, rule, true
}

// buildDecisionResolutionNote is §4.8's resolution note. Free text is
// sanitized; it always fits conflict.resolution_note.
func buildDecisionResolutionNote(st decisionSettleState, rule int, res enums.ConflictResolution, rel DecisionRelease) string {
	decision := firstNonEmpty(st.DecisionKey, "the decision")
	intent := firstNonEmpty(st.IntentKey, "the plan")
	owner := st.Owner.name()
	rationale := clip(sanitizeNoteText(st.Rationale), decisionNoteRationale)
	quoted := ""
	if rationale != "" {
		quoted = fmt.Sprintf(": \"%s\"", rationale)
	}
	at := clock(rel.At)
	whenOr := func(t sql.NullTime) string {
		if t.Valid {
			return clock(t.Time)
		}
		return at
	}

	var s string
	switch rule {
	case decisionRuleDecisionEnded:
		actor := firstNonEmpty(st.RecorderName, "an agent")
		if rel.Kind == DecisionReleaseDecisionChanged {
			actor = firstNonEmpty(st.ActorName, actor)
		}
		switch {
		case !st.DecisionFound:
			s = fmt.Sprintf("Cleared by metiche: %s no longer exists, so %s no longer breaks a standing decision.", decision, intent)
		case st.DecisionStatus == enums.DECISION_STATUS_SUPERSEDED:
			s = fmt.Sprintf("Settled by the agents: %s superseded %s with %s at %s, so %s no longer breaks a standing decision.",
				actor, decision, firstNonEmpty(st.SupersededByKey, "a new decision"), whenOr(st.DecisionUpdated), intent)
		default:
			s = fmt.Sprintf("Settled by the agents: %s revoked %s at %s, so %s no longer breaks a standing decision.",
				actor, decision, whenOr(st.DecisionUpdated), intent)
		}
	case decisionRuleIntentEnded:
		if !st.IntentFound {
			s = fmt.Sprintf("Cleared by metiche: %s no longer exists, so it no longer breaks %s.", intent, decision)
		} else {
			s = fmt.Sprintf("Settled by the agents: %s marked %s %s at %s.", owner, intent, st.IntentStatus.String(), whenOr(st.IntentEnded))
		}
	case decisionRuleSessionEnded:
		switch {
		case !st.SessionFound:
			s = fmt.Sprintf("Cleared by metiche: the session behind %s no longer exists, ending %s.", intent, intent)
		case st.Owner.Status == enums.SESSION_STATUS_ABANDONED:
			s = fmt.Sprintf("Cleared by metiche: %s was abandoned at %s after no heartbeat, ending %s.", owner, whenOr(st.Owner.EndedAt), intent)
		default:
			s = fmt.Sprintf("Settled by the agents: %s ended its session at %s%s, ending %s.", owner, whenOr(st.Owner.EndedAt), outcomeSuffix(st.Owner), intent)
		}
	case decisionRuleConverged:
		judge := firstNonEmpty(st.JudgeName, owner)
		if st.DecisionRevised {
			s = fmt.Sprintf("Settled by the agents: %s revised %s to r%d at %s, and %s judged %s no longer contradicts it%s.",
				firstNonEmpty(st.RecorderName, "an agent"), decision, st.DecisionRevision, whenOr(st.DecisionUpdated), judge, intent, quoted)
		} else {
			s = fmt.Sprintf("Settled by the agents: %s revised %s at %s and judged it no longer contradicts %s (r%d)%s.",
				judge, intent, at, decision, st.DecisionRevision, quoted)
		}
	}

	tail := ""
	if st.EscalatedAt.Valid {
		tail = " A person was asked at " + clock(st.EscalatedAt.Time) + "."
	}
	var b strings.Builder
	b.WriteString(s)
	if res == enums.CONFLICT_RESOLUTION_COORDINATED {
		room := resolutionNoteChars - len([]rune(s)) - len([]rune(tail))
		var quotedNotes []settleNote
		for _, n := range st.Notes {
			if n.Text != "" {
				quotedNotes = append(quotedNotes, n)
			}
		}
		if len(quotedNotes) > 0 {
			per := room/len(quotedNotes) - 18
			if per > settleQuoteChars {
				per = settleQuoteChars
			}
			if per >= 24 {
				for _, n := range quotedNotes {
					fmt.Fprintf(&b, " %s said: \"%s\"", firstNonEmpty(n.SessionKey, "an agent"), clip(sanitizeNoteText(n.Text), per))
				}
			}
		}
	}
	body := clip(b.String(), resolutionNoteChars-len([]rune(tail)))
	return clip(body+tail, resolutionNoteChars)
}

// ─────────────────────────────────────────────
// Loading the state
// ─────────────────────────────────────────────

func loadDecisionSettleState(ctx context.Context, q queryer, rel DecisionRelease, conflictID uuid.UUID, decisionKey string) (decisionSettleState, error) {
	st := decisionSettleState{DecisionKey: decisionKey}

	var (
		decisionID, supersededBy, recorder string
		dStatus                            int64
	)
	err := q.QueryRowContext(ctx,
		"SELECT `id`, `status`, `revision`, `updated_at`, COALESCE(`superseded_by_uuid`, ''), COALESCE(`recorded_by_session_uuid`, '') "+
			"FROM `decision` WHERE `team_uuid` = ? AND `key` = ?",
		rel.TeamUUID.String(), decisionKey).Scan(&decisionID, &dStatus, &st.DecisionRevision, &st.DecisionUpdated, &supersededBy, &recorder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return st, retryable(err, "reading the conflict's decision")
	default:
		st.DecisionFound = true
		st.DecisionStatus = enums.DecisionStatus(dStatus)
	}

	// The plan: the participant whose subject is an intent. Driven by the
	// conflict_has_participants foreign key.
	var intentID, ownerSession string
	err = q.QueryRowContext(ctx,
		"SELECT `subject_uuid`, `session_uuid` FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `subject_kind` = ? "+
			"ORDER BY `created_at` LIMIT 1",
		conflictID.String(), int64(enums.SUBJECT_KIND_INTENT)).Scan(&intentID, &ownerSession)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, retryable(err, "reading the conflict's plan")
	}

	var intentRev int64
	if intentID != "" {
		var (
			iStatus int64
			session string
		)
		err = q.QueryRowContext(ctx,
			"SELECT `key`, `status`, `revision`, `ended_at`, `session_uuid` FROM `intent` WHERE `id` = ?", intentID).
			Scan(&st.IntentKey, &iStatus, &intentRev, &st.IntentEnded, &session)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return st, retryable(err, "reading the conflict's plan")
		default:
			st.IntentFound = true
			st.IntentStatus = enums.IntentStatus(iStatus)
			ownerSession = session
		}
	}

	if ownerSession != "" {
		side, found, err := loadSettleSide(ctx, q, ownerSession)
		if err != nil {
			return st, err
		}
		st.Owner, st.SessionFound = side, found
	}

	if st.DecisionFound && st.IntentFound {
		var (
			verdict  sql.NullInt64
			judge    sql.NullString
			rational sql.NullString
		)
		err = q.QueryRowContext(ctx,
			"SELECT `verdict`, `judge_session_uuid`, `rationale` FROM `judgement` WHERE `team_uuid` = ? AND `pair_key` = ? AND `status` = ?",
			rel.TeamUUID.String(), decisionPairKey(decisionID, st.DecisionRevision, intentID, intentRev),
			int64(enums.JUDGEMENT_STATUS_JUDGED)).Scan(&verdict, &judge, &rational)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return st, retryable(err, "reading the verdict on the current pair")
		default:
			st.CurrentVerdict = enums.JudgementVerdict(verdict.Int64)
			st.Rationale = rational.String
			if judge.Valid {
				if side, found, err := loadSettleSide(ctx, q, judge.String); err != nil {
					return st, err
				} else if found {
					st.JudgeName = side.name()
				}
			}
		}

		// "Plan revised" and "decision revised" are told apart by the latest
		// verdict that raised the conflict (§4.8). idx_judgement_subject.
		var latestRev sql.NullInt64
		err = q.QueryRowContext(ctx,
			"SELECT `subject_a_revision` FROM `judgement` WHERE `team_uuid` = ? AND `subject_a_uuid` = ? AND `subject_b_uuid` = ? "+
				"AND `status` = ? AND `verdict` IN (?, ?) ORDER BY `judged_at` DESC LIMIT 1",
			rel.TeamUUID.String(), decisionID, intentID, int64(enums.JUDGEMENT_STATUS_JUDGED),
			int64(enums.JUDGEMENT_VERDICT_CONFLICT), int64(enums.JUDGEMENT_VERDICT_UNSURE)).Scan(&latestRev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return st, retryable(err, "reading the verdict that raised the conflict")
		}
		st.DecisionRevised = latestRev.Valid && latestRev.Int64 < st.DecisionRevision
	}

	if supersededBy != "" {
		if err := q.QueryRowContext(ctx, "SELECT `key` FROM `decision` WHERE `id` = ?", supersededBy).
			Scan(&st.SupersededByKey); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return st, retryable(err, "reading the superseding decision")
		}
	}
	if recorder != "" {
		side, found, err := loadSettleSide(ctx, q, recorder)
		if err != nil {
			return st, err
		}
		if found {
			st.RecorderName = side.name()
		}
	}
	if !rel.SessionUUID.IsNil() {
		side, found, err := loadSettleSide(ctx, q, rel.SessionUUID.String())
		if err != nil {
			return st, err
		}
		if found {
			st.ActorName = side.name()
		}
	}

	sides, err := loadDecisionParticipantSides(ctx, q, conflictID)
	if err != nil {
		return st, err
	}
	notes, err := loadSettleNotes(ctx, q, conflictID, sides)
	if err != nil {
		return st, err
	}
	st.Notes = notes
	return st, nil
}

// loadSettleSide reads one session with its agent label, by primary key.
func loadSettleSide(ctx context.Context, q queryer, sessionUUID string) (settleSide, bool, error) {
	s := settleSide{SessionUUID: sessionUUID}
	var status, outcome int64
	err := q.QueryRowContext(ctx,
		"SELECT s.`key`, COALESCE(a.`label`, ''), s.`status`, COALESCE(s.`outcome`, 0), COALESCE(s.`outcome_note`, ''), s.`ended_at` "+
			"FROM `session` s LEFT JOIN `agent` a ON a.`id` = s.`agent_uuid` WHERE s.`id` = ?",
		sessionUUID).Scan(&s.Key, &s.Label, &status, &outcome, &s.OutcomeNote, &s.EndedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s, false, nil
	case err != nil:
		return s, false, retryable(err, "reading a session")
	}
	s.Status, s.Outcome = enums.SessionStatus(status), enums.SessionOutcome(outcome)
	return s, true, nil
}

func loadDecisionParticipantSides(ctx context.Context, q queryer, conflictID uuid.UUID) ([]settleSide, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT p.`session_uuid`, COALESCE(s.`key`, '') FROM `conflict_participant` p "+
			"LEFT JOIN `session` s ON s.`id` = p.`session_uuid` WHERE p.`conflict_uuid` = ? "+
			"ORDER BY p.`created_at`, p.`session_uuid` LIMIT ?",
		conflictID.String(), settleMaxParticipants)
	if err != nil {
		return nil, retryable(err, "reading the decision conflict's participants")
	}
	defer func() { _ = rows.Close() }()
	var out []settleSide
	for rows.Next() {
		var s settleSide
		if err := rows.Scan(&s.SessionUUID, &s.Key); err != nil {
			return nil, retryable(err, "reading the decision conflict's participants")
		}
		out = append(out, s)
	}
	return out, retryable(rows.Err(), "reading the decision conflict's participants")
}

// settleDecisionConflicts settles each of ids from inside a tool's Apply or
// Detect, and appends one conflict_resolved event per conflict it closed. It
// returns the settled conflicts.
func (h *Handler) settleDecisionConflicts(ctx context.Context, tc *TxContext, rel DecisionRelease, ids []uuid.UUID, actor eventActor) ([]SettledConflict, error) {
	var out []SettledConflict
	for _, id := range ids {
		settled, ok, err := SettleDecisionConflict(ctx, tc.Tx, id, rel)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := appendConflictResolvedEvent(ctx, tc, rel.TeamUUID, settled, actor); err != nil {
			return nil, err
		}
		out = append(out, settled)
	}
	return out, nil
}

// ─────────────────────────────────────────────
// The ledger
// ─────────────────────────────────────────────

// decisionSettings is the part of team_settings the decisions feature reads.
type decisionSettings struct {
	JudgeWindow         time.Duration
	MaxReviewsPerMinute int
	ReviewBlockEnabled  bool
}

// loadDecisionSettings reads the team row's settings by primary key.
func loadDecisionSettings(ctx context.Context, q queryer, teamUUID uuid.UUID) (decisionSettings, error) {
	out := decisionSettings{JudgeWindow: decisionJudgeWindow, MaxReviewsPerMinute: defaultMaxReviewsPerMinute, ReviewBlockEnabled: true}
	var raw sql.NullString
	if err := q.QueryRowContext(ctx, "SELECT `settings` FROM `team` WHERE `id` = ?", teamUUID.String()).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, retryable(err, "reading the team's settings")
	}
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return out, nil
	}
	s := team_settings_entity.TeamSettingsFromJSON([]byte(raw.String))
	if s.JudgeWindowSeconds.Valid && s.JudgeWindowSeconds.Int64 > 0 {
		out.JudgeWindow = time.Duration(s.JudgeWindowSeconds.Int64) * time.Second
	}
	if s.MaxReviewsPerMinute.Valid && s.MaxReviewsPerMinute.Int64 > 0 {
		out.MaxReviewsPerMinute = int(s.MaxReviewsPerMinute.Int64)
	}
	if s.ReviewBlockEnabled.Valid {
		out.ReviewBlockEnabled = s.ReviewBlockEnabled.Bool
	}
	return out, nil
}

// reviewsAssignedRecently counts a session's pairs assigned in the last
// judgeRateWindow, up to limit. idx_judgement_assignment, capped.
func reviewsAssignedRecently(ctx context.Context, q queryer, sessionUUID string, now time.Time, limit int) (int, error) {
	var n int
	err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM (SELECT 1 FROM `judgement` WHERE `judge_session_uuid` = ? AND `status` = ? AND `updated_at` >= ? LIMIT ?) recent",
		sessionUUID, int64(enums.JUDGEMENT_STATUS_PENDING), now.Add(-judgeRateWindow), limit).Scan(&n)
	return n, retryable(err, "counting the pairs assigned to judge")
}

// decisionJudgement is one pair to write into the ledger.
type decisionJudgement struct {
	DecisionUUID     string
	DecisionRevision int64
	IntentUUID       string
	IntentRevision   int64
	// JudgeSession is the session assigned to judge it; empty leaves the pair
	// unassigned for the sweeper.
	JudgeSession string
}

// insertDecisionJudgement writes one pair. The unique index makes the first
// insert win, so a pair that already exists is left exactly as it is and
// reported as not inserted.
func insertDecisionJudgement(ctx context.Context, q queryer, teamUUID uuid.UUID, j decisionJudgement, window time.Duration, now time.Time) (string, bool, error) {
	id, err := uuid.NewV4()
	if err != nil {
		return "", false, err
	}
	pairKey := decisionPairKey(j.DecisionUUID, j.DecisionRevision, j.IntentUUID, j.IntentRevision)
	var (
		judge, expires any
		count          int
	)
	if j.JudgeSession != "" {
		judge, expires, count = j.JudgeSession, now.Add(window), 1
	}
	res, err := q.ExecContext(ctx,
		"INSERT INTO `judgement` (`id`,`team_uuid`,`pair_key`,`kind`,`subject_a_kind`,`subject_a_uuid`,`subject_a_revision`,"+
			"`subject_b_kind`,`subject_b_uuid`,`subject_b_revision`,`status`,`judge_session_uuid`,`judging_expires_at`,"+
			"`assignment_count`,`pinned`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?) "+
			"ON DUPLICATE KEY UPDATE `id` = `id`",
		id.String(), teamUUID.String(), pairKey, int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION),
		int64(enums.SUBJECT_KIND_DECISION), j.DecisionUUID, j.DecisionRevision,
		int64(enums.SUBJECT_KIND_INTENT), j.IntentUUID, j.IntentRevision,
		int64(enums.JUDGEMENT_STATUS_PENDING), judge, expires, count, now, now)
	if err != nil {
		return pairKey, false, retryable(err, "recording the pair to judge")
	}
	n, _ := res.RowsAffected()
	return pairKey, n == 1, nil
}

// existingPairKeys returns which of keys already have a judgement, in any
// status. uq_judgement_pair point lookups.
func existingPairKeys(ctx context.Context, q queryer, teamUUID uuid.UUID, keys []string) (map[string]bool, error) {
	out := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	args := []any{teamUUID.String()}
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, len(keys))
	rows, err := q.QueryContext(ctx,
		"SELECT `pair_key` FROM `judgement` WHERE `team_uuid` = ? AND `pair_key` IN ("+placeholders(len(keys))+") LIMIT ?", args...)
	if err != nil {
		return nil, retryable(err, "checking which pairs were already asked")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, retryable(err, "checking which pairs were already asked")
		}
		out[k] = true
	}
	return out, retryable(rows.Err(), "checking which pairs were already asked")
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
