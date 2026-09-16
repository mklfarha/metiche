package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/browser"
	"github.com/mklfarha/metiche/backend/app/sweeper"
	"github.com/mklfarha/metiche/backend/enums"
)

// Decisions end to end through the REAL wiring (docs/DECISIONS.md §7.4): two
// people, two agents, each with its own agent token, over /v1/mcp, plus the
// board's REST reads as a signed-in viewer. Ana's agent records a decision,
// Bob's agent declares a plan that contradicts it, Bob's own model judges the
// pair, the decider is told once, and the conflict settles itself when the plan
// changes. Then the variants: the decision changes instead, nobody settles and
// the sweeper asks a person, and the plan's session is abandoned.
//
// Every name is a test name and every secret is minted at run time.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&multiStatements=true&interpolateParams=true' \
//	  go test -p 1 ./app/ -run Decisions -v

// The decision every test records. The statement is 110 runes, so §4.8's
// clip(statement, 120) leaves it whole and the suggested action can be
// asserted character for character.
const (
	decKey       = "#auth-jwt-cookie"
	decTitle     = "Auth is a JWT in an httpOnly cookie"
	decStatement = "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage."

	decBreakingPlan = "store the session token in localStorage after login"
	decFixedPlan    = "read the session from the httpOnly cookie the login endpoint sets"
	decPlanPath     = "web/src/auth/session.ts"

	decConflictRationale   = "plan stores the token in localStorage; #auth-jwt-cookie forbids it"
	decNoConflictRationale = "plan now relies on the httpOnly cookie"

	// instructionTextChars, as app/mcp holds it unexported: what
	// get_instructions shows of a body, and the bound §4.8's wording fits.
	decInstructionChars = 200
	// The room §4.8 gives the quoted rationale inside the decider's notice.
	decDeciderQuoteChars = 60
)

func decScope() []string { return []string{"internal/auth/**", "web/src/auth/**"} }

// ─────────────────────────────────────────────
// Wire shapes
// ─────────────────────────────────────────────

// decAnswer is the tool envelope these tests read.
type decAnswer struct {
	OK       bool            `json:"ok"`
	Key      string          `json:"key"`
	Sequence int64           `json:"sequence"`
	Revision int64           `json:"revision"`
	Note     string          `json:"note"`
	Review   json.RawMessage `json:"review"`
	Pending  struct {
		Instructions int `json:"instructions"`
		Conflicts    int `json:"conflicts"`
		Reviews      int `json:"reviews"`
	} `json:"pending"`
	Conflicts []struct {
		Key             string `json:"key"`
		Kind            string `json:"kind"`
		Severity        string `json:"severity"`
		With            string `json:"with"`
		Decision        string `json:"decision"`
		AtFault         string `json:"at_fault"`
		SuggestedAction string `json:"suggested_action"`
	} `json:"conflicts"`
}

// decReviewBlock is the inline review block on declare_intent/update_intent.
type decReviewBlock struct {
	Pairs []struct {
		PairKey   string `json:"pair_key"`
		Decision  string `json:"decision"`
		Statement string `json:"statement"`
		Why       string `json:"why"`
	} `json:"pairs"`
	More       int    `json:"more"`
	AnswerWith string `json:"answer_with"`
}

// decReviewContext is get_review_context's answer.
type decReviewContext struct {
	Note        string `json:"note"`
	MoreWaiting int    `json:"more_waiting"`
	Reviews     []struct {
		PairKey  string   `json:"pair_key"`
		Kind     string   `json:"kind"`
		Question string   `json:"question"`
		Why      []string `json:"why"`
		Decision *struct {
			Key       string   `json:"key"`
			Title     string   `json:"title"`
			Statement string   `json:"statement"`
			Scope     []string `json:"scope"`
			DecidedBy string   `json:"decided_by"`
			Revision  int64    `json:"revision"`
		} `json:"decision"`
		Plan struct {
			Key      string   `json:"key"`
			Summary  string   `json:"summary"`
			Paths    []string `json:"paths"`
			Revision int64    `json:"revision"`
		} `json:"plan"`
		AnswerWith string `json:"answer_with"`
		ExpiresAt  string `json:"expires_at"`
	} `json:"reviews"`
}

// decInstructionList is get_instructions' answer.
type decInstructionList struct {
	Instructions []struct {
		Key             string `json:"key"`
		Kind            string `json:"kind"`
		From            string `json:"from"`
		What            string `json:"what"`
		SuggestedAction string `json:"suggested_action"`
		Ref             string `json:"ref"`
		Severity        string `json:"severity"`
		With            string `json:"with"`
		ReportBack      bool   `json:"report_back"`
	} `json:"instructions"`
}

// decBoardDecisions is GET /v1/teams/{slug}/decisions (§9.2).
type decBoardDecisions struct {
	Decisions []struct {
		Key        string   `json:"key"`
		Title      string   `json:"title"`
		Statement  string   `json:"statement"`
		Status     string   `json:"status"`
		AlwaysShow bool     `json:"always_show"`
		Revision   int64    `json:"revision"`
		DecidedBy  string   `json:"decided_by"`
		Scope      []string `json:"scope"`
		ProjectKey string   `json:"project_key"`
		Rationale  string   `json:"rationale"`
		Judged     struct {
			NoConflict int64 `json:"no_conflict"`
			Conflict   int64 `json:"conflict"`
			Unsure     int64 `json:"unsure"`
			Pending    int64 `json:"pending"`
		} `json:"judged"`
		OpenConflicts []string `json:"open_conflicts"`
	} `json:"decisions"`
}

// decBoardConflicts is GET /conflicts and /conflicts/history (§9.4).
type decBoardConflicts struct {
	Conflicts []struct {
		Key             string   `json:"key"`
		Kind            string   `json:"kind"`
		Severity        string   `json:"severity"`
		Status          string   `json:"status"`
		Resolution      string   `json:"resolution"`
		ResolutionNote  string   `json:"resolution_note"`
		SuggestedAction string   `json:"suggested_action"`
		DecisionKey     string   `json:"decision_key"`
		JudgeNote       string   `json:"judge_note"`
		EscalatedAt     string   `json:"escalated_at"`
		Paths           []string `json:"paths"`
		Participants    []struct {
			SessionKey  string `json:"session_key"`
			MemberName  string `json:"member_name"`
			Role        string `json:"role"`
			SubjectKind string `json:"subject_kind"`
		} `json:"participants"`
	} `json:"conflicts"`
}

