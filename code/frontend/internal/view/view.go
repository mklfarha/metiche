// Package view renders the board. Every fragment the browser ever receives is
// produced here, including the ones pushed over SSE — the client does no
// templating at all, which is why the frontend is a Go service and not a bundle.
package view

import (
	"fmt"
	"strings"

	"github.com/a-h/templ"

	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
	"github.com/mklfarha/metiche/frontend/internal/wording"
)

// Tab identifies the active nav item.
type Tab string

// The nav tabs.
const (
	TabBoard     Tab = "board"
	TabConflicts Tab = "conflicts"
	TabContracts Tab = "contracts"
	TabDecisions Tab = "decisions"
	TabRuns      Tab = "runs"
	TabGraph     Tab = "graph"
)

// Renderer satisfies hub.Renderer, so the fan-out can produce fragments
// without the hub importing templates directly.
type Renderer struct{}

// Board renders the whole board container — the fragment swapped with
// outerHTML when board_revision moves.
func (Renderer) Board(s state.Snapshot) templ.Component { return BoardFrame(s) }

// TimelineItem renders one log row — the fragment appended when sequence moves.
func (Renderer) TimelineItem(s state.Snapshot, ev model.Event) templ.Component {
	return TimelineFrame(s, ev)
}

// worstConflict returns the open conflict that should own the banner, or nil
// when the team is calm. Snapshot already sorts worst-first.
func worstConflict(s state.Snapshot) *model.Conflict {
	open := s.OpenConflicts()
	if len(open) == 0 {
		return nil
	}
	if model.SeverityRank(open[0].Severity) < model.SeverityRank("medium") {
		// Record floor is low, notify floor is medium. A low-severity conflict
		// is listed on the conflicts page and does not get to shout here.
		return nil
	}
	return open[0]
}

// conflictBadge picks the badge colour for a conflict.
//
// Severity normally drives it, with one deliberate exception: an unclaimed
// contract gets the cyan spark. It is not a collision between two sides — it
// is a side with nobody opposite, which is a different shape of problem and
// the icon already has a colour for "look here".
func conflictBadge(c *model.Conflict) string {
	if c.Kind == model.KindContractUnclaimed {
		return "alert"
	}
	return c.Severity
}

// partyClass colours the two sides of a collision with the two strokes of the
// mark: the one who was there first is indigo, the one who arrived into it is
// coral. Their overlap — the conflict itself — is violet everywhere else.
func partyClass(role string) string {
	switch role {
	case "holder", "producer":
		return "sideB"
	case "challenger", "consumer":
		return "sideA"
	}
	return ""
}

// modeAbbrev shortens a claim mode to the two characters the board has room
// for: wr / rd / st.
func modeAbbrev(mode string) string {
	switch mode {
	case "write":
		return "wr"
	case "read":
		return "rd"
	case "structural":
		return "st"
	}
	if len(mode) >= 2 {
		return mode[:2]
	}
	return "??"
}

// ---------------------------------------------------------------- helpers

func sevClass(prefix, sev string) string {
	if sev == "" {
		return ""
	}
	return prefix + sev
}

func agentClass(l state.AgentLane) string {
	classes := []string{"agent"}
	if l.Idle() {
		classes = append(classes, "idle")
	} else {
		classes = append(classes, "working")
	}
	if w := l.Worst(); w != "" {
		classes = append(classes, "sev-"+w)
	}
	if l.Nested {
		classes = append(classes, "sub")
		if l.Depth > 1 {
			classes = append(classes, "sub-deep")
		}
	}
	return strings.Join(classes, " ")
}

// subagentOf is a subagent lane's link to its supervisor, in words.
func subagentOf(l state.AgentLane) string {
	if l.ParentKey == "" {
		return ""
	}
	if !l.ParentLive {
		return "subagent of " + l.ParentKey + " (ended)"
	}
	return "subagent of " + l.ParentKey
}

func laneClass(l state.Lane) string {
	if w := l.Worst(); w != "" {
		return "lane sev-" + w
	}
	return "lane"
}

func pipClass(s *model.Session) string {
	switch {
	case s == nil:
		return "pip"
	case s.Status == model.SessionLive:
		return "pip live"
	case s.Status == model.SessionStale:
		return "pip stale"
	}
	return "pip"
}

func pathClass(l state.AgentLane, p string) string {
	if sev, ok := l.HotPaths[p]; ok && sev != "" {
		return "path hot-" + sev
	}
	return "path"
}

func eventClass(ev model.Event) string {
	classes := []string{"ev"}
	if ev.Structural {
		classes = append(classes, "structural")
	}
	if t := ev.Tone(); t != "" {
		classes = append(classes, t)
	}
	return strings.Join(classes, " ")
}

