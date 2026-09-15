package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/coordination"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// publishcontract.go is tool 8 from PLAN.md: "publish_contract — I produce or
// consume this shape (role param)".
//
// An agent about to build or call an interface between two parts of the
// system says so, with the shape it believes in, BEFORE it writes the code.
// The server canonicalizes and hashes the shape itself (agents never send a
// hash, or two agents describing one shape would produce different bytes),
// records the assertion, and compares it with the other side inside the same
// locked transaction — so the agent that just walked into a disagreement is
// told in this very response, and the other side on its next call.
//
// Three things are detected here and one elsewhere:
//
//   - contract_mismatch: a producer and a consumer disagree about a field the
//     other side needs (contractdetect.go);
//   - contract_naming_variant: the same field under another serializer's
//     spelling, or a consumed key with no producer next to a produced key one
//     or two edits away (/api/session vs /api/sessions);
//   - settling: a conflict whose shapes now agree, or whose producer finally
//     showed up, is closed in the same transaction (contractresolve.go);
//   - contract_unclaimed — a consumer nobody produces for — is defined by
//     elapsed time, so the sweeper raises it (app/sweeper/unclaimed.go).
//
// There is no retract verb, because PLAN.md specifies none: an assertion
// stops counting when its session ends or is abandoned, exactly the way a
// claim is released.

const (
	// MaxContractKeyChars bounds the written key. contract.key is 255; the
	// normalized form adds the kind prefix, so the cap leaves room for it.
	MaxContractKeyChars = 200

	// MaxContractTitleChars is contract.title.
	MaxContractTitleChars = 200

	// MaxContractShapeBytes bounds request and response together, as JSON.
	// A contract is the handful of fields two agents must agree on, not a
	// schema dump; anything bigger is refused rather than stored.
	MaxContractShapeBytes = 32 * 1024

	// MaxContractShapeDepth bounds nesting. Eight levels is far past any
	// payload two teammates reason about in one conversation, and it keeps
	// the walker's recursion bounded before a byte is parsed.
	MaxContractShapeDepth = 8

	// MaxContractShapeNodes bounds the total number of JSON values.
	MaxContractShapeNodes = 512

	// MaxContractFields bounds the flattened fields, which is the number of
	// contract_field rows one publish writes inside the team lock.
	MaxContractFields = 128

	// contractPathChars is contract_field.path and path_snake.
	contractPathChars = 255
)

// RegisterContractTools registers publish_contract.
func RegisterContractTools(s *mcp.Server, h *Handler, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}
	// Idempotent: publishing the same shape again changes nothing — no new
	// assertion, no new fields, no new conflict — and an idempotency_key makes
	// a retry replay the first answer byte for byte.
	idempotent := &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}

	addTool(s, h, logger, &mcp.Tool{
		Name: "publish_contract",
		Description: "Say which interface between parts of the system you are about to build or call, and the shape you believe it has — BEFORE you write the code. " +
			"Call it when you start building an endpoint, event, shared type, table, env var, component prop, CLI flag or config key that another part of the system uses (role 'produces'), " +
			"or when you start writing code that calls or reads one somebody else builds (role 'consumes'). " +
			"Send the request fields the producer reads and/or the response fields it returns, e.g. request {\"email!\": \"string\"}, response {\"token!\": \"string\", \"expires_at\": \"timestamp\"}; '!' marks a required field, '?' a nullable one. " +
			"metiche canonicalizes and hashes the shape itself and compares it with the other side in this same call: a disagreement comes back in conflicts[] with which side is missing what, and the other agent is told on its next call. " +
			"A consumer nobody produces is flagged to the team if it stays that way. Publish again whenever your shape changes; a conflict closes by itself once the shapes agree or a producer appears. " +
			"Your assertions stop counting when your session ends.",
		Annotations: idempotent,
	}, h.PublishContract)
}

