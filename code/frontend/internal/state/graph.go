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
	Href   string
	Title  string
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

// EntanglementGraph builds and lays out the graph.
func (s Snapshot) EntanglementGraph() Graph {
	type artifact struct {
		id       string
		kind     string
		label    string
		sub      string
		sessions map[string]string // session -> role ("holder"/"challenger"/"produces"/"consumes"/"")
		order    []string
	}

	arts := map[string]*artifact{}
	artOrder := []string{}
	touch := func(id, kind, label, sub, session, role string) {
		a, ok := arts[id]
		if !ok {
			a = &artifact{id: id, kind: kind, label: label, sub: sub, sessions: map[string]string{}}
			arts[id] = a
			artOrder = append(artOrder, id)
		}
		if _, seen := a.sessions[session]; !seen {
			a.order = append(a.order, session)
		}
		a.sessions[session] = role
	}

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
	}

	for _, c := range s.Claims {
		if !c.Active(s.Now) || !isLive[c.SessionKey] {
			continue
		}
		for _, p := range c.Paths {
			area := pathArea(p)
			touch("path:"+area, ArtifactPath, area, c.Mode, c.SessionKey, c.Mode)
		}
	}
	for _, c := range s.Contracts {
		for _, a := range c.Assertions {
			if a.Status != "active" || !isLive[a.SessionKey] {
				continue
			}
			touch("contract:"+c.Key, ArtifactContract, c.Key, c.Kind, a.SessionKey, a.Role)
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
					touch("decision:"+d.Key, ArtifactDecision, d.Key, d.Title, p.SessionKey, "contradicts")
				}
			}
		}
	}

	// Conflict severity attaches to the artifact it is about.
	sevFor := map[string]string{}
	alertFor := map[string]bool{}
	for _, c := range s.Conflicts {
		if !c.Open() {
			continue
		}
		mark := func(id string) {
			if model.SeverityRank(c.Severity) > model.SeverityRank(sevFor[id]) {
				sevFor[id] = c.Severity
			}
			if c.Kind == model.KindContractUnclaimed {
				alertFor[id] = true
			}
		}
		if c.ContractKey != "" {
			mark("contract:" + c.ContractKey)
		}
		if c.DecisionKey != "" {
			mark("decision:" + c.DecisionKey)
		}
		for _, p := range c.Paths {
			mark("path:" + pathArea(p))
		}
	}

	// Keep artifacts that are contended, alarming, or at least attached to a
	// session — but drop a session's private area when the session has other,
	// more interesting attachments, or the middle column fills with noise.
	kept := []*artifact{}
	for _, id := range artOrder {
		a := arts[id]
		contended := len(a.sessions) > 1
		if contended || sevFor[id] != "" || alertFor[id] || a.kind != ArtifactPath {
			kept = append(kept, a)
			continue
		}
		kept = append(kept, a) // uncontended path areas stay: they are the calm
	}
	sort.Slice(kept, func(i, j int) bool {
		ai, aj := kept[i], kept[j]
		// Contended first, then alarming, then alphabetical — a stable order
		// is what keeps nodes from jumping between repaints.
		if (len(ai.sessions) > 1) != (len(aj.sessions) > 1) {
			return len(ai.sessions) > 1
		}
		if model.SeverityRank(sevFor[ai.id]) != model.SeverityRank(sevFor[aj.id]) {
			return model.SeverityRank(sevFor[ai.id]) > model.SeverityRank(sevFor[aj.id])
		}
		return ai.id < aj.id
	})

	// --- layout ---------------------------------------------------------
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

	for i, sess := range live {
		// Alternating sides is the mark: the strokes come from opposite
		// directions. It also means any two adjacent sessions crossing the
		// same artifact are drawn in contrasting colours.
		side := "A"
		if i%2 == 1 {
			side = "B"
		}
		sideOf[sess.Key] = side
		y := topPad + float64(i)*rowH
		sessionY[sess.Key] = y
		g.Nodes = append(g.Nodes, GraphNode{
			ID: "session:" + sess.Key, Kind: "session", Side: side,
			Label: s.SessionLabel(sess.Key), Sub: sess.StatusLine,
			X: sessionX, Y: y, R: 15,
			Href:  fmt.Sprintf("/t/%s/runs/%s", s.Team.Slug, sess.Key),
			Title: sess.Goal,
		})
	}

	for i, a := range kept {
		y := topPad + float64(i)*rowH
		degree := len(a.sessions)
		node := GraphNode{
			ID: a.id, Kind: a.kind, Label: a.label, Sub: a.sub,
			X: artifactX, Y: y, Degree: degree,
			Severity: sevFor[a.id], Alert: alertFor[a.id],
			R: 13,
		}
		if degree > 1 {
			// The crossing. Heavier, and it carries the severity.
			node.R = 13 + float64(min(degree, 4))*3
			g.Contended++
		} else if len(a.order) == 1 {
			node.Side = sideOf[a.order[0]]
		}
		if node.Alert {
			g.Alerts++
		}
		switch a.kind {
		case ArtifactContract:
			node.Href = fmt.Sprintf("/t/%s/contracts", s.Team.Slug)
		case ArtifactDecision:
			node.Href = fmt.Sprintf("/t/%s/decisions", s.Team.Slug)
		}
		node.Title = strings.Join(a.order, ", ")
		g.Nodes = append(g.Nodes, node)

		for _, sessionKey := range a.order {
			sy, ok := sessionY[sessionKey]
			if !ok {
				continue
			}
			g.Edges = append(g.Edges, GraphEdge{
				From: "session:" + sessionKey, To: a.id,
				X1: sessionX, Y1: sy, X2: artifactX, Y2: y,
				Side:  sideOf[sessionKey],
				Hot:   degree > 1,
				Alert: alertFor[a.id],
				Title: s.SessionLabel(sessionKey) + " " + a.sessions[sessionKey] + " " + a.label,
			})
		}
	}

	rows := max(len(live), len(kept))
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
