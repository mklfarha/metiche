package mcp

import (
	"context"
	"sort"

	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/core"
	"github.com/mklfarha/metiche/backend/enums"
)

// duplicatereview.go pairs a declared or reworded plan with live plans that may
// be the same work, and puts the pairs in front of the plan's own agent in the
// same response (docs/DUPLICATES.md §3.1).
//
// It is a DetectHook chained after the path detector and before the decision
// reviewer, so it runs inside the team lock, after the claim rows exist, and it
// takes the shared per-minute cap first: a duplicate is time-critical. It mints
// at most duplicateMaxPairsPerCall pairs, so one slot is always left for a
// decision.
//
// The server never decides "same thing". It finds candidates (the same issue id
// anywhere on the team, or summaries that say the same thing in this project)
// and hands each to the agent that just declared or reworded. That agent
// declared later, so on a conflict it is the one asked to yield.
//
// The budget: one point read of the caller's plan, an optional issue lookup
// (LIMIT 8), one idx_intent_live scan (LIMIT 100) scored in Go, the pinned and
// existing-key checks, ≤ 2 inserts and one bounded session hydration. The scan
// is skipped when the caller's summary names nothing but vague words.

// NewDuplicateReviewer returns the Detect hook that pairs a declared or reworded intent with
// live plans that may be the same work, assigns the judgements and appends review pairs to the
// request for the renderer (§3.1). It returns no ConflictNotices.
func NewDuplicateReviewer(coreImpl *core.Implementation, logger *zap.Logger) DetectHook {
	if logger == nil {
		logger = zap.NewNop()
	}
	d := &duplicateReviewer{core: coreImpl, logger: logger}
	return d.review
}

type duplicateReviewer struct {
	core   *core.Implementation
	logger *zap.Logger
}

// duplicateCallerPlan is the caller's plan as step 1 reads it.
type duplicateCallerPlan struct {
	Summary         string
	ExternalRef     string
	Status          enums.IntentStatus
	Kind            enums.IntentKind
	WordingRevision int64
	ProjectKey      string
}

// duplicateCandidateRow is one other plan the reviewer may pair.
type duplicateCandidateRow struct {
	ID              string
	Key             string
	Summary         string
	WordingRevision int64
	Session         string
	Kind            enums.IntentKind
	Parent          string
	Signal          coordination.DuplicateSignal
	Core, Shared    int
}

func (d *duplicateReviewer) review(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
	req := pathDetectionFrom(ctx)
	if req == nil || req.IntentUUID == nil || req.IntentUUID.IsNil() {
		return nil, nil
	}
	reworded := false
	switch m.Kind {
	case enums.EVENT_KIND_INTENT_DECLARED:
	case enums.EVENT_KIND_INTENT_UPDATED:
		// Only a material wording change. A path-only or status-line update
		// mints nothing and leaves pending pairs answerable.
		if !req.WordingChanged {
			return nil, nil
		}
		reworded = true
	default:
		return nil, nil
	}
	return nil, d.reviewIntent(ctx, tc, req, reworded)
}

