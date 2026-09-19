package probe

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// Limiter gates every probe through backoff, a concurrency semaphore, single-flight
// de-duplication and a very short result cache, in that order.
type Limiter struct {
	cfg     config.Probe
	sem     *semaphore.Weighted
	group   singleflight.Group
	backoff *Backoff
	clock   func() time.Time

	mu    sync.Mutex
	cache map[string]cachedResult

	inFlight atomic64
	shared   atomic64
	skipped  map[Reason]*atomic64
}

type cachedResult struct {
	metrics []prometheus.Metric
	result  Result
	at      time.Time
}

// NewLimiter builds the gate.
func NewLimiter(cfg config.Probe) *Limiter {
	return &Limiter{
		cfg:     cfg,
		sem:     semaphore.NewWeighted(int64(cfg.MaxConcurrent)),
		backoff: NewBackoff(cfg.Backoff),
		clock:   time.Now,
		cache:   map[string]cachedResult{},
		skipped: map[Reason]*atomic64{},
	}
}

// Backoff exposes the tracker so discovery can clear a device on re-announcement.
func (l *Limiter) Backoff() *Backoff { return l.backoff }

// InFlight reports probes currently executing.
func (l *Limiter) InFlight() int64 { return l.inFlight.Load() }

// Shared reports probes served by de-duplication rather than a device request.
func (l *Limiter) Shared() int64 { return l.shared.Load() }

// SkippedByReason reports every short-circuit count, keyed by reason.
func (l *Limiter) SkippedByReason() map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]uint64, len(l.skipped))
	for r, c := range l.skipped {
		if n := c.Load(); n > 0 {
			out[string(r)] = uint64(n)
		}
	}
	return out
}

// Skipped reports probes short-circuited, by reason.
func (l *Limiter) Skipped(r Reason) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.skipped[r]; ok {
		return c.Load()
	}
	return 0
}

// Do runs fn under every gate, returning the metrics and outcome.
//
// The ordering is deliberate. Backoff comes first because it must cost nothing: a dead
// device should not consume a concurrency token merely to be told it is still dead.
func (l *Limiter) Do(
	ctx context.Context,
	dev registry.Device,
	fn func(context.Context) ([]prometheus.Metric, Result),
) ([]prometheus.Metric, Result, time.Duration) {
	if allowed, remaining := l.backoff.Allow(dev.ID); !allowed {
		l.countSkip(ReasonBackoff)
		return nil, Fail(ReasonBackoff, 0), remaining
	}

	if cached, ok := l.lookup(dev.ID); ok {
		l.shared.Add(1)
		return cached.metrics, cached.result, 0
	}

	type outcome struct {
		metrics []prometheus.Metric
		result  Result
	}

	v, _, shared := l.group.Do(dev.ID, func() (any, error) {
		// Acquire with the probe's own context rather than rejecting immediately, so a
		// queued probe waits for its turn and only fails if the whole scrape budget
		// expires while waiting.
		if err := l.sem.Acquire(ctx, 1); err != nil {
			l.countSkip(ReasonInFlightLimit)
			return outcome{result: Fail(ReasonInFlightLimit, 0)}, nil
		}
		defer l.sem.Release(1)

		l.inFlight.Add(1)
		defer l.inFlight.Add(-1)

		metrics, res := fn(ctx)
		l.backoff.Record(dev.ID, res)
		l.store(dev.ID, metrics, res)
		return outcome{metrics: metrics, result: res}, nil
	})

	if shared {
		l.shared.Add(1)
	}
	out, _ := v.(outcome)
	return out.metrics, out.result, 0
}

func (l *Limiter) lookup(id string) (cachedResult, bool) {
	if l.cfg.CacheTTL <= 0 {
		return cachedResult{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	c, ok := l.cache[id]
	if !ok || l.clock().Sub(c.at) > l.cfg.CacheTTL {
		return cachedResult{}, false
	}
	return c, true
}

func (l *Limiter) store(id string, metrics []prometheus.Metric, res Result) {
	if l.cfg.CacheTTL <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cache[id] = cachedResult{metrics: metrics, result: res, at: l.clock()}
}

// Forget drops a device's cached result and backoff, for use when its address changes.
func (l *Limiter) Forget(id string) {
	l.mu.Lock()
	delete(l.cache, id)
	l.mu.Unlock()
	l.backoff.Reset(id)
}

func (l *Limiter) countSkip(r Reason) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.skipped[r]
	if !ok {
		c = &atomic64{}
		l.skipped[r] = c
	}
	c.Add(1)
}
