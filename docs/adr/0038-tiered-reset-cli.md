# ADR-0038: tiered reset CLI on raftd/managerd

## Status

Accepted

## Context

This project's own four-plus-machine cluster is explicitly a test and
resilience harness, not a production deployment meant to run forever
untouched — nodes get deliberately crashed, disconnected, and rebuilt to
prove the system actually survives those events. A real crash this
session forced exactly that kind of recovery by hand: stop all four
services, `mv` aside `raftd`'s data directory, restart fresh, re-join
the surviving peer. That manual procedure worked, but it's also the
kind of multi-step sequence that contributed to the same night's chaos
(a stale `daemon(8)`-supervisor gotcha bit the recovery four separate
times). A repeatable, built-in way to reset between test runs is
recurring infrastructure for this project, not a one-off cleanup script.

## Design

Three tiers, by increasing blast radius, each requiring an explicit
confirmation **phrase** rather than a bare boolean flag — every service
here runs under `daemon(8)`'s `-r` auto-restart supervisor, so a bare
`-reset` boolean accidentally left in `rc.conf`'s `apiary_*_args` would
wipe state on every single respawn. A wrong or missing phrase is a hard
rejection with nothing done, never a silent no-op.

- **Tier 1 — `raftd -reset=yes-wipe-raft-state`**: a one-shot mode (run
  instead of the normal server) that moves `-data-dir` aside to a
  timestamped backup path and recreates it empty, then exits. The next
  normal start bootstraps a fresh single-node cluster automatically,
  since `cmd/raftd`'s existing bootstrap-vs-join logic already does the
  right thing against an empty directory. Real VMs/jails/disks are
  completely untouched — this only ever touches raft's own coordination
  state.
- **Tier 2 — `managerd -reset-managed=yes-wipe-managed-resources`**: a
  one-shot mode with **no raft/RPC dependency at all** — it constructs
  the same `internal/zfs`/`internal/jail`/`internal/bhyve`/
  `internal/isostore` managers `main()` already builds from its own
  `-zfs-base`/`-jail-prefix`/`-bhyve-prefix`/`-iso-dir` flags, and
  destroys everything each can see, in the same dependency order
  `internal/cluster/reconciler.go`'s own `teardownVM`/`purgeJail` already
  use (jails via `RemoveJail` first, then bhyve VMs via `DestroyVM`,
  then remaining datasets via `DestroyDataset`, then stored ISOs via
  `Delete`). The actual enumerate-and-destroy logic lives in a new,
  independently unit-tested `internal/resetutil` package, keeping
  `cmd/managerd/main.go` itself thin.
- **Tier 3 — `managerd -factory-reset=yes-nuke-everything`**: runs Tier
  2's full destruction first, then two companion flags
  (`-factory-reset-extra-jails`, `-factory-reset-extra-datasets`) name
  anything *outside* normal scope to also destroy, via raw `jail(8)`/
  `zfs(8)` invocations that bypass the scoped Manager types entirely.
  Nothing is ever auto-discovered at this tier either — only what's
  explicitly named dies, but *any* named thing dies, including things
  Tier 2 structurally can't reach.

### The key insight: Tier 2's safety isn't new code, it's existing scoping

`internal/zfs.Manager` is hard-scoped to a configured `Base` dataset
with strict name validation (ADR-0006); `internal/jail.Manager` and
`internal/bhyve.Manager` are hard-scoped to a configured name `Prefix`
(ADR-0007); `internal/isostore.Manager` is scoped to its own directory.
A jail like `timemachine` (no `apiary-` prefix) or a dataset like
`ztank`/`tank` (outside `zroot/apiary`) is **structurally unreachable**
through these managers today — not because Tier 2 added a new
protected-resource check, but because the existing scoping already
excludes them. This is what makes Tier 2 safe to run without
double-checking, and it's why Tier 3 has to reach for raw `jail(8)`/
`zfs(8)` calls instead of the Manager types: going outside scope is a
deliberate, structural departure, not a flag flip on the same code path.

## Consequences

- Full unit coverage: `internal/resetutil`'s enumerate-and-destroy logic
  against fake managers (normal operation, one failure not aborting the
  rest, nil managers skipped, empty scope), plus `cmd/raftd`/
  `cmd/managerd`'s own confirm-phrase gating (wrong phrase does nothing,
  correct phrase acts, backup directory actually preserves the old
  state).
- Live-verified on `apiverse` (a low-risk target with zero real
  VMs/jails at the time): `-reset-managed` and `raftd -reset` both
  confirmed working end-to-end, including `apiverse` re-joining
  `apiarium`'s cluster cleanly afterward.
- Tier 3 is **not** live-tested against `apiarium`'s real `timemachine`
  jail or `ztank`/`tank` data — deliberately too destructive to rehearse
  casually. Covered by unit tests of the extra-resource-list code path
  using throwaway names instead, and by code review of the raw
  `jail(8)`/`zfs(8)` invocations themselves.
- Not addressed: Tier 2/3 destruction is genuinely irreversible (no
  ZFS-snapshot-based undo) — matching what was actually asked for, not
  a scope gap.
