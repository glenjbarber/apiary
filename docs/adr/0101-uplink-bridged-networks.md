# ADR-0101: `NetworkDefinition.uplink_bridged` - VMs on the host's own LAN

## Status

Proposed

## Context

`NetworkDefinition` (ADR-0022) already lets a cluster run multiple,
simultaneous, independently-addressed networks, and every VM already
picks one via `VMDefinition.network_id`. Today there are exactly two
modes, distinguished by whether `external_gateway` is set:
isolated-NAT (ADR-0048, the default - Apiary's own per-network bridge,
`dnsmasq` DHCP, `pf(8)` NAT out through the uplink) and
shared-VLAN-external-gateway (ADR-0047 - a dedicated VLAN/subnet with
a real upstream router already serving it).

The user asked for a third case: some VMs should land directly on the
*same* subnet as the rest of the physical network (e.g. `10.50.x.x`),
getting a real DHCP lease from the LAN's own router, indistinguishable
from any other device on that LAN - while other VMs stay isolated
(e.g. `10.62.x.x`) on the existing modes, simultaneously, on the same
cluster. Neither existing mode does this: a VM either gets the legacy
flat `-bhyve-bridge` fallback (no VLAN/IP/NAT bookkeeping at all,
under-exercised in production) or an Apiary-owned isolated/gatewayed
network - never the host's own management bridge.

`apiarium` already has the right shape by hand: `re0` (the physical
uplink NIC) is a member of `bridge0`, and `bridge0` - not `re0` - is
what actually holds the host's own DHCP-assigned management address
(`10.50.0.14`). This feature formalizes that existing, hand-migrated
topology into a reusable, per-network, cluster-wide option instead of
one-off host plumbing.

## Decision

Add `bool uplink_bridged` to `NetworkDefinition` in both
`api/rpc/manager.proto` and `api/internalpb/state.proto`, orthogonal to
`external_gateway`. `vlan_id == 0` already means "untagged, but still
Apiary's own isolated per-network bridge" for existing networks, so it
can't double as this mode's own discriminator - a dedicated boolean is
unambiguous.

When `uplink_bridged` is true, the network reuses the node's own
*existing* `-bhyve-bridge`/`allow_uplink_bridging`-gated bridge
(`Reconciler.Bridge`) directly - it never creates a new bridge, never
tags a VLAN, never claims an address on it, and never applies NAT. The
raft FSM (`internal/raft/fsm.go`) rejects `uplink_bridged=true`
combined with a nonzero `vlan_id`, a set `external_gateway`, or a set
`bridge_name` at `CreateNetwork` time - this mode always reuses the
node's own bridge as-is, so none of those three make sense together
with it. `applyCreateVM` skips its normal `allocateIP` call for a VM on
such a network: `vm.ip_address` stays empty, since the physical LAN's
own router owns DHCP for this segment, not Apiary. `deriveMAC` still
runs unconditionally, so an operator can pre-register a static DHCP
reservation on their own router by MAC if desired.

The reconciler (`internal/cluster/reconciler.go`) branches `ensureNetwork`
into a new `ensureUplinkBridgedNetwork` for this mode: it verifies the
node has opted in (`Reconciler.UplinkBridgingEnabled`) and that the
configured bridge already exists (`vlanManager.InterfaceStatus`,
read-only - never falls back to creating it), then returns a
`networkArtifact` naming that bridge with `OwnBridge`/`OwnVLAN`/
`OutboundNAT` all `false`. That triple is the entire safety argument:
`reconcileNetworkArtifacts` (and `DeleteNetwork` by extension) already
key off these booleans to decide what to tear down, so an
uplink-bridged `NetworkDefinition` can never cause Apiary to destroy or
detach the host's own pre-existing bridge - there is nothing of this
mode's own for Apiary to ever consider owning. `internal/bhyve`'s
`CreateVM`/`createTap` need no changes at all - they already take a
plain bridge-name string with zero awareness of which mode produced
it.

## Safety mechanism: per-node opt-in + independent dead-man's switch

Bridging a VM's tap onto the host's own management-plane bridge is a
materially higher-blast-radius operation than any existing network
mode: a misbehaving VM there has direct L2 access to the same
broadcast domain the host's own management IP lives on. No
dead-man's-switch/rollback-timer mechanism existed anywhere in this
codebase before this feature - the one real precedent, `apiarium`'s
`re0`->`bridge0` migration, was done by hand with an ad hoc,
never-committed shell script. Two independent layers address this:

**Layer 1 - explicit per-node confirmation phrase.**
`nodeconfig.Config.AllowUplinkBridging` must be exactly
`"yes-share-uplink-bridge"` to opt in; any other value (including a
plain `"true"`) is treated as unset, deliberately - a boolean invites
an unreviewed `"true"` copy-pasted from node to node for a setting
whose whole risk is "this node's management connectivity can now be
affected by VM traffic." `internal/install`'s new `uplink-bridging`
check (report-only, `StatusManual`) makes the risk explicit at install
time whenever the phrase is set, and flags the meaningless case of
opting in without a working `-bhyve-bridge`/`-vlan-uplink` pair for it
to reuse.

**Layer 2 - a dead-man's switch independent of managerd's own
liveness.** New package `internal/deadman` schedules revert jobs via
FreeBSD base-system `at(8)`/`atq(1)`/`atrm(1)` - a separate,
always-running OS daemon, not a goroutine inside managerd. The key
property: the revert must still fire even if managerd itself is wedged
(e.g. the VM's own traffic knocks the reconciler or its raft heartbeat
over), so the timer cannot live purely inside managerd's own process.
`Reconciler.ensureVM` arms a revert (`ArmTapRevert`) for a newly
created uplink-bridged VM's specific tap immediately after it joins
the shared bridge; on a *later* tick, once that VM is observed still
running and reconciling cleanly, `ConfirmBridgeHealthy` cancels it -
deliberately not confirmed immediately at creation time, so the window
actually covers a guest's post-boot traffic (e.g. a broadcast storm),
not just the moment the tap is created before the guest OS has even
started. The scheduled revert action (`ifconfig <bridge> deletem
<tap>`) only ever removes the single named tap, never the uplink NIC
itself - worst case after a revert, that one VM loses network and
nothing else does; the host's own management access is never at risk.

## Not addressed (existing, predate this ADR)

- No `UpdateNetwork` RPC exists - `uplink_bridged` is a create-time-only
  choice, same as `vlan_id`/`subnet` today; changing it requires
  delete+recreate.
- `DeleteNetwork` still doesn't tear down bridge/VLAN interfaces in
  general for networks that do own them (ADR-0047's own "Not
  addressed" section) - explicitly *not* a new risk for this mode
  specifically, since `ensureUplinkBridgedNetwork` never sets
  `OwnBridge`/`OwnVLAN`/`OutboundNAT`, so there is nothing of this
  mode's own to leak; the pre-existing host bridge was never Apiary's
  to begin with, and `DeleteNetwork` correctly never touches it.
- No web/CLI-level confirmation UX exists yet for creating an
  `uplink_bridged` network specifically (beyond the install-time
  report-only check) - a follow-up, not a wire-protocol concern.

## Verification

Unit tests added alongside existing patterns:
`internal/raft/fsm_test.go` (the three mutual-exclusion rejections, and
that IP allocation is skipped and never exhausts even on a tiny
subnet), `internal/cluster/reconciler_test.go`
(`ensureUplinkBridgedNetwork`'s opt-in/bridge-exists/success paths
against a fake `vlanManager`, a full `RunOnce` end-to-end check that a
VM's tap joins the node's own bridge with no VLAN/address/NAT calls
made, and a regression test proving `reconcileNetworkArtifacts` never
destroys the shared bridge when such a network is deleted),
`internal/deadman/manager_test.go` (arm/confirm/re-arm idempotency
against a fake `at`/`atq`/`atrm` runner), and
`internal/manager/integration_test.go` (a full RPC round trip through
`CreateNetwork`/`ListNetworks`/`DeleteNetwork`, and that the FSM's
validation is reachable through the real RPC layer). `go build ./...`,
`go vet ./...`, `go test ./...`, and `gofmt -l .` all pass.

Live verification against `apiverse`/`apiarium` is pending - to be
recorded here once run, following ADR-0047/ADR-0048's own precedent of
documenting exactly what was observed live: enabling
`allow_uplink_bridging` on one node at a time, arming/observing the
dead-man's switch in isolation before any real VM exists, then with one
disposable test VM confirming it receives a real DHCP lease from the
physical LAN's own router while host management connectivity stays
uninterrupted throughout, then deliberately testing the revert path by
stopping managerd before it can confirm health.
