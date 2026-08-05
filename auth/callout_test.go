// SPDX-License-Identifier: BSD-3-Clause

package auth_test

import (
	"slices"
	"testing"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"

	"src.kyanite.computer/core/auth"
)

// authRequest builds a server-signed authorization request for a connection
// presenting connNKey, mirroring what the embedded server sends on
// $SYS.REQ.USER.AUTH.
func authRequest(t *testing.T, srv nkeys.KeyPair, serverID, userNKey, connNKey string) string {
	t.Helper()
	rc := jwt.NewAuthorizationRequestClaims(userNKey)
	rc.UserNkey = userNKey
	rc.Server.ID = serverID
	rc.ConnectOptions.Nkey = connNKey
	token, err := rc.Encode(srv)
	if err != nil {
		t.Fatalf("encode auth request: %v", err)
	}
	return token
}

func newCallout(t *testing.T, reg *auth.LocalRegistry, trusted string) *auth.Callout {
	t.Helper()
	pb := auth.NewPolicy().DenySystem()
	pb.Capability("telemetry.publish").Publish("kyanite.{domain}.{device}.telemetry.>")
	policy, err := pb.Build()
	if err != nil {
		t.Fatalf("policy build: %v", err)
	}
	issKP, err := auth.GenerateIssuerKey()
	if err != nil {
		t.Fatalf("issuer key: %v", err)
	}
	c, err := auth.NewCallout(auth.CalloutConfig{
		Registry:      reg,
		Policy:        policy,
		Issuer:        auth.NewIssuer(issKP),
		TrustedServer: trusted,
	})
	if err != nil {
		t.Fatalf("NewCallout: %v", err)
	}
	return c
}

func TestCalloutMintsForRegisteredLocalActor(t *testing.T) {
	srv, _ := nkeys.CreateServer()
	srvPub, _ := srv.PublicKey()

	reg := auth.NewLocalRegistry()
	_, childPub, _ := auth.GenerateUserKey()
	if err := reg.Register(childPub, auth.Principal{
		Name:         "thermal",
		Domain:       "r1",
		Device:       "bmc07",
		InboxPrefix:  "_INBOX_thermal",
		Capabilities: []auth.Capability{"telemetry.publish"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	c := newCallout(t, reg, srvPub)

	_, userNKey, _ := auth.GenerateUserKey()
	respJWT, err := c.Handle(authRequest(t, srv, "NID1", userNKey, childPub))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	rc, err := jwt.DecodeAuthorizationResponseClaims(respJWT)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if rc.Error != "" {
		t.Fatalf("expected mint, got error %q", rc.Error)
	}
	uc, err := jwt.DecodeUserClaims(rc.Jwt)
	if err != nil {
		t.Fatalf("decode user jwt: %v", err)
	}
	if uc.Subject != userNKey {
		t.Fatalf("user sub = %q, want %q", uc.Subject, userNKey)
	}
	if uc.Audience != "LOCAL" {
		t.Fatalf("user aud = %q, want LOCAL", uc.Audience)
	}
	if !slices.Contains([]string(uc.Permissions.Pub.Allow), "kyanite.r1.bmc07.telemetry.>") {
		t.Fatalf("permissions missing telemetry subject: %v", uc.Permissions.Pub.Allow)
	}
	if !slices.Contains([]string(uc.Permissions.Sub.Allow), "_INBOX_thermal.>") {
		t.Fatalf("permissions missing private inbox: %v", uc.Permissions.Sub.Allow)
	}
}

func TestCalloutDeniesUnknownPrincipal(t *testing.T) {
	srv, _ := nkeys.CreateServer()
	srvPub, _ := srv.PublicKey()
	reg := auth.NewLocalRegistry()
	c := newCallout(t, reg, srvPub)

	_, userNKey, _ := auth.GenerateUserKey()
	_, strangerPub, _ := auth.GenerateUserKey()
	respJWT, err := c.Handle(authRequest(t, srv, "NID1", userNKey, strangerPub))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	rc, err := jwt.DecodeAuthorizationResponseClaims(respJWT)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rc.Error == "" {
		t.Fatal("expected denial for unknown principal")
	}
	if rc.Jwt != "" {
		t.Fatal("denial should carry no user jwt")
	}
}

func TestCalloutRejectsUntrustedServer(t *testing.T) {
	trusted, _ := nkeys.CreateServer()
	trustedPub, _ := trusted.PublicKey()
	rogue, _ := nkeys.CreateServer()

	reg := auth.NewLocalRegistry()
	c := newCallout(t, reg, trustedPub)

	_, userNKey, _ := auth.GenerateUserKey()
	_, childPub, _ := auth.GenerateUserKey()
	// Signed by a server key other than the pinned one.
	_, err := c.Handle(authRequest(t, rogue, "NID1", userNKey, childPub))
	if err == nil {
		t.Fatal("expected error for untrusted server signature")
	}
}

func TestNewCalloutValidation(t *testing.T) {
	if _, err := auth.NewCallout(auth.CalloutConfig{}); err != auth.ErrCalloutIncomplete {
		t.Fatalf("err = %v, want ErrCalloutIncomplete", err)
	}
}
