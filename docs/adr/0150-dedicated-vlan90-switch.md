# ADR-0120: Dedicated VLAN 90 switch for cluster nodes

Date: 2026-09-28

Status: Accepted

## Context

During the initial VLAN 90 build (see session handoff 2026-09-16), `brood`
and `drone` were connected to ports 5 and 6 of the single host-side Netgear
GSS108E (`switch1.home.arpa`, 10.50.0.6).
That switch carries traffic for VLANs 1, 60, 61, and 62 as well.
As the cluster grows, sharing a switch with the main LAN adds operational
risk: a misconfigured VLAN membership or PVID on a shared switch can silently
deliver cluster traffic onto the plain LAN, or vice versa.
A second Netgear GSS108E was available in the unallocated inventory.

## Decision

Dedicate a second Netgear GSS108E (`switch2.lab3.home.arpa`) exclusively to
VLAN 90.
All eight of its ports are untagged access ports with PVID 90.
It uplinks to port 8 of `switch1.home.arpa`, which is configured as a VLAN 90
untagged access port with PVID 90.
The cluster nodes (`brood`, `drone`, and any future VLAN 90 node) connect only
to `switch2`.
`switch1` carries no cluster node ports directly.

Management of `switch2` is via DHCP reservation at 10.90.0.2 (MAC
C0:FF:D4:D4:BA:83) on the VLAN 90 segment.

## Consequences

- Cluster node ports are physically isolated from the mixed-VLAN switch.
  A VLAN misconfiguration on `switch1` cannot expose cluster traffic to other
  segments unless port 8 itself is misconfigured.
- Adding a cluster node requires only connecting it to `switch2`; no changes
  to `switch1` are needed.
- The uplink between the two switches is a single gigabit port (port 8 on
  `switch1`).
  All VLAN 90 traffic between the cluster and the rest of the network shares
  that link.
  This is not a bottleneck for the current two-node cluster, but it is a
  ceiling to be aware of before adding high-bandwidth workloads.
- `switch2` has six free ports after the uplink and the two current nodes,
  leaving room for up to six additional nodes without hardware changes.
