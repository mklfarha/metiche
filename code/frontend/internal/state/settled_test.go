package state

import (
	"testing"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

const settledNote = "Settled by the agents: S-20 (claude) released app/rest.go at 16:47 UTC and kept working in app/salsas_picosas.go; S-19 (codex) still holds app/rest.go."

func laneFor(s Snapshot, sessionKey string) *AgentLane {
	for _, l := range s.Lanes() {
		for i := range l.Agents {
			if l.Agents[i].Session != nil && l.Agents[i].Session.Key == sessionKey {
				return &l.Agents[i]
			}
		}
	}
	return nil
}

// TestResolvedEventMovesConflictToSettled is the board half of auto-resolution
// on the folded path: a conflict_resolved event moves the conflict from the
// open list to the settled one, with its note, and clears the lane badge and
// the hot path.
func TestResolvedEventMovesConflictToSettled(t *testing.T) {
	s := New("demo", "Demo")
	s.Apply(ev(1, "member_joined", true, `{"member_key":"M-1","display_name":"Ana"}`))
	s.Apply(ev(2, "agent_registered", true, `{"member_key":"M-1","agent_key":"A-claude","label":"claude"}`))
	s.Apply(ev(3, "agent_registered", true, `{"member_key":"M-1","agent_key":"A-codex","label":"codex"}`))
	s.Apply(ev(4, "session_started", true, `{"session_key":"S-19","member_key":"M-1","agent_key":"A-codex"}`))
	s.Apply(ev(5, "session_started", true, `{"session_key":"S-20","member_key":"M-1","agent_key":"A-claude"}`))
	s.Apply(ev(6, "claim_created", true, `{"claim_key":"CL-1","session_key":"S-20","paths":["app/rest.go"],"ttl_seconds":3600}`))
	s.Apply(ev(7, "claim_created", true, `{"claim_key":"CL-2","session_key":"S-19","paths":["app/rest.go"],"ttl_seconds":3600}`))
	s.Apply(ev(8, "conflict_raised", true, `{"conflict_key":"CF-23","kind":"path_overlap","severity":"high",
		"suggested_action":"settle it between you","paths":["app/rest.go"],
		"participants":[{"session_key":"S-19","member_key":"M-1","role":"challenger"},{"session_key":"S-20","member_key":"M-1","role":"holder"}]}`))

	before := s.Snapshot()
	if len(before.OpenConflicts()) != 1 || len(before.ClosedConflicts()) != 0 {
		t.Fatalf("before: open=%d settled=%d, want 1/0", len(before.OpenConflicts()), len(before.ClosedConflicts()))
	}
	lane := laneFor(before, "S-20")
	if lane == nil || len(lane.Conflicts) != 1 || lane.HotPaths["app/rest.go"] != "high" || lane.Worst() != "high" {
		t.Fatalf("before: the S-20 lane does not carry the conflict badge: %+v", lane)
	}

	// The backend's own payload shape: detail is the resolution, message the note.
	if _, ok := s.Apply(ev(9, "conflict_resolved", true,
		`{"conflict_key":"CF-23","previous_status":"open","new_status":"resolved","detail":"coordinated","message":"`+settledNote+`"}`)); !ok {
		t.Fatal("conflict_resolved was not applied")
	}

	after := s.Snapshot()
	if len(after.OpenConflicts()) != 0 {
		t.Fatalf("after: %d open conflicts, want 0", len(after.OpenConflicts()))
	}
	settled := after.ClosedConflicts()
	if len(settled) != 1 {
		t.Fatalf("after: %d settled conflicts, want 1", len(settled))
	}
	c := settled[0]
	if c.Key != "CF-23" || c.Status != "resolved" || c.Resolution != "coordinated" || c.ResolutionNote != settledNote || c.ResolvedAt.IsZero() {
		t.Fatalf("settled conflict = %+v", c)
	}
	for _, key := range []string{"S-19", "S-20"} {
		l := laneFor(after, key)
		if l == nil {
			t.Fatalf("no lane for %s", key)
		}
		if len(l.Conflicts) != 0 || len(l.HotPaths) != 0 || l.Worst() != "" {
			t.Errorf("after: lane %s still shows the conflict: conflicts=%d hot=%v worst=%q", key, len(l.Conflicts), l.HotPaths, l.Worst())
		}
	}
	if after.WorstOpen() != "" || len(after.SeverityCounts()) != 0 {
		t.Errorf("after: header still counts it: worst=%q counts=%v", after.WorstOpen(), after.SeverityCounts())
	}
}

// TestRefreshAfterResolvedFrameMovesConflictToSettled is the live path: the
// store does not fold backend frames, the structural conflict_resolved frame
// makes the hub refresh, and the refreshed state has the conflict resolved.
func TestRefreshAfterResolvedFrameMovesConflictToSettled(t *testing.T) {
	s := New("demo", "Demo")
	open := &model.Conflict{Key: "CF-23", Kind: model.KindPathOverlap, Severity: "high", Status: "open",
		Paths:        []string{"app/rest.go"},
		Participants: []model.Participant{{SessionKey: "S-19", Role: "initiator"}, {SessionKey: "S-20", Role: "incumbent"}}}
	s.Load(model.TeamState{
		Team:      model.Team{Slug: "demo", Sequence: 8, BoardRevision: 3},
		Members:   []*model.Member{{Key: "M-1", DisplayName: "Ana", Agents: []*model.Agent{{Key: "A-1", MemberKey: "M-1", Label: "claude"}}}},
		Sessions:  []*model.Session{{Key: "S-20", AgentKey: "A-1", MemberKey: "M-1", Status: model.SessionLive}},
		Conflicts: []*model.Conflict{open},
	})
	if l := laneFor(s.Snapshot(), "S-20"); l == nil || len(l.Conflicts) != 1 {
		t.Fatalf("before: lane = %+v, want the conflict badge", l)
	}

	applied, ok := s.Apply(ev(9, "conflict_resolved", true, `{"new_status":"resolved","detail":"coordinated"}`))
	if !ok || !applied.Structural {
		t.Fatalf("the frame must apply and stay structural so the hub refreshes: %+v ok=%v", applied, ok)
	}

	resolved := *open
	resolved.Status, resolved.Resolution, resolved.ResolutionNote = "resolved", "coordinated", settledNote
	s.Refresh(model.TeamState{
		Team:      model.Team{Slug: "demo", Sequence: 9, BoardRevision: 4},
		Members:   []*model.Member{{Key: "M-1", DisplayName: "Ana", Agents: []*model.Agent{{Key: "A-1", MemberKey: "M-1", Label: "claude"}}}},
		Sessions:  []*model.Session{{Key: "S-20", AgentKey: "A-1", MemberKey: "M-1", Status: model.SessionLive}},
		Conflicts: []*model.Conflict{&resolved},
	})

	snap := s.Snapshot()
	if len(snap.OpenConflicts()) != 0 || len(snap.ClosedConflicts()) != 1 || snap.ClosedConflicts()[0].ResolutionNote != settledNote {
		t.Fatalf("after refresh: open=%v settled=%v", snap.OpenConflicts(), snap.ClosedConflicts())
	}
	if l := laneFor(snap, "S-20"); l == nil || len(l.Conflicts) != 0 {
		t.Fatalf("after refresh: lane still carries the badge: %+v", l)
	}
}
