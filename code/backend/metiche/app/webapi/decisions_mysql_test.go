package webapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/backend/enums"
)

// The decisions half of the board's read API over a real MySQL: GET
// /decisions, GET /decisions/history, and the three fields a
// decision_contradiction adds to every conflict wire. Same DSN rule as the
// rest of this package (see dsnEnv). Names are Ana, Bob and test.
//
// The expectations are not retyped here. They are the §9 examples of
// docs/DECISIONS.md as code/frontend/internal/feed's test data holds them —
// the board frontend is already built and merged against those bytes, and
// that file's own TestSpecExamplesAreVerbatim pins it to the spec. This file
// seeds the rows those examples describe, serves them through the real
// handlers, and asserts the JSON that comes back is that example.

// specFile is the frontend's frozen copy of the §9 examples, from this
// package's directory.
var specFile = filepath.Join("..", "..", "..", "..", "frontend", "internal", "feed", "decisions_wire_test.go")

// specExample returns the backquoted const of that name from the frontend's
// test data.
func specExample(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(specFile)
	if err != nil {
		t.Skipf("skipped: the frontend's frozen §9 examples are not beside this checkout: %v", err)
	}
	marker := "const " + name + " = `"
	i := strings.Index(string(raw), marker)
	if i < 0 {
		t.Fatalf("%s is no longer in %s; the frozen example moved", name, specFile)
	}
	rest := string(raw)[i+len(marker):]
	j := strings.Index(rest, "`")
	if j < 0 {
		t.Fatalf("%s is not terminated in %s", name, specFile)
	}
	return rest[:j]
}

// specQuoted returns a plain quoted const from the same file.
func specQuoted(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(specFile)
	if err != nil {
		t.Skipf("skipped: the frontend's frozen §9 examples are not beside this checkout: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "const "+name+" = ") {
			continue
		}
		s, err := strconv.Unquote(strings.TrimPrefix(line, "const "+name+" = "))
		if err != nil {
			t.Fatalf("%s is not a quoted string in %s: %v", name, specFile, err)
		}
		return s
	}
	t.Fatalf("%s is no longer in %s", name, specFile)
	return ""
}

func jsonMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode %.80s: %v", raw, err)
	}
	return m
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("%q is not a string in %v", key, m)
	}
	return s
}

func firstOf(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	list, ok := m[key].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("%q is not a non-empty list in %v", key, m)
	}
	one, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("%q[0] is not an object", key)
	}
	return one
}

// same compares two decoded JSON values and reports the difference.
func same(t *testing.T, what string, got, want any) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	g, _ := json.MarshalIndent(got, "", "  ")
	w, _ := json.MarshalIndent(want, "", "  ")
	t.Fatalf("%s does not match the frozen §9 example\n got:\n%s\nwant:\n%s", what, g, w)
}

// sqlTime turns one of the examples' RFC 3339 instants into the literal the
// DATETIME column stores.
func sqlTime(t *testing.T, rfc string) string {
	t.Helper()
	at, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		t.Fatalf("parse %q: %v", rfc, err)
	}
	return at.UTC().Format("2006-01-02 15:04:05")
}

// ---------------------------------------------------------------- fixture

type shopFixture struct {
	teamID, slug   string
	project        string
	ana, bob       string // member uuids
	jwt, bearer    string // decision uuids
	older          string // a third past decision, so the history has two pages
	cf31           string // the open decision conflict of §9.4
	spec92, spec93 map[string]any
	spec94         map[string]any
	converged      string
}

