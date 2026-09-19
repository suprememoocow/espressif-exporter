package avahi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/holoplot/go-avahi"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeServer stands in for avahi-daemon, so the whole session state machine is testable
// without a D-Bus daemon — which matters because the machine's job is to survive
// failures that are impractical to reproduce on demand.
type fakeServer struct {
	mu sync.Mutex

	browsers    map[string]*avahi.ServiceBrowser
	resolve     func(name string) (avahi.Service, error)
	hostNameErr error
	closed      bool

	browserCalls  int
	resolveCalls  int
	hostNameCalls int
}

func newFakeServer() *fakeServer {
	return &fakeServer{browsers: map[string]*avahi.ServiceBrowser{}}
}

func (f *fakeServer) ServiceBrowserNew(
	_, _ int32, serviceType, _ string, _ uint32,
) (*avahi.ServiceBrowser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.browserCalls++
	b := &avahi.ServiceBrowser{
		AddChannel:    make(chan avahi.Service, 8),
		RemoveChannel: make(chan avahi.Service, 8),
	}
	f.browsers[serviceType] = b
	return b, nil
}

func (f *fakeServer) ServiceBrowserFree(*avahi.ServiceBrowser) {}

func (f *fakeServer) ResolveService(
	_, _ int32, name, _, _ string, _ int32, _ uint32,
) (avahi.Service, error) {
	f.mu.Lock()
	f.resolveCalls++
	fn := f.resolve
	f.mu.Unlock()
	if fn == nil {
		return avahi.Service{}, errors.New("no resolver configured")
	}
	return fn(name)
}

func (f *fakeServer) GetHostName() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostNameCalls++
	if f.hostNameErr != nil {
		return "", f.hostNameErr
	}
	return "truenas", nil
}

func (f *fakeServer) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakeServer) browser(t *testing.T, serviceType string) *avahi.ServiceBrowser {
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
	d.connect = func(context.Context, string) (*dbus.Conn, avahiService, error) {
		if connectErr != nil {
			return nil, nil, connectErr
		}
		return nil, server, nil
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
	server.resolve = func(name string) (avahi.Service, error) {
		return avahi.Service{
			Interface: 2, Protocol: avahi.ProtoInet, Name: name,
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
	b.AddChannel <- avahi.Service{
		Interface: 2, Protocol: avahi.ProtoInet,
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
	b.RemoveChannel <- avahi.Service{
		Interface: 2, Protocol: avahi.ProtoInet,
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
