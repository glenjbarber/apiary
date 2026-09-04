# ADR-0051: raftd configuration save/restore (-export/-restore)

## Status

Accepted

## Context

Apiary had no way to back up or restore its own raft-replicated
ephemeral state (VM/jail/network definitions, API keys) - distinct
from two things it's easy to confuse this with. HAST (`internal/hast`,
ADR-0008/ADR-0026) replicates VM/jail *disk bytes*, a physical, per-node
concern entirely separate from raft. `raftd -reset` (ADR-0038, Tier 1)
operates on the same ephemeral state this feature does, but only ever
*destroys* it, as a deliberate reset-between-test-runs tool - it has no
save/restore counterpart.

## Design

This lives entirely on `raftd`, not `managerd`: VMs/jails/networks/API
keys are `internal/raft/fsm.go`'s own ephemeral state, the same
category ADR-0038's Tier 1 already operates on. `managerd`'s Tier 2/3
resets exist specifically because *their* targets (ZFS datasets, jails,
bhyve VMs, ISOs) are `managerd`'s own local, non-replicated concern -
a structurally different thing. No `managerd` involvement here, in v1.

### Export needs a live RPC, not on-disk snapshot reading

`internal/raft/node.go` builds raft with unmodified
`raft.DefaultConfig()` defaults - `SnapshotInterval=120s`,
`SnapshotThreshold=8192` log entries. Apiary's real workload (occasional
VM/network/jail/API-key CRUD) will rarely if ever accumulate 8192
outstanding commands, so raft's own on-disk snapshot directory can sit
stale, or completely empty, for a cluster's entire life. Reading it
directly for "the current state" is a real staleness bug waiting to
happen, not a style choice.

Instead, a new leader-only RPC, `RaftInternal.ExportState`, mirrors
`ListAPIKeys`'s own leader-only/error/leader-hint shape. A live `raftd`
serves it by calling its own FSM directly:
`FSM.Snapshot()`'s existing map-copying body was extracted into an
exported, mutex-protected `FSM.SnapshotState()`, with `Snapshot()`
becoming a thin wrapper around it - no behavior change to raft's own
periodic snapshotting. `Node.ExportState()` requires leadership, the
same deliberate choice `ListAPIKeys` makes: this is an occasional,
explicit admin action, not a hot-path reconciler read, so it gets the
strongest consistency guarantee available rather than the `*Local`
reasoning used elsewhere in this file for reconciler reads.

### Restore is offline snapshot pre-seeding, not a live Apply

Traced against `hashicorp/raft`'s actual behavior, not assumed:

- `raft.HasExistingState` (wrapped by this package's own
  `HasExistingState`) returns `true` once *any* snapshot exists, even
  with a completely empty log and zero current term.
- `cmd/raftd`'s real startup already branches on this: `Bootstrap()` is
  only ever called when `HasExistingState` is `false`.
- `raft.NewRaft` restores from the newest snapshot *before* ever
  scanning the log, setting both committed and latest configuration
  straight from that snapshot's own metadata (`Configuration`/
  `ConfigurationIndex` fields) - no log entries needed at all.
- `raft.BootstrapCluster` itself refuses with `ErrCantBootstrap` once
  `HasExistingState` is already true.

These compose cleanly with zero new runtime code path: a new
`raftnode.SeedSnapshot(cfg, state)` pre-populates an empty `-data-dir`
with a real raft snapshot (the payload, plus a single-voter
`Configuration` naming that node) using `raft.FileSnapshotStore.Create`
directly. The *next normal start* then takes the existing
`hadState == true` branch automatically - `Bootstrap()` never runs, so
`ErrCantBootstrap` can never trigger. One easy-to-miss detail: `raftd`'s
own `HasExistingState` wrapper checks for the BoltDB log/stable-store
*file's* existence before ever consulting the snapshot store at all -
so `SeedSnapshot` also opens (creating) and immediately closes that
file, even though it writes no log entries into it, purely so that
check sees a real file on the next start. Skipping this would make a
seeded snapshot silently invisible.

