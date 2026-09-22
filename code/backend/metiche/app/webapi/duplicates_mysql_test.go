package webapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mklfarha/metiche/backend/enums"
)

// The duplicate_work half of the board's read API over a real MySQL: the
// three fields a duplicate conflict adds to every conflict wire (plans,
// signals, issue_ref), the judge's note and escalated_at on it, and the
// history's kind filter (docs/DUPLICATES.md §5.1, §9.3, §9.4). Same DSN rule
// as the rest of this package (see dsnEnv). Names are Ana, Bob and test.
//
// As with the decision tests, the expectations are not retyped here. They are
// §9.3's example and §9.4's evidence as code/frontend/internal/feed's test
// data holds them: the board frontend is built and merged against those
// bytes, and that file's own TestDuplicateSpecExamplesAreVerbatim pins them
// to docs/DUPLICATES.md. The conflict row is seeded with §9.4's evidence
// exactly as the spec prints it, served through the real handlers, and the
// response must be §9.3's example.

// dupSpecFile is the frontend's frozen copy of the §9.3 and §9.4 examples.
var dupSpecFile = filepath.Join("..", "..", "..", "..", "frontend", "internal", "feed", "conflicts_wire_test.go")

func dupSpecSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(dupSpecFile)
	if err != nil {
		t.Skipf("skipped: the frontend's frozen §9.3 examples are not beside this checkout: %v", err)
	}
	return string(raw)
}

// dupSpecExample returns the backquoted const of that name from the
// frontend's conflict wire test data.
func dupSpecExample(t *testing.T, name string) string {
	t.Helper()
	src := dupSpecSource(t)
	marker := "const " + name + " = `"
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("%s is no longer in %s; the frozen example moved", name, dupSpecFile)
	}
	rest := src[i+len(marker):]
	j := strings.Index(rest, "`")
	if j < 0 {
		t.Fatalf("%s is not terminated in %s", name, dupSpecFile)
	}
	return rest[:j]
}

// dupSpecQuoted returns a plain quoted const from the same file.
func dupSpecQuoted(t *testing.T, name string) string {
	t.Helper()
	for _, line := range strings.Split(dupSpecSource(t), "\n") {
		if !strings.HasPrefix(line, "const "+name+" = ") {
			continue
		}
		s, err := strconv.Unquote(strings.TrimPrefix(line, "const "+name+" = "))
		if err != nil {
			t.Fatalf("%s is not a quoted string in %s: %v", name, dupSpecFile, err)
		}
		return s
	}
	t.Fatalf("%s is no longer in %s", name, dupSpecFile)
	return ""
}

// ---------------------------------------------------------------- fixture

type dupFixture struct {
	teamID, slug string
	project      string
	cf44         string // the duplicate conflict of §9.3
	s17, s22     string // session uuids: Ana's (ui, the incumbent) and Bob's (frontend, the judge)
	ana, bob     string // member uuids
	agentA       string
	agentB       string
	example      string         // §9.3's example, raw
	spec93       map[string]any // the same, decoded
	evidence     string         // §9.4, raw
	yielded      string         // §9.3's settled resolution_note
}

