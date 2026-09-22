package mcp

// Unit tests for duplicate work (docs/DUPLICATES.md §7.1, app/mcp part): the
// settlement rule, the wording limits, the shared review block and the golden
// bytes of a decision-only block. No database.
//
//	go test ./app/mcp/ -run 'Duplicate|ReviewRender|ReviewBlock' -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// decideDuplicateSettlement (§4.7)
// ─────────────────────────────────────────────

func dupLive(key string) duplicatePlanState {
	return duplicatePlanState{Found: true, Key: key, Status: enums.INTENT_STATUS_ACTIVE, SessionFound: true,
		Owner: settleSide{Key: "S-" + key, Label: "test", Status: enums.SESSION_STATUS_LIVE}}
}

func TestDecideDuplicateSettlement(t *testing.T) {
	with := func(f func(*duplicateSettleState)) duplicateSettleState {
		st := duplicateSettleState{A: dupLive("INT-83"), B: dupLive("INT-92")}
		f(&st)
		return st
	}
	agree := []settleNote{{SessionKey: "S-17", Outcome: "done", Text: "we split it: Bob takes the API", Agrees: true}}
	cases := []struct {
		name string
		st   duplicateSettleState
		kind DuplicateReleaseKind
		want enums.ConflictResolution
		rule int
		open bool
	}{
		{"1: b superseded", with(func(s *duplicateSettleState) { s.B.Status = enums.INTENT_STATUS_SUPERSEDED }), DuplicateReleaseIntentEnded, enums.CONFLICT_RESOLUTION_YIELDED, duplicateRuleYielded, false},
		{"1: b abandoned", with(func(s *duplicateSettleState) { s.B.Status = enums.INTENT_STATUS_ABANDONED }), DuplicateReleaseIntentEnded, enums.CONFLICT_RESOLUTION_YIELDED, duplicateRuleYielded, false},
		{"1: a abandoned", with(func(s *duplicateSettleState) { s.A.Status = enums.INTENT_STATUS_ABANDONED }), DuplicateReleaseIntentEnded, enums.CONFLICT_RESOLUTION_YIELDED, duplicateRuleYielded, false},
		{"1: a row gone", with(func(s *duplicateSettleState) { s.A.Found = false }), DuplicateReleaseJudged, enums.CONFLICT_RESOLUTION_YIELDED, duplicateRuleYielded, false},
		{"1 before 2: b done and a superseded", with(func(s *duplicateSettleState) {
			s.B.Status = enums.INTENT_STATUS_DONE
			s.A.Status = enums.INTENT_STATUS_SUPERSEDED
		}), DuplicateReleaseIntentEnded, enums.CONFLICT_RESOLUTION_YIELDED, duplicateRuleYielded, false},
		{"2: b done", with(func(s *duplicateSettleState) { s.B.Status = enums.INTENT_STATUS_DONE }), DuplicateReleaseIntentEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, duplicateRuleYieldDone, false},
		{"3: b session ended", with(func(s *duplicateSettleState) { s.B.Owner.Status = enums.SESSION_STATUS_ENDED }), DuplicateReleaseSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, duplicateRuleYieldSessionEnded, false},
		{"3: b session gone", with(func(s *duplicateSettleState) { s.B.SessionFound = false }), DuplicateReleaseSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, duplicateRuleYieldSessionEnded, false},
		{"4: a session abandoned", with(func(s *duplicateSettleState) { s.A.Owner.Status = enums.SESSION_STATUS_ABANDONED }), DuplicateReleaseSessionAbandoned, enums.CONFLICT_RESOLUTION_SUPERSEDED, duplicateRuleOtherSessionEnded, false},
		{"4 needs a not done: a done, its session ended, b live", with(func(s *duplicateSettleState) {
			s.A.Status = enums.INTENT_STATUS_DONE
			s.A.Owner.Status = enums.SESSION_STATUS_ENDED
		}), DuplicateReleaseSessionEnded, 0, 0, true},
		{"5: latest verdict no_conflict", with(func(s *duplicateSettleState) { s.Latest = enums.JUDGEMENT_VERDICT_NO_CONFLICT }), DuplicateReleaseJudged, enums.CONFLICT_RESOLUTION_CONVERGED, duplicateRuleConverged, false},
		{"1 before 5", with(func(s *duplicateSettleState) {
			s.Latest = enums.JUDGEMENT_VERDICT_NO_CONFLICT
			s.B.Status = enums.INTENT_STATUS_SUPERSEDED
		}), DuplicateReleaseIntentEnded, enums.CONFLICT_RESOLUTION_YIELDED, duplicateRuleYielded, false},
		{"stays open: latest conflict", with(func(s *duplicateSettleState) { s.Latest = enums.JUDGEMENT_VERDICT_CONFLICT }), DuplicateReleaseJudged, 0, 0, true},
		{"stays open: a done, b live", with(func(s *duplicateSettleState) { s.A.Status = enums.INTENT_STATUS_DONE }), DuplicateReleaseIntentEnded, 0, 0, true},
		{"coordinated: a note that agrees", with(func(s *duplicateSettleState) {
			s.B.Status = enums.INTENT_STATUS_SUPERSEDED
			s.Notes = agree
		}), DuplicateReleaseIntentEnded, enums.CONFLICT_RESOLUTION_COORDINATED, duplicateRuleYielded, false},
		{"never coordinated when metiche writes a session off", with(func(s *duplicateSettleState) {
			s.B.Owner.Status = enums.SESSION_STATUS_ABANDONED
			s.Notes = agree
		}), DuplicateReleaseSessionAbandoned, enums.CONFLICT_RESOLUTION_SUPERSEDED, duplicateRuleYieldSessionEnded, false},
		{"a note that does not agree", with(func(s *duplicateSettleState) {
			s.B.Status = enums.INTENT_STATUS_SUPERSEDED
			s.Notes = []settleNote{{SessionKey: "S-17", Outcome: "blocked", Text: "no"}}
		}), DuplicateReleaseIntentEnded, enums.CONFLICT_RESOLUTION_YIELDED, duplicateRuleYielded, false},
	}
	for _, c := range cases {
		res, rule, ok := decideDuplicateSettlement(c.st, c.kind)
		t.Logf("%-58s -> %s rule %d settled=%v", c.name, res.String(), rule, ok)
		if c.open {
			if ok {
				t.Errorf("%s: settled as %s, want it to stay open", c.name, res.String())
			}
			continue
		}
		if !ok || res != c.want || rule != c.rule {
			t.Errorf("%s: got %s rule %d ok=%v, want %s rule %d", c.name, res.String(), rule, ok, c.want.String(), c.rule)
		}
	}
}

