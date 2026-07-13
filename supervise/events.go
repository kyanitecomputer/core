// SPDX-License-Identifier: BSD-3-Clause

package supervise

import (
	"context"
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

// log writes the event through the supervisor's logger at a severity matching
// its kind.
func (s *Supervisor) emit(e Event) {
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
