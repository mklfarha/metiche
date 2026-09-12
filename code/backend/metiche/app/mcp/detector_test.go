package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/enums"
)

// These tests need no database, and that is the point of them. The decision
// layer — confirm, score, apply the noise rules, write the suggested action,
// mint the dedupe key — is a pure function of two claim sides and the
// project's hotspot list. Anything in it that needed a query would be a rule
// that could not be reproduced from the conflict row later, when somebody
// asks why they were interrupted.

func mustPath(t *testing.T, pattern string) coordination.NormalizedPath {
	t.Helper()
	np, err := coordination.NormalizePath(pattern, nil, false)
	if err != nil {
		t.Fatalf("normalizing %q: %v", pattern, err)
	}
	return np
}

func side(t *testing.T, member, branch, pattern string, mode coordination.ClaimMode) claimSide {
	t.Helper()
	return claimSide{
		ClaimUUID:   "11111111-1111-1111-1111-11111111111" + member,
		SessionUUID: "22222222-2222-2222-2222-22222222222" + member,
		AgentUUID:   "33333333-3333-3333-3333-33333333333" + member,
		MemberUUID:  "member-" + member,
		SessionKey:  "S-" + member,
		MemberName:  "Person" + member,
		AgentLabel:  "agent" + member,
		Branch:      branch,
		Mode:        mode,
		Path:        mustPath(t, pattern),
	}
}

