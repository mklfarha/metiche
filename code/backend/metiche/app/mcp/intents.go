package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/coordination"
	claimmod "github.com/mklfarha/metiche/backend/core/module/claim"
	claim_types "github.com/mklfarha/metiche/backend/core/module/claim/types"
	claimpathmod "github.com/mklfarha/metiche/backend/core/module/claim_path"
	claim_path_types "github.com/mklfarha/metiche/backend/core/module/claim_path/types"
	intentmod "github.com/mklfarha/metiche/backend/core/module/intent"
	intent_types "github.com/mklfarha/metiche/backend/core/module/intent/types"
	claim_entity "github.com/mklfarha/metiche/backend/entity/claim"
	claim_path_entity "github.com/mklfarha/metiche/backend/entity/claim_path"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	intent_entity "github.com/mklfarha/metiche/backend/entity/intent"
	project_entity "github.com/mklfarha/metiche/backend/entity/project"
	"github.com/mklfarha/metiche/backend/enums"
)

// intents.go is the main surface of metiche: an agent says what it is about
// to do and which files it is taking, and the collision comes back in that
// same response.
//
// THERE IS NO USER-FACING CLAIM VERB, deliberately. declare_intent creates
// the intent, its claim and the normalized claim_path rows in ONE
// transaction, and update_intent absorbs what would otherwise be
// record_activity, release_claim, extend_claim and complete_intent. Agents
// learn one concept, not five, and the one they learn is the one they were
// already going to say in prose.

const (
	// ClaimTTLMinSeconds and ClaimTTLMaxSeconds bound what an agent may ask
	// for. The floor stops a claim that lapses before the agent finishes
	// reading the response; the ceiling is the hard cap below.
	ClaimTTLMinSeconds = 60
	ClaimTTLMaxSeconds = 4 * 60 * 60

	// ClaimHardCap is the ceiling a heartbeat can never push a claim past.
	// It is what stops a forgotten agent holding half the repo until
	// somebody notices tomorrow.
	ClaimHardCap = 4 * time.Hour

	// IntentHorizon is how long a declared intent stays live without being
	// updated. Much longer than a claim on purpose: a claim has to be able to
	// expire without abandoning the plan — an agent thinking for twenty
	// minutes should drop its holds, not its intent.
	IntentHorizon = 12 * time.Hour

	// MaxDeclaredPaths bounds one declaration. Everything about a
	// declaration's cost — the candidate scans, the rows written, the time
	// the team lock is held — is linear in this number, so it is capped
	// rather than trusted. An agent that needs more than this is claiming a
	// subtree and should say so with one glob.
	MaxDeclaredPaths = 32
)

// ─────────────────────────────────────────────
// Registration
// ─────────────────────────────────────────────

// RegisterWorkTools registers the tools in this file on the MCP server:
// declare_intent, update_intent and check_paths.
//
// Annotations are not decoration here. MCP's destructiveHint DEFAULTS TO TRUE
// when a tool omits its annotations, so an unannotated tool advertises itself
// as destructive and a well-behaved client gates every call behind a
// confirmation — which for the tool an agent is supposed to call on every
// loop means it never gets called at all.
func RegisterWorkTools(s *mcp.Server, h *Handler, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}

	var (
		readOnly   = &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
		idempotent = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
		additive   = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
	)

	addTool(s, h, logger, &mcp.Tool{
		Name: "declare_intent",
		Description: "Say what you are ABOUT TO DO and which files you are taking, BEFORE you start. This is the main tool: call it at the top of every piece of work and again whenever your plan changes. " +
			"It creates the intent and claims the paths in one step — there is no separate claim tool. " +
			"Claims never block anybody: overlap is allowed, and if somebody else is already in those files you are told in THIS response, with what to do about it, while they find out on their next call. " +
			"Claim what you will actually edit, not the whole directory: a claim on a subtree collides with everyone and is reported as low-value noise.",
		// Additive, not idempotent: two calls with two different idempotency
		// keys are two real intents. The key makes a RETRY safe, which is a
		// different promise.
		Annotations: additive,
	}, h.DeclareIntent)

	addTool(s, h, logger, &mcp.Tool{
		Name: "update_intent",
		Description: "Update the intent you already declared: mark it active or done, change the summary, add or drop paths, or set the one-line 'what I am doing right now' the board shows. " +
			"This is also how you release files early (drop_paths, or status=done) and how you take new ones mid-flight (add_paths) — added paths are collision-checked exactly like declare_intent. " +
			"Call it whenever what you are doing stops matching what you said you would do.",
		Annotations: idempotent,
	}, h.UpdateIntent)

	addTool(s, h, logger, &mcp.Tool{
		Name: "check_paths",
		Description: "Ask who else is in these files WITHOUT claiming anything. Use it before you explore, or when you are weighing two ways to do something and want to know which one walks into somebody else. " +
			"It writes nothing, commits you to nothing and raises no conflict; when you decide, call declare_intent, which is what actually reserves the files and tells the other agents.",
		Annotations: readOnly,
	}, h.CheckPaths)
}

// ─────────────────────────────────────────────
// Tool: declare_intent
// ─────────────────────────────────────────────

