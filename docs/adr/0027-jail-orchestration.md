# ADR-0027: Jail orchestration

## Status

Accepted

## Context

`internal/jail` has existed since early in the project as a standalone
`jail(8)`/`jls(8)` wrapper, but was never wired into anything: no
ephemeral state, no reconciler involvement, no RPCs. VMs got a full
create/reconcile/delete lifecycle (raft-replicated `VMDefinition`,
`internal/cluster`'s `Reconciler`, `ManagerService` RPCs, a web UI
page) years before jails got any of it. This ADR builds that same
lifecycle for jails, deliberately mirroring the VM path at every layer
rather than inventing a new shape, and reuses ADR-0026's HAST
machinery so a jail's root filesystem can be replicated the same way a
VM's disk can.

## Design: `JailDefinition` mirrors `VMDefinition`, deliberately minimal

`api/internalpb/state.proto` gains `JailState`/`JailPhase` enums
(identical shape to `VMState`/`VMPhase`) and `JailDefinition{id, name,
hostname, node_id, replica_node_id, desired_state, phase,
phase_error}` - no `vcpus`/`memory_mb`/`iso_name`/`network_id`/
`firewall_rules`. `internal/jail`'s own v1 scope is flat
`ip4=inherit` networking with no dedicated resource limits or attached
media, so there's nothing for those fields to configure yet; adding
them to the schema now, unused, would just be dead surface area. Five
new `Command` oneof variants (`CreateJail`/`UpdateJail`/`DeleteJail`/
`UpdateJailPhase`/`PurgeJail`, field numbers 10-14) and their message
types mirror `CreateVM`/`UpdateVM`/`DeleteVM`/`UpdateVMPhase`/`PurgeVM`
exactly, including `DeleteJail`'s soft-delete-when-`node_id`-is-set
semantics (`JAIL_STATE_DELETING`, copied from `VM_STATE_DELETING`'s own
reasoning in ADR-0016). `FSMSnapshotState` gains `map<string,
JailDefinition> jails = 6`.

`internal/raft/fsm.go`'s five new `applyCreateJail`/`applyUpdateJail`/
`applyDeleteJail`/`applyUpdateJailPhase`/`applyPurgeJail` methods are
close to line-for-line copies of their VM equivalents. `GetJail`/
`ListJails` (leader-only, mirroring `GetNetwork`/`ListNetworks`) and
`ListJailsLocal` (non-leader-restricted, mirroring `ListVMsLocal`/
`ListNetworksLocal` from ADR-0026 - `internal/cluster`'s Reconciler
runs on every node, not just the leader) round out the internal RPC
surface. The external `api/rpc/manager.proto` mirror and
`internal/manager`'s `applyJailCommand`/handlers follow the same
pattern used for `CreateNetwork`/`DeleteNetwork` earlier in the
project.

## Reconciler: same `Reconciler`, same `RunOnce`, a parallel jail pass

Jail provisioning was added to the *existing* `internal/cluster.Reconciler`
and its single `RunOnce` tick, not a separate reconciler type or a
second ticker - one pass over VMs, one pass over jails, sharing the
same raft-state fetch cadence and the same combined HAST role
reconciliation (see below). `Reconciler` gains three new nil-able
fields, all opt-in like every other reconciler dependency
(`Bhyve`/`VLAN`/`HAST`):

- `Jail jailManager` - wraps `*jail.Manager`; nil disables jail
  provisioning on this node entirely.
- `Mount mountManager` - wraps a new `*ufsmount.Manager` (below); only
  ever consulted for a *replicated* jail's root.
- `JailBase`/`JailDiskSizeMB` - where a replicated jail's root gets
  mounted, and how big its HAST-backed filesystem is. A non-replicated
  jail needs neither: its root is its own ZFS dataset's mountpoint,
  sized however that dataset already is.

`internal/cluster/plan.go` gains `JailPlacement` and
`PlanJail`/`PlanJailReclaim`/`PlanJailReplica`/`PlanJailReplicaReclaim`
- byte-for-byte the same logic as `VMPlacement`/`Plan`/`PlanReclaim`/
`PlanReplica`/`PlanReplicaReclaim`, including the same owner-vs-replica
mutual-exclusion guard `PlanReplicaReclaim`'s own doc comment explains
(ADR-0026 found this the hard way for VMs; jails get it correct from
the start by copying the fix, not by rediscovering the bug).

## Why a replicated jail needs a new package, `internal/ufsmount`

