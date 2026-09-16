package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/enums"
)

// The pure half of decisions: what record_decision refuses, what settles a
// decision conflict, the wording of every note, and the review block's cut
// order. No database anywhere in this file (docs/DECISIONS.md §7.1).

func goodDecision() RecordDecisionParams {
	return RecordDecisionParams{
		Key:       "auth-jwt-cookie",
		Title:     "Auth is a JWT in an httpOnly cookie",
		Statement: "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.",
		Scope:     []string{"internal/auth/**", "web/src/auth/**"},
	}
}

// Every row of §3.1's error table, word for word.
func TestParseRecordDecisionErrors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(p *RecordDecisionParams)
		want string
	}{
		{"key empty", func(p *RecordDecisionParams) { p.Key = "  # " },
			"key is required — a short kebab-case name like 'auth-jwt-cookie', cited as #auth-jwt-cookie"},
		{"key invalid", func(p *RecordDecisionParams) { p.Key = "ab" },
			`key must be 3-60 lowercase letters, digits and dashes (got "ab")`},
		{"key too long", func(p *RecordDecisionParams) { p.Key = strings.Repeat("a", 61) },
			`key must be 3-60 lowercase letters, digits and dashes (got "` + strings.Repeat("a", 61) + `")`},
		{"key control character", func(p *RecordDecisionParams) { p.Key = "auth\x07jwt" },
			"key contains a control character"},
		{"title missing", func(p *RecordDecisionParams) { p.Title = "  " },
			"title is required — a few words naming what was decided"},
		{"title long", func(p *RecordDecisionParams) { p.Title = strings.Repeat("t", 141) },
			"title is 141 characters and the limit is 140"},
		{"title control character", func(p *RecordDecisionParams) { p.Title = "Auth\nis a JWT" },
			"title contains a control character"},
		{"statement missing", func(p *RecordDecisionParams) { p.Statement = "" },
			"statement is required — the rule the code must follow, in one or two sentences another agent can check its plan against"},
		{"statement short", func(p *RecordDecisionParams) { p.Statement = "use cookies" },
			"statement is 11 characters; write the rule itself in at least 20, not a label"},
		{"statement long", func(p *RecordDecisionParams) { p.Statement = strings.Repeat("s", 401) },
			"statement is 401 characters and the limit is 400 — it ships inline to other agents; put the reasoning in rationale"},
		{"statement control character", func(p *RecordDecisionParams) { p.Statement = "Sessions are a signed JWT\x00in a cookie." },
			"statement contains a control character"},
		{"rationale long", func(p *RecordDecisionParams) { p.Rationale = strings.Repeat("r", 2001) },
			"rationale is 2001 characters and the limit is 2000"},
		{"scope too long", func(p *RecordDecisionParams) {
			p.Scope = nil
			for i := 0; i < 17; i++ {
				p.Scope = append(p.Scope, "internal/auth/file"+string(rune('a'+i))+".go")
			}
		}, "scope has 17 paths and the limit is 16 — name the folders the decision governs, not every file"},
		{"scope path invalid", func(p *RecordDecisionParams) { p.Scope = []string{"/etc/passwd"} },
			`path "/etc/passwd": path is absolute; claims are repo-relative — scope paths are repo-relative, like internal/auth/** or web/src/api/**`},
		{"revoke with title", func(p *RecordDecisionParams) { p.Revoke = true },
			"revoke withdraws a decision by key; do not send title, statement or supersedes with it"},
		{"revoke with supersedes", func(p *RecordDecisionParams) {
			p.Revoke, p.Title, p.Statement = true, "", ""
			p.Supersedes = "auth-bearer-header"
		}, "revoke withdraws a decision by key; do not send title, statement or supersedes with it"},
		{"supersedes itself", func(p *RecordDecisionParams) { p.Supersedes = "#Auth_JWT cookie" },
			"a decision cannot supersede itself"},
		{"supersedes invalid", func(p *RecordDecisionParams) { p.Supersedes = "x" },
			`supersedes must be 3-60 lowercase letters, digits and dashes (got "x")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := goodDecision()
			tc.mut(&p)
			_, err := parseRecordDecision(p, coordination.DefaultIgnorePatterns(), true)
			if err == nil {
				t.Fatalf("accepted %+v", p)
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q\nwant     %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseRecordDecisionAccepts(t *testing.T) {
	p := goodDecision()
	p.Key = "#Auth JWT_cookie"
	p.Scope = append(p.Scope, "vendor/**", "web/src/auth/**") // ignored, and a duplicate
	p.Rationale = "the cookie is out of reach of injected scripts; mtk_supersecrettoken must never appear"
	in, err := parseRecordDecision(p, coordination.DefaultIgnorePatterns(), true)
	if err != nil {
		t.Fatal(err)
	}
	if in.Key != "#auth-jwt-cookie" {
		t.Errorf("key = %q", in.Key)
	}
	if len(in.Scope) != 2 {
		t.Errorf("scope = %v, want the two real paths (vendor ignored, duplicate dropped)", scopePatterns(in.Scope))
	}
	if strings.Contains(in.Rationale, "mtk_supersecrettoken") || !strings.Contains(in.Rationale, "[redacted]") {
		t.Errorf("the rationale reaches the board unsanitized: %q", in.Rationale)
	}
	// Title tokens weigh 2, statement-only tokens 1, capped at 32.
	weights := map[string]int{}
	for _, tok := range in.Tokens {
		weights[tok.Token] = tok.Weight
	}
	if weights["jwt"] != 2 || weights["cookie"] != 2 {
		t.Errorf("title tokens should weigh 2: %+v", in.Tokens)
	}
	if weights["storage"] != 1 {
		t.Errorf("statement-only tokens should weigh 1: %+v", in.Tokens)
	}
	if len(in.Tokens) > decisionMaxTokens {
		t.Errorf("%d tokens, the cap is %d", len(in.Tokens), decisionMaxTokens)
	}
	if in.Hash == "" || len(in.Hash) != 64 {
		t.Errorf("content hash = %q", in.Hash)
	}

	// A revoke reads only key and rationale.
	rev := RecordDecisionParams{Key: "auth-jwt-cookie", Revoke: true, Rationale: "moving to server-side sessions"}
	got, err := parseRecordDecision(rev, nil, true)
	if err != nil || !got.Revoke || got.Key != "#auth-jwt-cookie" || got.Rationale == "" {
		t.Fatalf("revoke = %+v, err %v", got, err)
	}
}

// ─────────────────────────────────────────────
// Settlement (§4.5)
// ─────────────────────────────────────────────

func liveSettleState() decisionSettleState {
	return decisionSettleState{
		DecisionFound: true, DecisionKey: "#auth-jwt-cookie", DecisionStatus: enums.DECISION_STATUS_ACCEPTED, DecisionRevision: 1,
		IntentFound: true, IntentKey: "INT-91", IntentStatus: enums.INTENT_STATUS_DECLARED,
		SessionFound: true, Owner: settleSide{Key: "S-22", Label: "ui", Status: enums.SESSION_STATUS_LIVE},
	}
}

func TestDecideDecisionSettlement(t *testing.T) {
	cases := []struct {
		name string
		mut  func(st *decisionSettleState)
		kind DecisionReleaseKind
		want enums.ConflictResolution
		rule int
		ok   bool
	}{
		{"everything still stands", func(*decisionSettleState) {}, DecisionReleaseJudged, enums.CONFLICT_RESOLUTION_INVALID, 0, false},
		{"a conflict verdict keeps it open", func(st *decisionSettleState) { st.CurrentVerdict = enums.JUDGEMENT_VERDICT_CONFLICT },
			DecisionReleaseJudged, enums.CONFLICT_RESOLUTION_INVALID, 0, false},
		{"decision superseded", func(st *decisionSettleState) { st.DecisionStatus = enums.DECISION_STATUS_SUPERSEDED },
			DecisionReleaseDecisionChanged, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleDecisionEnded, true},
		{"decision revoked", func(st *decisionSettleState) { st.DecisionStatus = enums.DECISION_STATUS_REVOKED },
			DecisionReleaseDecisionChanged, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleDecisionEnded, true},
		{"decision gone", func(st *decisionSettleState) { st.DecisionFound = false },
			DecisionReleaseJudged, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleDecisionEnded, true},
		{"intent done", func(st *decisionSettleState) { st.IntentStatus = enums.INTENT_STATUS_DONE },
			DecisionReleaseIntentEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleIntentEnded, true},
		{"intent abandoned", func(st *decisionSettleState) { st.IntentStatus = enums.INTENT_STATUS_ABANDONED },
			DecisionReleaseIntentEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleIntentEnded, true},
		{"intent superseded", func(st *decisionSettleState) { st.IntentStatus = enums.INTENT_STATUS_SUPERSEDED },
			DecisionReleaseIntentEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleIntentEnded, true},
		{"intent gone", func(st *decisionSettleState) { st.IntentFound = false },
			DecisionReleaseSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleIntentEnded, true},
		{"session ended", func(st *decisionSettleState) { st.Owner.Status = enums.SESSION_STATUS_ENDED },
			DecisionReleaseSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleSessionEnded, true},
		{"session abandoned", func(st *decisionSettleState) { st.Owner.Status = enums.SESSION_STATUS_ABANDONED },
			DecisionReleaseSessionAbandoned, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleSessionEnded, true},
		{"session gone", func(st *decisionSettleState) { st.SessionFound = false },
			DecisionReleaseSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleSessionEnded, true},
		{"converged", func(st *decisionSettleState) { st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT },
			DecisionReleaseJudged, enums.CONFLICT_RESOLUTION_CONVERGED, decisionRuleConverged, true},
		{"unsure keeps it open", func(st *decisionSettleState) { st.CurrentVerdict = enums.JUDGEMENT_VERDICT_UNSURE },
			DecisionReleaseJudged, enums.CONFLICT_RESOLUTION_INVALID, 0, false},
		{"coordinated beats converged", func(st *decisionSettleState) {
			st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT
			st.Notes = []settleNote{{SessionKey: "S-17", Text: "Ana's agent agreed to keep the cookie", Agrees: true}}
		}, DecisionReleaseJudged, enums.CONFLICT_RESOLUTION_COORDINATED, decisionRuleConverged, true},
		{"a note that agrees nothing does not coordinate", func(st *decisionSettleState) {
			st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT
			st.Notes = []settleNote{{SessionKey: "S-17", Text: "blocked on the API"}}
		}, DecisionReleaseJudged, enums.CONFLICT_RESOLUTION_CONVERGED, decisionRuleConverged, true},
		{"metiche writing off a session never coordinates", func(st *decisionSettleState) {
			st.Owner.Status = enums.SESSION_STATUS_ABANDONED
			st.Notes = []settleNote{{SessionKey: "S-17", Text: "we agreed to split it", Agrees: true}}
		}, DecisionReleaseSessionAbandoned, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleSessionEnded, true},
		// The order is load-bearing: a revoked decision supersedes even when
		// the current pair reads no_conflict.
		{"rule 1 before rule 4", func(st *decisionSettleState) {
			st.DecisionStatus = enums.DECISION_STATUS_REVOKED
			st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT
		}, DecisionReleaseDecisionChanged, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleDecisionEnded, true},
		{"rule 2 before rule 4", func(st *decisionSettleState) {
			st.IntentStatus = enums.INTENT_STATUS_DONE
			st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT
		}, DecisionReleaseIntentEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, decisionRuleIntentEnded, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := liveSettleState()
			tc.mut(&st)
			res, rule, ok := decideDecisionSettlement(st, tc.kind)
			if ok != tc.ok || res != tc.want || rule != tc.rule {
				t.Errorf("= (%s, rule %d, %v), want (%s, rule %d, %v)", res.String(), rule, ok, tc.want.String(), tc.rule, tc.ok)
			}
		})
	}
}

// ─────────────────────────────────────────────
// Wording (§4.8)
// ─────────────────────────────────────────────

func at(hh, mm int) time.Time { return time.Date(2026, 9, 15, hh, mm, 0, 0, time.UTC) }

// The §9.4 note, exactly.
func TestDecisionResolutionNotePlanRevised(t *testing.T) {
	st := liveSettleState()
	st.DecisionRevision = 2
	st.JudgeName = "S-22 (ui)"
	st.Rationale = "plan now relies on the httpOnly cookie"
	st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT
	st.EscalatedAt = sql.NullTime{Time: at(14, 12), Valid: true}
	got := buildDecisionResolutionNote(st, decisionRuleConverged, enums.CONFLICT_RESOLUTION_CONVERGED,
		DecisionRelease{Kind: DecisionReleaseJudged, At: at(14, 15)})
	want := `Settled by the agents: S-22 (ui) revised INT-91 at 14:15 UTC and judged it no longer contradicts #auth-jwt-cookie (r2): "plan now relies on the httpOnly cookie". A person was asked at 14:12 UTC.`
	if got != want {
		t.Errorf("note = %q\nwant    %q", got, want)
	}
}

func TestDecisionResolutionNoteDecisionRevised(t *testing.T) {
	st := liveSettleState()
	st.DecisionRevision = 2
	st.DecisionRevised = true
	st.DecisionUpdated = sql.NullTime{Time: at(13, 40), Valid: true}
	st.RecorderName = "S-17 (backend)"
	st.JudgeName = "S-22 (ui)"
	st.Rationale = "the plan now reads the cookie"
	st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT
	got := buildDecisionResolutionNote(st, decisionRuleConverged, enums.CONFLICT_RESOLUTION_CONVERGED,
		DecisionRelease{Kind: DecisionReleaseJudged, At: at(13, 45)})
	want := `Settled by the agents: S-17 (backend) revised #auth-jwt-cookie to r2 at 13:40 UTC, and S-22 (ui) judged INT-91 no longer contradicts it: "the plan now reads the cookie".`
	if got != want {
		t.Errorf("note = %q\nwant    %q", got, want)
	}
}

func TestDecisionResolutionNotesFitAndMaskSecrets(t *testing.T) {
	secrets := []string{"mtk_liveagenttoken", "hunter2", "abcdefsignature"}
	dirty := "plan keeps mtk_liveagenttoken, sends bearer abcdefsignature and sets password=hunter2 " + strings.Repeat("and more reasoning ", 30)

	build := func(rule int, res enums.ConflictResolution, kind DecisionReleaseKind, mut func(*decisionSettleState)) string {
		st := liveSettleState()
		st.Rationale = dirty
		st.SupersededByKey = "#auth-server-session"
		st.RecorderName, st.ActorName, st.JudgeName = "S-17 (backend)", "S-17 (backend)", "S-22 (ui)"
		st.DecisionUpdated = sql.NullTime{Time: at(13, 40), Valid: true}
		st.IntentEnded = sql.NullTime{Time: at(14, 1), Valid: true}
		st.Owner.EndedAt = sql.NullTime{Time: at(14, 2), Valid: true}
		st.Owner.Outcome = enums.SESSION_OUTCOME_SUCCEEDED
		st.Owner.OutcomeNote = "shipped the login screen with password=hunter2"
		st.Notes = []settleNote{{SessionKey: "S-17", Text: dirty, Agrees: true}}
		st.EscalatedAt = sql.NullTime{Time: at(14, 12), Valid: true}
		mut(&st)
		return buildDecisionResolutionNote(st, rule, res, DecisionRelease{Kind: kind, At: at(14, 15)})
	}

	cases := []struct {
		name   string
		note   string
		expect string
	}{
		{"superseded", build(decisionRuleDecisionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, DecisionReleaseDecisionChanged,
			func(st *decisionSettleState) { st.DecisionStatus = enums.DECISION_STATUS_SUPERSEDED }),
			"superseded #auth-jwt-cookie with #auth-server-session at 13:40 UTC"},
		{"revoked", build(decisionRuleDecisionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, DecisionReleaseDecisionChanged,
			func(st *decisionSettleState) { st.DecisionStatus = enums.DECISION_STATUS_REVOKED }),
			"revoked #auth-jwt-cookie at 13:40 UTC"},
		{"intent ended", build(decisionRuleIntentEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, DecisionReleaseIntentEnded,
			func(st *decisionSettleState) { st.IntentStatus = enums.INTENT_STATUS_DONE }),
			"S-22 (ui) marked INT-91 done at 14:01 UTC."},
		{"session ended", build(decisionRuleSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, DecisionReleaseSessionEnded,
			func(st *decisionSettleState) { st.Owner.Status = enums.SESSION_STATUS_ENDED }),
			"S-22 (ui) ended its session at 14:02 UTC (succeeded: shipped the login screen with password=[redacted]), ending INT-91."},
		{"abandoned", build(decisionRuleSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, DecisionReleaseSessionAbandoned,
			func(st *decisionSettleState) { st.Owner.Status = enums.SESSION_STATUS_ABANDONED }),
			"Cleared by metiche: S-22 (ui) was abandoned at 14:02 UTC after no heartbeat, ending INT-91."},
		{"converged", build(decisionRuleConverged, enums.CONFLICT_RESOLUTION_CONVERGED, DecisionReleaseJudged,
			func(st *decisionSettleState) { st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT }),
			"revised INT-91 at 14:15 UTC"},
		{"coordinated", build(decisionRuleConverged, enums.CONFLICT_RESOLUTION_COORDINATED, DecisionReleaseJudged,
			func(st *decisionSettleState) { st.CurrentVerdict = enums.JUDGEMENT_VERDICT_NO_CONFLICT }),
			"S-17 said:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("%s", tc.note)
			if n := utf8.RuneCountInString(tc.note); n > resolutionNoteChars {
				t.Errorf("note is %d characters, the column holds %d", n, resolutionNoteChars)
			}
			if !strings.Contains(tc.note, tc.expect) {
				t.Errorf("note does not say %q", tc.expect)
			}
			for _, s := range secrets {
				if strings.Contains(tc.note, s) {
					t.Errorf("note carries %q", s)
				}
			}
			if !strings.HasSuffix(tc.note, ".") && !strings.HasSuffix(tc.note, "…") && !strings.HasSuffix(tc.note, `"`) {
				t.Errorf("note does not end cleanly: %q", tc.note)
			}
		})
	}
}

