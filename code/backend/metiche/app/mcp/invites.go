package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/guregu/null/v6"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	invitemod "github.com/mklfarha/metiche/backend/core/module/invite"
	invite_types "github.com/mklfarha/metiche/backend/core/module/invite/types"
	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	invite_entity "github.com/mklfarha/metiche/backend/entity/invite"
	member_entity "github.com/mklfarha/metiche/backend/entity/member"
	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Tools: create_invite, list_invites, revoke_invite
// ─────────────────────────────────────────────
//
// Before these, a team's join code was shown exactly once, by create_team, and
// nothing could show or make another. These three are how a person gets a
// teammate in later (docs/CLI.md §1.6, §4.3-4.5, decision §10 Q1).
//
// What this file guarantees, and where:
//
//   - THE CODE IS SHOWN ONCE. create_invite returns it on the call that mints
//     it and nowhere else: list_invites never reads the column, a replay of the
//     same idempotency_key returns the invite WITHOUT it, and no error or log
//     line carries it (redactInviteCode). The invite row itself holds it in the
//     clear, because redemption looks invites up by code — see MintJoinCode.
//   - NO team_event. EventKind has no invite kind, and adding one is a schema
//     change. So there is no payload and no response_snapshot that could hold
//     the code, and idempotency is the create_team trick instead of a stored
//     response: the invite's primary key is derived from (member, key).
//   - SCOPED. Every call goes through RequireTeam, so the membership is live.
//     An owner acts on every invite of the team; a member only on invites they
//     created. Anything else — another team's invite, another member's, an id
//     that never existed, an id that is not a uuid — is one not_found, word for
//     word, so the tool cannot be used to discover invites.
//   - BOUNDED. Members are capped (§10 Q1), owners have a hard ceiling, and
//     creation is rate limited per account (createInvite, ratelimit.go).
//
// # One source of truth, two surfaces
//
// The rules live in CreateInviteAs, ListInvitesAs and RevokeInviteAs, which
// take a caller already pinned to a team (a Resolved: the account, its live
// membership and the team). The MCP tools below are thin: RequireTeam, then
// the shared function. The board's REST routes (app/webapi/invites.go) are the
// other caller: they authenticate a browser session, pin the team with
// InviteScope, and call the same three functions on the same Handler — so the
// defaults, the caps, the scope, the not_found and the per-account rate limit
// are the same values, and the budget is one budget whichever surface spends
// it.
//
// A refusal is an *InviteRefusal. Its Error() is the exact text the tools have
// always returned, so an MCP client sees the same bytes; Code lets the REST
// surface pick a status without parsing that text. Every other error is an
// infrastructure failure (retryable wraps it), never a verdict.

// createInviteNamespace derives an invite's uuid from (member, idempotency
// key). A sibling of createTeamNamespace, never the same value.
var createInviteNamespace = uuid.Must(uuid.FromString("6d657469-6368-4520-9465-616d73000002"))

const (
	// defaultInviteMaxUses is one: the common case is one teammate, and an
	// invite that admits one person cannot be forwarded into a crowd.
	defaultInviteMaxUses = 1
	// defaultInviteHours is 7 days.
	defaultInviteHours = 7 * 24

	// ownerInviteMaxUses and ownerInviteMaxHours are the hard ceiling for
	// anyone. A bigger door than that is a public link, not an invite.
	ownerInviteMaxUses  = 100
	ownerInviteMaxHours = 30 * 24

	// memberInviteMaxUses and memberInviteMaxHours are decision §10 Q1: a
	// plain member may add teammates without waiting on the owner, and the
	// caps bound what a member's invite can admit.
	memberInviteMaxUses  = 25
	memberInviteMaxHours = 7 * 24

	// inviteLabelMax is under invite.label's 120 so validation never refuses
	// a label a person typed.
	inviteLabelMax = 80

	// inviteMintAttempts covers a draw that collides on uq_invite_code, which
	// at 32^10 codes is a concurrent-retry case rather than a probability.
	inviteMintAttempts = 3

	// listInvitesMax is a runaway guard, not a product limit.
	listInvitesMax = 100

	installerCommand = "curl -fsSL https://metiche.xyz/install.sh | sh"
)

