// SPDX-License-Identifier: BSD-3-Clause

// Package mgmt bootstraps a Kyanite node's management plane: the embedded NATS
// server (natscore) with JetStream backed by Scree, the auth callout plane
// (auth), and the client connection layer (bus), assembled into one handle that
// the device runtimes (vein, cairn) start at boot.
//
// The baseline is a pure-IPC node: the server listens only on the in-process
// transport (no network sockets), so local supervise children authenticate with
// per-boot NKeys and exchange messages without any network dependency. Network
// ingress (websocket for the WebUI, leafnodes for the mesh) is layered on during
// hardware/network integration.
package mgmt

import (
	"context"
	"fmt"

	"github.com/nats-io/nats-server/v2/server"

	"src.kyanite.computer/core/auth"
	"src.kyanite.computer/core/bus"
	"src.kyanite.computer/core/natscore"
	"src.kyanite.computer/core/service"
)

// Config configures the management-plane bootstrap. Policy is required; the
// rest have defaults.
type Config struct {
	// Domain and Device identify this node for LOCAL principal subject
	// substitution and audit.
	Domain string
	Device string
	// Policy is the compiled authorization policy for this node's principals.
	Policy *auth.Policy
	// StoreDir is the JetStream store directory (a mount point name on device).
	StoreDir string
	// RAMStoreSize, when > 0, registers a RAM-backed Scree JetStream store of
	// that many bytes before starting the server (used until flash-backed
	// storage is wired).
	RAMStoreSize int64
}

// Plane is a started management plane: the embedded server, the auth plane, and
// the callout's bypass connection with its handler attached.
type Plane struct {
	server  *server.Server
	auth    *auth.Plane
	svcConn *bus.Conn
}

// Start registers the (optional) Scree store, builds the auth plane, starts the
// in-process-only embedded server with the callout enabled, and attaches the
// callout handler. On return the callout is answering, so LOCAL actors may
// connect.
func Start(cfg Config) (*Plane, error) {
	if cfg.Policy == nil {
		return nil, fmt.Errorf("mgmt: policy is required")
	}
	if cfg.RAMStoreSize > 0 {
		if _, err := natscore.RegisterRAMStore(cfg.RAMStoreSize); err != nil {
			return nil, fmt.Errorf("mgmt: register scree store: %w", err)
		}
	}

	authPlane, err := auth.NewPlane(auth.PlaneConfig{
		Policy: cfg.Policy,
		Domain: cfg.Domain,
		Device: cfg.Device,
	})
	if err != nil {
		return nil, fmt.Errorf("mgmt: build auth plane: %w", err)
	}
	issuerPub, err := authPlane.IssuerPublicKey()
	if err != nil {
		return nil, fmt.Errorf("mgmt: issuer key: %w", err)
	}

	opts := natscore.Options(cfg.StoreDir)
	opts.DontListen = true // pure-IPC baseline; network listeners added later
	if err := natscore.EnableAuthCallout(opts, natscore.AuthCallout{
		IssuerPublicKey:  issuerPub,
		ServicePublicKey: authPlane.ServicePublicKey(),
	}); err != nil {
		return nil, fmt.Errorf("mgmt: enable auth callout: %w", err)
	}

	srv, err := natscore.Start(opts)
	if err != nil {
		return nil, fmt.Errorf("mgmt: start server: %w", err)
	}

	svcConn, err := authPlane.DialService(srv)
	if err != nil {
		srv.Shutdown()
		return nil, fmt.Errorf("mgmt: dial callout service: %w", err)
	}
	if _, err := authPlane.Attach(svcConn); err != nil {
		svcConn.Close()
		srv.Shutdown()
		return nil, fmt.Errorf("mgmt: attach callout: %w", err)
	}

	return &Plane{server: srv, auth: authPlane, svcConn: svcConn}, nil
}

// ConnectLocal registers a LOCAL actor with a fresh per-boot NKey and returns
// its authorized in-process connection. Call during boot for each actor that
// needs the bus.
func (p *Plane) ConnectLocal(name string, caps ...auth.Capability) (*bus.Conn, error) {
	return p.auth.ConnectLocal(p.server, name, caps...)
}

// Server returns the embedded server (for advanced wiring, e.g. later network
// listeners).
func (p *Plane) Server() *server.Server { return p.server }

// Service returns a supervised service that keeps the management plane's own
// connection alive and drains it (and shuts the server down) when the context
// is cancelled. Add it to the operator's service set.
func (p *Plane) Service() service.Service {
	return service.New("mgmt", func(ctx context.Context) error {
		<-ctx.Done()
		_ = p.svcConn.Drain()
		p.server.Shutdown()
		return nil
	})
}
