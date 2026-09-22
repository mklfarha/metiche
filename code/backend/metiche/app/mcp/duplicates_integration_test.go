package mcp

// Integration tests for duplicate work against real MySQL (docs/DUPLICATES.md
// §7.2). They need METICHE_TEST_MYSQL_DSN and run with -p 1:
//
//	go test -p 1 ./app/mcp/ -run 'Duplicate' -v
//
// Every summary is a hackathon summary: a few informal words, no external_ref,
// unless the test is about the issue id.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	conflict_evidence_entity "github.com/mklfarha/metiche/backend/entity/conflict_evidence"
	"github.com/mklfarha/metiche/backend/enums"
)

// duplicateChain is the production wiring app/rest.go installs.
func duplicateChain(hs *harness) DetectHook {
	return ChainDetectors(
		NewPathDetector(hs.core, zap.NewNop()),
		NewDuplicateReviewer(hs.core, zap.NewNop()),
		NewDecisionReviewer(hs.core, zap.NewNop()),
		NewReviewRenderer(),
	)
}

func newDuplicateHarness(t *testing.T) *harness {
	t.Helper()
	hs := newHarness(t)
	hs.h.SetDetector(duplicateChain(hs))
	return hs
}

// dupStart starts a session for an existing identity on a project, optionally
// as a subagent of parent.
func (hs *harness) dupStart(t *testing.T, ctx context.Context, name, project, parent string) contractAgent {
	t.Helper()
	res, _, err := hs.h.StartSession(ctx, nil, StartSessionParams{
		ProjectKey: project, Goal: name + " work", ConfirmNewProject: "person", ParentSessionKey: parent})
	if err != nil {
		t.Fatalf("start_session for %s on %s: %v", name, project, err)
	}
	var env Envelope
	decodeResult(t, res, &env)
	who, err := hs.h.RequireSession(ctx, env.Key)
	if err != nil {
		t.Fatal(err)
	}
	return contractAgent{ctx: ctx, key: env.Key, session: who.Session.ID}
}

// dupAgentOn joins a new person and starts a session on project.
func (hs *harness) dupAgentOn(t *testing.T, name, client, project string) contractAgent {
	t.Helper()
	c := hs.join(t, name, client)
	return hs.dupStart(t, c.ctx, name, project, "")
}

type dupJudgementRow struct {
	id, pairKey, a, b string
	aRev, bRev        int64
	judge, conflict   sql.NullString
	status            enums.JudgementStatus
	verdict           enums.JudgementVerdict
	assignments       int64
	pinned            bool
}

func (hs *harness) dupJudgements(t *testing.T) []dupJudgementRow {
	t.Helper()
	rows, err := hs.core.DB().Query(
		"SELECT `id`, `pair_key`, `subject_a_uuid`, `subject_a_revision`, `subject_b_uuid`, `subject_b_revision`, `judge_session_uuid`, "+
			"`conflict_uuid`, `status`, COALESCE(`verdict`, 0), `assignment_count`, `pinned` FROM `judgement` "+
			"WHERE `team_uuid` = ? AND `kind` = ? ORDER BY `created_at`, `subject_b_revision`, `pair_key`",
		hs.teamID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []dupJudgementRow
	for rows.Next() {
		var (
			j               dupJudgementRow
			status, verdict int64
		)
		if err := rows.Scan(&j.id, &j.pairKey, &j.a, &j.aRev, &j.b, &j.bRev, &j.judge, &j.conflict, &status, &verdict, &j.assignments, &j.pinned); err != nil {
			t.Fatal(err)
		}
		j.status, j.verdict = enums.JudgementStatus(status), enums.JudgementVerdict(verdict)
		out = append(out, j)
	}
	return out
}

func (hs *harness) dupJudgementByKey(t *testing.T, pairKey string) dupJudgementRow {
	t.Helper()
	for _, j := range hs.dupJudgements(t) {
		if j.pairKey == pairKey {
			return j
		}
	}
	t.Fatalf("no duplicate judgement with pair_key %s", pairKey)
	return dupJudgementRow{}
}

type dupConflictRow struct {
	id, key, rule, action, note, yield, yieldReason string
	evidence                                        conflict_evidence_entity.ConflictEvidence
	severity, status, resolution, detectedBy        int64
	occurrences                                     int64
	confidence                                      sql.NullFloat64
	notified, escalated                             sql.NullTime
	resolvedBy                                      sql.NullString
}

func (hs *harness) dupConflicts(t *testing.T) []dupConflictRow {
	t.Helper()
	rows, err := hs.core.DB().Query(
		"SELECT `id`, `key`, COALESCE(`detector_rule`, ''), COALESCE(`suggested_action`, ''), COALESCE(`resolution_note`, ''), "+
			"COALESCE(`suggested_yield_session_uuid`, ''), COALESCE(`suggested_yield_reason`, ''), COALESCE(`evidence`, '{}'), `severity`, `status`, "+
			"COALESCE(`resolution`, 0), `detected_by`, `occurrence_count`, `confidence`, `notified_at`, `escalated_at`, `resolved_by_member_uuid` "+
			"FROM `conflict` WHERE `team_uuid` = ? AND `kind` = ? ORDER BY `created_at`, `key`",
		hs.teamID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []dupConflictRow
	for rows.Next() {
		var (
			c  dupConflictRow
			ev string
		)
		if err := rows.Scan(&c.id, &c.key, &c.rule, &c.action, &c.note, &c.yield, &c.yieldReason, &ev, &c.severity, &c.status,
			&c.resolution, &c.detectedBy, &c.occurrences, &c.confidence, &c.notified, &c.escalated, &c.resolvedBy); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(ev), &c.evidence); err != nil {
			t.Fatalf("evidence is not JSON: %v %s", err, ev)
		}
		out = append(out, c)
	}
	return out
}

func (hs *harness) onlyDupConflict(t *testing.T) dupConflictRow {
	t.Helper()
	all := hs.dupConflicts(t)
	if len(all) != 1 {
		t.Fatalf("%d duplicate conflicts, want 1: %+v", len(all), all)
	}
	return all[0]
}

func (hs *harness) intentIDOf(t *testing.T, key string) string {
	t.Helper()
	var id string
	if err := hs.core.DB().QueryRow("SELECT `id` FROM `intent` WHERE `team_uuid` = ? AND `key` = ?", hs.teamID.String(), key).Scan(&id); err != nil {
		t.Fatalf("intent %s: %v", key, err)
	}
	return id
}

func (hs *harness) wordingRevisionOf(t *testing.T, key string) (wording, revision int64) {
	t.Helper()
	if err := hs.core.DB().QueryRow("SELECT `wording_revision`, `revision` FROM `intent` WHERE `team_uuid` = ? AND `key` = ?",
		hs.teamID.String(), key).Scan(&wording, &revision); err != nil {
		t.Fatalf("intent %s: %v", key, err)
	}
	return wording, revision
}

func dupPairs(t *testing.T, env Envelope) []reviewBlockPair {
	t.Helper()
	if len(env.Review) == 0 {
		return nil
	}
	var b reviewBlock
	if err := json.Unmarshal(env.Review, &b); err != nil {
		t.Fatalf("review block is not JSON: %v", err)
	}
	return b.Pairs
}

// dupWorld is the hackathon pair: Ana declared "add login page", then Bob
// declared "build the login screen" and was handed the pair.
type dupWorld struct {
	hs                 *harness
	ana, bob           contractAgent
	aKey, bKey, pair   string
	aIntent, bIntent   string
	declaredB, textOfB string
}

func newDupWorld(t *testing.T) dupWorld {
	t.Helper()
	hs := newDuplicateHarness(t)
	w := dupWorld{hs: hs}
	w.ana = hs.contractAgent(t, "Ana", "client-a")
	w.bob = hs.contractAgent(t, "Bob", "client-b")
	envA, textA := hs.decDeclare(t, w.ana, DeclareIntentParams{Summary: "add login page", Paths: []string{"web/src/routes/login.tsx"}})
	t.Logf("Ana declare_intent: %s", textA)
	envB, textB := hs.decDeclare(t, w.bob, DeclareIntentParams{Summary: "build the login screen", Paths: []string{"web/src/pages/Login.tsx"}})
	t.Logf("Bob declare_intent: %s", textB)
	pairs := dupPairs(t, envB)
	if len(pairs) != 1 {
		t.Fatalf("Bob should be handed one pair: %s", textB)
	}
	w.aKey, w.bKey, w.pair, w.textOfB = envA.Key, envB.Key, pairs[0].PairKey, textB
	w.aIntent, w.bIntent = hs.intentIDOf(t, w.aKey), hs.intentIDOf(t, w.bKey)
	return w
}

func (w dupWorld) judge(t *testing.T, a contractAgent, pair, verdict string, confidence float64, extra ...func(*ReportJudgementParams)) (Envelope, string) {
	t.Helper()
	args := ReportJudgementParams{PairKey: pair, Verdict: verdict, Confidence: conf(confidence),
		Rationale: "both build the login page; " + w.aKey + " already has the route"}
	for _, f := range extra {
		f(&args)
	}
	return w.hs.decJudge(t, a, args)
}

// ─────────────────────────────────────────────
// Declaring
// ─────────────────────────────────────────────

// TestIntegrationDuplicateDeclarePairsTheLaterDeclarer: the hackathon case,
// no external_ref. The later declarer is handed one pair, a = Ana's plan,
// b = Bob's, assigned to Bob, why words.
func TestIntegrationDuplicateDeclarePairsTheLaterDeclarer(t *testing.T) {
	w := newDupWorld(t)
	var env Envelope
	decDecode(t, w.textOfB, &env)
	p := dupPairs(t, env)[0]
	if p.Kind != "duplicate_work" || p.Plan != w.aKey || p.With != "Ana (test)" || p.Summary != "add login page" || p.Why != "words" || p.Decision != "" {
		t.Errorf("review pair = %+v", p)
	}
	if env.Pending.Reviews != 1 || !strings.Contains(env.Note, "judge 1 pair(s) against your plan before you edit: see review, then report_judgement") {
		t.Errorf("pending = %+v note = %q", env.Pending, env.Note)
	}
	rows := w.hs.dupJudgements(t)
	if len(rows) != 1 {
		t.Fatalf("%d duplicate judgements, want 1", len(rows))
	}
	j := rows[0]
	if j.a != w.aIntent || j.b != w.bIntent || j.aRev != 1 || j.bRev != 1 || j.judge.String != w.bob.session.String() ||
		j.status != enums.JUDGEMENT_STATUS_PENDING || j.assignments != 1 || j.pairKey != p.PairKey {
		t.Errorf("judgement = %+v", j)
	}
	if j.pairKey != duplicatePairKey(w.aIntent, 1, w.bIntent, 1) {
		t.Error("the pair key is not the §3.0 key at both wording revisions")
	}
}

// TestIntegrationDuplicateSameIssueAcrossProjects: an issue id pairs across
// repositories; the same wording in two repositories does not.
func TestIntegrationDuplicateSameIssueAcrossProjects(t *testing.T) {
	hs := newDuplicateHarness(t)
	ana := hs.dupAgentOn(t, "Ana", "client-a", "api")
	bob := hs.dupAgentOn(t, "Bob", "client-b", "web")
	envA, _ := hs.decDeclare(t, ana, DeclareIntentParams{Summary: "rate limit the password reset endpoint", ExternalRef: "ISSUE-412", Paths: []string{"internal/reset/limit.go"}})
	envB, text := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "show the lockout banner", ExternalRef: "ISSUE-412", Paths: []string{"src/Banner.tsx"}})
	t.Logf("Bob on web, same issue: %s", text)
	pairs := dupPairs(t, envB)
	if len(pairs) != 1 || pairs[0].Plan != envA.Key || pairs[0].Why != "same_issue" {
		t.Fatalf("the same issue id on two projects should pair with why same_issue: %+v", pairs)
	}

	cai := hs.dupAgentOn(t, "Cai", "client-c", "web")
	dan := hs.dupAgentOn(t, "Dan", "client-d", "api")
	hs.decDeclare(t, cai, DeclareIntentParams{Summary: "add login page"})
	envD, text := hs.decDeclare(t, dan, DeclareIntentParams{Summary: "build the login screen"})
	t.Logf("Dan on api, Cai's wording on web: %s", text)
	if len(dupPairs(t, envD)) != 0 {
		t.Errorf("the same wording on two projects must not pair: %s", text)
	}
	if n := len(hs.dupJudgements(t)); n != 1 {
		t.Errorf("%d duplicate judgements, want only the issue pair", n)
	}
}

