package registry

import (
	"regexp"
	"strings"

	"github.com/suprememoocow/espressif-exporter/internal/discovery"
)

// ID prefixes, in descending order of confidence.
const (
	prefixMAC     = "mac:"
	prefixShortID = "shortid:"
	prefixName    = "name:"
	prefixStatic  = "static:"
)

// macFromInstance matches the trailing MAC in a device's mDNS instance name.
// Gen2+ Shelly and ESPHome expose all six bytes; Gen1 Shelly exposes only the last three.
var macFromInstance = regexp.MustCompile(`(?i)-([0-9a-f]{12}|[0-9a-f]{6})$`)

var nonHex = regexp.MustCompile(`[^0-9a-f]`)

// DeriveID produces a stable device identifier, preferring the strongest evidence
// available. The ordering matters because a weaker ID can later be promoted to a
// stronger one (see Registry.promote), whereas the reverse would split a device in two.
//
//  1. mac:  — a full MAC, from the ESPHome TXT record, a Shelly identity probe, or a
//     Gen2+ instance name.
//  2. shortid: — a Gen1 Shelly name, which reveals only the last three MAC bytes.
//  3. name: — anything else.
func DeriveID(kind discovery.Kind, instance string, txt map[string]string) string {
	if mac := NormaliseMAC(txt["mac"]); len(mac) == 12 {
		return prefixMAC + mac
	}
	// Shelly Gen2+ publish the device id, which embeds the full MAC.
	if mac := macFromName(txt["id"]); len(mac) == 12 {
		return prefixMAC + mac
	}

	label := instanceLabel(instance)
	switch mac := macFromName(label); len(mac) {
	case 12:
		return prefixMAC + mac
	case 6:
		return prefixShortID + strings.ToLower(label)
	}
	return prefixName + strings.ToLower(label)
}

// StaticID namespaces an operator-supplied identifier so it cannot collide with a
// discovered one.
func StaticID(id string) string { return discovery.StaticID(id) }

// IsMACID reports whether id carries a full MAC, i.e. the strongest identity.
func IsMACID(id string) bool { return strings.HasPrefix(id, prefixMAC) }

// MACOfID extracts the MAC from a mac: identifier, or "" for any other form.
func MACOfID(id string) string {
	if !IsMACID(id) {
		return ""
	}
	return strings.TrimPrefix(id, prefixMAC)
}

// NormaliseMAC lowercases a MAC and strips every separator, so "A8:03:2A:B1:C2:D3",
// "a8-03-2a-b1-c2-d3" and "a8032ab1c2d3" all compare equal.
func NormaliseMAC(s string) string {
	return nonHex.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "")
}

func macFromName(s string) string {
	m := macFromInstance.FindStringSubmatch(instanceLabel(s))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// instanceLabel strips any service and domain suffix, leaving the instance name.
func instanceLabel(instance string) string {
	if i := strings.Index(instance, "._"); i > 0 {
		instance = instance[:i]
	}
	return strings.TrimSuffix(strings.TrimSuffix(instance, "."), ".local")
}
