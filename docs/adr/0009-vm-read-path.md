# ADR-0009: GetVM/ListVMs read path

## Status

Accepted

## Context

ADR-0005 shipped `CreateVM`/`UpdateVM`/`DeleteVM` but explicitly deferred
a read path, since none of the existing internal RPCs
(`Apply`/`Status`/`AddVoter`/`RemoveServer`) could read FSM state without
mutating it, and read consistency needed its own decision. This slice
adds that: `GetVM`/`ListVMs` on both the internal `RaftInternal` protocol
and the external `ManagerService`.

## Decisions

### Reads only succeed against the leader — same model as writes

`Node.GetVM`/`Node.ListVMs` check `raft.State() == raft.Leader` before
touching the FSM at all, returning `ErrNotLeader` (with `leader_hint`,
same shape as every other leader-required operation) otherwise. The
alternative — serving reads from any node's local FSM state, including
followers — would be simpler to call (no leader-routing needed) but
introduces a real consistency question this project hasn't needed to
answer yet: a follower's FSM can lag behind the leader's by an
unbounded, un-signaled amount, so a client reading from a follower could
see an arbitrarily stale `VMDefinition` with no way to know it's stale.
Restricting reads to the leader keeps v1's consistency model to a single
sentence — "the leader is the source of truth for both reads and
writes" — rather than introducing lease-based reads, `ReadIndex`, or
some other linearizable-follower-read scheme prematurely.

The real cost of this choice: every read requires knowing (or
discovering, via `leader_hint`) which node is currently the leader, same
as every write already does. No new operational burden, just the same
one extended to reads.

### New RPCs, not reusing `Status`'s `servers` field or `Apply`

`GetVM`/`ListVMs` are new, dedicated internal RPCs rather than piggy-backing
VM data onto `StatusResponse` (which already carries the raft
configuration's `servers` list — a different kind of data, cluster
membership, not VM definitions) or requiring callers to encode a "read"
as a fake `Apply` (which would incorrectly go through the raft log for
an operation that doesn't mutate anything).

### External `GetVM`/`ListVMs` mirror the internal ones exactly

Same translation pattern as `CreateVM`/`UpdateVM`/`DeleteVM`: managerd's
`Server.GetVM`/`ListVMs` call through `RaftClient.GetVM`/`ListVMs`,
translate `internalpb.VMDefinition` to `rpcpb.VMDefinition` via the
existing `convert.go` helpers, and surface both connection-level and
leader-related failures as response fields (`error`/`leader_hint`), not
gRPC errors — continuing the pattern every other RPC on both services
already follows.

## Consequences

- A client wanting to read VM state must be prepared to retry against a
  `leader_hint` on a non-leader response, same as for writes. There is
  no "read from anywhere, get eventually-consistent data" mode in v1.
- If a future requirement genuinely needs follower reads (e.g. for load
  distribution once clusters are large), it will need a real design
  decision about staleness bounds/signaling — this ADR's approach
  punts that decision, it doesn't preclude revisiting it.