// TestIntegrationDuplicateSkipsOwnSupervisorAndHold: the caller's own other
// plan, a supervisor and its subagent, and a hold plan are never paired; two
// sibling subagents are.
func TestIntegrationDuplicateSkipsOwnSupervisorAndHold(t *testing.T) {
	hs := newDuplicateHarness(t)
	bob := hs.contractAgent(t, "Bob", "client-b")
	first, _ := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "add login page"})
	env, text := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "build the login screen"})
	if len(dupPairs(t, env)) != 0 {
		t.Errorf("a session is never paired with its own plan: %s", text)
	}
	hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: first.Key, Status: "done"})

	sub1 := hs.dupStart(t, bob.ctx, "Bob", "metiche", bob.key)
	env, text = hs.decDeclare(t, sub1, DeclareIntentParams{Summary: "login screen"})
	t.Logf("subagent 1 of Bob: %s", text)
	if len(dupPairs(t, env)) != 0 {
		t.Errorf("a subagent is never paired with its supervisor: %s", text)
	}
	sub2 := hs.dupStart(t, bob.ctx, "Bob", "metiche", bob.key)
	env, text = hs.decDeclare(t, sub2, DeclareIntentParams{Summary: "login page"})
	t.Logf("subagent 2 of Bob: %s", text)
	pairs := dupPairs(t, env)
	sub1Intent := hs.intentIDOf(t, pairs[0].Plan)
	var sub1Session string
	_ = hs.core.DB().QueryRow("SELECT `session_uuid` FROM `intent` WHERE `id` = ?", sub1Intent).Scan(&sub1Session)
	if len(pairs) != 1 || sub1Session != sub1.session.String() {
		t.Errorf("two sibling subagents are paired, the supervisor is not: %+v", pairs)
	}

	// A hold plan, in either order.
	hs2 := newDuplicateHarness(t)
	cai := hs2.contractAgent(t, "Cai", "client-c")
	dan := hs2.contractAgent(t, "Dan", "client-d")
	eve := hs2.contractAgent(t, "Eve", "client-e")
	hs2.decDeclare(t, cai, DeclareIntentParams{Summary: "login page", Kind: "hold"})
	env, text = hs2.decDeclare(t, dan, DeclareIntentParams{Summary: "login screen"})
	if len(dupPairs(t, env)) != 0 {
		t.Errorf("a hold plan is never a candidate: %s", text)
	}
	env, text = hs2.decDeclare(t, eve, DeclareIntentParams{Summary: "the login page", Kind: "hold"})
	if len(dupPairs(t, env)) != 0 {
		t.Errorf("a hold plan is never paired: %s", text)
	}
	if n := len(hs2.dupJudgements(t)); n != 0 {
		t.Errorf("%d judgements with hold plans", n)
	}
}

// TestIntegrationDuplicatePinnedPairSkippedEitherOrder: a pinned duplicate
// judgement at any revision stops the reviewer minting the pair again, with
// the caller as subject b and as subject a.
func TestIntegrationDuplicatePinnedPairSkippedEitherOrder(t *testing.T) {
	w := newDupWorld(t)
	hs := w.hs
	pin := func(pairKey string) {
		if _, err := hs.core.DB().Exec("UPDATE `judgement` SET `pinned` = 1 WHERE `pair_key` = ?", pairKey); err != nil {
			t.Fatal(err)
		}
	}
	pin(w.pair) // a = Ana, b = Bob

	// Bob (subject b of the pinned row) rewords.
	env, text := hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "build the login page and its form"})
	t.Logf("Bob rewords, pinned as b: %s", text)
	if wr, _ := hs.wordingRevisionOf(t, w.bKey); wr != 2 {
		t.Fatalf("the rewording should bump wording_revision to 2, got %d", wr)
	}
	if len(dupPairs(t, env)) != 0 || len(hs.dupJudgements(t)) != 1 {
		t.Errorf("a pinned pair must not be minted again (caller as b): %s", text)
	}

	// Ana (subject a of the pinned row) rewords.
	env, text = hs.decUpdate(t, w.ana, UpdateIntentParams{IntentKey: w.aKey, Summary: "add the login page with a form"})
	t.Logf("Ana rewords, pinned as a: %s", text)
	if len(dupPairs(t, env)) != 0 || len(hs.dupJudgements(t)) != 1 {
		t.Errorf("a pinned pair must not be minted again (caller as a): %s", text)
	}

	// Control: a third plan is paired as usual.
	cai := hs.contractAgent(t, "Cai", "client-c")
	env, text = hs.decDeclare(t, cai, DeclareIntentParams{Summary: "login page"})
	if len(dupPairs(t, env)) != 2 {
		t.Errorf("an unpinned plan is still paired: %s", text)
	}
}

// TestIntegrationDuplicateJudgementInsertIsIdempotent: the unique index makes
// the first insert win; a second insert of the same pair is no error, reports
// not inserted, and leaves the row exactly as it was.
func TestIntegrationDuplicateJudgementInsertIsIdempotent(t *testing.T) {
	w := newDupWorld(t)
	first := w.hs.dupJudgementByKey(t, w.pair)
	key, inserted, err := insertDuplicateJudgement(context.Background(), w.hs.core.DB(), duplicateJudgement{
		TeamUUID: w.hs.teamID, AUUID: w.bIntent, AWording: 1, BUUID: w.aIntent, BWording: 1,
		JudgeSession: w.ana.session.String(), Window: time.Minute, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("a second insert of the same pair must not fail: %v", err)
	}
	if inserted || key != w.pair {
		t.Errorf("second insert: key %s inserted %v, want %s false (the key is symmetric)", key, inserted, w.pair)
	}
	if again := w.hs.dupJudgementByKey(t, w.pair); again != first || len(w.hs.dupJudgements(t)) != 1 {
		t.Errorf("the first insert must win: %+v -> %+v", first, again)
	}
}

// TestIntegrationDuplicateReplayIsByteIdentical: a replayed declaration returns
// the same pair keys, byte for byte, and writes one event and one judgement.
func TestIntegrationDuplicateReplayIsByteIdentical(t *testing.T) {
	hs := newDuplicateHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decDeclare(t, ana, DeclareIntentParams{Summary: "add login page"})
	args := DeclareIntentParams{Summary: "build the login screen", IdempotencyKey: "declare-login-1"}
	_, first := hs.decDeclare(t, bob, args)
	_, again := hs.decDeclare(t, bob, args)
	t.Logf("first:  %s\nreplay: %s", first, again)
	if first != again {
		t.Errorf("replay is not byte-identical")
	}
	if n := len(hs.dupJudgements(t)); n != 1 {
		t.Errorf("%d judgements after a replay, want 1", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `idempotency_key` = ?",
		hs.teamID.String(), "intent_declared:"+bob.session.String()+":declare-login-1"); n != 1 {
		t.Errorf("%d events for one declaration", n)
	}
}

// TestIntegrationDuplicateSharesTheCapWithDecisions: 2 duplicate pairs and 2
// decision candidates on one declaration → 2 duplicate pairs and 1 decision
// pair assigned, the rest backlogged, and the per-minute count spans both.
func TestIntegrationDuplicateSharesTheCapWithDecisions(t *testing.T) {
	hs := newDuplicateHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	cai := hs.contractAgent(t, "Cai", "client-c")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decRecord(t, ana, goodDecision())
	second := goodDecision()
	second.Key, second.Title, second.Statement = "auth-errors", "Auth errors are one JSON shape", "Every auth failure answers 401 with {error, code}; never a redirect."
	hs.decRecord(t, ana, second)
	hs.decDeclare(t, ana, DeclareIntentParams{Summary: "add login page", Paths: []string{"web/src/routes/login.tsx"}})
	hs.decDeclare(t, cai, DeclareIntentParams{Summary: "login screen", Paths: []string{"web/src/pages/Login.tsx"}})

	env, text := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "login page", Paths: []string{"web/src/auth/session.ts"}})
	t.Logf("Bob declare_intent: %s", text)
	pairs := dupPairs(t, env)
	if len(pairs) != 3 || pairs[0].Kind != "duplicate_work" || pairs[1].Kind != "duplicate_work" || pairs[2].Decision == "" {
		t.Fatalf("want 2 duplicate pairs first, then 1 decision pair: %+v", pairs)
	}
	if env.Pending.Reviews != 3 || !strings.Contains(env.Note, "judge 3 pair(s)") {
		t.Errorf("pending = %+v note = %q", env.Pending, env.Note)
	}
	bIntent := hs.intentIDOf(t, env.Key)
	count := func(kind enums.ConflictKind, assigned bool) int {
		q := "SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `subject_b_uuid` = ? AND `kind` = ? AND `judge_session_uuid` IS NULL"
		if assigned {
			q = "SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `subject_b_uuid` = ? AND `kind` = ? AND `judge_session_uuid` IS NOT NULL"
		}
		return countRows(t, hs.core.DB(), q, hs.teamID.String(), bIntent, int64(kind))
	}
	if d, dec, back := count(enums.CONFLICT_KIND_DUPLICATE_WORK, true), count(enums.CONFLICT_KIND_DECISION_CONTRADICTION, true),
		count(enums.CONFLICT_KIND_DECISION_CONTRADICTION, false); d != 2 || dec != 1 || back != 1 {
		t.Errorf("assigned duplicates %d, assigned decisions %d, backlogged decisions %d; want 2, 1, 1", d, dec, back)
	}

	// The cap is spent: the next plan's pairs are all backlogged, both kinds.
	env, text = hs.decDeclare(t, bob, DeclareIntentParams{Summary: "login page modal", Paths: []string{"web/src/auth/modal.ts"}})
	t.Logf("Bob's second declaration, cap spent: %s", text)
	if len(env.Review) != 0 || env.Pending.Reviews != 3 {
		t.Errorf("past the cap nothing is assigned: %s", text)
	}
	b2 := hs.intentIDOf(t, env.Key)
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `judgement` WHERE `subject_b_uuid` = ? AND `judge_session_uuid` IS NULL", b2); n < 3 {
		t.Errorf("%d backlogged pairs for the second plan, want duplicates and decisions backlogged", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `judgement` WHERE `subject_b_uuid` = ? AND `judge_session_uuid` IS NOT NULL", b2); n != 0 {
		t.Errorf("%d pairs assigned past the cap", n)
	}
}

