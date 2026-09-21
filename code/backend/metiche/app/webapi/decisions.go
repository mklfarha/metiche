package webapi

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

const maxDecisions = 300

// maxDecisionRationale is how much of a decision's "why" the board is served.
// The tool accepts 2000 characters (app/mcp's MaxDecisionRationaleChars); the
// card shows the first 600 of them, so that is what crosses the wire —
// docs/DECISIONS.md §9.2.
const maxDecisionRationale = 600

// maxDecisionConflicts bounds the open-conflict lookup for one page of
// decisions. It is one query for the whole page, not one per decision, and
// this is the cap on what it may return.
const maxDecisionConflicts = 300

type decisionsResponse struct {
	cursors
	Team      teamRef        `json:"team"`
	Decisions []decisionWire `json:"decisions"`
}

// handleDecisions serves GET /v1/teams/{slug}/decisions.
//
// ACCEPTED decisions only (docs/DECISIONS.md §10.1). A superseded or revoked
// decision is never deleted — the row holds the wording forever, because
// there is no decision_revision table — so "everything on the team" would mean
// a Decisions tab that grows without bound and mixes rules that are in force
// with rules that were withdrawn. The past moves to /decisions/history.
//
// Always-show decisions come first. They are the ones flagged as "people keep
// violating this", and a page that buried them under whatever was recorded
// most recently would defeat the flag.
func (a *API) handleDecisions(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	limit := intQuery(r, "limit", maxDecisions, 1, maxDecisions)

	var out decisionsResponse
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		out.Decisions, err = loadDecisions(r.Context(), tx, team.UUID, limit)
		return err
	})
	if err != nil {
		a.fail(w, r, err, "read the decisions")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// decisionColumns is the one column list every decision read selects, in the
// order queryDecisions scans it. Nothing here is a wildcard, as everywhere
// else in this package.
//
// The two self-joins turn the supersede chain's uuids into the keys the rest
// of metiche speaks: the board links #auth-bearer-header to #auth-jwt-cookie,
// and a uuid never leaves this package (see wire.go).
const decisionColumns = "d.`id`, d.`key`, d.`title`, d.`statement`, d.`status`, d.`always_show`, " +
	"d.`revision`, d.`decided_at`, d.`updated_at`, d.`rationale`, " +
	"m.`display_name`, p.`key`, sup.`key`, sb.`key`"

const decisionJoins = "FROM `decision` d " +
	"LEFT JOIN `member` m ON m.`id` = d.`decided_by_member_uuid` " +
	"LEFT JOIN `project` p ON p.`id` = d.`project_uuid` " +
	"LEFT JOIN `decision` sup ON sup.`id` = d.`supersedes_uuid` " +
	"LEFT JOIN `decision` sb ON sb.`id` = d.`superseded_by_uuid` "

const decisionPathsQuery = "SELECT dp.`decision_uuid`, dp.`pattern_norm` FROM `decision_path` dp " +
	"WHERE dp.`team_uuid` = ? AND dp.`decision_uuid` IN ("

// loadDecisions reads the team's accepted decisions with everything the card
// draws: scope, the judged counts on the current revision, and the keys of
// the conflicts still open on each.
func loadDecisions(ctx context.Context, tx *sql.Tx, teamUUID string, limit int) ([]decisionWire, error) {
	q := "SELECT " + decisionColumns + " " + decisionJoins +
		"WHERE d.`team_uuid` = ? AND d.`status` = ? " +
		"ORDER BY d.`always_show` DESC, d.`updated_at` DESC, d.`key` LIMIT ?"
	args := []any{teamUUID, int64(enums.DECISION_STATUS_ACCEPTED), limit}

	out, ids, _, _, err := queryDecisions(ctx, tx, q, args, limit)
	if err != nil {
		return nil, err
	}
	return out, attachDecisionDetail(ctx, tx, teamUUID, ids, out)
}

// queryDecisions runs a query selecting decisionColumns and closes its rows
// before returning, so the three attach queries can use the same transaction.
// It stops at limit rows, reports whether there was another, and hands back
// the updated_at of the last row it served — the history's cursor.
func queryDecisions(ctx context.Context, tx *sql.Tx, q string, args []any, limit int) ([]decisionWire, []string, time.Time, bool, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, time.Time{}, false, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]decisionWire, 0, 16)
	ids := make([]string, 0, 16)
	var lastAt time.Time
	for rows.Next() {
		if len(out) == limit {
			return out, ids, lastAt, true, nil
		}
		var (
			id                       string
			d                        decisionWire
			status, revision         sql.NullInt64
			decidedAt, updatedAt     sql.NullTime
			rationale, decidedBy     sql.NullString
			projectKey               sql.NullString
			supersedes, supersededBy sql.NullString
		)
		if err := rows.Scan(&id, &d.Key, &d.Title, &d.Statement, &status, &d.AlwaysShow,
			&revision, &decidedAt, &updatedAt, &rationale,
			&decidedBy, &projectKey, &supersedes, &supersededBy); err != nil {
			return nil, nil, time.Time{}, false, err
		}
		st := enums.DecisionStatus(status.Int64)
		d.Status = st.String()
		d.Revision = revision.Int64
		d.DecidedAt, d.UpdatedAt = rfc3339(decidedAt), rfc3339(updatedAt)
		if isPastDecisionStatus(st) {
			// A terminal decision is never written again, so the row's
			// updated_at IS when it ended (docs/DECISIONS.md §9.3).
			d.EndedAt = rfc3339(updatedAt)
		}
		d.DecidedBy, d.ProjectKey = decidedBy.String, projectKey.String
		// The rationale is the one field on a decision that an agent wrote
		// for people rather than for another agent's model. Masked, then
		// clipped: the board is public on a public team.
		d.Rationale = boardText(rationale.String, maxDecisionRationale)
		d.Supersedes, d.SupersededBy = supersedes.String, supersededBy.String
		// Both are always present in the response, empty rather than null:
		// the board renders a list and a count object, never a nil check.
		d.Scope = []string{}
		d.OpenConflicts = []string{}
		out = append(out, d)
		ids = append(ids, id)
		lastAt = updatedAt.Time
	}
	return out, ids, lastAt, false, rows.Err()
}

