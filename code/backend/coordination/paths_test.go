package coordination

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// pathMustNormalize normalizes with no ignore list and case sensitivity on,
// which is the configuration every overlap and severity case below assumes.
func pathMustNormalize(t *testing.T, pattern string) NormalizedPath {
	t.Helper()
	n, err := NormalizePath(pattern, nil, false)
	if err != nil {
		t.Fatalf("NormalizePath(%q) unexpected error: %v", pattern, err)
	}
	return n
}

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		name            string
		in              string
		caseInsensitive bool

		wantNorm    string
		wantKind    PathKind
		wantPrefix  string
		wantSuffix  string
		wantDepth   int
		wantExt     string
		wantBreadth int
	}{
		{
			name: "exact file", in: "src/api/user.go",
			wantNorm: "src/api/user.go", wantKind: PathKindExact,
			wantPrefix: "src/api/user.go", wantSuffix: "", wantDepth: 3,
			wantExt: ".go", wantBreadth: 25,
		},
		{
			name: "glob in last segment", in: "src/api/*_test.go",
			wantNorm: "src/api/*_test.go", wantKind: PathKindGlob,
			wantPrefix: "src/api/", wantSuffix: "*_test.go", wantDepth: 2,
			wantExt: "", wantBreadth: 33,
		},
		{
			name: "subtree", in: "src/**",
			wantNorm: "src/**", wantKind: PathKindPrefix,
			wantPrefix: "src/", wantSuffix: "**", wantDepth: 1,
			wantExt: "", wantBreadth: 100,
		},
		{
			name: "bare doublestar", in: "**",
			wantNorm: "**", wantKind: PathKindPrefix,
			wantPrefix: "", wantSuffix: "**", wantDepth: 0,
			wantExt: "", wantBreadth: 100,
		},
		{
			name: "trailing slash becomes subtree", in: "app/handlers/",
			wantNorm: "app/handlers/**", wantKind: PathKindPrefix,
			wantPrefix: "app/handlers/", wantSuffix: "**", wantDepth: 2,
			wantExt: "", wantBreadth: 33,
		},
		{
			name: "wildcard-free dotless last segment is a directory", in: "app/handlers",
			wantNorm: "app/handlers/**", wantKind: PathKindPrefix,
			wantPrefix: "app/handlers/", wantSuffix: "**", wantDepth: 2,
			wantExt: "", wantBreadth: 33,
		},
		{
			// The known cost of the directory rule: an extensionless FILE is
			// read as a directory. Over-claiming is the cheaper mistake.
			name: "extensionless file is read as a directory", in: "docs/README",
			wantNorm: "docs/README/**", wantKind: PathKindPrefix,
			wantPrefix: "docs/README/", wantSuffix: "**", wantDepth: 2,
			wantExt: "", wantBreadth: 33,
		},
		{
			name: "backslashes", in: `src\api\user.go`,
			wantNorm: "src/api/user.go", wantKind: PathKindExact,
			wantPrefix: "src/api/user.go", wantDepth: 3,
			wantExt: ".go", wantBreadth: 25,
		},
		{
			name: "dot slash prefix and repeated separators", in: "./src//api///user.go",
			wantNorm: "src/api/user.go", wantKind: PathKindExact,
			wantPrefix: "src/api/user.go", wantDepth: 3,
			wantExt: ".go", wantBreadth: 25,
		},
		{
			name: "surrounding whitespace", in: "  src/api/user.go\t",
			wantNorm: "src/api/user.go", wantKind: PathKindExact,
			wantPrefix: "src/api/user.go", wantDepth: 3,
			wantExt: ".go", wantBreadth: 25,
		},
		{
			name: "bare dot is the whole repo", in: ".",
			wantNorm: "**", wantKind: PathKindPrefix,
			wantPrefix: "", wantSuffix: "**", wantDepth: 0,
			wantExt: "", wantBreadth: 100,
		},
		{
			name: "dot slash is the whole repo", in: "./",
			wantNorm: "**", wantKind: PathKindPrefix,
			wantPrefix: "", wantSuffix: "**", wantDepth: 0,
			wantExt: "", wantBreadth: 100,
		},
		{
			name: "case insensitive lowercases the norm only", in: "Src/API/User.go",
			caseInsensitive: true,
			wantNorm:        "src/api/user.go", wantKind: PathKindExact,
			wantPrefix: "src/api/user.go", wantDepth: 3,
			wantExt: ".go", wantBreadth: 25,
		},
		{
			name: "case sensitive keeps the norm as written", in: "Src/API/User.go",
			wantNorm: "Src/API/User.go", wantKind: PathKindExact,
			wantPrefix: "Src/API/User.go", wantDepth: 3,
			wantExt: ".go", wantBreadth: 25,
		},
		{
			name: "dotfile name is not an extension", in: ".env",
			wantNorm: ".env", wantKind: PathKindExact,
			wantPrefix: ".env", wantDepth: 1,
			// Breadth follows IsBroad, and an exact path is never broad, so a
			// root file scores by the formula instead of being forced to the
			// broad score.
			wantExt: "", wantBreadth: 50,
		},
		{
			name: "mid-pattern doublestar with literal tail", in: "src/**/user.go",
			wantNorm: "src/**/user.go", wantKind: PathKindGlob,
			wantPrefix: "src/", wantSuffix: "**/user.go", wantDepth: 1,
			wantExt: ".go", wantBreadth: 100,
		},
		{
			name: "brace is a wildcard for prefix purposes", in: "src/{api,web}/**",
			wantNorm: "src/{api,web}/**", wantKind: PathKindGlob,
			wantPrefix: "src/", wantSuffix: "{api,web}/**", wantDepth: 1,
			wantExt: "", wantBreadth: 100,
		},
		{
			name: "question mark in last segment clears ext", in: "src/api/user?.go",
			wantNorm: "src/api/user?.go", wantKind: PathKindGlob,
			wantPrefix: "src/api/", wantSuffix: "user?.go", wantDepth: 2,
			wantExt: "", wantBreadth: 33,
		},
		{
			name: "deep exact path", in: "a/b/c/d/e.go",
			wantNorm: "a/b/c/d/e.go", wantKind: PathKindExact,
			wantPrefix: "a/b/c/d/e.go", wantDepth: 5,
			wantExt: ".go", wantBreadth: 16,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePath(tc.in, nil, tc.caseInsensitive)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Pattern != strings.TrimSpace(tc.in) && got.Pattern != tc.in {
				t.Errorf("Pattern = %q, want the original input %q", got.Pattern, tc.in)
			}
			if got.PatternNorm != tc.wantNorm {
				t.Errorf("PatternNorm = %q, want %q", got.PatternNorm, tc.wantNorm)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Prefix != tc.wantPrefix {
				t.Errorf("Prefix = %q, want %q", got.Prefix, tc.wantPrefix)
			}
			if got.SuffixPattern != tc.wantSuffix {
				t.Errorf("SuffixPattern = %q, want %q", got.SuffixPattern, tc.wantSuffix)
			}
			if got.Depth != tc.wantDepth {
				t.Errorf("Depth = %d, want %d", got.Depth, tc.wantDepth)
			}
			if got.Ext != tc.wantExt {
				t.Errorf("Ext = %q, want %q", got.Ext, tc.wantExt)
			}
			if got.BreadthScore != tc.wantBreadth {
				t.Errorf("BreadthScore = %d, want %d", got.BreadthScore, tc.wantBreadth)
			}
		})
	}
}

