// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// A power model with a composite "on" state:
//
//	off (leaf)
//	on (composite, initial=idle)
//	├── idle (leaf)
//	└── active (leaf)
//	fault (leaf)
type pstate uint8

const (
	off pstate = iota
	on
	idle
	active
	fault
)

type ptrig uint8

const (
	power ptrig = iota
	shutdown
	activate
	deactivate
	reset
)

type pev struct{ allow bool }

func allowG() Guard[pev] {
	return Guard[pev]{Name: "allow", Fn: func(_ context.Context, e pev) bool { return e.allow }}
}

func rec(trace *[]string, s string) Action[pstate, ptrig, pev] {
	return func(_ context.Context, _ Transition[pstate, ptrig], _ pev) error {
		*trace = append(*trace, s)
		return nil
	}
}

// buildPower assembles the power model. Entry/exit actions append to trace.
func buildPower(t *testing.T, trace *[]string) *Config[pstate, ptrig, pev] {
	t.Helper()
	b := New[pstate, ptrig, pev](off)
	b.State(off).
		OnEntry(rec(trace, "enter-off")).
		OnExit(rec(trace, "exit-off")).
		Permit(power, on)
	b.State(on).
		OnEntry(rec(trace, "enter-on")).
		OnExit(rec(trace, "exit-on")).
		Initial(idle).
		Permit(shutdown, off).
		Reentry(reset)
	b.State(idle).Parent(on).
		OnEntry(rec(trace, "enter-idle")).
		OnExit(rec(trace, "exit-idle")).
		Permit(activate, active)
	b.State(active).Parent(on).
		OnEntry(rec(trace, "enter-active")).
		OnExit(rec(trace, "exit-active")).
		Permit(deactivate, idle)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cfg
}

