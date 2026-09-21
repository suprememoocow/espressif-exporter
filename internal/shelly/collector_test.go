package shelly

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// fakeDevice stands in for a real Shelly over HTTP.
type fakeDevice struct {
	shelly string
	status string
	config string
	// configStatus overrides the status code of the config response; 0 means 200. A 401
	// here stands in for the common case of wrong or missing credentials.
	configStatus int
	requests     map[string]int
}

func newFakeDevice(t *testing.T, shellyBody, statusBody string) (*fakeDevice, registry.Device) {
	t.Helper()
	d := &fakeDevice{shelly: shellyBody, status: statusBody, requests: map[string]int{}}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.requests[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/shelly":
			_, _ = w.Write([]byte(d.shelly))
		case "/rpc/Shelly.GetStatus", "/status":
			_, _ = w.Write([]byte(d.status))
		case "/rpc/Shelly.GetConfig", "/settings":
			if d.configStatus != 0 {
				w.WriteHeader(d.configStatus)
				return
			}
			_, _ = w.Write([]byte(d.config))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)

	addr, portStr, _ := strings.Cut(strings.TrimPrefix(ts.URL, "http://"), ":")
	port, _ := strconv.Atoi(portStr)
	return d, registry.Device{
		ID:    "mac:a8032ab12345",
		Kind:  discovery.KindShelly,
		Name:  "shellyplus1pm-a8032ab12345",
		Addrs: []netip.Addr{netip.MustParseAddr(addr)},
		Port:  uint16(port),
	}
}

func names(metricsOut []prometheus.Metric) []string {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collected{metricsOut})
	mfs, _ := reg.Gather()
	out := make([]string, 0, len(mfs))
	for _, mf := range mfs {
		out = append(out, mf.GetName())
	}
	return out
}

const gen2ShellyBody = `{"name":"Lamp","id":"shellyplus1pm-a8032ab12345","mac":"A8032AB12345",
 "model":"SNSW-001P16EU","gen":2,"fw_id":"20241011-114455/1.4.4","ver":"1.4.4",
 "app":"Plus1PM","auth_en":false}`

// Gen1 answers /shelly with no name field at all — the whole reason the device name has to
// come from /settings.
const gen1ShellyBody = `{"type":"SHSW-PM","mac":"A8032AB12345","auth":false,
 "fw":"20230913-112003/v1.14.0-gcb84623","num_outputs":1}`

func TestProbeEndToEnd(t *testing.T) {
	status := string(fixture(t, "gen2_plus1pm.json"))
	fake, dev := newFakeDevice(t, gen2ShellyBody, status)
	fake.config = string(fixture(t, "gen2_config.json"))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	out, res := c.Probe(context.Background(), dev)

	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	got := names(out)
	for _, wanted := range []string{
		"espressif_device_info", "espressif_switch_on", "espressif_power_watts",
		"espressif_energy_joules_total", "espressif_uptime_seconds",
	} {
		if !contains(got, wanted) {
			t.Errorf("missing %s; got %v", wanted, got)
		}
	}

	// The configured component name reaches the emitted series.
	if !hasLabel(out, "espressif_switch_on", "name", "Kitchen") {
		t.Errorf("switch:0 should carry name=Kitchen from GetConfig; got %v", labelDump(out, "espressif_switch_on"))
	}

	// Steady state must be one status request per scrape, with identity served from
	// cache. Anything more multiplies load across a hundred-device fleet.
	for range 5 {
		if _, res := c.Probe(context.Background(), dev); !res.Success {
			t.Fatalf("repeat probe failed: %+v", res)
		}
	}
	if n := fake.requests["/shelly"]; n != 1 {
		t.Errorf("/shelly requested %d times across 6 probes, want 1 (identity is cached for 6h)", n)
	}
	if n := fake.requests["/rpc/Shelly.GetStatus"]; n != 6 {
		t.Errorf("status requested %d times across 6 probes, want 6", n)
	}
	// GetConfig is on the cold path only, so it is fetched once and cached for the TTL.
	if n := fake.requests["/rpc/Shelly.GetConfig"]; n != 1 {
		t.Errorf("GetConfig requested %d times across 6 probes, want 1 (names cached for 6h)", n)
	}
}

