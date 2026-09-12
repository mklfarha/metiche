package state

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

func ev(seq int64, kind string, structural bool, payload string) model.Event {
	e := model.Event{Sequence: seq, Kind: kind, Structural: structural, OccurredAt: time.Now()}
	if payload != "" {
		e.Payload = json.RawMessage(payload)
	}
	return e
}

// TestApplyRejectsAnythingNotStrictlyNewer is the last line of defence on the
// boundary between a snapshot and the stream that resumes from it: if the two
// ever overlap, the overlap costs nothing.
func TestApplyRejectsAnythingNotStrictlyNewer(t *testing.T) {
	s := New("demo", "Demo")
	s.Load(model.TeamState{Team: model.Team{Slug: "demo", Sequence: 8, BoardRevision: 3}})

	if got := s.Sequence(); got != 8 {
		t.Fatalf("cursor after Load = %d, want 8", got)
	}
	for _, seq := range []int64{1, 7, 8} {
		if _, ok := s.Apply(ev(seq, "note", false, "")); ok {
			t.Fatalf("sequence %d was applied; everything at or below the cursor must be rejected", seq)
		}
	}
	applied, ok := s.Apply(ev(9, "note", false, ""))
	if !ok || applied.Sequence != 9 {
		t.Fatalf("sequence 9 = %+v, ok=%v", applied, ok)
	}
	if _, ok := s.Apply(ev(9, "note", false, "")); ok {
		t.Fatal("the same event was applied twice")
	}
	if got := len(s.EventsAfter(8)); got != 1 {
		t.Fatalf("timeline holds %d events past the cursor, want 1", got)
	}
}

// TestLoadedStateIsNotOverwrittenByThinLiveFrames is why the store stops
// folding once it has been handed a snapshot. A live frame's payload does not
// carry the session's identity, and folding one would invent an empty-keyed
// row where a real session already is.
func TestLoadedStateIsNotOverwrittenByThinLiveFrames(t *testing.T) {
	s := New("demo", "Demo")
	s.Load(model.TeamState{
		Team:     model.Team{Slug: "demo", Sequence: 8},
		Sessions: []*model.Session{{Key: "S-17", Status: model.SessionLive, StatusLine: "writing the handler"}},
	})

	// Exactly the shape a backend frame has: a kind, and a payload with none
	// of the keys the fixture fold reads.
	if _, ok := s.Apply(ev(9, "session_started", true, `{"message":"started"}`)); !ok {
		t.Fatal("the event should still be applied to the timeline")
	}

	snap := s.Snapshot()
	if len(snap.Sessions) != 1 {
		t.Fatalf("sessions = %+v — folding a thin frame invented a row", snap.Sessions)
	}
	if snap.Sessions[0].Key != "S-17" || snap.Sessions[0].StatusLine != "writing the handler" {
		t.Fatalf("session = %+v", snap.Sessions[0])
	}
	if len(snap.Events) != 1 {
		t.Fatalf("the event must still reach the timeline: %+v", snap.Events)
	}
	if snap.Team.BoardRevision != 1 {
		t.Fatalf("board revision = %d, want 1 — a structural event still moves it", snap.Team.BoardRevision)
	}
}

// TestRefreshReplacesStateAndLeavesTheCursorAlone.
func TestRefreshReplacesStateAndLeavesTheCursorAlone(t *testing.T) {
	s := New("demo", "Demo")
	s.Load(model.TeamState{
		Team:     model.Team{Slug: "demo", Sequence: 8, BoardRevision: 3},
		Sessions: []*model.Session{{Key: "S-17", Status: model.SessionLive}},
		Claims:   []*model.Claim{{Key: "CL-1", SessionKey: "S-17", Status: "active"}},
	})
	s.Apply(ev(9, "note", false, ""))

	s.Refresh(model.TeamState{
		// A refresh reporting a much later sequence must not drag the cursor
		// with it, or the events in between would never reach the timeline.
		Team:     model.Team{Slug: "demo", Sequence: 40, BoardRevision: 5},
		Sessions: []*model.Session{{Key: "S-18", Status: model.SessionLive}},
	})

	if got := s.Sequence(); got != 9 {
		t.Fatalf("cursor = %d, want 9 — the stream owns it, not the refresh", got)
	}
	snap := s.Snapshot()
	if len(snap.Sessions) != 1 || snap.Sessions[0].Key != "S-18" {
		t.Fatalf("sessions = %+v — a refresh replaces rather than merges", snap.Sessions)
	}
	if len(snap.Claims) != 0 {
		t.Fatalf("claims = %+v — a released claim must disappear with the refresh", snap.Claims)
	}
	if len(snap.Events) != 1 {
		t.Fatalf("a refresh must not clear the timeline: %+v", snap.Events)
	}
}

// TestFoldingIsUntouchedWithoutASnapshot guards the deployed fixture path.
func TestFoldingIsUntouchedWithoutASnapshot(t *testing.T) {
	s := New("demo", "Demo")
	s.Apply(ev(1, "member_joined", true, `{"member_key":"M-1","display_name":"Mara"}`))
	s.Apply(ev(2, "agent_registered", true, `{"member_key":"M-1","agent_key":"A-1","label":"api"}`))
	s.Apply(ev(3, "session_started", true, `{"session_key":"S-17","member_key":"M-1","agent_key":"A-1","branch":"feat/x"}`))
	s.Apply(ev(4, "status_line_updated", false, `{"session_key":"S-17","status_line":"writing the handler"}`))

	snap := s.Snapshot()
	if len(snap.Members) != 1 || snap.Members[0].DisplayName != "Mara" {
		t.Fatalf("members = %+v", snap.Members)
	}
	if len(snap.Members[0].Agents) != 1 || snap.Members[0].Agents[0].Key != "A-1" {
		t.Fatalf("agents = %+v", snap.Members[0].Agents)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].StatusLine != "writing the handler" {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	// Three structural events, one not.
	if snap.Team.BoardRevision != 3 || snap.Team.Sequence != 4 {
		t.Fatalf("cursors = seq %d rev %d, want 4 / 3", snap.Team.Sequence, snap.Team.BoardRevision)
	}
}
