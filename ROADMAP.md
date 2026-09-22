# Kyanite core — roadmap

Pending and in-progress work across the core device microkernel. Detailed
per-domain design notes live alongside the code:

- **Auth**: `nats-auth-architecture.md` (§19 lists remaining work in priority order).
- **Supervision**: `supervise-architecture.md`.
- **FSM**: `fsm-embedded-architecture.md`.
- **Telemetry**: `otel-embedded-go-architecture.md`.

## Status snapshot

| Package | State |
|---|---|
| `supervise` | Slices 1, 2, 4 done. Slice 3 (futures/combinators) deferred. |
| `fsm` | Slices 1–5 done (flat, hierarchy, defer+timeouts, snapshot+export, observer+bridges). Feature-complete. |
| `telemetry` (+`/keys`) | Attribute vocabulary + slog↔trace correlation (P2) done. Tracing SDK (P4) pending. |
| `bus` | `Conn` (auth-aware), `micro` `Service`, Scree-aware JetStream `Streams` done. |
| `auth` | Policy compiler, LOCAL registry, issuer, LOCAL callout, live `Plane`. See §19. |
| `natscore` | Bounded server + Scree JS store + `EnableAuthCallout`. |
| `mgmt` | Node bootstrap (server + auth + Scree). Host-validated; on-device pending. |

Core packages test green and build on TamaGo arm64 + riscv64.

## Remaining work by area

### fsm (optional follow-ups; feature-complete otherwise)
- `AllTransitions() iter.Seq[Transition]` helper for fuzz/property tests (§8).
- Build-time analyses reported in `BuildError`: unreachable states, never-used
  triggers (flags, not hard errors — terminal states are legitimate).

### supervise
- Slice 3 (deferred): futures + combinators + `runtime.AddCleanup` tripwire.
  Build only when a consumer needs it.

### telemetry
- P4: gated tracing SDK (ParentBased head sampler, non-blocking BSP, exporter)
  behind a build tag / runtime knob, per `otel-embedded-go-architecture.md`.
  Today: correlation-only (IDs stamped, WARN+→span events); no spans exported.

### bus
- Trace-context extraction from request headers in the endpoint handler wrapper
  (`service.go:wrapHandler` currently starts each request from
  `context.Background()`).
- JetStream consumers (durable push/pull `Consume`) — add when a real consumer
  exists (mesh/telemetry), per the "real consumers first" discipline.
- KV bucket helpers — add when config/registry moves onto KV.

### auth
- Authoritative list in `nats-auth-architecture.md` §19. Priority order: XKey
  encryption, LOCAL signed-nonce verification, live-restart hygiene (mailbox),
  revocation/kick + CONNZ, OPER token plane, network listeners, clock-gating
  FSM, audit stream, rate limiting, MESH mTLS, TEE-sealed issuer, supervised
  auth subtree.

### mgmt
- On-device validation of the embedded NATS server on the boot path (only
  host-validated so far). Ties to §18.3 (`DontListen`/listener granularity on
  the bare-metal port).
- Network listeners (websocket for the WebUI; leafnodes for mesh) once the
  network stack's `SocketFunc` is wired.

### downstream adoption (runtimes)
- Migrate the switch protocol state machines (LLDP/RSTP/LACP/IGMP/ERPS/Dot1X)
  onto `core/fsm`.
- Adopt telemetry correlation and the `fsm` slog/otel bridges in the agents.
- Replace RAM-backed Scree stores with flash-backed storage (QSPI/SPI-NOR) for
  persistence.

## Build / test reference

- Stdlib-only core packages (`-race`, no workspace):
  `GOWORK=off <tamago-go> test -race ./supervise/ ./fsm/` (also `service`,
  `operator`, `cfgstore`, `ramnor`).
- NATS/otel-dependent core packages need the workspace; run from a runtime repo,
  e.g. `GOTOOLCHAIN=local <tamago-go> test
  src.kyanite.computer/core/{bus,auth,mgmt,natscore,telemetry}`.
- `<tamago-go>` = the path to your tamago-go binary (go1.27 TamaGo port).
  Per-arch build env:
  `GOOS=tamago GOARCH={arm64|riscv64} GOOSPKG=github.com/usbarmory/tamago
  GOTOOLCHAIN=local`.
