// Package logging builds the process logger and exposes its level for runtime changes.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Level is the process-wide log level. It is a LevelVar so that POST /-/loglevel can
// raise verbosity at 2am without restarting the exporter and losing all cached state.
var Level = new(slog.LevelVar)

// New returns a logger writing to stderr in the requested format.
func New(level, format string) (*slog.Logger, error) {
	l, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	Level.Set(l)

	opts := &slog.HandlerOptions{Level: Level}

	var h slog.Handler
	switch strings.ToLower(format) {
	case "json", "":
		h = slog.NewJSONHandler(os.Stderr, opts)
	case "text", "logfmt":
		h = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q (want json or text)", format)
	}
	return slog.New(h), nil
}

// ParseLevel maps a name to a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
	}
}
