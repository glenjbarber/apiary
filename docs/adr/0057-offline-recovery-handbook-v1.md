# ADR-0057: Offline Recovery Handbook v1

## Status

Accepted

## Context

CODEX.md's "Future product directions," Priority 4, names **Offline
Recovery Handbook**: generate a printable, versioned, self-contained
break-glass DR document - useful when the web UI, raft cluster, or
management network is down - with scenario-specific editions (loss of
one hive, loss of the leader, management-network failure, total
control-plane loss, rebuild from surviving storage), a dated topology
snapshot, exact quorum-restoration order, HAST promotion/split-brain
checks, one recovery action at a time with explicit stop conditions and
clearly marked irreversible steps, and a creation time/version/checksum
on each edition.

CODEX.md's own text says this handbook is generated "from the current
Dependency Graph and a simulated disaster-recovery Flight Plan." Neither
"Apiary Flight Plan" nor "Continuous Recovery Proof" (both referenced by
this Priority) exist as real code - CODEX.md itself labels both "a
future design direction, not implemented functionality." This ADR does
the same honest scoping-down this project already did for Priority 2
(Automated Assumption Checks v1, ADR-0055) and Priority 3 (Evidence-Aware
Health v1, ADR-0056): build exactly the one scenario edition that
already has real, computed backing data - **loss of one hive** - and
explicitly name every other edition as not-yet-built.

## Scope

Parameterized by `?node_id=`, exactly like the existing `/simulate`
page, built entirely on top of already-exposed data: the Dependency
Graph Simulator's `SimulateNodeFailure` RPC (ADR-0052/53/54) plus one
`Status` call for raft leader identity and membership. **No new RPC, no
proto changes.**

CODEX's "loss of the leader" scenario is folded into this same edition
rather than built separately: comparing the chosen `node_id` against
`StatusResponse.raft_leader_id` changes the *quorum verdict itself*
(see "Three-state quorum" below), not a disclaimer bolted on afterward.

**Explicitly not built in this pass:**

- **Management-network failure.** Real disambiguation worth stating
  plainly: the existing `SimulateNetworkFailure` RPC (ADR-0053) already
  covers a *managed tenant network* (VLAN/bridge a VM attaches to)
  failing - a completely different thing from Apiary's own raft/gRPC
  control-plane network (between hives) failing, which nothing
  simulates today. The near-identical names make this an easy trap for
  a future reader to fall into.
- **Total control-plane loss.** Not a cheaper variant of the same
  computation - `SimulateNodeFailure` structurally requires a live raft
  leader to answer `ListVMs`/`ListJails` in the first place, so it
  cannot execute at all once quorum itself is gone. The real recovery
  path for that scenario is `raftd -export`'s `ConfigArchive`
  (ADR-0051, CLI-only, a completely different mechanism).
- **Rebuild from surviving storage.** Largely non-Apiary manual
  OS/ZFS-import content, not derived from any live cluster state.
