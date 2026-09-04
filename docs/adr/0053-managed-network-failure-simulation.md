# ADR-0053: Dependency Graph Simulator v2 managed-network failure

## Status

Accepted

## Context

ADR-0052 introduced a deliberately narrow, read-only answer to "what happens
if this node disappears?" The broader simulator direction also calls for
network scenarios. Apiary already has enough replicated intent to answer one
useful question honestly: which VMs declare an attachment to a selected managed
network. It does not yet model guest routes, extra interfaces, service
dependencies, or current packet flow.

## Decision

Add Viewer-readable `ManagerService.SimulateNetworkFailure`. It reads the
managed-network and VM lists from the raft leader, rejects an unknown
`network_id`, and returns the selected network plus every VM whose declared
`network_id` matches it. The entire request is forwarded when either read
returns a leader hint, preventing a report assembled from different nodes' FSM
views. The two reads remain sequential and are explicitly not an atomic
snapshot.

The pure computation belongs in `internal/cluster`, uses plain Go types, and
sorts affected Cells by ID. The existing `/simulate` page gains a separate,
bookmarkable managed-network form and report. Product copy says that declared
network connectivity would be unavailable, but never claims that the Cell
process or storage stops. Jails are absent because Apiary jails still use
`ip4=inherit` and have no managed-network attachment in replicated state.

## Consequences

- Operators can identify the known Cell blast radius of a managed-network loss
  without changing infrastructure.
- A network with no attached Cells produces an explicit empty report, while an
  unknown network produces an error.
- The result is configuration impact, not observed outage proof. It cannot say
  which services fail, whether an alternate guest path exists, or whether the
  network is currently passing traffic.
- Uplink loss, shared physical failure domains, packet-path diagnosis, and
  active probes remain separate future slices.
