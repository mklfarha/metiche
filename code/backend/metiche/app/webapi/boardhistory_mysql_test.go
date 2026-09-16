package webapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/backend/enums"
)

// The rest of the board's history over a real MySQL: the conflict history,
// the event log read backwards, and the graph over a past window. Same DSN
// rule as the rest of this package (see dsnEnv). Names are Ana, Bob and test.

// ---------------------------------------------------------------- helpers

type conflictHistoryPage struct {
	Status     string         `json:"status"`
	Kind       string         `json:"kind"`
	Conflicts  []conflictWire `json:"conflicts"`
	NextCursor string         `json:"next_cursor"`
	Statuses   []string       `json:"statuses"`
	Kinds      []string       `json:"kinds"`
}

type eventsPage struct {
	Events     []historyEventWire `json:"events"`
	NextBefore *int64             `json:"next_before"`
	Kinds      []string           `json:"kinds"`
}

func pageSize(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// walkConflictHistory pages through the conflict history with the given
// filters and returns every row in the order served, plus every raw body.
func walkConflictHistory(t *testing.T, base, slug string, filters url.Values, limit int, between func(page int)) ([]conflictWire, []string) {
	t.Helper()
	var (
		all    []conflictWire
		bodies []string
		cursor string
	)
	want := pageSize(limit, defaultConflictHistoryPage, maxConflictHistoryPage)
	for page := 0; ; page++ {
		if page > 200 {
			t.Fatal("more than 200 pages: the cursor is not advancing")
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
		var p conflictHistoryPage
		bodies = append(bodies, getJSON(t, base+"/v1/teams/"+slug+"/conflicts/history?"+q.Encode(), &p))
		if len(p.Conflicts) > want {
			t.Fatalf("page %d has %d rows, over its limit %d", page, len(p.Conflicts), want)
		}
		if p.NextCursor != "" && len(p.Conflicts) != want {
			t.Fatalf("page %d has %d rows and a next cursor; only a full page may have one", page, len(p.Conflicts))
		}
		all = append(all, p.Conflicts...)
		if p.NextCursor == "" {
			return all, bodies
		}
		if between != nil {
			between(page)
		}
		cursor = p.NextCursor
	}
}

// walkEvents pages backwards through the event log.
func walkEvents(t *testing.T, base, slug string, filters url.Values, limit int, between func(page int)) ([]historyEventWire, []string) {
	t.Helper()
	var (
		all    []historyEventWire
		bodies []string
		before int64
	)
	want := pageSize(limit, defaultEventsPage, maxEventsPage)
	for page := 0; ; page++ {
		if page > 400 {
			t.Fatal("more than 400 pages: the cursor is not advancing")
		}
		q := url.Values{}
		for k, v := range filters {
			q[k] = v
		}
		if limit > 0 {
			q.Set("limit", fmt.Sprint(limit))
		}
		if before > 0 {
			q.Set("before", fmt.Sprint(before))
		}
		var p eventsPage
		bodies = append(bodies, getJSON(t, base+"/v1/teams/"+slug+"/events?"+q.Encode(), &p))
		if len(p.Events) > want {
			t.Fatalf("page %d has %d events, over its limit %d", page, len(p.Events), want)
		}
		if p.NextBefore != nil && len(p.Events) != want {
			t.Fatalf("page %d has %d events and a next cursor; only a full page may have one", page, len(p.Events))
		}
		all = append(all, p.Events...)
		if p.NextBefore == nil {
			return all, bodies
		}
		if between != nil {
			between(page)
		}
		before = *p.NextBefore
	}
}

func queryStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func sessionID(t *testing.T, db *sql.DB, teamID, key string) string {
	t.Helper()
	var id string
	if err := db.QueryRow("SELECT `id` FROM `session` WHERE `team_uuid` = ? AND `key` = ?", teamID, key).Scan(&id); err != nil {
		t.Fatalf("session %s: %v", key, err)
	}
	return id
}

// canaryEvidence is detector evidence whose paths are real and whose intent
// summary carries the canary: the paths must be served and the summary never.
func canaryEvidence(overlap, a, b string) string {
	return `{"overlap_path":"` + overlap + `","a_pattern":"` + a + `","b_pattern":"` + b +
		`","a_label":"Ana · claude-1","a_summary":"deploy with ` + canaryToken + `","b_label":"Bob · claude-2","adjusters":[]}`
}

func insertConflictRow(t *testing.T, db *sql.DB, teamID, projectID, key string, kind enums.ConflictKind,
	status enums.ConflictStatus, first, resolved any, evidence any) string {
	t.Helper()
	id := newUUID(t)
	var resolution, note any
	if status == enums.CONFLICT_STATUS_RESOLVED {
		resolution, note = enums.CONFLICT_RESOLUTION_COORDINATED, "test: settled "+key
	}
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`evidence`,`resolution`,`resolution_note`,`resolved_at`,`occurrence_count`,`first_detected_at`,`last_detected_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, teamID, projectID, key, kind.ToInt64(), "dedupe-"+id[:12], 2, status.ToInt64(), 1,
		evidence, resolution, note, resolved, 1, first, first)
	return id
}

// ---------------------------------------------------------------- conflicts

// seedConflictHistory adds 132 past conflicts (every past status, two kinds,
// ties in the same second, rows with no resolved_at and rows with neither
// time) and five open ones that must never be in a history. HX-7 has two
// participants and canary-carrying evidence.
func seedConflictHistory(t *testing.T, db *sql.DB, fx seeded) {
	t.Helper()
	ids := fixtureIDsFor(t, db, fx.teamID)
	base := time.Now().UTC().Add(-96 * time.Hour).Truncate(time.Second)
	statuses := []enums.ConflictStatus{enums.CONFLICT_STATUS_RESOLVED, enums.CONFLICT_STATUS_DISMISSED, enums.CONFLICT_STATUS_EXPIRED}
	kinds := []enums.ConflictKind{enums.CONFLICT_KIND_PATH_OVERLAP, enums.CONFLICT_KIND_CONTRACT_MISMATCH}
	for i := 0; i < 132; i++ {
		first := base.Add(time.Duration(i) * time.Minute)
		var resolved any = base.Add(time.Duration(i/4) * 7 * time.Minute) // four per second: ties
		var firstArg any = first
		switch {
		case i%17 == 0:
			resolved, firstArg = nil, nil // sorts by created_at
		case i%11 == 0:
			resolved = nil // sorts by first_detected_at
		}
		key := fmt.Sprintf("HX-%d", i)
		var evidence any
		if i == 7 {
			evidence = canaryEvidence("web/login.tsx", "web/**", "web/login.tsx")
		}
		id := insertConflictRow(t, db, fx.teamID, ids.project, key, kinds[i%len(kinds)], statuses[i%len(statuses)], firstArg, resolved, evidence)
		if i == 7 {
			s1, s2 := sessionID(t, db, fx.teamID, "S-1"), sessionID(t, db, fx.teamID, "S-2")
			mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
				newUUID(t), id, fx.teamID, s1, ids.agentA, ids.ana, 3, newUUID(t), 1)
			mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
				newUUID(t), id, fx.teamID, s2, ids.agentB, ids.bob, 3, newUUID(t), 2)
		}
	}
	for i := 0; i < 5; i++ {
		st := []enums.ConflictStatus{enums.CONFLICT_STATUS_OPEN, enums.CONFLICT_STATUS_ACKNOWLEDGED, enums.CONFLICT_STATUS_RESOLVING}[i%3]
		insertConflictRow(t, db, fx.teamID, ids.project, fmt.Sprintf("OPEN-%d", i), enums.CONFLICT_KIND_PATH_OVERLAP, st, base, nil, nil)
	}
}

const conflictHistoryOrderSQL = "SELECT `key` FROM `conflict` c WHERE c.`team_uuid` = ? AND c.`status` IN (4,5,6)"

// TestConflictHistoryPagesEveryPastConflictWithoutGapsOrDuplicates walks 133
// past conflicts at three page sizes while a conflict closes and every row's
// last_detected_at moves mid-walk.
func TestConflictHistoryPagesEveryPastConflictWithoutGapsOrDuplicates(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	seedConflictHistory(t, db, fx)
	ids := fixtureIDsFor(t, db, fx.teamID)
	srv := newBoardServer(t, db)

	ordered := func() []string {
		return queryStrings(t, db, conflictHistoryOrderSQL+" ORDER BY "+conflictHistoryOrder+" DESC, c.`key`", fx.teamID)
	}
	if n := len(ordered()); n != 133 { // 132 + the fixture's resolved CF-2
		t.Fatalf("fixture has %d past conflicts, want 133", n)
	}

	for _, limit := range []int{100, 37, 0} {
		expected := ordered()
		late := fmt.Sprintf("LATE-%d", limit)
		got, bodies := walkConflictHistory(t, srv.URL, fx.slug, nil, limit, func(page int) {
			if page == 0 {
				// Closes mid-walk: sorts ahead of every cursor.
				insertConflictRow(t, db, fx.teamID, ids.project, late, enums.CONFLICT_KIND_PATH_OVERLAP,
					enums.CONFLICT_STATUS_RESOLVED, time.Now().UTC(), time.Now().UTC().Add(time.Hour), nil)
				// A re-detection bumps last_detected_at on a closed row: it
				// must not move anything across a page boundary.
				mustExec(t, db, "UPDATE `conflict` SET `last_detected_at` = UTC_TIMESTAMP(), `occurrence_count` = `occurrence_count` + 1 WHERE `team_uuid` = ?", fx.teamID)
			}
		})
		keys := make([]string, 0, len(got))
		seen := map[string]bool{}
		statuses := map[string]int{}
		for _, c := range got {
			if seen[c.Key] {
				t.Fatalf("limit %d: %s served twice", limit, c.Key)
			}
			seen[c.Key] = true
			keys = append(keys, c.Key)
			statuses[c.Status]++
			if strings.HasPrefix(c.Key, "OPEN-") || c.Key == "CF-1" {
				t.Fatalf("limit %d: open conflict %s is in the history", limit, c.Key)
			}
		}
		if strings.Join(keys, ",") != strings.Join(expected, ",") {
			t.Fatalf("limit %d: served %d rows in a different order or with gaps\n got  %v\n want %v", limit, len(keys), keys, expected)
		}
		for _, b := range bodies {
			assertNoSecrets(t, b, canaryToken, "a_summary", "evidence")
		}
		t.Logf("limit %d: %d pages, %d conflicts (%v), no duplicates, no gaps, %s excluded from this walk",
			limit, len(bodies), len(keys), statuses, late)
	}

	// One row, whole: paths from the evidence, participants with their
	// session keys, how it ended.
	all, _ := walkConflictHistory(t, srv.URL, fx.slug, nil, 100, nil)
	var hx7 conflictWire
	for _, c := range all {
		if c.Key == "HX-7" {
			hx7 = c
		}
	}
	if strings.Join(hx7.Paths, ",") != "web/login.tsx,web/**" || hx7.Status != "dismissed" || hx7.Kind != "contract_mismatch" ||
		len(hx7.Participants) != 2 || hx7.FirstDetectedAt == nil || hx7.ResolvedAt == nil {
		t.Fatalf("HX-7 = %+v", hx7)
	}
	parts := []string{}
	for _, p := range hx7.Participants {
		parts = append(parts, p.Role+":"+p.SessionKey+":"+p.MemberName)
	}
	sort.Strings(parts)
	joined := strings.Join(parts, " ")
	if !strings.Contains(joined, enums.ParticipantRole(1).String()+":S-1:Ana") ||
		!strings.Contains(joined, enums.ParticipantRole(2).String()+":S-2:") {
		t.Fatalf("HX-7 participants = %v", parts)
	}
	var hx3 conflictWire
	for _, c := range all {
		if c.Key == "HX-3" {
			hx3 = c
		}
	}
	if hx3.Status != "resolved" || hx3.Resolution != enums.ConflictResolution(enums.CONFLICT_RESOLUTION_COORDINATED).String() || hx3.ResolutionNote != "test: settled HX-3" || len(hx3.Paths) != 0 {
		t.Fatalf("HX-3 = %+v", hx3)
	}

	// limit is capped, not refused.
	var capped conflictHistoryPage
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts/history?limit=1000", &capped)
	if len(capped.Conflicts) != maxConflictHistoryPage || capped.NextCursor == "" {
		t.Fatalf("limit=1000 served %d rows (next %q), want %d and a cursor", len(capped.Conflicts), capped.NextCursor, maxConflictHistoryPage)
	}
	if strings.Join(capped.Statuses, ",") != "resolved,dismissed,expired" ||
		strings.Join(capped.Kinds, ",") != "path_overlap,contract_mismatch,contract_unclaimed,contract_naming_variant,decision_contradiction" {
		t.Fatalf("filter vocabulary = %v %v", capped.Statuses, capped.Kinds)
	}
}

// TestConflictHistoryFilters: status and kind, alone and together, walked to
// the end, against the table; and what each refuses.
func TestConflictHistoryFilters(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	seedConflictHistory(t, db, fx)
	srv := newBoardServer(t, db)

	for _, c := range []struct {
		status, kind string
		where        string
	}{
		{"resolved", "", " AND c.`status` = 4"},
		{"dismissed", "", " AND c.`status` = 5"},
		{"expired", "", " AND c.`status` = 6"},
		{"", "contract_mismatch", " AND c.`kind` = 2"},
		{"all", "path_overlap", " AND c.`kind` = 1"},
		{"expired", "path_overlap", " AND c.`status` = 6 AND c.`kind` = 1"},
		{"dismissed", "stale_base", " AND c.`status` = 5 AND c.`kind` = 7"},
	} {
		want := queryStrings(t, db, conflictHistoryOrderSQL+c.where+" ORDER BY "+conflictHistoryOrder+" DESC, c.`key`", fx.teamID)
		q := url.Values{}
		if c.status != "" {
			q.Set("status", c.status)
		}
		if c.kind != "" {
			q.Set("kind", c.kind)
		}
		got, bodies := walkConflictHistory(t, srv.URL, fx.slug, q, 20, nil)
		keys := []string{}
		for _, row := range got {
			keys = append(keys, row.Key)
			if c.status != "" && c.status != "all" && row.Status != c.status {
				t.Fatalf("status=%s served %s (%s)", c.status, row.Key, row.Status)
			}
			if c.kind != "" && row.Kind != c.kind {
				t.Fatalf("kind=%s served %s (%s)", c.kind, row.Key, row.Kind)
			}
		}
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Fatalf("status=%q kind=%q: got %v\nwant %v", c.status, c.kind, keys, want)
		}
		t.Logf("status=%-9q kind=%-19q -> %3d conflicts over %d pages", c.status, c.kind, len(keys), len(bodies))
	}

	for _, bad := range []string{"status=open", "status=acknowledged", "status=nope", "kind=nope", "kind=invalid",
		"cursor=!!!", "cursor=" + url.QueryEscape(strings.Repeat("A", 300)), "cursor=czF8MjAyNi0wMS0wMSAwMDowMDowMHxTLTE"} {
		resp, err := http.Get(srv.URL + "/v1/teams/" + fx.slug + "/conflicts/history?" + bad)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", bad, resp.StatusCode)
		}
	}
}

// ---------------------------------------------------------------- events

// seedEventHistory adds 300 events (sequences 100-399) over S-1, S-2 and no
// session, in four kinds, every one of them carrying the canaries in payload,
// idempotency_key and response_snapshot.
func seedEventHistory(t *testing.T, db *sql.DB, fx seeded) {
	t.Helper()
	ids := fixtureIDsFor(t, db, fx.teamID)
	s1, s2 := sessionID(t, db, fx.teamID, "S-1"), sessionID(t, db, fx.teamID, "S-2")
	kinds := []enums.EventKind{enums.EVENT_KIND_SESSION_STARTED, enums.EVENT_KIND_INTENT_DECLARED,
		enums.EVENT_KIND_CLAIM_RELEASED, enums.EVENT_KIND_CONFLICT_RAISED}
	snapshot := `{"ok":true,"token":"` + canaryToken + `","session":"` + canarySession + `"}`
	at := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 300; i++ {
		var session, member, agent any
		switch i % 3 {
		case 0:
			session = s1
			if i%2 == 1 {
				member, agent = ids.ana, ids.agentA
			}
		case 1:
			session = s2 // member and agent come from the session
		}
		mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`project_uuid`,`session_uuid`,`member_uuid`,`agent_uuid`,`kind`,`structural`,"+
			"`subject_key`,`summary`,`payload`,`idempotency_key`,`response_snapshot`,`occurred_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			newUUID(t), fx.teamID, int64(100+i), ids.project, session, member, agent, kinds[i%len(kinds)].ToInt64(), i%2,
			fmt.Sprintf("SUB-%d", i), fmt.Sprintf("test event %d", 100+i),
			`{"secret":"`+canaryToken+`"}`, fmt.Sprintf("%s-%s-hist-%d", canaryToken, canarySession, i), snapshot,
			at.Add(time.Duration(i)*time.Second))
	}
}

