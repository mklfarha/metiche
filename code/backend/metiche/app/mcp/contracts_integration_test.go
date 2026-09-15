package mcp

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// publish_contract against a real MySQL. Run with METICHE_TEST_MYSQL_DSN set
// (see integration_test.go), for example:
//
//	go test -p 1 ./app/mcp/ -run 'Contract' -v

type contractAgent struct {
	ctx     context.Context
	key     string
	session uuid.UUID
}

func (hs *harness) contractAgent(t *testing.T, name, client string) contractAgent {
	t.Helper()
	c := hs.join(t, name, client)
	return hs.contractAgentFor(t, c.ctx, name)
}

func (hs *harness) contractAgentFor(t *testing.T, ctx context.Context, name string) contractAgent {
	t.Helper()
	res, _, err := hs.h.StartSession(ctx, nil, StartSessionParams{
		ProjectKey: "metiche", Goal: name + " work", ConfirmNewProject: "person"})
	if err != nil {
		t.Fatalf("start_session for %s: %v", name, err)
	}
	var env Envelope
	decodeResult(t, res, &env)
	who, err := hs.h.RequireSession(ctx, env.Key)
	if err != nil {
		t.Fatal(err)
	}
	return contractAgent{ctx: ctx, key: env.Key, session: who.Session.ID}
}

func (hs *harness) publish(t *testing.T, a contractAgent, args PublishContractParams) (Envelope, string) {
	t.Helper()
	args.SessionKey = a.key
	res, _, err := hs.h.PublishContract(a.ctx, nil, args)
	if err != nil {
		t.Fatalf("publish_contract %s %s: %v", args.Role, args.Key, err)
	}
	text := resultText(t, res)
	var env Envelope
	decodeResult(t, res, &env)
	return env, text
}

func login(t *testing.T, role, request, response string) PublishContractParams {
	p := PublishContractParams{Key: "POST /api/login", Role: role}
	if request != "" {
		p.Request = shapeValue(t, request)
	}
	if response != "" {
		p.Response = shapeValue(t, response)
	}
	return p
}

type contractConflictRow struct {
	id, kind, severity, status, resolution int64
	key, rule, note, yield                 string
	occurrences                            int64
	uuid                                   string
}

