package feed

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// The backend's wire shapes, and the one place they are turned into the
// board's own model.
//
// These mirror app/webapi/wire.go and app/stream/frame.go in the backend as
// they actually are, not as the frontend would have liked them. Two rules
// hold over there and both simplify things here: enums go out by NAME, in the
// same lowercase vocabulary the model already uses (live, stale, declared,
// path_overlap, high), and uuids never appear — everything is addressed by the
// short key a human would say out loud.
//
// # What the API does not carry, and what this does about it
//
// Nothing below invents a value the backend did not send. Where the board's
// model has a field the read API has no answer for, the field is left at its
// zero value and the gap is listed here rather than papered over:
//
//   - agent key. sessionWire carries member_key and agent_label but no agent
//     key, and the board's lanes are keyed by agent. agentKey() derives a
//     stable synthetic key from the two fields it does have. It is a local
//     identifier for grouping lanes and it is never sent anywhere.
//   - session.base_commit. Not exposed; the board shows it as a tooltip on the
//     branch and in the runs list, so those render empty.
//   - claim.intent_key. Not exposed. Nothing on the board reads it today.
//   - conflict.paths and conflict.detail. The conflict's evidence json is not
//     exposed, so a live board cannot highlight WHICH held path is contended.
//     The suggested action, which is the part that matters, is exposed.
//   - conflict participants' member_key. Only member_name comes back, so the
//     key is resolved through the participant's session where there is one.
//   - contract assertion fields. The API returns shape_hash and field_count
//     but not the fields, because the server canonicalizes and compares the
//     shapes itself. The board therefore takes the backend's own one-word
//     agreement verdict instead of re-deriving it from fields it does not
//     have — see model.Contract.Agreement.
//   - project cadence. Not modelled on the backend at all; the store keeps its
//     default.
//
// # Vocabulary the adapter reconciles
//
//   - decision status. The schema says proposed | accepted | superseded |
//     revoked; the board's model and templates say active for the settled one.
//     accepted maps to active here, which is the whole of the difference.
//   - assertion status. The contracts endpoint returns only active assertions,
//     so they are marked active rather than left blank.

// ---------------------------------------------------------------- snapshot

type cursorsWire struct {
	Sequence      int64 `json:"sequence"`
	BoardRevision int64 `json:"board_revision"`
}

type teamWire struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	Sequence      int64  `json:"sequence"`
	BoardRevision int64  `json:"board_revision"`
}

type intentJSON struct {
	Key         string  `json:"key"`
	Summary     string  `json:"summary"`
	Kind        string  `json:"kind"`
	Status      string  `json:"status"`
	ExternalRef string  `json:"external_ref"`
	Revision    int64   `json:"revision"`
	DeclaredAt  *string `json:"declared_at"`
}

type claimJSON struct {
	Key       string   `json:"key"`
	Mode      string   `json:"mode"`
	ExpiresAt *string  `json:"expires_at"`
	Paths     []string `json:"paths"`
}

type sessionJSON struct {
	Key        string `json:"key"`
	ProjectKey string `json:"project_key"`
	MemberKey  string `json:"member_key"`
	MemberName string `json:"member_name"`
	AgentLabel string `json:"agent_label"`
	ClientKind string `json:"client_kind"`

	Branch     string `json:"branch"`
	Goal       string `json:"goal"`
	Status     string `json:"status"`
	StatusLine string `json:"status_line"`

	StartedAt       *string `json:"started_at"`
	LastHeartbeatAt *string `json:"last_heartbeat_at"`
	EndedAt         *string `json:"ended_at"`
	Outcome         string  `json:"outcome"`

	CurrentIntentKey string `json:"current_intent_key"`
	ParentSessionKey string `json:"parent_session_key"`

	Intents []intentJSON `json:"intents"`
	Claims  []claimJSON  `json:"claims"`
}

type participantJSON struct {
	SessionKey  string `json:"session_key"`
	MemberName  string `json:"member_name"`
	AgentLabel  string `json:"agent_label"`
	Role        string `json:"role"`
	SubjectKind string `json:"subject_kind"`
}

