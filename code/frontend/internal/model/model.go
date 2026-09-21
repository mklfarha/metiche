// Package model holds the board's view of metiche's data model.
//
// These are deliberately *read* shapes: the frontend never writes to the
// backend's tables, it folds an event stream into this state and renders it.
// UUIDs are omitted entirely — the backend keeps them for joins, but every
// human- and agent-facing surface is keyed on the short keys (S-17, INT-83,
// CF-14, #auth-jwt-cookie) and so is this.
package model

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------- identity

// Team is the scope for everything else, and carries the two cursors the whole
// realtime design hangs off.
type Team struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Sequence advances on every event. It is the timeline cursor.
	Sequence int64 `json:"sequence"`
	// BoardRevision advances only on structural change. It is the re-layout
	// cursor: two events can share a board revision, and the board must not be
	// repainted for the ones that do not move it.
	BoardRevision int64 `json:"board_revision"`
}

// Cadence is how fast the team is moving. It is a property of the project,
// not of a conflict, and it changes two things about every finding the board
// shows: how long something is allowed to sit before it is alarming, and what
// register the suggested action is written in.
//
// It matters because the same sentence is right in one register and useless in
// another. "Worth assigning at standup" is sound advice on a steady project and
// actively harmful at a hackathon, where the next planning meeting is never.
type Cadence string

// The three cadences.
const (
	CadenceHackathon Cadence = "hackathon"
	CadenceSprint    Cadence = "sprint"
	CadenceSteady    Cadence = "steady"
)

// Valid reports whether c is a known cadence.
func (c Cadence) Valid() bool {
	switch c {
	case CadenceHackathon, CadenceSprint, CadenceSteady:
		return true
	}
	return false
}

// Label is the cadence as a person would say it.
func (c Cadence) Label() string {
	switch c {
	case CadenceHackathon:
		return "hackathon"
	case CadenceSteady:
		return "steady"
	}
	return "sprint"
}

// Cadences lists them fastest first, for the board's selector.
func Cadences() []Cadence { return []Cadence{CadenceHackathon, CadenceSprint, CadenceSteady} }

// Project is one repo. Claims are project-scoped, and so is cadence.
type Project struct {
	Key           string  `json:"key"`
	RepoURL       string  `json:"repo_url"`
	DefaultBranch string  `json:"default_branch"`
	Cadence       Cadence `json:"cadence"`
}

// Agent is one agent instance belonging to a member.
type Agent struct {
	Key        string    `json:"key"`
	MemberKey  string    `json:"member_key"`
	Label      string    `json:"label"`
	ClientKind string    `json:"client_kind"`
	Status     string    `json:"status"` // active | idle | disconnected
	LastSeenAt time.Time `json:"last_seen_at"`
}

// Member is a human. One human may run several agents; that is the normal case,
// not the exotic one.
type Member struct {
	Key         string    `json:"key"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	Agents      []*Agent  `json:"agents"`
}

// ---------------------------------------------------------------- work

// SessionStatus values.
const (
	SessionLive      = "live"
	SessionStale     = "stale"
	SessionEnded     = "ended"
	SessionAbandoned = "abandoned"
)

// Session is one bounded piece of work by one agent. It doubles as the run
// record: history is the event log filtered by session.
type Session struct {
	Key        string `json:"key"`
	MemberKey  string `json:"member_key"`
	AgentKey   string `json:"agent_key"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"base_commit"`
	Goal       string `json:"goal"`
	// StatusLine is the "right now" line — the single most-read string on the
	// board, so it is stored first-class rather than dug out of the last event.
	StatusLine      string    `json:"status_line"`
	Status          string    `json:"status"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
	// ParentSessionKey is the supervising session when this one is a
	// subagent's (S-41), or "" for a session nobody delegated.
	ParentSessionKey string `json:"parent_session_key"`
}

// Live reports whether the session is still working.
func (s *Session) Live() bool { return s.Status == SessionLive || s.Status == SessionStale }

