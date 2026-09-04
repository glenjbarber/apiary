# ADR-0056: Evidence-Aware Health v1

## Status

Accepted

## Context

`CODEX.md`'s "Future product directions," Priority 3, names **Evidence-Aware
Health**: change Apiary's status contract so a green/healthy verdict means
*recently proven by relevant evidence*, not merely "no error reported."
Every health conclusion should carry its evidence source, observation time,
freshness limit, and the raw observations it was built from, with a
five-state model (healthy/degraded/unknown/stale/contradictory) replacing
implicit pass-by-default. CODEX.md scopes its own "first cluster-management
slice" to five signals - each hive's manager heartbeat, raft membership and
suffrage, peer reachability, applied index, and last successful
reconciliation - and names the concrete motivating example: "the simulator
may say membership can retain quorum while separately marking a remaining
voter's availability unverified" (exactly what ADR-0052's
`SimulateNodeFailure` already does internally for its own one narrow
question - this feature generalizes that pattern into a reusable status
contract).

Two scoping decisions were made explicitly before design began:

1. **Retrofit existing surfaces**, rather than build an isolated new
   RPC+page like the previous two features - add evidence-aware fields to
   the existing `Status`/`HostStats` RPCs and the `/` cluster overview page.
2. **Compute on-demand**, with no new background checker or persisted
   history (unlike the just-shipped Automated Assumption Checks, ADR-0055),
   since all five v1 signals are already observable live.

## Design review

This design went through one review pass before implementation, which found
the initial draft was not safe to build as written:

1. **The draft's own `NodeHealth` return type was never defined.** The
   signature and a `Status` enum existed; the struct that makes "keep raw
   observations separate from the conclusions derived from them" (CODEX's
   own words) have anywhere to live did not.
2. **The rule implementing CODEX's own named example never fired.** Real
   raft suffrage strings are `"Voter"/"Nonvoter"/"Staging"/"Unknown"`
   (capitalized - `internal/raft/node.go`'s `suffrageString()`, whose
   `default:` case returns `"Unknown"` for anything unrecognized). The draft
   compared `Suffrage == "voter"` (lowercase) - dead code, silently never
   matching, on the exact branch meant to catch "membership arithmetic
   masquerading as availability proof." The same bug meant raft's own real
   `"Unknown"` suffrage value fell through to a **Healthy** verdict instead
   of being capped at Unknown - raft's own admission of uncertainty getting
   silently promoted to a healthy answer is precisely the failure mode this
   feature exists to prevent. Fixed with a `Suffrage` type whose
   `ParseSuffrage` is the *only* place the raw wire string is ever read - no
   verdict-logic branch compares raw strings directly.
3. **The applied-index "lag" check measured the wrong thing, and no
   threshold would have fixed that.** `raft_last_log_index` is *appended*,
   not *committed* - it includes uncommitted log entries, which fluctuate
   continuously under ordinary write load as a normal side effect of raft's
   async apply pipeline, not as evidence of a problem. Nothing in this
   codebase calls or exposes raft's actual `CommitIndex()`, so there is no
   way today to compute the number that would make this check mean
   anything. A numeric threshold here would have been fabricated precision
   presented as evidence-backed judgment - the same class of mistake this
   project has already caught twice on prior features. Fixed by citing
   applied/last-log index as raw `Observation`s only, never deriving a
   verdict from them.

Two more real gaps were fixed in the same pass:

- `HeartbeatOK` originally conflated "the RPC transport succeeded" with
  "that node's own raft is healthy" - a node whose managerd is up but whose
  local raftd has died would have looked identical to a node cleanly removed
  from the cluster. This is the same class of conflation ADR-0055's
  `NOT_APPLICABLE` vs `UNKNOWN` split fought to eliminate, applied one level
  up here: `PeerReachability` (transport) and `HeartbeatOK` (that node's own
  self-reported raft health) are kept as genuinely separate `NodeSignals`
  fields.
