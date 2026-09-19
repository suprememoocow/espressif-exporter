package probe

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

func testLimiter(cfg config.Probe) *Limiter {
	return NewLimiter(cfg)
}

var testDev = registry.Device{ID: "mac:a8032ab1c2d3"}

// A backed-off device must cost nothing: no device request, and no concurrency token
// taken merely to be told it is still dead.
func TestLimiterBackoffPerformsNoIO(t *testing.T) {
	cfg := config.Default().Probe
	cfg.CacheTTL = 0
	l := testLimiter(cfg)

	var calls atomic.Int32
	fn := func(context.Context) ([]prometheus.Metric, Result) {
		calls.Add(1)
		return nil, Fail(ReasonTimeout, 0)
	}

	for range cfg.Backoff.FailuresBeforeOpen {
		l.Do(context.Background(), testDev, fn)
	}
	before := calls.Load()

	_, res, remaining := l.Do(context.Background(), testDev, fn)
	if calls.Load() != before {
		t.Error("the probe function ran while the device was backed off")
	}
	if res.Reason != ReasonBackoff {
		t.Errorf("Reason = %q, want backoff", res.Reason)
	}
	if remaining <= 0 {
		t.Error("expected a positive remaining backoff so probe_backoff_remaining_seconds is useful")
	}
	if l.Skipped(ReasonBackoff) != 1 {
		t.Errorf("Skipped(backoff) = %d, want 1", l.Skipped(ReasonBackoff))
	}
}

// A second Prometheus, an HA pair or a manual curl must not double-hit the device.
func TestLimiterDeduplicatesConcurrentProbes(t *testing.T) {
	cfg := config.Default().Probe
	l := testLimiter(cfg)

	var calls atomic.Int32
	release := make(chan struct{})
	fn := func(context.Context) ([]prometheus.Metric, Result) {
		calls.Add(1)
		<-release
		return nil, Success(200)
	}

	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Do(context.Background(), testDev, fn)
		}()
	}

	// Give the goroutines time to coalesce on the single-flight key.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("the device was contacted %d times, want exactly 1", got)
	}
}

// Back-to-back scrapes within the TTL are served from cache; past it, the device is
// contacted again. The TTL is deliberately tiny so a faster scraper is never lied to.
func TestLimiterResultCacheExpires(t *testing.T) {
	cfg := config.Default().Probe
	cfg.CacheTTL = 2 * time.Second
	l := testLimiter(cfg)

	now := time.Now()
	l.clock = func() time.Time { return now }

	var calls atomic.Int32
	fn := func(context.Context) ([]prometheus.Metric, Result) {
		calls.Add(1)
		return nil, Success(200)
	}

	l.Do(context.Background(), testDev, fn)
	l.Do(context.Background(), testDev, fn)
	if got := calls.Load(); got != 1 {
		t.Fatalf("the device was contacted %d times within the cache TTL, want 1", got)
	}

	now = now.Add(3 * time.Second)
	l.Do(context.Background(), testDev, fn)
	if got := calls.Load(); got != 2 {
		t.Errorf("the device was contacted %d times after the TTL expired, want 2", got)
	}
}

// A probe whose budget expires while queued is our fault, so it must be reported as
// in_flight_limit and must not push the device towards backoff.
func TestLimiterQueueTimeoutIsNotADeviceFault(t *testing.T) {
	cfg := config.Default().Probe
	cfg.MaxConcurrent = 1
	cfg.CacheTTL = 0
	l := testLimiter(cfg)

	hold := make(chan struct{})
	started := make(chan struct{})
	go func() {
		l.Do(context.Background(), registry.Device{ID: "other"},
			func(context.Context) ([]prometheus.Metric, Result) {
				close(started)
				<-hold
				return nil, Success(200)
			})
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, res, _ := l.Do(ctx, testDev, func(context.Context) ([]prometheus.Metric, Result) {
		t.Error("the probe function must not run when the semaphore was never acquired")
		return nil, Success(200)
	})
	close(hold)

	if res.Reason != ReasonInFlightLimit {
		t.Errorf("Reason = %q, want in_flight_limit", res.Reason)
	}
	if l.Backoff().Failures(testDev.ID) != 0 {
		t.Error("a queue timeout must not count against the device")
	}
}

func TestLimiterForgetClearsCacheAndBackoff(t *testing.T) {
	cfg := config.Default().Probe
	l := testLimiter(cfg)

	var calls atomic.Int32
	fn := func(context.Context) ([]prometheus.Metric, Result) {
		calls.Add(1)
		return nil, Success(200)
	}
	l.Do(context.Background(), testDev, fn)

	// Forget is what an address change triggers, so the next probe must really run.
	l.Forget(testDev.ID)
	l.Do(context.Background(), testDev, fn)
	if got := calls.Load(); got != 2 {
		t.Errorf("the device was contacted %d times, want 2 after Forget", got)
	}
}
