// Package avahi implements a Discoverer that browses through the host's avahi-daemon
// over the system D-Bus socket.
//
// This is what lets the exporter run in a bridged container. A bridged container cannot
// send or receive multicast, so it cannot do mDNS itself; borrowing the host's daemon
// over D-Bus means only discovery needs the host's help, while ordinary unicast HTTP and
// TCP to the devices continue to work straight from the bridge network.
//
// It also avoids a conflict that has no other solution on an appliance like TrueNAS,
// where avahi-daemon holds port 5353 on every interface and cannot be disabled: this
// backend never binds 5353 at all.
package avahi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/holoplot/go-avahi"
	"golang.org/x/sync/semaphore"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// Name identifies this backend in events and metrics.
const Name = "avahi"

// defaultBusAddress is used when neither config nor the environment names one.
//
// godbus compiles in /var/run/dbus/system_bus_socket, but a distroless image has no
// /var/run -> /run symlink, so the path is stated explicitly here and in the compose
// file rather than relying on the default.
const defaultBusAddress = "unix:path=/run/dbus/system_bus_socket"

// avahiService is the subset of the Avahi server we use, so the whole session state
// machine can be tested without a D-Bus daemon.
type avahiService interface {
	ServiceBrowserNew(iface, protocol int32, serviceType, domain string, flags uint32) (*avahi.ServiceBrowser, error)
	ServiceBrowserFree(b *avahi.ServiceBrowser)
	ResolveService(iface, protocol int32, name, serviceType, domain string, aprotocol int32, flags uint32) (avahi.Service, error)
	GetHostName() (string, error)
	Close()
}

// Discoverer browses via avahi-daemon, reconnecting for as long as it runs.
type Discoverer struct {
	cfg          config.Avahi
	serviceTypes []string
	log          *slog.Logger

	// connect is swappable so tests can drive the session machine with a fake.
	connect func(ctx context.Context, address string) (*dbus.Conn, avahiService, error)

	resolveSem *semaphore.Weighted
	reconnects atomic.Uint64
	resolvers  atomic.Int64

	mu     sync.Mutex
	health discovery.Health
}

// New builds the backend.
func New(cfg config.Avahi, serviceTypes []string, log *slog.Logger) *Discoverer {
	return &Discoverer{
		cfg:          cfg,
		serviceTypes: serviceTypes,
		log:          log.With("component", "avahi"),
		connect:      connectDBus,
		resolveSem:   semaphore.NewWeighted(int64(cfg.ResolveConcurrency)),
	}
}

// Name implements discovery.Discoverer.
func (d *Discoverer) Name() string { return Name }

// Health implements discovery.Discoverer.
func (d *Discoverer) Health() discovery.Health {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.health
	h.Reconnects = d.reconnects.Load()
	return h
}

// ActiveResolvers reports in-flight resolve calls, for self-metrics.
func (d *Discoverer) ActiveResolvers() int64 { return d.resolvers.Load() }

// Fatal reports whether startup failed permanently, so the caller can decide whether to
// exit. It is only ever true before the first successful session.
type Fatal struct{ err error }

func (f *Fatal) Error() string { return f.err.Error() }
func (f *Fatal) Unwrap() error { return f.err }

// Run supervises the Avahi session until ctx is cancelled.
//
// Startup is bounded fail-fast; runtime loss never is. Once a session has succeeded the
// backend retries forever, because a working exporter serving stale-but-flagged data
// beats a crash loop every time.
func (d *Discoverer) Run(ctx context.Context, out chan<- discovery.Event) error {
	backoff := d.cfg.ReconnectMin
	started := time.Now()
	everConnected := false

	for ctx.Err() == nil {
		reason, err := d.session(ctx, out)
		if err == nil {
			everConnected = true
		} else {
			d.setDown(err.Error())
			if isAccessDenied(err) {
				// A D-Bus policy refusal is not a connectivity problem and no amount of
				// retrying fixes it, so say what to change rather than logging the same
				// error every second.
				d.log.Error("the host's D-Bus policy refused access to Avahi",
					"error", err,
					"hint", "check /etc/dbus-1/system.d/avahi-dbus.conf on the host")
			} else {
				d.log.Warn("avahi session failed", "error", err, "retry_in", backoff)
			}

			if !everConnected && d.cfg.Required && time.Since(started) > d.cfg.StartupTimeout {
				return &Fatal{fmt.Errorf(
					"avahi was unreachable for %s: %w (is /run/dbus/system_bus_socket "+
						"bind-mounted, and is avahi-daemon running on the host?)",
					d.cfg.StartupTimeout, err)}
			}
		}

		if reason != "" {
			d.reconnects.Add(1)
			d.log.Info("avahi session ended; reconnecting", "reason", reason)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jitter(backoff)):
		}

		backoff *= 2
		if backoff > d.cfg.ReconnectMax {
			backoff = d.cfg.ReconnectMax
		}
	}
	return nil
}

