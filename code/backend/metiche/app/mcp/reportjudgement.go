package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/app/coordination"
	conflict_evidence_entity "github.com/mklfarha/metiche/backend/entity/conflict_evidence"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// reportjudgement.go is PLAN.md's tool 11, report_judgement (docs/DECISIONS.md
// §3.3): the plan's own agent answers a pair — would its plan, as written,
// break the recorded decision?
//
// The verdict is recorded in Apply; what it means is decided in Detect,
// dispatched on the judgement's kind (decision_contradiction today, duplicate
// work later): a no_conflict settles a standing conflict when the pair is the
// current one, an unsure is recorded low, and a conflict is raised or reopened
// with the decider's agent told when it is worth interrupting.

type ReportJudgementParams struct {
	SessionKey string   `json:"session_key" jsonschema:"The session_key start_session gave you."`
	TeamSlug   string   `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	PairKey    string   `json:"pair_key" jsonschema:"The pair_key from a review block or get_review_context, exactly as given. metiche computed it; never make one up."`
	Verdict    string   `json:"verdict" jsonschema:"'conflict' if carrying out your plan as written would break the decision; 'no_conflict' if it would not (the usual answer: unrelated or compatible); 'unsure' if the decision's wording does not settle it."`
	Confidence *float64 `json:"confidence,omitempty" jsonschema:"How sure you are, 0 to 1. Be honest: a conflict below 0.7 is recorded on the board but interrupts nobody. Required for conflict."`
	Severity   string   `json:"severity,omitempty" jsonschema:"For a conflict: 'low', 'medium' (default) or 'high', meaning how much breaks if the plan goes ahead. Ignored otherwise."`
	Rationale  string   `json:"rationale,omitempty" jsonschema:"One line naming what in your plan meets what in the decision: 'plan stores the token in localStorage; #auth-jwt-cookie forbids it'. Max 400 characters. Required for conflict and unsure. Shown on the board; no secrets."`
}

// judgementInput is a validated verdict.
type judgementInput struct {
	PairKey    string
	Verdict    enums.JudgementVerdict
	Confidence *float64
	Severity   coordination.Severity
	Rationale  string
}

// parseReportJudgement validates a verdict. Pure.
func parseReportJudgement(args ReportJudgementParams) (judgementInput, error) {
	var in judgementInput
	in.PairKey = strings.TrimSpace(args.PairKey)
	if in.PairKey == "" {
		return in, errors.New("pair_key is required — send back the pair_key a review block or get_review_context gave you")
	}
	switch strings.ToLower(strings.TrimSpace(args.Verdict)) {
	case "conflict":
		in.Verdict = enums.JUDGEMENT_VERDICT_CONFLICT
	case "no_conflict":
		in.Verdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT
	case "unsure":
		in.Verdict = enums.JUDGEMENT_VERDICT_UNSURE
	default:
		return in, fmt.Errorf("verdict must be conflict, no_conflict or unsure (got %q)", args.Verdict)
	}
	if in.Verdict == enums.JUDGEMENT_VERDICT_CONFLICT && args.Confidence == nil {
		return in, errors.New("confidence is required with a conflict verdict — how sure you are, 0 to 1")
	}
	if args.Confidence != nil {
		c := *args.Confidence
		if math.IsNaN(c) || c < 0 || c > 1 {
			return in, fmt.Errorf("confidence must be between 0 and 1 (got %v)", c)
		}
		in.Confidence = &c
	}
	if in.Verdict == enums.JUDGEMENT_VERDICT_CONFLICT {
		switch strings.ToLower(strings.TrimSpace(args.Severity)) {
		case "", "medium":
			in.Severity = coordination.SeverityMedium
		case "low":
			in.Severity = coordination.SeverityLow
		case "high":
			in.Severity = coordination.SeverityHigh
		default:
			return in, fmt.Errorf("severity must be low, medium or high (got %q)", args.Severity)
		}
	}
	rationale := strings.TrimSpace(args.Rationale)
	if rationale == "" && in.Verdict != enums.JUDGEMENT_VERDICT_NO_CONFLICT {
		return in, fmt.Errorf("rationale is required with a %s verdict — one line naming what in your plan meets what in the decision", in.Verdict.String())
	}
	in.Rationale = clip(sanitizeNoteText(rationale), judgeRationaleChars)
	return in, nil
}

// judgementReport travels from the tool through Apply to Detect.
type judgementReport struct {
	In          judgementInput
	TeamUUID    uuid.UUID
	SessionUUID uuid.UUID
	AgentLabel  string
	Row         judgementRow

	// Filled by Apply.
	DecisionKey        string
	DecisionStatement  string
	DecisionAlwaysShow bool
	DecidedBy          string
	RecordedBy         string
	IntentKey          string
	IntentSummary      string
	IntentSession      string
	IntentMember       string
	IntentProject      string
}

// ReportJudgement records a verdict on one pair.
func (h *Handler) ReportJudgement(ctx context.Context, _ *mcp.CallToolRequest, args ReportJudgementParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSessionOnTeam(ctx, args.SessionKey, args.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session
	if err := requireWorkableSession(sess.Status); err != nil {
		return nil, nil, err
	}
	in, err := parseReportJudgement(args)
	if err != nil {
		return nil, nil, err
	}
	row, found, err := loadJudgementByPairKey(ctx, h.core.DB(), who.Team.ID, in.PairKey)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, fmt.Errorf("pair_key %s is not a pair on this team — call get_review_context", in.PairKey)
	}
	if row.Judge != sess.ID.String() {
		return nil, nil, fmt.Errorf("this pair is %s's to judge, not yours", judgeName(ctx, h.core.DB(), row))
	}

	decisionID, err := uuid.FromString(row.DecisionID)
	if err != nil {
		return nil, nil, err
	}
	intentID, _ := uuid.FromString(row.IntentID)
	j := &judgementReport{In: in, TeamUUID: who.Team.ID, SessionUUID: sess.ID, AgentLabel: ag.Label, Row: row}

	response, err := h.commit(ctx, Mutation{
		TeamUUID: who.Team.ID,
		// The judgement and the verdict: the same verdict again replays the
		// first answer, whatever its confidence or rationale says this time.
		IdempotencyKey: "judgement_reported:" + row.ID + ":" + in.Verdict.String(),
		Kind:           enums.EVENT_KIND_JUDGEMENT_REPORTED,
		Structural:     false,
		ProjectUUID:    uuidPtr(sess.ProjectUUID),
		SessionUUID:    uuidPtr(sess.ID),
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(who.Member.ID),
		SubjectKind:    enums.SUBJECT_KIND_DECISION,
		SubjectUUID:    uuidPtr(decisionID),
		Payload:        payload_entity.EventPayload{IntentUUID: uuidPtr(intentID)},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			return applyReportJudgement(ctx, tc, env, j)
		},
		Detect: func(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
			return h.detectReportJudgement(ctx, tc, m, j)
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(response)
}

func applyReportJudgement(ctx context.Context, tc *TxContext, env *Envelope, j *judgementReport) error {
	var status int64
	if err := tc.Tx.QueryRowContext(ctx, "SELECT `status` FROM `session` WHERE `id` = ?", j.SessionUUID.String()).Scan(&status); err != nil {
		return retryable(err, "re-reading the session")
	}
	if err := requireWorkableSession(enums.SessionStatus(status)); err != nil {
		return err
	}

	row, err := scanJudgement(tc.Tx.QueryRowContext(ctx,
		"SELECT "+judgementColumns+" FROM `judgement` WHERE `id` = ?", j.Row.ID).Scan)
	if err != nil {
		return retryable(err, "re-reading the pair")
	}
	// The judge was checked before the lock (§3.3). A pair is only ever
	// assigned, or re-armed, to its plan's own session, so it cannot change
	// hands in between.
	switch row.Status {
	case enums.JUDGEMENT_STATUS_PENDING:
	case enums.JUDGEMENT_STATUS_JUDGED:
		at := ""
		if row.JudgedAt.Valid {
			at = clock(row.JudgedAt.Time)
		}
		return fmt.Errorf("already judged %s at %s; a changed plan earns a new pair when you update_intent", verdictWords(row.Verdict), at)
	default:
		return errors.New("this pair expired unanswered; call get_review_context for what is waiting now")
	}
	j.Row = row

	var (
		dStatus, dRev int64
		decidedBy     string
	)
	err = tc.Tx.QueryRowContext(ctx,
		"SELECT `key`, `status`, `revision`, `statement`, `always_show`, COALESCE(`decided_by_member_uuid`, ''), COALESCE(`recorded_by_session_uuid`, '') "+
			"FROM `decision` WHERE `id` = ?", row.DecisionID).
		Scan(&j.DecisionKey, &dStatus, &dRev, &j.DecisionStatement, &j.DecisionAlwaysShow, &decidedBy, &j.RecordedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errors.New("this pair's decision no longer exists; nothing to judge")
	case err != nil:
		return retryable(err, "re-reading the decision")
	}
	j.DecidedBy = decidedBy
	if st := enums.DecisionStatus(dStatus); st != enums.DECISION_STATUS_ACCEPTED {
		return fmt.Errorf("%s was %s; nothing to judge", j.DecisionKey, st.String())
	}
	if dRev != row.DecisionRev {
		return fmt.Errorf("%s is now revision %d and you judged revision %d: call get_review_context for the current pair", j.DecisionKey, dRev, row.DecisionRev)
	}

	var iStatus, iRev int64
	err = tc.Tx.QueryRowContext(ctx,
		"SELECT `key`, `status`, `revision`, `summary`, `session_uuid`, `member_uuid`, `project_uuid` FROM `intent` WHERE `id` = ?", row.IntentID).
		Scan(&j.IntentKey, &iStatus, &iRev, &j.IntentSummary, &j.IntentSession, &j.IntentMember, &j.IntentProject)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errors.New("this pair's plan no longer exists; nothing to judge")
	case err != nil:
		return retryable(err, "re-reading the plan")
	}
	if st := enums.IntentStatus(iStatus); st != enums.INTENT_STATUS_DECLARED && st != enums.INTENT_STATUS_ACTIVE {
		return fmt.Errorf("%s is %s; nothing to judge", j.IntentKey, st.String())
	}
	if iRev != row.IntentRev {
		return fmt.Errorf("%s is now revision %d and you judged revision %d: call get_review_context for the current pair", j.IntentKey, iRev, row.IntentRev)
	}

	in := j.In
	var severity, confidence, rationale any
	if in.Verdict == enums.JUDGEMENT_VERDICT_CONFLICT {
		severity = int64(severityEnum(in.Severity))
	}
	if in.Confidence != nil {
		confidence = *in.Confidence
	}
	if in.Rationale != "" {
		rationale = in.Rationale
	}
	res, err := tc.Tx.ExecContext(ctx,
		"UPDATE `judgement` SET `status` = ?, `verdict` = ?, `severity` = ?, `confidence` = ?, `rationale` = ?, "+
			"`judged_at` = ?, `updated_at` = ? WHERE `id` = ? AND `status` = ?",
		int64(enums.JUDGEMENT_STATUS_JUDGED), int64(in.Verdict), severity, confidence, rationale,
		tc.Now, tc.Now, row.ID, int64(enums.JUDGEMENT_STATUS_PENDING))
	if err != nil {
		return retryable(err, "recording the verdict")
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("this pair was answered by another call; call get_review_context for what is waiting now")
	}
	env.Key = j.IntentKey
	return nil
}

// ─────────────────────────────────────────────
// Detect, dispatched on the judgement's kind
// ─────────────────────────────────────────────

func (h *Handler) detectReportJudgement(ctx context.Context, tc *TxContext, m *Mutation, j *judgementReport) ([]ConflictNotice, error) {
	in := j.In
	m.SubjectKey = truncate(j.DecisionKey, 64)
	m.Summary = fmt.Sprintf("%s judged %s against %s: %s", j.AgentLabel, j.IntentKey, j.DecisionKey, verdictWords(in.Verdict))
	m.Payload.Detail = nullString(in.Verdict.String())
	m.Payload.Message = nullString(in.Rationale)
	m.Payload.Paths = []string{j.DecisionKey}

	switch j.Row.Kind {
	case enums.CONFLICT_KIND_DECISION_CONTRADICTION:
		return h.detectDecisionVerdict(ctx, tc, m, j)
	}
	return nil, fmt.Errorf("pairs of kind %s cannot be judged yet", j.Row.Kind.String())
}

func confidencePart(c *float64) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf(" (%.2f)", *c)
}

func (h *Handler) detectDecisionVerdict(ctx context.Context, tc *TxContext, m *Mutation, j *judgementReport) ([]ConflictNotice, error) {
	in := j.In
	dedupe := DecisionContradictionDedupeKey(j.Row.DecisionID, j.Row.IntentID)
	actor := eventActor{SessionUUID: j.SessionUUID}
	if m.ProjectUUID != nil {
		actor.ProjectUUID = *m.ProjectUUID
	}
	if m.AgentUUID != nil {
		actor.AgentUUID = *m.AgentUUID
	}
	if m.MemberUUID != nil {
		actor.MemberUUID = *m.MemberUUID
	}

	if in.Verdict == enums.JUDGEMENT_VERDICT_NO_CONFLICT {
		m.Envelope.Note = fmt.Sprintf("judged %s against %s: no conflict%s", j.IntentKey, j.DecisionKey, confidencePart(in.Confidence))
		var (
			id, key string
			status  int64
		)
		err := tc.Tx.QueryRowContext(ctx,
			"SELECT `id`, `key`, `status` FROM `conflict` WHERE `team_uuid` = ? AND `dedupe_key` = ?",
			j.TeamUUID.String(), dedupe).Scan(&id, &key, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil
		case err != nil:
			return nil, retryable(err, "looking up the conflict on this pair")
		}
		st := enums.ConflictStatus(status)
		if st != enums.CONFLICT_STATUS_OPEN && st != enums.CONFLICT_STATUS_ACKNOWLEDGED {
			return nil, nil
		}
		cid, err := uuid.FromString(id)
		if err != nil {
			return nil, err
		}
		settled, err := h.settleDecisionConflicts(ctx, tc, DecisionRelease{
			TeamUUID: j.TeamUUID, SessionUUID: j.SessionUUID, Kind: DecisionReleaseJudged, At: tc.Now,
		}, []uuid.UUID{cid}, actor)
		if err != nil {
			return nil, err
		}
		if len(settled) > 0 {
			m.Envelope.Key = key
			m.Envelope.Note += fmt.Sprintf("; %s settled", key)
			m.Payload.ConflictUUID = &cid
		}
		return nil, nil
	}

	return h.raiseDecisionConflict(ctx, tc, m, j, dedupe)
}

// decisionSide is one participant of a decision conflict.
type decisionSide struct {
	claimSide
	Found bool
}

// loadDecisionSide reads a session with its agent and member names, by
// primary key, and reports whether it is live or stale.
func loadDecisionSide(ctx context.Context, q queryer, sessionUUID string) (decisionSide, bool, error) {
	var (
		s      decisionSide
		status int64
	)
	err := q.QueryRowContext(ctx,
		"SELECT s.`id`, s.`key`, s.`agent_uuid`, s.`member_uuid`, COALESCE(a.`label`, ''), COALESCE(m.`display_name`, ''), s.`status` "+
			"FROM `session` s LEFT JOIN `agent` a ON a.`id` = s.`agent_uuid` LEFT JOIN `member` m ON m.`id` = s.`member_uuid` WHERE s.`id` = ?",
		sessionUUID).Scan(&s.SessionUUID, &s.SessionKey, &s.AgentUUID, &s.MemberUUID, &s.AgentLabel, &s.MemberName, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s, false, nil
	case err != nil:
		return s, false, retryable(err, "reading a session")
	}
	s.Found = true
	st := enums.SessionStatus(status)
	return s, st == enums.SESSION_STATUS_LIVE || st == enums.SESSION_STATUS_STALE, nil
}

// deciderSide is §3.3's decider participant: the recorder session while it is
// live or stale and the decider's, otherwise the decider member's most recent
// live or stale session on the team. Nil when there is none.
func deciderSide(ctx context.Context, q queryer, teamUUID uuid.UUID, deciderMember, recorderSession string) (*decisionSide, error) {
	if deciderMember == "" {
		return nil, nil
	}
	if recorderSession != "" {
		s, live, err := loadDecisionSide(ctx, q, recorderSession)
		if err != nil {
			return nil, err
		}
		if live && s.MemberUUID == deciderMember {
			return &s, nil
		}
	}
	var id string
	err := q.QueryRowContext(ctx,
		"SELECT `id` FROM `session` WHERE `team_uuid` = ? AND `status` IN (?, ?) AND `member_uuid` = ? "+
			"ORDER BY `last_heartbeat_at` DESC LIMIT 1",
		teamUUID.String(), int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE), deciderMember).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, retryable(err, "looking up the decider's session")
	}
	s, live, err := loadDecisionSide(ctx, q, id)
	if err != nil || !live {
		return nil, err
	}
	return &s, nil
}

// raiseDecisionConflict raises or reopens the conflict for an unsure or
// conflict verdict, following recordContractConflict.
func (h *Handler) raiseDecisionConflict(ctx context.Context, tc *TxContext, m *Mutation, j *judgementReport, dedupe string) ([]ConflictNotice, error) {
	in := j.In
	q := tc.Tx
	unsure := in.Verdict == enums.JUDGEMENT_VERDICT_UNSURE

	owner, _, err := loadDecisionSide(ctx, q, j.IntentSession)
	if err != nil {
		return nil, err
	}
	sameMember := j.DecidedBy != "" && j.DecidedBy == j.IntentMember
	conf := 0.0
	if in.Confidence != nil {
		conf = *in.Confidence
	}
	sev := coordination.JudgedSeverity(coordination.JudgedSeverityInput{
		Verdict: in.Verdict.String(), Confidence: conf, Requested: in.Severity,
		SameMember: sameMember, AlwaysShow: j.DecisionAlwaysShow,
	})
	rule := RuleDecisionJudged
	if unsure {
		rule = RuleDecisionUnsure
	}

	deciderName := ""
	if j.DecidedBy != "" {
		_ = q.QueryRowContext(ctx, "SELECT `display_name` FROM `member` WHERE `id` = ?", j.DecidedBy).Scan(&deciderName)
	}
	var decider *decisionSide
	if !sameMember {
		decider, err = deciderSide(ctx, q, j.TeamUUID, j.DecidedBy, j.RecordedBy)
		if err != nil {
			return nil, err
		}
	}

	// Evidence: the paths the two sides meet on, when they do.
	decisionID, _ := uuid.FromString(j.Row.DecisionID)
	scope, err := loadDecisionScope(ctx, q, decisionID)
	if err != nil {
		return nil, err
	}
	var caseInsensitive bool
	_ = q.QueryRowContext(ctx, "SELECT `case_insensitive_paths` FROM `project` WHERE `id` = ?", j.IntentProject).Scan(&caseInsensitive)
	held, err := loadIntentHeldPaths(ctx, q, j.Row.IntentID, tc.Now, settleMaxPathsPerSession)
	if err != nil {
		return nil, err
	}
	aPattern, bPattern, ok := firstScopeOverlap(scope, held, caseInsensitive)
	if !ok {
		aPattern, bPattern = "", ""
		if len(scope) > 0 {
			aPattern = scope[0]
		}
		if len(held) > 0 {
			bPattern = held[0].Path.PatternNorm
		}
	}
	aLabel := "decision: " + j.DecisionKey
	if deciderName != "" {
		aLabel = fmt.Sprintf("decision: %s (%s)", j.DecisionKey, deciderName)
	}
	judged := "(unsure)"
	if in.Confidence != nil {
		judged = fmt.Sprintf("(%.2f)", *in.Confidence)
	}
	evidence, err := json.Marshal(conflict_evidence_entity.ConflictEvidence{
		OverlapPath: nullString(truncate(j.DecisionKey, 255)),
		ALabel:      nullString(truncate(aLabel, 255)),
		ASummary:    nullString(j.DecisionStatement),
		APattern:    nullString(truncate(aPattern, 255)),
		BLabel:      nullString(truncate(describeHolder(owner.claimSide), 255)),
		BSummary:    nullString(j.IntentSummary),
		BPattern:    nullString(truncate(bPattern, 255)),
		FieldIssues: []string{fmt.Sprintf("%s's model %s: %s", owner.SessionKey, judged, in.Rationale)},
		Detail:      nullString(rule),
	})
	if err != nil {
		return nil, err
	}
	evidenceJSON := string(evidence) // string, not []byte: see detector.go

	deciderKey := ""
	if decider != nil {
		deciderKey = decider.SessionKey
	}
	action := planOwnerAction(j.IntentKey, j.DecisionKey, j.DecisionStatement, deciderName, deciderKey, sameMember, unsure)
	sevEnum := severityEnum(sev)
	var confArg any
	if in.Confidence != nil {
		confArg = *in.Confidence
	}
	yieldReason := truncate("plan contradicts "+j.DecisionKey, 80)

	var (
		rawID, key string
		status     int64
		resolvedBy sql.NullString
		maxNotif   sql.NullInt64
		fresh      bool
		silenced   bool
		conflictID uuid.UUID
	)
	err = q.QueryRowContext(ctx,
		"SELECT `id`, `key`, `status`, `resolved_by_member_uuid`, `max_severity_notified` FROM `conflict` WHERE `team_uuid` = ? AND `dedupe_key` = ?",
		j.TeamUUID.String(), dedupe).Scan(&rawID, &key, &status, &resolvedBy, &maxNotif)
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
			conflictID.String(), j.TeamUUID.String(), j.IntentProject, key, int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION), dedupe,
			int64(sevEnum), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.DETECTED_BY_AGENT), rule, confArg, evidenceJSON, action,
			j.IntentSession, yieldReason, tc.Now, tc.Now, tc.Now, tc.Now); err != nil {
			return nil, retryable(err, "recording the decision conflict")
		}
	case err != nil:
		return nil, retryable(err, "looking up the decision conflict")
	default:
		conflictID, err = uuid.FromString(rawID)
		if err != nil {
			return nil, err
		}
		st := enums.ConflictStatus(status)
		switch {
		case st == enums.CONFLICT_STATUS_OPEN || st == enums.CONFLICT_STATUS_ACKNOWLEDGED || st == enums.CONFLICT_STATUS_RESOLVING:
			if _, err := q.ExecContext(ctx,
				"UPDATE `conflict` SET `occurrence_count` = `occurrence_count` + 1, `last_detected_at` = ?, `severity` = ?, `evidence` = ?, "+
					"`suggested_action` = ?, `detector_rule` = ?, `confidence` = ?, `updated_at` = ? WHERE `id` = ?",
				tc.Now, int64(sevEnum), evidenceJSON, action, rule, confArg, tc.Now, rawID); err != nil {
				return nil, retryable(err, "updating the decision conflict")
			}
		case st == enums.CONFLICT_STATUS_RESOLVED && !resolvedBy.Valid:
			// metiche closed it and the agents disagree again: the same
			// conflict reopens, and it is news again — including to a person.
			fresh = true
			maxNotif = sql.NullInt64{}
			if _, err := q.ExecContext(ctx,
				"UPDATE `conflict` SET `status` = ?, `resolution` = NULL, `resolution_note` = NULL, `resolved_at` = NULL, "+
					"`notified_at` = NULL, `max_severity_notified` = NULL, `escalated_at` = NULL, `occurrence_count` = `occurrence_count` + 1, "+
					"`last_detected_at` = ?, `severity` = ?, `evidence` = ?, `suggested_action` = ?, `detector_rule` = ?, `confidence` = ?, "+
					"`updated_at` = ? WHERE `id` = ?",
				int64(enums.CONFLICT_STATUS_OPEN), tc.Now, int64(sevEnum), evidenceJSON, action, rule, confArg, tc.Now, rawID); err != nil {
				return nil, retryable(err, "reopening the decision conflict")
			}
		default:
			// A person resolved or dismissed it: count it, never shout again.
			silenced = true
			if _, err := q.ExecContext(ctx,
				"UPDATE `conflict` SET `occurrence_count` = `occurrence_count` + 1, `last_detected_at` = ?, `updated_at` = ? WHERE `id` = ?",
				tc.Now, tc.Now, rawID); err != nil {
				return nil, retryable(err, "counting the decision conflict")
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
		verdictPhrase = "conflict" + confidencePart(in.Confidence)
	}
	if silenced {
		m.Envelope.Note = fmt.Sprintf("judged %s against %s: %s; a person already settled this one, so it stays closed", j.IntentKey, j.DecisionKey, verdictPhrase)
		return nil, nil
	}
	m.Envelope.Key = key
	if fresh {
		// Raised or reopened: the conflict list changes shape.
		m.Structural = true
		tc.Revision++
	}

	if owner.Found {
		if err := upsertDecisionParticipant(ctx, tc, j.TeamUUID, conflictID, owner.claimSide, enums.SUBJECT_KIND_INTENT, j.Row.IntentID, enums.PARTICIPANT_ROLE_INITIATOR); err != nil {
			return nil, err
		}
	}
	if decider != nil {
		if err := upsertDecisionParticipant(ctx, tc, j.TeamUUID, conflictID, decider.claimSide, enums.SUBJECT_KIND_DECISION, j.Row.DecisionID, enums.PARTICIPANT_ROLE_INCUMBENT); err != nil {
			return nil, err
		}
		if sev >= notifySeverityFloor && (fresh || !maxNotif.Valid || int64(sevEnum) > maxNotif.Int64) {
			instrID, err := uuid.NewV4()
			if err != nil {
				return nil, err
			}
			ownerName := settleSide{Key: owner.SessionKey, Label: owner.AgentLabel}.name()
			body := fmt.Sprintf("%s (%s) on %s: %s", key, sev.String(), j.DecisionKey, deciderAction(ownerName, j.IntentKey, in.Rationale))
			if _, err := q.ExecContext(ctx,
				"INSERT INTO `instruction` (`id`, `team_uuid`, `target_session_uuid`, `target_agent_uuid`, `key`, "+
					"`source`, `kind`, `body`, `ref_kind`, `ref_uuid`, `requires_report`, `status`, "+
					"`expires_at`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)",
				instrID.String(), j.TeamUUID.String(), decider.SessionUUID, decider.AgentUUID, instructionKey(tc, 0),
				int64(enums.INSTRUCTION_SOURCE_SERVER), int64(enums.INSTRUCTION_KIND_CONFLICT_NOTICE),
				truncate(body, 600), int64(enums.SUBJECT_KIND_CONFLICT), conflictID.String(),
				int64(enums.INSTRUCTION_STATUS_PENDING), tc.Now.Add(detectInstructionTTL), tc.Now, tc.Now); err != nil {
				return nil, retryable(err, "queueing the decision notice for its author")
			}
			if _, err := q.ExecContext(ctx,
				"UPDATE `conflict` SET `notified_at` = ?, `max_severity_notified` = ?, `updated_at` = ? WHERE `id` = ?",
				tc.Now, int64(sevEnum), tc.Now, conflictID.String()); err != nil {
				return nil, retryable(err, "marking the decision conflict notified")
			}
		}
	}

	if sev < notifySeverityFloor {
		switch {
		case unsure:
			m.Envelope.Note = fmt.Sprintf("judged %s against %s: unsure, recorded low on the board", j.IntentKey, j.DecisionKey)
		case conf < coordination.JudgeMinConfidence:
			m.Envelope.Note = fmt.Sprintf("judged %s against %s: conflict%s, recorded low on the board; below 0.7 it interrupts nobody",
				j.IntentKey, j.DecisionKey, confidencePart(in.Confidence))
		default:
			m.Envelope.Note = fmt.Sprintf("judged %s against %s: conflict%s, recorded low on the board", j.IntentKey, j.DecisionKey, confidencePart(in.Confidence))
		}
		return nil, nil
	}

	if _, err := q.ExecContext(ctx,
		"UPDATE `conflict_participant` SET `notified_at` = COALESCE(`notified_at`, ?), `updated_at` = ? WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
		tc.Now, tc.Now, conflictID.String(), j.SessionUUID.String()); err != nil {
		return nil, retryable(err, "marking the caller notified of the conflict")
	}
	with := deciderName
	if decider != nil {
		with = describeHolder(decider.claimSide)
	}
	m.Envelope.Note = fmt.Sprintf("judged %s against %s: conflict%s — read conflicts[] before you edit", j.IntentKey, j.DecisionKey, confidencePart(in.Confidence))
	return []ConflictNotice{{
		Key:             key,
		Kind:            enums.ConflictKind(enums.CONFLICT_KIND_DECISION_CONTRADICTION).String(),
		Severity:        sev.String(),
		With:            with,
		Decision:        j.DecisionKey,
		AtFault:         "plan",
		SuggestedAction: action,
	}}, nil
}

// upsertDecisionParticipant attaches one side once, and keeps its subject
// current. Under the team lock, so check-then-write is safe.
func upsertDecisionParticipant(ctx context.Context, tc *TxContext, teamUUID, conflictID uuid.UUID, s claimSide, kind enums.SubjectKind, subject string, role enums.ParticipantRole) error {
	var exists int
	err := tc.Tx.QueryRowContext(ctx,
		"SELECT 1 FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? LIMIT 1",
		conflictID.String(), s.SessionUUID).Scan(&exists)
	switch {
	case err == nil:
		_, err := tc.Tx.ExecContext(ctx,
			"UPDATE `conflict_participant` SET `subject_uuid` = ?, `updated_at` = ? WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
			subject, tc.Now, conflictID.String(), s.SessionUUID)
		return retryable(err, "updating the conflict participant")
	case !errors.Is(err, sql.ErrNoRows):
		return retryable(err, "checking the conflict participants")
	}
	id, err := uuid.NewV4()
	if err != nil {
		return err
	}
	_, err = tc.Tx.ExecContext(ctx,
		"INSERT INTO `conflict_participant` (`id`, `conflict_uuid`, `team_uuid`, `session_uuid`, `agent_uuid`, "+
			"`member_uuid`, `subject_kind`, `subject_uuid`, `role`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id.String(), conflictID.String(), teamUUID.String(), s.SessionUUID, s.AgentUUID, s.MemberUUID,
		int64(kind), subject, int64(role), tc.Now, tc.Now)
	return retryable(err, "attaching the conflict participant")
}

// ─────────────────────────────────────────────
// Wording (§4.8)
// ─────────────────────────────────────────────

// planOwnerAction is the suggested action for the plan's owner, ≤ 400.
func planOwnerAction(intent, decision, statement, decider, deciderSession string, sameMember, unsure bool) string {
	stmt := clip(statement, 120)
	name := firstNonEmpty(decider, "its author")
	var s string
	switch {
	case unsure:
		s = fmt.Sprintf("The wording of %s does not settle whether %s breaks it. Nothing to do now; if it matters, ask %s's agent what it means, then revise the plan or the decision.",
			decision, intent, name)
	case sameMember:
		s = fmt.Sprintf("Your plan %s breaks %s (%s), which your own person decided. Change the plan to follow it and update_intent with the new summary, or revise %s with record_decision if the decision is what changed.",
			intent, decision, stmt, decision)
	case deciderSession != "":
		s = fmt.Sprintf("Your plan %s breaks %s (%s). Change the plan to follow it and update_intent with the new summary, which asks you to judge again. If the decision itself is wrong, settle that with %s's agent (%s); only %s or your person can change it.",
			intent, decision, stmt, name, deciderSession, name)
	default:
		s = fmt.Sprintf("Your plan %s breaks %s (%s). Change the plan to follow it and update_intent with the new summary, which asks you to judge again. If the decision itself is wrong, ask your person: only %s or your person agreeing can change it.",
			intent, decision, stmt, name)
	}
	return clip(s, resolutionNoteChars)
}

// deciderAction is what the decider's agent reads after "CF-31 (medium) on
// #key: ", within what get_instructions shows.
func deciderAction(ownerName, intent, rationale string) string {
	return clip(fmt.Sprintf("%s judged its plan %s breaks your decision: \"%s\". They were told to follow it. If the decision should change, revise it with record_decision.",
		ownerName, intent, clip(sanitizeNoteText(rationale), 60)), instructionTextChars)
}
