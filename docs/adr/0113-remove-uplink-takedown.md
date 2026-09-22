# ADR-0113: Remove the uplink admin down/up toggle (reverses ADR-0085)

## Status

Accepted. Supersedes and reverses [ADR-0085](0085-uplink-admin-toggle.md)'s
`SetUplinkState`/`GetUplinkStatus` toggle (the NAT-pause coupling added on
top of it by [ADR-0088](0088-pause-nat-on-uplink-down.md) is removed as
part of the same change, since it existed only to serve this toggle).

## Context

ADR-0085 added a Machine Configuration page feature letting an Admin
administratively bring a Comb's own uplink interface down or back up
(`ifconfig <uplink> down`/`up`) from the web UI. The RPC itself, and the
frontend handler that calls it, never checked whether the interface being
taken down was the host's only interface, or whether the caller's own
session depended on it. ADR-0085 disclosed this risk explicitly and
accepted it as a deliberate tradeoff, relying entirely on an `hx-confirm`
browser dialog and operator judgment rather than any server-side
guardrail. This was flagged as a paused, never-resumed TODO
(SHARED.md, 2026-09-19 entry): "a UI/RPC action that runs
`ifconfig <uplink> down` with no server-side check for whether it's the
host's only interface."

No guardrail was ever built for this. Rather than add one now, the
project owner asked for the feature to be removed outright: a Comb
should never be able to remove its own only path to reach it, and there
is no use case for this toggle important enough to justify carrying that
risk indefinitely on the strength of a confirm dialog alone. This is a
deliberate risk-reduction removal, not a refactor - the capability itself
is what's being cut, not just its missing guardrail.

## Decision

Remove the entire uplink admin down/up toggle feature, end to end:

- **Proto** (`api/rpc/manager.proto`): removed the `GetUplinkStatus` and
  `SetUplinkState` RPCs from `ManagerService`, and the
  `GetUplinkStatusRequest`/`GetUplinkStatusResponse`/
  `SetUplinkStateRequest`/`SetUplinkStateResponse` messages. Regenerated
  `api/rpc/manager.pb.go` and `api/rpc/manager_grpc.pb.go` via
  `buf generate` (confirmed a clean, reproducible diff - a second
  `buf generate` run produces no further changes).
- **`internal/manager/server.go`**: removed the `GetUplinkStatus` and
  `SetUplinkState` RPC handler methods; removed `UplinkInterface`/`Down`/
  `Up` from the local `VLANStatus` interface (the subset of
  `*vlan.Manager` this package depends on) since nothing else in the
  package used them; removed the `natPauser` interface, the server's
  `nat` field, and the `SetNATPauser` wiring method - all of which
  existed solely to let `SetUplinkState` pause outbound NAT
  (ADR-0088) and had no other caller.
- **`internal/manager/auth.go`**: removed the `GetUplinkStatus`
  (Viewer) and `SetUplinkState` (Admin) entries from the `requiredRole`
  map, and updated two neighboring comments that cross-referenced
  `SetUplinkState` as an example of another RPC's tier.
- **`internal/vlan/manager.go`**: removed `Manager.Down`, `Manager.Up`,
  and `Manager.UplinkInterface` - confirmed via a repo-wide grep that
  these three methods had no caller left anywhere outside the RPCs just
  removed and their own tests. `Manager.InterfaceStatus` (used by
  `ListNetworks`'s per-node bridge status) and `EnsureVLAN` (including
  its untagged/raw-uplink no-op short circuit) are untouched - they
  serve unrelated, legitimate normal interface lifecycle, not this
  guardrail-bypass toggle.