// ─────────────────────────────────────────────
// Wording (§4.10)
// ─────────────────────────────────────────────

// TestDuplicateYieldActionExactText pins §3.3's example, byte for byte.
func TestDuplicateYieldActionExactText(t *testing.T) {
	got := duplicateYieldAction(duplicateActionInput{A: "INT-83", B: "INT-92", Who: "Ana (ui)", Member: "Ana",
		Status: "active", Age: "4m", Summary: "add login page", ASession: "S-17"})
	want := `INT-83 (Ana (ui), active 4m) is already building this: "add login page". Stop before you edit: mark INT-92 superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with Ana's agent (S-17) first.`
	t.Logf("other member:\n%s", got)
	if got != want {
		t.Errorf("suggested action:\n got %q\nwant %q", got, want)
	}
	same := duplicateYieldAction(duplicateActionInput{A: "INT-83", B: "INT-92", Who: "Ana (ui)", Member: "Ana",
		Status: "declared", Age: "just now", Summary: "add login page", ASession: "S-17", SameMember: true})
	wantSame := `INT-83 (S-17, your own person's other agent, declared just now) is already building this: "add login page". Stop before you edit: mark INT-92 superseded, or re-scope it and update_intent the summary. If yours should continue, settle it with S-17 first.`
	t.Logf("same member:\n%s", same)
	if same != wantSame {
		t.Errorf("same-member action:\n got %q\nwant %q", same, wantSame)
	}
	unsure := duplicateYieldAction(duplicateActionInput{A: "INT-83", B: "INT-92", Who: "Ana (ui)", Member: "Ana",
		Status: "active", Summary: "add login page", ASession: "S-17", Unsure: true})
	wantUnsure := `INT-92 may overlap INT-83 (Ana (ui)): "add login page". Nothing to do now; if it matters, ask Ana's agent (S-17) which part each of you takes, then update_intent.`
	t.Logf("unsure:\n%s", unsure)
	if unsure != wantUnsure {
		t.Errorf("unsure action:\n got %q\nwant %q", unsure, wantUnsure)
	}
}

