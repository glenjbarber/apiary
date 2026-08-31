# ADR-0028: Manual MigrateVM/MigrateJail

## Status

Accepted

## Context

CLAUDE.md has long named a real gap: "nothing decides which node a
VM's `node_id` should be beyond whatever a caller sets directly, and
`MigrateVM` doesn't exist." This project has an equally long-standing,
deliberate stance against *automatic* scheduling (`node_id`/
`replica_node_id` are always caller-set, never computed). The request
here was specifically to close the "there's no way to move a VM/jail
at all" gap without reversing that stance - a manual, explicit,
human-triggered operation, not a scheduler.

## The naive version would silently destroy data

The obvious implementation - `MigrateVM(id, target_node_id)` just
overwrites `node_id` and lets the existing machinery run - already
technically "works" without any new code: `UpdateVM` already lets a
caller change `node_id` outright, and ADR-0025's resource reclaim
already tears down the old node's now-orphaned dataset/bhyve VM
automatically. So in one narrow sense `MigrateVM` was never truly
*impossible* - `UpdateVM` already provided the mechanism.

But that mechanism is dangerous for exactly the VMs/jails most worth
migrating: an **unreplicated** one. The target node has never seen
this VM's disk at all - `ensureVM`/`ensureJail` on the new owner
creates a **brand-new, empty** dataset/disk, while the reconciler on
the *old* node (now correctly seeing itself as no longer the owner)
destroys the real one via `reclaimStaleVM`/`reclaimStaleJail`. The net
effect: real, silent data loss, dressed up as a successful RPC call.
This project has a long track record (ADR-0016, ADR-0025, ADR-0026,
ADR-0027) of refusing to ship a default that looks like it works but
quietly destroys real data - the same caution applies here.

## Design: MigrateVM only promotes an existing, already-synced replica

`MigrateVM`/`MigrateJail` succeed **only when `target_node_id` already
equals the VM/jail's current `replica_node_id`** - i.e., a HAST
secondary that Apiary itself already believes holds a synced copy of
the disk (ADR-0026/ADR-0027). Any other target is rejected outright
with a clear, actionable error explaining the two-step path: set
`replica_node_id` via `UpdateVM`/`UpdateJail` first, confirm real
replication has reached `hastctl`'s own `status: complete` on the
target (a human check - Apiary has no cross-node RPC to verify this
itself, by design), *then* call Migrate.

On success, the RPC swaps `node_id` and `replica_node_id` (the old
owner becomes the new secondary, continuing replication back) and
submits the result as a normal `UpdateVM`/`UpdateJail` command -
**no new reconciler logic at all**. The existing, already-tested
per-node HAST role machinery (`reconcileHASTRoles`, `PlanReplica`/
`PlanReplicaReclaim` and their jail equivalents) converges this
exactly like any other role change:

- The new owner's reconciler sees itself as primary for this
  resource, calls `SetRole(primary)` on its already-synced provider
  (safe - a HAST secondary already holds a full copy, promoting is a
  role change, not a resync), and - since it's never run this VM/jail
  before - `ensureVM`/`ensureJail` starts it for the first time
  against that now-primary device.
- The old owner's reconciler sees `node_id` no longer matches itself,
  destroys its own bhyve VM/jail process (`reclaimStaleVM`/
  `reclaimStaleJail`), and - because it's now named in
  `replica_node_id` - correctly *keeps* the HAST resource instead of
  destroying it, converging to `SetRole(secondary)` instead.

This is real, caller-triggered HAST failover, built entirely on
machinery ADR-0026/ADR-0027 already proved live - not new reconciler
surface area to re-verify from scratch.

`MigrateVM`/`MigrateJail` also reject a VM/jail already marked for
deletion, and a request naming the VM/jail's current `node_id` as the
target (a no-op, reported as an error rather than silently succeeding).
Every other field (name, vcpus, memory, ISO, network, firewall rules)
is preserved exactly via `proto.Clone` plus a two-field mutation - the
caller only ever needs to name the target node, never re-supply the
whole definition (unlike a raw `UpdateVM` call, which requires the
full definition and would silently wipe any field the caller forgot
to re-include).

## REST surface

`internal/restshim` gains `POST /v1/vms/{id}/migrate` and
`POST /v1/jails/{id}/migrate`, taking `{"target_node_id": "..."}` -
mirroring every other RPC's REST translation in that package.

## Live verification found a serious, previously-latent bug - and a serious, still-open gap

`MigrateVM` itself was not live-verified: only `apiarium` in this
project can run bhyve VMs at all (ADR-0010/ADR-0015), so there is no
second bhyve-capable node to migrate a VM *to* in this project's real
environment. `MigrateVM`'s correctness rests on integration tests plus
sharing 100% of its reconciler-side logic with the now-live-verified
`MigrateJail` path below, not on its own live run.

