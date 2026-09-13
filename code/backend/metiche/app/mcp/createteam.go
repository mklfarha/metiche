package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	invitemod "github.com/mklfarha/metiche/backend/core/module/invite"
	invite_types "github.com/mklfarha/metiche/backend/core/module/invite/types"
	planmod "github.com/mklfarha/metiche/backend/core/module/plan"
	plan_types "github.com/mklfarha/metiche/backend/core/module/plan/types"
	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	invite_entity "github.com/mklfarha/metiche/backend/entity/invite"
	member_entity "github.com/mklfarha/metiche/backend/entity/member"
	plan_entity "github.com/mklfarha/metiche/backend/entity/plan"
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

// ErrNoDefaultPlan is a refusal, not a fallback.
//
// v3 put limits on a `plan` row and pointed team.plan_uuid at it. A team with
// no plan is a team with no ceiling on agents, members, projects or
// retention — on a hosted instance that is an unbounded tenant, and inventing
// a silent default here would mean the first anyone hears of it is the bill
// or the outage. So creation fails, loudly, and the instance operator seeds a
// plan.
var ErrNoDefaultPlan = errors.New(
	"this metiche instance has no default plan: no row in `plan` has is_instance_default set and status active. " +
		"Refusing to create a team with no plan, because a team with no plan has no limits. " +
		"Seed an instance-default plan first")

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
	Plan         string `json:"plan"`
	Visibility   string `json:"visibility"`
	Created      bool   `json:"created"`
}

