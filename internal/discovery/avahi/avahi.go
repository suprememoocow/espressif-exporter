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

// probeTimeout bounds the liveness round trip to avahi-daemon. A D-Bus call has no
// deadline of its own, so without this a wedged daemon wedges the loop whose whole job is
// to notice that it has wedged.
const probeTimeout = 5 * time.Second

// avahiService is the subset of the Avahi server we use, so the whole session state
// machine can be tested without a D-Bus daemon.
type avahiService interface {
	ServiceBrowserNew(ctx context.Context, iface, protocol int32, serviceType, domain string, flags uint32) (*browser, error)
	ResolveService(ctx context.Context, iface, protocol int32, name, serviceType, domain string, aprotocol int32, flags uint32) (Service, error)
	GetHostName(ctx context.Context) (string, error)
	// OwnerChanges reports avahi-daemon coming or going. The client owns the D-Bus
	// signal channel, so a browse hit can never arrive here.
	OwnerChanges() <-chan string
	Close()
}

// Discoverer browses via avahi-daemon, reconnecting for as long as it runs.
type Discoverer struct {
	cfg          config.Avahi
	serviceTypes []string
	log          *slog.Logger

	// connect is swappable so tests can drive the session machine with a fake.
	connect func(ctx context.Context, address string) (avahiService, error)

	resolveSem *semaphore.Weighted
	reconnects atomic.Uint64
	resolvers  atomic.Int64

	mu     sync.Mutex
	health discovery.Health
}