type DeclareIntentParams struct {
	SessionKey string   `json:"session_key" jsonschema:"The session_key start_session gave you."`
	Summary    string   `json:"summary" jsonschema:"One sentence on what you are about to do, written for a teammate: 'add the POST /api/login handler and its token refresh'. Max 280 characters. This is what other agents judge against, so name the thing, not the file."`
	Paths      []string `json:"paths,omitempty" jsonschema:"The repo-relative files you are about to touch. Globs allowed: 'src/api/*.go', 'internal/auth/**'. Claim what you will actually edit - a whole-subtree claim collides with everyone and is capped at low severity, which means nobody is warned about the file you really wanted. Generated and vendored paths are dropped automatically."`
	Mode       string   `json:"mode,omitempty" jsonschema:"What you are doing to those paths: 'read' (just reading), 'write' (editing, the default) or 'structural' (renaming, moving or deleting). Say structural when it applies - it breaks other people's code without any merge conflict to warn them, so it is scored higher than a plain edit."`
	Kind       string   `json:"kind,omitempty" jsonschema:"What kind of work this is: implement, fix, refactor, investigate, test, docs, infra, or hold. Defaults to implement."`

	ExternalRef string `json:"external_ref,omitempty" jsonschema:"The issue or ticket id this is for, if there is one. It is the highest-signal duplicate-work key there is, because it is an exact match - two agents on the same ticket is worth knowing immediately."`
	TTLSeconds  int    `json:"ttl_seconds,omitempty" jsonschema:"How long to hold the paths without a heartbeat, in seconds. Defaults to 900. Claims are advisory and expiring by design; heartbeat extends them, up to a hard 4-hour ceiling."`

	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Pass a key of your own and retrying this exact call returns the exact same answer - including the same conflicts - instead of declaring a second intent. Omit it and every call declares a new one."`
}

