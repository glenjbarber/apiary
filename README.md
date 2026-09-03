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
  trail. Every project machine ran the patched `hastd` at the time
  (the three FreeBSD VMs among them were later decommissioned - see
  "Not yet implemented" below), and real VM disk replication is wired
  in for real — see below.
- **Real HAST-backed VM disk replication** — a VM can name a
  `replica_node_id` (caller-set, like `node_id`) and its disk is then
  replicated to that node for real data redundancy - not automatic
  failover, since nothing decides on its own that a node is down and a
  replica should take over. Verified live on a real 2-node raft cluster: `hastctl`
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
  raw HAST device directly). Verified live on a real 2-node cluster,
  both plain and HAST-replicated (`hastctl` reaching `role: primary`/
  `status: complete` and `role: secondary`/`status: complete`, with a
  real write propagating between them) — this also caught and fixed
  two real bugs in the HAST role-reconciliation logic VM replication
  already shared, both now covered by regression tests. See
  [ADR-0027](docs/adr/0027-jail-orchestration.md).
- **Resource reclaim** — a VM reassigned to a different node no longer
  leaks its old node's dataset/bhyve VM: the reconciler detects and
  tears down its own leftover resources under a VM ID that's been
  reassigned elsewhere, without touching the (now-elsewhere-owned)
  record itself. Separately, `ForcePurgeVM`/`ForcePurgeJail` are
  human-triggered escape hatches for a VM or jail tombstoned by delete
  whose owning node never comes back to finish removing it - they only
  work on a record already marked for deletion, and never touch that
  (unreachable) node's real resources.
  See [ADR-0025](docs/adr/0025-resource-reclaim.md).
- **Manual VM/jail migration** — `MigrateVM`/`MigrateJail` move a
  VM/jail's ownership to a second node, but only when that node is
  already a synced HAST replica (any other target would silently
  destroy the VM/jail's real data - see the ADR). Verified live: a
  real HAST-replicated jail failed over from `apiarium` to
  `freebsd-apiary` for real, `hastctl` reaching `status: complete` on
  both sides before and after. This also caught a serious latent bug -
  and surfaced a real, still-open gap - in how the reconciler writes
  back to raft when a resource's owning node isn't the current raft
  leader. See [ADR-0028](docs/adr/0028-migrate-vm-and-jail.md).
- **Cross-node reconciler write forwarding** — closes the gap
  ADR-0028 found: a VM/jail's owning node can now successfully report
  phase updates and complete a delete's final purge even when it isn't
  the current raft leader, by forwarding the write to the leader's own
  managerd over the existing, already-authenticated `ManagerService`
  API instead of exposing `raftd`'s internal socket over the network.
  Live-verified: repeating ADR-0028's migrate-then-delete test with
  this fix deployed, the record purged automatically with no manual
  `ForcePurgeJail` needed. Requires every node's managerd `-rpc-addr`
  to be bound to a real, network-reachable interface, not loopback
  (this project's own flag default) - see
  [ADR-0029](docs/adr/0029-cross-node-write-forwarding.md).
- **Tiered RBAC with PAM-backed web UI login** — real per-identity
  accounts (Viewer / Operator / Admin) replace the single shared
  username/password (ADR-0019). The web UI authenticates against a
  real PAM service (`-pam-service`), so Kerberos or Active Directory
  work transitively through the host's own PAM configuration
  (`pam_krb5`/`pam_ldap`/`pam_winbind`) with no bespoke client code in
  Apiary; usernames map to roles via an explicit `-role-map` flag,
  independent of any UNIX/AD group. API keys (ADR-0023) gain the same
  three-tier role. This is the one binary in the project that now
  requires `CGO_ENABLED=1` and a native FreeBSD build — confirmed
  live, `managerd`/`raftd`/`restshimd` are unaffected and still
  cross-compile cleanly from any platform. Live-verified end-to-end on
  real hardware: real PAM logins against genuine UNIX accounts, a
  wrong password rejected, an unmapped valid account rejected outright
  (default-deny), and Viewer/Operator sessions each correctly allowed
  and blocked at the right routes. Repeated failed logins for one
  username now lock that account out for a fixed cooldown, checked
  before the PAM backend is ever called. See
  [ADR-0030](docs/adr/0030-tiered-rbac-pam-login.md).
- **VM base disk images** — `VMDefinition` gains `base_image_name`,
  resolved by the reconciler exactly like `iso_name` (reusing
  `internal/isostore` as-is). When set, a VM's disk is seeded by copying
  the base image the first time it's created, instead of a blank
  truncated file — never re-seeded afterward. No format conversion: the
  uploaded file must already be a raw, directly-bootable disk image.
  This is the prerequisite for the Kubernetes Cluster API provider work
  below — a k8s node VM needs a real Linux OS with cloud-init already on
  its boot disk. `internal/restshim`'s REST `vm` shape also gained
  `iso_name`/`network_id`/`ip_address`/`mac_address`/`base_image_name`,
  previously missing entirely. See
  [ADR-0031](docs/adr/0031-vm-base-images.md).
- **bhyve serial console log capture** — every VM's `com1` is attached
  to an `nmdm(4)` pair and continuously drained to a plain log file
  (always on, like the noVNC console), for guest boot output the VNC
  framebuffer can't show (many cloud images redirect their console to
  serial). Built specifically to diagnose a real bug: a VM wasn't
  accepting SSH or taking its seed hostname despite booting and getting
  a DHCP lease — the serial log immediately showed cloud-init rejecting
  the seed ISO as invalid, traced to a missing Rock Ridge extension in
  the CAPI provider's own ISO builder (fixed there; see that repo).
  Requires the `nmdm.ko` kernel module, not loaded by default. See
  [ADR-0032](docs/adr/0032-bhyve-serial-console-log.md).
- **`raftd` internal-socket token auth + TLS everywhere** —
  `raftd -internal-token` adds a real shared-secret credential on top
  of the internal socket's existing file permissions (opt-in;
  `managerd`/`restshimd` are the only real callers, so a single secret
  rather than a tiered system). `managerd`/`restshimd`/`frontend` can
  all serve and dial each other over real TLS
  (`-tls-cert`/`-tls-key`, `-manager-tls`/`-manager-tls-ca`) — also
  opt-in, defaulting to today's plaintext behavior. Closes the
  "API key travels in plaintext" gap that mattered once ADR-0029
  required a real network-bound `-rpc-addr`. Live-verified on
  `apiarium` against a throwaway instance (production untouched): a
  missing/wrong token both correctly rejected, a plaintext client
  failed outright against a TLS-only `managerd`, and TLS dial-and-
  serve confirmed working end-to-end through a real `restshimd`. See
  [ADR-0033](docs/adr/0033-internal-transport-security.md).
