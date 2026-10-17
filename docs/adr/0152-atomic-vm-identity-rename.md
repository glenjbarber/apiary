# ADR-0152: Atomic VM identity rename

## Status

Proposed. Implementation is not complete; do not enable UID-based identity migration until the acceptance criteria below are met.

## Context

Cluster API PR #5 changes newly created VM IDs from a namespace/name-derived identity to an identity scoped to the Kubernetes resource UID. Retries of one resource must retain identity, while deletion and recreation under the same namespace/name must not silently adopt the previous resource's VM.

Apiary currently has two operations that are not suitable for migrating the logical ID:

- `UpdateVM` requires the supplied ID to exist and replaces the complete record at that same key.
- `MigrateVM` transfers VM ownership to a replica node; it is not a logical identity rename.
- `DeleteVM` enters the normal deletion lifecycle and can cause physical teardown.

Therefore the migration requires a dedicated Raft command and managerd RPC. It must not be implemented as a sequence of UpdateVM and DeleteVM calls.

## Decision

Add an explicit VM identity rename command applied atomically by the Raft FSM. The operation takes a legacy source ID, a UID-scoped destination ID, and sufficient caller identity/ownership evidence to establish that the caller is migrating the intended VM.

The FSM operation must:

1. Validate both IDs using the existing resource-ID validation rules.
2. Require an eligible source VM and verify the requested ownership/migration preconditions.
3. Reject a destination ID that already identifies another VM. Never overwrite an existing destination.
4. Move the logical record atomically, preserving every field other than the identity key/ID that must change.
5. Never create a deletion tombstone or invoke deletion/purge behavior.
6. Define retry behavior explicitly. A repeated request may report success only when the destination is demonstrably the already-completed rename of the same source resource. A destination collision alone is not proof of success.
7. Return the authoritative resulting VM record to managerd callers.

The external managerd RPC must follow existing leader-forwarding and authorization conventions. Protobuf sources and generated Go code must be updated together using the repository's pinned generation toolchain.

## Reconciler and physical-resource compatibility

Source tracing confirms the resource-name coupling is real, not hypothetical:

- `internal/cluster/reconciler.go` converts each Raft VM record into a `VMPlacement` whose `ID` is exactly `VMDefinition.id`.
- ZFS dataset and bhyve existence/create/destroy operations use that ID.
- `teardownVM` destroys resources using `vm.ID`; `reclaimStaleVM` also removes local resources under the ID it receives.

A bare Raft-key rename is unsafe: the next reconcile can treat the destination as a new placement and provision resources under the new ID while the existing resources remain under the legacy ID. A later reclaim pass could also treat the old resources as stale.

### Preferred design: stable physical resource identity

Add a separate, persisted `resource_id` (name subject to repository conventions) to the VM definition and placement model. The logical/API `id` can then change from the legacy namespace/name value to the UID-scoped value while `resource_id` remains stable for host operations.

Compatibility requirements:

- Existing records with no `resource_id` must resolve to their current `id` for physical operations. This preserves the names of existing datasets and bhyve VMs.
- New records initialize `resource_id` once and never silently derive a new physical identity from a later logical-ID change.
- Create, existence-check, stop, teardown, stale reclaim, HAST resource naming, and any other host-resource paths must use the stable physical identity consistently. API lookup and Kubernetes ownership continue to use the logical ID.
- The migration must atomically assign/preserve the stable physical identity before changing the logical ID. It must be retry-safe and fenced so concurrent reconciliation cannot provision under both identities.
- Never destroy or rename host resources merely because the logical ID changed. If any physical-resource rename is ultimately required, that is a separate explicit migration with its own recovery protocol.

The physical-identity split also affects more than ZFS and bhyve. The initial call-site search found VM-ID-derived identifiers for:

- HAST resource names (owner, replica, reclaim, and recovery/cleanup paths).
- PF firewall anchors.
- Snapshot operations and disk paths, which address the VM's dataset.
- Frontend routes and displayed resource identity, which should generally continue to use the logical ID.
- Reconciler placement and stale-resource planning, which need a deliberate choice of logical versus physical identity.

This is a design recommendation requiring a complete call-site audit, not implemented behavior. Before coding, classify every use of `VMDefinition.id` and `VMPlacement.ID` as logical identity, stable physical identity, or a derived identifier that needs an explicit migration rule. In particular, HAST and PF naming must not silently diverge between manager-side operations and reconciler-side operations. Do not assume the initial search is exhaustive.

## Call-site audit: initial classification

The initial repository-wide search identifies these classes of ID use. This is a classification baseline, not a claim that every call site has been exhaustively reviewed.

| Area | Current ID use | Migration rule |
|---|---|---|
| Raft/API record and REST conversion | `VMDefinition.id`, manager conversion, REST DTO ID | Logical identity; changes to UID ID only through the explicit migration. |
| UI routes, resource lists, health tree | VM ID in routes and displayed resource identity | Continue to use logical ID. Ensure any route-to-resource lookup resolves to the same record during migration. |
| ZFS dataset and bhyve VM | Direct ID for existence, create, destroy, disk image | Stable physical identity; existing resources must keep their current names. |
| HAST resource names and cleanup | `vmHASTResourceName(id)`, mirrored manager-side HAST naming, replica/reclaim/recovery paths | Derive from stable physical identity consistently on owner and replica. Verify recovery and orphan cleanup APIs. |
| PF firewall anchor | `vmAnchor(id)` | Decide explicitly whether firewall state follows logical identity or stable resource identity; flush/reapply must not leave stale anchors or touch another VM. |
| Snapshots and disk clone/restore | Dataset name and `<source_vm_id>@<snapshot_name>` references | Snapshot storage follows stable physical identity; user-facing references need a logical-to-physical resolution rule. |
| Placement and stale reclaim | `VMPlacement.ID` feeds `Plan`, `PlanReclaim`, replica planning, and resource teardown | Split logical identity from physical resource key; never reclaim solely because a logical ID changed. |
| Derived indexes and diagnostics | HAST sync lookup keys and invariant facts include VM ID | Determine whether each key is an external/logical label or a physical HAST name; keep both sides consistent. |

### Implementation boundary

The protobuf/model change is cross-cutting: adding a persisted physical ID to `VMDefinition` alone is insufficient. The manager RPC DTO, REST conversion, VM placement type, ZFS/bhyve paths, HAST naming and manager-side cleanup, PF anchors, snapshots, recovery/invariants, and tests must all be reviewed together.

For legacy records, an absent physical ID must resolve to the current logical ID so existing datasets and bhyve VMs remain discoverable. New records must establish physical identity once. The logical rename operation must preserve that physical identity. No caller may fall back to provisioning a second physical resource merely because migration is incomplete.

## Additional source-trace results

The next pass confirmed these concrete coupling points:

- `internal/cluster/reconciler.go`: `ListVMsLocal` maps `VMDefinition.id` directly to `VMPlacement.ID`. `ensureVM`, stop, teardown, stale reclaim, HAST role construction, and PF flushing all consume that placement ID.
- `internal/cluster/hast.go` and `internal/manager/server.go`: HAST naming/cleanup conventions are mirrored across packages. Any physical-ID split must update both sides together, including replica and orphan-resource cleanup.
- `internal/manager/vmsnapshotsend.go`: snapshot transfer uses the requested VM ID as the ZFS dataset name and as the destination dataset name. A logical ID change therefore needs an explicit snapshot lookup/transfer compatibility rule.
- `internal/manager/convert.go` and `api/rpc/manager.proto`: public RPC VM definitions currently carry `id` but no separate physical resource identity; conversion code maps the ID directly into the internal record.
- `api/internalpb/state.proto`: the Raft `VMDefinition` likewise has no physical resource identity field today.

