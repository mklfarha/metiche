package webapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

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

// conflictColumns is the one column list every conflict read selects, in the
// order queryConflicts scans it.
//
// evidence is JSON the detector writes, and it is read only key by key, never
// whole: MySQL rewrites a JSON column's key order on the way in, and most of
// the document is not the board's to show. Its three path keys, the first
// field issue (the one line the judging plan's own model wrote,
// docs/DECISIONS.md §9.4 and docs/DUPLICATES.md §9.4), and the two sides'
// labels and summaries and the adjuster facts (docs/DUPLICATES.md §5.1) are
// read, each by name. queryConflicts serves the labels, summaries and facts
// on a duplicate_work only and drops them for every other kind, so a path
// overlap's or a decision conflict's intent summaries still never reach a
// response.
const conflictColumns = "c.`id`, c.`key`, c.`kind`, c.`severity`, c.`status`, c.`detected_by`, " +
	"c.`detector_rule`, c.`suggested_action`, c.`occurrence_count`, " +
	"c.`first_detected_at`, c.`last_detected_at`, " +
	"c.`resolution`, c.`resolution_note`, c.`dismiss_reason`, c.`resolved_at`, " +
	"JSON_VALUE(c.`evidence`, '$.overlap_path'), JSON_VALUE(c.`evidence`, '$.a_pattern'), " +
	"JSON_VALUE(c.`evidence`, '$.b_pattern'), c.`escalated_at`, " +
	"JSON_VALUE(c.`evidence`, '$.field_issues[0]'), " +
	"JSON_VALUE(c.`evidence`, '$.a_label'), JSON_VALUE(c.`evidence`, '$.a_summary'), " +
	"JSON_VALUE(c.`evidence`, '$.b_label'), JSON_VALUE(c.`evidence`, '$.b_summary'), " +
	"JSON_EXTRACT(c.`evidence`, '$.adjusters')"

// loadConflictsWhere is the shared read. where is appended verbatim, so it is
// only ever one of the literal fragments in this package; every value in it is
// a bound parameter from whereArgs.
func loadConflictsWhere(ctx context.Context, tx *sql.Tx, teamUUID, where string, whereArgs []any, limit int) ([]conflictWire, error) {
	q := "SELECT " + conflictColumns + " FROM `conflict` c WHERE c.`team_uuid` = ?" + where
	args := append([]any{teamUUID}, whereArgs...)
	// Severity first: a board that sorted by time would bury the critical one
	// under a stream of low-severity noise, which is the failure mode the
	// whole noise-control section of the plan exists to avoid.
	q += " ORDER BY c.`severity` DESC, c.`last_detected_at` DESC, c.`key` LIMIT ?"
	args = append(args, limit)

	out, ids, _, _, err := queryConflicts(ctx, tx, q, args, limit, false)
	if err != nil {
		return nil, err
	}
	return out, attachParticipants(ctx, tx, teamUUID, ids, out)
}

