package discovery

import (
	"strconv"
	"strings"
)

// Classify determines the device family from the mDNS service type and TXT record.
//
// The TXT record is authoritative where it speaks: ESPHome publishes mac and version on
// _esphomelib._tcp, and Shelly Gen2+ publish gen=2 or gen=3. The hostname heuristic
// exists only for Shelly Gen1, which advertises nothing but _http._tcp and so is
// otherwise indistinguishable from every printer and TV on the network.
func Classify(serviceType, instance string, txt map[string]string) Kind {
	switch normaliseServiceType(serviceType) {
	case ServiceESPHome:
		return KindESPHome
	case ServiceShelly:
		return KindShelly
	case ServiceHTTP:
		if _, ok := txt["gen"]; ok {
			return KindShelly
		}
		if looksLikeShellyGen1(instance) {
			return KindShelly
		}
		// ESPHome nodes with web_server: also advertise _http._tcp, carrying mac and
		// config_hash. We ignore them: the same node is already on _esphomelib._tcp, and
		// its mac TXT lets the registry merge the two records.
		return KindUnknown
	default:
		return KindUnknown
	}
}

// ShellyGeneration reads the gen TXT key. It returns 0 when absent, which means either a
// Gen1 device or a Gen2+ device whose TXT we did not see; the collector resolves the
// ambiguity with an unauthenticated GET /shelly.
func ShellyGeneration(txt map[string]string) int {
	g, err := strconv.Atoi(strings.TrimSpace(txt["gen"]))
	if err != nil || g < 1 || g > 9 {
		return 0
	}
	return g
}

// looksLikeShellyGen1 matches the shellyNAME-HEX naming Gen1 firmware uses, for example
// shelly1-AABBCC, shellyplug-s-1A2B3C, shellyht-4D5E6F.
func looksLikeShellyGen1(instance string) bool {
	name := strings.ToLower(instanceLabel(instance))
	if !strings.HasPrefix(name, "shelly") {
		return false
	}
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return false
	}
	suffix := name[i+1:]
	// Gen1 exposes the last three MAC bytes; Gen2+ instance names carry all six.
	if len(suffix) != 6 && len(suffix) != 12 {
		return false
	}
	for _, r := range suffix {
		if !isHexDigit(r) {
			return false
		}
	}
	return true
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// instanceLabel strips any trailing service and domain, leaving the instance name.
func instanceLabel(instance string) string {
	if i := strings.Index(instance, "._"); i > 0 {
		return instance[:i]
	}
	return strings.TrimSuffix(instance, ".")
}

// normaliseServiceType trims the trailing domain and dot that Avahi and zeroconf append
// inconsistently, so "_shelly._tcp.local." and "_shelly._tcp" compare equal.
func normaliseServiceType(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	s = strings.TrimSuffix(s, ".local")
	return strings.TrimSuffix(s, ".")
}
