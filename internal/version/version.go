// Package version holds build metadata stamped in at link time.
package version

import (
	"fmt"
	"runtime"
)

// These are set via -ldflags at build time.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// Name is the canonical exporter name, used as the Prometheus metric namespace.
const Name = "espressif_exporter"

// String renders a single human-readable version line.
func String() string {
	return fmt.Sprintf("%s version=%s commit=%s built=%s go=%s %s/%s",
		Name, Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
