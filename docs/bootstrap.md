# Apiary node runbooks

Use the runbook that matches the outcome you want:

- [Create a new standalone node](create-node.md) creates a fully working
  single-node Comb. It is the correct starting point for an independent
  installation or a host you want to validate before joining elsewhere.
- [Add a node to an existing Colony](add-node-to-colony.md) prepares a new
  host and uses Apiary's approved join flow. It does not use the old manual
  SSH Unix-socket forwarding procedure.

This document is retained as the detailed combined reference from which the
two focused runbooks were split. Prefer the two documents above for new work.

# Detailed combined bootstrap reference

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

**Fast path**: `make setup-quick` collapses Steps 2-9 below into
one command for a single-node Path A bring-up on a genuinely fresh
checkout - packages, building every binary, `apiaryinstall`'s safe
fixes plus its one risky network step, installing binaries where the
rc.d scripts expect them, and then handing the whole configuration job
to `apiaryctl install --apply`, the installed binary that writes every
daemon's config, issues this Comb's TLS serving certificate in Go, and
works out its node_id (ADR-0147 Part 1). Log in with whichever UNIX
account ran this command (see Step 11's own note on why no separate
account-creation step exists). This whole document
assumes you are operating as root throughout, the same way a fresh
FreeBSD install typically drops you at a root shell - no command
anywhere here, including `make setup-quick` itself, calls `sudo`;
become root first (`su -`, or log in as root directly) if you aren't
already. Override its `NODE_*` variables for a host that doesn't match the
defaults, e.g. `make setup-quick NODE_VLAN_UPLINK=em0
NODE_HTTP_ADDR=10.62.0.2:8080`. They are passed straight through to
`apiaryctl install` as its own flags.

**What the installer will not do**, and this is the thing to know
before running it on anything you have already configured: it never
overwrites a field that is already set, never changes a `node_id`,
never completes half a TLS pair, and refuses to write an address the
certificate does not cover. It also refuses a config file that does not
parse, and one carrying a field the installed binary does not know
about. Run it with no `--apply` first to see the whole plan - it needs
no privileges to do that, and the plan names every field it would
leave alone and why:

```bash
/usr/local/libexec/apiary/apiaryctl install
```

A run that refuses anything still does everything else, and exits
non-zero, so a second run finishes the job. It does not cover Path B (joining an
existing Colony) or Step 12/13 (creating a network, VM, or jail) -
read on for those regardless, and read on generally the first time
through so a `setup-quick` failure is legible rather than a black box.

## 0. Gotchas to know before you start

**`managerd`'s `rpc_addr` is a per-node DNS hostname, not `0.0.0.0`, and
the failure when you get it wrong looks exactly like a TLS fault.** This
step used to tell you to write `"rpc_addr": "0.0.0.0:17700"`. That
guidance is withdrawn (ADR-0139), because `rpc_addr` is not only the
address `managerd` binds - it is also the address `raftd` *dials* on this
same node to confirm a restart to its local `managerd` (ADR-0125), and on
FreeBSD dialing `0.0.0.0:17700` is connection-refused. A four-node Colony
configured that way logged, live and per-node:

```
cannot reach managerd at 0.0.0.0:17700 (connecting to 0.0.0.0:17700: dial tcp 0.0.0.0:17700: connect: connection refused)
```

which reads like a certificate or trust-anchor problem and is not one at
all. Set it to this node's own DNS name instead - `brood.lab3.home.arpa`
on `brood` - and give every Comb's `frontend`/`restshimd` the same name
for their `manager_addr`. The hostname is not a style preference: this
node's serving certificate carries SANs `IP:127.0.0.1` and
`DNS:<node>.lab3.home.arpa` and no SAN for its LAN address, so the
hostname is the only host it can be verified against. See Step 8 and
ADR-0139 for the full reasoning.

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

**On bare-metal hardware, check BIOS virtualization before installing
FreeBSD at all - not after.** Confirmed on real hardware (a Lenovo
ThinkPad T540p/Haswell and a ThinkPad X270/Kaby Lake): laptops of this
era frequently ship with VT-x/EPT disabled in BIOS by default, and the
resulting failure is identical to Step 13's *nested*-virtualization
symptom (`hw.vmm.vmx.initialized: 0`) even though nothing about it points
at firmware. If this host is bare metal (not itself a VM), go into BIOS
and confirm virtualization extensions are enabled before you even start
Step 1 - it's a much shorter detour than debugging a `vmm.ko` failure
that turns out to be a BIOS setting.

