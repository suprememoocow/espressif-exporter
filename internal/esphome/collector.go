package esphome

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// Collector renders a device's cached state as Prometheus metrics.
type Collector struct {
	mgr      *Manager
	cfg      config.ESPHome
	metrics  config.Metrics
	families *metrics.Registry
	log      *slog.Logger
	clock    func() time.Time
}

// NewCollector builds the collector.
func NewCollector(
	mgr *Manager, cfg config.ESPHome, metricsCfg config.Metrics,
	families *metrics.Registry, log *slog.Logger,
) *Collector {
	registerESPHomeFamilies(families)
	return &Collector{
		mgr: mgr, cfg: cfg, metrics: metricsCfg, families: families,
		log: log.With("component", "esphome"), clock: time.Now,
	}
}

// Kind implements probe.Prober.
func (c *Collector) Kind() string { return "esphome" }

// Probe renders the cached snapshot. It performs no network I/O: the native API is
// push-based, so the connection manager already holds the current state.
func (c *Collector) Probe(
	_ context.Context, dev registry.Device,
) ([]prometheus.Metric, probe.Result) {
	snap, ok := c.mgr.snapshotOf(dev.ID)
	if !ok {
		return nil, probe.Fail(probe.ReasonNotConnected, 0)
	}

	base := metrics.Labels{Device: dev.ID, Kind: "esphome"}
	e := metrics.NewEmitter(c.families, base)

	c.emitDeviceMetrics(e, snap)

	// With no live connection we have no basis to claim anything about the device's
	// current state, so no entity values are emitted at all and Prometheus's own
	// staleness handling takes over. The alternative — serving the last known values —
	// shows a frozen temperature for a device unplugged a week ago.
	if !snap.Connected {
		return e.Metrics(), probe.Fail(probe.ReasonNotConnected, 0)
	}

	now := c.clock()
	for key, ent := range snap.Entities {
		state, hasState := snap.States[key]
		c.emitEntity(e, ent, state, hasState, now)
	}

	if errs := e.Errs(); len(errs) > 0 {
		c.log.Error("metric emission errors", "device", dev.ID, "errors", errs)
	}
	return e.Metrics(), probe.Success(0)
}

func (c *Collector) emitDeviceMetrics(e *metrics.Emitter, snap snapshot) {
	info := snap.Info
	e.Info(metrics.FamilyDeviceInfo,
		info.GetMacAddress(),
		info.GetModel(),
		info.GetManufacturer(),
		info.GetEsphomeVersion(),
		"", // gen is a Shelly concept
		"",
		snap.Transport,
		info.GetFriendlyName(),
	)
	e.Info(familyESPHomeBuild, info.GetCompilationTime(), info.GetProjectName(), info.GetProjectVersion())

	if !snap.ConnectedSince.IsZero() {
		e.Value(familyConnectedSince, float64(snap.ConnectedSince.Unix()))
	}
	if !snap.LastMessageAt.IsZero() {
		e.Value(familyLastMessage, float64(snap.LastMessageAt.Unix()))
	}
	if snap.PingSeconds > 0 {
		// The single best signal for "is this node wedged, or is the wifi bad".
		e.Value(familyPing, snap.PingSeconds)
	}
	e.Value(familyEntityCount, float64(len(snap.Entities)))
	e.Value(familyStateUpdates, float64(snap.StateUpdates))
	e.Value(familyEntityGeneration, float64(snap.EntityGeneration))
	e.Value(familyConnectAttempts, float64(snap.ConnectAttempts))
	if snap.Truncated {
		e.Bool(familyEntitiesTruncated, true)
	}
	for reason, n := range snap.ConnectFailures {
		e.Value(familyConnectFailures, float64(n), reason)
	}
}

