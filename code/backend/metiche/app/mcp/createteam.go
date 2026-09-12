package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// createTeamNamespace makes the team's uuid a deterministic function of the
// request, which is what turns a retry into a no-op. See CreateTeam.
var createTeamNamespace = uuid.Must(uuid.FromString("6d657469-6368-4520-9465-616d73000001"))

// minIdempotencyKeyLength is enforced rather than suggested. The team uuid is
// derived from the key, so a one-character key from two different people
// would be two people deriving the same uuid — and the second would be told
// their team already exists.
const minIdempotencyKeyLength = 8

// ─────────────────────────────────────────────
// Tool: create_team
// ─────────────────────────────────────────────

type CreateTeamParams struct {
	TeamName       string `json:"team_name" jsonschema:"What to call the team, as a person would say it - 'Hack Night', 'Payments squad'."`
	MemberName     string `json:"member_name" jsonschema:"Your name, as your teammates would write it. You are joined to the team by this same call."`
	AgentLabel     string `json:"agent_label" jsonschema:"A short label for THIS agent instance - 'backend', 'ui', 'tests'."`
	ClientKey      string `json:"client_key" jsonschema:"A stable identifier for this agent process that survives a restart - your session id, or a hash of the working directory plus the label."`
	ClientKind     string `json:"client_kind,omitempty" jsonschema:"What kind of client you are, e.g. 'claude-code', 'cursor', 'codex'."`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"A key of your own, at least 8 characters - a uuid is ideal. Required: retrying this call with the same key returns the same team instead of creating a second one."`
}

type CreateTeamResult struct {
	JoinTeamResult
	TeamName     string `json:"team_name"`
	JoinCode     string `json:"join_code"`
	JoinCodeNote string `json:"join_code_note"`
	Created      bool   `json:"created"`
}

// CreateTeam mints a team and joins its creator in one call.
//
// UNAUTHENTICATED, like join_team, and for the same reason: metiche has no
// per-person account and no OAuth, because a coordination tool that takes ten
// minutes to join does not get joined. The join code IS the credential from
// here on. What bounds creation is therefore a per-IP rate limit and plan
// limits, not a login — see ratelimit.go.
//
// RETRY SAFETY, without a team row to lock. Every other mutating call takes
// the team's row lock and replays a stored response on a duplicate
// idempotency key. Neither is available here: the team is the thing being
// created. So the team's PRIMARY KEY is derived — uuid v5 over the team name,
// the member, the client key and the idempotency key — and the insert relies
// on that key to collide. A retry lands on the existing team instead of a
// second one, and because all four inputs feed the uuid, a collision means
// the same request rather than somebody else's team.
//
// The token is re-minted on a retry, exactly as in join_team: only its hash
// was kept, so the original cannot be handed back. The join code IS returned
// again, because it is a shared secret stored in the clear for precisely that
// reason.
func (h *Handler) CreateTeam(ctx context.Context, _ *mcp.CallToolRequest, args CreateTeamParams) (*mcp.CallToolResult, any, error) {
	if ok, retryIn := h.createLimit.Allow(ClientIPFromContext(ctx)); !ok {
		return nil, nil, fmt.Errorf(
			"too many teams created from this address; try again in %s. "+
				"If you are joining an existing team, use join_team with its join code instead", retryIn)
	}

	teamName := truncate(args.TeamName, 120)
	if teamName == "" {
		return nil, nil, errors.New("team_name is required")
	}
	if slugKey(teamName, 64) == "" {
		return nil, nil, errors.New("team_name must contain at least one letter or digit")
	}
	if strings.TrimSpace(args.MemberName) == "" {
		return nil, nil, errors.New("member_name is required — create_team joins you to the team it creates")
	}
	clientKey := truncate(args.ClientKey, 120)
	if clientKey == "" {
		return nil, nil, errors.New("client_key is required — it is what lets a restarted agent re-join as itself rather than as a duplicate")
	}
	idem := strings.TrimSpace(args.IdempotencyKey)
	if len(idem) < minIdempotencyKeyLength {
		return nil, nil, fmt.Errorf(
			"idempotency_key is required and must be at least %d characters (a uuid is ideal) — without it a retry would create a second team",
			minIdempotencyKeyLength)
	}

	teamID := uuid.NewV5(createTeamNamespace,
		"create_team:"+teamName+"|"+strings.TrimSpace(args.MemberName)+"|"+clientKey+"|"+idem)

	team, created, err := h.ensureTeam(ctx, teamID, teamName)
	if err != nil {
		return nil, nil, err
	}

	joined, err := h.joinAs(ctx, team, args.MemberName, args.AgentLabel, clientKey, args.ClientKind)
	if err != nil {
		return nil, nil, err
	}

	note := "share the join_code with your teammates; they call join_team with it. Then call start_session."
	if !created {
		note = "this team already existed for that idempotency_key, so it was not created twice. " + note
	}
	joined.Envelope.Note = note

	return jsonValue(CreateTeamResult{
		JoinTeamResult: joined,
		TeamName:       team.Name,
		JoinCode:       team.JoinCode,
		Created:        created,
		JoinCodeNote: "This is the team's shared secret. Anyone with it can join and see the board, so pass it the way you would " +
			"a door code, and rotate it from the board if it leaks.",
	})
}

// ensureTeam inserts the team at its derived uuid, or returns the one already
// there.
//
// The duplicate-key path is the normal retry path, not an error path: two
// concurrent identical create_team calls both try to insert, one wins, and
// the loser re-reads rather than failing. Reported as created=false so the
// caller can tell "I made this" from "this was already here".
func (h *Handler) ensureTeam(ctx context.Context, teamID uuid.UUID, teamName string) (team_entity.Team, bool, error) {
	if existing, err := h.teamByID(ctx, teamID); err == nil {
		return existing, false, nil
	}

	joinCode, err := MintJoinCode()
	if err != nil {
		return team_entity.Team{}, false, err
	}

	// The slug is what a person types after /t/ in the board's URL, so it is
	// the readable name first and unique second. The suffix is derived from
	// the team's uuid rather than random, so a retry that races past the
	// lookup above still computes the same slug and still collides instead of
	// creating a near-duplicate.
	base := slugKey(teamName, 40)
	slug := base
	if taken, err := h.slugTaken(ctx, slug); err != nil {
		return team_entity.Team{}, false, err
	} else if taken {
		slug = base + "-" + teamID.String()[:6]
	}

	// SEAM — plans.
	//
	// A later schema version adds a `plan` table and team.plan_uuid, and a new
	// team is supposed to get whichever plan carries is_instance_default,
	// with plan_source = instance_default. Neither the table nor the columns
	// exist in this generated tree yet, so there is nothing to set here.
	// When they arrive: resolve the default plan inside the same insert path
	// below, and fail the creation rather than defaulting silently if no
	// instance default is configured — a team with no plan is a team with no
	// limits.

	team := team_entity.Team{
		ID:       teamID,
		Name:     teamName,
		Slug:     slug,
		JoinCode: joinCode,
		Status:   enums.RECORD_STATUS_ACTIVE,
		// Both cursors start at zero: the first event this team ever produces
		// is member_joined, written by joinAs through the normal write path.
		Sequence:      0,
		BoardRevision: 0,
	}
	if _, err := h.core.Team().Insert(ctx,
		team_types.UpsertRequest{Team: team}, teammod.WithSkipCache()); err != nil {
		// Lost the race on our own uuid, or retried past the lookup above.
		// Either way the row that exists is the answer.
		if existing, lookupErr := h.teamByID(ctx, teamID); lookupErr == nil {
			return existing, false, nil
		}
		// Or lost the race on the SLUG, to a different team created at the
		// same instant with the same name. uq_team_slug is doing its job; the
		// fix is the disambiguated slug, which is derived from this team's
		// uuid and so cannot collide again.
		if team.Slug == base {
			team.Slug = base + "-" + teamID.String()[:6]
			if _, retryErr := h.core.Team().Insert(ctx,
				team_types.UpsertRequest{Team: team}, teammod.WithSkipCache()); retryErr == nil {
				return team, true, nil
			}
		}
		return team_entity.Team{}, false, retryable(err, "creating the team")
	}
	return team, true, nil
}

func (h *Handler) slugTaken(ctx context.Context, slug string) (bool, error) {
	res, err := h.core.Team().FetchTeamBySlug(ctx,
		team_types.FetchTeamBySlugRequest{Slug: slug, Limit: 1}, teammod.WithSkipCache())
	if err != nil {
		return false, retryable(err, "checking the team slug")
	}
	return len(res.Results) > 0, nil
}