// Invite bounds, exported for the board's form: what it shows as the defaults
// and the caps. The server enforces them either way (inviteBounds).
const (
	InviteDefaultMaxUses = defaultInviteMaxUses
	InviteDefaultHours   = defaultInviteHours
	InviteOwnerMaxUses   = ownerInviteMaxUses
	InviteOwnerMaxHours  = ownerInviteMaxHours
	InviteMemberMaxUses  = memberInviteMaxUses
	InviteMemberMaxHours = memberInviteMaxHours
	InviteLabelMax       = inviteLabelMax
)

// mintInviteCode is MintJoinCode, the generator ensureFirstInvite uses. A
// variable only so a test can draw a known canary and prove where it never
// ends up.
var mintInviteCode = MintJoinCode

// ─────────────────────────────────────────────
// Refusals
// ─────────────────────────────────────────────

// Refusal codes: the machine prefix every refusal's text starts with.
const (
	InviteInvalidArgument = "invalid_argument"
	InviteNotPermitted    = "not_permitted"
	InviteRateLimited     = "rate_limited"
	InviteNotFound        = "not_found"
)

// InviteRefusal is a verdict about the request, as opposed to a failure to
// decide. Error() is the whole caller-facing text, unchanged from what the MCP
// tools returned before the rules were shared.
type InviteRefusal struct {
	// Code is one of the Invite* refusal codes above.
	Code string
	// RetryAfter is set for InviteRateLimited: how long until the window
	// resets.
	RetryAfter time.Duration

	text string
}

func (e *InviteRefusal) Error() string { return e.text }

func inviteRefusal(code, format string, args ...any) *InviteRefusal {
	return &InviteRefusal{Code: code, text: fmt.Sprintf(format, args...)}
}

// ErrNoInviteScope is InviteScope's one refusal: no active team with that
// slug, no membership, a revoked or inactive one, or an inactive account. One
// error for all of them, so the REST surface can answer all of them with the
// same bytes as an unknown team.
var ErrNoInviteScope = errors.New("not a live member of that team")

// InviteScope pins an already-authenticated ACCOUNT to a team by slug, the way
// RequireTeam does for a token: the team must be active and the account a live
// member of it. It is the board's entry point; the MCP tools use RequireTeam.
//
// Every refusal is ErrNoInviteScope. Any other error means the answer is
// unknown (the database failed) and must not be rendered as a refusal.
func (h *Handler) InviteScope(ctx context.Context, account account_entity.Account, teamSlug string) (Resolved, error) {
	slug := strings.ToLower(strings.TrimSpace(teamSlug))
	if slug == "" || account.ID.IsNil() || account.Status != enums.RECORD_STATUS_ACTIVE {
		return Resolved{}, ErrNoInviteScope
	}
	res, err := h.core.Team().FetchTeamBySlug(ctx,
		team_types.FetchTeamBySlugRequest{Slug: slug, Limit: 1}, teammod.WithSkipCache())
	if err != nil {
		return Resolved{}, fmt.Errorf("looking up the team: %w", err)
	}
	if len(res.Results) == 0 || res.Results[0].Status != enums.RECORD_STATUS_ACTIVE {
		return Resolved{}, ErrNoInviteScope
	}
	team := res.Results[0]
	member, found, err := h.memberByAccount(ctx, nil, account.ID, team.ID)
	if err != nil {
		return Resolved{}, err
	}
	if !found || member.RevokedAt.Valid || member.Status != enums.RECORD_STATUS_ACTIVE {
		return Resolved{}, ErrNoInviteScope
	}
	return Resolved{Account: account, Member: member, Team: team}, nil
}

// ─────────────────────────────────────────────
// create_invite
// ─────────────────────────────────────────────