func TestPlanOwnerAndDeciderActions(t *testing.T) {
	stmt := goodDecision().Statement
	live := planOwnerAction("INT-91", "#auth-jwt-cookie", stmt, "Ana", "S-17", false, false)
	t.Logf("decider with a live agent: %s", live)
	for _, must := range []string{"Your plan INT-91 breaks #auth-jwt-cookie", "update_intent with the new summary",
		"settle that with Ana's agent (S-17)", "only Ana or your person can change it."} {
		if !strings.Contains(live, must) {
			t.Errorf("action does not say %q", must)
		}
	}
	away := planOwnerAction("INT-91", "#auth-jwt-cookie", stmt, "Ana", "", false, false)
	if !strings.Contains(away, "ask your person: only Ana or your person agreeing can change it.") || strings.Contains(away, "S-17") {
		t.Errorf("no live agent: %s", away)
	}
	own := planOwnerAction("INT-91", "#auth-jwt-cookie", stmt, "Ana", "S-17", true, false)
	if !strings.Contains(own, "which your own person decided") || !strings.Contains(own, "revise #auth-jwt-cookie with record_decision") {
		t.Errorf("same member: %s", own)
	}
	unsure := planOwnerAction("INT-91", "#auth-jwt-cookie", stmt, "Ana", "S-17", false, true)
	if !strings.Contains(unsure, "does not settle whether INT-91 breaks it") || !strings.Contains(unsure, "Nothing to do now") {
		t.Errorf("unsure: %s", unsure)
	}
	for _, s := range []string{live, away, own, unsure} {
		if n := utf8.RuneCountInString(s); n > 400 {
			t.Errorf("suggested_action is %d characters, the column holds 400: %s", n, s)
		}
	}

	notice := deciderAction("S-22 (ui)", "INT-91", "plan stores password=hunter2 in localStorage; #auth-jwt-cookie forbids it")
	t.Logf("decider's notice action: %s", notice)
	if n := utf8.RuneCountInString(notice); n > instructionTextChars {
		t.Errorf("the decider's action is %d characters; get_instructions shows %d", n, instructionTextChars)
	}
	if strings.Contains(notice, "hunter2") {
		t.Error("the decider's notice carries a credential")
	}
	for _, must := range []string{"S-22 (ui) judged its plan INT-91 breaks your decision", "revise it with record_decision"} {
		if !strings.Contains(notice, must) {
			t.Errorf("notice does not say %q", must)
		}
	}
}

