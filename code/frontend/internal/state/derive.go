package state

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// ---------------------------------------------------------------- lanes

// AgentLane is one session's column-within-a-column on the board: the agent,
// one session it is running, and everything hanging off that session. An agent
// with several live sessions gets one AgentLane per session.
type AgentLane struct {
	Agent     *model.Agent
	Session   *model.Session
	Intent    *model.Intent
	Claims    []*model.Claim
	Conflicts []*model.Conflict
	// HotPaths are paths this agent holds that appear in an open conflict.
	// Rendering them differently is the difference between "here are 9 paths"
	// and "this one is the problem".
	HotPaths map[string]string // path -> worst severity
	// Multi is true when this agent has more than one live lane — several
	// terminals of one client, each its own session. The view uses it to put
	// the session key on the label, or the lanes are indistinguishable.
	Multi bool
}

// Worst returns the highest severity among this lane's open conflicts.
func (l AgentLane) Worst() string {
	worst := ""
	for _, c := range l.Conflicts {
		if c.Open() && model.SeverityRank(c.Severity) > model.SeverityRank(worst) {
			worst = c.Severity
		}
	}
	return worst
}

// Idle reports an agent with nothing in flight, which is a normal state and
// must not look like a bug.
func (l AgentLane) Idle() bool { return l.Session == nil || !l.Session.Live() }

// Lane is one member and their agents.
type Lane struct {
	Member *model.Member
	Agents []AgentLane
}

// Worst returns the highest severity across the member's agents.
func (l Lane) Worst() string {
	worst := ""
	for _, a := range l.Agents {
		if model.SeverityRank(a.Worst()) > model.SeverityRank(worst) {
			worst = a.Worst()
		}
	}
	return worst
}

// Active counts the member's distinct agents that are actually working. An
// agent with two live sessions has two lanes but is one agent, so counting
// lanes would put "3/2" in the header.
func (l Lane) Active() int {
	seen := map[string]bool{}
	for _, a := range l.Agents {
		if !a.Idle() && a.Agent != nil {
			seen[a.Agent.Key] = true
		}
	}
	return len(seen)
}

// AgentCount counts the member's distinct agents. It is the header's
// denominator: an agent with two live sessions has two lanes but is one agent,
// so len(Agents) would read "1/3" for a member with two agents.
func (l Lane) AgentCount() int {
	seen := map[string]bool{}
	for _, a := range l.Agents {
		if a.Agent != nil {
			seen[a.Agent.Key] = true
		}
	}
	return len(seen)
}

