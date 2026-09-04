# ADR-0061: Why Not Engine v1

## Status

Accepted

## Context

CODEX.md names the **Why Not Engine**: answer concrete operator
questions - "Why can this cell not migrate?", "Why is this hive unsafe
to reboot?", "Why is this cell not recoverable?", "Why can this network
not provide connectivity?" - by traversing the Dependency Graph and
returning the smallest actual blocker set, not a dump of every warning
in the system. It must identify the minimum safe changes that would
remove those blockers, with the evidence and invariant behind each
conclusion, stay deterministic and inspectable, remain read-only, and
distinguish a proven remedy from a plausible option that still needs
evidence.

"Flight Plan" - the mechanism recommendations "may become" - does not
exist in Apiary yet, so this stays the "explain" half only, the same
honest scoping every CODEX priority has gotten so far.

This is the second of three related features chosen together this
session (Operational Invariants, then Why Not Engine, then Resilience
Coverage Map), built in this order so this feature could cite a
settled, named invariant catalog (`internal/invariant`, ADR-0060)
rather than inventing its own safety logic. In practice, this feature
needed almost no new gathering code at all: every one of CODEX's four
example questions already has a purpose-built, already-shipped answer
somewhere in this codebase - the Dependency Graph Simulator's
`SimulateNodeFailure` RPC (ADR-0052), `MigrateVM`/`MigrateJail`'s own
real precondition (ADR-0028), and the `cell-recoverability`/
`network-route-dns` invariants (ADR-0060). Why Not Engine v1 is almost
entirely a **synthesis and filtering layer**: given one question and
one target ID, call the one already-correct mechanism, and reduce its
output to the smallest blocker set with proven-vs-plausible remedies
clearly separated.

## Design review

