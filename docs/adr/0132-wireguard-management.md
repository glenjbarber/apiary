# ADR-0132: WireGuard management across the Colony

## Status

Proposed

## Context

ADR-0127 section 5 ("WireGuard VPN", High Priority) places WireGuard in
Phase 3 alongside Certificate Management, and its own closing open
question 3 asks: *"WireGuard vs. Tailscale/other: Sylve uses
WireGuard; should Apiary standardize on WireGuard or support multiple
VPN backends?"* This ADR answers that question and designs the
management surface.

ADR-0127's sketch is:

- WireGuard interface CRD: `private_key`, `listen_port`, `peers[]`
  (public_key, allowed_ips, endpoint, keepalive)
- Server mode — peer provisioning with QR/export; managed firewall
  rules for handshake
- Client mode — persistent outbound with FIB/route controls, MTU,
  reconnection
- **Integration:** WireGuard mesh as Cell interconnect; auto-provision
  on Colony join (derive from peer TLS hostname map)
- **Courier sync:** Air-gapped operations courier uses WireGuard
  endpoint for rendezvous

Three of those four bullets do not survive contact with the code, and
the reasons are recorded in "Findings that contradict ADR-0127" below.
The fourth (server/client modes, peers, allowed IPs, MTU, keepalive)
is real work and is what this ADR designs.

ADR-0127's problem statement is: *"No secure overlay for cross-site
Colony, management network redundancy, courier sync."* The first two
are genuine gaps. The third is not a gap a VPN can fill, for the
reason given below.

### Vocabulary

- **Comb** — a node running `managerd`. **Colony** — the
  raft-replicated control plane. **Cell** — a workload (a VM or a
  jail). **Combs' management path** — the set of paths a Comb needs
  to stay reachable and to reach its peers: managerd's gRPC
  listener, the raft transport listener, and the frontend
  listener.

## What already exists

Verified against the primary checkout at commit `5dad79f`.

### There is no WireGuard in this codebase

`grep -rli "wireguard\|tailscale\|wg-quick"` over `*.go`, `*.proto`
and `*.html` returns nothing. There is no `internal/wireguard`, no
`wg` proto message, no `AssumptionKind` for tunnels, no frontend
page. This ADR is greenfield. Nothing below is a refactor of
existing code; it is a new package plus a new slice of the existing
raft/managerd/reconcile/frontend machinery.

### The Colony's own control transport, which WireGuard must not touch

- `internal/tlsdial/tlsdial.go` (54 lines) builds the
  `grpc.DialOption` that `cmd/frontend` and `cmd/restshimd` use to
  reach `managerd`'s external API. It is a shared helper precisely
  because two `main` packages need identical TLS-or-plaintext logic.
  Its `ManagerDialOption(useTLS bool, caFile, serverName string)`
  supports plaintext, a CA-signed certificate verified against the
  host system pool, a self-signed certificate via `caFile`, and a
  `serverName` override independent of the dialed address.
- `managerd`'s external API is **loopback-only**. Its `-rpc-addr`
  default is `127.0.0.1:17700` (`internal/cluster/peer.go`'s
  `defaultPeerManagerdPort`). Cross-node reach goes through
  `internal/manager`'s `PeerReporter` (`internal/manager/peer.go`),
  which dials the leader and forwards a leader-gated write, with
  `PeerReporter.dial`/`dialOpts`/`dialRestartGuardrail` consulting
  `peer_tls_hostname_map` to override `tls.Config.ServerName` when
  dialing an address by IP (ADR-0115).
- The join flow is explicitly mutually authorized: ADR-0083, with
  ADR-0113 adding an operator trust prompt against a
  `tls_cert_fingerprint` carried in both `rpcpb.PendingJoinRequest`
  and `internalpb.PendingJoinRequest`
  (`api/internalpb/state.proto`, `PendingJoinRequest` field 8).
- The **raft transport already has its own TLS**, separate from
  managerd's: `-raft-tls-cert`/`-raft-tls-key`/`-raft-tls-ca`
  (ADR-0078), plumbed in `cmd/raftd/main.go` from
  `internal/raftdconfig`. `RaftTLSKey` is documented in
  `internal/raftdconfig/manager.go` as a real credential, and the
  file carrying it "must be root-owned, mode 0600".

So: the Colony already has authenticated, encrypted, operator-
approved control-plane transport, on two independent paths, and it
works. WireGuard is not needed for either and must not be
substituted for either.

### Key custody precedent — the most important thing in this ADR

Three existing packages already establish the rule this design
depends on, and the rule is that **private key material never goes
through raft**:

1. `internal/origincert/store.go` — the package doc, verbatim: *"keeps
   Cloudflare Origin CA private keys and certificate files local to one
   Hive. It deliberately has no raft dependency."* Its
   `InventoryEntry` doc repeats it from the other side: it contains
   "only the non-secret facts [...] It is local to one Hive and
   deliberately excludes token values, private keys, CSRs, and
   certificate PEM." `store.go` writes the key via `writeTemp(...,
   0o600)` after `os.MkdirAll(directory, 0o700)`.
2. `internal/raftdconfig/manager.go` — `InternalToken` ("A real
   credential - this file must be root-owned, mode 0600") and
   `RaftTLSKey` sit in one node-local JSON file that its own doc
   requires be root-owned and mode 0600, written atomically
   (temp file + explicit `chmod` + rename, `manager.go` around line
   184), and never RPC-editable. `internal/nodeconfig/manager.go`'s
   package doc says the same of its own contents: "it is never
   replicated through" raft.
3. `internal/cloudflare/sidecar.go` — `ExposureRecord`, the local
   "last successfully applied exposure" record, doc: *"persisted
   locally (never raft) so a removal can be detected across a managerd
   restart"*. The Cloudflare Tunnel **credentials JSON** is likewise a
   startup-flag path (`Reconciler.CloudflareTunnelCredentialsFile`),
   never a raft field.

The rule is already the house rule. This ADR applies it to
`private_key` — the one field ADR-0127's CRD sketch puts *in* the
replicated object.

### The evidence machinery, and the honest-unknown rule

- `internal/assumptions/manager.go` provides `Status`, `Key`,
  `Result`, `HistoryEntry`, `ClampDetail` (500-char cap, credential-
  shaped substrings redacted) and `MaxDetailLen`.
- `api/rpc/manager.proto` `enum AssumptionKind` (line 2515) has six
  values, `ASSUMPTION_KIND_UNSPECIFIED = 0` through
  `ASSUMPTION_KIND_REPLICA_NETWORK_BRIDGE_UP = 5`. **Next free: 6,
  7, 8.**
- `internal/assumecheck/checker.go`'s `checkNATUplink` is the
  precedent for a node-scoped check that reports `not_applicable`
  rather than a false verdict.
- ADR-0056 (Evidence-Aware Health v1) and ADR-0118 (Why Not
  quorum-blocker voter detail) are the sources of the rule this ADR
  leans on hardest: **absence of an observation is `unknown`, which is
  neither healthy nor failed.** A quiet peer has not been shown to be
  broken. ADR-0122 owns the cluster-wide evidence-aware health API
  and is where any future folding of tunnel state into a node health
  verdict must happen — not here.

### The existing tunnel implementation to learn from

`internal/cloudflare` is the closest thing Apiary has to a managed
tunnel, and its shape is worth copying deliberately:

- `tunnel.go`'s `Manager.EnsureRunning` converges one Hive-wide
  singleton process launched via `daemon(8)`, restarting on
  **config-content change or failed liveness, checked
  independently** — a liveness probe is a real `signal(0)`, and a
  missing or unparseable pidfile is "not alive", not an error. Its
  doc names the lesson explicitly: a config-change-only check misses a
  process that died on its own (ADR-0063 finding 6, mirroring
  ADR-0043/ADR-0027).
- `tunnel.go`'s `StopIfRunning` makes an empty desired set and a
  disabled feature the *same* code path, and the fixed `RunDir`
  (`/var/db/apiary/cloudflared`, `0700`) means it works with no
  feature flags configured at all.
- `RenderConfig` validates its own inputs rather than trusting the
  caller's validation, because the rendered file is what the daemon
  loads — and it sorts ingresses so the same desired set always
  renders byte-identically, which is what makes the content diff
  meaningful. Config is written `0600`.
- `sidecar.go` is the "local, never raft" record that makes removal
  and diffing work across a managerd restart.
- Ownership split: `Reconciler.Cloudflare` is nil-able; a node with it
  unset simply never reconciles a tunnel, and
  `reconcileCloudflareTunnel` still calls `StopIfRunning` so
  disabling the feature cleans up.

### The reconcile loop and the raft FSM it would plug into

- `internal/cluster/reconciler.go` (1717 lines) already has
  `reconcileCloudflareTunnel(ctx, planned []VMPlacement)` and a
  `pfManager` equivalent, both nil-able feature interfaces, plus the
  `peerReporter` interface in `internal/cluster/peer.go` for
  leader-forwarded writes.
- `internal/raft/fsm.go`'s `FSM` holds
  `vms map[string]*internalpb.VMDefinition`, `networks`, `apiKeys`,
  `jails`, `pendingJoinRequests`, `restartLeases`, `restartRecords`
  and dispatches on `internalpb.Command_*` oneof cases.
- `api/internalpb/state.proto`'s own header draws the governing line:
  "ephemeral state is small, JSON-shaped facts [...] as opposed to
  physical state (ZFS datasets, HAST-replicated bytes), which never
  goes through raft." Tunnel *intent* is a fact every Comb must agree
  on. Tunnel *effect* — an interface's handshake timestamps, transfer
  counters, the kernel's own state — is a fact about one kernel, and
  is not replicated.
- The leader-gated / ungated read split (`internal/raft/fsm.go`,
  `internal/raft/node.go`) is what makes "decide needs quorum,
  execute does not" possible, and `ErrNotLeader` + `LeaderHint` in the
  RPC response is the existing shape of that error, not something
  this ADR invents (`internal/manager/server.go`, e.g. line 1241).
- `internal/netif/interfaces.go` is the read-only interface inventory:
  `Name`, `Up`, `Addresses`, sorted, and deliberately tolerant of an
  interface disappearing mid-listing. It is the natural evidence
  source for "the `wg` interface is or is not present".

## Decision

### 1. One backend: WireGuard, with a deliberate seam and no
abstraction for its own sake

**Apiary standardizes on WireGuard and does not build a multi-backend
VPN abstraction in v1.**

Concretely:

- `internal/wireguard` is a package with one implementation. It does
  not define a `Backend` interface, does not register backends, and
  does not probe for `wg`/`wg-quick`/`ifconfig`-vs-`netlink`
  alternatives. The `Reconciler` gains a `WireGuard wireGuardManager`
  field of the same nil-able concrete-struct shape
  `Reconciler.Cloudflare` already has — a feature toggle, not a
  polymorphism seam.
- No Tailscale, no ZeroTier, no OpenVPN backend, no
  "bring-your-own-VPN" plugin point. If an operator wants a second
  overlay, it runs it themselves; Apiary will not manage it and will
  not try to detect it.
- The one concession to future proofing is that *evidence* about
  tunnels is expressed through `internal/assumptions`' existing
  `Kind`/`Key`/`Result` model rather than a WireGuard-shaped struct,
  so a future backend can publish its own kinds without changing
  `/assumptions`. That is reuse of an existing abstraction, not a new
  one.

**What would change the answer.** Three things, named in advance so
the reversal is a decision rather than an accident:

1. **The kernel module proves unobtainable or unmaintained on the
   target FreeBSD version.** `wg(4)` is a first-class FreeBSD
   interface, so this is unlikely — but if a future
   FreeBSD release deprecates it, or if the Colony must run on a
   platform where only a userspace `wireguard-go` implementation is
   available with materially different performance, the
   "no-backend-abstraction" decision is the one that has to be
   revisited first, because it is the only thing in this ADR that
   forecloses options.
2. **A real compliance requirement for a managed, authenticated
   coordinator** (a control plane that can revoke, audit and
   centrally re-key a mesh) appears in a deployment. Tailscale's
   coordination server is a different product with different trust
   properties; an operator who needs it needs it, and a
   plugin interface becomes the least-bad answer.
3. **Multi-tenancy arrives (ADR-0127's own open question 5).** A
   WireGuard mesh does not naturally express per-tenant key
   distribution or revocation. If tenants need isolated overlays,
   the "one tunnel, operator-provisioned peers" model here is
   insufficient and a different design is warranted.

Note what is *not* on that list: a second WireGuard use case. More
tunnels is a scale question, answered by more rows in
`TunnelDefinition`, not by a second backend.

### 2. The boundary: WireGuard is for the edge, never for the spine

This is the load-bearing section.

**What WireGuard is for**, in this design:

- **Operator access.** An administrator on a laptop or a phone
  reaching a Comb's management surface over a public network,
  without exposing managerd's gRPC listener, the frontend, or the
  raft port to that network. The `AllowedIPs` of a mobile peer is
  narrow — the site subnets, or nothing at all (a peer that only
  dials out, for a bastion).
- **Site-to-site.** Two physical sites, each a Comb or a small
  set, joined into one routable private space so an operator at site
  A can reach a Cell at site B. This is the "cross-site Colony"
  half of ADR-0127's problem statement.
- **Management network redundancy.** A second path to a Comb when
  the primary LAN is unavailable or has been re-addressed. This is
  the second half of the problem statement, and it is the *only*
  one of the three that touches the management path at all — and
  even here, WireGuard is an alternative *route* to the surface, not
  the transport the surface speaks.

**What WireGuard is explicitly not for**, and what already exists
instead:

| Control-plane concern | Existing mechanism | Why WireGuard is wrong for it |
| --- | --- | --- |
| Client → managerd | `internal/tlsdial` + `managerd`'s gRPC on `127.0.0.1:17700`, forwarded peer-to-peer by `internal/manager`'s `PeerReporter` | gRPC-over-mTLS with per-request identity, an API-key gate (ADR-0023), and a leader hint. A WireGuard interface has no per-peer identity at the application layer. |
| Join approval | ADR-0083's mutually authorized join + ADR-0113's operator trust prompt against a `tls_cert_fingerprint` | A tunnel that exists is not a node that was approved. Trust must be a human decision against a fingerprint, not a side effect of an encrypted packet arriving. |
| Raft replication | hashicorp/raft over the `-raft-bind` transport, with ADR-0078's own `-raft-tls-*` certificates | **This is the prohibition.** See below. |
| Server identity verification | ADR-0115's `peer_tls_hostname_map` giving `PeerReporter` a correct `ServerName` when dialing by IP | A WireGuard tunnel removes the *need* for an SNI override, which is precisely the problem: an encrypted overlay makes a misaddressed peer look like a working one. |

**The prohibition, stated plainly: WireGuard must never become a
dependency of the raft transport.** If raft's `Address` were a
WireGuard endpoint, or if the reconciler's own correctness were
gated on a `wg` interface being up, then a tunnel failure — a bad
`AllowedIPs`, a rotated key, a NAT rebind, a revoked peer — would
become a quorum failure. The Colony would lose its leader, and the
mechanism used to recover the tunnel would be the one thing the
broken tunnel took away. That is a single point of failure with no
recovery path, and it is the specific outcome this design is built
to make impossible.

The same reasoning extends, with less drama, to `managerd`'s own
gRPC listener. A Comb's `managerd` must be reachable over WireGuard
if an operator configures a route for it, and that is a *route*,
not a *dependency*: `internal/manager` must keep working identically
with the interface present, absent, or wedged. The rule for the
whole design is:

> **Nothing in the Colony's control path may consult WireGuard state.**
> Not the FSM, not the reconciler's quorum logic, not
> `PeerReporter`'s dial path, not health computation, not the join
> flow. A tunnel is an *edge* the Colony draws on its own diagram.

This is also why the design keeps tunnels **out of the default
path**: `Reconciler.WireGuard == nil` (the default) means no
`wg` interface is ever created, no `AllowedIPs` is ever installed,
and no route is ever changed. The feature is opt-in per Comb exactly
the way `Reconciler.Cloudflare` and `Reconciler.PF` are.

### 3. Key custody — the most safety-critical part

**Private keys are never raft-replicated. There is no encrypted-at-rest
raft, and this ADR does not build one.**

ADR-0127's CRD sketch lists `private_key` as a field of the
replicated interface object. That field does not exist in this
design, and the omission is the point. The chain of reasoning:

- Every voter of the raft group holds the full replicated log. A
  plaintext private key in a replicated message is a private key in
  the memory of every voter, in every snapshot, in every log
  segment, and in every raft log shipped off-host for debugging —
  which this project does, and which ADR-0103/ADR-0119's recovery
  surfaces make easy.
- The obvious mitigation, "replicate it encrypted with a
  cluster-wide key", moves the problem rather than solving it: the
  encryption key must be readable by every Comb that applies the
  entry, so the compromise surface is unchanged, and the
  construction is unaudited. A `grep` for a KMS, an `age` seal, a
  Sealed-Secrets scheme, or any envelope encryption in this tree
  returns nothing — this project has no such machinery, and building
  one is a project, not an ADR.
- The existing precedent is unambiguous and is quoted in "What
  already exists" above: `internal/origincert` "deliberately has no
  raft dependency"; `internal/raftdconfig`'s `internal_token` and
  `raft_tls_key` are node-local `0600` files; `internal/nodeconfig`
  is "never replicated through" raft. The raft transport's own TLS
  private key is already handled this way, which means the pattern is
  already load-bearing on the most security-critical path in the
  product.

**Where keys live.** On the Comb that owns the tunnel, at
`/var/db/apiary/wireguard/<tunnel-id>.key`, holding a base64
Curve25519 private key in the same shape `wg`'s own tooling writes:

- directory created `0700`, file written `0600`, root-owned;
- written **atomically**: temp file in the same directory, explicit
  `os.Chmod(tmpPath, 0o600)`, `fsync`, `rename` — the exact
  sequence `internal/frontendconfig/manager.go` (lines ~151-184) and
  `internal/manager/restartconfirm.go` (lines 38-65) already
  establish, and the reason both carry a long comment saying so;
- a permission wider than `0600` is **refused at load**, not warned
  about. `internal/assumptions/manager.go` already takes this
  stricter posture for its own file (line ~216: it *renames aside*
  a file whose permissions are wider than the expected `0600`),
  while `internal/raftdconfig` and `internal/frontendconfig` only
  `warnIfWorldReadable`. For a private key, refuse-to-start is the
  right answer: a warning an operator scrolls past is not a control.

**What raft holds.** Per tunnel, replicated and safe to replicate:

- `id`, `name` (operator-chosen, unique),
- `node_id` — the owning Comb,
- `listen_port`,
- `address_cidr` (the interface's own address/prefix),
- `mtu`,
- `public_key` — a Curve25519 **public** key. For this primitive a
  public key is derived from the private key and reveals nothing that
  a peer config does not already require every peer to hold, so
  replicating it is not a secret leak. It is the one asymmetry in
  this design and it is deliberate.
- `key_fingerprint` — a short hash of the public key, for display
  and for the operator to compare out of band,
- `key_path` — *where* the private key is expected to be found on
  that Comb, not its contents,
- `peers[]` (see below),
- `generation` (see lifecycle), and the lifecycle `state`.

**Who can read them.** Root on the owning Comb, and `managerd` as
that Comb's own process. Nobody else. Concretely, that means:

- the key file is never served by `ManagerService`, never returned
  by `ListTunnels`, never rendered into the frontend, never written
  to the audit log, and never included in an `assumptions` `Detail`
  (where `ClampDetail`'s credential redaction is the last line of
  defence, not the first);
- the **private key is write-once per generation** and is never
  accepted over RPC. There is no `SetTunnelPrivateKey` RPC and there
  will not be one. The key is *generated on the Comb*, or
  *imported by an operator with console access to the Comb*
  (ADR-0127 asks for QR/export of a peer's *config*; a peer's config
  contains that peer's private key and is therefore produced on the
  peer's own Comb, never on the server);
- `wg show` output is never logged wholesale — it prints
  `private key: (hidden)` by default, but the audit path should not
  depend on a tool's redaction behaviour.

**How keys are rotated.** Rotation is a generation bump on the
tunnel, and it is the one genuinely dangerous operation in this
design, because a WireGuard key is a shared secret between two ends
and there is no negotiation. Rotating means every peer's
`public_key` entry for this tunnel changes simultaneously, and any
peer missed is a peer that can no longer connect. Therefore rotation
is **staged and peer-by-peer, never a single action**:

1. `RotateTunnelKey` on a committed `TunnelDefinition` bumps
   `generation` and sets a new `pending_public_key`. The *current*
   key stays in service. Nothing changes on the interface yet.
2. Each peer is updated individually via `SetTunnelPeer`, with the
   new public key placed **in addition to** the old one where the
   implementation allows it, or in a strict order the operator
   confirms. `wg` itself supports multiple peers sharing a public
   key only as distinct entries, so the practical sequence is:
   distribute, then cut over.
3. The owning Comb applies the new key only on explicit
   confirmation, on the same staged-with-a-local-deadline mechanism
   as a route change (below), with the *old* key as the
   `known-good` value.
4. The old key material is deleted only after the operator confirms
   cutover, and its deletion is a distinct, separately-confirmed
   action, because a too-early delete is unrecoverable.

Rotation is deliberately **not** automatic. A background key
rotation that silently invalidates every peer is strictly worse than
an operator who has to think about it once a year. ADR-0133's
certificate auto-renewal is a different mechanism on a different
credential and does not touch a tunnel key; see "Boundary with
ADR-0133".

**How a lost key is recovered.** It is not. That is the answer, and
it is the correct one:

- A lost private key is **rekeyed**, not restored. `RecoverTunnelKey`
  generates a fresh Curve25519 keypair on the Comb, bumps the
  generation, and requires every peer to be re-provisioned from the
  new public key. Any peer not re-provisioned is `unknown`, not
  `false`, and will show as such.
- There is no escrow, no raft-replicated copy, no backup, no
  `age`-sealed blob, and no key-recovery service. If an operator
  wants escrow for a tunnel key, that is a deliberate decision to
  weaken the property this ADR exists to preserve, and it is not
  something this design offers by default.
- The one thing that *is* recoverable is the *peer* side: a peer's
  private key lives only on the peer's own Comb, and a peer that
  loses it is re-keyed the same way. The two ends are symmetric.
- **Operator access survives a lost key without WireGuard**, which
  is the property that makes "not recoverable" acceptable: the
  Comb's own serial console / physical access, `cmd/managerd`'s
  startup flags, and the physical recovery path ADR-0114's reasoning
  relies on. A tunnel is an edge; losing an edge must never be the
  reason a Comb is unreachable.

### 4. Tunnel lifecycle, and the evidence it produces

**Creation.** `CreateTunnel` is a raft `Apply` of a
`CreateTunnel` command; the FSM rejects a duplicate `id` (the
`CreateVM`/`CreateJail` precedent). The interface is created by the
owning Comb's reconciler, not by the RPC handler — the RPC never
touches the network stack. A tunnel with no peers is legal and
means "listening, nobody has called".

**Peer add/remove.** `SetTunnelPeer` / `RemoveTunnelPeer` are raft
`Apply`s like any other command. Add is additive and safe. Remove
is a one-entry config change on one Comb and touches nothing else —
this is the small, good, per-peer blast radius the design leans on.

**Interface apply.** Converged by the owning Comb's reconciler, one
`wg` interface per tunnel, named `wg<index>` (WireGuard's own
`ifconfig` naming; a collision is reported as drift, not silently
resolved). The apply order is fixed and matters:

1. `ifconfig wgN create`
2. `ifconfig wgN address <address_cidr>`
3. `ifconfig wgN mtu <mtu>` — before the peer set, because a wrong
   MTU with a peer already attached produces silent partial
   connectivity
4. `ifconfig wgN listen-port <port>`
5. `ifconfig wgN private-key <key_path>` (or
   `wg setconf`/`wg syncconf` for the peer set, which is the
   idempotent path)
6. route installation, only if the tunnel declares routes
7. `ifconfig wgN up`

The reconciler writes the rendered config to
`/var/db/apiary/wireguard/known-good.conf` (`0600`, fsynced)
*before* step 1, exactly as ADR-0129's firewall design writes
`known-good.rules` before its staged apply. `RenderConfig` follows
`internal/cloudflare/tunnel.go`'s precedent precisely: it validates
its own inputs, it renders a **sorted** peer set so the same desired
state always produces byte-identical output, and it is the rendered
bytes that get diffed against disk.

**Unreachable peers and failed handshakes — the evidence
distinction.** Three new `assumptions.Kind` constants, continuing
`api/rpc/manager.proto` line 2515 (**next free: 6, 7, 8**), each
`SubjectKind: SubjectKindNode` with the tunnel's `subject_id`:

- **`wg_interface_present`** (6). `true` when `internal/netif.List()`
  reports an interface whose name matches the tunnel's expected
  name, up, and carrying the tunnel's `address_cidr`. `false` +
  `interface_missing` / `address_mismatch` / `interface_down` on a
  positive observation. `unknown` + `stale` when there is no fresh
  observation. `not_applicable` when no tunnel is assigned to this
  Comb, per the `checkNATUplink` precedent.
- **`wg_peer_handshake_observed`** (7). This is the one the ADR
  exists to get right, so the rule is stated exhaustively:

  - `true` when `wg show <if> latest-handshakes` for the peer is
    within the observation window (default 180 s — see Open
    questions) **or** when the peer's `rx`/`tx` byte counters
    advanced since the previous poll;
  - `unknown` + `no_handshake_yet` when the peer has **never** been
    observed to handshake, and the interface is up, and the endpoint
    is configured, and the window has not yet elapsed. *This is the
    case ADR-0056 and ADR-0118 exist to make honest.* A tunnel that
    was just created and has not seen a packet is not a broken
    tunnel;
  - `false` + `handshake_stale` only when a handshake **was**
    previously observed and none has occurred within the window —
    i.e. a regression from a known-good state, which is a real,
    actionable failure;
  - `false` + `handshake_rejected` when the peer's counter set shows
    handshake attempts that produced no completed session, or when
    `wg` reports an authentication error;
  - `unknown` + `endpoint_unresolved` when the peer's endpoint is a
    hostname that does not currently resolve, and `unknown` +
    `wg_unavailable` when `wg show` itself could not be run
    (interface missing, `wg` binary absent, permission denied) — all
    three are *absence of evidence*, and reporting any of them as
    `false` would put a `false` on a dashboard that means "we know
    this is broken".

  A `false` here never causes automatic action. It is a signal for
  a human and for the coverage scenarios.
- **`wg_config_digest_applied`** (8). `true` when the digest of the
  rendered config on the Comb equals the digest of the generation
  raft committed. `false` + `drift_detected` /
  `config_write_failed` on positive observation. This is the
  honesty check that `managerd` did what raft decided, and it is the
  same shape as ADR-0129's `pf_policy_anchor_applied`.

Every `Detail` goes through `assumptions.ClampDetail` (500 chars,
credential-shaped substrings redacted) — mandatory, because `wg`
output and `ifconfig` stderr are untrusted text that can contain an
operator's typo'd hostname or, in a path, something
credential-shaped. **A `Detail` must never contain a private key,
even redacted**: the checkers read counters and timestamps, not
`wg show all dump`.

`internal/health`: **no change to the verdict chain in v1.** Folding
tunnel state into `ComputeNodeHealth` would silently change existing
health verdicts for every current deployment, which is ADR-0122's
territory and not this ADR's to take. The signal is delivered through
`/assumptions` and the ADR-0122 evidence API.

`internal/coverage`: add scenarios asserting (a) a brand-new peer
produces `unknown`, never `false`; (b) a peer that handshakes once
and then stops produces `false` + `handshake_stale`; (c) a
`wg`-unavailable Comb produces `unknown`, never `false`; (d) the
admissibility check rejects a route change that would capture the
Colony's management path.

### 5. Routing and DNS, and rollback

**DNS: the tunnel never manages DNS.** `wg-quick`'s
`DNS =` / `resolvconf` integration rewrites `/etc/resolv.conf`. A
management-plane tool rewriting the host's resolver is a
self-inflicted outage with no diagnostic, and there is no
`known-good.resolv.conf` analogue that makes it safe. Therefore:

- `TunnelDefinition` has **no DNS field in v1**, `wg-quick` is not
  invoked at all (the reconciler uses `ifconfig`/`wg` directly, so
  there is no `wg-quick` side effect to suppress), and the host's
  resolver is never touched by this feature.
- A site-to-site deployment that genuinely needs internal name
  resolution is an operator concern, handled outside Apiary. This is
  Open question 4.

**Routes: `AllowedIPs` is the kill switch, and it is checked
statically before anything is committed.** WireGuard's `AllowedIPs`
is simultaneously a peer ACL and a policy routing table. A peer with
`AllowedIPs = 0.0.0.0/0` captures *all* outbound traffic on the host,
including the managerd gRPC call that a leader-gated write depends
on, including the raft transport itself, and including the operator's
own SSH session. This is the single worst thing a bad tunnel
definition can do, and it is the reason the admissibility check
exists.

The check runs **in the FSM, on the rendered configuration**, so the
rejection is replicated and identical on every voter — ADR-0129's
"rejected at apply time, in the FSM" pattern, adopted wholesale
because the failure mode is the same shape (a ruleset that can cut
the management path) and the proven remedy is the same (deny by
default, check the rendered artifact, name the violated invariant).
Mandatory invariants:

- **The Colony's own subnets may not appear in any `AllowedIPs`.**
  Derived from raft membership (`internal/raft.Node.Status()`'
  `ServerInfo{ID, Address, Suffrage}`) and from `managerd`'s bind
  address, not hardcoded, so ADR-0108's changed fixed-listener ports
  cannot silently invalidate the check.
- **A default route requires an explicit, separate opt-in** — a
  `full_tunnel: true` field on the `TunnelDefinition`, rendered
  visibly in the UI as "captures all outbound traffic on this
  Comb", and refused outright on any Comb that currently hosts a
  `NetworkDefinition` the Colony's Cells hang off. A Cell bridge is
  the Colony's product; a full-tunnel default route on the same Comb
  is a configuration that should be nearly impossible to reach by
  accident.
- **No `AllowedIPs` entry may overlap an existing
  `NetworkDefinition` subnet**, or another tunnel's
  `address_cidr`, or any `address_cidr` in the Colony's Cells. Two
  overlapping WireGuard cryptokey routing tables produce a routing
  loop that presents as "the tunnel is up and nothing works", which
  is the hardest version of this failure to diagnose.
- **MTU must be in [1280, 1420].** Below 1280 the interface cannot
  carry a valid IPv4 packet; above 1420 you get silent fragmentation
  and dropped SSH sessions that look like a key problem. 1420 is
  WireGuard's own default for IPv4 and is used when unspecified.
- **No interface name may be a non-`wg` name, and no tunnel may
  claim the Colony's management interface.** The `wg<index>` naming
  rule makes this checkable.
- **Reject any peer referencing an interface that does not exist on
  any member node** (`internal/netif.List()` on each Comb,
  evidence-gathered). This is ADR-0129's `on em9` invariant
  verbatim: a typo'd name is something pf accepts and silently never
  matches, and `ifconfig` is worse because it will happily create
  it.

A tunnel that fails admissibility never reaches raft. Failure is
explicit, in the RPC response, with the violated invariant named.

**Staged apply, local deadline, local rollback.** Consistent with
ADR-0129's design, and reusing its four mechanisms rather than
reinventing them:

1. **Static admissibility check**, above, in the FSM.
2. **Staged apply with a local deadline.** A committed route-affecting
   change becomes `PENDING`, not `ACTIVE`. The owning Comb writes
   `pending.<generation>.conf` and `known-good.conf` (both `0600`,
   both fsynced), applies the staged config, starts a monotonic
   deadline (default 120 s, matching ADR-0129 so the two pages in the
   UI read the same way), and reports continuously whether its own
   management path is intact *under the staged config*.
3. **Automatic rollback, performed locally by `managerd` on the
   Comb itself**, with the same defining constraint as ADR-0129's:
   **it must not require raft, the leader, or the operator.**
   - The actor is `managerd` on the Comb, driven by its own timer.
   - The source is the Comb's local `known-good.conf`. If that is
     missing or unreadable, the second source is the previous
     generation from raft; if raft is also unreachable, the third and
     final fallback is **`ifconfig wgN down` + `ifconfig wgN
     destroy`**, i.e. *remove the tunnel entirely*. Fail safe,
     always, and record that it did.
   - The trigger is the deadline expiring without a positive
     `wg_management_path_intact` observation in the preceding 15 s.
     The two probes are the same ones ADR-0129 uses: a successful
     gRPC call to at least one raft peer, and a successful
     bind-and-accept on its own managerd listener.
   - Rollback is verified in the cheap direction: restore
     `known-good.conf` (or destroy the interface), confirm the
     probes pass, then report `ROLLED_BACK` naming the generation
     fallen back from. If even that fails, report
     `ROLLED_BACK_UNVERIFIED` — a loud, honest "we tried and we are
     not sure", which is `unknown` on the operator's screen, never
     `false`.
4. **Operator confirmation to promote, not to apply.** `PENDING`
   becomes `ACTIVE` on an explicit `CommitTunnel`, or automatically
   when the deadline passes with the probes green. There is no state
   in which a route change becomes permanent by accident.

**The one genuine asymmetry with ADR-0129, and it is in our favour.**
Tearing a `wg` interface down is a kernel-level operation with no
persistent host footprint: `ifconfig wgN destroy` leaves the host
*exactly* as it was before the tunnel existed. It does not touch
`/etc/pf.conf`, it does not need `pfctl -f`, it does not flush a
state table, and there is no "the last known good ruleset is gone"
moment. ADR-0129's floor — the empty, everything-allowed anchor
flush — has an exact WireGuard equivalent that is *strictly better*,
because it is the genuine pre-change state rather than a permissive
approximation. That asymmetry is a real argument for this feature
being safe to ship earlier than the firewall's host-scope work, and
it should be said out loud rather than left for an operator to
discover.

### 6. Quorum, and a degraded Colony

The split is ADR-0129's, unchanged, because the reasoning is
identical:

- **Deciding requires quorum.** A tunnel or peer change is a raft
  `Apply` against the leader. Raft commits a majority or not at all.
  There is no local, off-ledger path that changes a tunnel. In a
  degraded Colony with no leader the operator gets `ErrNotLeader`
  plus a `LeaderHint` — the exact existing behaviour of
  `SetVMFirewallRules` and `SetVMCloudflareExposure`, not a special
  case.
- **Executing must not.** Once committed, every member applies it
  locally, including a member that has since lost contact with the
  leader. A half-applied Colony is strictly worse than a fully-
  applied one, because the operator cannot tell which Combs are
  covered. A Comb's local apply is gated on its own generation
  counter, never on a fresh quorum check.
- **Rollback is raft-independent by construction.** No rollback path
  in this design consults raft before attempting to restore
  connectivity. The `local file → raft history → destroy interface`
  chain tries each in order, per Comb, without coordination. This is
  the reason the rollback floor is *interface destruction* and not
  "ask the leader what we had last": a Comb whose tunnel just killed
  its raft path cannot ask the leader anything.

**The degraded-Colony case, stated as an operator scenario.** A
Colony has lost quorum. The operator still has console access to
each Comb. They cannot add a tunnel, cannot add a peer, cannot
remove one, cannot rotate a key. What they *can* do is: read the
evidence (which is local and ungated), roll back any pending change
whose local deadline has not yet fired, and physically remove a
tunnel by hand with `ifconfig wgN down && ifconfig wgN destroy` at a
console — a one-liner that this design deliberately does not
automate, because automating it would require an RPC that can mutate
host networking without quorum, and that is the line this ADR
refuses to cross. ADR-0127's `apiaryctl courier` CLI (Phase 4) is
the right future home for an operator-initiated, quorum-free
`tunnel teardown`, and it should be added there.

### 7. Interaction with the existing Cloudflare Tunnel and sidecar

**There is real adjacency and no real overlap.** Being precise
about which owns which path:

- `internal/cloudflare` owns **egress to a third party for inbound
  exposure of a Cell's own HTTP port**. It speaks Cloudflare's
  tunnel protocol, authenticates with a credentials JSON that is an
  operator-supplied startup-flag path, and creates DNS records in a
  Cloudflare zone. Its "tunnel" is an outbound connection from the
  Comb to Cloudflare, and the public hostname terminates at
  Cloudflare.
- `internal/wireguard` owns **a private overlay between endpoints
  Apiary administers**. It speaks WireGuard, has no third party, has
  no DNS records, and the "tunnel" terminates at the far peer's
  WireGuard address.
- **Neither is a substitute for the other and neither is a backup
  for the other.** If Cloudflare is down, WireGuard does not serve
  a public hostname. If a WireGuard peer is down, cloudflared does
  not care. There is no failover story here, and pretending otherwise
  would be a design that fails at the moment it was needed.

**Three concrete interactions, each with a rule:**

1. **Overlap in mechanism, deliberately not in code.** Both are
   "a process or interface started by `managerd` via `daemon(8)` or
   `ifconfig`, converging toward a desired set, restarting on
   content-diff or failed liveness." `internal/wireguard` copies
   `internal/cloudflare`'s *shape* — nil-able reconciler field,
   fixed `RunDir` under `/var/db/apiary`, content-diff restart,
   `processAlive`-equivalent liveness — but does **not** share its
   code, because the two converge genuinely different things and a
   common abstraction over "start something and watch it" would be
   a three-line interface with five implementors and one real user.
   What *is* shared is the discipline, which is why this section
   exists.
2. **A bad MTU silently breaks cloudflared.** cloudflared is a
   long-lived outbound connection; a WireGuard interface with a wrong
   MTU will break it in a way that surfaces as "the tunnel is
   flapping", not as a routing problem. Therefore: the MTU invariant
   above is checked statically, and a `wg` interface's presence is
   surfaced on the same Combs' **Networking** page where the
   Cloudflare exposure status already lives, so an operator
   debugging cloudflared sees the overlay in the same screen. This
   is a real, predictable interaction and it belongs in the design
   rather than in a bug tracker.
3. **A Cell reachable only through a private overlay must not be
   given a public Cloudflare hostname.** `internal/raft.FSM`'s
   `SetVMCloudflareExposure` is the validation boundary ADR-0063
   relies on (`internal/cloudflare/tunnel.go`'s `RenderConfig` doc
   says so explicitly: "Hostname is validated at
   internal/raft.FSM's SetVMCloudflareExposure boundary" - which is
   `internal/raft/fsm.go`'s `applySetVMCloudflareExposure`, line 383,
   whose own check reads "only alphanumerics, '-', and '.' are
   allowed" (line 393). This ADR
   **adds a second check at that same boundary**: a Cell whose
   address falls inside any `TunnelDefinition`'s `address_cidr`, or
   inside any peer's `AllowedIPs`, is refused a
   `cloudflare_hostname`. Publishing a public hostname that proxies
   into a private site-to-site overlay turns "unreachable from the
   internet" into "reachable from the internet by anyone who knows
   the hostname", and it would be a genuine security regression
   introduced by enabling a VPN. This is the sharpest
   cross-feature interaction in the whole design and it is why the
   two features must be sequenced together in review even though
   neither calls the other.

### 8. Boundary with ADR-0133 (Certificate Management)

ADR-0133 is a sibling written in parallel, on Certificate Management
— the other half of ADR-0127's Phase 3. The split is by *credential
class*, and it is important that it be a clean line, because both
features involve "a key, in raft, rotated by a background task".

**ADR-0133 owns, and this ADR explicitly does not touch:**

- The TLS/ACME certificate chain: `type` (imported, self-signed,
  lets-encrypt, acme), `domains[]`, `key_algorithm`,
  `renewal_window`, and every ACME challenge type.
- **Expiry evidence.** `internal/origincert`'s `InventoryEntry`,
  `RenewalWindow` (30 days), `ExpiryOK` / `ExpirySoon` /
  `ExpiryExpired`, and the expiry-driven UI. A WireGuard private
  key has no expiry — that is a feature of it, and it is the reason
  its rotation cannot be automated the way a certificate's is.
- Auto-renewal, the background renewal task, and the SIGHUP-style
  hot reload of daemons on renewal.
- The peer TLS hostname map update on renewal
  (`peer_tls_hostname_map`, ADR-0115): that map exists to give
  `PeerReporter` a correct `ServerName` when dialing a peer by IP.
  It is a *routing hint for TLS*, has no key material, and is not a
  distribution channel for anything.
- Whether Apiary runs an internal CA.

**This ADR owns, and ADR-0133 explicitly does not touch:**

- Where a WireGuard private key is generated, on which Comb, at
  what path, with what file mode, and the refusal to start when that
  mode is wider than `0600`.
- The fact that a WireGuard key is **never** raft-replicated, in
  plaintext or encrypted. This is the sharpest of the split and it
  cuts across the 0127 family, which sketches *both* `private_key`
  on the tunnel CRD *and* "Private keys in Raft-replicated secrets
  (encrypted at rest)" for certificates. This ADR's position is
  that the answer for both is **never replicate**, because the
  encrypted-at-rest machinery does not exist here and building it is
  out of scope; ADR-0133 is welcome to reach the same conclusion
  independently and is strongly encouraged to.
- Key rotation, key loss, re-keying, and the generation counter.
- QR/config **export of a peer's own config** — produced on the
  peer's Comb, containing that peer's private key, and therefore a
  local export operation, not a raft field and not a
  managerd-served byte stream over a gRPC response an API-key-gated
  client could fetch in bulk.

**The join-flow interaction, and it is the one to be careful about.**
Both features enrol a credential against a Combs' identity, and both
touch the same moment — Colony join. ADR-0127 suggests
"auto-provision on Colony join (derive from peer TLS hostname map)".
This ADR **rejects auto-provisioning on join**, and the reason is
that it would undo ADR-0113. ADR-0113 exists because a node joining a
Colony should not be trusted merely because it completed a TLS
handshake — a human must compare a fingerprint and decide. Adding a
WireGuard key to a Comb as a *side effect of approving a join
request* would grant a second, network-level identity to the same
node at the same unexamined moment. Therefore: WireGuard key
enrolment is a **separate, explicit, operator-confirmed action**,
always after the join, always against a displayed
`key_fingerprint`, and never derivable from the TLS fingerprint or
the hostname map. A Comb with no tunnel has no tunnel; a Comb
approved to join has joined and nothing more.

## Rejected alternatives

### 1. Multi-backend VPN abstraction

Rejected for v1. The cost is not the interface — it is that a
backend interface forces every future backend to be a peer of
WireGuard in the reconciler, the evidence model, the frontend, and
the rollback ladder, and the second backend will never be as
well-tested as the first, so the interface's promise of uniformity
will be a lie precisely where an operator most needs it to be true
(evidence semantics differ per backend, and "unknown vs failed" is
the hardest part of this ADR). Tailscale additionally means either
running its coordination server (a third-party control plane for a
system whose entire value proposition is that its control plane is
yours) or running Headscale, which is a second raft-like system to
operate alongside the first. Reversal conditions are named in
Decision §1.

### 2. Replacing the Colony's mTLS control transport with WireGuard
tunnels

Rejected, and forbidden explicitly (Decision §2). The reasoning is in
Decision §2 and it reduces to one sentence: a control transport that
lives on a tunnel turns a tunnel failure into a quorum failure and
leaves no recovery path, because the mechanism used to recover the
tunnel is the thing the broken tunnel removed. Independent of that,
it is a strict functional downgrade: `tlsdial` + `PeerReporter`
gives per-request identity, an API-key gate, TLS `ServerName`
verification, and a `LeaderHint`; a WireGuard interface gives an
encrypted packet and nothing else.

### 3. Replicating the private key in raft (plaintext, or encrypted
at rest)

Rejected; this is the core of Decision §3. Plaintext replication puts
a Curve25519 private key in every voter's memory, every snapshot and
every log segment. "Encrypted at rest" moves the compromise surface
rather than reducing it — the envelope key must be readable by every
Comb that applies the entry — and the machinery does not exist in
this tree. The existing precedent (`internal/origincert` "deliberately
has no raft dependency", `internal/raftdconfig`'s `0600`
`internal_token`/`raft_tls_key`) is the house rule already, and it is
already load-bearing on the raft transport's own TLS.

### 4. A WireGuard mesh between Cells (ADR-0127's "WireGuard mesh as
Cell interconnect")

Rejected for v1, and it is not a small deferral. Apiary's Cells are
L2-attached to a bridge on their owning Comb.
`VMDefinition.network_id` resolves through
`internalpb.NetworkDefinition` to a bridge managed by
`internal/netif`/`internal/pf` (ADR-0022). A Cell-to-Cell
WireGuard mesh therefore needs either a `wireguard-go` (userspace)
implementation inside every guest, with its own MTU, its own routing
table, and its own CPU cost per Cell, or a layer-3 gateway Comb that
routes between Cells — which is a new product concept, not a
networking feature, and which re-introduces the Cell-interconnect
problem ADR-0117 and the jail network work are already about. v1
ships the tunnel terminating on the Combs; guest reachability is
"an address inside a Combs' tunnel", not a mesh. Revisit if
per-Cell userspace WireGuard becomes cheap enough to be free, which
is a runtime question, not an architectural one.

### 5. WireGuard as the rendezvous mechanism for the air-gapped
courier

Rejected as physically incoherent, and worth calling out because
ADR-0127 states it plainly: an air-gapped operations courier is
carried across a physical gap. A WireGuard endpoint requires packets
to arrive. There is no configuration in which a packet-carrying
protocol provides rendezvous across an air gap, and building the
tunnel machinery to try would produce a feature that appears to work
in a lab and fails on the one day it is used. The courier's needs are
addressed by the filesystem-shaped workflow ADR-0127 itself
describes elsewhere — `apiaryctl courier prepare|sync|verify` on
physical media, over the existing `UploadISO`/`PushISOTo` and
`receive_jail` streaming patterns. What a WireGuard tunnel *can* do
for a courier-adjacent workflow is be the transport for a
**connected** site-to-site link, which is Decision §1's
site-to-site case and is in scope.

### 6. `wg-quick` and generated config files

Rejected. `wg-quick` is a shell script that does several things this
design must not let happen implicitly: it edits
`/etc/resolv.conf` when a config carries `DNS =`, it edits
`/etc/wireguard/` outside the project's run-dir convention, it
manages firewall rules, and it is not a stable interface across
versions. The reconciler uses `ifconfig` and `wg setconf`/
`wg syncconf` directly, writes only under `/var/db/apiary/wireguard/`,
and never touches a file outside its own run dir. The cost is more
code in `internal/wireguard`; the benefit is that every side effect
is one this ADR can bound, which is the whole point.

### 7. Automatic key rotation on a timer, like certificate renewal

Rejected. A WireGuard key is shared with every peer and there is no
negotiation: rotating it without every peer updated in the same
moment severs every session. A background rotation task that runs at
03:00 and invalidates a mesh is a self-inflicted outage, and the
"but it's automated" argument is exactly the argument ADR-0127 makes
for certificate auto-renewal, where the credential is *not* shared
with third parties and renewal is transparent to them. Different
credential class, different answer. See Decision §3.

## Consequences

### Positive

- Operator access to a Comb over an untrusted network stops
  requiring managerd's, the frontend's, and the raft listener to be
  exposed to that network. That is a real reduction in attack
  surface, and it is the change an operator will notice first.
- A tunnel can be torn down with one local command and no quorum, no
  leader and no coordinator, leaving the host in its exact
  pre-tunnel state. That is a stronger safety property than
  ADR-0129's host-scope firewall can offer, and it is why this
  feature is a reasonable Phase 3 candidate rather than a Phase 1 one.
- Private key material is confined to the Comb that must hold it, in
  one `0600` file under one directory, with no new key-encryption
  machinery invented to justify replication.
- The evidence model is honest by construction: a peer that has
  never been seen handshaking is `unknown`, not `false`, and an
  unreachable `wg` binary is `unknown`, not `false`. A dashboard
  that cannot lie is a dashboard an operator will trust during an
  incident.
- The design costs one new package plus new slices of four existing
  ones, and it touches nothing on the control path. The blast radius
  of a code regression in `internal/wireguard` is one Comb's
  networking edge, not the Colony.
- Nothing in the feature can cause a quorum failure, by
  construction, and that is stated as a prohibition rather than
  hoped-for.

### Negative

- **The operator loses the ability to recover a lost key.** This is
  a deliberate trade and it will surprise people. A Combs' entire
  tunnel identity is unrecoverable if its key file is destroyed, and
  re-keying means re-provisioning every peer by hand. The
  mitigation is that the Comb is still reachable by console, but
  that is a weaker answer than "restore from backup" and the
  frontend must say so plainly rather than offering a help link.
- Peer provisioning is manual and staged. A five-site mesh takes
  five deliberate, ordered, operator-confirmed actions. There is no
  mesh-in-one-click, and adding one later means adding a
  distribution mechanism, which is the thing Decision §3 refuses to
  build.
- The design is FreeBSD- and `wg(4)`-shaped. `ifconfig wgN ...` is
  FreeBSD syntax; the same `internal/wireguard` would not port to
  Linux unchanged. That is acceptable — Apiary is a FreeBSD cluster
  manager — but it means the package is not a reusable component.
- Rotation being manual means a key that leaks in year three is
  rotated in year three, on a schedule driven by a human noticing.
  There is no compensating control here beyond "it is a
  `0600` file on a Comb the operator already has root on".
- The `full_tunnel: true` opt-in is a genuine footgun that survives
  every mitigation. It is refused on any Comb hosting a
  `NetworkDefinition`, which covers most real deployments, but on a
  bare Combs with no Cells it remains reachable — deliberately,
  because a legitimate site-to-site full tunnel is a real use case,
  and a feature that cannot express a real use case gets worked
  around, which is worse.
- Adding a second admissibility check to
  `applySetVMCloudflareExposure` in `internal/raft/fsm.go` is a
  change to an existing, working path in a feature this ADR does not
  own. It
  is small and correct, but it is a cross-feature edit and needs
  review by whoever owns ADR-0063's implementation.

## Implementation notes

### Raft state (`api/internalpb/state.proto`)

New messages, appended after `PurgeJoinRequest` / alongside the
`RestartLease` group. Field numbering must avoid the ranges already
in use; each message is new so numbering starts at 1.

```proto
// TunnelLifecycle is a TunnelDefinition's own application state,
// distinct from its desired content. PENDING is the staged-apply
// window described in ADR-0132's Decision section 5; it exists only
// for generations that change routes or the interface's own key.
enum TunnelLifecycle {
  TUNNEL_LIFECYCLE_UNSPECIFIED = 0;
  TUNNEL_LIFECYCLE_PENDING = 1;
  TUNNEL_LIFECYCLE_ACTIVE = 2;
  TUNNEL_LIFECYCLE_ROLLED_BACK = 3;
  TUNNEL_LIFECYCLE_ROLLED_BACK_UNVERIFIED = 4;
}

