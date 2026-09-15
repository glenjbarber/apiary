# ADR-0102: Live config editing for frontend/restshimd/raftd

## Status

Accepted

## Context

ADR-0100 gave every daemon (`managerd`, `frontend`, `restshimd`, `raftd`)
a fixed JSON config file under `/usr/local/etc/apiary/`, but only
`managerd` got live RPC/web-UI editing (`internal/nodeconfig` +
`GetNodeConfig`/`UpdateNodeConfig` + the Machine Configuration page).
The other three were explicitly left hand-edit-the-file-and-restart-only,
disclosed in that ADR's own "Consequences" section as a "not-yet-decided
follow-up, not an oversight." This ADR closes that gap.

The key architectural fact that shapes the whole design: **`frontend`
and `restshimd` have no RPC server of their own at all** - they are
pure HTTP-server-plus-gRPC-client processes, dialing `managerd`'s RPC
as a client. **`raftd`** has only an internal, token-authenticated
Unix-socket service (`internalpb.RaftInternal`) for cluster
replication, not a general config-management surface, and it isn't
meant to become one. So "give them the same editing `managerd` has"
cannot mean "give each one its own `GetNodeConfig`/`UpdateNodeConfig`"
- there is no RPC boundary there to add it to.

## Decision

Extend `managerd`'s own RPC surface to manage the other three daemons'
config files directly, rather than standing up new network-exposed
listeners on each. This reuses `managerd`'s existing root privileges,
its role as the one authenticated boundary in this architecture
(`frontend`/`restshimd` already defer all authorization to `managerd`'s
RPC layer, ADR-0067), and its existing `RestartNodeService` mechanism -
instead of tripling the RPC attack surface to secure. `managerd`
previously wrote no file under `/usr/local/etc/apiary/` at all - this
is a genuinely new capability, not an extension of an existing write
path.

Three independent Get/Update RPC pairs were added to `ManagerService`
(`GetFrontendConfig`/`UpdateFrontendConfig`,
`GetRestshimdConfig`/`UpdateRestshimdConfig`,
`GetRaftdConfig`/`UpdateRaftdConfig`), not a shared generic message -
matching ADR-0100's own stated preference for small single-purpose
things over generic abstraction. `internal/frontendconfig`,
`internal/restshimdconfig`, and `internal/raftdconfig` each gained a
`Save()` method mirroring `nodeconfig.Manager.Save`'s shape exactly:
validate, then a full-file replace (never a merge), 0600 permissions.

**Secrets** (`ManagerAPIKey` for frontend, `InternalToken` for raftd)
follow the exact write-only/explicit-clear pattern `UpdateNodeConfig`
already established for `PeerAPIKey`/`RaftdToken`: `Get*` never returns
the raw value, only a `_set` boolean; `Update*` takes a plain value
field (empty means "leave unchanged") plus an explicit `clear_*` flag
(the only way to actually blank it).

**raftd's field exclusion** (the one real judgment call in this
design): only `RaftTLSCert`/`RaftTLSKey`/`RaftTLSCA`/`InternalToken`
are RPC-editable - pure security-transport/credential knobs with no
topology meaning. `NodeID`, `Join`, `AwaitJoin`, `DataDir`, `Socket`,
and `RaftBind` are excluded from `UpdateRaftdConfig` entirely (the
first three never even appear in `GetRaftdConfig`'s response;
`DataDir`/`Socket`/`RaftBind` are returned read-only for display),
mirroring `GetNodeConfig`/`UpdateNodeConfig`'s own exclusion of
`node_id`/`rpc_addr`/`raftd_socket`. Reasoning: `Join`/`AwaitJoin` are
one-time-bootstrap-only flags (`raftd`'s own `startupJoinOrBootstrap`
only consults them on a truly empty `DataDir`) - editing them
post-bootstrap via a web form is meaningless at best, a live footgun at
worst. `NodeID` is raft identity. `DataDir`/`Socket`/`RaftBind`
changing without a restart (which this RPC never triggers for raftd -
see below) would either do nothing or point raftd at a
wrong/nonexistent path next restart with no validation path today -
exactly the "operator thinks the change already took effect" surprise
this design avoids.

