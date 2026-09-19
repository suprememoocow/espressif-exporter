// Package registry holds the authoritative set of known devices, merging observations
// from every discovery backend and publishing immutable snapshots to readers.
package registry

import (
	"net/netip"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// Device is the registry's view of one physical device.
//
// Readers receive a copy taken from an immutable snapshot, so a Device value is safe to
// hold and read without locking.
type Device struct {
	// ID is stable for the physical life of the device and is never an IP address.
	// Prometheus derives instance from the /sd target, so an IP-derived ID would rename
	// every series on every DHCP lease change.
	ID   string
	Kind discovery.Kind

	// Name is the mDNS instance or ESPHome node name: stable, and what a human reads.
	Name string
	// Hostname is display only. See discovery.Endpoint.Host.
	Hostname string

	// Addrs is ordered by preference; Addrs[0] is the primary.
	Addrs []netip.Addr

	// RejectedAddrs were observed but filtered out as unroutable. Exposed purely for
	// diagnosis: an empty Addrs with a populated RejectedAddrs is the signature of a
	// too-narrow allow_cidrs or an IPv6-only announcement.
	RejectedAddrs []netip.Addr
	Port          uint16
	TXT           map[string]string

	FirstSeen time.Time
	LastSeen  time.Time
	Static    bool

	// Epoch increments whenever Addrs or Port change. Collectors cache per-device state
	// keyed by it — a digest challenge and identity record for Shelly, a live connection
	// and entity map for ESPHome — and must discard that cache when it moves.
	Epoch uint64

	// Unverified is set when every endpoint is awaiting re-resolution after a remove
	// hint or a source reset. The device is still exported; it is not a deletion.
	Unverified bool
}

// Primary returns the preferred address, if any.
func (d Device) Primary() (netip.Addr, bool) {
	if len(d.Addrs) == 0 {
		return netip.Addr{}, false
	}
	return d.Addrs[0], true
}

// Stale reports whether the device has not been seen within d.
func (dev Device) Stale(now time.Time, after time.Duration) bool {
	return after > 0 && now.Sub(dev.LastSeen) > after
}

// deviceState is the registry actor's private, mutable record. Only the actor goroutine
// touches it; readers see Device values copied into a Snapshot.
type deviceState struct {
	Device

	endpoints map[discovery.EndpointKey]*endpointState

	// pinned is the address that last produced a successful scrape. Re-resolves update
	// the endpoint record but do not move the primary away from a pinned address, which
	// stops the exporter flapping between a device's two interfaces.
	pinned    netip.Addr
	hasPinned bool

	lastDiscovery time.Time
	lastSuccess   time.Time
	sources       map[string]bool
}

type endpointState struct {
	discovery.Endpoint

	// unverified marks an endpoint that received a remove hint or belonged to a source
	// that reset. It is evicted early only if it stays unverified past the grace period
	// without a successful scrape.
	unverified   bool
	removeHintAt time.Time
	source       string
}
