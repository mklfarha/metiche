package webapi

import (
	"database/sql"
	"regexp"
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

// sessionCore is a session as every endpoint names it. The snapshot and the
// session page add what it holds right now (sessionWire); the history list
// adds counts over its whole life instead (sessionSummaryWire).
type sessionCore struct {
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
	// OutcomeNote is the agent's own line on how the run ended.
	OutcomeNote string `json:"outcome_note,omitempty"`

	CurrentIntentKey string `json:"current_intent_key,omitempty"`

	// ParentSessionKey is the session that supervises this one (a subagent's
	// supervisor), by key. Absent for a session nobody delegated, and absent
	// again if the supervisor's row is ever deleted (ON DELETE SET NULL).
	ParentSessionKey string `json:"parent_session_key,omitempty"`
}

type sessionWire struct {
	sessionCore

	Intents []intentWire `json:"intents"`
	Claims  []claimWire  `json:"claims"`
}

// sessionSummaryWire is one row of the run history list.
type sessionSummaryWire struct {
	sessionCore

	Counts sessionCountsWire `json:"counts"`
}

// sessionCountsWire counts over a session's whole life, whatever its status.
type sessionCountsWire struct {
	Intents      int64 `json:"intents"`
	ClaimedPaths int64 `json:"claimed_paths"` // distinct normalized patterns
	Conflicts    int64 `json:"conflicts"`     // distinct conflicts it was a participant in
	Subagents    int64 `json:"subagents"`     // sessions that name this one as their parent
}

// runHistoryWire is what one session declared, claimed and collided on.
type runHistoryWire struct {
	Intents   []runIntentWire `json:"intents"`
	Claims    []runClaimWire  `json:"claims"`
	Conflicts []conflictWire  `json:"conflicts"`
}

type runIntentWire struct {
	intentWire

	StartedAt *string `json:"started_at,omitempty"`
	EndedAt   *string `json:"ended_at,omitempty"`
}

type runClaimWire struct {
	claimWire

	Status     string  `json:"status"`
	ClaimedAt  *string `json:"claimed_at,omitempty"`
	ReleasedAt *string `json:"released_at,omitempty"`
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

	// Paths are the path the two sides overlapped on and the two patterns
	// they claimed, from the detector's evidence. Empty for a conflict that is
	// not about paths.
	Paths []string `json:"paths"`

	// Plans, Signals and IssueRef are set only on duplicate_work
	// (docs/DUPLICATES.md §5.1, §9.3), each read from the detector's evidence
	// by name: the two plans that look like the same work, [incumbent, yield
	// side]; the human text of why the server paired them, in the evidence's
	// order; and the external_ref both plans name, when that is a reason.
	Plans    []conflictPlanWire `json:"plans,omitempty"`
	Signals  []string           `json:"signals,omitempty"`
	IssueRef string             `json:"issue_ref,omitempty"`

	// ContractKey is the contract a contract_* conflict is about.
	ContractKey string `json:"contract_key,omitempty"`

	// DecisionKey and JudgeNote are a decision_contradiction's own two facts
	// (docs/DECISIONS.md §9.4), both read from the detector's evidence by
	// name: the decision the plan contradicts, and the one line the plan's
	// own model wrote when it judged the pair. A duplicate_work carries
	// JudgeNote too: the yield side's model's line. EscalatedAt is any kind's: it
	// is set when metiche gave up waiting for the agents and asked a person,
	// and cleared again when a conflict it closed reopens.
	DecisionKey string  `json:"decision_key,omitempty"`
	JudgeNote   string  `json:"judge_note,omitempty"`
	EscalatedAt *string `json:"escalated_at,omitempty"`

	Participants []participantWire `json:"participants"`
}

// conflictPlanWire is one side of a duplicate_work conflict
// (docs/DUPLICATES.md §9.3): the plan, whose it is, what it says, the path it
// holds, and whether it is the side asked to stop.
type conflictPlanWire struct {
	Key     string `json:"key"`
	Who     string `json:"who"`
	Summary string `json:"summary"`
	Path    string `json:"path,omitempty"`
	Yields  bool   `json:"yields"`
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
	// Fields is the canonical shape the server stored, when it has one.
	Fields []contractFieldWire `json:"fields,omitempty"`
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

	// Issues are the field-level disagreements behind a mismatch (and any
	// naming variants), computed by the server from the stored shapes.
	Issues []contractIssueWire `json:"issues,omitempty"`

	Produces []assertionWire `json:"produces"`
	Consumes []assertionWire `json:"consumes"`
}

// contractFieldWire is one canonical field of an assertion's shape.
type contractFieldWire struct {
	Path      string `json:"path"`
	Type      string `json:"type"`
	Direction string `json:"direction"`
	Required  bool   `json:"required"`
	Nullable  bool   `json:"nullable"`
}

// contractIssueWire is one field-level disagreement between a producing and a
// consuming session, as coordination.CompareShapes found it.
type contractIssueWire struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Expected  string `json:"expected,omitempty"`
	Actual    string `json:"actual,omitempty"`
	Direction string `json:"direction"`
	Severity  string `json:"severity"`
	Note      string `json:"note,omitempty"`
	Producer  string `json:"producer"`
	Consumer  string `json:"consumer"`
}

