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
  get a disk, a NIC (a per-VM `tap` device on a host bridge), and
  optionally an installer ISO attached as a CD-ROM.
- **`internal/isostore`** — installer images uploaded through the web
  UI, verified against a pasted SHA-256 as they stream to disk and
  refused outright on a mismatch, so an unverified image never lands in
  the store. See
  [ADR-0017](docs/adr/0017-iso-upload-and-hash-verification.md).
- **`internal/hoststats`** — CPU load, memory, ZFS pool capacity and
  health, per-disk SMART status (via FreeBSD's own `smart(8)`), and
  network interface counters, surfaced on the UI's default page. See
  [ADR-0018](docs/adr/0018-host-stats-and-multipage-ui.md).
- **`internal/hast`** — storage replication config/lifecycle management.
  Cross-node replication itself is currently blocked by an upstream
  FreeBSD `hastd` bug; we diagnosed it, found it was
  [already reported](https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=292322)
  with a fix ([D57511](https://reviews.freebsd.org/D57511)) awaiting
  review, and independently confirmed the fix works. See
  [ADR-0008](docs/adr/0008-hast-config-and-lifecycle.md) for the full
  trail.
- **`internal/restshim`** — a REST/JSON translation of the external gRPC
  API, for non-browser clients. Built and tested, not yet wired into its
  own running binary.

**Not yet implemented:**

- Real cross-node HAST replication in normal operation (see above —
  waiting on the upstream fix to merge, not something fixable here)
- Node scheduling: nothing decides which cluster node a VM should run on
  beyond whatever a caller sets directly, and `MigrateVM` doesn't exist
- A console for a running VM — you can boot one from an installer ISO,
  but there's no way to see or interact with it yet (noVNC is the
  planned next step)
- Importing VMs from other hypervisors (e.g. Proxmox): no disk-format
  conversion, and Apiary is UEFI-only. Linux containers have no path at
  all — jails share the host FreeBSD kernel
- Authentication: the web UI has an optional shared-password gate
  (HTTP Basic Auth, off by default); `raftd`, `managerd`, and
  `restshim` have none, and there are no user accounts or roles
  anywhere

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
- `internal/` — core logic: `bhyve`, `jail`, `zfs`, `hast`, `cluster`,
  `raft`, `manager`, `restshim`, `frontend`, `isostore`, `hoststats`
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
