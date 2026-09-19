package discovery

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		service  string
		instance string
		txt      map[string]string
		want     Kind
	}{
		{"esphome by service type", ServiceESPHome, "bedroom-sensor", map[string]string{"mac": "a4cf129b3e70"}, KindESPHome},
		{"esphome with trailing domain", "_esphomelib._tcp.local.", "bedroom-sensor", nil, KindESPHome},
		{"shelly gen2 by service type", ServiceShelly, "shellyplus1pm-a8032ab12345", map[string]string{"gen": "2"}, KindShelly},
		{"shelly gen3 on http with gen txt", ServiceHTTP, "shelly1minig3-abc", map[string]string{"gen": "3"}, KindShelly},
		{"shelly gen1 on http by name", ServiceHTTP, "shelly1-AABBCC", nil, KindShelly},
		{"shelly gen1 plug-s", ServiceHTTP, "shellyplug-s-1A2B3C", nil, KindShelly},
		{"shelly gen1 with service suffix", ServiceHTTP, "shellyht-4D5E6F._http._tcp.local", nil, KindShelly},
		// An ESPHome node with web_server: also lands on _http._tcp. We ignore it there;
		// the same node is already on _esphomelib._tcp and merges via its mac TXT.
		{"esphome web_server on http", ServiceHTTP, "bedroom-sensor", map[string]string{"mac": "a4cf129b3e70"}, KindUnknown},
		{"random printer", ServiceHTTP, "Brother HL-L2350DW", nil, KindUnknown},
		{"shelly-prefixed but wrong suffix length", ServiceHTTP, "shellything-12345", nil, KindUnknown},
		{"shelly-prefixed but not hex", ServiceHTTP, "shellyroom-livingr", nil, KindUnknown},
		{"unrelated service", "_airplay._tcp", "Apple TV", nil, KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.service, tt.instance, tt.txt); got != tt.want {
				t.Errorf("Classify(%q, %q, %v) = %q, want %q", tt.service, tt.instance, tt.txt, got, tt.want)
			}
		})
	}
}

func TestShellyGeneration(t *testing.T) {
	tests := []struct {
		txt  map[string]string
		want int
	}{
		{map[string]string{"gen": "2"}, 2},
		{map[string]string{"gen": "3"}, 3},
		{map[string]string{"gen": " 4 "}, 4},
		{map[string]string{"gen": "banana"}, 0},
		{map[string]string{"gen": ""}, 0},
		{nil, 0},
	}
	for _, tt := range tests {
		if got := ShellyGeneration(tt.txt); got != tt.want {
			t.Errorf("ShellyGeneration(%v) = %d, want %d", tt.txt, got, tt.want)
		}
	}
}

func TestParseTXT(t *testing.T) {
	got := ParseTXT([]string{
		"mac=A4CF129B3E70",
		"version=2026.7.1",
		"ota_signed", // flag-style key with no '='
		"MAC=ffffffffffff",
		"", // empty record
		"=novalue",
		"friendly_name=Bedroom \xff Sensor", // invalid UTF-8
	})

	// Keys are lowercased, and on a duplicate the first occurrence wins per RFC 6763.
	if got["mac"] != "A4CF129B3E70" {
		t.Errorf("mac = %q, want the first occurrence A4CF129B3E70", got["mac"])
	}
	if v, ok := got["ota_signed"]; !ok || v != "" {
		t.Errorf("ota_signed = %q (present=%v), want an empty value", v, ok)
	}
	if _, ok := got[""]; ok {
		t.Error("an empty key should be skipped")
	}
	if got["friendly_name"] != "Bedroom � Sensor" {
		t.Errorf("friendly_name = %q, want invalid UTF-8 replaced", got["friendly_name"])
	}
	if len(got) != 4 {
		t.Errorf("got %d keys (%v), want 4", len(got), got)
	}
}

func TestParseTXTEmpty(t *testing.T) {
	if got := ParseTXT(nil); got == nil || len(got) != 0 {
		t.Errorf("ParseTXT(nil) = %v, want an empty non-nil map", got)
	}
}