// DeclareIntent is the call the whole system is built around.
//
// In ONE transaction, under the team lock: the intent row, its claim, the
// normalized claim_path rows, and then detection against everything else
// live in the project — which is why the answer the agent gets is about the
// world as it is after its own claim landed, rather than the world as it was
// a moment before somebody else's did.
//
// Not structural: a declaration fills in an existing lane on the board rather
// than adding one, so sequence advances and board_revision does not.
func (h *Handler) DeclareIntent(ctx context.Context, _ *mcp.CallToolRequest, args DeclareIntentParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSession(ctx, args.SessionKey)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session
	if err := requireWorkableSession(sess.Status); err != nil {
		return nil, nil, err
	}

	summary := truncate(args.Summary, 280)
	if summary == "" {
		return nil, nil, errors.New(
			"summary is required — one sentence on what you are about to do. It is what other agents' models read to spot a semantic collision, and a claim with no summary is just a lock")
	}
	mode, err := parseClaimMode(args.Mode)
	if err != nil {
		return nil, nil, err
	}
	kind, err := parseIntentKind(args.Kind)
	if err != nil {
		return nil, nil, err
	}
	ttl := clampClaimTTL(args.TTLSeconds)

	// The project read and the whole of normalization happen OUT here, before
	// the lock. Nothing that can be decided without the transaction is
	// allowed to run inside it.
	proj, err := h.loadProjectByUUID(ctx, sess.ProjectUUID)
	if err != nil {
		return nil, nil, err
	}
	paths, ignored, err := normalizeClaimPaths(args.Paths, proj)
	if err != nil {
		return nil, nil, err
	}

	// Pre-minted rather than generated inside Apply: the Mutation has to name
	// its subject before commit builds the event, and a uuid costs nothing if
	// the transaction rolls back.
	intentID, err := uuid.NewV4()
	if err != nil {
		return nil, nil, err
	}
	claimID, err := uuid.NewV4()
	if err != nil {
		return nil, nil, err
	}

	idem := strings.TrimSpace(args.IdempotencyKey)
	if idem == "" {
		idem = "intent_declared:" + intentID.String()
	} else {
		// Namespaced by session so two agents that happen to pick the same
		// key do not replay each other's declaration.
		idem = "intent_declared:" + sess.ID.String() + ":" + idem
	}

	req := &pathDetectionRequest{
		TeamUUID:        who.Team.ID,
		ProjectUUID:     sess.ProjectUUID,
		SessionUUID:     sess.ID,
		AgentUUID:       ag.ID,
		MemberUUID:      who.Member.ID,
		SessionKey:      sess.Key,
		Branch:          sess.Branch.ValueOrZero(),
		MemberName:      who.Member.DisplayName,
		AgentLabel:      ag.Label,
		HotspotPatterns: proj.HotspotPatterns,
		IntentUUID:      uuidPtr(intentID),
		IntentSummary:   summary,
	}
	// The detector reads this back out of the context INSIDE the transaction.
	// It is on the context rather than the Mutation so the detection layer
	// can be installed without editing the shared write path.
	ctx = withPathDetection(ctx, req)

	var intentKey string
	response, err := h.commit(ctx, Mutation{
		TeamUUID:       who.Team.ID,
		IdempotencyKey: idem,
		Kind:           enums.EVENT_KIND_INTENT_DECLARED,
		Structural:     false,
		ProjectUUID:    uuidPtr(sess.ProjectUUID),
		SessionUUID:    uuidPtr(sess.ID),
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(who.Member.ID),
		SubjectKind:    enums.SUBJECT_KIND_INTENT,
		SubjectUUID:    uuidPtr(intentID),
		Summary:        fmt.Sprintf("%s: %s", ag.Label, summary),
		Payload: payload_entity.EventPayload{
			Message:    nullString(summary),
			IntentUUID: uuidPtr(intentID),
			Paths:      patternList(paths),
		},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			intentKey = tc.Key("INT")
			req.IntentKey = intentKey

			if _, err := h.core.Intent().Insert(ctx, intent_types.UpsertRequest{
				Intent: intent_entity.Intent{
					ID:          intentID,
					TeamUUID:    who.Team.ID,
					ProjectUUID: sess.ProjectUUID,
					SessionUUID: sess.ID,
					MemberUUID:  who.Member.ID,
					Key:         intentKey,
					Summary:     summary,
					Kind:        kind,
					Status:      enums.INTENT_STATUS_DECLARED,
					ExternalRef: nullString(truncate(args.ExternalRef, 120)),
					Revision:    1,
					DeclaredAt:  nullTime(tc.Now),
					ExpiresAt:   nullTime(tc.Now.Add(IntentHorizon)),
				},
			}, intentmod.WithSQLTransaction(tc.Tx)); err != nil {
				return retryable(err, "recording the intent")
			}

			if len(paths) > 0 {
				if err := h.insertClaim(ctx, tc, claimInsert{
					ClaimID:     claimID,
					ClaimKey:    tc.Key("CL"),
					TeamUUID:    who.Team.ID,
					ProjectUUID: sess.ProjectUUID,
					SessionUUID: sess.ID,
					MemberUUID:  who.Member.ID,
					IntentUUID:  &intentID,
					Mode:        mode,
					TTLSeconds:  ttl,
					Paths:       paths,
				}, req); err != nil {
					return err
				}
			}

			// The board's "what is this lane doing" pointer. Targeted UPDATE
			// rather than a full-row write, so a concurrent heartbeat's
			// status_line survives.
			if _, err := tc.Tx.ExecContext(ctx,
				"UPDATE `session` SET `current_intent_uuid` = ?, `updated_at` = ? WHERE `id` = ?",
				intentID.String(), tc.Now, sess.ID.String()); err != nil {
				return retryable(err, "pointing the session at the new intent")
			}

			// Written here, inside the transaction, because the bytes commit
			// renders are the bytes it stores for a replay. Anything attached
			// after commit returns would be in the first answer and missing
			// from the retry.
			env.Key = intentKey
			env.Note = declarationNote(intentKey, len(paths), ignored, mode)
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(response)
}

// ─────────────────────────────────────────────
// Tool: update_intent
// ─────────────────────────────────────────────

type UpdateIntentParams struct {
	SessionKey string `json:"session_key" jsonschema:"The session_key start_session gave you."`
	IntentKey  string `json:"intent_key,omitempty" jsonschema:"Which intent, by the key declare_intent returned (INT-12). Omit it to update the one you declared most recently."`

	Status     string `json:"status,omitempty" jsonschema:"Where the work is now: 'active' (you have started), 'done' (finished - this releases your files immediately), 'abandoned' (you are not doing it after all) or 'superseded' (replaced by a different intent). Marking it done the moment you finish is the difference between unblocking a teammate now and in fifteen minutes."`
	Summary    string `json:"summary,omitempty" jsonschema:"A replacement summary, when the plan changed. Max 280 characters. Changing it bumps the intent's revision, which is what lets other agents' models take one fresh look at a plan they already judged."`
	StatusLine string `json:"status_line,omitempty" jsonschema:"What you are doing RIGHT NOW, one line, max 120 characters - 'rewriting the token refresh in auth.go'. This is what a teammate sees on the board."`

	AddPaths  []string `json:"add_paths,omitempty" jsonschema:"Files you have discovered you also need. They are collision-checked exactly like declare_intent, so you find out in this response if somebody is already there."`
	DropPaths []string `json:"drop_paths,omitempty" jsonschema:"Files you are finished with. Release them as soon as you are done rather than waiting for the TTL - a held file nobody is editing is the most annoying kind of false positive."`
	Mode      string   `json:"mode,omitempty" jsonschema:"The mode for add_paths: read, write or structural. Defaults to the mode of the claim you already hold for this intent."`

	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Pass a key of your own and retrying this exact call returns the exact same answer instead of applying the change twice."`
}

// UpdateIntent absorbs four verbs that would otherwise be four tools:
// record_activity, release_claim, extend_claim and complete_intent.
//
// The collapse is not tidiness. Every tool in the surface is a thing an agent
// has to decide between under time pressure, and an agent that picks
// "record_activity" when it meant "complete_intent" leaves its files held.
// One noun, one verb, and the parameters say what changed.
func (h *Handler) UpdateIntent(ctx context.Context, _ *mcp.CallToolRequest, args UpdateIntentParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSession(ctx, args.SessionKey)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session

	status, hasStatus, err := parseIntentStatus(args.Status)
	if err != nil {
		return nil, nil, err
	}
	summary := truncate(args.Summary, 280)
	statusLine := truncate(args.StatusLine, 120)

	intent, err := h.resolveIntent(ctx, who, args.IntentKey)
	if err != nil {
		return nil, nil, err
	}

	proj, err := h.loadProjectByUUID(ctx, sess.ProjectUUID)
	if err != nil {
		return nil, nil, err
	}
	added, ignored, err := normalizeClaimPaths(args.AddPaths, proj)
	if err != nil {
		return nil, nil, err
	}
	dropped, _, err := normalizeClaimPaths(args.DropPaths, proj)
	if err != nil {
		return nil, nil, err
	}
	mode, err := parseClaimMode(args.Mode)
	if err != nil {
		return nil, nil, err
	}
	explicitMode := strings.TrimSpace(args.Mode) != ""

	if !hasStatus && summary == "" && statusLine == "" && len(added) == 0 && len(dropped) == 0 {
		return nil, nil, errors.New(
			"update_intent needs something to change: status, summary, status_line, add_paths or drop_paths")
	}

	// A material edit is one that changes what another agent's model already
	// judged — the wording or the scope. It bumps the revision, which mints a
	// new pair_key and earns the pair exactly one more look. A status_line
	// change is not material, or every agent would re-judge every pair every
	// minute.
	material := summary != "" || len(added) > 0 || len(dropped) > 0

	terminal := hasStatus && (status == enums.INTENT_STATUS_DONE ||
		status == enums.INTENT_STATUS_ABANDONED ||
		status == enums.INTENT_STATUS_SUPERSEDED)

	idem := strings.TrimSpace(args.IdempotencyKey)
	if idem == "" {
		nonce, err := uuid.NewV4()
		if err != nil {
			return nil, nil, err
		}
		idem = "intent_updated:" + nonce.String()
	} else {
		idem = "intent_updated:" + intent.ID.String() + ":" + idem
	}

	req := &pathDetectionRequest{
		TeamUUID:        who.Team.ID,
		ProjectUUID:     sess.ProjectUUID,
		SessionUUID:     sess.ID,
		AgentUUID:       ag.ID,
		MemberUUID:      who.Member.ID,
		SessionKey:      sess.Key,
		Branch:          sess.Branch.ValueOrZero(),
		MemberName:      who.Member.DisplayName,
		AgentLabel:      ag.Label,
		HotspotPatterns: proj.HotspotPatterns,
		IntentUUID:      uuidPtr(intent.ID),
		IntentKey:       intent.Key,
		IntentSummary:   firstNonEmpty(summary, intent.Summary),
	}
	ctx = withPathDetection(ctx, req)

	eventKind := enums.EventKind(enums.EVENT_KIND_INTENT_UPDATED)
	if terminal {
		eventKind = enums.EVENT_KIND_INTENT_ENDED
	}

	response, err := h.commit(ctx, Mutation{
		TeamUUID:       who.Team.ID,
		IdempotencyKey: idem,
		Kind:           eventKind,
		Structural:     false,
		ProjectUUID:    uuidPtr(sess.ProjectUUID),
		SessionUUID:    uuidPtr(sess.ID),
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(who.Member.ID),
		SubjectKind:    enums.SUBJECT_KIND_INTENT,
		SubjectUUID:    uuidPtr(intent.ID),
		SubjectKey:     intent.Key,
		Summary:        fmt.Sprintf("%s updated %s", ag.Label, intent.Key),
		Payload: payload_entity.EventPayload{
			Message:        nullString(firstNonEmpty(statusLine, summary)),
			PreviousStatus: nullString(intent.Status.String()),
			NewStatus:      nullString(intentStatusName(hasStatus, status, intent.Status)),
			IntentUUID:     uuidPtr(intent.ID),
			Paths:          patternList(added),
		},
		Envelope: Envelope{Key: intent.Key},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			// One targeted UPDATE with COALESCE rather than a full-row write:
			// an omitted field must keep its value, not blank the board.
			var statusArg any
			if hasStatus {
				statusArg = int64(status)
			}
			var summaryArg any
			if summary != "" {
				summaryArg = summary
			}
			revBump := 0
			if material {
				revBump = 1
			}
			var endedArg any
			if terminal {
				endedArg = tc.Now
			}
			var startedArg any
			if hasStatus && status == enums.INTENT_STATUS_ACTIVE {
				startedArg = tc.Now
			}
			if _, err := tc.Tx.ExecContext(ctx,
				"UPDATE `intent` SET `status` = COALESCE(?, `status`), `summary` = COALESCE(?, `summary`), "+
					"`revision` = `revision` + ?, `started_at` = COALESCE(`started_at`, ?), "+
					"`ended_at` = COALESCE(?, `ended_at`), `expires_at` = ?, `updated_at` = ? WHERE `id` = ?",
				statusArg, summaryArg, revBump, startedArg, endedArg,
				tc.Now.Add(IntentHorizon), tc.Now, intent.ID.String()); err != nil {
				return retryable(err, "updating the intent")
			}

			if statusLine != "" {
				if _, err := tc.Tx.ExecContext(ctx,
					"UPDATE `session` SET `status_line` = ?, `updated_at` = ? WHERE `id` = ?",
					statusLine, tc.Now, sess.ID.String()); err != nil {
					return retryable(err, "updating the status line")
				}
			}

			var releasedPaths, releasedClaims int64
			if len(dropped) > 0 {
				np, nc, err := releaseIntentPaths(ctx, tc, intent.ID, sess.ID, dropped, tc.Now)
				if err != nil {
					return err
				}
				releasedPaths, releasedClaims = np, nc
			}
			if terminal {
				n, err := releaseIntentClaims(ctx, tc, intent.ID, tc.Now, "intent "+intentStatusName(hasStatus, status, intent.Status))
				if err != nil {
					return err
				}
				releasedClaims += n
				// The lane stops pointing at an intent that is over, so the
				// board does not keep showing finished work as current.
				if _, err := tc.Tx.ExecContext(ctx,
					"UPDATE `session` SET `current_intent_uuid` = NULL, `updated_at` = ? WHERE `id` = ? AND `current_intent_uuid` = ?",
					tc.Now, sess.ID.String(), intent.ID.String()); err != nil {
					return retryable(err, "clearing the session's current intent")
				}
			}

			if len(added) > 0 && !terminal {
				claimID, claimMode, found, err := heldClaimForIntent(ctx, tc, intent.ID)
				if err != nil {
					return err
				}
				useMode := claimMode
				if explicitMode || !found {
					useMode = mode
				}
				if found && useMode == claimMode {
					// Same mode, same intent: the new paths belong on the
					// claim that already exists, so one heartbeat extends
					// them all and one release drops them all.
					if err := h.insertClaimPaths(ctx, tc, claimID, sess.ProjectUUID, sess.ID, who.Member.ID, useMode, added, req); err != nil {
						return err
					}
				} else {
					newID, err := uuid.NewV4()
					if err != nil {
						return err
					}
					if err := h.insertClaim(ctx, tc, claimInsert{
						ClaimID:     newID,
						ClaimKey:    tc.Key("CL"),
						TeamUUID:    who.Team.ID,
						ProjectUUID: sess.ProjectUUID,
						SessionUUID: sess.ID,
						MemberUUID:  who.Member.ID,
						IntentUUID:  &intent.ID,
						Mode:        useMode,
						TTLSeconds:  DefaultClaimTTLSeconds,
						Paths:       added,
					}, req); err != nil {
						return err
					}
				}
			}

			env.Key = intent.Key
			env.Note = updateNote(intent.Key, hasStatus, status, len(added), releasedPaths, releasedClaims, ignored, terminal)
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(response)
}

// ─────────────────────────────────────────────
// Writing claims
// ─────────────────────────────────────────────

type claimInsert struct {
	ClaimID     uuid.UUID
	ClaimKey    string
	TeamUUID    uuid.UUID
	ProjectUUID uuid.UUID
	SessionUUID uuid.UUID
	MemberUUID  uuid.UUID
	IntentUUID  *uuid.UUID
	Mode        coordination.ClaimMode
	TTLSeconds  int
	Paths       []coordination.NormalizedPath
}

// insertClaim writes the claim and its paths, and registers them with the
// detector.
//
// hard_expires_at is set once, here, and nothing ever moves it: a heartbeat
// pushes expires_at out by the TTL but is clamped to this ceiling. That is
// the difference between "claims lapse when an agent stops working" and
// "claims lapse when an agent stops working, unless it crashed mid-loop".
func (h *Handler) insertClaim(ctx context.Context, tc *TxContext, in claimInsert, req *pathDetectionRequest) error {
	ttl := clampClaimTTL(in.TTLSeconds)
	hard := tc.Now.Add(ClaimHardCap)
	expires := tc.Now.Add(time.Duration(ttl) * time.Second)
	if expires.After(hard) {
		expires = hard
	}

	breadth := 0
	for _, p := range in.Paths {
		if p.BreadthScore > breadth {
			breadth = p.BreadthScore
		}
	}

	if _, err := h.core.Claim().Insert(ctx, claim_types.UpsertRequest{
		Claim: claim_entity.Claim{
			ID:            in.ClaimID,
			TeamUUID:      in.TeamUUID,
			ProjectUUID:   in.ProjectUUID,
			SessionUUID:   in.SessionUUID,
			MemberUUID:    in.MemberUUID,
			IntentUUID:    in.IntentUUID,
			Key:           in.ClaimKey,
			Mode:          claimModeEnum(in.Mode),
			Status:        enums.CLAIM_STATUS_HELD,
			TtlSeconds:    int64(ttl),
			ExpiresAt:     expires,
			HardExpiresAt: hard,
			BreadthScore:  int64(breadth),
		},
	}, claimmod.WithSQLTransaction(tc.Tx)); err != nil {
		return retryable(err, "recording the claim")
	}

	return h.writeClaimPaths(ctx, tc, in.ClaimID, in.ProjectUUID, in.SessionUUID, in.MemberUUID,
		in.Mode, expires, in.Paths, req)
}

// insertClaimPaths adds paths to a claim that already exists, inheriting its
// expiry so the denormalized copy stays honest.
func (h *Handler) insertClaimPaths(ctx context.Context, tc *TxContext, claimID, projectUUID, sessionUUID, memberUUID uuid.UUID,
	mode coordination.ClaimMode, paths []coordination.NormalizedPath, req *pathDetectionRequest) error {
	var expires time.Time
	if err := tc.Tx.QueryRowContext(ctx,
		"SELECT `expires_at` FROM `claim` WHERE `id` = ?", claimID.String()).Scan(&expires); err != nil {
		return retryable(err, "reading the claim's expiry")
	}
	return h.writeClaimPaths(ctx, tc, claimID, projectUUID, sessionUUID, memberUUID, mode, expires, paths, req)
}

// writeClaimPaths inserts the rows the detection scan reads, and appends each
// one to the detection request.
//
// claim_path repeats six columns from claim — project, session, member, mode,
// status, expires_at — and that duplication is the single deliberate
// denormalization in the model. It is what makes the overlap scan that runs
// inside EVERY declaration one index range scan with no joins, and the price
// of it is that every writer of these rows, including this one, keeps the
// copy honest.
func (h *Handler) writeClaimPaths(ctx context.Context, tc *TxContext, claimID, projectUUID, sessionUUID, memberUUID uuid.UUID,
	mode coordination.ClaimMode, expires time.Time, paths []coordination.NormalizedPath, req *pathDetectionRequest) error {
	for _, p := range paths {
		id, err := uuid.NewV4()
		if err != nil {
			return err
		}
		if _, err := h.core.ClaimPath().Insert(ctx, claim_path_types.UpsertRequest{
			ClaimPath: claim_path_entity.ClaimPath{
				ID:            id,
				ClaimUUID:     claimID,
				ProjectUUID:   projectUUID,
				SessionUUID:   sessionUUID,
				MemberUUID:    memberUUID,
				Mode:          claimModeEnum(mode),
				Status:        enums.CLAIM_STATUS_HELD,
				ExpiresAt:     expires,
				Pattern:       truncate(p.Pattern, 400),
				PatternNorm:   truncate(p.PatternNorm, 400),
				Kind:          pathKindEnum(p.Kind),
				Prefix:        truncate(p.Prefix, 400),
				SuffixPattern: nullString(truncate(p.SuffixPattern, 200)),
				Depth:         int64(p.Depth),
				Ext:           nullString(p.Ext),
			},
		}, claimpathmod.WithSQLTransaction(tc.Tx)); err != nil {
			return retryable(err, "recording the claimed paths")
		}
		if req != nil {
			// The detector runs after Apply, on this same transaction, and
			// reads exactly what was written here.
			req.Declared = append(req.Declared, pathDeclaration{ClaimUUID: claimID, Mode: mode, Path: p})
		}
	}
	return nil
}

// heldClaimForIntent finds the live claim already attached to an intent, so
// added paths join it instead of fragmenting into a claim per call.
func heldClaimForIntent(ctx context.Context, tc *TxContext, intentUUID uuid.UUID) (uuid.UUID, coordination.ClaimMode, bool, error) {
	var (
		id   string
		mode int64
	)
	err := tc.Tx.QueryRowContext(ctx,
		"SELECT `id`, `mode` FROM `claim` WHERE `intent_uuid` = ? AND `status` = ? "+
			"ORDER BY `created_at` DESC LIMIT 1",
		intentUUID.String(), enums.CLAIM_STATUS_HELD).Scan(&id, &mode)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return uuid.Nil, coordination.ModeWrite, false, nil
	case err != nil:
		return uuid.Nil, coordination.ModeWrite, false, retryable(err, "looking up the intent's claim")
	}
	parsed, err := uuid.FromString(id)
	if err != nil {
		return uuid.Nil, coordination.ModeWrite, false, err
	}
	return parsed, claimModeToCoordination(enums.ClaimMode(mode)), true, nil
}

// releaseIntentPaths drops named paths from an intent's claims and releases
// any claim left holding nothing.
//
// Both statements are bounded by one intent's own rows — a handful — and both
// are index-driven, which is what makes them safe inside the team lock.
func releaseIntentPaths(ctx context.Context, tc *TxContext, intentUUID, sessionUUID uuid.UUID,
	paths []coordination.NormalizedPath, now time.Time) (int64, int64, error) {
	if len(paths) == 0 {
		return 0, 0, nil
	}
	placeholders := make([]string, len(paths))
	args := []any{enums.CLAIM_STATUS_RELEASED, now, sessionUUID.String(), enums.CLAIM_STATUS_HELD}
	for i, p := range paths {
		placeholders[i] = "?"
		args = append(args, p.PatternNorm)
	}
	res, err := tc.Tx.ExecContext(ctx,
		"UPDATE `claim_path` SET `status` = ?, `updated_at` = ? "+
			"WHERE `session_uuid` = ? AND `status` = ? AND `pattern_norm` IN ("+strings.Join(placeholders, ",")+")",
		args...)
	if err != nil {
		return 0, 0, retryable(err, "releasing the dropped paths")
	}
	n, _ := res.RowsAffected()

	// A claim whose every path has gone is a claim nobody holds. Released
	// here rather than left dangling, because the heartbeat would otherwise
	// keep extending a hold on nothing.
	out, err := tc.Tx.ExecContext(ctx,
		"UPDATE `claim` c SET c.`status` = ?, c.`released_at` = ?, c.`release_reason` = ?, c.`updated_at` = ? "+
			"WHERE c.`intent_uuid` = ? AND c.`status` = ? "+
			"AND NOT EXISTS (SELECT 1 FROM `claim_path` cp WHERE cp.`claim_uuid` = c.`id` AND cp.`status` = ?)",
		enums.CLAIM_STATUS_RELEASED, now, "all paths dropped", now,
		intentUUID.String(), enums.CLAIM_STATUS_HELD, enums.CLAIM_STATUS_HELD)
	if err != nil {
		return n, 0, retryable(err, "releasing the emptied claims")
	}
	m, _ := out.RowsAffected()
	return n, m, nil
}

// releaseIntentClaims drops everything an intent was holding, the moment the
// intent is over. Fifteen minutes of TTL is fifteen minutes a teammate spends
// avoiding a file nobody is in.
func releaseIntentClaims(ctx context.Context, tc *TxContext, intentUUID uuid.UUID, now time.Time, reason string) (int64, error) {
	res, err := tc.Tx.ExecContext(ctx,
		"UPDATE `claim` SET `status` = ?, `released_at` = ?, `release_reason` = ?, `updated_at` = ? "+
			"WHERE `intent_uuid` = ? AND `status` = ?",
		enums.CLAIM_STATUS_RELEASED, now, truncate(reason, 200), now,
		intentUUID.String(), enums.CLAIM_STATUS_HELD)
	if err != nil {
		return 0, retryable(err, "releasing the intent's claims")
	}
	n, _ := res.RowsAffected()

	// Mirrored onto the denormalized copy, or the scan keeps finding a claim
	// that no longer exists as far as everything else is concerned.
	if _, err := tc.Tx.ExecContext(ctx,
		"UPDATE `claim_path` cp JOIN `claim` c ON c.`id` = cp.`claim_uuid` "+
			"SET cp.`status` = ?, cp.`updated_at` = ? WHERE c.`intent_uuid` = ? AND cp.`status` = ?",
		enums.CLAIM_STATUS_RELEASED, now, intentUUID.String(), enums.CLAIM_STATUS_HELD); err != nil {
		return n, retryable(err, "releasing the intent's paths")
	}
	return n, nil
}

// ─────────────────────────────────────────────
// Resolving and validating
// ─────────────────────────────────────────────

// intentRef is the little of an intent row these tools need.
type intentRef struct {
	ID      uuid.UUID
	Key     string
	Summary string
	Status  enums.IntentStatus
}

// resolveIntent finds the intent an update is about: the named one, or the
// session's current one.
//
// Scoped to the caller's own session, not just their team. Intent keys are
// short and guessable (INT-12), the endpoint is public, and without the scope
// any agent could mark any teammate's intent done and release their files.
func (h *Handler) resolveIntent(ctx context.Context, who Resolved, key string) (intentRef, error) {
	key = strings.TrimSpace(key)
	var (
		row    intentRef
		id     string
		status int64
		err    error
	)
	if key != "" {
		err = h.core.DB().QueryRowContext(ctx,
			"SELECT `id`, `key`, `summary`, `status` FROM `intent` WHERE `team_uuid` = ? AND `key` = ?",
			who.Team.ID.String(), key).Scan(&id, &row.Key, &row.Summary, &status)
	} else {
		err = h.core.DB().QueryRowContext(ctx,
			"SELECT `id`, `key`, `summary`, `status` FROM `intent` WHERE `session_uuid` = ? "+
				"ORDER BY `created_at` DESC LIMIT 1",
			who.Session.ID.String()).Scan(&id, &row.Key, &row.Summary, &status)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows) && key != "":
		return intentRef{}, fmt.Errorf("no intent with key %q on this team — declare_intent returns the key to use here", key)
	case errors.Is(err, sql.ErrNoRows):
		return intentRef{}, errors.New("you have not declared an intent on this session yet — call declare_intent first")
	case err != nil:
		return intentRef{}, retryable(err, "looking up the intent")
	}
	row.ID, err = uuid.FromString(id)
	if err != nil {
		return intentRef{}, err
	}
	row.Status = enums.IntentStatus(status)

	// The ownership check. Cheap, and the only thing standing between a short
	// guessable key and a stranger releasing somebody's claims.
	var owned int
	if err := h.core.DB().QueryRowContext(ctx,
		"SELECT 1 FROM `intent` WHERE `id` = ? AND `session_uuid` = ? LIMIT 1",
		row.ID.String(), who.Session.ID.String()).Scan(&owned); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return intentRef{}, fmt.Errorf("intent %s belongs to another session", row.Key)
		}
		return intentRef{}, retryable(err, "checking the intent's owner")
	}
	return row, nil
}

