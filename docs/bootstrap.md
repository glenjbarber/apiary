# Bootstrapping a fresh Apiary host

A step-by-step runbook for taking a blank FreeBSD host (VM or bare metal)
to a running Apiary node with real, working VM/jail networking - using
`apiaryinstall` (ADR-0082) for the host prerequisites, the four daemons
(`raftd`/`managerd`/`frontend`/`restshimd`) for the cluster itself, and
the web UI for the first network and first Cell. Covers both an
independent single-node Comb (Path A) and joining an existing multi-node
Colony (Path B) - they diverge only at the `raftd` step. Written up
(and twice revised) from a live, complete first bootstrap of a fresh
Colony VM, start to finish - every command here was actually run, and
every callout marks a real thing that went wrong the first time through,
not a hypothetical. This doc exists so the next fresh host doesn't
rediscover any of it.

## 0. Two gotchas to know before you start

**`go build ./...` does not produce a binary.** With multiple `main`
packages in this module, `go build ./...` only verifies everything
compiles and discards the result - it will not leave `raftd`,
`managerd`, `frontend`, `restshimd`, or `apiaryinstall` on disk. Every one
of them needs its own explicit build (step 2 below).

**Building as `root` in a repo cloned by another user fails Go's VCS
stamping.** If you `git clone` as one user (e.g. `admin`) and then `go
build` as `root`, git's "dubious ownership" protection makes the build
fail with `error obtaining VCS status: exit status 128`. Either build
with `-buildvcs=false` (shown throughout this doc), or fix it once with:

```bash
git config --global --add safe.directory /path/to/apiary
```

## 1. Base OS and source

```bash
pkg install -y git go
git clone https://github.com/glenjbarber/apiary.git ~/apiary
cd ~/apiary
```

(`go` here also satisfies `cmd/frontend`'s own cgo/PAM requirement -
`frontend` must be built natively on the FreeBSD host it will run on,
ADR-0030.)

## 2. Build all five binaries

The repo's own `Makefile` does this in one step:

```bash
make build
```

Equivalent by hand, if you ever need it (e.g. building just one):

```bash
go build -buildvcs=false -o apiaryinstall ./cmd/apiaryinstall
go build -buildvcs=false -o raftd ./cmd/raftd
go build -buildvcs=false -o managerd ./cmd/managerd
go build -buildvcs=false -o frontend ./cmd/frontend
go build -buildvcs=false -o restshimd ./cmd/restshimd
```

## 3. Run `apiaryinstall` - check first, then apply

```bash
./apiaryinstall
```

Review the report. Then apply the safe fixes (kernel modules, packages,
`pf`, `/etc/rc.conf` permissions, the `-zfs-base` dataset) in one pass:

```bash
./apiaryinstall -apply -zfs-pool <your-pool-name>
```

`zfs-pool` itself is report-only - `apiaryinstall` will tell you if the
pool is missing but will not create one itself (disk layout is
host-specific). If it's missing, create it by hand before continuing:

```bash
zpool create <your-pool-name> <vdev...>
```

`zfs-base-dataset` (managerd's own `-zfs-base`, `<pool>/apiary` by
default) *is* auto-fixable under `-apply` - a fresh pool has no child
datasets at all, and this was found live: the first VM ever created
failed with `zfs create zroot/apiary/<id>: cannot create '...': parent
does not exist`, one confusing layer removed from the actual missing
piece.

Re-run `./apiaryinstall -apply -zfs-pool <your-pool-name>` until every
check besides networking reports `ok`.

## 4. Networking - the one risky step

Find the real uplink NIC name first:

```bash
ifconfig
```

Check what `apiaryinstall` sees before touching anything:

```bash
./apiaryinstall -vlan-uplink <uplink-ifname> -bhyve-bridge bridge0
```

**This step can drop your SSH session** if `<uplink-ifname>` is the same
NIC you're connected through - ADR-0022 documents a real near-miss from
exactly this operation. Use console/hypervisor access instead of SSH if
you can. If you do it over SSH, don't close the terminal if it looks like
it dropped - give it 10-15 seconds and reconnect; the interruption is
normally sub-second, not a real outage.

```bash
./apiaryinstall -apply-network yes-modify-network -vlan-uplink <uplink-ifname> -bhyve-bridge bridge0
```

Confirm everything is clean:

```bash
./apiaryinstall -vlan-uplink <uplink-ifname> -bhyve-bridge bridge0
```

Every check should now read `ok`.

## 5. Find the bhyve firmware path

`bhyve-firmware`'s package may only carry license files, with the actual
`.fd` firmware shipped by its `edk2-bhyve` dependency - confirmed on a
real host. Don't assume the well-known path works without checking:

```bash
pkg info -l bhyve-firmware   # may show only license files
pkg info -d bhyve-firmware   # shows the dependency carrying the real files, e.g. edk2-bhyve-...
pkg info -l edk2-bhyve       # lists the real .fd files
```

Use the path under `/usr/local/share/uefi-firmware/BHYVE_UEFI.fd` if
present (the ADRs' assumed default) - `edk2-bhyve` installs the same
files under both `/usr/local/share/edk2-bhyve/` and
`/usr/local/share/uefi-firmware/`.

## 6. Runtime directories

```bash
mkdir -p /var/db/apiary/raftd /var/db/apiary/isos /var/run/apiary /var/log/apiary
```

## 7. Start `raftd`

Decide first: does this node bootstrap its own independent cluster, or
join an existing multi-node Colony? The two paths diverge here and
rejoin at step 8.

### Path A - independent single-node cluster

Omit `-join` entirely - do **not** pass an empty `-join ""`, just leave
the flag off:

```bash
./raftd -data-dir /var/db/apiary/raftd -socket /var/run/apiary/raftd.sock -node-id <this-node-id>
```

Confirm a clean single-node leader election in the output, `Ctrl-C`, then
launch it detached:

```bash
daemon -f -p /var/run/apiary/raftd.pid -o /var/log/apiary/raftd.log $(pwd)/raftd -data-dir /var/db/apiary/raftd -socket /var/run/apiary/raftd.sock -node-id <this-node-id>
```

### Path B - join an existing multi-node Colony

Two things `-join` needs that are easy to get wrong:

**`-raft-bind` must be a real, routable address, not the default.**
`raftd`'s default `-raft-bind` is `127.0.0.1:17600` - loopback, correct
only for a single-host test cluster. For a genuine cross-host join, pass
this node's own real, reachable address:

```bash
-raft-bind <this-host's-real-address>:17600
```

**`-join` only ever dials its argument as a local Unix domain socket**
(`unix://<path>`, hardcoded in `cmd/raftd`'s own `joinCluster` - see
ADR-0003) - it has no TCP option. That's fine for multiple `raftd`
processes on one host, but the existing cluster member's socket lives on
a *different* host. Make it reachable as a local path with a one-shot SSH
local forward before running `-join`, then tear the tunnel down once the
join succeeds - ongoing raft replication travels over the real
`-raft-bind` TCP addresses afterward, not through this tunnel:

```bash
ssh -f -N -L /tmp/existing-member-raftd.sock:/var/run/apiary/raftd.sock <existing-member-host>
```

Now join through the forwarded local path:

```bash
./raftd -data-dir /var/db/apiary/raftd -socket /var/run/apiary/raftd.sock -node-id <this-node-id> -raft-bind <this-host's-real-address>:17600 -join /tmp/existing-member-raftd.sock
```

If the existing cluster runs with `-internal-token` set, pass the same
token here too (`-internal-token <value>`) - every `raftd` in one cluster
is expected to share it. Once the join succeeds and this node's own log
shows it as a voter, kill the SSH tunnel and launch normally, detached:

```bash
daemon -f -p /var/run/apiary/raftd.pid -o /var/log/apiary/raftd.log $(pwd)/raftd -data-dir /var/db/apiary/raftd -socket /var/run/apiary/raftd.sock -node-id <this-node-id> -raft-bind <this-host's-real-address>:17600
```

(no `-join` on the detached relaunch - a node with existing on-disk raft
state ignores `-join` and simply resumes as the member it already is; see
ADR-0003's own `hadState` note.)

A node joined at the wrong target (a follower, not the leader) fails fast
with a `leader_hint` in the error rather than retrying automatically -
re-run `-join` against the hinted address instead.

### Verify (either path)

```bash
cat /var/log/apiary/raftd.log
ps auxww | grep raftd
```

## 8. Start `managerd`

```bash
daemon -f -p /var/run/apiary/managerd.pid -o /var/log/apiary/managerd.log $(pwd)/managerd \
  -raftd-socket /var/run/apiary/raftd.sock \
  -rpc-addr 0.0.0.0:17700 \
  -node-id <this-node-id> \
  -zfs-base <your-pool-name>/apiary \
  -bhyve-bootrom /usr/local/share/uefi-firmware/BHYVE_UEFI.fd \
  -bhyve-bridge bridge0 \
  -vlan-uplink <uplink-ifname> \
  -iso-dir /var/db/apiary/isos
```

**Joining a multi-node Colony (Path B only)** - add these so this node's
`managerd` can forward writes to (and be forwarded to from) the other
Combs, and so the cluster-overview page can reach their host stats
(ADR-0029):

```bash
  -peer-api-key <the-same-key-every-Comb-in-this-Colony-uses> \
  -peer-managerd-port 17700
```

`-peer-api-key` is required once the Colony has any API key at all
(ADR-0023) - peer-to-peer forwarding goes through the same authenticated
`ManagerService` API as everything else. Add `-peer-tls`/
`-peer-tls-hostname-map` too if the other Combs' `managerd` instances
serve TLS.

Verify:

```bash
cat /var/log/apiary/managerd.log
ps auxww | grep managerd
```

A clean log shows one line: `managerd: listening on 0.0.0.0:17700
(node-id=..., raftd-socket=..., ...)` and nothing after it.

## 9. Start `frontend`

```bash
daemon -f -p /var/run/apiary/frontend.pid -o /var/log/apiary/frontend.log $(pwd)/frontend \
  -manager-addr 127.0.0.1:17700 \
  -http-addr 0.0.0.0:8080
```

Without `-pam-service`/`-role-map` the web UI is open to anyone who can
reach the port - fine for initial verification, but add real login
(ADR-0030) before this host is reachable from anywhere untrusted.

## 10. Verify

Open `http://<this-host's-address>:8080` in a browser - the Colony
overview page should show this Comb as `Reachable`/`healthy` with its ZFS
pool and packet filter both reporting healthy/enabled.

Without `-pam-service`/`-role-map` on `frontend`, every page loads with no
login at all and the Users page shows "no active session" - that's the
expected state with login disabled, not a bug. Step 11 turns real login
on.

## 11. (Optional but recommended) Real login via PAM

Skip this only for throwaway testing - without it, the web UI is open to
anyone who can reach the port.

**Pick a PAM service name** (e.g. `apiary`) and create its policy file
with `printf`, not a pasted heredoc. A heredoc containing tab
characters, pasted into an interactive SSH session without bracketed-paste
support, can have its tabs consumed as tab-completion keystrokes instead
of inserted literally - confirmed live: it produced a corrupted,
field-less `/etc/pam.d/apiary` and a `pam: starting transaction ...
System error` at login. `printf` with explicit `\n`s sidesteps this
class of problem entirely:

```bash
printf 'auth required pam_unix.so no_warn\naccount required pam_unix.so\n' > /etc/pam.d/apiary
```

**Verify it landed correctly** before trying to log in - `cat -A` reveals
any hidden/stray characters:

```bash
cat -A /etc/pam.d/apiary
```

Expect exactly two clean lines, each ending in `$`, nothing squished
together.

**Create a real UNIX account** for each person who should log in (or
reuse existing ones):

```bash
pw useradd -n <username> -m -s /bin/sh
passwd <username>
```

**Restart `frontend`** with PAM and a role map:

```bash
kill $(cat /var/run/apiary/frontend.pid)
daemon -f -p /var/run/apiary/frontend.pid -o /var/log/apiary/frontend.log $(pwd)/frontend \
  -manager-addr 127.0.0.1:17700 \
  -http-addr 0.0.0.0:8080 \
  -pam-service apiary \
  -role-map "admin:<username>"
```

A PAM login for a username with no entry in `-role-map` is rejected
outright, not silently downgraded to Viewer (ADR-0030) - add every
account that should be able to log in to the map, semicolon-separated by
role (`"admin:alice;operator:bob,carol;viewer:dave"`).

`frontend` re-reads `/etc/pam.d/<service>` on every login attempt, not
just at startup - fixing the file doesn't require restarting `frontend`
again, only the first `-pam-service` flag change does.

## 12. Create your first network

The web UI's Networks page (Network -> Networks -> Create) is where a
Comb actually gets usable VM/jail networking - `apiaryinstall`'s own
network step (Step 4) only prepares the *host* (uplink, bridge). Two
real things to know before creating one:

**Free the uplink from `apiaryinstall`'s fallback bridge first.**
`bridge0` (from Step 4, `-bhyve-bridge`) is only ever a fallback for a
VM with no managed network attached. An untagged (`VLAN ID 0`) managed
network enslaves the same uplink NIC directly into its *own*,
separately auto-named bridge (`apnet-<hash>`) - and a FreeBSD interface
can only belong to one bridge at a time. If you already ran Step 4, free
the uplink before creating your first network:

```bash
ifconfig bridge0 deletem <uplink-ifname>
```

(Same SSH-session caution as Step 4 applies here too, though removing
from a bridge is generally less disruptive than adding.)

**Decide self-hosted NAT vs. sharing an existing router's VLAN.** This
project's own history (ADR-0047, superseded in part by ADR-0048) already
settled this: self-hosted NAT is the default, recommended path -
depending on a specific external router/VLAN topology is explicitly
something Apiary is meant to avoid. Create the network with:

- **VLAN ID**: `0` (untagged - no switch/trunk configuration needed
  either way)
- **Subnet**: a private range **not** already used by anything else on
  your physical network (check first - conflicting with an existing
  router's own subnet, even without an address collision, is possible
  and confusing on a shared physical segment)
- **External gateway**: **leave blank** - this is what makes Apiary
  claim the gateway address and NAT egress out through the uplink
  itself, rather than depending on a real router already serving that
  subnet

**Verify it live** once a VM/jail actually uses it (see Step 13 - the
network's bridge/NAT rule are provisioned lazily, only once something
references it, not the moment the network is created):

```bash
ifconfig apnet-<hash>                          # bridge exists, uplink is a member
pfctl -a apiary/net-<network-id> -s rules       # NAT rule loaded - NOT `-s nat`,
                                                 # which stays empty even when working
                                                 # (the rule is a modern `match ... nat-to`
                                                 # form, not a legacy `nat` rule)
```

The Networks page itself shows `unknown` for bridge health until the
first reconcile tick after something actually uses the network - that's
expected, not stuck.

## 13. Create your first VM (or use a jail instead)

**Check hardware virtualization actually works before creating any VM**,
especially if this host is itself a VM (nested virtualization):

```bash
sysctl hw.vmm.vmx.initialized
dmesg | grep -i vmm
```

Expect `hw.vmm.vmx.initialized: 1` and no error in `dmesg`. If it reads
`0`, or `dmesg` shows something like `module_register_init: MOD_LOAD
(vmm, ...) error 6` (`ENXIO`) - `vmm.ko` loading as a kernel module
(which is why `apiaryinstall`'s own `vmm-loaded` check can still report
`ok`) does **not** mean VT-x/EPT is actually usable. This means whatever
hosts this VM isn't exposing nested hardware virtualization to it - a
setting on the *outer* hypervisor (e.g. Proxmox/KVM needs CPU type
`host` and `nested=1` on `kvm_intel`; VMware needs "Expose hardware
assisted virtualization to the guest OS"), not fixable from inside this
host, and not an Apiary bug. **Jails remain fully available regardless**
- they need no hardware virtualization at all, and are the practical
fallback whenever this check fails.

**Always attach an ISO or base image as the install source.** A VM
created with neither has nothing bootable - `bhyve` launches and exits
again almost immediately, over and over, every reconcile tick.

**Known symptom of the above (or any other `bhyve` process dying
independently of Apiary's own tracking) - a permanent crash loop with a
misleading error.** `bhyve`'s serial console logger starts (as a
separate, independently-launched `daemon(8)` process) *before* `bhyve`
itself - if `bhyve` then exits on its own for any reason, the logger
keeps running, untouched, since nothing ties its lifecycle to `bhyve`'s.
The next reconcile retry then fails with:

```
creating bhyve VM: bhyve: starting serial console logger: starting reader:
daemon -f -p .../apiary-<vm>.serialpid ...: daemon: process already running, pid: <N>
```

This is a real gap (tracked as a follow-up, not yet fixed as of this
writing) - the reconciler's retry path doesn't clean up a previous
incarnation's leftover logger before trying again, so this repeats
indefinitely, once every reconcile interval, until an operator
intervenes by hand:

```bash
ps -p <N>                                        # confirm it's genuinely still alive
kill <N>
rm -f /var/run/apiary/bhyve/apiary-<vm>.serialpid
```

The next reconcile tick will retry cleanly - but if the VM still has no
bootable ISO/base image attached, or the host still can't do hardware
virtualization, it will simply hit the same wall again. Fix the actual
underlying cause (attach real boot media; confirm VT-x) rather than
repeating this workaround indefinitely.

## Not covered here (disclosed gaps, not oversights)

- **No rc.d scripts ship in this repo** for the four daemons, so nothing
  here survives a reboot on its own - the `daemon(8)` invocations above
  are for getting a node up and verified, not for production
  persistence. Writing real `/usr/local/etc/rc.d/apiary_*` scripts
  (matching the `apiary_raftd`/`apiary_managerd`/`apiary_frontend`/
  `apiary_restshimd` service names ADR-0049 already assumes exist) is a
  separate piece of work, not part of `apiaryinstall`'s own scope.
- **HAST (`-enable-hast`)** - `apiaryinstall` can check for `hastd_enable`
  but never configures it, and the known `hastd` source patch (ADR-0022,
  FreeBSD bug 298085) is never applied automatically; see ADR-0026 for
  what real setup requires.
- **`-internal-token`/raft transport TLS for a multi-node join** - step 7
  Path B covers the SSH-tunnel mechanics `-join` needs across hosts and
  `-internal-token`, but TLS between raft members (ADR-0078) is left to
  that ADR's own docs if the Colony requires it.
