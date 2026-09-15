package model

import "time"

// The rest of a team's history, read on demand from the backend for one page
// and never folded into the board's live state: past conflicts, the event log
// read backwards, and a past window of the entanglement graph.

// ConflictQuery asks for one page of past conflicts.
type ConflictQuery struct {
	Status string // "" (every past status) | resolved | dismissed | expired
	Kind   string // "" | a conflict kind
	Cursor string // "" for the newest page, else ConflictPage.NextCursor
	Limit  int
}

// ConflictPage is one page of past conflicts, newest first.
type ConflictPage struct {
	Conflicts  []*Conflict
	NextCursor string
	// Statuses and Kinds are what the filters accept, as the backend says.
	Statuses []string
	Kinds    []string
}

// DecisionQuery asks for one page of past decisions, or for one decision.
type DecisionQuery struct {
	Status string // "" (superseded and revoked) | superseded | revoked
	Cursor string // "" for the newest page, else DecisionPage.NextCursor
	// Key, when set, asks for that one decision in any status, with its
	// revisions; Status and Cursor are then not sent.
	Key   string
	Limit int
}

// DecisionPage is one page of past decisions, newest first, or one decision
// and its revisions.
type DecisionPage struct {
	Decisions  []*Decision
	NextCursor string
	// Statuses is what the status filter accepts, as the backend says.
	Statuses []string
	// Revisions are the decision's recorded wordings, newest first; only
	// for a DecisionQuery with a Key.
	Revisions []DecisionRevision
}

// DecisionRevision is one event that recorded, revised or ended a decision.
type DecisionRevision struct {
	Sequence   int64
	Kind       string // decision_recorded | decision_superseded
	Summary    string
	Statement  string // the wording that event recorded, when it carried one
	OccurredAt time.Time
}

// HistoryEvent is one event of the log as the history returns it: no
// payload, and the session's member and agent by name.
type HistoryEvent struct {
	Sequence   int64
	Kind       string
	Structural bool
	SubjectKey string
	Summary    string
	SessionKey string
	MemberName string
	AgentLabel string
	OccurredAt time.Time
}

// Who is the event's actor as a person names it, or "".
func (e HistoryEvent) Who() string {
	switch {
	case e.MemberName == "":
		return e.AgentLabel
	case e.AgentLabel == "":
		return e.MemberName
	}
	return e.MemberName + " · " + e.AgentLabel
}

// Event is the history row in the live log's shape, so the same row styling
// (structural, tone) applies to both.
func (e HistoryEvent) Event() Event {
	return Event{Sequence: e.Sequence, Kind: e.Kind, Summary: e.Summary, SessionKey: e.SessionKey,
		Structural: e.Structural, OccurredAt: e.OccurredAt}
}

// EventQuery asks for one page of the log, newest first.
type EventQuery struct {
	Before  int64 // 0 for the newest page, else EventPage.NextBefore
	Kind    string
	Session string
	Limit   int
}

// EventPage is one page of the log, newest first.
type EventPage struct {
	Events     []HistoryEvent
	NextBefore int64 // 0 when there is nothing older
	Kinds      []string
}

// GraphHold is one path a session held during a graph window.
type GraphHold struct {
	SessionKey string
	ClaimKey   string
	Mode       string
	Path       string
	Status     string
	From       time.Time
	Until      time.Time // zero while still held
}

// Overlaps reports whether two holds were held at the same moment. A hold
// still held runs to the end of time.
func (h GraphHold) Overlaps(o GraphHold) bool {
	hEnd, oEnd := h.Until, o.Until
	if hEnd.IsZero() {
		hEnd = time.Unix(1<<62, 0)
	}
	if oEnd.IsZero() {
		oEnd = time.Unix(1<<62, 0)
	}
	return h.From.Before(oEnd) && o.From.Before(hEnd)
}

// GraphWindow is what was entangled over a past window.
type GraphWindow struct {
	Window    string // 24h | 7d
	From, To  time.Time
	Sessions  []RunSummary // most recently started first
	Holds     []GraphHold  // oldest first
	Conflicts []*Conflict
	Truncated bool
}
