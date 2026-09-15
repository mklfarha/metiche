package web

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// stubBoardHistory is the rest of the backend's board history as app/webapi
// serves it: GET /v1/teams/{slug}/conflicts/history (newest first, an opaque
// cursor, 50 by default and 100 at most, status and kind filters),
// GET /v1/teams/{slug}/events (newest first, before=<sequence>, 100 by default
// and 200 at most, kind and session filters) and GET /v1/teams/{slug}/graph
// (window=24h|7d). Reached only after the stub's team gate, as the backend's
// guard runs first. It lives on stubLogin and shares the backend's lock.
type stubBoardHistory struct {
	conflicts map[string][]map[string]any          // slug -> newest first
	events    map[string][]map[string]any          // slug -> newest first
	graphs    map[string]map[string]map[string]any // slug -> window -> answer
	hits      map[string]map[string]int            // slug -> route -> requests
	queries   map[string]map[string][]string       // slug -> route -> raw queries, in order

	// pastDecisions is GET /v1/teams/{slug}/decisions/history (newest first,
	// cursor hd-N, 50 by default and 100 at most, status superseded|revoked,
	// key= one decision in any status with its revisions); decisionsGone
	// answers that route 404, as a backend that predates it does.
	pastDecisions map[string][]map[string]any
	revisions     map[string]map[string][]map[string]any // slug -> key -> newest first
	decisionsGone bool
}

var (
	stubConflictStatuses = []string{"resolved", "dismissed", "expired"}
	// stubConflictKinds is app/webapi's liveConflictKinds, which gains
	// decision_contradiction (docs/DECISIONS.md §5.1).
	stubConflictKinds    = []string{"path_overlap", "contract_mismatch", "stale_base", "decision_contradiction"}
	stubEventKinds       = []string{"session_started", "intent_declared", "claim_released", "conflict_raised"}
	stubDecisionStatuses = []string{"superseded", "revoked"}
)

func (b *stubBackend) setDecisions(accepted []any, past []map[string]any, slugs ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.decisions = accepted
	h := &b.login.hist
	if h.pastDecisions == nil {
		h.pastDecisions = map[string][]map[string]any{}
	}
	for _, slug := range slugs {
		h.pastDecisions[slug] = past
	}
}

func (b *stubBackend) setDecisionRevisions(slug, key string, revs []map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := &b.login.hist
	if h.revisions == nil {
		h.revisions = map[string]map[string][]map[string]any{}
	}
	if h.revisions[slug] == nil {
		h.revisions[slug] = map[string][]map[string]any{}
	}
	h.revisions[slug][key] = revs
}

func (b *stubBackend) setDecisionHistoryGone(gone bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.hist.decisionsGone = gone
}

func (b *stubBackend) setConflictHistory(slug string, rows []map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := &b.login.hist
	if h.conflicts == nil {
		h.conflicts = map[string][]map[string]any{}
	}
	h.conflicts[slug] = rows
}

func (b *stubBackend) setEventHistory(slug string, rows []map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := &b.login.hist
	if h.events == nil {
		h.events = map[string][]map[string]any{}
	}
	h.events[slug] = rows
}

func (b *stubBackend) setGraphWindow(slug, window string, answer map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := &b.login.hist
	if h.graphs == nil {
		h.graphs = map[string]map[string]map[string]any{}
	}
	if h.graphs[slug] == nil {
		h.graphs[slug] = map[string]map[string]any{}
	}
	h.graphs[slug][window] = answer
}

// historyHits is the requests one history route received for slug.
func (b *stubBackend) historyHits(slug, route string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.login.hist.hits[slug][route]
}

// allHistoryHits is every history request of any kind for slug, run history
// included.
func (b *stubBackend) allHistoryHits(slug string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.login.runs.hits[slug]
	for _, v := range b.login.hist.hits[slug] {
		n += v
	}
	return n
}

// lastHistoryQuery is the raw query of the latest request to a route.
func (b *stubBackend) lastHistoryQuery(slug, route string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	qs := b.login.hist.queries[slug][route]
	if len(qs) == 0 {
		return ""
	}
	return qs[len(qs)-1]
}

