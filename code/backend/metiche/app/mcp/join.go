package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentmod "github.com/mklfarha/metiche/backend/core/module/agent"
	agent_types "github.com/mklfarha/metiche/backend/core/module/agent/types"
	membermod "github.com/mklfarha/metiche/backend/core/module/member"
	member_types "github.com/mklfarha/metiche/backend/core/module/member/types"
	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	agent_entity "github.com/mklfarha/metiche/backend/entity/agent"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	member_entity "github.com/mklfarha/metiche/backend/entity/member"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Tool: join_team
// ─────────────────────────────────────────────

type JoinTeamParams struct {
	JoinCode   string `json:"join_code" jsonschema:"The team's join code, from whoever set the team up or from anyone on the team who made you an invite. Case-insensitive."`
	MemberName string `json:"member_name" jsonschema:"The name of the PERSON you are working for, as their teammates would write it - 'Ana', 'Mark Farha'. Several agents belonging to one person share this, which is how metiche knows two colliding sessions are the same human and softens the warning."`
	AgentLabel string `json:"agent_label" jsonschema:"A short label for THIS agent instance, distinguishing it from the person's other agents - 'backend', 'ui', 'tests'."`
	ClientKey  string `json:"client_key" jsonschema:"A stable identifier for this agent process that survives a restart - your session id, or a hash of the working directory plus the label. Re-joining with the same client_key re-joins as the SAME agent instead of creating a duplicate on the board."`
	ClientKind string `json:"client_kind,omitempty" jsonschema:"What kind of client you are, e.g. 'claude-code', 'cursor', 'codex'."`
}

// JoinTeamResult is the one response in this server that is not just the
// envelope: on a first contact it carries the freshly minted bearer token.
//
// The token is shown ONCE. Only its sha256 is stored, so nobody — not the
// server, not a database dump, not the event log — can produce it again.
//
// It is EMPTY when the request already carried a token, which is the v3
// change worth reading twice: the token belongs to the person, not to the
// agent, so joining a second team with the credential you already have does
// not (and must not) mint a second one.
type JoinTeamResult struct {
	Envelope
	Token      string `json:"token,omitempty"`
	AccountKey string `json:"account_key"`
	TeamSlug   string `json:"team_slug"`
	MemberKey  string `json:"member_key"`
	AgentKey   string `json:"agent_key"`
	TokenNote  string `json:"token_note"`
	Rejoined   bool   `json:"rejoined,omitempty"`
	AuthHeader string `json:"auth_header,omitempty"`
}

// JoinTeam redeems an invite code and puts the caller on the team.
//
// v3 split what v1 did in one step into three that are now separately true:
//
//	the ACCOUNT   is the person. Minted here on a first contact — anonymously,
//	              with no email and no signup — and carried on every later
//	              request as the bearer token. Reused when the caller already
//	              has one, so one person on three teams has one credential.
//	the MEMBER    is this person ON THIS TEAM. Idempotent on
//	              (account_uuid, team_uuid).
//	the AGENT     is this client process. Idempotent on
//	              (account_uuid, client_key), so a restarted agent re-joins as
//	              ITSELF rather than appearing as a second agent on the board
//	              next to the ghost of its previous run.
//
// It is the one mutating tool that does NOT replay a stored response, and the
// reason is worth stating plainly: the response can contain a bearer token, a
// replayable response is a PERSISTED response, and persisting a bearer token
// in the event log would defeat the point of only ever storing its hash. So
// join_team stores a redacted envelope and carries a unique idempotency key
// on every call. The identity is idempotent; the secret is not, by
// construction.
func (h *Handler) JoinTeam(ctx context.Context, _ *mcp.CallToolRequest, args JoinTeamParams) (*mcp.CallToolResult, any, error) {
	// Rate limited before the join code is even looked at: this is the only
	// tool where an invite code can be guessed at, so the budget is the thing
	// standing between a public endpoint and an offline-speed search.
	if ok, retryIn := h.joinLimit.Allow(ClientIPFromContext(ctx)); !ok {
		return nil, nil, fmt.Errorf("too many join attempts from this address; try again in %s", retryIn)
	}

	_, team, err := h.redeemInvite(ctx, args.JoinCode)
	if err != nil {
		return nil, nil, err
	}

	out, err := h.joinAs(ctx, team, args.MemberName, args.AgentLabel, args.ClientKey, args.ClientKind)
	if err != nil {
		return nil, nil, err
	}
	return jsonValue(out)
}

