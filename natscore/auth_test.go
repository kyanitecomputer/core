// SPDX-License-Identifier: BSD-3-Clause

package natscore

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func TestEnableAuthCalloutBypassConnects(t *testing.T) {
	svcKP, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create service user: %v", err)
	}
	svcPub, _ := svcKP.PublicKey()
	issKP, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("create issuer: %v", err)
	}
	issPub, _ := issKP.PublicKey()

	opts := Options(t.TempDir())
	opts.NoLog = true
	// Pure-IPC node: no network listeners, in-process transport only.
	opts.DontListen = true
	if err := EnableAuthCallout(opts, AuthCallout{IssuerPublicKey: issPub, ServicePublicKey: svcPub}); err != nil {
		t.Fatalf("EnableAuthCallout: %v", err)
	}

	srv, err := Start(opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	// The auth_users bypass identity connects in-process without a callout.
	nc, err := nats.Connect("", nats.InProcessServer(srv), nats.Nkey(svcPub, svcKP.Sign))
	if err != nil {
		t.Fatalf("bypass connect: %v", err)
	}
	t.Cleanup(nc.Close)
	if !nc.IsConnected() {
		t.Fatal("bypass user not connected")
	}
}

func TestEnableAuthCalloutValidation(t *testing.T) {
	opts := Options(t.TempDir())
	if err := EnableAuthCallout(opts, AuthCallout{}); err == nil {
		t.Fatal("expected error for missing keys")
	}
}