// TestNormalizePathKeepsOriginalCase pins the one field that must survive
// normalization verbatim: the board shows the agent what it typed.
func TestNormalizePathKeepsOriginalCase(t *testing.T) {
	got, err := NormalizePath("Src/API/User.go", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Pattern != "Src/API/User.go" {
		t.Errorf("Pattern = %q, want the original casing", got.Pattern)
	}
	if got.PatternNorm != "src/api/user.go" {
		t.Errorf("PatternNorm = %q, want lowercased", got.PatternNorm)
	}
}

func TestNormalizePathRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrPathEmpty},
		{"whitespace only", "   \t ", ErrPathEmpty},
		{"absolute", "/etc/passwd", ErrPathAbsolute},
		{"absolute after separator rewrite", `\etc\passwd`, ErrPathAbsolute},
		{"absolute with repeated separators", "//etc/passwd", ErrPathAbsolute},
		{"traversal at head", "../../etc/passwd", ErrPathTraversal},
		{"traversal in the middle", "src/../../etc/passwd", ErrPathTraversal},
		{"traversal after separator rewrite", `..\..\etc`, ErrPathTraversal},
		{"traversal behind a dot slash", ".././x", ErrPathTraversal},
		{"null byte", "src/api/us\x00er.go", ErrPathNullByte},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizePath(tc.in, DefaultIgnorePatterns(), false)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NormalizePath(%q) error = %v, want %v", tc.in, err, tc.want)
			}
			var pe *PathError
			if !errors.As(err, &pe) {
				t.Fatalf("error %v is not a *PathError; the agent needs the pattern back", err)
			}
			if pe.Pattern != tc.in {
				t.Errorf("PathError.Pattern = %q, want %q", pe.Pattern, tc.in)
			}
		})
	}
}

