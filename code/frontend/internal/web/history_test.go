package web

import (
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// The board's other history views (history.go) against the stub backend's
// history routes (history_stub_test.go): Conflicts history, Activity, the
// rail's older events and the graph's time range. Names are Ana, Bob and test.

var (
	railBeforeRE = regexp.MustCompile(`"before":"(\d+)"`)
	pastKeyRE    = regexp.MustCompile(`id="past-(HX-\d+)"`)
	seqRE        = regexp.MustCompile(`data-seq="(\d+)"`)
)

func stubPastConflict(i int) map[string]any {
	statuses := []string{"resolved", "dismissed", "expired"}
	kinds := []string{"path_overlap", "contract_mismatch"}
	at := func(h int) string { return historyBase.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }
	c := map[string]any{
		"key": fmt.Sprintf("HX-%d", i), "kind": kinds[i%2], "severity": "medium", "status": statuses[i%3],
		"occurrence_count": 1 + i%3, "first_detected_at": at(i), "resolved_at": at(i + 1),
		"paths": []any{fmt.Sprintf("web/page-%d.tsx", i), "web/**"},
		"participants": []any{
			map[string]any{"session_key": fmt.Sprintf("S-%d", i), "member_name": "Ana", "agent_label": "claude-1", "role": "holder", "subject_kind": "claim"},
			map[string]any{"session_key": fmt.Sprintf("S-%d", 1000+i), "member_name": "Bob", "agent_label": "claude-2", "role": "challenger", "subject_kind": "claim"},
		},
	}
	if statuses[i%3] == "resolved" {
		c["resolution"] = "yielded"
		c["resolution_note"] = fmt.Sprintf("test: Bob released web/page-%d.tsx", i)
	}
	return c
}

func stubHistoryEvent(seq int64) map[string]any {
	e := map[string]any{
		"sequence": seq, "kind": stubEventKinds[seq%4], "structural": seq%2 == 0,
		"subject_key": fmt.Sprintf("SUB-%d", seq), "summary": fmt.Sprintf("test event %d", seq),
		"occurred_at": historyBase.Add(time.Duration(seq) * time.Minute).Format(time.RFC3339),
	}
	switch seq % 3 {
	case 0:
		e["session_key"], e["member_name"], e["agent_label"] = "S-1", "Ana", "claude-1"
	case 1:
		e["session_key"], e["member_name"], e["agent_label"] = "S-2", "Bob", "claude-2"
	}
	return e
}

// stubGraph24h: S-501 (Ana) and S-502 (Bob) both in web/ at the same time;
// S-501 and S-503 in api/ one after the other; S-502 alone in docs/; and a
// high-severity conflict that ended about web/a.tsx.
func stubGraph24h() map[string]any {
	at := func(h int) string { return historyBase.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }
	session := func(key, name, label, status string) map[string]any {
		return map[string]any{"key": key, "member_name": name, "agent_label": label, "status": status,
			"goal": "goal of " + key, "started_at": at(1)}
	}
	hold := func(session, path string, from, until int) map[string]any {
		h := map[string]any{"session_key": session, "claim_key": "CL-" + session, "mode": "write", "path": path,
			"status": "released", "held_from": at(from)}
		if until > 0 {
			h["held_until"] = at(until)
		} else {
			h["status"] = "held"
		}
		return h
	}
	return map[string]any{
		"window": "24h", "from": at(0), "to": at(24),
		"sessions": []any{session("S-501", "Ana", "claude-1", "ended"), session("S-502", "Bob", "claude-2", "live"), session("S-503", "Ana", "claude-3", "ended")},
		"holds": []any{
			hold("S-501", "api/x.go", 8, 9), hold("S-503", "api/y.go", 10, 11),
			hold("S-501", "web/a.tsx", 10, 12), hold("S-502", "web/b.tsx", 11, 13),
			hold("S-502", "docs/readme.md", 14, 0),
		},
		"conflicts": []any{map[string]any{"key": "CF-77", "kind": "path_overlap", "severity": "high", "status": "resolved",
			"resolution": "split", "resolution_note": "test: Ana moved to web/c.tsx", "first_detected_at": at(11), "resolved_at": at(12),
			"paths": []any{"web/a.tsx", "web/**"},
			"participants": []any{map[string]any{"session_key": "S-501", "member_name": "Ana", "agent_label": "claude-1", "role": "holder"},
				map[string]any{"session_key": "S-502", "member_name": "Bob", "agent_label": "claude-2", "role": "challenger"}}}},
	}
}

// historyWorld is x.world with a history: every live board holds one open
// conflict, CF-1; the public and private teams have 120 past conflicts
// (HX-120 … HX-1, newest first), 250 events (sequence 250 … 1) and a 24h graph.
func historyWorld(t *testing.T) *harness {
	t.Helper()
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	x.backend.mu.Lock()
	x.backend.conflicts = []any{map[string]any{"key": "CF-1", "kind": "path_overlap", "severity": "high", "status": "open",
		"suggested_action": "test: wait for Ana to release app/auth.go", "first_detected_at": historyBase.Format(time.RFC3339),
		"paths": []any{"app/auth.go"}, "participants": []any{}}}
	x.backend.mu.Unlock()

	var conflicts, events []map[string]any
	for i := 120; i >= 1; i-- {
		conflicts = append(conflicts, stubPastConflict(i))
	}
	for seq := int64(250); seq >= 1; seq-- {
		events = append(events, stubHistoryEvent(seq))
	}
	for _, slug := range []string{pubSlug, privSlug} {
		x.backend.setConflictHistory(slug, conflicts)
		x.backend.setEventHistory(slug, events)
		x.backend.setGraphWindow(slug, "24h", stubGraph24h())
	}
	return x
}

// ---------------------------------------------------------------- conflicts

// TestConflictsHistoryListsPastConflictsBelowTheOpenOnes: the open conflict
// is the hub's, on top as before; the History is the backend's first page.
func TestConflictsHistoryListsPastConflictsBelowTheOpenOnes(t *testing.T) {
	x := historyWorld(t)
	rec := x.get("/t/" + pubSlug + "/conflicts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	openAt, histAt := strings.Index(body, `id="CF-1"`), strings.Index(body, `id="history"`)
	if openAt < 0 || histAt < openAt {
		t.Fatalf("want the open conflict above the history (open %d, history %d)", openAt, histAt)
	}
	keys := pastKeyRE.FindAllStringSubmatch(body, -1)
	if len(keys) != 50 || keys[0][1] != "HX-120" || keys[49][1] != "HX-71" {
		t.Fatalf("history rows = %d (%v … %v), want HX-120 … HX-71", len(keys), keys[0], keys[len(keys)-1])
	}
	if older := olderLink(body); older != "/t/"+pubSlug+"/conflicts?cursor=hc-50" {
		t.Fatalf("load-older = %q", older)
	}
	for _, want := range []string{
		"History", "Load older conflicts", "every past status", `href="/t/` + pubSlug + `/conflicts?kind=contract_mismatch"`,
		"web/page-120.tsx", runHref(pubSlug, "S-120"), runHref(pubSlug, "S-1120"), "Ana · claude-1", "Bob · claude-2",
		"how it ended", "test: Bob released web/page-120.tsx", "yielded", "first detected <b>2026-09-06 00:00 UTC</b>",
		"· ended <b>2026-09-06 01:00 UTC</b>", `data-status="dismissed"`, `data-status="expired"`,
	} {
		if !strings.Contains(body, want) && !strings.Contains(body, html.EscapeString(want)) {
			t.Errorf("the page does not show %q", want)
		}
	}
	if n := x.backend.historyHits(pubSlug, "conflicts/history"); n != 1 {
		t.Fatalf("conflict history reads = %d, want 1", n)
	}
}

// TestConflictsHistoryLoadOlderAppendsTheNextPage follows "Load older" as
// htmx does, to the end.
func TestConflictsHistoryLoadOlderAppendsTheNextPage(t *testing.T) {
	x := historyWorld(t)
	first := x.get("/t/" + pubSlug + "/conflicts").Body.String()
	seen := map[string]int{}
	for _, m := range pastKeyRE.FindAllStringSubmatch(first, -1) {
		seen[m[1]]++
	}
	next, pages := olderLink(first), 1
	for next != "" {
		rec := x.do(http.MethodGet, next, "", withHeader("HX-Request", "true"))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", next, rec.Code)
		}
		frag := rec.Body.String()
		if strings.Contains(frag, "<html") || strings.Contains(frag, `id="history"`) || strings.Contains(frag, `id="CF-1"`) {
			t.Fatalf("an older page's htmx answer carried more than rows:\n%.300s", frag)
		}
		if !strings.HasPrefix(strings.TrimSpace(frag), "<article") {
			t.Fatalf("fragment does not start with a row: %.80q", frag)
		}
		rows := pastKeyRE.FindAllStringSubmatch(frag, -1)
		for _, m := range rows {
			seen[m[1]]++
		}
		pages++
		t.Logf("page %d (%s): %d rows, next %q", pages, next, len(rows), olderLink(frag))
		next = olderLink(frag)
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
	for i := 1; i <= 120; i++ {
		if n := seen[fmt.Sprintf("HX-%d", i)]; n != 1 {
			t.Fatalf("HX-%d drawn %d times across the pages, want 1", i, n)
		}
	}
	rec := x.get("/t/" + pubSlug + "/conflicts?cursor=hc-50")
	page := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(page, "<html") || !strings.Contains(page, "Back to the newest conflicts") ||
		!strings.Contains(page, `id="past-HX-70"`) || strings.Contains(page, `id="past-HX-71"`) {
		t.Fatalf("no-JS older page: %d", rec.Code)
	}
	if rec := x.do(http.MethodGet, "/t/"+pubSlug+"/conflicts?cursor=bad%20cursor", "", withHeader("HX-Request", "true")); rec.Code != http.StatusBadRequest {
		t.Fatalf("a junk cursor: %d, want 400", rec.Code)
	}
}

// TestConflictsHistoryFilters: the filters reach the backend, the rows and the
// next page keep them, and a value nobody accepts is a 400.
func TestConflictsHistoryFilters(t *testing.T) {
	x := historyWorld(t)
	rec := x.get("/t/" + pubSlug + "/conflicts?status=dismissed&kind=path_overlap")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if q := x.backend.lastHistoryQuery(pubSlug, "conflicts/history"); !strings.Contains(q, "status=dismissed") || !strings.Contains(q, "kind=path_overlap") {
		t.Fatalf("backend query = %q", q)
	}
	statuses := regexp.MustCompile(`data-status="([a-z]+)"`).FindAllStringSubmatch(body, -1)
	if len(statuses) != 20 || olderLink(body) != "" {
		t.Fatalf("dismissed path_overlap rows = %d (older %q), want 20 and no older page", len(statuses), olderLink(body))
	}
	for _, s := range statuses {
		if s[1] != "dismissed" {
			t.Fatalf("a %s conflict under status=dismissed", s[1])
		}
	}
	for _, want := range []string{
		`aria-current="true" href="/t/` + pubSlug + `/conflicts?kind=path_overlap&status=dismissed">dismissed</a>`,
		`aria-current="true" href="/t/` + pubSlug + `/conflicts?kind=path_overlap&status=dismissed">path overlap</a>`,
		`href="/t/` + pubSlug + `/conflicts?kind=path_overlap">every past status</a>`,
		`href="/t/` + pubSlug + `/conflicts?status=dismissed">every kind</a>`,
	} {
		if !strings.Contains(html.UnescapeString(body), want) {
			t.Errorf("the filtered page does not show %q", want)
		}
	}

	kindOnly := x.get("/t/" + pubSlug + "/conflicts?kind=path_overlap").Body.String()
	if older := olderLink(kindOnly); older != "/t/"+pubSlug+"/conflicts?cursor=hc-50&kind=path_overlap" {
		t.Fatalf("filtered load-older = %q", older)
	}

	before := x.backend.historyHits(pubSlug, "conflicts/history")
	if rec := x.get("/t/" + pubSlug + "/conflicts?status=Open!"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status=Open!: %d, want 400", rec.Code)
	}
	if n := x.backend.historyHits(pubSlug, "conflicts/history"); n != before {
		t.Fatal("a malformed filter reached the backend")
	}
	if rec := x.get("/t/" + pubSlug + "/conflicts?status=open"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status=open, refused by the backend: %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------- activity

// TestActivityListsTheLogAndLoadsOlder.
func TestActivityListsTheLogAndLoadsOlder(t *testing.T) {
	x := historyWorld(t)
	rec := x.get("/t/" + pubSlug + "/activity")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	seqs := seqRE.FindAllStringSubmatch(body, -1)
	if len(seqs) != 100 || seqs[0][1] != "250" || seqs[99][1] != "151" {
		t.Fatalf("activity rows = %d, want 250 … 151", len(seqs))
	}
	if older := olderLink(body); older != "/t/"+pubSlug+"/activity?before=151" {
		t.Fatalf("load-older = %q", older)
	}
	for _, want := range []string{`class="on"`, ">Activity", "Every event this team has logged", `href="/t/` + pubSlug + `/activity?kind=claim_released"`,
		"test event 250", runHref(pubSlug, "S-1"), "Ana · claude-1", "2026-09-01 04:10 UTC", "Load older events",
		`href="/t/` + pubSlug + `/activity?session=S-1">only this run</a>`, "every session"} {
		if !strings.Contains(html.UnescapeString(body), want) {
			t.Errorf("the page does not show %q", want)
		}
	}

	seen := map[string]int{}
	for _, m := range seqs {
		seen[m[1]]++
	}
	next, pages := olderLink(body), 1
	for next != "" {
		rec := x.do(http.MethodGet, next, "", withHeader("HX-Request", "true"))
		frag := rec.Body.String()
		if rec.Code != http.StatusOK || strings.Contains(frag, "<html") || !strings.HasPrefix(strings.TrimSpace(frag), "<li") {
			t.Fatalf("GET %s: %d %.80q", next, rec.Code, frag)
		}
		for _, m := range seqRE.FindAllStringSubmatch(frag, -1) {
			seen[m[1]]++
		}
		pages++
		next = olderLink(frag)
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
	for i := 1; i <= 250; i++ {
		if n := seen[fmt.Sprint(i)]; n != 1 {
			t.Fatalf("event %d drawn %d times, want 1", i, n)
		}
	}

	filtered := x.get("/t/" + pubSlug + "/activity?kind=claim_released&session=S-2")
	fb := filtered.Body.String()
	if q := x.backend.lastHistoryQuery(pubSlug, "events"); !strings.Contains(q, "kind=claim_released") || !strings.Contains(q, "session=S-2") {
		t.Fatalf("backend query = %q", q)
	}
	rows := seqRE.FindAllStringSubmatch(fb, -1)
	if filtered.Code != http.StatusOK || len(rows) == 0 || !strings.Contains(fb, "S-2 ✕") || strings.Contains(fb, "only this run") ||
		!strings.Contains(html.UnescapeString(fb), `href="/t/`+pubSlug+`/activity?kind=claim_released">S-2 ✕`) {
		t.Fatalf("filtered activity: %d, %d rows", filtered.Code, len(rows))
	}
	for _, m := range rows {
		var seq int
		_, _ = fmt.Sscan(m[1], &seq)
		if seq%4 != 2 || seq%3 != 1 {
			t.Fatalf("event %d is not a claim_released on S-2", seq)
		}
	}

	hits := x.backend.historyHits(pubSlug, "events")
	for _, bad := range []string{"before=abc", "before=0", "session=bad%20key", "kind=Nope"} {
		if rec := x.get("/t/" + pubSlug + "/activity?" + bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want 400", bad, rec.Code)
		}
	}
	if n := x.backend.historyHits(pubSlug, "events"); n != hits {
		t.Fatal("a malformed parameter reached the backend")
	}
	if rec := x.get("/t/" + pubSlug + "/activity?kind=nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("kind=nope, refused by the backend: %d, want 400", rec.Code)
	}
}

// TestRailLoadsOlderEvents: a live board's rail offers older events, and
// following its control to the end draws every event once, each page oldest
// first so it lands above the rows already there.
func TestRailLoadsOlderEvents(t *testing.T) {
	x := historyWorld(t)
	board := html.UnescapeString(x.get("/t/" + pubSlug).Body.String())
	if !strings.Contains(board, `id="rail-more"`) || !strings.Contains(board, `href="/t/`+pubSlug+`/activity?before=4"`) ||
		!strings.Contains(board, `hx-get="/t/`+pubSlug+`/timeline"`) {
		t.Fatalf("the board's rail has no load-older control:\n%s", board[strings.Index(board, `id="timeline"`):])
	}
	if m := railBeforeRE.FindStringSubmatch(board); m == nil || m[1] != "4" {
		t.Fatalf("rail before = %v, want 4 (after the snapshot's sequence 3)", m)
	}

	seen := map[string]int{}
	before, pages := "251", 0
	for before != "" {
		rec := x.do(http.MethodGet, "/t/"+pubSlug+"/timeline?before="+before, "", withHeader("HX-Request", "true"))
		frag := html.UnescapeString(rec.Body.String())
		if rec.Code != http.StatusOK || strings.Contains(frag, "<html") {
			t.Fatalf("timeline before %s: %d", before, rec.Code)
		}
		rows := seqRE.FindAllStringSubmatch(frag, -1)
		for i, m := range rows {
			seen[m[1]]++
			if i > 0 {
				var a, b int
				_, _ = fmt.Sscan(rows[i-1][1], &a)
				_, _ = fmt.Sscan(m[1], &b)
				if b <= a {
					t.Fatalf("rail page before %s is not oldest first: %s then %s", before, rows[i-1][1], m[1])
				}
			}
		}
		pages++
		next := ""
		if m := railBeforeRE.FindStringSubmatch(frag); m != nil {
			if !strings.HasPrefix(strings.TrimSpace(frag), `<li class="rail-more"`) {
				t.Fatalf("the next control is not above the rows: %.80q", frag)
			}
			next = m[1]
		}
		t.Logf("rail page %d before %s: %d rows, next before %q", pages, before, len(rows), next)
		before = next
	}
	for i := 1; i <= 250; i++ {
		if n := seen[fmt.Sprint(i)]; n != 1 {
			t.Fatalf("event %d drawn %d times on the rail, want 1", i, n)
		}
	}
	if rec := x.get("/t/" + pubSlug + "/timeline"); rec.Code != http.StatusBadRequest {
		t.Fatalf("timeline with no before: %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------- graph

// TestGraphTimeRanges: live stays the default and asks nothing; a past window
// crosses only where holds overlapped, marks the hand-off and the conflict.
func TestGraphTimeRanges(t *testing.T) {
	x := historyWorld(t)
	live := x.get("/t/" + pubSlug + "/graph")
	lb := live.Body.String()
	if live.Code != http.StatusOK || !strings.Contains(lb, `class="win-select"`) || !strings.Contains(lb, `id="graph"`) ||
		!strings.Contains(lb, `href="/t/`+pubSlug+`/graph?window=24h"`) || !strings.Contains(lb, `aria-current="page" href="/t/`+pubSlug+`/graph"`) {
		t.Fatalf("live graph: %d", live.Code)
	}
	if n := x.backend.historyHits(pubSlug, "graph"); n != 0 {
		t.Fatalf("the live graph read %d past windows", n)
	}

	past := x.get("/t/" + pubSlug + "/graph?window=24h")
	pb := past.Body.String()
	if past.Code != http.StatusOK {
		t.Fatalf("24h graph: %d", past.Code)
	}
	for _, want := range []string{
		`id="graph-window"`, `aria-current="page" href="/t/` + pubSlug + `/graph?window=24h"`, "the last 24 hours",
		"3 sessions", "1 area held by two sessions at once", "1 area held in turn", "1 conflict",
		"gn-cross", "gn-handoff", "gn-sev-high", runHref(pubSlug, "S-501"), "CF-77", "test: Ana moved to web/c.tsx",
		"held by two sessions at different times",
	} {
		if !strings.Contains(pb, want) {
			t.Errorf("the 24h graph does not show %q", want)
		}
	}
	if strings.Contains(pb, `id="graph"`) {
		t.Fatal("a past window carries the live graph container the stream repaints")
	}
	if n := x.backend.historyHits(pubSlug, "graph"); n != 1 {
		t.Fatalf("graph reads = %d, want 1", n)
	}

	week := x.get("/t/" + pubSlug + "/graph?window=7d")
	if week.Code != http.StatusOK || !strings.Contains(week.Body.String(), "Nothing ran in the last 7 days") {
		t.Fatalf("empty 7d graph: %d", week.Code)
	}
	if rec := x.get("/t/" + pubSlug + "/graph?window=30d"); rec.Code != http.StatusBadRequest {
		t.Fatalf("window=30d: %d, want 400", rec.Code)
	}
	if n := x.backend.historyHits(pubSlug, "graph"); n != 2 {
		t.Fatalf("graph reads = %d, want 2 (the bad window never asked)", n)
	}
}

// ---------------------------------------------------------------- no calls

// TestDemoBoardsMakeNoHistoryCalls: a demo board keeps its pages and asks the
// backend nothing — even one whose feed could, and for a signed-in viewer.
func TestDemoBoardsMakeNoHistoryCalls(t *testing.T) {
	x := historyWorld(t)
	x.backend.setPublic("demo-live", "Recorded Crew")
	rec := &feed.Live{BaseURL: x.stub.URL, Slug: "demo-live", Client: x.stub.Client(), Logger: quiet()}
	if _, err := x.srv.AddDemoTeam(x.ctx, "demo-live", "Recorded Crew", "", rec); err != nil {
		t.Fatal(err)
	}
	x.backend.setConflictHistory("demo-live", []map[string]any{stubPastConflict(7)})
	x.backend.setEventHistory("demo-live", []map[string]any{stubHistoryEvent(9)})
	x.backend.setGraphWindow("demo-live", "24h", stubGraph24h())

	for _, slug := range []string{"demo-live", "demo"} {
		for _, who := range []string{"", memberSecret} {
			for _, path := range []string{"", "/conflicts", "/conflicts?cursor=hc-50&kind=path_overlap", "/activity",
				"/activity?before=5&kind=claim_released", "/timeline?before=5", "/graph?window=24h", "/graph?window=7d"} {
				r := x.do(http.MethodGet, "/t/"+slug+path, "", withHeader("HX-Request", "true"), func(r *http.Request) {
					if who != "" {
						withCookie(who)(r)
					}
				})
				body := r.Body.String()
				if r.Code != http.StatusOK && r.Code != http.StatusNotFound {
					t.Fatalf("%s%s (signed in %v): %d", slug, path, who != "", r.Code)
				}
				for _, never := range []string{`id="rail-more"`, `id="history"`, `class="win-select"`, `id="past-`, "test event 9", `id="graph-window"`} {
					if strings.Contains(body, never) {
						t.Fatalf("%s%s drew %q on a demo board", slug, path, never)
					}
				}
				t.Logf("/t/%s%-42s signed-in=%-5v -> %d", slug, path, who != "", r.Code)
			}
		}
		if n := x.backend.allHistoryHits(slug); n != 0 {
			t.Fatalf("demo board %s made %d history requests", slug, n)
		}
	}
}

// TestContractsAndDecisionsMakeNoHistoryCalls: nothing writes contracts or
// decisions yet, so their pages keep their empty states and ask the backend
// for no history, for an anonymous viewer of a public team and a member of a
// private one.
func TestContractsAndDecisionsMakeNoHistoryCalls(t *testing.T) {
	x := historyWorld(t)
	for _, c := range []struct{ slug, who string }{{pubSlug, ""}, {privSlug, memberSecret}} {
		for _, p := range []struct{ path, empty string }{
			{"/contracts", "No contracts published yet"},
			{"/contracts?cursor=hc-50&window=24h&before=9", "No contracts published yet"},
			{"/decisions", "No decisions recorded"},
			{"/decisions?cursor=hc-50&window=7d&before=9", "No decisions recorded"},
		} {
			rec := x.getAs(c.who, "/t/"+c.slug+p.path)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), p.empty) {
				t.Fatalf("%s%s: %d, want 200 with %q", c.slug, p.path, rec.Code, p.empty)
			}
		}
		if n := x.backend.allHistoryHits(c.slug); n != 0 {
			t.Fatalf("Contracts and Decisions on %s made %d history requests", c.slug, n)
		}
		t.Logf("%s (member %v): Contracts and Decisions rendered their empty states with 0 history requests", c.slug, c.who != "")
	}
}

// TestPrivateHistoryIsNotFoundForAnybodyButAMember: anonymous and a signed-in
// non-member get the unknown team's 404 on every history URL and no history is
// read for them; a member reads it with their own session.
func TestPrivateHistoryIsNotFoundForAnybodyButAMember(t *testing.T) {
	x := historyWorld(t)
	paths := []string{"/conflicts", "/conflicts?cursor=hc-50", "/activity", "/activity?before=151&kind=claim_released",
		"/timeline?before=151", "/graph?window=24h", "/graph?window=7d"}

	for _, who := range []string{"", outsiderSecret} {
		for _, p := range paths {
			r := x.do(http.MethodGet, "/t/"+privSlug+p, "", withHeader("HX-Request", "true"), func(r *http.Request) {
				if who != "" {
					withCookie(who)(r)
				}
			})
			body := r.Body.String()
			if r.Code != http.StatusNotFound {
				t.Fatalf("%s (outsider %v): %d, want 404", p, who != "", r.Code)
			}
			if strings.Contains(body, "HX-120") || strings.Contains(body, "test event") || strings.Contains(body, privName) || strings.Contains(body, "S-501") {
				t.Fatalf("%s leaked the private history in its 404", p)
			}
			if who == "" {
				want := x.do(http.MethodGet, "/t/"+missing+p, "", withHeader("HX-Request", "true")).Body.String()
				if strings.ReplaceAll(body, privSlug, missing) != want {
					t.Fatalf("%s: anonymous 404 differs from an unknown team's", p)
				}
			}
			t.Logf("%-42s outsider=%-5v -> %d", p, who != "", r.Code)
		}
	}
	if n := x.backend.allHistoryHits(privSlug); n != 0 {
		t.Fatalf("%d history requests were made for viewers who may not read the team", n)
	}

	for _, c := range []struct{ path, want string }{
		{"/conflicts", `id="past-HX-120"`},
		{"/activity", "test event 250"},
		{"/graph?window=24h", "1 area held by two sessions at once"},
	} {
		rec := x.getAs(memberSecret, "/t/"+privSlug+c.path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("member's %s: %d", c.path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
			t.Fatalf("member's private %s Cache-Control = %q", c.path, cc)
		}
	}
	frag := x.do(http.MethodGet, "/t/"+privSlug+"/timeline?before=151", "", withHeader("HX-Request", "true"), withCookie(memberSecret))
	if frag.Code != http.StatusOK || !strings.Contains(frag.Body.String(), "test event 150") {
		t.Fatalf("member's rail page: %d", frag.Code)
	}
	x.backend.mu.Lock()
	sawBearer := x.backend.login.sawBearer
	x.backend.mu.Unlock()
	if sawBearer {
		t.Fatal("the backend received a bearer")
	}
}