- **The online Living Runbook / Guided Recovery Mode**, and the
  separate **Offline Recovery Seed** (CODEX's own Priority 5) - both
  need machinery (a Flight Plan execution engine, a signed
  machine-readable package) that doesn't exist.
- **Credential-recovery-location references.** CODEX asks for this;
  Apiary has no credential-management surface to reference yet, so
  this is deferred rather than answered with an invented config flag
  for one static text blob.

## Design review

This design went through four review passes before implementation.

**Pass 1** found the draft assumed `SimulateNodeFailure` always either
succeeds or forwards transparently to the leader - false: its handler
(`internal/manager/server.go`) returns a normal-looking response with
`Error` populated whenever peer forwarding isn't configured or the
forward itself fails, exactly the degraded-cluster case this feature
exists for. It also found the draft's plan to re-fetch full
cluster-wide health/assumptions data via an unbounded per-node fan-out
on every page load was a real reliability risk: `SimulateNodeFailure`'s
own remaining-voter reachability loop is already sequential at 3s/voter
(`reachabilityCheckTimeout`), and the peer-dial paths this would
additionally invoke set no context deadline at all.

**Pass 2** found four blocking defects:

1. The originally-revised HAST "promotion" step was itself unsafe -
   checking only sync status doesn't prove the old primary is dead; a
   management-network partition could leave it still writing while an
   operator promotes the replica, a genuine split-brain. Apiary already
   has a reviewed, ownership-swapping path for exactly this
   (`MigrateVM`/`MigrateJail`, ADR-0028) that lets the existing
   reconciler handle old-owner teardown/demotion and new-owner
   promotion/startup, rather than a manual `hastctl role` change racing
   whatever the reconciler is doing. Separately, the draft used a
   resource's display **name** where the real HAST resource identifier
   is `"vm-"+id`/`"jail-"+id` (`internal/cluster/hast.go`,
   `internal/cluster/jail.go`) - a real naming bug that would reference
   a resource that doesn't exist.
2. Quorum needs three states, not a bool - `ComputeQuorumImpact`'s
   `Survives := remainingReachable >= quorumSize` conflates "even
   crediting every unknown-reachability voter as reachable, a majority
   is impossible" (genuinely lost) with "the confirmed-reachable count
   alone falls short, but the deficit could close if the unknown voters
   turn out reachable" (unknown, not lost). These need different
   operator guidance.
3. `statusErr == nil` is not sufficient generation gating -
   `Status`'s own handler deliberately returns a normal RPC response
   with `RaftReachable: false`/`RaftError` populated when raftd is
   unreachable, "so callers always get a diagnosable payload." Gating
   on the transport error alone would silently treat that state as
   "leader identity confirmed: no."
4. Image `unknown` and `unavailable` can't share one bool -
   `cluster.ImageAvailabilityVerdict` already models this correctly as
   three states; collapsing it turns "inventory unreadable" into the
   same "restoration is blocked" claim as a confirmed absence.

Three further corrections: a printed page whose only cluster-context
data is *links* to `/`/`/assumptions` isn't actually self-contained
during the outage it targets (resolved by "Bounded, embedded evidence"
below); the snapshot fingerprint must hash the generated `Steps`
alongside the raw inputs, or a change to step-generation logic could
silently produce different guidance under an unchanged fingerprint; and
the printed artifact needs concrete paper-safety handling (STOP/
IRREVERSIBLE as literal text, a step and its stop condition never split
across a page break, page numbers).

**Pass 3** found two more blocking issues, both concerning the same
HAST-promotion step:

5. The originally-proposed secondary-side check ("run `hastctl list
   <name>` on the replica and confirm `status: complete`") was, per
   `internal/hast/manager.go`'s own doc comment at the time, checking a
   field claimed to be primary-only - and `parseStatus` was documented
   as never parsing dirty-extent/sync-progress data on either role at
   all.
6. "Positively confirm the old owner is fenced" wasn't operationally
   specific - the step needed to enumerate real evidence and explicitly
   reject what doesn't count (a failed ping/SSH, or `managerd`/`raftd`
   merely unreachable from this report, is "we couldn't check," never
   "confirmed dead").

**Pass 4** found the pass-3 premise itself was wrong, plus six more
corrections:

7. **`internal/hast/manager.go`'s doc comment directly conflicted with
   this project's own recorded live results.** ADR-0008 and ADR-0028
   both explicitly record live `hastctl list` output showing `status:
   complete` on the *secondary* role, on this project's own real
   hardware. Resolved with a genuine, live pre-implementation check
   (see "Live HAST verification" below) rather than picking a side from
   documentation alone.
8. The fencing definition still permitted unsafe interpretations
   specific to Apiary's actual architecture: HAST replication here is
   host-local-disk-based, not shared-storage-based (no array to revoke
   access to), and a pulled network cable proves nothing about whether
   the old owner can still write to its own local disk.
9. `QuorumUnknown`'s stop condition wasn't a hard enough gate - it must
   explicitly prohibit every raft-mutating step (the migration step),
   not just say "don't assume automatic restoration."
10. The evidence structures discarded evidence: `AssumptionConcern` was
    missing `SubjectKind`/`SubjectID`/`DependencyID`/`Qualifier`/
    `ObservedStatus` (fields that identify *what* was checked and
    separate a merely-stale observation from a genuinely negative one),
    and the health portion kept only a status string instead of the
    actual cited `Observation`s.
11. `HealthUnreachable` (an invented boolean) couldn't actually be
    populated - `nodeHealthSignals` never returns an error; a failed
    peer call already flows into `health.NodeSignals` and comes back as
    `StatusUnknown`/`StatusContradictory` with a citing `Observation`,
    the correct existing vocabulary. Assumptions are different -
    `nodeAssumptions` does return a real fetch error, so a real
    `AssumptionsFetchError` field is legitimate there.
12. `ValidQuorumFact` needed more fields to validate its own claims -
    `QuorumSize` alone can't check internal consistency; `TargetIsVoter`/
    `TotalVoters`/`RemainingVoters` (all already on the wire) are needed.
13. The fingerprint envelope was still incomplete (omitted raft
    membership) and undefined about whether `GeneratedAt` participates
    (it must not - see "Snapshot fingerprint" below).

## Live HAST verification

To resolve finding 7 without guessing, a real throwaway HAST resource
was created and torn down on `apiarium`/`apiverse` as the first concrete
implementation step (not a real VM/jail - a small file-backed resource
under `zroot/apiary/hasttest`, entirely within Apiary's own already-used
ZFS scope, removed afterward with both nodes restored to their prior
`hastd` running state).

**Result: `status: complete` IS reported on the secondary role.**
`apiarium` (primary) and `apiverse` (secondary) both showed `status:
complete` in their own `hastctl list` output, on this project's actual
FreeBSD 16.0-CURRENT hosts running `FreeBSD-hast-16.snap20260828121544`.
This matches ADR-0008/ADR-0028's own recorded observations exactly.
`dirty: 0 (0B)` was also present on both roles in the raw output.
Inspecting `internal/hast/manager.go`'s `parseStatus` confirmed it
already extracts the `status:` key unconditionally, with no role-gating
logic at all - the doc comment claiming "no equivalent field" on a
secondary was simply stale prose, never a real parsing bug. Fixed by
correcting `Status.ResourceStatus`'s doc comment
(`internal/hast/manager.go`) to state plainly that it's reported for
either role, live-verified. `ADR-0008`'s own original (pre-"Update")
text making the same stale claim is left as historical record, per this
project's convention of appending corrections rather than rewriting
earlier reasoning.

This means Step 3(b)'s wording (below) checks `hastctl list`'s `status`
field directly on the secondary, rather than falling back to a
manual, tooling-blind dirty-extent-only instruction.

## Design

### `internal/recovery` (new pure package)

Mirrors `internal/health`'s structural precedent: small, zero OS/exec
dependencies. Duplicates `internal/cluster`'s fact types (a large
package with real `zfs`/`jail`/`bhyve`/`hast` dependencies bundled in),
but **imports `internal/health` directly** for `NodeHealth`/
`Observation` - unlike `internal/cluster`, `internal/health` is itself
small and dependency-free, so importing it carries none of the baggage
the cluster-type duplication exists to avoid.

`QuorumFact` mirrors `cluster.QuorumImpact`'s raw counts (never a
pre-collapsed `Survives` bool). `ClassifyQuorum(f, isCurrentLeader)`
returns a real `QuorumVerdict` (`Survives`/`Unknown`/`Lost`): `Lost`
only when even crediting every unknown-reachability voter as reachable
still falls short of quorum size; otherwise `Unknown` when the
confirmed-reachable count alone falls short but the deficit could
close; `Survives` when the confirmed-reachable count alone already
meets it. When `isCurrentLeader`, any non-`Lost` verdict is downgraded
to `Unknown` - the underlying reachability data was gathered via calls
answered by the current leader (`ListVMs`/`ListJails` are leader-only,
ADR-0035), which proves the leader can reach each remaining voter, never
that the remaining voters can reach *each other*, the actual
precondition for a new election once the leader itself is gone. A pure
count-based `Lost` is never downgraded, since that's independent of
reachability data entirely.

`ValidQuorumFact` rejects a structurally-implausible `QuorumFact` before
it reaches `ClassifyQuorum` as a convincing zero value: `QuorumSize ==
0` is the concrete signature of `internal/raft.Node.Status()`'s
pre-existing, already-disclosed gap (ADR-0056) silently leaving
`Servers` nil on a `GetConfiguration()` error - indistinguishable from
"0 total voters" otherwise, which would classify as a fabricated
`Lost`. Also checks `QuorumSize == TotalVoters/2+1`,
`RemainingReachable+RemainingUnknown <= RemainingVoters`,
`RemainingVoters <= TotalVoters`, and that removing exactly one target
changed the voter count by exactly 0 or 1.

`ResourceFact.ReplicaConfigured` - not `Protected` - deliberately
matches `RecoveryVerdictUnverifiedReplica`'s own hedged naming: a
configured replica is not a *confirmed* one. `HASTResourceName(kind,
id)` mirrors `vmHASTResourceName`/`jailHASTResourceName` exactly (small,
deliberate duplication across the package boundary, the same convention
`internal/health` already established for `cluster.Reachability`).

`ImageFact.Verdict` preserves `cluster.ImageAvailabilityVerdict`'s three
states. `AssumptionConcern` mirrors `assumptionResultView`'s complete
field set (`Kind`/`SubjectKind`/`SubjectID`/`DependencyID`/`Qualifier`/
`ObservedStatus`/`Status`/`ReasonCode`/`Detail`/`Stale`/
`LastObservedAt`, the last kept as the same pre-formatted string
`assumptionResultView` already produces, not re-parsed into `time.Time`
and back). `NodeContextFact.Health` embeds `health.NodeHealth` directly
rather than a separate boolean.

`BuildHandbook(Inputs) Handbook` is pure and deterministic, producing a
fixed-order `[]Step`: (1) quorum status, always first; (2) one
informational step per replica-backed resource; (3) one irreversible
migration step per owned resource with a configured replica; (4) one
step per unprotected owned resource; (5) one step per image whose
verdict isn't "available."

### Step 3: the corrected HAST migration procedure

Every migration step's Detail opens by quoting the Step 1 quorum
verdict, and whenever it isn't `Survives`, states plainly that the
migration below must not proceed until it is - a raft write attempted
without real quorum can silently fail, be rejected, or apply against a
leader without legitimate quorum support.

The ordered sub-procedure:

**(a) Fence the old owner - specific, enumerated evidence only.**
Narrowed to what Apiary's actual host-local-disk HAST architecture
makes meaningful (finding 8): acceptable evidence is confirmed
power-off via independent out-of-band management (IPMI/BMC or
equivalent), or physical removal of the old owner's storage device
confirmed on-site. Explicitly **not** acceptable: a failed ping, a
failed SSH attempt, `managerd`/`raftd` being unreachable from this
report, or a disconnected network cable alone - none of these prove the
host cannot still write locally, and an unreachable-but-still-writing
primary plus a promoted replica is a split-brain. The rendered page
includes a printable "Fencing evidence record" block (Method / Evidence
/ Operator / Timestamp blanks) directly under this step, since Apiary
has no fencing-evidence-recording feature of its own - the printed page
itself is where v1 captures it.

**(b) Verify secondary sync.** Per the live verification above: run
`hastctl list <HASTResourceName(kind, id)>` on the replica node and
confirm it shows `status: complete`. If the resource is disconnected,
uninitialized, or unable to identify its peer, that status reading (or
zero dirty extents) is **not** sufficient proof - report the last
known-successful-reconciliation timestamp instead and stop.

**(c)** Only once (a), (b), and a `Survives` quorum verdict are all
independently confirmed, call `MigrateVM`/`MigrateJail` with
`target_node_id` set to the replica - never a direct `hastctl role
primary` call.

**(d)** After migration, verify ownership swapped, the cell started,
its network is up, and any declared health check passes.

The step's `StopCondition` requires all three (quorum, fencing, sync)
explicitly, and the quorum step's own `Unknown`/`Lost` stop conditions
(Step 1) separately name the migration step by number - a reader who
only reads Step 1 first still sees the prohibition (finding 9).

### Bounded, embedded evidence

A printed page's link to `/`/`/assumptions` is unusable during the
exact outage this handbook targets, and dead text once printed - but
re-running their full N-node fan-out on every page load was already
rejected in pass 1 as a real reliability risk. The resolution: embed
evidence, bounded in **both count and time**, to only the nodes
relevant to *this* hive's own scenario - every distinct `ReplicaNodeID`
among its owned resources (a candidate migration target) plus every
distinct `OwnerNodeID` among resources it backs, deduplicated.

Every relevant node ID is always listed, even beyond a small fixed cap
(10) - a node beyond it shows `EvidenceLimitReached` and the literal
text "Not checked because the evidence limit was reached," never
silently dropped (a generic truncation count would hide *which*
dependency went unchecked). For the capped set, the existing private
`fetchHostStats`/`nodeHealthSignals`/`health.ComputeNodeHealth` and
`nodeAssumptions` helpers are reused unmodified, but wrapped in a new
per-node `context.WithTimeout` (3s, matching `server.go`'s own
`reachabilityCheckTimeout`) plus one overall timeout for the whole
phase (10s) and fetched concurrently - neither helper wraps its own
context in a deadline today. A health-fetch failure needs no separate
flag (it flows into `health.NodeHealth.Status` directly); an
assumptions-fetch failure sets a real `AssumptionsFetchError`, since
`nodeAssumptions` does return one.

The full raft membership/suffrage list is embedded for free (already
present in the anchor `Status()` response) - the "dated topology
snapshot" CODEX asks for, at zero extra cost.

The rendered non-atomicity disclosure names every read the page
performs across its recorded evidence-gathering window - the node
picker's own `Status`/`ListVMs`/`ListJails`, the separate anchor
`Status()` call for leader identity, `SimulateNodeFailure`, and the
bounded node-context fan-out - rather than undercounting it as "one
Status() call plus SimulateNodeFailure."

### Snapshot fingerprint

SHA-256 (first 16 hex chars) over a dedicated envelope covering every
*stable* fact the page presents: format version, target/leader
identity, quorum counts, owned/replica-backed/image facts, the
generated `Steps` themselves, and the raft membership snapshot.
Deliberately **excludes** two things: `GeneratedAt`/the evidence-window
timestamps (including them would change the fingerprint on every
regeneration regardless of whether cluster facts actually changed,
defeating its purpose), and the bounded Node Context section - whose
own evidence (`health.Observation.ObservedAt`, each
`AssumptionConcern.LastObservedAt`) is itself inherently time-varying
observational data, not a stable fact the way membership/ownership/
quorum are; including it would have the same fingerprint-never-repeats
problem as including `GeneratedAt` directly. The label is deliberately
"Snapshot fingerprint," not "checksum" or "signature": it detects
whether a printed copy matches what would be generated right now, with
no secret involved - not tamper-evidence.

### Printable

A `@media print` block hides `nav`/interactive chrome. Each step (and
its own stop condition) is one `break-inside: avoid` block, so a page
break can never separate the two. `@page` bottom-center content
provides page numbers. **Disclosed limitation**: a true *per-page*
repeating header (hive name/timestamp/fingerprint on every physical
page) needs CSS Paged Media running-header support (`running()`/
`element()`) most print engines only partially implement - v1 ships a
single top-of-document identification block plus page numbers instead,
named here rather than silently under-delivered. Page-number rendering
itself was confirmed live in the browser used for Verification below,
since `@page` margin-box content support is recent and version-specific
in Chromium-based engines.

## Consequences

Non-HTML consumers of `SimulateNodeFailure`/`Status` get the same raw
data this feature already relies on, but no computed `Handbook` -
`BuildHandbook` is only ever invoked from this one frontend page
handler, the same "retrofit existing surfaces, don't build a new RPC"
posture ADR-0056 already took, for the same reason: every fact used is
already wire-exposed, and this is presentation-layer synthesis, not new
cluster-state computation.

The bounded Node Context set can miss a node that's relevant to the
broader cluster but not to *this specific hive's own* recovery (e.g. a
third node with no replica/ownership relationship to the target) -
deliberate: the alternative is the full-cluster fan-out pass 1 already
rejected as a reliability risk.