// session runs one connection generation.
func (d *Discoverer) session(ctx context.Context, out chan<- discovery.Event) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, server, err := d.connect(ctx, d.busAddress())
	if err != nil {
		return "", err
	}
	defer func() {
		server.Close()
		if conn != nil {
			_ = conn.Close()
		}
	}()

	// Tell the registry its Avahi-sourced endpoints are unverified. Nothing is deleted:
	// avahi replays its record cache to a new browser within seconds, so the normal
	// outcome is that everything re-verifies immediately.
	emit(ctx, out, discovery.Event{
		Type: discovery.EventSourceReset, Source: Name, At: time.Now(),
	})

	browsers := make([]*avahi.ServiceBrowser, 0, len(d.serviceTypes))
	defer func() {
		for _, b := range browsers {
			server.ServiceBrowserFree(b)
		}
	}()

	var wg sync.WaitGroup
	for _, serviceType := range d.serviceTypes {
		b, err := server.ServiceBrowserNew(
			avahi.InterfaceUnspec, avahi.ProtoUnspec, serviceType, "local", 0)
		if err != nil {
			return "browser_failed", fmt.Errorf("browsing %s: %w", serviceType, err)
		}
		browsers = append(browsers, b)

		wg.Add(1)
		go func() {
			defer wg.Done()
			d.consume(ctx, server, b, serviceType, out)
		}()
	}

	d.setUp()
	d.log.Info("avahi session established", "service_types", d.serviceTypes)

	reason := d.watch(ctx, conn, server)
	cancel()
	wg.Wait()
	return reason, nil
}

// watch blocks until something ends the session, returning why.
func (d *Discoverer) watch(ctx context.Context, conn *dbus.Conn, server avahiService) string {
	// avahi-daemon restarting is the failure mode that hides best: the D-Bus connection
	// stays perfectly healthy because dbus-daemon is a separate process, and the
	// browsers simply go silent — their channels are never closed and never receive
	// anything again.
	// nameOwner stays nil when there is no bus to watch (in tests), in which case the
	// watchdog below is the sole detector — which is exactly the configuration the
	// watchdog exists to cover, so the machine degrades correctly rather than breaking.
	var nameOwner chan *dbus.Signal
	if conn != nil {
		nameOwner = make(chan *dbus.Signal, 8)
		conn.Signal(nameOwner)
		if err := conn.AddMatchSignal(
			dbus.WithMatchInterface("org.freedesktop.DBus"),
			dbus.WithMatchMember("NameOwnerChanged"),
			dbus.WithMatchArg(0, "org.freedesktop.Avahi"),
		); err != nil {
			d.log.Warn("could not watch for avahi restarts; relying on the watchdog", "error", err)
		}
	}

	watchdog := time.NewTicker(d.cfg.HealthCheckInterval)
	defer watchdog.Stop()
	rebrowse := time.NewTimer(d.cfg.RebrowseInterval)
	defer rebrowse.Stop()

	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return ""

		case sig, ok := <-nameOwner:
			if !ok {
				return "dbus_closed"
			}
			if sig == nil || len(sig.Body) < 3 {
				continue
			}
			if newOwner, _ := sig.Body[2].(string); newOwner == "" {
				return "name_lost"
			}
			// A new owner means avahi restarted; our browser objects belong to the dead
			// one and will never deliver anything again.
			return "name_changed"

		case <-watchdog.C:
			// The third failure mode: neither signal fires and nothing arrives. The
			// browsers are fed by a goroutine reading D-Bus signals, so if that source
			// goes quiet the backend looks healthy while receiving nothing. Only an
			// active round trip distinguishes the two.
			if _, err := server.GetHostName(); err != nil {
				consecutiveFailures++
				if consecutiveFailures >= 2 {
					return "watchdog"
				}
				continue
			}
			consecutiveFailures = 0

		case <-rebrowse.C:
			// Periodically recycle the browsers, which flushes any subscription that
			// has silently wedged and forces fresh queries.
			return "rebrowse"
		}
	}
}

// consume turns browser events into discovery events.
func (d *Discoverer) consume(
	ctx context.Context, server avahiService, b *avahi.ServiceBrowser,
	serviceType string, out chan<- discovery.Event,
) {
	for {
		select {
		case <-ctx.Done():
			return

		case svc, ok := <-b.AddChannel:
			if !ok {
				return
			}
			go d.resolve(ctx, server, svc, serviceType, out)

		case svc, ok := <-b.RemoveChannel:
			if !ok {
				return
			}
			// A hint only. mDNS goodbye packets are routinely lost on flaky wifi, and
			// the registry re-resolves rather than evicting.
			emit(ctx, out, discovery.Event{
				Type:     discovery.EventRemove,
				Source:   Name,
				Instance: svc.Name,
				Domain:   svc.Domain,
				Kind:     discovery.Classify(serviceType, svc.Name, nil),
				At:       time.Now(),
				Endpoint: discovery.Endpoint{Key: discovery.EndpointKey{
					ServiceType: serviceType,
					Interface:   svc.Interface,
					Protocol:    protocolName(svc.Protocol),
				}},
			})
		}
	}
}

