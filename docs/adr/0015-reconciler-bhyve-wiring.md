# ADR-0015: Wiring internal/bhyve into the reconciler

## Status

Accepted

## Context

ADR-0012 built `Reconciler` to provision a ZFS dataset per VM assigned to
the local node, and explicitly called out launching the actual VM as a
deliberate next step once `internal/bhyve` supported disk-backed VMs
(ADR-0010, extended here to add a `DiskPath` field to `bhyve.Config`).
This slice does that: `RunOnce` now provisions a running bhyve VM backed
by a disk image inside the VM's dataset, not just the dataset itself.

## Decisions

### `Bhyve` is an optional field, not a required dependency

Most nodes in this cluster (the FreeBSD VMs) don't have hardware-assisted
virtualization and can't run bhyve at all; only bare-metal hosts like
`apiarium` can. Making `Reconciler.Bhyve` nil-able — nil disables VM
provisioning entirely, leaving dataset-only reconciliation — lets every
node run the same reconciler safely, rather than needing a
hardware-capability flag threaded through from configuration or every
tick failing outright on hosts that can't run bhyve. This was verified
directly: the same reconciler code path, exercised against real
infrastructure, produces a full dataset+disk+VM on `apiarium` and a
dataset only on `freebsd-apiary`, both succeeding.

### Resource sizing comes from `VMDefinition`, with defaults when unset

`VMPlacement` gained `Vcpus`/`MemoryMB` fields sourced from the raft-side
`VMDefinition`. Since both are optional today (nothing enforces a caller
sets them), `ensureVM` falls back to `defaultCPUs`/`defaultMemoryMB` (1
vCPU / 512MB) rather than passing zero values through to `bhyve(8)`,
which would either fail outright or behave unpredictably.

### The disk image lives inside the VM's own dataset, sized by `DiskSizeMB`

`ensureDiskImage` creates a sparse file at `<mountpoint>/disk.img` using
plain `os.Create`/`Truncate` — no ZFS zvol, just a regular file on the
dataset's filesystem, matching how `bhyve`'s `ahci-hd` device backend
already expects a file or block device path. Defaulting to 10GiB when
`Reconciler.DiskSizeMB` is zero avoids a razor-thin default that would
make any real OS install fail immediately, while still letting tests
(and eventually real configuration) override it.

### Existence-checking moved out of `Plan` and into `RunOnce`/`ensureVM`

ADR-0012's `Plan` signature took `existingDatasets []string` and
computed a diff itself. Once there are three independent resources per
VM (dataset, disk image, bhyve VM), each with its own existence
semantics and its own fresh-per-tick check, cramming that into one
combined diff computation stopped making sense. `Plan` now only answers
"which VMs is this node responsible for" (sorted, filtered by
`node_id`); `RunOnce` calls `ensureVM` per VM, which checks and creates
each resource type in dependency order — dataset before disk image
(the image lives inside the dataset's mountpoint), disk image before
the VM (the VM's config needs the disk path). This also simplified
`Plan` back down to a pure, trivially-tested filter/sort with no I/O
awareness at all.

### Still create-only; still one error doesn't stop the rest

Consistent with ADR-0012, nothing here computes anything to destroy —
`Bhyve` never gets a "stop this VM" or "reclaim this disk" path, only
create/ensure-running. `RunOnce` also keeps the "continue past one
VM's failure" behavior: a failure provisioning one VM's disk or bhyve
VM is captured as the first error to return, but doesn't stop the loop
from attempting the remaining VMs assigned to this node in the same
tick.

## Consequences

- A node with `Bhyve` set now does real, hardware-backed VM
  provisioning end to end from a single `RunOnce` call: dataset, disk
  image, and a booted bhyve VM sized from `VMDefinition`.
- Verified for real on both ends of the capability split: `apiarium`
  (bare metal, full dataset+disk+VM path) and `freebsd-apiary` (no
  hardware virtualization, dataset-only path), both via the same
  reconciler code.
- Reclaiming resources for VMs that are stopped, deleted, or reassigned
  — for both datasets and now bhyve VMs — remains unimplemented, same
  gap ADR-0012 already called out.
- Real scheduling (deciding `node_id` placement based on available
  hardware capability, e.g. only assigning bhyve-requiring VMs to nodes
  with `Bhyve` support) still has no home; `Bhyve` being optional here
  only makes it *safe* to lack the capability, not aware of which nodes
  have it.
