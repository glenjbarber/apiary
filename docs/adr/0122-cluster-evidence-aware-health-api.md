# ADR-0122: Cluster-wide Evidence-Aware Health over the API

## Status

Accepted

## Context

`SHARED.md`'s "Future product directions", Priority 3, requires that
Evidence-Aware Health be "a shared UI **and API** semantic."

It is currently UI-only, and that is a deliberate, documented state
rather than an oversight. ADR-0056's Consequences section records it
verbatim:

> Non-HTML consumers of `Status`/`HostStats` (restshimd, any future
> CLI) get the new raw evidence fields (`members`, the three reconcile
> fields) in v1, but not a computed verdict - `ComputeNodeHealth` is
> only ever invoked from the Go-side frontend page handler. [...] a
> future consumer wanting a computed `NodeHealth` would need
> `internal/health` either exposed through a new RPC or reimplemented
> against the raw fields already on the wire.

That consumer now exists. `GetLocalNodeHealth` reports one node's
verdict, but a caller wanting a Colony view must still perform the
whole thing itself: read raft membership once, dial every node,
independently decide reachability, preserve "unknown" when a node
cannot be reached, and reimplement `internal/health`'s eight-step
decision chain against the raw wire fields.

That is precisely the reimplementation risk the feature exists to
prevent. A REST or CLI consumer deriving "healthy" by some simpler
rule reintroduces the exact failure - membership arithmetic masquerading
as availability proof - on the surface most likely to be automated
against.

There was also a latent drift risk already present: the
gathered-observation-to-`NodeSignals` derivation existed in two places
(the frontend's `nodeHealthSignals` and managerd's own
`GetLocalNodeHealth`) with no shared implementation. Adding a third
consumer would have made three.

## Decision

**1. One derivation, in `internal/health`.** `health.Inputs` and
`health.SignalsFrom` now own every rule that turns gathered
observations into `NodeSignals`. All three callers (the frontend
cluster-overview path, `GetLocalNodeHealth`, and the new
`ClusterHealth`) build an `Inputs` from their own I/O and call the same
function. I/O stays with each caller, deliberately: the frontend is a
separate process and must gather its own evidence, exactly as
ADR-0056 designed.

`SignalsFrom` decides only what was and was not observed, never a
verdict - `ComputeNodeHealth` remains the sole decider. Its most
important rule is that a remote node whose reachability was never
established reads as `Unknown`, never inheriting the answering node's
trivially-true local reachability.

**2. A new `ClusterHealth` RPC**, not a change to `Status` or
`HostStats`. Those answer for one node and sit on paths that must not
acquire a cluster-wide fan-out cost; ADR-0056's scoping decision is
preserved intact. `ClusterHealth` fans out concurrently, bounded by the
existing `reachabilityCheckTimeout`, and returns one verdict per known
Comb sorted by node ID.

Per node it reads raft membership **once** for the whole request
(raft membership is a cluster-wide-consistent replicated fact, so a
per-node self-report would add no defensive value), then one
`HostStats` and one independent `Status` per Comb. The answering node's
own self-report and log position are reused from the anchor call rather
than self-dialed.

A node whose evidence could not be gathered still appears in the
result, with a non-healthy status. An absent entry must never be
readable as a healthy one.

**3. `status` stays a string**, matching the existing
`GetLocalNodeHealthResponse` contract. A second, parallel enum
representation of the same five states on the same wire was considered
and rejected: it would be one more thing to keep in step, for a
guarantee a consumer can obtain more simply by mapping an unrecognized
string to "not healthy." That obligation is documented on
`ClusterNodeHealth.status` rather than enforced by the type system,
and is called out below as a known sharp edge.

`PeerForwarder` gained a `Status(ctx, addr)` method. `*PeerReporter`
already implemented it for the frontend's use; the interface simply did
not name it.

## Consequences

- `restshimd`, a future CLI, or any other non-HTML consumer can now read
  the same five-state verdict with its evidence, without
  reimplementing the decision chain, and without the ability to
  accidentally disagree with the UI about it.
- Three previously independent derivations collapse to one. A future
  change to the rules now has one place to change, and the existing
  frontend health tests plus the new `SignalsFrom` tests cover both
  consumers of it.
- The cluster overview page's behavior is unchanged. It keeps gathering
  its own evidence in the frontend process and calling the same
  `ComputeNodeHealth`, so this adds no extra per-node round trip to the
  page. The two paths are now equal by construction rather than by
  convention - but they remain two independent *gatherings*, so the
  non-atomic-snapshot caveat applies to both, and a verdict from the
  page and one from `ClusterHealth` taken seconds apart may legitimately
  differ.
- `ClusterHealth` itself is a multi-source sequential read, not an
  atomic snapshot. It is documented as such on the RPC.
- **Each peer probe gets its own bounded context.** `HostStats` and
  `Status` are two independent probes, so they are two independent
  `reachabilityCheckTimeout` budgets. Sharing one budget across the two
  sequential calls meant a slow-but-alive peer that spent all of it on
  `HostStats` handed `Status` an already-expired context, so its
  heartbeat evidence was lost to a timing accident rather than to
  anything true about that peer - and the resulting verdict could differ
  from a peer that failed fast. Distinguishing "could not check" from
  "checked and not fine" is this feature's entire purpose; wall-clock
  timing must not be allowed to decide which of the two a consumer sees.
- **A failed probe states its reason.** A non-healthy verdict with no
  stated cause is not actionable, so when a probe could not be
  completed its reason is attached as a raw `peer_probe` observation
  alongside the verdict, and returned separately for the caller's own
  logging. This covers the cases where no dial was even attempted
  (no peer forwarding configured, or a member with no address), not
  just RPC errors.
- **`ClusterNodeHealth.dialed` is true for the answering node.** The
  field documents whether reachability was "established trivially
  because it answered the request itself", so the node answering the
  request reports `dialed: true` even though nothing was dialed. A
  consumer must not read the field as "a socket was opened".
- **Known sharp edge, not fixed here:** `status` is a string, so a
  consumer reading a `ClusterHealthResponse` from a newer managerd can
  see a sixth state it does not recognize. The correct handling is to
  treat it as not-healthy, and that obligation is documented rather than
  enforced. An enum would enforce it at the type level; it was not
  adopted here to avoid a parallel representation of the same states.
- **Not fixed here:** ADR-0056's disclosed pre-existing gap stands -
  `internal/raft.Node.Status()` still leaves `Servers` nil with no error
  surfaced if its own `GetConfiguration()` call fails, so
  `membership_observed` here still inherits that gap.
- **Not fixed here:** raft's `CommitIndex()` still has no exposure in
  this codebase, so applied/last-log index remains a raw observation
  that no verdict is derived from, exactly as ADR-0056 requires.
- **Test coverage is uneven, deliberately.** `SignalsFrom` and the
  verdict conversion are unit-tested directly, and the
  never-healthy-without-observation rules are covered against
  hand-built inputs. The live multi-node fan-out path is covered only by
  the existing single-node raft harness, not by a real two-node
  cluster; it has not been exercised against running Combs. The
  per-probe context budget IS covered, by a fake peer that is slow
  rather than absent: it fails if `Status` is handed a context that
  `HostStats` already exhausted, which is the exact failure the shared
  budget produced.
