# ADR-0050: dnsmasq's per-network DHCP options never worked - `tag:`, not `interface:`

## Status

Accepted

## Context

Every DNS/DHCP-option investigation across ADR-0047, ADR-0048, and this
session's own extensive live debugging (the shared-VLAN duplicate-IP
conflict, the pivot to self-hosted NAT, the stale-lease bug) assumed the
underlying option-delivery mechanism itself worked once the *value* was
correct. It never did. `internal/dhcpd.RenderConfig` scoped both the DNS
server (option 6, added to fix "every VM gets a dead-end DNS server")
and the external gateway (option 3, ADR-0047) to a specific bridge using:

```
dhcp-option=interface:<bridge>,6,<value>
dhcp-option=interface:<bridge>,3,<value>
```

`interface:` is not a valid dnsmasq scope selector for `dhcp-option` -
unlike `dhcp-range=`/`interface=`, which *do* accept a literal interface
name. dnsmasq silently accepted the malformed directive and never
applied it: no parse error, no warning, no option 6 or 3 in any DHCP
reply, ever, on any network that ever set `DNSServer`/`Gateway` - since
the feature was first added.

## How this was finally found

While rebuilding the Kubernetes-ready base image (a fresh `ubuntu-cloud.raw`
boot, no custom guest-side network config involved at all) hit the exact
same `apt-get`/DNS failure as every previous attempt. `resolvectl status`
showed `Current Scopes: none` for the guest's only interface - DHCP had
assigned the IP and gateway correctly, but zero DNS servers were ever
registered. A live packet capture of the DHCP exchange settled it: the
guest's own DHCPDISCOVER explicitly requested option 6 in its parameter
list, and dnsmasq's ACK reply came back with no option 6 at all, despite
`dnsmasq.conf` clearly containing `dhcp-option=interface:apnet-a8eb99cf,6,1.1.1.1`.

Editing the live config to `dhcp-option=tag:apnet-a8eb99cf,6,1.1.1.1` and
restarting dnsmasq fixed it immediately, confirmed on the next capture
(`Domain-Name-Server (6), length 4: 1.1.1.1` now present in the ACK) and
via the guest actually resolving `archive.ubuntu.com` and reaching
`pkgs.k8s.io` over HTTPS.

dnsmasq automatically tags every DHCP request with the name of the
interface it arrived on - `tag:<interface-name>` is the documented way
to scope an option to one interface's clients. `interface:` doesn't
exist as a `dhcp-option` selector at all.

## Consequence

This single bug is the common root cause underneath every DNS symptom
chased this session, on top of (not instead of) the other genuine,
separately-confirmed bugs found along the way:

- The original "every VM gets a dead-end DNS server" fix (this session's
  first DNS fix, before the VLAN-60/skyview work) never actually took
  effect - dnsmasq kept falling back to its own default self-
  advertisement the whole time, which is *why* that default's dead end
  was never actually replaced by anything, on any network, ever.
- The self-hosted NAT work's own DNS-server value changes (`10.60.0.1`
  then `1.1.1.1`) each *appeared* to have "no effect" - correctly
  observed, but for the wrong reason assumed at the time (network-
  reconciliation-only-runs-once, ADR-0048) - the real reason is this
  option was never being emitted onto the wire at all, so no value
  handed to `DNSServer` could ever have worked.
- ADR-0047's `Gateway`/option-3 fix has the identical bug and was
  therefore equally non-functional, though the VLAN-60 investigation
  never got far enough to isolate that separately before the pivot to
  self-hosted NAT made it moot for that specific network.

## Fix

`internal/dhcpd.RenderConfig` now emits `dhcp-option=tag:<bridge>,...`
for both the DNS server and gateway options. No other behavior changes.

## Verification

Existing `TestRenderConfig_DNSServerOptionScopedToInterface`/
`TestRenderConfig_GatewayOptionScopedToInterface` updated to assert the
correct `tag:` prefix. Live-verified on `apiarium`: manually patched the
running `dnsmasq.conf`, captured a live DHCP renewal showing option 6
now present in the ACK, and confirmed the guest resolves real hostnames
and reaches the internet over HTTPS - then the code fix was made to
match and redeployed.
