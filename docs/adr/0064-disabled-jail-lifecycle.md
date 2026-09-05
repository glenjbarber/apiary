# ADR-0064: Disabled jail provisioning and explicit deletion

## Status

Proposed implementation on `jail-disabled-lifecycle-fix`.

## Context

An operator created `test-jail-01` through apiarium's frontend, assigned to
apiverse. The jail never appeared and could not finish deletion. Read-only
inspection found `jail-enabled=false` in apiverse's managerd startup log,
no running jail by that managed name, and no matching ZFS dataset.

RunOnce fetched jail records only when its Jail driver was non-nil. This
made ensureJail's disabled-support error unreachable. DeleteJail correctly
stored a tombstone for the owning reconciler, but that reconciler ignored
the tombstone too. The existing disabled-jail test explicitly expected this
silent behavior. In addition, the jail UI did not poll reconciliation state.

The operator also explicitly requires that the host-owned `timemachine` jail
always remain outside Apiary management.

## Decision

- Read local jail intent on every reconciliation tick, independently of
  whether creation is enabled. Preserve local owner filtering and explicit
  tombstone requirements. Failed reads never authorize cleanup.
- Separate the lifecycle driver from the provisioning switch. Managerd always
  supplies the jail driver and mount configuration; `-jail-enabled=false`
  sets `JailProvisioningDisabled`. This does not create a jail or enable
  jail replica provisioning. Assigned non-deleting jails report a visible
  phase error directing the operator to enable provisioning on the owner or
  delete the record.
- Explicit deletion on the owner still checks the running jail, removes it
  if present, destroys its scoped plain dataset if present, and only then
  purges the tombstone through the existing leader-forwarding path. This
  also lets a never-provisioned plain jail finish deletion.
- A nil lifecycle driver is not proof of absence: refuse teardown rather
  than destroy storage or purge blindly. With provisioning disabled, a
  replicated jail additionally requires HAST and mount support before any
  teardown action. Missing capabilities or failed inspection remain visible
  errors. Never use ForcePurge as an automatic fallback.
- Keep stale-assignment reclamation and jail HAST provisioning gated off
  when provisioning is disabled. The newly available cleanup path is for
  explicit owner tombstones, not general garbage collection.
- Exclude the protected `timemachine` ID before jail planning, including
  primary, replica, reclaim, and tombstone paths. The jail wrapper also
  rejects both the logical and qualified protected name before executing
  host commands, and omits it from managed inventory. The factory-reset
  extra-jail loop skips the exact protected jail name as well. This is
  defense in depth beyond the normal `apiary-` prefix boundary. This does
  not introduce a general storage-dependency or protected-dataset model.
- Poll the complete jail panel every three seconds via a read-only,
  authenticated endpoint. Preserve role-specific action visibility. Drop
  polling requests while a deletion request is in flight; a delete may
  replace an outstanding poll. The outer wrapper stays stable while the
  complete banner/table fragment is replaced, avoiding mixed table-row and
  out-of-band markup.

## Scope and verification

No protobuf, Raft state, placement, or service-boundary changes are required.
The creation API still records desired placement; this change reports an
unsupported destination during reconciliation rather than introducing a
new capability-aware admission policy. It does not turn provisioning on,
populate a jail root with FreeBSD userland, or change either live host.

Regression coverage exercises disabled creation, deletion before and after
provisioning, absent drivers, failed jail/dataset inspection, insufficient
replication support, owner boundaries, protected-jail exclusion, and panel
refresh/Viewer permissions. Native jail/ZFS teardown must still be verified
on FreeBSD under separately authorized deployment and testing.
