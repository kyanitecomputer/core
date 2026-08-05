// SPDX-License-Identifier: BSD-3-Clause

package auth

import (
	"fmt"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// GenerateUserKey creates a fresh ephemeral user NKey. It returns the seed to
// hand to the owning child and the public key to register with [LocalRegistry].
// Keys are per-boot and RAM-only; a power cycle invalidates them (architecture
// §7).
func GenerateUserKey() (seed []byte, public string, err error) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		return nil, "", fmt.Errorf("auth: create user nkey: %w", err)
	}
	seed, err = kp.Seed()
	if err != nil {
		return nil, "", fmt.Errorf("auth: user nkey seed: %w", err)
	}
	public, err = kp.PublicKey()
	if err != nil {
		return nil, "", fmt.Errorf("auth: user nkey public: %w", err)
	}
	return seed, public, nil
}

// GenerateIssuerKey creates an account NKey used to sign minted user JWTs and
// authorization responses. In production the seed is TEE-sealed and signing goes
// through the GoTEE oracle (§11); the returned pair is the plain-RAM form for
// platforms without a TEE and for tests.
func GenerateIssuerKey() (nkeys.KeyPair, error) {
	kp, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("auth: create issuer nkey: %w", err)
	}
	return kp, nil
}

// Issuer mints signed user JWTs and authorization responses for the callout. It
// is backed by an [nkeys.KeyPair]; because that is an interface, the issuer seed
// can stay in a TEE with signing performed by the GoTEE oracle, so it never
// enters the normal world (§11). The zero value is not usable; construct with
// [NewIssuer].
type Issuer struct {
	kp  nkeys.KeyPair
	now func() time.Time
}

// NewIssuer returns an Issuer signing with kp.
func NewIssuer(kp nkeys.KeyPair) *Issuer {
	return &Issuer{kp: kp, now: time.Now}
}

// PublicKey returns the issuer's public key, which must match the server's
// configured auth_callout issuer.
func (i *Issuer) PublicKey() (string, error) {
	return i.kp.PublicKey()
}

// Grant describes a permission grant to mint into a user JWT. UserNKey is the
// server-generated ephemeral key from the auth request (the minted JWT's sub,
// binding it to this connection); ServerID is the requesting server (the
// response audience); Account is the target NATS account (the user JWT
// audience, e.g. LOCAL or OPER).
type Grant struct {
	UserNKey    string
	ServerID    string
	Account     string
	Permissions Permissions
	TTL         time.Duration
}

// Mint builds a user JWT with the grant's permissions, wraps it in an
// authorization response bound to the connection, and signs both with the
// issuer key. The returned string is the encoded authorization response JWT to
// send back on the callout reply.
func (i *Issuer) Mint(g Grant) (string, error) {
	uc := jwt.NewUserClaims(g.UserNKey)
	uc.Audience = g.Account
	uc.Permissions = toJWTPermissions(g.Permissions)
	if g.TTL > 0 {
		uc.Expires = i.now().Add(g.TTL).Unix()
	}
	userJWT, err := uc.Encode(i.kp)
	if err != nil {
		return "", fmt.Errorf("auth: encode user jwt: %w", err)
	}
	return i.respond(g.UserNKey, g.ServerID, userJWT, "")
}

// Deny builds and signs an authorization response denying the connection with
// the given reason.
func (i *Issuer) Deny(userNKey, serverID, reason string) (string, error) {
	return i.respond(userNKey, serverID, "", reason)
}

func (i *Issuer) respond(userNKey, serverID, userJWT, errMsg string) (string, error) {
	rc := jwt.NewAuthorizationResponseClaims(userNKey)
	rc.Audience = serverID
	rc.Jwt = userJWT
	rc.Error = errMsg
	token, err := rc.Encode(i.kp)
	if err != nil {
		return "", fmt.Errorf("auth: encode authorization response: %w", err)
	}
	return token, nil
}

// toJWTPermissions converts the neutral policy permissions into the JWT form
// embedded in a user claim.
func toJWTPermissions(p Permissions) jwt.Permissions {
	return jwt.Permissions{
		Pub: jwt.Permission{Allow: jwt.StringList(p.PubAllow), Deny: jwt.StringList(p.PubDeny)},
		Sub: jwt.Permission{Allow: jwt.StringList(p.SubAllow), Deny: jwt.StringList(p.SubDeny)},
	}
}
