# `supervise` — stdlib-only supervision trees for Go 1.26/1.27

Architecture specification. Zero third-party imports. Minimum toolchain `go 1.26`; optional
`//go:build go1.27` files unlock generic-method sugar and default-on goroutine-leak profiling.

---

## 1. Design goals

1. **Erlang-grade supervision** (restart policies, strategies, intensity, escalation) — from `oversight`.
2. **Lexically-bound structured concurrency** (nothing outlives its scope; first-class futures) — from `scope`.
3. **Ergonomic combinators** (run-N-copies, first-completion) — from `nursery`.
4. **Panic containment as a total function**: every abnormal exit — `panic`, `runtime.Goexit`,
   panic-in-defer, memory fault (opt-in) — is converted into a typed, stack-carrying error and
   routed through one code path. A child can never take the process down.
5. **Leak-hostile by construction**: every goroutine is accounted for from spawn to exit or to an
   explicit, observable *abandoned* state. Verified in CI via the runtime `goroutineleak` profile
   and `testing/synctest`.
6. **stdlib only**: `context`, `sync`, `time`, `errors`, `log/slog`, `runtime`, `runtime/debug`,
   `runtime/pprof`, `math/rand/v2`, `iter`. No `x/`, no `errgroup`.

Non-goals: process-level supervision (exec), distribution, hot code reload, dynamic
`simple_one_for_one`-style unbounded child churn beyond what `Pool` provides.

---

## 2. Prior art — what is kept, what is rejected

| Library | Keep | Reject |
|---|---|---|
| `kakkky/scope` | Lexical `Run(ctx, body)` binding; `Future[T]`; child scopes; `WithCancelOnSuccess` | Panic → flat error loses typed stack; no restarts; options not inherited |
| `cirello-io/oversight` | `Permanent/Transient/Temporary`; `OneForOne/OneForAll/RestForOne`; intensity window; tree nesting | Mutex-heavy shared tree state; `time.Sleep` restart pacing; no abandonment protocol for non-cooperative children; `context` misuse for value plumbing |
| `arunsworld/nursery` | `RunMultipleCopiesConcurrently`, `RunUntilFirstCompletion` shapes | `chan error` as the job interface (caller can forget to drain → leak); no panic recovery at all |

Common defect in all three: an uncooperative child (ignores `ctx.Done()`) blocks shutdown forever
or is silently leaked. This design makes that case a first-class, observable state instead.

---

## 3. Package layout

```
supervise/
├── supervise.go        // Supervisor, Spec, Run, tree wiring
├── child.go            // child record, lifecycle FSM, spawn/guard
├── guard.go            // panic/Goexit/fault containment
├── policy.go           // Restart, Strategy, Intensity, Backoff
├── future.go           // Future[T], typed handles
├── combinators.go      // Race, Replicate, All (nursery/scope shapes)
├── events.go           // Event stream, slog bridge
├── errors.go           // PanicError, GoexitError, AbandonedError, IntensityError
├── clock.go            // injectable clock (real + synctest-friendly)
├── leak_check.go       // goroutineleak profile helpers for tests/CI
├── sugar_go127.go      // //go:build go1.27 — generic methods, synctest.Sleep
└── internal/ring/      // fixed-size ring buffer (restart timestamps, event backlog)
```

Single public package. `internal/ring` is the only sub-package; it is allocation-free after init
(relevant for TamaGo targets — see §13).

---

## 4. Core model

### 4.1 Ownership: the supervisor is an actor

All mutable state (child table, restart history, FSM states) is owned by exactly one goroutine —
the supervisor loop. External API calls (`Add`, `Stop`, `Inspect`) and child exits are messages on
a mailbox. There are **zero mutexes on the hot path** and no lock-ordering concerns across the
tree; the race detector has nothing to find because there is nothing shared.

```go
type command interface{ isCommand() }        // addChild, stopChild, inspect, shutdown
type exitMsg struct {                        // sent from the child's guard defer
    id   childID
    err  error                               // nil | app error | *PanicError | *GoexitError
    late bool                                // true if child was already Abandoned
}
```

Mailbox rules (leak-critical):

