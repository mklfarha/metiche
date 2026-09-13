package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// stubRuns is the backend's run history as app/webapi/history.go serves it:
// GET /v1/teams/{slug}/sessions (newest first, an opaque cursor, limit 50 by
// default and 100 at most) and GET /v1/teams/{slug}/sessions/{key}. It is
// reached only after the stub's team gate (public, or a member's session on a
// private team), exactly as the backend's guard runs first. It lives on
// stubLogin and shares the backend's lock.
type stubRuns struct {
	bySlug map[string][]map[string]any          // newest first
	detail map[string]map[string]map[string]any // slug -> key -> answer
	hits   map[string]int                       // every history request, per slug
}

func (b *stubBackend) setRuns(slug string, rows []map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs := &b.login.runs
	if rs.bySlug == nil {
		rs.bySlug = map[string][]map[string]any{}
	}
	rs.bySlug[slug] = rows
}

func (b *stubBackend) setRunDetail(slug, key string, answer map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs := &b.login.runs
	if rs.detail == nil {
		rs.detail = map[string]map[string]map[string]any{}
	}
	if rs.detail[slug] == nil {
		rs.detail[slug] = map[string]map[string]any{}
	}
	rs.detail[slug][key] = answer
}

func (b *stubBackend) runHits(slug string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.login.runs.hits[slug]
}

func (b *stubBackend) serveRuns(w http.ResponseWriter, r *http.Request, slug, sub string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs := &b.login.runs
	if rs.hits == nil {
		rs.hits = map[string]int{}
	}
	rs.hits[slug]++

	if sub == "sessions" {
		rows := rs.bySlug[slug]
		limit := 50
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
			limit = min(v, 100)
		}
		off := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(c, "cur-"))
			if !strings.HasPrefix(c, "cur-") || err != nil || n < 0 || n > len(rows) {
				problemJSON(w, http.StatusBadRequest, "bad request", "cursor is not one this endpoint issued")
				return
			}
			off = n
		}
		end := min(off+limit, len(rows))
		out := map[string]any{"sequence": 3, "board_revision": 1,
			"team": map[string]any{"key": slug, "sequence": 3, "board_revision": 1}, "sessions": rows[off:end]}
		if end < len(rows) {
			out["next_cursor"] = fmt.Sprintf("cur-%d", end)
		}
		writeJSON(w, out)
		return
	}
	answer, ok := rs.detail[slug][strings.TrimPrefix(sub, "sessions/")]
	if !ok {
		problemJSON(w, http.StatusNotFound, "not found", "no such session on this team")
		return
	}
	writeJSON(w, answer)
}
