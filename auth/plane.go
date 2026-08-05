// SPDX-License-Identifier: BSD-3-Clause

package auth

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"src.kyanite.computer/core/bus"
)

// PlaneConfig configures a [Plane]. Policy is required; the rest have defaults.
type PlaneConfig struct {
	// Policy compiles principal capabilities to permissions.
	Policy *Policy
	// Issuer signs minted JWTs. Generated (RAM-only) if nil.
	Issuer *Issuer
	// Domain and Device are the substitution context for LOCAL principals'
	// subject templates.
	Domain string
	Device string
	// LocalAccount is the account minted LOCAL principals land in. Defaults to
	// natscore's LOCAL account.
	LocalAccount string
	// LocalTTL bounds LOCAL user JWTs; 0 (default) means no wall-clock expiry.
	LocalTTL time.Duration
}

// Plane is the runtime assembly of the auth callout: the LOCAL registry, the
// issuer, the policy, and the decision handler, plus the callout service's own
// bypass identity. It supplies the public keys the embedded server's
// auth_callout block needs, opens the service's bypass connection, answers
// authorization requests, and issues authenticated in-process connections for
// LOCAL actors.
//
// The intended lifecycle is single-threaded at boot: build the plane, enable
// the callout on the server with its public keys, DialService, Attach, then
// ConnectLocal each actor. The registry is written only during that boot
// registration and read by the (serially dispatched) callout handler, so it
// stays lock-free — matching the single-owner discipline. Minting fresh keys on
// a live restart, which would write the registry concurrently, is future work
// (see the auth notes).
type Plane struct {
	registry *LocalRegistry
	callout  *Callout
	issuer   *Issuer
	domain   string
	device   string

	serviceSeed []byte
	servicePub  string
}

// NewPlane assembles a Plane from cfg.
func NewPlane(cfg PlaneConfig) (*Plane, error) {
	if cfg.Policy == nil {
		return nil, fmt.Errorf("auth: plane requires a policy")
	}
	issuer := cfg.Issuer
	if issuer == nil {
		kp, err := GenerateIssuerKey()
		if err != nil {
			return nil, err
		}
		issuer = NewIssuer(kp)
	}
	serviceSeed, servicePub, err := GenerateUserKey()
	if err != nil {
		return nil, err
	}

	reg := NewLocalRegistry()
	callout, err := NewCallout(CalloutConfig{
		Registry:     reg,
		Policy:       cfg.Policy,
		Issuer:       issuer,
		LocalAccount: cfg.LocalAccount,
		LocalTTL:     cfg.LocalTTL,
	})
	if err != nil {
		return nil, err
	}
	return &Plane{
		registry:    reg,
		callout:     callout,
		issuer:      issuer,
		domain:      cfg.Domain,
		device:      cfg.Device,
		serviceSeed: serviceSeed,
		servicePub:  servicePub,
	}, nil
}

// IssuerPublicKey returns the issuer account public key for the server's
// auth_callout issuer field.
func (p *Plane) IssuerPublicKey() (string, error) { return p.issuer.PublicKey() }

// ServicePublicKey returns the callout service's public key for the server's
// auth_users bypass list.
func (p *Plane) ServicePublicKey() string { return p.servicePub }

// DialService opens the plane's own bypass connection to the in-process server.
// It connects without invoking the callout (its key is in auth_users), so it
// must be established before any LOCAL actor connects.
func (p *Plane) DialService(srv nats.InProcessConnProvider) (*bus.Conn, error) {
	return bus.Connect(bus.Options{
		Name:        "auth",
		InProcess:   srv,
		NKeySeed:    p.serviceSeed,
		InboxPrefix: "_INBOX_auth",
	})
}

// Attach subscribes the callout handler on conn (the plane's bypass connection).
// The callout is active on return, so call it before LOCAL actors connect. The
// async subscription dispatches serially, preserving the callout's single-owner
// access to the registry.
func (p *Plane) Attach(conn *bus.Conn) (*nats.Subscription, error) {
	return conn.Subscribe(CalloutSubject, func(m *nats.Msg) {
		resp, err := p.callout.Handle(string(m.Data))
		if err != nil {
			// A malformed or untrusted request cannot be answered; drop it and
			// let the server's authorization timeout reject the connection.
			return
		}
		_ = m.Respond([]byte(resp))
	})
}

// ConnectLocal registers a LOCAL principal with a fresh per-boot NKey and opens
// its authenticated in-process connection, which the callout authorizes against
// the registered capabilities. Call during boot, after Attach.
func (p *Plane) ConnectLocal(srv nats.InProcessConnProvider, name string, caps ...Capability) (*bus.Conn, error) {
	seed, pub, err := GenerateUserKey()
	if err != nil {
		return nil, err
	}
	inbox := "_INBOX_" + name
	pr := Principal{
		Name:         name,
		Domain:       p.domain,
		Device:       p.device,
		InboxPrefix:  inbox,
		Capabilities: caps,
	}
	if err := p.registry.Register(pub, pr); err != nil {
		return nil, fmt.Errorf("auth: register %q: %w", name, err)
	}
	conn, err := bus.Connect(bus.Options{
		Name:        name,
		InProcess:   srv,
		NKeySeed:    seed,
		InboxPrefix: inbox,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: connect %q: %w", name, err)
	}
	return conn, nil
}
