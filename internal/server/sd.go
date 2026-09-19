package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// targetGroup is one entry in the Prometheus http_sd_configs response.
type targetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// sdHandler serves Prometheus HTTP service discovery.
//
// Targets carry the stable device ID, not the address. Prometheus derives instance from
// __address__, so an IP there would rename instance on every DHCP lease change: counters
// restart, rate() breaks at the seam, dashboards lose history and for: clauses reset. On
// a home LAN with a hundred-plus devices and flaky wifi, lease churn is routine rather
// than hypothetical. /probe resolves the ID to a current address on each request, so an
// address change is invisible to Prometheus. The address is still available for Grafana
// links via espressif_device_info{ip=...}.
func sdHandler(p SnapshotProvider, staleAfter time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := p.Snapshot()
		kind := discovery.Kind(r.URL.Query().Get("kind"))
		now := time.Now()

		// Always an empty array, never null: a null body is not a valid target list.
		groups := make([]targetGroup, 0, snap.Len())
		for _, d := range snap.List(kind) {
			groups = append(groups, targetGroup{
				Targets: []string{d.ID},
				Labels:  sdLabels(d, now, staleAfter),
			})
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(groups)
	}
}

func sdLabels(d registry.Device, now time.Time, staleAfter time.Duration) map[string]string {
	labels := map[string]string{
		"__meta_device_id":        d.ID,
		"__meta_device_kind":      string(d.Kind),
		"__meta_device_name":      d.Name,
		"__meta_device_hostname":  d.Hostname,
		"__meta_device_service":   "",
		"__meta_device_last_seen": d.LastSeen.UTC().Format(time.RFC3339),
		// Exposed as a label rather than filtered here, so the operator decides the drop
		// policy in relabel_configs instead of the exporter deciding for them.
		"__meta_device_stale":  strconv.FormatBool(d.Stale(now, staleAfter)),
		"__meta_device_static": strconv.FormatBool(d.Static),
	}
	if addr, ok := d.Primary(); ok {
		labels["__meta_device_address"] = net.JoinHostPort(addr.String(), strconv.Itoa(int(d.Port)))
	}
	if gen := d.TXT["gen"]; gen != "" {
		labels["__meta_device_gen"] = gen
	}
	return labels
}