**A laptop node's lid-close suspend looks exactly like a raft network
partition, not a power event.** If this node is a laptop, disable
lid-close suspend (and check `hw.acpi.cpu.cx_lowest`/
`performance_cx_lowest`) before it ever joins a cluster - the actual
symptom (missed heartbeats, an election on the surviving member) reads
as a networking or raft bug and is easy to chase in entirely the wrong
direction if you don't already know the real cause is "someone closed
the lid."

**Mixing CPU generations across cluster nodes creates a one-directional
migration hazard.** A newer-generation node (e.g. Kaby Lake) exposes CPU
instruction-set extensions an older-generation node (e.g. Haswell)
doesn't have. A guest that boots on the newer node and detects those
extensions can execute an unsupported instruction after migrating to the
older node - the guest dies on resume, not a clean migration error.
Migrating old-generation -> new-generation is safe; the reverse is not.
Until every guest's visible CPU features are masked down to the
cluster's lowest common baseline (other hypervisors solve this with
explicit CPU model definitions; whether bhyve currently exposes the
necessary knobs has not been determined as of this writing - may warrant
its own ADR), know which of your nodes is the oldest generation and
default to creating/booting new guests there first.

## 1. Base OS and source

```bash
pkg install -y git go
git clone https://github.com/glenjbarber/apiary.git ~/apiary
cd ~/apiary
```

(`go` here also satisfies `cmd/managerd`'s own cgo/PAM requirement -
`managerd` must be built natively on the FreeBSD host it will run on
(ADR-0030/ADR-0087; PAM moved from `frontend` into `managerd` in
ADR-0087, so `frontend` itself has no native-build requirement at
all).)

## 2. Build all five binaries

The repo's own `Makefile` does this in one step:

```bash
make build
```

`make clean` removes all five built binaries from the checkout root
(it does not touch anything installed under `/usr/local/libexec/apiary`
- see Step 6).

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

When the uplink currently uses DHCP, this writes an addressless physical
member plus a MAC-pinned `bridge0` using `SYNCDHCP`. It does not disable
FreeBSD's stock `/etc/devd/dhclient.conf`; that rule respects the normal
`dhcpif()` eligibility check and is not a conflicting DHCP client.

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

## 6. Runtime directories, rc.d scripts, and installed binaries

`make setup` runs four sub-targets in one pass, each independently
re-runnable if you only need to redo one piece:

- `setup-dirs` - creates the directories every `apiary_*` rc.d script
  or daemon expects to already exist (`/var/db/apiary/raftd`,
  `/var/db/apiary/isos`, `/var/run/apiary`, `/var/log/apiary` -
  `/var/log/apiary` is the one genuine gap, since a first `service
  apiary_raftd start` on a truly fresh host fails outright without it).
- `setup-rcd` - installs and enables the rc.d scripts themselves
  (`etc/rc.d/apiary_*`). Re-run this alone (`make setup-rcd`)
  after pulling a change to one of those scripts, without redoing
  PAM/TLS provisioning.
- `setup-pam` - writes a fresh `/etc/pam.d/apiary` only if one isn't
  already present, so a later hand-edited policy file is never
  clobbered by a re-run (see Step 11 for what this is for).
- `setup-tls` - **not part of `make setup` any more.** It generates a
  self-signed RSA certificate/key pair at
  `/usr/local/etc/apiary-tls/{cert.pem,key.pem}` (override the
  directory with `NODE_TLS_DIR`) only if that path doesn't already have
  one, and it is kept only for an operator who wants openssl's output.
  The certificate this document's Path A actually gets is issued in Go
  by `apiaryctl install`, as ECDSA P-256, and that installer keeps and
  reports whatever pair it finds already in place rather than replacing
  it - so running `make setup-tls` first is safe, and the RSA pair it
  writes is the one the Comb keeps. See ADR-0147 Part 1.

Run the three together:

```bash
make setup
```

Each rc.d script runs a fixed binary path, `/usr/local/libexec/apiary/<name>`
- not the copy sitting in this checkout - so install the daemons
there too, every time you rebuild them:

```bash
make install
```

