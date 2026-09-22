# ADR-0117: Dedicated jail networking (VNET/epair)

## Status

Accepted (design). The first increment described in "Stage 1" below is
implemented. Stages 2+ are explicitly NOT implemented - see "What is
NOT in this increment."

## Context

Today every jail Apiary creates uses `ip4=inherit` - it shares the
owning Comb's own network stack directly, with no VNET, no dedicated
IP, no isolation, no firewalling of its own, and no per-jail interface
for the reconciler to manage. This was a deliberate, explicit v1 scope
decision (ADR-0007, "`ip4=inherit` for v1, not dedicated networking"):
jails and VMs were both new at the time, and proving the basic
create/list/remove jail lifecycle didn't need real networking to do
it. ADR-0007's own "Consequences" section already anticipated this
exact follow-up:

> Dedicated IP allocation/VNET jails, when they're needed, will likely
> want to reuse the ephemeral-state/node-ownership machinery already
> built for `VMDefinition` (an IP is exactly the kind of small,
> JSON-shaped fact raft already replicates) rather than being purely
> local jail configuration.

This ADR is that follow-up. The user's ask (2026-09-19) is for real
VNET jail addressing, routing, isolation, firewalling, and lifecycle
reconciliation, plus UI - while preserving bhyve's own `tap(4)`/bridge
model rather than assuming one network backend must serve both VMs and
jails. `net/if_pair-kmod` (a newer FreeBSD 15+ routed-VNET-jail kernel
module, still 0.0.1/unproven) was flagged as worth assessing alongside
stock `epair(4)`, but explicitly not a dependency to adopt without
strong justification.

### Where `ip4=inherit` is actually set today

Confirmed by reading the real code, not assumed: `internal/jail/manager.go`,
`Manager.CreateJail`, builds a plain `jail -c` command-line invocation
(no `jail.conf` file is ever written - Apiary has never used jail.conf
templating, contrary to what one might assume from FreeBSD convention).
Before this ADR's own change, that invocation always included the
literal argument `ip4=inherit` with no way to opt out. There is no
`vnet;` anywhere, no `epair(4)` anywhere, and no per-jail interface
concept anywhere in `internal/jail` or `internal/cluster/jail.go`
before this change - the claim in the original task description is
fully confirmed, not an oversight to second-guess.

### How VM networking works today (the precedent to reuse)

