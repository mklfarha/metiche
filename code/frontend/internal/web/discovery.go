package web

import (
	"context"
	"regexp"
	"sync"
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
// team this board may not read (app/authz, deliberately indistinguishable), so
// in practice discovery finds the teams the board's own credential can read —
// with no METICHE_BOARD_TOKEN, public teams only.
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
	// Now is the clock, for tests. Default time.Now.
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

// resolveTeam finds the team for a request, registering it on demand if
// discovery is on. A registered slug — demo or live — always wins and is never
// looked up on the backend, which is what reserves demo slugs.
func (s *Server) resolveTeam(ctx context.Context, slug string) (*Team, resolveResult) {
	if t, ok := s.Lookup(slug); ok {
		return t, resolveFound
	}
	if s.disc == nil || !ValidSlug(slug) {
		return nil, resolveNotFound
	}
	return s.disc.discover(ctx, s, slug)
}

func (d *discovery) discover(ctx context.Context, s *Server, slug string) (*Team, resolveResult) {
	d.mu.Lock()
	now := d.cfg.Now()
	if m, ok := d.negative[slug]; ok {
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
		d.remember(slug, resolveNotFound, d.cfg.NotFoundTTL)
	default:
		d.discovered--
		d.remember(slug, resolveUnavailable, d.cfg.ErrorTTL)
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
	t, err := s.addTeam(d.ctx, slug, slug, "", false, true, d.cfg.NewFeed(slug))
	if err != nil {
		// The probe said yes and the feed's own snapshot read then failed —
		// a race with a visibility change, or the backend going away. Either
		// way nothing was registered; Run leaves no goroutine on error.
		s.log.Warn("discovered team failed to start", "err", err)
		return nil, resolveUnavailable
	}
	s.log.Info("team discovered", "slug", slug, "feed", t.Feed.Name())
	return t, resolveFound
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
