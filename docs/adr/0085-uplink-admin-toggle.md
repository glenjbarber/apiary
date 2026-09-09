# ADR-0085: Uplink admin down/up toggle, decoupled from bhyve

## Status

Accepted

## Context

`cmd/managerd/main.go`'s VLAN/DHCP/PF manager construction was nested
inside the `-bhyve-bootrom` non-empty branch, even though `-vlan-uplink`
is a separate flag - on the stated reasoning that "a dataset-only node
has no VM NICs to attach anywhere." That reasoning doesn't hold: jails
use `ip4=inherit`, so they never needed VLAN/DHCP either, and this
coupling had a real, previously undiscovered side effect - it silently
left `reconciler.Uplink` unset (so `assumecheck.Checker`'s
uplink-mismatch health check went inert) on any node with bhyve
disabled, regardless of whether it ran jails.

Separately, the user wanted an operator-controlled toggle to
administratively bring the host's uplink NIC down/up
(`ifconfig <uplink> down`/`up`). This project already hit a near-miss
doing something adjacent (ADR-0022: bridging the uplink NIC over the
same SSH session that depended on it, requiring a rollback).

## Design decisions

- **VLAN/DHCP/PF construction is now keyed only on `-vlan-uplink`**,
  independent of `-bhyve-bootrom` (`cmd/managerd/main.go`). Bhyve
  construction is unchanged, just no longer a prerequisite. This is a
  real, if narrow, fix: any bhyve-disabled node that sets
  `-vlan-uplink` now gets uplink-mismatch health checking (and, with
  this ADR, the down/up toggle below) that it silently didn't before.
- **`internal/vlan.Manager` gains `Down`/`Up`/`UplinkInterface`** -
  thin wrappers around `ifconfig <uplink> down`/`up`, plus a getter
  (`UplinkInterface`, not `Uplink`, since `Uplink` is already the
  struct's public field name). Confirmed safe against the reconciler's
  own tick loop: `EnsureVLAN` short-circuits immediately for
  `vlanID == 0` (the untagged/raw-uplink case) without issuing any
  `ifconfig` call at all - guarded by a dedicated unit test
  (`TestEnsureVLAN_UntaggedIsANoOp`) that needs no root/real interface,
  since that code path is a pure early return. An administratively-
  downed uplink is therefore never silently re-upped by normal
  reconciliation.
- **Two new `ManagerService` RPCs, mirroring `RestartNodeService`'s
  shape exactly**: `GetUplinkStatus` (Viewer, read-only) and
  `SetUplinkState` (Admin, the actual toggle). Both are strictly
  host-local - no raft, no peer-forwarding - the same reasoning as
  `RestartNodeService`: the Machine Configuration page is inherently
  "this Comb," and the frontend already talks to one node's own
  managerd.
- **No automatic dead-man's-switch revert.** The user was offered one
  (mirroring ADR-0022's own proven `daemon -f sh -c 'sleep N &&
  ifconfig up'` rollback, which survives even a managerd crash/restart
  during the window) and explicitly chose the simpler alternative
  instead: a plain toggle gated by this codebase's existing
  `hx-confirm` browser-dialog pattern (the same one `SetVMFirewallPaused`
  already uses for its own "All its traffic will be allowed until
  resumed" warning), naming the lockout risk explicitly in the confirm
  text. This is a deliberate, accepted tradeoff, not an oversight - if
  the operator's own access path shares the physical NIC with the
  uplink being downed, recovery may require physical or out-of-band
  console access, exactly the class of risk ADR-0022 already
  documented once.
- **PF NAT rules are not paused or cleared when the uplink goes down.**
  `internal/pf.Manager.ApplyNAT` renders `nat-to (<uplink>)` literally
  by interface name; `pfctl` doesn't validate interface state at
  rule-load time, and FreeBSD doesn't clear an interface's addresses on
  `ifconfig down`. In practice this means egress traffic relying on
  that NAT simply stops working silently until the interface comes back
  up - no new PF coordination code was added to handle this, since the
  toggle itself is a manual, deliberate operator action with a visible
  effect (the interface reports "down" on the same page).

  **Resolved**: `SetUplinkState` now proactively flushes the matching
  NAT anchor(s) immediately on down, and the reconciler's own
  unconditional-every-tick `ApplyNAT` already restores it on the next
  tick once the interface comes back up - see
  [ADR-0088](docs/adr/0088-pause-nat-on-uplink-down.md).

## Consequences

- `internal/manager/auth.go`'s `requiredRole` map gained explicit
  entries for both new RPCs (`GetUplinkStatus`: Viewer,
  `SetUplinkState`: Admin), matching this file's own documented
  convention of listing every RPC rather than relying on the implicit
  Admin-by-default fallback.
- The Machine Configuration page gained an Admin-only "Uplink" panel
  (`web/templates/machine.html`) showing the interface name and
  up/down state with a single toggle button.
- Full test coverage: a pure `internal/vlan` unit test proves
  `EnsureVLAN(0)` never calls `ifconfig` (the safety property the whole
  design leans on); `internal/vlan` integration tests exercise real
  `Down`/`Up` against a disposable bridge interface created just for
  the test - deliberately never the real uplink NIC named by
  `APIARY_VLAN_TEST_UPLINK`, since that would risk actually severing
  whatever host runs the test suite; `internal/manager` covers both
  RPCs' behavior and RBAC tiers; `internal/frontend` covers the route,
  the Admin-only page gating, and the form's forwarded value.
- This is a narrow, host-local capability - it does not add any
  cluster-wide network-partition tooling, does not touch raft, and does
  not attempt to coordinate with PF/NAT state. Both limitations above
  are accepted tradeoffs, recorded here rather than silently omitted.