type conflictJSON struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Status   string `json:"status"`

	DetectedBy      string `json:"detected_by"`
	DetectorRule    string `json:"detector_rule"`
	SuggestedAction string `json:"suggested_action"`

	OccurrenceCount int64   `json:"occurrence_count"`
	FirstDetectedAt *string `json:"first_detected_at"`
	LastDetectedAt  *string `json:"last_detected_at"`

	Resolution     string  `json:"resolution"`
	ResolutionNote string  `json:"resolution_note"`
	DismissReason  string  `json:"dismiss_reason"`
	ResolvedAt     *string `json:"resolved_at"`

	// Paths are the overlapping path and the two claimed patterns, from the
	// detector's evidence. Absent from a backend that predates them.
	Paths []string `json:"paths"`

	// ContractKey is the contract a contract_* conflict is about.
	ContractKey string `json:"contract_key"`

	Participants []participantJSON `json:"participants"`
}

type snapshotWire struct {
	cursorsWire
	Team      teamWire       `json:"team"`
	Sessions  []sessionJSON  `json:"sessions"`
	Conflicts []conflictJSON `json:"conflicts"`
}

// teamState turns the snapshot into the board's state.
//
// Members and agents are DERIVED here. The backend has both tables but the
// board's read endpoints report a team through its sessions, so the lanes are
// rebuilt from the member and agent that each session names. A member with no
// live or stale session has nothing on the board to draw, which is the same
// answer the backend gives by not returning them.
func (s snapshotWire) teamState(slug string) model.TeamState {
	ts := model.TeamState{
		Team: model.Team{
			Slug:          firstNonEmpty(s.Team.Key, slug),
			Name:          firstNonEmpty(s.Team.Name, s.Team.Key, slug),
			Sequence:      s.Sequence,
			BoardRevision: s.BoardRevision,
		},
		ResumeFrom: s.Sequence,
	}

	members := map[string]*model.Member{}
	agents := map[string]bool{}
	memberBySession := map[string]string{}

	for _, sw := range s.Sessions {
		memberKey := firstNonEmpty(sw.MemberKey, sw.MemberName)
		agentKey := agentKey(memberKey, sw.AgentLabel)
		memberBySession[sw.Key] = memberKey

		if sw.ProjectKey != "" && ts.Project.Key == "" {
			ts.Project.Key = sw.ProjectKey
		}

		seenAt := latest(parseTime(sw.LastHeartbeatAt), parseTime(sw.StartedAt))
		m, ok := members[memberKey]
		if !ok {
			m = &model.Member{Key: memberKey, DisplayName: firstNonEmpty(sw.MemberName, memberKey)}
			members[memberKey] = m
			ts.Members = append(ts.Members, m)
		}
		if seenAt.After(m.LastSeenAt) {
			m.LastSeenAt = seenAt
		}
		if !agents[agentKey] {
			agents[agentKey] = true
			m.Agents = append(m.Agents, &model.Agent{
				Key:        agentKey,
				MemberKey:  memberKey,
				Label:      sw.AgentLabel,
				ClientKind: sw.ClientKind,
				// The agent table has its own status; the read API reports the
				// session's, which is the one the board draws from anyway.
				Status:     agentStatus(sw.Status),
				LastSeenAt: seenAt,
			})
		}

		ts.Sessions = append(ts.Sessions, &model.Session{
			Key:              sw.Key,
			MemberKey:        memberKey,
			AgentKey:         agentKey,
			Branch:           sw.Branch,
			Goal:             sw.Goal,
			StatusLine:       sw.StatusLine,
			Status:           firstNonEmpty(sw.Status, model.SessionLive),
			StartedAt:        parseTime(sw.StartedAt),
			EndedAt:          parseTime(sw.EndedAt),
			LastHeartbeatAt:  parseTime(sw.LastHeartbeatAt),
			ParentSessionKey: sw.ParentSessionKey,
		})

		for _, iw := range sw.Intents {
			ts.Intents = append(ts.Intents, &model.Intent{
				Key:         iw.Key,
				SessionKey:  sw.Key,
				Summary:     iw.Summary,
				Kind:        iw.Kind,
				Status:      firstNonEmpty(iw.Status, "declared"),
				ExternalRef: iw.ExternalRef,
				Revision:    int(iw.Revision),
				UpdatedAt:   parseTime(iw.DeclaredAt),
			})
		}
		for _, cw := range sw.Claims {
			ts.Claims = append(ts.Claims, &model.Claim{
				Key:        cw.Key,
				SessionKey: sw.Key,
				Mode:       firstNonEmpty(cw.Mode, "write"),
				Paths:      cw.Paths,
				// The endpoint returns held claims whose expiry is still in
				// the future — the lazy filter IS the authority on the
				// backend — so everything that comes back is active.
				Status:    "active",
				ExpiresAt: parseTime(cw.ExpiresAt),
			})
		}
	}

	for _, cw := range s.Conflicts {
		ts.Conflicts = append(ts.Conflicts, cw.conflict(memberBySession))
	}
	return ts
}

