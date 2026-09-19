// Package static implements a Discoverer over an operator-configured seed list.
//
// It exists for three cases: devices on a VLAN that does not forward mDNS, devices with
// mDNS disabled, and as the fallback that keeps the exporter useful when Avahi is
// unavailable. Configuring it alongside avahi is recommended, so a total Avahi outage
// degrades coverage rather than eliminating it.
package static

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// Name identifies this backend in events and metrics.
const Name = "static"

// Discoverer republishes its seed list on an interval so that static devices never age
// out of the registry through the ordinary TTL path.
type Discoverer struct {
	entries  []entry
	interval time.Duration

	mu     sync.Mutex
	health discovery.Health
}

type entry struct {
	id   string
	kind discovery.Kind
	addr netip.Addr
	port uint16
}

// New validates the configured entries and builds the backend.
func New(entries []config.StaticEntry, interval time.Duration) (*Discoverer, error) {
	out := make([]entry, 0, len(entries))
	for i, e := range entries {
		addr, err := netip.ParseAddr(e.Address)
		if err != nil {
			return nil, fmt.Errorf("static[%d] %q: %w", i, e.ID, err)
		}
		kind := discovery.Kind(e.Kind)
		if kind != discovery.KindESPHome && kind != discovery.KindShelly {
			return nil, fmt.Errorf("static[%d] %q: kind %q is not esphome or shelly", i, e.ID, e.Kind)
		}
		port := e.Port
		if port == 0 {
			port = defaultPort(kind)
		}
		out = append(out, entry{id: e.ID, kind: kind, addr: addr.Unmap(), port: port})
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Discoverer{entries: out, interval: interval}, nil
}

func defaultPort(kind discovery.Kind) uint16 {
	if kind == discovery.KindESPHome {
		return 6053
	}
	return 80
}

// Name implements discovery.Discoverer.
func (d *Discoverer) Name() string { return Name }

// Health implements discovery.Discoverer. A static list is always up: there is nothing
// to fail.
func (d *Discoverer) Health() discovery.Health {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.health
	h.Up = true
	return h
}

// Run publishes the seed list immediately, then on every interval tick.
func (d *Discoverer) Run(ctx context.Context, out chan<- discovery.Event) error {
	d.emitAll(ctx, out)

	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			d.emitAll(ctx, out)
		}
	}
}

func (d *Discoverer) emitAll(ctx context.Context, out chan<- discovery.Event) {
	now := time.Now()
	for _, e := range d.entries {
		proto := discovery.ProtoIPv4
		if e.addr.Is6() {
			proto = discovery.ProtoIPv6
		}
		ev := discovery.Event{
			Type:     discovery.EventAdd,
			Source:   Name,
			DeviceID: discovery.StaticID(e.id),
			Instance: e.id,
			Domain:   "local",
			Kind:     e.kind,
			At:       now,
			Endpoint: discovery.Endpoint{
				Key: discovery.EndpointKey{
					ServiceType: Name,
					Protocol:    proto,
				},
				Addr:       e.addr,
				Port:       e.port,
				Trusted:    true,
				TXT:        map[string]string{},
				ObservedAt: now,
			},
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return
		}
	}

	d.mu.Lock()
	d.health.LastEventAt = now
	d.mu.Unlock()
}
