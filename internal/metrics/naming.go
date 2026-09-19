package metrics

import (
	"strings"
	"unicode"
)

// Quantity is the Prometheus family a reading belongs to, plus how to get there.
type Quantity struct {
	// Family is the name after the namespace, for example "power_watts". It already
	// encodes the unit, which is why unit is not a label on these families.
	Family string

	// Scale converts the device's reported value into the family's unit.
	Scale func(float64) float64

	// Counter marks a monotonic family, whose name gains the _total suffix.
	Counter bool

	// Unknown marks the escape-hatch family, which does carry a unit label.
	Unknown bool
}

func identity(v float64) float64 { return v }
func mul(f float64) func(float64) float64 {
	return func(v float64) float64 { return v * f }
}
func div(f float64) func(float64) float64 {
	return func(v float64) float64 { return v / f }
}

// Conversion constants, named so the call sites read as physics rather than magic.
const (
	joulesPerWattHour   = 3600
	joulesPerWattMinute = 60
)

// WattHoursToJoules converts a Shelly Gen2 aenergy.total or a Gen1 emeters[].total.
func WattHoursToJoules(wh float64) float64 { return wh * joulesPerWattHour }

// WattMinutesToJoules converts a Shelly Gen1 meters[].total.
//
// Gen1 firmware is internally inconsistent about energy units: meters[] reports
// watt-minutes while emeters[] reports watt-hours. Confusing the two is a silent 60x
// error that still looks plausible on a dashboard, so each call site names its source
// unit and the conversions are pinned by tests.
func WattMinutesToJoules(wmin float64) float64 { return wmin * joulesPerWattMinute }

// KilowattHoursToJoules converts a kWh reading.
func KilowattHoursToJoules(kwh float64) float64 { return kwh * 1000 * joulesPerWattHour }

