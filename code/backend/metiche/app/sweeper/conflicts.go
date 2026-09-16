package sweeper

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Conflicts a lapsed hold settled
// ─────────────────────────────────────────────

// errNothingToSay is what an extra hook returns when, once it holds the team
// lock, it finds there is no event to write after all. appendEvent rolls back
// without consuming a sequence number.
var errNothingToSay = errors.New("nothing to say")

// settleAfterExpiry re-evaluates the conflicts of every session that just lost
// a claim to its TTL. See app/mcp/conflictresolve.go for the rules.
//
// The agent-driven releases (drop_paths, done, end_session) are re-evaluated
// by the tools inside their own transactions. Expiry and abandonment happen
// with no tool call at all, so without this a collision cleared by a claim
// lapsing would stay open on the board forever.
func (s *Sweeper) settleAfterExpiry(ctx context.Context, batch []expiredClaim, rep *Report) {
	seen := map[uuid.UUID]bool{}
	for _, c := range batch {
		if seen[c.sessionUUID] {
			continue
		}
		seen[c.sessionUUID] = true
		project, member := c.projectUUID, c.memberUUID
		s.settleConflicts(ctx, c.teamUUID, c.sessionUUID, &project, nil, &member, mcp.ReleaseClaimExpired, rep)
	}
}

// settleAfterAbandon does the same for sessions just written off, after their
// claims were dropped.
func (s *Sweeper) settleAfterAbandon(ctx context.Context, teamUUID uuid.UUID, sessions []liveSession, rep *Report) {
	for _, c := range sessions {
		project, agent, member := c.projectUUID, c.agentUUID, c.memberUUID
		s.settleConflicts(ctx, teamUUID, c.uuid, &project, &agent, &member, mcp.ReleaseSessionAbandoned, rep)
		s.settleContractConflicts(ctx, teamUUID, c.uuid, &project, &agent, &member, rep)
		s.settleDecisionConflicts(ctx, teamUUID, c.uuid, &project, &agent, &member, rep)
	}
}

// settleConflicts closes each of one session's conflicts whose overlap is gone,
// one conflict_resolved event each.
//
// The candidate list is read outside the lock. The decision is not: each
// conflict is re-evaluated by mcp.SettleConflict INSIDE appendEvent's locked
// transaction, so the overlap check, the conditional UPDATE and the event are
// one atomic step, and an agent re-declaring the file in between keeps its
// conflict open. A conflict that turns out to still overlap writes nothing and
// consumes no sequence number.
//
// Deliberately NOT charged to the pass's event budget: here the event and the
// status flip are the same transaction, and a conflict skipped for budget
// would never be revisited — its claim no longer matches the expiry scan.
// Each session contributes at most a handful (mcp's own cap).
func (s *Sweeper) settleConflicts(ctx context.Context, teamUUID, sessionUUID uuid.UUID,
	projectUUID, agentUUID, memberUUID *uuid.UUID, kind mcp.ReleaseKind, rep *Report) {
	ids, err := mcp.OpenConflictsOfSession(ctx, s.db, teamUUID, sessionUUID)
	if err != nil {
		rep.addErr("finding the conflicts a lapsed hold may have settled", err)
		return
	}
	for _, id := range ids {
		conflictID, session := id, sessionUUID
		var settled mcp.SettledConflict
		wrote, err := s.appendEvent(ctx, sweepEvent{
			teamUUID:       teamUUID,
			idempotencyKey: "sweep:conflict_resolved:" + conflictID.String(),
			kind:           enums.EVENT_KIND_CONFLICT_RESOLVED,
			// Structural: a settled conflict leaves the lane badges and the
			// open list, and a structural frame is what makes the board
			// refresh its state and repaint.
			structural:  true,
			projectUUID: projectUUID,
			sessionUUID: &session,
			agentUUID:   agentUUID,
			memberUUID:  memberUUID,
			subjectKind: enums.SUBJECT_KIND_CONFLICT,
			subjectUUID: &conflictID,
			extra: func(ctx context.Context, tx *sql.Tx, _ int64, now time.Time) error {
				out, ok, err := mcp.SettleConflict(ctx, tx, conflictID, mcp.Release{
					TeamUUID:    teamUUID,
					SessionUUID: session,
					Kind:        kind,
					At:          now,
				})
				if err != nil {
					return err
				}
				if !ok {
					return errNothingToSay
				}
				settled = out
				return nil
			},
			finish: func() (string, string, []byte) {
				return mcp.ConflictResolvedSummary(settled), settled.Key, mcp.ConflictResolvedPayload(settled).ToJSON()
			},
		})
		switch {
		case err != nil:
			rep.addErr("settling conflict "+conflictID.String(), err)
		case wrote:
			rep.ConflictsResolved++
			rep.EventsEmitted++
		}
	}
}

