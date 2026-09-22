package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/core"
	"github.com/mklfarha/metiche/backend/enums"
)

// decisionreview.go pairs a declared or updated plan with the recorded
// decisions it touches and puts the pairs in front of the plan's own agent, in
// the same response (docs/DECISIONS.md §3.2 "The inline review block").
//
// It is a DetectHook chained after the path detector, so it runs inside the
// team lock, after the claim rows exist, and the review block and the
// judgement rows it assigns are part of the stored snapshot: a replayed
// declaration returns the same pair keys.
//
// The fast path is the whole cost for a team with no accepted decisions: one
// LIMIT 1 probe on idx_decision_always_show, and the response bytes are
// exactly what they were before decisions existed.

// NewDecisionReviewer returns the Detect hook that pairs a declared or updated intent with
// candidate decisions, assigns the judgements and writes the inline review block (§3.2).
// It returns no ConflictNotices.
func NewDecisionReviewer(coreImpl *core.Implementation, logger *zap.Logger) DetectHook {
	if logger == nil {
		logger = zap.NewNop()
	}
	d := &decisionReviewer{core: coreImpl, logger: logger}
	return d.review
}

// ChainDetectors runs hooks in order on the same TxContext and Mutation, skipping nil hooks,
// concatenating their notices and returning the first error.
//
// After its last hook it renders any review pairs the reviewers assigned and no
// NewReviewRenderer rendered (docs/DUPLICATES.md §3.1): a chain installed as
// path detector + decision reviewer, with no renderer, still answers with the
// review block and note it always did.
func ChainDetectors(hooks ...DetectHook) DetectHook {
	var live []DetectHook
	for _, hook := range hooks {
		if hook != nil {
			live = append(live, hook)
		}
	}
	return func(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
		var all []ConflictNotice
		for _, hook := range live {
			notices, err := hook(ctx, tc, m)
			if err != nil {
				return nil, err
			}
			all = append(all, notices...)
		}
		renderReviewPairs(pathDetectionFrom(ctx), m)
		return all, nil
	}
}

type decisionReviewer struct {
	core   *core.Implementation
	logger *zap.Logger
}

// materialUpdateCtxKey carries whether an update_intent bumped the intent's
// revision. Only a material update is reviewed: an unchanged revision mints no
// new pair, and a pair dropped past the rate cap comes back on the plan's next
// revision, not on its next status line.
type materialUpdateCtxKey struct{}

func withMaterialUpdate(ctx context.Context, material bool) context.Context {
	return context.WithValue(ctx, materialUpdateCtxKey{}, material)
}

func materialUpdateFrom(ctx context.Context) bool {
	v, _ := ctx.Value(materialUpdateCtxKey{}).(bool)
	return v
}

// reviewBlock is the envelope's review field.
type reviewBlock struct {
	Pairs      []reviewBlockPair `json:"pairs"`
	More       int               `json:"more"`
	AnswerWith string            `json:"answer_with"`
}

// reviewBlockPair is one pair in the block. The duplicate fields are all
// omitempty and Decision is always set on a decision pair, so a decision pair
// renders byte for byte as it did before duplicate work (docs/DUPLICATES.md
// §3.1).
type reviewBlockPair struct {
	PairKey   string `json:"pair_key"`
	Kind      string `json:"kind,omitempty"`      // "duplicate_work"; omitted on decision pairs
	Decision  string `json:"decision,omitempty"`  // decision pairs
	Statement string `json:"statement,omitempty"` // decision pairs
	Plan      string `json:"plan,omitempty"`      // duplicate pairs: the other plan's key
	With      string `json:"with,omitempty"`      // duplicate pairs: describeHolder of its owner
	Summary   string `json:"summary,omitempty"`   // duplicate pairs: the other plan's summary
	Why       string `json:"why"`                 // decisions: scope|always_show|words; duplicates: same_issue|words_and_paths|words
}

