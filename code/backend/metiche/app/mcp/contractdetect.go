package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/coordination"
	conflict_evidence_entity "github.com/mklfarha/metiche/backend/entity/conflict_evidence"
	"github.com/mklfarha/metiche/backend/enums"
)

// contractdetect.go compares the assertion publish_contract just wrote with
// every other live assertion that could disagree with it, inside the same
// locked transaction, and records what it finds.
//
// The comparison itself is coordination.CompareShapes — the one tested
// implementation of PLAN.md's four directional branches — run over the
// canonical shapes stored on the assertion rows. The SQL's part is the fast
// path and the candidate set: a pair whose shape hashes are equal is done
// without comparing anything, and only assertions whose session is live or
// stale are candidates, which is how an ended or abandoned session's
// assertions stop counting even before anything withdraws them.

const (
	// contractMaxSidesScanned bounds the assertions read per contract. A team
	// is at most ten people times five agents.
	contractMaxSidesScanned = 64

	// contractMaxNearContracts bounds the key-variant scan over one project's
	// contracts of one kind.
	contractMaxNearContracts = 200

	// contractMaxVariantMatches bounds how many near keys are followed up.
	contractMaxVariantMatches = 4

	// contractKeyVariantMaxDistance is PLAN.md's "key-level Levenshtein ≤2".
	contractKeyVariantMaxDistance = 2

	// contractKeyVariantMinChars keeps two short, genuinely different keys
	// (/api/a and /api/b) from being called variants of each other.
	contractKeyVariantMinChars = 6

	// contractMaxEvidenceIssues bounds conflict.evidence.field_issues.
	contractMaxEvidenceIssues = 12

	// contractMaxNoticeFields bounds ConflictNotice.Fields.
	contractMaxNoticeFields = 5
)

// Detector rules, stored on conflict.detector_rule. Finer than the kind so a
// rule that is wrong for one repository could be demoted on its own.
const (
	RuleContractMissingOut   = "contract_mismatch.missing_out"
	RuleContractMissingIn    = "contract_mismatch.missing_in"
	RuleContractType         = "contract_mismatch.type"
	RuleContractFieldVariant = "contract_naming_variant.field"
	RuleContractKeyVariant   = "contract_naming_variant.key"
)

// Which side a mismatch is on. See ContractFault.
const (
	FaultProducer = "producer"
	FaultConsumer = "consumer"
	FaultBoth     = "both"
)

// ContractMismatchDedupeKey is one contract_mismatch per (contract, producing
// session, consuming session). Keyed on sessions rather than assertion rows:
// a republish mints a new assertion, and the same disagreement must stay the
// same conflict — which is also what lets convergence close it.
func ContractMismatchDedupeKey(contractUUID, producerSession, consumerSession string) string {
	return contractDedupe("contract_mismatch", contractUUID, producerSession, consumerSession)
}

// ContractFieldVariantDedupeKey is the field-spelling naming variant's key.
func ContractFieldVariantDedupeKey(contractUUID, producerSession, consumerSession string) string {
	return contractDedupe("contract_naming_variant", "field", contractUUID, producerSession, consumerSession)
}

// ContractKeyVariantDedupeKey is the near-identical-key naming variant's key:
// a consumer on one contract, a producer on a key one or two edits away.
func ContractKeyVariantDedupeKey(consumerContract, producerContract, consumerSession, producerSession string) string {
	return contractDedupe("contract_naming_variant", "key", consumerContract, producerContract, consumerSession, producerSession)
}

