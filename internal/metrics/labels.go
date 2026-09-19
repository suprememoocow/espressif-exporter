// Package metrics defines the exporter's metric names, units and label sets.
//
// ESPHome and Shelly deliberately share one namespace and one label vocabulary, so a
// single query covers all power monitoring regardless of which brand the plug is.
package metrics

// Namespace prefixes every metric. Both vendors share it; the kind label distinguishes
// them where that matters.
const Namespace = "espressif"

// ExporterNamespace prefixes the exporter's own metrics, served on /metrics.
const ExporterNamespace = "espressif_exporter"

// BaseLabels is the fixed label set carried by every value series.
//
// Every emitter supplies all of these, using an empty string where a label does not
// apply. Prometheus treats foo="" as equivalent to foo being absent when matching, so
// this costs nothing semantically.
//
// Note that the client library does not enforce this: it will happily emit a family
// whose members carry different labels. The cost of ragged labels is not an error but a
// silently broken dashboard, because group_left joins and `sum by (...)` stop lining up.
// Help and metric type, by contrast, *are* enforced at Gather time, which is why they
// are properties of a Family rather than arguments at the call site.
var BaseLabels = []string{
	"device",       // stable device ID; never an IP
	"kind",         // esphome | shelly
	"component",    // ESPHome entity domain, or Shelly component type
	"id",           // ESPHome object_id, or Shelly component instance index
	"name",         // human-facing name; 1:1 with (component, id), so no extra series
	"device_class", // bounded vocabulary, the most useful filter
	"phase",        // a|b|c|n|total for three-phase Shelly energy meters, else empty
	"area",         // ESPHome area, else empty
}

// Labels carries the values for BaseLabels, in order.
type Labels struct {
	Device      string
	Kind        string
	Component   string
	ID          string
	Name        string
	DeviceClass string
	Phase       string
	Area        string
}

// Values renders the label values in BaseLabels order.
func (l Labels) Values() []string {
	return []string{l.Device, l.Kind, l.Component, l.ID, l.Name, l.DeviceClass, l.Phase, l.Area}
}

// With appends extra label values after the base set, for families that carry one more
// dimension (energy direction, enum member, and so on).
func (l Labels) With(extra ...string) []string {
	return append(l.Values(), extra...)
}

// withLabels appends extra label names to the base set.
func withLabels(extra ...string) []string {
	out := make([]string, 0, len(BaseLabels)+len(extra))
	out = append(out, BaseLabels...)
	return append(out, extra...)
}

// Phase values for polyphase energy meters. An empty phase on a single-phase device is
// invisible at query time, which is what lets three-phase em:0 and single-phase
// switch:0 share one metric family.
const (
	PhaseA     = "a"
	PhaseB     = "b"
	PhaseC     = "c"
	PhaseN     = "n"
	PhaseTotal = "total"
)

// Energy direction values.
const (
	DirectionImport = "import"
	DirectionExport = "export"
)
