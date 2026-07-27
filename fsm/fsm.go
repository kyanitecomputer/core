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
// # Hierarchy
//
// States form a tree via [StateCfg.Parent]. A trigger fired in a leaf state is
// resolved by walking up the ancestor chain: the innermost state with an
// enabled (guards passing) candidate handles it (behavioral inheritance). An
// external transition exits from the active leaf up to — but not including —
// the least common ancestor of the handler and destination, then enters down
// to the destination, following [StateCfg.Initial] transitions to a resting
// leaf. [Instance.IsIn] is substate-aware: it reports true for the current leaf
// and every ancestor.
//
// This slice provides flat and hierarchical machines: external / internal /
// reentry / ignore transitions, guards, entry/exit/trigger-specific-entry
// actions, initial transitions, and the run-to-completion queue. Deferral,
// state timeouts, snapshots, and diagram export follow in later slices.
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
	// Initial is an initial transition into a substate, run while descending
	// into a composite state after its entry actions.
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

	// Hierarchy, computed once by Build. parent[s] is the parent state index or
	// -1 for a root; initialSub[s] is the initial substate index or -1 for a
	// simple (leaf) state. depth[s] is the number of hops to a root. ancestors
	// is a per-state bitset (wordsPerState uint64 words each) with a bit set for
	// the state itself and every ancestor, backing substate-aware IsIn.
	parent        []int
	depth         []int
	initialSub    []int
	ancestors     []uint64
	wordsPerState int
	maxDepth      int
}

// Instance returns a fresh machine instance positioned at the configured
// initial state, descending initial transitions to a resting leaf. It runs no
// entry actions: an instance is positioned, not activated. The owner drives all
// behavior through Fire.
func (c *Config[S, T, E]) Instance() *Instance[S, T, E] {
	st := int(c.initial)
	for c.initialSub[st] >= 0 {
		st = c.initialSub[st]
	}
	return &Instance[S, T, E]{
		cfg:     c,
		state:   S(st),
		queue:   make([]queued[T, E], c.queueCap),
		pathBuf: make([]S, c.maxDepth+1),
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

	// pathBuf is a scratch buffer for the enter-down walk, sized to the tree
	// height at Instance creation so the fire path allocates nothing.
	pathBuf []S

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
	if int(t) >= i.cfg.numTriggers {
		i.c.unhandled.Add(1)
		return &UnhandledError[S, T]{State: i.state, Trigger: t}
	}

	// Resolve the handler by walking up the ancestor chain. The innermost state
	// with an enabled (guards-passing) candidate wins; a guard-rejected inner
	// candidate does not shadow an enabled ancestor.
	var rejected []string
	sawCandidate := false
	for h := int(i.state); ; {
		cl := i.cfg.cell(h, int(t))
		if len(cl.candidates) > 0 {
			sawCandidate = true
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
					return i.doTransition(ctx, h, h, t, ev, true, cand.action)
				default: // ckExternal
					return i.doTransition(ctx, h, int(cand.dst), t, ev, false, cand.action)
				}
			}
		}
		p := i.cfg.parent[h]
		if p < 0 {
			break
		}
		h = p
	}
	if !sawCandidate {
		i.c.unhandled.Add(1)
		return &UnhandledError[S, T]{State: i.state, Trigger: t}
	}
	i.c.rejected.Add(1)
	return &GuardsRejectedError[S, T]{State: i.state, Trigger: t, Guards: rejected}
}

// doTransition performs an external or reentry transition whose handler is
// state src and whose target is dst. It exits from the active leaf up to (but
// not including) the boundary, runs the transition action, enters down to dst,
// then follows initial transitions to a resting leaf. For an external
// transition the boundary is the least common ancestor of src and dst; for a
// reentry it is the parent of src, so src itself is exited and re-entered.
func (i *Instance[S, T, E]) doTransition(ctx context.Context, src, dst int, t T, ev E, reentry bool, action Action[S, T, E]) error {
	kind := External
	boundary := i.cfg.lcaOf(src, dst)
	if reentry {
		kind = Reentry
		boundary = i.cfg.parent[src]
	}
	tr := Transition[S, T]{Source: i.state, Destination: S(dst), Trigger: t, Kind: kind}

	// Exit from the active leaf upward, bottom-up, stopping below the boundary.
	for s := int(i.state); s != boundary; {
		if err := i.runExitState(ctx, S(s), tr, ev); err != nil {
			return err
		}
		p := i.cfg.parent[s]
		if p < 0 {
			break
		}
		s = p
	}

	if err := i.runAction(ctx, action, tr, ev); err != nil {
		return err
	}

	// Collect the entry path (dst up to just below the boundary), then enter
	// top-down.
	n := 0
	for s := dst; s != boundary; {
		i.pathBuf[n] = S(s)
		n++
		p := i.cfg.parent[s]
		if p < 0 {
			break
		}
		s = p
	}
	for k := n - 1; k >= 0; k-- {
		if err := i.runEntryState(ctx, i.pathBuf[k], tr, ev, true); err != nil {
			return err
		}
		i.state = i.pathBuf[k]
	}

	// Descend initial transitions into any composite destination.
	for st := int(i.state); i.cfg.initialSub[st] >= 0; {
		sub := i.cfg.initialSub[st]
		itr := Transition[S, T]{Source: S(st), Destination: S(sub), Trigger: t, Kind: Initial}
		if err := i.runEntryState(ctx, S(sub), itr, ev, false); err != nil {
			return err
		}
		i.state = S(sub)
		st = sub
	}

	i.c.transitions.Add(1)
	return nil
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

func (i *Instance[S, T, E]) runExitState(ctx context.Context, st S, tr Transition[S, T], ev E) error {
	for _, a := range i.cfg.states[st].onExit {
		if err := i.runAction(ctx, a, tr, ev); err != nil {
			return err
		}
	}
	return nil
}

// runEntryState runs a state's entry actions. If allowFrom is set, the
// trigger-specific OnEntryFrom action (if any) runs after the unconditional
// entry actions; initial-transition descents pass allowFrom=false.
func (i *Instance[S, T, E]) runEntryState(ctx context.Context, st S, tr Transition[S, T], ev E, allowFrom bool) error {
	si := &i.cfg.states[st]
	for _, a := range si.onEntry {
		if err := i.runAction(ctx, a, tr, ev); err != nil {
			return err
		}
	}
	if allowFrom && si.onEntryFrom != nil {
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

// State returns the current leaf state. IsIn reports true for the current leaf
// and every ancestor.
//
// IsIn reports whether the machine is currently in state s, taking hierarchy
// into account: it is true when s is the current leaf or any of its ancestors.
func (i *Instance[S, T, E]) IsIn(s S) bool {
	return i.cfg.isAncestorOrSelf(int(i.state), int(s))
}

// CanFire reports whether trigger t would be accepted now: an enabled candidate
// exists in the current state or an ancestor. It has no side effects.
func (i *Instance[S, T, E]) CanFire(ctx context.Context, t T, ev E) bool {
	if int(t) >= i.cfg.numTriggers {
		return false
	}
	for h := int(i.state); ; {
		cl := i.cfg.cell(h, int(t))
		for idx := range cl.candidates {
			if i.guardsPassNoRecord(ctx, cl.candidates[idx].guards, ev) {
				return true
			}
		}
		p := i.cfg.parent[h]
		if p < 0 {
			return false
		}
		h = p
	}
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