// ─────────────────────────────────────────────
// The review block's cut order (§3.2)
// ─────────────────────────────────────────────

func TestReviewBlockCutOrder(t *testing.T) {
	pair := func(key, statement string) reviewBlockPair {
		return reviewBlockPair{PairKey: strings.Repeat(key, 64)[:64], Decision: "#" + key, Statement: statement, Why: "scope"}
	}
	short := []reviewBlockPair{pair("a", "Sessions are a signed JWT in an httpOnly cookie."), pair("b", "Errors are a JSON envelope.")}
	raw, ok := renderReviewBlock(short)
	if !ok {
		t.Fatal("a small block was dropped")
	}
	var got reviewBlock
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Pairs) != 2 || got.More != 0 || got.Pairs[0].Statement == "" || got.AnswerWith != "report_judgement" {
		t.Fatalf("small block = %s", raw)
	}

	// Three full-length statements are over the budget: the statements go
	// first, and every pair survives.
	long := []reviewBlockPair{pair("a", strings.Repeat("s", 400)), pair("b", strings.Repeat("t", 400)), pair("c", strings.Repeat("u", 400))}
	raw, ok = renderReviewBlock(long)
	if !ok {
		t.Fatal("the block was dropped before the statements were")
	}
	got = reviewBlock{}
	_ = json.Unmarshal(raw, &got)
	if utf8.RuneCount(raw) > reviewInlineChars {
		t.Errorf("block is %d characters, the budget is %d", utf8.RuneCount(raw), reviewInlineChars)
	}
	if len(got.Pairs) != 3 || got.More != 0 {
		t.Fatalf("statements should be dropped before pairs: %s", raw)
	}
	for _, p := range got.Pairs {
		if p.Statement != "" {
			t.Errorf("statement survived the first cut: %s", raw)
		}
	}

	// Pairs whose own keys blow the budget are dropped from the end and
	// counted in more.
	huge := []reviewBlockPair{
		{PairKey: strings.Repeat("1", 64), Decision: "#" + strings.Repeat("d", 380), Why: "scope"},
		{PairKey: strings.Repeat("2", 64), Decision: "#" + strings.Repeat("e", 380), Why: "words"},
		{PairKey: strings.Repeat("3", 64), Decision: "#" + strings.Repeat("f", 380), Why: "words"},
	}
	raw, ok = renderReviewBlock(huge)
	if !ok {
		t.Fatal("the block was dropped when trimming pairs would have fitted")
	}
	got = reviewBlock{}
	_ = json.Unmarshal(raw, &got)
	if len(got.Pairs) != 2 || got.More != 1 || utf8.RuneCount(raw) > reviewInlineChars {
		t.Fatalf("pairs = %d more = %d size = %d", len(got.Pairs), got.More, utf8.RuneCount(raw))
	}

	// One pair that cannot fit at all: no block, and the note sends the agent
	// to get_review_context.
	if _, ok := renderReviewBlock([]reviewBlockPair{{PairKey: strings.Repeat("9", 64), Decision: strings.Repeat("z", 1400), Why: "scope"}}); ok {
		t.Error("a block over the budget with one pair left should be dropped")
	}
}