func TestNormalizePathIgnored(t *testing.T) {
	ignore := DefaultIgnorePatterns()

	dropped := []string{
		"vendor/util.go",
		"vendor",
		"vendor/",
		"node_modules/react/index.js",
		"dist/bundle.js",
		"build",
		"yarn.lock",
		"app/user.gen.go",
		"user.gen.go",
		"core/module/team_gen.go",
		"api/types.generated.ts",
		".git/config",
	}
	for _, p := range dropped {
		t.Run("drop "+p, func(t *testing.T) {
			_, err := NormalizePath(p, ignore, false)
			if !errors.Is(err, ErrPathIgnored) {
				t.Fatalf("NormalizePath(%q) error = %v, want ErrPathIgnored", p, err)
			}
			var pe *PathError
			if errors.As(err, &pe) && pe.Detail == "" {
				t.Errorf("ErrPathIgnored for %q carries no Detail naming the entry that matched", p)
			}
		})
	}

	kept := []string{
		"src/api/user.go",
		"vendors/util.go",
		"my_vendor/util.go",
		"app/generated.go",
		"src/**",
	}
	for _, p := range kept {
		t.Run("keep "+p, func(t *testing.T) {
			if _, err := NormalizePath(p, ignore, false); err != nil {
				t.Fatalf("NormalizePath(%q) = %v, want it kept", p, err)
			}
		})
	}
}

func TestPathDefaultPatterns(t *testing.T) {
	// The generated-code entries are the reason the ignore list exists at all.
	for _, want := range []string{"vendor/**", "**/*.gen.go", "**/*_gen.go", "**/*.generated.*", ".git/**", "node_modules/**", "dist/**", "build/**", "*.lock"} {
		if !pathSliceHas(DefaultIgnorePatterns(), want) {
			t.Errorf("DefaultIgnorePatterns() is missing %q", want)
		}
	}
	for _, want := range []string{"go.mod", "go.sum", "package.json", "*.lock", "db/migrations/**", "**/schema.sql", ".env*"} {
		if !pathSliceHas(DefaultHotspotPatterns(), want) {
			t.Errorf("DefaultHotspotPatterns() is missing %q", want)
		}
	}

	// Each call must hand back its own slice, or one project mutating its
	// effective ignore list would edit every other project's.
	a := DefaultIgnorePatterns()
	a[0] = "mutated"
	if DefaultIgnorePatterns()[0] == "mutated" {
		t.Error("DefaultIgnorePatterns() returns shared backing state")
	}
}

func pathSliceHas(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestPathAncestors(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   []string
	}{
		{"file prefix", "app/handlers/auth.go", []string{"", "app/", "app/handlers/", "app/handlers/auth.go"}},
		{"directory prefix", "app/handlers/", []string{"", "app/", "app/handlers/"}},
		{"single segment", "src/", []string{"", "src/"}},
		{"repo root", "", []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PathAncestors(tc.prefix)
			if len(got) != len(tc.want) {
				t.Fatalf("PathAncestors(%q) = %q, want %q", tc.prefix, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("PathAncestors(%q) = %q, want %q", tc.prefix, got, tc.want)
				}
			}
		})
	}
}