// unitTable maps a device-reported unit to its Prometheus family.
//
// Energy is stored in joules because that is the base unit the Prometheus naming guide
// names explicitly and the one node_exporter uses (node_rapl_package_joules_total).
// Kilowatt-hours read better on a bill, but storing them would make a mixed dashboard
// unit-inconsistent; Grafana formats joules as kWh with a unit setting and a division.
// Precision is not a concern: 10,000 kWh is 3.6e10 J, and float64 is exact past 9e15.
var unitTable = map[string]Quantity{
	// Temperature.
	"°c": {Family: "temperature_celsius", Scale: identity},
	"c":  {Family: "temperature_celsius", Scale: identity},
	"°f": {Family: "temperature_celsius", Scale: func(v float64) float64 { return (v - 32) * 5 / 9 }},
	"k":  {Family: "temperature_celsius", Scale: func(v float64) float64 { return v - 273.15 }},

	// Power and energy.
	"w":   {Family: "power_watts", Scale: identity},
	"kw":  {Family: "power_watts", Scale: mul(1000)},
	"mw":  {Family: "power_watts", Scale: div(1000)},
	"va":  {Family: "apparent_power_voltamperes", Scale: identity},
	"var": {Family: "reactive_power_voltamperes_reactive", Scale: identity},
	"wh":  {Family: "energy_joules", Scale: WattHoursToJoules, Counter: true},
	"kwh": {Family: "energy_joules", Scale: KilowattHoursToJoules, Counter: true},
	"mwh": {Family: "energy_joules", Scale: func(v float64) float64 { return v * 1e6 * joulesPerWattHour }, Counter: true},
	"j":   {Family: "energy_joules", Scale: identity, Counter: true},
	"kj":  {Family: "energy_joules", Scale: mul(1000), Counter: true},

	// Electrical.
	"v":       {Family: "voltage_volts", Scale: identity},
	"mv":      {Family: "voltage_volts", Scale: div(1000)},
	"kv":      {Family: "voltage_volts", Scale: mul(1000)},
	"a":       {Family: "current_amperes", Scale: identity},
	"ma":      {Family: "current_amperes", Scale: div(1000)},
	"Ω":       {Family: "resistance_ohms", Scale: identity},
	"ω":       {Family: "resistance_ohms", Scale: identity},
	"ohm":     {Family: "resistance_ohms", Scale: identity},
	"ohms":    {Family: "resistance_ohms", Scale: identity},
	"kohm":    {Family: "resistance_ohms", Scale: mul(1000)},
	"volt":    {Family: "voltage_volts", Scale: identity},
	"volts":   {Family: "voltage_volts", Scale: identity},
	"amp":     {Family: "current_amperes", Scale: identity},
	"amps":    {Family: "current_amperes", Scale: identity},
	"watt":    {Family: "power_watts", Scale: identity},
	"watts":   {Family: "power_watts", Scale: identity},
	"sec":     {Family: "duration_seconds", Scale: identity},
	"secs":    {Family: "duration_seconds", Scale: identity},
	"bytes":   {Family: "bytes", Scale: identity},
	"celsius": {Family: "temperature_celsius", Scale: identity},

	// Pressure.
	"pa":   {Family: "pressure_pascals", Scale: identity},
	"hpa":  {Family: "pressure_pascals", Scale: mul(100)},
	"kpa":  {Family: "pressure_pascals", Scale: mul(1000)},
	"mbar": {Family: "pressure_pascals", Scale: mul(100)},
	"bar":  {Family: "pressure_pascals", Scale: mul(1e5)},
	"psi":  {Family: "pressure_pascals", Scale: mul(6894.757)},
	"inhg": {Family: "pressure_pascals", Scale: mul(3386.389)},

	// Time.
	"s":   {Family: "duration_seconds", Scale: identity},
	"ms":  {Family: "duration_seconds", Scale: div(1000)},
	"µs":  {Family: "duration_seconds", Scale: div(1e6)},
	"us":  {Family: "duration_seconds", Scale: div(1e6)},
	"min": {Family: "duration_seconds", Scale: mul(60)},
	"h":   {Family: "duration_seconds", Scale: mul(3600)},
	"d":   {Family: "duration_seconds", Scale: mul(86400)},

	// Signal strength. Logarithmic, so deliberately not normalised or averaged.
	"db":  {Family: "signal_dbm", Scale: identity},
	"dbm": {Family: "signal_dbm", Scale: identity},

	// Data.
	"b":   {Family: "bytes", Scale: identity},
	"kb":  {Family: "bytes", Scale: mul(1000)},
	"mb":  {Family: "bytes", Scale: mul(1e6)},
	"gb":  {Family: "bytes", Scale: mul(1e9)},
	"kib": {Family: "bytes", Scale: mul(1024)},
	"mib": {Family: "bytes", Scale: mul(1024 * 1024)},
	"gib": {Family: "bytes", Scale: mul(1024 * 1024 * 1024)},

	// Length, volume, flow.
	"m":     {Family: "distance_meters", Scale: identity},
	"cm":    {Family: "distance_meters", Scale: div(100)},
	"mm":    {Family: "distance_meters", Scale: div(1000)},
	"km":    {Family: "distance_meters", Scale: mul(1000)},
	"ft":    {Family: "distance_meters", Scale: mul(0.3048)},
	"in":    {Family: "distance_meters", Scale: mul(0.0254)},
	"l":     {Family: "volume_liters", Scale: identity},
	"ml":    {Family: "volume_liters", Scale: div(1000)},
	"m³":    {Family: "volume_liters", Scale: mul(1000)},
	"gal":   {Family: "volume_liters", Scale: mul(3.78541)},
	"l/min": {Family: "flow_liters_per_minute", Scale: identity},
	"l/h":   {Family: "flow_liters_per_minute", Scale: div(60)},
	"m³/h":  {Family: "flow_liters_per_minute", Scale: mul(1000.0 / 60.0)},

	// Miscellaneous.
	"lx":    {Family: "illuminance_lux", Scale: identity},
	"lux":   {Family: "illuminance_lux", Scale: identity},
	"hz":    {Family: "frequency_hertz", Scale: identity},
	"khz":   {Family: "frequency_hertz", Scale: mul(1000)},
	"mhz":   {Family: "frequency_hertz", Scale: mul(1e6)},
	"rpm":   {Family: "rotation_rpm", Scale: identity},
	"°":     {Family: "angle_degrees", Scale: identity},
	"deg":   {Family: "angle_degrees", Scale: identity},
	"m/s":   {Family: "speed_meters_per_second", Scale: identity},
	"km/h":  {Family: "speed_meters_per_second", Scale: div(3.6)},
	"mph":   {Family: "speed_meters_per_second", Scale: mul(0.44704)},
	"µg/m³": {Family: "concentration_micrograms_per_cubic_meter", Scale: identity},
	"ug/m3": {Family: "concentration_micrograms_per_cubic_meter", Scale: identity},
	"µs/cm": {Family: "conductivity_microsiemens_per_centimeter", Scale: identity},
	"ph":    {Family: "acidity_ph", Scale: identity},

	// Concentrations stay in their reported unit: CO2 rendered as 0.0004 is unusable.
	"ppm": {Family: "concentration_ppm", Scale: identity},
	"ppb": {Family: "concentration_ppb", Scale: identity},
}

