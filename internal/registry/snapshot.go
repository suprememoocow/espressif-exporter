package registry

import (
	"slices"
	"strings"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// Snapshot is an immutable view of the registry at one instant.
//
// Readers obtain one with Registry.Snapshot and may hold it indefinitely without
// locking. Publishing whole snapshots rather than guarding a shared map removes all
// contention between discovery and the HTTP handlers, and makes a torn read impossible.
type Snapshot struct {
	At      time.Time
	Devices []Device // ordered by ID, so output is deterministic

	byID map[string]int
}

func newSnapshot(at time.Time, devices []Device) *Snapshot {
	slices.SortFunc(devices, func(a, b Device) int { return strings.Compare(a.ID, b.ID) })
	byID := make(map[string]int, len(devices))
	for i, d := range devices {
		byID[d.ID] = i
	}
	return &Snapshot{At: at, Devices: devices, byID: byID}
}

// Get returns the device with the given ID.
func (s *Snapshot) Get(id string) (Device, bool) {
	if s == nil {
		return Device{}, false
	}
	i, ok := s.byID[id]
	if !ok {
		return Device{}, false
	}
	return s.Devices[i], true
}

// List returns every device of the given kind, or all of them when kind is empty.
func (s *Snapshot) List(kind discovery.Kind) []Device {
	if s == nil {
		return nil
	}
	if kind == "" {
		return slices.Clone(s.Devices)
	}
	out := make([]Device, 0, len(s.Devices))
	for _, d := range s.Devices {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

// Len reports the device count.
func (s *Snapshot) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Devices)
}

// CountByKind summarises the fleet for self-metrics.
func (s *Snapshot) CountByKind() map[discovery.Kind]int {
	out := map[discovery.Kind]int{}
	if s == nil {
		return out
	}
	for _, d := range s.Devices {
		out[d.Kind]++
	}
	return out
}

// NewSnapshotForTest builds a snapshot directly. It exists so that tests in other
// packages can exercise handlers without standing up the whole registry actor.
func NewSnapshotForTest(at time.Time, devices []Device) *Snapshot {
	return newSnapshot(at, devices)
}
