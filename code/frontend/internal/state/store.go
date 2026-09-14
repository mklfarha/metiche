// Package state folds an event stream into the board's view of a team.
//
// Everything the UI draws comes from here. The store is authoritative for the
// two cursors: it stamps every event with the sequence it applied at, and bumps
// board_revision only for structural events. That split is the entire realtime
// design — see hub.Hub for the other half.
package state

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// Snapshot is an immutable read of a team at one revision. Handlers render from
// a snapshot so a repaint can never see half of an applied event.
type Snapshot struct {
	Team      model.Team
	Project   model.Project
	Members   []*model.Member
	Sessions  []*model.Session
	Intents   []*model.Intent
	Claims    []*model.Claim
	Contracts []*model.Contract
	Decisions []*model.Decision
	Conflicts []*model.Conflict
	Events    []model.Event // oldest first
	Now       time.Time
}

// Store is the folded state of one team. Safe for concurrent use.
type Store struct {
	mu sync.RWMutex

	team      model.Team
	project   model.Project
	members   map[string]*model.Member
	sessions  map[string]*model.Session
	intents   map[string]*model.Intent
	claims    map[string]*model.Claim
	contracts map[string]*model.Contract
	decisions map[string]*model.Decision
	conflicts map[string]*model.Conflict

	events    []model.Event
	maxEvents int
	clock     func() time.Time

	// snapshotAuthoritative switches off event folding.
	//
	// A fixture recording carries the whole world in its payloads, so folding
	// the log IS the state. The live backend's frames do not: they carry a
	// kind, a sequence, a subject key and a summary, and deliberately nothing
	// else. Folding those would not merely be thin, it would be wrong —
	// mutate() would key a session off an absent payload field and invent an
	// empty-keyed row on the board.
	//
	// So when a Load has handed this store a real snapshot, events stop being
	// the source of entity state and become what they are on that path: the
	// timeline, and the signal to go and read the snapshot again.
	snapshotAuthoritative bool
}

// New returns an empty store for a team.
func New(slug, name string) *Store {
	return &Store{
		team: model.Team{Slug: slug, Name: name},
		// Sprint is the default because it is the register that is least wrong
		// when nobody has said: it neither invents urgency nor invents a
		// ceremony that may not exist.
		project:   model.Project{Key: slug, Cadence: model.CadenceSprint},
		members:   map[string]*model.Member{},
		sessions:  map[string]*model.Session{},
		intents:   map[string]*model.Intent{},
		claims:    map[string]*model.Claim{},
		contracts: map[string]*model.Contract{},
		decisions: map[string]*model.Decision{},
		conflicts: map[string]*model.Conflict{},
		maxEvents: 2000,
		clock:     time.Now,
	}
}

// Sequence returns the timeline cursor.
func (s *Store) Sequence() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.team.Sequence
}

// Apply folds one event in and returns it as stored, with Sequence and
// BoardRevision stamped. Events arriving at or below the current sequence are
// ignored and reported with ok=false: replay after a reconnect must be
// idempotent, and the cheapest place to guarantee that is here.
func (s *Store) Apply(ev model.Event) (model.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ev.Sequence == 0 {
		ev.Sequence = s.team.Sequence + 1
	}
	if ev.Sequence <= s.team.Sequence {
		return ev, false
	}
	s.team.Sequence = ev.Sequence
	if ev.Structural {
		s.team.BoardRevision++
	}
	ev.BoardRevision = s.team.BoardRevision

	if !s.snapshotAuthoritative {
		s.mutate(ev)
	}

	s.events = append(s.events, ev)
	if len(s.events) > s.maxEvents {
		s.events = s.events[len(s.events)-s.maxEvents:]
	}
	return ev, true
}

// EventsAfter returns stored events with a sequence strictly greater than
// after. This is what makes ?after=<sequence> reconnection exact.
func (s *Store) EventsAfter(after int64) []model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Event, 0, 16)
	for _, ev := range s.events {
		if ev.Sequence > after {
			out = append(out, ev)
		}
	}
	return out
}

// Load seeds the store from a backend snapshot and moves the cursors to it.
//
// This is the first half of the live path, and it is what makes the boundary
// exact: the snapshot describes the team as of its own sequence, the cursor is
// set to that sequence (or to ts.ResumeFrom, see below), and the stream is
// then opened with ?after=<that number>. Everything at or below it is already
// drawn and everything above it arrives on the stream — no gap to fill and no
// event applied twice, because Apply still rejects anything that is not
// strictly newer than the cursor.
//
// Loading a snapshot makes it authoritative for entity state from then on; see
// the field comment on snapshotAuthoritative for why that is not optional.
func (s *Store) Load(ts model.TeamState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshotAuthoritative = true
	s.replace(ts)

	cursor := ts.Team.Sequence
	if ts.ResumeFrom > 0 && ts.ResumeFrom < cursor {
		cursor = ts.ResumeFrom
	}
	s.team.Sequence = cursor
	s.team.BoardRevision = ts.Team.BoardRevision
}

