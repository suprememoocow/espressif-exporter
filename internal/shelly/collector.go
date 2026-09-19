// Package shelly collects metrics from Shelly devices of any generation.
//
// Gen1 and Gen2+ speak entirely different protocols — legacy REST versus JSON-RPC — but
// deliberately emit the same metric families wherever the readings mean the same thing,
// so one dashboard panel works across the whole fleet without an `or` clause.
package shelly

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

var errNoAddress = errors.New("device has no routable address")

// Collector implements probe.Prober for Shelly devices.
type Collector struct {
	cfg             config.Shelly
	log             *slog.Logger
	families        *metrics.Registry
	clock           func() time.Time
	genericFallback bool

	clients    *clientCache
	identities *identityCache
	auth       *authResolver

	unknownComponents *unknownCounter

	namesMu sync.RWMutex
	names   map[string]map[string]string // device ID -> "switch:0" -> name
}

// New builds the collector.
func New(cfg config.Shelly, families *metrics.Registry, log *slog.Logger) *Collector {
	log = log.With("component", "shelly")
	return &Collector{
		cfg:               cfg,
		log:               log,
		families:          families,
		clock:             time.Now,
		genericFallback:   cfg.GenericFallback,
		clients:           newClientCache(cfg),
		identities:        newIdentityCache(cfg.IdentityTTL),
		auth:              newAuthResolver(cfg.Auth, log),
		unknownComponents: newUnknownCounter(),
		names:             map[string]map[string]string{},
	}
}

// Kind implements probe.Prober.
func (c *Collector) Kind() string { return "shelly" }

// UnknownComponents reports how often each uncurated component type has been seen.
func (c *Collector) UnknownComponents() map[string]uint64 { return c.unknownComponents.snapshot() }

// IdentityCacheSize reports cached identities, for self-metrics.
func (c *Collector) IdentityCacheSize() int { return c.identities.len() }

// Forget drops all cached state for a device, for use when its address changes.
func (c *Collector) Forget(deviceID string) {
	c.clients.forget(deviceID)
	c.identities.forget(deviceID)
	c.namesMu.Lock()
	delete(c.names, deviceID)
	c.namesMu.Unlock()
}

// Probe collects one device's metrics.
//
// In steady state this is exactly one HTTP request per device per scrape, on a
// keep-alive connection. Identity and per-component names are refreshed on a six-hour
// timer, never on the hot path.
func (c *Collector) Probe(
	ctx context.Context, dev registry.Device,
) ([]prometheus.Metric, probe.Result) {
	addr, ok := dev.Primary()
	if !ok {
		return nil, probe.Fail(probe.ReasonNoAddress, 0)
	}

	id, err := c.fetchIdentity(ctx, dev)
	if err != nil {
		var mismatch *macMismatchError
		if errors.As(err, &mismatch) {
			// Never emit device metrics under a mismatched identity: doing so would
			// silently attribute one device's readings to another.
			c.log.Warn("MAC mismatch; refusing to attribute metrics", "device", dev.ID, "error", err)
			return nil, probe.Fail(probe.ReasonMACMismatch, 0)
		}
		return nil, probe.Fail(classify(err), statusOf(err))
	}

	cred := c.auth.resolve(deviceMatch{
		DeviceID: dev.ID,
		MAC:      id.MAC,
		Hostname: dev.Hostname,
		Gen:      id.Gen,
	})
	client := c.clients.get(clientKey{deviceID: dev.ID, epoch: dev.Epoch}, id.Gen, cred)

	base := metrics.Labels{Device: dev.ID, Kind: "shelly"}
	e := metrics.NewEmitter(c.families, base)

	e.Info(metrics.FamilyDeviceInfo,
		id.MAC, id.Model, "Shelly", id.Version, itoa(id.Gen), addr.String(), "http", id.Name)
	e.Bool(metrics.FamilyAuthRequired, id.AuthEnabled)

	path, decode := c.plan(id.Gen)
	body, status, err := c.fetch(ctx, client, dev, path)
	if err != nil {
		return nil, probe.Fail(classify(err), statusOf(err))
	}
	if err := decode(e, body); err != nil {
		c.log.Debug("decoding status failed", "device", dev.ID, "path", path, "error", err)
		return nil, probe.Fail(probe.ReasonDecode, status)
	}

	if errs := e.Errs(); len(errs) > 0 {
		// Emission errors are always programming errors, never device faults, so they
		// are logged loudly but do not fail the probe or trigger backoff.
		c.log.Error("metric emission errors", "device", dev.ID, "errors", errs)
	}
	return e.Metrics(), probe.Success(status)
}

// plan picks the status endpoint and decoder for a generation.
func (c *Collector) plan(gen int) (string, func(*metrics.Emitter, []byte) error) {
	if gen >= 2 {
		return "/rpc/Shelly.GetStatus", c.decodeGen2Status
	}
	return "/status", c.decodeGen1Status
}

// fetch performs the status request, returning the raw body.
func (c *Collector) fetch(
	ctx context.Context, client *http.Client, dev registry.Device, path string,
) ([]byte, int, error) {
	addr, ok := dev.Primary()
	if !ok {
		return nil, 0, errNoAddress
	}
	url := "http://" + net.JoinHostPort(addr.String(), itoa(int(dev.Port))) + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.cfg.MaxBodyBytes))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, resp.StatusCode, &authError{code: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, resp.StatusCode, &statusError{code: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// componentName returns a configured component name, or "".
func (c *Collector) componentName(component, id string) string {
	c.namesMu.RLock()
	defer c.namesMu.RUnlock()
	for _, byKey := range c.names {
		if name, ok := byKey[component+":"+id]; ok {
			return name
		}
	}
	return ""
}

// decodeJSON is a small helper for the Gen1 path, which has a fixed schema.
func decodeJSON(body []byte, out any) error { return json.Unmarshal(body, out) }