// TestEventHistoryPagesBackwardsWithoutGapsOrDuplicates walks 305 events at
// four page sizes while a new event is appended mid-walk.
func TestEventHistoryPagesBackwardsWithoutGapsOrDuplicates(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	seedEventHistory(t, db, fx)
	srv := newBoardServer(t, db)

	ordered := func() []string {
		return queryStrings(t, db, "SELECT CAST(`sequence` AS CHAR) FROM `team_event` WHERE `team_uuid` = ? ORDER BY `sequence` DESC", fx.teamID)
	}
	if n := len(ordered()); n != 305 {
		t.Fatalf("fixture has %d events, want 305", n)
	}
	next := int64(1000)
	for _, limit := range []int{0, 77, 200, 1000} {
		expected := ordered()
		got, bodies := walkEvents(t, srv.URL, fx.slug, nil, limit, func(page int) {
			if page == 0 {
				next++
				mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`kind`,`structural`,`summary`,`idempotency_key`) VALUES (?,?,?,?,?,?,?)",
					newUUID(t), fx.teamID, next, enums.EVENT_KIND_MEMBER_JOINED, 1, "test late event", fmt.Sprintf("late-%d", next))
			}
		})
		seqs := make([]string, 0, len(got))
		seen := map[int64]bool{}
		for _, e := range got {
			if seen[e.Sequence] {
				t.Fatalf("limit %d: %d served twice", limit, e.Sequence)
			}
			seen[e.Sequence] = true
			seqs = append(seqs, fmt.Sprint(e.Sequence))
		}
		if strings.Join(seqs, ",") != strings.Join(expected, ",") {
			t.Fatalf("limit %d: served %d events in a different order or with gaps\n got  %v\n want %v", limit, len(seqs), seqs, expected)
		}
		for _, b := range bodies {
			assertNoSecrets(t, b, canaryToken, canarySession, "TESTCANARY", `"payload"`, `"idempotency_key"`, `"response_snapshot"`)
		}
		t.Logf("limit %4d: %d pages, %d events newest first, no duplicates, no gaps, sequence %d excluded from this walk",
			limit, len(bodies), len(seqs), next)
	}

	// The fields, on known rows.
	all, _ := walkEvents(t, srv.URL, fx.slug, nil, 200, nil)
	bySeq := map[int64]historyEventWire{}
	for _, e := range all {
		bySeq[e.Sequence] = e
	}
	var bobName string
	if err := db.QueryRow("SELECT m.`display_name` FROM `session` s JOIN `member` m ON m.`id` = s.`member_uuid` WHERE s.`team_uuid` = ? AND s.`key` = 'S-2'", fx.teamID).Scan(&bobName); err != nil {
		t.Fatal(err)
	}
	for seq, want := range map[int64]historyEventWire{
		100: {Sequence: 100, Kind: "session_started", SubjectKey: "SUB-0", Summary: "test event 100", SessionKey: "S-1", MemberName: "Ana", AgentLabel: "claude-1"},
		103: {Sequence: 103, Kind: "conflict_raised", Structural: true, SubjectKey: "SUB-3", Summary: "test event 103", SessionKey: "S-1", MemberName: "Ana", AgentLabel: "claude-1"},
		101: {Sequence: 101, Kind: "intent_declared", Structural: true, SubjectKey: "SUB-1", Summary: "test event 101", SessionKey: "S-2", MemberName: bobName, AgentLabel: "claude-2"},
		102: {Sequence: 102, Kind: "claim_released", SubjectKey: "SUB-2", Summary: "test event 102"},
	} {
		got := bySeq[seq]
		if got.OccurredAt == nil {
			t.Fatalf("event %d has no occurred_at", seq)
		}
		got.OccurredAt = nil
		if got != want {
			t.Fatalf("event %d = %+v, want %+v", seq, got, want)
		}
	}
}