// Refresh replaces entity state from a newer snapshot WITHOUT touching the
// cursors.
//
// It is called when a structural frame says the shape of the board changed.
// The cursors are deliberately left alone: the stream, not the snapshot, owns
// the timeline cursor, and a refresh that raised it would silently swallow the
// stream frames between the old cursor and whatever instant the refresh
// happened to read — they would be rejected by Apply as already-seen and never
// appear in the log.
func (s *Store) Refresh(ts model.TeamState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshotAuthoritative = true
	s.replace(ts)
	if ts.Team.BoardRevision > s.team.BoardRevision {
		s.team.BoardRevision = ts.Team.BoardRevision
	}
}

// replace swaps in the entity maps from a snapshot. The caller holds the lock.
//
// Wholesale replacement rather than a merge, on purpose: the snapshot is one
// coherent read of the whole team, and merging it into what was there before
// would keep exactly the rows the backend has stopped reporting — the session
// that ended, the claim that expired, the conflict somebody resolved.
func (s *Store) replace(ts model.TeamState) {
	if ts.Team.Slug != "" {
		s.team.Slug = ts.Team.Slug
	}
	if ts.Team.Name != "" {
		s.team.Name = ts.Team.Name
	}
	if ts.Project.Key != "" {
		s.project.Key = ts.Project.Key
	}
	if ts.Project.RepoURL != "" {
		s.project.RepoURL = ts.Project.RepoURL
	}
	if ts.Project.Cadence.Valid() {
		s.project.Cadence = ts.Project.Cadence
	}

	s.members = make(map[string]*model.Member, len(ts.Members))
	for _, m := range ts.Members {
		s.members[m.Key] = m
	}
	s.sessions = make(map[string]*model.Session, len(ts.Sessions))
	for _, v := range ts.Sessions {
		s.sessions[v.Key] = v
	}
	s.intents = make(map[string]*model.Intent, len(ts.Intents))
	for _, v := range ts.Intents {
		s.intents[v.Key] = v
	}
	s.claims = make(map[string]*model.Claim, len(ts.Claims))
	for _, v := range ts.Claims {
		s.claims[v.Key] = v
	}
	s.contracts = make(map[string]*model.Contract, len(ts.Contracts))
	for _, v := range ts.Contracts {
		s.contracts[v.Key] = v
	}
	s.decisions = make(map[string]*model.Decision, len(ts.Decisions))
	for _, v := range ts.Decisions {
		s.decisions[v.Key] = v
	}
	s.conflicts = make(map[string]*model.Conflict, len(ts.Conflicts))
	for _, v := range ts.Conflicts {
		s.conflicts[v.Key] = v
	}
}

// Snapshot copies the current state out under the read lock.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := Snapshot{Team: s.team, Project: s.project, Now: s.clock()}

	snap.Members = make([]*model.Member, 0, len(s.members))
	for _, m := range s.members {
		cp := *m
		cp.Agents = append([]*model.Agent(nil), m.Agents...)
		snap.Members = append(snap.Members, &cp)
	}
	sort.Slice(snap.Members, func(i, j int) bool { return snap.Members[i].Key < snap.Members[j].Key })

	for _, v := range s.sessions {
		cp := *v
		snap.Sessions = append(snap.Sessions, &cp)
	}
	sort.Slice(snap.Sessions, func(i, j int) bool { return snap.Sessions[i].Key < snap.Sessions[j].Key })

	for _, v := range s.intents {
		cp := *v
		snap.Intents = append(snap.Intents, &cp)
	}
	sort.Slice(snap.Intents, func(i, j int) bool { return snap.Intents[i].Key < snap.Intents[j].Key })

	for _, v := range s.claims {
		cp := *v
		snap.Claims = append(snap.Claims, &cp)
	}
	sort.Slice(snap.Claims, func(i, j int) bool { return snap.Claims[i].Key < snap.Claims[j].Key })

	for _, v := range s.contracts {
		cp := *v
		cp.Assertions = append([]*model.Assertion(nil), v.Assertions...)
		snap.Contracts = append(snap.Contracts, &cp)
	}
	sort.Slice(snap.Contracts, func(i, j int) bool { return snap.Contracts[i].Key < snap.Contracts[j].Key })

	for _, v := range s.decisions {
		cp := *v
		snap.Decisions = append(snap.Decisions, &cp)
	}
	sort.Slice(snap.Decisions, func(i, j int) bool { return snap.Decisions[i].Key < snap.Decisions[j].Key })

	for _, v := range s.conflicts {
		cp := *v
		snap.Conflicts = append(snap.Conflicts, &cp)
	}
	// Worst first, then newest: the board's top-left is the most expensive
	// pixel on the page and the critical row has to own it.
	sort.Slice(snap.Conflicts, func(i, j int) bool {
		a, b := snap.Conflicts[i], snap.Conflicts[j]
		if a.Open() != b.Open() {
			return a.Open()
		}
		if ra, rb := model.SeverityRank(a.Severity), model.SeverityRank(b.Severity); ra != rb {
			return ra > rb
		}
		return a.RaisedAt.After(b.RaisedAt)
	})

	snap.Events = append([]model.Event(nil), s.events...)
	return snap
}

