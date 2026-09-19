package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix namespaces environment overrides. Nesting uses a double underscore, so
// EE_SCRAPE__WORKERS maps to scrape.workers.
const EnvPrefix = "EE_"

const envNestDelim = "__"

// Load layers defaults, then an optional file, then the environment. Later layers win.
//
// koanf is used rather than viper: viper drags in afero, pflag and every format parser
// whether you want them or not, keeps config in package-level global state, and folds
// key case — which bites on maps keyed by device name, exactly what the Shelly auth
// overrides use.
func Load(path string) (Config, error) {
	k := koanf.New(".")

	def := Default()
	if err := k.Load(structs.Provider(def, "koanf"), nil); err != nil {
		return Config{}, fmt.Errorf("loading defaults: %w", err)
	}

	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return Config{}, fmt.Errorf("loading config file %s: %w", path, err)
		}
	}

	envProvider := env.Provider(EnvPrefix, ".", func(key string) string {
		key = strings.ToLower(strings.TrimPrefix(key, EnvPrefix))
		return strings.ReplaceAll(key, envNestDelim, ".")
	})
	if err := k.Load(envProvider, nil); err != nil {
		return Config{}, fmt.Errorf("loading environment: %w", err)
	}

	cfg := Default()
	unmarshalConf := koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result:           &cfg,
			WeaklyTypedInput: true,
			Squash:           true,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
				stringToSecretHookFunc(),
			),
		},
	}
	if err := k.UnmarshalWithConf("", &cfg, unmarshalConf); err != nil {
		return Config{}, fmt.Errorf("decoding config: %w", err)
	}

	if err := cfg.resolveSecrets(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// stringToSecretHookFunc lets a plain YAML string or env var decode into Secret, which
// mapstructure would otherwise reject because Secret is a named type.
func stringToSecretHookFunc() mapstructure.DecodeHookFuncType {
	secretType := reflect.TypeOf(Secret(""))
	return func(from, to reflect.Type, data any) (any, error) {
		if from.Kind() != reflect.String || to != secretType {
			return data, nil
		}
		s, _ := data.(string)
		return Secret(s), nil
	}
}

func (c *Config) resolveSecrets() error {
	var err error

	if c.Shelly.Auth.Password, err = resolveSecret(
		"shelly.auth.password", c.Shelly.Auth.Password, c.Shelly.Auth.PasswordFile); err != nil {
		return err
	}
	for i := range c.Shelly.Auth.Overrides {
		o := &c.Shelly.Auth.Overrides[i]
		name := fmt.Sprintf("shelly.auth.overrides[%d].password", i)
		if o.Password, err = resolveSecret(name, o.Password, o.PasswordFile); err != nil {
			return err
		}
	}
	if c.ESPHome.EncryptionKey, err = resolveSecret(
		"esphome.encryption_key", c.ESPHome.EncryptionKey, c.ESPHome.EncryptionKeyFile); err != nil {
		return err
	}
	if c.ESPHome.Password, err = resolveSecret(
		"esphome.password", c.ESPHome.Password, c.ESPHome.PasswordFile); err != nil {
		return err
	}
	return nil
}

// Validate reports every problem at once rather than the first, so a bad config is one
// round trip to fix rather than several.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if _, err := parseLogLevel(c.Log.Level); err != nil {
		add("log.level: %w", err)
	}
	if f := strings.ToLower(c.Log.Format); f != "json" && f != "text" && f != "logfmt" {
		add("log.format: %q is not json or text", c.Log.Format)
	}
	if c.Server.Listen == "" {
		add("server.listen: must not be empty")
	}

	if len(c.Discovery.Sources) == 0 {
		add("discovery.sources: at least one of avahi, zeroconf, static is required")
	}
	seen := map[string]bool{}
	for _, s := range c.Discovery.Sources {
		switch s {
		case "avahi", "zeroconf", "static":
		default:
			add("discovery.sources: unknown source %q (want avahi, zeroconf or static)", s)
		}
		if seen[s] {
			add("discovery.sources: %q listed more than once", s)
		}
		seen[s] = true
	}
	if len(c.Discovery.ServiceTypes) == 0 {
		add("discovery.service_types: must not be empty")
	}
	for _, label := range []struct {
		name  string
		cidrs []string
	}{
		{"discovery.allow_cidrs", c.Discovery.AllowCIDRs},
		{"discovery.deny_cidrs", c.Discovery.DenyCIDRs},
	} {
		for _, s := range label.cidrs {
			if _, err := netip.ParsePrefix(s); err != nil {
				add("%s: %q is not a CIDR: %w", label.name, s, err)
			}
		}
	}
	for i, s := range c.Discovery.Static {
		if s.ID == "" {
			add("discovery.static[%d].id: must not be empty", i)
		}
		if s.Kind != "esphome" && s.Kind != "shelly" {
			add("discovery.static[%d].kind: %q is not esphome or shelly", i, s.Kind)
		}
		if _, err := netip.ParseAddr(s.Address); err != nil {
			add("discovery.static[%d].address: %q is not an IP address: %w", i, s.Address, err)
		}
	}

	if c.Probe.MaxConcurrent < 1 {
		add("probe.max_concurrent: must be at least 1")
	}
	if c.Probe.CacheTTL > 5*time.Second {
		add("probe.cache_ttl: %s is too long; it would serve stale data to a faster scraper",
			c.Probe.CacheTTL)
	}
	if c.Probe.MinTimeout > c.Probe.MaxTimeout {
		add("probe.min_timeout (%s) exceeds probe.max_timeout (%s)",
			c.Probe.MinTimeout, c.Probe.MaxTimeout)
	}
	if c.Probe.Backoff.Factor <= 1 {
		add("probe.backoff.factor: must be greater than 1")
	}
	if j := c.Probe.Backoff.Jitter; j < 0 || j >= 1 {
		add("probe.backoff.jitter: must be in [0, 1)")
	}

	if c.Registry.DeviceTTL < c.Registry.EndpointTTL {
		add("registry.device_ttl (%s) must be at least registry.endpoint_ttl (%s)",
			c.Registry.DeviceTTL, c.Registry.EndpointTTL)
	}

	// A password with no username fails as a 401 at scrape time, which is much harder to
	// diagnose than a startup error.
	if c.Shelly.Auth.Password.IsSet() && c.Shelly.Auth.Username == "" {
		add("shelly.auth: password is set but username is empty")
	}
	for i, o := range c.Shelly.Auth.Overrides {
		if o.Match == (ShellyAuthMatch{}) {
			add("shelly.auth.overrides[%d].match: must constrain at least one of "+
				"device_id, mac, hostname or gen", i)
		}
		if o.Match.Gen != 0 && o.Match.Gen != 1 && o.Match.Gen != 2 && o.Match.Gen != 3 {
			add("shelly.auth.overrides[%d].match.gen: %d is not a known generation", i, o.Match.Gen)
		}
	}

	if c.ESPHome.RequireEncryption && !c.ESPHome.EncryptionKey.IsSet() {
		add("esphome.require_encryption is set but esphome.encryption_key is empty")
	}
	if c.ESPHome.PingTimeout >= c.ESPHome.ReadDeadline {
		add("esphome.ping_timeout (%s) must be below esphome.read_deadline (%s)",
			c.ESPHome.PingTimeout, c.ESPHome.ReadDeadline)
	}

	return errors.Join(errs...)
}

// parseLogLevel mirrors logging.ParseLevel without importing it, keeping config free of
// a dependency on the logger it configures.
func parseLogLevel(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "info", "warn", "warning", "error", "":
		return s, nil
	default:
		return "", fmt.Errorf("%q is not debug, info, warn or error", s)
	}
}

// MustExist returns an error if path is set but unreadable, so a typo in --config is a
// startup failure rather than a silent fallback to defaults.
func MustExist(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	return nil
}
