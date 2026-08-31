# ADR-0026: Real HAST-backed VM disk replication

## Status

Accepted

## Context

All four project machines (`freebsd-apiary`, `freebsd-apiary2`,
`freebsd-apiary3` - VMs, no nested-virt, can't run bhyve - and
`apiarium` - real hardware, the only bhyve-capable node) run a patched
`hastd` (ADR-0008/D57511) confirmed to actually replicate. This wires
that into real VM disk provisioning: a `VMDefinition` can name a
`replica_node_id`, and its disk is then HAST-replicated to that node
for data redundancy - explicitly **not** automatic failover, since
only `apiarium` can run a bhyve VM at all. A HAST secondary is
provisioned and left in `secondary` role, silently replicating, never
mounted or used - that's a deliberate scope boundary carried through
from the initial design, not something this pass changed.

This ADR exists mainly to record what real end-to-end testing against
actual `hastd` on real hardware found - the design changed twice, and
one genuinely subtle root-cause bug took a long live-debugging session
to isolate. Anyone touching this code should read the whole trail
before assuming the current shape is arbitrary.

## Design: caller-explicit `replica_node_id`, per-node role reconciliation

`VMDefinition` gains `replica_node_id` (both `api/internalpb/state.proto`
and `api/rpc/manager.proto`), caller-set exactly like `node_id` - this
project has an established, deliberate stance against automatic node
scheduling (CLAUDE.md), and extending that to replica assignment keeps
the feature scoped to replication mechanics rather than quietly
building a scheduler as a side effect. "Spread across all three [VM
nodes]" is an *operational* pattern (pick a different `replica_node_id`
per VM), not new FSM logic.

Every node reconciles only its own local role from shared raft state -
no cross-node RPC or remote exec anywhere in `internal/cluster/hast.go`.
`internal/cluster/plan.go` gains `PlanReplica`/`PlanReplicaReclaim`,
mirroring `Plan`/`PlanReclaim` exactly but against `ReplicaNodeID`
instead of `NodeID`. `hast.conf`'s `Node.Name` values are just
`node_id`/`replica_node_id` directly (this project's node IDs already
equal real hostnames), and `Node.Remote` addresses are resolved from
raft's own cluster membership (a new non-leader-restricted
`RaftInternal.ListVMsLocal`/`ListNetworksLocal` pair, see below) via
`Reconciler.resolvePeerAddresses` - no new node-address config needed.

## First design decision that changed: zvols don't work as HAST providers here

The original plan used a dedicated **zvol** as each resource's local
GEOM provider, since no project host has a spare raw disk/partition.
Live testing on `apiarium` (FreeBSD 16.0-CURRENT) found this simply
doesn't work: `hastd`'s primary-role worker consistently failed with
`Unable to read metadata from /dev/zvol/... : No such file or
directory` and the role never actually took effect, no matter how long
it was given to settle. Isolated with a plain `md(4)` vnode device
(`mdconfig -a -t vnode -f <file>`) - that worked immediately - and then
with a **plain file** pointed at directly in `hast.conf`'s `local`
directive, with no device-allocation layer at all - that also worked
immediately, and is simpler (no `mdconfig` unit to track/persist across
ticks or restarts).

The fix: `ensureHASTProvider` creates a dedicated ZFS **dataset**
(`hast-<resource>`, a plain `CreateDataset`, not a zvol) and a sparse
file inside its mountpoint (`backing.img`, sized like a normal VM disk
- see `Reconciler.diskSizeMB`), then points `hast.conf` straight at
that file's path. `bhyve.Config.DiskPath` is a fully opaque string
concatenated into `-s N,ahci-hd,<path>` with zero validation - a HAST
device path (`/dev/hast/vm-<id>`) works exactly like a plain file
there, so a replicated VM's disk skips the dataset-plus-file path
entirely and uses the HAST device as its whole disk, no filesystem
needed. `internal/zfs` needed no new capability for this (`CreateDataset`/
`DatasetExists`/`GetProperty`/`DestroyDataset` already existed) - the
`CreateZvol`/`FullPath` methods added for the original zvol design were
removed again once it was abandoned.

## Second finding: a freshly-truncated file needs an explicit `Sync`

Even after switching to files, an immediately-following `hastd` open
occasionally still failed to read metadata near the end of a
just-`Truncate`'d file, despite `ls` already showing the correct size.
`ensureHASTProvider` calls `f.Sync()` after `Truncate` to force the
extended size fully committed before anything else touches the file.

