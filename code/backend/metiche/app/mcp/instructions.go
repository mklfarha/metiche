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
	"go.uber.org/zap"

	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// instructions.go is the other half of the push-without-push mechanism.
//
// MCP cannot push. Everything an agent is ever told out-of-band — a conflict
// raised by somebody else's declaration, a human's stop or steer from the
// board, a request to judge a pair — is written to `instruction` and COUNTED
// on every response the agent gets. get_instructions is how the agent then
// collects the contents, and report_back is how it says what it did, so the
// board stops showing an open loop and the person who raised it sees an
// answer rather than silence.
//
// Without these two the loop dead-ends: every response that raises a conflict
// tells the agent to call get_instructions.

const (
	// InstructionsDefaultLimit is how many instructions one call takes when
	// the caller does not say. Three, because the response has a token
	// budget (see the cut order below) and because an agent handed ten
	// notices at once acts on none of them.
	InstructionsDefaultLimit = 3

	// InstructionsMaxLimit is the ceiling. There is no "all": an unbounded
	// read is the one thing PLAN.md forbids of every tool in this surface,
	// and a session that has accumulated forty notices needs its oldest
	// three, not all forty.
	InstructionsMaxLimit = 10

	// instructionTextChars caps each of the two prose fields. A conflict
	// notice that cannot say what happened in 200 characters is not going to
	// say it in 600.
	instructionTextChars = 200

	// instructionsResponseCharBudget is PLAN.md's hard cap of ~700 tokens
	// expressed in characters, at the conventional ~4 chars/token (so 2800),
	// minus the ~600 the envelope and its note can cost. It bounds the
	// INSTRUCTIONS; the whole response is what has to come in under the cap.
	//
	// The CUT ORDER when a batch does not fit, in the order applied:
	//
	//  1. every body and every suggested action is truncated to
	//     instructionTextChars;
	//  2. paths collapse to the one path the two claims actually meet on —
	//     the full claim patterns stay in the conflict row for the board;
	//  3. items past the budget are NOT delivered at all. They stay PENDING,
	//     they are counted in more_waiting, and the note tells the agent to
	//     call again. That last step is what makes the budget safe: the
	//     response degrades to fewer instructions, never to a truncated one
	//     that an agent would act on half of.
	//
	// One instruction is always delivered, however long it is, or an
	// oversized body would be undeliverable forever.
	instructionsResponseCharBudget = 2200

	// instructionNoteChars bounds the one line of prose on the envelope.
	instructionNoteChars = 400
)

// ─────────────────────────────────────────────
// Registration
// ─────────────────────────────────────────────

// RegisterInstructionTools registers tools 13 and 14 from PLAN.md:
// get_instructions and report_back.
//
// Annotations are explicit, like everywhere else in this package, because
// MCP's destructiveHint DEFAULTS TO TRUE when it is omitted and a client that
// believes these are destructive will gate them behind a confirmation the
// agent cannot give.
func RegisterInstructionTools(s *mcp.Server, h *Handler, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}

	// get_instructions is NOT readOnly, however much it looks like it, and
	// this is the whole reason the annotation is written out here rather than
	// reusing the shared readOnly value: READING AN INSTRUCTION IS WHAT MARKS
	// IT DELIVERED. The read is the delivery receipt, the receipt is a write,
	// and a client that cached this call because it was advertised read-only
	// would silently stop delivering anything.
	//
	// Not idempotent either: the second call deliberately answers differently
	// from the first, because the first one consumed what it returned.
	delivering := &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		IdempotentHint:  false,
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(false),
	}

	// report_back IS idempotent, and by construction rather than by promise:
	// its idempotency key is (instruction, outcome), so the same report twice
	// replays the first answer and appends no second event.
	reporting := &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		IdempotentHint:  true,
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(false),
	}

	addTool(s, h, logger, &mcp.Tool{
		Name: "get_instructions",
		Description: "Collect what is waiting for you: conflict notices raised by somebody else's work, stop/steer nudges from a human watching the board, and requests to judge a pair. " +
			"Call it whenever the pending.instructions count on any response is above zero — that count is the only way you find out, because metiche cannot push to you. " +
			"Each instruction comes with what happened, who else is involved and a suggested next action you can take without asking anybody. " +
			"Reading is what marks an instruction delivered, so it is handed to you ONCE: act on what you get, and call report_back to say what you did. " +
			"Bounded on purpose — it returns a few at a time and tells you how many remain.",
		Annotations: delivering,
	}, h.GetInstructions)

	addTool(s, h, logger, &mcp.Tool{
		Name: "report_back",
		Description: "Say what you did about an instruction: 'done', 'acknowledged', 'refused' or 'blocked', plus one line of why. " +
			"This is what closes the loop — until you report, the board shows the person who raised it an open request and no answer, which is worse than a refusal. " +
			"Refusing is a perfectly good outcome and is far better than silence; say why in the note. " +
			"Safe to retry: reporting the same outcome on the same instruction twice returns the first answer and records nothing new, so it needs no idempotency_key.",
		Annotations: reporting,
	}, h.ReportBack)
}

