package shelly

import (
	"strconv"

	"github.com/suprememoocow/espressif-exporter/internal/metrics"
)

// gen1Status is the subset of GET /status this exporter uses.
//
// Unlike Gen2, the Gen1 schema is fixed and small, so a struct is clearer than a
// map walk. Pointers distinguish "absent" from "zero", which matters for the same
// reason JSON null does on Gen2: a missing reading must produce a gap, not a 0.
type gen1Status struct {
	Relays []struct {
		IsOn      bool `json:"ison"`
		HasTimer  bool `json:"has_timer"`
		Overpower bool `json:"overpower"`
	} `json:"relays"`

	Meters []struct {
		Power   float64 `json:"power"`
		IsValid bool    `json:"is_valid"`
		// Watt-minutes. See WattMinutesToJoules.
		Total         *float64 `json:"total"`
		TotalReturned *float64 `json:"total_returned"`
	} `json:"meters"`

	EMeters []struct {
		Power   *float64 `json:"power"`
		Voltage *float64 `json:"voltage"`
		Current *float64 `json:"current"`
		PF      *float64 `json:"pf"`
		IsValid bool     `json:"is_valid"`
		// Watt-hours. See WattHoursToJoules.
		Total         *float64 `json:"total"`
		TotalReturned *float64 `json:"total_returned"`
	} `json:"emeters"`

	Inputs []struct {
		Input    int `json:"input"`
		EventCnt int `json:"event_cnt"`
	} `json:"inputs"`

	Lights []struct {
		IsOn       bool     `json:"ison"`
		Brightness *float64 `json:"brightness"`
	} `json:"lights"`

	Temperature     *float64 `json:"temperature"`
	OverTemperature bool     `json:"overtemperature"`

	Tmp *struct {
		TC      *float64 `json:"tC"`
		IsValid bool     `json:"is_valid"`
	} `json:"tmp"`

	Hum *struct {
		Value   *float64 `json:"value"`
		IsValid bool     `json:"is_valid"`
	} `json:"hum"`

	Lux *struct {
		Value   *float64 `json:"value"`
		IsValid bool     `json:"is_valid"`
	} `json:"lux"`

	Bat *struct {
		Value   *float64 `json:"value"`
		Voltage *float64 `json:"voltage"`
	} `json:"bat"`

	ExtTemperature map[string]struct {
		TC *float64 `json:"tC"`
	} `json:"ext_temperature"`

	ExtHumidity map[string]struct {
		Hum *float64 `json:"hum"`
	} `json:"ext_humidity"`

	WiFiSta struct {
		Connected bool     `json:"connected"`
		RSSI      *float64 `json:"rssi"`
	} `json:"wifi_sta"`

	Cloud struct {
		Connected bool `json:"connected"`
	} `json:"cloud"`

	MQTT struct {
		Connected bool `json:"connected"`
	} `json:"mqtt"`

	Update struct {
		HasUpdate bool `json:"has_update"`
	} `json:"update"`

	Uptime   *float64 `json:"uptime"`
	RAMTotal *float64 `json:"ram_total"`
	RAMFree  *float64 `json:"ram_free"`
	FSSize   *float64 `json:"fs_size"`
	FSFree   *float64 `json:"fs_free"`
}

