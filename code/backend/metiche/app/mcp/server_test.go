package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// TestParseRole covers the decision that says what a pod serves.
//
// The fallback direction is the point: an unrecognised value must serve
// EVERYTHING. A misconfigured pod that serves too much is a performance
// problem; one that serves too little is an outage with no error message.
func TestParseRole(t *testing.T) {
	cases := []struct {
		in        string
		want      Role
		recognise bool
		mcp, api  bool
	}{
		{"", RoleAll, true, true, true},
		{"all", RoleAll, true, true, true},
		{"ALL", RoleAll, true, true, true},
		{"  all  ", RoleAll, true, true, true},
		{"mcp", RoleMCP, true, true, false},
		{"MCP", RoleMCP, true, true, false},
		{"api", RoleAPI, true, false, true},
		{"Api", RoleAPI, true, false, true},
		{"worker", RoleAll, false, true, true},
		{"mcp,api", RoleAll, false, true, true},
		{"none", RoleAll, false, true, true},
	}
	for _, tc := range cases {
		t.Run("role="+tc.in, func(t *testing.T) {
			got, ok := ParseRole(tc.in)
			if got != tc.want {
				t.Errorf("ParseRole(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if ok != tc.recognise {
				t.Errorf("ParseRole(%q) recognised = %v, want %v", tc.in, ok, tc.recognise)
			}
			if got.ServesMCP() != tc.mcp {
				t.Errorf("%q.ServesMCP() = %v, want %v", got, got.ServesMCP(), tc.mcp)
			}
			if got.ServesAPI() != tc.api {
				t.Errorf("%q.ServesAPI() = %v, want %v", got, got.ServesAPI(), tc.api)
			}
		})
	}
}

// TestRoleFromEnv checks the environment plumbing, including the default that
// v1 actually runs on.
func TestRoleFromEnv(t *testing.T) {
	t.Setenv("METICHE_ROLE", "")
	if got := RoleFromEnv(zap.NewNop()); got != RoleAll {
		t.Errorf("unset METICHE_ROLE = %q, want all", got)
	}
	t.Setenv("METICHE_ROLE", "mcp")
	if got := RoleFromEnv(zap.NewNop()); got != RoleMCP {
		t.Errorf("METICHE_ROLE=mcp = %q, want mcp", got)
	}
	t.Setenv("METICHE_ROLE", "nonsense")
	if got := RoleFromEnv(zap.NewNop()); got != RoleAll {
		t.Errorf("METICHE_ROLE=nonsense = %q, want all", got)
	}
}

// TestToolSurface builds the real server. Registration is where the SDK
// derives each tool's JSON schema from its params struct, so this catches a
// malformed jsonschema tag or an unsupported field type — which would
// otherwise surface as a panic on the first request in production.
//
// The handler's core is nil because no tool is invoked here; only registration
// is exercised.
func TestToolSurface(t *testing.T) {
	newServer(NewHandler(nil, zap.NewNop()), zap.NewNop())

	want := map[string]struct {
		readOnly   bool
		idempotent bool
	}{
		"create_team":    {false, false},
		"join_team":      {false, true},
		"start_session":  {false, true},
		"end_session":    {false, true},
		"heartbeat":      {false, true},
		"get_team_state": {true, false},
		"list_teams":     {true, false},
		"health":         {true, false},

		// Tools 5-7, registered by RegisterWorkTools. declare_intent is
		// additive and NOT idempotent: two calls are two real intents, and
		// what makes a retry safe is the idempotency key, which is a
		// different promise.
		"declare_intent": {false, false},
		"update_intent":  {false, true},
		"check_paths":    {true, false},

		// Tools 13-14, registered by RegisterInstructionTools.
		// get_instructions is NOT readOnly however much it looks like it:
		// reading an instruction is what marks it delivered, and a client
		// that believed otherwise would cache the call and stop delivering
		// anything. Not idempotent either — the second call deliberately
		// answers differently, because the first consumed what it returned.
		// report_back IS idempotent: its key is (instruction, outcome), so
		// the same report twice replays rather than appending a second event.
		"get_instructions": {false, false},
		"report_back":      {false, true},
	}

	if len(registered) != len(want) {
		names := make([]string, 0, len(registered))
		for _, tool := range registered {
			names = append(names, tool.Name)
		}
		t.Fatalf("registered %d tools (%s), want %d", len(registered), strings.Join(names, ", "), len(want))
	}

	seen := map[string]bool{}
	for _, tool := range registered {
		seen[tool.Name] = true
		spec, known := want[tool.Name]
		if !known {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		if tool.Description == "" {
			t.Errorf("%s has no description; an agent picks tools by description", tool.Name)
		}
		// MCP's destructiveHint DEFAULTS TO TRUE when annotations are omitted,
		// and clients gate calls on it. An unannotated tool would gate the
		// whole surface behind a confirmation prompt.
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations, so it advertises itself as destructive", tool.Name)
			continue
		}
		if d := tool.Annotations.DestructiveHint; d == nil || *d {
			t.Errorf("%s does not explicitly declare destructiveHint=false", tool.Name)
		}
		if o := tool.Annotations.OpenWorldHint; o == nil || *o {
			t.Errorf("%s does not declare openWorldHint=false; metiche talks to nothing but its own database", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint != spec.readOnly {
			t.Errorf("%s readOnlyHint = %v, want %v", tool.Name, tool.Annotations.ReadOnlyHint, spec.readOnly)
		}
		if tool.Annotations.IdempotentHint != spec.idempotent {
			t.Errorf("%s idempotentHint = %v, want %v", tool.Name, tool.Annotations.IdempotentHint, spec.idempotent)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("tool %q was not registered", name)
		}
	}
}

// TestAddToolRefusesUnannotatedTools proves the guard in addTool is real: it
// is the only thing that keeps "forgot the annotations" from shipping, and
// what it prevents is a client gating the entire surface because MCP treats a
// missing destructiveHint as true.
func TestAddToolRefusesUnannotatedTools(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("addTool accepted a tool with no annotations")
		}
		if !strings.Contains(fmt.Sprint(r), "destructiveHint") {
			t.Errorf("the panic should explain why: %v", r)
		}
	}()
	s := newServer(NewHandler(nil, zap.NewNop()), zap.NewNop())
	addTool(s, nil, zap.NewNop(), &mcp.Tool{Name: "unannotated", Description: "x"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, nil
		})
}

// TestServerInstructionsSayTheLoop: the instructions are the only thing a
// model reads before it picks its first tool. They have to name the loop.
func TestServerInstructionsSayTheLoop(t *testing.T) {
	got := serverInstructions()
	for _, must := range []string{"join_team", "start_session", "heartbeat", "end_session", "pending", "sequence", "idempotency_key"} {
		if !strings.Contains(got, must) {
			t.Errorf("server instructions never mention %q", must)
		}
	}
}

// ─────────────────────────────────────────────
// Lock-hold histogram
// ─────────────────────────────────────────────

// TestHistogram checks the metric that PLAN.md calls the thing protecting the
// write-path headroom. A metric nobody verified is a metric that reports zero
// forever.
//
// The expected bounds are derived from lockHoldBucketsMS rather than written
// out, so re-cutting the buckets is not a test edit.
func TestHistogram(t *testing.T) {
	h := NewHistogram(MetricLockHold)
	if s := h.Snapshot(); s.Count != 0 || s.P99MS != 0 {
		t.Errorf("an empty histogram should report nothing: %+v", s)
	}

	for i := 0; i < 99; i++ {
		h.Observe(3 * time.Millisecond)
	}
	h.Observe(400 * time.Millisecond)

	bound3 := bucketFor(3)
	s := h.Snapshot()
	if s.Name != MetricLockHold {
		t.Errorf("metric name = %q, want %q", s.Name, MetricLockHold)
	}
	if s.Count != 100 {
		t.Errorf("count = %d, want 100", s.Count)
	}
	if s.MaxMS < 400 {
		t.Errorf("max = %v, want >= 400", s.MaxMS)
	}
	// 99 of the 100 samples are 3ms, so the p50 and p95 must both land in
	// that sample's bucket.
	if s.P50MS != bound3 {
		t.Errorf("p50 = %v, want %v", s.P50MS, bound3)
	}
	if s.P95MS != bound3 {
		t.Errorf("p95 = %v, want %v", s.P95MS, bound3)
	}
	if s.Buckets[formatBound(bound3)] != 99 {
		t.Errorf("cumulative count at %vms = %d, want 99", bound3, s.Buckets[formatBound(bound3)])
	}
	if s.Buckets["+Inf"] != 100 {
		t.Errorf("cumulative count at +Inf = %d, want 100", s.Buckets["+Inf"])
	}
	if s.Tripwire == "" {
		t.Error("the snapshot should carry the tripwire so whoever reads it knows the threshold")
	}
}

// bucketFor is the bound a sample of ms milliseconds falls in.
func bucketFor(ms float64) float64 {
	for _, b := range lockHoldBucketsMS {
		if ms <= b {
			return b
		}
	}
	return lockHoldBucketsMS[len(lockHoldBucketsMS)-1] * 2
}

// TestHistogramQuantileOverEstimates: a tripwire must err towards tripping
// early. Reporting the bucket's upper bound guarantees the reported quantile
// is never LOWER than the true one, which is the only direction that is safe
// to be wrong in.
func TestHistogramQuantileOverEstimates(t *testing.T) {
	// A sample deliberately just inside a bucket rather than on its edge.
	const sampleMS = 6
	want := bucketFor(sampleMS)
	if want <= sampleMS {
		t.Fatalf("the test sample %vms sits on a bucket edge (%v); pick one that does not", float64(sampleMS), want)
	}

	h := NewHistogram(MetricLockHold)
	for i := 0; i < 100; i++ {
		h.Observe(sampleMS * time.Millisecond)
	}
	got := h.Snapshot().P99MS
	if got != want {
		t.Errorf("p99 = %v, want the bucket's upper bound %v", got, want)
	}
	if got < sampleMS {
		t.Errorf("p99 = %v under-reports the true value %v; a tripwire must never trip late", got, float64(sampleMS))
	}
}

// TestHistogramBucketsAreSorted: quantile() and Observe() both binary-search
// the bounds, so an unsorted or duplicated bucket list silently mis-files
// every sample.
func TestHistogramBucketsAreSorted(t *testing.T) {
	for i := 1; i < len(lockHoldBucketsMS); i++ {
		if lockHoldBucketsMS[i] <= lockHoldBucketsMS[i-1] {
			t.Fatalf("buckets are not strictly increasing at index %d: %v", i, lockHoldBucketsMS)
		}
	}
	// The tripwire's threshold must itself be a bucket bound, or the p99 can
	// never report exactly 25 and the comparison is against a number the
	// histogram cannot produce.
	found := false
	for _, b := range lockHoldBucketsMS {
		if b == LockHoldWarnMS {
			found = true
		}
	}
	if !found {
		t.Errorf("%d is the tripwire but is not a bucket bound: %v", LockHoldWarnMS, lockHoldBucketsMS)
	}
}