func (d *decisionReviewer) review(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
	req := pathDetectionFrom(ctx)
	if req == nil || req.IntentUUID == nil || req.IntentUUID.IsNil() {
		return nil, nil
	}
	update := false
	switch m.Kind {
	case enums.EVENT_KIND_INTENT_DECLARED:
	case enums.EVENT_KIND_INTENT_UPDATED:
		if !materialUpdateFrom(ctx) {
			return nil, nil
		}
		update = true
	default:
		return nil, nil
	}

	// The fast path. idx_decision_always_show (team_uuid, status, …).
	var one int
	err := tc.Tx.QueryRowContext(ctx,
		"SELECT 1 FROM `decision` WHERE `team_uuid` = ? AND `status` = ? LIMIT 1",
		tc.TeamUUID.String(), int64(enums.DECISION_STATUS_ACCEPTED)).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, retryable(err, "checking for recorded decisions")
	}
	return nil, d.reviewIntent(ctx, tc, m, req, update)
}

type reviewCandidate struct {
	signal coordination.CandidateSignal
	shared int
}

func (d *decisionReviewer) reviewIntent(ctx context.Context, tc *TxContext, m *Mutation, req *pathDetectionRequest, update bool) error {
	q := tc.Tx
	intentID := req.IntentUUID.String()

	var (
		revision, status int64
		summary          string
		caseInsensitive  bool
	)
	err := q.QueryRowContext(ctx,
		"SELECT i.`revision`, i.`summary`, i.`status`, COALESCE(p.`case_insensitive_paths`, 1) FROM `intent` i "+
			"LEFT JOIN `project` p ON p.`id` = i.`project_uuid` WHERE i.`id` = ?", intentID).
		Scan(&revision, &summary, &status, &caseInsensitive)
	if err != nil {
		return retryable(err, "reading the plan to review")
	}
	if st := enums.IntentStatus(status); st != enums.INTENT_STATUS_DECLARED && st != enums.INTENT_STATUS_ACTIVE {
		return nil
	}
	// A pair against the plan's previous wording is NOT expired here. It stays
	// pending, and report_judgement refuses it as stale (§3.3) so the agent is
	// told its plan moved; the sweeper expires it later (§4.4).
	var paths []coordination.NormalizedPath
	if update {
		held, err := loadIntentHeldPaths(ctx, q, intentID, tc.Now, settleMaxPathsPerSession)
		if err != nil {
			return err
		}
		for _, p := range held {
			if p.Mode != coordination.ModeRead {
				paths = append(paths, p.Path)
			}
		}
	} else {
		for _, p := range req.Declared {
			if p.Mode != coordination.ModeRead {
				paths = append(paths, p.Path)
			}
		}
	}
	if len(paths) > reviewMaxPathsScanned {
		paths = paths[:reviewMaxPathsScanned]
	}

	cands := map[string]*reviewCandidate{}
	mark := func(id string, sig coordination.CandidateSignal, shared int) {
		c, ok := cands[id]
		if !ok {
			if len(cands) >= reviewMaxDecisionsRead {
				return
			}
			c = &reviewCandidate{}
			cands[id] = c
		}
		if sig > c.signal {
			c.signal = sig
		}
		if shared > c.shared {
			c.shared = shared
		}
	}

	// 1. scope
	for _, p := range paths {
		rows, err := decisionPathCandidates(ctx, q, tc.TeamUUID, req.ProjectUUID, p)
		if err != nil {
			return err
		}
		for _, row := range rows {
			np, err := coordination.NormalizePath(row.PatternNorm, nil, caseInsensitive)
			if err != nil {
				continue
			}
			if coordination.PathsOverlap(np, p) {
				mark(row.DecisionUUID, coordination.SignalScope, 0)
			}
		}
	}

	// 2. words
	tokens := coordination.Tokenize(summary, decisionMaxTokens)
	if len(tokens) >= decisionMinSharedTokens {
		args := []any{tc.TeamUUID.String()}
		for _, t := range tokens {
			args = append(args, t)
		}
		args = append(args, req.ProjectUUID.String(), decisionMinSharedTokens, reviewTokenCandidateLimit)
		rows, err := q.QueryContext(ctx,
			"SELECT dt.`decision_uuid`, COUNT(DISTINCT dt.`token`) AS shared, SUM(dt.`weight`) AS score "+
				"FROM `decision_token` dt WHERE dt.`team_uuid` = ? AND dt.`token` IN ("+placeholders(len(tokens))+") "+
				"AND (dt.`project_uuid` = ? OR dt.`project_uuid` IS NULL) "+
				"GROUP BY dt.`decision_uuid` HAVING shared >= ? ORDER BY score DESC, dt.`decision_uuid` LIMIT ?", args...)
		if err != nil {
			return retryable(err, "matching the plan's words to decisions")
		}
		for rows.Next() {
			var (
				id            string
				shared, score int
			)
			if err := rows.Scan(&id, &shared, &score); err != nil {
				_ = rows.Close()
				return retryable(err, "matching the plan's words to decisions")
			}
			mark(id, coordination.SignalWords, shared)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return retryable(err, "matching the plan's words to decisions")
		}
		_ = rows.Close()
	}

	// 3. always_show
	{
		rows, err := q.QueryContext(ctx,
			"SELECT `id` FROM `decision` WHERE `team_uuid` = ? AND `status` = ? AND `always_show` = 1 "+
				"AND (`project_uuid` = ? OR `project_uuid` IS NULL) ORDER BY `key` LIMIT ?",
			tc.TeamUUID.String(), int64(enums.DECISION_STATUS_ACCEPTED), req.ProjectUUID.String(), MaxAlwaysShowPerTeam)
		if err != nil {
			return retryable(err, "reading the always-show decisions")
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return retryable(err, "reading the always-show decisions")
			}
			mark(id, coordination.SignalAlwaysShow, 0)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return retryable(err, "reading the always-show decisions")
		}
		_ = rows.Close()
	}
	if len(cands) == 0 {
		return nil
	}

	// 4. accepted only, not the caller's own, not pinned, not already asked.
	ids := make([]string, 0, len(cands))
	for id := range cands {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	decisions, err := loadReviewDecisions(ctx, q, tc.TeamUUID, ids)
	if err != nil {
		return err
	}
	pinned, err := pinnedDecisionsOfIntent(ctx, q, tc.TeamUUID, intentID, ids)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(decisions))
	keyOf := map[string]string{}
	for _, dec := range decisions {
		k := decisionPairKey(dec.ID, dec.Revision, intentID, revision)
		keyOf[dec.ID] = k
		keys = append(keys, k)
	}
	existing, err := existingPairKeys(ctx, q, tc.TeamUUID, keys)
	if err != nil {
		return err
	}
	byID := map[string]reviewDecision{}
	var list []coordination.DecisionCandidate
	for _, dec := range decisions {
		if dec.RecordedBy == req.SessionUUID.String() || pinned[dec.ID] || existing[keyOf[dec.ID]] {
			continue
		}
		c := cands[dec.ID]
		byID[dec.ID] = dec
		list = append(list, coordination.DecisionCandidate{
			DecisionUUID: dec.ID, DecisionKey: dec.Key, IntentUUID: intentID, IntentKey: req.IntentKey,
			Signal: c.signal, SharedTokens: c.shared,
		})
	}
	if len(list) == 0 {
		return nil
	}

	// 5. rank, assign under the per-minute cap.
	settings, err := loadDecisionSettings(ctx, q, tc.TeamUUID)
	if err != nil {
		return err
	}
	recent, err := reviewsAssignedRecently(ctx, q, req.SessionUUID.String(), tc.Now, settings.MaxReviewsPerMinute)
	if err != nil {
		return err
	}
	if !settings.ReviewBlockEnabled {
		req.reviewBlockOff = true
	}
	for _, c := range coordination.RankDecisionCandidates(list, reviewMaxInlinePairs) {
		judge := ""
		if recent < settings.MaxReviewsPerMinute {
			judge = req.SessionUUID.String()
			recent++
		} else if c.Signal == coordination.SignalAlwaysShow {
			// Lowest priority inside the cap: dropped, not backlogged. It
			// comes back on the plan's next revision.
			continue
		}
		dec := byID[c.DecisionUUID]
		pairKey, inserted, err := insertDecisionJudgement(ctx, q, tc.TeamUUID, decisionJudgement{
			DecisionUUID: dec.ID, DecisionRevision: dec.Revision, IntentUUID: intentID, IntentRevision: revision, JudgeSession: judge,
		}, settings.JudgeWindow, tc.Now)
		if err != nil {
			return err
		}
		if inserted && judge != "" {
			// Appended, never rendered here: the review renderer writes the
			// envelope once for both reviewers.
			req.ReviewPairs = append(req.ReviewPairs, reviewBlockPair{PairKey: pairKey, Decision: dec.Key, Statement: dec.Statement, Why: c.Signal.String()})
		}
	}
	return nil
}

