package view

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
)

const settledViewNote = "Settled by the agents: S-20 (claude) released app/rest.go at 16:47 UTC and kept working in app/salsas_picosas.go; S-19 (codex) still holds app/rest.go."

func settledSnapshot() state.Snapshot {
	now := time.Date(2026, 9, 13, 16, 50, 0, 0, time.UTC)
	return state.Snapshot{
		Team: model.Team{Slug: "demo", Name: "Demo"},
		Members: []*model.Member{{
			Key: "M-1", DisplayName: "Ana",
			Agents: []*model.Agent{
				{Key: "A-claude", MemberKey: "M-1", Label: "claude"},
				{Key: "A-codex", MemberKey: "M-1", Label: "codex"},
			},
		}},
		Sessions: []*model.Session{
			{Key: "S-19", MemberKey: "M-1", AgentKey: "A-codex", Status: model.SessionLive, StartedAt: now.Add(-time.Hour), LastHeartbeatAt: now},
			{Key: "S-20", MemberKey: "M-1", AgentKey: "A-claude", Status: model.SessionLive, StartedAt: now.Add(-time.Hour), LastHeartbeatAt: now},
		},
		Conflicts: []*model.Conflict{
			{Key: "CF-24", Kind: model.KindPathOverlap, Severity: "medium", Status: "open",
				SuggestedAction: "settle it between you", RaisedAt: now.Add(-2 * time.Minute),
				Participants: []model.Participant{{SessionKey: "S-19", Role: "initiator"}, {SessionKey: "S-20", Role: "incumbent"}}},
			{Key: "CF-23", Kind: model.KindPathOverlap, Severity: "high", Status: "resolved",
				Resolution: "coordinated", ResolutionNote: settledViewNote,
				SuggestedAction: "settle it between you", RaisedAt: now.Add(-20 * time.Minute),
				ResolvedAt:   time.Date(2026, 9, 13, 16, 47, 0, 0, time.UTC),
				Participants: []model.Participant{{SessionKey: "S-19", Role: "initiator"}, {SessionKey: "S-20", Role: "incumbent"}}},
		},
		Now: now,
	}
}

// TestConflictsPageShowsHowItWasSettled renders the conflicts page: the open
// conflict above, and the settled one under "Settled" with its resolution
// kind, its note and the time — read-only, with no suggested action left.
func TestConflictsPageShowsHowItWasSettled(t *testing.T) {
	var buf bytes.Buffer
	if err := ConflictsPage(settledSnapshot()).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	heading := strings.Index(html, ">Settled</h2>")
	if heading < 0 {
		t.Fatalf("no Settled section:\n%s", html)
	}
	open, settled := strings.Index(html, `id="CF-24"`), strings.Index(html, `id="CF-23"`)
	if open < 0 || open > heading {
		t.Fatalf("the open conflict is not above the Settled section (open at %d, heading at %d)", open, heading)
	}
	if settled < heading {
		t.Fatalf("the settled conflict is not in the Settled section (settled at %d, heading at %d)", settled, heading)
	}
	card := html[settled:]
	for _, want := range []string{"coordinated", "how it was settled", settledViewNote, "settled 16:47 UTC", "3m ago"} {
		if !strings.Contains(card, want) {
			t.Errorf("settled card is missing %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "do this") {
		t.Errorf("a settled card still tells people what to do:\n%s", card)
	}
	for _, control := range []string{"<button", "<form", "hx-post"} {
		if strings.Contains(html, control) {
			t.Errorf("the conflicts page grew a write control (%s)", control)
		}
	}
	t.Logf("settled card: %s", strings.Join(strings.Fields(card[:strings.Index(card, `class="parties"`)]), " "))
}

// TestBoardBadgesSkipSettledConflicts: the lane badges and the banner are for
// open conflicts only.
func TestBoardBadgesSkipSettledConflicts(t *testing.T) {
	var buf bytes.Buffer
	if err := Board(settledSnapshot()).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if strings.Contains(html, "#CF-23") {
		t.Errorf("a settled conflict still has a lane badge:\n%s", html)
	}
	if !strings.Contains(html, "#CF-24") {
		t.Errorf("the open conflict lost its lane badge:\n%s", html)
	}
}
