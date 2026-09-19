package esphome

import "github.com/suprememoocow/espressif-exporter/internal/metrics"

// ESPHome-specific metric families. Everything a Shelly also reports lives in the shared
// families in the metrics package, so one query covers both vendors.
const (
	familyESPHomeBuild      = "esphome_build_info"
	familyConnectedSince    = "connected_since_timestamp_seconds"
	familyLastMessage       = "last_message_timestamp_seconds"
	familyPing              = "api_ping_seconds"
	familyEntityCount       = "entities"
	familyStateUpdates      = "state_updates"
	familyEntityGeneration  = "entity_list_generation"
	familyConnectAttempts   = "connect_attempts"
	familyConnectFailures   = "connect_failures"
	familyEntitiesTruncated = "entities_truncated"

	familyFanOn        = "fan_on"
	familyLockState    = "lock_state"
	familyClimateMode  = "climate_mode"
	familySelectOption = "select_option"
)

func registerESPHomeFamilies(r *metrics.Registry) {
	for _, f := range []metrics.Family{
		{Name: familyESPHomeBuild, Help: "ESPHome build metadata. Always 1.",
			Extra: []string{"compilation_time", "project_name", "project_version"}},
		{Name: familyConnectedSince, Help: "When the current API connection was established."},
		{Name: familyLastMessage, Help: "When the device last sent anything, in seconds since the epoch."},
		{Name: familyPing, Help: "Round-trip time of the last API ping. The clearest signal " +
			"of a wedged node or poor WiFi.", DeviceClass: "duration"},
		{Name: familyEntityCount, Help: "Entities the device reports."},
		{Name: familyStateUpdates, Counter: true,
			Help: "State updates received from this device."},
		{Name: familyEntityGeneration, Counter: true,
			Help: "Increments whenever the entity list is replaced, which happens on every " +
				"reconnect and after an OTA. A rising rate means a device is crash-looping."},
		{Name: familyConnectAttempts, Counter: true,
			Help: "Connection attempts made to this device."},
		{Name: familyConnectFailures, Counter: true,
			Help: "Connection failures, by cause.", Extra: []string{"reason"}},
		{Name: familyEntitiesTruncated, Help: "Set when the entity list exceeded the configured cap."},

		{Name: familyFanOn, Help: "Whether a fan is running."},
		{Name: familyLockState, Help: "Lock state as reported by the device."},
		{Name: familyClimateMode, Help: "Active climate mode.", Extra: []string{"mode"}},
		{Name: familySelectOption, Help: "Active select option.", Extra: []string{"option"}},
	} {
		r.MustRegister(f)
	}
}
