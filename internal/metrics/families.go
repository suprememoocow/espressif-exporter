package metrics

import (
	"fmt"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Family describes one metric family: its help text, its type, and any label names it
// carries beyond BaseLabels.
//
// Help and type live here, not at the call site, because prometheus.Registry rejects a
// family whose members disagree on either. Two emitters passing slightly different help
// strings for espressif_power_watts would fail the whole scrape at Gather time, with an
// error naming a device rather than the code that drifted. Centralising them makes that
// class of bug unrepresentable.
//
// Label *names* are not enforced by the registry — it will happily emit ragged label
// sets — but they are declared here anyway, because a family whose members carry
// different labels breaks group_left joins and `sum by (...)` in ways that show up as a
// confusing dashboard rather than an error.
type Family struct {
	Name    string
	Help    string
	Counter bool
	Extra   []string

	// DeviceClass is the semantic class implied by this family, used when the collector
	// has none of its own. ESPHome reads device_class from the entity metadata, but
	// Shelly has no equivalent concept, so without this every Shelly series would carry
	// an empty device_class and the label would be useless as a cross-vendor filter.
	DeviceClass string
}

// Labels returns the family's full ordered label names.
func (f Family) Labels() []string { return withLabels(f.Extra...) }

// FQName returns the fully qualified metric name.
func (f Family) FQName() string { return MetricName(f.Name, f.Counter) }

// Registry interns family definitions for a process.
type Registry struct {
	mu       sync.RWMutex
	families map[string]Family
	descs    *DescCache
}

// NewRegistry builds a registry pre-populated with every family the unit table can
// produce, plus the curated cross-vendor families below.
func NewRegistry() *Registry {
	r := &Registry{families: map[string]Family{}, descs: NewDescCache()}
	for _, f := range builtinFamilies() {
		r.MustRegister(f)
	}
	return r
}

// Descs exposes the shared descriptor cache.
func (r *Registry) Descs() *DescCache { return r.descs }

// MustRegister adds a family, panicking on a conflicting redefinition. A conflict is
// always a programming error, and failing at startup beats failing at scrape time.
func (r *Registry) MustRegister(f Family) {
	if err := r.Register(f); err != nil {
		panic(err)
	}
}

// Register adds a family. Registering an identical definition twice is a no-op, so
// independent collectors may each declare the shared families they use.
func (r *Registry) Register(f Family) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.families[f.Name]; ok {
		if existing.Help != f.Help || existing.Counter != f.Counter {
			return fmt.Errorf(
				"metric family %q redefined: help %q/%q, counter %v/%v",
				f.Name, existing.Help, f.Help, existing.Counter, f.Counter)
		}
		return nil
	}
	r.families[f.Name] = f
	return nil
}

// Lookup returns a family definition.
func (r *Registry) Lookup(name string) (Family, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.families[name]
	return f, ok
}

