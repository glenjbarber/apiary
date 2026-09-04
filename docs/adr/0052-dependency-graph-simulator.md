# ADR-0052: Dependency Graph Simulator v1 ("what if this node disappears?")

## Status

Accepted

## Context

`CODEX.md`'s "Future product directions" names the Dependency Graph
Simulator as priority 1: read-only counterfactual questions like "what
happens if this hive, uplink, storage replica, image source, or raft
voter disappears right now?" That's far too broad for one slice - this
ADR covers v1's actual scope: **"what happens if this node disappears?"** -
raft quorum impact (using live reachability, not just configured
membership), which VMs/jails it owns, which VMs/jails it backs as a
HAST replica, and a recovery verdict for each. More scenarios (uplink
loss, HAST-pair desync) stay deferred to later slices.

**Naming**: the RPC and every Go/proto identifier is `SimulateNodeFailure`,
not `SimulateHiveFailure` - `README.md` is explicit that Hive/Comb/Cell
are product/UI terms only, never code identifiers ("they don't rename
any Go type, protobuf field, API resource..."). "Hive"/"Cell" only
appear in `web/templates/simulate.html`'s own copy.

## Design

The RPC (`ManagerService.SimulateNodeFailure`, leader-only like
`ListVMs`/`ListJails`, `RoleViewer`) is served by calling `ListVMs`,
`ListJails`, and `Status` against this node's own raft client, then
building a plain-Go report via a new `internal/cluster/simulate.go`
(mirroring `plan.go`'s style: no I/O, no proto types, fully unit
testable). The entire original request is forwarded to the leader on
a `leader_hint` rejection from either list call - not just the failed
sub-call - so the report never mixes this node's own raft view with a
different node's VM/jail list.

Six real design corrections came out of review before this was
approved - each is a genuine, disclosed limitation or a fixed gap, not
a nice-to-have:

1. **A missing or mistyped target must never look safe.** Before
   computing anything, the handler checks the requested `node_id`
   against every raft server ID and every VM/jail's `node_id`/
   `replica_node_id` (`cluster.IsKnownTarget`). If it matches none of
   them, the response's `error` field is set to a specific message
   naming exactly why, and the frontend renders that as an error
   banner - never a "quorum survives, 0 resources owned" report, which
   would look indistinguishable from a genuinely safe finding.
2. **`RECOVERY_VERDICT_UNPROTECTED`, not `LOST`.** No `replica_node_id`
   proves only the absence of HAST-based redundancy Apiary knows
   about - not that the resource's data is permanently unrecoverable
   by any means (a surviving physical disk or an external backup might
   exist outside Apiary's own tracking). `RECOVERY_VERDICT_UNVERIFIED_REPLICA`
   is the strongest verdict a replica-configured resource can ever get
   in v1, since live HAST sync status has zero RPC exposure anywhere
   in this codebase - `internal/hast/manager.go`'s `Manager.Status` is
   called only internally by `internal/cluster/hast.go` to self-verify
   a role change it just made, and `MigrateVM`/`MigrateJail`'s own
   rejection strings already tell the operator to check `hastctl
   status` by hand. This gap is deliberately not closed here.
3. **This is not a self-consistent snapshot.** `ListVMs`, `ListJails`,
   and `Status` are three separate sequential reads, and the live
   reachability checks (below) are a fourth kind of separate,
   sequential I/O - nothing makes any of this atomic against
   concurrent cluster changes. Whole-request leader-forwarding (above)
   still earns its keep - it stops the report from mixing two
   *different nodes'* views - but does not make the reads atomic
   against each other. The RPC's own doc comment, this ADR, and the
   rendered page all say so plainly, rather than letting the report's
   internal consistency be assumed.
4. **The node picker includes placement-only nodes.** A node can
   remain a VM/jail's `node_id` or `replica_node_id` after being
   removed from raft entirely (ADR-0025's reassignment-reclaim
   territory) - simulating "what if *that* node disappears" is a
   legitimate question (e.g. discovering a stale replica target is
   already gone). `internal/frontend`'s `/simulate` page dropdown is
   the union of `Status`'s `known_node_ids` and every VM/jail's
   `node_id`/`replica_node_id`, not raft membership alone.
5. **Replica-backed Cells are a distinct, separately-computed
   consequence.** An early draft of this feature said it would report
   resources the target node "owns or backs as a replica," but only
   ever computed ownership impact. v1 actually computes both:
   `cluster.ComputeOwnedResourceImpacts` (resources this node hosts -
   these stop if it disappears) and `cluster.ComputeReplicaBackedImpacts`
   (resources owned by a *different* node, for which the target is the
   configured HAST replica - these keep running unaffected on their
   real owner, but lose their redundancy). A resource never appears in
   both lists for the same target - confirmed by a dedicated
   regression test.
6. **Quorum survival reflects live reachability, not configured
   membership counts alone.** Removing one node from a raft
   configuration can look survivable by pure arithmetic (e.g. "3
   voters minus 1 leaves 2, majority of 3 is 2") while actually
   failing right now, if one of those "remaining" voters is already
   unreachable for an unrelated reason. `Server.SimulateNodeFailure`
   checks every remaining voter's live reachability via the existing
   `PeerReporter.HostStats` mechanism (the same one ADR-0036's cluster
   overview already uses) before computing `survives`. When
   reachability can't be verified at all (`s.peers == nil` - no peer
   forwarding configured on this node), that voter's status is
   `Unknown`, a real third state alongside reachable/unreachable -
   never silently folded into either. `Survives` is computed from
   confirmed-reachable voters only; an `Unknown` voter is flagged
   explicitly in `Note` even when it doesn't change the answer, never
   silently resolved either way.

### A genuine import-cycle fix along the way

`internal/manager`'s regular code importing `internal/cluster` (for
the plain-Go report types) collided with `internal/cluster/integration_test.go`
already importing `internal/manager` (for a real raftd+managerd
integration test predating this ADR). Fixed by converting that file
from an internal test (`package cluster`) to an external one
(`package cluster_test`) - a separate compiled unit from `cluster`
itself, so `cluster_test -> manager -> cluster` creates no cycle even
though `cluster`'s own package can never import `manager`. This is the
standard, idiomatic Go fix for this exact class of problem, not a
workaround specific to this feature.

## Consequences

- An operator gets a fast, self-service answer to a question that
  previously required manually correlating raft `Status`, `ListVMs`/
  `ListJails`, and `hastctl status` by hand across every host.
- The "unprotected"/"unverified replica" verdicts are deliberately
  conservative, not guarantees - an operator could still misread
  "replica configured" as "safe to lose this node," which the UI copy
  and explanation text push back against explicitly, not just in this
  document.
- Live HAST sync status remains a real, disclosed gap for a future
  slice - closing it means new RPC plumbing this ADR deliberately
  didn't add (see correction 2).
- Reachability checks add real network calls (bounded to 3s each) to
  every `SimulateNodeFailure` request against a multi-voter cluster -
  acceptable for an occasional, explicit admin query, not something to
  put on any hot path.
- v1 only ever simulates one node disappearing at a time, and only
  reports raft/VM/jail/HAST-placement impact - uplink loss, uplink
  capability, image-source availability, and console/network
  dependencies are all explicitly out of scope for this slice, per
  `CODEX.md`'s own broader (unscoped) framing of the feature.
