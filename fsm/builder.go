// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"fmt"
	"time"
)

// DefaultQueueCapacity is the run-to-completion queue depth used when
// [WithQueueCapacity] is not supplied.
const DefaultQueueCapacity = 8

// DefaultDeferCapacity is the deferral ring depth used when [WithDeferCapacity]
// is not supplied.
const DefaultDeferCapacity = 8

// settings holds non-generic build options so Option values need not be
// parameterized.
type settings struct {
	queueCap   int
	deferCap   int
	guardNames bool
	clock      Clock
}

// Option configures a [Builder] (and the [Config] it produces).
type Option func(*settings)

// WithQueueCapacity sets the run-to-completion queue depth (per instance).
// Values < 1 are clamped to 1.
func WithQueueCapacity(n int) Option { return func(s *settings) { s.queueCap = n } }

// WithDeferCapacity sets the deferral ring depth (per instance) for machines
// that use Defer. Values < 1 are clamped to 1.
func WithDeferCapacity(n int) Option { return func(s *settings) { s.deferCap = n } }

// WithGuardNames enables collection of failing guard names into
// [GuardsRejectedError]. Off by default to avoid the allocation.
func WithGuardNames() Option { return func(s *settings) { s.guardNames = true } }

// WithClock sets the time source for state timeouts. The default is real
// wall-clock time; tests inject a fake clock for deterministic timeout edges.
func WithClock(c Clock) Option { return func(s *settings) { s.clock = c } }

// stateBuild accumulates one state's configuration during building.
type stateBuild[S ~uint8, T ~uint8, E any] struct {
	onEntry     []Action[S, T, E]
	onExit      []Action[S, T, E]
	onEntryFrom map[T]Action[S, T, E]
	cells       map[T][]candidate[S, T, E]

	hasParent  bool
	parent     S
	hasInitial bool
	initial    S

	deferTrigs  []T
	hasTimeout  bool
	timeoutDur  time.Duration
	timeoutTrig T
}

// Builder configures a machine fluently, then lowers it with [Builder.Build].
type Builder[S ~uint8, T ~uint8, E any] struct {
	initial S
	set     settings
	states  map[S]*stateBuild[S, T, E]
}

// New returns a Builder for a machine whose initial state is initial.
func New[S ~uint8, T ~uint8, E any](initial S, opts ...Option) *Builder[S, T, E] {
	set := settings{queueCap: DefaultQueueCapacity, deferCap: DefaultDeferCapacity, clock: realClock{}}
	for _, o := range opts {
		o(&set)
	}
	if set.queueCap < 1 {
		set.queueCap = 1
	}
	if set.deferCap < 1 {
		set.deferCap = 1
	}
	if set.clock == nil {
		set.clock = realClock{}
	}
	return &Builder[S, T, E]{
		initial: initial,
		set:     set,
		states:  make(map[S]*stateBuild[S, T, E]),
	}
}

// StateCfg configures a single state. Methods return the same StateCfg for
// chaining.
type StateCfg[S ~uint8, T ~uint8, E any] struct {
	b  *Builder[S, T, E]
	s  S
	sb *stateBuild[S, T, E]
}

// State begins (or resumes) configuration of state s.
func (b *Builder[S, T, E]) State(s S) *StateCfg[S, T, E] {
	sb := b.states[s]
	if sb == nil {
		sb = &stateBuild[S, T, E]{cells: make(map[T][]candidate[S, T, E])}
		b.states[s] = sb
	}
	return &StateCfg[S, T, E]{b: b, s: s, sb: sb}
}

// Parent nests this state under state p, making it a substate. A trigger with
// no handler in this state is resolved by walking up to p (and its ancestors).
func (c *StateCfg[S, T, E]) Parent(p S) *StateCfg[S, T, E] {
	c.sb.hasParent = true
	c.sb.parent = p
	c.b.State(p) // ensure the parent is a known state
	return c
}

// Initial declares sub as the initial substate entered when this composite
// state is entered directly. sub must be a direct child (its Parent is this
// state), enforced at Build time.
func (c *StateCfg[S, T, E]) Initial(sub S) *StateCfg[S, T, E] {
	c.sb.hasInitial = true
	c.sb.initial = sub
	c.b.State(sub) // ensure the substate is a known state
	return c
}

// Permit adds an external transition on trigger t to dst, taken when all guards
// pass. Multiple Permit/Ignore/Reentry calls for the same trigger form an
// ordered candidate list; the first whose guards all pass fires. At most one
// candidate may be unguarded, and it must be declared last.
func (c *StateCfg[S, T, E]) Permit(t T, dst S, guards ...Guard[E]) *StateCfg[S, T, E] {
	c.add(t, candidate[S, T, E]{kind: ckExternal, dst: dst, guards: guards})
	return c
}

