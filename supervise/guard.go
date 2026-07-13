// SPDX-License-Identifier: BSD-3-Clause

package supervise

import (
	"context"
	"runtime/debug"
	"runtime/pprof"
)

// exitMsg is sent from a guard's deferred epilogue to the supervisor loop when
// a child returns, panics, or exits via Goexit. It carries the incarnation so
// the loop can drop stale messages from a previous incarnation.
type exitMsg struct {
	id          childID
	incarnation uint64
	err         error
}

// spawn starts a child's guard goroutine. It is the single goroutine-creation
// site in the package (grep-enforceable): every supervised goroutine is born
// here, carries pprof labels for leak attribution, and reports exactly one exit
// through the mailbox.
//
// The guard converts all four abnormal-exit shapes into a typed error routed
// through one path:
//   - a normal error return,
//   - a panic (→ *PanicError with the stack at the recover point),
//   - runtime.Goexit (→ *GoexitError, detected via the completed flag),
//   - an optional memory fault (→ panic, if WithPanicOnFault is set).
func (s *Supervisor) spawn(c *child) {
	ctx, cancel := context.WithCancelCause(s.ctx)
	c.cancel = cancel
	c.incarnation++
	c.state = stRunning
	inc := c.incarnation
	id := c.id
	name := c.spec.Name
	start := c.spec.Start
	panicOnFault := s.panicOnFault

	s.emit(Event{Kind: EventStarted, Tree: s.path, Child: name, At: s.now()})

	go func() {
		pprof.Do(ctx, pprof.Labels(
			labelTree, s.path,
			labelChild, name,
		), func(ctx context.Context) {
			var err error
			completed := false
			defer func() {
				if r := recover(); r != nil {
					err = &PanicError{Child: name, Value: r, Stack: debug.Stack()}
				} else if !completed {
					// recover() == nil yet Start never returned: runtime.Goexit
					// is unwinding. Defers still run; report before the
					// goroutine dies.
					err = &GoexitError{Child: name, Stack: debug.Stack()}
				}
				select {
				case s.exitCh <- exitMsg{id: id, incarnation: inc, err: err}:
				case <-s.done:
				}
			}()
			if panicOnFault {
				old := debug.SetPanicOnFault(true)
				defer debug.SetPanicOnFault(old)
			}
			err = start(ctx)
			completed = true
		})
	}()
}

// pprof label keys applied to every supervised goroutine, so a goroutine or
// goroutineleak profile attributes any leak to an exact tree path and child.
const (
	labelTree  = "supervise.tree"
	labelChild = "supervise.child"
)
