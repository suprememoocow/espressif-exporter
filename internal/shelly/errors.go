package shelly

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"syscall"

	"github.com/suprememoocow/espressif-exporter/internal/probe"
)

// classify maps a transport or decode error onto a probe reason.
//
// The distinction that matters most is auth versus unreachable. A device with a password
// we do not have looks exactly like a device that is offline unless we separate them,
// and "a device has auth enabled and we have no working credential" is the alert an
// operator actually needs.
func classify(err error) probe.Reason {
	switch {
	case err == nil:
		return probe.ReasonNone
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return probe.ReasonTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return probe.ReasonConnRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return probe.ReasonNoRoute
	}

	var authErr *authError
	if errors.As(err, &authErr) {
		return probe.ReasonAuth
	}
	var jsonErr *json.SyntaxError
	if errors.As(err, &jsonErr) {
		return probe.ReasonDecode
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return probe.ReasonDecode
	}
	var statusErr *statusError
	if errors.As(err, &statusErr) {
		if statusErr.code == http.StatusUnauthorized || statusErr.code == http.StatusForbidden {
			return probe.ReasonAuth
		}
		return probe.ReasonHTTPStatus
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return probe.ReasonTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return probe.ReasonNoRoute
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return probe.ReasonNoRoute
	}
	return probe.ReasonDecode
}

// statusError reports a non-2xx response.
type statusError struct{ code int }

func (e *statusError) Error() string { return "unexpected HTTP status " + http.StatusText(e.code) }

// Code returns the HTTP status.
func (e *statusError) Code() int { return e.code }

// authError reports a credential failure that survived the digest retry.
type authError struct{ code int }

func (e *authError) Error() string { return "authentication failed" }

// statusOf extracts an HTTP status from an error, or 0.
func statusOf(err error) int {
	var s *statusError
	if errors.As(err, &s) {
		return s.code
	}
	var a *authError
	if errors.As(err, &a) {
		return a.code
	}
	return 0
}
