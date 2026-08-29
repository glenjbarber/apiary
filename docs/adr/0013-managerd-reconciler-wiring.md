# ADR-0013: wiring internal/cluster's Reconciler into cmd/managerd

## Status

Accepted

## Context

`internal/cluster`'s `Reconciler` (ADR-0012) and `internal/restshim`
(ADR-0011) both landed as complete, tested packages without being wired
into a running process. This slice closes that gap for the reconciler:
`cmd/managerd` now runs it on a periodic background loop, making
`CreateVM` → real local ZFS storage happen automatically for the first
time, with no manual trigger required.

## Decisions

### The reconciler's `LocalNodeID` comes from raftd's own `Status`, not managerd's `-node-id` flag

`cmd/managerd` already has a `-node-id` flag, but it's used only for the
identity managerd reports in its own external `Status` responses — a
separate concept from raft's node identity, which is what
`VMDefinition.node_id` actually references. In practice the two default
to the same hostname and would usually coincide, but using managerd's
flag for the reconciler would be a real correctness bug if they were
ever configured differently (e.g. running multiple managerd identities
against the same raftd for some reason). Instead, `run()` queries
raftd's `Status` RPC once at startup and uses `raft_node_id` — the
actual cluster-membership identity — as `Reconciler.LocalNodeID`.

### Reconcile errors are logged, not fatal

A non-leader node's `ListVMs` call failing with "not the leader" is an
expected, routine condition (any node that isn't currently raft's
leader will see this on every tick) — not a reason to crash the whole
daemon. `runReconcileLoop`/`reconcileOnce` log the error and continue to
the next tick, matching the existing fail-fast-only-at-startup
philosophy: `managerd` fails fast if it can't reach raftd *at all*
during startup, but a routine per-tick reconciliation failure during
normal operation is not the same class of problem.

### Reconcile runs once immediately, then on a fixed interval

Waiting a full `-reconcile-interval` before the very first attempt would
mean a freshly started `managerd` looks idle for no reason; running once
immediately (before entering the ticker loop) means a `CreateVM` that
happens to race with `managerd` startup still gets picked up promptly.

### Default `-zfs-base` is `zroot/apiary`, `-reconcile-interval` is 30s

Placeholders consistent with the pattern established for other
defaults in this project (e.g. raftd's `17600`, managerd's `17700`) —
no real deployment/tuning policy exists yet to derive these from.

## Consequences

- Verified for real end-to-end on `freebsd-apiary`: a `CreateVM` call
  through the external API resulted in a real ZFS dataset appearing
  within one reconcile tick, with zero manual steps — the first time the
  full pipeline (external API → raft → reconciler → ZFS) has run live.
- `internal/restshim`'s `Server` (ADR-0011) is still not wired into any
  `cmd/` binary — that remains `cmd/frontend`'s job, still unstarted.
- Once `internal/bhyve` needs real disk-backed VMs, the reconciler
  (already running as a loop) is the natural place to also launch the
  VM once its dataset exists, per ADR-0010's consequences.