// New builds the backend.
func New(cfg config.Avahi, serviceTypes []string, log *slog.Logger) *Discoverer {
	d := &Discoverer{
		cfg:          cfg,
		serviceTypes: serviceTypes,
		log:          log.With("component", "avahi"),
		resolveSem:   semaphore.NewWeighted(int64(cfg.ResolveConcurrency)),
	}
	d.connect = func(ctx context.Context, address string) (avahiService, error) {
		return connectDBus(ctx, address, d.log)
	}
	return d
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
	lastCause := causeUnknown

	for ctx.Err() == nil {
		reason, err := d.session(ctx, out)
		if err == nil {
			everConnected = true
			lastCause = causeUnknown
		} else {
			d.setDown(err.Error())
			d.logFailure(err, backoff, &lastCause)

			if !everConnected && d.cfg.Required && time.Since(started) > d.cfg.StartupTimeout {
				return &Fatal{fmt.Errorf("avahi was unreachable for %s: %w (%s)",
					d.cfg.StartupTimeout, err, adviceFor(err))}
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

	server, err := d.connect(ctx, d.busAddress())
	if err != nil {
		return "", err
	}
	// Closing the client closes the D-Bus connection, which is what releases the browser
	// objects in avahi-daemon. Freeing them individually is what the previous client
	// library did, and it raced its own signal dispatch.
	defer server.Close()

	// Tell the registry its Avahi-sourced endpoints are unverified. Nothing is deleted:
	// avahi replays its record cache to a new browser within seconds, so the normal
	// outcome is that everything re-verifies immediately.
	emit(ctx, out, discovery.Event{
		Type: discovery.EventSourceReset, Source: Name, At: time.Now(),
	})

	var wg sync.WaitGroup
	for _, serviceType := range d.serviceTypes {
		b, err := server.ServiceBrowserNew(
			ctx, interfaceUnspec, protoUnspec, serviceType, "local", 0)
		if err != nil {
			return "browser_failed", fmt.Errorf("browsing %s: %w", serviceType, err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			d.consume(ctx, server, b, serviceType, out)
		}()
	}

	d.setUp()
	d.log.Info("avahi session established", "service_types", d.serviceTypes)

	reason := d.watch(ctx, server)
	cancel()
	wg.Wait()
	return reason, nil
}

// watch blocks until something ends the session, returning why.
func (d *Discoverer) watch(ctx context.Context, server avahiService) string {
	// avahi-daemon restarting is the failure mode that hides best: the D-Bus connection
	// stays perfectly healthy because dbus-daemon is a separate process, and the
	// browsers simply go silent — their channels are never closed and never receive
	// anything again.
	owners := server.OwnerChanges()

	watchdog := time.NewTicker(d.cfg.HealthCheckInterval)
	defer watchdog.Stop()
	rebrowse := time.NewTimer(d.cfg.RebrowseInterval)
	defer rebrowse.Stop()

	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return ""

		case newOwner, ok := <-owners:
			if !ok {
				return "dbus_closed"
			}
			if newOwner == "" {
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
			if _, err := d.probe(ctx, server); err != nil {
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
	ctx context.Context, server avahiService, b *browser,
	serviceType string, out chan<- discovery.Event,
) {
	// The client never closes these channels, so the exit from this loop is always
	// ctx.Done. The ok guards below are belt and braces.
	for {
		select {
		case <-ctx.Done():
			return

		case svc, ok := <-b.Add():
			if !ok {
				return
			}
			go d.resolve(ctx, server, svc, serviceType, out)

		case svc, ok := <-b.Remove():
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
	ctx context.Context, server avahiService, svc Service,
	serviceType string, out chan<- discovery.Event,
) {
	if err := d.resolveSem.Acquire(ctx, 1); err != nil {
		return
	}
	defer d.resolveSem.Release(1)

	d.resolvers.Add(1)
	defer d.resolvers.Add(-1)

	// A resolve against a wedged daemon must not pin a slot in the semaphore forever,
	// so the call is bounded by its own deadline rather than by the session ending.
	callCtx, cancel := context.WithTimeout(ctx, d.cfg.ResolveTimeout)
	defer cancel()

	resolved, err := server.ResolveService(
		callCtx, svc.Interface, svc.Protocol, svc.Name, svc.Type, svc.Domain, protoUnspec, 0)
	if err != nil {
		d.log.Debug("resolve failed", "instance", svc.Name, "service", serviceType, "error", err)
		return
	}
	d.emitResolved(ctx, resolved, serviceType, out)
}

func (d *Discoverer) emitResolved(
	ctx context.Context, svc Service, serviceType string, out chan<- discovery.Event,
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
// Dial on an explicit address, not SystemBus: the latter returns a process-global,
// reference-counted connection that cannot meaningfully be closed and reopened, which
// makes reconnecting after an outage impossible.
//
// Every failure is classified before it is returned, because the causes need different
// fixes on the host and are indistinguishable from the error text alone.
func connectDBus(ctx context.Context, address string, log *slog.Logger) (avahiService, error) {
	if ce := preflight(address); ce != nil {
		return nil, ce
	}

	conn, err := dbus.Dial(address, dbus.WithContext(ctx))
	if err != nil {
		return nil, diagnose(stageDial, address, fmt.Errorf("dialling %s: %w", address, err))
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, diagnose(stageAuth, address, fmt.Errorf("authenticating to D-Bus: %w", err))
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, diagnose(stageHello, address, fmt.Errorf("D-Bus Hello: %w", err))
	}

	server, err := newClient(conn, log)
	if err != nil {
		_ = conn.Close()
		return nil, diagnose(stageAvahi, address, fmt.Errorf("connecting to avahi-daemon: %w", err))
	}
	// One round trip before declaring the session up, so a daemon that is registered on
	// the bus but not answering is caught here rather than looking healthy forever.
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, err := server.GetHostName(probeCtx); err != nil {
		server.Close()
		return nil, diagnose(stageAvahi, address, fmt.Errorf("avahi-daemon did not respond: %w", err))
	}
	return server, nil
}

// probe asks avahi-daemon its hostname, bounded. Which answer comes back does not
// matter; that one comes back at all is the only proof this session still works.
func (d *Discoverer) probe(ctx context.Context, server avahiService) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return server.GetHostName(ctx)
}

// logFailure reports a session failure, with its advice the first time and on every change
// of cause.
//
// The advice is a paragraph and the retry loop runs as often as once a second, so
// repeating it verbatim would bury everything else in the log. Logging it on change keeps
// the first line an operator reads actionable without making the rest unreadable.
func (d *Discoverer) logFailure(err error, backoff time.Duration, last *cause) {
	var ce *connError
	if !errors.As(err, &ce) {
		d.log.Warn("avahi session failed", "error", err, "retry_in", backoff)
		return
	}
	if *last == ce.cause {
		d.log.Warn("avahi session failed", "error", err, "cause", ce.Cause(), "retry_in", backoff)
		return
	}
	*last = ce.cause

	if ce.cause == causePolicyDenied {
		// A policy refusal is not a connectivity problem and no amount of retrying fixes
		// it, so it is reported as an error and says what to change.
		d.log.Error("the host's D-Bus policy refused access to Avahi",
			"error", err, "cause", ce.Cause(), "hint", ce.Advice())
		return
	}
	d.log.Warn("avahi session failed",
		"error", err, "cause", ce.Cause(), "stage", ce.Stage(),
		"uid", ce.euid, "gid", ce.egid, "hint", ce.Advice(), "retry_in", backoff)
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
	if proto == protoInet6 {
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
