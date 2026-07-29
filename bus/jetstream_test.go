// SPDX-License-Identifier: BSD-3-Clause

package bus_test

import (
	"context"
	"testing"
	"time"

	"src.kyanite.computer/core/bus"
	"src.kyanite.computer/core/natscore"
)

func TestEnsureStreamAndPublish(t *testing.T) {
	if _, err := natscore.RegisterRAMStore(8 * 1024 * 1024); err != nil {
		t.Fatalf("RegisterRAMStore: %v", err)
	}
	srv := startServer(t)
	c, err := bus.Connect(bus.Options{Name: "js-host", URL: srv.ClientURL()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)

	streams, err := c.Streams()
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := bus.StreamConfig{
		Name:     "AUDIT",
		Subjects: []string{"audit.>"},
		Storage:  bus.StorageScree,
	}
	if err := streams.EnsureStream(ctx, cfg); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	// Idempotent: a second call is a no-op.
	if err := streams.EnsureStream(ctx, cfg); err != nil {
		t.Fatalf("EnsureStream (again): %v", err)
	}

	seq, err := streams.Publish(ctx, "audit.decision", []byte("allow"))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if seq != 1 {
		t.Fatalf("first publish sequence = %d, want 1", seq)
	}

	// Read the message back via the JetStream context to confirm persistence.
	js, err := c.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	msg, err := js.GetMsg("AUDIT", 1)
	if err != nil {
		t.Fatalf("GetMsg: %v", err)
	}
	if string(msg.Data) != "allow" {
		t.Fatalf("stored data = %q, want allow", msg.Data)
	}
}

func TestEnsureStreamValidation(t *testing.T) {
	srv := startServer(t)
	c, err := bus.Connect(bus.Options{Name: "js-host2", URL: srv.ClientURL()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)
	streams, err := c.Streams()
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	if err := streams.EnsureStream(context.Background(), bus.StreamConfig{}); err != bus.ErrNoStreamName {
		t.Fatalf("empty name err = %v, want ErrNoStreamName", err)
	}
}

func TestStreamConfigNormalize(t *testing.T) {
	var c bus.StreamConfig
	c.Name = "S"
	c.Normalize()
	if c.Storage != bus.StorageScree || c.Retention != bus.RetentionLimits || c.Replicas != 1 {
		t.Fatalf("Normalize defaults wrong: %+v", c)
	}
}
