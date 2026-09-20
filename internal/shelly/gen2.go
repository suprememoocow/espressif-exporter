package shelly

import (
	"encoding/json"
	"strings"

	"github.com/suprememoocow/espressif-exporter/internal/metrics"
)

// extractor turns one component's status object into metrics.
type extractor func(e *metrics.Emitter, component, id string, m map[string]any)

// extractors is the curated set. A component type listed here is handled exclusively:
// the generic fallback never also walks it, so dashboards depend only on names with a
// known type, a known unit, and a stability promise.
var extractors = map[string]extractor{
	"switch":      extractSwitch,
	"light":       extractLight,
	"rgb":         extractLight,
	"rgbw":        extractLight,
	"cct":         extractLight,
	"cover":       extractCover,
	"input":       extractInput,
	"em":          extractEM,
	"em1":         extractEM1,
	"emdata":      extractEMData,
	"em1data":     extractEM1Data,
	"pm1":         extractPM1,
	"temperature": extractTemperature,
	"humidity":    extractHumidity,
	"illuminance": extractIlluminance,
	"voltmeter":   extractVoltmeter,
	"devicepower": extractDevicePower,
	"smoke":       extractSmoke,
	"sys":         extractSys,
	"wifi":        extractWiFi,
	"cloud":       extractCloud,
	"mqtt":        extractMQTT,
	"ws":          extractWebsocket,
	"eth":         extractEth,
}

// skipComponents carry no measurements worth exporting. Listing them explicitly keeps
// them out of the unknown-component counter, so that counter stays a useful signal for
// "a component type shipped that we should curate".
var skipComponents = map[string]bool{
	"ble":      true,
	"ht_ui":    true,
	"knx":      true,
	"modbus":   true,
	"plugs_ui": true,
	"matter":   true,
	"script":   true,
	"schedule": true,
	"webhook":  true,
}

// decodeGen2Status walks Shelly.GetStatus.
//
// The document is a heterogeneous, component-keyed object and there are dozens of models
// with new ones shipping constantly, so it is decoded into raw messages and dispatched
// per component type rather than into a struct per model.
func (c *Collector) decodeGen2Status(e *metrics.Emitter, body []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}

	for key, msg := range raw {
		component, id, _ := strings.Cut(key, ":")

		var m map[string]any
		if err := json.Unmarshal(msg, &m); err != nil {
			continue // a scalar or array at the top level is not a component
		}

		switch {
		case extractors[component] != nil:
			name := c.componentName(e.Labels().Device, component, id)
			extractors[component](e.WithComponent(component, id, name), component, id, m)
		case skipComponents[component]:
		default:
			c.unknownComponents.inc(component)
			if c.genericFallback {
				genericWalk(e.WithComponent(component, id, ""), component, id, m)
			}
		}
	}
	return nil
}

// electrical emits the readings common to every powered component.
func electrical(e *metrics.Emitter, m map[string]any) {
	if v, ok := num(m, "apower"); ok {
		e.Value(metrics.FamilyPower, v)
	}
	if v, ok := num(m, "aprt_power"); ok {
		e.Value(metrics.FamilyApparentPower, v)
	}
	if v, ok := num(m, "voltage"); ok {
		e.Value(metrics.FamilyVoltage, v)
	}
	if v, ok := num(m, "current"); ok {
		e.Value(metrics.FamilyCurrent, v)
	}
	if v, ok := num(m, "pf"); ok {
		e.Value(metrics.FamilyPowerFactor, v)
	}
	if v, ok := num(m, "freq"); ok {
		e.Value(metrics.FamilyFrequency, v)
	}
	// aenergy.total is watt-hours and is monotonic, so it becomes a joules counter.
	// Counter resets on factory reset or reboot are exactly what rate() expects.
	if v, ok := num(m, "aenergy", "total"); ok {
		e.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(v), metrics.DirectionImport)
	}
	if v, ok := num(m, "ret_aenergy", "total"); ok {
		e.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(v), metrics.DirectionExport)
	}
}

// componentTemperature emits a component's internal temperature, which for a switch is
// the MOSFET, not the room.
func componentTemperature(e *metrics.Emitter, m map[string]any) {
	if v, ok := num(m, "temperature", "tC"); ok {
		e.Value(metrics.FamilyTemperature, v)
	}
}

func componentErrors(e *metrics.Emitter, m map[string]any) {
	// Emitted only while present. That is correct here precisely because each probe
	// builds a fresh registry: the series simply stops appearing and Prometheus marks
	// it stale, so `sum(espressif_component_error) > 0` behaves as expected.
	if errs, ok := strs(m, "errors"); ok {
		for _, name := range errs {
			e.Value(metrics.FamilyComponentError, 1, name)
		}
	}
}