// ---------------------------------------------------------------- folding

type payload struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	MemberKey   string   `json:"member_key"`
	DisplayName string   `json:"display_name"`
	Role        string   `json:"role"`
	AgentKey    string   `json:"agent_key"`
	Label       string   `json:"label"`
	ClientKind  string   `json:"client_kind"`
	Status      string   `json:"status"`
	SessionKey  string   `json:"session_key"`
	ParentKey   string   `json:"parent_session_key"`
	Branch      string   `json:"branch"`
	BaseCommit  string   `json:"base_commit"`
	Goal        string   `json:"goal"`
	StatusLine  string   `json:"status_line"`
	IntentKey   string   `json:"intent_key"`
	Summary     string   `json:"summary"`
	Kind        string   `json:"kind"`
	ExternalRef string   `json:"external_ref"`
	ClaimKey    string   `json:"claim_key"`
	Mode        string   `json:"mode"`
	Paths       []string `json:"paths"`
	TTLSeconds  int      `json:"ttl_seconds"`

	ContractKey string        `json:"contract_key"`
	Assertion   *assertionPL  `json:"assertion"`
	DecisionKey string        `json:"decision_key"`
	Title       string        `json:"title"`
	Statement   string        `json:"statement"`
	AlwaysShow  bool          `json:"always_show"`
	Scope       string        `json:"scope"`
	ProjectKey  string        `json:"project_key"`
	RepoURL     string        `json:"repo_url"`
	Cadence     string        `json:"cadence"`
	ConflictKey string        `json:"conflict_key"`
	Severity    string        `json:"severity"`
	Detail      string        `json:"detail"`
	Suggested   string        `json:"suggested_action"`
	Resolution  string        `json:"resolution"`
	Members     []participant `json:"participants"`

	// The backend's conflict_resolved payload: the explanation, the new
	// status, and (in Detail) the resolution. A recording may instead say
	// resolution_note.
	Message        string `json:"message"`
	NewStatus      string `json:"new_status"`
	ResolutionNote string `json:"resolution_note"`
}

type assertionPL struct {
	Key        string        `json:"key"`
	Role       string        `json:"role"`
	SessionKey string        `json:"session_key"`
	ShapeHash  string        `json:"shape_hash"`
	Fields     []model.Field `json:"fields"`
}

type participant struct {
	SessionKey string `json:"session_key"`
	MemberKey  string `json:"member_key"`
	Role       string `json:"role"`
	Detail     string `json:"detail"`
}

