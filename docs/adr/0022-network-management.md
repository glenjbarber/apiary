# ADR-0022: VLAN networks, real DHCP-backed IP allocation, and per-VM firewall

## Status

Accepted

## Context

Every VM before this ADR got, at most, a single flat NIC on one pre-
existing bridge (`bhyve.Config.Bridge`/`Reconciler.Bridge`/
`-bhyve-bridge`) — no VLAN isolation, no IP assignment (a guest got
nothing unless it DHCPed against something external to Apiary
entirely), and no per-VM firewalling. This was flagged as a deliberate
v1 gap as far back as ADR-0010, and was the single largest structural
difference between Apiary and Proxmox identified in an explicit
feature-comparison exercise. The user asked for all three — VLAN/bridge
management, IP allocation, and per-VM firewall rules — in one pass,
confirming along the way that IP allocation should mean a real DHCP
server (not just bookkeeping), firewall rules should be a simple
allow/block list, and that `apiarium`'s uplink NIC is `re0` (the
project's three FreeBSD VMs use `em0` — uplink names differ per node,
confirming this has to be a per-node config value, not hardcoded).

## New concept: `NetworkDefinition`

A `Network` is a named, cluster-wide L2 segment: a VLAN ID (0 =
untagged) plus a subnet (CIDR). *Which networks exist and their VLAN/
subnet* is a small coordination fact that must agree across the whole
cluster — two nodes must never double-assign an IP or a VLAN tag — so
it's ephemeral state, replicated through raft exactly like
`VMDefinition` (new `NetworkDefinition` message, `CreateNetwork`/
`DeleteNetwork` command variants, `FSM.networks` map, new `GetNetwork`/
`ListNetworks` read RPCs on `RaftInternal` mirroring `GetVM`/`ListVMs`).
*Creating the actual `vlan(4)`/`bridge(4)` interfaces* a network implies
is physical, per-node work, done by `internal/cluster`'s `Reconciler`
(via a new `internal/vlan` package) exactly the way it already
provisions ZFS datasets and bhyve VMs — never stored in raft.

`DeleteNetwork` has no soft-delete tombstone (unlike `DeleteVM`): a
network has no physical resources of its own to reconcile away first,
so it's rejected outright (not applied) if any VM still references it —
no cascade, matching the same "no orphan-reclaim" caution already
accepted for VM deletion (ADR-0016).

## IP and MAC assignment happens in the FSM, not managerd

