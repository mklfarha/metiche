package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/coordination"
	conflict_evidence_entity "github.com/mklfarha/metiche/backend/entity/conflict_evidence"
	team_settings_entity "github.com/mklfarha/metiche/backend/entity/team_settings"
	"github.com/mklfarha/metiche/backend/enums"
)

// duplicateresolve.go is the ledger, the conflict and the settlement of
// duplicate_work (docs/DUPLICATES.md §3.0, §3.3, §4.7, §4.10, §9.2): two live
// plans that may build the same thing.
//
// # Storage convention
//
// Subject b is always the judge's plan: the intent whose declaration or
// rewording surfaced the pair. Subject a is the other plan. The pair key is
// symmetric; the storage order is fixed. On a conflict b is the yield side
// (at_fault "later").
//
// # Settlement (§4.7), first rule that fits wins
//
//  1. either intent is abandoned or superseded, or its row is gone → YIELDED;
//  2. b is done → SUPERSEDED (it finished anyway; the work was duplicated);
//  3. b's session is ended or abandoned, or gone → SUPERSEDED;
//  4. a's session is ended or abandoned, or gone, and a is not done → SUPERSEDED;
//  5. the latest judged verdict on the unordered pair is no_conflict → CONVERGED;
//
// anything else, including a done while b is still live, stays open. A chosen
// resolution becomes COORDINATED when a participant answered the conflict's
// notice with report_back done or acknowledged and a note, unless metiche is
// writing off an abandoned session.
//
// Everything here runs on the transaction that holds the team lock (or inside
// the sweeper's locked appendEvent hook), so every query is a point lookup or
// a LIMITed index range scan.

const (
	RuleDuplicateSameIssue = "duplicate_work.same_issue"
	RuleDuplicateWords     = "duplicate_work.words"
	RuleDuplicateUnsure    = "duplicate_work.unsure"
)

const (
	// duplicatePairKind is the judgement and pair kind.
	duplicatePairKind = "duplicate_work"
	// duplicateMaxLiveIntentsScanned bounds the project scan (idx_intent_live).
	duplicateMaxLiveIntentsScanned = 100
	// duplicateSameIssueLimit bounds the team-wide issue lookup.
	duplicateSameIssueLimit = 8
	// duplicateMaxCandidates is how many scored candidates are kept before
	// the pinned and existing checks.
	duplicateMaxCandidates = 10
	// duplicateMaxPairsPerCall is how many pairs one call mints, so one slot
	// of the shared per-minute cap is always left for a decision.
	duplicateMaxPairsPerCall = 2
	// duplicateNoticeActionChars is the incumbent notice action, what
	// get_instructions shows.
	duplicateNoticeActionChars = instructionTextChars
	// duplicateQuoteChars caps the rationale quoted in the incumbent notice.
	duplicateQuoteChars = 60
	// duplicateSummaryActionChars caps the other plan's summary in the
	// suggested action.
	duplicateSummaryActionChars = 120
	// duplicateSummaryQuestionChars caps a summary in a question body.
	duplicateSummaryQuestionChars = 40
	// duplicateSessionIntentsLimit bounds one session's live intents read.
	duplicateSessionIntentsLimit = 64
	// duplicateWordsShown is how many shared words a why line or an
	// adjuster names.
	duplicateWordsShown = 4
	// duplicateNoteRationale caps the rationale quoted in a resolution note.
	duplicateNoteRationale = 160
	// duplicateYieldReasonChars is conflict.suggested_yield_reason.
	duplicateYieldReasonChars = 80
)

// DuplicateWorkDedupeKey is one conflict per unordered pair of intents, not per
// revision: whichever side surfaces the pair, one row.
func DuplicateWorkDedupeKey(intentA, intentB string) string {
	a, b := strings.ToLower(strings.TrimSpace(intentA)), strings.ToLower(strings.TrimSpace(intentB))
	if b < a {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte("duplicate_work|" + a + "|" + b))
	return hex.EncodeToString(sum[:])
}

// duplicatePairKey is §3.0's pair key: subject a is the other plan, subject b
// the judge's, both at their wording_revision.
func duplicatePairKey(otherIntent string, otherWording int64, judgeIntent string, judgeWording int64) string {
	return coordination.PairKey(duplicatePairKind,
		coordination.PairSubject{Kind: intentSubjectKind, UUID: otherIntent, Revision: int(otherWording)},
		coordination.PairSubject{Kind: intentSubjectKind, UUID: judgeIntent, Revision: int(judgeWording)})
}

// ─────────────────────────────────────────────
// The frozen interface (§9.2)
// ─────────────────────────────────────────────

// DuplicateReleaseKind says what happened that may have settled a duplicate conflict.
type DuplicateReleaseKind int

const (
	DuplicateReleaseJudged           DuplicateReleaseKind = iota + 1 // report_judgement no_conflict
	DuplicateReleaseIntentEnded                                      // update_intent done/abandoned/superseded
	DuplicateReleaseSessionEnded                                     // end_session
	DuplicateReleaseSessionAbandoned                                 // the sweeper writing a session off
)

// DuplicateRelease is one such event.
type DuplicateRelease struct {
	TeamUUID    uuid.UUID
	SessionUUID uuid.UUID // whose call or lapse caused it
	Kind        DuplicateReleaseKind
	At          time.Time // the transaction's clock
}

// OpenDuplicateConflictsOfSession lists the open or acknowledged duplicate_work conflicts a
// session is in, as a participant or as the owner of subject a (§4.7), oldest first, at most
// settleMaxConflicts.
func OpenDuplicateConflictsOfSession(ctx context.Context, q queryer, teamUUID, sessionUUID uuid.UUID) ([]uuid.UUID, error) {
	return openDuplicateConflictsOf(ctx, q, teamUUID, sessionUUID, nil)
}

