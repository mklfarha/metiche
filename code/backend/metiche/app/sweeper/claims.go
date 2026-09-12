package sweeper

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gofrs/uuid"

	"github.com/guregu/null/v6"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Claims
// ─────────────────────────────────────────────

// expiredClaim is one lapsed hold.
type expiredClaim struct {
	uuid        uuid.UUID
	teamUUID    uuid.UUID
	projectUUID uuid.UUID
	sessionUUID uuid.UUID
	memberUUID  uuid.UUID
	intentUUID  *uuid.UUID
	key         string
}

// expireClaims flips held claims whose TTL (or 4h hard ceiling) has passed.
//
// Read this function with PLAN.md's rule in mind: "TTL expiry (lazy filter is
// authoritative; sweeper is for *visibility*)". The candidate scan behind
// every declare_intent already carries `AND expires_at > NOW()`, so the rows
// this function rewrites had ALREADY stopped producing conflicts. Deleting
// this function would not change one detection result. What it changes is
// that the board stops showing a dead agent holding auth.go, and that
// claim_expired appears in the log where an operator can see it.
//
// The denormalized copy on claim_path is updated in the same breath, because
// that is the column the hot query reads — MODEL.md's "claim_path repeats six
// columns from claim" is a read-path trade, and this is the write that pays
// for it.
func (s *Sweeper) expireClaims(ctx context.Context, now time.Time, rep *Report, budget *eventBudget) error {
	remaining := s.opts.MaxClaimsPerPass
	for remaining > 0 {
		limit := s.opts.BatchSize
		if limit > remaining {
			limit = remaining
		}

		batch, err := s.loadExpiredClaims(ctx, now, limit)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		remaining -= len(batch)

		ids := make([]uuid.UUID, 0, len(batch))
		for _, c := range batch {
			ids = append(ids, c.uuid)
		}

		// Conditional on `status = held`, so a concurrent release, a
		// concurrent revoke, or a second pod's identical UPDATE leaves this
		// one a no-op rather than resurrecting a status somebody else moved
		// on from.
		claims, err := s.execCount(ctx,
			"UPDATE `claim` SET `status` = ?, `updated_at` = ? WHERE `id` IN ("+placeholders(len(ids))+") AND `status` = ?",
			append(append([]any{int64(enums.CLAIM_STATUS_EXPIRED), now}, anySlice(ids)...), int64(enums.CLAIM_STATUS_HELD))...)
		if err != nil {
			return fmt.Errorf("flipping claims to expired: %w", err)
		}
		rep.ClaimsExpired += claims

		paths, err := s.execCount(ctx,
			"UPDATE `claim_path` SET `status` = ?, `updated_at` = ? WHERE `claim_uuid` IN ("+placeholders(len(ids))+") AND `status` = ?",
			append(append([]any{int64(enums.CLAIM_STATUS_EXPIRED), now}, anySlice(ids)...), int64(enums.CLAIM_STATUS_HELD))...)
		if err != nil {
			return fmt.Errorf("flipping claim paths to expired: %w", err)
		}
		rep.ClaimPathsExpired += paths

		// Events last, and only after the rows are right. If the process dies
		// here the claims are still expired; only the telling is missing, and
		// the next pass will not re-tell it because the status no longer
		// matches the scan.
		for _, c := range batch {
			c := c
			payload := payload_entity.EventPayload{
				PreviousStatus: null.StringFrom(enums.ClaimStatus(enums.CLAIM_STATUS_HELD).String()),
				NewStatus:      null.StringFrom(enums.ClaimStatus(enums.CLAIM_STATUS_EXPIRED).String()),
				ClaimUUID:      &c.uuid,
				IntentUUID:     c.intentUUID,
				Message:        null.StringFrom("claim TTL elapsed"),
			}
			s.emit(ctx, rep, budget, sweepEvent{
				teamUUID:       c.teamUUID,
				idempotencyKey: "sweep:claim_expired:" + c.uuid.String(),
				kind:           enums.EVENT_KIND_CLAIM_EXPIRED,
				projectUUID:    &c.projectUUID,
				sessionUUID:    &c.sessionUUID,
				memberUUID:     &c.memberUUID,
				subjectKind:    enums.SUBJECT_KIND_CLAIM,
				subjectUUID:    &c.uuid,
				subjectKey:     c.key,
				summary:        "claim " + c.key + " expired",
				payload:        payload.ToJSON(),
			})
		}

		if len(batch) < limit {
			return nil
		}
	}
	return nil
}

