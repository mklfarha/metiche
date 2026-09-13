package webapi

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

type conflictsResponse struct {
	cursors
	Team      teamRef        `json:"team"`
	Status    string         `json:"status"`
	Conflicts []conflictWire `json:"conflicts"`
}

// handleConflicts serves GET /v1/teams/{slug}/conflicts?status=open|all&limit=N.
//
// status defaults to "open" — the set a person still has to act on — because
// that is what the page is for. "all" includes the resolved and dismissed
// ones, which is what you want when you are asking whether a rule is crying
// wolf.
func (a *API) handleConflicts(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	wanted := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	limit := intQuery(r, "limit", 100, 1, maxConflicts)

	statuses := openConflictStatuses
	switch wanted {
	case "", "open":
		wanted = "open"
	case "all":
		statuses = nil
	default:
		if s := enums.ConflictStatusFromString(wanted); s != enums.CONFLICT_STATUS_INVALID {
			statuses = []any{s}
		} else {
			writeProblem(w, http.StatusBadRequest, "bad request",
				"status must be 'open', 'all', or a conflict status name")
			return
		}
	}

	var out conflictsResponse
	out.Status = wanted
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		out.Conflicts, err = loadConflicts(r.Context(), tx, team.UUID, statuses, limit)
		return err
	})
	if err != nil {
		a.fail(w, r, err, "read the conflicts")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// loadConflicts reads conflicts and their participants.
//
// Two queries, not one plus N: participants is a child table precisely because
// three agents in one directory is ordinary, so a per-conflict round trip
// would be the common case rather than the rare one. The second query filters
// by the same status set through a join, which keeps it a single indexed pass
// and avoids building an IN list of conflict uuids.
//
// nil statuses means every status.
func loadConflicts(ctx context.Context, tx *sql.Tx, teamUUID string, statuses []any, limit int) ([]conflictWire, error) {
	if len(statuses) == 0 {
		return loadConflictsWhere(ctx, tx, teamUUID, "", nil, limit)
	}
	return loadConflictsWhere(ctx, tx, teamUUID,
		" AND c.`status` IN ("+placeholders(len(statuses))+")", statuses, limit)
}

// loadSessionConflicts reads every conflict one session took part in, in any
// status, for that session's run history.
func loadSessionConflicts(ctx context.Context, tx *sql.Tx, teamUUID, sessionUUID string, limit int) ([]conflictWire, error) {
	return loadConflictsWhere(ctx, tx, teamUUID,
		" AND c.`id` IN (SELECT sp.`conflict_uuid` FROM `conflict_participant` sp "+
			"WHERE sp.`team_uuid` = ? AND sp.`session_uuid` = ?)",
		[]any{teamUUID, sessionUUID}, limit)
}

// loadConflictsWhere is the shared read. where is appended verbatim, so it is
// only ever one of the literal fragments above; every value in it is a bound
// parameter from whereArgs.
func loadConflictsWhere(ctx context.Context, tx *sql.Tx, teamUUID, where string, whereArgs []any, limit int) ([]conflictWire, error) {
	q := "SELECT c.`id`, c.`key`, c.`kind`, c.`severity`, c.`status`, c.`detected_by`, " +
		"c.`detector_rule`, c.`suggested_action`, c.`occurrence_count`, " +
		"c.`first_detected_at`, c.`last_detected_at`, " +
		"c.`resolution`, c.`resolution_note`, c.`dismiss_reason`, c.`resolved_at` " +
		"FROM `conflict` c WHERE c.`team_uuid` = ?" + where
	args := append([]any{teamUUID}, whereArgs...)
	// Severity first: a board that sorted by time would bury the critical one
	// under a stream of low-severity noise, which is the failure mode the
	// whole noise-control section of the plan exists to avoid.
	q += " ORDER BY c.`severity` DESC, c.`last_detected_at` DESC, c.`key` LIMIT ?"
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]conflictWire, 0, 16)
	index := map[string]*conflictWire{}
	ids := make([]string, 0, 16)
	for rows.Next() {
		var (
			id                                 string
			c                                  conflictWire
			kind, severity, status, detectedBy sql.NullInt64
			rule, action, resNote              sql.NullString
			first, last, resolvedAt            sql.NullTime
			occurrences                        sql.NullInt64
			resolution, dismissReason          sql.NullInt64
		)
		if err := rows.Scan(&id, &c.Key, &kind, &severity, &status, &detectedBy,
			&rule, &action, &occurrences, &first, &last,
			&resolution, &resNote, &dismissReason, &resolvedAt); err != nil {
			return nil, err
		}
		c.Kind = enums.ConflictKind(kind.Int64).String()
		c.Severity = enums.ConflictSeverity(severity.Int64).String()
		c.Status = enums.ConflictStatus(status.Int64).String()
		if detectedBy.Valid && enums.DetectedBy(detectedBy.Int64) != enums.DETECTED_BY_INVALID {
			c.DetectedBy = enums.DetectedBy(detectedBy.Int64).String()
		}
		c.DetectorRule, c.SuggestedAction, c.ResolutionNote = rule.String, action.String, resNote.String
		c.OccurrenceCount = occurrences.Int64
		c.FirstDetectedAt, c.LastDetectedAt, c.ResolvedAt = rfc3339(first), rfc3339(last), rfc3339(resolvedAt)
		if resolution.Valid && enums.ConflictResolution(resolution.Int64) != enums.CONFLICT_RESOLUTION_INVALID {
			c.Resolution = enums.ConflictResolution(resolution.Int64).String()
		}
		if dismissReason.Valid && enums.DismissReason(dismissReason.Int64) != enums.DISMISS_REASON_INVALID {
			c.DismissReason = enums.DismissReason(dismissReason.Int64).String()
		}
		c.Participants = []participantWire{}
		out = append(out, c)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	for i := range out {
		index[ids[i]] = &out[i]
	}

	pq := "SELECT cp.`conflict_uuid`, cp.`role`, cp.`subject_kind`, " +
		"s.`key`, m.`display_name`, a.`label` " +
		"FROM `conflict_participant` cp " +
		"JOIN `conflict` c ON c.`id` = cp.`conflict_uuid` " +
		"LEFT JOIN `session` s ON s.`id` = cp.`session_uuid` " +
		"LEFT JOIN `member` m ON m.`id` = cp.`member_uuid` " +
		"LEFT JOIN `agent` a ON a.`id` = cp.`agent_uuid` " +
		"WHERE cp.`team_uuid` = ? AND cp.`conflict_uuid` IN (" + placeholders(len(ids)) + ")"
	pargs := make([]any, 0, len(ids)+1)
	pargs = append(pargs, teamUUID)
	for _, id := range ids {
		pargs = append(pargs, id)
	}

	prows, err := tx.QueryContext(ctx, pq, pargs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = prows.Close() }()

	for prows.Next() {
		var (
			conflictUUID            string
			role, subjectKind       sql.NullInt64
			sessionKey, name, label sql.NullString
		)
		if err := prows.Scan(&conflictUUID, &role, &subjectKind, &sessionKey, &name, &label); err != nil {
			return nil, err
		}
		c, ok := index[conflictUUID]
		if !ok {
			continue
		}
		p := participantWire{
			SessionKey: sessionKey.String,
			MemberName: name.String,
			AgentLabel: label.String,
		}
		if role.Valid && enums.ParticipantRole(role.Int64) != enums.PARTICIPANT_ROLE_INVALID {
			p.Role = enums.ParticipantRole(role.Int64).String()
		}
		if subjectKind.Valid && enums.SubjectKind(subjectKind.Int64) != enums.SUBJECT_KIND_INVALID {
			p.SubjectKind = enums.SubjectKind(subjectKind.Int64).String()
		}
		c.Participants = append(c.Participants, p)
	}
	return out, prows.Err()
}
