# ADR-0087: Move PAM authentication into managerd

## Status

Accepted

## Context

Earlier research confirmed `internal/pam` (wrapping real `libpam` via
`github.com/msteinert/pam/v2`) is the sole cgo dependency in the entire
module, and the only reason `cmd/frontend` had to be built natively on
the target FreeBSD host instead of cross-compiled - `frontend` is
otherwise a pure gRPC client of `managerd`. Offered three relocation
options (a tiny native PAM helper process, moving PAM into `managerd`,
or reusing `managerd`'s API-key/raft role system for human login), the
user chose moving PAM into `managerd`: it already must run on the
FreeBSD host for real VM/jail/ZFS work, so it absorbs cgo without
adding a new component - at the explicitly accepted cost that total
native-build surface doesn't shrink (it moves from `frontend` to
`managerd`), valuable only for iterating on/deploying `frontend`
independently.

## The detail that actually makes this work - and a mistake caught mid-build

Go's cgo requirement is per-*package*, not per-symbol: any package that
imports `internal/pam` at all requires cgo to build, even if it only
references the `Authenticator` interface and never touches
`PAMAuthenticator` itself. So moving *where PAM runs* isn't enough by
itself - every package on `cmd/frontend`'s import path must stop
importing `internal/pam`, or the cgo requirement follows the import,
not the call site.

This was applied to `internal/frontend` (a new local `Authenticator`
interface, identical shape, replacing the `pam.Authenticator` field
type) - but the first attempt still failed
`GOOS=freebsd GOARCH=amd64 go build ./cmd/frontend` from a non-FreeBSD
host, because `internal/manager` (which `cmd/frontend` also imports
directly, for `manager.Role`/`NewPeerReporter`) had *itself* just been
given a new `authPAM pam.Authenticator` field to support this same
ADR - re-introducing the exact same transitive leak one layer removed.
The fix was the identical pattern applied a second time: a local
`pamAuthenticator` interface inside `internal/manager` too, with
`cmd/managerd` (the only caller of `SetPAMAuthenticator` with a real
implementation) as the sole remaining importer of `internal/pam` in
the entire module. This project already has an established answer for
this shape of problem - duplicate the tiny interface locally rather
than share a dependency edge (`datasetManager`, `vlanManager`,
`VLANStatus` all do this already) - but it has to be applied
everywhere the cgo-requiring import could re-enter, not just at the
first place it's noticed. The actual, verified proof this was done
correctly is `go list -deps ./cmd/frontend/...` no longer listing
`internal/pam` at all, plus a real `GOOS=freebsd GOARCH=amd64 go build
./cmd/frontend` succeeding from this (non-FreeBSD) dev machine for the
first time.

## Design decisions

- **New RPC, `AuthenticatePassword`** (`api/rpc/manager.proto`) checks
  a username/password pair against `managerd`'s own PAM stack.
  Exempted from `checkAuth` entirely (`internal/manager/auth.go`),
  mirroring `RequestJoinColony`'s existing exemption for the identical
  reason: a caller proving their identity here has no Colony API key
  yet, by definition.
- **`StatusResponse` gains `pam_configured`**, replacing `frontend`'s
  own `-pam-service` flag as the "should login be enabled at all"
  signal - `frontend` already calls `Status` at startup, and this
  keeps whether login is configured a single source of truth on
  `managerd`'s side, where the real PAM config now actually lives.
- **New hard requirement: `-pam-service` requires `-tls-cert`/`-tls-key`
  on `managerd`.** This is a real, new risk this design introduces: a
  login password now necessarily transits the `frontend`-to-`managerd`
  network channel (previously checked in-process inside `frontend`,
  never crossing a wire at all) - over the real LAN interface
  `-rpc-addr` is already bound to, not loopback (per ADR-0022's own
  bridge-migration history). Refused outright at startup, not merely
  documented, mirroring the existing `-tls-cert`/`-tls-key` pairing
  check's own "cheap to prevent" posture.
- **`internal/frontend`/`internal/manager` both gained a local,
  structurally-identical `Authenticator`/`pamAuthenticator` interface**
  instead of importing `internal/pam`'s type - see above.

## Migration note for apiarium/apiverse

Both hosts currently pass `-pam-service apiary` in `frontend`'s own
`apiary_frontend_args` (`/etc/rc.conf`). Deploying this moves that flag
(and its value) to `apiary_managerd_args` instead - `frontend`'s own
`rc.conf` args need no login-related flag at all anymore (having
already lost `-role-map` in ADR-0086). Both hosts already have
`-tls-cert`/`-tls-key` configured on `managerd`, so the new safety rail
passes without further changes needed there.

## Consequences

- `cmd/frontend` has zero cgo dependencies now and cross-compiles
  cleanly from any platform, for the first time - confirmed directly,
  not just inferred.
- `cmd/managerd` now requires `CGO_ENABLED=1`/a native FreeBSD build -
  confirmed via the same cross-compile check now failing there
  instead. `raftd`/`restshimd` remain unaffected and cross-compile
  cleanly, as before.
- Full test coverage: `AuthenticatePassword` handler tests (no-PAM
  error path, wraps a fake authenticator correctly, surfaces backend
  errors) using a fake `pamAuthenticator`; a direct
  `AuthUnaryInterceptor` exemption test mirroring
  `RequestJoinColony`'s own; a real end-to-end integration test
  (real raftd + managerd + gRPC client) proving `Status`'s
  `pam_configured` field reflects whether a PAM authenticator is
  configured, since that handler needs a real raft connection to
  exercise at all.

## Correction (2026-09-11, ADR-0096)

`AuthenticatePassword`'s own exemption from `checkAuth` (necessary,
since a caller here has no session yet) had a real, unaddressed
consequence at the time this ADR was written: `internal/frontend`'s
`handleLogin` lockout was the only rate-limiting protecting a real PAM/
UNIX account from repeated guesses, and nothing stopped a network
client from calling this RPC directly, bypassing frontend (and its
lockout) entirely. ADR-0096 adds an equivalent lockout directly in this
handler, closing that bypass at the actual trust boundary rather than
relying on a caller's own good behavior.
