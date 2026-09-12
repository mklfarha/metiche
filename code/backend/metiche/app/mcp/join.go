package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentmod "github.com/mklfarha/metiche/backend/core/module/agent"
	agent_types "github.com/mklfarha/metiche/backend/core/module/agent/types"
	membermod "github.com/mklfarha/metiche/backend/core/module/member"
	member_types "github.com/mklfarha/metiche/backend/core/module/member/types"
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
	JoinCode   string `json:"join_code" jsonschema:"The team's join code, from whoever set the team up. Case-insensitive."`
	MemberName string `json:"member_name" jsonschema:"The name of the PERSON you are working for, as their teammates would write it - 'Ana', 'Mark Farha'. Several agents belonging to one person share this, which is how metiche knows two colliding sessions are the same human and softens the warning."`
	AgentLabel string `json:"agent_label" jsonschema:"A short label for THIS agent instance, distinguishing it from the person's other agents - 'backend', 'ui', 'tests'."`
	ClientKey  string `json:"client_key" jsonschema:"A stable identifier for this agent process that survives a restart - your session id, or a hash of the working directory plus the label. Re-joining with the same client_key re-joins as the SAME agent instead of creating a duplicate on the board."`
	ClientKind string `json:"client_kind,omitempty" jsonschema:"What kind of client you are, e.g. 'claude-code', 'cursor', 'codex'."`
}

// JoinTeamResult is the one response in this server that is not just the
// envelope: it carries the freshly minted bearer token.
//
// The token is shown ONCE. Only its sha256 is stored, so nobody — not the
// server, not a database dump, not the event log — can produce it again.
type JoinTeamResult struct {
	Envelope
	Token      string `json:"token"`
	TeamSlug   string `json:"team_slug"`
	MemberKey  string `json:"member_key"`
	AgentKey   string `json:"agent_key"`
	TokenNote  string `json:"token_note"`
	Rejoined   bool   `json:"rejoined,omitempty"`
	AuthHeader string `json:"auth_header"`
}

// JoinTeam exchanges a team join code for a per-agent bearer token.
//
// Idempotent on (member, client_key): uq_agent_member_client means a restarted
// agent re-joins as ITSELF rather than appearing as a second agent on the
// board next to the ghost of its previous run.
//
// It is the one mutating tool that does NOT replay a stored response, and the
// reason is worth stating plainly: the response contains a bearer token, a
// replayable response is a PERSISTED response, and persisting a bearer token
// in the event log would defeat the point of only ever storing its hash. So
// join_team stores a redacted envelope, carries a unique idempotency key on
// every call, and re-mints the token when an agent re-joins. The identity is
// idempotent; the secret is not, by construction.
func (h *Handler) JoinTeam(ctx context.Context, _ *mcp.CallToolRequest, args JoinTeamParams) (*mcp.CallToolResult, any, error) {
	// Rate limited before the join code is even looked at: this is the only
	// tool where a join code can be guessed at, so the budget is the thing
	// standing between a public endpoint and an offline-speed search.
	if ok, retryIn := h.joinLimit.Allow(ClientIPFromContext(ctx)); !ok {
		return nil, nil, fmt.Errorf("too many join attempts from this address; try again in %s", retryIn)
	}

	team, err := h.teamByJoinCode(ctx, args.JoinCode)
	if err != nil {
		return nil, nil, err
	}

	out, err := h.joinAs(ctx, team, args.MemberName, args.AgentLabel, args.ClientKey, args.ClientKind)
	if err != nil {
		return nil, nil, err
	}
	return jsonValue(out)
}