// TestDuplicateWordingFitsItsLimits: every §4.10 string fits its column or
// field with maximal names and keys, and masks credential-shaped text.
func TestDuplicateWordingFitsItsLimits(t *testing.T) {
	long := strings.Repeat("W", 300)
	key := "INT-" + strings.Repeat("9", 40)
	secret := "use mtk_abcdefghijklmnop and Bearer abc.def.ghi and password=hunter2 " + long
	check := func(name, s string, max int) {
		t.Helper()
		n := utf8.RuneCountInString(s)
		t.Logf("%-22s %3d/%d runes", name, n, max)
		if n > max {
			t.Errorf("%s is %d runes, over %d: %q", name, n, max, s)
		}
		for _, leak := range []string{"mtk_abcdefghijklmnop", "abc.def.ghi", "hunter2"} {
			if strings.Contains(s, leak) {
				t.Errorf("%s leaks %q: %q", name, leak, s)
			}
		}
	}
	for _, same := range []bool{false, true} {
		for _, unsure := range []bool{false, true} {
			check("yield action", duplicateYieldAction(duplicateActionInput{A: key, B: key, Who: long, Member: long,
				Status: "declared", Age: "12h59m", Summary: secret, ASession: long, SameMember: same, Unsure: unsure}), resolutionNoteChars)
		}
	}
	check("incumbent action", DuplicateIncumbentAction(long, key, secret, long, "CF-"+strings.Repeat("1", 40)), instructionTextChars)
	check("incumbent (short)", DuplicateIncumbentAction("S-22 (frontend)", "INT-92", secret, "S-22", "CF-40"), instructionTextChars)
	y, i := DuplicateEscalationQuestions("CF-"+strings.Repeat("4", 40), key, key, long, long, secret, "2h")
	check("question, yield side", y, instructionTextChars)
	check("question, incumbent", i, instructionTextChars)

	// The quoted rationale gives way first, at most 60, and the sentence
	// always survives.
	short := DuplicateIncumbentAction("S-22 (frontend)", "INT-92", strings.Repeat("r", 200), "S-22", "")
	if !strings.HasSuffix(short, "settle the split with S-22.") || !strings.Contains(short, `rrr…"`) || utf8.RuneCountInString(short) != instructionTextChars {
		t.Errorf("incumbent action lost its sentence, or the quote did not give way: %q", short)
	}
	roomy := DuplicateIncumbentAction("S-2", "INT-9", strings.Repeat("r", 200), "S-2", "")
	if !strings.Contains(roomy, `"`+strings.Repeat("r", 59)+`…"`) {
		t.Errorf("the quote is capped at 60 runes even when there is room: %q", roomy)
	}
	withPath := DuplicateIncumbentAction("S-22 (frontend)", "INT-92", "both build the login page", "S-22", "CF-40")
	t.Logf("incumbent action with a folded path notice:\n%s", withPath)
	if !strings.HasSuffix(withPath, " (also CF-40)") {
		t.Errorf("the folded path conflict should be named when it fits: %q", withPath)
	}
	want := `S-22 (frontend) judged its plan INT-92 is the same work as yours: "both build the login page", and was told to stop. Carry on; if its part should stay, settle the split with S-22. (also CF-40)`
	if withPath != want {
		t.Errorf("incumbent action:\n got %q\nwant %q", withPath, want)
	}
	yq, iq := DuplicateEscalationQuestions("CF-44", "INT-92", "INT-83", "Bob", "Ana", "add login page", "10m")
	t.Logf("questions:\n%s\n%s", yq, iq)
	if yq != "CF-44: INT-92 duplicates INT-83 (Ana), unsettled 10m. Ask your person: stop INT-92, or agree with Ana who keeps it? Then report_back their answer." {
		t.Errorf("yield question = %q", yq)
	}
	if iq != `CF-44: Bob's agent is building the same thing as your INT-83, unsettled 10m. Ask your person who keeps it: "add login page". Then report_back their answer.` {
		t.Errorf("incumbent question = %q", iq)
	}
}

