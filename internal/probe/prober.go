// Package probe holds the collector-agnostic machinery shared by the ESPHome and Shelly
// collectors: the Prober contract, concurrency limiting, request de-duplication and
// per-device backoff.
package probe

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// Reason classifies why a probe failed. The full set is exported on every probe as
// probe_failure_reason, with exactly one value set to 1.
type Reason string

const (
	ReasonNone          Reason = ""
	ReasonTimeout       Reason = "timeout"
	ReasonConnRefused   Reason = "conn_refused"
	ReasonNoRoute       Reason = "no_route"
	ReasonNoAddress     Reason = "no_address"
	ReasonAuth          Reason = "auth"
	ReasonHTTPStatus    Reason = "http_status"
	ReasonDecode        Reason = "decode"
	ReasonMACMismatch   Reason = "mac_mismatch"
	ReasonBackoff       Reason = "backoff"
	ReasonInFlightLimit Reason = "in_flight_limit"
	ReasonNotConnected  Reason = "not_connected"
)

// AllReasons is the complete enum.
//
// Every reason is emitted on every probe, at 0 or 1. Emitting only the active one would
// leave the previous value to go stale after five minutes rather than being explicitly
// cleared, so `probe_failure_reason == 1` would report two reasons at once for those
// five minutes after every transition.
var AllReasons = []Reason{
	ReasonTimeout,
	ReasonConnRefused,
	ReasonNoRoute,
	ReasonNoAddress,
	ReasonAuth,
	ReasonHTTPStatus,
	ReasonDecode,
	ReasonMACMismatch,
	ReasonBackoff,
	ReasonInFlightLimit,
	ReasonNotConnected,
}

// IsDeviceFault reports whether a reason reflects a problem with the device rather than
// with the exporter.
//
// This distinction drives backoff. in_flight_limit means we were too busy, so counting
// it as a device failure would back off healthy devices during a load spike and turn a
// transient overload into a self-sustaining outage.
func (r Reason) IsDeviceFault() bool {
	switch r {
	case ReasonNone, ReasonInFlightLimit, ReasonBackoff:
		return false
	default:
		return true
	}
}

// IsAuth reports whether a reason warrants the longer credential backoff ladder.
func (r Reason) IsAuth() bool { return r == ReasonAuth }

// Result is the outcome of one probe.
type Result struct {
	Success    bool
	Reason     Reason
	HTTPStatus int
}

// Success builds a successful result.
func Success(httpStatus int) Result {
	return Result{Success: true, HTTPStatus: httpStatus}
}

// Fail builds a failed result.
func Fail(reason Reason, httpStatus int) Result {
	return Result{Reason: reason, HTTPStatus: httpStatus}
}

// Prober collects metrics from one device.
//
// It returns const metrics rather than implementing prometheus.Collector so that the
// HTTP handler can time the work precisely and treat failures as ordinary control flow.
// This mirrors blackbox_exporter.
type Prober interface {
	Kind() string
	Probe(ctx context.Context, dev registry.Device) ([]prometheus.Metric, Result)
}
