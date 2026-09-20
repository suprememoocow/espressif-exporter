package avahi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeServer stands in for avahi-daemon, so the whole session state machine is testable
// without a D-Bus daemon — which matters because the machine's job is to survive
// failures that are impractical to reproduce on demand.
type fakeServer struct {
	mu sync.Mutex

	browsers    map[string]*browser
	resolve     func(name string) (Service, error)
	hostNameErr error
	closed      bool

	// owners stands in for the D-Bus name watch, so an avahi restart can be simulated.
	owners chan string

	browserCalls  int
	resolveCalls  int
	hostNameCalls int
}

func newFakeServer() *fakeServer {
	return &fakeServer{browsers: map[string]*browser{}, owners: make(chan string, 4)}
}

func (f *fakeServer) ServiceBrowserNew(
	_ context.Context, _, _ int32, serviceType, _ string, _ uint32,
) (*browser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.browserCalls++
	b := &browser{
		add:    make(chan Service, 8),
		remove: make(chan Service, 8),
	}
	f.browsers[serviceType] = b
	return b, nil
}

func (f *fakeServer) ResolveService(
	_ context.Context, _, _ int32, name, _, _ string, _ int32, _ uint32,
) (Service, error) {
	f.mu.Lock()
	f.resolveCalls++
	fn := f.resolve
	f.mu.Unlock()
	if fn == nil {
		return Service{}, errors.New("no resolver configured")
	}
	return fn(name)
}

func (f *fakeServer) GetHostName(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostNameCalls++
	if f.hostNameErr != nil {
		return "", f.hostNameErr
	}
	return "truenas", nil
}

func (f *fakeServer) OwnerChanges() <-chan string { return f.owners }

