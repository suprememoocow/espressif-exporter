package shelly

import (
	"regexp"
	"sort"
	"strconv"
	"sync"

	"github.com/suprememoocow/espressif-exporter/internal/metrics"
)

// Caps on the generic walk, so one pathological component cannot flood the output.
const (
	maxGenericDepth  = 3
	maxGenericLeaves = 40
	maxGenericIndex  = 8
	rawFamilyPrefix  = "raw_"
)

// genericDenylist covers leaves that look like measurements but are not: timestamps and
// revision counters.
var genericDenylist = regexp.MustCompile(
	`^(ts|_ts|minute_ts|id|_updated|cfg_rev|kvs_rev|schedule_rev|webhook_rev)$`)

// genericWalk exports an uncurated component's numeric leaves into a quarantined
// namespace.
//
// The tempting argument against doing this at all is cardinality, but that is not the
// real objection: a few hundred extra series across a home fleet is nothing. The real
// problems are that a generic walk cannot know a leaf's type (so a monotonic counter
// emitted as a gauge silently breaks rate() across reboots), cannot know its unit, and
// cannot notice when a firmware release renames a field — which blanks a dashboard with
// no error anywhere.
//
// So the walk is quarantined rather than disabled. Everything lands under espressif_raw_
// as an untyped gauge with no unit suffix and no _total suffix, because both would be
// lies, and the prefix tells anyone building on it that the name may change. The
// alternative — dropping data from a brand-new model on the day it ships — is the exact
// failure this exporter exists to avoid.
func genericWalk(e *metrics.Emitter, component, id string, m map[string]any) {
	leaves := make(map[string]float64, maxGenericLeaves)
	collectLeaves(m, "", 0, leaves)

	// Sorted so that truncation is deterministic rather than map-order roulette.
	names := make([]string, 0, len(leaves))
	for name := range leaves {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxGenericLeaves {
		names = names[:maxGenericLeaves]
	}

	for _, name := range names {
		family := rawFamilyPrefix + metrics.SanitizeName(component+"_"+name)
		registerRawFamily(e, family)
		e.Value(family, leaves[name])
	}
}

func collectLeaves(m map[string]any, prefix string, depth int, out map[string]float64) {
	if depth > maxGenericDepth || len(out) >= maxGenericLeaves*2 {
		return
	}
	for key, v := range m {
		if genericDenylist.MatchString(key) {
			continue
		}
		name := key
		if prefix != "" {
			name = prefix + "_" + key
		}

		switch t := v.(type) {
		case float64:
			out[name] = t
		case bool:
			b := 0.0
			if t {
				b = 1
			}
			out[name] = b
		case map[string]any:
			collectLeaves(t, name, depth+1, out)
		case []any:
			for i, item := range t {
				if i >= maxGenericIndex {
					break
				}
				// The index goes in the metric name, never in a label: an unbounded
				// index as a label is the one thing here that genuinely could explode.
				indexed := name + "_" + strconv.Itoa(i)
				switch iv := item.(type) {
				case float64:
					out[indexed] = iv
				case map[string]any:
					collectLeaves(iv, indexed, depth+1, out)
				}
			}
		}
	}
}

// registerRawFamily declares a family discovered at runtime.
//
// Registration is idempotent for an identical definition, and the help text here is a
// constant, so this is safe to call on every probe. It deliberately does not memoise in
// package-level state: the registry is per-process but constructed by the caller, and a
// global cache would register into whichever one happened to be built first.
func registerRawFamily(e *metrics.Emitter, family string) {
	_ = e.Registry().Register(metrics.Family{Name: family, Help: rawFamilyHelp})
}

const rawFamilyHelp = "Uncurated Shelly reading. Untyped and unitless; " +
	"the name may change between firmware releases."

// unknownCounter counts component types we have no extractor for, which is the feedback
// loop that tells an operator when promoting one to a curated extractor is due.
type unknownCounter struct {
	mu sync.Mutex
	m  map[string]uint64
}

func newUnknownCounter() *unknownCounter { return &unknownCounter{m: map[string]uint64{}} }

func (u *unknownCounter) inc(component string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.m[component]++
}

func (u *unknownCounter) snapshot() map[string]uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]uint64, len(u.m))
	for k, v := range u.m {
		out[k] = v
	}
	return out
}
