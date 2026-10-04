package shellycloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hiddenRoom is the room_id the Shelly app gives devices the operator has hidden.
const hiddenRoom = "-10"

// Names is what the Shelly app knows a device as.
type Names struct {
	// Device is the device's name, set only for single-channel devices. The cloud names
	// channels, not devices: a two-channel device is two tiles in the app, and naming the
	// whole device after its first output would mislabel every non-output series.
	Device string

	// Room is the name of the room the device's first channel is in, or "" when it is in
	// none or is hidden.
	Room string

	// Channels maps a channel index to the app's name for it and what kind of channel it
	// is. Only named channels are present.
	Channels map[int]Channel
}

// Channel is one tile in the Shelly app.
type Channel struct {
	Name string

	// Category is what the channel is, in the cloud's vocabulary: relay, light, emeter,
	// roller and so on. It decides which component the name belongs to: on a Shelly EM,
	// channel 0 is the first meter, not the relay.
	Category string
}

// Channel returns the channel at an index, if the app names it.
func (n Names) Channel(i int) (Channel, bool) {
	ch, ok := n.Channels[i]
	return ch, ok
}

// channelEntry is one entry of /interface/device/list. Channel 0 is keyed by the bare
// device id and channel n by "<id>_<n>".
type channelEntry struct {
	Name     text `json:"name"`
	Category text `json:"category"`
	RoomID   text `json:"room_id"`
}

// DeviceList reads the raw per-channel device list. Join it with Rooms via decodeNames.
func (c *Client) DeviceList(ctx context.Context) (json.RawMessage, error) {
	return c.post(ctx, "/interface/device/list", nil)
}

// Rooms maps room id to room name. Hidden devices' room is omitted.
func (c *Client) Rooms(ctx context.Context) (map[string]string, error) {
	data, err := c.post(ctx, "/interface/room/list", nil)
	if err != nil {
		return nil, err
	}
	return decodeRooms(data), nil
}

// decodeNames joins the device list with the room names, keyed by device id.
func decodeNames(devicesData json.RawMessage, rooms map[string]string) (map[string]Names, error) {
	var devices struct {
		Devices map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(devicesData, &devices); err != nil {
		return nil, fmt.Errorf("shelly cloud device list: %w", err)
	}

	type channel struct {
		index    int
		name     string
		category string
		room     string
	}
	byDevice := map[string][]channel{}
	for key, raw := range devices.Devices {
		var e channelEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		id, index := splitChannelKey(key)
		byDevice[id] = append(byDevice[id], channel{
			index:    index,
			name:     cleanName(string(e.Name)),
			category: strings.ToLower(strings.TrimSpace(string(e.Category))),
			room:     rooms[strings.TrimSpace(string(e.RoomID))],
		})
	}

	out := make(map[string]Names, len(byDevice))
	for id, chans := range byDevice {
		sort.Slice(chans, func(i, j int) bool { return chans[i].index < chans[j].index })
		n := Names{Channels: map[int]Channel{}}
		for _, ch := range chans {
			if ch.name != "" {
				n.Channels[ch.index] = Channel{Name: ch.name, Category: ch.category}
			}
			if n.Room == "" {
				n.Room = ch.room
			}
		}
		if len(chans) == 1 && chans[0].index == 0 {
			n.Device = chans[0].name
		}
		out[id] = n
	}
	return out, nil
}

// decodeRooms maps room id to name. A failure to decode is not fatal: it costs the area
// label, not the names.
func decodeRooms(data json.RawMessage) map[string]string {
	var body struct {
		Rooms map[string]struct {
			Name text `json:"name"`
		} `json:"rooms"`
	}
	out := map[string]string{}
	if err := json.Unmarshal(data, &body); err != nil {
		return out
	}
	for id, r := range body.Rooms {
		if id != hiddenRoom {
			out[id] = strings.TrimSpace(string(r.Name))
		}
	}
	return out
}

// splitChannelKey splits "a8032ab12345_1" into ("a8032ab12345", 1). A key with no
// numeric suffix is channel 0.
func splitChannelKey(key string) (string, int) {
	key = strings.ToLower(key)
	if i := strings.LastIndexByte(key, '_'); i > 0 {
		if n, err := strconv.Atoi(key[i+1:]); err == nil && n >= 0 {
			return key[:i], n
		}
	}
	return key, 0
}

// cleanName treats the app's placeholder for an unset name as no name.
func cleanName(s string) string {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "unnamed") {
		return ""
	}
	return s
}

