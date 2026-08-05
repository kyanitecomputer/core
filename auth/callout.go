// SPDX-License-Identifier: BSD-3-Clause

package auth

import (
	"errors"
	"time"

	"github.com/nats-io/jwt/v2"
)

// ErrCalloutIncomplete is returned by [NewCallout] when a required component is
// missing.
var ErrCalloutIncomplete = errors.New("auth: callout requires registry, policy, and issuer")

// CalloutSubject is the system subject on which the embedded server issues
// authorization requests for the callout to answer.
const CalloutSubject = "$SYS.REQ.USER.AUTH"

// CalloutConfig configures a [Callout]. Registry, Policy, and Issuer are
// required.
type CalloutConfig struct {
	// Registry resolves LOCAL principals by their per-boot public key.
	Registry *LocalRegistry
	// Policy compiles a principal's capabilities into permissions.
	Policy *Policy
	// Issuer mints and signs the authorization response.
	Issuer *Issuer

	// LocalAccount is the target NATS account for LOCAL principals. Defaults to
	// "LOCAL".
	LocalAccount string
	// LocalTTL bounds a LOCAL user JWT. It defaults to 0 (no wall-clock expiry):
	// LOCAL keys are per-boot and RAM-only, so their lifetime is the node epoch
	// and they carry no clock dependency (architecture §7).
	LocalTTL time.Duration
	// TrustedServer, when set, pins the expected auth-request issuer (the
	// embedded server's key). Requests signed by any other key are denied. When
	// empty, the transport (in-process, our firmware) is the trust anchor.
	TrustedServer string
}

// Callout answers NATS auth-callout requests for LOCAL principals: it decodes
// the server's authorization request, resolves the connecting actor by its
// per-boot NKey, compiles its permissions, and mints a signed response — or a
// signed denial. Deny is the default at every branch (architecture §6.3).
//
// It is single-owner (driven from the auth actor goroutine). This slice covers
// the LOCAL plane without XKey encryption; OPER token resolution and XKey are
// later slices.
type Callout struct {
	registry *LocalRegistry
	policy   *Policy
	issuer   *Issuer
	account  string
	ttl      time.Duration
	trusted  string
}

// NewCallout builds a Callout from cfg.
func NewCallout(cfg CalloutConfig) (*Callout, error) {
	if cfg.Registry == nil || cfg.Policy == nil || cfg.Issuer == nil {
		return nil, ErrCalloutIncomplete
	}
	account := cfg.LocalAccount
	if account == "" {
		account = "LOCAL"
	}
	return &Callout{
		registry: cfg.Registry,
		policy:   cfg.Policy,
		issuer:   cfg.Issuer,
		account:  account,
		ttl:      cfg.LocalTTL,
		trusted:  cfg.TrustedServer,
	}, nil
}

// Handle decodes and answers one authorization request. requestJWT is the token
// carried by the $SYS.REQ.USER.AUTH message; the returned string is the encoded
// authorization response JWT to reply with.
//
// A malformed or untrusted request returns an error (no response can be
// produced). An authenticated-but-unauthorized request returns a signed denial,
// not an error: a rejected principal is a decision, not a failure.
func (c *Callout) Handle(requestJWT string) (string, error) {
	req, err := jwt.DecodeAuthorizationRequestClaims(requestJWT)
	if err != nil {
		return "", err
	}
	if c.trusted != "" && req.Issuer != c.trusted {
		return "", errors.New("auth: authorization request signed by untrusted server")
	}

	userNKey := req.UserNkey
	serverID := req.Server.ID

	connNKey := req.ConnectOptions.Nkey
	if connNKey == "" {
		return c.issuer.Deny(userNKey, serverID, "no nkey presented")
	}
	pr, ok := c.registry.Lookup(connNKey)
	if !ok {
		return c.issuer.Deny(userNKey, serverID, "unknown principal")
	}

	perm, err := c.policy.Compile(pr)
	if err != nil {
		return c.issuer.Deny(userNKey, serverID, "policy error")
	}
	return c.issuer.Mint(Grant{
		UserNKey:    userNKey,
		ServerID:    serverID,
		Account:     c.account,
		Permissions: perm,
		TTL:         c.ttl,
	})
}
