# ADR-0103: Action preflight - join-approval preview + concurrent-manager-restart guardrail

## Status

Accepted

## Context

The differentiated-product roadmap (`SHARED.md`) names "live-state change
simulator plus intent guardrails" as a strategic bet, with two concrete
examples: "do not approve a join until the claimed Raft address is
reachable" and "do not restart both managers within ten minutes." Both are
grounded in a real incident (2026-09-11): approving a join before the
joining Comb's raftd was actually reachable stranded a two-voter cluster,
recoverable only by wiping raft state.

Two findings shaped this ADR before any code was written:

- The join-reachability example was **already half-built**. ADR-0097 added
  a reachability check (`internal/manager/joincolony.go`, `s.reachabilityCheck`
  dialing `pending.raft_bind_address` before `s.raft.AddVoter`) directly
  inside `ApproveJoinRequest`. It enforces correctly, but an operator had
  no way to *preview* the result before clicking Approve.
- The restart-cooldown example had **zero existing infrastructure**.
  `RestartNodeService` had no rate limiting, no last-restart tracking, and
  no cross-node awareness. Nothing stopped an operator from restarting
  `apiary_managerd` on both Raft voters within the same short window - the
  same class of self-inflicted quorum risk as the 2026-09-11 incident.

The rest of this codebase's "explainability" architecture (`internal/cluster.Simulate*`
/ ADR-0052, `internal/whynot` / ADR-0061, `internal/invariant` / ADR-0060,
`internal/recovery` / ADR-0057, `internal/coverage` / ADR-0062) is entirely
retrospective: every one of them answers "what if this already-existing
thing failed," never "what would happen if I did X right now." This ADR
introduces that concept for the first time, deliberately scoped to the two
guardrails above - not a generic per-RPC preflight system - mirroring
ADR-0081's own "ship the visibility now, honestly scoped" precedent.

## Decision

### `internal/guardrail` (new package)

A pure, no-I/O package mirroring `internal/invariant`/`internal/whynot`'s
convention: `Verdict` (`allow`/`block`/`unknown`), `Finding` (a stable rule
id, detail, and `[]invariant.Evidence`), `Report`. Two functions:

- `EvaluateJoinReachability` - the single source of truth for the
  reachability gate. `ApproveJoinRequest`'s real enforcement and
  `PreflightApproveJoinRequest`'s preview both call this same function, so
  the two can never silently drift apart.
- `EvaluateConcurrentManagerRestart` - advisory/preview-only, backing
  `PreflightRestartNodeService`. The real, authoritative enforcement lives
  in the Raft FSM (below), not here.

### Join approval: reordered to check leadership first

`ApproveJoinRequest` ran its reachability dial *before* discovering
whether the receiving node was even the Raft leader - a follower with its
own, different network path to the joiner's address could reject (or
wrongly approve) based on the wrong node's vantage point, since only the
leader's own reachability actually matters (only the leader calls
`AddVoter`). Fixed: both `ApproveJoinRequest` and the new
`PreflightApproveJoinRequest` now check `s.raft.Status().IsLeader` first
(cheap, local, no RPC - `internal/raft.Node.Status()`) and forward the
*entire* call to the leader (found via `Status().Servers` matched against
`LeaderId`) before ever dialing. The existing post-`AddVoter`-failure
forward remains as a fallback for the race window where leadership changes
between this check and the `AddVoter` call itself.

`PreflightApproveJoinRequest` is Admin-tier, not Viewer - it makes
managerd dial a caller-selected pending request's address, which a Viewer
could otherwise use as a network reachability oracle.

### The concurrent-manager-restart guardrail: a Raft-committed lease, not local bookkeeping

The design went through five review cycles before landing here; each
earlier attempt is worth recording because the failure mode it fixed is
the reason the final shape looks the way it does:

1. **A local, per-node last-restart timestamp, checked live via peer RPC
   fan-out.** Rejected: two managers can each preflight-check clean before
   either records anything, then both restart - a local check can never
   provide atomicity across nodes. **Fixed** by moving enforcement into
   `internal/raft`'s `FSM`, the same state machine every other
   consequential command (`CreateNetwork`, `ApproveJoinRequest`, ...)
   already goes through - Raft's own serialized log-apply order is what
   actually prevents two nodes from acquiring a lease for the same service
   at the same time, not any coordination in caller code (see
   `TestFSM_AcquireRestartLease_ConcurrentRequestsOnlyOneGranted`).