`internal/vlan.Manager` (`internal/vlan/manager.go`) is the per-node
driver for VLAN/bridge interface lifecycle: `EnsureVLAN` creates/finds a
`vlan(4)` sub-interface tagging `Uplink`, `EnsureBridge` creates/finds a
named `bridge(4)` interface, `EnsureMember` adds an arbitrary interface
name to a bridge, `EnsureBridgeAddress` assigns a subnet's gateway
address (`.1`) to the bridge. Critically, **none of this is VM-specific
already** - every method takes a bare interface name string. A VM's own
`tap(4)` device is created by `internal/bhyve.Manager.createTap`
(shells out to `ifconfig tap create` + `ifconfig <bridge> addm <tap>`
directly, not through `vlan.Manager` - bhyve has its own tiny
`ifconfig`-wrapping `runCmd`, mirroring `internal/vlan`'s own private
copy rather than sharing one, a pattern this project's `vlan.go` doc
comment calls out explicitly as intentional: "internal/hast/internal/bhyve/
internal/jail each keeping their own [runCmd] rather than sharing one").

The orchestration layer that ties these together is
`internal/cluster/reconciler.go`'s `ensureVM`/`ensureNetwork`: for a VM
naming a `network_id`, it calls `r.ensureNetwork` (which calls
`VLAN.EnsureVLAN`/`EnsureBridge`/`EnsureMember`/`EnsureBridgeAddress`
and returns a `networkArtifact{Bridge, VLANID, OwnBridge, OwnVLAN,
OutboundNAT}`), then passes the resulting bridge name into
`bhyve.Config.Bridge` for `CreateVM` to attach the VM's own tap to.

IP allocation is raft-replicated, deterministic, and computed once at
creation time: `internal/raft/fsm.go`'s `applyCreateVM` calls
`f.allocateIP(network)` when `vm.NetworkID != ""` (skipped for
`uplink_bridged` networks - ADR-0101 - since those are served by the
physical LAN's own DHCP, not Apiary's). `allocateIP` scans `f.vms` for
already-used addresses on that network and returns the lowest free host
address, skipping `.0` (network) and `.1` (reserved for the bridge's own
gateway, assigned by `EnsureBridgeAddress`). `MacAddress` is derived
deterministically from the VM's own ID via `deriveMAC`, independent of
networking mode.

**This confirms the design assumption the task asked to verify**:
`vlan.Manager` is already interface-agnostic, not VM-specific in any
way that would require a large unrelated refactor before jails could
reuse it. The reconciler-level orchestration (`ensureNetwork`,
bridge/artifact bookkeeping) is the layer that's VM-specific today,
and that's exactly the layer this ADR extends for jails.

### How VNET+epair(4) jail networking normally works (FreeBSD)

Standard FreeBSD practice, not something requiring implementation
research: a jail given `vnet;` in its creation parameters gets its own
independent network stack (its own routing table, its own interface
list, its own `pf` view if `pf` is jailed too) instead of sharing the
host's. One end of an `epair(4)` virtual Ethernet pair
(`ifconfig epair create`, which always creates both `epairNa` and
`epairNb` together, auto-numbered - there is no way to name a pair
directly the way `bridge(4)` can be created with `name=`) is handed to
the jail as its own interface via `vnet.interface=epairNb` at
`jail -c` time; jail(8) itself moves that interface into the jail's own
vnet. The other end (`epairNa`) stays on the host and joins a
`bridge(4)` interface, exactly parallel to how a VM's tap device joins a
bridge. Unlike `ip4=inherit`, a vnet jail's interface has no address of
its own at creation time - jail(8) has no `ip4.addr`-style parameter
for a vnet interface - so assigning an address is a separate step
(`jexec <jail> ifconfig <iface> inet <addr>/<prefix> up`) run once,
after `jail -c` returns.

### `net/if_pair-kmod` vs stock `epair(4)`

Assessed as asked, not adopted. `epair(4)` is a stock GENERIC-kernel
FreeBSD driver, has existed and been production-hardened for many
years, needs no separate kernel module build/load step, and already
does everything this design needs: a host-side end joinable to
`vlan.Manager`'s existing bridges, and a jail-side end for `vnet;`.
`if_pair` (`net/if_pair-kmod`) is a newer, routed (not bridged)
point-to-point interface pair - conceptually aimed at a different
topology (route-based jail networking without a bridge in the middle)
and is explicitly called out by the task as FreeBSD 15+, "still
0.0.1/unproven." There is no concrete reason found in this
investigation to prefer it: this design's bridge-based topology (mirror
bhyve's own tap/bridge model, as the task explicitly asked) doesn't
need what `if_pair` uniquely offers, and adopting a 0.0.1 external
kernel module as a dependency for the *default* jail networking path
would be a real, avoidable operational risk (module build/load on every
node, no long production track record, one more thing to break across
a `freebsd-update`). **Decision: `epair(4)`, not `if_pair`.** If a
future stage's needs (e.g. routed jail networking without a bridge, for
some isolation model bridging can't express) can't be met by `epair(4)`
+ `pf`, `if_pair` should be re-evaluated then, once it has more of a
production track record - not adopted speculatively now.

## Decision

### Target design (full picture, staged - see "Rollout" below)

- **Addressing**: mirrors `VMDefinition` exactly, not a new scheme.
  `JailDefinition` gains `network_id` (which `NetworkDefinition` this
  jail's VNET interface belongs to) and `ip_address` (FSM-assigned, not
  caller-set, deterministic - same `allocateIP` used for VMs, extended
  to also treat jail addresses as used so a VM and a jail on the same
  network can never collide). No DHCP-inside-the-jail option in this
  design: Apiary's own raft-replicated allocation is the existing
  precedent (VMs don't get a DHCP option either, except via
  `uplink_bridged`'s "the physical LAN's own DHCP" escape hatch, which
  a VNET jail could equally use in a later stage by skipping
  `ip_address` the same way `uplink_bridged` VMs do today - not built in
  this increment, since no jail-side DHCP client wiring exists to make
  it usable yet).
- **Interface wiring**: `epair(4)`, not `if_pair` (see assessment
  above). The host-side end joins the same bridge `vlan.Manager`
  already creates/manages for that `network_id` (`ensureNetwork`,
  reused unmodified) - one new `vlan.Manager` method,
  `EnsureEpair(ctx, bridge) (hostSide, jailSide string, err error)`,
  plus `DestroyEpair(ctx, hostSide) error`, is the only extension to
  `internal/vlan` itself. This directly satisfies the "reuse, don't
  fork, vlan.Manager's existing bridge infrastructure" requirement.
- **Isolation semantics**: a VNET jail's default posture, with no
  firewall rules configured, is the same as a VNET-networked VM's
  default posture today - full L2/L3 connectivity to everything else on
  that network's bridge, gatewayed the same way (this node's own
  `EnsureBridgeAddress`-assigned `.1`, or `network.ExternalGateway` if
  set), with no special "jails can't reach VMs" or "jails can't reach
  each other" boundary. This is a deliberate parity choice, not an
  oversight: introducing an asymmetric isolation default between VMs
  and jails on the same `NetworkDefinition` would be a surprising,
  hard-to-reason-about inconsistency with no clear operator benefit
  over "put jails that need isolation on their own `NetworkDefinition`
  (they already get their own bridge that way) or add PF rules."