func (s *Store) mutate(ev model.Event) {
	var p payload
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &p)
	}
	at := ev.OccurredAt

	switch ev.Kind {
	case "team_created":
		if c := model.Cadence(p.Cadence); c.Valid() {
			s.project.Cadence = c
		}
		if p.Slug != "" {
			s.team.Slug = p.Slug
		}
		if p.Name != "" {
			s.team.Name = p.Name
		}

	case "project_registered":
		if p.ProjectKey != "" {
			s.project.Key = p.ProjectKey
		}
		s.project.RepoURL = p.RepoURL
		if c := model.Cadence(p.Cadence); c.Valid() {
			s.project.Cadence = c
		}

	case "project_cadence_changed":
		if c := model.Cadence(p.Cadence); c.Valid() {
			s.project.Cadence = c
		}

	case "member_joined":
		m := s.member(p.MemberKey)
		m.DisplayName = p.DisplayName
		m.Role = p.Role
		m.LastSeenAt = at

	case "agent_registered":
		m := s.member(p.MemberKey)
		for _, a := range m.Agents {
			if a.Key == p.AgentKey {
				a.Label, a.Status, a.LastSeenAt = p.Label, orDefault(p.Status, "active"), at
				return
			}
		}
		m.Agents = append(m.Agents, &model.Agent{
			Key: p.AgentKey, MemberKey: m.Key, Label: p.Label,
			ClientKind: p.ClientKind, Status: orDefault(p.Status, "active"), LastSeenAt: at,
		})

	case "agent_status_changed":
		if a := s.agent(p.AgentKey); a != nil {
			a.Status, a.LastSeenAt = p.Status, at
		}

	case "session_started":
		sess := s.session(p.SessionKey)
		sess.MemberKey, sess.AgentKey = p.MemberKey, p.AgentKey
		sess.Branch, sess.BaseCommit, sess.Goal = p.Branch, p.BaseCommit, p.Goal
		sess.ParentSessionKey = p.ParentKey
		sess.StatusLine = orDefault(p.StatusLine, "starting up")
		sess.Status = model.SessionLive
		sess.StartedAt, sess.LastHeartbeatAt = at, at

	case "status_line_updated":
		sess := s.session(p.SessionKey)
		sess.StatusLine, sess.LastHeartbeatAt = p.StatusLine, at
		sess.Status = model.SessionLive

	case "heartbeat":
		sess := s.session(p.SessionKey)
		sess.LastHeartbeatAt = at
		if sess.Status == model.SessionStale {
			sess.Status = model.SessionLive
		}

	case "session_stalled":
		s.session(p.SessionKey).Status = model.SessionStale

	case "session_ended":
		sess := s.session(p.SessionKey)
		sess.Status = orDefault(p.Status, model.SessionEnded)
		sess.EndedAt = at
		sess.StatusLine = orDefault(p.StatusLine, "finished")
		// A session that ends drops its claims; leaving them held is how a
		// board starts lying to people.
		for _, c := range s.claims {
			if c.SessionKey == sess.Key && c.Status == "active" {
				c.Status = "released"
			}
		}
		for _, i := range s.intents {
			if i.SessionKey == sess.Key && i.Open() {
				i.Status = "done"
				i.UpdatedAt = at
			}
		}

	case "intent_declared":
		i := s.intent(p.IntentKey)
		i.SessionKey, i.Summary, i.Kind = p.SessionKey, p.Summary, orDefault(p.Kind, "change")
		i.Status = orDefault(p.Status, "declared")
		i.ExternalRef, i.UpdatedAt = p.ExternalRef, at
		i.Revision++
		if p.StatusLine != "" {
			s.session(p.SessionKey).StatusLine = p.StatusLine
		}

	case "intent_updated", "intent_completed":
		i := s.intent(p.IntentKey)
		if p.Summary != "" {
			i.Summary = p.Summary
			i.Revision++
		}
		if p.Status != "" {
			i.Status = p.Status
		} else if ev.Kind == "intent_completed" {
			i.Status = "done"
		}
		i.UpdatedAt = at
		if p.StatusLine != "" && i.SessionKey != "" {
			s.session(i.SessionKey).StatusLine = p.StatusLine
		}

	case "claim_created":
		c := s.claim(p.ClaimKey)
		c.SessionKey, c.IntentKey = p.SessionKey, p.IntentKey
		c.Mode = orDefault(p.Mode, "write")
		c.Paths = p.Paths
		c.Status = "active"
		ttl := p.TTLSeconds
		if ttl == 0 {
			ttl = 1800
		}
		c.ExpiresAt = at.Add(time.Duration(ttl) * time.Second)

	case "claim_released":
		s.claim(p.ClaimKey).Status = "released"

	case "claim_expired":
		s.claim(p.ClaimKey).Status = "expired"

	case "contract_published":
		c := s.contract(p.ContractKey)
		if p.Kind != "" {
			c.Kind = p.Kind
		}
		c.UpdatedAt = at
		if p.Assertion == nil {
			return
		}
		a := p.Assertion
		// Re-publishing supersedes this session's previous assertion on the
		// same side. That is what makes a mismatch converge rather than
		// accumulate two contradictory rows forever.
		for _, existing := range c.Assertions {
			if existing.SessionKey == a.SessionKey && existing.Role == a.Role && existing.Status == "active" {
				existing.Status = "superseded"
			}
		}
		c.Assertions = append(c.Assertions, &model.Assertion{
			Key: a.Key, Role: a.Role, SessionKey: a.SessionKey,
			ShapeHash: a.ShapeHash, Status: "active", Fields: a.Fields, UpdatedAt: at,
		})

	case "contract_withdrawn":
		c := s.contract(p.ContractKey)
		for _, a := range c.Assertions {
			if a.SessionKey == p.SessionKey && a.Status == "active" {
				a.Status = "withdrawn"
			}
		}

	case "decision_recorded":
		d := s.decision(p.DecisionKey)
		d.Title, d.Statement = p.Title, p.Statement
		d.Status = orDefault(p.Status, "active")
		d.Scope, d.AlwaysShow, d.SessionKey, d.RecordedAt = p.Scope, p.AlwaysShow, p.SessionKey, at

	case "decision_updated":
		d := s.decision(p.DecisionKey)
		if p.Status != "" {
			d.Status = p.Status
		}
		if p.Statement != "" {
			d.Statement = p.Statement
		}

	case "conflict_raised":
		c := s.conflict(p.ConflictKey)
		c.Kind, c.Severity = p.Kind, orDefault(p.Severity, "medium")
		c.Status = orDefault(p.Status, "open")
		c.Detail, c.SuggestedAction = p.Detail, p.Suggested
		c.ContractKey, c.DecisionKey, c.Paths = p.ContractKey, p.DecisionKey, p.Paths
		c.Participants = toParticipants(p.Members)
		if c.RaisedAt.IsZero() {
			c.RaisedAt = at
		}
		c.Occurrences++

	case "conflict_escalated":
		c := s.conflict(p.ConflictKey)
		c.Severity = orDefault(p.Severity, c.Severity)
		if p.Suggested != "" {
			c.SuggestedAction = p.Suggested
		}
		c.Occurrences++

	case "conflict_acknowledged":
		s.conflict(p.ConflictKey).Status = "acknowledged"

	case "conflict_resolved":
		// The backend's own conflict_resolved payload names the resolution in
		// detail and the explanation in message; a recording names them
		// resolution and resolution_note. Either shape settles the card.
		c := s.conflict(p.ConflictKey)
		c.Status = orDefault(p.Status, orDefault(p.NewStatus, "resolved"))
		c.Resolution = orDefault(p.Resolution, orDefault(p.Detail, "resolved"))
		c.ResolutionNote = orDefault(p.ResolutionNote, p.Message)
		c.ResolvedAt = at

	case "judgement_reported", "note", "commit_pushed", "review_requested", "instruction_delivered":
		// Log-only. These are exactly the events that must NOT move
		// board_revision, and the fixture marks them non-structural.
	}
}

