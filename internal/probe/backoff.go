package probe

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/config"
)

// Backoff tracks per-device failure state.
//
// Without it a single dead device occupies a concurrency slot for the full probe budget
// on every scrape, forever; ten dead devices would consume a third of the budget doing
// nothing. With it, a permanently dead device is contacted once every five minutes and
// still produces a probe_success 0 sample on every scrape — the observation frequency is
// decoupled from the probe frequency, which is the whole point.
type Backoff struct {
	cfg   config.Backoff
	clock func() time.Time
	rand  func() float64

	mu    sync.Mutex
	state map[string]*deviceBackoff
}

type deviceBackoff struct {
	failures    int
	nextAttempt time.Time
	lastReason  Reason
}

// NewBackoff builds the tracker.
func NewBackoff(cfg config.Backoff) *Backoff {
	return &Backoff{
		cfg:   cfg,
		clock: time.Now,
		rand:  rand.Float64,
		state: map[string]*deviceBackoff{},
	}
}

// Allow reports whether a probe may proceed. When it may not, the caller must
// short-circuit without any network I/O and without taking a concurrency token, and the
// returned duration is how long remains.
func (b *Backoff) Allow(deviceID string) (allowed bool, remaining time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	st, ok := b.state[deviceID]
	if !ok || st.nextAttempt.IsZero() {
		return true, 0
	}
	now := b.clock()
	if !now.Before(st.nextAttempt) {
		// Half-open: exactly one attempt is admitted. Singleflight upstream prevents a
		// thundering herd if several scrapers arrive at the same moment.
		return true, 0
	}
	return false, st.nextAttempt.Sub(now)
}

// Record folds a probe outcome into the device's state.
func (b *Backoff) Record(deviceID string, res Result) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if res.Success {
		delete(b.state, deviceID)
		return
	}
	if !res.Reason.IsDeviceFault() {
		return
	}

	st, ok := b.state[deviceID]
	if !ok {
		st = &deviceBackoff{}
		b.state[deviceID] = st
	}
	st.failures++
	st.lastReason = res.Reason

	if st.failures < b.cfg.FailuresBeforeOpen {
		st.nextAttempt = time.Time{}
		return
	}
	st.nextAttempt = b.clock().Add(b.delay(st.failures, res.Reason))
}

// Reset clears a device's backoff.
//
// Discovery calls this when a device re-announces itself on mDNS: fresh presence is
// strong evidence the device is back, and making it sit out the remainder of a
// fifteen-minute timer after that would be poor behaviour.
func (b *Backoff) Reset(deviceID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, deviceID)
}

// Failures returns the consecutive failure count, for per-device metrics.
func (b *Backoff) Failures(deviceID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st, ok := b.state[deviceID]; ok {
		return st.failures
	}
	return 0
}

// Open counts devices currently in backoff, for self-metrics.
func (b *Backoff) Open() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	n := 0
	for _, st := range b.state {
		if !st.nextAttempt.IsZero() && now.Before(st.nextAttempt) {
			n++
		}
	}
	return n
}

// delay computes the next wait, jittered.
//
// Auth failures use a separate, longer ladder: retrying a wrong password every thirty
// seconds will never start working, and some Shelly firmwares rate-limit or temporarily
// lock out after repeated failures, so backing off hard is both polite and protective.
func (b *Backoff) delay(failures int, reason Reason) time.Duration {
	initial, maxDelay := b.cfg.Initial, b.cfg.Max
	if reason.IsAuth() {
		initial, maxDelay = b.cfg.AuthInitial, b.cfg.AuthMax
	}

	steps := failures - b.cfg.FailuresBeforeOpen
	d := float64(initial) * math.Pow(b.cfg.Factor, float64(steps))
	if d > float64(maxDelay) || math.IsInf(d, 0) {
		d = float64(maxDelay)
	}

	// Equal jitter rather than full jitter: full jitter can return a near-zero delay at
	// the top of the ladder, which defeats the purpose of having climbed it.
	if j := b.cfg.Jitter; j > 0 {
		d *= 1 - j + 2*j*b.rand()
	}
	return time.Duration(d)
}
