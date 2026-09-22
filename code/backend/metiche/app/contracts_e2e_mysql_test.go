package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/sweeper"
	"github.com/mklfarha/metiche/backend/enums"
)

// Contracts end to end through the REAL wiring: two agents with their own
// tokens publish over /v1/mcp, the board reads the result over REST, the
// sweeper raises contract_unclaimed on a test clock, and every conflict
// settles itself. Every name is a test name.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&interpolateParams=true' go test -p 1 ./app/ -run Contracts -v

type publishAnswer struct {
	Key       string `json:"key"`
	Note      string `json:"note"`
	Conflicts []struct {
		Key             string   `json:"key"`
		Kind            string   `json:"kind"`
		Severity        string   `json:"severity"`
		With            string   `json:"with"`
		Contract        string   `json:"contract"`
		AtFault         string   `json:"at_fault"`
		Fields          []string `json:"fields"`
		SuggestedAction string   `json:"suggested_action"`
	} `json:"conflicts"`
	Pending struct {
		Instructions int `json:"instructions"`
		Conflicts    int `json:"conflicts"`
	} `json:"pending"`
}

func (w *inviteWorld) publish(cs *mcp.ClientSession, args map[string]any) (publishAnswer, string) {
	w.t.Helper()
	isErr, text := w.tool(cs, "publish_contract", args)
	if isErr {
		w.t.Fatalf("publish_contract %v: %s", args, text)
	}
	var out publishAnswer
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		w.t.Fatalf("publish_contract answer %s: %v", text, err)
	}
	return out, text
}

func (w *inviteWorld) sweep(at time.Time) sweeper.Report {
	w.t.Helper()
	s := sweeper.New(w.core, zap.NewNop(), sweeper.Options{Clock: func() time.Time { return at }})
	rep, err := s.RunOnce(context.Background())
	if err != nil || len(rep.Errors) > 0 {
		w.t.Fatalf("sweeper: %v %v", err, rep.Errors)
	}
	return rep
}

type teamConflict struct {
	key, rule, note        string
	kind, status, resolved int
}

func (w *inviteWorld) conflictsOf(tm testTeam) map[string]teamConflict {
	w.t.Helper()
	rows, err := w.db.Query("SELECT `key`, COALESCE(`detector_rule`, ''), COALESCE(`resolution_note`, ''), `kind`, `status`, COALESCE(`resolution`, 0) "+
		"FROM `conflict` WHERE `team_uuid` = ?", tm.id)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]teamConflict{}
	for rows.Next() {
		var c teamConflict
		if err := rows.Scan(&c.key, &c.rule, &c.note, &c.kind, &c.status, &c.resolved); err != nil {
			w.t.Fatal(err)
		}
		out[c.key] = c
	}
	return out
}