// PublishContractParams is the tool's input. Request and Response are
// deliberately untyped: agents describe shapes in whatever dialect they think
// in (sample values, "string!", JSON-Schema-ish descriptors), and
// coordination.ParseShape maps all of it onto one closed vocabulary.
type PublishContractParams struct {
	SessionKey string `json:"session_key" jsonschema:"The session_key start_session gave you."`
	TeamSlug   string `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	Key        string `json:"key" jsonschema:"The interface, spelled the way a teammate would say it: 'POST /api/login', 'GET /api/users/{id}', 'user.created', 'DATABASE_URL'. For an HTTP endpoint include the method. Path parameters in any spelling ({id}, :id, a concrete id) and a trailing slash or letter case do not fork the contract. Max 200 characters."`
	Role       string `json:"role" jsonschema:"'produces' if you are building it (the handler, the publisher, the table owner), 'consumes' if you are writing code that calls or reads it."`
	Kind       string `json:"kind,omitempty" jsonschema:"What sort of interface: http_endpoint (the default), type, db_table, env_var, component_prop, cli_flag, queue_topic or config_key."`
	Request    any    `json:"request,omitempty" jsonschema:"The fields the producer READS: request body, query and path params, event input. An object of field name to type, e.g. {\"email!\": \"string\", \"remember\": \"bool\"}. Types: string, int, float, bool, timestamp, uuid, json, object, and arrays as 'string[]' or [{...}]. A trailing '!' on the name or type marks it required, '?' nullable. Nested objects are fine."`
	Response   any    `json:"response,omitempty" jsonschema:"The fields the producer RETURNS, in the same notation as request. A consumer lists only the fields it actually reads and marks the ones it cannot work without with '!'; a producer lists everything it returns. Send request, response or both."`
	Title      string `json:"title,omitempty" jsonschema:"An optional human name for the contract, shown on the board. Max 200 characters."`

	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Pass a key of your own and retrying this exact call returns the exact same answer, conflicts included, instead of publishing again."`
}

// contractInput is a validated, canonicalized publish: everything decided
// before the team lock is taken.
type contractInput struct {
	Key         string
	KeyNorm     string
	Title       string
	Kind        enums.ContractKind
	Role        enums.AssertionRole
	Shape       coordination.Shape
	Canonical   []byte
	Hash        string
	Fingerprint string
}

// parseContractInput validates and canonicalizes a publish. Pure: no
// database, no clock, so every refusal is testable without MySQL.
func parseContractInput(args PublishContractParams) (contractInput, error) {
	var in contractInput

	in.Key = strings.Join(strings.Fields(args.Key), " ")
	switch {
	case in.Key == "":
		return in, errors.New("key is required — the interface you produce or consume, spelled like 'POST /api/login'")
	case utf8.RuneCountInString(in.Key) > MaxContractKeyChars:
		return in, fmt.Errorf("key is %d characters and the limit is %d — name the interface, not its documentation",
			utf8.RuneCountInString(in.Key), MaxContractKeyChars)
	case strings.IndexFunc(in.Key, unicode.IsControl) >= 0:
		return in, errors.New("key contains a control character")
	}

	role, err := parseAssertionRole(args.Role)
	if err != nil {
		return in, err
	}
	in.Role = role

	kind, err := parseContractKind(args.Kind)
	if err != nil {
		return in, err
	}
	in.Kind = kind

	in.Title = strings.TrimSpace(args.Title)
	if utf8.RuneCountInString(in.Title) > MaxContractTitleChars {
		return in, fmt.Errorf("title is longer than %d characters", MaxContractTitleChars)
	}

	if args.Request == nil && args.Response == nil {
		return in, errors.New("send the shape: request (the fields the producer reads), response (the fields it returns), or both — " +
			"e.g. response {\"token!\": \"string\"}. An empty object is fine when there really are no fields")
	}

	// Depth and size are checked on the decoded value BEFORE anything walks
	// it, so a pathological submission costs a bounded amount of work.
	nodes := 0
	body := map[string]any{}
	for name, v := range map[string]any{"in": args.Request, "out": args.Response} {
		if v == nil {
			continue
		}
		if err := measureContractValue(v, 1, &nodes); err != nil {
			return in, err
		}
		body[name] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return in, fmt.Errorf("the shape could not be read as JSON: %w", err)
	}
	if len(raw) > MaxContractShapeBytes {
		return in, fmt.Errorf("the shape is %d bytes and the limit is %d — publish the fields the two sides must agree on, not the whole schema",
			len(raw), MaxContractShapeBytes)
	}

	shape, err := coordination.ParseShape(raw)
	if err != nil {
		return in, err
	}
	if len(shape.Fields) > MaxContractFields {
		return in, fmt.Errorf("the shape has %d fields and the limit is %d — publish the fields the two sides must agree on",
			len(shape.Fields), MaxContractFields)
	}
	for _, f := range shape.Fields {
		if len(f.Path) > contractPathChars || len(f.PathSnake) > contractPathChars {
			return in, fmt.Errorf("field path %.60q… is longer than %d characters", f.Path, contractPathChars)
		}
	}
	in.Shape = shape

	in.KeyNorm = coordination.NormalizeContractKey(kind.String(), in.Key)
	if len(in.KeyNorm) > 255 {
		return in, errors.New("key is too long once normalized")
	}
	in.Canonical = coordination.CanonicalBytes(shape)
	in.Hash = coordination.ShapeHash(shape)
	in.Fingerprint = coordination.ShapeFingerprint(shape)
	return in, nil
}

