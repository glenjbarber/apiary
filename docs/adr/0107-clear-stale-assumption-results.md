# ADR-0107: Clear stale Automated Assumption Check results

## Status

Accepted

## Context

The "/assumptions" page (ADR-0055) already groups a node's automated
check results into a current table and a collapsed "superseded/stale
result(s)" section - entries "keyed on something (an interface, a
peer) no longer actively checked, kept for history," per the page's
own existing description. A check's current-snapshot entry
(`internal/assumptions.Result`) is never removed once created: the
checker's `Append` call only ever adds or refreshes entries for keys
present in the current tick, never deletes a key that stops appearing
(e.g. a VM that had a HAST replica check is deleted, or a NAT-uplink
interface is renamed). That entry then sits in the snapshot file
forever, permanently labeled stale once `-assumption-stale-after`
elapses, with no way to remove it - only the separate history journal
is self-pruning (by count and age, see `pruneHistory`).

This was surfaced as a user-requested TODO: "a mechanism to clear the
superseded/stale results list, on the assumptions page" - the page's
own text already named the problem; nothing existed to act on it.

## Decision

`Manager.PurgeStale(staleAfter, now)` (`internal/assumptions/manager.go`)
removes every current-snapshot entry whose `LastObservedAt` is older
than `staleAfter` - deliberately the exact same staleness definition
`toRPCAssumptionResult` already uses to compute the `stale` flag a
caller sees, so this can only ever remove an entry already visibly
labeled stale, never a surprise. It never touches the history journal,
which stays independently self-pruning. `staleAfter <= 0` removes
nothing, matching that same function's "never stale" convention. Safe
and idempotent: a purged key whose checker resumes ticking simply
reappears via the next `Append` call, exactly as a never-before-seen
key would - there is no destructive edge case to guard against.

A new `PurgeStaleAssumptionResults` RPC (`internal/manager`) is
per-node local data, like `ListAssumptionResults` itself - never routed
through Raft, never leader-forwarded, and reads the same
`s.assumptionStaleAfter` threshold `ListAssumptionResults` already
uses. Operator-tier, matching `SaveAssumptionClaim`/
`DeleteAssumptionClaim` (the sibling, operator-authored Assumption
Register's own mutations) - this only clears accounting data, never
live cluster state, so Admin would overstate what it does.

The frontend adds a "Clear stale results" button inside each node's
existing collapsed stale-results section, visible only to a session
that can operate (`CanOperate`), `hx-post`ing to
`POST /assumptions/purge-stale` with that node's `node_id` - the same
per-node locality `nodeAssumptions` already established for reading
this data, forwarding through `s.peers` when the target isn't the
locally-colocated node. The route sits behind
`requireRole(RoleOperator, ...)`, matching the RPC's own tier.

## Consequences

- An operator can now de-clutter a node's stale-results section without
  restarting managerd or hand-editing `/var/db/apiary/assumptions.json`.
- Purging is per-node, not cluster-wide - matching every other piece of
  this page's own data model, which has no cluster-wide concept for
  this physical, per-node observational data.
- A check that is only briefly stale (a transient outage, not a
  genuinely removed subject) can be purged and will simply reappear on
  its next successful observation - this is by design, not a gap to
  close, since the store cannot distinguish "temporarily stale" from
  "permanently orphaned" any more precisely than the existing `stale`
  flag already does.

## Verification

- `internal/assumptions`: `TestManager_PurgeStale_RemovesOnlyEntriesOlderThanThreshold`,
  `TestManager_PurgeStale_NothingStaleIsANoOp`,
  `TestManager_PurgeStale_ZeroStaleAfterRemovesNothing`.
- `internal/manager`: `TestIntegration_PurgeStaleAssumptionResults_NilStoreReturnsError`,
  `TestIntegration_PurgeStaleAssumptionResults_RemovesStaleAndKeepsFresh`
  (real raftd/managerd harness, matching this package's existing
  `ListAssumptionResults` integration tests), `TestRequiredRoleFor_
  PurgeStaleAssumptionResultsIsOperator`.
- `internal/frontend`: `TestServer_AssumptionsPage_ClearStaleButtonRequiresOperator`,
  `TestServer_HandlePurgeStaleAssumptionResults_CallsRPCAndRerendersPage`,
  `TestServer_HandlePurgeStaleAssumptionResults_RPCErrorSurfacedAsBanner`,
  `TestServer_PurgeStaleAssumptionResultsRoute_ViewerBlockedByRouteGate`
  (mirrors the existing `TestServer_ConsoleRoutes_ViewerBlockedByRouteGate`
  pattern for a new state-changing route).
- `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), `buf generate`
  diff reviewed, full `go test ./...` for the whole repository.
