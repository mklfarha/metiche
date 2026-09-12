package stream

import (
	"context"
	"sync"
	"time"

	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/stream/publish"
)

// TailInterval is how often a WATCHED team is polled for new events.
//
// It is the backstop, not the delivery mechanism. In a single-process
// deployment (METICHE_ROLE=all, which is v1) every write reaches a browser via
// the in-process fast path in a couple of milliseconds and this ticker finds
// nothing on almost every tick. What it buys is the case the fast path cannot
// cover: a write committed by ANOTHER process. Then this is the whole delivery
// path, and 250ms is the worst case a browser attached here waits for it.
//
// 250ms rather than something larger because the cost is one indexed range
// scan per watched team, and rather than something smaller because below that
// the poll rate stops buying perceptible latency and starts buying load.
const TailInterval = 250 * time.Millisecond

// subscriberBuffer is how far behind a single browser may fall before it is
// dropped from a fan-out rather than waited on.
//
// Dropping is recoverable and blocking is not: a dropped frame leaves a gap in
// the client's sequence, which it notices and fixes by reconnecting with
// ?after=<last good>. Blocking would stall the tailer that serves every other
// browser on the team.
const subscriberBuffer = 128

// Hub fans team events out to the browsers watching a team.
//
// It runs two delivery paths on purpose, and the second one is why the first
// one is safe to have:
//
//  1. The FAST PATH. The write path calls Publish immediately after its
//     transaction commits. That wakes the team's tailer, which reads and fans
//     out within a couple of milliseconds instead of waiting for a tick.
//
//  2. The BACKSTOP. The same tailer polls every TailInterval regardless. A
//     write committed in another process never calls this process's Publish,
//     and without the poll a browser attached here would simply never see it.
//
// Having both means the same event can be picked up twice — the fast path's
// read racing a tick. Deduplication is one comparison in deliver(), done while
// holding the lock that also advances the cursor, so a frame at or below the
// team's high-water mark is dropped before it reaches any subscriber. That
// check is the entire reason two paths are safe rather than a source of
// double-painted boards.
//
// Only teams with at least one subscriber are tailed. An idle team costs
// nothing: no goroutine, no query, no timer.
type Hub struct {
	src    Source
	logger *zap.Logger
	tail   time.Duration

	mu     sync.Mutex
	teams  map[uuid.UUID]*teamWatch
	nextID uint64
	closed bool

	wg sync.WaitGroup
}

// teamWatch is the set of subscribers on one team, plus the tailer serving
// them.
type teamWatch struct {
	chans map[uint64]chan Frame

	// lastSeen is the highest sequence this process has already fanned out for
	// the team. It is both the cursor the next read starts from and the
	// deduplication high-water mark.
	lastSeen int64

	// wake is the fast path. Buffered to one and written to without blocking:
	// a burst of commits coalesces into a single extra read, and the caller —
	// a tool response on its way out — never waits.
	wake chan struct{}

	cancel context.CancelFunc
}

// NewHub returns a hub reading from src. logger may be nil.
func NewHub(src Source, logger *zap.Logger) *Hub {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Hub{
		src:    src,
		logger: logger,
		tail:   TailInterval,
		teams:  make(map[uuid.UUID]*teamWatch),
	}
}

// Publish implements publish.Publisher. It is called by the write path AFTER
// its transaction has committed.
//
// It does no work beyond a map lookup and a non-blocking channel send, so the
// tool response it is called from is never slowed by fan-out, and a team
// nobody is watching costs a lookup and nothing else.
func (h *Hub) Publish(ev publish.Event) {
	h.mu.Lock()
	w, ok := h.teams[ev.TeamUUID]
	if !ok || h.closed {
		h.mu.Unlock()
		return
	}
	wake := w.wake
	h.mu.Unlock()

	select {
	case wake <- struct{}{}:
	default:
		// A wake is already pending; the read it triggers will pick this event
		// up too, because the read is "everything after the cursor" and not
		// "the event I was told about".
	}
}

var _ publish.Publisher = (*Hub)(nil)

