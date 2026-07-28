// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"context"
	"testing"
)

func TestObserverReportsTransitions(t *testing.T) {
	var seen []Transition[pstate, ptrig]
	b := New[pstate, ptrig, pev](off)
	b.Observe(func(_ context.Context, tr Transition[pstate, ptrig], _ pev) {
		seen = append(seen, tr)
	})
	b.State(off).Permit(power, on)
	b.State(on).Initial(idle).Permit(shutdown, off)
	b.State(idle).Parent(on).Internal(reset, func(_ context.Context, _ Transition[pstate, ptrig], _ pev) error { return nil })
	b.State(active).Parent(on)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m := cfg.Instance()
	ctx := context.Background()

	// External into composite on: the observer's destination is the resting
	// leaf (idle), not the declared target (on).
	_ = m.Fire(ctx, power, pev{})
	if len(seen) != 1 {
		t.Fatalf("observations = %d, want 1", len(seen))
	}
	if seen[0].Source != off || seen[0].Destination != idle || seen[0].Kind != External {
		t.Fatalf("transition = %+v, want off->idle external", seen[0])
	}

	// Internal reaction inherited from idle: reported with Kind Internal, no
	// state change.
	_ = m.Fire(ctx, reset, pev{})
	if len(seen) != 2 || seen[1].Kind != Internal || seen[1].Destination != idle {
		t.Fatalf("internal observation = %+v", seen[1])
	}
}

func TestObserverNotCalledOnRejectionOrIgnore(t *testing.T) {
	var count int
	b := New[pstate, ptrig, pev](off)
	b.Observe(func(_ context.Context, _ Transition[pstate, ptrig], _ pev) { count++ })
	b.State(off).Ignore(reset).Permit(power, on)
	b.State(on)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m := cfg.Instance()
	ctx := context.Background()

	_ = m.Fire(ctx, reset, pev{})    // ignored: not a transition
	_ = m.Fire(ctx, shutdown, pev{}) // unhandled: not a transition
	if count != 0 {
		t.Fatalf("observer called %d times on ignore/unhandled, want 0", count)
	}
	_ = m.Fire(ctx, power, pev{}) // real transition
	if count != 1 {
		t.Fatalf("observer called %d times, want 1", count)
	}
}
