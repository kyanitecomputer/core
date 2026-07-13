// SPDX-License-Identifier: BSD-3-Clause

package supervise

import (
	"context"
	"time"

	"src.kyanite.computer/core/supervise/internal/ring"
)

// Spec describes a supervised unit of work. Start is the entire contract: run
// until done or until ctx is cancelled, then return the reason (nil for a clean
// completion). There are no error channels and no interface to implement;
// dependencies are captured by the closure.
type Spec struct {
	// Name identifies the child in logs, events, and Stop. It should be unique
	// within a supervisor.
	Name string
	// Start runs the child. It must return promptly once ctx is cancelled.
	Start func(ctx context.Context) error
	// Restart is the restart policy. The zero value is Permanent.
	Restart Restart
	// Shutdown is the grace period between cancellation and declaring the child
	// Abandoned. Zero uses the supervisor default.
	Shutdown time.Duration
	// Backoff overrides the restart backoff for this child. The zero value uses
	// the supervisor default.
	Backoff Backoff
	// Critical, when true, escalates (fails the supervisor) if this child is
	// abandoned, instead of merely logging it.
	Critical bool
}

// childID is a stable per-supervisor identifier for a child record.
type childID uint64

// childState is the lifecycle state of a child, owned by the supervisor loop.
type childState uint8

const (
	// stIdle: registered, not yet spawned.
	stIdle childState = iota
	// stRunning: a guard goroutine is executing Start.
	stRunning
	// stBackoff: exited and scheduled to respawn after a delay.
	stBackoff
	// stStopping: cancelled, within its grace period, awaiting exit.
	stStopping
	// stAbandoned: grace elapsed while still running; no longer waited on.
	stAbandoned
	// stDone: terminal, will not restart (Temporary, or clean Transient).
	stDone
)

func (c childState) String() string {
	switch c {
	case stIdle:
		return "idle"
	case stRunning:
		return "running"
	case stBackoff:
		return "backoff"
	case stStopping:
		return "stopping"
	case stAbandoned:
		return "abandoned"
	case stDone:
		return "done"
	default:
		return "unknown"
	}
}

// child is the supervisor's mutable record for one supervised unit. All fields
// are read and written only by the supervisor loop goroutine.
type child struct {
	id          childID
	spec        Spec
	state       childState
	incarnation uint64 // bumped on each (re)spawn; stale exits are ignored

	cancel context.CancelCauseFunc // cancels the current incarnation

	backoff  time.Duration         // last backoff delay (for decorrelated jitter)
	restarts *ring.Ring[time.Time] // restart timestamps for intensity

	dueAt      time.Time // respawn deadline when stBackoff (0 otherwise)
	graceUntil time.Time // abandonment deadline when stStopping (0 otherwise)
}

// active reports whether the supervisor is still waiting on this child (either
// a guard is running or a respawn is pending).
func (c *child) active() bool {
	return c.state == stRunning || c.state == stBackoff || c.state == stStopping
}
