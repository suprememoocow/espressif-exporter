package esphome

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richard87/esphome-apiclient/pb"
	"google.golang.org/protobuf/proto"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeConn stands in for a device, so the connection state machine is testable without
// hardware — which matters most for the invariants that only show up under failure.
type fakeConn struct {
	mu sync.Mutex

	info     *pb.DeviceInfoResponse
	entities []proto.Message
	handler  func(proto.Message)
	done     chan struct{}
	closed   atomic.Bool

	listErr error
	pingErr error

	listCalls atomic.Int32
}

func newFakeConn(entities ...proto.Message) *fakeConn {
	return &fakeConn{
		info: &pb.DeviceInfoResponse{
			Name: "bedroom", MacAddress: "a4:cf:12:9b:3e:70",
			EsphomeVersion: "2026.3.2", CompilationTime: "t1",
		},
		entities: entities,
		done:     make(chan struct{}),
	}
}

func (f *fakeConn) DeviceInfo() (*pb.DeviceInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, nil
}

func (f *fakeConn) ListEntities() ([]proto.Message, error) {
	f.listCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.entities, nil
}

func (f *fakeConn) SubscribeStates(h func(proto.Message)) (func(), error) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
	return func() {}, nil
}

func (f *fakeConn) On(uint32, func(proto.Message)) func()   { return func() {} }
func (f *fakeConn) SendMessage(proto.Message, uint32) error { return nil }
func (f *fakeConn) Connected() bool                         { return !f.closed.Load() }
func (f *fakeConn) Done() <-chan struct{}                   { return f.done }

func (f *fakeConn) Ping() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

func (f *fakeConn) Close() error {
	f.closed.Store(true)
	return nil
}

func (f *fakeConn) push(msg proto.Message) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h(msg)
	}
}

func sensorEntity(key uint32, objectID, unit string, stateClass pb.SensorStateClass) *pb.ListEntitiesSensorResponse {
	return &pb.ListEntitiesSensorResponse{
		Key: key, ObjectId: objectID, Name: objectID,
		UnitOfMeasurement: unit, StateClass: stateClass,
	}
}

func testDevice(t *testing.T, dial Dialer) *device {
	t.Helper()
	return testDeviceWithLiveness(t, dial, nil)
}

func testDeviceWithLiveness(t *testing.T, dial Dialer, liveness LivenessReporter) *device {
	t.Helper()
	cfg := config.Default().ESPHome
	cfg.PingInterval = 20 * time.Millisecond
	cfg.RefreshInterval = 20 * time.Millisecond
	cfg.ConnectBudget = 2 * time.Second
	return newDevice("mac:a4cf129b3e70", "bedroom", netip.MustParseAddr("192.168.1.10"),
		6053, 1, cfg, dial, liveness, discardLogger())
}

// fakeLiveness collects what the device tells the registry.
type fakeLiveness struct {
	mu      sync.Mutex
	results []registry.ScrapeResult
}

func (f *fakeLiveness) ReportScrape(res registry.ScrapeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, res)
}

