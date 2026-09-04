# ADR-0062: Resilience Coverage Map v1

## Status

Accepted

## Context

CODEX.md names **Resilience Coverage Map**: enumerate meaningful
failure scenarios from the Dependency Graph and classify each as
simulated, physically rehearsed, stale, untested, or currently unsafe
or impossible to test, showing which cells, service contracts,
invariants, and recovery paths lack trustworthy evidence. Treat
coverage as evidence, not a vanity percentage: weight scenarios by
consequence and dependency reach, preserve the exact scope and date of
every test, and never imply that one successful probe validates a
different hive, network, storage path, or correlated failure.

This is the third of three related features chosen together this
session (Operational Invariants, then Why Not Engine, then Resilience
Coverage Map). CODEX's own third bullet - "use the map to schedule or
recommend Continuous Recovery Proof and Synthetic Infrastructure
Checks, subject to Flight Plans and collision detection" - names four
other CODEX concepts with zero implementation anywhere in this
codebase: no snapshot/clone support, no disposable probe-cell
mechanism, no scheduling/collision engine. That whole bullet is out of
scope for v1, the same honest scoping every prior feature this session
has applied - this stays diagnostic-only.

Almost every scenario this feature needs already has a purpose-built,
already-shipped evaluator: the Dependency Graph Simulator's
`SimulateNodeFailure`/`SimulateNetworkFailure` RPCs (ADR-0052/0053) and
the Operational Invariants catalog (ADR-0060). This feature is
therefore mostly a synthesis and classification layer, not a new safety
computation.

## Design review

