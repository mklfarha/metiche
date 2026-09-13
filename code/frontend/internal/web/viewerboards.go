package web

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// viewerBoards holds the private boards open for signed-in viewers: one hub
// per (browser session, team), each read from the backend with that viewer's
// own session (§4.2).
//
// It is deliberately a SEPARATE structure from Server.teams. Server.teams is
// what an anonymous request resolves against, what Teams() lists and what the
// public pages are built from; a private board in it would be served to
// everybody. Nothing here is ever consulted for an anonymous request.
//
// A board is closed when:
//   - it has had no subscriber for IdleGrace (the page load → stream connect
//     gap needs a grace), and nobody has asked for it in that time;
//   - its upstream answers 401 or 404, or a stream re-check refuses it;
//   - its viewer signs out, or the session is found invalid;
//   - the cap is reached and it is the least recently used idle board.
type viewerBoards struct {
	s   *Server
	l   *login
	ctx context.Context

	mu       sync.Mutex
	boards   map[viewerBoardKey]*viewerBoard
	inflight map[viewerBoardKey]*boardFlight
}

type viewerBoardKey struct {
	session string // the session's public key, not its secret
	slug    string
}

type viewerBoard struct {
	key     viewerBoardKey
	account string
	hash    string // hex sha256 of the session secret
	team    *Team

	lastUsed  time.Time // last resolved by a request
	idleSince time.Time // zero while it has subscribers
}

type boardFlight struct {
	done   chan struct{}
	team   *Team
	result resolveResult
}

func newViewerBoards(ctx context.Context, s *Server, l *login) *viewerBoards {
	return &viewerBoards{s: s, l: l, ctx: ctx,
		boards: map[viewerBoardKey]*viewerBoard{}, inflight: map[viewerBoardKey]*boardFlight{}}
}

// get returns the viewer's private board for slug, starting it if needed. It
// is called only after the backend's /access said this viewer may read a
// private team.
func (vb *viewerBoards) get(ctx context.Context, v *Viewer, slug string) (*Team, resolveResult) {
	key := viewerBoardKey{session: v.SessionKey, slug: slug}
	vb.mu.Lock()
	if b, ok := vb.boards[key]; ok && !b.team.Hub.Closed() {
		b.lastUsed = time.Now()
		vb.mu.Unlock()
		return b.team, resolveFound
	}
	if f, ok := vb.inflight[key]; ok {
		vb.mu.Unlock()
		select {
		case <-f.done:
			return f.team, f.result
		case <-ctx.Done():
			return nil, resolveUnavailable
		}
	}
	if len(vb.boards)+len(vb.inflight) >= vb.l.cfg.MaxViewerBoards && !vb.evictIdleLocked() {
		vb.mu.Unlock()
		vb.s.log.Warn("private boards are at their cap and none is idle", "max_viewer_boards", vb.l.cfg.MaxViewerBoards)
		return nil, resolveFull
	}
	f := &boardFlight{done: make(chan struct{})}
	vb.inflight[key] = f
	vb.mu.Unlock()

	// On the boards' context, not the request's: a second tab may be waiting
	// on this flight.
	board, ref, result := vb.start(v, slug, key)

	vb.mu.Lock()
	delete(vb.inflight, key)
	if board != nil {
		vb.boards[key] = board
		f.team = board.team
	}
	f.result = result
	close(f.done)
	vb.mu.Unlock()

	if board != nil {
		ref.Store(board)
	}
	return f.team, f.result
}

// start builds the feed and the hub. The returned pointer is filled once the
// board is in the map, so an upstream 404 reported before then is not lost:
// see the reported flag.
func (vb *viewerBoards) start(v *Viewer, slug string, key viewerBoardKey) (*viewerBoard, *boardRef, resolveResult) {
	f := vb.l.cfg.NewViewerFeed(slug, v.secret)
	ref := &boardRef{vb: vb}
	if r, ok := f.(feed.NotFoundReporter); ok {
		r.OnNotFound(ref.refused)
	}
	t, err := vb.s.startTeam(vb.ctx, slug, slug, "", false, false, f)
	if err != nil {
		if errors.Is(err, feed.ErrNotFound) {
			// /access said yes and the snapshot said no: access changed in
			// between. The same answer as any other not-found.
			return nil, nil, resolveNotFound
		}
		vb.s.log.Warn("a private board failed to start", "err", err)
		return nil, nil, resolveUnavailable
	}
	t.viewer = true
	now := time.Now()
	return &viewerBoard{key: key, account: v.AccountKey, hash: v.hash, team: t, lastUsed: now, idleSince: now}, ref, resolveFound
}