// seedShopHack builds exactly the team the §9 examples describe: the
// hackathon, Ana's accepted decision at revision 2, the bearer-token decision
// it superseded, an older revoked one, the judgements behind the counts, and
// the open decision conflict CF-31 with its two participants.
//
// Everything textual — titles, statements, the rationale, the suggested
// action, the judge's note — is read OUT of the frozen examples and written
// into the database, so a transcription slip here cannot quietly become the
// expectation as well.
func seedShopHack(t *testing.T, db *sql.DB) shopFixture {
	t.Helper()
	fx := shopFixture{
		spec92:    jsonMap(t, specExample(t, "spec92Decisions")),
		spec93:    jsonMap(t, specExample(t, "spec93DecisionHistory")),
		spec94:    jsonMap(t, specExample(t, "spec94DecisionConflict")),
		converged: specQuoted(t, "spec94ConvergedNote"),
	}
	team := fx.spec92["team"].(map[string]any)
	d92 := firstOf(t, fx.spec92, "decisions")
	d93 := firstOf(t, fx.spec93, "decisions")

	fx.teamID, fx.slug = newUUID(t), str(t, team, "key")
	// A previous run that died between seeding and cleanup must not make
	// every later run fail on uq_team_slug.
	mustExec(t, db, "DELETE FROM `team` WHERE `slug` = ?", fx.slug)
	mustExec(t, db, "INSERT INTO `team` (`id`,`name`,`slug`,`sequence`,`board_revision`,`status`,`visibility`) VALUES (?,?,?,?,?,?,?)",
		fx.teamID, str(t, team, "name"), fx.slug, int64(team["sequence"].(float64)), int64(team["board_revision"].(float64)),
		1, enums.TEAM_VISIBILITY_PUBLIC)
	t.Cleanup(func() { mustExec(t, db, "DELETE FROM `team` WHERE `id` = ?", fx.teamID) })

	acctA, acctB := newUUID(t), newUUID(t)
	for _, a := range []struct{ id, name, hash string }{
		{acctA, "Ana", "not-a-real-hash-ana"},
		{acctB, "Bob", "not-a-real-hash-bob"},
	} {
		mustExec(t, db, "INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
			a.id, "acct-"+a.id[:8], a.name, a.hash+"-"+a.id[:8], 1, 1)
	}
	t.Cleanup(func() { mustExec(t, db, "DELETE FROM `account` WHERE `id` IN (?,?)", acctA, acctB) })

	fx.ana, fx.bob = newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `member` (`id`,`account_uuid`,`team_uuid`,`key`,`display_name`,`role`,`status`) VALUES (?,?,?,?,?,?,?)",
		fx.ana, acctA, fx.teamID, "M-1", "Ana", 1, 1)
	mustExec(t, db, "INSERT INTO `member` (`id`,`account_uuid`,`team_uuid`,`key`,`display_name`,`role`,`status`) VALUES (?,?,?,?,?,?,?)",
		fx.bob, acctB, fx.teamID, "M-2", "Bob", 2, 1)

	agentA, agentB := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_kind`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?)",
		agentA, acctA, "A-1", "backend", "claude-code", "client-shop-a", 1)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_kind`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?)",
		agentB, acctB, "A-2", "ui", "claude-code", "client-shop-b", 1)

	fx.project = newUUID(t)
	mustExec(t, db, "INSERT INTO `project` (`id`,`team_uuid`,`key`,`name`,`status`) VALUES (?,?,?,?,?)",
		fx.project, fx.teamID, str(t, d92, "project_key"), "Shop", 1)

	// S-17 is Ana's (the decider), S-22 is Bob's (the plan owner).
	s17, s22 := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`status`,`started_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,NOW(),NOW())", s17, fx.teamID, fx.project, agentA, fx.ana, "S-17", 1)
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`status`,`started_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,NOW(),NOW())", s22, fx.teamID, fx.project, agentB, fx.bob, "S-22", 1)

	// The two decisions of §9.2 and §9.3, plus an older revoked one so the
	// history has a second page.
	fx.jwt, fx.bearer, fx.older = newUUID(t), newUUID(t), newUUID(t)
	insertDecision(t, db, fx.teamID, fx.project, fx.jwt, d92, fx.ana, enums.DECISION_STATUS_ACCEPTED)
	insertDecision(t, db, fx.teamID, fx.project, fx.bearer, d93, fx.bob, enums.DECISION_STATUS_SUPERSEDED)
	mustExec(t, db, "UPDATE `decision` SET `supersedes_uuid` = ? WHERE `id` = ?", fx.bearer, fx.jwt)
	mustExec(t, db, "UPDATE `decision` SET `superseded_by_uuid` = ? WHERE `id` = ?", fx.jwt, fx.bearer)
	mustExec(t, db, "INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`status`,`always_show`,"+
		"`revision`,`decided_by_member_uuid`,`decided_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		fx.older, fx.teamID, fx.project, "#auth-session-in-redis", "Sessions live in redis",
		"Every session is a row in redis keyed by its id, with a sliding expiry of one hour.",
		enums.DECISION_STATUS_REVOKED, 0, 1, fx.bob,
		sqlTime(t, "2026-09-15T09:00:00Z"), sqlTime(t, "2026-09-15T10:00:00Z"))
	// A second decision revoked in the SAME second. Two rows that tie on the
	// sort column are what the cursor's key tiebreak is for: page them one at
	// a time and a tiebreak that runs the other way loses the row whose key
	// sorts first, silently, with no gap anywhere else in the walk.
	mustExec(t, db, "INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`status`,`always_show`,"+
		"`revision`,`decided_by_member_uuid`,`decided_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		newUUID(t), fx.teamID, fx.project, "#auth-session-in-cache", "Sessions live in the process cache",
		"A session is held in the API process's own memory cache until it expires.",
		enums.DECISION_STATUS_REVOKED, 0, 1, fx.bob,
		sqlTime(t, "2026-09-15T09:00:00Z"), sqlTime(t, "2026-09-15T10:00:00Z"))

	// The judged counts of §9.2 and §9.3, and two rows that must NOT be
	// counted: one on an older revision of the same decision, one expired.
	judged := d92["judged"].(map[string]any)
	seedJudgements(t, fx.teamID, db, fx.jwt, 2, judged)
	seedJudgements(t, fx.teamID, db, fx.bearer, 1, d93["judged"].(map[string]any))
	insertJudgement(t, db, fx.teamID, fx.jwt, 1, enums.JUDGEMENT_STATUS_JUDGED, enums.JUDGEMENT_VERDICT_NO_CONFLICT)
	insertJudgement(t, db, fx.teamID, fx.jwt, 2, enums.JUDGEMENT_STATUS_EXPIRED, enums.JUDGEMENT_VERDICT_INVALID)

	// CF-31, open and escalated: the conflict of §9.4, seeded from its own
	// example. CF-30 is the same decision settled (it must not be listed as
	// open), and CF-29 is a path overlap (a different kind entirely).
	fx.cf31 = insertDecisionConflict(t, db, fx, str(t, fx.spec94, "key"), enums.CONFLICT_STATUS_OPEN)
	insertDecisionConflict(t, db, fx, "CF-30", enums.CONFLICT_STATUS_RESOLVED)
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`evidence`,`occurrence_count`,`first_detected_at`,`last_detected_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,NOW(),NOW())",
		newUUID(t), fx.teamID, fx.project, "CF-29", enums.CONFLICT_KIND_PATH_OVERLAP, "dedupe-cf29",
		enums.CONFLICT_SEVERITY_MEDIUM, enums.CONFLICT_STATUS_OPEN, enums.DETECTED_BY_SERVER,
		`{"overlap_path":"web/src/auth/session.ts","a_pattern":"web/**","b_pattern":"web/src/auth/session.ts"}`, 1)

	// The event log's account of the decision: the revision of §9.3, an older
	// wording before it, and one event on the OTHER decision that ?key= must
	// not serve.
	rev := firstOf(t, fx.spec93, "revisions")
	insertDecisionEvent(t, db, fx.teamID, s17, int64(rev["sequence"].(float64)), enums.EVENT_KIND_DECISION_RECORDED,
		str(t, d92, "key"), str(t, rev, "summary"), str(t, rev, "statement"), str(t, rev, "occurred_at"))
	insertDecisionEvent(t, db, fx.teamID, s17, 380, enums.EVENT_KIND_DECISION_RECORDED,
		str(t, d92, "key"), "backend recorded #auth-jwt-cookie", "Sessions are a bearer JWT held in memory.",
		"2026-09-15T11:19:00Z")
	insertDecisionEvent(t, db, fx.teamID, s17, 401, enums.EVENT_KIND_DECISION_SUPERSEDED,
		str(t, d93, "key"), "backend superseded #auth-bearer-header", "", "2026-09-15T13:02:11Z")
	return fx
}

// insertDecision writes one decision exactly as a §9 example describes it.
func insertDecision(t *testing.T, db *sql.DB, teamID, project, id string, ex map[string]any, member string, status enums.DecisionStatus) {
	t.Helper()
	var rationale any
	if r, ok := ex["rationale"].(string); ok {
		rationale = r
	}
	alwaysShow := 0
	if ex["always_show"].(bool) {
		alwaysShow = 1
	}
	mustExec(t, db, "INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`rationale`,`status`,"+
		"`always_show`,`revision`,`decided_by_member_uuid`,`decided_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, teamID, project, str(t, ex, "key"), str(t, ex, "title"), str(t, ex, "statement"), rationale,
		status.ToInt64(), alwaysShow, int64(ex["revision"].(float64)), member,
		sqlTime(t, str(t, ex, "decided_at")), sqlTime(t, str(t, ex, "updated_at")))
	for _, p := range ex["scope"].([]any) {
		pattern := p.(string)
		mustExec(t, db, "INSERT INTO `decision_path` (`id`,`decision_uuid`,`team_uuid`,`project_uuid`,`pattern`,`pattern_norm`,`kind`,`prefix`,`depth`) "+
			"VALUES (?,?,?,?,?,?,?,?,?)",
			newUUID(t), id, teamID, project, pattern, pattern, 3, strings.SplitN(pattern, "/", 2)[0]+"/", 2)
	}
}