`make install` builds the four daemons (`raftd`/`managerd`/`frontend`/
`restshimd`) plus `apiaryctl`, the operator command line, and makes
sure the runtime directories from Step 6 exist first, then copies each
binary to `/usr/local/libexec/apiary/<name>.new` and atomically renames
it into place - `cp` over a binary a live process still has open fails
with "Text file busy," but `mv`'s atomic rename doesn't disturb the
running process's already-open file descriptor at all.

`apiaryinstall` is deliberately **not** installed. It is a one-shot
host-prep CLI meant to be run from this checkout, never installed
permanently.

`apiaryctl` is the exception to the "this checkout is where the source
is" rule that the rest of this document assumes, and the reason is worth
stating once. Both of its subcommands are operations on a Comb rather
than on a checkout: `force-restart` restarts two running daemons, and
`install` configures a host. Both therefore have to be files on the
Comb; a Comb does not have this checkout on it, which is why a `make`
target could never be the operational path for either (ADR-0136,
ADR-0141, ADR-0147). They are the only things here you type by name
rather than by `make`, and they need nothing but a root shell. If
`apiaryctl` is missing after an install, the install did not run to
completion.

From here on, every "start the daemon" step below means `service
apiary_<name> start`, never a direct `daemon`/`./<binary>` invocation -
the rc.d scripts are what actually supervise these processes correctly
(auto-restart, correct pidfile handling on stop/restart - see the
"Not covered here" section's own history of getting this wrong by
hand). Re-run the binary-install loop above and `service apiary_<name>
restart` after every rebuild.

## 7. Start `raftd`

Do not start `apiary_raftd` merely to validate a newly prepared host. A fresh
raftd without `await_join` immediately bootstraps its own independent Raft
cluster. Start it normally only for the first node in a new Colony or for a
permanently standalone node. A node joining an existing Colony must remain
stopped until its `await_join` configuration is complete and the operator is
ready to begin the join flow.

As of ADR-0100, `raftd` takes no CLI flags for its steady-state
configuration - it reads `/usr/local/etc/apiary/raftd.json`
unconditionally (only `-reset`/`-restore`/`-restore-file`/
`-restore-dry-run`/`-export` remain CLI flags, and only for their own
one-shot recovery/export runs - see ADR-0038/ADR-0051). `make
install` (Step 6) already dropped a fully-commented reference copy at
`/usr/local/etc/apiary/raftd.json.sample` - copy it and strip its
comments as a starting point instead of writing the file from scratch:

```bash
mkdir -p /usr/local/etc/apiary
sed -e '/^[[:space:]]*\/\//d' -e '/^[[:space:]]*$/d' \
  /usr/local/etc/apiary/raftd.json.sample > /usr/local/etc/apiary/raftd.json
chmod 600 /usr/local/etc/apiary/raftd.json
```

Then edit the generated file with this separate command:

```bash
${EDITOR:-vi} /usr/local/etc/apiary/raftd.json
```

Decide first: does this node bootstrap its own independent cluster, or
join an existing multi-node Colony? The two paths diverge in what
`raftd.json` contains and rejoin at step 8.

### Path A - independent single-node cluster

Use this path only for the first node in a new Colony or a node that will
remain standalone.

Leave `join` unset entirely - do **not** set it to `""` explicitly,
just omit the key:

```json
{
  "data_dir": "/var/db/apiary/raftd",
  "socket": "/var/run/apiary/raftd.sock",
  "node_id": "<this-node-id>",
  "raft_bind": "<this-host-address>:17600"
}
```

Replace `<this-host-address>` with this host's real, reachable address.
Do not use `0.0.0.0`: Apiary also advertises this value to Raft peers.

That warning applies to `raftd`'s `raft_bind` on **17600** and to nothing
else in this document - Raft peer transport is a separate thing from
`managerd`'s manager RPC on **17700**, and the two fields have different
rules. Here the wildcard is wrong because the address is *advertised* to
peers. `managerd`'s `rpc_addr` on 17700 has the opposite problem
(Step 8): it is not advertised, but it is *dialed locally*, so the
wildcard is wrong there too - for a completely different reason.

Run it in the foreground once to confirm a clean single-node leader
election in the output, `Ctrl-C`, then start it via rc.d (Step 6 must
already have installed the binary to `/usr/local/libexec/apiary/raftd`
and enabled the service):

