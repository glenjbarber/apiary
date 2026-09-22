# ADR-0115: Automatic derivation of peer_tls_hostname_map

## Status

Accepted

## Context

buzz's Create Jail and Jails-list pages failed with a bare
"raft: this node is not the leader" despite a stable elected leader
(SHARED.md, 2026-09-21 23:15 EDT and 23:55 EDT). The real cause:
`raft.LeaderWithID()` (internal/raft/node.go's `Node.LeaderHint`) reports
the leader's address as a resolved IP (e.g. `10.90.0.94:17600`), never
the hostname the peer was actually configured with. `internal/manager`'s
leader-forwarding path (`PeerReporter.dial`/`dialOpts`, in
internal/manager/peer.go) builds its dial target directly from that IP.
Each Comb's TLS certificate is only valid for its own hostname plus
`127.0.0.1` (ADR-0033), never its LAN IP, so dialing by IP fails TLS
ServerName verification unless something overrides `tls.Config.ServerName`
for that IP.

`peer_tls_hostname_map` (a comma-separated `ip=hostname` list, parsed once
at startup in cmd/managerd/main.go into a `map[string]string` and passed
into `manager.NewPeerReporter`) already exists for exactly this override.
`PeerReporter.dial`/`dialOpts`/`dialRestartGuardrail` all consult it: if
the IP being dialed has an entry, `tls.Config.ServerName` is set to the
mapped hostname before the handshake. This was the actual root cause of
the live incident - the map was empty on buzz - and setting it by hand is
unmentioned in docs/add-node-to-colony.md's guided join flow until an
operator hits this failure and works backward to it.

### What this codebase already tracks about raft membership

`internal/raft.Node.Status()` (internal/raft/node.go) calls
`raft.GetConfiguration()` and returns every server's `ID` and `Address` as
`ServerInfo{ID, Address, Suffrage}`. This is already exposed outside
`internal/raft`: `internal/manager.RaftClient.Status` (raftclient.go) calls
raftd's internal `Status` RPC and gets back `internalpb.StatusResponse`
with a `Servers` list carrying the same `Id`/`Address`/`Suffrage` fields
over the wire (internal/raft/server.go's `Server.Status` does the
conversion). cmd/managerd/main.go already dials raftd and calls
`raftClient.Status(ctx)` once at startup (to read `raftStatus.GetNodeId()`)
- the exact same call this ADR's refresh loop reuses.

`Address` is whatever was passed to `raft.AddVoter`/`BootstrapCluster` as
`raft_bind_address` (internal/manager/joincolony.go's
`ApproveJoinRequest`/`UpdateVoterAddress`, both call
`RaftClient.AddVoter` with `pending.GetRaftBindAddress()`). Per
docs/bootstrap.md's own raftd.json.sample guidance
(`"raft_bind": "<this-host-address>:17600"`) and
docs/add-node-to-colony.md's join flow, an operator is expected to set
this to the node's real, resolvable address - in every real deployment
example in this repo's own docs and SHARED.md, that address is the node's
DNS hostname (e.g. `brood.lab3.home.arpa`), matching the hostname its TLS
certificate is issued for (ADR-0033). `ID` is `raft.ServerID`, set from
raftd's `-node-id` flag which defaults to `os.Hostname()`
(cmd/raftd/main.go) - also conventionally the same hostname, though this
ADR derives from `Address`, not `ID`, since `Address` is what actually
carries the raft-bind host:port a peer is dialed at.

This means raft's own committed configuration - already fetched by every
managerd via its local raftd, no new RPC or cross-node call needed - is a
reliable source for "every known voter's raft-bind hostname." There is no
separate nodeconfig/commonconfig-tracked peer list in this codebase to
fall back to (internal/commonconfig, ADR-0112, only holds this node's own
`NodeID`/`Hostname`, not a roster of other nodes) - raft's own
configuration is in fact the *only* place a managerd already knows every
other voter's identity, so the fallback described in this ADR's tasking
("derive from nodeconfig's own peer list instead") does not apply here;
raft membership is not just feasible, it is the only real option.

## Decision

`internal/manager.PeerReporter` gains a `RefreshDerivedHostnames(servers
[]KnownRaftServer)` method (`KnownRaftServer` is a local
`{Address string}` shape, not `internalpb.ServerInfo`/`raft.ServerInfo`,
so `internal/manager` doesn't need to import either just for this). For
each server, it splits `Address` into host and port; if the host is
already a bare IP (`net.ParseIP`), there is nothing to derive and the
entry is skipped; otherwise it resolves the host via `PeerReporter
.ResolveFunc` (defaults to `net.LookupHost`, overridable per-instance so
tests never touch real DNS) and records `ip -> host` for every IP
returned. A host that fails to resolve is skipped, not fatal - the whole
refresh does not abort over one bad peer. The rebuilt map fully replaces
the previous one on every refresh (guarded by a `sync.RWMutex`), so a
voter removed from raft's configuration, or renamed, stops being derived
on the very next refresh rather than leaking a stale mapping.

**Precedence**: `PeerReporter.serverNameFor(ip)` checks the manual
`PeerHostnames` map first and only falls through to the derived map if the
IP has no manual entry. This mirrors ADR-0112's own precedent exactly (a
value explicitly set in the service file "always wins" over the
auto-filled one): an operator who hand-configured
`peer_tls_hostname_map` did so for a reason - unreliable DNS, a
certificate hostname that doesn't match the raft-bind hostname - and
automatic derivation must never silently out-vote that choice.
`dial`/`dialOpts`/`dialRestartGuardrail` all now call `serverNameFor`
instead of indexing `PeerHostnames` directly, so every dial path gets the
override uniformly.

**Refresh timing**: a background goroutine, not resolve-per-dial.
`PeerReporter.dial` is deliberately a fresh dial per call already (see its
own doc comment - simplicity over an optimization nothing needs), but a
DNS lookup on every forwarded RPC would add latency to the
already-latency-sensitive leader-forwarding path (ADR-0029/ADR-0035) for
no benefit, since raft membership changes are rare and operator-driven.
cmd/managerd/main.go instead starts `runPeerHostnameRefreshLoop`
alongside its three existing tick loops (`runReconcileLoop`,
`runAssumptionCheckLoop`, `runOriginCARenewalLoop`), following their exact
shape: an immediate first run, then one per tick, until context
cancellation, with failures logged and not fatal. Each tick calls the
same `raftClient.Status(ctx)` managerd already calls once at startup and
feeds the resulting `Servers` into `RefreshDerivedHostnames`. The interval
is a fixed 30-second constant (`peerHostnameRefreshInterval`), not a new
config field: this codebase has no existing "raft membership changed"
signal cheap enough to hook into without adding one (raft's own
`ConfigurationFuture`/observer mechanisms are not currently plumbed out of
internal/raft), and a fixed short interval is simpler than adding one for
an event this infrequent. A raftd query failure just leaves the
previously-derived map in place until the next successful tick.

`peer_tls_hostname_map` (nodeconfig.Config.PeerTLSHostnameMap,
managerd.json/frontend.json) is unchanged and fully supported - it
remains the explicit override path for exotic networks where automatic
derivation cannot be trusted.

## Out of scope

- frontend's own peer-forwarding path (internal/frontend, ADR-0093's
  cluster-overview dialing) is not wired to the new derivation in this
  change - it has no local raftd to query `Status` from the way managerd
  does, and no equivalent "known cluster membership" source. It keeps
  relying on `peer_tls_hostname_map` alone. Extending automatic
  derivation to frontend would need its own design (likely: ask its local
  managerd for the same raft `Servers` list over the existing RPC
  connection) and is a reasonable follow-up, not bundled in here.
