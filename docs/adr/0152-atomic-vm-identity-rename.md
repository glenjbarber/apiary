# ADR-0152: Atomic VM identity rename

## Status

Accepted. Implemented in the tree by PR #173 (merge 93d3420). The acceptance criteria below are met: the dedicated FSM command, the managerd RPC with leader forwarding, the logical/physical identity split, and the reconciler conversion are all implemented and covered by tests. Cluster API PR #5 remains the caller's responsibility - it must still retain the legacy identity on any unresolved migration result, as described under "Cluster API PR #5 coordination" below.

**The call-site conversion is NOT complete.** A follow-up audit of the
current tree found four physical-resource call sites that still address a
host resource by the VM's logical id, and therefore miss after a rename:
the console VNC-port lookup, the serial-log path lookup, the
node-failure simulation's HAST status query, and the Command Center's
HAST invariant resource name. They are listed, with the code each one
touches, under "Residual physical-identity gaps" below. The FSM command
and the managerd RPC are implemented and tested; these four read paths
are not yet converted, and they are recorded here rather than left
implied by the original audit's language.

Supersedes: None
Superseded by: None
Tracking issue: #137

## Context

Cluster API PR #5 changes newly created VM IDs from a namespace/name-derived identity to an identity scoped to the Kubernetes resource UID. Retries of one resource must retain identity, while deletion and recreation under the same namespace/name must not silently adopt the previous resource's VM.

Apiary currently has two operations that are not suitable for migrating the logical ID:

- `UpdateVM` requires the supplied ID to exist and replaces the complete record at that same key.
- `MigrateVM` transfers VM ownership to a replica node; it is not a logical identity rename.
- `DeleteVM` enters the normal deletion lifecycle and can cause physical teardown.

A sequence of UpdateVM and DeleteVM is not an atomic or safe substitute for identity rename. Apiary needs a dedicated Raft command and managerd RPC.

## Decision

### Logical identity and physical identity

The API and Raft map key `id` is the logical VM identity. It is the stable identifier exposed to callers, stored in VM references, and used by Cluster API to associate its resource with the VM. Host resources use a separate persisted `resource_id`, which is the stable physical identity. New VMs set `resource_id` to their initial `id`. Existing VM records that have no `resource_id` resolve it as their current `id`; this legacy fallback preserves every existing host resource name.

The rename changes only logical `id` and the map key. It preserves `resource_id` and every VM definition field. The reconciler's placement record carries both values: logical ID for API-facing lookup, error context, and user-visible identity; resource ID for host-resource lookup, creation, and destruction. No physical resource is renamed because the logical ID changed.

### VM-ID call-site audit

This completes the audit left open in the prior text of ADR-0152. The classifications below are the design outcome for all VM-ID uses found in the inspected VM reconciliation, lifecycle, snapshot, disk, HAST, and PF paths. A code change must preserve the same distinction at managerd and reconciler call sites.

| VM-ID use | Identity | Required behavior |
| --- | --- | --- |
| Raft VM map key, `VMDefinition.id`, API get/list/update/delete requests, and Cluster API references | Logical | Rename the key and `id` atomically; API callers use the new ID afterward. |
| Reconciler desired placement identity and placement-plan bookkeeping | Logical | Keep the logical ID to correlate Raft records and report status. Carry `resource_id` alongside it; do not infer physical identity from a changed logical ID. |
| bhyve VM names and existence, create, stop, and destroy calls | Physical | Always use `resource_id`; an existing VM keeps its legacy name after rename. |
| ZFS VM dataset names and mountpoint lookup, create, clone destination, destroy, and stale reclaim | Physical | Always use `resource_id`. Dataset-backed disk paths inherit that dataset name and therefore remain unchanged. |
| HAST resource names, role planning, device lookup, reclaim, and recovery | Physical | Derive the HAST name from `resource_id` on primary, replica, and cleanup paths. Never create a second HAST resource from the new logical ID. |
| PF per-VM anchor names, apply, flush, and reclaim | Physical | Derive the anchor from `resource_id`, so existing rules remain attached and a rename cannot strand or double-flush an anchor. |
| VM snapshots and snapshot-derived disk clones | Logical at the public RPC and VM-reference boundary; physical at the ZFS operation | Resolve the supplied logical VM ID through the current VM record, then access snapshots under its `resource_id`. `clone_from_snapshot` remains a logical source reference and resolves to the source resource ID before ZFS clone. Snapshot names themselves do not change. |
| VM disk file path (`disk.img`) | Physical | It is inside the physical dataset or HAST device and remains attached to `resource_id`; never derive a new path from the renamed logical ID. |
| DHCP hostname, frontend routes/display, logs and API responses | Logical | Show the current caller-facing ID. Host-resource error details may include both IDs. |
| Stale-resource reconciliation and reclaim | Physical, with logical ownership correlation | Reclaim only a physical resource ID proven stale by the plan. A logical rename alone is not proof that the old physical identity is stale. |

