package mcp

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/coordination"
	project_entity "github.com/mklfarha/metiche/backend/entity/project"
)

// TestRegisterWorkToolsAnnotatesEverything is a small test guarding a big
// failure mode.
//
// MCP's destructiveHint DEFAULTS TO TRUE when a tool omits its annotations.
// An unannotated declare_intent advertises itself as destructive, a
// well-behaved client puts a confirmation in front of it, and the tool an
// agent is supposed to call on every loop silently never gets called. The
// three tools here are the whole product, so this is asserted rather than
// reviewed.
func TestRegisterWorkToolsAnnotatesEverything(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "metiche-test", Version: ProtocolVersion}, nil)
	registered = nil
	RegisterWorkTools(server, nil, zap.NewNop())

	want := map[string]struct {
		readOnly   bool
		idempotent bool
	}{
		"declare_intent": {readOnly: false, idempotent: false}, // additive: two calls are two intents
		"update_intent":  {readOnly: false, idempotent: true},
		"check_paths":    {readOnly: true, idempotent: false},
	}
	if len(registered) != len(want) {
		t.Fatalf("registered %d tools, want %d: %v", len(registered), len(want), toolNames(registered))
	}
	for _, tool := range registered {
		spec, ok := want[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations, so destructiveHint defaults to TRUE", tool.Name)
			continue
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Errorf("%s does not declare destructiveHint=false; nothing in metiche destroys anything", tool.Name)
		}
		if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s does not declare openWorldHint=false; metiche talks to its own database and nothing else", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint != spec.readOnly {
			t.Errorf("%s readOnly = %v, want %v", tool.Name, tool.Annotations.ReadOnlyHint, spec.readOnly)
		}
		if tool.Annotations.IdempotentHint != spec.idempotent {
			t.Errorf("%s idempotent = %v, want %v", tool.Name, tool.Annotations.IdempotentHint, spec.idempotent)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("%s has no description; the description is the only thing a model reads before choosing it", tool.Name)
		}
	}
	registered = nil
}

func toolNames(tools []*mcp.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// TestNormalizeClaimPathsDropsAndRejects covers the asymmetry that matters
// most in path handling: an IGNORED path is dropped quietly, a MALFORMED one
// fails the call.
//
// Dropping generated code silently is the single most important noise rule
// in this repository — nuzur writes most of the Go, and without it every
// agent collides with every other agent on files nobody hand-edits. Dropping
// a malformed path silently is the opposite: the agent walks away believing
// it holds something it does not.
func TestNormalizeClaimPathsDropsAndRejects(t *testing.T) {
	proj := project_entity.Project{
		IgnorePatterns:       coordination.DefaultIgnorePatterns(),
		HotspotPatterns:      coordination.DefaultHotspotPatterns(),
		CaseInsensitivePaths: false,
	}

	paths, ignored, err := normalizeClaimPaths([]string{
		"internal/auth/token.go",
		"vendor/github.com/x/y.go", // ignored, not an error
		"core/entity/user.gen.go",  // generated, ignored
		"internal/auth",            // a directory: becomes internal/auth/**
		"internal/auth/",           // the same thing spelled differently
	}, proj)
	if err != nil {
		t.Fatalf("a list containing ignored paths failed the whole call: %v", err)
	}
	if len(ignored) != 2 {
		t.Errorf("ignored = %v, want the vendored and generated paths", ignored)
	}
	// Two spellings of one directory are one claim, or the caller collides
	// with itself on every re-detection.
	if len(paths) != 2 {
		t.Fatalf("kept %d paths, want 2 (the file and the de-duplicated directory): %+v", len(paths), patternList(paths))
	}
	if got := patternList(paths); got[1] != "internal/auth/**" {
		t.Errorf("normalized directory = %q, want internal/auth/**", got[1])
	}

	for _, bad := range []string{"/etc/passwd", "../other-repo/main.go"} {
		if _, _, err := normalizeClaimPaths([]string{bad}, proj); err == nil {
			t.Errorf("%q was accepted; a path the caller cannot actually hold must fail loudly", bad)
		}
	}

	// The cap is a lock-hold budget, not a style rule: everything a
	// declaration costs is linear in the path count.
	many := make([]string, MaxDeclaredPaths+1)
	for i := range many {
		many[i] = "src/pkg/file.go"
	}
	if _, _, err := normalizeClaimPaths(many, proj); err == nil {
		t.Errorf("a list of %d paths was accepted, want it capped at %d", len(many), MaxDeclaredPaths)
	}
}

func TestParseClaimMode(t *testing.T) {
	cases := map[string]coordination.ClaimMode{
		"":           coordination.ModeWrite,
		"write":      coordination.ModeWrite,
		"READ":       coordination.ModeRead,
		"structural": coordination.ModeStructural,
		"rename":     coordination.ModeStructural,
	}
	for in, want := range cases {
		got, err := parseClaimMode(in)
		if err != nil {
			t.Errorf("parseClaimMode(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseClaimMode(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := parseClaimMode("delete-everything"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

// TestParseIntentStatus checks the distinction an omitted field has to keep:
// "not mentioned" and "set it back to declared" are different instructions,
// and collapsing them would silently reopen finished work.
func TestParseIntentStatus(t *testing.T) {
	if _, has, err := parseIntentStatus("  "); err != nil || has {
		t.Errorf("an empty status reported has=%v err=%v, want has=false", has, err)
	}
	for _, in := range []string{"done", "complete", "finished"} {
		got, has, err := parseIntentStatus(in)
		if err != nil || !has || got.String() != "done" {
			t.Errorf("parseIntentStatus(%q) = %v/%v/%v, want done", in, got, has, err)
		}
	}
	if _, _, err := parseIntentStatus("nearly"); err == nil {
		t.Error("an unknown status was accepted")
	}
}

func TestClampClaimTTL(t *testing.T) {
	cases := map[int]int{
		0:      DefaultClaimTTLSeconds,
		-5:     DefaultClaimTTLSeconds,
		10:     ClaimTTLMinSeconds,
		900:    900,
		999999: ClaimTTLMaxSeconds,
	}
	for in, want := range cases {
		if got := clampClaimTTL(in); got != want {
			t.Errorf("clampClaimTTL(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestCheckNoteAlwaysSaysWhatToDoNext guards the one line of prose a model
// reads before deciding whether to look at the rest of the response.
func TestCheckNoteAlwaysSaysWhatToDoNext(t *testing.T) {
	clear := checkNote(3, 0, coordination.ModeWrite)
	if !strings.Contains(clear, "declare_intent") {
		t.Errorf("a clear result does not name the next tool to call: %q", clear)
	}
	busy := checkNote(3, 2, coordination.ModeWrite)
	if !strings.Contains(busy, "holders") {
		t.Errorf("a busy result does not point at the holders list: %q", busy)
	}
	if strings.Contains(busy, "1 is") {
		t.Errorf("plurality is wrong for two holders: %q", busy)
	}
}