```bash
./raftd
service apiary_raftd start
```

### Path B - join an existing multi-node Colony

**This historical path is retired. Do not use `join` or an SSH Unix-socket
forward to join a different host.** Apiary now has a mutually authorized,
UI-driven join flow with `await_join`, reachability preflight, and an
Admin approval on the target Colony. Follow
[Add a node to an existing Colony](add-node-to-colony.md) instead, and keep
`apiary_raftd` stopped until that guide tells you to start it.

### Verify (Path A)

```bash
cat /var/log/apiary/raftd.log
ps auxww | grep raftd
```

## 8. Start `managerd`

As of ADR-0100, `managerd` takes no CLI flags for its steady-state
configuration either - it reads `/usr/local/etc/apiary/managerd.json`
unconditionally (only `-reset-managed`/`-factory-reset`/
`-factory-reset-extra-jails`/`-factory-reset-extra-datasets`/
`-export-host-config` remain CLI flags, and only for their own one-shot
runs - see ADR-0038/ADR-0069). This same file is also what the Machine
Configuration web page reads and writes later, for every field except
`node_id`/`rpc_addr`/`raftd_socket` (deliberately not exposed there -
see ADR-0100). `make install` (Step 6) already dropped a
fully-commented reference copy at
`/usr/local/etc/apiary/managerd.json.sample` covering every field
this daemon has, including the advanced ones (Cloudflare exposure,
peer forwarding, assumption-checker tuning) not shown below - copy it
and strip its comments as a starting point instead of writing the file
from scratch:

```bash
sed -e '/^[[:space:]]*\/\//d' -e '/^[[:space:]]*$/d' \
  /usr/local/etc/apiary/managerd.json.sample > /usr/local/etc/apiary/managerd.json
chmod 600 /usr/local/etc/apiary/managerd.json
```

Then edit the generated file with this separate command:

```bash
${EDITOR:-vi} /usr/local/etc/apiary/managerd.json
```

```json
{
  "raftd_socket": "/var/run/apiary/raftd.sock",
  "rpc_addr": "brood.lab3.home.arpa:17700",
  "node_id": "<this-node-id>",
  "zfs_base": "<your-pool-name>/apiary",
  "bhyve_bootrom": "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd",
  "bhyve_bridge": "bridge0",
  "uplink": "<uplink-ifname>",
  "iso_dir": "/var/db/apiary/isos",
  "tls_cert": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_key": "/usr/local/etc/apiary-tls/key.pem"
}
```

**`rpc_addr` is this node's own DNS name, and that is not
interchangeable with a wildcard or a LAN address** (ADR-0139). This field
does three separate jobs:

1. It is the address `managerd` binds with `net.Listen`, so peers can
   reach this Comb's external RPC API.
2. It is a **local dial target**: `cmd/raftd/confirm.go` reads this same
   `rpc_addr` out of this file and dials it, on this node, to confirm a
   pending `apiary_raftd` restart to this node's own `managerd`. An
   unspecified address is a legal bind and an illegal dial target - it
   binds fine and then refuses the connection - so `0.0.0.0` strands
   every restart confirmation.
