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

	// A human acting from the board injects an event locally, which consumes a
	// sequence number the feed does not know about. drift keeps the feed's own
	// numbering monotonic on top of that, so an injected event never makes the
	// store start rejecting real ones.
	seqMu    sync.Mutex
	drift    int64
	lastFeed int64

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
func (h *Hub) Run(ctx context.Context, f feed.Feed) error {
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
	h.seqMu.Lock()
	h.lastFeed = ev.Sequence
	ev.Sequence += h.drift
	h.seqMu.Unlock()

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

func (h *Hub) broadcastBoard() {
	snap := h.store.Snapshot()
	h.broadcast(Frame{
		Name: "board",
		ID:   snap.Team.Sequence,
		HTML: render(h.renderer.Board(snap)),
	})
}

// Inject applies an event raised by a human on this board rather than by the
// feed — resolving a conflict, nudging an agent. Once the backend exists these
// become POSTs to it and arrive back through the feed like everything else;
// until then they are applied locally so the controls are real and not mimed.
func (h *Hub) Inject(ev model.Event) model.Event {
	ev.Sequence = 0 // let the store assign the next one
	applied, ok := h.store.Apply(ev)
	if !ok {
		return applied
	}
	h.seqMu.Lock()
	h.drift = applied.Sequence - h.lastFeed
	h.seqMu.Unlock()

	snap := h.store.Snapshot()
	h.broadcast(Frame{Name: "timeline", ID: applied.Sequence, HTML: render(h.renderer.TimelineItem(snap, applied))})
	if applied.Structural {
		h.broadcastBoard()
	}
	return applied
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
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
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