func (s *Sweeper) loadExpiredClaims(ctx context.Context, now time.Time, limit int) ([]expiredClaim, error) {
	// Two conditions, not one. `expires_at` is the TTL a heartbeat keeps
	// pushing forward; `hard_expires_at` is PLAN.md's 4h ceiling that a
	// heartbeat cannot push past. A claim is done when EITHER has passed, and
	// checking only the first would let a session that heartbeats forever
	// hold a path for the length of the hackathon.
	rows, err := s.db.QueryContext(ctx,
		"SELECT `id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`intent_uuid`,`key` "+
			"FROM `claim` WHERE `status` = ? AND (`expires_at` <= ? OR `hard_expires_at` <= ?) "+
			"ORDER BY `expires_at` LIMIT ?",
		int64(enums.CLAIM_STATUS_HELD), now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []expiredClaim
	for rows.Next() {
		var (
			id, team, project, session, member string
			intent                             sql.NullString
			key                                string
		)
		if err := rows.Scan(&id, &team, &project, &session, &member, &intent, &key); err != nil {
			return nil, err
		}
		c := expiredClaim{key: key}
		var err error
		if c.uuid, err = uuid.FromString(id); err != nil {
			continue
		}
		if c.teamUUID, err = uuid.FromString(team); err != nil {
			continue
		}
		c.projectUUID, _ = uuid.FromString(project)
		c.sessionUUID, _ = uuid.FromString(session)
		c.memberUUID, _ = uuid.FromString(member)
		if intent.Valid && intent.String != "" {
			if iu, err := uuid.FromString(intent.String); err == nil {
				c.intentUUID = &iu
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────
// Sessions
// ─────────────────────────────────────────────

type liveSession struct {
	uuid        uuid.UUID
	projectUUID uuid.UUID
	agentUUID   uuid.UUID
	memberUUID  uuid.UUID
	key         string
	status      enums.SessionStatus
	age         time.Duration
}

// sweepSessions walks a team's live and stale sessions and moves the ones
// that stopped heartbeating.
//
// PLAN.md's numbers: live → stale at 180s without a heartbeat, stale →
// abandoned at 600s. Both are overridable per team through team.settings.
//
// Two notes on what this does and does not emit:
//
// A session going STALE gets no event. The generated event_kind enum has
// session_started, session_ended and session_abandoned and no
// session_stale — and event_kind is generated from the nuzur schema, so
// inventing one here would mean editing generated code. Staleness is a
// derived, reversible display state that the board can compute from
// last_heartbeat_at, which it already has; abandonment is the terminal one,
// and that one gets its event.
//
// A session going ABANDONED also drops its held claims. Their TTL would lapse
// on its own within minutes, so this is not load-bearing either — it exists
// so that a session written off after ten minutes does not keep a 4h-ceiling
// claim alive on the board for the rest of the afternoon.
func (s *Sweeper) sweepSessions(ctx context.Context, t teamRow, now time.Time, rep *Report, budget *eventBudget) error {
	candidates, err := s.loadDueSessions(ctx, t, now)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}

	var toStale, toAbandon []liveSession
	for _, c := range candidates {
		switch {
		case c.age >= t.abandonedAfter:
			// Straight from live to abandoned is legitimate: a pass that was
			// down for twenty minutes should not have to walk a session
			// through stale first, one pass at a time.
			toAbandon = append(toAbandon, c)
		case c.status == enums.SESSION_STATUS_LIVE && c.age >= t.staleAfter:
			toStale = append(toStale, c)
		}
	}

	if len(toStale) > 0 {
		ids := sessionIDs(toStale)
		n, err := s.execCount(ctx,
			"UPDATE `session` SET `status` = ?, `updated_at` = ? WHERE `id` IN ("+placeholders(len(ids))+") AND `status` = ?",
			append(append([]any{int64(enums.SESSION_STATUS_STALE), now}, anySlice(ids)...), int64(enums.SESSION_STATUS_LIVE))...)
		if err != nil {
			return fmt.Errorf("marking sessions stale: %w", err)
		}
		rep.SessionsStale += n
	}

	if len(toAbandon) > 0 {
		ids := sessionIDs(toAbandon)
		args := []any{
			int64(enums.SESSION_STATUS_ABANDONED),
			int64(enums.SESSION_OUTCOME_ABANDONED),
			now, now,
		}
		args = append(args, anySlice(ids)...)
		args = append(args, int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE))
		n, err := s.execCount(ctx,
			"UPDATE `session` SET `status` = ?, `outcome` = ?, `ended_at` = COALESCE(`ended_at`, ?), `updated_at` = ? "+
				"WHERE `id` IN ("+placeholders(len(ids))+") AND `status` IN (?,?)",
			args...)
		if err != nil {
			return fmt.Errorf("marking sessions abandoned: %w", err)
		}
		rep.SessionsAbandoned += n

		if err := s.dropClaimsOfSessions(ctx, ids, now, rep); err != nil {
			rep.addErr("dropping the claims of abandoned sessions", err)
		}

		for _, c := range toAbandon {
			c := c
			payload := payload_entity.EventPayload{
				PreviousStatus: null.StringFrom(c.status.String()),
				NewStatus:      null.StringFrom(enums.SessionStatus(enums.SESSION_STATUS_ABANDONED).String()),
				Message:        null.StringFrom("no heartbeat"),
				Detail: null.StringFrom(fmt.Sprintf("last heartbeat %s ago, past the %s abandon threshold",
					c.age.Round(time.Second), t.abandonedAfter)),
			}
			s.emit(ctx, rep, budget, sweepEvent{
				teamUUID:       t.uuid,
				idempotencyKey: "sweep:session_abandoned:" + c.uuid.String(),
				kind:           enums.EVENT_KIND_SESSION_ABANDONED,
				// Structural: a session leaving the board changes its shape,
				// which is board_revision's meaning — "re-layout", as opposed
				// to sequence's "you missed something".
				structural:  true,
				projectUUID: &c.projectUUID,
				sessionUUID: &c.uuid,
				agentUUID:   &c.agentUUID,
				memberUUID:  &c.memberUUID,
				subjectKind: enums.SUBJECT_KIND_SESSION,
				subjectUUID: &c.uuid,
				subjectKey:  c.key,
				summary:     "session " + c.key + " abandoned after no heartbeat",
				payload:     payload.ToJSON(),
			})
		}
	}
	return nil
}

// loadDueSessions returns this team's live/stale sessions whose last sign of
// life is older than the stale threshold.
//
// COALESCE(last_heartbeat_at, started_at, created_at) rather than
// last_heartbeat_at alone: a session that was created and never heartbeated
// once — an agent that crashed on its first loop — has a NULL there, and
// comparing NULL to anything is NULL, so it would sit on the board as "live"
// forever.
func (s *Sweeper) loadDueSessions(ctx context.Context, t teamRow, now time.Time) ([]liveSession, error) {
	cutoff := now.Add(-t.staleAfter)
	rows, err := s.db.QueryContext(ctx,
		"SELECT `id`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`status`,"+
			"COALESCE(`last_heartbeat_at`, `started_at`, `created_at`) AS `seen_at` "+
			"FROM `session` WHERE `team_uuid` = ? AND `status` IN (?,?) "+
			"AND COALESCE(`last_heartbeat_at`, `started_at`, `created_at`) <= ? "+
			"ORDER BY `seen_at` LIMIT ?",
		t.uuid.String(), int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE),
		cutoff, s.opts.MaxSessionsPerTeam)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []liveSession
	for rows.Next() {
		var (
			id, project, agent, member, key string
			status                          int64
			seenAt                          sql.NullTime
		)
		if err := rows.Scan(&id, &project, &agent, &member, &key, &status, &seenAt); err != nil {
			return nil, err
		}
		c := liveSession{key: key, status: enums.SessionStatus(status)}
		var err error
		if c.uuid, err = uuid.FromString(id); err != nil {
			continue
		}
		c.projectUUID, _ = uuid.FromString(project)
		c.agentUUID, _ = uuid.FromString(agent)
		c.memberUUID, _ = uuid.FromString(member)
		if seenAt.Valid {
			c.age = now.Sub(seenAt.Time.UTC())
		} else {
			// No timestamp at all anywhere. Treat it as maximally old: a row
			// that cannot say when it was last alive is not alive.
			c.age = t.abandonedAfter
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// dropClaimsOfSessions expires the held claims of sessions just written off.
func (s *Sweeper) dropClaimsOfSessions(ctx context.Context, ids []uuid.UUID, now time.Time, rep *Report) error {
	if len(ids) == 0 {
		return nil
	}
	n, err := s.execCount(ctx,
		"UPDATE `claim` SET `status` = ?, `updated_at` = ? WHERE `session_uuid` IN ("+placeholders(len(ids))+") AND `status` = ?",
		append(append([]any{int64(enums.CLAIM_STATUS_EXPIRED), now}, anySlice(ids)...), int64(enums.CLAIM_STATUS_HELD))...)
	if err != nil {
		return err
	}
	rep.ClaimsExpiredWithSession += n

	paths, err := s.execCount(ctx,
		"UPDATE `claim_path` SET `status` = ?, `updated_at` = ? WHERE `session_uuid` IN ("+placeholders(len(ids))+") AND `status` = ?",
		append(append([]any{int64(enums.CLAIM_STATUS_EXPIRED), now}, anySlice(ids)...), int64(enums.CLAIM_STATUS_HELD))...)
	if err != nil {
		return err
	}
	rep.ClaimPathsExpired += paths
	return nil
}

func sessionIDs(in []liveSession) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(in))
	for _, s := range in {
		out = append(out, s.uuid)
	}
	return out
}

// execCount runs a statement outside any transaction and returns the rows it
// changed.
//
// Outside a transaction on purpose: each of these is a single bounded
// statement, and autocommit releases its row locks the moment it returns.
// Wrapping a pass's worth of them in one transaction would hold every lock
// until the end of the pass, which is precisely the thing a background job
// must never do to a table agents are writing to.
func (s *Sweeper) execCount(ctx context.Context, query string, args ...any) (int, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