3. It supplies the **port only** of the address other Combs use to reach
   this one. The host half of that is derived independently, as
   `node_id` + `peer_hostname_suffix` (`frontend.json`'s own field for
   the cluster overview page's peer dials), so the host is never
   advertised from this field.

Use the node's DNS name rather than its numeric LAN address
(`10.90.0.94`) because of what the certificate can verify. The serving
certificate `apiaryctl install` issued carries `IP:127.0.0.1` plus a
DNS SAN for the short hostname and another for the FQDN, and no IP SAN
for the LAN address (ADR-0139, and the certificate Step 6's `setup-tls`
still generates if you ran it carries `IP:127.0.0.1,DNS:$(hostname)`
only).
`cmd/raftd/confirm.go` leaves its TLS `serverName` empty, so Go verifies
that certificate against whatever host was dialed - so neither a
wildcard nor a bare `10.90.0.94` can verify, while
`brood.lab3.home.arpa` verifies because it is the SAN that is there.
Point `NODE_RPC_ADDR` at the same name if you use `make setup-quick`.

One consequence worth knowing before you write Step 9: a listener bound
to the node's own name resolves to that name's LAN address, so it serves
LAN clients and **deliberately does not serve `127.0.0.1`**. `frontend`
and `restshimd` must therefore name the same hostname in `manager_addr`,
not loopback.

`apiaryctl install` derives both of those `manager_addr` values from
the `rpc_addr` it is giving managerd, so this cannot be got wrong by
hand and then forgotten. If you are writing the config by hand instead,
it is the one field in this document that has to be read together with
`rpc_addr` rather than on its own.

**TLS is mandatory here, not an afterthought reserved for Step 11's
PAM login** - `tls_cert`/`tls_key` above already point at the
certificate `apiaryctl install` issued
(`/usr/local/etc/apiary-tls/cert.pem`/`key.pem` by default, or your
own `NODE_TLS_DIR` if overridden). managerd's external RPC API is
encrypted from this point on regardless of whether real login is ever
turned on - even a purely loopback-only single-node Comb gets TLS on
this channel, since it costs nothing to have it and Step 11 requires
it anyway the moment `pam_service` is set.

```bash
service apiary_managerd start
```

**Joining a multi-node Colony (Path B only)** - add these so this node's
`managerd` can forward writes to (and be forwarded to from) the other
Combs, and so the cluster-overview page can reach their host stats
(ADR-0029):

```json
  "peer_api_key": "<the-same-key-every-Comb-in-this-Colony-uses>",
  "peer_managerd_port": "17700"
```

A peer key is required once the Colony has any API key at all
(ADR-0023) - peer-to-peer forwarding goes through the same authenticated
`ManagerService` API as everything else. This value now lives directly
in `managerd.json` (mode `0600`, root-owned) rather than a separate
file a `-peer-api-key-file` flag pointed at - the whole reason for that
indirection (ADR-0096) was avoiding a literal flag value's `ps(1)`
visibility, which no longer applies once nothing is passed via argv at
all.

Add `peer_tls`/`peer_tls_hostname_map` too if the other Combs'
`managerd` instances serve TLS. Optionally, add `known_peer_addresses`
(ADR-0097) listing every Comb's `host:port` in this Colony, comma-
separated - once set, the join-colony RPCs refuse a `target_address`
outside that list, closing off the residual risk that an unauthenticated
caller could otherwise point this managerd at an arbitrary host. Leave
it unset on a Comb's very first bootstrap (before any peer addresses are
even known yet); it's safe to add once the Colony's membership is
settled.

Verify:

```bash
cat /var/log/apiary/managerd.log
ps auxww | grep managerd
```

A clean log shows one line: `managerd: listening on
brood.lab3.home.arpa:17700 (node-id=..., raftd-socket=..., ...)` and
nothing after it. The host in that line is whatever `rpc_addr` says, so
if it reads `0.0.0.0:17700` you still have the withdrawn wildcard from
this step's first warning, and a `raftd` log on this same node is
already showing `cannot reach managerd at 0.0.0.0:17700 (connecting to
0.0.0.0:17700: dial tcp 0.0.0.0:17700: connect: connection refused)`.
That message is an address-role failure, not a TLS one: it is `raftd`
trying to use the bind address as a dial target, and `0.0.0.0` is
unroutable for a client. Fix `rpc_addr` and restart `managerd`; do not go
looking at certificate trust.

Note also that `listening on` is a bind confirmation only. The
certificate check that actually bites is the one on the *client* side,
so a `managerd` that starts cleanly can still leave `frontend`,
`restshimd` and `raftd` unable to connect - which is why Step 9 uses the
same hostname for `manager_addr` rather than `127.0.0.1`.

## 9. Start `frontend` and `restshimd`

As of ADR-0100, both take no CLI flags either - `frontend` reads
`/usr/local/etc/apiary/frontend.json`, `restshimd` reads
`/usr/local/etc/apiary/restshimd.json`, each unconditionally. `make
install` (Step 6) already dropped a commented reference copy of
each alongside its real path (`frontend.json.sample`/
`restshimd.json.sample`) - copy and strip comments as a starting point
instead of writing either from scratch:

```bash
sed -e '/^[[:space:]]*\/\//d' -e '/^[[:space:]]*$/d' \
  /usr/local/etc/apiary/frontend.json.sample > /usr/local/etc/apiary/frontend.json
```

Then edit the generated file with this separate command:

```bash
${EDITOR:-vi} /usr/local/etc/apiary/frontend.json
```

```json
{
  "manager_addr": "brood.lab3.home.arpa:17700",
  "http_addr": "0.0.0.0:8080",
  "manager_tls": true,
  "manager_tls_ca": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_cert": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_key": "/usr/local/etc/apiary-tls/key.pem"
}
```

`manager_addr` is this node's own name and must match Step 8's
`rpc_addr` host exactly. It is not `127.0.0.1` and it is not copied
from a wildcard: a managerd listening on its own DNS name resolves to
its LAN address and does not serve loopback (ADR-0139), and
`manager_tls_ca` only establishes trust in the certificate - it does
not tell Go which name the certificate has to match, so the dialed host
still has to be a SAN.

`http_addr` **is** deliberately `0.0.0.0`, and this is the one wildcard
in these two services that is correct. The web UI is the operator's
browser surface: it is meant to be reachable from a LAN, and Step 10
opens it from another machine. Nothing inside the Colony dials
`frontend` either, so - unlike `managerd` - this address has no
dial-target role to break. Reach it at `https://<this-host-address>:8080`.

`manager_tls`/`manager_tls_ca` are required here, not optional -
`managerd` now always serves its external RPC API over TLS (Step 8),
so a plaintext dial from `frontend` would fail the handshake outright.
Point `manager_tls_ca` at the same certificate Step 6 generated (a
self-signed certificate has no public CA to verify against otherwise).

```bash
service apiary_frontend start
```

