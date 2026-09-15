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

// convergedNote is §9.4's settled resolution_note, verbatim.
const convergedNote = "Settled by the agents: S-22 (ui) revised INT-91 at 14:15 UTC and judged it no longer contradicts #auth-jwt-cookie (r2): \"plan now relies on the httpOnly cookie\". A person was asked at 14:12 UTC."

// TestConflictsPageDecisionCards: an open decision conflict shows its
// decision key, what the judge said under the suggested action, and that a
// person was asked, with the time; a converged one shows the note unchanged.
func TestConflictsPageDecisionCards(t *testing.T) {
	now := time.Date(2026, 9, 15, 14, 20, 0, 0, time.UTC)
	parts := []model.Participant{
		{SessionKey: "S-22", Role: "initiator", Detail: "intent", MemberName: "Bob", AgentLabel: "ui"},
		{SessionKey: "S-17", Role: "incumbent", Detail: "decision", MemberName: "Ana", AgentLabel: "backend"},
	}
	snap := state.Snapshot{
		Team: model.Team{Slug: "test", Name: "Test"},
		Now:  now,
		Conflicts: []*model.Conflict{
			{Key: "CF-31", Kind: model.KindDecisionContradiction, Severity: "high", Status: "open",
				SuggestedAction: "test: change the plan to follow #auth-jwt-cookie", DecisionKey: "#auth-jwt-cookie",
				JudgeNote:   "S-22's model (0.90): plan stores the token in localStorage; #auth-jwt-cookie forbids it",
				EscalatedAt: time.Date(2026, 9, 15, 14, 12, 40, 0, time.UTC), RaisedAt: now.Add(-18 * time.Minute),
				Paths: []string{"web/src/auth/session.ts", "web/src/auth/**"}, Participants: parts},
			{Key: "CF-30", Kind: model.KindDecisionContradiction, Severity: "high", Status: "resolved", Resolution: "converged",
				ResolutionNote: convergedNote, DecisionKey: "#auth-jwt-cookie", JudgeNote: "test: an older note",
				EscalatedAt: time.Date(2026, 9, 15, 14, 12, 40, 0, time.UTC),
				RaisedAt:    now.Add(-40 * time.Minute), ResolvedAt: time.Date(2026, 9, 15, 14, 15, 2, 0, time.UTC), Participants: parts},
		},
	}
	page := renderHTML(t, ConflictsPage(snap))

	open := section(t, page, "CF-31")
	openText := flat(open)
	for _, want := range []string{
		"contradicts a decision #auth-jwt-cookie a person was asked 14:12 UTC",
		"do this", "what the judge said S-22's model (0.90): plan stores the token in localStorage; #auth-jwt-cookie forbids it",
	} {
		if !strings.Contains(openText, want) {
			t.Errorf("the open decision card does not read %q:\n%s", want, openText)
		}
	}
	if strings.Index(openText, "do this") > strings.Index(openText, "what the judge said") {
		t.Error("the judge's note is not under the suggested action")
	}
	for _, want := range []string{`class="badge dec-asked"`, `title="metiche asked a person at 2026-09-15 14:12 UTC`, `href="/t/test/decisions/history?key=%23auth-jwt-cookie"`} {
		if !strings.Contains(open, want) {
			t.Errorf("the open decision card is missing %s", want)
		}
	}
	// Only the plan's side contradicts the decision; the decider does not.
	if strings.Contains(openText, "S-17 is about to contradict") || strings.Contains(openText, "and S-17") {
		t.Errorf("the decider was named as contradicting its own decision:\n%s", openText)
	}

	settled := section(t, page, "CF-30")
	settledText := flat(settled)
	for _, want := range []string{"converged", "how it was settled", convergedNote, "a person was asked 14:12 UTC"} {
		if !strings.Contains(settledText, want) {
			t.Errorf("the converged card does not read %q:\n%s", want, settledText)
		}
	}
	if strings.Contains(settledText, "what the judge said") || strings.Contains(settledText, "do this") {
		t.Errorf("a settled decision card still shows the judge's note or an action:\n%s", settledText)
	}
	t.Logf("open card: %s", openText)
	t.Logf("settled card: %s", settledText)
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
