package sweeper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Retention
// ─────────────────────────────────────────────
//
// This is the only part of the sweeper that destroys information, so the
// rules it follows are written out rather than implied:
//
//  1. OFF by default. Options.RetentionEnabled is the operator's switch and
//     nothing defaults it to true. metiche is self-hostable; a self-hoster
//     who never asked for a retention policy must never lose an hour of
//     history because a plan row they never looked at carried a number.
//
//  2. A team with no retention_days keeps everything, forever. NULL means
//     forever in the plan table and it means forever here.
//
//  3. Events and closed sessions are deletable. Decisions and contracts are
//     NOT — not the decision, not its paths or tokens, not the contract, its
//     assertions or its fields. Those are standing agreements, not history:
//     "auth is a JWT in an httpOnly cookie" does not become less true because
//     it was agreed ninety days ago, and if it silently vanished the team
//     would go on believing it had been agreed while every new agent saw
//     nothing. That is a change to what the team believes it agreed, which is
//     not a thing a cleanup job gets to do.
//
//  4. Bounded batches. Never one unbounded DELETE: a single statement
//     removing a month of a busy team's events holds row locks across that
//     whole range while agents are trying to append to it.
//
//  5. The team sequence lock is never taken. Every DELETE runs in autocommit
//     against team_event and session — neither of which is the team row — and
//     the floor update at the end is one single-row UPDATE with no FOR UPDATE
//     and no surrounding transaction. A tool call can never queue behind a
//     retention pass for longer than one statement.

// enforceRetention runs the policy for one team.
func (s *Sweeper) enforceRetention(ctx context.Context, t teamRow, now time.Time, defaultDays sql.NullInt64, rep *Report) error {
	if !s.opts.RetentionEnabled {
		// Not "skip the deletes but still stamp the row": with enforcement
		// off the sweeper writes nothing at all, so an operator turning it on
		// later can tell from last_retention_sweep_at being NULL that it had
		// never run.
		return nil
	}
	rep.Retention.TeamsConsidered++

	days, err := s.retentionDaysFor(ctx, t, defaultDays)
	if err != nil {
		return err
	}
	if !days.Valid || days.Int64 <= 0 {
		return nil // forever
	}

	// Rate-limited by what is in the database rather than by a timer in this
	// process, so it is correct across restarts and across pods.
	if s.opts.RetentionInterval > 0 && t.lastSweepAt.Valid {
		if now.Sub(t.lastSweepAt.Time.UTC()) < s.opts.RetentionInterval {
			return nil
		}
	}
	rep.Retention.TeamsEnforced++

	cutoff := now.Add(-time.Duration(days.Int64) * 24 * time.Hour)

	events, batches, err := s.deleteOldEvents(ctx, t, cutoff)
	rep.Retention.EventsDeleted += events
	rep.Retention.Batches += batches
	if err != nil {
		// Still advance the floor below: whatever WAS deleted is gone, and a
		// floor that does not match the disk is worse than a partial sweep.
		rep.addErr("deleting expired events", err)
	}

	sessions, sbatches, serr := s.deleteClosedSessions(ctx, t, cutoff)
	rep.Retention.SessionsDeleted += sessions
	rep.Retention.Batches += sbatches
	if serr != nil {
		rep.addErr("deleting closed sessions", serr)
	}

	advanced, ferr := s.advanceFloor(ctx, t, now)
	if ferr != nil {
		return ferr
	}
	if advanced {
		rep.Retention.FloorsAdvanced++
	}
	if err != nil {
		return err
	}
	return serr
}

// retentionDaysFor resolves the team's policy: its own plan, or the
// instance's default plan when it has none.
func (s *Sweeper) retentionDaysFor(ctx context.Context, t teamRow, defaultDays sql.NullInt64) (sql.NullInt64, error) {
	if t.planUUID == nil {
		return defaultDays, nil
	}
	var days sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT `retention_days` FROM `plan` WHERE `id` = ? AND `status` = ?",
		t.planUUID.String(), int64(enums.RECORD_STATUS_ACTIVE)).Scan(&days)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The plan row is gone or inactive. Fall back to the instance
		// default; do NOT invent a policy.
		return defaultDays, nil
	case err != nil:
		return sql.NullInt64{}, err
	}
	return days, nil
}

// instanceDefaultRetentionDays reads the plan marked is_instance_default,
// which is what applies to a team that has no plan of its own.
func (s *Sweeper) instanceDefaultRetentionDays(ctx context.Context) (sql.NullInt64, error) {
	var days sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT `retention_days` FROM `plan` WHERE `is_instance_default` = 1 AND `status` = ? "+
			"ORDER BY `sort_order` LIMIT 1", int64(enums.RECORD_STATUS_ACTIVE)).Scan(&days)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No default plan configured anywhere: nothing is deletable, which is
		// the right answer for a fresh self-hosted instance.
		return sql.NullInt64{}, nil
	case err != nil:
		return sql.NullInt64{}, err
	}
	return days, nil
}

