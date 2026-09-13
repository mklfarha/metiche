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

// The Runs page with a history (runs.go) against the stub backend's history
// routes (runs_stub_test.go). Names and values are obvious fakes.

var (
	historyBase = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	olderRE     = regexp.MustCompile(`hx-get="([^"]+)"`)
)

func runHref(slug, key string) string { return `href="/t/` + slug + `/runs/` + key + `"` }

func stubRunRow(key, status string, started time.Time) map[string]any {
	row := map[string]any{
		"key": key, "project_key": "web", "member_key": "M-2", "member_name": "Bob",
		"agent_label": "claude-2", "client_kind": "claude-code",
		"goal": "goal of " + key, "status": status, "status_line": "line of " + key,
		"started_at": started.UTC().Format(time.RFC3339),
		"counts":     map[string]any{"intents": 2, "claimed_paths": 3, "conflicts": 1},
	}
	if status == "ended" {
		row["ended_at"] = started.Add(90 * time.Minute).UTC().Format(time.RFC3339)
		row["outcome"] = "succeeded"
	}
	return row
}

// runsWorld is x.world with a history. Every team's hub holds two live
// sessions: S-120, the newest run, and S-30, an old run still going. The
// backend's history for the public and the private team is S-120 … S-1,
// newest first — three pages of 50.
func runsWorld(t *testing.T) *harness {
	t.Helper()
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	live := func(key string, started time.Time) map[string]any {
		return map[string]any{"key": key, "member_key": "M-1", "member_name": "Ana", "agent_label": "claude-1",
			"status": "live", "goal": "goal of " + key, "started_at": started.Format(time.RFC3339),
			"intents": []any{}, "claims": []any{}}
	}
	x.backend.mu.Lock()
	x.backend.sessions = []any{live("S-120", historyBase.Add(120*time.Hour)), live("S-30", historyBase.Add(30*time.Hour))}
	x.backend.mu.Unlock()

	rows := make([]map[string]any, 0, 120)
	for i := 120; i >= 1; i-- {
		status := "ended"
		if i == 120 || i == 30 {
			status = "live"
		}
		rows = append(rows, stubRunRow(fmt.Sprintf("S-%d", i), status, historyBase.Add(time.Duration(i)*time.Hour)))
	}
	for _, slug := range []string{pubSlug, privSlug} {
		x.backend.setRuns(slug, rows)
		x.backend.setRunDetail(slug, "S-7", stubRunDetail())
	}
	return x
}

func stubRunDetail() map[string]any {
	at := func(h int) string { return historyBase.Add(time.Duration(h) * time.Minute).Format(time.RFC3339) }
	session := stubRunRow("S-7", "ended", historyBase.Add(7*time.Hour))
	session["goal"] = "build the settings page"
	session["branch"] = "feat/settings"
	session["outcome_note"] = "shipped behind the flag"
	return map[string]any{
		"sequence": 3, "board_revision": 1, "team": map[string]any{"key": "x"},
		"session": session,
		"events": []any{
			map[string]any{"sequence": 70, "kind": "session_started", "structural": true, "subject_key": "S-7", "summary": "S-7 started", "occurred_at": at(420)},
			map[string]any{"sequence": 71, "kind": "intent_declared", "structural": false, "subject_key": "INT-70", "summary": "INT-70 declared", "occurred_at": at(421)},
			map[string]any{"sequence": 79, "kind": "session_ended", "structural": true, "subject_key": "S-7", "summary": "S-7 ended", "occurred_at": at(510)},
		},
		"history": map[string]any{
			"intents": []any{map[string]any{"key": "INT-70", "summary": "wire the settings form", "kind": "feature",
				"status": "done", "revision": 1, "declared_at": at(421), "ended_at": at(500)}},
			"claims": []any{map[string]any{"key": "CL-70", "mode": "write", "status": "released",
				"paths": []any{"web/settings.tsx", "web/settings/**"}, "claimed_at": at(422), "expires_at": at(480), "released_at": at(505)}},
			"conflicts": []any{map[string]any{"key": "CF-70", "kind": "path_overlap", "severity": "medium",
				"status": "resolved", "resolution": "yielded", "occurrence_count": 1,
				"resolution_note":   "Bob released web/settings.tsx and Ana took it from there",
				"first_detected_at": at(430), "resolved_at": at(505),
				"participants": []any{map[string]any{"session_key": "S-7", "member_name": "Bob", "role": "holder", "subject_kind": "claim"}}}},
		},
	}
}