// openDuplicateConflictsOf is OpenDuplicateConflictsOfSession plus conflicts
// reached through extra intents of the session, for update_intent: the intent
// that just ended is no longer declared or active, so the session's live
// intents no longer name it, and an incumbent that was not told yet is not a
// participant.
func openDuplicateConflictsOf(ctx context.Context, q queryer, teamUUID, sessionUUID uuid.UUID, extraIntents []uuid.UUID) ([]uuid.UUID, error) {
	seen := map[string]bool{}
	var candidates []string

	// (a) The session's participant rows. idx_participant_session.
	rows, err := q.QueryContext(ctx,
		"SELECT c.`id` FROM `conflict_participant` p JOIN `conflict` c ON c.`id` = p.`conflict_uuid` "+
			"WHERE p.`session_uuid` = ? AND c.`team_uuid` = ? AND c.`kind` = ? AND c.`status` IN (?, ?) "+
			"GROUP BY c.`id`, c.`created_at`, c.`key` ORDER BY c.`created_at`, c.`key` LIMIT ?",
		sessionUUID.String(), teamUUID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED), settleMaxConflicts)
	if err != nil {
		return nil, retryable(err, "finding the duplicate conflicts this session is in")
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, retryable(err, "finding the duplicate conflicts this session is in")
		}
		if !seen[id] {
			seen[id] = true
			candidates = append(candidates, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, retryable(err, "finding the duplicate conflicts this session is in")
	}
	_ = rows.Close()

	// (b) Conflicts on pairs where one of the session's plans is subject a.
	intents, err := sessionLiveIntentIDs(ctx, q, sessionUUID)
	if err != nil {
		return nil, err
	}
	for _, id := range extraIntents {
		intents = appendUnique(intents, id.String())
	}
	if len(intents) > 0 {
		args := []any{teamUUID.String()}
		for _, id := range intents {
			args = append(args, id)
		}
		args = append(args, int64(enums.CONFLICT_KIND_DUPLICATE_WORK), settleMaxConflicts*4)
		jrows, err := q.QueryContext(ctx,
			"SELECT DISTINCT `conflict_uuid` FROM `judgement` WHERE `team_uuid` = ? AND `subject_a_uuid` IN ("+placeholders(len(intents))+") "+
				"AND `kind` = ? AND `conflict_uuid` IS NOT NULL LIMIT ?", args...)
		if err != nil {
			return nil, retryable(err, "finding the duplicate conflicts on this session's plans")
		}
		var reached []string
		for jrows.Next() {
			var id string
			if err := jrows.Scan(&id); err != nil {
				_ = jrows.Close()
				return nil, retryable(err, "finding the duplicate conflicts on this session's plans")
			}
			if !seen[id] {
				reached = append(reached, id)
			}
		}
		if err := jrows.Err(); err != nil {
			_ = jrows.Close()
			return nil, retryable(err, "finding the duplicate conflicts on this session's plans")
		}
		_ = jrows.Close()
		for _, id := range reached {
			seen[id] = true
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Open or acknowledged only, oldest first, by primary key.
	args := []any{teamUUID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED)}
	for _, id := range candidates {
		args = append(args, id)
	}
	args = append(args, settleMaxConflicts)
	return scanConflictIDs(ctx, q,
		"SELECT `id` FROM `conflict` WHERE `team_uuid` = ? AND `kind` = ? AND `status` IN (?, ?) "+
			"AND `id` IN ("+placeholders(len(candidates))+") ORDER BY `created_at`, `key` LIMIT ?", args...)
}

// sessionLiveIntentIDs reads a session's declared or active intents, capped.
// The intent_has_session foreign key's index.
func sessionLiveIntentIDs(ctx context.Context, q queryer, sessionUUID uuid.UUID) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT `id` FROM `intent` WHERE `session_uuid` = ? AND `status` IN (?, ?) ORDER BY `created_at`, `id` LIMIT ?",
		sessionUUID.String(), int64(enums.INTENT_STATUS_DECLARED), int64(enums.INTENT_STATUS_ACTIVE), duplicateSessionIntentsLimit)
	if err != nil {
		return nil, retryable(err, "reading the session's plans")
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, retryable(err, "reading the session's plans")
		}
		out = append(out, id)
	}
	return out, retryable(rows.Err(), "reading the session's plans")
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// ExpireDuplicatePairsOnIntents marks pending duplicate_work judgements whose subject a is one of
// intentIDs expired. idx_judgement_subject. No event.
func ExpireDuplicatePairsOnIntents(ctx context.Context, q queryer, teamUUID uuid.UUID, intentIDs []uuid.UUID, now time.Time) (int64, error) {
	if len(intentIDs) == 0 {
		return 0, nil
	}
	args := []any{int64(enums.JUDGEMENT_STATUS_EXPIRED), now, teamUUID.String()}
	for _, id := range intentIDs {
		args = append(args, id.String())
	}
	args = append(args, int64(enums.CONFLICT_KIND_DUPLICATE_WORK), int64(enums.JUDGEMENT_STATUS_PENDING))
	res, err := q.ExecContext(ctx,
		"UPDATE `judgement` SET `status` = ?, `updated_at` = ? WHERE `team_uuid` = ? AND `subject_a_uuid` IN ("+placeholders(len(intentIDs))+") "+
			"AND `kind` = ? AND `status` = ?", args...)
	if err != nil {
		return 0, retryable(err, "expiring the pairs other agents were asked about these plans")
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DuplicatePlanKeys reads the "plans:<a>,<b>" fact from a conflict's evidence adjusters.
func DuplicatePlanKeys(adjusters []string) (a, b string, ok bool) {
	for _, f := range adjusters {
		rest, found := strings.CutPrefix(f, "plans:")
		if !found {
			continue
		}
		parts := strings.Split(rest, ",")
		if len(parts) != 2 {
			return "", "", false
		}
		a, b = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if a == "" || b == "" {
			return "", "", false
		}
		return a, b, true
	}
	return "", "", false
}

// SettleDuplicateConflict re-evaluates one duplicate_work conflict (§4.7) and closes it when a
// rule fits. It returns false and writes nothing when the conflict is closed already or still
// stands. Must run on the transaction holding the team lock. The caller appends the
// conflict_resolved event (ConflictResolvedSummary / ConflictResolvedPayload handle the kind).
func SettleDuplicateConflict(ctx context.Context, q queryer, conflictID uuid.UUID, rel DuplicateRelease) (SettledConflict, bool, error) {
	out := SettledConflict{ID: conflictID, Kind: enums.CONFLICT_KIND_DUPLICATE_WORK}
	var (
		project, yield, evidence string
		sev, status              int64
		escalated                sql.NullTime
	)
	err := q.QueryRowContext(ctx,
		"SELECT `key`, `project_uuid`, `severity`, `status`, `escalated_at`, COALESCE(`suggested_yield_session_uuid`, ''), "+
			"COALESCE(`evidence`, '{}') FROM `conflict` WHERE `id` = ? AND `team_uuid` = ? AND `kind` = ? AND `status` IN (?, ?)",
		conflictID.String(), rel.TeamUUID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED)).
		Scan(&out.Key, &project, &sev, &status, &escalated, &yield, &evidence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, false, nil
	case err != nil:
		return out, false, retryable(err, "reading the duplicate conflict to re-evaluate")
	}
	out.ProjectUUID, _ = uuid.FromString(project)
	out.Severity = enums.ConflictSeverity(sev)
	out.Previous = enums.ConflictStatus(status)
	ev := conflict_evidence_entity.ConflictEvidenceFromJSON([]byte(evidence))
	out.OverlapPath = ev.OverlapPath.ValueOrZero()
	aKey, bKey, ok := DuplicatePlanKeys(ev.Adjusters)
	if !ok {
		// Nothing truthful can be said about which plans these are.
		return out, false, nil
	}

	st, err := loadDuplicateSettleState(ctx, q, rel, conflictID, aKey, bKey, yield)
	if err != nil {
		return out, false, err
	}
	st.EscalatedAt = escalated
	out.PlanKeys = [2]string{st.B.Key, st.A.Key}

	resolution, rule, settled := decideDuplicateSettlement(st, rel.Kind)
	if !settled {
		return out, false, nil
	}
	out.Resolution = resolution
	out.Note = buildDuplicateResolutionNote(st, rule, resolution, rel)

	res, err := q.ExecContext(ctx,
		"UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolution_note` = ?, `resolved_at` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` IN (?, ?)",
		int64(enums.CONFLICT_STATUS_RESOLVED), int64(out.Resolution), out.Note, rel.At, rel.At,
		conflictID.String(), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED))
	if err != nil {
		return out, false, retryable(err, "closing the settled duplicate conflict")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return out, false, nil
	}
	return out, true, nil
}