// deleteOldEvents removes the team's event log below the cutoff, in batches.
//
// ORDER BY sequence so the oldest go first and the deletion walks the
// (team_uuid, sequence) unique index in order — which also means an
// interrupted run leaves a contiguous log with a higher floor, never a hole
// in the middle.
func (s *Sweeper) deleteOldEvents(ctx context.Context, t teamRow, cutoff time.Time) (deleted, batches int, err error) {
	for batches < s.opts.MaxBatches {
		if ctx.Err() != nil {
			return deleted, batches, ctx.Err()
		}
		n, err := s.execCount(ctx,
			"DELETE FROM `team_event` WHERE `team_uuid` = ? AND `occurred_at` < ? ORDER BY `sequence` LIMIT ?",
			t.uuid.String(), cutoff, s.opts.BatchSize)
		if err != nil {
			return deleted, batches, err
		}
		batches++
		deleted += n
		if n < s.opts.BatchSize {
			return deleted, batches, nil
		}
	}
	// Out of batch budget with work left. Not an error: the next pass picks
	// it up, and yielding is the point of the budget.
	return deleted, batches, nil
}

// deleteClosedSessions removes ended and abandoned sessions past the cutoff.
//
// Only closed ones, ever: a live or stale session is current state, not
// history, and deleting one out from under an agent that is about to
// heartbeat would fail its next call rather than tidy anything.
//
// The children go with it through the schema's own ON DELETE CASCADE —
// intents, their tokens, claims and their paths. Contracts and decisions do
// NOT cascade from a session (their foreign keys hang off `contract` and
// `team`), which is not an accident of the schema so much as the schema
// agreeing with rule 3 above.
func (s *Sweeper) deleteClosedSessions(ctx context.Context, t teamRow, cutoff time.Time) (deleted, batches int, err error) {
	for batches < s.opts.MaxBatches {
		if ctx.Err() != nil {
			return deleted, batches, ctx.Err()
		}
		n, err := s.execCount(ctx,
			"DELETE FROM `session` WHERE `team_uuid` = ? AND `status` IN (?,?) "+
				"AND COALESCE(`ended_at`, `updated_at`) < ? LIMIT ?",
			t.uuid.String(),
			int64(enums.SESSION_STATUS_ENDED), int64(enums.SESSION_STATUS_ABANDONED),
			cutoff, s.opts.BatchSize)
		if err != nil {
			return deleted, batches, err
		}
		batches++
		deleted += n
		if n < s.opts.BatchSize {
			return deleted, batches, nil
		}
	}
	return deleted, batches, nil
}

// advanceFloor moves team.retention_floor_sequence to the lowest sequence
// still on disk, and stamps last_retention_sweep_at.
//
// This is the load-bearing half of retention, and the reason is the SSE
// stream. A board reconnects with ?after=N and is handed everything above N.
// If the events above N were deleted, that request looks exactly like a quiet
// team — the client gets an empty batch, believes it is caught up, and renders
// a board with a silent hole in it. The floor is how the endpoint can answer
// "you are below the floor, reload from scratch" instead.
//
// Three deliberate choices in one statement:
//
//   - MIN(sequence) is computed by the database AS PART OF the update, not
//     read into Go first. A value read a moment ago could be stale the
//     instant another pod deletes one more batch, and a floor lower than the
//     truth is the one error that matters: it tells a client its cursor is
//     safe when the events it needs are gone.
//
//   - GREATEST means the floor never moves backwards. Two pods writing
//     different values converge on the higher one, and the loser is harmless.
//
//   - COALESCE to sequence+1 covers the empty log: nothing at or below the
//     current sequence remains, so the first cursor that could still be
//     served is the next event that will be written.
//
// It takes the team row's lock for the length of one single-row UPDATE — no
// transaction, no FOR UPDATE, nothing held across the deletes above.
func (s *Sweeper) advanceFloor(ctx context.Context, t teamRow, now time.Time) (bool, error) {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE `team` SET "+
			"`retention_floor_sequence` = GREATEST(`retention_floor_sequence`, "+
			"COALESCE((SELECT MIN(e.`sequence`) FROM `team_event` e WHERE e.`team_uuid` = `team`.`id`), `team`.`sequence` + 1)), "+
			"`last_retention_sweep_at` = ?, `updated_at` = ? WHERE `id` = ?",
		now, now, t.uuid.String()); err != nil {
		return false, fmt.Errorf("advancing the retention floor: %w", err)
	}

	var floor int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT `retention_floor_sequence` FROM `team` WHERE `id` = ?", t.uuid.String()).Scan(&floor); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return floor > t.retentionFloor, nil
}
