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

### 1. `-known-peer-addresses`: an opt-in allowlist for `target_address`

A new `Server.knownPeerAddresses map[string]bool` (nil by default),
set via `SetKnownPeerAddresses([]string)` from a new
`cmd/managerd -known-peer-addresses` flag (comma-separated `host:port`,
parsed with the existing `splitCommaList` helper). A new
`checkTargetAddressAllowed(target string) error` method is called at
the top of all three `target_address` branches
(`RequestJoinColony`/`GetJoinRequestStatus`/`CancelJoinRequest`,
`internal/manager/joincolony.go`): when `knownPeerAddresses` is nil (the
default, unconfigured), every `target_address` is accepted exactly as
before (ADR-0092's original behavior, unchanged); when configured,
`target_address` must exactly match an entry or the call is refused
before ever dialing - `s.peers.*Unauthenticated` is never reached for a
disallowed target.

**Deliberately opt-in, not a forced default.** This project's own real
deployments are a small, fixed set of hosts (four, as of this writing) -
pre-listing them is practical and the natural, low-friction fix for
someone who already knows their Colony's membership. But this RPC's
whole purpose is bootstrapping a node that, by definition, doesn't yet
have a way to know its peers are trustworthy in advance; forcing every
fresh, never-configured deployment to pre-populate this flag before a
first join could ever succeed would break bootstrap with no migration
path. An unconfigured node keeps today's accepted-and-disclosed
posture exactly as ADR-0096 left it.

**Deliberately not the audit's "separately-authenticated enrollment
record" alternative.** That would mean real new plumbing - an
out-of-band token/pairing exchange, closer to how `kubeadm join`
tokens work - genuinely new infrastructure, not a hardening pass over
existing RPCs. The allowlist gets most of the same practical benefit
(closing the SSRF for anyone who configures it) at a fraction of the
implementation and operational cost, and fits this project's own
existing "opt-in hardening flag" pattern (`-peer-tls-ca`,
`-internal-token`) rather than introducing a new one.

**Deliberately CLI-flag-only, not also nodeconfig/UI-editable.** Several
sibling peer flags (`-peer-tls-ca`, `-peer-tls-hostname-map`) are also
persisted via `nodeconfig.Config` and editable from the Machine
Configuration page. Adding that here would mean a proto field, a
`GetNodeConfig`/`UpdateNodeConfig` round trip, and a new UI form field -
real, additional surface area not required to close the actual security
finding. Left as a natural, separately-scoped follow-up if it turns out
to matter in practice, not built speculatively now.

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
   forwards normally; `SetKnownPeerAddresses(nil)` restores the
   original unconfigured (accept-any) behavior after having been
   configured.
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
4. Live, if convenient: confirm an unconfigured node still joins exactly
   as before (no regression for the default case), then configure
   `-known-peer-addresses` on a real Hive and confirm an out-of-list
   `target_address` is refused.
