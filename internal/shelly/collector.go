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
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
	"github.com/suprememoocow/espressif-exporter/internal/shellycloud"
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

	names *namesCache
	cloud CloudNames
}

// CloudNames supplies the names an operator gave a device in the Shelly app, by MAC.
// Implemented by shellycloud.NameStore.
type CloudNames interface {
	Lookup(mac string) (shellycloud.Names, bool)
	// Loaded is closed once the first refresh has finished.
	Loaded() <-chan struct{}
}

// cloudNamesWait bounds how long a probe waits for the first cloud refresh after startup.
// Without the wait, the first scrapes after every restart would carry the device's own
// names and then switch to the cloud's, creating a short-lived duplicate of every series.
// The app also holds discovery back until that refresh, so this is a backstop for a
// device probed directly by ID.
const cloudNamesWait = 3 * time.Second

// cloudChannelComponents maps a Shelly Cloud channel category to the component types its
// name applies to. Channel n names component n of a matching type and nothing else.
//
// The category is what makes this safe on devices with mixed components: a Shelly EM or
// Pro EM-50 has a relay and two meters, and its cloud channels are the meters (category
// emeter), so they name em1:0 and em1:1 while switch:0 keeps the device's own name.
// Inputs and sensors have no cloud channel and always keep the device's own names, as
// does three-phase em, whose phases share one component and so one name. A category not
// listed here names nothing.
var cloudChannelComponents = map[string]map[string]bool{
	"relay":  {"switch": true},
	"roller": {"cover": true},
	"cover":  {"cover": true},
	"light":  {"light": true, "rgb": true, "rgbw": true, "cct": true},
	"emeter": {"em1": true, "pm1": true},
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
		names:             newNamesCache(cfg.IdentityTTL),
	}
}

// SetCloudNames makes names from the Shelly app take precedence over the names devices
// report about themselves. Call it before the first probe.
func (c *Collector) SetCloudNames(n CloudNames) { c.cloud = n }

// cloudNames returns a device's names from the Shelly app, if a source is configured and
// knows the device.
func (c *Collector) cloudNames(mac string) (shellycloud.Names, bool) {
	if c.cloud == nil || mac == "" {
		return shellycloud.Names{}, false
	}
	return c.cloud.Lookup(mac)
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
	c.names.forget(deviceID)
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

	// Refresh the configured names on the same cold path as identity; best-effort, so a
	// failure never fails the probe (see ensureNames).
	if c.cfg.FetchConfig {
		names := c.ensureNames(ctx, client, dev, id.Gen)
		// Gen1's /shelly carries no name, so the name an operator set in the app lives only
		// on /settings. Apply it to this probe's copy of the identity rather than to the
		// cached one: the two caches expire moments apart, so a cached override would be
		// lost on the next identity refresh and not restored until the names entry expired
		// too — six more hours of the wrong name. An empty name leaves the mDNS fallback
		// below to apply.
		if names.device != "" {
			id.Name = names.device
		}
	}

	base := metrics.Labels{Device: dev.ID, Kind: "shelly"}

	// Names from the Shelly app win over the device's own: the app is where people
	// actually name things, and the on-device copies are often empty or truncated.
	// The lookup is an in-memory read, so this adds no request to the scrape.
	if c.cloud != nil {
		t := time.NewTimer(cloudNamesWait)
		select {
		case <-c.cloud.Loaded():
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
	if cloud, ok := c.cloudNames(id.MAC); ok {
		if cloud.Device != "" {
			id.Name = cloud.Device
		}
		base.Area = cloud.Room
	}
	// Fall back to the registry's name when neither the app nor the device has one. It is
	// read per probe rather than cached with the identity, because the registry replaces
	// a provisional cloud-id name once mDNS sees the device.
	if id.Name == "" {
		id.Name = dev.Name
	}
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

// componentName returns a device's configured name for a component, or "". A name from
// the Shelly app takes precedence over the device's own; see cloudChannelComponents.
//
// The energy-only variants emdata/em1data re-emit under em/em1 (see extractEMData), so their
// names live under the em/em1 config key: normalising here keeps the energy series' name in
// step with the power series', which is what preserves the (component,id,phase) join.
func (c *Collector) componentName(deviceID, component, id string) string {
	switch component {
	case "emdata":
		component = "em"
	case "em1data":
		component = "em1"
	}
	if name := c.cloudChannelName(deviceID, component, id); name != "" {
		return name
	}
	return c.names.lookup(deviceID, component+":"+id)
}

// cloudChannelName returns the Shelly app's name for a component, or "" when the app has
// none for it; see cloudChannelComponents.
func (c *Collector) cloudChannelName(deviceID, component, id string) string {
	if c.cloud == nil {
		return ""
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return ""
	}
	cloud, ok := c.cloudNames(c.identities.mac(deviceID))
	if !ok {
		return ""
	}
	ch, ok := cloud.Channel(n)
	if !ok || !cloudChannelComponents[ch.Category][component] {
		return ""
	}
	return ch.Name
}

// decodeJSON is a small helper for the Gen1 path, which has a fixed schema.
func decodeJSON(body []byte, out any) error { return json.Unmarshal(body, out) }
