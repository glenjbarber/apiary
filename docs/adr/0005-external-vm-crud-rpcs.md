# ADR-0005: real CreateVM/UpdateVM/DeleteVM on ManagerService

## Status

Accepted

## Context

ADR-0002 scoped managerd's external API to a diagnostic-only `Status`
RPC, since `api/internalpb`'s ephemeral-state schema didn't exist yet.
ADR-0004 added that schema (`VMDefinition`, `Command`). This slice
connects the two: real `CreateVM`/`UpdateVM`/`DeleteVM` RPCs on
`ManagerService` that build and submit `Command`s through the existing
`RaftClient.Apply` path. Nothing runs a VM yet — there's still no
bhyve/jail/zfs backend — but the definitions themselves are now real,
replicated, and persisted, which is everything that doesn't require a
FreeBSD host to build and verify.

## Decisions

### The external API defines its own `VMDefinition`/`VMState`, not reusing `api/internalpb`'s

Same reasoning as ADR-0002's `StatusResponse`: coupling the external
schema directly to the internal protocol's types would mean any future
change to the internal ephemeral-state representation is also an
external API change, and vice versa. `internal/manager/convert.go` holds
the small mapping between the two - the cost of the decoupling, paid
once, in one place.

### Raft-level and application-level failures both surface as response fields

`CreateVM`/`UpdateVM`/`DeleteVM` responses each carry `error` and
`leader_hint` fields, populated whether the failure was raftd being
unreachable, this raftd not being the leader, or an application-level
rejection (duplicate id on create, missing id on update/delete) from the
FSM itself. All three are indistinguishable from an external caller's
perspective - "the operation didn't happen" - so they're handled
uniformly via `Server.applyCommand`, rather than giving raft-level
failures a gRPC error and application-level ones a response field (or
vice versa). This continues the pattern `Status` already established for
raftd-unreachable.

### No `GetVM`/`ListVMs` yet

Only mutations were added. A read path would need a new *query* RPC on
the internal protocol (today's `Apply`/`Status`/`AddVoter`/`RemoveServer`
have no way to fetch current FSM state without mutating it), plus a
decision about read consistency (served from any node's local state vs.
routed to the leader). That's a real design question deserving its own
slice rather than a rushed addition here - `CreateVM`/`UpdateVM`/`DeleteVM`
already responding with the resulting `VMDefinition` covers verification
needs for now (including in this slice's own tests).

## Consequences

- Adding a `GetVM`/`ListVMs` read path later will need a new internal
  RPC (not just an external one), and a decision about read consistency
  guarantees - flagged here so it isn't forgotten.
- Any future VM field (disk/network config, etc.) needs to be added to
  *three* places: `api/internalpb/state.proto`'s `VMDefinition`,
  `api/rpc/manager.proto`'s `VMDefinition`, and the conversion functions
  in `internal/manager/convert.go` - a direct cost of the decoupling
  decision above.
