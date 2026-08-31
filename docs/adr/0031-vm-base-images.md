# ADR-0031: VM base disk images

## Status

Accepted

## Context

CLAUDE.md has long named "importing existing VMs from other hypervisors"
as fully out of scope, and specifically that there's no path to give a
VM a pre-installed OS at all - every VM's disk starts as a blank,
truncated file, sized by `-disk-size-mb`. That's fine for a VM whose
first boot is an interactive installer (the existing `iso_name`/ISO-CD
path), but it rules out ever automating provisioning of a Linux VM meant
to run unattended - the motivating case being a Kubernetes Cluster API
(CAPI) provider driving Apiary, where a node VM must boot straight into
a working OS with cloud-init already present, with no human at a
console to click through an installer.

This ADR closes that one specific gap - a way to seed a VM's disk from
a pre-built image - without touching the much larger, explicitly
out-of-scope problem of *converting* someone else's VM/disk format
(qcow2, etc.) into something Apiary can use. The caller must supply an
already-raw, already-bootable image; Apiary does no conversion.

## Design decisions

- **`VMDefinition` gains `base_image_name`** (`api/rpc/manager.proto`,
  `api/internalpb/state.proto`, field 15) - caller-set at `CreateVM`
  time, exactly like `iso_name`. Empty means today's behavior (a blank
  disk), unchanged for every existing VM.
- **Reuses `internal/isostore` as-is, not a new storage mechanism.** A
  base image is, mechanically, just another named file a caller uploads
  once and the reconciler resolves to a local path - exactly what
  `UploadISO`/`ISOs.Path` already do for `iso_name` (ADR-0017). Standing
  up a second upload/verify/local-path pipeline for what's structurally
  the same problem would be needless duplication. The RPC name
  (`UploadISO`) doesn't get renamed - it already means "upload a file
  into this node's local store," which stays accurate; the ADR is where
  this reuse is recorded, not a new name.
- **Seeded only on first creation, never re-seeded.** `ensureDiskImage`
  (`internal/cluster/reconciler.go`) already only acts when the disk
  file doesn't yet exist - `base_image_name`, when set, changes *what*
  gets written that one time (a copy of the base image instead of a
  truncated blank file) but doesn't change *when* - an already-existing
  disk file is left untouched on every later tick, matching how every
  other resource this reconciler manages (datasets, bhyve VMs) already
  behaves.
- **A plain file copy (`io.Copy`), not `dd`.** Nothing about this needs
  a subprocess - Go's own `io.Copy` between two open file handles does
  the same thing with less error-handling surface (no shelling out, no
  parsing `dd`'s own error output). The destination file is removed on
  any copy failure, so a partial copy can never be mistaken for a valid
  disk image on a retry.
- **No image format conversion, no size handling beyond what's copied.**
  The uploaded file must already be a raw disk image bhyve can boot
  directly (e.g. an official Ubuntu/Debian cloud image already converted
  from qcow2 to raw, `qemu-img convert -O raw` run by the operator
  outside Apiary) - the same "no qcow2 conversion" limit CLAUDE.md
  already names for VM import in general. `-disk-size-mb` is ignored
  when a base image is used; the disk is exactly the size of the image
  copied in.

## Consequences

- `internal/restshim`'s REST-facing `vm` JSON shape
  (`internal/restshim/convert.go`) was extended to carry `iso_name`,
  `network_id`, `ip_address`/`mac_address`, and `base_image_name` -
  previously missing entirely (the REST API had no way to set an ISO or
  network on VM creation at all, a real pre-existing gap this pass
  happened to surface while wiring up a REST client for the field this
  ADR adds). This is what unblocks a REST-only client (e.g. the
  Terraform provider, or a future CAPI provider) from ever setting these
  fields - previously only the gRPC `ManagerService` API and the web UI
  could.
- `internal/restshim` also gained `POST /v1/isos` - `UploadISO`
  (ADR-0017) previously had no REST equivalent at all, only
  `ManagerService`'s own client-streaming gRPC and `internal/frontend`'s
  multipart form. `handleUploadISO`/`uploadISOStream`
  (`internal/restshim/server.go`) mirror `internal/frontend`'s identical
  handler exactly - same multipart-to-gRPC-stream relay, same
  `expected_sha256`-before-`file` field-order requirement. Without this,
  a REST-only client would have no way to upload the base image or
  cloud-init seed ISO this ADR's own field exists to reference.
- Full test coverage: `internal/cluster`'s reconciler tests cover
  seeding a new disk from a base image, never re-seeding an existing
  one, the unresolvable-name and no-store-configured error paths
  (mirroring the equivalent `iso_name` tests exactly).
- This is a real, if narrow, expansion of Apiary's own scope (VM disk
  provisioning can now start from *something* other than blank) - it
  does not touch or relax the much larger stated non-goal of importing/
  converting VMs from other hypervisors, which remains fully out of
  scope.
