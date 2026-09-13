package mcp

import (
	"strings"
	"testing"

	"github.com/mklfarha/metiche/backend/enums"
)

// These reproduce a production failure: declare_intent refused a claim with
//
//	recording the claimed paths failed: data validation failed: claim_path.prefix: field is required
//
// Any pattern whose first wildcard sits in the first segment — "*.go",
// "**/*.go", "**", "." — has an empty literal prefix, and the generated
// claim_path validation treats "" as missing. The claim was refused, so those
// paths never entered collision detection at all.
//
// Same real-MySQL harness as intents_test.go; skipped without
// METICHE_TEST_MYSQL_DSN.

// TestIntegrationRootLevelPatternsAreRecorded sends the inputs real agents
// send, one declaration each, through the real handler.
func TestIntegrationRootLevelPatternsAreRecorded(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	anaKey := startWork(t, hs, ana, "feat/root", "claim at the repo root")
	anaSession := sessionUUIDFor(t, hs, anaKey)

	cases := []struct {
		in       string
		wantNorm string
	}{
		{"*.go", "*.go"},
		{"**/*.go", "**/*.go"},
		{"**", "**"},
		{".", "**"},
		{"go.mod", "go.mod"},
		{"app/*.go", "app/*.go"},
		{"./app/rest.go", "app/rest.go"},
		{"app/", "app/**"},
		{`app\rest.go`, "app/rest.go"},
	}
	var failed []string
	for _, tc := range cases {
		res, _, err := hs.h.DeclareIntent(ana.ctx, nil, DeclareIntentParams{
			SessionKey: anaKey,
			Summary:    "work on " + tc.in,
			Paths:      []string{tc.in},
			Mode:       "write",
		})
		if err != nil {
			failed = append(failed, tc.in)
			t.Errorf("declare_intent(%q) was refused: %v", tc.in, err)
			continue
		}
		t.Logf("declare_intent(%q) -> %s", tc.in, resultText(t, res))
		if n := countRows(t, hs.core.DB(),
			"SELECT COUNT(*) FROM `claim_path` WHERE `session_uuid` = ? AND `pattern_norm` = ? AND `status` = ?",
			anaSession, tc.wantNorm, enums.CLAIM_STATUS_HELD); n < 1 {
			t.Errorf("declare_intent(%q) recorded no held claim_path with pattern_norm %q", tc.in, tc.wantNorm)
		}
	}
	if len(failed) > 0 {
		t.Logf("refused inputs: %s", strings.Join(failed, "  "))
	}
}

// TestIntegrationBadPatternsAreRefusedClearly is the other half: an input that
// cannot be a claim is refused with a reason that names the problem, never the
// generic validation failure an agent cannot act on.
func TestIntegrationBadPatternsAreRefusedClearly(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	anaKey := startWork(t, hs, ana, "feat/root", "claim badly")

	cases := []struct {
		in   string
		want string
	}{
		{"", "path is empty"},
		{"   ", "path is empty"},
		{"../other-repo/x.go", `".." segment`},
		{`app\..\..\etc`, `".." segment`},
		// Absolute paths stay refused by design (PLAN.md: reject absolute):
		// stripping the slash would turn a machine-absolute path like
		// /Users/x/repo/app/rest.go into a claim on a path that does not exist.
		{"/app/rest.go", "absolute"},
	}
	for _, tc := range cases {
		_, _, err := hs.h.DeclareIntent(ana.ctx, nil, DeclareIntentParams{
			SessionKey: anaKey,
			Summary:    "work on " + tc.in,
			Paths:      []string{tc.in},
			Mode:       "write",
		})
		switch {
		case err == nil:
			t.Errorf("declare_intent(%q) was accepted, want a refusal", tc.in)
		case strings.Contains(err.Error(), "data validation failed"):
			t.Errorf("declare_intent(%q) failed generic validation instead of saying why: %v", tc.in, err)
		case !strings.Contains(err.Error(), tc.want):
			t.Errorf("declare_intent(%q) error = %v, want it to mention %q", tc.in, err, tc.want)
		default:
			t.Logf("declare_intent(%q) refused: %v", tc.in, err)
		}
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `claim_path`"); n != 0 {
		t.Errorf("refused declarations left %d claim_path row(s), want 0", n)
	}
}

// conflictsBetween counts conflicts that have both sessions as participants.
func conflictsBetween(t *testing.T, hs *harness, sessionA, sessionB string) int {
	t.Helper()
	return countRows(t, hs.core.DB(),
		"SELECT COUNT(DISTINCT a.`conflict_uuid`) FROM `conflict_participant` a "+
			"JOIN `conflict_participant` b ON b.`conflict_uuid` = a.`conflict_uuid` "+
			"WHERE a.`session_uuid` = ? AND b.`session_uuid` = ?", sessionA, sessionB)
}

func conflictBetween(t *testing.T, hs *harness, sessionA, sessionB string) (severity int, rule string) {
	t.Helper()
	if err := hs.core.DB().QueryRow(
		"SELECT c.`severity`, COALESCE(c.`detector_rule`, '') FROM `conflict` c "+
			"JOIN `conflict_participant` a ON a.`conflict_uuid` = c.`id` AND a.`session_uuid` = ? "+
			"JOIN `conflict_participant` b ON b.`conflict_uuid` = c.`id` AND b.`session_uuid` = ?",
		sessionA, sessionB).Scan(&severity, &rule); err != nil {
		t.Fatalf("reading the conflict between %s and %s: %v", sessionA, sessionB, err)
	}
	return severity, rule
}