// decodeGen1Status maps Gen1 readings onto the same families Gen2 uses.
//
// The mapping is deliberately not one-to-one with the JSON: meters[i] describes the same
// physical channel as output i, and Gen2 reports an output's power inside the output's own
// component, so Gen1 emits meters[i] under that output's component with the same index:
// switch:N on a relay device, light:N on a dimmer or RGBW2. That is what makes a Shelly
// 1PM and a Plus 1PM produce identical label sets, which a test asserts, and what keeps a
// dimmer's watts on the same series as its name and on/off state.
func (c *Collector) decodeGen1Status(e *metrics.Emitter, body []byte) error {
	var s gen1Status
	if err := decodeJSON(body, &s); err != nil {
		return err
	}

	// name resolves a component's configured name for this device, or "".
	name := func(component, id string) string {
		return c.componentName(e.Labels().Device, component, id)
	}

	for i, r := range s.Relays {
		sw := e.WithComponent("switch", strconv.Itoa(i), name("switch", strconv.Itoa(i)))
		sw.Bool(metrics.FamilySwitchOn, r.IsOn)
		if r.Overpower {
			sw.Value(metrics.FamilyComponentError, 1, "overpower")
		}
	}

	// output is the component meters[] and the device temperature belong to: the relays if
	// the device has any, else its lights. A dimmer has no relays, and filing its meter
	// under switch:0 produced a phantom switch with watts but no state and no name.
	output := "switch"
	if len(s.Relays) == 0 && len(s.Lights) > 0 {
		output = "light"
	}

	for i, m := range s.Meters {
		if !m.IsValid {
			continue // an invalid meter means no reading, not a zero reading
		}
		sw := e.WithComponent(output, strconv.Itoa(i), name(output, strconv.Itoa(i)))
		sw.Value(metrics.FamilyPower, m.Power)
		// Gen1 meters[] reports WATT-MINUTES, while emeters[] below reports
		// watt-hours. Mixing them up is a silent 60x error on every Shelly 1PM.
		if m.Total != nil {
			sw.Value(metrics.FamilyEnergy, metrics.WattMinutesToJoules(*m.Total), metrics.DirectionImport)
		}
		if m.TotalReturned != nil {
			sw.Value(metrics.FamilyEnergy, metrics.WattMinutesToJoules(*m.TotalReturned), metrics.DirectionExport)
		}
	}

	for i, m := range s.EMeters {
		em := e.WithComponent("em1", strconv.Itoa(i), name("em1", strconv.Itoa(i)))
		setIf(em, metrics.FamilyPower, m.Power)
		setIf(em, metrics.FamilyVoltage, m.Voltage)
		setIf(em, metrics.FamilyCurrent, m.Current)
		setIf(em, metrics.FamilyPowerFactor, m.PF)
		// Gen1 emeters[] reports WATT-HOURS, unlike meters[] above.
		if m.Total != nil {
			em.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(*m.Total), metrics.DirectionImport)
		}
		if m.TotalReturned != nil {
			em.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(*m.TotalReturned), metrics.DirectionExport)
		}
	}

	for i, in := range s.Inputs {
		input := e.WithComponent("input", strconv.Itoa(i), name("input", strconv.Itoa(i)))
		input.Bool(metrics.FamilyInputState, in.Input != 0)
		input.Value(metrics.FamilyInputCounts, float64(in.EventCnt), "event")
	}

	for i, l := range s.Lights {
		light := e.WithComponent("light", strconv.Itoa(i), name("light", strconv.Itoa(i)))
		light.Bool(metrics.FamilyLightOn, l.IsOn)
		if l.Brightness != nil {
			light.Value(metrics.FamilyLightBrightness, *l.Brightness/100)
		}
	}

	// tmp means different things on different devices. On a device with outputs it is the
	// internal temperature: a 1PM or 2.5 reports it again as the top-level temperature,
	// and a dimmer reports it nowhere else. On a battery sensor with no outputs (H&T,
	// Flood, Door/Window) it is the ambient reading. So it is internal exactly when the
	// device has outputs, and only the ambient kind becomes temperature:0.
	hasOutputs := len(s.Relays) > 0 || len(s.Lights) > 0 || len(s.Meters) > 0
	tmp := s.Tmp != nil && s.Tmp.IsValid && s.Tmp.TC != nil

	// The internal temperature is reported by Gen2 on its first output:
	// switch:0.temperature.tC, or light:0 on a dimmer. The top-level field wins over tmp
	// where both exist; they carry the same reading.
	internal := s.Temperature
	if internal == nil && tmp && hasOutputs {
		internal = s.Tmp.TC
	}
	if internal != nil {
		sw := e.WithComponent(output, "0", name(output, "0"))
		sw.Value(metrics.FamilyTemperature, *internal)
		if s.OverTemperature {
			sw.Value(metrics.FamilyComponentError, 1, "overtemp")
		}
	}

	// tmp/hum/lux on a sensor are ambient, matching Gen2's temperature:0 and humidity:0.
	if tmp && !hasOutputs {
		e.WithComponent("temperature", "0", "").Value(metrics.FamilyTemperature, *s.Tmp.TC)
	}
	if s.Hum != nil && s.Hum.IsValid && s.Hum.Value != nil {
		e.WithComponent("humidity", "0", "").Value(metrics.FamilyHumidity, *s.Hum.Value/100)
	}
	if s.Lux != nil && s.Lux.IsValid && s.Lux.Value != nil {
		e.WithComponent("illuminance", "0", "").Value(metrics.FamilyIlluminance, *s.Lux.Value)
	}
	if s.Bat != nil {
		bat := e.WithComponent("devicepower", "0", "")
		if s.Bat.Value != nil {
			bat.Value(metrics.FamilyBattery, *s.Bat.Value/100)
		}
		setIf(bat, metrics.FamilyVoltage, s.Bat.Voltage)
	}

	for id, t := range s.ExtTemperature {
		setIf(e.WithComponent("ext_temperature", id, ""), metrics.FamilyTemperature, t.TC)
	}
	for id, h := range s.ExtHumidity {
		if h.Hum != nil {
			v := *h.Hum / 100
			e.WithComponent("ext_humidity", id, "").Value(metrics.FamilyHumidity, v)
		}
	}

	sys := e.WithComponent("sys", "", "")
	setIf(sys, metrics.FamilyUptime, s.Uptime)
	setIfLabelled(sys, metrics.FamilyRAMBytes, s.RAMTotal, "size")
	setIfLabelled(sys, metrics.FamilyRAMBytes, s.RAMFree, "free")
	setIfLabelled(sys, metrics.FamilyFSBytes, s.FSSize, "size")
	setIfLabelled(sys, metrics.FamilyFSBytes, s.FSFree, "free")
	// Both channels every scrape, matching Gen2, so alert expressions are symmetric
	// even though Gen1 has no beta channel.
	sys.Bool(metrics.FamilyUpdateAvailable, s.Update.HasUpdate, "stable")
	sys.Bool(metrics.FamilyUpdateAvailable, false, "beta")

	wifi := e.WithComponent("wifi", "", "")
	setIf(wifi, metrics.FamilyWiFiRSSI, s.WiFiSta.RSSI)
	wifi.Bool(metrics.FamilyWiFiConnected, s.WiFiSta.Connected)

	e.WithComponent("cloud", "", "").Bool(metrics.FamilyCloudConnected, s.Cloud.Connected)
	e.WithComponent("mqtt", "", "").Bool(metrics.FamilyMQTTConnected, s.MQTT.Connected)
	return nil
}

func setIf(e *metrics.Emitter, family string, v *float64) {
	if v != nil {
		e.Value(family, *v)
	}
}

func setIfLabelled(e *metrics.Emitter, family string, v *float64, label string) {
	if v != nil {
		e.Value(family, *v, label)
	}
}