// ─────────────────────────────────────────────
// Updating
// ─────────────────────────────────────────────

// TestIntegrationDuplicateUpdateRules: paths, status lines and cosmetic edits
// never re-ask; a real rewording bumps wording_revision, mints a new pair and
// expires the pairs other sessions were asked about the old wording; reverting
// bumps again and asks once more.
func TestIntegrationDuplicateUpdateRules(t *testing.T) {
	w := newDupWorld(t)
	hs := w.hs
	before := len(hs.dupJudgements(t))

	for _, u := range []UpdateIntentParams{
		{AddPaths: []string{"web/src/pages/Form.tsx"}},
		{DropPaths: []string{"web/src/pages/Form.tsx"}},
		{StatusLine: "wiring the form"},
		{Status: "active"},
		{Summary: "Build the Login Screen!"},
	} {
		u.IntentKey = w.bKey
		env, text := hs.decUpdate(t, w.bob, u)
		t.Logf("update %+v: %s", u, text)
		if wr, _ := hs.wordingRevisionOf(t, w.bKey); wr != 1 {
			t.Errorf("update %+v moved wording_revision to %d", u, wr)
		}
		if len(env.Review) != 0 || len(hs.dupJudgements(t)) != before {
			t.Errorf("update %+v minted a pair: %s", u, text)
		}
	}
	if _, rev := hs.wordingRevisionOf(t, w.bKey); rev < 3 {
		t.Errorf("the path and summary edits still bump intent.revision for decisions (got %d)", rev)
	}
	// The pending pair is still answerable.
	env, text := w.judge(t, w.bob, w.pair, "no_conflict", 0.8)
	t.Logf("the original pair, answered after the edits: %s", text)
	if env.Note != fmt.Sprintf("judged %s against %s: not the same work (0.80)", w.bKey, w.aKey) {
		t.Errorf("note = %q", env.Note)
	}

	// Cai joins: pairs with Ana and Bob, pending.
	cai := hs.contractAgent(t, "Cai", "client-c")
	envC, text := hs.decDeclare(t, cai, DeclareIntentParams{Summary: "login page"})
	t.Logf("Cai declare_intent: %s", text)
	if len(dupPairs(t, envC)) != 2 {
		t.Fatalf("Cai should be handed two pairs: %s", text)
	}
	caiIntent := hs.intentIDOf(t, envC.Key)
	statusOf := func(a, b string) enums.JudgementStatus {
		for _, j := range hs.dupJudgements(t) {
			if j.a == a && j.b == b {
				return j.status
			}
		}
		return enums.JUDGEMENT_STATUS_INVALID
	}

	// Ana rewords for real.
	env, text = hs.decUpdate(t, w.ana, UpdateIntentParams{IntentKey: w.aKey, Summary: "add the login page and its form"})
	t.Logf("Ana rewords: %s", text)
	if wr, _ := hs.wordingRevisionOf(t, w.aKey); wr != 2 {
		t.Errorf("a real rewording bumps wording_revision to 2, got %d", wr)
	}
	if got := statusOf(w.aIntent, caiIntent); got != enums.JUDGEMENT_STATUS_EXPIRED {
		t.Errorf("Cai's pair about Ana's old wording should be expired, is %s", got.String())
	}
	if got := statusOf(w.bIntent, caiIntent); got != enums.JUDGEMENT_STATUS_PENDING {
		t.Errorf("Cai's pair about Bob's plan should still be pending, is %s", got.String())
	}
	pairs := dupPairs(t, env)
	if len(pairs) != 2 {
		t.Fatalf("Ana's rewording asks her about Bob and Cai: %s", text)
	}
	for _, j := range hs.dupJudgements(t) {
		if j.b == w.aIntent && j.bRev != 2 {
			t.Errorf("Ana's new pairs store her wording_revision 2: %+v", j)
		}
	}

	// Reverting is a new revision, asked once more.
	n := len(hs.dupJudgements(t))
	_, text = hs.decUpdate(t, w.ana, UpdateIntentParams{IntentKey: w.aKey, Summary: "add login page"})
	t.Logf("Ana reverts: %s", text)
	if wr, _ := hs.wordingRevisionOf(t, w.aKey); wr != 3 {
		t.Errorf("reverting bumps wording_revision to 3, got %d", wr)
	}
	if got := len(hs.dupJudgements(t)); got != n+2 {
		t.Errorf("reverting should mint 2 new pairs, minted %d", got-n)
	}
}

