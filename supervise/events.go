// SPDX-License-Identifier: BSD-3-Clause

package supervise

import (
	"context"
	"iter"
	"log/slog"
	"time"
)

// EventKind classifies a supervision lifecycle event.
type EventKind uint8

const (
	// EventStarted is emitted when a child's guard goroutine is spawned.
	EventStarted EventKind = iota
	// EventExited is emitted when a child returns or fails (before any restart
	// decision).
	EventExited
	// EventRestarting is emitted when a child is scheduled to restart after
	// backoff.
	EventRestarting
	// EventStopped is emitted when a child reaches a terminal state without
	// restart (Temporary, or Transient clean return).
	EventStopped
	// EventAbandoned is emitted when a child ignores cancellation past its
	// grace period.
	EventAbandoned
	// EventLateReap is emitted when an abandoned child eventually exits.
	EventLateReap
	// EventIntensity is emitted when a child exceeds the restart-intensity
	// window and the supervisor fails.
	EventIntensity
)

// String returns the event kind name.
func (k EventKind) String() string {
	switch k {
	case EventStarted:
		return "started"
	case EventExited:
		return "exited"
	case EventRestarting:
		return "restarting"
	case EventStopped:
		return "stopped"
	case EventAbandoned:
		return "abandoned"
	case EventLateReap:
		return "late_reap"
	case EventIntensity:
		return "intensity"
	default:
		return "unknown"
	}
}

// Event is a supervision lifecycle fact. Events are logged through the
// supervisor's slog.Logger; a pull-based [Supervisor.Events] stream is added in
// a later slice.
type Event struct {
	Kind  EventKind
	Tree  string        // supervisor path, e.g. "root/net"
	Child string        // child name
	Err   error         // non-nil for failure/exit events
	At    time.Time     // event time
	Extra time.Duration // elapsed/backoff, event-specific
}

// Events returns a pull-based stream of supervision lifecycle events, suitable
// for a management/telemetry consumer. It ranges until the supervisor stops
// (its Run returns) or the caller breaks. The stream is bounded and lossy: if
// the consumer falls behind, events are dropped rather than blocking the
// supervisor loop, and the drop count is available via [Supervisor.DroppedEvents].
//
// A single consumer is supported; events are delivered to whichever range is
// active. Call it from a dedicated goroutine, typically after Run has started.
func (s *Supervisor) Events() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for e := range s.events {
			if !yield(e) {
				return
			}
		}
	}
}

// DroppedEvents returns the number of lifecycle events dropped because the
// [Supervisor.Events] buffer was full.
func (s *Supervisor) DroppedEvents() uint64 { return s.droppedEvents.Load() }

// emit publishes an event to the bounded stream (non-blocking; drops with a
// counter on overflow) and logs it through the supervisor's logger at a
// severity matching its kind.
func (s *Supervisor) emit(e Event) {
	select {
	case s.events <- e:
	default:
		s.droppedEvents.Add(1)
	}

	if s.log == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("event", e.Kind.String()),
		slog.String("tree", e.Tree),
		slog.String("child", e.Child),
	}
	if e.Extra > 0 {
		attrs = append(attrs, slog.Duration("elapsed", e.Extra))
	}
	if e.Err != nil {
		attrs = append(attrs, slog.String("error", e.Err.Error()))
	}
	level := slog.LevelInfo
	switch e.Kind {
	case EventAbandoned, EventIntensity:
		level = slog.LevelError
	case EventRestarting, EventLateReap:
		level = slog.LevelWarn
	}
	s.log.LogAttrs(context.Background(), level, "supervise", attrs...)
}
