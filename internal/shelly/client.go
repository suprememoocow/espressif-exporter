package shelly

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/suprememoocow/espressif-exporter/internal/config"
)

var nonHex = regexp.MustCompile(`[^0-9a-f]`)

func normaliseMAC(s string) string {
	return nonHex.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "")
}

// newBaseTransport builds the single process-wide transport all devices share.
//
// Two settings carry most of the weight. MaxIdleConns must exceed the fleet size: the
// default of 100, combined with MaxIdleConnsPerHost of 2, means 120 distinct hosts evict
// each other continuously and most scrapes pay a fresh TCP handshake — and on flaky wifi
// the handshake is where the packet loss lives. IdleConnTimeout must exceed the scrape
// interval, or connections are reaped between scrapes and the pool sizing is moot.
func newBaseTransport(cfg config.Shelly) *http.Transport {
	return &http.Transport{
		Proxy: nil, // never proxy LAN devices
		DialContext: (&net.Dialer{
			Timeout:   cfg.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 2,
		// Gen1 runs a Mongoose server with a very small connection limit, and exceeding
		// it produces resets rather than queueing.
		MaxConnsPerHost:       2,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		// The ESP CPU is the bottleneck and payloads are a few kilobytes, so gzip costs
		// the device more than it saves on a LAN.
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
}

// clientKey identifies a cached client. Including the epoch is what invalidates a digest
// challenge bound to an address the device no longer has.
type clientKey struct {
	deviceID string
	epoch    uint64
}

// clientCache holds one authenticated client per device.
type clientCache struct {
	base         http.RoundTripper
	gen1Base     http.RoundTripper
	maxBodyBytes int64

	mu      sync.Mutex
	clients map[clientKey]*http.Client
}

func newClientCache(cfg config.Shelly) *clientCache {
	base := newBaseTransport(cfg)

	// Gen1 firmware handles keep-alive poorly enough that reusing connections causes
	// more failures than the handshakes it saves.
	gen1 := base.Clone()
	gen1.DisableKeepAlives = true
	gen1.MaxConnsPerHost = 1

	return &clientCache{
		base:         base,
		gen1Base:     gen1,
		maxBodyBytes: cfg.MaxBodyBytes,
		clients:      map[clientKey]*http.Client{},
	}
}

// get returns the client for a device, building it on first use.
func (c *clientCache) get(key clientKey, gen int, cred Credential) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()

	if cl, ok := c.clients[key]; ok {
		return cl
	}

	base := c.base
	if gen == 1 {
		base = c.gen1Base
	}
	cl := &http.Client{
		Transport: newAuthTransport(gen, cred, base),
		// Deliberately zero: every deadline comes from the request context, so /probe
		// controls the whole budget end to end.
		Timeout: 0,
	}
	c.clients[key] = cl

	// Drop any client for an earlier epoch of this device.
	for k := range c.clients {
		if k.deviceID == key.deviceID && k.epoch != key.epoch {
			delete(c.clients, k)
		}
	}
	return cl
}

// forget drops every client for a device.
func (c *clientCache) forget(deviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.clients {
		if k.deviceID == deviceID {
			delete(c.clients, k)
		}
	}
}

// getJSON performs one request and decodes the body into out.
func (c *clientCache) getJSON(
	ctx context.Context, cl *http.Client, addr netip.Addr, port uint16, path string, out any,
) error {
	url := "http://" + net.JoinHostPort(addr.String(), itoa(int(port))) + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		// Drain before closing so the connection is reusable; otherwise the carefully
		// sized pool above never actually gets reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.maxBodyBytes))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return &authError{code: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return &statusError{code: resp.StatusCode}
	}

	dec := json.NewDecoder(io.LimitReader(resp.Body, c.maxBodyBytes))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
