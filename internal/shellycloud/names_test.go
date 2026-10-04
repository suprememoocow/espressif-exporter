package shellycloud

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRefreshJoinsNamesAndRooms(t *testing.T) {
	_, c := newFakeCloud(t)
	s := NewNameStore(c, time.Hour, discardLogger())
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := *s.names.Load()

	for _, tc := range []struct {
		desc     string
		id       string
		device   string
		room     string
		channels map[int]Channel
	}{
		{
			desc: "single channel: its name is the device's name; numeric room_id",
			id:   "a8032ab12345", device: "Kitchen Lamp", room: "Kitchen",
			channels: map[int]Channel{0: {"Kitchen Lamp", "relay"}},
		},
		{
			desc: "string room_id",
			id:   "a8032ab1c2d3", device: "Water Heater Supply", room: "Utility Room",
			channels: map[int]Channel{0: {"Water Heater Supply", "relay"}},
		},
		{
			desc: "two channels: no device name, since the first output's name is not the device's",
			id:   "485519aabbcc", device: "", room: "Kitchen",
			channels: map[int]Channel{0: {"Landing", "relay"}, 1: {"Hallway", "relay"}},
		},
		{
			desc: "energy meter channels keep their category",
			id:   "244cab0a0b0c", device: "", room: "Utility Room",
			channels: map[int]Channel{
				0: {"Kitchen and Dining Room Sockets EM", "emeter"},
				1: {"Upstairs Hall and Spare Room EM", "emeter"},
			},
		},
		{
			desc: "unnamed placeholder dropped, whitespace trimmed, hidden room empty",
			id:   "c45bbe010203", device: "", room: "",
			channels: map[int]Channel{1: {"Garage Door", "relay"}},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			n, ok := got[tc.id]
			if !ok {
				t.Fatalf("%s missing; got %v", tc.id, got)
			}
			if n.Device != tc.device || n.Room != tc.room {
				t.Errorf("device=%q room=%q, want device=%q room=%q", n.Device, n.Room, tc.device, tc.room)
			}
			if len(n.Channels) != len(tc.channels) {
				t.Errorf("channels = %v, want %v", n.Channels, tc.channels)
			}
			for i, want := range tc.channels {
				if got, _ := n.Channel(i); got != want {
					t.Errorf("channel %d = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// Names are cosmetic, and dropping them on a transient outage would rename every series
// and then rename them back, so a failed refresh keeps what the last good one found.
func TestNameStoreKeepsTheLastGoodNamesOnFailure(t *testing.T) {
	f, c := newFakeCloud(t)
	s := NewNameStore(c, time.Hour, discardLogger())

	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Loaded():
	default:
		t.Fatal("Loaded not closed after the first refresh")
	}
	if up, _ := s.Health(); !up {
		t.Fatal("store not up after a successful refresh")
	}

	f.status["/interface/device/list"] = http.StatusBadGateway
	if err := s.refresh(context.Background()); err == nil {
		t.Fatal("refresh reported success despite a failing device list")
	}

	if up, last := s.Health(); up || last.IsZero() {
		t.Errorf("Health() = %v, %v; want down with the earlier success time kept", up, last)
	}
	// Any common MAC notation finds the device.
	n, ok := s.Lookup("A8:03:2A:B1:23:45")
	if !ok || n.Device != "Kitchen Lamp" {
		t.Errorf("Lookup after a failed refresh = %+v, %v; want the previous names", n, ok)
	}
}

// A refused room list costs the area label at most: names still refresh, and the rooms
// from the last good list stand in.
func TestRoomListFailureKeepsNamesAndPreviousRooms(t *testing.T) {
	f, c := newFakeCloud(t)
	s := NewNameStore(c, time.Hour, discardLogger())
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.status["/interface/room/list"] = http.StatusBadGateway
	f.bodies["/interface/device/list"] = `{"isok":true,"data":{"devices":{
		"a8032ab12345":{"name":"Kitchen Island Pendant","room_id":3}}}}`
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("a room list failure failed the refresh: %v", err)
	}

	n, ok := s.Lookup("a8032ab12345")
	if !ok || n.Device != "Kitchen Island Pendant" || n.Room != "Kitchen" {
		t.Errorf("Lookup = %+v, %v; want the new name with the previous room", n, ok)
	}
}

// A failed first refresh still releases probes waiting on Loaded: they proceed with the
// devices' own names rather than waiting for the cloud to recover.
func TestNameStoreLoadedEvenWhenTheFirstRefreshFails(t *testing.T) {
	f, c := newFakeCloud(t)
	f.status["/interface/device/list"] = http.StatusBadGateway
	s := NewNameStore(c, time.Hour, discardLogger())

	if err := s.refresh(context.Background()); err == nil {
		t.Fatal("refresh reported success despite a failing device list")
	}
	select {
	case <-s.Loaded():
	default:
		t.Fatal("Loaded not closed after a failed first refresh")
	}
	if _, ok := s.Lookup("a8032ab12345"); ok {
		t.Error("Lookup found names although no refresh has succeeded")
	}
}

func TestSplitChannelKey(t *testing.T) {
	for key, want := range map[string]struct {
		id string
		ch int
	}{
		"a8032ab12345":   {"a8032ab12345", 0},
		"A8032AB12345_1": {"a8032ab12345", 1},
		"a8032ab12345_x": {"a8032ab12345_x", 0},
	} {
		if id, ch := splitChannelKey(key); id != want.id || ch != want.ch {
			t.Errorf("splitChannelKey(%q) = %q, %d; want %q, %d", key, id, ch, want.id, want.ch)
		}
	}
}

// Early Gen1 devices are keyed in the cloud by the last three MAC bytes only, while the
// collector looks names up by the full MAC from /shelly.
func TestLookupFallsBackToTheLegacyShortID(t *testing.T) {
	_, c := newFakeCloud(t)
	s := NewNameStore(c, time.Hour, discardLogger())
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	n, ok := s.Lookup("5C:CF:7F:A1:B2:C3")
	if !ok || n.Device != "Awning Strip Lights" {
		t.Errorf("Lookup = %+v, %v; want the device keyed a1b2c3", n, ok)
	}
	if _, ok := s.Lookup("5ccf7fffffff"); ok {
		t.Error("Lookup matched an unrelated MAC")
	}
}
