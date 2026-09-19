// Package discovery finds ESPHome and Shelly devices on the local network.
//
// Three backends implement the same Discoverer interface:
//
//   - avahi:    browses through the host's avahi-daemon over the system D-Bus socket.
//     This is what lets the exporter run in a bridged container, which cannot
//     send or receive multicast and so cannot do mDNS itself.
//   - zeroconf: a native Go mDNS browser, for network_mode: host and for development.
//   - static:   a configured seed list, for VLANs that do not forward mDNS and as the
//     fallback when Avahi is unavailable.
//
// Swapping between them is a configuration change, not a code change.
package discovery

import (
	"context"
	"net/netip"
	"strings"
	"time"
)

// Kind is the device family, which selects the collector.
type Kind string

const (
	KindESPHome Kind = "esphome"
	KindShelly  Kind = "shelly"
	KindUnknown Kind = "unknown"
)

// Service types we browse. Note that Shelly Gen1 devices advertise only _http._tcp, so
// browsing _shelly._tcp alone would miss the entire Gen1 fleet.
const (
	ServiceESPHome = "_esphomelib._tcp"
	ServiceShelly  = "_shelly._tcp"
	ServiceHTTP    = "_http._tcp"
)

// EndpointKey identifies one (service, interface, protocol) observation slot.
//
// Keying endpoints this way is what absorbs DHCP address changes: a re-resolve for the
// same key replaces the previous value rather than appending a second one, so stale
// addresses cannot accumulate.
type EndpointKey struct {
	ServiceType string
	Interface   int32 // Avahi interface index; 0 for zeroconf and static
	Protocol    string
}

// Protocol values for EndpointKey.
const (
	ProtoIPv4 = "ipv4"
	ProtoIPv6 = "ipv6"
)

// Endpoint is one resolved address for a device.
type Endpoint struct {
	Key EndpointKey

	// Host is the mDNS hostname, for display only. It must never be dialled: the
	// container has no mDNS resolver and no multicast path, so a .local name does not
	// resolve there. Addr is the authoritative value.
	Host string

	Addr netip.Addr
	Port uint16
	TXT  map[string]string

	// Cached reports an Avahi LOOKUP_RESULT_CACHED result, i.e. a cache replay rather
	// than a fresh query.
	Cached bool

	// Trusted marks an address the operator supplied explicitly rather than one we
	// discovered. Such an address bypasses the routability heuristics: if someone
	// configures a loopback or an off-subnet address, they mean it, and second-guessing
	// them produces a device that silently never scrapes.
	Trusted bool

	ObservedAt time.Time
}

// EventType distinguishes the three things a backend can tell the registry.
type EventType uint8

const (
	// EventAdd is a successful resolve. It also clears any probe backoff, because a
	// device re-announcing itself is strong evidence that it is reachable again.
	EventAdd EventType = iota

	// EventRemove is a hint, never an instruction to delete. mDNS goodbye packets are
	// routinely lost on flaky wifi, so the registry re-resolves instead of evicting.
	EventRemove

	// EventSourceReset means a backend reconnected and its prior state is unverified.
	// Avahi replays its record cache to a new browser within seconds, so the normal
	// outcome is that everything re-verifies immediately and nothing is lost.
	EventSourceReset
)

func (t EventType) String() string {
	switch t {
	case EventAdd:
		return "add"
	case EventRemove:
		return "remove"
	case EventSourceReset:
		return "source_reset"
	default:
		return "unknown"
	}
}

// Event is one observation from a discovery backend.
type Event struct {
	Type   EventType
	Source string // avahi | zeroconf | static

	// DeviceID, when set, overrides the registry's identity derivation. Only a backend
	// that already knows the identity should set it — currently just static, whose
	// entries are named by the operator rather than discovered.
	DeviceID string

	Instance string // mDNS instance name, or the static entry's id
	Domain   string
	Kind     Kind
	Endpoint Endpoint
	At       time.Time
}

// StaticID namespaces an operator-supplied identifier so it cannot collide with a
// discovered one.
func StaticID(id string) string { return "static:" + strings.ToLower(id) }

// Health describes a backend's current state, for /readyz and self-metrics.
type Health struct {
	Up          bool
	Reason      string
	LastEventAt time.Time
	Reconnects  uint64
}

// Discoverer is a source of device observations.
type Discoverer interface {
	Name() string

	// Run blocks until ctx is done. It owns its own reconnect and retry loop, so it must
	// not return on a transient error, and it must not close out.
	Run(ctx context.Context, out chan<- Event) error

	Health() Health
}

// Refresher is implemented by backends that can re-resolve a single instance on demand.
// The registry uses it to verify an EventRemove hint instead of acting on it.
type Refresher interface {
	Refresh(ctx context.Context, instance, serviceType string) error
}