- **Remote serial console log viewing** — closes the gap ADR-0032 left
  open: a new `GetVMSerialLog` RPC (read-only, same role tier as the
  console) and a `/vms/{id}/serial` web UI page, polling on a timer
  rather than streaming since a log has no continuous-framebuffer need.
  Capped server-side at 1MB regardless of what's requested, since a
  runaway VM's serial log can grow to megabytes within minutes. See
  [ADR-0034](docs/adr/0034-remote-serial-log-viewing.md).
- **Leader-only read forwarding** — `GetVM`/`ListVMs`/`GetJail`/
  `ListJails`/`ListNetworks` have always been leader-only reads; a real
  raft leadership change (surfaced by a live reboot) showed this as a
  broken VM list on whichever node wasn't currently leader. Mirrors
  ADR-0029's existing write-forwarding: a rejected read is transparently
  forwarded to the leader's own `managerd` over the same authenticated
  API, using the same `-peer-api-key`. See
  [ADR-0035](docs/adr/0035-leader-only-read-forwarding.md).
- **Cluster overview and per-node host page** — the default landing
  page now shows a lightweight, concurrently-fetched status row per
  known cluster node (reachable/unreachable, load, memory, ZFS pool
  health, `pf` status), rather than always showing whichever node the
  web UI happened to be colocated with. The old verbose single-node
  view moved to `/host/{id}`, addressable per node. See
  [ADR-0036](docs/adr/0036-cluster-overview-and-per-node-host-page.md).
- **Write RPC forwarding to the leader** — closes the write-side gap
  the read-forwarding above left open: `CreateVM`/`UpdateVM`/`DeleteVM`
  and the jail/network/API-key equivalents now forward a rejected
  "not the leader" write to the leader's own `managerd` and return its
  real response, instead of surfacing raft's own rejection straight to
  the caller. See
  [ADR-0037](docs/adr/0037-write-rpc-forwarding-to-leader.md).
- **Tiered reset CLI** — `raftd -reset` wipes only raft-replicated
  ephemeral state, leaving real ZFS datasets/bhyve VMs/jails/ISOs
  untouched; `managerd -reset-managed`/`-factory-reset` add two more
  tiers (destroy every Apiary-managed resource within existing scoping,
  or ignore scoping entirely with explicit extra-resource lists) - each
  gated behind its own hardcoded confirmation phrase. See
  [ADR-0038](docs/adr/0038-tiered-reset-cli.md).