// joinAs is the whole of joining: find or mint the person, find or create
// their membership, find or create the agent. Shared by join_team and
// create_team, which differ only in how the caller proved they are allowed to
// be here.
func (h *Handler) joinAs(ctx context.Context, team team_entity.Team, memberName, agentLabel, rawClientKey, clientKind string) (JoinTeamResult, error) {
	displayName := truncate(memberName, 120)
	clientKey := truncate(rawClientKey, 120)
	if clientKey == "" {
		return JoinTeamResult{}, errors.New("client_key is required — it is what lets a restarted agent re-join as itself rather than as a duplicate")
	}
	label := truncate(agentLabel, 80)
	if label == "" {
		label = "agent"
	}

	// ── the person ──────────────────────────────────────────────────────────
	// A request that already carries a token is already somebody; only a
	// first contact mints an identity. Minting one per call would hand the
	// same human a new anonymous account every time they joined a team, and
	// the whole reason `account` exists in v3 is that it is the person ACROSS
	// teams.
	acct, hadAccount := AccountFromContext(ctx)
	var token string
	if !hadAccount {
		var err error
		acct, token, err = h.mintAccount(ctx, displayName)
		if err != nil {
			return JoinTeamResult{}, err
		}
	}

	// ── the membership ──────────────────────────────────────────────────────
	member, err := h.ensureMember(ctx, team, acct, displayName)
	if err != nil {
		return JoinTeamResult{}, err
	}

	// A unique key per call. join_team must never replay, because its
	// response can carry the token and the stored snapshot deliberately does
	// not.
	nonce, err := uuid.NewV4()
	if err != nil {
		return JoinTeamResult{}, err
	}

	var (
		agentKey string
		rejoined bool
	)
	response, err := h.commit(ctx, Mutation{
		TeamUUID:       team.ID,
		IdempotencyKey: "agent_joined:" + nonce.String(),
		Kind:           enums.EVENT_KIND_AGENT_JOINED,
		Structural:     true,
		MemberUUID:     uuidPtr(member.ID),
		SubjectKind:    enums.SUBJECT_KIND_INVALID,
		Summary:        fmt.Sprintf("%s connected agent %q", member.DisplayName, label),
		Payload:        payload_entity.EventPayload{Message: nullString(label)},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			existing, found, err := h.agentByClientKey(ctx, tc.Tx, acct.ID, clientKey)
			if err != nil {
				return err
			}
			if found {
				rejoined = true
				agentKey = existing.Key
				existing.Label = label
				existing.ClientKind = nullString(truncate(clientKind, 60))
				existing.Status = enums.AGENT_STATUS_ACTIVE
				existing.LastSeenAt = nullTime(tc.Now)
				_, err = h.core.Agent().Update(ctx,
					agent_types.UpsertRequest{Agent: existing}, agentmod.WithSQLTransaction(tc.Tx))
				return err
			}

			id, err := uuid.NewV4()
			if err != nil {
				return err
			}
			agentKey = tc.Key("A")
			_, err = h.core.Agent().Insert(ctx, agent_types.UpsertRequest{
				Agent: agent_entity.Agent{
					ID:          id,
					AccountUUID: acct.ID,
					Key:         agentKey,
					Label:       label,
					ClientKind:  nullString(truncate(clientKind, 60)),
					ClientKey:   clientKey,
					Status:      enums.AGENT_STATUS_ACTIVE,
					LastSeenAt:  nullTime(tc.Now),
				},
			}, agentmod.WithSQLTransaction(tc.Tx))
			return err
		},
		// See the Snapshot field on Mutation: what is stored is what the
		// caller gets MINUS the token, which is never persisted anywhere but
		// as a hash on the account row.
		Snapshot: json.RawMessage(`{"ok":true,"note":"agent joined; the response is not replayed because it can carry a one-time token","pending":{"instructions":0,"conflicts":0,"reviews":0}}`),
	})
	if err != nil {
		return JoinTeamResult{}, err
	}

	var env Envelope
	if err := json.Unmarshal(response, &env); err != nil {
		return JoinTeamResult{}, err
	}
	env.Key = agentKey

	out := JoinTeamResult{
		Envelope:   env,
		AccountKey: acct.Key,
		TeamSlug:   team.Slug,
		MemberKey:  member.Key,
		AgentKey:   agentKey,
		Rejoined:   rejoined,
	}
	if token != "" {
		out.Token = token
		out.AuthHeader = "Authorization: Bearer " + token
		out.TokenNote = "Send this on every later MCP request as an Authorization: Bearer header. " +
			"It identifies YOU, not this agent, so the same token works for every team you join and every agent you run. " +
			"metiche stores only its sha256 and cannot show it to you again."
		out.Envelope.Note = "save the token; it is shown once and cannot be recovered. Call start_session next."
	} else {
		out.TokenNote = "no new token: this request already carried yours, and one person has one token across every team. Keep using it."
		out.Envelope.Note = "already authenticated; keep using your existing token. Call start_session next."
	}
	if rejoined {
		out.Envelope.Note = "re-joined as the same agent. " + out.Envelope.Note
	}
	return out, nil
}