type decBoardEvents struct {
	Events []struct {
		Kind       string `json:"kind"`
		Summary    string `json:"summary"`
		SubjectKey string `json:"subject_key"`
		Structural bool   `json:"structural"`
	} `json:"events"`
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

// decCall runs a tool that must succeed and decodes its envelope.
func (w *inviteWorld) decCall(cs *mcp.ClientSession, name string, args map[string]any) (decAnswer, string) {
	w.t.Helper()
	isErr, text := w.tool(cs, name, args)
	if isErr {
		w.t.Fatalf("%s %v: %s", name, args, text)
	}
	var out decAnswer
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		w.t.Fatalf("%s answer %s: %v", name, text, err)
	}
	return out, text
}

// decBlockOf decodes the inline review block, which must be there.
func decBlockOf(t *testing.T, env decAnswer) decReviewBlock {
	t.Helper()
	if len(env.Review) == 0 {
		t.Fatalf("the response carries no review block: note %q", env.Note)
	}
	var b decReviewBlock
	if err := json.Unmarshal(env.Review, &b); err != nil {
		t.Fatalf("review block is not JSON: %v (%s)", err, env.Review)
	}
	return b
}

// decRecord records the team's decision as the given session.
func (w *inviteWorld) decRecord(cs *mcp.ClientSession, tm testTeam, sessionKey string, extra map[string]any) (decAnswer, string) {
	w.t.Helper()
	args := map[string]any{
		"session_key": sessionKey, "team_slug": tm.slug,
		"key": "auth-jwt-cookie", "title": decTitle, "statement": decStatement,
		"scope": decScope(),
	}
	for k, v := range extra {
		args[k] = v
	}
	return w.decCall(cs, "record_decision", args)
}

// decDeclare declares the plan that contradicts the decision.
func (w *inviteWorld) decDeclare(cs *mcp.ClientSession, tm testTeam, sessionKey, summary string, paths ...string) (decAnswer, string) {
	w.t.Helper()
	if len(paths) == 0 {
		paths = []string{decPlanPath}
	}
	return w.decCall(cs, "declare_intent", map[string]any{
		"session_key": sessionKey, "team_slug": tm.slug,
		"summary": summary, "paths": paths, "mode": "write",
	})
}

func (w *inviteWorld) decReview(cs *mcp.ClientSession, tm testTeam, sessionKey string) (decReviewContext, string) {
	w.t.Helper()
	isErr, text := w.tool(cs, "get_review_context", map[string]any{"session_key": sessionKey, "team_slug": tm.slug})
	if isErr {
		w.t.Fatalf("get_review_context: %s", text)
	}
	var out decReviewContext
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		w.t.Fatalf("get_review_context answer %s: %v", text, err)
	}
	return out, text
}

func (w *inviteWorld) decJudge(cs *mcp.ClientSession, tm testTeam, sessionKey, pairKey, verdict string, extra map[string]any) (decAnswer, string) {
	w.t.Helper()
	args := map[string]any{
		"session_key": sessionKey, "team_slug": tm.slug,
		"pair_key": pairKey, "verdict": verdict,
	}
	for k, v := range extra {
		args[k] = v
	}
	return w.decCall(cs, "report_judgement", args)
}

func (w *inviteWorld) decInstructions(cs *mcp.ClientSession, tm testTeam, sessionKey string) (decInstructionList, string) {
	w.t.Helper()
	isErr, text := w.tool(cs, "get_instructions", map[string]any{"session_key": sessionKey, "team_slug": tm.slug})
	if isErr {
		w.t.Fatalf("get_instructions: %s", text)
	}
	var out decInstructionList
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		w.t.Fatalf("get_instructions answer %s: %v", text, err)
	}
	return out, text
}

// decConflictRow is one conflict row, read straight from the table.
type decConflictRow struct {
	key, rule, note, action string
	status, resolution, sev int64
	resolvedAt, escalatedAt sql.NullTime
	occurrences             int64
}

func (w *inviteWorld) decConflictRow(tm testTeam, key string) decConflictRow {
	w.t.Helper()
	var c decConflictRow
	if err := w.db.QueryRow(
		"SELECT `key`, COALESCE(`detector_rule`, ''), COALESCE(`resolution_note`, ''), COALESCE(`suggested_action`, ''), "+
			"`status`, COALESCE(`resolution`, 0), `severity`, `resolved_at`, `escalated_at`, `occurrence_count` "+
			"FROM `conflict` WHERE `team_uuid` = ? AND `key` = ?", tm.id, key).
		Scan(&c.key, &c.rule, &c.note, &c.action, &c.status, &c.resolution, &c.sev,
			&c.resolvedAt, &c.escalatedAt, &c.occurrences); err != nil {
		w.t.Fatalf("conflict %s: %v", key, err)
	}
	return c
}

// decDecisionUpdatedAt is when the decision row was last written, which §4.8's
// "decision revised" note renders as {recorded time}.
func (w *inviteWorld) decDecisionUpdatedAt(tm testTeam) time.Time {
	w.t.Helper()
	var at time.Time
	if err := w.db.QueryRow("SELECT `updated_at` FROM `decision` WHERE `team_uuid` = ? AND `key` = ?", tm.id, decKey).Scan(&at); err != nil {
		w.t.Fatalf("decision %s: %v", decKey, err)
	}
	return at
}

// decInterruptions counts every instruction ever addressed to any session of
// one member: the whole of what this feature may cost that person.
func (w *inviteWorld) decInterruptions(tm testTeam, memberUUID string) int {
	return w.count(
		"SELECT COUNT(*) FROM `instruction` i JOIN `session` s ON s.`id` = i.`target_session_uuid` "+
			"WHERE s.`team_uuid` = ? AND s.`member_uuid` = ?", tm.id, memberUUID)
}

// decSweep runs one sweeper pass on a test clock with explicit options.
func (w *inviteWorld) decSweep(at time.Time, opts sweeper.Options) sweeper.Report {
	w.t.Helper()
	opts.Clock = func() time.Time { return at }
	s := sweeper.New(w.core, zap.NewNop(), opts)
	rep, err := s.RunOnce(context.Background())
	if err != nil || len(rep.Errors) > 0 {
		w.t.Fatalf("sweeper: %v %v", err, rep.Errors)
	}
	return rep
}

// decClock is app/mcp's clock(): the time as §4.8 renders it.
func decClock(t time.Time) string { return t.UTC().Format("15:04") + " UTC" }