// seedJudgements writes the rows behind one example's judged counts.
func seedJudgements(t *testing.T, teamID string, db *sql.DB, decisionID string, revision int64, counts map[string]any) {
	t.Helper()
	for _, c := range []struct {
		field   string
		status  enums.JudgementStatus
		verdict enums.JudgementVerdict
	}{
		{"no_conflict", enums.JUDGEMENT_STATUS_JUDGED, enums.JUDGEMENT_VERDICT_NO_CONFLICT},
		{"conflict", enums.JUDGEMENT_STATUS_JUDGED, enums.JUDGEMENT_VERDICT_CONFLICT},
		{"unsure", enums.JUDGEMENT_STATUS_JUDGED, enums.JUDGEMENT_VERDICT_UNSURE},
		{"pending", enums.JUDGEMENT_STATUS_PENDING, enums.JUDGEMENT_VERDICT_INVALID},
	} {
		for i := int64(0); i < int64(counts[c.field].(float64)); i++ {
			insertJudgement(t, db, teamID, decisionID, revision, c.status, c.verdict)
		}
	}
}

func insertJudgement(t *testing.T, db *sql.DB, teamID, decisionID string, revision int64,
	status enums.JudgementStatus, verdict enums.JudgementVerdict) {
	t.Helper()
	id := newUUID(t)
	var verdictArg any
	if verdict != enums.JUDGEMENT_VERDICT_INVALID {
		verdictArg = verdict.ToInt64()
	}
	mustExec(t, db, "INSERT INTO `judgement` (`id`,`team_uuid`,`pair_key`,`kind`,`subject_a_kind`,`subject_a_uuid`,`subject_a_revision`,"+
		"`subject_b_kind`,`subject_b_uuid`,`subject_b_revision`,`status`,`verdict`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		id, teamID, "pair-"+id[:18], enums.CONFLICT_KIND_DECISION_CONTRADICTION,
		enums.SUBJECT_KIND_DECISION, decisionID, revision,
		enums.SUBJECT_KIND_INTENT, newUUID(t), 1, status.ToInt64(), verdictArg)
}