2. Snapshot/restore gaps, caller-authored (rather than leader-authored)
   timestamps and voter snapshots, and a naive "forward the whole RPC to
   the leader" pattern that would have restarted the **leader's** service
   instead of the node the operator actually targeted - all fixed by
   splitting the flow into a leader-mediated reservation
   (`ReserveRestartLease`, the only place that touches `raft.Apply`) and a
   node-local restart (`RestartNodeService`, always executed on the node
   that received the original request, never the leader).
3. An unbounded `service apiary_managerd restart` command racing a lease
   TTL, and a killed command not proving the process actually stopped.
4. **The fatal flaw that reshaped the whole design**: restarting
   `apiary_managerd` kills the very process whose goroutine was supposed to
   observe and confirm the outcome. No amount of post-restart polling
   inside `RestartNodeService`'s own goroutine can work, because that
   goroutine's process is what's being replaced. **Fixed** by moving
   confirmation entirely to the **new** process's own startup
   (`cmd/managerd`'s `confirmPendingRestartOnStartup`) - the first point
   after the restart that is actually alive to do it. This also removed
   the need for a lease TTL, a renewal mechanism, or post-timeout
   verification: a lease now blocks **indefinitely** until either a
   genuine `ConfirmRestartCompleted` clears it (the restarted node's own
   next startup) or an operator explicitly re-acquires with `force=true` -
   a deliberate, logged override, never a silent timeout-based recovery.
5. Two credential designs for the reservation/confirmation RPCs both
   failed to actually close off Admin access: a `CreateAPIKey`-mintable
   `"peer"` role (any Admin could mint one through the public API) and a
   write-only `nodeconfig.Config` field (write-only only hides a value on
   *read*; the Admin who *wrote* it can trivially use the value they just
   supplied). **Fixed** by moving the credential entirely outside any RPC:
   a plain root-owned file, `/usr/local/etc/apiary/restart-guardrail-token`
   (0600, identical content required on every node), read once at
   `managerd` startup and held only in memory - never a `nodeconfig.Config`
   field, never `UpdateNodeConfig`-writable, not even a `GetNodeConfig`
   `_set` boolean. `ReserveRestartLease`/`ConfirmRestartCompleted` check
   the presented bearer token against this in-memory value
   (`crypto/subtle.ConstantTimeCompare`) **entirely outside**
   `checkAuth`/`ValidateAPIKeyHash`/the Viewer<Admin role hierarchy -
   `internal/manager/auth.go`'s `AuthUnaryInterceptor` exempts both
   methods from `checkAuth` the same way `RequestJoinColony`/
   `AuthenticatePassword` already are, for a different reason (a stricter
   check, not none). The comparison requires the configured token to be
   **non-empty** before ever comparing -
   `subtle.ConstantTimeCompare("", "")` returns 1, so an unprovisioned
   node must reject unconditionally rather than let two empty values pass
   as equal (`TestRestartGuardrailTokenValid_RequiresNonEmptyConfigured`).

**Final shape.** New Raft-replicated FSM state (`api/internalpb/state.proto`):
`RestartLease{lease_id, service, holder_node_id, requested_at_unix, force}`
(no TTL - `lease_id` is simply the granting command's own raft log index,
unique and monotonic for free) and `RestartRecord{service, node_id,
completed_at_unix}`, both included in `FSMSnapshotState`. Two new `Command`
variants: `AcquireRestartLease` (blocks if an unconfirmed lease already
exists for the service, or if a **currently-known-voter** node's
`RestartRecord` is within a 10-minute cooldown - a non-voter's own restart
carries no quorum risk and never blocks a voter's; `force=true` overrides
either) and `RecordRestartCompleted` (always writes the record - a real
restart really did complete; releases the lease only on an **exact**
`lease_id` *and* `holder_node_id` match, so a stale or out-of-order
confirmation can never release a different, currently-active lease for
the same node/service pair).

`RestartNodeService`, for `apiary_managerd` only: reserves the lease
in-process (`reserveRestartLease`, no token check - the same trusted
process calling itself, never over the wire), writes a small local
pending-restart file **before** issuing the restart command (so a crash
immediately after issuing it still leaves a discoverable, correctly-
blocking trace), then fires the restart and returns without waiting to
observe the outcome. `frontend`/`restshimd` restarts are entirely
unaffected - neither kills the process handling the RPC call.

`PreflightRestartNodeService` (Viewer-tier, pure local Raft-internal read,
no live dial) previews the guardrail's current state for the Machine
page; always `Allow` for services with no quorum stake.

## Not addressed

- This is not a generic per-RPC preflight system. Only these two
  guardrails exist; every other mutating RPC in `auth.go`'s
  `requiredRoleFor` map is unaffected.
- Synchronized cluster clocks are an assumed precondition, not enforced.
  Leader-authored timestamps stop request forgery but not clock skew
  across a leader election; a negative elapsed value from skew already
  falls out safely from the existing cooldown comparison (blocks rather
  than allows), which is a property of the arithmetic, not a defense
  built for this case.
- The restart-guardrail token closes the **RPC-level** path only: no
  combination of Admin-tier API calls can ever discover or set a valid
  value. It does not, and cannot, exclude a fully root-privileged operator
  who can read the token file directly - no software boundary can. An
  Admin who does obtain a valid token this way can still only produce a
  *more* conservative outcome (a spurious block/cooldown), never bypass a
  real one, which is the safe direction to fail in; both calls are logged
  with caller identity for auditability regardless.
- No web UI control ever calls `ReserveRestartLease`/`ConfirmRestartCompleted`
  directly - they exist solely as `RestartNodeService`'s own internal
  plumbing.
- `confirmPendingRestartOnStartup`'s own retry loop is bounded (5 attempts,
  3s backoff) and gives up loudly rather than retrying forever - a
  persistent failure needs an operator to notice (via
  `PreflightRestartNodeService`'s own persistent `Block`) and investigate,
  matching this codebase's "operator decides, the system never guesses"
  posture (`ForcePurgeVM`/`ForcePurgeJail`) rather than an unbounded
  background loop.
- A genuine two-node forwarding regression test (a follower receiving
  `RestartNodeService`, reserving through a real leader peer, and the
  restart executing on the follower rather than the leader) is not yet
  written - the single-node test suite verifies the mechanism's logic
  exhaustively, but the specific "which physical node runs the restart
  command" property is currently guaranteed by code construction
  (`RestartNodeService` never forwards, only `reserveRestartLease`'s
  Apply-submission half does) and reviewed carefully, not yet proven by a
  live two-node integration test.

## Verification

`internal/guardrail`: table-driven tests for both `Evaluate*` functions.
`internal/raft/fsm_test.go`: grant/block/force/voter-filtering/clock-skew
cases for `applyAcquireRestartLease`; exact-match release semantics and
unconditional record-writing for `applyRecordRestartCompleted`;
`TestFSM_AcquireRestartLease_ConcurrentRequestsOnlyOneGranted` (the core
safety property); snapshot/restore round-trip for both new maps.
`internal/manager`: `TestRestartGuardrailToken_EmptyConfiguredTokenAlwaysRejects`,
`TestRestartGuardrailToken_NoRPCPathCanProduceOrChangeIt` (a freshly-minted
Admin API key never satisfies the guardrail RPCs), the end-to-end
`TestRestartNodeService_ManagerdGoesThroughGuardrail` (grant, block, force
override, real confirm via `ConfirmRestartCompletedLocal` clearing the
lease while the separate 10-minute cooldown correctly remains active),
`TestAuthUnaryInterceptor_RestartGuardrailRPCsBypassCheckAuthEntirely`,
`TestRestartGuardrailTokenValid_RequiresNonEmptyConfigured`, and
role-tier tests for both new Preflight RPCs. `go build ./...`,
`go vet ./...`, `gofmt -l .`, full `go test ./...` all pass.

Live verification against `apiverse`/`apiarium` is pending - to be
recorded here once run, following this project's own established
precedent of documenting exactly what was observed live: provisioning an
identical `restart-guardrail-token` file on both hosts, confirming
`RestartNodeService("apiary_managerd")` is genuinely refused cluster-wide
(not just locally) on a second attempt without `force`, and confirming
the block clears automatically once the restarted node's own next startup
completes - the real test of whether the mechanism this ADR describes
holds up against actual `daemon(8)`-supervised process replacement, not
just a test harness.