// ─────────────────────────────────────────────
// Keys, notes and the verdict's validation
// ─────────────────────────────────────────────

func TestDecisionContradictionDedupeKeyIgnoresRevisions(t *testing.T) {
	const decision = "11111111-1111-4111-8111-111111111111"
	const intent = "22222222-2222-4222-8222-222222222222"

	dedupe := DecisionContradictionDedupeKey(decision, intent)
	if len(dedupe) != 64 {
		t.Fatalf("dedupe key = %q", dedupe)
	}
	if again := DecisionContradictionDedupeKey(decision, intent); again != dedupe {
		t.Error("the dedupe key is not stable")
	}
	// The pair key is revision-scoped; the dedupe key is not, which is what
	// keeps one disagreement one conflict row across revisions.
	first := decisionPairKey(decision, 1, intent, 1)
	second := decisionPairKey(decision, 2, intent, 3)
	if first == second {
		t.Error("a revision bump must mint a new pair key")
	}
	if dedupe == first || dedupe == second {
		t.Error("the dedupe key must not be a pair key")
	}
	if other := DecisionContradictionDedupeKey(decision, "33333333-3333-4333-8333-333333333333"); other == dedupe {
		t.Error("another plan is another conflict")
	}
}

func TestNoteForPendingReviews(t *testing.T) {
	if got := (Pending{Reviews: 2}).NoteForPending(); got != "2 pair(s) to judge against your plan — call get_review_context, then report_judgement" {
		t.Errorf("NoteForPending = %q", got)
	}
}

