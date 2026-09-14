package model

import "time"

// The run history: every session a team has run, as the backend's
// GET /v1/teams/{slug}/sessions lists it, and one session's whole story from
// GET /v1/teams/{slug}/sessions/{key}.
//
// These are separate from Session, Intent and Claim on purpose. Those are the
// board's live state, folded from a snapshot and a stream; these are history
// read on demand for one page and never folded into anything.

// RunSummary is one row of the run history.
type RunSummary struct {
	Key        string
	ProjectKey string
	MemberKey  string
	MemberName string
	AgentLabel string
	ClientKind string

	Branch     string
	Goal       string
	Status     string // live | stale | ended | abandoned
	StatusLine string

	StartedAt       time.Time
	LastHeartbeatAt time.Time
	EndedAt         time.Time
	Outcome         string // succeeded | failed | abandoned, or ""
	OutcomeNote     string

	// ParentSessionKey is the run that supervised this one, or "".
	ParentSessionKey string

	// Counts over the session's whole life.
	Intents      int
	ClaimedPaths int
	Conflicts    int
	Subagents    int // runs that name this one as their parent
}

// Live reports whether the run is still working.
func (r RunSummary) Live() bool { return r.Status == SessionLive || r.Status == SessionStale }

// Agent is the run as a person names it: "Member · agent-label".
func (r RunSummary) Agent() string {
	switch {
	case r.MemberName == "":
		return r.AgentLabel
	case r.AgentLabel == "":
		return r.MemberName
	}
	return r.MemberName + " · " + r.AgentLabel
}

// RunPage is one page of the run history, newest first.
type RunPage struct {
	Runs []RunSummary
	// NextCursor asks for the next, older page; "" when there is none.
	NextCursor string
}

// RunIntent is an intent a run declared, in whatever state it ended up.
type RunIntent struct {
	Key         string
	Summary     string
	Kind        string
	Status      string // declared | active | done | abandoned | superseded
	ExternalRef string
	Revision    int
	DeclaredAt  time.Time
	StartedAt   time.Time
	EndedAt     time.Time
}

// RunClaim is a claim a run took, with the paths it named.
type RunClaim struct {
	Key        string
	Mode       string // read | write | structural
	Status     string // held | released | expired | stale | revoked
	Paths      []string
	ClaimedAt  time.Time
	ExpiresAt  time.Time
	ReleasedAt time.Time
}

// RunDetail is one run's whole story.
type RunDetail struct {
	Run       RunSummary
	Intents   []RunIntent
	Claims    []RunClaim
	Conflicts []*Conflict
	Events    []Event // oldest first

	// Subagents are the runs this one supervised, oldest first.
	Subagents []RunSummary

	// MoreEvents is true when the run logged more events than one page shows.
	MoreEvents bool
}
