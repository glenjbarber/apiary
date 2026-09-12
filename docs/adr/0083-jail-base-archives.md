# ADR-0083: Jail base archives and the empty-root safety check

## Status

Accepted

## Context

Creating a jail through the web UI (Jails -> Create Jail) only ever
collected ID/Name/Hostname/Node/Replica node - there was no
install-source or base-image field of any kind. `ensureJail`
(`internal/cluster/jail.go`) creates a brand-new, completely empty ZFS
dataset via `ZFS.CreateDataset` when one doesn't already exist, then
calls `Jail.CreateJail` directly against that empty root. `jail(8)`
does not validate that its root holds a real FreeBSD userland - it
"successfully" starts a jail attached to an empty directory with zero
complaint, and the reconciler went on to report that jail `Ready`, with
no error anywhere, even though the jail had no `/bin/sh`, nothing -
completely non-functional. There was no README section, no ADR, and no
code path anywhere that documented or automated how an operator was
supposed to get a real base FreeBSD system into a jail's root - not
even a manual workaround was written down.

`VMDefinition` already solved the equivalent problem for VMs:
`base_image_name` (ADR-0031) names a raw disk image already uploaded
via `UploadISO`/`internal/isostore`, which the reconciler resolves and
copies in as the VM's starting disk contents the first time it creates
that disk file. This ADR gives jails the same capability, adapted to
the fact that a jail's root is a directory tree, not a single disk
file, plus a safety check that catches the empty-root failure mode
even for a jail that names no base archive at all.

## Design decisions

- **`JailDefinition` gains `base_archive_name`** (`api/rpc/manager.proto`,
  `api/internalpb/state.proto`, field 9) - caller-set at `CreateJail`
  time, exactly like `VMDefinition.base_image_name`. Empty means a jail
  whose root must already be populated some other way before it's
  created; the safety check below still applies to it.
- **base.txz extraction, not a ZFS clone-from-template.** A ZFS
  snapshot/clone approach was considered (cloning a jail's dataset from
  a pre-populated "template" dataset, which would fit this project's
  existing ZFS-heavy patterns and be faster than extracting a
  multi-hundred-megabyte archive on every jail creation), but it
  requires a separate mechanism to create and refresh those template
  datasets in the first place - which itself needs a real base.txz
  extraction at least once. Extracting the standard FreeBSD release
  archive directly is the simpler, more transparent mechanism, matches
  what `bsdinstall`/`ezjail`/`iocage` already do, and doesn't require
  Apiary to invent and maintain its own template-dataset lifecycle.
  Nothing here rules out adding a clone-from-template path later as a
  faster alternative once base_archive_name-driven extraction has
  proven itself.
- **Reuses `internal/isostore` as-is, not a new storage mechanism** -
  the exact same reasoning ADR-0031 already gives for
  `base_image_name`: a base archive is, mechanically, just another
  named file a caller uploads once and the reconciler resolves to a
  local path via the same `ISOs.Path`/`UploadISO` pipeline. A jail's
  `base_archive_name` and a VM's `iso_name`/`base_image_name` all draw
  from the same per-node pool of uploaded files.
- **Checksum verified against the archive's own published MANIFEST.**
  FreeBSD publishes a `MANIFEST` file alongside every release's
  `base.txz` listing `name<TAB>sha256<TAB>size<TAB>"description"` for
  every distribution file. `isostore.ParseManifest`
  (`internal/isostore/store.go`) parses that format into a
  name -> sha256 map; `internal/frontend`'s upload handler
  (`handleUploadISO`) accepts an optional `manifest` form field (a
  paste of that file's contents) alongside the existing
  `expected_sha256` field - when the hash is left blank, the uploaded
  file's own name is looked up in the parsed manifest instead, so
  `base.txz` is checksum-verified against the exact hash FreeBSD itself
  published for that release, rather than an operator transcribing a
  64-character hex string by hand. This is layered on top of
  `isostore.Save`'s existing "an unverified upload is never kept"
  guarantee (ADR-0017) - it changes *where the expected hash comes
  from*, not the verification itself, and it's generic (any upload can
  use it), not jail-archive-specific. Automatically fetching a MANIFEST
  from the internet is explicitly out of scope, matching ADR-0017's own
  existing boundary that uploads are always manual, never auto-fetched.