func TestParseReportJudgement(t *testing.T) {
	conf := func(v float64) *float64 { return &v }
	good := ReportJudgementParams{PairKey: "9c1e", Verdict: "conflict", Confidence: conf(0.9), Rationale: "plan stores the token in localStorage"}

	cases := []struct {
		name string
		mut  func(p *ReportJudgementParams)
		want string
	}{
		{"pair_key empty", func(p *ReportJudgementParams) { p.PairKey = " " },
			"pair_key is required — send back the pair_key a review block or get_review_context gave you"},
		{"verdict invalid", func(p *ReportJudgementParams) { p.Verdict = "maybe" },
			`verdict must be conflict, no_conflict or unsure (got "maybe")`},
		{"confidence missing", func(p *ReportJudgementParams) { p.Confidence = nil },
			"confidence is required with a conflict verdict — how sure you are, 0 to 1"},
		{"confidence high", func(p *ReportJudgementParams) { p.Confidence = conf(1.5) },
			"confidence must be between 0 and 1 (got 1.5)"},
		{"confidence negative", func(p *ReportJudgementParams) { p.Confidence = conf(-0.2) },
			"confidence must be between 0 and 1 (got -0.2)"},
		{"severity invalid", func(p *ReportJudgementParams) { p.Severity = "critical" },
			`severity must be low, medium or high (got "critical")`},
		{"rationale missing on conflict", func(p *ReportJudgementParams) { p.Rationale = "" },
			"rationale is required with a conflict verdict — one line naming what in your plan meets what in the decision"},
		{"rationale missing on unsure", func(p *ReportJudgementParams) {
			p.Verdict, p.Rationale, p.Confidence = "unsure", "", nil
		}, "rationale is required with a unsure verdict — one line naming what in your plan meets what in the decision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			tc.mut(&p)
			if _, err := parseReportJudgement(p); err == nil || err.Error() != tc.want {
				t.Errorf("err = %v\nwant  %q", err, tc.want)
			}
		})
	}

	// no_conflict needs neither confidence nor rationale, and ignores severity.
	ok := ReportJudgementParams{PairKey: "9c1e", Verdict: "no_conflict", Severity: "nonsense"}
	got, err := parseReportJudgement(ok)
	if err != nil || got.Verdict != enums.JUDGEMENT_VERDICT_NO_CONFLICT || got.Confidence != nil {
		t.Fatalf("no_conflict = %+v, err %v", got, err)
	}
	// A conflict defaults to medium, and the rationale is sanitized and clipped.
	dirty := good
	dirty.Rationale = "stores password=hunter2 and mtk_livetoken in localStorage " + strings.Repeat("x", 400)
	parsed, err := parseReportJudgement(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Severity != coordination.SeverityMedium {
		t.Errorf("severity = %s, want medium", parsed.Severity)
	}
	if strings.Contains(parsed.Rationale, "hunter2") || strings.Contains(parsed.Rationale, "mtk_livetoken") {
		t.Errorf("rationale reaches the board unsanitized: %q", parsed.Rationale)
	}
	if n := utf8.RuneCountInString(parsed.Rationale); n > judgeRationaleChars {
		t.Errorf("rationale is %d characters, the column holds %d", n, judgeRationaleChars)
	}
}

// ─────────────────────────────────────────────
// ChainDetectors (§9.1)
// ─────────────────────────────────────────────

func TestChainDetectors(t *testing.T) {
	var order []string
	first := func(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
		order = append(order, "first")
		return []ConflictNotice{{Key: "CF-1"}}, nil
	}
	second := func(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
		order = append(order, "second")
		return []ConflictNotice{{Key: "CF-2"}}, nil
	}
	notices, err := ChainDetectors(nil, first, nil, second)(context.Background(), &TxContext{}, &Mutation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 || notices[0].Key != "CF-1" || notices[1].Key != "CF-2" {
		t.Errorf("notices = %+v", notices)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("hooks ran %v", order)
	}

	boom := errors.New("detection failed")
	order = nil
	_, err = ChainDetectors(first, func(context.Context, *TxContext, *Mutation) ([]ConflictNotice, error) {
		return nil, boom
	}, second)(context.Background(), &TxContext{}, &Mutation{})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the first error", err)
	}
	if strings.Join(order, ",") != "first" {
		t.Errorf("the chain kept going after an error: %v", order)
	}
}
