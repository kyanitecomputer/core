// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// A small door machine used across tests.
type state uint8

const (
	closed state = iota
	open
	locked
)

type trigger uint8

const (
	openT trigger = iota
	closeT
	lockT
	unlockT
	knockT
)

type ev struct{ lockable bool }

func lockable() Guard[ev] {
	return Guard[ev]{Name: "lockable", Fn: func(_ context.Context, e ev) bool { return e.lockable }}
}

// doorConfig builds a door: closed<->open, closed->locked (guarded),
// locked->closed, locked ignores knock. Entry/exit actions record a trace.
func doorConfig(t *testing.T, trace *[]string) *Config[state, trigger, ev] {
	t.Helper()
	b := New[state, trigger, ev](closed, WithGuardNames())
	b.State(closed).
		Permit(openT, open).
		Permit(lockT, locked, lockable())
	b.State(open).
		OnEntry(func(_ context.Context, _ Transition[state, trigger], _ ev) error {
			*trace = append(*trace, "enter-open")
			return nil
		}).
		OnExit(func(_ context.Context, _ Transition[state, trigger], _ ev) error {
			*trace = append(*trace, "exit-open")
			return nil
		}).
		OnEntryFrom(openT, func(_ context.Context, _ Transition[state, trigger], _ ev) error {
			*trace = append(*trace, "enter-open-via-open")
			return nil
		}).
		Permit(closeT, closed)
	b.State(locked).
		Ignore(knockT).
		Permit(unlockT, closed)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cfg
}