`MigrateJail` **was** live-verified for real on the project's actual
2-node cluster: created a HAST-replicated jail owned by `apiarium`
with a synced secondary on `freebsd-apiary` (confirmed `hastctl`
`role: primary`/`status: complete` and `role: secondary`/
`status: complete`), called `MigrateJail(id, "freebsd-apiary")`, and
watched the swap converge for real - `apiarium` tore down its own
running jail and demoted to `role: secondary`/`status: complete`;
`freebsd-apiary` promoted to `role: primary`/`status: complete` and
started running the real jail for the first time. Exactly the failover
this ADR's design section predicted, with no new reconciler code
involved.

**Deleting that migrated jail surfaced a serious, previously-latent
bug**, invisible until this exact scenario: `freebsd-apiary` had
become the jail's owner, but `apiarium` remained the raft *leader* -
the first time in this project's history that a VM/jail's owning node
and the raft leader were different nodes (every earlier live test kept
them the same). `internal/cluster`'s own `purgeJail`/`teardownVM`
(and the jail equivalent) submit their `PurgeJail`/`PurgeVM` commands
via `Raft.Apply` against *that node's own local raftd* - which only
accepts an `Apply` when it is itself the current leader. On a
follower, the gRPC call to the local raftd still **succeeds** (no
transport error) - the rejection ("this node is not the leader")
comes back as a normal, non-error `ApplyResponse.Error` field instead.
`purgeJail`/`teardownVM`'s purge step checked only the transport
error, never `ApplyResponse.Error`, so a rejected purge was silently
treated as a success: the real jail and its dataset were correctly
destroyed, but the raft tombstone was never removed, and the
reconciler kept reporting a clean `RunOnce` every tick afterward with
no visible sign anything was wrong. The stale tombstone was only
discovered by manually cross-referencing `ListJails` against real
`jls`/`zfs list` output on the machines themselves - the same
in-cluster observation ADR-0025/ADR-0026/ADR-0027's own bugs were
found this way, this time discovered from an entirely new axis
(leadership vs. ownership) that had simply never diverged before.

**Fixed the visible half**: `purgeJail`/`teardownVM`'s purge step (and
`ForcePurgeVM`/`ForcePurgeJail`, which already checked `ApplyResponse
.Error` correctly, unaffected) now check `ApplyResponse.Error` in
addition to the transport error, so a rejected purge is surfaced as a
real reconcile error every tick instead of silently vanishing.
Regression tests added for both VM and jail. The stuck live tombstone
was cleaned up with `ForcePurgeJail` once the real physical teardown
was confirmed already complete.

**The underlying gap is NOT fixed, and is significant**: nothing gives
a non-leader node's reconciler any way to actually get an `Apply` to
succeed for a resource it owns. `internal/raft`'s `RaftInternal` gRPC
service is deliberately Unix-socket-only, per-node (see CLAUDE.md's
"no authentication on raftd's internal socket... judged sufficient for
now" stance) - there is no network path today for one node's managerd
to reach a *different* node's raftd to retry against the real leader,
and hashicorp/raft's own `Raft.Apply` has no built-in forwarding-to-
leader behavior. Concretely: **any VM or jail owned by a node that is
not also the current raft leader can have its real, physical resources
correctly provisioned/torn down, but can never have its phase updates
or final delete-purge actually reach raft state** - the visibility fix
above means this now shows up as a logged, repeating reconcile error
instead of silent success, but it does not make the write succeed.
Before this ADR, this was untested and assumed to work simply because
every earlier live test happened to keep the owner and the leader on
the same node. Properly fixing this needs real new infrastructure
(most plausibly: exposing `RaftInternal.Apply` over the network as
well as the Unix socket, and having a rejected local `Apply` retry
against `LeaderHint`'s address) - real, separate work, tracked here as
a genuine, now-known correctness gap rather than a hypothetical one.

## Consequences

- Full integration test coverage on a real raft harness: successful
  migration (fields preserved, node/replica swapped), rejection when
  no replica exists at the target, rejection when the replica is at a
  *different* node than the target, rejection for a VM/jail already
  marked for deletion, and not-found. Plus the two new regression
  tests for the rejected-purge bug above.
- This still doesn't provide a path for migrating a VM/jail that was
  never replicated in the first place - that gap is deliberate, not
  an oversight: any such path would need to actually move the disk
  bytes themselves (a real transfer, not a config change), which is
  meaningfully different work and out of scope here.
- The non-leader-Apply gap above is the most significant finding in
  this ADR and applies far beyond `MigrateVM`/`MigrateJail` themselves
  - it affects phase reporting and delete-purge for *any* VM/jail
  whose owning node isn't the current raft leader, for any reason
  (including a plain leadership election following a leader restart
  or network blip, with no migration involved at all). Tracked in
  CLAUDE.md's "What's not implemented yet" as a real, open item.