// Retry delays after a failed refresh. The cap is the refresh interval itself: retrying
// more slowly than we would refresh anyway would only make an outage last longer.
const (
	retryInitial = 30 * time.Second
	retryFactor  = 2
)

// NameStore keeps the account's names in memory, refreshing them in the background, so
// that a scrape reads them without making a request.
type NameStore struct {
	client   *Client
	interval time.Duration
	log      *slog.Logger

	// rooms survives a failed room-list request, so a refusal costs nothing until a room
	// actually changes. Owned by the Run goroutine.
	rooms map[string]string

	names  atomic.Pointer[map[string]Names]
	loaded chan struct{}

	mu          sync.Mutex
	up          bool
	attempted   bool
	lastSuccess time.Time
}

// NewNameStore builds a store. Run must be called to populate it.
func NewNameStore(client *Client, interval time.Duration, log *slog.Logger) *NameStore {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	return &NameStore{
		client:   client,
		interval: interval,
		log:      log.With("component", "shelly_cloud_names"),
		loaded:   make(chan struct{}),
	}
}

// Loaded is closed once the first refresh has finished, whether or not it succeeded.
func (s *NameStore) Loaded() <-chan struct{} { return s.loaded }

// Lookup returns the names for a device by MAC, in any common notation.
//
// The cloud keys most devices by their full MAC, but early Gen1 devices by only the last
// three bytes (an RGBW2 with MAC 5CCF7FA1B2C3 is "a1b2c3"), so a miss on the full MAC
// falls back to that short form.
func (s *NameStore) Lookup(mac string) (Names, bool) {
	m := s.names.Load()
	if m == nil {
		return Names{}, false
	}
	mac = normaliseMAC(mac)
	if n, ok := (*m)[mac]; ok {
		return n, true
	}
	if len(mac) == 12 {
		n, ok := (*m)[mac[6:]]
		return n, ok
	}
	return Names{}, false
}

// Health reports whether the last refresh succeeded, and when one last did.
func (s *NameStore) Health() (up bool, lastSuccess time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.up, s.lastSuccess
}

// Run refreshes immediately, then on every interval, until ctx is done. A failed refresh
// is retried with backoff rather than after a full interval, so that a refusal at startup
// does not leave every device on its own names for the next fifteen minutes.
//
// A failed refresh keeps the previous names: they are cosmetic, and dropping them on a
// transient cloud outage would rename every series and then rename them back.
func (s *NameStore) Run(ctx context.Context) error {
	retry := retryInitial
	for {
		wait := s.interval
		if err := s.refresh(ctx); err != nil {
			wait = min(retry, s.interval)
			retry = min(retry*retryFactor, s.interval)
		} else {
			retry = retryInitial
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func (s *NameStore) refresh(ctx context.Context) error {
	devices, err := s.client.DeviceList(ctx)
	var names map[string]Names
	if err == nil {
		// Rooms are fetched only once the device list is in hand, and a failure costs the
		// area label alone: the last good room names stand in.
		if rooms, rerr := s.client.Rooms(ctx); rerr == nil {
			s.rooms = rooms
		} else if ctx.Err() == nil {
			s.log.Warn("refreshing rooms from Shelly Cloud failed; keeping the previous rooms",
				"error", rerr)
		}
		names, err = decodeNames(devices, s.rooms)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	first := !s.attempted
	s.attempted = true
	if first {
		defer close(s.loaded)
	}
	if err != nil {
		// Log the transition, not every retry: a revoked key would otherwise warn forever.
		if s.up || first {
			s.log.Warn("refreshing names from Shelly Cloud failed; keeping the previous names",
				"error", err)
		}
		s.up = false
		return err
	}
	if !s.up && !first {
		s.log.Info("refreshing names from Shelly Cloud recovered")
	}
	s.names.Store(&names)
	s.up, s.lastSuccess = true, time.Now()
	s.log.Debug("refreshed names from Shelly Cloud", "devices", len(names))
	return nil
}

func normaliseMAC(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
