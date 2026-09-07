# ADR-0073: Orphaned HAST resource discovery and cleanup

## Status

Accepted

## Context

ADR-0026 (real HAST-backed VM disk replication) named a limitation it
deliberately left open: once a replicated VM or jail's raft record is
fully purged, the *secondary* node's own local HAST provider dataset
has no signal left to clean itself up. `internal/cluster/plan.go`'s
`PlanReplicaReclaim`/`PlanJailReplicaReclaim` already handle the
*reassignment* case (a VM/jail whose `replica_node_id` moved away from
this node while the record still exists) - but a record that's gone
entirely disappears from every list this reconciler ever sees, and
this project deliberately never infers teardown from a record's mere
absence (the same principle `Plan`'s own doc comment has established
since ADR-0012). ADR-0026 named the fix this ADR builds: "an explicit
admin-facing clean up this node's orphaned HAST resources operation."

This is genuinely a physical, per-node gap, not a raft/consensus one:
the leftover state is a real ZFS dataset (`hast-vm-<id>`/
`hast-jail-<id>`, see `internal/cluster/hast.go`'s
`hastProviderDatasetName`) holding real, no-longer-wanted replicated
bytes on the node that was the *secondary* for a resource whose record
is gone.

## Decision

Two new local-only `ManagerService` RPCs, never routed through raft
(physical, per-node data, the same locality convention `HostStats`/
`ListISOs` already established):

- **`ListOrphanedHASTResources`** (Viewer) scans this node's own ZFS
  datasets (`internal/zfs.Manager.ListDatasets`) for names matching
  the `hast-vm-*`/`hast-jail-*` provider convention, then
  cross-references each bare id against the current raft-replicated
  VM/jail list (`RaftClient.ListVMsLocal`/`ListJailsLocal`, checking
  **both** `node_id` and `replica_node_id` against this node's own
  id) - anything with no match at all, in either role, is reported as
  orphaned, along with its dataset's `used` ZFS property as
  best-effort operator context.
- **`CleanupOrphanedHASTResource`** (Admin) is the explicit,
  human-triggered destructive half - re-verifies the resource is
  *still* orphaned at the moment of the call (never trusts an
  earlier `List` result), then destroys the dataset directly via
  `internal/zfs.Manager.DestroyDataset`. No `hast.conf`/`hastd`
  interaction is needed at cleanup time: a resource that's truly
  orphaned already dropped out of the *next* `reconcileHASTRoles`
  tick's rendered config on its own (built strictly from the current
  desired list), including the restart that releases `hastd`'s
  worker for it - by the time an operator notices the orphan at all,
  nothing is holding the file open any more, so a plain dataset
  destroy is sufficient and safe.

Mirrors `ForcePurgeVM`/`ForcePurgeJail`'s own "an operator decides, the
system never guesses" posture exactly, including the Admin tier (a
human-triggered override of what would otherwise require inferring
teardown from absence, deliberately not something Operator can do
unilaterally) and the re-verify-before-destroying discipline.

**No web UI** for either RPC, matching `ForcePurgeVM`/`ForcePurgeJail`'s
own precedent - neither of those has a UI button either, despite both
existing for over a session's worth of prior work. This class of
rare, destructive, human-judgment-driven action stays a direct RPC
call in this project, not a page dedicated to a decision an operator
should be making deliberately and infrequently, not from a routine
UI workflow.

## Naming convention duplicated, not shared

`hastOrphanResourceType`/`hastOrphanDatasetName`
(`internal/manager/server.go`) re-derive the same
`"hast-" + ("vm"|"jail") + "-" + id` convention
`internal/cluster/hast.go`'s `hastProviderDatasetName`/
`vmHASTResourceName`/`jailHASTResourceName` already define, rather than
exporting a shared helper across the package boundary - it's a
two-line string format, not shared logic, and `internal/manager` has
no other reason to import `internal/cluster`.

## Role gating

- `ListOrphanedHASTResources`: Viewer - a read-only local report, same
  tier as `HostStats`/`ListISOs`/`GetVMSerialLog`.
- `CleanupOrphanedHASTResource`: Admin - matches `ForcePurgeVM`/
  `ForcePurgeJail`'s own tier for the same reasoning (a human-triggered
  override of normal reconciliation, not routine Operator lifecycle
  work).

## Not addressed

- No automatic/periodic scan or alerting for orphaned resources - an
  operator must think to check. A future Automated Assumption Check
  (ADR-0055) surfacing "this node has N orphaned HAST resources" as a
  named, continuously-evaluated assumption would close this without
  changing today's manual `CleanupOrphanedHASTResource` action itself.
- `ListDatasets`'s recursion is filtered by rejecting any name
  containing `/` (a HAST provider dataset created by this codebase is
  always a childless leaf) rather than switching to a non-recursive
  listing call - simplest fix for the one shape that actually occurs
  today; would need revisiting if a HAST provider dataset ever
  legitimately gained children.
- No REST (`internal/restshim`) mirror, matching `HostStats`/
  `GetVMConsole`'s own precedent of local-only RPCs with no REST
  translation.

## Verification

Unit tests (`internal/manager/server_test.go`): not-configured errors
for both RPCs when `zfs` is nil; `CleanupOrphanedHASTResource` rejects
a missing `resource_id` and an invalid `resource_type` before ever
touching raft (both checked with a nil `*RaftClient`, proving the
validation order is safe even with no raft connection at all).

Integration tests (`internal/manager/integration_test.go`, real
raft-harness): `ListOrphanedHASTResources` correctly distinguishes an
owned VM, a replicated (secondary-role) VM, an owned jail, an orphaned
VM, an orphaned jail, and an unrelated non-HAST dataset - reporting
only the two genuine orphans, each with the right `resource_id`/
`resource_type`/`used_size`. `CleanupOrphanedHASTResource` refuses a
still-referenced resource (re-verified fresh, not trusted from a prior
list) with zero destructive calls made, then successfully destroys a
genuinely orphaned one; a missing dataset is a clean not-found error.

`go build ./...`, `go vet ./...`, `gofmt`, `git diff --check`, and the
full `go test ./...` suite all pass. `buf generate` re-run for the
proto changes. FreeBSD cross-compile confirmed for
`managerd`/`raftd`/`restshimd`. Not live-verified against a real
2-node HAST pairing this pass (no throwaway orphaned resource was
manufactured on `apiarium`/`apiverse` for this change) - the
integration-test harness's real raft cluster plus a fake ZFS manager
is the verification depth for this pass, matching how deep unit/
integration coverage typically substitutes for full live verification
elsewhere in this project.
