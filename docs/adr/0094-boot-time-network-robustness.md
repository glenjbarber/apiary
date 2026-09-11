# ADR-0094: Two real boot-time network races, found live on a reboot

## Status

Accepted

## Context

The user rebooted `apiverse` (one of the two production hosts,
`apiverse`+`apiarium`) and reported "something isn't right there." Live
diagnosis over SSH found two distinct, real problems - one had already
made the host briefly unreachable; the other was visible on the
physical console as an interface that "did not come back up."

### Problem 1: a duplicate DHCP lease for the host's own management address

`apiverse`'s `rc.conf` configures `em0` as a pure, address-less bridge
member (`ifconfig_em0="up"`) and `bridge0` (inheriting `em0`'s own MAC
via `create_args_bridge0`, so the DHCP reservation still matches) as the
actual DHCP client holding the real management address, `10.50.0.9` -
documented directly in the `rc.conf`'s own comment as a deliberate
migration. This is the correct design.

But FreeBSD ships a stock `/etc/devd/dhclient.conf` rule that fires
`service dhclient quietstart <if>` on ANY Ethernet-like interface's
link-up event - completely independent of that interface's own
`ifconfig_<if>` value or FreeBSD's own `dhcpif()` eligibility check
(`/etc/network.subr`, confirmed by reading it directly on the host:
`ifconfig_em0="up"` contains no `DHCP` token and should not be
DHCP-eligible under that check alone). This devd rule still
independently DHCPs `em0` on every link-up - including every reboot -
racing `bridge0` for the identical MAC-keyed lease. Confirmed live via
both interfaces' own lease files
(`/var/db/dhclient.leases.em0`/`.bridge0`), each holding a lease for
the same `fixed-address 10.50.0.9` under the same
`dhcp-client-identifier`. Whichever interface's dhclient process most
recently won the race holds the address; the other doesn't - an
unstable, duplicate-address configuration, not a one-time fluke, that
can flip on any future reboot or link flap and is the confirmed root
cause of `apiverse` going unreachable (SSH and ICMP both timed out,
while `apiarium`'s ARP table still showed a fresh entry for `em0`'s own
MAC - consistent with the physical link being up and answering ARP
while the actual management traffic path was unstable).

`apiarium` has the identical vulnerable pattern (`ifconfig_re0="up"` +
`ifconfig_bridge0="addm re0 up DHCP"`) and simply hadn't been rebooted
recently enough to trigger it - a dormant, equally real risk on that
host too. `node01`/`node02` (this session's freshly bootstrapped test
VMs) use a different architecture entirely - `vtnet0` itself holds the
DHCP-acquired address directly (`ifconfig_vtnet0="DHCP"`), and
`bridge0` there is never told to also DHCP - so they were never at risk
of this specific race.

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

### Both closed the same way: new `apiaryinstall` preflight checks (ADR-0082), not just a one-off manual fix

Manually fixing `apiverse` (and `apiarium`, dormant but equally
vulnerable) tonight would leave the exact same trap for the next fresh
bootstrap or the next time someone hand-edits `rc.conf`. Both are
folded into `internal/install`'s existing registry (`internal/install/checks.go`),
so `apiaryinstall -apply` catches and fixes them the same way it
already catches every other host prerequisite this project has been
burned by before.

- **`dnsmasq-rc-enable`** (`RiskSafe`): flags `dnsmasq_enable=YES` in
  `rc.conf` with a fix hint explaining Apiary already manages dnsmasq's
  own lifecycle; `Apply` runs `sysrc dnsmasq_enable=NO`. Always
  applicable - this holds regardless of which networks exist yet.
- **`devd-dhclient-conflict`** (`RiskSafe`): flags a live
  `/etc/devd/dhclient.conf` when both `-vlan-uplink` and `-bhyve-bridge`
  are configured (i.e., whenever this host actually has a
  bridge-member-NIC setup this rule could conflict with); `Apply`
  renames it to `.disabled` and restarts `devd`. The `FixHint` names
  the real tradeoff directly rather than hiding it: disabling this
  system-wide rule also removes automatic DHCP-on-link-up for any
  OTHER, non-bridged NIC on the same host - acceptable for Apiary's own
  dedicated-uplink hosts, but a real behavior change worth knowing
  about, not a free lunch.

Neither check requires a new RPC, proto change, or runtime component -
both are pure host inspection/mutation, matching every other check in
this registry.

## Consequences

- `internal/install/checks_test.go` gained direct regression tests for
  both: `TestDnsmasqRcEnableCheck` (YES/NO/unset probe states, and a
  real Apply call) and `TestDevdDhclientConflictCheck` (applicability
  gated on both uplink+bridge being set, absent-file is OK, a live file
  is misconfigured, Apply moves it and restarts `devd`, and re-applying
  after it's already gone is a safe no-op).
- These checks report and fix host `rc.conf`/`devd` state; they do not
  and cannot retroactively un-race a boot that already happened. The
  live hosts (`apiverse`, `apiarium`) still needed their actual,
  already-affected configuration fixed by hand tonight, tracked
  separately from this code change in `SHARED.md`.
- `apiaryinstall -apply` was already the established, safe way to fix
  `RiskSafe` findings - no new flag or workflow needed for an operator
  to pick these up on a future run.
- Full `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .`
  all clean.