// measureContractValue enforces MaxContractShapeDepth and MaxContractShapeNodes.
func measureContractValue(v any, depth int, nodes *int) error {
	*nodes++
	if *nodes > MaxContractShapeNodes {
		return fmt.Errorf("the shape has more than %d values — publish the fields the two sides must agree on, not the whole schema", MaxContractShapeNodes)
	}
	if depth > MaxContractShapeDepth {
		return fmt.Errorf("the shape is nested deeper than %d levels", MaxContractShapeDepth)
	}
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if len(k) > contractPathChars {
				return fmt.Errorf("a field name is longer than %d characters", contractPathChars)
			}
			if err := measureContractValue(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range t {
			if err := measureContractValue(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseAssertionRole(s string) (enums.AssertionRole, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "produces", "produce", "producer", "provides", "provide", "builds", "build", "owns":
		return enums.ASSERTION_ROLE_PRODUCES, nil
	case "consumes", "consume", "consumer", "uses", "use", "calls", "call", "reads":
		return enums.ASSERTION_ROLE_CONSUMES, nil
	case "":
		return enums.ASSERTION_ROLE_INVALID, errors.New("role is required: 'produces' if you are building it, 'consumes' if you are calling or reading it")
	default:
		return enums.ASSERTION_ROLE_INVALID, fmt.Errorf("role must be 'produces' or 'consumes' (got %q)", s)
	}
}

func parseContractKind(s string) (enums.ContractKind, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", "http", "http_endpoint", "endpoint", "route", "api", "rest":
		return enums.CONTRACT_KIND_HTTP_ENDPOINT, nil
	case "table", "db", "database":
		return enums.CONTRACT_KIND_DB_TABLE, nil
	case "env", "environment", "env_variable":
		return enums.CONTRACT_KIND_ENV_VAR, nil
	case "prop", "props", "component":
		return enums.CONTRACT_KIND_COMPONENT_PROP, nil
	case "flag", "cli":
		return enums.CONTRACT_KIND_CLI_FLAG, nil
	case "topic", "queue", "event", "message":
		return enums.CONTRACT_KIND_QUEUE_TOPIC, nil
	case "config", "setting":
		return enums.CONTRACT_KIND_CONFIG_KEY, nil
	case "struct", "schema", "model":
		return enums.CONTRACT_KIND_TYPE, nil
	}
	if k := enums.ContractKindFromString(v); k != enums.CONTRACT_KIND_INVALID {
		return k, nil
	}
	return enums.CONTRACT_KIND_INVALID, fmt.Errorf(
		"kind must be one of http_endpoint, type, db_table, env_var, component_prop, cli_flag, queue_topic, config_key (got %q)", s)
}

// contractPublication is one publish as it travels from the tool through
// Apply (which fills in the rows it wrote) to the detection hook (which reads
// them back on the same transaction).
type contractPublication struct {
	TeamUUID    uuid.UUID
	ProjectUUID uuid.UUID
	SessionUUID uuid.UUID
	AgentUUID   uuid.UUID
	MemberUUID  uuid.UUID
	SessionKey  string
	MemberName  string
	AgentLabel  string

	In contractInput

	// Filled by Apply.
	ContractUUID  uuid.UUID
	ContractKey   string
	AssertionUUID uuid.UUID
	Revision      int64
	// Outcome is "new", "revised" or "unchanged".
	Outcome string
}

// PublishContract records one side of one interface.
//
// In ONE transaction under the team lock: the contract row (matched on its
// normalized key, so two spellings of one endpoint land on one row), the
// caller's single active assertion for that role (superseding its previous
// one when the shape changed, and touching nothing when it did not), the
// flattened contract_field rows, the settling of conflicts this publish
// resolved, and then detection against every other live assertion on the
// contract. Structural, because a contract or an assertion appearing changes
// the Contracts tab's shape and the board refetches it on a structural frame.
func (h *Handler) PublishContract(ctx context.Context, _ *mcp.CallToolRequest, args PublishContractParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSessionOnTeam(ctx, args.SessionKey, args.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session
	if err := requireWorkableSession(sess.Status); err != nil {
		return nil, nil, err
	}

	in, err := parseContractInput(args)
	if err != nil {
		return nil, nil, err
	}

	idem := strings.TrimSpace(args.IdempotencyKey)
	if idem == "" {
		nonce, err := uuid.NewV4()
		if err != nil {
			return nil, nil, err
		}
		idem = "contract_published:" + nonce.String()
	} else {
		// Namespaced by session so two agents that pick the same key do not
		// replay each other's publish.
		idem = "contract_published:" + sess.ID.String() + ":" + idem
	}

	p := &contractPublication{
		TeamUUID:    who.Team.ID,
		ProjectUUID: sess.ProjectUUID,
		SessionUUID: sess.ID,
		AgentUUID:   ag.ID,
		MemberUUID:  who.Member.ID,
		SessionKey:  sess.Key,
		MemberName:  who.Member.DisplayName,
		AgentLabel:  ag.Label,
		In:          in,
	}

	response, err := h.commit(ctx, Mutation{
		TeamUUID:       who.Team.ID,
		IdempotencyKey: idem,
		Kind:           enums.EVENT_KIND_CONTRACT_PUBLISHED,
		Structural:     true,
		ProjectUUID:    uuidPtr(sess.ProjectUUID),
		SessionUUID:    uuidPtr(sess.ID),
		AgentUUID:      uuidPtr(ag.ID),
		MemberUUID:     uuidPtr(who.Member.ID),
		SubjectKind:    enums.SUBJECT_KIND_CONTRACT,
		Summary:        fmt.Sprintf("%s %s %s", ag.Label, in.Role.String(), in.Key),
		Payload: payload_entity.EventPayload{
			Message: nullString(in.Key),
			Detail:  nullString(in.Role.String()),
		},
		Apply: func(ctx context.Context, tc *TxContext, env *Envelope) error {
			return h.applyContractPublication(ctx, tc, env, p)
		},
		// The contract detector is this call's own hook, not the handler-wide
		// path detector: publish_contract claims no paths, and the check has
		// to see the assertion Apply just wrote, under the same lock.
		Detect: func(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
			return h.detectContractConflicts(ctx, tc, m, p)
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(response)
}

// applyContractPublication writes the contract, the assertion and its fields,
// then settles whatever this publish resolved.
func (h *Handler) applyContractPublication(ctx context.Context, tc *TxContext, env *Envelope, p *contractPublication) error {
	// Re-read under the lock: an end_session that committed between
	// RequireSession and here must not leave an active assertion on an ended
	// session, which nothing would ever withdraw.
	var status int64
	if err := tc.Tx.QueryRowContext(ctx, "SELECT `status` FROM `session` WHERE `id` = ?", p.SessionUUID.String()).Scan(&status); err != nil {
		return retryable(err, "re-reading the session")
	}
	if err := requireWorkableSession(enums.SessionStatus(status)); err != nil {
		return err
	}

	if err := upsertContract(ctx, tc, p); err != nil {
		return err
	}
	if err := upsertAssertion(ctx, tc, p); err != nil {
		return err
	}

	env.Key = truncate(p.ContractKey, 200)
	switch p.Outcome {
	case "unchanged":
		env.Note = fmt.Sprintf("%s: you already %s this shape (revision %d); nothing changed", p.ContractKey, p.In.Role.String(), p.Revision)
	case "revised":
		env.Note = fmt.Sprintf("%s: your %s shape is now revision %d (%d field(s))", p.ContractKey, p.In.Role.String(), p.Revision, len(p.In.Shape.Fields))
	default:
		env.Note = fmt.Sprintf("%s: published as %s (%d field(s))", p.ContractKey, p.In.Role.String(), len(p.In.Shape.Fields))
	}

	// A publish can only settle conflicts on this contract: shapes that now
	// agree, or a producer arriving for a consumer that had none.
	return h.settleContractConflicts(ctx, tc, ContractRelease{
		TeamUUID:    p.TeamUUID,
		SessionUUID: p.SessionUUID,
		Kind:        ContractReleasePublished,
		At:          tc.Now,
	}, p.ContractUUID, uuid.Nil, eventActor{
		ProjectUUID: p.ProjectUUID, SessionUUID: p.SessionUUID, AgentUUID: p.AgentUUID, MemberUUID: p.MemberUUID,
	})
}

// upsertContract finds the contract by its normalized key or creates it.
func upsertContract(ctx context.Context, tc *TxContext, p *contractPublication) error {
	in := p.In
	var (
		id, key string
		st      int64
		title   sql.NullString
	)
	err := tc.Tx.QueryRowContext(ctx,
		"SELECT `id`, `key`, `status`, `title` FROM `contract` WHERE `project_uuid` = ? AND `key_norm` = ? AND `team_uuid` = ?",
		p.ProjectUUID.String(), in.KeyNorm, p.TeamUUID.String()).Scan(&id, &key, &st, &title)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		cid, err := uuid.NewV4()
		if err != nil {
			return err
		}
		// Proposed until somebody says they build it: a contract only
		// consumed is exactly the one the unclaimed rule watches.
		status := enums.ContractStatus(enums.CONTRACT_STATUS_PROPOSED)
		if in.Role == enums.ASSERTION_ROLE_PRODUCES {
			status = enums.CONTRACT_STATUS_PUBLISHED
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"INSERT INTO `contract` (`id`,`team_uuid`,`project_uuid`,`key`,`key_norm`,`kind`,`status`,`title`,`created_at`,`updated_at`) "+
				"VALUES (?,?,?,?,?,?,?,?,?,?)",
			cid.String(), p.TeamUUID.String(), p.ProjectUUID.String(), in.Key, in.KeyNorm,
			int64(in.Kind), int64(status), nullIfEmpty(in.Title), tc.Now, tc.Now); err != nil {
			return retryable(err, "recording the contract")
		}
		p.ContractUUID, p.ContractKey = cid, in.Key
		return nil
	case err != nil:
		return retryable(err, "looking up the contract")
	}

	p.ContractUUID, err = uuid.FromString(id)
	if err != nil {
		return err
	}
	// The first spelling stays the contract's name; later spellings of the
	// same normalized key are the same contract.
	p.ContractKey = key
	promote := in.Role == enums.ASSERTION_ROLE_PRODUCES && enums.ContractStatus(st) == enums.CONTRACT_STATUS_PROPOSED
	fillTitle := in.Title != "" && strings.TrimSpace(title.String) == ""
	if promote || fillTitle {
		newStatus := st
		if promote {
			newStatus = int64(enums.CONTRACT_STATUS_PUBLISHED)
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"UPDATE `contract` SET `status` = ?, `title` = COALESCE(NULLIF(`title`, ''), ?), `updated_at` = ? WHERE `id` = ?",
			newStatus, nullIfEmpty(in.Title), tc.Now, id); err != nil {
			return retryable(err, "updating the contract")
		}
	}
	return nil
}

// upsertAssertion keeps exactly one active assertion per (contract, session,
// role): the unique index on active_marker enforces it, and this function is
// what keeps a republish from ever needing a second one.
func upsertAssertion(ctx context.Context, tc *TxContext, p *contractPublication) error {
	in := p.In
	var (
		oldID, oldHash string
		oldRev         int64
		oldAsserted    sql.NullTime
	)
	err := tc.Tx.QueryRowContext(ctx,
		"SELECT `id`, `shape_hash`, `revision`, `asserted_at` FROM `contract_assertion` "+
			"WHERE `contract_uuid` = ? AND `session_uuid` = ? AND `role` = ? AND `status` = ? LIMIT 1",
		p.ContractUUID.String(), p.SessionUUID.String(), int64(in.Role), int64(enums.ASSERTION_STATUS_ACTIVE)).
		Scan(&oldID, &oldHash, &oldRev, &oldAsserted)
	found := true
	switch {
	case errors.Is(err, sql.ErrNoRows):
		found = false
	case err != nil:
		return retryable(err, "looking up your assertion")
	}

	if found && oldHash == in.Hash {
		// Same canonical shape: nothing to write. asserted_at is left alone
		// on purpose — it is when this session started depending on the
		// contract, and the unclaimed rule measures the wait from it.
		p.AssertionUUID, _ = uuid.FromString(oldID)
		p.Revision = oldRev
		p.Outcome = "unchanged"
		return nil
	}

	newID, err := uuid.NewV4()
	if err != nil {
		return err
	}
	rev := int64(1)
	asserted := tc.Now
	p.Outcome = "new"
	if found {
		// Retire the old row FIRST: it still holds active_marker = 1, and the
		// unique index would refuse the new one beside it. Its field rows go
		// with it — the canonical shape stays on the row for history, and the
		// flattened copy exists only for live comparison.
		if _, err := tc.Tx.ExecContext(ctx,
			"UPDATE `contract_assertion` SET `status` = ?, `active_marker` = NULL, `superseded_by_uuid` = ?, `updated_at` = ? WHERE `id` = ?",
			int64(enums.ASSERTION_STATUS_SUPERSEDED), newID.String(), tc.Now, oldID); err != nil {
			return retryable(err, "superseding your previous assertion")
		}
		if _, err := tc.Tx.ExecContext(ctx, "DELETE FROM `contract_field` WHERE `assertion_uuid` = ?", oldID); err != nil {
			return retryable(err, "clearing your previous fields")
		}
		rev = oldRev + 1
		if oldAsserted.Valid {
			asserted = oldAsserted.Time
		}
		p.Outcome = "revised"
	}

	// string(), not []byte: see the note on JSON columns in detector.go.
	if _, err := tc.Tx.ExecContext(ctx,
		"INSERT INTO `contract_assertion` (`id`,`contract_uuid`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`role`,"+
			"`shape`,`shape_hash`,`shape_fingerprint`,`status`,`revision`,`active_marker`,`asserted_at`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		newID.String(), p.ContractUUID.String(), p.TeamUUID.String(), p.ProjectUUID.String(), p.SessionUUID.String(),
		p.MemberUUID.String(), int64(in.Role), string(in.Canonical), in.Hash, in.Fingerprint,
		int64(enums.ASSERTION_STATUS_ACTIVE), rev, 1, asserted, tc.Now, tc.Now); err != nil {
		return retryable(err, "recording your assertion")
	}
	p.AssertionUUID, p.Revision = newID, rev

	return insertContractFields(ctx, tc, p)
}

// insertContractFields flattens the shape into contract_field in one
// statement, at most MaxContractFields rows. A path present in both the request
// and the response gets one row per direction, matching
// uq_contract_field_path (assertion_uuid, direction, path).
func insertContractFields(ctx context.Context, tc *TxContext, p *contractPublication) error {
	fields := p.In.Shape.Fields
	if len(fields) == 0 {
		return nil
	}
	var (
		ph   []string
		args []any
	)
	type fieldKey struct {
		direction coordination.ContractDirection
		path      string
	}
	seen := map[fieldKey]bool{}
	for _, f := range fields {
		k := fieldKey{f.Direction, f.Path}
		if seen[k] {
			continue
		}
		seen[k] = true
		id, err := uuid.NewV4()
		if err != nil {
			return err
		}
		ph = append(ph, "(?,?,?,?,?,?,?,?,?,?,?)")
		args = append(args, id.String(), p.AssertionUUID.String(), p.ContractUUID.String(),
			f.Path, f.PathSnake, int64(contractFieldTypeEnum(f.Type)), f.Required, f.Nullable,
			int64(fieldDirectionEnum(f.Direction)), tc.Now, tc.Now)
	}
	if _, err := tc.Tx.ExecContext(ctx,
		"INSERT INTO `contract_field` (`id`,`assertion_uuid`,`contract_uuid`,`path`,`path_snake`,`type`,`required`,`nullable`,`direction`,`created_at`,`updated_at`) VALUES "+
			strings.Join(ph, ","), args...); err != nil {
		return retryable(err, "recording the contract's fields")
	}
	return nil
}

func contractFieldTypeEnum(t coordination.ContractType) enums.ContractFieldType {
	if strings.HasPrefix(string(t), "array<") {
		return enums.CONTRACT_FIELD_TYPE_ARRAY
	}
	switch t {
	case coordination.ContractTypeString:
		return enums.CONTRACT_FIELD_TYPE_STRING
	case coordination.ContractTypeInt:
		return enums.CONTRACT_FIELD_TYPE_INT
	case coordination.ContractTypeFloat:
		return enums.CONTRACT_FIELD_TYPE_FLOAT
	case coordination.ContractTypeBool:
		return enums.CONTRACT_FIELD_TYPE_BOOL
	case coordination.ContractTypeTimestamp:
		return enums.CONTRACT_FIELD_TYPE_TIMESTAMP
	case coordination.ContractTypeUUID:
		return enums.CONTRACT_FIELD_TYPE_UUID
	case coordination.ContractTypeNull:
		return enums.CONTRACT_FIELD_TYPE_NULL
	case coordination.ContractTypeObject:
		return enums.CONTRACT_FIELD_TYPE_OBJECT
	default:
		return enums.CONTRACT_FIELD_TYPE_JSON
	}
}

func fieldDirectionEnum(d coordination.ContractDirection) enums.FieldDirection {
	if d == coordination.DirectionIn {
		return enums.FIELD_DIRECTION_IN
	}
	return enums.FIELD_DIRECTION_OUT
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