A VM optionally references `network_id`; if set, `applyCreateVM` itself
(not `managerd`'s RPC handler, not the reconciler) assigns `ip_address`
and `mac_address` before storing the record. This has to happen inside
the FSM specifically: raft's log serializes every `CreateVM` command
through the same `Apply` path, so every replica computes the identical
allocation independently from the same already-committed state — safe
without a separate allocation round-trip or lock. Doing it in
`managerd` instead would let two concurrent `CreateVM` calls against
different (or even the same) leader race and hand out the same IP.

IP allocation scans existing VMs on the same network for used
addresses and picks the lowest free host address, skipping the network
address, the broadcast address, and `.1` (reserved for the bridge's own
gateway address — see `internal/vlan.EnsureBridgeAddress`). MAC
addresses are derived deterministically from `sha256(vm_id)`, masked
into a locally-administered unicast address (IEEE 802's `0x02` bit) —
no allocation bookkeeping needed, stable across reconciler ticks and
FSM restarts, since it's a pure function of the VM's own id.

## Bridge names are hashed, not `"apiary-net-<id>"` — a real bug caught live

The first implementation derived a network's default bridge name as
`"apiary-net-" + id`. Live testing on `apiarium` immediately broke this:
creating a network named `net-1` produced the interface name
`apiary-net-net-1` (16 characters), and `ifconfig` rejected it —
`ioctl SIOCSIFNAME: File name too long`. FreeBSD interface names are
capped at `IF_NAMESIZE` (16 bytes including the trailing NUL, so 15
usable characters), and a network id of arbitrary caller-chosen length
can't be embedded directly and still fit. Fixed by hashing instead:
`fmt.Sprintf("apnet-%x", sha256(id)[:4])` — always exactly 14
characters regardless of id length. A regression test
(`TestNetworkBridgeName_FitsFreeBSDInterfaceNameLimit`) checks this
against a deliberately long id so the same class of bug can't return
silently. The Networks page shows the auto-generated name as
`(auto-generated)` rather than a fabricated guess, since the real
value is only knowable by asking a specific node (see below).

## Real DHCP via `dnsmasq`, not bookkeeping-only IP allocation

The user explicitly chose a real DHCP server over pure bookkeeping, so
IP allocation actually reaches a VM's guest OS. `internal/dhcpd`
renders a `dnsmasq.conf` (one `interface=`/`dhcp-range=` pair per local
network, one `dhcp-host=` static lease per VM with an assigned IP) and
restarts the `dnsmasq` service to apply it — a restart, not a reload
signal, for the same "no reliable hot-reload" reason `internal/hast`
already documented for `hastd`. `port=0` and `bind-interfaces` are set
unconditionally: Apiary only wants DHCP, and leaving `dnsmasq`'s DNS
resolver enabled risks it answering queries on an interface/network it
was never meant to serve. This requires `pkg install dnsmasq` on each
bhyve-capable node — a new external dependency, confirmed not installed
on `apiarium` before this work — the same class of one-time host
prerequisite as `bhyve-firmware`/`edk2-bhyve` already are.

## Per-VM firewall via `pf(8)` anchors, one anchor per VM

`internal/pf` renders a VM's `FirewallRule` list (direction: in/out;
action: pass/block; protocol: tcp/udp/icmp/any; port or port range)
into real `pf.conf` syntax and loads it into a dedicated anchor
(`apiary/vm-<id>`) via `pfctl -a <anchor> -f -`, replacing the anchor's
entire ruleset each time — the same full-replace convention
`internal/hast`'s `WriteConfig` already uses. No rules means an empty
anchor (everything allowed), matching this project's de facto behavior
before firewall support existed, so no existing VM's behavior changes.
Firewall rules are re-applied on *every* reconcile tick, even for an
already-running VM (unlike disk/network config, which is fixed at
bhyve launch) — a rule change takes effect without recreating the VM.
This requires pf enabled on the host with an anchor point reserved for
Apiary in `/etc/pf.conf` (`anchor "apiary/*"`) — confirmed no
`/etc/pf.conf` existed on `apiarium` before this work, though `pfctl`
itself is present as part of the base system. Enabling pf host-wide is
a real, once-per-node security-relevant change; it was done with
explicit confirmation before touching `apiarium`, using a minimal
anchor-only ruleset with no default-block rules so existing traffic on
the host was unaffected (confirmed live: SSH kept working immediately
after enabling).

## `pflog` was also enabled while working on this

The user asked for `pflog` (pf's packet-logging pseudo-interface) to be
enabled alongside pf itself, for future log-based troubleshooting.
`pflogd` starts and runs correctly on `apiarium`, but the traditional
`ifconfig pflog0` interface never becomes visible - FreeBSD 16.0-
CURRENT's `ifconfig` explicitly reports "pflog(4) logging does not need
interface creation in FreeBSD 16.0", a platform-version quirk, not a
misconfiguration. `pflogd`'s own process state and `/var/log/pflog`'s
existence are what confirm it's actually working on this particular
FreeBSD version.

## Host Stats page gained a live pf summary

Prompted by having pf running at all for the first time, the Host
Stats page (ADR-0018) now shows a `pfctl -s info`-derived summary:
whether pf is enabled, its current state-table size, and cumulative
rule-match count — `internal/hoststats` gained a `gatherPF` step
following the exact same best-effort pattern every other subsystem
there already uses (a failure here doesn't blank out CPU/memory/ZFS/
disk/network, and vice versa).

## The Networks page shows real, per-node bridge status — colored

The Networks page lists each network's actual bridge interface state
on the answering node (`up`/`down`/`unknown`), not just its static
definition — physical, per-node data computed on demand via a new
`VLANStatus` interface (`internal/vlan.Manager.InterfaceStatus`,
parsing `ifconfig`'s own `UP` flag), wired into `ListNetworks` the same
opt-in nil-able pattern already used for `VNCLookup`/`Bhyve`/`ISOs`.
Per the user's explicit ask, `up` renders in green and `down` in red
(reusing the existing `.success`/`.error` CSS classes), with `unknown`
shown plainly when this node has no VLAN support configured or the
bridge doesn't exist here yet (e.g. no VM on this network has reached
this node). This is why `NetworkDefinition`'s external RPC type
(`api/rpc/manager.proto`) carries a `bridge_status` field that the
internal ephemeral-state schema (`api/internalpb`) deliberately does
not: it's physical, computed, per-node-caller data, not a cluster-wide
fact — the same ephemeral/physical split this project has followed
throughout, just expressed as an extra read-only field on the RPC type
rather than a whole separate message.

## Consequences

- Verified with unit tests across every new package
  (`internal/vlan`, `internal/dhcpd`, `internal/pf`), FSM-level tests
  (`internal/raft`: network CRUD, IP allocation determinism/exhaustion/
  skip-used-IPs, MAC derivation stability, delete-blocked-while-
  referenced), reconciler tests (`internal/cluster`: network ensure/
  attach/teardown, firewall apply-on-create and re-apply-when-already-
  running, DHCP lease aggregation and no-op-when-unchanged), and
  integration tests against a real raft harness
  (`internal/manager`: full `CreateNetwork`/`ListNetworks`/
  `DeleteNetwork` round trips, a VM getting a real IP/MAC through the
  full stack, and bridge-status reporting).
- Verified live end-to-end on `apiarium` (`re0` uplink): created a
  network and a VM on it through the real UI; confirmed the `vlan100`
  tagged interface and `apnet-<hash>` bridge actually exist, the
  bridge carries the `.1` gateway address, the VM's tap is a bridge
  member, `dnsmasq` is serving the exact static lease from the UI form,
  `pfctl -a apiary/vm-<id> -s rules` shows the exact rule entered in
  the UI (with live match counters incrementing), and the Networks/
  Host Stats pages show the real bridge-up state and pf counters,
  colored as asked.
- `dnsmasq` and pf are now real host-level dependencies/state changes
  on every bhyve-capable node running network management - documented
  prerequisites, not something Apiary installs/enables itself (same
  posture as `bhyve-firmware`, the ZFS pool, and the uplink NIC name
  already being assumed present).
- Still v1-scoped in ways worth naming: `internal/dhcpd`'s subnet
  arithmetic only supports `/24`-or-smaller networks (documented, not
  silently wrong - anything larger is rejected outright); firewall
  rules are a flat allow/block list with no priority/ordering system
  beyond pf's own top-to-bottom last-match evaluation; a VM's console/
  network management both still assume the querying `internal/frontend`
  is colocated with the answering `managerd` (same real-multi-node gap
  ADR-0020 already noted for the console).