// boardRef connects a feed's 404 callback to the board it feeds.
type boardRef struct {
	vb       *viewerBoards
	board    atomic.Pointer[viewerBoard]
	reported atomic.Bool
}

func (r *boardRef) refused() {
	r.reported.Store(true)
	if b := r.board.Load(); b != nil {
		r.vb.evict(b, "upstream refused the viewer")
	}
}

// Store records the registered board; a refusal that arrived first closes it
// now, since the feed's stream does not report a 404 twice.
func (r *boardRef) Store(b *viewerBoard) {
	r.board.Store(b)
	if r.reported.Load() {
		r.vb.evict(b, "upstream refused the viewer")
	}
}

// evict closes one board, if it is still the one registered under its key.
func (vb *viewerBoards) evict(b *viewerBoard, why string) bool {
	vb.mu.Lock()
	if vb.boards[b.key] != b {
		vb.mu.Unlock()
		return false
	}
	delete(vb.boards, b.key)
	vb.mu.Unlock()
	vb.close(b, why)
	return true
}

// evictWhere closes every board match selects.
func (vb *viewerBoards) evictWhere(why string, match func(*viewerBoard) bool) int {
	vb.mu.Lock()
	var victims []*viewerBoard
	for k, b := range vb.boards {
		if match(b) {
			delete(vb.boards, k)
			victims = append(victims, b)
		}
	}
	vb.mu.Unlock()
	for _, b := range victims {
		vb.close(b, why)
	}
	return len(victims)
}

// evictIdleLocked closes the least recently used board with no subscriber.
// Called with vb.mu held.
func (vb *viewerBoards) evictIdleLocked() bool {
	var victim *viewerBoard
	for _, b := range vb.boards {
		if b.team.Hub.Subscribers() > 0 {
			continue
		}
		if victim == nil || b.lastUsed.Before(victim.lastUsed) {
			victim = b
		}
	}
	if victim == nil {
		return false
	}
	delete(vb.boards, victim.key)
	vb.close(victim, "cap reached; least recently used idle board")
	return true
}

// close stops a board's feed and ends its open streams. Neither the slug nor
// the session is logged.
func (vb *viewerBoards) close(b *viewerBoard, why string) {
	b.team.cancel()
	b.team.Hub.Close()
	vb.s.log.Info("private board closed", "reason", why)
}

// reap closes boards idle for longer than grace, until the boards' context
// ends (which stops every feed anyway).
func (vb *viewerBoards) reap(grace time.Duration) {
	period := grace / 4
	if period < 10*time.Millisecond {
		period = 10 * time.Millisecond
	}
	if period > 30*time.Second {
		period = 30 * time.Second
	}
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-vb.ctx.Done():
			return
		case <-tick.C:
			vb.reapOnce(grace)
		}
	}
}

func (vb *viewerBoards) reapOnce(grace time.Duration) {
	now := time.Now()
	vb.mu.Lock()
	var victims []*viewerBoard
	for k, b := range vb.boards {
		if b.team.Hub.Subscribers() > 0 {
			b.idleSince = time.Time{}
			continue
		}
		if b.idleSince.IsZero() {
			b.idleSince = now
			continue
		}
		if now.Sub(b.idleSince) >= grace && now.Sub(b.lastUsed) >= grace {
			delete(vb.boards, k)
			victims = append(victims, b)
		}
	}
	vb.mu.Unlock()
	for _, b := range victims {
		vb.close(b, "idle")
	}
}

// count is the number of open private boards.
func (vb *viewerBoards) count() int {
	vb.mu.Lock()
	defer vb.mu.Unlock()
	return len(vb.boards)
}
