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
	shelly   string
	status   string
	config   string
	requests map[string]int
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
	const gen1Shelly = `{"type":"SHSW-PM","mac":"A8032AB12345","auth":false,
	 "fw":"20230913-112003/v1.14.0-gcb84623","num_outputs":1}`
	fake, dev := newFakeDevice(t, gen1Shelly, string(fixture(t, "gen1_1pm.json")))
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