// decClip is app/mcp's clip(): shorten to max runes, marking the cut.
func decClip(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	if max <= 1 {
		return string(r[:max])
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

// decPlanOwnerAction is §4.8's suggested action for the plan's owner, in the
// case this file exercises: the decider is another member with a live agent.
func decPlanOwnerAction(intent, deciderName, deciderSession string) string {
	return fmt.Sprintf("Your plan %s breaks %s (%s). Change the plan to follow it and update_intent with the new summary, "+
		"which asks you to judge again. If the decision itself is wrong, settle that with %s's agent (%s); "+
		"only %s or your person can change it.",
		intent, decKey, decStatement, deciderName, deciderSession, deciderName)
}

// decDeciderAction is §4.8's decider notice action, with the quoted rationale
// given the room that is left, at most 60.
func decDeciderAction(ownerName, intent, rationale string) string {
	const shape = "%s judged its plan %s breaks your decision: \"%s\". They were told to follow it. " +
		"If the decision should change, revise it with record_decision."
	room := decInstructionChars - utf8.RuneCountInString(fmt.Sprintf(shape, ownerName, intent, ""))
	if room > decDeciderQuoteChars {
		room = decDeciderQuoteChars
	}
	if room < 0 {
		room = 0
	}
	return decClip(fmt.Sprintf(shape, ownerName, intent, decClip(rationale, room)), decInstructionChars)
}

// decSessionName is §4.8's session name, "S-22 (test)": every agent in this
// file joins with agent_label "test".
func decSessionName(sessionKey string) string { return sessionKey + " (test)" }

// ─────────────────────────────────────────────
// 1. The whole loop
// ─────────────────────────────────────────────

// Two people, two agents, over the real transport: Ana's agent records
// #auth-jwt-cookie, Bob's agent declares a plan inside its scope, Bob's own
// model judges it a conflict, Ana is told exactly once, Bob changes the plan
// and the conflict settles itself CONVERGED. Then the board reads all of it
// over REST (§7.4 steps 1-7).
func TestDecisionsEndToEndOverMCP(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the auth service"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})

	// ── 1. Ana's agent records the decision, with scope. ──────────────────
	rec, recRaw := w.decRecord(csA, tm, sa.Key, map[string]any{
		"rationale": "one session mechanism for the web app and the API",
	})
	t.Logf("Ana's record_decision over /v1/mcp:\n%s", recRaw)
	if rec.Key != decKey {
		t.Fatalf("record_decision key = %q, want %q", rec.Key, decKey)
	}
	if want := decKey + " recorded (revision 1, 2 scope path(s)); no live plan touches it yet"; rec.Note != want {
		t.Errorf("record_decision note =\n  %q\nwant\n  %q", rec.Note, want)
	}
	if len(rec.Conflicts) != 0 {
		t.Errorf("record_decision returned conflicts: %+v", rec.Conflicts)
	}

	// ── 2. Bob's agent declares a plan inside that scope. ─────────────────
	dec, decRaw := w.decDeclare(csB, tm, sb.Key, decBreakingPlan)
	t.Logf("Bob's declare_intent over /v1/mcp:\n%s", decRaw)
	intent := dec.Key
	if dec.Pending.Reviews != 1 {
		t.Fatalf("Bob's declare_intent pending.reviews = %d, want 1: %s", dec.Pending.Reviews, decRaw)
	}
	block := decBlockOf(t, dec)
	if len(block.Pairs) != 1 || block.Pairs[0].Decision != decKey || block.Pairs[0].Why != "scope" ||
		block.Pairs[0].Statement != decStatement || block.AnswerWith != "report_judgement" || block.More != 0 {
		t.Fatalf("inline review block = %+v", block)
	}
	if want := "; judge 1 pair(s) against your plan before you edit: see review, then report_judgement"; !strings.HasSuffix(dec.Note, want) {
		t.Errorf("declare_intent note = %q, want it to end with %q", dec.Note, want)
	}
	firstPair := block.Pairs[0].PairKey

	// ── 3. Bob reads the pair, then his model reports a conflict. ─────────
	rc, rcRaw := w.decReview(csB, tm, sb.Key)
	t.Logf("Bob's get_review_context:\n%s", rcRaw)
	if len(rc.Reviews) != 1 {
		t.Fatalf("get_review_context returned %d pairs, want 1", len(rc.Reviews))
	}
	item := rc.Reviews[0]
	if item.PairKey != firstPair || item.Kind != "decision_contradiction" ||
		item.AnswerWith != "report_judgement(pair_key, verdict, confidence, severity, rationale)" || item.ExpiresAt == "" {
		t.Errorf("review item = %+v", item)
	}
	if want := fmt.Sprintf("Would %s, as planned, break decision %s?", intent, decKey); item.Question != want {
		t.Errorf("question = %q, want %q", item.Question, want)
	}
	if want := "your claim " + decPlanPath + " is in its scope"; len(item.Why) == 0 || item.Why[0] != want {
		t.Errorf("why = %v, want the first to be %q", item.Why, want)
	}
	if item.Decision == nil || item.Decision.Key != decKey || item.Decision.Statement != decStatement ||
		item.Decision.DecidedBy != "Ana" || item.Decision.Revision != 1 || strings.Join(item.Decision.Scope, ",") != strings.Join(decScope(), ",") {
		t.Errorf("review decision = %+v", item.Decision)
	}
	if item.Plan.Key != intent || item.Plan.Summary != decBreakingPlan || item.Plan.Revision != 1 ||
		len(item.Plan.Paths) != 1 || item.Plan.Paths[0] != decPlanPath {
		t.Errorf("review plan = %+v", item.Plan)
	}
	if want := "1 pair(s) to judge: read each, then report_judgement"; rc.Note != want {
		t.Errorf("get_review_context note = %q, want %q", rc.Note, want)
	}
	// get_review_context is read-only: it must not have moved the team on.
	seqBefore := w.count("SELECT `sequence` FROM `team` WHERE `id` = ?", tm.id)
	if _, _ = w.decReview(csB, tm, sb.Key); w.count("SELECT `sequence` FROM `team` WHERE `id` = ?", tm.id) != seqBefore {
		t.Error("get_review_context advanced the team's sequence")
	}

	jud, judRaw := w.decJudge(csB, tm, sb.Key, firstPair, "conflict", map[string]any{
		"confidence": 0.9, "rationale": decConflictRationale,
	})
	t.Logf("Bob's report_judgement (conflict 0.9):\n%s", judRaw)
	if len(jud.Conflicts) != 1 {
		t.Fatalf("report_judgement conflicts = %+v", jud.Conflicts)
	}
	cf := jud.Conflicts[0]
	cfKey := cf.Key
	if cf.Kind != "decision_contradiction" || cf.AtFault != "plan" || cf.Decision != decKey ||
		cf.Severity != "medium" || !strings.Contains(cf.With, "Ana") {
		t.Fatalf("conflict notice = %+v", cf)
	}
	if want := decPlanOwnerAction(intent, "Ana", sa.Key); cf.SuggestedAction != want {
		t.Errorf("suggested_action =\n  %q\nwant\n  %q", cf.SuggestedAction, want)
	}
	if want := fmt.Sprintf("judged %s against %s: conflict (0.90) — read conflicts[] before you edit", intent, decKey); jud.Note != want {
		t.Errorf("report_judgement note =\n  %q\nwant\n  %q", jud.Note, want)
	}
	if jud.Key != cfKey {
		t.Errorf("envelope key = %q, want the conflict key %q", jud.Key, cfKey)
	}

	// ── 4. Ana hears once, on a bare heartbeat. ───────────────────────────
	hb, hbRaw := w.decCall(csA, "heartbeat", map[string]any{"session_key": sa.Key, "team_slug": tm.slug})
	t.Logf("Ana's heartbeat:\n%s", hbRaw)
	if hb.Pending.Instructions != 1 || hb.Pending.Conflicts != 1 {
		t.Fatalf("Ana's heartbeat pending = %+v, want 1 instruction and 1 conflict", hb.Pending)
	}
	if hb.Pending.Reviews != 0 {
		t.Errorf("Ana was asked to judge %d pair(s); only the plan's own agent judges", hb.Pending.Reviews)
	}
	instr, instrRaw := w.decInstructions(csA, tm, sa.Key)
	t.Logf("Ana's get_instructions:\n%s", instrRaw)
	if len(instr.Instructions) != 1 {
		t.Fatalf("Ana got %d instructions, want 1", len(instr.Instructions))
	}
	notice := instr.Instructions[0]
	if notice.Kind != "conflict_notice" || notice.Ref != cfKey || notice.From != "metiche" || notice.Severity != "medium" {
		t.Errorf("Ana's notice = %+v", notice)
	}
	if want := fmt.Sprintf("%s (medium) on %s", cfKey, decKey); notice.What != want {
		t.Errorf("notice what = %q, want %q", notice.What, want)
	}
	if want := decDeciderAction(decSessionName(sb.Key), intent, decConflictRationale); notice.SuggestedAction != want {
		t.Errorf("notice suggested_action =\n  %q\nwant\n  %q", notice.SuggestedAction, want)
	}
	if n := utf8.RuneCountInString(notice.SuggestedAction); n > decInstructionChars {
		t.Errorf("the notice action is %d characters; get_instructions shows %d", n, decInstructionChars)
	}

	// ── 5. Bob changes the plan, is asked again, and it settles. ──────────
	upd, updRaw := w.decCall(csB, "update_intent", map[string]any{
		"session_key": sb.Key, "team_slug": tm.slug, "intent_key": intent, "summary": decFixedPlan,
	})
	t.Logf("Bob's update_intent:\n%s", updRaw)
	if upd.Pending.Reviews != 1 {
		t.Fatalf("update_intent pending.reviews = %d, want 1: %s", upd.Pending.Reviews, updRaw)
	}
	second := decBlockOf(t, upd)
	if len(second.Pairs) != 1 || second.Pairs[0].Decision != decKey {
		t.Fatalf("the changed plan's review block = %+v", second)
	}
	secondPair := second.Pairs[0].PairKey
	if secondPair == firstPair {
		t.Fatal("a changed plan was handed back the pair key it was already judged under")
	}

	settle, settleRaw := w.decJudge(csB, tm, sb.Key, secondPair, "no_conflict", map[string]any{
		"confidence": 0.95, "rationale": decNoConflictRationale,
	})
	t.Logf("Bob's report_judgement (no_conflict 0.95):\n%s", settleRaw)
	if want := fmt.Sprintf("judged %s against %s: no conflict (0.95); %s settled", intent, decKey, cfKey); settle.Note != want {
		t.Errorf("settling note =\n  %q\nwant\n  %q", settle.Note, want)
	}
	if settle.Key != cfKey {
		t.Errorf("settling envelope key = %q, want %q", settle.Key, cfKey)
	}

	row := w.decConflictRow(tm, cfKey)
	if row.status != int64(enums.CONFLICT_STATUS_RESOLVED) || row.resolution != int64(enums.CONFLICT_RESOLUTION_CONVERGED) {
		t.Fatalf("conflict after the agents converged: status %d resolution %d", row.status, row.resolution)
	}
	if !row.resolvedAt.Valid {
		t.Fatal("a settled conflict has no resolved_at")
	}
	// §4.8, CONVERGED with the plan revised — character for character, with
	// the clock read back from the row the server wrote.
	wantNote := fmt.Sprintf("Settled by the agents: %s revised %s at %s and judged it no longer contradicts %s (r%d): \"%s\".",
		decSessionName(sb.Key), intent, decClock(row.resolvedAt.Time), decKey, 1, decNoConflictRationale)
	t.Logf("§4.8 CONVERGED note, as the server produced it:\n  %s", row.note)
	if row.note != wantNote {
		t.Errorf("resolution note =\n  %q\nwant\n  %q", row.note, wantNote)
	}

	// ── 6. The board reads it over REST. ──────────────────────────────────
	var board decBoardDecisions
	body := w.get(ana, "/v1/teams/"+tm.slug+"/decisions", &board)
	t.Logf("GET /v1/teams/%s/decisions:\n%s", tm.slug, body)
	if len(board.Decisions) != 1 {
		t.Fatalf("board decisions = %+v", board.Decisions)
	}
	bd := board.Decisions[0]
	if bd.Key != decKey || bd.Title != decTitle || bd.Statement != decStatement || bd.Status != "accepted" ||
		bd.Revision != 1 || bd.DecidedBy != "Ana" || bd.AlwaysShow {
		t.Errorf("board decision = %+v", bd)
	}
	if strings.Join(bd.Scope, ",") != strings.Join(decScope(), ",") {
		t.Errorf("board scope = %v, want %v", bd.Scope, decScope())
	}
	// Both verdicts were given against revision 1 of the decision, so §5.1's
	// rule ("judgements on the decision's CURRENT revision, by verdict") counts
	// both. §7.4's sketch of "conflict 0" is unreachable while the conflict
	// verdict's row still names revision 1 — see the report.
	if bd.Judged.NoConflict != 1 || bd.Judged.Conflict != 1 || bd.Judged.Unsure != 0 || bd.Judged.Pending != 0 {
		t.Errorf("judged counts = %+v, want 1 no_conflict and 1 conflict on revision 1", bd.Judged)
	}
	if len(bd.OpenConflicts) != 0 {
		t.Errorf("open_conflicts = %v after the conflict settled", bd.OpenConflicts)
	}

	var hist decBoardConflicts
	body = w.get(ana, "/v1/teams/"+tm.slug+"/conflicts/history?kind=decision_contradiction", &hist)
	t.Logf("GET /v1/teams/%s/conflicts/history?kind=decision_contradiction:\n%s", tm.slug, body)
	if len(hist.Conflicts) != 1 {
		t.Fatalf("conflict history = %+v", hist.Conflicts)
	}
	hc := hist.Conflicts[0]
	if hc.Key != cfKey || hc.Kind != "decision_contradiction" || hc.Status != "resolved" || hc.Resolution != "converged" {
		t.Errorf("history conflict = %+v", hc)
	}
	if hc.ResolutionNote != wantNote {
		t.Errorf("history resolution_note =\n  %q\nwant\n  %q", hc.ResolutionNote, wantNote)
	}
	if hc.DecisionKey != decKey {
		t.Errorf("history decision_key = %q, want %q", hc.DecisionKey, decKey)
	}
	if want := fmt.Sprintf("%s's model (0.90): %s", sb.Key, decConflictRationale); hc.JudgeNote != want {
		t.Errorf("history judge_note =\n  %q\nwant\n  %q", hc.JudgeNote, want)
	}
	// The decision key is the decision_key, never a path.
	for _, p := range hc.Paths {
		if p == decKey {
			t.Errorf("the decision key leaked into paths: %v", hc.Paths)
		}
	}
	roles := map[string]string{}
	for _, p := range hc.Participants {
		roles[p.SessionKey] = p.Role + "/" + p.SubjectKind
	}
	if roles[sb.Key] != "initiator/intent" || roles[sa.Key] != "incumbent/decision" {
		t.Errorf("participants = %+v", hc.Participants)
	}

	var events decBoardEvents
	body = w.get(ana, "/v1/teams/"+tm.slug+"/events?kind=judgement_reported", &events)
	t.Logf("GET /v1/teams/%s/events?kind=judgement_reported:\n%s", tm.slug, body)
	if len(events.Events) != 2 {
		t.Fatalf("%d judgement_reported events, want 2", len(events.Events))
	}
	summaries := map[string]bool{}
	for _, e := range events.Events {
		summaries[e.Summary] = true
		if e.SubjectKey != decKey {
			t.Errorf("judgement_reported subject_key = %q, want %q", e.SubjectKey, decKey)
		}
	}
	for _, want := range []string{
		fmt.Sprintf("test judged %s against %s: conflict", intent, decKey),
		fmt.Sprintf("test judged %s against %s: no conflict", intent, decKey),
	} {
		if !summaries[want] {
			t.Errorf("no judgement_reported event says %q; got %v", want, summaries)
		}
	}

	// ── 7. Ana was interrupted exactly once in the whole run. ─────────────
	if n := w.decInterruptions(tm, ana.member); n != 1 {
		t.Errorf("Ana was interrupted %d time(s) across the run, want exactly 1", n)
	}
	if n := w.decInterruptions(tm, bob.member); n != 0 {
		t.Errorf("Bob was interrupted %d time(s); the plan's owner is told in its own response", n)
	}
}

// ─────────────────────────────────────────────
// 2. Variant: the decision changes instead
// ─────────────────────────────────────────────

// Ana revises the decision rather than Bob changing the plan. Bob is asked
// once more against the new wording, and his no_conflict settles the conflict
// with §4.8's "decision revised" note.
func TestDecisionsEndToEndDecisionRevisedConverges(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the auth service"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})

	w.decRecord(csA, tm, sa.Key, nil)
	dec, _ := w.decDeclare(csB, tm, sb.Key, decBreakingPlan)
	intent := dec.Key
	first := decBlockOf(t, dec).Pairs[0].PairKey
	jud, _ := w.decJudge(csB, tm, sb.Key, first, "conflict", map[string]any{
		"confidence": 0.9, "rationale": decConflictRationale,
	})
	if len(jud.Conflicts) != 1 {
		t.Fatalf("no conflict was raised: %+v", jud)
	}
	cfKey := jud.Conflicts[0].Key

	// Ana revises the wording. Bob's live plan is paired against it again.
	const revised = "Sessions are a signed JWT in an httpOnly, Secure cookie, and a short-lived copy in localStorage is allowed for the SPA."
	rev, revRaw := w.decRecord(csA, tm, sa.Key, map[string]any{"statement": revised})
	t.Logf("Ana's record_decision (revision 2):\n%s", revRaw)
	if want := decKey + " revised to revision 2; 1 live plan(s) will be checked against the new wording"; rev.Note != want {
		t.Errorf("revision note =\n  %q\nwant\n  %q", rev.Note, want)
	}
	// A revision does not close the conflict on its own (§4.5).
	if row := w.decConflictRow(tm, cfKey); row.status != int64(enums.CONFLICT_STATUS_OPEN) {
		t.Fatalf("the conflict closed on a revision alone: status %d", row.status)
	}

	rc, _ := w.decReview(csB, tm, sb.Key)
	if len(rc.Reviews) != 1 || rc.Reviews[0].Decision == nil || rc.Reviews[0].Decision.Revision != 2 ||
		rc.Reviews[0].Decision.Statement != revised {
		t.Fatalf("Bob was not handed the new wording: %+v", rc.Reviews)
	}
	secondPair := rc.Reviews[0].PairKey
	if secondPair == first {
		t.Fatal("the revised decision reused the judged pair key")
	}

	settle, settleRaw := w.decJudge(csB, tm, sb.Key, secondPair, "no_conflict", map[string]any{
		"confidence": 0.95, "rationale": decNoConflictRationale,
	})
	t.Logf("Bob's re-judge after the decision changed:\n%s", settleRaw)
	if want := fmt.Sprintf("judged %s against %s: no conflict (0.95); %s settled", intent, decKey, cfKey); settle.Note != want {
		t.Errorf("note =\n  %q\nwant\n  %q", settle.Note, want)
	}

	row := w.decConflictRow(tm, cfKey)
	if row.status != int64(enums.CONFLICT_STATUS_RESOLVED) || row.resolution != int64(enums.CONFLICT_RESOLUTION_CONVERGED) {
		t.Fatalf("conflict = status %d resolution %d, want resolved/converged", row.status, row.resolution)
	}
	wantNote := fmt.Sprintf("Settled by the agents: %s revised %s to r%d at %s, and %s judged %s no longer contradicts it: \"%s\".",
		decSessionName(sa.Key), decKey, 2, decClock(w.decDecisionUpdatedAt(tm)), decSessionName(sb.Key), intent, decNoConflictRationale)
	t.Logf("§4.8 CONVERGED (decision revised) note, as the server produced it:\n  %s", row.note)
	if row.note != wantNote {
		t.Errorf("resolution note =\n  %q\nwant\n  %q", row.note, wantNote)
	}
	if n := w.decInterruptions(tm, ana.member); n != 1 {
		t.Errorf("Ana was interrupted %d time(s), want exactly 1", n)
	}
}

// ─────────────────────────────────────────────
// 3. Variant: nobody settles it, so a person is asked
// ─────────────────────────────────────────────

// On a hackathon project nobody settles a high-severity contradiction. At the
// budget the sweeper asks a person — once — sets escalated_at, emits
// conflict_escalated, and the question reaches the plan's own agent. The board
// shows the asked badge over REST (§4.7, §4.8).
func TestDecisionsEndToEndSweeperAsksAPersonOnce(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the auth service"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})
	// Hackathon: §4.7's budget is 10 minutes.
	w.exec("UPDATE `project` SET `cadence` = ? WHERE `team_uuid` = ?", enums.PROJECT_CADENCE_HACKATHON, tm.id)

	w.decRecord(csA, tm, sa.Key, nil)
	dec, _ := w.decDeclare(csB, tm, sb.Key, decBreakingPlan)
	jud, _ := w.decJudge(csB, tm, sb.Key, decBlockOf(t, dec).Pairs[0].PairKey, "conflict", map[string]any{
		"confidence": 0.9, "severity": "high", "rationale": decConflictRationale,
	})
	if len(jud.Conflicts) != 1 || jud.Conflicts[0].Severity != "high" {
		t.Fatalf("want one high conflict, got %+v", jud.Conflicts)
	}
	cfKey := jud.Conflicts[0].Key

	// The sessions must stay live long enough to be asked: §4.7 only asks
	// about a plan that is still live, and the default abandon is 10 minutes.
	opts := sweeper.Options{SessionStale: time.Hour, SessionAbandoned: 2 * time.Hour}
	now := time.Now().UTC()

	// A minute before the budget: nothing.
	early := w.decSweep(now.Add(9*time.Minute), opts)
	if early.ConflictsEscalated != 0 {
		t.Fatalf("escalated %d conflict(s) before the budget", early.ConflictsEscalated)
	}
	if row := w.decConflictRow(tm, cfKey); row.escalatedAt.Valid {
		t.Fatalf("escalated_at is set at budget - 1m: %v", row.escalatedAt.Time)
	}
	if n := w.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ? AND `kind` = ?",
		tm.id, enums.INSTRUCTION_KIND_QUESTION); n != 0 {
		t.Fatalf("%d question(s) asked before the budget", n)
	}

	// At the budget: once.
	fired := w.decSweep(now.Add(11*time.Minute), opts)
	if fired.ConflictsEscalated != 1 {
		t.Fatalf("escalated %d conflict(s) at the budget, want 1", fired.ConflictsEscalated)
	}
	row := w.decConflictRow(tm, cfKey)
	if !row.escalatedAt.Valid {
		t.Fatal("escalated_at was not set")
	}
	escalatedAt := row.escalatedAt.Time
	if n := w.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `structural` = 1",
		tm.id, enums.EVENT_KIND_CONFLICT_ESCALATED); n != 1 {
		t.Errorf("%d structural conflict_escalated events, want 1", n)
	}

	// The plan's own agent is asked, with §4.8's wording, inside 200 chars.
	q, qRaw := w.decInstructions(csB, tm, sb.Key)
	t.Logf("Bob's get_instructions after the escalation:\n%s", qRaw)
	if len(q.Instructions) != 1 {
		t.Fatalf("Bob got %d instructions, want the one question", len(q.Instructions))
	}
	question := q.Instructions[0]
	if question.Kind != "question" || question.Ref != cfKey || !question.ReportBack {
		t.Errorf("Bob's question = %+v", question)
	}
	wantPrefix := fmt.Sprintf("%s on %s, unsettled 10m. Ask your person: follow the decision, or agree with Ana to change it? Plan: \"", cfKey, decKey)
	if !strings.HasPrefix(question.What, wantPrefix) || !strings.HasSuffix(question.What, "\". Then report_back their answer.") {
		t.Errorf("Bob's question body =\n  %q\nwant it to start %q and end with the report_back sentence", question.What, wantPrefix)
	}
	if n := utf8.RuneCountInString(question.What); n > 200 {
		t.Errorf("the question is %d characters; get_instructions shows 200", n)
	}
	t.Logf("§4.8 plan-owner question, as the server produced it (%d chars):\n  %s", utf8.RuneCountInString(question.What), question.What)

	// The decider's agent is asked too, from the other side (§4.7 step 2).
	da, daRaw := w.decInstructions(csA, tm, sa.Key)
	t.Logf("Ana's get_instructions after the escalation:\n%s", daRaw)
	var deciderQuestion string
	for _, i := range da.Instructions {
		if i.Kind == "question" {
			deciderQuestion = i.What
		}
	}
	if deciderQuestion == "" {
		t.Error("the decider's agent was not asked")
	} else {
		wantDecider := fmt.Sprintf("%s on %s, unsettled 10m. Ask your person: keep the decision, or change it for Bob's plan \"", cfKey, decKey)
		if !strings.HasPrefix(deciderQuestion, wantDecider) {
			t.Errorf("the decider's question =\n  %q\nwant it to start %q", deciderQuestion, wantDecider)
		}
		t.Logf("§4.8 decider question, as the server produced it:\n  %s", deciderQuestion)
	}

	// The board shows the asked badge.
	var open decBoardConflicts
	body := w.get(ana, "/v1/teams/"+tm.slug+"/conflicts", &open)
	t.Logf("GET /v1/teams/%s/conflicts:\n%s", tm.slug, body)
	found := false
	for _, c := range open.Conflicts {
		if c.Key != cfKey {
			continue
		}
		found = true
		if c.EscalatedAt == "" {
			t.Error("the board shows no escalated_at, so no asked badge")
		}
		if c.DecisionKey != decKey || c.Severity != "high" || c.Status != "open" {
			t.Errorf("board conflict = %+v", c)
		}
	}
	if !found {
		t.Fatalf("%s is not on the board's open conflicts", cfKey)
	}

	// And never twice.
	again := w.decSweep(now.Add(40*time.Minute), opts)
	if again.ConflictsEscalated != 0 {
		t.Errorf("a second pass escalated %d conflict(s); §4.7 asks once", again.ConflictsEscalated)
	}
	if after := w.decConflictRow(tm, cfKey); !after.escalatedAt.Valid || !after.escalatedAt.Time.Equal(escalatedAt) {
		t.Errorf("escalated_at moved from %v to %v", escalatedAt, after.escalatedAt.Time)
	}
	if n := w.count("SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ? AND `kind` = ?",
		tm.id, enums.INSTRUCTION_KIND_QUESTION); n != 2 {
		t.Errorf("%d question(s) after two passes, want the 2 of one escalation", n)
	}
	if n := w.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		tm.id, enums.EVENT_KIND_CONFLICT_ESCALATED); n != 1 {
		t.Errorf("%d conflict_escalated events after two passes, want 1", n)
	}
}

