# ADR-0007: internal/jail lifecycle

## Status

Accepted

## Context

`internal/jail` is the second FreeBSD-specific package, following the
same pattern `internal/zfs` established: shell out to the real system
tool (`jail(8)`/`jls(8)`), scope operations so Apiary can't affect
resources it didn't create, and verify against the real VM.

## Decisions

### Jails are scoped by name prefix, not a hierarchical namespace

`internal/zfs`'s `Base` path works because ZFS datasets are hierarchical
— `Base/*` is a real containment boundary enforced by ZFS itself. Jails
have no equivalent: `jail(8)` names are a flat namespace (dots are only
meaningful for FreeBSD's *nested*-jail hierarchy, which this package
doesn't use). The closest available scoping tool is a name prefix
(`Manager.Prefix`, e.g. `apiary-`): every jail this package creates is
named `Prefix+name`, and `ListJails` filters `jls`'s system-wide jail
list down to only names starting with `Prefix` before stripping it back
off. This is enforced convention, not a kernel-level guarantee like
`Base` scoping is for ZFS — worth remembering if a bug elsewhere ever
constructs a `Manager` with an empty or overly generic prefix.

### Jails are identified by name, not JID

`jail(8)` assigns a fresh numeric JID each time a jail starts; a name is
stable across restarts and matches the `id` field convention already
used for `VMDefinition`. `Info.JID` is still surfaced (useful for
external tooling/debugging) but is not how this package's own methods
address a jail.

### `ip4=inherit` for v1, not dedicated networking

Jails share the host's network stack rather than getting their own
IP/VNET. Real IP allocation and VNET jails are a meaningfully separate
concern (coordinating address assignment is itself ephemeral-state-like
work) and aren't needed to prove the create/list/remove lifecycle this
slice is about.

### Tests run as root, not a delegated user

Unlike ZFS, FreeBSD has no delegation mechanism for jail
creation/removal — `PRIV_JAIL_SET`/`PRIV_JAIL_REMOVE` are unconditional
superuser checks. The cross-compiled test binary is therefore run as
root directly on the VM (`su -m root -c '/path/to/test.bin -test.v'`),
rather than via a delegated unprivileged user the way `internal/zfs`'s
tests run. This matches how the package will actually run in
production too: whatever process manages jails needs to run as root (or
with the specific jail privileges granted via `login.conf`/similar) —
there's no unprivileged mode to design toward here.

### `jls -n <specific params>` over parsing full `jls -n` output

Requesting only the parameters actually needed (`name`, `path`,
`host.hostname`, `jid`) rather than parsing `jls`'s full per-jail
parameter dump keeps `parseKeyValues` a plain space-split — safe because
none of Apiary's own controlled inputs (validated jail names, caller-
provided paths/hostnames) can contain spaces. This does not attempt to
handle `jls`'s quoted-string escaping for arbitrary parameters; if a
future caller needs a parameter whose value might contain a space,
`parseKeyValues` will need real quote-aware parsing first.

## Consequences

- A `Manager` constructed with `Prefix: ""` would list every jail on the
  host as if Apiary owned it — always construct with a real, specific
  prefix. Consider validating a non-empty `Prefix` in `New` if this ever
  becomes a real footgun in practice.
- Dedicated IP allocation/VNET jails, when they're needed, will likely
  want to reuse the ephemeral-state/node-ownership machinery already
  built for `VMDefinition` (an IP is exactly the kind of small,
  JSON-shaped fact raft already replicates) rather than being purely
  local jail configuration.