// TestAssessOverlapDecisions walks the severity matrix and every noise rule
// PLAN.md calls non-negotiable.
func TestAssessOverlapDecisions(t *testing.T) {
	cases := []struct {
		name     string
		mine     claimSide
		theirs   claimSide
		hotspots []string

		wantConflict bool
		wantSeverity coordination.Severity
		wantNotify   bool
		wantInstruct bool
		wantAdjuster string
	}{
		{
			// The first noise rule, and the one that would otherwise produce
			// most of the traffic: everybody reads everything.
			name:         "read x read is never a conflict",
			mine:         side(t, "1", "feat/a", "internal/auth/token.go", coordination.ModeRead),
			theirs:       side(t, "2", "feat/b", "internal/auth/**", coordination.ModeRead),
			wantConflict: false,
		},
		{
			name:         "no overlap at all",
			mine:         side(t, "1", "feat/a", "internal/auth/token.go", coordination.ModeWrite),
			theirs:       side(t, "2", "feat/b", "web/ui/login.tsx", coordination.ModeWrite),
			wantConflict: false,
		},
		{
			name:         "two writers on the identical file is critical",
			mine:         side(t, "1", "feat/a", "internal/auth/token.go", coordination.ModeWrite),
			theirs:       side(t, "2", "feat/b", "internal/auth/token.go", coordination.ModeWrite),
			wantConflict: true,
			wantSeverity: coordination.SeverityCritical,
			wantNotify:   true,
			wantInstruct: true,
		},
		{
			name:         "write x write through a glob is high",
			mine:         side(t, "1", "feat/a", "internal/auth/token.go", coordination.ModeWrite),
			theirs:       side(t, "2", "feat/b", "internal/auth/**", coordination.ModeWrite),
			wantConflict: true,
			wantSeverity: coordination.SeverityHigh,
			wantNotify:   true,
			wantInstruct: true,
		},
		{
			name:         "write x read is medium, which is exactly the notify floor",
			mine:         side(t, "1", "feat/a", "internal/auth/token.go", coordination.ModeWrite),
			theirs:       side(t, "2", "feat/b", "internal/auth/token.go", coordination.ModeRead),
			wantConflict: true,
			wantSeverity: coordination.SeverityMedium,
			wantNotify:   true,
			wantInstruct: true,
		},
		{
			// A rename breaks the reader silently: nothing in their working
			// copy changes, their build just stops.
			name:         "structural against a mere reader is high",
			mine:         side(t, "1", "feat/a", "internal/auth/token.go", coordination.ModeStructural),
			theirs:       side(t, "2", "feat/b", "internal/auth/token.go", coordination.ModeRead),
			wantConflict: true,
			wantSeverity: coordination.SeverityHigh,
			wantNotify:   true,
			wantInstruct: true,
		},
		{
			// One person's two agents are coordinated by that person.
			// Softened by one, and no interrupt — the instruction is the
			// interrupt, and it is not raised.
			name:         "same member softens and never interrupts",
			mine:         side(t, "1", "feat/a", "internal/auth/token.go", coordination.ModeWrite),
			theirs:       sameMember(side(t, "2", "feat/b", "internal/auth/token.go", coordination.ModeWrite), "member-1"),
			wantConflict: true,
			wantSeverity: coordination.SeverityHigh,
			wantNotify:   true,
			wantInstruct: false,
			wantAdjuster: coordination.PathAdjusterSameMember,
		},
		{
			name:         "same branch softens, because git will show them at merge",
			mine:         side(t, "1", "feat/shared", "internal/auth/token.go", coordination.ModeWrite),
			theirs:       side(t, "2", "feat/shared", "internal/auth/token.go", coordination.ModeWrite),
			wantConflict: true,
			wantSeverity: coordination.SeverityHigh,
			wantNotify:   true,
			wantInstruct: true,
			wantAdjuster: coordination.PathAdjusterSameBranch,
		},
		{
			// "I am working in src/" overlaps everybody. Recorded at low so
			// the board still has it, and deliberately below the notify floor
			// so nobody is interrupted by it.
			name:         "an over-broad claim is capped at low and does not interrupt",
			mine:         side(t, "1", "feat/a", "internal/auth/token.go", coordination.ModeWrite),
			theirs:       side(t, "2", "feat/b", "internal/**", coordination.ModeWrite),
			wantConflict: true,
			wantSeverity: coordination.SeverityLow,
			wantNotify:   false,
			wantInstruct: false,
			wantAdjuster: coordination.PathAdjusterBroadClaim,
		},
		{
			// Two edits to go.mod merge cleanly and are still wrong.
			name:         "a hotspot escalates",
			mine:         side(t, "1", "feat/a", "go.mod", coordination.ModeWrite),
			theirs:       side(t, "2", "feat/b", "go.mod", coordination.ModeRead),
			hotspots:     coordination.DefaultHotspotPatterns(),
			wantConflict: true,
			wantSeverity: coordination.SeverityHigh, // medium + 1
			wantNotify:   true,
			wantInstruct: true,
			wantAdjuster: coordination.PathAdjusterHotspot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assessOverlap(tc.mine, tc.theirs, tc.hotspots)
			if got.Conflict != tc.wantConflict {
				t.Fatalf("Conflict = %v, want %v (severity %s)", got.Conflict, tc.wantConflict, got.Severity)
			}
			if !tc.wantConflict {
				return
			}
			if got.Severity != tc.wantSeverity {
				t.Errorf("Severity = %s, want %s (adjusters %v)", got.Severity, tc.wantSeverity, got.Adjusters)
			}
			if got.NotifyInitiator != tc.wantNotify {
				t.Errorf("NotifyInitiator = %v, want %v", got.NotifyInitiator, tc.wantNotify)
			}
			if got.InstructIncumbent != tc.wantInstruct {
				t.Errorf("InstructIncumbent = %v, want %v", got.InstructIncumbent, tc.wantInstruct)
			}
			if tc.wantAdjuster != "" && !hasAdjuster(got.Adjusters, tc.wantAdjuster) {
				t.Errorf("adjusters = %v, want %q among them", got.Adjusters, tc.wantAdjuster)
			}
			// The rule that makes the difference between noise and signal,
			// asserted on every single conflict this test produces.
			if strings.TrimSpace(got.ActionForInitiator) == "" {
				t.Error("a conflict was surfaced with no suggested action for the caller")
			}
			if strings.TrimSpace(got.ActionForIncumbent) == "" {
				t.Error("a conflict was surfaced with no suggested action for the other agent")
			}
			if got.DedupeKey == "" {
				t.Error("a conflict was recorded with no dedupe key, so re-detection would insert a second row")
			}
			if got.Rule == "" || len(got.Rule) > 48 {
				t.Errorf("detector_rule = %q, want a non-empty name that fits the column", got.Rule)
			}
			t.Logf("%s -> %s via %s: %s", tc.name, got.Severity, got.Rule, got.ActionForInitiator)
		})
	}
}

func sameMember(s claimSide, member string) claimSide {
	s.MemberUUID = member
	return s
}

// TestAssessOverlapIsOrderIndependent is the property that keeps one
// collision from becoming two rows.
//
// Whichever agent declares second is the one that runs detection, so the same
// pair arrives in either order depending on who was slower. An
// order-sensitive dedupe key would mint a second conflict for the same pair
// the moment the first agent re-declared.
func TestAssessOverlapIsOrderIndependent(t *testing.T) {
	a := side(t, "1", "feat/a", "internal/auth/**", coordination.ModeWrite)
	b := side(t, "2", "feat/b", "internal/auth/token.go", coordination.ModeWrite)

	first := assessOverlap(a, b, nil)
	second := assessOverlap(b, a, nil)

	if !first.Conflict || !second.Conflict {
		t.Fatalf("both directions must confirm: %v / %v", first.Conflict, second.Conflict)
	}
	if first.DedupeKey != second.DedupeKey {
		t.Errorf("dedupe key depends on argument order:\n a,b: %s\n b,a: %s", first.DedupeKey, second.DedupeKey)
	}
	if first.OverlapPath != second.OverlapPath {
		t.Errorf("overlap label depends on argument order: %q vs %q", first.OverlapPath, second.OverlapPath)
	}
	if first.Severity != second.Severity {
		t.Errorf("severity depends on argument order: %s vs %s", first.Severity, second.Severity)
	}
	// The label names the file, not the glob: that is what an agent can act
	// on, and what the two sides have to agree about.
	if first.OverlapPath != "internal/auth/token.go" {
		t.Errorf("overlap label = %q, want the exact file the two claims meet on", first.OverlapPath)
	}
}

