package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/enums"
)

// contractresolve.go closes contract conflicts once they are no longer true,
// the way conflictresolve.go closes path overlaps.
//
// # When it runs
//
//   - publish_contract, inside its Apply, for every open contract conflict on
//     the contract just published: the shapes may now agree, or a producer may
//     have appeared for a consumer that had none;
//   - end_session, inside its Apply, for every open contract conflict the
//     ending session takes part in, after its assertions were withdrawn;
//   - the sweeper, inside appendEvent's locked hook, for sessions it abandons.
//
// # The rules
//
// A conflict closes as SUPERSEDED when one of its sides no longer counts: its
// session ended or was abandoned, or it holds no active assertion for that
// role any more. It closes as CONVERGED when both sides still count and the
// thing that made it a conflict is gone: the producer's and consumer's shapes
// no longer disagree (for a mismatch, no breaking issue; for a field naming
// variant, no spelling difference), or — for contract_unclaimed and a key
// variant — the consumed contract now has a live producer. Anything else
// stays open.

// ContractReleaseKind says what happened that may have settled something.
type ContractReleaseKind int

const (
	// ContractReleasePublished is publish_contract.
	ContractReleasePublished ContractReleaseKind = iota + 1
	// ContractReleaseSessionEnded is end_session.
	ContractReleaseSessionEnded
	// ContractReleaseSessionAbandoned is the sweeper writing off a session.
	ContractReleaseSessionAbandoned
)

// ContractRelease is one such event.
type ContractRelease struct {
	TeamUUID    uuid.UUID
	SessionUUID uuid.UUID
	Kind        ContractReleaseKind
	At          time.Time
}

var contractConflictKindArgs = []any{
	int64(enums.CONFLICT_KIND_CONTRACT_MISMATCH),
	int64(enums.CONFLICT_KIND_CONTRACT_UNCLAIMED),
	int64(enums.CONFLICT_KIND_CONTRACT_NAMING_VARIANT),
}

// WithdrawSessionAssertions marks a session's active assertions withdrawn.
//
// There is no index leading with session_uuid on contract_assertion, so the
// update goes through the session's project: uq_contract_key_norm finds the
// project's contracts and uq_contract_assertion_active the session's rows on
// each. A session only ever asserts on its own project's contracts. Detection
// already ignores a finished session's assertions through the session-status
// filter; this is what makes the stored rows say so too.
func WithdrawSessionAssertions(ctx context.Context, q queryer, projectUUID, sessionUUID uuid.UUID, now time.Time) (int64, error) {
	res, err := q.ExecContext(ctx,
		"UPDATE `contract_assertion` a JOIN `contract` c ON c.`id` = a.`contract_uuid` "+
			"SET a.`status` = ?, a.`active_marker` = NULL, a.`updated_at` = ? "+
			"WHERE c.`project_uuid` = ? AND a.`session_uuid` = ? AND a.`status` = ?",
		int64(enums.ASSERTION_STATUS_WITHDRAWN), now, projectUUID.String(), sessionUUID.String(),
		int64(enums.ASSERTION_STATUS_ACTIVE))
	if err != nil {
		return 0, retryable(err, "withdrawing the session's contract assertions")
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// OpenContractConflictsOfSession lists the open contract conflicts a session
// takes part in. Driven by idx_participant_session.
func OpenContractConflictsOfSession(ctx context.Context, q queryer, teamUUID, sessionUUID uuid.UUID) ([]uuid.UUID, error) {
	args := []any{sessionUUID.String(), teamUUID.String()}
	args = append(args, contractConflictKindArgs...)
	args = append(args, int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED), settleMaxConflicts)
	return scanConflictIDs(ctx, q,
		"SELECT c.`id` FROM `conflict_participant` p JOIN `conflict` c ON c.`id` = p.`conflict_uuid` "+
			"WHERE p.`session_uuid` = ? AND c.`team_uuid` = ? AND c.`kind` IN (?, ?, ?) AND c.`status` IN (?, ?) "+
			"ORDER BY c.`created_at`, c.`key` LIMIT ?", args...)
}

// OpenContractConflictsOfContract lists the open contract conflicts with a
// participant asserting on this contract. Driven by idx_conflict_open
// (team_uuid, status, severity) — a team's open conflicts are few — then the
// participant foreign key and the assertion's primary key.
func OpenContractConflictsOfContract(ctx context.Context, q queryer, teamUUID, contractUUID uuid.UUID) ([]uuid.UUID, error) {
	args := []any{int64(enums.SUBJECT_KIND_CONTRACT_ASSERTION), teamUUID.String(),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED)}
	args = append(args, contractConflictKindArgs...)
	args = append(args, contractUUID.String(), settleMaxConflicts)
	return scanConflictIDs(ctx, q,
		"SELECT c.`id` FROM `conflict` c "+
			"JOIN `conflict_participant` p ON p.`conflict_uuid` = c.`id` AND p.`subject_kind` = ? "+
			"JOIN `contract_assertion` a ON a.`id` = p.`subject_uuid` "+
			"WHERE c.`team_uuid` = ? AND c.`status` IN (?, ?) AND c.`kind` IN (?, ?, ?) AND a.`contract_uuid` = ? "+
			"GROUP BY c.`id`, c.`created_at`, c.`key` ORDER BY c.`created_at`, c.`key` LIMIT ?", args...)
}

