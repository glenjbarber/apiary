# ADR-0148: A Colony-wide VM disk-size floor that can only be raised

## Status

**Accepted and implemented** on 2026-10-01. The floor is raft-replicated FSM
state with a new `SetColonyDiskSize` command whose monotonicity is enforced
inside the FSM apply function, the per-VM override is recorded on
`VMDefinition.disk_size_mb`, and the reconciler resolves
`max(colony floor, per-Comb config, per-VM override)` when it creates a disk.
Neither disk path resizes anything.

**Accepted but not built: the operator-facing web page.** The floor is set and
read today through `ManagerService.SetColonyDiskSize` /
`GetColonyDiskSize`, and through `raftd`'s own
`RaftInternal.GetColonyDiskSizeLocal`. No `web/templates` page edits it yet, so
an operator using only the web UI cannot currently change the floor. That is a
missing affordance, not a missing guarantee: the monotonicity the requirement
is about is enforced in the FSM and holds regardless of which client submits
the command. The page is deliberately not in this change because the Machine
page's existing "Disk size (MB)" row is *per-Comb* and putting a Colony-wide,
replicated, never-lowered value on it would invite exactly the confusion this
ADR exists to remove. It needs its own surface.

The requirement, verbatim from the owner's note:

> VMs disk image needs to be configurable. Throughout the colony, a default
> is set, and can never be reduced, only increased. We do not yet have to do
> resizing.

## The decision in one paragraph

The floor lives in raft's FSM as one Colony-wide value, `ColonyDiskSize`, set
by a single new command. The FSM apply function refuses any value below the
value already in state, so "never reduced" is a property every replica decides
for itself from the same log rather than something a handler asks nicely for.
A per-VM override is allowed but only above the floor, it is recorded on
`VMDefinition.disk_size_mb` so every replica computes the same disk size, and
the reconciler takes the maximum of the floor, this Comb's own config, and
that override at the moment it creates the file. Neither path resizes an
existing disk, which is what "we do not yet have to do resizing" asks for.

## Where the floor lives, and why it is not a per-node config

**(a): a replicated raft/FSM value.** Rejected: **(b), a per-node local config
set identically everywhere.**

The requirement has two words in it that (b) cannot satisfy. "Throughout the
colony" wants one value every Comb reads. "Can never be reduced" wants a
guarantee, not a convention.

A per-node JSON value satisfies neither structurally. Four Combs each holding
`disk_size_mb: 20480` is four independent numbers that happen to be equal at
one moment. The moment one of them is edited, is restored from an older
`managerd.json`, or joins the Colony from a config that says `40960`, the
Colony has two defaults, and nothing in the system says so. The reconciler on
each Comb would then size a disk from its own local file, so the *same*
`VMDefinition` in the same raft log would produce *different* disk sizes
depending on which Comb happened to own the VM. That is the one thing Apiary's
architecture forbids outright: replicas must compute identical results from
the same log, never from local state that could differ between them
(`SHARED.md`, "Architecture to preserve").

The evidence for that reading is already in this codebase, three times, in
nearly the same words. `AcquireRestartLease` is raft-replicated "because the
whole safety property depends on raft's own serialized log-apply order making
concurrent acquisition impossible, which a per-node check could never
guarantee". `ColonyJoinWindow` exists as FSM state because "a window held in
one managerd's memory is a window that silently disappears when that Comb
steps aside". `AcquireColonyUpdate` is replicated because the single-flight
"has to survive the managerd that is coordinating it being restarted, and
because it has to hold across coordinators on different Combs". A floor that
"can never be reduced" is exactly that class of guarantee: it must survive the
Comb that set it going away, and it must be the same number everywhere.

## Where monotonicity is ENFORCED

**Inside the FSM apply function**, `applySetColonyDiskSize`, not in a
managerd handler.

A handler check is advisory. It runs on one process, before the command
enters the log, and it protects only the code path that went through it. The
same command can arrive from a peer-forwarded write, from a
`raftd` invoked directly, or from a future client that was not in the
handler's mind when the check was written. A follower that applies the log
without that check would then hold a lower floor than the leader, and the two
would compute different disk sizes for the same VMDefinition - the exact
failure (b) is rejected above, arrived at by a different route.

The apply function is authoritative for a structural reason: it is the only
place that runs, in the same order, on every replica. It sees the committed
log and the state it is about to change, so "is this below what we already
have" is a question only it can answer without racing. That is the same
posture `applyPinTrustedPeer` takes when it re-evaluates a certificate at the
FSM boundary "rather than trusted from whatever wrote the command", and it is
why the state digest (ADR-0143) can be trusted to cover the floor: the value
is inside `FSMSnapshotState`, so it is inside the digest.

