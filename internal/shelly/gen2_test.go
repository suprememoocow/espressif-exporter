package shelly

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
)

func testCollector(t *testing.T) (*Collector, *metrics.Registry) {
	t.Helper()
	reg := metrics.NewRegistry()
	c := New(config.Default().Shelly, reg, discardLogger())
	return c, reg
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type collected struct{ metrics []prometheus.Metric }

func (c collected) Describe(chan<- *prometheus.Desc) {}
func (c collected) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c.metrics {
		ch <- m
	}
}

// series renders every emitted sample as "name{k=v,...} value", sorted, so a test can
// assert on exact label sets rather than on substrings.
func series(t *testing.T, e *metrics.Emitter) []string {
	t.Helper()
	if errs := e.Errs(); len(errs) > 0 {
		t.Fatalf("emitter errors: %v", errs)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collected{e.Metrics()})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var out []string
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				if l.GetValue() == "" {
					continue // an empty label is invisible at query time
				}
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			sort.Strings(labels)
			out = append(out, mf.GetName()+"{"+strings.Join(labels, ",")+"} "+fmtValue(m))
		}
	}
	sort.Strings(out)
	return out
}

func fmtValue(m *dto.Metric) string {
	switch {
	case m.Gauge != nil:
		return trimFloat(m.Gauge.GetValue())
	case m.Counter != nil:
		return trimFloat(m.Counter.GetValue())
	default:
		return "?"
	}
}

