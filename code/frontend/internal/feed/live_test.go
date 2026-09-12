package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// A fake metiche backend, speaking the shapes in app/webapi and app/stream.
//
// It is faked rather than run because what is being proven here is the
// ORDERING contract between the two halves — the snapshot's cursor and the
// stream's ?after= — and that contract is a property of this client's
// arithmetic, not of MySQL. The fake honours the one guarantee the real server
// documents: everything strictly greater than after, once each, in order.
type fakeBackend struct {
	mu sync.Mutex

	// snapshotSeq is the sequence the snapshot reports. Events at or below it
	// are "already drawn"; everything above arrives on the stream.
	snapshotSeq int64
	events      []frameWire

	// dropAfter closes the stream connection after this many frames, once, to
	// force a reconnect. Zero means never.
	dropAfter int

	// afterRequested records the ?after= of every stream request, which is
	// what the reconnect test asserts on.
	afterRequested []int64
	authSeen       []string
	paths          []string
}

func (b *fakeBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/teams/demo", b.snapshot)
	mux.HandleFunc("/v1/teams/demo/contracts", b.contracts)
	mux.HandleFunc("/v1/teams/demo/decisions", b.decisions)
	mux.HandleFunc("/v1/teams/demo/stream", b.stream)
	return mux
}

func (b *fakeBackend) record(r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paths = append(b.paths, r.URL.Path)
	b.authSeen = append(b.authSeen, r.Header.Get("Authorization"))
}

func (b *fakeBackend) snapshot(w http.ResponseWriter, r *http.Request) {
	b.record(r)
	b.mu.Lock()
	seq := b.snapshotSeq
	b.mu.Unlock()

	writeJSON(w, map[string]any{
		"sequence":       seq,
		"board_revision": 12,
		"team":           map[string]any{"key": "demo", "name": "Orbital Freight", "sequence": seq, "board_revision": 12},
		"sessions": []any{
			map[string]any{
				"key": "S-17", "project_key": "web", "member_key": "M-1",
				"member_name": "Mara", "agent_label": "api", "client_kind": "claude-code",
				"branch": "feat/booking-api", "goal": "the bookings endpoint",
				"status": "live", "status_line": "writing the bookings handler",
				"started_at": "2026-09-12T10:00:00Z", "last_heartbeat_at": "2026-09-12T10:04:00Z",
				"current_intent_key": "INT-83",
				"intents": []any{
					map[string]any{"key": "INT-83", "summary": "add the bookings handler",
						"kind": "change", "status": "active", "external_ref": "#212",
						"revision": 2, "declared_at": "2026-09-12T10:01:00Z"},
				},
				"claims": []any{
					map[string]any{"key": "CL-4", "mode": "write",
						"expires_at": "2026-09-12T10:34:00Z",
						"paths":      []string{"api/router.go", "api/bookings.go"}},
				},
			},
			map[string]any{
				"key": "S-18", "project_key": "web", "member_key": "M-2",
				"member_name": "Devesh", "agent_label": "web", "client_kind": "claude-code",
				"branch": "feat/booking-ui", "status": "stale",
				"status_line": "wiring the booking form", "started_at": "2026-09-12T10:02:00Z",
				"intents": []any{}, "claims": []any{},
			},
		},
		"conflicts": []any{
			map[string]any{
				"key": "CF-14", "kind": "path_overlap", "severity": "high", "status": "open",
				"detected_by": "server", "detector_rule": "path_overlap.write_write",
				"suggested_action":  "Take api/bookings.go and let the router land first.",
				"occurrence_count":  2,
				"first_detected_at": "2026-09-12T10:03:00Z",
				"last_detected_at":  "2026-09-12T10:05:00Z",
				"participants": []any{
					map[string]any{"session_key": "S-17", "member_name": "Mara", "agent_label": "api", "role": "holder", "subject_kind": "claim"},
					map[string]any{"session_key": "S-18", "member_name": "Devesh", "agent_label": "web", "role": "challenger", "subject_kind": "claim"},
				},
			},
		},
		"counts": map[string]any{"live_sessions": 2, "open_conflicts": 1, "held_claims": 1},
	}, w)
}