// settleContractConflicts closes the contract conflicts an abandoned session
// took part in, one conflict_resolved event each. Same shape as
// settleConflicts: candidates outside the lock, the decision inside it.
func (s *Sweeper) settleContractConflicts(ctx context.Context, teamUUID, sessionUUID uuid.UUID,
	projectUUID, agentUUID, memberUUID *uuid.UUID, rep *Report) {
	ids, err := mcp.OpenContractConflictsOfSession(ctx, s.db, teamUUID, sessionUUID)
	if err != nil {
		rep.addErr("finding the contract conflicts an abandoned session may have settled", err)
		return
	}
	for _, id := range ids {
		conflictID, session := id, sessionUUID
		var settled mcp.SettledConflict
		wrote, err := s.appendEvent(ctx, sweepEvent{
			teamUUID:       teamUUID,
			idempotencyKey: "sweep:conflict_resolved:" + conflictID.String(),
			kind:           enums.EVENT_KIND_CONFLICT_RESOLVED,
			structural:     true,
			projectUUID:    projectUUID,
			sessionUUID:    &session,
			agentUUID:      agentUUID,
			memberUUID:     memberUUID,
			subjectKind:    enums.SUBJECT_KIND_CONFLICT,
			subjectUUID:    &conflictID,
			extra: func(ctx context.Context, tx *sql.Tx, _ int64, now time.Time) error {
				out, ok, err := mcp.SettleContractConflict(ctx, tx, conflictID, mcp.ContractRelease{
					TeamUUID:    teamUUID,
					SessionUUID: session,
					Kind:        mcp.ContractReleaseSessionAbandoned,
					At:          now,
				})
				if err != nil {
					return err
				}
				if !ok {
					return errNothingToSay
				}
				settled = out
				return nil
			},
			finish: func() (string, string, []byte) {
				return mcp.ConflictResolvedSummary(settled), settled.Key, mcp.ConflictResolvedPayload(settled).ToJSON()
			},
		})
		switch {
		case err != nil:
			rep.addErr("settling contract conflict "+conflictID.String(), err)
		case wrote:
			rep.ConflictsResolved++
			rep.EventsEmitted++
		}
	}
}

// settleDecisionConflicts closes the decision_contradiction conflicts an
// abandoned session took part in, one conflict_resolved event each. Same shape
// as its two neighbours: candidates outside the lock, the decision inside it.
//
// The plans of a session nobody is running any more cannot break a decision:
// docs/DECISIONS.md §4.5 rule 3. The DECISION itself survives — it is a
// standing agreement, and writing it off because the agent that happened to
// record it stopped heartbeating would quietly un-decide things. Only this
// session's plans' conflicts close, and its pending pairs to judge expire.
//
// DecisionReleaseSessionAbandoned is not merely the wording: it is also what
// suppresses COORDINATED (§4.5). Nobody coordinated here — metiche noticed a
// silence — so the note says "Cleared by metiche", and a report_back somebody
// left earlier does not get to claim the credit.
func (s *Sweeper) settleDecisionConflicts(ctx context.Context, teamUUID, sessionUUID uuid.UUID,
	projectUUID, agentUUID, memberUUID *uuid.UUID, rep *Report) {
	// The pairs this session was asked to judge die with it (§4.5). No event,
	// and no team lock: one conditional UPDATE off idx_judgement_assignment.
	// Without this, a pair armed a minute before the session went quiet would
	// sit pending until its window lapsed, and get re-armed for an agent that
	// is never coming back.
	if n, err := mcp.ExpireJudgementsOfSession(ctx, s.db, sessionUUID, s.now()); err != nil {
		rep.addErr("expiring the abandoned session's pairs to judge", err)
	} else {
		rep.JudgementsExpired += int(n)
	}

	ids, err := mcp.OpenDecisionConflictsOfSession(ctx, s.db, teamUUID, sessionUUID)
	if err != nil {
		rep.addErr("finding the decision conflicts an abandoned session may have settled", err)
		return
	}
	for _, id := range ids {
		conflictID, session := id, sessionUUID
		var settled mcp.SettledConflict
		wrote, err := s.appendEvent(ctx, sweepEvent{
			teamUUID:       teamUUID,
			idempotencyKey: "sweep:conflict_resolved:" + conflictID.String(),
			kind:           enums.EVENT_KIND_CONFLICT_RESOLVED,
			structural:     true,
			projectUUID:    projectUUID,
			sessionUUID:    &session,
			agentUUID:      agentUUID,
			memberUUID:     memberUUID,
			subjectKind:    enums.SUBJECT_KIND_CONFLICT,
			subjectUUID:    &conflictID,
			extra: func(ctx context.Context, tx *sql.Tx, _ int64, now time.Time) error {
				out, ok, err := mcp.SettleDecisionConflict(ctx, tx, conflictID, mcp.DecisionRelease{
					TeamUUID:    teamUUID,
					SessionUUID: session,
					Kind:        mcp.DecisionReleaseSessionAbandoned,
					At:          now,
				})
				if err != nil {
					return err
				}
				if !ok {
					return errNothingToSay
				}
				settled = out
				return nil
			},
			finish: func() (string, string, []byte) {
				return mcp.ConflictResolvedSummary(settled), settled.Key, mcp.ConflictResolvedPayload(settled).ToJSON()
			},
		})
		switch {
		case err != nil:
			rep.addErr("settling decision conflict "+conflictID.String(), err)
		case wrote:
			rep.ConflictsResolved++
			rep.EventsEmitted++
		}
	}
}
