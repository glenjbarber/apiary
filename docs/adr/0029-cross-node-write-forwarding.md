# ADR-0029: Cross-node reconciler write forwarding

## Status

Accepted

## Context

ADR-0028's own live testing found a serious, previously-latent bug:
a node's own raftd only accepts an `Apply` when it is itself the
current raft leader. Every earlier live test in this project's history
happened to keep a VM/jail's owning node and the raft leader on the
same machine, so `internal/cluster`'s reconciler-initiated writes
(`UpdateVMPhase`/`UpdateJailPhase`, `PurgeVM`/`PurgeJail`) never
actually needed to succeed from a follower. `MigrateJail` was the
first real scenario where they diverged: the migrated jail's new owner
(`freebsd-apiary`) was not the raft leader (`apiarium`). ADR-0028
fixed the *visibility* of this (checking `ApplyResponse.Error`, not
just the transport error) but left the underlying gap open: nothing
gave the non-leader owner's reconciler an actual path to make the
write succeed.

This ADR closes that gap.

## Design: forward through ManagerService, not raftd's internal socket

The two candidate designs were:

1. Expose `RaftInternal.Apply` over the network (in addition to the
   existing Unix socket) and have a rejected local `Apply` retry
   against `LeaderHint`'s address.
2. Add narrow, purpose-specific RPCs to `ManagerService` - already
   networked, already optionally API-key-gated (ADR-0023) - and have
   a rejected local `Apply` forward to the *leader's own managerd*
   instead of trying to reach a peer's raftd directly.

Option 2 was chosen. `raftd`'s internal socket is deliberately
Unix-socket-only, unauthenticated, and scoped by file permissions -
CLAUDE.md already documents this as "judged sufficient for now"
specifically *because* it's local-only. Exposing it over the network
would widen that trust boundary for every command the FSM understands,
not just the two narrow write kinds that actually need cross-node
delivery. Reusing `ManagerService` instead adds no new trust surface:
it's the same external API every human, `restshimd` caller, and now
Terraform provider already talks to, with the same optional API-key
gate already built for it.

## Four new RPCs, one per write kind

`ManagerService` gains `ReportVMPhase`, `ReportVMTeardownComplete`,
`ReportJailPhase`, `ReportJailTeardownComplete` - deliberately narrow
(each does exactly one `Apply`, nothing else) rather than one generic
"submit a raw Command" RPC, which would have reintroduced exactly the
kind of open door raftd's own internal API was designed to avoid.
These are peer-to-peer RPCs (managerd calling managerd), not meant for
human/API-client use, but they sit on the same service and go through
the same `AuthUnaryInterceptor` as everything else - only `Status` is
exempted from auth (ADR-0023), and that exemption doesn't extend here.

## Reconciler-side: try locally first, forward only on a real "not leader" rejection

`internal/cluster`'s `applyPhase`/`applyJailPhase` (best-effort, as
before) and `teardownVM`'s `PurgeVM` submission/`purgeJail` (must
propagate failure, as ADR-0028 established) all follow the same
pattern: attempt the local `Raft.Apply` first; if it's rejected with
`ApplyResponse.LeaderHint` set (the existing, established signal that
a rejection specifically means "not the leader" - see `Server.Apply`),
and a new nil-able `Reconciler.Peers` field is configured, forward the
identical operation to the leader's managerd via a new `peerReporter`
interface instead. `Peers` being nil (or the leader lookup failing)
falls back to exactly ADR-0028's behavior - a visible, honest failure,
never a silent one.

`resolvePeerManagerdAddr` turns `LeaderHint` (the leader's *raft
transport* address, e.g. `10.50.0.14:17600`) into that node's managerd
address by keeping the host and substituting a configured/default
port (`Reconciler.PeerManagerdPort`, defaulting to managerd's own
`17700`). There's no separate node-address directory to consult here -
the raft leader hint is the only per-node address already on hand at
the exact point a write is rejected, and this project already makes
the same "every node uses the same port, just a different host"
assumption for HAST's own peer address resolution (`hast.go`).

`internal/manager.PeerReporter` is the concrete implementation
(dials fresh per call rather than caching a connection, since the
leader can change between calls - simplicity over an optimization
nothing here needs yet), wired into `cmd/managerd`'s `Reconciler.Peers`
unconditionally (harmless on a single-node deployment, where the local
node is always its own leader and these forwarding paths are simply
never exercised). A new `-peer-api-key` flag attaches an API key to
every peer call, mirroring `cmd/frontend`'s own `APIARY_MANAGER_API_KEY`
pattern - **required once the cluster has any API key created**
(ADR-0023), or peer calls start failing `Unauthenticated` instead of
the "not leader" rejection they're meant to route around. A new
`-peer-managerd-port` flag overrides the assumed peer port; it
defaults to this node's own `-rpc-addr` port.

## Consequences

- Full test coverage: reconciler tests with a fake `peerReporter`
  confirming a rejected purge is forwarded (and its address resolved
  correctly), and that a peer-forward failure still propagates as a
  real error; `internal/manager.PeerReporter` unit tests against a
  local gRPC server (request shape, API-key attachment, error
  propagation); integration tests exercising the four new RPCs' server
  handlers end-to-end against a real raft harness.
- Deliberately does **not** add general-purpose leader-forwarding for
  every RPC in this project - only the two write kinds the reconciler
  itself submits on its own initiative (phase reporting, final purge).
  Every human/external-API write (`CreateVM`, `DeleteVM`, etc.) still
  simply reports `leader_hint` back to its own caller and expects that
  caller to retry elsewhere, unchanged - that's a different, already-
  established pattern (every external RPC already does this), and
  extending it here would have been unrelated scope creep.
- **Live-verified on the real cluster**, repeating ADR-0028's exact
  migrate-then-delete sequence: a HAST-replicated jail was created
  owned by `apiarium` with a secondary on `freebsd-apiary`, migrated to
  `freebsd-apiary`, then deleted. The first attempt reproduced the
  exact ADR-0028 failure mode one layer further in: the reconciler
  correctly detected the "not leader" rejection, resolved `apiarium`'s
  host from `LeaderHint`, and attempted to forward - but `apiarium`'s
  managerd was still bound to `127.0.0.1:17700` (loopback-only, this
  project's existing default), so the peer connection was refused. On
  redeploying `apiarium`'s managerd bound to its real interface
  (`10.50.0.14:17700` - a real, explicit, user-approved exposure of a
  currently-unauthenticated write API, reverted immediately after the
  test), the very next reconcile tick on `freebsd-apiary` succeeded:
  the record purged automatically with **no `ForcePurgeJail` needed**,
  both machines confirmed back to a clean baseline (`jls`/`zfs list`
  empty on both, `timemachine` untouched). This also confirms the
  concrete deployment requirement this design implies: **every node's
  managerd `-rpc-addr` must be bound to a real, network-reachable
  interface, not loopback, for peer forwarding to work at all** - true
  regardless of whether API-key auth is enabled, and worth calling out
  explicitly since this project's own flag default (`127.0.0.1:17700`)
  does not satisfy it out of the box.
