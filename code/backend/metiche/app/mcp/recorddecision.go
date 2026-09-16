package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/coordination"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// recorddecision.go is PLAN.md's tool 9, record_decision (docs/DECISIONS.md
// §3.1): the team settled something the rest of the code must obey, and an
// agent writes it down so every other agent's plan can be checked against it.
//
// The server never judges a plan against a decision. It pairs the decision
// with the live plans that touch it — by their claimed files, their wording,
// or every plan when the decision is always-show — and hands each pair to the
// plan's own agent through pending.reviews. That pairing runs in this call's
// Detect hook, under the same team lock as the write.

// The limits of docs/DECISIONS.md §3.0.
const (
	MinDecisionKeyChars       = 3
	MaxDecisionKeyChars       = 60
	MaxDecisionTitleChars     = 140
	MinDecisionStatementChars = 20
	MaxDecisionStatementChars = 400
	MaxDecisionRationaleChars = 2000
	MaxDecisionScopePaths     = 16
	MaxAlwaysShowPerTeam      = 5

	decisionMaxTokens              = 32
	decisionMinSharedTokens        = 2
	decisionNearDuplicateTokens    = 5
	decisionMaxCandidatesPerRecord = 12
	decisionMaxLiveIntentsScanned  = 100
	decisionMaxProjectsScanned     = 8
	decisionMaxScopeClaims         = 200

	reviewMaxPathsScanned     = 16
	reviewDecisionPathLimit   = 200
	reviewTokenCandidateLimit = 10
	reviewMaxInlinePairs      = 3
	reviewInlineChars         = 1200
	reviewMaxDecisionsRead    = 64

	ReviewContextDefaultLimit = 3
	ReviewContextMaxLimit     = 3
	reviewContextCharBudget   = 2200

	judgeRateWindow            = 60 * time.Second
	defaultMaxReviewsPerMinute = 3
	decisionJudgeWindow        = 15 * time.Minute
	judgeMaxAssignments        = 3
	judgeRationaleChars        = 400

	decisionBoardRationaleChars = 600
	decisionEventMessageChars   = 400
)

// RegisterDecisionTools registers record_decision, get_review_context and
// report_judgement.
func RegisterDecisionTools(s *mcp.Server, h *Handler, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}
	var (
		readOnly   = &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
		idempotent = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	)

	// Idempotent and non-destructive: a revoke can be undone by recording the
	// key again.
	addTool(s, h, logger, &mcp.Tool{
		Name: "record_decision",
		Description: "Record a decision the team has settled that the rest of the code must obey — how auth works, an error format, a library choice, who owns a module — so other agents' plans are checked against it. " +
			"Call it after the decision is agreed, never as a proposal, and not for task plans (those are intents). " +
			"Give a short kebab-case key (cited as #key), a statement another agent can check a plan against, and the paths it governs as scope. " +
			"Recording the same key again revises it; supersedes replaces another decision; revoke withdraws one. " +
			"Changing a decision somebody else recorded needs your person's agreement, passed as person_confirmed. " +
			"metiche asks the agents whose live plans touch the decision to judge them against it; this call returns no conflicts.",
		Annotations: idempotent,
	}, h.RecordDecision)

	addTool(s, h, logger, &mcp.Tool{
		Name: "get_review_context",
		Description: "Read the pairs metiche has asked you to judge: a recorded team decision next to your own plan, with why they were paired. " +
			"Call it when pending.reviews is above zero, or when a response's review block did not carry everything you need, then answer each pair with report_judgement. " +
			"Read-only: it changes nothing and can be called again. metiche never judges anything itself — your model does.",
		Annotations: readOnly,
	}, h.GetReviewContext)

	// Idempotent: the key is the judgement and the verdict, like report_back.
	addTool(s, h, logger, &mcp.Tool{
		Name: "report_judgement",
		Description: "Give your verdict on a pair from get_review_context or a review block: would your plan, as written, break the recorded decision? " +
			"'no_conflict' is the usual answer; 'conflict' only when doing the plan would break the statement; 'unsure' when the wording does not settle it. " +
			"Include an honest confidence and a one-line rationale; both are shown on the board. " +
			"A conflict comes back in conflicts[] with what to do, and the decision's author's agent is told. " +
			"Change your plan and update_intent, and you will be asked once more; a no_conflict then settles it. " +
			"Safe to retry: the same verdict on the same pair returns the first answer.",
		Annotations: idempotent,
	}, h.ReportJudgement)
}

