// Package config defines the exporter's configuration and its defaults.
package config

import (
	"time"
)

// Config is the root configuration. Every duration has a default; see Default().
type Config struct {
	Log       Log       `koanf:"log"`
	Server    Server    `koanf:"server"`
	Discovery Discovery `koanf:"discovery"`
	Registry  Registry  `koanf:"registry"`
	Probe     Probe     `koanf:"probe"`
	Shelly    Shelly    `koanf:"shelly"`
	ESPHome   ESPHome   `koanf:"esphome"`
	Metrics   Metrics   `koanf:"metrics"`
}

type Log struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

type Server struct {
	Listen            string        `koanf:"listen"`
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout"`
	ShutdownTimeout   time.Duration `koanf:"shutdown_timeout"`
	MaxRequests       int           `koanf:"max_requests"`
	WebConfigFile     string        `koanf:"web_config_file"`
	Pprof             bool          `koanf:"pprof"`
}

type Discovery struct {
	// Sources is an ordered list of backends: avahi, zeroconf, static.
	Sources      []string `koanf:"sources"`
	ServiceTypes []string `koanf:"service_types"`

	// IPv6 is off by default: a Docker bridge network usually has no IPv6 at all, so
	// preferring an IPv6 address makes a device permanently unscrapeable.
	IPv6       bool     `koanf:"ipv6"`
	AllowCIDRs []string `koanf:"allow_cidrs"`
	DenyCIDRs  []string `koanf:"deny_cidrs"`

	// StaleAfter marks a device stale in /sd without removing it, so the operator
	// decides the drop policy in relabel_configs rather than the exporter.
	StaleAfter time.Duration `koanf:"stale_after"`

	Avahi  Avahi         `koanf:"avahi"`
	Static []StaticEntry `koanf:"static"`
}

type Avahi struct {
	// DBusAddress overrides $DBUS_SYSTEM_BUS_ADDRESS. Distroless has no
	// /var/run -> /run symlink, while godbus defaults to /var/run/dbus/system_bus_socket.
	DBusAddress string `koanf:"dbus_address"`

	// Required makes a permanently unavailable Avahi fatal at startup, but only when no
	// other discovery source is configured. Runtime loss is never fatal.
	Required bool `koanf:"required"`

	StartupTimeout time.Duration `koanf:"startup_timeout"`
	ReconnectMin   time.Duration `koanf:"reconnect_min"`
	ReconnectMax   time.Duration `koanf:"reconnect_max"`

	ResolveTimeout time.Duration `koanf:"resolve_timeout"`
	// ResolveConcurrency stays well under avahi-daemon's objects-per-client-max (1024).
	ResolveConcurrency int `koanf:"resolve_concurrency"`

	// HealthCheckInterval drives the watchdog that catches the silent wedge: neither
	// NameOwnerChanged nor StateChanged fires, but no events ever arrive.
	HealthCheckInterval time.Duration `koanf:"health_check_interval"`
	RebrowseInterval    time.Duration `koanf:"rebrowse_interval"`
}

type StaticEntry struct {
	ID      string `koanf:"id"`
	Kind    string `koanf:"kind"`
	Address string `koanf:"address"`
	Port    uint16 `koanf:"port"`
}

type Registry struct {
	RefreshInterval time.Duration `koanf:"refresh_interval"`

	// EndpointTTL is how long an address survives with nothing confirming it. Both an
	// mDNS announcement and a successful scrape count as confirmation, so this is not a
	// measure of how chatty a device is on mDNS. It must exceed the interval at which
	// discovery re-observes a device: Avahi only replays its cache to a new browser, so
	// for the Avahi backend that interval is avahi.rebrowse_interval, and a shorter TTL
	// leaves every device addressless between rebrowses.
	EndpointTTL      time.Duration `koanf:"endpoint_ttl"`
	DeviceTTL        time.Duration `koanf:"device_ttl"`
	RemoveGrace      time.Duration `koanf:"remove_grace"`
	SnapshotCoalesce time.Duration `koanf:"snapshot_coalesce"`
}