func contractDedupe(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// ─────────────────────────────────────────────
// Reading assertions
// ─────────────────────────────────────────────

// contractSide is one live assertion, with who made it.
type contractSide struct {
	AssertionUUID string
	ContractUUID  string
	ContractKey   string
	SessionUUID   string
	AgentUUID     string
	MemberUUID    string
	SessionKey    string
	AgentLabel    string
	MemberName    string
	Role          enums.AssertionRole
	Hash          string
	Shape         coordination.Shape
	// ShapeOK is false when the stored shape could not be read back; such an
	// assertion is never compared, because a verdict on a shape the server
	// cannot see would be invented.
	ShapeOK bool
}

func (s contractSide) holder() string {
	return describeHolder(claimSide{MemberName: s.MemberName, AgentLabel: s.AgentLabel, SessionKey: s.SessionKey})
}

// loadLiveContractSides reads active assertions on one contract whose session
// is live or stale, optionally of one role. Driven by
// idx_contract_assertion_role (contract_uuid, status, role).
func loadLiveContractSides(ctx context.Context, q queryer, contractUUID string, role enums.AssertionRole, limit int) ([]contractSide, error) {
	query := "SELECT a.`id`, a.`contract_uuid`, c.`key`, a.`session_uuid`, s.`agent_uuid`, a.`member_uuid`, s.`key`, " +
		"COALESCE(ag.`label`, ''), COALESCE(m.`display_name`, ''), a.`role`, a.`shape_hash`, a.`shape` " +
		"FROM `contract_assertion` a " +
		"JOIN `contract` c ON c.`id` = a.`contract_uuid` " +
		"JOIN `session` s ON s.`id` = a.`session_uuid` " +
		"LEFT JOIN `agent` ag ON ag.`id` = s.`agent_uuid` " +
		"LEFT JOIN `member` m ON m.`id` = a.`member_uuid` " +
		"WHERE a.`contract_uuid` = ? AND a.`status` = ? AND s.`status` IN (?, ?)"
	args := []any{contractUUID, int64(enums.ASSERTION_STATUS_ACTIVE),
		int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE)}
	if role != enums.ASSERTION_ROLE_INVALID {
		query += " AND a.`role` = ?"
		args = append(args, int64(role))
	}
	query += " ORDER BY a.`asserted_at`, a.`id` LIMIT ?"
	args = append(args, limit)

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, retryable(err, "reading the contract's assertions")
	}
	defer func() { _ = rows.Close() }()
	var out []contractSide
	for rows.Next() {
		var (
			s     contractSide
			r     int64
			shape sql.NullString
		)
		if err := rows.Scan(&s.AssertionUUID, &s.ContractUUID, &s.ContractKey, &s.SessionUUID, &s.AgentUUID,
			&s.MemberUUID, &s.SessionKey, &s.AgentLabel, &s.MemberName, &r, &s.Hash, &shape); err != nil {
			return nil, retryable(err, "reading the contract's assertions")
		}
		s.Role = enums.AssertionRole(r)
		if shape.Valid {
			if parsed, err := coordination.ShapeFromCanonical([]byte(shape.String)); err == nil {
				s.Shape, s.ShapeOK = parsed, true
			}
		}
		out = append(out, s)
	}
	return out, retryable(rows.Err(), "reading the contract's assertions")
}

// ─────────────────────────────────────────────
// The decision layer — pure
// ─────────────────────────────────────────────

// contractFinding is one disagreement worth a conflict row.
type contractFinding struct {
	Kind     enums.ConflictKind
	Rule     string
	Dedupe   string
	Severity coordination.Severity
	Fault    string
	Producer contractSide
	Consumer contractSide
	Issues   []coordination.ContractIssue
	// Where names the contract the conflict is about: the consumer's key,
	// because the consumer is the side that breaks.
	Where string
}

// splitContractIssues separates what breaks a caller from a spelling.
func splitContractIssues(issues []coordination.ContractIssue) (breaking, naming []coordination.ContractIssue) {
	for _, is := range issues {
		if is.Kind == coordination.IssueNamingVariant {
			naming = append(naming, is)
		} else {
			breaking = append(breaking, is)
		}
	}
	return breaking, naming
}

// ContractFault names the side that has to change.
//
// PLAN.md: "a missing out field breaks the consumer, a missing in field
// breaks the producer". The side at FAULT is the one whose shape lacks what
// the other requires: a required response field the producer does not return
// is the producer's to add; a required request field the consumer does not
// send is the consumer's to send. A type disagreement has no direction — each
// side is equally likely to be wrong — so it is "both". When issues point both
// ways, the worse one decides; a tie is "both". Getting this backwards is
// worse than saying nothing: the wrong agent "fixes" a shape that was right.
func ContractFault(issues []coordination.ContractIssue) string {
	var prod, cons, both coordination.Severity
	for _, is := range issues {
		switch is.Kind {
		case coordination.IssueMissingOut:
			prod = max(prod, is.Severity)
		case coordination.IssueMissingIn:
			cons = max(cons, is.Severity)
		case coordination.IssueTypeMismatch:
			both = max(both, is.Severity)
		}
	}
	switch {
	case prod > cons && prod > both:
		return FaultProducer
	case cons > prod && cons > both:
		return FaultConsumer
	default:
		return FaultBoth
	}
}