// queryConflicts runs a query selecting conflictColumns — plus, when ordered,
// one trailing sort-time column — and closes its rows before returning, so the
// participants query can use the same transaction. It stops at limit rows and
// reports whether there was another.
func queryConflicts(ctx context.Context, tx *sql.Tx, q string, args []any, limit int, ordered bool) ([]conflictWire, []string, time.Time, bool, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, time.Time{}, false, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]conflictWire, 0, 16)
	ids := make([]string, 0, 16)
	var lastAt time.Time
	for rows.Next() {
		if len(out) == limit {
			return out, ids, lastAt, true, nil
		}
		var (
			id                                 string
			c                                  conflictWire
			kind, severity, status, detectedBy sql.NullInt64
			rule, action, resNote              sql.NullString
			first, last, resolvedAt            sql.NullTime
			occurrences                        sql.NullInt64
			resolution, dismissReason          sql.NullInt64
			overlapPath, aPattern, bPattern    sql.NullString
			escalatedAt                        sql.NullTime
			judgeNote                          sql.NullString
			dup                                duplicateEvidence
			at                                 sql.NullTime
		)
		dest := []any{&id, &c.Key, &kind, &severity, &status, &detectedBy,
			&rule, &action, &occurrences, &first, &last,
			&resolution, &resNote, &dismissReason, &resolvedAt,
			&overlapPath, &aPattern, &bPattern, &escalatedAt, &judgeNote,
			&dup.aLabel, &dup.aSummary, &dup.bLabel, &dup.bSummary, &dup.adjusters}
		if ordered {
			dest = append(dest, &at)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, time.Time{}, false, err
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
		c.EscalatedAt = rfc3339(escalatedAt)
		switch enums.ConflictKind(kind.Int64) {
		case enums.CONFLICT_KIND_DUPLICATE_WORK:
			// A duplicate's overlap_path is the shared issue id when there
			// is one, else the path both plans also hold (docs/DUPLICATES.md
			// §9.4); either way it is a signal, not a claimed pattern, so
			// paths is the yield side's pattern then the incumbent's.
			dup.overlapPath, dup.aPattern, dup.bPattern = overlapPath, aPattern, bPattern
			c.Plans, c.Signals, c.IssueRef = dup.wire()
			c.JudgeNote = boardText(judgeNote.String, maxJudgeNoteChars)
			c.Paths = conflictPaths(bPattern, aPattern)
		case enums.CONFLICT_KIND_DECISION_CONTRADICTION:
			// A decision conflict's overlap_path is the decision's KEY, not a
			// path (docs/DECISIONS.md §9.4), so it goes to decision_key and
			// never into paths. What is left is the plan's path and the
			// decision's scope pattern, in that order: what the agent is
			// touching first, what governs it second.
			c.DecisionKey = strings.TrimSpace(overlapPath.String)
			c.JudgeNote = strings.TrimSpace(judgeNote.String)
			c.Paths = conflictPaths(bPattern, aPattern)
		default:
			c.Paths = conflictPaths(overlapPath, aPattern, bPattern)
			if strings.HasPrefix(c.Kind, "contract_") {
				c.ContractKey = strings.TrimSpace(overlapPath.String)
			}
		}
		c.Participants = []participantWire{}
		out = append(out, c)
		ids = append(ids, id)
		lastAt = at.Time
	}
	return out, ids, lastAt, false, rows.Err()
}

// Clip lengths for a duplicate's agent-written text. A summary is at most
// VARCHAR(280) where it was typed, and a label is truncated to 255 where the
// evidence is written, so neither clips in practice; the judge's note is an
// agent's free text and gets a decision rationale's room.
const (
	maxPlanSummaryChars = 280
	maxPlanWhoChars     = 255
	maxJudgeNoteChars   = 600
)

// duplicateEvidence is what a duplicate_work conflict's evidence holds, each
// key read by name (docs/DUPLICATES.md §9.4).
type duplicateEvidence struct {
	overlapPath, aPattern, bPattern sql.NullString
	aLabel, aSummary                sql.NullString
	bLabel, bSummary                sql.NullString
	adjusters                       sql.NullString // a JSON array of facts
}