type Probe struct {
	DefaultTimeout time.Duration `koanf:"default_timeout"`
	// TimeoutOffset is subtracted from the scrape timeout so the exporter finishes
	// writing the exposition before Prometheus gives up; otherwise a slow device costs
	// us the probe_success 0 sample we most need.
	TimeoutOffset time.Duration `koanf:"timeout_offset"`
	MaxTimeout    time.Duration `koanf:"max_timeout"`
	MinTimeout    time.Duration `koanf:"min_timeout"`

	// MaxConcurrent bounds wifi airtime, not CPU: 120 simultaneous transactions on one
	// home AP cause contention and retransmits that look like device failures.
	MaxConcurrent int `koanf:"max_concurrent"`

	// CacheTTL collapses near-simultaneous scrapes onto one device request. Never raise
	// this far: a longer cache silently serves stale data to a faster scraper.
	CacheTTL time.Duration `koanf:"cache_ttl"`

	Retries int     `koanf:"retries"`
	Backoff Backoff `koanf:"backoff"`
}

type Backoff struct {
	FailuresBeforeOpen int           `koanf:"failures_before_open"`
	Initial            time.Duration `koanf:"initial"`
	Max                time.Duration `koanf:"max"`
	// AuthInitial/AuthMax are longer: retrying a wrong password every 30s will never
	// succeed, and some Shelly firmwares rate-limit repeated auth failures.
	AuthInitial time.Duration `koanf:"auth_initial"`
	AuthMax     time.Duration `koanf:"auth_max"`
	Factor      float64       `koanf:"factor"`
	Jitter      float64       `koanf:"jitter"`
}

type Shelly struct {
	Auth                  ShellyAuth    `koanf:"auth"`
	DialTimeout           time.Duration `koanf:"dial_timeout"`
	ResponseHeaderTimeout time.Duration `koanf:"response_header_timeout"`
	IdentityTTL           time.Duration `koanf:"identity_ttl"`
	FetchConfig           bool          `koanf:"fetch_config"`
	GenericFallback       bool          `koanf:"generic_fallback"`
	MaxBodyBytes          int64         `koanf:"max_body_bytes"`
}

type ShellyAuth struct {
	Username     string               `koanf:"username"`
	Password     Secret               `koanf:"password"`
	PasswordFile string               `koanf:"password_file"`
	Overrides    []ShellyAuthOverride `koanf:"overrides"`
}

type ShellyAuthOverride struct {
	Match        ShellyAuthMatch `koanf:"match"`
	Username     string          `koanf:"username"`
	Password     Secret          `koanf:"password"`
	PasswordFile string          `koanf:"password_file"`
}

// ShellyAuthMatch fields are ANDed. Overrides are evaluated top to bottom, first match
// wins. Matching on IP is deliberately unsupported: DHCP makes it a trap.
type ShellyAuthMatch struct {
	DeviceID string `koanf:"device_id"`
	MAC      string `koanf:"mac"`
	Hostname string `koanf:"hostname"`
	Gen      int    `koanf:"gen"`
}

type ESPHome struct {
	// EncryptionKey is the single shared Noise PSK for the fleet, base64, 32 bytes.
	EncryptionKey     Secret `koanf:"encryption_key"`
	EncryptionKeyFile string `koanf:"encryption_key_file"`
	RequireEncryption bool   `koanf:"require_encryption"`

	// Password is the legacy plaintext API password, for nodes on firmware older
	// than ESPHome 2026.1.0 where it was removed. Such a node accepts the connection
	// and answers DeviceInfo, then silently drops it -- a failure that is otherwise
	// very hard to attribute. Prefer an encryption key where the firmware supports one.
	Password     Secret `koanf:"password"`
	PasswordFile string `koanf:"password_file"`
	Port         uint16 `koanf:"port"`

	DialTimeout         time.Duration `koanf:"dial_timeout"`
	HandshakeTimeout    time.Duration `koanf:"handshake_timeout"`
	ListEntitiesTimeout time.Duration `koanf:"list_entities_timeout"`
	ConnectBudget       time.Duration `koanf:"connect_budget"`

	PingInterval      time.Duration `koanf:"ping_interval"`
	PingTimeout       time.Duration `koanf:"ping_timeout"`
	PingFailThreshold int           `koanf:"ping_fail_threshold"`
	ReadDeadline      time.Duration `koanf:"read_deadline"`
	RefreshInterval   time.Duration `koanf:"refresh_interval"`

	MaxConcurrentConnects int `koanf:"max_concurrent_connects"`
	MaxEntitiesPerDevice  int `koanf:"max_entities_per_device"`
	MaxOrphanStates       int `koanf:"max_orphan_states"`

	// MaxStateAge suppresses values older than this. Default 0 (disabled): "old" is often
	// legitimate, and thresholding would break rate() on energy counters exactly when it
	// matters. Freshness is exposed as a timestamp metric instead.
	MaxStateAge time.Duration `koanf:"max_state_age"`
}

