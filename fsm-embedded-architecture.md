# Embedded State Machine Library — Evaluation of `qmuntal/stateless` and Tailored Architecture

**Status:** Draft
**Context:** Companion to `supervise` (pure-Go supervisor, actor model, Go 1.26/1.27, stdlib-only)
and the OTel-embedded-Go architecture. Target runtimes: TamaGo bare-metal, constrained Linux
userland (Cairn/Vein). Working name used below: `fsm` (placeholder).

---

## 1. Evaluation of `qmuntal/stateless` (v1.8.0)

Solid semantics — UML statechart model, hierarchical states, guards, queued run-to-completion,
ctx-first callbacks, DOT export. The problems are all *mechanical*, and they are exactly the
class of problems that matter on embedded targets.

### 1.1 What to keep

| Feature | Verdict |
|---|---|
| Run-to-completion queued firing as default | Keep — correct semantics for protocol FSMs |
| Hierarchical states + `IsInState` substate semantics | Keep |
| Guards evaluated at fire time, required side-effect free | Keep |
| `OnEntryFrom` (trigger-specific entry actions) | Keep |
| Reentry vs. ignore as distinct, explicit configurations | Keep |
| Initial transitions into substates | Keep |
| ctx threading through every callback | Keep — prerequisite for span continuity |
| DOT export (code is the authoritative diagram source) | Keep, extend to Mermaid |
| External state storage concept | Keep, redesign (see §5) |

### 1.2 What disqualifies it as-is for TamaGo/Cairn

1. **`State = any`, `Trigger = any`.** Every state/trigger is boxed; lookups go through
   `map[any]`; every `Fire` allocates via `args ...any`. No compile-time relationship between a
   trigger and its payload type. On a target where the GC pause budget is shared with hardware
   servicing, per-event heap traffic is a defect, not a style issue.
2. **`reflect` + `runtime.FuncForPC`.** Trigger parameter validation is runtime `reflect.Type`
   checking that **panics** on mismatch; guard names for diagnostics are derived from function
   pointers via reflection. `reflect` inflates TamaGo binaries and moves type errors from compile
   time to a panic in the field.
3. **Panic as API.** Unhandled trigger panics by default (opt-out via `OnUnhandledTrigger`);
   parameter mismatch panics unconditionally. In firmware, every panic path is a reboot path.
   The `supervise` guard would contain it, but a *library* should never make the supervisor the
   error-handling mechanism for ordinary inputs (an unexpected IPMI command is not exceptional).
4. **"Thread-safe" via mutexes, with a silent-drop hazard.** `fireModeQueued` guards a growable
   slice with a mutex; if goroutine A is mid-fire, goroutine B's `Fire` enqueues and returns
   `nil` — B never learns whether its trigger ran or what error it produced. Error attribution
   is lost exactly when two writers race. This is the same defect class rejected in `supervise`
   (mutex-shared tree state in oversight): the fix is ownership, not locking.
5. **Unbounded internal queue.** `append` on the trigger slice — unbounded memory under trigger
   storms (e.g. an interrupt source going pathological). Everything must be
   bounded-by-construction, same rule as the telemetry pipeline.
6. **Runtime config forever mutable.** `Configure` can be called at any time; every fire walks
   maps and behaviour slices. No freeze point, no validation pass, no dense representation.
7. **Allocating diagnostics.** Unmet-guard reporting builds `[]string` per rejection.
8. **Missing protocol-FSM primitives.** No state timeouts, no deferred/postponed events, no
   event payload typing. These are not exotic: SPDM sequencing, MCTP reassembly windows, IPMI
   session expiry, link-state debounce all need timers and postponement as first-class concepts
   (Erlang `gen_statem`: `state_timeout`, `postpone`).

Conclusion: adopt the *semantics*, reject the *representation*. As with `supervise`, this is a
synthesis-and-correct design, not a fork. (BSD-2-Clause makes borrowing test vectors and DOT
formatting legitimate.)

---

## 2. Design principles

1. **Two-phase: Builder → compiled immutable Machine.** All maps, reflection-free validation,
   cycle/reachability checks, and hierarchy flattening happen once in `Build()`. The runtime
   object is a dense transition table over small integers. `Build()` returns `error`; after it,
   nothing allocates on the fire path.