// renderReviewBlock applies §3.2's cut order at reviewInlineChars: drop every
// statement and every duplicate summary, then drop pairs from the end
// (counted in more), then drop the block.
func renderReviewBlock(pairs []reviewBlockPair) (json.RawMessage, bool) {
	b := reviewBlock{Pairs: append([]reviewBlockPair(nil), pairs...), AnswerWith: "report_judgement"}
	render := func() (json.RawMessage, bool) {
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, false
		}
		return raw, utf8.RuneCount(raw) <= reviewInlineChars
	}
	if raw, ok := render(); ok {
		return raw, true
	}
	for i := range b.Pairs {
		b.Pairs[i].Statement = ""
		b.Pairs[i].Summary = ""
	}
	if raw, ok := render(); ok {
		return raw, true
	}
	for len(b.Pairs) > 1 {
		b.Pairs = b.Pairs[:len(b.Pairs)-1]
		b.More++
		if raw, ok := render(); ok {
			return raw, true
		}
	}
	return nil, false
}

// decisionPathRow is one decision_path candidate off idx_decision_path_scan.
type decisionPathRow struct {
	DecisionUUID string
	PatternNorm  string
}

// decisionPathCandidates is §3.2's scope query: the claims' prefix algorithm
// over decision_path, this project's decisions and the team-wide ones.
func decisionPathCandidates(ctx context.Context, q queryer, teamUUID, projectUUID uuid.UUID, mine coordination.NormalizedPath) ([]decisionPathRow, error) {
	ancestors := coordination.PathAncestors(mine.Prefix)
	args := make([]any, 0, len(ancestors)+4)
	args = append(args, teamUUID.String())
	for _, a := range ancestors {
		args = append(args, coordination.PathPrefixToStored(a))
	}
	args = append(args, likePrefixPattern(mine.Prefix), projectUUID.String(), reviewDecisionPathLimit)
	rows, err := q.QueryContext(ctx,
		"SELECT dp.`decision_uuid`, dp.`pattern_norm` FROM `decision_path` dp "+
			"WHERE dp.`team_uuid` = ? AND (dp.`prefix` IN ("+placeholders(len(ancestors))+") OR dp.`prefix` LIKE ? ESCAPE '!') "+
			"AND (dp.`project_uuid` = ? OR dp.`project_uuid` IS NULL) LIMIT ?", args...)
	if err != nil {
		return nil, retryable(err, "scanning the decisions' scopes")
	}
	defer func() { _ = rows.Close() }()
	var out []decisionPathRow
	for rows.Next() {
		var r decisionPathRow
		if err := rows.Scan(&r.DecisionUUID, &r.PatternNorm); err != nil {
			return nil, retryable(err, "scanning the decisions' scopes")
		}
		out = append(out, r)
	}
	return out, retryable(rows.Err(), "scanning the decisions' scopes")
}

