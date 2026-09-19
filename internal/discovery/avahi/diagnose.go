package avahi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/godbus/dbus/v5"
)

// cause is the classified reason a session could not be established. The values are
// stable slugs, so they work as a log attribute an operator can grep for.
type cause string

const (
	causeUnknown       cause = "unknown"
	causeSocketMissing cause = "socket_missing"
	causeNotASocket    cause = "not_a_socket"
	causeConnRefused   cause = "connection_refused"
	causePermission    cause = "permission_denied"
	causeAuthRejected  cause = "auth_rejected"
	causeHelloDropped  cause = "hello_dropped"
	causePolicyDenied  cause = "policy_denied"
)

// stage names where in the handshake the failure happened. The stage carries most of the
// diagnosis, because the same errno means different things at different stages.
type stage string

const (
	stagePreflight stage = "preflight"
	stageDial      stage = "dial"
	stageAuth      stage = "auth"
	stageHello     stage = "hello"
	stageAvahi     stage = "avahi"
)

// connError is a connection failure that knows what an operator should do about it.
//
// Every reachable D-Bus failure here is a property of the host, not of the exporter, and
// they need different fixes that the error text alone cannot tell apart: a missing mount,
// a stale socket, a uid the host cannot resolve, and a policy refusal all arrive as some
// flavour of "connection reset by peer".
type connError struct {
	cause   cause
	stage   stage
	address string
	path    string

	euid, egid int

	// mode and owner describe the socket when it could be stat'ed, and are zero otherwise.
	mode     fs.FileMode
	ownerUID uint32
	ownerGID uint32

	err error
}

func (e *connError) Error() string { return e.err.Error() }
func (e *connError) Unwrap() error { return e.err }

// Cause and Stage are for structured logging.
func (e *connError) Cause() string { return string(e.cause) }
func (e *connError) Stage() string { return string(e.stage) }

// Advice returns the next step, naming the command to run or the setting to change.
func (e *connError) Advice() string {
	switch e.cause {
	case causeSocketMissing:
		return fmt.Sprintf("%s does not exist in the container: bind-mount the host's "+
			"/run/dbus/system_bus_socket there. Check the host path exists first, because "+
			"Docker creates a directory when the source of a bind mount is missing.",
			e.where())

	case causeNotASocket:
		return fmt.Sprintf("%s exists but is %s, not a socket, which is what Docker "+
			"creates when the source of a bind mount is missing. Check `ls -l "+
			"/run/dbus/system_bus_socket` on the host, then remove the stray path on both "+
			"sides and recreate the container.", e.where(), e.mode.Type())

	case causeConnRefused:
		return fmt.Sprintf("%s exists but nothing is listening on it, which is what a "+
			"restarted dbus-daemon leaves behind: the mount still points at the socket it "+
			"replaced. Check `systemctl is-active dbus avahi-daemon` on the host, then "+
			"recreate this container so the mount resolves again.", e.where())

	case causePermission:
		return fmt.Sprintf("this process is uid %d, gid %d, and %s is mode %s owned by "+
			"%d:%d. Run the container as a uid that may write to the socket, or make the "+
			"socket world-writable on the host.",
			e.euid, e.egid, e.where(), e.mode.Perm(), e.ownerUID, e.ownerGID)

	case causeAuthRejected:
		return fmt.Sprintf("the bus refused authentication. dbus-daemon takes the peer's "+
			"identity from the kernel rather than from anything this process sends, so it "+
			"refused the uid it was told the socket belongs to — which is %d here, unless "+
			"a user-namespace remap is in play, from rootless Docker or "+
			"`dockerd --userns-remap`. Run the container outside the remap, or permit that "+
			"uid on the host.", e.euid)

	case causeHelloDropped:
		return fmt.Sprintf("the bus authenticated uid %d and then closed the connection "+
			"without a D-Bus error reply. dbus-daemon does that when it cannot look the "+
			"uid up in the host's passwd database, which it must do before it will accept "+
			"the connection, and it records the refusal at verbose level only — so the "+
			"host journal stays silent and every other check looks healthy. Run `getent "+
			"passwd %d` on the host: if it prints nothing, run the container as a uid that "+
			"exists there, which is 568:568 (apps) on TrueNAS SCALE and 65534:65534 "+
			"(nobody) on most Linux hosts.", e.euid, e.euid)

	case causePolicyDenied:
		return "the host's D-Bus policy refuses this client access to Avahi. Check " +
			"/etc/dbus-1/system.d/avahi-dbus.conf on the host, including whether it grants " +
			"access by group: a group rule needs this container's uid to resolve to a host " +
			"user before its groups can be looked up at all."

	default:
		// Deliberately the message this backend has always ended with. It is only ever
		// printed when nothing more specific was determined, which is the one situation
		// where pointing at the two usual suspects is still the best available advice.
		return "is /run/dbus/system_bus_socket bind-mounted, and is avahi-daemon running " +
			"on the host?"
	}
}

