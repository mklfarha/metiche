package state

import (
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// multiSessionSnapshot is one member running two terminals of one client —
// two live sessions on the same agent — plus a second agent with nothing
// started. internal/view/board_test.go mirrors it for the render test.
func multiSessionSnapshot() Snapshot {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	return Snapshot{
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
			// Listed newest first on purpose: lanes must come out by start time.
			{Key: "S-second", MemberKey: "M-1", AgentKey: "A-claude", Status: model.SessionLive,
				StatusLine: "second terminal", StartedAt: now.Add(-5 * time.Minute), LastHeartbeatAt: now},
			{Key: "S-first", MemberKey: "M-1", AgentKey: "A-claude", Status: model.SessionLive,
				StatusLine: "first terminal", StartedAt: now.Add(-20 * time.Minute), LastHeartbeatAt: now},
			// A finished session on the same agent must not get its own lane
			// while live ones exist.
			{Key: "S-old", MemberKey: "M-1", AgentKey: "A-claude", Status: model.SessionEnded,
				StartedAt: now.Add(-2 * time.Hour)},
		},
		Now: now,
	}
}

// TestTwoLiveSessionsOnOneAgentYieldTwoLanes is the point of a board that
// understands terminals: a second concurrent session on one agent must be
// visible, and the header must still count agents, not lanes.
func TestTwoLiveSessionsOnOneAgentYieldTwoLanes(t *testing.T) {
	lanes := multiSessionSnapshot().Lanes()
	if len(lanes) != 1 {
		t.Fatalf("lanes = %d, want 1 member lane", len(lanes))
	}
	lane := lanes[0]
	if len(lane.Agents) != 3 {
		for _, a := range lane.Agents {
			t.Logf("agent lane: agent=%s session=%+v", a.Agent.Key, a.Session)
		}
		t.Fatalf("agent lanes = %d, want 3 (two live sessions + one idle agent)", len(lane.Agents))
	}

	wantKeys := []string{"S-first", "S-second"}
	for i, want := range wantKeys {
		al := lane.Agents[i]
		if al.Agent.Key != "A-claude" {
			t.Fatalf("lane %d agent = %s, want A-claude", i, al.Agent.Key)
		}
		if al.Session == nil || al.Session.Key != want {
			t.Fatalf("lane %d session = %+v, want %s (sorted by start)", i, al.Session, want)
		}
		if !al.Multi {
			t.Fatalf("lane %d (%s) Multi = false, want true", i, want)
		}
		if al.Idle() {
			t.Fatalf("lane %d (%s) is idle, want working", i, want)
		}
	}

	idle := lane.Agents[2]
	if idle.Agent.Key != "A-cursor" {
		t.Fatalf("idle lane agent = %s, want A-cursor", idle.Agent.Key)
	}
	if idle.Multi {
		t.Fatal("idle agent lane is Multi, want false")
	}
	if !idle.Idle() || idle.Session != nil {
		t.Fatalf("idle agent lane = %+v, want idle with no session", idle)
	}

	if got := lane.Active(); got != 1 {
		t.Fatalf("Active() = %d, want 1 — two lanes of one agent are one working agent", got)
	}
}

// TestIdleAgentKeepsItsLastFinishedSession is the other half: nothing live
// still renders one lane, showing the most recent finished run, not Multi.
func TestIdleAgentKeepsItsLastFinishedSession(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s := Snapshot{
		Members: []*model.Member{{Key: "M-1", DisplayName: "Ada",
			Agents: []*model.Agent{{Key: "A-1", MemberKey: "M-1", Label: "claude"}}}},
		Sessions: []*model.Session{
			{Key: "S-older", AgentKey: "A-1", Status: model.SessionEnded, StartedAt: now.Add(-3 * time.Hour)},
			{Key: "S-newer", AgentKey: "A-1", Status: model.SessionAbandoned, StartedAt: now.Add(-1 * time.Hour)},
		},
		Now: now,
	}
	lane := s.Lanes()[0]
	if len(lane.Agents) != 1 {
		t.Fatalf("agent lanes = %d, want 1", len(lane.Agents))
	}
	al := lane.Agents[0]
	if al.Session == nil || al.Session.Key != "S-newer" || al.Multi || !al.Idle() {
		t.Fatalf("lane = %+v session=%+v, want idle S-newer, not Multi", al, al.Session)
	}
	if lane.Active() != 0 {
		t.Fatalf("Active() = %d, want 0", lane.Active())
	}
}