// reviewDecision is one accepted decision as the reviewer needs it.
type reviewDecision struct {
	ID         string
	Key        string
	Statement  string
	Revision   int64
	AlwaysShow bool
	RecordedBy string
}

// loadReviewDecisions reads the accepted decisions among ids, by primary key.
func loadReviewDecisions(ctx context.Context, q queryer, teamUUID uuid.UUID, ids []string) ([]reviewDecision, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := []any{teamUUID.String(), int64(enums.DECISION_STATUS_ACCEPTED)}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, len(ids))
	rows, err := q.QueryContext(ctx,
		"SELECT `id`, `key`, `statement`, `revision`, `always_show`, COALESCE(`recorded_by_session_uuid`, '') FROM `decision` "+
			"WHERE `team_uuid` = ? AND `status` = ? AND `id` IN ("+placeholders(len(ids))+") ORDER BY `key` LIMIT ?", args...)
	if err != nil {
		return nil, retryable(err, "reading the candidate decisions")
	}
	defer func() { _ = rows.Close() }()
	var out []reviewDecision
	for rows.Next() {
		var d reviewDecision
		if err := rows.Scan(&d.ID, &d.Key, &d.Statement, &d.Revision, &d.AlwaysShow, &d.RecordedBy); err != nil {
			return nil, retryable(err, "reading the candidate decisions")
		}
		out = append(out, d)
	}
	return out, retryable(rows.Err(), "reading the candidate decisions")
}

