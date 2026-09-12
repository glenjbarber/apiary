# ADR-0096: Security audit follow-up (six findings)

## Status

Accepted

## Context

The user asked for a fresh security audit of the codebase, independent
of ADR-0067's own 2026-09-06 audit. Six findings came out of it, ranging
from a genuinely serious pre-authentication SSRF/credential-exfiltration
path to a documentation-only consistency gap. Each is fixed here; none
required rethinking an existing architectural decision.

### 1. Unauthenticated SSRF + `-peer-api-key` exfiltration via join-colony `target_address`

`RequestJoinColony`/`GetJoinRequestStatus`/`CancelJoinRequest` are
deliberately exempt from `checkAuth` (ADR-0083: a joining Comb has no
Colony API key yet, by definition). ADR-0092 added a caller-supplied
`target_address` to all three so a joining Comb's own managerd could
dial the operator-named existing Colony member directly. The handlers
passed that address, unvalidated, into `PeerReporter.dial()`
(`internal/manager/peer.go`), which attaches this node's own configured
`-peer-api-key` as a Bearer token on every call whenever one is set -
regardless of whether the dialed address is a real, trusted peer.

Concretely: an unauthenticated network caller who can reach any
managerd's gRPC port (already LAN-reachable per ADR-0067's own finding)
could call `RequestJoinColony` with `target_address` pointing at a host
they control. Managerd would dial it and hand over its own shared peer
secret in the `Authorization` header - a real credential-exfiltration
path requiring no authentication at all, on RPCs that exist specifically
*because* the caller has no credential yet. Even without a key
configured, this is still a generic pre-auth SSRF primitive: an
unauthenticated caller can make managerd originate arbitrary outbound
connections to a host and port of their choosing.

### 2. `-peer-api-key` as a literal CLI argument

Separately, and independent of file permissions: `cmd/managerd`'s
`-peer-api-key <value>` puts the secret directly in the process's own
argument list, which is visible to any local user via `ps(1)`/
`procstat(1)` on FreeBSD regardless of `/etc/rc.conf`'s file mode. This
was true even before yesterday's `/etc/rc.conf` mode-644 reversion
(`rcconf-default-perms`, 2026-09-11) and remains true after it - fixing
`rc.conf`'s own permissions was never going to close this on its own,
since the exposure isn't really about the config file at all once the
process is running. This was confirmed live: `apiverse` and `apiarium`
both currently carry a real `-peer-api-key` value directly in
`apiary_managerd_args`.

### 3. PAM brute-force lockout bypassable by calling managerd directly

ADR-0087 moved PAM authentication into managerd's own
`AuthenticatePassword` RPC, also exempt from `checkAuth` by design (the
caller has no session yet). The only rate-limiting (`loginAttemptTracker`)
lived in `internal/frontend`'s `handleLogin` - the intended caller, but
not the only one that can reach this RPC. Since `AuthenticatePassword`
is unauthenticated and managerd's port is LAN-reachable, any network
client could call it directly in a tight loop, skipping frontend (and
its lockout) entirely, and brute-force a real PAM/UNIX account with no
rate limit at all.

### 4. RBAC tier mismatch: VM console control at Viewer, not Operator

`GetVMConsole`/`ProxyVMConsole` (and the matching frontend routes
`/vms/{id}/console`, `/vms/{id}/console/ws`) sat at Viewer - the same
tier as every other read-only report in this codebase. But a VM console
is a full bidirectional VNC/RFB tunnel: `proxyConsole`
(`internal/frontend/console.go`) pumps bytes both ways, so keyboard and
mouse input reach the guest, not just a framebuffer view. A Viewer-tier
account could therefore reboot a VM, reach single-user mode, or interact
with a bootloader - a materially different capability than "read-only"
implies for the lowest tier this project defines everywhere else.

### 5. `BaseTemplate`/`CloneFromSnapshot` skip FSM-boundary validation

