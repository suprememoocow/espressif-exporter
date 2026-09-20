package registry

import (
	"context"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// Registry merges observations from every discovery backend into a single set of
// devices and publishes immutable snapshots.
//
// Concurrency model: one goroutine (Run) owns all mutable state. Everything else reads
// an atomically swapped *Snapshot. There are no locks on the read path.
type Registry struct {
	cfg    config.Registry
	policy AddressPolicy
	log    *slog.Logger
	clock  func() time.Time

	// Owned by the actor goroutine.
	devices map[string]*deviceState
	aliases map[string]string // every known handle -> device ID

	snapshot atomic.Pointer[Snapshot]
	events   chan discovery.Event
	results  chan ScrapeResult

	stats Stats
}

// sourceStatic is the backend name whose devices are exempt from expiry.
const sourceStatic = "static"

// ScrapeResult reports a probe outcome back to the registry.
type ScrapeResult struct {
	DeviceID string
	Addr     netip.Addr
	Success  bool
	At       time.Time
}

// Stats counts registry activity for self-metrics. Read via the actor only.
type Stats struct {
	Merges         uint64
	Promotions     uint64
	Expirations    map[string]uint64
	AddressChanges map[string]uint64
	EventsDropped  uint64
}

// New builds a registry. Run must be called to start the actor.
func New(cfg config.Registry, policy AddressPolicy, log *slog.Logger) *Registry {
	r := &Registry{
		cfg:     cfg,
		policy:  policy,
		log:     log.With("component", "registry"),
		clock:   time.Now,
		devices: map[string]*deviceState{},
		aliases: map[string]string{},
		// Buffered so a burst of mDNS announcements after a router reboot does not block
		// a backend's reader goroutine. Overflow is counted, never silently ignored.
		events:  make(chan discovery.Event, 1024),
		results: make(chan ScrapeResult, 256),
		stats: Stats{
			Expirations:    map[string]uint64{},
			AddressChanges: map[string]uint64{},
		},
	}
	r.snapshot.Store(newSnapshot(r.clock(), nil))
	return r
}

// Events is the channel discovery backends publish to.
func (r *Registry) Events() chan<- discovery.Event { return r.events }

// Snapshot returns the current immutable view. Never nil.
func (r *Registry) Snapshot() *Snapshot { return r.snapshot.Load() }

// Get resolves a device ID against the current snapshot.
func (r *Registry) Get(id string) (Device, bool) { return r.Snapshot().Get(id) }

// List returns devices of a kind from the current snapshot.
func (r *Registry) List(kind discovery.Kind) []Device { return r.Snapshot().List(kind) }

// ReportScrape records a probe outcome. It never blocks: a full channel means the actor
// is busy, and a dropped liveness hint is far cheaper than a stalled probe.
func (r *Registry) ReportScrape(res ScrapeResult) {
	if res.At.IsZero() {
		res.At = r.clock()
	}
	select {
	case r.results <- res:
	default:
	}
}

// Run owns the registry state until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) error {
	refresh := time.NewTicker(r.cfg.RefreshInterval)
	defer refresh.Stop()
	expire := time.NewTicker(time.Minute)
	defer expire.Stop()

	// Snapshot publication is coalesced: a burst of announcements produces one swap
	// rather than hundreds, and readers never see a half-applied batch.
	var (
		dirty   bool
		publish <-chan time.Time
		timer   *time.Timer
	)
	markDirty := func() {
		if dirty {
			return
		}
		dirty = true
		if timer == nil {
			timer = time.NewTimer(r.cfg.SnapshotCoalesce)
		} else {
			timer.Reset(r.cfg.SnapshotCoalesce)
		}
		publish = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev := <-r.events:
			if r.applyEvent(ev) {
				markDirty()
			}

		case res := <-r.results:
			if r.applyScrapeResult(res) {
				markDirty()
			}

		case <-expire.C:
			if r.expire(r.clock()) {
				markDirty()
			}

		case <-refresh.C:
			// Re-resolution itself is driven by the backends; this tick only ages the
			// snapshot's staleness flags.
			markDirty()

		case <-publish:
			dirty, publish = false, nil
			r.publish()
		}
	}
}

func (r *Registry) applyEvent(ev discovery.Event) bool {
	now := r.clock()
	if ev.At.IsZero() {
		ev.At = now
	}

	switch ev.Type {
	case discovery.EventSourceReset:
		return r.markSourceUnverified(ev.Source, ev.At)
	case discovery.EventRemove:
		return r.applyRemoveHint(ev)
	case discovery.EventAdd:
		return r.applyAdd(ev, now)
	default:
		return false
	}
}

