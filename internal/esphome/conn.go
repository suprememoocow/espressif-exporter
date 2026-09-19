// Package esphome collects metrics from ESPHome devices over the native API.
//
// The native API is push-based: the exporter holds one connection per device, subscribes
// once, and receives state updates indefinitely. A probe therefore reads a cached
// snapshot and performs no network I/O at all, which is what keeps /probe fast for a
// hundred-plus devices and what makes freshness an explicit, exported quantity rather
// than an implicit property of scrape timing.
package esphome

import (
	"context"
	"time"

	api "github.com/richard87/esphome-apiclient"
	"google.golang.org/protobuf/proto"

	"github.com/richard87/esphome-apiclient/pb"
)

// Message type IDs from the native API. They are a custom protobuf option rather than
// part of the wire format, so they are restated here for the messages we send directly.
const (
	msgAuthenticationRequest  uint32 = 3
	msgAuthenticationResponse uint32 = 4
	msgDisconnectRequest      uint32 = 5
)

// Conn is the seam between this package and the upstream client library.
//
// The library is small and lightly maintained, so every call into it goes through this
// interface. Swapping to a fork, or to a hand-written client, becomes a single-file
// change rather than a rewrite — and it makes the whole connection state machine
// testable without a device.
type Conn interface {
	DeviceInfo() (*pb.DeviceInfoResponse, error)
	ListEntities() ([]proto.Message, error)
	SubscribeStates(handler func(proto.Message)) (func(), error)
	On(msgType uint32, handler func(proto.Message)) func()
	SendMessage(msg proto.Message, msgType uint32) error
	Ping() error
	Connected() bool
	Done() <-chan struct{}
	Close() error
}

// Dialer opens a connection to a device.
type Dialer func(ctx context.Context, addr string, opts DialOptions) (Conn, error)

// DialOptions configures one connection attempt.
type DialOptions struct {
	ClientInfo    string
	EncryptionKey string
	ExpectedName  string
	Timeout       time.Duration
}

// dialReal is the production Dialer.
func dialReal(ctx context.Context, addr string, opts DialOptions) (Conn, error) {
	clientOpts := []api.Option{
		api.WithClientInfo(opts.ClientInfo),
		// The library's own reconnect loop would fight ours, which needs backoff and
		// connection-slot accounting it cannot know about.
		api.WithReconnect(0),
	}
	if opts.EncryptionKey != "" {
		clientOpts = append(clientOpts, api.WithEncryptionKey(opts.EncryptionKey))
	}
	if opts.ExpectedName != "" {
		// Validates the name the device reports during the Noise handshake, so another
		// device sharing the fleet PSK cannot impersonate this one.
		clientOpts = append(clientOpts, api.WithExpectedName(opts.ExpectedName))
	}

	c, err := api.DialWithContext(ctx, addr, opts.Timeout, clientOpts...)
	if err != nil {
		return nil, err
	}
	return &realConn{c}, nil
}

// realConn adapts the upstream client to Conn.
type realConn struct{ c *api.Client }

func (r *realConn) DeviceInfo() (*pb.DeviceInfoResponse, error) { return r.c.DeviceInfo() }
func (r *realConn) ListEntities() ([]proto.Message, error)      { return r.c.ListEntities() }
func (r *realConn) Ping() error                                 { return r.c.Ping() }
func (r *realConn) Connected() bool                             { return r.c.Connected() }
func (r *realConn) Done() <-chan struct{}                       { return r.c.Done() }
func (r *realConn) Close() error                                { return r.c.Close() }

func (r *realConn) SubscribeStates(h func(proto.Message)) (func(), error) {
	return r.c.SubscribeStates(h)
}

func (r *realConn) On(msgType uint32, h func(proto.Message)) func() {
	return r.c.On(msgType, api.MessageHandler(h))
}

func (r *realConn) SendMessage(msg proto.Message, msgType uint32) error {
	return r.c.SendMessage(msg, msgType)
}