func (b *fakeBackend) contracts(w http.ResponseWriter, r *http.Request) {
	b.record(r)
	writeJSON(w, map[string]any{
		"sequence": 8, "board_revision": 12,
		"team": map[string]any{"key": "demo", "name": "Orbital Freight"},
		"contracts": []any{
			map[string]any{
				"key": "POST /api/bookings", "project_key": "web", "kind": "http",
				"status": "active", "title": "create a booking", "agreement": "unclaimed",
				"produces": []any{},
				"consumes": []any{
					map[string]any{"role": "consumes", "session_key": "S-18", "member_name": "Devesh",
						"agent_label": "web", "shape_hash": "abc123", "revision": 1,
						"asserted_at": "2026-09-12T10:02:30Z", "field_count": 4},
				},
			},
			map[string]any{
				"key": "GET /api/legs", "project_key": "web", "kind": "http",
				"status": "active", "agreement": "mismatch",
				"produces": []any{
					map[string]any{"role": "produces", "session_key": "S-17", "shape_hash": "aaa", "field_count": 3,
						"asserted_at": "2026-09-12T10:01:30Z"},
				},
				"consumes": []any{
					map[string]any{"role": "consumes", "session_key": "S-18", "shape_hash": "bbb", "field_count": 3,
						"asserted_at": "2026-09-12T10:02:00Z"},
				},
			},
		},
	}, w)
}

func (b *fakeBackend) decisions(w http.ResponseWriter, r *http.Request) {
	b.record(r)
	writeJSON(w, map[string]any{
		"sequence": 8, "board_revision": 12,
		"team": map[string]any{"key": "demo", "name": "Orbital Freight"},
		"decisions": []any{
			map[string]any{
				"key": "#auth-jwt-cookie", "title": "session tokens live in an httpOnly cookie",
				"statement": "Session tokens are carried in an httpOnly cookie, never in web storage.",
				"status":    "accepted", "always_show": true, "revision": 1,
				"decided_by": "Mara", "decided_at": "2026-09-12T09:00:00Z",
				"scope": []string{"web/auth/**", "api/auth/**"},
			},
		},
	}, w)
}

// stream serves the SSE endpoint, honouring ?after exactly as the real one
// documents: strictly greater than after, once each, in order.
func (b *fakeBackend) stream(w http.ResponseWriter, r *http.Request) {
	b.record(r)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	b.mu.Lock()
	b.afterRequested = append(b.afterRequested, after)
	drop := b.dropAfter
	if drop > 0 {
		b.dropAfter = 0 // only drop the first connection
	}
	events := append([]frameWire(nil), b.events...)
	b.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sent := 0
	for _, ev := range events {
		if ev.Sequence <= after {
			continue
		}
		body, _ := json.Marshal(ev)
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Sequence, ev.Kind, body)
		flusher.Flush()
		sent++
		if drop > 0 && sent >= drop {
			return // connection closes mid-stream
		}
	}
	// Hold the connection open the way a real stream does, until the client
	// goes away.
	<-r.Context().Done()
}

func writeJSON(w http.ResponseWriter, v any, _ ...any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// events 1..n, marking every third one structural.
func seedEvents(n int) []frameWire {
	out := make([]frameWire, 0, n)
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		out = append(out, frameWire{
			Sequence:      int64(i),
			BoardRevision: int64(i / 3),
			Structural:    i%3 == 0,
			Kind:          "intent_updated",
			OccurredAt:    base.Add(time.Duration(i) * time.Second),
			SessionKey:    "S-17",
			MemberKey:     "M-1",
			AgentLabel:    "api",
			SubjectKind:   "intent",
			SubjectKey:    "INT-83",
			Summary:       fmt.Sprintf("event %d", i),
			Payload:       json.RawMessage(`{"message":"working"}`),
		})
	}
	return out
}