func TestBasicTransitions(t *testing.T) {
	var trace []string
	m := doorConfig(t, &trace).Instance()
	ctx := context.Background()

	if err := m.Fire(ctx, openT, ev{}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if m.State() != open {
		t.Fatalf("state = %d, want open", m.State())
	}
	if err := m.Fire(ctx, closeT, ev{}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if m.State() != closed {
		t.Fatalf("state = %d, want closed", m.State())
	}
	want := []string{"enter-open", "enter-open-via-open", "exit-open"}
	if !slices.Equal(trace, want) {
		t.Fatalf("action trace = %v, want %v", trace, want)
	}
}

func TestUnhandled(t *testing.T) {
	var trace []string
	m := doorConfig(t, &trace).Instance()
	err := m.Fire(context.Background(), closeT, ev{}) // closed has no closeT
	if _, ok := errors.AsType[*UnhandledError[state, trigger]](err); !ok {
		t.Fatalf("err = %v, want *UnhandledError", err)
	}
	if m.Stats().Unhandled != 1 {
		t.Fatalf("unhandled counter = %d, want 1", m.Stats().Unhandled)
	}
}

func TestIgnore(t *testing.T) {
	var trace []string
	m := doorConfig(t, &trace).Instance()
	ctx := context.Background()
	_ = m.Fire(ctx, lockT, ev{lockable: true}) // -> locked
	if m.State() != locked {
		t.Fatalf("state = %d, want locked", m.State())
	}
	if err := m.Fire(ctx, knockT, ev{}); err != nil {
		t.Fatalf("ignored knock returned %v, want nil", err)
	}
	if m.State() != locked {
		t.Fatalf("ignore changed state to %d", m.State())
	}
}

func TestGuardRejected(t *testing.T) {
	var trace []string
	m := doorConfig(t, &trace).Instance()
	err := m.Fire(context.Background(), lockT, ev{lockable: false})
	gre, ok := errors.AsType[*GuardsRejectedError[state, trigger]](err)
	if !ok {
		t.Fatalf("err = %v, want *GuardsRejectedError", err)
	}
	if !slices.Contains(gre.Guards, "lockable") {
		t.Fatalf("rejected guards = %v, want to contain lockable", gre.Guards)
	}
	if m.State() != closed {
		t.Fatalf("state changed to %d on rejected guard", m.State())
	}
}

func TestGuardSelectedCandidate(t *testing.T) {
	// closed: openT -> locked if lockable, else -> open (unguarded, last).
	b := New[state, trigger, ev](closed)
	b.State(closed).
		Permit(openT, locked, lockable()).
		Permit(openT, open)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	m := cfg.Instance()
	_ = m.Fire(context.Background(), openT, ev{lockable: false})
	if m.State() != open {
		t.Fatalf("state = %d, want open (fell through to unguarded)", m.State())
	}

	m2 := cfg.Instance()
	_ = m2.Fire(context.Background(), openT, ev{lockable: true})
	if m2.State() != locked {
		t.Fatalf("state = %d, want locked (guard passed)", m2.State())
	}
}

func TestInternalAndReentry(t *testing.T) {
	var hits []string
	b := New[state, trigger, ev](open)
	b.State(open).
		OnEntry(func(_ context.Context, _ Transition[state, trigger], _ ev) error {
			hits = append(hits, "entry")
			return nil
		}).
		OnExit(func(_ context.Context, _ Transition[state, trigger], _ ev) error {
			hits = append(hits, "exit")
			return nil
		}).
		Internal(knockT, func(_ context.Context, tr Transition[state, trigger], _ ev) error {
			hits = append(hits, "internal")
			if tr.Kind != Internal {
				t.Errorf("internal action kind = %s", tr.Kind)
			}
			return nil
		}).
		Reentry(openT)
	cfg, _ := b.Build()
	m := cfg.Instance()
	ctx := context.Background()

	_ = m.Fire(ctx, knockT, ev{}) // internal: no exit/entry
	_ = m.Fire(ctx, openT, ev{})  // reentry: exit + entry
	if m.State() != open {
		t.Fatalf("state = %d, want open", m.State())
	}
	want := []string{"internal", "exit", "entry"}
	if !slices.Equal(hits, want) {
		t.Fatalf("hits = %v, want %v", hits, want)
	}
}

func TestRunToCompletion(t *testing.T) {
	// Entering open immediately fires closeT (reentrant); it must be queued and
	// processed after the current transition, landing back in closed.
	var m *Instance[state, trigger, ev]
	b := New[state, trigger, ev](closed)
	b.State(closed).Permit(openT, open)
	b.State(open).
		OnEntry(func(ctx context.Context, _ Transition[state, trigger], e ev) error {
			return m.Fire(ctx, closeT, e) // reentrant → queued
		}).
		Permit(closeT, closed)
	cfg, _ := b.Build()
	m = cfg.Instance()

	if err := m.Fire(context.Background(), openT, ev{}); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if m.State() != closed {
		t.Fatalf("state = %d, want closed (RTC drained the queued closeT)", m.State())
	}
	if got := m.Stats().Transitions; got != 2 {
		t.Fatalf("transitions = %d, want 2", got)
	}
}

func TestActionError(t *testing.T) {
	boom := errors.New("boom")
	b := New[state, trigger, ev](closed)
	b.State(closed).Permit(openT, open)
	b.State(open).OnEntry(func(_ context.Context, _ Transition[state, trigger], _ ev) error {
		return boom
	})
	cfg, _ := b.Build()
	m := cfg.Instance()

	err := m.Fire(context.Background(), openT, ev{})
	ae, ok := errors.AsType[*ActionError[state, trigger]](err)
	if !ok {
		t.Fatalf("err = %v, want *ActionError", err)
	}
	if !errors.Is(ae, boom) {
		t.Fatalf("ActionError does not wrap boom: %v", err)
	}
}

func TestInstancesIndependent(t *testing.T) {
	var trace []string
	cfg := doorConfig(t, &trace)
	a := cfg.Instance()
	b := cfg.Instance()
	ctx := context.Background()

	_ = a.Fire(ctx, openT, ev{})
	if a.State() != open || b.State() != closed {
		t.Fatalf("instances not independent: a=%d b=%d", a.State(), b.State())
	}
}

func TestBuildErrors(t *testing.T) {
	// Two unguarded candidates for the same (state, trigger).
	b := New[state, trigger, ev](closed)
	b.State(closed).Permit(openT, open).Permit(openT, locked)
	if _, err := b.Build(); err == nil {
		t.Fatal("expected BuildError for duplicate unguarded transitions")
	} else if _, ok := errors.AsType[*BuildError](err); !ok {
		t.Fatalf("err = %v, want *BuildError", err)
	}

	// Unguarded candidate not declared last.
	b2 := New[state, trigger, ev](closed)
	b2.State(closed).Permit(openT, open).Permit(openT, locked, lockable())
	if _, err := b2.Build(); err == nil {
		t.Fatal("expected BuildError for unguarded transition not last")
	}
}

func TestPermitted(t *testing.T) {
	var trace []string
	m := doorConfig(t, &trace).Instance()
	// In closed with lockable=true, both openT and lockT are permitted.
	var got []trigger
	for tr := range m.Permitted(context.Background(), ev{lockable: true}) {
		got = append(got, tr)
	}
	if !slices.Contains(got, openT) || !slices.Contains(got, lockT) {
		t.Fatalf("permitted = %v, want to contain openT and lockT", got)
	}
	if slices.Contains(got, closeT) {
		t.Fatalf("permitted = %v, should not contain closeT in closed", got)
	}
}

func TestQueueOverflow(t *testing.T) {
	// A tiny queue; an entry action that fires many reentrant triggers overflows.
	var m *Instance[state, trigger, ev]
	overflowed := false
	b := New[state, trigger, ev](closed, WithQueueCapacity(1))
	b.State(closed).Permit(openT, open)
	b.State(open).
		OnEntry(func(ctx context.Context, _ Transition[state, trigger], e ev) error {
			// Queue is depth 1: first reentrant fire enqueues, second overflows.
			_ = m.Fire(ctx, knockT, e)
			if err := m.Fire(ctx, knockT, e); err != nil {
				if _, ok := errors.AsType[*QueueOverflowError[trigger]](err); ok {
					overflowed = true
				}
			}
			return nil
		}).
		Internal(knockT, func(_ context.Context, _ Transition[state, trigger], _ ev) error { return nil })
	cfg, _ := b.Build()
	m = cfg.Instance()
	_ = m.Fire(context.Background(), openT, ev{})
	if !overflowed {
		t.Fatal("expected QueueOverflowError on the second reentrant fire")
	}
}