// Internal adds an internal reaction on trigger t: action runs without leaving
// the state (no exit/entry).
func (c *StateCfg[S, T, E]) Internal(t T, action Action[S, T, E]) *StateCfg[S, T, E] {
	c.add(t, candidate[S, T, E]{kind: ckInternal, dst: c.s, action: action})
	return c
}

// Reentry adds a self-transition on trigger t that re-runs exit then entry
// actions, taken when all guards pass.
func (c *StateCfg[S, T, E]) Reentry(t T, guards ...Guard[E]) *StateCfg[S, T, E] {
	c.add(t, candidate[S, T, E]{kind: ckReentry, dst: c.s, guards: guards})
	return c
}

// Ignore marks trigger t as explicitly ignored (accepted, no transition) when
// all guards pass — distinct from unhandled.
func (c *StateCfg[S, T, E]) Ignore(t T, guards ...Guard[E]) *StateCfg[S, T, E] {
	c.add(t, candidate[S, T, E]{kind: ckIgnore, dst: c.s, guards: guards})
	return c
}

// Defer marks triggers ts as deferred in this state: while here, they are
// neither handled nor discarded but stashed and re-offered in FIFO order after
// the next transition that changes the leaf state. Deferral is inherited by
// substates. A state that also handles a trigger consumes it (handling wins
// over deferral).
func (c *StateCfg[S, T, E]) Defer(ts ...T) *StateCfg[S, T, E] {
	c.sb.deferTrigs = append(c.sb.deferTrigs, ts...)
	return c
}

// Timeout arms a state timeout: entering this state records a deadline d in the
// future (per the injected clock); when the owner calls Tick at or after the
// deadline, trigger t is fired with Kind Timeout. A state has at most one
// timeout; a later call replaces an earlier one. The timeout only fires while
// the state is the resting leaf, so declaring it on a composite state is a
// [BuildError].
func (c *StateCfg[S, T, E]) Timeout(d time.Duration, t T) *StateCfg[S, T, E] {
	c.sb.hasTimeout = true
	c.sb.timeoutDur = d
	c.sb.timeoutTrig = t
	return c
}

// OnEntry adds an action run when the state is entered (any trigger).
func (c *StateCfg[S, T, E]) OnEntry(a Action[S, T, E]) *StateCfg[S, T, E] {
	c.sb.onEntry = append(c.sb.onEntry, a)
	return c
}

// OnExit adds an action run when the state is left.
func (c *StateCfg[S, T, E]) OnExit(a Action[S, T, E]) *StateCfg[S, T, E] {
	c.sb.onExit = append(c.sb.onExit, a)
	return c
}

// OnEntryFrom adds an action run when the state is entered specifically via
// trigger t, after the unconditional OnEntry actions.
func (c *StateCfg[S, T, E]) OnEntryFrom(t T, a Action[S, T, E]) *StateCfg[S, T, E] {
	if c.sb.onEntryFrom == nil {
		c.sb.onEntryFrom = make(map[T]Action[S, T, E])
	}
	c.sb.onEntryFrom[t] = a
	return c
}

func (c *StateCfg[S, T, E]) add(t T, cand candidate[S, T, E]) {
	c.sb.cells[t] = append(c.sb.cells[t], cand)
}

