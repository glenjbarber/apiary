# ADR-0066: Host-default egress contract for managed Cells

## Status

Accepted

## Context

Apiary-managed networks already have a self-hosted outbound path: when a
network has no `ExternalGateway`, the owning Hive installs a per-network
PF anchor that NATs the Cell subnet through the Hive's NAT uplink
(ADR-0048). The remaining operational gap was that the required host
inputs were split between hand-edited `rc.conf` flags and the Apiary
Machine page. In particular, a Hive could be correctly NATing traffic
while DHCP still handed Cells no usable DNS server.

That split made a newly created Cell appear broken after a managerd
restart, and encouraged host-specific workarounds such as inventing a
shared interface name. A host abstraction is useful, but it must not hide
which concrete interface PF and the FreeBSD routing table actually use.

## Decision

The default egress contract is explicit and node-local:

1. The Hive operator supplies a VLAN-tagging uplink and, when necessary,
   a separate NAT uplink. An empty NAT uplink falls back to the VLAN
   uplink, preserving the existing bootstrap behavior.
2. Apiary owns only its `apiary/*` PF anchors. The host owns forwarding,
   its default route, and the global PF policy. Apiary never rewrites the
   host's global rules.
3. The Hive operator supplies a DHCP DNS server alongside those interface
   settings. Apiary persists it in the node-local configuration and uses
   it when rendering dnsmasq option 6 for every managed network.
4. Network intent remains topology-independent: an empty
   `ExternalGateway` means self-hosted NAT through the Hive; a populated
   `ExternalGateway` means the network uses that real router instead.

The Machine Configuration page is now the durable source for all three
node-local network inputs (VLAN uplink, NAT uplink, and DHCP DNS server).
The values are applied on the next managerd restart, and the page states
that explicitly. Startup flags remain bootstrap defaults for a fresh node
that has no saved node configuration.

The Machine Configuration page now presents the VLAN uplink and NAT uplink
as separate dropdowns populated from the answering Hive's live host
interface inventory. Each option includes its up/down state and addresses.
An interface saved in node-local configuration but no longer present is
retained as a marked unavailable option, so an operator can select `(unset)`
and clear it without editing JSON by hand. Interface discovery is advisory:
the host remains responsible for deciding whether an interface is suitable
for VLAN tagging or NAT.

## Why there is no implicit `defaultif0` in this version

FreeBSD PF's `nat-to` rule and the kernel's default route both ultimately
need a concrete interface. A synthetic `defaultif0` would require a
second host-side service to create it, route it, and keep it synchronized
with the real WAN interface. That would add another failure domain while
making the actual egress less observable. The existing `NATUplink` field
is the narrower, inspectable host-default contract. A host may still expose
a stable real interface or bridge, such as `bridge999`, and select it in the
NAT uplink dropdown. The host must give that interface the actual L3 address,
default-route path, and PF/NAT behavior. Apiary does not infer or create that
host topology, and `ifconfig_DEFAULT` remains a FreeBSD configuration
fallback rather than an interface or route selector.

## Safety and failure behavior

- Missing forwarding or an unsuitable host PF policy remains visible as a
  host/network failure; Apiary does not silently broaden global firewall
  rules.
- Missing DHCP DNS configuration is visible on `/machine` and results in
  no DHCP option 6, rather than advertising a dead-end resolver.
- Node-local settings are never replicated through Raft because interface
  names and reachable resolvers are meaningful only on their own Hive.

## Verification

The node-config JSON, manager RPC, Machine page, and managerd startup
override are covered by unit tests. Existing reconciler tests continue to
verify that self-hosted NAT is applied per network and that external
gateway networks do not receive an Apiary NAT rule.