// percentFamilies disambiguate the "%" unit, which on its own says nothing about what is
// being measured. device_class supplies the missing meaning.
var percentFamilies = map[string]string{
	"humidity":                   "humidity_ratio",
	"battery":                    "battery_ratio",
	"moisture":                   "moisture_ratio",
	"illuminance":                "illuminance_ratio",
	"power_factor":               "power_factor_ratio",
	"volatile_organic_compounds": "voc_ratio",
}

// UnknownFamily is the escape hatch for units we do not recognise.
//
// It is the only family carrying a unit label, which both flags it as heterogeneous and
// keeps a long tail of sanitised nonsense like sensor_g_m_3 out of the metric namespace.
const UnknownFamily = "sensor_value"

// ResolveUnit maps a device-reported unit and device class onto a metric family.
//
// percentAsRatio follows the Prometheus convention of storing ratios rather than
// percentages, so humidity reads 0.482. Grafana's percentunit renders 0-1 correctly with
// no configuration.
func ResolveUnit(unit, deviceClass string, percentAsRatio bool) Quantity {
	u := strings.ToLower(strings.TrimSpace(unit))

	if u == "%" || u == "pct" || u == "percent" {
		family := "ratio"
		if f, ok := percentFamilies[strings.ToLower(deviceClass)]; ok {
			family = f
		}
		if percentAsRatio {
			return Quantity{Family: family, Scale: div(100)}
		}
		return Quantity{Family: strings.TrimSuffix(family, "_ratio") + "_percent", Scale: identity}
	}

	if q, ok := unitTable[u]; ok {
		return q
	}
	if u == "" {
		return Quantity{Family: UnknownFamily, Scale: identity, Unknown: true}
	}
	return Quantity{Family: UnknownFamily, Scale: identity, Unknown: true}
}

// MetricName assembles the fully qualified name for a family.
func MetricName(family string, counter bool) string {
	name := Namespace + "_" + family
	if counter && !strings.HasSuffix(name, "_total") {
		name += "_total"
	}
	return name
}

// SanitizeName coerces an arbitrary string into a valid Prometheus name fragment.
func SanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	lastUnderscore := true // suppresses a leading underscore
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case unicode.IsSpace(r), r == '_', r == '-', r == '.', r == '/', r == ':':
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}

	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unknown"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

// StateClass values as reported by the ESPHome native API.
const (
	StateClassNone            = 0
	StateClassMeasurement     = 1
	StateClassTotalIncreasing = 2
	StateClassTotal           = 3
)

// IsCounter maps an ESPHome state_class onto a Prometheus metric type.
//
// TOTAL_INCREASING resets to zero on device reboot, which is exactly what a Prometheus
// counter means, so rate() and increase() handle it natively. TOTAL may decrease, so it
// stays a gauge. This inference is free and is the single most valuable thing the
// entity metadata gives us.
func IsCounter(stateClass int32) bool {
	return stateClass == StateClassTotalIncreasing
}
