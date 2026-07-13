// SPDX-License-Identifier: BSD-3-Clause

// Package ring is a fixed-size generic ring buffer used by supervise for
// restart-intensity windows and (later) the event backlog. It is allocation
// free after construction, which matters on the bare-metal TamaGo targets.
package ring

// Ring is a fixed-capacity circular buffer. The zero value is not usable;
// construct one with [New]. It is not safe for concurrent use; supervise owns
// its rings from a single goroutine.
type Ring[T any] struct {
	buf  []T
	head int // next write position
	used int // number of valid entries (<= cap)
}

// New returns a Ring holding at most n elements. n must be > 0.
func New[T any](n int) *Ring[T] {
	if n <= 0 {
		n = 1
	}
	return &Ring[T]{buf: make([]T, n)}
}

// Push appends v, overwriting the oldest element once the ring is full.
func (r *Ring[T]) Push(v T) {
	r.buf[r.head] = v
	r.head = (r.head + 1) % len(r.buf)
	if r.used < len(r.buf) {
		r.used++
	}
}

// Len reports the number of valid elements currently stored.
func (r *Ring[T]) Len() int { return r.used }

// Cap reports the maximum capacity.
func (r *Ring[T]) Cap() int { return len(r.buf) }

// Full reports whether the ring holds Cap() elements.
func (r *Ring[T]) Full() bool { return r.used == len(r.buf) }

// Oldest returns the oldest stored element and true, or the zero value and
// false if the ring is empty.
func (r *Ring[T]) Oldest() (T, bool) {
	var zero T
	if r.used == 0 {
		return zero, false
	}
	start := (r.head - r.used + len(r.buf)*2) % len(r.buf)
	return r.buf[start], true
}

// Reset empties the ring without releasing the backing array.
func (r *Ring[T]) Reset() {
	r.head = 0
	r.used = 0
}