## Reject, or accept-and-clamp

**Reject.** A request for a value below the current floor returns an error
naming both numbers and changes nothing.

Clamping would silently override operator intent, and there is no way for the
operator to tell a clamped success from a real one. Worse, it would make the
log lie: the command says `floor_mb = 1024`, the state says `20480`, and every
future reader of that log entry has to know which one happened. Apiary already
refuses rather than clamps for exactly this reason, twice, in as many words:
`applyOpenColonyJoinWindow` on a duration over the ceiling, because "clamping
is how an RPC becomes a way around configured policy, and because an operator
who asked for twenty minutes and silently got ten has no way to tell the
difference from a Colony that opened fine"; and `applyPinTrustedPeer` on a
pin claiming voter membership, refused rather than accepted-and-ignored.

A floor that clamps upward is also not really a floor. It is a default that
lies about being a default.

`floor_mb == 0` is refused for the same reason and not treated as "unset".
There is exactly one way to express "no floor recorded" in this design, and
it is the absence of any `SetColonyDiskSize` entry in the log - which is the
same shape `ColonyJoinWindow` uses, where "never opened" and "closed" are
deliberately the same case for every caller.

## The per-VM override

**Allowed, but only above the floor, and recorded in the FSM.**

"A default is set" implies something can be set instead of it, so an override
is part of the requirement rather than an extension of it. A per-VM request
*below* the floor is refused: if it were honoured, the floor would be a
recommendation, and "can never be reduced" would be false for exactly the VMs
an operator would most want to shrink.

The value is recorded on `internalpb.VMDefinition.disk_size_mb` (field 22),
mirrored on `rpcpb.VMDefinition.disk_size_mb` (field 21). This is not
optional bookkeeping. If the override lived only in the caller's head or in
the reconciler's memory, the disk size would again be a function of local
state, and two Combs reconciling the same log entry would disagree about how
big the disk is. Recording it on the definition is what makes the size a pure
function of the replicated state, exactly as `ip_address` and `mac_address`
are assigned by the FSM rather than by the caller.

Zero means "no override": a VM that says nothing about its size gets the
floor. That is deliberately the same default-means-unset shape as every other
optional field on `VMDefinition`, and it is what makes an existing VM created
before this ADR keep working untouched.

### The trap in `UpdateVM`, and why it is handled narrowly

`UpdateVM` is a full-record replace, so a VM that already records a
`disk_size_mb` below a *later* floor would be refused on any later update for
a reason that has nothing to do with what was being updated. That would make
the record un-updatable, which is a bug this codebase has form about:
`SetVMFirewallPaused` exists (ADR-0049) precisely because `UpdateVM`'s
full-replace semantics make it unsafe for a narrow change.

So the floor is checked against a value only when that value is *new*:
`applyCreateVM` refuses a non-zero `disk_size_mb` below the floor, and
`applyUpdateVM` refuses one that is below the floor **and** differs from what
the record already holds. Carrying an existing value forward is not a new
request and is not refused. The stored value is deliberately not rewritten
when the floor rises past it either - the record says what was asked for, and
the reconciler resolves the effective size.

## What each disk path does with a floor

A floor is a minimum at creation, never an adjustment.

**The plain, dataset-backed path** (`ensureDiskImage`): the size is the
`Truncate` target on a freshly `os.Create`d sparse file, set to the resolved
maximum. The function already returns early when the file exists, so a raised
floor never touches a disk that is already there. The file is sparse, so a
floor well above what a guest writes costs no blocks until data is written.

**The HAST-replicated path** (`ensureHASTProvider`): the size is the
`Truncate` target on the provider file, set the same way, and the same
early-return on an existing file applies. One correction to the framing of the
request: this is **not** a zvol. `ADR-0026` recorded, and confirmed live on
apiarium, that `hastd` cannot read or write its own metadata against a
zvol-backed provider ("Unable to read metadata ... No such file or directory",
role never takes effect), so the provider is a plain file inside a dedicated
dataset. The sizing call site is the same one either way, which is why
`diskSizeMB()` is shared by both paths and is still shared now.

**Nothing resizes.** Not the plain file, not the HAST provider, not a
snapshot. ADR-0031's rule that `-disk-size-mb` is ignored for a disk seeded
from a base image is unchanged: a seeded disk is the base image's size, and
the floor does not silently pad it. That is a real gap this ADR does not
close, and it is recorded here rather than left to be discovered.

