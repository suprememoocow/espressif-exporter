package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func shellyLabels() Labels {
	return Labels{
		Device: "mac:a8032ab1c2d3", Kind: "shelly", Component: "switch", ID: "0",
		Name: "Living Room Lamp", DeviceClass: "power",
	}
}

// staticCollector renders a pre-built metric slice, mirroring how /probe serves a probe.
type staticCollector struct{ metrics []prometheus.Metric }

// Describe is deliberately empty. An unchecked collector never collides on
// dynamically-built descriptors at registration time.
func (c staticCollector) Describe(chan<- *prometheus.Desc) {}
func (c staticCollector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c.metrics {
		ch <- m
	}
}

func gather(t *testing.T, e *Emitter) []*dto {
	t.Helper()
	if errs := e.Errs(); len(errs) > 0 {
		t.Fatalf("emitter reported errors: %v", errs)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(staticCollector{e.Metrics()})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make([]*dto, 0, len(mfs))
	for _, mf := range mfs {
		out = append(out, &dto{name: mf.GetName(), typ: mf.GetType().String(), n: len(mf.GetMetric())})
	}
	return out
}

type dto struct {
	name string
	typ  string
	n    int
}

func find(t *testing.T, got []*dto, name string) *dto {
	t.Helper()
	for _, d := range got {
		if d.name == name {
			return d
		}
	}
	names := make([]string, 0, len(got))
	for _, d := range got {
		names = append(names, d.name)
	}
	t.Fatalf("family %q not emitted; got %v", name, names)
	return nil
}

func TestEmitterCounterGetsTotalSuffixAndType(t *testing.T) {
	e := NewEmitter(NewRegistry(), shellyLabels())
	e.Value(FamilyEnergy, 4.7016e7, DirectionImport)

	d := find(t, gather(t, e), "espressif_energy_joules_total")
	if d.typ != "COUNTER" {
		t.Errorf("type = %s, want COUNTER", d.typ)
	}
}

func TestEmitterEnumEmitsEveryValue(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(Family{Name: "climate_mode", Help: "Active climate mode.", Extra: []string{"mode"}})

	e := NewEmitter(reg, shellyLabels())
	modes := []string{"off", "heat", "cool"}
	e.Enum("climate_mode", "heat", modes)

	d := find(t, gather(t, e), "espressif_climate_mode")
	if d.n != len(modes) {
		t.Errorf("emitted %d series, want one per possible mode (%d)", d.n, len(modes))
	}
}

// Help and type are family properties, so a caller cannot make them drift and fail the
// whole scrape at Gather time. A conflicting redefinition fails loudly at startup.
func TestRegistryRejectsConflictingRedefinition(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(Family{Name: FamilyPower, Help: "Instantaneous active power."}); err != nil {
		t.Errorf("re-registering an identical family should be a no-op, got %v", err)
	}
	if err := reg.Register(Family{Name: FamilyPower, Help: "something else"}); err == nil {
		t.Error("a conflicting help string should be rejected")
	}
	if err := reg.Register(Family{Name: FamilyPower, Help: "Instantaneous active power.", Counter: true}); err == nil {
		t.Error("a conflicting metric type should be rejected")
	}
}

// Supplying the wrong number of extra labels is a programming error, and must surface as
// an error rather than a silently malformed series.
func TestEmitterReportsLabelArityMistakes(t *testing.T) {
	e := NewEmitter(NewRegistry(), shellyLabels())
	e.Value(FamilyEnergy, 1) // energy requires a direction
	e.Value(FamilyPower, 1, "extra")
	e.Value("no_such_family", 1)

	if len(e.Errs()) != 3 {
		t.Errorf("got %d errors (%v), want 3", len(e.Errs()), e.Errs())
	}
	if len(e.Metrics()) != 0 {
		t.Errorf("emitted %d metrics despite errors, want 0", len(e.Metrics()))
	}
}

// Both vendors must land in one family with one label set; that is what makes a single
// cross-fleet query work.
func TestSharedFamilyAcrossVendors(t *testing.T) {
	reg := NewRegistry()

	e := NewEmitter(reg, Labels{
		Device: "mac:a8032ab1c2d3", Kind: "shelly", Component: "switch", ID: "0",
		Name: "Lamp", DeviceClass: "power",
	})
	e.Value(FamilyPower, 18.4)

	esphome := e.For(Labels{
		Device: "mac:a4cf129b3e70", Kind: "esphome", Component: "sensor", ID: "plug_power",
		Name: "Plug Power", DeviceClass: "power", Area: "Study",
	})
	esphome.Value(FamilyPower, 42.0)

	d := find(t, gather(t, e), "espressif_power_watts")
	if d.n != 2 {
		t.Errorf("got %d series in espressif_power_watts, want both vendors in one family", d.n)
	}
	if reg.Descs().Len() != 1 {
		t.Errorf("DescCache holds %d descriptors, want 1 reused", reg.Descs().Len())
	}
}

// promlint mechanically catches unit-suffix and _total violations across every family
// the exporter can produce, which is far more reliable than catching them in review.
func TestEveryRegisteredFamilyPassesPromlint(t *testing.T) {
	reg := NewRegistry()
	e := NewEmitter(reg, shellyLabels())

	for _, name := range reg.Names() {
		f, _ := reg.Lookup(name)
		extra := make([]string, len(f.Extra))
		for i := range extra {
			extra[i] = "x"
		}
		e.Value(name, 1, extra...)
	}
	if errs := e.Errs(); len(errs) > 0 {
		t.Fatalf("emitter errors: %v", errs)
	}

	problems, err := testutil.CollectAndLint(staticCollector{e.Metrics()})
	if err != nil {
		t.Fatalf("CollectAndLint: %v", err)
	}
	var bad []string
	for _, p := range problems {
		// promlint objects to any metric whose name lacks a recognised unit; our
		// boolean and info families are legitimately unitless.
		if strings.Contains(p.Text, "no unit suffix") || strings.Contains(p.Text, "use base unit") {
			continue
		}
		bad = append(bad, p.Metric+": "+p.Text)
	}
	for _, b := range bad {
		t.Error("promlint: " + b)
	}
}

func TestDescCacheIsConcurrencySafe(t *testing.T) {
	c := NewDescCache()
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 100 {
				c.Get("espressif_power_watts", "help", BaseLabels)
			}
		}()
	}
	for range 8 {
		<-done
	}
	if c.Len() != 1 {
		t.Errorf("DescCache holds %d descriptors, want 1", c.Len())
	}
}