type RecordDecisionParams struct {
	SessionKey string   `json:"session_key" jsonschema:"The session_key start_session gave you."`
	TeamSlug   string   `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	Key        string   `json:"key" jsonschema:"The decision's short name, kebab-case, the way a teammate would cite it: 'auth-jwt-cookie' (a leading # is fine). Lowercase letters, digits and dashes, max 60 characters. Recording the same key again revises that decision instead of adding a second one."`
	Title      string   `json:"title,omitempty" jsonschema:"A few words naming what was decided: 'Auth is a JWT in an httpOnly cookie'. Max 140 characters. Required unless revoke is true."`
	Statement  string   `json:"statement,omitempty" jsonschema:"The rule the code must follow, in one or two plain sentences another agent can check its plan against: 'Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage.' Max 400 characters. This is exactly what other agents' models read. Required unless revoke is true."`
	Rationale  string   `json:"rationale,omitempty" jsonschema:"Why, for the people on the board. Never sent to other agents inline. Max 2000 characters. No secrets."`
	Scope      []string `json:"scope,omitempty" jsonschema:"Repo-relative paths or globs the decision governs, written like claims: ['internal/auth/**', 'web/src/api/**']. An agent whose claimed files touch the scope is asked to judge its plan against the decision. Leave empty for a decision about wording rather than files; it is still matched by its words. Max 16."`
	TeamWide   bool     `json:"team_wide,omitempty" jsonschema:"true when the decision holds for every repository on the team, not only this session's project. Default false."`
	AlwaysShow bool     `json:"always_show,omitempty" jsonschema:"true only when your person says this decision is load-bearing enough to check against every plan in the project, whatever its files. At most 5 per team."`
	Supersedes string   `json:"supersedes,omitempty" jsonschema:"The key of a decision this one replaces. The old one is marked superseded and stops being checked."`
	Revoke     bool     `json:"revoke,omitempty" jsonschema:"true to withdraw the decision named by key without replacing it. Only key and rationale (why) are read."`

	PersonConfirmed bool   `json:"person_confirmed,omitempty" jsonschema:"Pass true ONLY when your person explicitly agreed to revise, supersede or revoke a decision somebody else recorded. Never on your own judgement."`
	IdempotencyKey  string `json:"idempotency_key,omitempty" jsonschema:"Pass a key of your own and retrying this exact call returns the exact same answer instead of recording again."`
}

// ─────────────────────────────────────────────
// Validation — pure
// ─────────────────────────────────────────────

type decisionTokenRow struct {
	Token  string
	Weight int
}

// decisionInput is a validated, normalized record_decision: everything decided
// before the team lock is taken.
type decisionInput struct {
	Key             string
	Revoke          bool
	Title           string
	Statement       string
	Rationale       string
	Scope           []coordination.NormalizedPath
	TeamWide        bool
	AlwaysShow      bool
	Supersedes      string
	PersonConfirmed bool
	Tokens          []decisionTokenRow
	Hash            string
}

func hasControlChar(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

// parseRecordDecision validates and normalizes a record_decision. Pure: every
// refusal in §3.1's table is testable without MySQL.
func parseRecordDecision(args RecordDecisionParams, ignore []string, caseInsensitive bool) (decisionInput, error) {
	var in decisionInput

	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args.Key), "#")) == "" {
		return in, errors.New("key is required — a short kebab-case name like 'auth-jwt-cookie', cited as #auth-jwt-cookie")
	}
	if hasControlChar(args.Key) {
		return in, errors.New("key contains a control character")
	}
	key, err := coordination.NormalizeDecisionKey(args.Key)
	if err != nil {
		return in, fmt.Errorf("key must be 3-60 lowercase letters, digits and dashes (got %q)", args.Key)
	}
	in.Key = key
	in.PersonConfirmed = args.PersonConfirmed

	if n := utf8.RuneCountInString(args.Rationale); n > MaxDecisionRationaleChars {
		return in, fmt.Errorf("rationale is %d characters and the limit is %d", n, MaxDecisionRationaleChars)
	}
	// The rationale is shown on the board, so credential-shaped text is masked
	// before it is ever stored.
	in.Rationale = sanitizeNoteText(args.Rationale)

	if args.Revoke {
		if strings.TrimSpace(args.Title) != "" || strings.TrimSpace(args.Statement) != "" || strings.TrimSpace(args.Supersedes) != "" {
			return in, errors.New("revoke withdraws a decision by key; do not send title, statement or supersedes with it")
		}
		in.Revoke = true
		return in, nil
	}

	in.Title = strings.TrimSpace(args.Title)
	switch {
	case in.Title == "":
		return in, errors.New("title is required — a few words naming what was decided")
	case hasControlChar(in.Title):
		return in, errors.New("title contains a control character")
	case utf8.RuneCountInString(in.Title) > MaxDecisionTitleChars:
		return in, fmt.Errorf("title is %d characters and the limit is %d", utf8.RuneCountInString(in.Title), MaxDecisionTitleChars)
	}

	in.Statement = strings.TrimSpace(args.Statement)
	n := utf8.RuneCountInString(in.Statement)
	switch {
	case in.Statement == "":
		return in, errors.New("statement is required — the rule the code must follow, in one or two sentences another agent can check its plan against")
	case hasControlChar(in.Statement):
		return in, errors.New("statement contains a control character")
	case n < MinDecisionStatementChars:
		return in, fmt.Errorf("statement is %d characters; write the rule itself in at least %d, not a label", n, MinDecisionStatementChars)
	case n > MaxDecisionStatementChars:
		return in, fmt.Errorf("statement is %d characters and the limit is %d — it ships inline to other agents; put the reasoning in rationale", n, MaxDecisionStatementChars)
	}

	if len(args.Scope) > MaxDecisionScopePaths {
		return in, fmt.Errorf("scope has %d paths and the limit is %d — name the folders the decision governs, not every file", len(args.Scope), MaxDecisionScopePaths)
	}
	seen := map[string]bool{}
	for _, p := range args.Scope {
		np, err := coordination.NormalizePath(p, ignore, caseInsensitive)
		if err != nil {
			if errors.Is(err, coordination.ErrPathIgnored) {
				continue
			}
			return in, fmt.Errorf("%w — scope paths are repo-relative, like internal/auth/** or web/src/api/**", err)
		}
		if seen[np.PatternNorm] {
			continue
		}
		seen[np.PatternNorm] = true
		np.Pattern = strings.TrimSpace(np.Pattern)
		in.Scope = append(in.Scope, np)
	}

	if strings.TrimSpace(args.Supersedes) != "" {
		sk, err := coordination.NormalizeDecisionKey(args.Supersedes)
		if err != nil {
			return in, fmt.Errorf("supersedes must be 3-60 lowercase letters, digits and dashes (got %q)", args.Supersedes)
		}
		if sk == key {
			return in, errors.New("a decision cannot supersede itself")
		}
		in.Supersedes = sk
	}

	in.TeamWide, in.AlwaysShow = args.TeamWide, args.AlwaysShow
	in.Tokens = decisionTokens(in.Title, in.Statement)
	in.Hash = coordination.DecisionContentHash(in.Title, in.Statement, scopePatterns(in.Scope), in.TeamWide)
	return in, nil
}

// decisionTokens: title tokens weigh 2, statement tokens not in the title 1,
// at most decisionMaxTokens rows.
func decisionTokens(title, statement string) []decisionTokenRow {
	var out []decisionTokenRow
	seen := map[string]bool{}
	for _, t := range coordination.Tokenize(title, decisionMaxTokens) {
		seen[t] = true
		out = append(out, decisionTokenRow{Token: t, Weight: 2})
	}
	for _, t := range coordination.Tokenize(statement, decisionMaxTokens) {
		if len(out) >= decisionMaxTokens {
			break
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, decisionTokenRow{Token: t, Weight: 1})
	}
	return out
}

func scopePatterns(scope []coordination.NormalizedPath) []string {
	out := make([]string, 0, len(scope))
	for _, p := range scope {
		out = append(out, p.PatternNorm)
	}
	return out
}

// ─────────────────────────────────────────────
// Tool: record_decision
// ─────────────────────────────────────────────

// Outcomes of one record_decision.
const (
	decisionOutcomeNew        = "new"
	decisionOutcomeRevised    = "revised"
	decisionOutcomeUpdated    = "updated"
	decisionOutcomeUnchanged  = "unchanged"
	decisionOutcomeReinstated = "reinstated"
	decisionOutcomeRevoked    = "revoked"
)

// decisionRecord travels from the tool through Apply, which fills in what it
// wrote, to Detect, which pairs the decision with live plans.
type decisionRecord struct {
	In          decisionInput
	TeamUUID    uuid.UUID
	ProjectUUID uuid.UUID
	SessionUUID uuid.UUID
	AgentUUID   uuid.UUID
	MemberUUID  uuid.UUID
	AgentLabel  string

	// Filled by Apply.
	DecisionUUID      uuid.UUID
	Key               string
	Revision          int64
	Outcome           string
	Statement         string
	AlwaysShow        bool
	DecisionProject   *uuid.UUID
	SupersededKey     string
	SupersededSettled int
	RevokedSettled    int
	NearKey           string
	NearShared        int
}

// RecordDecision writes a decision, revises, supersedes, revokes or reinstates
// one, and pairs it with the live plans it governs. Structural: the Decisions
// tab changes shape.
func (h *Handler) RecordDecision(ctx context.Context, _ *mcp.CallToolRequest, args RecordDecisionParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSessionOnTeam(ctx, args.SessionKey, args.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session
	if err := requireWorkableSession(sess.Status); err != nil {
		return nil, nil, err
	}
	proj, err := h.loadProjectByUUID(ctx, sess.ProjectUUID)
	if err != nil {
		return nil, nil, err
	}
	in, err := parseRecordDecision(args, proj.IgnorePatterns, proj.CaseInsensitivePaths)
	if err != nil {
		return nil, nil, err
	}

	idem := strings.TrimSpace(args.IdempotencyKey)
	if idem == "" {
		nonce, err := uuid.NewV4()
		if err != nil {
			return nil, nil, err
		}
		idem = "decision_recorded:" + nonce.String()
	} else {
		// Namespaced by session so two agents that pick the same key do not
		// replay each other's decision.
		idem = "decision_recorded:" + sess.ID.String() + ":" + idem
	}

	r := &decisionRecord{
		In:          in,
		TeamUUID:    who.Team.ID,
		ProjectUUID: sess.ProjectUUID,
		SessionUUID: sess.ID,
		AgentUUID:   ag.ID,
		MemberUUID:  who.Member.ID,
		AgentLabel:  ag.Label,
		Key:         in.Key,
	}
	kind := enums.EventKind(enums.EVENT_KIND_DECISION_RECORDED)
	if in.Revoke {
		kind = enums.EVENT_KIND_DECISION_SUPERSEDED
	}

	response, err := h.commit(ctx, Mutation{
		TeamUUID:       who.Team.ID,
		IdempotencyKey: idem,
		Kind:           kind,
		Structural:     true,
		ProjectUUID:    uuidPtr(sess.ProjectUUID),
		SessionUUID:    uuidPtr(sess.ID),
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(who.Member.ID),
		SubjectKind:    enums.SUBJECT_KIND_DECISION,
		SubjectKey:     in.Key,
		Summary:        fmt.Sprintf("%s recorded %s", ag.Label, in.Key),
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			return h.applyRecordDecision(ctx, tc, env, r)
		},
		// Its own hook, not the handler-wide path detector: record_decision
		// claims no paths, and the pairing has to see the rows Apply wrote.
		Detect: func(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
			return h.detectRecordDecision(ctx, tc, m, r)
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(response)
}

// decisionRow is one decision as Apply reads it.
type decisionRow struct {
	ID           uuid.UUID
	Key          string
	Title        string
	Statement    string
	Rationale    sql.NullString
	Status       enums.DecisionStatus
	AlwaysShow   bool
	ProjectUUID  sql.NullString
	SupersededBy sql.NullString
	DecidedBy    sql.NullString
	RecordedBy   sql.NullString
	Revision     int64
	UpdatedAt    time.Time
}

func loadDecisionByKey(ctx context.Context, q queryer, teamUUID uuid.UUID, key string) (decisionRow, bool, error) {
	var (
		d      decisionRow
		id     string
		status int64
	)
	err := q.QueryRowContext(ctx,
		"SELECT `id`, `key`, `title`, `statement`, `rationale`, `status`, `always_show`, `project_uuid`, `superseded_by_uuid`, "+
			"`decided_by_member_uuid`, `recorded_by_session_uuid`, `revision`, `updated_at` FROM `decision` WHERE `team_uuid` = ? AND `key` = ?",
		teamUUID.String(), key).Scan(&id, &d.Key, &d.Title, &d.Statement, &d.Rationale, &status, &d.AlwaysShow, &d.ProjectUUID,
		&d.SupersededBy, &d.DecidedBy, &d.RecordedBy, &d.Revision, &d.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return d, false, nil
	case err != nil:
		return d, false, retryable(err, "looking up the decision")
	}
	d.ID, err = uuid.FromString(id)
	if err != nil {
		return d, false, err
	}
	d.Status = enums.DecisionStatus(status)
	return d, true, nil
}

func (h *Handler) applyRecordDecision(ctx context.Context, tc *TxContext, env *Envelope, r *decisionRecord) error {
	var status int64
	if err := tc.Tx.QueryRowContext(ctx, "SELECT `status` FROM `session` WHERE `id` = ?", r.SessionUUID.String()).Scan(&status); err != nil {
		return retryable(err, "re-reading the session")
	}
	if err := requireWorkableSession(enums.SessionStatus(status)); err != nil {
		return err
	}
	in := r.In
	env.Key = in.Key
	if !in.TeamWide {
		p := r.ProjectUUID
		r.DecisionProject = &p
	}
	actor := eventActor{ProjectUUID: r.ProjectUUID, SessionUUID: r.SessionUUID, AgentUUID: r.AgentUUID, MemberUUID: r.MemberUUID}
	changed := DecisionRelease{TeamUUID: r.TeamUUID, SessionUUID: r.SessionUUID, Kind: DecisionReleaseDecisionChanged, At: tc.Now}

	row, found, err := loadDecisionByKey(ctx, tc.Tx, r.TeamUUID, in.Key)
	if err != nil {
		return err
	}

	switch {
	case !found && in.Revoke:
		return fmt.Errorf("no decision %s on this team to revoke", in.Key)

	case !found && in.Supersedes != "":
		target, ok, err := loadDecisionByKey(ctx, tc.Tx, r.TeamUUID, in.Supersedes)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no decision %s on this team to supersede — check the key on the Decisions tab, or record without supersedes", in.Supersedes)
		}
		if target.Status != enums.DECISION_STATUS_ACCEPTED {
			return fmt.Errorf("%s is already %s; only an accepted decision can be superseded", target.Key, target.Status.String())
		}
		takeOver, err := h.decisionPermission(ctx, tc, r, target)
		if err != nil {
			return err
		}
		if in.AlwaysShow {
			if err := alwaysShowCap(ctx, tc, r.TeamUUID, target.ID); err != nil {
				return err
			}
		}
		if err := r.findNearDuplicate(ctx, tc, target.ID); err != nil {
			return err
		}
		if err := r.insertDecision(ctx, tc, &target.ID); err != nil {
			return err
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"UPDATE `decision` SET `status` = ?, `superseded_by_uuid` = ?, `recorded_by_session_uuid` = ?, "+
				"`decided_by_member_uuid` = COALESCE(?, `decided_by_member_uuid`), `updated_at` = ? WHERE `id` = ?",
			int64(enums.DECISION_STATUS_SUPERSEDED), r.DecisionUUID.String(), r.SessionUUID.String(),
			takeOverArg(takeOver, r.MemberUUID), tc.Now, target.ID.String()); err != nil {
			return retryable(err, "superseding the old decision")
		}
		n, err := h.settleConflictsOnDecision(ctx, tc, changed, target.Key, actor)
		if err != nil {
			return err
		}
		r.SupersededKey, r.SupersededSettled = target.Key, n
		r.Outcome = decisionOutcomeNew
		return nil

	case !found:
		if in.AlwaysShow {
			if err := alwaysShowCap(ctx, tc, r.TeamUUID, uuid.Nil); err != nil {
				return err
			}
		}
		if err := r.findNearDuplicate(ctx, tc, uuid.Nil); err != nil {
			return err
		}
		if err := r.insertDecision(ctx, tc, nil); err != nil {
			return err
		}
		r.Outcome = decisionOutcomeNew
		return nil

	case row.Status == enums.DECISION_STATUS_SUPERSEDED:
		successor := "a newer decision"
		if row.SupersededBy.Valid {
			var k string
			if err := tc.Tx.QueryRowContext(ctx, "SELECT `key` FROM `decision` WHERE `id` = ?", row.SupersededBy.String).Scan(&k); err == nil {
				successor = k
			}
		}
		return fmt.Errorf("%s was superseded by %s at %s; record a new key or revise %s",
			row.Key, successor, row.UpdatedAt.UTC().Format("2006-01-02 15:04 UTC"), successor)

	case in.Supersedes != "":
		return fmt.Errorf("supersedes names the decision this new key replaces; %s already exists — revise it, or pick a new key", row.Key)

	case row.Status == enums.DECISION_STATUS_REVOKED && in.Revoke:
		return fmt.Errorf("%s is already revoked", row.Key)
	}

	r.DecisionUUID, r.Key = row.ID, row.Key

	switch {
	case in.Revoke:
		takeOver, err := h.decisionPermission(ctx, tc, r, row)
		if err != nil {
			return err
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"UPDATE `decision` SET `status` = ?, `recorded_by_session_uuid` = ?, `decided_by_member_uuid` = COALESCE(?, `decided_by_member_uuid`), "+
				"`rationale` = COALESCE(?, `rationale`), `updated_at` = ? WHERE `id` = ?",
			int64(enums.DECISION_STATUS_REVOKED), r.SessionUUID.String(), takeOverArg(takeOver, r.MemberUUID),
			nullIfEmpty(in.Rationale), tc.Now, row.ID.String()); err != nil {
			return retryable(err, "revoking the decision")
		}
		n, err := h.settleConflictsOnDecision(ctx, tc, changed, row.Key, actor)
		if err != nil {
			return err
		}
		r.RevokedSettled, r.Revision, r.Statement = n, row.Revision, row.Statement
		r.Outcome = decisionOutcomeRevoked
		return nil

	case row.Status == enums.DECISION_STATUS_ACCEPTED:
		scope, err := loadDecisionScope(ctx, tc.Tx, row.ID)
		if err != nil {
			return err
		}
		sameContent := coordination.DecisionContentHash(row.Title, row.Statement, scope, !row.ProjectUUID.Valid) == in.Hash
		if sameContent {
			r.Revision, r.Statement, r.AlwaysShow = row.Revision, row.Statement, row.AlwaysShow
			if row.AlwaysShow == in.AlwaysShow && (in.Rationale == "" || in.Rationale == row.Rationale.String) {
				r.Outcome = decisionOutcomeUnchanged
				return nil
			}
			takeOver, err := h.decisionPermission(ctx, tc, r, row)
			if err != nil {
				return err
			}
			if in.AlwaysShow && !row.AlwaysShow {
				if err := alwaysShowCap(ctx, tc, r.TeamUUID, row.ID); err != nil {
					return err
				}
			}
			// Cosmetic for judging: no revision bump, so no plan is asked again.
			if _, err := tc.Tx.ExecContext(ctx,
				"UPDATE `decision` SET `always_show` = ?, `rationale` = COALESCE(?, `rationale`), `recorded_by_session_uuid` = ?, "+
					"`decided_by_member_uuid` = COALESCE(?, `decided_by_member_uuid`), `updated_at` = ? WHERE `id` = ?",
				in.AlwaysShow, nullIfEmpty(in.Rationale), r.SessionUUID.String(), takeOverArg(takeOver, r.MemberUUID),
				tc.Now, row.ID.String()); err != nil {
				return retryable(err, "updating the decision")
			}
			r.AlwaysShow = in.AlwaysShow
			r.Outcome = decisionOutcomeUpdated
			return nil
		}
		return r.rewriteDecision(ctx, h, tc, row, decisionOutcomeRevised)

	case row.Status == enums.DECISION_STATUS_REVOKED:
		return r.rewriteDecision(ctx, h, tc, row, decisionOutcomeReinstated)
	}
	return fmt.Errorf("%s is %s and cannot be recorded over", row.Key, row.Status.String())
}

// rewriteDecision is a revision or a reinstatement: new wording and scope, the
// revision bumped, paths and tokens rewritten.
func (r *decisionRecord) rewriteDecision(ctx context.Context, h *Handler, tc *TxContext, row decisionRow, outcome string) error {
	in := r.In
	takeOver, err := h.decisionPermission(ctx, tc, r, row)
	if err != nil {
		return err
	}
	if in.AlwaysShow && (!row.AlwaysShow || row.Status != enums.DECISION_STATUS_ACCEPTED) {
		if err := alwaysShowCap(ctx, tc, r.TeamUUID, row.ID); err != nil {
			return err
		}
	}
	var decidedAt any
	if outcome == decisionOutcomeReinstated {
		decidedAt = tc.Now
	}
	if _, err := tc.Tx.ExecContext(ctx,
		"UPDATE `decision` SET `status` = ?, `title` = ?, `statement` = ?, `rationale` = COALESCE(?, `rationale`), `always_show` = ?, "+
			"`project_uuid` = ?, `revision` = `revision` + 1, `decided_at` = COALESCE(?, `decided_at`), `recorded_by_session_uuid` = ?, "+
			"`decided_by_member_uuid` = COALESCE(?, `decided_by_member_uuid`), `updated_at` = ? WHERE `id` = ?",
		int64(enums.DECISION_STATUS_ACCEPTED), in.Title, in.Statement, nullIfEmpty(in.Rationale), in.AlwaysShow,
		uuidArg(r.DecisionProject), decidedAt, r.SessionUUID.String(), takeOverArg(takeOver, r.MemberUUID),
		tc.Now, row.ID.String()); err != nil {
		return retryable(err, "revising the decision")
	}
	r.DecisionUUID, r.Key = row.ID, row.Key
	r.Revision, r.Statement, r.AlwaysShow = row.Revision+1, in.Statement, in.AlwaysShow
	if err := r.writeScope(ctx, tc, true); err != nil {
		return err
	}
	// Pairs against the old wording stay pending: report_judgement refuses one
	// as stale, naming the new revision (§3.3), and the new wording earns every
	// live plan a fresh pair in Detect. Revisions close nothing (§4.5).
	r.Outcome = outcome
	return nil
}

func (r *decisionRecord) insertDecision(ctx context.Context, tc *TxContext, supersedes *uuid.UUID) error {
	in := r.In
	id, err := uuid.NewV4()
	if err != nil {
		return err
	}
	if _, err := tc.Tx.ExecContext(ctx,
		"INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`rationale`,`status`,`always_show`,"+
			"`supersedes_uuid`,`decided_by_member_uuid`,`decided_at`,`revision`,`created_at`,`updated_at`,`recorded_by_session_uuid`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?)",
		id.String(), r.TeamUUID.String(), uuidArg(r.DecisionProject), in.Key, in.Title, in.Statement, nullIfEmpty(in.Rationale),
		int64(enums.DECISION_STATUS_ACCEPTED), in.AlwaysShow, uuidArg(supersedes), r.MemberUUID.String(), tc.Now,
		tc.Now, tc.Now, r.SessionUUID.String()); err != nil {
		return retryable(err, "recording the decision")
	}
	r.DecisionUUID, r.Key, r.Revision, r.Statement, r.AlwaysShow = id, in.Key, 1, in.Statement, in.AlwaysShow
	return r.writeScope(ctx, tc, false)
}

// writeScope writes the decision's paths and tokens, replacing any it had.
// Both are bounded by §3.0's limits: 16 paths, 32 tokens.
func (r *decisionRecord) writeScope(ctx context.Context, tc *TxContext, replace bool) error {
	in := r.In
	id := r.DecisionUUID.String()
	if replace {
		if _, err := tc.Tx.ExecContext(ctx, "DELETE FROM `decision_path` WHERE `decision_uuid` = ?", id); err != nil {
			return retryable(err, "clearing the decision's scope")
		}
		if _, err := tc.Tx.ExecContext(ctx, "DELETE FROM `decision_token` WHERE `decision_uuid` = ?", id); err != nil {
			return retryable(err, "clearing the decision's words")
		}
	}
	project := uuidArg(r.DecisionProject)
	if len(in.Scope) > 0 {
		var (
			ph   []string
			args []any
		)
		for _, p := range in.Scope {
			pid, err := uuid.NewV4()
			if err != nil {
				return err
			}
			ph = append(ph, "(?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, pid.String(), id, r.TeamUUID.String(), project, truncate(p.Pattern, 400), truncate(p.PatternNorm, 400),
				int64(pathKindEnum(p.Kind)), truncate(coordination.PathPrefixToStored(p.Prefix), 400), p.Depth, tc.Now, tc.Now)
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"INSERT INTO `decision_path` (`id`,`decision_uuid`,`team_uuid`,`project_uuid`,`pattern`,`pattern_norm`,`kind`,`prefix`,`depth`,`created_at`,`updated_at`) VALUES "+
				strings.Join(ph, ","), args...); err != nil {
			return retryable(err, "recording the decision's scope")
		}
	}
	if len(in.Tokens) > 0 {
		var (
			ph   []string
			args []any
		)
		for _, t := range in.Tokens {
			tid, err := uuid.NewV4()
			if err != nil {
				return err
			}
			ph = append(ph, "(?,?,?,?,?,?,?,?)")
			args = append(args, tid.String(), id, r.TeamUUID.String(), project, t.Token, t.Weight, tc.Now, tc.Now)
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"INSERT INTO `decision_token` (`id`,`decision_uuid`,`team_uuid`,`project_uuid`,`token`,`weight`,`created_at`,`updated_at`) VALUES "+
				strings.Join(ph, ","), args...); err != nil {
			return retryable(err, "recording the decision's words")
		}
	}
	return nil
}

// loadDecisionScope reads a decision's normalized scope. The
// decision_has_paths foreign key's index, capped.
func loadDecisionScope(ctx context.Context, q queryer, decisionUUID uuid.UUID) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT `pattern_norm` FROM `decision_path` WHERE `decision_uuid` = ? ORDER BY `pattern_norm` LIMIT ?",
		decisionUUID.String(), MaxDecisionScopePaths)
	if err != nil {
		return nil, retryable(err, "reading the decision's scope")
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, retryable(err, "reading the decision's scope")
		}
		out = append(out, p)
	}
	return out, retryable(rows.Err(), "reading the decision's scope")
}

// decisionPermission is §3.1's rule: changing a decision another member
// recorded needs person_confirmed. It reports whether the decision now
// belongs to the caller's member.
func (h *Handler) decisionPermission(ctx context.Context, tc *TxContext, r *decisionRecord, row decisionRow) (bool, error) {
	if row.DecidedBy.Valid && row.DecidedBy.String == r.MemberUUID.String() {
		return false, nil
	}
	if r.In.PersonConfirmed {
		return true, nil
	}
	name := "another member"
	if row.DecidedBy.Valid {
		var n string
		if err := tc.Tx.QueryRowContext(ctx, "SELECT `display_name` FROM `member` WHERE `id` = ?", row.DecidedBy.String).Scan(&n); err == nil && strings.TrimSpace(n) != "" {
			name = n
		}
	}
	live := ""
	if row.DecidedBy.Valid {
		key, err := deciderLiveSessionKey(ctx, tc.Tx, r.TeamUUID, row.DecidedBy.String, row.RecordedBy.String)
		if err != nil {
			return false, err
		}
		live = key
	}
	if live != "" {
		return false, fmt.Errorf("not_permitted: %s was recorded by %s. Changing it changes what %s agreed to: settle it with %s's agent (%s is live) or ask your person, then call again with person_confirmed: true.",
			row.Key, name, name, name, live)
	}
	return false, fmt.Errorf("not_permitted: %s was recorded by %s. Changing it changes what %s agreed to: ask your person, then call again with person_confirmed: true.",
		row.Key, name, name)
}

// deciderLiveSessionKey is the decider's session to settle with: the recorder
// session while it is live or stale and the decider's, otherwise the decider's
// most recently heartbeating live or stale session on the team.
func deciderLiveSessionKey(ctx context.Context, q queryer, teamUUID uuid.UUID, memberUUID, recorderSession string) (string, error) {
	var key string
	if recorderSession != "" {
		err := q.QueryRowContext(ctx,
			"SELECT `key` FROM `session` WHERE `id` = ? AND `member_uuid` = ? AND `status` IN (?, ?)",
			recorderSession, memberUUID, int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE)).Scan(&key)
		switch {
		case err == nil:
			return key, nil
		case !errors.Is(err, sql.ErrNoRows):
			return "", retryable(err, "looking up the decider's session")
		}
	}
	err := q.QueryRowContext(ctx,
		"SELECT `key` FROM `session` WHERE `team_uuid` = ? AND `status` IN (?, ?) AND `member_uuid` = ? "+
			"ORDER BY `last_heartbeat_at` DESC LIMIT 1",
		teamUUID.String(), int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE), memberUUID).Scan(&key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", retryable(err, "looking up the decider's session")
	}
	return key, nil
}

func takeOverArg(takeOver bool, member uuid.UUID) any {
	if takeOver {
		return member.String()
	}
	return nil
}

func uuidArg(id *uuid.UUID) any {
	if id == nil || id.IsNil() {
		return nil
	}
	return id.String()
}

// alwaysShowCap refuses a sixth accepted always-show decision on the team.
// idx_decision_always_show, capped.
func alwaysShowCap(ctx context.Context, tc *TxContext, teamUUID, exclude uuid.UUID) error {
	rows, err := tc.Tx.QueryContext(ctx,
		"SELECT `key` FROM `decision` WHERE `team_uuid` = ? AND `status` = ? AND `always_show` = 1 AND `id` <> ? ORDER BY `key` LIMIT ?",
		teamUUID.String(), int64(enums.DECISION_STATUS_ACCEPTED), exclude.String(), MaxAlwaysShowPerTeam)
	if err != nil {
		return retryable(err, "counting the team's always-show decisions")
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			_ = rows.Close()
			return retryable(err, "counting the team's always-show decisions")
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return retryable(err, "counting the team's always-show decisions")
	}
	_ = rows.Close()
	if len(keys)+1 > MaxAlwaysShowPerTeam {
		return fmt.Errorf("the team already has %d always-show decisions (%s); an always-show decision is checked against every plan, so turn one off before adding another",
			MaxAlwaysShowPerTeam, strings.Join(keys, ", "))
	}
	return nil
}

// findNearDuplicate looks for an accepted decision sharing at least
// decisionNearDuplicateTokens words with this one. idx_decision_token_lookup.
func (r *decisionRecord) findNearDuplicate(ctx context.Context, tc *TxContext, exclude uuid.UUID) error {
	if len(r.In.Tokens) < decisionNearDuplicateTokens {
		return nil
	}
	args := []any{r.TeamUUID.String()}
	for _, t := range r.In.Tokens {
		args = append(args, t.Token)
	}
	args = append(args, int64(enums.DECISION_STATUS_ACCEPTED), exclude.String(), decisionNearDuplicateTokens)
	var (
		key    string
		shared int
	)
	err := tc.Tx.QueryRowContext(ctx,
		"SELECT d.`key`, COUNT(DISTINCT dt.`token`) AS shared FROM `decision_token` dt JOIN `decision` d ON d.`id` = dt.`decision_uuid` "+
			"WHERE dt.`team_uuid` = ? AND dt.`token` IN ("+placeholders(len(r.In.Tokens))+") AND d.`status` = ? AND d.`id` <> ? "+
			"GROUP BY d.`id`, d.`key` HAVING shared >= ? ORDER BY shared DESC, d.`key` LIMIT 1", args...).Scan(&key, &shared)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return retryable(err, "looking for a near-identical decision")
	}
	r.NearKey, r.NearShared = key, shared
	return nil
}

// settleConflictsOnDecision settles the open conflicts on a decision that was
// just superseded or revoked, and reports how many closed.
func (h *Handler) settleConflictsOnDecision(ctx context.Context, tc *TxContext, rel DecisionRelease, key string, actor eventActor) (int, error) {
	ids, err := openDecisionConflictsOfDecision(ctx, tc.Tx, rel.TeamUUID, key)
	if err != nil {
		return 0, err
	}
	settled, err := h.settleDecisionConflicts(ctx, tc, rel, ids, actor)
	return len(settled), err
}

// ─────────────────────────────────────────────
// Detect: pairing the decision with live plans
// ─────────────────────────────────────────────

func (h *Handler) detectRecordDecision(ctx context.Context, tc *TxContext, m *Mutation, r *decisionRecord) ([]ConflictNotice, error) {
	id := r.DecisionUUID
	m.SubjectUUID = &id
	m.SubjectKey = truncate(r.Key, 64)

	pairs := 0
	switch r.Outcome {
	case decisionOutcomeNew, decisionOutcomeRevised, decisionOutcomeReinstated:
		n, err := h.pairDecisionWithLivePlans(ctx, tc, r)
		if err != nil {
			return nil, err
		}
		pairs = n
	}
	m.Envelope.Note = recordDecisionNote(r, pairs)
	m.Summary, m.Payload = recordDecisionEvent(r)
	return nil, nil
}

// recordDecisionNote is §3.1's note table, exactly.
func recordDecisionNote(r *decisionRecord, pairs int) string {
	var s string
	switch r.Outcome {
	case decisionOutcomeNew:
		if pairs > 0 {
			s = fmt.Sprintf("%s recorded (revision 1, %d scope path(s)); %d live plan(s) will be checked against it", r.Key, len(r.In.Scope), pairs)
		} else {
			s = fmt.Sprintf("%s recorded (revision 1, %d scope path(s)); no live plan touches it yet", r.Key, len(r.In.Scope))
		}
	case decisionOutcomeRevised:
		s = fmt.Sprintf("%s revised to revision %d; %d live plan(s) will be checked against the new wording", r.Key, r.Revision, pairs)
	case decisionOutcomeUpdated:
		s = fmt.Sprintf("%s updated (revision %d unchanged: rationale or always_show only)", r.Key, r.Revision)
	case decisionOutcomeUnchanged:
		s = fmt.Sprintf("%s: already recorded with this wording and scope (revision %d); nothing changed", r.Key, r.Revision)
	case decisionOutcomeReinstated:
		s = fmt.Sprintf("%s reinstated as revision %d; %d live plan(s) will be checked against it", r.Key, r.Revision, pairs)
	case decisionOutcomeRevoked:
		s = fmt.Sprintf("%s revoked; %d conflict(s) on it settled", r.Key, r.RevokedSettled)
	}
	if r.SupersededKey != "" {
		s += fmt.Sprintf("; superseded %s, %d conflict(s) on it settled", r.SupersededKey, r.SupersededSettled)
	}
	if r.NearKey != "" {
		s += fmt.Sprintf("; looks like %s (%d shared words): if it is the same decision, revise that key or pass supersedes", r.NearKey, r.NearShared)
	}
	return s
}

// recordDecisionEvent is §3.1's event table.
func recordDecisionEvent(r *decisionRecord) (string, payload_entity.EventPayload) {
	label := r.AgentLabel
	p := payload_entity.EventPayload{Message: nullString(r.Statement)}
	var summary string
	switch r.Outcome {
	case decisionOutcomeNew:
		p.Paths = scopePatterns(r.In.Scope)
		if r.SupersededKey != "" {
			summary = fmt.Sprintf("%s recorded %s, superseding %s", label, r.Key, r.SupersededKey)
			p.Detail = nullString("r1 supersedes " + r.SupersededKey)
		} else {
			summary = fmt.Sprintf("%s recorded %s", label, r.Key)
			p.Detail = nullString("r1")
		}
	case decisionOutcomeRevised:
		summary = fmt.Sprintf("%s revised %s (r%d)", label, r.Key, r.Revision)
		p.Detail = nullString(fmt.Sprintf("r%d revised", r.Revision))
		p.Paths = scopePatterns(r.In.Scope)
	case decisionOutcomeUpdated:
		summary = fmt.Sprintf("%s updated %s", label, r.Key)
		p.Detail = nullString(fmt.Sprintf("r%d", r.Revision))
	case decisionOutcomeUnchanged:
		summary = fmt.Sprintf("%s recorded %s again (unchanged)", label, r.Key)
		p.Detail = nullString(fmt.Sprintf("r%d", r.Revision))
	case decisionOutcomeReinstated:
		summary = fmt.Sprintf("%s reinstated %s (r%d)", label, r.Key, r.Revision)
		p.PreviousStatus = nullString(enums.DecisionStatus(enums.DECISION_STATUS_REVOKED).String())
		p.NewStatus = nullString(enums.DecisionStatus(enums.DECISION_STATUS_ACCEPTED).String())
		p.Detail = nullString(fmt.Sprintf("r%d", r.Revision))
		p.Paths = scopePatterns(r.In.Scope)
	case decisionOutcomeRevoked:
		summary = fmt.Sprintf("%s revoked %s", label, r.Key)
		p.PreviousStatus = nullString(enums.DecisionStatus(enums.DECISION_STATUS_ACCEPTED).String())
		p.NewStatus = nullString(enums.DecisionStatus(enums.DECISION_STATUS_REVOKED).String())
		p.Message = nullString(clip(sanitizeNoteText(r.In.Rationale), decisionEventMessageChars))
	}
	return summary, p
}

// liveIntent is one plan that can be paired: declared or active, unexpired,
// its session live or stale.
type liveIntent struct {
	ID       string
	Key      string
	Summary  string
	Revision int64
	Session  string
}

// loadLiveIntents reads live plans matching where. For a project,
// idx_intent_live (project_uuid, status, expires_at); for ids, the primary
// key. Always capped.
func loadLiveIntents(ctx context.Context, q queryer, where string, whereArgs []any, excludeSession uuid.UUID, now time.Time, limit int) ([]liveIntent, error) {
	args := append([]any{}, whereArgs...)
	args = append(args, int64(enums.INTENT_STATUS_DECLARED), int64(enums.INTENT_STATUS_ACTIVE), now,
		int64(enums.SESSION_STATUS_LIVE), int64(enums.SESSION_STATUS_STALE), excludeSession.String(), limit)
	rows, err := q.QueryContext(ctx,
		"SELECT i.`id`, i.`key`, i.`summary`, i.`revision`, i.`session_uuid` FROM `intent` i "+
			"JOIN `session` s ON s.`id` = i.`session_uuid` "+
			"WHERE "+where+" AND i.`status` IN (?, ?) AND i.`expires_at` > ? AND s.`status` IN (?, ?) AND i.`session_uuid` <> ? "+
			"ORDER BY i.`declared_at` DESC, i.`key` LIMIT ?", args...)
	if err != nil {
		return nil, retryable(err, "reading the live plans")
	}
	defer func() { _ = rows.Close() }()
	var out []liveIntent
	for rows.Next() {
		var li liveIntent
		if err := rows.Scan(&li.ID, &li.Key, &li.Summary, &li.Revision, &li.Session); err != nil {
			return nil, retryable(err, "reading the live plans")
		}
		out = append(out, li)
	}
	return out, retryable(rows.Err(), "reading the live plans")
}

// pairDecisionWithLivePlans is §3.1's Detect: collect the live plans the
// decision touches, drop the ones that must not be asked, rank, and write one
// judgement per pair — assigned to the plan's session under its per-minute
// cap, unassigned otherwise. It returns how many pairs it wrote.
func (h *Handler) pairDecisionWithLivePlans(ctx context.Context, tc *TxContext, r *decisionRecord) (int, error) {
	q := tc.Tx
	var projects []string
	if r.DecisionProject != nil {
		projects = []string{r.DecisionProject.String()}
	} else {
		rows, err := q.QueryContext(ctx,
			"SELECT `id` FROM `project` WHERE `team_uuid` = ? AND `status` = ? ORDER BY `key` LIMIT ?",
			r.TeamUUID.String(), int64(enums.RECORD_STATUS_ACTIVE), decisionMaxProjectsScanned)
		if err != nil {
			return 0, retryable(err, "reading the team's projects")
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return 0, retryable(err, "reading the team's projects")
			}
			projects = append(projects, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, retryable(err, "reading the team's projects")
		}
		_ = rows.Close()
	}

	intents := map[string]liveIntent{}
	scopeClaims := map[string]bool{}
	for _, project := range projects {
		projectID, err := uuid.FromString(project)
		if err != nil {
			continue
		}
		for _, p := range r.In.Scope {
			cands, err := scanClaimPathCandidates(ctx, q, projectID, r.SessionUUID, p, tc.Now)
			if err != nil {
				return 0, err
			}
			for _, c := range cands {
				if c.Mode == coordination.ModeRead || !coordination.PathsOverlap(p, c.Path) {
					continue
				}
				if len(scopeClaims) < decisionMaxScopeClaims {
					scopeClaims[c.ClaimUUID] = true
				}
			}
		}
		live, err := loadLiveIntents(ctx, q, "i.`project_uuid` = ?", []any{project}, r.SessionUUID, tc.Now, decisionMaxLiveIntentsScanned)
		if err != nil {
			return 0, err
		}
		for _, li := range live {
			intents[li.ID] = li
		}
	}

	signals := map[string]*coordination.DecisionCandidate{}
	mark := func(li liveIntent, sig coordination.CandidateSignal, shared int) {
		c, ok := signals[li.ID]
		if !ok {
			c = &coordination.DecisionCandidate{DecisionUUID: r.DecisionUUID.String(), DecisionKey: r.Key, IntentUUID: li.ID, IntentKey: li.Key}
			signals[li.ID] = c
		}
		if sig > c.Signal {
			c.Signal = sig
		}
		if shared > c.SharedTokens {
			c.SharedTokens = shared
		}
	}

	if len(scopeClaims) > 0 {
		ids := make([]string, 0, len(scopeClaims))
		for id := range scopeClaims {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		args := make([]any, 0, len(ids)+2)
		for _, id := range ids {
			args = append(args, id)
		}
		args = append(args, int64(enums.CLAIM_STATUS_HELD), len(ids))
		rows, err := q.QueryContext(ctx,
			"SELECT DISTINCT `intent_uuid` FROM `claim` WHERE `id` IN ("+placeholders(len(ids))+") AND `intent_uuid` IS NOT NULL AND `status` = ? LIMIT ?", args...)
		if err != nil {
			return 0, retryable(err, "reading which plans hold the scope")
		}
		var scoped []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return 0, retryable(err, "reading which plans hold the scope")
			}
			scoped = append(scoped, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, retryable(err, "reading which plans hold the scope")
		}
		_ = rows.Close()

		var missing []any
		for _, id := range scoped {
			if _, ok := intents[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			extra, err := loadLiveIntents(ctx, q, "i.`id` IN ("+placeholders(len(missing))+")", missing, r.SessionUUID, tc.Now, len(missing))
			if err != nil {
				return 0, err
			}
			for _, li := range extra {
				intents[li.ID] = li
			}
		}
		for _, id := range scoped {
			if li, ok := intents[id]; ok {
				mark(li, coordination.SignalScope, 0)
			}
		}
	}

	decisionTokens := map[string]bool{}
	for _, t := range r.In.Tokens {
		decisionTokens[t.Token] = true
	}
	for _, li := range intents {
		shared := 0
		for _, t := range coordination.Tokenize(li.Summary, decisionMaxTokens) {
			if decisionTokens[t] {
				shared++
			}
		}
		if shared >= decisionMinSharedTokens {
			mark(li, coordination.SignalWords, shared)
		}
		if r.AlwaysShow {
			mark(li, coordination.SignalAlwaysShow, shared)
		}
	}
	if len(signals) == 0 {
		return 0, nil
	}

	// Drop pinned subjects (a person dismissed the pair at some revision) and
	// pairs this exact revision pair already has.
	ids := make([]string, 0, len(signals))
	for id := range signals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	pinned, err := pinnedIntentsOfDecision(ctx, q, r.TeamUUID, r.DecisionUUID.String(), ids)
	if err != nil {
		return 0, err
	}
	keys := make([]string, 0, len(ids))
	keyOf := map[string]string{}
	for _, id := range ids {
		k := decisionPairKey(r.DecisionUUID.String(), r.Revision, id, intents[id].Revision)
		keyOf[id] = k
		keys = append(keys, k)
	}
	existing, err := existingPairKeys(ctx, q, r.TeamUUID, keys)
	if err != nil {
		return 0, err
	}
	var list []coordination.DecisionCandidate
	for _, id := range ids {
		if pinned[id] || existing[keyOf[id]] {
			continue
		}
		list = append(list, *signals[id])
	}

	settings, err := loadDecisionSettings(ctx, q, r.TeamUUID)
	if err != nil {
		return 0, err
	}
	assigned := map[string]int{}
	pairs := 0
	for _, c := range coordination.RankDecisionCandidates(list, decisionMaxCandidatesPerRecord) {
		li := intents[c.IntentUUID]
		if _, ok := assigned[li.Session]; !ok {
			n, err := reviewsAssignedRecently(ctx, q, li.Session, tc.Now, settings.MaxReviewsPerMinute)
			if err != nil {
				return 0, err
			}
			assigned[li.Session] = n
		}
		judge := ""
		if assigned[li.Session] < settings.MaxReviewsPerMinute {
			judge = li.Session
			assigned[li.Session]++
		}
		_, inserted, err := insertDecisionJudgement(ctx, q, r.TeamUUID, decisionJudgement{
			DecisionUUID: r.DecisionUUID.String(), DecisionRevision: r.Revision,
			IntentUUID: li.ID, IntentRevision: li.Revision, JudgeSession: judge,
		}, settings.JudgeWindow, tc.Now)
		if err != nil {
			return 0, err
		}
		if inserted {
			pairs++
		}
	}
	return pairs, nil
}

// pinnedIntentsOfDecision reports which intents have a pinned judgement with
// this decision at any revision. idx_judgement_subject, capped.
func pinnedIntentsOfDecision(ctx context.Context, q queryer, teamUUID uuid.UUID, decisionUUID string, intentIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(intentIDs) == 0 {
		return out, nil
	}
	args := []any{teamUUID.String(), decisionUUID}
	for _, id := range intentIDs {
		args = append(args, id)
	}
	args = append(args, len(intentIDs))
	rows, err := q.QueryContext(ctx,
		"SELECT DISTINCT `subject_b_uuid` FROM `judgement` WHERE `team_uuid` = ? AND `subject_a_uuid` = ? "+
			"AND `subject_b_uuid` IN ("+placeholders(len(intentIDs))+") AND `pinned` = 1 LIMIT ?", args...)
	if err != nil {
		return nil, retryable(err, "checking for dismissed pairs")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, retryable(err, "checking for dismissed pairs")
		}
		out[id] = true
	}
	return out, retryable(rows.Err(), "checking for dismissed pairs")
}
