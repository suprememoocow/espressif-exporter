package esphome

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/richard87/esphome-apiclient/pb"
	"google.golang.org/protobuf/proto"

	"github.com/suprememoocow/espressif-exporter/internal/config"
)

// transportMode records how a device is spoken to, once decided.
type transportMode string

const (
	transportUnknown   transportMode = ""
	transportNoise     transportMode = "noise"
	transportPlaintext transportMode = "plaintext"
)

// device owns exactly one connection to one ESPHome node.
//
// Sole ownership is a correctness requirement, not hygiene: an ESP8266 accepts only four
// concurrent API clients and an ESP32 five, with Home Assistant usually holding one. The
// Conn is created and closed inside run and never escapes, so no other goroutine can
// hold a reference and leak a socket into one of those slots.
type device struct {
	id   string
	name string
	cfg  config.ESPHome
	log  *slog.Logger

	dial  Dialer
	cache *deviceCache
	clock func() time.Time

	addr  netip.Addr
	port  uint16
	epoch uint64

	// transport is sticky for the process once negotiated, so a device that fell back
	// to plaintext does not re-attempt a doomed handshake on every reconnect.
	transport transportMode

	exited chan struct{}
}

func newDevice(id, name string, addr netip.Addr, port uint16, epoch uint64,
	cfg config.ESPHome, dial Dialer, log *slog.Logger) *device {
	return &device{
		id: id, name: name, cfg: cfg, dial: dial, addr: addr, port: port, epoch: epoch,
		log:    log.With("device", id, "addr", addr.String()),
		cache:  newDeviceCache(cfg.MaxOrphanStates),
		clock:  time.Now,
		exited: make(chan struct{}),
	}
}

// run drives the connection for the device's lifetime.
func (d *device) run(ctx context.Context) {
	defer close(d.exited)

	attempt := 0
	for ctx.Err() == nil {
		start := d.clock()
		cleanDisconnect, err := d.session(ctx)

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.log.Debug("session ended", "error", err)
		}
		d.cache.setConnected(false, "", d.clock())

		// A session that stayed up resets the ladder; without this a device flapping
		// every thirty seconds climbs to the cap and stays there once it recovers.
		if d.clock().Sub(start) > time.Minute {
			attempt = 0
		}

		delay := d.backoff(attempt, cleanDisconnect, err)
		attempt++

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// session is one full connect-to-disconnect cycle.
func (d *device) session(ctx context.Context) (cleanDisconnect bool, err error) {
	connectCtx, cancel := context.WithTimeout(ctx, d.cfg.ConnectBudget)
	defer cancel()

	d.cache.recordAttempt()

	conn, mode, err := d.connect(connectCtx)
	if err != nil {
		return false, err
	}
	// The socket is released here and nowhere else, so it cannot be leaked into one of
	// the device's scarce API slots.
	defer func() { _ = conn.Close() }()

	d.transport = mode

	info, err := conn.DeviceInfo()
	if err != nil {
		d.cache.recordFailure("device_info", err.Error())
		return false, fmt.Errorf("device info: %w", err)
	}
	d.cache.setInfo(info)

	if err := d.enumerate(conn); err != nil {
		d.cache.recordFailure("list_entities", err.Error())
		if d.cfg.Password.Reveal() == "" {
			// ESPHome before 2026.1.0 answers Hello and DeviceInfo without
			// authentication, then closes the connection on the first real request.
			// A node with encryption enabled refuses plaintext outright instead, so
			// reaching this point over plaintext identifies a password specifically.
			d.log.Warn("node answered DeviceInfo over plaintext then closed the "+
				"connection during ListEntities, which means it requires an API password",
				"node_name", info.GetName(),
				"esphome_version", info.GetEsphomeVersion(),
				"hint", "set esphome.password, or migrate the node to an encryption key")
		}
		return false, err
	}

	unsub, err := conn.SubscribeStates(func(msg proto.Message) {
		if key, state, ok := stateFromMessage(msg, d.clock()); ok {
			d.cache.putState(key, state)
		}
	})
	if err != nil {
		d.cache.recordFailure("subscribe", err.Error())
		return false, fmt.Errorf("subscribe states: %w", err)
	}
	defer unsub()

	// The device sends this on reboot, OTA or shutdown. Treating it as a clean
	// disconnect means we retry promptly instead of climbing the backoff ladder for
	// something the device politely warned us about.
	disconnected := make(chan struct{})
	stopWatch := conn.On(msgDisconnectRequest, func(proto.Message) {
		select {
		case <-disconnected:
		default:
			close(disconnected)
		}
	})
	defer stopWatch()

	d.cache.setConnected(true, string(mode), d.clock())
	d.log.Info("connected", "transport", mode,
		"esphome_version", info.GetEsphomeVersion(), "model", info.GetModel())

	return d.pump(ctx, conn, disconnected, info)
}

// pump keeps the connection alive and watches for it dying.
func (d *device) pump(
	ctx context.Context, conn Conn, disconnected <-chan struct{}, info *pb.DeviceInfoResponse,
) (bool, error) {
	ping := time.NewTicker(d.cfg.PingInterval)
	defer ping.Stop()
	refresh := time.NewTicker(d.cfg.RefreshInterval)
	defer refresh.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return true, nil

		case <-conn.Done():
			return false, errors.New("connection closed by peer")

		case <-disconnected:
			return true, nil

		case <-ping.C:
			start := d.clock()
			if err := conn.Ping(); err != nil {
				failures++
				d.log.Debug("ping failed", "consecutive", failures, "error", err)
				if failures >= d.cfg.PingFailThreshold {
					return false, fmt.Errorf("device stopped answering pings: %w", err)
				}
				continue
			}
			failures = 0
			rtt := d.clock().Sub(start)
			d.cache.setPing(rtt.Seconds())
			d.cache.touch(d.clock())

			// TCP alone takes ten to fifteen minutes to notice a powered-off ESP, so a
			// stalled stream is detected here rather than by the socket.
			if last := d.cache.Snapshot().LastMessageAt; !last.IsZero() &&
				d.clock().Sub(last) > d.cfg.ReadDeadline {
				return false, errors.New("no traffic within the read deadline")
			}

		case <-refresh.C:
			// ESPHome has no "entities changed" notification. A reconnect re-enumerates
			// anyway; this catches an OTA that somehow did not drop the connection.
			fresh, err := conn.DeviceInfo()
			if err != nil {
				continue
			}
			if fresh.GetCompilationTime() != info.GetCompilationTime() {
				d.log.Info("firmware changed; re-enumerating entities",
					"from", info.GetCompilationTime(), "to", fresh.GetCompilationTime())
				info = fresh
				d.cache.setInfo(fresh)
				if err := d.enumerate(conn); err != nil {
					return false, err
				}
			}
		}
	}
}

