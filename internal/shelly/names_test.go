package shelly

import (
	"context"
	"reflect"
	"testing"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
)

func TestDecodeGen2Config(t *testing.T) {
	got, err := decodeGen2Config(fixture(t, "gen2_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"switch:0": "Kitchen", "input:0": "Doorbell"}
	if !reflect.DeepEqual(got.components, want) {
		t.Errorf("components = %v, want %v", got.components, want)
	}
}

// Gen2+ report the device name on /shelly, which identity reads unauthenticated, so taking it
// from sys.device.name here as well would make the label depend on fetch_config. Worse, the
// fixture shows what sys.device.name holds for a device nobody has named: the device id.
func TestDecodeGen2ConfigLeavesTheDeviceNameEmpty(t *testing.T) {
	got, err := decodeGen2Config(fixture(t, "gen2_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.device != "" {
		t.Errorf("device = %q, want empty", got.device)
	}
}

// A null or empty name is dropped, not stored as "", so an unnamed component simply has no
// name label rather than an empty one.
func TestDecodeGen2ConfigDropsNullAndEmptyNames(t *testing.T) {
	body := []byte(`{"switch:0":{"name":"Boiler"},"switch:1":{"name":null},
	                 "switch:2":{"name":""},"switch:3":{},
	                 "sys":{"device":{"name":"x"}},"scalar":5,"list":[1,2]}`)
	got, err := decodeGen2Config(body)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"switch:0": "Boiler"}
	if !reflect.DeepEqual(got.components, want) {
		t.Errorf("components = %v, want %v", got.components, want)
	}
}

func TestDecodeGen1Settings(t *testing.T) {
	got, err := decodeGen1Settings(fixture(t, "gen1_settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"switch:0": "Water Heater", "input:0": "Wall Switch"}
	if !reflect.DeepEqual(got.components, want) {
		t.Errorf("components = %v, want %v", got.components, want)
	}
	// Gen1's /shelly has no name field, so this document is the only place the name an
	// operator set in the app exists.
	if got.device != "My 1PM" {
		t.Errorf("device = %q, want \"My 1PM\"", got.device)
	}
}

// An absent, empty or null name must stay empty so Probe keeps the mDNS fallback rather than
// exporting a blank device_name.
func TestDecodeGen1SettingsWithoutADeviceName(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"absent", `{"relays":[{"name":"Water Heater"}]}`},
		{"empty", `{"name":"","relays":[{"name":"Water Heater"}]}`},
		{"null", `{"name":null,"relays":[{"name":"Water Heater"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeGen1Settings([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if got.device != "" {
				t.Errorf("device = %q, want empty", got.device)
			}
			if got.components["switch:0"] != "Water Heater" {
				t.Errorf("components = %v, want the relay name preserved", got.components)
			}
		})
	}
}

// The name label reaches the emitted series, and — crucially — the emdata-derived energy
// series carries the same name as the em power series, so the (component,id,phase) join that
// pairs energy with power is not broken by a mismatched name label.
func TestGen2NameReachesSeriesAndEnergyMatchesPower(t *testing.T) {
	c, reg := testCollector(t)
	c.names.put("mac:aabbccddeeff", 0, c.clock(),
		deviceNames{components: map[string]string{"em:0": "Main Panel"}})

	e := emitterFor(reg, "mac:aabbccddeeff")
	if err := c.decodeGen2Status(e, fixture(t, "gen2_em3.json")); err != nil {
		t.Fatal(err)
	}
	got := series(t, e)

	want(t, got,
		`espressif_power_watts{component=em,device=mac:aabbccddeeff,device_class=power,id=0,kind=shelly,name=Main Panel,phase=a} 280.5`,
		// emdata:0 normalises to em:0, so energy inherits the same name as power above.
		`espressif_energy_joules_total{component=em,device=mac:aabbccddeeff,device_class=energy,direction=import,id=0,kind=shelly,name=Main Panel,phase=total} 720000000`,
	)
}

func TestGen1NameReachesSeries(t *testing.T) {
	c, reg := testCollector(t)
	c.names.put("mac:a8032ab1c2d4", 0, c.clock(),
		deviceNames{components: map[string]string{"switch:0": "Water Heater"}})

	e := emitterFor(reg, "mac:a8032ab1c2d4")
	if err := c.decodeGen1Status(e, fixture(t, "gen1_1pm.json")); err != nil {
		t.Fatal(err)
	}

	want(t, series(t, e),
		`espressif_switch_on{component=switch,device=mac:a8032ab1c2d4,id=0,kind=shelly,name=Water Heater} 1`,
		`espressif_power_watts{component=switch,device=mac:a8032ab1c2d4,device_class=power,id=0,kind=shelly,name=Water Heater} 18.4`,
	)
}

// With FetchConfig off, no config request is made and the probe still succeeds with no names.
func TestProbeWithFetchConfigDisabled(t *testing.T) {
	fake, dev := newFakeDevice(t, gen2ShellyBody, string(fixture(t, "gen2_plus1pm.json")))
	fake.config = string(fixture(t, "gen2_config.json"))

	cfg := config.Default().Shelly
	cfg.FetchConfig = false
	c := New(cfg, metrics.NewRegistry(), discardLogger())

	out, res := c.Probe(context.Background(), dev)
	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if n := fake.requests["/rpc/Shelly.GetConfig"]; n != 0 {
		t.Errorf("GetConfig requested %d times with FetchConfig off, want 0", n)
	}
	if hasLabel(out, "espressif_switch_on", "name", "Kitchen") {
		t.Error("no name label expected when FetchConfig is off")
	}
}
