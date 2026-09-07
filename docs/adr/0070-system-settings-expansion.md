# ADR-0070: System settings expansion

## Status

Accepted. **Update**: the `jail_console_enabled` tri-state setting this
ADR added (mentioned below alongside `HASTEnabled`/`PeerTLS`) no longer
exists - it was removed along with the jexec jail console feature
itself. See ADR-0068's own "Removed" section for why. Every other
setting this ADR describes is unaffected.

## Context

ADR-0049 gave the Machine Configuration page four settings (uplink,
NAT uplink, DNS server, jail provisioning), leaving roughly two dozen
more `managerd` startup flags reachable only by hand-editing
`/etc/rc.conf` and restarting: ZFS/bhyve/jail resource-scope paths,
bhyve VM tuning, HAST/jail-console/peer-TLS tri-state toggles, peer
forwarding (port, TLS, hostname map, API key), TLS cert/key paths,
Cloudflare Tunnel config, Assumption Register tuning, and the raftd
internal token. The user asked for "all (or most)" of `rc.conf` to be
configurable from a system settings page, extending the existing
Machine page rather than adding a new one.

Two judgment calls needed a decision before implementation:

1. **Secrets and raft identity.** `-node-id`, `-rpc-addr`, and
   `-raftd-socket` are this node's own identity/plumbing into the
   cluster - changing one live, through a form, risks a node silently
   losing its own raft membership or dialing the wrong socket, with no
   way to distinguish "meant to change identity" from "fat-fingered a
   field." `-peer-api-key` and `-raftd-token` are live secrets. The
   user chose: exclude the three identity/plumbing flags from this
   page entirely (`rc.conf`/restart only, forever), and expose the two
   secrets as write-only fields - never displayed back once saved,
   mirroring the Users page's own password-change field (ADR-0039).