func extractSwitch(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := boolean(m, "output"); ok {
		e.Bool(metrics.FamilySwitchOn, v)
	}
	electrical(e, m)
	componentTemperature(e, m)
	componentErrors(e, m)
}

func extractLight(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := boolean(m, "output"); ok {
		e.Bool(metrics.FamilyLightOn, v)
	}
	if v, ok := num(m, "brightness"); ok {
		e.Value(metrics.FamilyLightBrightness, v/100)
	}
	electrical(e, m)
	componentTemperature(e, m)
	componentErrors(e, m)
}

func extractCover(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := num(m, "current_pos"); ok {
		e.Value(metrics.FamilyCoverPosition, v/100, "current")
	}
	if v, ok := num(m, "target_pos"); ok {
		e.Value(metrics.FamilyCoverPosition, v/100, "target")
	}
	electrical(e, m)
	componentTemperature(e, m)
	componentErrors(e, m)
}

func extractInput(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := boolean(m, "state"); ok {
		e.Bool(metrics.FamilyInputState, v)
	}
	if v, ok := num(m, "percent"); ok {
		e.Value("ratio", v/100)
	}
	if v, ok := num(m, "counts", "total"); ok {
		e.Value(metrics.FamilyInputCounts, v, "total")
	}
	if v, ok := num(m, "counts", "xtotal"); ok {
		e.Value(metrics.FamilyInputCounts, v, "xtotal")
	}
	if v, ok := num(m, "freq"); ok {
		e.Value(metrics.FamilyFrequency, v)
	}
	componentErrors(e, m)
}

// extractEM handles the three-phase energy monitor. Every reading carries a phase label,
// which is what lets three-phase em:0 and single-phase switch:0 share one family.
func extractEM(e *metrics.Emitter, _, _ string, m map[string]any) {
	for _, phase := range []string{metrics.PhaseA, metrics.PhaseB, metrics.PhaseC} {
		p := withPhase(e, phase)
		if v, ok := num(m, phase+"_act_power"); ok {
			p.Value(metrics.FamilyPower, v)
		}
		if v, ok := num(m, phase+"_aprt_power"); ok {
			p.Value(metrics.FamilyApparentPower, v)
		}
		if v, ok := num(m, phase+"_voltage"); ok {
			p.Value(metrics.FamilyVoltage, v)
		}
		if v, ok := num(m, phase+"_current"); ok {
			p.Value(metrics.FamilyCurrent, v)
		}
		if v, ok := num(m, phase+"_pf"); ok {
			p.Value(metrics.FamilyPowerFactor, v)
		}
		if v, ok := num(m, phase+"_freq"); ok {
			p.Value(metrics.FamilyFrequency, v)
		}
	}

	neutral := withPhase(e, metrics.PhaseN)
	if v, ok := num(m, "n_current"); ok {
		neutral.Value(metrics.FamilyCurrent, v)
	}

	total := withPhase(e, metrics.PhaseTotal)
	if v, ok := num(m, "total_act_power"); ok {
		total.Value(metrics.FamilyPower, v)
	}
	if v, ok := num(m, "total_aprt_power"); ok {
		total.Value(metrics.FamilyApparentPower, v)
	}
	if v, ok := num(m, "total_current"); ok {
		total.Value(metrics.FamilyCurrent, v)
	}
	componentErrors(e, m)
}

func extractEM1(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := num(m, "act_power"); ok {
		e.Value(metrics.FamilyPower, v)
	}
	if v, ok := num(m, "aprt_power"); ok {
		e.Value(metrics.FamilyApparentPower, v)
	}
	electrical(e, m)
	componentErrors(e, m)
}

// extractEMData emits under component="em" rather than "emdata", so energy and power
// join on (component, id, phase) in a query.
func extractEMData(e *metrics.Emitter, _, id string, m map[string]any) {
	e = e.WithComponent("em", id, e.Labels().Name)

	for _, phase := range []string{metrics.PhaseA, metrics.PhaseB, metrics.PhaseC} {
		p := withPhase(e, phase)
		if v, ok := num(m, phase+"_total_act_energy"); ok {
			p.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(v), metrics.DirectionImport)
		}
		if v, ok := num(m, phase+"_total_act_ret_energy"); ok {
			p.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(v), metrics.DirectionExport)
		}
	}

	total := withPhase(e, metrics.PhaseTotal)
	if v, ok := num(m, "total_act_energy"); ok {
		total.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(v), metrics.DirectionImport)
	}
	if v, ok := num(m, "total_act_ret_energy"); ok {
		total.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(v), metrics.DirectionExport)
	}
}