func (r *Registry) applyAdd(ev discovery.Event, now time.Time) bool {
	id := r.resolveID(ev)
	if id == "" {
		return false
	}

	dev, ok := r.devices[id]
	if !ok {
		dev = &deviceState{
			Device: Device{
				ID:        id,
				Kind:      ev.Kind,
				Name:      instanceLabel(ev.Instance),
				FirstSeen: now,
			},
			endpoints: map[discovery.EndpointKey]*endpointState{},
			sources:   map[string]bool{},
		}
		r.devices[id] = dev
		r.log.Info("device discovered",
			"device", id, "kind", ev.Kind, "name", dev.Name, "source", ev.Source)
	}

	r.bindAliases(id, ev)
	if ev.Source == sourceStatic {
		// The operator asserted this device exists, and that assertion outranks mDNS
		// silence, so it is exempt from TTL expiry.
		dev.Static = true
	}

	if dev.Kind == discovery.KindUnknown && ev.Kind != discovery.KindUnknown {
		dev.Kind = ev.Kind
	}
	if ev.Endpoint.Host != "" {
		dev.Hostname = ev.Endpoint.Host
	}
	dev.sources[ev.Source] = true

	// Replace, never append: keying by (service, interface, protocol) is what keeps a
	// re-resolve after a DHCP change from leaving the old address behind.
	dev.endpoints[ev.Endpoint.Key] = &endpointState{Endpoint: ev.Endpoint, source: ev.Source}

	dev.TXT = mergeTXT(dev.TXT, ev.Endpoint.TXT)
	dev.lastDiscovery = ev.At
	dev.Unverified = false
	if ev.Endpoint.Port != 0 {
		dev.Port = ev.Endpoint.Port
	}

	r.recomputeAddrs(dev, "discovery")
	r.touch(dev)
	return true
}

// resolveID finds the device an event belongs to, creating a new identity only when no
// known handle matches. Promotion from a weak to a strong identity happens here too.
func (r *Registry) resolveID(ev discovery.Event) string {
	if ev.DeviceID != "" {
		return ev.DeviceID
	}
	derived := DeriveID(ev.Kind, ev.Instance, ev.Endpoint.TXT)
	if derived == "" {
		return ""
	}

	// A handle we have already bound wins, because it may point at a device that has
	// since been promoted to a stronger ID than this event alone would derive.
	for _, handle := range eventHandles(ev) {
		if id, ok := r.aliases[handle]; ok {
			if IsMACID(derived) && !IsMACID(id) {
				return r.promote(id, derived)
			}
			return id
		}
	}
	if id, ok := r.aliases[derived]; ok {
		return id
	}
	return derived
}

// promote folds a weakly identified device into its strong identity. This is how a Gen1
// Shelly known only by shortid: becomes a mac: device once its MAC is learned, and how
// the _shelly._tcp and _http._tcp records for one Gen2 device converge.
func (r *Registry) promote(oldID, newID string) string {
	if oldID == newID {
		return newID
	}
	old, ok := r.devices[oldID]
	if !ok {
		return newID
	}

	target, exists := r.devices[newID]
	if !exists {
		delete(r.devices, oldID)
		old.ID = newID
		r.devices[newID] = old
		target = old
		r.stats.Promotions++
		r.log.Info("device identity promoted", "from", oldID, "to", newID)
	} else {
		for k, ep := range old.endpoints {
			target.endpoints[k] = ep
		}
		target.TXT = mergeTXT(target.TXT, old.TXT)
		maps.Copy(target.sources, old.sources)
		if target.lastDiscovery.Before(old.lastDiscovery) {
			target.lastDiscovery = old.lastDiscovery
		}
		if target.lastSuccess.Before(old.lastSuccess) {
			target.lastSuccess = old.lastSuccess
		}
		if old.FirstSeen.Before(target.FirstSeen) {
			target.FirstSeen = old.FirstSeen
		}
		delete(r.devices, oldID)
		r.stats.Merges++
		r.log.Info("devices merged", "from", oldID, "into", newID)
	}

	for handle, id := range r.aliases {
		if id == oldID {
			r.aliases[handle] = newID
		}
	}
	r.aliases[oldID] = newID
	r.recomputeAddrs(target, "merge")
	return newID
}

func (r *Registry) bindAliases(id string, ev discovery.Event) {
	for _, h := range eventHandles(ev) {
		r.aliases[h] = id
	}
	r.aliases[id] = id
}

