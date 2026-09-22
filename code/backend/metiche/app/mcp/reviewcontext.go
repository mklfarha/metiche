package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// reviewcontext.go is PLAN.md's tool 10, get_review_context (docs/DECISIONS.md
// §3.2): the pairs metiche asked this session to judge, each a recorded
// decision beside the session's own plan, with why they were paired.
//
// READ-ONLY, and not by convention: one read-only transaction, no commit, no
// event, no write of any kind. It can be called again and again, from a live,
// stale or ended session, and answers the same until something else changes.

type GetReviewContextParams struct {
	SessionKey string `json:"session_key" jsonschema:"The session_key start_session gave you. You only ever see pairs assigned to your own session."`
	TeamSlug   string `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	PairKey    string `json:"pair_key,omitempty" jsonschema:"One pair, by the pair_key a review block or an earlier call gave you. Omit it to get the pairs waiting for you, oldest first."`
	Limit      int    `json:"limit,omitempty" jsonschema:"How many pairs, 1-3. Defaults to 3. Anything left over is reported as more_waiting."`
}

type ReviewContextResult struct {
	Envelope
	Reviews     []ReviewItem `json:"reviews"`
	MoreWaiting int          `json:"more_waiting,omitempty"`
}

type ReviewItem struct {
	PairKey    string          `json:"pair_key"`
	Kind       string          `json:"kind"`     // "decision_contradiction" | "duplicate_work"
	Question   string          `json:"question"` // "Would %s, as planned, break decision %s?"
	Why        []string        `json:"why"`
	Decision   *ReviewDecision `json:"decision,omitempty"`
	Plan       ReviewPlan      `json:"plan"`            // always the caller's own plan
	Other      *ReviewPlan     `json:"other,omitempty"` // duplicate_work: the other agent's plan
	AnswerWith string          `json:"answer_with"`     // "report_judgement(pair_key, verdict, confidence, severity, rationale)"
	ExpiresAt  *string         `json:"expires_at,omitempty"`
}

type ReviewDecision struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Statement string   `json:"statement"`
	Scope     []string `json:"scope"`
	DecidedBy string   `json:"decided_by,omitempty"`
	Revision  int64    `json:"revision"`
}

type ReviewPlan struct {
	Key         string   `json:"key"`
	Summary     string   `json:"summary"`
	Paths       []string `json:"paths"` // at most 8 of the intent's held paths
	Revision    int64    `json:"revision"`
	Who         string   `json:"who,omitempty"`          // other plan only: describeHolder
	SessionKey  string   `json:"session_key,omitempty"`  // other plan only
	Status      string   `json:"status,omitempty"`       // other plan only: declared | active
	ExternalRef string   `json:"external_ref,omitempty"` // duplicate_work, when set
}

const (
	reviewAnswerWith    = "report_judgement(pair_key, verdict, confidence, severity, rationale)"
	reviewPlanPathsShow = 8
)

// judgementRow is one ledger row as the review tools read it.
type judgementRow struct {
	ID      string
	PairKey string
	Kind    enums.ConflictKind
	Status  enums.JudgementStatus
	Verdict enums.JudgementVerdict
	// Subject a is the decision (decision_contradiction) or the other plan
	// (duplicate_work); subject b is always the judge's own plan.
	SubjectAID  string
	SubjectARev int64
	SubjectBID  string
	SubjectBRev int64
	Judge       string
	ExpiresAt   sql.NullTime
	JudgedAt    sql.NullTime
}

const judgementColumns = "`id`, `pair_key`, `kind`, `status`, COALESCE(`verdict`, 0), `subject_a_uuid`, `subject_a_revision`, " +
	"`subject_b_uuid`, `subject_b_revision`, COALESCE(`judge_session_uuid`, ''), `judging_expires_at`, `judged_at`"

func scanJudgement(scan func(...any) error) (judgementRow, error) {
	var (
		j                     judgementRow
		kind, status, verdict int64
	)
	err := scan(&j.ID, &j.PairKey, &kind, &status, &verdict, &j.SubjectAID, &j.SubjectARev, &j.SubjectBID, &j.SubjectBRev,
		&j.Judge, &j.ExpiresAt, &j.JudgedAt)
	j.Kind, j.Status, j.Verdict = enums.ConflictKind(kind), enums.JudgementStatus(status), enums.JudgementVerdict(verdict)
	return j, err
}

// loadJudgementByPairKey reads one pair through uq_judgement_pair.
func loadJudgementByPairKey(ctx context.Context, q queryer, teamUUID uuid.UUID, pairKey string) (judgementRow, bool, error) {
	j, err := scanJudgement(q.QueryRowContext(ctx,
		"SELECT "+judgementColumns+" FROM `judgement` WHERE `team_uuid` = ? AND `pair_key` = ?",
		teamUUID.String(), pairKey).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return j, false, nil
	case err != nil:
		return j, false, retryable(err, "looking up the pair")
	}
	return j, true, nil
}

// judgeName is who a pair belongs to, for a refusal: its judge's session key,
// or, while it waits unassigned, the plan's session key.
func judgeName(ctx context.Context, q queryer, j judgementRow) string {
	var key string
	if j.Judge != "" {
		if err := q.QueryRowContext(ctx, "SELECT `key` FROM `session` WHERE `id` = ?", j.Judge).Scan(&key); err == nil {
			return key
		}
	}
	if err := q.QueryRowContext(ctx,
		"SELECT s.`key` FROM `intent` i JOIN `session` s ON s.`id` = i.`session_uuid` WHERE i.`id` = ?", j.SubjectBID).Scan(&key); err == nil {
		return key
	}
	return "another session"
}

func verdictWords(v enums.JudgementVerdict) string {
	return strings.ReplaceAll(v.String(), "_", " ")
}

// GetReviewContext returns the caller's pairs to judge. It writes nothing.
func (h *Handler) GetReviewContext(ctx context.Context, _ *mcp.CallToolRequest, args GetReviewContextParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSessionOnTeam(ctx, args.SessionKey, args.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	sess := who.Session

	limit := args.Limit
	if limit <= 0 {
		limit = ReviewContextDefaultLimit
	}
	if limit > ReviewContextMaxLimit {
		limit = ReviewContextMaxLimit
	}

	tx, err := h.core.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, retryable(err, "opening a read-only transaction")
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()

	var rows []judgementRow
	if pk := strings.TrimSpace(args.PairKey); pk != "" {
		j, found, err := loadJudgementByPairKey(ctx, tx, who.Team.ID, pk)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case !found:
			return nil, nil, fmt.Errorf("pair_key %s is not a pair on this team — call get_review_context to see what is waiting for you", pk)
		case j.Judge != sess.ID.String():
			return nil, nil, fmt.Errorf("this pair is %s's to judge; nothing for you here", judgeName(ctx, tx, j))
		case j.Status == enums.JUDGEMENT_STATUS_JUDGED:
			at := ""
			if j.JudgedAt.Valid {
				at = clock(j.JudgedAt.Time)
			}
			return nil, nil, fmt.Errorf("pair %s was already judged %s at %s", pk, verdictWords(j.Verdict), at)
		case j.Status != enums.JUDGEMENT_STATUS_PENDING:
			return nil, nil, errors.New("this pair expired unanswered; call get_review_context for what is waiting now")
		}
		// Pending and assigned to the caller, even when its window lapsed:
		// the caller is still its only judge.
		rows = []judgementRow{j}
	} else {
		// The same set pending.reviews counts, oldest first.
		// idx_judgement_assignment (judge_session_uuid, status, judging_expires_at).
		qrows, err := tx.QueryContext(ctx,
			"SELECT "+judgementColumns+" FROM `judgement` WHERE `judge_session_uuid` = ? AND `status` = ? AND `judging_expires_at` > ? "+
				"ORDER BY `updated_at`, `id` LIMIT ?",
			sess.ID.String(), int64(enums.JUDGEMENT_STATUS_PENDING), now, limit)
		if err != nil {
			return nil, nil, retryable(err, "reading your pairs to judge")
		}
		for qrows.Next() {
			j, err := scanJudgement(qrows.Scan)
			if err != nil {
				_ = qrows.Close()
				return nil, nil, retryable(err, "reading your pairs to judge")
			}
			rows = append(rows, j)
		}
		if err := qrows.Err(); err != nil {
			_ = qrows.Close()
			return nil, nil, retryable(err, "reading your pairs to judge")
		}
		_ = qrows.Close()
	}

	pending, err := h.pendingCounts(ctx, tx, uuidPtr(sess.ID))
	if err != nil {
		return nil, nil, retryable(err, "counting pending work")
	}

	items := make([]ReviewItem, 0, len(rows))
	used, counted := 0, 0
	for _, j := range rows {
		item, err := buildReviewItem(ctx, tx, j, now)
		if err != nil {
			return nil, nil, err
		}
		b, err := json.Marshal(item)
		if err != nil {
			return nil, nil, fmt.Errorf("rendering a pair: %w", err)
		}
		if len(items) > 0 && used+len([]rune(string(b))) > reviewContextCharBudget {
			break
		}
		used += len([]rune(string(b)))
		items = append(items, item)
		if j.ExpiresAt.Valid && j.ExpiresAt.Time.After(now) {
			counted++
		}
	}
	more := pending.Reviews - counted
	if more < 0 {
		more = 0
	}

	var seq, rev int64
	if err := tx.QueryRowContext(ctx, "SELECT `sequence`, `board_revision` FROM `team` WHERE `id` = ?", who.Team.ID.String()).
		Scan(&seq, &rev); err != nil {
		return nil, nil, retryable(err, "reading the team cursors")
	}

	note := "nothing waiting for you to judge"
	if len(items) > 0 {
		note = fmt.Sprintf("%d pair(s) to judge: read each, then report_judgement", len(items))
		if more > 0 {
			note += fmt.Sprintf("; %d more waiting — call get_review_context again", more)
		}
	}
	out := ReviewContextResult{
		Envelope:    Envelope{OK: true, Key: sess.Key, Sequence: seq, Revision: rev, Pending: pending, Note: note},
		Reviews:     items,
		MoreWaiting: more,
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, nil, fmt.Errorf("rendering the review context: %w", err)
	}
	return jsonResult(raw)
}

// buildReviewItem dispatches on the pair's kind (docs/DUPLICATES.md §3.2).
func buildReviewItem(ctx context.Context, q queryer, j judgementRow, now time.Time) (ReviewItem, error) {
	if j.Kind == enums.CONFLICT_KIND_DUPLICATE_WORK {
		return buildDuplicateReviewItem(ctx, q, j, now)
	}
	return buildDecisionReviewItem(ctx, q, j, now)
}

// buildDecisionReviewItem reads one pair's decision and plan by primary key and says
// why they were paired. The rationale is never included.
func buildDecisionReviewItem(ctx context.Context, q queryer, j judgementRow, now time.Time) (ReviewItem, error) {
	item := ReviewItem{
		PairKey:    j.PairKey,
		Kind:       j.Kind.String(),
		Why:        []string{},
		AnswerWith: reviewAnswerWith,
		Plan:       ReviewPlan{Paths: []string{}},
	}
	if j.ExpiresAt.Valid {
		s := j.ExpiresAt.Time.UTC().Format(time.RFC3339)
		item.ExpiresAt = &s
	}

	var (
		dec        ReviewDecision
		alwaysShow bool
		decFound   bool
	)
	err := q.QueryRowContext(ctx,
		"SELECT d.`key`, d.`title`, d.`statement`, d.`revision`, d.`always_show`, COALESCE(m.`display_name`, '') "+
			"FROM `decision` d LEFT JOIN `member` m ON m.`id` = d.`decided_by_member_uuid` WHERE d.`id` = ?", j.SubjectAID).
		Scan(&dec.Key, &dec.Title, &dec.Statement, &dec.Revision, &alwaysShow, &dec.DecidedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return item, retryable(err, "reading the pair's decision")
	default:
		decFound = true
		id, _ := uuid.FromString(j.SubjectAID)
		scope, err := loadDecisionScope(ctx, q, id)
		if err != nil {
			return item, err
		}
		dec.Scope = scope
		item.Decision = &dec
	}

	var caseInsensitive bool
	err = q.QueryRowContext(ctx,
		"SELECT i.`key`, i.`summary`, i.`revision`, COALESCE(p.`case_insensitive_paths`, 1) FROM `intent` i "+
			"LEFT JOIN `project` p ON p.`id` = i.`project_uuid` WHERE i.`id` = ?", j.SubjectBID).
		Scan(&item.Plan.Key, &item.Plan.Summary, &item.Plan.Revision, &caseInsensitive)
	intentFound := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return item, retryable(err, "reading the pair's plan")
	}
	var held []settlePath
	if intentFound {
		held, err = loadIntentHeldPaths(ctx, q, j.SubjectBID, now, settleMaxPathsPerSession)
		if err != nil {
			return item, err
		}
		seen := map[string]bool{}
		for _, p := range held {
			if len(item.Plan.Paths) >= reviewPlanPathsShow {
				break
			}
			if !seen[p.Path.PatternNorm] {
				seen[p.Path.PatternNorm] = true
				item.Plan.Paths = append(item.Plan.Paths, p.Path.PatternNorm)
			}
		}
	}
	item.Question = fmt.Sprintf("Would %s, as planned, break decision %s?", firstNonEmpty(item.Plan.Key, "your plan"), firstNonEmpty(dec.Key, "a recorded decision"))

	if decFound && intentFound {
		if _, planPath, ok := firstScopeOverlap(dec.Scope, held, caseInsensitive); ok {
			item.Why = append(item.Why, fmt.Sprintf("your claim %s is in its scope", planPath))
		}
		if alwaysShow {
			item.Why = append(item.Why, "the team checks every plan against it")
		}
		tokens, err := loadDecisionTokenSet(ctx, q, j.SubjectAID)
		if err != nil {
			return item, err
		}
		if shared := sharedTokens(item.Plan.Summary, tokens); len(shared) >= decisionMinSharedTokens {
			item.Why = append(item.Why, "your summary shares words with it: "+joinFirst(shared, 4))
		}
	}
	return item, nil
}

// loadDecisionTokenSet reads a decision's tokens (the decision_has_tokens
// foreign key's index), capped.
func loadDecisionTokenSet(ctx context.Context, q queryer, decisionUUID string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT `token` FROM `decision_token` WHERE `decision_uuid` = ? LIMIT ?", decisionUUID, decisionMaxTokens)
	if err != nil {
		return nil, retryable(err, "reading the decision's words")
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, retryable(err, "reading the decision's words")
		}
		out[t] = true
	}
	return out, retryable(rows.Err(), "reading the decision's words")
}

// buildDuplicateReviewItem reads a duplicate_work pair's two plans by primary
// key (docs/DUPLICATES.md §3.2): plan is always the caller's own (subject b),
// other the other agent's (subject a). The criterion is in the question on
// purpose, against the yes-bias. wording_revision is never shown.
func buildDuplicateReviewItem(ctx context.Context, q queryer, j judgementRow, now time.Time) (ReviewItem, error) {
	item := ReviewItem{
		PairKey:    j.PairKey,
		Kind:       j.Kind.String(),
		Why:        []string{},
		AnswerWith: reviewAnswerWith,
		Plan:       ReviewPlan{Paths: []string{}},
	}
	if j.ExpiresAt.Valid {
		s := j.ExpiresAt.Time.UTC().Format(time.RFC3339)
		item.ExpiresAt = &s
	}
	a, err := loadDuplicatePlan(ctx, q, j.SubjectAID)
	if err != nil {
		return item, err
	}
	b, err := loadDuplicatePlan(ctx, q, j.SubjectBID)
	if err != nil {
		return item, err
	}
	shown := func(p duplicatePlan) ([]string, error) {
		out := []string{}
		if !p.Found {
			return out, nil
		}
		held, err := loadIntentHeldPaths(ctx, q, p.ID, now, settleMaxPathsPerSession)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, h := range held {
			if len(out) >= reviewPlanPathsShow {
				break
			}
			if !seen[h.Path.PatternNorm] {
				seen[h.Path.PatternNorm] = true
				out = append(out, h.Path.PatternNorm)
			}
		}
		return out, nil
	}
	item.Plan = ReviewPlan{Key: b.Key, Summary: b.Summary, Revision: b.Revision, ExternalRef: strings.TrimSpace(b.ExternalRef)}
	if item.Plan.Paths, err = shown(b); err != nil {
		return item, err
	}
	other := ReviewPlan{Key: a.Key, Summary: a.Summary, Revision: a.Revision, ExternalRef: strings.TrimSpace(a.ExternalRef)}
	if other.Paths, err = shown(a); err != nil {
		return item, err
	}
	if a.Found {
		other.Status = a.Status.String()
		owner, _, err := loadDecisionSide(ctx, q, a.Session)
		if err != nil {
			return item, err
		}
		if owner.Found {
			other.Who = describeHolder(owner.claimSide)
			other.SessionKey = owner.SessionKey
		}
	}
	item.Other = &other
	item.Question = fmt.Sprintf("Would carrying out %s build the same thing %s (%s) is already building — the same change, not just the same area?",
		firstNonEmpty(b.Key, "your plan"), firstNonEmpty(a.Key, "another plan"), firstNonEmpty(other.Who, "another agent"))

	if a.Found && b.Found {
		facts, err := loadDuplicateFacts(ctx, q, a, b, now)
		if err != nil {
			return item, err
		}
		if facts.Issue != "" {
			item.Why = append(item.Why, "same issue: "+facts.Issue)
		}
		if len(facts.Words) > 0 {
			item.Why = append(item.Why, "your summaries share words: "+strings.Join(facts.Words, ", "))
		}
		if facts.Overlap != "" {
			item.Why = append(item.Why, "you also claim overlapping files: "+facts.Overlap)
		}
	}
	return item, nil
}
