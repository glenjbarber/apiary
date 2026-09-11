# ADR-0095: Create VM from an existing VM's snapshot

## Status

Accepted

## Context

ADR-0090 gave a VM's own ZFS dataset (holding `disk.img`) real
snapshot/rollback support, so an operator can undo a bad in-guest
change without rebuilding from scratch. It didn't give any way to use
that same snapshot to *start* a brand-new VM - the only ways to seed a
new VM's disk remained a blank file, or `base_image_name`'s copy of a
raw uploaded image (ADR-0031). Needing to stand up a new node quickly
by cloning an already-configured VM (rather than reinstalling an OS
from an ISO again) surfaced this gap directly.

ADR-0084 already established the right primitive for exactly this
shape of problem - cloning a ZFS dataset from a snapshot - for jail
base templates, using `internal/zfs.Manager`'s `Clone`/`SnapshotExists`
methods. Those same two methods already exist and are unchanged by
this ADR; only a new caller (VM creation) and a new snapshot source
(any VM's own ADR-0090 snapshot, rather than a manually-created
`templates/<name>@apiary-template`) are added.

## Decision

### `VMDefinition` gains `clone_from_snapshot`

Added to both `api/internalpb/state.proto` and `api/rpc/manager.proto`
(field 19 in both, mirroring `base_image_name`'s own "next available
field number, added identically to both protos" precedent). Value is
`"<source_vm_id>@<snapshot_name>"` - the same combined form
`internal/zfs.Manager.Clone`/`SnapshotExists` already take, so no new
parsing/validation helper is needed beyond what those methods already
do. Caller-set at `CreateVM` time only, consulted exactly once (mirrors
`base_image_name`'s "seeded only on first creation, never re-seeded"
rule, inherited for free from the same `!exists` guard `ensureVM`
already uses).

### Reconciler: `internal/cluster/reconciler.go`'s `ensureVM`

Mirrors `ensureJail`'s own `base_template` branch (ADR-0084) exactly:

- Rejected combinations, checked up front: `clone_from_snapshot` with
  `replica_node_id` (a HAST-replicated VM's disk is a raw device, not
  a cloneable ZFS dataset - identical reasoning to `base_template`'s
  own HAST rejection), and `clone_from_snapshot` with `base_image_name`
  together (two different ways to seed the same disk; only one can
  apply, and picking a winner silently would hide a caller mistake
  rather than surface it).
- When the VM's own dataset doesn't exist yet: if `clone_from_snapshot`
  is set, `SnapshotExists` is checked first (a clear "does not exist
  locally" error if not, naming both the VM and the snapshot), then
  `r.ZFS.Clone(ctx, clone_from_snapshot, vm.ID)` runs instead of
  `CreateDataset`.

**Node-local only, no cross-node fetch** - the same disclosed
limitation ADR-0084's jail templates originally shipped with (later
narrowed, not eliminated, by ADR-0089's peer-fetch for that specific
case). If the source VM's snapshot lives on a different node than the
new VM's target `node_id`, `SnapshotExists` simply returns false there,
producing the same clear "not found locally" error - correct behavior
without any special-casing, since a `zfs clone` genuinely can't reach
across nodes. Building a `zfs send`/`receive` peer-fetch equivalent
(mirroring ADR-0089) is a plausible, undisclosed-scope follow-up if
cross-node cloning turns out to matter in practice - not attempted
here.

### Frontend: cascading dropdowns, not a free-text field

ADR-0084's jail `base_template` used a plain text field, justified at
the time by "no existing UI catalog to mirror." That reasoning doesn't
apply here - VM snapshots already have a real, enumerable
cluster-wide list (`ListVMSnapshots`, one owner-forwarded call per VM,
already used by the VM detail page's own Snapshots panel from
ADR-0090). Asked directly, the user chose real dropdowns over a text
field.

`internal/frontend/vm_snapshots.go` gained `currentCloneSources`: fans
out `listVMSnapshots` across every known VM (bounded to
`nodeContextLimit` concurrent calls, mirroring `currentClusterISOs`'s
own per-node concurrent-fetch shape exactly, just per-VM instead of
per-node), keeping only VMs with at least one snapshot - a VM with none
would be a dead, unusable entry in the picker. A VM whose fetch fails
(unreachable owner right now) is silently omitted, the same fail-soft
posture every other create-VM form data source (`ClusterISOs`,
`Networks`, `PlacementHives`) already follows.

`cloneSourceSnapshotsJSON` embeds the result as a JS object literal
(`{"vm-1":{"name":"web-1","node_id":"apiverse","snapshots":["nightly"]}}`),
the same embedded-cue-data pattern `isoMissingByNode` (ADR-0041)
established - no extra round-trip needed when the source-VM dropdown
changes. The second dropdown's own options are annotated with a
"different node than X, clone will fail" warning when the currently-
selected Owner Node doesn't match the source VM's node - informational
only (the real refusal happens server-side via `SnapshotExists`), the
same "cue, not a blocker" posture the ISO/base-image pickers' own
"will be fetched from a peer" annotation already uses.

The two dropdowns (`clone_source_vm_id`, `clone_snapshot_name`) are
combined server-side in `handleCreateVM` into the single
`clone_from_snapshot` field only when both are non-empty - an
incomplete pair (shouldn't happen given the dropdowns cascade in JS,
but the server doesn't trust that) is treated as "not requested" rather
than forwarding a malformed `"vm-1@"` or `"@snapshot"` value.

## Consequences

- `internal/manager/convert.go` and `internal/restshim/convert.go` both
  gained the field in their `toInternal*`/`fromInternal*` and
  `toRPCVM`/`fromRPCVM` pairs, mirroring `base_image_name`'s own
  threading exactly.
- New tests: `internal/cluster/reconciler_test.go` gained direct
  regression coverage mirroring `ensureJail`'s own `base_template`
  suite - successful clone, missing-snapshot error, both rejected
  combinations (`replica_node_id`, `base_image_name`), and
  never-re-clone-once-exists. `internal/manager/integration_test.go`
  and `internal/restshim/server_test.go` each gained a real
  create-then-read round-trip test for the new field.
  `internal/frontend/server_test.go` gained tests for the dropdown
  rendering, the two-field-to-one-field combination, and the
  incomplete-pair safety case.
- Full `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .`
  all clean.
