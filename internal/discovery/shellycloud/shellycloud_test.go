package shellycloud

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
	cloudapi "github.com/suprememoocow/espressif-exporter/internal/shellycloud"
)

type fakeLister struct {
	mu      sync.Mutex
	devices []cloudapi.DeviceStatus
	err     error
}

func (f *fakeLister) AllStatus(context.Context) ([]cloudapi.DeviceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.devices, f.err
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fleet() []cloudapi.DeviceStatus {
	return []cloudapi.DeviceStatus{
		{ID: "a8032ab1c2d3", Gen: 1, Code: "SHSW-PM", Online: true, Addr: netip.MustParseAddr("192.168.1.61")},
		{ID: "a8032ab12345", Gen: 2, Code: "SNSW-001P16EU", Online: true, Addr: netip.MustParseAddr("192.168.1.57")},
		{ID: "dc4f2276846a", Gen: 1, Code: "SHSW-1", Online: false, Addr: netip.MustParseAddr("192.168.1.40")},
		{ID: "7c2e0d112233", Code: "SBDW-002C", Online: true},
	}
}

func drain(out chan discovery.Event) map[string]discovery.Event {
	got := map[string]discovery.Event{}
	for {
		select {
		case ev := <-out:
			got[ev.Instance] = ev
		default:
			return got
		}
	}
}

func TestPollPublishesOnlineDevicesWithAnAddress(t *testing.T) {
	d := New(&fakeLister{devices: fleet()}, 0, false, discardLogger())
	out := make(chan discovery.Event, 16)

	if err := d.poll(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	got := drain(out)

	if len(got) != 2 {
		t.Fatalf("published %d events, want 2 (offline and address-less skipped): %v", len(got), got)
	}
	ev := got["a8032ab12345"]
	if ev.Type != discovery.EventAdd || ev.Kind != discovery.KindShelly || ev.Source != Name {
		t.Errorf("event = %+v, want an add of a shelly from %s", ev, Name)
	}
	// No DeviceID: the registry derives mac: from the mac TXT key, which is what merges
	// this observation with the device's mDNS one.
	if ev.DeviceID != "" {
		t.Errorf("DeviceID = %q, want empty so the registry derives the identity", ev.DeviceID)
	}
	if ev.Endpoint.TXT["mac"] != "a8032ab12345" || ev.Endpoint.TXT["gen"] != "2" {
		t.Errorf("TXT = %v, want mac and gen", ev.Endpoint.TXT)
	}
	if ev.Endpoint.Trusted {
		t.Error("a cloud-reported address must not bypass the address policy")
	}
	if ev.Endpoint.Port != 80 || ev.Endpoint.Addr != netip.MustParseAddr("192.168.1.57") {
		t.Errorf("endpoint = %v:%d, want 192.168.1.57:80", ev.Endpoint.Addr, ev.Endpoint.Port)
	}
	if h := d.Health(); !h.Up || h.LastEventAt.IsZero() {
		t.Errorf("Health() = %+v, want up with a last event time", h)
	}
}

func TestPollIncludesOfflineDevicesWhenAsked(t *testing.T) {
	d := New(&fakeLister{devices: fleet()}, 0, true, discardLogger())
	out := make(chan discovery.Event, 16)

	if err := d.poll(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if got := drain(out); len(got) != 3 {
		t.Errorf("published %d events, want 3 (only the address-less device skipped)", len(got))
	}
}

func TestPollFailureMarksTheBackendDown(t *testing.T) {
	lister := &fakeLister{devices: fleet()}
	d := New(lister, 0, false, discardLogger())
	out := make(chan discovery.Event, 16)

	if err := d.poll(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	drain(out)

	lister.err = errors.New("shelly cloud /device/all_status: HTTP 503")
	if err := d.poll(context.Background(), out); err == nil {
		t.Fatal("poll succeeded despite a failing cloud")
	}
	h := d.Health()
	if h.Up || h.Reason == "" {
		t.Errorf("Health() = %+v, want down with a reason", h)
	}
	if got := drain(out); len(got) != 0 {
		t.Errorf("a failed poll published %d events", len(got))
	}

	lister.err = nil
	if err := d.poll(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if h := d.Health(); !h.Up || h.Reconnects != 1 {
		t.Errorf("Health() = %+v, want up again with one recovery counted", h)
	}
}

func TestRunReturnsOnCancel(t *testing.T) {
	d := New(&fakeLister{err: errors.New("down")}, 0, false, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- d.Run(ctx, make(chan discovery.Event, 1)) }()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on cancel, want nil", err)
	}
}

// An early Gen1 device's cloud id is only the last three MAC bytes. Its status carries the
// full MAC, which is what gives it a mac: identity rather than a weak name: one.
func TestEventPrefersTheStatusMAC(t *testing.T) {
	ev := event(cloudapi.DeviceStatus{
		ID: "a1b2c3", MAC: "5ccf7fa1b2c3", Gen: 1, Online: true,
		Addr: netip.MustParseAddr("192.168.1.18"),
	}, time.Now())
	if got := ev.Endpoint.TXT["mac"]; got != "5ccf7fa1b2c3" {
		t.Errorf("mac TXT = %q, want the status MAC", got)
	}
	if !ev.Unnamed {
		t.Error("Unnamed = false; a cloud id must not outrank the mDNS name")
	}
}
