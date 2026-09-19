package shelly

import (
	"encoding/json"
	"testing"

	"github.com/suprememoocow/espressif-exporter/internal/registry"
)

// Generation is decided by which key is present, never by HTTP status: a Gen2 device
// with authentication enabled still answers /shelly with 200.
func TestIdentityGenerationDetection(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantGen int
		wantMAC string
		wantVer string
		wantErr bool
	}{
		{
			name: "gen2",
			body: `{"name":"Living Room Lamp","id":"shellyplus1pm-a8032ab12345","mac":"A8032AB12345",
			        "model":"SNSW-001P16EU","gen":2,"fw_id":"20241011-114455/1.4.4-g6d2a586",
			        "ver":"1.4.4","app":"Plus1PM","auth_en":true}`,
			wantGen: 2, wantMAC: "a8032ab12345", wantVer: "1.4.4",
		},
		{
			name:    "gen3",
			body:    `{"id":"shelly1minig3-aabbccddeeff","mac":"AABBCCDDEEFF","gen":3,"ver":"1.4.4","auth_en":false}`,
			wantGen: 3, wantMAC: "aabbccddeeff", wantVer: "1.4.4",
		},
		{
			name: "gen1",
			body: `{"type":"SHSW-1","mac":"A8032AB1C2D3","auth":false,
			        "fw":"20230913-112003/v1.14.0-gcb84623","num_outputs":1}`,
			wantGen: 1, wantMAC: "a8032ab1c2d3", wantVer: "1.14.0",
		},
		{
			name:    "neither shape",
			body:    `{"something":"else"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r shellyResponse
			if err := json.Unmarshal([]byte(tt.body), &r); err != nil {
				t.Fatal(err)
			}
			id, err := r.toIdentity()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error for an unrecognised response shape")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id.Gen != tt.wantGen {
				t.Errorf("Gen = %d, want %d", id.Gen, tt.wantGen)
			}
			if id.MAC != tt.wantMAC {
				t.Errorf("MAC = %q, want %q", id.MAC, tt.wantMAC)
			}
			if id.Version != tt.wantVer {
				t.Errorf("Version = %q, want %q", id.Version, tt.wantVer)
			}
		})
	}
}

// If DHCP reassigns an address between discovery and scrape, we would otherwise scrape
// whatever now answers and label it with the original device's identity. That is silent
// data corruption, so it must fail the probe instead.
func TestVerifyMACRejectsAReassignedAddress(t *testing.T) {
	dev := registry.Device{ID: "mac:a8032ab1c2d3"}

	if err := verifyMAC(dev, Identity{MAC: "a8032ab1c2d3"}); err != nil {
		t.Errorf("a matching MAC should verify: %v", err)
	}
	if err := verifyMAC(dev, Identity{MAC: "ffffffffffff"}); err == nil {
		t.Error("a different MAC must be rejected")
	}

	// Nothing to compare against is not a failure: a Gen1 device known only by its
	// short ID has no full MAC in its identifier.
	if err := verifyMAC(registry.Device{ID: "shortid:shelly1-aabbcc"}, Identity{MAC: "a8032ab1c2d3"}); err != nil {
		t.Errorf("a device without a MAC-derived ID should skip the check: %v", err)
	}
	if err := verifyMAC(dev, Identity{MAC: ""}); err != nil {
		t.Errorf("a device that reports no MAC should skip the check: %v", err)
	}
}

func TestGen1VersionParsing(t *testing.T) {
	tests := map[string]string{
		"20230913-112003/v1.14.0-gcb84623": "1.14.0",
		"20211109-125708/v1.11.7-g682a0db": "1.11.7",
		"nonsense":                         "nonsense",
	}
	for in, want := range tests {
		if got := gen1Version(in); got != want {
			t.Errorf("gen1Version(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMACFromShellyID(t *testing.T) {
	tests := map[string]string{
		"shellyplus1pm-a8032ab12345": "a8032ab12345",
		"shelly1minig3-aabbccddeeff": "aabbccddeeff",
		"shelly1-aabbcc":             "", // only three bytes, not a full MAC
		"noseparator":                "",
	}
	for in, want := range tests {
		if got := macFromShellyID(in); got != want {
			t.Errorf("macFromShellyID(%q) = %q, want %q", in, got, want)
		}
	}
}
