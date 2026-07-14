// SPDX-License-Identifier: BSD-3-Clause

// Package operator assembles and runs a Kyanite device microkernel: a
// declarative set of services supervised together with panic/Goexit containment
// and per-service restart policies.
//
// It is the single entry point shared by the device runtimes (vein, cairn).
// Each hardware platform's main builds a [Config] describing the services it
// needs and calls [New] then Run; the operator translates that set into a
// [supervise.Supervisor] tree. This keeps platform mains declarative — a
// caller-owned list of services plus a few knobs — instead of registering
// goroutines one by one.
//
// The configuration model is a plain struct with a [DefaultConfig] baseline,
// not functional options, keeping memory ownership explicit and avoiding the
// closure/slice allocations variadic options imply on constrained targets. The
// Services slice is owned by the caller; the operator never grows it.
//
//	svcs := []operator.Service{
//		operator.PermanentFunc("net-tx", netLoop),
//		operator.Permanent(lldp),
//	}
//	cfg := operator.DefaultConfig()
//	cfg.Name = "vein"
//	cfg.MemoryLimit = 192 * 1024 * 1024
//	cfg.Services = svcs
//	err := operator.New(cfg).Run(context.Background())
package operator

import (
	"context"
	"runtime/debug"

	"src.kyanite.computer/core/service"
	"src.kyanite.computer/core/supervise"
)

// DefaultName is the operator name used when [Config.Name] is left empty.
const DefaultName = "operator"

// Operator is itself a service, so it can be nested or tested uniformly.
var _ service.Service = (*Operator)(nil)

// Service pairs a supervised unit of work with its restart policy for
// declarative registration with an [Operator].
type Service struct {
	// Service is the unit of work to supervise.
	Service service.Service
	// Restart controls whether and when the service is restarted.
	Restart supervise.Restart
}

// Permanent wraps svc so it is always restarted when it stops (the policy for
// long-running loops that must always be present).
func Permanent(svc service.Service) Service {
	return Service{Service: svc, Restart: supervise.Permanent}
}

// Transient wraps svc so it is restarted only if it stops with an error.
func Transient(svc service.Service) Service {
	return Service{Service: svc, Restart: supervise.Transient}
}

// Temporary wraps svc so it runs at most once (never restarted).
func Temporary(svc service.Service) Service {
	return Service{Service: svc, Restart: supervise.Temporary}
}

// PermanentFunc wraps a ctx-aware function as a Permanent service.
func PermanentFunc(name string, fn func(ctx context.Context) error) Service {
	return Permanent(service.New(name, fn))
}

// PermanentChan wraps a blocking done-channel loop (fn returns when the channel
// is closed) as a Permanent service. Convenient for protocol agents whose loop
// takes a <-chan struct{}.
func PermanentChan(name string, fn func(done <-chan struct{})) Service {
	return Permanent(service.NewChan(name, fn))
}

// Config describes the microkernel a platform wants to run. Construct a
// baseline with [DefaultConfig], set the fields the platform needs, and pass it
// to [New]. Unset fields are filled from the defaults by New.
type Config struct {
	// Name identifies the operator in logs and as its service name. Empty means
	// [DefaultName].
	Name string
	// MemoryLimit is a soft heap limit (bytes) applied via
	// runtime/debug.SetMemoryLimit before services start. A value <= 0 leaves
	// the runtime default in place.
	MemoryLimit int64
	// Backoff is the supervisor restart backoff. A zero-value Backoff is
	// replaced with [supervise.DefaultBackoff].
	Backoff supervise.Backoff
	// Services is the caller-owned set of services to supervise. The operator
	// reads this slice but never appends to it.
	Services []Service
}

// DefaultConfig returns a baseline configuration by value.
func DefaultConfig() Config {
	return Config{
		Name:    DefaultName,
		Backoff: supervise.DefaultBackoff,
	}
}

func (c *Config) normalize() {
	if c.Name == "" {
		c.Name = DefaultName
	}
	if c.Backoff.Base == 0 && c.Backoff.Cap == 0 {
		c.Backoff = supervise.DefaultBackoff
	}
}

// Operator supervises a fixed, declarative set of services. Construct one with
// [New].
type Operator struct {
	cfg Config
}

// New returns an Operator configured by cfg. Unset fields are filled from
// [DefaultConfig].
func New(cfg Config) *Operator {
	cfg.normalize()
	return &Operator{cfg: cfg}
}

// Name returns the operator's configured name.
func (o *Operator) Name() string { return o.cfg.Name }

// Run applies the optional memory limit, registers every configured service
// with a [supervise.Supervisor], and runs it until ctx is cancelled. A panic,
// Goexit, or error in one service never propagates to the others: the
// supervisor contains and restarts it per its policy. Run returns the
// supervisor's result (ctx.Err() on graceful shutdown).
func (o *Operator) Run(ctx context.Context) error {
	if o.cfg.MemoryLimit > 0 {
		debug.SetMemoryLimit(o.cfg.MemoryLimit)
	}

	sup := supervise.New(o.cfg.Name, supervise.WithDefaultBackoff(o.cfg.Backoff))
	for _, s := range o.cfg.Services {
		if s.Service == nil {
			continue
		}
		sup.Add(supervise.Spec{
			Name:    s.Service.Name(),
			Start:   s.Service.Start,
			Restart: s.Restart,
		})
	}
	return sup.Run(ctx)
}

// Start satisfies [service.Service] so an Operator can be nested as a child of
// another supervisor. It is equivalent to [Operator.Run].
func (o *Operator) Start(ctx context.Context) error { return o.Run(ctx) }