// insertDecisionConflict writes the conflict of §9.4 under a given key and
// status, with its two participants.
func insertDecisionConflict(t *testing.T, db *sql.DB, fx shopFixture, key string, status enums.ConflictStatus) string {
	t.Helper()
	ex := fx.spec94
	paths := ex["paths"].([]any)
	evidence, err := json.Marshal(map[string]any{
		"overlap_path": str(t, ex, "decision_key"),
		"a_pattern":    paths[1].(string), // the decision's scope
		"b_pattern":    paths[0].(string), // the plan's path
		"a_label":      "decision: #auth-jwt-cookie (Ana)",
		"a_summary":    "Sessions are a signed JWT in an httpOnly, Secure cookie.",
		"b_label":      "Bob · ui",
		"b_summary":    "store the session token in localStorage after login",
		"field_issues": []string{str(t, ex, "judge_note")},
		"detail":       str(t, ex, "detector_rule"),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := newUUID(t)
	var resolution, note, resolvedAt any
	if status == enums.CONFLICT_STATUS_RESOLVED {
		resolution, note = enums.CONFLICT_RESOLUTION_SUPERSEDED, "test: settled "+key
		resolvedAt = sqlTime(t, "2026-09-15T13:50:00Z")
	}
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`detector_rule`,`suggested_action`,`evidence`,`resolution`,`resolution_note`,`resolved_at`,`occurrence_count`,"+
		"`first_detected_at`,`last_detected_at`,`escalated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, fx.teamID, fx.project, key, enums.CONFLICT_KIND_DECISION_CONTRADICTION, "dedupe-"+id[:12],
		enums.ConflictSeverityFromString(str(t, ex, "severity")).ToInt64(), status.ToInt64(),
		enums.DetectedByFromString(str(t, ex, "detected_by")).ToInt64(),
		str(t, ex, "detector_rule"), str(t, ex, "suggested_action"), string(evidence),
		resolution, note, resolvedAt, int64(ex["occurrence_count"].(float64)),
		sqlTime(t, str(t, ex, "first_detected_at")), sqlTime(t, str(t, ex, "last_detected_at")),
		sqlTime(t, str(t, ex, "escalated_at")))

	for _, p := range ex["participants"].([]any) {
		pw := p.(map[string]any)
		member, session := fx.bob, "S-22"
		if str(t, pw, "member_name") == "Ana" {
			member, session = fx.ana, "S-17"
		}
		var sessionUUID, agentUUID string
		if err := db.QueryRow("SELECT `id`, `agent_uuid` FROM `session` WHERE `team_uuid` = ? AND `key` = ?",
			fx.teamID, session).Scan(&sessionUUID, &agentUUID); err != nil {
			t.Fatalf("session %s: %v", session, err)
		}
		mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
			"`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
			newUUID(t), id, fx.teamID, sessionUUID, agentUUID, member,
			enums.SubjectKindFromString(str(t, pw, "subject_kind")).ToInt64(), newUUID(t),
			enums.ParticipantRoleFromString(str(t, pw, "role")).ToInt64())
	}
	return id
}

func insertDecisionEvent(t *testing.T, db *sql.DB, teamID, session string, sequence int64, kind enums.EventKind,
	subjectKey, summary, message, occurredAt string) {
	t.Helper()
	payload := `{}`
	if message != "" {
		raw, err := json.Marshal(map[string]string{"message": message})
		if err != nil {
			t.Fatal(err)
		}
		payload = string(raw)
	}
	mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`session_uuid`,`sequence`,`kind`,`structural`,`subject_kind`,"+
		"`subject_key`,`summary`,`payload`,`idempotency_key`,`occurred_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		newUUID(t), teamID, session, sequence, kind.ToInt64(), 1, enums.SUBJECT_KIND_DECISION,
		subjectKey, summary, payload, fmt.Sprintf("seed-decision-%d-%s", sequence, teamID[:8]), sqlTime(t, occurredAt))
}

// ---------------------------------------------------------------- §9.2

// TestDecisionsPayloadIsTheSpecExample serves GET /decisions for the team the
// §9.2 example describes and asserts the response IS that example — the whole
// document, not a field at a time. This is the test that says the already
// merged board frontend will render what this backend sends.
func TestDecisionsPayloadIsTheSpecExample(t *testing.T) {
	db := testDB(t)
	fx := seedShopHack(t, db)
	srv := newBoardServer(t, db)

	var got map[string]any
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions", &got)
	same(t, "GET /decisions", got, fx.spec92)
	assertNoSecrets(t, raw)
	t.Logf("GET /decisions is byte-for-byte the §9.2 example (%d bytes)", len(raw))

	// The superseded and the revoked decisions are not on it — #auth-bearer-header
	// appears only as what this one supersedes — and the counts ignore the
	// older revision and the expired pair.
	for _, gone := range []string{`"key":"#auth-bearer-header"`, `"key":"#auth-session-in-redis"`} {
		if strings.Contains(raw, gone) {
			t.Fatalf("a decision that is no longer in force is on /decisions: %s", raw)
		}
	}
}

