# OpenTelemetry in an Embedded Go Runtime — Architecture Document

**Status:** Draft
**Scope:** Tracing + structured logging (`log/slog`) for a bare-metal / embedded Go program (e.g. TamaGo or a constrained Linux userland), covering the full path from external API request down to hardware register access.
**Non-goals:** Metrics pipeline design (mentioned only where it interacts with tracing), remote profiling.

---

## 1. Goals and Constraints

| Goal | Constraint it collides with |
|---|---|
| End-to-end traces: API → service layer → driver → hardware access | RAM budget in the low single-digit MB for all telemetry state |
| Every log line correlatable to a trace | `slog` must not allocate per record on hot paths |
| Predictable latency; telemetry must never block the data path | OTLP export is network I/O with unbounded tail latency |
| Useful spans (not noise) | Span-per-register-access would produce thousands of spans per request |

Design rule of thumb: **spans are for causality and timing boundaries; span events and logs are for detail.** RAM is spent on structure, not on payload.

---

## 2. High-Level Architecture

```
 ┌────────────┐   ctx    ┌─────────────┐   ctx    ┌────────────┐   ctx   ┌──────────────┐
 │ API server │ ───────▶ │ service /   │ ───────▶ │ driver     │ ──────▶ │ HW access    │
 │ (root span)│          │ domain layer│          │ (child span│         │ (span events,│
 └────────────┘          └─────────────┘          │  per txn)  │         │  NOT spans)  │
       │                        │                 └────────────┘         └──────────────┘
       │ slog w/ trace ctx      │                        │
       ▼                        ▼                        ▼
 ┌──────────────────────────────────────────────────────────────┐
 │ TracerProvider                                               │
 │  - ParentBased(head sampler)                                 │
 │  - bounded BatchSpanProcessor (fixed-size ring, drop-oldest) │
 │  - attribute/event/link limits enforced at SDK level         │
 └──────────────────────────────────────────────────────────────┘
       │ export goroutine (single, low priority)
       ▼
 OTLP/gRPC or OTLP/HTTP collector (off-device)  — or —  local ring buffer + pull endpoint
```

Three structural decisions define everything else:

1. **One `context.Context` chain per external request, never broken.** The root span is created at the API ingress (HTTP/Redfish/gRPC/NATS handler). Every function on the request path takes `ctx` as its first argument, including driver and bus-transaction functions. Hardware access itself does not create spans; it records **span events** on the driver span.
2. **All telemetry state is bounded at construction time.** Span attribute counts, event counts, queue depth, and export batch size are fixed numbers chosen against the RAM budget. Overflow drops telemetry, never blocks or grows.
3. **Sampling decisions are made once, at the root.** Children inherit via `ParentBased`. This makes per-span overhead on unsampled requests nearly free and makes RAM usage proportional to *sampled* traffic only.

---

## 3. Span Model — Where Spans Begin and End

### 3.1 Span granularity ladder

| Layer | Instrument as | Rationale |
|---|---|---|
| API ingress (HTTP/gRPC/NATS handler) | Root span, `SpanKind=Server` | One per request; carries request-level attributes |
| Domain/service operation | Child span | Meaningful timing boundary (e.g. "apply sensor config") |
| Bus/driver transaction (I2C transfer, SPI transaction, MMIO burst, flash erase block) | Child span **only if the operation is ≥ ~100 µs or can fail independently** | Erase/write/long transfers deserve timing; single register reads do not |
| Individual register read/write, GPIO toggle | **Span event** on the enclosing driver span, or nothing | A span costs ~1–2 KB in the SDK; an event costs one attribute set. 500 register accesses as spans would evict everything else from the queue |
| Interrupt handlers, tight polling loops | **Nothing inline.** Counters/pre-recorded ring entries, flushed into an event afterwards | Never allocate or take SDK locks in interrupt/poll context |

### 3.2 Continuity rules (tracing "all the way down")

- Driver APIs are defined as `func (d *Dev) Transfer(ctx context.Context, ...)`. If a driver method doesn't take `ctx`, it can't participate — fix the signature, don't work around it.
- **Never use `context.Background()` below the ingress.** The only legitimate `Background()` sites are `main`, long-lived supervisors, and detached maintenance loops — and those should create their own root spans (e.g. a span per health-poll cycle, heavily sampled).
- **Goroutine handoff:** when work is queued to a worker (common in driver architectures: a single goroutine owns the bus), the trace context must travel with the job:

  ```go
  type busJob struct {
      ctx  context.Context // carries span context; do NOT use for cancellation of the worker itself
      req  Transfer
      done chan error
  }
  ```

  If storing a full `context.Context` in a queue is undesirable (lifetime confusion), store `trace.SpanContext` explicitly and rebuild:

  ```go
  ctx = trace.ContextWithSpanContext(workerCtx, job.spanCtx)
  ```