type Metrics struct {
	// PercentAsRatio divides percentages by 100 so humidity reads 0.482, per the
	// Prometheus convention. Grafana's percentunit renders 0-1 with no configuration.
	PercentAsRatio bool `koanf:"percent_as_ratio"`
	TextSensors    bool `koanf:"text_sensors"`
	LightChannels  bool `koanf:"light_channels"`
	// MaxTextValues caps distinct values tracked per text entity before suppressing it.
	MaxTextValues     int `koanf:"max_text_values"`
	MaxTextValueBytes int `koanf:"max_text_value_bytes"`
}

// Default returns the baseline configuration. Every value here is deliberate; see the
// comments on the struct fields for the reasoning behind the non-obvious ones.
func Default() Config {
	return Config{
		Log: Log{Level: "info", Format: "json"},
		Server: Server{
			Listen:            ":9826",
			ReadHeaderTimeout: 5 * time.Second,
			ShutdownTimeout:   10 * time.Second,
			MaxRequests:       64,
		},
		Discovery: Discovery{
			Sources:      []string{"avahi", "static"},
			ServiceTypes: []string{"_esphomelib._tcp", "_shelly._tcp", "_http._tcp"},
			IPv6:         false,
			DenyCIDRs:    []string{"169.254.0.0/16", "fe80::/10"},
			StaleAfter:   24 * time.Hour,
			Avahi: Avahi{
				Required:            true,
				StartupTimeout:      30 * time.Second,
				ReconnectMin:        time.Second,
				ReconnectMax:        60 * time.Second,
				ResolveTimeout:      5 * time.Second,
				ResolveConcurrency:  8,
				HealthCheckInterval: 30 * time.Second,
				RebrowseInterval:    30 * time.Minute,
			},
		},
		Registry: Registry{
			RefreshInterval:  5 * time.Minute,
			EndpointTTL:      45 * time.Minute,
			DeviceTTL:        90 * time.Minute,
			RemoveGrace:      5 * time.Minute,
			SnapshotCoalesce: 250 * time.Millisecond,
		},
		Probe: Probe{
			DefaultTimeout: 10 * time.Second,
			TimeoutOffset:  500 * time.Millisecond,
			MaxTimeout:     30 * time.Second,
			MinTimeout:     time.Second,
			MaxConcurrent:  32,
			CacheTTL:       2 * time.Second,
			Retries:        1,
			Backoff: Backoff{
				FailuresBeforeOpen: 3,
				Initial:            15 * time.Second,
				Max:                5 * time.Minute,
				AuthInitial:        time.Minute,
				AuthMax:            15 * time.Minute,
				Factor:             2,
				Jitter:             0.2,
			},
		},
		Shelly: Shelly{
			Auth:                  ShellyAuth{Username: "admin"},
			DialTimeout:           2 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			IdentityTTL:           6 * time.Hour,
			FetchConfig:           true,
			GenericFallback:       true,
			MaxBodyBytes:          1 << 20,
		},
		ESPHome: ESPHome{
			Port:                  6053,
			DialTimeout:           5 * time.Second,
			HandshakeTimeout:      5 * time.Second,
			ListEntitiesTimeout:   20 * time.Second,
			ConnectBudget:         45 * time.Second,
			PingInterval:          20 * time.Second,
			PingTimeout:           10 * time.Second,
			PingFailThreshold:     3,
			ReadDeadline:          90 * time.Second,
			RefreshInterval:       5 * time.Minute,
			MaxConcurrentConnects: 16,
			MaxEntitiesPerDevice:  2000,
			MaxOrphanStates:       256,
			MaxStateAge:           0,
		},
		Metrics: Metrics{
			PercentAsRatio:    true,
			TextSensors:       true,
			LightChannels:     false,
			MaxTextValues:     10,
			MaxTextValueBytes: 64,
		},
	}
}
