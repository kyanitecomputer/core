// SPDX-License-Identifier: BSD-3-Clause

// Package bus is the Kyanite client-side messaging layer over the embedded NATS
// server (see core/natscore for the server). It gives supervise children a
// small, auth-aware handle for the three roles NATS plays on a node — in-process
// IPC between local actors, the node-to-node data plane, and the human/API
// access plane — with the connection conventions the auth plane requires baked
// in:
//
//   - per-boot NKey authentication for LOCAL actors over the in-process
//     transport (no persisted secrets; see the auth architecture §7);
//   - a per-principal inbox prefix so no principal can read another's replies
//     (never a shared _INBOX.>; see §10);
//   - session-token authentication for OPER principals over the network
//     transport.
//
// Configuration follows the project discipline: a default-struct plus Normalize,
// storage owned by the caller, no functional options in the exported surface.
// The microservice ergonomics (Service/Endpoint) and JetStream helpers build on
// this connection in later files.
package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/nats-io/nuid"
)

// Errors reported by [Connect] for malformed options.
var (
	// ErrNoTransport is returned when neither URL nor InProcess is set.
	ErrNoTransport = errors.New("bus: no transport configured (set URL or InProcess)")
	// ErrBothTransports is returned when both URL and InProcess are set.
	ErrBothTransports = errors.New("bus: URL and InProcess are mutually exclusive")
)

// Options configures a [Conn]. The zero value is not usable directly: exactly
// one transport (URL or InProcess) must be set. Call [Options.Normalize] to fill
// defaults; [Connect] does this for you.
type Options struct {
	// Name identifies the connection in server monitoring (CONNZ) and the audit
	// stream. Defaults to "kyanite".
	Name string

	// InboxPrefix isolates this principal's request replies. It must be unique
	// per principal; the auth plane sets a principal-scoped value. When empty, a
	// unique prefix is generated so replies are never shared by default.
	InboxPrefix string

	// URL is the network transport target (e.g. a websocket or TCP URL) for OPER
	// and MESH principals. Mutually exclusive with InProcess.
	URL string

	// InProcess is the in-process transport for LOCAL actors, satisfied by the
	// embedded *server.Server. Mutually exclusive with URL.
	InProcess nats.InProcessConnProvider

	// NKeySeed is an Ed25519 NKey seed used to authenticate as a LOCAL actor.
	// The seed is used only to sign the server nonce and is not retained beyond
	// the connection's signer closure.
	NKeySeed []byte

	// Token is a session token presented as connect_opts.token for OPER
	// principals (validated by the auth callout).
	Token string

	// ReconnectWait is the delay between reconnect attempts. Defaults to 2s.
	ReconnectWait time.Duration
	// MaxReconnects caps reconnect attempts; 0 selects the device default of
	// unlimited, matching a permanent on-node service.
	MaxReconnects int
	// Timeout bounds the initial connect (and each dial). Defaults to 5s.
	Timeout time.Duration
}

// Normalize fills unset fields with defaults. It is idempotent.
func (o *Options) Normalize() {
	if o.Name == "" {
		o.Name = "kyanite"
	}
	if o.InboxPrefix == "" {
		o.InboxPrefix = "_INBOX_" + nuid.Next()
	}
	if o.ReconnectWait <= 0 {
		o.ReconnectWait = 2 * time.Second
	}
	if o.MaxReconnects == 0 {
		o.MaxReconnects = -1 // unlimited
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
}

// Conn is a managed NATS connection for one Kyanite principal. It is safe for
// concurrent use (the underlying *nats.Conn is). Close (or Drain) it when done.
type Conn struct {
	nc *nats.Conn
}

// Connect opens a connection using opts. Exactly one transport must be set.
func Connect(opts Options) (*Conn, error) {
	switch {
	case opts.URL == "" && opts.InProcess == nil:
		return nil, ErrNoTransport
	case opts.URL != "" && opts.InProcess != nil:
		return nil, ErrBothTransports
	}
	opts.Normalize()

	natsOpts := []nats.Option{
		nats.Name(opts.Name),
		nats.CustomInboxPrefix(opts.InboxPrefix),
		nats.ReconnectWait(opts.ReconnectWait),
		nats.MaxReconnects(opts.MaxReconnects),
		nats.Timeout(opts.Timeout),
	}
	if opts.InProcess != nil {
		natsOpts = append(natsOpts, nats.InProcessServer(opts.InProcess))
	}
	if len(opts.NKeySeed) > 0 {
		kp, err := nkeys.FromSeed(opts.NKeySeed)
		if err != nil {
			return nil, fmt.Errorf("bus: parse nkey seed: %w", err)
		}
		pub, err := kp.PublicKey()
		if err != nil {
			return nil, fmt.Errorf("bus: derive nkey public key: %w", err)
		}
		natsOpts = append(natsOpts, nats.Nkey(pub, kp.Sign))
	}
	if opts.Token != "" {
		natsOpts = append(natsOpts, nats.Token(opts.Token))
	}

	// The in-process transport ignores the URL; nats requires the argument to
	// be empty in that case.
	url := opts.URL
	nc, err := nats.Connect(url, natsOpts...)
	if err != nil {
		return nil, fmt.Errorf("bus: connect: %w", err)
	}
	return &Conn{nc: nc}, nil
}

// NATS returns the underlying connection for advanced use (JetStream setup,
// low-level subscriptions). Prefer the wrapper methods where they suffice.
func (c *Conn) NATS() *nats.Conn { return c.nc }

// Publish sends data on subject with no expectation of a reply.
func (c *Conn) Publish(subject string, data []byte) error {
	return c.nc.Publish(subject, data)
}

// Request sends data on subject and waits for a single reply, bounded by ctx.
func (c *Conn) Request(ctx context.Context, subject string, data []byte) (*nats.Msg, error) {
	return c.nc.RequestWithContext(ctx, subject, data)
}

// Subscribe delivers messages on subject to h asynchronously.
func (c *Conn) Subscribe(subject string, h func(*nats.Msg)) (*nats.Subscription, error) {
	return c.nc.Subscribe(subject, h)
}

// QueueSubscribe is Subscribe with load-balancing across a queue group.
func (c *Conn) QueueSubscribe(subject, queue string, h func(*nats.Msg)) (*nats.Subscription, error) {
	return c.nc.QueueSubscribe(subject, queue, h)
}

// JetStream returns a JetStream context for durable streams/consumers and KV.
func (c *Conn) JetStream() (nats.JetStreamContext, error) {
	return c.nc.JetStream()
}

// Connected reports whether the connection is currently established.
func (c *Conn) Connected() bool { return c.nc.IsConnected() }

// Drain unsubscribes and flushes in-flight messages, then closes. Prefer it over
// Close for a graceful shutdown of a service.
func (c *Conn) Drain() error { return c.nc.Drain() }

// Close tears down the connection immediately.
func (c *Conn) Close() { c.nc.Close() }
