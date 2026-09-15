package feed

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
)

// The §9 examples of docs/DECISIONS.md, byte for byte. They are the contract
// with app/webapi; TestSpecExamplesAreVerbatim fails if the spec moves on
// without this file.

const spec92Decisions = `{
  "sequence": 431,
  "board_revision": 92,
  "team": {"key": "shop-hack", "name": "Shop hackathon", "sequence": 431, "board_revision": 92},
  "decisions": [
    {
      "key": "#auth-jwt-cookie",
      "title": "Auth is a JWT in an httpOnly cookie",
      "statement": "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.",
      "status": "accepted",
      "always_show": true,
      "revision": 2,
      "decided_by": "Ana",
      "decided_at": "2026-09-15T13:02:11Z",
      "updated_at": "2026-09-15T13:40:05Z",
      "scope": ["internal/auth/**", "web/src/auth/**"],
      "project_key": "shop",
      "rationale": "One session mechanism for the web app and the API; a cookie is out of reach of injected scripts.",
      "supersedes": "#auth-bearer-header",
      "judged": {"no_conflict": 5, "conflict": 1, "unsure": 0, "pending": 2},
      "open_conflicts": ["CF-31"]
    }
  ]
}`

const spec93DecisionHistory = `{
  "sequence": 431,
  "board_revision": 92,
  "team": {"key": "shop-hack", "name": "Shop hackathon", "sequence": 431, "board_revision": 92},
  "status": "",
  "statuses": ["superseded", "revoked"],
  "decisions": [
    {
      "key": "#auth-bearer-header",
      "title": "Auth is a bearer token in the Authorization header",
      "statement": "Clients send the session JWT as Authorization: Bearer on every API call.",
      "status": "superseded",
      "always_show": false,
      "revision": 1,
      "decided_by": "Bob",
      "decided_at": "2026-09-15T11:20:00Z",
      "updated_at": "2026-09-15T13:02:11Z",
      "ended_at": "2026-09-15T13:02:11Z",
      "scope": ["internal/auth/**"],
      "project_key": "shop",
      "superseded_by": "#auth-jwt-cookie",
      "judged": {"no_conflict": 3, "conflict": 0, "unsure": 1, "pending": 0},
      "open_conflicts": []
    }
  ],
  "next_cursor": "ZDF8MjAyNi0wOS0xNVQxMzowMjoxMVp8I2F1dGgtYmVhcmVyLWhlYWRlcg",
  "revisions": [
    {
      "sequence": 402,
      "kind": "decision_recorded",
      "summary": "backend revised #auth-jwt-cookie (r2)",
      "statement": "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.",
      "occurred_at": "2026-09-15T13:40:05Z"
    }
  ]
}`

const spec94DecisionConflict = `{
  "key": "CF-31",
  "kind": "decision_contradiction",
  "severity": "high",
  "status": "open",
  "detected_by": "agent",
  "detector_rule": "decision_contradiction.judged",
  "suggested_action": "Your plan INT-91 breaks #auth-jwt-cookie (Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage…). Change the plan to follow it and update_intent with the new summary, which asks you to judge again. If the decision itself is wrong, settle that with Ana's agent (S-17); only Ana or your person can change it.",
  "occurrence_count": 1,
  "first_detected_at": "2026-09-15T14:02:40Z",
  "last_detected_at": "2026-09-15T14:02:40Z",
  "paths": ["web/src/auth/session.ts", "web/src/auth/**"],
  "decision_key": "#auth-jwt-cookie",
  "judge_note": "S-22's model (0.90): plan stores the token in localStorage; #auth-jwt-cookie forbids it",
  "escalated_at": "2026-09-15T14:12:40Z",
  "participants": [
    {"session_key": "S-22", "member_name": "Bob", "agent_label": "ui", "role": "initiator", "subject_kind": "intent"},
    {"session_key": "S-17", "member_name": "Ana", "agent_label": "backend", "role": "incumbent", "subject_kind": "decision"}
  ]
}`

// §9.4's settled variant: "the same object carries" these.
const spec94ConvergedNote = "Settled by the agents: S-22 (ui) revised INT-91 at 14:15 UTC and judged it no longer contradicts #auth-jwt-cookie (r2): \"plan now relies on the httpOnly cookie\". A person was asked at 14:12 UTC."

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestSpecExamplesAreVerbatim: the constants above are the spec's own text.
func TestSpecExamplesAreVerbatim(t *testing.T) {
	raw, err := os.ReadFile("../../../../docs/DECISIONS.md")
	if err != nil {
		t.Skipf("docs/DECISIONS.md is not beside this checkout: %v", err)
	}
	for name, example := range map[string]string{"§9.2": spec92Decisions, "§9.3": spec93DecisionHistory, "§9.4": spec94DecisionConflict} {
		if !strings.Contains(string(raw), "```json\n"+example+"\n```") {
			t.Errorf("the %s example is no longer the spec's text", name)
		}
	}
}