// resolve turns a browse hit into a full endpoint.
//
// Avahi's ResolveService D-Bus method is synchronous and returns the address and TXT in
// one call, so there is no resolver object to create and free — and therefore no risk of
// exhausting avahi-daemon's objects-per-client-max, which a long-lived resolver per
// device would approach at this fleet size.
func (d *Discoverer) resolve(
	ctx context.Context, server avahiService, svc avahi.Service,
	serviceType string, out chan<- discovery.Event,
) {
	if err := d.resolveSem.Acquire(ctx, 1); err != nil {
		return
	}
	defer d.resolveSem.Release(1)

	d.resolvers.Add(1)
	defer d.resolvers.Add(-1)

	type result struct {
		svc avahi.Service
		err error
	}
	// ResolveService has no context parameter, so it is bounded here instead. A
	// goroutine stuck on a wedged daemon is released when the session tears down and
	// closes the D-Bus connection, which errors out every pending call; the semaphore
	// caps how many can be stuck in the meantime.
	done := make(chan result, 1)
	go func() {
		resolved, err := server.ResolveService(
			svc.Interface, svc.Protocol, svc.Name, svc.Type, svc.Domain, avahi.ProtoUnspec, 0)
		done <- result{resolved, err}
	}()

	select {
	case <-ctx.Done():
		return
	case <-time.After(d.cfg.ResolveTimeout):
		d.log.Debug("resolve timed out", "instance", svc.Name, "service", serviceType)
		return
	case r := <-done:
		if r.err != nil {
			d.log.Debug("resolve failed", "instance", svc.Name, "error", r.err)
			return
		}
		d.emitResolved(ctx, r.svc, serviceType, out)
	}
}

func (d *Discoverer) emitResolved(
	ctx context.Context, svc avahi.Service, serviceType string, out chan<- discovery.Event,
) {
	// Avahi hands back a hostname too, but it must never be dialled: the container has
	// no mDNS resolver and no multicast path, so a .local name does not resolve there.
	addr, err := netip.ParseAddr(svc.Address)
	if err != nil {
		d.log.Debug("unparseable address from avahi", "instance", svc.Name, "address", svc.Address)
		return
	}

	txt := discovery.ParseTXTBytes(svc.Txt)
	kind := discovery.Classify(serviceType, svc.Name, txt)
	if kind == discovery.KindUnknown {
		return
	}

	now := time.Now()
	emit(ctx, out, discovery.Event{
		Type:     discovery.EventAdd,
		Source:   Name,
		Instance: svc.Name,
		Domain:   svc.Domain,
		Kind:     kind,
		At:       now,
		Endpoint: discovery.Endpoint{
			Key: discovery.EndpointKey{
				ServiceType: serviceType,
				Interface:   svc.Interface,
				Protocol:    protocolName(svc.Protocol),
			},
			Host:       svc.Host,
			Addr:       addr.Unmap(),
			Port:       svc.Port,
			TXT:        txt,
			ObservedAt: now,
		},
	})

	d.mu.Lock()
	d.health.LastEventAt = now
	d.mu.Unlock()
}

// busAddress resolves the D-Bus address to use.
func (d *Discoverer) busAddress() string {
	if d.cfg.DBusAddress != "" {
		return d.cfg.DBusAddress
	}
	if env := os.Getenv("DBUS_SYSTEM_BUS_ADDRESS"); env != "" {
		return env
	}
	return defaultBusAddress
}

// connectDBus opens a private connection to the system bus.
//
// SystemBusPrivate, not SystemBus: the latter returns a process-global, reference-counted
// connection that cannot meaningfully be closed and reopened, which makes reconnecting
// after an outage impossible.
func connectDBus(ctx context.Context, address string) (*dbus.Conn, avahiService, error) {
	conn, err := dbus.Dial(address, dbus.WithContext(ctx))
	if err != nil {
		return nil, nil, fmt.Errorf("dialling %s: %w", address, err)
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("authenticating to D-Bus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("D-Bus Hello: %w", err)
	}

	server, err := avahi.ServerNew(conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("connecting to avahi-daemon: %w", err)
	}
	if _, err := server.GetHostName(); err != nil {
		server.Close()
		_ = conn.Close()
		return nil, nil, fmt.Errorf("avahi-daemon did not respond: %w", err)
	}
	return conn, server, nil
}

func (d *Discoverer) setUp() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.health.Up = true
	d.health.Reason = ""
}

func (d *Discoverer) setDown(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.health.Up = false
	d.health.Reason = reason
}

func emit(ctx context.Context, out chan<- discovery.Event, ev discovery.Event) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}

func protocolName(proto int32) string {
	if proto == avahi.ProtoInet6 {
		return discovery.ProtoIPv6
	}
	return discovery.ProtoIPv4
}

func isAccessDenied(err error) bool {
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) {
		return strings.Contains(dbusErr.Name, "AccessDenied")
	}
	return strings.Contains(err.Error(), "AccessDenied")
}

// jitter spreads reconnect timing. math/rand is correct here: it is not a security
// boundary.
func jitter(d time.Duration) time.Duration {
	f := float64(d)
	return time.Duration(f*0.8 + 0.4*f*rand.Float64()) //nolint:gosec // G404: jitter, not a secret
}