// CreateTeam mints a team and joins its creator in one call.
//
// UNAUTHENTICATED, like join_team, and for the same reason: metiche has no
// signup, because a coordination tool that takes ten minutes to join does not
// get joined. A first contact mints an anonymous `account` and hands back the
// creating agent's one-time token; a request that already carries a token
// creates the team as the person it already is. The token decision (mint,
// keep or rotate) is joinAs's, identical to join_team's. What bounds creation
// is therefore a per-IP rate limit and the team's plan, not a login — see
// ratelimit.go.
//
// One consequence of the derived primary key below, left alone on purpose:
// client_key feeds the team's uuid, so each of a person's clients calling
// create_team with the same idempotency key would make its own team. The
// installer runs create_team ONCE, as the first client, and attaches the others
// with join_team by team_slug.
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
// WHAT v3 ADDED HERE. team.join_code is gone, so the creator's first `invite`
// row is minted as part of creation — otherwise a new team would be a team
// nobody else could ever reach. And the instance-default `plan` is resolved
// and attached, or creation fails: see ErrNoDefaultPlan.
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

	// joinAs resolved the membership; read it back for the invite's
	// created_by and for the owner promotion.
	member, found, err := h.memberByKey(ctx, nil, team.ID, joined.MemberKey)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, errors.New("the creator's membership vanished immediately after being created; retry create_team")
	}
	if created && member.Role != enums.MEMBER_ROLE_OWNER {
		if _, err := h.core.DB().ExecContext(ctx,
			"UPDATE `member` SET `role` = ?, `updated_at` = UTC_TIMESTAMP() WHERE `id` = ?",
			enums.MEMBER_ROLE_OWNER, member.ID.String()); err != nil {
			return nil, nil, retryable(err, "making you the team's owner")
		}
		member.Role = enums.MEMBER_ROLE_OWNER
	}

	invite, err := h.ensureFirstInvite(ctx, team, member)
	if err != nil {
		return nil, nil, err
	}

	planName := ""
	if team.PlanUUID != nil {
		if p, err := h.planByID(ctx, *team.PlanUUID); err == nil {
			planName = p.Name
		}
	}

	note := "share the join_code with your teammates; they call join_team with it. Then call start_session."
	if !created {
		note = "this team already existed for that idempotency_key, so it was not created twice. " + note
	}
	joined.Envelope.Note = note

	return jsonValue(CreateTeamResult{
		JoinTeamResult: joined,
		TeamName:       team.Name,
		JoinCode:       invite.Code,
		Created:        created,
		Plan:           planName,
		Visibility:     team.Visibility.String(),
		JoinCodeNote: "This is an invite code, and it is the team's shared secret. Anyone with it can join and see the board, so pass it the way " +
			"you would a door code. Unlike v1's join code it is a row of its own: it can be revoked, expired or use-capped from the board, " +
			"and a team can have several at once.",
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

	// PLANS. Resolved BEFORE anything is written, so the loud failure happens
	// before a half-made team exists rather than after.
	plan, err := h.instanceDefaultPlan(ctx)
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

	planID := plan.ID
	team := team_entity.Team{
		ID:         teamID,
		Name:       teamName,
		Slug:       slug,
		Status:     enums.RECORD_STATUS_ACTIVE,
		PlanUUID:   &planID,
		PlanSource: enums.PLAN_SOURCE_INSTANCE_DEFAULT,
		// Private, always. A board is other people's work in progress, and
		// nothing in this tool surface should be able to make one public by
		// accident — that is a deliberate act on the board, not a side effect
		// of creating a team from an agent.
		Visibility:              enums.TEAM_VISIBILITY_PRIVATE,
		RequiresClaimedAccounts: false,
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

// instanceDefaultPlan resolves the plan a brand new team gets.
//
// Lowest sort_order wins when an operator has marked more than one, rather
// than failing: two defaults is a configuration smell, but refusing to create
// any team because of it is worse than picking the one the operator listed
// first. NO default at all is the case that must fail — see ErrNoDefaultPlan.
func (h *Handler) instanceDefaultPlan(ctx context.Context) (plan_entity.Plan, error) {
	res, err := h.core.Plan().FetchPlanByIsInstanceDefaultAndStatus(ctx,
		plan_types.FetchPlanByIsInstanceDefaultAndStatusRequest{
			IsInstanceDefault: true,
			Status:            enums.RECORD_STATUS_ACTIVE,
			Limit:             10,
		}, planmod.WithSkipCache())
	if err != nil {
		return plan_entity.Plan{}, retryable(err, "resolving the instance default plan")
	}
	if len(res.Results) == 0 {
		return plan_entity.Plan{}, ErrNoDefaultPlan
	}
	best := res.Results[0]
	for _, p := range res.Results[1:] {
		if p.SortOrder < best.SortOrder {
			best = p
		}
	}
	return best, nil
}

func (h *Handler) planByID(ctx context.Context, id uuid.UUID) (plan_entity.Plan, error) {
	res, err := h.core.Plan().FetchPlanByID(ctx,
		plan_types.FetchPlanByIDRequest{ID: id}, planmod.WithSkipCache())
	if err != nil {
		return plan_entity.Plan{}, retryable(err, "reading the team's plan")
	}
	if len(res.Results) == 0 {
		return plan_entity.Plan{}, fmt.Errorf("plan %s no longer exists", id)
	}
	return res.Results[0], nil
}

// ensureFirstInvite returns the creator's usable invite, minting one if they
// have none.
//
// Idempotent on purpose: a retried create_team must hand back the SAME code,
// or the first answer's code and the retry's code would both be live and the
// creator would have quietly leaked a second door key. Uncapped and
// unexpiring, because the first invite is how a team gets its second member
// and a hackathon team should not have to think about it; narrower invites
// are made from the board.
func (h *Handler) ensureFirstInvite(ctx context.Context, team team_entity.Team, member member_entity.Member) (invite_entity.Invite, error) {
	res, err := h.core.Invite().FetchInviteByTeamUUIDAndStatus(ctx,
		invite_types.FetchInviteByTeamUUIDAndStatusRequest{
			TeamUUID: team.ID,
			Status:   enums.INVITE_STATUS_ACTIVE,
			Limit:    50,
		}, invitemod.WithSkipCache())
	if err != nil {
		return invite_entity.Invite{}, retryable(err, "looking up the team's invites")
	}
	for _, inv := range res.Results {
		if inv.RevokedAt.Valid || inv.ExpiresAt.Valid || inv.MaxUses.Valid {
			continue // a narrowed invite is somebody's deliberate choice, not the creator's default one
		}
		if inv.CreatedByMemberUUID != nil && *inv.CreatedByMemberUUID == member.ID {
			return inv, nil
		}
	}

	code, err := MintJoinCode()
	if err != nil {
		return invite_entity.Invite{}, err
	}
	id, err := uuid.NewV4()
	if err != nil {
		return invite_entity.Invite{}, err
	}
	createdBy := member.ID
	inv := invite_entity.Invite{
		ID:                  id,
		TeamUUID:            team.ID,
		Code:                code,
		Label:               nullString("first invite"),
		CreatedByMemberUUID: &createdBy,
		Uses:                0,
		Status:              enums.INVITE_STATUS_ACTIVE,
	}
	if _, err := h.core.Invite().Insert(ctx,
		invite_types.UpsertRequest{Invite: inv}, invitemod.WithSkipCache()); err != nil {
		// uq_invite_code is global, so the only way this collides is the
		// 1-in-1.1e15 draw or a concurrent retry that got there first. Both
		// are answered by re-reading.
		if again, lookupErr := h.core.Invite().FetchInviteByTeamUUIDAndStatus(ctx,
			invite_types.FetchInviteByTeamUUIDAndStatusRequest{
				TeamUUID: team.ID, Status: enums.INVITE_STATUS_ACTIVE, Limit: 50,
			}, invitemod.WithSkipCache()); lookupErr == nil {
			for _, existing := range again.Results {
				if existing.CreatedByMemberUUID != nil && *existing.CreatedByMemberUUID == member.ID {
					return existing, nil
				}
			}
		}
		return invite_entity.Invite{}, retryable(err, "creating the team's first invite")
	}
	return inv, nil
}

func (h *Handler) slugTaken(ctx context.Context, slug string) (bool, error) {
	res, err := h.core.Team().FetchTeamBySlug(ctx,
		team_types.FetchTeamBySlugRequest{Slug: slug, Limit: 1}, teammod.WithSkipCache())
	if err != nil {
		return false, retryable(err, "checking the team slug")
	}
	return len(res.Results) > 0, nil
}