// Names lists every registered family, sorted. Used by tests.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.families))
	for n := range r.families {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// desc resolves a family to a cached descriptor.
func (r *Registry) desc(f Family) *prometheus.Desc {
	return r.descs.Get(f.FQName(), f.Help, f.Labels())
}

// Curated cross-vendor family names. Both collectors emit into these, which is what lets
// one query cover the whole fleet regardless of brand.
const (
	FamilyPower         = "power_watts"
	FamilyApparentPower = "apparent_power_voltamperes"
	FamilyEnergy        = "energy_joules"
	FamilyVoltage       = "voltage_volts"
	FamilyCurrent       = "current_amperes"
	FamilyPowerFactor   = "power_factor"
	FamilyFrequency     = "frequency_hertz"
	FamilyTemperature   = "temperature_celsius"
	FamilyHumidity      = "humidity_ratio"
	FamilyBattery       = "battery_ratio"
	FamilyIlluminance   = "illuminance_lux"
	FamilyUptime        = "uptime_seconds"
	FamilyWiFiRSSI      = "wifi_rssi_dbm"

	FamilySwitchOn        = "switch_on"
	FamilyInputState      = "input_state"
	FamilyInputCounts     = "input_counts"
	FamilyCoverPosition   = "cover_position_ratio"
	FamilyLightOn         = "light_on"
	FamilyLightBrightness = "light_brightness_ratio"
	FamilyBinaryState     = "binary_state"

	FamilyDeviceInfo      = "device_info"
	FamilyEntityInfo      = "entity_info"
	FamilyTextInfo        = "text_info"
	FamilyEntityUnavail   = "entity_unavailable"
	FamilyEntityUpdatedTS = "entity_updated_timestamp_seconds"
	FamilyComponentError  = "component_error"
	FamilyUpdateAvailable = "update_available"
	FamilyRestartRequired = "restart_required"
	FamilyAuthRequired    = "auth_required"
	FamilyCloudConnected  = "cloud_connected"
	FamilyMQTTConnected   = "mqtt_connected"
	FamilyWiFiConnected   = "wifi_connected"
	FamilyRAMBytes        = "ram_bytes"
	FamilyFSBytes         = "fs_bytes"
	FamilyWebsocketConn   = "websocket_connected"
	FamilyEthernetUp      = "ethernet_up"
	FamilyExternalPower   = "external_power_present"
	FamilySmokeAlarm      = "smoke_alarm"
	FamilySmokeMuted      = "smoke_muted"
)

func builtinFamilies() []Family {
	fs := []Family{
		{Name: FamilyPower, Help: "Instantaneous active power.", DeviceClass: "power"},
		{Name: FamilyApparentPower, Help: "Instantaneous apparent power.", DeviceClass: "apparent_power"},
		{Name: FamilyEnergy, Help: "Cumulative active energy.", Counter: true, Extra: []string{"direction"}, DeviceClass: "energy"},
		{Name: FamilyVoltage, Help: "RMS voltage.", DeviceClass: "voltage"},
		{Name: FamilyCurrent, Help: "RMS current.", DeviceClass: "current"},
		{Name: FamilyPowerFactor, Help: "Power factor, from -1 to 1.", DeviceClass: "power_factor"},
		{Name: FamilyFrequency, Help: "Mains frequency.", DeviceClass: "frequency"},
		{Name: FamilyTemperature, Help: "Temperature. component=\"switch\" is the device's internal temperature, not ambient.", DeviceClass: "temperature"},
		{Name: FamilyHumidity, Help: "Relative humidity, as a ratio from 0 to 1.", DeviceClass: "humidity"},
		{Name: FamilyBattery, Help: "Battery charge, as a ratio from 0 to 1.", DeviceClass: "battery"},
		{Name: FamilyIlluminance, Help: "Ambient illuminance.", DeviceClass: "illuminance"},
		{Name: FamilyUptime, Help: "Time since the device last booted.", DeviceClass: "duration"},
		{Name: FamilyWiFiRSSI, Help: "WiFi signal strength. Logarithmic; do not average naively.", DeviceClass: "signal_strength"},

		{Name: FamilySwitchOn, Help: "Whether a switch output is on."},
		{Name: FamilyInputState, Help: "Whether an input is asserted."},
		{Name: FamilyInputCounts, Help: "Input pulse count.", Counter: true, Extra: []string{"counter"}},
		{Name: FamilyCoverPosition, Help: "Cover position, as a ratio from 0 (closed) to 1 (open).", Extra: []string{"target"}},
		{Name: FamilyLightOn, Help: "Whether a light is on."},
		{Name: FamilyLightBrightness, Help: "Light brightness, as a ratio from 0 to 1."},
		{Name: FamilyBinaryState, Help: "Binary sensor state."},

		{Name: FamilyDeviceInfo, Help: "Device identity and firmware metadata. Always 1.", Extra: []string{
			"mac", "model", "manufacturer", "fw_version", "gen", "ip", "transport", "device_name",
		}},
		{Name: FamilyEntityInfo, Help: "Entity metadata. Always 1.", Extra: []string{
			"unit", "state_class", "entity_category", "icon",
		}},
		{Name: FamilyTextInfo, Help: "A string-valued reading, carried as a label. Always 1.", Extra: []string{"value"}},
		{Name: FamilyEntityUnavail, Help: "Set when an entity has no usable reading.", Extra: []string{"reason"}},
		{Name: FamilyEntityUpdatedTS, Help: "When this entity's value last changed, in seconds since the epoch."},
		{Name: FamilyComponentError, Help: "Set while a component reports an error.", Extra: []string{"error"}},
		{Name: FamilyUpdateAvailable, Help: "Whether a firmware update is available.", Extra: []string{"channel"}},
		{Name: FamilyRestartRequired, Help: "Whether the device needs a restart to apply its configuration."},
		{Name: FamilyAuthRequired, Help: "Whether the device has authentication enabled."},
		{Name: FamilyCloudConnected, Help: "Whether the device is connected to the vendor cloud."},
		{Name: FamilyMQTTConnected, Help: "Whether the device is connected to its MQTT broker."},
		{Name: FamilyWiFiConnected, Help: "Whether the device is associated with a WiFi network."},
		{Name: FamilyRAMBytes, Help: "Device RAM.", Extra: []string{"state"}, DeviceClass: "data_size"},
		{Name: FamilyFSBytes, Help: "Device filesystem space.", Extra: []string{"state"}, DeviceClass: "data_size"},
		{Name: FamilyWebsocketConn, Help: "Whether the device's outbound websocket is connected."},
		{Name: FamilyEthernetUp, Help: "Whether the device's ethernet interface has an address."},
		{Name: FamilyExternalPower, Help: "Whether external power is present on a battery device."},
		{Name: FamilySmokeAlarm, Help: "Whether a smoke alarm is sounding."},
		{Name: FamilySmokeMuted, Help: "Whether a smoke alarm has been muted."},
	}

	// Every family the unit table can produce must exist too, since ESPHome sensors
	// reach them dynamically from whatever unit the YAML declares.
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.Name] = true
	}
	for _, q := range unitTable {
		if seen[q.Family] {
			continue
		}
		seen[q.Family] = true
		f := Family{Name: q.Family, Help: helpForUnitFamily(q.Family), Counter: q.Counter}
		if q.Counter {
			f.Extra = []string{"direction"}
		}
		fs = append(fs, f)
	}
	for _, name := range percentFamilies {
		for _, n := range []string{name, trimRatio(name) + "_percent"} {
			if !seen[n] {
				seen[n] = true
				fs = append(fs, Family{Name: n, Help: helpForUnitFamily(n)})
			}
		}
	}
	for _, n := range []string{"ratio", "percent", UnknownFamily} {
		if !seen[n] {
			seen[n] = true
			f := Family{Name: n, Help: helpForUnitFamily(n)}
			if n == UnknownFamily {
				// The only family carrying a unit label, which marks it as the
				// heterogeneous escape hatch rather than a real quantity.
				f.Extra = []string{"unit"}
			}
			fs = append(fs, f)
		}
	}
	return fs
}

func trimRatio(s string) string {
	if len(s) > len("_ratio") && s[len(s)-len("_ratio"):] == "_ratio" {
		return s[:len(s)-len("_ratio")]
	}
	return s
}

func helpForUnitFamily(family string) string {
	if family == UnknownFamily {
		return "A reading whose unit the exporter does not recognise; see the unit label."
	}
	return "Device reading, in the unit named by this metric."
}