// A device whose address was reassigned by DHCP must not have another device's readings
// attributed to it.
func TestProbeRefusesOnMACMismatch(t *testing.T) {
	body := strings.ReplaceAll(gen2ShellyBody, "A8032AB12345", "FFFFFFFFFFFF")
	_, dev := newFakeDevice(t, body, string(fixture(t, "gen2_plus1pm.json")))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	out, res := c.Probe(context.Background(), dev)

	if res.Success {
		t.Fatal("probe succeeded despite a MAC mismatch")
	}
	if res.Reason != probe.ReasonMACMismatch {
		t.Errorf("reason = %q, want mac_mismatch", res.Reason)
	}
	if len(out) != 0 {
		t.Errorf("emitted %d metrics under a mismatched identity, want none", len(out))
	}
}

// Gen1 is detected from the type key and scraped over the legacy endpoint.
func TestProbeDetectsGen1AndUsesLegacyEndpoint(t *testing.T) {
	fake, dev := newFakeDevice(t, gen1ShellyBody, string(fixture(t, "gen1_1pm.json")))
	fake.config = string(fixture(t, "gen1_settings.json"))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	out, res := c.Probe(context.Background(), dev)

	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if fake.requests["/status"] != 1 {
		t.Errorf("Gen1 should be scraped via /status, got requests %v", fake.requests)
	}
	if fake.requests["/rpc/Shelly.GetStatus"] != 0 {
		t.Error("Gen1 must not be asked for the Gen2 RPC endpoint")
	}
	// Gen1 names come from /settings, not Shelly.GetConfig.
	if fake.requests["/settings"] != 1 {
		t.Errorf("Gen1 names should be fetched from /settings, got requests %v", fake.requests)
	}
	if !contains(names(out), "espressif_switch_on") {
		t.Error("expected switch state from the Gen1 fixture")
	}
	if !hasLabel(out, "espressif_switch_on", "name", "Water Heater") {
		t.Errorf("switch:0 should carry name=Water Heater from /settings; got %v", labelDump(out, "espressif_switch_on"))
	}
}

// The mDNS instance name is a serial number to whoever reads the dashboard. Gen1's /shelly
// omits the name entirely, so without /settings every Gen1 device exports device_name as
// "shelly1-C45BBE7891BF" rather than the name its owner typed into the app.
func TestProbeGen1UsesTheConfiguredDeviceName(t *testing.T) {
	fake, dev := newFakeDevice(t, gen1ShellyBody, string(fixture(t, "gen1_1pm.json")))
	fake.config = string(fixture(t, "gen1_settings.json"))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	out, res := c.Probe(context.Background(), dev)

	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if !hasLabel(out, "espressif_device_info", "device_name", "My 1PM") {
		t.Errorf("device_name should come from the /settings name; got %v",
			labelDump(out, "espressif_device_info"))
	}
	if hasLabel(out, "espressif_device_info", "device_name", dev.Name) {
		t.Errorf("device_name fell back to the mDNS name %q despite a configured name", dev.Name)
	}
}

// The identity and names caches share a TTL but are written at different instants within a
// probe, so identity expires marginally first. If the configured name were applied only after
// a /settings fetch, that refresh would pin the mDNS name for another six hours — the very
// symptom this fix removes. Forgetting the identity reproduces the state exactly.
func TestProbeGen1DeviceNameSurvivesAnIdentityRefresh(t *testing.T) {
	fake, dev := newFakeDevice(t, gen1ShellyBody, string(fixture(t, "gen1_1pm.json")))
	fake.config = string(fixture(t, "gen1_settings.json"))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	if _, res := c.Probe(context.Background(), dev); !res.Success {
		t.Fatalf("first probe failed: %+v", res)
	}

	c.identities.forget(dev.ID)

	out, res := c.Probe(context.Background(), dev)
	if !res.Success {
		t.Fatalf("probe after identity refresh failed: %+v", res)
	}
	if !hasLabel(out, "espressif_device_info", "device_name", "My 1PM") {
		t.Errorf("device_name reverted to the mDNS name after an identity refresh; got %v",
			labelDump(out, "espressif_device_info"))
	}
	if n := fake.requests["/shelly"]; n != 2 {
		t.Errorf("/shelly requested %d times, want 2 (the identity really was refetched)", n)
	}
	if n := fake.requests["/settings"]; n != 1 {
		t.Errorf("/settings requested %d times, want 1 (the warm names entry supplied the name)", n)
	}
}