- **Cross-process / cross-transport propagation:** use W3C `traceparent`. For NATS, inject into message headers; for vendor buses (e.g. MCTP, IPMB) where no header space exists, propagate only within the device and create a **span link** at the boundary instead of pretending continuity exists.
- **Fire-and-forget detachment:** for work that outlives the request (e.g. deferred flash commit), start a new root span with a **link** to the originating span rather than parenting it — otherwise the request span's trace stays "open" in analysis tooling and cancellation semantics get tangled.

### 3.3 What goes *in* a span (RAM discipline)

Budget per sampled span, enforced via SDK limits:

```go
sdktrace.NewTracerProvider(
    sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{
        AttributeCountLimit:       8,
        AttributeValueLengthLimit: 64,
        EventCountLimit:           16,
        LinkCountLimit:            2,
        AttributePerEventCountLimit: 4,
    }),
    ...
)
```

Attribute policy:

- **Identity, not payload.** Record `i2c.bus=3 i2c.addr=0x4c op=write len=2`, never the transferred bytes. Payload belongs in a debug log behind a level gate, if anywhere.
- **Static keys, bounded values.** Every attribute key is a package-level `attribute.Key` constant. Values must be from a bounded set (bus number, device address, opcode name) — no user input, no error strings as attribute *keys*, no formatted strings.
- **Errors:** `span.SetStatus(codes.Error, "")` + `span.RecordError(err)` (which becomes one event). Do not additionally copy the error text into three attributes.
- Resource attributes (firmware version, board ID, service name) are set **once** on the `Resource`, not per span.

---

## 4. Sampling and Buffering — the RAM Story

### 4.1 Memory model

Approximate steady-state RAM cost:

```
RAM ≈ queue_depth × avg_span_size
    + max_export_batch × avg_span_size   (marshaling buffer)
    + per-active-request live span state
```

With `avg_span_size ≈ 1.5 KB` (8 attrs, few events), a queue of 512 spans plus a 128-span batch is ~1 MB. Pick numbers from the budget, then derive the sampling rate that keeps you under it at peak request rate — not the other way around.

### 4.2 Sampler

```go
sampler := sdktrace.ParentBased(
    sdktrace.TraceIDRatioBased(0.05), // steady-state
)
```

- **Head sampling only** on-device. Tail sampling requires buffering whole traces — that is a collector-side job.
- Make the ratio **runtime-adjustable** (atomic value read by a custom `Sampler`): 100% during bring-up/debug sessions, ≤5% in production, 0% as a kill switch. This is the single most valuable operational knob.
- Consider **always-sample for errors**: you cannot head-sample on outcome, but you *can* run a cheap custom sampler that always samples known-rare endpoints (firmware update, cold boot) and ratio-samples chatty ones (sensor polling). Route decisions on span name/attributes at `ShouldSample` time.
- Unsampled spans still propagate context (`trace.SpanContext` with `sampled=false` flag), so downstream logs remain correlatable even when the span is dropped — an argument for logging trace IDs unconditionally (§5).

### 4.3 Processor and exporter

- Use `BatchSpanProcessor` with explicit small limits:

  ```go
  sdktrace.WithBatcher(exporter,
      sdktrace.WithMaxQueueSize(512),
      sdktrace.WithMaxExportBatchSize(128),
      sdktrace.WithBatchTimeout(5*time.Second),
      sdktrace.WithBlocking() /* NEVER set this */,
  )
  ```

  Default behavior on full queue is **drop** — that is correct. `WithBlocking()` turns telemetry into a data-path stall; it must never be enabled on-device.
- **Never `SimpleSpanProcessor` in production.** It exports synchronously inside `span.End()`, putting network latency on the request path.
- Export over **OTLP/HTTP with gzip off** if CPU-bound, or OTLP/gRPC if a connection is long-lived anyway. On very constrained targets, export to a **local fixed-size ring buffer** exposed via a pull endpoint (`GET /debug/traces`) so the network stack cost is paid only when someone is looking.
- Exporter failures must be **fail-open**: drop the batch, increment a self-observability counter (`otel.dropped_spans`), retry with capped backoff. No unbounded retry queue.
- Instrument the instrumentation: expose `dropped_spans`, `queue_high_watermark`, `export_failures` as plain atomic counters readable via the management API. When sampling/limits silently drop data, you need to know.