Because `UpdateRaftdConfig` builds `raftdconfig.Config` fresh from the
request rather than merging onto the current on-disk value - the same
shape `UpdateNodeConfig` already uses, and the same shape that caused a
real bug ADR-0100 had to fix once (its own "`UpdateNodeConfig` merge
fix" section) - every excluded field (`NodeID`, `DataDir`, `Socket`,
`RaftBind`, `Join`, `AwaitJoin`) is explicitly carried over from the
loaded current config before every single `Save` call. This is the
single most safety-critical line of code in the whole feature: without
it, any call to `UpdateRaftdConfig` - not just ones touching raft TLS -
would silently wipe this node's raft identity and topology. It has a
dedicated regression test
(`TestServer_UpdateRaftdConfig_PreservesIdentityTopologyAndBootstrapFields`).

**Restart-after-save**: `frontend` and `restshimd` auto-restart on a
successful config save (both are stateless and non-consensus);
`raftd` never does. `internal/manager/services.go`'s `apiaryServices`
allowlist gained `apiary_restshimd` as `restartable: true` (previously
excluded for the weaker reason "not currently a user-facing web
dependency," not a real safety concern) - `apiary_raftd` stays
excluded, consensus-critical, the exact same judgment
`UpdateRaftdConfig`'s own no-auto-restart design already makes,
applied consistently in two places. The restart mechanism reuses
`RestartNodeService`'s own internal `s.services.Restart` call plus its
250ms flush-first delay (factored into a shared
`scheduleServiceRestart` helper) rather than the RPC itself, since the
target service name is always one of two hardcoded, already-known
values here, never caller-supplied.

**Authorization**: all three `Get*` RPCs sit at `RoleViewer` (matching
`GetNodeConfig`'s own tier - the Machine Configuration page already
shows other daemons' config to viewers, secrets redacted); all three
`Update*` RPCs sit at `RoleAdmin` (matching `UpdateNodeConfig`). No new
confirmation-phrase gate beyond `RoleAdmin` - `RestartNodeService`
itself has no such gate today (just `RoleAdmin` + a browser
`hx-confirm`), and `nodeconfig`'s own secrets are already handled
without one; adding a stronger bar here specifically would be an
inconsistent one-off.

**Web UI**: three new panels (`frontend_config_panel`,
`restshimd_config_panel`, `raftd_config_panel`) added to the existing
Machine Configuration page (`web/templates/machine.html`), inside its
"TLS and security" section rather than a new top-level nav section -
following the exact `{{if .CanAdmin}}`-gated-form-vs-read-only-table,
`hx-post`-to-own-route, secret-badge-plus-password-input-plus-clear-
checkbox pattern every existing panel on that page already uses. The
raftd panel shows `DataDir`/`Socket`/`RaftBind` as plain read-only text
with a note to hand-edit the file and restart manually; its form
carries a visible warning that raftd is never restarted automatically.

## Consequences

- `managerd` now writes files under `/usr/local/etc/apiary/` for
  daemons other than itself for the first time - a genuinely new
  capability and trust boundary. It assumes co-location of `managerd`
  with the daemon it's configuring, which is already true in this
  project's own two-node topology and is not a new constraint this ADR
  introduces.
- `Server` gained three new nil-able fields (`frontendConfig`,
  `restshimdConfig`, `raftdConfig`) wired via new setter methods
  (`SetFrontendConfig`/`SetRestshimdConfig`/`SetRaftdConfig`), not new
  `NewServer` parameters - `NewServer` already has 13 positional
  parameters and ~90 existing call sites across this package's own test
  suite; the setter pattern (already established by
  `SetAssumptionRegister`/`SetOriginCAIssuer`/`SetNetworkInterfaceLister`
  for exactly this situation) avoids touching any of them.
- A saved `raftd.json` change (raft TLS or internal token) is inert
  until an operator manually restarts `apiary_raftd` on that host -
  documented prominently in both this ADR and the web panel's own copy
  so it is never mistaken for a no-op bug.
- restshimd is now part of the auto-restart-capable allowlist for the
  first time; this is a strictly additive capability change (nothing
  previously depended on restshimd never restarting) and mirrors
  frontend's own existing behavior exactly.

## Verification

Unit tests added:
- `internal/frontendconfig`, `internal/restshimdconfig`,
  `internal/raftdconfig`: `Save`/`Load` round-trip every field
  (`raftdconfig`'s round-trip deliberately includes the RPC-excluded
  fields, confirming `Save` itself is exclusion-agnostic - that's the
  handler's job, not `Save`'s), validation rejects malformed
  addresses/paths and injected newlines, `Save` is a full replace not a
  merge.
- `internal/manager`: all six new RPC handlers - `Get*` never returns a
  secret's raw value, `Update*` round-trips a full field set, the
  three-way secret semantics (empty leaves unchanged / non-empty sets /
  explicit clear), `RoleViewer` can call `Get*` but not `Update*`,
  restart-after-save invokes the fake service controller for
  frontend/restshimd but never for raftd, and the
  identity/topology/bootstrap-field-preservation regression test for
  `UpdateRaftdConfig` described above.
- `internal/manager/services_test.go` (new): `apiary_restshimd` is now
  `restartable: true`, `apiary_raftd` remains `false`.
- `internal/frontend`: the Machine page renders all three new panels;
  each `*ConfigUpdateRequest` helper's partial-form behavior (a form
  touching one field doesn't clobber the rest, mirroring the existing
  `nodeConfigUpdateRequest` test pattern); the raftd panel never
  renders an editable `data_dir` input.
- `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .`, and
  `buf generate` re-run all clean.

Live rollout on `apiverse`/`apiarium` is pending - to be done
incrementally per this ADR's own recommended order (restshimd first,
lowest risk; then frontend, verifying the web UI's own HTTP connection
survives its self-triggered restart; then raftd's config-write-only
path, verified without ever test-restarting raftd through this new
RPC), with explicit operator confirmation at each step given the
production stakes.