// Lanes builds the board: a lane per member, an agent lane per live session
// (or one for an idle agent), and for each the session, intent, held paths and
// live conflicts.
func (s Snapshot) Lanes() []Lane {
	// Every live session gets its own lane: one agent can run several
	// terminals at once, and keeping only one would hide the others. An agent
	// with nothing live keeps its most recently started finished session, so
	// it still shows what it last did instead of going blank.
	liveByAgent := map[string][]*model.Session{}
	lastByAgent := map[string]*model.Session{}
	for _, sess := range s.Sessions {
		if sess.Live() {
			liveByAgent[sess.AgentKey] = append(liveByAgent[sess.AgentKey], sess)
			continue
		}
		if prev, ok := lastByAgent[sess.AgentKey]; !ok || sess.StartedAt.After(prev.StartedAt) {
			lastByAgent[sess.AgentKey] = sess
		}
	}
	for _, list := range liveByAgent {
		sort.SliceStable(list, func(i, j int) bool {
			if !list[i].StartedAt.Equal(list[j].StartedAt) {
				return list[i].StartedAt.Before(list[j].StartedAt)
			}
			return list[i].Key < list[j].Key
		})
	}

	intentBySession := map[string]*model.Intent{}
	for _, i := range s.Intents {
		prev, ok := intentBySession[i.SessionKey]
		if !ok || (i.Open() && !prev.Open()) || (i.Open() == prev.Open() && i.UpdatedAt.After(prev.UpdatedAt)) {
			intentBySession[i.SessionKey] = i
		}
	}

	claimsBySession := map[string][]*model.Claim{}
	for _, c := range s.Claims {
		if c.Active(s.Now) {
			claimsBySession[c.SessionKey] = append(claimsBySession[c.SessionKey], c)
		}
	}

	conflictsBySession := map[string][]*model.Conflict{}
	hot := map[string]map[string]string{}
	for _, c := range s.Conflicts {
		if !c.Open() {
			continue
		}
		for _, p := range c.Participants {
			conflictsBySession[p.SessionKey] = append(conflictsBySession[p.SessionKey], c)
			if len(c.Paths) == 0 {
				continue
			}
			if hot[p.SessionKey] == nil {
				hot[p.SessionKey] = map[string]string{}
			}
			for _, path := range c.Paths {
				if model.SeverityRank(c.Severity) > model.SeverityRank(hot[p.SessionKey][path]) {
					hot[p.SessionKey][path] = c.Severity
				}
			}
		}
	}

	lanes := make([]Lane, 0, len(s.Members))
	for _, m := range s.Members {
		lane := Lane{Member: m}
		for _, a := range m.Agents {
			sessions := liveByAgent[a.Key]
			if len(sessions) == 0 {
				// Idle agent: one lane, carrying its last finished session
				// (or none at all).
				sessions = []*model.Session{lastByAgent[a.Key]}
			}
			multi := len(liveByAgent[a.Key]) > 1
			for _, sess := range sessions {
				al := AgentLane{Agent: a, Multi: multi}
				if sess != nil {
					al.Session = sess
					al.Intent = intentBySession[sess.Key]
					al.Claims = claimsBySession[sess.Key]
					al.Conflicts = conflictsBySession[sess.Key]
					al.HotPaths = hot[sess.Key]
				}
				lane.Agents = append(lane.Agents, al)
			}
		}
		// Busy agents first: the lane's top line should be the live one.
		sort.SliceStable(lane.Agents, func(i, j int) bool {
			return !lane.Agents[i].Idle() && lane.Agents[j].Idle()
		})
		lanes = append(lanes, lane)
	}
	// Members with trouble sort left, then members with the most agents
	// working. A board read left-to-right should degrade in urgency.
	sort.SliceStable(lanes, func(i, j int) bool {
		if a, b := model.SeverityRank(lanes[i].Worst()), model.SeverityRank(lanes[j].Worst()); a != b {
			return a > b
		}
		if a, b := lanes[i].Active(), lanes[j].Active(); a != b {
			return a > b
		}
		return lanes[i].Member.Key < lanes[j].Member.Key
	})
	return lanes
}

// OpenConflicts returns the conflicts still wanting attention, worst first.
func (s Snapshot) OpenConflicts() []*model.Conflict {
	out := make([]*model.Conflict, 0, len(s.Conflicts))
	for _, c := range s.Conflicts {
		if c.Open() {
			out = append(out, c)
		}
	}
	return out
}

// ClosedConflicts returns the resolved/dismissed/expired ones, newest first.
func (s Snapshot) ClosedConflicts() []*model.Conflict {
	out := make([]*model.Conflict, 0, len(s.Conflicts))
	for _, c := range s.Conflicts {
		if !c.Open() {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResolvedAt.After(out[j].ResolvedAt) })
	return out
}

// SeverityCounts tallies open conflicts by severity for the header.
func (s Snapshot) SeverityCounts() map[string]int {
	out := map[string]int{}
	for _, c := range s.OpenConflicts() {
		out[c.Severity]++
	}
	return out
}

// WorstOpen returns the highest open severity, or "" when the team is calm.
func (s Snapshot) WorstOpen() string {
	worst := ""
	for _, c := range s.OpenConflicts() {
		if model.SeverityRank(c.Severity) > model.SeverityRank(worst) {
			worst = c.Severity
		}
	}
	return worst
}

// LiveSessions counts sessions currently working.
func (s Snapshot) LiveSessions() int {
	n := 0
	for _, sess := range s.Sessions {
		if sess.Live() {
			n++
		}
	}
	return n
}

// Empty reports a team with nobody working — a real state, not an error.
func (s Snapshot) Empty() bool { return s.LiveSessions() == 0 }

// Member looks a member up by key.
func (s Snapshot) Member(key string) *model.Member {
	for _, m := range s.Members {
		if m.Key == key {
			return m
		}
	}
	return nil
}

