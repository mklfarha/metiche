package sweeper

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
	"github.com/guregu/null/v6"

	conflict_evidence_entity "github.com/mklfarha/metiche/backend/entity/conflict_evidence"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// DetectorRuleUnclaimed is the rule name written to conflict.detector_rule
// and the name a team uses to demote this rule in team.settings.demoted_rules.
const DetectorRuleUnclaimed = "contract_unclaimed"

// unclaimedConsumer is one `consumes` assertion with nobody on the other end.
type unclaimedConsumer struct {
	assertionUUID uuid.UUID
	contractUUID  uuid.UUID
	projectUUID   uuid.UUID
	sessionUUID   uuid.UUID
	memberUUID    uuid.UUID
	agentUUID     uuid.UUID
	sessionKey    string
	contractKey   string
	contractKind  enums.ContractKind
	cadence       enums.ProjectCadence
	waited        time.Duration
}

// UnclaimedDedupeKey is the stable identity of one contract_unclaimed
// conflict: this session, consuming this contract, with no producer.
//
// It is keyed on the SESSION rather than on the assertion row, and that is
// deliberate. Re-publishing a consumer assertion supersedes the old row and
// mints a new uuid, so an assertion-keyed dedupe would raise the same
// complaint again every time the consumer refined its shape — which is
// exactly the "cries wolf" failure PLAN.md spends a section forbidding.
func UnclaimedDedupeKey(contractUUID, sessionUUID string) string {
	sum := sha256.Sum256([]byte(DetectorRuleUnclaimed + "|" + contractUUID + "|" + sessionUUID))
	return hex.EncodeToString(sum[:])
}

// detectUnclaimed raises `contract_unclaimed`.
//
// PLAN.md: "a `consumes` assertion with no active `produces` for >5 minutes,
// i.e. *'Bob is coding against an endpoint nobody is building'*, which in a
// hackathon is the single highest-value signal in the system."
//
// It lives in the sweeper rather than in a tool call for a structural reason:
// what makes it true is elapsed time. Nothing an agent says creates it — the
// consumer said its piece correctly and the conflict is that the OTHER call
// never came. No tool invocation can be the trigger for an event defined by
// absence.
//
// The threshold is the project's cadence: five minutes is right for a
// hackathon and absurd for a steady team where the backend lands next
// Tuesday. MODEL.md puts cadence on `project` for exactly this.
func (s *Sweeper) detectUnclaimed(ctx context.Context, t teamRow, now time.Time, rep *Report) error {
	candidates, err := s.loadUnclaimedConsumers(ctx, t, now)
	if err != nil {
		return err
	}

	raised := 0
	for _, c := range candidates {
		if raised >= s.opts.MaxUnclaimedPerPass {
			return nil
		}
		// The coarse filter in SQL used the shortest cadence threshold so one
		// query serves every project; the exact per-project threshold is
		// applied here.
		if c.waited < s.opts.unclaimedThreshold(c.cadence) {
			continue
		}

		dedupe := UnclaimedDedupeKey(c.contractUUID.String(), c.sessionUUID.String())

		// Outside the lock: if this conflict already exists, re-detection is
		// a counter bump on an unrelated row and must not queue behind the
		// team's agents, let alone write a second event.
		bumped, existed, err := s.bumpExistingConflict(ctx, t.uuid, dedupe, now)
		if err != nil {
			rep.addErr("bumping an existing contract_unclaimed conflict", err)
			continue
		}
		if existed {
			if bumped {
				rep.UnclaimedReDetected++
			}
			continue
		}

		if err := s.raiseUnclaimed(ctx, t, c, dedupe, rep); err != nil {
			rep.addErr("raising contract_unclaimed for "+c.contractKey, err)
			continue
		}
		raised++
	}
	return nil
}

