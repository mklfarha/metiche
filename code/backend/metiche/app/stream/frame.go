// Package stream is the server → browser half of metiche: an SSE endpoint per
// team, fed by an in-process fan-out on commit with a database tailer behind it
// as the cross-process backstop.
//
// The shape of the thing, top to bottom:
//
//	publish.Publisher   the write path says "sequence N landed for team T"
//	Hub                 wakes that team's tailer, hydrates, fans out, dedupes
//	Source              turns team_event rows into Frames (one implementation
//	                    over MySQL, faked in tests)
//	Server              GET /v1/teams/{slug}/stream?after=N
//
// Two cursors ride every frame and they are NOT interchangeable:
//
//   - sequence advances on every event. It is the repaint cursor and the
//     resume cursor: a client reconnects with ?after=<last sequence> and gets
//     precisely what it missed.
//   - board_revision advances only on a STRUCTURAL change — a session starting
//     or ending, a lane appearing. It is the re-layout cursor. A client that
//     treats a sequence bump as a re-layout will rebuild the whole board on
//     every status-line edit, which is the exact flicker the split exists to
//     prevent.
package stream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// Frame is one team event as a browser receives it.
//
// It is addressed the way the rest of metiche is addressed — short keys, never
// uuids. The board, the tool surface and the prose all speak in S-17 / INT-83 /
// CF-14, and a stream that spoke in uuids would be the one consumer that had to
// keep its own lookup table.
type Frame struct {
	// Sequence is the team-scoped monotonic cursor. It is also the SSE id, so a
	// browser's own Last-Event-ID reconnect hands back exactly the cursor this
	// protocol is built on.
	Sequence int64 `json:"sequence"`

	// BoardRevision is the team's re-layout cursor as observed when this frame
	// was read. It is the team's CURRENT value, not a historical one — the
	// schema keeps the counter on the team row, not on the event — so during a
	// replay of old events it reports where the board is now. Structural is the
	// per-event fact: this event is one of the ones that moved the counter.
	BoardRevision int64 `json:"board_revision"`
	Structural    bool  `json:"structural"`

	// Kind is the event name, e.g. "intent_declared". It is also the SSE event
	// name, so a client can attach a listener per kind instead of switching on
	// a field.
	Kind string `json:"kind"`

	OccurredAt time.Time `json:"occurred_at"`

	ProjectKey string `json:"project_key,omitempty"`
	SessionKey string `json:"session_key,omitempty"`
	MemberKey  string `json:"member_key,omitempty"`
	MemberName string `json:"member_name,omitempty"`
	AgentLabel string `json:"agent_label,omitempty"`

	// SubjectKind/SubjectKey name what the event is about: the intent, claim,
	// conflict, contract or decision that changed, by its short key.
	SubjectKind string `json:"subject_kind,omitempty"`
	SubjectKey  string `json:"subject_key,omitempty"`

	Summary string       `json:"summary,omitempty"`
	Payload FramePayload `json:"payload"`
}

// FramePayload is the stored event payload rendered for the wire: enums by
// name rather than by ordinal, and absent fields omitted.
//
// Enum ordinals are deliberately not exposed. They are an artifact of the
// generated code and they shift if the schema's enum is ever reordered; a
// client that switched on the number would break silently on a regeneration.
type FramePayload struct {
	Message        string   `json:"message,omitempty"`
	PreviousStatus string   `json:"previous_status,omitempty"`
	NewStatus      string   `json:"new_status,omitempty"`
	Severity       string   `json:"severity,omitempty"`
	Paths          []string `json:"paths,omitempty"`
	Detail         string   `json:"detail,omitempty"`
}

// framePayloadFrom converts the stored payload to its wire form. A payload that
// will not decode yields an empty one rather than dropping the frame: the kind
// and the sequence still tell the client what changed and keep its cursor
// moving, and a stalled cursor is far worse than a thin frame.
func framePayloadFrom(raw []byte) FramePayload {
	if len(raw) == 0 {
		return FramePayload{}
	}
	p := event_payload.EventPayloadFromJSON(raw)
	out := FramePayload{
		Message:        p.Message.String,
		PreviousStatus: p.PreviousStatus.String,
		NewStatus:      p.NewStatus.String,
		Paths:          p.Paths,
		Detail:         p.Detail.String,
	}
	if p.Severity != enums.CONFLICT_SEVERITY_INVALID {
		out.Severity = p.Severity.String()
	}
	return out
}

// writeFrame emits one SSE frame and flushes it.
//
// Flushing per frame is not an optimisation, it is the protocol: without it the
// response sits in Go's buffer and the stream looks dead until something else
// happens to fill it.
func writeFrame(w http.ResponseWriter, flusher http.Flusher, f Frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", f.Sequence, f.Kind, body); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
