// SPDX-License-Identifier: BSD-3-Clause

package fsm

import "fmt"

// cell returns the candidate list for a (state, trigger) pair. Callers must
// ensure s and t are in range (s < numStates, t < numTriggers).
func (c *Config[S, T, E]) cell(s, t int) *cell[S, T, E] {
	return &c.table[s*c.numTriggers+t]
}

// lcaOf returns the least common ancestor of state indices a and b, or -1 if
// they share no ancestor (distinct roots). It walks parent pointers with a
// depth equalization step; the loop is bounded by the tree height, which is
// tiny for statecharts, so no O(numStates^2) matrix is precomputed.
func (c *Config[S, T, E]) lcaOf(a, b int) int {
	for c.depth[a] > c.depth[b] {
		a = c.parent[a]
	}
	for c.depth[b] > c.depth[a] {
		b = c.parent[b]
	}
	for a != b {
		a = c.parent[a]
		b = c.parent[b]
		if a < 0 || b < 0 {
			return -1
		}
	}
	return a
}

// isAncestorOrSelf reports whether target is the state cur or one of its
// ancestors, via the precomputed per-state ancestor bitset.
func (c *Config[S, T, E]) isAncestorOrSelf(cur, target int) bool {
	if target < 0 || target >= c.numStates {
		return false
	}
	return c.ancestors[cur*c.wordsPerState+target/64]&(1<<(uint(target)%64)) != 0
}

// isDeferred reports whether trigger t is deferred in state s (directly or by
// inheritance from an ancestor).
func (c *Config[S, T, E]) isDeferred(s, t int) bool {
	if !c.hasDefer || t >= c.numTriggers {
		return false
	}
	return c.deferMask[s*c.deferWords+t/64]&(1<<(uint(t)%64)) != 0
}

// hierarchy holds the compiled hierarchy tables. All fields are plain integers
// so the computation is non-generic.
type hierarchy struct {
	parent        []int
	depth         []int
	initialSub    []int
	ancestors     []uint64
	hasChildren   []bool
	wordsPerState int
	maxDepth      int
}

// computeHierarchy validates the parent/initial relations and lowers them into
// depth, ancestor-bitset, and max-depth tables. parentOf[s] and initialOf[s]
// are -1 when unset. directlyEntered[s] marks states that are entered as a
// transition target (or the machine initial) and therefore, if composite, must
// declare an initial substate. Problems are appended to issues.
func computeHierarchy(numStates int, parentOf, initialOf []int, directlyEntered []bool, issues *[]string) hierarchy {
	h := hierarchy{
		parent:     parentOf,
		initialSub: initialOf,
		depth:      make([]int, numStates),
	}

	// Detect parent cycles before any unbounded parent walk.
	cyclic := false
	for s := 0; s < numStates; s++ {
		steps := 0
		for x := s; parentOf[x] >= 0; {
			x = parentOf[x]
			steps++
			if steps > numStates {
				*issues = append(*issues, fmt.Sprintf("fsm: hierarchy cycle through state %d", s))
				cyclic = true
				break
			}
		}
		if cyclic {
			break
		}
	}

	h.wordsPerState = (numStates + 63) / 64
	if h.wordsPerState < 1 {
		h.wordsPerState = 1
	}
	h.ancestors = make([]uint64, numStates*h.wordsPerState)
	if cyclic {
		// Bail out before the unbounded walks below; Build reports the issue.
		return h
	}

	for s := 0; s < numStates; s++ {
		d := 0
		for x := s; parentOf[x] >= 0; {
			x = parentOf[x]
			d++
		}
		h.depth[s] = d
		if d > h.maxDepth {
			h.maxDepth = d
		}
	}

	hasChildren := make([]bool, numStates)
	for s := 0; s < numStates; s++ {
		if parentOf[s] >= 0 {
			hasChildren[parentOf[s]] = true
		}
	}
	h.hasChildren = hasChildren

	// An initial substate must be a direct child of the state declaring it.
	for s := 0; s < numStates; s++ {
		if initialOf[s] >= 0 && parentOf[initialOf[s]] != s {
			*issues = append(*issues, fmt.Sprintf(
				"fsm: initial substate %d of state %d is not a direct child", initialOf[s], s))
		}
	}

	// A composite state that is directly entered must resolve to a leaf via
	// initial transitions; every composite along that descent needs an initial.
	for s := 0; s < numStates; s++ {
		if !directlyEntered[s] {
			continue
		}
		steps := 0
		for x := s; hasChildren[x]; {
			if initialOf[x] < 0 {
				*issues = append(*issues, fmt.Sprintf(
					"fsm: composite state %d is entered but has no initial substate", x))
				break
			}
			x = initialOf[x]
			steps++
			if steps > numStates {
				break
			}
		}
	}

	for s := 0; s < numStates; s++ {
		for x := s; x >= 0; x = parentOf[x] {
			h.ancestors[s*h.wordsPerState+x/64] |= 1 << (uint(x) % 64)
		}
	}

	return h
}
