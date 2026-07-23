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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"src.kyanite.computer/core/supervise/internal/ring"
)

// DefaultEventBuffer is the lifecycle-event stream buffer used when
// [WithEventBuffer] is not supplied.
const DefaultEventBuffer = 64

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
	eventBuf     int

	// mu guards specs and started only across the Add/Run setup handoff. It is
	// never taken on the exit/restart hot path (that is single-owner loop
	// state); it exists solely to make pre-Run Add and Run's startup
	// race-free with respect to each other.
	mu      sync.Mutex
	specs   []Spec // registered pre-Run
	started bool

	// runStarted is closed once Run's loop is accepting commands, so live
	// Add/Stop can wait for the mailbox to exist.
	runStarted chan struct{}

	// events is the bounded, lossy lifecycle-event stream consumed via Events.
	// The loop is the sole producer and never blocks on it: a full buffer drops
	// the event and increments droppedEvents.
	events        chan Event
	droppedEvents atomic.Uint64

	// Runtime state, owned by the Run loop goroutine after Run starts.
	ctx        context.Context
	rootCancel context.CancelCauseFunc
	done       chan struct{}
	exitCh     chan exitMsg
	cmdCh      chan command
	timer      *time.Timer
	children   map[childID]*child
	order      []childID
	nextID     childID
	live       int
	shutting   bool
	failure    error
}

// command is a request delivered to the run loop's mailbox by Add/Stop.
type command interface{ isCommand() }

type addCommand struct {
	spec  Spec
	reply chan error
}

type stopCommand struct {
	name  string
	reply chan error
}

func (addCommand) isCommand()  {}
func (stopCommand) isCommand() {}

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

// WithEventBuffer sets the capacity of the [Supervisor.Events] stream buffer.
// Defaults to [DefaultEventBuffer]. A slow or absent consumer causes overflow
// events to be dropped (counted by [Supervisor.DroppedEvents]); the supervisor
// loop never blocks on event delivery.
func WithEventBuffer(n int) Option { return func(s *Supervisor) { s.eventBuf = n } }

