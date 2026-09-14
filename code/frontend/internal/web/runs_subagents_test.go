package web

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The supervisor -> subagent link on the Runs pages, against the stub
// backend: the list says it compactly, and a run page links both ways, for a
// run the hub holds (live) and for one told from the backend's history.

func subagentsWorld(t *testing.T) *harness {
	t.Helper()
	x := newLoginHarness(t, Discovery{}, Login{})
	x.world(t)
	live := func(key, parent string, started time.Time) map[string]any {
		return map[string]any{"key": key, "member_key": "M-1", "member_name": "Ana", "agent_label": "claude-1",
			"status": "live", "goal": "goal of " + key, "started_at": started.Format(time.RFC3339),
			"parent_session_key": parent, "intents": []any{}, "claims": []any{}}
	}
	x.backend.mu.Lock()
	x.backend.sessions = []any{
		live("S-41", "", historyBase.Add(41*time.Hour)),
		live("S-42", "S-41", historyBase.Add(42*time.Hour)),
		live("S-43", "S-41", historyBase.Add(43*time.Hour)),
	}
	x.backend.mu.Unlock()

	row := func(key, status, parent string, subagents int, hour int) map[string]any {
		r := stubRunRow(key, status, historyBase.Add(time.Duration(hour)*time.Hour))
		if parent != "" {
			r["parent_session_key"] = parent
		}
		r["counts"].(map[string]any)["subagents"] = subagents
		return r
	}
	rows := []map[string]any{
		row("S-43", "live", "S-41", 0, 43), row("S-42", "live", "S-41", 0, 42), row("S-41", "live", "", 2, 41),
		row("S-12", "ended", "S-10", 0, 12), row("S-11", "ended", "S-10", 0, 11), row("S-10", "ended", "", 2, 10),
	}
	detail := func(session map[string]any, subagents ...map[string]any) map[string]any {
		subs := []any{}
		for _, s := range subagents {
			subs = append(subs, s)
		}
		return map[string]any{"sequence": 3, "board_revision": 1, "team": map[string]any{"key": "x"},
			"session": session, "subagents": subs, "events": []any{},
			"history": map[string]any{"intents": []any{}, "claims": []any{}, "conflicts": []any{}}}
	}
	x.backend.setRuns(pubSlug, rows)
	x.backend.setRunDetail(pubSlug, "S-10", detail(rows[5], rows[4], rows[3]))
	x.backend.setRunDetail(pubSlug, "S-11", detail(rows[4]))
	return x
}

func mustGet(t *testing.T, x *harness, path string) string {
	t.Helper()
	rec := x.get(path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	return rec.Body.String()
}

func TestRunsListShowsTheDelegationCompactly(t *testing.T) {
	x := subagentsWorld(t)
	body := mustGet(t, x, "/t/"+pubSlug+"/runs")
	badges := strings.Count(body, `subrel-badge`)
	for _, want := range []string{">subagent of S-41<", ">2 subagents<", ">subagent of S-10<"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Runs list does not say %q", want)
		}
	}
	// S-41 (2 subagents), S-42 and S-43 live; S-10 (2), S-11 and S-12 in history.
	if badges != 6 {
		t.Fatalf("relation badges = %d, want 6", badges)
	}
	t.Logf("Runs list: %d relation badges", badges)
}

func TestHistoricalRunLinksToItsSubagentsAndBack(t *testing.T) {
	x := subagentsWorld(t)
	sup := mustGet(t, x, "/t/"+pubSlug+"/runs/S-10")
	for _, want := range []string{`<h2 class="run-h">Subagents</h2>`, "2 runs this run delegated",
		runHref(pubSlug, "S-11"), runHref(pubSlug, "S-12"), ">subagent of S-10<"} {
		if !strings.Contains(sup, want) {
			t.Errorf("S-10's run page does not show %q", want)
		}
	}
	sub := mustGet(t, x, "/t/"+pubSlug+"/runs/S-11")
	if !strings.Contains(sub, `Subagent of <a class="mono" `+runHref(pubSlug, "S-10")+`>S-10</a>`) {
		t.Fatalf("S-11's run page does not link to its supervisor S-10:\n%s", sub)
	}
	if strings.Contains(sub, `<h2 class="run-h">Subagents</h2>`) {
		t.Fatal("S-11 lists subagents it does not have")
	}
}

func TestLiveRunLinksToItsSubagentsAndBack(t *testing.T) {
	x := subagentsWorld(t)
	hits := x.backend.runHits(pubSlug)
	sup := mustGet(t, x, "/t/"+pubSlug+"/runs/S-41")
	for _, want := range []string{">Subagents</h2>", runHref(pubSlug, "S-42"), runHref(pubSlug, "S-43"), "subagent of S-41 ·"} {
		if !strings.Contains(sup, want) {
			t.Errorf("S-41's run page does not show %q", want)
		}
	}
	sub := mustGet(t, x, "/t/"+pubSlug+"/runs/S-42")
	if !strings.Contains(sub, `Subagent of <a class="mono" `+runHref(pubSlug, "S-41")+`>S-41</a>`) {
		t.Fatalf("S-42's run page does not link to its supervisor S-41:\n%s", sub)
	}
	if got := x.backend.runHits(pubSlug); got != hits {
		t.Fatalf("hub-held runs were fetched from the backend (%d -> %d)", hits, got)
	}
}
