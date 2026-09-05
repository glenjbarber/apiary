# ADR-0049: Machine Configuration page

## Status

Accepted

## Context

The web UI had pages for VMs, jails, networks, images, host stats, and
API keys - but nothing for per-node operational settings. Three gaps
were named as a starter list: which physical NIC a node uses (today
only settable via `-vlan-uplink`/`-nat-uplink` flags baked into
`/etc/rc.conf`, requiring hand-editing plus a full `managerd` restart -
the exact manual dance ADR-0048's own debugging needed this session), a
way to temporarily disable a VM's firewall rules without deleting them,
and setting a ZFS quota on a dataset (`internal/zfs` already has the
primitive - `SetProperty` - but no RPC/UI ever exposed it).

## Decision

One new `/machine` page with independent sections, each its own
HTMX-targeted panel (`internal/frontend/machine.go`).

### Uplink settings

New `internal/nodeconfig` package: a `Manager{Path string}` doing plain
local JSON file I/O (mirrors `internal/isostore`'s shape exactly),
persisting `{Uplink, NATUplink, JailEnabled}`. Physical, per-node data - never
raft-replicated, since a NIC name is only meaningful to the one node
that has it. `cmd/managerd` loads this file once at startup, overriding
the matching `-vlan-uplink`/`-nat-uplink`/`-jail-enabled` flag value when set
(flags remain the bootstrap default for a fresh install with no file yet).
New RPCs `GetNodeConfig`/`UpdateNodeConfig` (local-only, modeled
directly on `HostStats`'s existing shape - empty-request-message, no
raft involved) let the UI read/write the file through managerd's
already-running process.

**Takes effect on next `managerd` restart, not live** - deliberately
matching how every other node-level flag in this project already
behaves, rather than introducing live-reload/mutex-guarded-field
machinery for a first pass. The page says so next to the save button.

### Jail provisioning override

The same Machine configuration panel can explicitly enable or disable
`-jail-enabled` for this Hive, or clear the override and use the startup flag.
The field is optional end to end (`optional bool jail_enabled` in the RPC and
`*bool JailEnabled` in `internal/nodeconfig.Config`) so saving unrelated
Machine settings does not accidentally convert "use the startup flag" into
"disabled." This was added after ADR-0064 made disabled jail provisioning
visible as a reconciliation error; the obvious operator action now lives on
the same node-local configuration page as the other restart-required settings.

### Per-VM firewall pause

`VMDefinition` gains `bool firewall_paused`. Deliberately **not**
routed through the general `UpdateVM` RPC: `internal/raft/fsm.go`'s
`applyUpdateVM` does a full-replace (`f.vms[id] = vm`), and
`internal/manager/convert.go`'s `toInternalVM` builds a fresh record
from only the fields an external caller sent (already dropping
`IpAddress`/`MacAddress`/`Phase` for exactly this reason) - a caller
toggling just this one field via UpdateVM would silently wipe
`FirewallRules`/`NetworkId`/etc. unless it first fetched and resent the
entire current record.

Instead: a new narrow `Command` variant `SetVMFirewallPaused{id,
paused}` (mirrors `UpdateVMPhase`'s single-field shape) and matching
external RPC, applied by a new `applySetVMFirewallPaused` that clones
the existing record and touches only that field. This is also provably
race-free in a way a fetch-then-clone-then-`UpdateVM` approach (the
pattern `MigrateVM` itself uses) is not: the read-modify-write happens
atomically inside the FSM's own single `Apply` call under its own lock,
with no window for a concurrent `UpdateVM` to change some other field
in between.

`internal/cluster/reconciler.go`'s two firewall-apply sites (VM-creation
and the "already running" branch in `ensureVM`) now compute rules via a
new `effectivePFRules(vm)` helper: `nil` (everything allowed, per
`pf.Manager.Apply`'s own doc comment) when `FirewallPaused` is set,
`toPFRules(vm.FirewallRules)` otherwise. `FirewallRules` itself is
never touched - un-pausing just resumes normal enforcement on the next
tick.

### ZFS dataset quota

New local-only RPC `SetDatasetQuota(dataset_name, quota)`, handled
directly against a `*zfs.Manager` wired onto `internal/manager.Server`
the same nil-able-field way `ISOs`/`VNCLookup` already are. Calls the
existing `Manager.SetProperty(ctx, name, "quota", quota)` - already
scoped safely under `Manager.Base` by the existing `path()` validation.

**Known limitation, not worked around**: `zfs.Manager.path()` rejects
an empty name, so this can only quota a named sub-dataset (a VM's or
jail's own dataset), never the `Base` dataset itself directly -
consistent with `zfs.Manager`'s own existing scoping.

## Role gating

- `GetNodeConfig`: Viewer (read-only, mirrors `HostStats`).
- `UpdateNodeConfig`: Admin - host-wide network and jail-provisioning
  reconfiguration, same tier as `CreateAPIKey`.
- `SetVMFirewallPaused`: Operator - matches other VM lifecycle actions.
- `SetDatasetQuota`: Operator - matches `UploadISO`/`DeleteISO`.
- The `/machine` page itself is Operator-visible (the lowest of the
  three); the uplink form is additionally hidden from a non-Admin
  viewer in the template (`.CanAdmin`), the same defense-in-depth
  pattern the Users page already uses for its per-row password action.

## A corrected assumption during implementation

The initial plan for the firewall-pause table said to reuse
`ListVMsLocal` for "VMs on this node." That RPC turned out to be
internal-only (`api/internalpb/raftd.proto`, used by the reconciler via
`RaftClient`) - `internal/frontend` only ever talks to the external
`ManagerService`, which has no local-only VM listing RPC at all.
Fixed by reusing the existing `ListVMs` (already leader-forwarding,
ADR-0035) and filtering client-side in the frontend handler for
`NodeId == this node's own id` (from `Status`) - achieves the same
result with no new RPC needed.

## Not addressed

- No live-reload for uplink settings - restart required, as above.
- No REST (`internal/restshim`) mirror for any of the three new RPCs -
  same posture as `HostStats`/`GetVMConsole`, which also never got one.
- No dataset/quota listing - the quota form is fire-and-report only,
  a "starter" not a full storage management UI.
- Multi-node dispatch (viewing/editing another node's settings from
  this node's UI, mirroring `HostStats`'s peer-fetch pattern) is out of
  scope for v1 - same limitation `GetVMConsole` already carries.

## Verification

Unit tests: `internal/nodeconfig` (load/save round-trip, missing-file
zero-value, optional jail provisioning override); `internal/raft/fsm`
(`SetVMFirewallPaused` touches only that field); `internal/manager`
(local-only handler tests for `GetNodeConfig`/`UpdateNodeConfig`/
`SetDatasetQuota` with fakes, plus a real raft-harness integration test for
`SetVMFirewallPaused`); `internal/cluster` (`effectivePFRules` applied
correctly on both the create and already-running branches);
`internal/frontend` (`/machine` panels, mirroring the `apikeys`/`networks`
page test patterns, including jail provisioning's explicit and default modes).

`go build ./...`, `go vet ./...`, and the full `go test ./...` suite
all pass.
