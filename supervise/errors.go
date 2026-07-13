// SPDX-License-Identifier: BSD-3-Clause

package supervise

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel causes attached to a child's context cancellation, retrievable via
// [context.Cause]. They are the supervision verdict for the cancelled child.
var (
	// ErrShutdown is the cause when a child is cancelled because its supervisor
	// is shutting down (parent context cancelled or Run returning).
	ErrShutdown = errors.New("supervise: supervisor shutting down")
	// ErrStopped is the cause when a child is cancelled by an explicit Stop.
	ErrStopped = errors.New("supervise: stopped by request")
	// ErrRestart is the cause when a child is cancelled to be restarted by the
	// restart engine.
	ErrRestart = errors.New("supervise: restarted by strategy")
)

// PanicError wraps a value recovered from a panicking child, capturing the
// stack at the recover point. Extract it with errors.AsType[*PanicError].
type PanicError struct {
	Child string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("supervise: child %q panicked: %v", e.Child, e.Value)
}

// GoexitError reports that a child goroutine exited via runtime.Goexit (for
// example a t.FailNow in a child, or an explicit Goexit) rather than returning.
// Such an exit cannot be prevented, but it is reported instead of being
// mistaken for a clean return.
type GoexitError struct {
	Child string
	Stack []byte
}

func (e *GoexitError) Error() string {
	return fmt.Sprintf("supervise: child %q exited via runtime.Goexit", e.Child)
}

// AbandonedError reports that a child did not honor cancellation within its
// shutdown grace period. The goroutine is still accounted for (a later exit is
// reaped and logged) but the supervisor stopped waiting for it.
type AbandonedError struct {
	Child   string
	Elapsed time.Duration
}

func (e *AbandonedError) Error() string {
	return fmt.Sprintf("supervise: child %q abandoned after %s grace", e.Child, e.Elapsed)
}

// IntensityError reports that a child exceeded the restart-intensity window,
// which fails the supervisor (escalating up the tree in nested supervisors).
type IntensityError struct {
	Child    string
	Restarts int
	Window   time.Duration
}

func (e *IntensityError) Error() string {
	return fmt.Sprintf("supervise: child %q exceeded %d restarts within %s",
		e.Child, e.Restarts, e.Window)
}

// SiblingFailure is the cancellation cause given to siblings taken down by a
// OneForAll or RestForOne strategy because another child failed.
type SiblingFailure struct {
	Sibling string
	Err     error
}

func (e SiblingFailure) Error() string {
	return fmt.Sprintf("supervise: cancelled due to sibling %q failure: %v", e.Sibling, e.Err)
}

func (e SiblingFailure) Unwrap() error { return e.Err }