func extractEM1Data(e *metrics.Emitter, _, id string, m map[string]any) {
	e = e.WithComponent("em1", id, e.Labels().Name)
	if v, ok := num(m, "total_act_energy"); ok {
		e.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(v), metrics.DirectionImport)
	}
	if v, ok := num(m, "total_act_ret_energy"); ok {
		e.Value(metrics.FamilyEnergy, metrics.WattHoursToJoules(v), metrics.DirectionExport)
	}
}

func extractPM1(e *metrics.Emitter, _, _ string, m map[string]any) {
	electrical(e, m)
	componentErrors(e, m)
}

func extractTemperature(e *metrics.Emitter, _, _ string, m map[string]any) {
	// tF is ignored: one family, one unit.
	if v, ok := num(m, "tC"); ok {
		e.Value(metrics.FamilyTemperature, v)
	}
	componentErrors(e, m)
}

func extractHumidity(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := num(m, "rh"); ok {
		e.Value(metrics.FamilyHumidity, v/100)
	}
	componentErrors(e, m)
}

func extractIlluminance(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := num(m, "lux"); ok {
		e.Value(metrics.FamilyIlluminance, v)
	}
	componentErrors(e, m)
}

func extractVoltmeter(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := num(m, "voltage"); ok {
		e.Value(metrics.FamilyVoltage, v)
	}
	if v, ok := num(m, "xvoltage"); ok {
		e.Value(metrics.FamilyVoltage, v)
	}
	componentErrors(e, m)
}

func extractDevicePower(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := num(m, "battery", "V"); ok {
		e.Value(metrics.FamilyVoltage, v)
	}
	if v, ok := num(m, "battery", "percent"); ok {
		e.Value(metrics.FamilyBattery, v/100)
	}
	if v, ok := boolean(m, "external", "present"); ok {
		e.Bool(metrics.FamilyExternalPower, v)
	}
	componentErrors(e, m)
}

func extractSmoke(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := boolean(m, "alarm"); ok {
		e.Bool(metrics.FamilySmokeAlarm, v)
	}
	if v, ok := boolean(m, "mute"); ok {
		e.Bool(metrics.FamilySmokeMuted, v)
	}
	componentErrors(e, m)
}

func extractSys(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := num(m, "uptime"); ok {
		e.Value(metrics.FamilyUptime, v)
	}
	if v, ok := num(m, "ram_size"); ok {
		e.Value(metrics.FamilyRAMBytes, v, "size")
	}
	if v, ok := num(m, "ram_free"); ok {
		e.Value(metrics.FamilyRAMBytes, v, "free")
	}
	if v, ok := num(m, "fs_size"); ok {
		e.Value(metrics.FamilyFSBytes, v, "size")
	}
	if v, ok := num(m, "fs_free"); ok {
		e.Value(metrics.FamilyFSBytes, v, "free")
	}
	if v, ok := boolean(m, "restart_required"); ok {
		e.Bool(metrics.FamilyRestartRequired, v)
	}

	// Both channels are emitted every scrape, at 0 or 1. A series that vanished when an
	// update was installed would make `== 1` alerts behave oddly around resolution.
	updates, _ := obj(m, "available_updates")
	for _, channel := range []string{"stable", "beta"} {
		_, present := updates[channel]
		e.Bool(metrics.FamilyUpdateAvailable, present, channel)
	}
}

func extractWiFi(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := num(m, "rssi"); ok {
		e.Value(metrics.FamilyWiFiRSSI, v)
	}
	if v, ok := str(m, "status"); ok {
		e.Bool(metrics.FamilyWiFiConnected, v == "got ip")
	}
}

func extractCloud(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := boolean(m, "connected"); ok {
		e.Bool(metrics.FamilyCloudConnected, v)
	}
}

func extractMQTT(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := boolean(m, "connected"); ok {
		e.Bool(metrics.FamilyMQTTConnected, v)
	}
}

func extractWebsocket(e *metrics.Emitter, _, _ string, m map[string]any) {
	if v, ok := boolean(m, "connected"); ok {
		e.Bool(metrics.FamilyWebsocketConn, v)
	}
}

func extractEth(e *metrics.Emitter, _, _ string, m map[string]any) {
	if ip, ok := str(m, "ip"); ok {
		e.Bool(metrics.FamilyEthernetUp, ip != "")
	}
}

func withPhase(e *metrics.Emitter, phase string) *metrics.Emitter {
	l := e.Labels()
	l.Phase = phase
	return e.For(l)
}
