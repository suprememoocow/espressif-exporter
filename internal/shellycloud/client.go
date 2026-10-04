// Package shellycloud reads a Shelly Cloud account: which devices it owns, where each one
// last reported itself on the LAN, and what the operator named each device, channel and
// room in the Shelly app.
//
// Three endpoints are used. /device/all_status is documented (Real Time Events, with a
// bearer token) and accepts the account auth_key as well. /interface/device/list and
// /interface/room/list are what the Shelly app itself calls; they are undocumented but
// have been stable for years and are what pyShelly and ShellyForHASS rely on. Every
// decoder here is therefore shape-tolerant: a missing or retyped field drops that one
// value, never the whole response.
package shellycloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MinRequestGap is the pause between the end of one request and the start of the next.
// Shelly documents one request per second per account, but its limiter counts a request
// that overlaps another, or follows the previous response too closely, against the limit:
// an exporter spacing request starts 1.1s apart was refused with max_req. pyShelly
// settled on 2s for the same reason.
const MinRequestGap = 2 * time.Second

// rateLimitPause is how long to back off after the cloud answers max_req, and
// rateLimitRetries how many times to try again before giving up. The limit may be shared
// with the Shelly app and other integrations on the same account, so one refusal is not
// proof that our own pacing is wrong.
const (
	rateLimitPause   = 10 * time.Second
	rateLimitRetries = 2
)

// maxBodyBytes bounds a response. all_status carries every device's full status, so it is
// far larger than any single device document: around 4 KiB per device.
const maxBodyBytes = 16 << 20

const redacted = "<secret>"

// Client calls the Shelly Cloud API for one account. It is safe for concurrent use, and
// runs one request at a time with a pause between them, so that every caller sharing it
// stays within the account's rate limit together.
type Client struct {
	base  *url.URL
	key   string
	http  *http.Client
	gap   time.Duration
	pause time.Duration

	// slot admits one request at a time. Its holder alone reads and writes notBefore.
	slot      chan struct{}
	notBefore time.Time
}

// NewClient builds a client for the server shown beside the key in the Shelly app.
func NewClient(server, authKey string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil {
		return nil, fmt.Errorf("shelly cloud server %q: %w", server, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("shelly cloud server %q: want a URL such as https://shelly-58-eu.shelly.cloud", server)
	}
	if authKey == "" {
		return nil, errors.New("shelly cloud: auth key is empty")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		base:  u,
		key:   authKey,
		http:  &http.Client{Timeout: timeout},
		gap:   MinRequestGap,
		pause: rateLimitPause,
		slot:  make(chan struct{}, 1),
	}, nil
}

// ErrRateLimited reports that the cloud refused a request with max_req, and kept refusing
// after the client backed off and retried.
var ErrRateLimited = errors.New("shelly cloud: request limit reached")

// APIError is a well-formed response with isok=false, for example a revoked key.
type APIError struct {
	Path   string
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("shelly cloud %s: request refused: %s", e.Path, e.Detail)
}

// envelope is the wrapper every endpoint returns.
type envelope struct {
	IsOK   bool            `json:"isok"`
	Data   json.RawMessage `json:"data"`
	Errors json.RawMessage `json:"errors"`
}

// post calls one endpoint and returns its data member, backing off and retrying when the
// account's request limit is reached.
func (c *Client) post(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		if err := c.acquire(ctx); err != nil {
			return nil, err
		}
		data, err := c.do(ctx, path, params)
		limited := errors.Is(err, ErrRateLimited)
		if limited {
			c.release(c.pause)
		} else {
			c.release(c.gap)
		}
		if !limited || attempt >= rateLimitRetries {
			return data, err
		}
	}
}

// acquire waits for the request slot, then for the pause after the previous request.
func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	d := time.Until(c.notBefore)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		<-c.slot
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// release frees the slot, holding the next request off for pause from now. Measuring from
// the end of a request rather than its start is what keeps a slow response from leaving
// too little room before the next request.
func (c *Client) release(pause time.Duration) {
	c.notBefore = time.Now().Add(pause)
	<-c.slot
}

// do performs one request.
//
// The key travels in the form body rather than the URL, so it cannot leak through a
// *url.Error, an access log or a proxy. Errors are passed through redact all the same,
// because a server that echoes the request back would otherwise put it in our logs.
func (c *Client) do(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {

	form := url.Values{}
	for k, v := range params {
		form[k] = v
	}
	form.Set("auth_key", c.key)

	u := c.base.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, c.redact(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.redact(fmt.Errorf("shelly cloud %s: %w", path, err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, c.redact(fmt.Errorf("shelly cloud %s: reading response: %w", path, err))
	}
	// The limit arrives as HTTP 401 or 429 with errors.max_req, so it is recognised by its
	// body rather than its status: treating it as an auth failure would hide that it clears
	// by itself.
	if isRateLimited(body) {
		return nil, fmt.Errorf("%w (%s)", ErrRateLimited, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.redact(fmt.Errorf("shelly cloud %s: HTTP %d: %s",
			path, resp.StatusCode, snippet(body)))
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, c.redact(fmt.Errorf("shelly cloud %s: decoding response: %w", path, err))
	}
	if !env.IsOK {
		return nil, c.redact(&APIError{Path: path, Detail: snippet(env.Errors)})
	}
	return env.Data, nil
}

// isRateLimited reports whether a response body is the cloud's max_req refusal.
func isRateLimited(body []byte) bool {
	var env struct {
		Errors map[string]any `json:"errors"`
	}
	if json.Unmarshal(body, &env) != nil {
		return false
	}
	_, ok := env.Errors["max_req"]
	return ok
}

// redact strips the auth key from an error's text.
func (c *Client) redact(err error) error {
	if err == nil || !strings.Contains(err.Error(), c.key) {
		return err
	}
	return redactedError{msg: strings.ReplaceAll(err.Error(), c.key, redacted), err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string { return e.msg }
func (e redactedError) Unwrap() error { return e.err }

// snippet renders a short, single-line excerpt of a response for an error message.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if s == "" || s == "null" {
		return "no detail"
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
