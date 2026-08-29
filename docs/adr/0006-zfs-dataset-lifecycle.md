# ADR-0006: internal/zfs dataset lifecycle

## Status

Accepted

## Context

`internal/zfs` is the first FreeBSD-specific package, and the first
tested against a real FreeBSD host (a VM, ahead of real hardware) rather
than something portable to macOS/Linux dev machines. It will eventually
back VM disk images and jail root filesystems with real ZFS datasets;
this slice proves the dataset lifecycle primitives against real `zfs(8)`.

## Decisions

### Every operation is scoped under a configured `Base` path, with strict name validation

`Manager.Base` (e.g. `zroot/apiary` in production, `apiarytest` in tests)
is the root all operations are confined to; dataset names are validated
(no empty segments, no `.`/`..`, no leading `/`) before being joined onto
`Base`. `zfs destroy` is irreversible, so the priority here is making it
structurally impossible for a bug elsewhere in Apiary to construct a path
that escapes `Base` and destroys something unrelated — this is enforced
in `Manager.path`, which every method routes through.

### `DestroyDataset` has no recursive option

It fails naturally (matching plain `zfs destroy`'s own default) if the
target has children or snapshots. A single accidental call cascading
through a subtree is a worse failure mode than an operation refusing to
proceed; a recursive variant can be added explicitly once something real
needs cascading delete.

### Test permissions are delegated via `zfs allow`, not run as root

Tests run as the unprivileged VM user (`claude`) against a dedicated test
pool (`apiarytest`), with `zfs allow` delegating exactly the permissions
needed (dataset CRUD plus specific properties) rather than requiring
root/`su` in the test loop itself. Root was used once, manually, to
create the test pool and issue the delegation — not something the test
suite or package code does or depends on at runtime.

**Gotcha discovered doing this**: `zfs allow`'s `userprop` permission
only covers user-defined custom properties (e.g. `apiary:foo`) — it does
*not* cover setting native properties like `compression`. Each native
property that should be settable by a delegated user needs to be named
explicitly in the `zfs allow` invocation. The test pool's delegation was
expanded to include `compression,quota,reservation,mountpoint,atime,exec,readonly`
once this surfaced (a `SetProperty(name, "compression", "lz4")` call
failing with "permission denied" despite the dataset-CRUD permissions
already being granted).

### Verification requires cross-compiling and running on a real FreeBSD host

There's no `zfs` binary on the dev machine, so `internal/zfs`'s
integration tests skip cleanly there (checked via `exec.LookPath("zfs")`)
and only the pure `path()` validation test runs locally. Real
verification is `GOOS=freebsd GOARCH=amd64 go test -c ./internal/zfs`,
copied to the VM via `scp`, and run directly over SSH — this is a manual
step for now, not part of any CI this project has (none exists yet).

## Consequences

- Any future package needing to delegate additional native ZFS
  properties to the `claude` test user will hit the same
  `userprop`-doesn't-cover-native-properties gotcha; check delegated
  permissions (`zfs allow <pool>`) before assuming a `SetProperty` call
  will work in tests.
- `internal/bhyve`/`internal/jail`, once implemented, will likely depend
  on `internal/zfs.Manager` for their backing datasets — the `Base`
  scoping means each can be given (or share) a `Manager` pointed at an
  appropriate subtree without risk of one stepping on the other's
  datasets, as long as their `Base` values don't overlap.