func newLive(t *testing.T, b *fakeBackend) (*Live, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(b.handler())
	t.Cleanup(srv.Close)
	return &Live{
		BaseURL:          srv.URL,
		Slug:             "demo",
		Token:            "test-token",
		Backoff:          time.Millisecond,
		TimelineBackfill: 0,
	}, srv
}

// collect reads n events off the channel or fails.
func collect(t *testing.T, ch <-chan model.Event, n int) []model.Event {
	t.Helper()
	out := make([]model.Event, 0, n)
	deadline := time.After(5 * time.Second)
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("feed closed after %d of %d events", len(out), n)
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(out), n)
		}
	}
	return out
}

// assertExact is the whole point of this file: the sequences delivered are
// precisely first..last, in order, once each.
func assertExact(t *testing.T, got []model.Event, first, last int64) {
	t.Helper()
	want := int64(0)
	for i, ev := range got {
		want = first + int64(i)
		if ev.Sequence != want {
			t.Fatalf("event %d: got sequence %d, want %d (full: %v)", i, ev.Sequence, want, sequences(got))
		}
	}
	if want != last {
		t.Fatalf("last delivered sequence is %d, want %d (full: %v)", want, last, sequences(got))
	}
	seen := map[int64]bool{}
	for _, ev := range got {
		if seen[ev.Sequence] {
			t.Fatalf("sequence %d delivered twice (full: %v)", ev.Sequence, sequences(got))
		}
		seen[ev.Sequence] = true
	}
}

func sequences(evs []model.Event) []int64 {
	out := make([]int64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Sequence)
	}
	return out
}

// TestSnapshotThenStreamHasNoGapAndNoDuplicate is the proof the live path
// hangs on.
//
// The snapshot is taken at sequence 8; the stream is opened with ?after=8;
// events 9 through 14 arrive, in order, once each. Nothing between the two
// reads falls down the crack, and nothing on either side of the boundary is
// applied twice.
func TestSnapshotThenStreamHasNoGapAndNoDuplicate(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 8, events: seedEvents(14)}
	live, _ := newLive(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts, err := live.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if ts.Team.Sequence != 8 {
		t.Fatalf("snapshot sequence = %d, want 8", ts.Team.Sequence)
	}
	if ts.ResumeFrom != 8 {
		t.Fatalf("resume cursor = %d, want 8 (the snapshot's own sequence)", ts.ResumeFrom)
	}
	if ts.Team.BoardRevision != 12 {
		t.Fatalf("board revision = %d, want 12", ts.Team.BoardRevision)
	}

	ch, err := live.Stream(ctx, ts.ResumeFrom)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	got := collect(t, ch, 6)
	assertExact(t, got, 9, 14)

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.afterRequested) != 1 || b.afterRequested[0] != 8 {
		t.Fatalf("stream ?after= requests were %v, want exactly [8]", b.afterRequested)
	}
}

// TestReconnectResumesFromTheExactCursor drops the connection mid-stream.
//
// The client must come back with ?after=<the last sequence it actually handed
// downstream> — not the snapshot's cursor, which would replay, and not a
// guess past it, which would lose events.
func TestReconnectResumesFromTheExactCursor(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 8, events: seedEvents(14), dropAfter: 2}
	live, _ := newLive(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts, err := live.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ch, err := live.Stream(ctx, ts.ResumeFrom)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	// 6 events across a connection drop: 9 and 10 on the first connection,
	// 11..14 after the reconnect.
	got := collect(t, ch, 6)
	assertExact(t, got, 9, 14)

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.afterRequested) != 2 {
		t.Fatalf("expected exactly two stream connections, got %v", b.afterRequested)
	}
	if b.afterRequested[0] != 8 || b.afterRequested[1] != 10 {
		t.Fatalf("stream ?after= requests were %v, want [8 10]", b.afterRequested)
	}
}