// TestSuggestPathActionSpeaksToEachSide checks the halves are written for
// different readers. The same collision means "somebody is already there" to
// the agent that arrived second and "somebody just arrived" to the one that
// was already working.
func TestSuggestPathActionSpeaksToEachSide(t *testing.T) {
	mine := side(t, "1", "feat/login", "internal/auth/token.go", coordination.ModeWrite)
	theirs := side(t, "2", "feat/auth", "internal/auth/token.go", coordination.ModeWrite)
	theirs.HeldFor = 4 * time.Minute

	v := assessOverlap(mine, theirs, nil)

	if !strings.Contains(v.ActionForInitiator, "Person2") {
		t.Errorf("the caller's advice does not name who they collided with: %q", v.ActionForInitiator)
	}
	if !strings.Contains(v.ActionForInitiator, "4m") {
		t.Errorf("the caller's advice does not say how long the other claim has been held: %q", v.ActionForInitiator)
	}
	if !strings.Contains(v.ActionForInitiator, "feat/auth") {
		t.Errorf("the caller's advice does not name the other branch: %q", v.ActionForInitiator)
	}
	if !strings.Contains(v.ActionForIncumbent, "Person1") {
		t.Errorf("the incumbent's advice does not name who arrived: %q", v.ActionForIncumbent)
	}
	if v.ActionForInitiator == v.ActionForIncumbent {
		t.Error("both sides were given the same sentence; they are in different positions")
	}
	for _, s := range []string{v.ActionForInitiator, v.ActionForIncumbent} {
		if len(s) > 400 {
			t.Errorf("suggested action is %d characters and the column holds 400", len(s))
		}
	}
	t.Logf("initiator: %s", v.ActionForInitiator)
	t.Logf("incumbent: %s", v.ActionForIncumbent)
}

// TestSuggestedActionIsNeverEmpty covers the mode matrix directly, because
// the guarantee is absolute: PLAN.md says never surface a conflict without a
// suggested next action, and the column is nullable, so nothing but this
// enforces it.
func TestSuggestedActionIsNeverEmpty(t *testing.T) {
	modes := []coordination.ClaimMode{coordination.ModeRead, coordination.ModeWrite, coordination.ModeStructural}
	for _, mine := range modes {
		for _, theirs := range modes {
			for _, hotspots := range [][]string{nil, coordination.DefaultHotspotPatterns()} {
				a := side(t, "1", "feat/a", "go.mod", mine)
				b := side(t, "2", "feat/b", "go.mod", theirs)
				v := assessOverlap(a, b, hotspots)
				if !v.Conflict {
					continue
				}
				if strings.TrimSpace(v.ActionForInitiator) == "" || strings.TrimSpace(v.ActionForIncumbent) == "" {
					t.Errorf("%s x %s (hotspots=%v) produced a conflict with no advice", mine, theirs, hotspots != nil)
				}
			}
		}
	}
}

