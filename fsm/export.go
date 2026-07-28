// SPDX-License-Identifier: BSD-3-Clause

package fsm

import (
	"fmt"
	"strings"
)

// Labeler supplies human-readable names for states and triggers when exporting
// a machine to DOT or Mermaid. Either function may be nil, in which case a
// numeric fallback (S<n>, T<n>) is used. No reflection is involved; names are
// the caller's (typically generated String methods).
type Labeler[S ~uint8, T ~uint8] struct {
	State   func(S) string
	Trigger func(T) string
}

// StateName returns the label for state s, falling back to S<n> when no State
// function is set.
func (l Labeler[S, T]) StateName(s S) string {
	if l.State != nil {
		return l.State(s)
	}
	return fmt.Sprintf("S%d", uint8(s))
}

// TriggerName returns the label for trigger t, falling back to T<n> when no
// Trigger function is set.
func (l Labeler[S, T]) TriggerName(t T) string {
	if l.Trigger != nil {
		return l.Trigger(t)
	}
	return fmt.Sprintf("T%d", uint8(t))
}

func (c *Config[S, T, E]) roots() []int {
	var r []int
	for s := 0; s < c.numStates; s++ {
		if c.parent[s] < 0 {
			r = append(r, s)
		}
	}
	return r
}

func (c *Config[S, T, E]) children(p int) []int {
	var ch []int
	for s := 0; s < c.numStates; s++ {
		if c.parent[s] == p {
			ch = append(ch, s)
		}
	}
	return ch
}

func (c *Config[S, T, E]) composite(s int) bool {
	for x := 0; x < c.numStates; x++ {
		if c.parent[x] == s {
			return true
		}
	}
	return false
}

// repLeaf returns a representative leaf inside state s (s itself if it is a
// leaf), following initial transitions where present. DOT edges to a composite
// point at this leaf with lhead/ltail=cluster, the standard compound-graph
// idiom, instead of a phantom node named after the composite.
func (c *Config[S, T, E]) repLeaf(s int) int {
	for c.composite(s) {
		if c.initialSub[s] >= 0 {
			s = c.initialSub[s]
			continue
		}
		s = c.children(s)[0]
	}
	return s
}

// edgeLabel renders a transition label: the trigger name, any guard names in
// brackets (or a bare [guard] marker for unnamed guards), and a kind suffix in
// parentheses for non-external transitions.
func (c *Config[S, T, E]) edgeLabel(l Labeler[S, T], t T, guards []Guard[E], extra string) string {
	s := l.TriggerName(t)
	if len(guards) > 0 {
		var names []string
		for _, g := range guards {
			if g.Name != "" {
				names = append(names, g.Name)
			}
		}
		if len(names) > 0 {
			s += " [" + strings.Join(names, ",") + "]"
		} else {
			s += " [guard]"
		}
	}
	if extra != "" {
		s += " (" + extra + ")"
	}
	return s
}

// candidateEdge describes a lowered transition for the exporters.
type candidateEdge struct {
	src, dst int
	label    string
	extra    string // "", "internal", "reentry", "ignore"
}

// edges returns every transition in deterministic (state, trigger, order)
// order.
func (c *Config[S, T, E]) edges(l Labeler[S, T]) []candidateEdge {
	var out []candidateEdge
	for s := 0; s < c.numStates; s++ {
		for t := 0; t < c.numTriggers; t++ {
			cl := c.cell(s, t)
			for k := range cl.candidates {
				cand := &cl.candidates[k]
				e := candidateEdge{src: s, dst: s}
				switch cand.kind {
				case ckExternal:
					e.dst = int(cand.dst)
					e.label = c.edgeLabel(l, T(t), cand.guards, "")
				case ckReentry:
					e.extra = "reentry"
					e.label = c.edgeLabel(l, T(t), cand.guards, "reentry")
				case ckInternal:
					e.extra = "internal"
					e.label = c.edgeLabel(l, T(t), cand.guards, "internal")
				case ckIgnore:
					e.extra = "ignore"
					e.label = c.edgeLabel(l, T(t), cand.guards, "ignore")
				}
				out = append(out, e)
			}
		}
	}
	return out
}

