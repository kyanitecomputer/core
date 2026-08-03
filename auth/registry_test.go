// SPDX-License-Identifier: BSD-3-Clause

package auth

import "testing"

func TestRegistryRegisterLookup(t *testing.T) {
	r := NewLocalRegistry()
	pr := Principal{Name: "thermal", Capabilities: []Capability{"telemetry.publish"}}
	if err := r.Register("Uaaa", pr); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("Uaaa")
	if !ok || got.Name != "thermal" {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
}

func TestRegistryRejectsEmptyAndDuplicate(t *testing.T) {
	r := NewLocalRegistry()
	if err := r.Register("", Principal{}); err != ErrEmptyKey {
		t.Fatalf("empty key err = %v, want ErrEmptyKey", err)
	}
	if err := r.Register("Ubbb", Principal{Name: "a"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register("Ubbb", Principal{Name: "b"}); err != ErrKeyRegistered {
		t.Fatalf("dup err = %v, want ErrKeyRegistered", err)
	}
}

func TestRegistryRevokeHygiene(t *testing.T) {
	r := NewLocalRegistry()
	if err := r.Register("Uccc", Principal{Name: "svc"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	pr, ok := r.Revoke("Uccc")
	if !ok || pr.Name != "svc" {
		t.Fatalf("Revoke = %+v, %v; want svc, true", pr, ok)
	}
	if _, ok := r.Lookup("Uccc"); ok {
		t.Fatal("revoked key still resolves")
	}
	if !r.Revoked("Uccc") {
		t.Fatal("key not tombstoned after revoke")
	}
	// A revoked per-boot key value must never be reusable.
	if err := r.Register("Uccc", Principal{Name: "svc2"}); err != ErrKeyRevoked {
		t.Fatalf("reuse err = %v, want ErrKeyRevoked", err)
	}
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
}

func TestRegistryRevokeUnknown(t *testing.T) {
	r := NewLocalRegistry()
	if _, ok := r.Revoke("Uzzz"); ok {
		t.Fatal("Revoke of unknown key reported ok")
	}
	// Revoking an unknown key still tombstones its value.
	if !r.Revoked("Uzzz") {
		t.Fatal("unknown key not tombstoned")
	}
}
