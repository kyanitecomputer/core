// SPDX-License-Identifier: BSD-3-Clause

package mgmt_test

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"src.kyanite.computer/core/auth"
	"src.kyanite.computer/core/mgmt"
)

func TestPlaneBootstrapAndLocalMessaging(t *testing.T) {
	pb := auth.NewPolicy().DenySystem()
	pb.Capability("beat.publish").Publish("kyanite.{domain}.{device}.beat")
	pb.Capability("beat.collect").Subscribe("kyanite.{domain}.{device}.beat")
	policy, err := pb.Build()
	if err != nil {
		t.Fatalf("policy build: %v", err)
	}

	plane, err := mgmt.Start(mgmt.Config{
		Domain:       "r1",
		Device:       "n1",
		Policy:       policy,
		StoreDir:     t.TempDir(),
		RAMStoreSize: 8 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("mgmt.Start: %v", err)
	}
	t.Cleanup(func() { plane.Server().Shutdown() })

	collector, err := plane.ConnectLocal("collector", "beat.collect")
	if err != nil {
		t.Fatalf("ConnectLocal collector: %v", err)
	}
	t.Cleanup(collector.Close)

	got := make(chan []byte, 1)
	if _, err := collector.Subscribe("kyanite.r1.n1.beat", func(m *nats.Msg) { got <- m.Data }); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sensor, err := plane.ConnectLocal("sensor", "beat.publish")
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
		t.Fatal("authorized message not delivered through the management plane")
	}
}

func TestStartRequiresPolicy(t *testing.T) {
	if _, err := mgmt.Start(mgmt.Config{}); err == nil {
		t.Fatal("expected error without policy")
	}
}
