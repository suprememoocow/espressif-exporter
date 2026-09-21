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

// deviceNames is everything the exporter labels with from a device's own configuration: the
// device's name and one name per component.
//
// The device name rides along here rather than on Identity because Gen1's /shelly — the only
// unauthenticated endpoint, and so the only one identity can rely on — does not report it. It
// arrives on the same document as the component names, on the same timer.
type deviceNames struct {
	device     string            // configured device name; "" when the device has none
	components map[string]string // "switch:0" -> "Water Heater"
}

// namesCache holds one device's configured names: its own, and one per component.
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
	names     deviceNames
	fetchedAt time.Time
	epoch     uint64
}

func newNamesCache(ttl time.Duration) *namesCache {
	return &namesCache{ttl: ttl, m: map[string]namesEntry{}}
}

func (c *namesCache) get(deviceID string, epoch uint64, now time.Time) (deviceNames, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.m[deviceID]
	if !ok || e.epoch != epoch || now.Sub(e.fetchedAt) > c.ttl {
		return deviceNames{}, false
	}
	return e.names, true
}

func (c *namesCache) put(deviceID string, epoch uint64, now time.Time, names deviceNames) {
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
	return c.m[deviceID].names.components[key]
}

// ensureNames refreshes a device's configured names when the cache is cold, and returns them
// either way.
//
// It runs only on the cold path, so steady state stays at one status request per scrape.
// Names are cosmetic, so a failure never fails the probe: it is logged and an empty map is
// cached, which bounds the request rate to one attempt per TTL. That is the opposite of
// identityCache, which does not cache failures — but identity has probe backoff protecting
// its hot path, and names do not.
//
// Returning the cached value on the warm path is load-bearing, not a convenience. The identity
// and names caches share a TTL but are written at different instants within a probe, so
// identity expires marginally first. Were the caller to depend on this call having just
// fetched, an identity refresh would reset the device name to the mDNS fallback while this
// returned early, pinning the wrong name for a further six hours.
func (c *Collector) ensureNames(
	ctx context.Context, client *http.Client, dev registry.Device, gen int,
) deviceNames {
	now := c.clock()
	if names, ok := c.names.get(dev.ID, dev.Epoch, now); ok {
		return names
	}

	names, err := c.fetchNames(ctx, client, dev, gen)
	if err != nil {
		c.log.Debug("fetching device configuration failed", "device", dev.ID, "error", err)
		names = deviceNames{components: map[string]string{}}
	}
	c.names.put(dev.ID, dev.Epoch, now, names)
	return names
}

// fetchNames retrieves the configured names for a generation.
func (c *Collector) fetchNames(
	ctx context.Context, client *http.Client, dev registry.Device, gen int,
) (deviceNames, error) {
	if gen >= 2 {
		body, _, err := c.fetch(ctx, client, dev, "/rpc/Shelly.GetConfig")
		if err != nil {
			return deviceNames{}, err
		}
		return decodeGen2Config(body)
	}
	body, _, err := c.fetch(ctx, client, dev, "/settings")
	if err != nil {
		return deviceNames{}, err
	}
	return decodeGen1Settings(body)
}

// decodeGen2Config pulls the configured name out of each component in Shelly.GetConfig.
//
// The document is component-keyed exactly like Shelly.GetStatus, so it is walked the same
// shape-tolerant way, and the keys ("switch:0", "em1:0") already match the status keys.
//
// The device name is deliberately not taken from sys.device.name: Gen2+ report it on /shelly,
// which identity already reads unauthenticated, so sourcing it here as well would make the
// label depend on whether fetch_config is on. Worse, sys.device.name holds the device id for a
// device nobody has named, so an unnamed Gen2 would export "shellyplus1pm-a8032ab12345" in
// place of the mDNS fallback.
func decodeGen2Config(body []byte) (deviceNames, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return deviceNames{}, err
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
	return deviceNames{components: names}, nil
}

// decodeGen1Settings pulls the device name and the per-channel names out of GET /settings.
//
// The channel names are mapped onto the same component keys decodeGen1Status emits, so a lookup
// by "switch:0" works identically across generations. Gen1 meters[] have no name; the switch
// name comes from relays[].
//
// The top-level name is the device name an operator set in the app. Gen1's /shelly omits it
// entirely, so this document is the only place it exists.
func decodeGen1Settings(body []byte) (deviceNames, error) {
	var s struct {
		Name   string `json:"name"`
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
		return deviceNames{}, err
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
	return deviceNames{device: s.Name, components: names}, nil
}
