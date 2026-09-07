# ADR-0077: Origin CA certificate expiry health and scheduled renewal

## Status

Accepted

## Context

ADR-0072 shipped the actual Origin CA issuance transaction (local ECC
key/CSR generation, atomic install, non-secret inventory), and a later
change (`dea9cdb`, "Add Cloudflare Origin CA activation controls")
added the Admin-facing API/UI slice that lets an operator actually
trigger it. That change's own completion note named the real gaps
still open: "Expiry health, scheduled renewal, certificate revocation,
and service support beyond managerd remain future work."

This ADR picks up the two gaps with the clearest operator value and
the smallest, safest scope:

1. **Expiry health.** Today the Machine Configuration page's
   certificate table shows a bare expiry date and nothing else - an
   operator has to remember to check it, and there is no visual signal
   that a certificate is close to (or past) expiry.
2. **Scheduled renewal.** There is no automation at all today; every
   renewal is a manual "Issue or renew" click, with no reminder to make
   it happen before the certificate actually expires.

**Certificate revocation** and **service support beyond managerd** are
explicitly deferred, not attempted here:

- Revocation needs its own Cloudflare API call, its own audit trail,
  and a real decision about what happens to a service currently
  serving the revoked certificate - a different-shaped problem from
  "keep an existing certificate current," not a natural extension of
  it.
- `IssueOriginCertificate` today hard-validates that the requested
  certificate name matches managerd's own configured `TLSCert`/
  `TLSKey` paths specifically (`internal/manager/server.go`) - there is
  no equivalent tracked configuration for `frontend`'s own separate
  `-tls-cert`/`-tls-key` flags anywhere `managerd` can see, since
  `frontend` is a distinct process with its own flags, not part of
  `internal/nodeconfig`. Extending this validation to `frontend` (or
  any other service) is a real, separate design question - what
  tracks that service's TLS paths, and how does managerd learn them -
  not a small addition to this ADR's scope.

## Decision

### Expiry classification: `origincert.ExpiryStatus`

`internal/origincert.InventoryEntry` gains an `Expiry(now time.Time)
ExpiryStatus` method classifying a certificate as `ExpiryOK`,
`ExpirySoon` (within `RenewalWindow`, 30 days), or `ExpiryExpired`
(already past `ExpiresAt`). This is deliberately unrelated to
`internal/health`'s node-health computations - it only ever describes
one certificate's own remaining lifetime, computed fresh from the
current time rather than stored, so it's never stale.

The Machine Configuration page's Origin CA table renders this as a
colored badge next to the expiry date (reusing the existing `.badge`
CSS vocabulary - `ok` aliases to the same green as `ready`/`healthy`,
`soon` to the same amber as `pending`/`creating`, `expired` to the same
red as `error`/`blocked`), computed in `internal/frontend/machine.go`
from the existing `expires_at_unix` wire field - no new proto field was
needed for this half.

### Scheduled renewal: an opt-in flag per certificate, checked hourly

`InventoryEntry` gains two new fields: `AutoRenew bool` and
`ValidityDays int` (the latter didn't exist before - it's now needed
so a renewal can request the same validity period as the original
issuance without the operator re-entering it). Both are set only at
issuance time, via a new `auto_renew` checkbox on the existing "Issue
or renew" form and a matching `auto_renew` field on
`IssueOriginCertificateRequest`/`OriginCertificateInfo`.

A new `internal/origincert.Renewer` type (`Config`, `Issuer`,
`RestartService`, `Now` - all injectable, matching every other
testable capability in this codebase) checks the local inventory on
each `RunOnce`: any entry with `AutoRenew` set and `Expiry() !=
ExpiryOK` gets re-issued through the exact same `Issue` function a
manual renewal uses (same validation, same atomic install, same
inventory update), then its own `Service` is restarted via the
existing `RestartNodeService` RPC path. A failure renewing one
certificate is joined into the returned error but never blocks the
others - `errors.Join` over per-entry failures, not a single
first-error-wins return.

`cmd/managerd` ticks this on a new, independent `-origin-ca-renewal-
check-interval` flag (default 1 hour) - mirroring `runReconcileLoop`/
`runAssumptionCheckLoop`'s exact shape (immediate first run, then one
per tick, errors logged not fatal) rather than folding it into the
existing cluster `Reconciler`: this isn't cluster/VM/jail
reconciliation, it's a local, Hive-only certificate lifecycle concern,
and `cmd/managerd` already runs multiple independent periodic loops
side by side for exactly this kind of unrelated-concern separation.
Checking hourly against a 30-day window leaves wide margin without
needing tick-precision.

### Why re-read `Config` every tick instead of storing it once

`Renewer.Config` is a closure re-invoked on every `RunOnce`, not a
static field, so an Origin CA directory or token-file path edited
through the Machine Configuration page (a plain, freely-editable form,
unlike the write-once resource-scope paths) takes effect on the very
next tick without a `managerd` restart - the same reasoning
`internal/nodeconfig`-backed capabilities elsewhere in this codebase
already follow.

## Consequences

- An operator can now tell at a glance, from the Machine Configuration
  page alone, whether a certificate needs attention - no need to do
  date arithmetic against today's date by hand.
- A certificate marked Auto-renew keeps itself current indefinitely
  without operator action, as long as its token file stays valid and
  readable and Cloudflare's API stays reachable - both failure modes
  are logged (`managerd: origin-ca renewal: ...`) but never fatal to
  `managerd` itself.
- A certificate issued before this ADR has no stored `ValidityDays`
  (defaults to 0) and cannot practically have `AutoRenew` set anyway,
  since the only way to set it is through the issuance form, which
  always supplies a validity period - there is no real path to an
  inconsistent AutoRenew-without-ValidityDays state through normal use.
- No proto or code changes were needed for certificate revocation or
  non-managerd service support - both remain exactly as absent as
  before this ADR, disclosed above rather than half-attempted.
- `Renewer` has no raft dependency and no cluster-reconciliation
  dependency, matching `internal/origincert`'s own existing "no raft
  dependency" framing - it is pure local-Hive certificate lifecycle,
  identical in spirit to the manual issuance flow it reuses.

## Verification

Unit tests: `internal/origincert/renew_test.go` - a certificate without
`AutoRenew` is never renewed even when expiring; a healthy `AutoRenew`
certificate is left alone; an `ExpirySoon` and an already-`ExpiryExpired`
`AutoRenew` certificate are both renewed and their service restarted;
one entry's renewal failure doesn't block a second, healthy renewal in
the same tick (verified via a per-call fake `Issuer` that fails only for
one specific hostname); `InventoryEntry.Expiry` boundary-tested across
OK/soon/expired, including the exact expiry instant. `internal/manager`
- `AutoRenew` round-trips through `IssueOriginCertificate` into
`ListOriginCertificates`, and defaults to `false` when not requested.
`internal/frontend` - the issuance form forwards the `auto_renew`
checkbox (both checked and omitted); the Machine Configuration page
renders both the `soon` and `expired` badges and both the "on"/"off"
auto-renew badges from real RPC responses. `go build ./...`, `go vet
./...`, `gofmt -l`, `git diff --check`, and the complete `go test ./...`
suite all pass.
