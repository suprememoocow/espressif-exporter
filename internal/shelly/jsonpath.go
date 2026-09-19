package shelly

import "encoding/json"

// Nil-safe accessors over a decoded JSON object.
//
// The load-bearing rule is that a JSON null returns ok=false, exactly like a missing
// key. Shelly emits null when a measurement is unavailable — a faulted sensor, an
// unpowered channel, a Shelly 1 with no power meter — and emitting 0 for a broken CT
// clamp produces a plausible-looking wrong number, which is worse than a gap.

func obj(m map[string]any, path ...string) (map[string]any, bool) {
	cur := m
	for _, key := range path {
		v, ok := cur[key]
		if !ok || v == nil {
			return nil, false
		}
		cur, ok = v.(map[string]any)
		if !ok {
			return nil, false
		}
	}
	return cur, cur != nil
}

// leaf walks every path element but the last, returning the final container and key.
func leaf(m map[string]any, path []string) (any, bool) {
	if len(path) == 0 {
		return nil, false
	}
	cur := m
	for _, key := range path[:len(path)-1] {
		next, ok := obj(cur, key)
		if !ok {
			return nil, false
		}
		cur = next
	}
	v, ok := cur[path[len(path)-1]]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

func num(m map[string]any, path ...string) (float64, bool) {
	v, ok := leaf(m, path)
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case int:
		return float64(t), true
	default:
		return 0, false
	}
}

func boolean(m map[string]any, path ...string) (bool, bool) {
	v, ok := leaf(m, path)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func str(m map[string]any, path ...string) (string, bool) {
	v, ok := leaf(m, path)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func arr(m map[string]any, path ...string) ([]any, bool) {
	v, ok := leaf(m, path)
	if !ok {
		return nil, false
	}
	a, ok := v.([]any)
	return a, ok
}

// strs extracts a string slice, skipping non-string members.
func strs(m map[string]any, path ...string) ([]string, bool) {
	a, ok := arr(m, path...)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}
