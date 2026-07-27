// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

// buildDefer: off defers activate and permits power->on; on permits
// activate->active. A flat machine, so deferral is exercised independently of
// hierarchy.
func buildDefer(t *testing.T, opts ...Option) *Config[pstate, ptrig, pev] {
	t.Helper()
	b := New[pstate, ptrig, pev](off, opts...)
	b.State(off).Defer(activate).Permit(power, on)
	b.State(on).Permit(activate, active)
	b.State(active)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cfg
}

func TestDeferReofferedOnStateChange(t *testing.T) {
	m := buildDefer(t).Instance()
	ctx := context.Background()

	// activate is deferred in off: accepted, no error, no state change.
	if err := m.Fire(ctx, activate, pev{}); err != nil {
		t.Fatalf("deferred activate returned %v, want nil", err)
	}
	if m.State() != off {
		t.Fatalf("state = %d, want off (deferred, not consumed)", m.State())
	}
	if got := m.Stats().Deferred; got != 1 {
		t.Fatalf("deferred counter = %d, want 1", got)
	}

	// power -> on changes state, so the deferred activate is re-offered and
	// consumed in on: activate -> active.
	if err := m.Fire(ctx, power, pev{}); err != nil {
		t.Fatalf("power: %v", err)
	}
	if m.State() != active {
		t.Fatalf("state = %d, want active (deferred activate re-offered)", m.State())
	}
	if got := m.Stats().Transitions; got != 2 {
		t.Fatalf("transitions = %d, want 2", got)
	}
}

func TestDeferHeldWithoutStateChange(t *testing.T) {
	// off defers activate and ignores knock-like triggers; firing a deferred
	// trigger then a non-transition must keep the deferred entry pending.
	b := New[pstate, ptrig, pev](off)
	b.State(off).
		Defer(activate).
		Internal(reset, func(_ context.Context, _ Transition[pstate, ptrig], _ pev) error { return nil }).
		Permit(power, on)
	b.State(on).Permit(activate, active)
	b.State(active)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m := cfg.Instance()
	ctx := context.Background()

	_ = m.Fire(ctx, activate, pev{}) // deferred
	_ = m.Fire(ctx, reset, pev{})    // internal: no state change, no re-offer
	if m.State() != off {
		t.Fatalf("state = %d, want off", m.State())
	}
	// Still deferred; only a state change releases it.
	_ = m.Fire(ctx, power, pev{})
	if m.State() != active {
		t.Fatalf("state = %d, want active after state change released defer", m.State())
	}
}

func TestDeferOverflow(t *testing.T) {
	m := buildDefer(t, WithDeferCapacity(1)).Instance()
	ctx := context.Background()

	if err := m.Fire(ctx, activate, pev{}); err != nil {
		t.Fatalf("first defer: %v", err)
	}
	err := m.Fire(ctx, activate, pev{}) // ring full
	if _, ok := errors.AsType[*DeferOverflowError[ptrig]](err); !ok {
		t.Fatalf("err = %v, want *DeferOverflowError", err)
	}
	if got := m.Stats().DeferOverflow; got != 1 {
		t.Fatalf("deferOverflow counter = %d, want 1", got)
	}
}

func TestTimeoutFires(t *testing.T) {
	base := time.Unix(1000, 0)
	clk := &fakeClock{t: base}
	var gotKind Kind
	b := New[pstate, ptrig, pev](off, WithClock(clk))
	b.State(off).Timeout(10*time.Second, power).Permit(power, on)
	b.State(on).OnEntry(func(_ context.Context, tr Transition[pstate, ptrig], _ pev) error {
		gotKind = tr.Kind
		return nil
	})
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m := cfg.Instance()
	ctx := context.Background()

	dl, ok := m.NextDeadline()
	if !ok || !dl.Equal(base.Add(10*time.Second)) {
		t.Fatalf("NextDeadline = (%v, %v), want (%v, true)", dl, ok, base.Add(10*time.Second))
	}

	// Tick before the deadline is a no-op.
	if err := m.Tick(ctx, base.Add(5*time.Second)); err != nil {
		t.Fatalf("early Tick: %v", err)
	}
	if m.State() != off {
		t.Fatalf("state = %d, want off (deadline not reached)", m.State())
	}

	// Tick at the deadline fires the timeout trigger with Kind Timeout.
	if err := m.Tick(ctx, base.Add(10*time.Second)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if m.State() != on {
		t.Fatalf("state = %d, want on", m.State())
	}
	if gotKind != Timeout {
		t.Fatalf("entry transition kind = %s, want timeout", gotKind)
	}
	if got := m.Stats().Timeouts; got != 1 {
		t.Fatalf("timeouts counter = %d, want 1", got)
	}
	if _, ok := m.NextDeadline(); ok {
		t.Fatal("on has no timeout; NextDeadline should be false")
	}
}

func TestTimeoutRearmsOnReentry(t *testing.T) {
	base := time.Unix(2000, 0)
	clk := &fakeClock{t: base}
	b := New[pstate, ptrig, pev](off, WithClock(clk))
	b.State(off).Timeout(10*time.Second, power).Permit(power, off) // self-transition
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m := cfg.Instance()

	// Advance the clock to the deadline, as the owner would before ticking.
	clk.t = base.Add(10 * time.Second)
	if err := m.Tick(context.Background(), clk.Now()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// The self-transition re-arms the timeout relative to the (advanced) clock.
	dl, ok := m.NextDeadline()
	if !ok || !dl.Equal(base.Add(20*time.Second)) {
		t.Fatalf("re-armed deadline = (%v, %v), want (%v, true)", dl, ok, base.Add(20*time.Second))
	}
	if got := m.Stats().Timeouts; got != 1 {
		t.Fatalf("timeouts = %d, want 1", got)
	}
}

func TestTimeoutOnCompositeIsBuildError(t *testing.T) {
	b := New[pstate, ptrig, pev](off)
	b.State(off).Permit(power, on)
	b.State(on).Initial(idle).Timeout(10*time.Second, reset)
	b.State(idle).Parent(on)
	if _, err := b.Build(); err == nil {
		t.Fatal("expected BuildError for timeout on composite state")
	} else if _, ok := errors.AsType[*BuildError](err); !ok {
		t.Fatalf("err = %v, want *BuildError", err)
	}
}