// TestIntegrationDuplicateConcurrentRewordings: two concurrent rewordings of
// one intent bump wording_revision twice: it is read under the lock.
func TestIntegrationDuplicateConcurrentRewordings(t *testing.T) {
	w := newDupWorld(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, s := range []string{"build the login screen with remember me", "build the login screen with a captcha"} {
		wg.Add(1)
		go func(summary string) {
			defer wg.Done()
			_, _, err := w.hs.h.UpdateIntent(w.bob.ctx, nil, UpdateIntentParams{SessionKey: w.bob.key, IntentKey: w.bKey, Summary: summary})
			errs <- err
		}(s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if wr, _ := w.hs.wordingRevisionOf(t, w.bKey); wr != 3 {
		t.Errorf("two concurrent rewordings should bump wording_revision twice (to 3), got %d", wr)
	}
}

// ─────────────────────────────────────────────
// get_review_context
// ─────────────────────────────────────────────

// TestIntegrationDuplicateReviewContext: other is populated, the decision item
// keeps its shape, and nothing is written.
func TestIntegrationDuplicateReviewContext(t *testing.T) {
	hs := newDuplicateHarness(t)
	db := hs.core.DB()
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	scoped := goodDecision()
	scoped.Scope = []string{"web/src/pages/**"}
	hs.decRecord(t, ana, scoped)
	envA, _ := hs.decDeclare(t, ana, DeclareIntentParams{Summary: "add login page", Paths: []string{"web/src/routes/login.tsx"}})
	envB, text := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "build the login screen", Paths: []string{"web/src/pages/Login.tsx"}})
	t.Logf("Bob declare_intent: %s", text)
	if len(dupPairs(t, envB)) != 2 {
		t.Fatalf("Bob should get one duplicate and one decision pair: %s", text)
	}

	snapshot := func() [4]int {
		var seq int
		_ = db.QueryRow("SELECT `sequence` FROM `team` WHERE `id` = ?", hs.teamID.String()).Scan(&seq)
		return [4]int{countRows(t, db, "SELECT COUNT(*) FROM `judgement`"), countRows(t, db, "SELECT COUNT(*) FROM `instruction`"),
			countRows(t, db, "SELECT COUNT(*) FROM `team_event`"), seq}
	}
	was := snapshot()
	out, raw := hs.decContext(t, bob, GetReviewContextParams{})
	t.Logf("Bob get_review_context: %s", raw)
	if now := snapshot(); now != was {
		t.Errorf("get_review_context wrote something: %v -> %v", was, now)
	}
	if len(out.Reviews) != 2 {
		t.Fatalf("%d review items, want 2", len(out.Reviews))
	}
	var dup, dec *ReviewItem
	for i := range out.Reviews {
		if out.Reviews[i].Kind == "duplicate_work" {
			dup = &out.Reviews[i]
		} else {
			dec = &out.Reviews[i]
		}
	}
	if dup == nil || dec == nil {
		t.Fatalf("want one item of each kind: %s", raw)
	}
	wantQ := fmt.Sprintf("Would carrying out %s build the same thing %s (Ana (test)) is already building — the same change, not just the same area?", envB.Key, envA.Key)
	if dup.Question != wantQ {
		t.Errorf("question = %q\nwant       %q", dup.Question, wantQ)
	}
	if strings.Join(dup.Why, "|") != "your summaries share words: login, screen" {
		t.Errorf("why = %q", dup.Why)
	}
	if dup.Plan.Key != envB.Key || dup.Plan.Summary != "build the login screen" || strings.Join(dup.Plan.Paths, ",") != "web/src/pages/login.tsx" /* the project folds case */ ||
		dup.Plan.Revision != 1 || dup.Plan.Who != "" || dup.Plan.SessionKey != "" {
		t.Errorf("plan = %+v", dup.Plan)
	}
	if dup.Other == nil || dup.Other.Key != envA.Key || dup.Other.Summary != "add login page" || strings.Join(dup.Other.Paths, ",") != "web/src/routes/login.tsx" ||
		dup.Other.Revision != 1 || dup.Other.Who != "Ana (test)" || dup.Other.SessionKey != ana.key || dup.Other.Status != "declared" {
		t.Errorf("other = %+v", dup.Other)
	}
	if dup.Decision != nil || dup.ExpiresAt == nil || dup.AnswerWith != reviewAnswerWith {
		t.Errorf("duplicate item = %+v", dup)
	}

	// The decision item keeps exactly its pre-duplicate keys.
	b, _ := json.Marshal(dec)
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(b, &keys)
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "answer_with,decision,expires_at,kind,pair_key,plan,question,why" {
		t.Errorf("decision item keys = %v", got)
	}
	var plan map[string]json.RawMessage
	_ = json.Unmarshal(keys["plan"], &plan)
	if len(plan) != 4 {
		t.Errorf("decision item plan keys = %v, want key, summary, paths, revision", plan)
	}
}

// ─────────────────────────────────────────────
// report_judgement
// ─────────────────────────────────────────────

// TestIntegrationDuplicateNoConflictIsCheap: a no_conflict writes one
// non-structural event and no conflict.
func TestIntegrationDuplicateNoConflictIsCheap(t *testing.T) {
	w := newDupWorld(t)
	env, text := w.judge(t, w.bob, w.pair, "no_conflict", 0.9)
	t.Logf("no_conflict: %s", text)
	if env.Note != fmt.Sprintf("judged %s against %s: not the same work (0.90)", w.bKey, w.aKey) || env.Key != w.bKey {
		t.Errorf("envelope = %+v", env)
	}
	if n := len(w.hs.dupConflicts(t)); n != 0 {
		t.Errorf("%d conflicts after no_conflict", n)
	}
	var structural bool
	if err := w.hs.core.DB().QueryRow("SELECT `structural` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		w.hs.teamID.String(), int64(enums.EVENT_KIND_JUDGEMENT_REPORTED)).Scan(&structural); err != nil || structural {
		t.Errorf("the judgement_reported event must be non-structural: %v %v", structural, err)
	}
}

// TestIntegrationDuplicateConflictVerdict: §3.3 end to end on the store.
func TestIntegrationDuplicateConflictVerdict(t *testing.T) {
	w := newDupWorld(t)
	hs := w.hs
	env, text := w.judge(t, w.bob, w.pair, "conflict", 0.85)
	t.Logf("Bob report_judgement conflict 0.85: %s", text)
	c := hs.onlyDupConflict(t)
	if env.Key != c.key {
		t.Errorf("envelope key = %q, want the conflict %q", env.Key, c.key)
	}
	if env.Note != fmt.Sprintf("judged %s against %s: same work (0.85) — read conflicts[] before you edit", w.bKey, w.aKey) {
		t.Errorf("note = %q", env.Note)
	}
	wantAction := fmt.Sprintf(`%s (Ana (test), declared just now) is already building this: "add login page". Stop before you edit: mark %s superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with Ana's agent (%s) first.`,
		w.aKey, w.bKey, w.ana.key)
	if len(env.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v", env.Conflicts)
	}
	n := env.Conflicts[0]
	if n.Key != c.key || n.Kind != "duplicate_work" || n.Severity != "medium" || n.With != "Ana (test)" || n.DuplicateOf != w.aKey ||
		n.AtFault != "later" || n.SuggestedAction != wantAction || n.Decision != "" || len(n.Paths) != 0 {
		t.Errorf("conflict notice = %+v\nwant action %q", n, wantAction)
	}
	if env.Pending.Conflicts != 1 {
		t.Errorf("Bob's pending = %+v", env.Pending)
	}

	// The row.
	if c.rule != RuleDuplicateWords || enums.ConflictSeverity(c.severity) != enums.CONFLICT_SEVERITY_MEDIUM ||
		enums.ConflictStatus(c.status) != enums.CONFLICT_STATUS_OPEN || enums.DetectedBy(c.detectedBy) != enums.DETECTED_BY_AGENT ||
		c.confidence.Float64 != 0.85 || c.yield != w.bob.session.String() || c.yieldReason != "same work as "+w.aKey ||
		c.action != wantAction || c.occurrences != 1 {
		t.Errorf("conflict row = %+v", c)
	}
	ev := c.evidence
	wantIssue := fmt.Sprintf("%s's model (0.85): both build the login page; %s already has the route", w.bob.key, w.aKey)
	if ev.OverlapPath.Valid || ev.ALabel.String != "Ana (test)" || ev.ASummary.String != "add login page" || ev.APattern.String != "web/src/routes/login.tsx" ||
		ev.BLabel.String != "Bob (test)" || ev.BSummary.String != "build the login screen" || ev.BPattern.String != "web/src/pages/login.tsx" ||
		strings.Join(ev.Adjusters, "|") != fmt.Sprintf("plans:%s,%s|words:login,screen", w.aKey, w.bKey) ||
		len(ev.FieldIssues) != 1 || ev.FieldIssues[0] != wantIssue || ev.Detail.String != RuleDuplicateWords {
		raw, _ := json.Marshal(ev)
		t.Errorf("evidence = %s", raw)
	}
	if j := hs.dupJudgementByKey(t, w.pair); j.conflict.String != c.id || j.status != enums.JUDGEMENT_STATUS_JUDGED || j.verdict != enums.JUDGEMENT_VERDICT_CONFLICT {
		t.Errorf("judgement = %+v", j)
	}

	// Only the judge is a participant; nobody got an instruction.
	var sess, role, kind int64
	var subject, session string
	rows := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ?", c.id)
	if err := hs.core.DB().QueryRow("SELECT `session_uuid`, `role`, `subject_kind`, `subject_uuid` FROM `conflict_participant` WHERE `conflict_uuid` = ?", c.id).
		Scan(&session, &role, &kind, &subject); err != nil {
		t.Fatal(err)
	}
	_ = sess
	if rows != 1 || session != w.bob.session.String() || enums.ParticipantRole(role) != enums.PARTICIPANT_ROLE_INITIATOR ||
		enums.SubjectKind(kind) != enums.SUBJECT_KIND_INTENT || subject != w.bIntent {
		t.Errorf("participants: %d, first %s role %d kind %d subject %s", rows, session, role, kind, subject)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `instruction` WHERE `team_uuid` = ?", hs.teamID.String()); n != 0 {
		t.Errorf("report_judgement wrote %d instruction(s); the incumbent is told by the sweeper, after the grace", n)
	}

	// The event.
	var (
		structural             bool
		subjKind               int64
		subjUUID, summary, pay string
	)
	if err := hs.core.DB().QueryRow("SELECT `structural`, `subject_kind`, `subject_uuid`, `summary`, `payload` FROM `team_event` "+
		"WHERE `team_uuid` = ? AND `kind` = ?", hs.teamID.String(), int64(enums.EVENT_KIND_JUDGEMENT_REPORTED)).
		Scan(&structural, &subjKind, &subjUUID, &summary, &pay); err != nil {
		t.Fatal(err)
	}
	t.Logf("judgement_reported: structural=%v summary=%q payload=%s", structural, summary, pay)
	if !structural || enums.SubjectKind(subjKind) != enums.SUBJECT_KIND_INTENT || subjUUID != w.aIntent ||
		summary != fmt.Sprintf("test judged %s against %s: conflict", w.bKey, w.aKey) ||
		!strings.Contains(pay, w.bIntent) || !strings.Contains(pay, c.id) {
		t.Errorf("event: structural=%v kind=%d subject=%s summary=%q", structural, subjKind, subjUUID, summary)
	}

	// Ana is not interrupted.
	res, _, err := hs.h.Heartbeat(w.ana.ctx, nil, HeartbeatParams{SessionKey: w.ana.key})
	if err != nil {
		t.Fatal(err)
	}
	var beat Envelope
	decodeResult(t, res, &beat)
	if beat.Pending.Any() {
		t.Errorf("Ana was interrupted: %+v", beat.Pending)
	}

	// Replay byte for byte; a different verdict is refused.
	_, again := w.judge(t, w.bob, w.pair, "conflict", 0.99, func(p *ReportJudgementParams) { p.Rationale = "something else" })
	if again != text {
		t.Errorf("replay differs:\n%s\n%s", text, again)
	}
	msg := hs.decJudgeErr(t, w.bob, ReportJudgementParams{PairKey: w.pair, Verdict: "no_conflict"})
	t.Logf("a different verdict: %s", msg)
	if !strings.Contains(msg, "already judged conflict at") {
		t.Errorf("different verdict: %s", msg)
	}
}

// TestIntegrationDuplicateLowVerdicts: below 0.7 and unsure are recorded low
// and interrupt nobody; exactly 0.70 interrupts.
func TestIntegrationDuplicateLowVerdicts(t *testing.T) {
	t.Run("conflict 0.6", func(t *testing.T) {
		w := newDupWorld(t)
		env, text := w.judge(t, w.bob, w.pair, "conflict", 0.6)
		t.Logf("%s", text)
		c := w.hs.onlyDupConflict(t)
		if len(env.Conflicts) != 0 || enums.ConflictSeverity(c.severity) != enums.CONFLICT_SEVERITY_LOW ||
			env.Note != fmt.Sprintf("judged %s against %s: same work (0.60), recorded low on the board; below 0.7 it interrupts nobody", w.bKey, w.aKey) {
			t.Errorf("0.6: conflicts=%v severity=%d note=%q", env.Conflicts, c.severity, env.Note)
		}
	})
	t.Run("unsure", func(t *testing.T) {
		w := newDupWorld(t)
		env, text := w.hs.decJudge(t, w.bob, ReportJudgementParams{PairKey: w.pair, Verdict: "unsure", Rationale: "both touch login; unclear which part"})
		t.Logf("%s", text)
		c := w.hs.onlyDupConflict(t)
		if len(env.Conflicts) != 0 || enums.ConflictSeverity(c.severity) != enums.CONFLICT_SEVERITY_LOW || c.rule != RuleDuplicateUnsure ||
			env.Note != fmt.Sprintf("judged %s against %s: unsure, recorded low on the board", w.bKey, w.aKey) ||
			!strings.HasPrefix(c.action, w.bKey+" may overlap "+w.aKey) {
			t.Errorf("unsure: conflicts=%v severity=%d rule=%s note=%q action=%q", env.Conflicts, c.severity, c.rule, env.Note, c.action)
		}
		if c.evidence.FieldIssues[0] != fmt.Sprintf("%s's model (unsure): both touch login; unclear which part", w.bob.key) {
			t.Errorf("field issue = %q", c.evidence.FieldIssues[0])
		}
	})
	t.Run("conflict exactly 0.70", func(t *testing.T) {
		w := newDupWorld(t)
		env, text := w.judge(t, w.bob, w.pair, "conflict", 0.70)
		t.Logf("%s", text)
		if len(env.Conflicts) != 1 || env.Conflicts[0].Severity != "medium" {
			t.Errorf("a conflict at exactly 0.70 interrupts the judge: %s", text)
		}
	})
}

// TestIntegrationDuplicateSameIssueFloorsSeverity: a shared issue id floors a
// low-requested conflict at medium, and is named on the row.
func TestIntegrationDuplicateSameIssueFloorsSeverity(t *testing.T) {
	hs := newDuplicateHarness(t)
	ana := hs.dupAgentOn(t, "Ana", "client-a", "api")
	bob := hs.dupAgentOn(t, "Bob", "client-b", "web")
	envA, _ := hs.decDeclare(t, ana, DeclareIntentParams{Summary: "rate limit the password reset endpoint", ExternalRef: "ISSUE-412"})
	envB, _ := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "throttle reset requests", ExternalRef: "ISSUE-412"})
	pair := dupPairs(t, envB)[0].PairKey
	env, text := hs.decJudge(t, bob, ReportJudgementParams{PairKey: pair, Verdict: "conflict", Confidence: conf(0.9), Severity: "low",
		Rationale: "both throttle the reset endpoint"})
	t.Logf("%s", text)
	c := hs.onlyDupConflict(t)
	if len(env.Conflicts) != 1 || env.Conflicts[0].Severity != "medium" || enums.ConflictSeverity(c.severity) != enums.CONFLICT_SEVERITY_MEDIUM ||
		c.rule != RuleDuplicateSameIssue || c.evidence.OverlapPath.String != "ISSUE-412" ||
		strings.Join(c.evidence.Adjusters, "|") != fmt.Sprintf("plans:%s,%s|same_issue:ISSUE-412", envA.Key, envB.Key) {
		t.Errorf("same issue: conflicts=%+v row=%+v", env.Conflicts, c)
	}
}

