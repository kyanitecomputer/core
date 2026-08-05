// SPDX-License-Identifier: BSD-3-Clause

package auth_test

import (
	"slices"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"

	"src.kyanite.computer/core/auth"
)

func TestIssuerMintProducesValidResponse(t *testing.T) {
	issKP, err := auth.GenerateIssuerKey()
	if err != nil {
		t.Fatalf("GenerateIssuerKey: %v", err)
	}
	iss := auth.NewIssuer(issKP)
	issPub, err := iss.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	_, userNKey, err := auth.GenerateUserKey()
	if err != nil {
		t.Fatalf("GenerateUserKey: %v", err)
	}
	const serverID = "NABCDEF"

	perm := auth.Permissions{
		PubAllow: []string{"kyanite.r1.bmc07.telemetry.>"},
		SubAllow: []string{"_INBOX_thermal.>"},
		SubDeny:  []string{"$SYS.>"},
	}
	respJWT, err := iss.Mint(auth.Grant{
		UserNKey:    userNKey,
		ServerID:    serverID,
		Account:     "LOCAL",
		Permissions: perm,
		TTL:         time.Minute,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	rc, err := jwt.DecodeAuthorizationResponseClaims(respJWT)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if rc.Subject != userNKey {
		t.Fatalf("response sub = %q, want %q", rc.Subject, userNKey)
	}
	if rc.Audience != serverID {
		t.Fatalf("response aud = %q, want %q", rc.Audience, serverID)
	}
	if rc.Issuer != issPub {
		t.Fatalf("response iss = %q, want %q", rc.Issuer, issPub)
	}
	if rc.Error != "" {
		t.Fatalf("response carried error %q", rc.Error)
	}

	uc, err := jwt.DecodeUserClaims(rc.Jwt)
	if err != nil {
		t.Fatalf("decode user jwt: %v", err)
	}
	if uc.Subject != userNKey {
		t.Fatalf("user sub = %q, want %q (connection binding)", uc.Subject, userNKey)
	}
	if uc.Audience != "LOCAL" {
		t.Fatalf("user aud = %q, want LOCAL", uc.Audience)
	}
	if uc.Issuer != issPub {
		t.Fatalf("user iss = %q, want issuer", uc.Issuer)
	}
	if uc.Expires == 0 {
		t.Fatal("user jwt has no expiry")
	}
	if !slices.Contains([]string(uc.Permissions.Pub.Allow), "kyanite.r1.bmc07.telemetry.>") {
		t.Fatalf("pub allow missing: %v", uc.Permissions.Pub.Allow)
	}
	if !slices.Contains([]string(uc.Permissions.Sub.Deny), "$SYS.>") {
		t.Fatalf("sub deny missing $SYS.>: %v", uc.Permissions.Sub.Deny)
	}
}

func TestIssuerDeny(t *testing.T) {
	issKP, err := auth.GenerateIssuerKey()
	if err != nil {
		t.Fatalf("GenerateIssuerKey: %v", err)
	}
	iss := auth.NewIssuer(issKP)
	_, userNKey, _ := auth.GenerateUserKey()

	respJWT, err := iss.Deny(userNKey, "NABCDEF", "not authorized")
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	rc, err := jwt.DecodeAuthorizationResponseClaims(respJWT)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rc.Error != "not authorized" {
		t.Fatalf("error = %q, want 'not authorized'", rc.Error)
	}
	if rc.Jwt != "" {
		t.Fatal("deny response should carry no user jwt")
	}
}