func (c conflictJSON) conflict(memberBySession map[string]string) *model.Conflict {
	out := &model.Conflict{
		Key:             c.Key,
		Kind:            c.Kind,
		SuggestedAction: c.SuggestedAction,
		Severity:        firstNonEmpty(c.Severity, "medium"),
		Status:          firstNonEmpty(c.Status, "open"),
		RaisedAt:        parseTime(c.FirstDetectedAt),
		ResolvedAt:      parseTime(c.ResolvedAt),
		Resolution:      firstNonEmpty(c.Resolution, c.DismissReason),
		ResolutionNote:  c.ResolutionNote,
		Occurrences:     int(c.OccurrenceCount),
		Paths:           c.Paths,
		ContractKey:     c.ContractKey,
	}
	if out.RaisedAt.IsZero() {
		out.RaisedAt = parseTime(c.LastDetectedAt)
	}
	for _, p := range c.Participants {
		out.Participants = append(out.Participants, model.Participant{
			SessionKey: p.SessionKey,
			MemberKey:  memberBySession[p.SessionKey],
			Role:       p.Role,
			// subject_kind is what this participant is in the conflict by —
			// their claim, their contract assertion, their intent. It is the
			// only per-participant detail the API returns.
			Detail:     p.SubjectKind,
			MemberName: p.MemberName,
			AgentLabel: p.AgentLabel,
		})
	}
	return out
}

// ---------------------------------------------------------------- contracts

type assertionJSON struct {
	Role       string              `json:"role"`
	SessionKey string              `json:"session_key"`
	MemberName string              `json:"member_name"`
	AgentLabel string              `json:"agent_label"`
	ShapeHash  string              `json:"shape_hash"`
	Revision   int64               `json:"revision"`
	AssertedAt *string             `json:"asserted_at"`
	FieldCount int64               `json:"field_count"`
	Fields     []contractFieldJSON `json:"fields"`
}

type contractFieldJSON struct {
	Path      string `json:"path"`
	Type      string `json:"type"`
	Direction string `json:"direction"`
	Required  bool   `json:"required"`
}

type contractJSON struct {
	Key        string                `json:"key"`
	ProjectKey string                `json:"project_key"`
	Kind       string                `json:"kind"`
	Status     string                `json:"status"`
	Title      string                `json:"title"`
	Agreement  string                `json:"agreement"`
	Issues     []model.ContractIssue `json:"issues"`
	Produces   []assertionJSON       `json:"produces"`
	Consumes   []assertionJSON       `json:"consumes"`
}

type contractsWire struct {
	cursorsWire
	Team      teamWire       `json:"team"`
	Contracts []contractJSON `json:"contracts"`
}

func (c contractsWire) contracts() []*model.Contract {
	out := make([]*model.Contract, 0, len(c.Contracts))
	for _, cw := range c.Contracts {
		contract := &model.Contract{
			Key:       cw.Key,
			Kind:      firstNonEmpty(cw.Kind, "http"),
			Agreement: cw.Agreement,
			Issues:    cw.Issues,
			// The live API always sends its verdict; with it, the matrix shows
			// the backend's issues instead of comparing again.
			ServerVerdict: cw.Agreement != "",
		}
		for _, aw := range append(append([]assertionJSON{}, cw.Produces...), cw.Consumes...) {
			at := parseTime(aw.AssertedAt)
			if at.After(contract.UpdatedAt) {
				contract.UpdatedAt = at
			}
			contract.Assertions = append(contract.Assertions, &model.Assertion{
				// The API has no assertion key; this one is local, stable, and
				// exactly as unique as the row is — one active assertion per
				// (contract, session, role) is enforced on the backend.
				Key:        cw.Key + "#" + aw.Role + ":" + aw.SessionKey,
				Role:       aw.Role,
				SessionKey: aw.SessionKey,
				ShapeHash:  aw.ShapeHash,
				Status:     "active",
				Fields:     liveFields(aw.Fields),
				UpdatedAt:  at,
			})
		}
		out = append(out, contract)
	}
	return out
}