- `exitCh` is buffered to `len(children)` and resized on `Add` — a dying child **never blocks**
  sending its exit. Guard sends use `select { case exitCh <- m: case <-supDone: }` so a child
  outliving a dead supervisor discards instead of blocking (defense in depth; §6 makes a dead
  supervisor with live children structurally impossible except through abandonment).
- Command sends are synchronous request/response with a per-request reply channel of capacity 1,
  and every reply select includes `<-supDone` so callers cannot hang on a stopping supervisor.

### 4.2 Child FSM

```
            spawn                 exit(err)
  Idle ───────────► Running ───────────────► Exited
                      │                         │ policy says restart
                      │ cancel(cause)           ▼
                      ▼                    Backoff ──timer──► Running (respawn)
                   Stopping
                      │ grace elapsed, still running
                      ▼
                  Abandoned ──(late exitMsg)──► Reaped (logged, counted)
```

States live only inside the supervisor loop. `Abandoned` is the honest encoding of Go's inability
to kill goroutines: the supervisor stops *waiting* but never stops *accounting* (§8).

### 4.3 Child specification

```go
type Spec struct {
    Name     string
    Start    func(ctx context.Context) error
    Restart  Restart        // Permanent | Transient | Temporary
    Shutdown time.Duration  // grace between cancel and Abandoned; 0 = policy default
    Backoff  Backoff        // per-child override; zero value = supervisor default
    Critical bool           // if true, its abandonment escalates instead of merely logging
}
```