The initial source trace directly established that placement currently copies `VMDefinition.id`, and that bhyve, ZFS dataset lifecycle, HAST naming, PF anchors, snapshot operations, and the disk file path derive from or address that ID. The table is the explicit migration contract for these call sites. Any additional VM-ID-derived host identifier found during implementation must be classified as physical before changing behavior; it must not fall back to the new logical ID by default.

### Persisted field and compatibility

Add `resource_id` to internal and external VM definition protobuf surfaces and to the reconciler's placement model, using a new unused field number in each message. The FSM assigns it to `id` on creation when absent. On reads and reconciliation, an empty legacy value resolves to `id`. Once populated, it is immutable through ordinary VM updates and identity rename. Rename materializes the effective legacy value into `resource_id` in the same state-machine apply that changes the key, so a later logical rename cannot change the physical name. Snapshot and restore retain it as ordinary VM definition state.

### Atomic Raft command semantics

Add a dedicated `RenameVMIdentity` operation to the Raft `Command` oneof, implemented in the FSM, and a corresponding explicit managerd RPC. Its request contains the source logical ID, destination logical ID, and expected ownership/migration evidence. It is not expressible as `UpdateVM`, `DeleteVM`, or `PurgeVM`.

At FSM apply time, in this order:

1. Validate both IDs with the existing resource-ID validation rules. Reject empty, malformed, equal, or unsafe identifiers before state changes.
2. Look up the source. If it is absent, apply the idempotent-retry rule below; otherwise return a missing-source error.
3. Verify the source's ownership and migration eligibility against the caller's expected ownership evidence. At minimum, the expected owner and replica assignment must match the current record, and the record must satisfy the same eligibility constraints required by the migration request. A mismatch is rejected without mutation. Validation is repeated in the FSM, not trusted solely to a managerd pre-check.
4. Check the destination. If another VM record already occupies it, reject the request without changing either record. Never overwrite.
5. Clone the source record, materialize its effective `resource_id` using the legacy fallback if needed, change only `id` to the destination ID, and atomically remove the source map key and insert the destination map key in one FSM apply. Preserve name, ownership, replica assignment, desired state, phase, phase error, MAC/IP, disk settings, image and snapshot references, firewall configuration, Cloudflare exposure, and every other VM definition field.
6. Return the resulting authoritative VM record and an explicit outcome. Do not set deleting state, create a deletion tombstone, call normal delete/purge behavior, or trigger teardown.

### Idempotent retry and collision distinction

When the source key is absent and the destination exists, the FSM reports `already_renamed` success only if the destination record proves that it is the same completed rename: its logical ID is the requested destination, its stable `resource_id` matches the requested legacy source identity or supplied migration token, and its ownership evidence matches the request. The response includes the destination VM and the distinct `already_renamed` outcome so callers can distinguish a completed retry from a newly applied rename.

If the destination exists but does not prove this source-to-destination relationship, return a destination-collision error. If neither source nor destination exists, return a missing-source error. A matching destination by name alone is never proof of idempotency.

### Reconciler behavior after rename

The FSM's rename is the sole logical transition; the same VM remains placed on the same owner and replica. Because `resource_id` is unchanged, the next reconciliation sees one logical placement referring to the existing physical bhyve, ZFS, HAST, PF, snapshot, and disk resources. It must not provision a second set under the new logical ID.

A legacy host resource whose name derives from the old ID is the expected physical resource when `resource_id` resolves to that old ID. It is neither stale nor an orphan and must not enter reclaim. The old logical key is absent, but that absence does not authorize destruction: stale reclaim must compare physical IDs against current placements and their persisted/fallback resource IDs. Teardown after a later explicit VM deletion uses `resource_id` exactly once. No rename path calls teardown. Rollout must fence concurrent reconciliation so the atomic state change cannot be observed as two placements or provisioned under both IDs.

### managerd RPC and leader forwarding

The managerd RPC validates request shape and authorization, submits only the dedicated rename command to the local Raft apply path, and returns the FSM outcome and authoritative VM. If the local raftd reports a leader hint, managerd forwards the same request to the leader's managerd using the existing peer-forwarding pattern. The leader repeats validation and relies on FSM checks for authority. A forwarding failure or changed leader is returned as an actionable error with the leader hint; callers re-resolve and retry the same source/destination pair. Retries use the idempotent contract above. Do not claim success from a stale local read or managerd-only key move.

### Cluster API PR #5 coordination

Cluster API PR #5 must not create a replacement VM, switch to a newly generated identity, or treat migration failure as absence. On a collision, ownership mismatch, timeout, leader change, or any other unresolved migration result, it must retain the existing legacy identity and retry or surface a migration error. It may use the UID-scoped identity only after Apiary returns success or the explicit `already_renamed` result for that exact source/destination pair. The provider integration is blocked until this RPC and identity-safe reconciliation are implemented and tested.