// TestEventHistoryFilters: kind, session and both, against the table; an
// unknown session is an empty page; and what is refused.
func TestEventHistoryFilters(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	seedEventHistory(t, db, fx)
	srv := newBoardServer(t, db)
	s2 := sessionID(t, db, fx.teamID, "S-2")

	for _, c := range []struct {
		kind, session string
		where         string
		args          []any
	}{
		{"claim_released", "", " AND `kind` = ?", []any{enums.EVENT_KIND_CLAIM_RELEASED}},
		{"", "S-2", " AND `session_uuid` = ?", []any{s2}},
		{"intent_declared", "S-2", " AND `kind` = ? AND `session_uuid` = ?", []any{enums.EVENT_KIND_INTENT_DECLARED, s2}},
		{"decision_recorded", "", " AND `kind` = ?", []any{enums.EVENT_KIND_DECISION_RECORDED}},
	} {
		want := queryStrings(t, db, "SELECT CAST(`sequence` AS CHAR) FROM `team_event` WHERE `team_uuid` = ?"+c.where+" ORDER BY `sequence` DESC",
			append([]any{fx.teamID}, c.args...)...)
		q := url.Values{}
		if c.kind != "" {
			q.Set("kind", c.kind)
		}
		if c.session != "" {
			q.Set("session", c.session)
		}
		got, bodies := walkEvents(t, srv.URL, fx.slug, q, 16, nil)
		seqs := []string{}
		for _, e := range got {
			seqs = append(seqs, fmt.Sprint(e.Sequence))
			if (c.kind != "" && e.Kind != c.kind) || (c.session != "" && e.SessionKey != c.session) {
				t.Fatalf("kind=%q session=%q served %+v", c.kind, c.session, e)
			}
		}
		if strings.Join(seqs, ",") != strings.Join(want, ",") {
			t.Fatalf("kind=%q session=%q: got %v\nwant %v", c.kind, c.session, seqs, want)
		}
		t.Logf("kind=%-19q session=%-5q -> %3d events over %d pages", c.kind, c.session, len(seqs), len(bodies))
	}

	var empty eventsPage
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/events?session=S-404", &empty)
	if len(empty.Events) != 0 || empty.NextBefore != nil || len(empty.Kinds) != 22 {
		t.Fatalf("unknown session = %+v", empty)
	}
	var capped eventsPage
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/events?limit=5000", &capped)
	if len(capped.Events) != maxEventsPage || capped.NextBefore == nil {
		t.Fatalf("limit=5000 served %d events, want %d and a cursor", len(capped.Events), maxEventsPage)
	}

	for _, bad := range []string{"before=abc", "before=0", "before=-5", "before=1e3", "before=" + strings.Repeat("9", 25),
		"kind=nope", "kind=invalid", "session=" + url.QueryEscape("S 1"), "session=" + strings.Repeat("S", 40)} {
		resp, err := http.Get(srv.URL + "/v1/teams/" + fx.slug + "/events?" + bad)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", bad, resp.StatusCode)
		}
	}
}