func TestPathAncestorsIsBounded(t *testing.T) {
	deep := strings.Repeat("a/", 30)
	got := PathAncestors(deep)
	if len(got) != PathMaxAncestors {
		t.Fatalf("PathAncestors on a 30-deep prefix returned %d entries, want the %d cap", len(got), PathMaxAncestors)
	}
	// The shallow end survives truncation: those are the broad claims that
	// actually collide.
	if got[0] != "" {
		t.Errorf("got[0] = %q, want the repo-wide entry first", got[0])
	}
	if got[PathMaxAncestors-1] != strings.Repeat("a/", PathMaxAncestors-1) {
		t.Errorf("got[last] = %q, want the shallowest %d entries kept", got[PathMaxAncestors-1], PathMaxAncestors)
	}
}

func TestPathsOverlap(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical exact", "src/api/user.go", "src/api/user.go", true},
		{"different exact", "src/api/user.go", "src/api/team.go", false},
		{"subtree contains exact", "src/**", "src/api/user.go", true},
		{"subtree does not contain sibling exact", "web/**", "src/api/user.go", false},
		{"glob matches exact", "src/api/*_test.go", "src/api/user_test.go", true},
		{"glob does not match exact", "src/api/*_test.go", "src/api/user.go", false},
		{"sibling subtrees", "src/api/**", "src/web/**", false},
		{"nested subtrees", "src/**", "src/api/**", true},
		{"deeper nested subtrees", "src/api/**", "src/api/handlers/**", true},
		{"bare doublestar contains everything", "**", "src/api/user.go", true},
		{"bare doublestar contains every subtree", "**", "web/**", true},
		{"directory shorthand contains its file", "app/handlers", "app/handlers/auth.go", true},
		{"root files are distinct", "go.mod", "go.sum", false},
		{"same prefix different literal ext does not overlap", "src/**/user.go", "src/**/user.ts", false},
		{"same prefix same literal ext overlaps", "src/**/user.go", "src/api/**/user.go", true},
		{"empty ext on one side still overlaps", "src/**/user.go", "src/api/**", true},
		{
			// The deliberate over-report: neither last segment is literal, so
			// the extensions cannot rule it out and the prefixes match.
			name: "wildcarded extensions cannot be ruled out",
			a:    "src/**/*.go", b: "src/**/*.ts", want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := pathMustNormalize(t, tc.a)
			b := pathMustNormalize(t, tc.b)
			if got := PathsOverlap(a, b); got != tc.want {
				t.Errorf("PathsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Overlap is a symmetric relation and detection runs it in
			// whichever order the claims happen to come back from the query.
			if got := PathsOverlap(b, a); got != tc.want {
				t.Errorf("PathsOverlap(%q, %q) = %v, want %v (not symmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestPathsOverlapCaseInsensitive(t *testing.T) {
	a, err := NormalizePath("SRC/API/User.go", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizePath("src/api/user.go", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !PathsOverlap(a, b) {
		t.Error("case-insensitive project: SRC/API/User.go and src/api/user.go must overlap")
	}
}

// pathSide builds one side of a severity input.
func pathSide(t *testing.T, mode ClaimMode, pattern, member, branch string) PathClaimSide {
	t.Helper()
	return PathClaimSide{
		Mode:       mode,
		Path:       pathMustNormalize(t, pattern),
		MemberUUID: member,
		Branch:     branch,
	}
}

func TestPathConflictSeverityBaseMatrix(t *testing.T) {
	// Every path here is at least two segments deep so the broad-claim cap
	// stays out of the base matrix.
	cases := []struct {
		name string
		a, b PathClaimSide
		want Severity
	}{
		{
			name: "read x read is never a conflict",
			a:    pathSide(t, ModeRead, "src/api/user.go", "m1", "feat/a"),
			b:    pathSide(t, ModeRead, "src/api/user.go", "m2", "feat/b"),
			want: SeverityNone,
		},
		{
			name: "write x read",
			a:    pathSide(t, ModeWrite, "src/api/user.go", "m1", "feat/a"),
			b:    pathSide(t, ModeRead, "src/api/user.go", "m2", "feat/b"),
			want: SeverityMedium,
		},
		{
			name: "write x write on overlapping globs",
			a:    pathSide(t, ModeWrite, "src/api/**", "m1", "feat/a"),
			b:    pathSide(t, ModeWrite, "src/api/handlers/**", "m2", "feat/b"),
			want: SeverityHigh,
		},
		{
			name: "write x write on an identical exact path",
			a:    pathSide(t, ModeWrite, "src/api/user.go", "m1", "feat/a"),
			b:    pathSide(t, ModeWrite, "src/api/user.go", "m2", "feat/b"),
			want: SeverityCritical,
		},
		{
			name: "structural x read",
			a:    pathSide(t, ModeStructural, "src/api/user.go", "m1", "feat/a"),
			b:    pathSide(t, ModeRead, "src/api/user.go", "m2", "feat/b"),
			want: SeverityHigh,
		},
		{
			name: "structural x write",
			a:    pathSide(t, ModeStructural, "src/api/**", "m1", "feat/a"),
			b:    pathSide(t, ModeWrite, "src/api/user.go", "m2", "feat/b"),
			want: SeverityHigh,
		},
		{
			name: "structural x structural on the same path",
			a:    pathSide(t, ModeStructural, "src/api/user.go", "m1", "feat/a"),
			b:    pathSide(t, ModeStructural, "src/api/user.go", "m2", "feat/b"),
			want: SeverityCritical,
		},
		{
			name: "structural x structural on different paths",
			a:    pathSide(t, ModeStructural, "src/api/**", "m1", "feat/a"),
			b:    pathSide(t, ModeStructural, "src/api/handlers/**", "m2", "feat/b"),
			want: SeverityHigh,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := PathConflictSeverity(SeverityInput{A: tc.a, B: tc.b})
			if got != tc.want {
				t.Errorf("severity = %v, want %v", got, tc.want)
			}
			// Detection sees the pair in whichever order the query returns it.
			if swapped, _ := PathConflictSeverity(SeverityInput{A: tc.b, B: tc.a}); swapped != tc.want {
				t.Errorf("swapped severity = %v, want %v", swapped, tc.want)
			}
		})
	}
}

func TestPathConflictSeverityReadReadReturnsNoAdjusters(t *testing.T) {
	// A read x read pair must short-circuit: no hotspot escalation, no
	// adjuster names, nothing to record.
	sev, fired := PathConflictSeverity(SeverityInput{
		A:               pathSide(t, ModeRead, "db/migrations/0007_users.sql", "m1", "feat/a"),
		B:               pathSide(t, ModeRead, "db/migrations/0007_users.sql", "m2", "feat/b"),
		HotspotPatterns: DefaultHotspotPatterns(),
	})
	if sev != SeverityNone {
		t.Errorf("severity = %v, want none even on a hotspot", sev)
	}
	if len(fired) != 0 {
		t.Errorf("adjusters = %v, want none", fired)
	}
}

func TestPathConflictSeverityAdjusters(t *testing.T) {
	cases := []struct {
		name     string
		in       SeverityInput
		want     Severity
		wantFire []string
	}{
		{
			name: "same member drops one step",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "src/api/**", "m1", "feat/a"),
				B: pathSide(t, ModeWrite, "src/api/handlers/**", "m1", "feat/b"),
			},
			want:     SeverityMedium,
			wantFire: []string{PathAdjusterSameMember},
		},
		{
			name: "same branch drops one step",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "src/api/**", "m1", "feat/shared"),
				B: pathSide(t, ModeWrite, "src/api/handlers/**", "m2", "feat/shared"),
			},
			want:     SeverityMedium,
			wantFire: []string{PathAdjusterSameBranch},
		},
		{
			name: "an empty branch on both sides is not the same branch",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "src/api/**", "m1", ""),
				B: pathSide(t, ModeWrite, "src/api/handlers/**", "m2", ""),
			},
			want:     SeverityHigh,
			wantFire: nil,
		},
		{
			name: "an empty member uuid on both sides is not the same member",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "src/api/**", "", "feat/a"),
				B: pathSide(t, ModeWrite, "src/api/handlers/**", "", "feat/b"),
			},
			want:     SeverityHigh,
			wantFire: nil,
		},
		{
			name: "a broad claim on either side caps at low",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "src/**", "m1", "feat/a"),
				B: pathSide(t, ModeWrite, "src/api/handlers/**", "m2", "feat/b"),
			},
			want:     SeverityLow,
			wantFire: []string{PathAdjusterBroadClaim},
		},
		{
			name: "the repo-wide claim caps at low",
			in: SeverityInput{
				A: pathSide(t, ModeStructural, "**", "m1", "feat/a"),
				B: pathSide(t, ModeWrite, "src/api/handlers/user.go", "m2", "feat/b"),
			},
			want:     SeverityLow,
			wantFire: []string{PathAdjusterBroadClaim},
		},
		{
			name: "a hotspot escalates one step",
			in: SeverityInput{
				A:               pathSide(t, ModeWrite, "db/migrations/0007_users.sql", "m1", "feat/a"),
				B:               pathSide(t, ModeRead, "db/migrations/0007_users.sql", "m2", "feat/b"),
				HotspotPatterns: DefaultHotspotPatterns(),
			},
			want:     SeverityHigh,
			wantFire: []string{PathAdjusterHotspot},
		},
		{
			name: "a hotspot matching only one side still escalates",
			in: SeverityInput{
				A:               pathSide(t, ModeWrite, "db/migrations/**", "m1", "feat/a"),
				B:               pathSide(t, ModeRead, "db/migrations/0007_users.sql", "m2", "feat/b"),
				HotspotPatterns: DefaultHotspotPatterns(),
			},
			want:     SeverityHigh,
			wantFire: []string{PathAdjusterHotspot},
		},
		{
			name: "no hotspot list configured fires nothing",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "db/migrations/0007_users.sql", "m1", "feat/a"),
				B: pathSide(t, ModeRead, "db/migrations/0007_users.sql", "m2", "feat/b"),
			},
			want:     SeverityMedium,
			wantFire: nil,
		},
		{
			name: "same member and same branch stack",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "src/api/**", "m1", "feat/shared"),
				B: pathSide(t, ModeWrite, "src/api/handlers/**", "m1", "feat/shared"),
			},
			want:     SeverityLow,
			wantFire: []string{PathAdjusterSameMember, PathAdjusterSameBranch},
		},
		{
			name: "same member then the broad cap stack",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "src/**", "m1", "feat/a"),
				B: pathSide(t, ModeWrite, "src/api/**", "m1", "feat/b"),
			},
			want:     SeverityLow,
			wantFire: []string{PathAdjusterSameMember, PathAdjusterBroadClaim},
		},
		{
			name: "the broad cap applies before the hotspot bump, so a hotspot can climb back out",
			in: SeverityInput{
				A:               pathSide(t, ModeWrite, "**", "m1", "feat/a"),
				B:               pathSide(t, ModeWrite, "go.mod", "m2", "feat/b"),
				HotspotPatterns: DefaultHotspotPatterns(),
			},
			want:     SeverityMedium,
			wantFire: []string{PathAdjusterBroadClaim, PathAdjusterHotspot},
		},
		{
			name: "clamped at the bottom, never below low",
			in: SeverityInput{
				A: pathSide(t, ModeWrite, "src/api/user.go", "m1", "feat/shared"),
				B: pathSide(t, ModeRead, "src/api/user.go", "m1", "feat/shared"),
			},
			// Two -1 adjusters stack, but the floor is low: none would mean
			// "not a conflict", which only read x read is, and it is handled
			// by an early return before any adjuster runs.
			want:     SeverityLow,
			wantFire: []string{PathAdjusterSameMember, PathAdjusterSameBranch},
		},
		{
			name: "clamped at the top, never above critical",
			in: SeverityInput{
				A:               pathSide(t, ModeWrite, "db/migrations/0007_users.sql", "m1", "feat/a"),
				B:               pathSide(t, ModeWrite, "db/migrations/0007_users.sql", "m2", "feat/b"),
				HotspotPatterns: DefaultHotspotPatterns(),
			},
			want:     SeverityCritical,
			wantFire: []string{PathAdjusterHotspot},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fired := PathConflictSeverity(tc.in)
			if got != tc.want {
				t.Errorf("severity = %v, want %v (adjusters fired: %v)", got, tc.want, fired)
			}
			if strings.Join(fired, ",") != strings.Join(tc.wantFire, ",") {
				t.Errorf("adjusters = %v, want %v", fired, tc.wantFire)
			}
		})
	}
}

