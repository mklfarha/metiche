package web

import (
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The Decisions tab and /t/{slug}/decisions/history (history.go) against the
// stub backend's decisions and decisions/history routes. Names are Ana, Bob
// and test.

var (
	pastDecisionRE = regexp.MustCompile(`id="past-dec-(past-\d+)"`)
	tagRE          = regexp.MustCompile(`<[^>]+>`)
)

// textOf is a page as a person reads it: tags gone, whitespace collapsed.
func textOf(body string) string {
	return strings.Join(strings.Fields(html.UnescapeString(tagRE.ReplaceAllString(body, " "))), " ")
}

func stubAcceptedDecision(key, by string, alwaysShow bool, project string, judged [4]int, open ...string) map[string]any {
	d := map[string]any{
		"key": key, "title": "title of " + key, "statement": "statement of " + key, "status": "accepted",
		"always_show": alwaysShow, "revision": 1, "decided_by": by,
		"decided_at": historyBase.Format(time.RFC3339), "updated_at": historyBase.Format(time.RFC3339),
		"scope":  []any{"internal/auth/**"},
		"judged": map[string]any{"no_conflict": judged[0], "conflict": judged[1], "unsure": judged[2], "pending": judged[3]},
	}
	if project != "" {
		d["project_key"] = project
	}
	if open == nil {
		open = []string{}
	}
	d["open_conflicts"] = open
	return d
}

func stubPastDecision(i int) map[string]any {
	status := []string{"superseded", "revoked"}[i%2]
	at := historyBase.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
	d := map[string]any{
		"key": fmt.Sprintf("#past-%d", i), "title": fmt.Sprintf("past %d", i), "statement": fmt.Sprintf("test: past wording %d", i),
		"status": status, "always_show": false, "revision": 1, "decided_by": "Bob",
		"decided_at": historyBase.Format(time.RFC3339), "updated_at": at, "ended_at": at,
		"scope": []any{}, "project_key": "test",
		"judged": map[string]any{"no_conflict": 0, "conflict": 0, "unsure": 0, "pending": 0}, "open_conflicts": []any{},
	}
	if status == "superseded" {
		d["superseded_by"] = "#auth-jwt-cookie"
	}
	return d
}

// decisionsWorld is historyWorld with decisions: three in force on every
// board, and 60 past ones (#past-60 … #past-1, newest first) on the public and
// private teams, with two revisions of #auth-jwt-cookie.
func decisionsWorld(t *testing.T) *harness {
	t.Helper()
	x := historyWorld(t)
	var past []map[string]any
	for i := 60; i >= 1; i-- {
		past = append(past, stubPastDecision(i))
	}
	x.backend.setDecisions([]any{
		stubAcceptedDecision("#auth-jwt-cookie", "Ana", true, "", [4]int{5, 1, 0, 2}, "CF-1"),
		stubAcceptedDecision("#errors-rfc7807", "Bob", false, "test", [4]int{}),
	}, past, pubSlug, privSlug)
	for _, slug := range []string{pubSlug, privSlug} {
		x.backend.setDecisionRevisions(slug, "#auth-jwt-cookie", []map[string]any{
			{"sequence": 402, "kind": "decision_recorded", "summary": "Ana revised #auth-jwt-cookie (r2)", "statement": "test: the second wording", "occurred_at": "2026-09-01T02:00:00Z"},
			{"sequence": 311, "kind": "decision_recorded", "summary": "Ana recorded #auth-jwt-cookie", "statement": "test: the first wording", "occurred_at": "2026-09-01T01:00:00Z"},
		})
	}
	return x
}

func decisionsOlderLink(body string) string {
	at := strings.Index(body, `id="decisions-more"`)
	if at < 0 {
		return ""
	}
	return olderLink(body[at:])
}

// TestDecisionsTabShowsCardsInForceAndThePastPage: the decisions in force are
// the board's, the Past section is the history's first page with its cursor.
func TestDecisionsTabShowsCardsInForceAndThePastPage(t *testing.T) {
	x := decisionsWorld(t)
	rec := x.get("/t/" + pubSlug + "/decisions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	cardAt, pastAt := strings.Index(body, `id="dec-auth-jwt-cookie"`), strings.Index(body, `id="past"`)
	if cardAt < 0 || pastAt < cardAt || strings.Index(body, `id="dec-errors-rfc7807"`) < cardAt {
		t.Fatalf("want the always-show card first and both above Past (card %d, past %d)", cardAt, pastAt)
	}
	rows := pastDecisionRE.FindAllStringSubmatch(body, -1)
	if len(rows) != 50 || rows[0][1] != "past-60" || rows[49][1] != "past-11" {
		t.Fatalf("past rows = %d (%v … %v), want #past-60 … #past-11", len(rows), rows[0], rows[len(rows)-1])
	}
	if older := decisionsOlderLink(body); older != "/t/"+pubSlug+"/decisions/history?cursor=hd-50" {
		t.Fatalf("load-older = %q", older)
	}
	text := textOf(body)
	for _, want := range []string{"checked against 6 plans · 1 open conflict CF-1 · 2 waiting",
		"#auth-jwt-cookie active always-show team-wide", "#errors-rfc7807 active test", "superseded by #auth-jwt-cookie", "Load older decisions"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Decisions tab does not read %q", want)
		}
	}
	for _, want := range []string{`href="/t/` + pubSlug + `/conflicts#CF-1"`, `href="/t/` + pubSlug + `/decisions/history"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the Decisions tab has no %s", want)
		}
	}
	if n := x.backend.historyHits(pubSlug, "decisions/history"); n != 1 {
		t.Fatalf("decision history reads = %d, want 1", n)
	}
	if q := x.backend.lastHistoryQuery(pubSlug, "decisions/history"); q != "limit=50" {
		t.Fatalf("first page query = %q", q)
	}
}

// TestDecisionHistoryLoadOlderAppendsTheNextPage follows "Load older" as htmx
// does, to the end, and without JavaScript to a page that starts there.
func TestDecisionHistoryLoadOlderAppendsTheNextPage(t *testing.T) {
	x := decisionsWorld(t)
	first := x.get("/t/" + pubSlug + "/decisions").Body.String()
	seen := map[string]int{}
	for _, m := range pastDecisionRE.FindAllStringSubmatch(first, -1) {
		seen[m[1]]++
	}
	next, pages := decisionsOlderLink(first), 1
	for next != "" {
		rec := x.do(http.MethodGet, next, "", withHeader("HX-Request", "true"))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", next, rec.Code)
		}
		frag := rec.Body.String()
		if strings.Contains(frag, "<html") || strings.Contains(frag, `id="past"`) || strings.Contains(frag, `id="dec-auth-jwt-cookie"`) {
			t.Fatalf("an older page's htmx answer carried more than rows:\n%.300s", frag)
		}
		if !strings.HasPrefix(strings.TrimSpace(frag), "<article") {
			t.Fatalf("fragment does not start with a row: %.80q", frag)
		}
		rows := pastDecisionRE.FindAllStringSubmatch(frag, -1)
		for _, m := range rows {
			seen[m[1]]++
		}
		pages++
		t.Logf("page %d (%s): %d rows, next %q", pages, next, len(rows), decisionsOlderLink(frag))
		next = decisionsOlderLink(frag)
	}
	if pages != 2 {
		t.Fatalf("pages = %d, want 2", pages)
	}
	for i := 1; i <= 60; i++ {
		if n := seen[fmt.Sprintf("past-%d", i)]; n != 1 {
			t.Fatalf("#past-%d drawn %d times across the pages, want 1", i, n)
		}
	}

	rec := x.get("/t/" + pubSlug + "/decisions/history?cursor=hd-50")
	page := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(page, "<html") || !strings.Contains(page, "Back to the newest past decisions") ||
		!strings.Contains(page, `id="past-dec-past-10"`) || strings.Contains(page, `id="past-dec-past-11"`) {
		t.Fatalf("the no-JavaScript older page: %d", rec.Code)
	}
	if q := x.backend.lastHistoryQuery(pubSlug, "decisions/history"); q != "cursor=hd-50&limit=50" {
		t.Fatalf("older page query = %q", q)
	}
}

// TestDecisionHistoryFiltersAndBadInput: the status filter is forwarded, the
// backend's 400 is a 400, and a malformed cursor or key never reaches it.
func TestDecisionHistoryFiltersAndBadInput(t *testing.T) {
	x := decisionsWorld(t)
	rec := x.get("/t/" + pubSlug + "/decisions/history?status=revoked")
	body := rec.Body.String()
	rows := pastDecisionRE.FindAllStringSubmatch(body, -1)
	// Odd numbers are revoked: #past-59 is the newest of the 30.
	if rec.Code != http.StatusOK || len(rows) != 30 || rows[0][1] != "past-59" || rows[29][1] != "past-1" || strings.Contains(body, "superseded by") {
		t.Fatalf("revoked filter: %d, %d rows", rec.Code, len(rows))
	}
	if !strings.Contains(body, `aria-current="true" href="/t/`+pubSlug+`/decisions/history?status=revoked"`) {
		t.Error("the revoked chip is not marked current")
	}
	if q := x.backend.lastHistoryQuery(pubSlug, "decisions/history"); q != "limit=50&status=revoked" {
		t.Fatalf("filter query = %q", q)
	}

	before := x.backend.historyHits(pubSlug, "decisions/history")
	for _, bad := range []string{"?cursor=bad!", "?status=Superseded", "?key=%3Cscript%3E", "?key=ab"} {
		if rec := x.get("/t/" + pubSlug + "/decisions/history" + bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want 400", bad, rec.Code)
		}
	}
	if n := x.backend.historyHits(pubSlug, "decisions/history"); n != before {
		t.Fatalf("malformed input reached the backend (%d reads)", n-before)
	}
	if rec := x.get("/t/" + pubSlug + "/decisions/history?cursor=hd-999"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a cursor the backend refuses: %d, want 400", rec.Code)
	}
}

// TestDecisionHistoryOneDecisionWithRevisions: ?key= reads one decision in any
// status with its revisions; an unknown key says so.
func TestDecisionHistoryOneDecisionWithRevisions(t *testing.T) {
	x := decisionsWorld(t)
	for _, key := range []string{"%23auth-jwt-cookie", "auth-jwt-cookie"} {
		rec := x.get("/t/" + pubSlug + "/decisions/history?key=" + key)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("key %s: %d", key, rec.Code)
		}
		for _, want := range []string{`id="dec-auth-jwt-cookie"`, "Revisions", "test: the second wording", "test: the first wording",
			"recorded <b>2026-09-01 02:00 UTC</b>", "Ana revised #auth-jwt-cookie (r2)"} {
			if !strings.Contains(body, want) {
				t.Errorf("key %s: the page does not show %q", key, want)
			}
		}
		if strings.Index(body, "test: the second wording") > strings.Index(body, "test: the first wording") {
			t.Errorf("key %s: revisions are not newest first", key)
		}
		if q := x.backend.lastHistoryQuery(pubSlug, "decisions/history"); q != "key=%23auth-jwt-cookie&limit=50" {
			t.Fatalf("key query = %q", q)
		}
	}
	rec := x.get("/t/" + pubSlug + "/decisions/history?key=%23past-7")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="past-dec-past-7"`) {
		t.Fatalf("a past decision by key: %d", rec.Code)
	}
	rec = x.get("/t/" + pubSlug + "/decisions/history?key=%23no-such-thing")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No decision on this team has that key.") {
		t.Fatalf("an unknown key: %d", rec.Code)
	}
}