ADR-0067 established the pattern of validating every caller-supplied,
later-interpolated identifier at the raft FSM boundary that owns it
(`validResourceID`/`validInterfaceName`/`validHostname`), with defense
in depth at the renderer that actually consumes the value. `JailDefinition.BaseTemplate`
(ADR-0084) and `VMDefinition.CloneFromSnapshot` (ADR-0095) were added
later and never got the FSM-boundary half of that pattern - they relied
solely on `internal/zfs.Manager`'s own `path()`/`snapshotPath()`
confinement (verified correct: it already rejects `..`, absolute paths,
and an embedded `/` in a snapshot suffix). Not independently exploitable
today, but a real gap against this project's own stated doctrine, and
the kind of thing a future refactor of the zfs layer could silently
reopen if the FSM check isn't also there as a second, independent line
of defense.

### 6. `RestartNodeService` missing from the explicit role map

`internal/manager/auth.go`'s `requiredRole` map is documented as
listing every RPC explicitly so a missing entry is never mistaken for
an oversight. `RestartNodeService` was absent - safely defaulting to
`RoleAdmin` via `requiredRoleFor`'s fail-closed default (not a
vulnerability), but inconsistent with the map's own stated completeness
goal.

## Decision

### 1 & 2: `dialUnauthenticated` + `-peer-api-key-file`

`PeerReporter.dial` is split into `dialOpts(addr string, attachAPIKey bool)`,
with `dial` (`attachAPIKey=true`, unchanged behavior for every existing
caller) and a new `dialUnauthenticated` (`attachAPIKey=false`). Three
new `PeerReporter` methods - `RequestJoinColonyUnauthenticated`,
`GetJoinRequestStatusUnauthenticated`, `CancelJoinRequestUnauthenticated`
- use `dialUnauthenticated` and are the only thing
`internal/manager/joincolony.go`'s three `target_address` branches call
now. The *leader-hint* forwarding paths (`RequestJoinColony`'s own
fallback, `ApproveJoinRequest`/`RejectJoinRequest`/`PurgeJoinRequest`)
are untouched - those addresses come from this node's own trusted raft
state, not a caller, and still authenticate normally. `PeerForwarder`
gained the three new methods; the existing `fakeJoinColonyPeerForwarder`
test double now implements the `*Unauthenticated` variants, since
that's what the code under test actually calls.

Separately, `cmd/managerd` gained `-peer-api-key-file` (a plain
"read the key from this file" flag, mirroring `-cloudflare-token-file`'s
own established "never a flag value" precedent exactly), resolved via a
new pure `resolvePeerAPIKey(flagKey, keyFile string) (string, error)`
function - mutually exclusive with `-peer-api-key`, trims the file's
content. This is a real, distinct fix from #1: even with the SSRF path
closed, a key passed as `-peer-api-key <value>` is still visible via
`ps`/`procstat` to any local user, independent of `/etc/rc.conf`'s own
permissions. `-peer-api-key` itself is kept (not removed) for backward
compatibility, with its flag help text now pointing at the file-based
alternative.

### 3: `pamLockoutTracker` in `internal/manager`

