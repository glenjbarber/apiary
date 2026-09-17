# ADR-0094: Boot-time network robustness

## Status

Accepted

## Context

The user rebooted `apiverse` (one of the two production hosts,
`apiverse`+`apiarium`) and reported "something isn't right there." The
investigation found a real dnsmasq boot race and a duplicate management
lease. The original explanation for the duplicate lease was later
disproved and is corrected below.

### Problem 1: management DHCP must belong to the bridge

`apiverse`'s `rc.conf` configures `em0` as a pure, address-less bridge
member (`ifconfig_em0="up"`) and `bridge0` (inheriting `em0`'s own MAC
via `create_args_bridge0`, so the DHCP reservation still matches) as the
actual DHCP client holding the real management address, `10.50.0.9` -
documented directly in the `rc.conf`'s own comment as a deliberate
migration. This is the correct design.

The duplicate lease was real, but the first diagnosis blamed FreeBSD's
stock `/etc/devd/dhclient.conf` rule. That was wrong. The rule invokes
`service dhclient quietstart <if>`, which still honors
`/etc/network.subr`'s `dhcpif()` test. An uplink configured only as
`ifconfig_em0="up"` is not DHCP-eligible, so the stock rule does not
independently DHCP that interface. Disabling the rule system-wide is
neither required nor appropriate.

For a DHCP-managed host whose physical uplink is a bridge member, the
persistent layout is:

```sh
ifconfig_em0="up"
cloned_interfaces="bridge0"
create_args_bridge0="ether <em0-mac>"
ifconfig_bridge0="addm em0 up SYNCDHCP"
```

The physical member remains addressless. The bridge owns the management
lease, uses the physical NIC's MAC so an existing reservation remains
valid, and acquires the lease synchronously during boot.

### Problem 2: dnsmasq starting before Apiary has recreated its interface

The console showed an `apnet-<hash>` interface (one of Apiary's own
managed VM networks, VLAN+bridge, created at runtime by `managerd`'s
reconciler - never by `rc.conf`) that appeared not to come back up; the
user could not scroll back far enough on the physical console to
confirm. Log evidence resolved it precisely:

```
00:52:13  dnsmasq: unknown interface apnet-e5c2aa38   (rc.d's boot-time dnsmasq, too early)
00:59:30  dnsmasq: unknown interface apnet-e5c2aa38   (retried, still too early)
01:01:50  kernel: bridge1: changing name to 'apnet-e5c2aa38'   (managerd's reconciler finally creates it)
01:01:50  kernel: apnet-e5c2aa38: link state changed to UP
```

`rc.conf`'s `dnsmasq_enable="YES"` starts dnsmasq via the standard
`rc.d` boot sequence, before `managerd`'s reconciler has had its first
tick to recreate the network. But `internal/dhcpd.Manager.WriteAndReload`
(ADR-0022) already calls `service dnsmasq restart` itself, every time
it renders a new config - the only start dnsmasq should ever need. The
`rc.d` boot-time start is not just redundant, it actively races the
reconciler: on this boot it happened to self-heal (the reconciler's own
restart landed a few minutes later), but that's luck, not a guarantee -
a slower boot, a delayed first reconcile tick, or dnsmasq exiting
instead of retrying on "unknown interface" would leave DHCP genuinely
unserved for that network indefinitely, with no signal beyond a log
line an operator has to know to look for.

## Decision

### Enforce the real host invariants through `apiaryinstall`

The relevant invariants are folded into `internal/install`'s existing
registry (`internal/install/checks.go`) so a fresh bootstrap does not
depend on remembered host-specific steps.

- **`dnsmasq-rc-enable`** (`RiskSafe`): flags `dnsmasq_enable=YES` in
  `rc.conf` with a fix hint explaining Apiary already manages dnsmasq's
  own lifecycle; `Apply` runs `sysrc dnsmasq_enable=NO`. Always
  applicable - this holds regardless of which networks exist yet.
- **`bhyve-bridge`** (`RiskNetwork`): in addition to checking the live
  bridge membership, validates the persistent `rc.conf` layout. When
  DHCP is currently configured on the physical uplink, `Apply` leaves
  the uplink addressless, pins the bridge to the uplink MAC, and places
  `SYNCDHCP` on the bridge. It does not move the live lease while the
  installer is running, avoiding an intentional SSH disconnect.

Neither invariant requires a new RPC, proto change, or runtime component.

## Consequences

- `internal/install/checks_test.go` covers `dnsmasq-rc-enable` and the
  corrected bridge DHCP migration. The obsolete devd test and mutation
  were removed.
- The installer no longer renames `/etc/devd/dhclient.conf` or restarts
  `devd`.
- `apiaryinstall -apply` was already the established, safe way to fix
  `RiskSafe` findings - no new flag or workflow needed for an operator
  to pick these up on a future run.
- Full `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .`
  all clean.