// TestDuplicateResolutionNotes: each §4.10 note, exact, and every one fits
// resolution_note with maximal names.
func TestDuplicateResolutionNotes(t *testing.T) {
	at := time.Date(2026, 9, 21, 14, 15, 2, 0, time.UTC)
	escalated := sql.NullTime{Time: time.Date(2026, 9, 21, 14, 12, 40, 0, time.UTC), Valid: true}
	base := func() duplicateSettleState {
		a, b := dupLive("INT-83"), dupLive("INT-92")
		a.Owner.Key, a.Owner.Label = "S-17", "ui"
		b.Owner.Key, b.Owner.Label = "S-22", "frontend"
		return duplicateSettleState{A: a, B: b}
	}
	rel := DuplicateRelease{Kind: DuplicateReleaseIntentEnded, At: at}

	st := base()
	st.B.Status = enums.INTENT_STATUS_SUPERSEDED
	st.EscalatedAt = escalated
	got := buildDuplicateResolutionNote(st, duplicateRuleYielded, enums.CONFLICT_RESOLUTION_YIELDED, rel)
	want := "Settled by the agents: S-22 (frontend) marked INT-92 superseded at 14:15 UTC, leaving INT-83 (S-17 (ui)) to build it. A person was asked at 14:12 UTC."
	t.Logf("YIELDED: %s", got)
	if got != want {
		t.Errorf("YIELDED note:\n got %q\nwant %q", got, want)
	}

	st = base()
	st.B.Status = enums.INTENT_STATUS_DONE
	got = buildDuplicateResolutionNote(st, duplicateRuleYieldDone, enums.CONFLICT_RESOLUTION_SUPERSEDED, rel)
	if want := "Settled by the agents: S-22 (frontend) finished INT-92 at 14:15 UTC anyway, so the work was duplicated; reconcile the two at merge."; got != want {
		t.Errorf("SUPERSEDED b done:\n got %q\nwant %q", got, want)
	}

	st = base()
	st.A.Owner.Status = enums.SESSION_STATUS_ENDED
	st.A.Owner.Outcome = enums.SESSION_OUTCOME_SUCCEEDED
	st.A.Owner.EndedAt = sql.NullTime{Time: at, Valid: true}
	got = buildDuplicateResolutionNote(st, duplicateRuleOtherSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, DuplicateRelease{Kind: DuplicateReleaseSessionEnded, At: at})
	if want := "Settled by the agents: S-17 (ui) ended its session at 14:15 UTC (succeeded), ending INT-83."; got != want {
		t.Errorf("SUPERSEDED session ended:\n got %q\nwant %q", got, want)
	}

	st = base()
	st.B.Owner.Status = enums.SESSION_STATUS_ABANDONED
	st.B.Owner.EndedAt = sql.NullTime{Time: at, Valid: true}
	got = buildDuplicateResolutionNote(st, duplicateRuleYieldSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED, DuplicateRelease{Kind: DuplicateReleaseSessionAbandoned, At: at})
	if want := "Cleared by metiche: S-22 (frontend) was abandoned at 14:15 UTC after no heartbeat, ending INT-92."; got != want {
		t.Errorf("SUPERSEDED abandoned:\n got %q\nwant %q", got, want)
	}

	st = base()
	st.Latest, st.JudgeName, st.JudgedKey, st.OtherKey = enums.JUDGEMENT_VERDICT_NO_CONFLICT, "S-22 (frontend)", "INT-92", "INT-83"
	st.Rationale = `now the "API call" behind the page, token=abc123`
	got = buildDuplicateResolutionNote(st, duplicateRuleConverged, enums.CONFLICT_RESOLUTION_CONVERGED, DuplicateRelease{Kind: DuplicateReleaseJudged, At: at})
	if want := `Settled by the agents: S-22 (frontend) re-scoped INT-92 at 14:15 UTC and judged it no longer duplicates INT-83: "now the 'API call' behind the page, token=[redacted]".`; got != want {
		t.Errorf("CONVERGED:\n got %q\nwant %q", got, want)
	}
	st.Rationale = ""
	got = buildDuplicateResolutionNote(st, duplicateRuleConverged, enums.CONFLICT_RESOLUTION_CONVERGED, DuplicateRelease{Kind: DuplicateReleaseJudged, At: at})
	if want := "Settled by the agents: S-22 (frontend) re-scoped INT-92 at 14:15 UTC and judged it no longer duplicates INT-83."; got != want {
		t.Errorf("CONVERGED without a rationale:\n got %q\nwant %q", got, want)
	}

	st = base()
	st.B.Status = enums.INTENT_STATUS_SUPERSEDED
	st.Notes = []settleNote{{SessionKey: "S-17", Text: "Bob takes the API, I keep the page", Agrees: true}}
	got = buildDuplicateResolutionNote(st, duplicateRuleYielded, enums.CONFLICT_RESOLUTION_COORDINATED, rel)
	if want := `Settled by the agents: S-22 (frontend) marked INT-92 superseded at 14:15 UTC, leaving INT-83 (S-17 (ui)) to build it. S-17 said: "Bob takes the API, I keep the page"`; got != want {
		t.Errorf("COORDINATED:\n got %q\nwant %q", got, want)
	}

	// Maximal: every rule within 400 runes, with the escalation tail kept.
	huge := strings.Repeat("x", 300)
	for rule := duplicateRuleYielded; rule <= duplicateRuleConverged; rule++ {
		st := base()
		st.A.Key, st.B.Key = "INT-"+huge, "INT-"+huge
		st.A.Owner.Key, st.B.Owner.Key, st.A.Owner.Label, st.B.Owner.Label = huge, huge, huge, huge
		st.B.Status = enums.INTENT_STATUS_SUPERSEDED
		st.Rationale, st.JudgeName = huge, huge
		st.Notes = []settleNote{{SessionKey: "S-1", Text: huge, Agrees: true}}
		st.EscalatedAt = escalated
		for _, res := range []enums.ConflictResolution{enums.CONFLICT_RESOLUTION_YIELDED, enums.CONFLICT_RESOLUTION_COORDINATED} {
			note := buildDuplicateResolutionNote(st, rule, res, rel)
			if n := utf8.RuneCountInString(note); n > resolutionNoteChars {
				t.Errorf("rule %d %s: %d runes", rule, res.String(), n)
			}
			if !strings.HasSuffix(note, " A person was asked at 14:12 UTC.") {
				t.Errorf("rule %d lost the escalation tail: %q", rule, note)
			}
		}
	}
}