func (f *fakeLiveness) snapshot() []registry.ScrapeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.results)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDeviceConnectsAndCachesStates(t *testing.T) {
	conn := newFakeConn(sensorEntity(1, "temperature", "°C", pb.SensorStateClass_STATE_CLASS_MEASUREMENT))
	d := testDevice(t, func(context.Context, string, DialOptions) (Conn, error) { return conn, nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx)

	waitFor(t, "connection", func() bool { return d.cache.Snapshot().Connected })

	conn.push(&pb.SensorStateResponse{Key: 1, State: 21.4})
	waitFor(t, "state", func() bool { return len(d.cache.Snapshot().States) == 1 })

	snap := d.cache.Snapshot()
	if got := snap.States[1].Value; got < 21.3 || got > 21.5 {
		t.Errorf("cached value = %v, want ~21.4", got)
	}
	if snap.States[1].Missing {
		t.Error("a valid reading should not be marked missing")
	}
}

// A NaN and an explicit missing_state both mean "no reading". Exporting 0 would be a
// plausible-looking lie; exporting NaN would silently poison avg, sum and rate.
func TestMissingAndNaNAreBothTreatedAsNoReading(t *testing.T) {
	conn := newFakeConn(sensorEntity(1, "temperature", "°C", pb.SensorStateClass_STATE_CLASS_MEASUREMENT))
	d := testDevice(t, func(context.Context, string, DialOptions) (Conn, error) { return conn, nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx)
	waitFor(t, "connection", func() bool { return d.cache.Snapshot().Connected })

	conn.push(&pb.SensorStateResponse{
		Key: 1, State: 0, MissingState: true})
	waitFor(t, "missing state", func() bool {
		s, ok := d.cache.Snapshot().States[1]
		return ok && s.Missing
	})

	conn.push(&pb.SensorStateResponse{Key: 1, State: float32(math.NaN())})
	waitFor(t, "NaN treated as missing", func() bool {
		s, ok := d.cache.Snapshot().States[1]
		return ok && s.Missing
	})
}

// Merging entity maps would leave an entity deleted by a firmware change in the cache
// forever, still exported with whatever value it last had.
func TestEntitySwapDropsRemovedEntitiesButCarriesSurvivors(t *testing.T) {
	cache := newDeviceCache(16)

	cache.swapEntities(map[uint32]*entityRecord{
		1: {Key: 1, ObjectID: "kept"},
		2: {Key: 2, ObjectID: "removed"},
	}, false)
	cache.putState(1, stateRecord{Value: 21.4, UpdatedAt: time.Now()})
	cache.putState(2, stateRecord{Value: 99, UpdatedAt: time.Now()})

	// An OTA removes entity 2. The entity key is a hash of the object ID, so 1 survives.
	cache.swapEntities(map[uint32]*entityRecord{1: {Key: 1, ObjectID: "kept"}}, false)

	snap := cache.Snapshot()
	if _, ok := snap.Entities[2]; ok {
		t.Error("a removed entity must not survive the swap")
	}
	if _, ok := snap.States[2]; ok {
		t.Error("a removed entity's value must not linger and keep being exported")
	}
	if got := snap.States[1].Value; got != 21.4 {
		t.Errorf("surviving entity value = %v, want it carried forward so a firmware "+
			"change does not blank every gauge", got)
	}
	if snap.EntityGeneration != 2 {
		t.Errorf("generation = %d, want 2", snap.EntityGeneration)
	}
}

// ESPHome sends its full state dump immediately after SubscribeStates, so a state that
// lands mid-swap must be reconciled rather than discarded: for a total_daily_energy,
// discarding it means a blank graph until the next hourly update.
func TestOrphanStatesAreReconciledAtTheNextSwap(t *testing.T) {
	cache := newDeviceCache(16)

	cache.putState(7, stateRecord{Value: 42, UpdatedAt: time.Now()})
	if len(cache.Snapshot().States) != 0 {
		t.Fatal("a state for an unknown key should not appear as a reading yet")
	}

	cache.swapEntities(map[uint32]*entityRecord{7: {Key: 7, ObjectID: "energy"}}, false)
	if got := cache.Snapshot().States[7].Value; got != 42 {
		t.Errorf("orphan value = %v, want 42 reconciled into the new entity map", got)
	}
}

func TestOrphanBufferIsBounded(t *testing.T) {
	cache := newDeviceCache(4)
	for i := range uint32(50) {
		cache.putState(i, stateRecord{Value: float64(i), UpdatedAt: time.Now()})
	}
	cache.mu.RLock()
	n := len(cache.orphans)
	cache.mu.RUnlock()
	if n > 4 {
		t.Errorf("orphan buffer holds %d, want it capped at 4", n)
	}
}

// With a key configured we try Noise first and fall back to plaintext exactly once,
// because the mDNS api_encryption TXT key is absent on older firmware and encryption
// therefore cannot be determined ahead of time.
func TestTransportFallsBackToPlaintextOnce(t *testing.T) {
	var attempts []string
	conn := newFakeConn()

	cfg := config.Default().ESPHome
	cfg.EncryptionKey = "a2V5"
	d := newDevice("d", "bedroom", netip.MustParseAddr("192.168.1.10"), 6053, 1, cfg,
		func(_ context.Context, _ string, opts DialOptions) (Conn, error) {
			if opts.EncryptionKey != "" {
				attempts = append(attempts, "noise")
				return nil, errors.New("invalid frame preamble, expected 0x00")
			}
			attempts = append(attempts, "plaintext")
			return conn, nil
		}, nil, discardLogger())

	got, mode, err := d.connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if mode != transportPlaintext {
		t.Errorf("mode = %q, want plaintext after the handshake failed", mode)
	}
	if len(attempts) != 2 || attempts[0] != "noise" || attempts[1] != "plaintext" {
		t.Errorf("attempts = %v, want noise then plaintext", attempts)
	}
	_ = got.Close()
}

// With require_encryption set there is no fallback, and a handshake failure must not
// turn into a plaintext connection.
func TestRequireEncryptionNeverFallsBack(t *testing.T) {
	cfg := config.Default().ESPHome
	cfg.EncryptionKey = "a2V5"
	cfg.RequireEncryption = true

	var plaintextTried bool
	d := newDevice("d", "bedroom", netip.MustParseAddr("192.168.1.10"), 6053, 1, cfg,
		func(_ context.Context, _ string, opts DialOptions) (Conn, error) {
			if opts.EncryptionKey == "" {
				plaintextTried = true
			}
			return nil, errors.New("Handshake MAC failure")
		}, nil, discardLogger())

	if _, _, err := d.connect(context.Background()); err == nil {
		t.Fatal("expected the connection to fail")
	}
	if plaintextTried {
		t.Error("require_encryption must never fall back to plaintext")
	}
}

// A wrong key will not start working, and retrying every second across a hundred
// devices is a reconnect storm that makes the misconfiguration worse.
func TestAuthFailuresGoStraightToTheBackoffCap(t *testing.T) {
	d := testDevice(t, nil)

	auth := d.backoff(0, false, errors.New("Handshake MAC failure"))
	transient := d.backoff(0, false, errors.New("connection refused"))
	if auth <= transient {
		t.Errorf("auth backoff %s should far exceed the first transient backoff %s", auth, transient)
	}

	// A device that told us it was going away will be back in seconds.
	clean := d.backoff(5, true, nil)
	if clean > 10*time.Second {
		t.Errorf("clean disconnect backoff = %s, want a short retry", clean)
	}
}

func TestBackoffIsBoundedAndJittered(t *testing.T) {
	d := testDevice(t, nil)
	for attempt := range 20 {
		got := d.backoff(attempt, false, errors.New("timeout"))
		if got > 6*time.Minute {
			t.Fatalf("attempt %d gave %s, want it capped", attempt, got)
		}
		if got <= 0 {
			t.Fatalf("attempt %d gave a non-positive delay %s", attempt, got)
		}
	}
}

// An ESP8266 accepts only four concurrent API clients. Deleting the map entry before
// the old goroutine confirms exit would briefly leave two racing for the same slots.
func TestManagerWaitsForExitBeforeReconnecting(t *testing.T) {
	var live atomic.Int32
	var maxLive atomic.Int32

	cfg := config.Default().ESPHome
	cfg.ConnectBudget = 2 * time.Second
	m := NewManager(cfg, nil, discardLogger())
	m.dial = func(context.Context, string, DialOptions) (Conn, error) {
		n := live.Add(1)
		for {
			old := maxLive.Load()
			if n <= old || maxLive.CompareAndSwap(old, n) {
				break
			}
		}
		c := newFakeConn()
		go func() {
			<-c.done
			live.Add(-1)
		}()
		return &closingConn{fakeConn: c}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()
	waitFor(t, "manager start", func() bool { m.mu.RLock(); defer m.mu.RUnlock(); return m.ctx != nil })

	dev := registry.Device{
		ID: "mac:a4cf129b3e70", Name: "bedroom", Port: 6053, Epoch: 1,
		Addrs: []netip.Addr{netip.MustParseAddr("192.168.1.10")},
	}
	m.Sync([]registry.Device{dev})
	waitFor(t, "first connection", func() bool { return live.Load() >= 1 })

	// The device moves, as a DHCP renewal would cause.
	moved := dev
	moved.Addrs = []netip.Addr{netip.MustParseAddr("192.168.1.99")}
	moved.Epoch = 2
	m.Sync([]registry.Device{moved})
	waitFor(t, "reconnection", func() bool { return m.Managed() == 1 })

	if got := maxLive.Load(); got > 1 {
		t.Errorf("%d connections were live at once; an ESP8266 has only four slots and "+
			"Home Assistant usually holds one", got)
	}
}

// closingConn closes its done channel on Close, so the fake reports a real teardown.
type closingConn struct{ *fakeConn }

func (c *closingConn) Close() error {
	c.mu.Lock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	c.mu.Unlock()
	return c.fakeConn.Close()
}

// The exporter must not depend on Prometheus scraping /probe to keep its own connections
// from being reaped for mDNS silence: a live API connection is the evidence, so the
// manager reports it itself.
func TestConnectionReportsLivenessToTheRegistry(t *testing.T) {
	conn := newFakeConn(sensorEntity(1, "temperature", "°C", pb.SensorStateClass_STATE_CLASS_MEASUREMENT))
	live := &fakeLiveness{}
	d := testDeviceWithLiveness(t, func(context.Context, string, DialOptions) (Conn, error) {
		return conn, nil
	}, live)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx)

	// One report on connect, then one per ping.
	waitFor(t, "two liveness reports", func() bool { return len(live.snapshot()) >= 2 })

	for i, res := range live.snapshot() {
		if res.DeviceID != "mac:a4cf129b3e70" {
			t.Errorf("report %d: device = %q, want mac:a4cf129b3e70", i, res.DeviceID)
		}
		if res.Addr != netip.MustParseAddr("192.168.1.10") {
			t.Errorf("report %d: addr = %s, want 192.168.1.10", i, res.Addr)
		}
		if !res.Success {
			t.Errorf("report %d: reported a failure; only success belongs here", i)
		}
		if res.At.IsZero() {
			t.Errorf("report %d: no timestamp, so it cannot renew an endpoint", i)
		}
	}
}

// A nil reporter is a supported configuration and must not panic.
func TestNilLivenessReporterIsHarmless(t *testing.T) {
	conn := newFakeConn(sensorEntity(1, "temperature", "°C", pb.SensorStateClass_STATE_CLASS_MEASUREMENT))
	d := testDevice(t, func(context.Context, string, DialOptions) (Conn, error) { return conn, nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx)

	waitFor(t, "connection", func() bool { return d.cache.Snapshot().Connected })
}
