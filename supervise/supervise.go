// SPDX-License-Identifier: BSD-3-Clause

// Package supervise is a stdlib-only, actor-model supervision tree for Go 1.27.
//
// It converts every abnormal goroutine exit — a returned error, a panic,
// runtime.Goexit, or (opt-in) a memory fault — into a typed, stack-carrying
// error routed through a single code path, so a child can never take the
// process down. All supervisor state is owned by one goroutine (the run loop)
// and mutated only there; external interaction and child exits arrive as
// messages, so there are no mutexes on the hot path and the race detector has
// nothing to find.
//
// This is the shared supervision substrate for the Kyanite device runtimes
// (vein, cairn); it replaces the earlier core/supervisor. The child contract is
// a bare func(ctx) error — no NATS connection, no interface to implement;
// dependencies are captured by the closure.
//
// Slice 1 provides single-level OneForOne supervision with restart policies,
// decorrelated-jitter backoff, restart-intensity limits, and the two-phase
// abandonment protocol for goroutines that ignore cancellation. OneForAll /
// RestForOne strategies, tree nesting, futures, combinators, and the pull-based
// event stream land in subsequent slices.
package supervise

import (
	"context"
	"log/slog"
	"time"

	"src.kyanite.computer/core/supervise/internal/ring"
)

// DefaultShutdown is the grace period between cancelling a child and declaring
// it abandoned, when a Spec does not override it.
const DefaultShutdown = 5 * time.Second

// Supervisor runs and restarts a set of children under an actor loop. Construct
// one with [New], register children with [Add], then call [Run].
type Supervisor struct {
	name         string
	path         string
	log          *slog.Logger
	strategy     Strategy
	intensity    Intensity
	defShutdown  time.Duration
	defBackoff   Backoff
	panicOnFault bool

	specs []Spec // registered pre-Run

	// Runtime state, owned by the Run loop goroutine after Run starts.
	ctx        context.Context
	rootCancel context.CancelCauseFunc
	done       chan struct{}
	exitCh     chan exitMsg
	timer      *time.Timer
	children   map[childID]*child
	order      []childID
	live       int
	shutting   bool
	failure    error
}

// Option configures a [Supervisor].
type Option func(*Supervisor)

// WithStrategy sets the restart strategy. (Slice 1 honors OneForOne; the other
// strategies are accepted but treated as OneForOne until a later slice.)
func WithStrategy(st Strategy) Option { return func(s *Supervisor) { s.strategy = st } }

// WithIntensity bounds restarts to max within window; exceeding it fails the
// supervisor. A max <= 0 disables the limit (the default).
func WithIntensity(max int, window time.Duration) Option {
	return func(s *Supervisor) { s.intensity = Intensity{MaxRestarts: max, Window: window} }
}

// WithDefaultShutdown sets the default per-child shutdown grace.
func WithDefaultShutdown(d time.Duration) Option {
	return func(s *Supervisor) { s.defShutdown = d }
}

// WithDefaultBackoff sets the default restart backoff.
func WithDefaultBackoff(b Backoff) Option { return func(s *Supervisor) { s.defBackoff = b } }

// WithPanicOnFault makes children convert memory faults (bad unsafe/MMIO
// pointers) into recoverable panics via debug.SetPanicOnFault. Useful for
// register-poking children; best-effort (faults in runtime internals stay
// fatal).
func WithPanicOnFault() Option { return func(s *Supervisor) { s.panicOnFault = true } }

// WithLogger sets the slog.Logger for lifecycle events. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) Option { return func(s *Supervisor) { s.log = l } }