## What the existing per-Comb `DiskSizeMB` becomes

`Reconciler.DiskSizeMB`, its `-disk-size-mb` flag, and
`nodeconfig.Manager.DiskSizeMB` all stay exactly as they are. They become the
middle term of the maximum:

    effective = max(colonyFloor, r.DiskSizeMB, vm.DiskSizeMB)

This is deliberate. It means the Machine page's existing per-Comb "Disk size
(MB)" field keeps working for an operator who wants one Comb to provision
bigger disks than the rest, which is a real capability today, and it means no
existing deployment's `managerd.json` changes behaviour on upgrade. It also
means the per-Comb value cannot lower the colony floor, because a maximum
cannot. The guarantee the requirement asks for is on the colony floor; a
per-Comb value below it is not a reduction of the floor, it is a value the
floor outranks.

`JailDiskSizeMB` (2048) is untouched. A replicated jail's HAST-backed root is a
different resource with a different size history, and the requirement is
about VM disk images. Extending the floor to jails would change jail
provisioning on every Comb that has never asked for it.

## Shape of the change

- `api/internalpb/state.proto`: the `ColonyDiskSize` message;
  `Command.set_colony_disk_size = 45`; `FSMSnapshotState.colony_disk_size =
  16`; `VMDefinition.disk_size_mb = 22`.
- `internal/raft/colonydisksize.go`: the apply function and its clone helper,
  mirroring `internal/raft/colonyjoinwindow.go`.
- `internal/raft/fsm.go`: the state field, the dispatch arm, the snapshot and
  the restore.
- `api/internalpb/raftd.proto` plus `internal/raft/node.go` and
  `internal/raft/server.go`: `GetColonyDiskSizeLocal`, deliberately
  **not** leader-only, mirroring `GetColonyJoinWindowLocal` - the floor is
  replicated state and a follower must be able to answer for itself, because
  the reconciler reads it on every Comb.
- `api/rpc/manager.proto` plus `internal/manager`: `SetColonyDiskSize`
  (Admin) and `GetColonyDiskSize` (Viewer), with the usual leader-hint
  forwarding on the write.
- `internal/cluster/reconciler.go`: `GetColonyDiskSizeLocal` on the reconciler's
  own `raftClient` interface, `DiskSizeMB` on `VMPlacement`, and the resolved
  maximum at both sizing call sites.

## What this does not claim

- **No web page sets the floor.** Stated at the top; the RPCs are the only
  affordance today.
- **No resizing.** A raised floor applies to disks created after the raise.
  An existing disk keeps the size it was created with, by design and by the
  request.
- **A disk seeded from a base image is not padded to the floor.** ADR-0031's
  existing rule, unchanged, and a real gap.
- **Not verified on FreeBSD.** The `Truncate` behaviour on both paths is
  unchanged by this ADR - only the number passed to it changed - but the
  macOS test run establishes nothing about ZFS, HAST, or bhyve. See the
  Verification list.
- **The per-Comb `DiskSizeMB` is not itself monotonic.** An operator can
  lower it. That is not the floor, and lowering it does not lower the floor.

## Verification

- `gofmt -l .` empty, `go build ./...`, `go vet ./...`,
  `go test -count=1 ./...` all clean on macOS darwin/arm64.
- `TestSetColonyDiskSizeRefusesLowering` and
  `TestSetColonyDiskSizeRefusesLoweringOnASecondReplica` in
  `internal/raft/colonydisksize_test.go`: a lowering command is refused after
  a raise, including when a second FSM instance is fed the identical command
  sequence, which is the determinism claim made above rather than asserted.
- `TestUpdateVMCarriesAStalePerVMDiskSize` and
  `TestCreateVMRefusesAPerVMDiskSizeBelowTheFloor` in
  `internal/raft/colonydisksize_test.go`: the `UpdateVM` full-replace trap and
  the create-time refusal.
- `TestSnapshotRestorePreservesColonyDiskSize` in `internal/raft/fsm_test.go`:
  a restore cannot silently drop the floor, which would let a voter re-open
  the Colony to a lower default.
- `TestEffectiveDiskSizeMB*` in `internal/cluster/reconciler_test.go`: the
  maximum, and both disk paths sizing from it.
- Neither disk path's early-return-on-exists behaviour is exercised here, so
  "no resize" is established by reading `ensureDiskImage` and
  `ensureHASTProvider`, not by a test on this machine.