message TunnelPeer {
  string public_key = 1;      // base64 Curve25519; the only peer secret
  repeated string allowed_ips = 2;
  string endpoint = 3;        // "host:port"; empty = dial-only peer
  uint32 keepalive_seconds = 4;
  string comment = 5;
  bool enabled = 6;
}

message TunnelDefinition {
  string id = 1;
  string name = 2;
  string node_id = 3;         // the owning Comb
  uint32 listen_port = 4;
  string address_cidr = 5;    // this interface's own address/prefix
  uint32 mtu = 6;             // 0 = default 1420
  string public_key = 7;      // THIS tunnel's own public key
  string key_fingerprint = 8; // short hash of public_key, for display
  string key_path = 9;        // where the private key must be, never
                              // its contents - see ADR-0132
  repeated TunnelPeer peers = 10;
  bool full_tunnel = 11;      // explicit opt-in for 0.0.0.0/0 capture
  uint64 generation = 12;
  TunnelLifecycle lifecycle = 13;
  string pending_public_key = 14; // set only during a staged rotation
  uint64 pending_generation = 15;
  string lifecycle_note = 16;     // human-readable; never a secret
}
```

The commands: `CreateTunnel`, `UpdateTunnel` (name, mtu,
`full_tunnel`), `SetTunnelPeers` (whole-set, the shape
`SetVMFirewallRules` already uses), `CommitTunnel` (promote
`PENDING`→`ACTIVE`), `BeginTunnelKeyRotation`, `PurgeTunnel`.
`Command` gains oneof cases numbered from **17** (16 is
`set_vm_cloudflare_exposure`).

**What is NOT replicated**, stated so it is not mistaken for an
oversight:

- **The private key.** Decision §3. Not in the message, not in the
  snapshot, not in any RPC response, ever.
- **The generated `wg` interface's runtime state**: `latest
  handshake` timestamps, `rx`/`tx` byte counters, `last handshake`
  ages, the kernel's own peer list. High-churn, physical, and two
  Combs with identical definitions will never have identical
  counters — replicating them produces permanent, meaningless
  divergence and real log pressure for zero decision value. This is
  ADR-0129's "packet counters" exclusion, for the same reason.
- **The rendered config file's content** as raft truth. The rendered
  bytes are a *derivation*; the truth is `TunnelDefinition`, and a
  Comb whose file disagrees with raft is **drift**, repaired by
  re-rendering, not a conflict to resolve. `wg_config_digest_applied`
  reports the drift.
- **Which `wg` interfaces exist on a Comb.** A report, like
  `internal/netif`'s inventory.
- **The `known-good.conf` content** — recoverable from raft
  (previous generation), but the design never *requires* raft to
  reach it, because raft may be the thing that was cut. Local disk
  primary, raft secondary, interface destruction the floor.

### Raft state keys (`internal/raft/fsm.go`)

`FSM` gains `tunnels map[string]*internalpb.TunnelDefinition`,
initialised beside `vms`/`networks`/`jails`. Every mutation is a
`Command_*` case; every read path (`ListTunnels`, and the reconciler's
desired-state read) is an **ungated** read, because a Comb that has
lost the leader must still converge its own local interface and, more
importantly, must still be able to roll back.

`applySetVMCloudflareExposure` gains the second admissibility check
named in Decision §7 item 3: refuse when the Cell's resolved
address falls inside any `TunnelDefinition.address_cidr` or any peer
`allowed_ips`. This is the one edit this ADR makes outside its own
feature's surface.

### `internal/wireguard`

New package, mirroring `internal/pf` and `internal/cloudflare` in
shape, not in code:

- `DefaultRunDir = "/var/db/apiary/wireguard"` (`0700`), config and
  known-good files `0600`.
- `RenderConfig(def) (string, error)` — validates its own inputs
  rather than trusting the caller's validation (the
  `internal/cloudflare/tunnel.go` `RenderConfig` lesson: the
  rendered bytes are what `wg` loads), **sorts the peer set** so the
  same desired state always renders byte-identically, and refuses
  newlines in any field, because a newline in a rendered key is an
  injected second stanza.
- `Manager.EnsureRunning(ctx, def) error` — converges the interface,
  restarts on a rendered-config content diff, and separately on
  failed liveness. Liveness for an interface is not a pidfile: it is
  `internal/netif.List()` reporting the interface up with the
  expected address, checked independently of the content diff, for
  the same reason `processAlive` is (ADR-0063 finding 6).
- `Manager.StopIfRunning(ctx) error` — down + destroy + remove the
  config, callable with no feature flags configured, mirroring
  `internal/cloudflare`'s fixed-`RunDir` property.
- `KeyStore` — generate, load (refusing any mode wider than `0600`),
  write atomically (temp + `Chmod` + fsync + rename), and re-key.
  This is the only code in the feature that touches a private key,
  and it is small on purpose.
- `Admissible(def, colony) error` — the static check from Decision
  §5, in the FSM, over the rendered config.

**Failure modes:** `ifconfig` missing or a kernel without `wg(4)` →
`wg_unavailable`, `unknown` evidence, feature stays configured but
inert with a prerequisite banner, never a false verdict. Key file
missing → the tunnel is not created and the evidence is `false` +
`key_absent` for *this* Combs, because unlike a peer handshake this
is a positive, locally-verifiable absence. Key file present but mode
too wide → refuse to start, per Decision §3. Rendered config
unparseable by `wg` → `config_rejected`, `ROLLED_BACK`, no interface
left half-created.

### managerd RPC handlers (`internal/manager/server.go`,
`api/rpc/manager.proto`)

New `ManagerService` RPCs, appended after `UpdateVoterAddress`:
`CreateTunnel`, `GetTunnel`, `ListTunnels`, `UpdateTunnel`,
`SetTunnelPeers`, `BeginTunnelKeyRotation`, `CommitTunnel`,
`ExportTunnelPeerConfig` (local-only; returns a rendered `wg-quick`
config **for a peer being provisioned**, containing only *that
peer's* key, and only on the Comb that holds it),
`PurgeTunnel`, `GetLocalTunnelStatus` (the live, node-local,
**ungated** observation: interface presence, per-peer handshake age,
counters, config digest).

Every mutating handler follows the existing `SetVMFirewallRules` /
`SetVMCloudflareExposure` shape: forward to the leader when this node
is not the leader, return `ErrNotLeader` + `LeaderHint` when there is
no leader, return the FSM's admissibility error verbatim so the
violated invariant is named in the response. `ExportTunnelPeerConfig`
is the one handler that is deliberately **not** forwarded, because
the key it returns is node-local by definition; forwarding it would
mean moving a private key across a process boundary to a client that
does not need it there.

New `AssumptionKind` values **6, 7, 8** at
`api/rpc/manager.proto` line 2515:
`ASSUMPTION_KIND_WG_INTERFACE_PRESENT`,
`ASSUMPTION_KIND_WG_PEER_HANDSHAKE_OBSERVED`,
`ASSUMPTION_KIND_WG_CONFIG_DIGEST_APPLIED`.

### Reconcile loop (`internal/cluster/reconciler.go`)

`Reconciler` gains `WireGuard wireGuardManager` (nil-able, exactly
like `Cloudflare` at line 327) and `WireGuardConfigured()`. Each
tick, per owned tunnel, in order:

1. Read the local ungated `TunnelDefinition`. No leadership check.
2. If absent from raft → `StopIfRunning` and return. (A purged
   tunnel must not leave an orphaned interface: this is
   `ADR-0063` finding 4's lesson, restated.)
3. Render, write `known-good.conf` if `generation` is new, diff
   against the applied config.
4. If the change is route-affecting or key-affecting and the current
   lifecycle is `ACTIVE`, mark `PENDING` and stage. Otherwise apply
   directly.
5. Apply, and on `PENDING` start the local deadline and begin
   probing: a successful gRPC call to at least one raft peer, and a
   successful bind-and-accept on its own managerd listener. Record
   `wg_management_path_intact` continuously.
6. Report evidence. **The evidence step never gates the apply step**,
   and the deadline is enforced by a local timer owned by
   `managerd`, not by this tick — a reconcile loop that is not
   running must not be able to strand a Comb on a pending route
   change.
7. On deadline expiry without a green probe: `StopIfRunning` and
   restore `known-good.conf` (or, if neither works, destroy the
   interface), then report `ROLLED_BACK` or
   `ROLLED_BACK_UNVERIFIED`.

Failure modes: raft unreachable → the reconcile still runs from the
last known local generation, because a Comb that cannot reach the
leader must still be able to roll back. A local timer that fails to
arm → the change stays `PENDING` and is reported; it is never
silently promoted. An interface that exists but is not the one raft
describes → `wg_interface_present: false` + `drift_detected`, and
`StopIfRunning` then re-apply, which is safe because a WireGuard
interface is cheap to destroy and recreate.

### Frontend surface (`internal/frontend`)

One **Tunnels** page per Comb, mirroring ADR-0129's Firewall page in
spirit: desired vs observed, generation, `known-good_generation`, a
live countdown when pending, the last three drift-and-repair events,
per-peer handshake age rendered as `unknown`/`stale` in
distinguishable styling, and a prerequisite banner when `wg(4)` is
unavailable. A peer's private key is **never** on this page or any
page — the export action produces a file/download on the Comb that
holds it, and the page says in as many words that the key is
recoverable only by re-keying. A Comb's Networking page shows whether
a `wg` interface is present and at what MTU, because that is where
an operator debugging cloudflared will look (Decision §7 item 2).

## Test plan

### Validatable on macOS (no FreeBSD required)

- `RenderConfig` determinism: the same desired set sorted
  differently renders byte-identically; a newline in any field is
  rejected; peer order in the proto does not change the digest.
- `Admissible`: each invariant in Decision §5 has a case —
  management subnet in `AllowedIPs`; `0.0.0.0/0` without
  `full_tunnel`; `full_tunnel` on a Comb with a
  `NetworkDefinition`; overlapping `AllowedIPs` with a
  `NetworkDefinition`, another tunnel's `address_cidr`, or a Cell
  address; MTU 1279 and 1421 rejected, 1280 and 1420 accepted; a
  non-`wg` interface name; a peer naming an interface absent from
  every member node. The Colony inputs are constructed, so this is
  pure table-driven logic.
- The admissibility check produces the **same** verdict on a follower
  as on the leader, because it lives in the FSM and is exercised
  through the real apply path, not by calling the function directly.
- `KeyStore`: atomic write leaves no partial file on simulated
  failure; a file at `0644` or `0640` is refused, not warned about;
  a missing key file yields `key_absent`; re-key produces a different
  public key and a different fingerprint.
- Evidence truth table, exhaustively: a peer that has never
  handshaked is `unknown`/`no_handshake_yet`; a peer that handshaked
  once and then stopped is `false`/`handshake_stale`; a Comb where
  `wg show` cannot run is `unknown`/`wg_unavailable`; an unresolved
  endpoint hostname is `unknown`/`endpoint_unresolved`; counters
  advancing is `true` even with a handshake outside the window.
  **No input may produce `false` from an absent observation.** This is
  the honest-unknown rule as an executable assertion, not a comment.
- FSM: duplicate `id` rejected; `PurgeTunnel` idempotent; a
  `PENDING` tunnel is not promoted by a second identical apply;
  `SetVMCloudflareExposure` refuses a Cell inside a tunnel's
  `address_cidr` and one inside a peer's `AllowedIPs`, and accepts
  one outside both.
- The protobuf boundary: `wg show` text containing something
  credential-shaped is redacted by `ClampDetail` before it reaches a
  `Detail`, and a `Detail` can never contain a private key even
  unredacted (assert on the checker's own construction, not on
  `wg`'s redaction behaviour, because depending on a tool's redaction
  is a design error).
- `Manager.StopIfRunning` with no feature flags configured is a no-op
  that leaves the host unchanged — the fixed-`RunDir` property.
- Coverage scenarios: quorum-lost → every mutating RPC returns
  `ErrNotLeader` with a `LeaderHint`; evidence reads still succeed;
  a committed generation still applies locally on a follower.

### Only validatable on brood (10.90.0.94) or drone (10.90.0.95)

**macOS cannot validate any of this.** There is no `wg(4)`, no
`ifconfig wgN`, no real UDP, no NAT, no `pf`, no real-network timing
and no handshake of any kind on a Mac. Every claim below is a claim
that has to be measured on bare-metal FreeBSD or not made at all.

- **Does `wg(4)` exist and behave on the installed FreeBSD 16.0-
  CURRENT kernel**, and what is the actual `ifconfig wgN` syntax
  surface (create, address, mtu, listen-port, private-key, and the
  `wg setconf`/`wg syncconf` path) on this kernel specifically?
  Everything in `internal/wireguard` is written against that syntax
  and is wrong if it differs.
- **Real handshake timing.** The 180 s observation window in
  Decision §4 is a guess. Measure: time to first handshake after
  `ifconfig wgN up` on a real Comb pair; how long `latest-handshake`
  stays current under an idle but healthy tunnel; the behaviour of
  `rx`/`tx` counters for a peer that is up but sending nothing. The
  window must be comfortably longer than any observed healthy gap or
  the evidence model will produce false `false`s in normal operation —
  which is the one failure mode that would make operators stop
  trusting the page.
- **The staged-apply deadline under real conditions.** Does a
  120 s deadline leave enough time for a legitimate, slow route
  change to prove itself? Does the 15 s probe window hold under
  real network latency between Combs?
- **The real lockout rehearsal.** A Combs' own management path
  intact / severed, measured under a deliberately bad tunnel: a
  `full_tunnel: true` with a colliding `AllowedIPs`, applied for
  real, with the operator watching from a *separate* console. This
  is the ADR-0129 rehearsal repeated for the tunnel, and it is the
  only way to know the admissibility check's rule set is complete.
  If the rehearsal is not performed, this ADR's central safety claim
  is **untested**, and that must be stated rather than implied.
- **Real MTU behaviour**, including the fragmentation-and-silent-
  drop case, and the interaction with cloudflared (Decision §7
  item 2) under a deliberately wrong MTU.
- **Peer revocation latency**: how long a removed peer takes to
  actually stop being able to send, and whether the removal's
  evidence flips to the right value.
- **Key rotation rehearsal**: the full ordered dance with two Combs,
  including the failure case where one peer is missed, so the
  `unknown` result is observed rather than assumed.
- **Reboot survival.** A `wg` interface created by `managerd` does
  not survive a reboot. Whether the tunnel is re-established at
  boot (a startup hook), or whether the operator re-applies it, is
  Open question 1, and only a real reboot answers what the operator's
  actual experience is.

## Open questions

1. **Reboot survival.** A `wg` interface created by `managerd` is
   not persistent; after a reboot the tunnel is simply gone until
   something recreates it. Options: a `managerd` startup hook that
   re-applies every `ACTIVE` tunnel (safe, because the apply is
   idempotent and raft-independent — but it means a key rotation
   applied just before a reboot comes back automatically, which is
   correct); or checkpoint into `/etc/wireguard/`, which is a host
   file this project has decided not to manage (ADR-0129's
   reasoning, applied again); or do nothing and document the
   re-apply. Is startup re-application a Phase 1 requirement, given
   that a site-to-site tunnel silently vanishing on an unrelated
   reboot will read as a security event to the operator?
2. **The 180 s handshake window and the 120 s apply deadline are
   guesses.** They need brood/drone measurement (see Test plan).
   Should they be per-tunnel operator settings, or fixed constants?
   Fixed constants are simpler and safer to reason about; per-tunnel
   settings are more honest about a slow link. I lean fixed, with
   the measured numbers replacing the guesses.
3. **Peer distribution ergonomics.** v1 is a rendered config /
   QR-style export per peer, produced on the peer's Comb. Is that
   enough for a five-site mesh, or does the operator need a
   "distribute to this Combs' peer" action that pushes a peer's
   config across the Colony? That action would move a private key
   between Combs, which Decision §3 refuses, so the answer is
   probably "no" — but the operator experience of "no" should be
   confirmed before shipping.
4. **DNS for site-to-site.** v1 has no DNS field at all. A
   two-site deployment that needs to resolve internal names over the
   tunnel has to solve it outside Apiary. Is that acceptable, or does
   a real deployment need a managed, explicitly opt-in,
   `known-good.resolv.conf`-guarded resolver change? If the latter,
   it is a separate ADR — the resolver is a host file with the same
   properties `/etc/pf.conf` has.
5. **ADR-0127's "auto-provision on Colony join (derive from peer
   TLS hostname map)".** This ADR rejects it (Decision §8). Confirm
   the rejection. The specific objection is that the map is an
   `ip=hostname` SNI hint with no key material and is not derivable
   into a WireGuard identity, and that auto-provisioning undoes
   ADR-0113's operator trust prompt by granting a network identity
   at the same unexamined moment. If the intent was only "a Combs'
   tunnel should be easy to set up after it joins", the answer is the
   guided setup flow, not auto-provisioning.
6. **Is `full_tunnel: true` ever genuinely needed?** v1 refuses it
   on any Comb hosting a `NetworkDefinition`, which covers most real
   deployments. If no current deployment needs it, deleting it
   removes the ADR's worst footgun entirely. Worth deciding before
   implementation rather than after.
7. **Multiple tunnels per Comb.** v1 permits any number (one
   `wg<index>` each). Is there a practical cap worth enforcing, and
   should the admissibility check reject two tunnels whose own
   `address_cidr`s overlap on the same Comb?
8. **Cell reachability through a tunnel.** v1 lets a Cell's address
   be inside a tunnel's `AllowedIPs` (site-to-site) but refuses it
   a Cloudflare hostname (Decision §7). Should there be a first-class
   "this Cell is reachable at address X over tunnel Y" statement, or
   is inferring reachability from the addressing enough for v1? A
   first-class statement would be a network-management feature, not a
   VPN one.
9. **Multi-tenancy.** ADR-0127's open question 5, and the reason it
   appears here: a single operator-provisioned peer list does not
   express per-tenant key custody or revocation. If tenants arrive
   before this ships, the key-custody model in Decision §3 needs
   revisiting before the code does.

## References

- `internal/tlsdial/tlsdial.go` - the Colony's own client-side
  transport credentials; `ManagerDialOption`, and its `serverName`
  override comment
- `internal/cluster/peer.go` - `peerReporter`,
  `defaultPeerManagerdPort` (`127.0.0.1:17700`), the leader-forward
  rationale
- `internal/manager/peer.go` - `PeerReporter`'s dial path and its
  `peer_tls_hostname_map` consult (from
  `managerdConfig.PeerTLSHostnameMap`, `internal/manager/server.go`
  line 2562; parsed in `cmd/managerd/main.go` line 158)
- `internal/raftdconfig/manager.go` - `InternalToken`, `RaftTLSKey`,
  and the "root-owned, mode 0600" rule this ADR copies for WireGuard
  keys
- `internal/origincert/store.go` - the key-custody precedent,
  verbatim: "deliberately has no raft dependency"; its 0700
  directory and `writeTemp(..., 0o600)` key write
- `internal/nodeconfig/manager.go` - "never replicated through" raft
- `internal/cloudflare/tunnel.go` - `EnsureRunning`'s
  content-diff-plus-independent-liveness rule, `StopIfRunning`'s
  fixed-`RunDir` rule, `RenderConfig`'s self-validation and sorting,
  and its reference to the `SetVMCloudflareExposure` FSM boundary
- `internal/cloudflare/sidecar.go` - `ExposureRecord`, "persisted
  locally (never raft)", the removal-detection problem
- `internal/cloudflare/exec.go` - `runCmd`
- `internal/netif/interfaces.go` - `List()`, the interface inventory
  used as tunnel evidence and by the admissibility check
- `internal/pf/`, `internal/cluster/reconciler.go` (`pfManager`,
  `reconcileCloudflareTunnel`, `Cloudflare` at line 327) - the
  nil-able feature-field pattern and the existing tunnel apply loop
- `internal/raft/fsm.go` - `FSM`'s map fields, the `Command_*`
  dispatch, and `applySetVMCloudflareExposure` (line 383, the
  boundary this ADR adds a check to)
- `internal/raft/node.go` - `Status()`, `ServerInfo{ID, Address,
  Suffrage}`, the leader-gated/ungated read split
- `internal/manager/server.go` - `SetVMCloudflareExposure` (line 1700),
  the `ErrNotLeader` + `LeaderHint` response shape (e.g. line 1241)
- `api/internalpb/state.proto` - the "small, JSON-shaped facts"
  header rule; `VMDefinition`; `NetworkDefinition`;
  `PendingJoinRequest` field 8 (`tls_cert_fingerprint`); the
  `Command` oneof (field 16 in use)
- `api/rpc/manager.proto` - `service ManagerService` (line 11),
  `enum AssumptionKind` (line 2515, next free 6/7/8)
- `cmd/raftd/main.go` - the `-raft-tls-cert`/`-raft-tls-key`/
  `-raft-tls-ca` plumbing
- `internal/frontendconfig/manager.go` (lines ~151-184),
  `internal/manager/restartconfirm.go` (lines 38-65) - the atomic
  `0600` write sequence
- `internal/assumptions/manager.go` - `Status`, `Key`, `Result`,
  `ClampDetail`, and the refuse-on-wide-permissions precedent (line
  ~216)
- `internal/assumecheck/checker.go` - `checkNATUplink`, the
  `not_applicable` precedent
- [ADR-0022](0022-network-management.md) - network management; the
  bridge model that rules out a Cell mesh in v1
- [ADR-0033](0033-internal-transport-security.md) - internal
  transport security
- [ADR-0063](0063-cloudflare-tunnel-exposure-v1.md) - the existing
  tunnel feature this ADR learns from, including findings 4, 5 and 6
- [ADR-0078](0078-raft-transport-tls.md) - raft transport TLS; the
  existing mechanism a WireGuard transport would displace, and must
  not
- [ADR-0083](0083-mutually-authorized-colony-join.md) - the
  mutually authorized join flow
- [ADR-0103](0103-action-preflight-guardrails.md) and
  [ADR-0119](0119-replica-freshness-and-maintenance-wave-planner.md)
  - preflight guardrails and replica freshness; the recovery
  surfaces cited in the key-material argument
- [ADR-0113](0113-tls-trust-prompt-on-join.md) - the operator trust
  prompt this ADR refuses to undo with auto-provisioning
- [ADR-0114](0114-remove-uplink-takedown.md) - no out-of-band
  recovery channel; physical/console access is the answer
- [ADR-0115](0115-automatic-peer-tls-hostname-map.md) -
  `peer_tls_hostname_map`; an SNI hint, not a key-distribution channel
- [ADR-0118](0118-why-not-quorum-blocker-detail.md) - voter-by-voter
  detail; the honest-unknown rule
- [ADR-0056](0056-evidence-aware-health-v1.md) - evidence-aware
  health; `unknown` is neither healthy nor failed
- [ADR-0122](0122-cluster-evidence-aware-health-api.md) - the
  cluster-wide evidence API this feeds, and the verdict-chain change
  this ADR explicitly does not make
- [ADR-0127](0127-sylve-io-features.md) section 5 and Phase 3 - the
  originating description, and the four assumptions this ADR
  contradicts
- [ADR-0128](0128-guest-migration.md),
  [ADR-0129](0129-pf-firewall-management.md),
  [ADR-0130](0130-zfs-replication.md) - the Phase 3 siblings whose
  lockout-prevention, quorum and evidence designs this ADR is
  deliberately consistent with
- ADR-0133 - Certificate Management, the parallel Phase 3 sibling;
  the credential-class split is in Decision §8
- `SHARED.md` - Apiary architecture, pillars, and the update-lock
  protocol