func initials(name string) string {
	parts := strings.Fields(name)
	switch len(parts) {
	case 0:
		return "??"
	case 1:
		if len(parts[0]) >= 2 {
			return strings.ToUpper(parts[0][:2])
		}
		return strings.ToUpper(parts[0])
	}
	return strings.ToUpper(string(parts[0][0]) + string(parts[len(parts)-1][0]))
}

// severityGlyph gives each severity a distinct shape, so the board still reads
// correctly in greyscale or to a colour-blind viewer.
func severityGlyph(sev string) string {
	switch sev {
	case "critical":
		return "◆" // filled diamond
	case "high":
		return "▲" // filled triangle
	case "medium":
		return "●" // filled circle
	case "low":
		return "○" // hollow circle
	}
	return "•"
}

func conflictLabel(kind string) string {
	switch kind {
	case model.KindPathOverlap:
		return "path overlap"
	case model.KindContractMismatch:
		return "contract mismatch"
	case model.KindContractUnclaimed:
		return "nobody is building this"
	case model.KindContractNamingVariant:
		return "naming variant"
	case model.KindDecisionContradiction:
		return "contradicts a decision"
	case model.KindDuplicateWork:
		return "duplicate work"
	case model.KindStaleBase:
		return "stale base"
	}
	return strings.ReplaceAll(kind, "_", " ")
}

