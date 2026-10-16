# ADR-0097: Join-flow hardening (target-address allowlist, pre-approval reachability check)

## Status

Accepted

## Context

Two separate, previously-disclosed gaps in the Colony-join flow
(ADR-0083/ADR-0092), both explicitly left open rather than fixed at the
time:

1. **The 2026-09-12 independent audit's residual finding**
   (`docs/audits/2026-09-12-security-audit.md`, reviewed and merged in
   ADR-0096's own follow-up). ADR-0096 closed the credential-leak half
   of the `target_address` SSRF finding (`dialUnauthenticated` no
   longer attaches this node's `-peer-api-key`), and a later follow-up
   bounded the dial-hang half (`defaultUnauthenticatedForwardTimeout`).
   What remained open: `RequestJoinColony`/`GetJoinRequestStatus`/
   `CancelJoinRequest` are deliberately unauthenticated (a joining Comb
   has no Colony API key yet), so `target_address` is still whatever an
   unauthenticated caller supplies - usable as a limited
   network-reachability oracle even with no credential to steal. The
   audit itself flagged this as needing "an explicit join-enrollment
   design decision," not a unilateral patch.

2. **A real, previously-lived incident this session** (see `SHARED.md`'s
   own dated entries): approving a join before the joining node's
   `raftd` was actually reachable stranded the raft cluster twice.
   `AddVoter` commits immediately under whatever quorum exists the
   instant it's called; reversing it needs agreement from every current
   voter, including one that was never reachable in the first place -
   the only recovery available in this codebase is a full raft state
   wipe (`raftd -reset yes-wipe-raft-state`). This was offered as a
   follow-up during that incident and not built at the time.

Asked to resolve both, the user chose to proceed with both fixes now
rather than defer further.

## Decision

### 1. `-known-peer-addresses`: a fail-closed allowlist for forwarded `target_address`

`Server.knownPeerAddresses map[string]bool`, set via
`SetKnownPeerAddresses([]string)` from the persisted node config or the
`cmd/managerd -known-peer-addresses` flag (comma-separated `host:port`,
parsed with the existing `splitCommaList` helper). A new
`checkTargetAddressAllowed(target string) error` method is called at
the top of all three `target_address` branches
(`RequestJoinColony`/`GetJoinRequestStatus`/`CancelJoinRequest`,
`internal/manager/joincolony.go`): when `knownPeerAddresses` is empty or
unset, forwarded `target_address` requests are refused before any dial;
when configured, `target_address` must exactly match an entry or the
call is refused. This replaces the previous accept-any default, which
left an unauthenticated reachability oracle on every unconfigured Comb.

The Web UI forwarding path on a joining Comb requires
`known_peer_addresses` to include the selected Colony member. The
installer seeds that address when run with
`apiaryctl install --colony-member HOST:PORT`; operators can extend the
list as needed. Direct `apiaryctl join-introduce` already dials its
selected Colony member, so it leaves `target_address` empty. The target
records the request locally or forwards it through the existing Raft
leader path and makes no second unauthenticated dial. Direct status
polling is also performed against the named member without forwarding.
Single-Comb bootstrap and direct CLI enrollment therefore do not need
an allowlist.

**2026-10-16 update: fail closed and apply config changes live.** The
original opt-in default left the audited reachability oracle available
on every Comb without an allowlist, so #84 makes the empty configuration
deny forwarded targets. `known_peer_addresses` was already persisted in
`nodeconfig.Config` and editable on the Machine page; successful config
updates now refresh the in-memory allowlist immediately. Startup flag
configuration remains supported.

### 2. Pre-approval reachability check

`ApproveJoinRequest` (`internal/manager/joincolony.go`) now calls a new
`Server.reachabilityCheck func(ctx, addr) error` field against
`pending.raft_bind_address` before ever calling `AddVoter` - a
refusal short-circuits the whole handler with a clear error naming the
actual risk, rather than proceeding into a membership change that
might never be reversible. Production wires this to a new
`dialReachable` (a plain TCP `net.Dialer.DialContext`, bounded by a new
`preApprovalReachabilityTimeout`, 5s); tests substitute a fake function
directly on the field (package-internal, no exported setter needed for
a check this narrowly test-only-overridden).

This is a plain TCP connect, not a real raft protocol handshake - it
cannot guarantee the joining node's raft implementation is healthy,
only that *something* is listening on the claimed address. That's a
deliberate, disclosed limit: it catches the exact failure this project
has actually hit (approving before the joiner's `raftd` process was up
at all), not every conceivable misconfiguration, and doesn't attempt to
be a full health check that duplicates `internal/assumecheck`'s own,
separate job.

## Critical files

- `internal/manager/server.go` - `knownPeerAddresses` field,
  `SetKnownPeerAddresses`, `reachabilityCheck` field, `NewServer`'s
  `dialReachable` wiring.
- `internal/manager/joincolony.go` - `checkTargetAddressAllowed`,
  `dialReachable`, `preApprovalReachabilityTimeout`, the three
  allowlist call sites, `ApproveJoinRequest`'s new pre-`AddVoter` check.
- `cmd/managerd/main.go` - `-known-peer-addresses` flag and
  `SetKnownPeerAddresses` wiring.
- `docs/audits/2026-09-12-security-audit.md` - the finding this
  partially closes.

## Verification

1. `internal/manager/joincolony_test.go` - allowlist unit tests: a
   disallowed `target_address` is refused with the peer never dialed
   (`lastAddr` stays empty) for all three RPCs; an allowlisted one still
   forwards normally; `SetKnownPeerAddresses(nil)` keeps forwarding
   disabled.
2. `internal/manager/integration_test.go` - a new
   `newManagerdRPCClientAndServer` harness variant (mirrors
   `newManagerdRPCClientFull`, also returns the `*Server` so a test can
   override `reachabilityCheck`) backs two new tests: an unreachable
   `raft_bind_address` is refused AND never actually becomes a raft
   voter (checked via `Status().Members`, not just the error string);
   a genuinely reachable one (a real `raftnode.New`, mirroring the
   existing `TestIntegration_ApproveJoinRequest_AddsRealRaftVoter`)
   still approves normally through the same harness.
3. `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .` all
   clean.
4. Live, if convenient: confirm direct CLI enrollment works with no
   allowlist, an unconfigured forwarding request is refused, and an
   out-of-list target is refused when an allowlist is configured.

For the 2026-10-16 fail-closed update, focused validation passed:
`go test ./internal/manager` filtered to the three forwarding RPC tests,
the empty-allowlist case, live config refresh, and direct join request;
`go test ./cmd/apiaryctl` filtered to join request construction;
`go test ./cmd/managerd -run TestKnownPeersWarning`; and
`go test ./internal/hostinstall -run 'Test.*KnownPeer'`. The same focused
manager tests passed with `-race`. The full manager package run was
stopped after 87 seconds of verbose Raft integration output without a
reported test failure.
