# ADR-0002: managerd external RPC transport and api/rpc schema

## Status

Accepted

## Context

With `raftd` (ADR-0001) in place, this slice adds `managerd`: it dials
raftd's internal socket as a client and exposes its own external RPC API.
As with raftd's v1 slice, the goal is to prove the architecture — the
full external-client -> managerd -> raftd chain — before any real
VM/container backend exists to give operations like `CreateVM` actual
meaning.

## Decisions

### managerd's external API is gRPC over TCP, not another Unix socket

raftd's internal protocol is UDS because its only client is managerd on
the same host. managerd's external API is different: it's the surface
CLAUDE.md's "RPC-style first, REST translation layer on top" framing
describes, and the REST shim and eventual web frontend need to reach it
— potentially from a different process, and eventually across a real
network in a clustered deployment. A TCP listener (loopback by default,
`-rpc-addr` configurable) is the right transport for that role from the
start, even though today's only client is a local test/smoke client.

### `api/rpc` schema: `apiary.rpc.v1.ManagerService`, no directory-visibility hazard

New proto package `apiary.rpc.v1`, Go package `rpcpb`
(`api/rpc/manager.proto`, generated stubs co-located per the same
convention as `api/internalpb`). Named `ManagerService` rather than
something raftd-specific, since future operations (`CreateVM`,
`MigrateVM`) belong on this same external-facing service, not a new one
per operation area.

Unlike `api/internalpb` (renamed from `api/internal` in ADR-0001 to avoid
Go's internal-package visibility rule), `api/rpc` has no directory
literally named `internal` in its path, so no equivalent import
restriction applies — this closes the open item ADR-0001 flagged under
"Consequences" for future proto packages.

### v1's external API is diagnostic-only (`Status`)

Like raftd's own Apply/Status-only v1 slice, managerd's first RPC is
`Status`, not a real VM operation. There is no `bhyve`/`jail`/`zfs`
backend yet, so `CreateVM`/`MigrateVM` would have nothing to actually do.
`Status` instead proves the full chain works: an external client calls
managerd, managerd calls raftd over the internal socket, and the real
raft state (leader, applied index, etc.) comes back out the other end.

### `StatusResponse` flattens raftd's fields rather than nesting its type

managerd's `StatusResponse` duplicates the relevant raftd status fields
(`raft_is_leader`, `raft_node_id`, etc.) as its own top-level fields,
plus `raft_reachable`/`raft_error` for the unreachable case, rather than
embedding `api/internalpb.StatusResponse` verbatim. This keeps the
external-facing schema decoupled from the internal protocol's evolution
— exactly the boundary having two separate proto packages is meant to
preserve — at the cost of a small amount of field-mapping code in
`internal/manager/server.go`.

### Fail-fast, not retry-with-backoff, when raftd is unreachable at startup

`cmd/managerd/main.go` calls `manager.Dial` once at startup and exits
immediately (`log.Fatalf`) if it fails. There's no process-supervision or
retry infrastructure in the project yet, so a managerd that can't reach
raftd at all isn't in a useful state regardless of whether it keeps
retrying in-process. Revisit this once a real process supervisor (rc.d
script or similar) exists that can restart-with-backoff externally —
at that point in-process retry may be redundant anyway.

Note this is different from *runtime* unreachability (raftd disappearing
after managerd has already started successfully): that case is already
handled per-request by `Server.Status` returning `raft_reachable=false`
rather than failing the whole process.

### Default `-rpc-addr` is `127.0.0.1:17700`

An arbitrary but memorable placeholder, distinct from raftd's raft-bind
default (`17600`). No port-allocation policy exists yet for the project;
revisit if one is established.

## Consequences

- Any future external-facing operation (`CreateVM`, `MigrateVM`, etc.)
  is added to `ManagerService` in `api/rpc/manager.proto`, following the
  same buf-generate-and-commit workflow already established.
- `internal/manager/server.go`'s field-mapping between raftd's and
  managerd's status types will need a corresponding update any time
  either schema's status fields change — there is no automatic sync
  between them by design.
- Fail-fast startup means managerd currently cannot be started before
  raftd (or before raftd's socket exists); operational tooling/scripts
  that launch both need to sequence raftd first.
