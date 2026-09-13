package webapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/backend/enums"
)

// Run history over a real MySQL: GET /v1/teams/{slug}/sessions, and the
// history block on GET /v1/teams/{slug}/sessions/{key}. Same DSN rule as the
// rest of this package (see dsnEnv).

// Token-shaped canaries, obvious fakes. They are written into the three
// team_event columns this API must never return — response_snapshot,
// idempotency_key and payload — and no response may carry them.
const (
	canaryToken   = "mtk_TESTCANARYnotarealtoken00000000000000000000"
	canarySession = "mbs_TESTCANARYnotarealsession000000000000000000"
)

type fixtureIDs struct {
	project, ana, bob, agentA, agentB string
}

// fixtureIDsFor finds the rows seedBoardVisibility created, so a test can add
// sessions to the same team.
func fixtureIDsFor(t *testing.T, db *sql.DB, teamID string) fixtureIDs {
	t.Helper()
	var ids fixtureIDs
	q := func(dst *string, query string, args ...any) {
		t.Helper()
		if err := db.QueryRow(query, args...).Scan(dst); err != nil {
			t.Fatalf("fixture lookup %q: %v", query, err)
		}
	}
	q(&ids.project, "SELECT `id` FROM `project` WHERE `team_uuid` = ? AND `key` = 'api'", teamID)
	q(&ids.ana, "SELECT `id` FROM `member` WHERE `team_uuid` = ? AND `key` = 'M-1'", teamID)
	q(&ids.bob, "SELECT `id` FROM `member` WHERE `team_uuid` = ? AND `key` = 'M-2'", teamID)
	q(&ids.agentA, "SELECT a.`id` FROM `agent` a JOIN `member` m ON m.`account_uuid` = a.`account_uuid` WHERE m.`id` = ?", ids.ana)
	q(&ids.agentB, "SELECT a.`id` FROM `agent` a JOIN `member` m ON m.`account_uuid` = a.`account_uuid` WHERE m.`id` = ?", ids.bob)
	return ids
}

type sessionsPage struct {
	Sequence   int64 `json:"sequence"`
	Sessions   []sessionSummaryWire
	NextCursor string `json:"next_cursor"`
}

// walkSessions pages through the whole history and returns every row in the
// order served, plus every raw body.
func walkSessions(t *testing.T, base, slug string, limit int, between func(page int)) ([]sessionSummaryWire, []string) {
	t.Helper()
	var (
		all    []sessionSummaryWire
		bodies []string
		cursor string
	)
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("more than 100 pages: the cursor is not advancing")
		}
		u := base + "/v1/teams/" + slug + "/sessions"
		q := url.Values{}
		if limit > 0 {
			q.Set("limit", fmt.Sprint(limit))
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		var p sessionsPage
		bodies = append(bodies, getJSON(t, u, &p))
		want := limit
		if want <= 0 {
			want = defaultSessionsPage
		}
		if want > maxSessionsPage {
			want = maxSessionsPage
		}
		if len(p.Sessions) > want {
			t.Fatalf("page %d has %d rows, over its limit %d", page, len(p.Sessions), want)
		}
		if p.NextCursor != "" && len(p.Sessions) != want {
			t.Fatalf("page %d has %d rows and a next cursor; only a full page may have one", page, len(p.Sessions))
		}
		all = append(all, p.Sessions...)
		if p.NextCursor == "" {
			return all, bodies
		}
		if between != nil {
			between(page)
		}
		cursor = p.NextCursor
	}
}