// TestDecisionsCountEachPlansLatestVerdict: judged counts one entry per plan,
// its latest judgement on the decision's current revision (§5.1), not every
// verdict it was ever given. Each decision below is one case, all served by
// one GET.
func TestDecisionsCountEachPlansLatestVerdict(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	ids := fixtureIDsFor(t, db, fx.teamID)
	srv := newBoardServer(t, db)

	decision := func(key string, revision int64) string {
		id := newUUID(t)
		mustExec(t, db, "INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`status`,"+
			"`always_show`,`revision`,`decided_by_member_uuid`,`decided_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,NOW(),NOW())",
			id, fx.teamID, ids.project, key, "test "+key, "test statement for "+key,
			enums.DECISION_STATUS_ACCEPTED, 0, revision, ids.ana)
		return id
	}
	judge := func(decisionID string, decisionRev int64, plan string, planRev int64,
		status enums.JudgementStatus, verdict enums.JudgementVerdict) {
		insertPlanJudgement(t, db, fx.teamID, decisionID, decisionRev, plan, planRev, status, verdict)
	}
	const (
		judged  = enums.JUDGEMENT_STATUS_JUDGED
		pending = enums.JUDGEMENT_STATUS_PENDING
		expired = enums.JUDGEMENT_STATUS_EXPIRED
		none    = enums.JUDGEMENT_VERDICT_INVALID
	)

	// A plan judged conflict, then revised (a new pair key) and judged
	// no_conflict: the contradiction is settled, so it counts once, as
	// no_conflict. The newer row is written FIRST, so insertion order cannot
	// be what picks it. The decision is at revision 3; a verdict on its
	// revision 2 is about wording it no longer has and is not counted at all.
	rejudged, plan := decision("#test-rejudged", 3), newUUID(t)
	judge(rejudged, 3, plan, 2, judged, enums.JUDGEMENT_VERDICT_NO_CONFLICT)
	judge(rejudged, 3, plan, 1, judged, enums.JUDGEMENT_VERDICT_CONFLICT)
	judge(rejudged, 2, newUUID(t), 1, judged, enums.JUDGEMENT_VERDICT_CONFLICT)

	// Two plans, each judged once: two entries.
	twoPlans := decision("#test-two-plans", 1)
	judge(twoPlans, 1, newUUID(t), 1, judged, enums.JUDGEMENT_VERDICT_NO_CONFLICT)
	judge(twoPlans, 1, newUUID(t), 4, judged, enums.JUDGEMENT_VERDICT_NO_CONFLICT)

	// A plan judged conflict, revised, and not yet re-judged: it is waiting
	// on its new wording, not still in conflict.
	recheck, plan := decision("#test-recheck", 1), newUUID(t)
	judge(recheck, 1, plan, 1, judged, enums.JUDGEMENT_VERDICT_CONFLICT)
	judge(recheck, 1, plan, 2, pending, none)

	// Expired rows are never counted and never the latest. A plan whose only
	// pair expired counts nothing; a plan whose re-check expired unanswered
	// still counts by the verdict it was last given.
	lapsed := decision("#test-expired", 1)
	judge(lapsed, 1, newUUID(t), 1, expired, none)
	plan = newUUID(t)
	judge(lapsed, 1, plan, 1, judged, enums.JUDGEMENT_VERDICT_UNSURE)
	judge(lapsed, 1, plan, 2, expired, none)

	var out decisionsResponse
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions", &out)
	got := map[string]decisionJudgedWire{}
	for _, d := range out.Decisions {
		got[d.Key] = d.Judged
	}
	for key, want := range map[string]decisionJudgedWire{
		"#test-rejudged":  {NoConflict: 1},
		"#test-two-plans": {NoConflict: 2},
		"#test-recheck":   {Pending: 1},
		"#test-expired":   {Unsure: 1},
	} {
		g, ok := got[key]
		if !ok {
			t.Fatalf("%s is not on /decisions: %s", key, raw)
		}
		if g != want {
			t.Errorf("%s judged = %+v, want %+v", key, g, want)
		} else {
			t.Logf("%s judged = %+v", key, g)
		}
	}
}

// insertPlanJudgement writes one judgement row for a named plan at a named
// plan revision. The pair key carries both revisions, as the reviewer's does.
func insertPlanJudgement(t *testing.T, db *sql.DB, teamID, decisionID string, decisionRev int64, plan string, planRev int64,
	status enums.JudgementStatus, verdict enums.JudgementVerdict) {
	t.Helper()
	var verdictArg any
	if verdict != enums.JUDGEMENT_VERDICT_INVALID {
		verdictArg = verdict.ToInt64()
	}
	mustExec(t, db, "INSERT INTO `judgement` (`id`,`team_uuid`,`pair_key`,`kind`,`subject_a_kind`,`subject_a_uuid`,`subject_a_revision`,"+
		"`subject_b_kind`,`subject_b_uuid`,`subject_b_revision`,`status`,`verdict`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		newUUID(t), teamID, fmt.Sprintf("pair-%s-%d-%s-%d", decisionID[:8], decisionRev, plan[:8], planRev),
		enums.CONFLICT_KIND_DECISION_CONTRADICTION, enums.SUBJECT_KIND_DECISION, decisionID, decisionRev,
		enums.SUBJECT_KIND_INTENT, plan, planRev, status.ToInt64(), verdictArg)
}