func (d *duplicateReviewer) reviewIntent(ctx context.Context, tc *TxContext, req *pathDetectionRequest, reworded bool) error {
	q := tc.Tx
	intentID := req.IntentUUID.String()

	// 1. The caller's plan, by primary key.
	var (
		plan         duplicateCallerPlan
		status, kind int64
	)
	err := q.QueryRowContext(ctx,
		"SELECT i.`summary`, COALESCE(i.`external_ref`, ''), i.`status`, i.`kind`, i.`wording_revision`, COALESCE(p.`key`, '') "+
			"FROM `intent` i LEFT JOIN `project` p ON p.`id` = i.`project_uuid` WHERE i.`id` = ?", intentID).
		Scan(&plan.Summary, &plan.ExternalRef, &status, &kind, &plan.WordingRevision, &plan.ProjectKey)
	if err != nil {
		return retryable(err, "reading the plan to compare")
	}
	plan.Status, plan.Kind = enums.IntentStatus(status), enums.IntentKind(kind)
	if plan.Status != enums.INTENT_STATUS_DECLARED && plan.Status != enums.INTENT_STATUS_ACTIVE {
		return nil
	}

	// 2. On a rewording, the pairs other sessions were asked about this plan's
	// old wording are over. idx_judgement_subject. The caller's own stale
	// pairs (subject b) stay pending: report_judgement refuses them as stale
	// and the sweeper expires them, exactly as decisions do.
	if reworded {
		if _, err := q.ExecContext(ctx,
			"UPDATE `judgement` SET `status` = ?, `updated_at` = ? "+
				"WHERE `team_uuid` = ? AND `subject_a_uuid` = ? AND `kind` = ? AND `status` = ?",
			int64(enums.JUDGEMENT_STATUS_EXPIRED), tc.Now, tc.TeamUUID.String(), intentID,
			int64(enums.CONFLICT_KIND_DUPLICATE_WORK), int64(enums.JUDGEMENT_STATUS_PENDING)); err != nil {
			return retryable(err, "expiring the pairs asked about the old wording")
		}
	}

	// A hold is not work anybody could duplicate (§4.11).
	if plan.Kind == enums.INTENT_KIND_HOLD {
		return nil
	}

	cands := map[string]*duplicateCandidateRow{}
	var order []string
	keep := func(c duplicateCandidateRow) {
		if prev, ok := cands[c.ID]; ok {
			if c.Signal > prev.Signal {
				prev.Signal = c.Signal
			}
			if c.Core > prev.Core {
				prev.Core = c.Core
			}
			if c.Shared > prev.Shared {
				prev.Shared = c.Shared
			}
			return
		}
		cc := c
		cands[c.ID] = &cc
		order = append(order, c.ID)
	}

	// 3. same_issue: team-wide, idx_intent_external_ref.
	if plan.ExternalRef != "" {
		rows, err := q.QueryContext(ctx,
			"SELECT i.`id`, i.`key`, i.`summary`, i.`wording_revision`, i.`session_uuid`, i.`kind`, COALESCE(s.`parent_session_uuid`, '') "+
				"FROM `intent` i JOIN `session` s ON s.`id` = i.`session_uuid` "+
				"WHERE i.`team_uuid` = ? AND i.`external_ref` = ? AND i.`status` IN (?, ?) AND i.`expires_at` > ? "+
				"AND s.`status` IN (?, ?) AND i.`session_uuid` <> ? LIMIT ?",
			tc.TeamUUID.String(), plan.ExternalRef, int64(enums.INTENT_STATUS_DECLARED), int64(enums.INTENT_STATUS_ACTIVE), tc.Now,
			int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE), req.SessionUUID.String(), duplicateSameIssueLimit)
		if err != nil {
			return retryable(err, "looking up plans on the same issue")
		}
		for rows.Next() {
			var (
				c     duplicateCandidateRow
				ckind int64
			)
			if err := rows.Scan(&c.ID, &c.Key, &c.Summary, &c.WordingRevision, &c.Session, &ckind, &c.Parent); err != nil {
				_ = rows.Close()
				return retryable(err, "looking up plans on the same issue")
			}
			c.Kind, c.Signal = enums.IntentKind(ckind), coordination.DupSignalSameIssue
			keep(c)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return retryable(err, "looking up plans on the same issue")
		}
		_ = rows.Close()
	}

	// 4. words: this project only. Skipped when the summary names nothing
	// but vague words: no wording rule can fire on m = 0.
	mine := coordination.DuplicateTermsOf(plan.Summary, plan.ProjectKey)
	if duplicateNamesSomething(mine) {
		live, err := loadLiveIntents(ctx, q, "i.`project_uuid` = ?", []any{req.ProjectUUID.String()}, req.SessionUUID, tc.Now, duplicateMaxLiveIntentsScanned)
		if err != nil {
			return err
		}
		terms := make([]coordination.DuplicateTerms, 0, len(live)+1)
		terms = append(terms, mine)
		for _, li := range live {
			terms = append(terms, coordination.DuplicateTermsOf(li.Summary, plan.ProjectKey))
		}
		drop := coordination.FrequentTerms(terms)
		for i, li := range live {
			overlap := req.OverlapSessions[li.Session] != ""
			score := coordination.ScoreDuplicate(mine, terms[i+1], drop, overlap)
			if !score.Candidate {
				continue
			}
			signal := coordination.DupSignalWords
			if score.Rule == "words_and_paths" {
				signal = coordination.DupSignalWordsAndPaths
			}
			keep(duplicateCandidateRow{
				ID: li.ID, Key: li.Key, Summary: li.Summary, WordingRevision: li.WordingRevision, Session: li.Session,
				Kind: li.Kind, Parent: li.Parent, Signal: signal, Core: score.Core, Shared: len(score.Shared),
			})
		}
	}
	if len(cands) == 0 {
		return nil
	}

	// 5. Drop: a supervisor and its subagent (siblings are kept: two
	// subagents on one thing is a real duplicate), and hold plans.
	var list []coordination.DuplicateCandidate
	for _, id := range order {
		c := cands[id]
		if c.Session == req.SessionUUID.String() {
			continue
		}
		if (req.ParentSessionUUID != "" && c.Session == req.ParentSessionUUID) || c.Parent == req.SessionUUID.String() {
			continue
		}
		if c.Kind == enums.INTENT_KIND_HOLD {
			continue
		}
		list = append(list, coordination.DuplicateCandidate{
			OtherIntentUUID: c.ID, OtherIntentKey: c.Key, Signal: c.Signal, Core: c.Core, Shared: c.Shared,
		})
	}
	// 6. Rank, keep the best scored, then the pinned and existing checks.
	list = coordination.RankDuplicateCandidates(list, duplicateMaxCandidates)
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, 0, len(list))
	keys := make([]string, 0, len(list))
	keyOf := map[string]string{}
	for _, c := range list {
		ids = append(ids, c.OtherIntentUUID)
		k := duplicatePairKey(c.OtherIntentUUID, cands[c.OtherIntentUUID].WordingRevision, intentID, plan.WordingRevision)
		keyOf[c.OtherIntentUUID] = k
		keys = append(keys, k)
	}
	pinned, err := pinnedDuplicatePairs(ctx, q, tc, intentID, ids)
	if err != nil {
		return err
	}
	existing, err := existingPairKeys(ctx, q, tc.TeamUUID, keys)
	if err != nil {
		return err
	}
	var chosen []coordination.DuplicateCandidate
	for _, c := range list {
		if pinned[c.OtherIntentUUID] || existing[keyOf[c.OtherIntentUUID]] {
			continue
		}
		chosen = append(chosen, c)
		if len(chosen) >= duplicateMaxPairsPerCall {
			break
		}
	}
	if len(chosen) == 0 {
		return nil
	}

	// 7. Insert, assigned to the caller while the shared per-minute cap
	// allows, otherwise unassigned (backlog; the sweeper assigns it to
	// subject b's session).
	settings, err := loadDecisionSettings(ctx, q, tc.TeamUUID)
	if err != nil {
		return err
	}
	if !settings.ReviewBlockEnabled {
		req.reviewBlockOff = true
	}
	recent, err := reviewsAssignedRecently(ctx, q, req.SessionUUID.String(), tc.Now, settings.MaxReviewsPerMinute)
	if err != nil {
		return err
	}
	sessionIDs := make([]string, 0, len(chosen))
	for _, c := range chosen {
		sessionIDs = appendUnique(sessionIDs, cands[c.OtherIntentUUID].Session)
	}
	sort.Strings(sessionIDs)
	owners, err := hydrateSessions(ctx, q, sessionIDs)
	if err != nil {
		return err
	}
	for _, c := range chosen {
		other := cands[c.OtherIntentUUID]
		judge := ""
		if recent < settings.MaxReviewsPerMinute {
			judge = req.SessionUUID.String()
			recent++
		}
		pairKey, inserted, err := insertDuplicateJudgement(ctx, q, duplicateJudgement{
			TeamUUID: tc.TeamUUID,
			AUUID:    other.ID, AWording: other.WordingRevision,
			BUUID: intentID, BWording: plan.WordingRevision,
			JudgeSession: judge, Window: settings.JudgeWindow, Now: tc.Now,
		})
		if err != nil {
			return err
		}
		// 8. The pairs assigned in this call ride back in the review block.
		if inserted && judge != "" {
			info := owners[other.Session]
			req.ReviewPairs = append(req.ReviewPairs, reviewBlockPair{
				PairKey: pairKey,
				Kind:    duplicatePairKind,
				Plan:    other.Key,
				With: describeHolder(claimSide{
					SessionKey: info.Key, MemberName: info.MemberName, AgentLabel: info.AgentLabel,
				}),
				Summary: other.Summary,
				Why:     c.Signal.String(),
			})
		}
	}
	return nil
}

