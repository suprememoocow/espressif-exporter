// Package server exposes the exporter's HTTP surface: service discovery, multi-target
// probes, the exporter's own metrics, and health endpoints.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/probe"
)

// ReadyFunc reports whether the exporter has enough state to serve useful answers.
// It returns a reason when not ready, which /readyz surfaces to the operator.
type ReadyFunc func() (ready bool, reason string)

// Options wires the server's collaborators without importing them at package level.
type Options struct {
	Config  config.Server
	Logger  *slog.Logger
	Ready   ReadyFunc
	Handler http.Handler // application routes; health and pprof are added here

	// Devices backs /sd, /probe and /debug/devices. Optional; those routes are omitted
	// when nil.
	Devices SnapshotProvider

	// Runner executes probes. Required for /probe.
	Runner ProbeRunner

	// Probe configures probe deadlines.
	Probe config.Probe

	// StaleAfter marks a device stale in /sd without dropping it.
	StaleAfter time.Duration

	// ExporterMetrics is the exporter's own registry, served on /metrics.
	ExporterMetrics prometheus.Gatherer

	// ObserveProbe records a probe outcome in the exporter's self-metrics.
	ObserveProbe func(kind string, res probe.Result, d time.Duration)
}

// Server owns the HTTP listener and its graceful shutdown.
type Server struct {
	http   *http.Server
	log    *slog.Logger
	closed time.Duration
}

// New builds the server. It does not listen; call Run.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Ready == nil {
		opts.Ready = func() (bool, string) { return true, "" }
	}

	mux := http.NewServeMux()

	// Liveness must never depend on Avahi, D-Bus or any device. A liveness check that
	// fails when a dependency is down makes Docker restart a healthy process, which is
	// strictly worse than the outage it is reacting to.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writePlain(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /readyz", readyHandler(opts.Ready))
	mux.HandleFunc("POST /-/loglevel", logLevelHandler(opts.Logger))
	mux.HandleFunc("GET /", landingHandler)

	if opts.Devices != nil {
		mux.HandleFunc("GET /debug/devices", debugDevicesHandler(opts.Devices))
		mux.HandleFunc("GET /sd", sdHandler(opts.Devices, opts.StaleAfter))
		if opts.Runner != nil {
			mux.HandleFunc("GET /probe", probeHandler(
				opts.Devices, opts.Runner, opts.Probe, opts.ObserveProbe))
		}
	}
	if opts.ExporterMetrics != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(opts.ExporterMetrics, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}

	if opts.Handler != nil {
		mux.Handle("/", opts.Handler)
	}
	if opts.Config.Pprof {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}

	var handler http.Handler = mux
	handler = recoverMiddleware(opts.Logger, handler)
	if opts.Config.MaxRequests > 0 {
		handler = limitInFlight(opts.Config.MaxRequests, handler)
	}

	return &Server{
		log:    opts.Logger,
		closed: opts.Config.ShutdownTimeout,
		http: &http.Server{
			Addr:              opts.Config.Listen,
			Handler:           handler,
			ReadHeaderTimeout: opts.Config.ReadHeaderTimeout,
		},
	}
}

// Run serves until ctx is cancelled, then shuts down gracefully.
//
// The listener is bound before any discovery backend starts, so that
// espressif_exporter_avahi_up 0 is scrapeable rather than swallowed by a crash loop.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", s.http.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Deliberately not derived from ctx: ctx is already cancelled by the time we get
	// here, and a shutdown context inheriting that cancellation would abort in-flight
	// scrapes instantly rather than draining them.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.closed)
	defer cancel()

	s.log.Info("http server shutting down", "timeout", s.closed)
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return <-errCh
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// body is always an internal constant or a validated level name, and the response
	// is text/plain, so this is not an injection vector.
	_, _ = w.Write([]byte(body)) //nolint:gosec // G705: not HTML, not user-controlled
}
