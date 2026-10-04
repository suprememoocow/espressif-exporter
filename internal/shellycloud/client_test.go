package shellycloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures are modelled on responses captured from a real account (Gen1, Gen2 and
// Gen3 relays, dimmers, RGBW2 and Shelly EM), with names, IDs and addresses changed.
const testKey = "MTIzNDU2Nzg5MGFiY2RlZg-secret"

type fakeCloud struct {
	t *testing.T

	mu       sync.Mutex
	requests []*http.Request
	forms    []url.Values
	bodies   map[string]string
	status   map[string]int
	// limited answers this many requests with max_req before serving normally.
	limited int
	// inFlight and overlapped detect concurrent requests, which the cloud counts against
	// the account's limit.
	inFlight   int
	overlapped bool
	delay      time.Duration
}

func newFakeCloud(t *testing.T) (*fakeCloud, *Client) {
	t.Helper()
	f := &fakeCloud{t: t, bodies: map[string]string{}, status: map[string]int{}}
	for path, file := range map[string]string{
		"/device/all_status":     "all_status.json",
		"/interface/device/list": "device_list.json",
		"/interface/room/list":   "room_list.json",
	} {
		f.bodies[path] = fixture(t, file)
	}
	ts := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(ts.Close)

	c, err := NewClient(ts.URL+"/", testKey, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.gap, c.pause = 0, 0
	return f, c
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parsing form: %v", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.forms = append(f.forms, r.PostForm)
	body, ok := f.bodies[r.URL.Path]
	code := f.status[r.URL.Path]
	f.inFlight++
	if f.inFlight > 1 {
		f.overlapped = true
	}
	limited := f.limited > 0
	if limited {
		f.limited--
	}
	delay := f.delay
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	time.Sleep(delay)

	if limited {
		// As observed from the real service: HTTP 401 with errors.max_req.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"isok":false,"errors":{"max_req":"Request limit reached!"}}`))
		return
	}

	if r.PostForm.Get("auth_key") != testKey {
		_, _ = w.Write([]byte(`{"isok":false,"errors":{"wrong_auth_key":"bad key"}}`))
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if code != 0 {
		w.WriteHeader(code)
	}
	_, _ = w.Write([]byte(body))
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The key goes in the POST body, never the URL, so it cannot surface in a *url.Error or a
// proxy's access log.
func TestKeyTravelsInTheBodyNotTheURL(t *testing.T) {
	f, c := newFakeCloud(t)
	if _, err := c.AllStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := f.requests[0]
	if r.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.Method)
	}
	if strings.Contains(r.URL.String(), testKey) {
		t.Errorf("auth key appears in the request URL %q", r.URL)
	}
	form := f.forms[0]
	if form.Get("show_info") != "true" || form.Get("no_shared") != "true" {
		t.Errorf("all_status form = %v, want show_info and no_shared", form)
	}
}

func TestAllStatusLocatesEveryGeneration(t *testing.T) {
	_, c := newFakeCloud(t)
	got, err := c.AllStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]DeviceStatus{}
	for _, d := range got {
		byID[d.ID] = d
	}
	// The status's own MAC is what lets an early Gen1 device, whose cloud id is only
	// the last three bytes, still derive a mac: identity.
	if got := byID["a1b2c3"].MAC; got != "5ccf7fa1b2c3" {
		t.Errorf("a1b2c3 MAC = %q, want 5ccf7fa1b2c3 from the status", got)
	}
	// An all-digit MAC arrives as a JSON number. Rejecting it once dropped the device.
	if got := byID["100200300400"].MAC; got != "100200300400" {
		t.Errorf("100200300400 MAC = %q, want it read from a JSON number", got)
	}
	if got := byID["a8032ab12345"].MAC; got != "a8032ab12345" {
		t.Errorf("a8032ab12345 MAC = %q, want it from sys.mac", got)
	}
	if _, ok := byID["broken0000ff"]; ok {
		t.Error("a malformed entry should be skipped, not reported")
	}

	for _, tc := range []struct {
		id     string
		gen    int
		online bool
		addr   string
	}{
		{"a8032ab1c2d3", 1, true, "192.168.1.61"},  // Gen1: wifi_sta.ip
		{"a8032ab12345", 2, true, "192.168.1.57"},  // Gen2: wifi.sta_ip; id lowercased; online as 1
		{"ecc9ff00aa11", 2, true, "10.20.0.14"},    // Pro on ethernet: wifi.sta_ip null, eth.ip set
		{"dc4f2276846a", 1, false, "192.168.1.40"}, // offline: last-known address still reported
		{"7c2e0d112233", 0, true, ""},              // BLU: no generation we can scrape, no address
		{"a1b2c3", 1, true, "192.168.1.18"},        // early Gen1: cloud id is the short MAC
		{"100200300400", 1, true, "192.168.1.45"},  // MAC sent as a JSON number; sys of the wrong shape
	} {
		d, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s missing from %+v", tc.id, got)
			continue
		}
		if d.Gen != tc.gen || d.Online != tc.online {
			t.Errorf("%s: gen=%d online=%v, want gen=%d online=%v", tc.id, d.Gen, d.Online, tc.gen, tc.online)
		}
		var want netip.Addr
		if tc.addr != "" {
			want = netip.MustParseAddr(tc.addr)
		}
		if d.Addr != want {
			t.Errorf("%s: addr = %v, want %v", tc.id, d.Addr, want)
		}
	}
}

