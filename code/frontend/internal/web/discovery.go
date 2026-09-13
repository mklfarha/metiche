package web

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// Discovery configures on-demand registration of live teams.
//
// Without it the board serves exactly the teams it was started with, so a
// team created five minutes ago is a 404 until somebody edits a deploy. With
// it, a request for an unregistered slug asks the backend about that slug and,
// only if the backend answers 200, registers a live feed and serves the board.
//
// The backend answers 404 for a team that does not exist AND for a private
// team (app/authz, deliberately indistinguishable). Discovery asks with no
// credential at all, so it finds public teams only, and everything it
// remembers is an ANONYMOUS answer. A signed-in viewer never reads that
// memory: their access is asked per request (resolveTeam), and a private
// team they may read becomes a board of their own (viewerboards.go), never a
// registered one.
//
// Every request that reaches the backend is one a stranger can cause by typing
// a URL, so the safeguards are the point rather than a detail:
//
//   - a slug that cannot be a slug is refused before any network call;
//   - concurrent first requests for one slug share ONE probe and ONE feed;
//   - a 404 is remembered for NotFoundTTL, so a hammered junk URL costs the
//     backend one read per TTL, not one per request;
//   - discovered teams are capped at MaxTeams, and at the cap nothing new is
//     probed at all;
//   - nothing — no feed, no goroutine — exists for a slug the backend has not
//     confirmed.
//
// And a confirmation is not forever. A team can be made private or deleted
// while its board is open, so a discovered team is TAKEN DOWN — feed stopped,
// open streams closed, slot freed, the 404 remembered — as soon as either its
// feed reads a 404 or the periodic re-probe (RecheckInterval) gets one. The
// re-probe is not optional: the backend authorizes its stream once, when it
// is opened, so a stream that stays healthy never reports the change.
// Demo and pre-registered teams are never taken down.
type Discovery struct {
	// Probe asks the backend about one slug. Required.
	Probe func(ctx context.Context, slug string) (feed.ProbeResult, error)
	// NewFeed builds the live feed for a slug the backend confirmed. It is
	// called only after Probe returned ProbeFound. Required.
	NewFeed func(slug string) feed.Feed

	// MaxTeams caps teams registered through discovery (pre-warmed and demo
	// teams do not count). Default 50.
	MaxTeams int
	// NotFoundTTL is how long a 404 is believed. Short, because a team created
	// or made public a moment ago should not stay invisible for long.
	// Default 30s.
	NotFoundTTL time.Duration
	// ErrorTTL is how long a failed probe (backend down, 5xx) is believed.
	// Shorter still: it is not a verdict about the team, only a brake on
	// retrying an outage once per request. Default 5s.
	ErrorTTL time.Duration
	// MaxRemembered bounds the negative cache itself, since every distinct
	// junk slug is an entry. Default 10000.
	MaxRemembered int
	// RecheckInterval is how often a discovered team's visibility is asked
	// again while its board is up. A team made private stops being served
	// within about one interval even if its feed stays healthy. Each wait is
	// shortened by a random up to a fifth of it, so teams discovered together
	// do not re-probe together, and the bound stays one interval. Default 60s.
	RecheckInterval time.Duration
	// Now is the clock, for tests. Default time.Now. It governs the negative
	// cache only; RecheckInterval runs on real timers.
	Now func() time.Time
}