// assessContractPair compares one producer with one consumer on one contract.
// Equal hashes are the fast path: identical canonical shapes cannot disagree.
func assessContractPair(prod, cons contractSide) (contractFinding, bool) {
	if prod.SessionUUID == cons.SessionUUID || prod.Hash == cons.Hash || !prod.ShapeOK || !cons.ShapeOK {
		return contractFinding{}, false
	}
	breaking, naming := splitContractIssues(coordination.CompareShapes(prod.Shape, cons.Shape))
	f := contractFinding{Producer: prod, Consumer: cons, Where: cons.ContractKey}
	switch {
	case len(breaking) > 0:
		f.Kind = enums.CONFLICT_KIND_CONTRACT_MISMATCH
		f.Issues = append(append([]coordination.ContractIssue{}, breaking...), naming...)
		f.Fault = ContractFault(breaking)
		f.Severity, f.Rule = worstContractIssue(breaking)
		f.Dedupe = ContractMismatchDedupeKey(prod.ContractUUID, prod.SessionUUID, cons.SessionUUID)
	case len(naming) > 0:
		// PLAN.md: naming variants are "low, usually a serializer detail".
		f.Kind = enums.CONFLICT_KIND_CONTRACT_NAMING_VARIANT
		f.Issues = naming
		f.Severity, f.Rule = coordination.SeverityLow, RuleContractFieldVariant
		f.Dedupe = ContractFieldVariantDedupeKey(prod.ContractUUID, prod.SessionUUID, cons.SessionUUID)
	default:
		return contractFinding{}, false
	}
	return f, true
}

func worstContractIssue(issues []coordination.ContractIssue) (coordination.Severity, string) {
	var (
		sev  coordination.Severity
		rule string
	)
	for _, is := range issues {
		if is.Severity <= sev {
			continue
		}
		sev = is.Severity
		switch is.Kind {
		case coordination.IssueMissingOut:
			rule = RuleContractMissingOut
		case coordination.IssueMissingIn:
			rule = RuleContractMissingIn
		default:
			rule = RuleContractType
		}
	}
	return sev, rule
}

// keyVariantFinding is a consumer on a key nobody produces beside a producer
// on a key one or two edits away. Low, per PLAN.md's naming-variant severity:
// it is recorded and on the board, below the notify floor; the consumer is
// still told once the wait passes the unclaimed threshold.
func keyVariantFinding(prod, cons contractSide) contractFinding {
	return contractFinding{
		Kind:     enums.CONFLICT_KIND_CONTRACT_NAMING_VARIANT,
		Rule:     RuleContractKeyVariant,
		Dedupe:   ContractKeyVariantDedupeKey(cons.ContractUUID, prod.ContractUUID, cons.SessionUUID, prod.SessionUUID),
		Severity: coordination.SeverityLow,
		Producer: prod,
		Consumer: cons,
		Where:    cons.ContractKey,
	}
}

// ContractKeysAreVariants reports two normalized keys of one kind that are
// probably one interface spelled two ways: same HTTP verb (or none), the rest
// within Levenshtein 2, and neither trivially short. Trailing slashes, letter
// case and path-parameter spellings never get here — NormalizeContractKey
// already folds them into one contract.
func ContractKeysAreVariants(aNorm, bNorm string) bool {
	if aNorm == bNorm {
		return false
	}
	av, ap := splitContractVerb(aNorm)
	bv, bp := splitContractVerb(bNorm)
	if av != bv || utf8.RuneCountInString(ap) < contractKeyVariantMinChars || utf8.RuneCountInString(bp) < contractKeyVariantMinChars {
		return false
	}
	d := coordination.ContractKeyDistance(ap, bp)
	return d >= 1 && d <= contractKeyVariantMaxDistance
}

// splitContractVerb drops the kind prefix and splits off an HTTP verb.
func splitContractVerb(norm string) (verb, rest string) {
	if i := strings.Index(norm, ":"); i >= 0 && !strings.Contains(norm[:i], "/") && !strings.Contains(norm[:i], " ") {
		norm = norm[i+1:]
	}
	if i := strings.Index(norm, " "); i >= 0 {
		return norm[:i], norm[i+1:]
	}
	return "", norm
}

// ─────────────────────────────────────────────
// The hook
// ─────────────────────────────────────────────

