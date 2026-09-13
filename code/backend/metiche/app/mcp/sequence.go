package mcp

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	team_eventmod "github.com/mklfarha/metiche/backend/core/module/team_event"
	team_event_types "github.com/mklfarha/metiche/backend/core/module/team_event/types"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	team_event_entity "github.com/mklfarha/metiche/backend/entity/team_event"
	"github.com/mklfarha/metiche/backend/enums"
)

// TxContext is what runs inside the team lock get to see.
//
// It carries the open transaction and — the part that matters — the sequence
// the pending event WILL carry, decided before anything is written. Short keys
// (S-17, A-4) are minted from it: the sequence is unique per team by
// construction, so a key derived from it needs no counter table, no extra
// round trip, and cannot collide with a concurrent caller's.
type TxContext struct {
	Tx       *sql.Tx
	TeamUUID uuid.UUID
	// Sequence is the sequence this event will be written with (last + 1).
	Sequence int64
	// Revision is what team.board_revision will be after this event.
	Revision int64
	// Now is one timestamp for the whole transaction, so every row it writes
	// agrees about when it happened.
	Now time.Time

	// Summary, when Apply sets it, replaces Mutation.Summary on the event.
	// It is for a summary that names something only decided under the lock —
	// start_session's project, which the repository picks, not the key sent.
	// The summary is not part of the stored response, so this changes nothing
	// a replay returns.
	Summary string
}

// Key mints a short human-facing key for something created by this event.
// S-17, A-4, M-2 — unique per team because Sequence is.
func (tc *TxContext) Key(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, tc.Sequence)
}

// ApplyHook is the caller's change to the snapshot — the current-state tables
// the event describes. It runs inside the lock, before the event is written,
// and may read its own writes because it is handed the transaction.
//
// It is also handed the response under construction, and that is not a
// convenience. Anything a tool wants to say that it only learns INSIDE the
// transaction — the session key it just minted, whether it had to create the
// project — has to be written onto the envelope here, before commit renders
// it. A field bolted on after commit returns would be missing from the stored
// snapshot, and the retry would answer differently from the original.
type ApplyHook func(ctx context.Context, tc *TxContext, env *Envelope) error

// DetectHook is the seam for the deterministic detection layer.
//
// PLAN.md: "detection and insertion happen inside the same transaction as the
// sequence lock. Check overlap outside the lock and you get exactly the TOCTOU
// race where both agents see a clean world and both insert." So this hook is
// handed the open transaction and may BOTH query the candidate set and insert
// the conflict, participants and instructions it decides on — all under the
// same row lock that orders the events.
//
// Whatever it returns is folded into the response the caller gets back and
// into the snapshot stored for a replay, so a retry tells the agent about the
// same collisions it was told about the first time.
//
// The contract it must honour: no network I/O, and no query that can walk an
// unbounded row set. It is inside the one serialization point in the system.
type DetectHook func(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error)

// Mutation is one atomic change to the team's state: whatever the caller does
// to the snapshot, plus exactly one team_event appended to the log.
type Mutation struct {
	TeamUUID uuid.UUID

	// IdempotencyKey makes a retried tool call a no-op that still answers
	// correctly. Required: uq_team_event_idempotency is what turns a duplicate
	// into a replay instead of a second event, and a mutation without one is a
	// bug rather than a valid call.
	IdempotencyKey string

	Kind enums.EventKind

	// Structural says whether this change alters the SHAPE of the board — a
	// member, agent or session appearing — rather than the state of something
	// already on it. Only a structural change bumps board_revision. Two
	// cursors, two meanings: sequence says "you missed something", revision
	// says "re-layout".
	Structural bool

	ProjectUUID *uuid.UUID
	SessionUUID *uuid.UUID
	AgentUUID   *uuid.UUID
	MemberUUID  *uuid.UUID

	SubjectKind enums.SubjectKind
	SubjectUUID *uuid.UUID
	// SubjectKey is filled by Apply when the subject is created inside the
	// transaction, which is after this struct is built.
	SubjectKey string

	Summary string
	Payload payload_entity.EventPayload

	// Apply mutates the snapshot inside the transaction.
	Apply ApplyHook

	// Detect overrides the handler-wide detection hook for this one call.
	// Normally nil: the detection layer installs itself once with
	// SetDetector.
	Detect DetectHook

	// Envelope is the response under construction. commit fills in Sequence,
	// Revision, Pending and Conflicts; the tool supplies Key and Note. Apply
	// may set Key on it (through the pointer commit passes to Apply's closure)
	// when the key is minted inside the transaction.
	Envelope Envelope

	// Snapshot, when non-nil, is stored in team_event.response_snapshot
	// INSTEAD of the response the caller receives.
	//
	// It exists for exactly one caller: join_team, whose live response carries
	// a freshly minted bearer token. A token must never be persisted, and the
	// event log is persistence. So join_team stores a redacted envelope and
	// opts out of replay entirely — see join.go.
	Snapshot json.RawMessage
}