// EnableDiscovery turns on-demand registration on. ctx is the lifetime of
// every feed discovery creates — the server's, not a request's, because the
// board must outlive the request that first asked for it.
func (s *Server) EnableDiscovery(ctx context.Context, cfg Discovery) {
	if cfg.MaxTeams <= 0 {
		cfg.MaxTeams = 50
	}
	if cfg.NotFoundTTL <= 0 {
		cfg.NotFoundTTL = 30 * time.Second
	}
	if cfg.ErrorTTL <= 0 {
		cfg.ErrorTTL = 5 * time.Second
	}
	if cfg.MaxRemembered <= 0 {
		cfg.MaxRemembered = 10000
	}
	if cfg.RecheckInterval <= 0 {
		cfg.RecheckInterval = 60 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s.disc = &discovery{
		cfg:      cfg,
		ctx:      ctx,
		negative: map[string]remembered{},
		inflight: map[string]*flight{},
	}
}

// slugPattern is what the backend can produce: app/mcp slugKey lowercases,
// keeps [a-z0-9], collapses everything else to single dashes and trims them
// (up to 40 characters, plus a "-xxxxxx" collision suffix). Its lookup also
// accepts a team uuid, which fits the same shape. 64 is comfortably above both.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// ValidSlug reports whether slug is plausible enough to ask the backend about.
func ValidSlug(slug string) bool { return slugPattern.MatchString(slug) }

type resolveResult int

const (
	resolveNotFound    resolveResult = iota // unknown, invalid, or the backend said 404
	resolveFound                            // registered (now, or already)
	resolveUnavailable                      // the backend could not be asked
	resolveFull                             // at the discovery cap
)

type remembered struct {
	until  time.Time
	result resolveResult
}

type flight struct {
	done   chan struct{}
	team   *Team
	result resolveResult
}

type discovery struct {
	cfg Discovery
	ctx context.Context

	mu         sync.Mutex
	discovered int // registered plus in flight: a slot is taken before the probe
	negative   map[string]remembered
	inflight   map[string]*flight
}

// resolveTeam finds the team for a request (§4.2):
//
//  1. A registered slug — demo or public live — always wins and is not looked
//     up on the backend per request, which is what reserves demo slugs. A
//     DISCOVERED team stays registered only while the backend keeps
//     confirming it: see unregister.
//  2. A signed-in viewer: the backend's /access, uncached. Private and allowed
//     → that viewer's own board; public → ordinary discovery; 404 → not
//     found, remembered nowhere.
//  3. A viewer whose session could not be checked: unavailable, not a 404.
//  4. Anonymous: discovery, with its negative cache.
func (s *Server) resolveTeam(ctx context.Context, v *Viewer, vs viewerStatus, slug string) (*Team, resolveResult) {
	if t, ok := s.Lookup(slug); ok {
		return t, resolveFound
	}
	if !ValidSlug(slug) {
		return nil, resolveNotFound
	}
	anonymous := vs == viewerAnonymous
	// The negative cache holds anonymous answers, for anonymous requests.
	if s.disc != nil && anonymous {
		if res, hit := s.disc.recall(slug); hit {
			return nil, res
		}
	}
	switch vs {
	case viewerSignedIn:
		return s.resolveForViewer(ctx, v, slug)
	case viewerUnavailable:
		return nil, resolveUnavailable
	}
	if s.disc == nil {
		return nil, resolveNotFound
	}
	return s.disc.discover(ctx, s, slug, true)
}

// resolveForViewer is step 2 of resolveTeam.
func (s *Server) resolveForViewer(ctx context.Context, v *Viewer, slug string) (*Team, resolveResult) {
	acc, err := s.login.cfg.Backend.Access(ctx, v.secret, slug)
	switch {
	case errors.Is(err, feed.ErrNotFound):
		return nil, resolveNotFound
	case errors.Is(err, feed.ErrSessionInvalid):
		s.login.sessionEnded(v.hash)
		return nil, resolveNotFound
	case err != nil:
		s.log.Warn("asking the backend about a viewer's access failed", "err", err)
		return nil, resolveUnavailable
	case acc.Private():
		return s.login.boards.get(ctx, v, slug)
	case s.disc == nil:
		return nil, resolveNotFound
	default:
		// Public: the shared board every anonymous visitor gets too. The probe
		// discovery makes is itself anonymous; this request only skips the
		// negative cache, and teaches it nothing.
		return s.disc.discover(ctx, s, slug, false)
	}
}

// recall returns a remembered, unexpired negative answer for slug.
func (d *discovery) recall(slug string) (resolveResult, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.negative[slug]
	if !ok {
		return 0, false
	}
	if !d.cfg.Now().Before(m.until) {
		delete(d.negative, slug)
		return 0, false
	}
	return m.result, true
}

// discover probes and registers slug. anonymous says whether the request may
// read and write the negative cache.
func (d *discovery) discover(ctx context.Context, s *Server, slug string, anonymous bool) (*Team, resolveResult) {
	d.mu.Lock()
	now := d.cfg.Now()
	if m, ok := d.negative[slug]; ok && anonymous {
		if now.Before(m.until) {
			d.mu.Unlock()
			return nil, m.result
		}
		delete(d.negative, slug)
	}
	if f, ok := d.inflight[slug]; ok {
		d.mu.Unlock()
		select {
		case <-f.done:
			return f.team, f.result
		case <-ctx.Done():
			return nil, resolveUnavailable
		}
	}
	// A flight for this slug may have landed between the caller's Lookup and
	// taking d.mu.
	if t, ok := s.Lookup(slug); ok {
		d.mu.Unlock()
		return t, resolveFound
	}
	if d.discovered >= d.cfg.MaxTeams {
		d.mu.Unlock()
		s.log.Warn("team discovery is at its cap; not asking the backend", "max_teams", d.cfg.MaxTeams)
		return nil, resolveFull
	}
	d.discovered++
	f := &flight{done: make(chan struct{})}
	d.inflight[slug] = f
	d.mu.Unlock()

	// The probe and the feed run on the server's context, not the request's:
	// every waiter shares this flight, and the first requester closing its tab
	// must not fail it for the others.
	f.team, f.result = d.register(s, slug)

	d.mu.Lock()
	delete(d.inflight, slug)
	switch f.result {
	case resolveFound:
	case resolveNotFound:
		d.discovered--
		if anonymous {
			d.remember(slug, resolveNotFound, d.cfg.NotFoundTTL)
		}
	default:
		d.discovered--
		if anonymous {
			d.remember(slug, resolveUnavailable, d.cfg.ErrorTTL)
		}
	}
	close(f.done)
	d.mu.Unlock()
	return f.team, f.result
}

// register probes, and only on a confirmed 200 creates the feed.
func (d *discovery) register(s *Server, slug string) (*Team, resolveResult) {
	res, err := d.cfg.Probe(d.ctx, slug)
	if err != nil {
		s.log.Warn("team discovery probe failed", "err", err)
		return nil, resolveUnavailable
	}
	if res != feed.ProbeFound {
		return nil, resolveNotFound
	}
	f := d.cfg.NewFeed(slug)
	// The feed reports a 404 on any of its reads. It can fire before addTeam
	// returns (the first snapshot read, or the stream's first connection) or
	// after the team is long gone (a straggler); registered is empty in the
	// first case, and unregister's identity check makes the second a no-op.
	// A 404 is terminal for the feed's stream, so one that landed before
	// registered was set is not repeated: reported remembers it, and the
	// takedown happens as soon as the team is registered.
	var (
		registered atomic.Pointer[Team]
		reported   atomic.Bool
	)
	if r, ok := f.(feed.NotFoundReporter); ok {
		r.OnNotFound(func() {
			reported.Store(true)
			if t := registered.Load(); t != nil {
				d.unregister(s, t, "feed read a 404")
			}
		})
	}
	t, err := s.addTeam(d.ctx, slug, slug, "", false, true, f)
	if err != nil {
		// The probe said yes and the feed's own snapshot read then failed —
		// a race with a visibility change, or the backend going away. Either
		// way nothing was registered; Run leaves no goroutine on error.
		s.log.Warn("discovered team failed to start", "err", err)
		return nil, resolveUnavailable
	}
	registered.Store(t)
	if reported.Load() {
		d.unregister(s, t, "feed read a 404")
		return nil, resolveNotFound
	}
	go d.recheck(s, t)
	s.log.Info("team discovered", "slug", slug, "feed", t.Feed.Name())
	return t, resolveFound
}

// recheck re-probes a discovered team until it is taken down or the server
// stops. It exists because the feed alone cannot see a visibility change: the
// backend authorizes GET /stream once, before the first byte, and never again
// on that connection, so a private team's healthy stream stays open until
// something else drops it.
//
// A probe that ERRORS is not a verdict — an outage must not take a board down
// — so only a clean 404 unregisters.
func (d *discovery) recheck(s *Server, t *Team) {
	interval := d.cfg.RecheckInterval
	for {
		wait := interval - time.Duration(rand.Int64N(int64(interval)/5+1))
		timer := time.NewTimer(wait)
		select {
		case <-t.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		res, err := d.cfg.Probe(t.ctx, t.Slug)
		if t.ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Warn("re-probing a discovered team failed; keeping it", "err", err)
			continue
		}
		if res != feed.ProbeFound {
			d.unregister(s, t, "re-probe answered 404")
			return
		}
	}
}

// unregister takes a discovered team down: it leaves the registry, its slot is
// freed, its 404 is remembered like any other, its feed and re-probe stop
// (cancel), and every open SSE stream on it ends (Hub.Close). The next request
// for the slug is an ordinary not-found.
//
// It refuses — and reports false — for a demo, for a pre-registered live team,
// and for a team that is no longer the one registered under its slug (already
// taken down, or replaced by a later discovery). That identity check is what
// makes it safe to call from several places at once, and exactly once
// effective: the slot count moves only when the registry entry is removed.
//
// Lock order is d.mu then s.mu, the same as discover's.
func (d *discovery) unregister(s *Server, t *Team, why string) bool {
	if t == nil || t.Demo || !t.Discovered {
		return false
	}
	d.mu.Lock()
	s.mu.Lock()
	if s.teams[t.Slug] != t {
		s.mu.Unlock()
		d.mu.Unlock()
		return false
	}
	delete(s.teams, t.Slug)
	for i, slug := range s.order {
		if slug == t.Slug {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	d.discovered--
	d.remember(t.Slug, resolveNotFound, d.cfg.NotFoundTTL)
	d.mu.Unlock()

	t.cancel()
	t.Hub.Close()
	// The slug is not logged, for the reason the public pages never show a
	// live team: logs travel further than the board does.
	s.log.Info("discovered team taken down", "reason", why)
	return true
}

// remember records a negative answer. Called with d.mu held.
func (d *discovery) remember(slug string, result resolveResult, ttl time.Duration) {
	now := d.cfg.Now()
	if len(d.negative) >= d.cfg.MaxRemembered {
		for k, m := range d.negative {
			if !now.Before(m.until) {
				delete(d.negative, k)
			}
		}
		// Still full of unexpired junk: forget it all rather than grow. The
		// worst case is one more probe per slug, which is what the cache was
		// saving, not a correctness problem.
		if len(d.negative) >= d.cfg.MaxRemembered {
			d.negative = map[string]remembered{}
		}
	}
	d.negative[slug] = remembered{until: now.Add(ttl), result: result}
}