// requireWorkableSession refuses to attach work to a session that is over.
// Declaring against an ended session would hold files nothing will ever
// release, because the release happens when the session ends.
func requireWorkableSession(status enums.SessionStatus) error {
	switch status {
	case enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE:
		return nil
	default:
		return fmt.Errorf("this session is already %s — call start_session to begin a new one", status.String())
	}
}

// normalizeClaimPaths turns what the agent sent into rows, dropping what the
// project ignores and rejecting what is malformed.
//
// The two are treated differently ON PURPOSE. An ignored path is not an
// error: an agent that lists twelve paths of which one is generated must not
// have the other eleven rejected, and dropping generated code silently is the
// single most important noise rule in the system — nuzur writes most of the
// Go in this repo, and without it every agent collides with every other agent
// on files nobody hand-edits.
//
// A malformed path IS an error, and a loud one. Silently dropping "/etc/passwd"
// or "../other-repo/x.go" would leave the agent believing it holds something
// it does not, which is worse than a failed call it can fix.
func normalizeClaimPaths(raw []string, proj project_entity.Project) ([]coordination.NormalizedPath, []string, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	if len(raw) > MaxDeclaredPaths {
		return nil, nil, fmt.Errorf(
			"that is %d paths and the limit is %d — claim the directory with one glob (src/api/**) instead of listing every file in it",
			len(raw), MaxDeclaredPaths)
	}

	var (
		out     []coordination.NormalizedPath
		ignored []string
	)
	seen := make(map[string]bool, len(raw))
	for _, p := range raw {
		np, err := coordination.NormalizePath(p, proj.IgnorePatterns, proj.CaseInsensitivePaths)
		if err != nil {
			if errors.Is(err, coordination.ErrPathIgnored) {
				ignored = append(ignored, strings.TrimSpace(p))
				continue
			}
			return nil, nil, fmt.Errorf("%w — paths are repo-relative, like internal/auth/token.go or src/api/**", err)
		}
		// Two spellings of one path are one claim. Without this a caller
		// listing both "src/api" and "src/api/" would hold the same subtree
		// twice and collide with itself on every re-detection.
		if seen[np.PatternNorm] {
			continue
		}
		seen[np.PatternNorm] = true
		out = append(out, np)
	}
	return out, ignored, nil
}