// ---------------------------------------------------------------- graph

type graphFixtureHold struct {
	claim, path   string
	status        enums.ClaimStatus
	created, left time.Time // left: when it stopped being held (updated_at); zero while held
	expires       time.Time
}

func insertGraphSession(t *testing.T, db *sql.DB, fx seeded, ids fixtureIDs, key string, status enums.SessionStatus,
	started, ended, heartbeat any, holds ...graphFixtureHold) string {
	t.Helper()
	s := newUUID(t)
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`goal`,`status`,`started_at`,`ended_at`,`last_heartbeat_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		s, fx.teamID, ids.project, ids.agentB, ids.bob, key, "test goal "+key, status.ToInt64(), started, ended, heartbeat)
	for _, h := range holds {
		c := newUUID(t)
		var released any
		updated := h.created
		if !h.left.IsZero() {
			updated = h.left
			if h.status == enums.CLAIM_STATUS_RELEASED {
				released = h.left
			}
		}
		mustExec(t, db, "INSERT INTO `claim` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`mode`,`status`,`expires_at`,`hard_expires_at`,`released_at`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			c, fx.teamID, ids.project, s, ids.bob, h.claim, enums.CLAIM_MODE_WRITE, h.status.ToInt64(), h.expires, h.created.Add(4*time.Hour).Add(30*24*time.Hour), released, h.created, updated)
		mustExec(t, db, "INSERT INTO `claim_path` (`id`,`claim_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`mode`,`status`,`expires_at`,`pattern`,`pattern_norm`,`kind`,`prefix`,`depth`,`created_at`,`updated_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			newUUID(t), c, ids.project, s, ids.bob, enums.CLAIM_MODE_WRITE, h.status.ToInt64(), h.expires, h.path, h.path, 1, "test/", 2, h.created, updated)
	}
	return s
}

// seedGraphWindows lays out sessions and holds across three windows, relative
// to now:
//
//	G-RECENT  ended 2h ago; held web/a.tsx 5h->3h ago               24h, 7d
//	G-SPAN    40h->2h ago; api/early.go 40h->30h, api/late.go 5h->2h  both; early only in 7d
//	G-WEEK    3d ago, 2h long; docs/x.md expired at 3d-1h (swept 1d)  7d only
//	G-OLD     ended 10d ago; web/old.tsx                               neither
//	G-LIVE    live since 30d; web/live.tsx held (open-ended),
//	          web/lapsed.tsx held but its TTL ran out 26h ago          both; lapsed only in 7d
//
// plus a conflict settled 3h ago between G-RECENT and G-SPAN (24h and 7d)
// and one settled 4d ago (7d only).
func seedGraphWindows(t *testing.T, db *sql.DB, fx seeded) time.Time {
	t.Helper()
	ids := fixtureIDsFor(t, db, fx.teamID)
	now := time.Now().UTC().Truncate(time.Second)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	h := time.Hour
	day := 24 * h

	recent := insertGraphSession(t, db, fx, ids, "G-RECENT", enums.SESSION_STATUS_ENDED, ago(5*h), ago(2*h), ago(2*h),
		graphFixtureHold{"GC-1", "web/a.tsx", enums.CLAIM_STATUS_RELEASED, ago(5 * h), ago(3 * h), ago(3*h - 10*time.Minute)})
	span := insertGraphSession(t, db, fx, ids, "G-SPAN", enums.SESSION_STATUS_ENDED, ago(40*h), ago(2*h), ago(2*h),
		graphFixtureHold{"GC-2", "api/early.go", enums.CLAIM_STATUS_RELEASED, ago(40 * h), ago(30 * h), ago(29 * h)},
		graphFixtureHold{"GC-3", "api/late.go", enums.CLAIM_STATUS_RELEASED, ago(5 * h), ago(2 * h), ago(1 * h)})
	insertGraphSession(t, db, fx, ids, "G-WEEK", enums.SESSION_STATUS_ENDED, ago(3*day), ago(3*day-2*h), ago(3*day-2*h),
		graphFixtureHold{"GC-4", "docs/x.md", enums.CLAIM_STATUS_EXPIRED, ago(3 * day), ago(1 * day), ago(3*day - h)})
	insertGraphSession(t, db, fx, ids, "G-OLD", enums.SESSION_STATUS_ABANDONED, ago(11*day), ago(10*day), ago(10*day),
		graphFixtureHold{"GC-5", "web/old.tsx", enums.CLAIM_STATUS_EXPIRED, ago(11 * day), ago(10 * day), ago(10*day + h)})
	insertGraphSession(t, db, fx, ids, "G-LIVE", enums.SESSION_STATUS_LIVE, ago(30*day), nil, now,
		graphFixtureHold{"GC-6", "web/live.tsx", enums.CLAIM_STATUS_HELD, ago(2 * h), time.Time{}, now.Add(h)},
		graphFixtureHold{"GC-7", "web/lapsed.tsx", enums.CLAIM_STATUS_HELD, ago(29 * h), time.Time{}, ago(26 * h)})

	cf := insertConflictRow(t, db, fx.teamID, ids.project, "GX-1", enums.CONFLICT_KIND_PATH_OVERLAP, enums.CONFLICT_STATUS_RESOLVED,
		ago(4*h), ago(3*h), canaryEvidence("web/a.tsx", "web/**", "web/a.tsx"))
	for _, p := range []struct{ s, agent, member string }{{recent, ids.agentB, ids.bob}, {span, ids.agentB, ids.bob}} {
		mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
			newUUID(t), cf, fx.teamID, p.s, p.agent, p.member, 3, newUUID(t), 1)
	}
	insertConflictRow(t, db, fx.teamID, ids.project, "GX-2", enums.CONFLICT_KIND_PATH_OVERLAP, enums.CONFLICT_STATUS_RESOLVED,
		ago(4*day+h), ago(4*day), nil)
	insertConflictRow(t, db, fx.teamID, ids.project, "GX-3", enums.CONFLICT_KIND_PATH_OVERLAP, enums.CONFLICT_STATUS_RESOLVED,
		ago(9*day), ago(8*day), nil)
	return now
}

func graphSummary(g graphResponse) (sessions, holds, conflicts []string) {
	for _, s := range g.Sessions {
		sessions = append(sessions, s.Key)
	}
	for _, h := range g.Holds {
		until := "held"
		if h.HeldUntil != nil {
			until = "until"
		}
		holds = append(holds, h.SessionKey+":"+h.Path+":"+h.Status+":"+until)
	}
	for _, c := range g.Conflicts {
		conflicts = append(conflicts, c.Key)
	}
	sort.Strings(sessions)
	sort.Strings(holds)
	sort.Strings(conflicts)
	return sessions, holds, conflicts
}

// TestGraphWindowReturnsTheSessionsAndPathsActiveInIt.
func TestGraphWindowReturnsTheSessionsAndPathsActiveInIt(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	now := seedGraphWindows(t, db, fx)
	srv := newBoardServer(t, db)

	for _, c := range []struct {
		window                     string
		sessions, holds, conflicts string
	}{
		{"24h",
			// S-1, S-2 (live, stale) and S-3 (ended just now) are the base fixture's.
			"G-LIVE,G-RECENT,G-SPAN,S-1,S-2,S-3",
			"G-LIVE:web/live.tsx:held:held,G-RECENT:web/a.tsx:released:until,G-SPAN:api/late.go:released:until," +
				"S-1:app/auth.go:held:held,S-1:app/session/**:held:held,S-2:web/login.tsx:expired:until",
			"CF-1,CF-2,GX-1"},
		{"7d",
			"G-LIVE,G-RECENT,G-SPAN,G-WEEK,S-1,S-2,S-3",
			"G-LIVE:web/lapsed.tsx:expired:until,G-LIVE:web/live.tsx:held:held,G-RECENT:web/a.tsx:released:until," +
				"G-SPAN:api/early.go:released:until,G-SPAN:api/late.go:released:until,G-WEEK:docs/x.md:expired:until," +
				"S-1:app/auth.go:held:held,S-1:app/session/**:held:held,S-2:web/login.tsx:expired:until",
			"CF-1,CF-2,GX-1,GX-2"},
	} {
		var g graphResponse
		body := getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/graph?window="+c.window, &g)
		sessions, holds, conflicts := graphSummary(g)
		if strings.Join(sessions, ",") != c.sessions {
			t.Fatalf("%s sessions = %v\nwant %s", c.window, sessions, c.sessions)
		}
		if strings.Join(holds, ",") != c.holds {
			t.Fatalf("%s holds = %v\nwant %s", c.window, holds, c.holds)
		}
		if strings.Join(conflicts, ",") != c.conflicts {
			t.Fatalf("%s conflicts = %v\nwant %s", c.window, conflicts, c.conflicts)
		}
		if g.Window != c.window || g.Truncated || g.From == "" || g.To == "" {
			t.Fatalf("%s: window %q truncated %v from %q to %q", c.window, g.Window, g.Truncated, g.From, g.To)
		}
		assertNoSecrets(t, body, canaryToken, "a_summary", "evidence")
		t.Logf("window %-3s: %d sessions %v, %d holds, conflicts %v", c.window, len(sessions), sessions, len(holds), conflicts)
	}

	// Exact intervals on the holds whose ends are derived.
	var g graphResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/graph?window=7d", &g)
	stamp := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	for _, h := range g.Holds {
		until := ""
		if h.HeldUntil != nil {
			until = *h.HeldUntil
		}
		switch h.Path {
		case "api/early.go": // released: updated_at
			if *h.HeldFrom != stamp(40*time.Hour) || until != stamp(30*time.Hour) {
				t.Fatalf("api/early.go = %s..%s", *h.HeldFrom, until)
			}
		case "docs/x.md": // swept a day late: ends at its expiry, not the sweep
			if until != stamp(3*24*time.Hour-time.Hour) {
				t.Fatalf("docs/x.md until %s, want %s", until, stamp(3*24*time.Hour-time.Hour))
			}
		case "web/lapsed.tsx": // never swept: ends at its expiry
			if until != stamp(26*time.Hour) || h.Status != "expired" {
				t.Fatalf("web/lapsed.tsx = %s until %s", h.Status, until)
			}
		case "web/live.tsx":
			if h.HeldUntil != nil || h.ClaimKey != "GC-6" || h.Mode != "write" {
				t.Fatalf("web/live.tsx = %+v", h)
			}
		}
	}
	for _, cf := range g.Conflicts {
		if cf.Key == "GX-1" && (strings.Join(cf.Paths, ",") != "web/a.tsx,web/**" || len(cf.Participants) != 2 ||
			cf.Participants[0].SessionKey == "" || cf.ResolvedAt == nil) {
			t.Fatalf("GX-1 = %+v", cf)
		}
	}

	for _, bad := range []string{"window=30d", "window=live", "window=1h"} {
		resp, err := http.Get(srv.URL + "/v1/teams/" + fx.slug + "/graph?" + bad)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", bad, resp.StatusCode)
		}
	}
	var def graphResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/graph?limit=2", &def)
	if def.Window != "24h" || len(def.Sessions) != 2 || !def.Truncated {
		t.Fatalf("default window with limit=2: %q, %d sessions, truncated %v", def.Window, len(def.Sessions), def.Truncated)
	}
}

// ---------------------------------------------------------------- gate

var historyRoutes = []string{
	"/conflicts/history", "/conflicts/history?status=resolved&kind=path_overlap&limit=5",
	"/events", "/events?before=41&kind=intent_declared&session=S-1&limit=2",
	"/graph", "/graph?window=7d",
}

// badHistoryRoutes are refused with 400 for a reader the guard lets in, and
// must still be the unknown team's 404 for anybody it does not.
var badHistoryRoutes = []string{"/conflicts/history?cursor=garbage", "/events?before=abc", "/graph?window=30d"}

// TestBoardHistoryGateOnAPrivateTeam: members read every history route (browser
// session, or bearer as every board read allows); a non-member, an anonymous
// request and a garbage session get the unknown team's bytes, with filters,
// cursors and bad parameters alike.
func TestBoardHistoryGateOnAPrivateTeam(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	seedConflictHistory(t, db, fx)
	srv := newGuardedServer(t, db)

	for name, h := range map[string]map[string]string{
		"owner session":  session(fx.ownerSession),
		"member session": session(fx.memberSession),
		"member bearer":  {"Authorization": "Bearer " + fx.memberToken},
	} {
		for _, p := range historyRoutes {
			got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h)
			if got.code != http.StatusOK || !json.Valid([]byte(got.body)) {
				t.Fatalf("%s %s: got %d, want 200 (%.200s)", name, p, got.code, got.body)
			}
			assertNoSecrets(t, got.body, fx.ownerSession, fx.memberSession, fx.memberToken, fx.memberTokenHash, canaryToken)
		}
		for _, p := range badHistoryRoutes {
			if got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h); got.code != http.StatusBadRequest {
				t.Fatalf("%s %s: got %d, want 400", name, p, got.code)
			}
		}
		t.Logf("%-15s -> 200 on %d history routes, 400 on %d bad parameters", name, len(historyRoutes), len(badHistoryRoutes))
	}

	const unknown = "no-such-team-7q"
	for _, p := range append(append([]string{}, historyRoutes...), badHistoryRoutes...) {
		want := fetchAll(t, srv.URL+"/v1/teams/"+unknown+p, nil)
		if want.code != http.StatusNotFound {
			t.Fatalf("unknown slug %s: got %d, want 404", p, want.code)
		}
		for name, h := range map[string]map[string]string{
			"anonymous":          nil,
			"non-member session": session(fx.outsiderSession),
			"non-member bearer":  {"Authorization": "Bearer " + fx.outsiderToken},
			"garbage session":    session("mbs_" + strings.Repeat("B", 43)),
			"bearer and session": {"Authorization": "Bearer " + fx.memberToken, "X-Metiche-Browser-Session": fx.ownerSession},
		} {
			got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h)
			if got != want {
				t.Fatalf("%s, %s: distinguishable from an unknown team:\n got  %d %q %q\n want %d %q %q",
					p, name, got.code, got.contentType, got.body, want.code, want.contentType, want.body)
			}
		}
		t.Logf("%-62s anonymous, non-member session/bearer, garbage session, both -> %d %q identical to unknown team", p, want.code, want.body)
	}
}

// TestBoardHistoryPublicTeamIsReadableAnonymously.
func TestBoardHistoryPublicTeamIsReadableAnonymously(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PUBLIC)
	seedConflictHistory(t, db, fx)
	srv := newGuardedServer(t, db)

	for name, h := range map[string]map[string]string{
		"anonymous":       nil,
		"garbage session": session("mbs_garbage"),
	} {
		for _, p := range historyRoutes {
			got := fetchAll(t, srv.URL+"/v1/teams/"+fx.slug+p, h)
			if got.code != http.StatusOK || !json.Valid([]byte(got.body)) {
				t.Fatalf("%s %s on a PUBLIC team: %d %.200s", name, p, got.code, got.body)
			}
		}
		t.Logf("%s -> 200 on %d history routes", name, len(historyRoutes))
	}
}

// fetchAll is fetch without its 4KB cap: a history page is larger, and a
// canary hunt that reads the first 4KB of a body proves nothing about the rest.
func fetchAll(t *testing.T, target string, headers map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return response{code: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b.String()}
}

// ---------------------------------------------------------------- canary

// TestBoardHistoryNeverCarriesTheCanary reads every page of the conflict
// history and the event log, filtered and not, and both graph windows, and
// hunts for the canaries written into team_event's response_snapshot,
// idempotency_key and payload, and into conflict.evidence's intent summary.
func TestBoardHistoryNeverCarriesTheCanary(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	seedOldRun(t, db, fx)
	seedEventHistory(t, db, fx)
	seedConflictHistory(t, db, fx)
	seedGraphWindows(t, db, fx)
	srv := newBoardServer(t, db)

	// The needles are real rows, or this hunt checks nothing.
	var events, evidence int
	if err := db.QueryRow("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `idempotency_key` LIKE ? AND "+
		"CAST(`response_snapshot` AS CHAR) LIKE ? AND CAST(`payload` AS CHAR) LIKE ?",
		fx.teamID, "%"+canaryToken+"%", "%"+canarySession+"%", "%"+canaryToken+"%").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ? AND CAST(`evidence` AS CHAR) LIKE ?",
		fx.teamID, "%"+canaryToken+"%").Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if events != 304 || evidence != 2 {
		t.Fatalf("%d events and %d conflicts carry the canary, want 304 and 2", events, evidence)
	}

	var bodies []string
	for _, limit := range []int{1, 0} {
		_, pages := walkEvents(t, srv.URL, fx.slug, nil, limit, nil)
		bodies = append(bodies, pages...)
		_, pages = walkConflictHistory(t, srv.URL, fx.slug, nil, limit, nil)
		bodies = append(bodies, pages...)
	}
	for _, q := range []url.Values{{"kind": {"session_ended"}}, {"session": {"S-900"}}, {"session": {"S-1"}, "kind": {"intent_updated"}}} {
		_, pages := walkEvents(t, srv.URL, fx.slug, q, 3, nil)
		bodies = append(bodies, pages...)
	}
	_, pages := walkConflictHistory(t, srv.URL, fx.slug, url.Values{"status": {"dismissed"}}, 3, nil)
	bodies = append(bodies, pages...)
	for _, w := range []string{"24h", "7d"} {
		bodies = append(bodies, getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/graph?window="+w, nil))
	}

	served := map[string]int{}
	for _, b := range bodies {
		assertNoSecrets(t, b, canaryToken, canarySession, "TESTCANARY", `"payload"`, `"idempotency_key"`,
			`"response_snapshot"`, "a_summary", `"evidence"`)
		served["S-900 ended"] += strings.Count(b, `"S-900 ended"`)
		served["test event 250"] += strings.Count(b, `"test event 250"`)
		served["web/login.tsx path of HX-7"] += strings.Count(b, `"web/login.tsx","web/**"`)
		served["web/a.tsx path of GX-1"] += strings.Count(b, `"web/a.tsx","web/**"`)
	}
	for what, n := range served {
		if n == 0 {
			t.Fatalf("%s was never served, so the hunt proved nothing about its row", what)
		}
	}
	t.Logf("%d response bodies read; rows with the canaries served %v; no canary, payload, idempotency_key, response_snapshot or evidence", len(bodies), served)
}