// settleDuplicateConflicts settles each of ids from inside a tool's Apply or
// Detect and appends one conflict_resolved event per conflict it closed.
func (h *Handler) settleDuplicateConflicts(ctx context.Context, tc *TxContext, rel DuplicateRelease, ids []uuid.UUID, actor eventActor) ([]SettledConflict, error) {
	var out []SettledConflict
	for _, id := range ids {
		settled, ok, err := SettleDuplicateConflict(ctx, tc.Tx, id, rel)
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
// The rule — pure
// ─────────────────────────────────────────────

// duplicatePlanState is one of the two plans as the rule and the note read it.
type duplicatePlanState struct {
	Found   bool
	ID      string
	Key     string
	Status  enums.IntentStatus
	EndedAt sql.NullTime
	// SessionFound and Owner are the plan's session.
	SessionFound bool
	Owner        settleSide
}

func (p duplicatePlanState) intentOver() bool {
	return !p.Found || p.Status == enums.INTENT_STATUS_ABANDONED || p.Status == enums.INTENT_STATUS_SUPERSEDED
}

func (p duplicatePlanState) sessionOver() bool {
	return !p.SessionFound || p.Owner.Status == enums.SESSION_STATUS_ENDED || p.Owner.Status == enums.SESSION_STATUS_ABANDONED
}

// duplicateSettleState is everything decideDuplicateSettlement and the note read.
type duplicateSettleState struct {
	// B is the yield side, A the other plan.
	A, B duplicatePlanState

	// Latest is the latest judged verdict on the unordered pair, INVALID when
	// there is none; JudgeName, Rationale, JudgedKey and OtherKey describe it.
	Latest    enums.JudgementVerdict
	JudgeName string
	Rationale string
	JudgedKey string
	OtherKey  string

	Notes       []settleNote
	EscalatedAt sql.NullTime
}

// Which §4.7 rule settled it; the note is worded for it.
const (
	duplicateRuleYielded           = 1
	duplicateRuleYieldDone         = 2
	duplicateRuleYieldSessionEnded = 3
	duplicateRuleOtherSessionEnded = 4
	duplicateRuleConverged         = 5
)

// decideDuplicateSettlement is §4.7, and it is pure. It returns the resolution,
// the rule that fired, and false to keep the conflict open.
func decideDuplicateSettlement(st duplicateSettleState, kind DuplicateReleaseKind) (enums.ConflictResolution, int, bool) {
	var (
		res  enums.ConflictResolution
		rule int
	)
	switch {
	case st.A.intentOver() || st.B.intentOver():
		res, rule = enums.CONFLICT_RESOLUTION_YIELDED, duplicateRuleYielded
	case st.B.Status == enums.INTENT_STATUS_DONE:
		res, rule = enums.CONFLICT_RESOLUTION_SUPERSEDED, duplicateRuleYieldDone
	case st.B.sessionOver():
		res, rule = enums.CONFLICT_RESOLUTION_SUPERSEDED, duplicateRuleYieldSessionEnded
	case st.A.sessionOver() && st.A.Status != enums.INTENT_STATUS_DONE:
		res, rule = enums.CONFLICT_RESOLUTION_SUPERSEDED, duplicateRuleOtherSessionEnded
	case st.Latest == enums.JUDGEMENT_VERDICT_NO_CONFLICT:
		res, rule = enums.CONFLICT_RESOLUTION_CONVERGED, duplicateRuleConverged
	default:
		// Including a done while b is still live: b is rebuilding finished
		// work, and escalation may rightly fire.
		return enums.CONFLICT_RESOLUTION_INVALID, 0, false
	}
	if kind != DuplicateReleaseSessionAbandoned {
		for _, n := range st.Notes {
			if n.Agrees {
				return enums.CONFLICT_RESOLUTION_COORDINATED, rule, true
			}
		}
	}
	return res, rule, true
}

// buildDuplicateResolutionNote is §4.10's resolution note. Free text is
// sanitized; it always fits conflict.resolution_note.
func buildDuplicateResolutionNote(st duplicateSettleState, rule int, res enums.ConflictResolution, rel DuplicateRelease) string {
	at := clock(rel.At)
	whenOr := func(t sql.NullTime) string {
		if t.Valid {
			return clock(t.Time)
		}
		return at
	}
	keyOf := func(p duplicatePlanState, fallback string) string { return firstNonEmpty(p.Key, fallback) }
	abandoned := func(p duplicatePlanState) string {
		return fmt.Sprintf("Cleared by metiche: %s was abandoned at %s after no heartbeat, ending %s.",
			p.Owner.name(), whenOr(p.Owner.EndedAt), keyOf(p, "its plan"))
	}
	sessionEnded := func(p duplicatePlanState) string {
		switch {
		case !p.SessionFound:
			k := keyOf(p, "the plan")
			return fmt.Sprintf("Cleared by metiche: the session behind %s no longer exists, ending %s.", k, k)
		case p.Owner.Status == enums.SESSION_STATUS_ABANDONED:
			return abandoned(p)
		default:
			return fmt.Sprintf("Settled by the agents: %s ended its session at %s%s, ending %s.",
				p.Owner.name(), whenOr(p.Owner.EndedAt), outcomeSuffix(p.Owner), keyOf(p, "its plan"))
		}
	}

	var s string
	switch rule {
	case duplicateRuleYielded:
		yielded, other := st.B, st.A
		if !st.B.intentOver() {
			yielded, other = st.A, st.B
		}
		switch {
		case !yielded.Found:
			s = fmt.Sprintf("Cleared by metiche: %s no longer exists, leaving %s (%s) to build it.",
				keyOf(yielded, "the plan"), keyOf(other, "the other plan"), other.Owner.name())
		case rel.Kind == DuplicateReleaseSessionAbandoned && yielded.SessionFound && yielded.Owner.Status == enums.SESSION_STATUS_ABANDONED:
			s = abandoned(yielded)
		default:
			s = fmt.Sprintf("Settled by the agents: %s marked %s %s at %s, leaving %s (%s) to build it.",
				yielded.Owner.name(), yielded.Key, yielded.Status.String(), whenOr(yielded.EndedAt),
				keyOf(other, "the other plan"), other.Owner.name())
		}
	case duplicateRuleYieldDone:
		s = fmt.Sprintf("Settled by the agents: %s finished %s at %s anyway, so the work was duplicated; reconcile the two at merge.",
			st.B.Owner.name(), keyOf(st.B, "its plan"), whenOr(st.B.EndedAt))
	case duplicateRuleYieldSessionEnded:
		s = sessionEnded(st.B)
	case duplicateRuleOtherSessionEnded:
		s = sessionEnded(st.A)
	case duplicateRuleConverged:
		judge := firstNonEmpty(st.JudgeName, st.B.Owner.name())
		quoted := ""
		if r := clip(sanitizeNoteText(st.Rationale), duplicateNoteRationale); r != "" {
			quoted = fmt.Sprintf(": \"%s\"", r)
		}
		s = fmt.Sprintf("Settled by the agents: %s re-scoped %s at %s and judged it no longer duplicates %s%s.",
			judge, firstNonEmpty(st.JudgedKey, keyOf(st.B, "its plan")), at, firstNonEmpty(st.OtherKey, keyOf(st.A, "the other plan")), quoted)
	}

	tail := ""
	if st.EscalatedAt.Valid {
		tail = " A person was asked at " + clock(st.EscalatedAt.Time) + "."
	}
	var b strings.Builder
	b.WriteString(s)
	if res == enums.CONFLICT_RESOLUTION_COORDINATED {
		room := resolutionNoteChars - utf8.RuneCountInString(s) - utf8.RuneCountInString(tail)
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
	body := clip(b.String(), resolutionNoteChars-utf8.RuneCountInString(tail))
	return clip(body+tail, resolutionNoteChars)
}

// ─────────────────────────────────────────────
// Loading the state
// ─────────────────────────────────────────────

func loadDuplicateSettleState(ctx context.Context, q queryer, rel DuplicateRelease, conflictID uuid.UUID, aKey, bKey, yieldSession string) (duplicateSettleState, error) {
	var st duplicateSettleState
	a, err := loadDuplicatePlanState(ctx, q, rel.TeamUUID, aKey)
	if err != nil {
		return st, err
	}
	b, err := loadDuplicatePlanState(ctx, q, rel.TeamUUID, bKey)
	if err != nil {
		return st, err
	}
	// b is the yield side: the plan whose session is suggested_yield_session_uuid.
	if yieldSession != "" && a.Owner.SessionUUID == yieldSession && b.Owner.SessionUUID != yieldSession {
		a, b = b, a
	}
	st.A, st.B = a, b

	if a.Found && b.Found {
		latest, err := latestDuplicateVerdict(ctx, q, rel.TeamUUID, a.ID, b.ID)
		if err != nil {
			return st, err
		}
		if latest.found {
			st.Latest = latest.verdict
			st.Rationale = latest.rationale
			if latest.subjectB == a.ID {
				st.JudgedKey, st.OtherKey = a.Key, b.Key
			} else {
				st.JudgedKey, st.OtherKey = b.Key, a.Key
			}
			if latest.judge != "" {
				side, found, err := loadSettleSide(ctx, q, latest.judge)
				if err != nil {
					return st, err
				}
				if found {
					st.JudgeName = side.name()
				}
			}
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

// loadDuplicatePlanState reads one plan by uq_intent_team_key, and its session.
func loadDuplicatePlanState(ctx context.Context, q queryer, teamUUID uuid.UUID, key string) (duplicatePlanState, error) {
	p := duplicatePlanState{Key: key}
	var (
		status  int64
		session string
	)
	err := q.QueryRowContext(ctx,
		"SELECT `id`, `status`, `ended_at`, `session_uuid` FROM `intent` WHERE `team_uuid` = ? AND `key` = ?",
		teamUUID.String(), key).Scan(&p.ID, &status, &p.EndedAt, &session)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return p, nil
	case err != nil:
		return p, retryable(err, "reading a duplicate conflict's plan")
	}
	p.Found = true
	p.Status = enums.IntentStatus(status)
	side, found, err := loadSettleSide(ctx, q, session)
	if err != nil {
		return p, err
	}
	p.Owner, p.SessionFound = side, found
	return p, nil
}

type duplicateVerdict struct {
	found     bool
	verdict   enums.JudgementVerdict
	judgedAt  time.Time
	revisions int64
	judge     string
	rationale string
	subjectB  string
}

// latestDuplicateVerdict reads the latest judged verdict on the unordered pair,
// in both storage orders. idx_judgement_subject, LIMIT 1 each. A tie on
// judged_at (seconds) goes to the pair at the later wording: wording revisions
// only ever grow.
func latestDuplicateVerdict(ctx context.Context, q queryer, teamUUID uuid.UUID, x, y string) (duplicateVerdict, error) {
	var best duplicateVerdict
	for _, order := range [2][2]string{{x, y}, {y, x}} {
		var (
			v         duplicateVerdict
			verdict   int64
			judge     sql.NullString
			rationale sql.NullString
			judgedAt  sql.NullTime
		)
		err := q.QueryRowContext(ctx,
			"SELECT COALESCE(`verdict`, 0), `judged_at`, `subject_a_revision` + `subject_b_revision`, `judge_session_uuid`, `rationale`, `subject_b_uuid` "+
				"FROM `judgement` WHERE `team_uuid` = ? AND `subject_a_uuid` = ? AND `subject_b_uuid` = ? AND `kind` = ? AND `status` = ? "+
				"ORDER BY `judged_at` DESC, `subject_a_revision` + `subject_b_revision` DESC LIMIT 1",
			teamUUID.String(), order[0], order[1], int64(enums.CONFLICT_KIND_DUPLICATE_WORK), int64(enums.JUDGEMENT_STATUS_JUDGED)).
			Scan(&verdict, &judgedAt, &v.revisions, &judge, &rationale, &v.subjectB)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return best, retryable(err, "reading the latest verdict on the pair")
		}
		v.found, v.verdict = true, enums.JudgementVerdict(verdict)
		v.judgedAt = judgedAt.Time
		v.judge, v.rationale = judge.String, rationale.String
		if !best.found || v.judgedAt.After(best.judgedAt) || (v.judgedAt.Equal(best.judgedAt) && v.revisions > best.revisions) {
			best = v
		}
	}
	return best, nil
}

// ─────────────────────────────────────────────
// The ledger
// ─────────────────────────────────────────────

// duplicateJudgement is one pair to write: a is the other plan, b the judge's.
type duplicateJudgement struct {
	TeamUUID           uuid.UUID
	AUUID, BUUID       string
	AWording, BWording int64
	JudgeSession       string // empty leaves the pair unassigned for the sweeper
	Window             time.Duration
	Now                time.Time
}

// insertDuplicateJudgement writes one pair. The unique index makes the first
// insert win: a pair that already exists is left exactly as it is and
// reported as not inserted.
func insertDuplicateJudgement(ctx context.Context, q queryer, j duplicateJudgement) (string, bool, error) {
	id, err := uuid.NewV4()
	if err != nil {
		return "", false, err
	}
	pairKey := duplicatePairKey(j.AUUID, j.AWording, j.BUUID, j.BWording)
	var (
		judge, expires any
		count          int
	)
	if j.JudgeSession != "" {
		judge, expires, count = j.JudgeSession, j.Now.Add(j.Window), 1
	}
	res, err := q.ExecContext(ctx,
		"INSERT INTO `judgement` (`id`,`team_uuid`,`pair_key`,`kind`,`subject_a_kind`,`subject_a_uuid`,`subject_a_revision`,"+
			"`subject_b_kind`,`subject_b_uuid`,`subject_b_revision`,`status`,`judge_session_uuid`,`judging_expires_at`,"+
			"`assignment_count`,`pinned`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?) "+
			"ON DUPLICATE KEY UPDATE `id` = `id`",
		id.String(), j.TeamUUID.String(), pairKey, int64(enums.CONFLICT_KIND_DUPLICATE_WORK),
		int64(enums.SUBJECT_KIND_INTENT), j.AUUID, j.AWording,
		int64(enums.SUBJECT_KIND_INTENT), j.BUUID, j.BWording,
		int64(enums.JUDGEMENT_STATUS_PENDING), judge, expires, count, j.Now, j.Now)
	if err != nil {
		return pairKey, false, retryable(err, "recording the duplicate pair to judge")
	}
	n, _ := res.RowsAffected()
	return pairKey, n == 1, nil
}

// loadDemotedRules reads team_settings.demoted_rules, by primary key.
func loadDemotedRules(ctx context.Context, q queryer, teamUUID uuid.UUID) (map[string]bool, error) {
	out := map[string]bool{}
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
	for _, r := range team_settings_entity.TeamSettingsFromJSON([]byte(raw.String)).DemotedRules {
		out[strings.TrimSpace(r)] = true
	}
	return out, nil
}

// ─────────────────────────────────────────────
// Raising or reopening (§3.3)
// ─────────────────────────────────────────────

// duplicatePlan is one of the pair's two intents, read under the lock.
type duplicatePlan struct {
	Found           bool
	ID              string
	Key             string
	Summary         string
	Status          enums.IntentStatus
	WordingRevision int64
	Revision        int64
	Session         string
	Member          string
	Project         string
	ExternalRef     string
	DeclaredAt      sql.NullTime
}

// loadDuplicatePlan reads one intent by primary key.
func loadDuplicatePlan(ctx context.Context, q queryer, intentID string) (duplicatePlan, error) {
	p := duplicatePlan{ID: intentID}
	var status int64
	err := q.QueryRowContext(ctx,
		"SELECT `key`, `summary`, `status`, `wording_revision`, `revision`, `session_uuid`, `member_uuid`, `project_uuid`, "+
			"COALESCE(`external_ref`, ''), `declared_at` FROM `intent` WHERE `id` = ?", intentID).
		Scan(&p.Key, &p.Summary, &status, &p.WordingRevision, &p.Revision, &p.Session, &p.Member, &p.Project, &p.ExternalRef, &p.DeclaredAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return p, nil
	case err != nil:
		return p, retryable(err, "re-reading a plan")
	}
	p.Found = true
	p.Status = enums.IntentStatus(status)
	return p, nil
}

func (p duplicatePlan) live() bool {
	return p.Status == enums.INTENT_STATUS_DECLARED || p.Status == enums.INTENT_STATUS_ACTIVE
}

// sameIssue reports two plans on the same external_ref. The column's collation
// is case-insensitive, and so is this.
func sameIssue(a, b duplicatePlan) (string, bool) {
	ra, rb := strings.TrimSpace(a.ExternalRef), strings.TrimSpace(b.ExternalRef)
	if ra == "" || rb == "" || !strings.EqualFold(ra, rb) {
		return "", false
	}
	return rb, true
}

// projectKeyOf reads a project's key by primary key; "" when it is gone.
func projectKeyOf(ctx context.Context, q queryer, projectUUID string) string {
	var key string
	_ = q.QueryRowContext(ctx, "SELECT `key` FROM `project` WHERE `id` = ?", projectUUID).Scan(&key)
	return key
}

// duplicateFacts is what a pair's wording and claims have in common, for the
// why lines and the evidence.
type duplicateFacts struct {
	Issue      string   // the shared external_ref, "" when none
	Words      []string // the judge's own words for the shared keys, at most duplicateWordsShown
	Overlap    string   // the first overlap of the two plans' held write paths
	APattern   string   // a's first held write path
	BPattern   string   // b's first held write path
	SameMember bool
}

// loadDuplicateFacts works out §3.2's why and §3.3's evidence for a pair: b is
// the judge's plan, a the other.
func loadDuplicateFacts(ctx context.Context, q queryer, a, b duplicatePlan, now time.Time) (duplicateFacts, error) {
	var f duplicateFacts
	f.Issue, _ = sameIssue(a, b)
	f.SameMember = a.Member != "" && a.Member == b.Member
	aHeld, err := loadIntentHeldPaths(ctx, q, a.ID, now, settleMaxPathsPerSession)
	if err != nil {
		return f, err
	}
	bHeld, err := loadIntentHeldPaths(ctx, q, b.ID, now, settleMaxPathsPerSession)
	if err != nil {
		return f, err
	}
	f.APattern, f.BPattern = firstWritePath(aHeld), firstWritePath(bHeld)
	f.Overlap = firstPlanOverlap(aHeld, bHeld)

	bTerms := coordination.DuplicateTermsOf(b.Summary, projectKeyOf(ctx, q, b.Project))
	aProject := projectKeyOf(ctx, q, a.Project)
	aTerms := coordination.DuplicateTermsOf(a.Summary, aProject)
	score := coordination.ScoreDuplicate(bTerms, aTerms, nil, f.Overlap != "")
	// The wording matched: a words candidate, or shared words on a pair the
	// issue id did not make.
	if len(score.Shared) > 0 && (score.Candidate || f.Issue == "") {
		for _, k := range score.Shared {
			if len(f.Words) >= duplicateWordsShown {
				break
			}
			f.Words = append(f.Words, firstNonEmpty(bTerms.Words[k], k))
		}
	}
	return f, nil
}

func firstWritePath(held []settlePath) string {
	for _, p := range held {
		if p.Mode != coordination.ModeRead {
			return p.Path.PatternNorm
		}
	}
	return ""
}

// firstPlanOverlap is where two plans' held write paths first meet.
func firstPlanOverlap(a, b []settlePath) string {
	for _, x := range a {
		if x.Mode == coordination.ModeRead {
			continue
		}
		for _, y := range b {
			if y.Mode == coordination.ModeRead {
				continue
			}
			if coordination.PathsOverlap(x.Path, y.Path) {
				return overlapLabel(x.Path, y.Path)
			}
		}
	}
	return ""
}

// raiseDuplicateConflict raises or reopens the conflict for an unsure or
// conflict verdict on a duplicate pair, following raiseDecisionConflict. It
// never writes an instruction and never attaches subject a's session: the
// incumbent is told by the sweeper, after the grace, only if the conflict is
// still standing (§4.5).
func (h *Handler) raiseDuplicateConflict(ctx context.Context, tc *TxContext, m *Mutation, j *judgementReport) ([]ConflictNotice, error) {
	in := j.In
	q := tc.Tx
	a, b := j.DupA, j.DupB
	unsure := in.Verdict == enums.JUDGEMENT_VERDICT_UNSURE

	bOwner, _, err := loadDecisionSide(ctx, q, b.Session)
	if err != nil {
		return nil, err
	}
	aOwner, _, err := loadDecisionSide(ctx, q, a.Session)
	if err != nil {
		return nil, err
	}
	facts, err := loadDuplicateFacts(ctx, q, a, b, tc.Now)
	if err != nil {
		return nil, err
	}

	rule := RuleDuplicateWords
	switch {
	case unsure:
		rule = RuleDuplicateUnsure
	case facts.Issue != "":
		rule = RuleDuplicateSameIssue
	}
	demoted, err := loadDemotedRules(ctx, q, j.TeamUUID)
	if err != nil {
		return nil, err
	}
	conf := 0.0
	if in.Confidence != nil {
		conf = *in.Confidence
	}
	sev := coordination.DuplicateSeverity(coordination.DuplicateSeverityInput{
		Verdict: in.Verdict.String(), Confidence: conf, Requested: in.Severity,
		SameIssue: facts.Issue != "", Demoted: demoted[rule],
	})
	sevEnum := severityEnum(sev)

	// Evidence (§9.4).
	overlapPath := facts.Issue
	if overlapPath == "" {
		overlapPath = facts.Overlap
	}
	adjusters := []string{fmt.Sprintf("plans:%s,%s", a.Key, b.Key)}
	if facts.Issue != "" {
		adjusters = append(adjusters, "same_issue:"+facts.Issue)
	}
	if len(facts.Words) > 0 {
		adjusters = append(adjusters, "words:"+strings.Join(facts.Words, ","))
	}
	if facts.Overlap != "" {
		adjusters = append(adjusters, "paths:"+facts.Overlap)
	}
	if facts.SameMember {
		adjusters = append(adjusters, coordination.PathAdjusterSameMemberConcurrent)
	}
	judged := "(unsure)"
	if in.Confidence != nil {
		judged = fmt.Sprintf("(%.2f)", *in.Confidence)
	}
	evidence, err := json.Marshal(conflict_evidence_entity.ConflictEvidence{
		OverlapPath: nullString(truncate(overlapPath, 255)),
		ALabel:      nullString(truncate(describeHolder(aOwner.claimSide), 255)),
		ASummary:    nullString(a.Summary),
		APattern:    nullString(truncate(facts.APattern, 255)),
		BLabel:      nullString(truncate(describeHolder(bOwner.claimSide), 255)),
		BSummary:    nullString(b.Summary),
		BPattern:    nullString(truncate(facts.BPattern, 255)),
		Adjusters:   adjusters,
		FieldIssues: []string{fmt.Sprintf("%s's model %s: %s", bOwner.SessionKey, judged, in.Rationale)},
		Detail:      nullString(rule),
	})
	if err != nil {
		return nil, err
	}
	evidenceJSON := string(evidence) // string, not []byte: see detector.go

	age := ""
	if a.DeclaredAt.Valid {
		// DATETIME rounds to the second, so a plan declared this second can
		// read as a moment in the future.
		age = humanAge(max(tc.Now.Sub(a.DeclaredAt.Time), 0))
	}
	action := duplicateYieldAction(duplicateActionInput{
		A: a.Key, B: b.Key, Who: describeHolder(aOwner.claimSide), Member: aOwner.MemberName,
		Status: a.Status.String(), Age: age, Summary: a.Summary, ASession: aOwner.SessionKey,
		SameMember: facts.SameMember, Unsure: unsure,
	})
	var confArg any
	if in.Confidence != nil {
		confArg = *in.Confidence
	}
	yieldReason := truncate("same work as "+a.Key, duplicateYieldReasonChars)
	dedupe := DuplicateWorkDedupeKey(a.ID, b.ID)

	var (
		rawID, key string
		status     int64
		resolvedBy sql.NullString
		fresh      bool
		silenced   bool
		conflictID uuid.UUID
	)
	err = q.QueryRowContext(ctx,
		"SELECT `id`, `key`, `status`, `resolved_by_member_uuid` FROM `conflict` WHERE `team_uuid` = ? AND `dedupe_key` = ?",
		j.TeamUUID.String(), dedupe).Scan(&rawID, &key, &status, &resolvedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		conflictID, err = uuid.NewV4()
		if err != nil {
			return nil, err
		}
		key, fresh = conflictKey(tc, 0), true
		if _, err := q.ExecContext(ctx,
			"INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
				"`detector_rule`,`confidence`,`evidence`,`suggested_action`,`suggested_yield_session_uuid`,`suggested_yield_reason`,`occurrence_count`,"+
				"`first_detected_at`,`last_detected_at`,`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)",
			conflictID.String(), j.TeamUUID.String(), b.Project, key, int64(enums.CONFLICT_KIND_DUPLICATE_WORK), dedupe,
			int64(sevEnum), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.DETECTED_BY_AGENT), rule, confArg, evidenceJSON, action,
			b.Session, yieldReason, tc.Now, tc.Now, tc.Now, tc.Now); err != nil {
			return nil, retryable(err, "recording the duplicate conflict")
		}
	case err != nil:
		return nil, retryable(err, "looking up the duplicate conflict")
	default:
		conflictID, err = uuid.FromString(rawID)
		if err != nil {
			return nil, err
		}
		st := enums.ConflictStatus(status)
		switch {
		case st == enums.CONFLICT_STATUS_OPEN || st == enums.CONFLICT_STATUS_ACKNOWLEDGED || st == enums.CONFLICT_STATUS_RESOLVING:
			// The side that changed last is responsible.
			if _, err := q.ExecContext(ctx,
				"UPDATE `conflict` SET `occurrence_count` = `occurrence_count` + 1, `last_detected_at` = ?, `severity` = ?, `evidence` = ?, "+
					"`suggested_action` = ?, `detector_rule` = ?, `confidence` = ?, `suggested_yield_session_uuid` = ?, "+
					"`suggested_yield_reason` = ?, `updated_at` = ? WHERE `id` = ?",
				tc.Now, int64(sevEnum), evidenceJSON, action, rule, confArg, b.Session, yieldReason, tc.Now, rawID); err != nil {
				return nil, retryable(err, "updating the duplicate conflict")
			}
		case st == enums.CONFLICT_STATUS_RESOLVED && !resolvedBy.Valid:
			// metiche closed it and the agents say it is the same work again:
			// the same conflict reopens, and it is news again.
			fresh = true
			if _, err := q.ExecContext(ctx,
				"UPDATE `conflict` SET `status` = ?, `resolution` = NULL, `resolution_note` = NULL, `resolved_at` = NULL, "+
					"`notified_at` = NULL, `max_severity_notified` = NULL, `escalated_at` = NULL, `occurrence_count` = `occurrence_count` + 1, "+
					"`last_detected_at` = ?, `severity` = ?, `evidence` = ?, `suggested_action` = ?, `detector_rule` = ?, `confidence` = ?, "+
					"`suggested_yield_session_uuid` = ?, `suggested_yield_reason` = ?, `updated_at` = ? WHERE `id` = ?",
				int64(enums.CONFLICT_STATUS_OPEN), tc.Now, int64(sevEnum), evidenceJSON, action, rule, confArg,
				b.Session, yieldReason, tc.Now, rawID); err != nil {
				return nil, retryable(err, "reopening the duplicate conflict")
			}
		default:
			// A person resolved or dismissed it: count it, never shout again.
			silenced = true
			if _, err := q.ExecContext(ctx,
				"UPDATE `conflict` SET `occurrence_count` = `occurrence_count` + 1, `last_detected_at` = ?, `updated_at` = ? WHERE `id` = ?",
				tc.Now, tc.Now, rawID); err != nil {
				return nil, retryable(err, "counting the duplicate conflict")
			}
		}
	}

	if _, err := q.ExecContext(ctx, "UPDATE `judgement` SET `conflict_uuid` = ? WHERE `id` = ?", conflictID.String(), j.Row.ID); err != nil {
		return nil, retryable(err, "linking the verdict to its conflict")
	}
	m.Payload.ConflictUUID = &conflictID
	m.Payload.Severity = sevEnum

	verdictPhrase := "unsure"
	if !unsure {
		verdictPhrase = "same work" + confidencePart(in.Confidence)
	}
	if silenced {
		m.Envelope.Note = fmt.Sprintf("judged %s against %s: %s; a person already settled this one, so it stays closed", b.Key, a.Key, verdictPhrase)
		return nil, nil
	}
	m.Envelope.Key = key
	if fresh {
		// Raised or reopened: the conflict list changes shape.
		m.Structural = true
		tc.Revision++
	}

	// The judge's own plan only. Subject a's session is attached when it is
	// told, because attaching it is itself an interruption (F10).
	if bOwner.Found {
		if err := upsertDecisionParticipant(ctx, tc, j.TeamUUID, conflictID, bOwner.claimSide, enums.SUBJECT_KIND_INTENT, b.ID, enums.PARTICIPANT_ROLE_INITIATOR); err != nil {
			return nil, err
		}
	}

	notify := sev >= notifySeverityFloor && (unsure || conf >= coordination.JudgeMinConfidence)
	if unsure || !notify {
		switch {
		case unsure:
			m.Envelope.Note = fmt.Sprintf("judged %s against %s: unsure, recorded low on the board", b.Key, a.Key)
		case conf < coordination.JudgeMinConfidence:
			m.Envelope.Note = fmt.Sprintf("judged %s against %s: same work%s, recorded low on the board; below 0.7 it interrupts nobody",
				b.Key, a.Key, confidencePart(in.Confidence))
		default:
			m.Envelope.Note = fmt.Sprintf("judged %s against %s: same work%s, recorded low on the board", b.Key, a.Key, confidencePart(in.Confidence))
		}
		return nil, nil
	}

	if _, err := q.ExecContext(ctx,
		"UPDATE `conflict_participant` SET `notified_at` = COALESCE(`notified_at`, ?), `updated_at` = ? WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
		tc.Now, tc.Now, conflictID.String(), j.SessionUUID.String()); err != nil {
		return nil, retryable(err, "marking the caller notified of the conflict")
	}
	m.Envelope.Note = fmt.Sprintf("judged %s against %s: same work%s — read conflicts[] before you edit", b.Key, a.Key, confidencePart(in.Confidence))
	return []ConflictNotice{{
		Key:             key,
		Kind:            enums.ConflictKind(enums.CONFLICT_KIND_DUPLICATE_WORK).String(),
		Severity:        sev.String(),
		With:            describeHolder(aOwner.claimSide),
		DuplicateOf:     a.Key,
		AtFault:         "later",
		SuggestedAction: action,
	}}, nil
}

// ─────────────────────────────────────────────
// Wording (§4.10)
// ─────────────────────────────────────────────

// duplicateActionInput is the suggested action's material.
type duplicateActionInput struct {
	A, B       string // intent keys: the other plan, the yield side's
	Who        string // describeHolder of a's owner
	Member     string // a's member display name
	Status     string // declared | active
	Age        string // humanAge since a was declared
	Summary    string // a's summary
	ASession   string // a's session key
	SameMember bool
	Unsure     bool
}

// duplicateYieldAction is the suggested action for the yield side, ≤ 400.
func duplicateYieldAction(in duplicateActionInput) string {
	summary := clip(sanitizeNoteText(in.Summary), duplicateSummaryActionChars)
	member := firstNonEmpty(in.Member, in.Who, "its owner")
	session := firstNonEmpty(in.ASession, "its session")
	statusAge := firstNonEmpty(in.Status, "declared")
	if in.Age != "" {
		statusAge += " " + in.Age
	}
	var s string
	switch {
	case in.Unsure:
		s = fmt.Sprintf("%s may overlap %s (%s): \"%s\". Nothing to do now; if it matters, ask %s's agent (%s) which part each of you takes, then update_intent.",
			in.B, in.A, in.Who, summary, member, session)
	case in.SameMember:
		s = fmt.Sprintf("%s (%s, your own person's other agent, %s) is already building this: \"%s\". Stop before you edit: mark %s superseded, or re-scope it and update_intent the summary. If yours should continue, settle it with %s first.",
			in.A, session, statusAge, summary, in.B, session)
	default:
		s = fmt.Sprintf("%s (%s, %s) is already building this: \"%s\". Stop before you edit: mark %s superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with %s's agent (%s) first.",
			in.A, in.Who, statusAge, summary, in.B, member, session)
	}
	return clip(s, resolutionNoteChars)
}

// DuplicateIncumbentAction is what the incumbent's agent reads after
// "CF-44 (medium) on INT-83: ", within the 200 characters get_instructions
// shows. The quoted rationale gives way first, at most 60; the path conflict
// it replaces is named only when it fits.
func DuplicateIncumbentAction(judgeName, yieldKey, rationale, yieldSession, pathConflictKey string) string {
	judge := firstNonEmpty(judgeName, "another agent")
	yield := firstNonEmpty(yieldKey, "its plan")
	session := firstNonEmpty(yieldSession, "its session")
	const quotedShape = "%s judged its plan %s is the same work as yours: \"%s\", and was told to stop. Carry on; if its part should stay, settle the split with %s."
	const bareShape = "%s judged its plan %s is the same work as yours, and was told to stop. Carry on; if its part should stay, settle the split with %s."
	build := func(suffix string) (string, bool) {
		r := sanitizeNoteText(rationale)
		if strings.TrimSpace(r) == "" {
			s := fmt.Sprintf(bareShape, judge, yield, session) + suffix
			return s, utf8.RuneCountInString(s) <= duplicateNoticeActionChars
		}
		room := duplicateNoticeActionChars - utf8.RuneCountInString(fmt.Sprintf(quotedShape, judge, yield, "", session)+suffix)
		if room > duplicateQuoteChars {
			room = duplicateQuoteChars
		}
		if room < 1 {
			return fmt.Sprintf(quotedShape, judge, yield, "", session) + suffix, false
		}
		s := fmt.Sprintf(quotedShape, judge, yield, clip(r, room), session) + suffix
		return s, utf8.RuneCountInString(s) <= duplicateNoticeActionChars
	}
	if pathConflictKey != "" {
		if s, ok := build(" (also " + pathConflictKey + ")"); ok {
			return s
		}
	}
	s, _ := build("")
	return clip(s, duplicateNoticeActionChars)
}

// DuplicateEscalationQuestions are §4.10's question bodies for the yield side and the
// incumbent, each ≤ 200.
func DuplicateEscalationQuestions(conflictKey, yieldKey, incumbentKey, yieldMember, incumbentMember, incumbentSummary, budget string) (yieldSide, incumbent string) {
	other := firstNonEmpty(incumbentMember, "the other agent's person")
	member := firstNonEmpty(yieldMember, "another person")
	yieldSide = fmt.Sprintf("%s: %s duplicates %s (%s), unsettled %s. Ask your person: stop %s, or agree with %s who keeps it? Then report_back their answer.",
		conflictKey, yieldKey, incumbentKey, other, budget, yieldKey, other)
	incumbent = fmt.Sprintf("%s: %s's agent is building the same thing as your %s, unsettled %s. Ask your person who keeps it: \"%s\". Then report_back their answer.",
		conflictKey, member, incumbentKey, budget, clip(sanitizeNoteText(incumbentSummary), duplicateSummaryQuestionChars))
	return clip(yieldSide, instructionTextChars), clip(incumbent, instructionTextChars)
}
