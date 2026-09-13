package view

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
)

// twoTerminals mirrors the derive test fixture: one agent with two live
// sessions (two terminals of one client) and a second agent with none.
func twoTerminals() state.Snapshot {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	return state.Snapshot{
		Team: model.Team{Slug: "demo", Name: "Demo"},
		Members: []*model.Member{{
			Key:         "M-1",
			DisplayName: "Ada",
			Agents: []*model.Agent{
				{Key: "A-claude", MemberKey: "M-1", Label: "claude on laptop"},
				{Key: "A-cursor", MemberKey: "M-1", Label: "cursor on laptop"},
			},
		}},
		Sessions: []*model.Session{
			{Key: "S-second", MemberKey: "M-1", AgentKey: "A-claude", Status: model.SessionLive,
				StatusLine: "second terminal", StartedAt: now.Add(-5 * time.Minute), LastHeartbeatAt: now},
			{Key: "S-first", MemberKey: "M-1", AgentKey: "A-claude", Status: model.SessionLive,
				StatusLine: "first terminal", StartedAt: now.Add(-20 * time.Minute), LastHeartbeatAt: now},
		},
		Now: now,
	}
}

// TestBoardShowsSessionKeyForEachTerminal renders the board for one agent
// running two live sessions and checks both lanes carry their session key on
// the label — the badge further down already shows the key on every card, so
// the label is the only place that proves Multi reached the view — and that
// the idle agent's label does not.
func TestBoardShowsSessionKeyForEachTerminal(t *testing.T) {
	var buf bytes.Buffer
	if err := Board(twoTerminals()).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	heads := regexp.MustCompile(`(?s)<div class="agent-head">.*?</div>`).FindAllString(html, -1)
	if len(heads) != 3 {
		t.Fatalf("agent heads = %d, want 3\n%s", len(heads), html)
	}
	// The header counts agents, not lanes: two terminals of one agent plus an
	// idle agent is one of two agents working.
	meta := regexp.MustCompile(`(?s)<span class="meta">(.*?)</span>`).FindStringSubmatch(html)
	if meta == nil {
		t.Fatalf("no lane meta in:\n%s", html)
	}
	t.Logf("lane meta: %s", strings.Join(strings.Fields(meta[0]), " "))
	if got := strings.TrimSpace(meta[1]); got != "1/2" {
		t.Fatalf("lane header reads %q, want \"1/2\" (1 of 2 agents working, not lanes)", got)
	}
	for _, h := range heads {
		t.Logf("agent-head: %s", h)
	}

	for i, key := range []string{"S-first", "S-second"} {
		want := `<span class="session-key">` + key + `</span>`
		if !strings.Contains(heads[i], want) {
			t.Fatalf("agent head %d missing session key %s:\n%s", i, key, heads[i])
		}
	}
	if strings.Contains(heads[2], "session-key") {
		t.Fatalf("idle agent label carries a session key:\n%s", heads[2])
	}
}
