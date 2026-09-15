package state

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// The entanglement graph.
//
// The obvious wrong graph here is a DAG of steps. metiche is not about
// sequence — nothing in it happens "before" anything else in a way worth
// drawing. It is about *entanglement*: who is working on the same things as
// whom. So the graph is bipartite: live sessions down the left, the artifacts
// they touch down the middle, and an edge wherever a session claims or
// asserts one.
//
// An artifact touched by two sessions is the collision, and it is drawn as the
// product icon draws it — the two strokes are coral and indigo, and where they
// cross the node is violet. A contract with a consumer and no producer gets
// the cyan spark, because it is not a crossing at all: it is a stroke with
// nothing opposite, which is the failure this product exists to surface.
//
// Layout is computed here, deterministically, and emitted as inline SVG. No
// physics: a node that moves every repaint is unreadable, and the board
// repaints often. Positions depend only on the set of nodes and their sort
// order, so a node stays put across renders unless the structure changed.
//
// The same layout draws a past window (WindowGraph). There, everything a
// session held during the window is on the graph, and the crossing means
// what it means live — two sessions in one area AT THE SAME MOMENT — judged
// from when each hold began and ended. Two sessions that held one area at
// different times are a hand-off, drawn dashed and never as a crossing.

// Artifact kinds in the graph.
const (
	ArtifactPath     = "path"
	ArtifactContract = "contract"
	ArtifactDecision = "decision"
)

// GraphNode is one drawn node.
type GraphNode struct {
	ID    string
	Kind  string // session | path | contract | decision
	Label string
	Sub   string
	X, Y  float64
	R     float64
	// Side is which stroke of the mark this node belongs to: "A" (coral) or
	// "B" (indigo). Sessions get a side; artifacts only get one if exactly one
	// session touches them.
	Side string
	// Severity is set on contended artifacts, from the conflict they carry.
	Severity string
	// Alert marks the cyan state: consumed with no producer.
	Alert bool
	// Degree is how many sessions touch this artifact.
	Degree int
	// Handoff marks an artifact of a past window that more than one session
	// held, but never two at the same moment. Never set on a live graph.
	Handoff bool
	Href    string
	Title   string
}

// GraphEdge is one drawn edge.
type GraphEdge struct {
	From, To string
	X1, Y1   float64
	X2, Y2   float64
	Side     string
	Hot      bool
	// Alert edges run into a node with nobody opposite it. They are drawn
	// cyan so the eye follows the spark back to whoever is depending on it.
	Alert bool
	Title string
}

// Graph is a laid-out entanglement graph.
type Graph struct {
	Nodes         []GraphNode
	Edges         []GraphEdge
	Width, Height float64
	Contended     int
	Alerts        int
	// Handoffs counts the artifacts of a past window held by more than one
	// session, but never by two at once.
	Handoffs int
	// Calm is true when nothing is shared between two sessions. That is the
	// normal state and must read as "fine", not as "empty".
	Calm bool
}