// TestPathConflictSeverityRootFileIsNotBroad pins the rule that breadth is
// about reach, not depth. A file at the repo root has prefix depth 1, which a
// depth-only test reads as over-broad - exactly backwards, because an exact
// path is the narrowest claim expressible. Two agents writing go.mod is the
// most actionable collision this system detects, so it must stay critical
// rather than being capped down to medium by an accident of arithmetic.
func TestPathConflictSeverityRootFileIsNotBroad(t *testing.T) {
	in := SeverityInput{
		A:               pathSide(t, ModeWrite, "go.mod", "m1", "feat/a"),
		B:               pathSide(t, ModeWrite, "go.mod", "m2", "feat/b"),
		HotspotPatterns: DefaultHotspotPatterns(),
	}
	got, fired := PathConflictSeverity(in)
	if got != SeverityCritical {
		t.Errorf("severity = %v, want critical (identical exact write x write, hotspot cannot climb past the top)", got)
	}
	if strings.Join(fired, ",") != PathAdjusterHotspot {
		t.Errorf("adjusters = %v, want only the hotspot bump - the broad cap must not fire on an exact path", fired)
	}
	// And the predicate itself, stated directly.
	if pathSide(t, ModeWrite, "go.mod", "m1", "").Path.IsBroad() {
		t.Error("go.mod reports as broad; an exact path is never broad")
	}
	if !pathSide(t, ModeWrite, "app/**", "m1", "").Path.IsBroad() {
		t.Error("app/** does not report as broad; a depth-1 subtree is")
	}
}

