# ADR-0104: Local service endpoint configuration

## Status

Accepted

## Context

Apiary's Machine Configuration page showed service state but did not show
the configured `address:port` at which each local daemon listened. The
configuration was also uneven: frontend and restshimd accepted free-form
bind addresses in their existing panels, while managerd's `rpc_addr` was
file-only despite being needed when Combs must reach each other directly.

The existing safety boundaries remain important. A managerd restart can
temporarily remove a Raft voter, and a raftd bind-address change is part of
Raft topology, not merely a local web-service setting. Two independently
bootstrapped Combs also cannot be safely merged by simply adding a voter.

## Decision

The Machine page displays the configured bind endpoint for all four local
Apiary services: managerd, frontend, restshimd, and raftd.

`GetNodeConfig` now returns managerd's configured `rpc_addr` for display.
A narrow Admin-only `UpdateManagerdBindAddress` RPC changes only that one
field. It accepts an explicit port plus either a wildcard/loopback address
or a numeric address currently present in this Comb's interface inventory.
It does not restart managerd. The operator must use the existing
guardrail-protected `apiary_managerd` restart action after saving, so a
potentially disruptive step remains visible and separately authorized.

The page supplies local `address:port` suggestions derived from the same
host-local interface inventory already used for uplink selection. The
frontend and restshimd bind inputs retain their existing custom-value
capability while gaining those suggestions.

Raftd remains read-only. Its bind address may only be changed through a
separate, future membership-preparation workflow that can establish an
empty joiner state and require an explicit destructive acknowledgement when
resetting an independently bootstrapped node. This change must not pretend
that changing a displayed endpoint joins two existing Colonies.

## Consequences

- Operators can inspect local endpoints without opening configuration files.
- A managerd endpoint can be prepared in the web UI, but it remains inert
  until the independently guarded restart action occurs.
- The server independently validates a submitted managerd endpoint rather
  than trusting browser-provided suggestions.
- Raft topology and standalone-to-joiner conversion remain outside this
  local endpoint feature.

## Verification

Focused unit tests cover managerd endpoint persistence, preservation of
unrelated identity/socket settings, rejection of a remote address, endpoint
display, and the frontend request path. The normal repository-wide package
tests also contain socket-listening integration tests that require a host
where local TCP listeners are permitted.

## Amendment (2026-09-26): the address this ADR validates is not the address that works

The Decision section above is left exactly as written and as correct as it
was when written. This section records a conflict between it and a later
decision, rather than editing the original away.

`validateLocalBindAddress` accepts three shapes: a wildcard, loopback, or
a numeric address currently assigned to one of this Comb's interfaces. It
rejects a hostname, with `rpc_addr host %q must be a numeric address
assigned to this Comb`. ADR-0139 establishes that the per-node DNS
hostname is the only value that works for `rpc_addr` in a real Colony,
because `cmd/raftd/confirm.go` (ADR-0125) uses the same string as a local
dial target and leaves its TLS `serverName` empty — so the dialed host has
to be a SAN on the serving certificate, and this project's certificates
carry `IP:127.0.0.1` and `DNS:<node>` and no LAN-address SAN.

So the validator's "numeric address assigned to this Comb" clause, and
even its explicit retention of wildcard, both accept values that cannot
function as a dial target, while rejecting the one value that can. That
is not an oversight in the original reasoning — at the time, wildcard and
loopback genuinely were the established single-node configurations, as
the comment above says — but the premise no longer holds once ADR-0125
added the second role and ADR-0139 settled the certificate constraint.

**Status: resolved by the change that accompanies this amendment**, on the
same reasoning ADR-0139 records. `validateLocalBindAddress` now accepts a
DNS hostname, skipping the interface-inventory clause for a name (the
inventory holds only numeric addresses, so there is nothing in it to match
a name against, and resolving the name here would make a legitimate save
depend on DNS being up at that instant). A numeric non-loopback address
still goes through the inventory check unchanged — that clause is a real
anti-typo guard and is not weakened. The Machine page's `HostOptions` in
`internal/frontend/convert.go` now seeds this Comb's own names, which are
the values that verify. The wildcard remains a legal bind there; what
changed is that it is no longer *the* documented default, and
`internal/addrpolicy` now rejects it outright in the fields that dial.

Note this amendment is documentation of a decision, not of a UI redesign.
The broader `UpdateNodeConfig` Machine Configuration panel never carried
`rpc_addr` at all (ADR-0100), so there is no second place that needs the
same fix.
