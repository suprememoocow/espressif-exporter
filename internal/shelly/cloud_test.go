package shelly

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
	"github.com/suprememoocow/espressif-exporter/internal/shellycloud"
)

// fakeCloudNames stands in for shellycloud.NameStore.
type fakeCloudNames struct {
	mu     sync.Mutex
	names  map[string]shellycloud.Names
	loaded chan struct{}
}

func newFakeCloudNames(names map[string]shellycloud.Names) *fakeCloudNames {
	f := &fakeCloudNames{names: names, loaded: make(chan struct{})}
	close(f.loaded)
	return f
}

func (f *fakeCloudNames) Lookup(mac string) (shellycloud.Names, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.names[normaliseMAC(mac)]
	return n, ok
}

func (f *fakeCloudNames) Loaded() <-chan struct{} { return f.loaded }

// Names from the Shelly app win over the device's own, on the device and on its outputs,
// and the room becomes the area label.
func TestProbeGen2PrefersCloudNames(t *testing.T) {
	fake, dev := newFakeDevice(t, gen2ShellyBody, string(fixture(t, "gen2_plus1pm.json")))
	fake.config = string(fixture(t, "gen2_config.json"))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	c.SetCloudNames(newFakeCloudNames(map[string]shellycloud.Names{
		"a8032ab12345": {Device: "Kitchen Island Pendant", Room: "Kitchen",
			Channels: map[int]shellycloud.Channel{0: {Name: "Kitchen Island Pendant", Category: "relay"}}},
	}))

	out, res := c.Probe(context.Background(), dev)
	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if !hasLabel(out, "espressif_device_info", "device_name", "Kitchen Island Pendant") {
		t.Errorf("device_name should come from the cloud over /shelly's %q; got %v",
			"Lamp", labelDump(out, "espressif_device_info"))
	}
	for _, family := range []string{"espressif_switch_on", "espressif_power_watts"} {
		if !hasLabel(out, family, "name", "Kitchen Island Pendant") {
			t.Errorf("%s should carry the cloud channel name over GetConfig's %q; got %v",
				family, "Kitchen", labelDump(out, family))
		}
		if !hasLabel(out, family, "area", "Kitchen") {
			t.Errorf("%s should carry area=Kitchen from the cloud room; got %v",
				family, labelDump(out, family))
		}
	}
	// The cloud names outputs; input:0 keeps the device's own name.
	if !hasLabel(out, "espressif_input_state", "name", "Doorbell") {
		t.Errorf("input:0 should keep its GetConfig name; got %v", labelDump(out, "espressif_input_state"))
	}
}

// A device the cloud does not know, or knows without names, is labelled exactly as it
// would be with no cloud configured.
func TestProbeFallsBackWhenTheCloudHasNoName(t *testing.T) {
	for _, tc := range []struct {
		desc  string
		names map[string]shellycloud.Names
	}{
		{"unknown device", map[string]shellycloud.Names{}},
		{"known but unnamed", map[string]shellycloud.Names{"a8032ab12345": {Channels: map[int]shellycloud.Channel{}}}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			fake, dev := newFakeDevice(t, gen2ShellyBody, string(fixture(t, "gen2_plus1pm.json")))
			fake.config = string(fixture(t, "gen2_config.json"))

			c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
			c.SetCloudNames(newFakeCloudNames(tc.names))

			out, res := c.Probe(context.Background(), dev)
			if !res.Success {
				t.Fatalf("probe failed: %+v", res)
			}
			if !hasLabel(out, "espressif_device_info", "device_name", "Lamp") {
				t.Errorf("device_name should fall back to /shelly; got %v", labelDump(out, "espressif_device_info"))
			}
			if !hasLabel(out, "espressif_switch_on", "name", "Kitchen") {
				t.Errorf("switch:0 should fall back to GetConfig; got %v", labelDump(out, "espressif_switch_on"))
			}
			if !hasLabel(out, "espressif_switch_on", "area", "") {
				t.Errorf("area should stay empty; got %v", labelDump(out, "espressif_switch_on"))
			}
		})
	}
}

// Gen1 folds meters[i] into switch:i, so the relay's cloud name labels its watts too.
func TestProbeGen1CloudChannelNameReachesPower(t *testing.T) {
	fake, dev := newFakeDevice(t, gen1ShellyBody, string(fixture(t, "gen1_1pm.json")))
	fake.config = string(fixture(t, "gen1_settings.json"))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	c.SetCloudNames(newFakeCloudNames(map[string]shellycloud.Names{
		"a8032ab12345": {Device: "Water Heater Supply", Channels: map[int]shellycloud.Channel{0: {Name: "Water Heater Supply", Category: "relay"}}},
	}))

	out, res := c.Probe(context.Background(), dev)
	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if !hasLabel(out, "espressif_device_info", "device_name", "Water Heater Supply") {
		t.Errorf("device_name should come from the cloud over /settings; got %v",
			labelDump(out, "espressif_device_info"))
	}
	if !hasLabel(out, "espressif_power_watts", "name", "Water Heater Supply") {
		t.Errorf("power should carry the cloud channel name; got %v", labelDump(out, "espressif_power_watts"))
	}
}

