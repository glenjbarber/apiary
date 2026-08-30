# ADR-0021: sniff ISO9660 vs. raw disk images instead of trusting the name

## Status

Accepted

## Context

Building the noVNC console (ADR-0020) made a real, previously invisible
bug observable for the first time: a VM created with a FreeBSD
"memstick" image as its `iso_name` sat at a blank screen indefinitely.
`internal/cluster`'s Reconciler attached anything named via `iso_name`
to `ahci-cd` (a CD-ROM device) unconditionally. A FreeBSD memstick image
is not an ISO9660 filesystem at all - it's a raw, directly-bootable
MBR/GPT+UFS disk image meant to be `dd`'d onto a USB stick. Presenting
it to firmware as a CD-ROM gives firmware no ISO9660 filesystem to find,
so it never boots. This had been true since ISO upload was first built
(ADR-0017); nothing before the console page existed could actually show
it happening.

## Decisions

### Sniff the file's actual format, not its name or the store it came from

`internal/isostore.Manager.IsISO9660` opens the stored file and checks
for the literal string `"CD001"` at byte offset 32769 (sector 16 × 2048
bytes, plus 1 byte for the Volume Descriptor's own type field) - the
Standard Identifier of an ISO9660 Primary Volume Descriptor, and the
same check `file(1)`/libmagic use. This is deliberately not a filename
heuristic (e.g. "contains 'memstick'") - a name-based check is trivially
wrong for a renamed file and gives no principled way to extend to other
non-ISO9660 media later. The store itself stays name-agnostic, the same
way it already didn't care whether an upload's declared name ended in
`.iso`.

### The Reconciler decides ahci-cd vs. ahci-hd per VM, not per upload

The check happens in `internal/cluster.Reconciler.ensureVM`, at the
point an install image is about to be attached - not at upload time in
`internal/isostore`. This keeps the store's own responsibility narrow
(store bytes, verify a hash, answer "does this name exist") and puts the
attachment-type decision where the rest of a VM's device wiring already
happens. `bhyve.Config` grew a new field, `InstallDiskPath` (PCI slot 7,
`ahci-hd`), distinct from `DiskPath` (the VM's own persistent boot disk,
slot 4) and `ISOPath` (slot 6, `ahci-cd`, now only used for images that
actually pass the sniff test).

### No new user-facing "type" field on upload

The web UI's upload form and `ManagerService.UploadISO` are unchanged -
a user uploads a file and gives it a name, exactly as before. Requiring
them to correctly declare "this is actually a raw disk, not an ISO"
would just be a second, less reliable way of expressing the same fact
the file's own bytes already state - sniffing removes an entire class of
user error (a mislabeled upload silently failing to boot) for free.

## Consequences

- Verified with unit tests: `internal/isostore` covers `IsISO9660`
  directly (a crafted ISO9660 signature at the right offset, an
  unrelated raw-disk fixture, and a file too short to contain the
  descriptor at all); `internal/cluster` covers the Reconciler's
  resulting attachment choice (memstick-style image routes to
  `InstallDiskPath` and never touches `ISOPath`, and a format-check
  failure aborts before `CreateVM` is ever called, the same fail-closed
  pattern already used for a missing/unresolvable image).
- Verified live on `apiarium`: created a VM naming the exact memstick
  image already stored from earlier ISO-upload testing, watched it boot
  straight into the real FreeBSD installer's welcome screen over the new
  noVNC console (ADR-0020) - the same image that previously sat at a
  blank screen indefinitely under the old ahci-cd-always attachment.
- The two already-running VMs from earlier testing (`freebsd-t101`/
  `freebsd-t102`, both created before this fix, both with the same
  memstick image attached via the old buggy `ahci-cd` path) were left
  running rather than recreated - a bhyve VM's device configuration is
  fixed at launch, so this fix only affects VMs created (or reconciled
  for the first time) after it's deployed. Recreating those two wasn't
  necessary to verify the fix and was out of scope for this change.
- This only handles the ISO9660-vs-raw-disk distinction. Other disk
  image formats (qcow2, raw with a different partition scheme, etc.)
  aren't sniffed for or supported - see CLAUDE.md's "importing existing
  VMs from other hypervisors" gap, which remains unaddressed.