// TestDecisionsListsAlwaysShowFirstAndClipsTheRationale: the order the tab
// draws, and what happens to a rationale an agent wrote too long, with a
// credential in it.
func TestDecisionsListsAlwaysShowFirstAndClipsTheRationale(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	ids := fixtureIDsFor(t, db, fx.teamID)
	srv := newBoardServer(t, db)

	long := "why: " + strings.Repeat("a decision needs a reason, ", 60) + "token=" + canaryToken + " end"
	newer := newUUID(t)
	mustExec(t, db, "INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`rationale`,`status`,"+
		"`always_show`,`revision`,`decided_by_member_uuid`,`decided_at`,`updated_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,NOW(),DATE_ADD(NOW(), INTERVAL 1 HOUR))",
		newer, fx.teamID, ids.project, "#errors-are-problem-json", "Errors are problem+json",
		"Every error response is application/problem+json with type, title, status and detail.",
		long, enums.DECISION_STATUS_ACCEPTED, 0, 1, ids.bob)
	// Newer than both, and NOT accepted: it must not be served at all.
	mustExec(t, db, "INSERT INTO `decision` (`id`,`team_uuid`,`project_uuid`,`key`,`title`,`statement`,`status`,"+
		"`always_show`,`revision`,`decided_by_member_uuid`,`decided_at`,`updated_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,NOW(),DATE_ADD(NOW(), INTERVAL 2 HOUR))",
		newUUID(t), fx.teamID, ids.project, "#revoked-one", "Revoked", "This one was withdrawn and must not be served.",
		enums.DECISION_STATUS_REVOKED, 1, 1, ids.bob)

	var out decisionsResponse
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions", &out)
	keys := []string{}
	for _, d := range out.Decisions {
		keys = append(keys, d.Key)
	}
	// always_show first, whatever the times say; then updated_at desc.
	if strings.Join(keys, ",") != "#auth-jwt-cookie,#errors-are-problem-json" {
		t.Fatalf("order = %v, want the always-show decision first and the revoked one absent", keys)
	}
	clipped := out.Decisions[1].Rationale
	// At most 600 runes — one short when the cut landed on a space, which the
	// clip trims before it marks the cut.
	if n := len([]rune(clipped)); n > maxDecisionRationale || n < maxDecisionRationale-1 {
		t.Fatalf("rationale is %d runes, want %d", n, maxDecisionRationale)
	}
	if len([]rune(long)) <= maxDecisionRationale {
		t.Fatalf("the seeded rationale is only %d runes, so nothing was clipped", len([]rune(long)))
	}
	if !strings.HasSuffix(clipped, "…") {
		t.Fatalf("a clipped rationale does not say it was cut: %q", clipped[len(clipped)-20:])
	}
	if strings.Contains(raw, canaryToken) {
		t.Fatalf("the rationale carried a credential-shaped string: %s", raw)
	}
	assertNoSecrets(t, raw, canaryToken)
	t.Logf("rationale clipped to %d runes and masked; order %v", maxDecisionRationale, keys)
}

// ---------------------------------------------------------------- §9.3

type decisionHistoryPage struct {
	Status     string                 `json:"status"`
	Statuses   []string               `json:"statuses"`
	Decisions  []decisionWire         `json:"decisions"`
	NextCursor string                 `json:"next_cursor"`
	Revisions  []decisionRevisionWire `json:"revisions"`
}

// walkDecisionHistory pages through the decision history and returns every row
// in the order served, plus every raw body.
func walkDecisionHistory(t *testing.T, base, slug string, filters url.Values, limit int) ([]decisionWire, []string) {
	t.Helper()
	var (
		all    []decisionWire
		bodies []string
		cursor string
	)
	want := pageSize(limit, defaultDecisionHistoryPage, maxDecisionHistoryPage)
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("more than 50 pages: the cursor is not advancing")
		}
		q := url.Values{}
		for k, v := range filters {
			q[k] = v
		}
		if limit > 0 {
			q.Set("limit", fmt.Sprint(limit))
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var p decisionHistoryPage
		bodies = append(bodies, getJSON(t, base+"/v1/teams/"+slug+"/decisions/history?"+q.Encode(), &p))
		if len(p.Decisions) > want {
			t.Fatalf("page %d has %d rows, over its limit %d", page, len(p.Decisions), want)
		}
		if p.NextCursor != "" && len(p.Decisions) != want {
			t.Fatalf("page %d has %d rows and a next cursor; only a full page may have one", page, len(p.Decisions))
		}
		if len(p.Revisions) != 0 {
			t.Fatalf("page %d carries revisions without ?key=: %+v", page, p.Revisions)
		}
		all = append(all, p.Decisions...)
		if p.NextCursor == "" {
			return all, bodies
		}
		cursor = p.NextCursor
	}
}