// Mermaid renders the machine as a Mermaid stateDiagram-v2. Composite states
// nest their children; initial transitions render as [*] --> child. Output is
// deterministic, suitable for golden tests.
func (c *Config[S, T, E]) Mermaid(l Labeler[S, T]) string {
	var b strings.Builder
	b.WriteString("stateDiagram-v2\n")
	fmt.Fprintf(&b, "  [*] --> %s\n", l.StateName(c.initial))
	for _, s := range c.roots() {
		if c.composite(s) {
			c.mermaidState(&b, l, s, "  ")
		}
	}
	for _, e := range c.edges(l) {
		fmt.Fprintf(&b, "  %s --> %s : %s\n", l.StateName(S(e.src)), l.StateName(S(e.dst)), e.label)
	}
	return b.String()
}

func (c *Config[S, T, E]) mermaidState(b *strings.Builder, l Labeler[S, T], s int, indent string) {
	fmt.Fprintf(b, "%sstate %s {\n", indent, l.StateName(S(s)))
	if c.initialSub[s] >= 0 {
		fmt.Fprintf(b, "%s  [*] --> %s\n", indent, l.StateName(S(c.initialSub[s])))
	}
	for _, child := range c.children(s) {
		if c.composite(child) {
			c.mermaidState(b, l, child, indent+"  ")
		} else {
			fmt.Fprintf(b, "%s  %s\n", indent, l.StateName(S(child)))
		}
	}
	fmt.Fprintf(b, "%s}\n", indent)
}

// DOT renders the machine as a Graphviz digraph. Composite states become
// clusters; internal, reentry, and ignore transitions are styled distinctly.
// Node ids are state indices (n<index>) for stability; labels carry names.
// Output is deterministic, suitable for golden tests.
func (c *Config[S, T, E]) DOT(l Labeler[S, T]) string {
	var b strings.Builder
	b.WriteString("digraph fsm {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  compound=true;\n")
	for _, s := range c.roots() {
		c.dotNode(&b, l, s, "  ")
	}
	for _, e := range c.edges(l) {
		var attrs strings.Builder
		fmt.Fprintf(&attrs, "label=%q", e.label)
		switch e.extra {
		case "internal":
			attrs.WriteString(",style=dotted")
		case "ignore":
			attrs.WriteString(",style=dashed")
		}
		if c.composite(e.src) {
			fmt.Fprintf(&attrs, ",ltail=cluster_%d", e.src)
		}
		if c.composite(e.dst) {
			fmt.Fprintf(&attrs, ",lhead=cluster_%d", e.dst)
		}
		fmt.Fprintf(&b, "  n%d -> n%d [%s];\n", c.repLeaf(e.src), c.repLeaf(e.dst), attrs.String())
	}
	b.WriteString("}\n")
	return b.String()
}

func (c *Config[S, T, E]) dotNode(b *strings.Builder, l Labeler[S, T], s int, indent string) {
	if !c.composite(s) {
		fmt.Fprintf(b, "%sn%d [label=%q];\n", indent, s, l.StateName(S(s)))
		return
	}
	fmt.Fprintf(b, "%ssubgraph cluster_%d {\n", indent, s)
	fmt.Fprintf(b, "%s  label=%q;\n", indent, l.StateName(S(s)))
	if c.initialSub[s] >= 0 {
		fmt.Fprintf(b, "%s  ini%d [shape=point,label=\"\"];\n", indent, s)
		fmt.Fprintf(b, "%s  ini%d -> n%d;\n", indent, s, c.initialSub[s])
	}
	for _, child := range c.children(s) {
		c.dotNode(b, l, child, indent+"  ")
	}
	fmt.Fprintf(b, "%s}\n", indent)
}
