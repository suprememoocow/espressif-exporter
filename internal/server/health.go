package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/suprememoocow/espressif-exporter/internal/logging"
	"github.com/suprememoocow/espressif-exporter/internal/version"
)

func readyHandler(ready ReadyFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ok, reason := ready()
		if ok {
			writePlain(w, http.StatusOK, "ready\n")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready", "reason": reason})
	}
}

// logLevelHandler raises or lowers verbosity without a restart, which matters because
// restarting drops every cached ESPHome connection and the whole device registry.
func logLevelHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requested := r.URL.Query().Get("level")
		if requested == "" {
			requested = r.FormValue("level")
		}
		lvl, err := logging.ParseLevel(requested)
		if err != nil {
			writePlain(w, http.StatusBadRequest, err.Error()+"\n")
			return
		}
		logging.Level.Set(lvl)
		log.Info("log level changed", "level", lvl.String())
		writePlain(w, http.StatusOK, "level="+lvl.String()+"\n")
	}
}

func landingHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<title>espressif-exporter</title>
<h1>espressif-exporter</h1>
<p>` + version.String() + `</p>
<ul>
  <li><a href="/metrics">/metrics</a> &mdash; the exporter's own metrics</li>
  <li><a href="/sd">/sd</a> &mdash; Prometheus HTTP service discovery</li>
  <li><code>/probe?target=&lt;device-id&gt;</code> &mdash; per-device metrics</li>
  <li><a href="/debug/devices">/debug/devices</a> &mdash; discovery snapshot</li>
  <li><a href="/healthz">/healthz</a> &middot; <a href="/readyz">/readyz</a></li>
</ul>
`))
}
