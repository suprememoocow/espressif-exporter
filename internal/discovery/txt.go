package discovery

import (
	"strings"
)

// ParseTXT converts raw DNS-SD TXT strings into a map.
//
// Per RFC 6763: a key with no '=' is a boolean flag (stored as an empty value), keys are
// case-insensitive, and on a duplicate key the first occurrence wins. Values may contain
// arbitrary bytes, so invalid UTF-8 is replaced rather than dropped — a mangled value is
// easier to debug than a missing one.
func ParseTXT(records []string) map[string]string {
	if len(records) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(records))
	for _, rec := range records {
		key, value, hasValue := strings.Cut(rec, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		if _, exists := out[key]; exists {
			continue // first wins
		}
		if !hasValue {
			out[key] = ""
			continue
		}
		out[key] = strings.ToValidUTF8(value, "�")
	}
	return out
}

// ParseTXTBytes adapts Avahi's [][]byte representation.
func ParseTXTBytes(records [][]byte) map[string]string {
	strs := make([]string, 0, len(records))
	for _, r := range records {
		strs = append(strs, string(r))
	}
	return ParseTXT(strs)
}