// attachDecisionDetail fills the scope, the judged counts and the open
// conflicts of a page: three queries over the whole page, never three per row.
func attachDecisionDetail(ctx context.Context, tx *sql.Tx, teamUUID string, ids []string, out []decisionWire) error {
	if len(ids) == 0 {
		return nil
	}
	byID := make(map[string]*decisionWire, len(ids))
	byKey := make(map[string]*decisionWire, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, teamUUID)
	for i, id := range ids {
		byID[id] = &out[i]
		byKey[out[i].Key] = &out[i]
		args = append(args, id)
	}
	in := placeholders(len(ids))

	// The scope patterns are what make a decision findable by path, and they
	// are the same normalized form as claim_path — so showing them is showing
	// exactly what the detector matches on, not a prettier paraphrase of it.
	if err := func() error {
		rows, err := tx.QueryContext(ctx, decisionPathsQuery+in+") ORDER BY dp.`decision_uuid`, dp.`pattern_norm`", args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var decisionUUID, pattern string
			if err := rows.Scan(&decisionUUID, &pattern); err != nil {
				return err
			}
			if d, ok := byID[decisionUUID]; ok {
				d.Scope = append(d.Scope, pattern)
			}
		}
		return rows.Err()
	}(); err != nil {
		return err
	}

	if err := attachJudgedCounts(ctx, tx, args, in, byID); err != nil {
		return err
	}
	return attachOpenDecisionConflicts(ctx, tx, teamUUID, byKey)
}