- No new raft membership-change signal is added to internal/raft; see
  Refresh timing above.
- No proto/wire changes: `peer_tls_hostname_map` stays a plain config
  string end to end (api/rpc/manager.proto's `peer_tls_hostname_map`
  field, internal/frontend/machine.go's settings-page plumbing) - this
  change is purely runtime behavior inside `PeerReporter`.

## Consequences

- A freshly-joined Comb, or a Comb whose raft-bind hostnames all resolve
  normally, needs zero manual `peer_tls_hostname_map` configuration for
  leader-forwarding to work over TLS - closing the exact gap that caused
  the live incident on buzz.
- `peer_tls_hostname_map` keeps its full meaning as an explicit,
  higher-precedence override - no regression for a deployment already
  relying on it (e.g. a certificate hostname that differs from the
  raft-bind hostname, or DNS that only resolves from some hosts).
- A managerd whose local raftd is unreachable at startup, or whose raft
  cluster has no voters with hostnames (all bare IPs), simply derives
  nothing and behaves exactly as before this ADR - no new failure mode.
- Up to `peerHostnameRefreshInterval` (30s) of staleness after a raft
  membership change (a new voter joining, an address changing) before the
  derived map reflects it; the manual override remains available for
  anyone who cannot tolerate that window.
- One new background goroutine and one new periodic `RaftClient.Status`
  call per managerd - negligible load next to the existing reconcile/
  assumption-check/origin-CA-renewal loops already running at similar
  cadences.
- docs/add-node-to-colony.md's join flow no longer instructs operators to
  set `peer_tls_hostname_map` for the normal case, reducing one more
  manual step (and one more way to forget it, as buzz did) from joining a
  new Comb to a healthy Colony.

## Verification

- New tests in internal/manager/peer_test.go:
  `TestPeerReporter_RefreshDerivedHostnames_BasicDerivation` (a resolvable
  raft-bind hostname derives every IP it resolves to),
  `TestPeerReporter_RefreshDerivedHostnames_SkipsBareIPAddress` (a
  raft-bind address that is already an IP is never passed to
  `ResolveFunc`), `TestPeerReporter_RefreshDerivedHostnames_SkipsUnresolvableHostGracefully`
  (one peer failing to resolve does not stop a sibling peer from being
  derived), `TestPeerReporter_ManualHostnameWinsOverDerived` (a real TLS
  handshake against a certificate that only the manual entry's hostname
  matches, proving precedence end to end, not just by inspecting the
  map), and `TestPeerReporter_DerivedHostname_MakesTLSDialSucceed` (a real
  TLS handshake succeeding with *no* manual `peer_tls_hostname_map` entry
  at all, purely from automatic derivation).
- An ADR-0109-style two-real-raft-node integration test (a freshly-joined
  node with no manual `peer_tls_hostname_map` forwarding a leader RPC
  reported by IP) was not added in this change - internal/manager's
  existing integration tests (internal/manager/integration_test.go) build
  their two-node fixtures around real `internal/raft.Node` pairs wired
  directly to `internal/manager.Server`, and reworking one to also start
  real managerd-level TLS listeners with real certificates for both nodes
  was a larger lift than the unit-level TLS tests above, which already
  exercise the real handshake path end to end. Left as a reasonable
  follow-up if a regression here ever needs a more end-to-end guard.
- `go build ./...`, `go vet ./...`, `gofmt -l .`, and the full
  `go test ./...` all pass.