func scanConflictIDs(ctx context.Context, q queryer, query string, args ...any) ([]uuid.UUID, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, retryable(err, "finding the contract conflicts to re-evaluate")
	}
	defer func() { _ = rows.Close() }()
	var out []uuid.UUID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, retryable(err, "reading the contract conflicts to re-evaluate")
		}
		if id, err := uuid.FromString(raw); err == nil {
			out = append(out, id)
		}
	}
	return out, retryable(rows.Err(), "reading the contract conflicts to re-evaluate")
}

// contractSettlePart is one participant of a contract conflict, as the rules
// and the note need it.
type contractSettlePart struct {
	SessionUUID  string
	SessionKey   string
	AgentLabel   string
	Status       enums.SessionStatus
	Outcome      enums.SessionOutcome
	OutcomeNote  string
	EndedAt      sql.NullTime
	ContractUUID string
	ContractKey  string
	Role         enums.AssertionRole
	// Current is this session's active assertion for the same contract and
	// role, when it still counts (live or stale session). Nil otherwise.
	Current *contractSide
}

func (p contractSettlePart) name() string {
	return settleSide{Key: p.SessionKey, Label: p.AgentLabel}.name()
}

func (p contractSettlePart) sessionOver() bool {
	return p.Status != enums.SESSION_STATUS_LIVE && p.Status != enums.SESSION_STATUS_STALE
}

// contractProducerRef is the live producer a consumed contract now has.
type contractProducerRef struct {
	SessionKey string
	AgentLabel string
	AssertedAt sql.NullTime
}

// SettleContractConflict re-evaluates one contract conflict and closes it
// when it is no longer true. Must run on the transaction holding the team
// lock: the check and the conditional UPDATE are one decision.
func SettleContractConflict(ctx context.Context, q queryer, conflictID uuid.UUID, rel ContractRelease) (SettledConflict, bool, error) {
	out := SettledConflict{ID: conflictID}
	var (
		project           string
		kind, sev, status int64
		rule              sql.NullString
	)
	args := []any{conflictID.String(), rel.TeamUUID.String()}
	args = append(args, contractConflictKindArgs...)
	args = append(args, int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED))
	err := q.QueryRowContext(ctx,
		"SELECT `key`, `project_uuid`, `kind`, `severity`, `status`, `detector_rule`, "+
			"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(`evidence`, '$.overlap_path')), '') "+
			"FROM `conflict` WHERE `id` = ? AND `team_uuid` = ? AND `kind` IN (?, ?, ?) AND `status` IN (?, ?)",
		args...).Scan(&out.Key, &project, &kind, &sev, &status, &rule, &out.OverlapPath)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, false, nil
	case err != nil:
		return out, false, retryable(err, "reading the contract conflict to re-evaluate")
	}
	if out.OverlapPath == "null" {
		out.OverlapPath = ""
	}
	out.Kind = enums.ConflictKind(kind)
	out.ProjectUUID, _ = uuid.FromString(project)
	out.Severity = enums.ConflictSeverity(sev)
	out.Previous = enums.ConflictStatus(status)

	parts, err := loadContractSettleParts(ctx, q, conflictID)
	if err != nil {
		return out, false, err
	}

	// Unclaimed and key variants are about the CONSUMED contract having a
	// producer; look it up only for them.
	var producer *contractProducerRef
	if out.Kind == enums.CONFLICT_KIND_CONTRACT_UNCLAIMED || rule.String == RuleContractKeyVariant {
		for _, part := range parts {
			if part.Role == enums.ASSERTION_ROLE_CONSUMES && part.ContractUUID != "" {
				producer, err = firstLiveProducer(ctx, q, part.ContractUUID)
				if err != nil {
					return out, false, err
				}
				break
			}
		}
	}

	resolution, ok := decideContractSettlement(out.Kind, rule.String, parts, producer)
	if !ok {
		return out, false, nil
	}
	out.Resolution = resolution
	out.Note = buildContractResolutionNote(out, rule.String, parts, producer, rel)

	res, err := q.ExecContext(ctx,
		"UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolution_note` = ?, `resolved_at` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` IN (?, ?)",
		int64(enums.CONFLICT_STATUS_RESOLVED), int64(out.Resolution), out.Note, rel.At, rel.At,
		conflictID.String(), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED))
	if err != nil {
		return out, false, retryable(err, "closing the settled contract conflict")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return out, false, nil
	}
	return out, true, nil
}