A replicated VM's disk is the raw HAST device itself - `bhyve.Config
.DiskPath` is an opaque path string handed straight to `-s N,ahci-hd,
<path>`, so `/dev/hast/vm-<id>` works exactly like a plain file with
zero extra work (ADR-0026). A jail is different: `jail(8)` needs a real
mounted filesystem tree to chroot into (`internal/jail.Config.Path`'s
own doc comment: "callers are expected to provide an already-populated
root"). A HAST device is a raw block device with no filesystem on it
at all, so a replicated jail's root needs `newfs(8)` once, then
`mount(8)` before `jail -c` can use it - work neither `internal/jail`
nor `internal/hast` do today, and stretching either package's stated
scope to cover it would blur what each one owns (`internal/jail` wraps
`jail(8)`/`jls(8)`; `internal/hast` wraps `hastctl`/renders
`hast.conf`).

`internal/ufsmount` is a new, small, single-purpose package -
`FormatIfNeeded`/`Mount`/`Unmount`/`IsMounted`/`IsFormatted`, wrapping
`newfs(8)`/`mount(8)`/`umount(8)`/`dumpfs(8)` - following this
project's established one-package-per-FreeBSD-concern pattern
(`internal/zfs`, `internal/jail`, `internal/hast`, `internal/bhyve`,
`internal/vlan`, `internal/dhcpd`, `internal/pf`). `FormatIfNeeded`'s
idempotency check matters for the same reason ADR-0026's root-cause bug
mattered for HAST resources: `newfs` has no built-in "already
formatted" guard of its own, and running it a second time would
silently wipe whatever's already on the device. `IsFormatted` checks
via `dumpfs(8)` (read-only - it just reads and prints the existing
superblock, never risking a write) rather than tracking formatted
state anywhere in the reconciler's own memory, so a `managerd` restart
doesn't lose track of what's already been formatted and never
re-formats a device with real data on it. `IsMounted` similarly reads
`mount(8)`'s own live listing rather than tracking state locally, for
the same "never trust in-memory state that can silently diverge from
reality" reason. Unit-tested with fakes in `internal/cluster` (no real
`newfs`/`mount` needed to test the reconciler's own logic); a real
integration test (`internal/ufsmount/integration_test.go`) exercises
the actual commands against a memory-backed `md(4)` device on FreeBSD,
needing no spare real disk - the same technique this project's own
live HAST debugging used earlier (ADR-0026) to isolate provider
behavior without touching real storage.

## HAST roles: VMs and jails share one `hast.conf`, one reconciliation call

`hast.conf` holds every HAST resource a node participates in at once
(VMs and jails together) - `RunOnce` builds a single combined `[]hastRole`
slice covering both before calling `reconcileHASTRoles` exactly once
per tick, rather than calling it once per resource type. Two separate
calls would each independently diff-and-possibly-rewrite the *same*
file and restart the *same* `hastd` process, with the second call's
config silently overwriting whatever the first one just wrote - not a
correctness bug in the exact sense ADR-0026's bugs were, but the same
class of mistake (treating a node-wide resource as if it had
per-caller state). `jailHASTResourceName` (`"jail-" + id`) is `hastRole
.resourceName`'s jail-side counterpart to `vmHASTResourceName`
(`"vm-" + id`) - the two namespaces can never collide even if a VM and
a jail happen to share an id, since the resource name always carries
which kind it is.

## Consequences

- Full test coverage at every layer, mirroring the VM test suite
  exactly: FSM CRUD + snapshot/restore (`internal/raft/fsm_test.go`),
  a real raft-harness integration test exercising the actual
  `CreateJail`/`UpdateJail`/`DeleteJail`/`GetJail`/`ListJails` RPCs
  (`internal/manager/integration_test.go`), reconciler tests with fake
  `jailManager`/`mountManager` covering plain and HAST-replicated
  create/skip-already-running/delete/reclaim paths plus both
  "no HAST configured"/"no Mount configured" error paths
  (`internal/cluster/jail_test.go`), and frontend handler tests for
  `/jails` (`internal/frontend/server_test.go`).
- `cmd/managerd` gains `-jail-enabled`/`-jail-prefix`/
  `-jail-mount-base`/`-jail-disk-size-mb`, all following the existing
  "empty/false disables this subsystem" convention. Jail orchestration
  is independent of both bhyve and HAST: a node can run plain,
  non-replicated jails with neither enabled.
- The same secondary-orphan-on-full-purge limitation ADR-0026 accepted
  for VMs applies identically to replicated jails: nothing tears down
  a jail's secondary-role HAST resource on the replica node if the
  owning node is permanently gone and the record is force-purged from
  elsewhere. Same reasoning: never infer teardown from an absent
  record without a forced/orphan-reclaim path, which doesn't exist yet
  for either VMs or jails.
- No live verification against the project's real FreeBSD machines has
  been done for this feature yet (unlike ADR-0026, which was verified
  end-to-end on real hardware) - this ADR covers the implementation as
  built and unit/integration-tested, not as live-verified. Anyone
  deploying this for real should expect to find and fix at least one
  surprise the way ADR-0026's own live-debugging trail did; nothing
  about jails' `newfs`/`mount` step has been exercised against a real
  `hastd`-backed device yet, only against a local `md(4)` device in
  `internal/ufsmount`'s own integration test.