- Fetching raft membership from *every* node's own self-report (rather than
  the one anchor read already fetched for the page) would have added real
  cost and an undesigned disagreement case with no actual benefit, since
  raft membership is a cluster-wide-consistent replicated fact. CODEX's own
  motivating example already shows the right pattern (one membership read,
  a *separate* independent reachability probe per member) - the same
  pattern `SimulateNodeFailure` (ADR-0052) already implements, reused here
  rather than reinvented.

## Design

### `internal/health` (new package) - pure types and computation, no I/O

`Status` (healthy/degraded/unknown/stale/contradictory), `Reachability`
(deliberately duplicated from `internal/cluster.Reachability` - this
project's established convention already accepts small duplication across
package boundaries rather than pulling a large package into a small, pure
one for one 3-value type), `Suffrage` (with `ParseSuffrage` as the sole raw
suffrage-string reader), `Observation` (one raw, independently-gathered
fact: source, observed-at, freshness limit, value, detail), `NodeHealth`
(the safe-to-read conclusion: status, explanation, and the observations it
was built from), and `NodeSignals` (the raw per-node input - the only place
new facts enter the package).

`ComputeNodeHealth(s NodeSignals, now time.Time) NodeHealth` is a pure
function implementing a fixed-precedence, eight-step decision chain (see
`internal/health/compute.go`): membership-not-observed caps at Unknown for
every node on the page, not just one; a raft-voter reported unreachable is
Contradictory (CODEX's own named example); an unrecognized or raft-reported
`"Unknown"` suffrage is capped at Unknown regardless of reachability; a
confirmed-unreachable node is Degraded (never bucketed with "couldn't
check"); an undeterminable reachability is Unknown; a reachable node whose
own report says its local raft is down is Contradictory; a reachable node
with no usable payload is Unknown (defensive); and only once every raft
signal is clean does reconciliation freshness decide Healthy / Degraded
(actively attempting but not yet succeeding) / Stale (nothing recent at
all) - with "no Reconciler configured on this node" treated as excluded
from the combination, not a below-Healthy penalty.

Reconciliation freshness uses a `3x` multiplier mirroring
`-assumption-stale-after`'s own already-reviewed precedent
(`cmd/managerd/main.go`: "3x the check interval, not an invented number"),
applied **per node from that node's own reported `-reconcile-interval`**,
never one global constant, since that flag is itself per-node/uncoordinated.
`RunOnce`'s own "success" is whole-tick, not per-resource (its pre-existing
`firstErr` pattern: every VM/jail/network step is attempted regardless of an
earlier failure in the same tick, and the first error is returned) - every
reconciliation `Observation`'s `Detail` says "last reconcile tick fully
succeeded," never anything implying "every resource is currently correctly
reconciled."

### New state: `internal/cluster.Reconciler` tracks its own attempt/success

`Reconciler` gained an `Interval time.Duration` field (set once at
construction) and two private `atomic.Int64` fields storing UnixNano
attempt/success timestamps (race-free without a mutex: `RunOnce` runs from
one ticker loop, but the new RPC-path readers run concurrently with it).
`RunOnce` records an attempt unconditionally at the very top - before even
`ListVMsLocal` is called, so a node whose raft is fully down still "tries"
every tick, and that itself is real, citable evidence - and records success
via a deferred check of its own named return value, only when the tick's
`firstErr` is nil. New accessors: `LastReconcileAttempt()`,
`LastReconcileSuccess()`, `ReconcileInterval()`.

### Proto additions

`StatusResponse` gained `repeated RaftMember members` (id/address/suffrage
per server) alongside the existing `known_node_ids` (kept as-is for
compatibility). `HostStatsResponse` gained
`last_reconcile_success_unix`/`last_reconcile_attempt_unix`/
`reconcile_interval_seconds` (0 timestamps mean "never observed";
`reconcile_interval_seconds == 0` is the only reliable "no Reconciler
configured on this node" signal - a 0 timestamp alone can't distinguish that
from "configured but no tick yet"). Both additions are additive and
non-breaking. `api/internalpb/raftd.proto`'s `ServerInfo.suffrage` doc
comment was also fixed - it previously listed only 3 of the 4 real values,
missing `"Unknown"`.

No new externally-visible health enum/message in v1 - see Consequences
below.

### Wiring

`internal/manager/server.go` gained a local `reconcilerStats` interface
(mirroring `isoManager`/`VNCLookup`'s own local-interface style) and a new
**trailing**, nil-able `NewServer(...)` parameter (matching the "append at
the end" convention already used for `assumptionStore`) -
`cmd/managerd/main.go` passes the node's own `*cluster.Reconciler` directly,
which satisfies the interface once its three new accessor methods exist.
`Status`'s handler populates the new `Members` field alongside the unchanged
`KnownNodeIds` loop; `HostStats`'s handler populates the three new fields
from `s.reconciler` when non-nil, zero otherwise.

`internal/manager/peer.go` gained `PeerReporter.Status(ctx, addr)`,
mirroring `HostStats`'s exact "dial directly, no leader-forwarding" shape
(`Status`, like `HostStats`, always answers only for whoever receives the
call - there's no leader concept to forward through).

`internal/frontend/cluster_overview.go` is the actual retrofit target - the
fan-out/computation lives here, not inside the `Status`/`HostStats` RPC
handlers themselves, so every *other* existing caller of those two RPCs
(`handleHostPage`, `assumecheck.Checker`'s internal raftd status read, any
restshimd/API caller) keeps paying exactly what it pays today, not a silent
new N-node fan-out cost. `peerHostStatsClient` gained a `Status` method;
`clusterNodeView` gained `HealthStatus`/`HealthExplanation`/
`HealthObservations` (additive - every existing field untouched);
`handleClusterOverviewPage`'s existing anchor `Status()` call (already
fetched once, previously only for `KnownNodeIds`) is now reused for
membership/suffrage of every row - no new call for that part. Inside the
existing per-node goroutine, the raw `*rpcpb.HostStatsResponse` already
being fetched (via a new `fetchHostStats` helper factored out of
`nodeHostStats`) is threaded through to read reconcile fields directly, and
one new `s.peers.Status(ctx, s.peerAddr(nodeID))` call supplies a non-local
node's own applied/last-log index and heartbeat; the local node reuses the
same anchor call already fetched at the top - no redundant self-dial.

`web/templates/cluster_overview.html` gained a Health column (badge using
`{{.HealthStatus}}` as its CSS class, mirroring the exact
`{{.Status}}`-as-class pattern `assumptions.html` already established, plus
a `title` attribute carrying the explanation and an expandable `<details>`
listing every citing `Observation`). `web/templates/layout.html` gained
`.badge.healthy`/`.badge.degraded`/`.badge.contradictory` CSS rules -
`.badge.unknown`/`.badge.stale` already existed from Automated Assumption
Checks (ADR-0055) and are reused as-is for those two states.

### A disclosed, pre-existing limitation this feature inherits, not fixes

`internal/raft.Node.Status()` silently leaves `Servers` as `nil` with no
error surfaced anywhere in the returned `Status{}` struct if its own
`GetConfiguration()` call errors - indistinguishable from "this node has
zero configured servers." Evidence-Aware Health's own membership observation
inherits this gap as-is; not fixed in this pass.

## Consequences

Non-HTML consumers of `Status`/`HostStats` (restshimd, any future CLI) get
the new raw evidence fields (`members`, the three reconcile fields) in v1,
but not a computed verdict - `ComputeNodeHealth` is only ever invoked from
the Go-side frontend page handler. This is a direct, deliberate consequence
of "retrofit existing surfaces, don't build a new RPC," not an oversight: a
future consumer wanting a computed `NodeHealth` would need `internal/health`
either exposed through a new RPC or reimplemented against the raw fields
already on the wire.

Same disclosed integration-test gap as ADR-0035/ADR-0036/ADR-0037:
`Server.raft` is a concrete `*RaftClient`, not an interface a fake can
satisfy, so `Status`'s reachable/unreachable behavior is covered by real
raft-harness integration tests (`internal/manager/integration_test.go`)
rather than a unit-level fake; a genuine leader-rejection scenario for
`Status` specifically doesn't apply here (unlike the write/read-forwarding
RPCs), since `Status` has no leader concept at all.
