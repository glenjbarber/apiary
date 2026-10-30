# FreeBSD and Linux feature gap: what Apiary can integrate

Status: research note, not an ADR. Each item that becomes work should get its own ADR.

## Method

- FreeBSD source: `freebsd/freebsd-src`, shallow clone at commit `0974ef03b43f279f16289ad4d5be3a03923983ea`.
- "Native" means implemented in FreeBSD's kernel, libraries, or base tools. The Linux compatibility layer (`sys/compat/linux`) is listed separately because it changes what "FreeBSD cannot" means for Linux binaries.
- Ratings:
  - **No equivalent**: Linux has no kernel or base facility with the same semantics.
  - **Partial**: Linux has an approximation, usually in userspace or with different semantics.
- A strict "never" bar leaves very few items. Most FreeBSD features have some Linux analog, so the ratings matter more than the list length.

## Part 1: FreeBSD features Apiary can integrate

### Original seven

1. **Capsicum capability mode** (Partial)
   - Source: `sys/kern/sys_capability.c` (`sys_cap_enter`, `kern_cap_rights_limit`). `hastd` already uses it (ADR-0008).
   - Linux gap: seccomp and Landlock filter syscalls or paths, not rights on already-open descriptors.
   - Apiary hook: call `cap_enter` in `managerd`, `restshimd`, `frontend`, and `raftd` after sockets and config are open. Verify Go runtime threads under capability mode first.

2. **Netgraph** (No equivalent)
   - Source: `sys/netgraph/` (`ng_bridge.c`, `ng_vlan.c`, `ng_nat.c`, `ng_tee.c`, `ng_bpf.c`, `ng_iface.c`).
   - Linux gap: no in-kernel node-graph framework. Bridges, tc, nftables, and OVS are separate systems.
   - Apiary hook: `internal/netif`, `internal/jail/net.go`, and the NAT design in ADR-0048.

3. **Per-jail securelevel** (No equivalent)
   - Source: `sys/kern/kern_jail.c` (`pr_securelevel` is inherited; a child may only raise it, otherwise `EPERM`), `sys/sys/jail.h:300`, `usr.sbin/jail/jail.c` (`-s`).
   - Linux gap: the lockdown LSM is system-wide. Nothing lets one container be locked down while the host stays open.
   - Apiary hook: a hardening level in jail templates (ADR-0084, ADR-0098). It also gives the jail-console concern in ADR-0068 a kernel-enforced floor.

4. **Boot environments** (Partial)
   - Source: `lib/libbe/be.c` (`be_create`, `be_destroy`, `be_export`, clone paths), `sbin/bectl/bectl.c` (`activate`, `check`).
   - Linux gap: ZFS snapshots plus bootloader tooling get close, but there is no integrated activate-and-roll-back flow in the base system.
   - Apiary hook: ADR-0008 already does this by hand in the lab. It fits ADR-0141 and ADR-0145: snapshot before `pkg upgrade`, activate, roll back if the Comb does not return.

5. **CARP** (Partial)
   - Source: `sys/netinet/ip_carp.c` (`carp_set_state`, MASTER and BACKUP states).
   - Linux gap: VRRP runs in userspace (keepalived), not as a kernel primitive.
   - Apiary hook: floating addresses for `frontend` and the gateway. The README says Apiary has no automatic failover, so this is the first building block.

6. **pfsync** (Partial)
   - Source: `sys/netpfil/pf/if_pfsync.c` (`pfsync_insert_state`, bulk update).
   - Linux gap: conntrackd is the nearest analog and runs in userspace on netfilter.
   - Apiary hook: pairs with CARP so a standby firewall keeps live connections. Builds on ADR-0129.

7. **rctl and racct** (Partial)
   - Source: `sys/kern/kern_rctl.c`, actions `RCTL_ACTION_DENY`, `SIGTERM`, `LOG`, `DEVCTL` (`sys/sys/rctl.h`).
   - Linux gap: cgroups limit resources but do not offer this per-rule deny, log, and signal model.
   - Apiary hook: per-cell limits in `internal/jail`. Nothing in the Apiary code references rctl today, so this is new work.

### Additional seven

8. **libcasper services** (No equivalent)
   - Source: `lib/libcasper/services/`: `cap_dns`, `cap_fileargs`, `cap_grp`, `cap_net`, `cap_netdb`, `cap_pwd`, `cap_sysctl`, `cap_syslog`.
   - Linux gap: no capability-mode service broker in the base system.
   - Apiary hook: pairs with item 1. A daemon in capability mode can still resolve names, read sysctl values, and log through brokered services.

9. **Persistent jails** (No equivalent)
   - Source: `sys/sys/jail.h:326` (`PR_PERSIST`, "Can exist without processes"), `sys/kern/kern_jail.c`.
   - Linux gap: a container needs a running process to exist. There is no empty container that keeps its identity.
   - Apiary hook: a stopped cell keeps its jail identity and network configuration across restarts.