// TestBackfillRewindsTheCursorAndNothingElse proves the timeline backfill is
// a cursor decision only: it asks for older events, and it never asks for
// fewer.
func TestBackfillRewindsTheCursorAndNothingElse(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 8, events: seedEvents(10)}
	live, _ := newLive(t, b)
	live.TimelineBackfill = 5

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts, err := live.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if ts.Team.Sequence != 8 {
		t.Fatalf("state is still described at sequence %d, want 8", ts.Team.Sequence)
	}
	if ts.ResumeFrom != 3 {
		t.Fatalf("resume cursor = %d, want 3 (8 minus the 5 backfilled)", ts.ResumeFrom)
	}

	ch, err := live.Stream(ctx, ts.ResumeFrom)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	assertExact(t, collect(t, ch, 7), 4, 10)
}

// TestBackfillNeverGoesBelowZero — a young team has less history than the
// backfill asks for, and asking for ?after=-192 would be a 400.
func TestBackfillNeverGoesBelowZero(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 3, events: seedEvents(4)}
	live, _ := newLive(t, b)
	live.TimelineBackfill = 200

	ts, err := live.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if ts.ResumeFrom != 0 {
		t.Fatalf("resume cursor = %d, want 0", ts.ResumeFrom)
	}
}

// TestEveryRequestCarriesTheBearerToken — the read API is not public, and the
// board is the one client that has to prove it every time.
func TestEveryRequestCarriesTheBearerToken(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 2, events: seedEvents(3)}
	live, _ := newLive(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := live.Snapshot(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ch, err := live.Stream(ctx, 2)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	collect(t, ch, 1)

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.paths) != 4 {
		t.Fatalf("expected 4 requests (snapshot, contracts, decisions, stream), got %v", b.paths)
	}
	for i, got := range b.authSeen {
		if got != "Bearer test-token" {
			t.Fatalf("request %d (%s) carried Authorization %q", i, b.paths[i], got)
		}
	}
}

// TestSnapshotIsReadBeforeTheDerivedLists — the snapshot defines the cursor,
// so it must be the FIRST read. Contracts and decisions read after it can
// only be ahead of the cursor, which is harmless; read before it they would
// be behind it, and anything that changed in between would be in neither the
// state nor the stream.
func TestSnapshotIsReadBeforeTheDerivedLists(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 5, events: seedEvents(5)}
	live, _ := newLive(t, b)

	if _, err := live.Snapshot(context.Background()); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	want := []string{"/v1/teams/demo", "/v1/teams/demo/contracts", "/v1/teams/demo/decisions"}
	if strings.Join(b.paths, " ") != strings.Join(want, " ") {
		t.Fatalf("read order was %v, want %v", b.paths, want)
	}
}