// decideContractSettlement is the whole rule, and it is pure. See the file
// comment. It returns false to keep the conflict open.
func decideContractSettlement(kind enums.ConflictKind, rule string, parts []contractSettlePart, producer *contractProducerRef) (enums.ConflictResolution, bool) {
	need := 2
	if kind == enums.CONFLICT_KIND_CONTRACT_UNCLAIMED {
		need = 1
	}
	if len(parts) < need {
		// Something was deleted under it; nothing truthful to say.
		return enums.CONFLICT_RESOLUTION_INVALID, false
	}
	for _, p := range parts {
		if p.Current == nil {
			return enums.CONFLICT_RESOLUTION_SUPERSEDED, true
		}
	}

	if kind == enums.CONFLICT_KIND_CONTRACT_UNCLAIMED || rule == RuleContractKeyVariant {
		if producer != nil {
			return enums.CONFLICT_RESOLUTION_CONVERGED, true
		}
		return enums.CONFLICT_RESOLUTION_INVALID, false
	}

	var prod, cons *contractSide
	for _, p := range parts {
		switch p.Role {
		case enums.ASSERTION_ROLE_PRODUCES:
			prod = p.Current
		case enums.ASSERTION_ROLE_CONSUMES:
			cons = p.Current
		}
	}
	if prod == nil || cons == nil || !prod.ShapeOK || !cons.ShapeOK {
		return enums.CONFLICT_RESOLUTION_INVALID, false
	}
	var issues []coordination.ContractIssue
	if prod.Hash != cons.Hash {
		issues = coordination.CompareShapes(prod.Shape, cons.Shape)
	}
	breaking, naming := splitContractIssues(issues)
	switch kind {
	case enums.CONFLICT_KIND_CONTRACT_MISMATCH:
		if len(breaking) == 0 {
			return enums.CONFLICT_RESOLUTION_CONVERGED, true
		}
	case enums.CONFLICT_KIND_CONTRACT_NAMING_VARIANT:
		if len(naming) == 0 {
			return enums.CONFLICT_RESOLUTION_CONVERGED, true
		}
	}
	return enums.CONFLICT_RESOLUTION_INVALID, false
}