// TestDecisionsWireMapsSpec92: GET /decisions, every field.
func TestDecisionsWireMapsSpec92(t *testing.T) {
	var w decisionsWire
	if err := json.Unmarshal([]byte(spec92Decisions), &w); err != nil {
		t.Fatal(err)
	}
	if w.Sequence != 431 || w.BoardRevision != 92 || w.Team.Key != "shop-hack" {
		t.Fatalf("cursors/team = %+v", w)
	}
	ds := w.decisions()
	if len(ds) != 1 {
		t.Fatalf("decisions = %d", len(ds))
	}
	d := ds[0]
	checks := []struct {
		name      string
		got, want any
	}{
		{"key", d.Key, "#auth-jwt-cookie"},
		{"title", d.Title, "Auth is a JWT in an httpOnly cookie"},
		{"statement", d.Statement, "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage."},
		{"status accepted -> active", d.Status, "active"},
		{"always_show", d.AlwaysShow, true},
		{"revision", d.Revision, int64(2)},
		{"decided_by", d.DecidedBy, "Ana"},
		{"decided_at", d.RecordedAt, utc("2026-09-15T13:02:11Z")},
		{"updated_at", d.UpdatedAt, utc("2026-09-15T13:40:05Z")},
		{"ended_at absent", d.EndedAt.IsZero(), true},
		{"scope joined", d.Scope, "internal/auth/**, web/src/auth/**"},
		{"scope parts", fmt.Sprint(d.ScopeParts()), "[internal/auth/** web/src/auth/**]"},
		{"project_key", d.ProjectKey, "shop"},
		{"team-wide", d.TeamWide(), false},
		{"rationale", d.Rationale, "One session mechanism for the web app and the API; a cookie is out of reach of injected scripts."},
		{"supersedes", d.Supersedes, "#auth-bearer-header"},
		{"superseded_by absent", d.SupersededBy, ""},
		{"judged", d.Judged, model.DecisionJudged{NoConflict: 5, Conflict: 1, Unsure: 0, Pending: 2}},
		{"checked", d.Judged.Checked(), int64(6)},
		{"open_conflicts", fmt.Sprint(d.OpenConflicts), "[CF-31]"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

// TestDecisionHistoryWireMapsSpec93: GET /decisions/history, a past decision,
// the cursor and the revisions.
func TestDecisionHistoryWireMapsSpec93(t *testing.T) {
	var w decisionHistoryWire
	if err := json.Unmarshal([]byte(spec93DecisionHistory), &w); err != nil {
		t.Fatal(err)
	}
	p := w.page()
	if p.NextCursor != "ZDF8MjAyNi0wOS0xNVQxMzowMjoxMVp8I2F1dGgtYmVhcmVyLWhlYWRlcg" {
		t.Errorf("next_cursor = %q", p.NextCursor)
	}
	if fmt.Sprint(p.Statuses) != "[superseded revoked]" {
		t.Errorf("statuses = %v", p.Statuses)
	}
	if len(p.Decisions) != 1 {
		t.Fatalf("decisions = %d", len(p.Decisions))
	}
	d := p.Decisions[0]
	if d.Key != "#auth-bearer-header" || d.Status != "superseded" || d.Active() || d.AlwaysShow || d.Revision != 1 ||
		d.DecidedBy != "Bob" || !d.RecordedAt.Equal(utc("2026-09-15T11:20:00Z")) || !d.UpdatedAt.Equal(utc("2026-09-15T13:02:11Z")) ||
		!d.EndedAt.Equal(utc("2026-09-15T13:02:11Z")) || d.Scope != "internal/auth/**" || d.ProjectKey != "shop" ||
		d.SupersededBy != "#auth-jwt-cookie" || d.Supersedes != "" || d.Rationale != "" ||
		d.Title != "Auth is a bearer token in the Authorization header" ||
		d.Statement != "Clients send the session JWT as Authorization: Bearer on every API call." {
		t.Errorf("past decision = %+v", d)
	}
	if d.Judged != (model.DecisionJudged{NoConflict: 3, Conflict: 0, Unsure: 1, Pending: 0}) || d.Judged.Checked() != 4 {
		t.Errorf("judged = %+v", d.Judged)
	}
	if d.OpenConflicts == nil || len(d.OpenConflicts) != 0 {
		t.Errorf("open_conflicts = %#v, want present and empty", d.OpenConflicts)
	}
	if len(p.Revisions) != 1 {
		t.Fatalf("revisions = %d", len(p.Revisions))
	}
	r := p.Revisions[0]
	if r.Sequence != 402 || r.Kind != "decision_recorded" || r.Summary != "backend revised #auth-jwt-cookie (r2)" ||
		r.Statement != "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage." ||
		!r.OccurredAt.Equal(utc("2026-09-15T13:40:05Z")) {
		t.Errorf("revision = %+v", r)
	}
}

// TestConflictWireMapsSpec94: decision_key, judge_note and escalated_at, open
// and settled.
func TestConflictWireMapsSpec94(t *testing.T) {
	var cw conflictJSON
	if err := json.Unmarshal([]byte(spec94DecisionConflict), &cw); err != nil {
		t.Fatal(err)
	}
	c := cw.conflict(map[string]string{"S-22": "Bob", "S-17": "Ana"})
	if c.Key != "CF-31" || c.Kind != "decision_contradiction" || c.Severity != "high" || !c.Open() ||
		c.DecisionKey != "#auth-jwt-cookie" ||
		c.JudgeNote != "S-22's model (0.90): plan stores the token in localStorage; #auth-jwt-cookie forbids it" ||
		!c.EscalatedAt.Equal(utc("2026-09-15T14:12:40Z")) || !c.RaisedAt.Equal(utc("2026-09-15T14:02:40Z")) ||
		fmt.Sprint(c.Paths) != "[web/src/auth/session.ts web/src/auth/**]" || c.Occurrences != 1 ||
		!strings.HasPrefix(c.SuggestedAction, "Your plan INT-91 breaks #auth-jwt-cookie") || c.ContractKey != "" {
		t.Errorf("open conflict = %+v", c)
	}
	if len(c.Participants) != 2 || c.Participants[0].Detail != "intent" || c.Participants[0].Role != "initiator" ||
		c.Participants[1].Detail != "decision" || c.Participants[1].MemberName != "Ana" || c.Participants[1].AgentLabel != "backend" {
		t.Errorf("participants = %+v", c.Participants)
	}

	settledJSON := strings.Replace(spec94DecisionConflict, `"status": "open",`,
		`"status": "resolved", "resolution": "converged", "resolved_at": "2026-09-15T14:15:02Z", "resolution_note": `+
			fmt.Sprintf("%q", spec94ConvergedNote)+`,`, 1)
	var sw conflictJSON
	if err := json.Unmarshal([]byte(settledJSON), &sw); err != nil {
		t.Fatal(err)
	}
	s := sw.conflict(nil)
	if s.Open() || s.Resolution != "converged" || s.ResolutionNote != spec94ConvergedNote ||
		!s.ResolvedAt.Equal(utc("2026-09-15T14:15:02Z")) || s.DecisionKey != "#auth-jwt-cookie" || s.EscalatedAt.IsZero() {
		t.Errorf("settled conflict = %+v", s)
	}

	// Absent, the three are zero: an older backend's conflict still maps.
	var old conflictJSON
	if err := json.Unmarshal([]byte(`{"key":"CF-1","kind":"path_overlap"}`), &old); err != nil {
		t.Fatal(err)
	}
	if o := old.conflict(nil); o.DecisionKey != "" || o.JudgeNote != "" || !o.EscalatedAt.IsZero() {
		t.Errorf("a conflict without the new fields = %+v", o)
	}
}

// TestDecisionKeyOnTheWireLightsTheGraph: §9.4's decision_key, with §9.2's
// decision and the two live sessions, draws a decision node on the graph; the
// same conflict without it draws none.
func TestDecisionKeyOnTheWireLightsTheGraph(t *testing.T) {
	var cw conflictJSON
	if err := json.Unmarshal([]byte(spec94DecisionConflict), &cw); err != nil {
		t.Fatal(err)
	}
	var dw decisionsWire
	if err := json.Unmarshal([]byte(spec92Decisions), &dw); err != nil {
		t.Fatal(err)
	}
	snap := snapshotWire{
		Team: teamWire{Key: "shop-hack", Name: "Shop hackathon"},
		Sessions: []sessionJSON{
			{Key: "S-22", MemberName: "Bob", AgentLabel: "ui", Status: "live"},
			{Key: "S-17", MemberName: "Ana", AgentLabel: "backend", Status: "live"},
		},
		Conflicts: []conflictJSON{cw},
	}
	draw := func(withKey bool) []string {
		ts := snap.teamState("shop-hack")
		ts.Decisions = dw.decisions()
		if !withKey {
			ts.Conflicts[0].DecisionKey = ""
		}
		st := state.New("shop-hack", "Shop hackathon")
		st.Load(ts)
		var nodes []string
		for _, n := range st.Snapshot().EntanglementGraph().Nodes {
			if n.Kind == state.ArtifactDecision {
				nodes = append(nodes, fmt.Sprintf("%+v", n))
			}
		}
		return nodes
	}
	nodes := draw(true)
	if len(nodes) != 1 || !strings.Contains(nodes[0], "#auth-jwt-cookie") {
		t.Fatalf("decision nodes = %v, want the one for #auth-jwt-cookie", nodes)
	}
	if without := draw(false); len(without) != 0 {
		t.Fatalf("without decision_key the graph still drew %v", without)
	}
	t.Logf("decision node: %s", nodes[0])
}