2. **Generics, zero `reflect`, zero `any` in the public API.**
3. **Single-owner, not "thread-safe".** The machine is owned by one goroutine — in practice a
   `supervise` child. Cross-goroutine input arrives through that child's channel/mailbox, not
   through internal FSM locking. The library itself has zero mutexes and zero atomics on the
   fire path. A thin `Mailbox` adapter is provided for the ownership boundary, mirroring the
   supervisor's actor discipline.
4. **Errors, never panics, for input-shaped failures.** Panics only for programmer errors caught
   at `Build()` time — and even those are returned as errors.
5. **Bounded-by-construction.** Internal run-to-completion queue and deferral buffer are
   fixed-capacity rings (reuse `internal/ring` from `supervise`); overflow is a typed error plus
   a counter, never a block, never a grow.
6. **stdlib only**; TamaGo-clean (`context`, `errors`, `time` via injectable clock, `log/slog`
   optional bridge, `iter` for introspection).
7. **Telemetry per the embedded-OTel rules:** a transition is a **span event, not a span**; the
   FSM never creates spans; hooks are `IsRecording()`-guarded by the caller; drop/reject/timeout
   counters are plain atomics exposed for scraping.

---

## 3. Public API sketch

```go
package fsm

// States and triggers are small integer enums. This is a deliberate constraint,
// not a limitation: it is what makes the dense table, DOT golden tests, and
// wire-format persistence possible.
type Machine[S ~uint8, T ~uint8, E any] struct{ /* compiled, immutable config + tiny mutable core */ }

// E is the single typed event payload (often a small struct or a sum-type-ish
// tagged struct). One payload type per machine replaces SetTriggerParameters +
// reflect entirely. Machines needing heterogeneous payloads use a tagged union
// struct — explicit, allocation-free, greppable.

type Builder[S ~uint8, T ~uint8, E any] struct{ ... }

func New[S ~uint8, T ~uint8, E any](initial S, opts ...Option) *Builder[S, T, E]

func (b *Builder[S, T, E]) State(s S) *StateCfg[S, T, E]

type StateCfg[S ~uint8, T ~uint8, E any] struct{ ... }
func (c *StateCfg[...]) Parent(s S) *StateCfg[...]                 // hierarchy
func (c *StateCfg[...]) Initial(sub S) *StateCfg[...]              // initial transition
func (c *StateCfg[...]) Permit(t T, dst S, guards ...Guard[E]) *StateCfg[...]
func (c *StateCfg[...]) PermitDynamic(t T, f func(context.Context, E) (S, error), guards ...Guard[E]) *StateCfg[...]
func (c *StateCfg[...]) Internal(t T, a Action[S, T, E]) *StateCfg[...] // no exit/entry
func (c *StateCfg[...]) Reentry(t T) *StateCfg[...]
func (c *StateCfg[...]) Ignore(t T, guards ...Guard[E]) *StateCfg[...]
func (c *StateCfg[...]) Defer(t ...T) *StateCfg[...]               // gen_statem postpone
func (c *StateCfg[...]) OnEntry(a Action[S, T, E]) *StateCfg[...]
func (c *StateCfg[...]) OnEntryFrom(t T, a Action[S, T, E]) *StateCfg[...]
func (c *StateCfg[...]) OnExit(a Action[S, T, E]) *StateCfg[...]
func (c *StateCfg[...]) Timeout(d time.Duration, t T) *StateCfg[...] // state_timeout → fires t

func (b *Builder[S, T, E]) Build() (*Machine[S, T, E], error)

type Guard[E any] struct {
    Name string                                // explicit — no FuncForPC
    Fn   func(context.Context, E) bool
}
type Action[S ~uint8, T ~uint8, E any] func(context.Context, Transition[S, T], E) error
type Transition[S, T ~uint8] struct{ Source, Destination S; Trigger T; Kind Kind } // Kind: External|Internal|Reentry|Initial|Timeout

// Runtime (single-owner; no internal locking):
func (m *Machine[S, T, E]) Fire(ctx context.Context, t T, ev E) error
func (m *Machine[S, T, E]) State() S
func (m *Machine[S, T, E]) IsIn(s S) bool                       // substate-aware
func (m *Machine[S, T, E]) CanFire(ctx context.Context, t T, ev E) bool
func (m *Machine[S, T, E]) Permitted(ctx context.Context, ev E) iter.Seq[T]
func (m *Machine[S, T, E]) Tick(now time.Time)                  // drives state timeouts; see §6
```

Typed errors (mirroring `supervise` error taxonomy, extractable via `errors.AsType`):

