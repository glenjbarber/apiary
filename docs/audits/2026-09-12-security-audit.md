# Security audit: 2026-09-12

## Scope and method

Static review of commit `7f551fa` (`main` at audit start), with attention to
authentication and authorization, peer forwarding, network and process
boundaries, filesystem path handling, command construction, and the six
remediations in ADR-0096.

Validation completed in this worktree:

- `go test ./...`
- `go vet ./...`
- `git diff --check`

All three completed successfully. This was source review only. It did not
probe a live Hive, review deployed configuration, or run a dependency
vulnerability scanner. `govulncheck` was not installed in the audit
environment.

## Findings

### P2: unauthenticated join forwarding remains an outbound connection primitive

`RequestJoinColony`, `GetJoinRequestStatus`, and `CancelJoinRequest` remain
unauthenticated by design so an unjoined Comb can ask to join. When their
`target_address` field is set, `internal/manager/joincolony.go` passes that
caller-controlled `host:port` to the unauthenticated peer client.

ADR-0096 correctly prevents the serious prior failure: the new
`dialUnauthenticated` path does not attach this Hive's peer API key. The
caller can therefore no longer exfiltrate that credential through a target it
controls. The call still makes managerd initiate a TLS or plaintext gRPC
connection to an arbitrary supplied address, however. An attacker able to
reach managerd's gRPC listener can use response timing and returned connection
errors as a limited internal-network reachability oracle.

This is a residual, lower-severity SSRF class issue, not a regression of the
ADR-0096 credential-exfiltration fix.

Recommended resolution: make join routing select a known Colony member from
trusted local configuration or a separately authenticated enrollment record,
rather than accepting a raw socket address from the unauthenticated caller.
If arbitrary bootstrap routing must remain, constrain it with an explicit
allowlist of operator-configured addresses and ports. Do not restore the peer
API key on this path.

### Configuration risk: security controls remain opt-in

The default processes still permit plaintext transport, and managerd remains
unauthenticated until the first API key is created. Frontend login is also
disabled unless managerd is configured with `-pam-service`. These defaults are
documented and deliberate, so this is not a newly discovered implementation
defect. It is nevertheless the largest operational security boundary in a
real deployment.

Any managerd or restshimd listener reachable beyond a fully trusted network
should use TLS, API-key authentication, and restrictive packet filtering.
Frontend deployments should use TLS and PAM-backed login. The existing
bootstrap documentation describes these controls, but a future hardening
effort should consider refusing an externally bound plaintext managerd once
API-key authentication is active.

## Verified controls

- ADR-0096's caller-controlled join forwarding uses
  `dialUnauthenticated`, so the peer API key is not attached to the outbound
  request.
- `-peer-api-key-file` offers a file-backed alternative to exposing the key
  in managerd's command line.
- Managerd now applies PAM lockout tracking directly to
  `AuthenticatePassword`, closing the former frontend-only rate-limit bypass.
- VM console RPCs and frontend routes require the Operator role, not Viewer.
- VM and jail IDs are constrained at the raft FSM boundary; ZFS operations
  remain scoped beneath their configured base dataset and use argument arrays,
  not shell interpolation.
- Restartable rc.d services are selected from a fixed allowlist.
- Frontend sessions use cryptographically random tokens with `HttpOnly` and
  `SameSite=Lax` cookies. The VNC WebSocket endpoint independently checks
  same-origin handshakes.

## Disposition

No application-code change is included in this audit commit. The P2 finding
needs an explicit join-enrollment design decision before implementation.
The report is intentionally committed only on
`codex/security-audit-2026-09-12` for review.
