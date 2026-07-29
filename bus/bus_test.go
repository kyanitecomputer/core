// SPDX-License-Identifier: BSD-3-Clause

package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"src.kyanite.computer/core/bus"
	"src.kyanite.computer/core/natscore"
)

func startServer(t *testing.T) *server.Server {
	t.Helper()
	opts := natscore.Options(t.TempDir())
	opts.Host = "127.0.0.1"
	opts.Port = -1
	opts.Websocket.Port = -1
	opts.NoLog = true
	srv, err := natscore.Start(opts)
	if err != nil {
		t.Fatalf("natscore.Start: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func TestOptionsNormalizeDefaults(t *testing.T) {
	var o bus.Options
	o.URL = "nats://x"
	o.Normalize()
	if o.Name == "" || o.InboxPrefix == "" {
		t.Fatalf("Normalize left name/inbox empty: %+v", o)
	}
	if o.MaxReconnects != -1 || o.ReconnectWait != 2*time.Second || o.Timeout != 5*time.Second {
		t.Fatalf("Normalize defaults wrong: %+v", o)
	}
}

func TestConnectTransportValidation(t *testing.T) {
	if _, err := bus.Connect(bus.Options{}); !errors.Is(err, bus.ErrNoTransport) {
		t.Fatalf("no transport err = %v, want ErrNoTransport", err)
	}
	srv := startServer(t)
	if _, err := bus.Connect(bus.Options{URL: srv.ClientURL(), InProcess: srv}); !errors.Is(err, bus.ErrBothTransports) {
		t.Fatalf("both transports err = %v, want ErrBothTransports", err)
	}
}

func TestConnectInProcessRequestReply(t *testing.T) {
	srv := startServer(t)
	c, err := bus.Connect(bus.Options{Name: "test-local", InProcess: srv})
	if err != nil {
		t.Fatalf("Connect in-process: %v", err)
	}
	t.Cleanup(c.Close)
	if !c.Connected() {
		t.Fatal("not connected")
	}

	sub, err := c.Subscribe("echo", func(m *nats.Msg) { _ = m.Respond(m.Data) })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := c.Request(ctx, "echo", []byte("ping"))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(resp.Data) != "ping" {
		t.Fatalf("reply = %q, want ping", resp.Data)
	}
}

func TestConnectNetworkAndJetStream(t *testing.T) {
	if _, err := natscore.RegisterRAMStore(8 * 1024 * 1024); err != nil {
		t.Fatalf("RegisterRAMStore: %v", err)
	}
	srv := startServer(t)
	c, err := bus.Connect(bus.Options{Name: "test-net", URL: srv.ClientURL()})
	if err != nil {
		t.Fatalf("Connect network: %v", err)
	}
	t.Cleanup(c.Close)

	if _, err := c.JetStream(); err != nil {
		t.Fatalf("JetStream: %v", err)
	}
}

func TestCustomInboxPrefixIsolation(t *testing.T) {
	srv := startServer(t)
	c, err := bus.Connect(bus.Options{Name: "prefixed", InboxPrefix: "_INBOX_alice", InProcess: srv})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)

	// A reply subject minted by this connection must live under its prefix.
	inbox := c.NATS().NewRespInbox()
	if len(inbox) < len("_INBOX_alice") || inbox[:len("_INBOX_alice")] != "_INBOX_alice" {
		t.Fatalf("inbox %q not under custom prefix", inbox)
	}
}
