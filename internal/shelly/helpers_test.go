package shelly

import (
	"io"
	"log/slog"
	"strconv"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