// ErrNoIdempotencyKey is returned rather than silently generating one: a
// mutation with no key cannot be retried safely, and failing loudly at the
// call site is cheaper than discovering it as a duplicate row in production.
var ErrNoIdempotencyKey = errors.New("idempotency_key is required for every mutating call")

// commit is THE write path. Every mutating tool goes through it and no
// mutating tool writes an event any other way.
//
//	BEGIN
//	  SELECT sequence, board_revision FROM team WHERE id = ? FOR UPDATE
//	  if this idempotency key already has an event → ROLLBACK, replay its
//	    response_snapshot verbatim
//	  Apply   — the caller's snapshot change
//	  Detect  — the detection layer, querying and inserting under the SAME lock
//	  count the caller's pending work (after Detect, so a conflict this call
//	    raised is visible in its own response)
//	  INSERT team_event with sequence = last + 1 and the rendered response
//	  UPDATE team.sequence (+ board_revision when the change is structural)
//	COMMIT
//
// Why the row lock at all: without it two agents read the same last sequence
// and write the same next one. One insert loses to uq_team_event_sequence and
// takes its snapshot change down with it — and, worse, both of them ran their
// detection against a world neither had written to yet, which is the exact
// race the whole design exists to avoid.
//
// The cost of serializing per team is a lock held for single-digit
// milliseconds against a peak of 1–5 event-producing writes per second. The
// rule that keeps it that way: nothing inside the lock may do network I/O or
// walk an unbounded row set. lockHold is the histogram that tells us when that
// rule has been broken.
//
// Returns the JSON the tool should hand back.
func (h *Handler) commit(ctx context.Context, m Mutation) (json.RawMessage, error) {
	if m.TeamUUID.IsNil() {
		return nil, errors.New("team is required for every mutating call")
	}
	if m.IdempotencyKey == "" {
		return nil, ErrNoIdempotencyKey
	}
	if len(m.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("idempotency_key must be at most 255 characters (got %d)", len(m.IdempotencyKey))
	}

	// Fast path, OUTSIDE the lock: a key that already has an event replays
	// without ever touching the team row. This is the hot path for a retrying
	// agent and it must not queue behind everybody else's writes.
	if snap, found, err := replaySnapshot(ctx, h.core.DB(), m.TeamUUID, m.IdempotencyKey); err != nil {
		return nil, retryable(err, "checking the idempotency key")
	} else if found {
		return snap, nil
	}

	tx, err := h.core.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, retryable(err, "opening the transaction")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// ── the lock ────────────────────────────────────────────────────────────
	waitStart := time.Now()
	var lastSeq, boardRev int64
	err = tx.QueryRowContext(ctx,
		"SELECT `sequence`, `board_revision` FROM `team` WHERE `id` = ? FOR UPDATE",
		m.TeamUUID.String()).Scan(&lastSeq, &boardRev)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("team %s no longer exists", m.TeamUUID)
		}
		return nil, retryable(err, "taking the team sequence lock")
	}
	// The lock is HELD from here, not from the BeginTx above: everything
	// before this point was queueing behind other writers, which is a
	// different number and is recorded as one.
	h.lockWait.Observe(time.Since(waitStart))
	lockStart := time.Now()

	// Re-check under the lock. Between the fast path above and acquiring the
	// lock, a concurrent retry of the SAME call may have committed — and
	// without this check both would insert, and the second would die on the
	// unique index after its Apply had already run.
	if snap, found, err := replaySnapshot(ctx, tx, m.TeamUUID, m.IdempotencyKey); err != nil {
		return nil, retryable(err, "checking the idempotency key")
	} else if found {
		// Release the lock immediately; nothing is being written.
		_ = tx.Rollback()
		committed = true
		h.observeLock(lockStart, m.Kind, true)
		return snap, nil
	}

	tc := &TxContext{
		Tx:       tx,
		TeamUUID: m.TeamUUID,
		Sequence: lastSeq + 1,
		Revision: boardRev,
		Now:      time.Now().UTC(),
	}
	if m.Structural {
		tc.Revision++
	}

	if m.Apply != nil {
		if err := m.Apply(ctx, tc, &m.Envelope); err != nil {
			return nil, err
		}
		if tc.Summary != "" {
			m.Summary = tc.Summary
		}
	}

	// Detection runs here: after the snapshot change, before the event, still
	// inside the lock. It can see what this call just wrote and no other
	// agent's uncommitted work, which is what makes "who was first" a decided
	// question rather than a race.
	detect := m.Detect
	if detect == nil {
		detect = h.detect
	}
	if detect != nil {
		notices, err := detect(ctx, tc, &m)
		if err != nil {
			return nil, fmt.Errorf("detection failed: %w", err)
		}
		m.Envelope.Conflicts = append(m.Envelope.Conflicts, notices...)
	}

	// Counted on the transaction, after Detect, so an instruction or conflict
	// this very call raised against the caller shows up in the caller's own
	// pending counts rather than one call later.
	pending, err := h.pendingCounts(ctx, tx, m.SessionUUID)
	if err != nil {
		return nil, retryable(err, "counting pending work")
	}

	m.Envelope.OK = true
	m.Envelope.Sequence = tc.Sequence
	m.Envelope.Revision = tc.Revision
	response, err := marshalEnvelope(m.Envelope.withPending(pending))
	if err != nil {
		return nil, err
	}

	// The stored snapshot is normally the response byte-for-byte. join_team is
	// the one exception and says so explicitly.
	toStore := response
	if m.Snapshot != nil {
		toStore = m.Snapshot
	}
	stored, err := wrapSnapshot(toStore)
	if err != nil {
		return nil, err
	}

	evUUID, err := uuid.NewV4()
	if err != nil {
		return nil, err
	}
	if _, err := h.core.TeamEvent().Insert(ctx, team_event_types.UpsertRequest{
		TeamEvent: team_event_entity.TeamEvent{
			ID:               evUUID,
			TeamUUID:         m.TeamUUID,
			Sequence:         tc.Sequence,
			ProjectUUID:      m.ProjectUUID,
			SessionUUID:      m.SessionUUID,
			AgentUUID:        m.AgentUUID,
			MemberUUID:       m.MemberUUID,
			Kind:             m.Kind,
			SubjectKind:      m.SubjectKind,
			SubjectUUID:      m.SubjectUUID,
			SubjectKey:       nullString(truncate(m.SubjectKey, 64)),
			Structural:       m.Structural,
			Summary:          nullString(truncate(m.Summary, 240)),
			Payload:          m.Payload,
			IdempotencyKey:   m.IdempotencyKey,
			ResponseSnapshot: stored,
			OccurredAt:       tc.Now,
		},
	}, team_eventmod.WithSQLTransaction(tx)); err != nil {
		return nil, retryable(err, "appending the event")
	}

	// Raw UPDATE rather than the generated module: the two cursors are the
	// only columns that may move here, and a full-row update would race a
	// concurrent change to the team's name or settings by writing back the
	// values this transaction read.
	if _, err := tx.ExecContext(ctx,
		"UPDATE `team` SET `sequence` = ?, `board_revision` = ?, `updated_at` = ? WHERE `id` = ?",
		tc.Sequence, tc.Revision, tc.Now, m.TeamUUID.String()); err != nil {
		return nil, retryable(err, "advancing the team cursors")
	}

	if err := tx.Commit(); err != nil {
		return nil, retryable(err, "committing the change")
	}
	committed = true
	h.observeLock(lockStart, m.Kind, false)

	return response, nil
}