// decisionWire is a recorded decision as the board draws it
// (docs/DECISIONS.md §9.2). GET /decisions returns accepted ones; the history
// endpoint returns the superseded and revoked ones, which are the only rows
// that carry EndedAt.
//
// Rationale is the one free-text field a person wrote for the board rather
// than for another agent. It is clipped and sanitized where it is read, never
// here: a wire type that could hold an unbounded, unmasked note is a type that
// eventually serves one.
type decisionWire struct {
	Key           string             `json:"key"`
	Title         string             `json:"title"`
	Statement     string             `json:"statement"`
	Status        string             `json:"status"`
	AlwaysShow    bool               `json:"always_show"`
	Revision      int64              `json:"revision"`
	DecidedBy     string             `json:"decided_by,omitempty"`
	DecidedAt     *string            `json:"decided_at,omitempty"`
	UpdatedAt     *string            `json:"updated_at,omitempty"`
	EndedAt       *string            `json:"ended_at,omitempty"` // history only
	Scope         []string           `json:"scope"`
	ProjectKey    string             `json:"project_key,omitempty"` // empty = team-wide
	Rationale     string             `json:"rationale,omitempty"`   // clipped 600
	Supersedes    string             `json:"supersedes,omitempty"`
	SupersededBy  string             `json:"superseded_by,omitempty"`
	Judged        decisionJudgedWire `json:"judged"`
	OpenConflicts []string           `json:"open_conflicts"`
}

// decisionJudgedWire counts the judgements on the decision's CURRENT
// revision: what the plan owners' own models said when they checked their
// plans against this wording. A count that included older revisions would
// report agreement with a sentence nobody is being asked about any more.
type decisionJudgedWire struct {
	NoConflict int64 `json:"no_conflict"`
	Conflict   int64 `json:"conflict"`
	Unsure     int64 `json:"unsure"`
	Pending    int64 `json:"pending"`
}

// decisionRevisionWire is one earlier wording of a decision, read from the
// event log: there is no decision_revision table (§10, decision 5), so the
// decision row holds the current wording and the older ones live in
// decision_recorded events until event retention ages them out.
type decisionRevisionWire struct {
	Sequence   int64   `json:"sequence"`
	Kind       string  `json:"kind"`
	Summary    string  `json:"summary"`
	Statement  string  `json:"statement,omitempty"` // payload.message
	OccurredAt *string `json:"occurred_at,omitempty"`
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

// boardSecretPatterns are the shapes of credential that could reach a
// free-text column an agent wrote — a decision's rationale, an earlier
// wording in the event log. app/mcp masks them on the way in; this masks them
// again on the way out, because the board is the public surface and a column
// written by an older binary was never masked at all.
var boardSecretPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`mtk_[A-Za-z0-9_\-]+`), "[redacted]"},
	{regexp.MustCompile(`mbs_[A-Za-z0-9_\-]+`), "[redacted]"},
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=\-]+`), "[redacted]"},
	{regexp.MustCompile(`://[^\s/@:]+:[^\s/@]+@`), "://[redacted]@"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pswd|secret|token|api[_-]?key)\b\s*[:=]\s*\S+`), "$1=[redacted]"},
}

// sanitizeBoardText collapses whitespace and masks credential-shaped text. It
// mirrors app/mcp's sanitizeNoteText rather than calling it: that one is
// unexported, and duplicating five regexps is cheaper than exporting a
// sanitizer from the tool surface for a read endpoint to reach into.
func sanitizeBoardText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for _, p := range boardSecretPatterns {
		s = p.re.ReplaceAllString(s, p.with)
	}
	return s
}

// clipText shortens to max runes, marking the cut with an ellipsis. Runes,
// not bytes: cutting a multi-byte character in half would serve invalid UTF-8.
func clipText(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	if max <= 1 {
		return string(r[:max])
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

// boardText is what every agent-written note goes through before it is
// served: masked, then clipped to what the card has room for.
func boardText(s string, max int) string { return clipText(sanitizeBoardText(s), max) }

// placeholders builds "?, ?, ?" for an IN list of n bound parameters.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
