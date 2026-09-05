# ADR-0063: Cloudflare Tunnel Exposure v1

## Status

Accepted

## Context

The user asked whether any external service APIs (Hurricane Electric,
DigitalOcean, Cloudflare) could integrate with Apiary. Cloudflare was
the clearest fit: Apiary's existing network architecture (ADR-0047
external gateway networks, ADR-0048 self-hosted outbound NAT) handles
egress, but has **no existing mechanism for inbound exposure at all** -
`internal/pf` has zero port-forwarding/destination-NAT concept anywhere
(only source-NAT for egress and per-VM allow/block firewall rules).
The user chose to build this and tabled Hurricane Electric/DigitalOcean.

This is new ground for the project on three fronts, confirmed before
designing anything:

1. **No existing outbound third-party HTTPS API client anywhere** -
   every `net/http` use in the codebase before this feature was
   server-side.
2. **No CODEX.md prior art** - a full grep across CODEX.md's ~2300
   lines for `cloudflare|external.*dns|tunnel|third.party` returns
   nothing relevant. This is user-directed work outside that roadmap
   entirely, unlike every other feature this session.
3. **No existing pattern for "a cluster-relevant, live-cleartext,
   node-local secret."** API keys (ADR-0023) are raft-replicated but
   hash-only, the raw value never stored; TLS certs/`-peer-api-key` are
   node-local file/flag values, never raft. A Cloudflare API token is
   a live, immediately-exploitable third-party credential if leaked
   and would appear in every `raftd -export` archive (ADR-0051) in
   cleartext if ever placed in raft - it must follow the node-local,
   non-raft pattern instead.