// ensureMember finds or creates this account's membership of this team.
//
// The uniqueness that matters is uq_member_account_team: one person is one
// row on a team however many agents they run and however many times they
// re-join. member.key is still per-team-unique and still derived from the
// display name, but it is now a LABEL rather than the identity — so two
// different people who are both called Ana get ana and ana-<suffix> instead
// of one of them failing on uq_member_team_key.
func (h *Handler) ensureMember(ctx context.Context, team team_entity.Team, acct account_entity.Account, displayName string) (member_entity.Member, error) {
	if existing, found, err := h.memberByAccount(ctx, nil, acct.ID, team.ID); err != nil {
		return member_entity.Member{}, err
	} else if found {
		if existing.RevokedAt.Valid || existing.Status != enums.RECORD_STATUS_ACTIVE {
			// Reinstated rather than refused: a revoked member who turns up
			// with a currently-valid invite has been re-admitted by whoever
			// issued that invite, and an invite is revocable precisely so
			// that this is the team's decision and not ours.
			now := time.Now().UTC()
			if _, err := h.core.DB().ExecContext(ctx,
				"UPDATE `member` SET `revoked_at` = NULL, `status` = ?, `last_seen_at` = ?, `updated_at` = ? WHERE `id` = ?",
				enums.RECORD_STATUS_ACTIVE, now, now, existing.ID.String()); err != nil {
				return member_entity.Member{}, retryable(err, "reinstating your membership")
			}
			existing.RevokedAt = nullTime(time.Time{})
			existing.Status = enums.RECORD_STATUS_ACTIVE
		}
		return existing, nil
	}

	memberKey, err := h.freeMemberKey(ctx, team.ID, displayName, acct)
	if err != nil {
		return member_entity.Member{}, err
	}
	name := displayName
	if name == "" {
		name = acct.DisplayName
	}

	// Created through the same write path as everything else, so a person
	// appearing on the board is an event like any other. The idempotency key
	// is the ACCOUNT, so two of one person's agents joining at the same
	// moment produce one member and one event.
	if _, err := h.commit(ctx, Mutation{
		TeamUUID:       team.ID,
		IdempotencyKey: "member_joined:" + acct.ID.String(),
		Kind:           enums.EVENT_KIND_MEMBER_JOINED,
		Structural:     true, // a new lane on the board
		SubjectKey:     memberKey,
		Summary:        fmt.Sprintf("%s joined the team", name),
		Envelope:       Envelope{Key: memberKey},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			id, err := uuid.NewV4()
			if err != nil {
				return err
			}
			_, err = h.core.Member().Insert(ctx, member_types.UpsertRequest{
				Member: member_entity.Member{
					ID:          id,
					TeamUUID:    team.ID,
					AccountUUID: acct.ID,
					Key:         memberKey,
					DisplayName: name,
					Role:        enums.MEMBER_ROLE_MEMBER,
					Status:      enums.RECORD_STATUS_ACTIVE,
					LastSeenAt:  nullTime(tc.Now),
				},
			}, membermod.WithSQLTransaction(tc.Tx))
			return err
		},
	}); err != nil {
		return member_entity.Member{}, err
	}

	// Re-read rather than trusting the uuid we generated: the commit above
	// may have been a replay of a concurrent caller's insert, in which case
	// the row that exists is theirs, not ours.
	member, found, err := h.memberByAccount(ctx, nil, acct.ID, team.ID)
	if err != nil {
		return member_entity.Member{}, err
	}
	if !found {
		return member_entity.Member{}, errors.New("the member row vanished immediately after being created; retry join_team")
	}
	return member, nil
}

// freeMemberKey picks a per-team key that is not already somebody else's.
func (h *Handler) freeMemberKey(ctx context.Context, teamUUID uuid.UUID, displayName string, acct account_entity.Account) (string, error) {
	base := slugKey(displayName, 32)
	if base == "" {
		base = slugKey(acct.DisplayName, 32)
	}
	if base == "" {
		return "", errors.New("member_name is required, and must contain at least one letter or digit")
	}
	existing, found, err := h.memberByKey(ctx, nil, teamUUID, base)
	if err != nil {
		return "", err
	}
	if !found || existing.AccountUUID == acct.ID {
		return base, nil
	}
	// Someone else on this team already answers to that name. The suffix is
	// derived from the account rather than random, so a retry computes the
	// same key and lands on the same row.
	suffix := "-" + acct.Key[:6]
	return truncate(base, 32-len(suffix)) + suffix, nil
}
