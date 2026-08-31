# Apiary

A FreeBSD-native virtualization management platform for cluster
management, VM and container orchestration, storage replication, and a
web frontend.

Apiary is built around bhyve-backed virtual machines and jails under one
unified abstraction, with cluster consensus handled by a dedicated raft
agent rather than a bolted-on external dependency.

## Status

Past the design-only stage: every layer of the architecture below
exists and works end-to-end, from raft consensus up through a
browser-facing UI, with real VM provisioning happening automatically
underneath it. See [`docs/adr/`](docs/adr/) for the reasoning behind
each design decision, in order.

**Implemented and tested:**

- **`raftd`** — a real raft agent (HashiCorp's raft library, BoltDB-backed
  storage) supporting single-node bootstrap, joining an existing cluster,
  and removing a member, all over its own internal protocol.
- **`managerd`** — dials `raftd` and exposes an external gRPC API
  (`CreateVM`/`UpdateVM`/`DeleteVM`/`GetVM`/`ListVMs`/`Status`), and runs
  a periodic reconciliation loop that automatically provisions local ZFS
  storage for VMs assigned to its node — and, on nodes with hardware-
  assisted virtualization, a real running bhyve VM backed by that
  storage. Deleting a VM tears both back down for real (a soft-delete
  tombstone, reconciled by the owning node) rather than just removing
  the record.
- **`frontend`** — a server-rendered HTMX web UI; no client-side
  JavaScript framework, single self-contained binary. Four pages: host
  stats (the default view), the VM list, installer images, and a
  create-VM form. The create form picks a target node from the live
  cluster membership, and the VM table's State column reflects the
  reconciler's actual progress (`creating`/`ready`/`deleting`/`error`),
  not just what was requested, live-updating without a page reload.
- **`internal/zfs`, `internal/jail`, `internal/bhyve`** — dataset,
  jail, and VM lifecycle management, each tested against real FreeBSD
  hosts (VMs for `zfs`/`jail`, real bare-metal hardware for `bhyve`,
  since it needs genuine hardware-assisted virtualization). bhyve VMs
  get a disk, a NIC (a per-VM `tap` device on a host bridge), a VNC
  framebuffer for the console (below), and optionally installer media
  attached as either a CD-ROM or a second disk depending on what it
  actually is (below).
- **noVNC-based VM console** — every VM gets a real, interactive
  browser console (`/vms/{id}/console`), proxied over WebSocket straight
  to bhyve's own VNC framebuffer with no separate `websockify` process.
  See [ADR-0020](docs/adr/0020-novnc-console.md).
- **`internal/isostore`** — installer images uploaded through the web
  UI, verified against a pasted SHA-256 as they stream to disk and
  refused outright on a mismatch, so an unverified image never lands in
  the store. Whether an image is genuine ISO9660 or a raw bootable disk
  (e.g. a FreeBSD memstick image) is sniffed from the file itself, not
  trusted from its name, and attached to the right kind of device
  accordingly — an image misattached as a CD-ROM never boots. See
  [ADR-0017](docs/adr/0017-iso-upload-and-hash-verification.md) and
  [ADR-0021](docs/adr/0021-iso9660-sniffing-for-memstick-images.md).