A Plan-agent critique before implementation (mirroring every prior
feature's one hard review pass) found real defects in the first draft:

1. **"Cell migrate" and "cell recoverable" are provably different
   questions under Apiary's actual code, not two views of one fact.**
   `MigrateVM`/`MigrateJail` (`internal/manager/server.go`) never check
   destination capability at all - only `replica_node_id ==
   target_node_id`, `desired_state != DELETING`, and `target != current
   node_id`. `cell-recoverability` additionally requires the
   conjunctive "synced replica AND capable destination" and folds an
   incapable destination into `False`. Concretely: a VM whose replica
   target is confirmed `bhyve_configured: false` is real-code
   *migratable* (nothing stops the RPC) while `cell-recoverability`
   correctly reports it `False`; a fully unprotected VM
   (`replica_node_id == ""`) is a real migrate-blocker but
   `cell-recoverability`'s own evaluator treats it as *out of scope* -
   no evaluation emitted at all (ADR-0060's own deliberate design, not
   a bug). Fixed: `AnswerCellMigrate` and `AnswerCellRecoverable` are
   two separate functions with separate blocker rules (`internal/whynot/whynot.go`),
   sharing only the `CellFact` gathering step - `AnswerCellMigrate`
   re-derives `MigrateVM`'s real precondition directly rather than
   delegating to the invariant. A regression test
   (`TestAnswerCellMigrate_UnknownNotBlockedWhenIncapableDestination`)
   proves the divergence: an incapable destination resolves `Unknown`
   for migrate, `Blocked` for recoverable.
2. **Reusing `gatherCellRecoverability` (`internal/frontend/invariants.go`)
   and filtering its output by `Scope == targetID` would have silently
   returned *nothing* for the single most common case: a fully
   unprotected cell.** Its `ResourceFact` builder skips any resource
   with `ReplicaNodeID == ""` by design (ADR-0060: unprotected
   resources are out of the invariant's scope, not violations of it).
   An empty slice would be indistinguishable from "not evaluated yet"
   or a fetch error, on a page whose entire premise is "never let a
   missing observation look like a passed check." Fixed: Why Not Engine
   does its own **single-target** lookup (`lookupCell` in
   `internal/frontend/why_not.go`) - `GetVM(id)` then `GetJail(id)`
   (the same one-shot pattern `console.go`/`serial_log.go` already
   use), plus one bounded `fetchHostStats` call to that one
   `ReplicaNodeID` only - never a cluster-wide fan-out to every replica
   target. `AnswerCellRecoverable` synthesizes the "no replica
   configured" blocker itself before ever calling
   `invariant.EvaluateCellRecoverability`. `gatherNetworkRoute` does
   **not** have this defect (it emits one `Evaluation` per network
   unconditionally, even with zero observations), so the network
   question reuses its per-network fan-out pattern, just scoped to one
   network instead of every network.
3. **`invariant.Evaluation`'s folded `Evidence` list cannot be reliably
   split into "actual blocker" vs. "permanent disclosed caveat" from
   outside the package without parsing prose** - every entry is
   freeform text; only a zero `ObservedAt` reliably marks the
   permanent, non-blocker caveats (the HAST-sync gap, the DNS gap),
   the same discriminator `invariants.html`'s own `NeverObserved`
   template field already uses. Fixed: `AnswerNetworkConnectivity`
   takes the **raw** `invariant.NetworkFact` (already exported, already
   built before `gatherNetworkRoute` folds it into an `Evaluation`) and
   derives `Blockers` directly from each `BridgeObservation` with
   `Status == "down"`, never from the folded evidence text. The
   permanent DNS-gap note is always a `Caveat`, never a `Blocker`.
4. **A resource this hive backs as a HAST replica for a *different*
   owner (`ReplicaBackedImpact`) must never appear in `Blockers` for a
   "why is this hive unsafe to reboot" answer** - it keeps running,
   unaffected, on its real owner if this hive reboots; only its
   redundancy is lost. Surfaced only as a `Caveat`
   (`TestAnswerHiveReboot_ReplicaBackedNeverAppearsInBlockers` regresses
   this).
5. **An owned resource with `Verdict == unverified_replica` (a replica
   *is* configured, but live HAST sync status can never be confirmed)
   must NOT be downgraded to a softer severity than `unprotected` (no
   replica at all)** - both populate `Blockers` with equal severity,
   differing only in the cited detail text
   (`TestAnswerHiveReboot_UnprotectedAndUnverifiedReplicaAreEquallySevere`).
   Treating "unverified" as merely informational would be exactly the
   "missing observation treated as a passed safety check" CODEX's own
   text warns against.
6. **The one real, code-verified proven remedy this feature has - "set
   `replica_node_id` first" - must not become a *third*, silently
   driftable hand-copy of `MigrateVM`/`MigrateJail`'s own rejection
   text** (two near-identical copies already live in
   `internal/manager/server.go`). A shared helper was considered, but
   `internal/manager`/`internal/cluster` pull in heavy OS-exec
   dependencies (`internal/zfs`/`internal/jail`/`internal/bhyve`/
   `internal/hast`) that no pure leaf package or `internal/frontend`
   has ever been allowed to import in this codebase - the established,
   repeatedly-applied convention at exactly this boundary is small,
   deliberate duplication (e.g. `internal/recovery.HASTResourceName`
   mirrors, but does not import, `internal/cluster/hast.go`'s own
   naming scheme), not a shared symbol reaching across it. Fixed the
   same way: `AnswerCellMigrate`'s doc comment in `internal/whynot/whynot.go`
   explicitly cites `MigrateVM`/`MigrateJail`'s exact precondition and
   file location as the thing it mirrors, cross-referencing back so a
   future change to one is easy to notice needs to update the other.
   "Proven" means the *described precondition* matches real,
   currently-enforced code, not that the sentence is
   character-identical to the RPC's own error string.
7. **Every question needs the same "a missing or mistyped target must
   never look safe" discipline ADR-0052 established for the Dependency
   Graph Simulator.** `hive-reboot` gets this for free by calling
   `SimulateNodeFailure` directly - its handler already runs
   `cluster.IsKnownTarget` and returns an explicit error string.
   `network-connectivity` needed an explicit `found` check against
   `currentNetworks(r)` added in `handleWhyNotPage` before ever
   building a `NetworkFact` - `gatherNetworkRoute` itself has no such
   check. The cell questions needed an explicit `GetVM`/`GetJail`
   `found` check in `lookupCell` (already required by finding 2's
   design) - `cluster.IsKnownTarget` was not reused here, since it
   answers a different question (raft-server-or-resource-placement),
   not "does this VM/jail ID exist at all."
8. **`Blocker.Invariant` is a stable, code-level provenance field, not
   free text** - populated with the underlying invariant's own `Name`
   (`"cell-recoverability"`, `"network-route-dns"`) where one exists, or
   a stable local id (`"migrate-replica-precondition"`,
   `"dependency-graph-simulator"`, `"quorum-tolerance"`) where the
   citation is a different already-shipped mechanism - so a caller can
   inspect or link to the source of a conclusion, not just read prose.
9. **Page shape: three input sections, not four.** Cell-migrate and
   cell-recoverable are answered from the exact same `CellFact` and the
   exact same operator-supplied Cell ID, so `/why-not` takes one
   `?cell_id=` (answering both questions together), one `?node_id=`
   (hive reboot), and one `?network_id=` (network connectivity) -
   CODEX's four example questions map to three distinct inputs, never
   four forms asking for the same Cell ID twice under different labels.

## Design

### `internal/whynot` (new pure package)

Mirrors `internal/health`/`internal/recovery`/`internal/invariant`'s
own precedent: zero OS/exec dependencies, zero import of
`internal/cluster` or `internal/manager` (finding 6). Imports
`internal/invariant` directly for `Evidence`/`Result`/`NetworkFact`/
`ResourceFact` - the same "reuse an already-pure sibling package's
exported types directly" pattern `internal/invariant` itself already
established by importing `internal/recovery`.

Four question functions, not one dispatcher - each has different
blocker-derivation rules that a single entry point would just relocate
behind one name for no benefit:

- `AnswerCellMigrate(f CellFact) Answer` - re-derives `MigrateVM`/
  `MigrateJail`'s real precondition (deleting state, empty
  `ReplicaNodeID`) directly; never delegates to
  `EvaluateCellRecoverability` (finding 1). The one `Remedy{Proven:
  true}` in this whole feature lives here.
- `AnswerCellRecoverable(f CellFact) Answer` - synthesizes the
  no-replica blocker itself (finding 2), otherwise delegates to
  `invariant.EvaluateCellRecoverability` for a single resource.
- `AnswerHiveReboot(nodeID string, quorum QuorumFact, owned
  []OwnedResourceFact, replicaBacked []ReplicaBackedFact) Answer` -
  `QuorumFact`/`OwnedResourceFact`/`ReplicaBackedFact` are small,
  deliberate duplicates of `internal/cluster.QuorumImpact`/
  `OwnedResourceImpact`/`ReplicaBackedImpact`'s shapes (finding 6's
  same reasoning). Quorum-not-surviving and every `unprotected`/
  `unverified_replica` owned resource are equally-severe `Blockers`
  (finding 5); `ReplicaBackedFact` entries are `Caveats` only (finding
  4).