// Subscribe returns a channel of frames for one team and a release function.
//
// The caller MUST call release, or the subscription and — once it is the last
// one — the team's tailing goroutine outlive the request. Every caller does it
// with a defer immediately after subscribing.
//
// from is the cursor the caller has already handled. It seeds the tailer only
// when this is the FIRST subscriber for the team; a later subscriber must not
// be able to drag an existing one's cursor backwards or forwards. Each SSE
// connection replays its own backlog from the database anyway, so a subscriber
// joining a team that is already being tailed loses nothing.
func (h *Hub) Subscribe(teamUUID uuid.UUID, from int64) (<-chan Frame, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		ch := make(chan Frame)
		close(ch)
		return ch, func() {}
	}

	w, existing := h.teams[teamUUID]
	if !existing {
		ctx, cancel := context.WithCancel(context.Background())
		w = &teamWatch{
			chans:    make(map[uint64]chan Frame),
			lastSeen: from,
			wake:     make(chan struct{}, 1),
			cancel:   cancel,
		}
		h.teams[teamUUID] = w
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.tailTeam(ctx, teamUUID)
		}()
	}

	id := h.nextID
	h.nextID++
	ch := make(chan Frame, subscriberBuffer)
	w.chans[id] = ch

	var once sync.Once
	return ch, func() {
		once.Do(func() { h.release(teamUUID, id) })
	}
}

func (h *Hub) release(teamUUID uuid.UUID, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	w, ok := h.teams[teamUUID]
	if !ok {
		return
	}
	if ch, ok := w.chans[id]; ok {
		close(ch)
		delete(w.chans, id)
	}
	if len(w.chans) == 0 {
		// Nobody is watching this team any more. Stop querying for it — the
		// "tail only teams with a subscriber" rule is enforced here.
		w.cancel()
		delete(h.teams, teamUUID)
	}
}

// Close stops every tailer and waits for them. Subscribers' channels are
// closed, which their readers see as a clean end of stream.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for id, w := range h.teams {
		for cid, ch := range w.chans {
			close(ch)
			delete(w.chans, cid)
		}
		w.cancel()
		delete(h.teams, id)
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// tailTeam serves one team for as long as somebody is watching it.
//
// One goroutine handles both delivery paths. The ticker is the backstop and
// the wake channel is the fast path, and they call the same read and the same
// fan-out — so a frame delivered because a local commit woke us is
// indistinguishable from one delivered because the poll found it, which is
// what makes the fast path safe to add to a system that was already correct
// without it.
func (h *Hub) tailTeam(ctx context.Context, teamUUID uuid.UUID) {
	ticker := time.NewTicker(h.tail)
	defer ticker.Stop()

	wake := h.wakeChan(teamUUID)
	if wake == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.pollOnce(ctx, teamUUID)
		case <-wake:
			h.pollOnce(ctx, teamUUID)
		}
	}
}

func (h *Hub) wakeChan(teamUUID uuid.UUID) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w, ok := h.teams[teamUUID]; ok {
		return w.wake
	}
	return nil
}

// pollOnce reads everything after the team's cursor and fans it out, looping
// while the source keeps returning full batches so a large backlog is not left
// to trickle out one tick at a time.
//
// The read runs WITHOUT the hub lock held. Holding it across a query would let
// one slow database call block every other team's fan-out, and deliver()
// re-checks the cursor under the lock anyway — which is what makes it safe for
// this read to be working from a cursor that has moved by the time its rows
// come back.
func (h *Hub) pollOnce(ctx context.Context, teamUUID uuid.UUID) {
	for {
		h.mu.Lock()
		w, ok := h.teams[teamUUID]
		if !ok || h.closed {
			h.mu.Unlock()
			return
		}
		from := w.lastSeen
		h.mu.Unlock()

		frames, err := h.src.FramesAfter(ctx, teamUUID, from, batchLimit)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A transient read failure must never kill the stream: the next
			// tick retries from the same cursor, and nothing has been
			// acknowledged, so nothing is lost.
			h.logger.Warn("tailing team events failed", zap.Error(err))
			return
		}
		if len(frames) == 0 {
			return
		}
		h.deliver(teamUUID, frames)
		if len(frames) < batchLimit {
			return
		}
	}
}

// deliver fans frames out to a team's subscribers, dropping any that have
// already been delivered.
//
// This is the deduplication point, and it is deliberately the same place the
// cursor advances: both happen under one lock, so two reads that overlap — the
// fast path's and the tailer's, or two tailers' after a slow query — cannot
// both decide a frame is new. Whichever gets the lock first delivers it and
// moves the high-water mark; the other sees Sequence <= lastSeen and drops it.
//
// Without this, having two delivery paths would mean every locally committed
// event painting twice on every open board.
func (h *Hub) deliver(teamUUID uuid.UUID, frames []Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()

	w, ok := h.teams[teamUUID]
	if !ok {
		return
	}
	for _, f := range frames {
		if f.Sequence <= w.lastSeen {
			continue
		}
		for _, ch := range w.chans {
			select {
			case ch <- f:
			default:
				// This subscriber is too far behind. Dropping leaves it with a
				// sequence gap it can see and recover from by reconnecting;
				// blocking here would punish every other browser on the team
				// for one slow one.
			}
		}
		w.lastSeen = f.Sequence
	}
}