// seedDupHack builds the team §9.3 describes: Ana's agent "ui" on S-17
// holding INT-83, Bob's agent "frontend" on S-22 holding INT-92, and CF-44
// open with §9.4's evidence and only the initiator attached, which is how
// report_judgement leaves it before the incumbent is told. A path overlap
// sits beside it, whose evidence also carries labels and summaries that must
// never be served.
func seedDupHack(t *testing.T, db *sql.DB) dupFixture {
	t.Helper()
	fx := dupFixture{
		example:  dupSpecExample(t, "spec93DuplicateConflict"),
		evidence: dupSpecExample(t, "spec94DuplicateEvidence"),
		yielded:  dupSpecQuoted(t, "spec93YieldedNote"),
	}
	fx.spec93 = jsonMap(t, fx.example)
	ex := fx.spec93

	fx.teamID, fx.slug = newUUID(t), "dup-hack-test"
	mustExec(t, db, "DELETE FROM `team` WHERE `slug` = ?", fx.slug)
	mustExec(t, db, "INSERT INTO `team` (`id`,`name`,`slug`,`sequence`,`board_revision`,`status`,`visibility`) VALUES (?,?,?,?,?,?,?)",
		fx.teamID, "Dup Hack", fx.slug, 90, 12, 1, enums.TEAM_VISIBILITY_PUBLIC)
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

	// The labels come out of the example's participants, not retyped.
	labels := map[string]string{}
	for _, p := range ex["participants"].([]any) {
		pw := p.(map[string]any)
		labels[str(t, pw, "member_name")] = str(t, pw, "agent_label")
	}
	fx.agentA, fx.agentB = newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_kind`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?)",
		fx.agentA, acctA, "A-1", labels["Ana"], "claude-code", "client-dup-a", 1)
	mustExec(t, db, "INSERT INTO `agent` (`id`,`account_uuid`,`key`,`label`,`client_kind`,`client_key`,`status`) VALUES (?,?,?,?,?,?,?)",
		fx.agentB, acctB, "A-2", labels["Bob"], "claude-code", "client-dup-b", 1)

	fx.project = newUUID(t)
	mustExec(t, db, "INSERT INTO `project` (`id`,`team_uuid`,`key`,`name`,`status`) VALUES (?,?,?,?,?)",
		fx.project, fx.teamID, "web", "Web", 1)

	fx.s17, fx.s22 = newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`status`,`started_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,NOW(),NOW())", fx.s17, fx.teamID, fx.project, fx.agentA, fx.ana, "S-17", 1)
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`status`,`started_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,NOW(),NOW())", fx.s22, fx.teamID, fx.project, fx.agentB, fx.bob, "S-22", 1)

	plans := ex["plans"].([]any)
	a, b := plans[0].(map[string]any), plans[1].(map[string]any)
	intentA, intentB := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,`revision`,`declared_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,NOW())", intentA, fx.teamID, fx.project, fx.s17, fx.ana, str(t, a, "key"), str(t, a, "summary"), 1, 1, 1)
	mustExec(t, db, "INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,`revision`,`declared_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,NOW())", intentB, fx.teamID, fx.project, fx.s22, fx.bob, str(t, b, "key"), str(t, b, "summary"), 1, 1, 1)

	// CF-44 with §9.4's evidence, byte for byte as the spec prints it. MySQL
	// stores it with its own key order; the read must not care.
	fx.cf44 = newUUID(t)
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`detector_rule`,`suggested_action`,`evidence`,`suggested_yield_session_uuid`,`occurrence_count`,`first_detected_at`,`last_detected_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		fx.cf44, fx.teamID, fx.project, str(t, ex, "key"), enums.CONFLICT_KIND_DUPLICATE_WORK, "dedupe-"+fx.cf44[:12],
		enums.ConflictSeverityFromString(str(t, ex, "severity")).ToInt64(), enums.CONFLICT_STATUS_OPEN,
		enums.DetectedByFromString(str(t, ex, "detected_by")).ToInt64(),
		str(t, ex, "detector_rule"), str(t, ex, "suggested_action"), fx.evidence, fx.s22,
		int64(ex["occurrence_count"].(float64)),
		sqlTime(t, str(t, ex, "first_detected_at")), sqlTime(t, str(t, ex, "last_detected_at")))
	fx.attach(t, db, "S-22", intentB)

	// CF-43: a path overlap whose evidence also carries sides' labels and
	// summaries, as the path detector writes them. None of it may reach the
	// wire, and its paths read exactly as before.
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`evidence`,`occurrence_count`,`first_detected_at`,`last_detected_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,NOW(),NOW())",
		newUUID(t), fx.teamID, fx.project, "CF-43", enums.CONFLICT_KIND_PATH_OVERLAP, "dedupe-cf43-"+fx.teamID[:8],
		enums.CONFLICT_SEVERITY_MEDIUM, enums.CONFLICT_STATUS_OPEN, enums.DETECTED_BY_SERVER,
		`{"overlap_path":"web/src/auth/session.ts","a_pattern":"web/**","b_pattern":"web/src/auth/session.ts",`+
			`"a_label":"test label a","a_summary":"test overlap summary a","b_label":"test label b","b_summary":"test overlap summary b",`+
			`"adjusters":["same_member_concurrent"]}`, 1)
	return fx
}