// detectContractConflicts is publish_contract's DetectHook.
func (h *Handler) detectContractConflicts(ctx context.Context, tc *TxContext, m *Mutation, p *contractPublication) ([]ConflictNotice, error) {
	cid := p.ContractUUID
	m.SubjectUUID = &cid
	m.SubjectKey = truncate(p.ContractKey, 64)
	m.Payload.ContractUUID = &cid

	sides, err := loadLiveContractSides(ctx, tc.Tx, cid.String(), enums.ASSERTION_ROLE_INVALID, contractMaxSidesScanned)
	if err != nil {
		return nil, err
	}
	var mine *contractSide
	producers := 0
	for i := range sides {
		if sides[i].SessionUUID == p.SessionUUID.String() && sides[i].Role == p.In.Role {
			mine = &sides[i]
		}
		if sides[i].Role == enums.ASSERTION_ROLE_PRODUCES && sides[i].SessionUUID != p.SessionUUID.String() {
			producers++
		}
	}
	if mine == nil {
		return nil, errors.New("the assertion this call wrote is not visible to detection")
	}

	var findings []contractFinding
	for _, other := range sides {
		if other.SessionUUID == mine.SessionUUID || other.Role == mine.Role {
			continue
		}
		prod, cons := *mine, other
		if mine.Role == enums.ASSERTION_ROLE_CONSUMES {
			prod, cons = other, *mine
		}
		if f, ok := assessContractPair(prod, cons); ok {
			findings = append(findings, f)
		}
	}
	variants, err := findContractKeyVariants(ctx, tc.Tx, p, *mine, producers > 0)
	if err != nil {
		return nil, err
	}
	findings = append(findings, variants...)

	// Worst first, stable, so the same world always renders the same
	// response and the stored snapshot is worth replaying.
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity > findings[j].Severity
		}
		return findings[i].Dedupe < findings[j].Dedupe
	})

	var (
		notices []ConflictNotice
		topID   *uuid.UUID
		topSev  coordination.Severity
	)
	for i, f := range findings {
		rec, err := h.recordContractConflict(ctx, tc, p, f, i)
		if err != nil {
			return nil, err
		}
		if rec.Silenced {
			continue
		}
		if topID == nil || f.Severity > topSev {
			id := rec.ID
			topID, topSev = &id, f.Severity
		}
		if f.Severity < notifySeverityFloor || len(notices) >= detectMaxNotices {
			continue
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"UPDATE `conflict_participant` SET `notified_at` = COALESCE(`notified_at`, ?), `updated_at` = ? "+
				"WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
			tc.Now, tc.Now, rec.ID.String(), p.SessionUUID.String()); err != nil {
			return nil, retryable(err, "marking the caller notified of the conflict")
		}
		notices = append(notices, contractNotice(rec, f, p))
	}

	if topID != nil {
		m.Payload.ConflictUUID = topID
		m.Payload.Severity = severityEnum(topSev)
	}
	if p.In.Role == enums.ASSERTION_ROLE_CONSUMES && producers == 0 {
		m.Envelope.Note += "; nobody produces it yet — find out who owns it before you build on this shape (metiche tells the team if it stays unproduced)"
	}
	if len(notices) > 0 {
		m.Envelope.Note += fmt.Sprintf("; %d contract conflict(s) — read conflicts[] before you write the code", len(notices))
	}
	return notices, nil
}