func (f *fakeServer) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakeServer) browser(t *testing.T, serviceType string) *browser {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		b, ok := f.browsers[serviceType]
		f.mu.Unlock()
		if ok {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no browser was created for %s", serviceType)
	return nil
}

func testDiscoverer(t *testing.T, server avahiService, connectErr error) *Discoverer {
	t.Helper()
	cfg := config.Default().Discovery.Avahi
	cfg.ReconnectMin = 10 * time.Millisecond
	cfg.ReconnectMax = 20 * time.Millisecond
	cfg.HealthCheckInterval = 20 * time.Millisecond
	cfg.RebrowseInterval = time.Hour
	cfg.ResolveTimeout = 200 * time.Millisecond
	cfg.StartupTimeout = 50 * time.Millisecond

	d := New(cfg, []string{discovery.ServiceShelly}, discardLogger())
	d.connect = func(context.Context, string) (avahiService, error) {
		if connectErr != nil {
			return nil, connectErr
		}
		return server, nil
	}
	return d
}

func TestBusAddressPrecedence(t *testing.T) {
	cfg := config.Default().Discovery.Avahi
	d := New(cfg, nil, discardLogger())

	// Distroless has no /var/run -> /run symlink, so the default must be the /run path
	// rather than godbus's compiled-in one.
	if got := d.busAddress(); got != defaultBusAddress {
		t.Errorf("default = %q, want %q", got, defaultBusAddress)
	}

	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path=/custom/socket")
	if got := d.busAddress(); got != "unix:path=/custom/socket" {
		t.Errorf("env = %q, want the environment value", got)
	}

	cfg.DBusAddress = "unix:path=/explicit"
	d = New(cfg, nil, discardLogger())
	if got := d.busAddress(); got != "unix:path=/explicit" {
		t.Errorf("config = %q, want the configured value to win over the environment", got)
	}
}

// A device seen on the wire must reach the registry with its address and TXT intact.
func TestResolvesAndEmits(t *testing.T) {
	server := newFakeServer()
	server.resolve = func(name string) (Service, error) {
		return Service{
			Interface: 2, Protocol: protoInet, Name: name,
			Type: discovery.ServiceShelly, Domain: "local",
			Host: "shellyplus1pm-a8032ab12345.local", Address: "192.168.1.57", Port: 80,
			Txt: [][]byte{[]byte("gen=2"), []byte("app=Plus1PM")},
		}, nil
	}

	d := testDiscoverer(t, server, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan discovery.Event, 16)
	go func() { _ = d.Run(ctx, out) }()

	b := server.browser(t, discovery.ServiceShelly)
	b.add <- Service{
		Interface: 2, Protocol: protoInet,
		Name: "shellyplus1pm-a8032ab12345", Type: discovery.ServiceShelly, Domain: "local",
	}

	ev := awaitEvent(t, out, discovery.EventAdd)
	if ev.Endpoint.Addr.String() != "192.168.1.57" {
		t.Errorf("address = %s, want 192.168.1.57", ev.Endpoint.Addr)
	}
	if ev.Endpoint.Port != 80 {
		t.Errorf("port = %d, want 80", ev.Endpoint.Port)
	}
	if ev.Endpoint.TXT["gen"] != "2" {
		t.Errorf("TXT = %v, want gen=2", ev.Endpoint.TXT)
	}
	if ev.Kind != discovery.KindShelly {
		t.Errorf("kind = %q, want shelly", ev.Kind)
	}
	// The hostname is carried for display but must never be what gets dialled: the
	// container has no mDNS resolver.
	if ev.Endpoint.Host == "" {
		t.Error("the mDNS hostname should be carried through for display")
	}
}

// Every session starts by telling the registry its prior Avahi state is unverified,
// which is what lets a reconnect avoid deleting devices that are perfectly fine.
func TestEmitsSourceResetOnEverySession(t *testing.T) {
	d := testDiscoverer(t, newFakeServer(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan discovery.Event, 16)
	go func() { _ = d.Run(ctx, out) }()

	awaitEvent(t, out, discovery.EventSourceReset)
}

// A remove is a hint, not a deletion instruction.
func TestRemoveIsEmittedAsAHint(t *testing.T) {
	server := newFakeServer()
	d := testDiscoverer(t, server, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan discovery.Event, 16)
	go func() { _ = d.Run(ctx, out) }()

	b := server.browser(t, discovery.ServiceShelly)
	b.remove <- Service{
		Interface: 2, Protocol: protoInet,
		Name: "shellyplus1pm-a8032ab12345", Type: discovery.ServiceShelly, Domain: "local",
	}

	ev := awaitEvent(t, out, discovery.EventRemove)
	if ev.Instance != "shellyplus1pm-a8032ab12345" {
		t.Errorf("instance = %q", ev.Instance)
	}
}

// The failure that hides best: avahi-daemon dies, the D-Bus connection stays healthy,
// and the browsers go silent without ever erroring. Only an active round trip catches
// it, so the watchdog must tear the session down and reconnect.
func TestWatchdogCatchesTheSilentWedge(t *testing.T) {
	server := newFakeServer()
	d := testDiscoverer(t, server, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan discovery.Event, 64)
	go func() { _ = d.Run(ctx, out) }()

	// Let the first session establish.
	awaitEvent(t, out, discovery.EventSourceReset)

	// Now the daemon stops answering, without closing anything.
	server.mu.Lock()
	server.hostNameErr = errors.New("daemon gone")
	server.mu.Unlock()

	// A new session must start, which re-emits a source reset.
	awaitEvent(t, out, discovery.EventSourceReset)

	if d.Health().Reconnects == 0 {
		t.Error("the reconnect should have been counted")
	}
}

// A D-Bus policy refusal cannot be fixed by retrying, and when Avahi is both required
// and the only source, the exporter should say so and stop rather than loop silently.
func TestStartupFailsFastWhenRequiredAndAlone(t *testing.T) {
	d := testDiscoverer(t, nil, errors.New("connection refused"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := d.Run(ctx, make(chan discovery.Event, 8))

	var fatal *Fatal
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v, want a Fatal after the startup timeout", err)
	}
	// The message has to name the two things an operator can actually check.
	if msg := err.Error(); !contains(msg, "bind-mounted") || !contains(msg, "avahi-daemon") {
		t.Errorf("error message is not actionable: %q", msg)
	}
}

// Once a session has worked, runtime loss must never take the process down: a degraded
// exporter serving flagged data beats a crash loop.
func TestRuntimeLossIsNeverFatal(t *testing.T) {
	server := newFakeServer()
	d := testDiscoverer(t, server, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	out := make(chan discovery.Event, 64)
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, out) }()

	awaitEvent(t, out, discovery.EventSourceReset)
	server.mu.Lock()
	server.hostNameErr = errors.New("daemon gone")
	server.mu.Unlock()

	if err := <-done; err != nil {
		t.Errorf("Run returned %v; runtime loss must not be fatal once a session has succeeded", err)
	}
}

func TestAccessDeniedIsRecognised(t *testing.T) {
	if !isAccessDenied(dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}) {
		t.Error("a D-Bus AccessDenied should be recognised")
	}
	if isAccessDenied(errors.New("connection refused")) {
		t.Error("an ordinary connection error is not AccessDenied")
	}
}

func awaitEvent(t *testing.T, out <-chan discovery.Event, want discovery.EventType) discovery.Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-out:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event within 3s", want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// captureLogger records what was logged, for the assertions that are about the log line
// itself rather than about behaviour.
func captureLogger() (*slog.Logger, func() string) {
	var mu sync.Mutex
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(&lockedWriter{mu: &mu, w: buf}, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// helloDropped is the failure this backend spent an afternoon misdiagnosing: the bus
// authenticates the connection and then hangs up, because the host cannot resolve the
// container's uid.
func helloDropped() error {
	return diagnose(stageHello, defaultBusAddress,
		fmt.Errorf("D-Bus Hello: %w", &net.OpError{Op: "write", Err: syscall.EPIPE}))
}

// A browse hit must never be mistaken for anything else. Reading one as an avahi restart
// tore the session down on the first device found, so discovery never completed a browse.
func TestBrowseHitDoesNotEndTheSession(t *testing.T) {
	server := newFakeServer()
	server.resolve = func(name string) (Service, error) {
		return Service{
			Interface: 2, Protocol: protoInet, Name: name,
			Type: discovery.ServiceShelly, Domain: "local",
			Host: "shelly1pmg3-dcb4d9cc6eec.local", Address: "192.168.150.99", Port: 80,
			Txt: [][]byte{[]byte("gen=3")},
		}, nil
	}

	d := testDiscoverer(t, server, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan discovery.Event, 64)
	go func() { _ = d.Run(ctx, out) }()

	b := server.browser(t, discovery.ServiceShelly)
	b.add <- Service{
		Interface: 2, Protocol: protoInet,
		Name: "shelly1pmg3-dcb4d9cc6eec", Type: discovery.ServiceShelly, Domain: "local",
	}
	awaitEvent(t, out, discovery.EventAdd)

	// The session must still be the same one: a second source reset would mean it was
	// torn down and rebuilt.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-out:
			if ev.Type == discovery.EventSourceReset {
				t.Fatal("the session restarted after a browse hit")
			}
		case <-deadline:
			if n := d.Health().Reconnects; n != 0 {
				t.Errorf("reconnects = %d, want 0 after an ordinary browse hit", n)
			}
			if !d.Health().Up {
				t.Error("the backend should still be up")
			}
			return
		}
	}
}

// Losing the bus name is the real avahi restart, and it does have to end the session:
// the browser objects belong to the dead daemon.
func TestOwnerLossEndsTheSession(t *testing.T) {
	server := newFakeServer()
	d := testDiscoverer(t, server, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan discovery.Event, 64)
	go func() { _ = d.Run(ctx, out) }()

	awaitEvent(t, out, discovery.EventSourceReset)
	server.owners <- ""

	awaitEvent(t, out, discovery.EventSourceReset)
	if d.Health().Reconnects == 0 {
		t.Error("the reconnect should have been counted")
	}
}

// The fatal message is the last thing an operator sees before the process exits, so it
// has to carry the specific fix rather than the generic one.
func TestFatalCarriesTheClassifiedAdvice(t *testing.T) {
	d := testDiscoverer(t, nil, helloDropped())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := d.Run(ctx, make(chan discovery.Event, 8))

	var fatal *Fatal
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v, want a Fatal after the startup timeout", err)
	}
	msg := err.Error()
	if !contains(msg, "getent passwd") || !contains(msg, strconv.Itoa(os.Geteuid())) {
		t.Errorf("the message does not name the uid check: %q", msg)
	}
	if contains(msg, "bind-mounted") {
		t.Errorf("the generic hint should give way to the specific one: %q", msg)
	}
}