// MemberName resolves a member key to a display name, falling back to the key.
func (s Snapshot) MemberName(key string) string {
	if m := s.Member(key); m != nil && m.DisplayName != "" {
		return m.DisplayName
	}
	return key
}

// Session looks a session up by key.
func (s Snapshot) Session(key string) *model.Session {
	for _, sess := range s.Sessions {
		if sess.Key == key {
			return sess
		}
	}
	return nil
}

// Agent looks an agent up by key.
func (s Snapshot) Agent(key string) *model.Agent {
	for _, m := range s.Members {
		for _, a := range m.Agents {
			if a.Key == key {
				return a
			}
		}
	}
	return nil
}

// SessionLabel renders a session as "Member · agent-label", which is how a
// human refers to it out loud.
func (s Snapshot) SessionLabel(key string) string {
	sess := s.Session(key)
	if sess == nil {
		return key
	}
	name := s.MemberName(sess.MemberKey)
	if a := s.Agent(sess.AgentKey); a != nil && a.Label != "" {
		return name + " · " + a.Label
	}
	return name
}

// EventsForSession filters the log down to one run.
func (s Snapshot) EventsForSession(key string) []model.Event {
	out := make([]model.Event, 0, 32)
	for _, ev := range s.Events {
		if ev.SessionKey == key {
			out = append(out, ev)
		}
	}
	return out
}

// RecentEvents returns the last n events, oldest first.
func (s Snapshot) RecentEvents(n int) []model.Event {
	if len(s.Events) <= n {
		return s.Events
	}
	return s.Events[len(s.Events)-n:]
}

