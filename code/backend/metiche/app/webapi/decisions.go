package webapi

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

const maxDecisions = 300

type decisionsResponse struct {
	cursors
	Team      teamRef        `json:"team"`
	Decisions []decisionWire `json:"decisions"`
}

// handleDecisions serves GET /v1/teams/{slug}/decisions.
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

const decisionsQuery = "SELECT d.`id`, d.`key`, d.`title`, d.`statement`, d.`status`, d.`always_show`, " +
	"d.`revision`, d.`decided_at`, m.`display_name` " +
	"FROM `decision` d " +
	"LEFT JOIN `member` m ON m.`id` = d.`decided_by_member_uuid` " +
	"WHERE d.`team_uuid` = ? " +
	"ORDER BY d.`always_show` DESC, d.`created_at` DESC, d.`key` LIMIT ?"

const decisionPathsQuery = "SELECT dp.`decision_uuid`, dp.`pattern_norm` " +
	"FROM `decision_path` dp WHERE dp.`team_uuid` = ? ORDER BY dp.`pattern_norm`"

func loadDecisions(ctx context.Context, tx *sql.Tx, teamUUID string, limit int) ([]decisionWire, error) {
	rows, err := tx.QueryContext(ctx, decisionsQuery, teamUUID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]decisionWire, 0, 16)
	ids := make([]string, 0, 16)
	for rows.Next() {
		var (
			id        string
			d         decisionWire
			status    sql.NullInt64
			revision  sql.NullInt64
			decidedAt sql.NullTime
			decidedBy sql.NullString
		)
		if err := rows.Scan(&id, &d.Key, &d.Title, &d.Statement, &status, &d.AlwaysShow,
			&revision, &decidedAt, &decidedBy); err != nil {
			return nil, err
		}
		d.Status = enums.DecisionStatus(status.Int64).String()
		d.Revision = revision.Int64
		d.DecidedAt = rfc3339(decidedAt)
		d.DecidedBy = decidedBy.String
		d.Scope = []string{}
		out = append(out, d)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	index := make(map[string]*decisionWire, len(out))
	for i := range out {
		index[ids[i]] = &out[i]
	}

	// The scope patterns are what make a decision findable by path, and they
	// are the same normalized form as claim_path — so showing them is showing
	// exactly what the detector matches on, not a prettier paraphrase of it.
	prows, err := tx.QueryContext(ctx, decisionPathsQuery, teamUUID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = prows.Close() }()

	for prows.Next() {
		var decisionUUID, pattern string
		if err := prows.Scan(&decisionUUID, &pattern); err != nil {
			return nil, err
		}
		if d, ok := index[decisionUUID]; ok {
			d.Scope = append(d.Scope, pattern)
		}
	}
	return out, prows.Err()
}
