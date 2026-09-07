# ADR-0081: Guided network replacement - per-Comb teardown status

## Status

Accepted

## Context

ADR-0071 designed, but never built, the visibility half of "replace,
never in-place edit": deleting a network is already rejected while any
VM references it, and `internal/cluster.reconcileNetworkArtifacts`
already tears down each Comb's own bridge/VLAN/NAT anchor automatically
once a network leaves the raft-replicated list - but there was no way
for an operator to actually *see* whether that teardown had converged
on every Comb before recreating the same network id. ADR-0071's own
words: "the future UI will expose the correction as a guided
replacement action... Before enabling the final Create action it will
show, per Hive, the old bridge/VLAN/NAT status and whether teardown is
complete or unknown. Unknown is a blocking state, not evidence that
cleanup succeeded." That ADR also named the constraint on how: "a
read-only, per-Hive artifact-status RPC... deliberately separate from
mutation and must not expose arbitrary host interface inventory."

## Decision

### Full v1 scope, deliberately smaller than "guided" implies

This ADR builds the **visibility** ADR-0071 called for - a per-Comb
teardown-status check the operator runs themselves before recreating a
network - not a live-blocking wizard that gates the Create button
automatically. Distinguishing these matters: a wizard that polls every
known Comb on every keystroke of the Create form, disables Submit
until all report clear, and handles "recreate with the same id" versus
"a genuinely new id" as different flows is a substantially larger and
more fragile UI engineering effort than the actual, currently-missing
capability (an operator has *no way at all* today to see this status).
Shipping the visibility now, honestly scoped, beats deferring
everything until a full wizard is built. The operator is trusted to
check status before clicking Create, the same "operator decides, the
system never guesses" posture `ForcePurgeVM`/`ForcePurgeJail` and the
orphaned-HAST-resource RPCs already established for this codebase's
other manually-triggered, evidence-first actions.

### `Reconciler.NetworkArtifactStatus`: the read-only accessor

A new exported method on `*cluster.Reconciler` reads this node's own
`NetworkStatePath` file and reports whether a given network id still
has an entry - present means teardown hasn't converged here yet
(`reconcileNetworkArtifacts` only removes an entry after it
successfully destroys everything it owns; a failed cleanup leaves the
entry in place and retries next tick, so "still present" already means
exactly the right thing for this purpose with zero new bookkeeping).
Returns plain fields (`bridge string`, three `bool`s), not the
package-private `networkArtifact` struct, since this is called from
`internal/manager` across the package boundary via the existing
`reconcilerStats` interface - an unexported type can't appear in
another package's interface method signature.

### `GetNetworkTeardownStatus`: a local-only RPC, mirroring `ListOrphanedHASTResources`

Same posture exactly: physical, per-node, never routed through raft,
Viewer-tier (read-only). `present=true` or a non-empty `error` both
mean "not safe to recreate here yet" - the RPC never tries to
distinguish those two blocking cases for the caller, since both are
equally disqualifying.

### Frontend: query every known Comb, never trust an unreachable one

The Networks page gains a "Check teardown status before recreating"
panel: an operator types the network id they deleted, and the frontend
calls `GetNetworkTeardownStatus` once per `KnownNodeIds` entry -
locally via `s.client` for the Comb this frontend is colocated with,
via `s.peers` (the same `peerHostStatsClient` interface
`GetLocalNetworkBridgeStatus` already uses) for every other Comb. Each
row renders one of three states: **clear** (reachable, confirmed no
artifact), **still present** (reachable, artifact confirmed present),
or **unknown** (unreachable or errored) - unknown is rendered
identically to "still present" in effect (both block recreating), per
ADR-0071's own explicit instruction that unknown must never be
mistaken for evidence cleanup succeeded.

A plain `GET` with a `teardown_network_id` query parameter, not an
htmx fragment post: the check is entirely read-only, so making it a
plain page load keeps the result linkable and durable across a
refresh, unlike the panel's other htmx-driven forms which mutate state
and don't need that property.

## Not addressed

- **The Create form does not automatically block on incomplete
  teardown.** This is the deliberate scope line drawn above - the
  operator must check the status panel themselves first. A future
  enhancement could poll this same RPC and disable Submit live, but
  that's real additional engineering, not a natural extension of
  today's change.
- **No new artifact types were added to the status check.** It reports
  exactly what `reconcileNetworkArtifacts` already tracks (bridge
  ownership, VLAN ownership, outbound NAT) - nothing about DHCP scope
  state, which that same function's sibling `reconcileDHCP` manages
  separately and doesn't persist a comparable per-network record for.
- **No arbitrary host interface inventory is exposed**, matching
  ADR-0071's own explicit constraint - the response is scoped to
  exactly one network id's own recorded artifact, nothing else on the
  host.

## Verification

Unit tests: `internal/cluster/reconciler_test.go` -
`NetworkArtifactStatus` reports a present artifact's fields correctly,
reports absent for an id with no record, and reports absent-with-no-
error when unconfigured (no `NetworkStatePath`).
`internal/manager/server_test.go` - `GetNetworkTeardownStatus` errors
cleanly with no reconciler configured, reports a present artifact's
fields, reports clear when absent, and surfaces a reconciler read
error as the response's own error field.
`internal/frontend/network_teardown_status_test.go` - a two-node
check aggregates local and peer-forwarded results correctly (verified
against the real forwarded address), an unreachable peer renders as
unknown and is never conflated with clear, and no query parameter
means no status table renders at all. `go build ./...`, `go vet
./...`, `gofmt -l`, `git diff --check`, and the complete `go test
./...` suite all pass.