// TestDecisionsTabSurvivesABackendWithoutHistory: a backend that predates
// /decisions/history answers it 404; the decisions in force still render and
// only Past says it could not be read.
func TestDecisionsTabSurvivesABackendWithoutHistory(t *testing.T) {
	x := decisionsWorld(t)
	x.backend.setDecisionHistoryGone(true)
	rec := x.get("/t/" + pubSlug + "/decisions")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `id="dec-auth-jwt-cookie"`) ||
		!strings.Contains(body, "The past decisions could not be loaded right now.") {
		t.Fatalf("status %d", rec.Code)
	}
}

// TestConflictKindFilterIncludesDecisionContradiction: the backend's live
// kinds now include decision_contradiction, and the filter offers and
// forwards it.
func TestConflictKindFilterIncludesDecisionContradiction(t *testing.T) {
	x := historyWorld(t)
	body := x.get("/t/" + pubSlug + "/conflicts").Body.String()
	chip := `href="/t/` + pubSlug + `/conflicts?kind=decision_contradiction"`
	if !strings.Contains(body, chip) || !strings.Contains(body, ">contradicts a decision</a>") {
		t.Fatalf("the kind filter does not offer decision_contradiction")
	}
	rec := x.get("/t/" + pubSlug + "/conflicts?kind=decision_contradiction")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `aria-current="true" `+chip) {
		t.Fatalf("filtering by decision_contradiction: %d", rec.Code)
	}
	if q := x.backend.lastHistoryQuery(pubSlug, "conflicts/history"); q != "kind=decision_contradiction&limit=50" {
		t.Fatalf("kind query = %q", q)
	}
}
