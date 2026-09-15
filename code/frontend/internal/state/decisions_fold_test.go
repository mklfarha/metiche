package state

import (
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// TestDecisionSupersededFoldsSupersededAndRevoked: decision_superseded with
// new_status superseded or revoked ends a decision on a folded board, which
// moves it from the decisions in force to the Past section; any other
// new_status, and the never-emitted decision_updated, change nothing.
func TestDecisionSupersededFoldsSupersededAndRevoked(t *testing.T) {
	s := New("test", "Test")
	s.clock = func() time.Time { return time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC) }
	s.Apply(ev(1, "decision_recorded", true, `{"decision_key":"#auth-bearer-header","title":"Bearer header","statement":"Clients send Authorization: Bearer.","scope":"internal/auth/**","decided_by":"Bob","project_key":"shop"}`))
	s.Apply(ev(2, "decision_recorded", true, `{"decision_key":"#auth-jwt-cookie","title":"JWT cookie","statement":"Sessions are a JWT in an httpOnly cookie.","always_show":true,"decided_by":"Ana"}`))
	s.Apply(ev(3, "decision_recorded", true, `{"decision_key":"#ids-are-ulid","title":"ULIDs","statement":"Ids are ULIDs.","decided_by":"Ana"}`))
	s.Apply(ev(4, "decision_recorded", true, `{"decision_key":"#errors-rfc7807","title":"Problem details","statement":"Errors are RFC 7807 documents.","decided_by":"Bob"}`))

	if got := len(s.Snapshot().AcceptedDecisions()); got != 4 {
		t.Fatalf("accepted before any end = %d, want 4", got)
	}

	s.Apply(ev(5, "decision_superseded", true, `{"decision_key":"#auth-bearer-header","previous_status":"accepted","new_status":"superseded","superseded_by":"#auth-jwt-cookie"}`))
	s.Apply(ev(6, "decision_superseded", true, `{"decision_key":"#ids-are-ulid","previous_status":"accepted","new_status":"revoked","message":"test: ids are opaque now"}`))
	// Neither of these ends anything.
	s.Apply(ev(7, "decision_superseded", true, `{"decision_key":"#errors-rfc7807","previous_status":"revoked","new_status":"accepted"}`))
	s.Apply(ev(8, "decision_updated", true, `{"decision_key":"#errors-rfc7807","status":"revoked"}`))

	snap := s.Snapshot()
	byKey := map[string]*model.Decision{}
	for _, d := range snap.Decisions {
		byKey[d.Key] = d
	}
	if d := byKey["#auth-bearer-header"]; d.Status != "superseded" || d.EndedAt.IsZero() || d.SupersededBy != "#auth-jwt-cookie" {
		t.Errorf("superseded fold: %+v", d)
	}
	if d := byKey["#ids-are-ulid"]; d.Status != "revoked" || d.EndedAt.IsZero() {
		t.Errorf("revoked fold: %+v", d)
	}
	if d := byKey["#errors-rfc7807"]; d.Status != "active" || !d.EndedAt.IsZero() {
		t.Errorf("new_status accepted or a decision_updated ended a decision: %+v", d)
	}

	accepted, past := snap.AcceptedDecisions(), snap.PastDecisions()
	if len(accepted) != 2 || accepted[0].Key != "#auth-jwt-cookie" {
		t.Fatalf("accepted = %v, want the always-show #auth-jwt-cookie first of 2", keysOf(accepted))
	}
	if len(past) != 2 || past[0].Key != "#ids-are-ulid" || past[1].Key != "#auth-bearer-header" {
		t.Fatalf("past = %v, want newest end first: #ids-are-ulid, #auth-bearer-header", keysOf(past))
	}
	t.Logf("accepted %v; past %v", keysOf(accepted), keysOf(past))
}

// TestDecisionRecordedWithSupersedesEndsTheOldOne: a recording that names
// supersedes on the new decision ends the old one too, and recording the same
// key with new wording is a revision.
func TestDecisionRecordedWithSupersedesEndsTheOldOne(t *testing.T) {
	s := New("test", "Test")
	s.Apply(ev(1, "decision_recorded", true, `{"decision_key":"#auth-bearer-header","statement":"Bearer header."}`))
	s.Apply(ev(2, "decision_recorded", true, `{"decision_key":"#auth-jwt-cookie","statement":"JWT cookie.","supersedes":"#auth-bearer-header"}`))
	s.Apply(ev(3, "decision_recorded", true, `{"decision_key":"#auth-jwt-cookie","statement":"JWT in an httpOnly, Secure cookie.","supersedes":"#auth-bearer-header"}`))

	snap := s.Snapshot()
	past := snap.PastDecisions()
	if len(past) != 1 || past[0].Key != "#auth-bearer-header" || past[0].SupersededBy != "#auth-jwt-cookie" {
		t.Fatalf("past = %+v", past)
	}
	acc := snap.AcceptedDecisions()
	if len(acc) != 1 || acc[0].Revision != 2 || acc[0].Supersedes != "#auth-bearer-header" {
		t.Fatalf("accepted = %+v", acc)
	}
}

// TestConflictEscalatedMarksOnlyDecisionConflictsAsked: a recording's
// conflict_escalated about a decision conflict means a person was asked; the
// older recordings use the same kind for a contract that sat too long.
func TestConflictEscalatedMarksOnlyDecisionConflictsAsked(t *testing.T) {
	s := New("test", "Test")
	s.Apply(ev(1, "conflict_raised", true, `{"conflict_key":"CF-1","kind":"decision_contradiction","decision_key":"#auth-jwt-cookie","judge_note":"test note","severity":"high"}`))
	s.Apply(ev(2, "conflict_raised", true, `{"conflict_key":"CF-2","kind":"contract_unclaimed","severity":"high"}`))
	s.Apply(ev(3, "conflict_escalated", true, `{"conflict_key":"CF-1","severity":"high"}`))
	s.Apply(ev(4, "conflict_escalated", true, `{"conflict_key":"CF-2","severity":"critical"}`))
	for _, c := range s.Snapshot().Conflicts {
		switch c.Key {
		case "CF-1":
			if c.EscalatedAt.IsZero() || c.JudgeNote != "test note" || c.DecisionKey != "#auth-jwt-cookie" {
				t.Errorf("CF-1 = %+v", c)
			}
		case "CF-2":
			if !c.EscalatedAt.IsZero() {
				t.Errorf("a contract conflict was marked as a person asked: %+v", c)
			}
		}
	}
}

func keysOf(ds []*model.Decision) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Key)
	}
	return out
}