// A multi-channel device has no cloud device name, so the device keeps its own while each
// output takes its channel name.
func TestProbeMultiChannelKeepsTheDeviceOwnName(t *testing.T) {
	fake, dev := newFakeDevice(t, gen2ShellyBody, string(fixture(t, "gen2_plus1pm.json")))
	fake.config = string(fixture(t, "gen2_config.json"))

	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	c.SetCloudNames(newFakeCloudNames(map[string]shellycloud.Names{
		"a8032ab12345": {Room: "Hall", Channels: map[int]shellycloud.Channel{0: {Name: "Landing", Category: "relay"}, 1: {Name: "Hallway", Category: "relay"}}},
	}))

	out, res := c.Probe(context.Background(), dev)
	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if !hasLabel(out, "espressif_device_info", "device_name", "Lamp") {
		t.Errorf("device_name should stay the device's own; got %v", labelDump(out, "espressif_device_info"))
	}
	if !hasLabel(out, "espressif_switch_on", "name", "Landing") {
		t.Errorf("switch:0 should take cloud channel 0; got %v", labelDump(out, "espressif_switch_on"))
	}
}

// Until the first cloud refresh lands, a probe waits briefly rather than emitting the
// device's own names and renaming every series moments later.
func TestProbeWaitsBrieflyForTheFirstCloudRefresh(t *testing.T) {
	_, dev := newFakeDevice(t, gen2ShellyBody, string(fixture(t, "gen2_plus1pm.json")))

	// The names exist only once the refresh lands, as with the real store.
	cloud := &fakeCloudNames{loaded: make(chan struct{})}
	c := New(config.Default().Shelly, metrics.NewRegistry(), discardLogger())
	c.SetCloudNames(cloud)

	time.AfterFunc(50*time.Millisecond, func() {
		cloud.mu.Lock()
		cloud.names = map[string]shellycloud.Names{"a8032ab12345": {Device: "Kitchen Island Pendant"}}
		cloud.mu.Unlock()
		close(cloud.loaded)
	})
	out, res := c.Probe(context.Background(), dev)
	if !res.Success {
		t.Fatalf("probe failed: %+v", res)
	}
	if !hasLabel(out, "espressif_device_info", "device_name", "Kitchen Island Pendant") {
		t.Errorf("the probe did not wait for the first refresh; got %v", labelDump(out, "espressif_device_info"))
	}
}

// On a Shelly EM the cloud's channels are the two meters (category emeter), not the
// relay. Their full names reach every em1 series, including energy, and the relay keeps
// the device's own name rather than inheriting the first meter's.
func TestCloudEnergyMeterNamesGoToTheMetersNotTheRelay(t *testing.T) {
	c, reg := testCollector(t)
	c.identities.put("mac:244cab0a0b0c", Identity{Gen: 1, MAC: "244cab0a0b0c"})
	c.names.put("mac:244cab0a0b0c", 0, c.clock(), deviceNames{components: map[string]string{
		"switch:0": "Contactor",
		"em1:0":    "Kitchen and Dining Room Sock", // truncated on the device
		"em1:1":    "Upstairs Hall and Spare Ro",
	}})
	c.SetCloudNames(newFakeCloudNames(map[string]shellycloud.Names{
		"244cab0a0b0c": {Channels: map[int]shellycloud.Channel{
			0: {Name: "Kitchen and Dining Room Sockets EM", Category: "emeter"},
			1: {Name: "Upstairs Hall and Spare Room EM", Category: "emeter"},
		}},
	}))

	e := emitterFor(reg, "mac:244cab0a0b0c")
	body := []byte(`{"relays":[{"ison":true}],"emeters":[
		{"power":100,"voltage":230,"total":1000,"is_valid":true},
		{"power":50,"voltage":230,"total":500,"is_valid":true}]}`)
	if err := c.decodeGen1Status(e, body); err != nil {
		t.Fatal(err)
	}

	want(t, series(t, e),
		`espressif_switch_on{component=switch,device=mac:244cab0a0b0c,id=0,kind=shelly,name=Contactor} 1`,
		`espressif_power_watts{component=em1,device=mac:244cab0a0b0c,device_class=power,id=0,kind=shelly,name=Kitchen and Dining Room Sockets EM} 100`,
		`espressif_energy_joules_total{component=em1,device=mac:244cab0a0b0c,device_class=energy,direction=import,id=0,kind=shelly,name=Kitchen and Dining Room Sockets EM} 3600000`,
		`espressif_power_watts{component=em1,device=mac:244cab0a0b0c,device_class=power,id=1,kind=shelly,name=Upstairs Hall and Spare Room EM} 50`,
	)
}