// joinAs is the whole of joining: find or create the person, find or create
// the agent, mint a token. Shared by join_team and create_team, which differ
// only in how the caller proved they are allowed to be here.
func (h *Handler) joinAs(ctx context.Context, team team_entity.Team, memberName, agentLabel, rawClientKey, clientKind string) (JoinTeamResult, error) {
	args := JoinTeamParams{
		MemberName: memberName,
		AgentLabel: agentLabel,
		ClientKey:  rawClientKey,
		ClientKind: clientKind,
	}

	memberKey := slugKey(args.MemberName, 32)
	if memberKey == "" {
		return JoinTeamResult{}, errors.New("member_name is required, and must contain at least one letter or digit")
	}
	clientKey := truncate(args.ClientKey, 120)
	if clientKey == "" {
		return JoinTeamResult{}, errors.New("client_key is required — it is what lets a restarted agent re-join as itself rather than as a duplicate")
	}
	label := truncate(args.AgentLabel, 80)
	if label == "" {
		label = "agent"
	}

	// ── the member ──────────────────────────────────────────────────────────
	// Created through the same write path as everything else, so a person
	// appearing on the board is an event like any other. The idempotency key
	// is the member key, so two of one person's agents joining at the same
	// moment produce one member and one event.
	member, found, err := h.memberByKey(ctx, nil, team.ID, memberKey)
	if err != nil {
		return JoinTeamResult{}, err
	}
	if !found {
		if _, err := h.commit(ctx, Mutation{
			TeamUUID:       team.ID,
			IdempotencyKey: "member_joined:" + memberKey,
			Kind:           enums.EVENT_KIND_MEMBER_JOINED,
			Structural:     true, // a new lane on the board
			SubjectKey:     memberKey,
			Summary:        fmt.Sprintf("%s joined the team", truncate(args.MemberName, 120)),
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
						Key:         memberKey,
						DisplayName: truncate(args.MemberName, 120),
						Role:        enums.MEMBER_ROLE_MEMBER,
						Status:      enums.RECORD_STATUS_ACTIVE,
						LastSeenAt:  nullTime(tc.Now),
					},
				}, membermod.WithSQLTransaction(tc.Tx))
				return err
			},
		}); err != nil {
			return JoinTeamResult{}, err
		}
		// Re-read rather than trusting the uuid we generated: the commit above
		// may have been a replay of a concurrent caller's insert, in which
		// case the row that exists is theirs, not ours.
		member, found, err = h.memberByKey(ctx, nil, team.ID, memberKey)
		if err != nil {
			return JoinTeamResult{}, err
		}
		if !found {
			return JoinTeamResult{}, errors.New("the member row vanished immediately after being created; retry join_team")
		}
	}

	// ── the token ───────────────────────────────────────────────────────────
	token, hash, err := MintToken()
	if err != nil {
		return JoinTeamResult{}, err
	}

	// A unique key per call. join_team must never replay, because its response
	// carries the token and the stored snapshot deliberately does not.
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
			existing, found, err := h.agentByClientKey(ctx, tc.Tx, member.ID, clientKey)
			if err != nil {
				return err
			}
			if found {
				rejoined = true
				agentKey = existing.Key
				existing.Label = label
				existing.ClientKind = nullString(truncate(args.ClientKind, 60))
				// Rotating rather than reusing: the previous token cannot be
				// handed back — only its hash was kept — so the honest
				// behaviour is to issue a new one and invalidate the old.
				existing.TokenHash = hash
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
					ID:         id,
					TeamUUID:   team.ID,
					MemberUUID: member.ID,
					Key:        agentKey,
					Label:      label,
					ClientKind: nullString(truncate(args.ClientKind, 60)),
					TokenHash:  hash,
					ClientKey:  clientKey,
					Status:     enums.AGENT_STATUS_ACTIVE,
					LastSeenAt: nullTime(tc.Now),
				},
			}, agentmod.WithSQLTransaction(tc.Tx))
			return err
		},
		// See the Snapshot field on Mutation: what is stored is what the
		// caller gets MINUS the token, which is never persisted anywhere but
		// as a hash on the agent row.
		Snapshot: json.RawMessage(`{"ok":true,"note":"agent joined; the response is not replayed because it carries a one-time token","pending":{"instructions":0,"conflicts":0,"reviews":0}}`),
	})
	if err != nil {
		return JoinTeamResult{}, err
	}

	var env Envelope
	if err := json.Unmarshal(response, &env); err != nil {
		return JoinTeamResult{}, err
	}
	env.Key = agentKey
	env.Note = "save the token; it is shown once and cannot be recovered. Call start_session next."
	if rejoined {
		env.Note = "re-joined as the same agent; the previous token for this client_key no longer works. " + env.Note
	}

	return JoinTeamResult{
		Envelope:   env,
		Token:      token,
		TeamSlug:   team.Slug,
		MemberKey:  member.Key,
		AgentKey:   agentKey,
		Rejoined:   rejoined,
		AuthHeader: "Authorization: Bearer " + token,
		TokenNote:  "Send this on every later MCP request as an Authorization: Bearer header. metiche stores only its sha256 and cannot show it to you again.",
	}, nil
}
