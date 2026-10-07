# ADR-0151: AI-agent cell API - implementation and v1 decisions

Status: Implemented
Supersedes: None (amends ADR-0150)
Superseded by: None
Affected projects: Apiary
Legacy identifier: ADR-0151 (Apiary)
Loreloom identifier: Pending allocation by Glen

## Status qualifications

This ADR documents the v1 implementation of ADR-0150's `CreateCell` and
`RecommendCellType` RPCs, and resolves the six open questions ADR-0150
deliberately left for Glen. ADR-0150 itself stays in place as the
design record; this one is additive, not a revision of its reasoning.

## Context

ADR-0150 proposed both RPCs' shapes but implemented nothing and left
six open questions unresolved. The task that produced this ADR asked
for a real, working v1: "the smallest, most reasonable v1 decision that
lets the feature actually work end-to-end," with any single question
too architecturally significant to decide blind left genuinely
unbuilt and flagged, rather than guessed at.

All six questions turned out to have a reasonable v1 default; none
required inventing a new security boundary, resource-limits policy, or
anything irreversible once other code depends on it. Every decision
below is a decision, not a settled fact - Glen can override any of
them.

## Decisions made

1. **Does `RecommendCellType` need Raft/the leader at all?** No. It is
   pure computation (`internal/cellrecommend`) over the request's own
   `WorkloadDescription` - no cluster state is consulted at all in v1
   (see decision 4 below), so it is answerable by any node, exactly
   like `HostStats`/`ClusterHealth`/`whynot`. It never forwards to the
   leader.

2. **Is `CreateCell` the right shape, or should type resolution live
   only in `RecommendCellType`?** `CreateCell` resolves `cell_type`
   itself, by calling `internal/cellrecommend.Recommend` directly - the
   exact same function `RecommendCellType` calls. There is exactly one
   copy of the selection logic, not two that could drift, which was
   ADR-0150's own stated assumption. The two RPCs stay independent:
   calling one never requires the other.

3. **Should there be a unified `GetCell`/`ListCells`?** Not built in
   v1. `CreateCellResponse.cell_type` tells the caller which of
   `GetVM`/`GetJail` to call next, and the full `VMDefinition`/
   `JailDefinition` is already inline in the response, so nothing is
   blocked on this. A unified read/list/watch surface remains a
   separate, larger design question, same as ADR-0150 said.

4. **Wire format / restshim translation.** `CreateCell` and
   `RecommendCellType` get explicit REST routes
   (`POST /v1/cells`, `POST /v1/cells/recommend`) and hand-written
   JSON shapes in `internal/restshim/convert.go`, exactly like every
   other RPC in this service - `internal/restshim` has no generic
   reflection-based translation for any RPC, so there was never a
   question of whether enums/repeated messages "just work" generically.
   `CellType`/`RecommendationFactor`/`RecommendationConfidence` all
   render as fixed lowercase strings (`"vm"`, `"isolation_required"`,
   `"high"`), not numeric enum values, matching this layer's existing
   `VMState`/`JailState` string convention.

5. **How exhaustive should `RecommendationFactor` be, and who owns
   extending it?** v1 ships exactly the five factors ADR-0150 proposed
   plus `FACTOR_UNSPECIFIED`. The forward-compatibility rule is
   documented on the enum itself (manager.proto): a caller receiving an
   unrecognized future factor value must treat it as informational
   evidence, never as a value it refuses to proceed over. Nothing in
   the schema enforces this mechanically - it is a documented
   convention, the same as proto3's own "ignore unknown enum values"
   norm, not a new mechanism.

6. **`resource_profile` validation ordering.** Validation happens
   AFTER `cell_type` is resolved, using the existing per-kind
   `applyCreateVM`/`applyCreateJail` checks unchanged (including the
   ADR-0148 disk-size floor for a VM). `cellrecommend.Recommend` never
   reads `resource_profile` as an input to the type decision itself in
   v1 - only `WorkloadDescription`'s own estimate fields
   (`estimated_vcpus`/`estimated_memory_mb`) feed the recommendation,
   and `resource_profile` is the separate, later value actually applied
   to the created VM/jail. This keeps the two concerns (what the
   workload implies vs. what to actually provision) from being
   silently conflated.

