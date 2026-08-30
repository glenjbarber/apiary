# ADR-0017: ISO upload with hash verification, and bhyve CD-ROM attachment

## Status

Accepted

## Context

Every VM the reconciler provisions boots from a blank, zero-filled disk
image - there's no way to actually install an operating system into
one. This ADR adds the first piece of a real install workflow: a way to
upload an installer image (ISO) to a node, verify its integrity against
a hash the operator already trusts, and reference it when creating a
VM so the reconciler attaches it as a CD-ROM. A console (noVNC or
otherwise) to actually watch and interact with that install is
deliberately out of scope here - a separate, later piece of work.

## Decisions

### ISOs are physical, per-node data - not raft-replicated

Like ZFS datasets and bhyve disk images, an uploaded ISO is real bytes
on one node's local disk, matching the physical/ephemeral split
CLAUDE.md already establishes for everything else in this codebase.
`internal/isostore` is a new package with no dependency on raft at
all - `ManagerService.UploadISO`/`ListISOs`/`DeleteISO` are answered
directly from managerd's own local store, the same way `Status`
answers from local knowledge rather than going through `Apply`.

### Hash verification happens server-side, during the write - not after

The upload flow is deliberately closer to Proxmox's than to a typical
"upload then separately verify" flow: the client sends both the file
and the hash it expects in one request, and the server computes the
actual hash *as it streams the file to disk*, comparing at the end. If
they don't match, the partially-written file is deleted and the upload
fails outright - there is no window where an unverified or
wrong-hash file sits in the store under its real name. A client-
computed hash sent without ever being checked would defeat the entire
point of asking for one; `isostore.Manager.Save` refuses to proceed
without an `expectedSHA256` argument at all, so this isn't something a
caller can accidentally skip.

### Upload is a client-streaming gRPC call, not a unary one

An ISO can be many gigabytes - buffering it into one Protobuf message
isn't viable, and matches this project's "RPC-style first, explicit
named operations" design principle (CLAUDE.md's Architecture section)
rather than reaching for an ad hoc REST multipart endpoint instead. The
client streams a single `metadata` message (name + expected hash)
followed by a sequence of `chunk` messages; the frontend's own HTTP
handler mirrors this by using `http.Request.MultipartReader` to relay
the browser's multipart upload directly into the gRPC stream without
ever buffering the whole file in the frontend process either - the
hash field is placed before the file field in the HTML form
specifically so it's already known by the time the file part needs to
open the gRPC stream and send its `Metadata` message first.

### A hash sidecar file avoids re-hashing multi-gigabyte files on every list

`isostore.Manager.List` doesn't recompute each file's SHA-256 - it
reads back whatever `Save` recorded in a `<name>.sha256` sidecar at
upload time. Re-hashing on every page load would make the Images table
noticeably slow once a few large ISOs accumulate, for a benefit (
detecting silent on-disk corruption) this package doesn't claim to
provide anyway.

### `VMDefinition.iso_name` is a plain reference, resolved locally by the reconciler

A VM names an ISO by filename (`iso_name`), not by embedding a path -
the actual local filesystem path is only meaningful on whichever node
ends up running the VM, so resolving it is the reconciler's job
(`Reconciler.ISOs`, mirroring `Bhyve`/`ZFS`'s own local-manager
fields), not something baked into the ephemeral VM definition itself.
A VM naming an ISO that isn't present on its assigned node fails
reconciliation with a clear error (surfaced via the existing `Phase` /
`phase_error` mechanism from ADR-0016) rather than silently booting
without one.

### Fixed a real gap found while wiring this in: bhyve networking was never connected to `cmd/managerd`

While adding `Reconciler.ISOs`, it became clear `Reconciler.Bridge` had
no equivalent - `internal/bhyve`'s `Config.Bridge` (tap/bridge
networking) was implemented and tested directly against `internal/bhyve`,
but `cmd/managerd`'s `Reconciler` construction never set it, so no VM
provisioned by the real daemon binary ever actually got a NIC. This is
the same class of gap ADR-0015 already found and fixed once for
`Bhyve` itself. Added `-bhyve-bridge` alongside the new `-iso-dir` flag.

## Consequences

- A VM can now be created with `iso_name` set and boot into a real
  installer, once a build with a console (not yet built) can actually
  observe it. Until then this is provisioned but unobservable - the
  VM boots, but nothing shows what happens next.
- No image library/catalog exists - a name is just whatever the
  uploader chose to call the file; there's no dedup, versioning, or
  metadata beyond size and hash.
- Nothing reclaims an ISO no longer referenced by any VM - `DeleteISO`
  is a manual, explicit action, consistent with this project's
  create-only-by-default caution elsewhere (ADR-0012, ADR-0016).
- This unblocks the next planned piece of work: a console (likely
  noVNC, via bhyve's own framebuffer device) to actually watch and
  interact with a VM booted from one of these images.