// New returns a Supervisor named name.
func New(name string, opts ...Option) *Supervisor {
	s := &Supervisor{
		name:        name,
		path:        name,
		log:         slog.Default(),
		strategy:    OneForOne,
		defShutdown: DefaultShutdown,
		defBackoff:  DefaultBackoff,
		eventBuf:    DefaultEventBuffer,
		runStarted:  make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	if s.eventBuf < 1 {
		s.eventBuf = 1
	}
	s.events = make(chan Event, s.eventBuf)
	return s
}

// Name returns the supervisor name.
func (s *Supervisor) Name() string { return s.name }

// Add registers a child specification. Called before [Run] it appends to the
// initial set (do this from a single goroutine during setup). Called after Run
// has started it registers the child live through the command mailbox and
// spawns it immediately. A Spec with a nil Start is rejected.
func (s *Supervisor) Add(spec Spec) error {
	if spec.Start == nil {
		return errors.New("supervise: Add with nil Spec.Start")
	}
	s.mu.Lock()
	if !s.started {
		s.specs = append(s.specs, spec)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	// Live: route through the mailbox so the loop owns the mutation.
	reply := make(chan error, 1)
	return s.deliver(addCommand{spec: spec, reply: reply}, reply)
}

// AddSupervisor registers a child supervisor as a supervised subtree with
// restart policy r. A child supervisor's failure (for example *IntensityError)
// is an error return from its Run, so it escalates through the parent's normal
// restart handling — Erlang-style tree escalation.
func (s *Supervisor) AddSupervisor(child *Supervisor, r Restart) error {
	return s.Add(Spec{Name: child.name, Start: child.Run, Restart: r})
}

// Stop cancels the named child and prevents it from restarting. It waits for
// the loop to accept the request (not for the child to exit). It returns an
// error if no active child has that name, or [ErrShutdown] if the supervisor is
// stopping. Stop is only meaningful after [Run] has started.
func (s *Supervisor) Stop(name string) error {
	reply := make(chan error, 1)
	return s.deliver(stopCommand{name: name, reply: reply}, reply)
}

// deliver sends a command to the run loop and waits for its reply, aborting if
// the supervisor stops first.
func (s *Supervisor) deliver(c command, reply chan error) error {
	select {
	case <-s.runStarted:
	case <-s.done:
		return ErrShutdown
	}
	select {
	case s.cmdCh <- c:
	case <-s.done:
		return ErrShutdown
	}
	select {
	case err := <-reply:
		return err
	case <-s.done:
		return ErrShutdown
	}
}

// createChild builds and registers a child record for spec, assigning a unique
// id. It does not spawn the child. Called only from the run loop (or Run's
// setup before the loop starts).
func (s *Supervisor) createChild(spec Spec) *child {
	s.nextID++
	c := &child{id: s.nextID, spec: spec}
	if s.intensity.enabled() {
		c.restarts = ring.New[time.Time](s.intensity.MaxRestarts)
	}
	s.children[c.id] = c
	s.order = append(s.order, c.id)
	return c
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
	// Close the event stream after the loop (all emit calls are loop-owned, so
	// no send can race this) to end any active Events range.
	defer close(s.events)

	// All reads of s.specs and the initial spawn happen under mu so they cannot
	// race a concurrent Add; started is then flipped so subsequent Adds route
	// through the mailbox.
	s.mu.Lock()
	bufsz := 2 * len(s.specs)
	if bufsz < 1 {
		bufsz = 1
	}
	s.exitCh = make(chan exitMsg, bufsz)
	s.cmdCh = make(chan command)
	s.children = make(map[childID]*child, len(s.specs))
	s.order = make([]childID, 0, len(s.specs))
	for i := range s.specs {
		c := s.createChild(s.specs[i])
		s.spawn(c)
		s.live++
	}
	s.started = true
	s.mu.Unlock()

	s.timer = time.NewTimer(time.Hour)
	s.timer.Stop()
	defer s.timer.Stop()

	// Signal that live Add/Stop may now use the mailbox. Everything after this
	// runs in the single-owner loop.
	close(s.runStarted)

	ctxDone := s.ctx.Done()
	for s.live > 0 {
		s.rearm()
		select {
		case m := <-s.exitCh:
			s.handleExit(m)
		case c := <-s.cmdCh:
			s.handleCommand(c)
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
		if c.restartPending {
			// Cancelled as part of a group restart and now quiesced; wait for
			// the rest of the affected set, then respawn together.
			c.state = stQuiesced
			s.maybeGroupRespawn()
			return
		}
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

	s.beginRestart(c, m.err)
}

// beginRestart starts a (possibly group) restart triggered by child c failing
// with err. For OneForOne the affected set is just c; for OneForAll it is all
// restartable children; for RestForOne it is c and every child declared after
// it. Affected siblings that are running are cancelled with a SiblingFailure
// cause and, once the whole set has quiesced, the set is respawned in
// declaration order after backoff.
func (s *Supervisor) beginRestart(c *child, err error) {
	affected := s.affectedIDs(c)
	for _, id := range affected {
		s.children[id].restartPending = true
	}

	// The triggering child has already exited.
	c.state = stQuiesced

	now := s.now()
	for _, id := range affected {
		a := s.children[id]
		if a.id == c.id {
			continue
		}
		switch a.state {
		case stRunning:
			a.cancel(SiblingFailure{Sibling: c.spec.Name, Err: err})
			a.state = stStopping
			a.graceUntil = now.Add(s.graceFor(a))
		case stBackoff:
			// Not running; ready to respawn with the group.
			a.state = stQuiesced
		}
	}
	s.maybeGroupRespawn()
}

// affectedIDs returns the ordered ids of children a restart of c affects, per
// the supervisor strategy. Only restartable children are included: Temporary
// and already-terminal children are left untouched.
func (s *Supervisor) affectedIDs(c *child) []childID {
	restartable := func(x *child) bool {
		return x.spec.Restart != Temporary && x.state != stDone && x.state != stAbandoned
	}
	switch s.strategy {
	case OneForAll:
		ids := make([]childID, 0, len(s.order))
		for _, id := range s.order {
			if restartable(s.children[id]) {
				ids = append(ids, id)
			}
		}
		return ids
	case RestForOne:
		ids := make([]childID, 0, len(s.order))
		after := false
		for _, id := range s.order {
			if id == c.id {
				after = true
			}
			if after && restartable(s.children[id]) {
				ids = append(ids, id)
			}
		}
		return ids
	default: // OneForOne
		return []childID{c.id}
	}
}

// maybeGroupRespawn respawns the pending affected set once every one of its
// members has quiesced (none still running or stopping), in declaration order
// after per-child backoff.
func (s *Supervisor) maybeGroupRespawn() {
	for _, id := range s.order {
		c := s.children[id]
		if c.restartPending && (c.state == stRunning || c.state == stStopping) {
			return // still quiescing
		}
	}
	now := s.now()
	for _, id := range s.order {
		c := s.children[id]
		if !c.restartPending {
			continue
		}
		c.restartPending = false
		c.backoff = s.backoffFor(c).next(c.backoff)
		c.dueAt = now.Add(c.backoff)
		c.state = stBackoff
		s.emit(Event{Kind: EventRestarting, Tree: s.path, Child: c.spec.Name, At: now, Extra: c.backoff})
	}
}

// handleCommand processes a mailbox command (live Add / Stop) in the loop.
func (s *Supervisor) handleCommand(c command) {
	switch cmd := c.(type) {
	case addCommand:
		if s.shutting {
			cmd.reply <- ErrShutdown
			return
		}
		ch := s.createChild(cmd.spec)
		s.spawn(ch)
		s.live++
		cmd.reply <- nil
	case stopCommand:
		ch := s.findActive(cmd.name)
		if ch == nil {
			cmd.reply <- fmt.Errorf("supervise: no active child named %q", cmd.name)
			return
		}
		s.stopChild(ch)
		cmd.reply <- nil
	}
}

// findActive returns the first active child with the given name, or nil.
func (s *Supervisor) findActive(name string) *child {
	for _, id := range s.order {
		c := s.children[id]
		if c.spec.Name == name && c.active() {
			return c
		}
	}
	return nil
}

// stopChild cancels a child and marks it terminal (no restart).
func (s *Supervisor) stopChild(c *child) {
	c.stopRequested = true
	switch c.state {
	case stRunning:
		c.cancel(ErrStopped)
		c.state = stStopping
		c.graceUntil = s.now().Add(s.graceFor(c))
	case stBackoff, stQuiesced:
		c.state = stDone
		s.live--
	}
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
				elapsed := s.graceFor(c)
				s.emit(Event{
					Kind: EventAbandoned, Tree: s.path, Child: c.spec.Name,
					At: now, Extra: elapsed,
					Err: &AbandonedError{Child: c.spec.Name, Elapsed: elapsed},
				})
				if c.restartPending {
					// Uncooperative during a group restart: respawn a fresh
					// incarnation and stop waiting on the rogue one (which is
					// tracked and late-reaped). live is unchanged — the child
					// remains active via its new incarnation.
					c.state = stQuiesced
					s.maybeGroupRespawn()
				} else {
					c.state = stAbandoned
					s.live--
					if c.spec.Critical && s.failure == nil {
						s.failure = &AbandonedError{Child: c.spec.Name, Elapsed: elapsed}
						s.rootCancel(s.failure)
					}
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
		c.restartPending = false // no group restarts survive shutdown
		switch c.state {
		case stRunning:
			c.cancel(ErrShutdown)
			c.state = stStopping
			c.graceUntil = now.Add(s.graceFor(c))
		case stBackoff, stQuiesced:
			// No goroutine running; the pending (re)spawn is cancelled.
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
