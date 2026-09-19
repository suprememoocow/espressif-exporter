package config

import (
	"fmt"
	"os"
	"strings"
)

const redacted = "<secret>"

// Secret is a string that refuses to reveal itself when formatted or serialised.
//
// prometheus/common/config has an equivalent type, but importing it drags in jwt, uuid,
// oauth2 and go-conntrack for a dozen lines of redaction — a poor trade for an image we
// want small and quiet in a CVE scan.
//
// All three of String, MarshalYAML and MarshalJSON must be implemented: missing any one
// leaks the value through a %v, a config dump or a debug endpoint respectively.
type Secret string

// String implements fmt.Stringer, covering %v and %s.
func (s Secret) String() string { return redactedIfSet(s) }

// GoString implements fmt.GoStringer, covering %#v.
func (s Secret) GoString() string { return redactedIfSet(s) }

// MarshalYAML redacts the value in YAML output.
func (s Secret) MarshalYAML() (any, error) { return redactedIfSet(s), nil }

// MarshalJSON redacts the value in JSON output.
func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte(`""`), nil
	}
	return []byte(`"` + redacted + `"`), nil
}

// Reveal returns the underlying value. Call this only where the secret is actually used.
func (s Secret) Reveal() string { return string(s) }

// IsSet reports whether a value is present.
func (s Secret) IsSet() bool { return s != "" }

func redactedIfSet(s Secret) string {
	if s == "" {
		return ""
	}
	return redacted
}

// resolveSecret returns inline if set, else reads file. Setting both is a configuration
// error rather than a silent precedence rule, because guessing wrong fails as an
// intermittent auth error rather than a startup error.
func resolveSecret(name string, inline Secret, file string) (Secret, error) {
	switch {
	case inline.IsSet() && file != "":
		return "", fmt.Errorf("%s: set either the inline value or the _file variant, not both", name)
	case file != "":
		b, err := os.ReadFile(file) //nolint:gosec // operator-supplied path, by design
		if err != nil {
			return "", fmt.Errorf("%s: reading %s: %w", name, file, err)
		}
		return Secret(strings.TrimRight(string(b), "\r\n")), nil
	default:
		return inline, nil
	}
}
