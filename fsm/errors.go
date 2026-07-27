// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"fmt"
	"strings"
)

// UnhandledError reports that no transition is configured for a trigger in the
// current state. It is an ordinary input-shaped failure, not a panic: an
// unexpected trigger is data, not a bug. Extract with
// errors.AsType[*UnhandledError[S,T]].
type UnhandledError[S ~uint8, T ~uint8] struct {
	State   S
	Trigger T
}

func (e *UnhandledError[S, T]) Error() string {
	return fmt.Sprintf("fsm: trigger %d unhandled in state %d", uint8(e.Trigger), uint8(e.State))
}

// GuardsRejectedError reports that a trigger is configured for the current
// state but every candidate transition's guard(s) rejected it. Guards holds the
// names of the guards that failed, when guard-name collection is enabled.
type GuardsRejectedError[S ~uint8, T ~uint8] struct {
	State   S
	Trigger T
	Guards  []string
}

func (e *GuardsRejectedError[S, T]) Error() string {
	if len(e.Guards) == 0 {
		return fmt.Sprintf("fsm: trigger %d rejected by guards in state %d", uint8(e.Trigger), uint8(e.State))
	}
	return fmt.Sprintf("fsm: trigger %d rejected by guards %s in state %d",
		uint8(e.Trigger), strings.Join(e.Guards, ","), uint8(e.State))
}

// QueueOverflowError reports that a trigger fired reentrantly (from within an
// action) could not be enqueued because the run-to-completion queue was full.
type QueueOverflowError[T ~uint8] struct {
	Trigger  T
	Capacity int
}

func (e *QueueOverflowError[T]) Error() string {
	return fmt.Sprintf("fsm: run-to-completion queue full (cap %d), dropped trigger %d",
		e.Capacity, uint8(e.Trigger))
}

// ActionError wraps an error returned by an entry, exit, transition, or
// internal action, tagged with the transition it occurred on.
type ActionError[S ~uint8, T ~uint8] struct {
	Transition Transition[S, T]
	Err        error
}

func (e *ActionError[S, T]) Error() string {
	return fmt.Sprintf("fsm: action failed on %s: %v", e.Transition, e.Err)
}

func (e *ActionError[S, T]) Unwrap() error { return e.Err }

// BuildError reports one or more problems found while compiling a machine
// (duplicate unguarded transitions, an unguarded transition that is not last,
// and similar). It is returned by Build; a compiled machine can never produce
// it, and after Build the fire path cannot panic.
type BuildError struct {
	Issues []string
}

func (e *BuildError) Error() string {
	return "fsm: build failed: " + strings.Join(e.Issues, "; ")
}

// sprintTransition formats a transition compactly for diagnostics.
func sprintTransition(src, trig, dst uint8, k Kind) string {
	return fmt.Sprintf("%d-(%d)->%d %s", src, trig, dst, k)
}
