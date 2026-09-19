package metrics

import (
	"math"
	"testing"
)

func almost(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// Both vendors must land in the same family for the same physical quantity; that is the
// entire point of the unified namespace.
func TestResolveUnitSharedFamilies(t *testing.T) {
	tests := []struct {
		unit, class string
		wantFamily  string
		in, want    float64
		counter     bool
	}{
		{"W", "power", "power_watts", 18.4, 18.4, false},
		{"kW", "power", "power_watts", 1.5, 1500, false},
		{"°C", "temperature", "temperature_celsius", 21.4, 21.4, false},
		{"°F", "temperature", "temperature_celsius", 212, 100, false},
		{"K", "temperature", "temperature_celsius", 273.15, 0, false},
		{"V", "voltage", "voltage_volts", 241.3, 241.3, false},
		{"mA", "current", "current_amperes", 82, 0.082, false},
		{"hPa", "pressure", "pressure_pascals", 1013.2, 101320, false},
		{"dBm", "signal_strength", "signal_dbm", -58, -58, false},
		{"s", "duration", "duration_seconds", 411902, 411902, false},
		{"B", "", "bytes", 168432, 168432, false},
		{"Hz", "", "frequency_hertz", 50.1, 50.1, false},
		{"lx", "illuminance", "illuminance_lux", 140, 140, false},
		// Energy is a counter in joules, per the Prometheus base-unit convention.
		{"Wh", "energy", "energy_joules", 13060, 4.7016e7, true},
		{"kWh", "energy", "energy_joules", 13.06, 4.7016e7, true},
		// Concentrations stay in their reported unit: CO2 at 0.0004 is unusable.
		{"ppm", "carbon_dioxide", "concentration_ppm", 812, 812, false},
	}
	for _, tt := range tests {
		q := ResolveUnit(tt.unit, tt.class, true)
		if q.Family != tt.wantFamily {
			t.Errorf("ResolveUnit(%q, %q).Family = %q, want %q", tt.unit, tt.class, q.Family, tt.wantFamily)
		}
		if q.Counter != tt.counter {
			t.Errorf("ResolveUnit(%q).Counter = %v, want %v", tt.unit, q.Counter, tt.counter)
		}
		almost(t, q.Scale(tt.in), tt.want, "ResolveUnit("+tt.unit+").Scale")
	}
}

// "%" alone says nothing about what is being measured, so device_class disambiguates.
func TestResolveUnitPercentUsesDeviceClass(t *testing.T) {
	tests := []struct {
		class      string
		wantFamily string
	}{
		{"humidity", "humidity_ratio"},
		{"battery", "battery_ratio"},
		{"", "ratio"},
		{"something_new", "ratio"},
	}
	for _, tt := range tests {
		q := ResolveUnit("%", tt.class, true)
		if q.Family != tt.wantFamily {
			t.Errorf("ResolveUnit(%%, %q).Family = %q, want %q", tt.class, q.Family, tt.wantFamily)
		}
		almost(t, q.Scale(48.2), 0.482, "percent scaled to a ratio")
	}

	// Opting out keeps raw percent and renames the family so the unit stays honest.
	q := ResolveUnit("%", "humidity", false)
	if q.Family != "humidity_percent" {
		t.Errorf("Family = %q, want humidity_percent when percent_as_ratio is off", q.Family)
	}
	almost(t, q.Scale(48.2), 48.2, "raw percent")
}

// An unrecognised unit goes to one clearly-marked escape hatch rather than generating a
// long tail of sanitised nonsense families.
func TestResolveUnitUnknown(t *testing.T) {
	for _, unit := range []string{"", "bananas", "g/m^3"} {
		q := ResolveUnit(unit, "", true)
		if q.Family != UnknownFamily || !q.Unknown {
			t.Errorf("ResolveUnit(%q) = %+v, want the unknown escape hatch", unit, q)
		}
	}
}

// Gen1 Shelly reports watt-minutes in meters[] and watt-hours in emeters[]. Confusing
// them is a silent 60x error, so both conversions are pinned here.
func TestEnergyConversionsAreExact(t *testing.T) {
	almost(t, WattMinutesToJoules(1), 60, "1 Wmin")
	almost(t, WattHoursToJoules(1), 3600, "1 Wh")
	almost(t, KilowattHoursToJoules(1), 3.6e6, "1 kWh")

	// The same physical energy, reported in each unit, must converge on one value.
	const oneKWhInJoules = 3.6e6
	almost(t, WattHoursToJoules(1000), oneKWhInJoules, "1000 Wh")
	almost(t, WattMinutesToJoules(60000), oneKWhInJoules, "60000 Wmin")
	almost(t, KilowattHoursToJoules(1), oneKWhInJoules, "1 kWh")
}

func TestMetricName(t *testing.T) {
	if got := MetricName("power_watts", false); got != "espressif_power_watts" {
		t.Errorf("got %q", got)
	}
	if got := MetricName("energy_joules", true); got != "espressif_energy_joules_total" {
		t.Errorf("got %q", got)
	}
	// The suffix must never be doubled.
	if got := MetricName("energy_joules_total", true); got != "espressif_energy_joules_total" {
		t.Errorf("got %q", got)
	}
}

func TestSanitizeName(t *testing.T) {
	tests := map[string]string{
		"Bedroom Temperature": "bedroom_temperature",
		"switch:0":            "switch_0",
		"aenergy.total":       "aenergy_total",
		"  leading":           "leading",
		"µg/m³":               "g_m",
		"3phase":              "_3phase",
		"!!!":                 "unknown",
		"a---b":               "a_b",
	}
	for in, want := range tests {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TOTAL_INCREASING resets to zero on reboot, which is precisely a Prometheus counter.
func TestIsCounterFromStateClass(t *testing.T) {
	if !IsCounter(StateClassTotalIncreasing) {
		t.Error("TOTAL_INCREASING should map to a counter")
	}
	for _, sc := range []int32{StateClassNone, StateClassMeasurement, StateClassTotal} {
		if IsCounter(sc) {
			t.Errorf("state_class %d should map to a gauge", sc)
		}
	}
}
