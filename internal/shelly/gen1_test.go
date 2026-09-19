package shelly

import (
	"sort"
	"strings"
	"testing"

	"github.com/suprememoocow/espressif-exporter/internal/metrics"
)

func TestGen1OnePM(t *testing.T) {
	c, reg := testCollector(t)
	e := emitterFor(reg, "mac:a8032ab1c2d4")

	if err := c.decodeGen1Status(e, fixture(t, "gen1_1pm.json")); err != nil {
		t.Fatal(err)
	}
	got := series(t, e)

	want(t, got,
		`espressif_switch_on{component=switch,device=mac:a8032ab1c2d4,id=0,kind=shelly} 1`,
		`espressif_power_watts{component=switch,device=mac:a8032ab1c2d4,device_class=power,id=0,kind=shelly} 18.4`,
		// meters[].total is WATT-MINUTES: 783600 * 60 = 4.7016e7 J, the same energy the
		// Gen2 fixture reports as 13060 Wh.
		`espressif_energy_joules_total{component=switch,device=mac:a8032ab1c2d4,device_class=energy,direction=import,id=0,kind=shelly} 47016000`,
		`espressif_temperature_celsius{component=switch,device=mac:a8032ab1c2d4,device_class=temperature,id=0,kind=shelly} 44.6`,
		`espressif_uptime_seconds{component=sys,device=mac:a8032ab1c2d4,device_class=duration,kind=shelly} 918442`,
		`espressif_wifi_rssi_dbm{component=wifi,device=mac:a8032ab1c2d4,device_class=signal_strength,kind=shelly} -61`,
		`espressif_input_state{component=input,device=mac:a8032ab1c2d4,id=0,kind=shelly} 0`,
		`espressif_update_available{channel=stable,component=sys,device=mac:a8032ab1c2d4,kind=shelly} 1`,
	)
}

// An invalid meter means "no reading", so it must produce a gap rather than a zero.
func TestGen1InvalidMeterIsOmitted(t *testing.T) {
	c, reg := testCollector(t)
	e := emitterFor(reg, "mac:a8032ab1c2d4")

	body := []byte(`{"relays":[{"ison":true}],
	                 "meters":[{"power":0,"is_valid":false,"total":0}]}`)
	if err := c.decodeGen1Status(e, body); err != nil {
		t.Fatal(err)
	}
	got := series(t, e)

	// The relay state is still known; only the meter reading is missing.
	want(t, got, `espressif_switch_on{component=switch,device=mac:a8032ab1c2d4,id=0,kind=shelly} 1`)
	notPresent(t, got, "espressif_power_watts")
	notPresent(t, got, "espressif_energy_joules_total")
}

// The dashboard-portability contract: a Gen1 1PM and a Gen2 Plus 1PM must produce
// byte-identical label sets for the readings they share, so a panel needs no `or` clause
// and no per-generation variant. If this breaks, every cross-fleet query silently
// returns half the devices.
func TestGen1AndGen2ProduceIdenticalLabelSets(t *testing.T) {
	shared := []string{
		"espressif_switch_on",
		"espressif_power_watts",
		"espressif_energy_joules_total",
		"espressif_temperature_celsius",
		"espressif_uptime_seconds",
		"espressif_wifi_rssi_dbm",
		"espressif_wifi_connected",
		"espressif_cloud_connected",
		"espressif_mqtt_connected",
		"espressif_update_available",
		"espressif_ram_bytes",
		"espressif_fs_bytes",
	}

	c, reg := testCollector(t)

	gen2 := emitterFor(reg, "DEVICE")
	if err := c.decodeGen2Status(gen2, fixture(t, "gen2_plus1pm.json")); err != nil {
		t.Fatal(err)
	}
	gen1 := emitterFor(reg, "DEVICE")
	if err := c.decodeGen1Status(gen1, fixture(t, "gen1_1pm.json")); err != nil {
		t.Fatal(err)
	}

	g2 := labelSets(t, gen2, shared)
	g1 := labelSets(t, gen1, shared)

	for _, family := range shared {
		a, b := g2[family], g1[family]
		if len(a) == 0 {
			t.Errorf("%s: the Gen2 fixture emitted nothing; the contract is untested for it", family)
			continue
		}
		if len(b) == 0 {
			t.Errorf("%s: Gen1 emits nothing, so a shared dashboard panel would show only Gen2 devices", family)
			continue
		}
		if strings.Join(a, " | ") != strings.Join(b, " | ") {
			t.Errorf("%s label sets differ between generations:\n  gen2: %v\n  gen1: %v", family, a, b)
		}
	}
}

// labelSets returns, per family, the sorted label signatures (names and values, minus
// the value itself) so two generations can be compared exactly.
func labelSets(t *testing.T, e *metrics.Emitter, families []string) map[string][]string {
	t.Helper()
	wanted := map[string]bool{}
	for _, f := range families {
		wanted[f] = true
	}

	out := map[string][]string{}
	for _, s := range series(t, e) {
		name, rest, _ := strings.Cut(s, "{")
		if !wanted[name] {
			continue
		}
		labels, _, _ := strings.Cut(rest, "} ")
		out[name] = append(out[name], labels)
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out
}
