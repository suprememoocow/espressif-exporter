package esphome

import (
	"math"
	"time"

	"github.com/richard87/esphome-apiclient/pb"
	"google.golang.org/protobuf/proto"
)

// entityFromMessage converts a ListEntities*Response into a record.
//
// The upstream library's own entity registry omits several domains its generated
// protobuf already supports, so the raw messages are type-switched here instead. New
// domains can then be added without waiting on upstream.
func entityFromMessage(msg proto.Message) *entityRecord {
	switch m := msg.(type) {
	case *pb.ListEntitiesSensorResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "sensor", ObjectID: m.GetObjectId(), Name: m.GetName(),
			Unit: m.GetUnitOfMeasurement(), DeviceClass: m.GetDeviceClass(),
			StateClass: int32(m.GetStateClass()), Category: int32(m.GetEntityCategory()),
			Icon: m.GetIcon(), Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesBinarySensorResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "binary_sensor", ObjectID: m.GetObjectId(), Name: m.GetName(),
			DeviceClass: m.GetDeviceClass(), Category: int32(m.GetEntityCategory()),
			Icon: m.GetIcon(), Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesTextSensorResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "text_sensor", ObjectID: m.GetObjectId(), Name: m.GetName(),
			DeviceClass: m.GetDeviceClass(), Category: int32(m.GetEntityCategory()),
			Icon: m.GetIcon(), Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesSwitchResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "switch", ObjectID: m.GetObjectId(), Name: m.GetName(),
			DeviceClass: m.GetDeviceClass(), Category: int32(m.GetEntityCategory()),
			Icon: m.GetIcon(), Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesLightResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "light", ObjectID: m.GetObjectId(), Name: m.GetName(),
			Category: int32(m.GetEntityCategory()), Icon: m.GetIcon(),
			Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesFanResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "fan", ObjectID: m.GetObjectId(), Name: m.GetName(),
			Category: int32(m.GetEntityCategory()), Icon: m.GetIcon(),
			Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesCoverResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "cover", ObjectID: m.GetObjectId(), Name: m.GetName(),
			DeviceClass: m.GetDeviceClass(), Category: int32(m.GetEntityCategory()),
			Icon: m.GetIcon(), Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesNumberResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "number", ObjectID: m.GetObjectId(), Name: m.GetName(),
			Unit: m.GetUnitOfMeasurement(), DeviceClass: m.GetDeviceClass(),
			Category: int32(m.GetEntityCategory()), Icon: m.GetIcon(),
			Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesSelectResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "select", ObjectID: m.GetObjectId(), Name: m.GetName(),
			Category: int32(m.GetEntityCategory()), Icon: m.GetIcon(),
			Options: m.GetOptions(), Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesLockResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "lock", ObjectID: m.GetObjectId(), Name: m.GetName(),
			Category: int32(m.GetEntityCategory()), Icon: m.GetIcon(),
			Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesButtonResponse:
		// A button has no state in the protocol; the record exists so its presence is
		// still visible in entity_info.
		return &entityRecord{
			Key: m.GetKey(), Domain: "button", ObjectID: m.GetObjectId(), Name: m.GetName(),
			DeviceClass: m.GetDeviceClass(), Category: int32(m.GetEntityCategory()),
			Icon: m.GetIcon(), Disabled: m.GetDisabledByDefault(),
		}
	case *pb.ListEntitiesClimateResponse:
		return &entityRecord{
			Key: m.GetKey(), Domain: "climate", ObjectID: m.GetObjectId(), Name: m.GetName(),
			Category: int32(m.GetEntityCategory()), Icon: m.GetIcon(),
			Modes: climateModeNames(m.GetSupportedModes()), Disabled: m.GetDisabledByDefault(),
		}
	default:
		return nil
	}
}

func climateModeNames(modes []pb.ClimateMode) []string {
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		out = append(out, climateModeName(m))
	}
	return out
}

func climateModeName(m pb.ClimateMode) string {
	switch m {
	case pb.ClimateMode_CLIMATE_MODE_OFF:
		return "off"
	case pb.ClimateMode_CLIMATE_MODE_HEAT_COOL:
		return "heat_cool"
	case pb.ClimateMode_CLIMATE_MODE_COOL:
		return "cool"
	case pb.ClimateMode_CLIMATE_MODE_HEAT:
		return "heat"
	case pb.ClimateMode_CLIMATE_MODE_FAN_ONLY:
		return "fan_only"
	case pb.ClimateMode_CLIMATE_MODE_DRY:
		return "dry"
	case pb.ClimateMode_CLIMATE_MODE_AUTO:
		return "auto"
	default:
		return "unknown"
	}
}

// stateFromMessage converts a *StateResponse into a record.
//
// A NaN or an explicit missing_state both mean "no reading". They are normalised to the
// same Missing flag here so the emitter has one case to handle, and so neither can leak
// out as a value: NaN poisons avg, sum and rate silently, and 0 is simply a lie.
func stateFromMessage(msg proto.Message, now time.Time) (uint32, stateRecord, bool) {
	switch m := msg.(type) {
	case *pb.SensorStateResponse:
		v := float64(m.GetState())
		missing := m.GetMissingState() || math.IsNaN(v) || math.IsInf(v, 0)
		return m.GetKey(), stateRecord{Value: v, Missing: missing, UpdatedAt: now}, true

	case *pb.BinarySensorStateResponse:
		return m.GetKey(), stateRecord{
			Value: boolToFloat(m.GetState()), Missing: m.GetMissingState(), UpdatedAt: now,
		}, true

	case *pb.SwitchStateResponse:
		return m.GetKey(), stateRecord{Value: boolToFloat(m.GetState()), UpdatedAt: now}, true

	case *pb.TextSensorStateResponse:
		return m.GetKey(), stateRecord{
			Text: m.GetState(), IsText: true, Missing: m.GetMissingState(), UpdatedAt: now,
		}, true

	case *pb.LightStateResponse:
		return m.GetKey(), stateRecord{Value: boolToFloat(m.GetState()), UpdatedAt: now}, true

	case *pb.FanStateResponse:
		return m.GetKey(), stateRecord{Value: boolToFloat(m.GetState()), UpdatedAt: now}, true

	case *pb.CoverStateResponse:
		return m.GetKey(), stateRecord{Value: float64(m.GetPosition()), UpdatedAt: now}, true

	case *pb.NumberStateResponse:
		v := float64(m.GetState())
		missing := m.GetMissingState() || math.IsNaN(v)
		return m.GetKey(), stateRecord{Value: v, Missing: missing, UpdatedAt: now}, true

	case *pb.SelectStateResponse:
		return m.GetKey(), stateRecord{
			Text: m.GetState(), IsText: true, Missing: m.GetMissingState(), UpdatedAt: now,
		}, true

	case *pb.LockStateResponse:
		return m.GetKey(), stateRecord{Value: float64(m.GetState()), UpdatedAt: now}, true

	case *pb.ClimateStateResponse:
		return m.GetKey(), stateRecord{
			Value: float64(m.GetCurrentTemperature()), Text: climateModeName(m.GetMode()),
			IsText: false, UpdatedAt: now,
		}, true

	default:
		return 0, stateRecord{}, false
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