`FSM.Persist` already writes raw `proto.Marshal(FSMSnapshotState)` with
no extra framing, so the seeded snapshot's payload file is
byte-for-byte what a real in-process snapshot would write.

**Scope: single-node restore only, for v1** - matching Tier 1's own
single-node-bootstrap-only scope. Restoring a multi-node cluster means
restoring one node, starting it, then `-join`-ing the rest normally;
the existing join/`InstallSnapshot` replication path is unmodified and
already covered by its own tests.

### File format

`internalpb.ConfigArchive` wraps an unmodified `FSMSnapshotState`
payload in a small envelope: `format_version` (this envelope's own
schema version, independent of `FSMSnapshotState`'s field additions),
`exported_unix`/`node_id`/`applied_index` (provenance only, never
consulted for correctness), `fsm_snapshot_state` (the raw payload), and
`checksum` (SHA-256 of that payload) - verified by `-restore` before
anything is touched, independent of raft's own on-disk CRC64, since a
portable archive may sit outside `raftd`'s data directory, on removable
media, for a long time before being read back.

### CLI shape

`raftd -export <path>`: requires `raftd` already running, dials
`-socket` exactly like `-join` already does, calls `ExportState`, wraps
the response in a `ConfigArchive`, writes it out. Non-destructive - no
confirmation phrase, unlike every other one-shot mode in this file.

`raftd -restore <phrase> -restore-file <path>`: a new
`restoreConfirmPhrase = "yes-restore-raft-state"`, exact-match-or-
nothing-happens, mirroring `resetConfirmPhrase`'s own posture (every
service here runs under `daemon(8)`'s auto-restart supervisor, so a
bare boolean flag left in `rc.conf` would repeat the action on every
respawn). Verifies `format_version` and checksum, then calls
`SeedSnapshot`. Refuses outright against a data dir that already has
raft state - run `-reset` first, same as any fresh-bootstrap
precondition.

`raftd -restore-dry-run <path>`: runs the exact same archive
validation `-restore` does (`format_version`, checksum, via a shared
`loadConfigArchive` helper) and prints a summary of what it contains -
node ID, export timestamp, applied index, and record counts per type -
without calling `SeedSnapshot` or touching `-data-dir` at all. Like
`-export`, needs no confirmation phrase, since it makes no changes.

## Consequences

**API keys carry two distinct, permanent limitations here - not one,
and not bugs to work around:**

1. *Restore is a point-in-time rollback, for all state, not just API
   keys.* Anything created after the archive's own export timestamp -
   a new VM, a new key, whatever - is wiped out by a later restore from
   that archive, because restore replaces the FSM's entire state
   wholesale with the archive's contents. An operator restoring an
   older archive onto a node is trusting that they know it's safe to
   discard everything since - the same "caller decides" trust model
   `-reset` already assumes, not something this feature can verify on
   their behalf.
2. *API keys are hash-only, independent of the above.* Per ADR-0023,
   Apiary only ever stores a SHA-256 hash of an API key - the raw key
   string is shown exactly once at creation and never persisted
   anywhere. Export/restore only ever carries that hash record, in
   either direction. Restoring correctly brings back a key's ability to
   keep validating a raw value the operator already saved elsewhere,
   but it can never hand back the raw string itself, even for a key
   that round-trips through an export and restore perfectly - that
   string was never in Apiary's own state to begin with.

Combined, a key created after the last export and then lost (never
recorded by the operator) before the next restore is unrecoverable two
ways over: the restore erases its hash record entirely (limitation 1),
and even a hash record that did survive could never yield the original
raw value anyway (limitation 2).

Restore is single-node-only for v1 (see above) - a real limitation for
an operator wanting to restore directly into a multi-node cluster in
one step, though the two-step "restore one node, then `-join`" path
covers the same outcome using already-tested machinery.