func TestDuplicateConflictResolvedSummary(t *testing.T) {
	got := ConflictResolvedSummary(SettledConflict{Kind: enums.CONFLICT_KIND_DUPLICATE_WORK, Key: "CF-44",
		Resolution: enums.CONFLICT_RESOLUTION_YIELDED, PlanKeys: [2]string{"INT-92", "INT-83"}})
	if got != "CF-44 settled (yielded): INT-92 no longer duplicates INT-83" {
		t.Errorf("summary = %q", got)
	}
}

// ─────────────────────────────────────────────
// Keys
// ─────────────────────────────────────────────

func TestDuplicateWorkDedupeKeyIgnoresOrderAndCase(t *testing.T) {
	a, b := uuid.Must(uuid.NewV4()).String(), uuid.Must(uuid.NewV4()).String()
	k := DuplicateWorkDedupeKey(a, b)
	if k != DuplicateWorkDedupeKey(b, a) || k != DuplicateWorkDedupeKey(strings.ToUpper(b), strings.ToUpper(a)) {
		t.Error("the dedupe key must not depend on argument order or case")
	}
	if k == DecisionContradictionDedupeKey(a, b) {
		t.Error("a duplicate and a decision conflict on the same uuids must not share a dedupe key")
	}
	if len(k) != 64 {
		t.Errorf("dedupe key %q is not sha256 hex", k)
	}
	if duplicatePairKey(a, 1, b, 1) != duplicatePairKey(b, 1, a, 1) {
		t.Error("the pair key must be symmetric")
	}
	if duplicatePairKey(a, 1, b, 1) == duplicatePairKey(a, 1, b, 2) {
		t.Error("a wording_revision bump must mint a new pair key")
	}
}