```go
type UnhandledError[S, T ~uint8] struct{ State S; Trigger T }
type GuardsRejectedError[S, T ~uint8] struct{ State S; Trigger T; Guards []string } // names filled only if enabled
type QueueOverflowError[T ~uint8] struct{ Trigger T; Capacity int }
type ActionError[S, T ~uint8] struct{ Transition Transition[S, T]; Err error }      // wraps; Unwrap() error
type BuildError struct{ ... }  // unreachable state, guard overlap, hierarchy cycle, initial-transition loop, duplicate (state,trigger)
```

---

## 4. Compiled representation (the actual embedded payoff)

`Build()` lowers the fluent config into:

- `table [numStates * numTriggers]entry` — flat array, `entry` is a packed struct
  (`dst S`, `kind uint8`, `guardSet uint8`, `actionSet uint8` → indices into small slices).
  Lookup is one multiply-add. No map, no hash of `any`, no pointer chasing.
- Hierarchy resolved at build time: each concrete state's effective handler for each trigger is
  precomputed by walking the parent chain **once**, in `Build()` — never per fire. `IsIn` is a
  precomputed ancestor bitset per state (`[numStates]uint64` for ≤64 states, spilling to a slice
  above that).
- Entry/exit chains for every (source, destination) pair are *not* precomputed (quadratic);
  instead the least-common-ancestor per state pair is precomputed (small triangular matrix), so
  the exit-up/enter-down walk is direct with zero searching.
- Guard evaluation order fixed at build time; **overlapping guards for the same (state, trigger)
  are a `BuildError`** unless explicitly marked `Else()` — stateless documents mutual exclusivity
  as a convention but cannot enforce it; enforcing it at build time removes a whole class of
  nondeterminism.
- Memory: for a typical protocol FSM (≤32 states, ≤32 triggers) the whole compiled machine is a
  few KB, allocation-free after `Build()`, and shareable read-only across multiple machine
  *instances* (config/instance split: `*Config` immutable + `Instance{state S, rings}` tiny —
  relevant for per-session FSMs like IPMI/SPDM sessions where hundreds of instances share one
  table).

Run-to-completion: `Fire` during an action (same goroutine, by construction) pushes onto a
fixed ring (default cap 8, build-time option) and the outer `Fire` drains it. Deferred triggers
go to a second fixed ring and are re-offered in FIFO order after every state change. Both rings
are inline arrays in `Instance` — zero heap.

---

## 5. Persistence (external state storage, redesigned)

stateless routes *every* state read/write through user callbacks under a mutex. Invert it:

```go
type Snapshot[S ~uint8] struct {
    Version   uint32   // config hash from Build() — restore fails typed on mismatch
    State     S
    Deferred  []uint8  // deferred-trigger ring contents
}
func (m *Machine[S, T, E]) Snapshot() Snapshot[S]
func (m *Machine[S, T, E]) Restore(s Snapshot[S]) error // ErrSnapshotVersion, ErrUnknownState
```

- Fire path never touches storage. The owner decides when to persist (post-transition hook →
  write-through to Scree / NVRAM / JetStream KV).
- `Version` is a stable hash of the compiled table, so a firmware update that changes the FSM
  topology cannot silently restore into a state that no longer means the same thing —
  restoration failure is explicit and the owner picks a migration state.
- Composes with `supervise` restarts: a `Transient` child restores its snapshot in its own
  startup, giving crash-restart-resume without the FSM library knowing about supervisors at all.

---

## 6. Time (state timeouts) without owning a goroutine

The library must not spawn goroutines or own timers (supervisor accounting would be blind to
them, and TamaGo timer discipline belongs to the runtime owner). Instead:

- `Timeout(d, trigger)` records a deadline on state entry (any timescale; deadline is
  `entryTime + d` using an injected clock).
- `m.NextDeadline() (time.Time, bool)` tells the owner when to wake.
- `m.Tick(now)` fires the timeout trigger internally (Kind `Timeout`) if expired.

The owning `supervise` child integrates this into its existing single `time.Timer` + select
loop — same one-owned-timer pattern as the supervisor's min-heap, and `synctest`-testable for
free via the shared injectable clock.

---

## 7. Observability (aligned with the OTel-embedded doc)

- **No spans created by the FSM.** A transition is a fact: hook `OnTransitioned` records a span
  event on the caller's active span (`trace.SpanFromContext(ctx)`), guarded by `IsRecording()`
  in the *bridge*, not in core. Core exposes only a plain callback.
