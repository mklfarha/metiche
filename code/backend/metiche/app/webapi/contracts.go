package webapi

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/enums"
)

const maxContracts = 300

type contractsResponse struct {
	cursors
	Team      teamRef        `json:"team"`
	Contracts []contractWire `json:"contracts"`
}

// handleContracts serves GET /v1/teams/{slug}/contracts.
//
// This is the produces/consumes matrix — the view that shows the bottleneck.
// The interesting cell is not the mismatch, it is the CONSUMED-BUT-UNPRODUCED
// one: somebody is writing code against an endpoint nobody is building, and
// nothing else in the system surfaces that. It is computed here, from the
// assertions themselves, so the answer is the same everywhere it is shown.
func (a *API) handleContracts(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	limit := intQuery(r, "limit", maxContracts, 1, maxContracts)

	var out contractsResponse
	err := a.read(r.Context(), func(tx *sql.Tx) error {
		team, err := resolveTeam(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		out.Team = team
		out.Sequence = team.Sequence
		out.BoardRevision = team.BoardRevision

		out.Contracts, err = loadContracts(r.Context(), tx, team.UUID, limit)
		return err
	})
	if err != nil {
		a.fail(w, r, err, "read the contracts")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

const contractsQuery = "SELECT ct.`id`, ct.`key`, ct.`kind`, ct.`status`, ct.`title`, p.`key` " +
	"FROM `contract` ct " +
	"JOIN `project` p ON p.`id` = ct.`project_uuid` " +
	"WHERE ct.`team_uuid` = ? ORDER BY ct.`key` LIMIT ?"

// assertionsQuery counts each assertion's fields in the same pass.
//
// field_count is what tells a reader whether an assertion is a real shape or a
// placeholder, and fetching it separately would be a third query for one
// integer. Grouping by the assertion's own primary key keeps it a plain
// aggregate over the index that already backs the join.
const assertionsQuery = "SELECT ca.`contract_uuid`, ca.`role`, ca.`shape_hash`, ca.`revision`, ca.`asserted_at`, " +
	"s.`key`, m.`display_name`, a.`label`, COUNT(f.`id`) " +
	"FROM `contract_assertion` ca " +
	"JOIN `contract` ct ON ct.`id` = ca.`contract_uuid` " +
	"LEFT JOIN `session` s ON s.`id` = ca.`session_uuid` " +
	"LEFT JOIN `member` m ON m.`id` = ca.`member_uuid` " +
	"LEFT JOIN `agent` a ON a.`id` = s.`agent_uuid` " +
	"LEFT JOIN `contract_field` f ON f.`assertion_uuid` = ca.`id` " +
	"WHERE ca.`team_uuid` = ? AND ca.`status` = ? " +
	"GROUP BY ca.`id`, ca.`contract_uuid`, ca.`role`, ca.`shape_hash`, ca.`revision`, ca.`asserted_at`, " +
	"s.`key`, m.`display_name`, a.`label` " +
	"ORDER BY ca.`asserted_at`"

func loadContracts(ctx context.Context, tx *sql.Tx, teamUUID string, limit int) ([]contractWire, error) {
	rows, err := tx.QueryContext(ctx, contractsQuery, teamUUID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]contractWire, 0, 16)
	ids := make([]string, 0, 16)
	for rows.Next() {
		var (
			id           string
			c            contractWire
			kind, status sql.NullInt64
			title        sql.NullString
			projectKey   sql.NullString
		)
		if err := rows.Scan(&id, &c.Key, &kind, &status, &title, &projectKey); err != nil {
			return nil, err
		}
		c.Kind = enums.ContractKind(kind.Int64).String()
		c.Status = enums.ContractStatus(status.Int64).String()
		c.Title, c.ProjectKey = title.String, projectKey.String
		c.Produces = []assertionWire{}
		c.Consumes = []assertionWire{}
		out = append(out, c)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	index := make(map[string]*contractWire, len(out))
	for i := range out {
		index[ids[i]] = &out[i]
	}

	arows, err := tx.QueryContext(ctx, assertionsQuery, teamUUID, enums.ASSERTION_STATUS_ACTIVE)
	if err != nil {
		return nil, err
	}
	defer func() { _ = arows.Close() }()

	for arows.Next() {
		var (
			contractUUID            string
			role                    sql.NullInt64
			shapeHash               sql.NullString
			revision                sql.NullInt64
			asserted                sql.NullTime
			sessionKey, name, label sql.NullString
			fieldCount              int64
		)
		if err := arows.Scan(&contractUUID, &role, &shapeHash, &revision, &asserted,
			&sessionKey, &name, &label, &fieldCount); err != nil {
			return nil, err
		}
		c, ok := index[contractUUID]
		if !ok {
			continue
		}
		aw := assertionWire{
			SessionKey: sessionKey.String,
			MemberName: name.String,
			AgentLabel: label.String,
			ShapeHash:  shapeHash.String,
			Revision:   revision.Int64,
			AssertedAt: rfc3339(asserted),
			FieldCount: fieldCount,
		}
		switch enums.AssertionRole(role.Int64) {
		case enums.ASSERTION_ROLE_PRODUCES:
			aw.Role = enums.AssertionRole(enums.ASSERTION_ROLE_PRODUCES).String()
			c.Produces = append(c.Produces, aw)
		case enums.ASSERTION_ROLE_CONSUMES:
			aw.Role = enums.AssertionRole(enums.ASSERTION_ROLE_CONSUMES).String()
			c.Consumes = append(c.Consumes, aw)
		}
	}
	if err := arows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		out[i].Agreement = agreementOf(&out[i])
	}
	return out, nil
}

// agreementOf reduces one contract's active assertions to a single word.
//
// The shape hash is the whole comparison: the server canonicalizes every
// submitted shape and hashes it, precisely so that two agents describing the
// same thing in different word order produce the same bytes. One distinct hash
// across every active assertion means they agree; more than one means they do
// not, and nothing further needs to be compared to know that.
func agreementOf(c *contractWire) string {
	switch {
	case len(c.Produces) == 0 && len(c.Consumes) == 0:
		return "empty"
	case len(c.Produces) == 0:
		return "unclaimed"
	case len(c.Consumes) == 0:
		return "unconsumed"
	}
	seen := map[string]struct{}{}
	for _, a := range c.Produces {
		seen[a.ShapeHash] = struct{}{}
	}
	for _, a := range c.Consumes {
		seen[a.ShapeHash] = struct{}{}
	}
	if len(seen) == 1 {
		return "agreed"
	}
	return "mismatch"
}
