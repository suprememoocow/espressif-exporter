package shelly

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// namesCache holds one device's per-component names, keyed "switch:0" -> name.
//
// It mirrors identityCache deliberately: names are refreshed on the same six-hour timer,
// and both an epoch change (a DHCP reassignment) and TTL expiry must invalidate the entry.
// A populated map with no invalidation would attribute a stale name after a rename.
type namesCache struct {
	ttl time.Duration

	mu sync.Mutex
	m  map[string]namesEntry
}

type namesEntry struct {
	names     map[string]string
	fetchedAt time.Time
	epoch     uint64
}

func newNamesCache(ttl time.Duration) *namesCache {
	return &namesCache{ttl: ttl, m: map[string]namesEntry{}}
}

func (c *namesCache) get(deviceID string, epoch uint64, now time.Time) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.m[deviceID]
	if !ok || e.epoch != epoch || now.Sub(e.fetchedAt) > c.ttl {
		return nil, false
	}
	return e.names, true
}

func (c *namesCache) put(deviceID string, epoch uint64, now time.Time, names map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[deviceID] = namesEntry{names: names, fetchedAt: now, epoch: epoch}
}

func (c *namesCache) forget(deviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, deviceID)
}

// lookup returns a device's name for a component key, or "".
func (c *namesCache) lookup(deviceID, key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[deviceID].names[key]
}

// ensureNames refreshes a device's component names when the cache is cold.
//
// It runs only on the cold path, so steady state stays at one status request per scrape.
// Names are cosmetic, so a failure never fails the probe: it is logged and an empty map is
// cached, which bounds the request rate to one attempt per TTL. That is the opposite of
// identityCache, which does not cache failures — but identity has probe backoff protecting
// its hot path, and names do not.
func (c *Collector) ensureNames(ctx context.Context, client *http.Client, dev registry.Device, gen int) {
	now := c.clock()
	if _, ok := c.names.get(dev.ID, dev.Epoch, now); ok {
		return
	}

	names, err := c.fetchNames(ctx, client, dev, gen)
	if err != nil {
		c.log.Debug("fetching component names failed", "device", dev.ID, "error", err)
		names = map[string]string{}
	}
	c.names.put(dev.ID, dev.Epoch, now, names)
}

// fetchNames retrieves the per-component names for a generation.
func (c *Collector) fetchNames(
	ctx context.Context, client *http.Client, dev registry.Device, gen int,
) (map[string]string, error) {
	if gen >= 2 {
		body, _, err := c.fetch(ctx, client, dev, "/rpc/Shelly.GetConfig")
		if err != nil {
			return nil, err
		}
		return decodeGen2Config(body)
	}
	body, _, err := c.fetch(ctx, client, dev, "/settings")
	if err != nil {
		return nil, err
	}
	return decodeGen1Settings(body)
}

// decodeGen2Config pulls the configured name out of each component in Shelly.GetConfig.
//
// The document is component-keyed exactly like Shelly.GetStatus, so it is walked the same
// shape-tolerant way, and the keys ("switch:0", "em1:0") already match the status keys.
func decodeGen2Config(body []byte) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	names := map[string]string{}
	for key, msg := range raw {
		var cfg struct {
			Name *string `json:"name"`
		}
		if err := json.Unmarshal(msg, &cfg); err != nil {
			continue // a scalar or array at the top level is not a component
		}
		if cfg.Name != nil && *cfg.Name != "" {
			names[key] = *cfg.Name
		}
	}
	return names, nil
}

// decodeGen1Settings pulls per-channel names out of GET /settings.
//
// The names are mapped onto the same component keys decodeGen1Status emits, so a lookup by
// "switch:0" works identically across generations. Gen1 meters[] have no name; the switch
// name comes from relays[].
func decodeGen1Settings(body []byte) (map[string]string, error) {
	var s struct {
		Relays []struct {
			Name string `json:"name"`
		} `json:"relays"`
		EMeters []struct {
			Name string `json:"name"`
		} `json:"emeters"`
		Inputs []struct {
			Name string `json:"name"`
		} `json:"inputs"`
		Lights []struct {
			Name string `json:"name"`
		} `json:"lights"`
	}
	if err := decodeJSON(body, &s); err != nil {
		return nil, err
	}

	names := map[string]string{}
	add := func(component string, i int, name string) {
		if name != "" {
			names[component+":"+strconv.Itoa(i)] = name
		}
	}
	for i, r := range s.Relays {
		add("switch", i, r.Name)
	}
	for i, m := range s.EMeters {
		add("em1", i, m.Name)
	}
	for i, in := range s.Inputs {
		add("input", i, in.Name)
	}
	for i, l := range s.Lights {
		add("light", i, l.Name)
	}
	return names, nil
}
