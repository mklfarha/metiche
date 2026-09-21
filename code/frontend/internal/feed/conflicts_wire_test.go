package feed

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// The duplicate_work examples of docs/DUPLICATES.md §9.3 and §9.4, byte for
// byte. They are the contract with app/webapi, which is not built yet;
// TestDuplicateSpecExamplesAreVerbatim fails if the spec moves on without
// this file.

const spec93DuplicateConflict = `{
  "key": "CF-44",
  "kind": "duplicate_work",
  "severity": "medium",
  "status": "open",
  "detected_by": "agent",
  "detector_rule": "duplicate_work.words",
  "suggested_action": "INT-83 (Ana (ui), active 4m) is already building this: \"add login page\". Stop before you edit: mark INT-92 superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with Ana's agent (S-17) first.",
  "occurrence_count": 1,
  "first_detected_at": "2026-09-21T14:02:40Z",
  "last_detected_at": "2026-09-21T14:02:40Z",
  "paths": ["web/src/pages/Login.tsx", "web/src/routes/login.tsx"],
  "plans": [
    {"key": "INT-83", "who": "Ana (ui)", "summary": "add login page", "path": "web/src/routes/login.tsx", "yields": false},
    {"key": "INT-92", "who": "Bob (frontend)", "summary": "build the login screen", "path": "web/src/pages/Login.tsx", "yields": true}
  ],
  "signals": ["shared words: login, screen"],
  "judge_note": "S-22's model (0.85): both build the login page; INT-83 already has the route",
  "escalated_at": "2026-09-21T14:12:40Z",
  "participants": [
    {"session_key": "S-22", "member_name": "Bob", "agent_label": "frontend", "role": "initiator", "subject_kind": "intent"},
    {"session_key": "S-17", "member_name": "Ana", "agent_label": "ui", "role": "incumbent", "subject_kind": "intent"}
  ]
}`

// §9.3's settled variant: "the same object carries" these.
const spec93YieldedNote = "Settled by the agents: S-22 (frontend) marked INT-92 superseded at 14:15 UTC, leaving INT-83 (S-17 (ui)) to build it. A person was asked at 14:12 UTC."

// §9.4: the evidence the wire above is read from, as stored.
const spec94DuplicateEvidence = `{"overlap_path": null,
 "a_label": "Ana (ui)", "a_summary": "add login page", "a_pattern": "web/src/routes/login.tsx",
 "b_label": "Bob (frontend)", "b_summary": "build the login screen", "b_pattern": "web/src/pages/Login.tsx",
 "adjusters": ["plans:INT-83,INT-92", "words:login,screen"],
 "field_issues": ["S-22's model (0.85): both build the login page; INT-83 already has the route"],
 "detail": "duplicate_work.words"}`

// TestDuplicateSpecExamplesAreVerbatim: the constants above are the spec's
// own text, the settled note included.
func TestDuplicateSpecExamplesAreVerbatim(t *testing.T) {
	raw, err := os.ReadFile("../../../../docs/DUPLICATES.md")
	if err != nil {
		t.Skipf("docs/DUPLICATES.md is not beside this checkout: %v", err)
	}
	doc := string(raw)
	for name, example := range map[string]string{"§9.3": spec93DuplicateConflict, "§9.4": spec94DuplicateEvidence} {
		if !strings.Contains(doc, "```json\n"+example+"\n```") {
			t.Errorf("the %s example is no longer the spec's text", name)
		}
	}
	if !strings.Contains(doc, "`\"resolution_note\": "+fmt.Sprintf("%q", spec93YieldedNote)+"`") {
		t.Error("the §9.3 settled resolution_note is no longer the spec's text")
	}
	for _, want := range []string{"`\"resolution\": \"yielded\"`", "`\"resolved_at\": \"2026-09-21T14:15:02Z\"`",
		"`\"issue_ref\": \"ISSUE-412\"`", "`same issue ISSUE-412`", "`duplicate_work.same_issue`"} {
		if !strings.Contains(doc, want) {
			t.Errorf("§9.3 no longer says %s", want)
		}
	}
}

