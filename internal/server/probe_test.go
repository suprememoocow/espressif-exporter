package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/discovery"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

type fakeSnapshots struct{ snap *registry.Snapshot }

func (f fakeSnapshots) Snapshot() *registry.Snapshot { return f.snap }

type fakeRunner struct {
	result      probe.Result
	metrics     []prometheus.Metric
	gotDeadline time.Duration
}

func (f *fakeRunner) Run(
	ctx context.Context, _ registry.Device,
) ([]prometheus.Metric, probe.Result, time.Duration) {
	if dl, ok := ctx.Deadline(); ok {
		f.gotDeadline = time.Until(dl).Round(100 * time.Millisecond)
	}
	return f.metrics, f.result, 0
}

func snapshotWith(devices ...registry.Device) fakeSnapshots {
	r := registry.New(config.Default().Registry, registry.AddressPolicy{}, discardLogger())
	_ = r
	return fakeSnapshots{snap: registry.NewSnapshotForTest(time.Now(), devices)}
}

func TestProbeRequiresATarget(t *testing.T) {
	h := probeHandler(snapshotWith(), &fakeRunner{}, config.Default().Probe, nil)

	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// An unknown target is a discovery or configuration problem, not a device problem.
// Surfacing it as a scrape error keeps it visible instead of hiding it among genuinely
// unreachable devices.
func TestProbeUnknownTargetIs404(t *testing.T) {
	h := probeHandler(snapshotWith(), &fakeRunner{}, config.Default().Probe, nil)

	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/probe?target=mac:deadbeefcafe", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestProbeEmitsTheFullFailureReasonEnum(t *testing.T) {
	dev := registry.Device{ID: "mac:a8032ab1c2d3", Kind: discovery.KindShelly}
	runner := &fakeRunner{result: probe.Fail(probe.ReasonTimeout, 0)}
	h := probeHandler(snapshotWith(dev), runner, config.Default().Probe, nil)

	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/probe?target="+dev.ID, nil))
	body := w.Body.String()

	if !strings.Contains(body, "probe_success 0") {
		t.Error("probe_success 0 should be present")
	}
	if !strings.Contains(body, `probe_failure_reason{reason="timeout"} 1`) {
		t.Error("the active reason should be 1")
	}
	// Every other reason must be explicitly 0, so a transition clears the old value
	// rather than leaving it to go stale five minutes later.
	if !strings.Contains(body, `probe_failure_reason{reason="auth"} 0`) {
		t.Error("inactive reasons should be explicitly 0")
	}
	for _, r := range probe.AllReasons {
		if !strings.Contains(body, `reason="`+string(r)+`"`) {
			t.Errorf("reason %q missing from the enum", r)
		}
	}
}

// Prometheus declares its scrape timeout on every request. The offset is what guarantees
// the exporter finishes writing the response before Prometheus gives up, so a slow
// device still yields a probe_success 0 sample instead of a scrape timeout.
func TestProbeHonoursTheScrapeTimeoutHeader(t *testing.T) {
	dev := registry.Device{ID: "mac:a8032ab1c2d3", Kind: discovery.KindShelly}
	cfg := config.Default().Probe

	tests := []struct {
		header string
		want   time.Duration
	}{
		{"20", 19500 * time.Millisecond}, // 20s minus the 500ms offset
		{"", cfg.DefaultTimeout - cfg.TimeoutOffset},
		{"0.2", cfg.MinTimeout}, // clamped up
		{"600", cfg.MaxTimeout}, // clamped down
		{"garbage", cfg.DefaultTimeout - cfg.TimeoutOffset},
	}
	for _, tt := range tests {
		runner := &fakeRunner{result: probe.Success(200)}
		h := probeHandler(snapshotWith(dev), runner, cfg, nil)

		r := httptest.NewRequest(http.MethodGet, "/probe?target="+dev.ID, nil)
		if tt.header != "" {
			r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", tt.header)
		}
		h(httptest.NewRecorder(), r)

		if runner.gotDeadline != tt.want.Round(100*time.Millisecond) {
			t.Errorf("header %q gave a %s deadline, want %s", tt.header, runner.gotDeadline, tt.want)
		}
	}
}

func TestProbeObservesOutcomes(t *testing.T) {
	dev := registry.Device{ID: "mac:a8032ab1c2d3", Kind: discovery.KindShelly}
	var gotKind string
	var gotResult probe.Result

	h := probeHandler(snapshotWith(dev), &fakeRunner{result: probe.Success(200)},
		config.Default().Probe,
		func(kind string, res probe.Result, _ time.Duration) { gotKind, gotResult = kind, res })

	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/probe?target="+dev.ID, nil))

	if gotKind != "shelly" || !gotResult.Success {
		t.Errorf("observed kind=%q success=%v, want shelly/true", gotKind, gotResult.Success)
	}
}
