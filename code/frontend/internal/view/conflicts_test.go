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

// The duplicate_work wording of docs/DUPLICATES.md §9.3 / §4.10, verbatim.
const (
	dupAction      = "INT-83 (Ana (ui), active 4m) is already building this: \"add login page\". Stop before you edit: mark INT-92 superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with Ana's agent (S-17) first."
	dupJudgeNote   = "S-22's model (0.85): both build the login page; INT-83 already has the route"
	dupYieldedNote = "Settled by the agents: S-22 (frontend) marked INT-92 superseded at 14:15 UTC, leaving INT-83 (S-17 (ui)) to build it. A person was asked at 14:12 UTC."
)

func dupPlansLogin() []model.ConflictPlan {
	return []model.ConflictPlan{
		{Key: "INT-83", Who: "Ana (ui)", Summary: "add login page", Path: "web/src/routes/login.tsx"},
		{Key: "INT-92", Who: "Bob (frontend)", Summary: "build the login screen", Path: "web/src/pages/Login.tsx", Yields: true},
	}
}

// duplicatesSnapshot: §9.3's conflict escalated (CF-44), a fresh one the
// incumbent has not been told about yet (CF-45, the initiator only, sharing
// an issue id), the §9.3 conflict settled (CF-43), and a path overlap
// (CF-40), so both kinds show together.
func duplicatesSnapshot() state.Snapshot {
	now := time.Date(2026, 9, 21, 14, 20, 0, 0, time.UTC)
	initiator := model.Participant{SessionKey: "S-22", Role: "initiator", Detail: "intent", MemberName: "Bob", AgentLabel: "frontend"}
	incumbent := model.Participant{SessionKey: "S-17", Role: "incumbent", Detail: "intent", MemberName: "Ana", AgentLabel: "ui"}
	asked := time.Date(2026, 9, 21, 14, 12, 40, 0, time.UTC)
	raised := time.Date(2026, 9, 21, 14, 2, 40, 0, time.UTC)
	return state.Snapshot{
		Team: model.Team{Slug: "test", Name: "Test"},
		Now:  now,
		Conflicts: []*model.Conflict{
			{Key: "CF-44", Kind: model.KindDuplicateWork, Severity: "medium", Status: "open", SuggestedAction: dupAction,
				Plans: dupPlansLogin(), Signals: []string{"shared words: login, screen"}, JudgeNote: dupJudgeNote,
				EscalatedAt: asked, RaisedAt: raised,
				Paths:        []string{"web/src/pages/Login.tsx", "web/src/routes/login.tsx"},
				Participants: []model.Participant{initiator, incumbent}},
			{Key: "CF-45", Kind: model.KindDuplicateWork, Severity: "medium", Status: "open", IssueRef: "ISSUE-412",
				SuggestedAction: "test: INT-80 (Ana (ui), active 2m) is already building this: \"stripe checkout\". Stop before you edit.",
				Plans: []model.ConflictPlan{
					{Key: "INT-80", Who: "Ana (ui)", Summary: "stripe checkout"},
					{Key: "INT-95", Who: "Bob (frontend)", Summary: "checkout flow with stripe", Yields: true},
				},
				Signals:   []string{"same issue ISSUE-412", "shared words: stripe, checkout"},
				JudgeNote: "S-22's model (0.90): the same checkout", RaisedAt: now.Add(-time.Minute),
				Participants: []model.Participant{initiator}},
			{Key: "CF-40", Kind: model.KindPathOverlap, Severity: "medium", Status: "open",
				SuggestedAction: "test: settle web/src/app.tsx between you", RaisedAt: now.Add(-3 * time.Minute),
				Paths:        []string{"web/src/app.tsx"},
				Participants: []model.Participant{{SessionKey: "S-22", Role: "challenger"}, {SessionKey: "S-17", Role: "holder"}}},
			{Key: "CF-43", Kind: model.KindDuplicateWork, Severity: "medium", Status: "resolved", Resolution: "yielded",
				ResolutionNote: dupYieldedNote, SuggestedAction: dupAction, Plans: dupPlansLogin(),
				Signals: []string{"shared words: login, screen"}, JudgeNote: dupJudgeNote,
				EscalatedAt: asked, RaisedAt: raised, ResolvedAt: time.Date(2026, 9, 21, 14, 15, 2, 0, time.UTC),
				Participants: []model.Participant{initiator, incumbent}},
		},
	}
}