// A refused request must say why without echoing the key, even if the server's error
// message contains it.
func TestRefusalIsAnAPIErrorWithTheKeyRedacted(t *testing.T) {
	f, c := newFakeCloud(t)
	f.bodies["/device/all_status"] = `{"isok":false,"errors":{"wrong_auth_key":"` + testKey + ` is revoked"}}`

	_, err := c.AllStatus(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error leaks the auth key: %v", err)
	}
	if !strings.Contains(err.Error(), "wrong_auth_key") {
		t.Errorf("error should carry the server's reason: %v", err)
	}
}

func TestHTTPErrorIsReported(t *testing.T) {
	f, c := newFakeCloud(t)
	f.status["/device/all_status"] = http.StatusTooManyRequests
	f.bodies["/device/all_status"] = `{"isok":false}`

	if _, err := c.AllStatus(context.Background()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v, want an HTTP 429 error", err)
	}
}

// Discovery and names share one client, so it is what keeps the two together within the
// account's limit. The cloud refuses a request that overlaps another, so requests must
// run one at a time, and the gap is measured from the end of the previous one: a slow
// response must not eat into it.
func TestRequestsRunOneAtATimeWithAGapAfterEach(t *testing.T) {
	f, c := newFakeCloud(t)
	c.gap = 50 * time.Millisecond
	f.delay = 30 * time.Millisecond

	start := time.Now()
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := c.AllStatus(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if len(f.requests) != 3 {
		t.Fatalf("got %d requests, want 3", len(f.requests))
	}
	if f.overlapped {
		t.Error("requests overlapped; the cloud counts that against the limit")
	}
	// 3 responses of 30ms plus 2 gaps of 50ms after them.
	if elapsed := time.Since(start); elapsed < 190*time.Millisecond {
		t.Errorf("3 requests took %s, want at least 190ms", elapsed)
	}
}

// The cloud answers max_req with HTTP 401. It clears by itself, so the client backs off
// and retries rather than reporting an auth failure.
func TestRateLimitIsRetriedAfterAPause(t *testing.T) {
	f, c := newFakeCloud(t)
	c.pause = 20 * time.Millisecond
	f.limited = 2

	if _, err := c.AllStatus(context.Background()); err != nil {
		t.Fatalf("AllStatus failed despite the limit clearing: %v", err)
	}
	if len(f.requests) != 3 {
		t.Errorf("got %d requests, want 3 (two refused, one served)", len(f.requests))
	}
}

func TestPersistentRateLimitIsReported(t *testing.T) {
	f, c := newFakeCloud(t)
	f.limited = 100

	_, err := c.AllStatus(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if len(f.requests) != 1+rateLimitRetries {
		t.Errorf("got %d requests, want %d", len(f.requests), 1+rateLimitRetries)
	}
}

func TestWaitHonoursCancellation(t *testing.T) {
	_, c := newFakeCloud(t)
	c.notBefore = time.Now().Add(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.AllStatus(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestNewClientRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ server, key string }{
		{"", testKey},
		{"shelly-58-eu.shelly.cloud", testKey}, // no scheme
		{"https://shelly-58-eu.shelly.cloud", ""},
	} {
		if _, err := NewClient(tc.server, tc.key, 0); err == nil {
			t.Errorf("NewClient(%q, %q) accepted bad input", tc.server, tc.key)
		}
	}
}

func TestTextAcceptsStringsAndNumbers(t *testing.T) {
	for in, want := range map[string]string{
		`"Kitchen"`:    "Kitchen",
		`100200300400`: "100200300400",
		`-10`:          "-10",
		`null`:         "",
		`{"a":1}`:      "",
		`[1,2]`:        "",
		`true`:         "",
	} {
		var got text
		if err := json.Unmarshal([]byte(in), &got); err != nil || string(got) != want {
			t.Errorf("text from %s = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestStatusMACRestoresLeadingZeros(t *testing.T) {
	if got := statusMAC("8221782cf"); got != "" {
		t.Errorf("statusMAC kept a non-numeric short value: %q", got)
	}
	if got := statusMAC("12345678901"); got != "012345678901" {
		t.Errorf("statusMAC(12345678901) = %q, want the leading zero restored", got)
	}
}
