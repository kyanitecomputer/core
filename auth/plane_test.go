// SPDX-License-Identifier: BSD-3-Clause

package auth_test

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"src.kyanite.computer/core/auth"
	"src.kyanite.computer/core/bus"
	"src.kyanite.computer/core/natscore"
)

func buildPlaneAndServer(t *testing.T) (*auth.Plane, *server.Server) {
	t.Helper()
	pb := auth.NewPolicy().DenySystem()
	pb.Capability("beat.publish").Publish("kyanite.{domain}.{device}.beat")
	pb.Capability("beat.collect").Subscribe("kyanite.{domain}.{device}.beat")
	policy, err := pb.Build()
	if err != nil {
		t.Fatalf("policy build: %v", err)
	}
	plane, err := auth.NewPlane(auth.PlaneConfig{Policy: policy, Domain: "r1", Device: "n1"})
	if err != nil {
		t.Fatalf("NewPlane: %v", err)
	}
	issPub, err := plane.IssuerPublicKey()
	if err != nil {
		t.Fatalf("IssuerPublicKey: %v", err)
	}

	opts := natscore.Options(t.TempDir())
	opts.NoLog = true
	opts.DontListen = true
	if err := natscore.EnableAuthCallout(opts, natscore.AuthCallout{
		IssuerPublicKey:  issPub,
		ServicePublicKey: plane.ServicePublicKey(),
	}); err != nil {
		t.Fatalf("EnableAuthCallout: %v", err)
	}
	srv, err := natscore.Start(opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	svcConn, err := plane.DialService(srv)
	if err != nil {
		t.Fatalf("DialService: %v", err)
	}
	t.Cleanup(svcConn.Close)
	sub, err := plane.Attach(svcConn)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	return plane, srv
}

func TestPlaneLocalActorsAuthorizedAndDelivered(t *testing.T) {
	plane, srv := buildPlaneAndServer(t)

	collector, err := plane.ConnectLocal(srv, "collector", "beat.collect")
	if err != nil {
		t.Fatalf("ConnectLocal collector: %v", err)
	}
	t.Cleanup(collector.Close)

	got := make(chan []byte, 1)
	if _, err := collector.Subscribe("kyanite.r1.n1.beat", func(m *nats.Msg) {
		got <- m.Data
	}); err != nil {
		t.Fatalf("collector subscribe: %v", err)
	}

	sensor, err := plane.ConnectLocal(srv, "sensor", "beat.publish")
	if err != nil {
		t.Fatalf("ConnectLocal sensor: %v", err)
	}
	t.Cleanup(sensor.Close)

	if err := sensor.Publish("kyanite.r1.n1.beat", []byte("tick")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case data := <-got:
		if string(data) != "tick" {
			t.Fatalf("received %q, want tick", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("collector did not receive the authorized publish")
	}
}

func TestPlaneDeniesUnregisteredActor(t *testing.T) {
	_, srv := buildPlaneAndServer(t)

	// A random, unregistered NKey must be denied by the callout, so the connect
	// fails rather than succeeding with no permissions.
	seed, _, err := auth.GenerateUserKey()
	if err != nil {
		t.Fatalf("GenerateUserKey: %v", err)
	}
	conn, err := bus.Connect(bus.Options{
		Name:        "stranger",
		InProcess:   srv,
		NKeySeed:    seed,
		InboxPrefix: "_INBOX_stranger",
	})
	if err == nil {
		conn.Close()
		t.Fatal("unregistered actor was allowed to connect")
	}
}