func olderLink(body string) string {
	m := olderRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

var runKeyRE = regexp.MustCompile(`href="/t/[a-z-]+/runs/(S-\d+)"`)

func collectRuns(seen map[string]int, body string) {
	for _, m := range runKeyRE.FindAllStringSubmatch(body, -1) {
		seen[m[1]]++
	}
}

// TestRunsListLiveFirstThenHistoryWithoutDuplicates: the live section is the
// hub's, the history is the backend's first page, and a run in both is drawn
// once.
func TestRunsListLiveFirstThenHistoryWithoutDuplicates(t *testing.T) {
	x := runsWorld(t)
	rec := x.get("/t/" + pubSlug + "/runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()

	liveAt, histAt := strings.Index(body, `id="runs-live"`), strings.Index(body, `id="runs-history"`)
	if liveAt < 0 || histAt < liveAt {
		t.Fatalf("want the live section before the history (live %d, history %d)", liveAt, histAt)
	}
	for _, k := range []string{"S-120", "S-30"} {
		if n := strings.Count(body, runHref(pubSlug, k)); n != 1 {
			t.Fatalf("%s drawn %d times, want once", k, n)
		}
		if i := strings.Index(body, runHref(pubSlug, k)); i > histAt {
			t.Fatalf("live %s is drawn in the history, not the live section", k)
		}
	}
	for i := 119; i >= 1; i-- {
		want := 0
		if i >= 71 {
			want = 1 // the rest of the first page of 50
		}
		if n := strings.Count(body, runHref(pubSlug, fmt.Sprintf("S-%d", i))); n != want && i != 30 {
			t.Fatalf("S-%d drawn %d times, want %d", i, n, want)
		}
	}
	older := olderLink(body)
	if older != "/t/"+pubSlug+"/runs?cursor=cur-50&live=S-120%2CS-30" {
		t.Fatalf("load-older = %q", older)
	}
	for _, want := range []string{"Live now", "History", "Load older runs", "2 intents · 3 paths · 1 conflict",
		"succeeded", "Bob · claude-2", "goal of S-119", "ended ", "ran 1h30m"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
	if n := x.backend.runHits(pubSlug); n != 1 {
		t.Fatalf("history reads = %d, want 1", n)
	}
}

// TestRunsLoadOlderAppendsTheNextPage follows "Load older" as htmx does, to
// the end: each answer is only the next rows and the next control, and every
// run is drawn exactly once across the page and its fragments.
func TestRunsLoadOlderAppendsTheNextPage(t *testing.T) {
	x := runsWorld(t)
	first := x.get("/t/" + pubSlug + "/runs").Body.String()
	seen := map[string]int{}
	collectRuns(seen, first)

	next, pages := olderLink(first), 1
	for next != "" {
		rec := x.do(http.MethodGet, next, "", withHeader("HX-Request", "true"))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", next, rec.Code)
		}
		frag := rec.Body.String()
		if strings.Contains(frag, "<html") || strings.Contains(frag, "<body") || strings.Contains(frag, `id="runs-live"`) {
			t.Fatalf("an older page's htmx answer carried more than rows:\n%s", frag)
		}
		if !strings.HasPrefix(strings.TrimSpace(frag), "<a") {
			t.Fatalf("fragment does not start with a row: %.80q", frag)
		}
		collectRuns(seen, frag)
		pages++
		t.Logf("page %d (%s): %d rows, next %q", pages, next, len(runKeyRE.FindAllString(frag, -1)), olderLink(frag))
		next = olderLink(frag)
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
	for i := 1; i <= 120; i++ {
		if n := seen[fmt.Sprintf("S-%d", i)]; n != 1 {
			t.Fatalf("S-%d drawn %d times across the pages, want 1", i, n)
		}
	}

	// Without JavaScript the control is a link to a whole page.
	rec := x.get("/t/" + pubSlug + "/runs?cursor=cur-50&live=S-120%2CS-30")
	page := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(page, "<html") || !strings.Contains(page, "Back to the newest runs") ||
		!strings.Contains(page, runHref(pubSlug, "S-70")) || strings.Contains(page, runHref(pubSlug, "S-71")) {
		t.Fatalf("no-JS older page: %d", rec.Code)
	}
	if rec := x.do(http.MethodGet, "/t/"+pubSlug+"/runs?cursor=bad%20cursor", "", withHeader("HX-Request", "true")); rec.Code != http.StatusBadRequest {
		t.Fatalf("a junk cursor: %d, want 400", rec.Code)
	}
}

// TestHistoricalRunDetailRendersFromTheBackend: a run the hub does not hold is
// told from the backend, settled conflict and all; one the hub holds is not
// fetched; an unknown one is today's 404.
func TestHistoricalRunDetailRendersFromTheBackend(t *testing.T) {
	x := runsWorld(t)
	rec := x.get("/t/" + pubSlug + "/runs/S-7")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"S-7 — Bob · claude-2", "build the settings page", "feat/settings", "succeeded", "how it ended", "shipped behind the flag",
		"INT-70", "wire the settings form", "declared <b>2026-09-01 07:01 UTC</b>",
		"CL-70", "web/settings.tsx", "web/settings/**", "released <b>2026-09-01 08:25 UTC</b>",
		"CF-70", "how it was settled", "Bob released web/settings.tsx and Ana took it from there",
		"S-7 started", "INT-70 declared", "S-7 ended", "session_ended", "3 events",
	} {
		if !strings.Contains(body, html.EscapeString(want)) && !strings.Contains(body, want) {
			t.Errorf("the run page does not show %q", want)
		}
	}
	hits := x.backend.runHits(pubSlug)

	if rec := x.get("/t/" + pubSlug + "/runs/S-120"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Event log") {
		t.Fatalf("hub-held run: %d", rec.Code)
	}
	if got := x.backend.runHits(pubSlug); got != hits {
		t.Fatalf("a run the hub holds was fetched from the backend (%d -> %d)", hits, got)
	}
	if rec := x.get("/t/" + pubSlug + "/runs/S-404"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run: %d, want 404", rec.Code)
	}
	if rec := x.get("/t/" + pubSlug + "/runs/" + strings.Repeat("S", 40)); rec.Code != http.StatusNotFound {
		t.Fatalf("implausible key: %d, want 404", rec.Code)
	}
	if got := x.backend.runHits(pubSlug); got != hits+1 {
		t.Fatalf("history reads = %d, want %d (the unknown key only)", got, hits+1)
	}
}

// TestDemoRunsNeverAskTheBackend: a demo board keeps today's Runs pages and
// makes no history request — even one whose feed could read one, and even
// for a signed-in viewer.
func TestDemoRunsNeverAskTheBackend(t *testing.T) {
	x := runsWorld(t)
	x.backend.setPublic("demo-live", "Recorded Crew")
	rec := &feed.Live{BaseURL: x.stub.URL, Slug: "demo-live", Client: x.stub.Client(), Logger: quiet()}
	if _, err := x.srv.AddDemoTeam(x.ctx, "demo-live", "Recorded Crew", "", rec); err != nil {
		t.Fatal(err)
	}
	x.backend.setRuns("demo-live", []map[string]any{stubRunRow("S-7", "ended", historyBase)})
	x.backend.setRunDetail("demo-live", "S-7", stubRunDetail())

	for _, who := range []string{"", memberSecret} {
		for _, path := range []string{"/t/demo-live/runs", "/t/demo-live/runs?cursor=cur-50", "/t/demo-live/runs/S-7",
			"/t/demo/runs", "/t/demo/runs/S-7"} {
			r := x.do(http.MethodGet, path, "", withHeader("HX-Request", "true"), func(r *http.Request) {
				if who != "" {
					withCookie(who)(r)
				}
			})
			if r.Code != http.StatusOK && r.Code != http.StatusNotFound {
				t.Fatalf("%s (signed in %v): %d", path, who != "", r.Code)
			}
			if strings.Contains(r.Body.String(), `id="runs-history"`) || strings.Contains(r.Body.String(), "build the settings page") {
				t.Fatalf("%s drew history on a demo board", path)
			}
			t.Logf("%-34s signed-in=%-5v -> %d", path, who != "", r.Code)
		}
	}
	if n := x.backend.runHits("demo-live") + x.backend.runHits("demo"); n != 0 {
		t.Fatalf("a demo board made %d history requests", n)
	}
}

// TestPrivateRunsAreNotFoundForAnybodyButAMember: anonymous and a signed-in
// non-member get the unknown team's 404 on every Runs URL, and no history is
// read for them; a member reads it with their own session.
func TestPrivateRunsAreNotFoundForAnybodyButAMember(t *testing.T) {
	x := runsWorld(t)
	paths := []string{"/t/" + privSlug + "/runs", "/t/" + privSlug + "/runs?cursor=cur-50&live=S-120", "/t/" + privSlug + "/runs/S-7"}
	notFound := x.get("/t/" + missing + "/runs").Body.String()

	for _, who := range []string{"", outsiderSecret} {
		for _, p := range paths {
			r := x.do(http.MethodGet, p, "", withHeader("HX-Request", "true"), func(r *http.Request) {
				if who != "" {
					withCookie(who)(r)
				}
			})
			body := r.Body.String()
			if r.Code != http.StatusNotFound {
				t.Fatalf("%s (outsider %v): %d, want 404", p, who != "", r.Code)
			}
			if strings.Contains(body, "S-119") || strings.Contains(body, "build the settings page") || strings.Contains(body, privName) {
				t.Fatalf("%s leaked the private history in its 404", p)
			}
			if who == "" && strings.ReplaceAll(body, privSlug, missing) != notFound {
				t.Fatalf("%s: anonymous 404 differs from an unknown team's", p)
			}
			t.Logf("%-44s outsider=%-5v -> %d", p, who != "", r.Code)
		}
	}
	if n := x.backend.runHits(privSlug); n != 0 {
		t.Fatalf("%d history requests were made for viewers who may not read the team", n)
	}

	list := x.getAs(memberSecret, paths[0])
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), runHref(privSlug, "S-119")) {
		t.Fatalf("member's Runs page: %d", list.Code)
	}
	if cc := list.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("member's private Runs page Cache-Control = %q", cc)
	}
	if older := olderLink(list.Body.String()); !strings.Contains(older, "cursor=cur-50") || strings.Contains(older, memberSecret) {
		t.Fatalf("load-older = %q", older)
	}
	frag := x.do(http.MethodGet, paths[1], "", withHeader("HX-Request", "true"), withCookie(memberSecret))
	if frag.Code != http.StatusOK || !strings.Contains(frag.Body.String(), runHref(privSlug, "S-70")) {
		t.Fatalf("member's older page: %d", frag.Code)
	}
	detail := x.getAs(memberSecret, paths[2])
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "shipped behind the flag") {
		t.Fatalf("member's run page: %d", detail.Code)
	}
	if n := x.backend.runHits(privSlug); n != 3 {
		t.Fatalf("member history requests = %d, want 3", n)
	}
	x.backend.mu.Lock()
	sawBearer := x.backend.login.sawBearer
	x.backend.mu.Unlock()
	if sawBearer {
		t.Fatal("the backend received a bearer")
	}
}
