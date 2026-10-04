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

// Both cloud features need the account. A missing key would otherwise show up only as a
// backend that is never up, or names that silently never arrive.
func TestShellyCloudNeedsServerAndKey(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		mutate func(*Config)
	}{
		{"discovery source", func(c *Config) { c.Discovery.Sources = []string{"avahi", "shelly_cloud"} }},
		{"names", func(c *Config) { c.Shelly.Cloud.Names = true }},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("cloud feature without credentials was accepted")
			}
			for _, field := range []string{"shelly.cloud.server", "shelly.cloud.auth_key"} {
				if !strings.Contains(err.Error(), field) {
					t.Errorf("error does not name %s: %v", field, err)
				}
			}

			c.Shelly.Cloud.Server = "https://shelly-58-eu.shelly.cloud"
			c.Shelly.Cloud.AuthKey = "key"
			if err := c.Validate(); err != nil {
				t.Errorf("valid cloud config rejected: %v", err)
			}
		})
	}
}

// A poll is the only thing that re-observes a cloud-only device, the same cliff Avahi's
// rebrowse has.
func TestEndpointTTLMustExceedShellyCloudInterval(t *testing.T) {
	c := Default()
	c.Discovery.Sources = []string{"shelly_cloud"}
	c.Shelly.Cloud.Server = "https://shelly-58-eu.shelly.cloud"
	c.Shelly.Cloud.AuthKey = "key"
	c.Discovery.ShellyCloud.Interval = time.Hour

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "discovery.shelly_cloud.interval") {
		t.Errorf("err = %v, want the interval named as exceeding endpoint_ttl", err)
	}
}
