# ADR-0060: Operational Invariants v1

## Status

Accepted

## Context

CODEX.md names **Operational Invariants**: represent safety rules as
explicit, continuously evaluated invariants shared by the Dependency
Graph Simulator, Flight Plan, recovery proofs, and later automation.
Named examples: no HAST resource has two writable primaries; a plan
cannot remove raft quorum; a cell called recoverable has a synchronized
replica and a capable destination; a managed network has a valid route
and working DNS path; no physical resource is deleted without proven
ownership. **Each evaluation must report true, false, or unknown** -
CODEX's own literal three-state vocabulary, never a fourth "not
applicable" state the way `internal/pathtrace`/`internal/assumptions`
have - with the evidence and freshness behind that result. Missing
observations must never be treated as a passed safety check.

"Flight Plan" - the mechanism this feature would eventually gate ("use
invariants proactively, to block an unsafe plan") - does not exist in
Apiary yet; CODEX.md itself calls it "a future design direction, not
implemented functionality." This is the same honest scoping this
project has applied to every CODEX priority so far: v1 builds only the
"continuously evaluate and report" half.

This is the first of three related features chosen together this
session (Operational Invariants, then Why Not Engine, then Resilience
Coverage Map), built in this order because Why Not Engine wants to cite
"the evidence and invariant behind each conclusion," and this feature
is the settled, named catalog it will cite.

## Design review

One review pass before implementation found the initial draft reused
two pieces of "already computed elsewhere" data incorrectly, and
dropped a required conjunction into a false positive:

1. **Reusing `ListNetworks`'s `bridge_status` field would have
   reintroduced a bug this project already found and fixed once.**
   `bridge_status` is documented in `api/rpc/manager.proto` as
   physical, per-node state populated by whichever node answers the
   call - and `ListNetworks` is leader-only and forwards to the current
   leader. `GetLocalNetworkBridgeStatus`'s own doc comment
   (`internal/manager/server.go`) says outright it is "Deliberately NOT
   `ListNetworks`" for exactly this reason - see ADR-0055 for the live
   bug it exists to avoid. `internal/assumecheck/checker.go` already
   does this correctly, fanning `GetLocalNetworkBridgeStatus` out to
   the specific nodes that actually own a resource on a network. The
   network-route invariant mirrors that pattern instead.
2. **Cell recoverability's `Result` computation dropped the
   "synchronized replica" half of CODEX's own conjunction.** "a cell
   called recoverable has *a synchronized replica AND a capable
   destination*" - live HAST sync status has no RPC exposure anywhere
   (the same gap the HAST-dual-primary invariant names), so that half
   is never confirmed. The initial draft still resolved `True` from the
   capable-destination half alone - exactly the "missing observation
   treated as a passed safety check" CODEX's own text warns against.
   Fixed: this invariant can never resolve `True` - only `False`
   (destination confirmed incapable) or `Unknown`.
3. **A per-voter `SimulateNodeFailure` fan-out for quorum tolerance
   would have cost O(voters²) reachability RPCs, concentrated on the
   leader.** `SimulateNodeFailure`'s own handler already does one
   *sequential* `HostStats` reachability check per remaining voter for
   whatever single target it's asked about; calling it once per voter
   multiplies that by N, and since `ListVMs`/`ListJails` are
   leader-only, all N calls would land on the same leader. Voter
   reachability doesn't depend on which voter's *hypothetical* loss is
   being asked about - only real, current state does. Fixed: one
   `Status()` call plus one bounded, concurrent `HostStats` fan-out to
   every current voter exactly once, then every voter's own
   hypothetical-loss classification is computed **locally** from that
   single shared reachability snapshot using
   `recovery.QuorumFact`/`ClassifyQuorum`/`ValidQuorumFact` directly -
   already pure and exported, zero new arithmetic. `isCurrentLeader` in
   `ClassifyQuorum` is recomputed **inside** the per-voter loop as
   `voter.NodeID == leaderID` - reusing one hoisted flag for every voter
   would silently mis-classify every voter but the intended one.
4. **`Evidence` needs a freshness field.** CODEX's own text requires
   "the evidence and *freshness* behind that result" -
   `internal/health.Observation` already has `ObservedAt time.Time`;
   this package must not regress from that convention. A zero
   `ObservedAt` specifically marks evidence that was never
   runtime-observed at all (the ownership-gated-deletion invariant
   below), distinct from "observed a while ago."
5. **A `currentVMs`/`currentJails` fetch failure must not silently
   render zero findings.** Both helpers' only existing caller
   (`simulateNodeChoices`) treats a failure as fail-soft - correct for
   a picker convenience, wrong here, since a fetch failure and "no
   protected resources exist" must never look the same. A failure emits
   one cluster-scoped `Unknown` evaluation citing it.
6. **A `HostStats` fetch failure to a replica-target node resolves
   `Unknown`, never `False` or a silently-dropped resource** - most
   realistic exactly when that node is down, which is when this check
   matters most.

## Design

### `internal/invariant` (new pure package)

Mirrors `internal/health`/`internal/recovery`: small, zero OS/exec
dependencies. Imports `internal/recovery` directly for
`QuorumFact`/`ClassifyQuorum`/`ValidQuorumFact` (pure, already
exported, no cycle risk). Defines its own `Reachability` type - a
third, deliberate duplicate of the same 3-value concept
`internal/health.Reachability` already duplicates from
`cluster.Reachability` - since `internal/recovery` itself carries only
raw counts, not a reachability enum.

Five named invariants, each a pure function over already-gathered
facts, returning `Evaluation{Name, Scope, Result, Explanation,
Evidence}`:

- **`quorum-tolerance`** ("a plan cannot remove raft quorum," reframed
  for v1 as "does the cluster currently tolerate losing any one more
  voter," since no Flight Plan exists to name a specific plan) -
  `False` if any voter's simulated loss would be `QuorumLost`,
  `Unknown` if any is `QuorumUnknown` or structurally invalid (and none
  `Lost`), else `True`.
- **`hast-dual-primary`** - always `Unknown`, one per resource ID with
  a configured replica, citing the permanent no-RPC-exposure gap.
- **`cell-recoverability`** - one per VM/jail with a replica configured
  (unprotected resources are out of scope, not violations); never
  `True` (finding 2); `False` only for a VM whose replica-target node
  confirms `bhyve_configured: false`; `Unknown` otherwise, including
  unconditionally for every jail (no jail-capability signal exists
  anywhere in this codebase - a real, permanent v1 asymmetry with VMs).
- **`network-route-dns`** - one per managed network; the "route" half
  comes from a `GetLocalNetworkBridgeStatus` fan-out to the network's
  actual owning nodes (never `ListNetworks`); the "DNS" half is
  unconditionally `Unknown` (no DNS observability exists anywhere).
  `False` only when route itself is a confirmed blocker; otherwise
  never better than `Unknown`, stated explicitly in `Explanation`.
- **`ownership-gated-deletion`** - a single, cluster-wide, static
  `True`, Evidence with a **zero `ObservedAt`** and
  `Source: "code construction (not runtime-monitored)"`, citing
  `ForcePurgeVM`/`ForcePurgeJail`'s `desired_state` gate and the
  reconciler's tombstone-driven teardown path by name. Unlike the four
  above, this is not computed from live request-time evidence at all -
  it documents an already-enforced structural guarantee (confirmed by
  reading `internal/manager/server.go` and `internal/cluster/reconciler.go`:
  `ForcePurgeVM`/`ForcePurgeJail` both require `desired_state ==
  DELETING` before submitting a purge, and the reconciler's physical
  destroy path is only ever reached once that same raft-replicated
  tombstone is already set - no bypass found).

### Frontend: `internal/frontend/invariants.go`

New `GET /invariants` page, Viewer-accessible, **no new RPC or proto
change**. Reuses `nodeContextTimeout`/`nodeContextOverallTimeout`/
`nodeContextLimit` (already defined in `recovery_handbook.go`) for
every bounded fan-out:

- Anchor `Status()` filtered to `Suffrage == "Voter"` (never
  `KnownNodeIds`, which mixes in VM/jail node IDs).
- Quorum tolerance: one bounded concurrent `fetchHostStats` fan-out to
  every voter except the local node, then local classification per
  voter as described above.
- Cell recoverability: `currentVMs`/`currentJails` (explicit error
  check, finding 5), then a bounded concurrent `fetchHostStats` fan-out
  to each *distinct* VM replica-target node only.
- Network route: `currentNetworks` plus the VM list already fetched for
  cell recoverability (to find each network's owning nodes), then a
  bounded concurrent `GetLocalNetworkBridgeStatus` fan-out to those
  nodes specifically.
- Ownership-gated deletion: no gathering at all.

Rendered as two sections: "Live-evaluated invariants" (the four above,
each with an expandable evidence list showing `ObservedAt` or "not
runtime-observed") and a separate "Structural guarantees" section for
ownership-gated-deletion, so a reader never mistakes a static,
code-attested claim for something freshly checked against current
state.

`peerHostStatsClient` (`internal/frontend/cluster_overview.go`) gains
`GetLocalNetworkBridgeStatus` - purely additive; already implemented by
`*manager.PeerReporter` (used today by `internal/assumecheck`), so no
new server-side method was needed. Every existing test fake
implementing this interface got the same mechanical one-method
addition already established for every prior interface growth this
session.

## Consequences

Non-HTML consumers get nothing new here - every fact used was already
wire-exposed before this feature, and `EvaluateQuorumTolerance` /
`EvaluateCellRecoverability` / `EvaluateNetworkRoute` are only ever
invoked from this one frontend page handler, the same "retrofit
existing surfaces, don't build a new RPC" posture ADR-0056/ADR-0057
already took.

`cell-recoverability` and `hast-dual-primary` can never resolve `True`
in v1 - this is a deliberate, disclosed limitation (live HAST sync
status has no RPC exposure anywhere), not a bug to fix quietly later;
closing it would require new plumbing this ADR does not build.
`network-route-dns` is similarly capped at `Unknown` whenever a route
is otherwise clear, since DNS observability doesn't exist at all.

`ownership-gated-deletion` is the one invariant in this catalog that
isn't independently re-verified at runtime - it is only as trustworthy
as the code paths it cites remaining unchanged. A future regression in
`ForcePurgeVM`/`ForcePurgeJail`'s own gating logic would not be caught
by this page; it would need to be caught by the existing regression
tests (e.g. `TestIntegration_ForcePurgeVM_RequiresDeletingState`) that
already cover this behavior.

The "proactively block an unsafe plan" half of CODEX's own vision
remains entirely out of scope until a real Flight Plan execution engine
exists - this catalog is diagnostic only in v1.