// attach adds the participant a §9.3 example names by session key.
func (fx dupFixture) attach(t *testing.T, db *sql.DB, sessionKey, subject string) {
	t.Helper()
	for _, p := range fx.spec93["participants"].([]any) {
		pw := p.(map[string]any)
		if str(t, pw, "session_key") != sessionKey {
			continue
		}
		session, agent, member := fx.s22, fx.agentB, fx.bob
		if sessionKey == "S-17" {
			session, agent, member = fx.s17, fx.agentA, fx.ana
		}
		mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
			"`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
			newUUID(t), fx.cf44, fx.teamID, session, agent, member,
			enums.SubjectKindFromString(str(t, pw, "subject_kind")).ToInt64(), subject,
			enums.ParticipantRoleFromString(str(t, pw, "role")).ToInt64())
		return
	}
	t.Fatalf("%s is not a participant of the §9.3 example", sessionKey)
}

// rawConflict returns one conflict object of a response, as the server wrote
// its bytes.
func rawConflict(t *testing.T, body, key string) json.RawMessage {
	t.Helper()
	var page struct {
		Conflicts []json.RawMessage `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode %.200s: %v", body, err)
	}
	for _, raw := range page.Conflicts {
		var k struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(raw, &k); err != nil {
			t.Fatal(err)
		}
		if k.Key == key {
			return raw
		}
	}
	t.Fatalf("%s is not in %.400s", key, body)
	return nil
}

func decoded(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	return jsonMap(t, string(raw))
}

// copyMap is a shallow copy, so one expectation can be derived from another.
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------- §9.3

// TestDuplicateConflictWireIsTheSpecExample walks CF-44 through its life:
// open before the incumbent is told, escalated with the incumbent attached
// (§9.3's example itself, byte for byte), settled by the yield side, and
// with a shared issue id.
func TestDuplicateConflictWireIsTheSpecExample(t *testing.T) {
	db := testDB(t)
	fx := seedDupHack(t, db)
	srv := newBoardServer(t, db)
	base := srv.URL + "/v1/teams/" + fx.slug

	// 1. Open, before the incumbent is told: participants holds only the
	// initiator, nobody has been asked, plans still holds both.
	wantOpen := copyMap(fx.spec93)
	delete(wantOpen, "escalated_at")
	wantOpen["participants"] = fx.spec93["participants"].([]any)[:1]
	body := getJSON(t, base+"/conflicts", nil)
	same(t, "the open duplicate conflict, before the incumbent is told", decoded(t, rawConflict(t, body, "CF-44")), wantOpen)
	assertNoSecrets(t, body)
	t.Log("open: plans [INT-83 incumbent, INT-92 yields], signals from the words fact, judge_note, only the initiator, no escalated_at")

	// 2. The incumbent is told and a person is asked: this IS §9.3's example.
	var intentA string
	if err := db.QueryRow("SELECT `id` FROM `intent` WHERE `team_uuid` = ? AND `session_uuid` = ?", fx.teamID, fx.s17).Scan(&intentA); err != nil {
		t.Fatal(err)
	}
	fx.attach(t, db, "S-17", intentA)
	mustExec(t, db, "UPDATE `conflict` SET `escalated_at` = ? WHERE `id` = ?", sqlTime(t, str(t, fx.spec93, "escalated_at")), fx.cf44)
	body = getJSON(t, base+"/conflicts", nil)
	got := rawConflict(t, body, "CF-44")
	same(t, "the escalated duplicate conflict", decoded(t, got), fx.spec93)
	var gotCompact, wantCompact bytes.Buffer
	if err := json.Compact(&gotCompact, got); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&wantCompact, []byte(fx.example)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCompact.Bytes(), wantCompact.Bytes()) {
		t.Fatalf("the escalated conflict's bytes are not §9.3's, key order included\n got: %s\nwant: %s", gotCompact.Bytes(), wantCompact.Bytes())
	}
	t.Logf("escalated: GET /conflicts serves §9.3's example byte for byte once whitespace is compacted (%d bytes)", gotCompact.Len())

	// The same wire on the session page's run history, for either side.
	for _, key := range []string{"S-22", "S-17"} {
		var page struct {
			History struct {
				Conflicts []json.RawMessage `json:"conflicts"`
			} `json:"history"`
		}
		getJSON(t, base+"/sessions/"+key, &page)
		if len(page.History.Conflicts) != 1 {
			t.Fatalf("%s's run history has %d conflicts, want CF-44 only", key, len(page.History.Conflicts))
		}
		same(t, key+"'s run history conflict", decoded(t, page.History.Conflicts[0]), fx.spec93)
	}
	t.Log("the session page's run history serves the same object for S-22 and S-17")

	// What must not leak: the path overlap's labels and summaries, and any
	// raw evidence key.
	for _, leak := range []string{"test overlap summary", "test label", "a_summary", "b_label", "adjusters", "plans:"} {
		if strings.Contains(body, leak) {
			t.Fatalf("%q reached the response: %s", leak, body)
		}
	}
	cf43 := decoded(t, rawConflict(t, body, "CF-43"))
	for _, k := range []string{"plans", "signals", "issue_ref", "judge_note", "decision_key"} {
		if _, ok := cf43[k]; ok {
			t.Fatalf("a path overlap carries %s: %v", k, cf43)
		}
	}
	if fmt.Sprint(cf43["paths"]) != "[web/src/auth/session.ts web/**]" {
		t.Fatalf("a path overlap's paths changed: %v", cf43["paths"])
	}
	t.Log("a path overlap beside it: no plans, signals, issue_ref or judge_note, same paths, its summaries stay in the evidence")

	// 3. Settled: the yield side marked its plan superseded.
	mustExec(t, db, "UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolution_note` = ?, `resolved_at` = ? WHERE `id` = ?",
		enums.CONFLICT_STATUS_RESOLVED, enums.CONFLICT_RESOLUTION_YIELDED, fx.yielded, sqlTime(t, "2026-09-21T14:15:02Z"), fx.cf44)
	wantSettled := copyMap(fx.spec93)
	wantSettled["status"] = "resolved"
	wantSettled["resolution"] = "yielded"
	wantSettled["resolved_at"] = "2026-09-21T14:15:02Z"
	wantSettled["resolution_note"] = fx.yielded
	all := getJSON(t, base+"/conflicts?status=all", nil)
	same(t, "the settled duplicate conflict", decoded(t, rawConflict(t, all, "CF-44")), wantSettled)
	if strings.Contains(getJSON(t, base+"/conflicts", nil), `"CF-44"`) {
		t.Fatal("a settled duplicate is still in the open list")
	}
	hist := getJSON(t, base+"/conflicts/history", nil)
	same(t, "the settled duplicate in the history", decoded(t, rawConflict(t, hist, "CF-44")), wantSettled)
	t.Log("settled: resolved, yielded, resolved_at and §9.3's note, on /conflicts?status=all and /conflicts/history, off the open list")

	// 4. With a shared issue id: the same_issue fact comes first among the
	// signals, overlap_path is the ref, and it goes to issue_ref, never paths.
	ev := jsonMap(t, fx.evidence)
	ev["overlap_path"] = "ISSUE-412"
	adj := ev["adjusters"].([]any)
	ev["adjusters"] = append([]any{adj[0], "same_issue:ISSUE-412"}, adj[1:]...)
	ev["detail"] = "duplicate_work.same_issue"
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "UPDATE `conflict` SET `evidence` = ?, `detector_rule` = ? WHERE `id` = ?", string(raw), "duplicate_work.same_issue", fx.cf44)
	wantIssue := copyMap(wantSettled)
	wantIssue["detector_rule"] = "duplicate_work.same_issue"
	wantIssue["issue_ref"] = "ISSUE-412"
	wantIssue["signals"] = []any{"same issue ISSUE-412", "shared words: login, screen"}
	all = getJSON(t, base+"/conflicts?status=all", nil)
	same(t, "the duplicate conflict with a shared issue", decoded(t, rawConflict(t, all, "CF-44")), wantIssue)
	t.Log("shared issue: issue_ref ISSUE-412, signals [same issue ISSUE-412, shared words: login, screen], paths unchanged")
}

