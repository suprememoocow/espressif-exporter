// Package zeroconf implements a Discoverer using a native Go mDNS browser.
//
// This backend sends and receives multicast directly, so it only works where the process
// can reach the LAN's multicast groups: under network_mode: host, on a bare host, or
// during development on macOS. In a bridged container it will find nothing, which is why
// the Avahi backend exists.
package zeroconf

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"
	"golang.org/x/sync/errgroup"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// Name identifies this backend in events and metrics.
const Name = "zeroconf"

// browseRestartDelay paces restarts when Browse returns early, so a persistent failure
// (no multicast route, for instance) does not become a hot loop.
const browseRestartDelay = 5 * time.Second

// Discoverer browses one goroutine per service type.
type Discoverer struct {
	serviceTypes []string
	log          *slog.Logger

	mu     sync.Mutex
	health discovery.Health
}

// New builds the backend.
func New(serviceTypes []string, log *slog.Logger) *Discoverer {
	return &Discoverer{
		serviceTypes: serviceTypes,
		log:          log.With("component", "zeroconf"),
	}
}

// Name implements discovery.Discoverer.
func (d *Discoverer) Name() string { return Name }

// Health implements discovery.Discoverer.
func (d *Discoverer) Health() discovery.Health {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.health
}

// Run browses until ctx is cancelled.
func (d *Discoverer) Run(ctx context.Context, out chan<- discovery.Event) error {
	// Say so immediately when this backend cannot possibly work here. Without this the
	// exporter reports zero devices indefinitely and gives no reason, which is the
	// hardest kind of failure to diagnose.
	var inUse *portInUseError
	if err := checkPortOwnership(); errors.As(err, &inUse) {
		d.log.Error("another mDNS daemon owns UDP port 5353, so this backend will "+
			"receive nothing and discover no devices",
			"error", inUse, "fix", inUse.Advice())
		d.setUp(false, "UDP 5353 is owned by another mDNS daemon")
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, service := range d.serviceTypes {
		g.Go(func() error {
			d.browseForever(ctx, service, out)
			return nil
		})
	}

	defer d.setUp(false, "stopped")
	return g.Wait()
}

// browseForever keeps one browse running, restarting it if it returns.
//
// zeroconf.Browse returns when its context ends, but also on some transient network
// errors. Treating a return as fatal would silently stop discovering that service type
// for the life of the process.
func (d *Discoverer) browseForever(ctx context.Context, service string, out chan<- discovery.Event) {
	for ctx.Err() == nil {
		entries := make(chan *zeroconf.ServiceEntry, 32)

		done := make(chan struct{})
		go func() {
			defer close(done)
			for entry := range entries {
				d.emit(ctx, service, entry, out)
			}
		}()

		if err := zeroconf.Browse(ctx, service, "local.", entries); err != nil && ctx.Err() == nil {
			d.log.Warn("mDNS browse failed; retrying",
				"service", service, "error", err, "retry_in", browseRestartDelay)
			d.setUp(false, err.Error())
		}
		<-done

		select {
		case <-ctx.Done():
			return
		case <-time.After(browseRestartDelay):
		}
	}
}

func (d *Discoverer) emit(
	ctx context.Context, service string, entry *zeroconf.ServiceEntry, out chan<- discovery.Event,
) {
	if entry == nil {
		return
	}
	txt := discovery.ParseTXT(entry.Text)
	kind := discovery.Classify(service, entry.Instance, txt)
	if kind == discovery.KindUnknown {
		// A home LAN is full of printers and TVs on _http._tcp; there is nothing to be
		// gained from carrying them through the registry.
		return
	}

	now := time.Now()
	for _, ip := range append(append([]netipSource{}, ipv4(entry)...), ipv6(entry)...) {
		ev := discovery.Event{
			Type:     discovery.EventAdd,
			Source:   Name,
			Instance: entry.Instance,
			Domain:   entry.Domain,
			Kind:     kind,
			At:       now,
			Endpoint: discovery.Endpoint{
				Key: discovery.EndpointKey{
					ServiceType: service,
					// The native browser already de-duplicates across interfaces, so
					// there is no per-interface index to key on.
					Interface: 0,
					Protocol:  ip.proto,
				},
				Host:       entry.HostName,
				Addr:       ip.addr,
				Port:       uint16(entry.Port), //nolint:gosec // an mDNS port is a uint16 on the wire
				TXT:        txt,
				ObservedAt: now,
			},
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return
		}
	}

	// Receiving anything is the only proof this backend works, so health is asserted
	// here rather than on a successful browse call.
	d.mu.Lock()
	d.health.LastEventAt = now
	d.health.Up = true
	d.mu.Unlock()
}

type netipSource struct {
	addr  netip.Addr
	proto string
}

func ipv4(entry *zeroconf.ServiceEntry) []netipSource {
	out := make([]netipSource, 0, len(entry.AddrIPv4))
	for _, ip := range entry.AddrIPv4 {
		if a, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, netipSource{addr: a.Unmap(), proto: discovery.ProtoIPv4})
		}
	}
	return out
}

func ipv6(entry *zeroconf.ServiceEntry) []netipSource {
	out := make([]netipSource, 0, len(entry.AddrIPv6))
	for _, ip := range entry.AddrIPv6 {
		if a, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, netipSource{addr: a.Unmap(), proto: discovery.ProtoIPv6})
		}
	}
	return out
}

func (d *Discoverer) setUp(up bool, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.health.Up = up
	d.health.Reason = reason
}