// observeLock records the lock hold and complains when one call crosses the
// tripwire, so the offending tool is named in the logs before the p99 does.
func (h *Handler) observeLock(start time.Time, kind enums.EventKind, replayed bool) {
	d := time.Since(start)
	h.lockHold.Observe(d)
	if d >= LockHoldWarnMS*time.Millisecond {
		h.logger.Warn("team lock held too long",
			zap.String("metric", MetricLockHold),
			zap.Duration("held", d),
			zap.String("event_kind", kind.String()),
			zap.Bool("replayed", replayed))
	}
}

// replaySnapshot looks up a previous event for this idempotency key and
// returns the response the caller was given the first time.
//
// Verbatim, deliberately: no "replayed: true" flag, no recomputed counts, no
// re-rendered conflicts. A retry that answers differently from the original is
// not idempotent, it is merely similar — and the difference shows up as an
// agent that "resolved" a conflict it was never told about on the retry.
func replaySnapshot(ctx context.Context, q queryer, teamUUID uuid.UUID, key string) (json.RawMessage, bool, error) {
	var snapshot []byte
	var seq int64
	err := q.QueryRowContext(ctx,
		"SELECT `sequence`, `response_snapshot` FROM `team_event` WHERE `team_uuid` = ? AND `idempotency_key` = ?",
		teamUUID.String(), key).Scan(&seq, &snapshot)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	if raw, ok := unwrapSnapshot(snapshot); ok {
		return raw, true, nil
	}
	if len(snapshot) == 0 {
		// An event with no stored response. Nothing in this package writes
		// one, but the table is append-only and shared, so answer with
		// something honest rather than empty bytes the client cannot parse.
		return json.RawMessage(fmt.Sprintf(
			`{"ok":true,"sequence":%d,"revision":0,"pending":{"instructions":0,"conflicts":0,"reviews":0},"note":"already applied; no response was recorded for this call"}`,
			seq)), true, nil
	}
	return json.RawMessage(snapshot), true, nil
}