// duplicateNamesSomething reports a summary with at least one non-vague key.
func duplicateNamesSomething(t coordination.DuplicateTerms) bool {
	for _, k := range t.Keys {
		if !t.Vague[k] {
			return true
		}
	}
	return false
}

// pinnedDuplicatePairs reports which of the other intents have a pinned
// duplicate judgement with this one, at any revision, in either storage order.
// idx_judgement_subject serves both halves of the OR.
func pinnedDuplicatePairs(ctx context.Context, q queryer, tc *TxContext, intentID string, others []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(others) == 0 {
		return out, nil
	}
	args := []any{tc.TeamUUID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK), intentID}
	for _, id := range others {
		args = append(args, id)
	}
	for _, id := range others {
		args = append(args, id)
	}
	args = append(args, intentID, 2*len(others))
	rows, err := q.QueryContext(ctx,
		"SELECT `subject_a_uuid`, `subject_b_uuid` FROM `judgement` "+
			"WHERE `team_uuid` = ? AND `kind` = ? AND `pinned` = 1 "+
			"AND ((`subject_a_uuid` = ? AND `subject_b_uuid` IN ("+placeholders(len(others))+")) "+
			"OR (`subject_a_uuid` IN ("+placeholders(len(others))+") AND `subject_b_uuid` = ?)) LIMIT ?", args...)
	if err != nil {
		return nil, retryable(err, "checking for dismissed duplicate pairs")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, retryable(err, "checking for dismissed duplicate pairs")
		}
		if a == intentID {
			out[b] = true
		} else {
			out[a] = true
		}
	}
	return out, retryable(rows.Err(), "checking for dismissed duplicate pairs")
}