- **A new `internal/jailarchive` package, extracting via `tar -xpf`,
  not pure-Go.** base.txz is a tar archive compressed with xz; Go's
  standard library has no xz decoder, but `tar(1)` on FreeBSD (bsdtar)
  and every other platform this codebase targets auto-detects and
  decodes xz/gzip/bzip2 compression from the archive's own contents.
  Shelling out here matches the existing convention of
  `internal/bhyve`/`internal/hast`/`internal/jail` (all shell out),
  rather than `internal/isostore`'s deliberate pure-file-I/O exception
  (see that package's own doc comment on why it's the odd one out).
  `-p` preserves permissions and ownership, since a base userland
  includes setuid binaries and device nodes whose exact mode matters.
- **`Reconciler.JailArchives`** (`internal/cluster/reconciler.go`) is a
  new, narrow, optional dependency (the same opt-in-capability pattern
  as `Bhyve`/`ISOs`/`VLAN`) wrapping `jailarchive.Extractor`. A jail
  naming `base_archive_name` on a node with `JailArchives` (or `ISOs`)
  unset fails reconciliation with a clear, actionable error instead of
  silently leaving the root empty.
- **Extracted only into an empty root, never re-extracted.** Mirrors
  `ensureDiskImage`'s "seed only a fresh resource, never re-seed"
  pattern exactly: `ensureJailRoot` checks whether the root is empty
  before doing anything, extracts only in that case, then re-checks
  emptiness afterward. An already-populated root (a jail that's been
  running for a while, or one populated some other way) is never
  touched again on later reconcile ticks.
- **The empty-root safety check applies unconditionally, not just when
  a base archive is set.** This is the cheap, immediate usability
  improvement the bug report specifically called out: even before an
  operator adopts `base_archive_name` at all, a jail whose root turns
  out to be completely empty - for any reason - now fails
  reconciliation with a clear, visible error ("this jail's root
  filesystem is empty, so jail(8) would start with no /bin/sh...")
  instead of silently reaching `PhaseReady`. `dirEmpty`
  (`internal/cluster/jail.go`) treats a not-yet-existing path as empty
  too, since a real ZFS dataset or a real `ufsmount.Mount` always
  creates its target directory as part of creating/mounting it - a
  still-missing path means "definitely not populated," the same
  conclusion an empty existing directory reaches.
- **`SimulateNodeFailure`'s image-availability report now covers jails
  too.** `cluster.ImageRoleBaseArchive` (`internal/cluster/simulate.go`)
  and `rpcpb.ImageRole_IMAGE_ROLE_BASE_ARCHIVE`
  (`api/rpc/manager.proto`) extend the existing VM-only
  `iso_name`/`base_image_name` availability check
  (`internal/manager/server.go`'s `SimulateNodeFailure`) to also report
  when a jail's `base_archive_name` would become unavailable if its
  source node were lost - the same "what if this node disappears"
  question ADR-0052 already answers for VM images.

## Consequences

- The web UI's Create Jail form (`web/templates/new_jail.html`) gains a
  base-archive picker mirroring the VM creation form's own ISO/base-
  image selectors exactly, including the "will be fetched from a peer"
  cue (ADR-0041) driven by the same `ClusterISOs`/`isoMissingByNode`
  data the VM form already uses - jails and VMs draw from one shared,
  cluster-wide view of uploaded images.
- `internal/restshim`'s REST-facing `jail` JSON shape
  (`internal/restshim/convert.go`) gained `base_archive_name`, mirroring
  the VM base-image REST field ADR-0031 added.
- The images page (`web/templates/images.html`) gained an optional
  MANIFEST-paste field on the upload form; `expected_sha256` is no
  longer marked `required` in HTML, since either it or a MANIFEST entry
  for the uploaded filename must be present - `handleUploadISO`
  (`internal/frontend/server.go`) enforces that server-side with a
  clear error either way.
- Full test coverage: `internal/cluster/jail_test.go` covers extracting
  a base archive into an empty root, never re-extracting into an
  already-populated one, an archive that "succeeds" but leaves the root
  empty still being an error, the no-ISO-store-configured error path,
  and the empty-root safety check firing with no base archive named at
  all. Three pre-existing jail tests that used a bare `t.TempDir()` as
  a stand-in ZFS mountpoint/HAST root (which, being a real empty
  directory, would now trip the safety check) were updated to write a
  placeholder file first, since they aren't exercising root population.
  `internal/isostore/store_test.go` covers `ParseManifest` and using its
  output directly as `Save`'s `expectedSHA256`. `internal/jailarchive`
  has its own extraction tests against a real (uncompressed) tar
  archive via the real `tar(1)` binary. `internal/manager/
  integration_test.go` covers the jail-image-availability addition to
  `SimulateNodeFailure`.
- This does not change behavior for any jail that already has a
  populated root today - the safety check only ever fires on a root
  that is genuinely empty, which was already a completely broken jail
  before this change; it just used to fail silently.