// connect dials, negotiating the transport.
//
// The mDNS api_encryption TXT key is absent on older firmware, so encryption cannot be
// determined ahead of time. With a key configured we try Noise and fall back to
// plaintext exactly once, making the result sticky; with require_encryption set we never
// fall back, and a handshake failure goes straight to the backoff cap rather than
// hammering a device with a wrong key every second.
func (d *device) connect(ctx context.Context) (Conn, transportMode, error) {
	addr := net.JoinHostPort(d.addr.String(), strconv.Itoa(int(d.port)))
	key := d.cfg.EncryptionKey.Reveal()

	attempts := make([]transportMode, 0, 2)
	switch {
	case d.transport != transportUnknown:
		attempts = append(attempts, d.transport)
	case key != "":
		attempts = append(attempts, transportNoise)
		if !d.cfg.RequireEncryption {
			attempts = append(attempts, transportPlaintext)
		}
	default:
		attempts = append(attempts, transportPlaintext)
	}

	var lastErr error
	for _, mode := range attempts {
		opts := DialOptions{
			ClientInfo: "espressif-exporter",
			Timeout:    d.cfg.DialTimeout,
		}
		if mode == transportNoise {
			opts.EncryptionKey = key
			opts.ExpectedName = d.name
		}

		conn, err := d.dial(ctx, addr, opts)
		if err == nil {
			if authErr := d.authenticate(conn); authErr != nil {
				_ = conn.Close()
				d.cache.recordFailure("auth", authErr.Error())
				return nil, mode, authErr
			}
			if mode != d.transport && d.transport != transportUnknown {
				d.log.Warn("transport changed", "from", d.transport, "to", mode)
			}
			return conn, mode, nil
		}

		lastErr = err
		if mode == transportNoise && !d.cfg.RequireEncryption {
			// Expected, not alarming: a node with no encryption configured resets the
			// connection when it receives a Noise preamble. Since the mDNS record does
			// not reliably say whether a node is encrypted, trying and falling back is
			// the only way to find out.
			d.log.Info("node did not accept the encrypted handshake; using plaintext",
				"error", err)
		}
	}

	d.cache.recordFailure(failureReason(lastErr), lastErr.Error())
	return nil, transportUnknown, lastErr
}

