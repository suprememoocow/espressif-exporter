// Package app wires the exporter's components together and runs them.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"golang.org/x/sync/errgroup"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
	"github.com/suprememoocow/espressif-exporter/internal/discovery/avahi"
	"github.com/suprememoocow/espressif-exporter/internal/discovery/multi"
	"github.com/suprememoocow/espressif-exporter/internal/discovery/static"
	"github.com/suprememoocow/espressif-exporter/internal/discovery/zeroconf"
	"github.com/suprememoocow/espressif-exporter/internal/esphome"
	"github.com/suprememoocow/espressif-exporter/internal/metrics"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
	"github.com/suprememoocow/espressif-exporter/internal/server"
	"github.com/suprememoocow/espressif-exporter/internal/shelly"
	"github.com/suprememoocow/espressif-exporter/internal/version"
)

// App owns the exporter's long-lived components.
type App struct {
	cfg config.Config
	log *slog.Logger

	registry  *registry.Registry
	discovery *multi.Multi
	server    *server.Server

	self       *metrics.Self
	limiter    *probe.Limiter
	shelly     *shelly.Collector
	esphomeMgr *esphome.Manager
	esphome    *esphome.Collector
	avahi      *avahi.Discoverer

	// Baselines for counters that are sampled rather than incremented in place.
	lastUnknown map[string]uint64
	lastSkipped map[string]uint64
	lastShared  int64

	// hasStatic records whether an operator-supplied seed list exists, which decides
	// whether readiness can be satisfied before any mDNS response arrives.
	hasStatic bool
}

// New builds the application graph without starting anything.
func New(cfg config.Config, log *slog.Logger) (*App, error) {
	policy, err := registry.NewAddressPolicy(
		cfg.Discovery.IPv6, cfg.Discovery.AllowCIDRs, cfg.Discovery.DenyCIDRs)
	if err != nil {
		return nil, fmt.Errorf("address policy: %w", err)
	}

	families := metrics.NewRegistry()
	self := metrics.NewSelf(version.Version, version.Commit, runtime.Version())

	// The manager reports its live connections back to the registry, so the registry has
	// to exist first.
	reg := registry.New(cfg.Registry, policy, log)

	a := &App{
		cfg:         cfg,
		log:         log,
		registry:    reg,
		self:        self,
		limiter:     probe.NewLimiter(cfg.Probe),
		shelly:      shelly.New(cfg.Shelly, families, log),
		esphomeMgr:  esphome.NewManager(cfg.ESPHome, reg, log),
		lastUnknown: map[string]uint64{},
		lastSkipped: map[string]uint64{},
	}

	backends, err := a.buildBackends()
	if err != nil {
		return nil, err
	}
	a.discovery = multi.New(log, backends...)

	a.esphome = esphome.NewCollector(a.esphomeMgr, cfg.ESPHome, cfg.Metrics, families, log)

	a.server = server.New(server.Options{
		Config:  cfg.Server,
		Logger:  log,
		Ready:   a.ready,
		Devices: a.registry,
		Runner: &runner{
			limiter: a.limiter,
			probers: map[discovery.Kind]probe.Prober{
				discovery.KindShelly:  a.shelly,
				discovery.KindESPHome: a.esphome,
			},
			registry: a.registry,
		},
		Probe:           cfg.Probe,
		StaleAfter:      cfg.Discovery.StaleAfter,
		ExporterMetrics: self.Registry,
		ObserveProbe: func(kind string, res probe.Result, d time.Duration) {
			self.ObserveProbe(kind, string(res.Reason), res.Success, d)
		},
	})
	return a, nil
}

func (a *App) buildBackends() ([]discovery.Discoverer, error) {
	var backends []discovery.Discoverer

	for _, source := range a.cfg.Discovery.Sources {
		switch source {
		case static.Name:
			if len(a.cfg.Discovery.Static) == 0 {
				a.log.Warn("static discovery is enabled but no entries are configured")
				continue
			}
			d, err := static.New(a.cfg.Discovery.Static, a.cfg.Registry.RefreshInterval)
			if err != nil {
				return nil, fmt.Errorf("static discovery: %w", err)
			}
			backends = append(backends, d)
			a.hasStatic = true

		case zeroconf.Name:
			backends = append(backends, zeroconf.New(a.cfg.Discovery.ServiceTypes, a.log))

		case avahi.Name:
			a.avahi = avahi.New(a.cfg.Discovery.Avahi, a.cfg.Discovery.ServiceTypes, a.log)
			backends = append(backends, a.avahi)

		default:
			return nil, fmt.Errorf("unknown discovery source %q", source)
		}
	}

	if len(backends) == 0 {
		a.log.Warn("no discovery backend is active; the exporter will find no devices")
	}
	// Avahi only gets to be fatal when nothing else could possibly find a device.
	// Otherwise the exporter keeps retrying forever and serves what the other sources
	// found, because a degraded exporter beats a crash loop.
	if a.avahi != nil && len(backends) > 1 {
		a.cfg.Discovery.Avahi.Required = false
	}
	return backends, nil
}