// Shelly has no device_class concept of its own, so without a family-derived default
// every Shelly series would carry an empty device_class and the label would be useless
// as a cross-vendor filter.
func TestDeviceClassDefaultsFromTheFamily(t *testing.T) {
	reg := NewRegistry()

	shelly := NewEmitter(reg, Labels{Device: "d", Kind: "shelly", Component: "switch", ID: "0"})
	shelly.Value(FamilyPower, 18.4)

	got := gather(t, shelly)
	d := find(t, got, "espressif_power_watts")
	if d.n != 1 {
		t.Fatalf("expected one series, got %d", d.n)
	}
	if !hasLabel(t, shelly, "device_class", "power") {
		t.Error("device_class should default to the family's class")
	}

	// A collector that knows better always wins.
	esphome := NewEmitter(reg, Labels{
		Device: "e", Kind: "esphome", Component: "sensor", ID: "x", DeviceClass: "apparent_power",
	})
	esphome.Value(FamilyPower, 1)
	if !hasLabel(t, esphome, "device_class", "apparent_power") {
		t.Error("an explicit device_class must not be overwritten")
	}
}

func hasLabel(t *testing.T, e *Emitter, name, value string) bool {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(staticCollector{e.Metrics()})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == name && l.GetValue() == value {
					return true
				}
			}
		}
	}
	return false
}