// attachJudgedCounts counts, per decision at its CURRENT revision, one entry
// per plan: the state of that plan's LATEST judgement row, by verdict when it
// is judged and as one number when it is pending, assigned or not
// (docs/DECISIONS.md §5.1).
//
// A plan's latest row is, among its rows on the decision's current revision
// that have not expired, the one with the highest plan revision
// (subject_b_revision). Revisions only grow, so that is the row on the plan's
// current revision when one exists, and otherwise the newest row it has. A
// plan judged conflict, then revised and judged no_conflict, counts once, as
// no_conflict: the earlier verdict was about wording the plan no longer has.
// The plan revision is part of the pair key, so (decision revision, plan,
// plan revision) names one row; the id only makes the order total.
//
// Expired rows are neither counted nor chosen as latest: a pair whose window
// lapsed was never answered, and reporting it as waiting would show a board
// that is waiting for nobody. A plan that has ended still counts by its last
// verdict: the card reads "checked against N plans", and it was checked.
// Ending a plan expires its pending rows, so an ended plan is never waiting.
//
// The join to `decision` is what "current revision" means — subject_a is the
// decision, subject_b the plan — and idx_judgement_subject
// (team_uuid, subject_a_uuid, subject_b_uuid) serves the lookup, handing the
// rows over already grouped by (decision, plan), the window's partition.
func attachJudgedCounts(ctx context.Context, tx *sql.Tx, args []any, in string, byID map[string]*decisionWire) error {
	q := "SELECT l.`subject_a_uuid`, l.`status`, l.`verdict`, COUNT(*) FROM (" +
		"SELECT j.`subject_a_uuid`, j.`status`, j.`verdict`, ROW_NUMBER() OVER (" +
		"PARTITION BY j.`subject_a_uuid`, j.`subject_b_uuid` " +
		"ORDER BY j.`subject_b_revision` DESC, j.`id` DESC) AS `rn` " +
		"FROM `judgement` j " +
		"JOIN `decision` d ON d.`id` = j.`subject_a_uuid` AND d.`revision` = j.`subject_a_revision` " +
		"WHERE j.`team_uuid` = ? AND j.`subject_a_uuid` IN (" + in + ") AND j.`status` <> ?" +
		") l WHERE l.`rn` = 1 " +
		"GROUP BY l.`subject_a_uuid`, l.`status`, l.`verdict`"
	// A copy, so the caller's slice is never appended into.
	qargs := append(append(make([]any, 0, len(args)+1), args...), int64(enums.JUDGEMENT_STATUS_EXPIRED))
	rows, err := tx.QueryContext(ctx, q, qargs...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			decisionUUID    string
			status, verdict sql.NullInt64
			n               int64
		)
		if err := rows.Scan(&decisionUUID, &status, &verdict, &n); err != nil {
			return err
		}
		d, ok := byID[decisionUUID]
		if !ok {
			continue
		}
		switch enums.JudgementStatus(status.Int64) {
		case enums.JUDGEMENT_STATUS_PENDING:
			d.Judged.Pending += n
		case enums.JUDGEMENT_STATUS_JUDGED:
			switch enums.JudgementVerdict(verdict.Int64) {
			case enums.JUDGEMENT_VERDICT_NO_CONFLICT:
				d.Judged.NoConflict += n
			case enums.JUDGEMENT_VERDICT_CONFLICT:
				d.Judged.Conflict += n
			case enums.JUDGEMENT_VERDICT_UNSURE:
				d.Judged.Unsure += n
			}
		}
	}
	return rows.Err()
}

// attachOpenDecisionConflicts lists, per decision, the keys of the
// decision_contradiction conflicts still open or acknowledged on it.
//
// It matches on the conflict's EVIDENCE rather than on a participant row.
// A decision conflict has a participant for the decider only when that member
// has a session to name, so participants are not authoritative here; the
// evidence's overlap_path is the decision's key and is always written
// (docs/DECISIONS.md §5.1). idx_conflict_open's (team_uuid, status) prefix
// bounds the scan to the team's open conflicts.
func attachOpenDecisionConflicts(ctx context.Context, tx *sql.Tx, teamUUID string, byKey map[string]*decisionWire) error {
	if len(byKey) == 0 {
		return nil
	}
	keys := make([]any, 0, len(byKey)+4)
	keys = append(keys, teamUUID, int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED))
	for k := range byKey {
		keys = append(keys, k)
	}
	q := "SELECT JSON_VALUE(c.`evidence`, '$.overlap_path'), c.`key` FROM `conflict` c " +
		"WHERE c.`team_uuid` = ? AND c.`kind` = ? AND c.`status` IN (?, ?) " +
		"AND JSON_VALUE(c.`evidence`, '$.overlap_path') IN (" + placeholders(len(byKey)) + ") " +
		"ORDER BY c.`key` LIMIT ?"
	keys = append(keys, maxDecisionConflicts)

	rows, err := tx.QueryContext(ctx, q, keys...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]bool{}
	for rows.Next() {
		var decisionKey, conflictKey sql.NullString
		if err := rows.Scan(&decisionKey, &conflictKey); err != nil {
			return err
		}
		d, ok := byKey[decisionKey.String]
		if !ok || conflictKey.String == "" || seen[decisionKey.String+"\x00"+conflictKey.String] {
			continue
		}
		seen[decisionKey.String+"\x00"+conflictKey.String] = true
		d.OpenConflicts = append(d.OpenConflicts, conflictKey.String)
	}
	return rows.Err()
}
