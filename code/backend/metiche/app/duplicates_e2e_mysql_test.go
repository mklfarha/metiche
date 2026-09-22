package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/app/sweeper"
	"github.com/mklfarha/metiche/backend/enums"
)

// Duplicate work end to end through the REAL wiring (docs/DUPLICATES.md §7.4,
// Wave C): two people, two agents, each with its own agent token, over
// /v1/mcp, the board's REST reads as a signed-in viewer, and the sweeper
// driven on a test clock. Every summary is a hackathon summary and no plan
// carries an external_ref unless the scenario is about one.
//
// One world serves every scenario, so the team-lock-hold histogram the server
// keeps covers the whole run; each scenario has its own team. Sweeper passes
// are global, so every assertion reads rows scoped to the scenario's own team,
// never the pass's report counts.
//
// Every name is a test name and every secret is minted at run time.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&multiStatements=true&interpolateParams=true' \
//	  go test -p 1 ./app/ -run TestDuplicatesEndToEnd -v

const (
	dupAnaPlan = "add login page"
	dupBobPlan = "build the login screen"
	dupAnaPath = "web/src/routes/login.tsx"
	dupBobPath = "web/src/pages/Login.tsx"
	// dupBobShown is how get_review_context and the board show Bob's path:
	// the claim's normalized pattern, lowercased on a case-insensitive
	// project (the default), not the spelling the agent sent. Reported as a
	// finding; pinned here so a change is visible.
	dupBobShown = "web/src/pages/login.tsx"
	dupRescoped = "add password strength meter"

	dupConflictRationale   = "both build the login page; the other plan already has the route"
	dupNoConflictRationale = "now a password strength meter, not the login page"

	// §3.0: the notice grace on a hackathon project, EscalationBudget/5, and
	// the escalation budget itself.
	dupGrace  = 2 * time.Minute
	dupBudget = 10 * time.Minute

	// instructionTextChars and duplicateQuoteChars, as app/mcp holds them.
	dupInstructionChars = 200
	dupQuoteChars       = 60
)

// ─────────────────────────────────────────────
// Wire shapes
// ─────────────────────────────────────────────

type dupNotice struct {
	Key             string `json:"key"`
	Kind            string `json:"kind"`
	Severity        string `json:"severity"`
	With            string `json:"with"`
	DuplicateOf     string `json:"duplicate_of"`
	Decision        string `json:"decision"`
	AtFault         string `json:"at_fault"`
	SuggestedAction string `json:"suggested_action"`
}

type dupAnswer struct {
	OK      bool            `json:"ok"`
	Key     string          `json:"key"`
	Note    string          `json:"note"`
	Review  json.RawMessage `json:"review"`
	Pending struct {
		Instructions int `json:"instructions"`
		Conflicts    int `json:"conflicts"`
		Reviews      int `json:"reviews"`
	} `json:"pending"`
	Conflicts []dupNotice `json:"conflicts"`
}

type dupBlockPair struct {
	PairKey   string `json:"pair_key"`
	Kind      string `json:"kind"`
	Plan      string `json:"plan"`
	With      string `json:"with"`
	Summary   string `json:"summary"`
	Decision  string `json:"decision"`
	Statement string `json:"statement"`
	Why       string `json:"why"`
}

type dupBlock struct {
	Pairs      []dupBlockPair `json:"pairs"`
	More       int            `json:"more"`
	AnswerWith string         `json:"answer_with"`
}

type dupReviewPlan struct {
	Key        string   `json:"key"`
	Summary    string   `json:"summary"`
	Paths      []string `json:"paths"`
	Revision   int64    `json:"revision"`
	Who        string   `json:"who"`
	SessionKey string   `json:"session_key"`
	Status     string   `json:"status"`
}

type dupReviewContext struct {
	Note    string `json:"note"`
	Reviews []struct {
		PairKey  string         `json:"pair_key"`
		Kind     string         `json:"kind"`
		Question string         `json:"question"`
		Why      []string       `json:"why"`
		Plan     dupReviewPlan  `json:"plan"`
		Other    *dupReviewPlan `json:"other"`
		Decision *struct {
			Key string `json:"key"`
		} `json:"decision"`
	} `json:"reviews"`
}

// dupBoardConflicts is GET /conflicts and /conflicts/history with §9.3's
// duplicate fields.
type dupBoardConflicts struct {
	Conflicts []dupBoardConflict `json:"conflicts"`
}

type dupBoardConflict struct {
	Key             string   `json:"key"`
	Kind            string   `json:"kind"`
	Severity        string   `json:"severity"`
	Status          string   `json:"status"`
	DetectedBy      string   `json:"detected_by"`
	DetectorRule    string   `json:"detector_rule"`
	SuggestedAction string   `json:"suggested_action"`
	OccurrenceCount int64    `json:"occurrence_count"`
	FirstDetectedAt string   `json:"first_detected_at"`
	LastDetectedAt  string   `json:"last_detected_at"`
	Resolution      string   `json:"resolution"`
	ResolutionNote  string   `json:"resolution_note"`
	ResolvedAt      string   `json:"resolved_at"`
	Paths           []string `json:"paths"`
	Plans           []struct {
		Key     string `json:"key"`
		Who     string `json:"who"`
		Summary string `json:"summary"`
		Path    string `json:"path"`
		Yields  bool   `json:"yields"`
	} `json:"plans"`
	Signals      []string `json:"signals"`
	IssueRef     string   `json:"issue_ref"`
	JudgeNote    string   `json:"judge_note"`
	EscalatedAt  string   `json:"escalated_at"`
	DecisionKey  string   `json:"decision_key"`
	Participants []struct {
		SessionKey  string `json:"session_key"`
		MemberName  string `json:"member_name"`
		AgentLabel  string `json:"agent_label"`
		Role        string `json:"role"`
		SubjectKind string `json:"subject_kind"`
	} `json:"participants"`
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func (w *inviteWorld) dupCall(cs *mcp.ClientSession, name string, args map[string]any) (dupAnswer, string) {
	w.t.Helper()
	isErr, text := w.tool(cs, name, args)
	if isErr {
		w.t.Fatalf("%s %v: %s", name, args, text)
	}
	var out dupAnswer
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		w.t.Fatalf("%s answer %s: %v", name, text, err)
	}
	return out, text
}

func (w *inviteWorld) dupDeclare(cs *mcp.ClientSession, tm testTeam, sessionKey, summary string, paths []string, extra map[string]any) (dupAnswer, string) {
	w.t.Helper()
	args := map[string]any{
		"session_key": sessionKey, "team_slug": tm.slug,
		"summary": summary, "paths": paths, "mode": "write",
	}
	for k, v := range extra {
		args[k] = v
	}
	return w.dupCall(cs, "declare_intent", args)
}

func (w *inviteWorld) dupUpdate(cs *mcp.ClientSession, tm testTeam, sessionKey, intentKey string, extra map[string]any) (dupAnswer, string) {
	w.t.Helper()
	args := map[string]any{"session_key": sessionKey, "team_slug": tm.slug, "intent_key": intentKey}
	for k, v := range extra {
		args[k] = v
	}
	return w.dupCall(cs, "update_intent", args)
}

func (w *inviteWorld) dupJudge(cs *mcp.ClientSession, tm testTeam, sessionKey, pairKey, verdict string, extra map[string]any) (dupAnswer, string) {
	w.t.Helper()
	args := map[string]any{"session_key": sessionKey, "team_slug": tm.slug, "pair_key": pairKey, "verdict": verdict}
	for k, v := range extra {
		args[k] = v
	}
	return w.dupCall(cs, "report_judgement", args)
}

func (w *inviteWorld) dupReview(cs *mcp.ClientSession, tm testTeam, sessionKey string) (dupReviewContext, string) {
	w.t.Helper()
	isErr, text := w.tool(cs, "get_review_context", map[string]any{"session_key": sessionKey, "team_slug": tm.slug})
	if isErr {
		w.t.Fatalf("get_review_context: %s", text)
	}
	var out dupReviewContext
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		w.t.Fatalf("get_review_context answer %s: %v", text, err)
	}
	return out, text
}

func (w *inviteWorld) dupHeartbeat(cs *mcp.ClientSession, tm testTeam, sessionKey string) (dupAnswer, string) {
	w.t.Helper()
	return w.dupCall(cs, "heartbeat", map[string]any{"session_key": sessionKey, "team_slug": tm.slug})
}

