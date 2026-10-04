package shellycloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// DeviceStatus is one device as /device/all_status reports it.
type DeviceStatus struct {
	// ID is the cloud's device id. For Gen1 and Gen2+ it is the device's MAC in lowercase
	// hex, which is what lets a cloud observation merge with an mDNS one.
	ID string

	// Gen is the generation (1, 2, 3, ...), or 0 when the cloud does not say or reports
	// something that is not a Wi-Fi device generation, such as a BLU sensor.
	Gen int

	// MAC is the device's MAC in lowercase hex, from its status, or "" when the status
	// does not report one. It usually equals ID, but not for early Gen1 devices, whose
	// cloud id is only the last three bytes.
	MAC string

	// Code is the model code, e.g. SHSW-1 or SNSW-001P16EU.
	Code string

	// Online is the cloud's view of the device's cloud connection. A device can be
	// reachable on the LAN while its cloud connection is down, and vice versa.
	Online bool

	// Addr is the LAN address the device last reported. It is the zero Addr when the
	// status carries none, as for devices that reach the cloud through a gateway.
	Addr netip.Addr
}

// AllStatus lists every device the account owns, with its last-reported LAN address.
// Shared devices are excluded: they belong to someone else's network.
func (c *Client) AllStatus(ctx context.Context) ([]DeviceStatus, error) {
	data, err := c.post(ctx, "/device/all_status", url.Values{
		"show_info": {"true"},
		"no_shared": {"true"},
	})
	if err != nil {
		return nil, err
	}
	return decodeAllStatus(data)
}

// statusDoc is the subset of a device's status that locates it. The three address fields
// cover both protocols: Gen1 reports wifi_sta.ip, Gen2+ wifi.sta_ip and, on the wired Pro
// range, eth.ip. Each is null when that interface is down.
type statusDoc struct {
	MAC text // Gen1
	Sys struct {
		MAC text `json:"mac"`
	} // Gen2+
	DevInfo struct {
		ID     text            `json:"id"`
		Gen    json.RawMessage `json:"gen"`
		Code   text            `json:"code"`
		Online json.RawMessage `json:"online"`
	}
	WiFiSTA struct {
		IP text `json:"ip"`
	}
	WiFi struct {
		StaIP text `json:"sta_ip"`
	}
	Eth struct {
		IP text `json:"ip"`
	}
}

// decodeStatus decodes each field of a status independently, so that one field of an
// unexpected shape costs that field alone rather than the device.
func decodeStatus(raw json.RawMessage) (statusDoc, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return statusDoc{}, false
	}
	var doc statusDoc
	for name, dst := range map[string]any{
		"mac":       &doc.MAC,
		"sys":       &doc.Sys,
		"_dev_info": &doc.DevInfo,
		"wifi_sta":  &doc.WiFiSTA,
		"wifi":      &doc.WiFi,
		"eth":       &doc.Eth,
	} {
		if v, ok := fields[name]; ok {
			_ = json.Unmarshal(v, dst)
		}
	}
	return doc, true
}

func decodeAllStatus(data json.RawMessage) ([]DeviceStatus, error) {
	var body struct {
		DevicesStatus map[string]json.RawMessage `json:"devices_status"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("shelly cloud all_status: %w", err)
	}

	out := make([]DeviceStatus, 0, len(body.DevicesStatus))
	for key, raw := range body.DevicesStatus {
		doc, ok := decodeStatus(raw)
		if !ok {
			// One device's status changing shape must not cost us the whole fleet.
			continue
		}
		id := string(doc.DevInfo.ID)
		if id == "" {
			id = key
		}
		out = append(out, DeviceStatus{
			ID:     strings.ToLower(id),
			Gen:    parseGen(doc.DevInfo.Gen),
			MAC:    statusMAC(string(doc.MAC), string(doc.Sys.MAC)),
			Code:   string(doc.DevInfo.Code),
			Online: parseBool(doc.DevInfo.Online),
			Addr:   firstAddr(string(doc.WiFiSTA.IP), string(doc.WiFi.StaIP), string(doc.Eth.IP)),
		})
	}
	// Map iteration order is random; a stable order keeps logs and tests deterministic.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// parseGen reads "G1", "G2", ... as well as a bare number. Anything else, such as the
// "GBLE" the cloud uses for Bluetooth sensors, is not a generation we can scrape.
func parseGen(raw json.RawMessage) int {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0
	}
	switch g := v.(type) {
	case float64:
		return int(g)
	case string:
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(g)), "G"))
		if err != nil || n < 1 || n > 9 {
			return 0
		}
		return n
	}
	return 0
}

// parseBool accepts the cloud's true/false as well as the 0/1 its v2 API uses.
func parseBool(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case string:
		ok, _ := strconv.ParseBool(b)
		return ok || b == "1"
	}
	return false
}

// statusMAC returns the first candidate that is a full MAC, normalised. A MAC that
// arrived as a JSON number has lost any leading zeros, so an all-digit candidate is
// padded back to twelve digits.
func statusMAC(candidates ...string) string {
	for _, s := range candidates {
		mac := normaliseMAC(s)
		if mac != "" && len(mac) < 12 && strings.Trim(mac, "0123456789") == "" {
			mac = strings.Repeat("0", 12-len(mac)) + mac
		}
		if len(mac) == 12 {
			return mac
		}
	}
	return ""
}

func firstAddr(candidates ...string) netip.Addr {
	for _, s := range candidates {
		if a, err := netip.ParseAddr(strings.TrimSpace(s)); err == nil && a.IsValid() && !a.IsUnspecified() {
			return a.Unmap()
		}
	}
	return netip.Addr{}
}
