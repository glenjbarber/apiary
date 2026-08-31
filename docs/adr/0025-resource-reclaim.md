# ADR-0025: Resource reclaim for reassigned and stuck-deleting VMs

## Status

Accepted

## Context

CLAUDE.md's "What's not implemented yet" named two real gaps around
resource lifecycle, both flagged as deliberate, accepted limitations
rather than oversights:

1. **Reassignment leak**: nothing tears down a VM's old node's dataset/
   bhyve VM when its `node_id` is reassigned to a different node.
   `internal/cluster.Plan`'s own doc comment explains why this was left
   alone rather than guessed at: a VM disappearing from a node's own
   filtered view isn't safely distinguishable from "reassigned" versus
   "the fetch simply failed," and treating absence as a teardown signal
   is exactly the class of mistake `Plan` was designed to avoid.
2. **Stuck tombstones**: a VM marked `VM_STATE_DELETING` (ADR-0016)
   whose owning node never comes back online stays tombstoned forever -
   no forced/orphan-reclaim path existed.

Both are now addressed, with designs that stay inside the caution
`Plan`'s doc comment already established: act only on an unambiguous,
caller-originated fact, never on an inferred absence.

## Reassignment reclaim: safe because the record still exists

The key realization: a *reassigned* VM is not "absent" the way `Plan`'s
comment warns about - its record still exists in the list, just with a
different `node_id` now. That's a real, explicit, caller-originated
fact (the node_id was actually written by an `UpdateVM` call), not a
guess about *why* something disappeared. `internal/cluster/plan.go`
gains `PlanReclaim(desired []VMPlacement, localNodeID string) []string`,
returning every VM ID in `desired` whose `NodeID` is *not*
`localNodeID` - candidates whose local resources (if any exist under
that ID) are now orphaned on this node specifically.

`Reconciler.RunOnce` runs `PlanReclaim` after its normal `Plan`-driven
create/teardown pass and calls a new `reclaimStaleVM(ctx, id)` for each
candidate: it checks (via the existing `Bhyve.VMExists`/
`ZFS.DatasetExists`) whether *this* node still has resources under that
ID and destroys them if so, exactly mirroring `teardownVM`'s existence-
check-then-destroy pattern. Critically, **it never touches the raft
record itself or submits `PurgeVM`** - the record legitimately belongs
to whichever node `node_id` now names; this node's only job is
cleaning up its own leftovers. For the overwhelming common case (a VM
that never touched this node in the first place), both existence checks
come back negative and the whole pass is a cheap no-op - no new
per-tick cost for nodes that were never involved.

## Stuck tombstones: a narrow, explicit admin escape hatch, not automatic detection

Automatically deciding "the owning node is dead, not just slow" is a
real distributed-systems problem (this project isn't attempting node
liveness detection anywhere else, and doing so here would need failure
detectors, timeouts, and false-positive tradeoffs well beyond this
project's current scope). Instead: a new `ForcePurgeVM` RPC on
`ManagerService` is a deliberate, human-triggered escape hatch. It only
succeeds against a VM already in `VM_STATE_DELETING` - GetVM's
`desired_state` is checked first, so a live/running VM can't be
force-purged by mistake and silently orphan real resources on a node
that's actually still up - then submits the exact same `PurgeVM`
command a reconciler would (that FSM op was already unconditional and
idempotent, so no new FSM logic was needed). Its own proto doc comment
is explicit that it does **not** reach the (by definition unreachable)
owning node to clean up anything physical there - using it means
accepting that those resources, if the node ever does come back, are
now untracked.

This does mean `PurgeVM` is no longer strictly "reconciler-only,
never issued by managerd's RPC-facing Server" as CLAUDE.md previously
described it - `ForcePurgeVM` is a second, narrow, explicitly-named
caller of the same underlying operation, gated by the deletion-state
check above rather than by which caller can reach it.

## Consequences

- Verified with unit tests: `internal/cluster/plan_test.go`'s
  `TestPlanReclaim` (mirrors `TestPlan`'s table-test shape);
  `internal/cluster/reconciler_test.go`'s
  `TestReconciler_RunOnce_ReclaimsResourcesForVMReassignedElsewhere`
  (destroys both bhyve VM and dataset, confirms the raft record is
  *not* purged), `..._ReclaimIsNoOpWhenNothingLocalExists`, and
  `..._ReclaimPropagatesDestroyError`; `internal/manager`'s
  `TestIntegration_ForcePurgeVM_RequiresDeletingState` (live vs.
  deleting rejection, then success) and
  `..._MissingIsError`, against a real raft harness.
- Verified live end-to-end on `apiarium`: created a real VM
  (`reclaim-test-1`), confirmed the reconciler produced a real ZFS
  dataset and a real running bhyve VM within one tick; reassigned its
  `node_id` to a nonexistent node and confirmed the *next* tick
  destroyed both the real bhyve VM (`/dev/vmm/apiary-reclaim-test-1`
  gone) and the real dataset (`zroot/apiary/reclaim-test-1` gone)
  while leaving the raft record itself intact and owned by the new
  (fake) node; then soft-deleted that same record via `DeleteVM`
  (which tombstones rather than removes outright, since `node_id` was
  set - ADR-0016), confirmed `ForcePurgeVM` rejected it before that
  point, and confirmed it succeeded and the record was gone (`GetVM`
  returning `found: false`) afterward.
- `apiarium` was left with zero VMs and zero API keys after
  verification, matching its state before this work.
- Real, automatic node-liveness-based reclaim (deciding on its own
  that a node is gone, rather than requiring a human to invoke
  `ForcePurgeVM`) remains explicitly out of scope - the same caution
  CLAUDE.md already applied to not inferring teardown from absence.
