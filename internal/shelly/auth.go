package shelly

import (
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/icholy/digest"

	"github.com/suprememoocow/espressif-exporter/internal/config"
)

// Credential is a resolved username and password for one device.
type Credential struct {
	Username string
	Password config.Secret
}

// IsSet reports whether a usable credential exists.
func (c Credential) IsSet() bool { return c.Password.IsSet() }

// deviceMatch is what an override is evaluated against.
type deviceMatch struct {
	DeviceID string
	MAC      string
	Hostname string
	Gen      int
}

// authResolver applies the global credential and its overrides.
type authResolver struct {
	global    Credential
	overrides []config.ShellyAuthOverride
	log       *slog.Logger

	warnOnce sync.Map // device ID -> struct{}, so a username warning is logged once
}

func newAuthResolver(cfg config.ShellyAuth, log *slog.Logger) *authResolver {
	return &authResolver{
		global:    Credential{Username: cfg.Username, Password: cfg.Password},
		overrides: cfg.Overrides,
		log:       log,
	}
}

// resolve returns the credential for a device.
//
// Overrides are evaluated top to bottom and the first match wins, rather than being
// merged across every match. First-match is far easier to reason about and matches the
// intuition people already have from relabel_configs and firewall rules. Within the
// winning override, unset fields inherit from the global credential, so overriding just
// a password for one device does not require restating the username.
func (a *authResolver) resolve(d deviceMatch) Credential {
	cred := a.global

	for _, o := range a.overrides {
		if !matches(o.Match, d) {
			continue
		}
		if o.Username != "" {
			cred.Username = o.Username
		}
		if o.Password.IsSet() {
			cred.Password = o.Password
		}
		break
	}

	// Gen2+ digest realms ignore any username other than admin. Silently failing auth
	// because of a config typo is a bad afternoon, so coerce it and say so once.
	if d.Gen >= 2 && cred.Username != gen2Username {
		if _, seen := a.warnOnce.LoadOrStore(d.DeviceID, struct{}{}); !seen {
			a.log.Warn("Shelly Gen2+ only accepts the username \"admin\"; overriding",
				"device", d.DeviceID, "configured_username", cred.Username)
		}
		cred.Username = gen2Username
	}
	return cred
}

// gen2Username is the only username Gen2+ firmware accepts.
const gen2Username = "admin"

func matches(m config.ShellyAuthMatch, d deviceMatch) bool {
	if m.DeviceID != "" && !strings.EqualFold(m.DeviceID, d.DeviceID) {
		return false
	}
	if m.MAC != "" && !strings.EqualFold(normaliseMAC(m.MAC), d.MAC) {
		return false
	}
	if m.Hostname != "" {
		ok, err := path.Match(strings.ToLower(m.Hostname), strings.ToLower(d.Hostname))
		if err != nil || !ok {
			return false
		}
	}
	if m.Gen != 0 && m.Gen != d.Gen {
		return false
	}
	return true
}

// basicAuthTransport sends HTTP basic credentials pre-emptively.
//
// Gen1 firmware has no digest support, and pre-empting the challenge saves a round trip
// per scrape on hardware where a round trip is expensive.
type basicAuthTransport struct {
	username string
	password string
	base     http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.SetBasicAuth(t.username, t.password)
	return t.base.RoundTrip(clone)
}

// newAuthTransport wraps the shared base transport with whatever the device needs.
//
// Digest is delegated to github.com/icholy/digest rather than hand-rolled. Shelly Gen2+
// uses SHA-256, and the parts of RFC 7616 that are easy to get subtly wrong — nonce
// count monotonicity, cnonce generation, stale=true re-challenge, A1/A2 construction —
// are exactly the parts that fail intermittently rather than loudly. The library also
// caches the challenge per host, so the extra 401 is paid once per device per process
// rather than on every scrape.
func newAuthTransport(gen int, cred Credential, base http.RoundTripper) http.RoundTripper {
	if !cred.IsSet() {
		return base
	}
	if gen >= 2 {
		return &digest.Transport{
			Username:  gen2Username,
			Password:  cred.Password.Reveal(),
			Transport: base,
		}
	}
	return &basicAuthTransport{
		username: cred.Username,
		password: cred.Password.Reveal(),
		base:     base,
	}
}
