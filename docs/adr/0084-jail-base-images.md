# ADR-0084: Jail base images via ZFS clone

## Status

Accepted

## Context

`internal/cluster/jail.go`'s `ensureJail` has always created a jail's
root as a brand-new, empty ZFS dataset before calling `jail(8)` directly
on it - there was no equivalent of `VMDefinition.base_image_name`
(ADR-0031) for jails at all. `jail(8)` doesn't care that its root is
empty; it starts "successfully" with nothing usable inside it. This is
the exact gap a live-testing session surfaced: creating a jail "silently
fails" in the sense that it comes up but is useless, with nothing in the
existing code path surfacing that as an error.

VMs closed their equivalent gap by reusing `internal/isostore`: a caller
uploads a raw file once, and the reconciler copies it into a fresh disk
file (ADR-0031). That pattern doesn't fit jails as well - a jail's root
isn't a single flat file - so this ADR instead adopts the more idiomatic
FreeBSD/ZFS approach: **cloning a jail's root from a pre-populated
"template" ZFS dataset**, at the cost of more new surface area than
ADR-0031 needed (new `internal/zfs` primitives, a manual/operator-driven
template convention, no existing UI catalog to mirror).

## Design decisions

- **`JailDefinition` gains `base_template`** (`api/rpc/manager.proto`,
  `api/internalpb/state.proto`, field 9) - caller-set at `CreateJail`
  time. Empty means today's behavior (a blank dataset), unchanged for
  every existing jail.
- **A "jail template" is a ZFS dataset an operator creates and populates
  manually, not something Apiary itself creates or uploads.** The
  convention: `<node's ZFS base>/templates/<name>`, populated by hand
  (mount it, extract a `base.txz` or otherwise assemble a root, unmount),
  then snapshotted with a fixed, Apiary-recognized name:
  `<name>@apiary-template`. For example:
  ```
  zfs create zroot/apiary/templates/freebsd-14
  mount -t zfs zroot/apiary/templates/freebsd-14 /mnt/tmpl
  tar -xf base.txz -C /mnt/tmpl
  umount /mnt/tmpl
  zfs snapshot zroot/apiary/templates/freebsd-14@apiary-template
  ```
  A jail created with `base_template: "freebsd-14"` then clones that
  snapshot into its own root the first time it's created. This mirrors
  ADR-0031's "caller must supply an already-usable artifact, Apiary does
  no conversion" stance, adapted to ZFS: Apiary clones the snapshot, it
  never creates or populates a template itself.
- **Templates are node-local and not raft-replicated or fetched across
  nodes.** Unlike ADR-0031's base images (later given cross-node peer
  fetch by ADR-0041), this ADR does not add a ZFS-send/receive
  equivalent. An operator must create the identically-named template on
  every node a templated jail might be placed on. This is a real,
  disclosed limitation, not silently swept aside - a natural follow-up,
  not part of this build.
- **Two new `internal/zfs.Manager` primitives**: `SnapshotExists(ctx,
  "dataset@snapshot")` and `Clone(ctx, "dataset@snapshot", destName)`,
  following the existing `path()` validation style exactly (the
  snapshot half is validated separately - non-empty, no `/`). No new
  `internal/jail` changes were needed: `Config.Path`/`CreateJail`
  already work with any pre-populated directory, cloned or not.
- **Cloned only on first creation, never re-cloned**, exactly mirroring
  ADR-0031's `ensureDiskImage` rule: `ensureJail`'s existing
  `!exists` check already guards this for free - a `base_template`
  changes *what* gets created that one time (a clone instead of a blank
  dataset) but not *when*.
- **Unsupported together with `replica_node_id`.** A HAST-replicated
  jail's root is a raw device formatted via `internal/ufsmount`, not a
  ZFS dataset (ADR-0026) - cloning has no meaning there. `ensureJail`
  rejects the combination immediately with an explicit error, rather
  than silently ignoring one field or the other.
- **No catalog/dropdown in the UI.** Unlike VM base images (backed by
  isostore's raft-replicated-by-reference file list), there is no
  registry of templates to enumerate - they're manual and node-local by
  design. `new_jail.html` gets a plain optional text field instead.

## Consequences

- `internal/restshim`'s REST-facing `jail` JSON shape
  (`internal/restshim/convert.go`) gained `base_template`, so a
  REST-only client can set it exactly like the web UI and gRPC API.
- Full test coverage: `internal/zfs` gained integration tests for
  `SnapshotExists`/`Clone` (real `zfs(8)`, skipped without it, mirroring
  the package's existing convention) plus a pure unit test for the new
  snapshot-name validation; `internal/cluster`'s reconciler tests cover
  cloning from an existing template, the missing-snapshot error path,
  the `replica_node_id` conflict, and never-re-cloning an existing
  dataset (mirroring ADR-0031's equivalent `base_image_name` coverage);
  `internal/manager`, `internal/restshim`, and `internal/frontend` each
  gained a round-trip/plumbing test for the new field.
- This closes a real, user-reported gap (jail creation silently
  producing a useless empty root) with an idiomatic-for-FreeBSD
  mechanism, at the cost of a manual, per-node operational step
  (creating identical templates on every node that might host a
  templated jail) that ADR-0031's file-based approach didn't need. A
  cross-node template-fetch mechanism (ZFS send/receive, mirroring
  ADR-0041's peer-fetch for VM images) is a plausible, explicitly
  disclosed follow-up, not attempted here.

**Resolved (narrowed, not eliminated) by ADR-0089**: a jail base
template missing on the node a templated jail lands on is now fetched
automatically from the first cluster peer that has it, via a real `zfs
send`/`receive` stream - mirroring ADR-0041's VM/ISO peer-fetch. This
narrows the limitation above rather than eliminating it: an operator
must still create a template once, somewhere in the cluster - only the
"must exist on every node it might be scheduled to" part is gone.
