package mcp

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metiche has no login, and that is a product decision rather than an
// oversight: a coordination tool nobody can join in sixty seconds does not get
// joined. The join code is the credential, and creating a team is open.
//
// Open is not the same as unbounded. Two tools answer without a token —
// create_team, which mints a team, and join_team, which is the only place a
// join code can be guessed at — and both are rate limited per client IP.
// Abuse is bounded here and by plan limits, not by an account.
//
// The limiter is a fixed-window counter held in memory. Deliberately not a
// shared one: metiche must run with no third-party credential, so there is no
// Redis to lean on, and a per-pod window is the right shape for a v1 running
// one pod. When it is not one pod any more the budget is per pod, which is a
// number to raise, not a design to replace.

const (
	rateWindow = time.Hour

	// defaultCreateTeamPerHour is small on purpose. Creating a team is a
	// once-per-hackathon act; anyone doing it six times an hour from one
	// address is not setting up a team.
	defaultCreateTeamPerHour = 5

	// defaultJoinTeamPerHour bounds guessing at join codes. A real team of ten
	// people with five agents each joins fifty times, once, and then re-joins
	// only on restarts.
	defaultJoinTeamPerHour = 60
)

// RateLimiter is a fixed-window per-key counter.
type RateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]*rateEntry
	// lastSweep bounds the map without a goroutine: expired keys are dropped
	// on the next call after a window has passed. A background janitor for
	// something this small is a goroutine to leak, not a feature.
	lastSweep time.Time
}

type rateEntry struct {
	count int
	reset time.Time
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window, entries: map[string]*rateEntry{}, lastSweep: time.Now()}
}

// Allow records one use of key and reports whether it was within budget,
// along with how long until the window resets.
//
// An empty key is always allowed: it means the call did not arrive over HTTP
// (an in-process caller, or a test driving a handler directly), and there is
// no client to limit.
func (rl *RateLimiter) Allow(key string) (bool, time.Duration) {
	if rl == nil || rl.limit <= 0 || key == "" {
		return true, 0
	}
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	if now.Sub(rl.lastSweep) > rl.window {
		for k, e := range rl.entries {
			if now.After(e.reset) {
				delete(rl.entries, k)
			}
		}
		rl.lastSweep = now
	}

	e, ok := rl.entries[key]
	if !ok || now.After(e.reset) {
		e = &rateEntry{reset: now.Add(rl.window)}
		rl.entries[key] = e
	}
	if e.count >= rl.limit {
		return false, time.Until(e.reset).Round(time.Second)
	}
	e.count++
	return true, 0
}

// envInt reads a positive integer from the environment, falling back to a
// default. Budgets are deploy-time settings, not code.
func envInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// ─────────────────────────────────────────────
// Client address
// ─────────────────────────────────────────────

type clientIPCtxKey struct{}

// WithClientIP puts the caller's address on the context so an unauthenticated
// tool can be rate limited by it. Exported so a test can exercise the limiter
// without an HTTP server.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPCtxKey{}, ip)
}

// ClientIPFromContext returns the caller's address, or "" when the call did
// not arrive over HTTP.
func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPCtxKey{}).(string)
	return ip
}

// clientIP works out who is calling.
//
// X-Forwarded-For is honoured ONLY when METICHE_TRUST_PROXY is set, and this
// is the reason: the header is caller-supplied, so trusting it unconditionally
// turns a per-IP rate limit into no rate limit at all — an attacker sends a
// different X-Forwarded-For on every request and each one gets its own budget.
// Behind an ingress that overwrites the header, trusting it is correct and
// necessary, because otherwise every request appears to come from the ingress
// and the whole internet shares one bucket. Neither default is safe in the
// other's deployment, so it is a switch, and the safe-by-default position is
// off.
func clientIP(r *http.Request) string {
	if os.Getenv("METICHE_TRUST_PROXY") != "" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