func toParticipants(in []participant) []model.Participant {
	out := make([]model.Participant, 0, len(in))
	for _, p := range in {
		out = append(out, model.Participant(p))
	}
	return out
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func (s *Store) member(key string) *model.Member {
	m, ok := s.members[key]
	if !ok {
		m = &model.Member{Key: key, DisplayName: key}
		s.members[key] = m
	}
	return m
}

func (s *Store) agent(key string) *model.Agent {
	for _, m := range s.members {
		for _, a := range m.Agents {
			if a.Key == key {
				return a
			}
		}
	}
	return nil
}

func (s *Store) session(key string) *model.Session {
	v, ok := s.sessions[key]
	if !ok {
		v = &model.Session{Key: key, Status: model.SessionLive}
		s.sessions[key] = v
	}
	return v
}

func (s *Store) intent(key string) *model.Intent {
	v, ok := s.intents[key]
	if !ok {
		v = &model.Intent{Key: key, Status: "declared"}
		s.intents[key] = v
	}
	return v
}

func (s *Store) claim(key string) *model.Claim {
	v, ok := s.claims[key]
	if !ok {
		v = &model.Claim{Key: key, Status: "active", Mode: "write"}
		s.claims[key] = v
	}
	return v
}

func (s *Store) contract(key string) *model.Contract {
	v, ok := s.contracts[key]
	if !ok {
		v = &model.Contract{Key: key, Kind: "http"}
		s.contracts[key] = v
	}
	return v
}

func (s *Store) decision(key string) *model.Decision {
	v, ok := s.decisions[key]
	if !ok {
		v = &model.Decision{Key: key, Status: "active"}
		s.decisions[key] = v
	}
	return v
}

func (s *Store) conflict(key string) *model.Conflict {
	v, ok := s.conflicts[key]
	if !ok {
		v = &model.Conflict{Key: key, Status: "open", Severity: "medium"}
		s.conflicts[key] = v
	}
	return v
}
