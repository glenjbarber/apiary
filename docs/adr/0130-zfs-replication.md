# ADR-0130: ZFS dataset replication between Combs

**Date:** 2026-09-26
**Status:** Proposed
**Author:** Goose (subagent)

## Context

ADR-0127 catalogued Sylve's feature set and ranked **ZFS dataset
replication** as Phase 2, item 2, alongside the backup system. Sylve's
own position is that replication is early access, behind a feature flag,
and intended to be used alongside independent backups rather than
instead of them.

Apiary today has three storage-replication-adjacent things and no
fourth:

- `internal/hast` manages HAST resources for individual workloads. A
  dataset+file VM's `disk.img` is replicated by a HAST resource named
  `hast-vm-<id>` (ADR-0026, ADR-0028); a jail's root filesystem is not
  replicated at all.
- `internal/zfs` shells out to `zfs(8)` and already exposes `Send`,
  `Receive`, `CreateSnapshot`, `ListSnapshots`, `RollbackSnapshot`,
  `Clone`, and the dataset property getters. `Send` streams live
  (ADR-0089), and `internal/manager/server.go` already pushes one
  Comb's `zfs send` stream to a peer's streaming RPC
  (`PushJailTemplateTo` / `ReceiveJailTemplate`).
- `internal/cluster/simulate.go` turns direct observations of those
  HAST resources into `RecoveryVerdict` values, including
  `replica_unobserved`, which means *a query was attempted and returned
  nothing usable* — not healthy, not failed.

What is missing is any way to keep a *dataset* current on a peer Comb
for disaster recovery: today a lost Comb means rebuilding a workload
from nothing, unless the operator has an out-of-band copy. That is the
gap this ADR closes.