- Recommended event attributes (shared key-constants package): `fsm.name`, `fsm.from`,
  `fsm.to`, `fsm.trigger`, `fsm.kind` — all low-cardinality by construction since states and
  triggers are enums with `String()` generated (see §8).
- **Counters (plain atomics, exported):** fires, transitions, rejected-by-guard, unhandled,
  deferred, deferral-overflow, queue-overflow, timeouts. These graduate to OTel metrics only on
  targets that run a MeterProvider.
- **Dwell time:** entry timestamp retained (one `time.Time` per instance); exposed on the
  transition hook so long-dwell alarms are the owner's policy.
- **slog bridge (optional subpackage):** one `slog.LogAttrs` per transition at Debug,
  rejections at Warn, using preallocated `slog.Attr` patterns; trace/span IDs stamped by the
  handler, per the existing logging architecture.

---

## 8. Tooling

- `String()` for state/trigger enums via `go:generate stringer` (or a tiny bundled generator to
  avoid the x/tools dependency in constrained CI) — names exist for diagnostics without any
  runtime reflection.
- DOT **and** Mermaid export from the compiled table; golden tests over both (stateless's
  `testdata/golden` approach is worth copying directly).
- Build-time analyses reported in `BuildError` details: unreachable states, states with no exit
  (flag, not error — terminal states are legitimate), triggers never used, guard overlap.
- Exhaustive property test helper: for fuzzing, `AllTransitions() iter.Seq[Transition[S,T]]`
  lets a test drive every edge and assert invariants; pairs with `testing/synctest` for
  timeout edges.

---

## 9. Package layout

```
fsm/
├── fsm.go          // Machine, Instance, Fire, Tick, Snapshot/Restore
├── builder.go      // Builder, StateCfg, Build() lowering + validation
├── table.go        // packed entry, LCA matrix, ancestor bitsets
├── errors.go       // typed errors
├── introspect.go   // Permitted, AllTransitions, iter-based API
├── export.go       // DOT + Mermaid
├── counters.go     // atomic counters block
├── otelbridge/     // optional: span-event + metric bridge (only place importing otel)
├── slogbridge/     // optional: slog transition logging
└── internal/       // shares nothing with supervise at first; ring may be vendored
```

Core imports: `context`, `errors`, `fmt` (errors only), `time`, `iter`. No `sync`, no
`reflect`, no `runtime` beyond what stdlib pulls. The otel bridge is the only module boundary
with third-party dependencies, keeping core fit for TamaGo, Cairn coprocessor daemons, and the
Vein control plane alike.

---

## 10. Divergence summary vs. `qmuntal/stateless`

| Axis | stateless | this design |
|---|---|---|
| Typing | `any` states/triggers, reflect param checks | generic enums + one typed payload |
| Failure mode | panic on unhandled/param mismatch | typed errors; panics impossible post-Build |
| Concurrency | internal mutexes, "thread-safe" | single-owner + mailbox adapter; zero locks |
| Fire queue | unbounded slice, silent cross-goroutine drop of error attribution | fixed ring, typed overflow error |
| Config | mutable forever, map-walk per fire | Build() → immutable dense table, O(1) dispatch |
| Guards | mutual exclusivity by convention | overlap is a build error unless `Else()` |
| Timers/deferral | absent | state timeouts (owner-driven Tick) + Defer rings |
| Persistence | per-access callbacks under mutex | Snapshot/Restore with config-hash versioning |
| Diagnostics | FuncForPC names, alloc-per-rejection | explicit names, opt-in, alloc-free counters |
| Instances | one config = one machine | Config/Instance split for per-session FSMs |

---

## 11. Open questions

1. Should `E` support a build-time per-trigger payload discriminant check (tag field asserted in
   `Fire`), or stay fully caller-disciplined? Leaning: optional `Validate func(T, E) error` hook.
2. Deferral re-offer semantics on hierarchical transitions that don't change the leaf state —
   follow gen_statem (re-offer only on actual state change) or on every handled event?
3. Whether `Instance` should expose an epoch/incarnation counter for the same stale-message
   defense used in `supervise`, or leave that to the owning child (leaning: owner's concern).
4. Shared `clock` package between `supervise` and `fsm`, or duplicated 40-line interface to keep
   the modules independent (leaning: duplicate; module coupling is worse than 40 lines).
