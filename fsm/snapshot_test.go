// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"context"
	"testing"
)

func TestSnapshotRestoreRoundtrip(t *testing.T) {
	var trace []string
	cfg := doorConfig(t, &trace)
	ctx := context.Background()

	m := cfg.Instance()
	_ = m.Fire(ctx, openT, ev{}) // -> open
	snap := m.Snapshot()
	if snap.State != open {
		t.Fatalf("snapshot state = %d, want open", snap.State)
	}

	restored := cfg.Instance()
	if restored.State() != closed {
		t.Fatalf("fresh instance state = %d, want closed", restored.State())
	}
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.State() != open {
		t.Fatalf("restored state = %d, want open", restored.State())
	}
	// The restored instance behaves normally.
	if err := restored.Fire(ctx, closeT, ev{}); err != nil {
		t.Fatalf("fire after restore: %v", err)
	}
	if restored.State() != closed {
		t.Fatalf("state after close = %d, want closed", restored.State())
	}
}

func TestSnapshotRestoreDeferred(t *testing.T) {
	cfg := buildDefer(t)
	ctx := context.Background()

	m := cfg.Instance()
	_ = m.Fire(ctx, activate, pev{}) // deferred in off
	snap := m.Snapshot()
	if len(snap.Deferred) != 1 || snap.Deferred[0] != uint8(activate) {
		t.Fatalf("snapshot deferred = %v, want [%d]", snap.Deferred, activate)
	}

	restored := cfg.Instance()
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// The restored deferred trigger is re-offered on the next state change.
	_ = restored.Fire(ctx, power, pev{})
	if restored.State() != active {
		t.Fatalf("state = %d, want active (deferred activate re-offered)", restored.State())
	}
}

func TestRestoreVersionMismatch(t *testing.T) {
	var trace []string
	door := doorConfig(t, &trace)

	// A different topology over the same state/trigger types.
	b := New[state, trigger, ev](closed)
	b.State(closed).Permit(openT, open)
	b.State(open).Permit(closeT, locked) // differs from door
	b.State(locked)
	other, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if door.version == other.version {
		t.Fatal("distinct topologies produced equal version hashes")
	}

	snap := door.Instance().Snapshot()
	if err := other.Instance().Restore(snap); err != ErrSnapshotVersion {
		t.Fatalf("Restore across topologies err = %v, want ErrSnapshotVersion", err)
	}
}

func TestRestoreUnknownState(t *testing.T) {
	var trace []string
	cfg := doorConfig(t, &trace)
	snap := cfg.Instance().Snapshot()
	snap.State = 99 // out of range
	if err := cfg.Instance().Restore(snap); err != ErrUnknownState {
		t.Fatalf("Restore out-of-range state err = %v, want ErrUnknownState", err)
	}
}