---

## 5. slog Integration

### 5.1 Bridge direction

Two distinct integrations; use both, deliberately:

1. **Trace context → log records (correlation).** A `slog.Handler` middleware that stamps `trace_id` and `span_id` from `ctx` onto every record. This is cheap, works even for unsampled traces, and is the primary debugging tool when spans were sampled away.
2. **Log records → span events (optional, gated).** Bridging *every* log line into the active span as an event (as `otelslog` from `go.opentelemetry.io/contrib/bridges/otelslog` does toward the Logs API) multiplies telemetry volume. On embedded targets, restrict it: only `WARN`+ records become span events, and only when the span is recording.

### 5.2 Reference handler

```go
type traceHandler struct{ inner slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
    if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
        r.AddAttrs(
            slog.String("trace_id", sc.TraceID().String()),
            slog.String("span_id", sc.SpanID().String()),
        )
        // Escalate significant logs into the trace, bounded by SpanLimits.
        if r.Level >= slog.LevelWarn {
            if span := trace.SpanFromContext(ctx); span.IsRecording() {
                span.AddEvent(r.Message, trace.WithAttributes(
                    attribute.String("log.severity", r.Level.String()),
                ))
                if r.Level >= slog.LevelError {
                    span.SetStatus(codes.Error, "")
                }
            }
        }
    }
    return h.inner.Handle(ctx, r)
}
```

Notes:

- `TraceID().String()` allocates (hex encoding). If the log backend supports it, emit the raw 16/8-byte values via a custom `slog.LogValuer` and hex-encode off-device. Otherwise accept the two small allocations on *emitted* records only — the `slog.Handler.Enabled` gate must run first so disabled levels cost nothing.
- **Always call `logger.InfoContext(ctx, ...)` variants.** A `slog` call without `ctx` silently loses correlation; make the non-context methods unavailable via a thin wrapper type or a lint rule (`sloglint` supports enforcing context usage).
- Shared vocabulary: attribute keys used in spans and logs must come from **one constants package** (`telemetry/keys`), following OTel semantic conventions where they exist (`net.peer.*`) and a project-local `hw.*` namespace (`hw.bus`, `hw.addr`, `hw.reg`) where they don't. Divergent key names between logs and spans destroy cross-correlation queries.

### 5.3 Level strategy

- `DEBUG`: register-level detail; compiled to a ring buffer or disabled in production builds. Never bridged to spans.
- `INFO`: state transitions; correlated but not bridged.
- `WARN`/`ERROR`: bridged as span events; errors also set span status.

---

## 6. Latency Discipline

Hot-path rules, in priority order:

1. **Check before you build.** Guard any attribute construction with `span.IsRecording()`. For unsampled requests this reduces span cost to a context lookup:

   ```go
   if span.IsRecording() {
       span.SetAttributes(keyBus.Int(busNo), keyAddr.Int(int(addr)))
   }
   ```

2. **No `fmt.Sprintf` into attributes or span names.** Span names are compile-time constants (`"i2c.transfer"`, not `fmt.Sprintf("i2c.transfer[%#x]", addr)` — dynamic names also explode cardinality in backends).
3. **Preallocate attribute sets** for fixed configurations (per-device attribute slices built at driver init, reused via `trace.WithAttributes(dev.attrs...)`).
4. **`Tracer` handles are cached** at package init (`var tracer = otel.Tracer("kyanite/i2c")`), never fetched per call.
5. **Time-critical sections don't call the SDK at all.** For a bit-banged protocol or a timing-sensitive erase sequence: capture `time.Now()` (or a cycle counter) before/after, record the event *after* the critical section. `span.AddEvent` supports `trace.WithTimestamp(t)` for retroactive recording.
6. **GC interaction (TamaGo / soft-real-time):** span end pushes onto the processor queue; the batch processor's marshal step is the main allocation burst. Pin export batch size low and, where the runtime allows, schedule export during known-idle windows. Telemetry allocations are the first thing to shed when tuning GC pause budgets — the runtime-adjustable sampler (§4.2) is the mechanism.
7. Budget targets (validate with benchmarks, `-benchmem`):
   - Unsampled span start/end: **< 200 ns, 0–1 allocs**
   - Sampled span with 6 attrs + 2 events: **< 2 µs**
   - `slog.InfoContext` at disabled level: **< 50 ns, 0 allocs**