// liveFields maps the API's canonical fields onto the board's model.
func liveFields(in []contractFieldJSON) []model.Field {
	out := make([]model.Field, 0, len(in))
	for _, f := range in {
		out = append(out, model.Field{Name: f.Path, Type: f.Type, Direction: f.Direction, Required: f.Required})
	}
	return out
}

// ---------------------------------------------------------------- decisions

type decisionJSON struct {
	Key        string   `json:"key"`
	Title      string   `json:"title"`
	Statement  string   `json:"statement"`
	Status     string   `json:"status"`
	AlwaysShow bool     `json:"always_show"`
	Revision   int64    `json:"revision"`
	DecidedBy  string   `json:"decided_by"`
	DecidedAt  *string  `json:"decided_at"`
	Scope      []string `json:"scope"`
}

type decisionsWire struct {
	cursorsWire
	Team      teamWire       `json:"team"`
	Decisions []decisionJSON `json:"decisions"`
}

func (d decisionsWire) decisions() []*model.Decision {
	out := make([]*model.Decision, 0, len(d.Decisions))
	for _, dw := range d.Decisions {
		out = append(out, &model.Decision{
			Key:       dw.Key,
			Title:     dw.Title,
			Statement: dw.Statement,
			Status:    decisionStatus(dw.Status),
			// The scope patterns are normalized the same way claim paths are,
			// which is the point of showing them: it is what the detector
			// matches on rather than a paraphrase of it.
			Scope:      strings.Join(dw.Scope, ", "),
			AlwaysShow: dw.AlwaysShow,
			RecordedAt: parseTime(dw.DecidedAt),
		})
	}
	return out
}

// ---------------------------------------------------------------- frames

// frameWire is one SSE frame, as app/stream/frame.go writes it.
type frameWire struct {
	Sequence      int64     `json:"sequence"`
	BoardRevision int64     `json:"board_revision"`
	Structural    bool      `json:"structural"`
	Kind          string    `json:"kind"`
	OccurredAt    time.Time `json:"occurred_at"`

	ProjectKey string `json:"project_key"`
	SessionKey string `json:"session_key"`
	MemberKey  string `json:"member_key"`
	MemberName string `json:"member_name"`
	AgentLabel string `json:"agent_label"`

	SubjectKind string `json:"subject_kind"`
	SubjectKey  string `json:"subject_key"`

	Summary string          `json:"summary"`
	Payload json.RawMessage `json:"payload"`
}

// event turns a frame into the board's event.
//
// BoardRevision is deliberately NOT copied across. The frame reports the
// team's CURRENT revision, which during a replay of old events is the wrong
// number for the row being drawn; the store stamps the one it applied at,
// which is the right one. The frame's own per-event fact is Structural, and
// that is the one that is carried.
func (f frameWire) event() model.Event {
	return model.Event{
		Sequence:   f.Sequence,
		Kind:       f.Kind,
		Summary:    f.Summary,
		SessionKey: f.SessionKey,
		MemberKey:  f.MemberKey,
		AgentKey:   agentKey(f.MemberKey, f.AgentLabel),
		OccurredAt: f.OccurredAt,
		Structural: f.Structural,
		Payload:    f.Payload,
	}
}

// ---------------------------------------------------------------- helpers

// agentKey is the board's local identity for one agent.
//
// The read API names an agent by its label within a member and never by key,
// so this is derived rather than reported. Deriving it in one function is what
// keeps a session, its lane and its stream frames pointing at the same agent.
func agentKey(memberKey, label string) string {
	switch {
	case memberKey == "" && label == "":
		return ""
	case label == "":
		return memberKey
	case memberKey == "":
		return label
	}
	return memberKey + "/" + label
}

// agentStatus maps a session status onto the coarse agent status the board
// draws. An agent whose session has ended is not disconnected — it simply has
// nothing in flight — so it reads as idle.
func agentStatus(sessionStatus string) string {
	switch sessionStatus {
	case model.SessionLive:
		return "active"
	case model.SessionStale:
		return "idle"
	case "":
		return "active"
	}
	return "idle"
}

// decisionStatus reconciles the schema's vocabulary with the board's: the
// settled state is "accepted" on the backend and "active" here.
func decisionStatus(s string) string {
	if s == "accepted" || s == "" {
		return "active"
	}
	return s
}

func parseTime(s *string) time.Time {
	if s == nil || *s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
