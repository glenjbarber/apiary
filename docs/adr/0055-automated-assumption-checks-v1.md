# ADR-0055: Automated Assumption Checks v1

## Status

Accepted

## Context

`CODEX.md`'s "Future product directions," Priority 2, describes a fuller
**Assumption Register**: persisted claims with an owner, an explicit
scope, supporting-evidence citations, expiration/verification-method
metadata, and linkage to the Dependency Graph Simulator (ADR-0052) so a
simulated failure or a future "Flight Plan" can cite which assumptions it
relies on. **This ADR does not build that.** Per the user's own prior
scoping choice, v1 implements exactly three continuously re-evaluated,
system-observed checks, with real persisted history surviving a managerd
restart:

1. a peer managerd is reachable, and (separately) whether a call over
   this node's configured security path is accepted;
2. this node's configured NAT uplink actually owns the real default
   route;
3. a VM's HAST replica target actually has bhyve configured, and has the
   VM's required network's bridge up.

This is deliberately named **"Automated Assumption Checks v1,"** not
"Assumption Register," to be honest about the gap: no owner field, no
explicit scope, no evidence citation, no expiration/verification-method
selector, no Dependency-Graph linkage. The structured result identity
below is deliberately forward-compatible with the fuller register, but
this is a precursor, not the thing itself.

(Note: this ADR was originally drafted as ADR-0053, then ADR-0054 - both
numbers were claimed by other in-flight work (managed-network-failure
simulation, then image-availability-in-node-failure-simulation) merged
to `main` first each time. Renumbered to 0055 to avoid a second
collision, with no change to content.)

## Design review history

This design went through four review passes before implementation,
catching two real, would-have-shipped bugs and a series of substantive
gaps - summarized here because the reasoning behind several
non-obvious choices below only makes sense in light of what they fixed.

1. **Wrong raft-read variant.** An early draft read the VM list via the
   leader-only `RaftClient.ListVMs`, which would have silently produced
   zero replica-check results on every non-leader node forever, since
   this checker (unlike the simulator's request-scoped RPC) runs
   continuously on every node regardless of leadership. Fixed by using
   `ListVMsLocal` (the `internal/cluster.Reconciler`'s own established
   pattern - see its `raftClient` interface's doc comment for the exact
   same lesson learned there first).
2. **Wrong-Hive bridge status.** A draft that asked a replica target's
   own `ListNetworks` RPC for its bridge status would have silently
   returned the *raft leader's* bridge state instead, on any replica
   that happened not to be leader (`ListNetworks` is leader-only and
   forwards on rejection). Fixed by adding a genuinely local-only RPC,
   `GetLocalNetworkBridgeStatus`, built from `RaftClient.ListNetworksLocal`
   plus the existing local `VLANStatus` interface - it never forwards to
   a leader, by construction.
3. **A `stale` flag alongside an unchanged `status` field is not
   sufficient.** A consumer that reads only `status` (and eventually one
   will) would get a false signal from a stale `true`. Fixed by making
   the response carry both `observed_status` (raw, for diagnosis) and an
   **effective** `status` that the server itself collapses to
   `ASSUMPTION_STATUS_UNKNOWN` once stale - including a stored
   `NOT_APPLICABLE`, since applicability itself can silently change if
   the checker stops running. Safety lives in the data a naive consumer
   actually reads, not in a comment asking callers to check a second
   field first.
4. **`(Kind, Target)` is not a sufficient identity.** A VM and a jail can
   share a literal ID string; a bare string pair also conflates "which
   resource this is about" with "which node/network it depends on."
   Fixed with a structured `AssumptionKey{kind, subject_kind, subject_id,
   dependency_id, qualifier, observed_by_node_id}` - `subject_kind` is
   included now specifically so this identity schema doesn't need a
   breaking migration once real history has accumulated, even though
   v1's own check set never actually lets two different subject kinds
   collide under the same Kind.
5. **Heartbeat-throttled persistence conflated three different times.**
   An early persistence design only advanced `checked_at` on a
   transition or an hourly heartbeat, so a check that succeeded 5 seconds
   ago could still report an hour-old timestamp, and staleness was
   computed against that same throttled value - hiding exactly the
   discrepancy it should have caught. Fixed by splitting persistence into
   a **current snapshot** (one record per key, `last_observed_at` updated
   on *every* tick that actually checked it) and a **history journal**
   (transitions plus periodic heartbeats only, each with its own
   `recorded_at`). Freshness is always computed from the snapshot, never
   the journal.
6. **A corrupt store must never be silently replaced.** Treating an
   unparseable file as "missing, start fresh" means the very next write
   overwrites it, destroying whatever real history existed. Fixed: a file
   that fails to parse, or whose `schema_version` this build doesn't
   recognize, is renamed aside (`<path>.corrupt-<unixnano>`, `0600`)
   rather than overwritten; a persistent warning is exposed through
   `ListAssumptionResultsResponse` and rendered on `/assumptions`. This
   warning is recomputed on every `Load` (globbing for a prior
   quarantine file too), not a one-time in-memory latch - so it survives
   a managerd restart with no new corruption event, and self-clears once
   an operator removes the quarantined file.
7. **`ASSUMPTION_STATUS_NOT_APPLICABLE`.** "No NAT uplink configured on
   this node" is not uncertainty - the check does not apply here at all,
   and treating it as `Unknown` would make a perfectly healthy non-NAT
   node look permanently unverified. An earlier pass also applied this to
   a jail's replica-check results (for symmetry with the NAT case) and
   then reverted that: `bhyve`/`network_id` are fields that don't exist
   on `JailDefinition` at all, categorically different from "NAT uplink
   configurably absent" - a jail with a replica target produces **zero**
   assumption-(c) results, not `NotApplicable` and not silence-as-an-
   accident, but silence-as-the-honest-signal that these Kinds were never
   about jails.
8. **`Detail` must never be transition identity.** Free-text error
   strings change even when nothing meaningful does, which would
   reintroduce the exact write-amplification problem the heartbeat model
   exists to prevent. Every `Result` also carries a small, stable
   `ReasonCode`; a history entry is written on a `(Status, ReasonCode)`
   change, never on `Detail` alone.
9. **Peer-check semantics must not overclaim "valid credentials."** A
   successful RPC only proves a call completed against however a peer
   happens to be configured *right now* -
   `internal/manager/auth.go`'s own `checkAuth` accepts every call, key
   or not, until any API key has ever been created cluster-wide. Renamed
   the second peer Kind from an earlier "AUTH_VERIFIED" to
   `PEER_SECURITY_PATH_ACCEPTED`: it reports only whether a call over
   *this node's currently-configured* security path was accepted, never
   that the remote peer validated or enforces it.
10. **`GetLocalNetworkBridgeStatus`'s `error` field needed a defined,
    honest mapping.** An RPC-level error, or a network ID the local FSM
    doesn't yet recognize, may reflect replication lag rather than a
    genuinely absent bridge - mapped to `Unknown`, never `False`. Only an
    explicit `bridge_status == "down"` is ever a definitive false.

## Design

### Persistence (`internal/assumptions`)

Mirrors `internal/nodeconfig.Manager`'s shape (bare `Path`, `DefaultPath`,
missing-file-is-not-an-error) with upgrades justified by how differently
this package is used: `nodeconfig.Save` is triggered rarely, by a human
RPC; `assumptions.Manager.Append` is called by a background loop every
tick, concurrently with `Load` calls from `ListAssumptionResults`.
Written via a temp file in the same directory, `fsync`'d, renamed over
the real path, then the parent directory itself `fsync`'d - durable
against a crash mid-write, not just torn-read-safe. File mode `0600`; a
pre-existing file found wider than that sets the same storage-warning
mechanism as a corrupt file (without quarantining - the content is still
trusted). The on-disk format is `{"schema_version": 2, "snapshot": [...],
"history": [...]}`.

