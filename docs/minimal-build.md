# Minimal FreeBSD base for an Apiary Comb

Two related but distinct things live here, for two different moments:

1. **`etc/os-build/src.conf` and `etc/os-build/make.conf`** - build-time
   knobs for compiling a smaller FreeBSD 16.0-CURRENT base from source
   (`buildworld`/`buildkernel`, or a `release(7)`-built ISO). Use these
   when building a Comb image from scratch. Copy or symlink them to
   `/etc/src.conf` and `/etc/make.conf` on the build host (or point
   `SRCCONF`/`__MAKE_CONF` at them directly - both knob files say so in
   their own header comments).
2. **The package list below** - which `pkg(8)`-managed base packages
   (FreeBSD 16 ships a componentized "pkgbase" system; base is not one
   monolithic blob any more) can be `pkg delete`d from an **already
   installed** Comb, right now, without rebuilding anything.

Both answer the same question - "what does a Comb actually need to run"
- from two different directions: one keeps it from ever being built,
the other removes it after the fact.

## How this was verified

Nothing here is guessed from general FreeBSD knowledge or from a
different release's defaults. Everything was checked against a live
Comb (`buzz.lab3.home.arpa`, FreeBSD 16.0-CURRENT amd64,
`main-n288684-420428718da7`) and against Apiary's own source, on
2026-09-29:

- **What Apiary actually runs**: every `exec.Command`/`exec.CommandContext`
  call site across `internal/` and `cmd/`, *plus* a second pass finding
  every `runCmd`/`runCmdStdin` wrapper function (`internal/bhyve/exec.go`,
  `internal/hast/exec.go`, `internal/hoststats/stats.go`, `internal/pf/exec.go`,
  `internal/dhcpd/exec.go`, `internal/deadman/manager.go`,
  `internal/cloudflare/exec.go`, `internal/vlan/exec.go`,
  `internal/ufsmount/manager.go`) and re-grepping *their* literal command
  arguments too. The first pass alone missed `smart(8)` entirely - it is
  only ever called through `internal/hoststats`'s own `runCmd` wrapper, not
  a bare `exec.Command`. That near-miss is the reason this file exists as
  two passes, not one; if you extend this list later, repeat the
  wrapper-function sweep, not just a plain `exec.Command` grep.
- **What each binary belongs to**: `pkg which` against the actual
  installed path of every required binary (e.g. `sysrc` turned out to live
  in `FreeBSD-bsdconfig`, not its own package - an easy thing to get wrong
  by name alone).
- **What's actually running**: `service <name> status` and `ps`/`sockstat`
  for anything the source-grep couldn't settle by itself. This is how
  `local_unbound` was caught - an earlier draft of `src.conf` assumed
  Combs only use "the network's own resolver" per `docs/bootstrap.md`,
  which was wrong. `local_unbound` is genuinely running on the checked
  Comb, and the file was corrected before anything was proposed to you.
- **Official FreeBSD groupings**: FreeBSD 16 ships its own
  `FreeBSD-set-minimal` / `FreeBSD-set-base` / `FreeBSD-set-devel` /
  `FreeBSD-set-optional` meta-packages. `set-devel` alone (the compiler
  toolchain, debugger, dev headers, test frameworks) is **~480 MiB** on
  the checked Comb (`pkg info -s` summed across its full dependency list)
  and Apiary never compiles anything on a deployed Comb - binaries are
  built centrally and shipped via `apiaryctl install` - so that entire
  tier is the single biggest, safest win here.

## Architecture warning - read before applying either file

The fleet is not one architecture. `brood`/`drone`/`buzz`/`sting`/`apiverse`
are amd64; `frame` is an arm64 Raspberry Pi 4. A few knobs and several
packages below are hardware- or architecture-specific (`FreeBSD-firmware-iwm`,
`FreeBSD-efi-tools`, GPIO support, the U-Boot/device-tree loader path). This
document was verified against one **amd64** Comb only. Applying the amd64
package-deletion list unmodified to `frame` could remove something the RPi4
actually needs to boot. If you rebuild `frame`'s image, re-run the same
verification (`kldstat`, `pkg which` against Apiary's required binaries,
`service ... status`) on a real RPi4 running 16.0-CURRENT before trusting
this list there - do not assume the amd64 findings transfer.

## Packages that are load-bearing - never remove these

Grounded in the source-grep and `pkg which` above, not assumption:

