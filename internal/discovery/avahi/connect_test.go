package avahi

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBus speaks just enough of the D-Bus SASL handshake to reproduce how dbus-daemon
// refuses a uid it cannot resolve: it completes authentication, sending OK, and only then
// hangs up — before answering Hello, and without a D-Bus error reply.
//
// That ordering is the whole reason the failure is hard to read from the client side, so
// it is worth reproducing against the real godbus handshake rather than a stub.
func fakeBus(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "ee")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "s")
	if len(path) > 100 {
		t.Skipf("socket path %q is too long for sun_path", path)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		in := bufio.NewReader(conn)
		if _, err := in.ReadByte(); err != nil { // the credentials NUL byte
			return
		}
		for {
			line, err := in.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.Fields(strings.TrimSpace(line)); {
			case len(cmd) == 0:
				return
			case cmd[0] == "AUTH" && len(cmd) == 1:
				_, _ = conn.Write([]byte("REJECTED EXTERNAL\r\n"))
			case cmd[0] == "AUTH":
				_, _ = conn.Write([]byte("OK 0123456789abcdef0123456789abcdef\r\n"))
			case cmd[0] == "NEGOTIATE_UNIX_FD":
				_, _ = conn.Write([]byte("AGREE_UNIX_FD\r\n"))
			case cmd[0] == "BEGIN":
				// Authentication has completed. Hanging up here, rather than answering
				// Hello, is exactly what dbus-daemon does with an unresolvable uid.
				return
			}
		}
	}()

	return "unix:path=" + path
}

// The TrueNAS failure, end to end: a bus that authenticates and then hangs up must be
// reported as a uid the host cannot resolve, not as a mount or daemon problem.
func TestConnectClassifiesABusThatHangsUpAfterAuthenticating(t *testing.T) {
	address := fakeBus(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := connectDBus(ctx, address, discardLogger())
	if err == nil {
		t.Fatal("connecting to a bus that hangs up should fail")
	}

	var ce *connError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a classified error", err)
	}
	if ce.cause != causeHelloDropped {
		t.Fatalf("cause = %s (stage %s, error %v), want %s",
			ce.cause, ce.stage, err, causeHelloDropped)
	}
	if !strings.Contains(ce.Advice(), "getent passwd") {
		t.Errorf("advice does not name the uid check: %q", ce.Advice())
	}
	// The socket is real, so the advice can report its mode and owner.
	if ce.mode == 0 {
		t.Error("the socket should have been stat'ed")
	}
}

func TestConnectReportsAMissingSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := connectDBus(ctx, "unix:path="+filepath.Join(t.TempDir(), "absent"), discardLogger())

	var ce *connError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a classified error", err)
	}
	if ce.cause != causeSocketMissing {
		t.Errorf("cause = %s, want %s", ce.cause, causeSocketMissing)
	}
	if ce.stage != stagePreflight {
		t.Errorf("stage = %s, want %s: the dial cannot tell this apart from a stale socket",
			ce.stage, stagePreflight)
	}
}