func TestDuplicatePlanKeys(t *testing.T) {
	for _, c := range []struct {
		in   []string
		a, b string
		ok   bool
	}{
		{[]string{"plans:INT-83,INT-92", "words:login,screen"}, "INT-83", "INT-92", true},
		{[]string{"same_issue:ISSUE-412", "plans:INT-1,INT-2"}, "INT-1", "INT-2", true},
		{[]string{"words:login"}, "", "", false},
		{[]string{"plans:INT-83"}, "", "", false},
		{[]string{"plans:INT-83,INT-92,INT-99"}, "", "", false},
		{[]string{"plans:,INT-92"}, "", "", false},
		{nil, "", "", false},
	} {
		a, b, ok := DuplicatePlanKeys(c.in)
		if a != c.a || b != c.b || ok != c.ok {
			t.Errorf("DuplicatePlanKeys(%q) = %q, %q, %v", c.in, a, b, ok)
		}
	}
}

// ─────────────────────────────────────────────
// The shared review block
// ─────────────────────────────────────────────

// Captured from renderReviewBlock at 2ad1368, before duplicate work touched
// it: a decision-only block must stay byte for byte what it was.
const (
	goldenOneDecision  = `{"pairs":[{"pair_key":"9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e9c1e","decision":"#auth-jwt-cookie","statement":"Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.","why":"scope"}],"more":0,"answer_with":"report_judgement"}`
	goldenCutDecisions = `{"pairs":[{"pair_key":"a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1","decision":"#errors-envelope","why":"words"},{"pair_key":"b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2","decision":"#auth-jwt-cookie","why":"always_show"},{"pair_key":"c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3","decision":"#x","why":"scope"}],"more":0,"answer_with":"report_judgement"}`
)

func goldenDecisionPairs() ([]reviewBlockPair, []reviewBlockPair) {
	one := []reviewBlockPair{{PairKey: strings.Repeat("9c1e", 16), Decision: "#auth-jwt-cookie",
		Statement: "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.", Why: "scope"}}
	cut := []reviewBlockPair{
		{PairKey: strings.Repeat("a1", 32), Decision: "#errors-envelope", Statement: strings.Repeat("s", 500), Why: "words"},
		{PairKey: strings.Repeat("b2", 32), Decision: "#auth-jwt-cookie", Statement: strings.Repeat("t", 500), Why: "always_show"},
		{PairKey: strings.Repeat("c3", 32), Decision: "#x", Statement: "short", Why: "scope"},
	}
	return one, cut
}

