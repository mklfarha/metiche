package webapi

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/enums"
)

const maxContracts = 300

// maxContractIssues bounds the issues returned per contract.
const maxContractIssues = 20

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

// assertionsQuery reads every assertion that still counts: active, on a
// session that is live or stale. An ended or abandoned session's assertions
// stop counting the way its claims do, and the matrix must not show a column
// for somebody who has gone home.
//
// field_count is a correlated count over contract_field's unique
// (assertion_uuid, path) index; the shape is the canonical field list the
// server stored, which is what the fields and issues below are built from.
const assertionsQuery = "SELECT ca.`contract_uuid`, ca.`role`, ca.`shape_hash`, ca.`revision`, ca.`asserted_at`, " +
	"s.`key`, m.`display_name`, a.`label`, " +
	"(SELECT COUNT(*) FROM `contract_field` f WHERE f.`assertion_uuid` = ca.`id`), ca.`shape` " +
	"FROM `contract_assertion` ca " +
	"JOIN `contract` ct ON ct.`id` = ca.`contract_uuid` " +
	"JOIN `session` s ON s.`id` = ca.`session_uuid` " +
	"LEFT JOIN `member` m ON m.`id` = ca.`member_uuid` " +
	"LEFT JOIN `agent` a ON a.`id` = s.`agent_uuid` " +
	"WHERE ca.`team_uuid` = ? AND ca.`status` = ? AND s.`status` IN (?, ?) " +
	"ORDER BY ca.`asserted_at`, ca.`id`"

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
	shapes := make(map[string][]assertionShape, len(out))
	for i := range out {
		index[ids[i]] = &out[i]
	}

	arows, err := tx.QueryContext(ctx, assertionsQuery, teamUUID, enums.ASSERTION_STATUS_ACTIVE,
		enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE)
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
			shape                   sql.NullString
		)
		if err := arows.Scan(&contractUUID, &role, &shapeHash, &revision, &asserted,
			&sessionKey, &name, &label, &fieldCount, &shape); err != nil {
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
		as := assertionShape{session: sessionKey.String, hash: shapeHash.String, role: enums.AssertionRole(role.Int64)}
		if shape.Valid {
			if parsed, err := coordination.ShapeFromCanonical([]byte(shape.String)); err == nil {
				as.shape, as.ok = parsed, true
				aw.Fields = fieldsWire(parsed)
			}
		}
		switch enums.AssertionRole(role.Int64) {
		case enums.ASSERTION_ROLE_PRODUCES:
			aw.Role = enums.AssertionRole(enums.ASSERTION_ROLE_PRODUCES).String()
			c.Produces = append(c.Produces, aw)
		case enums.ASSERTION_ROLE_CONSUMES:
			aw.Role = enums.AssertionRole(enums.ASSERTION_ROLE_CONSUMES).String()
			c.Consumes = append(c.Consumes, aw)
		default:
			continue
		}
		shapes[contractUUID] = append(shapes[contractUUID], as)
	}
	if err := arows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		out[i].Agreement, out[i].Issues = agreementOf(&out[i], shapes[ids[i]])
	}
	return out, nil
}

// assertionShape is what the comparison needs from one assertion.
type assertionShape struct {
	session string
	hash    string
	role    enums.AssertionRole
	shape   coordination.Shape
	ok      bool
}

func fieldsWire(s coordination.Shape) []contractFieldWire {
	out := make([]contractFieldWire, 0, len(s.Fields))
	for _, f := range s.Fields {
		out = append(out, contractFieldWire{
			Path: f.Path, Type: string(f.Type), Direction: string(f.Direction),
			Required: f.Required, Nullable: f.Nullable,
		})
	}
	return out
}

// agreementOf reduces one contract's live assertions to a single word, and
// returns the field-level issues behind it.
//
// Equal shape hashes are the fast path: the server canonicalizes every shape,
// so one distinct hash means the sides agree. Different hashes are NOT a
// mismatch by themselves — a producer may return more than any consumer reads,
// or mark a field optional that a consumer does not need — so every producer
// is compared with every consumer through coordination.CompareShapes, the same
// function publish_contract detects with. That is what keeps the board from
// calling "mismatch" what the server did not raise. A contract whose stored
// shapes cannot be read falls back to the hash comparison alone.
func agreementOf(c *contractWire, shapes []assertionShape) (string, []contractIssueWire) {
	switch {
	case len(c.Produces) == 0 && len(c.Consumes) == 0:
		return "empty", nil
	case len(c.Produces) == 0:
		return "unclaimed", nil
	case len(c.Consumes) == 0:
		return "unconsumed", nil
	}
	seen := map[string]struct{}{}
	readable := true
	for _, s := range shapes {
		seen[s.hash] = struct{}{}
		readable = readable && s.ok
	}
	if len(seen) == 1 {
		return "agreed", nil
	}
	if !readable {
		return "mismatch", nil
	}

	var issues []contractIssueWire
	breaking := false
	for _, p := range shapes {
		if p.role != enums.ASSERTION_ROLE_PRODUCES {
			continue
		}
		for _, q := range shapes {
			if q.role != enums.ASSERTION_ROLE_CONSUMES || q.session == p.session || q.hash == p.hash {
				continue
			}
			for _, is := range coordination.CompareShapes(p.shape, q.shape) {
				if is.Kind != coordination.IssueNamingVariant {
					breaking = true
				}
				if len(issues) < maxContractIssues {
					issues = append(issues, contractIssueWire{
						Kind: string(is.Kind), Path: is.Path, Expected: is.Expected, Actual: is.Actual,
						Direction: string(is.Direction), Severity: is.Severity.String(), Note: is.Note,
						Producer: p.session, Consumer: q.session,
					})
				}
			}
		}
	}
	if breaking {
		return "mismatch", issues
	}
	return "agreed", issues
}