// ─────────────────────────────────────────────
// 4. Variant: the plan's session is abandoned
// ─────────────────────────────────────────────

// Bob stops heartbeating. The sweeper writes his session off, the decision
// conflict settles SUPERSEDED with §4.8's "Cleared by metiche" note, and the
// pairs he was armed to judge expire.
func TestDecisionsEndToEndAbandonedSessionSupersedes(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana, bob := w.join(tm, "Ana"), w.join(tm, "Bob")
	csA, csB := w.connect(ana.token), w.connect(bob.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the auth service"})
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})

	w.decRecord(csA, tm, sa.Key, nil)
	dec, _ := w.decDeclare(csB, tm, sb.Key, decBreakingPlan)
	intent := dec.Key
	jud, _ := w.decJudge(csB, tm, sb.Key, decBlockOf(t, dec).Pairs[0].PairKey, "conflict", map[string]any{
		"confidence": 0.9, "rationale": decConflictRationale,
	})
	if len(jud.Conflicts) != 1 {
		t.Fatalf("no conflict was raised: %+v", jud)
	}
	cfKey := jud.Conflicts[0].Key

	// A second plan inside the scope, left unjudged: this is the armed pair
	// that must expire with the session.
	second, _ := w.decDeclare(csB, tm, sb.Key, "add a token refresh endpoint", "internal/auth/refresh.go")
	if second.Pending.Reviews == 0 {
		t.Fatalf("the second plan was not paired with the decision: %+v", second)
	}
	if n := w.count("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `status` = ?",
		tm.id, enums.JUDGEMENT_STATUS_PENDING); n == 0 {
		t.Fatal("no pending pair to expire, so the assertion would prove nothing")
	}

	// Bob goes quiet; the sweeper writes the session off.
	rep := w.decSweep(time.Now().UTC().Add(30*time.Minute), sweeper.Options{})
	if rep.SessionsAbandoned == 0 {
		t.Fatalf("the sweeper abandoned no sessions: %+v", rep)
	}

	row := w.decConflictRow(tm, cfKey)
	t.Logf("§4.8 SUPERSEDED (abandoned) note, as the server produced it:\n  %s", row.note)
	if row.status != int64(enums.CONFLICT_STATUS_RESOLVED) || row.resolution != int64(enums.CONFLICT_RESOLUTION_SUPERSEDED) {
		t.Fatalf("conflict after Bob was abandoned: status %d resolution %d", row.status, row.resolution)
	}
	var endedAt sql.NullTime
	if err := w.db.QueryRow("SELECT `ended_at` FROM `session` WHERE `team_uuid` = ? AND `key` = ?", tm.id, sb.Key).Scan(&endedAt); err != nil {
		t.Fatal(err)
	}
	if !endedAt.Valid {
		t.Fatal("an abandoned session has no ended_at")
	}
	wantNote := fmt.Sprintf("Cleared by metiche: %s was abandoned at %s after no heartbeat, ending %s.",
		decSessionName(sb.Key), decClock(endedAt.Time), intent)
	if row.note != wantNote {
		t.Errorf("resolution note =\n  %q\nwant\n  %q", row.note, wantNote)
	}

	// Every pair he was armed to judge is expired; the one he answered stays
	// answered.
	if n := w.count("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `status` = ?",
		tm.id, enums.JUDGEMENT_STATUS_PENDING); n != 0 {
		t.Errorf("%d pair(s) still pending for an abandoned session", n)
	}
	if n := w.count("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `status` = ?",
		tm.id, enums.JUDGEMENT_STATUS_EXPIRED); n == 0 {
		t.Error("no pair expired with the abandoned session")
	}
	if n := w.count("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `status` = ? AND `verdict` = ?",
		tm.id, enums.JUDGEMENT_STATUS_JUDGED, enums.JUDGEMENT_VERDICT_CONFLICT); n != 1 {
		t.Errorf("%d judged conflict verdicts survive the abandonment, want 1", n)
	}

	// The decision itself is a standing agreement and is never withdrawn (F12).
	if n := w.count("SELECT COUNT(*) FROM `decision` WHERE `team_uuid` = ? AND `status` = ?",
		tm.id, enums.DECISION_STATUS_ACCEPTED); n != 1 {
		t.Errorf("%d accepted decisions after both sides went away, want 1", n)
	}
}

