# ADR-0003: raftd multi-node clustering (join/remove)

## Status

Accepted

## Context

ADR-0001 deliberately scoped raftd's first slice to single-node bootstrap
only, but chose a real TCP transport specifically so multi-node
clustering could be added without a transport rewrite. This slice adds
that: `AddVoter`/`RemoveServer` on the internal protocol, and a `-join`
flag so a real `raftd` process can join an existing cluster on its own
rather than requiring an external orchestrator to drive membership
changes by hand.

## Decisions

### Membership changes are exposed as new RaftInternal RPCs, not a separate service

`AddVoter` and `RemoveServer` were added to the existing `RaftInternal`
service in `api/internalpb/raftd.proto`, alongside `Apply`/`Status`,
rather than introducing a separate membership-management service. They
share the same not-leader/leader-hint error shape as `Apply` (now
factored into `translateMembershipErr` in `internal/raft/node.go`), and
callers already need a `RaftInternal` client for `Apply`/`Status`, so
there's no benefit to splitting membership operations out.

### `StatusResponse` gained a `servers` field

Membership changes are otherwise invisible to a caller — `Status`
already reports single-node state, and now also reports the full
cluster configuration (`id`, `address`, `suffrage` per member) as raft
itself currently sees it. This is what both the multi-node test and any
future operational tooling use to confirm a join or removal actually
took effect, rather than only trusting the RPC's own success/failure.

### A new node self-joins via `-join`, rather than requiring an external `AddVoter` call

The alternative was to leave `AddVoter`/`RemoveServer` as raw
capabilities and require an operator or higher-level tool (managerd, a
deploy script) to call `AddVoter` against the leader after starting a
new node manually. Instead, `cmd/raftd` accepts `-join
<existing-member-socket>`: on first start (no existing on-disk state), a
node with `-join` set skips `Bootstrap` and instead dials the given
peer's internal socket itself and calls `AddVoter` for its own
`node-id`/`raft-bind` address. This keeps "how do I add a node to this
cluster" a one-flag operation on the joining node itself, matching how
`-data-dir`/`-socket`/`-raft-bind` are already self-contained per-node
configuration, rather than splitting cluster-formation knowledge between
the new node's flags and a separate script that calls the internal RPC.

`hadState` still takes priority over `-join`: a restarting node that
already has on-disk raft state resumes as part of whatever configuration
it last knew, and does not re-bootstrap or re-join even if `-join` is
still passed on the command line (e.g. left in a supervisor's start
command across restarts).

### No automatic peer discovery

`-join` requires the operator to name a specific existing member's
socket path; there's no gossip, DNS-based discovery, or seed-list
mechanism. This matches the project's current stage — single-host
development and testing — and can be layered on top later (e.g. via
managerd, once it exists as a real orchestrator) without changing the
`AddVoter`/`RemoveServer` primitives themselves.

## Consequences

- Removing a node from the cluster (`RemoveServer`) is available as a
  protocol operation, but `cmd/raftd` has no equivalent `-leave` flag or
  CLI verb yet — it must be called directly (e.g. once managerd or an
  admin CLI exists to drive it). This is fine for now since the join
  path was the one blocking realistic multi-node testing.
- A node started with `-join` pointed at a peer that isn't actually the
  leader will fail fast with the returned `leader_hint`, rather than
  retrying against the hinted leader automatically. Retrying against the
  hint is a reasonable follow-up once there's a real deployment story
  that needs it.
- `internal/raft`'s multi-node test (`multinode_test.go`) is the primary
  regression coverage for this behavior; it exercises join, replication,
  and removal against real in-process raft instances, and was
  cross-checked against a real 3-process `raftd -join` run.