type CreateInviteParams struct {
	TeamSlug       string `json:"team_slug,omitempty" jsonschema:"The team to invite someone to. Omit it when you are on one team."`
	Label          string `json:"label,omitempty" jsonschema:"A note to recognise the invite by later, e.g. 'for Ana' or 'hack night'. Up to 80 characters. Never put the code in it."`
	MaxUses        int    `json:"max_uses,omitempty" jsonschema:"How many people can join with it. Defaults to 1. Owners may go up to 100; members up to 25."`
	ExpiresInHours int    `json:"expires_in_hours,omitempty" jsonschema:"How long it works, in hours. Defaults to 168 (7 days). Owners may go up to 720 (30 days); members up to 168."`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional, at least 8 characters (a uuid is ideal). Retrying with the same key returns the same invite instead of a second one, but WITHOUT the code: it is shown only on the call that created it."`
}

// CreateInviteResult is not an Envelope: an invite is not an event on the
// team, so there is no sequence it advanced.
type CreateInviteResult struct {
	OK       bool   `json:"ok"`
	TeamSlug string `json:"team_slug"`
	InviteID string `json:"invite_id"`
	Label    string `json:"label"`
	// Code is present only when this call created the invite.
	Code      string    `json:"code,omitempty"`
	MaxUses   int64     `json:"max_uses"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	Created   bool      `json:"created"`
	ShareNote string    `json:"share_note"`
}

// InviteRequest is what creating an invite takes, on every surface. Zero means
// "not given" for both numbers.
type InviteRequest struct {
	Label          string
	MaxUses        int
	ExpiresInHours int
	IdempotencyKey string
}

// CreateInvite mints an invite on the caller's team.
func (h *Handler) CreateInvite(ctx context.Context, _ *mcp.CallToolRequest, in CreateInviteParams) (*mcp.CallToolResult, any, error) {
	rs, err := h.RequireTeam(ctx, in.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	out, err := h.CreateInviteAs(ctx, rs, InviteRequest{
		Label: in.Label, MaxUses: in.MaxUses, ExpiresInHours: in.ExpiresInHours, IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return nil, nil, err
	}
	return jsonValue(out)
}

// CreateInviteAs mints an invite for a caller already pinned to a team: the
// rate limit, the bounds, the idempotency key and the insert, in that order.
func (h *Handler) CreateInviteAs(ctx context.Context, rs Resolved, in InviteRequest) (CreateInviteResult, error) {
	if ok, wait := h.accountLimiters().createInvite.Allow(rs.Account.ID.String()); !ok {
		r := inviteRefusal(InviteRateLimited,
			"rate_limited: too many invites created by this account in the last hour; try again in %s", wait)
		r.RetryAfter = wait
		return CreateInviteResult{}, r
	}

	maxUses, hours, err := inviteBounds(rs.Member.Role == enums.MEMBER_ROLE_OWNER, in.MaxUses, in.ExpiresInHours)
	if err != nil {
		return CreateInviteResult{}, err
	}

	id := uuid.Must(uuid.NewV4())
	if idem := strings.TrimSpace(in.IdempotencyKey); idem != "" {
		if len(idem) < minIdempotencyKeyLength {
			return CreateInviteResult{}, inviteRefusal(InviteInvalidArgument,
				"invalid_argument: idempotency_key must be at least %d characters (a uuid is ideal), or omitted", minIdempotencyKeyLength)
		}
		// The member id is per team, so one person's key on two teams, or two
		// people's same key, derive two different invites.
		id = uuid.NewV5(createInviteNamespace, "create_invite:"+rs.Member.ID.String()+"|"+idem)
		existing, found, err := h.inviteByID(ctx, id)
		if err != nil {
			return CreateInviteResult{}, err
		}
		if found {
			return inviteReplay(rs.Team.Slug, existing), nil
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	createdBy := rs.Member.ID
	label := truncate(in.Label, inviteLabelMax)
	inv := invite_entity.Invite{
		ID:                  id,
		TeamUUID:            rs.Team.ID,
		Label:               nullString(label),
		CreatedByMemberUUID: &createdBy,
		MaxUses:             null.IntFrom(int64(maxUses)),
		Uses:                0,
		ExpiresAt:           nullTime(now.Add(time.Duration(hours) * time.Hour)),
		Status:              enums.INVITE_STATUS_ACTIVE,
	}

	var lastErr error
	for attempt := 0; attempt < inviteMintAttempts; attempt++ {
		code, err := mintInviteCode()
		if err != nil {
			return CreateInviteResult{}, err
		}
		inv.Code = code
		_, err = h.core.Invite().Insert(ctx, invite_types.UpsertRequest{Invite: inv}, invitemod.WithSkipCache())
		if err == nil {
			return CreateInviteResult{
				OK:        true,
				TeamSlug:  rs.Team.Slug,
				InviteID:  inv.ID.String(),
				Label:     label,
				Code:      code,
				MaxUses:   int64(maxUses),
				ExpiresAt: inv.ExpiresAt.Time,
				State:     "active",
				Created:   true,
				ShareNote: inviteShareNote(rs.Team.Slug, int64(maxUses), inv.ExpiresAt.Time),
			}, nil
		}
		// A concurrent retry with the same idempotency_key won the primary
		// key: the row that exists is the answer, without its code.
		if existing, found, lookupErr := h.inviteByID(ctx, id); lookupErr == nil && found {
			return inviteReplay(rs.Team.Slug, existing), nil
		}
		// The driver's duplicate-key message quotes the value, which is a
		// live code — this draw's, and so somebody's door key if it collided.
		// The error goes to the caller and to the tool-failure log line, so
		// the code is cut out before it can reach either.
		lastErr = redactInviteCode(err, code)
		if !strings.Contains(lastErr.Error(), "uq_invite_code") {
			break
		}
	}
	return CreateInviteResult{}, retryable(lastErr, "creating the invite")
}

// inviteBounds applies the defaults, the hard ceiling and the member caps.
// Zero means "not given": the params are omitempty ints.
func inviteBounds(owner bool, maxUses, hours int) (int, int, error) {
	if maxUses < 0 || hours < 0 {
		return 0, 0, inviteRefusal(InviteInvalidArgument,
			"invalid_argument: max_uses and expires_in_hours must be positive; omit them for the defaults (%d use, %d hours)",
			defaultInviteMaxUses, defaultInviteHours)
	}
	if maxUses == 0 {
		maxUses = defaultInviteMaxUses
	}
	if hours == 0 {
		hours = defaultInviteHours
	}
	if maxUses > ownerInviteMaxUses {
		return 0, 0, inviteRefusal(InviteInvalidArgument, "invalid_argument: max_uses may be at most %d", ownerInviteMaxUses)
	}
	if hours > ownerInviteMaxHours {
		return 0, 0, inviteRefusal(InviteInvalidArgument, "invalid_argument: expires_in_hours may be at most %d (30 days)", ownerInviteMaxHours)
	}
	if !owner && (maxUses > memberInviteMaxUses || hours > memberInviteMaxHours) {
		return 0, 0, inviteRefusal(InviteNotPermitted,
			"not_permitted: members may create invites with at most %d uses and %d hours (7 days); the team's owner can make larger ones",
			memberInviteMaxUses, memberInviteMaxHours)
	}
	return maxUses, hours, nil
}

func inviteShareNote(slug string, maxUses int64, expiresAt time.Time) string {
	times := "times"
	if maxUses == 1 {
		times = "time"
	}
	return fmt.Sprintf(
		"Share this code like a door code: only with the people you want on %s, and never in a commit, an issue, a PR or a public channel. "+
			"Your teammate runs the metiche installer (%s), chooses join and pastes the code. "+
			"It works %d %s until %s. It is shown only this once and never again.",
		slug, installerCommand, maxUses, times, expiresAt.UTC().Format(time.RFC3339))
}

// inviteReplay answers a repeated idempotency_key with the invite as it
// stands, and without its code.
//
// Handing the code back would be easy — it is on the row — and is refused on
// purpose. The code is shown once everywhere else (create_team's first invite
// aside, which predates this): list_invites never shows it, and a replay that
// did would be a second way to read any live code with nothing but a token and
// a key that often sits in a client's transcript. Losing the first answer
// costs a revoke and a new invite; a replayable door key costs the team.
func inviteReplay(slug string, inv invite_entity.Invite) CreateInviteResult {
	out := CreateInviteResult{
		OK:       true,
		TeamSlug: slug,
		InviteID: inv.ID.String(),
		Label:    inv.Label.String,
		MaxUses:  inv.MaxUses.Int64,
		State:    inviteState(inv.Status, inv.RevokedAt.Valid, inv.MaxUses.Valid, inv.MaxUses.Int64, inv.Uses, inv.ExpiresAt.Valid, inv.ExpiresAt.Time, time.Now().UTC()),
		Created:  false,
		ShareNote: "This invite was already created by an earlier call with this idempotency_key, so no second one was made. " +
			"Its code was shown once, on that call, and is never shown again. If you no longer have it, " +
			"call revoke_invite with this invite_id and create_invite with a new idempotency_key.",
	}
	if inv.ExpiresAt.Valid {
		out.ExpiresAt = inv.ExpiresAt.Time.UTC()
	}
	return out
}

// redactInviteCode strips a code out of an error's text, whatever its case.
func redactInviteCode(err error, code string) error {
	if err == nil || code == "" {
		return err
	}
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(code))
	if !re.MatchString(err.Error()) {
		return err
	}
	return errors.New(re.ReplaceAllString(err.Error(), "<redacted>"))
}

// ─────────────────────────────────────────────
// list_invites
// ─────────────────────────────────────────────

type ListInvitesParams struct {
	TeamSlug string `json:"team_slug,omitempty" jsonschema:"The team whose invites to list. Omit it when you are on one team."`
}

// InviteInfo is one invite, never with its code: the column is not even read.
type InviteInfo struct {
	InviteID     string `json:"invite_id"`
	Label        string `json:"label"`
	CreatedBy    string `json:"created_by"`
	CreatedByYou bool   `json:"created_by_you"`
	Uses         int64  `json:"uses"`
	// MaxUses is null for an uncapped invite (a team's first one).
	MaxUses *int64 `json:"max_uses"`
	// ExpiresAt is null for an invite that never expires.
	ExpiresAt  *time.Time `json:"expires_at"`
	State      string     `json:"state"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type ListInvitesResult struct {
	OK       bool   `json:"ok"`
	TeamSlug string `json:"team_slug"`
	// Scope is "team" for an owner and "created_by_you" for a member.
	Scope   string       `json:"scope"`
	Invites []InviteInfo `json:"invites"`
	More    bool         `json:"more,omitempty"`
	Note    string       `json:"note"`
}

// ListInvites lists the invites the caller may act on, newest first.
func (h *Handler) ListInvites(ctx context.Context, _ *mcp.CallToolRequest, in ListInvitesParams) (*mcp.CallToolResult, any, error) {
	rs, err := h.RequireTeam(ctx, in.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	out, err := h.ListInvitesAs(ctx, rs)
	if err != nil {
		return nil, nil, err
	}
	return jsonValue(out)
}

// ListInvitesAs lists the invites a caller pinned to a team may act on: every
// invite of the team for its owner, only the ones they created for a member.
func (h *Handler) ListInvitesAs(ctx context.Context, rs Resolved) (ListInvitesResult, error) {
	owner := rs.Member.Role == enums.MEMBER_ROLE_OWNER

	// `code` is deliberately absent from this SELECT.
	query := "SELECT i.`id`, i.`label`, i.`created_by_member_uuid`, m.`display_name`, i.`uses`, i.`max_uses`, " +
		"i.`expires_at`, i.`revoked_at`, i.`status`, i.`last_used_at`, i.`created_at` " +
		"FROM `invite` i LEFT JOIN `member` m ON m.`id` = i.`created_by_member_uuid` " +
		"WHERE i.`team_uuid` = ?"
	args := []any{rs.Team.ID.String()}
	if !owner {
		query += " AND i.`created_by_member_uuid` = ?"
		args = append(args, rs.Member.ID.String())
	}
	query += " ORDER BY i.`created_at` DESC, i.`id` ASC LIMIT ?"
	args = append(args, listInvitesMax+1)

	rows, err := h.core.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return ListInvitesResult{}, retryable(err, "listing the team's invites")
	}
	defer func() { _ = rows.Close() }()

	now := time.Now().UTC()
	out := ListInvitesResult{OK: true, TeamSlug: rs.Team.Slug, Invites: []InviteInfo{}}
	for rows.Next() {
		var (
			id                         string
			label, createdBy, name     sql.NullString
			uses, status               int64
			maxUses                    sql.NullInt64
			expires, revoked, lastUsed sql.NullTime
			createdAt                  time.Time
		)
		if err := rows.Scan(&id, &label, &createdBy, &name, &uses, &maxUses,
			&expires, &revoked, &status, &lastUsed, &createdAt); err != nil {
			return ListInvitesResult{}, retryable(err, "reading the team's invites")
		}
		if len(out.Invites) == listInvitesMax {
			out.More = true
			break
		}
		info := InviteInfo{
			InviteID:     id,
			Label:        label.String,
			CreatedBy:    name.String,
			CreatedByYou: createdBy.Valid && createdBy.String == rs.Member.ID.String(),
			Uses:         uses,
			State: inviteState(enums.InviteStatus(status), revoked.Valid,
				maxUses.Valid, maxUses.Int64, uses, expires.Valid, expires.Time, now),
			CreatedAt: createdAt.UTC(),
		}
		if maxUses.Valid {
			v := maxUses.Int64
			info.MaxUses = &v
		}
		if expires.Valid {
			v := expires.Time.UTC()
			info.ExpiresAt = &v
		}
		if lastUsed.Valid {
			v := lastUsed.Time.UTC()
			info.LastUsedAt = &v
		}
		out.Invites = append(out.Invites, info)
	}
	if err := rows.Err(); err != nil {
		return ListInvitesResult{}, retryable(err, "listing the team's invites")
	}

	out.Note = "Codes are shown only by create_invite, once. To share a code you no longer have, create a new invite and revoke the old one."
	if owner {
		out.Scope = "team"
	} else {
		out.Scope = "created_by_you"
		out.Note = "You see only the invites you created. " + out.Note
	}
	return out, nil
}

// inviteState is the EFFECTIVE state, by the same predicate redeemInvite
// applies: a row still marked active is expired once expires_at has passed
// and exhausted once its uses reach max_uses.
func inviteState(status enums.InviteStatus, revoked, capped bool, maxUses, uses int64, expires bool, expiresAt, now time.Time) string {
	switch {
	case revoked || status == enums.INVITE_STATUS_REVOKED:
		return "revoked"
	case status == enums.INVITE_STATUS_EXHAUSTED || (capped && uses >= maxUses):
		return "exhausted"
	case status == enums.INVITE_STATUS_EXPIRED || (expires && !expiresAt.After(now)):
		return "expired"
	default:
		return "active"
	}
}

// ─────────────────────────────────────────────
// revoke_invite
// ─────────────────────────────────────────────

type RevokeInviteParams struct {
	TeamSlug string `json:"team_slug,omitempty" jsonschema:"The team the invite is on. Omit it when you are on one team."`
	InviteID string `json:"invite_id" jsonschema:"The invite's invite_id, as list_invites or create_invite returned it."`
}

type RevokeInviteResult struct {
	OK             bool      `json:"ok"`
	TeamSlug       string    `json:"team_slug"`
	InviteID       string    `json:"invite_id"`
	Label          string    `json:"label"`
	State          string    `json:"state"`
	AlreadyRevoked bool      `json:"already_revoked"`
	Uses           int64     `json:"uses"`
	MaxUses        *int64    `json:"max_uses"`
	RevokedAt      time.Time `json:"revoked_at"`
	Note           string    `json:"note"`
}

// RevokeInvite ends an invite. Idempotent: revoking a revoked invite changes
// nothing and says so.
func (h *Handler) RevokeInvite(ctx context.Context, _ *mcp.CallToolRequest, in RevokeInviteParams) (*mcp.CallToolResult, any, error) {
	rs, err := h.RequireTeam(ctx, in.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	out, err := h.RevokeInviteAs(ctx, rs, in.InviteID)
	if err != nil {
		return nil, nil, err
	}
	return jsonValue(out)
}

// RevokeInviteAs ends one invite of the caller's team, if the caller may.
func (h *Handler) RevokeInviteAs(ctx context.Context, rs Resolved, inviteID string) (RevokeInviteResult, error) {
	raw := strings.TrimSpace(inviteID)
	if raw == "" {
		return RevokeInviteResult{}, inviteRefusal(InviteInvalidArgument, "invalid_argument: invite_id is required; call list_invites to find it")
	}
	// One answer for every invite the caller may not act on. It names neither
	// the id nor why, so it cannot tell "another team's" from "another
	// member's" from "never existed".
	notFound := inviteRefusal(InviteNotFound,
		"not_found: no invite with that invite_id on team %s that you can revoke. Call list_invites to see the invites you can revoke",
		rs.Team.Slug)

	id, err := uuid.FromString(raw)
	if err != nil {
		return RevokeInviteResult{}, notFound
	}
	inv, found, err := h.inviteByID(ctx, id)
	if err != nil {
		return RevokeInviteResult{}, err
	}
	if !found || inv.TeamUUID != rs.Team.ID || !mayRevokeInvite(rs.Member, inv) {
		return RevokeInviteResult{}, notFound
	}

	now := time.Now().UTC().Truncate(time.Second)
	res, err := h.core.DB().ExecContext(ctx,
		"UPDATE `invite` SET `status` = ?, `revoked_at` = ?, `revoked_by_member_uuid` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `team_uuid` = ? AND `revoked_at` IS NULL",
		enums.INVITE_STATUS_REVOKED, now, rs.Member.ID.String(), now, inv.ID.String(), rs.Team.ID.String())
	if err != nil {
		return RevokeInviteResult{}, retryable(err, "revoking the invite")
	}
	n, _ := res.RowsAffected()

	after, found, err := h.inviteByID(ctx, id)
	if err != nil {
		return RevokeInviteResult{}, err
	}
	if !found {
		return RevokeInviteResult{}, notFound
	}
	out := RevokeInviteResult{
		OK:             true,
		TeamSlug:       rs.Team.Slug,
		InviteID:       after.ID.String(),
		Label:          after.Label.String,
		State:          inviteState(after.Status, after.RevokedAt.Valid, after.MaxUses.Valid, after.MaxUses.Int64, after.Uses, after.ExpiresAt.Valid, after.ExpiresAt.Time, time.Now().UTC()),
		AlreadyRevoked: n == 0,
		Uses:           after.Uses,
		Note:           "Revoked. People who already joined with it keep their access; nobody new can join with this code.",
	}
	if after.MaxUses.Valid {
		v := after.MaxUses.Int64
		out.MaxUses = &v
	}
	if after.RevokedAt.Valid {
		out.RevokedAt = after.RevokedAt.Time.UTC()
	}
	if out.AlreadyRevoked {
		out.Note = "This invite was already revoked; nothing changed. People who already joined with it keep their access."
	}
	return out, nil
}

// mayRevokeInvite: the team's owner may revoke any of its invites, a member
// only the ones they created. The team itself is checked by the caller.
func mayRevokeInvite(member member_entity.Member, inv invite_entity.Invite) bool {
	if member.Role == enums.MEMBER_ROLE_OWNER {
		return true
	}
	return inv.CreatedByMemberUUID != nil && *inv.CreatedByMemberUUID == member.ID
}

// ─────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────

func (h *Handler) inviteByID(ctx context.Context, id uuid.UUID) (invite_entity.Invite, bool, error) {
	res, err := h.core.Invite().FetchInviteByID(ctx,
		invite_types.FetchInviteByIDRequest{ID: id}, invitemod.WithSkipCache())
	if err != nil {
		return invite_entity.Invite{}, false, retryable(err, "reading the invite")
	}
	if len(res.Results) == 0 {
		return invite_entity.Invite{}, false, nil
	}
	return res.Results[0], true, nil
}
