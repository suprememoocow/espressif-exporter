package probe

import "sync/atomic"

// atomic64 is a thin alias so the counters above read cleanly.
type atomic64 struct{ v atomic.Int64 }

func (a *atomic64) Add(n int64) int64 { return a.v.Add(n) }
func (a *atomic64) Load() int64       { return a.v.Load() }
