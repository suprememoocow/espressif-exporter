package metrics

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// DescCache memoises prometheus.Desc values across probes.
//
// Descriptors are built from whatever entities a device happens to expose, so a naive
// implementation allocates one per entity per scrape. At roughly 3,000 entities and
// seven probes a second that is over 20,000 wasted allocations a second. The cache is
// bounded by the number of distinct (family, label-set) pairs — a few hundred — rather
// than by the number of entities.
type DescCache struct {
	mu sync.RWMutex
	m  map[string]*prometheus.Desc
}

// NewDescCache builds an empty cache.
func NewDescCache() *DescCache {
	return &DescCache{m: make(map[string]*prometheus.Desc, 256)}
}

// Get returns a cached descriptor, creating it on first use.
func (c *DescCache) Get(fqName, help string, labels []string) *prometheus.Desc {
	key := fqName + "\x00" + strings.Join(labels, ",")

	c.mu.RLock()
	d, ok := c.m[key]
	c.mu.RUnlock()
	if ok {
		return d
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok = c.m[key]; ok {
		return d
	}
	d = prometheus.NewDesc(fqName, help, labels, nil)
	c.m[key] = d
	return d
}

// Len reports the number of cached descriptors, for self-metrics.
func (c *DescCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}