- **`internal/cluster/reconciler.go`**: removed `Reconciler.NATUplink`
  and `Reconciler.PauseOutboundNAT` (ADR-0088) - both existed only to
  back `SetUplinkState`'s NAT-pause side effect and had no other caller.
  `Reconciler.Uplink` (the underlying config field) and `natAnchor`
  (still used by the reconciler's own normal NAT apply/flush paths) are
  untouched.
- **`cmd/managerd/main.go`**: removed the `srv.SetNATPauser(reconciler)`
  wiring call and its ADR-0088 comment.
- **`internal/frontend`**: removed `handleSetUplinkState` and
  `renderUplinkPanel` (`machine.go`); removed `currentUplinkStatus` and
  its two call sites (`machine.go`'s `machinePageData`,
  `standalonejoin.go`'s `renderMachinePageWithConvertJoiner`); removed
  `uplinkStatusView`/`fromRPCUplinkStatus` (`convert.go`); removed the
  `UplinkStatus`/`UplinkFormError`/`UplinkFormSuccess` fields from
  `pageData` and the `POST /machine/uplink-state` route registration
  (`server.go`). The unrelated `POST /machine/uplink` route
  (`handleUpdateNodeConfig`, which persists the configured uplink
  interface name for next restart - a different feature entirely) is
  untouched.
- **Templates**: removed the "Uplink maintenance" panel (including its
  `hx-confirm` down/up buttons) from `web/templates/machine.html` and
  `web/templates/machine_sections.html`, and the `uplink_panel` template
  definition itself from `machine.html`.
- **Tests**: removed tests that only asserted the removed behavior -
  `internal/manager/server_test.go` (`GetUplinkStatus`/`SetUplinkState`
  RPC tests and the `fakeNATPauser` helper), `internal/manager/auth_test.go`
  (RBAC-tier tests for both RPCs), `internal/manager/integration_test.go`
  (trimmed `fakeVLANStatus` back to just `InterfaceStatus`, dropping the
  `uplink`/`downErr`/`upErr` fields and the `UplinkInterface`/`Down`/`Up`
  methods it implemented only for this feature),
  `internal/cluster/reconciler_test.go` (`PauseOutboundNAT`/`NATUplink`
  tests), `internal/frontend/machine_test.go` and
  `internal/frontend/server_test.go` (page/RPC/fakeClient coverage of
  the toggle and its panel), `internal/restshim/server_test.go` (fake
  client methods for the two removed RPCs), `internal/vlan/manager_test.go`
  (`TestUplinkInterface`/`TestDown_NoUplinkConfiguredIsError`/
  `TestUp_NoUplinkConfiguredIsError`, and updated
  `TestEnsureVLAN_UntaggedIsANoOp`'s doc comment to no longer reference
  the now-deleted `Down`), and `internal/vlan/integration_test.go`
  (`TestIntegration_DownUp_TogglesInterfaceState`, the only test that
  exercised `Manager.Down`/`Up` against a real interface).

## What is explicitly not touched

- ADR-0085's other, unrelated fix - keying VLAN/DHCP/PF construction on
  `-vlan-uplink` alone rather than nesting it inside `-bhyve-bootrom` -
  remains in place. That was a real, narrow correctness fix
  (uplink-mismatch health checking on bhyve-disabled nodes) independent
  of the toggle being removed here.
- The `POST /machine/uplink` route and `UpdateNodeConfig`'s
  `Uplink`/`NatUplink` fields (the *configured*, persisted uplink/NAT
  interface names, applied at next restart) are a distinct feature from
  the administrative down/up toggle and are untouched.
- `vlan.Manager.InterfaceStatus`/`EnsureVLAN`/`EnsureBridge`/
  `EnsureMember`/`EnsureBridgeAddress`/`DestroyBridge`/`DestroyVLAN` -
  normal interface lifecycle used by the reconciler's own tick loop -
  are untouched.
- `internal/pf`'s NAT apply/flush machinery and `natAnchor` are
  untouched; only the ADR-0088-specific `PauseOutboundNAT`/`NATUplink`
  wrapper pair on `*cluster.Reconciler`, added solely to serve
  `SetUplinkState`, is removed.
- ADR-0085 and ADR-0088 themselves are left in place as historical
  record, not deleted - this ADR supersedes and reverses their design,
  it does not erase the record that they existed.

## Consequences

- There is no longer any way, from the web UI or the RPC surface, to
  administratively bring a Comb's uplink interface down or back up.
  Recovering a genuinely wedged interface again requires direct
  host-level access (SSH/console `ifconfig`), the same as before
  ADR-0085 existed.
- The Machine Configuration page's "Uplink maintenance" panel is gone
  entirely - both the display of the interface's current up/down state
  and the toggle buttons - since the display's only purpose was backing
  this toggle.
- No new guardrail was added anywhere. This ADR is a straight removal:
  the capability that had no safe way to be gated is gone, rather than
  gated after the fact.
