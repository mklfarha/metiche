package mcp

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// MetricLockHold is the name PLAN.md gives the one number that protects the
// write-path headroom. The tripwire is p99 > 25ms: past that, shard the
// sequence per project rather than reaching for another datastore.
//
// It measures the lock HELD — from the moment SELECT ... FOR UPDATE returns to
// the moment COMMIT does — and deliberately not the wait to acquire it. The
// two answer different questions and mixing them makes the metric useless:
// hold is what WE control and what the 25ms tripwire is about, while wait is a
// function of how many writers happen to be queued behind us, so under load
// the queue would drown out exactly the signal the number exists to carry.
const MetricLockHold = "metiche_team_lock_hold_ms"

// MetricLockWait is the other half, reported alongside. A rising wait with a
// flat hold is contention — more writers than the team's single serialization
// point can absorb — and points at sharding the sequence. A rising hold is
// something slow having been added inside the lock, and points at whoever
// added it.
const MetricLockWait = "metiche_team_lock_wait_ms"

// LockHoldWarnMS is the per-call threshold that earns a log line. It is
// deliberately the same 25ms as the p99 tripwire, so the logs point at the
// offending call before the aggregate crosses it.
const LockHoldWarnMS = 25

// lockHoldBucketsMS are the upper bounds, in milliseconds, of a fixed-bucket
// histogram.
//
// Fixed buckets rather than a reservoir on purpose: the whole point of this
// metric is that measuring it must cost less than the thing it measures. A
// bucket increment is one comparison loop over eleven ints under a mutex held
// for nanoseconds, and it cannot grow without bound however long the process
// runs.
var lockHoldBucketsMS = []float64{0.5, 1, 2, 3, 5, 8, 12, 16, 20, 25, 50, 100, 250, 500, 1000}

// Histogram is a tiny fixed-bucket histogram.
//
// No Prometheus client: metiche must run with no third-party credential and as
// few moving parts as possible, and a dependency-free JSON endpoint is enough
// to answer the only question anyone will ask of it — "is the lock hold still
// under 25ms?". Swapping in a real collector later means changing Observe and
// nothing else.
type Histogram struct {
	name    string
	mu      sync.Mutex
	counts  []uint64 // one per bucket, plus a final +Inf bucket
	total   uint64
	sumMS   float64
	maxMS   float64
	lastObs time.Time
}

func NewHistogram(name string) *Histogram {
	return &Histogram{name: name, counts: make([]uint64, len(lockHoldBucketsMS)+1)}
}

// Observe records one duration.
func (h *Histogram) Observe(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	idx := sort.SearchFloat64s(lockHoldBucketsMS, ms)
	// SearchFloat64s returns the first index whose bound is >= ms, which is the
	// bucket that contains it; len(bounds) means it exceeded every bound.
	h.mu.Lock()
	h.counts[idx]++
	h.total++
	h.sumMS += ms
	if ms > h.maxMS {
		h.maxMS = ms
	}
	h.lastObs = time.Now().UTC()
	h.mu.Unlock()
}

// HistogramSnapshot is a point-in-time read, safe to serialize.
type HistogramSnapshot struct {
	Name string `json:"name"`
	// Buckets maps an upper bound in ms ("5", "+Inf") to a cumulative count,
	// which is the shape a Prometheus scraper would want if one is ever added.
	Buckets  map[string]uint64 `json:"buckets"`
	Count    uint64            `json:"count"`
	SumMS    float64           `json:"sum_ms"`
	MaxMS    float64           `json:"max_ms"`
	MeanMS   float64           `json:"mean_ms"`
	P50MS    float64           `json:"p50_ms"`
	P95MS    float64           `json:"p95_ms"`
	P99MS    float64           `json:"p99_ms"`
	LastObs  string            `json:"last_observed_at,omitempty"`
	Tripwire string            `json:"tripwire"`
}

func (h *Histogram) Snapshot() HistogramSnapshot {
	h.mu.Lock()
	counts := append([]uint64(nil), h.counts...)
	total, sum, max, last := h.total, h.sumMS, h.maxMS, h.lastObs
	h.mu.Unlock()

	s := HistogramSnapshot{
		Name:     h.name,
		Buckets:  map[string]uint64{},
		Count:    total,
		SumMS:    round2(sum),
		MaxMS:    round2(max),
		Tripwire: "p99_ms > 25 → shard the sequence per project",
	}
	if !last.IsZero() {
		s.LastObs = last.Format(time.RFC3339Nano)
	}
	var cumulative uint64
	for i, bound := range lockHoldBucketsMS {
		cumulative += counts[i]
		s.Buckets[formatBound(bound)] = cumulative
	}
	cumulative += counts[len(counts)-1]
	s.Buckets["+Inf"] = cumulative

	if total > 0 {
		s.MeanMS = round2(sum / float64(total))
		s.P50MS = quantile(counts, total, 0.50)
		s.P95MS = quantile(counts, total, 0.95)
		s.P99MS = quantile(counts, total, 0.99)
	}
	return s
}

// quantile reports the upper bound of the bucket the requested quantile falls
// in. It is an over-estimate by construction (never an under-estimate), which
// is the right direction to be wrong in for a tripwire: it trips early, not
// late. A sample past the last bound reports +Inf as the max observed value.
func quantile(counts []uint64, total uint64, q float64) float64 {
	target := uint64(float64(total) * q)
	if target == 0 {
		target = 1
	}
	var cumulative uint64
	for i, bound := range lockHoldBucketsMS {
		cumulative += counts[i]
		if cumulative >= target {
			return bound
		}
	}
	return lockHoldBucketsMS[len(lockHoldBucketsMS)-1] * 2
}

func formatBound(b float64) string {
	switch {
	case b >= 1000:
		return "1000"
	case b == float64(int64(b)):
		return itoa(int64(b))
	}
	return itoa(int64(b))
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// metricsHandler serves the lock-hold histogram as JSON.
//
// Mounted outside the /v1/mcp prefix so it cannot shadow a protocol path, and
// it exposes nothing but timings — no team, session or agent identifiers — so
// it is safe on a public endpoint.
func (h *Handler) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"lock_hold": h.LockHoldStats(),
		"lock_wait": h.lockWait.Snapshot(),
	})
}
