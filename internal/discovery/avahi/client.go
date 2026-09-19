package avahi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
)

// D-Bus names used by avahi-daemon. They are constants because each one appears in both a
// method call and a signal comparison, where a typo fails silently rather than loudly.
const (
	busName      = "org.freedesktop.Avahi"
	serverIface  = "org.freedesktop.Avahi.Server"
	browserIface = "org.freedesktop.Avahi.ServiceBrowser"
	dbusIface    = "org.freedesktop.DBus"
)

// Avahi's wire constants, from avahi-common/address.h and avahi-common/defs.h.
const (
	interfaceUnspec int32 = -1
	protoUnspec     int32 = -1
	protoInet       int32 = 0
	protoInet6      int32 = 1
)

const (
	// signalBuffer absorbs a burst of announcements. Avahi replays its whole record cache
	// to a new browser, so the first second of a session is the busiest.
	signalBuffer = 256
	// browserBuffer is per service type, and sized for the same burst.
	browserBuffer = 64
)

// Service is one browsed or resolved mDNS service, in avahi-daemon's own terms.
//
// Txt stays [][]byte, which is what avahi sends (an array of byte arrays) and what
// discovery.ParseTXTBytes takes, so no conversion happens on the way through.
type Service struct {
	Interface int32
	Protocol  int32
	Name      string
	Type      string
	Domain    string
	Host      string
	Aprotocol int32
	Address   string
	Port      uint16
	Txt       [][]byte
	Flags     uint32
}

// browser is one ServiceBrowser object living in avahi-daemon.
//
// The channels are read-only to callers and are never closed: closing them is what makes
// a late signal fatal rather than merely stale. A consumer stops by returning from its own
// select on ctx.Done, not by observing a closed channel.
type browser struct {
	path   dbus.ObjectPath
	add    chan Service
	remove chan Service
}

// Add reports services appearing.
func (b *browser) Add() <-chan Service { return b.add }

// Remove reports services going away. Avahi sends these on a goodbye packet, which is
// routinely lost, so they are a hint rather than an instruction.
func (b *browser) Remove() <-chan Service { return b.remove }

// client is a minimal avahi-daemon D-Bus client: three method calls, two signals.
//
// It exists instead of github.com/holoplot/go-avahi, which closes its unbuffered browser
// channels from inside its own signal-dispatch goroutine. Freeing a browser while a browse
// hit is in flight therefore panics with "close of closed channel" or "send on closed
// channel", in a goroutine the caller does not own and so cannot recover.
type client struct {
	conn *dbus.Conn
	obj  dbus.BusObject
	log  *slog.Logger

	signals chan *dbus.Signal
	owners  chan string

	mu       sync.Mutex
	browsers map[dbus.ObjectPath]*browser
	closed   bool

	drops  atomic.Uint64
	router sync.WaitGroup
}

// newClient subscribes to the signals this backend needs and starts routing them.
func newClient(conn *dbus.Conn, log *slog.Logger) (*client, error) {
	c := &client{
		conn:     conn,
		obj:      conn.Object(busName, dbus.ObjectPath("/")),
		log:      log,
		signals:  make(chan *dbus.Signal, signalBuffer),
		owners:   make(chan string, 8),
		browsers: map[dbus.ObjectPath]*browser{},
	}

	// Only NameOwnerChanged needs a match rule. Browser signals need none: avahi-daemon
	// sends ItemNew and ItemRemove directed at the unique name of the client that created
	// the browser, and dbus-daemon delivers a directed message without consulting match
	// rules at all.
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(dbusIface),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, busName),
	); err != nil {
		return nil, fmt.Errorf("watching for avahi restarts: %w", err)
	}

	// godbus fans every signal this connection receives into every channel registered
	// here, so routing by member and object path below is not an optimisation: it is the
	// only thing that keeps a browse hit from being read as something else.
	conn.Signal(c.signals)

	c.router.Add(1)
	go c.route()

	return c, nil
}

// route dispatches signals until the connection closes.
func (c *client) route() {
	defer c.router.Done()
	// Closing the owner watch on the way out reports a lost connection at once. Without
	// it the session would look healthy until the next liveness probe timed out, twice.
	// Nothing can send on it after this point, because only this goroutine ever does.
	defer close(c.owners)

	// A range loop is deliberate: godbus closes this channel from Conn.Close, and a
	// receive that merely skips a closed channel spins a core forever.
	for sig := range c.signals {
		switch sig.Name {
		case browserIface + ".ItemNew":
			c.deliverItem(sig, true)
		case browserIface + ".ItemRemove":
			c.deliverItem(sig, false)
		case dbusIface + ".NameOwnerChanged":
			c.deliverOwner(sig)
		}
	}
}

