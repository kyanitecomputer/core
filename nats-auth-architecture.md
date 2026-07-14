# Kyanite Auth Plane — NATS Auth Callout Architecture

**Status:** Draft
**Working name:** `dike` (placeholder — geological: an intrusive sheet that acts as a barrier;
rename candidates: `gneiss`, `sill`, `bulwark`)
**Context:** Companion to `supervise` (actor-model supervisor), `fsm` (embedded state machine),
and the OTel-embedded architecture. Targets: TamaGo bare-metal (Cairn on AST2600/AST2700-class
BMCs, Vein on Milk-V Vega/FSL91030M), Facet (Svelte 5 WebUI), Scree (flash FS / JetStream).
NATS server embedded in-process per node (bare-metal porting surface previously analyzed).

---

## 1. Problem statement and scope

Every Kyanite node embeds a NATS server. NATS serves three roles simultaneously:

1. **IPC** — in-process connections between local actors (supervise children: sensor daemons,
   Redfish backend, Scree, telemetry BSP, shell).
2. **Node-to-node data plane** — autonomous exchange between devices (BMC↔BMC, BMC↔switch,
   switch↔switch) without a mandatory cloud/central dependency.
3. **Human/API access plane** — Facet over WebSocket, ConnectRPC bridge, CLI tooling.

One enforcement mechanism must govern all three, while authenticating against heterogeneous
identity sources: hardware-rooted device identity (DICE), ephemeral local principals, and
enterprise identity (passkeys/WebAuthn, OIDC, LDAP, WorkOS). The design goal is a **single
audited decision point** (the auth callout service) with **zero persisted shared secrets**,
**hardware-sealed signing keys**, and **fail-closed** semantics everywhere.

Non-goals: NATS operator-mode JWT resolver on-device (§4), account multi-tenancy beyond the
fixed five-account layout, dynamic per-connection permission mutation without reconnect (§12),
MQTT ingress.

---

## 2. Trust boundaries and threat model

On a TamaGo unikernel there is **no process isolation**: any code in the address space owns the
address space. Authenticating in-process connections *against each other* is therefore theater.
The boundaries that actually exist:

| Boundary | Enforced by | Threats considered |
|---|---|---|
| Network interface | TLS + NATS auth (callout, leafnode auth) | Rogue device on mgmt LAN, MITM, credential replay, compromised peer node |
| PMP/TEE boundary (GoTEE) | Hardware | Key extraction from a compromised normal-world image |
| Boot chain (Caliptra/AST1060 RoT → DICE) | Measured boot | Persistent implant, downgrade; stale/unmeasured firmware requesting mesh access |
| Account boundary inside NATS | Server subject isolation + import/export | Subject-space escape between LOCAL / OPER / MESH traffic |
| Browser ↔ Facet | TLS + WebAuthn origin binding | Phished operator, stolen session token |

Explicit assumptions: the local NATS server binary is trusted (it is our firmware); a fully
compromised node is *contained*, not prevented — the mesh design (§9) limits blast radius via
per-device subject grants; physical attackers with debug access are out of scope beyond what
the RoT work already covers.

Consequence: **in-process connections may use a lightweight auth path** (per-boot ephemeral
NKeys, §7) — not because the check protects against an in-process attacker, but because routing
local principals through the same permission compiler catches privilege bugs and produces a
uniform audit stream.

---

## 3. NATS auth callout — protocol facts the design builds on

Mechanics (nats-server ≥ 2.10, ADR-26; Go support in `nats-io/jwt/v2` + `nkeys`):

