// Package hub turns one team's event feed into rendered HTML fragments and
// fans them out over SSE.
//
// This is the half of the realtime design the browser sees. The two cursors in
// the data model map onto two named SSE event types, and nothing else:
//
//	sequence       -> event: timeline   the client appends   (hx-swap=beforeend)
//	board_revision -> event: board      the client replaces  (hx-swap=outerHTML)
//
// Every event moves the sequence, so every event produces a timeline frame.
// Only structural events move the board revision, so a heartbeat costs one
// small <li> and does not repaint a board somebody is reading. Frames carry
// HTML, never JSON: the browser does no templating, which is the whole reason
// this service exists as a separate process.
package hub

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
)

// Frame is one SSE message: a named event carrying an HTML fragment.
type Frame struct {
	// Name is the SSE event name — "timeline" or "board".
	Name string
	// ID is the sequence this frame corresponds to. It goes out as the SSE
	// id:, so a browser reconnect sends Last-Event-ID and the server can
	// resume exactly where it left off without the client tracking anything.
	ID   int64
	HTML string
}

// Renderer turns a snapshot into the fragments the client swaps. It lives here
// as an interface so the hub does not import the view package (and the view
// package can keep importing state).
type Renderer interface {
	Board(s state.Snapshot) templ.Component
	TimelineItem(s state.Snapshot, ev model.Event) templ.Component
}

// Subscriber is one open browser connection.
type Subscriber struct {
	C      chan Frame
	closed bool
}

// Hub owns one team: its store, its feed, and its subscribers.
type Hub struct {
	Slug string

	store    *state.Store
	renderer Renderer
	log      *slog.Logger

	mu   sync.Mutex
	subs map[*Subscriber]struct{}

	// snapshots is set when the feed can read whole state from the backend.
	// It is nil for a fixture, and that nil is what selects between the two
	// ways a board can be correct — see Run.
	snapshots feed.Snapshotter

	// closed is set by Close, under mu. A closed hub has no subscribers and
	// accepts none: Subscribe hands back an already-closed channel.
	closed bool

	started bool
	done    chan struct{}
}

// New wires a hub. It does not start consuming until Run is called.
func New(slug, name string, r Renderer, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		Slug:     slug,
		store:    state.New(slug, name),
		renderer: r,
		log:      log,
		subs:     map[*Subscriber]struct{}{},
		done:     make(chan struct{}),
	}
}

// Store exposes the folded state for page handlers.
func (h *Hub) Store() *state.Store { return h.store }

// Snapshot is a shorthand for Store().Snapshot().
func (h *Hub) Snapshot() state.Snapshot { return h.store.Snapshot() }

// Subscribers reports the current connection count.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Run consumes the feed until ctx ends. Events are applied to the store and
// broadcast; a burst of structural events coalesces into a single board
// repaint, because repainting four times in one tick is indistinguishable from
// repainting once except in how much it flickers.
//
// If the feed can also produce a snapshot, the snapshot is read FIRST and the
// stream is opened from the cursor it reports. That order is the whole of the
// "no gap, no double-apply" guarantee on the live path:
//
//	state at sequence S  ──►  stream ?after=S  ──►  S+1, S+2, …
//
// Everything at or below S is in the state; everything above it arrives on the
// stream; and Apply rejects anything not strictly newer than the cursor, so an
// overlap at the boundary costs nothing. The other order — subscribe, then
// read state — leaves a window whose events are in neither.
func (h *Hub) Run(ctx context.Context, f feed.Feed) error {
	if s, ok := f.(feed.Snapshotter); ok {
		ts, err := s.Snapshot(ctx)
		if err != nil {
			return err
		}
		h.snapshots = s
		h.store.Load(ts)
		h.log.Info("loaded team snapshot", "slug", h.Slug,
			"sequence", ts.Team.Sequence, "board_revision", ts.Team.BoardRevision,
			"resume_from", h.store.Sequence(), "sessions", len(ts.Sessions))
	}

	events, err := f.Stream(ctx, h.store.Sequence())
	if err != nil {
		return err
	}
	h.started = true
	go func() {
		defer close(h.done)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					h.log.Info("feed exhausted", "slug", h.Slug, "sequence", h.store.Sequence())
					return
				}
				structural := h.ingest(ev)
				// Drain anything already queued so a burst is one repaint.
				drained := true
				for drained {
					select {
					case next, ok := <-events:
						if !ok {
							drained = false
							break
						}
						if h.ingest(next) {
							structural = true
						}
					default:
						drained = false
					}
				}
				if structural {
					h.refresh(ctx)
					h.broadcastBoard()
				}
			}
		}
	}()
	return nil
}