// ─────────────────────────────────────────────
// Storing a response so it can be replayed VERBATIM
// ─────────────────────────────────────────────

// snapshotVersion is stamped on every stored response so the format can
// change without a migration: an unrecognised version falls back to treating
// the column as the response itself.
const snapshotVersion = 1

// storedSnapshot is what actually lands in team_event.response_snapshot.
//
// Raw is base64, and that is the whole reason this wrapper exists. MySQL
// PARSES a JSON column: it reorders keys, restyles whitespace, and — the one
// that actually bites — decodes \u003c back to a literal '<'. Storing the
// response as JSON and reading it back therefore returns bytes that are
// equivalent to the original but not equal to it, and "byte-for-byte replay"
// is the one promise idempotency here makes. Base64 has no character JSON
// touches, so it survives the round trip exactly.
//
// Body is the same response parsed, stored alongside purely so a human — or
// the board's run history — can read what an agent was told without decoding
// anything. Nothing reads it back.
type storedSnapshot struct {
	V    int             `json:"v"`
	Raw  string          `json:"raw"`
	Body json.RawMessage `json:"body,omitempty"`
}

func wrapSnapshot(response json.RawMessage) (json.RawMessage, error) {
	wrapped := storedSnapshot{
		V:   snapshotVersion,
		Raw: base64.StdEncoding.EncodeToString(response),
	}
	if json.Valid(response) {
		wrapped.Body = response
	}
	b, err := json.Marshal(wrapped)
	if err != nil {
		return nil, fmt.Errorf("wrapping the response snapshot: %w", err)
	}
	return b, nil
}

// unwrapSnapshot recovers the exact bytes a previous call returned. A column
// that is not one of our wrappers reports false, and the caller falls back.
func unwrapSnapshot(stored []byte) (json.RawMessage, bool) {
	if len(stored) == 0 {
		return nil, false
	}
	var wrapped storedSnapshot
	if err := json.Unmarshal(stored, &wrapped); err != nil {
		return nil, false
	}
	if wrapped.V != snapshotVersion || wrapped.Raw == "" {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(wrapped.Raw)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(raw), true
}
