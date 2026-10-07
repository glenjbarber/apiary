# ADR-0150: AI-agent-facing cell creation and cell-type recommendation API

Status: Unconfirmed
Supersedes: None
Superseded by: None
Affected projects: Apiary
Legacy identifier: ADR-0150 (Apiary)
Loreloom identifier: Pending allocation by Glen

## Status qualifications

**UNCONFIRMED. Not accepted, not implemented.**

This ADR is a design proposal only. Per the task that produced it, the
wire format of the recommendation response, the request/response
schema for cell creation, and how either maps onto Apiary's existing
gRPC/REST APIs are explicitly undecided. Implementing a final API
against an undecided wire format risks building the wrong thing and
redoing it, so this document proposes shapes for review and discussion;
no proto field, RPC, FSM command, or code in this repository reflects
any of it yet.

Loreloom-register allocation, if this proposal is accepted, is Glen's
to do - this document only reserves the next available apiary-local
`docs/adr/` number.

## Context

Scope, per the originating Notion task ("Todo: URGENT: APIARY API
integration"), confirmed with Glen 2026-10-07: design an API, callable
by an AI agent such as orcli, with two independent capabilities:

1. **Create a cell.** "Cell" is Apiary's general term for an instance,
   whether a VM or a jail. The calling agent asks for a cell to be
   created without specifying VM or jail - that choice belongs to
   Apiary, not the caller, for this capability.
2. **Recommend a cell type for a workload.** Given a workload
   description, Apiary returns a recommendation of which cell type
   suits it, in a structured format the calling agent can parse and
   act on.

These two capabilities are independent: calling one does not require
or trigger the other. A caller may ask for a recommendation and decide
for itself whether to act on it, or may ask Apiary to create a cell
without ever calling the recommendation endpoint.

### What exists today

Apiary has no "cell" abstraction in code. VMs and jails are two
distinct, symmetric RPC families, and the caller - human or script -
chooses which one it wants up front:

- `CreateVM(CreateVMRequest{vm: VMDefinition, timeout_ms})` ->
  `CreateVMResponse{vm, error, leader_hint}` (`api/rpc/manager.pb.go`).
  `VMDefinition` carries `id`, `name`, `vcpus`, `memory_mb`, `node_id`,
  `desired_state`, `phase`, `phase_error`, `iso_name`, `network_id`,
  `ip_address`/`mac_address` (read-only, server-assigned),
  `firewall_rules`, `replica_node_id`, `base_image_name`,
  `firewall_paused`, `cloudflare_hostname`/`cloudflare_port`,
  `clone_from_snapshot`, `disk_size_mb`.
- `CreateJail(CreateJailRequest{jail: JailDefinition})` ->
  `CreateJailResponse{jail}`. `JailDefinition` carries `id`, `name`,
  `hostname`, `node_id`, `replica_node_id`, `desired_state`, `phase`,
  `phase_error`, `base_template`, `base_archive_name`, `network_id`,
  `ip_address`, `vnet`, `firewall_rules`. Jails have no vcpus, memory,
  ISO, or bhyve-specific fields (`internal/cluster/plan.go`
  `JailPlacement`, cited directly in
  `internal/frontend/guided_create.go`).

Both RPCs are handled in `internal/manager/server.go`, which forwards
writes to the Raft leader (ADR-0029, ADR-0037) and applies them through
the FSM: `internal/raft/fsm.go` `applyCreateVM` / `applyCreateJail`
validate the id (alphanumeric/`-`/`_`, <=64 chars, must not already
exist) and kind-specific fields (hostname uniqueness and charset for
jails, `clone_from_snapshot` shape and network existence for VMs, a
per-kind disk-size floor check), then append a new `VMDefinition` /
`JailDefinition` row with `desired_state` set and `phase` left for the
reconciler. `internal/cluster/reconciler.go` (ADR-0012/ADR-0013) and
`internal/cluster/jail.go` (`ensureJail`, ADR-0007/ADR-0027) are what
actually provision the bhyve VM or the jail dataset/process once the
FSM command has committed - placement (which node, snapshot-clone vs.
base-image seed, HAST replica) is decided by the fields the caller
supplied in the request, not inferred by Apiary.

`internal/frontend/guided_create.go` is the one place today that
presents VM-vs-jail as a single step ("step 1's answer"), but it is a
UI wizard that still submits to the unchanged `POST /vms` /
`POST /jails` HTTP endpoints (restshimd's REST translation of
`CreateVM`/`CreateJail`, ADR-0011/ADR-0024) - the human picks VM or
jail in the form before anything is sent; no server-side selection
logic exists. There is no VM-creation RPC distinct from what's
described above beyond `CreateVM` itself, and no existing endpoint
anywhere in the repository returns a recommendation-shaped response
(a repo-wide search for "recommend" turns up only unrelated uses:
assumption-check "recommended actions", `whynot`, disk-size
recommendations in `internal/raft/colonydisksize.go` - none of these is
cell-type selection for a workload).

`cmd/apiaryctl` is a cluster-administration CLI (join/introduce,
trusted-peer pinning, force-restart, install) and has no VM/jail
creation command. `cmd/restshimd` is a generic gRPC<->REST translation
shim (ADR-0011/ADR-0024); it does not add creation logic of its own,
it exposes whatever the manager RPC surface already defines. So today,
every external caller - human via the web UI, or script via REST/gRPC
- must already know whether it wants a VM or a jail, and must supply a
kind-correct request; nothing in the stack performs workload-based
selection, and nothing returns a machine-parsable recommendation.

### Why this matters for an AI-agent caller specifically

An AI agent such as orcli does not necessarily know, and should not
need to determine on its own, Apiary's internal VM-vs-jail distinction
(bhyve full virtualization vs. FreeBSD jail containerization, their
different placement rules, image types, and field sets). Capability 1
exists so the agent can express *intent* ("give me a cell for this
workload") without first making a decision that is really an Apiary
operational concern. Capability 2 exists so an agent that already has
a workload description, but does not want to delegate the choice (or
wants to choose consciously, e.g. for cost/HA/isolation visibility),
can get Apiary's judgment back as data instead of prose.

## Proposal

### Guiding constraint

Build on the existing gRPC API rather than inventing a parallel one.
No reason was found in the existing code that the current
request/response shape, Raft-forwarding, or FSM-apply pattern cannot
fit an AI-agent caller - the gap is purely that no endpoint today lets
the caller omit the VM-vs-jail choice, and no endpoint returns a
recommendation. Both additions fit the same manager-RPC ->
Raft-forward -> FSM-apply -> reconciler pattern `CreateVM`/`CreateJail`
already use, with one exception noted below for recommendation (which
is pure computation, not cluster state, and likely does not need to go
through Raft at all).

### Capability 1: `CreateCell`

A new RPC, `CreateCell(CreateCellRequest) -> CreateCellResponse`,
sibling to `CreateVM`/`CreateJail` in the same service.

**Request shape (field list, not final `.proto` syntax):**

- `id` (string, required) - same identity contract as
  `VMDefinition.id`/`JailDefinition.id` today (caller-supplied,
  alphanumeric/`-`/`_`, <=64 chars, must not already exist).
- `name` (string, required) - display name, mirrors both kinds' `name`.
- `workload_hint` (message, optional) - the same shape proposed for
  `RecommendCellTypeRequest.workload` below (see Capability 2). When
  present, Apiary uses it to choose VM vs. jail. This is what lets a
  caller who already described its workload for a recommendation reuse
  that description verbatim for creation, without a second round trip
  it doesn't need to make to get the same choice applied.
- `node_id` (string, optional) - placement hint; empty means Apiary
  picks a node, mirroring today's manual field but making it optional
  for a caller that doesn't care.
- `resource_profile` (message, optional) - a kind-agnostic resource
  ask: `vcpus` (uint32), `memory_mb` (uint64), `disk_size_mb` (uint64).
  Ignored for the fields a jail doesn't have if Apiary resolves to a
  jail; this is the capability's whole point - the caller speaks in
  resources and intent, not in VM/jail-specific fields.
- `network_id` (string, optional) - both kinds already support this
  field with identical semantics, so it passes straight through
  regardless of which kind is chosen.
- `firewall_rules` (repeated `FirewallRule`, optional) - likewise
  already identical between `VMDefinition` and `JailDefinition`.
- `timeout_ms` (uint32, optional) - mirrors `CreateVMRequest`.

**Response shape:**

- `cell_id` (string) - echoes the request id.
- `cell_type` (enum: `CELL_TYPE_UNSPECIFIED`, `CELL_TYPE_VM`,
  `CELL_TYPE_JAIL`) - states Apiary's choice, so the caller can find
  the matching `GetVM`/`GetJail` (or a future `GetCell`, see open
  questions) afterward and knows which kind-specific fields apply.
  Returning this is the one piece of information a caller *must* get
  back even though it didn't ask for the distinction - otherwise it
  can't look up what was made.
- `vm` (`VMDefinition`, set iff `cell_type == CELL_TYPE_VM`) /
  `jail` (`JailDefinition`, set iff `cell_type == CELL_TYPE_JAIL`) -
  reuses the existing messages rather than inventing a merged "cell"
  message, so every field the caller might need (phase, phase_error,
  assigned IP, etc.) is already defined and already has reconciler
  support.
- `error` (string), `leader_hint` (string) - mirrors
  `CreateVMResponse`.

**Server-side flow:** `CreateCell` is handled in
`internal/manager/server.go` alongside `CreateVM`/`CreateJail`. It
resolves `cell_type` (using the same selection logic proposed for
`RecommendCellType` below, run internally rather than requiring the
caller to call it first), builds the corresponding `VMDefinition` or
`JailDefinition` from the request's kind-agnostic fields, and from that
point proceeds exactly as `applyCreateVM`/`applyCreateJail` already do
- same Raft forward, same FSM validation, same id/network/hostname
checks, same reconciler provisioning. No new FSM command kind is
strictly required if `CreateCell` is implemented as "resolve the type,
then internally issue the existing `Command_CreateVm` /
`Command_CreateJail`"; this keeps the Raft log and the FSM's replay
determinism (ADR-0003's concern, exercised by
`fsm_determinism_test.go`) exactly as they are today, with the type
decision made once, by the leader, before the command is logged
(rather than re-decided on every follower replay).

### Capability 2: `RecommendCellType`

A new RPC, `RecommendCellType(RecommendCellTypeRequest) ->
RecommendCellTypeResponse`. Unlike `CreateCell`, this is pure
computation over caller-supplied input and current cluster state; it
does not mutate anything, so it plausibly does not need Raft
forwarding or FSM involvement at all - it can likely be answered by
any node, leader or not, the same way existing read-only/advisory RPCs
(e.g. `whynot`, `colonydisksize`'s recommendation logic) are answered
today without going through the log. This is called out explicitly as
an open question below, since it changes the implementation's shape
non-trivially.

**Request shape:**

- `workload` (message, required) - the workload description:
  - `description` (string, optional) - free-text description, for
    logging/audit and for any future heuristic that wants it; not
    required to be machine-parsable on its own.
  - `needs_full_isolation` (bool, optional) - true if the workload
    needs kernel-level isolation from the host (untrusted code,
    multi-tenant, or needs its own kernel/sysctl tuning) - the
    strongest signal toward VM.
  - `needs_custom_kernel_or_os` (bool, optional) - true if the
    workload is not FreeBSD-compatible at the jail level (a different
    OS, or FreeBSD features a jail can't provide) - forces VM.
  - `estimated_vcpus` (uint32, optional), `estimated_memory_mb`
    (uint64, optional), `estimated_disk_mb` (uint64, optional) -
    resource estimate; absence means "unknown", not "zero".
  - `expects_live_migration_or_snapshot` (bool, optional) - signals
    toward VM, since VM snapshot/restore and migration (ADR-0090,
    ADR-0028) are more mature than the jail equivalents.
  - `density_priority` (bool, optional) - true if the caller wants
    many lightweight instances per node over per-instance isolation -
    signal toward jail.
- `node_id` (string, optional) - if the caller already knows (or
  cares about) placement, scope the recommendation to what's feasible
  on that node (e.g. available base images/templates there).

**Response shape - the part the task calls out as needing to be
"concrete" and "unambiguous for a calling AI to parse":**

- `recommended_cell_type` (enum: `CELL_TYPE_VM`, `CELL_TYPE_JAIL`) -
  the single answer. An agent that wants exactly one bit of
  information reads exactly this field and nothing else.
- `confidence` (enum: `CONFIDENCE_LOW`, `CONFIDENCE_MEDIUM`,
  `CONFIDENCE_HIGH`) - so a caller can decide whether to act
  automatically or surface the choice to a human; deliberately an
  enum, not a float, so "what does 0.73 mean" is never a question a
  calling agent has to answer.
- `reasons` (repeated message) - the evidence, each entry:
  - `factor` (enum, e.g. `FACTOR_ISOLATION_REQUIRED`,
    `FACTOR_CUSTOM_KERNEL_REQUIRED`, `FACTOR_MIGRATION_SUPPORT`,
    `FACTOR_DENSITY_PRIORITY`, `FACTOR_RESOURCE_PROFILE`) - a closed,
    enumerated reason code, not a free-text string, so a calling agent
    can branch on it programmatically rather than parsing prose.
    (A `detail` string field alongside each `factor` is reasonable for
    a human-readable explanation, but the agent-facing contract is the
    enum.)
  - `favors` (enum: `CELL_TYPE_VM`, `CELL_TYPE_JAIL`) - which way this
    factor pointed.
- `alternative_cell_type` (enum, optional) - the other kind, present
  only when the decision was close (ties into `confidence`), so a
  caller that wants to second-guess a low-confidence recommendation
  doesn't have to infer "the other one" itself.

This shape (a single enum "the answer", a closed enum for confidence,
and a repeated closed-enum list of reasons) is the concrete proposal
for "unambiguous for a calling AI to parse": every field a program
would branch on is an enum with a fixed value set, not a string to
pattern-match or a float threshold to tune against.

**Server-side flow:** stateless, callable on any node (pending the open
question below on whether that is actually safe given cluster-state
inputs like `node_id`-scoped image availability). A simple
rule-evaluation function - not ML, nothing resembling model inference -
walks the `workload` fields against the `reasons` factors in a fixed
order (hard requirements like `needs_custom_kernel_or_os` first and
decisive, then soft signals contributing to `confidence`), analogous in
spirit to the existing rule-based advisory logic in
`internal/whynot/whynot.go`.

## What this does not do

Per the task's own scope: this ADR does not implement any of the
above. No `.proto` message, no generated Go, no FSM command, no
manager-server handler, no reconciler change. It is a proposal for
review.

## Open questions

These could not be resolved from the existing code alone and are
intentionally left for Glen (or whoever accepts/rejects this proposal):

1. **Does `RecommendCellType` need to go through Raft/the leader at
   all**, or is it safe as a read-only call answerable by any node?
   If any input (e.g. node-scoped base-image/template availability)
   makes the answer depend on state that only the leader - or only a
   specific node - has a fresh view of, that constrains the
   implementation away from "answerable by any node."
2. **Is a `CreateCell` RPC the right shape, or should the type
   resolution instead live entirely inside `RecommendCellType`,
   with `CreateCell` *requiring* a `cell_type` the caller got from a
   prior recommendation call** (making the two capabilities sequential
   rather than genuinely independent)? The task states the two
   capabilities are independent and that VM-vs-jail is Apiary's choice
   for capability 1 specifically, which is why this proposal has
   `CreateCell` resolve the type itself rather than requiring a prior
   `RecommendCellType` call - but this means `CreateCell` and
   `RecommendCellType` could end up with two separately-maintained
   copies of the same selection logic unless the former is implemented
   as calling the latter internally. This proposal assumes it should,
   but that is a design choice, not a settled fact.
3. **Should there be a unified `GetCell`/`ListCells` read path**, so a
   caller that got back `cell_type: CELL_TYPE_JAIL` from `CreateCell`
   doesn't have to know to call `GetJail` rather than `GetVM`? This
   ADR's response returns the full `VMDefinition`/`JailDefinition`
   inline specifically to avoid forcing that second call, but a
   longer-lived "cell" read/list/watch surface is a separate, larger
   design question this task did not ask to be resolved.
4. **What is the actual wire format** (REST JSON body shape via
   restshimd, gRPC message encoding, or both) **and does restshimd's
   existing generic gRPC<->REST translation handle enum and repeated-
   message fields the way this proposal assumes**? This was not
   verified against restshimd's translation logic in this pass and the
   task explicitly defers wire-format decisions.
5. **How exhaustive should the `factor` enum in `RecommendCellType`
   be at launch**, and who owns extending it over time without
   breaking callers that switch on existing values? An unrecognized
   future `factor` value reaching an old caller is the standard
   proto3-enum forward-compatibility problem; this proposal does not
   resolve how strictly callers should be told to handle
   `FACTOR_UNSPECIFIED`/unknown values.
6. **Should `resource_profile` in `CreateCellRequest` be validated
   against the same per-kind floors `applyCreateVM`/`applyCreateJail`
   already enforce (e.g. the disk-size floor, ADR-0148) before or
   after the cell-type decision is made**, given that the floor itself
   may differ by kind and could, in principle, feed back into which
   kind is chosen? This proposal treats resource validation as
   happening after type resolution, using the existing per-kind checks
   unchanged, but that ordering is not settled.