- **Firewalling**: `internal/pf` already renders per-VM anchors keyed
  by VM ID (`vmAnchor`, `effectivePFRules`, applied from
  `VMDefinition.firewall_rules` - ADR-0075). `JailDefinition` has no
  `firewall_rules` field today. The natural, consistent extension for a
  later stage is a `jailAnchor(jailID)` convention and a
  `JailDefinition.firewall_rules` field reusing the exact same
  `FirewallRule` message and `internal/pf.RenderRules` - no new PF
  concepts needed, just wiring the existing mechanism to jails the same
  way ADR-0075 wired it to VMs. Not built in this increment (see
  "Rollout" below) - a VNET jail with no rules is unfiltered, exactly
  like an unfiltered VM today.
- **Lifecycle reconciliation**: mirrors `ensureVM`'s pattern exactly.
  `ensureJail` (via a new `ensureJailEpair` helper) provisions the
  network artifact and epair before calling `jail.Manager.CreateJail`,
  the same order `ensureVM` provisions network before calling
  `bhyve.Manager.CreateVM`. `teardownJail`/`reclaimStaleJail` destroy
  the recorded epair pair, mirroring how VM teardown implicitly
  destroys its tap (bhyve does this internally via its own tapfile
  record). Since an `epair(4)` pair can't be created with a
  caller-chosen name (unlike a named `bridge(4)`), and always creating
  a fresh pair on every tick would leak interfaces on any retry, a
  small node-local JSON state file
  (`DefaultJailEpairStatePath = /var/db/apiary/jail-epairs.json`,
  overridable via `Reconciler.JailEpairStatePath`) records
  `jailID -> {host_side, jail_side}` so the same pair is reused across
  ticks/restarts until the jail is torn down - the jail equivalent of
  `internal/bhyve`'s own per-VM `tapfile`, just centralized in one file
  (mirroring `DefaultNetworkStatePath`'s own existing convention)
  rather than one file per jail, since jails have no existing
  `RunDir`-style per-resource state directory to piggyback on the way
  `bhyve.Manager.RunDir` already does for VMs.

## Rollout: staged, additive, opt-in

### Stage 1 (this ADR's implementation - DONE)

An opt-in `vnet` boolean on `JailDefinition`/`Config` (jail(8)'s
`vnet;` parameter), requiring `network_id` to also be set. When unset
(the default for every existing jail and every new jail that doesn't
explicitly opt in), behavior is **completely unchanged**: `ip4=inherit`,
no epair, no IP, no new reconciler work. When set:

1. `internal/vlan.Manager` gains `EnsureEpair`/`DestroyEpair`.
2. `internal/jail.Config` gains `VNET`, `VNETInterface`,
   `IPAddress`/`IPPrefixLen`/`Gateway`. `Manager.CreateJail` builds
   `vnet; vnet.interface=<iface>` instead of `ip4=inherit` when `VNET`
   is set, then (only when `IPAddress` is also set) runs
   `jexec <jail> ifconfig <iface> inet <addr>/<prefix> up` and,
   if `Gateway` is set, `jexec <jail> route add default <gateway>` -
   once, right after `jail -c` succeeds.
3. `JailDefinition` (both `api/internalpb/state.proto` and
   `api/rpc/manager.proto`, kept in lockstep the way every other jail
   field already is) gains `network_id`, `ip_address` (FSM-assigned,
   never caller-set - enforced the same way `VMDefinition.ip_address`
   is), and `vnet`.
4. `internal/raft/fsm.go`'s `applyCreateJail` gains the same
   network-lookup/`allocateIP` step `applyCreateVM` already has, with
   the same `uplink_bridged`-skips-allocation carve-out. `allocateIP`
   itself is extended to also scan `f.jails`, not just `f.vms`, so a
   jail and a VM on the same network can never collide on address.
   Rejects `vnet=true` with no `network_id`.
5. `internal/cluster/plan.go`'s `JailPlacement` gains
   `NetworkID`/`IPAddress`/`VNET`, threaded through from
   `internal/cluster/reconciler.go`'s existing jail-listing loop.
6. `internal/cluster/jail.go`'s `ensureJail` provisions the network
   (reusing `ensureNetwork` unmodified) and epair only when `j.VNET` is
   true, and passes the resulting interface/address into
   `jail.Config`. `teardownJail`/`reclaimStaleJail` destroy the
   recorded epair.
7. RPC (`toInternalJail`/`fromInternalJail` in
   `internal/manager/convert.go`) and REST shim
   (`internal/restshim/convert.go`) both carry the three new fields
   through, matching every other jail field's existing convention.
8. UI: the create-jail form (`web/templates/new_jail.html`) gains a
   "Networking" section with a network picker (reusing the same
   `.Networks` data the create-VM form already fetches) and a VNET
   checkbox, wired into `handleCreateJail`
   (`internal/frontend/server.go`).

### NOT in this increment (explicitly deferred to later stages)

- **PF/firewall integration** for jails (`jailAnchor`/
  `JailDefinition.firewall_rules`, reusing `internal/pf.RenderRules`
  exactly as VMs already do). A VNET jail from this increment is
  unfiltered, same as an unfiltered VM today.
- **Isolation policy UI/config** beyond "which `NetworkDefinition`" -
  no per-jail allow/deny beyond what a future firewall-rules field
  would bring, no isolation levels, no jail-to-jail policy distinct
  from VM-to-VM policy on the same network.
- **Full network-artifact-style epair tracking** integrated into
  `reconcileNetworkArtifacts`'s existing bridge/VLAN
  cleanup-on-network-deletion pass - this increment's epair state file
  is deliberately separate and only ever touched by
  `ensureJail`/`teardownJail`/`reclaimStaleJail`, mirroring how a VM's
  tap device is *also* not tracked in that same artifact file today
  (it's tracked by `bhyve.Manager`'s own `tapfile` instead) - this is
  parity with the existing VM pattern, not a shortcut.
- **DHCP-inside-the-jail / `uplink_bridged`-style "skip Apiary's own
  allocation"** option for VNET jails - `uplink_bridged` VMs already
  get this by skipping `allocateIP`; the same carve-out is already
  present in `applyCreateJail` (an `uplink_bridged` network skips
  allocation for a jail exactly like it does for a VM) but nothing in
  the jail itself would configure a DHCP client on its vnet interface
  yet, so the field exists but isn't independently useful for jails
  until a later stage adds that.
- **Migration/live-move safety** for a VNET jail (e.g. if the owning
  node reassignment machinery is ever extended to jails at all - there
  is no such machinery for jails today, replicated or not, so this is
  out of scope regardless of networking mode).
- **`if_pair` adoption** - explicitly rejected for now, see assessment
  above; revisit only if a concrete future need can't be met by
  `epair(4)` + `pf`.

## Consequences

- Existing jails and the `ip4=inherit` default are provably unaffected:
  `createArgs` (the pure function `CreateJail` now delegates to, added
  specifically to make this testable without root/a real FreeBSD host)
  produces byte-identical output to before this change whenever `VNET`
  is false, verified by `TestCreateArgs_IP4Inherit`. Every existing
  jail test continues to pass unmodified.
- A VNET jail's epair state file
  (`/var/db/apiary/jail-epairs.json` by default) is node-local,
  unreplicated - a jail moved to a different node (were that ever
  supported for jails) would need a fresh epair provisioned on the new
  node, the same way a VM's tap is always freshly created wherever it
  actually runs.
- `allocateIP`'s extension to scan `f.jails` is a small but real
  behavior change to shared VM/jail address-pool math - a network with
  both VM and jail consumers now has its free-address pool computed
  across both, which is the correct behavior (the whole point of
  sharing one `NetworkDefinition`/subnet) but is worth flagging as a
  cross-cutting change to `internal/raft/fsm.go`, not something scoped
  purely to `internal/jail`/`internal/cluster`.
- The "Networking" section on the create-jail form is purely additive;
  a jail created without touching it behaves exactly as before.