// TestDecisionHistoryPagesTheSpecExample walks the past decisions two pages at
// a time and asserts the first page — its row and its cursor — is the §9.3
// example, then that ?key= answers with that decision's revisions.
func TestDecisionHistoryPagesTheSpecExample(t *testing.T) {
	db := testDB(t)
	fx := seedShopHack(t, db)
	srv := newBoardServer(t, db)

	// Page one of one row is exactly §9.3, cursor included.
	var first map[string]any
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions/history?limit=1", &first)
	want := map[string]any{}
	for k, v := range fx.spec93 {
		if k == "revisions" { // a real response carries the cursor or the revisions, never both
			continue
		}
		want[k] = v
	}
	same(t, "GET /decisions/history?limit=1", first, want)
	assertNoSecrets(t, raw)
	t.Logf("page 1 is the §9.3 example, next_cursor %q", first["next_cursor"])

	// Two pages, no duplicates and no gaps, in the table's own order.
	ordered := queryStrings(t, db,
		"SELECT `key` FROM `decision` WHERE `team_uuid` = ? AND `status` IN (?,?) ORDER BY `updated_at` DESC, `key`",
		fx.teamID, enums.DECISION_STATUS_SUPERSEDED, enums.DECISION_STATUS_REVOKED)
	if len(ordered) != 3 {
		t.Fatalf("fixture has %d past decisions, want 3 (two of them tied on updated_at)", len(ordered))
	}
	got, bodies := walkDecisionHistory(t, srv.URL, fx.slug, nil, 1)
	keys := []string{}
	seen := map[string]bool{}
	for _, d := range got {
		if seen[d.Key] {
			t.Fatalf("%s was served twice", d.Key)
		}
		seen[d.Key] = true
		keys = append(keys, d.Key)
		if d.Status == "accepted" {
			t.Fatalf("an accepted decision is in the history: %+v", d)
		}
		if d.EndedAt == nil || d.UpdatedAt == nil || *d.EndedAt != *d.UpdatedAt {
			t.Fatalf("%s: ended_at %v is not its updated_at %v", d.Key, d.EndedAt, d.UpdatedAt)
		}
	}
	if strings.Join(keys, ",") != strings.Join(ordered, ",") {
		t.Fatalf("paged %v, want %v", keys, ordered)
	}
	t.Logf("limit=1 walked %d pages over %d past decisions: %v", len(bodies), len(keys), keys)

	// The status filter, against the table.
	for _, c := range []struct{ status, want string }{
		{"superseded", "#auth-bearer-header"},
		// The two revoked ones tie on updated_at; the key breaks the tie
		// ascending, in the page and across the cursor alike.
		{"revoked", "#auth-session-in-cache,#auth-session-in-redis"},
		{"all", "#auth-bearer-header,#auth-session-in-cache,#auth-session-in-redis"},
	} {
		rows, _ := walkDecisionHistory(t, srv.URL, fx.slug, url.Values{"status": {c.status}}, 10)
		list := []string{}
		for _, d := range rows {
			list = append(list, d.Key)
		}
		if strings.Join(list, ",") != c.want {
			t.Fatalf("status=%s served %v, want %s", c.status, list, c.want)
		}
		t.Logf("status=%-11q -> %v", c.status, list)
	}

	// ?key= is one decision in any status, with its wordings, newest first.
	var one decisionHistoryPage
	rawKey := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions/history?key="+url.QueryEscape("#auth-jwt-cookie"), &one)
	if len(one.Decisions) != 1 || one.Decisions[0].Key != "#auth-jwt-cookie" || one.Decisions[0].Status != "accepted" ||
		one.Decisions[0].EndedAt != nil || one.NextCursor != "" {
		t.Fatalf("?key= served %+v", one)
	}
	var keyed map[string]any
	if err := json.Unmarshal([]byte(rawKey), &keyed); err != nil {
		t.Fatal(err)
	}
	same(t, "?key= revisions", keyed["revisions"].([]any)[0], fx.spec93["revisions"].([]any)[0])
	if len(one.Revisions) != 2 || one.Revisions[0].Sequence != 402 || one.Revisions[1].Sequence != 380 {
		t.Fatalf("revisions = %+v, want 402 then 380", one.Revisions)
	}
	// The key normalizes, and it filters: the other decision's event is not here.
	var bare decisionHistoryPage
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions/history?key=auth-jwt-cookie", &bare)
	if len(bare.Revisions) != 2 || len(bare.Decisions) != 1 {
		t.Fatalf("a key without its # served %+v", bare)
	}
	// A key this team never had is an empty answer, not a 404.
	var none decisionHistoryPage
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions/history?key="+url.QueryEscape("#never-recorded"), &none)
	if len(none.Decisions) != 0 || len(none.Revisions) != 0 {
		t.Fatalf("an unknown key served %+v", none)
	}
	assertNoSecrets(t, rawKey)
	t.Logf("?key= served 1 decision and %d revisions; an unknown key served none", len(one.Revisions))

	// What the endpoint refuses.
	// A spelling record_decision would accept is accepted here too (the space
	// case above), so what is refused is what is not a key at all.
	for _, bad := range []string{"status=accepted", "status=nope", "key=" + url.QueryEscape("!!!"),
		"key=" + url.QueryEscape("#Nope!"), "key=" + url.QueryEscape("#"+strings.Repeat("a", 61)),
		"cursor=!!!", "cursor=" + url.QueryEscape(strings.Repeat("A", 300)),
		"cursor=czF8MjAyNi0wMS0wMSAwMDowMDowMHxTLTE"} {
		resp, err := http.Get(srv.URL + "/v1/teams/" + fx.slug + "/decisions/history?" + bad)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", bad, resp.StatusCode)
		}
	}
	t.Log("bad status, bad key and a cursor this endpoint never issued are all 400")
}

// ---------------------------------------------------------------- §9.4

// TestDecisionConflictWireIsTheSpecExample: the open conflict, then the same
// conflict once the agents settled it.
func TestDecisionConflictWireIsTheSpecExample(t *testing.T) {
	db := testDB(t)
	fx := seedShopHack(t, db)
	srv := newBoardServer(t, db)

	find := func(raw string, key string) map[string]any {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		for _, c := range body["conflicts"].([]any) {
			cw := c.(map[string]any)
			if cw["key"] == key {
				return cw
			}
		}
		t.Fatalf("%s is not in %s", key, raw)
		return nil
	}

	open := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts", nil)
	same(t, "the open decision conflict", find(open, "CF-31"), fx.spec94)
	assertNoSecrets(t, open)
	t.Log("GET /conflicts serves §9.4's open conflict verbatim: decision_key, judge_note, escalated_at, paths")

	// The evidence's intent summary and labels are NOT on the wire, and the
	// decision key never leaks into paths.
	if strings.Contains(open, "store the session token in localStorage after login") || strings.Contains(open, "b_label") {
		t.Fatalf("the detector's evidence reached the response: %s", open)
	}
	cf29 := find(open, "CF-29")
	if _, ok := cf29["decision_key"]; ok {
		t.Fatalf("a path overlap carries decision_key: %v", cf29)
	}
	// A path overlap still reads its overlap_path into paths, first, and the
	// duplicate b_pattern is folded away as before.
	if fmt.Sprint(cf29["paths"]) != "[web/src/auth/session.ts web/**]" {
		t.Fatalf("a path overlap's paths changed: %v", cf29["paths"])
	}

	// Settled by the agents: the same object, plus how it ended.
	mustExec(t, db, "UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolution_note` = ?, `resolved_at` = ? WHERE `id` = ?",
		enums.CONFLICT_STATUS_RESOLVED, enums.CONFLICT_RESOLUTION_CONVERGED,
		fx.converged, sqlTime(t, "2026-09-15T14:15:02Z"), fx.cf31)

	want := map[string]any{}
	for k, v := range fx.spec94 {
		want[k] = v
	}
	want["status"] = "resolved"
	want["resolution"] = "converged"
	want["resolved_at"] = "2026-09-15T14:15:02Z"
	want["resolution_note"] = fx.converged

	all := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts?status=all", nil)
	same(t, "the settled decision conflict", find(all, "CF-31"), want)
	t.Log("settled, the same conflict carries converged, resolved_at and the §4.8 note, and keeps escalated_at")

	// It is out of the open list, and off the decision's card.
	stillOpen := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts", nil)
	if strings.Contains(stillOpen, `"CF-31"`) {
		t.Fatalf("a settled conflict is still open: %s", stillOpen)
	}
	var decisions decisionsResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/decisions", &decisions)
	if len(decisions.Decisions) != 1 || len(decisions.Decisions[0].OpenConflicts) != 0 {
		t.Fatalf("open_conflicts = %+v, want empty once CF-31 settled", decisions.Decisions[0].OpenConflicts)
	}
	t.Log("open_conflicts drops the conflict as soon as it is settled")
}