func insertSession(t *testing.T, db *sql.DB, teamID string, ids fixtureIDs, key string, status enums.SessionStatus, started any) string {
	t.Helper()
	id := newUUID(t)
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`goal`,`status`,`status_line`,`started_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?)",
		id, teamID, ids.project, ids.agentB, ids.bob, key, "history goal "+key, status.ToInt64(), "line "+key, started)
	return id
}

// TestSessionHistoryPagesEverySessionWithoutGapsOrDuplicates walks 233
// sessions — every status, ties within the same second, rows with no
// started_at, and a session started mid-walk — at three page sizes.
func TestSessionHistoryPagesEverySessionWithoutGapsOrDuplicates(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	ids := fixtureIDsFor(t, db, fx.teamID)
	srv := newBoardServer(t, db)

	statuses := []enums.SessionStatus{enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE,
		enums.SESSION_STATUS_ENDED, enums.SESSION_STATUS_ABANDONED}
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	for i := 0; i < 230; i++ {
		var started any = base.Add(time.Duration(i/7) * time.Minute) // seven per second: ties
		if i%23 == 0 {
			started = nil // sorts by created_at instead
		}
		insertSession(t, db, fx.teamID, ids, fmt.Sprintf("H-%d", i), statuses[i%len(statuses)], started)
	}

	// The order the endpoint promises, straight from the table.
	ordered := func() []string {
		t.Helper()
		var keys []string
		rows, err := db.Query("SELECT `key` FROM `session` WHERE `team_uuid` = ? "+
			"ORDER BY COALESCE(`started_at`, `created_at`) DESC, `id`", fx.teamID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
		}
		return keys
	}
	if n := len(ordered()); n != 233 {
		t.Fatalf("fixture has %d sessions, want 233", n)
	}

	for _, limit := range []int{100, 37, 0} {
		// Taken before the walk: the session inserted during it must not be
		// served by it.
		expected := ordered()
		lateKey := fmt.Sprintf("LATE-%d", limit)
		got, bodies := walkSessions(t, srv.URL, fx.slug, limit, func(page int) {
			if page == 0 {
				// A session starting mid-walk sorts ahead of every cursor: it
				// must not shift a row across the pages still to come.
				insertSession(t, db, fx.teamID, ids, lateKey, enums.SESSION_STATUS_LIVE, time.Now().UTC().Add(time.Hour))
			}
		})
		seen := map[string]bool{}
		keys := make([]string, 0, len(got))
		statusSeen := map[string]bool{}
		for _, s := range got {
			if seen[s.Key] {
				t.Fatalf("limit %d: %s served twice", limit, s.Key)
			}
			seen[s.Key] = true
			keys = append(keys, s.Key)
			statusSeen[s.Status] = true
		}
		if strings.Join(keys, ",") != strings.Join(expected, ",") {
			t.Fatalf("limit %d: served %d rows in a different order or with gaps\n got  %v\n want %v", limit, len(keys), keys, expected)
		}
		for _, st := range []string{"live", "stale", "ended", "abandoned"} {
			if !statusSeen[st] {
				t.Fatalf("limit %d: no %s session in the history", limit, st)
			}
		}
		for _, b := range bodies {
			assertNoSecrets(t, b)
		}
		t.Logf("limit %d: %d pages, %d sessions, no duplicates, no gaps, %s excluded from this walk", limit, len(bodies), len(keys), lateKey)
		expected = append([]string{lateKey}, expected...)
	}

	// limit is capped, not refused.
	var capped sessionsPage
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/sessions?limit=1000", &capped)
	if len(capped.Sessions) != maxSessionsPage || capped.NextCursor == "" {
		t.Fatalf("limit=1000 served %d rows (next %q), want %d and a cursor", len(capped.Sessions), capped.NextCursor, maxSessionsPage)
	}

	// The counts and the session fields, on the fixture's own three sessions.
	all, _ := walkSessions(t, srv.URL, fx.slug, 100, nil)
	byKey := map[string]sessionSummaryWire{}
	for _, s := range all {
		byKey[s.Key] = s
	}
	for key, want := range map[string]sessionCountsWire{
		"S-1": {Intents: 1, ClaimedPaths: 2, Conflicts: 1},
		"S-2": {Intents: 1, ClaimedPaths: 1, Conflicts: 1},
		"S-3": {Intents: 1, ClaimedPaths: 0, Conflicts: 0},
	} {
		if got := byKey[key].Counts; got != want {
			t.Fatalf("%s counts = %+v, want %+v", key, got, want)
		}
	}
	s1 := byKey["S-1"]
	if s1.MemberName != "Ana" || s1.AgentLabel != "claude-1" || s1.ClientKind != "claude-code" ||
		s1.ProjectKey != "api" || s1.Goal != "ship login" || s1.StatusLine != "writing the login handler" ||
		s1.Branch != "feat/auth" || s1.StartedAt == nil || s1.Status != "live" {
		t.Fatalf("S-1 = %+v", s1)
	}
	if s3 := byKey["S-3"]; s3.Status != "ended" || s3.Outcome != "succeeded" || s3.EndedAt == nil {
		t.Fatalf("S-3 = %+v", s3)
	}

	// A cursor this endpoint did not issue is a 400, for a reader the guard
	// already let in.
	for _, bad := range []string{"!!!", "czE", "eDF8MjAyNi0wMS0wMSAwMDowMDowMHxTLTE", strings.Repeat("A", 300)} {
		resp, err := http.Get(srv.URL + "/v1/teams/" + fx.slug + "/sessions?cursor=" + url.QueryEscape(bad))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("cursor %q: got %d, want 400", bad, resp.StatusCode)
		}
	}
}

// TestSessionHistoryGateOnAPrivateTeam: members read the history (browser
// session, or bearer as every board read allows); everybody else gets the
// unknown team's bytes, cursor or not.
func TestSessionHistoryGateOnAPrivateTeam(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PRIVATE)
	srv := newGuardedServer(t, db)

	var page sessionsPage
	for name, h := range map[string]map[string]string{
		"owner session":  session(fx.ownerSession),
		"member session": session(fx.memberSession),
		"member bearer":  {"Authorization": "Bearer " + fx.memberToken},
	} {
		got := fetch(t, srv.URL+"/v1/teams/"+fx.slug+"/sessions?limit=2", h)
		if got.code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200 (%s)", name, got.code, got.body)
		}
		if err := json.Unmarshal([]byte(got.body), &page); err != nil || len(page.Sessions) != 2 || page.NextCursor == "" {
			t.Fatalf("%s: page = %+v (%v)", name, page, err)
		}
		assertNoSecrets(t, got.body, fx.ownerSession, fx.memberSession, fx.memberToken, fx.memberTokenHash)
		t.Logf("%-15s -> %d, %d sessions, next cursor issued", name, got.code, len(page.Sessions))
	}

	const unknown = "no-such-team-7q"
	for _, q := range []string{"", "?limit=5", "?cursor=" + page.NextCursor, "?cursor=garbage"} {
		want := fetch(t, srv.URL+"/v1/teams/"+unknown+"/sessions"+q, nil)
		if want.code != http.StatusNotFound {
			t.Fatalf("unknown slug %q: got %d, want 404", q, want.code)
		}
		for name, h := range map[string]map[string]string{
			"anonymous":          nil,
			"non-member session": session(fx.outsiderSession),
			"non-member bearer":  {"Authorization": "Bearer " + fx.outsiderToken},
			"garbage session":    session("mbs_" + strings.Repeat("B", 43)),
			"bearer and session": {"Authorization": "Bearer " + fx.memberToken, "X-Metiche-Browser-Session": fx.ownerSession},
		} {
			got := fetch(t, srv.URL+"/v1/teams/"+fx.slug+"/sessions"+q, h)
			if got != want {
				t.Fatalf("sessions%s, %s: distinguishable from an unknown team:\n got  %d %q %q\n want %d %q %q",
					q, name, got.code, got.contentType, got.body, want.code, want.contentType, want.body)
			}
			t.Logf("sessions%-12.12s %-19s -> %d %q identical=%v", q, name, got.code, got.body, got == want)
		}
	}
}

// TestSessionHistoryPublicTeamIsReadableAnonymously.
func TestSessionHistoryPublicTeamIsReadableAnonymously(t *testing.T) {
	db := testDB(t)
	fx := seedBoardVisibility(t, db, enums.TEAM_VISIBILITY_PUBLIC)
	srv := newGuardedServer(t, db)

	for name, h := range map[string]map[string]string{
		"anonymous":       nil,
		"garbage session": session("mbs_garbage"),
	} {
		got := fetch(t, srv.URL+"/v1/teams/"+fx.slug+"/sessions", h)
		var p sessionsPage
		if got.code != http.StatusOK || json.Unmarshal([]byte(got.body), &p) != nil || len(p.Sessions) != 3 {
			t.Fatalf("%s on a PUBLIC team: %d %s", name, got.code, got.body)
		}
		t.Logf("%s -> %d, %d sessions", name, got.code, len(p.Sessions))
	}
}

// seedOldRun adds S-900: a session that ended four months ago, with two
// intents, two claims over three paths, a settled conflict, an unrelated
// conflict it was not in, and four events whose response_snapshot,
// idempotency_key and payload carry the canaries.
func seedOldRun(t *testing.T, db *sql.DB, fx seeded) {
	t.Helper()
	ids := fixtureIDsFor(t, db, fx.teamID)
	started := time.Now().UTC().Add(-120 * 24 * time.Hour).Truncate(time.Second)
	ended := started.Add(3 * time.Hour)

	s := newUUID(t)
	mustExec(t, db, "INSERT INTO `session` (`id`,`team_uuid`,`project_uuid`,`agent_uuid`,`member_uuid`,`key`,`branch`,`goal`,`status`,`status_line`,`started_at`,`last_heartbeat_at`,`ended_at`,`outcome`,`outcome_note`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		s, fx.teamID, ids.project, ids.agentB, ids.bob, "S-900", "feat/login-form", "build the login form",
		int64(enums.SESSION_STATUS_ENDED), "wrapping up", started, ended, ended,
		int64(enums.SESSION_OUTCOME_SUCCEEDED), "shipped the form behind the flag")

	i1, i2 := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,`revision`,`declared_at`,`started_at`,`ended_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		i1, fx.teamID, ids.project, s, ids.bob, "INT-900", "render the login form", 1, int64(enums.INTENT_STATUS_DONE), 2,
		started, started.Add(time.Minute), ended)
	mustExec(t, db, "INSERT INTO `intent` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`summary`,`kind`,`status`,`revision`,`declared_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		i2, fx.teamID, ids.project, s, ids.bob, "INT-901", "restyle the header", 1, int64(enums.INTENT_STATUS_ABANDONED), 1,
		started.Add(time.Hour))

	c1, c2 := newUUID(t), newUUID(t)
	mustExec(t, db, "INSERT INTO `claim` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`intent_uuid`,`key`,`mode`,`status`,`expires_at`,`hard_expires_at`,`released_at`,`created_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		c1, fx.teamID, ids.project, s, ids.bob, i1, "CL-900", int64(enums.CLAIM_MODE_WRITE), int64(enums.CLAIM_STATUS_RELEASED),
		started.Add(time.Hour), started.Add(4*time.Hour), started.Add(2*time.Hour), started)
	mustExec(t, db, "INSERT INTO `claim` (`id`,`team_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`key`,`mode`,`status`,`expires_at`,`hard_expires_at`,`created_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		c2, fx.teamID, ids.project, s, ids.bob, "CL-901", int64(enums.CLAIM_MODE_READ), int64(enums.CLAIM_STATUS_EXPIRED),
		started.Add(time.Hour), started.Add(4*time.Hour), started.Add(time.Minute))
	for _, p := range []struct{ claim, pattern string }{{c1, "web/login.tsx"}, {c1, "web/form/**"}, {c2, "docs/auth.md"}} {
		mustExec(t, db, "INSERT INTO `claim_path` (`id`,`claim_uuid`,`project_uuid`,`session_uuid`,`member_uuid`,`mode`,`status`,`expires_at`,`pattern`,`pattern_norm`,`kind`,`prefix`,`depth`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			newUUID(t), p.claim, ids.project, s, ids.bob, 2, 2, started.Add(time.Hour), p.pattern, p.pattern, 1, "web/", 2)
	}

	cf := newUUID(t)
	mustExec(t, db, "INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,`suggested_action`,`resolution`,`resolution_note`,`resolved_at`,`occurrence_count`,`first_detected_at`,`last_detected_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		cf, fx.teamID, ids.project, "CF-900", 1, "dedupe-"+cf[:8], 3, 4, 1,
		"Bob holds web/login.tsx; wait for his release", 4,
		"Bob released web/login.tsx at 14:02 and Ana picked it up after", started.Add(2*time.Hour), 1,
		started.Add(30*time.Minute), started.Add(30*time.Minute))
	mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
		newUUID(t), cf, fx.teamID, s, ids.agentB, ids.bob, 3, c1, 2)
	var s1 string
	if err := db.QueryRow("SELECT `id` FROM `session` WHERE `team_uuid` = ? AND `key` = 'S-1'", fx.teamID).Scan(&s1); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
		newUUID(t), cf, fx.teamID, s1, ids.agentA, ids.ana, 3, c1, 1)

	snapshot := `{"ok":true,"token":"` + canaryToken + `","session":"` + canarySession + `"}`
	for i, e := range []struct {
		kind       enums.EventKind
		structural int
		subject    string
		summary    string
	}{
		{enums.EVENT_KIND_SESSION_STARTED, 1, "S-900", "S-900 started"},
		{enums.EVENT_KIND_INTENT_DECLARED, 0, "INT-900", "INT-900 declared"},
		{enums.EVENT_KIND_CLAIM_RELEASED, 1, "CL-900", "CL-900 released"},
		{enums.EVENT_KIND_SESSION_ENDED, 1, "S-900", "S-900 ended"},
	} {
		mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`project_uuid`,`session_uuid`,`kind`,`structural`,`subject_key`,`summary`,`idempotency_key`,`payload`,`response_snapshot`,`occurred_at`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			newUUID(t), fx.teamID, int64(900+i), ids.project, s, e.kind.ToInt64(), e.structural, e.subject, e.summary,
			fmt.Sprintf("%s-%s-%d", canaryToken, canarySession, i),
			`{"secret":"`+canaryToken+`"}`, snapshot, started.Add(time.Duration(i)*time.Minute))
	}
	// And one on a live session, so the snapshot and the other pages have a
	// canary within reach too.
	mustExec(t, db, "INSERT INTO `team_event` (`id`,`team_uuid`,`sequence`,`project_uuid`,`session_uuid`,`kind`,`structural`,`subject_key`,`summary`,`idempotency_key`,`payload`,`response_snapshot`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		newUUID(t), fx.teamID, int64(950), ids.project, s1, int64(enums.EVENT_KIND_INTENT_UPDATED), 0, "INT-1", "INT-1 updated",
		canarySession+"-idem-950", `{"secret":"`+canarySession+`"}`, snapshot)
}