func loadContractSettleParts(ctx context.Context, q queryer, conflictID uuid.UUID) ([]contractSettlePart, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT p.`session_uuid`, COALESCE(s.`key`, ''), COALESCE(ag.`label`, ''), COALESCE(s.`status`, 0), "+
			"COALESCE(s.`outcome`, 0), COALESCE(s.`outcome_note`, ''), s.`ended_at`, "+
			"COALESCE(a.`contract_uuid`, ''), COALESCE(ct.`key`, ''), COALESCE(a.`role`, 0) "+
			"FROM `conflict_participant` p "+
			"LEFT JOIN `session` s ON s.`id` = p.`session_uuid` "+
			"LEFT JOIN `agent` ag ON ag.`id` = p.`agent_uuid` "+
			"LEFT JOIN `contract_assertion` a ON a.`id` = p.`subject_uuid` "+
			"LEFT JOIN `contract` ct ON ct.`id` = a.`contract_uuid` "+
			"WHERE p.`conflict_uuid` = ? AND p.`subject_kind` = ? ORDER BY p.`created_at`, p.`session_uuid` LIMIT ?",
		conflictID.String(), int64(enums.SUBJECT_KIND_CONTRACT_ASSERTION), settleMaxParticipants)
	if err != nil {
		return nil, retryable(err, "reading the contract conflict's participants")
	}
	var parts []contractSettlePart
	for rows.Next() {
		var (
			p                     contractSettlePart
			status, outcome, role int64
		)
		if err := rows.Scan(&p.SessionUUID, &p.SessionKey, &p.AgentLabel, &status, &outcome, &p.OutcomeNote,
			&p.EndedAt, &p.ContractUUID, &p.ContractKey, &role); err != nil {
			_ = rows.Close()
			return nil, retryable(err, "reading the contract conflict's participants")
		}
		p.Status, p.Outcome, p.Role = enums.SessionStatus(status), enums.SessionOutcome(outcome), enums.AssertionRole(role)
		parts = append(parts, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, retryable(err, "reading the contract conflict's participants")
	}
	_ = rows.Close()

	// One open result set at a time on the transaction's connection.
	for i := range parts {
		if parts[i].sessionOver() || parts[i].ContractUUID == "" {
			continue
		}
		cur, err := currentAssertion(ctx, q, parts[i].ContractUUID, parts[i].SessionUUID, parts[i].Role)
		if err != nil {
			return nil, err
		}
		parts[i].Current = cur
	}
	return parts, nil
}

// currentAssertion reads a session's active assertion for one contract and
// role through uq_contract_assertion_active.
func currentAssertion(ctx context.Context, q queryer, contractUUID, sessionUUID string, role enums.AssertionRole) (*contractSide, error) {
	var (
		s     = contractSide{ContractUUID: contractUUID, SessionUUID: sessionUUID, Role: role}
		shape sql.NullString
	)
	err := q.QueryRowContext(ctx,
		"SELECT `id`, `shape_hash`, `shape` FROM `contract_assertion` "+
			"WHERE `contract_uuid` = ? AND `session_uuid` = ? AND `role` = ? AND `status` = ? LIMIT 1",
		contractUUID, sessionUUID, int64(role), int64(enums.ASSERTION_STATUS_ACTIVE)).Scan(&s.AssertionUUID, &s.Hash, &shape)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, retryable(err, "reading a participant's current assertion")
	}
	if shape.Valid {
		if parsed, err := coordination.ShapeFromCanonical([]byte(shape.String)); err == nil {
			s.Shape, s.ShapeOK = parsed, true
		}
	}
	return &s, nil
}

func firstLiveProducer(ctx context.Context, q queryer, contractUUID string) (*contractProducerRef, error) {
	var p contractProducerRef
	err := q.QueryRowContext(ctx,
		"SELECT s.`key`, COALESCE(ag.`label`, ''), a.`asserted_at` FROM `contract_assertion` a "+
			"JOIN `session` s ON s.`id` = a.`session_uuid` LEFT JOIN `agent` ag ON ag.`id` = s.`agent_uuid` "+
			"WHERE a.`contract_uuid` = ? AND a.`status` = ? AND a.`role` = ? AND s.`status` IN (?, ?) "+
			"ORDER BY a.`asserted_at`, a.`id` LIMIT 1",
		contractUUID, int64(enums.ASSERTION_STATUS_ACTIVE), int64(enums.ASSERTION_ROLE_PRODUCES),
		int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE)).Scan(&p.SessionKey, &p.AgentLabel, &p.AssertedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, retryable(err, "looking for the contract's producer")
	}
	return &p, nil
}