// TestOverlapLabelIsSymmetric pins the helper the dedupe key is built from.
func TestOverlapLabelIsSymmetric(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"internal/auth/token.go", "internal/auth/**", "internal/auth/token.go"},
		{"internal/**", "internal/auth/token.go", "internal/auth/token.go"},
		{"internal/auth/**", "internal/**", "internal/auth/**"},
		{"internal/auth/*.go", "internal/auth/*_test.go", "internal/auth/*.go"},
		{"internal/auth/token.go", "internal/auth/token.go", "internal/auth/token.go"},
	}
	for _, tc := range cases {
		a, b := mustPath(t, tc.a), mustPath(t, tc.b)
		got, back := overlapLabel(a, b), overlapLabel(b, a)
		if got != back {
			t.Errorf("overlapLabel(%q,%q)=%q but reversed=%q", tc.a, tc.b, got, back)
		}
		if got != tc.want {
			t.Errorf("overlapLabel(%q,%q)=%q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestLikePrefixPatternEscapes guards the half of the candidate query that is
// a string rather than a bind parameter.
//
// "_" is the one that bites: it is a LIKE wildcard and a perfectly ordinary
// character in a directory name, so an unescaped prefix would quietly pull in
// neighbouring directories and report collisions in files nobody claimed.
func TestLikePrefixPatternEscapes(t *testing.T) {
	cases := map[string]string{
		"src/api/":      "src/api/%",
		"src/my_pkg/":   "src/my!_pkg/%",
		"src/100%/":     "src/100!%/%",
		"src/oh!/":      "src/oh!!/%",
		"":              "%",
		"a_b/c%d/e!f/g": "a!_b/c!%d/e!!f/g%",
	}
	for in, want := range cases {
		if got := likePrefixPattern(in); got != want {
			t.Errorf("likePrefixPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{4 * time.Minute, "4m"},
		{59 * time.Minute, "59m"},
		{90 * time.Minute, "1h30m"},
		{-time.Second, ""},
	}
	for _, tc := range cases {
		if got := humanAge(tc.in); got != tc.want {
			t.Errorf("humanAge(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEnumTranslationRoundTrips guards the one place the pure string world of
// app/coordination meets the generated int enums. A half-added mode here
// would silently downgrade a structural claim to a write.
func TestEnumTranslationRoundTrips(t *testing.T) {
	for _, m := range []coordination.ClaimMode{coordination.ModeRead, coordination.ModeWrite, coordination.ModeStructural} {
		if back := claimModeToCoordination(claimModeEnum(m)); back != m {
			t.Errorf("claim mode %q round-tripped to %q", m, back)
		}
	}
	for _, k := range []coordination.PathKind{coordination.PathKindExact, coordination.PathKindPrefix, coordination.PathKindGlob} {
		if back := pathKindToCoordination(pathKindEnum(k)); back != k {
			t.Errorf("path kind %q round-tripped to %q", k, back)
		}
	}
	// An unknown mode must fail towards write, never towards read: read x
	// read is the one pairing that is never a conflict, so a wrong guess in
	// that direction loses a real collision silently.
	if got := claimModeToCoordination(enums.ClaimMode(99)); got != coordination.ModeWrite {
		t.Errorf("an unknown claim mode became %q, want write", got)
	}

	sevs := map[coordination.Severity]enums.ConflictSeverity{
		coordination.SeverityLow:      enums.CONFLICT_SEVERITY_LOW,
		coordination.SeverityMedium:   enums.CONFLICT_SEVERITY_MEDIUM,
		coordination.SeverityHigh:     enums.CONFLICT_SEVERITY_HIGH,
		coordination.SeverityCritical: enums.CONFLICT_SEVERITY_CRITICAL,
	}
	for in, want := range sevs {
		if got := severityEnum(in); got != want {
			t.Errorf("severityEnum(%s) = %d, want %d", in, got, want)
		}
	}
}

// TestNormalizedPathFromRow proves a candidate read back out of claim_path is
// the same value the rules would have computed from the pattern, which is
// what lets a stored row and a fresh declaration go through one overlap test.
func TestNormalizedPathFromRow(t *testing.T) {
	for _, pattern := range []string{"internal/auth/token.go", "internal/auth/**", "src/api/*_test.go", "**"} {
		fresh := mustPath(t, pattern)
		rebuilt := normalizedPathFromRow(fresh.Pattern, fresh.PatternNorm, pathKindEnum(fresh.Kind),
			fresh.Prefix, fresh.SuffixPattern, int64(fresh.Depth), fresh.Ext)
		if rebuilt != fresh {
			t.Errorf("round trip through claim_path changed the path:\n stored: %+v\n fresh:  %+v", rebuilt, fresh)
		}
	}
}

// TestPathDetectionContextSeam checks the seam the tools use to hand the
// detector its work, including that an unrelated call carries nothing — which
// is what keeps heartbeat free of detection entirely.
func TestPathDetectionContextSeam(t *testing.T) {
	ctx := t.Context()
	if got := pathDetectionFrom(ctx); got != nil {
		t.Fatalf("a bare context carried a detection request: %+v", got)
	}
	req := &pathDetectionRequest{SessionKey: "S-1"}
	ctx = withPathDetection(ctx, req)
	got := pathDetectionFrom(ctx)
	if got == nil || got.SessionKey != "S-1" {
		t.Fatalf("the detection request did not survive the context: %+v", got)
	}
	// The pointer identity is load-bearing: Apply fills Declared in after the
	// context was built, and the hook must see it.
	got.Declared = append(got.Declared, pathDeclaration{Path: mustPath(t, "a/b.go")})
	if len(req.Declared) != 1 {
		t.Error("the hook and the tool are not looking at the same request")
	}
}
