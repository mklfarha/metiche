package coordination

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeDecisionKey(t *testing.T) {
	ok := map[string]string{
		"#Auth JWT_cookie":       "#auth-jwt-cookie",
		"auth-jwt-cookie":        "#auth-jwt-cookie",
		"  #auth--jwt  cookie- ": "#auth-jwt-cookie",
		"-_api_errors_-":         "#api-errors",
		"abc":                    "#abc",
		strings.Repeat("a", 60):  "#" + strings.Repeat("a", 60),
	}
	for in, want := range ok {
		got, err := NormalizeDecisionKey(in)
		if err != nil || got != want {
			t.Errorf("NormalizeDecisionKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "#", "# "} {
		if _, err := NormalizeDecisionKey(in); !errors.Is(err, ErrDecisionKeyEmpty) {
			t.Errorf("NormalizeDecisionKey(%q) err = %v, want empty", in, err)
		}
	}
	for _, in := range []string{"ab", "---", "##auth", "auth.jwt", "auth/jwt", "ça-va", strings.Repeat("a", 61)} {
		if got, err := NormalizeDecisionKey(in); !errors.Is(err, ErrDecisionKeyInvalid) {
			t.Errorf("NormalizeDecisionKey(%q) = %q, %v; want invalid", in, got, err)
		}
	}
}

func TestDecisionContentHash(t *testing.T) {
	base := DecisionContentHash("Auth is a JWT", "Sessions are a signed JWT in a cookie.", []string{"internal/auth/**", "web/src/auth/**"}, false)
	if len(base) != 64 {
		t.Fatalf("hash length %d", len(base))
	}
	if got := DecisionContentHash("Auth is a JWT", "Sessions are a signed JWT in a cookie.", []string{"web/src/auth/**", "internal/auth/**", "web/src/auth/**"}, false); got != base {
		t.Error("scope order and duplicates must not change the hash")
	}
	if got := DecisionContentHash("Auth is a JWT", "Sessions are a signed JWT in a cookie.", []string{"internal/auth/**", "web/src/auth/**"}, true); got == base {
		t.Error("team_wide must change the hash")
	}
	if got := DecisionContentHash("Auth is a JWT", "Sessions are a signed JWT in a header.", []string{"internal/auth/**", "web/src/auth/**"}, false); got == base {
		t.Error("the statement must change the hash")
	}
	if got := DecisionContentHash("Auth is a JWT", "Sessions are a signed JWT in a cookie.", []string{"internal/auth/**"}, false); got == base {
		t.Error("the scope must change the hash")
	}
}

func TestRankDecisionCandidates(t *testing.T) {
	in := []DecisionCandidate{
		{DecisionKey: "#w", IntentKey: "INT-2", Signal: SignalWords, SharedTokens: 5},
		{DecisionKey: "#a", IntentKey: "INT-2", Signal: SignalAlwaysShow},
		{DecisionKey: "#s2", IntentKey: "INT-3", Signal: SignalScope, SharedTokens: 1},
		{DecisionKey: "#s1", IntentKey: "INT-9", Signal: SignalScope, SharedTokens: 2},
		{DecisionKey: "#s0", IntentKey: "INT-1", Signal: SignalScope, SharedTokens: 1},
		{DecisionKey: "#w2", IntentKey: "INT-1", Signal: SignalWords, SharedTokens: 2},
	}
	got := RankDecisionCandidates(in, 10)
	want := []string{"#s1", "#s0", "#s2", "#a", "#w", "#w2"}
	for i, k := range want {
		if got[i].DecisionKey != k {
			t.Fatalf("rank %d = %s, want %s (all: %+v)", i, got[i].DecisionKey, k, got)
		}
	}
	if in[0].DecisionKey != "#w" {
		t.Error("the input slice was reordered")
	}
	if capped := RankDecisionCandidates(in, 3); len(capped) != 3 || capped[2].DecisionKey != "#s2" {
		t.Errorf("cap 3 = %+v", capped)
	}
	if SignalScope.String() != "scope" || SignalAlwaysShow.String() != "always_show" || SignalWords.String() != "words" {
		t.Error("signal names are the review block's why")
	}
}

func TestJudgedSeverity(t *testing.T) {
	cases := []struct {
		name string
		in   JudgedSeverityInput
		want Severity
	}{
		{"unsure is low", JudgedSeverityInput{Verdict: "unsure", Confidence: 0.99, Requested: SeverityHigh}, SeverityLow},
		{"unsure on always-show is still low", JudgedSeverityInput{Verdict: "unsure", AlwaysShow: true}, SeverityLow},
		{"0.69 is low", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.69, Requested: SeverityHigh}, SeverityLow},
		{"0.70 is not", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.70}, SeverityMedium},
		{"default medium", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9}, SeverityMedium},
		{"requested high", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityHigh}, SeverityHigh},
		{"critical clamps to high", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityCritical}, SeverityHigh},
		{"requested low", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityLow}, SeverityLow},
		{"same member -1", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityHigh, SameMember: true}, SeverityMedium},
		{"same member floor low", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityLow, SameMember: true}, SeverityLow},
		{"same member medium to low", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, SameMember: true}, SeverityLow},
		{"always-show floor medium", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityLow, AlwaysShow: true}, SeverityMedium},
		{"always-show after same member", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, SameMember: true, AlwaysShow: true}, SeverityMedium},
		{"always-show keeps high", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityHigh, AlwaysShow: true}, SeverityHigh},
		{"always-show does not lift a low-confidence verdict", JudgedSeverityInput{Verdict: "conflict", Confidence: 0.5, AlwaysShow: true}, SeverityLow},
		{"no_conflict earns nothing", JudgedSeverityInput{Verdict: "no_conflict", Confidence: 0.9}, SeverityNone},
	}
	for _, tc := range cases {
		if got := JudgedSeverity(tc.in); got != tc.want {
			t.Errorf("%s: JudgedSeverity(%+v) = %s, want %s", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestEscalationBudget(t *testing.T) {
	for cadence, want := range map[string]time.Duration{
		"hackathon": 10 * time.Minute,
		"sprint":    30 * time.Minute,
		"steady":    2 * time.Hour,
		"":          30 * time.Minute,
		"invalid":   30 * time.Minute,
	} {
		if got := EscalationBudget(cadence); got != want {
			t.Errorf("EscalationBudget(%q) = %v, want %v", cadence, got, want)
		}
	}
}

func TestShouldEscalate(t *testing.T) {
	now := time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC)
	open := now.Add(-10 * time.Minute)
	if !ShouldEscalate(SeverityHigh, SeverityHigh, open, now, "hackathon", true, false) {
		t.Error("high at the high floor, exactly one budget old, live plan: must escalate")
	}
	if ShouldEscalate(SeverityHigh, SeverityHigh, open.Add(time.Second), now, "hackathon", true, false) {
		t.Error("one second short of the budget must not escalate")
	}
	if ShouldEscalate(SeverityMedium, SeverityHigh, open, now, "hackathon", true, false) {
		t.Error("below the human floor must not escalate")
	}
	if !ShouldEscalate(SeverityMedium, SeverityMedium, open, now, "hackathon", true, false) {
		t.Error("at a medium floor, medium must escalate")
	}
	if ShouldEscalate(SeverityMedium, SeverityNone, open, now, "hackathon", true, false) {
		t.Error("an unset floor is high")
	}
	if ShouldEscalate(SeverityHigh, SeverityHigh, open, now, "hackathon", false, false) {
		t.Error("a finished plan must not escalate")
	}
	if ShouldEscalate(SeverityHigh, SeverityHigh, open, now, "hackathon", true, true) {
		t.Error("an escalated conflict must not escalate again")
	}
	if ShouldEscalate(SeverityHigh, SeverityHigh, open, now, "sprint", true, false) {
		t.Error("ten minutes is inside the sprint budget")
	}
}
