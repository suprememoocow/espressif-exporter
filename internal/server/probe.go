package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// ProbeRunner executes a probe for one device under every shared gate.
type ProbeRunner interface {
	Run(ctx context.Context, dev registry.Device) ([]prometheus.Metric, probe.Result, time.Duration)
}

// probeHandler serves GET /probe?target=<device-id>.
func probeHandler(
	devices SnapshotProvider, runner ProbeRunner, cfg config.Probe, observe func(string, probe.Result, time.Duration),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			http.Error(w, "missing target parameter", http.StatusBadRequest)
			return
		}

		dev, ok := devices.Snapshot().Get(target)
		if !ok {
			// A 404 rather than probe_success 0. An unknown target means Prometheus's
			// target list is out of step with discovery — a configuration problem, not a
			// device problem — and surfacing it as up == 0 with a scrape error makes it
			// visible instead of hiding it among genuinely unreachable devices.
			http.Error(w, "unknown target: "+target, http.StatusNotFound)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), scrapeTimeout(r, cfg))
		defer cancel()

		start := time.Now()
		deviceMetrics, res, backoffRemaining := runner.Run(ctx, dev)
		duration := time.Since(start)

		if observe != nil {
			observe(string(dev.Kind), res, duration)
		}

		reg := prometheus.NewRegistry()
		reg.MustRegister(probeCollector{
			metrics:          deviceMetrics,
			result:           res,
			duration:         duration,
			backoffRemaining: backoffRemaining,
		})

		w.Header().Set("Cache-Control", "no-store")
		promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		}).ServeHTTP(w, r)
	}
}

// scrapeTimeout derives the device budget from Prometheus's declared scrape timeout.
//
// The offset is what guarantees the exporter finishes serialising the response before
// Prometheus gives up. Without it, a device that takes the full budget produces a scrape
// timeout and we lose the probe_success 0 sample — the one data point most worth having
// when a device is slow.
func scrapeTimeout(r *http.Request, cfg config.Probe) time.Duration {
	timeout := cfg.DefaultTimeout
	if v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			timeout = time.Duration(f * float64(time.Second))
		}
	}
	timeout -= cfg.TimeoutOffset
	return min(max(timeout, cfg.MinTimeout), cfg.MaxTimeout)
}

// probeCollector renders one probe's output.
//
// Describe is empty — an unchecked collector — because descriptors are built from
// whatever entities the device happens to expose, and vary between probes.
type probeCollector struct {
	metrics          []prometheus.Metric
	result           probe.Result
	duration         time.Duration
	backoffRemaining time.Duration
}

func (c probeCollector) Describe(chan<- *prometheus.Desc) {}

func (c probeCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- gauge(probeSuccessDesc, boolValue(c.result.Success))
	ch <- gauge(probeDurationDesc, c.duration.Seconds())
	ch <- gauge(probeStatusDesc, float64(c.result.HTTPStatus))

	// Every reason, every scrape, at 0 or 1. Emitting only the active one would leave
	// the previous reason to go stale rather than being cleared, so a query would report
	// two reasons at once for five minutes after every transition.
	for _, reason := range probe.AllReasons {
		ch <- gaugeWithLabel(probeReasonDesc, boolValue(c.result.Reason == reason), string(reason))
	}
	if c.backoffRemaining > 0 {
		ch <- gauge(probeBackoffDesc, c.backoffRemaining.Seconds())
	}

	for _, m := range c.metrics {
		ch <- m
	}
}

// These names are unprefixed, matching blackbox_exporter, so existing dashboards and
// mental models transfer directly.
var (
	probeSuccessDesc = prometheus.NewDesc(
		"probe_success", "Whether the probe of the device succeeded.", nil, nil)
	probeDurationDesc = prometheus.NewDesc(
		"probe_duration_seconds", "How long the probe took.", nil, nil)
	probeStatusDesc = prometheus.NewDesc(
		"probe_http_status_code", "HTTP status from the device, or 0 if no response was received.", nil, nil)
	probeReasonDesc = prometheus.NewDesc(
		"probe_failure_reason", "Why the probe failed. Exactly one reason is 1 when probe_success is 0.",
		[]string{"reason"}, nil)
	probeBackoffDesc = prometheus.NewDesc(
		"probe_backoff_remaining_seconds", "Time until the device will be contacted again.", nil, nil)
)

func gauge(d *prometheus.Desc, v float64) prometheus.Metric {
	return prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)
}

func gaugeWithLabel(d *prometheus.Desc, v float64, label string) prometheus.Metric {
	return prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, label)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