// TestDuplicateConflictReadsTheOtherFacts: the paths and same-member facts
// render as §9.3 words them, an overlapping path in overlap_path never
// becomes issue_ref, and when the incumbent judged later (the detector
// rewrote the evidence with the sides swapped) the yield side follows the
// plans fact.
func TestDuplicateConflictReadsTheOtherFacts(t *testing.T) {
	db := testDB(t)
	fx := seedDupHack(t, db)
	srv := newBoardServer(t, db)

	ev := jsonMap(t, fx.evidence)
	ev["overlap_path"] = "web/src/auth/**"
	// Swapped: INT-83's side judged last, so it is b now and yields.
	ev["a_label"], ev["b_label"] = ev["b_label"], ev["a_label"]
	ev["a_summary"], ev["b_summary"] = ev["b_summary"], ev["a_summary"]
	ev["a_pattern"], ev["b_pattern"] = ev["b_pattern"], ev["a_pattern"]
	ev["adjusters"] = []any{"plans:INT-92,INT-83", "words:login,screen", "paths:web/src/auth/**", "same_member_concurrent"}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "UPDATE `conflict` SET `evidence` = ? WHERE `id` = ?", string(raw), fx.cf44)

	var page conflictsResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts", &page)
	var c conflictWire
	for _, cw := range page.Conflicts {
		if cw.Key == "CF-44" {
			c = cw
		}
	}
	want := []conflictPlanWire{
		{Key: "INT-92", Who: "Bob (frontend)", Summary: "build the login screen", Path: "web/src/pages/Login.tsx", Yields: false},
		{Key: "INT-83", Who: "Ana (ui)", Summary: "add login page", Path: "web/src/routes/login.tsx", Yields: true},
	}
	if fmt.Sprintf("%+v", c.Plans) != fmt.Sprintf("%+v", want) {
		t.Fatalf("plans = %+v, want %+v", c.Plans, want)
	}
	if strings.Join(c.Signals, "|") != "shared words: login, screen|also share web/src/auth/**|one person's two agents" {
		t.Fatalf("signals = %q", c.Signals)
	}
	if c.IssueRef != "" {
		t.Fatalf("an overlapping path became issue_ref %q", c.IssueRef)
	}
	if fmt.Sprint(c.Paths) != "[web/src/routes/login.tsx web/src/pages/Login.tsx]" {
		t.Fatalf("paths = %v, want [b_pattern a_pattern]", c.Paths)
	}
	t.Logf("swapped sides: plans %s then %s (yields), signals %q, no issue_ref, paths %v", c.Plans[0].Key, c.Plans[1].Key, c.Signals, c.Paths)
}

