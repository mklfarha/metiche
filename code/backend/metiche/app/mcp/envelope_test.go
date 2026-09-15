package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEnvelopeShape pins the wire contract from PLAN.md:
//
//	{"ok":true,"key":"...","sequence":417,"revision":88,
//	 "pending":{"instructions":0,"conflicts":0,"reviews":0},"note":"..."}
//
// Every field name here is one an agent's model reads on every single call, so
// a rename is a protocol break and belongs in a version bump, not a refactor.
func TestEnvelopeShape(t *testing.T) {
	raw, err := marshalEnvelope(Envelope{
		OK:       true,
		Key:      "S-17",
		Sequence: 417,
		Revision: 88,
		Note:     "heartbeat every ~60s",
		Pending:  Pending{},
	})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}

	got := string(raw)
	want := `{"ok":true,"key":"S-17","sequence":417,"revision":88,"pending":{"instructions":0,"conflicts":0,"reviews":0},"note":"heartbeat every ~60s"}`
	if got != want {
		t.Errorf("envelope bytes changed\n got: %s\nwant: %s", got, want)
	}
}

// TestEnvelopeAlwaysCarriesPending is the push mechanism's only guarantee.
//
// The counts must serialize even when they are zero. An omitted field reads to
// a model as "unknown", and "unknown" invites a needless get_instructions call
// on every turn — which is exactly the cost the count was introduced to avoid.
func TestEnvelopeAlwaysCarriesPending(t *testing.T) {
	raw, err := marshalEnvelope(Envelope{OK: true})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	pending, ok := decoded["pending"]
	if !ok {
		t.Fatalf("pending is absent from %s", raw)
	}
	for _, field := range []string{"instructions", "conflicts", "reviews"} {
		if !strings.Contains(string(pending), `"`+field+`"`) {
			t.Errorf("pending is missing %q: %s", field, pending)
		}
	}
}

// TestEnvelopeOmitsEmptyExtras keeps the steady-state response small. Conflicts
// and review are the exceptional case; paying for their keys on every call is
// how a tool response stops being read.
func TestEnvelopeOmitsEmptyExtras(t *testing.T) {
	raw, err := marshalEnvelope(Envelope{OK: true, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Checked at the top level: "conflicts" also names a field inside
	// "pending", which is always present by design.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"conflicts", "review", "key", "note"} {
		if _, present := top[field]; present {
			t.Errorf("empty %q should be omitted: %s", field, raw)
		}
	}
}

// TestEnvelopeNeverCarriesTheBoard is the noise rule as a test.
//
// "Never the board, never everybody's claims." If somebody adds a field here
// that grows with team size, this is where it should hurt.
func TestEnvelopeNeverCarriesTheBoard(t *testing.T) {
	raw, err := marshalEnvelope(Envelope{
		OK:       true,
		Sequence: 3,
		Conflicts: []ConflictNotice{{
			Key: "CF-1", Kind: "path_overlap", Severity: "high",
			With: "ana/backend", Paths: []string{"auth.go"},
			SuggestedAction: "consume her POST /api/login contract instead",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{`"sessions"`, `"claims"`, `"board"`, `"members"`, `"agents"`} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("envelope carries %s, which rides on every single call: %s", banned, raw)
		}
	}
	if len(raw) > 700 {
		t.Errorf("a one-conflict envelope is %d bytes; the budget is 700 tokens for the WHOLE response", len(raw))
	}
}

func TestPendingNote(t *testing.T) {
	cases := []struct {
		name    string
		pending Pending
		any     bool
		note    string
	}{
		{"quiet", Pending{}, false, ""},
		{"instructions", Pending{Instructions: 2}, true, "2 instruction(s) waiting — call get_instructions"},
		{"conflicts", Pending{Conflicts: 1}, true, "1 open conflict(s) involve you — call get_instructions"},
		{"both", Pending{Instructions: 1, Conflicts: 3}, true, "1 instruction(s) and 3 open conflict(s) involve you — call get_instructions"},
		{"reviews only", Pending{Reviews: 1}, true, "1 pair(s) to judge against your plan — call get_review_context, then report_judgement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pending.Any(); got != tc.any {
				t.Errorf("Any() = %v, want %v", got, tc.any)
			}
			if got := tc.pending.NoteForPending(); got != tc.note {
				t.Errorf("NoteForPending() = %q, want %q", got, tc.note)
			}
		})
	}
}

// TestWithPendingDoesNotOverwriteANote: a tool that has something specific to
// say keeps saying it. The generic nudge is a fallback, not an override.
func TestWithPendingDoesNotOverwriteANote(t *testing.T) {
	e := Envelope{OK: true, Note: "project created"}.withPending(Pending{Instructions: 4})
	if e.Note != "project created" {
		t.Errorf("note was overwritten: %q", e.Note)
	}
	if e.Pending.Instructions != 4 {
		t.Errorf("pending was not attached: %+v", e.Pending)
	}

	e2 := Envelope{OK: true}.withPending(Pending{Instructions: 4})
	if e2.Note == "" {
		t.Error("an empty note should pick up the pending nudge")
	}
}

// TestMarshalEnvelopeIsStable is the idempotency contract in miniature: the
// bytes handed to the caller and the bytes stored for a replay are produced by
// one function, once, so they cannot drift.
func TestMarshalEnvelopeIsStable(t *testing.T) {
	e := Envelope{OK: true, Key: "A-2", Sequence: 9, Revision: 4, Pending: Pending{Conflicts: 1}}
	a, err := marshalEnvelope(e)
	if err != nil {
		t.Fatal(err)
	}
	b, err := marshalEnvelope(e)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("same envelope marshalled to different bytes:\n%s\n%s", a, b)
	}
}

// TestTwoCursorsAreSeparate documents, executably, why there are two numbers.
func TestTwoCursorsAreSeparate(t *testing.T) {
	repaint := Envelope{OK: true, Sequence: 418, Revision: 88}
	relayout := Envelope{OK: true, Sequence: 419, Revision: 89}

	if repaint.Revision != 88 {
		t.Error("a non-structural change must leave board_revision alone")
	}
	if relayout.Sequence == repaint.Sequence {
		t.Error("every event advances sequence, structural or not")
	}
}