// ─────────────────────────────────────────────
// 5. The team with no decisions pays nothing
// ─────────────────────────────────────────────

// A team that never recorded a decision gets exactly the envelope it got
// before this feature existed: no review block, no reviews count above zero,
// no judgement rows, and not one field more on the wire.
func TestDecisionsTeamWithNoDecisionsPaysNothing(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	bob := w.join(tm, "Bob")
	csB := w.connect(bob.token)
	sb, _ := w.start(csB, tm.slug, map[string]any{"goal": "the login screen"})

	dec, raw := w.decDeclare(csB, tm, sb.Key, decBreakingPlan)
	t.Logf("declare_intent on a team with no decisions:\n%s", raw)

	if strings.Contains(raw, `"review"`) {
		t.Errorf("the response carries a review block: %s", raw)
	}
	if dec.Pending.Reviews != 0 {
		t.Errorf("pending.reviews = %d on a team with no decisions", dec.Pending.Reviews)
	}
	// The exact field set declare_intent had before decisions existed.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(fields))
	for k := range fields {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"key", "note", "ok", "pending", "revision", "sequence"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("envelope fields = %v, want exactly %v", got, want)
	}
	if n := w.count("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ?", tm.id); n != 0 {
		t.Errorf("the fast path wrote %d judgement row(s)", n)
	}
	// And an update takes the same path.
	upd, updRaw := w.decCall(csB, "update_intent", map[string]any{
		"session_key": sb.Key, "team_slug": tm.slug, "intent_key": dec.Key, "summary": decFixedPlan,
	})
	if strings.Contains(updRaw, `"review"`) || upd.Pending.Reviews != 0 {
		t.Errorf("update_intent on a decisionless team: %s", updRaw)
	}
	if n := w.count("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ?", tm.id); n != 0 {
		t.Errorf("update_intent wrote %d judgement row(s)", n)
	}
}