- **Per-role password-change feature** — the Users page lets Admin
  change anyone's password, Operator change its own and Viewer's (never
  Admin's), and Viewer change no one's - backed by a real `pw usermod`
  call, requiring the actor's own current password to re-authenticate
  first. See
  [ADR-0039](docs/adr/0039-per-role-password-change.md).
- **Automatic image fetching at VM-creation time** — the create-VM form
  now shows every known node's stored images, not just the local node's,
  with a live cue for whichever ones aren't yet on the currently-selected
  node; `internal/cluster`'s reconciler fetches a missing image
  automatically from whichever peer has it the moment a VM actually
  needs it, rather than requiring a manual pre-copy step. (Supersedes an
  earlier, browser-triggered manual "copy to node" feature that didn't
  work reliably in practice and was reconsidered in favor of this.) See
  [ADR-0041](docs/adr/0041-image-fetching-at-vm-creation-time.md).
- **Fixed a real, previously-mysterious bhyve reliability bug**: a
  Linux guest VM could reliably boot into a sustained high host-CPU
  state with its serial console flooded by newlines, previously
  suspected to be a kernel/TSC-level lockup. Root cause: the host-side
  serial-log reader opened its end of the VM's `nmdm(4)` pair without
  disabling that endpoint's default terminal echo - since `nmdm`'s two
  ends are cross-wired like a null modem, that echo bounced every byte
  the guest wrote right back into itself as bogus keystrokes, forever.
  Confirmed independent of any guest/hardware factor by reproducing it
  with a bare `nmdm` pair and no VM at all. Fixed by putting the reader
  into raw mode - not as simple as it sounds, since a naive fix using
  two separate command invocations doesn't stick (the device's own
  `hupcl` flag resets it the moment the first one exits) - see
  [ADR-0042](docs/adr/0042-serial-console-echo-loop-fix.md) for the
  full trail, including how a hardware hypothesis test on a second real
  bhyve-capable node ended up being what surfaced the actual clue.
- **Fixed a related reliability gap in the same investigation**: a
  VM's `bhyve` process exiting for any reason (a guest reboot, a
  crash) used to leave the reconciler reporting it as running forever,
  since it only checked whether the kernel still had a `vmm(4)` context
  allocated - not whether `bhyve` itself was still alive. The guest
  never actually ran again until someone destroyed the stale context
  by hand. Fixed so a dead process is now torn down and relaunched
  automatically. See
  [ADR-0043](docs/adr/0043-vmexists-checks-real-process-not-just-vmm-context.md).
- **Every VM now gets a deterministic MAC address**, not just ones
  attached to an Apiary-managed network - a flat-bridge VM previously
  got whatever random MAC `bhyve` generated, making it impossible to
  set up a static DHCP reservation ahead of creating it. Surfaced a
  real, currently-invisible-in-the-UI gap too: the VM table never
  actually displayed a MAC address at all, even though it was already
  being tracked - now it does. See
  [ADR-0044](docs/adr/0044-deterministic-mac-for-every-vm.md).
- A real "Kubernetes-ready" Linux base image
  (`containerd`/`kubeadm`/`kubelet`/`kubectl` pre-installed), driving
  the first genuine, non-bypassed `kubeadm init` success on a real
  Apiary bhyve VM - two real bugs found only via live boot testing
  (a missing `conntrack` preflight dependency, and a DHCP
  client-identifier fix that broke the router's MAC-keyed static
  reservation from ADR-0044) were found and fixed along the way. See
  [ADR-0045](docs/adr/0045-kubernetes-ready-base-image-and-first-real-kubeadm-init.md).
- Fixed a real bug caught live in a browser: the VM table's periodic
  poll response mixed an out-of-band error-banner `<div>` in with the
  `<tr>` rows it swaps into the table body. htmx's response parser
  sniffs the response's first tag to decide whether to wrap it for
  table parsing, saw `<div>` instead of `<tr>`, and skipped that
  wrapping - so the browser silently dropped every `<tr>`/`<td>` tag
  (a stray table element with no table ancestor is a parse error per
  the HTML5 spec), collapsing the table's columns into one run of text
  on every single poll. htmx's own `useTemplateFragments` config, meant
  for exactly this, didn't fix it either (confirmed live in
  Safari/WebKit). Fixed by never mixing non-table content into this
  response - the error banner now arrives via an `HX-Trigger` header
  instead. See
  [ADR-0046](docs/adr/0046-vm-table-polling-corruption-from-oob-swap.md).

**Not yet implemented:**

