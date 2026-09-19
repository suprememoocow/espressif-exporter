package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// Emitter accumulates const metrics for one probe.
//
// Values are collected into a slice rather than pushed to a channel, so the HTTP handler
// can scrape eagerly, time the work precisely, and treat a mid-probe failure as ordinary
// control flow instead of a half-written response.
//
// Callers name a family, never a help string or a metric type: both are properties of
// the family (see Family), and passing them per call is how they drift apart and fail
// the whole scrape at Gather time.
type Emitter struct {
	reg    *Registry
	labels Labels

	metrics *[]prometheus.Metric
	errs    *[]error
}

// NewEmitter builds an emitter bound to a device's base labels.
func NewEmitter(reg *Registry, labels Labels) *Emitter {
	metrics := make([]prometheus.Metric, 0, 64)
	var errs []error
	return &Emitter{reg: reg, labels: labels, metrics: &metrics, errs: &errs}
}

// For returns an emitter writing into the same output but with different base labels,
// for another component of the same device.
func (e *Emitter) For(labels Labels) *Emitter {
	return &Emitter{reg: e.reg, labels: labels, metrics: e.metrics, errs: e.errs}
}

// WithComponent is shorthand for switching component, id and name.
func (e *Emitter) WithComponent(component, id, name string) *Emitter {
	l := e.labels
	l.Component, l.ID, l.Name = component, id, name
	return e.For(l)
}

// Labels returns the emitter's current base labels.
func (e *Emitter) Labels() Labels { return e.labels }

// Registry exposes the family registry, so a collector that discovers families at
// runtime (the Shelly generic fallback) can declare them before emitting.
func (e *Emitter) Registry() *Registry { return e.reg }

// Metrics returns everything emitted so far.
func (e *Emitter) Metrics() []prometheus.Metric { return *e.metrics }

// Errs returns emission errors, which are always programming errors: an unregistered
// family, or the wrong number of label values.
func (e *Emitter) Errs() []error { return *e.errs }

// Value emits a reading into a family, using that family's declared type.
func (e *Emitter) Value(family string, value float64, extra ...string) {
	f, ok := e.reg.Lookup(family)
	if !ok {
		e.fail("metric family %q is not registered", family)
		return
	}
	kind := prometheus.GaugeValue
	if f.Counter {
		kind = prometheus.CounterValue
	}
	e.emit(f, kind, value, extra)
}

// Bool emits a 0/1 gauge.
func (e *Emitter) Bool(family string, value bool, extra ...string) {
	v := 0.0
	if value {
		v = 1
	}
	e.Value(family, v, extra...)
}

// Info emits a metric that is always 1, carrying metadata in its labels.
//
// Volatile metadata belongs here. Firmware version or compilation time on a value series
// would replace every series for that device on every OTA flash; on an info series a
// rollover costs one series per device, and dashboards join it back with group_left.
func (e *Emitter) Info(family string, extra ...string) {
	e.Value(family, 1, extra...)
}

// Enum emits every possible value, with 1 for the active one and 0 for the rest.
//
// Emitting only the active value looks tidier but is wrong: Prometheus never sees the
// previous value fall to 0, it merely goes stale after five minutes, so `... == 1`
// reports two active states for those five minutes after every transition.
func (e *Emitter) Enum(family, active string, all []string) {
	for _, v := range all {
		e.Bool(family, v == active, v)
	}
}

func (e *Emitter) emit(f Family, kind prometheus.ValueType, value float64, extra []string) {
	if len(extra) != len(f.Extra) {
		e.fail("metric family %q takes %d extra labels %v, got %d %v",
			f.Name, len(f.Extra), f.Extra, len(extra), extra)
		return
	}
	labels := e.labels
	if labels.DeviceClass == "" {
		// A collector-supplied class always wins; this only fills the gap for vendors
		// that have no such concept of their own.
		labels.DeviceClass = f.DeviceClass
	}
	values := labels.With(extra...)
	*e.metrics = append(*e.metrics,
		prometheus.MustNewConstMetric(e.reg.desc(f), kind, value, values...))
}

func (e *Emitter) fail(format string, args ...any) {
	*e.errs = append(*e.errs, fmt.Errorf(format, args...))
}