| Binary(ies) | Package | Why |
|---|---|---|
| `zfs`, `zpool` | `FreeBSD-zfs`, `FreeBSD-zfs-lib` | Apiary's entire storage model (`zroot`) |
| `jail`, `jexec`, `jls` | `FreeBSD-jail` | Apiary's jail lifecycle |
| `bhyve`, `bhyvectl` | `FreeBSD-bhyve` | Apiary's VM lifecycle |
| `hastctl`, `hastd` | `FreeBSD-hast` | `internal/hast` replication |
| `pfctl` | `FreeBSD-pf` | Apiary's only firewall (ADR-0048's NAT is PF-based, not `natd`) |
| `at`, `atq`, `atrm` | `FreeBSD-at` | `internal/deadman`'s dead-man's-switch restart guard |
| `smart` | `FreeBSD-smart` | `internal/hoststats` disk-health reporting - see the wrapper-function note above; `FreeBSD-smart-dbg` (its debug symbols) is still safe to drop |
| `pw` | `FreeBSD-runtime` | `internal/frontend/password.go`'s `SetPassword` |
| `sysrc` | `FreeBSD-bsdconfig` | read by name into `NodeConfig`; do not treat bsdconfig as installer-only |
| `pkg` | `FreeBSD-pkg-bootstrap`, `FreeBSD-pkgconf` | bootstraps `dnsmasq` (see below) and any future pkgbase update |
| `ifconfig`, `route`, `sysctl`, `mount`, `umount`, `kldload`, `tar`, `sh`, `daemon` | `FreeBSD-runtime` | networking, ZFS/jail plumbing, `internal/deadman`'s scripts |
| `netstat`, `sockstat` | `FreeBSD-utilities` | diagnostics this session used directly against live Combs |
| `newfs`, `dumpfs` | `FreeBSD-ufs` | filesystem creation/inspection (backup/restore paths) |
| `service` | `FreeBSD-rc` | every `apiary_*` daemon's own restart path |
| n/a (runtime library) | `FreeBSD-pam`, `FreeBSD-pam-lib` | all web UI login (`internal/pam`) |
| n/a | `FreeBSD-ssh` | the only remote administration path to a Comb |
| n/a | `FreeBSD-caroot`, `FreeBSD-certctl` | TLS peer verification, `pkg`'s own HTTPS repo access |
| n/a (linked library) | `FreeBSD-openssl-lib` | crypto primitives other kept packages link against, even though the `openssl` CLI itself is not called anywhere in Apiary's source |
| n/a | `FreeBSD-zoneinfo` | correct log timestamps everywhere |
| n/a | `FreeBSD-libarchive` | `tar`'s own runtime library |
| n/a | `FreeBSD-libucl` | `pkg`'s own config-parsing dependency |
| n/a | `FreeBSD-clibs` | base C runtime; nothing runs without it |
| n/a | `FreeBSD-kernel-generic` | the kernel |
| n/a | `FreeBSD-bootloader` | booting at all |
| n/a | `FreeBSD-devd`, `FreeBSD-devmatch` | hardware hotplug/driver auto-matching at boot |
| n/a | `FreeBSD-geom` | GEOM framework ZFS/UFS/HAST all sit on |
| n/a | `FreeBSD-local-unbound` | **confirmed running live** - do not remove on the assumption Combs only use an external resolver |
| n/a | `FreeBSD-ncurses`, `FreeBSD-ncurses-lib` | terminal handling for `vi`/line editing/anything curses-based over SSH |

`FreeBSD-hast` in particular is worth a specific note: `hastd` running on
the checked Comb had `geom_gate.ko` loaded (`kldstat`) - HAST implements its
replicated provider through the kernel's GEOM Gate mechanism, confirmed by
`pkg which /boot/kernel/geom_gate.ko` showing it ships **inside**
`FreeBSD-kernel-generic` itself, not as a separate package. The *userland*
`FreeBSD-ggate` package (`ggatec`/`ggated`, never called by Apiary) is
still safely removable - it's a different thing from the kernel module.

## Safe to remove from an already-installed Comb

Grouped by why, not alphabetically, so the reasoning stays attached. Every
`-dev` package anywhere in the 222 currently installed (64 of them) is
its own blanket category: **all headers/static-lib/dev-tool packages are
removable together**, since Apiary never compiles anything on a deployed
Comb. They are not re-listed individually below.

**The devel/toolchain tier** (`FreeBSD-set-devel`'s own dependency list,
~480 MiB): `FreeBSD-clang`, `FreeBSD-lld`, `FreeBSD-lldb`, `FreeBSD-bmake`
is the one exception worth keeping (some rc.d/base scripts assume a POSIX
`make` exists; its cost once clang/lld are gone is small), `FreeBSD-ctf`,
`FreeBSD-ctf-lib`, `FreeBSD-googletest`, `FreeBSD-kyua`, `FreeBSD-toolchain`.