- Cross-node HAST replication works for real on this project's own two
  remaining machines, `apiarium`/`apiverse` (both patched - see above;
  the three FreeBSD VMs this was originally verified on were later
  decommissioned when their host hypervisor was retired), but the
  underlying fix still isn't merged upstream, so it isn't something a
  stock FreeBSD install elsewhere could rely on yet. Automatic failover
  of a replicated VM also isn't implemented - this is data redundancy,
  not HA. A replica's dataset also isn't cleaned up once its VM's (or
  jail's)
  record is fully purged (a deliberate consequence of never inferring
  teardown from an absent record - see ADR-0026/ADR-0027)
- Node scheduling: nothing decides which cluster node a VM should run
  on beyond whatever a caller sets directly (`MigrateVM`/`MigrateJail`
  now exist, but only as a manual, explicit operator action - see
  ADR-0028)
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
- Authentication: the web UI supports real PAM-backed per-identity
  login with tiered roles (Viewer/Operator/Admin, ADR-0030), and API
  keys carry the same roles (ADR-0023) — both still off by default.
  Repeated failed logins for one username lock that account out for a
  fixed cooldown (checked before PAM is ever called), but there's
  still no direct Kerberos/LDAP client code (PAM's own host
  configuration bridges to both instead — see ADR-0030's "Deferred"
  section). `raftd`'s internal socket now supports a shared-secret
  token (`-internal-token`, opt-in, ADR-0033) instead of relying on
  file permissions alone, and `managerd`/`restshimd`/`frontend` can all
  serve and dial each other over real TLS (also opt-in, same ADR) —
  closing the "API key travels in plaintext" gap that mattered once
  ADR-0029 required binding beyond loopback.
- Importing VMs from other hypervisors (e.g. Proxmox): still no
  disk-format conversion, and Apiary is UEFI-only. **Partially narrowed
  by ADR-0031**: a VM's disk can now be seeded from a pre-uploaded raw
  base image (`base_image_name`, reusing `internal/isostore`) instead of
  always starting blank — the caller still has to supply an
  already-raw, already-bootable image. Linux containers have no path at
  all — jails share the host FreeBSD kernel.
- A separate `cluster-api-provider-apiary` repo implements a real
  Cluster API infrastructure provider (`ApiaryCluster`/`ApiaryMachine`/
  `ApiaryMachineTemplate`) driving Apiary through `internal/restshim`'s
  REST API. **Single-control-plane bootstrap is done** - see the
  ADR-0044/ADR-0045 bullets above for the ready base image, the
  deterministic-MAC/static-IP fix that unblocked a real
  `Cluster.spec.controlPlaneEndpoint`, and the real, non-bypassed
  `kubeadm init` success this all led to. Getting there found and fixed
  six real bugs along the way (none in the CAPI provider's own core
  design) - an `internal/pf.Flush` idempotency gap, a stale-cache
  duplicate-create case, a live `hastd`/`hastctl` crash-loop (filed
  upstream as
  [bug 298085](https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=298085)),
  a missing flat bridge on `apiarium`, a NoCloud seed ISO missing Rock
  Ridge extensions, and the two ADR-0045 image-build bugs (missing
  `conntrack`, DHCP client-identifier drift) - see the CAPI repo's own
  README and ADR-0022/ADR-0032/ADR-0044/ADR-0045 for the full trail.
  **Still not done**: a real, joined multi-node `kubeadm` cluster (only
  a single control-plane node has been verified so far) - v1 also has
  no load-balancer/HA control plane (a single control-plane node's own
  IP is used directly), and `providerID`-to-kubelet wiring has no
  automatic path (no cloud-controller-manager exists for Apiary),
  documented as a manual `preKubeadmCommands` step in the CAPI repo's
  own README.
- **Tabled for now** (evaluated, deliberately deferred):
  - **Terraform support** — the infrastructure now exists (`managerd`'s
    API-key auth, `restshimd`'s own binary forwarding each caller's
    key), what's left is the provider itself: translating Terraform's
    plan/apply lifecycle to `restshim`'s create/read/update/delete calls

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

- `cmd/` — entry points for each binary (`raftd`, `managerd`, `frontend`,
  `restshimd`)
- `api/` — protobuf schema definitions: `api/internalpb` (internal raft
  socket protocol) and `api/rpc` (external RPC API)
- `internal/` — core logic: `bhyve`, `jail`, `zfs`, `hast`, `ufsmount`,
  `cluster`, `raft`, `manager`, `restshim`, `frontend`, `isostore`,
  `hoststats`, `vlan`, `dhcpd`, `pf`, `pam`, `tlsdial`, `resetutil`
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

`cmd/frontend` links `internal/pam` (cgo) for real PAM login (ADR-0030)
and can no longer be cross-compiled from macOS the way every other
binary here can — build it natively on a real FreeBSD host instead.
`managerd`/`raftd`/`restshimd` are unaffected.

## License

See [LICENSE](LICENSE).