`Status` is four-valued (`true`/`false`/`unknown`/`not_applicable`) -
never collapsed to a boolean anywhere in this feature.
`ClampDetail` strips control characters, redacts text following common
credential markers ("bearer ", "authorization", "api-key"/"token"), and
truncates to 500 bytes - a persisted `Detail` string must never become an
unbounded, potentially credential-bearing diagnostic dump.

### Default-route introspection (`internal/netroute`)

Genuinely greenfield: no existing package shells `netstat -rn`/`route
get` anywhere. `DefaultRouteInterface` shells `route -n get default`;
`parseDefaultRouteOutput` is split out pure or unit-testing without
shelling out. **Disclosed limitation**: the exact output/exit behavior
when a node has no default route at all was not verified against a real
FreeBSD host during design (none was reachable from the planning
sandbox) - the parser defaults to `StatusUnknown` for any output that
doesn't unambiguously match the known "no route" signature, and should be
checked against `apiarium`/`apiverse`'s real output post-deploy.

### Orchestration (`internal/assumecheck`)

A sibling to `internal/cluster.Reconciler` in style (plain-Go dependency
injection, narrow local interfaces, nil-able fields), not part of that
package - it never touches ZFS/bhyve/jail provisioning.
`Checker.RunOnce` wraps the whole tick in a `RunDeadline` timeout,
attempts all three checks independently (a failure gathering one never
discards results already gathered from another), caches
`HostStats`/`GetLocalNetworkBridgeStatus` calls per tick by address (and,
for bridge checks, by `(address, network_id)`) so N VMs sharing one
replica target and network produce one call each, not N, and makes
exactly one `Store.Append` call at the end with whatever was produced.

`Checker.NodeID` must be the raft node ID (`raftStatus.GetNodeId()`),
never the `-node-id` flag's value - the two happen to default to the
same hostname today but are logically distinct, and using the wrong one
would silently exclude this node from its own peer list and match zero
VMs for the replica check. `Checker.Uplink` is `reconciler.Uplink`
passed through verbatim (read after `cmd/managerd`'s nodeconfig-override
application and nat-uplink-falls-back-to-vlan-uplink resolution) - never
re-derived independently, which would risk a second, driftable copy of
that logic.