// Done closes when the feed is exhausted or the context ends.
func (h *Hub) Done() <-chan struct{} { return h.done }

// Started reports whether Run has been called successfully.
func (h *Hub) Started() bool { return h.started }

func (h *Hub) ingest(ev model.Event) bool {
	applied, ok := h.store.Apply(ev)
	if !ok {
		return false
	}
	snap := h.store.Snapshot()
	h.broadcast(Frame{
		Name: "timeline",
		ID:   applied.Sequence,
		HTML: render(h.renderer.TimelineItem(snap, applied)),
	})
	return applied.Structural
}

// refresh re-reads state from the backend after a structural change.
//
// This is the second cursor doing its job. sequence moved on every frame and
// each one cost a timeline row; board_revision moved only on the ones that
// changed the SHAPE of the board, and only those are worth a round trip and a
// re-layout. A status line edit repaints nothing here.
//
// It is a no-op for a fixture feed, which has no backend to ask. A failed
// refresh is logged and dropped: the board keeps the state it had and the next
// structural frame tries again, which is a great deal better than blanking a
// board somebody is reading because one request timed out.
func (h *Hub) refresh(ctx context.Context) {
	if h.snapshots == nil {
		return
	}
	ts, err := h.snapshots.Snapshot(ctx)
	if err != nil {
		if ctx.Err() == nil {
			h.log.Warn("refreshing team state failed; keeping the last good board",
				"slug", h.Slug, "err", err)
		}
		return
	}
	h.store.Refresh(ts)
}

func (h *Hub) broadcastBoard() {
	snap := h.store.Snapshot()
	h.broadcast(Frame{
		Name: "board",
		ID:   snap.Team.Sequence,
		HTML: render(h.renderer.Board(snap)),
	})
}

// There is deliberately no way to apply an event that did not come from the
// feed. The hub used to have one (Inject), for board controls that resolved
// conflicts, nudged agents and changed pace; they had no authentication, and
// what they applied was broadcast to every viewer of the board as if the team
// had done it. Everything a board shows now comes from its feed.

// Closed reports whether Close has been called.
func (h *Hub) Closed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// Close ends every open subscription and refuses new ones. It is how a team
// that is no longer allowed to be shown gets its open browsers off it: each
// stream handler sees its channel close and ends the response. It does not
// stop the feed — that is the context's job — and it is safe to call twice.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for sub := range h.subs {
		delete(h.subs, sub)
		if !sub.closed {
			sub.closed = true
			close(sub.C)
		}
	}
}

// Replay returns the frames a client reconnecting at `after` has missed: every
// timeline item past the cursor, then the current board. Sending the board
// last means a client that missed a structural change gets the truth even if
// it also missed the events that caused it.
func (h *Hub) Replay(after int64) []Frame {
	missed := h.store.EventsAfter(after)
	snap := h.store.Snapshot()
	frames := make([]Frame, 0, len(missed)+1)
	for _, ev := range missed {
		frames = append(frames, Frame{
			Name: "timeline",
			ID:   ev.Sequence,
			HTML: render(h.renderer.TimelineItem(snap, ev)),
		})
	}
	frames = append(frames, Frame{
		Name: "board",
		ID:   snap.Team.Sequence,
		HTML: render(h.renderer.Board(snap)),
	})
	return frames
}

// Subscribe registers a connection. The buffer is generous and a subscriber
// that fills it is dropped rather than allowed to block the fan-out: a stalled
// browser must not slow the board down for everyone else, and it will
// reconnect with its cursor and lose nothing.
func (h *Hub) Subscribe() *Subscriber {
	sub := &Subscriber{C: make(chan Frame, 256)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		sub.closed = true
		close(sub.C)
		return sub
	}
	h.subs[sub] = struct{}{}
	return sub
}

// Unsubscribe removes a connection.
func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		if !sub.closed {
			sub.closed = true
			close(sub.C)
		}
	}
}

func (h *Hub) broadcast(f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.closed {
			continue
		}
		select {
		case sub.C <- f:
		default:
			h.log.Warn("dropping slow subscriber", "slug", h.Slug)
			delete(h.subs, sub)
			sub.closed = true
			close(sub.C)
		}
	}
}

func render(c templ.Component) string {
	var b strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Render(ctx, &b); err != nil {
		return "<!-- render error -->"
	}
	return b.String()
}