func cellMark(c state.MatrixCell, unclaimed bool) (class, glyph, title string) {
	switch c.State {
	case state.CellProduces:
		class, glyph, title = "mk p", "P", "produces this contract"
	case state.CellConsumes:
		class, glyph, title = "mk c", "C", "consumes this contract"
		if unclaimed {
			class, title = "mk c orphan", "consumes a contract nobody produces"
		}
	case state.CellBoth:
		class, glyph, title = "mk b", "PC", "both produces and consumes"
	default:
		return "mk none", "·", ""
	}
	if c.Mismatched && !unclaimed {
		class += " bad"
		title += " — shape disagreement"
	}
	return class, glyph, title
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ---------------------------------------------------------------- cadence

// suggestedAction is the one sentence a conflict exists to deliver.
//
// The facts come from the conflict; the register comes from the project's
// cadence, via the single map in internal/wording. If that map has nothing for
// this combination the backend's own suggested action is used unchanged — the
// board does not invent advice it cannot stand behind.
func suggestedAction(s state.Snapshot, c *model.Conflict) string {
	cad := s.Project.Cadence
	vars := map[string]string{}
	var kind wording.Kind

	byRole := func(roles ...string) []string {
		out := []string{}
		for _, p := range c.Participants {
			for _, r := range roles {
				if p.Role == r {
					out = append(out, s.SessionLabel(p.SessionKey))
				}
			}
		}
		return out
	}
	all := make([]string, 0, len(c.Participants))
	for _, p := range c.Participants {
		all = append(all, s.SessionLabel(p.SessionKey))
	}

	switch c.Kind {
	case model.KindContractUnclaimed:
		kind = wording.KindUnclaimed
		if wording.Overdue(cad, s.Now, c.RaisedAt) {
			kind = wording.KindUnclaimedOverdue
		}
		vars["what"] = c.ContractKey
		vars["who"] = wording.Join(byRole("consumer"))
		vars["elapsed"] = wording.Short(s.Now.Sub(c.RaisedAt))
		vars["budget"] = wording.Short(wording.Threshold(cad))
		vars["waiting"] = wording.Count(waitingFiles(s, c), "file")

	case model.KindPathOverlap:
		kind = wording.KindPathOverlap
		vars["what"] = strings.Join(c.Paths, ", ")
		vars["holder"] = wording.Join(byRole("holder"))
		vars["other"] = wording.Join(byRole("challenger"))

	case model.KindContractMismatch:
		kind = wording.KindContractMismatch
		vars["what"] = c.ContractKey
		vars["who"] = wording.Join(all)
		vars["other"] = wording.Join(byRole("consumer"))

	case model.KindDecisionContradiction:
		kind = wording.KindDecision
		vars["what"] = c.DecisionKey
		vars["who"] = wording.Join(all)

	case model.KindDuplicateWork:
		kind = wording.KindDuplicate
		vars["who"] = wording.Join(all)

	default:
		return c.SuggestedAction
	}

	// A fact the board does not have makes the sentence WRONG rather than
	// merely shorter: "Mara holds ." reads as a bug, and it is one. The live
	// read API does not expose a conflict's paths or the contract it is about
	// — they live in the conflict's evidence json, which is not on the wire —
	// so this is the ordinary path against a real backend rather than an edge
	// case, and the backend's own suggested action is both present and better
	// than a sentence with a hole in it.
	for _, v := range vars {
		if strings.TrimSpace(v) == "" {
			return c.SuggestedAction
		}
	}

	if phrase := wording.Phrase(cad, kind, vars); phrase != "" {
		return phrase
	}
	return c.SuggestedAction
}

// waitingFiles counts the files the affected sessions currently hold, which is
// the closest honest answer to "how much work is riding on this".
func waitingFiles(s state.Snapshot, c *model.Conflict) int {
	involved := map[string]bool{}
	for _, p := range c.Participants {
		involved[p.SessionKey] = true
	}
	n := 0
	for _, claim := range s.Claims {
		if involved[claim.SessionKey] && claim.Active(s.Now) {
			n += len(claim.Paths)
		}
	}
	return n
}

// ageBadge renders how long a finding has been sitting against what this
// cadence tolerates — "12m / 5m" — so the threshold is visible rather than
// implied. Returns "" for kinds where elapsed time is not the point.
func ageBadge(s state.Snapshot, c *model.Conflict) (text, class string, show bool) {
	if c.Kind != model.KindContractUnclaimed || c.RaisedAt.IsZero() {
		return "", "", false
	}
	cad := s.Project.Cadence
	elapsed := wording.Short(s.Now.Sub(c.RaisedAt))
	budget := wording.Short(wording.Threshold(cad))
	if wording.Overdue(cad, s.Now, c.RaisedAt) {
		return elapsed + " / " + budget + " budget", "alert", true
	}
	return elapsed + " of " + budget, "plain", true
}

// calmLine is what the board says when nothing is wrong.
func calmLine(s state.Snapshot) string {
	if p := wording.Phrase(s.Project.Cadence, wording.KindCalm, nil); p != "" {
		return p
	}
	return "Nothing colliding."
}

// cadenceHint explains what the current setting is doing, because a cadence
// set wrong is invisible until its advice is wrong.
func cadenceHint(c model.Cadence) string {
	return wording.Phrase(c, wording.KindCadenceHint, nil)
}

// ---------------------------------------------------------------- graph

// graphNodeClass turns a laid-out node into the classes the stylesheet paints
// it with. The rule the whole graph follows: a node touched by two sessions is
// the crossing, so it is violet and heavy; a node with nobody opposite is the
// cyan spark; everything else wears the side it belongs to, coral or indigo,
// lightly.
func graphNodeClass(n state.GraphNode) string {
	classes := []string{"gn", "gn-" + n.Kind}
	switch {
	case n.Alert:
		classes = append(classes, "gn-alert")
	case n.Degree > 1 && n.Handoff:
		// Two sessions in one area, never at the same moment: no crossing.
		classes = append(classes, "gn-handoff")
		if n.Severity != "" {
			classes = append(classes, "gn-sev-"+n.Severity)
		}
	case n.Degree > 1:
		classes = append(classes, "gn-cross")
		if n.Severity != "" {
			classes = append(classes, "gn-sev-"+n.Severity)
		}
	case n.Side != "":
		classes = append(classes, "gn-side"+n.Side)
	default:
		classes = append(classes, "gn-quiet")
	}
	return strings.Join(classes, " ")
}

func graphEdgeClass(e state.GraphEdge) string {
	classes := []string{"ge", "ge-side" + e.Side}
	if e.Hot {
		classes = append(classes, "ge-hot")
	}
	if e.Alert {
		classes = append(classes, "ge-alert")
	}
	return strings.Join(classes, " ")
}

// graphCurve draws the edge as a flat S so many edges into one node stay
// individually traceable, which straight lines at these angles do not.
func graphCurve(e state.GraphEdge) string {
	dx := (e.X2 - e.X1) * 0.55
	return fmt.Sprintf("M %.1f %.1f C %.1f %.1f, %.1f %.1f, %.1f %.1f",
		e.X1, e.Y1, e.X1+dx, e.Y1, e.X2-dx, e.Y2, e.X2, e.Y2)
}

// graphGlyph gives each artifact kind a shape, so the graph reads without
// colour: a square area of the repo, a round contract, a diamond decision.
func graphDiamond(n state.GraphNode) string {
	r := n.R
	return fmt.Sprintf("%.1f,%.1f %.1f,%.1f %.1f,%.1f %.1f,%.1f",
		n.X, n.Y-r, n.X+r, n.Y, n.X, n.Y+r, n.X-r, n.Y)
}

func f(v float64) string { return fmt.Sprintf("%.1f", v) }