### Proto additions (`api/rpc/manager.proto`)

- `HostStatsResponse.bhyve_configured` - `= s.vnc != nil`, named for
  exactly what's observed (managerd was started with `-bhyve-bootrom`
  set), not the stronger claim that bhyve is currently usable.
- `GetLocalNetworkBridgeStatus` RPC - local-only, `RoleViewer`, never
  forwards.
- `AssumptionKind` (`PEER_MANAGER_RPC_SUCCEEDED`,
  `PEER_SECURITY_PATH_ACCEPTED`, `NAT_UPLINK_DEFAULT_ROUTE`,
  `REPLICA_BHYVE_CONFIGURED`, `REPLICA_NETWORK_BRIDGE_UP`),
  `AssumptionStatus` (adding `NOT_APPLICABLE`), `AssumptionSubjectKind`
  (`NODE`/`VM`/`JAIL`), `AssumptionKey`, `AssumptionResult` (snapshot
  view: `observed_status` + effective `status` + `stale`),
  `AssumptionHistoryEntry` (journal view: no staleness/effective-status
  split, since it describes a specific past moment).
- `ListAssumptionResults` RPC - local-only, `RoleViewer`, `filter` is
  `optional AssumptionKey` (proto3 presence-tracked, so "no filter" and
  "a filter whose fields happen to be empty" are distinguishable);
  `storage_degraded`/`storage_degraded_detail` surface the persistence
  layer's own warning.

### Wiring

`cmd/managerd` gains `-assumption-check-interval` (default 60s),
`-assumption-heartbeat-interval` (default 1h, validated `>=`
check-interval), `-assumption-stale-after` (default `3 *
check-interval` if zero, validated `>= 2x` check-interval - since
`last_observed_at` now advances every tick, this is based on the check
interval, not the heartbeat interval), `-assumption-run-deadline`
(default 20s, validated `<` check-interval), `-assumption-history-limit`
(default 200/key), `-assumption-history-max-age` (default 30 days) - all
validated at startup, refusing to start on a nonsensical combination.
`runAssumptionCheckLoop` mirrors `runReconcileLoop`'s exact
immediate-first-run-then-ticker shape. `manager.NewServer` gained two
trailing parameters (an `assumptionStore` interface and the stale-after
duration) specifically so the ~15 existing positional call sites needed
only a mechanical one-line edit each.

### Frontend

New `/assumptions` page reuses ADR-0036's per-node fan-out pattern
exactly (`sync.WaitGroup` + `Status().KnownNodeIds` + per-node goroutine
+ local-client/peer-dial split). Shows `latest` per node, colored
true/false/unknown/not-applicable each with a distinct badge treatment,
plus a distinct `stale` marker - `status` is already the safe value to
render, `stale` is purely informational. A `storage_degraded` node shows
a banner naming the detail. History drill-down is explicitly deferred,
not promised.

## Consequences

- An operator gets a self-service, persisted answer to three concrete
  "is my assumption about this environment still true?" questions,
  without needing to manually correlate `HostStats`, `route(8)`, and
  `hastctl status` output by hand across hosts.
- This is **not** the Assumption Register `CODEX.md` describes - no
  owner, scope, evidence citation, expiration, or Dependency-Graph
  linkage. A future slice could build on the structured `AssumptionKey`
  here without a breaking migration, but would still need real new work
  for those properties.
- **Retirement/lifecycle for a snapshot key whose subject no longer
  exists** (a deleted VM, a removed raft peer, a changed replica target)
  has no lifecycle in v1 - explicitly deferred, not solved. This is
  acceptable because of fix #3 above: a retired key can never be mistaken
  for a currently-true/false claim, since it ages past
  `-assumption-stale-after` and collapses to effective `UNKNOWN` like
  anything else the checker has stopped reporting on - it just isn't
  hidden or cleaned up from the file until a later pass adds real
  retirement handling.
- A real database (SQLite or similar) was considered for the persistence
  layer and deliberately not adopted: this project has zero database
  dependencies anywhere in its stack today, and its demonstrated real
  scale (2-3 physical nodes, tens of VMs, not thousands) doesn't yet
  justify the operational cost of a new storage engine - the
  snapshot/journal split's bounded, transition-driven growth is judged
  sufficient for v1. Disclosed, revisitable if that scale assumption ever
  stops holding.
- The full separation of reachability/authentication/transport-identity
  into independently-tracked evidence (rather than one Kind with an
  honest but limited Detail caveat) is out of scope for v1 - a real,
  disclosed simplification.
- `route(8)`'s exact no-default-route output was not verified against
  real hardware during design; the parser defaults to the safe direction
  (`Unknown`, not a guessed `False`) for anything it doesn't recognize,
  but should be checked live post-deploy.