// The advice has to appear on the first failure, not only at the startup deadline 30
// seconds later.
func TestFirstFailureLogsTheAdvice(t *testing.T) {
	log, dump := captureLogger()

	cfg := config.Default().Discovery.Avahi
	cfg.ReconnectMin = 10 * time.Millisecond
	cfg.ReconnectMax = 10 * time.Millisecond
	cfg.StartupTimeout = time.Hour // not the thing under test

	d := New(cfg, []string{discovery.ServiceShelly}, log)
	d.connect = func(context.Context, string) (avahiService, error) { return nil, helloDropped() }

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx, make(chan discovery.Event, 8))

	out := dump()
	for _, want := range []string{"cause=hello_dropped", "stage=hello", "getent passwd", "hint="} {
		if !contains(out, want) {
			t.Errorf("the first failure did not log %q:\n%s", want, out)
		}
	}
}

// The routine rebrowse is not a failure. Treating it as one made the reconnect delay
// double on every cycle until a healthy process was pinned at reconnect_max, leaving it
// with no browsers at all for a full minute every half hour — and, because a rebrowse is
// the only thing that re-observes an Avahi endpoint, stretching the window in which
// devices age out of the registry.
func TestRebrowseDoesNotClimbTheReconnectLadder(t *testing.T) {
	server := newFakeServer()
	d := testDiscoverer(t, server, nil)
	d.cfg.ReconnectMin = time.Millisecond
	d.cfg.ReconnectMax = 500 * time.Millisecond
	d.cfg.RebrowseInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan discovery.Event, 1024)
	// Drain, so a full channel never becomes the thing that paces the loop.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-out:
			}
		}
	}()
	go func() { _ = d.Run(ctx, out) }()

	time.Sleep(500 * time.Millisecond)
	cancel()

	server.mu.Lock()
	sessions := server.browserCalls
	server.mu.Unlock()

	// With the ladder climbing, the delays go 1, 2, 4, 8, 16, 32, 64, 128, 256ms and only
	// about eight sessions fit. Reset each time, each cycle costs ~11ms, so dozens do.
	if sessions < 20 {
		t.Errorf("%d sessions in 500ms; want at least 20, so the reconnect delay is "+
			"not doubling across routine rebrowses", sessions)
	}
}