func (hs *harness) contractConflicts(t *testing.T) []contractConflictRow {
	t.Helper()
	rows, err := hs.core.DB().Query(
		"SELECT `id`, `key`, `kind`, `severity`, `status`, COALESCE(`resolution`, 0), COALESCE(`detector_rule`, ''), "+
			"COALESCE(`resolution_note`, ''), COALESCE(`suggested_yield_session_uuid`, ''), `occurrence_count` "+
			"FROM `conflict` WHERE `team_uuid` = ? ORDER BY `created_at`, `key`", hs.teamID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []contractConflictRow
	for rows.Next() {
		var r contractConflictRow
		if err := rows.Scan(&r.uuid, &r.key, &r.kind, &r.severity, &r.status, &r.resolution, &r.rule, &r.note, &r.yield, &r.occurrences); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestIntegrationContractMatchingShapesNoConflict: a producer that returns
// more than the consumer reads, and a consumer that sends what the producer
// requires, agree.
func TestIntegrationContractMatchingShapesNoConflict(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")

	hs.publish(t, ana, login(t, "produces", `{"email!":"string","password!":"string"}`, `{"token!":"string","expires_at":"timestamp"}`))
	env, text := hs.publish(t, bob, login(t, "consumes", `{"password":"string","email":"string"}`, `{"token!":"string"}`))
	t.Logf("consumer response: %s", text)
	if len(env.Conflicts) != 0 {
		t.Fatalf("matching shapes raised %v", env.Conflicts)
	}
	db := hs.core.DB()
	if n := countRows(t, db, "SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ?", hs.teamID.String()); n != 0 {
		t.Fatalf("%d conflict rows, want 0", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `contract` WHERE `team_uuid` = ?", hs.teamID.String()); n != 1 {
		t.Fatalf("%d contracts, want 1", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `contract_assertion` WHERE `team_uuid` = ? AND `status` = ?", hs.teamID.String(), enums.ASSERTION_STATUS_ACTIVE); n != 2 {
		t.Fatalf("%d active assertions, want 2", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `contract_field`"); n != 7 {
		t.Fatalf("%d contract_field rows, want 7 (4 produced + 3 consumed)", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `structural` = 1",
		hs.teamID.String(), enums.EVENT_KIND_CONTRACT_PUBLISHED); n != 2 {
		t.Fatalf("%d structural contract_published events, want 2", n)
	}
	if !strings.Contains(env.Note, "published as consumes") {
		t.Errorf("note = %q", env.Note)
	}
}

// TestIntegrationContractMissingOutFaultsProducer: the consumer requires a
// response field the producer does not return. The consumer is told now; the
// producer is at fault and gets the notice.
func TestIntegrationContractMissingOutFaultsProducer(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")

	hs.publish(t, ana, login(t, "produces", `{"email!":"string"}`, `{"token!":"string"}`))
	env, text := hs.publish(t, bob, login(t, "consumes", `{"email":"string"}`, `{"token!":"string","expires_at!":"timestamp"}`))
	t.Logf("consumer response: %s", text)

	if len(env.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", env.Conflicts)
	}
	c := env.Conflicts[0]
	if c.Kind != "contract_mismatch" || c.Severity != "high" || c.AtFault != FaultProducer || c.Contract != "POST /api/login" {
		t.Fatalf("notice = %+v, want a high contract_mismatch with the producer at fault", c)
	}
	if !strings.Contains(c.With, "Ana") || !strings.Contains(c.SuggestedAction, "expires_at") || len(c.Fields) == 0 {
		t.Fatalf("notice does not name the other side and the field: %+v", c)
	}
	if env.Pending.Conflicts != 1 {
		t.Errorf("pending.conflicts = %d, want 1", env.Pending.Conflicts)
	}

	rows := hs.contractConflicts(t)
	if len(rows) != 1 || rows[0].yield != ana.session.String() || rows[0].rule != RuleContractMissingOut ||
		enums.ConflictSeverity(rows[0].severity) != enums.CONFLICT_SEVERITY_HIGH {
		t.Fatalf("conflict rows = %+v, want one high missing_out yielding to Ana's session", rows)
	}

	// The notice goes to the OTHER side — the producer — and nobody else.
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ? AND `kind` = ?",
		bob.session.String(), enums.INSTRUCTION_KIND_CONFLICT_NOTICE); n != 0 {
		t.Errorf("the caller got %d instruction(s); it was told synchronously", n)
	}
	res, _, err := hs.h.GetInstructions(ana.ctx, nil, GetInstructionsParams{SessionKey: ana.key})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("producer's get_instructions: %s", resultText(t, res))
	var got InstructionsResult
	decodeResult(t, res, &got)
	if len(got.Instructions) != 1 || got.Instructions[0].Kind != "conflict_notice" ||
		!strings.Contains(got.Instructions[0].SuggestedAction, "Add it") || !strings.Contains(got.Instructions[0].SuggestedAction, "expires_at") ||
		got.Instructions[0].Ref != c.Key {
		t.Fatalf("producer's instructions = %+v", got.Instructions)
	}
}

// TestIntegrationContractMissingInFaultsConsumer: the producer requires a
// request field the consumer does not send. The consumer is at fault.
func TestIntegrationContractMissingInFaultsConsumer(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")

	hs.publish(t, bob, login(t, "consumes", `{"email":"string"}`, `{"token!":"string"}`))
	env, text := hs.publish(t, ana, login(t, "produces", `{"email!":"string","password!":"string"}`, `{"token!":"string"}`))
	t.Logf("producer response: %s", text)

	if len(env.Conflicts) != 1 || env.Conflicts[0].AtFault != FaultConsumer || env.Conflicts[0].Severity != "high" {
		t.Fatalf("conflicts = %+v, want one high with the consumer at fault", env.Conflicts)
	}
	rows := hs.contractConflicts(t)
	if len(rows) != 1 || rows[0].yield != bob.session.String() || rows[0].rule != RuleContractMissingIn {
		t.Fatalf("conflict rows = %+v, want missing_in yielding to Bob's session", rows)
	}
	var body string
	if err := hs.core.DB().QueryRow("SELECT `body` FROM `instruction` WHERE `target_session_uuid` = ?", bob.session.String()).Scan(&body); err != nil {
		t.Fatalf("the consumer got no notice: %v", err)
	}
	if !strings.Contains(body, "password") || !strings.Contains(body, "Send it") {
		t.Errorf("consumer notice = %q", body)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ?", ana.session.String()); n != 0 {
		t.Errorf("the producer (the caller) got %d instruction(s)", n)
	}
}

// TestIntegrationContractTypeMismatch: two type families cannot deserialize,
// which is critical, and neither side is presumed right.
func TestIntegrationContractTypeMismatch(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")

	hs.publish(t, ana, PublishContractParams{Key: "GET /api/cart", Role: "produces", Response: shapeValue(t, `{"total!":"int"}`)})
	env, text := hs.publish(t, bob, PublishContractParams{Key: "GET /api/cart", Role: "consumes", Response: shapeValue(t, `{"total!":"string"}`)})
	t.Logf("consumer response: %s", text)
	if len(env.Conflicts) != 1 || env.Conflicts[0].Severity != "critical" || env.Conflicts[0].AtFault != FaultBoth ||
		!strings.Contains(env.Conflicts[0].Fields[0], "type_mismatch") {
		t.Fatalf("conflicts = %+v, want one critical type mismatch with both at fault", env.Conflicts)
	}
	if rows := hs.contractConflicts(t); len(rows) != 1 || rows[0].yield != "" || rows[0].rule != RuleContractType {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestIntegrationContractNamingVariants: a field under another serializer's
// spelling, and a consumed key one edit from a produced one, are recorded at
// PLAN.md's low severity — on the board, below the notify floor. Trailing
// slashes and case are not variants at all: they are the same contract.
func TestIntegrationContractNamingVariants(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")

	hs.publish(t, ana, PublishContractParams{Key: "GET /api/me", Role: "produces", Response: shapeValue(t, `{"userId":"uuid"}`)})
	env, _ := hs.publish(t, bob, PublishContractParams{Key: "GET /api/me", Role: "consumes", Response: shapeValue(t, `{"user_id":"uuid"}`)})
	if len(env.Conflicts) != 0 {
		t.Errorf("a low naming variant interrupted the caller: %+v", env.Conflicts)
	}

	hs.publish(t, bob, PublishContractParams{Key: "GET /api/session", Role: "consumes", Response: shapeValue(t, `{"id!":"uuid"}`)})
	env, text := hs.publish(t, ana, PublishContractParams{Key: "GET /api/sessions", Role: "produces", Response: shapeValue(t, `{"id!":"uuid"}`)})
	t.Logf("producer on the near key: %s", text)
	if len(env.Conflicts) != 0 {
		t.Errorf("a low key variant interrupted the caller: %+v", env.Conflicts)
	}
	// Same contract, different spelling: no variant, no second row.
	hs.publish(t, bob, PublishContractParams{Key: "get /API/Sessions/", Role: "consumes", Response: shapeValue(t, `{"id!":"uuid"}`)})

	rows := hs.contractConflicts(t)
	rules := map[string]contractConflictRow{}
	for _, r := range rows {
		rules[r.rule] = r
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want a field variant and a key variant", rows)
	}
	for _, rule := range []string{RuleContractFieldVariant, RuleContractKeyVariant} {
		r, ok := rules[rule]
		if !ok || enums.ConflictKind(r.kind) != enums.CONFLICT_KIND_CONTRACT_NAMING_VARIANT || enums.ConflictSeverity(r.severity) != enums.CONFLICT_SEVERITY_LOW {
			t.Errorf("%s: row %+v, want a low contract_naming_variant", rule, r)
		}
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `contract` WHERE `team_uuid` = ?", hs.teamID.String()); n != 3 {
		t.Errorf("%d contracts, want 3 (/api/me, /api/session, /api/sessions)", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", hs.teamID.String()); n != 0 {
		t.Errorf("%d instructions for low variants, want 0", n)
	}
	// Bob now consumes the produced key too, so his orphaned key has a... no:
	// the variant closes when /api/session gets a producer.
	env, _ = hs.publish(t, ana, PublishContractParams{Key: "GET /api/session", Role: "produces", Response: shapeValue(t, `{"id!":"uuid"}`)})
	for _, r := range hs.contractConflicts(t) {
		if r.rule == RuleContractKeyVariant && (enums.ConflictStatus(r.status) != enums.CONFLICT_STATUS_RESOLVED ||
			enums.ConflictResolution(r.resolution) != enums.CONFLICT_RESOLUTION_CONVERGED) {
			t.Errorf("key variant after /api/session got a producer: %+v", r)
		}
	}
}

// TestIntegrationContractConvergenceSettlesAndReopens: once the producer adds
// the field the conflict closes as converged, with a truthful note and a
// conflict_resolved event; a regression reopens the same conflict.
func TestIntegrationContractConvergenceSettlesAndReopens(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	db := hs.core.DB()

	hs.publish(t, ana, login(t, "produces", "", `{"token!":"string"}`))
	hs.publish(t, bob, login(t, "consumes", "", `{"token!":"string","expires_at!":"timestamp"}`))

	env, text := hs.publish(t, ana, login(t, "produces", "", `{"token!":"string","expires_at":"timestamp"}`))
	t.Logf("producer's fixing publish: %s", text)
	if len(env.Conflicts) != 0 || env.Pending.Conflicts != 0 {
		t.Fatalf("after converging: conflicts %+v pending %+v", env.Conflicts, env.Pending)
	}
	rows := hs.contractConflicts(t)
	if len(rows) != 1 || enums.ConflictStatus(rows[0].status) != enums.CONFLICT_STATUS_RESOLVED ||
		enums.ConflictResolution(rows[0].resolution) != enums.CONFLICT_RESOLUTION_CONVERGED {
		t.Fatalf("rows = %+v, want one resolved as converged", rows)
	}
	t.Logf("resolution note: %s", rows[0].note)
	if !strings.Contains(rows[0].note, "now agree") || !strings.Contains(rows[0].note, ana.key) || !strings.HasPrefix(rows[0].note, "Settled by the agents") {
		t.Errorf("note = %q", rows[0].note)
	}
	var summary string
	if err := db.QueryRow("SELECT `summary` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), enums.EVENT_KIND_CONFLICT_RESOLVED).Scan(&summary); err != nil {
		t.Fatalf("no conflict_resolved event: %v", err)
	}
	t.Logf("conflict_resolved summary: %s", summary)
	if !strings.Contains(summary, "converged") || !strings.Contains(summary, "contract mismatch") {
		t.Errorf("summary = %q", summary)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `contract_assertion` WHERE `session_uuid` = ? AND `status` = ?", ana.session.String(), enums.ASSERTION_STATUS_SUPERSEDED); n != 1 {
		t.Errorf("%d superseded assertions for the producer, want 1", n)
	}

	env, _ = hs.publish(t, ana, login(t, "produces", "", `{"token!":"string"}`))
	rows = hs.contractConflicts(t)
	if len(env.Conflicts) != 1 || len(rows) != 1 || enums.ConflictStatus(rows[0].status) != enums.CONFLICT_STATUS_OPEN || rows[0].key != env.Conflicts[0].Key {
		t.Fatalf("a regression should reopen the same conflict: notices %+v rows %+v", env.Conflicts, rows)
	}
}

// TestIntegrationContractSessionEndSupersedes: an ended session's assertions
// stop counting, and the conflicts they were in close as superseded.
func TestIntegrationContractSessionEndSupersedes(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	db := hs.core.DB()

	hs.publish(t, ana, login(t, "produces", "", `{"token!":"string"}`))
	hs.publish(t, bob, login(t, "consumes", "", `{"token!":"string","expires_at!":"timestamp"}`))

	if _, _, err := hs.h.EndSession(bob.ctx, nil, EndSessionParams{SessionKey: bob.key, Note: "login screen shipped"}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `contract_assertion` WHERE `session_uuid` = ? AND `status` = ?", bob.session.String(), enums.ASSERTION_STATUS_WITHDRAWN); n != 1 {
		t.Fatalf("%d withdrawn assertions for the ended session, want 1", n)
	}
	rows := hs.contractConflicts(t)
	if len(rows) != 1 || enums.ConflictResolution(rows[0].resolution) != enums.CONFLICT_RESOLUTION_SUPERSEDED {
		t.Fatalf("rows = %+v, want superseded", rows)
	}
	t.Logf("resolution note: %s", rows[0].note)
	if !strings.Contains(rows[0].note, "ended its session") || !strings.Contains(rows[0].note, bob.key) {
		t.Errorf("note = %q", rows[0].note)
	}
	// And it no longer counts for detection.
	env, _ := hs.publish(t, ana, login(t, "produces", "", `{"other":"string"}`))
	if len(env.Conflicts) != 0 {
		t.Errorf("an ended session's assertion still raised %+v", env.Conflicts)
	}
	if _, _, err := hs.h.PublishContract(bob.ctx, nil, PublishContractParams{SessionKey: bob.key, Key: "GET /x", Role: "consumes", Response: shapeValue(t, `{}`)}); err == nil ||
		!strings.Contains(err.Error(), "already ended") {
		t.Errorf("publishing on an ended session: err = %v", err)
	}
}

// TestIntegrationContractReplayIsByteIdentical: the same idempotency_key
// returns the same bytes, conflicts included, and applies once.
func TestIntegrationContractReplayIsByteIdentical(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.publish(t, ana, login(t, "produces", "", `{"token!":"string"}`))

	args := login(t, "consumes", "", `{"token!":"string","expires_at!":"timestamp"}`)
	args.IdempotencyKey = "bob-login-1"
	_, first := hs.publish(t, bob, args)
	hs.publish(t, ana, login(t, "produces", "", `{"token!":"string","other":"int"}`)) // the world moves on
	_, second := hs.publish(t, bob, args)
	t.Logf("first:  %s", first)
	t.Logf("replay: %s", second)
	if first != second {
		t.Fatal("the replay is not byte-identical")
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `idempotency_key` LIKE ?",
		hs.teamID.String(), "%:bob-login-1"); n != 1 {
		t.Errorf("%d events for the key, want 1", n)
	}
}

// TestIntegrationContractRepublishWritesNoDuplicates: the same shape in
// another key order and dialect is unchanged — no new assertion, fields or
// conflict — and re-detection only bumps the counter.
func TestIntegrationContractRepublishWritesNoDuplicates(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	db := hs.core.DB()

	hs.publish(t, bob, login(t, "consumes", "", `{"token!":"string","expires_at!":"timestamp"}`))
	for i, shape := range []string{
		`{"token!":"string","ttl":"int"}`,
		`{"ttl":"integer","token":"string!"}`,
		`{"ttl":{"type":"int"},"token":{"type":"string","required":true,"description":"JWT"}}`,
	} {
		env, _ := hs.publish(t, ana, login(t, "produces", "", shape))
		if i > 0 && !strings.Contains(env.Note, "nothing changed") {
			t.Errorf("republish %d note = %q, want unchanged", i, env.Note)
		}
		if len(env.Conflicts) != 1 {
			t.Errorf("republish %d: %d notices, want the one open mismatch", i, len(env.Conflicts))
		}
	}
	team := hs.teamID.String()
	for q, want := range map[string]int{
		"SELECT COUNT(*) FROM `contract` WHERE `team_uuid` = ?":                                                           1,
		"SELECT COUNT(*) FROM `contract_assertion` WHERE `team_uuid` = ?":                                                 2,
		"SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ?":                                                           1,
		"SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?":                                                        1,
		"SELECT COUNT(*) FROM `contract_field` f JOIN `contract` c ON c.`id` = f.`contract_uuid` WHERE c.`team_uuid` = ?": 4,
		"SELECT `occurrence_count` FROM `conflict` WHERE `team_uuid` = ?":                                                 3,
	} {
		if n := countRows(t, db, q, team); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
}

// TestIntegrationContractSamePathBothDirections: `id` sent and returned is one
// field row (see insertContractFields) and does not fail the publish.
func TestIntegrationContractSamePathBothDirections(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	env, _ := hs.publish(t, ana, PublishContractParams{Key: "PUT /api/items/{id}", Role: "produces",
		Request: shapeValue(t, `{"id!":"uuid","name":"string"}`), Response: shapeValue(t, `{"id!":"uuid"}`)})
	if !strings.Contains(env.Note, "3 field(s)") {
		t.Errorf("note = %q, want the 3 canonical fields", env.Note)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `contract_field`"); n != 2 {
		t.Errorf("%d field rows, want 2", n)
	}
}

// TestIntegrationContractCrossTeamIsolation: the same key on another team is a
// different contract and never meets this team's assertions.
func TestIntegrationContractCrossTeamIsolation(t *testing.T) {
	hs := newHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	hs.publish(t, ana, login(t, "produces", "", `{"token!":"string"}`))

	other := uuid.Must(uuid.NewV4())
	planID := hs.planID
	if _, err := hs.core.Team().Insert(context.Background(), team_types.UpsertRequest{Team: team_entity.Team{
		ID: other, Name: "Other team", Slug: "other-" + other.String()[:8], Status: enums.RECORD_STATUS_ACTIVE,
		PlanUUID: &planID, PlanSource: enums.PLAN_SOURCE_INSTANCE_DEFAULT, Visibility: enums.TEAM_VISIBILITY_PRIVATE,
	}}, teammod.WithSkipCache()); err != nil {
		t.Fatal(err)
	}
	code := "OTHR" + strings.ToUpper(other.String()[:6])
	if _, err := hs.core.DB().Exec("INSERT INTO `invite` (`id`,`team_uuid`,`code`,`uses`,`status`) VALUES (?,?,?,?,?)",
		uuid.Must(uuid.NewV4()).String(), other.String(), code, 0, enums.INVITE_STATUS_ACTIVE); err != nil {
		t.Fatal(err)
	}
	res, _, err := hs.h.JoinTeam(context.Background(), nil, JoinTeamParams{JoinCode: code, MemberName: "Test", AgentLabel: "test", ClientKey: "client-t"})
	if err != nil {
		t.Fatal(err)
	}
	var joined JoinTeamResult
	decodeResult(t, res, &joined)
	stranger := hs.contractAgentFor(t, hs.ctxForToken(t, joined.Token), "Test")

	env, _ := hs.publish(t, stranger, login(t, "consumes", "", `{"token!":"string","expires_at!":"timestamp"}`))
	if len(env.Conflicts) != 0 {
		t.Fatalf("another team's assertion raised %+v", env.Conflicts)
	}
	var n sql.NullInt64
	_ = hs.core.DB().QueryRow("SELECT COUNT(*) FROM `conflict`").Scan(&n)
	if n.Int64 != 0 {
		t.Errorf("%d conflicts across both teams, want 0", n.Int64)
	}
	if c := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `contract` WHERE `key_norm` = ?", "http_endpoint:post /api/login"); c != 2 {
		t.Errorf("%d contracts for the key across teams, want 2", c)
	}
}