2. **Resource-scope paths.** `-zfs-base`, `-bhyve-prefix`, `-iso-dir`,
   `-jail-prefix`, `-jail-mount-base` scope where this Hive looks for
   its own existing resources (mirrors ADR-0007's jail-prefix
   reasoning). Changing one after real VMs/jails/ISOs already exist
   under the old value doesn't move them - it just makes the
   reconciler stop seeing them, silently orphaning real resources. The
   user's answer, verbatim: "Allow changing them on initial setup, but
   do not allow changes for the sake of making changes." Implemented
   as a strict write-once rule: editable only while unset; once a
   non-empty value is saved, saving a different non-empty value is
   rejected - both server-side (the actual boundary) and in the UI
   (which simply doesn't offer an input once set).

## Decision

Eight new panels on the existing `/machine` page
(`web/templates/machine.html`), each its own HTMX-targeted fragment
exactly like ADR-0049's original four: **Resource scope paths**,
**Bhyve / VM provisioning**, **HAST-backed disk replication**,
**Peer-to-peer forwarding**, **TLS**, **Cloudflare Tunnel** (config
fields only - ADR-0063 already owns the feature's own reconciler/DNS
logic), **Automated Assumption Checks tuning**, and **Internal
security**. No new page, no new RPC service - every panel reuses the
existing `GetNodeConfig`/`UpdateNodeConfig` pair ADR-0049 established,
extended rather than replaced.

### `internal/nodeconfig.Config` grows from 4 fields to ~29

Covers every newly-exposed flag. Three field shapes, reusing existing
precedent rather than inventing new ones:

- **Tri-state `*bool`** (nil = use startup flag, true/false = explicit
  override) for `HASTEnabled`, `JailConsoleEnabled`, `PeerTLS` -
  `JailEnabled`'s existing pattern, generalized into a shared
  `triState()` helper in `internal/frontend/convert.go` rather than
  copy-pasted three more times.
- **Write-once scope paths** (`ZFSBase`, `BhyvePrefix`, `ISODir`,
  `JailPrefix`, `JailMountBase`): a new `checkScopePathConflicts(current,
  next Config) error` inside `Manager.Save` rejects a changed non-empty
  value when one is already saved. This lives in the library, ahead of
  and independent of any UI enforcement - the same "the backend is the
  real boundary" posture ADR-0067 established for config-injection
  hardening, applied here to an accidental-overwrite hazard instead of
  a malicious one.
- **Write-only secrets** (`PeerAPIKey`, `RaftdToken`): the RPC layer
  never returns the raw value, only `*_set bool` flags
  (`PeerApiKeySet`/`RaftdTokenSet`). Setting one requires a
  non-empty value in the corresponding form field; clearing one
  requires an explicit `clear_peer_api_key`/`clear_raftd_token` flag,
  since an empty string alone can't distinguish "leave unchanged" from
  "clear it" the way every other field in this form can.

Durations (`ReconcileInterval` and five Assumption-tuning fields) are
stored as `time.Duration` internally but transmitted as
`time.ParseDuration`/`Duration.String()` strings over the wire, with a
special-cased empty string for the zero value (`durationString`) so an
unset field never misleadingly renders as Go's own `"0s"`.

`internal/nodeconfig.validate` gained roughly 15 new checks run before
any value is ever accepted or persisted - interface names, IP
addresses, file paths (rejecting embedded `\n`/`\r`), ZFS-dataset/
prefix charset, the `ip=hostname` pair format for
`PeerTLSHostnameMap`, port range, and non-negative durations - the
same "validate at the point first accepted" discipline ADR-0067
established for this codebase's config-generation surface, extended to
every new field a form can now write.

### Proto and RPC

`GetNodeConfigResponse`/`UpdateNodeConfigRequest` (`api/rpc/manager.proto`)
gain matching fields 8-36, plus four write-only-secret fields on the
request (`peer_api_key`/`clear_peer_api_key`/`raftd_token`/
`clear_raftd_token`). `UpdateNodeConfig` (`internal/manager/server.go`)
loads the current config first, parses six duration fields (returning
immediately on any parse error, before `Save` is ever called), applies
the set/clear/unchanged secret logic, and calls `Save` with the full
resulting config - `Save`'s existing "replace the whole file, never
merge" contract is unchanged.

### Frontend: one shared baseline, many panels

`nodeConfigUpdateRequest` (`internal/frontend/machine.go`) unconditionally
rebuilds **all** ~30 fields from the currently-saved config as a
baseline, then overrides only the fields the specific submitting
form actually included (`r.Form.Has("field_name")`). Eight new
one-line handlers (`handleUpdateResourceScope`, `handleUpdateBhyveConfig`,
`handleUpdateHASTProvisioning`, `handleUpdatePeerForwarding`,
`handleUpdateTLSConfig`, `handleUpdateCloudflareConfig`,
`handleUpdateAssumptionTuning`, `handleUpdateInternalSecurity`) each
delegate to the same `handleUpdateMachineConfig`, differing only in
which panel name they re-render. This is what makes eight independent
panels safe: no panel's save can silently wipe another panel's
settings, since every submission starts from the real current state,
not a zeroed struct.

An earlier draft of `handleUpdateMachineConfig` reconstructed only
four fields of the already-built request before forwarding it - caught
before any test failure, since it would have silently discarded every
new field on every single save. Fixed by passing the built request
straight through.

### Flag-override ordering in `cmd/managerd`

The pre-existing node-config-to-flag override block ran too late in
`run()` - after `raftClient` was already dialed with `*raftdToken`,
after `isos := isostore.New(*iso Dir)`, after
`manager.NewPeerReporter(*peerAPIKey, *peerTLS, ...)` was already
constructed. Several of the newly-overridable fields
(`RaftdToken`, `ISODir`, `PeerAPIKey`, `PeerTLS`,
`PeerTLSHostnameMap`) feed exactly those constructions, so the whole
load-and-override block had to move to immediately after the one-shot
CLI-mode early-return checks, before any flag value is consumed by
anything else.

## Role gating

Unchanged from ADR-0049's existing posture: `GetNodeConfig` stays
Viewer (read-only); `UpdateNodeConfig` stays Admin. All eight new
routes (`POST /machine/resource-scope`, `/machine/bhyve`, `/machine/hast`,
`/machine/peer-forwarding`, `/machine/tls`, `/machine/cloudflare-config`,
`/machine/assumption-tuning`, `/machine/internal-security`) are gated
`manager.RoleAdmin`, matching every existing Machine Configuration
write.

## Not addressed

- `-node-id`/`-rpc-addr`/`-raftd-socket` remain `rc.conf`/restart-only
  by design (see Context) - there is no plan to expose these later
  without a materially different safety mechanism than a plain form.
- No live-reload for any of the new fields - "changes take effect on
  next `managerd` restart, not live" is preserved for every field
  added here, matching ADR-0049's original posture exactly.
- No UI affordance to un-set a scope path once written through this
  page - by design (Context, judgment call 2). The only way to change
  one is to edit `rc.conf`/the node-config JSON file directly and
  restart, same as the identity flags above.
- No REST (`internal/restshim`) mirror for any of the newly exposed
  fields - same posture as `GetNodeConfig`/`UpdateNodeConfig`
  themselves, which never got one either.

## Verification

Unit tests: `internal/nodeconfig` (`TestManager_SaveRejectsUnsafeNewFields`
- table-driven, ~19 cases covering every new validation check;
`TestManager_SaveEnforcesScopePathWriteOnce` and its `_AllFields`
variant; `TestManager_Save_FileModeIs0600`); `internal/manager`
(`TestServer_GetNodeConfig_NeverReturnsSecrets` - a `protojson`-marshaled
response asserted to contain no secret substring anywhere, not just in
the specific field one might remember to check;
`TestServer_UpdateNodeConfig_SecretSemantics` - set/clear/unchanged;
`TestServer_UpdateNodeConfig_InvalidDurationRejectedBeforeSave`;
`TestServer_UpdateNodeConfig_ParsesDurationFields`;
`TestServer_NodeConfig_NewFieldsRoundTrip`); `internal/frontend`
(one page-render test asserting all eight panels show their current
values and that a set scope path has no editable input, one asserting
an unset scope path is editable, and one forwarding-test per new
panel, mirroring ADR-0049's own `TestServer_UpdateNodeConfig_ForwardsFormValues`
pattern).

`go build ./...`, `go vet ./...`, `gofmt`, `git diff --check`, and the
full `go test ./...` suite all pass. FreeBSD cross-compile confirmed
for `managerd`/`raftd`/`restshimd` (unaffected by this change;
`cmd/frontend` remains native-build-only per ADR-0030's cgo/PAM
constraint). Live-verified by running `cmd/frontend` natively against
an unreachable `managerd`: all eight panels rendered with correct
"(unset)"/tri-state/secret-badge placeholders and the expected form
fields, confirming the templates parse and render correctly end to end
(not just via `go test`'s in-process harness).