// TestSnapshotAdaptsTheWireToTheBoardsModel walks the whole adapter over one
// realistic payload.
func TestSnapshotAdaptsTheWireToTheBoardsModel(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 8, events: seedEvents(8)}
	live, _ := newLive(t, b)

	ts, err := live.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if ts.Team.Slug != "demo" || ts.Team.Name != "Orbital Freight" {
		t.Fatalf("team = %+v", ts.Team)
	}

	// Members and their agents are derived from the sessions, because the
	// read API reports a team through its sessions.
	if len(ts.Members) != 2 {
		t.Fatalf("got %d members, want 2", len(ts.Members))
	}
	mara := ts.Members[0]
	if mara.Key != "M-1" || mara.DisplayName != "Mara" {
		t.Fatalf("first member = %+v", mara)
	}
	if len(mara.Agents) != 1 || mara.Agents[0].Label != "api" {
		t.Fatalf("Mara's agents = %+v", mara.Agents)
	}
	if mara.Agents[0].Key != "M-1/api" {
		t.Fatalf("derived agent key = %q, want M-1/api", mara.Agents[0].Key)
	}

	// The lane join: the session must point at the agent that was derived.
	if len(ts.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(ts.Sessions))
	}
	s17 := ts.Sessions[0]
	if s17.Key != "S-17" || s17.AgentKey != "M-1/api" || s17.MemberKey != "M-1" {
		t.Fatalf("session = %+v", s17)
	}
	if s17.Status != "live" || s17.StatusLine != "writing the bookings handler" {
		t.Fatalf("session status = %q / %q", s17.Status, s17.StatusLine)
	}
	if !s17.Live() {
		t.Fatal("S-17 should read as live")
	}
	if !ts.Sessions[1].Live() {
		t.Fatal("a stale session is still on the board — it is holding claims")
	}
	if got := s17.StartedAt.UTC().Format(time.RFC3339); got != "2026-09-12T10:00:00Z" {
		t.Fatalf("started_at = %s", got)
	}
	if !s17.EndedAt.IsZero() {
		t.Fatal("an absent ended_at must stay the zero time, not the dawn of the common era")
	}

	if len(ts.Intents) != 1 || ts.Intents[0].SessionKey != "S-17" || ts.Intents[0].Revision != 2 {
		t.Fatalf("intents = %+v", ts.Intents)
	}
	if !ts.Intents[0].Open() {
		t.Fatal("an active intent must read as open")
	}

	if len(ts.Claims) != 1 {
		t.Fatalf("claims = %+v", ts.Claims)
	}
	claim := ts.Claims[0]
	if claim.SessionKey != "S-17" || claim.Mode != "write" || len(claim.Paths) != 2 {
		t.Fatalf("claim = %+v", claim)
	}
	// The endpoint only returns held, unexpired claims, so they are active.
	if !claim.Active(time.Date(2026, 9, 12, 10, 10, 0, 0, time.UTC)) {
		t.Fatal("a returned claim must count as active")
	}

	if len(ts.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v", ts.Conflicts)
	}
	cf := ts.Conflicts[0]
	if cf.Key != "CF-14" || cf.Kind != "path_overlap" || cf.Severity != "high" {
		t.Fatalf("conflict = %+v", cf)
	}
	if !cf.Open() || cf.Occurrences != 2 {
		t.Fatalf("conflict status = %q occurrences = %d", cf.Status, cf.Occurrences)
	}
	if cf.SuggestedAction == "" {
		t.Fatal("the suggested action is the only part of a conflict that is not noise")
	}
	if got := cf.RaisedAt.UTC().Format(time.RFC3339); got != "2026-09-12T10:03:00Z" {
		t.Fatalf("raised_at = %s", got)
	}
	if len(cf.Participants) != 2 {
		t.Fatalf("participants = %+v", cf.Participants)
	}
	// member_key is not on a participant; it is resolved through the session.
	if cf.Participants[0].MemberKey != "M-1" || cf.Participants[1].MemberKey != "M-2" {
		t.Fatalf("participant member keys = %+v", cf.Participants)
	}

	if len(ts.Contracts) != 2 {
		t.Fatalf("contracts = %+v", ts.Contracts)
	}
	bookings := ts.Contracts[0]
	if bookings.Key != "POST /api/bookings" || bookings.Agreement != "unclaimed" {
		t.Fatalf("contract = %+v", bookings)
	}
	if len(bookings.Consumers()) != 1 || len(bookings.Producers()) != 0 {
		t.Fatalf("the unclaimed contract should have one consumer and no producer: %+v", bookings.Assertions)
	}
	if ts.Contracts[1].Agreement != "mismatch" {
		t.Fatalf("agreement = %q, want mismatch", ts.Contracts[1].Agreement)
	}

	if len(ts.Decisions) != 1 {
		t.Fatalf("decisions = %+v", ts.Decisions)
	}
	d := ts.Decisions[0]
	// The schema says "accepted" for a settled decision; the board says
	// "active", and the adapter is where the two are reconciled.
	if d.Status != "active" {
		t.Fatalf("decision status = %q, want active", d.Status)
	}
	if d.Scope != "web/auth/**, api/auth/**" || !d.AlwaysShow {
		t.Fatalf("decision = %+v", d)
	}
}

