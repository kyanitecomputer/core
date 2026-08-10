# Kyanite core — roadmap / remaining work

Durable handoff of pending work, meant to survive context compaction. Detailed
per-domain notes live where the work is:

- **Auth** remaining work: `nats-auth-architecture.md` §19 (authoritative).
- **Hardware** resume point: `../vein/notes-interrupts.md` ("HARDWARE BRING-UP
  RESUME POINT").
- Architecture specs: `supervise-architecture.md`, `fsm-embedded-architecture.md`,
  `otel-embedded-go-architecture.md`, `nats-auth-architecture.md`.

## Status snapshot (done)

| Package | State |
|---|---|
| `supervise` | Slices 1, 2, 4 done. Slice 3 (futures/combinators) deferred. |
| `fsm` | Slices 1–5 done (flat, hierarchy, defer+timeouts, snapshot+export, observer+bridges). Feature-complete. |
| `telemetry` (+`/keys`) | Attribute vocabulary + slog↔trace correlation (P2) done. Tracing SDK (P4) pending. |
| `bus` | `Conn` (auth-aware), `micro` `Service`, Scree-aware JetStream `Streams` done. |
| `auth` | Policy compiler, LOCAL registry, issuer, LOCAL callout, live `Plane`. See §19. |
| `natscore` | Bounded server + Scree JS store + `EnableAuthCallout`. |
| `mgmt` | Node bootstrap (server + auth + Scree). Host-validated; on-device pending. |
| `vein`/`cairn` | `mgmt` scaffolding wired into `main.go`; both ELFs build. **UNCOMMITTED** (standing policy: do not commit vein/cairn until asked). |

All committed core packages test green and build on tamago arm64 + riscv64.

## Remaining work by area

### fsm (optional follow-ups; feature-complete otherwise)
- `AllTransitions() iter.Seq[Transition]` helper for fuzz/property tests (§8).
- Build-time analyses reported in `BuildError`: unreachable states, never-used
  triggers (flags, not hard errors — terminal states are legitimate).

### supervise
- Slice 3 (deferred, path-2): futures + combinators + `runtime.AddCleanup`
  tripwire. Build only when a consumer needs it.

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
  network stack's `SocketFunc` is wired (vein has one; cairn needs network
  bring-up first).

### vein / cairn (adoption + housekeeping)
- **Uncommitted scaffolding**: `target/*/main.go` now bootstraps `mgmt` and runs
  a demo LOCAL actor over the authorized bus. Commit when asked.
- **`vein/TODO.md` is partly superseded**: its NATS-core Phases 1–4 are largely
  realized in `core/{natscore,bus,mgmt}`; Phases 5–7 (config-over-KV, user
  management, drop-REST) are still open but now land via `core/auth` + `bus`.
  Reconcile that doc against the core implementation.
- Migrate the switch protocol state machines (`vein/.../sw/*`: LLDP/RSTP/LACP/
  IGMP/ERPS/Dot1X) onto `core/fsm`.
- Adopt telemetry correlation and the `fsm` slog/otel bridges in the agents.
- Replace the RAM-backed Scree store with flash-backed storage (QSPI NOR on
  vega; SPI-NOR on cairn) for persistence.

### hardware (resume after compaction)
See `../vein/notes-interrupts.md` for full context. Summary of open items:
- **EMAC bring-up**: MAC revision reads `0x00000000` on cold boot (block
  clock-gated). `eth.EnableSubsystem()` is flashed but UNTESTED. Next: flash,
  read the `[eth] subsystem pre/post` line, confirm `rev` goes non-zero, plug a
  cable, check link LED + `net stats rx`. If `rev` non-zero but no link,
  reverse-engineer per-PHY MDIO power-up/autoneg from `libsdk.so` (ghidra-cli).
- **Watchdog**: arm + kick the FSL91030M watchdog from a supervised goroutine so
  a fatal runtime crash auto-resets instead of hanging.
- **(Later)** rewrite the vega TamaGo fork's RISC-V trap entry/exit for safe
  async interrupts (`RET`→`MRET`, full frame, dedicated stack), then move RX
  from polling to interrupt-driven.

## Build / test reference

- Stdlib-only core packages (`-race`, no workspace):
  `GOWORK=off <tamago-go> test -race ./supervise/ ./fsm/` (also `service`,
  `operator`, `cfgstore`, `ramnor`).
- NATS/otel-dependent core packages (need the workspace): run from a runtime
  repo, e.g. `cd ../cairn && GOTOOLCHAIN=local <tamago-go> test
  src.kyanite.computer/core/{bus,auth,mgmt,natscore,telemetry}`.
- Tamago ELF builds: `cd ../cairn && make build` (arm64),
  `cd ../vein && make build` (riscv64).
- `<tamago-go>` = `/home/mdr164/private/tamago/tamago-go/bin/go` (go1.27rc1
  tamago port). Per-arch build env:
  `GOOS=tamago GOARCH={arm64|riscv64} GOOSPKG=github.com/usbarmory/tamago
  GOTOOLCHAIN=local`.