// A wrong password must cost the name label and nothing else: /status is unauthenticated on
// Gen1, so a device with no working credentials still exports its full readings today and must
// keep doing so.
func TestProbeGen1FallsBackToMDNSNameWhenSettingsIsUnauthorized(t *testing.T) {
	fake, dev := newFakeDevice(t, gen1ShellyBody, string(fixture(t, "gen1_1pm.json")))
	fake.configStatus = http.StatusUnauthorized

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	out, res := c.Probe(context.Background(), dev)

	if !res.Success {
		t.Fatalf("a 401 on /settings must not fail the probe: %+v", res)
	}
	for _, wanted := range []string{"espressif_switch_on", "espressif_power_watts"} {
		if !contains(names(out), wanted) {
			t.Errorf("missing %s; a 401 on /settings must not cost any readings", wanted)
		}
	}
	if !hasLabel(out, "espressif_device_info", "device_name", dev.Name) {
		t.Errorf("device_name should fall back to the mDNS name; got %v",
			labelDump(out, "espressif_device_info"))
	}
}

// fetch_config: false means exactly one request per scrape, so the device name is simply not
// available on Gen1. That is the documented cost of the flag, not a regression.
func TestProbeGen1WithFetchConfigDisabledKeepsTheMDNSName(t *testing.T) {
	fake, dev := newFakeDevice(t, gen1ShellyBody, string(fixture(t, "gen1_1pm.json")))
	fake.config = string(fixture(t, "gen1_settings.json"))

	cfg := config.Default().Shelly
	cfg.FetchConfig = false
	c := New(cfg, metrics.NewRegistry(), discardLogger())

	out, res := c.Probe(context.Background(), dev)
	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if n := fake.requests["/settings"]; n != 0 {
		t.Errorf("/settings requested %d times with fetch_config off, want 0", n)
	}
	if !hasLabel(out, "espressif_device_info", "device_name", dev.Name) {
		t.Errorf("device_name should fall back to the mDNS name; got %v",
			labelDump(out, "espressif_device_info"))
	}
}

// Gen2+ take the name from /shelly, which identity reads unauthenticated. Shelly.GetConfig must
// not override it: its sys.device.name holds the device id for an unnamed device, so sourcing
// it there would export "shellyplus1pm-a8032ab12345" in place of a real name.
func TestProbeGen2DeviceNameComesFromShelly(t *testing.T) {
	fake, dev := newFakeDevice(t, gen2ShellyBody, string(fixture(t, "gen2_plus1pm.json")))
	fake.config = string(fixture(t, "gen2_config.json"))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	out, res := c.Probe(context.Background(), dev)

	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if !hasLabel(out, "espressif_device_info", "device_name", "Lamp") {
		t.Errorf("device_name should come from /shelly; got %v",
			labelDump(out, "espressif_device_info"))
	}
}

// A device with no routable address fails fast rather than dialling nothing.
func TestProbeWithoutAnAddress(t *testing.T) {
	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	_, res := c.Probe(context.Background(), registry.Device{ID: "mac:a8032ab12345"})

	if res.Reason != probe.ReasonNoAddress {
		t.Errorf("reason = %q, want no_address", res.Reason)
	}
}

// A cancelled scrape must surface as a timeout, not as a decode failure, or the backoff
// ladder and the dashboards both point at the wrong cause.
func TestProbeCancellationIsATimeout(t *testing.T) {
	_, dev := newFakeDevice(t, gen2ShellyBody, `{}`)
	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, res := c.Probe(ctx, dev)
	if res.Reason != probe.ReasonTimeout {
		t.Errorf("reason = %q, want timeout", res.Reason)
	}
}

// hasLabel reports whether any sample of a family carries label=value.
func hasLabel(metricsOut []prometheus.Metric, family, label, value string) bool {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collected{metricsOut})
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return true
				}
			}
		}
	}
	return false
}

// labelDump renders a family's samples for failure messages.
func labelDump(metricsOut []prometheus.Metric, family string) []string {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collected{metricsOut})
	mfs, _ := reg.Gather()
	var out []string
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			out = append(out, m.String())
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