// Runs lists every session newest-first for the history page.
func (s Snapshot) Runs() []*model.Session {
	out := append([]*model.Session(nil), s.Sessions...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Live() != out[j].Live() {
			return out[i].Live()
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// RunCounts counts what this board holds for one session: its intents, the
// distinct paths its claims name, and the conflicts it is in.
func (s Snapshot) RunCounts(key string) (intents, paths, conflicts int) {
	for _, in := range s.Intents {
		if in.SessionKey == key {
			intents++
		}
	}
	seen := map[string]bool{}
	for _, c := range s.Claims {
		if c.SessionKey != key {
			continue
		}
		for _, p := range c.Paths {
			if !seen[p] {
				seen[p] = true
				paths++
			}
		}
	}
	return intents, paths, len(s.ConflictsForSession(key))
}

// ConflictsForSession returns every conflict a session participates in.
func (s Snapshot) ConflictsForSession(key string) []*model.Conflict {
	out := []*model.Conflict{}
	for _, c := range s.Conflicts {
		for _, p := range c.Participants {
			if p.SessionKey == key {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- contracts

// Cell states in the produces/consumes matrix.
const (
	CellEmpty    = ""
	CellProduces = "produces"
	CellConsumes = "consumes"
	CellBoth     = "both"
)

// FieldDiff is one concrete disagreement between a producer and a consumer.
// The four kinds are the four anti-join branches: a required out field the
// producer omits, a required in field the consumer omits, a type disagreement,
// and a naming variant.
type FieldDiff struct {
	Kind     string // missing_out | missing_in | type | naming
	Field    string
	Expected string
	Actual   string
	Severity string
	Note     string
}

// MatrixCell is one (contract, session) intersection.
type MatrixCell struct {
	State      string
	Mismatched bool
	Diffs      []FieldDiff
	ShapeHash  string
	FieldCount int
}

// ContractRow is one contract across every session that touches it.
type ContractRow struct {
	Contract  *model.Contract
	Cells     []MatrixCell // parallel to Matrix.Columns
	Producers []*model.Assertion
	Consumers []*model.Assertion
	// Status is the row verdict: unclaimed | mismatch | contested | converged |
	// unconsumed. This is the column a human actually scans.
	Status   string
	Severity string
	Headline string
	Diffs    []FieldDiff
}

// MatrixColumn is one session (an agent doing work), which is the honest unit:
// one human running two agents can genuinely be on both sides of a contract.
type MatrixColumn struct {
	SessionKey string
	MemberKey  string
	Member     string
	Agent      string
	Live       bool
}

// Matrix is the produces/consumes view. Rows are contracts, columns are the
// sessions asserting on them.
type Matrix struct {
	Columns []MatrixColumn
	Rows    []ContractRow
}

// Counts tallies rows by status for the page header.
func (m Matrix) Counts() map[string]int {
	out := map[string]int{}
	for _, r := range m.Rows {
		out[r.Status]++
	}
	return out
}

// ContractMatrix builds the produces/consumes matrix.
//
// The point of this view is not to list contracts — it is to make the
// bottleneck obvious. Two things are bottlenecks: a contract several people
// consume and nobody produces (the consumer is coding against an endpoint that
// does not exist, and will not find out until integration), and a contract
// whose producer and consumer disagree about the shape. Both get a row verdict
// and a severity, and both are sorted to the top.
func (s Snapshot) ContractMatrix() Matrix {
	var m Matrix

	// Columns: every session with an active assertion anywhere.
	seen := map[string]bool{}
	for _, c := range s.Contracts {
		for _, a := range c.Assertions {
			if a.Status != "active" || seen[a.SessionKey] {
				continue
			}
			seen[a.SessionKey] = true
			col := MatrixColumn{SessionKey: a.SessionKey, Member: a.SessionKey}
			if sess := s.Session(a.SessionKey); sess != nil {
				col.MemberKey = sess.MemberKey
				col.Member = s.MemberName(sess.MemberKey)
				col.Live = sess.Live()
				if ag := s.Agent(sess.AgentKey); ag != nil {
					col.Agent = ag.Label
				}
			}
			m.Columns = append(m.Columns, col)
		}
	}
	sort.Slice(m.Columns, func(i, j int) bool {
		if m.Columns[i].Member != m.Columns[j].Member {
			return m.Columns[i].Member < m.Columns[j].Member
		}
		return m.Columns[i].SessionKey < m.Columns[j].SessionKey
	})

	for _, c := range s.Contracts {
		row := ContractRow{Contract: c, Producers: c.Producers(), Consumers: c.Consumers()}

		bySession := map[string]*MatrixCell{}
		for i := range m.Columns {
			bySession[m.Columns[i].SessionKey] = &MatrixCell{}
		}
		for _, a := range c.Assertions {
			if a.Status != "active" {
				continue
			}
			cell := bySession[a.SessionKey]
			if cell == nil {
				continue
			}
			switch {
			case cell.State == CellEmpty:
				cell.State = a.Role
			case cell.State != a.Role:
				cell.State = CellBoth
			}
			cell.ShapeHash = a.ShapeHash
			cell.FieldCount += len(a.Fields)
		}

		// Diff every consumer against every producer.
		for _, cons := range row.Consumers {
			diffs := []FieldDiff{}
			for _, prod := range row.Producers {
				diffs = append(diffs, diffShapes(prod, cons)...)
			}
			if len(diffs) > 0 {
				if cell := bySession[cons.SessionKey]; cell != nil {
					cell.Mismatched = true
					cell.Diffs = diffs
				}
				row.Diffs = append(row.Diffs, diffs...)
			}
		}
		if len(row.Producers) > 1 {
			// Two producers is its own bottleneck: duplicate work on the
			// highest-coupling surface in the repo.
			for _, p := range row.Producers {
				if cell := bySession[p.SessionKey]; cell != nil {
					cell.Mismatched = true
				}
			}
		}

		row.Cells = make([]MatrixCell, len(m.Columns))
		for i, col := range m.Columns {
			row.Cells[i] = *bySession[col.SessionKey]
		}

		row.Status, row.Severity, row.Headline = verdict(row, s)
		m.Rows = append(m.Rows, row)
	}

	order := map[string]int{"unclaimed": 0, "mismatch": 1, "contested": 2, "unconsumed": 3, "converged": 4}
	sort.SliceStable(m.Rows, func(i, j int) bool {
		a, b := m.Rows[i], m.Rows[j]
		if order[a.Status] != order[b.Status] {
			return order[a.Status] < order[b.Status]
		}
		return a.Contract.Key < b.Contract.Key
	})
	return m
}

func verdict(row ContractRow, s Snapshot) (status, severity, headline string) {
	switch {
	case len(row.Consumers) > 0 && len(row.Producers) == 0:
		names := make([]string, 0, len(row.Consumers))
		for _, c := range row.Consumers {
			names = append(names, s.SessionLabel(c.SessionKey))
		}
		return "unclaimed", "critical",
			fmt.Sprintf("%s is coding against this — nobody is building it", strings.Join(names, ", "))
	case len(row.Diffs) > 0:
		sev := "low"
		for _, d := range row.Diffs {
			if model.SeverityRank(d.Severity) > model.SeverityRank(sev) {
				sev = d.Severity
			}
		}
		return "mismatch", sev, fmt.Sprintf("%d field disagreement(s) between producer and consumer", len(row.Diffs))
	case row.Contract.Agreement == "mismatch":
		// The backend said so, and on the live path it is the only one that
		// can: the read API returns each assertion's shape hash but not its
		// fields, so there are no diffs here to count. Reporting "converged"
		// because we cannot see the fields would be the board lying about the
		// one view it exists for.
		return "mismatch", "high", "producer and consumer canonicalize to different shapes"
	case len(row.Producers) > 1:
		return "contested", "high", fmt.Sprintf("%d sessions claim to produce this", len(row.Producers))
	case len(row.Producers) > 0 && len(row.Consumers) == 0:
		return "unconsumed", "", "produced, nobody consuming it yet"
	default:
		return "converged", "", "producer and consumer agree"
	}
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32)
		case r == '-' || r == ' ' || r == '.':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// diffShapes runs the four directional branches over one producer/consumer pair.
func diffShapes(prod, cons *model.Assertion) []FieldDiff {
	out := []FieldDiff{}

	index := func(a *model.Assertion) (byName map[string]model.Field, bySnake map[string]model.Field) {
		byName, bySnake = map[string]model.Field{}, map[string]model.Field{}
		for _, f := range a.Fields {
			byName[f.Direction+":"+f.Name] = f
			bySnake[f.Direction+":"+snake(f.Name)] = f
		}
		return
	}
	pByName, pBySnake := index(prod)
	cByName, cBySnake := index(cons)

	// 1. Required OUT fields the consumer needs and the producer omits.
	//    Breaks the consumer.
	for _, f := range cons.Fields {
		if f.Direction != "out" || !f.Required {
			continue
		}
		if _, ok := pByName["out:"+f.Name]; ok {
			continue
		}
		if alt, ok := pBySnake["out:"+snake(f.Name)]; ok {
			out = append(out, FieldDiff{Kind: "naming", Field: f.Name, Expected: f.Name, Actual: alt.Name,
				Severity: "low", Note: "same field, different spelling — usually a serializer setting"})
			continue
		}
		out = append(out, FieldDiff{Kind: "missing_out", Field: f.Name, Expected: f.Type, Actual: "absent",
			Severity: "high", Note: "consumer reads it, producer does not return it"})
	}

	// 2. Required IN fields the producer needs and the consumer omits.
	//    Breaks the producer.
	for _, f := range prod.Fields {
		if f.Direction != "in" || !f.Required {
			continue
		}
		if _, ok := cByName["in:"+f.Name]; ok {
			continue
		}
		if alt, ok := cBySnake["in:"+snake(f.Name)]; ok {
			out = append(out, FieldDiff{Kind: "naming", Field: f.Name, Expected: f.Name, Actual: alt.Name,
				Severity: "low", Note: "same field, different spelling"})
			continue
		}
		out = append(out, FieldDiff{Kind: "missing_in", Field: f.Name, Expected: f.Type, Actual: "absent",
			Severity: "high", Note: "producer requires it, consumer does not send it"})
	}

	// 3. Type disagreements on fields both sides name.
	for _, f := range cons.Fields {
		if pf, ok := pByName[f.Direction+":"+f.Name]; ok && pf.Type != f.Type {
			out = append(out, FieldDiff{Kind: "type", Field: f.Name, Expected: pf.Type, Actual: f.Type,
				Severity: "high", Note: "producer and consumer disagree on the type"})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return model.SeverityRank(out[i].Severity) > model.SeverityRank(out[j].Severity)
	})
	return out
}

// Ago renders a short relative time, the only time format a board needs.
func Ago(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