A design review pass (a Plan-agent critique, mirroring every prior
feature's one hard review) found the initial draft would have repeated
a mistake ADR-0060 already caught once, missed an already-shipped
simulator entirely, and misapplied its own "unsafe to rehearse"
classification to scenarios that are actually just describing an
already-current fault.

1. **Looping the `SimulateNodeFailure` RPC once per node to build
   hive-failure scenarios would repeat ADR-0060 finding 3's own
   already-fixed mistake, at a worse multiplier.** ADR-0060 rejected a
   per-voter `SimulateNodeFailure` fan-out for quorum-tolerance
   specifically because the RPC re-runs `ListVMs`/`ListJails`/`Status`
   and (since ADR-0054) fans `ListISOs` out to every remaining server,
   all on the leader, for whatever single target it's asked about.
   Looping it over every node in `simulateNodeChoices(r)` (which
   includes stale, long-removed placement-only nodes, not just current
   voters) would cost O(candidateNodes x clusterServers) RPCs on one
   page load, just to extract two counts and one verdict per node.
   Every existing caller of this RPC (`/simulate`, Why Not Engine's
   `AnswerHiveReboot`) calls it exactly once, for one operator-chosen
   target - never in a sweep. **Fixed**: this page never calls
   `SimulateNodeFailure` at all. Owned/replica-backed counts per node
   come from a local loop over the VM/jail lists already fetched for
   `gatherCellRecoverability` - zero RPCs. Per-node quorum-loss comes
   from a new exported `internal/invariant.ClassifyVoterQuorumImpacts`
   (mirroring the exact fix ADR-0061 finding 3 applied to `NetworkFact`,
   exposing structured data instead of folded Evidence prose),
   extracted from `EvaluateQuorumTolerance`'s own per-voter loop without
   changing its behavior - verified against its existing 13-test suite,
   now 17 with the new function's own tests. `gatherQuorumTolerance`
   itself was further split so the underlying
   `[]invariant.VoterReachability` snapshot (one bounded `HostStats`
   fan-out) is reusable by both the folded `Evaluation` and the new
   per-voter classification, rather than fetched twice or duplicated.
2. **`ManagerService.SimulateNetworkFailure` (ADR-0053) - a second,
   already-shipped, already-wired counterfactual simulator - was
   missing from the initial draft entirely**, even though it is the
   obviously correct source for a "network failure" scenario kind,
   structurally parallel to `SimulateNodeFailure` for hive-failure. It
   answers a genuinely different question than the `network-route-dns`
   invariant: "which Cells would lose this managed network's declared
   attachment if it disappeared" (a counterfactual blast radius) vs.
   "is this network's bridge currently reporting up" (current bridge
   health). Both belong on the map as distinct scenario kinds. Unlike
   `SimulateNodeFailure`, this RPC's handler does only two sequential
   leader-only reads (`ListNetworks`, `ListVMs`) with no per-server
   fan-out - looping it once per managed network (a small, bounded
   count, bounded concurrently the same way every prior fan-out in this
   codebase is) is cheap and fine.
3. **The "rehearsing this scenario for real would itself be the
   incident" (`unsafe_or_impossible`) classification is sound for
   hive-failure and quorum-tolerance, but was wrongly proposed for
   cell-recoverability and network-connectivity too.**
   `cell-recoverability`'s `False` means "this replica destination is
   CONFIRMED incapable RIGHT NOW" and `network-route-dns`'s `False`
   means "a bridge is reported down RIGHT NOW" - both describe an
   already-active, present-tense fault, not a hypothetical future loss
   whose rehearsal would be dangerous. There is nothing to "rehearse"
   about a bridge that is already down; labeling that row
   `unsafe_or_impossible` would misdescribe an active incident as an
   untestable hypothetical. **Fixed**: `cell-recoverability` and
   `network-connectivity` always resolve `simulated` regardless of
   `Result` - a confirmed-bad `Result` is exactly the evidence this
   feature exists to surface, not a testing hazard.
   **Correction made to the review agent's own suggestion**: it grouped
   `quorum-tolerance` in with cell-recoverability/network-connectivity
   for this same "always simulated" treatment. That is wrong on the
   actual semantics, verified directly against
   `EvaluateQuorumTolerance`'s implementation: its `ResultFalse` case
   fires only when some voter's *simulated* (not-yet-happened) loss
   would lose quorum - the cluster currently HAS quorum for this
   evaluation to even run meaningfully. This is a genuine hypothetical
   about a future loss, structurally identical to hive-failure's own
   quorum-loss check (quorum-tolerance is that same fact aggregated
   across every voter, rather than reported per node). Rehearsing
   "let's find out which voter is the weak link by actually removing
   voters" when this is `false` would risk causing a real quorum loss -
   the same hazard hive-failure's own per-node check names. `quorum-
   tolerance` therefore classifies `unsafe_or_impossible` on
   `Result == false`, grouped with hive-failure's own derivation rule,
   not with the two "already-current-fault" kinds.
4. **`hast-dual-primary` must not be labeled `simulated`.**
   `invariant.EvaluateHASTDualPrimary` is structurally incapable of ever
   resolving anything but `ResultUnknown` - no code path exists that
   could ever confirm or deny it. Labeling that "simulated" would imply
   a mechanism producing real evidence, exactly the false impression
   CODEX's "lack trustworthy evidence" language warns against.
   **Fixed**: classified `untested` - "a mechanism runs but can never
   produce a confirmed answer" is, for this feature's purpose,
   functionally identical to "no mechanism exists."
5. **`ownership-gated-deletion` (the fifth, static, always-`True`,
   non-live invariant) is excluded from the map entirely** - it is not
   a failure scenario with a coverage gap to track, it is a structural
   code guarantee already segregated into `/invariants`' own separate
   "structural guarantees" section for exactly this reason.
6. **A numeric weight formula invents false precision that CODEX's own
   "not a vanity percentage" language directly warns against** - moving
   invented scoring from a displayed percentage into a hidden sort key
   doesn't fix the underlying problem, it just hides it. **Fixed**:
   three coarse, honestly-labeled tiers with no numeric weighting
   inside them - cluster-wide (quorum-tolerance: the whole cluster's
   fate), multi-resource (hive-failure, network-failure: blast radius
   varies but is never reduced to an invented number), single-resource
   (cell-recoverability, network-connectivity, hast-dual-primary: each
   scoped to one resource/network). Sorted by tier, then Status
   (`unsafe_or_impossible` first - most actionable), then Kind/Target
   alphabetically for determinism.
7. **The disclosed-gaps list needed correcting against actual code, not
   just ADR-0052's now-partly-stale prose, and needed visual separation
   from computed rows.** Image-source availability was already closed
   by ADR-0054 (`cluster.ComputeImageAvailability`, wired into
   `SimulateNodeFailure`) - dropped from the list. "Uplink loss" is not
   quite "zero mechanism at all": `internal/assumecheck` already has
   live, per-node uplink-configuration-correctness checks surfaced on
   `/assumptions` today - the real, still-open gap is narrower ("no
   failure-*impact* simulation for uplink loss, unlike node/network
   failure"), not a blanket absence. Kept as a gap, reworded precisely;
   added "correlated/simultaneous multi-node failure" (ADR-0052's own
   explicit v1 limit: only ever one node at a time) and the meta-gap
   that explains why `physically_rehearsed`/`stale` are permanently
   unreachable in v1 (no persisted test-execution ledger exists
   anywhere in this codebase - every scenario here is computed fresh at
   page-load time, never cached or dated from an earlier run). The
   `Gaps` list is returned from a pure, testable function
   (`coverage.KnownGaps`), but renders in a visually distinct "known
   unmodeled failure classes" callout, never interleaved with the table
   of computed `Scenario` rows - the same segregation `/invariants`
   already applies to `ownership-gated-deletion`.
8. **Route naming**: every comparable page's route matches its nav
   label in kebab-case (`/recovery-handbook`, `/why-not`, `/invariants`,
   `/simulate`) - `/coverage` alone would be ambiguous (test coverage?
   network coverage?). **Fixed**: `GET /resilience-coverage`, nav label
   "Coverage Map".
9. **"Never imply one probe validates a different target" needs an
   explicit on-page statement, not just independent row scoping.**
   Per-row independence is necessary but not sufficient - a `Counts`
   breakdown, even as plain text, still invites a reader to mentally
   aggregate it into an informal score. **Fixed**: CODEX's own caveat
   sentence is rendered verbatim on the page (in the header subtitle
   and again beside the Counts tally); `Counts` is a plain tally (every
   Status including the permanently-zero `physically_rehearsed`/
   `stale`, made explicit rather than silently absent) rendered below
   the scenario table, never styled as a progress bar or placed as a
   headline metric.
10. **A real, unrelated `.gitignore` collision was caught during
    implementation, not during review**: this project's existing
    `coverage.*` glob (meant for Go's own `coverage.out`/`coverage.html`
    test-artifact output) also silently matched this feature's own
    source files, since they were initially named `coverage.go`/
    `coverage.html` throughout. `git add`/`git status` gave no warning -
    the files simply never appeared as untracked. Caught only by
    diffing an expected file list against actual `git status` output
    before committing. Fixed by renaming this feature's files
    (`internal/coverage/scenario.go`, `internal/frontend/
    resilience_coverage.go`, `web/templates/resilience_coverage.html`)
    rather than narrowing the shared, pre-existing `.gitignore` rule -
    altering repo-wide ignore behavior for Go tooling is a bigger, less
    obviously-correct change than renaming three new files, and the
    rename also makes every filename match the route name
    (`/resilience-coverage`) more clearly than the original "coverage"
    name did. The `internal/coverage` package name itself is unaffected
    (a bare directory name has no dot, so it was never matched).

## Design

### `internal/invariant` (existing package, one additive change)

`ClassifyVoterQuorumImpacts(voters []VoterReachability, leaderID string) []VoterQuorumImpact` -
extracted from `EvaluateQuorumTolerance`'s existing per-voter loop,
which now calls it internally. `VoterQuorumImpact{NodeID string,
Verdict recovery.QuorumVerdict, Valid bool}` - `Valid` false means the
underlying `QuorumFact` was internally inconsistent, mirroring
`ValidQuorumFact`'s own guard; `Verdict` is meaningless in that case
and must never be treated as a confirmed finding.
`internal/frontend/invariants.go`'s `gatherQuorumTolerance` was split
into a new `gatherVoterReachability` (the bounded `HostStats` fan-out
alone) plus a thin wrapper, so Resilience Coverage Map reuses the exact
same reachability snapshot for both the folded `Evaluation` and the new
per-voter classification - one fan-out, not two.

### `internal/coverage` (new pure package: `internal/coverage/scenario.go`)

Mirrors `internal/health`/`internal/recovery`/`internal/invariant`/
`internal/whynot`'s own precedent: zero OS/exec dependencies, imports
`internal/invariant` and `internal/recovery` directly (pure-to-pure
reuse, the same pattern `internal/invariant` already established),
never `internal/cluster`/`internal/manager`.

`Status` (CODEX's own five-word vocabulary: `simulated`,
`physically_rehearsed` and `stale` unreachable in v1, `untested`,
`unsafe_or_impossible`), `Tier` (`SingleResource`/`MultiResource`/
`ClusterWide` - no numeric weighting), `Scenario{Kind, Target, Label,
Status, Tier, Result, Explanation}`, `Report{Scenarios, Gaps, Counts}`.
Six `Classify*` functions (one per scenario kind, not a dispatcher,
matching `internal/pathtrace`/`internal/whynot`'s own precedent of
keeping genuinely different derivation rules in separate named
functions) plus `BuildReport` (deterministic sort, zero-filled Counts
across all five statuses, attaches `KnownGaps()`).

### Frontend: `internal/frontend/resilience_coverage.go` (new file, same package)

New `GET /resilience-coverage` page, Viewer-accessible, no new RPC or
proto change beyond the one additive `internal/invariant` function:

- Quorum-tolerance + hive-failure: `gatherVoterReachability` (shared
  fan-out) feeds both `invariant.EvaluateQuorumTolerance` (the
  cluster-wide row) and `invariant.ClassifyVoterQuorumImpacts` (per-
  node data); owned/replica-backed counts come from a local loop over
  the already-fetched VM/jail lists - zero extra RPCs.
- Network-failure: a bounded concurrent loop (the same
  `nodeContextTimeout`/`nodeContextOverallTimeout`/`nodeContextLimit`
  constants from `recovery_handbook.go`) over `currentNetworks(r)`
  calling `SimulateNetworkFailure` once per network.
- Network-connectivity, cell-recoverability, hast-dual-primary: calls
  `gatherNetworkRoute`, `gatherCellRecoverability`,
  `invariant.EvaluateHASTDualPrimary` exactly as `handleInvariantsPage`
  already does, verbatim.
- `coverageBadgeClass` maps each `coverage.Status` onto this project's
  already-defined badge CSS classes (`true`/`false`/`unknown`/`stale`)
  - no new CSS; `simulated` reuses the "healthy" styling regardless of
  the scenario's own `Result`, since `Status` describes whether
  evidence exists, not whether the evidence is good news.
- `pageData` gains `Coverage*` fields, additive only. Nav: "Coverage
  Map" added to the sidebar's "Status" section.

### Template: `web/templates/resilience_coverage.html`

One table of computed `Scenario` rows (already sorted by tier/status/
kind/target from `BuildReport`), a plain `Counts` tally with CODEX's
caveat sentence rendered verbatim, and a visually separate "Known
unmodeled failure classes" callout for `Gaps` - never interleaved with
the scenario table.

## Consequences

- Non-HTML consumers get nothing new here - every fact used was
  already wire-exposed before this feature, and the `coverage.Classify*`
  functions are only ever invoked from this one frontend page handler,
  the same "retrofit existing surfaces, don't build a new RPC" posture
  every prior feature this session has taken.
- `physically_rehearsed` and `stale` are permanently unreachable in
  v1 - closing that gap means building a persisted test-execution
  ledger, which this ADR deliberately does not attempt (CODEX's own
  "Continuous Recovery Proof" and "Synthetic Infrastructure Checks" are
  the concepts that would eventually produce that evidence, and neither
  exists yet).
- `hast-dual-primary` can never resolve better than `untested` for the
  same permanent reason ADR-0060 already disclosed: live HAST role/sync
  status has zero RPC exposure anywhere in this codebase.
- The "schedule or recommend Continuous Recovery Proof / Synthetic
  Infrastructure Checks" half of CODEX's own vision remains entirely
  out of scope until those execution mechanisms exist - this map is
  diagnostic only in v1, exactly like every prior feature's own
  disclosed non-goal.