---

## 7. Common Pitfalls

| # | Pitfall | Consequence | Correction |
|---|---|---|---|
| 1 | `context.Background()` in a driver or worker goroutine | Trace breaks; API span and hardware activity uncorrelatable | Pass ctx through queues (§3.2); rebuild from stored `SpanContext` |
| 2 | Span per register access | Queue floods, everything else dropped; KB-per-span RAM burn | Span events on the driver-transaction span |
| 3 | `WithBlocking()` batch processor, or `SimpleSpanProcessor` | Network tail latency injected into request path; deadlock risk during export outage | Non-blocking batcher, drop on overflow |
| 4 | Unbounded attribute values (error strings, payload dumps, paths) | Per-span RAM unbounded; value-length limits truncate uselessly | Identity attributes only; `RecordError` for errors; payloads to gated debug logs |
| 5 | Dynamic span names / high-cardinality attributes (`reg=0x1a2b` as key, per-request IDs as names) | Backend cardinality explosion; on-device string churn | Constant names; bounded value sets; IDs go in one attribute |
| 6 | Forgetting `defer span.End()` on error paths | Span leaks: never exported, held in memory until GC, trace appears truncated | `defer span.End()` immediately after `Start`, no exceptions |
| 7 | Bridging all `slog` records into span events | Event limit (16) consumed by chatter; RAM ×N | `WARN`+ only, `IsRecording()`-gated |
| 8 | Instrumenting ISRs / poll loops with SDK calls | Locking + allocation in interrupt context; jitter | Record raw timestamps, emit events after the fact |
| 9 | Creating a new root span mid-stack "because ctx wasn't available" | Orphan traces; band-aids over broken plumbing | Fix signatures to accept ctx; treat missing ctx as an API bug |
| 10 | Exporter retry with unbounded queue during collector outage | Slow-motion OOM on device | Capped retries, drop, count drops |
| 11 | Divergent attribute keys between logs and spans (`bus` vs `i2c.bus`) | Correlation queries fail silently | Single shared key-constants package |
| 12 | Sampling decision re-made per layer | Partial traces (parent dropped, child kept) | `ParentBased` everywhere; decide once at root |
| 13 | `otel.Tracer(...)` / `slog.With(...)` inside hot functions | Per-call map lookups and allocations | Package-level tracer vars; prebuilt loggers/attr sets |
| 14 | Trusting defaults (128 attrs, 2048 queue, unlimited value length) | Defaults sized for servers, not devices | Set every limit explicitly (§3.3, §4.3) |

---

## 8. Patterns to Promote

- **`ctx`-first everywhere, including drivers.** The signature *is* the tracing architecture.
- **Span = timing boundary; event = fact; log = detail.** One sentence, applied at every review.
- **Bounded-by-construction telemetry:** every queue, batch, attribute count, and value length is an explicit number derived from the RAM budget, with drop-not-block overflow.
- **`ParentBased(ratio)` head sampling with a runtime knob** and self-observability counters for drops.
- **Retroactive event recording** (`WithTimestamp`) to keep the SDK out of timing-critical sections.
- **Shared semantic-key package** spanning traces and logs; OTel semconv where applicable, `hw.*` namespace for hardware.
- **`IsRecording()` guards** around all non-trivial attribute construction.
- **Span links, not parenting, across detachment boundaries** (deferred work, non-propagating buses).
- **Trace IDs on every log record regardless of sampling** — logs are the fallback when spans were sampled out.
- **Kill switch:** sampler → 0 and exporter → no-op, switchable at runtime, so telemetry can never be the reason a device is down.

---

## 9. Open Questions / Follow-ups

1. Exporter transport for fully bare-metal targets (no TCP idle budget): local ring + pull endpoint vs. push-on-idle heuristic.
2. Whether the OTel Logs API (via `otelslog` bridge) should ship at all, or logs stay on an independent lighter path with only trace-ID correlation. Current recommendation: correlation only; revisit when the Go Logs SDK's allocation profile is measured on target.
3. Cycle-counter-based span timestamps vs. `time.Now()` monotonic clock on the target runtime — needed if µs-level hardware timing accuracy matters.
4. Metrics: which counters graduate from atomic self-observability counters to a real OTel `MeterProvider` (likely none on the smallest targets).