## Residual physical-identity gaps

The implementation above converted the reconciler's own paths, but a
follow-up audit of the current tree found four call sites that still
address a PHYSICAL host resource by the VM's LOGICAL id. Each of them
misses after a rename, because the host resource keeps the name it was
created under - the physical id - while the lookup still asks for the
logical id.

The pattern is the same in all four: something creates or names a
resource from the physical id, and something else later looks that
resource up by the logical id.

| Call site | Physical resource | What it does today |
|---|---|---|
| `internal/manager/server.go` - `GetVMConsole` | the bhyve VM's recorded `.vnc` port file | calls `s.vnc.VNCPort(req.GetId())` with the logical id |
| `internal/manager/server.go` - `GetVMSerialLog` | the bhyve VM's captured serial console file | calls `s.serialLog.SerialLogPath(req.GetId())` with the logical id |
| `internal/manager/server.go` - `SimulateNodeFailure` | the VM's HAST resource on its owner and replica nodes | builds `OwnedResourcePlacement{ID: vm.GetId()}`, which `hastResourceName` turns into `vm-<logical id>` for the peer status query |
| `internal/frontend/invariant_hast.go` - `hastResourcesFromVMsJails` | the VM's HAST resource, for the Command Center's HAST invariants | builds `ID: "vm-" + vm.ID`, and `vmView` carries no physical id to build it from |

The bhyve manager derives the `.vnc` and serial-log file names from the
name it was asked to create the VM under, which the reconciler supplies
as the physical id (`r.Bhyve.CreateVM(ctx, physicalID, ...)`), and the
HAST resource name is derived from the physical id as well
(`vmHASTResourceName(vm.PhysicalID())`). The frontend's `vmView` drops
`resource_id` entirely, so the HAST invariant path cannot form the
physical resource name even if it wanted to.

These four are the remaining half of the conversion this ADR originally
called for. The reconciler's own paths - ZFS datasets, bhyve VM names,
tap devices, HAST role planning and reclaim, pf anchors, snapshots, and
stale reclaim - already resolve through `VMPlacement.PhysicalID()`, and
the manager-side HAST orphan discovery and cleanup already compare
`vmPhysicalID(vm)`. Nothing here contradicts the "Implemented" status
of the FSM command and the managerd RPC; it records that the CALL-SITE
CONVERSION is not yet complete, which is what this ADR's original
"call-site audit" language required and what the status line has always
implied by pointing at it.

The fix, when it is made, is the same rule the reconciler already
follows: resolve the logical id to the physical id at the point of use,
and carry the physical id (rather than re-deriving it from a logical id
on the far side of an RPC) wherever the lookup happens on another node.

## Validation

Required FSM and managerd tests cover successful rename; invalid IDs; missing source; destination collision without mutation; ownership/eligibility mismatch without mutation; idempotent retry; rejection of a non-matching destination as a collision; preservation of every VM definition field; materialization and legacy fallback of `resource_id`; no deletion tombstone, purge, or teardown behavior; snapshot/restore persistence; managerd leader forwarding and returned authoritative result; and reconciliation of bhyve, ZFS, HAST, PF, snapshots, disk paths, and stale reclaim without duplicate provisioning or destruction.

Converting the four residual gaps carries its own required tests: a
renamed VM's console must still answer with its VNC port; its serial log
must still be readable; the node-failure simulation must still find the
renamed VM's HAST resource on its owner and replica; and the Command
Center's HAST invariants must still report a verdict for it rather than
an unknown. Each of those needs a case where the logical id differs from
the physical id, because that is the only state in which the distinction
is observable - a test that uses a legacy record, where the two ids are
equal, proves nothing about the conversion.

## Consequences

- Logical IDs can migrate without changing the physical names that bhyve, ZFS, HAST, and PF already use.
- Existing records and host resources remain compatible because an absent `resource_id` falls back to the current ID and is materialized atomically during rename.
- The dedicated FSM command gives all replicas the same collision, ownership, preservation, and idempotency rules.
- The resource-ID field must be added to both protobuf surfaces and preserved by conversions, updates, snapshots, restore, and managerd responses.
- Cluster API PR #5 remains blocked until the operation, full call-site conversion, and required tests are implemented. The caller must preserve the legacy identity on every failure.
- The four residual gaps in "Residual physical-identity gaps" are the remaining part of that full call-site conversion. Each of them is a READ path, so a renamed VM loses access to its console, its serial log, and its HAST status in the Command Center and the node-failure simulation - it does not corrupt or destroy anything. They should be converted on the same rule the reconciler already follows before the rename path is relied on in anger.