// TestConflictsPageDuplicateCards: an open duplicate card names both plans
// (key, who, summary, path), badges the side asked to stop and says why,
// shows why they were paired, the backend's §4.10 action and what the judge
// said; the escalated one says a person was asked; the settled one shows its
// §4.10 note instead of an action; the path overlap beside them is unchanged.
func TestConflictsPageDuplicateCards(t *testing.T) {
	page := renderHTML(t, ConflictsPage(duplicatesSnapshot()))

	esc := section(t, page, "CF-44")
	escText := flat(esc)
	for _, want := range []string{
		"CF-44 duplicate work a person was asked 14:12 UTC",
		"INT-83 Ana (ui) “add login page” web/src/routes/login.tsx declared first",
		"INT-92 asked to stop Bob (frontend) “build the login screen” web/src/pages/Login.tsx declared later, so asked to yield",
		"why paired shared words: login, screen",
		"do this " + dupAction,
		"what the judge said " + dupJudgeNote,
	} {
		if !strings.Contains(escText, want) {
			t.Errorf("the escalated duplicate card does not read %q:\n%s", want, escText)
		}
	}
	order := []string{"INT-83 Ana", "INT-92 asked", "why paired", "do this", "what the judge said"}
	for i := 1; i < len(order); i++ {
		if strings.Index(escText, order[i-1]) > strings.Index(escText, order[i]) {
			t.Errorf("%q is not above %q", order[i-1], order[i])
		}
	}
	for _, want := range []string{
		`class="dup-plan"`, `class="dup-plan dup-yields"`, `class="badge dup-stop"`, `class="dup-chip"`,
		`class="badge dec-asked"`, `title="metiche asked a person at 2026-09-21 14:12 UTC`, `class="suggest dec-judge"`,
	} {
		if !strings.Contains(esc, want) {
			t.Errorf("the escalated duplicate card is missing %s", want)
		}
	}
	if n := strings.Count(esc, ">asked to stop<"); n != 1 {
		t.Errorf("%d plans badged asked to stop, want exactly the yield side", n)
	}
	if strings.Contains(esc, `class="paths"`) || strings.Contains(escText, "are building the same thing") {
		t.Error("the duplicate card repeated its paths in a paths block, or replaced the backend's action with the cadence phrase")
	}

	open := section(t, page, "CF-45")
	openText := flat(open)
	for _, want := range []string{
		"CF-45 duplicate work ISSUE-412",
		"INT-80 Ana (ui) “stripe checkout” declared first",
		"INT-95 asked to stop Bob (frontend) “checkout flow with stripe” declared later, so asked to yield",
		"why paired same issue ISSUE-412 shared words: stripe, checkout",
		"what the judge said S-22's model (0.90): the same checkout",
	} {
		if !strings.Contains(openText, want) {
			t.Errorf("the open duplicate card does not read %q:\n%s", want, openText)
		}
	}
	if strings.Contains(open, "dec-asked") || strings.Contains(open, "dup-path") {
		t.Error("the fresh duplicate says a person was asked, or drew a path it does not have")
	}

	settled := section(t, page, "CF-43")
	settledText := flat(settled)
	for _, want := range []string{"resolved yielded", "settled 14:15 UTC", "how it was settled " + dupYieldedNote,
		"INT-92 asked to stop", "a person was asked 14:12 UTC"} {
		if !strings.Contains(settledText, want) {
			t.Errorf("the settled duplicate card does not read %q:\n%s", want, settledText)
		}
	}
	if strings.Contains(settledText, "do this") || strings.Contains(settledText, "what the judge said") {
		t.Errorf("the settled duplicate card still shows an action or the judge's note:\n%s", settledText)
	}

	overlap := flat(section(t, page, "CF-40"))
	if !strings.Contains(overlap, "path overlap") || !strings.Contains(overlap, "web/src/app.tsx") || strings.Contains(overlap, "why paired") {
		t.Errorf("the path overlap card changed:\n%s", overlap)
	}
	t.Logf("escalated card: %s", escText)
	t.Logf("settled card: %s", settledText)
}

// TestConflictHistoryDuplicateRowAndKindFilter: duplicate_work is a kind
// with a readable label, the history filter offers it by that label, and a
// past duplicate row shows its two plans and its note.
func TestConflictHistoryDuplicateRowAndKindFilter(t *testing.T) {
	if got := conflictLabel(model.KindDuplicateWork); got != "duplicate work" {
		t.Fatalf("conflictLabel(duplicate_work) = %q", got)
	}
	snap := duplicatesSnapshot()
	p := ConflictHistoryParams{Slug: "test", Now: snap.Now, Kind: model.KindDuplicateWork,
		Statuses: []string{"resolved", "dismissed", "expired"},
		Kinds:    []string{"path_overlap", "contract_mismatch", "stale_base", "decision_contradiction", "duplicate_work"},
		Rows:     []*model.Conflict{snap.Conflicts[3]}}
	page := renderHTML(t, ConflictsHistoryPage(snap, p))
	for _, want := range []string{
		`<a class="fchip on" aria-current="true" href="/t/test/conflicts?kind=duplicate_work">duplicate work</a>`,
		`<a class="fchip" href="/t/test/conflicts?kind=path_overlap">path overlap</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the kind filter is missing %s", want)
		}
	}
	row := flat(section(t, page, "past-CF-43"))
	for _, want := range []string{"CF-43 duplicate work", "resolved yielded", "how it ended " + dupYieldedNote,
		"INT-83 Ana (ui) “add login page”", "INT-92 asked to stop Bob (frontend) “build the login screen”",
		"why paired shared words: login, screen"} {
		if !strings.Contains(row, want) {
			t.Errorf("the past duplicate row does not read %q:\n%s", want, row)
		}
	}
	t.Logf("past row: %s", row)
}