// TestConflictHistoryTakesTheDuplicateKind: duplicate_work is a filter the
// history offers, and it serves only duplicates.
func TestConflictHistoryTakesTheDuplicateKind(t *testing.T) {
	db := testDB(t)
	fx := seedDupHack(t, db)
	srv := newBoardServer(t, db)

	mustExec(t, db, "UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolution_note` = ?, `resolved_at` = ?, `escalated_at` = ? WHERE `id` = ?",
		enums.CONFLICT_STATUS_RESOLVED, enums.CONFLICT_RESOLUTION_YIELDED, fx.yielded, sqlTime(t, "2026-09-21T14:15:02Z"),
		sqlTime(t, str(t, fx.spec93, "escalated_at")), fx.cf44)
	// A settled path overlap, which the kind filter must leave out.
	mustExec(t, db, "UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolved_at` = NOW() WHERE `team_uuid` = ? AND `key` = ?",
		enums.CONFLICT_STATUS_RESOLVED, enums.CONFLICT_RESOLUTION_COORDINATED, fx.teamID, "CF-43")

	var every conflictHistoryPage
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts/history", &every)
	if len(every.Conflicts) != 2 {
		t.Fatalf("the unfiltered history has %d conflicts, want CF-43 and CF-44", len(every.Conflicts))
	}

	var page conflictHistoryPage
	raw := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts/history?kind=duplicate_work", &page)
	if !strings.Contains(","+strings.Join(page.Kinds, ",")+",", ",duplicate_work,") {
		t.Fatalf("the kind vocabulary %v does not offer duplicate_work", page.Kinds)
	}
	if page.Kind != "duplicate_work" || len(page.Conflicts) != 1 || page.Conflicts[0].Key != "CF-44" || page.Conflicts[0].Kind != "duplicate_work" {
		t.Fatalf("kind=duplicate_work served %+v", page.Conflicts)
	}
	c := page.Conflicts[0]
	if len(c.Plans) != 2 || !c.Plans[1].Yields || c.JudgeNote == "" || c.EscalatedAt == nil || c.Resolution != "yielded" {
		t.Fatalf("a past duplicate conflict = %+v", c)
	}
	assertNoSecrets(t, raw)
	t.Logf("kinds %v; conflicts/history?kind=duplicate_work -> %s alone, with its plans, judge note and escalated_at", page.Kinds, c.Key)
}