// ─────────────────────────────────────────────
// 6. One person, two agents
// ─────────────────────────────────────────────

// A decision one of Ana's agents recorded, contradicted by another of Ana's
// agents: §4.2 step 4 drops the severity a rung, so it is recorded on the
// board and interrupts nobody — Ana is not told twice about her own two
// agents disagreeing.
func TestDecisionsOnePersonTwoAgentsIsSoftened(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	csA := w.connect(ana.token)

	// A second agent of the SAME person: her own token, a new client_key.
	isErr, joinRaw := w.tool(csA, "join_team", map[string]any{
		"join_code": tm.code, "member_name": "Ana", "agent_label": "test",
		"client_key": "ana-second-" + randHex(t, 3),
	})
	if isErr {
		t.Fatalf("Ana's second agent could not join: %s", joinRaw)
	}
	var joined struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(joinRaw), &joined); err != nil || joined.Token == "" {
		t.Fatalf("second agent got no token: %v (%s)", err, joinRaw)
	}
	csA2 := w.connect(joined.Token)
	if n := w.count("SELECT COUNT(*) FROM `agent` a JOIN `member` m ON m.`account_uuid` = a.`account_uuid` WHERE m.`id` = ?", ana.member); n != 2 {
		t.Fatalf("%d agents for Ana, want 2", n)
	}

	s1, _ := w.start(csA, tm.slug, map[string]any{"goal": "the auth service"})
	s2, _ := w.start(csA2, tm.slug, map[string]any{"goal": "the login screen"})
	if n := w.count("SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ? AND `member_uuid` = ?", tm.id, ana.member); n != 2 {
		t.Fatalf("Ana's two agents are not on one member: %d sessions", n)
	}

	w.decRecord(csA, tm, s1.Key, nil)
	dec, decRaw := w.decDeclare(csA2, tm, s2.Key, decBreakingPlan)
	t.Logf("the second agent's declare_intent:\n%s", decRaw)
	if dec.Pending.Reviews != 1 {
		t.Fatalf("the second agent was not asked to judge: %s", decRaw)
	}
	intent := dec.Key

	jud, judRaw := w.decJudge(csA2, tm, s2.Key, decBlockOf(t, dec).Pairs[0].PairKey, "conflict", map[string]any{
		"confidence": 0.9, "rationale": decConflictRationale,
	})
	t.Logf("the second agent's report_judgement (same person):\n%s", judRaw)

	// Softened to low: no conflicts[] entry, and the note says so.
	if len(jud.Conflicts) != 0 {
		t.Errorf("a same-person conflict interrupted the caller: %+v", jud.Conflicts)
	}
	if want := fmt.Sprintf("judged %s against %s: conflict (0.90), recorded low on the board", intent, decKey); jud.Note != want {
		t.Errorf("note =\n  %q\nwant\n  %q", jud.Note, want)
	}
	var cfKey string
	if err := w.db.QueryRow("SELECT `key` FROM `conflict` WHERE `team_uuid` = ? AND `kind` = ?",
		tm.id, enums.CONFLICT_KIND_DECISION_CONTRADICTION).Scan(&cfKey); err != nil {
		t.Fatalf("the conflict was not recorded at all: %v", err)
	}
	row := w.decConflictRow(tm, cfKey)
	if row.sev != int64(enums.CONFLICT_SEVERITY_LOW) {
		t.Errorf("severity = %d, want low for one person's own two agents", row.sev)
	}
	// §4.8's own-person wording.
	wantAction := fmt.Sprintf("Your plan %s breaks %s (%s), which your own person decided. Change the plan to follow it and "+
		"update_intent with the new summary, or revise %s with record_decision if the decision is what changed.",
		intent, decKey, decStatement, decKey)
	t.Logf("§4.8 same-person action, as the server produced it:\n  %s", row.action)
	if row.action != wantAction {
		t.Errorf("suggested_action =\n  %q\nwant\n  %q", row.action, wantAction)
	}

	// Nobody was interrupted: not once, let alone twice.
	if n := w.decInterruptions(tm, ana.member); n != 0 {
		t.Errorf("Ana was interrupted %d time(s) by her own two agents, want 0", n)
	}
}

