# ADR-0143: a per-voter FSM state digest, shown as a colony-view badge

## Status

Accepted (decision). Not implemented. No code has been written against
this ADR; it records a decision and the survey behind it so the
implementation does not have to be re-derived.

## Context

The code audit that landed as `c37c739` changed the behaviour of four
FSM apply functions: `applyPurgeVM` and `applyPurgeJail`
(`1af45c7`, now refusing a purge of anything not already `DELETING`),
`applyCreatePendingJoinRequest` (`e7f59fd`, adding field validation,
expiry eviction and a 100-record cap), and
`applyResolvePendingJoinRequest` (`a6823e8`, removing a `time.Now()`
comparison that made replays non-deterministic).

While voters run different builds, the same committed log entry can
therefore produce different state on different replicas. Raft cannot
detect this: `applied` and `last_log` agreeing on every voter proves the
logs agree, not that the state machines do. The library compares no
state hash, and none exists anywhere in `internal/raft`.

The audit's own mitigation was a one-time manual comparison of each
voter's local view (`ListVMsLocal`, `ListJailsLocal`, the pending
join-request list). That is a snapshot. The same window reopens on
every future apply-path change, and nothing will notice.

## Decision

Add a **canonical state digest** to the per-voter status surface, and
render agreement as a **badge in the colony view**.

- One new field, no new RPC. `state_digest` on raftd's
  `StatusResponse` (`api/internalpb/raftd.proto`, the block at
  `last_log_index = 4` / `applied_index = 5`), computed by a new
  `FSM.StateDigest()` beside the existing `AppliedIndex()`
  (`internal/raft/fsm.go:1155`), and surfaced on managerd's
  `StatusResponse` next to `raft_applied_index = 8`.
- `Status` is already peer-dialable (`PeerForwarder.Status`), already
  authenticated, and already fanned out per member by the
  `ClusterHealth` collection (`internal/manager/server.go:970-1015`,
  which already collects every peer's applied and last index at
  `1097-1113`). The digest rides that existing fan-out.
- The colony view shows one row per Comb carrying its digest, with a
  match/mismatch badge across voters. The user's decision, 2026-09-27.

### The digest must be canonical, and it must be cached

`FSMSnapshotState` is entirely `map<string, ...>` fields, so a raw
`proto.Marshal` hash is not a stable contract - Go map iteration order
would make it differ between processes on identical state. The encoding
must be sorted-key.

It must be cached in the FSM and recomputed on `Apply`, not computed
per `Status` call. `Status` is on the health-check and colony-view path;
serialising the whole state machine on every call would make a
diagnostic into a load problem.

### What the comparison must not include

`FSM.ListPendingJoinRequests` filters expired records lazily at read
time (`internal/raft/fsm.go:862-870`). Two voters holding byte-identical
state will legitimately return different lists. The digest must be
computed over the unfiltered stored records, or it will report
divergence that is not there.

## Consequences

- **The digest detects disagreement but does not localise it.** It says
  the four disagree; it does not say which one is wrong. Diagnosing
  which is the operator's problem, and the badge must not imply
  otherwise.
- **A digest is not a correctness proof.** It shows the state machines
  converged. It says nothing about whether the apply functions are
  right, and nothing about non-FSM state.
- **Sampling races a moving cluster.** Entries committed during
  collection can make a correct cluster look divergent. The badge
  should show the per-voter `applied` index alongside the digest, so a
  mismatch at differing indices is readable as a sampling artifact
  rather than a real one.
- **Any future apply-path change reopens the window this closes** for
  the duration of a rollout. The mitigation is unchanged and still
  worth stating: deploy `raftd` to all voters close together.

## Current state of the live cluster

Verified 2026-09-27, read-only, from each daemon's own startup log:

- brood, drone, buzz and sting `managerd`/`raftd` all on `c37c739`.
- buzz and sting `frontend`/`restshimd` on `71e5151`, from a
  `make update` sweep that correctly left `managerd` and `raftd`
  alone, per ADR-0141.

So **no skew window is open on this cluster right now**, and this ADR
is not closing a live defect. It removes a class of future one. The
audit's manual check remains unrun and, with all four voters on one
build, has nothing to find.

## Verification

None yet: nothing is implemented. The design claims above were read
from the code at `71e5151` and cited by file and line; the live cluster
state was read from logs, not inferred from a build stamp on disk.

Live verification will need this deployed to a voter, which means a
service restart, which is the operator's call.