- `AnswerNetworkConnectivity(fact invariant.NetworkFact) Answer` -
  takes the raw fact, not the folded `Evaluation` (finding 3).

`Answer{Question, Scope, Verdict, Blockers, Remedies, Caveats}`,
`Blocker{Invariant, Detail, Evidence}` (finding 8), `Remedy{Detail,
Proven}` (finding 6), `Verdict` (`blocked`/`unknown`/`clear` -
`VerdictClear` never means "guaranteed safe," only "no blocker found
within v1's observed scope").

### Frontend: `internal/frontend/why_not.go` (new file, same package)

New `GET /why-not` page, Viewer-accessible, **no new RPC or proto
change** - every fact comes from RPCs/gathering helpers that already
exist for other pages:

- `lookupCell(ctx, id, localNodeID) (whynot.CellFact, bool)` - `GetVM`
  then `GetJail`, reusing the existing `fromRPCVM`/`fromRPCJail`
  converters; one bounded `fetchHostStats` call to a VM's
  `ReplicaNodeID` only (jails get `ResultUnknown` unconditionally, no
  capability signal exists for jails anywhere in this codebase).
- `?cell_id=` calls `lookupCell` once and renders both
  `AnswerCellMigrate`/`AnswerCellRecoverable` (finding 9).
- `?node_id=` calls `s.client.SimulateNodeFailure` directly, exactly as
  `handleSimulatePage` already does, converting the response into
  `whynot.QuorumFact`/`OwnedResourceFact`/`ReplicaBackedFact` via the
  existing `fromRPCOwnedResourceImpact`/`fromRPCReplicaBackedImpact`
  converters from `simulate.go`.
- `?network_id=` (`answerNetworkConnectivity`) finds the target
  network's actual VM-owning nodes (via `currentVMs`, never
  `ListNetworks`'s own `bridge_status` field - the ADR-0055/0060-
  documented bug class) and fans `GetLocalNetworkBridgeStatus` out to
  just those nodes, scoped to the one requested network.
- `pageData` gains `WhyNot*` fields, additive only.
- Nav: "Why Not" added to the sidebar's Status section
  (`web/templates/layout.html`), alongside Invariants/Assumptions/
  Simulations/Recovery handbook.

### Template: `web/templates/why_not.html`

Mirrors `simulate.html`'s shape: one form per section (Cell/Hive/
Network); a shared `why_not_answer` sub-template renders a `Verdict`
badge (`blocked`/`unknown`/`clear` map directly onto this project's
existing `.badge.blocked`/`.badge.unknown`/`.badge.clear` CSS classes -
no translation needed), `Blockers` with expandable evidence, `Remedies`
labeled proven vs. plausible, and `Caveats` in a visually distinct,
muted "Disclosed limitations" section - never mixed into the Blockers
list.

## Consequences

- Non-HTML consumers get nothing new here - every fact used was
  already wire-exposed before this feature, and the `whynot.Answer*`
  functions are only ever invoked from this one frontend page handler,
  the same "retrofit existing surfaces, don't build a new RPC" posture
  ADR-0056/0057/0060 already took.
- Migrate-eligibility and cell-recoverability can genuinely disagree
  for the same resource (finding 1) - this is a real, disclosed
  property of Apiary's own current code, not a bug in either
  evaluator, and an operator reading both answers for one Cell ID needs
  to understand why they can differ.
- `cell-recoverable` and `network-connectivity` inherit the same
  permanent v1 ceiling their underlying invariants already have (never
  `Blocked` from a positive confirmation, never `Clear` at all for
  network connectivity - DNS observability doesn't exist anywhere).
- This engine never blocks an action or produces a Flight Plan - CODEX's
  own text explicitly defers that to a future execution engine that
  does not exist in Apiary yet. Recommendations here are always
  citations of already-shipped, already-tested mechanisms, never a new
  safety computation.
- The one proven remedy's text lives in `internal/whynot` as a
  hand-written description cross-referencing `MigrateVM`/`MigrateJail`'s
  real code, not a shared symbol - a future change to that
  precondition could silently make this remedy inaccurate without a
  compiler error; only the cross-referencing comments guard against
  drift (finding 6). This mirrors an already-accepted risk pattern in
  this codebase (`internal/recovery.HASTResourceName`), not a new one.