`Start` is the entire contract: run until done or until `ctx` is cancelled, return the reason.
No error channels (nursery's mistake), no interfaces to implement.

---

## 5. Panic containment (`guard.go`)

Every child runs under one guard. It must convert **four** abnormal-exit shapes, not one:

```go
func (s *Supervisor) spawn(c *child) {
    ctx, cancel := context.WithCancelCause(s.ctx)
    c.cancel = cancel
    s.wg.Go(func() {                      // sync.WaitGroup.Go (1.25)
        pprof.Do(ctx, pprof.Labels(
            "supervise.tree", s.path,     // e.g. "root/net/redfish"
            "supervise.child", c.spec.Name,
        ), func(ctx context.Context) {
            var err error
            completed := false
            defer func() {
                if r := recover(); r != nil {
                    err = &PanicError{Value: r, Stack: debug.Stack(), Child: c.spec.Name}
                } else if !completed {
                    // recover() == nil but fn never returned: runtime.Goexit is in
                    // flight (t.FailNow in a child, or explicit Goexit). It cannot be
                    // stopped — defers run, then the goroutine dies. Report before dying.
                    err = &GoexitError{Child: c.spec.Name, Stack: debug.Stack()}
                }
                select {
                case s.exitCh <- exitMsg{id: c.id, err: err}:
                case <-s.done:
                }
            }()
            if s.opts.PanicOnFault {      // opt-in: MMIO / unsafe-heavy children
                old := debug.SetPanicOnFault(true)
                defer debug.SetPanicOnFault(old)
            }
            err = c.spec.Start(ctx)
            completed = true
        })
    })
}
```

Details that the reference libraries get wrong or omit:

- **`runtime.Goexit` detection** via the `completed` flag. `scope` and `oversight` treat this as a
  clean nil-error exit; here it is a distinct `*GoexitError` (default policy: treat as failure).
- **Panic-in-defer inside `Start`**: the outermost recover still catches it; the *original* panic
  is lost by the runtime, so the doc contract says "wrap cleanup panics yourself if you need
  both." The guard cannot fix language semantics, only never crash.
- **Stack capture at recover point**, not at exit-message processing — `debug.Stack()` inside the
  deferred recover yields the panicking frames.
- **`debug.SetPanicOnFault`** (opt-in per supervisor): converts SIGSEGV-class faults from bad
  `unsafe`/MMIO pointers into recoverable panics instead of process death. Directly useful for
  register-poking children; documented as best-effort (fault in runtime internals still fatal).
- **pprof labels**: every goroutine in the tree carries its path. A `goroutine` or `goroutineleak`
  profile dump attributes any leak to an exact child (§8.4).
- Typed extraction downstream uses 1.26 `errors.AsType`:

```go
if pe, ok := errors.AsType[*PanicError](err); ok { slog.Error("child panicked", "stack", pe.Stack) }
```

---

## 6. Cancellation and cause propagation

`context.WithCancelCause` everywhere; the cause is the supervision verdict, retrievable by the
child and by post-mortem inspection:

```go
var (
    ErrShutdown       = errors.New("supervise: supervisor shutting down")
    ErrRestartCycle   = errors.New("supervise: restarted by strategy")
)
type SiblingFailure struct{ Sibling string; Err error } // cause for OneForAll/RestForOne victims
```

Rules:

- Child contexts derive from the supervisor's context, which derives from the parent supervisor's
  child context — cancelling any node cancels its entire subtree, transitively, with causes intact.
- `context.WithoutCancel` is deliberately **not** offered in the API surface; detaching from the
  tree is exactly the leak class this package exists to kill. Children needing background flush
  get it via `Shutdown` grace, not detachment.
- Values are never smuggled through context by the package itself.

---

## 7. Restart engine (`policy.go`)

### 7.1 Policies and strategies (oversight-compatible semantics)

```go
type Restart uint8  // Permanent (always), Transient (only on error/panic), Temporary (never)
type Strategy uint8 // OneForOne, OneForAll, RestForOne
```

- `OneForAll` / `RestForOne` victims are cancelled with `SiblingFailure` cause, then wait for their
  exit (through the same guard path — so a victim that panics during teardown is still contained),
  then the affected set restarts **in declaration order** (Erlang ordering guarantee).
- A restart never reuses the old context or channels; each incarnation is a fresh
  `WithCancelCause`. Incarnation counter in the child record prevents a stale `exitMsg` from a
  previous incarnation (possible after Abandoned→Reaped races) from being mistaken for the
  current one — messages carry `(childID, incarnation)`.

### 7.2 Intensity (restart storm) window

Fixed-size ring of `time.Time` (capacity = `MaxRestarts`); a restart is admitted iff the oldest
recorded restart is older than `Window`. Exceeding it fails the **supervisor** with
`*IntensityError{Child, Restarts, Window}` — which, since a supervisor is itself a child of its
parent, escalates up the tree exactly like Erlang. Root escalation returns from `Run`.

### 7.3 Backoff

Decorrelated jitter (no shared state, `math/rand/v2` top-level functions are already
concurrency-safe and non-overridable):

```go
next = min(cap, rand.N(prev*3 - base) + base)   // rand/v2, integer duration domain
```

Pacing uses the supervisor loop's single `time.Timer`, multiplexed over pending restarts via a
monotonic min-heap of due times — **no `time.After` in loops, ever**. (Go 1.27 makes timer
channels synchronous, removing the classic `time.After` leak, but per-iteration allocation and
late fires remain; one owned timer is strictly better and synctest-friendly.)

---

## 8. Leak defenses

### 8.1 The abandonment protocol (two-phase stop)

Go cannot kill a goroutine; pretending otherwise is where other libraries leak. Sequence for any
stop (individual, strategy victim, or full shutdown):

1. `cancel(cause)`.
2. Arm grace timer (`Spec.Shutdown`, default e.g. 5s; supervisor-wide default configurable).
3. Exit arrives in time → normal path.
4. Timer fires first → state `Abandoned`: supervisor decrements its liveness barrier, emits
   `EventAbandoned` (name, tree path, elapsed, goroutine labels already in place for profiling),
   increments an `expvar`-style counter, and — if `Spec.Critical` — escalates as a failure.
5. If the goroutine *eventually* exits, its guard's `exitMsg` still fires (`late: true`);
   supervisor logs `EventLateReap`. The books always balance.

### 8.2 Liveness barrier

`Supervisor.Run` returns only when `wg.Wait()` (all guards) completes **or** all remaining guards
belong to Abandoned children — implemented by tracking a `live` count in the loop rather than
waiting on the WaitGroup directly for the early-out; the WaitGroup remains the ground truth for
the non-abandoned case. Result: shutdown is bounded by `max(Shutdown_i)`, never unbounded.

### 8.3 `runtime.AddCleanup` tripwire

On construction, each `Future[T]`/handle registers `runtime.AddCleanup(h, reportDroppedHandle, meta)`.
If user code drops a handle for a still-running child without calling `Wait`/`Stop`, the cleanup
(post-GC) emits `EventHandleDropped` — catching the "fire-and-forget a future" bug class that
`scope` cannot see. Cleanup is unregistered on terminal states (1.24 API; unlike finalizers,
cleanups don't resurrect and may run concurrently — the report path is a non-blocking event send).

### 8.4 Runtime leak profile integration (1.26 experimental → 1.27 default)

`leak_check.go` ships a test helper:

```go
func AssertNoLeaks(t *testing.T) {
    t.Helper()
    p := pprof.Lookup("goroutineleak") // GOEXPERIMENT=goroutineleakprofile on 1.26; default on 1.27
    if p == nil { t.Skip("goroutineleak profile unavailable") }
    var buf bytes.Buffer
    p.WriteTo(&buf, 1) // triggers the leak-detecting GC cycle
    if strings.Contains(buf.String(), "supervise.child") { t.Fatalf("leaked:\n%s", buf.String()) }
}
```

Because §5 labels every child goroutine, the profile output self-attributes. Production services
get `/debug/pprof/goroutineleak` for free via `net/http/pprof`.

### 8.5 Channel discipline (internal invariants)

- Every internal channel has exactly one closer, and it is the sender's owner.
- Every blocking send/receive pairs with a lifetime guard (`<-s.done`) in a `select`.
- `Future[T].Wait(ctx)` selects on result **and** caller ctx **and** supervisor done — a future
  can never park a caller past the tree's lifetime.
- No goroutine is created outside `spawn` (grep-enforceable: one `go`/`wg.Go` site in the package).

---

## 9. API surface

```go
// Construction — options use 1.26 new(expr) for pointer-optional fields internally.
func New(name string, opts ...Option) *Supervisor
func (s *Supervisor) Add(spec Spec) error                    // pre-Run or live (mailbox)
func (s *Supervisor) AddSupervisor(child *Supervisor, r Restart) error // tree composition
func (s *Supervisor) Run(ctx context.Context) error          // blocks; lexical root binding
func (s *Supervisor) Stop(name string) error                 // two-phase, §8.1
func (s *Supervisor) Events() iter.Seq[Event]                // pull-based; ring-buffered, lossy-with-counter

// Options
WithStrategy(Strategy), WithIntensity(max int, window time.Duration),
WithDefaultShutdown(time.Duration), WithDefaultBackoff(Backoff),
WithPanicOnFault(), WithLogger(*slog.Logger), WithClock(Clock)

// Futures (package-level on 1.26)
func Go[T any](s *Supervisor, name string, fn func(ctx context.Context) (T, error)) *Future[T]
func (f *Future[T]) Wait(ctx context.Context) (T, error)

// Combinators (nursery/scope shapes, all supervision-backed)
func All(ctx context.Context, fns ...func(context.Context) error) error          // join, errors.Join aggregate
func Race(ctx context.Context, fns ...func(context.Context) error) error         // first completion wins, rest cancelled+reaped
func Replicate(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error
```

Go 1.27 sugar (`sugar_go127.go`, `//go:build go1.27` — generic methods require the file's language
version ≥ 1.27, so it is build-tag isolated while `go.mod` can stay at 1.26):

```go
func (s *Supervisor) Spawn[T any](name string, fn func(context.Context) (T, error)) *Future[T]
```

Logging: events bridge to `slog`; users fanning out to multiple sinks (console + OTLP-ish shipper)
use 1.26 `slog.NewMultiHandler` — the package takes one `*slog.Logger` and stays agnostic.

Error taxonomy (all extractable via `errors.AsType`): `*PanicError`, `*GoexitError`,
`*AbandonedError`, `*IntensityError`, `SiblingFailure`; aggregates via `errors.Join`.

---

## 10. Concurrency invariants (checklist form)

1. One goroutine mutates supervisor state (the loop). Everything else is message passing.
2. One `go` site (the guard). Every goroutine carries pprof labels.
3. No unguarded blocking channel op anywhere.
4. No `time.After`; one owned timer + heap.
5. No context detachment API.
6. Restart incarnations are versioned; stale messages are dropped by version check.
7. `Run` termination is bounded: `O(max child Shutdown)` after cancellation.
8. Zero allocations in steady state after warm-up (ring buffers, reused timer, value-type
   messages ≤ 80 B to hit 1.27 size-specialized malloc fast path when they do occur).

---

## 11. Testing strategy

- **`testing/synctest`** for everything time-dependent: intensity windows, backoff schedules,
  grace timers, abandonment — deterministic, instant, no flakes. The injectable `Clock` exists
  only to keep non-synctest benchmarks honest; under `synctest.Test` the real clock is already
  virtualized. 1.27's `synctest.Sleep` (sleep+wait) tightens restart-ordering tests.
- **Race detector** on the full matrix; the actor design should make it structurally quiet.
- **`AssertNoLeaks`** (§8.4) appended to every test via `t.Cleanup`.
- Fault-injection children: panicker, goexiter, ctx-ignorer (abandonment path), defer-panicker,
  fast-crash-loop (intensity), slow-stopper (grace boundary), fault-toucher (with `PanicOnFault`).
- Fuzz the loop with randomized command/exit interleavings (property: books balance — spawned ==
  reaped + abandoned_live; abandoned_live == 0 after late reaps drain).

---

## 12. Go 1.26 / 1.27 feature usage matrix

| Feature | Version | Use here |
|---|---|---|
| `errors.AsType[T]` | 1.26 | typed extraction of `*PanicError` etc. |
| `new(expr)` | 1.26 | option defaults / pointer-optional Spec fields |
| `goroutineleak` pprof profile | 1.26 exp / **1.27 default** | CI leak assertions, prod endpoint |
| Green Tea GC | 1.26 default | cheaper frequent small exits/restarts; nothing to do, benefits ring/msg design |
| `slog.NewMultiHandler` | 1.26 | user-side event fan-out |
| Heap base randomization | 1.26 | free hardening, no action |
| Generic methods | **1.27** | `s.Spawn[T]` sugar (build-tagged) |
| `synctest.Sleep` | 1.27 | deterministic pacing tests |
| Synchronous `time` channels (asynctimerchan removed) | 1.27 | documented rationale for owned-timer design |
| Size-specialized malloc (<80 B) | 1.27 | message structs sized under threshold |
| `sync.WaitGroup.Go` | 1.25 | guard spawn |
| `context.WithCancelCause` | 1.20 | verdict-carrying cancellation |
| `runtime.AddCleanup` | 1.24 | dropped-handle tripwire |
| `math/rand/v2` | 1.22 | jittered backoff |
| `iter.Seq` | 1.23 | `Events()` |

Not used: `runtime/secret`, `simd/archsimd` (irrelevant); `sync.Map`/mutexes (actor model);
`x/sync/errgroup` (forbidden and unnecessary — `All` covers it with panic safety errgroup lacks).

---

## 13. Portability note (TamaGo)

The core (`context`, `sync`, `time`, `errors`, `runtime`, `math/rand/v2`, `iter`, `internal/ring`)
is TamaGo-clean. `runtime/pprof` labels, `log/slog`, and `runtime.AddCleanup` degrade gracefully;
gate the pprof-label wrap and `leak_check.go` behind a `supervise_minimal` build tag if targeting
bare metal, keeping the FSM, guard, restart engine, and abandonment protocol byte-identical. The
zero-steady-state-allocation invariant (§10.8) was chosen with that target in mind.

---

## 14. Failure-mode walkthrough (worked example)

Tree: `root(OneForOne) → net(OneForAll){dhcp, redfish, ntp}`.

1. `redfish` panics → guard converts to `*PanicError` w/ stack → `exitMsg`.
2. `net` loop: strategy OneForAll → cancel `dhcp`, `ntp` with `SiblingFailure{redfish}`.
3. `ntp` ignores ctx → grace 5 s → `Abandoned`, `EventAbandoned{net/ntp}`, counter++.
4. Intensity check passes → backoff (decorrelated jitter) → respawn `dhcp`, `redfish`, and a
   **new incarnation** of `ntp` (abandoned one still tracked; its labels distinguish incarnations
   in any goroutine profile).
5. Old `ntp` finally returns 40 s later → late `exitMsg{late:true}` → `EventLateReap`, counter--.
6. If `redfish` crash-loops past 5 restarts/60 s → `net` fails with `*IntensityError` → `root`
   applies its own policy to the `net` subtree; if root exceeds too, `Run` returns the joined
   escalation chain.

At no point can any of these paths block shutdown indefinitely, lose an exit, or leave a goroutine
unaccounted for.