// TestIntegrationRootGlobsCollide proves a root-level claim, once recorded,
// is found by the candidate scan from both directions and scored by the
// documented rules:
//
//   - "*.go" is root files only. doublestar's "*" does not cross "/", so it
//     does NOT touch app/rest.go.
//   - "**/*.go" covers every .go file at any depth, so it DOES touch
//     app/rest.go. write x write is high, but a depth-0 claim is over-broad
//     and PLAN.md caps that at low ("capped at low if either claim is
//     over-broad (depth <= 1)"), so it is recorded at low with rule
//     path_overlap.broad and does not interrupt (notify floor is medium).
//   - Two root globs overlap with each other (glob x glob, one prefix covers
//     the other), also capped at low.
func TestIntegrationRootGlobsCollide(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	cid := hs.join(t, "Cid", "client-cid")
	bob := hs.join(t, "Bob", "client-bob")
	anaKey := startWork(t, hs, ana, "feat/root-files", "tidy the root files")
	cidKey := startWork(t, hs, cid, "feat/all-go", "rename a type in every Go file")
	bobKey := startWork(t, hs, bob, "feat/rest", "add a route")
	anaSession := sessionUUIDFor(t, hs, anaKey)
	cidSession := sessionUUIDFor(t, hs, cidKey)
	bobSession := sessionUUIDFor(t, hs, bobKey)

	declare(t, hs, ana, DeclareIntentParams{
		SessionKey: anaKey, Summary: "tidy the root Go files",
		Paths: []string{"*.go"}, Mode: "write",
	})

	// Two root globs: Cid's scan has to find Ana's root row.
	declare(t, hs, cid, DeclareIntentParams{
		SessionKey: cidKey, Summary: "rename a type everywhere",
		Paths: []string{"**/*.go"}, Mode: "write",
	})
	if n := conflictsBetween(t, hs, cidSession, anaSession); n != 1 {
		t.Fatalf("**/*.go against *.go produced %d conflict(s), want 1", n)
	}
	sev, rule := conflictBetween(t, hs, cidSession, anaSession)
	if enums.ConflictSeverity(sev) != enums.CONFLICT_SEVERITY_LOW || rule != "path_overlap.broad" {
		t.Errorf("root glob x root glob = severity %d rule %q, want low (%d) path_overlap.broad",
			sev, rule, enums.CONFLICT_SEVERITY_LOW)
	}

	// Bob's deep file: his scan (prefix app/) has to reach the root rows.
	second := declare(t, hs, bob, DeclareIntentParams{
		SessionKey: bobKey, Summary: "add the health route",
		Paths: []string{"app/rest.go"}, Mode: "write",
	})
	if n := conflictsBetween(t, hs, bobSession, cidSession); n != 1 {
		t.Fatalf("app/rest.go against a held **/*.go produced %d conflict(s), want 1", n)
	}
	sev, rule = conflictBetween(t, hs, bobSession, cidSession)
	if enums.ConflictSeverity(sev) != enums.CONFLICT_SEVERITY_LOW || rule != "path_overlap.broad" {
		t.Errorf("**/*.go x app/rest.go = severity %d rule %q, want low (%d) path_overlap.broad",
			sev, rule, enums.CONFLICT_SEVERITY_LOW)
	}
	if n := conflictsBetween(t, hs, bobSession, anaSession); n != 0 {
		t.Errorf("app/rest.go collided with *.go %d time(s); *.go is root files only, want 0", n)
	}
	// Low is below the notify floor: recorded and on the board, not an interrupt.
	if len(second.Conflicts) != 0 {
		t.Errorf("a low (broad) conflict interrupted the declarer: %+v", second.Conflicts)
	}
}

// TestIntegrationRootClaimReplaysVerbatim retries a root-level declaration
// that DOES interrupt: "**" against go.mod is capped low as over-broad, then
// +1 for the go.mod hotspot, which is medium and at the notify floor.
func TestIntegrationRootClaimReplaysVerbatim(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	ana := hs.join(t, "Ana", "client-ana")
	bob := hs.join(t, "Bob", "client-bob")
	anaKey := startWork(t, hs, ana, "feat/deps", "bump a dependency")
	bobKey := startWork(t, hs, bob, "feat/sweep", "repo-wide sweep")

	declare(t, hs, ana, DeclareIntentParams{
		SessionKey: anaKey, Summary: "bump the mysql driver",
		Paths: []string{"go.mod"}, Mode: "write",
	})

	args := DeclareIntentParams{
		SessionKey:     bobKey,
		Summary:        "reformat the whole repo",
		Paths:          []string{"**"},
		Mode:           "write",
		IdempotencyKey: "retry-root",
	}
	first, _, err := hs.h.DeclareIntent(bob.ctx, nil, args)
	if err != nil {
		t.Fatalf("first declare_intent(**): %v", err)
	}
	second, _, err := hs.h.DeclareIntent(bob.ctx, nil, args)
	if err != nil {
		t.Fatalf("retried declare_intent(**): %v", err)
	}
	a, b := resultText(t, first), resultText(t, second)
	if a != b {
		t.Errorf("a retry answered differently:\nfirst:  %s\nsecond: %s", a, b)
	}
	var env Envelope
	decodeResult(t, first, &env)
	if len(env.Conflicts) != 1 || env.Conflicts[0].Severity != "medium" {
		t.Errorf("** against go.mod = %+v, want exactly one medium notice (broad cap, then hotspot +1)", env.Conflicts)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict`"); n != 1 {
		t.Errorf("the retry produced %d conflicts, want 1", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `claim_path` WHERE `session_uuid` = ?",
		sessionUUIDFor(t, hs, bobKey)); n != 1 {
		t.Errorf("the retry recorded %d claim_path rows for **, want 1", n)
	}
	t.Logf("both calls returned: %s", a)
}
