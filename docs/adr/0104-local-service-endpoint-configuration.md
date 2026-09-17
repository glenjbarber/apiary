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
