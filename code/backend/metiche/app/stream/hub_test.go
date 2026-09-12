package stream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/stream/publish"
)

// fakeSource stands in for the team_event table.
//
// The ordering and deduplication in this package are the subtle part, and they
// are pure logic over "give me everything after N" — so they are tested
// without a database in the way. The one database-backed behaviour, that the
// SQL actually produces these frames, is covered separately in
// source_mysql_test.go against a real MySQL.
type fakeSource struct {
	mu sync.Mutex

	log []Frame

	// calls counts FramesAfter invocations, which is how the "only tail teams
	// with a subscriber" rule is checked.
	calls int

	// replayEverything makes the source ignore the cursor and hand back the
	// whole log every time.
	//
	// That is not a contrived failure: it is exactly what the two delivery
	// paths look like from the hub's side when they race. The fast path reads
	// from the cursor, the tailer's tick reads from the same cursor before the
	// first has finished delivering, and both come back holding the same rows.
	// A subscriber must still see each sequence once.
	replayEverything bool
}

func (f *fakeSource) FramesAfter(_ context.Context, _ uuid.UUID, after int64, limit int) ([]Frame, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	out := make([]Frame, 0, len(f.log))
	for _, fr := range f.log {
		if !f.replayEverything && fr.Sequence <= after {
			continue
		}
		out = append(out, fr)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeSource) append(seqs ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range seqs {
		f.log = append(f.log, Frame{Sequence: s, Kind: "intent_declared", BoardRevision: 7})
	}
}

func (f *fakeSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testTeam(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.FromString("11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id
}

// collect drains a frame channel until it has n frames or the deadline passes.
func collect(t *testing.T, ch <-chan Frame, n int, within time.Duration) []Frame {
	t.Helper()
	out := make([]Frame, 0, n)
	deadline := time.After(within)
	for len(out) < n {
		select {
		case f, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
	return out
}

// TestTwoDeliveryPathsDoNotDoubleDeliver is the proof that running an
// in-process fast path AND a database tailer over the same events is safe.
//
// The source here hands back every frame it holds on every read, ignoring the
// cursor — the worst case of the two paths racing, where both reads see the
// same rows as new. Over fifty ticks and fifty publishes the subscriber must
// still see each sequence exactly once, in order.
func TestTwoDeliveryPathsDoNotDoubleDeliver(t *testing.T) {
	src := &fakeSource{replayEverything: true}
	src.append(1, 2, 3, 4, 5)

	hub := NewHub(src, nil)
	// Fast ticks, so the backstop fires many times over the life of the test
	// and every one of them re-offers the whole log.
	hub.tail = 2 * time.Millisecond
	defer hub.Close()

	team := testTeam(t)
	frames, release := hub.Subscribe(team, 0)
	defer release()

	// Hammer the fast path at the same time as the ticker is running.
	for i := 0; i < 50; i++ {
		hub.Publish(publish.Event{TeamUUID: team, Sequence: 5})
	}

	got := collect(t, frames, 5, time.Second)
	if len(got) != 5 {
		t.Fatalf("expected 5 frames, got %d: %v", len(got), seqsOf(got))
	}
	for i, f := range got {
		if want := int64(i + 1); f.Sequence != want {
			t.Fatalf("frame %d has sequence %d, want %d (got %v)", i, f.Sequence, want, seqsOf(got))
		}
	}

	// Give both paths a generous window to deliver a duplicate if the
	// deduplication is not doing its job, then assert nothing else arrived.
	time.Sleep(100 * time.Millisecond)
	select {
	case extra := <-frames:
		t.Fatalf("received a duplicate frame: sequence %d", extra.Sequence)
	default:
	}

	if src.callCount() < 2 {
		t.Fatalf("expected the source to be read repeatedly by both paths, got %d reads", src.callCount())
	}
}

// TestFastPathDeliversWithoutWaitingForTheTick pins the reason the fast path
// exists: a locally committed event must not wait out the poll interval.
func TestFastPathDeliversWithoutWaitingForTheTick(t *testing.T) {
	src := &fakeSource{}
	hub := NewHub(src, nil)
	// An interval long enough that the backstop cannot be what delivers.
	hub.tail = time.Hour
	defer hub.Close()

	team := testTeam(t)
	frames, release := hub.Subscribe(team, 0)
	defer release()

	src.append(1)
	start := time.Now()
	hub.Publish(publish.Event{TeamUUID: team, Sequence: 1})

	got := collect(t, frames, 1, 2*time.Second)
	if len(got) != 1 || got[0].Sequence != 1 {
		t.Fatalf("fast path did not deliver: %v", seqsOf(got))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fast path took %v, which is the tick, not the fast path", elapsed)
	}
}

// TestBackstopDeliversWithoutAPublish pins the reason the tailer exists: an
// event committed by ANOTHER process never calls this process's Publish, and
// must still reach a browser attached here.
func TestBackstopDeliversWithoutAPublish(t *testing.T) {
	src := &fakeSource{}
	hub := NewHub(src, nil)
	hub.tail = 5 * time.Millisecond
	defer hub.Close()

	team := testTeam(t)
	frames, release := hub.Subscribe(team, 0)
	defer release()

	// No Publish call at all — this is the cross-process case.
	src.append(1, 2)

	got := collect(t, frames, 2, 2*time.Second)
	if len(got) != 2 || got[0].Sequence != 1 || got[1].Sequence != 2 {
		t.Fatalf("backstop did not deliver: %v", seqsOf(got))
	}
}

// TestIdleTeamsAreNotPolled pins "tail only teams that currently have a
// subscriber": no subscriber, no query — before the first one and after the
// last one lets go.
func TestIdleTeamsAreNotPolled(t *testing.T) {
	src := &fakeSource{}
	hub := NewHub(src, nil)
	hub.tail = 5 * time.Millisecond
	defer hub.Close()

	team := testTeam(t)

	// Nobody is watching.
	hub.Publish(publish.Event{TeamUUID: team, Sequence: 1})
	time.Sleep(50 * time.Millisecond)
	if n := src.callCount(); n != 0 {
		t.Fatalf("an idle team was polled %d times", n)
	}

	_, release := hub.Subscribe(team, 0)
	time.Sleep(50 * time.Millisecond)
	during := src.callCount()
	if during == 0 {
		t.Fatal("a watched team was never polled")
	}

	release()
	// Let any in-flight tick finish, then take the baseline.
	time.Sleep(30 * time.Millisecond)
	after := src.callCount()
	time.Sleep(80 * time.Millisecond)
	if now := src.callCount(); now != after {
		t.Fatalf("the team was still polled %d more times after the last subscriber left", now-after)
	}
}

// TestReleaseIsIdempotentAndStopsTheTailer guards the leak: the SSE handler
// releases with a defer, and a double release — or a release racing Close —
// must not panic or close a channel twice.
func TestReleaseIsIdempotentAndStopsTheTailer(t *testing.T) {
	src := &fakeSource{}
	hub := NewHub(src, nil)
	hub.tail = 5 * time.Millisecond

	team := testTeam(t)
	chA, releaseA := hub.Subscribe(team, 0)
	_, releaseB := hub.Subscribe(team, 0)

	releaseA()
	releaseA() // idempotent

	if _, open := <-chA; open {
		t.Fatal("a released subscriber's channel should be closed")
	}

	releaseB()
	hub.Close()
	hub.Close() // idempotent
}

// TestSubscribeDoesNotMoveAnotherSubscribersCursor guards the case of a second
// browser attaching to a team that is already being tailed: it must not be
// able to drag the shared cursor, in either direction.
func TestSubscribeDoesNotMoveAnotherSubscribersCursor(t *testing.T) {
	src := &fakeSource{}
	hub := NewHub(src, nil)
	hub.tail = time.Hour
	defer hub.Close()

	team := testTeam(t)
	first, releaseFirst := hub.Subscribe(team, 0)
	defer releaseFirst()

	// A second subscriber that claims to have already seen everything.
	second, releaseSecond := hub.Subscribe(team, 1000)
	defer releaseSecond()

	src.append(1, 2, 3)
	hub.Publish(publish.Event{TeamUUID: team, Sequence: 3})

	got := collect(t, first, 3, 2*time.Second)
	if len(got) != 3 {
		t.Fatalf("the first subscriber lost frames when a second one joined: %v", seqsOf(got))
	}
	// The second subscriber gets them too; its own SSE connection is what
	// filters against the cursor it asked for.
	if len(collect(t, second, 3, time.Second)) != 3 {
		t.Fatal("the second subscriber received nothing")
	}
}

func seqsOf(frames []Frame) []int64 {
	out := make([]int64, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Sequence)
	}
	return out
}
