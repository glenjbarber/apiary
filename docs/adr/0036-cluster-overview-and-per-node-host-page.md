# ADR-0036: cluster overview and per-node host page

## Status

Accepted

## Context

The default landing page ("/") only ever showed `HostStats` for whichever node's `managerd` the running `frontend` happened to be colocated with - there was no way to see any other cluster member's health without pointing a separate `frontend` instance at it. Requested directly: a "pagified" default host page, showing basic status across the whole cluster by default, with a more detailed view available per node.

`HostStats` itself has no leader/forwarding concept to lean on (unlike `ListVMs`/`GetVM`/etc., ADR-0035) - it always answers only for whoever receives the call, by design (it's physical, per-node data, the same reasoning ISOs/consoles/serial logs already follow). Reaching another node's stats means dialing that node's `managerd` directly.

## Design decisions

- **Reuses `internal/manager.PeerReporter` rather than inventing a second peer-dialing mechanism.** `HostStats(ctx, addr)` was added to it exactly like `ListVMs`/`GetVM`/etc. were for ADR-0035 - same TLS/API-key/dial machinery, one more forwarded RPC.
- **Addressing is by real hostname, not IP.** ADR-0035's peer forwarding derives a peer's address from a raft `leader_hint` (always a bare IP), needing an IP-to-hostname map for TLS verification. Here there's no such hint - `cmd/frontend` is given a node ID (from `known_node_ids`) and turns it into an address itself, via new `-peer-hostname-suffix`/`-peer-manager-port` flags (e.g. node ID `freebsd-apiary` + suffix `.apiary.work` dials `freebsd-apiary.apiary.work:17700`). Since the dialed address *is* the real hostname the peer's certificate names, no `ServerName` override is needed at all - default TLS verification works unmodified.
- **Two pages, not one with a toggle.** `/` is a lightweight, basic-status table (reachable, CPU load, memory, ZFS pool health, pf enabled) - one row per known node, fetched *concurrently* so one unreachable node doesn't hold up the rest. `/host/{id}` is the full, previously-existing verbose breakdown (CPU/Memory/ZFS Pools/Disks/Network/PF tables), now addressable per node instead of always the local one.
- **The local node never dials itself as a peer.** `nodeHostStats` compares the requested node ID against `Status`'s own `manager_node_id` and uses the already-configured `s.client` directly when they match (or when no peer client is configured at all) - avoids an unnecessary network round-trip and works correctly even with `-peer-hostname-suffix`/`-peer-manager-port` unset.
- **A node's own reachability failure doesn't fail the whole overview page.** Each row is fetched independently; an unreachable node just renders with a red "Unreachable" badge and its error message, matching this project's established fail-soft convention (`internal/hoststats.Snapshot`'s own best-effort-per-subsystem posture, `currentVMs`/`currentISOs`'s zero-value-not-error convention).

## Consequences

- New `cmd/frontend` flags: `-peer-tls`, `-peer-hostname-suffix`, `-peer-manager-port` (default `"17700"`). All opt-in; with none set, the overview page still works but only ever shows the local node reachable (every other known node reports "Unreachable" via a real, honest connection failure - not silently hidden).
- Reuses the same `APIARY_MANAGER_API_KEY` the frontend already uses for its own `managerd` - no second credential to configure, since `HostStats` only requires `RoleViewer`, well within what that key already grants.
- Full unit coverage: the new `PeerReporter.HostStats` method, the overview page's per-node fetch/summarize logic (including the unreachable-node and local-vs-peer dispatch cases), and the per-node host page against both the local client and a fake peer.
- Not addressed: automatic discovery of a node's real hostname/TLS setup - `-peer-hostname-suffix`/`-peer-manager-port` assume every node follows the same convention (true for this project's own four machines, not a general guarantee). A node with a differently-shaped address needs a real per-node directory, which doesn't exist yet (the same gap ADR-0020/ADR-0022 already name for console/bridge-status reporting).