func (b *stubBackend) serveBoardHistory(w http.ResponseWriter, r *http.Request, slug, sub string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := &b.login.hist
	if h.hits == nil {
		h.hits = map[string]map[string]int{}
		h.queries = map[string]map[string][]string{}
	}
	if h.hits[slug] == nil {
		h.hits[slug] = map[string]int{}
		h.queries[slug] = map[string][]string{}
	}
	h.hits[slug][sub]++
	h.queries[slug][sub] = append(h.queries[slug][sub], r.URL.RawQuery)
	q := r.URL.Query()
	bad := func(detail string) { problemJSON(w, http.StatusBadRequest, "bad request", detail) }
	limit := func(def, max int) int {
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			return min(v, max)
		}
		return def
	}

	switch sub {
	case "decisions/history":
		if h.decisionsGone {
			http.NotFound(w, r)
			return
		}
		out := map[string]any{"sequence": 3, "board_revision": 1, "status": q.Get("status"), "statuses": stubDecisionStatuses}
		if key := q.Get("key"); key != "" {
			var found []any
			for _, d := range b.decisions {
				if d.(map[string]any)["key"] == key {
					found = append(found, d)
				}
			}
			for _, d := range h.pastDecisions[slug] {
				if d["key"] == key {
					found = append(found, d)
				}
			}
			if len(found) == 0 {
				problemJSON(w, http.StatusNotFound, "not found", "no such decision")
				return
			}
			out["status"], out["decisions"] = "", found
			if revs := h.revisions[slug][key]; len(revs) > 0 {
				out["revisions"] = revs
			}
			writeJSON(w, out)
			return
		}
		status := q.Get("status")
		if status != "" && !slices.Contains(stubDecisionStatuses, status) {
			bad("status must be superseded or revoked")
			return
		}
		rows := []map[string]any{}
		for _, d := range h.pastDecisions[slug] {
			if status == "" || d["status"] == status {
				rows = append(rows, d)
			}
		}
		off := 0
		if c := q.Get("cursor"); c != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(c, "hd-"))
			if !strings.HasPrefix(c, "hd-") || err != nil || n < 0 || n > len(rows) {
				bad("cursor is not one this endpoint issued")
				return
			}
			off = n
		}
		end := min(off+limit(50, 100), len(rows))
		out["decisions"] = rows[off:end]
		if end < len(rows) {
			out["next_cursor"] = fmt.Sprintf("hd-%d", end)
		}
		writeJSON(w, out)

	case "conflicts/history":
		status, kind := q.Get("status"), q.Get("kind")
		if status != "" && !slices.Contains(stubConflictStatuses, status) {
			bad("status must be resolved, dismissed or expired")
			return
		}
		if kind != "" && !slices.Contains(stubConflictKinds, kind) {
			bad("kind is not a conflict kind")
			return
		}
		var rows []map[string]any
		for _, c := range h.conflicts[slug] {
			if (status == "" || c["status"] == status) && (kind == "" || c["kind"] == kind) {
				rows = append(rows, c)
			}
		}
		off := 0
		if c := q.Get("cursor"); c != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(c, "hc-"))
			if !strings.HasPrefix(c, "hc-") || err != nil || n < 0 || n > len(rows) {
				bad("cursor is not one this endpoint issued")
				return
			}
			off = n
		}
		end := min(off+limit(50, 100), len(rows))
		out := map[string]any{"sequence": 3, "board_revision": 1, "status": status, "kind": kind,
			"conflicts": rows[off:end], "statuses": stubConflictStatuses, "kinds": stubConflictKinds}
		if out["conflicts"] == nil {
			out["conflicts"] = []any{}
		}
		if end < len(rows) {
			out["next_cursor"] = fmt.Sprintf("hc-%d", end)
		}
		writeJSON(w, out)

	case "events":
		var before int64
		if raw := q.Get("before"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 {
				bad("before is not a sequence this endpoint issued")
				return
			}
			before = n
		}
		kind, session := q.Get("kind"), q.Get("session")
		if kind != "" && !slices.Contains(stubEventKinds, kind) {
			bad("kind is not an event kind")
			return
		}
		max := limit(100, 200)
		page := []map[string]any{}
		more := false
		for _, e := range h.events[slug] {
			if before > 0 && e["sequence"].(int64) >= before {
				continue
			}
			if (kind != "" && e["kind"] != kind) || (session != "" && e["session_key"] != session) {
				continue
			}
			if len(page) == max {
				more = true
				break
			}
			page = append(page, e)
		}
		out := map[string]any{"sequence": 3, "board_revision": 1, "kind": kind, "session_key": session,
			"events": page, "kinds": stubEventKinds}
		if more {
			out["next_before"] = page[len(page)-1]["sequence"]
		}
		writeJSON(w, out)

	case "graph":
		window := q.Get("window")
		if window != "24h" && window != "7d" {
			bad("window must be 24h or 7d")
			return
		}
		if answer, ok := h.graphs[slug][window]; ok {
			writeJSON(w, answer)
			return
		}
		writeJSON(w, map[string]any{"window": window, "from": "2026-09-01T00:00:00Z", "to": "2026-09-08T00:00:00Z",
			"sessions": []any{}, "holds": []any{}, "conflicts": []any{}})
	}
}