The initial classification is therefore validated across reconciler, HAST, snapshot transfer, and the internal/public data models. Remaining before coding: finish searching all direct ID use sites, especially snapshot create/restore/delete, PF rule application, recovery/orphan cleanup, and API/UI lookup assumptions; then decide whether the persisted field should be named `resource_id` or another term consistent with existing model conventions. This is still a source audit only, not an implementation or test result.

## Additional audit findings: snapshot and orphan semantics

The next search pass found additional concrete dependencies:

- `internal/manager/server.go` implements Create/List/Restore/Delete VM snapshot RPCs against local ZFS storage. These are local physical-storage operations, not Raft-replicated VM record changes.
- `internal/manager/vmsnapshotsend.go` constructs snapshot references as `<id>@<snapshot>` and uses the ID as the receive destination dataset. A logical-ID migration needs an explicit mapping rule for existing snapshots and transfer destinations.
- `internal/frontend/vm_snapshots.go` resolves the VM owner from the logical ID and sends snapshot operations to that node. Preserve this logical lookup while resolving the stable physical dataset name at the owning node.
- `ListOrphanedHASTResources` and `CleanupOrphanedHASTResources` scan and remove local provider datasets using HAST physical resource names. Cleanup must compare stable physical identity and cannot infer that an old-name resource is orphaned just because the logical VM ID changed.
- `vmAnchor(id)` is used when flushing PF state. Audit the matching PF rule-application path and make sure rule application and flush use the same identity policy during and after migration.

These findings confirm the migration has both a logical lookup plane (API/UI and owner resolution) and a local physical-storage plane (datasets, snapshots, HAST provider resources, PF anchors). The implementation must keep them distinct and define resolution at the owning node; a blanket replacement of ID uses is unsafe.

## PF and recovery lookup checks

The matching PF rule-application path is now confirmed: `ensureVM` applies rules with `vmAnchor(vm.ID)`, and `stopVM`, `teardownVM`, and stale reclaim flush anchors derived from the same ID. Once logical and physical IDs are split, these paths must use one deliberately chosen anchor identity; applying under one ID and flushing under another would leave stale rules.

The recovery/orphan-cleanup path needs particular care. Manager-side orphan HAST cleanup builds its active-resource set from VM logical IDs, while the orphan resources it scans are physical HAST dataset names. After identity separation, the active set must be derived using the same stable physical identity and HAST naming function as the reconciler and manager-side replication code. Otherwise a valid legacy-named provider dataset could be misclassified as orphaned.

Local console and serial-log paths also use the VM ID to find host resources. These should be checked as physical-resource lookups at the owning node, while API selection and owner resolution remain logical-ID lookups.

## Validation

Required tests include:

- Successful rename preserves all VM fields and leaves exactly one logical VM.
- Missing source is rejected.
- Existing destination is rejected without mutation.
- Ownership or eligibility mismatch is rejected.
- Repeated completed rename is safe only with sufficient evidence that it is the same resource.
- The operation does not mark the VM deleting, purge it, or invoke teardown.
- Snapshot/restart preserves the resulting state.
- managerd leader forwarding returns the leader's authoritative response.
- Reconciler/resource-name behavior is verified for legacy and UID-scoped IDs.

Run the focused tests and the complete relevant Go test suites in the repository's required Go 1.27 environment. Do not merge on source inspection alone.

## Rollout and dependency

Cluster API PR #5 remains blocked until this operation is implemented, tested, and integrated. The controller must not create a replacement VM while migration is unresolved or failed. Keep the existing identity behavior available for existing resources until the explicit migration succeeds.

Related: [Cluster API PR #5](https://github.com/glenjbarber/cluster-api-provider-apiary/pull/5)
Related tracking issue: [Apiary issue #137](https://github.com/glenjbarber/apiary/issues/137)
