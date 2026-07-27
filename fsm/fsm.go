// SPDX-License-Identifier: BSD-3-Clause

// Package fsm is a generic, compiled, allocation-free finite state machine for
// embedded Go (TamaGo bare metal and constrained userland). It adopts the UML
// statechart semantics of qmuntal/stateless — run-to-completion firing, guards,
// entry/exit actions — but rejects its representation: states and triggers are
// small integer enums (no boxing, no reflect), configuration is validated and
// lowered once into a dense transition table by Build, and input-shaped
// failures are typed errors rather than panics.
//
// # Config / Instance split
//
// [Build] produces an immutable [Config] — the dense table, shared read-only.
// Each running machine is a tiny [Instance] created with [Config.Instance],
// holding only the current state, a fixed run-to-completion ring, and counters.
// Hundreds of per-session machines (IPMI/SPDM sessions, per-port protocol
// state) therefore share one table with negligible per-instance cost.
//
// # Ownership
//
// An Instance is single-owner: it must be driven from one goroutine (in
// practice a supervise child). Cross-goroutine input arrives through that
// owner, not through locking; the library has no mutexes on the fire path. The
// exported counters are atomic so a second goroutine may scrape them.
//
// This slice provides flat (non-hierarchical) machines: external / internal /
// reentry / ignore transitions, guards, entry/exit/trigger-specific-entry
// actions, and the run-to-completion queue. Hierarchy, deferral, state
// timeouts, snapshots, and diagram export follow in later slices.
package fsm

import (
	"context"
	"iter"
	"sync/atomic"
)

// Kind classifies a transition for the Transition value passed to actions.
type Kind uint8

const (
	// External is a transition to a (possibly different) state that runs exit
	// then entry actions.
	External Kind = iota
	// Internal is a reaction that runs an action without leaving the state (no
	// exit/entry).
	Internal
	// Reentry re-enters the current state, running exit then entry actions.
	Reentry
	// Initial is an initial transition into a substate (later slice).
	Initial
	// Timeout is a transition driven by a state timeout (later slice).
	Timeout
)