// TestIntegrationDuplicateSameMemberNoSoftening: one person's two live agents
// duplicating each other is solo fan-out, exactly who needs telling.
func TestIntegrationDuplicateSameMemberNoSoftening(t *testing.T) {
	hs := newDuplicateHarness(t)
	one := hs.join(t, "Ana", "client-1")
	two := hs.rejoin(t, one, "Ana", "client-2")
	a1 := hs.dupStart(t, one.ctx, "Ana", "metiche", "")
	a2 := hs.dupStart(t, hs.ctxForToken(t, two.token), "Ana", "metiche", "")
	envA, _ := hs.decDeclare(t, a1, DeclareIntentParams{Summary: "add login page"})
	envB, _ := hs.decDeclare(t, a2, DeclareIntentParams{Summary: "build the login screen"})
	pairs := dupPairs(t, envB)
	if len(pairs) != 1 {
		t.Fatalf("one person's two agents are paired: %+v", pairs)
	}
	env, text := hs.decJudge(t, a2, ReportJudgementParams{PairKey: pairs[0].PairKey, Verdict: "conflict", Confidence: conf(0.9), Rationale: "same page twice"})
	t.Logf("%s", text)
	c := hs.onlyDupConflict(t)
	if len(env.Conflicts) != 1 || env.Conflicts[0].Severity != "medium" || enums.ConflictSeverity(c.severity) != enums.CONFLICT_SEVERITY_MEDIUM {
		t.Errorf("no softening: %s", text)
	}
	if c.evidence.Adjusters[len(c.evidence.Adjusters)-1] != "same_member_concurrent" {
		t.Errorf("adjusters = %v", c.evidence.Adjusters)
	}
	if !strings.HasPrefix(env.Conflicts[0].SuggestedAction, fmt.Sprintf("%s (%s, your own person's other agent, declared", envA.Key, a1.key)) {
		t.Errorf("same-member action = %q", env.Conflicts[0].SuggestedAction)
	}
}

// TestIntegrationDuplicateDemotedRule: a rule in demoted_rules records low.
func TestIntegrationDuplicateDemotedRule(t *testing.T) {
	w := newDupWorld(t)
	if _, err := w.hs.core.DB().Exec("UPDATE `team` SET `settings` = ? WHERE `id` = ?",
		`{"demoted_rules":["duplicate_work.words"]}`, w.hs.teamID.String()); err != nil {
		t.Fatal(err)
	}
	env, text := w.judge(t, w.bob, w.pair, "conflict", 0.9)
	t.Logf("%s", text)
	c := w.hs.onlyDupConflict(t)
	if len(env.Conflicts) != 0 || enums.ConflictSeverity(c.severity) != enums.CONFLICT_SEVERITY_LOW ||
		env.Note != fmt.Sprintf("judged %s against %s: same work (0.90), recorded low on the board", w.bKey, w.aKey) {
		t.Errorf("demoted: conflicts=%v severity=%d note=%q", env.Conflicts, c.severity, env.Note)
	}
}

// TestIntegrationDuplicateRefusals: §3.3's refusals, word for word.
func TestIntegrationDuplicateRefusals(t *testing.T) {
	t.Run("judge is not the caller", func(t *testing.T) {
		w := newDupWorld(t)
		msg := w.hs.decJudgeErr(t, w.ana, ReportJudgementParams{PairKey: w.pair, Verdict: "no_conflict"})
		if msg != fmt.Sprintf("this pair is %s's to judge, not yours", w.bob.key) {
			t.Errorf("refusal = %q", msg)
		}
	})
	t.Run("the judge's plan was reworded", func(t *testing.T) {
		w := newDupWorld(t)
		w.hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "wire the login page to POST /api/login"})
		msg := w.hs.decJudgeErr(t, w.bob, ReportJudgementParams{PairKey: w.pair, Verdict: "no_conflict"})
		if msg != fmt.Sprintf("%s was reworded after this pair was made: call get_review_context for the current pair", w.bKey) {
			t.Errorf("refusal = %q", msg)
		}
	})
	t.Run("the other plan was reworded", func(t *testing.T) {
		w := newDupWorld(t)
		// Straight to the row: a rewording through update_intent expires the
		// pair outright (the next case).
		if _, err := w.hs.core.DB().Exec("UPDATE `intent` SET `wording_revision` = 2 WHERE `id` = ?", w.aIntent); err != nil {
			t.Fatal(err)
		}
		msg := w.hs.decJudgeErr(t, w.bob, ReportJudgementParams{PairKey: w.pair, Verdict: "no_conflict"})
		if msg != fmt.Sprintf("%s was reworded after this pair was made: call get_review_context for the current pair", w.aKey) {
			t.Errorf("refusal = %q", msg)
		}
	})
	t.Run("the other plan reworded through update_intent", func(t *testing.T) {
		w := newDupWorld(t)
		w.hs.decUpdate(t, w.ana, UpdateIntentParams{IntentKey: w.aKey, Summary: "add the login page and its form"})
		msg := w.hs.decJudgeErr(t, w.bob, ReportJudgementParams{PairKey: w.pair, Verdict: "no_conflict"})
		if !strings.Contains(msg, "expired unanswered") {
			t.Errorf("refusal = %q", msg)
		}
	})
	t.Run("a plan ended", func(t *testing.T) {
		w := newDupWorld(t)
		if _, err := w.hs.core.DB().Exec("UPDATE `intent` SET `status` = ? WHERE `id` = ?", int64(enums.INTENT_STATUS_DONE), w.aIntent); err != nil {
			t.Fatal(err)
		}
		msg := w.hs.decJudgeErr(t, w.bob, ReportJudgementParams{PairKey: w.pair, Verdict: "no_conflict"})
		if msg != fmt.Sprintf("%s is done; nothing to judge", w.aKey) {
			t.Errorf("refusal = %q", msg)
		}
	})
	t.Run("the other session ended", func(t *testing.T) {
		w := newDupWorld(t)
		if _, err := w.hs.core.DB().Exec("UPDATE `session` SET `status` = ? WHERE `id` = ?", int64(enums.SESSION_STATUS_ENDED), w.ana.session.String()); err != nil {
			t.Fatal(err)
		}
		msg := w.hs.decJudgeErr(t, w.bob, ReportJudgementParams{PairKey: w.pair, Verdict: "no_conflict"})
		if msg != fmt.Sprintf("%s's session has ended; nothing to judge", w.aKey) {
			t.Errorf("refusal = %q", msg)
		}
	})
}

// ─────────────────────────────────────────────
// Settlement
// ─────────────────────────────────────────────

// normClock blanks the HH:MM of a note: resolved_at is a DATETIME, which
// rounds to the second, so the minute the note was written in can differ from
// the stored column's by one at a minute boundary.
var clockInNote = regexp.MustCompile(`\d{2}:\d{2} UTC`)

func normClock(s string) string { return clockInNote.ReplaceAllString(s, "HH:MM UTC") }

