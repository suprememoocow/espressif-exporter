package shelly

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/suprememoocow/espressif-exporter/internal/config"
)

func TestAuthResolverFirstMatchWins(t *testing.T) {
	cfg := config.ShellyAuth{
		Username: "admin",
		Password: "global",
		Overrides: []config.ShellyAuthOverride{
			{Match: config.ShellyAuthMatch{Hostname: "shelly1-*"}, Password: "legacy"},
			{Match: config.ShellyAuthMatch{DeviceID: "mac:a8032ab1c2d3"}, Password: "specific"},
			// Deliberately unreachable for shelly1-aabbcc: the first match already won.
			{Match: config.ShellyAuthMatch{Gen: 1}, Password: "never"},
		},
	}
	a := newAuthResolver(cfg, discardLogger())

	tests := []struct {
		name string
		dev  deviceMatch
		want string
	}{
		{"no override matches", deviceMatch{DeviceID: "mac:ffffffffffff", Gen: 2}, "global"},
		{"hostname glob", deviceMatch{DeviceID: "shortid:x", Hostname: "shelly1-aabbcc", Gen: 1}, "legacy"},
		{"device id", deviceMatch{DeviceID: "mac:a8032ab1c2d3", Gen: 2}, "specific"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.resolve(tt.dev).Password.Reveal(); got != tt.want {
				t.Errorf("password = %q, want %q", got, tt.want)
			}
		})
	}
}

// Unset fields in the winning override inherit from the global, so overriding only a
// password does not require restating the username.
func TestAuthResolverOverrideInheritsUnsetFields(t *testing.T) {
	a := newAuthResolver(config.ShellyAuth{
		Username: "houseuser",
		Password: "global",
		Overrides: []config.ShellyAuthOverride{
			{Match: config.ShellyAuthMatch{Gen: 1}, Password: "legacy"},
		},
	}, discardLogger())

	cred := a.resolve(deviceMatch{Gen: 1})
	if cred.Username != "houseuser" {
		t.Errorf("username = %q, want it inherited from the global credential", cred.Username)
	}
	if cred.Password.Reveal() != "legacy" {
		t.Errorf("password = %q, want the override", cred.Password.Reveal())
	}
}

// Gen2+ digest realms ignore any username but admin, so a config typo would otherwise
// fail authentication silently.
func TestAuthResolverCoercesGen2Username(t *testing.T) {
	a := newAuthResolver(config.ShellyAuth{Username: "wrong", Password: "p"}, discardLogger())

	if got := a.resolve(deviceMatch{Gen: 2}).Username; got != "admin" {
		t.Errorf("gen2 username = %q, want it coerced to admin", got)
	}
	if got := a.resolve(deviceMatch{Gen: 1}).Username; got != "wrong" {
		t.Errorf("gen1 username = %q, want it left alone", got)
	}
}

// A Secret must not leak through a log line, a config dump, or a debug endpoint.
func TestSecretRedaction(t *testing.T) {
	s := config.Secret("hunter2")
	for _, got := range []string{
		fmt.Sprintf("%v", s),
		s.String(),
		fmt.Sprintf("%#v", s),
	} {
		if strings.Contains(got, "hunter2") {
			t.Errorf("formatted secret leaked the value: %q", got)
		}
	}
	if s.Reveal() != "hunter2" {
		t.Error("Reveal must return the real value")
	}
}

// digestServer implements just enough RFC 7616 SHA-256 to exercise the client.
type digestServer struct {
	realm, nonce, username, password string
	challenges                       atomic.Int32
}

func (d *digestServer) handler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Digest ") || !d.valid(r, auth) {
			d.challenges.Add(1)
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Digest qop="auth", realm=%q, nonce=%q, algorithm=SHA-256`, d.realm, d.nonce))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func (d *digestServer) valid(r *http.Request, auth string) bool {
	p := parseDigest(strings.TrimPrefix(auth, "Digest "))
	if p["username"] != d.username || p["nonce"] != d.nonce {
		return false
	}
	h := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	a1 := h(d.username + ":" + d.realm + ":" + d.password)
	a2 := h(r.Method + ":" + p["uri"])
	want := h(strings.Join([]string{a1, p["nonce"], p["nc"], p["cnonce"], p["qop"], a2}, ":"))
	return p["response"] == want
}

func parseDigest(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[k] = strings.Trim(v, `"`)
	}
	return out
}

func randomNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Shelly Gen2 uses SHA-256 digest, not MD5. The parts of RFC 7616 that are easy to get
// wrong by hand fail intermittently rather than loudly, so this proves the wiring end to
// end before it ever meets real hardware — including that the challenge is reused, which
// is what keeps the extra 401 to once per device rather than once per scrape.
func TestDigestAuthSHA256AndChallengeReuse(t *testing.T) {
	srv := &digestServer{realm: "shelly", nonce: randomNonce(t), username: "admin", password: "s3cret"}
	ts := httptest.NewServer(srv.handler(`{"sys":{"uptime":42}}`))
	defer ts.Close()

	client := &http.Client{Transport: newAuthTransport(2,
		Credential{Username: "admin", Password: "s3cret"}, http.DefaultTransport)}

	u, _ := url.Parse(ts.URL + "/rpc/Shelly.GetStatus")
	for i := range 3 {
		resp, err := client.Get(u.String())
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, resp.StatusCode)
		}
	}

	if got := srv.challenges.Load(); got != 1 {
		t.Errorf("server issued %d challenges across 3 requests, want 1 (the challenge should be cached)", got)
	}
}

// A wrong password must be reported as auth, not as an unreachable device: the two are
// indistinguishable otherwise, and only one of them is fixed by waiting.
func TestWrongPasswordIsClassifiedAsAuth(t *testing.T) {
	srv := &digestServer{realm: "shelly", nonce: randomNonce(t), username: "admin", password: "correct"}
	ts := httptest.NewServer(srv.handler(`{}`))
	defer ts.Close()

	client := &http.Client{Transport: newAuthTransport(2,
		Credential{Username: "admin", Password: "wrong"}, http.DefaultTransport)}

	resp, err := client.Get(ts.URL + "/rpc/Shelly.GetStatus")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := classify(&authError{code: resp.StatusCode}); got != "auth" {
		t.Errorf("classify = %q, want auth", got)
	}
}

// Gen1 has no digest support, so credentials are pre-empted to save a round trip.
func TestGen1UsesPreemptiveBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var requests atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	client := &http.Client{Transport: newAuthTransport(1,
		Credential{Username: "houseuser", Password: "p"}, http.DefaultTransport)}
	resp, err := client.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if requests.Load() != 1 {
		t.Errorf("made %d requests, want 1 with credentials pre-empted", requests.Load())
	}
	if gotUser != "houseuser" || gotPass != "p" {
		t.Errorf("basic auth = %q/%q, want houseuser/p", gotUser, gotPass)
	}
}

// With no credential configured, no auth transport is layered on at all.
func TestNoCredentialMeansNoAuthTransport(t *testing.T) {
	base := http.DefaultTransport
	if got := newAuthTransport(2, Credential{Username: "admin"}, base); got != base {
		t.Error("an unset password should leave the base transport untouched")
	}
}
