package registry

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

func testRegistry(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	policy, err := NewAddressPolicy(false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().Registry
	r := New(cfg, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	clk := &fakeClock{now: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)}
	r.clock = clk.Now
	return r, clk
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func addEvent(kind discovery.Kind, service, instance, addr string, port uint16, txt map[string]string) discovery.Event {
	if txt == nil {
		txt = map[string]string{}
	}
	return discovery.Event{
		Type:     discovery.EventAdd,
		Source:   "test",
		Instance: instance,
		Kind:     kind,
		Endpoint: discovery.Endpoint{
			Key:  discovery.EndpointKey{ServiceType: service, Protocol: discovery.ProtoIPv4},
			Addr: netip.MustParseAddr(addr),
			Port: port,
			TXT:  txt,
		},
	}
}

// A Gen2+ Shelly announces on both _shelly._tcp and _http._tcp. Both records share an
// instance name, so the alias table must join them before a duplicate device is created.
func TestMergesShellyAcrossBothServiceTypes(t *testing.T) {
	r, _ := testRegistry(t)

	r.applyEvent(addEvent(discovery.KindShelly, discovery.ServiceShelly,
		"shellyplus1pm-a8032ab1c2d3", "192.168.1.57", 80, map[string]string{"gen": "2"}))
	r.applyEvent(addEvent(discovery.KindShelly, discovery.ServiceHTTP,
		"shellyplus1pm-a8032ab1c2d3", "192.168.1.57", 80, map[string]string{"gen": "2"}))
	r.publish()

	snap := r.Snapshot()
	if snap.Len() != 1 {
		t.Fatalf("got %d devices, want 1 merged device: %+v", snap.Len(), snap.Devices)
	}
	dev := snap.Devices[0]
	if dev.ID != "mac:a8032ab1c2d3" {
		t.Errorf("ID = %q, want mac:a8032ab1c2d3 derived from the instance name", dev.ID)
	}
}

// A Gen1 Shelly exposes only the last three MAC bytes, so it starts as shortid:. Once an
// identity probe supplies the full MAC, the two records must fold into one device rather
// than coexisting.
func TestPromotesGen1ShortIDToMAC(t *testing.T) {
	r, _ := testRegistry(t)

	r.applyEvent(addEvent(discovery.KindShelly, discovery.ServiceHTTP,
		"shelly1-b1c2d3", "192.168.1.61", 80, nil))
	r.publish()
	if got := r.Snapshot().Devices[0].ID; got != "shortid:shelly1-b1c2d3" {
		t.Fatalf("ID = %q, want shortid:shelly1-b1c2d3", got)
	}

	// The identity probe learns the full MAC.
	r.applyEvent(addEvent(discovery.KindShelly, discovery.ServiceHTTP,
		"shelly1-b1c2d3", "192.168.1.61", 80, map[string]string{"mac": "A8:03:2A:B1:C2:D3"}))
	r.publish()

	snap := r.Snapshot()
	if snap.Len() != 1 {
		t.Fatalf("got %d devices, want 1 after promotion: %+v", snap.Len(), snap.Devices)
	}
	if got := snap.Devices[0].ID; got != "mac:a8032ab1c2d3" {
		t.Errorf("ID = %q, want the promoted mac: identity", got)
	}
	if r.stats.Promotions != 1 {
		t.Errorf("Promotions = %d, want 1", r.stats.Promotions)
	}
}

// Epoch is what tells collectors to drop a cached digest challenge or API connection.
func TestEpochBumpsOnAddressChangeOnly(t *testing.T) {
	r, _ := testRegistry(t)

	r.applyEvent(addEvent(discovery.KindESPHome, discovery.ServiceESPHome,
		"bedroom", "192.168.1.10", 6053, map[string]string{"mac": "a4cf129b3e70"}))
	r.publish()
	first := r.Snapshot().Devices[0].Epoch

	// Re-announcing the same address must not churn the epoch.
	r.applyEvent(addEvent(discovery.KindESPHome, discovery.ServiceESPHome,
		"bedroom", "192.168.1.10", 6053, map[string]string{"mac": "a4cf129b3e70"}))
	r.publish()
	if got := r.Snapshot().Devices[0].Epoch; got != first {
		t.Errorf("Epoch = %d after an identical re-announce, want %d", got, first)
	}

	// A DHCP change must bump it. The same EndpointKey replaces rather than appends.
	r.applyEvent(addEvent(discovery.KindESPHome, discovery.ServiceESPHome,
		"bedroom", "192.168.1.99", 6053, map[string]string{"mac": "a4cf129b3e70"}))
	r.publish()
	dev := r.Snapshot().Devices[0]
	if dev.Epoch == first {
		t.Error("Epoch did not change after the address changed")
	}
	if len(dev.Addrs) != 1 || dev.Addrs[0].String() != "192.168.1.99" {
		t.Errorf("Addrs = %v, want only the new address (the old one must not linger)", dev.Addrs)
	}
}

// An mDNS goodbye is routinely lost on flaky wifi, so a remove must never delete.
func TestRemoveHintDoesNotDelete(t *testing.T) {
	r, clk := testRegistry(t)

	ev := addEvent(discovery.KindShelly, discovery.ServiceShelly,
		"shellyplus1pm-a8032ab1c2d3", "192.168.1.57", 80, nil)
	ev.Endpoint.ObservedAt = clk.Now()
	r.applyEvent(ev)

	remove := ev
	remove.Type = discovery.EventRemove
	remove.At = clk.Now()
	r.applyEvent(remove)
	r.publish()

	snap := r.Snapshot()
	if snap.Len() != 1 {
		t.Fatalf("got %d devices, want the device retained after a remove hint", snap.Len())
	}
	if !snap.Devices[0].Unverified {
		t.Error("device should be marked unverified after a remove hint")
	}
}

// A working-but-quiet device must not be reaped: a successful scrape counts as liveness.
func TestSuccessfulScrapeKeepsDeviceAlive(t *testing.T) {
	r, clk := testRegistry(t)

	ev := addEvent(discovery.KindShelly, discovery.ServiceShelly,
		"shellyplus1pm-a8032ab1c2d3", "192.168.1.57", 80, nil)
	ev.Endpoint.ObservedAt = clk.Now()
	r.applyEvent(ev)

	// Past both endpoint_ttl and device_ttl since the last mDNS announcement, but
	// scraping fine throughout.
	deadline := clk.Now().Add(r.cfg.DeviceTTL + 10*time.Minute)
	for clk.Now().Before(deadline) {
		clk.Advance(5 * time.Minute)
		r.applyScrapeResult(ScrapeResult{
			DeviceID: "mac:a8032ab1c2d3",
			Addr:     netip.MustParseAddr("192.168.1.57"),
			Success:  true,
			At:       clk.Now(),
		})
		r.expire(clk.Now())
	}
	r.publish()

	if r.Snapshot().Len() != 1 {
		t.Fatal("a device scraping successfully was expired on mDNS silence alone")
	}

	// Surviving is not enough. A device kept alive by device_ttl but stripped of its
	// endpoints by endpoint_ttl is known and unusable: every probe fails with "no
	// address" and the ESPHome manager closes its connection.
	dev, ok := r.Snapshot().Get("mac:a8032ab1c2d3")
	if !ok {
		t.Fatal("device missing from the snapshot")
	}
	if _, ok := dev.Primary(); !ok {
		t.Error("a device scraping successfully lost its address to endpoint_ttl")
	}
}

// A successful scrape is evidence about the address that answered, not about every
// record the device ever announced. An IPv6 endpoint nothing can reach must still age out.
func TestScrapeSuccessRenewsTheScrapedEndpointOnly(t *testing.T) {
	r, clk := testRegistry(t)

	v4 := addEvent(discovery.KindShelly, discovery.ServiceShelly,
		"shellyplus1pm-a8032ab1c2d3", "192.168.1.57", 80, nil)
	v4.Endpoint.ObservedAt = clk.Now()
	r.applyEvent(v4)

	v6 := addEvent(discovery.KindShelly, discovery.ServiceShelly,
		"shellyplus1pm-a8032ab1c2d3", "192.168.1.58", 80, nil)
	v6.Endpoint.Key.Protocol = discovery.ProtoIPv6
	v6.Endpoint.ObservedAt = clk.Now()
	r.applyEvent(v6)

	dev := r.devices["mac:a8032ab1c2d3"]
	if len(dev.endpoints) != 2 {
		t.Fatalf("setup: got %d endpoints, want 2", len(dev.endpoints))
	}

	// Well past endpoint_ttl, scraping .57 throughout and never .58.
	deadline := clk.Now().Add(r.cfg.EndpointTTL + 10*time.Minute)
	for clk.Now().Before(deadline) {
		clk.Advance(5 * time.Minute)
		r.applyScrapeResult(ScrapeResult{
			DeviceID: "mac:a8032ab1c2d3",
			Addr:     netip.MustParseAddr("192.168.1.57"),
			Success:  true,
			At:       clk.Now(),
		})
		r.expire(clk.Now())
	}

	if len(dev.endpoints) != 1 {
		t.Fatalf("got %d endpoints, want only the scraped one to survive", len(dev.endpoints))
	}
	for _, ep := range dev.endpoints {
		if ep.Addr != netip.MustParseAddr("192.168.1.57") {
			t.Errorf("surviving endpoint is %s, want the scraped 192.168.1.57", ep.Addr)
		}
	}
}

func TestExpiresAfterDeviceTTL(t *testing.T) {
	r, clk := testRegistry(t)

	ev := addEvent(discovery.KindShelly, discovery.ServiceShelly,
		"shellyplus1pm-a8032ab1c2d3", "192.168.1.57", 80, nil)
	ev.Endpoint.ObservedAt = clk.Now()
	r.applyEvent(ev)

	clk.Advance(r.cfg.DeviceTTL + time.Minute)
	r.expire(clk.Now())
	r.publish()

	if r.Snapshot().Len() != 0 {
		t.Error("device should expire after device_ttl with no announcements and no scrapes")
	}
}

func TestStaticDevicesNeverExpire(t *testing.T) {
	r, clk := testRegistry(t)

	ev := addEvent(discovery.KindShelly, "static", "boiler", "192.168.1.51", 80, nil)
	ev.Source = "static"
	ev.DeviceID = discovery.StaticID("boiler")
	ev.Endpoint.ObservedAt = clk.Now()
	r.applyEvent(ev)
	r.publish()

	if got := r.Snapshot().Devices[0].ID; got != "static:boiler" {
		t.Fatalf("ID = %q, want the explicit static: identity", got)
	}

	clk.Advance(48 * time.Hour)
	r.expire(clk.Now())
	r.publish()

	if r.Snapshot().Len() != 1 {
		t.Error("static devices must not expire; the operator asserted they exist")
	}
}

func TestRunPublishesSnapshots(t *testing.T) {
	r, _ := testRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	r.Events() <- addEvent(discovery.KindESPHome, discovery.ServiceESPHome,
		"bedroom", "192.168.1.10", 6053, map[string]string{"mac": "a4cf129b3e70"})

	deadline := time.After(2 * time.Second)
	for r.Snapshot().Len() == 0 {
		select {
		case <-deadline:
			t.Fatal("no snapshot published within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}
