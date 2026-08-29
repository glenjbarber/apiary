# ADR-0011: internal/restshim REST-over-gRPC translation

## Status

Accepted

## Context

CLAUDE.md's architecture calls for "RPC-style first ... a REST
translation layer sits on top afterward for broader client
compatibility." Now that `ManagerService` has real operations
(`CreateVM`/`UpdateVM`/`DeleteVM`/`GetVM`/`ListVMs`/`Status`), this slice
builds that layer: `internal/restshim` translates JSON-over-HTTP requests
into calls against `ManagerService`, the same way `managerd` itself
translates its external API into calls against raftd's internal
protocol.

## Decisions

### `restshim` is a `ManagerService` client, not a raftd client

`Server` holds an `rpcpb.ManagerServiceClient` — the exact interface any
gRPC client of managerd already uses — and never talks to raftd
directly. This keeps the layering strict: external HTTP clients →
restshim → managerd (gRPC) → raftd (internal protocol), with no layer
skipping the one below it.

### Routes use Go 1.22+'s enhanced `http.ServeMux`, not a third-party router

`"GET /v1/vms/{id}"`-style patterns (method + path wildcards) have been
in the standard library since Go 1.22; the module already targets Go
1.27. No dependency needed for basic REST routing.

### The REST JSON schema is its own type, not `api/rpc`'s generated struct

Continuing the same reasoning ADR-0002 and ADR-0005 already
established between `api/internalpb` and `api/rpc`: exposing protobuf's
generated Go struct (and its default JSON field behavior) directly as
the REST wire format would couple the REST API's shape to protobuf
codegen details it has no reason to depend on. `convert.go`'s small
`vm` type and `toRPCVM`/`fromRPCVM` functions are the (intentionally
thin) translation layer this costs.

### Error status codes are a deliberate v1 simplification

Every RPC in `ManagerService` reports failure as a plain string
(`error`, `leader_hint`) rather than a typed code. `restshim` maps this
into HTTP status three ways:

- a transport-level failure reaching managerd → `502 Bad Gateway`
- `leader_hint` present (that managerd's raftd isn't the leader) →
  `503 Service Unavailable`
- any other application-level rejection → `400 Bad Request`

This cannot currently distinguish, say, "VM already exists" (arguably
`409 Conflict`) from "VM not found" on update/delete (arguably
`404 Not Found`) — the underlying RPC layer doesn't hand back anything
but a message string, and pattern-matching on error text would be
fragile and break silently if a message's wording ever changed. `GetVM`
is the one exception: its `found` field is a real boolean, so a miss
there does correctly return `404`. Real per-error-type codes elsewhere
would need `ManagerService`'s responses to carry a typed error code —
a real API change, not something to fake from the REST side.

## Consequences

- Any future error-code improvement needs to start at `api/rpc/manager.proto`
  (a typed error enum or `google.rpc.Status`-style detail), not in
  `restshim` alone.
- `cmd/frontend` (not yet built) is the natural place to wire
  `restshim.Server` into a running process — dial managerd the same way
  `restshim`'s own tests fake it, then `http.ListenAndServe` with the
  server as the handler. This slice deliberately stops at the tested,
  reusable package, matching how `internal/cluster`'s `Reconciler` isn't
  wired into `cmd/managerd` yet either.
- `web/`'s HTMX frontend, whenever it's built, can sit in front of this
  same REST API rather than needing its own gRPC client.