// Node looks a node up by id.
func (g Graph) Node(id string) *GraphNode {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// pathArea collapses a claim's paths to the area they share, so a session
// holding nine files in one directory is one node and not nine. Two sessions
// only collide in this graph if they land in the same area, which is the same
// prefix logic the backend's detection uses.
func pathArea(p string) string {
	if i := strings.Index(p, "*"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSuffix(p, "/")
	parts := strings.Split(p, "/")
	if len(parts) <= 1 {
		if p == "" {
			return "/"
		}
		return p
	}
	// One directory deep is the useful granularity: "api/", "web/",
	// "deploy/chart". Deeper and every session gets its own node and nothing
	// ever appears to collide.
	if len(parts) == 2 {
		return parts[0] + "/"
	}
	return strings.Join(parts[:len(parts)-1], "/") + "/"
}

// ---------------------------------------------------------------- building

type graphSession struct {
	key, label, sub, title string
}

type graphArtifact struct {
	id, kind, label, sub string
	sessions             map[string]string // session -> role ("holder"/"challenger"/"produces"/"consumes"/"")
	order                []string
	// holds are each session's holds on this artifact, for a past window.
	holds map[string][]model.GraphHold
}

// overlapped reports whether two different sessions held this artifact at
// the same moment.
func (a *graphArtifact) overlapped() bool {
	for i, si := range a.order {
		for _, sj := range a.order[i+1:] {
			for _, hi := range a.holds[si] {
				for _, hj := range a.holds[sj] {
					if hi.Overlaps(hj) {
						return true
					}
				}
			}
		}
	}
	return false
}

// graphBuild is what the layout draws, from the live board or a past window.
type graphBuild struct {
	slug     string
	timed    bool // a past window: crossings are judged by time
	sessions []graphSession
	arts     map[string]*graphArtifact
	artOrder []string
	sevFor   map[string]string
	alertFor map[string]bool
}

func newGraphBuild(slug string, timed bool) *graphBuild {
	return &graphBuild{slug: slug, timed: timed, arts: map[string]*graphArtifact{},
		sevFor: map[string]string{}, alertFor: map[string]bool{}}
}

func (b *graphBuild) touch(id, kind, label, sub, session, role string) *graphArtifact {
	a, ok := b.arts[id]
	if !ok {
		a = &graphArtifact{id: id, kind: kind, label: label, sub: sub,
			sessions: map[string]string{}, holds: map[string][]model.GraphHold{}}
		b.arts[id] = a
		b.artOrder = append(b.artOrder, id)
	}
	if _, seen := a.sessions[session]; !seen {
		a.order = append(a.order, session)
	}
	a.sessions[session] = role
	return a
}

// mark attaches a conflict's severity (and the unclaimed spark) to the
// artifact it is about.
func (b *graphBuild) mark(id, severity string, alert bool) {
	if model.SeverityRank(severity) > model.SeverityRank(b.sevFor[id]) {
		b.sevFor[id] = severity
	}
	if alert {
		b.alertFor[id] = true
	}
}

// EntanglementGraph builds and lays out the graph.
func (s Snapshot) EntanglementGraph() Graph {
	b := newGraphBuild(s.Team.Slug, false)

	// Live sessions only: a graph of finished work is a history, not a board.
	live := []*model.Session{}
	for _, sess := range s.Sessions {
		if sess.Live() {
			live = append(live, sess)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Key < live[j].Key })
	isLive := map[string]bool{}
	for _, sess := range live {
		isLive[sess.Key] = true
		b.sessions = append(b.sessions, graphSession{key: sess.Key, label: s.SessionLabel(sess.Key),
			sub: sess.StatusLine, title: sess.Goal})
	}

	for _, c := range s.Claims {
		if !c.Active(s.Now) || !isLive[c.SessionKey] {
			continue
		}
		for _, p := range c.Paths {
			area := pathArea(p)
			b.touch("path:"+area, ArtifactPath, area, c.Mode, c.SessionKey, c.Mode)
		}
	}
	for _, c := range s.Contracts {
		for _, a := range c.Assertions {
			if a.Status != "active" || !isLive[a.SessionKey] {
				continue
			}
			b.touch("contract:"+c.Key, ArtifactContract, c.Key, c.Kind, a.SessionKey, a.Role)
		}
	}
	for _, d := range s.Decisions {
		if d.Status != "active" {
			continue
		}
		// A decision is only worth drawing when a live session is arguing with
		// it; an uncontested decision is a page, not a graph node.
		for _, c := range s.Conflicts {
			if !c.Open() || c.DecisionKey != d.Key {
				continue
			}
			for _, p := range c.Participants {
				if isLive[p.SessionKey] {
					b.touch("decision:"+d.Key, ArtifactDecision, d.Key, d.Title, p.SessionKey, "contradicts")
				}
			}
		}
	}

	// Conflict severity attaches to the artifact it is about.
	for _, c := range s.Conflicts {
		if !c.Open() {
			continue
		}
		alert := c.Kind == model.KindContractUnclaimed
		if c.ContractKey != "" {
			b.mark("contract:"+c.ContractKey, c.Severity, alert)
		}
		if c.DecisionKey != "" {
			b.mark("decision:"+c.DecisionKey, c.Severity, alert)
		}
		for _, p := range c.Paths {
			b.mark("path:"+pathArea(p), c.Severity, alert)
		}
	}
	return b.layout()
}

// WindowGraph builds the graph of a past window: every session active in it,
// every area it held a path in during it, a crossing only where two sessions'
// holds overlapped in time, and the window's conflicts on the areas they
// were about. A hold on a session the window does not list is left out rather
// than drawn without its session.
func WindowGraph(slug string, w model.GraphWindow) Graph {
	b := newGraphBuild(slug, true)

	runs := append([]model.RunSummary(nil), w.Sessions...)
	sort.Slice(runs, func(i, j int) bool { return runs[i].Key < runs[j].Key })
	inWindow := map[string]bool{}
	for _, run := range runs {
		inWindow[run.Key] = true
		label := run.Agent()
		if label == "" {
			label = run.Key
		}
		sub := run.Key + " · " + run.Status
		if run.Outcome != "" {
			sub += " · " + run.Outcome
		}
		b.sessions = append(b.sessions, graphSession{key: run.Key, label: label, sub: sub, title: run.Goal})
	}
	for _, h := range w.Holds {
		if !inWindow[h.SessionKey] {
			continue
		}
		area := pathArea(h.Path)
		a := b.touch("path:"+area, ArtifactPath, area, h.Mode, h.SessionKey, h.Mode)
		a.holds[h.SessionKey] = append(a.holds[h.SessionKey], h)
	}
	for _, c := range w.Conflicts {
		for _, p := range c.Paths {
			b.mark("path:"+pathArea(p), c.Severity, false)
		}
	}
	return b.layout()
}

// holdSpan is when a session held an area, for a node's tooltip.
func holdSpan(holds []model.GraphHold) string {
	spans := make([]string, 0, len(holds))
	for _, h := range holds {
		until := "still held"
		if !h.Until.IsZero() {
			until = h.Until.UTC().Format("Jan 2 15:04")
		}
		spans = append(spans, h.From.UTC().Format("Jan 2 15:04")+"–"+until)
	}
	return strings.Join(spans, ", ") + " UTC"
}

func (b *graphBuild) crossing(a *graphArtifact) bool {
	return len(a.sessions) > 1 && (!b.timed || a.overlapped())
}

// layout places the build's sessions and artifacts.
func (b *graphBuild) layout() Graph {
	// Keep artifacts that are contended, alarming, or at least attached to a
	// session. Uncontended path areas stay: they are the calm.
	kept := make([]*graphArtifact, 0, len(b.artOrder))
	for _, id := range b.artOrder {
		kept = append(kept, b.arts[id])
	}
	sort.Slice(kept, func(i, j int) bool {
		ai, aj := kept[i], kept[j]
		// Crossings first, then shared at different times, then alarming,
		// then alphabetical — a stable order is what keeps nodes from jumping
		// between repaints.
		if b.crossing(ai) != b.crossing(aj) {
			return b.crossing(ai)
		}
		if (len(ai.sessions) > 1) != (len(aj.sessions) > 1) {
			return len(ai.sessions) > 1
		}
		if model.SeverityRank(b.sevFor[ai.id]) != model.SeverityRank(b.sevFor[aj.id]) {
			return model.SeverityRank(b.sevFor[ai.id]) > model.SeverityRank(b.sevFor[aj.id])
		}
		return ai.id < aj.id
	})

	const (
		sessionX  = 318.0
		artifactX = 680.0
		rowH      = 76.0
		topPad    = 48.0
		width     = 1060.0
	)

	g := Graph{Width: width}
	sideOf := map[string]string{}
	sessionY := map[string]float64{}
	labelOf := map[string]string{}

	for i, sess := range b.sessions {
		// Alternating sides is the mark: the strokes come from opposite
		// directions. It also means any two adjacent sessions crossing the
		// same artifact are drawn in contrasting colours.
		side := "A"
		if i%2 == 1 {
			side = "B"
		}
		sideOf[sess.key] = side
		labelOf[sess.key] = sess.label
		y := topPad + float64(i)*rowH
		sessionY[sess.key] = y
		g.Nodes = append(g.Nodes, GraphNode{
			ID: "session:" + sess.key, Kind: "session", Side: side,
			Label: sess.label, Sub: sess.sub,
			X: sessionX, Y: y, R: 15,
			Href:  fmt.Sprintf("/t/%s/runs/%s", b.slug, sess.key),
			Title: sess.title,
		})
	}

	for i, a := range kept {
		y := topPad + float64(i)*rowH
		degree := len(a.sessions)
		crossing := b.crossing(a)
		node := GraphNode{
			ID: a.id, Kind: a.kind, Label: a.label, Sub: a.sub,
			X: artifactX, Y: y, Degree: degree,
			Severity: b.sevFor[a.id], Alert: b.alertFor[a.id],
			R: 13,
		}
		switch {
		case crossing:
			// The crossing. Heavier, and it carries the severity.
			node.R = 13 + float64(min(degree, 4))*3
			g.Contended++
		case degree > 1:
			node.R = 13 + float64(min(degree, 4))*2
			node.Handoff = true
			g.Handoffs++
		case len(a.order) == 1:
			node.Side = sideOf[a.order[0]]
		}
		if node.Alert {
			g.Alerts++
		}
		switch a.kind {
		case ArtifactContract:
			node.Href = fmt.Sprintf("/t/%s/contracts", b.slug)
		case ArtifactDecision:
			node.Href = fmt.Sprintf("/t/%s/decisions", b.slug)
		}
		node.Title = strings.Join(a.order, ", ")
		if b.timed {
			parts := make([]string, 0, len(a.order))
			for _, sessionKey := range a.order {
				parts = append(parts, sessionKey+" "+holdSpan(a.holds[sessionKey]))
			}
			node.Title = strings.Join(parts, "; ")
		}
		g.Nodes = append(g.Nodes, node)

		for _, sessionKey := range a.order {
			sy, ok := sessionY[sessionKey]
			if !ok {
				continue
			}
			title := labelOf[sessionKey] + " " + a.sessions[sessionKey] + " " + a.label
			if b.timed {
				title += ", " + holdSpan(a.holds[sessionKey])
			}
			g.Edges = append(g.Edges, GraphEdge{
				From: "session:" + sessionKey, To: a.id,
				X1: sessionX, Y1: sy, X2: artifactX, Y2: y,
				Side:  sideOf[sessionKey],
				Hot:   crossing,
				Alert: b.alertFor[a.id],
				Title: title,
			})
		}
	}

	rows := max(len(b.sessions), len(kept))
	g.Height = topPad + float64(max(rows, 1))*rowH - rowH/3
	g.Calm = g.Contended == 0 && g.Alerts == 0
	return g
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