// eventHandles lists every identifier this observation could be known by. Binding all of
// them is what lets the _shelly._tcp and _http._tcp records for one device converge
// before a duplicate is ever created.
func eventHandles(ev discovery.Event) []string {
	handles := make([]string, 0, 4)
	label := instanceLabel(ev.Instance)
	if label != "" {
		handles = append(handles, prefixName+lower(label))
	}
	if mac := NormaliseMAC(ev.Endpoint.TXT["mac"]); len(mac) == 12 {
		handles = append(handles, prefixMAC+mac)
	}
	if id := ev.Endpoint.TXT["id"]; id != "" {
		handles = append(handles, prefixName+lower(id))
	}
	if derived := DeriveID(ev.Kind, ev.Instance, ev.Endpoint.TXT); derived != "" {
		handles = append(handles, derived)
	}
	return handles
}

func (r *Registry) applyRemoveHint(ev discovery.Event) bool {
	id, ok := r.aliases[DeriveID(ev.Kind, ev.Instance, ev.Endpoint.TXT)]
	if !ok {
		if id, ok = r.aliases[prefixName+lower(instanceLabel(ev.Instance))]; !ok {
			return false
		}
	}
	dev, ok := r.devices[id]
	if !ok {
		return false
	}

	// A remove is a hint, not an instruction. mDNS goodbye packets are routinely lost on
	// flaky wifi, and a device that stops announcing is usually still perfectly
	// reachable. Mark it for re-resolution and let expiry decide.
	changed := false
	for k, ep := range dev.endpoints {
		if ep.source != ev.Source {
			continue
		}
		if ev.Endpoint.Key != (discovery.EndpointKey{}) && k != ev.Endpoint.Key {
			continue
		}
		if !ep.unverified {
			ep.unverified = true
			ep.removeHintAt = ev.At
			changed = true
		}
	}
	if changed {
		dev.Unverified = allUnverified(dev)
		r.log.Debug("remove hint recorded", "device", id, "source", ev.Source)
	}
	return changed
}

func (r *Registry) markSourceUnverified(source string, at time.Time) bool {
	changed := false
	for _, dev := range r.devices {
		devChanged := false
		for _, ep := range dev.endpoints {
			if ep.source == source && !ep.unverified {
				ep.unverified = true
				ep.removeHintAt = at
				devChanged = true
			}
		}
		if devChanged {
			changed = true
			dev.Unverified = allUnverified(dev)
		}
	}
	if changed {
		r.log.Info("discovery source reset; endpoints pending re-verification", "source", source)
	}
	return changed
}

func (r *Registry) applyScrapeResult(res ScrapeResult) bool {
	dev, ok := r.devices[res.DeviceID]
	if !ok {
		return false
	}
	if !res.Success {
		return false
	}

	// A successful scrape is stronger liveness evidence than an mDNS announcement, so it
	// counts towards LastSeen. Without this, a device that is working but quiet on mDNS
	// would eventually be reaped.
	dev.lastSuccess = res.At
	if res.Addr.IsValid() && (!dev.hasPinned || dev.pinned != res.Addr) {
		dev.pinned, dev.hasPinned = res.Addr, true
		r.recomputeAddrs(dev, "scrape_success")
	}
	for _, ep := range dev.endpoints {
		if ep.Addr == res.Addr {
			ep.unverified = false
			// The address answered, so it is observed just as surely as if mDNS had
			// announced it. Without this the endpoint still ages out under endpoint_ttl
			// while the device survives under device_ttl, leaving it known but
			// addressless: every probe fails with "no address" and the ESPHome manager
			// closes a perfectly healthy connection.
			ep.ObservedAt = res.At
		}
	}
	dev.Unverified = allUnverified(dev)
	r.touch(dev)
	return true
}

func (r *Registry) expire(now time.Time) bool {
	changed := false

	for id, dev := range r.devices {
		if dev.Static {
			continue
		}
		// Per device: a previous device losing an endpoint must not drag this one
		// through a pointless recomputeAddrs and touch.
		devChanged := false
		for k, ep := range dev.endpoints {
			expired := now.Sub(ep.ObservedAt) > r.cfg.EndpointTTL
			// An endpoint that received a remove hint, stayed unverified past the grace
			// period, and has produced no successful scrape since, is genuinely gone.
			hinted := ep.unverified && !ep.removeHintAt.IsZero() &&
				now.Sub(ep.removeHintAt) > r.cfg.RemoveGrace &&
				dev.lastSuccess.Before(ep.removeHintAt)

			if expired || hinted {
				delete(dev.endpoints, k)
				devChanged = true
			}
		}
		if devChanged {
			changed = true
			r.recomputeAddrs(dev, "expiry")
			r.touch(dev)
		}

		// device_ttl is deliberately generous. On a home LAN a twenty-minute absence is a
		// wifi problem, not a decommission, and an alertable probe_success 0 beats a
		// series that silently disappears.
		if now.Sub(dev.LastSeen) > r.cfg.DeviceTTL {
			r.forget(id, dev, "ttl")
			changed = true
		}
	}
	return changed
}