10. **Jail kqueue events** (No equivalent)
    - Source: `EVFILT_JAIL` and `EVFILT_JAILDESC` (`sys/sys/event.h:48-49`), registered in `sys/kern/kern_event.c:398-399`, implemented in `sys/kern/kern_jaildesc.c:386`.
    - Linux gap: no jail lifecycle events. Watching `cgroup.events` through inotify is an approximation.
    - Apiary hook: replace polling of cell state in `managerd` with kernel-delivered events.

11. **mac_veriexec** (Partial)
    - Source: `sys/security/mac_veriexec/` (fingerprint checks at exec, SHA-256, SHA-384, SHA-512).
    - Linux gap: IMA and dm-verity cover parts of this, but not the same exec-time fingerprint policy.
    - Apiary hook: refuse to run `managerd`, `hastd`, or `bhyve` binaries whose fingerprints do not match the release manifest.

12. **mac_do** (Partial)
    - Source: `sys/security/mac_do/mac_do.c`, rule-based privilege delegation enforced in the kernel.
    - Linux gap: sudo and polkit do this in userspace.
    - Apiary hook: delegate specific privileged actions to the Apiary service account without granting full root.

13. **mac_partition** (Partial)
    - Source: `sys/security/mac_partition/mac_partition.c`. Processes see only peers with the same label.
    - Linux gap: PID namespaces and `hidepid` approximate this.
    - Apiary hook: stop cell-level users from seeing host or other-cell processes.

14. **mac_portacl** (Partial)
    - Source: `sys/security/mac_portacl/mac_portacl.c`, per-user bind policy by port.
    - Linux gap: Landlock network rules and SELinux port labels approximate this.
    - Apiary hook: let a cell's service user bind 80 or 443 without running as root.

## Part 2: Linux features FreeBSD cannot do natively

For each item, the FreeBSD source check is listed. Items marked **stub** exist in `sys/compat/linux` but return ENOSYS or EINVAL, so they do not give Linux binaries real behavior.

1. **io_uring**: no native implementation. `sys/compat/linux/linux_dummy.c:116-118` (stub).
2. **eBPF and XDP**: no eBPF or XDP code in `sys/kern`, `sys/net`, `sys/netinet`, or `sys/netpfil`. FreeBSD has classic BPF only (`bpf(4)`).
3. **cgroups** (v1 and v2 controllers, freezer, `memory.max`, `io.max`): no cgroup code in `sys/kern`, `sys/vm`, `sys/net`, or `sys/fs`. FreeBSD uses rctl and racct instead (item 7 above).
4. **Namespaces** (`unshare`, `setns`, mount, PID, network, user): `unshare` is a stub (`linux_dummy.c:77`). Jails and VNET are a different model.
5. **fanotify**: stub (`linux_dummy.c:88-89`).
6. **userfaultfd**: stub (`linux_dummy.c:105`).
7. **Landlock**: stub (`linux_dummy.c:136-138`).
8. **seccomp-bpf filters**: the Linux shim returns `EINVAL` or `EOPNOTSUPP` (`sys/compat/linux/linux_misc.c:2767`). No native filter.
9. **overlayfs**: no code. The closest native facility is `unionfs` (`sys/fs/unionfs`), which behaves differently.
10. **Btrfs**: no code in the base tree.
11. **device-mapper, dm-crypt, and LVM**: no code. FreeBSD uses GEOM, with `geli` for encryption.
12. **VFIO and mdev device passthrough**: no code. FreeBSD uses `ppt(4)` passthrough in `usr.sbin/bhyve/pci_passthru.c`.
13. **KSM (kernel samepage merging)**: no code in `sys/vm`.
14. **CRIU-style process checkpoint and restore**: no code. bhyve VM snapshots are a different mechanism.

### Linux binaries that FreeBSD does run

The Linux compatibility layer emulates many calls natively, including `epoll` (`sys/compat/linux/linux_event.c`) and `futex` (`linux_futex.c`). Those items are not on the list above because a Linux binary can run on FreeBSD through the emulation. Native FreeBSD code does not implement them, though, so they do not map to Apiary's own code.

FreeBSD also has native `inotify` (`sys/kern/vfs_inotify.c`), native `timerfd` (`sys/kern/sys_timerfd.c`), native `kexec` (`sys/kern/kern_kexec.c`), and native WireGuard (`sys/dev/wg/`). Those are not gaps.

## Recommended first builds

- Capsicum with libcasper (items 1 and 8). They are one design: the daemon drops privileges and still has brokered access to DNS, sysctl, and syslog.
- Per-jail securelevel (item 3). Small change in jail templates, with a clear security win.
- Jail kqueue events (item 10) if `managerd` polling becomes a problem.
