# ADR-0030: Tiered RBAC with PAM-backed web UI login

## Status

Accepted

## Context

Apiary's web UI had no real user accounts at all: `cmd/frontend`'s
`APIARY_UI_USER`/`APIARY_UI_PASSWORD` env vars (ADR-0019) are a single
shared username/password gating every route identically, and the
external API's `ApiKey` records (ADR-0023) are named credentials with
no role or scope - a valid key can call every RPC once auth is
enabled, or none at all. The request was for real per-identity
accounts with a tiered role hierarchy, authenticated against real
system identity (PAM - and, via PAM's own configuration, transitively
Kerberos and possibly Active Directory), explicitly avoiding any
reliance on a specific well-known UNIX group name (e.g. `operator`).

## Design decisions

- **Auth backend: real PAM via cgo** (`github.com/msteinert/pam/v2`),
  not a pure-Go Kerberos client. FreeBSD's own PAM stack, configured
  by the operator (entirely outside Apiary's code) with
  `pam_unix.so`/`pam_krb5.so`/`pam_ldap.so`/`pam_winbind.so` in a new
  `/etc/pam.d/<service>` file, is what actually delivers Kerberos or
  Active Directory - Apiary only ever calls PAM itself once, the same
  way `login(1)`/`sshd(8)` do. This is a deliberate choice to build
  **one** integration point that three separate asks (PAM, Kerberos,
  AD) all route through, rather than three separate bespoke client
  implementations.
- **Identity → role mapping: an explicit, operator-maintained config
  mapping** (`cmd/frontend`'s new `-role-map` flag), independent of
  any UNIX/AD group - sidesteps the "don't use the `operator` GID"
  requirement entirely by never touching OS groups at all.
- **Role tiers**: Viewer (every read-only RPC/route) < Operator (full
  VM/jail/network create/update/delete/migrate, ISO upload/delete,
  plus the ADR-0029 peer-forwarding RPCs) < Admin (everything,
  including API-key management and the `ForcePurge*` escape hatches).

## `ApiKey` gains a `Role`

`ApiKey` (`api/internalpb/state.proto`) gains `role` - normalized to
`"viewer"` (least privilege) by `applyCreateAPIKey` if empty or
unrecognized, never treated as "no restriction" (the same default-deny
stance ADR-0025 through ADR-0029 already established for reconciler
behavior, applied here to authorization). `ValidateAPIKeyHash`
(`raftd.proto`, the internal, non-leader-restricted RPC ADR-0023
already added) threads the matched key's role back to `checkAuth`
(`internal/manager/auth.go`), which now looks up each RPC's minimum
required role from a new `requiredRole` map (keyed by `FullMethod`,
the same key `statusMethod` already used) and rejects an insufficient
role with `codes.PermissionDenied` - a new, distinct outcome from the
existing `codes.Unauthenticated` (missing/invalid key entirely), so an
operator can tell "wrong role" from "no key at all" apart from the
error alone. **An RPC missing from `requiredRole` defaults to
requiring Admin**, not open access - a fail-closed default so a newly
added RPC can never ship silently under-protected just because someone
forgot to list it.

`api/rpc/manager.proto`'s `CreateAPIKeyRequest`/`APIKeyInfo` gain the
same `role` field for external creation/display.

## `internal/pam`: the one PAM integration point

```go
type Authenticator interface {
    Authenticate(username, password string) (bool, error)
}

type PAMAuthenticator struct{ ServiceName string }
```

`PAMAuthenticator.Authenticate` runs a real `Authenticate` +
`AcctMgmt` PAM sequence (the same two-step `login(1)`/`sshd(8)`
perform, so an account PAM itself considers expired/locked is rejected
even with a correct password) against a configured PAM service name.
`Authenticator` is defined as an interface specifically so
`internal/frontend`'s own tests use a fake, the same
`isoManager`/`VNCLookup`/`VLANStatus` pattern this project already
follows for every other external dependency.

**This package requires cgo** - it wraps the system `libpam` via
`github.com/msteinert/pam/v2`. This is the one significant, deliberate
build-system consequence of this ADR:

- `cmd/frontend` must be built with `CGO_ENABLED=1` on a real FreeBSD
  host (confirmed live: `GOOS=freebsd GOARCH=amd64 go build
  ./cmd/frontend` from macOS - the cross-compile workflow every other
  binary in this project uses - now fails outright with "build
  constraints exclude all Go files", since cross-compiling disables
  cgo by default and there's no cross C toolchain available). Building
  natively on one of the project's FreeBSD boxes (or setting up a real
  FreeBSD cross toolchain) is required going forward for this one
  binary.
- `managerd`, `raftd`, and `restshimd` are **unaffected** - confirmed
  live, all three still cross-compile cleanly from macOS exactly as
  before. Only `cmd/frontend` links PAM.
- A real `/etc/pam.d/<service>` file is a new one-time host
  prerequisite, the same posture as ADR-0022's pf/dnsmasq setup or
  ADR-0026's `hastd_enable=YES`.

## `internal/frontend`: real identity + role-gated routes

`sessionStore` (`session.go`) now carries a `sessionInfo{expiry,
username, role}` per token instead of just an expiry - `Create`
requires a username/role, `Valid` returns the resolved identity
alongside whether the session is live. `Server` replaces its
`authUser string; authPass string` fields with `auth pam.Authenticator`
and `roleMap map[string]manager.Role`. `handleLogin` calls
`s.auth.Authenticate`; on success, looks up `roleMap[user]` - **no
entry means the login is rejected outright** ("no Apiary role is
assigned to this account"), never silently downgraded to Viewer. A new
`requireRole` wrapper gates individual route registrations (Viewer:
every read-only page; Operator: VM/jail/network/ISO mutation routes,
including the create-VM form's own `GET` since a Viewer has nothing
useful to do with a form it can't submit; Admin: the entire `/apikeys`
surface, including just viewing the list) - a no-op when login itself
is disabled, preserving this project's existing "no login configured"
default of leaving every route open. The nav partial shows "logged in
as `<username>` (`<role>`)" when a session exists.

`cmd/frontend` replaces `APIARY_UI_USER`/`APIARY_UI_PASSWORD` with
`-pam-service` (empty disables login entirely) and `-role-map`
(`"admin:alice;operator:bob,carol;viewer:dave"` - parsed once at
startup, rejecting an unrecognized role or a user listed under two
roles).

## Follow-up: login lockout (closes one of the items below)

A new `internal/frontend/lockout.go` adds a `loginAttemptTracker`:
after `defaultMaxFailedAttempts` (5) wrong passwords for one username
within `defaultAttemptWindow` (15 minutes), that username is locked
out for `defaultLockDuration` (15 minutes) - checked in `handleLogin`
*before* ever calling `s.auth.Authenticate`, so a locked-out username
costs no PAM round-trip on retry and can't be used to probe the auth
backend's timing while locked. All three durations are fixed
constants, not flags - the same "simple enough for now" posture
`sessionTTL` already established. A successful login clears any
tracked failures for that username. Keyed by username only, not source
IP - deliberately simple, with an accepted, named tradeoff: someone
who already knows a valid username could lock out its real owner by
failing on purpose (a denial-of-service against that one account, not
a way to get in) - a real cost, judged worth accepting over doing
nothing at all against online password guessing now that a wrong
guess is checked against a real credential.

This also fixed a latent bug this same code path had since the PAM
work landed: `handleLogin` called `s.auth.Authenticate` with no nil
check, so a `POST /login` while login was disabled (`s.auth == nil`)
would have panicked - never hit in practice since the UI's own login
form doesn't render when disabled, but reachable by anyone crafting
the request directly. Fixed with the same disabled-login redirect
`handleLoginPage`'s `GET` already had.

## Deferred (explicitly out of scope for this pass)

- Bespoke direct-Kerberos or direct-LDAP/AD client code in Go - PAM's
  own configuration already bridges to both; a dedicated integration
  is only worth building later if system-PAM delegation proves
  insufficient for a real deployment.
- Any self-service account creation/password-reset UI - PAM already
  manages real accounts; Apiary never touches credentials directly.

## Live verification

Confirmed for real on `apiarium`, end to end: `pkg install go git`
(neither was previously present), the local working tree copied over
and built natively there (`go build ./cmd/frontend` - real cgo
linkage confirmed via `ldd`, showing `libpam.so.6` pulled in, unlike
this project's other, statically-linked pure-Go binaries), a real
`/etc/pam.d/apiary` written (`pam_unix.so` for `auth`/`account`), and
two real, throwaway UNIX accounts created with `pw useradd`. Against
the real running `frontend` binary:

- A real PAM login with the correct password for a Viewer-mapped
  account succeeded (a genuine session cookie issued), while the exact
  same account with a wrong password was rejected with no cookie set.
- That Viewer session could reach a read-only page (`/vms`, 200) but
  was rejected with a real `403 Forbidden` from both an Operator-only
  route (`/vms/new`) and the Admin-only `/apikeys` surface.
- A second, real UNIX account with valid credentials but **no
  `-role-map` entry** was rejected at login outright ("no Apiary role
  is assigned to this account") - confirming the default-deny design
  decision holds against a real PAM success, not just a mocked one.
- Redeploying with that second account mapped to Operator, its session
  could reach `/vms/new` (200) but was still correctly rejected from
  `/apikeys` (403) - confirming the role hierarchy's middle tier
  behaves correctly, not just the two extremes.

All test accounts, the PAM service file, and the built binary were
removed afterward; `apiarium` was left with no trace of the test setup.

## Consequences

- Full test coverage, unit and live: role-hierarchy and `checkAuth`
  unit tests (including the fail-closed-for-unlisted-RPC case) in
  `internal/manager`; real raft-harness integration tests confirming a
  Viewer-role key can read but not write and an Operator-role key can
  write VMs but not manage API keys; `internal/frontend` route-gating
  and role-map-rejection tests against a fake `Authenticator`;
  `-role-map` flag-parsing unit tests in `cmd/frontend`; `internal/pam`
  unit tests against a fake `ServiceName`; `loginAttemptTracker` unit
  tests (locks after the threshold, doesn't cross-lock a different
  username, a success resets the count, both the window and the lock
  duration expire correctly) plus `Server`-level tests confirming
  lockout after repeated failures, that a success never counts toward
  it, and that the fixed nil-auth `POST /login` bug doesn't panic; and,
  per the live verification above, real PAM authentication itself
  confirmed working end-to-end against genuine UNIX accounts on real
  FreeBSD hardware.
- The cgo/native-build requirement for `cmd/frontend` is a real,
  ongoing cost of this design, accepted explicitly in exchange for
  literal PAM (and, transitively, Kerberos/AD) support rather than a
  narrower pure-Go alternative.