// wire turns the evidence into §9.3's plans, signals and issue_ref.
//
// The facts are written in a fixed order (app/mcp/duplicateresolve.go): the
// plans fact "plans:<a>,<b>" first, where b is the yield side and the judge,
// then "same_issue:<ref>", "words:w1,w2", "paths:<path>" and
// "same_member_concurrent". The plans fact names the two plans and is never
// a signal; every other fact is one, in the order it was written. A fact this
// binary does not know is left out rather than shown raw.
//
// When the incumbent later judges the pair itself, the detector rewrites the
// whole evidence with the sides swapped, so a and b here are always the
// current incumbent and yield side.
func (d duplicateEvidence) wire() (plans []conflictPlanWire, signals []string, issueRef string) {
	var facts []string
	if d.adjusters.Valid && strings.TrimSpace(d.adjusters.String) != "" {
		// A malformed array reads as no facts: the plans still show, with
		// no keys, and the card still draws.
		_ = json.Unmarshal([]byte(d.adjusters.String), &facts)
	}
	var aKey, bKey string
	sameIssue := false
	for _, raw := range facts {
		f := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(f, "plans:"):
			if aKey != "" || bKey != "" {
				continue
			}
			if a, b, ok := strings.Cut(strings.TrimPrefix(f, "plans:"), ","); ok {
				aKey, bKey = strings.TrimSpace(a), strings.TrimSpace(b)
			}
		case strings.HasPrefix(f, "same_issue:"):
			ref := boardText(strings.TrimPrefix(f, "same_issue:"), maxPlanWhoChars)
			if ref == "" {
				continue
			}
			sameIssue = true
			if issueRef == "" {
				issueRef = ref
			}
			signals = append(signals, "same issue "+ref)
		case strings.HasPrefix(f, "words:"):
			var words []string
			for _, w := range strings.Split(strings.TrimPrefix(f, "words:"), ",") {
				if w = strings.TrimSpace(w); w != "" {
					words = append(words, w)
				}
			}
			if len(words) > 0 {
				signals = append(signals, "shared words: "+boardText(strings.Join(words, ", "), maxPlanSummaryChars))
			}
		case strings.HasPrefix(f, "paths:"):
			if p := strings.TrimSpace(strings.TrimPrefix(f, "paths:")); p != "" {
				signals = append(signals, "also share "+p)
			}
		case f == "same_member_concurrent":
			signals = append(signals, "one person's two agents")
		}
	}
	// overlap_path IS the ref when there is a shared issue (§9.4); it is the
	// overlapping path otherwise, and then never the issue_ref.
	if sameIssue && d.overlapPath.Valid {
		if ref := boardText(d.overlapPath.String, maxPlanWhoChars); ref != "" {
			issueRef = ref
		}
	}
	plans = []conflictPlanWire{
		{Key: aKey, Who: boardText(d.aLabel.String, maxPlanWhoChars), Summary: boardText(d.aSummary.String, maxPlanSummaryChars),
			Path: strings.TrimSpace(d.aPattern.String), Yields: false},
		{Key: bKey, Who: boardText(d.bLabel.String, maxPlanWhoChars), Summary: boardText(d.bSummary.String, maxPlanSummaryChars),
			Path: strings.TrimSpace(d.bPattern.String), Yields: true},
	}
	return plans, signals, issueRef
}

// conflictPaths is the overlapping path and the two claimed patterns, once
// each, in that order. A conflict that is not about paths has none.
func conflictPaths(vals ...sql.NullString) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range vals {
		p := strings.TrimSpace(v.String)
		if !v.Valid || p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// attachParticipants fills the participants of out (whose uuids are ids) in
// one query, not one per conflict.
func attachParticipants(ctx context.Context, tx *sql.Tx, teamUUID string, ids []string, out []conflictWire) error {
	if len(ids) == 0 {
		return nil
	}
	index := make(map[string]*conflictWire, len(ids))
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
		"WHERE cp.`team_uuid` = ? AND cp.`conflict_uuid` IN (" + placeholders(len(ids)) + ") " +
		"ORDER BY cp.`conflict_uuid`, cp.`role`, s.`key`"
	pargs := make([]any, 0, len(ids)+1)
	pargs = append(pargs, teamUUID)
	for _, id := range ids {
		pargs = append(pargs, id)
	}

	prows, err := tx.QueryContext(ctx, pq, pargs...)
	if err != nil {
		return err
	}
	defer func() { _ = prows.Close() }()

	for prows.Next() {
		var (
			conflictUUID            string
			role, subjectKind       sql.NullInt64
			sessionKey, name, label sql.NullString
		)
		if err := prows.Scan(&conflictUUID, &role, &subjectKind, &sessionKey, &name, &label); err != nil {
			return err
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
	return prows.Err()
}