// TestRunDetailTellsTheWholeStoryOfAnOldEndedSession.
func TestRunDetailTellsTheWholeStoryOfAnOldEndedSession(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	seedOldRun(t, db, fx)
	srv := newBoardServer(t, db)

	var out sessionResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/sessions/S-900", &out)

	s := out.Session
	if s.Key != "S-900" || s.Status != "ended" || s.Outcome != "succeeded" ||
		s.OutcomeNote != "shipped the form behind the flag" || s.MemberName != "Beto" ||
		s.Goal != "build the login form" || s.EndedAt == nil || s.StartedAt == nil {
		t.Fatalf("session = %+v", s)
	}
	if len(s.Intents) != 0 || len(s.Claims) != 0 {
		t.Fatalf("an ended session holds nothing now, got intents %+v claims %+v", s.Intents, s.Claims)
	}

	h := out.History
	if len(h.Intents) != 2 ||
		h.Intents[0].Key != "INT-900" || h.Intents[0].Status != "done" || h.Intents[0].Summary != "render the login form" ||
		h.Intents[0].DeclaredAt == nil || h.Intents[0].StartedAt == nil || h.Intents[0].EndedAt == nil || h.Intents[0].Revision != 2 ||
		h.Intents[1].Key != "INT-901" || h.Intents[1].Status != "abandoned" || h.Intents[1].EndedAt != nil {
		t.Fatalf("history intents = %+v", h.Intents)
	}
	if len(h.Claims) != 2 {
		t.Fatalf("history claims = %+v", h.Claims)
	}
	cl900, cl901 := h.Claims[0], h.Claims[1]
	if cl900.Key != "CL-900" || cl900.Mode != "write" || cl900.Status != "released" || cl900.ReleasedAt == nil ||
		cl900.ClaimedAt == nil || cl900.ExpiresAt == nil || strings.Join(cl900.Paths, ",") != "web/form/**,web/login.tsx" {
		t.Fatalf("CL-900 = %+v", cl900)
	}
	if cl901.Key != "CL-901" || cl901.Mode != "read" || cl901.Status != "expired" || cl901.ReleasedAt != nil ||
		strings.Join(cl901.Paths, ",") != "docs/auth.md" {
		t.Fatalf("CL-901 = %+v", cl901)
	}
	if len(h.Conflicts) != 1 {
		t.Fatalf("history conflicts = %+v (CF-1 is not this session's)", h.Conflicts)
	}
	cf := h.Conflicts[0]
	if cf.Key != "CF-900" || cf.Status != enums.ConflictStatus(4).String() || cf.Kind != "path_overlap" ||
		cf.Severity != enums.ConflictSeverity(3).String() || cf.Resolution != enums.ConflictResolution(4).String() ||
		cf.ResolutionNote != "Bob released web/login.tsx at 14:02 and Ana picked it up after" ||
		cf.ResolvedAt == nil || len(cf.Participants) != 2 {
		t.Fatalf("CF-900 = %+v", cf)
	}

	kinds := []string{}
	for _, e := range out.Events {
		kinds = append(kinds, fmt.Sprintf("%d:%s:%s", e.Sequence, e.Kind, e.SubjectKey))
	}
	if strings.Join(kinds, " ") != "900:session_started:S-900 901:intent_declared:INT-900 902:claim_released:CL-900 903:session_ended:S-900" {
		t.Fatalf("events = %v", kinds)
	}
	if out.Events[3].Summary != "S-900 ended" || out.Events[3].OccurredAt == nil {
		t.Fatalf("last event = %+v", out.Events[3])
	}

	// The events page; the history rides on every page.
	var p1, p2 sessionResponse
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/sessions/S-900?limit=2", &p1)
	if len(p1.Events) != 2 || p1.NextAfter == nil || *p1.NextAfter != 901 || len(p1.History.Claims) != 2 {
		t.Fatalf("page 1 = %+v", p1)
	}
	getJSON(t, srv.URL+"/v1/teams/"+fx.slug+fmt.Sprintf("/sessions/S-900?limit=2&after=%d", *p1.NextAfter), &p2)
	if len(p2.Events) != 2 || p2.Events[0].Sequence != 902 {
		t.Fatalf("page 2 = %+v", p2.Events)
	}

	// And its row in the list.
	all, _ := walkSessions(t, srv.URL, fx.slug, 100, nil)
	found := false
	for _, row := range all {
		if row.Key == "S-900" {
			found = true
			if row.Counts != (sessionCountsWire{Intents: 2, ClaimedPaths: 3, Conflicts: 1}) || row.OutcomeNote == "" {
				t.Fatalf("S-900 row = %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("S-900 is not in the history list")
	}
}

// TestRunHistoryNeverCarriesTheCanary reads every page of every run-history
// response, and the board's other reads, and hunts for the canaries written
// into response_snapshot, idempotency_key and payload.
func TestRunHistoryNeverCarriesTheCanary(t *testing.T) {
	db := testDB(t)
	fx := seedBoard(t, db)
	seedOldRun(t, db, fx)
	srv := newBoardServer(t, db)

	// The needles are real rows, or this test checks nothing.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND "+
		"`idempotency_key` LIKE ? AND CAST(`response_snapshot` AS CHAR) LIKE ? AND CAST(`payload` AS CHAR) LIKE ?",
		fx.teamID, "%"+canaryToken+"%", "%"+canarySession+"%", "%"+canaryToken+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("%d team_event rows carry every canary, want 4", n)
	}

	var bodies []string
	_, pages := walkSessions(t, srv.URL, fx.slug, 1, nil)
	bodies = append(bodies, pages...)
	for _, key := range []string{"S-900", "S-1", "S-2", "S-3"} {
		after := int64(0)
		for {
			var p sessionResponse
			bodies = append(bodies, getJSON(t, srv.URL+"/v1/teams/"+fx.slug+fmt.Sprintf("/sessions/%s?limit=1&after=%d", key, after), &p))
			if p.NextAfter == nil {
				break
			}
			after = *p.NextAfter
		}
	}
	bodies = append(bodies,
		getJSON(t, srv.URL+"/v1/teams/"+fx.slug, nil),
		getJSON(t, srv.URL+"/v1/teams/"+fx.slug+"/conflicts?status=all", nil))

	summaries := 0
	for _, b := range bodies {
		assertNoSecrets(t, b, canaryToken, canarySession, "TESTCANARY", `"payload"`, `"idempotency_key"`, `"response_snapshot"`)
		summaries += strings.Count(b, `"S-900 ended"`)
	}
	if summaries == 0 {
		t.Fatal("the event with the canaries was never served, so the hunt proved nothing")
	}
	t.Logf("%d response bodies read; no canary, no payload, no idempotency_key, no response_snapshot", len(bodies))
}
