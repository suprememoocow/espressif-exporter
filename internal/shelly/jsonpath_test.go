package shelly

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A JSON null means "no reading", not zero. Emitting 0 for a faulted CT clamp would
// produce a plausible-looking wrong number, which is worse than a gap in the graph.
func TestNullIsIndistinguishableFromMissing(t *testing.T) {
	m := decode(t, `{"switch:0":{"apower":null,"voltage":241.3,"aenergy":{"total":13060}}}`)
	sw, ok := obj(m, "switch:0")
	if !ok {
		t.Fatal("switch:0 should decode")
	}

	if _, ok := num(sw, "apower"); ok {
		t.Error("a null apower must report ok=false, not 0")
	}
	if _, ok := num(sw, "nonexistent"); ok {
		t.Error("a missing key must report ok=false")
	}
	if v, ok := num(sw, "voltage"); !ok || v != 241.3 {
		t.Errorf("voltage = %v (ok=%v), want 241.3", v, ok)
	}
	if v, ok := num(sw, "aenergy", "total"); !ok || v != 13060 {
		t.Errorf("aenergy.total = %v (ok=%v), want 13060", v, ok)
	}
}

func TestAccessorsRejectWrongTypes(t *testing.T) {
	m := decode(t, `{"a":"text","b":42,"c":true,"d":{"e":1},"f":[1,2]}`)

	if _, ok := num(m, "a"); ok {
		t.Error("a string must not decode as a number")
	}
	if _, ok := boolean(m, "b"); ok {
		t.Error("a number must not decode as a boolean")
	}
	if _, ok := str(m, "c"); ok {
		t.Error("a boolean must not decode as a string")
	}
	if _, ok := obj(m, "f"); ok {
		t.Error("an array must not decode as an object")
	}
	if _, ok := arr(m, "d"); ok {
		t.Error("an object must not decode as an array")
	}
	// Walking through a non-object must not panic.
	if _, ok := num(m, "a", "b", "c"); ok {
		t.Error("walking through a scalar must fail cleanly")
	}
}

func TestAccessorsOnEmptyAndNilInput(t *testing.T) {
	for _, m := range []map[string]any{nil, {}} {
		if _, ok := num(m, "x"); ok {
			t.Error("num on an empty object should fail")
		}
		if _, ok := obj(m, "x"); ok {
			t.Error("obj on an empty object should fail")
		}
		if _, ok := num(m); ok {
			t.Error("an empty path should fail")
		}
	}
}
