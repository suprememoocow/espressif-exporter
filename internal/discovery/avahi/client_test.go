package avahi

import (
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// routerOnly builds a client with no D-Bus connection, so signal routing — the part that
// decides what a signal means — can be driven directly.
func routerOnly(t *testing.T) *client {
	t.Helper()
	c := &client{
		log:      discardLogger(),
		signals:  make(chan *dbus.Signal, 16),
		owners:   make(chan string, 4),
		browsers: map[dbus.ObjectPath]*browser{},
	}
	c.router.Add(1)
	go c.route()
	t.Cleanup(func() {
		close(c.signals)
		c.router.Wait()
	})
	return c
}

func registerBrowser(c *client, path dbus.ObjectPath, depth int) *browser {
	b := &browser{
		path:   path,
		add:    make(chan Service, depth),
		remove: make(chan Service, depth),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.browsers[path] = b
	return b
}

func itemSignal(path dbus.ObjectPath, member, name string) *dbus.Signal {
	return &dbus.Signal{
		Sender: ":1.123",
		Path:   path,
		Name:   browserIface + "." + member,
		Body: []any{
			int32(2), protoInet, name, discovery.ServiceShelly, "local", uint32(0),
		},
	}
}

func ownerSignal(name, oldOwner, newOwner string) *dbus.Signal {
	return &dbus.Signal{
		Sender: "org.freedesktop.DBus",
		Path:   "/org/freedesktop/DBus",
		Name:   dbusIface + ".NameOwnerChanged",
		Body:   []any{name, oldOwner, newOwner},
	}
}

func awaitService(t *testing.T, ch <-chan Service) Service {
	t.Helper()
	select {
	case svc, ok := <-ch:
		if !ok {
			t.Fatal("the channel was closed; a late signal would then be fatal rather than stale")
		}
		return svc
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was delivered within 2s")
		return Service{}
	}
}

func assertEmpty[T any](t *testing.T, what string, ch <-chan T) {
	t.Helper()
	// Give the router a moment to have done the wrong thing.
	time.Sleep(20 * time.Millisecond)
	select {
	case v := <-ch:
		t.Fatalf("%s received %v, want nothing", what, v)
	default:
	}
}

func TestRoutesItemNewToTheOwningBrowser(t *testing.T) {
	c := routerOnly(t)
	shelly := registerBrowser(c, "/Client1/ServiceBrowser1", 4)
	esphome := registerBrowser(c, "/Client1/ServiceBrowser2", 4)

	c.signals <- itemSignal(shelly.path, "ItemNew", "shelly1pmg3-dcb4d9cc6eec")

	if got := awaitService(t, shelly.Add()).Name; got != "shelly1pmg3-dcb4d9cc6eec" {
		t.Errorf("name = %q", got)
	}
	assertEmpty(t, "the other browser", esphome.Add())
}

func TestRoutesItemRemoveToTheRemoveChannel(t *testing.T) {
	c := routerOnly(t)
	b := registerBrowser(c, "/Client1/ServiceBrowser1", 4)

	c.signals <- itemSignal(b.path, "ItemRemove", "shelly1g3-48f6ee8adf78")

	if got := awaitService(t, b.Remove()).Name; got != "shelly1g3-48f6ee8adf78" {
		t.Errorf("name = %q", got)
	}
	assertEmpty(t, "the add channel", b.Add())
}

// The regression that mattered most: godbus fans every signal into every channel
// registered on a connection, so a browse hit used to arrive at the code watching for
// avahi restarts, whose third body field happened to be non-empty. Every device found
// therefore looked like a restart.
func TestItemNewNeverReachesTheOwnerWatch(t *testing.T) {
	c := routerOnly(t)
	b := registerBrowser(c, "/Client1/ServiceBrowser1", 4)

	c.signals <- itemSignal(b.path, "ItemNew", "shelly1pmg3-dcb4d9cc6eec")
	awaitService(t, b.Add())

	assertEmpty(t, "the owner watch", c.OwnerChanges())
}

func TestOwnerChangeIsRoutedAndFiltered(t *testing.T) {
	c := routerOnly(t)

	c.signals <- ownerSignal("org.freedesktop.systemd1", ":1.4", ":1.9")
	assertEmpty(t, "the owner watch", c.OwnerChanges())

	c.signals <- ownerSignal(busName, ":1.4", "")
	select {
	case owner := <-c.OwnerChanges():
		if owner != "" {
			t.Errorf("owner = %q, want the empty string for a released name", owner)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the name loss was not reported")
	}
}

// A signal for a browser that is no longer registered has nobody to go to. Delivering it
// anyway is what panicked the previous client library, which closed the browser's channel
// from this goroutine.
func TestSignalForAnUnknownBrowserIsDropped(t *testing.T) {
	c := routerOnly(t)
	b := registerBrowser(c, "/Client1/ServiceBrowser1", 4)

	c.signals <- itemSignal("/Client1/ServiceBrowser99", "ItemNew", "stranger")
	assertEmpty(t, "the registered browser", b.Add())
}

func TestUnparseableSignalBodyIsIgnored(t *testing.T) {
	c := routerOnly(t)
	b := registerBrowser(c, "/Client1/ServiceBrowser1", 4)

	bad := itemSignal(b.path, "ItemNew", "shelly")
	bad.Body = []any{int32(2)}
	c.signals <- bad

	assertEmpty(t, "the browser", b.Add())
}

// A consumer that has stopped reading must cost browse hits, not the process.
func TestFullBrowserChannelDropsRatherThanBlocks(t *testing.T) {
	c := routerOnly(t)
	b := registerBrowser(c, "/Client1/ServiceBrowser1", 1)

	for i := 0; i < 3; i++ {
		c.signals <- itemSignal(b.path, "ItemNew", "shelly1pmg3-dcb4d9cc6eec")
	}

	// Count the drops before draining, or a freed buffer slot takes one of them.
	deadline := time.Now().Add(2 * time.Second)
	for c.drops.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.drops.Load(); got != 2 {
		t.Errorf("drops = %d, want 2", got)
	}

	// The one that fitted is still there, on a channel that is still open.
	if svc := awaitService(t, b.Add()); svc.Name == "" {
		t.Error("the buffered hit should have been delivered")
	}
}

// A connection that dies on its own closes the signal channel, and the watcher has to
// hear about it immediately rather than waiting for two liveness probes to time out.
func TestRouterExitClosesTheOwnerWatch(t *testing.T) {
	c := &client{
		log:      discardLogger(),
		signals:  make(chan *dbus.Signal, 4),
		owners:   make(chan string, 4),
		browsers: map[dbus.ObjectPath]*browser{},
	}
	c.router.Add(1)
	go c.route()

	close(c.signals) // what godbus does when the connection goes away
	c.router.Wait()

	if _, ok := <-c.OwnerChanges(); ok {
		t.Error("the owner watch should be closed once routing has stopped")
	}
}