## Third finding: `hastd` needs a few seconds to restart, and retrying fast makes it *worse*

Restarting `hastd` (`RestartService`, see below) and immediately
attempting a role change hit a worker that hadn't finished
(re)spawning yet, failing with hastd's own "Error 57" (`ENOTCONN`).
The fix that actually worked was an **upfront wait** after a real
restart (`Reconciler.hastRestartSettleDelay`, defaulting to 3s,
overridable per-`Reconciler` so tests aren't slowed down) - not a
tight retry loop. A tight loop (5 attempts, 1s apart) was tried first
and made things reliably *worse*: a resource that failed its first
rapid attempt kept failing every subsequent retry and every later tick
too, while an identical resource that had never been attempted before
succeeded first-try after the same upfront wait. That pointed at each
failed attempt leaving `hastd`'s per-resource on-disk state worse than
it started, not at the underlying cause being transient - so
`ensureHASTResourceAndRole` makes exactly one attempt per tick, the
same "try once, let convergence happen across ticks" philosophy every
other operation in this reconciler already follows.

## The actual root cause: `hastctl list` succeeds whether or not the resource was ever `create`d

The failure above ("Error 57", worker crashes, metadata errors) turned
out to be a downstream symptom of a single, more fundamental bug, only
found after eliminating every other variable (exec mechanism, file
creation method, `daemon(8)` detachment, timing, and even a leftover
second test VM being reconciled at the same time - all ruled out one
at a time by reproducing pieces of the pipeline standalone until the
real difference was isolated). Confirmed directly: `hastctl list
<name>` returns a normal, successful `role: init` the moment a
resource is merely *defined in hast.conf and loaded by a running
hastd* - **whether or not `hastctl create <name>` has ever actually
been run against it.**

`ensureHASTResourceAndRole`'s original logic used exactly that command
(`Status`, this package's only status-reading primitive) as an
idempotency guard: "if `Status` succeeds, this resource must already
be created - skip `CreateResource`." Since `Status` always succeeds
once the resource is configured, that guard always evaluated true and
`CreateResource` was **never called at all**, on any resource, ever -
the resource's on-disk metadata block was never initialized, and the
following `SetRole(primary)` failed with "Unable to read metadata"
every single time, on every VM tested, regardless of provider type or
timing. Every earlier finding in this ADR was real and worth fixing on
its own merits, but none of them were the reason role changes kept
failing - this was.

The fix: call `CreateResource` **unconditionally**, every tick, no
guard at all. Confirmed live that a second `hastctl create` against an
already-created resource exits 0 with no error - it's idempotent in
practice even though ADR-0008 already noted it isn't formally
documented as such.

## `internal/raft`: `ListVMsLocal`/`ListNetworksLocal`, a second exception to the leader-only read convention

Getting a real second raft node's `Reconciler.RunOnce` to run at all
surfaced a genuine, previously-latent architectural gap: `ListVMs`/
`ListNetworks` (the RPCs the reconciler used to read state) are
leader-only by design (v1's "read consistency" choice, same as
`GetVM`/`GetNetwork`). Every deployment before this one had exactly
one raft node, always the leader, so this never actually mattered -
the first time a genuine non-leader node's reconciler tried to read
the VM list, it failed outright with "this node is not the leader."

Fixed the same way ADR-0023's `ValidateAPIKeyHash` fixed the analogous
problem for auth: two new, deliberately non-leader-restricted internal
RPCs, `ListVMsLocal`/`ListNetworksLocal`, reading each node's own FSM
state directly (`Node.ListVMsLocal`/`ListNetworksLocal`, no
`raft.State() != raft.Leader` guard). `internal/cluster`'s local
`raftClient` interface now declares these instead of the leader-only
ones; the *external* `GetVM`/`ListVMs` RPCs on `ManagerService` are
untouched and remain leader-only, since that's a different, deliberate
v1 choice for external reads specifically.

## Two more real bugs, both a version of the same mistake

Live testing (a real 2-node raft cluster, `apiarium` primary +
`freebsd-apiary` secondary, joined via an SSH-forwarded Unix socket
since `raftd -join` dials a local socket path) caught two further
bugs, both "a role's own resources got destroyed by a *different*
reclaim pass that didn't know this role was legitimate":

- **Primary's own zvol destroyed by the replica-reclaim pass.**
  `PlanReplicaReclaim`'s naive `ReplicaNodeID != localNodeID` check is
  also true for the *owner* of a VM (a node is never simultaneously
  primary and secondary for the same VM) - so a node's own
  just-created primary-role resource looked exactly like a stale
  secondary to reclaim, and got destroyed the same tick it was
  created. Fixed by skipping any VM this node owns (`NodeID ==
  localNodeID`) in `PlanReplicaReclaim` entirely.
- **Secondary's own zvol destroyed by the owner-reclaim pass**, the
  mirror image: `PlanReclaim`'s `NodeID != localNodeID` check is
  naturally true for every VM a node is merely *replicating* (it's
  never the owner), so `reclaimStaleVM`'s HAST cleanup step destroyed
  the legitimate, just-created secondary resource. Fixed by passing
  the current tick's `PlanReplica` result into `reclaimStaleVM` as an
  `isCurrentReplica` flag, skipping the HAST reclaim step when true.
- **A `Deleting` replicated VM was also included in the "ensure
  primary" role list**, racing its own teardown within the same tick:
  the ensure step (re)created the dataset the teardown step was
  simultaneously destroying. Fixed by excluding `Deleting` VMs from
  the primary-role ensure list - `teardownVM`'s own `reclaimHASTRole`
  call already handles that case.

## Consequences

- Verified with unit tests throughout `internal/cluster` (fakes for
  every manager interface, including a new `fakeHASTManager`):
  `TestPlanReplica`/`TestPlanReplicaReclaim` (including the two
  regression cases above), and `Reconciler.RunOnce` tests covering
  primary provisioning, secondary provisioning, both reclaim-survives
  regressions, the deleting-VM-not-also-ensured regression, teardown
  reclaiming HAST instead of a dataset, and the no-HAST-configured
  error path.
- Verified live, for real, end-to-end on `apiarium` + `freebsd-apiary`
  (a genuine 2-node raft cluster, not a simulated one): created a real
  VM naming `replica_node_id`, watched a real dataset+file appear as
  primary on `apiarium` and as secondary on `freebsd-apiary`, watched
  `hastctl list` report **`role: primary`/`status: complete`** on
  `apiarium` and **`role: secondary`/`status: complete`** on
  `freebsd-apiary` (not just `degraded` - real, active, synced
  replication), and watched a real bhyve VM boot with its disk backed
  by `/dev/hast/vm-<id>`. Deleted the VM and confirmed the owner's own
  resources (bhyve VM, dataset+file, raft record) were fully torn down.
- **Known limitation found during the same verification, not yet
  addressed**: once a VM's record is fully purged, the *secondary*
  node has no signal left to clean up its own replica - the VM is
  gone from every node's `desired` list at that point, and (per
  `Plan`'s own long-established doc comment) this project deliberately
  never infers teardown from a record's absence. The secondary's
  dataset+file becomes a genuinely orphaned resource in that case,
  the same class of accepted limitation already documented for a
  tombstoned VM whose owning node never returns (ADR-0025's
  `ForcePurgeVM` doesn't help here either, since there's no record
  left to force-purge). Confirmed live: after a full delete/purge
  cycle, `freebsd-apiary` still had the dataset+file behind. Cleaned
  up manually for this verification; a real fix would need some kind
  of two-phase purge (don't fully purge until the secondary also
  confirms cleanup) or an explicit admin-facing "clean up this node's
  orphaned HAST resources" operation - out of scope for this pass.
- `apiarium` and `freebsd-apiary` were left with zero VMs and a clean
  `hast.conf` after verification, joined as a real 2-node raft cluster
  (previously `apiarium` ran alone) with `hastd` enabled and ready for
  real use on both. `freebsd-apiary2`/`freebsd-apiary3` were not
  joined or exercised in this pass - the "spread across all three"
  operational pattern is proven mechanically (via `freebsd-apiary`)
  but not yet demonstrated against the other two specifically.
- `-hast-enabled` is a new, explicit opt-in `managerd` flag (deviating
  slightly from the original plan's "no separate flag needed, HAST
  turns on automatically" - on reflection, matching every other
  physical-side-effect capability in this project by requiring an
  explicit flag was judged safer, since leaving it always-on would
  mean writing an empty `/etc/hast.conf` on every node regardless of
  whether it ever intends to use HAST at all). Needed on both a
  replicated VM's owning node and its replica node - it's independent
  of `-bhyve-bootrom`, since a pure secondary never runs bhyve.
- Automatic node-liveness-based failover remains explicitly out of
  scope, as originally scoped: this is data redundancy, not HA - only
  `apiarium` can run bhyve VMs at all today.