func dupBlockOf(t *testing.T, env dupAnswer) dupBlock {
	t.Helper()
	if len(env.Review) == 0 {
		t.Fatalf("the response carries no review block: note %q", env.Note)
	}
	var b dupBlock
	if err := json.Unmarshal(env.Review, &b); err != nil {
		t.Fatalf("review block is not JSON: %v (%s)", err, env.Review)
	}
	return b
}

// dupHackathon puts every project of the team on the hackathon cadence:
// grace 2m, budget 10m.
func (w *inviteWorld) dupHackathon(tm testTeam) {
	w.exec("UPDATE `project` SET `cadence` = ? WHERE `team_uuid` = ?", enums.PROJECT_CADENCE_HACKATHON, tm.id)
}

// dupStaySessions keeps every session live across the sweeper's test clock,
// which runs minutes ahead of the heartbeats the agents really sent.
func dupStaySessions() sweeper.Options {
	return sweeper.Options{SessionStale: time.Hour, SessionAbandoned: 2 * time.Hour}
}

type dupConflictRow struct {
	id, key, rule, note, action string
	kind, status, resolution    int64
	sev                         int64
	lastDetected                time.Time
	notifiedAt, escalatedAt     sql.NullTime
	resolvedAt                  sql.NullTime
	occurrences                 int64
}

func (w *inviteWorld) dupConflict(tm testTeam, key string) dupConflictRow {
	w.t.Helper()
	var c dupConflictRow
	if err := w.db.QueryRow(
		"SELECT `id`, `key`, COALESCE(`detector_rule`, ''), COALESCE(`resolution_note`, ''), COALESCE(`suggested_action`, ''), "+
			"`kind`, `status`, COALESCE(`resolution`, 0), `severity`, COALESCE(`last_detected_at`, `first_detected_at`), "+
			"`notified_at`, `escalated_at`, `resolved_at`, `occurrence_count` "+
			"FROM `conflict` WHERE `team_uuid` = ? AND `key` = ?", tm.id, key).
		Scan(&c.id, &c.key, &c.rule, &c.note, &c.action, &c.kind, &c.status, &c.resolution, &c.sev, &c.lastDetected,
			&c.notifiedAt, &c.escalatedAt, &c.resolvedAt, &c.occurrences); err != nil {
		w.t.Fatalf("conflict %s: %v", key, err)
	}
	c.lastDetected = c.lastDetected.UTC()
	return c
}

type dupIntentRow struct {
	id              string
	status          enums.IntentStatus
	wordingRevision int64
	endedAt         sql.NullTime
}

func (w *inviteWorld) dupIntent(tm testTeam, key string) dupIntentRow {
	w.t.Helper()
	var (
		r  dupIntentRow
		st int64
	)
	if err := w.db.QueryRow("SELECT `id`, `status`, `wording_revision`, `ended_at` FROM `intent` WHERE `team_uuid` = ? AND `key` = ?",
		tm.id, key).Scan(&r.id, &st, &r.wordingRevision, &r.endedAt); err != nil {
		w.t.Fatalf("intent %s: %v", key, err)
	}
	r.status = enums.IntentStatus(st)
	return r
}

// dupJudgements counts one team's duplicate_work pairs, optionally in one status.
func (w *inviteWorld) dupJudgements(tm testTeam, status ...enums.JudgementStatus) int {
	if len(status) == 0 {
		return w.count("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `kind` = ?", tm.id, enums.CONFLICT_KIND_DUPLICATE_WORK)
	}
	return w.count("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `kind` = ? AND `status` = ?",
		tm.id, enums.CONFLICT_KIND_DUPLICATE_WORK, status[0])
}

// dupInstructionsTo counts the instructions ever addressed to one session,
// optionally of one status.
func (w *inviteWorld) dupInstructionsTo(tm testTeam, sessionKey string, status ...enums.InstructionStatus) int {
	q := "SELECT COUNT(*) FROM `instruction` i JOIN `session` s ON s.`id` = i.`target_session_uuid` WHERE s.`team_uuid` = ? AND s.`key` = ?"
	args := []any{tm.id, sessionKey}
	if len(status) > 0 {
		q += " AND i.`status` = ?"
		args = append(args, status[0])
	}
	return w.count(q, args...)
}

// dupYieldAction is §4.10's suggested action for the yield side, another
// member's plan.
func dupYieldAction(a, who, status, age, summary, b, member, aSession string) string {
	return fmt.Sprintf("%s (%s, %s %s) is already building this: \"%s\". Stop before you edit: mark %s superseded with update_intent, "+
		"or re-scope it to a different part and update_intent the summary, which asks you to judge once more. "+
		"If your plan should be the one that continues, settle that with %s's agent (%s) first.",
		a, who, status, age, summary, b, member, aSession)
}

// dupIncumbentAction is §4.10's incumbent notice action: the quoted rationale
// gives way first, at most 60, and the folded path conflict is named only when
// it fits in the 200 characters get_instructions shows.
func dupIncumbentAction(judge, b, rationale, bSession, pathKey string) string {
	const shape = "%s judged its plan %s is the same work as yours: \"%s\", and was told to stop. Carry on; if its part should stay, settle the split with %s."
	build := func(suffix string) string {
		room := dupInstructionChars - utf8.RuneCountInString(fmt.Sprintf(shape, judge, b, "", bSession)+suffix)
		if room > dupQuoteChars {
			room = dupQuoteChars
		}
		return fmt.Sprintf(shape, judge, b, decClip(rationale, room), bSession) + suffix
	}
	if pathKey != "" {
		if s := build(" (also " + pathKey + ")"); utf8.RuneCountInString(s) <= dupInstructionChars {
			return s
		}
	}
	return decClip(build(""), dupInstructionChars)
}