// pinnedDecisionsOfIntent reports which decisions have a pinned judgement with
// this intent at any revision. idx_judgement_subject, capped.
func pinnedDecisionsOfIntent(ctx context.Context, q queryer, teamUUID uuid.UUID, intentUUID string, decisionIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(decisionIDs) == 0 {
		return out, nil
	}
	args := []any{teamUUID.String()}
	for _, id := range decisionIDs {
		args = append(args, id)
	}
	args = append(args, intentUUID, len(decisionIDs))
	rows, err := q.QueryContext(ctx,
		"SELECT DISTINCT `subject_a_uuid` FROM `judgement` WHERE `team_uuid` = ? AND `subject_a_uuid` IN ("+placeholders(len(decisionIDs))+") "+
			"AND `subject_b_uuid` = ? AND `pinned` = 1 LIMIT ?", args...)
	if err != nil {
		return nil, retryable(err, "checking for dismissed pairs")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, retryable(err, "checking for dismissed pairs")
		}
		out[id] = true
	}
	return out, retryable(rows.Err(), "checking for dismissed pairs")
}

// loadIntentHeldPaths reads an intent's held, unexpired paths through its
// claims (the intent_has_claims foreign key's index), capped.
func loadIntentHeldPaths(ctx context.Context, q queryer, intentUUID string, now time.Time, limit int) ([]settlePath, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT cp.`mode`, cp.`pattern`, cp.`pattern_norm`, cp.`kind`, cp.`prefix`, COALESCE(cp.`suffix_pattern`, ''), cp.`depth`, COALESCE(cp.`ext`, '') "+
			"FROM `claim` c JOIN `claim_path` cp ON cp.`claim_uuid` = c.`id` "+
			"WHERE c.`intent_uuid` = ? AND c.`status` = ? AND cp.`status` = ? AND cp.`expires_at` > ? "+
			"ORDER BY cp.`created_at`, cp.`pattern_norm` LIMIT ?",
		intentUUID, int64(enums.CLAIM_STATUS_HELD), int64(enums.CLAIM_STATUS_HELD), now, limit)
	if err != nil {
		return nil, retryable(err, "reading the plan's paths")
	}
	defer func() { _ = rows.Close() }()
	var out []settlePath
	for rows.Next() {
		var (
			mode, kind, depth                  int64
			pattern, norm, prefix, suffix, ext string
		)
		if err := rows.Scan(&mode, &pattern, &norm, &kind, &prefix, &suffix, &depth, &ext); err != nil {
			return nil, retryable(err, "reading the plan's paths")
		}
		out = append(out, settlePath{
			Mode: claimModeToCoordination(enums.ClaimMode(mode)),
			Path: normalizedPathFromRow(pattern, norm, enums.ClaimPathKind(kind), prefix, suffix, depth, ext),
		})
	}
	return out, retryable(rows.Err(), "reading the plan's paths")
}

// firstScopeOverlap finds the first (decision path, plan path) that touch, for
// a why line and a conflict's evidence. Plan paths only reading never count.
func firstScopeOverlap(decisionScope []string, plan []settlePath, caseInsensitive bool) (decisionPattern, planPattern string, ok bool) {
	for _, dp := range decisionScope {
		np, err := coordination.NormalizePath(dp, nil, caseInsensitive)
		if err != nil {
			continue
		}
		for _, p := range plan {
			if p.Mode == coordination.ModeRead {
				continue
			}
			if coordination.PathsOverlap(np, p.Path) {
				return dp, p.Path.PatternNorm, true
			}
		}
	}
	return "", "", false
}

// sharedTokens lists the plan summary's tokens the decision also has, in the
// summary's order.
func sharedTokens(summary string, decisionTokens map[string]bool) []string {
	var out []string
	for _, t := range coordination.Tokenize(summary, decisionMaxTokens) {
		if decisionTokens[t] {
			out = append(out, t)
		}
	}
	return out
}

func joinFirst(items []string, n int) string {
	if len(items) > n {
		items = items[:n]
	}
	return strings.Join(items, ", ")
}