// TestConflictWireMapsSpec93Duplicate: plans, signals, judge_note and
// escalated_at, open; then settled; then with a shared issue id.
func TestConflictWireMapsSpec93Duplicate(t *testing.T) {
	var cw conflictJSON
	if err := json.Unmarshal([]byte(spec93DuplicateConflict), &cw); err != nil {
		t.Fatal(err)
	}
	c := cw.conflict(map[string]string{"S-22": "Bob", "S-17": "Ana"})
	checks := []struct {
		name      string
		got, want any
	}{
		{"key", c.Key, "CF-44"},
		{"kind", c.Kind, model.KindDuplicateWork},
		{"severity", c.Severity, "medium"},
		{"open", c.Open(), true},
		{"suggested_action", c.SuggestedAction, "INT-83 (Ana (ui), active 4m) is already building this: \"add login page\". Stop before you edit: mark INT-92 superseded with update_intent, or re-scope it to a different part and update_intent the summary, which asks you to judge once more. If your plan should be the one that continues, settle that with Ana's agent (S-17) first."},
		{"occurrences", c.Occurrences, 1},
		{"raised", c.RaisedAt, utc("2026-09-21T14:02:40Z")},
		{"paths", fmt.Sprint(c.Paths), "[web/src/pages/Login.tsx web/src/routes/login.tsx]"},
		{"plans", fmt.Sprintf("%+v", c.Plans), "[" +
			"{Key:INT-83 Who:Ana (ui) Summary:add login page Path:web/src/routes/login.tsx Yields:false} " +
			"{Key:INT-92 Who:Bob (frontend) Summary:build the login screen Path:web/src/pages/Login.tsx Yields:true}]"},
		{"signals", fmt.Sprintf("%q", c.Signals), `["shared words: login, screen"]`},
		{"issue_ref absent", c.IssueRef, ""},
		{"judge_note", c.JudgeNote, "S-22's model (0.85): both build the login page; INT-83 already has the route"},
		{"escalated_at", c.EscalatedAt, utc("2026-09-21T14:12:40Z")},
		{"no decision", c.DecisionKey, ""},
		{"participants", fmt.Sprintf("%+v", c.Participants), "[" +
			"{SessionKey:S-22 MemberKey:Bob Role:initiator Detail:intent MemberName:Bob AgentLabel:frontend} " +
			"{SessionKey:S-17 MemberKey:Ana Role:incumbent Detail:intent MemberName:Ana AgentLabel:ui}]"},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %#v, want %#v", ck.name, ck.got, ck.want)
		}
	}

	settledJSON := strings.Replace(spec93DuplicateConflict, `"status": "open",`,
		`"status": "resolved", "resolution": "yielded", "resolved_at": "2026-09-21T14:15:02Z", "resolution_note": `+
			fmt.Sprintf("%q", spec93YieldedNote)+`,`, 1)
	var sw conflictJSON
	if err := json.Unmarshal([]byte(settledJSON), &sw); err != nil {
		t.Fatal(err)
	}
	s := sw.conflict(nil)
	if s.Open() || s.Resolution != "yielded" || s.ResolutionNote != spec93YieldedNote ||
		!s.ResolvedAt.Equal(utc("2026-09-21T14:15:02Z")) || len(s.Plans) != 2 || !s.Plans[1].Yields || s.EscalatedAt.IsZero() {
		t.Errorf("settled duplicate = %+v", s)
	}

	issueJSON := strings.NewReplacer(
		`"detector_rule": "duplicate_work.words",`, `"detector_rule": "duplicate_work.same_issue", "issue_ref": "ISSUE-412",`,
		`"signals": ["shared words: login, screen"],`, `"signals": ["same issue ISSUE-412", "shared words: login, screen"],`,
	).Replace(spec93DuplicateConflict)
	var iw conflictJSON
	if err := json.Unmarshal([]byte(issueJSON), &iw); err != nil {
		t.Fatal(err)
	}
	if i := iw.conflict(nil); i.IssueRef != "ISSUE-412" || fmt.Sprint(i.Signals) != "[same issue ISSUE-412 shared words: login, screen]" {
		t.Errorf("issue duplicate = %+v", i)
	}

	// Absent, the three are zero: every other kind still maps as before.
	var old conflictJSON
	if err := json.Unmarshal([]byte(`{"key":"CF-1","kind":"path_overlap","paths":["a.go"]}`), &old); err != nil {
		t.Fatal(err)
	}
	if o := old.conflict(nil); o.Plans != nil || o.Signals != nil || o.IssueRef != "" || len(o.Paths) != 1 {
		t.Errorf("a conflict without the duplicate fields = %+v", o)
	}
}

