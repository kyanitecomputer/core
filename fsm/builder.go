// SPDX-License-Identifier: BSD-3-Clause

package fsm

import "fmt"

// DefaultQueueCapacity is the run-to-completion queue depth used when
// [WithQueueCapacity] is not supplied.
const DefaultQueueCapacity = 8

// settings holds non-generic build options so Option values need not be
// parameterized.
type settings struct {
	queueCap   int
	guardNames bool
}

// Option configures a [Builder] (and the [Config] it produces).
type Option func(*settings)

// WithQueueCapacity sets the run-to-completion queue depth (per instance).
// Values < 1 are clamped to 1.
func WithQueueCapacity(n int) Option { return func(s *settings) { s.queueCap = n } }

// WithGuardNames enables collection of failing guard names into
// [GuardsRejectedError]. Off by default to avoid the allocation.
func WithGuardNames() Option { return func(s *settings) { s.guardNames = true } }

// stateBuild accumulates one state's configuration during building.
type stateBuild[S ~uint8, T ~uint8, E any] struct {
	onEntry     []Action[S, T, E]
	onExit      []Action[S, T, E]
	onEntryFrom map[T]Action[S, T, E]
	cells       map[T][]candidate[S, T, E]
}

// Builder configures a machine fluently, then lowers it with [Builder.Build].
type Builder[S ~uint8, T ~uint8, E any] struct {
	initial S
	set     settings
	states  map[S]*stateBuild[S, T, E]
}

// New returns a Builder for a machine whose initial state is initial.
func New[S ~uint8, T ~uint8, E any](initial S, opts ...Option) *Builder[S, T, E] {
	set := settings{queueCap: DefaultQueueCapacity}
	for _, o := range opts {
		o(&set)
	}
	if set.queueCap < 1 {
		set.queueCap = 1
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

	table := make([]cell[S, T, E], numStates*numTriggers)
	for s, sb := range b.states {
		for t, cands := range sb.cells {
			validateCandidates(uint8(s), uint8(t), cands, &issues)
			table[int(s)*numTriggers+int(t)].candidates = cands
		}
	}

	if len(issues) > 0 {
		return nil, &BuildError{Issues: issues}
	}
	return &Config[S, T, E]{
		initial:     b.initial,
		numStates:   numStates,
		numTriggers: numTriggers,
		table:       table,
		states:      states,
		queueCap:    b.set.queueCap,
		guardNames:  b.set.guardNames,
	}, nil
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
