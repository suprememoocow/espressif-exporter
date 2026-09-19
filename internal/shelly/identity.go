package shelly

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// Identity is a device's generation and firmware metadata.
type Identity struct {
	Gen         int
	MAC         string
	Model       string
	App         string
	FWID        string
	Version     string
	ShellyID    string
	Name        string
	AuthEnabled bool

	fetchedAt time.Time
	epoch     uint64
}

// identityCache holds one Identity per device.
//
// A six-hour TTL keeps the cost at roughly one request in a thousand while still
// surfacing a firmware upgrade the same day. Failures are deliberately not cached: an
// unreachable device is already handled by the probe backoff, and caching the failure
// would add a second, redundant timer that has to be reasoned about separately.
type identityCache struct {
	ttl time.Duration

	mu sync.Mutex
	m  map[string]Identity
}

func newIdentityCache(ttl time.Duration) *identityCache {
	return &identityCache{ttl: ttl, m: map[string]Identity{}}
}

func (c *identityCache) get(deviceID string, epoch uint64, now time.Time) (Identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id, ok := c.m[deviceID]
	if !ok || id.epoch != epoch || now.Sub(id.fetchedAt) > c.ttl {
		return Identity{}, false
	}
	return id, true
}

func (c *identityCache) put(deviceID string, id Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[deviceID] = id
}

func (c *identityCache) forget(deviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, deviceID)
}

func (c *identityCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// shellyResponse is the union of the Gen1 and Gen2+ shapes of GET /shelly.
//
// This endpoint is unauthenticated on both generations — the only one that is — so it
// works even when we have the wrong credentials, which is what makes it a reliable
// generation probe.
type shellyResponse struct {
	// Gen2+ only.
	Gen    int    `json:"gen"`
	ID     string `json:"id"`
	Model  string `json:"model"`
	App    string `json:"app"`
	Ver    string `json:"ver"`
	FWID   string `json:"fw_id"`
	Name   string `json:"name"`
	AuthEn *bool  `json:"auth_en"`

	// Gen1 only.
	Type string `json:"type"`
	FW   string `json:"fw"`
	Auth *bool  `json:"auth"`

	// Both.
	MAC string `json:"mac"`
}

// toIdentity normalises either shape.
//
// Generation is decided by which key is present, never by HTTP status: a Gen2 device
// with authentication enabled still answers /shelly with 200.
func (r shellyResponse) toIdentity() (Identity, error) {
	id := Identity{MAC: normaliseMAC(r.MAC)}

	switch {
	case r.Gen > 0:
		id.Gen = r.Gen
		id.Model, id.App, id.Version, id.FWID = r.Model, r.App, r.Ver, r.FWID
		id.ShellyID, id.Name = r.ID, r.Name
		if r.AuthEn != nil {
			id.AuthEnabled = *r.AuthEn
		}
	case r.Type != "":
		id.Gen = 1
		id.Model = r.Type
		id.FWID = r.FW
		id.Version = gen1Version(r.FW)
		if r.Auth != nil {
			id.AuthEnabled = *r.Auth
		}
	default:
		return Identity{}, fmt.Errorf("GET /shelly has neither a gen nor a type field")
	}

	if id.MAC == "" && id.ShellyID != "" {
		id.MAC = macFromShellyID(id.ShellyID)
	}
	return id, nil
}

// gen1Version pulls the semantic version out of a Gen1 fw string, which looks like
// "20230913-112003/v1.14.0-gcb84623".
func gen1Version(fw string) string {
	_, after, found := strings.Cut(fw, "/")
	if !found {
		return fw
	}
	version, _, _ := strings.Cut(after, "-")
	return strings.TrimPrefix(version, "v")
}

func macFromShellyID(id string) string {
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return ""
	}
	mac := normaliseMAC(id[i+1:])
	if len(mac) != 12 {
		return ""
	}
	return mac
}

// verifyMAC guards against attributing one device's metrics to another.
//
// Because /probe resolves a stable device ID to a current IP, a DHCP reassignment
// between discovery and scrape would otherwise mean we cheerfully scrape whatever now
// answers on that address and label it with the original device's identity. Comparing
// the reported MAC against the one embedded in the ID costs one comparison per identity
// refresh, and is checked on every cold client build — that is, after every address
// change, which is precisely when it matters.
func verifyMAC(dev registry.Device, id Identity) error {
	expected := registry.MACOfID(dev.ID)
	if expected == "" || id.MAC == "" {
		return nil // nothing to compare against
	}
	if expected != id.MAC {
		return fmt.Errorf("device %s reports MAC %s, expected %s: the address was probably "+
			"reassigned by DHCP", dev.ID, id.MAC, expected)
	}
	return nil
}

// fetchIdentity retrieves and caches a device's identity.
func (c *Collector) fetchIdentity(ctx context.Context, dev registry.Device) (Identity, error) {
	now := c.clock()
	if id, ok := c.identities.get(dev.ID, dev.Epoch, now); ok {
		return id, nil
	}

	addr, ok := dev.Primary()
	if !ok {
		return Identity{}, errNoAddress
	}

	// The generation is unknown at this point, so use an unauthenticated client:
	// /shelly needs no credentials on either generation.
	client := c.clients.get(clientKey{deviceID: dev.ID + "|probe", epoch: dev.Epoch}, 0, Credential{})

	var resp shellyResponse
	if err := c.clients.getJSON(ctx, client, addr, dev.Port, "/shelly", &resp); err != nil {
		return Identity{}, err
	}

	id, err := resp.toIdentity()
	if err != nil {
		return Identity{}, err
	}
	if err := verifyMAC(dev, id); err != nil {
		return Identity{}, &macMismatchError{err}
	}

	// Fall back to the mDNS name when the device has none configured.
	if id.Name == "" {
		id.Name = dev.Name
	}
	id.fetchedAt = now
	id.epoch = dev.Epoch
	c.identities.put(dev.ID, id)
	return id, nil
}

type macMismatchError struct{ err error }

func (e *macMismatchError) Error() string { return e.err.Error() }
func (e *macMismatchError) Unwrap() error { return e.err }