// TestDuplicateWireAgreesWithItsEvidence: §9.3's plans, signals and judge
// note are §9.4's evidence as §5.1 says the backend reads it: plans are
// [a, b] with b the yield side, `paths` is [b_pattern, a_pattern], the
// judge's note is field_issues[0], and the words: fact is the one signal
// (the plans: fact never is).
func TestDuplicateWireAgreesWithItsEvidence(t *testing.T) {
	var ev struct {
		ALabel, ASummary, APattern string
		BLabel, BSummary, BPattern string
		Adjusters, FieldIssues     []string
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(spec94DuplicateEvidence), &raw); err != nil {
		t.Fatal(err)
	}
	str := func(k string) string { s, _ := raw[k].(string); return s }
	strs := func(k string) (out []string) {
		for _, v := range raw[k].([]any) {
			out = append(out, v.(string))
		}
		return out
	}
	ev.ALabel, ev.ASummary, ev.APattern = str("a_label"), str("a_summary"), str("a_pattern")
	ev.BLabel, ev.BSummary, ev.BPattern = str("b_label"), str("b_summary"), str("b_pattern")
	ev.Adjusters, ev.FieldIssues = strs("adjusters"), strs("field_issues")

	var cw conflictJSON
	if err := json.Unmarshal([]byte(spec93DuplicateConflict), &cw); err != nil {
		t.Fatal(err)
	}
	c := cw.conflict(nil)
	a, b := c.Plans[0], c.Plans[1]
	if a.Who != ev.ALabel || a.Summary != ev.ASummary || a.Path != ev.APattern || a.Yields {
		t.Errorf("plans[0] = %+v, evidence a = %s / %s / %s", a, ev.ALabel, ev.ASummary, ev.APattern)
	}
	if b.Who != ev.BLabel || b.Summary != ev.BSummary || b.Path != ev.BPattern || !b.Yields {
		t.Errorf("plans[1] = %+v, evidence b = %s / %s / %s", b, ev.BLabel, ev.BSummary, ev.BPattern)
	}
	if fmt.Sprint(c.Paths) != fmt.Sprint([]string{ev.BPattern, ev.APattern}) {
		t.Errorf("paths = %v, want [b_pattern a_pattern]", c.Paths)
	}
	if ev.Adjusters[0] != "plans:"+a.Key+","+b.Key {
		t.Errorf("plans fact %q does not name %s,%s", ev.Adjusters[0], a.Key, b.Key)
	}
	if c.JudgeNote != ev.FieldIssues[0] {
		t.Errorf("judge_note %q is not field_issues[0] %q", c.JudgeNote, ev.FieldIssues[0])
	}
	words := strings.ReplaceAll(strings.TrimPrefix(ev.Adjusters[1], "words:"), ",", ", ")
	if len(c.Signals) != 1 || c.Signals[0] != "shared words: "+words {
		t.Errorf("signals = %q, want only the words fact", c.Signals)
	}
}
