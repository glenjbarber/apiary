# Bootstrapping a fresh Apiary host

A step-by-step runbook for taking a blank FreeBSD host (VM or bare metal)
to a running Apiary node, using `apiaryinstall` (ADR-0082) for the host
prerequisites and the four daemons (`raftd`/`managerd`/`frontend`/
`restshimd`) for the cluster itself. Written up after a live, first-time
run on a fresh Colony VM (`node01`) surfaced two real `apiaryinstall` bugs
(both fixed - see the ADR) and two bootstrap gotchas that aren't code bugs
but cost real time the first time through. This doc exists so the next
fresh host doesn't rediscover either.

## 0. Two gotchas to know before you start

**`go build ./...` does not produce a binary.** With multiple `main`
packages in this module, `go build ./...` only verifies everything
compiles and discards the result - it will not leave `raftd`,
`managerd`, `frontend`, `restshimd`, or `apiaryinstall` on disk. Every one
of them needs its own explicit build (step 2 below).

**Building as `root` in a repo cloned by another user fails Go's VCS
stamping.** If you `git clone` as one user (e.g. `gjb`) and then `go
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

## 2. Build all five binaries explicitly

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
`pf`, `/etc/rc.conf` permissions) in one pass:

```bash
./apiaryinstall -apply -zfs-pool <your-pool-name>
```

`zfs-pool` is report-only - `apiaryinstall` will tell you if it's missing
but will not create one itself (disk layout is host-specific). If it's
missing, create it by hand before continuing:

```bash
zpool create <your-pool-name> <vdev...>
```

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

Decide first: does this node join an existing cluster, or bootstrap its
own? Omit `-join` entirely to bootstrap a new, independent single-node
cluster - do **not** pass an empty `-join ""`, just leave the flag off.

Run in the foreground first to see it come up cleanly:

```bash
./raftd -data-dir /var/db/apiary/raftd -socket /var/run/apiary/raftd.sock -node-id <this-node-id>
```

Confirm a clean single-node leader election in the output, `Ctrl-C`, then
launch it detached:

```bash
daemon -f -p /var/run/apiary/raftd.pid -o /var/log/apiary/raftd.log $(pwd)/raftd -data-dir /var/db/apiary/raftd -socket /var/run/apiary/raftd.sock -node-id <this-node-id>
```

To join an existing cluster instead, add `-join <existing-member's-internal-socket-path>`
(that socket must be reachable from this host - typically means the
existing member's `-socket` path if this is a shared filesystem, or more
realistically the existing member's `raftd` needs to be reachable via its
`-raft-bind` TCP address once joined).

Verify:

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

## Not covered here (disclosed gaps, not oversights)

- **No rc.d scripts ship in this repo** for the four daemons, so nothing
  here survives a reboot on its own - the `daemon(8)` invocations above
  are for getting a node up and verified, not for production
  persistence. Writing real `/usr/local/etc/rc.d/apiary_*` scripts
  (matching the `apiary_raftd`/`apiary_managerd`/`apiary_frontend`/
  `apiary_restshimd` service names ADR-0049 already assumes exist) is a
  separate piece of work, not part of `apiaryinstall`'s own scope.
- **PAM (`-pam-service`), HAST (`-enable-hast`)** - `apiaryinstall` can
  check for these but never configures them; see ADR-0030/ADR-0026 for
  what real setup they require.
- **Joining an existing multi-node cluster** - step 7 sketches it, but a
  real multi-node join has its own network-reachability and
  `-internal-token`/TLS considerations (ADR-0023, ADR-0078) beyond this
  single-node-focused doc's scope.