func TestSeverityStringIsOrdered(t *testing.T) {
	if !(SeverityNone < SeverityLow && SeverityLow < SeverityMedium &&
		SeverityMedium < SeverityHigh && SeverityHigh < SeverityCritical) {
		t.Fatal("Severity must be ordered; the adjusters do arithmetic on it")
	}
	want := []string{"none", "low", "medium", "high", "critical"}
	for i, s := range []Severity{SeverityNone, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical} {
		if s.String() != want[i] {
			t.Errorf("Severity(%d).String() = %q, want %q", i, s.String(), want[i])
		}
	}
}

func TestPathDedupeKeyIsSymmetric(t *testing.T) {
	pairs := [][2]string{
		{"aaaaaaaa-0000-0000-0000-000000000001", "ffffffff-0000-0000-0000-000000000002"},
		{"zz", "aa"},
		{"same", "same"},
		{"", "b"},
	}
	for _, p := range pairs {
		t.Run(p[0]+"/"+p[1], func(t *testing.T) {
			forward := PathDedupeKey(p[0], p[1], "src/api/user.go")
			reverse := PathDedupeKey(p[1], p[0], "src/api/user.go")
			if forward != reverse {
				t.Fatalf("key is order sensitive: %s != %s", forward, reverse)
			}
		})
	}
}

