# ADR-0090: VM snapshot and restore

## Status

Accepted

## Context

The user reported a real, recurring cost while iterating on new-node
bootstrap and other in-guest changes: recovering from a botched setup
inside a VM meant either manually undoing whatever was done, or
deleting and recreating the VM entirely (rebuilding it via `apiaryinstall`
or the create-VM form from scratch). Asked how to close that gap, the
user chose a real Apiary-managed feature over the alternative of
snapshotting `node02` itself as a hypervisor VM - the latter would only
help for whole-node experiments, not for "I broke something inside a
guest VM and want to undo it," which is the actual recurring pain.

A VM's disk already lives at a natural checkpoint boundary: its own ZFS
dataset (named `<vm.ID>` directly, holding `disk.img` -
`internal/bhyve`'s `ensureDiskImage`). ZFS snapshot/rollback of that one
dataset is a direct, idiomatic-for-FreeBSD way to checkpoint and restore
a VM's entire disk state, with no new storage format or backup pipeline
needed.

## Decision

### `internal/zfs.Manager` gains four primitives

`CreateSnapshot`, `RollbackSnapshot`, `DestroySnapshot`, `ListSnapshots`,
each reusing the existing `snapshotPath(name string)` helper (already
used by `SnapshotExists`/`Clone`/`Send`) by passing `id+"@"+snapshotName`
- no new name-validation logic, just the existing helper applied to a
new caller.

`RollbackSnapshot` deliberately does **not** pass ZFS's own `-r` flag.
`zfs rollback` without `-r` refuses outright if a newer snapshot exists
on the dataset than the one being rolled back to, rather than silently
destroying it. This is the correct default here: an operator restoring
an older checkpoint should be told a newer one exists and would be lost,
not have it destroyed without being asked.

### Local-only, never raft-replicated - mirrors `SetDatasetQuota`'s own precedent exactly

`internal/manager`'s existing ZFS-touching RPCs (like `SetDatasetQuota`)
are deliberately local-only: no raft `Apply`, no leader-hint forwarding.
The four new RPCs (`CreateVMSnapshot`, `ListVMSnapshots`,
`RestoreVMSnapshot`, `DeleteVMSnapshot`) follow the same shape, added to
the same `quotaSetter` interface `SetDatasetQuota` already uses.

`RestoreVMSnapshot`'s handler consults `s.raft.GetVM(...)` to refuse
restoring into a VM the manager believes is currently running (a live
`disk.img` rollback underneath a running bhyve process is unsafe) - but
only `if s.raft != nil`, guarding against a nil raft client in
manager-only unit tests (the same nil-guard convention already
established elsewhere in this package for optional dependencies). This
check is best-effort: it reflects raft's last-known desired/observed
state, not a live process check, so it is not the real safety boundary
- the UI copy is (see below).

RBAC: `ListVMSnapshots` is Viewer tier (read-only, matches `ListISOs`);
`CreateVMSnapshot`/`RestoreVMSnapshot`/`DeleteVMSnapshot` are Operator
tier (matches every other VM lifecycle-affecting action).

### Cross-node reach: owner-forwarding, mirrored from `serial_log.go` (ADR-0065's own template)

A VM-owner-scoped, non-raft, purely-local physical resource (console,
serial log, and now snapshots) is never auto-forwarded by the managerd
handler itself - it just errors "query that node's managerd directly"
if it doesn't own the VM. Instead, the *frontend* resolves ownership
(`GetVM` + `Status`, comparing node IDs) and dials the owning node's
managerd directly via its own peer-forwarding client, exactly as
`serialLogForVM` already does. `internal/frontend/vm_snapshots.go`'s
`ownerManagerdAddrForVM` mirrors that resolution function line for line.

### Frontend: a Snapshots panel on the VM detail page

Lists existing snapshots, a form to create a new one (a free-text name,
validated only for uniqueness by ZFS itself), a Restore button per
snapshot (browser `confirm()`, naming the real risk: stop the VM first,
this replaces its current disk contents and cannot be undone), and a
Delete button per snapshot (`confirm()`, cannot be undone). A visible
note appears when the VM's desired state is `running`, telling the
operator to stop it first - this is the real safety boundary this
feature relies on; the server-side raft-state check above is a
best-effort backstop, not a substitute for it, since a live check would
require reaching into bhyve process state this handler doesn't have.

## Consequences

- Restoring or deleting a snapshot while the owning Comb's bhyve
  process still holds the disk open is not prevented by anything in
  this feature beyond the best-effort raft-state check and the UI's
  explicit warning - an operator who restores a live VM's disk out from
  under a running bhyve process gets whatever ZFS and bhyve do in that
  situation, undefined and not tested here. This is a disclosed,
  accepted limitation, not silently swept aside.
- `internal/zfs` gained two new integration tests
  (`TestIntegration_SnapshotCreateRestoreDestroy`,
  `TestIntegration_RollbackRefusesWithNewerSnapshot`), real `zfs(8)`,
  skipped without it, mirroring this package's existing convention.
- `internal/manager` gained full unit coverage (a fake `quotaSetter`,
  RBAC tier tests, the nil-raft guard test) and two integration tests
  against a real `raftd` (`TestIntegration_RestoreVMSnapshot_RefusesWhileVMIsRunning`,
  `TestIntegration_RestoreVMSnapshot_AllowedWhileVMIsStopped`).
- `internal/frontend` gained handler tests for create/restore/delete and
  a detail-page render test confirming the Snapshots panel lists
  existing snapshots and surfaces a managerd-reported error.
- As with every prior RPC addition to `ManagerServiceClient`, every test
  double implementing that interface across the repo
  (`internal/frontend`'s four separate fakes, `internal/restshim`'s own
  `fakeClient`) needed the four new methods stubbed - an expected,
  mechanical cost of extending this interface, not a design concern.
- `internal/restshim` gained only the test-fake stubs needed to keep
  `rpcpb.ManagerServiceClient` satisfied; no new REST routes were added
  for these four RPCs in this change - the feature is UI-driven for now,
  consistent with how it was requested. A REST-facing route for
  snapshot management is a plausible, undisclosed-scope follow-up if a
  REST-only client ever needs it.