Scope note on vocabulary: in Apiary's code a **Cell** is a workload — a
VM or a jail (`TraceCellPathRequest.cell_id` resolves "as either a VM
or a jail", and `internal/coverage` says "this comb owns N cell(s)").
ADR-0127's glossary table calls a Cell a failure-domain grouping of
Combs; that is wrong relative to the code, and this ADR uses the code's
meaning.

## Relationship to existing HAST support

HAST and ZFS dataset replication are different mechanisms with
different failure properties. Conflating them is the main way this
feature goes wrong, so the boundary is stated first.

| | HAST (`internal/hast`) | ZFS replication (this ADR) |
|---|---|---|
| Granularity | One block device (a GEOM provider) shared by exactly two nodes | A filesystem dataset, one file tree, one direction, N hops |
| Transport | `hastd(8)`'s own TCP protocol, separate from managerd and raftd | A `zfs send` stream carried over the existing managerd mTLS channel |
| Failure detection | `hastd` maintains a live role/status (`complete`, `degraded`, `unknown`) and dirty-extent counters; a dead peer shows as `degraded` | A generation counter. A stopped stream is an interrupted `zfs send`; there is no background liveness to poll |
| Split-brain handling | Delegated entirely to `hastd` + `hastctl role`; Apiary only runs `SetRole` and reports what `hastctl` said | Must be built: a raft-granted lease with a monotonic generation, enforced by the receiving node |
| Consistency | Block-level and guest-visible; a torn guest write is visible on both sides | Filesystem-consistent at send time, but not application-consistent |
| Time to recovery | Seconds — promote the secondary, the disk is already there | Minutes to hours — recover the dataset, then re-materialize a Cell from it |
| What it is for | Redundancy for a *running* workload, with a manual cutover | Disaster recovery of a *Comb*, and a warm copy of *cold* data |

HAST is **not** ZFS-aware: the provider dataset is a plain file or md(4)
vnode device, confirmed live on this project's FreeBSD 16.0-CURRENT
(ADR-0026), precisely because `hastd` cannot read its own metadata
against a zvol-backed provider. So HAST gives Apiary no dataset-level
copy to replicate, no snapshot stream, and no third target.

**Choose HAST** when a workload must keep running through a single
Comb's loss and the second copy sits on a node reachable over a
low-latency LAN: VMs with a `replica_node_id`, where `MigrateVM` is a
real, safe cutover (ADR-0028).

**Choose ZFS replication** when the goal is surviving the *loss of a
whole Comb or Cell* — a second site, a cold standby, a warm copy of a
dataset that has no live replica — or when the workload is a jail, which
HAST does not cover at all. Replication produces a copy, not a running
workload, and recovery from it is an explicit operator action.

They are complementary and neither replaces the other. A VM may have
both `replica_node_id` and a replication policy; the two are
independent fields on independent code paths, and neither reconciler is
aware of the other.

## Decision

Replication is a raft-governed, lease-fenced, scheduled, one-way
`zfs send`/`recv` of a **Cell's dataset** from a designated source Comb
to a designated target Comb, for disaster recovery. It is a *copy*,
never a live replica and never a backup.

### 1. Replication unit

Replicated:

- `<zfs_base>/<jail-id>` — a jail's root dataset. This is the only
  redundancy option a jail has at all.
- `<zfs_base>/<vm-id>` — a dataset+file VM's dataset, which holds
  `disk.img` (`diskImageName` in `internal/cluster/reconciler.go`) plus
  its snapshots (ADR-0090). Only for VMs with no `replica_node_id`.

Never replicated by this subsystem:

- `hast-vm-<id>` and every other `hast-*` provider dataset. Those hold
  a node-local block image whose coherence is `hastd`'s business; a
  `zfs send` of one captures a byte image that is not atomic with
  respect to guest writes, and HAST's second copy already exists. An
  operator wanting a third independent copy removes
  `replica_node_id` (dataset+file disk) or uses ADR-0131.
- `templates/<name>` — immutable, hash-verified, and already copied
  cross-node on demand by ADR-0089's push. Continuous replication
  would duplicate that mechanism for no gain.
- The ISO store (`iso_dir`, ADR-0017), any dataset outside
  `zfs_base`, the pool root, the OS dataset, and the jail mount base.
  A policy naming any of these is rejected at command-apply time.
- Anything not named by a `ReplicationPolicy`. There is no "replicate
  everything under `zfs_base`" mode.

The target dataset name is the policy's `target_dataset`, defaulting to
the source's base-relative name; the two Combs' `zfs_base` values need
not match.

### 2. Mechanism

Snapshot naming and generation. Each policy owns a monotonically
increasing uint32 `generation`, replicated in raft and never reused. A
run snapshots the source as `<dataset>@apiary-repl-<gen:08d>`, sends it,
and advances `generation` only after the target's ack is committed. The
last `retention` (default 3) generations are kept on the source; the
target keeps the snapshots it received, which carry the same names. A
snapshot the target has not acked is never destroyed, even if it is
older than `retention` — destroying it would make the next incremental
send impossible and force a full resend.

Transport. The source runs `zfs.Send` and pushes the reader to the
target over the existing managerd peer client, exactly the shape of
`PushJailTemplateTo` (ADR-0089). The target's handler runs `zfs.Receive`
with the stream piped in via `io.Pipe` — never buffered in memory. This
reuses the colony's existing mTLS (`tls_cert`/`tls_key`,
`internal/tlsdial`, ADR-0115's peer hostname map) and introduces no
second transport, no second TLS configuration, and no new listening
port.

Incremental vs. full. The first successful receive is
`zfs send -I <guid>@<n>`. Every later run is
`zfs send -i <last_acked_guid> <dataset>@apiary-repl-<gen>`. If the
incremental send fails with a "previous snapshot not found" class
error, the run escalates **once** to a full `-I` send and increments
`full_resync_count`, which is displayed. A full resend is an event the
operator should see, not a quiet recovery.

`zfs receive` runs with `-u` (do not mount) so the source's mountpoint
path — a different absolute path on the target, if it exists at all —
is never used. The target sets its own mountpoint afterwards from its
own `jail_mount_base`-derived configuration.

Staging on first receive. A first full receive lands in a staging
dataset `<zfs_base>/.apiary-replication/<policy-id>`; on success the
target renames it to `target_dataset` and sets the mountpoint.
Subsequent incremental receives go straight into the renamed dataset.
This keeps a failed first receive from leaving a half-populated dataset
occupying the final name, where it would be indistinguishable from a
real one.

Resume tokens and partial receives. A `zfs send` interrupted mid-stream
leaves the destination dataset in a resumable receive state, addressable
by `zfs get -H -o value receive_resume_token <dest>`. The policy:

- persists the token on the **target node** in a local file
  (`/var/db/apiary/replication/<policy-id>.token`) as that node's own
  source of truth, and returns a copy in the ack for display. The token
  is never the source of truth and is never required from raft.
- on the next run, if the target observed a live resume token, resumes
  with `zfs send -t <token>` rather than restarting.
- if the token is missing or rejected (destination dataset destroyed,
  token expired), restarts the incremental send from the **last acked
  snapshot**, not from the origin full send. A failed incremental is a
  bandwidth and time cost, not a data-loss event.
- if the last acked snapshot is itself gone, falls back to a full `-I`
  send and bumps `full_resync_count`.

Detection is an observation, not an inference: the target reports
`replica_partial_receive` when it can read a non-`-` resume token, which
is a *confirmed* not-current state, distinct from both "current" and
"could not check".

### 3. Authoritative direction and split-brain

Every `ReplicationPolicy` is unidirectional: it has exactly one
`source_node_id` and one `target_node_id`. Only the source may send,
and the target refuses any stream whose metadata names a source other
than its own current view of the policy's source.

Authority to send is a raft-granted **lease**, not a configuration
field:

```
ReplicationLease {
  policy_id, holder_node_id, generation, expires_unix
}
```

- Only the current raft leader grants leases, via
  `AcquireReplicationLease`, which is a compare-and-swap applied
  through raft like every other FSM command.
- `generation` increases on every grant and is strictly monotonic. A
  stream carrying a generation lower than the target's recorded one is
  rejected with a fence error. This is the actual split-brain fence.
- The lease is renewed at `ttl/3` and expires. An expired lease
  authorises nothing.
- Creating a policy whose source equals the target of another live
  policy on the same dataset is rejected at apply time. A→B and B→A
  cannot both exist for one dataset.

**Tie-break rule.** Two Combs cannot both believe they are the source,
because the source is a raft-committed fact, not a discovery. When the
lease lapses — a Comb was lost, or a partition left two Combs unsure —
the cluster does *not* auto-promote. The operator runs an explicit
`RepointReplicationPolicy` command, which is a single raft commit that
changes the source, increments the generation, and invalidates the old
holder's lease. If two operators race, raft's own log order decides,
the first committed repoint wins, and the second command is rejected
because the generation it carried is stale. This is deliberately
Apiary's standing no-automatic-scheduling stance applied to storage: a
human moves the source, and the same human accepts the RPO.

### 4. Encryption

In transit: the colony's existing managerd mTLS. The replication stream
rides the same authenticated, per-peer channel as ADR-0089's template
push. No plaintext fallback, and a policy cannot be created against a
peer that does not present a valid certificate.

At rest: optional native ZFS dataset encryption on the **target**
dataset only, which is the interesting half — the DR copy is the one
that sits on a second site, and a plaintext `zfs send` can be received
into an encrypted dataset. The source's dataset is left exactly as it
is.

Keys never touch raft. The operator places a passphrase at
`/etc/apiary/replication.key` (root-only, mode 0600, matching
`internal/raftdconfig`'s `InternalToken` file handling) on each
endpoint; both ends read their own local copy. Raft records only a
non-secret `key_id` and a **verifier** — a random 32-byte value minted
at setup time, safe to replicate and safe to display — so a mismatch is
detectable without revealing or transporting the key. A verifier
mismatch reports `key_mismatch`, refuses to start the run, and never
falls back to an unencrypted receive.

This follows existing precedent rather than inventing a rule:
`internal/origincert` "deliberately has no raft dependency" and its
`InventoryEntry` excludes tokens, private keys, and CSRs;
`internal/raftdconfig` rejects newline injection into secret-bearing
fields and keeps `InternalToken` out of raft. A key placed in raft
would be recoverable by anyone who can read a `-export` archive
(ADR-0051) — and those archives are designed to sit on removable media
for a long time.

If the *source* dataset is already encrypted, `zfs send` needs the key
loaded in the local keyring. Apiary does not load it; the operator
unlocks the dataset out of band, exactly as they would with `zfs send`
by hand.

### 5. Bandwidth, throttling, and scheduling

- `rate_limit_bps` (default 64 MiB/s, `0` = unlimited) applied as a
  token bucket in the source's push path, per direction, before the
  reader is handed to the stream. The cap exists so replication cannot
  starve `raftd`'s transport or managerd's own gRPC, which share the
  same NIC.
- `schedule` restricts runs to a window (`every` plus an optional
  `at`/`until` local-time pair), defaulting to off-peak. Outside the
  window a due run is deferred, not dropped, and the policy shows
  `deferred` with the next eligible time.
- `max_run_seconds` bounds one run so a single policy cannot occupy the
  link indefinitely; the remainder is picked up next window.

Interaction with the maintenance wave planner (ADR-0119): replication
is a bandwidth consumer, not a maintenance action, and the planner
stays read-only. The rule runs in both directions:

- Before starting a run, a policy defers if a planned or in-progress
  maintenance wave covers either endpoint. The deferral is reported,
  not silent.
- The planner's page additionally shows, as read-only context, which
  policies are currently active on each Comb, so an operator sequences
  deliberately.
- The planner must never treat a fresh replication replica as a reason
  a Comb is "safe to maintain". Replication gives a *recoverable copy*;
  it does not give a running workload. Only HAST evidence supports
  cutover-shaped claims, and ADR-0119 already declines to claim a
  numeric RPO from either.

Quorum-sensitive operations: replication is pure data plane. It changes
no raft membership, and it is never a quorum blocker (ADR-0118,
ADR-0056). It must, however, defer rather than run while ADR-0128 guest
migration is transferring the same dataset — two concurrent
`zfs send`s of one dataset to two targets is exactly the "replication
must not fight other work" case. The same lock covers a manually
triggered run.

### 6. Evidence and honesty

Replication reports its own observation vocabulary, separate from the
HAST one, so a HAST answer can never be read as a replication answer
and vice versa. The target's answers are gathered by a node-local RPC
and are never forwarded through raft — the same discipline ADR-0119
established for HAST.

`ReplicationVerdict`:

| Verdict | Meaning |
|---|---|
| `replica_current` | Target was queried, holds the policy's last acked generation, and reports no live resume token |
| `replica_stale` | Target was queried and demonstrably holds an *older* generation |
| `replica_partial_receive` | Target was queried and reports a live `receive_resume_token`: a confirmed interrupted receive, not a completed one |
| `replica_unobserved` | A query was attempted and returned nothing usable |
| `replica_unprotected` | No policy exists for this dataset |
| `replica_disabled` | A policy exists but is disabled by the feature flag or a key verifier mismatch |

Two rules are load-bearing:

1. **`replica_unobserved` keeps the exact meaning it has today** in
   `internal/cluster/simulate.go`: a query was attempted and produced no
   usable answer. Not healthy, not failed, not in-sync, not
   out-of-sync. The new verdicts never collapse it into either
   neighbour.
2. **A stale replica never masquerades as healthy.** "The target
   answered" is not "the target is current". A target that answers
   promptly and reports an old generation is `replica_stale`, full
   stop. Freshness is the generation comparison, and only the
   generation comparison.

Replication contributes **nothing** to
`internal/recovery.ClassifyQuorum`. That function accepts `QuorumFact`s
about voters; a policy state is not a quorum fact, and a stale replica
must never add a `why_not` quorum blocker (ADR-0118, ADR-0122,
ADR-0056). What a stale replica *may* do is mark the Cell's secondary
evidence as not-current in the Operational Continuity scorecard
(`internal/coverage`) — a presentation fact about recoverability, not
a statement about consensus.

Lag is reported as a **generation count and the last acked generation's
age**, both of which are observed. Apiary does not convert lag into
seconds-of-data-loss, and does not infer an RPO number, for the same
reason ADR-0119 declines to turn `hastctl`'s dirty-extent text into
one.

### 7. Disaster recovery

A whole Comb is lost. Nothing is automatic; the operator runs a Flight
Plan:

1. Confirm the loss (existing `raftd` membership handling; a
   quorum-safe restart is ADR-0116).
2. On the surviving target Comb, read the replication evidence. Only
   `replica_current` on the last acked generation means the copy is
   usable. `replica_stale` means recovering a known-older generation,
   and the Flight Plan must say so out loud.
3. Repoint the policy: `RepointReplicationPolicy` makes the old target
   the new source, so the surviving copy immediately begins feeding a
   replacement Comb instead of sitting still.
4. Re-materialize the Cell. For a jail, point the jail at the
   recovered dataset. For a VM, clone or restore from the recovered
   snapshot (ADR-0090) and set `node_id` to the recovered Comb. This
   is a deliberate operator step, matching the same reasoning that made
   `MigrateVM` refuse to run without a confirmed HAST secondary
   (ADR-0028).
5. Destroy the policy's snapshots on the new source only after the
   recovery is confirmed working.

**Boundary against ADR-0131 (backup).** Replication is not a backup and
must never be counted as one. It keeps at most `retention` generations
on exactly one peer; a logical mistake — a bad write, a deleted file, a
corrupted dataset — replicates faithfully, so replication faithfully
propagates damage; it has no manifest and no atomic multi-dataset
generation; and it is lost by the same pool-level events that take the
source. Backup is the recoverability evidence for the Operational
Continuity Scorecard; replication is the fast leg of Comb-loss recovery
that gets a workload back in minutes rather than hours. The operator
guidance — and the UI text — says to use both.

### 8. Operation under a degraded or quorum-less Colony

Replication **may continue** when raft has no quorum, under three
constraints:

- Only an already-granted, unexpired lease authorises a run. No new
  lease can be granted without a leader.
- The acked generation is written to a **node-local journal** on the
  source before the ack is treated as durable, and reconciled into raft
  when consensus returns. A generation the source believes was acked
  but raft never saw is re-acked, not re-sent, on recovery.
- No promotion, no repointing, no policy mutation, no reversal. Every
  authority change requires consensus, by definition.

The reasoning: during a quorum outage the highest-value traffic is
keeping the DR copy current, and the fence that makes that safe is the
lease plus the monotonic generation, not consensus. Consensus is
required to change *authority*; it is not required to move bytes under
an authority that already exists and has not lapsed. If the lease has
lapsed, the run stops — an expired lease is the honest signal that
nobody can say who the source is.

### 9. Early-access gate

Matching ADR-0127, and Sylve's own posture: replication is behind an
`experimental_replication` boolean in Machine Configuration (default
`false`), requires at least 3 raft voters, and its UI is not rendered
at all when the flag is off. A flag that gates *visibility* rather than
*behaviour* would be useless — a policy must be impossible to create
while the flag is off, enforced at command-apply time, not merely
hidden.

## Rejected alternatives

1. **Extend or reuse HAST for everything HAST does not cover (jails,
   third sites, cold DR).** HAST is a two-node block-device protocol
   with its own transport, its own roles, and no knowledge of ZFS: its
   provider must be a plain file or md(4) device, not a zvol, on this
   project's own FreeBSD build (ADR-0026). It cannot take a third
   target, it cannot carry dataset metadata or several datasets in one
   transfer, and it would make every dataset-replication request a
   per-resource GEOM resource. Reusing it would also import HAST's
   split-brain delegation into a system that needs explicit fencing.
2. **Continuous / synchronous replication (a `zfs send` per guest
   write, HAST's continuous-mode shape).** It has no bounded
   bandwidth, cannot tolerate a slow or absent target without blocking
   the guest, makes the source Comb the bottleneck for every workload
   it protects, and would fight the maintenance wave planner and the
   shared management link for the same NIC. It also cannot be made
   safe without a fence the Colony does not currently own.
3. **Encrypt the replication key in raft** (a KEK-wrapped key, or the
   plaintext key in the policy record) so provisioning is zero-touch. A
   raft-archived secret is recoverable by anyone who can read a
   `-export` archive, and ADR-0051 archives are meant to sit on
   removable media. `internal/origincert` and `internal/raftdconfig`
   both already keep secrets out of raft deliberately; replicating a
   key through the consensus log would be the single worst possible
   place to put one. The out-of-band passphrase is the honest cost, and
   the feature is behind an early-access gate anyway.
4. **Automatic promotion of a replica to source when a Comb is lost.**
   This is precisely the split-brain this ADR exists to prevent, and it
   cannot be made safe without an external witness. It would also be
   the first automatic failover in Apiary, contradicting the project's
   standing no-automatic-scheduling stance. Recovery stays an operator
   runbook.
5. **Let each node decide whether it is the source**, with no lease —
   "push if you have the dataset". Two Combs partitioned from raft
   would both conclude they are the source, and the loser's changes
   would overwrite the winner's on the next incremental. Direction
   must be a committed fact, not an inference.
6. **Treat replication as the recoverability story** for the
   Operational Continuity Scorecard, dropping the backup system.
   Replication has no manifest, no point-in-time protection against a
   bad write, and one peer. That is ADR-0131's job; the two are
   designed to be used together and their scopes are stated
   separately on purpose.

## Consequences

### Positive

- A lost Comb is now recoverable in minutes for jails and dataset+file
  VMs, rather than by rebuilding from nothing.
- The mechanism reuses the existing `zfs send` push path, mTLS
  transport, raft command path, and node-local evidence discipline — no
  new port, no new TLS trust, no new consensus primitive.
- Direction is enforced by a monotonic, raft-granted lease that the
  receiving node checks, so the fence does not depend on the source
  being reachable or honest.
- Replication's evidence vocabulary is separate from HAST's, so the two
  can never be conflated in a status line, a scorecard, or a why-not.
- Replication is explicitly a non-blocker for quorum, so a degraded
  Colony keeps making its DR copy current under an existing lease.
- Fronted by an early-access flag, so nothing changes for an operator
  who does not opt in — including a policy that cannot be created while
  the flag is off.

### Negative

- A replicated dataset is a second copy of the same data; it doubles
  storage cost for the covered Cells and the operator has to budget for
  it.
- Recovery is still manual and still lossy by exactly the RPO the
  operator chose. This feature adds a way to lose data quietly (a stale
  replica believed current) as well as a way to save it.
- The feature adds a real interaction surface: the maintenance planner,
  ADR-0128 migration, the shared NIC, and the lease all have to be
  reasoned about together. A bug in any one of them can stall or
  double-send.
- The out-of-band key file is a genuine operational burden, and a
  verifier mismatch is a failure mode an operator will hit.
- The first receive stages and renames a dataset, which is more moving
  parts than a straight `zfs recv`, and the staging dataset needs its
  own cleanup on abandoned policies.
- Because the evidence is generation-based, an operator cannot see a
  numeric RPO, only how many generations behind the target is — which
  is the honest answer but a less satisfying one.

## Implementation notes

### Raft state (raft-replicated; no secrets, no key material)

New messages in `api/internalpb/state.proto`:

```
ReplicationPolicy {
  string id; string name;
  string cell_kind;          // "vm" or "jail"
  string cell_id;
  string source_node_id;     // the single authoritative direction
  string target_node_id;
  string target_dataset;     // base-relative; default = the cell's own
  uint32 retention;
  uint32 generation;         // monotonically increasing, never reused
  uint32 last_acked_generation;
  uint64 rate_limit_bps;
  string schedule;           // e.g. "nightly", "every 30m"
  bool   encrypt_target;
  string key_id;             // non-secret id only
  bytes  key_verifier;       // non-secret; compare, never a key
  bool   enabled;
}

ReplicationLease {
  string policy_id; string holder_node_id; uint32 generation;
  int64  expires_unix;
}
```

Fields added to `FSMSnapshotState` (10 and 11 are the next free numbers
after `restart_records = 9`):

```
map<string, ReplicationPolicy> replication_policies = 10;
map<string, ReplicationLease>  replication_leases   = 11;
```

They belong in `FSMSnapshotState` for the same reason `restart_leases`
does (per its own comment in the proto): a raft snapshot restore or an
`raftd -restore` seed (ADR-0051) must not silently drop live policies
and reopen a window where two Combs both think they are a source.

Raft state keys, for a key/value view of the FSM:
`replication/policies/<policy-id>`, `replication/leases/<policy-id>`.

New commands: `CreateReplicationPolicy`, `UpdateReplicationPolicy`,
`DeleteReplicationPolicy`, `AcquireReplicationLease`,
`RenewReplicationLease`, `ReleaseReplicationLease`,
`RepointReplicationPolicy`, `RecordReplicationAck`.

Failure modes: `AcquireReplicationLease` on a policy whose lease is
live and held by another node is rejected at apply time, so the fence is
enforced by the state machine, not by a caller-side check that could
race. `RepointReplicationPolicy` carries the generation it expects; a
stale one is rejected. `RecordReplicationAck` with a generation lower
than `last_acked_generation` is rejected, so a late-arriving ack from a
run whose lease lapsed can never roll the counter backwards. Deleting a
policy revokes its lease in the same commit.

### `internal/zfs` additions

`Receive` currently takes `(ctx, destName, r)` and is used by ADR-0089's
template push. Do not change its signature; add siblings:

- `ReceiveInto(ctx, dest string, r io.Reader, opts ReceiveOptions)
  error`, with `NoMount bool` (`-u`) and `Force bool` (`-F`), plus the
  existing validation from `m.path` and the same stderr-surfaced error
  format (`zfs receive <dataset>: <stderr>`) so failures stay
  diagnosable.
- `ReceiveResumeToken(ctx, dest) (string, error)` reading
  `zfs get -H -o value receive_resume_token`.
- `SendIncremental(ctx, from, to string) (io.ReadCloser, error)` for
  `zfs send -i from to`, and `SendResume(ctx, token string)`.
- `SendFrom(ctx, dataset, snapshot string) (...)` for the `-I` full
  send.

`Send` already returns a live-streaming `io.ReadCloser` whose `Close`
waits on the subprocess and reports a non-zero exit — the replication
pusher must `Close` it and treat a close error as a failed run, not
discard it.

Failure modes: a `zfs send` whose reader is abandoned leaves a child
process; always `Close`. A `zfs receive` interrupted mid-stream leaves
the dataset resumable, and the next receive on that dataset must be a
`-t` resume or a `-F` overwrite, never a bare receive, which ZFS
rejects.

### managerd RPC handlers

In `api/rpc/manager.proto`:

- `rpc StreamReplication(stream ReplicationChunk) returns
  (ReplicationStreamResponse)` — the target-side handler, modelled
  directly on `ReceiveJailTemplate`: the first message must carry
  `ReplicationMetadata` (policy id, from/to generation, resume token or
  `-`, whether this is a full or incremental send), every later message
  carries a chunk, and chunks are piped straight into `zfs receive` with
  an `io.Pipe`. A first message without metadata is a protocol error.
- `rpc GetLocalReplicationStatus(GetLocalReplicationStatusRequest)
  returns (GetLocalReplicationStatusResponse)` — node-local, never
  forwarded through raft, returning the target's own view: held
  generation, resume token presence, dataset existence, observation
  time, and raw `zfs` text.
- `rpc ListReplicationPolicies` / `GetReplicationPolicy` for the
  frontend, plus `PutReplicationPolicy` and `DeleteReplicationPolicy`
  wrappers over the raft commands.

In `internal/manager/server.go`, the `peers` interface gains
`PushReplication(ctx, addr string, meta *ReplicationMetadata,
r io.Reader) (*ReplicationAck, error)`, mirroring the existing
`PushJailTemplate(ctx, addr, name, r)`.

Failure modes: the target rejects any stream whose `meta.HolderNodeID`
disagrees with its current raft view of the lease holder, or whose
generation is below its recorded one — a fence error, returned as a
typed response, not a dropped connection. A target with no ZFS support
configured returns the same explicit "this node has no ZFS support
configured" error the template handlers use.

### Reconcile loop (`internal/cluster/replication.go`)

A new `ReplicationReconciler`, ticked from the existing
`reconcile_interval` (nodeconfig), that per tick: refuses to run when
the `experimental_replication` flag is off; checks the maintenance wave
planner's current state for both endpoints and defers; checks the
ADR-0128 migration lock for the same dataset; verifies an unexpired
lease; checks the rate-limit window; and if all clear, runs one
generation synchronously in a bounded goroutine — snapshot, push, ack.
It reads through a `replicationZFSManager` interface in the same
`datasetManager` style `internal/cluster/reconciler.go` already uses, so
tests substitute a fake and never touch a real pool.

Failure modes: a failed run leaves the generation un-acked, writes the
failure reason into the policy's own status (not into the workload's),
and does not advance the counter. A run whose lease expired mid-flight
aborts and cleans up the staging dataset. A stuck run past
`max_run_seconds` is killed and reported as `aborted`, never as
`current`.

### Frontend surface

A Replication page listing every policy with its direction, target
Comb, last acked generation, generation lag, `ReplicationVerdict`
badge, `full_resync_count`, and the raw `zfs` evidence text — never a
derived seconds-of-loss figure. Each Cell detail page shows replication
evidence next to HAST evidence under explicitly different headings,
with the HAST-only `MigrateVM` affordance unchanged. The DR runbook
from section 7 is linked from the policy row. The whole page is not
rendered at all unless the flag is on.

Failure modes: an unrendered page must not be the only enforcement of
the flag — the command-apply check in the FSM is. A policy whose target
Comb is no longer a member shows `target_missing` and is not silently
dropped.

## Test plan

On macOS (unit tests only): fence rejection on a stale generation;
lease CAS rejection; ack monotonicity; a `zfs send` failure not
advancing the generation; `replica_unobserved` produced by a failed
target query and never mapped to `current` or `stale`; a target
reporting an old generation yielding `replica_stale` even though it
answered promptly; replication contributing nothing to
`ClassifyQuorum`; the feature flag blocking `CreateReplicationPolicy`
at apply time; staging-dataset naming and cleanup; throttle token-bucket
rate over a synthetic reader; window deferral across the maintenance
planner state machine; deferral while a migration lock is held; and that
no key material ever appears in a marshalled `FSMSnapshotState` or an
ack.

Only on brood/drone (10.90.0.94 / 10.90.0.95, FreeBSD 16.0-CURRENT,
bare metal) — **macOS cannot validate any of this**: it has no ZFS of
the kind this uses, no `zfs(8)` streaming, no resumable receive, no
HAST, no bhyve, no jail, and no real-network latency. Set
`APIARY_ZFS_TEST_POOL` to an existing pool (the default `apiarytest`
does not exist on a host that only has `zroot`):

- full `-I` receive into a staging dataset, rename, mountpoint, and
  `zfs list` verification of a jail dataset;
- incremental send after a second snapshot, verified by GUID;
- an interrupted `zfs send` (kill mid-stream) followed by
  `zfs send -t <token>` resume, proving the token round-trip and that
  the received data is complete;
- a destroyed destination dataset forcing a restart from the last acked
  snapshot rather than from the origin full send;
- the fence: two managerd processes attempting to push the same policy,
  with the second rejected on a stale generation;
- measured throughput against a real 1 GbE and 10 GbE link with the
  throttle engaged, confirming the link is not saturated;
- `full_resync_count` observed to increment on a genuine incremental
  failure;
- recovery from a real dataset: destroy the "source" dataset, recover
  on the second node, clone back, and confirm the Cell starts;
- an actual multi-Comb quorum loss, confirming runs continue under an
  unexpired lease and stop when it expires.

## Open questions

1. Should a replication policy be allowed to target a Comb in a
   different Cell by default, or must `target_node_id` be explicitly in
   another Cell with the failure-domain check enforced at apply time?
   ADR-0127 assumes the latter.
2. Should the DR copy be permitted to auto-promote to a running Cell
   (`protected autostart` in Sylve), or does this stay operator-only
   for good? The safe answer is the second, but operators will ask.
3. Is native ZFS encryption on the target sufficient, or do operators
   on multi-tenant Cells need a per-policy key rather than a per-node
   passphrase file?
4. Does the acked-generation node-local journal need to survive a Comb
   rebuild from scratch, or is losing it acceptable (it would force a
   full resend, not data loss)?
5. Should replication eventually cover `hast-*` provider datasets
   behind an explicit opt-in for operators who want a genuine third
   copy, given the coherence concern in section 1?
6. Retention: is a fixed generation count enough, or is retention by
   age (and by a total-bytes budget) required to stop replication
   consuming a pool?

## References

- ADR-0008 — internal/hast config rendering and lifecycle
- ADR-0017 — ISO upload with hash verification
- ADR-0022 — VLAN networks, DHCP-backed IP allocation, per-VM firewall
- ADR-0023 — API-key authentication for managerd
- ADR-0025 — resource reclaim for reassigned and stuck-deleting VMs
- ADR-0026 — real HAST-backed VM disk replication
- ADR-0028 — manual `MigrateVM`/`MigrateJail`
- ADR-0049 — Machine Configuration page
- ADR-0051 — `raftd` configuration save/restore
- ADR-0056 — evidence-aware health v1
- ADR-0084 — jail base images via ZFS clone
- ADR-0089 — cross-node jail base-template fetch
- ADR-0090 — VM snapshot and restore
- ADR-0103 — action preflight and concurrent-restart guardrail
- ADR-0115 — automatic peer TLS hostname map
- ADR-0116 — quorum-safe `raftd` restart
- ADR-0118 — voter-by-voter quorum blocker detail
- ADR-0119 — replica freshness evidence and maintenance wave planner
- ADR-0121 — live HAST synchronization evidence in Comb-failure
  simulation
- ADR-0122 — cluster-wide evidence-aware health API
- ADR-0123 — Colony leader indicator
- ADR-0124 — read-only offline `raftd -status`
- ADR-0127 — Sylve.io feature adoption (Phase 2, item 2)
- ADR-0128 — guest migration (in parallel; this ADR supplies its
  storage handoff)
- ADR-0131 — backup and restore (in parallel; replication is not a
  backup)
- `internal/hast/manager.go`, `internal/hast/config.go`
- `internal/zfs/manager.go`, `internal/zfs/exec.go`
- `internal/cluster/simulate.go`, `internal/cluster/reconciler.go`,
  `internal/cluster/hast.go`, `internal/cluster/jail.go`
- `internal/recovery/quorum.go`, `internal/coverage/scenario.go`
- `internal/manager/server.go` (`PushJailTemplateTo`,
  `ReceiveJailTemplate`)
- `internal/origincert/store.go`, `internal/raftdconfig/manager.go`
- `api/internalpb/state.proto`, `api/internalpb/raftd.proto`,
  `api/rpc/manager.proto`
- Sylve v0.3.0 feature catalog, as surveyed in ADR-0127
