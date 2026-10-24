# ADR-0153: Configurable base-image disk size and safe expansion

## Status

Proposed. This design has not been approved by the owner and is not implemented.

Supersedes: None
Superseded by: None
Tracking issue: #110

## Context

Apiary stores uploaded installer and VM base images in `internal/isostore`. The store currently records the uploaded file's byte length and checksum. For compressed base images, the uploaded byte length is not the disk's virtual size. Seeded VM disks are copied from the image but are not expanded, and the VM creation form does not select a disk size. ADR-0148 already defines a replicated Colony-wide disk-size floor and a per-VM size override, but neither disk path currently resizes a seeded image.

The upload may be a compressed raw image or a compressed qcow2 image. The original upload is immutable input shared by image consumers. Expansion therefore has to target only the VM's own newly-created disk, and the capacity check has to account for concurrent VM creation while preserving Apiary's existing free-space reserve.

## Decision

### Measure and publish the virtual size

`isostore.Info` will carry `VirtualSizeBytes` as a positive byte count, distinct from its existing `SizeBytes`, which continues to mean stored upload bytes. Upload completion and image listing protobufs will expose the same value as `virtual_size_bytes` on `UploadISOResponse` and `ISOInfo`. The protobuf fields are additive. A zero or absent value on an older stored image is unknown and must be measured before the image can be selected for VM creation; it must never be treated as a zero-byte disk.

The upload path stages the verified upload, identifies compression from the existing supported compression signatures, and inspects the uncompressed disk payload before publishing the image. For raw input, virtual size is the byte length after decompression. For qcow2, `qemu-img info --output=json` on the uncompressed staging file supplies `virtual-size`; compressed upload length is never used as a substitute. If format inspection fails, reports an unsupported format, or yields a non-positive or unrepresentable size, the upload is rejected and its staging files are removed. The stored file remains the original uploaded byte stream; metadata is stored with the image record in isostore and copied with image transfers so a receiving store can verify or recompute it.

### Per-image default with the Colony floor

Each uploaded base image has a configurable default disk size in MiB, initially rounded up from its measured virtual byte size. The image upload/edit surface lets an operator choose a default, and the store persists it with that image's metadata. The value cannot be less than the image virtual size. This is per-image because the useful default is tied to the footprint and intended use of a particular image; a single per-node value would differ across nodes, while a Colony-wide per-image default would replicate image-specific local storage metadata unnecessarily.

ADR-0148 remains authoritative for the Colony-wide minimum. On VM creation, the form pre-fills the selected image's per-image default. The user may increase it, but cannot reduce it below the image's virtual size or the current Colony-wide floor. The accepted value is written to `VMDefinition.disk_size_mb` and is subject to the FSM's ADR-0148 floor validation. The effective creation size is the greater of the selected request and the Colony floor. Blank disks and snapshot clones retain ADR-0148's existing sizing behavior. The per-image default does not replace, lower, or bypass that floor.

### Expand only the VM-owned copy

Creation must resolve and validate the image, create a new disk at the destination for this VM, and fully copy or decompress the uploaded image into that destination first. Only after copy/decompression completes successfully may expansion run. The uploaded image and its metadata are never opened for writing or resized. This ordering also applies to compressed streaming at VM creation: decompression streams into the VM disk, then the completed disk is expanded.

Let `source_bytes` be the measured virtual size and `target_bytes` the accepted target. Reject `target_bytes < source_bytes`; equality is a no-op. All byte conversions use checked integer arithmetic. Convert MiB with checked multiplication by 1,048,576, reject zero, negative values, malformed values, values beyond the supported signed file-offset range, and any addition or multiplication overflow. Do not narrow an unchecked integer or accept a wrapped result.

- **Raw:** determine the completed destination's actual length and confirm it equals `source_bytes`. Compute `delta = target_bytes - source_bytes` only after proving `target_bytes >= source_bytes` and that the subtraction is representable. Expand the destination with `truncate -s +<delta bytes>` or an equivalent checked absolute-size operation to `target_bytes`. Never run truncate against the compressed upload.
- **qcow2:** run `qemu-img resize <destination> <target_bytes>` (or the equivalent explicit byte-size syntax supported by the pinned toolchain) against the VM-owned qcow2 destination. Do not resize the compressed source or infer its virtual size from its compressed length.
- **Other formats:** reject expansion until an explicit format-specific implementation is designed. Do not silently treat an unknown format as raw.

After expansion, verify the destination virtual/logical size is exactly the requested size before exposing it to bhyve. Errors identify the VM, image, requested size, source size, and the failed operation where known, and advise an operator to check the target node's available storage and image format. Any copy, decompression, inspection, resize, verification, or capacity failure removes the incomplete destination disk. A partially initialized VM must not be reported ready.

### Thin provisioning and capacity claims

Choose configurable thin provisioning with overcommit accounting, not a claim of strict physical reservation. `truncate` changes a sparse file's logical length and does not reserve physical blocks. A future implementation may use ZFS accounting or filesystem allocation primitives for observations, but it must not describe sparse growth as reserved capacity.

Before accepting an expansion, the target node checks the same backing filesystem or ZFS pool used for the VM disk against the existing 1 GiB free-space reserve policy. The reserve remains a minimum protected headroom, not an extra disk-size cap. A node-wide capacity coordinator serializes claims for concurrent provisioning on that storage domain. Its ledger tracks both active thin-disk logical commitments and in-flight claims, so completing a provision does not make its commitment disappear. Each in-flight claim accounts for the full requested logical growth above the source virtual size. The configured overcommit ratio limits aggregate logical commitment relative to the capacity policy, while the 1 GiB reserve is checked independently against currently available physical capacity and can never be counted as overcommit allowance. The policy ratio, active commitments, in-flight claims, and observed free capacity must be visible in actionable capacity errors. A claim remains held through copy, expansion, and final verification; on success it becomes an active commitment, and on failure it is released. Active commitments are removed only when the corresponding disk is deleted or an authoritative storage rescan proves it no longer exists. Recheck physical free capacity during long copies and immediately before expansion; abort and clean up if the reserve can no longer be maintained.

This accounting prevents two concurrent Apiary provisions from each spending the same observed headroom and avoids forgetting already provisioned thin disks. It cannot guarantee future physical writes by guests on thin disks: guest writes, snapshots, compression, and unrelated host consumers can change actual allocation after admission. It cannot account for unrelated host consumers without an operating-system or storage-pool reservation interface. The implementation must state these limits in operator-facing settings and errors. ADR-0148's replicated floor remains separate from this local physical-capacity decision.

## Consequences

- Upload and list results distinguish compressed/stored bytes from the source disk's virtual size, including for compressed images.
- Operators can set an image-specific default; VM creation starts there and stores the accepted per-VM size in replicated state while respecting ADR-0148.
- A VM's disk can be larger than its base image without changing the uploaded source or converting an unknown format.
- Capacity admission serializes Apiary's own concurrent allocations and preserves the established 1 GiB reserve. Thin allocation does not promise that future guest writes will fit physically.
- Implementation must update generated protobuf code with its sources, image transfer metadata, upload and image editing UI, VM creation request handling, format-aware expansion, and cleanup paths. The streaming copy behavior must remain compatible with #95.
- The owner must approve the overcommit setting semantics and the limits of non-Apiary host allocation before this design is implemented. No arbitrary global disk-size cap is introduced; configured limits remain tied to the existing reserve and storage policy.