func parseClaimMode(s string) (coordination.ClaimMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "write", "edit":
		return coordination.ModeWrite, nil
	case "read":
		return coordination.ModeRead, nil
	case "structural", "rename", "move", "delete":
		return coordination.ModeStructural, nil
	default:
		return coordination.ModeWrite, fmt.Errorf(
			"mode must be read, write or structural (got %q). Use structural for a rename, move or delete: it breaks other people's code with no merge conflict to warn them", s)
	}
}

func parseIntentKind(s string) (enums.IntentKind, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return enums.INTENT_KIND_IMPLEMENT, nil
	}
	if k := enums.IntentKindFromString(v); k != enums.INTENT_KIND_INVALID {
		return k, nil
	}
	return enums.INTENT_KIND_INVALID, fmt.Errorf(
		"kind must be one of implement, fix, refactor, investigate, test, docs, infra, hold (got %q)", s)
}

// parseIntentStatus reports whether a status was given at all, because "not
// mentioned" and "set it back to declared" are different instructions.
func parseIntentStatus(s string) (enums.IntentStatus, bool, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "":
		return enums.INTENT_STATUS_INVALID, false, nil
	case "complete", "completed", "finished":
		v = "done"
	case "cancelled", "canceled", "dropped":
		v = "abandoned"
	case "replaced":
		v = "superseded"
	case "started", "working", "in_progress":
		v = "active"
	}
	if st := enums.IntentStatusFromString(v); st != enums.INTENT_STATUS_INVALID {
		return st, true, nil
	}
	return enums.INTENT_STATUS_INVALID, false, fmt.Errorf(
		"status must be one of declared, active, done, abandoned, superseded (got %q)", s)
}