func (r *Registry) forget(id string, dev *deviceState, reason string) {
	delete(r.devices, id)
	for handle, target := range r.aliases {
		if target == id {
			delete(r.aliases, handle)
		}
	}
	r.stats.Expirations[reason]++
	r.log.Info("device expired", "device", id, "reason", reason,
		"last_seen", dev.LastSeen.Format(time.RFC3339))
}

func (r *Registry) recomputeAddrs(dev *deviceState, reason string) {
	candidates := make([]AddressCandidate, 0, len(dev.endpoints))
	for _, ep := range dev.endpoints {
		candidates = append(candidates, AddressCandidate{Addr: ep.Addr, Trusted: ep.Trusted})
		if dev.Port == 0 && ep.Port != 0 {
			dev.Port = ep.Port
		}
	}

	pinned := netip.Addr{}
	if dev.hasPinned {
		pinned = dev.pinned
	}
	next, rejected := r.policy.OrderCandidates(candidates, pinned)
	dev.RejectedAddrs = rejected

	// A device with endpoints but no usable address would otherwise fail every probe
	// with a bare "no address" and no clue why. This is the most common consequence of
	// a too-narrow allow_cidrs or an IPv6-only announcement.
	if len(next) == 0 && len(rejected) > 0 {
		r.log.Warn("device has no routable address; every observed address was filtered out",
			"device", dev.ID, "rejected", rejected,
			"hint", "check discovery.allow_cidrs, discovery.deny_cidrs and discovery.ipv6")
	}

	// Losing the last address used to be completely silent, which made the resulting
	// metric gap look like a device fault rather than a TTL. In practice only expiry can
	// empty the set, so name the TTL that did it.
	if len(next) == 0 && len(rejected) == 0 && len(dev.Addrs) > 0 {
		r.log.Warn("device has no usable address left; its metrics will stop",
			"device", dev.ID, "reason", reason,
			"last_seen", dev.LastSeen.Format(time.RFC3339),
			"endpoint_ttl", r.cfg.EndpointTTL,
			"hint", "registry.endpoint_ttl must exceed the interval at which the "+
				"discovery source re-observes a device; for avahi that is "+
				"discovery.avahi.rebrowse_interval")
	}

	if !slices.Equal(next, dev.Addrs) {
		// Epoch is what tells the collectors to discard per-device caches: a Shelly
		// digest challenge bound to the old IP, or an ESPHome connection to it.
		dev.Addrs = next
		dev.Epoch++
		r.stats.AddressChanges[reason]++
		r.log.Debug("device addresses changed",
			"device", dev.ID, "reason", reason, "addrs", next, "epoch", dev.Epoch)
	}
}

func (r *Registry) touch(dev *deviceState) {
	dev.LastSeen = laterOf(dev.lastDiscovery, dev.lastSuccess)
}

func (r *Registry) publish() {
	now := r.clock()
	devices := make([]Device, 0, len(r.devices))
	for _, dev := range r.devices {
		d := dev.Device
		d.Addrs = slices.Clone(dev.Addrs)
		d.RejectedAddrs = slices.Clone(dev.RejectedAddrs)
		d.TXT = maps.Clone(dev.TXT)
		devices = append(devices, d)
	}
	r.snapshot.Store(newSnapshot(now, devices))
}

// StatsSnapshot copies the counters for self-metrics.
func (r *Registry) StatsSnapshot() Stats {
	return Stats{
		Merges:         r.stats.Merges,
		Promotions:     r.stats.Promotions,
		Expirations:    maps.Clone(r.stats.Expirations),
		AddressChanges: maps.Clone(r.stats.AddressChanges),
		EventsDropped:  r.stats.EventsDropped,
	}
}

func allUnverified(dev *deviceState) bool {
	if len(dev.endpoints) == 0 {
		return false
	}
	for _, ep := range dev.endpoints {
		if !ep.unverified {
			return false
		}
	}
	return true
}

func mergeTXT(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	maps.Copy(dst, src)
	return dst
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
