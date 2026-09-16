package sweeper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"

	team_settings_entity "github.com/mklfarha/metiche/backend/entity/team_settings"
	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Team settings
// ─────────────────────────────────────────────

type teamSettings struct {
	staleSeconds     int64
	abandonedSeconds int64
	notifyFloor      enums.ConflictSeverity
	demotedRules     []string
}

// parseTeamSettings reads the per-team overrides out of the settings JSON
// through the generated entity, so a field renamed in nuzur breaks the build
// here instead of silently reverting a team to the defaults.
func parseTeamSettings(raw []byte) teamSettings {
	e := team_settings_entity.TeamSettingsFromJSON(raw)
	out := teamSettings{notifyFloor: e.NotifyFloor, demotedRules: e.DemotedRules}
	if e.SessionStaleSeconds.Valid {
		out.staleSeconds = e.SessionStaleSeconds.Int64
	}
	if e.SessionAbandonedSeconds.Valid {
		out.abandonedSeconds = e.SessionAbandonedSeconds.Int64
	}
	return out
}

// ─────────────────────────────────────────────
// Appending an event
// ─────────────────────────────────────────────

// sweepEvent is one line the sweeper wants in the team's log.
//
// The sweeper cannot reuse app/mcp's commit(): that function is unexported,
// it is built around a tool call's response envelope and idempotency-key
// replay, and a background pass has neither. What it DOES reuse is the shape
// of the transaction, because the team row is the sequence lock for the whole
// system and there is exactly one correct way to advance it.
type sweepEvent struct {
	teamUUID uuid.UUID

	// idempotencyKey is derived from what happened — the claim's uuid, the
	// session's uuid, the conflict's dedupe key — and never from the clock.
	// That is what makes two pods racing the same pass write one event:
	// uq_team_event_idempotency turns the loser into a no-op.
	idempotencyKey string

	kind       enums.EventKind
	structural bool

	projectUUID *uuid.UUID
	sessionUUID *uuid.UUID
	agentUUID   *uuid.UUID
	memberUUID  *uuid.UUID

	subjectKind enums.SubjectKind
	subjectUUID *uuid.UUID
	subjectKey  string

	summary string
	payload []byte

	// extra runs inside the same transaction, after the lock is taken and
	// before the event is written, and is handed the sequence the event will
	// carry so it can mint short keys (CF-17) from it. It is how the
	// contract_unclaimed detector writes its conflict, its participant and
	// its instruction under the same lock that orders the event describing
	// them.
	extra func(ctx context.Context, tx *sql.Tx, seq int64, now time.Time) error

	// finish, when set, runs after extra and fills in the summary, subject key
	// and payload from what extra decided under the lock. It exists for the
	// conflict_resolved event, whose explanation is only known once the
	// conflict has been re-evaluated inside the transaction.
	finish func() (summary, subjectKey string, payload []byte)
}

