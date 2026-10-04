package shellycloud

import (
	"bytes"
	"encoding/json"
)

// text decodes a JSON string or number as its text, and anything else as "".
//
// The cloud is inconsistent about types: a MAC made only of digits, such as
// 100200300400, arrives as a number. A plain string field would reject it and, with it,
// the whole device, so every identifier and name is decoded through this type.
type text string

// UnmarshalJSON implements json.Unmarshaler. It never fails: a value of an unexpected
// shape costs that one field, not the document around it.
func (t *text) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*t = ""
	switch {
	case len(b) == 0:
	case b[0] == '"':
		var s string
		if json.Unmarshal(b, &s) == nil {
			*t = text(s)
		}
	case b[0] == '-' || (b[0] >= '0' && b[0] <= '9'):
		*t = text(b)
	}
	return nil
}
