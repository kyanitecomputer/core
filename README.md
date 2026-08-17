# core

The shared device microkernel for the [Kyanite](https://github.com/kyanitecomputer)
stack. Part of the Kyanite stack.

> **Status:** experimental — expect breaking changes.

## Overview

`core` provides the common runtime that Kyanite device firmware (such as
[cairn](https://github.com/kyanitecomputer/cairn) and
[vein](https://github.com/kyanitecomputer/vein)) is assembled from: an
actor-style supervision tree, an embedded NATS message bus with capability-based
auth, telemetry, a compiled state-machine engine, configuration storage, and the
management/web surfaces. It is designed to run bare-metal under TamaGo as well as
on a host.

## Import

```sh
go get src.kyanite.computer/core
```

## Packages

| Package | Purpose |
| ------- | ------- |
| `supervise` | Actor-model supervision tree (OneForOne/OneForAll/RestForOne, live add/stop) |
| `service` | Supervised-service contract |
| `operator` | Assembles the device microkernel on top of `supervise` |
| `bus` | Auth-aware managed NATS connection, micro-service helpers, JetStream streams |
| `natscore` | Embedded NATS core configuration |
| `auth` | Capability-to-permission policy, per-boot NKey registry, JWT issuance, callout |
| `mgmt` | Management-plane bootstrap |
| `telemetry` | slog console, in-process ring buffer, trace-correlated logging |
| `fsm` | Generic compiled hierarchical state machine with snapshot/restore and DOT/Mermaid export |
| `cfgstore` | Scree-backed typed configuration store |
| `ramnor` | RAM-backed block device for Scree |
| `console` | Shared interactive console |
| `sshd` | Shared SSH management server |
| `webui` | ConnectRPC management UI mux, HTTPS serving, SPA hosting |

## Architecture

See the design notes in this repository:

- `supervise-architecture.md`
- `nats-auth-architecture.md`
- `otel-embedded-go-architecture.md`
- `fsm-embedded-architecture.md`
- `ROADMAP.md`

## Contributing

See the org-wide [CONTRIBUTING guide](https://github.com/kyanitecomputer/.github/blob/main/CONTRIBUTING.md).
Contributions are dual-licensed.

## Security

See the org-wide [SECURITY policy](https://github.com/kyanitecomputer/.github/blob/main/SECURITY.md).

## License

Dual-licensed under either of Apache-2.0 ([LICENSE-APACHE](LICENSE-APACHE)) or
MIT ([LICENSE-MIT](LICENSE-MIT)) at your option.