func (hs *harness) dupResolvedAt(t *testing.T, id string) time.Time {
	t.Helper()
	var at time.Time
	if err := hs.core.DB().QueryRow("SELECT `resolved_at` FROM `conflict` WHERE `id` = ?", id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// raised is a world where Bob judged the pair a conflict.
func raised(t *testing.T) (dupWorld, dupConflictRow) {
	t.Helper()
	w := newDupWorld(t)
	w.judge(t, w.bob, w.pair, "conflict", 0.85)
	return w, w.hs.onlyDupConflict(t)
}

func TestIntegrationDuplicateSettlement(t *testing.T) {
	t.Run("the yield side supersedes its plan: yielded", func(t *testing.T) {
		w, c := raised(t)
		_, text := w.hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Status: "superseded"})
		t.Logf("%s", text)
		got := w.hs.onlyDupConflict(t)
		want := fmt.Sprintf("Settled by the agents: %s (test) marked %s superseded at %s, leaving %s (%s (test)) to build it.",
			w.bob.key, w.bKey, clock(w.hs.dupResolvedAt(t, c.id)), w.aKey, w.ana.key)
		if enums.ConflictResolution(got.resolution) != enums.CONFLICT_RESOLUTION_YIELDED || normClock(got.note) != normClock(want) {
			t.Errorf("resolution %d note %q\nwant %q", got.resolution, got.note, want)
		}
		var summary string
		_ = w.hs.core.DB().QueryRow("SELECT `summary` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?", w.hs.teamID.String(),
			int64(enums.EVENT_KIND_CONFLICT_RESOLVED)).Scan(&summary)
		if summary != fmt.Sprintf("%s settled (yielded): %s no longer duplicates %s", c.key, w.bKey, w.aKey) {
			t.Errorf("timeline summary = %q", summary)
		}
	})
	t.Run("the incumbent abandons its plan before it is a participant: yielded", func(t *testing.T) {
		w, c := raised(t)
		if n := countRows(t, w.hs.core.DB(), "SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ?", c.id, w.ana.session.String()); n != 0 {
			t.Fatal("Ana should not be a participant yet")
		}
		w.hs.decUpdate(t, w.ana, UpdateIntentParams{IntentKey: w.aKey, Status: "abandoned"})
		got := w.hs.onlyDupConflict(t)
		want := fmt.Sprintf("Settled by the agents: %s (test) marked %s abandoned at %s, leaving %s (%s (test)) to build it.",
			w.ana.key, w.aKey, clock(w.hs.dupResolvedAt(t, c.id)), w.bKey, w.bob.key)
		if enums.ConflictResolution(got.resolution) != enums.CONFLICT_RESOLUTION_YIELDED || normClock(got.note) != normClock(want) {
			t.Errorf("resolution %d note %q\nwant %q", got.resolution, got.note, want)
		}
	})
	t.Run("the yield side finishes anyway: superseded", func(t *testing.T) {
		w, c := raised(t)
		w.hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Status: "done"})
		got := w.hs.onlyDupConflict(t)
		want := fmt.Sprintf("Settled by the agents: %s (test) finished %s at %s anyway, so the work was duplicated; reconcile the two at merge.",
			w.bob.key, w.bKey, clock(w.hs.dupResolvedAt(t, c.id)))
		if enums.ConflictResolution(got.resolution) != enums.CONFLICT_RESOLUTION_SUPERSEDED || normClock(got.note) != normClock(want) {
			t.Errorf("resolution %d note %q\nwant %q", got.resolution, got.note, want)
		}
	})
	t.Run("the incumbent finishes while the yield side builds: stays open", func(t *testing.T) {
		w, _ := raised(t)
		w.hs.decUpdate(t, w.ana, UpdateIntentParams{IntentKey: w.aKey, Status: "done"})
		if got := w.hs.onlyDupConflict(t); enums.ConflictStatus(got.status) != enums.CONFLICT_STATUS_OPEN {
			t.Errorf("a done while b is live must stay open, got status %d note %q", got.status, got.note)
		}
	})
	t.Run("the yield side ends its session: superseded", func(t *testing.T) {
		w, c := raised(t)
		if _, _, err := w.hs.h.EndSession(w.bob.ctx, nil, EndSessionParams{SessionKey: w.bob.key}); err != nil {
			t.Fatal(err)
		}
		got := w.hs.onlyDupConflict(t)
		want := fmt.Sprintf("Settled by the agents: %s (test) ended its session at %s (succeeded), ending %s.",
			w.bob.key, clock(w.hs.dupResolvedAt(t, c.id)), w.bKey)
		if enums.ConflictResolution(got.resolution) != enums.CONFLICT_RESOLUTION_SUPERSEDED || normClock(got.note) != normClock(want) {
			t.Errorf("resolution %d note %q\nwant %q", got.resolution, got.note, want)
		}
	})
	t.Run("the incumbent ends its session, found through the judgement: superseded", func(t *testing.T) {
		w, c := raised(t)
		if _, _, err := w.hs.h.EndSession(w.ana.ctx, nil, EndSessionParams{SessionKey: w.ana.key}); err != nil {
			t.Fatal(err)
		}
		got := w.hs.onlyDupConflict(t)
		want := fmt.Sprintf("Settled by the agents: %s (test) ended its session at %s (succeeded), ending %s.",
			w.ana.key, clock(w.hs.dupResolvedAt(t, c.id)), w.aKey)
		if enums.ConflictResolution(got.resolution) != enums.CONFLICT_RESOLUTION_SUPERSEDED || normClock(got.note) != normClock(want) {
			t.Errorf("resolution %d note %q\nwant %q", got.resolution, got.note, want)
		}
	})
	t.Run("re-scoped, then no_conflict: converged, then reopened, then silenced", func(t *testing.T) {
		w, c := raised(t)
		hs := w.hs
		env, text := hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "wire the login page to POST /api/login"})
		t.Logf("Bob rewords: %s", text)
		pairs := dupPairs(t, env)
		if len(pairs) != 1 || pairs[0].PairKey == w.pair {
			t.Fatalf("the rewording should earn one new pair: %s", text)
		}
		env, text = hs.decJudge(t, w.bob, ReportJudgementParams{PairKey: pairs[0].PairKey, Verdict: "no_conflict", Confidence: conf(0.9),
			Rationale: "now the API call behind the page"})
		t.Logf("Bob no_conflict: %s", text)
		if env.Key != c.key || env.Note != fmt.Sprintf("judged %s against %s: not the same work (0.90); %s settled", w.bKey, w.aKey, c.key) {
			t.Errorf("envelope = %+v", env)
		}
		got := hs.onlyDupConflict(t)
		want := fmt.Sprintf("Settled by the agents: %s (test) re-scoped %s at %s and judged it no longer duplicates %s: \"now the API call behind the page\".",
			w.bob.key, w.bKey, clock(hs.dupResolvedAt(t, c.id)), w.aKey)
		if enums.ConflictResolution(got.resolution) != enums.CONFLICT_RESOLUTION_CONVERGED || normClock(got.note) != normClock(want) {
			t.Errorf("resolution %d note %q\nwant %q", got.resolution, got.note, want)
		}

		// metiche closed it; the agents say it is the same work again.
		if _, err := hs.core.DB().Exec("UPDATE `conflict` SET `notified_at` = ?, `max_severity_notified` = ?, `escalated_at` = ? WHERE `id` = ?",
			time.Now().UTC(), int64(enums.CONFLICT_SEVERITY_MEDIUM), time.Now().UTC(), c.id); err != nil {
			t.Fatal(err)
		}
		env, _ = hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "add the login page myself"})
		pairs = dupPairs(t, env)
		if len(pairs) != 1 {
			t.Fatalf("the second rewording should earn a pair: %+v", env)
		}
		env, text = w.judge(t, w.bob, pairs[0].PairKey, "conflict", 0.9)
		t.Logf("Bob conflict again: %s", text)
		got = hs.onlyDupConflict(t)
		if enums.ConflictStatus(got.status) != enums.CONFLICT_STATUS_OPEN || got.resolution != 0 || got.note != "" ||
			got.notified.Valid || got.escalated.Valid || got.occurrences != 2 || len(env.Conflicts) != 1 {
			t.Errorf("reopened: %+v", got)
		}
		var structural bool
		_ = hs.core.DB().QueryRow("SELECT `structural` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? ORDER BY `sequence` DESC LIMIT 1",
			hs.teamID.String(), int64(enums.EVENT_KIND_JUDGEMENT_REPORTED)).Scan(&structural)
		if !structural {
			t.Error("a reopen is structural")
		}

		// A person closes it; the agents disagree again and are only counted.
		if _, err := hs.core.DB().Exec("UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolved_by_member_uuid` = ? WHERE `id` = ?",
			int64(enums.CONFLICT_STATUS_RESOLVED), int64(enums.CONFLICT_RESOLUTION_COORDINATED), hs.decMember(t, w.ana), c.id); err != nil {
			t.Fatal(err)
		}
		env, _ = hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "build the login screen again"})
		pairs = dupPairs(t, env)
		if len(pairs) != 1 {
			t.Fatalf("the third rewording should earn a pair: %+v", env)
		}
		env, text = w.judge(t, w.bob, pairs[0].PairKey, "conflict", 0.9)
		t.Logf("Bob conflict, silenced: %s", text)
		got = hs.onlyDupConflict(t)
		if enums.ConflictStatus(got.status) != enums.CONFLICT_STATUS_RESOLVED || got.occurrences != 3 || len(env.Conflicts) != 0 ||
			env.Note != fmt.Sprintf("judged %s against %s: same work (0.90); a person already settled this one, so it stays closed", w.bKey, w.aKey) {
			t.Errorf("silenced: row=%+v note=%q", got, env.Note)
		}
	})
}

// TestIntegrationDuplicateWithPathOverlap: a path overlap between the two
// sessions lowers the wording bar to S 1, and the two kinds are two rows.
func TestIntegrationDuplicateWithPathOverlap(t *testing.T) {
	hs := newDuplicateHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decDeclare(t, ana, DeclareIntentParams{Summary: "navbar icons", Paths: []string{"web/src/components/Navbar.tsx"}})
	env, text := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "navbar spacing tweaks", Paths: []string{"web/src/components/Navbar.tsx"}})
	t.Logf("Bob declare_intent: %s", text)
	pairs := dupPairs(t, env)
	if len(env.Conflicts) != 1 || env.Conflicts[0].Kind != "path_overlap" || len(pairs) != 1 || pairs[0].Why != "words_and_paths" {
		t.Fatalf("want a path conflict and a words_and_paths pair: %s", text)
	}
	env, text = hs.decJudge(t, bob, ReportJudgementParams{PairKey: pairs[0].PairKey, Verdict: "conflict", Confidence: conf(0.8), Rationale: "both restyle the navbar"})
	t.Logf("Bob conflict: %s", text)
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ?", hs.teamID.String()); n != 2 {
		t.Errorf("%d conflicts, want the path overlap and the duplicate as two rows", n)
	}
	c := hs.onlyDupConflict(t)
	if c.evidence.OverlapPath.String != "web/src/components/navbar.tsx" || !strings.Contains(strings.Join(c.evidence.Adjusters, "|"), "|paths:web/src/components/navbar.tsx") {
		t.Errorf("evidence = %+v", c.evidence)
	}

	// Without the overlap, the same two summaries are not a candidate.
	hs2 := newDuplicateHarness(t)
	cai := hs2.contractAgent(t, "Cai", "client-c")
	dan := hs2.contractAgent(t, "Dan", "client-d")
	hs2.decDeclare(t, cai, DeclareIntentParams{Summary: "navbar icons", Paths: []string{"web/src/components/Icons.tsx"}})
	env, text = hs2.decDeclare(t, dan, DeclareIntentParams{Summary: "navbar spacing tweaks", Paths: []string{"web/src/components/Navbar.tsx"}})
	if len(dupPairs(t, env)) != 0 {
		t.Errorf("no claim overlap, S 1: not a candidate: %s", text)
	}
}

// ─────────────────────────────────────────────
// Concurrency and the lock budget (risk 1)
// ─────────────────────────────────────────────

var seedNouns = []string{"leaderboard", "checkout", "profile", "search", "upload", "billing", "invoice", "dashboard", "chat", "calendar"}
var seedThings = []string{"page", "endpoint", "schema", "export", "filters", "cache", "emails", "tests", "webhook", "settings"}