func clampClaimTTL(seconds int) int {
	if seconds <= 0 {
		return DefaultClaimTTLSeconds
	}
	if seconds < ClaimTTLMinSeconds {
		return ClaimTTLMinSeconds
	}
	if seconds > ClaimTTLMaxSeconds {
		return ClaimTTLMaxSeconds
	}
	return seconds
}

func patternList(paths []coordination.NormalizedPath) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, p.PatternNorm)
	}
	return out
}

func intentStatusName(has bool, next, current enums.IntentStatus) string {
	if has {
		return next.String()
	}
	return current.String()
}

// declarationNote is the one line of prose the model reads first. It says what
// landed, and — when something was dropped — why, because an agent that
// silently holds fewer paths than it asked for will act as though it holds
// them all.
func declarationNote(key string, claimed int, ignored []string, mode coordination.ClaimMode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s declared", key)
	if claimed > 0 {
		fmt.Fprintf(&b, "; holding %d path(s) for %s", claimed, mode)
	} else {
		b.WriteString("; no paths claimed, so nobody can see which files you are in — pass paths[] next time")
	}
	if len(ignored) > 0 {
		fmt.Fprintf(&b, "; %d ignored (generated or vendored: %s)", len(ignored), strings.Join(ignored, ", "))
	}
	return truncate(b.String(), 400)
}

func updateNote(key string, hasStatus bool, status enums.IntentStatus, added int, droppedPaths, releasedClaims int64, ignored []string, terminal bool) string {
	var parts []string
	if hasStatus {
		parts = append(parts, fmt.Sprintf("%s is now %s", key, status.String()))
	} else {
		parts = append(parts, key+" updated")
	}
	if added > 0 {
		parts = append(parts, fmt.Sprintf("%d path(s) added", added))
	}
	if droppedPaths > 0 {
		parts = append(parts, fmt.Sprintf("%d path(s) released", droppedPaths))
	}
	if terminal && releasedClaims > 0 {
		parts = append(parts, "your files are free again")
	}
	if len(ignored) > 0 {
		parts = append(parts, fmt.Sprintf("%d ignored (generated or vendored)", len(ignored)))
	}
	return truncate(strings.Join(parts, "; "), 400)
}
