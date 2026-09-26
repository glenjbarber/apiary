# ADR-0124: Read-Only Offline Raft Status (`raftd -status`)

## Status

Accepted

## Context

`raftd` had no way to report what a node knows about its own persisted Raft state
without starting a server. The only modes were:
- The server itself (`raftd` with no flags)
- `-reset` (destroys state)
- `-export` / `-restore` (operational formats for backup/migration)

Answering "what does this node's Raft state say" therefore required `raftd` to be
running and answering RPCs — which is exactly the situation where a node cannot
answer, since `raft.db` is locked by BoltDB for `raftd`'s entire lifetime, and the
membership read is what fails first when a node is suspected of being in a bad
state.

Operators needed a diagnostic tool that could:
- Run against a **stopped** node (no lock contention)
- Report **persisted** state only (term, log bounds, membership)
- Make **no network calls**, **take no votes**, **write nothing**
- Be safe to point at a production data directory without side effects
- Distinguish "not observed" from "observed as zero" for every field

## Decision

Add `raftd -status` (and `raftd -status-json`), a read-only, one-shot command
that opens the node's own `raft.db` through BoltDB's read-only mode and reports
the persisted current term, the log bounds (first/last index), and the persisted
membership — then exits.

### What it reads, and how

| field | source | note |
| --- | --- | --- |
| **Current term** | BoltDB key `CurrentTerm` (mirrored from hashicorp/raft's unexported `keyCurrentTerm`) | Missing = "never held Raft state", not an error |
| **Log bounds** | `BoltStore.FirstIndex()` / `LastIndex()` | `LastIndex` includes uncommitted entries; not a commit index |
| **Membership** | Newest `LogConfiguration` entry in the log, falling back to latest snapshot's configuration | Chosen by **index**, so a compacted log cannot silently report stale membership |

**Membership source is reported explicitly** (`log`, `snapshot`, or `unknown`), so a
reader can tell a fresh log entry from a snapshot-carried value. A node with no
configuration anywhere reports membership as `unknown` — **never as an empty
cluster**.

**Leadership is explicitly reported as unrecoverable.** Raft never persists who
holds leadership, so no offline read of any kind can answer it. The output says
this clearly rather than omitting the field (which reads like an oversight).

### Safety guarantees

- **Read-only end to end**: BoltDB opened with `ReadOnly: true` (shared lock only).
- **No MkdirAll**: Never creates the data directory — a half-initialized node
  must still work, and a tool that silently `mkdir -p` would leave a
  plausible-looking empty state behind.
- **Snapshot metadata read directly**: `FileSnapshotStore` is deliberately **not**
  used, because constructing it performs a `MkdirAll` and a create-then-delete
  permissions probe — which would violate read-only.
- **Bounded lock wait**: `offlineOpenTimeout = 2s`. If `raftd` holds the lock, the
  open times out and returns `ErrOfflineRaftdRunning` rather than blocking.
  Callers should stop `raftd` first, or query the node over its live gRPC socket.
- **Bounded log scan**: `offlineMaxLogScan = 10000` entries walked backwards.
  Hitting the cap is reported in `MembershipNote`, never silently truncated.

### Output formats

**Text** (default, human-readable):
```
node:            brood.lab3.home.arpa
data dir:        /var/db/apiary
term:            27 (persisted, read-only)
log bounds:      indexes 1..124 (last index includes uncommitted entries - this is not a commit index)
membership:      4 member(s), read from the log's newest configuration entry at index 118
  * brood.lab3.home.arpa        Voter      10.90.0.94:17600
    drone.lab3.home.arpa        Voter      10.90.0.95:17600
    buzz.lab3.home.arpa         Voter      10.90.0.96:17600
    sting.lab3.home.arpa        Voter      10.90.0.97:17600

note:            this is an offline read of persisted state, not a live query.
                 raft never persists its leader, so who currently holds leadership
                 cannot be answered from disk at all - only a running raftd can say.
                 * marks this node itself.
```

**JSON** (`-status-json`): Machine-readable shape mirroring the Go struct
field-for-field, including `*_observed` booleans so a script can distinguish an
unread value from a real zero.

### No confirmation phrase required

Unlike `-reset`, `-export`, or `-restore`, `-status` changes nothing. It is
deliberately a no-op if left sitting in `rc.conf`'s `apiary_raftd_args` the way
`-reset` would be catastrophic.

## Consequences

- Operators can diagnose a node's persisted Raft state **without starting it**,
  which is exactly when they need to know.
- The tool reports what is **actually on disk**, not what a live node would
  claim — membership comes from the newest log configuration entry (or snapshot
  fallback), chosen by index, matching Raft's own recovery logic.
- Leadership is correctly absent: an offline read cannot answer it, and the
  output states this explicitly.
- Safe for monitoring/scripts: bounded timeout, read-only, no side effects,
  machine-readable JSON.
- No new RPC, no proto change, no live dependency.

## Alternatives considered

**Reuse `raftd -export` for offline status.** Rejected: `-export` is an
operational backup format (envelope with version, provenance, checksum) meant for
restore/migration. It requires a leader, writes a file, and its membership comes
from the live FSM — not the same thing as "what is on this node's disk right
now."

**Add a `Status` RPC to the internal `RaftInternal` service.** Rejected: that
requires `raftd` to be running and taking the lock — the exact situation this
tool avoids.

**Make the live `Status` RPC include persisted term/membership.** Rejected: the
live `Status` already reports `raft_state`, `raft_leader_id`, etc. from a live
node. Conflating live and persisted views would blur the distinction that makes
diagnosis possible (a live node can report "I am follower, leader is X" while
its disk says term 5; a stopped node's disk might say term 27 — both are true
and both are useful).

**Use `FileSnapshotStore` for snapshot reading.** Rejected: it is not read-only
(`MkdirAll` + permissions probe). The snapshot metadata is read directly instead,
decoded into `raft.SnapshotMeta` — the payload format stays Raft's.

**Return empty membership on no-configuration.** Rejected: an empty cluster is a
valid state; "unknown" is a different state (no evidence). Collapsing them is
the pass-by-default failure ADR-0056 exists to eliminate.