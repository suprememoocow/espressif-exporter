package avahi

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestSocketPathParsing(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    string
	}{
		{"unix:path=/run/dbus/system_bus_socket", "/run/dbus/system_bus_socket"},
		{"unix:path=/run/dbus/system_bus_socket,guid=abc", "/run/dbus/system_bus_socket"},
		// An abstract socket has no filesystem path, so every path check must no-op
		// rather than report a missing file.
		{"unix:abstract=/tmp/dbus-XYZ", ""},
		{"tcp:host=localhost,port=1234", ""},
		{"", ""},
	} {
		if got := socketPath(tc.address); got != tc.want {
			t.Errorf("socketPath(%q) = %q, want %q", tc.address, got, tc.want)
		}
	}
}

func TestPreflightClassifiesTheSocketPath(t *testing.T) {
	dir := t.TempDir()

	missing := preflight("unix:path=" + filepath.Join(dir, "absent"))
	if missing == nil || missing.cause != causeSocketMissing {
		t.Errorf("a missing path = %v, want %s", missing, causeSocketMissing)
	}

	// Docker creates a directory when the source of a bind mount does not exist, and
	// connecting to it fails with the same errno as a socket whose daemon has gone.
	notASocket := preflight("unix:path=" + dir)
	if notASocket == nil || notASocket.cause != causeNotASocket {
		t.Errorf("a directory = %v, want %s", notASocket, causeNotASocket)
	}

	// sun_path is 104 bytes on macOS, and the temp dir is already most of that.
	sockDir, err := os.MkdirTemp("", "ee")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	if len(sock) > 100 {
		t.Skipf("socket path %q is too long for sun_path", sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if ce := preflight("unix:path=" + sock); ce != nil {
		t.Errorf("a live socket = %v, want no complaint", ce)
	}

	if ce := preflight("tcp:host=localhost,port=1"); ce != nil {
		t.Errorf("a non-path transport = %v, want no complaint", ce)
	}
}

// The failure this classifier exists for: the bus authenticates the connection, then
// hangs up mid-Hello without a D-Bus error, because the host cannot resolve the uid.
func TestClassifiesADroppedHello(t *testing.T) {
	for name, err := range map[string]error{
		"broken pipe":       &net.OpError{Op: "write", Err: syscall.EPIPE},
		"connection reset":  &net.OpError{Op: "read", Err: syscall.ECONNRESET},
		"unexpected EOF":    io.EOF,
		"connection closed": errors.New("dbus: connection closed by user"),
	} {
		t.Run(name, func(t *testing.T) {
			got := diagnose(stageHello, defaultBusAddress, fmt.Errorf("D-Bus Hello: %w", err))

			var ce *connError
			if !errors.As(got, &ce) {
				t.Fatalf("err = %v, want a classified error", got)
			}
			if ce.cause != causeHelloDropped {
				t.Errorf("cause = %s, want %s", ce.cause, causeHelloDropped)
			}
			// The wrapped message must survive untouched, so the log still says what
			// actually happened.
			if !strings.HasPrefix(ce.Error(), "D-Bus Hello: ") {
				t.Errorf("error = %q, want the original message", ce.Error())
			}
			advice := ce.Advice()
			if !strings.Contains(advice, "getent passwd") ||
				!strings.Contains(advice, strconv.Itoa(os.Geteuid())) {
				t.Errorf("advice does not name the uid check: %q", advice)
			}
		})
	}
}

// A refusal arrives as a D-Bus error, a drop does not. They need opposite fixes: one is
// the host's policy, the other is the host's user database.
func TestDistinguishesAPolicyRefusalFromADroppedHello(t *testing.T) {
	refused := dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}
	got := diagnose(stageHello, defaultBusAddress, fmt.Errorf("D-Bus Hello: %w", refused))

	var ce *connError
	if !errors.As(got, &ce) {
		t.Fatalf("err = %v, want a classified error", got)
	}
	if ce.cause != causePolicyDenied {
		t.Errorf("cause = %s, want %s", ce.cause, causePolicyDenied)
	}
}

func TestClassifiesDialErrnos(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want cause
	}{
		{syscall.ECONNREFUSED, causeConnRefused},
		{syscall.EACCES, causePermission},
		{syscall.EPERM, causePermission},
		{syscall.ENOENT, causeSocketMissing},
		{fs.ErrNotExist, causeSocketMissing},
		{errors.New("something else entirely"), causeUnknown},
	} {
		wrapped := fmt.Errorf("dialling %s: %w", defaultBusAddress,
			&net.OpError{Op: "dial", Err: tc.err})

		var ce *connError
		if !errors.As(diagnose(stageDial, defaultBusAddress, wrapped), &ce) {
			t.Fatalf("%v was not classified", tc.err)
		}
		if ce.cause != tc.want {
			t.Errorf("cause for %v = %s, want %s", tc.err, ce.cause, tc.want)
		}
	}
}

func TestAuthFailureBlamesTheUserNamespace(t *testing.T) {
	var ce *connError
	err := diagnose(stageAuth, defaultBusAddress, errors.New("dbus: authentication failed"))
	if !errors.As(err, &ce) || ce.cause != causeAuthRejected {
		t.Fatalf("err = %v, want %s", err, causeAuthRejected)
	}
	if !strings.Contains(ce.Advice(), "userns-remap") {
		t.Errorf("advice does not name the cause: %q", ce.Advice())
	}
}

// Every cause has to name something an operator can do, or the classification is just a
// different way of saying "it failed".
func TestAdviceNamesANextStep(t *testing.T) {
	actionable := []string{"getent", "ls -l", "systemctl", "bind-mount", "uid", "userns-remap", "avahi-dbus.conf"}
	for _, c := range []cause{
		causeUnknown, causeSocketMissing, causeNotASocket, causeConnRefused,
		causePermission, causeAuthRejected, causeHelloDropped, causePolicyDenied,
	} {
		advice := (&connError{cause: c}).Advice()
		if advice == "" {
			t.Errorf("%s has no advice", c)
			continue
		}
		found := false
		for _, want := range actionable {
			if strings.Contains(advice, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("advice for %s names no command or setting: %q", c, advice)
		}
	}
}

// adviceFor is what the fatal message calls, and it must still work for an error that
// never went through the classifier.
func TestAdviceForFallsBackToTheGenericHint(t *testing.T) {
	advice := adviceFor(errors.New("connection refused"))
	if !strings.Contains(advice, "bind-mounted") || !strings.Contains(advice, "avahi-daemon") {
		t.Errorf("fallback advice = %q", advice)
	}
}
