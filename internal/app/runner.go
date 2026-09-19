package app

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// runner dispatches a probe to the collector for the device's kind, under the shared
// concurrency, de-duplication and backoff gates.
type runner struct {
	limiter  *probe.Limiter
	probers  map[discovery.Kind]probe.Prober
	registry *registry.Registry
}

// Run implements server.ProbeRunner.
func (r *runner) Run(
	ctx context.Context, dev registry.Device,
) ([]prometheus.Metric, probe.Result, time.Duration) {
	prober, ok := r.probers[dev.Kind]
	if !ok {
		return nil, probe.Fail(probe.ReasonNotConnected, 0), 0
	}

	metrics, res, backoffRemaining := r.limiter.Do(ctx, dev,
		func(ctx context.Context) ([]prometheus.Metric, probe.Result) {
			return prober.Probe(ctx, dev)
		})

	// Feed the outcome back so a device that scrapes cleanly is never reaped for mDNS
	// silence, and so the pinned address follows what actually works.
	if addr, ok := dev.Primary(); ok {
		r.registry.ReportScrape(registry.ScrapeResult{
			DeviceID: dev.ID,
			Addr:     addr,
			Success:  res.Success,
		})
	}
	return metrics, res, backoffRemaining
}