// TestConflictHistoryTakesTheDecisionKind: the new kind is a filter the
// history offers and serves.
func TestConflictHistoryTakesTheDecisionKind(t *testing.T) {
	db := testDB(t)
	fx := seedShopHack(t, db)
	srv := newBoardServer(t, db)

	var page conflictHistoryPage
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts/history?kind=decision_contradiction", &page)
	if strings.Join(page.Kinds, ",") != "path_overlap,contract_mismatch,contract_unclaimed,contract_naming_variant,decision_contradiction" {
		t.Fatalf("the kind vocabulary is %v", page.Kinds)
	}
	if len(page.Conflicts) != 1 || page.Conflicts[0].Key != "CF-30" || page.Conflicts[0].Kind != "decision_contradiction" {
		t.Fatalf("kind=decision_contradiction served %+v", page.Conflicts)
	}
	c := page.Conflicts[0]
	if c.DecisionKey != "#auth-jwt-cookie" || c.JudgeNote == "" || c.EscalatedAt == nil ||
		fmt.Sprint(c.Paths) != "[web/src/auth/session.ts web/src/auth/**]" {
		t.Fatalf("a past decision conflict = %+v", c)
	}
	assertNoSecrets(t, raw)
	t.Logf("conflicts/history?kind=decision_contradiction -> %s with its decision key and judge note", c.Key)
}

// ---------------------------------------------------------------- the gate

// decisionRoutes are the reads this work adds, with their parameters.
var decisionRoutes = []string{
	"/decisions", "/decisions?limit=5",
	"/decisions/history", "/decisions/history?status=revoked&limit=2",
	"/decisions/history?key=%23auth-jwt-cookie",
}

var badDecisionRoutes = []string{"/decisions/history?cursor=garbage", "/decisions/history?status=accepted",
	"/decisions/history?key=%21%21%21"}

// TestDecisionReadsGateOnAPrivateTeam: a member reads them; a non-member, an
// anonymous request and a garbage session get the unknown team's bytes — the
// same 404, byte for byte, for a good route and a bad parameter alike.
func TestDecisionReadsGateOnAPrivateTeam(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	for name, h := range map[string]map[string]string{
		"owner session":  session(fx.ownerSession),
		"member session": session(fx.memberSession),
		"member bearer":  {"Authorization": "Bearer " + fx.memberToken},
	} {
		for _, p := range decisionRoutes {
			got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h)
			if got.code != http.StatusOK || !json.Valid([]byte(got.body)) {
				t.Fatalf("%s %s: got %d, want 200 (%.200s)", name, p, got.code, got.body)
			}
			assertNoSecrets(t, got.body, fx.ownerSession, fx.memberSession, fx.memberToken, fx.memberTokenHash)
		}
		for _, p := range badDecisionRoutes {
			if got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h); got.code != http.StatusBadRequest {
				t.Fatalf("%s %s: got %d, want 400", name, p, got.code)
			}
		}
		t.Logf("%-15s -> 200 on %d decision reads, 400 on %d bad parameters", name, len(decisionRoutes), len(badDecisionRoutes))
	}

	const unknown = "no-such-team-7q"
	for _, p := range append(append([]string{}, decisionRoutes...), badDecisionRoutes...) {
		want := fetchAll(t, srv.URL+"/v1/teams/"+unknown+p, nil)
		if want.code != http.StatusNotFound {
			t.Fatalf("unknown slug %s: got %d, want 404", p, want.code)
		}
		for name, h := range map[string]map[string]string{
			"anonymous":          nil,
			"non-member session": session(fx.outsiderSession),
			"non-member bearer":  {"Authorization": "Bearer " + fx.outsiderToken},
			"garbage session":    session("mbs_" + strings.Repeat("B", 43)),
		} {
			got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h)
			if got != want {
				t.Fatalf("%s, %s: distinguishable from an unknown team:\n got  %d %q %q\n want %d %q %q",
					p, name, got.code, got.contentType, got.body, want.code, want.contentType, want.body)
			}
		}
		t.Logf("%-46s anonymous, non-member session/bearer, garbage session -> %d %q identical to unknown team", p, want.code, want.body)
	}
}