// ─────────────────────────────────────────────
// Tool: get_instructions
// ─────────────────────────────────────────────

type GetInstructionsParams struct {
	SessionKey string `json:"session_key" jsonschema:"The session_key start_session gave you. You only ever see instructions addressed to your own session."`
	TeamSlug   string `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	Limit      int    `json:"limit,omitempty" jsonschema:"How many to take this call, 1-10. Defaults to 3. Anything left over stays waiting and is reported as more_waiting."`

	IncludeDelivered bool `json:"include_delivered,omitempty" jsonschema:"Also return instructions already delivered to you that you have not reported back on yet. Use it when you lost a response mid-call, or to re-read what you were asked before you report_back. Off by default, because an instruction repeated looks like a new one."`
}

// InstructionsResult is the envelope plus the instructions themselves.
//
// Shaped like TeamStateResult: the envelope is embedded so the pending counts,
// both cursors and the note ride along exactly as they do on every other tool,
// and the payload the caller actually asked for sits next to them.
type InstructionsResult struct {
	Envelope
	Instructions []DeliveredInstruction `json:"instructions"`

	// MoreWaiting is how many are still PENDING after this call took its
	// batch. Told explicitly rather than left to be inferred from the pending
	// count, because "there are more" is an instruction to call again and a
	// model should not have to do arithmetic to receive it.
	MoreWaiting int `json:"more_waiting,omitempty"`

	// Truncated says the batch was cut by the limit or the token budget
	// rather than by there being nothing left.
	Truncated bool `json:"truncated,omitempty"`
}

// DeliveredInstruction is one thing the agent is being told.
//
// Every field here exists to answer one of three questions — what happened,
// who is involved, what do I do about it — and SuggestedAction is never empty.
// PLAN.md's rule is absolute: "you and Ana both hold auth.go" is noise;
// "Ana (claude-ana) on feat/auth holds auth.go — consume her POST /api/login
// contract instead" is signal, and the difference is whether the agent can act
// without asking a human.
type DeliveredInstruction struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`

	// From is who raised it: a teammate's name for a human nudge, "metiche"
	// for something the server detected.
	From string `json:"from,omitempty"`

	// With is the other agent involved, named the way a person would name
	// them: "Ana (claude-ana) on feat/auth".
	With string `json:"with,omitempty"`

	Severity string   `json:"severity,omitempty"`
	Paths    []string `json:"paths,omitempty"`

	// What happened, in one line.
	What string `json:"what"`

	// SuggestedAction is the next thing to do. Never empty — see the type
	// comment. Named to match ConflictNotice.SuggestedAction, because an
	// agent meets both and they mean the same thing.
	SuggestedAction string `json:"suggested_action"`

	// Ref is the conflict this notice is about (CF-7), so the agent can name it
	// in its report_back note and a person can find it on the board. No tool
	// resolves a conflict yet.
	Ref string `json:"ref,omitempty"`

	// ReportBack is true when somebody is waiting for an answer.
	ReportBack bool `json:"report_back,omitempty"`
}