// ready reports readiness. Liveness deliberately does not depend on discovery; see
// server.New. Readiness does, because there is nothing useful to scrape until the
// registry has been populated at least once.
func (a *App) ready() (bool, string) {
	if a.registry.Snapshot().Len() > 0 {
		return true, ""
	}
	if a.hasStatic {
		return true, ""
	}
	for _, b := range a.discovery.Backends() {
		if b.Health().Up {
			return true, ""
		}
	}
	return false, "no discovery backend is up and no devices are known"
}

// Run starts every component and blocks until ctx is cancelled.
//
// Shutdown order matters: the HTTP server stops accepting first, so Prometheus sees a
// clean connection refusal rather than a half-served scrape, and only then do the
// discovery backends tear down their connections.
func (a *App) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.registry.Run(ctx) })
	g.Go(func() error { return a.discovery.Run(ctx, a.registry.Events()) })
	g.Go(func() error { return a.server.Run(ctx) })
	g.Go(func() error { return a.esphomeMgr.Run(ctx) })
	g.Go(func() error { a.refreshSelfMetrics(ctx); return nil })
	return g.Wait()
}

// addDelta advances a counter vector by the change since the previous sample, and
// updates the baseline in place.
func (a *App) addDelta(vec *prometheus.CounterVec, baseline map[string]uint64, current map[string]uint64) {
	for key, total := range current {
		if prev, ok := baseline[key]; !ok || total > prev {
			vec.WithLabelValues(key).Add(float64(total - prev))
			baseline[key] = total
		}
	}
}

// refreshSelfMetrics samples state that is cheaper to poll than to instrument at every
// mutation point.
func (a *App) refreshSelfMetrics(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		snap := a.registry.Snapshot()
		now := time.Now()

		// The ESPHome manager holds a long-lived connection per device, so it has to be
		// told which devices exist; the Shelly collector is driven by probes instead.
		a.esphomeMgr.Sync(snap.List(discovery.KindESPHome))
		counts := map[[2]string]int{}
		for _, d := range snap.Devices {
			state := "active"
			switch {
			case d.Stale(now, a.cfg.Discovery.StaleAfter):
				state = "stale"
			case d.Unverified:
				state = "unverified"
			}
			counts[[2]string{string(d.Kind), state}]++
		}
		a.self.RegistryDevices.Reset()
		for k, n := range counts {
			a.self.RegistryDevices.WithLabelValues(k[0], k[1]).Set(float64(n))
		}

		for _, b := range a.discovery.Backends() {
			h := b.Health()
			up := 0.0
			if h.Up {
				up = 1
			}
			a.self.DiscoverySourceUp.WithLabelValues(b.Name()).Set(up)
			if !h.LastEventAt.IsZero() {
				a.self.DiscoveryLastSeen.WithLabelValues(b.Name()).
					Set(float64(h.LastEventAt.Unix()))
			}
		}

		if a.avahi != nil {
			up := 0.0
			if a.avahi.Health().Up {
				up = 1
			}
			a.self.AvahiUp.Set(up)
			a.self.AvahiResolvers.Set(float64(a.avahi.ActiveResolvers()))
		}

		a.self.ProbesInFlight.Set(float64(a.limiter.InFlight()))
		a.self.DevicesBackoff.Set(float64(a.limiter.Backoff().Open()))
		a.self.ShellyIdentityCache.Set(float64(a.shelly.IdentityCacheSize()))

		// These are counters, so they cannot be set to an observed total; add the
		// delta since the previous sample instead.
		a.addDelta(a.self.ShellyUnknownComponents, a.lastUnknown, a.shelly.UnknownComponents())
		a.addDelta(a.self.ProbesSkipped, a.lastSkipped, a.limiter.SkippedByReason())
		if shared := a.limiter.Shared(); shared > a.lastShared {
			a.self.ProbesShared.Add(float64(shared - a.lastShared))
			a.lastShared = shared
		}
	}
}
