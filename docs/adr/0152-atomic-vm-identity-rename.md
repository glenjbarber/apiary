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

A logical record rename is not sufficient by itself. Before enabling the caller, trace every physical resource name and reconciliation key derived from VM ID, including dataset and bhyve-related paths. The implementation must either preserve stable physical identity across the logical rename or provide an explicit, recoverable transition that cannot create a second VM or tear down the original.

Do not infer that physical resources have been renamed merely because the Raft record has.

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