// TestIntegrationDuplicateLockHoldWithSeededProject: 8 agents declare
// near-identical summaries concurrently on a project seeded with 100 live
// intents. Exactly one judgement per pair key, a gapless sequence, and the
// p99 of metiche_team_lock_hold_ms under the 25ms tripwire.
func TestIntegrationDuplicateLockHoldWithSeededProject(t *testing.T) {
	hs := newDuplicateHarness(t)
	for i, noun := range seedNouns {
		a := hs.contractAgent(t, fmt.Sprintf("Seed%d", i), fmt.Sprintf("client-seed-%d", i))
		for j, thing := range seedThings {
			hs.decDeclare(t, a, DeclareIntentParams{
				Summary: fmt.Sprintf("%s %s", noun, thing),
				Paths:   []string{fmt.Sprintf("src/%s/%s_%d.go", noun, thing, j)},
			})
		}
	}
	live := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `intent` WHERE `team_uuid` = ? AND `status` IN (?, ?)",
		hs.teamID.String(), int64(enums.INTENT_STATUS_DECLARED), int64(enums.INTENT_STATUS_ACTIVE))
	if live != 100 {
		t.Fatalf("seeded %d live intents, want 100", live)
	}

	// A fresh handler so its histogram holds only the measured calls.
	measured := NewHandler(hs.core, zap.NewNop())
	measured.SetDetector(duplicateChain(hs))
	const agents, perAgent = 8, 6
	summaries := []string{"build the login screen", "add login page", "login page", "login form ui", "sign in page", "the login view"}
	var team []contractAgent
	for i := 0; i < agents; i++ {
		team = append(team, hs.contractAgent(t, fmt.Sprintf("Racer%d", i), fmt.Sprintf("client-race-%d", i)))
	}
	var wg sync.WaitGroup
	errs := make(chan error, agents*perAgent)
	start := make(chan struct{})
	for i, a := range team {
		wg.Add(1)
		go func(i int, a contractAgent) {
			defer wg.Done()
			<-start
			for k := 0; k < perAgent; k++ {
				_, _, err := measured.DeclareIntent(a.ctx, nil, DeclareIntentParams{SessionKey: a.key,
					Summary: summaries[(i+k)%len(summaries)], Paths: []string{fmt.Sprintf("web/src/login/%d_%d.tsx", i, k)}})
				if err != nil {
					errs <- err
				}
			}
		}(i, a)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	stats := measured.LockHoldStats()
	raw, _ := json.Marshal(stats)
	t.Logf("metiche_team_lock_hold_ms over %d concurrent declarations (100 live intents seeded): p50=%vms p95=%vms p99=%vms max=%.2fms mean=%.2fms\n%s",
		stats.Count, stats.P50MS, stats.P95MS, stats.P99MS, stats.MaxMS, stats.MeanMS, raw)
	if stats.Count != agents*perAgent {
		t.Errorf("histogram count %d, want %d", stats.Count, agents*perAgent)
	}
	if stats.P99MS >= LockHoldWarnMS {
		t.Errorf("lock hold p99 %vms is at or over the %dms tripwire", stats.P99MS, LockHoldWarnMS)
	}

	// Exactly one judgement per pair key, and per unordered pair of plans at
	// one pair of wordings.
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM (SELECT `pair_key` FROM `judgement` WHERE `team_uuid` = ? GROUP BY `pair_key` HAVING COUNT(*) > 1) d",
		hs.teamID.String()); n != 0 {
		t.Errorf("%d pair keys with more than one judgement", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM (SELECT LEAST(`subject_a_uuid`, `subject_b_uuid`) x, GREATEST(`subject_a_uuid`, `subject_b_uuid`) y "+
		"FROM `judgement` WHERE `team_uuid` = ? AND `kind` = ? GROUP BY x, y HAVING COUNT(*) > 1) d",
		hs.teamID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK)); n != 0 {
		t.Errorf("%d unordered plan pairs judged twice at one wording", n)
	}
	pairs := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `kind` = ?", hs.teamID.String(), int64(enums.CONFLICT_KIND_DUPLICATE_WORK))
	t.Logf("%d duplicate pairs minted", pairs)
	if pairs == 0 {
		t.Error("near-identical summaries should have been paired")
	}

	// A gapless sequence.
	var maxSeq, count, teamSeq int
	if err := hs.core.DB().QueryRow("SELECT MAX(`sequence`), COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", hs.teamID.String()).Scan(&maxSeq, &count); err != nil {
		t.Fatal(err)
	}
	_ = hs.core.DB().QueryRow("SELECT `sequence` FROM `team` WHERE `id` = ?", hs.teamID.String()).Scan(&teamSeq)
	var minSeq int
	_ = hs.core.DB().QueryRow("SELECT MIN(`sequence`) FROM `team_event` WHERE `team_uuid` = ?", hs.teamID.String()).Scan(&minSeq)
	if count != maxSeq-minSeq+1 || teamSeq != maxSeq {
		t.Errorf("sequence is not gapless: %d events over %d..%d, team.sequence %d", count, minSeq, maxSeq, teamSeq)
	}
}

// ─────────────────────────────────────────────
// Session keys across teams
// ─────────────────────────────────────────────

// TestIntegrationDuplicateToolsNeedTeamSlugAcrossTeams: a session key present
// on two teams: declare_intent, get_review_context and report_judgement each
// refuse a bare key and land on the named team.
func TestIntegrationDuplicateToolsNeedTeamSlugAcrossTeams(t *testing.T) {
	hs := newHarness(t)
	endpoint := duplicateServer(t, hs)
	slugA := hs.teamSlug(t)
	teamB, slugB, codeB := hs.otherTeam(t)
	ana := hs.join(t, "Ana", "client-a")
	hs.joinWith(t, ana.ctx, ana.token, codeB, "Ana", "client-a")
	cs := connectAs(t, endpoint, ana.token)
	a, b := startSessionOn(t, cs, slugA, nil), startSessionOn(t, cs, slugB, nil)
	rekey(t, hs, hs.teamID, map[string]string{a.Key: "S-77"})
	rekey(t, hs, teamB, map[string]string{b.Key: "S-77"})

	for tool, args := range map[string]map[string]any{
		"declare_intent":     {"session_key": "S-77", "summary": "add login page"},
		"get_review_context": {"session_key": "S-77"},
		"report_judgement":   {"session_key": "S-77", "pair_key": "nope", "verdict": "no_conflict"},
	} {
		text := mustRefuse(t, cs, tool, args)
		t.Logf("%s with a bare session_key: %s", tool, text)
		if !strings.Contains(text, slugA) || !strings.Contains(text, slugB) || !strings.Contains(text, "team_slug") {
			t.Errorf("%s should name both teams and team_slug: %s", tool, text)
		}
	}
	t.Logf("declare_intent on team B: %s", mustOK(t, cs, "declare_intent", map[string]any{"session_key": "S-77", "team_slug": slugB, "summary": "add login page"}))
	var team string
	if err := hs.core.DB().QueryRow("SELECT `team_uuid` FROM `intent` WHERE `summary` = ?", "add login page").Scan(&team); err != nil || team != teamB.String() {
		t.Errorf("the intent landed on %s (%v), want team B", team, err)
	}
	t.Logf("get_review_context on team B: %s", mustOK(t, cs, "get_review_context", map[string]any{"session_key": "S-77", "team_slug": slugB}))
	if text := mustRefuse(t, cs, "report_judgement", map[string]any{"session_key": "S-77", "team_slug": slugB, "pair_key": "nope", "verdict": "no_conflict"}); !strings.Contains(text, "is not a pair on this team") {
		t.Errorf("report_judgement on team B: %s", text)
	}
}

// ─────────────────────────────────────────────
// Two agents through the real transport
// ─────────────────────────────────────────────

// duplicateServer mounts the production wiring behind httptest.
func duplicateServer(t *testing.T, hs *harness) string {
	t.Helper()
	t.Setenv("METICHE_ROLE", "all")
	t.Setenv("METICHE_JOIN_PER_HOUR", "1000")
	r := chi.NewRouter()
	handler := Register(r, hs.core, zap.NewNop())
	handler.SetDetector(duplicateChain(hs))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { r.ServeHTTP(w, req) }))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/mcp"
}

