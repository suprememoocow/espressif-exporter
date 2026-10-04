// Package shellycloud implements a Discoverer over a Shelly Cloud account.
//
// The cloud knows every device on the account and the LAN address each one last reported,
// so it finds devices that mDNS cannot: those on a VLAN that does not forward multicast,
// and those with mDNS disabled. It is a poll, not a browse, so it notices a DHCP change
// only on its next interval; run it alongside avahi rather than instead of it.
package shellycloud

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
	cloudapi "github.com/suprememoocow/espressif-exporter/internal/shellycloud"
)

// Name identifies this backend in events and metrics.
const Name = "shelly_cloud"

// Initial and maximum retry delay after a failed poll. The cap is the poll interval
// itself: retrying more slowly than we would poll anyway would make an outage last longer.
const (
	retryInitial = 30 * time.Second
	retryFactor  = 2
)

// Lister is the subset of the cloud client this backend uses.
type Lister interface {
	AllStatus(ctx context.Context) ([]cloudapi.DeviceStatus, error)
}

// Discoverer polls the account's device list.
type Discoverer struct {
	client         Lister
	interval       time.Duration
	includeOffline bool
	log            *slog.Logger

	mu        sync.Mutex
	health    discovery.Health
	attempted bool
}

// New builds the backend.
func New(client Lister, interval time.Duration, includeOffline bool, log *slog.Logger) *Discoverer {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Discoverer{
		client:         client,
		interval:       interval,
		includeOffline: includeOffline,
		log:            log.With("component", "discovery", "source", Name),
		health:         discovery.Health{Reason: "not yet polled"},
	}
}

// Name implements discovery.Discoverer.
func (d *Discoverer) Name() string { return Name }

// Health implements discovery.Discoverer.
func (d *Discoverer) Health() discovery.Health {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.health
}

// Run polls immediately, then every interval. A failed poll is retried with backoff and
// never returns: the cloud being down is a reason to serve what other sources found, not
// to stop the exporter.
func (d *Discoverer) Run(ctx context.Context, out chan<- discovery.Event) error {
	retry := retryInitial
	for {
		wait := d.interval
		if err := d.poll(ctx, out); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			wait = min(retry, d.interval)
			retry = min(retry*retryFactor, d.interval)
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

func (d *Discoverer) poll(ctx context.Context, out chan<- discovery.Event) error {
	devices, err := d.client.AllStatus(ctx)

	d.mu.Lock()
	first := !d.attempted
	d.attempted = true
	if err != nil {
		wasUp := d.health.Up
		d.health.Up, d.health.Reason = false, err.Error()
		d.mu.Unlock()
		// Log the transition, not every retry: a revoked key would otherwise warn forever.
		if (wasUp || first) && ctx.Err() == nil {
			d.log.Warn("polling Shelly Cloud failed", "error", err)
		}
		return err
	}
	d.mu.Unlock()

	now := time.Now()
	var published, offline, addressless int
	for _, dev := range devices {
		switch {
		case !dev.Addr.IsValid():
			addressless++
			continue
		case !dev.Online && !d.includeOffline:
			offline++
			continue
		}
		select {
		case out <- event(dev, now):
			published++
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	d.mu.Lock()
	if !d.health.Up && !first {
		d.health.Reconnects++
	}
	d.health.Up, d.health.Reason = true, ""
	d.health.LastEventAt = now
	d.mu.Unlock()

	d.log.Debug("polled Shelly Cloud",
		"devices", len(devices), "published", published,
		"skipped_offline", offline, "skipped_no_address", addressless)
	return nil
}

// event renders one cloud device as an observation.
//
// It deliberately sets no DeviceID. The mac TXT key makes the registry derive the same
// mac: identity an mDNS observation of this device derives, so the two merge into one
// device instead of being scraped twice.
//
// The endpoint is not Trusted: a cloud-reported address is a report, not operator intent,
// so the allow/deny CIDR and routability checks apply to it as they do to mDNS.
func event(dev cloudapi.DeviceStatus, now time.Time) discovery.Event {
	proto := discovery.ProtoIPv4
	if dev.Addr.Is6() {
		proto = discovery.ProtoIPv6
	}
	// The status's own MAC is preferred: an early Gen1 device's cloud id is only the last
	// three MAC bytes, which would derive a weak name: identity instead of mac:.
	mac := dev.MAC
	if mac == "" {
		mac = dev.ID
	}
	txt := map[string]string{"mac": mac}
	if dev.Gen > 0 {
		txt["gen"] = strconv.Itoa(dev.Gen)
	}
	if dev.Code != "" {
		txt["model"] = dev.Code
	}
	return discovery.Event{
		Type:     discovery.EventAdd,
		Source:   Name,
		Instance: dev.ID,
		Domain:   "local",
		Kind:     discovery.KindShelly,
		At:       now,
		Endpoint: discovery.Endpoint{
			Key: discovery.EndpointKey{
				ServiceType: Name,
				Protocol:    proto,
			},
			Addr:       dev.Addr,
			Port:       80,
			TXT:        txt,
			ObservedAt: now,
		},
	}
}