// GetInstructions delivers the instructions waiting for the calling session.
//
// ── Why this is not readOnly ─────────────────────────────────────────────
// Reading is the delivery receipt. The instruction is marked delivered, with
// delivered_at, in the SAME transaction that reads it, which is what lets the
// board show a human "your nudge reached the agent at 14:02" and what stops
// the pending count reporting the same notice forever.
//
// ── Why it does not take the team lock ───────────────────────────────────
// Delivery is not a team event: nothing about the team's shared state
// changed, only this session's copy of its own inbox, and the `instruction`
// row IS the durable record. heartbeat set the same precedent for the same
// reason (PLAN.md: "it takes no team lock, writes no event"). Taking the lock
// here would queue an agent's inbox read behind every writer on the team, and
// the lock's one rule is that nothing inside it may walk a row set it does
// not control.
//
// ── Which failure is preferred, deliberately ─────────────────────────────
// Read and mark happen in one transaction, so the two possible crashes are:
// the transaction never commits and the instruction stays PENDING (it is
// delivered on the next call, nothing is lost); or it commits and the
// response is lost on the way back, and the agent never sees an instruction
// that is now marked delivered.
//
// THE SECOND IS THE ONE WE ACCEPT: at-most-once delivery. Duplicating is
// worse than losing here, because a re-delivered notice is indistinguishable
// from a new one — the agent redoes work it already did and reports back
// twice on a conflict that moved on, which is exactly the crying-wolf failure
// the noise rules exist to prevent. And the loss is not silent: the conflict
// stays open and keeps counting in pending.conflicts, the board shows an
// instruction delivered with no report against it, and include_delivered=true
// hands the agent back anything it was given but never answered.
func (h *Handler) GetInstructions(ctx context.Context, _ *mcp.CallToolRequest, args GetInstructionsParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSessionOnTeam(ctx, args.SessionKey, args.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	// Deliberately NOT requireWorkableSession: an agent that has ended its
	// session must still be able to read what it was told and answer it.
	sess := who.Session

	limit := args.Limit
	if limit <= 0 {
		limit = InstructionsDefaultLimit
	}
	if limit > InstructionsMaxLimit {
		limit = InstructionsMaxLimit
	}

	now := time.Now().UTC()
	tx, err := h.core.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, retryable(err, "opening the delivery transaction")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	waiting, err := selectDeliverable(ctx, tx, sess.ID, now, limit, args.IncludeDelivered)
	if err != nil {
		return nil, nil, err
	}

	items, delivered, err := h.renderInstructions(ctx, tx, sess.ID, waiting)
	if err != nil {
		return nil, nil, err
	}

	// The receipt. delivered_at is COALESCEd so an include_delivered re-read
	// keeps saying when the agent FIRST got it — that timestamp is what a
	// human reads as "it reached them", and moving it on a re-read would
	// rewrite history to look prompter than it was.
	if len(delivered) > 0 {
		ph := make([]string, len(delivered))
		dargs := []any{enums.INSTRUCTION_STATUS_DELIVERED, now, now}
		for i, id := range delivered {
			ph[i] = "?"
			dargs = append(dargs, id)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE `instruction` SET `status` = ?, `delivered_at` = COALESCE(`delivered_at`, ?), "+
				"`updated_at` = ? WHERE `id` IN ("+strings.Join(ph, ",")+")",
			dargs...); err != nil {
			return nil, nil, retryable(err, "marking the instructions delivered")
		}
	}

	// Counted after the receipt, so "more" means what is still waiting for a
	// FUTURE call rather than what was waiting before this one.
	var more int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ? AND `status` = ? "+
			"AND (`expires_at` IS NULL OR `expires_at` > ?)",
		sess.ID.String(), enums.INSTRUCTION_STATUS_PENDING, now).Scan(&more); err != nil {
		return nil, nil, retryable(err, "counting what is still waiting")
	}

	pending, err := h.pendingCounts(ctx, tx, uuidPtr(sess.ID))
	if err != nil {
		return nil, nil, retryable(err, "counting pending work")
	}

	var seq, rev int64
	if err := tx.QueryRowContext(ctx,
		"SELECT `sequence`, `board_revision` FROM `team` WHERE `id` = ?", who.Team.ID.String()).
		Scan(&seq, &rev); err != nil {
		return nil, nil, retryable(err, "reading the team cursors")
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, retryable(err, "recording the delivery")
	}
	committed = true

	// Delivery is when the agent holding the files was actually told about a
	// conflict, so its participant row is marked notified now. AFTER the
	// commit, as its own statement: inside the delivery transaction it would
	// take conflict_participant locks while holding instruction locks — the
	// second lock order selectDeliverable warns about. Best-effort for the
	// same reason: a lost receipt costs a timestamp, never a delivery.
	if err := markConflictNoticesDelivered(ctx, h.core.DB(), sess.ID, waiting, delivered, now); err != nil {
		h.logger.Warn("marking conflict participants notified failed", zap.Error(err))
	}

	out := InstructionsResult{
		Envelope:     Envelope{OK: true, Key: sess.Key, Sequence: seq, Revision: rev},
		Instructions: items,
		MoreWaiting:  more,
		Truncated:    more > 0,
	}
	out.Envelope.Note = deliveryNote(items, more)
	out.Envelope = out.Envelope.withPending(pending)
	return jsonValue(out)
}

// waitingInstruction is the `instruction` row itself, before anything is
// joined onto it.
type waitingInstruction struct {
	id       string
	key      string
	kind     enums.InstructionKind
	source   enums.InstructionSource
	body     string
	report   bool
	refKind  int64
	refUUID  string
	raisedBy string
}