// TestFrameAdaptsToEvent covers the stream half of the adapter.
func TestFrameAdaptsToEvent(t *testing.T) {
	b := &fakeBackend{snapshotSeq: 0, events: seedEvents(1)}
	live, _ := newLive(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := live.Stream(ctx, 0)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	ev := collect(t, ch, 1)[0]

	if ev.Sequence != 1 || ev.Kind != "intent_updated" || ev.Summary != "event 1" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.SessionKey != "S-17" || ev.MemberKey != "M-1" {
		t.Fatalf("event identity = %+v", ev)
	}
	// The same derivation as the snapshot's, or the timeline would point at an
	// agent the board does not have a lane for.
	if ev.AgentKey != "M-1/api" {
		t.Fatalf("agent key = %q, want M-1/api", ev.AgentKey)
	}
	if ev.Structural {
		t.Fatal("event 1 is not structural")
	}
	// The frame's board_revision is the team's CURRENT one, which is the wrong
	// number for a replayed row; the store stamps the right one.
	if ev.BoardRevision != 0 {
		t.Fatalf("board revision = %d, want 0 — the store stamps it", ev.BoardRevision)
	}
	if string(ev.Payload) != `{"message":"working"}` {
		t.Fatalf("payload = %s", ev.Payload)
	}
	if got := ev.OccurredAt.UTC().Format(time.RFC3339); got != "2026-09-12T10:00:01Z" {
		t.Fatalf("occurred_at = %s", got)
	}
}

// TestStreamSurvivesAnUnparseableFrame — one bad frame must not stall the
// cursor or kill the connection.
func TestStreamSurvivesAnUnparseableFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "id: 1\nevent: note\ndata: {not json\n\n")
		fmt.Fprint(w, ": keepalive\n\n")
		fmt.Fprint(w, "id: 2\nevent: note\ndata: {\"sequence\":2,\"kind\":\"note\",\"summary\":\"second\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	live := &Live{BaseURL: srv.URL, Slug: "demo", Backoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := live.Stream(ctx, 0)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	ev := collect(t, ch, 1)[0]
	if ev.Sequence != 2 || ev.Summary != "second" {
		t.Fatalf("event = %+v", ev)
	}
}

// TestStreamRefusesToGoBackwards — a server that resends an old frame (a
// proxy replaying a buffer, an overlapping reconnect) must not push the
// timeline backwards.
func TestStreamRefusesToGoBackwards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, seq := range []int{5, 4, 5, 6} {
			fmt.Fprintf(w, "id: %d\nevent: note\ndata: {\"sequence\":%d,\"kind\":\"note\"}\n\n", seq, seq)
		}
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	live := &Live{BaseURL: srv.URL, Slug: "demo", Backoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := live.Stream(ctx, 0)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	got := collect(t, ch, 2)
	if got[0].Sequence != 5 || got[1].Sequence != 6 {
		t.Fatalf("delivered %v, want [5 6]", sequences(got))
	}
}

// TestRootAcceptsEitherFormOfBaseURL — an operator will write both.
func TestRootAcceptsEitherFormOfBaseURL(t *testing.T) {
	for _, in := range []string{"http://api.example", "http://api.example/", "http://api.example/v1", "http://api.example/v1/"} {
		l := &Live{BaseURL: in}
		if got := l.root(); got != "http://api.example/v1" {
			t.Fatalf("root(%q) = %q", in, got)
		}
	}
}
