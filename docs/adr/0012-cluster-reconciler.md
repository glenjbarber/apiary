# ADR-0012: internal/cluster per-node reconciliation loop

## Status

Accepted

## Context

Once `GetVM`/`ListVMs` existed (ADR-0009) and `internal/zfs` could
actually provision storage, the next question was how a VM's
`VMDefinition` turns into real local resources. The naive answer —
provision as a side effect inside raftd's `FSM.Apply` — is wrong: `Apply`
runs identically on *every* node as they replay the raft log, so a side
effect there would make every node in the cluster try to provision every
VM, not just the one node its `node_id` actually names. `internal/cluster`
exists to do this correctly: a per-node reconciliation loop, the same
shape a Kubernetes kubelet uses for exactly this class of problem.

## Decisions

### Provisioning, not scheduling

`internal/cluster` (as named in the original repo layout doc) was
described as "higher-level cluster/scheduling logic (which node should
own a given VM, rebalancing, etc.)." What's built here is narrower and
more concrete: given VM-to-node assignments *as they already are*, what
does *this* node need to provision locally? Deciding which node a VM's
`node_id` should be — real scheduling — remains unimplemented; a
`VMDefinition`'s `node_id` is still just whatever a caller sets directly.
This package answers "given the assignment, act on it," not "make the
assignment."

### `Plan` is a pure function; `Reconciler` depends on small local interfaces

`Plan(desired []VMPlacement, existingDatasets []string, localNodeID string) []string`
takes no raft or ZFS input directly — it's a plain comparison, fully
unit-tested with table cases and no I/O. `Reconciler.RunOnce` wires this
to real dependencies, but through two local interfaces (`vmLister`,
`datasetManager`) matching the exact subset of methods needed, rather
than importing `*manager.RaftClient`/`*zfs.Manager` into the function
signatures. `*manager.RaftClient` and `*zfs.Manager` satisfy them today
without any changes on their side — this is purely so `RunOnce`'s
orchestration (error handling, translating `VMDefinition` to
`VMPlacement`) can be tested with fakes, independent of the real
end-to-end integration test that also exists.

### Create-only: `Plan` never computes anything to destroy

A VM disappearing from `ListVMs`'s output — because it was deleted,
reassigned to another node, or the fetch simply failed partway — is not
safely distinguishable from "this VM's storage should now be destroyed"
without more care than a first slice should assume. Automatically
tearing down local ZFS datasets on that basis is exactly the kind of
default that turns a transient error into data loss. Real cleanup
semantics (grace periods, confirming a removal is intentional, tying it
to `DeleteVM` specifically rather than "absence from a list") are left
as a deliberate, separate future decision.

### `RunOnce` refuses to provision anything if either fetch fails

If `ListVMs` returns a transport error *or* an application-level
`.Error` (e.g. this raftd isn't the leader), `RunOnce` returns
immediately without calling `Plan` at all — never treating a failed or
partial fetch as "the desired list is empty." Same for a failed local
`ListDatasets`. This is the same caution the create-only design above is
protecting: a reconciler that silently proceeds on bad input is worse
than one that does nothing and reports why.

### Not yet wired into a running daemon

`Reconciler` is a complete, tested package, but nothing calls
`RunOnce` in a loop from any `cmd/` binary yet. That's a deliberate,
separate next step (most naturally as a background goroutine inside
`cmd/managerd`, since each node already runs its own `managerd` dialing
its own `raftd`) — matching how `internal/restshim`'s `Server` (ADR-0011)
is also a tested package without a `cmd/` binary hosting it yet.

## Consequences

- Wiring `Reconciler.RunOnce` into `cmd/managerd` as a periodic
  background loop is the natural next step to make provisioning actually
  happen automatically, rather than requiring a manual trigger.
- Once `internal/bhyve` needs real disk-backed VMs, the reconciler is
  the natural place to also launch the VM (via `internal/bhyve`) once its
  dataset exists — `RunOnce` already knows exactly which VMs are newly
  provisioned.
- Real scheduling (deciding `node_id` placement, rebalancing) still has
  no home; it doesn't obviously belong in this package's create-only,
  per-node-only design, and may need its own component once it's
  designed.