// TestReviewBlockDecisionOnlyGolden: the block and the note lead of a
// response with only decision pairs, through the new renderer, are the bytes
// the decision reviewer wrote before this change.
func TestReviewBlockDecisionOnlyGolden(t *testing.T) {
	one, cut := goldenDecisionPairs()
	for _, c := range []struct {
		pairs []reviewBlockPair
		want  string
	}{{one, goldenOneDecision}, {cut, goldenCutDecisions}} {
		raw, ok := renderReviewBlock(c.pairs)
		if !ok || string(raw) != c.want {
			t.Errorf("decision-only block changed:\n got %s\nwant %s", raw, c.want)
		}
	}

	// Through the renderer hook, as the chain runs it.
	req := &pathDetectionRequest{ReviewPairs: one}
	m := &Mutation{Envelope: Envelope{Note: "INT-7 declared; holding 1 path(s) for write"}}
	if _, err := NewReviewRenderer()(withPathDetection(context.Background(), req), &TxContext{}, m); err != nil {
		t.Fatal(err)
	}
	if string(m.Envelope.Review) != goldenOneDecision {
		t.Errorf("rendered block = %s", m.Envelope.Review)
	}
	if want := "INT-7 declared; holding 1 path(s) for write; judge 1 pair(s) against your plan before you edit: see review, then report_judgement"; m.Envelope.Note != want {
		t.Errorf("note = %q\nwant %q", m.Envelope.Note, want)
	}
	env, err := json.Marshal(m.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decision-only envelope: %s", env)

	// review_block_enabled = false: no block, and the note says where to look.
	req = &pathDetectionRequest{ReviewPairs: one, reviewBlockOff: true}
	m = &Mutation{}
	renderReviewPairs(req, m)
	if m.Envelope.Review != nil || m.Envelope.Note != "judge 1 pair(s) against your plan before you edit: call get_review_context, then report_judgement" {
		t.Errorf("block off: review=%s note=%q", m.Envelope.Review, m.Envelope.Note)
	}
}

// TestReviewBlockMixedCutOrder: duplicate pairs first, then decision pairs;
// the first cut drops every statement and summary, the second drops pairs
// from the end, counted in more.
func TestReviewBlockMixedCutOrder(t *testing.T) {
	dup := func(k, summary string) reviewBlockPair {
		return reviewBlockPair{PairKey: strings.Repeat(k, 64)[:64], Kind: "duplicate_work", Plan: "INT-83", With: "Ana (ui)", Summary: summary, Why: "words"}
	}
	dec := func(k, statement string) reviewBlockPair {
		return reviewBlockPair{PairKey: strings.Repeat(k, 64)[:64], Decision: "#" + k, Statement: statement, Why: "scope"}
	}
	fits := []reviewBlockPair{dup("a", "add login page"), dec("b", "Errors are a JSON envelope.")}
	raw, ok := renderReviewBlock(fits)
	t.Logf("fits: %s", raw)
	if !ok || !strings.Contains(string(raw), `"kind":"duplicate_work","plan":"INT-83","with":"Ana (ui)","summary":"add login page","why":"words"`) ||
		!strings.Contains(string(raw), `"statement":"Errors are a JSON envelope."`) {
		t.Fatalf("a block that fits must carry everything: %s", raw)
	}
	if strings.Index(string(raw), `"kind":"duplicate_work"`) > strings.Index(string(raw), `"decision"`) {
		t.Errorf("duplicate pairs come first: %s", raw)
	}

	long := []reviewBlockPair{dup("a", strings.Repeat("s", 270)), dup("b", strings.Repeat("u", 270)), dec("c", strings.Repeat("t", 400)), dec("d", strings.Repeat("v", 400))}
	raw, ok = renderReviewBlock(long)
	t.Logf("first cut: %d runes %s", utf8.RuneCount(raw), raw)
	var b reviewBlock
	if !ok || json.Unmarshal(raw, &b) != nil || len(b.Pairs) != 4 || b.More != 0 {
		t.Fatalf("the first cut should keep all four pairs: %s", raw)
	}
	for _, p := range b.Pairs {
		if p.Summary != "" || p.Statement != "" {
			t.Errorf("the first cut drops every summary and statement: %+v", p)
		}
	}

	var many []reviewBlockPair
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		many = append(many, dup(k, "x"))
	}
	raw, ok = renderReviewBlock(many)
	b = reviewBlock{}
	if !ok || json.Unmarshal(raw, &b) != nil || b.More == 0 || len(b.Pairs)+b.More != len(many) || utf8.RuneCount(raw) > reviewInlineChars {
		t.Fatalf("pairs past the budget are counted in more: %s", raw)
	}
	if b.Pairs[0].PairKey != many[0].PairKey {
		t.Errorf("pairs are dropped from the end: %s", raw)
	}
	t.Logf("second cut: %d pairs, more %d, %d runes", len(b.Pairs), b.More, utf8.RuneCount(raw))
}

// TestReviewRenderRunsOnce: a chain with the renderer and ChainDetectors'
// own tail render appends the note lead once.
func TestReviewRenderRunsOnce(t *testing.T) {
	one, _ := goldenDecisionPairs()
	req := &pathDetectionRequest{}
	appendPair := func(context.Context, *TxContext, *Mutation) ([]ConflictNotice, error) {
		req.ReviewPairs = append(req.ReviewPairs, one...)
		return nil, nil
	}
	m := &Mutation{}
	if _, err := ChainDetectors(appendPair, NewReviewRenderer())(withPathDetection(context.Background(), req), &TxContext{}, m); err != nil {
		t.Fatal(err)
	}
	if strings.Count(m.Envelope.Note, "judge 1 pair(s)") != 1 {
		t.Errorf("note lead rendered %d times: %q", strings.Count(m.Envelope.Note, "judge"), m.Envelope.Note)
	}
	// Without a renderer, the chain's tail renders.
	req = &pathDetectionRequest{}
	m = &Mutation{}
	if _, err := ChainDetectors(appendPair)(withPathDetection(context.Background(), req), &TxContext{}, m); err != nil {
		t.Fatal(err)
	}
	if string(m.Envelope.Review) != goldenOneDecision {
		t.Errorf("a chain with no renderer must still render the block: %s", m.Envelope.Review)
	}
}