// TestIntegrationDuplicateTwoAgentsOverTheTransport: the hackathon scenario
// over /v1/mcp with each agent's own token and no external_ref anywhere.
func TestIntegrationDuplicateTwoAgentsOverTheTransport(t *testing.T) {
	hs := newHarness(t)
	endpoint := duplicateServer(t, hs)
	slug := hs.teamSlug(t)
	ana := hs.join(t, "Ana", "client-ana")
	bob := hs.join(t, "Bob", "client-bob")
	csA, csB := connectAs(t, endpoint, ana.token), connectAs(t, endpoint, bob.token)

	// 1. Ana declares "add login page".
	sA := startSessionOn(t, csA, slug, map[string]any{"goal": "the login page"})
	text := mustOK(t, csA, "declare_intent", map[string]any{"session_key": sA.Key, "team_slug": slug,
		"summary": "add login page", "paths": []string{"web/src/routes/login.tsx"}})
	t.Logf("1. Ana declare_intent:\n%s", text)
	var declaredA Envelope
	decDecode(t, text, &declaredA)
	if len(declaredA.Review) != 0 {
		t.Fatalf("Ana declared first and must not be handed a pair: %s", text)
	}

	// 2. Bob declares "build the login screen" and is handed the pair.
	sB := startSessionOn(t, csB, slug, map[string]any{"goal": "the login screen"})
	text = mustOK(t, csB, "declare_intent", map[string]any{"session_key": sB.Key, "team_slug": slug,
		"summary": "build the login screen", "paths": []string{"web/src/pages/Login.tsx"}})
	t.Logf("2. Bob declare_intent:\n%s", text)
	var declaredB Envelope
	decDecode(t, text, &declaredB)
	pairs := dupPairs(t, declaredB)
	if len(pairs) != 1 || pairs[0].Kind != "duplicate_work" || pairs[0].Plan != declaredA.Key || pairs[0].Why != "words" || declaredB.Pending.Reviews != 1 {
		t.Fatalf("Bob's review pair: %s", text)
	}

	// 3. Bob gets the context and reports conflict 0.9.
	text = mustOK(t, csB, "get_review_context", map[string]any{"session_key": sB.Key, "team_slug": slug})
	t.Logf("3. Bob get_review_context:\n%s", text)
	var ctxOut ReviewContextResult
	decDecode(t, text, &ctxOut)
	if len(ctxOut.Reviews) != 1 || ctxOut.Reviews[0].Other == nil || ctxOut.Reviews[0].Other.Key != declaredA.Key {
		t.Fatalf("review context: %s", text)
	}
	text = mustOK(t, csB, "report_judgement", map[string]any{"session_key": sB.Key, "team_slug": slug,
		"pair_key": pairs[0].PairKey, "verdict": "conflict", "confidence": 0.9,
		"rationale": "both build the login page; " + declaredA.Key + " already has the route"})
	t.Logf("3. Bob report_judgement conflict 0.9:\n%s", text)
	var judged Envelope
	decDecode(t, text, &judged)
	if len(judged.Conflicts) != 1 {
		t.Fatalf("conflicts: %s", text)
	}
	cf := judged.Conflicts[0]
	if cf.Kind != "duplicate_work" || cf.AtFault != "later" || cf.DuplicateOf != declaredA.Key || cf.Severity != "medium" ||
		cf.With != "Ana (test)" || !strings.Contains(cf.SuggestedAction, "mark "+declaredB.Key+" superseded with update_intent") {
		t.Fatalf("Bob's conflict notice: %+v", cf)
	}

	// 4. Ana is not interrupted during the grace.
	text = mustOK(t, csA, "heartbeat", map[string]any{"session_key": sA.Key, "team_slug": slug})
	t.Logf("4. Ana heartbeat inside the grace:\n%s", text)
	var beat Envelope
	decDecode(t, text, &beat)
	if beat.Pending.Any() {
		t.Fatalf("Ana was interrupted during the grace: %+v", beat.Pending)
	}

	// 5. A path-only update does not re-ask.
	text = mustOK(t, csB, "update_intent", map[string]any{"session_key": sB.Key, "team_slug": slug, "intent_key": declaredB.Key,
		"add_paths": []string{"web/src/pages/LoginForm.tsx"}, "status_line": "laying out the form"})
	t.Logf("5. Bob path-only update_intent:\n%s", text)
	var pathOnly Envelope
	decDecode(t, text, &pathOnly)
	if len(pathOnly.Review) != 0 || pathOnly.Pending.Reviews != 0 {
		t.Fatalf("a path-only update must not ask again: %s", text)
	}
	if wr, _ := hs.wordingRevisionOf(t, declaredB.Key); wr != 1 {
		t.Fatalf("a path-only update moved wording_revision to %d", wr)
	}

	// 6. A real rewording bumps wording_revision and asks once more.
	text = mustOK(t, csB, "update_intent", map[string]any{"session_key": sB.Key, "team_slug": slug, "intent_key": declaredB.Key,
		"summary": "build the login page and its remember-me form"})
	t.Logf("6. Bob rewords:\n%s", text)
	var reworded Envelope
	decDecode(t, text, &reworded)
	again := dupPairs(t, reworded)
	if len(again) != 1 || again[0].PairKey == pairs[0].PairKey || reworded.Pending.Reviews != 1 {
		t.Fatalf("a rewording asks once more: %s", text)
	}
	if wr, _ := hs.wordingRevisionOf(t, declaredB.Key); wr != 2 {
		t.Fatalf("wording_revision = %d, want 2", wr)
	}

	// 7. Bob marks his plan superseded: the conflict settles with §4.10's note.
	text = mustOK(t, csB, "update_intent", map[string]any{"session_key": sB.Key, "team_slug": slug, "intent_key": declaredB.Key, "status": "superseded"})
	t.Logf("7. Bob supersedes %s:\n%s", declaredB.Key, text)
	var (
		status, resolution int64
		note               string
		resolvedAt         time.Time
	)
	if err := hs.core.DB().QueryRow("SELECT `status`, COALESCE(`resolution`, 0), COALESCE(`resolution_note`, ''), `resolved_at` FROM `conflict` "+
		"WHERE `team_uuid` = ? AND `key` = ?", hs.teamID.String(), cf.Key).Scan(&status, &resolution, &note, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	t.Logf("the settled conflict: status=%s resolution=%s\n%s", enums.ConflictStatus(status).String(), enums.ConflictResolution(resolution).String(), note)
	want := fmt.Sprintf("Settled by the agents: %s (test) marked %s superseded at %s, leaving %s (%s (test)) to build it.",
		sB.Key, declaredB.Key, clock(resolvedAt), declaredA.Key, sA.Key)
	if enums.ConflictResolution(resolution) != enums.CONFLICT_RESOLUTION_YIELDED || normClock(note) != normClock(want) {
		t.Errorf("resolution %s note %q\nwant %q", enums.ConflictResolution(resolution).String(), note, want)
	}

	// Ana was interrupted zero times over the whole run.
	whoA, err := hs.h.RequireSession(ana.ctx, sA.Key)
	if err != nil {
		t.Fatal(err)
	}
	if n := hs.decInstructionsFor(t, whoA.Session.ID); n != 0 {
		t.Errorf("Ana received %d instruction(s), want 0", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict_participant` WHERE `session_uuid` = ?", whoA.Session.ID.String()); n != 0 {
		t.Errorf("Ana is a participant on %d conflict(s), want 0", n)
	}
	_ = uuid.Nil
}

// ─────────────────────────────────────────────
// Re-judging an open conflict after a re-scope (§3.1 step 4b)
// ─────────────────────────────────────────────

// TestIntegrationDuplicateRescopeIsAskedOnceMore: a re-scope that stops
// resembling the other plan still earns one re-judge pair while the duplicate
// conflict is open, and its no_conflict converges it with §4.10's note.
func TestIntegrationDuplicateRescopeIsAskedOnceMore(t *testing.T) {
	w, c := raised(t)
	hs := w.hs
	env, text := hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "add password strength meter"})
	t.Logf("Bob re-scopes to unrelated words: %s", text)
	pairs := dupPairs(t, env)
	if len(pairs) != 1 || pairs[0].Plan != w.aKey || pairs[0].Why != "open_conflict" || pairs[0].PairKey == w.pair || env.Pending.Reviews != 1 {
		t.Fatalf("a re-scope under an open conflict must earn exactly one re-judge pair against %s: %s", w.aKey, text)
	}
	j := hs.dupJudgementByKey(t, pairs[0].PairKey)
	if j.a != w.aIntent || j.b != w.bIntent || j.aRev != 1 || j.bRev != 2 || j.judge.String != w.bob.session.String() {
		t.Errorf("re-judge pair = %+v", j)
	}
	out, raw := hs.decContext(t, w.bob, GetReviewContextParams{PairKey: pairs[0].PairKey})
	t.Logf("Bob get_review_context: %s", raw)
	if len(out.Reviews) != 1 || strings.Join(out.Reviews[0].Why, "|") != "the duplicate conflict "+c.key+" between these plans is still open: judge your plan as it is worded now" {
		t.Errorf("why = %q", out.Reviews[0].Why)
	}

	env, text = hs.decJudge(t, w.bob, ReportJudgementParams{PairKey: pairs[0].PairKey, Verdict: "no_conflict", Confidence: conf(0.95),
		Rationale: "now the password strength meter, not the login page"})
	t.Logf("Bob no_conflict: %s", text)
	if env.Key != c.key || env.Note != fmt.Sprintf("judged %s against %s: not the same work (0.95); %s settled", w.bKey, w.aKey, c.key) {
		t.Errorf("envelope = %+v", env)
	}
	got := hs.onlyDupConflict(t)
	want := fmt.Sprintf("Settled by the agents: %s (test) re-scoped %s at %s and judged it no longer duplicates %s: \"now the password strength meter, not the login page\".",
		w.bob.key, w.bKey, clock(hs.dupResolvedAt(t, c.id)), w.aKey)
	t.Logf("settled: resolution=%s\n%s", enums.ConflictResolution(got.resolution).String(), got.note)
	if enums.ConflictResolution(got.resolution) != enums.CONFLICT_RESOLUTION_CONVERGED || normClock(got.note) != normClock(want) {
		t.Errorf("resolution %d note %q\nwant %q", got.resolution, got.note, want)
	}

	// Settled: the next unrelated rewording is back behind the score gate.
	env, text = hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "add password strength meter with zxcvbn"})
	if len(dupPairs(t, env)) != 0 {
		t.Errorf("with the conflict settled, an unrelated rewording mints nothing: %s", text)
	}
}

// TestIntegrationDuplicateRescopeWithoutConflictMintsNothing: a plan with no
// open duplicate conflict rewording to unrelated words gets no pair, even
// with a live plan it once resembled.
func TestIntegrationDuplicateRescopeWithoutConflictMintsNothing(t *testing.T) {
	w := newDupWorld(t)
	w.judge(t, w.bob, w.pair, "no_conflict", 0.9)
	before := len(w.hs.dupJudgements(t))
	env, text := w.hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "add password strength meter"})
	t.Logf("Bob rewords with no open conflict: %s", text)
	if wr, _ := w.hs.wordingRevisionOf(t, w.bKey); wr != 2 {
		t.Fatalf("the rewording should bump wording_revision, got %d", wr)
	}
	if len(dupPairs(t, env)) != 0 || len(w.hs.dupJudgements(t)) != before {
		t.Errorf("no open conflict: the score gate holds and nothing is minted: %s", text)
	}
}

// TestIntegrationDuplicateRejudgeGoesFirstUnderTheCap: with one review slot
// left, the re-judge pair takes it and a new scored candidate is backlogged.
func TestIntegrationDuplicateRejudgeGoesFirstUnderTheCap(t *testing.T) {
	w, _ := raised(t)
	hs := w.hs
	cai := hs.contractAgent(t, "Cai", "client-c")
	envC, _ := hs.decDeclare(t, cai, DeclareIntentParams{Summary: "password strength meter"})
	if _, err := hs.core.DB().Exec("UPDATE `team` SET `settings` = ? WHERE `id` = ?", `{"max_reviews_per_minute":1}`, hs.teamID.String()); err != nil {
		t.Fatal(err)
	}
	env, text := hs.decUpdate(t, w.bob, UpdateIntentParams{IntentKey: w.bKey, Summary: "add password strength meter"})
	t.Logf("Bob re-scopes onto Cai's words, one slot left: %s", text)
	pairs := dupPairs(t, env)
	if len(pairs) != 1 || pairs[0].Plan != w.aKey || pairs[0].Why != "open_conflict" {
		t.Fatalf("the re-judge pair must take the only slot: %s", text)
	}
	caiIntent := hs.intentIDOf(t, envC.Key)
	backlogged := false
	for _, j := range hs.dupJudgements(t) {
		if j.a == caiIntent && j.b == w.bIntent {
			backlogged = !j.judge.Valid
		}
	}
	if !backlogged {
		t.Errorf("the new scored candidate (Cai's plan) should be minted and backlogged behind the re-judge pair")
	}
}

// TestIntegrationDuplicateIncumbentRewordIsRejudged: the incumbent rewording
// under an open conflict is asked too, found through the judgement before it
// is a participant.
func TestIntegrationDuplicateIncumbentRewordIsRejudged(t *testing.T) {
	w, _ := raised(t)
	env, text := w.hs.decUpdate(t, w.ana, UpdateIntentParams{IntentKey: w.aKey, Summary: "add oauth buttons"})
	t.Logf("Ana rewords under the open conflict: %s", text)
	pairs := dupPairs(t, env)
	if len(pairs) != 1 || pairs[0].Plan != w.bKey || pairs[0].Why != "open_conflict" {
		t.Fatalf("the incumbent's rewording earns a re-judge pair against %s: %s", w.bKey, text)
	}
}
