# ADR-0047: NetworkDefinition.ExternalGateway - sharing a VLAN with a real router

## Status

Accepted

## Context

Every Apiary-managed network's per-node bridge unconditionally claims
the subnet's first host address (".1") as its own gateway
(`internal/vlan.Manager.EnsureBridgeAddress`) - sound when Apiary's own
`dnsmasq` (ADR-0022) is the only thing serving that subnet, but this was
the first network built to share a real, hand-configured external
router (`skyview`, a separate FreeBSD firewall on the same physical
switch trunk) on the same VLAN, specifically so VMs get real internet
routing/DNS that Apiary itself doesn't provide.

`skyview`'s own `vlan60` interface was configured at `10.60.0.1` - the
same address Apiary's reconciler was independently, unconditionally
assigning to its own bridge on `apiarium`, on the same L2 segment.

## Root cause of the observed symptom

A guest VM's DNS/routing queries to `10.60.0.1` never reached `skyview`
at all - confirmed via a live packet capture on `skyview`'s own `vlan60`
interface showing complete silence during a query attempt that was
independently confirmed (via the guest's own serial console) to have
just happened. `skyview`'s Unbound and `pf` configuration were both
separately confirmed correct (`drill` against Unbound directly returned
a correct answer; `pfctl -sr` showed the expected pass rule loaded) -
ruling out anything on `skyview`'s own side.

The real cause: `apiarium`'s own per-network bridge (`apnet-a8eb99cf`)
was *also* configured with `10.60.0.1/24`, on the same tagged VLAN 60
segment. Two hosts claiming the same address on one broadcast domain
means whichever host answers ARP for that address (in practice,
`apiarium` itself, being directly attached to the guest's own bridge)
intercepts traffic meant for the other - the guest's query never left
`apiarium` because `apiarium` believed itself to be `10.60.0.1`.

A second, compounding finding from the same investigation: an orphaned
bridge (`apnet-7d035157`, bridged to `vlan1`) from an earlier, already-
deleted network attempt this session was *also* squatting
`10.60.0.1` on the same node - a separate, previously-undiscovered gap
where deleting a `NetworkDefinition` never tears down the bridge/vlan
interfaces a node created for it. That orphan was removed manually
(`ifconfig destroy`, safe since no raft record referenced it); the
underlying teardown gap is not fixed by this ADR - see "Not addressed"
below.

## Decision

Add `NetworkDefinition.external_gateway` (optional, empty by default).
When set to a real router's address already serving this subnet:

- The reconciler's `ensureNetwork` skips `EnsureBridgeAddress` entirely
  for that network on every node - Apiary's own bridge exists purely
  for L2 forwarding (attaching VM taps and the tagged VLAN interface),
  never claiming an address that would conflict with the real gateway.
- `internal/dhcpd`'s `RenderConfig` emits `dhcp-option=<bridge>,3,<gateway>`
  (DHCP's router option) when `NetworkScope.Gateway` is set, so VMs are
  told to route through the real external gateway instead of dnsmasq's
  own default behavior of advertising the interface's own address -
  mirroring the DNSServer/option-6 fix from earlier this session
  exactly (same class of bug: a correct-looking default that's actually
  a dead end once a workload needs real external connectivity).

Empty `external_gateway` (the default) preserves every existing
network's behavior exactly - this is purely additive.

## Why not just move skyview off .1

`.1` is the conventional first-host address every other part of this
project already reserves as "the gateway" (`internal/raft`'s
`allocateIP`, `internal/dhcpd`'s `dhcpRange`, `internal/vlan`'s own
doc comments) - moving the *real* router off it would work but fights
that convention everywhere else, and `skyview`'s own configuration is
externally managed (Ansible), higher-friction to change than an Apiary
code path built exactly for extension points like this one.

## Superseded in part by ADR-0048

Live testing after this ADR landed surfaced a further constraint: this
project's own operating principle is that Apiary must be self-hosted
and not depend on any particular external network topology (a shared
VLAN trunk plus a hand-configured external router, as built here,
fails that test). ADR-0048 replaces `ExternalGateway`/shared-VLAN as
the default path for a network needing real internet access with
self-contained outbound NAT through the node's own uplink instead.
`ExternalGateway` itself is not removed - it remains a valid, purely
opt-in escape hatch for a deployment that genuinely wants to integrate
with existing network infrastructure - but it's no longer the
recommended approach for the common case.

## Not addressed

- The orphaned-bridge-on-delete gap noted above (`DeleteNetwork` doesn't
  tear down any node's physical bridge/vlan interfaces) remains open -
  not exercised again until a network is deleted while VMs are still
  live on some node, which hasn't happened in this project's own
  history until the stale bridge this ADR found.
- `internal/dhcpd`'s existing `DNSServer` is still a node-wide
  `Reconciler` field (a `-dhcp-dns-server` flag), not per-network like
  `ExternalGateway` is - fine for this project's own single-shared-VLAN
  deployment today, but a real inconsistency if a node ever reconciles
  two networks needing different DNS servers.

## Verification

New unit tests: `internal/dhcpd/config_test.go`'s
`TestRenderConfig_GatewayOptionOmittedByDefault`/
`TestRenderConfig_GatewayOptionScopedToInterface`, and
`internal/cluster/reconciler_test.go`'s
`TestReconciler_RunOnce_SkipsBridgeAddressForExternalGateway`/
`TestReconciler_RunOnce_ReconcilesDHCPGatewayForExternalGateway`.

Live verification is staged as a follow-up to this change: recreate the
`k8s-workers` network with `external_gateway=10.60.0.1` set, confirm
`apiarium`'s bridge no longer holds an `inet` address, and re-run the
`tcpdump`-on-`skyview` test that originally caught this - a real DNS
query should now actually arrive.
