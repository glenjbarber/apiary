# ADR-0048: Self-hosted outbound NAT for Apiary-managed networks

## Status

Accepted

## Context

ADR-0047 solved a duplicate-IP conflict by letting a network share a
VLAN with a real external router (`skyview`, a separate hand-configured
FreeBSD firewall) for real internet-routed DNS/connectivity - needed so
a guest VM's `kubeadm init` could actually pull images.

Mid-implementation, explicit user guidance reset the requirement:
**Apiary itself must be self-managed/self-hosted, and must not depend
on the user's particular network topology** (their VLANs, physical
switch config, or a specific external router). Hardware/physical
accommodations are fine; a hard architectural dependency on a specific
piece of home-network infrastructure is not. ADR-0047's shared-VLAN
design fails that test - it works only because this specific network
happens to have `skyview` sitting on the same trunk.

## Decision

Give Apiary-managed networks real internet access on their own, the
same way any home router does: NAT the network's own subnet out through
the node's own existing internet-facing interface.

- `internal/pf.Manager` gains `ApplyNAT(ctx, anchor, subnet, uplink)`,
  loading `match out on <uplink> from <subnet> to any nat-to (<uplink>)`
  into a per-network anchor (`apiary/net-<id>`, a flat sibling of the
  existing `apiary/vm-<id>` per-VM anchors, covered by the same
  `anchor "apiary/*" all` reservation with no extra host config).
- `Reconciler` gains an `Uplink` field; `ensureNetwork` calls `ApplyNAT`
  whenever a network has no `ExternalGateway` (ADR-0047) and `Uplink`/
  `PF` are set - the same "this node's own bridge is the gateway"
  branch that already assigns `.1` to the bridge.
- New `cmd/managerd` flag `-nat-uplink`, defaulting to `-vlan-uplink`'s
  value. Real host prerequisite (disclosed, not managed by Apiary,
  matching `nmdm.ko`/`dnsmasq`/`fdescfs` precedent): `gateway_enable="YES"`
  in `/etc/rc.conf` (`net.inet.ip.forwarding=1`) - without it, a NAT'd
  packet is translated but the kernel never routes it anywhere.

`ExternalGateway` (ADR-0047) is not removed - it remains available for
a deployment that genuinely wants to integrate with existing network
infrastructure - but self-hosted NAT is now the default, recommended
path, requiring nothing beyond the node's own existing internet
connection.

## Real bugs found only by live testing

**The VLAN-tagging uplink and the NAT-egress interface can be two
different interfaces.** `apiarium`'s `-vlan-uplink re0` is correct for
VLAN tagging (`vlandev re0`), but `re0` had already been bridged into
`bridge0` for unrelated flat-VM-networking work earlier in this
project's history - meaning `re0` itself carries no IPv4 address, and
the node's actual default route egresses via `bridge0`. A NAT rule
written against `re0` silently never matched any real outbound traffic
(no error - the rule just never fired), which is exactly why `-nat-uplink`
exists as a separate, overridable setting rather than always reusing
`-vlan-uplink`'s value.

**Network infrastructure was only ever provisioned once, at VM
creation.** `ensureNetwork` (bridge address, NAT rule) was only called
from the "VM not yet running" branch of `ensureVM` - an already-running
VM returned early after reapplying only its firewall rules. This meant
a network-level config change (like the `-nat-uplink` fix above) never
took effect for any VM that was already running on that network, only
for one recreated from scratch - confirmed live: setting `-nat-uplink
bridge0` and restarting `managerd` did not change the already-running
VM's stale `nat-to (re0)` rule at all. Fixed by moving the network-
provisioning block ahead of the running-VM early return in `ensureVM`,
so it runs every tick regardless of whether this specific VM was just
created or has been running for a while - the same reasoning
`internal/pf`'s per-VM firewall-rule reapplication already established
for an unrelated but structurally identical reason.

**Changing a network's `vlan_id` (via delete+recreate, since
`NetworkDefinition` has no Update RPC) leaves the old VLAN interface
bridged.** `EnsureMember` only adds members, never removes stale ones -
recreating `k8s-workers` with `vlan_id=61` (replacing the old `vlan_id=60`
from ADR-0047's now-abandoned design) left `vlan60` still bridged
alongside the new `vlan61`, silently reintroducing the exact duplicate-
`.1` conflict ADR-0047 fixed, on the same node, for no reason anyone
would notice without checking bridge membership directly. Cleaned up
manually (`ifconfig <bridge> deletem vlan60`); not fixed in code - real,
disclosed gap, the same class of issue as ADR-0047's own "deleting a
network never tears down a node's bridge" note.

## Not addressed

- The vlan_id-change / stale-bridge-member gap above is not fixed in
  code - manual cleanup only.
- `internal/pf.ApplyNAT`'s anchor is never flushed on network deletion,
  same as `internal/vlan`'s own bridge - see ADR-0047's own "Not
  addressed" section.
- No egress traffic shaping/rate limiting - NAT is unconditional and
  unrestricted once enabled.

## Verification

New unit tests in `internal/cluster/reconciler_test.go`:
`TestReconciler_RunOnce_AppliesOutboundNATForSelfHostedNetwork`,
`TestReconciler_RunOnce_NoNATForExternalGatewayNetwork`, and
`TestReconciler_RunOnce_ReconcilesNetworkForAlreadyRunningVM` (the
regression guard for the running-VM gap above).

Live-verified on `apiarium` up through the host/network layer: recreated
`k8s-workers` as a plain self-hosted network (`vlan_id=61`, no
`ExternalGateway`), confirmed `pfctl -a apiary/net-k8s-workers -s rules`
shows the expected `nat-to (bridge0)` rule (after finding and fixing the
`-vlan-uplink`-vs-egress-interface bug above), and confirmed
`net.inet.ip.forwarding=1`.

**Not yet verified end-to-end**: a live guest VM's `kubeadm init` still
fails identically to before (`lookup registry.k8s.io on 127.0.0.53:53:
server misbehaving`), but packet captures on the guest's own `tap`
interface show **zero outbound DNS queries ever leave the guest at
all** - ruling out NAT/pf/routing on the Apiary/host side as the cause
(there's nothing on the wire for it to translate). The same captures
surfaced a separate, unrelated, real bug worth its own fix: dnsmasq's
static `dhcp-host` reservation is never honored - every VM this session
(across every network/VLAN/gateway configuration tried) received a
effectively-random pool address (e.g. `10.60.0.237`) instead of the
`VMDefinition.ip_address` the FSM actually assigned and dnsmasq was
told to reserve. The current best hypothesis for the DNS failure itself
is that the custom Kubernetes-ready base image's own
`/etc/systemd/network/05-dhcp-mac.network` override (built in an
earlier session specifically to fix a different DHCP-client-identifier
problem - see ADR-0045) does not set `UseDNS=yes`, so systemd-resolved
never learns of *any* upstream nameserver regardless of what dnsmasq
sends via DHCP option 6 - consistent with changing that option's value
having no effect at all. This is unconfirmed (no shell access to the
guest to check directly) and, if true, lives in the base-image build
tooling, which is deliberately not part of this repository or
`cluster-api-provider-apiary` (see ADR-0045's own "Not part of this
codebase" section) - so it cannot be fixed here.