- On every client connect (and every **reconnect**), the server suspends the connection and
  performs a request-reply on `$SYS.REQ.USER.AUTH` carrying an **authorization request JWT**
  signed by the server. Claims include:
  - `nats.server_id` — server name/id/version/cluster/tags + one-time `xkey`;
  - `nats.user_nkey` — a **server-generated ephemeral user public NKey for this connection**;
    the minted user JWT's `sub` MUST equal it (binds response to connection; prevents replay);
  - `nats.connect_opts` — user/pass/token/jwt/nkey+sig, client name, lang, protocol;
  - `nats.client_info` — remote host, connection kind/type (incl. websocket/leafnode markers);
  - `nats.tls` — TLS version, cipher, **peer cert chain / verified chains** → cert-based
    decisions are possible in the callout.
- The callout service authenticates by whatever means, then returns an **authorization response
  JWT** (signed by the configured issuer NKey) containing either `error` or a **user JWT**:
  `sub` = the request's `user_nkey`, `aud` = target account (server mode), embedded
  `jwt.Permissions` (pub/sub allow/deny, resp limits), `exp` for TTL.
- **XKey encryption**: when configured, the server generates a one-time x25519 keypair per
  connection; request is encrypted to the service's XKey, response encrypted to the server's
  one-time key. Mandatory in this design.
- **Bypass**: `auth_users` (names or nkeys under the callout `account`) skip the callout — the
  mechanism by which the callout service itself connects. Everything else in callout-governed
  accounts goes through the callout, *including mTLS-authenticated clients* (nats-server #8044:
  callout overrides other client auth forms; no mixing of callout and other auth within one
  account).
- **Scope limitation**: callout governs *client* connections (plain, websocket, MQTT). Leafnode
  / cluster / gateway connections authenticate via their own blocks (`leafnodes.authorization`)
  and do **not** traverse the callout. Node-to-node auth is therefore a separate plane (§9) —
  the earlier idea of pushing leaf connections through the callout does not hold; the
  enforcement point moves to certificate issuance + static account binding.
- **Permissions are frozen per connection.** The user JWT is evaluated once; later policy
  changes require a `$SYS` kick → reconnect → fresh callout (§12).