The user was asked to pick the tunnel-provisioning model, since it
determines both the size of the new Cloudflare API surface and how
powerful the stored credential needs to be, and chose the narrowest
option: **the operator pre-provisions one Cloudflare Tunnel per Hive by
hand**, the same posture this project already takes for disclosed
manual setup steps (`pkg install dnsmasq`, HAST's manually-applied
kernel patch, PAM's `/etc/pam.d/apiary`, `nmdm.ko`). Apiary's own new
code is scoped to exactly two things: reconcile which Cells are
exposed into that Hive's local `cloudflared` ingress config and manage
the `cloudflared` process lifecycle, and call Cloudflare's DNS API to
create/update just a CNAME record per exposed Cell. This needs only a
narrow Zone:DNS:Edit token and no implementation of Cloudflare's own
Tunnel-provisioning API at all.

A design review pass (a Plan-agent critique, mirroring every CODEX
feature this session's one hard review, applied here too despite this
not being a CODEX-tracked feature, given the real new security surface)
found one design-invalidating defect and one factually wrong claim
about Cloudflare's own product behavior that would have shipped a
feature that doesn't do what it claims.

## Design review

1. **Routing `cloudflare_hostname`/`cloudflare_port` through the
   general `CreateVM`/`UpdateVM` RPCs would repeat the exact mistake
   ADR-0049 already found and fixed once for `firewall_paused`.**
   `toInternalVM` (`internal/manager/convert.go`) builds the internal
   record field-by-field from only what an external caller's proto
   carries, deliberately excluding `IpAddress`/`MacAddress`/`Phase`.
   ADR-0049 names the exact hazard: a caller toggling one field via
   `UpdateVM` would silently wipe unrelated fields unless it first
   fetched and resent the entire current record - its fix was a
   dedicated narrow command, `SetVMFirewallPaused`/
   `applySetVMFirewallPaused`, cloning the existing record and
   touching only that one field atomically inside one `Apply`. Adding
   the two new fields to `toInternalVM`'s carried-through set would
   mean any unrelated `UpdateVM` call (e.g. resizing `vcpus` through
   `PUT /v1/vms/{id}`, which round-trips the whole record) that doesn't
   resend `cloudflare_hostname` silently un-exposes the Cell - a
   security-relevant field disappearing as a side effect of an
   unrelated edit, with no error. **Fixed**: a dedicated
   `SetVMCloudflareExposure{id, hostname, port}` command and RPC,
   mirroring `SetVMFirewallPaused` exactly - clone-and-touch-only-
   those-two-fields inside one atomic `Apply`.
   `cloudflare_hostname`/`cloudflare_port` are excluded from
   `toInternalVM`'s carried-through set, exactly like `IpAddress`.
2. **The premise - "expose a Cell's port to the internet" via `service:
   tcp://<address>:<port>` in cloudflared's ingress config - does not
   do what a plain internet client needs.** `tcp://` ingress in
   Cloudflare Tunnel requires the *connecting client* to run
   `cloudflared access tcp` locally or be enrolled in Cloudflare Zero
   Trust/WARP with private-network routing - it is not reachable by an
   ordinary browser or `curl`/`nc` the way a real port-forward would
   be. Only `http://`/`https://` ingress is transparently reachable by
   any ordinary client at `https://<hostname>` with zero client-side
   tooling (Cloudflare terminates public TLS at its edge). Raw-TCP
   reachable-by-anyone is a different, paid Cloudflare product
   (Spectrum), out of scope. **Fixed**: v1 is scoped to HTTP origin
   traffic only - `cloudflare_port` names the Cell's own local HTTP
   port, and `cloudflared` always proxies to it as plain
   `http://<address>:<port>` (the public leg is HTTPS, terminated at
   Cloudflare's edge; the private leg from `cloudflared` to the Cell
   stays on the Hive's own bridge network, an accepted, common Tunnel
   deployment pattern). TLS-terminated-at-Cell (https origin) and raw
   TCP are explicitly out of scope, regression-tested
   (`TestRenderConfig_IncludesCatchAllAndHTTPScheme` asserts the
   rendered config never contains `tcp://` or `https://`).
3. **Calling Cloudflare's DNS API unconditionally every reconcile tick
   (30s default) borrows the wrong precedent.** `reconcileHASTRoles`'s
   "call `CreateResource` unconditionally, confirmed idempotent in
   practice" pattern is justified specifically because the underlying
   operation (`hastctl`, `pfctl`) is a free, unlimited local CLI call.
   Cloudflare's DNS API is neither free nor unlimited, and shared
   account-wide with anything else using that token. **Fixed**:
   `EnsureCNAME`/`DeleteCNAME` are called only when a VM's desired
   `(hostname, address)` pair actually differs from a persisted local
   record of what was last successfully applied (finding 5) - the same
   "only act when rendered state changed" discipline
   `reconcileHASTRoles` already applies to `hastd` restarts, applied
   per-hostname instead of to one aggregate config blob. No tick-
   counting or periodic-drift-correction machinery was added; a manual
   out-of-band edit to a CNAME in Cloudflare's own dashboard is a
   disclosed, accepted gap, not defended against.
4. **Create-time-only validation of "`cloudflare_hostname` requires
   `network_id`" is insufficient - `network_id` can drift independently
   afterward.** `network_id` (unlike `IpAddress`) *is* carried through
   by a plain `UpdateVM` call, so it can change or clear independently
   of whenever `SetVMCloudflareExposure` last ran - there is no
   raft-level invariant keeping the two in sync after the fact.
   **Fixed**: `reconcileCloudflareTunnel`'s ingress-building step
   independently checks `IPAddress != ""` before building an `Ingress`
   entry for a VM with `CloudflareHostname` set, skipping rather than
   building a broken `http://:80`-shaped entry - regression-tested
   (`TestReconciler_RunOnce_CloudflareSkipsVMWithNoIPAddress`).
5. **Tracking "what did I previously expose" needs a persisted local
   sidecar, not in-process memory.** `EnsureCNAME` being idempotent
   covers *additions* correctly even after a `managerd` restart with no
   memory at all - but *removals* need to know what to delete, and the
   only sources are a persisted local record or querying Cloudflare's
   own zone. Without persistence, a hostname exposed under old raft
   state, reassigned away from this node while it happened to be down,
   would never be torn down after that node restarts with an empty
   in-memory map - the stale CNAME would leak indefinitely. **Fixed**:
   a persisted JSON sidecar (`internal/cloudflare/sidecar.go`,
   mirroring `internal/nodeconfig.Manager`'s own "physical, per-node,
   never raft" shape) recording `{hostname: {vm_id, address}}` last
   successfully applied, loaded fresh each call and diffed against
   current desired state - regression-tested
   (`TestReconcileExposures_SurvivesRestartWithPersistedSidecar`
   exercises a brand-new `Manager` value with no shared in-memory state
   from a prior one, standing in for a real `managerd` restart).
   Disclosed, not defended against: a manually-deleted-in-the-dashboard
   record and a lost/corrupted sidecar file are both accepted risk.
6. **A config-content-only restart check misses `cloudflared` crashing
   independently of any config change** - the same class of bug
   ADR-0043 fixed for bhyve VMs surviving a guest reboot. **Fixed**:
   `Manager.EnsureRunning` also runs its own pidfile + signal-0
   liveness check every call (mirroring `internal/bhyve`'s
   `processAlive` exactly), relaunching whenever the recorded process
   is dead - independent of the "did the config change" restart
   trigger, regression-tested
   (`TestEnsureRunning_RestartsOnDeadProcessEvenWithUnchangedConfig`).
7. **The `daemon(8)` invocation needs `-o <logfile>`, or this project's
   first internet-facing, first third-party-API-calling feature has
   zero visibility into why a tunnel won't connect.** `cloudflared`'s
   only diagnostic signal is its own stdout/stderr (edge connection
   status, auth/token failures, rate-limit responses) -
   `startSerialLogger` already establishes the `-o` precedent for
   exactly this reason. **Fixed**: `daemon -f -p <pidfile> -o <logfile>
   cloudflared tunnel run --config <path>`, one per-Hive log file
   (there is only one `cloudflared` process per Hive, unlike per-VM
   serial logs).
8. **No teardown path existed for the Hive-level `cloudflared` process
   when the feature is fully disabled** (operator unsets
   `-cloudflare-token-file` on a subsequent restart) - a
   `daemon(8)`-detached process from a previous `managerd` lifetime
   would keep running indefinitely, continuing to proxy every
   previously-configured hostname. Unlike every other detached-process
   precedent in this codebase (tied to one VM's own lifecycle via
   `DestroyVM`), `cloudflared` here is a Hive-wide singleton with no
   owning resource to trigger cleanup. **Fixed, not just disclosed**:
   the sidecar file and pidfile live at fixed, well-known paths
   (`internal/cloudflare.DefaultRunDir`) independent of the feature's
   own flags, so even with `Reconciler.Cloudflare == nil`, each tick
   still calls `(&cloudflare.Manager{}).StopIfRunning(ctx)` against
   those fixed paths - closing the loop the same "reconcile toward
   desired state" way every other resource in this codebase does,
   where "disabled" is itself a real desired state (zero exposure).
9. **Jail deferral is correct, restated precisely.** `JailPlacement`
   (`internal/cluster/plan.go`) has no `NetworkID`/`IPAddress` fields at
   all - jails have no per-Cell raft-tracked address today, matching
   this project's own repeated framing ("`internal/jail`'s v1 scope is
   flat `ip4=inherit` networking"). Deferred as a disclosed limitation.

## Design

### New per-Hive host prerequisites (disclosed, operator-performed, not automated)

1. `pkg install cloudflared` on each Hive that will expose Cells.
2. `cloudflared tunnel login` + `cloudflared tunnel create <hive-name>`
   once per Hive - produces a tunnel ID and a credentials JSON file;
   place the credentials file at the path `managerd` is configured to
   read.
3. A Cloudflare API token scoped to **Zone:DNS:Edit only** (never
   Tunnel:Edit or account-level), placed in a file `managerd` reads.
4. Operator must manually stop any leftover `cloudflared` process if
   the feature is fully removed by deleting/replacing the `managerd`
   binary rather than simply unsetting `-cloudflare-token-file` on
   restart (finding 8 covers the normal disable path).

### New `cmd/managerd` flags

`-cloudflare-token-file` (file path, mirrors `-tls-cert`/`-tls-key`'s
precedent - never a flag value or env var), `-cloudflare-zone-id`,
`-cloudflare-tunnel-id` (not secret, plain flag value),
`-cloudflare-tunnel-credentials-file` (file path). Empty
`-cloudflare-token-file` disables the whole feature, matching every
other opt-in capability flag (`-hast-enabled`, `-jail-enabled`,
`-bhyve-bootrom`); when set, the other three are required together
(checked at startup, fails fast like the existing `-tls-cert`/`-tls-key`
pairing check).

### New raft-replicated command, not a general-purpose field (finding 1)

`VMDefinition` (`api/internalpb/state.proto`, `api/rpc/manager.proto`)
gains `cloudflare_hostname string`/`cloudflare_port uint32` (empty
hostname = not exposed, mirrors `iso_name`'s convention) plus a new
`Command` variant `SetVMCloudflareExposure{id, hostname, port}` /
`applySetVMCloudflareExposure` (clone-and-touch-only-those-fields,
mirroring `applySetVMFirewallPaused` exactly). `ManagerService` gains
the matching `SetVMCloudflareExposure` RPC (`RoleOperator`, mirroring
`SetVMFirewallPaused`'s own role). `toInternalVM`/`fromInternalVM`
(`internal/manager/convert.go`) exclude the write direction and
include the read direction, exactly like `firewall_paused`. The RPC
handler fetches the VM's current record first (the same `GetVM`-then-
validate-then-submit shape `MigrateVM` uses) only when `hostname != ""`
- clearing exposure skips the fetch entirely, since there's nothing to
validate when turning exposure off - and rejects a non-empty hostname
when the VM's current `network_id` is empty.

### New package: `internal/cloudflare` (real I/O, mirrors `internal/hast`/`internal/pf`'s role)

- **`dns.go`**: a minimal hand-rolled REST client (stdlib `net/http`/
  `encoding/json` only, no new dependency - matching ADR-0020's own
  demonstrated dependency-minimalism). `EnsureCNAME`/`DeleteCNAME`
  against Cloudflare's `/zones/{id}/dns_records` API; a "record not
  found" response on delete is success, mirroring
  `internal/pf.Manager.Flush`'s own "No such anchor" idempotency fix.
- **`tunnel.go`**: `Ingress{Hostname, Address}`, `RenderConfig` (the
  YAML `cloudflared` needs: `tunnel:`, `credentials-file:`, one
  `hostname`/`service: http://<address>` entry per ingress sorted by
  hostname for deterministic diffing, plus the required catch-all
  `service: http_status:404`), `Manager` (fixed default paths mirroring
  `internal/bhyve`'s `RunDir` convention, `EnsureRunning`/
  `StopIfRunning`/`processAlive` mirroring `internal/bhyve`'s own
  daemon-launch/pidfile/liveness pattern exactly).
- **`sidecar.go`**: the persisted exposure record (finding 5) and
  `ReconcileExposures`, the single entry point that diffs desired state
  against the sidecar, calls `EnsureCNAME`/`DeleteCNAME` only on a real
  change, then calls `EnsureRunning` with the full current ingress list
  regardless (finding 6's independent liveness check).

### Reconciler: one new aggregate step per tick, mirroring `reconcileHASTRoles`'s shape

`internal/cluster/reconciler.go` gains a `cloudflareManager` interface
(mirroring every other reconciler dependency's own narrow-interface
pattern - `vmManager`, `datasetManager`, `pfManager` - so tests inject
a fake with no real outbound HTTP calls or `daemon(8)`/`cloudflared`
processes) and a nil-able `Reconciler.Cloudflare cloudflareManager`
field, called once per `RunOnce` tick via `reconcileCloudflareTunnel`:
gathers every VM in `planned` (already filtered to this node's own
owned, non-deleting VMs) with `CloudflareHostname != ""` and
`IPAddress != ""` (finding 4), builds the desired `[]DesiredExposure`,
and calls `Cloudflare.ReconcileExposures`. A VM entering
`VM_STATE_DELETING` drops out of `planned` (and so its desired
exposure) even before its physical resources are torn down - the
correct behavior, matching how `reconcileHASTRoles` itself already
treats a deleting VM. Errors are captured into `RunOnce`'s existing
`firstErr` pattern (recorded, but reconciliation of other resources
continues), the same isolation every other aggregate step already
gets.

**Disclosed interaction, not a bug**: a Cell's own per-VM `pf` firewall
rules (`apiary/vm-<id>` anchor) still apply to `cloudflared`'s proxied
connection like any other traffic reaching the VM's tap interface -
exposing a hostname via Tunnel does not bypass Apiary's own per-Cell
firewall.

### Frontend: dedicated form action on the VM detail page

A "Public exposure" panel on `/vms/{id}` (`web/templates/vm.html`),
gated behind `.CanOperate`, posting to a new
`POST /vms/{id}/cloudflare-exposure` (`RoleOperator`) handled by
`handleSetVMCloudflareExposure` - a dedicated action, not folded into
any general edit form, mirroring `firewall_paused`'s own precedent.
The Hostname/Port inputs stay enabled if either a network is attached
or a hostname is already set (so a VM whose `network_id` later drifted
away, per finding 4, can still have its stale exposure cleared, not
locked into a disabled form with no way out).

### ADR

This document. Verified: `go build`/`go vet`/`go test ./...`,
`buf generate` (first proto/raft schema change since ADR-0060), FreeBSD
cross-compile of managerd/raftd/restshimd.

## Consequences

- This is the project's first outbound third-party HTTPS API
  integration and its first real internet-facing exposure surface -
  both deliberately minimized in scope (a single DNS record type, an
  operator-provisioned Tunnel, HTTP-origin-only).
- `physically_rehearsed`-style verification of "does this actually
  work end-to-end" has no automated equivalent - live verification
  requires a real Cloudflare account, zone, and pre-provisioned Tunnel,
  and was not performed as part of this change (out of scope for an
  automated check, per this project's own established practice for
  anything needing real third-party infrastructure).
- A manual, out-of-band change to a CNAME record in Cloudflare's own
  dashboard is never detected or corrected (finding 3) - only a change
  to the VM's own desired state (a new `SetVMCloudflareExposure` call,
  or a node/network reassignment) triggers reconciliation.
- Disabling the feature via `-cloudflare-token-file` on restart cleans
  up the leftover `cloudflared` process (finding 8), but a `managerd`
  binary built without this feature at all, replacing one that had it
  enabled, cannot self-clean - the operator must stop `cloudflared`
  manually in that case.
- Jails cannot be exposed in v1 (finding 9) - no per-Cell address
  exists for them to reconcile against.
- TLS-terminated-at-origin (https) and raw TCP exposure (finding 2)
  are both out of scope; the latter would need either Cloudflare
  Access client tooling (defeating the "no client tooling" value
  proposition) or the separate Spectrum product.