// ─────────────────────────────────────────────
// 7. A private team's gates on the new routes
// ─────────────────────────────────────────────

// Both decision routes answer a non-member with the bytes an unknown team
// gets: 404, same body, same headers, on a private team.
func TestDecisionsRESTRefusalsAreAnUnknownTeam(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	csA := w.connect(ana.token)
	sa, _ := w.start(csA, tm.slug, map[string]any{"goal": "the auth service"})
	w.decRecord(csA, tm, sa.Key, nil)

	elsewhere := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	outsider := w.join(elsewhere, "Bob")

	const missing = "no-such-team-q7z"
	// The package's unknown-slug answer on the board's snapshot route.
	snapshot := w.call(http.MethodGet, "/v1/teams/"+missing, "", nil)

	for _, route := range []string{"/decisions", "/decisions/history"} {
		base := w.call(http.MethodGet, "/v1/teams/"+missing+route, "", sessionHeader(ana))
		if base.status != http.StatusNotFound || base.body != snapshot.body ||
			base.header.Get("Content-Type") != snapshot.header.Get("Content-Type") {
			t.Fatalf("GET %s on an unknown team: %d %q, want the snapshot route's 404 %q",
				route, base.status, base.body, snapshot.body)
		}
		for _, c := range []struct {
			name    string
			headers map[string]string
		}{
			{"no credential", nil},
			{"a garbage session", map[string]string{browser.HeaderSession: "mbs_not-a-real-session-0000000000000000"}},
			{"a member of another team", sessionHeader(outsider)},
		} {
			got := w.call(http.MethodGet, "/v1/teams/"+tm.slug+route, "", c.headers)
			for _, h := range []string{"Content-Type", "Cache-Control"} {
				if got.header.Get(h) != base.header.Get(h) {
					t.Errorf("GET %s as %s: header %s %q, unknown team %q", route, c.name, h, got.header.Get(h), base.header.Get(h))
				}
			}
			if got.status != base.status || got.body != base.body {
				t.Errorf("GET %s as %s: %d %q, want the unknown team's %d %q",
					route, c.name, got.status, got.body, base.status, base.body)
			}
			if strings.Contains(got.body, decKey) {
				t.Errorf("GET %s as %s leaked the decision: %s", route, c.name, got.body)
			}
		}
		// An agent's bearer token is a credential for the team, and the board
		// reads accept one. What must hold is that these two new routes are no
		// more permissive than the board routes that already shipped: whatever
		// /conflicts answers a bearer, /decisions answers too.
		bearer := map[string]string{"Authorization": "Bearer " + ana.token}
		shipped := w.call(http.MethodGet, "/v1/teams/"+tm.slug+"/conflicts", "", bearer)
		mine := w.call(http.MethodGet, "/v1/teams/"+tm.slug+route, "", bearer)
		if mine.status != shipped.status {
			t.Errorf("GET %s with an agent bearer = %d, but the shipped /conflicts route = %d",
				route, mine.status, shipped.status)
		}
		t.Logf("agent bearer on %s = %d (the shipped /conflicts route = %d)", route, mine.status, shipped.status)
	}
	// And the member with her own session is served.
	var board decBoardDecisions
	w.get(ana, "/v1/teams/"+tm.slug+"/decisions", &board)
	if len(board.Decisions) != 1 || board.Decisions[0].Key != decKey {
		t.Errorf("the member's own read: %+v", board.Decisions)
	}
}
