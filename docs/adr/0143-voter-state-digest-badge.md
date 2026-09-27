# ADR-0143: a per-voter FSM state digest, shown as a colony-view badge

## Status

Implemented. The digest, the three protobuf fields, the per-Comb
fan-out, the colony-wide comparison, and the badge are all in place;
`internal/raft/statedigest.go` and `internal/frontend/statedigest.go`
are the two files to read. The "Decision" section below records the
design as it was decided; the three places the implementation went
further, and why, are recorded under "Where the implementation went
beyond this ADR" at the end.

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

Superseded later the same day, for completeness: the colony was
converged onto `a987996` across all sixteen daemons, and a separate
stopped `nptd` on sting (since fixed) had been making that node's local
clock read 79s slow. Neither changes anything above - the point stands
that the cluster was and is on one build - but the specific commit
named in this section is no longer what is running.

## Verification

Go: `gofmt -l .` clean, `buf generate` re-run with the generated stubs
committed, `go build ./...`, `go vet ./...` and `go test -count=1 ./...`
all pass (60 packages), plus `go test -race` on the three packages this
touched. The shell suites are unaffected and still pass (16 recorder
cases, 18 worktree-state checks).

The digest's own properties are pinned by tests rather than asserted in
prose, because the comparison is only sound if they hold: identical state
digests identically however it was built (including under reversed map
insertion order), every state section moves the digest, `last_index`
alone does not, the empty state still has a real non-empty digest, and a
restored FSM digests as its restored state rather than as the empty one
it started from. ADR-0143's own recorded hazard is a test too: an
expired-by-read-time pending join request still moves the digest, so the
digest cannot be built over `ListPendingJoinRequests`' filtered view.

Two real bugs were caught by those tests during implementation and are
worth naming, because both would have shipped as a working-looking
feature that was silently wrong:

- `protoreflect.Value.Bytes()` panics on a string field, and nearly
  every field in this state is a string. `String()` is the accessor.
- `canonicalMap` builds and returns its own buffer, so assigning its
  result discarded everything encoded so far. Only the **last** map in
  the message was reaching the hash; the digest was stable, was 44 bytes
  of input, and would have reported agreement between any two voters
  whose restart records matched, regardless of their VMs. The
  per-section sensitivity test is what exposed it.

Live verification is still outstanding, and it needs a service restart,
which is the operator's call. The colony-view badge cannot be shown
honestly until raftd is deployed to a voter and the digests are read
back across all four.

## Where the implementation went beyond this ADR

Three places, each a case where the design as written was either
weaker than the requirement or pointed at the wrong layer.

**The encoding is walked from the message descriptor, not hand-listed.**
The Decision section says the digest must be "computed over the
unfiltered stored records" with "sorted-key canonical encoding", which
reads as a hand-written list of sections. Implemented that way it would
be exactly the failure this ADR exists to prevent: a future state field
added to `FSMSnapshotState` and forgotten in the digest would be
permanently invisible to the comparison, and nothing would report it.
`internal/raft/statedigest.go` therefore walks
`FSMSnapshotState`'s descriptor via `protoreflect`, visiting fields in
field-number order and emitting map entries in sorted key order, with
every variable-length value length-prefixed. A new state field is
covered the moment it is declared, and the per-section sensitivity test
fails loudly if a section ever stops mattering.

**`raft_applied_index` is a named field on `ClusterNodeHealth`, not
something a consumer parses out of an observation.** The digest and the
applied index are only meaningful as a pair read at the same instant;
the index was already present, but only inside an observation's
`"applied=%d last_log=%d"` value string. Parsing prose to reconstruct a
pairing that the whole verdict rests on would be fragile in a way that
shows up as a false alarm. `dialed` on the same message set the
precedent - already implied by the observations, named on the row so no
consumer has to infer it.

**The colony view compares in the frontend, not in managerd's
`ClusterHealth`.** The Decision section says the digest "rides that
existing fan-out", naming `internal/manager`'s `ClusterHealth`. That
fan-out does carry the digest, as `ClusterNodeHealth.raft_state_digest`
- but the colony overview page does not use `ClusterHealth` at all. It
computes health per node locally and dials each peer's `Status` itself,
in `nodeHealthSignals`. So the digest is read there, off the `Status`
response that call already needed, and the comparison is done in the
frontend where the whole node set is in hand. The cost claim holds
exactly as written: no new RPC anywhere, on either path.

The verdict is four states, not two, and the extra two are the point.
`match` is ready, `mismatch` is error, and the two that a two-state
badge would have to fold together are `unsettled` (degraded - digests
differ, applied indexes differ, so it may be only a sample of a moving
colony) and `unobserved` (unknown). Two rules are enforced by test
rather than by review:

- A single observation is never agreement. One node always agrees with
  itself, so a one-reading sample satisfies "all observed digests match"
  trivially. The observed row itself reads `unobserved` in that case,
  not `match`.
- No verdict names a culprit. Every diverged row states how many others
  it matches and how many it differs from, and carries an explicit "the
  digest does not say which side is wrong".
