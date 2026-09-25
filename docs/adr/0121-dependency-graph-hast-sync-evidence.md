# ADR-0121: Live HAST synchronization evidence in Comb-failure simulation

## Status

Accepted

## Context

`SHARED.md`'s "Future product directions", Priority 1 (Dependency Graph
Simulator), requires a graph of "HAST pairs **and synchronization**",
and requires that resources be classified by whether they "stop
immediately, can recover automatically in principle, require an
operator, or cannot recover with current topology."

The shipped slices (ADR-0052, ADR-0053, ADR-0054) model HAST
*placement* only. ADR-0052's own correction 2 capped every
replica-backed Cell at `RECOVERY_VERDICT_UNVERIFIED_REPLICA` because,
in that ADR's words, "live HAST sync status has `hastctl list`-quality
information available in zero RPC exposure anywhere in this codebase,"
and it named the remedy as future work: "closing it means new RPC
plumbing this ADR deliberately didn't add."

That plumbing now exists. `GetLocalHASTResourceStatus` reports a single
node's own `hastctl list` view, is deliberately local and read-only,
already validates resource names against caller-controlled `hastctl`
paths, and already has a `PeerReporter` method for reaching a specific
node without leader forwarding. ADR-0121 reuses all of it rather than
inventing a second path to the same facts.

So the stated justification for the weakest verdict has lapsed while
the verdict itself remained - the class of drift this project has
already had to correct twice. Leaving it would also leave the simulator
unable to distinguish the two answers that actually matter to an
operator facing a real Comb failure: "redundancy exists and is caught
up" versus "redundancy is configured and is *not* currently usable."

## Decision

`SimulateNodeFailure` additionally queries each owned, replica-backed
Cell's **configured replica** for its own live HAST status, and reports
a three-way verdict in place of the old single `unverified_replica`:

- `replica_in_sync` - the replica answered, and its own hastd reported
  a real role (`primary`/`secondary`) with status `complete`.
- `replica_out_of_sync` - the replica answered and is confirmed not
  usable as-is: role `init` (never initialized on that node), or a real
  role whose status is not `complete` (e.g. `degraded`).
- `replica_unobserved` - the replica was queried but could not be read:
  RPC error, hastd itself reported status `unknown`, **or hastd gave no
  usable statement at all** (an absent status, or a role this build
  does not recognize). See "Silence is not badness" below.

`unverified_replica` is **retained** and remains distinct: it now means
no query was even *attempted* - this node has no peer forwarding
configured, or the replica is a placement-only node with no raft address
to dial. That is the honest answer, and collapsing it into
`unobserved` would misreport a node that never tried as one that tried
and failed.

The `unprotected` verdict is unchanged and never gains evidence, because
it is a statement about absent configuration, not about observed state.

### Silence is not badness

`replica_out_of_sync` requires a **positive statement** that the replica
is unusable. An absent status, hastd's own `unknown`, and any role value
this build does not recognize are all `replica_unobserved`, never
out-of-sync.

This distinction is reachable, not theoretical: `internal/hast`'s parser
treats a `role:` line as sufficient to parse successfully, so
`hastctl list` output that carries a role but no `status:` line yields a
non-error observation with an empty status. The first implementation of
this ADR bucketed that - and any unrecognized role - into
`replica_out_of_sync`, which would have stated "confirmed NOT usable
as-is" on the strength of evidence that said only "could not check". That
is the same inversion this feature exists to prevent, and it was caught
in review. The same rule that makes a *real* role reporting `degraded`
out-of-sync keeps an *unrecognized* role out of it: one is hastd
reporting trouble, the other is Apiary not understanding hastd.

Interpretation stays in `internal/cluster` as a pure function over
already-fetched observations (`ComputeOwnedResourceImpacts`), preserving
ADR-0052's no-I/O-in-`cluster` separation. Observations are keyed by
**kind and id**, not id alone: HAST resource names are `vm-<id>` and
`jail-<id>`, so a VM and a jail sharing an id are different resources
and must never be shown each other's evidence. All dialing stays in the
RPC handler, bounded by the existing three-second
`reachabilityCheckTimeout` - including the local read, which shells out
to `hastctl list` and would otherwise stall the whole RPC on a wedged
hastd.

The new `ReplicaSyncEvidence` message carries the verbatim observation
(role, status, replication, or the failure detail) alongside every
evidence-backed verdict, so a consumer sees the fact and not only
Apiary's conclusion drawn from it.

## Consequences

- An operator can now see that a Comb's Cells have *no working
  redundancy* right now, rather than having to know that "unverified
  replica" was hiding an unreadable `hastctl status` behind it. This is
  the "require an operator" branch of Priority 1's classification,
  previously unreachable.
- The strongest verdict is still not a guarantee, and the copy says so
  at the point of use. A sync observation is a point-in-time fact about
  the replica's hastd worker. It does not prove the replica stays
  reachable, that the owning disk survives, or that the Cell can be
  recreated from it. No explanation string may say "will recover," and
  a test enforces that.
- Both `toRPCVerdict` and `fromRPCRecoveryVerdict` previously bucketed
  every unrecognized value into a *real* verdict - the manager side
  returned `UNPROTECTED` and the template rendered "unprotected" for
  anything that was not `UNVERIFIED_REPLICA`. That would have silently
  turned any future verdict into "this Cell has no redundancy." Both
  are now explicit switches defaulting to `UNSPECIFIED`/"unknown".
- `GetLocalHASTResourceStatus` is now on a path with a real fan-out:
  one bounded call per owned replica-backed Cell per simulation, from a
  node answering a read-only admin query. This is the same cost profile
  ADR-0052 already accepted for reachability checks, and it is not on
  any hot path.
- The non-atomic-snapshot caveat from ADR-0052 is unchanged and
  slightly worse: these are additional sequential reads, so a replica's
  state can change between the query and the operator acting on it.
- **Not fixed here:** the verdict is per-Cell and per-Comb, with no
  view of replication *lag* magnitude. `hastctl list`'s `dirty` counter
  is deliberately still uninterpreted, exactly as
  `internal/hast.Status`'s own doc comment requires ("callers must not
  reinterpret extent or dirty counters as a numeric RPO").
- **Not fixed here:** `hastctl list` remains name-based, so two Combs
  with divergent same-name images remain indistinguishable (ADR-0054's
  own separate future issue).
- **Not fixed here:** FreeBSD behavior. `status: complete` and
  role `init` are read from real `hastctl` output as ADR-0057 recorded
  it on this project's own FreeBSD 16.0-CURRENT build, but the new
  verdicts are covered by unit tests over parsed values only. They have
  not been exercised against a live desynced pair, so the
  out-of-sync branch is unverified on real hardware.