// selectDeliverable takes this session's waiting instructions under a row
// lock.
//
// FOR UPDATE, on the instruction table alone: two concurrent get_instructions
// calls on one session would otherwise both read the same PENDING rows and
// both return them, which is the double delivery this design refuses. The
// lock is taken on `instruction` and nothing else — joining conflict or member
// into a locking read would lock rows the detector writes under the team lock,
// and two lock orders is how a deadlock is built.
//
// Ordered so a person outranks a machine: a human on the board who typed
// "stop" is waiting for an answer, and a conflict notice is not. Oldest first
// within each group, so nothing starves behind a steady drip of new ones.
func selectDeliverable(ctx context.Context, tx *sql.Tx, sessionUUID uuid.UUID, now time.Time, limit int, includeDelivered bool) ([]waitingInstruction, error) {
	q := "SELECT `id`, `key`, `kind`, `source`, `body`, `requires_report`, " +
		"COALESCE(`ref_kind`, 0), COALESCE(`ref_uuid`, ''), COALESCE(`raised_by_member_uuid`, '') " +
		"FROM `instruction` WHERE `target_session_uuid` = ? AND (`expires_at` IS NULL OR `expires_at` > ?) "
	qargs := []any{sessionUUID.String(), now}
	if includeDelivered {
		q += "AND `status` IN (?, ?) "
		qargs = append(qargs, enums.INSTRUCTION_STATUS_PENDING, enums.INSTRUCTION_STATUS_DELIVERED)
	} else {
		q += "AND `status` = ? "
		qargs = append(qargs, enums.INSTRUCTION_STATUS_PENDING)
	}
	q += "ORDER BY (`source` = ?) DESC, `created_at` ASC, `key` ASC LIMIT ? FOR UPDATE"
	qargs = append(qargs, enums.INSTRUCTION_SOURCE_HUMAN, limit)

	rows, err := tx.QueryContext(ctx, q, qargs...)
	if err != nil {
		return nil, retryable(err, "reading your instructions")
	}
	defer func() { _ = rows.Close() }()

	var out []waitingInstruction
	for rows.Next() {
		var (
			w          waitingInstruction
			kind, src  int64
			reportBack bool
		)
		if err := rows.Scan(&w.id, &w.key, &kind, &src, &w.body, &reportBack,
			&w.refKind, &w.refUUID, &w.raisedBy); err != nil {
			return nil, retryable(err, "reading your instructions")
		}
		w.kind = enums.InstructionKind(kind)
		w.source = enums.InstructionSource(src)
		w.report = reportBack
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "reading your instructions")
	}
	return out, nil
}

// conflictContext is what a conflict notice needs to be actionable: the
// conflict's own key and severity, the file the two claims actually meet on,
// and who the other agent is.
type conflictContext struct {
	key      string
	severity string
	path     string
	with     string
	others   int
}

// renderInstructions turns rows into the items the agent gets, and reports
// which ids were actually included — only those are marked delivered.
func (h *Handler) renderInstructions(ctx context.Context, tx *sql.Tx, sessionUUID uuid.UUID, waiting []waitingInstruction) ([]DeliveredInstruction, []string, error) {
	items := make([]DeliveredInstruction, 0, len(waiting))
	if len(waiting) == 0 {
		return items, nil, nil
	}

	conflicts, err := loadConflictContext(ctx, tx, sessionUUID, waiting)
	if err != nil {
		return nil, nil, err
	}
	raisers, err := loadRaiserNames(ctx, tx, waiting)
	if err != nil {
		return nil, nil, err
	}

	var (
		delivered []string
		used      int
	)
	for _, w := range waiting {
		item := DeliveredInstruction{
			Key:        w.key,
			Kind:       w.kind.String(),
			From:       raisers[w.id],
			What:       truncate(w.body, instructionTextChars),
			ReportBack: w.report,
		}
		if item.From == "" && w.source == enums.INSTRUCTION_SOURCE_SERVER {
			item.From = "metiche"
		}
		if c, ok := conflicts[w.refUUID]; ok {
			item.Ref = c.key
			item.Severity = c.severity
			item.With = c.with
			if c.others > 0 {
				item.With = fmt.Sprintf("%s +%d other(s)", c.with, c.others)
			}
			if c.path != "" {
				item.Paths = []string{c.path}
			}
		}
		// The body the detector writes carries the incumbent's action inline;
		// split it out so the agent reads the action as an action rather than
		// as the tail of a sentence, and so it is never repeated twice in one
		// response.
		//
		// Only for a conflict notice. A human's prose has no shape to parse,
		// and splitting it on its first colon would present half a sentence
		// as the thing to do next.
		if w.kind == enums.INSTRUCTION_KIND_CONFLICT_NOTICE {
			if what, action, ok := splitNoticeBody(w.body); ok {
				item.What = truncate(what, instructionTextChars)
				item.SuggestedAction = truncate(action, instructionTextChars)
			}
		}
		if item.SuggestedAction == "" {
			item.SuggestedAction = truncate(fallbackAction(w.kind, w.key, item.With), instructionTextChars)
		}

		// The budget. Measured on the rendered bytes rather than guessed,
		// and the first item always goes out however big it is.
		size, err := renderedSize(item)
		if err != nil {
			return nil, nil, err
		}
		if len(items) > 0 && used+size > instructionsResponseCharBudget {
			break
		}
		used += size
		items = append(items, item)
		delivered = append(delivered, w.id)
	}
	return items, delivered, nil
}