// where names the socket if the address has a path, and the address otherwise.
func (e *connError) where() string {
	if e.path != "" {
		return e.path
	}
	return e.address
}

// socketPath returns the filesystem path of a unix bus address, and "" for any other
// transport — including unix:abstract=, which has no path to stat. Every path-based check
// then no-ops instead of guessing.
func socketPath(address string) string {
	rest, ok := strings.CutPrefix(address, "unix:")
	if !ok {
		return ""
	}
	for _, kv := range strings.Split(rest, ",") {
		if v, found := strings.CutPrefix(kv, "path="); found {
			return v
		}
	}
	return ""
}

// preflight reports what only a look at the filesystem can establish, and nil otherwise.
//
// It has to run before the dial, not instead of it: connecting to a path that exists but
// is not a socket fails with ECONNREFUSED on Linux, which is indistinguishable by errno
// from a socket whose daemon has gone. The two need opposite fixes.
func preflight(address string) *connError {
	path := socketPath(address)
	if path == "" {
		return nil
	}

	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return newConnError(causeSocketMissing, stagePreflight, address,
			fmt.Errorf("bus socket %s does not exist", path))
	case err != nil:
		// Anything else the dial reports better, with the errno the kernel really
		// returned for a connect.
		return nil
	case fi.Mode()&fs.ModeSocket == 0:
		return newConnError(causeNotASocket, stagePreflight, address,
			fmt.Errorf("%s is %s, not a socket", path, fi.Mode().Type()))
	}
	return nil
}

// diagnose classifies a stage failure, leaving the wrapped message exactly as it was.
func diagnose(st stage, address string, err error) error {
	return newConnError(classify(st, err), st, address, err)
}

func classify(st stage, err error) cause {
	if isAccessDenied(err) {
		return causePolicyDenied
	}

	switch st {
	case stageDial:
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			return causeConnRefused
		case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
			return causePermission
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOENT):
			return causeSocketMissing
		}

	case stageAuth:
		return causeAuthRejected

	case stageHello:
		// A dbus.Error means the bus answered and refused, which is a policy decision. A
		// transport error means it hung up mid-handshake without replying, which is what
		// an unresolvable uid looks like from this side.
		var dbusErr dbus.Error
		if errors.As(err, &dbusErr) {
			return causePolicyDenied
		}
		return causeHelloDropped

	case stagePreflight, stageAvahi:
		// Classified by the caller or not at all.
	}
	return causeUnknown
}

// newConnError records the facts an operator would otherwise have to go and collect: the
// uid this process presents to the bus, and the socket's mode and owner.
func newConnError(c cause, st stage, address string, err error) *connError {
	ce := &connError{
		cause:   c,
		stage:   st,
		address: address,
		path:    socketPath(address),
		euid:    os.Geteuid(),
		egid:    os.Getegid(),
		err:     err,
	}
	if ce.path == "" {
		return ce
	}
	fi, statErr := os.Stat(ce.path)
	if statErr != nil {
		return ce
	}
	ce.mode = fi.Mode()
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		ce.ownerUID, ce.ownerGID = sys.Uid, sys.Gid
	}
	return ce
}

// adviceFor returns the next step for any error, generic hint included.
func adviceFor(err error) string {
	var ce *connError
	if errors.As(err, &ce) {
		return ce.Advice()
	}
	return (&connError{cause: causeUnknown}).Advice()
}