// authenticate performs the legacy password handshake when one is configured.
//
// ESPHome deprecated API passwords in 2026.1.0 and the client library does not implement
// them, but nodes on older firmware are still out there and simply drop the connection
// after DeviceInfo without it — a failure that is otherwise very hard to attribute.
func (d *device) authenticate(conn Conn) error {
	password := d.cfg.Password.Reveal()
	if password == "" {
		return nil
	}

	result := make(chan bool, 1)
	// AuthenticationRequest/Response are marked deprecated in api.proto because ESPHome
	// removed password auth in 2026.1.0. They are used here deliberately: nodes on older
	// firmware are still on real networks, and without this they connect, answer
	// DeviceInfo and then silently drop — a failure that is very hard to attribute.
	//nolint:staticcheck // SA1019: deliberate legacy-firmware support
	stop := conn.On(msgAuthenticationResponse, func(msg proto.Message) {
		resp, ok := msg.(*pb.AuthenticationResponse)
		select {
		case result <- ok && !resp.GetInvalidPassword():
		default:
		}
	})
	defer stop()

	//nolint:staticcheck // SA1019: see above
	if err := conn.SendMessage(&pb.AuthenticationRequest{Password: password},
		msgAuthenticationRequest); err != nil {
		return fmt.Errorf("sending legacy password: %w", err)
	}

	select {
	case ok := <-result:
		if !ok {
			return errors.New("device rejected the configured API password")
		}
		return nil
	case <-time.After(d.cfg.HandshakeTimeout):
		return errors.New("device did not answer the legacy password handshake")
	}
}

// enumerate reads the entity list and swaps it in atomically.
func (d *device) enumerate(conn Conn) error {
	messages, err := conn.ListEntities()
	if err != nil {
		return fmt.Errorf("list entities: %w", err)
	}

	records := make([]*entityRecord, 0, len(messages))
	for _, msg := range messages {
		if rec := entityFromMessage(msg); rec != nil {
			records = append(records, rec)
		}
	}

	// Sorted by key so truncation is deterministic across reconnects rather than
	// map-order roulette, which would make the exported set flap.
	truncated := false
	if len(records) > d.cfg.MaxEntitiesPerDevice {
		sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
		records = records[:d.cfg.MaxEntitiesPerDevice]
		truncated = true
		d.log.Warn("entity list truncated",
			"limit", d.cfg.MaxEntitiesPerDevice, "reported", len(messages))
	}

	next := make(map[uint32]*entityRecord, len(records))
	for _, rec := range records {
		next[rec.Key] = rec
	}
	d.cache.swapEntities(next, truncated)
	return nil
}

// backoff returns the delay before the next attempt.
func (d *device) backoff(attempt int, cleanDisconnect bool, err error) time.Duration {
	// The device told us it was going away, so it will be back in seconds. Climbing the
	// ladder here would keep a node offline for minutes after every OTA.
	if cleanDisconnect {
		return jitter(5*time.Second, 0.2)
	}

	maxDelay := d.cfg.ConnectBudget
	if maxDelay < 5*time.Minute {
		maxDelay = 5 * time.Minute
	}
	// A wrong key or password will not start working, and retrying every second across
	// a hundred devices is a reconnect storm that makes the misconfiguration worse.
	if isAuthFailure(err) {
		return jitter(maxDelay, 0.2)
	}

	d0 := float64(time.Second) * math.Pow(2, float64(attempt))
	if d0 > float64(maxDelay) {
		d0 = float64(maxDelay)
	}
	return jitter(time.Duration(d0), 0.2)
}

// jitter applies equal jitter. Full jitter can return a near-zero delay at the top of
// the ladder, defeating the purpose of having climbed it.
//
// math/rand is correct here: this spreads reconnect timing, it is not a security
// boundary, and crypto/rand would be slower for no benefit.
func jitter(d time.Duration, fraction float64) time.Duration {
	f := float64(d)
	return time.Duration(f*(1-fraction) + 2*fraction*f*rand.Float64()) //nolint:gosec // G404: jitter, not a secret
}

func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "password") ||
		strings.Contains(s, "handshake") ||
		strings.Contains(s, "mac failure") ||
		strings.Contains(s, "preamble")
}

func failureReason(err error) string {
	switch {
	case err == nil:
		return "none"
	case isAuthFailure(err):
		return "encryption"
	case strings.Contains(err.Error(), "refused"):
		return "conn_refused"
	case strings.Contains(err.Error(), "timeout"), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "dial"
	}
}
