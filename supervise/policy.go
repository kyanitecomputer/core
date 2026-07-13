// SPDX-License-Identifier: BSD-3-Clause

package supervise

import (
	"math/rand/v2"
	"time"
)

// Restart is a child's restart policy.
type Restart uint8

const (
	// Permanent restarts the child whenever it stops, for any reason. Use it
	// for long-running loops that must always be present.
	Permanent Restart = iota
	// Transient restarts the child only when it stops with an error, panic, or
	// Goexit; a clean (nil) return is treated as successful completion.
	Transient
	// Temporary never restarts the child; it runs at most once.
	Temporary
)

// String returns the policy name.
func (r Restart) String() string {
	switch r {
	case Permanent:
		return "permanent"
	case Transient:
		return "transient"
	case Temporary:
		return "temporary"
	default:
		return "unknown"
	}
}

// Strategy governs which children a supervisor restarts when one fails.
type Strategy uint8

const (
	// OneForOne restarts only the failed child.
	OneForOne Strategy = iota
	// OneForAll restarts every child when any one fails.
	OneForAll
	// RestForOne restarts the failed child and every child declared after it.
	RestForOne
)

// String returns the strategy name.
func (s Strategy) String() string {
	switch s {
	case OneForOne:
		return "one_for_one"
	case OneForAll:
		return "one_for_all"
	case RestForOne:
		return "rest_for_one"
	default:
		return "unknown"
	}
}

// Backoff configures the delay between restarts using decorrelated jitter,
// which spreads restarts to avoid thundering herds without shared state.
type Backoff struct {
	// Base is the minimum (and initial) delay. If <= 0, DefaultBackoff.Base.
	Base time.Duration
	// Cap bounds the delay. If <= 0, DefaultBackoff.Cap.
	Cap time.Duration
}

// DefaultBackoff is used when a Backoff field is left zero.
var DefaultBackoff = Backoff{
	Base: 100 * time.Millisecond,
	Cap:  30 * time.Second,
}

func (b Backoff) normalized() Backoff {
	if b.Base <= 0 {
		b.Base = DefaultBackoff.Base
	}
	if b.Cap <= 0 {
		b.Cap = DefaultBackoff.Cap
	}
	if b.Cap < b.Base {
		b.Cap = b.Base
	}
	return b
}

// next computes the next decorrelated-jitter delay given the previous one.
// The recurrence is next = min(cap, rand[base, prev*3]). math/rand/v2 top-level
// functions are concurrency-safe and require no seeding.
func (b Backoff) next(prev time.Duration) time.Duration {
	bb := b.normalized()
	if prev < bb.Base {
		prev = bb.Base
	}
	span := int64(prev)*3 - int64(bb.Base)
	if span <= 0 {
		return bb.Base
	}
	d := time.Duration(rand.N(span)) + bb.Base
	if d > bb.Cap {
		d = bb.Cap
	}
	return d
}

// Intensity bounds how often a child may restart before the supervisor gives
// up and fails (escalating in a tree). A zero MaxRestarts disables the limit —
// the child restarts indefinitely, paced only by Backoff. This is the default
// for the device runtimes, whose permanent services must survive transient
// faults forever.
type Intensity struct {
	// MaxRestarts is the number of restarts tolerated within Window. <= 0
	// disables the limit.
	MaxRestarts int
	// Window is the sliding time window over which restarts are counted.
	Window time.Duration
}

// enabled reports whether the intensity limit is active.
func (i Intensity) enabled() bool { return i.MaxRestarts > 0 && i.Window > 0 }
