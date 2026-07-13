// Package service defines the contract for a supervised unit of work shared by
// the Kyanite device runtimes (vein, cairn).
//
// A Service is run by [src.kyanite.computer/core/supervise], which isolates each
// service in its own goroutine with panic/Goexit containment and a restart
// policy, so no single routine can crash the process.
//
// The contract is deliberately minimal: Start runs until it returns or ctx is
// cancelled. There is no NATS connection or other ambient dependency in the
// signature — a service captures whatever it needs (a *nats.Conn, a driver, a
// config manager) in the closure or struct it is constructed with. This keeps
// the supervision substrate stdlib-only and dependency-free.
package service

import "context"

// Service is a long-running (or one-shot) unit of work managed by a supervisor.
//
// Start executes the service until it returns or ctx is cancelled. A nil return
// means the service completed successfully; a non-nil error (or a panic/Goexit,
// which the supervisor contains) signals failure. The supervisor decides
// whether to restart based on the service's policy. Start must return promptly
// once ctx is cancelled.
type Service interface {
	// Name identifies the service in logs and the supervision tree.
	Name() string
	// Start executes the service.
	Start(ctx context.Context) error
}

// Func adapts a plain function into a [Service]. It is the convenient way to
// supervise an existing goroutine body.
type Func struct {
	// FuncName is reported by Name.
	FuncName string
	// Fn is the work to run; it must return promptly once ctx is cancelled.
	Fn func(ctx context.Context) error
}

// Name returns the service name.
func (f Func) Name() string { return f.FuncName }

// Start invokes Fn.
func (f Func) Start(ctx context.Context) error { return f.Fn(ctx) }

// New returns a [Func] service with the given name wrapping fn.
func New(name string, fn func(ctx context.Context) error) Func {
	return Func{FuncName: name, Fn: fn}
}

// NewChan adapts a blocking loop that stops when a done channel is closed into a
// [Service]. It passes ctx.Done() to fn and returns nil once fn returns. This is
// the convenient adapter for protocol agents whose loop takes a
// <-chan struct{} (for example fn = agent.serve).
func NewChan(name string, fn func(done <-chan struct{})) Func {
	return Func{FuncName: name, Fn: func(ctx context.Context) error {
		fn(ctx.Done())
		return nil
	}}
}
