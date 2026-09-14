package state

import (
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// supervisorSnapshot is Ana running one supervisor session (S-41) with three
// subagent sessions on the same agent, one subagent whose supervisor (S-40)
// is not on the board, and a second agent with nothing started. Listed out
// of order on purpose. internal/view/subagents_test.go mirrors it.
func supervisorSnapshot() Snapshot {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	live := func(key, parent string, startedMin int) *model.Session {
		return &model.Session{Key: key, MemberKey: "M-1", AgentKey: "A-claude", Status: model.SessionLive,
			StatusLine: "working on " + key, Goal: "goal of " + key, ParentSessionKey: parent,
			StartedAt: now.Add(-time.Duration(startedMin) * time.Minute), LastHeartbeatAt: now}
	}
	return Snapshot{
		Team: model.Team{Slug: "demo", Name: "Demo"},
		Members: []*model.Member{{
			Key: "M-1", DisplayName: "Ana",
			Agents: []*model.Agent{
				{Key: "A-claude", MemberKey: "M-1", Label: "claude"},
				{Key: "A-idle", MemberKey: "M-1", Label: "cursor"},
			},
		}},
		Sessions: []*model.Session{
			live("S-50", "S-40", 50), // its supervisor ended: not on the board
			live("S-43", "S-41", 10),
			live("S-41", "", 30),
			live("S-44", "S-41", 5),
			live("S-42", "S-41", 20),
		},
		Now: now,
	}
}

func TestSubagentLanesNestUnderTheirSupervisor(t *testing.T) {
	lanes := supervisorSnapshot().Lanes()
	if len(lanes) != 1 {
		t.Fatalf("member lanes = %d, want 1", len(lanes))
	}
	lane := lanes[0]
	type row struct {
		key, parent      string
		nested, parentOK bool
		depth, subagents int
	}
	var got []row
	for _, al := range lane.Agents {
		key := ""
		if al.Session != nil {
			key = al.Session.Key
		}
		got = append(got, row{key, al.ParentKey, al.Nested, al.ParentLive, al.Depth, al.Subagents})
		t.Logf("lane %-5s parent=%-5s nested=%-5v parentLive=%-5v depth=%d subagents=%d", key, al.ParentKey, al.Nested, al.ParentLive, al.Depth, al.Subagents)
	}
	want := []row{
		{"S-50", "S-40", false, false, 0, 0}, // oldest live lane, supervisor gone: normal place
		{"S-41", "", false, false, 0, 3},     // the supervisor
		{"S-42", "S-41", true, true, 1, 0},   // its subagents, oldest first, right under it
		{"S-43", "S-41", true, true, 1, 0},
		{"S-44", "S-41", true, true, 1, 0},
		{"", "", false, false, 0, 0}, // the idle agent last
	}
	if len(got) != len(want) {
		t.Fatalf("agent lanes = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lane %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Nesting reorders; it must not change what the header counts. Five
	// working lanes of one agent plus an idle agent is 1 of 2 agents.
	if a, n := lane.Active(), lane.AgentCount(); a != 1 || n != 2 {
		t.Fatalf("Active/AgentCount = %d/%d, want 1/2", a, n)
	}
	if n := supervisorSnapshot().LiveSessions(); n != 5 {
		t.Fatalf("LiveSessions = %d, want 5", n)
	}
}

func TestSubagentOfAnEndedSupervisorIsNotNested(t *testing.T) {
	s := supervisorSnapshot()
	// The supervisor ends: it stays in the snapshot, but not as a working lane.
	for _, sess := range s.Sessions {
		if sess.Key == "S-41" {
			sess.Status = model.SessionEnded
		}
	}
	lane := s.Lanes()[0]
	for _, al := range lane.Agents {
		if al.Session == nil {
			continue
		}
		if al.Session.Key == "S-41" {
			t.Fatalf("the ended supervisor still has a lane while its agent has live ones")
		}
		if al.Nested || al.Depth != 0 || al.Subagents != 0 {
			t.Fatalf("%s nested under an ended supervisor: %+v", al.Session.Key, al)
		}
		if al.Session.ParentSessionKey != "" && (al.ParentKey == "" || al.ParentLive) {
			t.Fatalf("%s should say its supervisor %s ended: parent=%q live=%v", al.Session.Key, al.Session.ParentSessionKey, al.ParentKey, al.ParentLive)
		}
	}
	if len(lane.Agents) != 5 {
		t.Fatalf("agent lanes = %d, want 4 subagents + the idle agent", len(lane.Agents))
	}
	if a, n := lane.Active(), lane.AgentCount(); a != 1 || n != 2 {
		t.Fatalf("Active/AgentCount = %d/%d, want 1/2", a, n)
	}
	subs := s.SubagentsOf("S-41")
	if len(subs) != 3 || subs[0].Key != "S-42" || subs[2].Key != "S-44" {
		t.Fatalf("SubagentsOf(S-41) = %v, want S-42, S-43, S-44", subs)
	}
}
