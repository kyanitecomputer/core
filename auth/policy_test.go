// SPDX-License-Identifier: BSD-3-Clause

package auth

import (
	"errors"
	"slices"
	"testing"
)

func buildPolicy(t *testing.T) *Policy {
	t.Helper()
	b := NewPolicy().DenySystem()
	b.Capability("telemetry.publish").
		Publish("kyanite.{domain}.{device}.telemetry.>")
	b.Capability("scree.rw").
		Publish("$KV.config.>").
		Subscribe("$KV.config.>")
	b.Capability("svc.serve").
		Subscribe("svc.{principal}.>")
	p, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return p
}

func TestCompileSubstitutesAndIsolates(t *testing.T) {
	p := buildPolicy(t)
	perm, err := p.Compile(Principal{
		Name:         "thermal",
		Domain:       "r1",
		Device:       "bmc07",
		InboxPrefix:  "_INBOX_thermal",
		Capabilities: []Capability{"telemetry.publish", "svc.serve"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if !slices.Contains(perm.PubAllow, "kyanite.r1.bmc07.telemetry.>") {
		t.Fatalf("PubAllow missing substituted subject: %v", perm.PubAllow)
	}
	if !slices.Contains(perm.SubAllow, "svc.thermal.>") {
		t.Fatalf("SubAllow missing svc subject: %v", perm.SubAllow)
	}
	// Private inbox is granted automatically for reply isolation.
	if !slices.Contains(perm.SubAllow, "_INBOX_thermal.>") {
		t.Fatalf("SubAllow missing private inbox: %v", perm.SubAllow)
	}
	// Systematic $SYS.> deny.
	if !slices.Contains(perm.SubDeny, "$SYS.>") || !slices.Contains(perm.PubDeny, "$SYS.>") {
		t.Fatalf("missing $SYS.> deny: pub=%v sub=%v", perm.PubDeny, perm.SubDeny)
	}
}

func TestCompileUnknownCapability(t *testing.T) {
	p := buildPolicy(t)
	_, err := p.Compile(Principal{Name: "x", Capabilities: []Capability{"does.not.exist"}})
	var uce *UnknownCapabilityError
	if !errors.As(err, &uce) {
		t.Fatalf("err = %v, want *UnknownCapabilityError", err)
	}
	if uce.Capability != "does.not.exist" {
		t.Fatalf("capability = %q", uce.Capability)
	}
}

func TestCompileDeduplicates(t *testing.T) {
	// Two capabilities granting the same subject must not duplicate it.
	b := NewPolicy()
	b.Capability("a").Publish("kyanite.{device}.x")
	b.Capability("b").Publish("kyanite.{device}.x")
	p, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	perm, err := p.Compile(Principal{Device: "d", Capabilities: []Capability{"a", "b"}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	count := 0
	for _, s := range perm.PubAllow {
		if s == "kyanite.d.x" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("subject appears %d times, want 1: %v", count, perm.PubAllow)
	}
}

func TestBuildRejectsEmptySubject(t *testing.T) {
	b := NewPolicy()
	b.Capability("bad").Publish("")
	if _, err := b.Build(); err == nil {
		t.Fatal("expected PolicyBuildError for empty subject")
	} else if _, ok := err.(*PolicyBuildError); !ok {
		t.Fatalf("err = %v, want *PolicyBuildError", err)
	}
}
