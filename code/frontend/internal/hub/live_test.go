package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/view"
)

// End-to-end over the live path: a fake backend, the real feed.Live, the real
// store, the real hub. What it is here to prove is the seam between the
// snapshot and the stream — that the board starts from state the backend gave
// it, resumes at exactly the cursor that state was read at, and ends up
// having applied every event once.

type liveBackend struct {
	mu sync.Mutex

	seq        int64 // what the snapshot reports
	statusLine string
	events     []map[string]any

	snapshotHits int
	afterAsked   []int64
}

func (b *liveBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/teams/demo", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.snapshotHits++
		seq, line := b.seq, b.statusLine
		b.mu.Unlock()
		writeJSON(w, map[string]any{
			"sequence": seq, "board_revision": 4,
			"team": map[string]any{"key": "demo", "name": "Orbital Freight", "sequence": seq, "board_revision": 4},
			"sessions": []any{map[string]any{
				"key": "S-17", "project_key": "web", "member_key": "M-1", "member_name": "Mara",
				"agent_label": "api", "status": "live", "status_line": line,
				"branch": "feat/booking-api", "started_at": "2026-09-12T10:00:00Z",
				"intents": []any{}, "claims": []any{},
			}},
			"conflicts": []any{},
			"counts":    map[string]any{"live_sessions": 1},
		})
	})
	mux.HandleFunc("/v1/teams/demo/contracts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sequence": 0, "board_revision": 0, "contracts": []any{}})
	})
	mux.HandleFunc("/v1/teams/demo/decisions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sequence": 0, "board_revision": 0, "decisions": []any{}})
	})
	mux.HandleFunc("/v1/teams/demo/stream", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		b.mu.Lock()
		b.afterAsked = append(b.afterAsked, after)
		events := append([]map[string]any(nil), b.events...)
		b.mu.Unlock()

		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Replay, then stay open and deliver whatever the test appends next,
		// which is how the real stream behaves: a live tail, not a page.
		sent := after
		for {
			for _, ev := range events {
				seq, _ := ev["sequence"].(int64)
				if seq <= sent {
					continue
				}
				body, _ := json.Marshal(ev)
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, ev["kind"], body)
				flusher.Flush()
				sent = seq
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
			b.mu.Lock()
			events = append([]map[string]any(nil), b.events...)
			b.mu.Unlock()
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func frame(seq int64, kind string, structural bool) map[string]any {
	return map[string]any{
		"sequence": seq, "kind": kind, "structural": structural,
		"board_revision": 4, "occurred_at": "2026-09-12T10:05:00Z",
		"session_key": "S-17", "member_key": "M-1", "agent_label": "api",
		"summary": kind + " " + strconv.FormatInt(seq, 10),
		"payload": map[string]any{},
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestLiveBoardStartsFromTheSnapshotAndAppliesEveryEventOnce is the whole live
// path in one test.
func TestLiveBoardStartsFromTheSnapshotAndAppliesEveryEventOnce(t *testing.T) {
	b := &liveBackend{seq: 8, statusLine: "writing the bookings handler"}
	for _, seq := range []int64{9, 10, 11, 12} {
		b.events = append(b.events, frame(seq, "intent_updated", seq == 11))
	}
	srv := httptest.NewServer(b.handler())
	defer srv.Close()

	live := &feed.Live{BaseURL: srv.URL, Slug: "demo", Token: "t", Backoff: time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := New("demo", "", view.Renderer{}, quietLogger())
	if err := h.Run(ctx, live); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The snapshot is read BEFORE the stream is opened, and the cursor is the
	// sequence it reported.
	snap := h.Snapshot()
	if snap.Team.Sequence != 8 {
		t.Fatalf("cursor after Run = %d, want 8", snap.Team.Sequence)
	}
	if snap.Team.Name != "Orbital Freight" {
		t.Fatalf("team name = %q — the snapshot is what names the team", snap.Team.Name)
	}
	// State came from the snapshot, not from folding an event log that has
	// not been read yet.
	if len(snap.Sessions) != 1 || snap.Sessions[0].Key != "S-17" {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	if len(snap.Members) != 1 || len(snap.Members[0].Agents) != 1 {
		t.Fatalf("lanes = %+v", snap.Members)
	}
	if len(snap.Lanes()) != 1 || snap.Lanes()[0].Agents[0].Session == nil {
		t.Fatal("the board must have a lane with a session on it straight off the snapshot")
	}

	waitFor(t, "all four streamed events", func() bool { return h.Store().Sequence() == 12 })

	snap = h.Snapshot()
	if got := len(snap.Events); got != 4 {
		t.Fatalf("timeline holds %d events, want 4", got)
	}
	for i, ev := range snap.Events {
		if want := int64(9 + i); ev.Sequence != want {
			t.Fatalf("timeline[%d] = %d, want %d", i, ev.Sequence, want)
		}
	}

	// The two cursors keep their separate meanings: four events moved the
	// sequence, one of them was structural and moved the board revision.
	if snap.Team.BoardRevision != 5 {
		t.Fatalf("board revision = %d, want 5 (4 from the snapshot, +1 structural)", snap.Team.BoardRevision)
	}

	// Exactly one stream connection, opened at the snapshot's cursor.
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.afterAsked) != 1 || b.afterAsked[0] != 8 {
		t.Fatalf("stream connections asked for %v, want [8]", b.afterAsked)
	}
}

// TestStructuralEventRefreshesStateAndOthersDoNot is the second cursor earning
// its keep: only a structural frame is worth a round trip to the backend.
func TestStructuralEventRefreshesStateAndOthersDoNot(t *testing.T) {
	b := &liveBackend{seq: 8, statusLine: "writing the bookings handler"}
	b.events = []map[string]any{frame(9, "heartbeat", false)}
	srv := httptest.NewServer(b.handler())
	defer srv.Close()

	live := &feed.Live{BaseURL: srv.URL, Slug: "demo", Token: "t", Backoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := New("demo", "", view.Renderer{}, quietLogger())
	if err := h.Run(ctx, live); err != nil {
		t.Fatalf("run: %v", err)
	}
	waitFor(t, "the non-structural event", func() bool { return h.Store().Sequence() == 9 })

	b.mu.Lock()
	hitsAfterNonStructural := b.snapshotHits
	b.mu.Unlock()
	if hitsAfterNonStructural != 1 {
		t.Fatalf("the backend was read %d times; a non-structural event must not trigger a refresh", hitsAfterNonStructural)
	}

	// Now change the world and send a structural frame. The frame itself
	// carries no status line — the refresh is what brings it in.
	b.mu.Lock()
	b.statusLine = "running the migration"
	b.seq = 10
	b.events = append(b.events, frame(10, "session_started", true))
	b.mu.Unlock()

	waitFor(t, "the structural refresh", func() bool {
		s := h.Snapshot()
		return len(s.Sessions) == 1 && s.Sessions[0].StatusLine == "running the migration"
	})

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.snapshotHits < 2 {
		t.Fatalf("the backend was read %d times; a structural event must trigger a refresh", b.snapshotHits)
	}
	// The refresh must not have moved the timeline cursor on its own: the
	// stream owns that number.
	if got := h.Store().Sequence(); got != 10 {
		t.Fatalf("cursor = %d, want 10 — a refresh must not touch it", got)
	}
}

// TestFixtureModeStillFolds is the regression guard on the deployed path.
// A fixture feed cannot snapshot, so the store must keep folding events into
// entity state exactly as it did before any of this existed.
func TestFixtureModeStillFolds(t *testing.T) {
	if _, err := os.Stat("../../fixtures/demo.jsonl"); err != nil {
		t.Skipf("no fixture recording to replay: %v", err)
	}
	f := &feed.Fixture{
		FS:     os.DirFS("../../fixtures"),
		Path:   "demo.jsonl",
		Speed:  10000,
		Warmup: 10000, // deliver the whole recording immediately
	}
	if _, ok := any(f).(feed.Snapshotter); ok {
		t.Fatal("a fixture must NOT satisfy Snapshotter, or it would try to read a backend that is not there")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := New("demo", "Orbital Freight", view.Renderer{}, quietLogger())
	if err := h.Run(ctx, f); err != nil {
		t.Fatalf("run: %v", err)
	}
	waitFor(t, "the recording to be folded", func() bool {
		s := h.Snapshot()
		return len(s.Sessions) > 0 && len(s.Members) > 0
	})

	s := h.Snapshot()
	if s.Team.Sequence == 0 {
		t.Fatal("the fixture must move the sequence")
	}
	if len(s.Events) == 0 {
		t.Fatal("the fixture must fill the timeline")
	}
	// Folding is the whole point of this path: sessions, members and their
	// agents exist only because the events said so.
	for _, m := range s.Members {
		if m.DisplayName == "" {
			t.Fatalf("member %q folded without a display name", m.Key)
		}
	}
	if len(s.Lanes()) == 0 {
		t.Fatal("the board must have lanes")
	}
}