// renderedSize is how many characters this item costs the response.
func renderedSize(item DeliveredInstruction) (int, error) {
	b, err := json.Marshal(item)
	if err != nil {
		return 0, fmt.Errorf("rendering an instruction: %w", err)
	}
	return len(b), nil
}

// loadConflictContext fetches, in one query, everything the conflict notices
// in this batch need — including who the OTHER participant is.
//
// The other participant comes from conflict_participant rather than from the
// conflict's evidence labels, because evidence is rewritten every time the
// pair is re-detected and the two sides swap roles when the other agent
// declares next. The participant row does not move.
func loadConflictContext(ctx context.Context, tx *sql.Tx, sessionUUID uuid.UUID, waiting []waitingInstruction) (map[string]conflictContext, error) {
	ids := make([]string, 0, len(waiting))
	seen := map[string]bool{}
	for _, w := range waiting {
		if w.refUUID == "" || w.refKind != int64(enums.SUBJECT_KIND_CONFLICT) || seen[w.refUUID] {
			continue
		}
		seen[w.refUUID] = true
		ids = append(ids, w.refUUID)
	}
	out := map[string]conflictContext{}
	if len(ids) == 0 {
		return out, nil
	}

	ph := make([]string, len(ids))
	args := []any{sessionUUID.String()}
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	// JSON_VALUE, not JSON_UNQUOTE(JSON_EXTRACT(...)): a duplicate_work
	// conflict with no shared file stores overlap_path as JSON null, which the
	// latter turns into the four-letter string "null" and shows the agent as a
	// path. JSON_VALUE gives SQL NULL, so the COALESCE leaves no path.
	rows, err := tx.QueryContext(ctx,
		"SELECT c.`id`, c.`key`, c.`severity`, "+
			"COALESCE(JSON_VALUE(c.`evidence`, '$.overlap_path'), ''), "+
			"COALESCE(m.`display_name`, ''), COALESCE(a.`label`, ''), COALESCE(s.`branch`, '') "+
			"FROM `conflict` c "+
			"LEFT JOIN `conflict_participant` p ON p.`conflict_uuid` = c.`id` AND p.`session_uuid` <> ? "+
			"LEFT JOIN `member` m ON m.`id` = p.`member_uuid` "+
			"LEFT JOIN `agent` a ON a.`id` = p.`agent_uuid` "+
			"LEFT JOIN `session` s ON s.`id` = p.`session_uuid` "+
			"WHERE c.`id` IN ("+strings.Join(ph, ",")+")",
		args...)
	if err != nil {
		return nil, retryable(err, "reading the conflicts behind your instructions")
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id, key, path, member, label, branch string
			severity                             int64
		)
		if err := rows.Scan(&id, &key, &severity, &path, &member, &label, &branch); err != nil {
			return nil, retryable(err, "reading the conflicts behind your instructions")
		}
		// Three agents in one directory is normal, so a conflict can have
		// more than one other participant. The first is named and the rest
		// are counted: naming all of them is how a notice turns into a
		// paragraph nobody reads.
		if prev, ok := out[id]; ok {
			prev.others++
			out[id] = prev
			continue
		}
		out[id] = conflictContext{
			key:      key,
			severity: enums.ConflictSeverity(severity).String(),
			path:     path,
			with:     describeOtherSide(member, label, branch),
		}
	}
	return out, retryable(rows.Err(), "reading the conflicts behind your instructions")
}

// loadRaiserNames names the human behind a nudge, so "stop" arrives attached
// to a person rather than to the system.
func loadRaiserNames(ctx context.Context, tx *sql.Tx, waiting []waitingInstruction) (map[string]string, error) {
	ids := make([]string, 0, len(waiting))
	for _, w := range waiting {
		if w.raisedBy != "" {
			ids = append(ids, w.id)
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT i.`id`, m.`display_name` FROM `instruction` i "+
			"JOIN `member` m ON m.`id` = i.`raised_by_member_uuid` "+
			"WHERE i.`id` IN ("+strings.Join(ph, ",")+")",
		args...)
	if err != nil {
		return nil, retryable(err, "reading who raised your instructions")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, retryable(err, "reading who raised your instructions")
		}
		out[id] = name
	}
	return out, retryable(rows.Err(), "reading who raised your instructions")
}

