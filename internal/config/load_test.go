package config

import (
	"strings"
	"testing"
	"time"
)

// The shipped defaults have to satisfy the exporter's own invariants, or every operator
// who does not override them starts from a broken configuration.
func TestDefaultsAreValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config is invalid: %v", err)
	}
}

// Avahi replays its record cache only to a newly created browser, so a rebrowse is the
// only thing that re-observes a device quiet on mDNS. An endpoint_ttl below that interval
// guarantees every device goes addressless partway through each cycle, taking its metrics
// with it — a failure that is very hard to read from the outside, so it is refused here.
func TestEndpointTTLMustExceedRebrowseInterval(t *testing.T) {
	c := Default()
	c.Discovery.Sources = []string{"avahi", "static"}
	c.Discovery.Avahi.RebrowseInterval = 30 * time.Minute
	c.Registry.EndpointTTL = 10 * time.Minute

	err := c.Validate()
	if err == nil {
		t.Fatal("endpoint_ttl below rebrowse_interval was accepted")
	}
	if !strings.Contains(err.Error(), "rebrowse_interval") {
		t.Errorf("error does not name the conflicting setting: %v", err)
	}
}

// The constraint belongs to the Avahi backend alone. The zeroconf library re-delivers
// entries on its own periodic re-queries, so there is no rebrowse cliff to clear.
func TestEndpointTTLIsUnconstrainedWithoutAvahi(t *testing.T) {
	c := Default()
	c.Discovery.Sources = []string{"zeroconf", "static"}
	c.Discovery.Avahi.RebrowseInterval = 30 * time.Minute
	c.Registry.EndpointTTL = 10 * time.Minute
	c.Registry.DeviceTTL = 30 * time.Minute

	if err := c.Validate(); err != nil {
		t.Fatalf("a short endpoint_ttl without avahi should be fine: %v", err)
	}
}

func TestDeviceTTLMustCoverEndpointTTL(t *testing.T) {
	c := Default()
	c.Registry.DeviceTTL = time.Minute

	err := c.Validate()
	if err == nil {
		t.Fatal("device_ttl below endpoint_ttl was accepted")
	}
	if !strings.Contains(err.Error(), "device_ttl") {
		t.Errorf("error does not name the conflicting setting: %v", err)
	}
}
