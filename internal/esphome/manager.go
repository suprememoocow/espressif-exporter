package esphome

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// Manager keeps exactly one live connection per known ESPHome device.
type Manager struct {
	cfg  config.ESPHome
	log  *slog.Logger
	dial Dialer

	// connectSem stops a router reboot turning into a hundred simultaneous SYNs.
	connectSem *semaphore.Weighted

	mu      sync.RWMutex
	devices map[string]*managedDevice
	ctx     context.Context
}

type managedDevice struct {
	dev    *device
	cancel context.CancelFunc
	epoch  uint64
	addr   string
}

// NewManager builds the manager. Call Run before Sync.
func NewManager(cfg config.ESPHome, log *slog.Logger) *Manager {
	return &Manager{
		cfg:        cfg,
		log:        log.With("component", "esphome"),
		dial:       dialReal,
		connectSem: semaphore.NewWeighted(int64(cfg.MaxConcurrentConnects)),
		devices:    map[string]*managedDevice{},
	}
}

// Run owns the manager's lifetime, tearing every connection down on cancellation.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()

	<-ctx.Done()

	m.mu.Lock()
	devices := make([]*managedDevice, 0, len(m.devices))
	for _, d := range m.devices {
		devices = append(devices, d)
	}
	m.devices = map[string]*managedDevice{}
	m.mu.Unlock()

	for _, d := range devices {
		d.cancel()
		m.awaitExit(d)
	}
	return nil
}

// Sync reconciles the live connection set against the registry.
func (m *Manager) Sync(devices []registry.Device) {
	m.mu.RLock()
	ctx := m.ctx
	m.mu.RUnlock()
	if ctx == nil || ctx.Err() != nil {
		return
	}

	wanted := make(map[string]registry.Device, len(devices))
	for _, d := range devices {
		if _, ok := d.Primary(); ok {
			wanted[d.ID] = d
		}
	}

	m.mu.Lock()
	var toStop []*managedDevice
	for id, managed := range m.devices {
		if _, keep := wanted[id]; !keep {
			toStop = append(toStop, managed)
			delete(m.devices, id)
		}
	}
	m.mu.Unlock()

	for _, d := range toStop {
		m.log.Info("device gone; closing connection", "device", d.dev.id)
		d.cancel()
		m.awaitExit(d)
	}

	for id, d := range wanted {
		m.ensure(ctx, id, d)
	}
}

// ensure starts a connection, or restarts it if the device moved.
func (m *Manager) ensure(ctx context.Context, id string, d registry.Device) {
	addr, _ := d.Primary()
	addrStr := addr.String()

	m.mu.Lock()
	existing, ok := m.devices[id]
	if ok && existing.epoch == d.Epoch && existing.addr == addrStr {
		m.mu.Unlock()
		return
	}
	if ok {
		delete(m.devices, id)
	}
	m.mu.Unlock()

	if ok {
		// Wait for the old goroutine to confirm exit before starting a replacement.
		// Starting first would briefly leave two goroutines racing for the same
		// device's API slots, and on an ESP8266 with only four, that can lock Home
		// Assistant out entirely.
		m.log.Info("device moved; reconnecting",
			"device", id, "from", existing.addr, "to", addrStr)
		existing.cancel()
		m.awaitExit(existing)
	}

	devCtx, cancel := context.WithCancel(ctx)
	dev := newDevice(id, d.Name, addr, d.Port, d.Epoch, m.cfg, m.gatedDial(), m.log)

	m.mu.Lock()
	m.devices[id] = &managedDevice{dev: dev, cancel: cancel, epoch: d.Epoch, addr: addrStr}
	m.mu.Unlock()

	go dev.run(devCtx)
}

// gatedDial wraps the dialer in the connect semaphore, held only for the dial itself.
func (m *Manager) gatedDial() Dialer {
	return func(ctx context.Context, addr string, opts DialOptions) (Conn, error) {
		if err := m.connectSem.Acquire(ctx, 1); err != nil {
			return nil, err
		}
		defer m.connectSem.Release(1)
		return m.dial(ctx, addr, opts)
	}
}

func (m *Manager) awaitExit(d *managedDevice) {
	select {
	case <-d.dev.exited:
	case <-time.After(m.cfg.ConnectBudget):
		// Leaking a goroutine is bad; leaking it silently is worse, because the
		// symptom appears later as a device that mysteriously refuses connections.
		m.log.Error("device goroutine did not exit; its API slot may be held",
			"device", d.dev.id)
	}
}

// snapshotOf returns a device's cached state.
func (m *Manager) snapshotOf(id string) (snapshot, bool) {
	m.mu.RLock()
	managed, ok := m.devices[id]
	m.mu.RUnlock()
	if !ok {
		return snapshot{}, false
	}
	// dev.addr is written once in newDevice and never mutated: an address change
	// arrives as an epoch change, which rebuilds the device rather than editing it.
	snap := managed.dev.cache.Snapshot()
	snap.Addr = managed.dev.addr
	return snap, true
}

// Connected reports how many devices currently hold a live connection.
func (m *Manager) Connected() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, d := range m.devices {
		if d.dev.cache.Snapshot().Connected {
			n++
		}
	}
	return n
}

// Managed reports how many devices the manager is responsible for.
func (m *Manager) Managed() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.devices)
}