// buildContractResolutionNote writes the board's one paragraph from what
// actually happened. It always fits conflict.resolution_note.
func buildContractResolutionNote(c SettledConflict, rule string, parts []contractSettlePart, producer *contractProducerRef, rel ContractRelease) string {
	where := firstNonEmpty(c.OverlapPath, "the contract")
	at := clock(rel.At)
	lead := "Settled by the agents: "
	if rel.Kind == ContractReleaseSessionAbandoned {
		lead = "Cleared by metiche: "
	}

	var releaser *contractSettlePart
	for i := range parts {
		if parts[i].SessionUUID == rel.SessionUUID.String() {
			releaser = &parts[i]
		}
	}

	var b strings.Builder
	b.WriteString(lead)
	switch c.Resolution {
	case enums.CONFLICT_RESOLUTION_SUPERSEDED:
		var clauses []string
		for _, p := range parts {
			if p.Current != nil {
				continue
			}
			key := firstNonEmpty(p.ContractKey, where)
			switch {
			case p.Status == enums.SESSION_STATUS_ABANDONED:
				clauses = append(clauses, fmt.Sprintf("%s was abandoned%s after no heartbeat, so its %s on %s no longer counts",
					p.name(), contractEndedAt(p), p.Role.String(), key))
			case p.sessionOver():
				clauses = append(clauses, fmt.Sprintf("%s ended its session%s%s, withdrawing its %s on %s",
					p.name(), contractEndedAt(p), contractOutcomeSuffix(p), p.Role.String(), key))
			default:
				clauses = append(clauses, fmt.Sprintf("%s no longer %s %s", p.name(), p.Role.String(), key))
			}
		}
		b.WriteString(strings.Join(clauses, "; "))
		b.WriteString(".")
	case enums.CONFLICT_RESOLUTION_CONVERGED:
		consumer := contractPartOfRole(parts, enums.ASSERTION_ROLE_CONSUMES)
		switch {
		case c.Kind == enums.CONFLICT_KIND_CONTRACT_UNCLAIMED || rule == RuleContractKeyVariant:
			prod := "a producer"
			if producer != nil {
				prod = settleSide{Key: producer.SessionKey, Label: producer.AgentLabel}.name()
			}
			key := where
			if consumer != nil && consumer.ContractKey != "" {
				key = consumer.ContractKey
			}
			fmt.Fprintf(&b, "%s now produces %s (published %s)", prod, key, at)
			if consumer != nil {
				fmt.Fprintf(&b, ", so %s is no longer building against something nobody builds", consumer.name())
			}
			b.WriteString(".")
		default:
			prod := contractPartOfRole(parts, enums.ASSERTION_ROLE_PRODUCES)
			if releaser != nil {
				fmt.Fprintf(&b, "%s published its %s shape for %s at %s, and the producer and consumer shapes now agree",
					releaser.name(), releaser.Role.String(), where, at)
			} else {
				fmt.Fprintf(&b, "the producer and consumer shapes for %s agree as of %s", where, at)
			}
			if prod != nil && consumer != nil {
				fmt.Fprintf(&b, " (producer %s, consumer %s)", prod.name(), consumer.name())
			}
			b.WriteString(".")
		}
	}
	return clip(b.String(), resolutionNoteChars)
}

func contractPartOfRole(parts []contractSettlePart, role enums.AssertionRole) *contractSettlePart {
	for i := range parts {
		if parts[i].Role == role {
			return &parts[i]
		}
	}
	return nil
}

func contractEndedAt(p contractSettlePart) string {
	if !p.EndedAt.Valid {
		return ""
	}
	return " at " + clock(p.EndedAt.Time)
}

func contractOutcomeSuffix(p contractSettlePart) string {
	return outcomeSuffix(settleSide{Outcome: p.Outcome, OutcomeNote: p.OutcomeNote})
}

// settleContractConflicts re-evaluates, from inside a tool's Apply, the open
// contract conflicts on one contract (contractUUID set) or of one session
// (sessionUUID set), closes the settled ones and appends one
// conflict_resolved event each — the same sequence discipline as
// settleAfterRelease.
func (h *Handler) settleContractConflicts(ctx context.Context, tc *TxContext, rel ContractRelease, contractUUID, sessionUUID uuid.UUID, actor eventActor) error {
	var (
		ids []uuid.UUID
		err error
	)
	if !contractUUID.IsNil() {
		ids, err = OpenContractConflictsOfContract(ctx, tc.Tx, rel.TeamUUID, contractUUID)
	} else {
		ids, err = OpenContractConflictsOfSession(ctx, tc.Tx, rel.TeamUUID, sessionUUID)
	}
	if err != nil {
		return err
	}
	for _, id := range ids {
		settled, ok, err := SettleContractConflict(ctx, tc.Tx, id, rel)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := appendConflictResolvedEvent(ctx, tc, rel.TeamUUID, settled, actor); err != nil {
			return err
		}
	}
	return nil
}
