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

// TestContractsHeadersOnePerSessionInColumnOrder: each session heads its own
// column, in the matrix's column order, with a class of its own. The header was
// once class="who", which is also the topbar's flex indicator, and the session
// headers stacked in one cell.
func TestContractsHeadersOnePerSessionInColumnOrder(t *testing.T) {
	snap := liveContractsSnapshot(t)
	var buf bytes.Buffer
	if err := ContractsPage(snap).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	thead := html[strings.Index(html, "<thead>"):strings.Index(html, "</thead>")]
	if strings.Contains(thead, `class="who"`) {
		t.Error(`a matrix header still uses class="who", the topbar indicator's flex rule`)
	}

	cols := snap.ContractMatrix().Columns
	if len(cols) != 2 {
		t.Fatalf("fixture should have 2 session columns, has %d", len(cols))
	}
	parts := strings.Split(thead, `<th class="col-session" scope="col">`)
	if got := len(parts) - 1; got != len(cols) {
		t.Fatalf("%d session header cells for %d sessions:\n%s", got, len(cols), thead)
	}
	for i, col := range cols {
		cell := parts[i+1]
		cell = cell[:strings.Index(cell, "</th>")]
		want := `<span class="m">` + col.Member + `</span> <span class="a">` + col.Agent + `</span>`
		if !strings.Contains(cell, want) {
			t.Errorf("header %d = %q, want %s", i, cell, want)
		}
	}
	if got := []string{cols[0].Member, cols[1].Member}; got[0] != "Ana" || got[1] != "Bob" {
		t.Errorf("column order = %v, want [Ana Bob]", got)
	}
	// Every row has exactly one marker cell per session header.
	for _, tr := range strings.Split(html[strings.Index(html, "<tbody>"):], "<tr ")[1:] {
		if n := strings.Count(tr, `<td class="cell`); n != len(cols) {
			t.Errorf("a row has %d marker cells for %d session headers", n, len(cols))
		}
	}
}

// TestContractsRowReadsContractVerdictThenSessions: the phone layout stacks
// each row in source order, so the DOM order is the reading order: contract
// key, verdict and diffs, then the session chips. Each chip names its session
// and its role in words, not by colour alone.
func TestContractsRowReadsContractVerdictThenSessions(t *testing.T) {
	var buf bytes.Buffer
	if err := ContractsPage(liveContractsSnapshot(t)).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	start := strings.Index(html, "POST /api/login")
	if start < 0 {
		t.Fatal("no POST /api/login row")
	}
	row := html[start : start+strings.Index(html[start:], "</tr>")]
	order := []string{`class="rowkind"`, `class="verdict"`, `<ul class="diffs">`, `<td class="cell`}
	last := -1
	for _, mark := range order {
		i := strings.Index(row, mark)
		if i < 0 {
			t.Fatalf("row has no %s:\n%s", mark, row)
		}
		if i < last {
			t.Errorf("%s comes before the previous part of the row; want order %v", mark, order)
		}
		last = i
	}
	for _, want := range []string{
		`<span class="cell-who"><b>Ana</b> <span class="a">api</span></span>`,
		`<span class="cell-role">produces</span>`,
		`<span class="cell-who"><b>Bob</b> <span class="a">web</span></span>`,
		`<span class="cell-role">consumes · shape differs</span>`,
		`<span class="fc-long">2 fields</span>`,
		`<span class="fc-long">3 fields</span>`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("the login row's session chips do not show %s", want)
		}
	}
	// The unclaimed row: Ana has no assertion, so her cell is marked cell-none
	// (no chip on a phone; not "empty", the board's empty-state class) and
	// Bob's says there is no producer.
	legs := html[strings.Index(html, "GET /api/legs"):]
	legs = legs[:strings.Index(legs, "</tr>")]
	if strings.Contains(legs, `class="cell empty"`) {
		t.Error(`a marker cell uses class "empty", which picks up the empty-state block's styles`)
	}
	for _, want := range []string{`<td class="cell cell-none">`, `<span class="cell-role">consumes · no producer</span>`} {
		if !strings.Contains(legs, want) {
			t.Errorf("the unclaimed row does not show %s", want)
		}
	}
}

func TestCellRoleSaysTheMarkerInWords(t *testing.T) {
	for _, tc := range []struct {
		cell      state.MatrixCell
		unclaimed bool
		want      string
	}{
		{state.MatrixCell{State: state.CellEmpty}, false, ""},
		{state.MatrixCell{State: state.CellProduces}, false, "produces"},
		{state.MatrixCell{State: state.CellConsumes}, false, "consumes"},
		{state.MatrixCell{State: state.CellBoth}, false, "produces + consumes"},
		{state.MatrixCell{State: state.CellConsumes, Mismatched: true}, false, "consumes · shape differs"},
		{state.MatrixCell{State: state.CellConsumes}, true, "consumes · no producer"},
		{state.MatrixCell{State: state.CellProduces, Mismatched: true}, true, "produces"},
	} {
		if got := cellRole(tc.cell, tc.unclaimed); got != tc.want {
			t.Errorf("cellRole(%+v, unclaimed=%v) = %q, want %q", tc.cell, tc.unclaimed, got, tc.want)
		}
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