**Legacy/unused network protocols and daemons** (none appear anywhere in
Apiary's source; PF + `dnsmasq` are the only networking Apiary itself
drives): `FreeBSD-ipf`, `FreeBSD-ipfw`, `FreeBSD-rip` (routed's helper),
`FreeBSD-bootparamd`\*, `FreeBSD-bootpd`\*, `FreeBSD-inetd`,
`FreeBSD-kerberos`, `FreeBSD-kerberos-kdc`, `FreeBSD-kerberos-lib`,
`FreeBSD-gssd` (Kerberos-NFS), `FreeBSD-librpcsec_gss`, `FreeBSD-tcpd`
(TCP wrappers - PF's own rules are the real defense here),
`FreeBSD-blocklist`, `FreeBSD-blocklist-lib`, `FreeBSD-natd` (Apiary's NAT
is PF-based, ADR-0048), `FreeBSD-ftp`, `FreeBSD-telnet`, `FreeBSD-rcmds`
(rlogin/rsh/rcp - SSH is the real remote-access path), `FreeBSD-yp`,
`FreeBSD-yp-lib` (NIS), `FreeBSD-nfs`, `FreeBSD-iscsi`, `FreeBSD-ctl`
(iSCSI/SCSI target), `FreeBSD-lp` (line printing), `FreeBSD-autofs`.
(\*`bootparamd`/`bootpd` are legacy BOOTP - `dnsmasq` handles DHCP.)

**Hardware absent from this fleet** (commodity amd64 servers - see the
architecture warning before applying this to `frame`):
`FreeBSD-bluetooth`, `FreeBSD-bluetooth-lib`, `FreeBSD-sound`,
`FreeBSD-wpa` (WPA supplicant - no WiFi), `FreeBSD-hostapd` (WiFi AP),
`FreeBSD-firmware-iwm` (Intel wireless firmware), `FreeBSD-games`,
`FreeBSD-cxgbe-tools` (Chelsio - no such NICs), `FreeBSD-mlx-tools`
(Mellanox - none), `FreeBSD-hyperv-tools` (Combs are bhyve *hosts*, never
Hyper-V guests), `FreeBSD-rdma`, `FreeBSD-rdma-lib` (no InfiniBand/RoCE),
`FreeBSD-apm` (legacy laptop power management), `FreeBSD-nvme-tools`
(only if no Comb in your fleet actually has NVMe storage - verify per
machine, brood/drone's drives were last confirmed as SATA SSDs, not
NVMe).

**Mail** (no local-mail-delivery feature in Apiary; any future alerting is
a Colony-wide notification concern, ADR-0134, not a local MTA):
`FreeBSD-sendmail`, `FreeBSD-dma`, `FreeBSD-libmilter`.

**Tracing/profiling/debug tooling** (no consumer once the toolchain is
gone): `FreeBSD-dtrace` (does **not** touch ZFS - that's `WITHOUT_CDDL`,
which is a different, much more dangerous knob covered in `src.conf`'s
own comments), `FreeBSD-dwatch`, `FreeBSD-pmc`, `FreeBSD-libdwarf`,
`FreeBSD-libipt`, `FreeBSD-libthread_db`, `FreeBSD-atf`, `FreeBSD-atf-lib`.

**Misc unused daemons/tools**: `FreeBSD-acct` (process accounting),
`FreeBSD-audit`, `FreeBSD-audit-lib` (OpenBSM auditing - revert this one
first if compliance-grade audit trails are ever wanted, rather than
re-adding it piecemeal), `FreeBSD-bsnmp`, `FreeBSD-libbegemot`,
`FreeBSD-libbsdstat`, `FreeBSD-librss` (all bsnmp-only), `FreeBSD-ccdconfig`
(superseded by GEOM/ZFS), `FreeBSD-quotacheck` (no disk quotas used
anywhere), `FreeBSD-ppp` (no dial-up links), `FreeBSD-lib9p` (Plan 9
protocol, unused), `FreeBSD-libcuse`, `FreeBSD-libvgl` (VGA console
graphics, headless server), `FreeBSD-netmap` (no consumer identified),
`FreeBSD-ggate` (userland `ggatec`/`ggated` only - see the HAST note
above for why the *kernel* module is unaffected), `FreeBSD-csh`
(confirmed: root's shell is `/bin/sh`, not `csh` - checked
`/etc/passwd` directly rather than assumed), `FreeBSD-smart-dbg` (debug
symbols only; `FreeBSD-smart` itself stays), `FreeBSD-libblocksruntime`
(Clang "blocks" extension runtime - nothing on a Comb uses it once the
toolchain is gone).

**Docs/examples**: `FreeBSD-examples`, `FreeBSD-bsdinstall` (installer-only,
Apiary provisions via `apiaryinstall`/`apiaryctl install`, never
`bsdinstall`), `FreeBSD-nuageinit` (cloud-init-style first-boot tool -
Apiary has its own provisioning path).

**The `openssl` CLI specifically** (`FreeBSD-openssl`, keeping
`FreeBSD-openssl-lib`): confirmed no source in Apiary shells out to the
`openssl` binary (all TLS/cert work goes through Go's own `crypto/tls`
and `crypto/x509`), and `certctl` (checked directly, a stripped native
binary) does not appear to invoke it either.

## Verify before removing - genuinely uncertain, not confidently either way

- **`FreeBSD-locales`, `FreeBSD-nls`, `FreeBSD-nls-catalogs`** - nothing in
  Apiary or the tools it calls localizes output, but this wasn't checked
  against every possible PAM/login message path. Low risk either way.
- **`FreeBSD-libsqlite3`, `FreeBSD-libyaml`, `FreeBSD-libpathconv`,
  `FreeBSD-libevent1`, `FreeBSD-libldns`, `FreeBSD-ldns-utils`,
  `FreeBSD-libexecinfo`, `FreeBSD-efi-tools`, `FreeBSD-flua`,
  `FreeBSD-resolvconf`, `FreeBSD-mandoc`, `FreeBSD-mtree`,
  `FreeBSD-console-tools`, `FreeBSD-fwget`** - an attempt to trace their
  actual consumers on the live Comb (`pkg query` for reverse
  dependencies) did not produce a clean answer in the time available for
  this pass. Individually small; not worth removing on a guess given
  `resolvconf` in particular could interact with how `local_unbound`
  gets its config, and `flua` may be load-bearing for the EFI loader's
  own Lua scripting on some boot configurations. Verify each with `pkg
  info -d`/`pkg query %ro` (reverse-depends) against your actual running
  Combs before removing.
- **`FreeBSD-rescue`, kernel debug symbols** (`WITHOUT_RESCUE`,
  `WITHOUT_KERNEL_SYMBOLS` in `src.conf`) - deliberately left at their
  defaults (kept) rather than recommended for removal. Both trade real
  incident-recovery capability for size, on a project whose own stated
  design principle is that state should be obvious and gaps should never
  be silently assumed away. See `src.conf`'s own closing comment for the
  reasoning; this is a call for you to make, not one this document makes
  for you.

## Third-party runtime dependencies - not pkgbase, don't forget them

Two binaries Apiary needs are **not** part of FreeBSD base at all, so no
`src.conf` knob or pkgbase removal decision touches them - they come from
the binary package repo or a separate download, and a minimal image still
needs `pkg`'s own network access (or a bundled copy) to get them:

- **`dnsmasq`** - `internal/dhcpd`'s DHCP server. `internal/install/checks.go`
  auto-installs it via `pkg install -y dnsmasq` if missing; a fully
  offline/air-gapped image build needs to bundle this package rather than
  rely on that auto-install path working at first boot.
- **`cloudflared`** - only needed on a Comb with Cloudflare Tunnel exposure
  configured (`internal/cloudflare`, ADR-0063). Conditional, not universal
  - most Combs will never need it, but an image meant to support that
  feature needs a way to get it installed.

## Not done in this pass - real follow-up work, not oversights

- **Per-architecture verification on `frame`** (the arm64 RPi4) - this
  entire document was checked against one amd64 Comb. The RPi4's real
  module/package needs (device tree, U-Boot, GPIO) were not checked live
  and must not be assumed from the amd64 findings.
- **Kernel module trimming** (`MODULES_OVERRIDE`/`WITHOUT_MODULES` in
  `make.conf`) - `kldstat` on the checked Comb showed a real, small
  module set (`zfs`, `fdescfs`, `nmdm` for bhyve serial console,
  `if_bridge`+`bridgestp`, `pf`+`pflog`, `geom_gate` for HAST, plus
  several `acpi_wmi`/`hidmap`/`hsctrl`/`ichsmb`/`smbus` modules that
  `devd` auto-matched to *that specific motherboard* - excluding those by
  name would be safe on that one machine and silently wrong on different
  hardware). A real whitelist needs the same live `kldstat` check
  repeated across every distinct hardware platform in the fleet,
  including `frame`'s completely different arm64 module needs - not
  something to guess at from one box.
- **Reverse-dependency tracing** for the "verify before removing" list
  above, ideally with `pkg query -e` run against a disposable snapshot of
  a real Comb rather than a production one.
