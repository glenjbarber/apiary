# ADR-0044: every VM gets a deterministic MAC, not just network-attached ones

## Status

Accepted

## Context

`internal/raft`'s `applyCreateVM` only computed a stable, `deriveMAC(id)`-
derived MAC address for a VM naming a `NetworkDefinition` (ADR-0022). A
flat-bridge VM (no `network_id`) got whatever random MAC `bhyve`'s own
`virtio-net` device generated on each fresh create - `internal/cluster`'s
`ensureVM` explicitly discarded any MAC for that case
(`macAddress = vm.MACAddress` lived inside the `if vm.NetworkID != ""`
branch).

This became a real, concrete problem while setting up real Kubernetes
CAPI live verification (see ADR-0042/ADR-0043): the only working
networking path on the second bhyve-capable node (`apiverse`) is a flat
bridge reaching the real home LAN's own DHCP server - not one of
Apiary's own managed `NetworkDefinition`s. `KubeadmControlPlane` (real
upstream Cluster API machinery) refuses to render any bootstrap data at
all until `Cluster.spec.controlPlaneEndpoint` is set to a real, known
IP - impossible to know ahead of time for a VM getting a dynamic DHCP
lease with a random MAC. The user's own resolution: reserve a static
DHCP lease on the router itself, keyed by MAC - which only works if the
VM's MAC is knowable *before* the VM is ever created, i.e. deterministic
from something the caller already controls (the VM's own id).

Building Apiary's own VLAN/DHCP/`pf` network management (ADR-0022) on
`apiverse` - matching `apiarium`'s own existing setup - was considered
and explicitly rejected in favor of the router-side reservation: it
would have meant standing up a second node's worth of `dnsmasq`/`pf`
anchor/`-vlan-uplink` configuration (real, non-trivial infrastructure
work) just to get a predictable IP, when a static reservation on
hardware the operator already controls achieves the identical result
with far less Apiary-side surface area - and, crucially, still needed
this exact fix regardless, since MAC-keyed reservations are only useful
once a VM's MAC is knowable ahead of time in the first place.

## Fix

`deriveMAC(id)` (already a pure, id-only function - no networking
concepts inside it at all) is now called unconditionally in
`applyCreateVM` for every new VM, moved out of the `network_id != ""`
branch (which still gates IP allocation, correctly - Apiary has no
address pool to draw from for a flat-bridge VM, only a MAC). This
means:

- Any operator can compute a VM's future MAC themselves, before ever
  creating it, using nothing but its planned `id` string and the exact
  same algorithm (SHA-256, clear the multicast bit, set the
  locally-administered bit, per IEEE 802) - and set up a router-side
  static DHCP reservation for that MAC ahead of time.
- `internal/cluster`'s `ensureVM` now passes `vm.MACAddress` to
  `bhyve.Config.MACAddress` unconditionally too, instead of only when
  `NetworkID` is set - `bhyve.Config.MACAddress` was already a plain,
  optional pass-through (`,mac=<addr>` appended to the `virtio-net`
  device string whenever non-empty), so this required no change to
  `internal/bhyve` itself.

## UI gap closed in the same pass

`internal/frontend`'s `vmView.MACAddress` was already populated (from
`fromRPCVM`) but never actually rendered anywhere - the VM table
(`vm_rows.html`) showed `IPAddress` but had no MAC column at all, so
even a network-attached VM's MAC (previously always deterministic, per
ADR-0022) was invisible in the UI. Added a MAC column to the VM table
(`vms.html`'s header, `vm_rows.html`'s row) - now every VM's real MAC
is visible in the same place an operator already looks for its IP.

## Verification

New unit tests: `TestFSM_Apply_CreateVMWithoutNetworkStillGetsMAC` /
`TestFSM_Apply_CreateVMWithoutNetworkMACIsDeterministic` (mirroring the
existing network-attached equivalents, confirming a flat-bridge VM now
gets a real, stable MAC and that it's reproducible across independent
FSM instances given the same id - the same raft-replication-safety
property the network-attached case already required),
`TestReconciler_RunOnce_FlatBridgeVMStillGetsMACAddress` (confirming
the reconciler actually passes it through to `bhyve.Config` for a VM
with no `NetworkID`), and `TestServer_VMsPage_ShowsMACAddress`
(confirming the new UI column actually renders it).

Live-verified for real, end to end, deployed to both raft voters
(`apiarium`/`apiverse` - a real gap caught mid-verification: `deriveMAC`
lives in `internal/raft`, `raftd`'s own binary, not `managerd`'s; the
first live attempt returned an empty MAC because only `managerd`/
`frontend` had been redeployed with the fix, not `raftd` on either
voter). Precomputed `deriveMAC("capi-f21194fcd6cb")` (the exact VM id
the `cluster-api-provider-apiary` controller's own `apiaryMachineVMID`
hash produces for `default/apiary-cp-1`) as `4e:50:cb:24:ab:af` *before*
creating anything, handed it to the operator for a real static DHCP
reservation on their own router (keyed by MAC, resolving to
`10.50.0.50`), then created the real `Cluster`/`ApiaryCluster`/
`Machine`/`KubeadmConfig`/`ApiaryMachine` set referencing that exact
pre-reserved IP as `Cluster.spec.controlPlaneEndpoint` - confirmed the
real VM `CreateVM` produced (`capi-f21194fcd6cb`) received precisely
`4e:50:cb:24:ab:af`, an exact match with zero manual adjustment needed.
See the `cluster-api-provider-apiary` repo's own docs for the fuller
CAPI-side trail this unblocked (the `ApiaryMachineSpec.StaticIPAddress`
addition, and the first real, non-bypassed `kubeadm` bootstrap-provider
round trip).