// New returns a Supervisor named name.
func New(name string, opts ...Option) *Supervisor {
	s := &Supervisor{
		name:        name,
		path:        name,
		log:         slog.Default(),
		strategy:    OneForOne,
		defShutdown: DefaultShutdown,
		defBackoff:  DefaultBackoff,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Name returns the supervisor name.
func (s *Supervisor) Name() string { return s.name }

// Add registers a child specification. It must be called before [Run]. A Spec
// with a nil Start is ignored.
func (s *Supervisor) Add(spec Spec) {
	if spec.Start == nil {
		return
	}
	s.specs = append(s.specs, spec)
}

// Run starts every registered child in its own guarded goroutine and blocks
// until the supervisor stops: either ctx is cancelled (graceful two-phase
// shutdown of all children) or every child has reached a terminal state. A
// panic, Goexit, or error in one child is contained and handled per its policy;
// it never propagates to other children or to the caller.
//
// Run returns the escalation error (for example *IntensityError) if the
// supervisor failed, the parent ctx error on external cancellation, or nil if
// all children completed cleanly.
func (s *Supervisor) Run(ctx context.Context) error {
	s.ctx, s.rootCancel = context.WithCancelCause(ctx)
	defer s.rootCancel(nil)
	s.done = make(chan struct{})
	defer close(s.done)

	bufsz := 2 * len(s.specs)
	if bufsz < 1 {
		bufsz = 1
	}
	s.exitCh = make(chan exitMsg, bufsz)
	s.children = make(map[childID]*child, len(s.specs))
	s.order = make([]childID, 0, len(s.specs))

	for i := range s.specs {
		c := &child{
			id:   childID(i + 1),
			spec: s.specs[i],
		}
		if s.intensity.enabled() {
			c.restarts = ring.New[time.Time](s.intensity.MaxRestarts)
		}
		s.children[c.id] = c
		s.order = append(s.order, c.id)
		s.spawn(c)
		s.live++
	}

	s.timer = time.NewTimer(time.Hour)
	s.timer.Stop()
	defer s.timer.Stop()

	ctxDone := s.ctx.Done()
	for s.live > 0 {
		s.rearm()
		select {
		case m := <-s.exitCh:
			s.handleExit(m)
		case <-s.timer.C:
			s.handleTimer()
		case <-ctxDone:
			s.beginShutdown()
			ctxDone = nil // closed channel would busy-loop; stop selecting it
		}
	}

	if s.failure != nil {
		return s.failure
	}
	return ctx.Err()
}

// handleExit processes one child exit message.
func (s *Supervisor) handleExit(m exitMsg) {
	c := s.children[m.id]
	if c == nil {
		return
	}
	if m.incarnation != c.incarnation {
		// A stale message from a previous incarnation — the child was already
		// abandoned and has finally exited. The books balance.
		s.emit(Event{Kind: EventLateReap, Tree: s.path, Child: c.spec.Name, At: s.now(), Err: m.err})
		return
	}

	s.emit(Event{Kind: EventExited, Tree: s.path, Child: c.spec.Name, At: s.now(), Err: m.err})

	if c.state == stStopping {
		// Exited within grace during shutdown/stop: terminal, no restart.
		c.state = stDone
		s.live--
		return
	}

	if !s.shouldRestart(c, m.err) {
		c.state = stDone
		s.live--
		s.emit(Event{Kind: EventStopped, Tree: s.path, Child: c.spec.Name, At: s.now()})
		return
	}

	// Intensity check before scheduling the restart.
	if s.intensity.enabled() {
		if c.restarts.Full() {
			if oldest, ok := c.restarts.Oldest(); ok && s.now().Sub(oldest) < s.intensity.Window {
				s.failure = &IntensityError{
					Child:    c.spec.Name,
					Restarts: s.intensity.MaxRestarts,
					Window:   s.intensity.Window,
				}
				s.emit(Event{Kind: EventIntensity, Tree: s.path, Child: c.spec.Name, At: s.now(), Err: s.failure})
				c.state = stDone
				s.live--
				s.rootCancel(s.failure) // escalate: shut the rest down
				return
			}
		}
		c.restarts.Push(s.now())
	}

	// Schedule respawn after decorrelated-jitter backoff.
	c.backoff = s.backoffFor(c).next(c.backoff)
	c.dueAt = s.now().Add(c.backoff)
	c.state = stBackoff
	s.emit(Event{Kind: EventRestarting, Tree: s.path, Child: c.spec.Name, At: s.now(), Extra: c.backoff})
}

// shouldRestart applies the child's restart policy to an exit reason.
func (s *Supervisor) shouldRestart(c *child, err error) bool {
	switch c.spec.Restart {
	case Permanent:
		return true
	case Transient:
		return err != nil
	default: // Temporary
		return false
	}
}

// handleTimer respawns due backoff children and abandons children whose grace
// has elapsed.
func (s *Supervisor) handleTimer() {
	now := s.now()
	for _, id := range s.order {
		c := s.children[id]
		switch c.state {
		case stBackoff:
			if !c.dueAt.After(now) {
				s.spawn(c) // → stRunning, incarnation++
			}
		case stStopping:
			if !c.graceUntil.After(now) {
				c.state = stAbandoned
				s.live--
				elapsed := s.graceFor(c)
				s.emit(Event{
					Kind: EventAbandoned, Tree: s.path, Child: c.spec.Name,
					At: now, Extra: elapsed,
					Err: &AbandonedError{Child: c.spec.Name, Elapsed: elapsed},
				})
				if c.spec.Critical && s.failure == nil {
					s.failure = &AbandonedError{Child: c.spec.Name, Elapsed: elapsed}
					s.rootCancel(s.failure)
				}
			}
		}
	}
}

// beginShutdown cancels all still-active children and moves them toward
// terminal states. It is idempotent.
func (s *Supervisor) beginShutdown() {
	if s.shutting {
		return
	}
	s.shutting = true
	now := s.now()
	for _, id := range s.order {
		c := s.children[id]
		switch c.state {
		case stRunning:
			c.cancel(ErrShutdown)
			c.state = stStopping
			c.graceUntil = now.Add(s.graceFor(c))
		case stBackoff:
			// No goroutine running; the pending respawn is cancelled.
			c.state = stDone
			s.live--
		}
	}
}

// rearm resets the single owned timer to the earliest pending deadline
// (respawn or grace) across all children, or stops it if none is pending.
func (s *Supervisor) rearm() {
	var earliest time.Time
	found := false
	for _, id := range s.order {
		c := s.children[id]
		var t time.Time
		switch c.state {
		case stBackoff:
			t = c.dueAt
		case stStopping:
			t = c.graceUntil
		default:
			continue
		}
		if !found || t.Before(earliest) {
			earliest = t
			found = true
		}
	}
	if !found {
		s.timer.Stop()
		return
	}
	d := earliest.Sub(s.now())
	if d < 0 {
		d = 0
	}
	s.timer.Reset(d)
}

func (s *Supervisor) backoffFor(c *child) Backoff {
	if c.spec.Backoff.Base > 0 || c.spec.Backoff.Cap > 0 {
		return c.spec.Backoff
	}
	return s.defBackoff
}

func (s *Supervisor) graceFor(c *child) time.Duration {
	if c.spec.Shutdown > 0 {
		return c.spec.Shutdown
	}
	return s.defShutdown
}

func (s *Supervisor) now() time.Time { return time.Now() }
