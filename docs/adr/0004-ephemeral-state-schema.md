# ADR-0004: typed ephemeral-state schema (VMDefinition, Command)

## Status

Accepted

## Context

ADR-0001 deliberately kept `Apply`'s payload as opaque bytes for raftd's
first slice, since the real ephemeral-state schema hadn't been designed
yet and doing so wasn't necessary to prove the raft/protocol stack.
That schema is designed now: `Apply` needs to carry something real before
managerd's future `CreateVM`/`MigrateVM` RPCs have anything meaningful to
submit through it.

## Decisions

### The schema models VM definitions and node ownership, not cluster membership

CLAUDE.md's ephemeral-state examples are "cluster membership, VM
definitions, and node ownership assignments." Of these, cluster
membership is already handled — it's exactly what raft's own
configuration mechanism (`AddVoter`/`RemoveServer`, from ADR-0003)
tracks, replicated by raft itself via `LogConfiguration` entries, not
through `Apply`/the FSM. Modeling it again in the FSM would duplicate
state that already exists and can drift from it. What actually needed
designing here is the other half: `VMDefinition` (id, vcpus, memory,
`node_id` as the ownership assignment, and a `desired_state`), which has
no other home.

`VMDefinition` deliberately excludes anything physical (disk image
bytes, dataset contents) — those stay local per node, replicated by
HAST, never through raft, per the physical-vs-ephemeral split CLAUDE.md
already draws.

### `Command` is a oneof of CRUD operations, not a generic key-value Apply

`CreateVM`/`UpdateVM`/`DeleteVM` are explicit, named operations (a
`Command` oneof) rather than a generic `Set(key, value)`/`Delete(key)`
pair. This matches CLAUDE.md's "RPC-style first, explicit named
operations" philosophy for the external API, and there's no reason for
the internal Apply payload to be more generic than the operations that
will eventually construct it — managerd's future `CreateVM` RPC handler
builds exactly a `CreateVM` command and calls `Apply` with it.

### Application-level rejections are a response field, not a raft-level failure

A duplicate-id `CreateVM` or a missing-id `UpdateVM`/`DeleteVM` is
rejected via `FSMApplyResult.Error` (surfaced as `ApplyResponse.error`),
while the raft-level `Apply` call itself still succeeds — the command was
legitimately committed to the log, it's just that its business-logic
precondition failed. This mirrors how `ErrNotLeader` and the future
`leader_hint` are already reported: as response fields a caller must
check, not as gRPC/raft errors. Treating a bad payload as a crash
(instead of a value in `FSMApplyResult.Error`) would let a single
malformed command take down the state machine — `FSM.Apply` never
returns an error to raft itself for this reason, only to its own result
type.

### Snapshot/Restore use protobuf, not JSON

`fsmState` was JSON in v1 (matching "small, JSON-shaped facts" from
CLAUDE.md, which describes the *content*, not literally the *wire
encoding*). Now that the state has real generated types
(`internalpb.VMDefinition`, `internalpb.FSMSnapshotState`), marshaling
them with `proto.Marshal`/`proto.Unmarshal` is more natural than
round-tripping through JSON tags on top of already-generated structs,
and keeps one encoding (protobuf) for everything that crosses a
process/persistence boundary in this package.

## Consequences

- Every existing test that applied arbitrary byte-string payloads
  (`"persist-me"`, `"over-the-wire"`, `"replicate-me"`) was rewritten to
  build and assert against real `CreateVM`/`DeleteVM` commands — there is
  no longer a supported way to `Apply` an arbitrary opaque payload.
- Adding a new operation (e.g. a future `AssignNode` or `SetDesiredState`
  distinct from a full `UpdateVM`) means adding a new `Command` oneof
  variant and FSM case, not just changing caller-side bytes — this is a
  deliberate cost of the explicit-named-operations approach.
- managerd's future `CreateVM`/`MigrateVM` external RPCs will construct
  `internalpb.Command` messages directly and pass them to
  `RaftClient.Apply` — `internal/manager` already depends on
  `api/internalpb` for this reason (see ADR-0002).