func TestInitialDescendOnEntry(t *testing.T) {
	var trace []string
	m := buildPower(t, &trace).Instance()
	if m.State() != off {
		t.Fatalf("initial state = %d, want off", m.State())
	}
	if err := m.Fire(context.Background(), power, pev{}); err != nil {
		t.Fatalf("power: %v", err)
	}
	if m.State() != idle {
		t.Fatalf("state = %d, want idle (descended into composite on)", m.State())
	}
	want := []string{"exit-off", "enter-on", "enter-idle"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestIsInSubstate(t *testing.T) {
	var trace []string
	m := buildPower(t, &trace).Instance()
	_ = m.Fire(context.Background(), power, pev{})
	// Now in idle, which is a substate of on.
	for s, want := range map[pstate]bool{idle: true, on: true, off: false, active: false, fault: false} {
		if m.IsIn(s) != want {
			t.Errorf("IsIn(%d) = %v, want %v", s, m.IsIn(s), want)
		}
	}
	if m.State() != idle {
		t.Fatalf("State() = %d, want idle", m.State())
	}
}

func TestBehavioralInheritance(t *testing.T) {
	var trace []string
	m := buildPower(t, &trace).Instance()
	ctx := context.Background()
	_ = m.Fire(ctx, power, pev{}) // -> idle
	trace = trace[:0]

	// shutdown has no handler in idle; it is inherited from on.
	if err := m.Fire(ctx, shutdown, pev{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if m.State() != off {
		t.Fatalf("state = %d, want off", m.State())
	}
	want := []string{"exit-idle", "exit-on", "enter-off"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestSiblingSwitchKeepsParent(t *testing.T) {
	var trace []string
	m := buildPower(t, &trace).Instance()
	ctx := context.Background()
	_ = m.Fire(ctx, power, pev{}) // -> idle
	trace = trace[:0]

	// idle -> active: LCA is on, so on is neither exited nor re-entered.
	if err := m.Fire(ctx, activate, pev{}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if m.State() != active {
		t.Fatalf("state = %d, want active", m.State())
	}
	want := []string{"exit-idle", "enter-active"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v (parent on must be untouched)", trace, want)
	}
	if !m.IsIn(on) {
		t.Fatal("IsIn(on) should still hold after sibling switch")
	}
}

func TestReentryComposite(t *testing.T) {
	var trace []string
	m := buildPower(t, &trace).Instance()
	ctx := context.Background()
	_ = m.Fire(ctx, power, pev{}) // -> idle
	trace = trace[:0]

	// reset is a reentry on the composite on, inherited from idle: exit down to
	// on, re-enter on, and descend the initial transition back to idle.
	if err := m.Fire(ctx, reset, pev{}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if m.State() != idle {
		t.Fatalf("state = %d, want idle", m.State())
	}
	want := []string{"exit-idle", "exit-on", "enter-on", "enter-idle"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestInheritedPermitted(t *testing.T) {
	var trace []string
	m := buildPower(t, &trace).Instance()
	ctx := context.Background()
	_ = m.Fire(ctx, power, pev{}) // -> idle

	var got []ptrig
	for tr := range m.Permitted(ctx, pev{}) {
		got = append(got, tr)
	}
	// idle permits activate; on (inherited) permits shutdown and reset.
	for _, want := range []ptrig{activate, shutdown, reset} {
		if !slices.Contains(got, want) {
			t.Errorf("permitted = %v, want to contain %d", got, want)
		}
	}
	if !m.CanFire(ctx, shutdown, pev{}) {
		t.Fatal("CanFire(shutdown) should be true via inheritance")
	}
}

func TestInnerGuardDoesNotShadowAncestor(t *testing.T) {
	// idle handles activate only when allowed; otherwise on handles it -> fault.
	b := New[pstate, ptrig, pev](off)
	b.State(off).Permit(power, on)
	b.State(on).Initial(idle).Permit(activate, fault)
	b.State(idle).Parent(on).Permit(activate, active, allowG())
	b.State(active).Parent(on)
	b.State(fault)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx := context.Background()

	m1 := cfg.Instance()
	_ = m1.Fire(ctx, power, pev{})
	_ = m1.Fire(ctx, activate, pev{allow: false}) // inner rejects -> ancestor
	if m1.State() != fault {
		t.Fatalf("state = %d, want fault (ancestor handled)", m1.State())
	}

	m2 := cfg.Instance()
	_ = m2.Fire(ctx, power, pev{})
	_ = m2.Fire(ctx, activate, pev{allow: true}) // inner handles
	if m2.State() != active {
		t.Fatalf("state = %d, want active (inner handled)", m2.State())
	}
}

func TestCompositeInitialInstance(t *testing.T) {
	// A machine whose top-level initial state is composite descends to a leaf.
	b := New[pstate, ptrig, pev](on)
	b.State(on).Initial(idle).Permit(shutdown, off)
	b.State(idle).Parent(on)
	b.State(active).Parent(on)
	b.State(off)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := cfg.Instance().State(); got != idle {
		t.Fatalf("initial leaf = %d, want idle", got)
	}
}

func TestHierarchyBuildErrors(t *testing.T) {
	tests := []struct {
		name  string
		build func() *Builder[pstate, ptrig, pev]
	}{
		{
			name: "parent cycle",
			build: func() *Builder[pstate, ptrig, pev] {
				b := New[pstate, ptrig, pev](off)
				b.State(off).Parent(on)
				b.State(on).Parent(off)
				return b
			},
		},
		{
			name: "initial not a child",
			build: func() *Builder[pstate, ptrig, pev] {
				b := New[pstate, ptrig, pev](off)
				b.State(off).Permit(power, on)
				b.State(on).Initial(off) // off is not a child of on
				return b
			},
		},
		{
			name: "composite entered without initial",
			build: func() *Builder[pstate, ptrig, pev] {
				b := New[pstate, ptrig, pev](off)
				b.State(off).Permit(power, on)
				b.State(idle).Parent(on) // on is composite but has no Initial
				return b
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.build().Build(); err == nil {
				t.Fatal("expected BuildError")
			} else if _, ok := errors.AsType[*BuildError](err); !ok {
				t.Fatalf("err = %v, want *BuildError", err)
			}
		})
	}
}
