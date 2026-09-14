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

// supervisorBoard mirrors internal/state/subagents_test.go: S-41 supervises
// S-42, S-43 and S-44; S-50's supervisor S-40 is not on the board; a second
// agent is idle.
func supervisorBoard() state.Snapshot {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	live := func(key, parent string, startedMin int) *model.Session {
		return &model.Session{Key: key, MemberKey: "M-1", AgentKey: "A-claude", Status: model.SessionLive,
			StatusLine: "working on " + key, Goal: "goal of " + key, ParentSessionKey: parent,
			StartedAt: now.Add(-time.Duration(startedMin) * time.Minute), LastHeartbeatAt: now}
	}
	return state.Snapshot{
		Team: model.Team{Slug: "demo", Name: "Demo"},
		Members: []*model.Member{{
			Key: "M-1", DisplayName: "Ana",
			Agents: []*model.Agent{
				{Key: "A-claude", MemberKey: "M-1", Label: "claude"},
				{Key: "A-idle", MemberKey: "M-1", Label: "cursor"},
			},
		}},
		Sessions: []*model.Session{
			live("S-50", "S-40", 50), live("S-43", "S-41", 10), live("S-41", "", 30),
			live("S-44", "S-41", 5), live("S-42", "S-41", 20),
		},
		Now: now,
	}
}

var articleRE = regexp.MustCompile(`(?s)<article class="([^"]*)">(.*?)</article>`)

func TestBoardNestsSubagentLanesUnderTheSupervisor(t *testing.T) {
	var buf bytes.Buffer
	if err := Board(supervisorBoard()).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	type card struct{ class, key, rel string }
	var cards []card
	keyRE := regexp.MustCompile(`<span class="session-key">([^<]+)</span>`)
	relRE := regexp.MustCompile(`(?s)<div class="subrel">(.*?)</div>`)
	tagRE := regexp.MustCompile(`<[^>]+>`)
	for _, m := range articleRE.FindAllStringSubmatch(html, -1) {
		c := card{class: m[1]}
		if k := keyRE.FindStringSubmatch(m[2]); k != nil {
			c.key = k[1]
		}
		if r := relRE.FindStringSubmatch(m[2]); r != nil {
			c.rel = strings.Join(strings.Fields(tagRE.ReplaceAllString(r[1], "")), " ")
		}
		cards = append(cards, c)
		t.Logf("card class=%-26q key=%-5s rel=%q", c.class, c.key, c.rel)
	}
	want := []card{
		{"agent working", "S-50", "↳ subagent of S-40 (ended)"},
		{"agent working", "S-41", "3 subagents"},
		{"agent working sub", "S-42", "↳ subagent of S-41"},
		{"agent working sub", "S-43", "↳ subagent of S-41"},
		{"agent working sub", "S-44", "↳ subagent of S-41"},
		{"agent idle", "", ""},
	}
	if len(cards) != len(want) {
		t.Fatalf("cards = %d, want %d\n%s", len(cards), len(want), html)
	}
	for i := range want {
		if cards[i] != want[i] {
			t.Fatalf("card %d = %+v, want %+v", i, cards[i], want[i])
		}
	}

	meta := regexp.MustCompile(`(?s)<span class="meta">(.*?)</span>`).FindStringSubmatch(html)
	if meta == nil || strings.TrimSpace(meta[1]) != "1/2" {
		t.Fatalf("lane header = %v, want 1/2 (one agent working of two, however many subagent lanes)", meta)
	}
}

func TestBoardSubagentOfMissingSupervisorFallsBack(t *testing.T) {
	s := supervisorBoard()
	// Only S-42 is left: its supervisor has ended and is not in the snapshot.
	var kept []*model.Session
	for _, sess := range s.Sessions {
		if sess.Key == "S-42" {
			kept = append(kept, sess)
		}
	}
	s.Sessions = kept

	var buf bytes.Buffer
	if err := Board(s).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if !strings.Contains(html, "↳ subagent of S-41 (ended)") {
		t.Fatalf("the orphaned subagent does not say its supervisor ended:\n%s", html)
	}
	if strings.Contains(html, "agent working sub") {
		t.Fatalf("a lane is nested under a supervisor that is not on the board:\n%s", html)
	}
	if strings.Contains(html, "subagents</span>") {
		t.Fatalf("a lane claims subagents it does not have:\n%s", html)
	}
	meta := regexp.MustCompile(`(?s)<span class="meta">(.*?)</span>`).FindStringSubmatch(html)
	if meta == nil || strings.TrimSpace(meta[1]) != "1/2" {
		t.Fatalf("lane header = %v, want 1/2", meta)
	}
}
