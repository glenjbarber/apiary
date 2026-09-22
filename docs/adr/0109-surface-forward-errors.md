# ADR-0109: Surface leader-forwarding failures instead of swallowing them

## Status

Accepted

## Context

Every RPC that forwards a request to the current raft leader when this
node isn't it - roughly 40 call sites across `internal/manager/server.go`,
covering every VM/jail/network/API-key write, several leader-only reads
(`ListVMs`, `ListJails`, `ListNetworks`, `ListAPIKeys`, `GetVM`, `GetJail`),
and the ADR-0103 restart-guardrail RPCs - followed the same shape:

```go
if leaderHint != "" && s.peers != nil {
    if fwd, ferr := s.peers.X(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
        return fwd, nil
    }
}
return &rpcpb.XResponse{Error: appErr, LeaderHint: leaderHint}, nil
```

When the forward itself failed (`ferr != nil` - a TLS handshake failure,
a peer-auth rejection, a network error), that error was discarded
completely. The caller only ever saw `appErr` - the *original* reason
this node wasn't the leader (typically the bare `raft: this node is
not the leader`) - with no indication a forward was even attempted,
let alone why it failed.

This was a live, real bug, not a hypothetical: a 2026-09-21 incident on
the user's own 4-node Colony (`buzz`/`brood`/`drone`/`sting`, recorded
in SHARED.md) took an extensive SSH/log investigation and a custom-built
diagnostic Go tool to root-cause, purely because the actual failure
(a TLS certificate-hostname-verification mismatch when dialing the
leader by its raw IP) was invisible from the error text. Calling
`CreateJail` directly against the affected node reproduced `Error="raft:
this node is not the leader"` **with `LeaderHint` populated** - proving
a forward had been attempted and failed, but nothing said so.

Two related call sites (`ReserveRestartLease`/`ConfirmRestartCompleted`)
had a worse variant of the same bug: on a failed forward, they returned
a *hardcoded* message - `"not leader and no reachable leader hint..."` -
that is actively false whenever a hint existed and a forward was
actually attempted and failed.

## Decision

A single helper, `augmentForwardError(baseErr, leaderHint string, ferr
error) string`, appends `ferr`'s own text to `baseErr` whenever a
forward was attempted and failed. `ferr == nil` (no forward attempted at
all - no hint, no peers configured, or the base error path was never
reached) returns `baseErr` completely unchanged, so this never alters
the "no peers configured" or "no leader known" cases, only the specific
case this incident found: a forward that was tried and failed.

Every one of the ~40 call sites was converted uniformly: `ferr` is now
captured outside the inner `if`, and the final fallback response uses
`augmentForwardError(...)` instead of the bare original error string.
The two restart-guardrail sites additionally stop returning their
misleading hardcoded text once a hint was actually known and a forward
attempted - they now report the real failure via the same helper.

Two call sites (`GetVMConsole`/`GetVMSerialLog`) use a compound
condition (`ferr == nil && fwd.GetError() == "" && fwd.GetFound() && ...`)
rather than the simple shape above, since a successful forward is only
useful there if the resolved VM also turns out to be found and owned by
this node. These were restructured to capture `ferr` separately so a
genuine forwarding failure is distinguished from "the leader forwarded
fine, but the VM just isn't there" - the latter keeps its existing,
unaugmented error text, since that's not a forwarding failure.

## Consequences

- Any future TLS/auth/network misconfiguration in the peer-forwarding
  path will now show up directly in the API error text (e.g. `raft:
  this node is not the leader (forwarding to leader hint "10.90.0.94:17600"
  also failed: tls: failed to verify certificate: x509: certificate is
  valid for 127.0.0.1, not 10.90.0.94)`), diagnosable in seconds instead
  of requiring live SSH investigation and a custom diagnostic tool.
- This is purely additive to existing error text - no passing test
  relied on the old bare error string being exact, and the full
  existing suite passes unchanged.
- Does not address the separate, already-tracked TODO of automating
  `peer_tls_hostname_map` maintenance on Colony join (SHARED.md,
  2026-09-21) - that's the actual root cause of the incident that
  surfaced this bug; this ADR only closes the diagnosability gap that
  made root-causing it unnecessarily hard.

## Verification

- `internal/manager`: `TestAugmentForwardError` (unit test of the
  helper itself: nil `ferr` leaves `baseErr` untouched; non-nil `ferr`
  preserves the original error and the leader hint while adding the
  real failure reason).
- `TestIntegration_CreateJail_ForwardingFailureSurfacedInError`: a
  genuine end-to-end reproduction of the live incident - two real raft
  nodes (a real leader, a real joined-and-synced follower, not stubs),
  a `CreateJail` call against the follower with its `peers` swapped for
  a fake that fails with the exact TLS error text observed live, and an
  assertion that the response contains both the original raft error and
  the real forwarding failure reason.
- `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), full `go test
  ./...` for the whole repository - all green, confirming none of the
  ~40 touched call sites' existing tests depended on the old bare error
  text.
