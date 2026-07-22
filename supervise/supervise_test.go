// SPDX-License-Identifier: BSD-3-Clause

package supervise

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// quietLogger discards supervision events so tests stay silent.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fastBackoff makes restarts effectively immediate so real-time tests stay quick.
func fastBackoff() Option {
	return WithDefaultBackoff(Backoff{Base: time.Millisecond, Cap: 2 * time.Millisecond})
}

// runAsync starts s.Run in a goroutine and returns a channel delivering its result.
func runAsync(s *Supervisor, ctx context.Context) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	return errc
}

func TestTemporaryRunsOnce(t *testing.T) {
	var runs atomic.Int64
	s := New("t", quietOpts(fastBackoff())...)
	s.Add(Spec{Name: "temp", Restart: Temporary, Start: func(context.Context) error {
		runs.Add(1)
		return errors.New("boom")
	}})

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("Temporary ran %d times, want 1", got)
	}
}

func TestTransientRestartsUntilSuccess(t *testing.T) {
	var runs atomic.Int64
	s := New("t", quietOpts(fastBackoff())...)
	s.Add(Spec{Name: "trans", Restart: Transient, Start: func(context.Context) error {
		if runs.Add(1) < 3 {
			return errors.New("fail")
		}
		return nil // third run succeeds; Transient must then stop
	}})

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if got := runs.Load(); got != 3 {
		t.Fatalf("Transient ran %d times, want 3 (2 failures + 1 success)", got)
	}
}