A new `internal/manager/lockout.go` duplicates
`internal/frontend`'s `loginAttemptTracker` shape exactly (same fixed
5-attempts/15-minute-window/15-minute-lockout defaults) as
`pamLockoutTracker` - duplicated rather than shared, matching this
project's own established convention for small cross-package
duplication (e.g. `peer.go`'s `apiKeyCredentials`). `Server` gained an
always-initialized `pamLockouts` field (set in `NewServer`, never nil);
`AuthenticatePassword` checks `Locked` before ever calling
`authPAM.Authenticate`, and calls `RecordFailure`/`RecordSuccess`
afterward. This makes managerd's own RPC boundary enforce the same
protection frontend already had, instead of relying entirely on
whichever HTTP layer happens to sit in front of it.

### 4: Console access reclassified to Operator

`internal/manager/auth.go`'s `requiredRole` map moves `GetVMConsole`/
`ProxyVMConsole` from `RoleViewer` to `RoleOperator`; `internal/frontend/server.go`'s
matching routes are now wrapped in `s.requireRole(manager.RoleOperator, ...)`,
the same gate every other state-changing route already uses. This is a
real behavior change for any existing Viewer-tier account: they can no
longer open a VM's console at all. That's the correct outcome, not a
regression - a Viewer session should never have had interactive control
of a guest in the first place.

### 5: `validSnapshotRef` at the FSM boundary

A new `validSnapshotRef(ref string) bool` in `internal/raft/fsm.go`
splits on `@` and validates both halves with `validResourceID`'s own
character class - a value that passes can never contain a `/` or a
second `@`. `applyCreateVM`/`applyUpdateVM` reject an invalid, non-empty
`clone_from_snapshot`; `applyCreateJail`/`applyUpdateJail` reject an
invalid, non-empty `base_template` using `validResourceID` directly
(it's a bare name, not a `dataset@snapshot` pair). Empty stays valid in
both (the existing opt-out). This is explicitly defense in depth, not a
fix for a proven exploit - `internal/zfs.Manager`'s own validation
already correctly rejects the same inputs.

### 6: `RestartNodeService` listed explicitly

Added to `requiredRole` as `RoleAdmin` with a comment explaining it was
already this by default - purely closes the map's own stated
completeness gap, no behavior change.

## Consequences

- **Real behavior changes an operator should know about**: an existing
  Viewer-tier account loses VM console access (finding #4, intentional).
  `-peer-api-key` set directly as a flag still works, but its help text
  now steers toward `-peer-api-key-file`; migrating requires writing the
  key to a file and restarting managerd with the new flag, tracked as a
  live-host follow-up in `SHARED.md`, not done as part of this code
  change.
- Finding #2 does not, by itself, retroactively protect `apiverse`/
  `apiarium`'s *currently* running processes - the CLI-argument exposure
  persists until each host is actually migrated to `-peer-api-key-file`
  and its managerd restarted with the secret removed from `rc.conf`'s
  own argument string.
- Findings #5 and #6 are defense-in-depth/documentation-completeness
  fixes with no observable behavior change for any valid existing input.
- New regression tests: `TestPeerReporter_RequestJoinColonyUnauthenticated_NeverAttachesAPIKey`
  (and its `_AttachesAPIKey` contrast case) for #1;
  `TestResolvePeerAPIKey_*` for #2's file-reading logic;
  `TestServer_AuthenticatePassword_LocksOutAfterRepeatedFailures` and
  `_SuccessDoesNotCountAsFailureTowardLockout` for #3;
  `TestRequiredRoleFor_GetVMConsoleIsOperator`/`_ProxyVMConsoleIsOperator`
  and `TestServer_ConsoleRoutes_ViewerBlockedByRouteGate` for #4;
  `TestFSM_Apply_CreateVMInvalidCloneFromSnapshotRejected` and
  `TestFSM_Apply_CreateJailInvalidBaseTemplateRejected` for #5.
- `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .` all
  clean.

## Follow-up (2026-09-12): bounded timeout on unauthenticated forwarding

A second, independent audit (`docs/audits/2026-09-12-security-audit.md`,
Codex) reviewed this ADR's own fixes directly against `main` and
confirmed all six land correctly. It also raised a residual point on
finding #1 that this ADR already disclosed but didn't fully close:
closing the credential leak didn't stop `target_address` from making
managerd dial an arbitrary caller-chosen host, usable as a limited
network-reachability oracle. The audit's full recommended fix (route
joins through trusted local configuration or a separately-authenticated
enrollment record) is correctly flagged as needing its own design
decision, not something to patch in unilaterally - not attempted here.

What *is* fixed here, since it's bounded and doesn't touch the feature's
trust model at all: `RequestJoinColony`/`GetJoinRequestStatus`/
`CancelJoinRequest`'s three `target_address` branches had no timeout of
their own on the outbound dial - a target that never completes a TCP
handshake (a black-holed IP, a firewalled port) could hang the handling
goroutine for as long as the OS's own connect timeout, on top of the
reachability-oracle behavior itself. A new `defaultUnauthenticatedForwardTimeout`
(10s, matching `defaultApplyTimeout`'s existing value) wraps each of the
three forwarding calls via `context.WithTimeout`, independent of
whatever deadline the caller's own incoming request carries. This
doesn't eliminate the reachability-oracle primitive (an attacker can
still learn "did this connect within 10s" for an address of their
choosing) - only the unbounded-hang / mild resource-exhaustion angle on
top of it.
