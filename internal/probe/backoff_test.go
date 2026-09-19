package probe

import (
	"testing"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/config"
)

func testBackoff(t *testing.T) (*Backoff, *clock) {
	t.Helper()
	cfg := config.Default().Probe.Backoff
	b := NewBackoff(cfg)
	c := &clock{now: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)}
	b.clock = c.Now
	b.rand = func() float64 { return 0.5 } // midpoint: jitter cancels out
	return b, c
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// The ladder must climb 15s, 30s, 60s, 120s, 240s, then hold at the 300s cap.
func TestBackoffLadder(t *testing.T) {
	b, c := testBackoff(t)
	const id = "mac:a8032ab1c2d3"

	// The first two failures are tolerated without opening: transient wifi loss on a
	// home network is routine and should not immediately halve the sample rate.
	for i := range 2 {
		b.Record(id, Fail(ReasonTimeout, 0))
		if ok, _ := b.Allow(id); !ok {
			t.Fatalf("backoff opened after %d failures, want it to tolerate 2", i+1)
		}
	}

	want := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second,
		120 * time.Second, 240 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, w := range want {
		b.Record(id, Fail(ReasonTimeout, 0))
		ok, remaining := b.Allow(id)
		if ok {
			t.Fatalf("step %d: probe allowed, want it blocked", i)
		}
		if remaining != w {
			t.Errorf("step %d: remaining = %s, want %s", i, remaining, w)
		}
		c.Advance(remaining)
	}
}

// Auth failures use a separate, longer ladder, because retrying a wrong password every
// thirty seconds will never start working.
func TestBackoffAuthLadderIsLonger(t *testing.T) {
	b, _ := testBackoff(t)
	const id = "mac:a8032ab1c2d3"

	for range 3 {
		b.Record(id, Fail(ReasonAuth, 401))
	}
	_, remaining := b.Allow(id)
	if remaining != time.Minute {
		t.Errorf("first auth backoff = %s, want 1m", remaining)
	}
}

// in_flight_limit is our fault, not the device's. Counting it would back off healthy
// devices during a load spike and make a transient overload self-sustaining.
func TestBackoffIgnoresOurOwnFaults(t *testing.T) {
	b, _ := testBackoff(t)
	const id = "mac:a8032ab1c2d3"

	for range 10 {
		b.Record(id, Fail(ReasonInFlightLimit, 0))
	}
	if ok, _ := b.Allow(id); !ok {
		t.Error("in_flight_limit must not open the breaker")
	}
	if got := b.Failures(id); got != 0 {
		t.Errorf("Failures = %d, want 0", got)
	}
}

func TestBackoffSuccessResets(t *testing.T) {
	b, _ := testBackoff(t)
	const id = "mac:a8032ab1c2d3"

	for range 5 {
		b.Record(id, Fail(ReasonTimeout, 0))
	}
	if ok, _ := b.Allow(id); ok {
		t.Fatal("expected the breaker to be open")
	}

	b.Record(id, Success(200))
	if ok, _ := b.Allow(id); !ok {
		t.Error("a success must clear the backoff immediately")
	}
	if got := b.Failures(id); got != 0 {
		t.Errorf("Failures = %d after success, want 0", got)
	}
}

// A device re-announcing itself on mDNS is strong evidence it is back, so it should not
// have to sit out the rest of a fifteen-minute timer.
func TestBackoffResetOnRediscovery(t *testing.T) {
	b, _ := testBackoff(t)
	const id = "mac:a8032ab1c2d3"

	for range 8 {
		b.Record(id, Fail(ReasonTimeout, 0))
	}
	if ok, _ := b.Allow(id); ok {
		t.Fatal("expected the breaker to be open")
	}

	b.Reset(id)
	if ok, _ := b.Allow(id); !ok {
		t.Error("rediscovery must clear the backoff")
	}
}

// When the timer expires exactly one attempt is admitted, and a failure re-opens.
func TestBackoffHalfOpen(t *testing.T) {
	b, c := testBackoff(t)
	const id = "mac:a8032ab1c2d3"

	for range 3 {
		b.Record(id, Fail(ReasonTimeout, 0))
	}
	_, remaining := b.Allow(id)
	c.Advance(remaining)

	if ok, _ := b.Allow(id); !ok {
		t.Fatal("expected a half-open probe to be admitted")
	}
	b.Record(id, Fail(ReasonTimeout, 0))
	ok, next := b.Allow(id)
	if ok {
		t.Error("a failed half-open probe must re-open the breaker")
	}
	if next != 30*time.Second {
		t.Errorf("next delay = %s, want the ladder to advance to 30s", next)
	}
}

func TestBackoffOpenCount(t *testing.T) {
	b, _ := testBackoff(t)
	for _, id := range []string{"a", "b", "c"} {
		for range 3 {
			b.Record(id, Fail(ReasonTimeout, 0))
		}
	}
	b.Record("d", Fail(ReasonTimeout, 0)) // below the threshold
	if got := b.Open(); got != 3 {
		t.Errorf("Open = %d, want 3", got)
	}
}

func TestReasonClassification(t *testing.T) {
	faults := []Reason{ReasonTimeout, ReasonConnRefused, ReasonNoRoute, ReasonAuth,
		ReasonHTTPStatus, ReasonDecode, ReasonMACMismatch, ReasonNoAddress, ReasonNotConnected}
	for _, r := range faults {
		if !r.IsDeviceFault() {
			t.Errorf("%q should count as a device fault", r)
		}
	}
	for _, r := range []Reason{ReasonNone, ReasonInFlightLimit, ReasonBackoff} {
		if r.IsDeviceFault() {
			t.Errorf("%q must not count as a device fault", r)
		}
	}
}
