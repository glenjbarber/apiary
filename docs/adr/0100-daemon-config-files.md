# ADR-0100: Move CLI flags to per-daemon JSON config files

## Status

Accepted

## Context

All four daemons (`managerd`, `frontend`, `restshimd`, `raftd`) took
their configuration almost entirely as CLI flags, passed via
`/etc/rc.conf`'s `apiary_<name>_args` shell strings on each host. This
was unwieldy to audit and inconsistent across daemons - `managerd`
already had a partial JSON override layer (`internal/nodeconfig`) atop
its flags, the other three had none at all. It also meant every flag
value, including sensitive paths and (for `raftd`'s `-internal-token`)
a literal secret, sat in plain `ps(1)`/`procstat(1)` output for as
long as the process ran - the exact class of exposure `-peer-api-key-file`
(ADR-0096) and `-cloudflare-token-file` had each separately worked
around, one flag at a time, without ever generalizing the fix.

The user asked for CLI flags to be eliminated almost entirely, moving
configuration into a file instead.

## Decision

Each daemon reads its own JSON config file at a fixed, well-known path
under `/usr/local/etc/apiary/` - `managerd.json`, `frontend.json`,
`restshimd.json`, `raftd.json` - with no `-config <path>` flag; the
path is hardcoded. `/usr/local/etc/apiary/` was already the
established convention on both live hosts (`frontend.env` and the
TLS cert/key relocation both already lived there).

`managerd` absorbs every remaining flag, including the three
identity/wiring fields (`node_id`, `rpc_addr`, `raftd_socket`) its own
`internal/nodeconfig` package previously excluded by design - but
those three stay out of the live web UI (`GetNodeConfig`/
`UpdateNodeConfig`): loadable from the file, never RPC-editable. The
caution is relocated, not removed - raft identity and internal wiring
still can't be changed through a running instance, just from a
different starting point (CLI-only -> file-only-not-RPC-editable).

`frontend`, `restshimd`, and `raftd` get their own new, small packages
(`internal/frontendconfig`, `internal/restshimdconfig`,
`internal/raftdconfig`) mirroring `internal/loginconfig`'s shape - no
`Save()`, no write-once/secret-write-only RPC semantics, hand-edited
only. This is deliberately less machinery than `internal/nodeconfig`:
none of the three have a web UI to expose live editing through in this
pass, so building that layer now would be speculative. A future ADR
can add it if a real need arises.

## What stays CLI-only, and why

`managerd`'s `-reset-managed`/`-factory-reset`/`-factory-reset-extra-jails`/
`-factory-reset-extra-datasets`/`-export-host-config`, and `raftd`'s
`-reset`/`-restore`/`-restore-file`/`-restore-dry-run`/`-export`, all
stay literal CLI flags, never fields on any config struct. Every
daemon here runs under `daemon(8)`'s `-r` auto-restart supervisor - a
value accidentally left in a config file would act on **every single
respawn**, not once. A CLI arg typed by hand for one invocation has no
such risk. This reasoning already existed in the flags' own doc
comments before this ADR; it's restated here because it's the single
most safety-critical invariant this whole redesign depends on, and
should be impossible for a future contributor to accidentally undo
without first reading why.

A real structural asymmetry follows from this: `managerd`'s
`-reset-managed`/`-factory-reset` and `raftd`'s `-reset`/`-export`/
`-restore` all need values that now live in the config file (a data
directory, a socket path, a token) to know what to act on - unlike
`managerd`'s `-export-host-config`, which needs nothing. So the config
file must load *before* branching on these flags, not after. To keep
these "break glass" recovery tools usable even when the config file
itself is broken - exactly the situation an operator most needs them
for - a `Load()` error on this path falls back to hardcoded defaults
with a warning, rather than aborting. Normal server startup is
stricter: a malformed config file there is a fatal error, since there
is no flag default left to silently fall back to for most settings,
and silently reverting to defaults on every respawn would be far
harder to debug than a clear startup failure.

## The `UpdateNodeConfig` merge fix this required

`internal/manager/server.go`'s `UpdateNodeConfig` builds its
`nodeconfig.Config` literal entirely fresh from the incoming RPC
request rather than merging onto the currently-saved config - every
field, before this ADR, had a 1:1 proto counterpart, so this was never
a bug. `NodeID`/`RPCAddr`/`RaftdSocket` are the first fields with **no**
proto counterpart at all (by design - kept off the RPC surface). Left
out of that literal, they would have been silently zeroed on *every*
`UpdateNodeConfig` call, not just ones touching identity - saving an
unrelated Cloudflare setting through the Machine Configuration page
would have wiped `raftd_socket`. Fixed by explicitly carrying these
three fields over from the currently-saved config before applying the
request's own fields. A regression test
(`TestServer_UpdateNodeConfig_PreservesIdentityFieldsAcrossUnrelatedUpdate`)
proves this: it fails without the explicit carry-over and passes with
it.

## File permissions: warn, don't block

`raftd.json` (`internal_token`) and `frontend.json`
(`manager_api_key`) hold real secrets but, unlike `managerd.json`,
have no `Save()` to enforce `0600` the way `nodeconfig.Manager.Save`
already does - these three are hand-edited. Each package's `Load()`
warns (to stderr, matching this codebase's existing best-effort
background-error convention) when the file is readable by group/other
and holds a secret field, but never refuses to start over it. A hard
startup failure over a permissions nit on a system where restarts must
not cause downtime would itself be a self-inflicted outage risk.
`restshimd.json` has no secrets - no check at all.

## rc.d scripts and the frontend envfile

No `etc/rc.d/apiary_*` script needed structural changes - each already
interpolates `${apiary_<name>_args}` (empty-string-safe) into
`command_args`, so once every daemon reads its own fixed config path
unconditionally, that variable in `/etc/rc.conf` simply becomes
unused. The scripts themselves are kept (not removed) for the FreeBSD
port. The one exception: `etc/rc.d/apiary_frontend`'s
`apiary_frontend_envfile`/`apiary_frontend_loadenv` mechanism (which
sourced `/usr/local/etc/apiary/frontend.env` for
`APIARY_MANAGER_API_KEY`) is retired - that value now lives in
`frontend.json`'s `manager_api_key` field directly.

## Migration

Live migration on `apiverse`/`apiarium` happens per-daemon, one host
at a time, in `rc.d` dependency order (`raftd` -> `managerd` ->
`restshimd` -> `frontend`): hand-construct the new JSON file from that
host's *current* `apiary_<name>_args` string (1:1 field mapping),
`chmod 600`, deploy the new binary, clear the old `_args` string,
restart just that daemon, verify, then repeat on the second host
before moving to the next daemon. Deploying a new binary before its
config file exists at the new path would be a breaking no-op - the
binary would silently fall back to hardcoded defaults, diverging
immediately from the live-working config - so the file must exist with
correct values before or atomically with each binary swap, never
after.

## Consequences

- Every daemon's steady-state configuration now lives in one
  inspectable file instead of shell-quoted argv, and no longer leaks
  into `ps(1)` output.
- Only `managerd` gets live, RPC-backed editing in this pass. Adding
  the same for `frontend`/`restshimd`/`raftd` is a disclosed,
  not-yet-decided follow-up, not an oversight.
- `-peer-api-key-file` (ADR-0096) is retired: its only purpose was
  avoiding `-peer-api-key`'s argv visibility, which is moot once
  nothing is passed via argv at all. `nodeconfig.Config.PeerAPIKey`
  now holds the literal secret directly, protected by the file's own
  permissions instead of a path indirection.
- `nodeconfig.DefaultPath` moves from `/var/db/apiary/node-config.json`
  to `/usr/local/etc/apiary/managerd.json` - a hard rename with no
  dual-path fallback-read in code; the migration procedure above
  handles the transition operationally, once per host.