// String returns the kind name.
func (k Kind) String() string {
	switch k {
	case External:
		return "external"
	case Internal:
		return "internal"
	case Reentry:
		return "reentry"
	case Initial:
		return "initial"
	case Timeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// Transition describes a transition presented to an action.
type Transition[S ~uint8, T ~uint8] struct {
	Source      S
	Destination S
	Trigger     T
	Kind        Kind
}

// String renders the transition compactly, e.g. "3-(2)->5 external".
func (t Transition[S, T]) String() string {
	return sprintTransition(uint8(t.Source), uint8(t.Trigger), uint8(t.Destination), t.Kind)
}

// Guard is a named, side-effect-free predicate evaluated at fire time. The name
// is explicit (no reflection) and used only for diagnostics when guard-name
// collection is enabled.
type Guard[E any] struct {
	Name string
	Fn   func(ctx context.Context, ev E) bool
}

// Action runs on a transition (entry, exit, transition, or internal). It
// receives the transition and the event payload and may return an error, which
// is wrapped in an [ActionError].
type Action[S ~uint8, T ~uint8, E any] func(ctx context.Context, tr Transition[S, T], ev E) error

// candidateKind is the internal transition classification stored per cell.
type candidateKind uint8

const (
	ckExternal candidateKind = iota
	ckInternal
	ckReentry
	ckIgnore
)

// candidate is one possible transition for a (state, trigger) pair.
type candidate[S ~uint8, T ~uint8, E any] struct {
	kind   candidateKind
	dst    S
	guards []Guard[E]
	action Action[S, T, E]
}

// cell holds the ordered candidates for a (state, trigger) pair. Candidates are
// evaluated in declaration order; the first whose guards all pass fires.
type cell[S ~uint8, T ~uint8, E any] struct {
	candidates []candidate[S, T, E]
}

// stateInfo holds the entry/exit actions for a state.
type stateInfo[S ~uint8, T ~uint8, E any] struct {
	onEntry     []Action[S, T, E]
	onExit      []Action[S, T, E]
	onEntryFrom map[T]Action[S, T, E]
}

// Config is an immutable, compiled machine definition. Create instances from it
// with [Config.Instance]; it is safe to share read-only across many instances
// and goroutines.
type Config[S ~uint8, T ~uint8, E any] struct {
	initial     S
	numStates   int
	numTriggers int
	table       []cell[S, T, E] // len numStates*numTriggers
	states      []stateInfo[S, T, E]
	queueCap    int
	guardNames  bool
}

// Instance returns a fresh machine instance positioned at the configured
// initial state.
func (c *Config[S, T, E]) Instance() *Instance[S, T, E] {
	return &Instance[S, T, E]{
		cfg:   c,
		state: c.initial,
		queue: make([]queued[T, E], c.queueCap),
	}
}

// Counters is a snapshot of an instance's activity counters.
type Counters struct {
	Fires         uint64
	Transitions   uint64
	Rejected      uint64
	Unhandled     uint64
	QueueOverflow uint64
}

type counters struct {
	fires         atomic.Uint64
	transitions   atomic.Uint64
	rejected      atomic.Uint64
	unhandled     atomic.Uint64
	queueOverflow atomic.Uint64
}

type queued[T ~uint8, E any] struct {
	t  T
	ev E
}

// Instance is a running machine. It is single-owner: drive Fire/Tick from one
// goroutine. Create it with [Config.Instance].
type Instance[S ~uint8, T ~uint8, E any] struct {
	cfg   *Config[S, T, E]
	state S

	firing bool
	queue  []queued[T, E]
	qhead  int
	qlen   int

	c counters
}

// State returns the current state.
func (i *Instance[S, T, E]) State() S { return i.state }

// Stats returns a snapshot of the instance counters.
func (i *Instance[S, T, E]) Stats() Counters {
	return Counters{
		Fires:         i.c.fires.Load(),
		Transitions:   i.c.transitions.Load(),
		Rejected:      i.c.rejected.Load(),
		Unhandled:     i.c.unhandled.Load(),
		QueueOverflow: i.c.queueOverflow.Load(),
	}
}

func (c *Config[S, T, E]) at(s S, t T) *cell[S, T, E] {
	si, ti := int(s), int(t)
	if si >= c.numStates || ti >= c.numTriggers {
		return nil
	}
	return &c.table[si*c.numTriggers+ti]
}

// Fire delivers trigger t with event ev. If called from within an action
// (reentrantly, same goroutine), the trigger is queued and processed after the
// current transition completes (run-to-completion); otherwise it is processed
// immediately and any triggers queued during it are drained before returning.
//
// It returns the first error produced: [UnhandledError] if the trigger has no
// transition, [GuardsRejectedError] if all candidates' guards reject it,
// [ActionError] wrapping an action failure, or [QueueOverflowError] if a
// reentrant fire overflows the queue. A nil return means the trigger (and any
// it queued) were handled.
func (i *Instance[S, T, E]) Fire(ctx context.Context, t T, ev E) error {
	if i.firing {
		if i.qlen >= len(i.queue) {
			i.c.queueOverflow.Add(1)
			return &QueueOverflowError[T]{Trigger: t, Capacity: len(i.queue)}
		}
		i.queue[(i.qhead+i.qlen)%len(i.queue)] = queued[T, E]{t: t, ev: ev}
		i.qlen++
		return nil
	}

	i.firing = true
	err := i.fireOne(ctx, t, ev)
	for i.qlen > 0 {
		q := i.queue[i.qhead]
		i.qhead = (i.qhead + 1) % len(i.queue)
		i.qlen--
		if e := i.fireOne(ctx, q.t, q.ev); e != nil && err == nil {
			err = e
		}
	}
	i.firing = false
	return err
}

func (i *Instance[S, T, E]) fireOne(ctx context.Context, t T, ev E) error {
	i.c.fires.Add(1)
	cl := i.cfg.at(i.state, t)
	if cl == nil || len(cl.candidates) == 0 {
		i.c.unhandled.Add(1)
		return &UnhandledError[S, T]{State: i.state, Trigger: t}
	}

	var rejected []string
	for idx := range cl.candidates {
		cand := &cl.candidates[idx]
		if !i.guardsPass(ctx, cand.guards, ev, &rejected) {
			continue
		}
		switch cand.kind {
		case ckIgnore:
			return nil
		case ckInternal:
			tr := Transition[S, T]{Source: i.state, Destination: i.state, Trigger: t, Kind: Internal}
			return i.runAction(ctx, cand.action, tr, ev)
		case ckReentry:
			tr := Transition[S, T]{Source: i.state, Destination: i.state, Trigger: t, Kind: Reentry}
			if err := i.runExit(ctx, tr, ev); err != nil {
				return err
			}
			if err := i.runAction(ctx, cand.action, tr, ev); err != nil {
				return err
			}
			if err := i.runEntry(ctx, i.state, tr, ev); err != nil {
				return err
			}
			i.c.transitions.Add(1)
			return nil
		default: // ckExternal
			tr := Transition[S, T]{Source: i.state, Destination: cand.dst, Trigger: t, Kind: External}
			if err := i.runExit(ctx, tr, ev); err != nil {
				return err
			}
			if err := i.runAction(ctx, cand.action, tr, ev); err != nil {
				return err
			}
			i.state = cand.dst
			if err := i.runEntry(ctx, cand.dst, tr, ev); err != nil {
				return err
			}
			i.c.transitions.Add(1)
			return nil
		}
	}
	i.c.rejected.Add(1)
	return &GuardsRejectedError[S, T]{State: i.state, Trigger: t, Guards: rejected}
}

func (i *Instance[S, T, E]) guardsPass(ctx context.Context, guards []Guard[E], ev E, rejected *[]string) bool {
	for _, g := range guards {
		if !g.Fn(ctx, ev) {
			if i.cfg.guardNames && g.Name != "" {
				*rejected = append(*rejected, g.Name)
			}
			return false
		}
	}
	return true
}

func (i *Instance[S, T, E]) runExit(ctx context.Context, tr Transition[S, T], ev E) error {
	for _, a := range i.cfg.states[i.state].onExit {
		if err := i.runAction(ctx, a, tr, ev); err != nil {
			return err
		}
	}
	return nil
}

func (i *Instance[S, T, E]) runEntry(ctx context.Context, dst S, tr Transition[S, T], ev E) error {
	si := i.cfg.states[dst]
	for _, a := range si.onEntry {
		if err := i.runAction(ctx, a, tr, ev); err != nil {
			return err
		}
	}
	if si.onEntryFrom != nil {
		if a, ok := si.onEntryFrom[tr.Trigger]; ok {
			if err := i.runAction(ctx, a, tr, ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *Instance[S, T, E]) runAction(ctx context.Context, a Action[S, T, E], tr Transition[S, T], ev E) error {
	if a == nil {
		return nil
	}
	if err := a(ctx, tr, ev); err != nil {
		return &ActionError[S, T]{Transition: tr, Err: err}
	}
	return nil
}

// CanFire reports whether trigger t would be accepted now: a candidate exists
// and its guards pass. It has no side effects.
func (i *Instance[S, T, E]) CanFire(ctx context.Context, t T, ev E) bool {
	cl := i.cfg.at(i.state, t)
	if cl == nil {
		return false
	}
	for idx := range cl.candidates {
		if i.guardsPassNoRecord(ctx, cl.candidates[idx].guards, ev) {
			return true
		}
	}
	return false
}

func (i *Instance[S, T, E]) guardsPassNoRecord(ctx context.Context, guards []Guard[E], ev E) bool {
	for _, g := range guards {
		if !g.Fn(ctx, ev) {
			return false
		}
	}
	return true
}

// Permitted yields, in trigger order, every trigger that would be accepted now
// (a candidate exists and its guards pass).
func (i *Instance[S, T, E]) Permitted(ctx context.Context, ev E) iter.Seq[T] {
	return func(yield func(T) bool) {
		for t := 0; t < i.cfg.numTriggers; t++ {
			trig := T(t)
			if i.CanFire(ctx, trig, ev) {
				if !yield(trig) {
					return
				}
			}
		}
	}
}