func TestPermanentKeepsRestarting(t *testing.T) {
	var runs atomic.Int64
	s := New("t", quietOpts(fastBackoff())...)
	s.Add(Spec{Name: "perm", Restart: Permanent, Start: func(context.Context) error {
		runs.Add(1)
		return nil // clean return; Permanent restarts anyway
	}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)
	waitFor(t, func() bool { return runs.Load() >= 3 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if got := runs.Load(); got < 3 {
		t.Fatalf("Permanent ran %d times, want >= 3", got)
	}
}

func TestPanicContained(t *testing.T) {
	var runs atomic.Int64
	rec := &recordHandler{}
	s := New("t", WithLogger(slog.New(rec)), fastBackoff())
	s.Add(Spec{Name: "panicker", Restart: Permanent, Start: func(context.Context) error {
		runs.Add(1)
		panic("kaboom")
	}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)
	waitFor(t, func() bool { return runs.Load() >= 2 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if !rec.sawError("panicked") {
		t.Fatalf("expected a contained panic error to be logged; events: %v", rec.errors())
	}
}

func TestGoexitContained(t *testing.T) {
	var runs atomic.Int64
	rec := &recordHandler{}
	s := New("t", WithLogger(slog.New(rec)), fastBackoff())
	s.Add(Spec{Name: "goexiter", Restart: Permanent, Start: func(context.Context) error {
		runs.Add(1)
		runtime.Goexit()
		return nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)
	waitFor(t, func() bool { return runs.Load() >= 2 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if !rec.sawError("Goexit") {
		t.Fatalf("expected a contained Goexit error to be logged; events: %v", rec.errors())
	}
}

func TestGracefulShutdown(t *testing.T) {
	started := make(chan struct{})
	s := New("t", quietOpts()...)
	s.Add(Spec{Name: "coop", Restart: Permanent, Start: func(ctx context.Context) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)
	<-started
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after cancellation")
	}
}

func TestAbandonment(t *testing.T) {
	release := make(chan struct{})
	var released sync.Once
	defer released.Do(func() { close(release) })

	rec := &recordHandler{}
	s := New("t", WithLogger(slog.New(rec)), WithDefaultShutdown(20*time.Millisecond))
	s.Add(Spec{Name: "stubborn", Restart: Permanent, Start: func(context.Context) error {
		<-release // deliberately ignores ctx
		return nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)
	time.Sleep(20 * time.Millisecond) // let it start
	start := time.Now()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
			t.Fatalf("Run returned after %s, expected to wait ~grace (20ms)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked on an uncooperative child (abandonment failed)")
	}
	if !rec.sawEvent("abandoned") {
		t.Fatalf("expected an abandonment event; events: %v", rec.kinds())
	}
	released.Do(func() { close(release) }) // let the rogue goroutine exit
}

func TestIntensityEscalates(t *testing.T) {
	var runs atomic.Int64
	s := New("t", WithLogger(quietLogger()), fastBackoff(), WithIntensity(3, time.Minute))
	s.Add(Spec{Name: "crashloop", Restart: Permanent, Start: func(context.Context) error {
		runs.Add(1)
		return errors.New("boom")
	}})

	err := s.Run(context.Background())
	ie, ok := errors.AsType[*IntensityError](err)
	if !ok {
		t.Fatalf("Run returned %v, want *IntensityError", err)
	}
	if ie.Restarts != 3 {
		t.Fatalf("IntensityError.Restarts = %d, want 3", ie.Restarts)
	}
	if runs.Load() < 4 {
		t.Fatalf("crashloop ran %d times, want >= 4 before escalation", runs.Load())
	}
}

func TestSynctestCompat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := make(chan struct{})
		s := New("t", quietOpts()...)
		s.Add(Spec{Name: "coop", Restart: Permanent, Start: func(ctx context.Context) error {
			<-ctx.Done()
			close(done)
			return ctx.Err()
		}})
		ctx, cancel := context.WithCancel(context.Background())
		errc := runAsync(s, ctx)
		synctest.Wait()
		cancel()
		<-done
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	})
}

func TestStopPreventsRestart(t *testing.T) {
	var victimRuns, keepRuns atomic.Int64
	s := New("t", quietOpts(fastBackoff())...)
	s.Add(Spec{Name: "keep", Restart: Permanent, Start: func(ctx context.Context) error {
		keepRuns.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}})
	s.Add(Spec{Name: "victim", Restart: Permanent, Start: func(ctx context.Context) error {
		victimRuns.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runAsync(s, ctx)
	waitFor(t, func() bool { return victimRuns.Load() >= 1 && keepRuns.Load() >= 1 })

	if err := s.Stop("victim"); err != nil {
		t.Fatalf("Stop(victim): %v", err)
	}
	time.Sleep(20 * time.Millisecond) // give any (erroneous) restart a chance
	if got := victimRuns.Load(); got != 1 {
		t.Fatalf("victim ran %d times after Stop, want 1 (no restart)", got)
	}
	if err := s.Stop("nonexistent"); err == nil {
		t.Fatal("Stop of unknown child should error")
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

func TestLiveAdd(t *testing.T) {
	var addedRuns atomic.Int64
	s := New("t", quietOpts()...)
	s.Add(Spec{Name: "anchor", Restart: Permanent, Start: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)

	// Add blocks until Run has started, then registers and spawns live.
	if err := s.Add(Spec{Name: "late", Restart: Permanent, Start: func(ctx context.Context) error {
		addedRuns.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatalf("live Add: %v", err)
	}
	waitFor(t, func() bool { return addedRuns.Load() >= 1 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

func TestTreeNesting(t *testing.T) {
	var leafRuns atomic.Int64
	sub := New("sub", quietOpts()...)
	sub.Add(Spec{Name: "leaf", Restart: Permanent, Start: func(ctx context.Context) error {
		leafRuns.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}})

	root := New("root", quietOpts()...)
	if err := root.AddSupervisor(sub, Permanent); err != nil {
		t.Fatalf("AddSupervisor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(root, ctx)
	waitFor(t, func() bool { return leafRuns.Load() >= 1 })
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("root Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("root Run did not cascade-cancel the subtree")
	}
}

func TestOneForAllRestartsAll(t *testing.T) {
	var aRuns, bRuns, cRuns atomic.Int64
	s := New("t", quietOpts(fastBackoff(), WithStrategy(OneForAll))...)
	s.Add(Spec{Name: "a", Restart: Permanent, Start: func(ctx context.Context) error {
		if aRuns.Add(1) == 1 {
			return errors.New("a boom") // fail once; triggers group restart
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	s.Add(Spec{Name: "b", Restart: Permanent, Start: func(ctx context.Context) error {
		bRuns.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}})
	s.Add(Spec{Name: "c", Restart: Permanent, Start: func(ctx context.Context) error {
		cRuns.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)
	// All three must restart (reach a 2nd run) because a's single failure
	// takes the whole set down under OneForAll.
	waitFor(t, func() bool {
		return aRuns.Load() >= 2 && bRuns.Load() >= 2 && cRuns.Load() >= 2
	})
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

func TestRestForOneRestartsSuffix(t *testing.T) {
	var aRuns, bRuns, cRuns atomic.Int64
	s := New("t", quietOpts(fastBackoff(), WithStrategy(RestForOne))...)
	s.Add(Spec{Name: "a", Restart: Permanent, Start: func(ctx context.Context) error {
		aRuns.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}})
	s.Add(Spec{Name: "b", Restart: Permanent, Start: func(ctx context.Context) error {
		if bRuns.Add(1) == 1 {
			return errors.New("b boom") // fail once
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	s.Add(Spec{Name: "c", Restart: Permanent, Start: func(ctx context.Context) error {
		cRuns.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)
	// b and c (declared at/after b) restart; a (before b) is untouched.
	waitFor(t, func() bool { return bRuns.Load() >= 2 && cRuns.Load() >= 2 })
	time.Sleep(20 * time.Millisecond)
	if got := aRuns.Load(); got != 1 {
		t.Fatalf("a (declared before the failure) ran %d times, want 1", got)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

func TestNoChildrenReturnsNil(t *testing.T) {
	s := New("empty", quietOpts()...)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}

func TestNoLeaks(t *testing.T) {
	var runs atomic.Int64
	s := New("t", quietOpts(fastBackoff())...)
	s.Add(Spec{Name: "perm", Restart: Permanent, Start: func(ctx context.Context) error {
		runs.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(s, ctx)
	waitFor(t, func() bool { return runs.Load() >= 1 })
	cancel()
	<-errc
	runtime.GC()
	AssertNoLeaks(t)
}

// --- test helpers -----------------------------------------------------------

func quietOpts(extra ...Option) []Option {
	return append([]Option{WithLogger(quietLogger())}, extra...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// recordHandler is a slog.Handler that records event kinds and error strings.
type recordHandler struct {
	mu     sync.Mutex
	kindsL []string
	errsL  []string
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordHandler) WithGroup(string) slog.Handler            { return h }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "event":
			h.kindsL = append(h.kindsL, a.Value.String())
		case "error":
			h.errsL = append(h.errsL, a.Value.String())
		}
		return true
	})
	return nil
}

func (h *recordHandler) sawError(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.errsL {
		if contains(e, substr) {
			return true
		}
	}
	return false
}

func (h *recordHandler) sawEvent(kind string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range h.kindsL {
		if k == kind {
			return true
		}
	}
	return false
}

func (h *recordHandler) errors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.errsL...)
}

func (h *recordHandler) kinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.kindsL...)
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