## Correction (2026-09-15): removed UpdateRaftdConfig; fixed a permissions bug in every Save()

A post-merge audit (before any real `Update*` write was ever made
against the live cluster - only read-only `Get*` calls had been
exercised) found two release-blocking issues and one real bug in the
design above:

- **P1**: `internal_token` (raftd's own config) and managerd's
  separately-configured `raftd_token` (`nodeconfig.Config`) must match
  for `RaftInternal` auth to keep working - `UpdateRaftdConfig` let an
  operator change one independently of the other via two entirely
  separate panels, with no coordination. Restarting raftd after such a
  change would break managerd-to-raftd authentication until the
  mismatch was noticed and fixed by hand.
- **P1**: raft TLS certificate/key/CA is cluster-coupled, not a plain
  per-host setting - changing it on one voter and manually restarting
  raftd (which this RPC never automated, by design) can isolate that
  voter and lose quorum in a two-node cluster.
- **P2**: `Save()` in every one of the four config packages
  (`nodeconfig` included - this bug predates this ADR, inherited by
  the three new packages that mirrored its shape) used a plain
  `os.WriteFile(path, body, 0o600)`. Go only applies the mode argument
  when the file is *created*; saving over an already-existing file
  left it at whatever permissions it already had. A `frontend.json` or
  `raftd.json` that had ever existed at `0644` - however that
  happened - would stay world/group-readable across every subsequent
  `Save`, despite holding real credentials.

**Remediation, chosen over building a full coordinated rotation
workflow** (a materially larger feature - cross-host transactions,
peer TLS compatibility checks - deferred as genuinely future work, not
implemented here): `UpdateRaftdConfig` and its request/response
messages were removed entirely from the proto, the RPC handler, the
auth map, and the web UI. `GetRaftdConfig` is unchanged and still shows
every field (including raft TLS paths and whether `internal_token` is
set) for context - raftd's config remains hand-edit-the-file-and-
restart-only for every field, exactly as before this ADR, with the
Machine page's raftd panel now explaining why in its own copy. All
four `Save()` implementations were changed to write via a temp file in
the same directory, explicit `chmod 0600`, then atomic rename -
mirroring `internal/assumptions.Manager`'s own existing
write-then-rename convention - with a dedicated regression test per
package proving `Save` tightens permissions on a file that pre-existed
at `0644`.

`frontend`/`restshimd` config editing (the two daemons where this audit
found no equivalent coupling risk) is unaffected by this correction.