func (c *Collector) emitEntity(
	e *metrics.Emitter, ent *entityRecord, state stateRecord, hasState bool, now time.Time,
) {
	labels := metrics.Labels{
		Device:      e.Labels().Device,
		Kind:        "esphome",
		Component:   ent.Domain,
		ID:          ent.ObjectID,
		Name:        ent.Name,
		DeviceClass: ent.DeviceClass,
	}
	ee := e.For(labels)

	ee.Info(metrics.FamilyEntityInfo,
		ent.Unit, stateClassName(ent.StateClass), categoryName(ent.Category), ent.Icon)

	if !hasState {
		// Distinguished from a faulted sensor: this entity exists but has simply never
		// reported, which on a slow sensor is normal shortly after a reconnect.
		ee.Value(metrics.FamilyEntityUnavail, 1, "never_reported")
		return
	}
	if state.Missing {
		// A NaN or an explicit missing_state means no reading. Exporting 0 would be a
		// plausible-looking lie and exporting NaN would silently poison avg, sum and rate.
		ee.Value(metrics.FamilyEntityUnavail, 1, "missing_state")
		ee.Value(metrics.FamilyEntityUpdatedTS, float64(state.UpdatedAt.Unix()))
		return
	}

	// Freshness is exported rather than enforced. Suppressing values past a threshold
	// would delete legitimately slow sensors — a total_daily_energy updates hourly, a
	// WiFi SSID once per boot — and would break rate() on energy counters exactly when
	// it matters. The operator thresholds this instead.
	ee.Value(metrics.FamilyEntityUpdatedTS, float64(state.UpdatedAt.Unix()))
	if c.cfg.MaxStateAge > 0 && now.Sub(state.UpdatedAt) > c.cfg.MaxStateAge {
		ee.Value(metrics.FamilyEntityUnavail, 1, "stale")
		return
	}

	switch ent.Domain {
	case "sensor", "number":
		c.emitNumeric(ee, ent, state)
	case "binary_sensor":
		ee.Value(metrics.FamilyBinaryState, state.Value)
	case "switch":
		ee.Value(metrics.FamilySwitchOn, state.Value)
	case "light":
		ee.Value(metrics.FamilyLightOn, state.Value)
	case "fan":
		ee.Value(familyFanOn, state.Value)
	case "cover":
		ee.Value(metrics.FamilyCoverPosition, state.Value, "current")
	case "lock":
		ee.Value(familyLockState, state.Value)
	case "climate":
		ee.Value(metrics.FamilyTemperature, state.Value)
		if len(ent.Modes) > 0 {
			ee.Enum(familyClimateMode, state.Text, ent.Modes)
		}
	case "select":
		if len(ent.Options) > 0 {
			ee.Enum(familySelectOption, state.Text, ent.Options)
		}
	case "text_sensor":
		c.emitText(ee, state)
	case "button":
		// No state exists in the protocol; entity_info above records its presence.
	}
}

func (c *Collector) emitNumeric(e *metrics.Emitter, ent *entityRecord, state stateRecord) {
	q := metrics.ResolveUnit(ent.Unit, ent.DeviceClass, c.metrics.PercentAsRatio)
	value := q.Scale(state.Value)

	if q.Unknown {
		// The one family carrying a unit label, which marks it as the heterogeneous
		// escape hatch rather than a real quantity.
		e.Value(q.Family, value, ent.Unit)
		return
	}
	// The unit table's counter flag reflects the unit; the entity's state_class is the
	// authority on whether this particular reading is monotonic.
	if metrics.IsCounter(ent.StateClass) && q.Counter {
		e.Value(q.Family, value, metrics.DirectionImport)
		return
	}
	if q.Counter {
		// A non-monotonic reading in a counter family (an energy gauge, say) would be
		// mistyped, so it goes to the generic family instead of lying about its type.
		e.Value(metrics.UnknownFamily, value, ent.Unit)
		return
	}
	e.Value(q.Family, value)
}

func (c *Collector) emitText(e *metrics.Emitter, state stateRecord) {
	if !c.metrics.TextSensors {
		return
	}
	value := state.Text
	if n := c.metrics.MaxTextValueBytes; n > 0 && len(value) > n {
		value = truncateUTF8(value, n)
	}
	e.Info(metrics.FamilyTextInfo, value)
}

// truncateUTF8 cuts at a rune boundary so a label never contains a partial character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func stateClassName(sc int32) string {
	switch sc {
	case metrics.StateClassMeasurement:
		return "measurement"
	case metrics.StateClassTotalIncreasing:
		return "total_increasing"
	case metrics.StateClassTotal:
		return "total"
	default:
		return ""
	}
}

func categoryName(c int32) string {
	switch c {
	case 1:
		return "config"
	case 2:
		return "diagnostic"
	default:
		return ""
	}
}
