package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

// Envelope is what EVERY tool returns. Deliberately tiny, and deliberately the
// same shape from every tool.
//
// What is NOT in here is the point of it. No board, no list of everyone's
// claims, no team roster. This struct goes in front of a language model on
// every single call an agent makes — a payload that grows with team size would
// cost every agent tokens for work that is not theirs, and would train the
// model to skim tool output, which is unrecoverable.
type Envelope struct {
	OK bool `json:"ok"`

	// Key is the short human-facing key of whatever the call was about
	// (A-4, S-17). Empty on calls that are about nothing in particular.
	Key string `json:"key,omitempty"`

	// ProjectKey is the project start_session actually put the session on,
	// which is not always the project_key the agent sent: the repository
	// decides. Empty, and so absent, on every other tool — which keeps their
	// bytes, and every snapshot stored before this field existed, unchanged.
	ProjectKey string `json:"project_key,omitempty"`

	// Sequence is the team's event cursor. It advances on EVERY event.
	// A client uses it to notice it missed something.
	Sequence int64 `json:"sequence"`

	// Revision is the board's structural cursor. It advances only when the
	// SHAPE of the board changed — a member, agent or session appeared. A
	// status change moves Sequence and leaves Revision alone, which is exactly
	// the difference between "repaint" and "re-layout".
	Revision int64 `json:"revision"`

	// Pending is the entire push mechanism. MCP is request/response: nothing
	// can be pushed to an agent, so a COUNT of what is waiting rides on every
	// response and the agent fetches the contents only when it is non-zero.
	//
	// A count rather than the content, on purpose: fetching the content is what
	// marks an instruction delivered, and silently acknowledging one inside an
	// unrelated call would tell the person who raised it that the agent had
	// seen it when it had not.
	Pending Pending `json:"pending"`

	// Note is one short line of prose for the model. Never a report.
	Note string `json:"note,omitempty"`

	// Conflicts carries collisions this very call caused, detected inside the
	// same transaction that wrote the event. The later declarer is told
	// synchronously because it has the information in hand and has not started
	// yet; the earlier declarer learns asynchronously through Pending.
	Conflicts []ConflictNotice `json:"conflicts,omitempty"`

	// Review is the semantic half — candidate decisions and similar intents for
	// the caller's own model to judge. Populated by the detection layer, absent
	// in the vast majority of calls.
	Review json.RawMessage `json:"review,omitempty"`
}

// Pending is the three counts an agent needs to decide whether to make another
// call. Serialized always, zeros included: an absent field reads to a model as
// "unknown", and "unknown" invites a needless fetch.
type Pending struct {
	Instructions int `json:"instructions"`
	Conflicts    int `json:"conflicts"`
	Reviews      int `json:"reviews"`
}

// Any reports whether anything is waiting.
func (p Pending) Any() bool { return p.Instructions > 0 || p.Conflicts > 0 || p.Reviews > 0 }

// NoteForPending is the one-line nudge appended to a response that has work
// waiting. Written for a model deciding what to call next, so it names the
// tool - and names none when no tool can serve it yet, because a model told to
// call a tool that is not registered either errors or invents one.
func (p Pending) NoteForPending() string {
	switch {
	case p.Instructions > 0 && p.Conflicts > 0:
		return fmt.Sprintf("%d instruction(s) and %d open conflict(s) involve you — call get_instructions",
			p.Instructions, p.Conflicts)
	case p.Instructions > 0:
		return fmt.Sprintf("%d instruction(s) waiting — call get_instructions", p.Instructions)
	case p.Conflicts > 0:
		return fmt.Sprintf("%d open conflict(s) involve you — call get_instructions", p.Conflicts)
	case p.Reviews > 0:
		// Judging pairs is not built: no tool serves a review yet. The count
		// stays in the envelope so the shape does not change when it is.
		return fmt.Sprintf("%d pair(s) assigned to you to judge — judging is not available yet, so there is nothing to call; carry on", p.Reviews)
	}
	return ""
}

// ConflictNotice is one collision, told to the agent that just caused it.
//
// Never surfaced without SuggestedAction: "you and Ana both hold auth.go" is
// noise; "Ana holds auth.go (write, 4m, feat/auth); consider consuming her
// POST /api/login contract instead" is signal, and the difference is whether
// the agent can act on it without asking anybody.
type ConflictNotice struct {
	Key             string   `json:"key"`
	Kind            string   `json:"kind"`
	Severity        string   `json:"severity"`
	With            string   `json:"with,omitempty"`
	Paths           []string `json:"paths,omitempty"`
	SuggestedAction string   `json:"suggested_action"`
}

// withPending attaches the counts and, when something is waiting and the caller
// left Note empty, the standard nudge.
func (e Envelope) withPending(p Pending) Envelope {
	e.Pending = p
	if e.Note == "" {
		e.Note = p.NoteForPending()
	}
	return e
}

// marshalEnvelope renders the envelope exactly once, so the bytes returned to
// the caller and the bytes stored in team_event.response_snapshot for a future
// replay are the same bytes.
//
// That identity is the whole idempotency contract: a retried call replays the
// stored snapshot VERBATIM, including whatever conflicts the caller was told
// about the first time. Rebuilding the response on replay would quietly drop
// them, and the retry would look cleaner than the original.
func marshalEnvelope(e Envelope) (json.RawMessage, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("rendering the response envelope: %w", err)
	}
	return json.RawMessage(b), nil
}

// ─────────────────────────────────────────────
// Pending counts
// ─────────────────────────────────────────────

// pendingCounts answers "is anything waiting for this session?" in three
// index-only COUNT(*) queries.
//
// Each one is served by an index whose leading column is the session
// (idx_instruction_pending, idx_participant_session, idx_judgement_assignment),
// so none of them scans: the row set is this session's own waiting work, which
// is bounded by design. That is what makes it safe to run inside the team lock
// in commit(), where the counts have to be taken AFTER the detection hook or a
// conflict this very call raised would be missing from its own response.
//
// q is either the pool or the open transaction. Inside commit it must be the
// transaction, so the count sees rows the detection hook just inserted.
func (h *Handler) pendingCounts(ctx context.Context, q queryer, sessionUUID *uuid.UUID) (Pending, error) {
	var p Pending
	if sessionUUID == nil || sessionUUID.IsNil() {
		return p, nil
	}
	id := sessionUUID.String()
	now := time.Now().UTC()

	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ? AND `status` = ? AND (`expires_at` IS NULL OR `expires_at` > ?)",
		id, enums.INSTRUCTION_STATUS_PENDING, now).Scan(&p.Instructions); err != nil {
		return Pending{}, err
	}

	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `conflict_participant` p JOIN `conflict` c ON c.`id` = p.`conflict_uuid` "+
			"WHERE p.`session_uuid` = ? AND c.`status` IN (?, ?)",
		id, enums.CONFLICT_STATUS_OPEN, enums.CONFLICT_STATUS_RESOLVING).Scan(&p.Conflicts); err != nil {
		return Pending{}, err
	}

	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `judgement` WHERE `judge_session_uuid` = ? AND `status` = ? AND (`judging_expires_at` IS NULL OR `judging_expires_at` > ?)",
		id, enums.JUDGEMENT_STATUS_PENDING, now).Scan(&p.Reviews); err != nil {
		return Pending{}, err
	}

	return p, nil
}

// queryer is the shared read surface of *sql.DB and *sql.Tx, so the same count
// query runs inside the team transaction and outside it (heartbeat) without a
// second copy that could drift from this one.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