// deliverItem hands a browse hit to the browser that asked for it.
func (c *client) deliverItem(sig *dbus.Signal, added bool) {
	var svc Service
	if err := dbus.Store(sig.Body,
		&svc.Interface, &svc.Protocol, &svc.Name, &svc.Type, &svc.Domain, &svc.Flags,
	); err != nil {
		c.log.Debug("unparseable browser signal", "signal", sig.Name, "error", err)
		return
	}

	c.mu.Lock()
	b := c.browsers[sig.Path]
	c.mu.Unlock()
	if b == nil {
		// The session is tearing down, or the path belongs to another client's object.
		// Either way there is nobody left to tell.
		return
	}

	ch := b.add
	if !added {
		ch = b.remove
	}
	select {
	case ch <- svc:
	default:
		// Dropping is recoverable, because avahi re-announces, the periodic rebrowse
		// re-queries, and the registry re-resolves what it already knows. Blocking here
		// would stall every other browser, and closing the channel would crash.
		c.drops.Add(1)
	}
}

// deliverOwner reports that avahi-daemon came or went.
func (c *client) deliverOwner(sig *dbus.Signal) {
	var name, oldOwner, newOwner string
	if err := dbus.Store(sig.Body, &name, &oldOwner, &newOwner); err != nil {
		return
	}
	if name != busName {
		return
	}
	select {
	case c.owners <- newOwner:
	default:
		// One pending restart notice is as actionable as ten.
	}
}

// OwnerChanges reports changes of ownership of the org.freedesktop.Avahi bus name. An
// empty string means the name was released, which is avahi-daemon going away.
func (c *client) OwnerChanges() <-chan string { return c.owners }

// ServiceBrowserNew starts a browse for one service type.
func (c *client) ServiceBrowserNew(
	ctx context.Context, iface, protocol int32, serviceType, domain string, flags uint32,
) (*browser, error) {
	var path dbus.ObjectPath
	err := c.obj.CallWithContext(ctx, serverIface+".ServiceBrowserNew", 0,
		iface, protocol, serviceType, domain, flags).Store(&path)
	if err != nil {
		return nil, err
	}

	b := &browser{
		path:   path,
		add:    make(chan Service, browserBuffer),
		remove: make(chan Service, browserBuffer),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("client is closed")
	}
	c.browsers[path] = b
	return b, nil
}

// ResolveService turns a browse hit into an address, port and TXT record in one call.
func (c *client) ResolveService(
	ctx context.Context, iface, protocol int32, name, serviceType, domain string,
	aprotocol int32, flags uint32,
) (Service, error) {
	var svc Service
	err := c.obj.CallWithContext(ctx, serverIface+".ResolveService", 0,
		iface, protocol, name, serviceType, domain, aprotocol, flags).
		Store(&svc.Interface, &svc.Protocol, &svc.Name, &svc.Type, &svc.Domain,
			&svc.Host, &svc.Aprotocol, &svc.Address, &svc.Port, &svc.Txt, &svc.Flags)
	if err != nil {
		return Service{}, err
	}
	return svc, nil
}

// GetHostName is also the liveness probe: it is the cheapest round trip that proves
// avahi-daemon is still answering rather than merely connected.
func (c *client) GetHostName(ctx context.Context) (string, error) {
	var name string
	if err := c.obj.CallWithContext(ctx, serverIface+".GetHostName", 0).Store(&name); err != nil {
		return "", err
	}
	return name, nil
}

// Close ends the session. It is idempotent.
//
// Browser objects are not freed individually: avahi-daemon destroys everything a client
// owns when its connection drops, and a session only ever ends by dropping it.
func (c *client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.browsers = map[dbus.ObjectPath]*browser{}
	c.mu.Unlock()

	// Closing the connection closes the signal channel, which is what stops the router.
	// RemoveSignal deliberately is not called: it would unregister the channel and leave
	// nothing to close it, so the router would never return.
	_ = c.conn.Close()
	c.router.Wait()

	if n := c.drops.Load(); n > 0 {
		c.log.Warn("dropped browse notifications during the session; avahi re-announces, "+
			"so this costs discovery latency rather than devices", "count", n)
	}
}