func TestPathDedupeKeyDistinguishesInputs(t *testing.T) {
	base := PathDedupeKey("a", "b", "src/api/user.go")
	if same := PathDedupeKey("a", "b", "src/api/team.go"); same == base {
		t.Error("a different overlap path must mint a different key")
	}
	if same := PathDedupeKey("a", "c", "src/api/user.go"); same == base {
		t.Error("a different claim must mint a different key")
	}
	// The "|" separator is not escaped, so a claim uuid containing one would
	// let a field bleed into the next. Claim uuids are uuids and cannot, and
	// the overlap path is last so nothing follows it to bleed into. Asserting
	// that a "|"-bearing overlap path still distinguishes keys is the part of
	// that reasoning worth pinning.
	if PathDedupeKey("a", "b", "src/x|y.go") == PathDedupeKey("a", "b", "src/x") {
		t.Error("the overlap path must be part of the key verbatim")
	}
}

func TestPathDedupeKeyIsStable(t *testing.T) {
	// sha256("path_overlap|aaa|bbb|src/api/user.go") - pinned so the key
	// survives refactoring. Changing it re-opens every conflict on the board.
	const want = "f1323c1d32a49d99cba6064cdb54c16f3b2abe0656a17c6d2936bee957f812c2"
	if got := PathDedupeKey("bbb", "aaa", "src/api/user.go"); got != want {
		t.Errorf("PathDedupeKey = %s, want %s", got, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(want) {
		t.Error("key must be lowercase hex")
	}
}
