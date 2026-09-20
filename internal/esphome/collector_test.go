package esphome

import (
	"net/netip"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/richard87/esphome-apiclient/pb"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
)

// collected adapts emitted metrics into a prometheus.Collector, so a registry can gather
// them and hand back parsed names and labels rather than rendered text.
type collected struct{ metrics []prometheus.Metric }

func (c collected) Describe(chan<- *prometheus.Desc) {}
func (c collected) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c.metrics {
		ch <- m
	}
}

func testSnapshot(addr netip.Addr) snapshot {
	return snapshot{
		Connected: true,
		Info: &pb.DeviceInfoResponse{
			Name:           "bedroom",
			MacAddress:     "a4:cf:12:9b:3e:70",
			EsphomeVersion: "2026.3.2",
			Model:          "esp32-c3-devkitm-1",
			Manufacturer:   "Espressif",
			FriendlyName:   "Bedroom Plug",
		},
		Transport: "noise",
		Addr:      addr,
	}
}

// deviceInfoLabels emits device metrics for a snapshot and returns the single
// espressif_device_info sample as a label map.
func deviceInfoLabels(t *testing.T, snap snapshot) map[string]string {
	t.Helper()

	families := metrics.NewRegistry()
	cfg := config.Default()
	c := NewCollector(nil, cfg.ESPHome, cfg.Metrics, families, discardLogger())

	e := metrics.NewEmitter(families, metrics.Labels{Device: "mac:a4cf129b3e70", Kind: "esphome"})
	c.emitDeviceMetrics(e, snap)
	if errs := e.Errs(); len(errs) > 0 {
		t.Fatalf("emission errors: %v", errs)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collected{e.Metrics()})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, mf := range mfs {
		if mf.GetName() != "espressif_device_info" {
			continue
		}
		if n := len(mf.GetMetric()); n != 1 {
			t.Fatalf("espressif_device_info has %d samples, want 1", n)
		}
		labels := map[string]string{}
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		return labels
	}

	t.Fatal("espressif_device_info was not emitted")
	return nil
}

// Prometheus targets devices by a stable ID, so instance never carries an address, and
// device_info is the documented way back to one (see internal/server/sd.go). Shelly
// populated it and ESPHome did not, which left half the fleet with no address anywhere
// in the metrics.
func TestDeviceInfoCarriesTheConnectionAddress(t *testing.T) {
	got := deviceInfoLabels(t, testSnapshot(netip.MustParseAddr("192.168.1.50")))

	if got["ip"] != "192.168.1.50" {
		t.Errorf("ip = %q, want 192.168.1.50", got["ip"])
	}

	// device_info takes eight positional label values, so a value inserted in the wrong
	// position shifts every later one silently. These pin the neighbours.
	for _, tc := range []struct{ label, want string }{
		{"gen", ""}, // a Shelly concept, always empty here
		{"transport", "noise"},
		{"device_name", "Bedroom Plug"},
		{"model", "esp32-c3-devkitm-1"},
		{"manufacturer", "Espressif"},
		{"fw_version", "2026.3.2"},
	} {
		if got[tc.label] != tc.want {
			t.Errorf("%s = %q, want %q", tc.label, got[tc.label], tc.want)
		}
	}
}

// netip.Addr's zero value stringifies to "invalid IP", which would read as a real value
// on a dashboard. An empty label is dropped on ingest, which is the honest answer.
func TestDeviceInfoOmitsAnUnsetAddress(t *testing.T) {
	got := deviceInfoLabels(t, testSnapshot(netip.Addr{}))

	if got["ip"] != "" {
		t.Errorf("ip = %q, want empty", got["ip"])
	}
}