// findContractKeyVariants looks for the /api/session vs /api/sessions case.
//
// One scan over the project's contracts of the same kind through
// uq_contract_key_norm (project_uuid, key_norm), capped, with the distance
// computed in Go; then at most contractMaxVariantMatches point reads.
func findContractKeyVariants(ctx context.Context, q queryer, p *contractPublication, mine contractSide, hereHasProducer bool) ([]contractFinding, error) {
	if mine.Role == enums.ASSERTION_ROLE_CONSUMES && hereHasProducer {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx,
		"SELECT `id`, `key_norm` FROM `contract` WHERE `project_uuid` = ? AND `kind` = ? AND `id` <> ? AND `status` IN (?, ?) "+
			"ORDER BY `key_norm` LIMIT ?",
		p.ProjectUUID.String(), int64(p.In.Kind), p.ContractUUID.String(),
		int64(enums.CONTRACT_STATUS_PROPOSED), int64(enums.CONTRACT_STATUS_PUBLISHED), contractMaxNearContracts)
	if err != nil {
		return nil, retryable(err, "looking for near-identical contract keys")
	}
	var near []string
	for rows.Next() {
		var id, norm string
		if err := rows.Scan(&id, &norm); err != nil {
			_ = rows.Close()
			return nil, retryable(err, "looking for near-identical contract keys")
		}
		if len(near) < contractMaxVariantMatches && ContractKeysAreVariants(p.In.KeyNorm, norm) {
			near = append(near, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, retryable(err, "looking for near-identical contract keys")
	}
	_ = rows.Close()

	var out []contractFinding
	for _, id := range near {
		if mine.Role == enums.ASSERTION_ROLE_CONSUMES {
			prods, err := loadLiveContractSides(ctx, q, id, enums.ASSERTION_ROLE_PRODUCES, 2)
			if err != nil {
				return nil, err
			}
			for _, prod := range prods {
				if prod.SessionUUID != mine.SessionUUID {
					out = append(out, keyVariantFinding(prod, mine))
				}
			}
			continue
		}
		// I produce: a near key's consumers are only a variant of mine when
		// nobody produces theirs.
		prods, err := loadLiveContractSides(ctx, q, id, enums.ASSERTION_ROLE_PRODUCES, 1)
		if err != nil {
			return nil, err
		}
		if len(prods) > 0 {
			continue
		}
		cons, err := loadLiveContractSides(ctx, q, id, enums.ASSERTION_ROLE_CONSUMES, contractMaxVariantMatches)
		if err != nil {
			return nil, err
		}
		for _, c := range cons {
			if c.SessionUUID != mine.SessionUUID {
				out = append(out, keyVariantFinding(mine, c))
			}
		}
	}
	return out, nil
}

// ─────────────────────────────────────────────
// Writing it down
// ─────────────────────────────────────────────

type recordedContractConflict struct {
	ID  uuid.UUID
	Key string
	// Fresh is a conflict inserted, or reopened after metiche had settled it.
	Fresh bool
	// Silenced is a conflict a person dismissed or resolved: counted, never
	// re-announced.
	Silenced     bool
	CallerAction string
}

// recordContractConflict upserts one conflict, its two participants and the
// notice for the other side. Under the team lock, so check-then-write is safe.
func (h *Handler) recordContractConflict(ctx context.Context, tc *TxContext, p *contractPublication, f contractFinding, n int) (recordedContractConflict, error) {
	caller, other := f.Consumer, f.Producer
	if f.Producer.SessionUUID == p.SessionUUID.String() {
		caller, other = f.Producer, f.Consumer
	}
	rec := recordedContractConflict{CallerAction: contractAction(f, caller.Role, false)}
	otherAction := contractAction(f, other.Role, true)

	summaries := make([]string, 0, len(f.Issues))
	for i, is := range f.Issues {
		if i >= contractMaxEvidenceIssues {
			break
		}
		summaries = append(summaries, ContractIssueSummary(is))
	}
	evidence, err := json.Marshal(conflict_evidence_entity.ConflictEvidence{
		OverlapPath: nullString(truncate(f.Where, 255)),
		ALabel:      nullString("produces: " + f.Producer.holder()),
		APattern:    nullString(truncate(f.Producer.ContractKey, 255)),
		BLabel:      nullString("consumes: " + f.Consumer.holder()),
		BPattern:    nullString(truncate(f.Consumer.ContractKey, 255)),
		FieldIssues: summaries,
		Detail:      nullString(f.Rule),
	})
	if err != nil {
		return rec, err
	}
	evidenceJSON := string(evidence)

	var yieldSession, yieldReason any
	switch f.Fault {
	case FaultProducer:
		yieldSession, yieldReason = f.Producer.SessionUUID, "producer lacks what the consumer requires"
	case FaultConsumer:
		yieldSession, yieldReason = f.Consumer.SessionUUID, "consumer lacks what the producer requires"
	}
	sev := severityEnum(f.Severity)

	var (
		rawID, key string
		status     int64
		resolvedBy sql.NullString
		maxNotif   sql.NullInt64
	)
	err = tc.Tx.QueryRowContext(ctx,
		"SELECT `id`, `key`, `status`, `resolved_by_member_uuid`, `max_severity_notified` FROM `conflict` WHERE `team_uuid` = ? AND `dedupe_key` = ?",
		p.TeamUUID.String(), f.Dedupe).Scan(&rawID, &key, &status, &resolvedBy, &maxNotif)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, err := uuid.NewV4()
		if err != nil {
			return rec, err
		}
		rec.ID, rec.Key, rec.Fresh = id, conflictKey(tc, n), true
		if _, err := tc.Tx.ExecContext(ctx,
			"INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
				"`detector_rule`,`evidence`,`suggested_action`,`suggested_yield_session_uuid`,`suggested_yield_reason`,`occurrence_count`,"+
				"`first_detected_at`,`last_detected_at`,`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)",
			id.String(), p.TeamUUID.String(), p.ProjectUUID.String(), rec.Key, int64(f.Kind), f.Dedupe, int64(sev),
			int64(enums.CONFLICT_STATUS_OPEN), int64(enums.DETECTED_BY_SERVER), f.Rule, evidenceJSON, rec.CallerAction,
			yieldSession, yieldReason, tc.Now, tc.Now, tc.Now, tc.Now); err != nil {
			return rec, retryable(err, "recording the contract conflict")
		}
	case err != nil:
		return rec, retryable(err, "looking up the contract conflict")
	default:
		rec.ID, err = uuid.FromString(rawID)
		if err != nil {
			return rec, err
		}
		rec.Key = key
		st := enums.ConflictStatus(status)
		switch {
		case st == enums.CONFLICT_STATUS_OPEN || st == enums.CONFLICT_STATUS_ACKNOWLEDGED || st == enums.CONFLICT_STATUS_RESOLVING:
			// Severity follows the current disagreement rather than only
			// climbing: a half-fixed contract is less broken than it was.
			if _, err := tc.Tx.ExecContext(ctx,
				"UPDATE `conflict` SET `occurrence_count` = `occurrence_count` + 1, `last_detected_at` = ?, `severity` = ?, "+
					"`evidence` = ?, `suggested_action` = ?, `detector_rule` = ?, `suggested_yield_session_uuid` = ?, "+
					"`suggested_yield_reason` = ?, `updated_at` = ? WHERE `id` = ?",
				tc.Now, int64(sev), evidenceJSON, rec.CallerAction, f.Rule, yieldSession, yieldReason, tc.Now, rawID); err != nil {
				return rec, retryable(err, "updating the contract conflict")
			}
		case st == enums.CONFLICT_STATUS_RESOLVED && !resolvedBy.Valid:
			// metiche closed it (the shapes had agreed, or a side had gone)
			// and the disagreement is back: the same conflict reopens rather
			// than a second row appearing, and it is news again.
			rec.Fresh = true
			maxNotif = sql.NullInt64{}
			if _, err := tc.Tx.ExecContext(ctx,
				"UPDATE `conflict` SET `status` = ?, `resolution` = NULL, `resolution_note` = NULL, `resolved_at` = NULL, "+
					"`notified_at` = NULL, `max_severity_notified` = NULL, `occurrence_count` = `occurrence_count` + 1, "+
					"`last_detected_at` = ?, `severity` = ?, `evidence` = ?, `suggested_action` = ?, `detector_rule` = ?, "+
					"`suggested_yield_session_uuid` = ?, `suggested_yield_reason` = ?, `updated_at` = ? WHERE `id` = ?",
				int64(enums.CONFLICT_STATUS_OPEN), tc.Now, int64(sev), evidenceJSON, rec.CallerAction, f.Rule,
				yieldSession, yieldReason, tc.Now, rawID); err != nil {
				return rec, retryable(err, "reopening the contract conflict")
			}
		default:
			// A person dismissed or resolved it: count it, never shout again.
			rec.Silenced = true
			_, err := tc.Tx.ExecContext(ctx,
				"UPDATE `conflict` SET `occurrence_count` = `occurrence_count` + 1, `last_detected_at` = ?, `updated_at` = ? WHERE `id` = ?",
				tc.Now, tc.Now, rawID)
			return rec, retryable(err, "counting the contract conflict")
		}
	}

	if err := upsertContractParticipant(ctx, tc, p.TeamUUID, rec.ID, caller, enums.PARTICIPANT_ROLE_INITIATOR); err != nil {
		return rec, err
	}
	if err := upsertContractParticipant(ctx, tc, p.TeamUUID, rec.ID, other, enums.PARTICIPANT_ROLE_INCUMBENT); err != nil {
		return rec, err
	}

	// The other side cannot be pushed to, so it gets an instruction that
	// rides on its next call. Re-notified only when the conflict is new again
	// or got worse: the pair is re-detected on every publish either side makes.
	if f.Severity >= notifySeverityFloor && (rec.Fresh || !maxNotif.Valid || int64(sev) > maxNotif.Int64) {
		instrID, err := uuid.NewV4()
		if err != nil {
			return rec, err
		}
		body := fmt.Sprintf("%s (%s) on %s: %s", rec.Key, f.Severity.String(), f.Where, otherAction)
		if _, err := tc.Tx.ExecContext(ctx,
			"INSERT INTO `instruction` (`id`, `team_uuid`, `target_session_uuid`, `target_agent_uuid`, `key`, "+
				"`source`, `kind`, `body`, `ref_kind`, `ref_uuid`, `requires_report`, `status`, "+
				"`expires_at`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)",
			instrID.String(), p.TeamUUID.String(), other.SessionUUID, other.AgentUUID, instructionKey(tc, n),
			int64(enums.INSTRUCTION_SOURCE_SERVER), int64(enums.INSTRUCTION_KIND_CONFLICT_NOTICE),
			truncate(body, 600), int64(enums.SUBJECT_KIND_CONFLICT), rec.ID.String(),
			int64(enums.INSTRUCTION_STATUS_PENDING), tc.Now.Add(detectInstructionTTL), tc.Now, tc.Now); err != nil {
			return rec, retryable(err, "queueing the contract notice for the other side")
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"UPDATE `conflict` SET `notified_at` = ?, `max_severity_notified` = ?, `updated_at` = ? WHERE `id` = ?",
			tc.Now, int64(sev), tc.Now, rec.ID.String()); err != nil {
			return rec, retryable(err, "marking the contract conflict notified")
		}
	}
	return rec, nil
}

// upsertContractParticipant attaches one side once, and keeps its subject
// pointing at that side's current assertion revision.
func upsertContractParticipant(ctx context.Context, tc *TxContext, teamUUID, conflictID uuid.UUID, s contractSide, role enums.ParticipantRole) error {
	var exists int
	err := tc.Tx.QueryRowContext(ctx,
		"SELECT 1 FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? LIMIT 1",
		conflictID.String(), s.SessionUUID).Scan(&exists)
	switch {
	case err == nil:
		_, err := tc.Tx.ExecContext(ctx,
			"UPDATE `conflict_participant` SET `subject_uuid` = ?, `updated_at` = ? WHERE `conflict_uuid` = ? AND `session_uuid` = ?",
			s.AssertionUUID, tc.Now, conflictID.String(), s.SessionUUID)
		return retryable(err, "updating the conflict participant")
	case !errors.Is(err, sql.ErrNoRows):
		return retryable(err, "checking the conflict participants")
	}
	id, err := uuid.NewV4()
	if err != nil {
		return err
	}
	_, err = tc.Tx.ExecContext(ctx,
		"INSERT INTO `conflict_participant` (`id`, `conflict_uuid`, `team_uuid`, `session_uuid`, `agent_uuid`, "+
			"`member_uuid`, `subject_kind`, `subject_uuid`, `role`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id.String(), conflictID.String(), teamUUID.String(), s.SessionUUID, s.AgentUUID, s.MemberUUID,
		int64(enums.SUBJECT_KIND_CONTRACT_ASSERTION), s.AssertionUUID, int64(role), tc.Now, tc.Now)
	return retryable(err, "attaching the conflict participant")
}

// ─────────────────────────────────────────────
// What the agents read
// ─────────────────────────────────────────────

func contractNotice(rec recordedContractConflict, f contractFinding, p *contractPublication) ConflictNotice {
	other := f.Producer
	if f.Producer.SessionUUID == p.SessionUUID.String() {
		other = f.Consumer
	}
	var fields []string
	for i, is := range f.Issues {
		if i >= contractMaxNoticeFields {
			break
		}
		fields = append(fields, ContractIssueSummary(is))
	}
	return ConflictNotice{
		Key:             rec.Key,
		Kind:            f.Kind.String(),
		Severity:        f.Severity.String(),
		With:            other.holder(),
		Contract:        f.Where,
		AtFault:         f.Fault,
		Fields:          fields,
		SuggestedAction: rec.CallerAction,
	}
}

// ContractIssueSummary is one issue in one line, stored in the conflict's
// evidence and returned in ConflictNotice.Fields.
func ContractIssueSummary(is coordination.ContractIssue) string {
	switch is.Kind {
	case coordination.IssueMissingOut:
		return fmt.Sprintf("missing_out: response field `%s` (%s) the consumer requires is not returned by the producer", is.Path, is.Expected)
	case coordination.IssueMissingIn:
		return fmt.Sprintf("missing_in: request field `%s` (%s) the producer requires is not sent by the consumer", is.Path, is.Expected)
	case coordination.IssueTypeMismatch:
		return fmt.Sprintf("type_mismatch: %s field `%s` is %s on the producer, %s on the consumer", directionWord(is.Direction), is.Path, is.Expected, is.Actual)
	case coordination.IssueNamingVariant:
		return fmt.Sprintf("naming_variant: %s field `%s` on the producer, `%s` on the consumer", directionWord(is.Direction), is.Expected, is.Actual)
	}
	return string(is.Kind) + ": " + is.Path
}

func directionWord(d coordination.ContractDirection) string {
	if d == coordination.DirectionIn {
		return "request"
	}
	return "response"
}

// issuesPhrase names at most n issues for a sentence, spelling first.
func issuesPhrase(issues []coordination.ContractIssue, n int) string {
	breaking, naming := splitContractIssues(issues)
	use := breaking
	if len(use) == 0 {
		use = naming
	}
	parts := make([]string, 0, len(use))
	for _, is := range use {
		switch is.Kind {
		case coordination.IssueMissingOut:
			parts = append(parts, fmt.Sprintf("response field `%s` (%s)", is.Path, is.Expected))
		case coordination.IssueMissingIn:
			parts = append(parts, fmt.Sprintf("request field `%s` (%s)", is.Path, is.Expected))
		case coordination.IssueTypeMismatch:
			parts = append(parts, fmt.Sprintf("`%s` (producer %s, consumer %s)", is.Path, is.Expected, is.Actual))
		default:
			parts = append(parts, fmt.Sprintf("`%s` vs `%s`", is.Expected, is.Actual))
		}
	}
	if len(parts) > n {
		return strings.Join(parts[:n], ", ") + fmt.Sprintf(" +%d more", len(parts)-n)
	}
	return strings.Join(parts, ", ")
}

// contractAction is what one side should do, from that side's point of view.
// Never empty. short is the version the other side receives through
// get_instructions, which delivers at most instructionTextChars.
func contractAction(f contractFinding, reader enums.AssertionRole, short bool) string {
	prod, cons := f.Producer.holder(), f.Consumer.holder()
	n, limit := 3, 400
	if short {
		n, limit = 1, instructionTextChars
	}
	what := issuesPhrase(f.Issues, n)
	var s string
	switch {
	case f.Rule == RuleContractKeyVariant && reader == enums.ASSERTION_ROLE_CONSUMES:
		s = fmt.Sprintf("Nobody produces %s, but %s produces %s. If that is the one you mean, publish_contract consumes on %s; if not, ask who builds %s.",
			f.Consumer.ContractKey, prod, f.Producer.ContractKey, f.Producer.ContractKey, f.Consumer.ContractKey)
	case f.Rule == RuleContractKeyVariant:
		s = fmt.Sprintf("%s consumes %s, which nobody produces, and you produce %s. If they mean yours, tell them.",
			cons, f.Consumer.ContractKey, f.Producer.ContractKey)
	case f.Kind == enums.CONFLICT_KIND_CONTRACT_NAMING_VARIANT:
		s = fmt.Sprintf("Same field, different spelling on %s: %s. Usually a serializer setting: agree on one spelling and publish_contract again.", f.Where, what)
	case f.Fault == FaultProducer && reader == enums.ASSERTION_ROLE_PRODUCES:
		s = fmt.Sprintf("%s consumes %s and requires %s, which your shape lacks. Add it and publish_contract again; this closes when the shapes agree.", cons, f.Where, what)
	case f.Fault == FaultProducer:
		s = fmt.Sprintf("%s produces %s without %s, which you require. Don't build on it yet: they were told to add it. If you don't need it, drop it and publish_contract again.", prod, f.Where, what)
	case f.Fault == FaultConsumer && reader == enums.ASSERTION_ROLE_CONSUMES:
		s = fmt.Sprintf("%s produces %s and requires %s, which you don't send. Send it and publish_contract again; this closes when the shapes agree.", prod, f.Where, what)
	case f.Fault == FaultConsumer:
		s = fmt.Sprintf("%s consumes %s without %s, which you require. They were told to send it; if it is optional, make it so and publish_contract again.", cons, f.Where, what)
	default:
		other := prod
		if reader == enums.ASSERTION_ROLE_PRODUCES {
			other = cons
		}
		s = fmt.Sprintf("You and %s disagree on %s: %s. Agree which side changes; whoever changes calls publish_contract again, and this closes when the shapes agree.", other, f.Where, what)
	}
	return clip(s, limit)
}