## A seventh decision ADR-0150 did not anticipate

ADR-0150 proposed `CreateCellRequest.node_id` as **optional**, with
"Apiary picks a node" for capability 1. Implementation found no
node-placement or scheduling logic anywhere in this codebase:
`CreateVM`/`CreateJail` both require the caller to already know
`node_id`, and nothing resembling bin-packing, spread, or
HAST-awareness-based placement exists to reuse or extend. Implementing
"Apiary picks a node" would have meant inventing Apiary's first
scheduler as a side effect of this feature, guessing at a policy nobody
asked for.

**v1 decision: `node_id` is REQUIRED on `CreateCellRequest`.** This
keeps placement exactly where every other creation path in this
codebase already puts it - the caller's choice - and defers "Apiary
picks a node" to whenever a real scheduler is designed on its own
terms. `workload_hint` is likewise REQUIRED (not optional as
ADR-0150 proposed): `CreateCell`'s whole point is Apiary deciding
vm-vs-jail, and v1 has no other signal in the request to decide from.
A caller that doesn't want to describe a workload can call
`CreateVM`/`CreateJail` directly instead - that path is unaffected by
this ADR.

## What was built

- `CreateCell(CreateCellRequest) -> CreateCellResponse` and
  `RecommendCellType(RecommendCellTypeRequest) ->
  RecommendCellTypeResponse` in `api/rpc/manager.proto`, with the
  `CellType`/`ResourceProfile`/`WorkloadDescription`/
  `RecommendationFactor`/`RecommendationConfidence`/
  `RecommendationReason` messages/enums ADR-0150 proposed, generated
  into `api/rpc/manager.pb.go`/`manager_grpc.pb.go`.
- `internal/cellrecommend`, a new pure-computation package (no I/O,
  no cluster-state lookups, mirroring `internal/whynot`'s own posture)
  implementing the rule evaluation: hard requirements
  (`needs_custom_kernel_or_os`, then `needs_full_isolation`) are
  checked first and are decisive at `CONFIDENCE_HIGH`; soft signals
  (migration/snapshot expectation, density priority, resource
  estimate) are tallied as votes, with `CONFIDENCE_MEDIUM` when they
  agree, `CONFIDENCE_LOW` with `alternative_cell_type` set when they
  conflict or when no signal is present at all (defaulting to the
  lighter-weight jail).
- `internal/manager.Server.CreateCell`/`RecommendCellType`, with
  `CreateCell` building a `VMDefinition`/`JailDefinition` from the
  resolved type and issuing the existing `Command_CreateVm`/
  `Command_CreateJail` through `applyCommand`/`applyJailCommand` - the
  same Raft-forward-to-leader, FSM-apply, reconciler-provisioning path
  `CreateVM`/`CreateJail` already use. No new FSM command kind exists.
- `PeerForwarder.CreateCell`/`PeerReporter.CreateCell` for the same
  leader-forwarding-on-rejection pattern every other write RPC uses.
- Auth tiers (`internal/manager/auth.go`): `CreateCell` is
  `RoleOperator` (matches `CreateVM`/`CreateJail`); `RecommendCellType`
  is `RoleViewer` (matches other read-only advisory RPCs).
- REST routes `POST /v1/cells` / `POST /v1/cells/recommend`
  (`internal/restshim`), with hand-written JSON conversion in
  `convert.go` following this layer's existing per-RPC pattern.
- Integration tests (`internal/manager/integration_test.go`) proving
  `CreateCell` actually resolves to both kinds and that the resulting
  VM/jail is reachable through the real `GetVM`/`GetJail` path (not a
  parallel bookkeeping record), plus request-validation and
  duplicate-id tests sharing `applyCreateVM`'s own check. Unit tests
  for `internal/cellrecommend`'s rule evaluation and for the REST
  translation layer (`internal/restshim`).