// describeOtherSide names the other agent the way a teammate would.
func describeOtherSide(member, label, branch string) string {
	var b strings.Builder
	switch {
	case member != "" && label != "":
		fmt.Fprintf(&b, "%s (%s)", member, label)
	case member != "":
		b.WriteString(member)
	case label != "":
		b.WriteString(label)
	default:
		return ""
	}
	if branch != "" {
		fmt.Fprintf(&b, " on %s", branch)
	}
	return b.String()
}

// splitNoticeBody pulls the suggested action out of a conflict notice.
//
// The detector writes one line — "CF-3 (high) on internal/auth/token.go:
// <what to do> They said they are about to: <their summary>" — and the middle
// of it is the ONLY place the incumbent's own action exists. It is not in
// conflict.suggested_action: that column holds the action for the agent that
// ARRIVED, and handing the incumbent the other side's instruction is worse
// than handing them none.
//
// Coupled to one format string in detector.go, and deliberately fails soft: a
// body it does not recognise comes back whole, with a generic action from
// fallbackAction, so the rule that a conflict is never surfaced without a next
// action holds even if that format changes.
func splitNoticeBody(body string) (what, action string, ok bool) {
	const saidMarker = " They said they are about to: "
	head, rest, found := strings.Cut(body, ": ")
	if !found || strings.TrimSpace(rest) == "" {
		return body, "", false
	}
	what, action = head, rest
	if before, said, hasSaid := strings.Cut(rest, saidMarker); hasSaid {
		action = before
		what = head + " — they are about to: " + said
	}
	action = strings.TrimSpace(action)
	return strings.TrimSpace(what), action, action != ""
}

// fallbackAction is the guarantee behind "never surface a conflict without a
// suggested next action". Every kind has one, and every one of them names the
// tool to call, because an action a model cannot execute is a sentence.
func fallbackAction(kind enums.InstructionKind, key, with string) string {
	who := with
	if who == "" {
		who = "the other agent"
	}
	switch kind {
	case enums.INSTRUCTION_KIND_STOP:
		return fmt.Sprintf("stop this work now: call update_intent(status='done') to release the files you hold, then report_back('%s', 'done')", key)
	case enums.INSTRUCTION_KIND_STEER:
		return fmt.Sprintf("fold this into what you are doing and call update_intent with the new summary, then report_back('%s', 'done') — or report_back('%s', 'refused') with why not", key, key)
	case enums.INSTRUCTION_KIND_CONFLICT_NOTICE:
		return fmt.Sprintf("settle it with %s: split the file or sequence the work, drop what you give up with update_intent(drop_paths=[...]), then report_back('%s', ...) with a one-line note of what you agreed", who, key)
	case enums.INSTRUCTION_KIND_JUDGE_REQUEST:
		// Judge requests travel as pending.reviews, never as instructions
		// (docs/DECISIONS.md §1.3); this is for a stray row.
		return fmt.Sprintf("judge it with get_review_context, then report_judgement; then report_back('%s', 'done')", key)
	case enums.INSTRUCTION_KIND_QUESTION:
		return fmt.Sprintf("answer it in the note: report_back('%s', 'done', note='<your answer>')", key)
	default:
		return fmt.Sprintf("nothing to do but say you saw it: report_back('%s', 'acknowledged')", key)
	}
}

// deliveryNote is the one line the model reads first.
func deliveryNote(items []DeliveredInstruction, more int) string {
	if len(items) == 0 {
		if more > 0 {
			// Only reachable when everything waiting expired between the
			// count and the read; say something honest rather than nothing.
			return fmt.Sprintf("nothing delivered, %d still waiting — call get_instructions again", more)
		}
		return "nothing waiting for you"
	}
	keys := make([]string, 0, len(items))
	needsReport := 0
	for _, it := range items {
		keys = append(keys, it.Key)
		if it.ReportBack {
			needsReport++
		}
	}
	note := fmt.Sprintf("%d instruction(s) delivered (%s) — act on the suggested_action, then report_back on each",
		len(items), strings.Join(keys, ", "))
	if needsReport > 0 && needsReport < len(items) {
		note += fmt.Sprintf("; %d of them is somebody waiting on an answer", needsReport)
	}
	if more > 0 {
		note += fmt.Sprintf("; %d more waiting — call get_instructions again", more)
	}
	return truncate(note, instructionNoteChars)
}