// dupFind returns the board's copy of one conflict.
func dupFind(t *testing.T, list dupBoardConflicts, key string) dupBoardConflict {
	t.Helper()
	for _, c := range list.Conflicts {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("%s is not on the board: %+v", key, list.Conflicts)
	return dupBoardConflict{}
}

func dupRFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// dupNoNullPaths fails when get_instructions shows any instruction a path
// that is the JSON null of the evidence rendered as text.
func dupNoNullPaths(t *testing.T, what, raw string) {
	t.Helper()
	var out struct {
		Instructions []struct {
			Key   string   `json:"key"`
			Paths []string `json:"paths"`
		} `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	for _, i := range out.Instructions {
		for _, p := range i.Paths {
			if p == "null" || p == "" {
				t.Errorf("%s: instruction %s shows paths %q; a duplicate with no shared file has none", what, i.Key, i.Paths)
			}
		}
	}
}

// dupPair is the common start of scenarios 1, 2, 3 and 7: Ana declares "add
// login page", Bob declares "build the login screen", and Bob's model judges
// the pair the same work.
type dupPair struct {
	tm                   testTeam
	ana, bob             person
	csA, csB             *mcp.ClientSession
	sa, sb               startAnswer
	anaIntent, bobIntent string
	pairKey              string
	cf                   string
	judged               dupAnswer
}

func (w *inviteWorld) dupStart(t *testing.T, severity string) dupPair {
	t.Helper()
	var p dupPair
	p.tm = w.team(enums.TEAM_VISIBILITY_PRIVATE)
	p.ana, p.bob = w.join(p.tm, "Ana"), w.join(p.tm, "Bob")
	p.csA, p.csB = w.connect(p.ana.token), w.connect(p.bob.token)
	p.sa, _ = w.start(p.csA, p.tm.slug, map[string]any{"goal": "the login page"})
	p.sb, _ = w.start(p.csB, p.tm.slug, map[string]any{"goal": "the login screen"})
	w.dupHackathon(p.tm)

	a, _ := w.dupDeclare(p.csA, p.tm, p.sa.Key, dupAnaPlan, []string{dupAnaPath}, nil)
	p.anaIntent = a.Key
	b, _ := w.dupDeclare(p.csB, p.tm, p.sb.Key, dupBobPlan, []string{dupBobPath}, nil)
	p.bobIntent = b.Key
	block := dupBlockOf(t, b)
	if len(block.Pairs) != 1 || block.Pairs[0].Kind != "duplicate_work" || block.Pairs[0].Plan != p.anaIntent {
		t.Fatalf("Bob was not handed the pair: %+v", block)
	}
	p.pairKey = block.Pairs[0].PairKey
	extra := map[string]any{"confidence": 0.9, "rationale": dupConflictRationale}
	if severity != "" {
		extra["severity"] = severity
	}
	p.judged, _ = w.dupJudge(p.csB, p.tm, p.sb.Key, p.pairKey, "conflict", extra)
	if len(p.judged.Conflicts) != 1 {
		t.Fatalf("no duplicate conflict was raised: %+v", p.judged)
	}
	p.cf = p.judged.Conflicts[0].Key
	return p
}

// ─────────────────────────────────────────────
// The run
// ─────────────────────────────────────────────

func TestDuplicatesEndToEnd(t *testing.T) {
	w := newInviteWorld(t)
	sub := func(name string, fn func(t *testing.T)) {
		t.Run(name, func(st *testing.T) {
			prev := w.t
			w.t = st
			defer func() { w.t = prev }()
			fn(st)
		})
	}

	sub("1_yield_inside_the_grace", func(t *testing.T) { dupScenarioYield(t, w) })
	sub("2_nobody_yields", func(t *testing.T) { dupScenarioNobodyYields(t, w) })
	sub("3_rescope", func(t *testing.T) { dupScenarioRescope(t, w) })
	sub("4_different_parts", func(t *testing.T) { dupScenarioDifferentParts(t, w) })
	sub("5_same_issue_across_projects", func(t *testing.T) { dupScenarioSameIssue(t, w) })
	sub("6_path_overlap_too", func(t *testing.T) { dupScenarioPathOverlap(t, w) })
	sub("7_vanished_agent", func(t *testing.T) { dupScenarioVanished(t, w) })
	sub("8_decisions_alongside", func(t *testing.T) { dupScenarioDecisions(t, w) })

	// The team-lock hold over the whole run (every scenario above shares
	// this server and its histogram). PLAN.md's tripwire is p99 > 25ms.
	r := w.call(http.MethodGet, "/v1/metrics/mcp", "", nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /v1/metrics/mcp = %d %s", r.status, r.body)
	}
	var m struct {
		LockHold struct {
			Count uint64  `json:"count"`
			P50   float64 `json:"p50_ms"`
			P95   float64 `json:"p95_ms"`
			P99   float64 `json:"p99_ms"`
			Max   float64 `json:"max_ms"`
			Mean  float64 `json:"mean_ms"`
		} `json:"lock_hold"`
	}
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("metrics: %v (%s)", err, r.body)
	}
	t.Logf("metiche_team_lock_hold_ms over the whole run: count %d, mean %.2f, p50 ≤ %.1f, p95 ≤ %.1f, p99 ≤ %.1f, max %.2f",
		m.LockHold.Count, m.LockHold.Mean, m.LockHold.P50, m.LockHold.P95, m.LockHold.P99, m.LockHold.Max)
	if m.LockHold.Count == 0 {
		t.Error("the lock-hold histogram saw no writes: the run did not go through the handler")
	}
	if m.LockHold.P99 > 25 {
		t.Errorf("lock-hold p99 ≤ %.1fms is past the 25ms tripwire", m.LockHold.P99)
	}
}

// ─────────────────────────────────────────────
// 1. Yield inside the grace: Ana hears nothing
// ─────────────────────────────────────────────

func dupScenarioYield(t *testing.T, w *inviteWorld) {
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the login page"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})
	w.dupHackathon(tm)

	a, aRaw := w.dupDeclare(csA, tm, sa.Key, dupAnaPlan, []string{dupAnaPath}, nil)
	t.Logf("Ana's declare_intent over /v1/mcp:\n%s", aRaw)
	anaIntent := a.Key
	if len(a.Review) != 0 || a.Pending.Reviews != 0 {
		t.Fatalf("the first plan on the project was asked to judge something: %s", aRaw)
	}

	// ── Bob's inline review pair. ─────────────────────────────────────────
	b, bRaw := w.dupDeclare(csB, tm, sb.Key, dupBobPlan, []string{dupBobPath}, nil)
	t.Logf("Bob's declare_intent over /v1/mcp:\n%s", bRaw)
	bobIntent := b.Key
	if b.Pending.Reviews != 1 {
		t.Fatalf("Bob's pending.reviews = %d, want 1", b.Pending.Reviews)
	}
	block := dupBlockOf(t, b)
	if len(block.Pairs) != 1 || block.More != 0 || block.AnswerWith != "report_judgement" {
		t.Fatalf("review block = %+v", block)
	}
	pair := block.Pairs[0]
	if pair.Kind != "duplicate_work" || pair.Plan != anaIntent || pair.With != "Ana (test)" ||
		pair.Summary != dupAnaPlan || pair.Why != "words" || pair.Decision != "" {
		t.Errorf("the duplicate pair = %+v", pair)
	}
	if want := "; judge 1 pair(s) against your plan before you edit: see review, then report_judgement"; !strings.HasSuffix(b.Note, want) {
		t.Errorf("declare_intent note = %q, want it to end with %q", b.Note, want)
	}
	if n := w.count("SELECT COUNT(*) FROM `judgement` j JOIN `intent` i ON i.`id` = j.`subject_b_uuid` "+
		"JOIN `session` s ON s.`id` = j.`judge_session_uuid` WHERE j.`team_uuid` = ? AND j.`kind` = ? AND i.`key` = ? AND s.`key` = ?",
		tm.id, enums.CONFLICT_KIND_DUPLICATE_WORK, bobIntent, sb.Key); n != 1 {
		t.Errorf("%d pair(s) with Bob's plan as subject b and Bob as judge, want 1", n)
	}

	// ── Bob reads the context and reports conflict 0.9. ───────────────────
	rc, rcRaw := w.dupReview(csB, tm, sb.Key)
	t.Logf("Bob's get_review_context:\n%s", rcRaw)
	if len(rc.Reviews) != 1 || rc.Reviews[0].PairKey != pair.PairKey || rc.Reviews[0].Kind != "duplicate_work" {
		t.Fatalf("review context = %+v", rc.Reviews)
	}
	item := rc.Reviews[0]
	if want := fmt.Sprintf("Would carrying out %s build the same thing %s (Ana (test)) is already building — the same change, not just the same area?",
		bobIntent, anaIntent); item.Question != want {
		t.Errorf("question =\n  %q\nwant\n  %q", item.Question, want)
	}
	if len(item.Why) != 1 || item.Why[0] != "your summaries share words: login, screen" {
		t.Errorf("why = %q", item.Why)
	}
	if item.Plan.Key != bobIntent || item.Plan.Summary != dupBobPlan || strings.Join(item.Plan.Paths, ",") != dupBobShown {
		t.Errorf("plan = %+v", item.Plan)
	}
	if item.Other == nil || item.Other.Key != anaIntent || item.Other.Summary != dupAnaPlan || item.Other.Who != "Ana (test)" ||
		item.Other.SessionKey != sa.Key || strings.Join(item.Other.Paths, ",") != dupAnaPath {
		t.Errorf("other = %+v", item.Other)
	}

	jud, judRaw := w.dupJudge(csB, tm, sb.Key, pair.PairKey, "conflict", map[string]any{
		"confidence": 0.9, "rationale": dupConflictRationale,
	})
	t.Logf("Bob's report_judgement (conflict 0.9):\n%s", judRaw)
	if len(jud.Conflicts) != 1 {
		t.Fatalf("conflicts[] = %+v", jud.Conflicts)
	}
	cf := jud.Conflicts[0]
	if cf.Kind != "duplicate_work" || cf.Severity != "medium" || cf.With != "Ana (test)" ||
		cf.DuplicateOf != anaIntent || cf.AtFault != "later" || jud.Key != cf.Key {
		t.Errorf("conflict notice = %+v (envelope key %q)", cf, jud.Key)
	}
	anaRow := w.dupIntent(tm, anaIntent)
	wantAction := dupYieldAction(anaIntent, "Ana (test)", anaRow.status.String(), "just now", dupAnaPlan, bobIntent, "Ana", sa.Key)
	t.Logf("§4.10 yield-side suggested action, as the server produced it:\n  %s", cf.SuggestedAction)
	if cf.SuggestedAction != wantAction {
		t.Errorf("suggested_action =\n  %q\nwant\n  %q", cf.SuggestedAction, wantAction)
	}
	if want := fmt.Sprintf("judged %s against %s: same work (0.90) — read conflicts[] before you edit", bobIntent, anaIntent); jud.Note != want {
		t.Errorf("report_judgement note =\n  %q\nwant\n  %q", jud.Note, want)
	}
	row := w.dupConflict(tm, cf.Key)
	if row.action != cf.SuggestedAction {
		t.Errorf("stored suggested_action differs from the one Bob was shown:\n  %q\n  %q", row.action, cf.SuggestedAction)
	}
	// No instruction and no incumbent participant from report_judgement.
	if n := w.count("SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ?", row.id); n != 1 {
		t.Errorf("%d participant(s) on the fresh conflict, want only the initiator", n)
	}

	// ── The sweeper at grace − 1s: Ana is not interrupted. ────────────────
	w.decSweep(row.lastDetected.Add(dupGrace-time.Second), dupStaySessions())
	if n := w.dupInstructionsTo(tm, sa.Key); n != 0 {
		t.Fatalf("Ana got %d instruction(s) inside the grace", n)
	}
	if hb, _ := w.dupHeartbeat(csA, tm, sa.Key); hb.Pending.Instructions != 0 || hb.Pending.Conflicts != 0 || hb.Pending.Reviews != 0 {
		t.Errorf("Ana's heartbeat inside the grace: pending %+v, want all zero", hb.Pending)
	}

	// ── Bob yields inside the grace. ──────────────────────────────────────
	upd, updRaw := w.dupUpdate(csB, tm, sb.Key, bobIntent, map[string]any{"status": "superseded"})
	t.Logf("Bob's update_intent (superseded):\n%s", updRaw)
	if !upd.OK {
		t.Fatalf("update_intent: %s", updRaw)
	}
	row = w.dupConflict(tm, cf.Key)
	if row.status != int64(enums.CONFLICT_STATUS_RESOLVED) || row.resolution != int64(enums.CONFLICT_RESOLUTION_YIELDED) {
		t.Fatalf("conflict after Bob yielded: status %d resolution %d, want resolved/yielded", row.status, row.resolution)
	}
	bobRow := w.dupIntent(tm, bobIntent)
	at := row.resolvedAt.Time
	if bobRow.endedAt.Valid {
		at = bobRow.endedAt.Time
	}
	wantNote := fmt.Sprintf("Settled by the agents: %s marked %s superseded at %s, leaving %s (%s) to build it.",
		decSessionName(sb.Key), bobIntent, decClock(at), anaIntent, decSessionName(sa.Key))
	t.Logf("§4.10 YIELDED note, as the server produced it:\n  %s", row.note)
	if row.note != wantNote {
		t.Errorf("resolution note =\n  %q\nwant\n  %q", row.note, wantNote)
	}
	if n := w.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ? AND `structural` = 1",
		tm.id, enums.EVENT_KIND_CONFLICT_RESOLVED, row.id); n != 1 {
		t.Errorf("%d structural conflict_resolved event(s), want 1", n)
	}

	// ── After the grace the sweeper still tells Ana nothing. ──────────────
	w.decSweep(row.lastDetected.Add(dupGrace+time.Second), dupStaySessions())
	w.decSweep(row.lastDetected.Add(dupGrace+time.Minute), dupStaySessions())
	if hb, hbRaw := w.dupHeartbeat(csA, tm, sa.Key); hb.Pending.Instructions != 0 || hb.Pending.Conflicts != 0 || hb.Pending.Reviews != 0 {
		t.Errorf("Ana's heartbeat after the grace: %s", hbRaw)
	}

	// The board agrees with the row.
	var hist dupBoardConflicts
	body := w.get(ana, "/v1/teams/"+tm.slug+"/conflicts/history?kind=duplicate_work", &hist)
	t.Logf("GET /v1/teams/%s/conflicts/history?kind=duplicate_work:\n%s", tm.slug, body)
	hc := dupFind(t, hist, cf.Key)
	if hc.Status != "resolved" || hc.Resolution != "yielded" || hc.ResolutionNote != row.note || hc.Kind != "duplicate_work" {
		t.Errorf("history = %+v", hc)
	}

	// ── Across the whole run Ana was interrupted zero times. ──────────────
	if n := w.decInterruptions(tm, ana.member); n != 0 {
		t.Errorf("Ana was interrupted %d time(s); a judge that yields in the grace interrupts nobody", n)
	}
	if n := w.decInterruptions(tm, bob.member); n != 0 {
		t.Errorf("Bob was interrupted %d time(s); the judge is told in its own responses", n)
	}
}

// ─────────────────────────────────────────────
// 2. Nobody yields: the notice after the grace, then a person
// ─────────────────────────────────────────────

func dupScenarioNobodyYields(t *testing.T, w *inviteWorld) {
	p := w.dupStart(t, "high")
	tm, sa, sb := p.tm, p.sa, p.sb
	if p.judged.Conflicts[0].Severity != "high" {
		t.Fatalf("severity = %q, want high", p.judged.Conflicts[0].Severity)
	}
	row := w.dupConflict(tm, p.cf)
	detected := row.lastDetected
	opts := dupStaySessions()

	w.decSweep(detected.Add(dupGrace-time.Second), opts)
	if n := w.dupInstructionsTo(tm, sa.Key); n != 0 {
		t.Fatalf("Ana was told %d time(s) inside the grace", n)
	}

	// ── After the grace: once. ────────────────────────────────────────────
	noticeAt := detected.Add(dupGrace)
	w.decSweep(noticeAt, opts)
	w.decSweep(noticeAt.Add(30*time.Second), opts)
	if n := w.dupInstructionsTo(tm, sa.Key); n != 1 {
		t.Fatalf("Ana got %d instruction(s) after the grace and a second pass, want exactly 1", n)
	}
	if n := w.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ? AND `structural` = 1",
		tm.id, enums.EVENT_KIND_INSTRUCTION_RAISED, row.id); n != 1 {
		t.Errorf("%d structural instruction_raised event(s) for the notice, want 1", n)
	}
	hb, hbRaw := w.dupHeartbeat(p.csA, tm, sa.Key)
	t.Logf("Ana's heartbeat after the grace:\n%s", hbRaw)
	if hb.Pending.Instructions != 1 || hb.Pending.Conflicts != 1 || hb.Pending.Reviews != 0 {
		t.Errorf("Ana's pending = %+v, want 1 instruction, 1 conflict, 0 reviews", hb.Pending)
	}
	in, inRaw := w.decInstructions(p.csA, tm, sa.Key)
	t.Logf("Ana's get_instructions:\n%s", inRaw)
	if len(in.Instructions) != 1 {
		t.Fatalf("Ana got %d instructions", len(in.Instructions))
	}
	notice := in.Instructions[0]
	if notice.Kind != "conflict_notice" || notice.Ref != p.cf || notice.Severity != "high" || notice.From != "metiche" || notice.ReportBack {
		t.Errorf("Ana's notice = %+v", notice)
	}
	if want := fmt.Sprintf("%s (high) on %s", p.cf, p.anaIntent); notice.What != want {
		t.Errorf("notice what = %q, want %q", notice.What, want)
	}
	wantAction := dupIncumbentAction(decSessionName(sb.Key), p.bobIntent, dupConflictRationale, sb.Key, "")
	t.Logf("§4.5 incumbent notice, as the server produced it:\n  what:   %s\n  action: %s", notice.What, notice.SuggestedAction)
	if notice.SuggestedAction != wantAction {
		t.Errorf("notice action =\n  %q\nwant\n  %q", notice.SuggestedAction, wantAction)
	}
	if n := utf8.RuneCountInString(notice.SuggestedAction); n > dupInstructionChars {
		t.Errorf("the notice action is %d characters", n)
	}
	// A duplicate with no shared file has no path to show: the evidence's
	// overlap_path is JSON null, and it must not reach the agent as the
	// string "null".
	dupNoNullPaths(t, "Ana's notice", inRaw)
	row = w.dupConflict(tm, p.cf)
	if !row.notifiedAt.Valid {
		t.Error("notified_at was not stamped")
	}

	// ── Past the budget: a person is asked, once. ─────────────────────────
	// openSince is the latest of the detection and the participants'
	// notified_at, which is Ana's notice (§4.8).
	w.decSweep(noticeAt.Add(dupBudget-time.Second), opts)
	if r := w.dupConflict(tm, p.cf); r.escalatedAt.Valid {
		t.Fatalf("escalated a second before the budget: %v", r.escalatedAt.Time)
	}
	w.decSweep(noticeAt.Add(dupBudget), opts)
	w.decSweep(noticeAt.Add(dupBudget+5*time.Minute), opts)
	row = w.dupConflict(tm, p.cf)
	if !row.escalatedAt.Valid {
		t.Fatal("escalated_at was not set past the budget")
	}
	if n := w.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ? AND `structural` = 1",
		tm.id, enums.EVENT_KIND_CONFLICT_ESCALATED, row.id); n != 1 {
		t.Errorf("%d structural conflict_escalated event(s), want exactly 1", n)
	}
	var escSummary string
	_ = w.db.QueryRow("SELECT `summary` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		tm.id, enums.EVENT_KIND_CONFLICT_ESCALATED, row.id).Scan(&escSummary)
	if want := fmt.Sprintf("%s: a person was asked about %s / %s", p.cf, p.bobIntent, p.anaIntent); escSummary != want {
		t.Errorf("conflict_escalated summary = %q, want %q", escSummary, want)
	}
	if n := w.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ? AND `kind` = ? AND `ref_uuid` = ?",
		tm.id, enums.INSTRUCTION_KIND_QUESTION, row.id); n != 2 {
		t.Errorf("%d question(s) after three passes, want the 2 of one escalation", n)
	}
	qb, qbRaw := w.decInstructions(p.csB, tm, sb.Key)
	t.Logf("Bob's get_instructions after the escalation:\n%s", qbRaw)
	qa, qaRaw := w.decInstructions(p.csA, tm, sa.Key)
	t.Logf("Ana's get_instructions after the escalation:\n%s", qaRaw)
	wantYield := fmt.Sprintf("%s: %s duplicates %s (Ana), unsettled 10m. Ask your person: stop %s, or agree with Ana who keeps it? Then report_back their answer.",
		p.cf, p.bobIntent, p.anaIntent, p.bobIntent)
	wantIncumbent := fmt.Sprintf("%s: Bob's agent is building the same thing as your %s, unsettled 10m. Ask your person who keeps it: \"%s\". Then report_back their answer.",
		p.cf, p.anaIntent, dupAnaPlan)
	checkQuestion := func(who string, list decInstructionList, want string) {
		t.Helper()
		var got []string
		for _, i := range list.Instructions {
			if i.Kind == "question" {
				got = append(got, i.What)
				if i.Ref != p.cf || !i.ReportBack {
					t.Errorf("%s's question = %+v", who, i)
				}
			}
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s's question(s) = %q, want [%q]", who, got, want)
		}
		if len(got) == 1 {
			t.Logf("§4.10 %s question, as the server produced it (%d chars):\n  %s", who, utf8.RuneCountInString(got[0]), got[0])
		}
	}
	checkQuestion("Bob's (yield side)", qb, wantYield)
	checkQuestion("Ana's (incumbent)", qa, wantIncumbent)
	dupNoNullPaths(t, "Bob's question", qbRaw)
	dupNoNullPaths(t, "Ana's question", qaRaw)

	// ── REST: §9.3's shape, and the board agrees with the row. ────────────
	var open dupBoardConflicts
	body := w.get(p.ana, "/v1/teams/"+tm.slug+"/conflicts", &open)
	t.Logf("GET /v1/teams/%s/conflicts:\n%s", tm.slug, body)
	c := dupFind(t, open, p.cf)
	row = w.dupConflict(tm, p.cf)
	if c.Kind != "duplicate_work" || c.Severity != "high" || c.Status != "open" || c.DetectedBy != "agent" ||
		c.DetectorRule != "duplicate_work.words" || c.OccurrenceCount != row.occurrences {
		t.Errorf("board conflict head = %+v", c)
	}
	if c.SuggestedAction != row.action {
		t.Errorf("board suggested_action differs from the row:\n  board %q\n  row   %q", c.SuggestedAction, row.action)
	}
	if c.EscalatedAt != dupRFC3339(row.escalatedAt.Time) {
		t.Errorf("board escalated_at = %q, row %q", c.EscalatedAt, dupRFC3339(row.escalatedAt.Time))
	}
	if c.LastDetectedAt != dupRFC3339(row.lastDetected) {
		t.Errorf("board last_detected_at = %q, row %q", c.LastDetectedAt, dupRFC3339(row.lastDetected))
	}
	if strings.Join(c.Paths, ",") != dupBobShown+","+dupAnaPath {
		t.Errorf("board paths = %v, want [yield side's, incumbent's]", c.Paths)
	}
	if len(c.Plans) != 2 {
		t.Fatalf("board plans = %+v", c.Plans)
	}
	if pl := c.Plans[0]; pl.Key != p.anaIntent || pl.Who != "Ana (test)" || pl.Summary != dupAnaPlan || pl.Path != dupAnaPath || pl.Yields {
		t.Errorf("board plans[0] (incumbent) = %+v", pl)
	}
	if pl := c.Plans[1]; pl.Key != p.bobIntent || pl.Who != "Bob (test)" || pl.Summary != dupBobPlan || pl.Path != dupBobShown || !pl.Yields {
		t.Errorf("board plans[1] (Bob, yielding) = %+v", pl)
	}
	if strings.Join(c.Signals, "|") != "shared words: login, screen" || c.IssueRef != "" || c.DecisionKey != "" {
		t.Errorf("board signals %q issue_ref %q decision_key %q", c.Signals, c.IssueRef, c.DecisionKey)
	}
	if want := fmt.Sprintf("%s's model (0.90): %s", sb.Key, dupConflictRationale); c.JudgeNote != want {
		t.Errorf("board judge_note = %q, want %q", c.JudgeNote, want)
	}
	roles := map[string]string{}
	for _, pt := range c.Participants {
		roles[pt.SessionKey] = pt.Role + "/" + pt.SubjectKind
	}
	if len(roles) != 2 || roles[sb.Key] != "initiator/intent" || roles[sa.Key] != "incumbent/intent" {
		t.Errorf("board participants = %+v", c.Participants)
	}
}

// ─────────────────────────────────────────────
// 3. Re-scope: asked once more, converged
// ─────────────────────────────────────────────

func dupScenarioRescope(t *testing.T, w *inviteWorld) {
	p := w.dupStart(t, "")
	tm, sa, sb := p.tm, p.sa, p.sb
	before := w.dupJudgements(tm)

	upd, updRaw := w.dupUpdate(p.csB, tm, sb.Key, p.bobIntent, map[string]any{"summary": dupRescoped})
	t.Logf("Bob's update_intent (re-scope):\n%s", updRaw)
	if upd.Pending.Reviews != 1 {
		t.Fatalf("pending.reviews after the re-scope = %d, want 1", upd.Pending.Reviews)
	}
	block := dupBlockOf(t, upd)
	if len(block.Pairs) != 1 || block.Pairs[0].Why != "open_conflict" || block.Pairs[0].Plan != p.anaIntent || block.Pairs[0].Kind != "duplicate_work" {
		t.Fatalf("re-scope review block = %+v", block)
	}
	second := block.Pairs[0].PairKey
	if second == p.pairKey {
		t.Fatal("the re-scoped plan was handed back the pair key it was already judged under")
	}
	if n := w.dupJudgements(tm); n != before+1 {
		t.Errorf("%d pair(s) after the re-scope, want %d: asked exactly once more", n, before+1)
	}
	if r := w.dupIntent(tm, p.bobIntent); r.wordingRevision != 2 {
		t.Errorf("wording_revision = %d after a rewording, want 2", r.wordingRevision)
	}
	rc, rcRaw := w.dupReview(p.csB, tm, sb.Key)
	t.Logf("Bob's get_review_context after the re-scope:\n%s", rcRaw)
	found := false
	for _, it := range rc.Reviews {
		if it.PairKey != second {
			continue
		}
		found = true
		want := fmt.Sprintf("the duplicate conflict %s between these plans is still open: judge your plan as it is worded now", p.cf)
		if len(it.Why) == 0 || it.Why[len(it.Why)-1] != want {
			t.Errorf("re-judge why = %q, want it to end with %q", it.Why, want)
		}
		if it.Plan.Summary != dupRescoped {
			t.Errorf("re-judge plan = %+v", it.Plan)
		}
	}
	if !found {
		t.Fatalf("the re-judge pair is not in get_review_context: %s", rcRaw)
	}

	settle, settleRaw := w.dupJudge(p.csB, tm, sb.Key, second, "no_conflict", map[string]any{
		"confidence": 0.9, "rationale": dupNoConflictRationale,
	})
	t.Logf("Bob's report_judgement (no_conflict 0.9):\n%s", settleRaw)
	if want := fmt.Sprintf("judged %s against %s: not the same work (0.90); %s settled", p.bobIntent, p.anaIntent, p.cf); settle.Note != want {
		t.Errorf("note =\n  %q\nwant\n  %q", settle.Note, want)
	}
	row := w.dupConflict(tm, p.cf)
	if row.status != int64(enums.CONFLICT_STATUS_RESOLVED) || row.resolution != int64(enums.CONFLICT_RESOLUTION_CONVERGED) {
		t.Fatalf("conflict = status %d resolution %d, want resolved/converged", row.status, row.resolution)
	}
	wantNote := fmt.Sprintf("Settled by the agents: %s re-scoped %s at %s and judged it no longer duplicates %s: \"%s\".",
		decSessionName(sb.Key), p.bobIntent, decClock(row.resolvedAt.Time), p.anaIntent, dupNoConflictRationale)
	t.Logf("§4.10 CONVERGED note, as the server produced it:\n  %s", row.note)
	if row.note != wantNote {
		t.Errorf("resolution note =\n  %q\nwant\n  %q", row.note, wantNote)
	}
	// The board carries the note the row holds. Its plans are the evidence
	// written with the conflict verdict, so Bob's side still reads the
	// wording that was judged the same work, not the re-scoped one: pinned
	// here, reported as a finding.
	var hist dupBoardConflicts
	body := w.get(p.ana, "/v1/teams/"+tm.slug+"/conflicts/history?kind=duplicate_work", &hist)
	t.Logf("GET /v1/teams/%s/conflicts/history?kind=duplicate_work after the re-scope:\n%s", tm.slug, body)
	hc := dupFind(t, hist, p.cf)
	if hc.Resolution != "converged" || hc.ResolutionNote != row.note {
		t.Errorf("history = %+v", hc)
	}
	if len(hc.Plans) != 2 || hc.Plans[1].Key != p.bobIntent || hc.Plans[1].Summary != dupBobPlan {
		t.Errorf("history plans = %+v", hc.Plans)
	}

	// ── A later file-only update asks nothing. ────────────────────────────
	pairs := w.dupJudgements(tm)
	files, filesRaw := w.dupUpdate(p.csB, tm, sb.Key, p.bobIntent, map[string]any{"add_paths": []string{"web/src/components/PasswordMeter.tsx"}})
	t.Logf("Bob's file-only update_intent:\n%s", filesRaw)
	if len(files.Review) != 0 || files.Pending.Reviews != 0 {
		t.Errorf("a file-only update asked to judge: %s", filesRaw)
	}
	if n := w.dupJudgements(tm); n != pairs {
		t.Errorf("a file-only update minted %d pair(s)", n-pairs)
	}
	if r := w.dupIntent(tm, p.bobIntent); r.wordingRevision != 2 {
		t.Errorf("wording_revision = %d after a file-only update, want 2", r.wordingRevision)
	}

	// Nothing reaches Ana, before or after the grace.
	w.decSweep(row.lastDetected.Add(dupGrace+time.Second), dupStaySessions())
	if n := w.decInterruptions(tm, p.ana.member); n != 0 {
		t.Errorf("Ana was interrupted %d time(s) by a duplicate Bob re-scoped away", n)
	}
	var events decBoardEvents
	w.get(p.ana, "/v1/teams/"+tm.slug+"/events?kind=judgement_reported", &events)
	if len(events.Events) != 2 {
		t.Errorf("%d judgement_reported events, want 2", len(events.Events))
	}
	_ = sa
}

// ─────────────────────────────────────────────
// 4. Different parts are not duplicates
// ─────────────────────────────────────────────

func dupScenarioDifferentParts(t *testing.T, w *inviteWorld) {
	for _, c := range []struct{ ana, bob, why string }{
		{"login page", "login endpoint", "layer guard: a page and an endpoint are producer and consumer"},
		{"add login page", "signup page", "no core key shared"},
	} {
		tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
		ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
		csA, csB := w.connect(ana.token), w.connect(bob.token)
		sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the web app"})
		sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the web app"})
		w.dupHackathon(tm)
		w.dupDeclare(csA, tm, sa.Key, c.ana, []string{"web/src/a.tsx"}, nil)
		b, raw := w.dupDeclare(csB, tm, sb.Key, c.bob, []string{"internal/b.go"}, nil)
		t.Logf("%q after %q (%s):\n%s", c.bob, c.ana, c.why, raw)
		if len(b.Review) != 0 || b.Pending.Reviews != 0 {
			t.Errorf("%q vs %q was paired: %s", c.ana, c.bob, raw)
		}
		if n := w.dupJudgements(tm); n != 0 {
			t.Errorf("%q vs %q wrote %d duplicate pair(s)", c.ana, c.bob, n)
		}
	}
}

// ─────────────────────────────────────────────
// 5. Same issue across projects; same wording does not cross
// ─────────────────────────────────────────────

func dupScenarioSameIssue(t *testing.T, w *inviteWorld) {
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the API", "project_key": "api"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the web app", "project_key": "web"})
	if n := w.count("SELECT COUNT(DISTINCT `project_uuid`) FROM `session` WHERE `team_uuid` = ?", tm.id); n != 2 {
		t.Fatalf("the two agents are on %d project(s), want 2", n)
	}

	a, _ := w.dupDeclare(csA, tm, sa.Key, "rate limit the password check", []string{"internal/auth/ratelimit.go"},
		map[string]any{"external_ref": "ISSUE-412"})
	b, raw := w.dupDeclare(csB, tm, sb.Key, "show a lockout message", []string{"web/src/pages/Lockout.tsx"},
		map[string]any{"external_ref": "ISSUE-412"})
	t.Logf("Bob's declare_intent on another project, same issue:\n%s", raw)
	block := dupBlockOf(t, b)
	if len(block.Pairs) != 1 || block.Pairs[0].Why != "same_issue" || block.Pairs[0].Plan != a.Key {
		t.Fatalf("same-issue block = %+v", block)
	}
	rc, _ := w.dupReview(csB, tm, sb.Key)
	if len(rc.Reviews) != 1 || len(rc.Reviews[0].Why) == 0 || rc.Reviews[0].Why[0] != "same issue: ISSUE-412" {
		t.Errorf("same-issue why = %+v", rc.Reviews)
	}
	// A conflict verdict requested low is floored at medium (§4.4 step 4).
	jud, judRaw := w.dupJudge(csB, tm, sb.Key, block.Pairs[0].PairKey, "conflict", map[string]any{
		"confidence": 0.8, "severity": "low", "rationale": "both handle the lockout for ISSUE-412",
	})
	t.Logf("Bob's report_judgement (conflict, low requested):\n%s", judRaw)
	if len(jud.Conflicts) != 1 || jud.Conflicts[0].Severity != "medium" {
		t.Fatalf("a same-issue conflict requested low = %+v, want medium", jud.Conflicts)
	}
	if row := w.dupConflict(tm, jud.Conflicts[0].Key); row.rule != "duplicate_work.same_issue" {
		t.Errorf("detector_rule = %q", row.rule)
	}
	var open dupBoardConflicts
	w.get(ana, "/v1/teams/"+tm.slug+"/conflicts", &open)
	c := dupFind(t, open, jud.Conflicts[0].Key)
	if c.IssueRef != "ISSUE-412" || len(c.Signals) == 0 || c.Signals[0] != "same issue ISSUE-412" {
		t.Errorf("board issue_ref %q signals %q", c.IssueRef, c.Signals)
	}

	// The same WORDING across two projects does not pair.
	pairs := w.dupJudgements(tm)
	w.dupDeclare(csA, tm, sa.Key, dupAnaPlan, []string{"internal/web/login.go"}, nil)
	b2, raw2 := w.dupDeclare(csB, tm, sb.Key, dupBobPlan, []string{dupBobPath}, nil)
	t.Logf("Bob's hackathon wording on another project:\n%s", raw2)
	if len(b2.Review) != 0 {
		t.Errorf("the same wording paired across projects: %s", raw2)
	}
	if n := w.dupJudgements(tm); n != pairs {
		t.Errorf("the same wording across projects wrote %d pair(s)", n-pairs)
	}
}

// ─────────────────────────────────────────────
// 6. Path overlap too: two conflicts, one interruption
// ─────────────────────────────────────────────

func dupScenarioPathOverlap(t *testing.T, w *inviteWorld) {
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the login page"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})
	w.dupHackathon(tm)

	a, _ := w.dupDeclare(csA, tm, sa.Key, dupAnaPlan, []string{dupAnaPath}, nil)
	b, raw := w.dupDeclare(csB, tm, sb.Key, dupBobPlan, []string{dupBobPath, dupAnaPath}, nil)
	t.Logf("Bob's declare_intent, also claiming Ana's file:\n%s", raw)
	var pathKey string
	for _, c := range b.Conflicts {
		if c.Kind == "path_overlap" {
			pathKey = c.Key
		}
	}
	if pathKey == "" {
		t.Fatalf("no path conflict in Bob's conflicts[]: %s", raw)
	}
	block := dupBlockOf(t, b)
	if len(block.Pairs) != 1 || block.Pairs[0].Kind != "duplicate_work" || block.Pairs[0].Plan != a.Key {
		t.Fatalf("review block = %+v", block)
	}
	t.Logf("the duplicate pair's why with a path overlap too: %q", block.Pairs[0].Why)
	rc, _ := w.dupReview(csB, tm, sb.Key)
	if len(rc.Reviews) != 1 || !strings.Contains(strings.Join(rc.Reviews[0].Why, "|"), "you also claim overlapping files: "+dupAnaPath) {
		t.Errorf("why = %+v", rc.Reviews)
	}
	pendingPath := w.dupInstructionsTo(tm, sa.Key, enums.INSTRUCTION_STATUS_PENDING)
	t.Logf("Ana's pending path notice(s) before the duplicate verdict: %d", pendingPath)
	if pendingPath != 1 {
		t.Fatalf("Ana has %d pending instruction(s) from the path overlap, want the 1 path notice", pendingPath)
	}

	jud, _ := w.dupJudge(csB, tm, sb.Key, block.Pairs[0].PairKey, "conflict", map[string]any{
		"confidence": 0.9, "rationale": dupConflictRationale,
	})
	var dupKey string
	for _, c := range jud.Conflicts {
		if c.Kind == "duplicate_work" {
			dupKey = c.Key
		}
	}
	if dupKey == "" {
		t.Fatalf("no duplicate conflict: %+v", jud.Conflicts)
	}
	kinds := map[int64]int{}
	rows, err := w.db.Query("SELECT `kind` FROM `conflict` WHERE `team_uuid` = ? AND `status` = ?", tm.id, enums.CONFLICT_STATUS_OPEN)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k int64
		_ = rows.Scan(&k)
		kinds[k]++
	}
	_ = rows.Close()
	if kinds[int64(enums.CONFLICT_KIND_PATH_OVERLAP)] != 1 || kinds[int64(enums.CONFLICT_KIND_DUPLICATE_WORK)] != 1 {
		t.Fatalf("open conflicts by kind = %v, want one path_overlap and one duplicate_work", kinds)
	}
	var dupEvidence string
	_ = w.db.QueryRow("SELECT JSON_EXTRACT(`evidence`, '$.adjusters') FROM `conflict` WHERE `team_uuid` = ? AND `key` = ?", tm.id, dupKey).Scan(&dupEvidence)
	t.Logf("duplicate conflict adjusters: %s", dupEvidence)

	// ── After the grace: the duplicate notice folds the unread path notice. ──
	row := w.dupConflict(tm, dupKey)
	w.decSweep(row.lastDetected.Add(dupGrace), dupStaySessions())
	var status int64
	var note string
	if err := w.db.QueryRow("SELECT i.`status`, COALESCE(i.`action_note`, '') FROM `instruction` i JOIN `conflict` c ON c.`id` = i.`ref_uuid` "+
		"WHERE c.`team_uuid` = ? AND c.`key` = ?", tm.id, pathKey).Scan(&status, &note); err != nil {
		t.Fatalf("the path notice: %v", err)
	}
	if status != int64(enums.INSTRUCTION_STATUS_DISMISSED) || note != "superseded by "+dupKey {
		t.Errorf("the path notice: status %d action_note %q, want dismissed, %q", status, note, "superseded by "+dupKey)
	}
	hb, hbRaw := w.dupHeartbeat(csA, tm, sa.Key)
	t.Logf("Ana's heartbeat after the grace:\n%s", hbRaw)
	if hb.Pending.Instructions != 1 {
		t.Errorf("Ana's pending.instructions = %d, want 1: one interruption", hb.Pending.Instructions)
	}
	in, inRaw := w.decInstructions(csA, tm, sa.Key)
	t.Logf("Ana's get_instructions:\n%s", inRaw)
	if len(in.Instructions) != 1 || in.Instructions[0].Ref != dupKey {
		t.Fatalf("Ana's instructions = %+v, want only the duplicate notice", in.Instructions)
	}
	want := dupIncumbentAction(decSessionName(sb.Key), b.Key, dupConflictRationale, sb.Key, pathKey)
	t.Logf("§4.5 incumbent notice with a folded path notice, as the server produced it:\n  what:   %s\n  action: %s",
		in.Instructions[0].What, in.Instructions[0].SuggestedAction)
	if in.Instructions[0].SuggestedAction != want {
		t.Errorf("folded notice action =\n  %q\nwant\n  %q", in.Instructions[0].SuggestedAction, want)
	}
	if n := w.dupInstructionsTo(tm, sa.Key, enums.INSTRUCTION_STATUS_DISMISSED); n != 1 {
		t.Errorf("%d dismissed instruction(s) for Ana, want the 1 folded path notice", n)
	}
}

// ─────────────────────────────────────────────
// 7. The agent vanishes
// ─────────────────────────────────────────────

func dupScenarioVanished(t *testing.T, w *inviteWorld) {
	p := w.dupStart(t, "")
	tm, sa, sb := p.tm, p.sa, p.sb

	// A pair Bob is judging (subject b = Bob's plan) and a pair Ana is
	// judging about one of Bob's plans (subject a = Bob's plan): both must
	// expire with him.
	w.dupDeclare(p.csA, tm, sa.Key, "dark mode toggle", []string{"web/src/theme/toggle.tsx"}, nil)
	bd, bdRaw := w.dupDeclare(p.csB, tm, sb.Key, "add dark mode", []string{"web/src/theme/dark.css"}, nil)
	if bd.Pending.Reviews == 0 {
		t.Fatalf("Bob's dark-mode plan was not paired: %s", bdRaw)
	}
	bl, _ := w.dupDeclare(p.csB, tm, sb.Key, "leaderboard", []string{"web/src/leaderboard/index.ts"}, nil)
	al, alRaw := w.dupDeclare(p.csA, tm, sa.Key, "leaderboard page", []string{"web/src/pages/Leaderboard.tsx"}, nil)
	if al.Pending.Reviews == 0 {
		t.Fatalf("Ana's leaderboard plan was not paired with Bob's: %s", alRaw)
	}
	if n := w.dupJudgements(tm, enums.JUDGEMENT_STATUS_PENDING); n != 2 {
		t.Fatalf("%d pending duplicate pair(s) before Bob vanishes, want 2", n)
	}
	_ = bl

	// Bob stops heartbeating; Ana keeps going.
	w.exec("UPDATE `session` SET `last_heartbeat_at` = DATE_SUB(UTC_TIMESTAMP(), INTERVAL 3 HOUR) WHERE `team_uuid` = ? AND `key` = ?", tm.id, sb.Key)
	rep := w.decSweep(time.Now().UTC(), dupStaySessions())
	var sessStatus int64
	var endedAt sql.NullTime
	if err := w.db.QueryRow("SELECT `status`, `ended_at` FROM `session` WHERE `team_uuid` = ? AND `key` = ?", tm.id, sb.Key).Scan(&sessStatus, &endedAt); err != nil {
		t.Fatal(err)
	}
	if sessStatus != int64(enums.SESSION_STATUS_ABANDONED) || !endedAt.Valid {
		t.Fatalf("Bob's session: status %d ended_at %v (report %+v)", sessStatus, endedAt, rep.SessionsAbandoned)
	}
	if st := w.count("SELECT `status` FROM `session` WHERE `team_uuid` = ? AND `key` = ?", tm.id, sa.Key); st != int(enums.SESSION_STATUS_LIVE) {
		t.Fatalf("Ana's session status = %d, want live", st)
	}

	row := w.dupConflict(tm, p.cf)
	t.Logf("§4.10 SUPERSEDED (abandoned) note, as the server produced it:\n  %s", row.note)
	if row.status != int64(enums.CONFLICT_STATUS_RESOLVED) || row.resolution != int64(enums.CONFLICT_RESOLUTION_SUPERSEDED) {
		t.Fatalf("conflict after Bob vanished: status %d resolution %d, want resolved/superseded", row.status, row.resolution)
	}
	wantNote := fmt.Sprintf("Cleared by metiche: %s was abandoned at %s after no heartbeat, ending %s.",
		decSessionName(sb.Key), decClock(endedAt.Time), p.bobIntent)
	if row.note != wantNote {
		t.Errorf("resolution note =\n  %q\nwant\n  %q", row.note, wantNote)
	}
	if n := w.dupJudgements(tm, enums.JUDGEMENT_STATUS_PENDING); n != 0 {
		t.Errorf("%d duplicate pair(s) still pending after Bob was abandoned", n)
	}
	if n := w.dupJudgements(tm, enums.JUDGEMENT_STATUS_EXPIRED); n != 2 {
		t.Errorf("%d duplicate pair(s) expired, want 2 (one each side)", n)
	}
	if n := w.dupJudgements(tm, enums.JUDGEMENT_STATUS_JUDGED); n != 1 {
		t.Errorf("%d judged pair(s) survive, want the 1 Bob answered", n)
	}

	// The stored conflict_resolved event, column by column.
	var (
		summary, subjectKey string
		payload             sql.NullString
		structural          bool
	)
	if err := w.db.QueryRow("SELECT `summary`, COALESCE(`subject_key`, ''), `payload`, `structural` FROM `team_event` "+
		"WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?", tm.id, enums.EVENT_KIND_CONFLICT_RESOLVED, row.id).
		Scan(&summary, &subjectKey, &payload, &structural); err != nil {
		t.Fatalf("the conflict_resolved event: %v", err)
	}
	t.Logf("stored conflict_resolved: summary %q, subject_key %q, structural %v, payload %s", summary, subjectKey, structural, payload.String)
	if want := fmt.Sprintf("%s settled (superseded): %s no longer duplicates %s", p.cf, p.bobIntent, p.anaIntent); summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
	if subjectKey != p.cf || !structural {
		t.Errorf("subject_key %q structural %v", subjectKey, structural)
	}
	var pl struct {
		Message      string `json:"message"`
		Detail       string `json:"detail"`
		ConflictUUID string `json:"conflict_uuid"`
		NewStatus    string `json:"new_status"`
	}
	if !payload.Valid || json.Unmarshal([]byte(payload.String), &pl) != nil {
		t.Fatalf("payload is not JSON: %q", payload.String)
	}
	if pl.Message != row.note || pl.Detail != "superseded" || pl.ConflictUUID != row.id || pl.NewStatus != "resolved" {
		t.Errorf("payload = %+v", pl)
	}
	if n := w.decInterruptions(tm, p.ana.member); n != 0 {
		t.Errorf("Ana was interrupted %d time(s)", n)
	}
}

// ─────────────────────────────────────────────
// 8. Decisions still work alongside
// ─────────────────────────────────────────────

func dupScenarioDecisions(t *testing.T, w *inviteWorld) {
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the auth service"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})
	w.dupHackathon(tm)

	w.decRecord(csA, tm, sa.Key, nil)
	a, aRaw := w.dupDeclare(csA, tm, sa.Key, dupAnaPlan, []string{dupAnaPath}, nil)
	t.Logf("Ana's declare_intent after her decision:\n%s", aRaw)

	// Bob's plan duplicates Ana's AND sits inside the decision's scope.
	b, raw := w.dupDeclare(csB, tm, sb.Key, dupBobPlan, []string{decPlanPath}, nil)
	t.Logf("Bob's declare_intent (duplicate and inside the decision's scope):\n%s", raw)
	if b.Pending.Reviews != 2 {
		t.Fatalf("pending.reviews = %d, want 2", b.Pending.Reviews)
	}
	block := dupBlockOf(t, b)
	if len(block.Pairs) != 2 || block.More != 0 {
		t.Fatalf("review block = %+v", block)
	}
	if p0 := block.Pairs[0]; p0.Kind != "duplicate_work" || p0.Plan != a.Key || p0.Why != "words" {
		t.Errorf("first pair (duplicates go first) = %+v", p0)
	}
	if p1 := block.Pairs[1]; p1.Kind != "" || p1.Decision != decKey || p1.Why != "scope" || p1.Statement != decStatement {
		t.Errorf("second pair (the decision, bytes as before) = %+v", p1)
	}
	if want := "; judge 2 pair(s) against your plan before you edit: see review, then report_judgement"; !strings.HasSuffix(b.Note, want) {
		t.Errorf("note = %q, want it to end with %q", b.Note, want)
	}
	if n := w.count("SELECT COUNT(*) FROM `judgement` j JOIN `session` s ON s.`id` = j.`judge_session_uuid` "+
		"WHERE j.`team_uuid` = ? AND s.`key` = ? AND j.`judging_expires_at` IS NOT NULL", tm.id, sb.Key); n != 2 {
		t.Errorf("%d pair(s) assigned to Bob, want both inside the per-minute cap", n)
	}
	rc, rcRaw := w.dupReview(csB, tm, sb.Key)
	t.Logf("Bob's get_review_context (both kinds):\n%s", rcRaw)
	kinds := map[string]string{}
	for _, it := range rc.Reviews {
		kinds[it.Kind] = it.PairKey
	}
	if kinds["duplicate_work"] != block.Pairs[0].PairKey || kinds["decision_contradiction"] != block.Pairs[1].PairKey {
		t.Fatalf("review context kinds = %v", kinds)
	}

	dj, djRaw := w.dupJudge(csB, tm, sb.Key, block.Pairs[1].PairKey, "conflict", map[string]any{
		"confidence": 0.9, "rationale": decConflictRationale,
	})
	t.Logf("Bob's decision verdict:\n%s", djRaw)
	if len(dj.Conflicts) != 1 || dj.Conflicts[0].Kind != "decision_contradiction" || dj.Conflicts[0].Decision != decKey || dj.Conflicts[0].AtFault != "plan" {
		t.Fatalf("decision conflicts[] = %+v", dj.Conflicts)
	}
	if want := decPlanOwnerAction(b.Key, "Ana", sa.Key); dj.Conflicts[0].SuggestedAction != want {
		t.Errorf("decision suggested_action =\n  %q\nwant\n  %q", dj.Conflicts[0].SuggestedAction, want)
	}
	nj, njRaw := w.dupJudge(csB, tm, sb.Key, block.Pairs[0].PairKey, "no_conflict", map[string]any{
		"confidence": 0.8, "rationale": "Ana builds the route; this plan is the session storage",
	})
	t.Logf("Bob's duplicate verdict:\n%s", njRaw)
	if want := fmt.Sprintf("judged %s against %s: not the same work (0.80)", b.Key, a.Key); nj.Note != want {
		t.Errorf("no_conflict note = %q, want %q", nj.Note, want)
	}
	if n := w.count("SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ? AND `kind` = ?", tm.id, enums.CONFLICT_KIND_DUPLICATE_WORK); n != 0 {
		t.Errorf("a no_conflict verdict wrote %d duplicate conflict(s)", n)
	}
	// The decider is told at once, exactly as before duplicates existed.
	in, inRaw := w.decInstructions(csA, tm, sa.Key)
	t.Logf("Ana's get_instructions:\n%s", inRaw)
	if len(in.Instructions) != 1 || in.Instructions[0].Kind != "conflict_notice" || in.Instructions[0].Ref != dj.Conflicts[0].Key {
		t.Errorf("Ana's instructions = %+v, want the one decision notice", in.Instructions)
	}
}
