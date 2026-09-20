# ADR-0108: Fixed listener ports, not operator-editable

## Status

Accepted

## Context

Every `<host>:<port>` field the web UI exposes - managerd's own gRPC
bind address, frontend/restshimd's own HTTP(S) listener, the
`manager_addr` fields frontend/restshimd use to dial managerd, and the
join-flow's `target_address`/`raft_bind_address`/
`target_managerd_address`/`raft_bind` fields - names an Apiary daemon
whose port is, by this project's own long-standing convention, fixed:
raftd 17600, managerd 17700, frontend 8080, restshimd 8081. Which host
runs a given daemon varies across a fleet; which port it listens or
dials on does not. Letting an operator type an arbitrary port into
these fields serves no real use case and is a real, previously
documented misconfiguration risk (this project's own docs already warn
about port mismatches in several places). Separately, the shared
`BindAddressOptions` datalist offered all four ports' worth of
suggestions on every field, meaning the browser's own autocomplete
could suggest raftd's port for the frontend HTTP address field.

## Decision

Every such field is now host-only in the UI: the operator enters just
the host/IP, and the daemon's own fixed port renders next to it as
plain, non-editable text (`.host-port`/`.fixed-port` in
`web/templates/layout.html`). `internal/frontend/fixedport.go` defines
the four fixed ports as constants and two small helpers: `hostOnly`
(also registered as a template function, for read-only display of an
existing full address) and `withFixedPort`, which joins an
operator-typed host with the correct fixed port - and strips any port
the host value might already contain, so a value pasted from
elsewhere, or a request crafted outside the browser entirely, gains
nothing by including one. This is enforced server-side, in every
handler that previously read a raw `*_addr` form field, not only
hidden by the UI.

The shared bind-address datalist (`NodeConfig.BindAddressOptions`,
`internal/frontend/convert.go`) is replaced with `HostOptions` - bare
hosts/IPs only, since the port suggestion problem no longer exists once
the port itself isn't a field.

Separately, but touching the same fixed-port list: `apiaryinstall`
gained a new `etc-services` check (`internal/install/checks.go`),
registering Apiary's own four daemons in `/etc/services` (services(5))
- a `netstat -p`/`sockstat`/`getservbyname(3)` convenience only; no
Apiary daemon consults `/etc/services` to find its own port. RiskSafe,
mirroring the existing `pf-anchor` check's own "back up before mutating
a shared system file" caution exactly. Fails closed on a real conflict:
if another entry already claims one of these ports under a different
name, the check reports Misconfigured and Apply refuses to touch the
file at all, rather than silently duplicating or overwriting an
existing registration.

## Consequences

- An operator can no longer misconfigure a daemon's own listener or
  dial-target port through the web UI, by design - the port is simply
  not a field anymore, not merely defaulted or validated.
- `docs/bootstrap.md`'s own port hints keep their present accuracy; a
  fresh install (or `-apply`) now also documents those same ports at
  the OS level via `/etc/services`.
- The `known_peer_addresses` field (a comma-separated list, ADR-0097)
  is deliberately left untouched - its shape (multiple host:port pairs
  in one free-text field) doesn't fit the same host-only-input
  treatment without a materially different, multi-value UI widget; not
  pursued here.

## Verification

- `internal/frontend`: `TestHostOnly`, `TestWithFixedPort_
  IgnoresAnyPortInHost`, plus updated existing tests across
  `daemon_config_test.go`/`machine_test.go`/`server_test.go`/
  `standalonejoin_test.go` confirming every affected handler joins a
  submitted host with the correct fixed port, never a client-supplied
  one.
- `internal/install`: `TestEtcServicesCheck` (append, backup, idempotent
  re-apply), `TestEtcServicesCheckMissingFile`, `TestEtcServicesCheckPortConflictFailsClosed`
  (a real bug was caught and fixed while writing these: the service-name
  column's fixed-width padding produced zero separating whitespace for
  a 17-character name, silently breaking idempotency - `apiary-restshimd`
  ran together with its own port on every re-apply).
- `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), full
  `go test ./...` for the whole repository.
- Manual visual check: rendered the Machine page via the existing test
  harness to a static file and confirmed every affected field shows a
  host-only input with the fixed port as adjacent plain text, in the
  same browser session used for the sidebar-tree visual check.