// Build validates the configuration and lowers it into an immutable [Config].
// It reports a [BuildError] for structural problems (duplicate unguarded
// transitions, an unguarded transition that is not declared last). After Build,
// the fire path allocates nothing and cannot panic on input.
func (b *Builder[S, T, E]) Build() (*Config[S, T, E], error) {
	var issues []string

	maxState := int(b.initial)
	maxTrigger := -1
	for s, sb := range b.states {
		if int(s) > maxState {
			maxState = int(s)
		}
		for t, cands := range sb.cells {
			if int(t) > maxTrigger {
				maxTrigger = int(t)
			}
			for i := range cands {
				if int(cands[i].dst) > maxState {
					maxState = int(cands[i].dst)
				}
			}
		}
		for _, t := range sb.deferTrigs {
			if int(t) > maxTrigger {
				maxTrigger = int(t)
			}
		}
		if sb.hasTimeout && int(sb.timeoutTrig) > maxTrigger {
			maxTrigger = int(sb.timeoutTrig)
		}
	}

	numStates := maxState + 1
	numTriggers := maxTrigger + 1
	if numTriggers < 1 {
		numTriggers = 1
	}

	states := make([]stateInfo[S, T, E], numStates)
	for s, sb := range b.states {
		states[s] = stateInfo[S, T, E]{
			onEntry:     sb.onEntry,
			onExit:      sb.onExit,
			onEntryFrom: sb.onEntryFrom,
		}
	}

	parentOf := make([]int, numStates)
	initialOf := make([]int, numStates)
	for i := range parentOf {
		parentOf[i] = -1
		initialOf[i] = -1
	}
	directlyEntered := make([]bool, numStates)
	directlyEntered[int(b.initial)] = true
	for s, sb := range b.states {
		if sb.hasParent {
			parentOf[int(s)] = int(sb.parent)
		}
		if sb.hasInitial {
			initialOf[int(s)] = int(sb.initial)
		}
	}

	table := make([]cell[S, T, E], numStates*numTriggers)
	for s, sb := range b.states {
		for t, cands := range sb.cells {
			validateCandidates(uint8(s), uint8(t), cands, &issues)
			for i := range cands {
				switch cands[i].kind {
				case ckExternal:
					directlyEntered[int(cands[i].dst)] = true
				case ckReentry:
					directlyEntered[int(s)] = true
				}
			}
			table[int(s)*numTriggers+int(t)].candidates = cands
		}
	}

	h := computeHierarchy(numStates, parentOf, initialOf, directlyEntered, &issues)

	// Deferral: lower per-state declared defers, then propagate down the
	// ancestor chain so a substate inherits its ancestors' deferrals.
	deferWords := (numTriggers + 63) / 64
	if deferWords < 1 {
		deferWords = 1
	}
	declared := make([]uint64, numStates*deferWords)
	hasDefer := false
	for s, sb := range b.states {
		for _, t := range sb.deferTrigs {
			declared[int(s)*deferWords+int(t)/64] |= 1 << (uint(t) % 64)
			hasDefer = true
		}
	}
	var deferMask []uint64
	if hasDefer && len(issues) == 0 {
		deferMask = make([]uint64, numStates*deferWords)
		for s := 0; s < numStates; s++ {
			for x := s; x >= 0; x = h.parent[x] {
				for w := 0; w < deferWords; w++ {
					deferMask[s*deferWords+w] |= declared[x*deferWords+w]
				}
			}
		}
	}

	// State timeouts.
	timeoutDur := make([]time.Duration, numStates)
	timeoutTrig := make([]T, numStates)
	for s, sb := range b.states {
		if !sb.hasTimeout {
			continue
		}
		if h.hasChildren != nil && h.hasChildren[int(s)] {
			issues = append(issues, fmt.Sprintf(
				"fsm: timeout on composite state %d never fires (only leaves rest)", uint8(s)))
			continue
		}
		timeoutDur[int(s)] = sb.timeoutDur
		timeoutTrig[int(s)] = sb.timeoutTrig
	}

	if len(issues) > 0 {
		return nil, &BuildError{Issues: issues}
	}
	cfg := &Config[S, T, E]{
		initial:       b.initial,
		numStates:     numStates,
		numTriggers:   numTriggers,
		table:         table,
		states:        states,
		queueCap:      b.set.queueCap,
		guardNames:    b.set.guardNames,
		parent:        h.parent,
		depth:         h.depth,
		initialSub:    h.initialSub,
		ancestors:     h.ancestors,
		wordsPerState: h.wordsPerState,
		maxDepth:      h.maxDepth,
		clock:         b.set.clock,
		deferCap:      b.set.deferCap,
		hasDefer:      hasDefer,
		deferMask:     deferMask,
		deferWords:    deferWords,
		timeoutDur:    timeoutDur,
		timeoutTrig:   timeoutTrig,
	}
	cfg.version = cfg.computeVersion()
	return cfg, nil
}

// validateCandidates enforces that a (state, trigger) cell has at most one
// unguarded candidate and that it is declared last.
func validateCandidates[S ~uint8, T ~uint8, E any](state, trigger uint8, cands []candidate[S, T, E], issues *[]string) {
	unguardedAt := -1
	for idx := range cands {
		if len(cands[idx].guards) == 0 {
			if unguardedAt >= 0 {
				*issues = append(*issues, fmt.Sprintf(
					"multiple unguarded transitions for trigger %d in state %d", trigger, state))
			}
			unguardedAt = idx
		}
	}
	if unguardedAt >= 0 && unguardedAt != len(cands)-1 {
		*issues = append(*issues, fmt.Sprintf(
			"unguarded transition for trigger %d in state %d must be declared last", trigger, state))
	}
}
