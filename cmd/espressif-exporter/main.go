// Command espressif-exporter exports Prometheus metrics from ESPHome and Shelly devices
// discovered over mDNS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/app"
	"github.com/suprememoocow/espressif-exporter/internal/config"
	"github.com/suprememoocow/espressif-exporter/internal/logging"
	"github.com/suprememoocow/espressif-exporter/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "espressif-exporter: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// The distroless image has no shell, so the container HEALTHCHECK invokes this
	// subcommand on our own binary in exec form.
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck(args[1:])
	}

	fs := flag.NewFlagSet("espressif-exporter", flag.ContinueOnError)
	var (
		configPath  = fs.String("config", "", "path to the YAML configuration file")
		listen      = fs.String("web.listen-address", "", "override server.listen")
		logLevel    = fs.String("log.level", "", "override log.level")
		logFormat   = fs.String("log.format", "", "override log.format")
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version.String())
		return nil
	}

	if err := config.MustExist(*configPath); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// Flags are the last layer, above defaults, file and environment.
	if *listen != "" {
		cfg.Server.Listen = *listen
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}
	if *logFormat != "" {
		cfg.Log.Format = *logFormat
	}

	log, err := logging.New(cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version.Version, "commit", version.Commit)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(cfg, log)
	if err != nil {
		return err
	}
	if err := application.Run(ctx); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

// healthcheck probes the local /healthz endpoint. It deliberately checks liveness only.
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	addr := fs.String("addr", "", "address to probe (defaults to $EE_SERVER__LISTEN or :9826)")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := *addr
	if target == "" {
		target = os.Getenv(config.EnvPrefix + "SERVER__LISTEN")
	}
	if target == "" {
		target = config.Default().Server.Listen
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("parsing address %q: %w", target, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// The target is the exporter's own listen address, resolved to a loopback host
	// above. It is operator-supplied by definition, and this subcommand exists purely to
	// give the distroless image a HEALTHCHECK without a shell.
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: loopback self-check
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: loopback self-check
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return errors.New("unhealthy: " + resp.Status)
	}
	return nil
}
