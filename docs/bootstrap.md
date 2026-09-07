# Bootstrapping a fresh Apiary host

A step-by-step runbook for taking a blank FreeBSD host (VM or bare metal)
to a running Apiary node, using `apiaryinstall` (ADR-0082) for the host
prerequisites and the four daemons (`raftd`/`managerd`/`frontend`/
`restshimd`) for the cluster itself. Covers both an independent
single-node Comb (Path A) and joining an existing multi-node Colony
(Path B) - they diverge only at the `raftd` step. Written up after a
live, first-time run on a fresh Colony VM (`node01`) surfaced two real
`apiaryinstall` bugs (both fixed - see the ADR), a PAM setup pitfall, and
two bootstrap gotchas that aren't code bugs but cost real time the first
time through. This doc exists so the next fresh host doesn't rediscover
any of it.

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