// ─────────────────────────────────────────────
// Tool: report_back
// ─────────────────────────────────────────────

type ReportBackParams struct {
	SessionKey     string `json:"session_key" jsonschema:"The session_key start_session gave you."`
	TeamSlug       string `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	InstructionKey string `json:"instruction_key" jsonschema:"Which instruction, by the key get_instructions returned (IN-12)."`
	Outcome        string `json:"outcome" jsonschema:"What you did about it: 'done' (you did it), 'acknowledged' (you have taken it on but are not finished), 'refused' (you are not going to, and the note says why), 'blocked' (you cannot until something else moves) or 'not_applicable' (it does not apply to you). Refusing is a fine answer; silence is not."`
	Note           string `json:"note,omitempty" jsonschema:"One line for the person watching the board: what you did, or why you did not. Required when you refuse or are blocked - a refusal with no reason leaves the loop open anyway."`
}

// ReportBack closes the loop.
//
// It is the only half of this pair that writes an event, and it writes it
// through commit like everything else: a report is a change to the team's
// shared state — the person who raised the instruction is waiting on it, and
// the board's timeline is how they see the answer — so it takes the team lock,
// appends one event and advances the sequence.
//
// IDEMPOTENT BY CONSTRUCTION. The idempotency key is derived from the
// instruction and the outcome rather than taken from the caller, so reporting
// the same outcome twice REPLAYS the first response and appends no second
// event, with no cooperation required from the agent — which matters because
// the most likely double report is a retry after a response the agent never
// received. Reporting a DIFFERENT outcome later (acknowledged, then done) is a
// different key and a real second event, which is correct: that is news.
func (h *Handler) ReportBack(ctx context.Context, _ *mcp.CallToolRequest, args ReportBackParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSessionOnTeam(ctx, args.SessionKey, args.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session

	outcome, action, err := parseReportOutcome(args.Outcome)
	if err != nil {
		return nil, nil, err
	}
	note := truncate(args.Note, 300)
	if note == "" && (action == enums.INSTRUCTION_ACTION_REFUSED || outcome == "blocked") {
		return nil, nil, fmt.Errorf(
			"say why in the note when you report %q — a refusal or a block with no reason leaves the person who raised it exactly as stuck as silence would", outcome)
	}

	instr, err := h.resolveInstruction(ctx, who, args.InstructionKey)
	if err != nil {
		return nil, nil, err
	}

	// The four outcomes collapse onto the four values instruction.action can
	// hold, so the exact word the agent chose is kept on the front of the
	// note. That is what keeps "blocked" and "acknowledged" — both PARTIAL —
	// distinguishable to the human reading the board.
	storedNote := outcome
	if note != "" {
		storedNote = outcome + ": " + note
	}

	var refConflict *uuid.UUID
	if instr.refKind == int64(enums.SUBJECT_KIND_CONFLICT) && instr.refUUID != "" {
		if id, err := uuid.FromString(instr.refUUID); err == nil {
			refConflict = &id
		}
	}
	instrUUID := instr.uuid

	response, err := h.commit(ctx, Mutation{
		TeamUUID:       who.Team.ID,
		IdempotencyKey: "instruction_acted:" + instr.uuid.String() + ":" + action.String() + ":" + outcome,
		Kind:           enums.EVENT_KIND_INSTRUCTION_ACTED,
		Structural:     false,
		ProjectUUID:    uuidPtr(sess.ProjectUUID),
		SessionUUID:    uuidPtr(sess.ID),
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(who.Member.ID),
		SubjectKind:    enums.SUBJECT_KIND_INSTRUCTION,
		SubjectUUID:    &instrUUID,
		SubjectKey:     instr.key,
		Summary:        fmt.Sprintf("%s reported %s on %s", ag.Label, outcome, instr.key),
		Payload: payload_entity.EventPayload{
			Message:         nullString(note),
			PreviousStatus:  nullString(instr.status.String()),
			NewStatus:       nullString(enums.InstructionStatus(enums.INSTRUCTION_STATUS_ACTED).String()),
			InstructionUUID: &instrUUID,
			ConflictUUID:    refConflict,
			Detail:          nullString(outcome),
		},
		Envelope: Envelope{Key: instr.key},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			// delivered_at is filled in when it is still empty: an agent can
			// legitimately answer something it learned about through the
			// conflicts on its own response rather than through
			// get_instructions, and an ACTED instruction that was never
			// delivered reads as a bug on the board.
			if _, err := tc.Tx.ExecContext(ctx,
				"UPDATE `instruction` SET `status` = ?, `action` = ?, `action_note` = ?, `acted_at` = ?, "+
					"`delivered_at` = COALESCE(`delivered_at`, ?), `updated_at` = ? WHERE `id` = ?",
				enums.INSTRUCTION_STATUS_ACTED, int64(action), truncate(storedNote, 400),
				tc.Now, tc.Now, tc.Now, instrUUID.String()); err != nil {
				return retryable(err, "recording your report")
			}
			// An answer to a conflict notice proves this participant was
			// told, and is its acknowledgement. Both COALESCEd, so the first
			// delivery and the first answer are what stay on record.
			if refConflict != nil {
				if _, err := tc.Tx.ExecContext(ctx,
					"UPDATE `conflict_participant` SET `notified_at` = COALESCE(`notified_at`, ?), "+
						"`acked_at` = COALESCE(`acked_at`, ?), `updated_at` = ? WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
					tc.Now, tc.Now, tc.Now, refConflict.String(), sess.ID.String()); err != nil {
					return retryable(err, "recording the answer on the conflict")
				}
			}
			env.Key = instr.key
			env.Note = truncate(fmt.Sprintf("%s reported as %s — the board shows your answer now", instr.key, outcome), instructionNoteChars)
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(response)
}

// instructionRef is the little of an instruction row report_back needs.
type instructionRef struct {
	uuid    uuid.UUID
	key     string
	status  enums.InstructionStatus
	refKind int64
	refUUID string
}

// resolveInstruction finds the instruction a report is about, scoped to the
// caller's own session.
//
// Scoped to the SESSION, not the team, and that is the whole security of it:
// instruction keys are short and guessable (IN-12), the endpoint is public,
// and without this check any agent could answer — and so silence — a nudge a
// human sent to somebody else.
func (h *Handler) resolveInstruction(ctx context.Context, who Resolved, key string) (instructionRef, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return instructionRef{}, errors.New("instruction_key is required — it is the key get_instructions returned (IN-12)")
	}

	var (
		row      instructionRef
		id       string
		status   int64
		targetID string
	)
	err := h.core.DB().QueryRowContext(ctx,
		"SELECT `id`, `key`, `status`, COALESCE(`ref_kind`, 0), COALESCE(`ref_uuid`, ''), "+
			"COALESCE(`target_session_uuid`, '') FROM `instruction` WHERE `team_uuid` = ? AND `key` = ?",
		who.Team.ID.String(), key).Scan(&id, &row.key, &status, &row.refKind, &row.refUUID, &targetID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return instructionRef{}, fmt.Errorf(
			"no instruction with key %q on this team — call get_instructions to see what is actually waiting for you", key)
	case err != nil:
		return instructionRef{}, retryable(err, "looking up the instruction")
	}
	if targetID != who.Session.ID.String() {
		return instructionRef{}, fmt.Errorf(
			"instruction %s was not sent to this session — only the agent it was addressed to can answer it", key)
	}
	row.uuid, err = uuid.FromString(id)
	if err != nil {
		return instructionRef{}, err
	}
	row.status = enums.InstructionStatus(status)
	return row, nil
}

// parseReportOutcome maps what an agent would say onto the four values the
// column can hold, and returns the canonical word alongside it.
//
// Two words share INSTRUCTION_ACTION_PARTIAL — "acknowledged" and "blocked" —
// because the enum is generated and has four values while the distinction
// between "working on it" and "stuck" is exactly what the human on the board
// needs. The word is preserved on the note rather than lost to the mapping.
func parseReportOutcome(s string) (string, enums.InstructionAction, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "done", "complied", "complete", "completed", "fixed", "resolved", "yes":
		return "done", enums.INSTRUCTION_ACTION_COMPLIED, nil
	case "acknowledged", "ack", "acknowledge", "noted", "seen", "partial", "in_progress", "working":
		return "acknowledged", enums.INSTRUCTION_ACTION_PARTIAL, nil
	case "refused", "refuse", "declined", "decline", "rejected", "no":
		return "refused", enums.INSTRUCTION_ACTION_REFUSED, nil
	case "blocked", "block", "stuck", "waiting":
		return "blocked", enums.INSTRUCTION_ACTION_PARTIAL, nil
	case "not_applicable", "not applicable", "n/a", "na", "irrelevant", "already_done":
		return "not_applicable", enums.INSTRUCTION_ACTION_NOT_APPLICABLE, nil
	case "":
		return "", enums.INSTRUCTION_ACTION_INVALID, errors.New(
			"outcome is required: done, acknowledged, refused, blocked or not_applicable")
	default:
		return "", enums.INSTRUCTION_ACTION_INVALID, fmt.Errorf(
			"outcome must be one of done, acknowledged, refused, blocked, not_applicable (got %q)", s)
	}
}