- Known ecosystem rough edges: non-Go callout implementations unstable (#7136) — irrelevant,
  ours is Go in-process; centralized-mode single issuer for all accounts (#4335) — acceptable,
  issuer is TEE-sealed (§11).

---

## 4. Mode decision: centralized (server-config) mode

| | Server mode (chosen) | Operator mode |
|---|---|---|
| On-device state | Config block + issuer pubkey | Operator/account JWTs + resolver dir + nsc lifecycle |
| Account selection | `aud` claim of minted user JWT | Signing key per target account held by service |
| Key material in service | 1 issuer NKey + 1 XKey (both TEE-sealed) | N account signing keys |
| Live permission templates (scoped signing keys) | ✗ | ✓ |
| Fleet credential rotation | Reissue device certs / tokens; kick | nsc push machinery |

Rationale: firmware-class devices have few principals, cheap reconnects, and no room for a JWT
resolver + `nsc` operational model. The single capability operator mode adds — reloadable
scoped-signing-key permission templates without disconnects — is not worth the machinery;
revocation-by-kick (§12) covers the requirement. Revisit only if fleet-wide template updates
without reconnects become a hard requirement (documented escape hatch: the account layout below
maps 1:1 onto operator-mode accounts; migration is additive).

---

## 5. Account topology

```
SYS    — system account: $SYS.> (kick, monitoring). No interactive principals.
AUTH   — callout service (auth_users nkey entry) + nothing else. Deny-all default.
LOCAL  — in-process actors. Never exported off-node. No network listener exposure.
OPER   — humans + API sessions via Facet/CLI (websocket ingress). Callout-governed.
MESH   — node-to-node subjects. Bound to leafnode connections via account mapping;
         local services that produce/consume mesh traffic get scoped imports/exports.
```

Server config sketch (embedded `server.Options`, expressed as conf for clarity):

```conf
accounts {
  SYS:   {}
  AUTH:  { users: [ { nkey: <callout-service-pubkey> } ] }
  LOCAL: { }
  OPER:  { }
  MESH:  {
    exports: [ { stream: "kyanite.<domain>.<self>.>" } ]
    imports: [ /* pinned peer subjects, generated from mesh policy */ ]
  }
}
system_account: SYS
authorization {
  auth_callout {
    issuer:     <issuer-pubkey>          # seed TEE-sealed
    account:    AUTH
    auth_users: [ <callout-service-pubkey> ]
    xkey:       <service-xkey-pubkey>    # seed TEE-sealed
  }
}
websocket { ... }        # Facet ingress, TLS, no separate auth (callout governs)
leafnodes { ... }        # §9 — own auth block, mTLS verify_and_map
```

Enablement ordering (per upstream guidance): deploy and verify the callout service *before*
enabling the `auth_callout` block; enabling first bricks all non-`auth_users` connects.

---

## 6. `dike` — the in-process callout service

### 6.1 Placement and lifecycle

`dike` is a supervise child (`Permanent`, OneForOne within an `auth` subtree). It connects via
the in-process connection (`nats.InProcessServer(ns)`) using its per-boot NKey listed in
`auth_users`. It is the **only** availability-critical auth component and has **no network
dependency** for the local decision path: a node that boots in isolation can authenticate its
own actors and (clock permitting, §13) local operators.

Boot sequence dependency: NATS server start → `dike` up + subscribed on `$SYS.REQ.USER.AUTH` →
`auth_callout` effective (compiled in from the start; the server holds client connects until
the callout answers, so ordering inside one boot is safe — the in-process actors connect after
`dike` reports ready to the supervisor).

### 6.2 Package layout

```
dike/
├── dike.go        // wiring, supervise child adapter, readiness
├── callout.go     // $SYS.REQ.USER.AUTH handler: decode, verify server sig, xkey open
├── decide.go      // the decision function: principal resolution → policy → mint/deny
├── issuer.go      // user-JWT minting; nkeys.KeyPair impl backed by TEE sign oracle
├── xkey.go        // x25519 open/seal (TEE-backed scalar mult or sealed seed)
├── local.go       // per-boot ephemeral NKey registry (LOCAL principals)
├── tokens.go      // session-token validation (Ed25519/PASETO-style, aud, exp, amr)
├── policy.go      // capability model → jwt.Permissions compiler; templates
├── revoke.go      // cid↔principal table, $SYS kick, revocation subject watcher
├── ratelimit.go   // per-remote-host token bucket, failure lockout, constant-time cmp
├── events.go      // decision events → OTel bridge + JetStream audit (Scree)
└── clock.go       // injectable clock; time-sync gating states
```

Single-owner actor: one goroutine owns the principal registry, cid table, rate-limit state —
same zero-mutex discipline as `supervise` and `fsm`. Callout requests arrive on the mailbox;
decisions are pure functions of (request claims, registry snapshot, policy table, clock).
Decision path is allocation-conscious: JWT decode/encode allocates (accepted — auth is not the
hot path), but registry/policy lookups are map-free after build where feasible (policy compiled
into flat tables at config load, `fsm`-style Builder→Build).

### 6.3 The decision function

```
resolve(request) → Principal:
  1. connect_opts.nkey present ∧ pubkey ∈ per-boot LOCAL registry     → LocalActor
  2. connect_opts.token parses as session token ∧ sig/aud/exp valid    → OperatorSession
  3. connect_opts has user/pass matching recovery credential (§8.4)    → RecoveryOperator
  4. otherwise                                                         → Deny

mint(principal) → user JWT:
  sub  = request.nats.user_nkey            (mandatory binding)
  aud  = principal.account                 (LOCAL | OPER)
  exp  = now + ttl(principal)              (LocalActor: node epoch; Operator: ≤ session exp,
                                            cap 15 min — cheap silent reconnect re-mints)
  nats.permissions = compile(policy, principal)   // §10
  sign with issuer KeyPair (TEE oracle)
```

Deny is the default at every branch; unknown connection kinds, malformed tokens, clock-gated
states (§13) all fall through to a signed error response (rate-limited, audited).

---

## 7. LOCAL plane — per-boot ephemeral NKeys

At boot, the supervisor generates one Ed25519 NKey pair **in RAM** per declared child that
needs a NATS connection, registers the public key + capability set with `dike` (direct call —
same address space — before the child starts), and hands the seed to the child via its spec.
Properties:

- **Zero persisted local secrets.** Power cycle invalidates everything.
- **Restart hygiene:** a supervise restart mints a fresh key and re-registers; the old pubkey is
  tombstoned and its cid kicked — a wedged-then-restarted child cannot leave a zombie
  authenticated connection with stale permissions.
- **Authorization source of truth = the supervision tree spec.** Each child spec declares
  capabilities (`telemetry.publish`, `scree.rw`, `redfish.backend`, …); `dike` compiles these to
  subject permissions. Privilege bugs surface as auth denials in the audit stream, not as
  silent over-grants.
- No clock dependency: LOCAL minting uses relative TTL against monotonic time; wall-clock
  gating does not apply (§13).

`auth_users` bypass is used **only** by `dike` itself. Nothing else skips the callout.

---

## 8. OPER plane — humans and API sessions

### 8.1 Two-phase authn: ceremony, then token exchange

Passkeys/WebAuthn, OIDC, and LDAP are ceremony-based (challenge/response, redirects, binds) and
cannot complete inside a single NATS `CONNECT`. Split:

```
Phase 1 (ceremony)   Browser/CLI ⇄ identity frontend (Facet backend, ConnectRPC over TLS)
                     WebAuthn assertion | OIDC code+PKCE | LDAP bind | WorkOS
                     → short-lived session token (Ed25519-signed, aud="kyanite-dike",
                       sub, roles, amr, exp ≤ 8h, jti)
Phase 2 (connect)    Client connects to NATS websocket with token in connect_opts.token
                     → callout validates token → mints OPER user JWT (exp ≤ 15 min)
                     → client library silently reconnects/re-mints until session exp
```

This is the established broker pattern (cf. `jr200-labs/nats-iam-broker`: exchange IdP JWT for
an RBAC'd NATS user JWT) collapsed into the on-device callout. The identity frontend and `dike`
share a trust anchor: the frontend's token-signing key is in the same TEE-sealed key family
(§11), so token validation is one Ed25519 verify against a pinned pubkey — no JWKS fetching on
the decision path.

### 8.2 Passkeys — RP ID strategy (fleet decision required)

WebAuthn credentials bind to an RP ID. Options:

- **A. Per-device RP ID** (device FQDN): fully autonomous, works air-gapped; operator enrolls a
  passkey *per device* — untenable beyond a handful of nodes.
- **B. Central auth domain** (`auth.<org>` fronting WorkOS/OIDC): one passkey for the fleet;
  Phase 1 happens against the central IdP, device validates the resulting token/assertion; but
  reintroduces a network dependency for interactive login.
- **C. Hybrid (recommended):** B as primary; A as the break-glass path on a designated
  `recovery` RP per device, enrolled at provisioning alongside §8.4.

### 8.3 OIDC/LDAP specifics

OIDC: code flow + PKCE at the frontend; validate `iss/aud/exp/nonce`; map claims→roles via a
static, reviewed table shipped in config (no Go-template RBAC engines on-device). LDAP: bind +
group query from the frontend; the *frontend* is the only component holding LDAP connectivity —
`dike` never talks to external identity systems, it only verifies frontend-signed tokens. This
keeps the decision path offline-capable and the external-protocol attack surface out of the
auth kernel.

### 8.4 Recovery credential

One provisioning-time recovery principal (Argon2id-hashed secret in sealed storage, physical-
presence-gated if the platform allows), resolving to a minimal `recovery` capability set.
Constant-time verify, aggressive lockout, loud audit event.

---

## 9. MESH plane — node-to-node

### 9.1 Auth mechanism: DICE-rooted mTLS on leafnode connections

Auth callout does not govern leafnode connections; the enforcement stack is:

1. **Identity issuance** (the real gate): each device's DICE chain (Caliptra / AST1060 RoT /
   GoTEE root) terminates in a TLS client certificate whose issuance is **attestation-bound** —
   a device whose measured boot state is stale or unknown does not get (or renew) a mesh cert.
   Short-lived certs (e.g., 7 days) + renewal-on-attestation replaces CRL/OCSP machinery.
2. **Connection auth**: `leafnodes { tls { verify: true, ca_file: <kyanite-mesh-CA> ,
   handshake_first: true } }` — TLS-first so no plaintext `INFO` ever leaves the port; peer
   cert SAN (device ID URI) maps to the MESH account (`verify_and_map`).
3. **Subject containment**: traffic entering via a leaf connection is capped by that
   connection's permissions and the MESH account's import/export lists regardless of what the
   remote claims — the containment mechanism against a compromised peer. Exports/imports are
   generated from mesh policy, pinned per peer device ID, deny-by-default.

### 9.2 Topology

Leafnode graphs must be acyclic; "fully autonomous mesh" needs discipline:

- **Within a management domain (rack):** the Vein switch is the hub — it is already the
  aggregation point and the highest-resource node. Cairn BMCs are leaves (outbound-only,
  NAT/firewall-friendly, buffer during hub loss, local IPC unaffected).
- **Cross-domain:** hub↔hub leaf connections in a configured tree (site → row → rack). No
  dynamic peering in v1; topology is configuration, changes are provisioning events.
- **Hub loss:** leaves keep operating locally (LOCAL + OPER planes fully functional); JetStream
  sources/mirrors resync MESH streams on reconnect (Scree-backed).
- Role-reversal (hub-initiated leaf connections) is available where the BMC network is only
  reachable inbound; note as deployment option, not default.

### 9.3 What crosses the mesh

Only explicitly exported subjects: telemetry aggregates, inventory/health, attestation
evidence, coordinated actions (power sequencing, firmware rollout) — each a separate export
with its own audience. `$JS.API.>`, `$SYS.>`, LOCAL, OPER never cross.

---

## 10. Authorization model — subjects as the policy language

All authz compiles to pub/sub permission maps; design the namespace so least privilege is
*expressible*:

```
kyanite.<domain>.<device>.<component>.<facility>...   # data plane
  kyanite.r1.bmc07.thermal.telemetry.>                (export candidate)
  kyanite.r1.bmc07.power.cmd.<verb>                   (per-verb grant)
svc.<name>.>                                          # local service RPC (LOCAL only)
_INBOX_<principal-hash>.>                             # per-principal inbox prefix
```

Rules:

- **Never grant shared `_INBOX.>`** — any subscriber can read others' replies. Each minted JWT
  gets a unique inbox prefix in its allow list; clients set `nats.CustomInboxPrefix`.
- Capability→permission compilation is a flat table built at config load (Builder→Build,
  `fsm` discipline): capability id → precompiled allow/deny subject lists; minting is table
  lookup + concatenation, no string formatting beyond principal substitution.
- JetStream: grant `$JS.API.*` narrowly (consumer create/ack on named streams only); KV/Scree
  access is expressed as `$KV.<bucket>.>` grants through the same table.
- Deny lists are used sparingly (deny-by-default via allow lists); the one systematic deny:
  `$SYS.>` on everything except SYS principals.

---

## 11. Key hierarchy and TEE sealing

```
DICE CDI (RoT-measured)
 ├─ Device identity key  → mesh TLS client cert (attestation-gated issuance, §9.1)
 ├─ dike issuer NKey     (Ed25519 seed sealed; sign via GoTEE oracle)
 ├─ dike XKey            (x25519 seed sealed; open/seal via oracle)
 └─ frontend token key   (Ed25519; same oracle)
```

- `nkeys.KeyPair` is an interface — implement it backed by the GoTEE sign oracle so the issuer
  **seed never enters the normal world**; `jwt.Claims.Encode` takes the interface directly.
  This is precisely the sign-oracle TEE use case from the GoTEE/PMP work; budget one world
  switch per mint against the crossing-frequency cost model (auth-rate ≪ crossing budget).
- Platforms without GoTEE (pure AST2600 path): seeds sealed to the RoT-held key, unsealed into
  locked RAM at boot; degradation is documented per-platform, not silent.
- Keys derived from DICE ⇒ firmware measurement change ⇒ new keys ⇒ mesh cert re-issuance is
  the *desired* behavior (implants don't inherit identity). Local plane unaffected (per-boot
  anyway); operator tokens survive via the central IdP path.

---

## 12. Revocation and dynamic permissions

Permissions are frozen per connection; the loop is:

```
policy/registry change → dike updates tables → lookup cid(s) for affected principal(s)
→ $SYS kick (system-account server request) → client reconnects → callout re-runs
→ fresh mint under new policy (or deny)
```

- `dike` maintains the cid↔principal table from its own decisions (RAM, rebuilt per boot;
  reconcile against `$SYS.REQ.SERVER.PING.CONNZ` after `dike` restart so a restarted `dike`
  can still revoke pre-restart connections).
- Session revocation (operator logout / IdP disable): frontend publishes to an internal
  revocation subject; `dike` tombstones the `jti` and kicks. Tombstone set is bounded (ring,
  horizon = max session TTL).
- Short OPER JWT TTL (≤ 15 min) bounds the window even if a kick is missed.
- Scoped-signing-key live templates (operator mode) deliberately rejected — reconnects are
  cheap at this scale; see §4.

---

## 13. Time

JWT validation (`exp`/`iat`/`nbf`) requires trustworthy wall-clock; embedded devices boot
without one. Clock-state gating in `dike` (an `fsm`, naturally):

```
BOOT ──roughtime sync──▶ SYNCED ──drift/holdover──▶ HOLDOVER ──▶ DEGRADED
```

- **BOOT (no trusted time):** LOCAL principals only (monotonic TTLs, no wall clock needed).
  External tokens and cert validation rejected → a node without time sync is IPC-functional
  but not remotely accessible. Fail-closed, by construction.
- **SYNCED:** full validation; skew tolerance ±30 s on `iat`/`nbf`, none on `exp`.
- **HOLDOVER/DEGRADED:** widen tolerance against modeled drift, emit security events, degrade
  to BOOT rules past a bound.
- Roughtime (per the roughtime+lneto integration analysis) is the time root; mesh hub may act
  as a roughtime relay so leaves sync inside the domain.

---

## 14. Observability and audit

Per the OTel-embedded architecture (span granularity ladder, non-blocking BSP):

- **One span per auth decision** — `dike.decide` with attributes: outcome, principal kind,
  account, amr, connection kind, remote host, latency, clock state. Auth is exactly the event
  class that earns a span at every granularity tier.
- Plain atomic counters in core: decisions{outcome}, kicks, rate-limit hits, token-tombstone
  size (`fsm`-style: counters in core, spans via optional bridge).
- **Audit stream**: every decision + revocation + clock-state transition published to an
  internal subject, JetStream-persisted via Scree (HMAC-Merkle authenticated storage gives
  tamper-evident audit for free), exported (read-only) over MESH for fleet SIEM aggregation.
- Rate-limited security alerts on: repeated denials per host, recovery-credential use,
  attestation-stale peer attempts, clock regression.

---

## 15. Failure modes

| Failure | Behavior | Notes |
|---|---|---|
| `dike` crash | supervise restarts (Permanent); connects held by server meanwhile time out and retry | cid table rebuilt via CONNZ reconcile |
| `dike` down hard | **No new connections** in callout accounts; existing connections unaffected | Fail-closed; LOCAL actors already connected keep running |
| Reconnect storm (network flap) | Callout runs per reconnect | Decision path is table-lookup cheap; rate limiter exempts LOCAL nkeys |
| Hub loss | Leaves autonomous (LOCAL/OPER live); MESH buffers via JetStream | §9.2 |
| Time sync loss | Degrades toward BOOT rules | §13; loud events |
| Identity frontend down | No *new* operator sessions; existing tokens valid until exp | Recovery credential (§8.4) independent |
| TEE oracle fault | No minting → no new connections | Panic-contained via supervise; treat as security event |
| Compromised peer node | Contained to its pinned MESH imports/exports; cert non-renewal on next attestation | §9.1/§9.3 |

---

## 16. TamaGo portability notes

- Callout service is pure Go: `nats-io/jwt/v2`, `nkeys`, `crypto/ed25519`,
  `golang.org/x/crypto/curve25519` (x25519) — all TamaGo-clean; no cgo, no syscalls beyond what
  the embedded nats-server port already required.
- Ed25519/x25519 on Cortex-A7/A35-class cores: sub-ms per operation; auth rate is negligible.
  RISC-V (Vega) unaccelerated: still fine at auth-plane rates.
- Entropy: NKey generation at boot requires seeded CSPRNG before supervisor spawn — order TRNG
  init ahead of the auth subtree; block, don't degrade.
- RAM budget: `dike` steady state is small (registry ≈ #children + #sessions, tombstone ring,
  policy tables); JWT encode/decode transiently allocates ~KBs — no dedicated arena needed.
- WebSocket listener + leafnode listener must run while the plain client port can stay closed
  on pure-IPC nodes; **verify** the embedded-server option interaction (`DontListen` vs
  per-listener enablement) during the porting work — flagged, not assumed.

---

## 17. Bring-up plan

1. `dike` core: callout handler + LOCAL plane + policy compiler; enable `auth_callout` in the
   embedded server; migrate in-process actors to per-boot NKeys. (No external identity yet.)
2. Audit/OTel wiring; kick/revocation loop; CONNZ reconcile.
3. OPER plane: frontend token format + validation; Facet websocket connect path; recovery
   credential.
4. Passkey/OIDC/LDAP frontends (Facet backend), RP-ID decision (§8.2).
5. MESH: mesh CA + attestation-gated issuance; leafnode config gen (exports/imports from
   policy); hub topology per domain.
6. TEE sealing (sign oracle KeyPair); clock-state gating with roughtime.

Each step is independently shippable; step 1 alone already replaces "no auth" with a complete
local enforcement regime.

---

## 18. Open questions

1. **Session token format**: minimal hand-rolled Ed25519-signed structure vs PASETO v4.public.
   Leaning hand-rolled (one fixed schema, one verify path, no library surface) — mirrors the
   stdlib-only discipline of `supervise`; counterargument is external auditability of a named
   format.
2. **Mesh CA**: per-fleet offline CA vs per-domain CA chained to fleet root. Affects blast
   radius of hub compromise and air-gapped provisioning flow.
3. **`DontListen`/listener granularity** in the embedded server for IPC-only nodes (§16) —
   verify against the bare-metal port.
4. **Attestation freshness policy** for cert renewal (§9.1): renewal-time-only vs periodic
   re-attestation over MESH (PLDM Type 5 / SPDM alignment with the AST1060 RoT work).
5. **Facet ConnectRPC bridge**: does the dual-adapter transport authenticate as one OPER
   principal per session (token pass-through) or as a LOCAL bridge service performing its own
   authz? Leaning token pass-through — keeps end-to-end principal identity in the audit stream.
6. Whether `dike`'s clock-gating FSM shares the injectable clock interface with `supervise`
   or duplicates it — same question as `fsm`, presumably same answer (duplicate, avoid module
   coupling).