// trimFloat renders a value the way the exposition format does. Trailing zeros are
// only ever stripped after a decimal point; doing it unconditionally would turn
// 720000000 into 72.
func trimFloat(f float64) string {
	s := formatFloat(f)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

func want(t *testing.T, got []string, wanted ...string) {
	t.Helper()
	index := map[string]bool{}
	for _, g := range got {
		index[g] = true
	}
	for _, w := range wanted {
		if !index[w] {
			t.Errorf("missing series:\n  want %s\n  got:\n    %s", w, strings.Join(got, "\n    "))
		}
	}
}

func notPresent(t *testing.T, got []string, prefix string) {
	t.Helper()
	for _, g := range got {
		if strings.HasPrefix(g, prefix) {
			t.Errorf("unexpected series %s", g)
		}
	}
}

func emitterFor(reg *metrics.Registry, device string) *metrics.Emitter {
	return metrics.NewEmitter(reg, metrics.Labels{Device: device, Kind: "shelly"})
}

func TestGen2Plus1PM(t *testing.T) {
	c, reg := testCollector(t)
	e := emitterFor(reg, "mac:a8032ab1c2d3")

	if err := c.decodeGen2Status(e, fixture(t, "gen2_plus1pm.json")); err != nil {
		t.Fatal(err)
	}
	got := series(t, e)

	want(t, got,
		`espressif_switch_on{component=switch,device=mac:a8032ab1c2d3,id=0,kind=shelly} 1`,
		`espressif_power_watts{component=switch,device=mac:a8032ab1c2d3,device_class=power,id=0,kind=shelly} 18.4`,
		`espressif_voltage_volts{component=switch,device=mac:a8032ab1c2d3,device_class=voltage,id=0,kind=shelly} 241.3`,
		`espressif_current_amperes{component=switch,device=mac:a8032ab1c2d3,device_class=current,id=0,kind=shelly} 0.082`,
		`espressif_power_factor{component=switch,device=mac:a8032ab1c2d3,device_class=power_factor,id=0,kind=shelly} 0.93`,
		`espressif_frequency_hertz{component=switch,device=mac:a8032ab1c2d3,device_class=frequency,id=0,kind=shelly} 50.1`,
		// 13060 Wh * 3600 = 4.7016e7 J
		`espressif_energy_joules_total{component=switch,device=mac:a8032ab1c2d3,device_class=energy,direction=import,id=0,kind=shelly} 47016000`,
		`espressif_temperature_celsius{component=switch,device=mac:a8032ab1c2d3,device_class=temperature,id=0,kind=shelly} 44.6`,
		`espressif_uptime_seconds{component=sys,device=mac:a8032ab1c2d3,device_class=duration,kind=shelly} 918442`,
		`espressif_wifi_rssi_dbm{component=wifi,device=mac:a8032ab1c2d3,device_class=signal_strength,kind=shelly} -61`,
		`espressif_wifi_connected{component=wifi,device=mac:a8032ab1c2d3,kind=shelly} 1`,
		`espressif_cloud_connected{component=cloud,device=mac:a8032ab1c2d3,kind=shelly} 0`,
		`espressif_mqtt_connected{component=mqtt,device=mac:a8032ab1c2d3,kind=shelly} 1`,
		`espressif_ram_bytes{component=sys,device=mac:a8032ab1c2d3,device_class=data_size,kind=shelly,state=free} 96016`,
		// Both channels every scrape, so an `== 1` alert resolves cleanly.
		`espressif_update_available{channel=stable,component=sys,device=mac:a8032ab1c2d3,kind=shelly} 1`,
		`espressif_update_available{channel=beta,component=sys,device=mac:a8032ab1c2d3,kind=shelly} 0`,
	)

	// ble and ws carry nothing worth exporting and must not reach the raw namespace.
	notPresent(t, got, "espressif_raw_ble")
	if n := len(c.UnknownComponents()); n != 0 {
		t.Errorf("unknown components = %v, want none for a fully curated fixture", c.UnknownComponents())
	}
}

// A null reading means the sensor is unavailable. Emitting 0 for phase C here would
// claim a de-energised phase is drawing exactly no power, which looks plausible and is
// wrong.
func TestGen2NullPhaseIsOmittedNotZeroed(t *testing.T) {
	c, reg := testCollector(t)
	e := emitterFor(reg, "mac:aabbccddeeff")

	if err := c.decodeGen2Status(e, fixture(t, "gen2_em3.json")); err != nil {
		t.Fatal(err)
	}
	got := series(t, e)

	want(t, got,
		`espressif_power_watts{component=em,device=mac:aabbccddeeff,device_class=power,id=0,kind=shelly,phase=a} 280.5`,
		`espressif_power_watts{component=em,device=mac:aabbccddeeff,device_class=power,id=0,kind=shelly,phase=b} 190.2`,
		`espressif_power_watts{component=em,device=mac:aabbccddeeff,device_class=power,id=0,kind=shelly,phase=total} 470.7`,
		`espressif_current_amperes{component=em,device=mac:aabbccddeeff,device_class=current,id=0,kind=shelly,phase=n} 0.4`,
		`espressif_component_error{component=em,device=mac:aabbccddeeff,error=phase_sequence,id=0,kind=shelly} 1`,
	)
	for _, g := range got {
		if strings.Contains(g, "phase=c") {
			t.Errorf("phase C reported null but produced a series: %s", g)
		}
	}

	// emdata is emitted under component="em" so energy joins power on (component,id,phase).
	want(t, got,
		`espressif_energy_joules_total{component=em,device=mac:aabbccddeeff,device_class=energy,direction=import,id=0,kind=shelly,phase=total} 720000000`,
	)
}

// An uncurated component is quarantined rather than dropped, and counted so an operator
// knows when to promote it.
func TestGen2UnknownComponentIsQuarantined(t *testing.T) {
	c, reg := testCollector(t)
	e := emitterFor(reg, "mac:aabbccddeeff")

	if err := c.decodeGen2Status(e, fixture(t, "gen2_em3.json")); err != nil {
		t.Fatal(err)
	}
	got := series(t, e)

	want(t, got,
		`espressif_raw_newfangled_widgets{component=newfangled,device=mac:aabbccddeeff,id=0,kind=shelly} 42`,
		`espressif_raw_newfangled_sub_depth{component=newfangled,device=mac:aabbccddeeff,id=0,kind=shelly} 7`,
	)
	// Timestamps look like measurements and are not.
	notPresent(t, got, "espressif_raw_newfangled_ts")

	if c.UnknownComponents()["newfangled"] != 1 {
		t.Errorf("unknown component counter = %v, want newfangled counted once", c.UnknownComponents())
	}
}

func TestGen2GenericFallbackCanBeDisabled(t *testing.T) {
	reg := metrics.NewRegistry()
	cfg := config.Default().Shelly
	cfg.GenericFallback = false
	c := New(cfg, reg, discardLogger())

	e := emitterFor(reg, "mac:aabbccddeeff")
	if err := c.decodeGen2Status(e, fixture(t, "gen2_em3.json")); err != nil {
		t.Fatal(err)
	}
	notPresent(t, series(t, e), "espressif_raw_")

	// It is still counted, so the feedback loop survives turning the walk off.
	if c.UnknownComponents()["newfangled"] != 1 {
		t.Error("unknown components should still be counted when the fallback is off")
	}
}
