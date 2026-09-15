package state

import (
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// TestWindowGraphCrossesOnlyWhereHoldsOverlapInTime: two sessions in one area
// at the same moment are a crossing; two in one area at different times are a
// hand-off and never a crossing; a hold still held overlaps everything after
// it began; a hold of a session the window does not list is left out.
func TestWindowGraphCrossesOnlyWhereHoldsOverlapInTime(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	w := model.GraphWindow{
		Window: "24h", From: at(0), To: at(24),
		Sessions: []model.RunSummary{
			{Key: "S-1", MemberName: "Ana", AgentLabel: "claude-1", Status: "ended"},
			{Key: "S-2", MemberName: "Bob", AgentLabel: "claude-2", Status: "live"},
			{Key: "S-3", MemberName: "Ana", AgentLabel: "claude-3", Status: "ended", Outcome: "succeeded"},
		},
		Holds: []model.GraphHold{
			{SessionKey: "S-1", Path: "api/x.go", Mode: "write", From: at(1), Until: at(2)},
			{SessionKey: "S-3", Path: "api/y.go", Mode: "write", From: at(2), Until: at(3)}, // touches at 2: not overlapping
			{SessionKey: "S-1", Path: "web/a.tsx", Mode: "write", From: at(5), Until: at(8)},
			{SessionKey: "S-2", Path: "web/b.tsx", Mode: "write", From: at(7)}, // still held
			{SessionKey: "S-3", Path: "docs/c.md", Mode: "read", From: at(9)},
			{SessionKey: "S-3", Path: "docs/d.md", Mode: "read", From: at(10), Until: at(11)},
			{SessionKey: "S-404", Path: "web/z.tsx", Mode: "write", From: at(6), Until: at(7)},
		},
		Conflicts: []*model.Conflict{{Key: "CF-9", Severity: "high", Status: "resolved", Paths: []string{"web/a.tsx", "web/**"}}},
	}
	g := WindowGraph("test", w)

	web, api, docs := g.Node("path:web/"), g.Node("path:api/"), g.Node("path:docs/")
	if web == nil || api == nil || docs == nil {
		t.Fatalf("nodes = %+v", g.Nodes)
	}
	if web.Degree != 2 || web.Handoff || web.Severity != "high" {
		t.Fatalf("web/ = %+v, want a crossing of 2 with the conflict's severity", web)
	}
	if api.Degree != 2 || !api.Handoff {
		t.Fatalf("api/ = %+v, want a hand-off of 2", api)
	}
	if docs.Degree != 1 || docs.Handoff || docs.Side == "" {
		t.Fatalf("docs/ = %+v, want one session's own area", docs)
	}
	if g.Contended != 1 || g.Handoffs != 1 || g.Calm {
		t.Fatalf("contended %d, handoffs %d, calm %v", g.Contended, g.Handoffs, g.Calm)
	}
	if g.Node("session:S-404") != nil {
		t.Fatal("a hold of a session outside the window drew that session")
	}
	for _, e := range g.Edges {
		if (e.To == "path:web/") != e.Hot {
			t.Fatalf("edge %s -> %s hot=%v; only the crossing's edges are hot", e.From, e.To, e.Hot)
		}
	}
	if s3 := g.Node("session:S-3"); s3 == nil || s3.Label != "Ana · claude-3" || s3.Sub != "S-3 · ended · succeeded" || s3.Href != "/t/test/runs/S-3" {
		t.Fatalf("S-3 = %+v", s3)
	}
	if g.Nodes[3].ID != "path:web/" || g.Nodes[4].ID != "path:api/" {
		t.Fatalf("artifact order = %s, %s; want the crossing, then the hand-off", g.Nodes[3].ID, g.Nodes[4].ID)
	}
	if web.Title != "S-1 Sep 1 05:00–Sep 1 08:00 UTC; S-2 Sep 1 07:00–still held UTC" {
		t.Fatalf("web/ title = %q", web.Title)
	}
}
