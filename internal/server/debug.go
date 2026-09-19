package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// SnapshotProvider supplies the current device view. The registry satisfies it; the
// interface keeps this package from depending on the registry's construction.
type SnapshotProvider interface {
	Snapshot() *registry.Snapshot
}

// debugDevice is the JSON shape of /debug/devices.
type debugDevice struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Hostname   string            `json:"hostname,omitempty"`
	Addrs      []string          `json:"addrs"`
	Rejected   []string          `json:"rejected_addrs,omitempty"`
	Port       uint16            `json:"port"`
	TXT        map[string]string `json:"txt,omitempty"`
	FirstSeen  time.Time         `json:"first_seen"`
	LastSeen   time.Time         `json:"last_seen"`
	AgeSeconds float64           `json:"age_seconds"`
	Static     bool              `json:"static"`
	Unverified bool              `json:"unverified"`
	Epoch      uint64            `json:"epoch"`
}

// debugDevicesHandler dumps the whole snapshot.
//
// With a hundred-plus devices this is the fastest way to answer "did it find my device,
// and at what address", which is the question that comes up during every deployment
// problem. It is worth having from day one.
func debugDevicesHandler(p SnapshotProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := p.Snapshot()
		now := time.Now()

		out := make([]debugDevice, 0, snap.Len())
		for _, d := range snap.Devices {
			if kind := r.URL.Query().Get("kind"); kind != "" && string(d.Kind) != kind {
				continue
			}
			addrs := make([]string, 0, len(d.Addrs))
			for _, a := range d.Addrs {
				addrs = append(addrs, a.String())
			}
			var rejected []string
			for _, a := range d.RejectedAddrs {
				rejected = append(rejected, a.String())
			}
			out = append(out, debugDevice{
				ID:         d.ID,
				Kind:       string(d.Kind),
				Name:       d.Name,
				Hostname:   d.Hostname,
				Addrs:      addrs,
				Rejected:   rejected,
				Port:       d.Port,
				TXT:        d.TXT,
				FirstSeen:  d.FirstSeen,
				LastSeen:   d.LastSeen,
				AgeSeconds: now.Sub(d.LastSeen).Seconds(),
				Static:     d.Static,
				Unverified: d.Unverified,
				Epoch:      d.Epoch,
			})
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	}
}