- **`internal/hoststats`** — CPU load, memory, ZFS pool capacity and
  health, per-disk SMART status (via FreeBSD's own `smart(8)`), and
  network interface counters, surfaced on the UI's default page. See
  [ADR-0018](docs/adr/0018-host-stats-and-multipage-ui.md).
- **`internal/hast`** — storage replication config/lifecycle management.
  Cross-node replication was originally blocked by an upstream FreeBSD
  `hastd` bug; we diagnosed it, found it was
  [already reported](https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=292322)
  with a fix ([D57511](https://reviews.freebsd.org/D57511)) awaiting
  review, and independently confirmed the fix works. See
  [ADR-0008](docs/adr/0008-hast-config-and-lifecycle.md) for the full
  trail. All four project machines now run the patched `hastd`, and
  real VM disk replication is wired in for real — see below.
- **Real HAST-backed VM disk replication** — a VM can name a
  `replica_node_id` (caller-set, like `node_id`) and its disk is then
  replicated to that node for real data redundancy - not automatic
  failover, since only one machine in this project can actually run
  bhyve VMs. Verified live on a real 2-node raft cluster: `hastctl`
  reports `role: primary`/`status: complete` on the owning node and
  `role: secondary`/`status: complete` on the replica, with a real
  bhyve VM booted against the replicated device. See
  [ADR-0026](docs/adr/0026-hast-vm-disk-replication.md) - it also
  documents a genuinely subtle root-cause bug that took an extensive
  live-debugging session to isolate, worth reading before touching this
  code.
- **`restshimd`** — a REST/JSON translation of the external gRPC API
  (`internal/restshim`), for non-browser clients: `curl`, CI, or (once
  built) a Terraform provider. Runs as its own binary, dialing
  `managerd` the same way `frontend` does. Each caller's own
  `Authorization` header is forwarded straight through to `managerd`'s
  API-key check, rather than the binary holding one shared credential.
- **Session-based login** — an optional gate on the web UI
  (`APIARY_UI_USER`/`APIARY_UI_PASSWORD`, off by default): a real HTML
  login form, an in-memory session cookie (24-hour TTL, `HttpOnly` +
  `SameSite=Lax`), and open-redirect protection on the return path. See
  [ADR-0019](docs/adr/0019-session-based-login.md).
- **Network management** — VLAN/bridge management, real DHCP-backed IP
  allocation, and per-VM firewall rules (`/networks` in the web UI). A
  VM's IP/MAC are assigned deterministically inside the replicated raft
  log itself, so every node computes the same collision-free result
  independently. `internal/dhcpd` renders and reloads a real `dnsmasq`
  config; `internal/pf` loads each VM's rules into its own `pf(8)`
  anchor. Requires `dnsmasq` installed and `pf` enabled with an
  `anchor "apiary/*"` stanza as one-time host setup. See
  [ADR-0022](docs/adr/0022-network-management.md).
- **API-key authentication** — `managerd`'s external API can require a
  bearer API key on every call (`/apikeys` in the web UI to create/list/
  revoke; off by default, until the first key is created). Keys are
  replicated cluster-wide like other ephemeral state, only ever stored
  as a SHA-256 hash, and the raw value is shown exactly once on
  creation. Enabling it is a one-way door: revoking every key locks the
  cluster down rather than reopening it, with no way back short of
  restoring an older raft snapshot. See
  [ADR-0023](docs/adr/0023-api-key-authentication.md) and
  [ADR-0024](docs/adr/0024-restshimd-binary.md) for `restshimd`'s own
  per-request auth forwarding.
- **Jail orchestration** — jails now have the same lifecycle VMs do:
  a `JailDefinition` (`/jails` in the web UI to create/list/delete),
  reconciled by the same `Reconciler` that provisions VMs. A jail's
  root is a plain ZFS dataset by default, or — like a VM's disk — can
  name a `replica_node_id` to get HAST-replicated instead, formatted
  and mounted via a new `internal/ufsmount` package (a jail needs a
  real filesystem to chroot into, unlike a VM's disk which uses the
  raw HAST device directly). See
  [ADR-0027](docs/adr/0027-jail-orchestration.md).
- **Resource reclaim** — a VM reassigned to a different node no longer
  leaks its old node's dataset/bhyve VM: the reconciler detects and
  tears down its own leftover resources under a VM ID that's been
  reassigned elsewhere, without touching the (now-elsewhere-owned)
  record itself. Separately, `ForcePurgeVM` is a human-triggered escape
  hatch for a VM tombstoned by delete whose owning node never comes
  back to finish removing it - it only works on a VM already marked for
  deletion, and never touches that (unreachable) node's real resources.
  See [ADR-0025](docs/adr/0025-resource-reclaim.md).

**Not yet implemented:**

- Cross-node HAST replication works for real on this project's own four
  machines (all patched - see above), but the underlying fix still
  isn't merged upstream, so it isn't something a stock FreeBSD install
  elsewhere could rely on yet. Automatic failover of a replicated VM
  also isn't implemented - only one machine in this project can
  actually run bhyve VMs, so this is data redundancy, not HA. A
  replica's dataset also isn't cleaned up once its VM's (or jail's)
  record is fully purged (a deliberate consequence of never inferring
  teardown from an absent record - see ADR-0026/ADR-0027). Jail
  orchestration itself is implemented and tested but not yet
  live-verified against the project's real FreeBSD machines the way
  VM HAST replication was (see ADR-0027's own consequences)
- Node scheduling: nothing decides which cluster node a VM should run on
  beyond whatever a caller sets directly, and `MigrateVM` doesn't exist
- Multi-node console/network access: the noVNC console and the Networks
  page's bridge status both only work when the web UI and the VM/
  network's owning node are the same machine (see ADR-0020/ADR-0022) —
  no VNC credentials/encryption either, relying entirely on the login
  gate in front of it
- Network management is v1-scoped: `internal/dhcpd` only supports
  `/24`-or-smaller subnets, and firewall rules are a flat allow/block
  list with no priority/ordering beyond `pf`'s own rule evaluation
- Importing VMs from other hypervisors (e.g. Proxmox): no disk-format
  conversion, and Apiary is UEFI-only. Linux containers have no path at
  all — jails share the host FreeBSD kernel
- Authentication: the web UI has its own optional shared-password login
  gate, and `managerd`'s external API has its own separate optional
  API-key gate (both off by default, see above); `raftd`'s internal
  socket has none, and there are no user accounts or roles anywhere
  (one flat set of API keys, no scoping)
- **Tabled for now** (evaluated, deliberately deferred):
  - **Terraform support** — the infrastructure now exists (`managerd`'s
    API-key auth, `restshimd`'s own binary forwarding each caller's
    key), what's left is the provider itself: translating Terraform's
    plan/apply lifecycle to `restshim`'s create/read/update/delete calls
  - **Kubernetes support** — not an Apiary gap specifically; no one runs
    `kubelet` natively on FreeBSD (it assumes Linux cgroups/overlayfs).
    The viable path is Linux VMs under bhyve running normal Kubernetes
    nodes, the same approach Proxmox itself uses — achievable by hand
    today, but no Cluster-API-style automation exists yet

## Architecture

- **Language:**
  Go, across the entire stack
- **Hypervisor:**
  bhyve, for both VMs and containers
- **Storage:**
  ZFS, replicated node-to-node via HAST
- **Consensus:**
  a dedicated raft agent (HashiCorp's raft library), run as a separate
	process from the management daemon, communicating over a schema-defined
	Unix domain socket protocol
- **External API:**
  RPC-style first, with explicit named operations (CreateVM, MigrateVM, and
	so on); a REST translation layer sits on top afterward for broader client
	compatibility
- **Frontend:**
  a Go backend serving a JSON API, with HTMX handling interactive elements
	server-side

### Terminology

- **physical**:
  HAST-replicated disk bytes and ZFS datasets
- **ephemeral**:
  small JSON-shaped facts, such as cluster membership, VM definitions, and
	node ownership assignments

Physical data stays local per node, already replicated by HAST.
Ephemeral state is what raft actually replicates across the cluster.

## Repository layout

- `cmd/` — entry points for each binary (`raftd`, `managerd`, `frontend`)
- `api/` — protobuf schema definitions: `api/internalpb` (internal raft
  socket protocol) and `api/rpc` (external RPC API)
- `internal/` — core logic: `bhyve`, `jail`, `zfs`, `hast`, `ufsmount`,
  `cluster`, `raft`, `manager`, `restshim`, `frontend`, `isostore`,
  `hoststats`, `vlan`, `dhcpd`, `pf`
- `web/` — HTML templates and static assets for the frontend, embedded
  into the `frontend` binary at build time
- `docs/adr/` — architecture decision records; start here for why things
  are built the way they are

## Building and testing

Standard Go tooling: `go build ./...`, `go vet ./...`, `go test ./...`.
Packages that shell out to FreeBSD-only tools (`internal/zfs`,
`internal/jail`, `internal/hast`, `internal/bhyve`) skip their
integration tests cleanly on a non-FreeBSD dev machine; cross-compile
(`GOOS=freebsd GOARCH=amd64 go test -c ./internal/<pkg>`) and run the
resulting binary on a FreeBSD host to exercise them for real.

Proto changes require [`buf`](https://buf.build) (`buf generate`);
generated `.go` files are committed, so this isn't needed just to build.

## License

See [LICENSE](LICENSE).
