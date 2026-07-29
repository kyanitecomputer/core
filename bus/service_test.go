// SPDX-License-Identifier: BSD-3-Clause

package bus_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"src.kyanite.computer/core/bus"
)

func TestServiceEndpointRequestReply(t *testing.T) {
	srv := startServer(t)
	c, err := bus.Connect(bus.Options{Name: "svc-host", InProcess: srv})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)

	svc, err := c.AddService(bus.ServiceConfig{Name: "greeter", Version: "1.2.3", Description: "greets"})
	if err != nil {
		t.Fatalf("AddService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	err = svc.AddEndpoint(bus.EndpointConfig{
		Name:    "hello",
		Subject: "greeter.hello",
		Handler: func(_ context.Context, req bus.Request) {
			_ = req.Respond(append([]byte("hello "), req.Data()...))
		},
	})
	if err != nil {
		t.Fatalf("AddEndpoint: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := c.Request(ctx, "greeter.hello", []byte("world"))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(resp.Data) != "hello world" {
		t.Fatalf("reply = %q, want %q", resp.Data, "hello world")
	}
}

func TestServiceGroupsAndDiscovery(t *testing.T) {
	srv := startServer(t)
	c, err := bus.Connect(bus.Options{Name: "svc-host2", InProcess: srv})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)

	svc, err := c.AddService(bus.ServiceConfig{Name: "sensors", Version: "0.1.0"})
	if err != nil {
		t.Fatalf("AddService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	g := svc.Group("sensors")
	if err := g.AddEndpoint(bus.EndpointConfig{
		Name:    "temperature",
		Handler: func(_ context.Context, req bus.Request) { _ = req.Respond([]byte("42")) },
	}); err != nil {
		t.Fatalf("group AddEndpoint: %v", err)
	}

	// The endpoint subject is the group prefix + endpoint name.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := c.Request(ctx, "sensors.temperature", nil)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(resp.Data) != "42" {
		t.Fatalf("reply = %q, want 42", resp.Data)
	}

	// Discovery: PING the service on the standard $SRV subject.
	pingResp, err := c.Request(ctx, "$SRV.PING.sensors", nil)
	if err != nil {
		t.Fatalf("PING: %v", err)
	}
	var ping struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(pingResp.Data, &ping); err != nil {
		t.Fatalf("decode ping: %v", err)
	}
	if ping.Name != "sensors" {
		t.Fatalf("ping name = %q, want sensors", ping.Name)
	}

	// Stats reflect the handled requests.
	stats := svc.Stats()
	if len(stats.Endpoints) == 0 {
		t.Fatal("expected at least one endpoint in stats")
	}
}

func TestServiceValidation(t *testing.T) {
	srv := startServer(t)
	c, err := bus.Connect(bus.Options{Name: "svc-host3", InProcess: srv})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)

	if _, err := c.AddService(bus.ServiceConfig{}); err != bus.ErrNoServiceName {
		t.Fatalf("empty name err = %v, want ErrNoServiceName", err)
	}
	svc, err := c.AddService(bus.ServiceConfig{Name: "v"})
	if err != nil {
		t.Fatalf("AddService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	if err := svc.AddEndpoint(bus.EndpointConfig{Handler: func(context.Context, bus.Request) {}}); err != bus.ErrNoEndpointName {
		t.Fatalf("no-name endpoint err = %v, want ErrNoEndpointName", err)
	}
	if err := svc.AddEndpoint(bus.EndpointConfig{Name: "x"}); err != bus.ErrNoHandler {
		t.Fatalf("no-handler endpoint err = %v, want ErrNoHandler", err)
	}
}
