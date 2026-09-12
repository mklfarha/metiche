package webapi

import (
	"database/sql"
	"strings"
	"time"
)

// The wire shapes the board draws from.
//
// Two rules hold across all of them. Enums go out by NAME, never by ordinal:
// the numbers are an artifact of the generated code and shift if the schema's
// enum is ever reordered, and a client switching on the number would break
// silently on a regeneration. And uuids stay out: the board addresses a
// session as S-17 and a conflict as CF-14, the same way the tools and the
// prose do.

// cursors is embedded in every response. The two numbers mean different things
// and clients depend on the difference — see the package comment.
type cursors struct {
	Sequence      int64 `json:"sequence"`
	BoardRevision int64 `json:"board_revision"`
}

type intentWire struct {
	Key         string  `json:"key"`
	Summary     string  `json:"summary"`
	Kind        string  `json:"kind,omitempty"`
	Status      string  `json:"status"`
	ExternalRef string  `json:"external_ref,omitempty"`
	Revision    int64   `json:"revision"`
	DeclaredAt  *string `json:"declared_at,omitempty"`
}

type claimWire struct {
	Key       string   `json:"key"`
	Mode      string   `json:"mode"`
	ExpiresAt *string  `json:"expires_at,omitempty"`
	Paths     []string `json:"paths"`
}

type sessionWire struct {
	Key        string `json:"key"`
	ProjectKey string `json:"project_key,omitempty"`
	MemberKey  string `json:"member_key,omitempty"`
	MemberName string `json:"member_name,omitempty"`
	AgentLabel string `json:"agent_label,omitempty"`
	ClientKind string `json:"client_kind,omitempty"`

	Branch string `json:"branch,omitempty"`
	Goal   string `json:"goal,omitempty"`
	Status string `json:"status"`
	// StatusLine is the "what I'm doing right now" line the board shows under
	// the agent's name.
	StatusLine string `json:"status_line,omitempty"`

	StartedAt       *string `json:"started_at,omitempty"`
	LastHeartbeatAt *string `json:"last_heartbeat_at,omitempty"`
	EndedAt         *string `json:"ended_at,omitempty"`
	Outcome         string  `json:"outcome,omitempty"`

	CurrentIntentKey string `json:"current_intent_key,omitempty"`

	Intents []intentWire `json:"intents"`
	Claims  []claimWire  `json:"claims"`
}

type participantWire struct {
	SessionKey  string `json:"session_key,omitempty"`
	MemberName  string `json:"member_name,omitempty"`
	AgentLabel  string `json:"agent_label,omitempty"`
	Role        string `json:"role,omitempty"`
	SubjectKind string `json:"subject_kind,omitempty"`
}

type conflictWire struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Status   string `json:"status"`

	DetectedBy      string `json:"detected_by,omitempty"`
	DetectorRule    string `json:"detector_rule,omitempty"`
	SuggestedAction string `json:"suggested_action,omitempty"`

	OccurrenceCount int64   `json:"occurrence_count"`
	FirstDetectedAt *string `json:"first_detected_at,omitempty"`
	LastDetectedAt  *string `json:"last_detected_at,omitempty"`

	Resolution     string  `json:"resolution,omitempty"`
	ResolutionNote string  `json:"resolution_note,omitempty"`
	DismissReason  string  `json:"dismiss_reason,omitempty"`
	ResolvedAt     *string `json:"resolved_at,omitempty"`

	Participants []participantWire `json:"participants"`
}

type assertionWire struct {
	Role       string  `json:"role"`
	SessionKey string  `json:"session_key,omitempty"`
	MemberName string  `json:"member_name,omitempty"`
	AgentLabel string  `json:"agent_label,omitempty"`
	ShapeHash  string  `json:"shape_hash,omitempty"`
	Revision   int64   `json:"revision"`
	AssertedAt *string `json:"asserted_at,omitempty"`
	FieldCount int64   `json:"field_count"`
}

type contractWire struct {
	Key        string `json:"key"`
	ProjectKey string `json:"project_key,omitempty"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Title      string `json:"title,omitempty"`

	// Agreement is the produces/consumes matrix reduced to the one word the
	// board colours a cell with. It is derived here rather than at the client
	// so two views of the same contract can never disagree:
	//
	//	unclaimed — somebody is coding against this and nobody is building it.
	//	            In a hackathon this is the highest-value signal in the
	//	            system, so it gets its own word rather than hiding inside
	//	            "no producer".
	//	unconsumed — built, nobody consuming it yet.
	//	agreed    — every active assertion canonicalizes to the same shape.
	//	mismatch  — they do not.
	//	empty     — no active assertions at all.
	Agreement string `json:"agreement"`

	Produces []assertionWire `json:"produces"`
	Consumes []assertionWire `json:"consumes"`
}

type decisionWire struct {
	Key        string   `json:"key"`
	Title      string   `json:"title"`
	Statement  string   `json:"statement"`
	Status     string   `json:"status"`
	AlwaysShow bool     `json:"always_show"`
	Revision   int64    `json:"revision"`
	DecidedBy  string   `json:"decided_by,omitempty"`
	DecidedAt  *string  `json:"decided_at,omitempty"`
	Scope      []string `json:"scope"`
}

type eventWire struct {
	Sequence    int64   `json:"sequence"`
	Kind        string  `json:"kind"`
	Structural  bool    `json:"structural"`
	SubjectKind string  `json:"subject_kind,omitempty"`
	SubjectKey  string  `json:"subject_key,omitempty"`
	Summary     string  `json:"summary,omitempty"`
	OccurredAt  *string `json:"occurred_at,omitempty"`
}

// rfc3339 renders a nullable timestamp, or nothing at all.
//
// A zero time is rendered as absent rather than as 0001-01-01: an optional
// datetime in this schema means "has not happened yet", and a client should be
// able to tell "not ended" from "ended at the dawn of the common era".
func rfc3339(t sql.NullTime) *string {
	if !t.Valid || t.Time.IsZero() {
		return nil
	}
	s := t.Time.UTC().Format(time.RFC3339)
	return &s
}

// placeholders builds "?, ?, ?" for an IN list of n bound parameters.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