// appendEvent writes one event and advances the team's cursors.
//
//	BEGIN
//	  SELECT sequence, board_revision FROM team WHERE id = ? FOR UPDATE
//	  extra()                        — the caller's rows, if any
//	  finish()                       — what extra decided, in words
//	  INSERT team_event              — sequence = last + 1
//	  UPDATE team SET sequence[, board_revision]
//	COMMIT
//
// The lock is held for the length of those statements and nothing else. No
// scan, no classification and no DELETE happens inside it: by the time
// appendEvent is called, the sweeper already knows exactly what it wants to
// say. That is the difference between a background pass that is invisible to
// agents and one that shows up as latency on every tool call.
//
// Returns false when the event was already written — by an earlier pass, or
// by another pod half a millisecond ago — which is a normal outcome and not
// an error.
func (s *Sweeper) appendEvent(ctx context.Context, ev sweepEvent) (bool, error) {
	if ev.idempotencyKey == "" {
		return false, errors.New("sweeper event without an idempotency key")
	}
	if len(ev.idempotencyKey) > 255 {
		return false, fmt.Errorf("idempotency key is %d characters, the column holds 255", len(ev.idempotencyKey))
	}

	// Outside the lock: if this event already exists there is nothing to
	// serialize and no reason to queue behind the team's agents.
	var exists int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM `team_event` WHERE `team_uuid` = ? AND `idempotency_key` = ?",
		ev.teamUUID.String(), ev.idempotencyKey).Scan(&exists)
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var lastSeq, boardRev int64
	if err := tx.QueryRowContext(ctx,
		"SELECT `sequence`, `board_revision` FROM `team` WHERE `id` = ? FOR UPDATE",
		ev.teamUUID.String()).Scan(&lastSeq, &boardRev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The team was deleted between the scan and here. Nothing to say.
			return false, nil
		}
		return false, err
	}

	seq := lastSeq + 1
	rev := boardRev
	if ev.structural {
		rev++
	}
	now := s.now()

	if ev.extra != nil {
		if err := ev.extra(ctx, tx, seq, now); err != nil {
			if errors.Is(err, errNothingToSay) {
				// Decided under the lock that there is nothing to record.
				// Rolled back by the defer, with no sequence consumed.
				return false, nil
			}
			if isDuplicateKey(err) {
				// Another pod got there first. Roll back without consuming a
				// sequence number — a gap in the log would make an SSE client
				// wait forever for an event that is never coming.
				return false, nil
			}
			return false, err
		}
	}

	// What extra decided under the lock, in the words the board reads. It runs
	// here — after extra, before the INSERT — because that is the only moment
	// when the decision has been made and the row does not yet exist. One
	// call site for all three settle paths, because all three learn what they
	// have to say at exactly this point.
	if ev.finish != nil {
		ev.summary, ev.subjectKey, ev.payload = ev.finish()
	}

	evUUID, err := uuid.NewV4()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO `team_event` "+
			"(`id`,`team_uuid`,`sequence`,`project_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
			"`kind`,`subject_kind`,`subject_uuid`,`subject_key`,`structural`,`summary`,`payload`,"+
			"`idempotency_key`,`occurred_at`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		evUUID.String(), ev.teamUUID.String(), seq,
		uuidArg(ev.projectUUID), uuidArg(ev.sessionUUID), uuidArg(ev.agentUUID), uuidArg(ev.memberUUID),
		int64(ev.kind), nullableEnum(ev.subjectKind), uuidArg(ev.subjectUUID), nullableString(truncate(ev.subjectKey, 64)),
		ev.structural, nullableString(truncate(ev.summary, 240)), nullableBytes(ev.payload),
		ev.idempotencyKey, now, now, now,
	); err != nil {
		if isDuplicateKey(err) {
			return false, nil
		}
		return false, err
	}

	// Only the two cursors, never a full-row update: the team row also holds
	// the plan, the visibility and the retention floor, and writing back
	// values this transaction read would clobber a concurrent change to any
	// of them.
	if _, err := tx.ExecContext(ctx,
		"UPDATE `team` SET `sequence` = ?, `board_revision` = ?, `updated_at` = ? WHERE `id` = ?",
		seq, rev, now, ev.teamUUID.String()); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		if isDuplicateKey(err) {
			return false, nil
		}
		return false, err
	}
	committed = true
	return true, nil
}

// emit is appendEvent plus the pass's event budget and the report's counters,
// which is what every caller actually wants.
func (s *Sweeper) emit(ctx context.Context, rep *Report, budget *eventBudget, ev sweepEvent) {
	if !budget.take() {
		return
	}
	wrote, err := s.appendEvent(ctx, ev)
	switch {
	case err != nil:
		rep.addErr("appending "+ev.kind.String()+" event", err)
	case wrote:
		rep.EventsEmitted++
	default:
		rep.EventsSkippedForDupe++
	}
}

// ─────────────────────────────────────────────
// SQL argument helpers
// ─────────────────────────────────────────────

func uuidArg(u *uuid.UUID) any {
	if u == nil || u.IsNil() {
		return nil
	}
	return u.String()
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func nullableEnum(k enums.SubjectKind) any {
	if k == enums.SUBJECT_KIND_INVALID {
		return nil
	}
	return int64(k)
}
