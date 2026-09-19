// Package multi fans several discovery backends into a single event stream.
package multi

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// Multi runs every configured backend concurrently.
type Multi struct {
	backends []discovery.Discoverer
	log      *slog.Logger
	dropped  atomic.Uint64
}

// New builds a fan-in over the given backends.
func New(log *slog.Logger, backends ...discovery.Discoverer) *Multi {
	return &Multi{backends: backends, log: log.With("component", "discovery")}
}

// Backends exposes the underlying discoverers for health reporting.
func (m *Multi) Backends() []discovery.Discoverer { return m.backends }

// Dropped reports events discarded because the registry fell behind.
func (m *Multi) Dropped() uint64 { return m.dropped.Load() }

// Run blocks until ctx is cancelled or a backend returns a fatal error.
//
// Backends own their own retry loops, so a returned error is genuinely unrecoverable
// (for example, Avahi is required, is the only source, and never came up).
func (m *Multi) Run(ctx context.Context, out chan<- discovery.Event) error {
	g, ctx := errgroup.WithContext(ctx)

	var once sync.Once
	for _, b := range m.backends {
		g.Go(func() error {
			m.log.Info("discovery backend starting", "source", b.Name())
			err := b.Run(ctx, m.wrap(ctx, out))
			if err != nil {
				once.Do(func() {
					m.log.Error("discovery backend failed", "source", b.Name(), "error", err)
				})
			}
			return err
		})
	}
	return g.Wait()
}

// wrap returns a channel that never blocks a backend's reader goroutine. Losing an event
// under extreme load is preferable to stalling the D-Bus signal reader, which would look
// to the backend like a healthy but silent connection.
func (m *Multi) wrap(ctx context.Context, out chan<- discovery.Event) chan<- discovery.Event {
	relay := make(chan discovery.Event, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-relay:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				default:
					if n := m.dropped.Add(1); n == 1 || n%100 == 0 {
						m.log.Warn("discovery events dropped; registry is behind",
							"source", ev.Source, "total", n)
					}
				}
			}
		}
	}()
	return relay
}