`restshimd` (Apiary's REST/JSON API, if you need it) follows the same
pattern:

```bash
sed -e '/^[[:space:]]*\/\//d' -e '/^[[:space:]]*$/d' \
  /usr/local/etc/apiary/restshimd.json.sample > /usr/local/etc/apiary/restshimd.json
```

Then edit the generated file with this separate command:

```bash
${EDITOR:-vi} /usr/local/etc/apiary/restshimd.json
```

```json
{
  "manager_addr": "brood.lab3.home.arpa:17700",
  "http_addr": "127.0.0.1:8081",
  "manager_tls": true,
  "manager_tls_ca": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_cert": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_key": "/usr/local/etc/apiary-tls/key.pem"
}
```

`manager_addr` and `manager_tls*` are for the same reasons as
`frontend` above - same hostname as Step 8's `rpc_addr`, same mandatory
TLS, same certificate-name requirement.

**`http_addr` is `127.0.0.1:8081`, not `0.0.0.0:8081`.** This reverses
what the shipped sample and this step used to say, and
`internal/restshimdconfig`'s own built-in default is already
`127.0.0.1:8081` - the documentation was contradicting the code. Four
things make loopback the right answer, and none of them is "it is more
secure in general":

- **Nothing inside the Colony ever calls `restshimd`.** The web UI has
  its own conversion path; `raftd`, `managerd` and `apiaryinstall`
  never dial it. Its only intended clients are external tooling -
  `curl`, a CI job, a future provider.
- **It is a full read/write control API.** `/v1/vms`, `/v1/jails` and
  `/v1/networks` all expose create, update, delete and migrate, and
  `POST /v1/isos` uploads install media. Exposed on a LAN address that
  is a colony-wide control surface, not a read-only status endpoint.
- **It has no authentication of its own.** `restshim` deliberately holds
  no static key: it forwards each caller's own `Authorization` header
  through to `managerd` unchanged (ADR-0024). Its entire security
  boundary is `managerd`'s per-key auth (ADR-0023), so on a Colony with
  no API key configured yet the header is decorative and the port is
  open to anyone who can reach it.
- **Loopback is where its clients already are, or can cheaply be.**
  Reach it remotely through an SSH local forward:

  ```bash
  ssh -L 8081:127.0.0.1:8081 brood.lab3.home.arpa
  ```

  then `curl` against `127.0.0.1:8081` locally, with the real API key
  attached. Only consider a LAN bind for `8081` once `managerd` API
  keys are genuinely configured and the keys are real credentials -
  not placeholder text.

Contrast `frontend` on `8080` immediately above: that one is meant to be
LAN-exposed, because it is the browser UI a human actually uses. Same
host, same software family, opposite exposure decisions, for that
reason and no other.

```bash
service apiary_restshimd start
```

Without `pam_service` on `managerd` (see Step 8), the web UI is open
to anyone who can reach the port - fine for initial verification, but
add real login (ADR-0030/ADR-0087) before this host is reachable from
anywhere untrusted. `frontend` itself has no login-related config field
at all - it asks `managerd`'s own `Status` RPC whether PAM is configured.

## 10. Verify

Open `https://<this-host-address>:8080` in a browser - the Colony
overview page should show this Comb as `Reachable`/`healthy` with its ZFS
pool and packet filter both reporting healthy/enabled.

Without `pam_service` set in `managerd.json`, every page loads with no
login at all and the Users page shows "no active session" - that's the
expected state with login disabled, not a bug. Step 11 turns real
login on.

## 11. (Optional but recommended) Real login via PAM

Skip this only for throwaway testing - without it, the web UI is open to
anyone who can reach the port. Since ADR-0087, PAM lives in `managerd`,
not `frontend` - a login password now travels over the RPC channel
between them, so `managerd` also needs `tls_cert`/`tls_key` set in its
own config for `pam_service` to be accepted at all (`managerd` refuses
to start otherwise, and `UpdateNodeConfig` rejects the same bad
combination through the web UI too). Step 8 already set `tls_cert`/
`tls_key` unconditionally, so that requirement is already satisfied -
this step only adds `pam_service`.

**Pick a PAM service name** (e.g. `apiary`) and create its policy file
with `make setup-pam` (bundled into `make setup` too, from Step 6) -
only if `/etc/pam.d/apiary` doesn't already exist, so it never
clobbers a hand-edited one:

```bash
make setup-pam
```

**Verify it landed correctly** before trying to log in - `cat -A` reveals
any hidden/stray characters:

```bash
cat -A /etc/pam.d/apiary
```

Expect exactly two clean lines, each ending in `$`, nothing squished
together.

**A real UNIX account is all PAM needs to authenticate against** - not
specifically one Apiary creates. If you're already logged into this
host as a suitable account (yourself, or whichever user is running
this bootstrap), you can use it directly; to create an additional
account instead:

```bash
pw useradd -n <username> -m -s /bin/sh
passwd <username>
```

**Add `pam_service` to `managerd.json`** (via `${EDITOR:-vi}
/usr/local/etc/apiary/managerd.json`, or through the Machine
Configuration page's TLS panel once `managerd` is already up) and
restart:

```json
  "pam_service": "apiary"
```

```bash
service apiary_managerd restart
service apiary_frontend restart
```

Restart `frontend` after `managerd`: it checks managerd's PAM status only at
frontend startup and keeps that login-enabled state for the life of the
process.

**Log in right away** - since no Apiary account exists on this Comb
yet, the first successful login automatically becomes Admin (ADR-0086;
the login page itself says so while this is true). Every login after
that first one needs an explicit role, granted through the Users page
(Admin-only, `/users`) - a PAM login for a username with no role
assigned is rejected outright, not silently downgraded to Viewer.

Log in as the intended Admin as soon as `managerd` comes back up:
whoever authenticates first wins the bootstrap, so leaving this window
open on a host reachable by other real UNIX accounts is a real, if
narrow, race - see ADR-0086's own disclosed risk.

`managerd` re-reads `/etc/pam.d/<service>` on every login attempt, not
just at startup - fixing the file doesn't require restarting `managerd`
again, only the first `pam_service` config change does.

## 12. Create your first network

The web UI's Networks page (Network -> Networks -> Create) is where a
Comb actually gets usable VM/jail networking - `apiaryinstall`'s own
network step (Step 4) only prepares the *host* (uplink, bridge). Two
real things to know before creating one:

**Free the uplink from `apiaryinstall`'s fallback bridge first.**
`bridge0` (from Step 4, `-bhyve-bridge`) is only ever a fallback for a
VM with no managed network attached. An untagged (`VLAN ID 0`) managed
network attaches the same uplink NIC directly into its *own*,
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

**A jail created with nothing else set has an empty root** - `jail(8)`
starts it "successfully" anyway, with nothing usable inside. Give it
real content via a `base_template` (ADR-0084) instead of discovering
this the hard way: on the node that will own the jail, populate a ZFS
template dataset once and snapshot it with the fixed name Apiary looks
for:

```bash
zfs create zroot/apiary/templates/freebsd-14
mount -t zfs zroot/apiary/templates/freebsd-14 /mnt
tar -xf base.txz -C /mnt
umount /mnt
zfs snapshot zroot/apiary/templates/freebsd-14@apiary-template
```

Then set `base_template: freebsd-14` (the web UI's "Create jail" page
has a field for this) when creating the jail - its root is cloned from
that snapshot the first time it's created, never re-cloned afterward.
This is manual and per-node by design (see ADR-0084's own disclosed
limitation): the identically-named template must exist separately on
every node a templated jail might land on, there's no cross-node fetch.

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

- ~~No rc.d scripts ship in this repo~~ **Resolved**: `etc/rc.d/apiary_raftd`/
  `apiary_managerd`/`apiary_frontend`/`apiary_restshimd` now ship real,
  working rc.d scripts (matching the service names ADR-0049 already
  assumed exist) - install with `make setup` (installs and enables the
  rc.d scripts, and writes a fresh `/etc/pam.d/apiary` if one isn't
  already present - see Step 11 below for what that's for), then
  `service apiary_raftd start` (and the others, in dependency
  order - each script's own `REQUIRE`/`BEFORE` lines handle that if you
  just use `service apiary_frontend start`, which pulls in the rest).
  As of ADR-0100, each daemon's own steady-state configuration lives in
  its own JSON file under `/usr/local/etc/apiary/` (`raftd.json`,
  `managerd.json`, `frontend.json`, `restshimd.json` - see Steps 7-9
  above for their shape), not `/etc/rc.conf`'s `apiary_<name>_args`.
  That variable is still read by each rc.d script (harmless if left
  empty) but should normally stay unset now - it exists only for
  raftd's/managerd's own one-shot recovery flags
  (`-reset`/`-restore`/`-export-host-config`/etc.), which are never
  meant to be persisted in rc.conf or any config file (see ADR-0100's
  own reasoning: a value left sitting in `apiary_<name>_args` would
  re-trigger on every `daemon(8)` respawn).

  **Found and fixed live, on real production hosts**: `daemon(8)`'s own
  pidfile can briefly outlive the process it supervised - `rc.subr`'s
  stop step only waits for the process to leave the process table, not
  for `daemon(8)` to finish unlinking its pidfile - so a `restart`'s
  immediate following `start` step races that still-present pidfile and
  refuses to launch a new instance (`daemon: process already running,
  pid: -1`), even though nothing is actually running. Every one of these
  scripts now has a `stop_postcmd` hook that removes the pidfile once
  `check_pidfile` confirms nothing matching it is still alive, closing
  that race.

  **Corrected - the real root cause of the repeated orphaned-process
  sightings was something else entirely.** Every one of these scripts
  originally used `-p pidfile` (daemon(8)'s *child*-pidfile option)
  together with `-r` (auto-restart). `daemon(8)`'s own man page names
  this exact combination as broken: "the `--child-pidfile` option will
  give you the child's ID to signal when you attempt to stop the
  service, causing daemon to restart the child." That's exactly what
  was happening on every single restart: `rc.subr`'s stop step correctly
  killed the real process, but the still-alive supervisor - never
  targeted at all, since the pidfile only ever named its child - saw its
  child die and, per `-r`, immediately spawned a replacement, racing the
  new instance the following `start` step brought up for the same port.
  Fixed by switching to `-P pidfile` (the *supervisor*-pidfile option)
  with `procname="/usr/sbin/daemon"` in every script - `rc.subr` now
  signals the supervisor directly, which forwards the signal to its
  child and exits cleanly instead of replacing it. The `stop_postcmd`
  hook above addresses a real but different, narrower timing race and
  is unchanged; it was never the actual fix for the orphan-respawn
  problem, which this `-P` change is.
- **HAST (`-enable-hast`)** - `apiaryinstall` can check for `hastd_enable`
  but never configures it, and the known `hastd` source patch (ADR-0022,
  FreeBSD bug 298085) is never applied automatically; see ADR-0026 for
  what real setup requires.
- **`internal_token`/raft transport TLS for a multi-node join** - step 7
  Path B covers the SSH-tunnel mechanics `join` needs across hosts and
  `internal_token`, but TLS between raft members (ADR-0078) is left to
  that ADR's own docs if the Colony requires it.
