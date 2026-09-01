# ADR-0037: write RPC forwarding to the raft leader

## Status

Accepted

## Context

ADR-0035 forwards leader-only *reads* (`ListVMs`/`GetVM`/`ListJails`/`GetJail`/`ListNetworks`) from a non-leader node to the leader's own `managerd`, so the web UI or a REST caller pointed at any node still sees a correct view of the cluster. Every *write* RPC (`CreateVM`/`UpdateVM`/`DeleteVM`, the jail/network/API-key equivalents) has always gone through raft's own `Apply`, which only ever succeeds on the leader - a non-leader's rejection (`raft: this node is not the leader`) was simply returned to the caller as-is, with no forwarding at all. This surfaced directly: a real user hit "Create virtual machine" on the web UI against a node that happened not to be the current leader and got that raw raft error in the form's error banner, with no way to tell what to do about it short of knowing which node currently held leadership.

## Design decisions

- **Mirrors ADR-0035 exactly, extended to writes.** The existing `PeerForwarder` interface (satisfied by `*manager.PeerReporter`) gains `CreateVM`/`UpdateVM`/`DeleteVM`/`CreateJail`/`UpdateJail`/`DeleteJail`/`CreateNetwork`/`DeleteNetwork`/`CreateAPIKey`/`RevokeAPIKey` - no new peer-dialing machinery, no new flags. The same `-peer-api-key`/`-peer-tls`/`-peer-tls-hostname-map` configuration ADR-0029/ADR-0035 already require covers this too.
- **Forward the original request, return the peer's real response.** Unlike ADR-0029's fire-and-forget `Report*` forwarding (an internal ack with no payload the caller needs back), a forwarded `CreateVM` must hand back the leader's actual response - the caller needs the created record (e.g. its assigned `ip_address`, or the web UI's redirect target). Each handler's own local `apply*Command` result is discarded in favor of the peer's when a forward succeeds, exactly like ADR-0035's read handlers already discard the local (rejected) response.
- **`CreateAPIKey` is the one wrinkle.** It generates a raw/hashed key pair locally *before* attempting the local `Apply`, since it needs the hash ready to submit. If that local attempt is rejected for not being the leader, forwarding sends the caller's *original* `CreateAPIKeyRequest` (name/role/timeout) - the locally generated raw/hashed pair is simply discarded, never transmitted anywhere. The leader generates its own fresh key from the forwarded request, exactly as if the caller had reached it directly. No key material ever crosses nodes.
- **Only a leader-hint rejection forwards.** Every handler's existing `if leaderHint != "" && s.peers != nil` check (the exact same condition ADR-0035's reads already use) means a validation error, a marshal failure, or any other non-leader-hint rejection is returned to the caller unchanged - forwarding only kicks in for the one specific "you asked the wrong node" case it exists to solve.
- **`ForcePurgeVM`/`ForcePurgeJail`/`MigrateVM`/`MigrateJail` are a named, deliberate gap, not an oversight.** Their own preliminary `GetVM`/`GetJail` calls go through `s.raft.GetVM`/`s.raft.GetJail` directly (the internal, leader-only-by-design FSM read - ADR-0009), not through the newly-forwarding external `GetVM`/`GetJail` RPC handlers, so a non-leader node still fails these four operations outright before ever reaching an `Apply` this ADR's forwarding could catch. Closing this would mean either forwarding inside `RaftClient` itself (a deeper, more invasive change than this pass intends) or duplicating each of these four handlers' logic on the peer side. Left as future work, following this project's own established practice of naming a real limitation rather than silently leaving it.

## Consequences

- A web UI or REST client pointed at any cluster node can now create/update/delete a VM, jail, network, or API key regardless of which node currently holds raft leadership - closing the gap a real user hit directly.
- Full unit coverage of the new `PeerReporter` forwarding methods (request pass-through, response pass-through), mirroring `peer_test.go`'s existing `ListVMs`/`GetVM`/etc. tests exactly.
- Same disclosed test gap ADR-0035 already named: no integration test exercises a genuine raft leader-rejection end-to-end, since `Server.raft` is a concrete `*RaftClient`, not an interface a fake can substitute for the "this node isn't the leader" case. Covered by live verification instead (a real non-leader `CreateVM` correctly reaching the leader's managerd and returning its real response).
- Not addressed: the `ForcePurgeVM`/`ForcePurgeJail`/`MigrateVM`/`MigrateJail` gap named above.