// loadUnclaimedConsumers finds active consumers with no active producer.
//
// NOT EXISTS rather than a LEFT JOIN with an IS NULL test: the anti-join reads
// as the sentence the rule is ("no active produces on this contract"), and it
// short-circuits on the first producer it finds through
// idx_contract_assertion_role (contract_uuid, status, role).
//
// The contract filter takes proposed AND published: a contract somebody is
// already coding against is exactly the one likely to still be proposed, and
// excluding it would mean the detector only fires once the thing is settled,
// which is the moment it stops being useful. Deprecated and retired contracts
// are excluded — nobody is expected to produce those.
//
// The session filter is noise control, not an optimisation. A consumer whose
// session ended or was abandoned is not waiting for anybody, and telling the
// team that a finished session is blocked is how a detector teaches an agent
// to stop reading its output.
func (s *Sweeper) loadUnclaimedConsumers(ctx context.Context, t teamRow, now time.Time) ([]unclaimedConsumer, error) {
	// The shortest configured threshold is the coarse cut-off; anything
	// younger than that cannot be a conflict under any cadence.
	shortest := s.opts.UnclaimedHackathon
	for _, d := range []time.Duration{s.opts.UnclaimedSprint, s.opts.UnclaimedSteady} {
		if d < shortest {
			shortest = d
		}
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT ca.`id`, ca.`contract_uuid`, ca.`project_uuid`, ca.`session_uuid`, ca.`member_uuid`, "+
			"se.`agent_uuid`, se.`key`, c.`key`, c.`kind`, p.`cadence`, "+
			"COALESCE(ca.`asserted_at`, ca.`created_at`) AS `since` "+
			"FROM `contract_assertion` ca "+
			"JOIN `contract` c ON c.`id` = ca.`contract_uuid` "+
			"JOIN `project` p ON p.`id` = ca.`project_uuid` "+
			"JOIN `session` se ON se.`id` = ca.`session_uuid` "+
			"WHERE ca.`team_uuid` = ? AND ca.`status` = ? AND ca.`role` = ? "+
			"AND c.`status` IN (?,?) AND se.`status` IN (?,?) "+
			"AND COALESCE(ca.`asserted_at`, ca.`created_at`) <= ? "+
			"AND NOT EXISTS (SELECT 1 FROM `contract_assertion` pa "+
			"WHERE pa.`contract_uuid` = ca.`contract_uuid` AND pa.`role` = ? AND pa.`status` = ?) "+
			"ORDER BY `since` LIMIT ?",
		t.uuid.String(),
		int64(enums.ASSERTION_STATUS_ACTIVE), int64(enums.ASSERTION_ROLE_CONSUMES),
		int64(enums.CONTRACT_STATUS_PROPOSED), int64(enums.CONTRACT_STATUS_PUBLISHED),
		int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE),
		now.Add(-shortest),
		int64(enums.ASSERTION_ROLE_PRODUCES), int64(enums.ASSERTION_STATUS_ACTIVE),
		s.opts.MaxUnclaimedPerPass*4)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []unclaimedConsumer
	for rows.Next() {
		var (
			id, contract, project, session, member string
			agent, sessionKey, contractKey         string
			kind, cadence                          int64
			since                                  sql.NullTime
		)
		if err := rows.Scan(&id, &contract, &project, &session, &member,
			&agent, &sessionKey, &contractKey, &kind, &cadence, &since); err != nil {
			return nil, err
		}
		c := unclaimedConsumer{
			sessionKey:   sessionKey,
			contractKey:  contractKey,
			contractKind: enums.ContractKind(kind),
			cadence:      enums.ProjectCadence(cadence),
		}
		var err error
		if c.assertionUUID, err = uuid.FromString(id); err != nil {
			continue
		}
		c.contractUUID, _ = uuid.FromString(contract)
		c.projectUUID, _ = uuid.FromString(project)
		c.sessionUUID, _ = uuid.FromString(session)
		c.memberUUID, _ = uuid.FromString(member)
		c.agentUUID, _ = uuid.FromString(agent)
		if since.Valid {
			c.waited = now.Sub(since.Time.UTC())
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// bumpExistingConflict turns a re-detection into a counter bump.
//
// PLAN.md: "Unique (team_uuid, dedupe_key) so re-detection bumps a counter
// instead of spamming." Resolved and dismissed conflicts are left completely
// alone — a human who dismissed this must not watch its counter tick upward
// every thirty seconds — and returning existed=true for them is what keeps
// the sweeper from raising it again.
func (s *Sweeper) bumpExistingConflict(ctx context.Context, teamUUID uuid.UUID, dedupe string, now time.Time) (bumped, existed bool, err error) {
	var status int64
	err = s.db.QueryRowContext(ctx,
		"SELECT `status` FROM `conflict` WHERE `team_uuid` = ? AND `dedupe_key` = ?",
		teamUUID.String(), dedupe).Scan(&status)
	switch {
	case err == sql.ErrNoRows:
		return false, false, nil
	case err != nil:
		return false, false, err
	}

	switch enums.ConflictStatus(status) {
	case enums.CONFLICT_STATUS_OPEN, enums.CONFLICT_STATUS_ACKNOWLEDGED, enums.CONFLICT_STATUS_RESOLVING:
		n, err := s.execCount(ctx,
			"UPDATE `conflict` SET `occurrence_count` = `occurrence_count` + 1, `last_detected_at` = ?, `updated_at` = ? "+
				"WHERE `team_uuid` = ? AND `dedupe_key` = ?",
			now, now, teamUUID.String(), dedupe)
		if err != nil {
			return false, true, err
		}
		return n > 0, true, nil
	default:
		return false, true, nil
	}
}

// raiseUnclaimed writes the conflict, its participant, the instruction that
// will reach the consumer on its next call, and the event that puts it on the
// board — all in one transaction under the team lock, so the short keys
// (CF-17, I-17) can be minted from the sequence the event will carry.
//
// Four small inserts and one update. That is the whole lock hold.
func (s *Sweeper) raiseUnclaimed(ctx context.Context, t teamRow, c unclaimedConsumer, dedupe string, rep *Report) error {
	severity := s.opts.UnclaimedSeverity
	notify := int64(severity) >= int64(t.notifyFloor) && !t.ruleDemoted(DetectorRuleUnclaimed)

	suggested := fmt.Sprintf(
		"Nobody has published a producer for %s. Ask who owns it, or claim it yourself before building against a shape that may not land.",
		c.contractKey)

	evidence := conflict_evidence_entity.ConflictEvidence{
		ASummary: null.StringFrom(fmt.Sprintf("session %s consumes %s", c.sessionKey, c.contractKey)),
		BSummary: null.StringFrom("no active producer"),
		Detail: null.StringFrom(fmt.Sprintf("waiting %s, past the %s threshold for a %s project",
			c.waited.Round(time.Second), s.opts.unclaimedThreshold(c.cadence), c.cadence.String())),
	}

	conflictUUID, err := uuid.NewV4()
	if err != nil {
		return err
	}
	participantUUID, err := uuid.NewV4()
	if err != nil {
		return err
	}
	instructionUUID, err := uuid.NewV4()
	if err != nil {
		return err
	}

	payload := payload_entity.EventPayload{
		Severity:     severity,
		ConflictUUID: &conflictUUID,
		ContractUUID: &c.contractUUID,
		Message:      null.StringFrom("no producer for " + c.contractKey),
		Detail:       null.StringFrom(suggested),
	}

	var conflictKey string
	wrote, err := s.appendEvent(ctx, sweepEvent{
		teamUUID: t.uuid,
		// Derived from the dedupe key, so the event is deduped by exactly the
		// same identity the conflict is. Two pods detecting the same absence
		// in the same instant write one conflict and one event.
		idempotencyKey: "sweep:unclaimed:" + dedupe,
		kind:           enums.EVENT_KIND_CONFLICT_RAISED,
		projectUUID:    &c.projectUUID,
		sessionUUID:    &c.sessionUUID,
		agentUUID:      &c.agentUUID,
		memberUUID:     &c.memberUUID,
		subjectKind:    enums.SUBJECT_KIND_CONFLICT,
		subjectUUID:    &conflictUUID,
		summary:        "nobody is producing " + c.contractKey,
		payload:        payload.ToJSON(),
		extra: func(ctx context.Context, tx *sql.Tx, seq int64, now time.Time) error {
			conflictKey = fmt.Sprintf("CF-%d", seq)

			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,"+
					"`detected_by`,`detector_rule`,`evidence`,`suggested_action`,`occurrence_count`,"+
					"`first_detected_at`,`last_detected_at`,`notified_at`,`max_severity_notified`,`created_at`,`updated_at`) "+
					"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
				conflictUUID.String(), t.uuid.String(), c.projectUUID.String(), conflictKey,
				int64(enums.CONFLICT_KIND_CONTRACT_UNCLAIMED), dedupe,
				int64(severity), int64(enums.CONFLICT_STATUS_OPEN),
				int64(enums.DETECTED_BY_SERVER), DetectorRuleUnclaimed,
				string(evidence.ToJSON()), truncate(suggested, 400), 1,
				now, now,
				nullableTimeIf(notify, now), nullableSeverityIf(notify, severity),
				now, now); err != nil {
				return err
			}

			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
					"`subject_kind`,`subject_uuid`,`role`,`notified_at`,`created_at`,`updated_at`) "+
					"VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
				participantUUID.String(), conflictUUID.String(), t.uuid.String(),
				c.sessionUUID.String(), c.agentUUID.String(), c.memberUUID.String(),
				int64(enums.SUBJECT_KIND_CONTRACT_ASSERTION), c.assertionUUID.String(),
				int64(enums.PARTICIPANT_ROLE_INITIATOR), nullableTimeIf(notify, now),
				now, now); err != nil {
				return err
			}

			if !notify {
				return nil
			}

			// The instruction IS the push. MCP cannot push, so the count of
			// these rides on every tool response and the consumer learns on
			// its next call — including a bare heartbeat.
			body := fmt.Sprintf("No active producer for %s (waiting %s). %s",
				c.contractKey, c.waited.Round(time.Second), suggested)
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `instruction` (`id`,`team_uuid`,`target_session_uuid`,`target_agent_uuid`,`key`,`source`,`kind`,`body`,"+
					"`ref_kind`,`ref_uuid`,`requires_report`,`status`,`created_at`,`updated_at`) "+
					"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
				instructionUUID.String(), t.uuid.String(), c.sessionUUID.String(), c.agentUUID.String(),
				fmt.Sprintf("I-%d", seq),
				int64(enums.INSTRUCTION_SOURCE_SERVER), int64(enums.INSTRUCTION_KIND_CONFLICT_NOTICE),
				truncate(body, 600),
				int64(enums.SUBJECT_KIND_CONFLICT), conflictUUID.String(),
				false, int64(enums.INSTRUCTION_STATUS_PENDING),
				now, now); err != nil {
				return err
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	if !wrote {
		// Another pod won the race, or the conflict already existed under a
		// key this pass had not seen. Either way nothing was written here.
		return nil
	}

	rep.UnclaimedRaised++
	if notify {
		rep.InstructionsRaised++
	}
	rep.EventsEmitted++
	return nil
}

func nullableTimeIf(cond bool, t time.Time) any {
	if !cond {
		return nil
	}
	return t
}

func nullableSeverityIf(cond bool, s enums.ConflictSeverity) any {
	if !cond {
		return nil
	}
	return int64(s)
}