// ---------------------------------------------------------------- the gate

var dupRoutes = []string{"/conflicts", "/conflicts?status=all", "/conflicts/history?kind=duplicate_work", "/sessions/S-1"}

var badDupRoutes = []string{"/conflicts/history?kind=same_work", "/conflicts?status=nonsense"}

// TestDuplicateReadsGateOnAPrivateTeam: on a private team holding a duplicate
// conflict, a member reads its plans; a non-member, an anonymous request and
// a garbage session get the unknown team's bytes, the same 404 byte for byte,
// for a good route and a bad parameter alike.
func TestDuplicateReadsGateOnAPrivateTeam(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	evidence := dupSpecExample(t, "spec94DuplicateEvidence")
	var project, s1, agent, member string
	if err := db.QueryRow("SELECT `project_uuid`, `id`, `agent_uuid`, `member_uuid` FROM `session` WHERE `team_uuid` = ? AND `key` = ?",
		fx.teamID, "S-1").Scan(&project, &s1, &agent, &member); err != nil {
		t.Fatal(err)
	}
	cf := newUUID(t)
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`detector_rule`,`evidence`,`occurrence_count`,`first_detected_at`,`last_detected_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,NOW(),NOW())",
		cf, fx.teamID, project, "CF-44", enums.CONFLICT_KIND_DUPLICATE_WORK, "dedupe-"+cf[:12], enums.CONFLICT_SEVERITY_MEDIUM,
		enums.CONFLICT_STATUS_OPEN, enums.DETECTED_BY_AGENT, "duplicate_work.words", evidence, 1)
	mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
		"`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
		newUUID(t), cf, fx.teamID, s1, agent, member, enums.SUBJECT_KIND_INTENT, newUUID(t), enums.PARTICIPANT_ROLE_INITIATOR)

	for name, h := range map[string]map[string]string{
		"owner session":  session(fx.ownerSession),
		"member session": session(fx.memberSession),
		"member bearer":  {"Authorization": "Bearer " + fx.memberToken},
	} {
		for _, p := range dupRoutes {
			got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h)
			if got.code != http.StatusOK || !json.Valid([]byte(got.body)) {
				t.Fatalf("%s %s: got %d, want 200 (%.200s)", name, p, got.code, got.body)
			}
			assertNoSecrets(t, got.body, fx.ownerSession, fx.memberSession, fx.memberToken, fx.memberTokenHash)
		}
		open := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts", h)
		if !strings.Contains(open.body, `"plans":[{"key":"INT-83"`) {
			t.Fatalf("%s: the member's /conflicts has no duplicate plans: %.400s", name, open.body)
		}
		for _, p := range badDupRoutes {
			if got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h); got.code != http.StatusBadRequest {
				t.Fatalf("%s %s: got %d, want 400", name, p, got.code)
			}
		}
		t.Logf("%-15s -> 200 on %d reads (plans on /conflicts), 400 on %d bad parameters", name, len(dupRoutes), len(badDupRoutes))
	}

	const unknown = "no-such-team-7q"
	for _, p := range append(append([]string{}, dupRoutes...), badDupRoutes...) {
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
			if strings.Contains(got.body, "INT-83") || strings.Contains(got.body, "login") {
				t.Fatalf("%s, %s: the duplicate reached a non-member: %s", p, name, got.body)
			}
		}
		t.Logf("%-40s anonymous, non-member session/bearer, garbage session -> %d %q identical to unknown team", p, want.code, want.body)
	}
}