func TestContractsEndToEndOverMCP(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the login API"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})
	pub := func(session, role, key string, request, response any) map[string]any {
		m := map[string]any{"session_key": session, "team_slug": tm.slug, "key": key, "role": role}
		if request != nil {
			m["request"] = request
		}
		if response != nil {
			m["response"] = response
		}
		return m
	}

	// ── 1. Ana produces; Bob consumes with a response field Ana does not return.
	w.publish(csA, pub(sa.Key, "produces", "POST /api/login",
		map[string]any{"email!": "string", "password!": "string"}, map[string]any{"token!": "string"}))
	got, raw := w.publish(csB, pub(sb.Key, "consumes", "POST /api/login",
		map[string]any{"email": "string", "password": "string"}, map[string]any{"token!": "string", "expires_at!": "timestamp"}))
	t.Logf("Bob's publish_contract response over /v1/mcp:\n%s", raw)
	if len(got.Conflicts) != 1 || got.Conflicts[0].Kind != "contract_mismatch" || got.Conflicts[0].AtFault != "producer" ||
		got.Conflicts[0].Severity != "high" || !strings.Contains(got.Conflicts[0].With, "Ana") {
		t.Fatalf("Bob's response does not carry the mismatch: %+v", got.Conflicts)
	}
	mismatchKey := got.Conflicts[0].Key

	// ── 2. Ana hears on a bare heartbeat, and the notice says what to add.
	isErr, hb := w.tool(csA, "heartbeat", map[string]any{"session_key": sa.Key, "team_slug": tm.slug})
	if isErr || !strings.Contains(hb, `"instructions":1`) {
		t.Fatalf("Ana's heartbeat: %s", hb)
	}
	_, instr := w.tool(csA, "get_instructions", map[string]any{"session_key": sa.Key, "team_slug": tm.slug})
	t.Logf("Ana's get_instructions:\n%s", instr)
	if !strings.Contains(instr, "conflict_notice") || !strings.Contains(instr, "expires_at") || !strings.Contains(instr, mismatchKey) {
		t.Fatalf("Ana's notice: %s", instr)
	}

	// ── 3. The board reads real contracts.
	var board struct {
		Contracts []struct {
			Key       string `json:"key"`
			Agreement string `json:"agreement"`
			Produces  []struct {
				SessionKey string `json:"session_key"`
				FieldCount int    `json:"field_count"`
				Fields     []struct {
					Path, Type, Direction string
					Required              bool
				} `json:"fields"`
			} `json:"produces"`
			Consumes []struct {
				SessionKey string `json:"session_key"`
			} `json:"consumes"`
			Issues []struct {
				Kind, Path, Severity, Producer, Consumer string
			} `json:"issues"`
		} `json:"contracts"`
	}
	body := w.get(ana, "/v1/teams/"+tm.slug+"/contracts", &board)
	t.Logf("GET /v1/teams/%s/contracts:\n%s", tm.slug, body)
	if len(board.Contracts) != 1 {
		t.Fatalf("board contracts = %+v", board.Contracts)
	}
	c := board.Contracts[0]
	if c.Key != "POST /api/login" || c.Agreement != "mismatch" || len(c.Produces) != 1 || c.Produces[0].SessionKey != sa.Key ||
		len(c.Produces[0].Fields) != 3 || c.Produces[0].FieldCount != 3 || len(c.Consumes) != 1 || c.Consumes[0].SessionKey != sb.Key {
		t.Fatalf("board contract = %+v", c)
	}
	if len(c.Issues) != 1 || c.Issues[0].Kind != "missing_out" || c.Issues[0].Path != "expires_at" ||
		c.Issues[0].Producer != sa.Key || c.Issues[0].Consumer != sb.Key {
		t.Fatalf("board issues = %+v", c.Issues)
	}
	var open struct {
		Conflicts []struct {
			Key         string `json:"key"`
			Kind        string `json:"kind"`
			ContractKey string `json:"contract_key"`
		} `json:"conflicts"`
	}
	w.get(ana, "/v1/teams/"+tm.slug+"/conflicts", &open)
	if len(open.Conflicts) != 1 || open.Conflicts[0].Kind != "contract_mismatch" || open.Conflicts[0].ContractKey != "POST /api/login" {
		t.Fatalf("open conflicts = %+v", open.Conflicts)
	}

	// ── 4. Nobody is building GET /api/legs: the sweeper says so after the
	// project's cadence threshold (hackathon: 5 minutes), not before.
	w.exec("UPDATE `project` SET `cadence` = ? WHERE `team_uuid` = ?", enums.PROJECT_CADENCE_HACKATHON, tm.id)
	got, _ = w.publish(csB, pub(sb.Key, "consumes", "GET /api/legs", nil, map[string]any{"legs!": "json"}))
	if !strings.Contains(got.Note, "nobody produces it yet") {
		t.Errorf("consumer note = %q", got.Note)
	}
	now := time.Now().UTC()
	w.sweep(now.Add(4 * time.Minute))
	if n := w.count("SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ? AND `kind` = ?", tm.id, enums.CONFLICT_KIND_CONTRACT_UNCLAIMED); n != 0 {
		t.Fatalf("contract_unclaimed raised after 4 minutes on a hackathon project")
	}
	w.sweep(now.Add(6 * time.Minute))
	if n := w.count("SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ? AND `kind` = ? AND `status` = ?",
		tm.id, enums.CONFLICT_KIND_CONTRACT_UNCLAIMED, enums.CONFLICT_STATUS_OPEN); n != 1 {
		t.Fatalf("%d open contract_unclaimed after 6 minutes, want 1", n)
	}
	isErr, hb = w.tool(csB, "get_instructions", map[string]any{"session_key": sb.Key, "team_slug": tm.slug})
	t.Logf("Bob's unclaimed notice:\n%s", hb)
	if isErr || !strings.Contains(hb, "GET /api/legs") || !strings.Contains(hb, `"suggested_action":"Nobody produces GET /api/legs yet`) || strings.Contains(hb, "split the file") {
		t.Fatalf("Bob's unclaimed notice: %s", hb)
	}

	// ── 5. Ana produces it: the unclaimed conflict settles as converged.
	got, _ = w.publish(csA, pub(sa.Key, "produces", "GET /api/legs", nil, map[string]any{"legs!": "json", "count": "int"}))
	if len(got.Conflicts) != 0 {
		t.Fatalf("producing GET /api/legs raised %+v", got.Conflicts)
	}
	for _, cf := range w.conflictsOf(tm) {
		if cf.kind == int(enums.CONFLICT_KIND_CONTRACT_UNCLAIMED) {
			t.Logf("unclaimed settled: %s", cf.note)
			if cf.status != int(enums.CONFLICT_STATUS_RESOLVED) || cf.resolved != int(enums.CONFLICT_RESOLUTION_CONVERGED) ||
				!strings.Contains(cf.note, "now produces GET /api/legs") {
				t.Fatalf("unclaimed after a producer appeared: %+v", cf)
			}
		}
	}

	// ── 6. Ana adds the field: the mismatch settles as converged.
	w.publish(csA, pub(sa.Key, "produces", "POST /api/login",
		map[string]any{"email!": "string", "password!": "string"}, map[string]any{"token!": "string", "expires_at": "timestamp"}))
	if cf := w.conflictsOf(tm)[mismatchKey]; cf.status != int(enums.CONFLICT_STATUS_RESOLVED) || cf.resolved != int(enums.CONFLICT_RESOLUTION_CONVERGED) {
		t.Fatalf("mismatch after converging: %+v", cf)
	} else {
		t.Logf("mismatch settled: %s", cf.note)
	}
	body = w.get(ana, "/v1/teams/"+tm.slug+"/contracts", &board)
	for _, bc := range board.Contracts {
		if bc.Key == "POST /api/login" && (bc.Agreement != "agreed" || len(bc.Issues) != 0) {
			t.Fatalf("board after converging: %+v", bc)
		}
	}

	// ── 7. A new disagreement, then Bob's session is abandoned by the sweeper:
	// his assertions stop counting and the conflict is cleared by metiche.
	got, _ = w.publish(csB, pub(sb.Key, "consumes", "GET /api/legs", nil, map[string]any{"legs!": "json", "eta!": "timestamp"}))
	if len(got.Conflicts) != 1 {
		t.Fatalf("second mismatch: %+v", got.Conflicts)
	}
	second := got.Conflicts[0].Key
	w.sweep(time.Now().UTC().Add(30 * time.Minute))
	cf := w.conflictsOf(tm)[second]
	t.Logf("after abandonment: %s", cf.note)
	if cf.status != int(enums.CONFLICT_STATUS_RESOLVED) || cf.resolved != int(enums.CONFLICT_RESOLUTION_SUPERSEDED) ||
		!strings.HasPrefix(cf.note, "Cleared by metiche") {
		t.Fatalf("mismatch after Bob's session was abandoned: %+v", cf)
	}
	if n := w.count("SELECT COUNT(*) FROM `contract_assertion` WHERE `team_uuid` = ? AND `status` = ?", tm.id, enums.ASSERTION_STATUS_ACTIVE); n != 0 {
		t.Errorf("%d active assertions after both sessions were abandoned, want 0", n)
	}

	// ── 8. Conflict history lists the contract kinds, and only live kinds.
	var hist struct {
		Conflicts []struct{ Key, Kind, Resolution string } `json:"conflicts"`
		Kinds     []string                                 `json:"kinds"`
	}
	body = w.get(ana, "/v1/teams/"+tm.slug+"/conflicts/history", &hist)
	// decision_contradiction joined the live kinds with report_judgement
	// (docs/DECISIONS.md §5.1), and duplicate_work with the duplicate
	// reviewer (docs/DUPLICATES.md §5.1); stale_base is still detected by
	// nothing and is still absent.
	if strings.Join(hist.Kinds, ",") != "path_overlap,contract_mismatch,contract_unclaimed,contract_naming_variant,decision_contradiction,duplicate_work" {
		t.Fatalf("history kinds = %v", hist.Kinds)
	}
	kinds := map[string]bool{}
	for _, h := range hist.Conflicts {
		kinds[h.Kind] = true
	}
	if !kinds["contract_mismatch"] || !kinds["contract_unclaimed"] {
		t.Fatalf("history = %s", body)
	}
	var filtered struct {
		Conflicts []struct{ Kind string } `json:"conflicts"`
	}
	w.get(ana, "/v1/teams/"+tm.slug+"/conflicts/history?kind=contract_unclaimed", &filtered)
	if len(filtered.Conflicts) != 1 || filtered.Conflicts[0].Kind != "contract_unclaimed" {
		t.Fatalf("history filtered to contract_unclaimed = %+v", filtered.Conflicts)
	}
}