// Intent is the semantic surface: what an agent is about to do.
type Intent struct {
	Key         string    `json:"key"`
	SessionKey  string    `json:"session_key"`
	Summary     string    `json:"summary"`
	Kind        string    `json:"kind"`
	Status      string    `json:"status"` // declared|active|done|abandoned|superseded
	ExternalRef string    `json:"external_ref"`
	Revision    int       `json:"revision"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Open reports whether this intent still describes work in flight.
func (i *Intent) Open() bool { return i.Status == "declared" || i.Status == "active" }

// Claim is the mechanical surface: the paths an agent is taking. Advisory and
// TTL'd — a claim never blocks anyone, it only makes the overlap visible.
type Claim struct {
	Key        string    `json:"key"`
	SessionKey string    `json:"session_key"`
	IntentKey  string    `json:"intent_key"`
	Mode       string    `json:"mode"` // write|read|structural
	Paths      []string  `json:"paths"`
	Status     string    `json:"status"` // active|released|expired
	ExpiresAt  time.Time `json:"expires_at"`
}

// Active reports whether the claim still counts for detection.
func (c *Claim) Active(now time.Time) bool {
	return c.Status == "active" && (c.ExpiresAt.IsZero() || c.ExpiresAt.After(now))
}

// ---------------------------------------------------------------- contracts

// Field is one field of a contract shape. Direction is load-bearing: a missing
// out field breaks the consumer, a missing in field breaks the producer.
type Field struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Direction string `json:"direction"` // in|out
	Required  bool   `json:"required"`
}

// Assertion is one side's claim about a contract: "I produce this" or "I consume
// this". One table with a role enum, so a mismatch is a self-join.
type Assertion struct {
	Key        string    `json:"key"`
	Role       string    `json:"role"` // produces|consumes
	SessionKey string    `json:"session_key"`
	ShapeHash  string    `json:"shape_hash"`
	Status     string    `json:"status"` // active|superseded|withdrawn
	Fields     []Field   `json:"fields"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Contract is an interface two sessions have to agree on, keyed the way humans
// say it: "POST /api/login".
type Contract struct {
	Key        string       `json:"key"`
	Kind       string       `json:"kind"` // http|event|module|cli
	Assertions []*Assertion `json:"assertions"`
	UpdatedAt  time.Time    `json:"updated_at"`
	// Agreement is the backend's own one-word verdict on this contract —
	// unclaimed | unconsumed | agreed | mismatch | empty — when the board is
	// reading from the live API. It is empty when the state was folded from an
	// event log, because there it is derived from the assertions instead.
	//
	// It exists because the read API returns each assertion's shape_hash and
	// field_count but not its fields: the server canonicalizes and compares
	// the shapes itself, and the board is told the answer rather than the
	// inputs. Without this the matrix would call a known mismatch "converged",
	// which is the board lying about the one view it exists for.
	Agreement string `json:"agreement,omitempty"`

	// Issues are the field-level disagreements the backend computed from the
	// stored shapes, and ServerVerdict says this contract came from the live
	// API, where those issues — not a second comparison here — are the truth.
	Issues        []ContractIssue `json:"issues,omitempty"`
	ServerVerdict bool            `json:"-"`
}

// ContractIssue is one disagreement between a producing and a consuming
// session, as the backend's comparison reported it.
type ContractIssue struct {
	Kind      string `json:"kind"` // missing_out | missing_in | type_mismatch | naming_variant
	Path      string `json:"path"`
	Expected  string `json:"expected"`
	Actual    string `json:"actual"`
	Direction string `json:"direction"`
	Severity  string `json:"severity"`
	Note      string `json:"note"`
	Producer  string `json:"producer"`
	Consumer  string `json:"consumer"`
}

// Producers returns the active produces-side assertions.
func (c *Contract) Producers() []*Assertion { return c.byRole("produces") }

// Consumers returns the active consumes-side assertions.
func (c *Contract) Consumers() []*Assertion { return c.byRole("consumes") }

func (c *Contract) byRole(role string) []*Assertion {
	out := make([]*Assertion, 0, len(c.Assertions))
	for _, a := range c.Assertions {
		if a.Role == role && a.Status == "active" {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionKey < out[j].SessionKey })
	return out
}

// ---------------------------------------------------------------- decisions

// Decision is a settled call the whole team is meant to code against.
type Decision struct {
	Key       string `json:"key"` // #auth-jwt-cookie
	Title     string `json:"title"`
	Statement string `json:"statement"`
	Status    string `json:"status"` // active|superseded|revoked
	// Scope is the governed paths joined with ", ", as the card has always
	// taken them; ScopeParts splits them back into chips.
	Scope      string `json:"scope"`
	AlwaysShow bool   `json:"always_show"`
	SessionKey string `json:"session_key"`
	// RecordedAt is when it was decided (the backend's decided_at).
	RecordedAt time.Time `json:"recorded_at"`

	// Revision is the wording's revision: recording the same key again with a
	// new statement or scope bumps it.
	Revision int64 `json:"revision"`
	// DecidedBy is the member who decided it, or who last changed it.
	DecidedBy string `json:"decided_by"`
	// ProjectKey is the project it governs; "" is team-wide.
	ProjectKey   string `json:"project_key"`
	Supersedes   string `json:"supersedes"`
	SupersededBy string `json:"superseded_by"`
	// Rationale is why, for people. Agents are never sent it inline.
	Rationale string         `json:"rationale"`
	Judged    DecisionJudged `json:"judged"`
	// OpenConflicts are the keys of the open conflicts on this decision.
	OpenConflicts []string  `json:"open_conflicts"`
	UpdatedAt     time.Time `json:"updated_at"`
	// EndedAt is when it was superseded or revoked; zero while in force.
	EndedAt time.Time `json:"ended_at"`
}

// DecisionJudged counts the verdicts on a decision's current revision: how
// many plans were checked against it, and how many still wait for a judge.
type DecisionJudged struct {
	NoConflict int64 `json:"no_conflict"`
	Conflict   int64 `json:"conflict"`
	Unsure     int64 `json:"unsure"`
	Pending    int64 `json:"pending"`
}

// Checked is how many plans a judge has answered for.
func (j DecisionJudged) Checked() int64 { return j.NoConflict + j.Conflict + j.Unsure }

// Active reports whether the decision is in force.
func (d *Decision) Active() bool { return d.Status == "active" }

// TeamWide reports whether the decision holds for every project on the team.
func (d *Decision) TeamWide() bool { return d.ProjectKey == "" }

// ScopeParts is the scope as separate patterns.
func (d *Decision) ScopeParts() []string {
	out := []string{}
	for _, p := range strings.Split(d.Scope, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- conflicts

// Conflict kinds.
const (
	KindPathOverlap           = "path_overlap"
	KindContractMismatch      = "contract_mismatch"
	KindContractUnclaimed     = "contract_unclaimed"
	KindContractNamingVariant = "contract_naming_variant"
	KindDecisionContradiction = "decision_contradiction"
	KindDuplicateWork         = "duplicate_work"
	KindStaleBase             = "stale_base"
)

// Severities, weakest first.
var severityRank = map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}

// SeverityRank orders severities so the UI can sort and compare without a
// stringly-typed switch at every call site.
func SeverityRank(s string) int { return severityRank[s] }

// Participant is one session caught up in a conflict. Conflicts are not
// two-sided: three agents in one directory is normal.
type Participant struct {
	SessionKey string `json:"session_key"`
	MemberKey  string `json:"member_key"`
	Role       string `json:"role"` // holder|challenger|producer|consumer
	Detail     string `json:"detail"`
	// MemberName and AgentLabel name the session's person and agent as the
	// backend reported them, for a session this board does not hold.
	MemberName string `json:"member_name,omitempty"`
	AgentLabel string `json:"agent_label,omitempty"`
}

// Conflict is a detected collision. It always carries a suggested action —
// "you and Alice both hold auth.go" is noise, the suggested action is the signal.
type Conflict struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	// SuggestedAction is mandatory by design. A conflict without one is noise
	// and the board renders it as a defect.
	SuggestedAction string        `json:"suggested_action"`
	Severity        string        `json:"severity"`
	Status          string        `json:"status"` // open|acknowledged|resolving|resolved|dismissed|expired
	Participants    []Participant `json:"participants"`
	ContractKey     string        `json:"contract_key"`
	DecisionKey     string        `json:"decision_key"`
	Paths           []string      `json:"paths"`
	RaisedAt        time.Time     `json:"raised_at"`
	ResolvedAt      time.Time     `json:"resolved_at"`
	Resolution      string        `json:"resolution"`
	// ResolutionNote is how it was settled, in a sentence the backend wrote
	// from what actually happened: who released what, when, and what the
	// agents said. Empty for a conflict that is still open.
	ResolutionNote string `json:"resolution_note"`
	Occurrences    int    `json:"occurrences"`
	// JudgeNote is what the plan's own agent said when it judged its plan:
	// its confidence and reason. decision_contradiction and duplicate_work.
	JudgeNote string `json:"judge_note"`
	// EscalatedAt is when metiche asked a person, because the agents did not
	// settle it in time. Zero when nobody was asked.
	EscalatedAt time.Time `json:"escalated_at"`
	// Plans, Signals and IssueRef are duplicate_work only
	// (docs/DUPLICATES.md §9.3). Plans are the two plans that look like the
	// same work, the incumbent first and the side asked to yield second, as
	// the backend sends them; Signals are why the server paired them, in its
	// own words ("shared words: login, screen"); IssueRef is the issue id
	// both plans name, when they share one.
	Plans    []ConflictPlan `json:"plans,omitempty"`
	Signals  []string       `json:"signals,omitempty"`
	IssueRef string         `json:"issue_ref,omitempty"`
}

// ConflictPlan is one side of a duplicate_work conflict: a plan, whose it is,
// what it says, and whether the server asked it to yield. The side asked to
// yield is the one declared (or reworded) later: ordering assigns
// responsibility, not permission.
type ConflictPlan struct {
	Key     string `json:"key"`
	Who     string `json:"who"`
	Summary string `json:"summary"`
	Path    string `json:"path,omitempty"`
	Yields  bool   `json:"yields"`
}

// Open reports whether the conflict still wants a human's attention.
func (c *Conflict) Open() bool {
	switch c.Status {
	case "open", "acknowledged", "resolving":
		return true
	}
	return false
}

// ---------------------------------------------------------------- events

// Event is one row of the append-only team log. The frontend treats it as both
// the thing it renders into the timeline and the thing it folds into state.
type Event struct {
	Sequence int64  `json:"sequence"`
	Kind     string `json:"kind"`
	// Summary is the human line. The backend writes it; the frontend does not
	// invent prose from payloads.
	Summary    string    `json:"summary"`
	SessionKey string    `json:"session_key"`
	MemberKey  string    `json:"member_key"`
	AgentKey   string    `json:"agent_key"`
	OccurredAt time.Time `json:"occurred_at"`
	// Structural is the board_revision trigger. Heartbeats, notes and commits
	// are false; anything that changes the shape of the board is true.
	Structural bool `json:"structural"`
	// BoardRevision is stamped by the store as it folds the event, so a
	// reconnecting client can tell which repaint it already has.
	BoardRevision int64           `json:"board_revision"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// Severity is a coarse display weight for timeline rows, derived from the kind
// so the log is skimmable without reading it.
func (e Event) Tone() string {
	switch {
	case strings.HasPrefix(e.Kind, "conflict_raised"):
		return "bad"
	case strings.HasPrefix(e.Kind, "conflict_resolved"):
		return "good"
	case e.Kind == "decision_recorded":
		return "note"
	case e.Kind == "session_ended" || e.Kind == "intent_completed":
		return "good"
	case !e.Structural:
		return "quiet"
	}
	return ""
}

// ---------------------------------------------------------------- snapshots

// TeamState is a whole team as the backend's read API describes it at one
// instant: the board's first paint, before a single stream frame arrives.
//
// It exists because a live feed cannot be replayed from nothing the way a
// fixture can. The backend's event frames are deliberately thin — a kind, a
// sequence, the short key of the subject and a one-line summary — so they tell
// a board WHAT changed but not enough to rebuild what the thing now is. The
// read API answers that in one coherent transaction, and this is its shape in
// the board's own vocabulary.
type TeamState struct {
	Team      Team
	Project   Project
	Members   []*Member
	Sessions  []*Session
	Intents   []*Intent
	Claims    []*Claim
	Contracts []*Contract
	Decisions []*Decision
	Conflicts []*Conflict

	// ResumeFrom is the sequence a stream should resume after in order to be
	// consistent with this state. It is normally Team.Sequence.
	//
	// A feed may set it LOWER to pull recent history into the timeline, and
	// that is safe for exactly one reason: state loaded from here is
	// authoritative, so the re-delivered events are appended to the log and
	// never folded back over entities they already produced. It must never be
	// set higher — that is the one direction that loses an event.
	ResumeFrom int64
}
