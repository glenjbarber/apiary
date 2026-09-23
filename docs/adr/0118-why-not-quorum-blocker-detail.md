# ADR-0118: Surface voter-by-voter detail on the Why Not quorum blocker

## Status

Accepted, implemented.

## Context

The Why Not Engine's "why is this comb unsafe to reboot" answer
(`internal/whynot.AnswerHiveReboot`, ADR-0061) already reuses the
Dependency Graph Simulator's quorum-impact computation
(`internal/cluster.ComputeQuorumImpact`, ADR-0052) to decide whether
rebooting a given Comb would break Raft quorum. When it does, the
answer's only quorum-tolerance Blocker carried a single prose `Note`
string (e.g. "quorum is LOST - the confirmed-reachable remaining
voters do not form a majority.") with no supporting detail: not the
total voter count, not the majority threshold, and not which specific
remaining voter(s) were reachable, unreachable, or unknown.

The user's request (2026-09-23): make the page explain its quorum
blocker with the voter-by-voter reachability results and quorum counts
(total voters, majority required, confirmed-reachable remaining
voters), so an operator can tell a genuine quorum-loss risk apart from
a reachability/TLS problem answering the question at all. The
underlying unreachable-vs-unknown distinction must be preserved, and
nothing should imply a result is verified beyond what was actually
checked live.

Investigation before implementing found the per-voter data already
exists, momentarily, inside the existing computation - this is a
plumbing gap, not a missing capability:

- `internal/manager/server.go`'s `SimulateNodeFailure` already dials
  every other raft voter via `s.peers.HostStats` and builds a real
  three-state `cluster.Reachability` (`Reachable`/`Unreachable`/
  `Unknown`) per voter (`internal/cluster/simulate.go`'s own doc
  comment: "a real three-state result, not a bool... Unknown must
  never be silently folded into either reachable or unreachable").
- `cluster.ComputeQuorumImpact` consumed that per-voter slice but
  returned only aggregate counts (`TotalVoters`, `RemainingVoters`,
  `RemainingReachable`, `RemainingUnknown`, `QuorumSize`, `Survives`,
  `Note`) - the per-voter identity/reachability pairing was discarded
  before it ever left the function.
- The RPC response (`rpcpb.QuorumImpact`) already carried the
  aggregate counts, but `internal/frontend/why_not.go`'s handler
  dropped `RemainingReachableVoters`/`RemainingUnknownVoters` on the
  floor when building `whynot.QuorumFact` - they were fetched from the
  RPC response and simply never copied over.
- `whynot.QuorumFact` itself never had a field to carry per-voter
  detail at all.

## Decision

Thread the reachability data that already exists all the way through,
rather than inventing a new reachability-check mechanism:

1. `cluster.QuorumImpact` gains a `Voters []VoterReachability` field
   (`internal/cluster/simulate.go`) - every OTHER raft voter (never the
   simulated target, never a non-voter), sorted by ID for deterministic
   output. `ComputeQuorumImpact` now populates it from the same loop
   that already computes the aggregate counts.
2. `rpcpb.QuorumImpact` gains a matching `repeated VoterReachability
   voters` field (`api/rpc/manager.proto`), with reachability
   represented as a plain string (`"reachable"`/`"unreachable"`/
   `"unknown"`) rather than a new proto enum, matching this message's
   existing plain-string `Suffrage` convention.
3. `internal/manager/server.go`'s `toRPCQuorumImpact` maps the new
   field straight across.
4. `whynot.QuorumFact` gains `QuorumSize`, `RemainingReachable`,
   `RemainingUnknown`, and `Voters []VoterFact` -
   `internal/frontend/why_not.go`'s handler now copies all of it from
   the RPC response instead of dropping it.
5. `whynot.AnswerHiveReboot` builds the quorum-tolerance Blocker's
   `Evidence` (a field this package's `Blocker` type already has,
   reused directly rather than adding a new one) via a new
   `quorumEvidence` helper: one summary entry with the raw counts
   ("3 total voter(s), majority requires 2, 0 confirmed reachable, 1
   unverified (unknown)."), then one entry per other voter naming its
   own reachability ("node-b: unreachable"). Each entry is stamped
   `ObservedAt: time.Now()` at evaluation time - this is fresh evidence
   gathered during the same `SimulateNodeFailure` call, not a permanent
   disclosed v1 limitation like this package's existing zero-`ObservedAt`
   Caveats convention, so it must never be confused with one.
6. No frontend view-type or template change was needed:
   `whyNotBlockerView`/`fromWhyNotAnswer` already copy every
   `Blocker.Evidence` entry generically, and `web/templates/why_not.html`
   already renders a Blocker's Evidence list (Source, Detail, and
   either "observed \<timestamp\>" or "(not runtime-observed)" per the
   existing `NeverObserved` convention) in a collapsible `<details>`
   section. The new evidence flows through that existing mechanism
   unchanged.

Scope decision: the new evidence is attached only to the
quorum-tolerance Blocker itself, i.e. only when quorum does NOT
survive - matching the literal ask ("explain its quorum blocker").
A quorum-survives-with-unknown-voters case already produces a `Note`
mentioning the unverified voter count, but does not get a structured
per-voter Evidence list in this change, since `Answer.Caveats` is
already a well-defined field reserved for permanent, zero-`ObservedAt`
v1 limitations (live HAST sync status, DNS observability) - reusing it
for fresh per-call evidence would blur that existing, documented
distinction. Extending the clear-with-unknown case is a reasonable
follow-up if wanted, but is not forced into an existing field with a
different meaning just to cover it here.

## Consequences

- An operator looking at a blocked "why can't I reboot this Comb"
  answer can now expand Evidence and see exactly which other voter(s)
  are reachable, unreachable, or unknown, plus the raw quorum
  arithmetic - not just a one-line prose conclusion.
- The unreachable-vs-unknown distinction is preserved end to end: a
  voter that could not be checked at all is never rendered or counted
  as either reachable or unreachable, matching
  `internal/cluster.Reachability`'s own documented invariant.
- No new dial/reachability-check code was added anywhere - this is
  entirely a plumbing change surfacing data that was already being
  computed and then discarded.
- `AnswerCellMigrate`, `AnswerCellRecoverable`, and
  `AnswerNetworkConnectivity` are unaffected; only `AnswerHiveReboot`'s
  quorum-tolerance Blocker gained new Evidence.

## Verification

- New unit tests: `TestComputeQuorumImpact_VotersListsEveryOtherVoterSortedByID`
  (`internal/cluster/simulate_test.go`) confirms the target itself and
  non-voters are excluded and the remaining voters are sorted by ID;
  `TestAnswerHiveReboot_QuorumBlockerCarriesVoterByVoterEvidence` and
  `TestAnswerHiveReboot_ClearQuorumHasNoQuorumBlockerOrEvidence`
  (`internal/whynot/whynot_test.go`) confirm the summary-plus-per-voter
  Evidence shape, that every entry is freshly `ObservedAt`-stamped, and
  that a surviving quorum produces no quorum-tolerance Blocker at all.
- `go build ./...`, `go vet ./...`, `gofmt -l .` all clean; full
  `go test ./...` for the entire repository passes.
- `buf generate` produces no further diff after regenerating
  `api/rpc/manager.pb.go` for the new `voters`/`VoterReachability`
  proto fields.
- Not verified: no live Colony exercise of a real blocked quorum
  answer was performed as part of this change - the existing
  integration test for `SimulateNodeFailure`
  (`internal/manager/integration_test.go`) uses a single-voter
  cluster, which has no OTHER voters to list; a multi-voter live or
  integration exercise of the new per-voter rendering is a reasonable
  follow-up but was not built here, since `toRPCQuorumImpact`'s mapping
  is a trivial one-to-one field copy already covered by the unit tests
  above.
