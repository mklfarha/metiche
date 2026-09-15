package view

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
)

// liveContractsSnapshot is what the live feed builds from the backend's
// /contracts answer for two agents: Ana produces POST /api/login, Bob consumes
// it requiring a field Ana does not return, and Bob consumes GET /api/legs
// that nobody produces. The issues come from the server, as on a live board.
func liveContractsSnapshot(t *testing.T) state.Snapshot {
	t.Helper()
	now := time.Date(2026, 9, 15, 20, 0, 0, 0, time.UTC)
	var issues []model.ContractIssue
	if err := json.Unmarshal([]byte(`[{"kind":"missing_out","path":"expires_at","expected":"timestamp","direction":"out","severity":"high",
		"note":"consumer requires out field expires_at (timestamp) that the producer does not return","producer":"S-5","consumer":"S-6"}]`), &issues); err != nil {
		t.Fatal(err)
	}
	return state.Snapshot{
		Team: model.Team{Slug: "test-team", Name: "Test team"},
		Members: []*model.Member{
			{Key: "M-1", DisplayName: "Ana", Agents: []*model.Agent{{Key: "A-1", MemberKey: "M-1", Label: "api"}}},
			{Key: "M-2", DisplayName: "Bob", Agents: []*model.Agent{{Key: "A-2", MemberKey: "M-2", Label: "web"}}},
		},
		Sessions: []*model.Session{
			{Key: "S-5", MemberKey: "M-1", AgentKey: "A-1", Status: model.SessionLive, StartedAt: now.Add(-time.Hour), LastHeartbeatAt: now},
			{Key: "S-6", MemberKey: "M-2", AgentKey: "A-2", Status: model.SessionLive, StartedAt: now.Add(-time.Hour), LastHeartbeatAt: now},
		},
		Contracts: []*model.Contract{
			{Key: "POST /api/login", Kind: "http_endpoint", Agreement: "mismatch", ServerVerdict: true, Issues: issues,
				Assertions: []*model.Assertion{
					{Key: "p", Role: "produces", SessionKey: "S-5", Status: "active", ShapeHash: "h1",
						Fields: []model.Field{{Name: "email", Type: "string", Direction: "in", Required: true}, {Name: "token", Type: "string", Direction: "out", Required: true}}},
					{Key: "c", Role: "consumes", SessionKey: "S-6", Status: "active", ShapeHash: "h2",
						Fields: []model.Field{{Name: "email", Type: "string", Direction: "in"}, {Name: "token", Type: "string", Direction: "out", Required: true}, {Name: "expires_at", Type: "timestamp", Direction: "out", Required: true}}},
				}},
			{Key: "GET /api/legs", Kind: "http_endpoint", Agreement: "unclaimed", ServerVerdict: true,
				Assertions: []*model.Assertion{{Key: "l", Role: "consumes", SessionKey: "S-6", Status: "active", ShapeHash: "h3",
					Fields: []model.Field{{Name: "legs", Type: "json", Direction: "out", Required: true}}}}},
			{Key: "GET /api/me", Kind: "http_endpoint", Agreement: "agreed", ServerVerdict: true,
				Assertions: []*model.Assertion{
					{Key: "m1", Role: "produces", SessionKey: "S-5", Status: "active", ShapeHash: "h4", Fields: []model.Field{{Name: "id", Type: "uuid", Direction: "out", Required: true}}},
					{Key: "m2", Role: "consumes", SessionKey: "S-6", Status: "active", ShapeHash: "h4", Fields: []model.Field{{Name: "id", Type: "uuid", Direction: "out", Required: true}}},
				}},
		},
		Now: now,
	}
}

// TestContractsPageRendersLiveContracts: the grid, the verdict column, the
// server's mismatch detail and the unclaimed row all render from live data.
func TestContractsPageRendersLiveContracts(t *testing.T) {
	var buf bytes.Buffer
	if err := ContractsPage(liveContractsSnapshot(t)).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		"POST /api/login", "GET /api/legs", "GET /api/me",
		"1 contract consumed but not produced", "1 shape mismatch", "1 converged",
		"is coding against this — nobody is building it",
		"1 field disagreement(s) between producer and consumer",
		`<span class="f">expires_at</span>`, "timestamp → absent", "missing_out",
		"producer and consumer agree",
		`class="mk c orphan"`, `class="mk p"`,
		"Ana", "Bob",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the Contracts page does not show %q", want)
		}
	}
	if strings.Contains(html, "No contracts published yet") {
		t.Error("the empty state rendered alongside real contracts")
	}
	// The unclaimed row sorts first, the mismatch second.
	if i, j := strings.Index(html, "GET /api/legs"), strings.Index(html, "POST /api/login"); i < 0 || j < 0 || i > j {
		t.Errorf("unclaimed row (at %d) should come before the mismatch (at %d)", i, j)
	}
}

// TestConflictLabelsNameTheLiveContractKinds: every kind the backend offers as
// a filter has a human label.
func TestConflictLabelsNameTheLiveContractKinds(t *testing.T) {
	for kind, want := range map[string]string{
		"path_overlap":            "path overlap",
		"contract_mismatch":       "contract mismatch",
		"contract_unclaimed":      "nobody is building this",
		"contract_naming_variant": "naming variant",
	} {
		if got := conflictLabel(kind); got != want {
			t.Errorf("conflictLabel(%q) = %q, want %q", kind, got, want)
		}
	}
}
