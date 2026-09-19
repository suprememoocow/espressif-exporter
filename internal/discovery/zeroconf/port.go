package zeroconf

import (
	"errors"
	"net"
	"runtime"
	"syscall"
)

// mdnsPort is the port every mDNS responder binds.
const mdnsPort = 5353

// checkPortOwnership reports whether another mDNS daemon already owns port 5353.
//
// This backend sends and receives multicast itself. Where a system daemon already holds
// the port — mDNSResponder on macOS, avahi-daemon on most Linux distributions and on
// TrueNAS SCALE — that daemon consumes the traffic and this backend receives nothing.
// It does not error and it does not log: it simply reports zero devices forever, which
// is the least diagnosable failure the exporter can have.
//
// An exclusive bind is the cheapest reliable probe for the condition. SO_REUSEPORT would
// let the bind succeed while still delivering no packets, so a permissive bind proves
// nothing.
func checkPortOwnership() error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: mdnsPort})
	if err == nil {
		_ = conn.Close()
		return nil
	}
	if errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, syscall.EACCES) {
		return &portInUseError{err: err}
	}
	// Any other bind error is the caller's problem to report verbatim.
	return nil
}

// portInUseError explains the condition and what to do about it.
type portInUseError struct{ err error }

func (e *portInUseError) Error() string { return e.err.Error() }
func (e *portInUseError) Unwrap() error { return e.err }

// Advice returns the fix for the current platform.
func (e *portInUseError) Advice() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS runs mDNSResponder, which cannot share the port. " +
			"Use discovery.sources: [static] and list devices found with " +
			"`dns-sd -B _shelly._tcp local`."
	case "linux":
		return "A local mDNS daemon, most likely avahi-daemon, owns the port. " +
			"Use discovery.sources: [avahi] to browse through it instead."
	default:
		return "Another mDNS daemon owns the port. Use discovery.sources: [static], " +
			"or [avahi] on a host running avahi-daemon."
	}
}
