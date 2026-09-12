# Apiary

A FreeBSD-native virtualization management platform for cluster
management, VM and container orchestration, storage replication, and a
web frontend.

Apiary is built around bhyve-backed virtual machines and jails under one
unified abstraction, with cluster consensus handled by a dedicated raft
agent rather than a bolted-on external dependency. Each physical machine
in the cluster is a **comb**; the VMs and jails it hosts are its
**cells**.

## Vocabulary

Apiary's product language draws on both the organization of the bees
and the physical structure of a real apiary:

- **Apiary** — the complete system.
- **Colony** — the entire swarm: every comb and everything running on
  them.
- **Comb** — one physical Apiary node/host.
- **Cell** — one individual VM or jail.

That gives `Apiary > Colony > Comb > Cell` — "Comb `apiarium` is
unreachable," "cell `web-01` is running," "move cell `web-01` to
another comb." "Colony" names the complete swarm, not an individual
node.

These are product and UI terms, not a code migration: they don't rename
any Go type, protobuf field, API resource, CLI flag, or storage
identifier. The rest of this document, the code, and every ADR
continue to say "node," "VM," and "jail," matching what's actually in
`api/`, `internal/`, and `cmd/`. See
[ADR-0076](docs/adr/0076-comb-hierarchy-rename.md) for why "Hive" was
retired (it reads as a typo of "bhyve" in prose) — the fifth tier this
document previously described (an unimplemented "Comb" meaning "the
cells collectively on one hive") is gone too, since ADR-0059 already
established it as UI-only grouping with no backing resource; "Comb"
simply took over the node-level term directly instead.

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
- **Cross-Hive console tunnel** — the console above previously only
  worked when the web UI and the VM's owning node were the same
  machine, since bhyve's VNC listener is deliberately loopback-only and
  never exposed on the network. A new bidirectional
  `ManagerService.ProxyVMConsole` closes that gap: the frontend forwards
  to the owning Comb's managerd over the existing authenticated peer
  path, and that managerd independently re-validates ownership through
  `GetVMConsole` before dialing its own local VNC socket — neither a
  caller-supplied host nor port is ever accepted. The raw VNC listener
  itself never leaves loopback; only one more authenticated gRPC hop is
  added. See
  [ADR-0065](docs/adr/0065-cross-hive-console-tunnel.md).
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
- **Cell lifecycle controls** — Operators can Stop, Start, or Restart a
  VM or jail from the web UI. Stop removes only the live bhyve or jail
  process while preserving the Cell definition and ZFS or HAST storage.
  Restart is durable desired state: reconciliation confirms the Cell has
  stopped before returning it to running, rather than depending on two
  racing client requests.
- **Disabled jail provisioning and explicit deletion** — a node with
  `-jail-enabled=false` used to skip reading jail intent entirely,
  which hid a real bug: a jail assigned to that node never appeared and
  its later deletion could never finish, because the reconciler that
  should have purged the tombstone never even looked at the jail list.
  The reconciler now always reads jail intent regardless of whether
  provisioning is enabled, reports an unsupported destination as a
  visible phase error instead of silence, and still lets an owner
  finish an explicit delete (removing any running jail/dataset first)
  even with provisioning disabled — a nil lifecycle driver is never
  treated as proof there's nothing to clean up. The protected
  `timemachine` jail is excluded from every jail-planning, wrapper, and
  factory-reset path as defense in depth beyond the normal `apiary-`
  prefix boundary. The jail panel now polls a complete, role-aware
  fragment every three seconds. See
  [ADR-0064](docs/adr/0064-disabled-jail-lifecycle.md).
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
  Apiary; usernames map to roles via a persisted, Users-page-editable
  map (ADR-0074), independent of any UNIX/AD group - a fresh Comb with
  no accounts yet grants Admin automatically to whoever logs in first
  (ADR-0086; there's no `-role-map` flag to hand-edit anymore). API
  keys (ADR-0023) gain the same
  three-tier role. The actual PAM check now lives in `managerd`, not
  `frontend` (ADR-0087) — `frontend` calls it over gRPC
  (`AuthenticatePassword`), which requires TLS between the two once
  `-pam-service` is set, since a login password now travels that
  channel. This means `managerd` is the one binary in the project that
  now requires `CGO_ENABLED=1` and a native FreeBSD build — confirmed
  live; `frontend` itself has no cgo dependency at all anymore and
  cross-compiles cleanly, along with `raftd`/`restshimd`. Live-verified
  end-to-end on
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
- **Raft transport TLS** — the raft member-to-member TCP transport
  (`internal/raft`, distinct from ADR-0033's own internal UDS/gRPC
  transport above) previously had no TLS option at all, not just "off
  by default." `-raft-tls-cert`/`-raft-tls-key`/`-raft-tls-ca` on
  `raftd` now enable a custom `raft.StreamLayer` over mutual TLS - each
  member presents its own certificate and verifies every peer's against
  a shared CA, appropriate since raft members are a closed, symmetric
  peer set rather than a public-facing API. Opt-in; all three flags
  unset preserves today's plain-TCP behavior exactly. Verified by this
  codebase's first automated multi-node raft test: two real
  `raft.Node`s, joined via `AddVoter` and replicating a real log entry,
  entirely over the new TLS transport. See
  [ADR-0078](docs/adr/0078-raft-transport-tls.md).
- **Remote serial console log viewing** — closes the gap ADR-0032 left
  open: a new `GetVMSerialLog` RPC (read-only, Viewer tier — a plain
  text log, unlike the interactive VNC console below, which needs
  Operator since it's a full bidirectional control tunnel, not a
  read-only view; see ADR-0096) and a `/vms/{id}/serial` web UI page,
  polling on a timer rather than streaming since a log has no
  continuous-framebuffer need.
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
- **Self-hosted outbound NAT for VM networks** - a network no longer
  has to share an existing external router/VLAN to reach the internet;
  the default path is now outbound NAT through the node's own uplink,
  with an optional `external_gateway` field for the case where a real
  router already serves the subnet. Found and fixed two real bugs
  along the way: the VLAN-tagging uplink and the NAT egress interface
  can differ once a node's own NIC has been bridged for other
  purposes, and network config was previously only ever applied once
  at VM creation, never re-synced for an already-running VM. See
  [ADR-0047](docs/adr/0047-external-gateway-networks.md) and
  [ADR-0048](docs/adr/0048-self-hosted-outbound-nat.md).
- **Machine Configuration page** - a new `/machine` page for per-node
  settings: which physical interface a node uses for VLAN tagging vs.
  NAT egress, which DNS server managed Cells receive by DHCP, a per-VM
  firewall-pause toggle for troubleshooting
  without losing the configured rule set, a ZFS dataset quota action,
  jail-provisioning control, and local Apiary service status. Admins can
  schedule a restart of `apiary_managerd` or `apiary_frontend` from the
  same page without changing `rc.conf`. The VLAN/NAT uplink fields are
  dropdowns populated from that Comb's own live interface inventory
  (state and addresses shown per option) rather than free text; a saved
  interface that's since disappeared stays selectable as
  `(saved, unavailable)` so it can be cleared without hand-editing JSON,
  and discovery failures are reported without discarding the saved
  configuration — the host still decides what's actually suitable for
  VLAN tagging or NAT, this is advisory only. See
  [ADR-0049](docs/adr/0049-machine-configuration-page.md) and
  [ADR-0066](docs/adr/0066-host-default-egress-contract.md).
- **Config injection hardening** — a security audit found that VM/jail/
  network IDs and node-config values (uplink interfaces, the DHCP DNS
  server, a Cloudflare Tunnel hostname) were validated only for
  non-emptiness/uniqueness, while being interpolated unescaped into
  generated `dnsmasq.conf`/`hast.conf`/pf-rule/cloudflared-YAML config -
  most seriously, a newline in a VM ID (used as a DHCP lease hostname)
  or a network's `bridge_name`/`external_gateway` let an Operator inject
  an arbitrary `dnsmasq.conf` directive, including `dhcp-script=`, which
  dnsmasq runs as root on every lease event. Every affected value now
  gets a strict allowlist at the point it's first accepted
  (`internal/raft.FSM`'s `applyCreate*`/`applySet*`, or
  `internal/nodeconfig.Manager.Save` for the one deliberately
  non-raft-replicated value), plus each generated-config renderer
  independently rejects a newline regardless of what its caller already
  validated. The same audit also closed an open-redirect bypass in the
  post-login flow (confirmed live in a real browser), added the missing
  `Secure` flag to the session cookie, and replaced the console
  WebSocket's permissive `CheckOrigin` with a real same-origin check.
  See [ADR-0067](docs/adr/0067-config-injection-hardening.md).
- **A real, joined multi-node Kubernetes cluster**, via the separate
  `cluster-api-provider-apiary` repo - a genuine 2-node cluster (one
  control-plane, one worker) bootstrapped through the actual upstream
  Cluster API kubeadm bootstrap/control-plane providers, not a bypass,
  with both nodes reaching `Ready` behind a real Calico CNI install.
  Getting there found and fixed three more real bugs on Apiary's own
  side: `dhcp-option=interface:` is invalid dnsmasq syntax (it should
  be `tag:`), which had silently broken DNS for every Apiary-managed
  network until now; the Kubernetes base image doesn't persist
  kubeadm's own `ip_forward`/`br_netfilter` prerequisites across a
  fresh boot (worked around in the CAPI repo's own example manifest
  rather than a third image rebuild); and a stale kubeconfig `Secret`
  left over from an earlier bootstrap approach, on the CAPI side's own
  Kubernetes management cluster. See
  [ADR-0050](docs/adr/0050-dnsmasq-tag-not-interface-scoping.md).
- **Fixed a real bug in `restshimd`'s and `frontend`'s ISO upload
  relay**: a very large (tens of GB) or long (several-minute) upload
  through either's REST/multipart endpoint would fail with a bare,
  content-free `"sending upload data: EOF"`, even though the client had
  sent every byte - isolated by comparing against a direct-to-managerd
  gRPC client, which succeeded reliably on the same file. Root cause:
  gRPC's own `ClientStream.Send` returns a plain `io.EOF` once a stream
  aborts for any reason other than local encoding - the real cause is
  only retrievable via `CloseAndRecv`, which both relays failed to call
  before giving up. Fixed by calling it and surfacing its error instead
  of the masked one, with a regression test. What actually aborts the
  stream in the first place on very large/long uploads specifically is
  still an open question - the fix makes the real reason visible for
  the next occurrence instead of resolving it.
- **A VM detail page** (`/vms/{id}`), a sectioned/responsive create-VM
  form, and permission-aware navigation and lifecycle controls (a
  Viewer never sees create/delete actions; API-key management is
  Admin-only) - Codex's first merged UI contribution, reviewed before
  merge.
- **A cleaner visual design** across every page (branded nav, status
  badges, panel cards, shared page-header blocks) - later superseded in
  large part by the hierarchical sidebar shell below, but the shared
  `.panel`/badge/page-header conventions it introduced are still used
  throughout.
- **Portable raft configuration export/restore** (`raftd -export`/
  `-restore`/`-restore-dry-run`) for moving or rebuilding a node's raft
  configuration without hand-editing BoltDB state. See
  [ADR-0051](docs/adr/0051-raftd-config-save-restore.md).
- **Redacted host-config export** (`managerd -export-host-config <dir>`)
  closes the gap raft's own export doesn't cover: a node's real
  `/etc/rc.conf`, `/etc/pf.conf`, and `/etc/master.passwd`. Every
  account's password hash is replaced with `*` (never an empty field),
  and `-peer-api-key`'s live value is redacted from the exported
  `rc.conf` — a local, read-only, one-shot CLI action, never a network
  RPC. Deliberately export-only: there is no restore/apply path, since
  automatically writing account or firewall data back to a live host is
  a distinctly higher-risk problem needing its own design. See
  [ADR-0069](docs/adr/0069-host-config-export.md).
- **Dependency Graph Simulator v1** - read-only "what happens if this
  node/network disappears right now" counterfactuals: raft quorum
  impact from live reachability (not just configured membership), which
  VMs/jails a node owns or backs as a HAST replica with a conservative
  recovery verdict, and which cells lose image availability. A
  mistyped or unknown target always returns an explicit error, never a
  report that looks identical to a clean result. See
  [ADR-0052](docs/adr/0052-dependency-graph-simulator.md),
  [ADR-0053](docs/adr/0053-managed-network-failure-simulation.md), and
  [ADR-0054](docs/adr/0054-image-availability-in-node-failure-simulation.md).
- **Automated Assumption Checks v1** - a small, named catalog of
  operator assumptions (e.g. "this network's bridge is up on every node
  that needs it") continuously re-verified against live cluster state,
  reusing the Dependency Graph Simulator's own per-node fan-out pattern
  rather than `ListNetworks`'s leader-only `bridge_status` field. See
  [ADR-0055](docs/adr/0055-automated-assumption-checks-v1.md).
- **Evidence-Aware Health v1** - every node's health rendering now
  cites the specific observation (and its age) behind each verdict
  instead of a bare status word, so a stale or missing observation is
  never mistaken for a healthy one. See
  [ADR-0056](docs/adr/0056-evidence-aware-health-v1.md).
- **Offline Recovery Handbook v1** (`/recovery-handbook`) - a
  point-in-time, printable guide answering "if this node is gone right
  now, what do I actually do," built from the same quorum/ownership
  facts as the Dependency Graph Simulator plus a real, disclosed
  three-state (survives/lost/unknown) quorum verdict. See
  [ADR-0057](docs/adr/0057-offline-recovery-handbook-v1.md).
- **Cell Path Trace v1** (`/trace`) - a stage-by-stage, evidence-labeled
  explanation of one VM's intended network path (cell state, virtual
  interface, managed network, DHCP, DNS, owner-comb bridge, firewall,
  route), each stage independently marked clear/blocked/unknown rather
  than stopping at the first problem - Codex's contribution, reviewed
  before merge. See [ADR-0058](docs/adr/0058-cell-path-trace-v1.md).
- **A hierarchical sidebar navigation shell** replacing the flat top
  nav, with light/dark theming and grouped sections (Colony, Network,
  Status, Media) - Codex's contribution; a print-CSS regression it
  introduced in the Recovery Handbook page (still targeting the removed
  `<nav>` element) was caught and fixed on review. See
  [ADR-0059](docs/adr/0059-hierarchical-sidebar-shell.md).
- **Operational Invariants v1** (`/invariants`) - a small, named catalog
  of safety rules (no HAST resource has two writable primaries, a
  cluster tolerates losing one more raft voter, a recoverable cell has
  a synced replica and a capable destination, a managed network has a
  working route) continuously evaluated to true/false/unknown with
  cited evidence - a missing observation is never treated as a passed
  check. See
  [ADR-0060](docs/adr/0060-operational-invariants-v1.md).
- **Why Not Engine v1** (`/why-not`) - read-only answers to concrete
  operator questions ("why can this cell not migrate," "why is this
  comb unsafe to reboot," "why is this cell not recoverable," "why can
  this network not provide connectivity"), citing the smallest actual
  blocker set from already-shipped mechanisms above rather than a dump
  of every warning, with proven remedies kept clearly separate from
  plausible-but-unverified ones. See
  [ADR-0061](docs/adr/0061-why-not-engine-v1.md).
- **Resilience Coverage Map v1** (`/resilience-coverage`) - enumerates
  every failure scenario already computable by the mechanisms above
  (comb failure, network failure, network connectivity, cell
  recoverability, HAST dual-primary, raft quorum tolerance) and
  classifies each by whether real evidence exists - simulated,
  untested, or unsafe/impossible to physically rehearse - never by
  whether the answer happens to be good news, and never rolled into a
  percentage. See
  [ADR-0062](docs/adr/0062-resilience-coverage-map-v1.md).
- **Cloudflare Tunnel exposure v1** - the operator pre-provisions one
  Cloudflare Tunnel per Comb by hand; Apiary reconciles which Cells are
  exposed into that Comb's own `cloudflared` ingress config, manages
  the `cloudflared` process lifecycle, and calls Cloudflare's DNS API
  to create/update just a CNAME record per exposed Cell - no Tunnel-
  provisioning API of Cloudflare's own is ever called, needing only a
  narrow Zone:DNS:Edit token. The project's first outbound third-party
  HTTPS API integration and first real internet-facing exposure
  surface, deliberately scoped to plain HTTP origin traffic only (raw
  TCP/HTTPS-origin would need Cloudflare Access client tooling or the
  separate Spectrum product - out of scope). See
  [ADR-0063](docs/adr/0063-cloudflare-tunnel-exposure-v1.md).
- **Origin CA certificate expiry health and scheduled renewal** - the
  Machine Configuration page's Origin CA panel (ADR-0072) now flags a
  locally issued certificate as "soon" (within 30 days of expiry) or
  "expired" instead of only showing a bare date, and an optional
  Auto-renew flag set at issuance time re-issues that certificate on
  its own, using the same hostnames and validity period, once it
  enters that window - checked hourly by a new independent tick in
  `managerd`, alongside the existing reconcile and Automated Assumption
  Checks loops. Certificate revocation and issuance for services other
  than `managerd` remain future work. See
  [ADR-0077](docs/adr/0077-origin-ca-expiry-health-and-renewal.md).
- **System settings expansion** - eight new panels on the Machine
  Configuration page (`/machine`) expose most of `managerd`'s
  remaining startup flags as editable settings: resource-scope paths,
  bhyve/VM tuning, HAST/jail-console/peer-TLS toggles, peer forwarding,
  TLS cert/key paths, Cloudflare Tunnel config, Assumption Register
  tuning, and the raftd internal token. Raft identity/plumbing flags
  (`-node-id`/`-rpc-addr`/`-raftd-socket`) stay `rc.conf`/restart-only
  by design; secrets (`-peer-api-key`/`-raftd-token`) are write-only,
  never displayed once saved; resource-scope paths are editable only
  while unset, since changing one after real resources exist under the
  old value orphans them rather than moving them. Every field still
  takes effect on the next `managerd` restart, not live, matching
  ADR-0049's original posture. See
  [ADR-0070](docs/adr/0070-system-settings-expansion.md).
- **Orphaned HAST resource discovery and cleanup** - closes a gap
  ADR-0026 named and left open: once a replicated VM/jail's record is
  fully purged (not just reassigned), the secondary node's own local
  HAST provider dataset has no signal left to clean itself up, since
  this project never infers teardown from a record's mere absence.
  `ListOrphanedHASTResources` (local-only, Viewer) reports this node's
  own `hast-vm-*`/`hast-jail-*` datasets with no VM/jail record - owner
  or replica - referencing them any more; `CleanupOrphanedHASTResource`
  (Admin) is the explicit, human-triggered action that actually
  destroys one, re-verifying it's still orphaned at the moment of the
  call. No web UI, matching `ForcePurgeVM`/`ForcePurgeJail`'s own
  precedent for this class of rare, destructive, human-judgment action.
  See [ADR-0073](docs/adr/0073-orphaned-hast-resource-cleanup.md).
- **Role-map editing UI** - closes the gap ADR-0030's own "Deferred"
  section named: who has a web UI role, and at what tier, no longer
  requires hand-editing `-role-map` and restarting `cmd/frontend`. The
  Users page gains three Admin-only actions - add a username with a
  role, change an existing account's role, remove an account entirely -
  persisted via a new `internal/loginconfig` package (physical,
  per-node, mirroring `internal/nodeconfig`'s own role for `managerd`).
  Refuses any edit that would leave zero Admin accounts. Adding an
  entry grants an already-existing PAM/UNIX account an Apiary role - it
  never creates the account itself, matching ADR-0030's own explicit
  scope. See [ADR-0074](docs/adr/0074-role-map-editing-ui.md). The
  `-role-map` flag itself is gone now - a fresh Comb with no accounts
  yet grants Admin to whoever logs in first instead. See
  [ADR-0086](docs/adr/0086-first-login-bootstrap-admin.md).

**Not yet implemented:**

- An interactive jail console (`jexec`-based) was built, shipped, and
  then removed entirely at the user's request - a jail's root shell
  shares the host's own kernel with no hardware isolation boundary,
  and every mitigation built for it (off by default, Operator-tier,
  fixed `/bin/sh` only) reduced but never eliminated that structural
  risk. See ADR-0068's own "Removed" section for the full reasoning
  and exactly what was taken out. No replacement was requested; direct
  host access remains the only way to get a shell inside a jail today.
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
- No VNC credentials/encryption on the underlying console connection
  itself — it relies entirely on the login gate in front of it (see
  ADR-0020/ADR-0065 above, which already closed the older cross-node
  console and Networks-page bridge-status gaps this bullet used to
  describe)
- Network management: `internal/dhcpd`'s subnet-size limit (previously
  `/24`-or-smaller only) is lifted - any valid IPv4 subnet with at
  least two usable host addresses is now supported, matching
  `internal/raft`'s own IP-allocation arithmetic. See ADR-0022's
  "Update" section. Firewall rules now carry an explicit `priority`
  (a higher number is evaluated later and wins under `pf`'s own
  unchanged last-match-wins semantics) - see
  [ADR-0075](docs/adr/0075-firewall-rule-priority.md). An existing VM's
  firewall rules can now be edited after creation too, via a dedicated
  `SetVMFirewallRules` command (not the general `UpdateVM`, matching
  every other frontend-initiated VM mutation) - see
  [ADR-0079](docs/adr/0079-vm-firewall-rule-editing.md). A managed
  network's Name can now be edited in place too, via a dedicated
  `SetNetworkName` command - every other field (subnet, VLAN, bridge,
  gateway) still requires the delete-and-recreate workflow ADR-0071
  established, since `Name` alone has no physical realization for that
  workflow's own hazard to apply to. See
  [ADR-0080](docs/adr/0080-network-name-editing.md). The guided
  network-replacement workflow that same ADR called for but never
  built now exists too: a "Check teardown status before recreating"
  panel on the Networks page queries every known Comb's own local
  artifact-cleanup record for a deleted network id and shows clear/
  still-present/unknown per Comb - an unreachable Comb is never
  mistaken for evidence that cleanup succeeded there. The Create form
  itself does not yet block automatically on incomplete teardown; the
  operator checks this panel first. See
  [ADR-0081](docs/adr/0081-guided-network-replacement-workflow.md).
- **`apiaryinstall`, a host preflight/provisioning tool** — every FreeBSD
  host prerequisite this project has ever disclosed only in ADR prose
  (`vmm.ko`/`nmdm.ko` loaded, `bhyve-firmware`/`dnsmasq` installed, a ZFS
  pool present, `pf` enabled with an `apiary/*` anchor, a bridge for the
  uplink NIC, PAM/HAST setup) is now a single command to check, with
  `-apply` to fix the safe ones automatically.
  Creating or modifying a bridge and attaching the uplink NIC to it is
  gated behind its own separate, exact-phrase-confirmed flag
  (`-apply-network yes-modify-network`) rather than the general `-apply`,
  since ADR-0022 already documents a real near-miss from exactly that
  operation over SSH. PAM/account setup, ZFS pool creation, and the known
  `hastd` source patch are permanently report-only — this tool tells you
  they're missing, it never attempts them. See
  [ADR-0082](docs/adr/0082-apiary-installer-preflight.md) for the design
  and [docs/bootstrap.md](docs/bootstrap.md) for a full step-by-step
  runbook (building all four daemons plus `apiaryinstall`, the network
  step's SSH risk, finding the `bhyve-firmware`/`edk2-bhyve` path, and
  bringing up `raftd`/`managerd`/`frontend`) written from a real, live
  first bootstrap of a fresh Colony VM.
- **Mutually-authorized Colony join** — joining an existing multi-node
  Colony is now a UI-driven action (Machine page → Join a Colony) rather
  than a boot-time `raftd -join` flag needing an SSH tunnel to satisfy
  its Unix-socket-only requirement. Modeled on device-pairing: the
  joining Comb shows a short code, an Admin on the existing Colony
  compares it against the same code on their own "Pending join
  requests" panel and approves. Building this surfaced a real,
  disclosed operational hazard (approving before the joining Comb is
  actually reachable can destabilize the existing Colony's leadership)
  and a real, disclosed gap that's since been resolved: `raftd -await-join`
  now gives it a passive "wait to be added, don't self-bootstrap" mode.
  A joining Comb can also cancel its own still-pending request, and an
  existing Colony Admin can delete one outright (distinct from Reject,
  which only ever marks a record, kept forever) — see
  [ADR-0083](docs/adr/0083-mutually-authorized-colony-join.md).
- **Real rc.d scripts** — `etc/rc.d/apiary_{raftd,managerd,frontend,restshimd}`
  now ship in the repo, closing a previously-disclosed gap
  (`docs/bootstrap.md` used to say none existed). Two real bugs found
  and fixed live on real production hosts in the process, both
  producing the same visible symptom (silently orphaned supervisor
  processes that keep retrying in the background and were caught live
  actually colliding with a real instance's port during a later
  restart): a `daemon(8)`/`rc.subr` pidfile-timing race (a `restart`'s
  immediate `start` step could race a not-yet-unlinked pidfile and
  refuse to launch, `daemon: process already running, pid: -1` -
  fixed with a `stop_postcmd` hook), and the actual dominant cause -
  every script used `daemon(8)`'s `-p` (child pidfile) together with
  `-r` (auto-restart), a combination its own man page names as broken:
  stopping the service only ever killed the tracked child, so the
  still-alive supervisor immediately spawned a replacement per `-r`,
  orphaned and racing the next restart for the same port. Fixed by
  switching every script to `-P` (supervisor pidfile), which `rc.subr`
  can actually signal to stop the whole thing cleanly. `make setup`
  installs and enables the scripts (plus writes `/etc/pam.d/apiary` if
  one doesn't already exist) instead of copy-pasting the commands from
  `docs/bootstrap.md` by hand.
- **Jail base images via ZFS clone** — a jail can now name a
  `base_template` at creation time, closing a real gap where a fresh
  jail's root was always an empty ZFS dataset (`jail(8)` doesn't care,
  so the jail "worked" but had nothing usable inside). An operator
  creates and populates a template dataset by hand
  (`<zfs base>/templates/<name>`, snapshotted as `<name>@apiary-template`)
  and the reconciler clones it into the jail's root the first time it's
  created — never re-cloned afterward, and not supported together with
  HAST replication (a replicated jail's root is a raw device, not a ZFS
  dataset). Deliberately more manual than VM base images (ADR-0031):
  templates are node-local, created outside Apiary, and not fetched
  across nodes — a disclosed limitation, not a built follow-up. See
  [ADR-0084](docs/adr/0084-jail-base-images.md).
- **Uplink admin down/up toggle, decoupled from bhyve** — the Machine
  Configuration page can now administratively bring a Comb's own
  uplink NIC down or back up (`ifconfig <uplink> down`/`up`), Admin-only
  and gated by a browser confirm dialog naming the real risk: this can
  disconnect the Comb's own network access, including the very browser
  session issuing it, if they share the same NIC — the same class of
  hazard ADR-0022 already hit once. By explicit user choice there's no
  automatic revert (an alternative modeled on ADR-0022's own proven
  rollback was offered and declined). Also fixed a real, previously
  undiscovered side effect while building this: VLAN/DHCP/PF wiring in
  `managerd` was nested inside bhyve being enabled, which silently also
  disabled uplink-mismatch health checking on any bhyve-disabled node —
  now keyed only on `-vlan-uplink`, independent of bhyve. See
  [ADR-0085](docs/adr/0085-uplink-admin-toggle.md). **Update**: bringing
  the uplink down now also immediately pauses outbound NAT for any
  self-hosted network using that interface (rather than leaving a
  stale `nat-to` rule silently pointing at a dead interface) — no
  separate "resume" step needed, since the reconciler already
  re-applies NAT on its own next tick once the interface comes back
  up. See [ADR-0088](docs/adr/0088-pause-nat-on-uplink-down.md).
- **VM snapshot and restore** — a VM's own ZFS dataset can now be
  checkpointed and rolled back from its detail page, so recovering from
  a botched in-guest change no longer means rebuilding the VM from
  scratch. Local-only (no raft, no cross-node replication of the
  snapshot itself), reached across nodes the same owner-forwarding way
  as the console and serial log (ADR-0065): the frontend resolves which
  Comb owns the VM and dials that Comb's managerd directly. Restoring
  refuses (best-effort, based on raft's last-known state) while the VM's
  desired state is running, and rollback refuses outright rather than
  silently destroying a newer snapshot if one exists. See
  [ADR-0090](docs/adr/0090-vm-snapshot-restore.md).
- **Create a VM from an existing VM's snapshot** — the create-VM form
  gained a "Clone from snapshot" section (cascading dropdowns: pick a
  source VM, then one of its ADR-0090 snapshots) that seeds the new
  VM's disk as a real ZFS clone instead of a blank disk or a copied
  base image. Node-local only, like ADR-0084's jail base templates - no
  cross-node fetch if the source snapshot lives elsewhere. See
  [ADR-0095](docs/adr/0095-create-vm-from-snapshot.md).
- **Single-node is first-class, not a lesser bootstrap state** — a full
  audit confirmed raftd's default bootstrap, health/assumption checks,
  and every peer-forwarding feature (ISO/jail-template fetch, console/
  serial-log/VM-snapshot forwarding) already degrade cleanly with no
  peers configured. Three real gaps are fixed: the raft package doc
  comment no longer calls single-node a "(for now)" state; the ISO/jail-
  template peer-fetch error messages now say plainly that this is
  expected on a single-node deployment rather than reading as an
  incomplete setup; and the Resilience Coverage Map no longer renders a
  single-node cluster's quorum-tolerance scenario as a failing
  `unsafe_or_impossible` badge purely because it has one voter — a
  fragile multi-node cluster (e.g. one live voter left out of three)
  still renders that way, since that IS a real, actionable hazard. See
  [ADR-0091](docs/adr/0091-single-node-first-class.md).
- **Join-a-Colony now actually reaches the target you name** — the
  "Join a Colony" form (Machine page) gained a required "Existing
  Colony member's address" field, and submitting it now really dials
  that address, rather than silently recording the request on whichever
  Comb's own page happened to receive the form (the confusing,
  backwards-and-invisible-on-the-real-target bug ADR-0083's own
  described design never actually got built to prevent). Status polling
  and Cancel now follow the same target automatically. See
  [ADR-0092](docs/adr/0092-join-colony-target-address.md).
- **Peer-forwarding TLS can now trust self-signed certificates** — a
  new `-peer-tls-ca` flag (`managerd`/`frontend`, also editable live on
  the Machine page's "Peer forwarding" panel) is the peer-to-peer
  equivalent of `-manager-tls-ca`: point it at a PEM file (one or more
  peers' own certificates concatenated together) to trust instead of
  the system pool. Without it, two Combs with self-signed certificates
  could never successfully forward a peer RPC to each other at all -
  found live while verifying ADR-0092: even a managerd dialing its own
  address failed with `x509: certificate signed by unknown authority`.
  See [ADR-0093](docs/adr/0093-peer-tls-ca-trust.md).
- **A new `apiaryinstall` preflight check, found live on a real
  reboot** — `dnsmasq-rc-enable` flags `dnsmasq_enable=YES` in
  `rc.conf`, which races Apiary's own `internal/dhcpd` (it already
  restarts dnsmasq itself on every network change) against the
  system's boot-time `rc.d` start, before `managerd`'s reconciler has
  had a chance to recreate the network interface dnsmasq is meant to
  serve. `apiaryinstall -apply`-fixable. A second check,
  `devd-dhclient-conflict`, was also added after the same reboot -
  disabling FreeBSD's stock `/etc/devd/dhclient.conf` rule, based on a
  theory that it was independently DHCPing the bridged uplink NIC and
  duplicating `-bhyve-bridge`'s own lease for the same MAC. That theory
  was disproven immediately afterward: this host's own `dhcpif()`
  logic (`/etc/network.subr`) refuses to start `dhclient` on an
  interface with no `DHCP` token in its `ifconfig_<if>` value, and
  devd's rule only ever calls `service dhclient quietstart` (not
  `forcestart`), so it should never have been able to start it in the
  first place - confirmed directly on the host (`dhcpif em0` returns
  false). The real mechanism behind the duplicate lease (reproducible
  on every boot since at least Sep 2) is still unidentified. The
  `devd-dhclient-conflict` check and its live fix on `apiverse`/
  `apiarium` are harmless either way, just not a confirmed fix for
  anything. See
  [ADR-0094](docs/adr/0094-boot-time-network-robustness.md).
- **Security audit follow-up (six findings)** — a caller-supplied
  `target_address` on the deliberately-unauthenticated join-colony RPCs
  (ADR-0092) could make managerd dial an attacker-chosen host and hand
  over its own `-peer-api-key`; a new `dialUnauthenticated` closes it.
  `-peer-api-key` also gained a `-peer-api-key-file` alternative
  (mirroring `-cloudflare-token-file`'s own precedent) since a value
  passed directly as a flag is visible to any local user via `ps(1)`
  regardless of `/etc/rc.conf`'s permissions. `AuthenticatePassword`
  (ADR-0087) now enforces its own lockout, not only `frontend`'s -
  calling it directly used to bypass rate-limiting entirely. VM console
  access (`GetVMConsole`/`ProxyVMConsole`) moved from Viewer to Operator
  tier, since a console is a full bidirectional control tunnel, not a
  read-only view. `clone_from_snapshot`/`base_template` (ADR-0095/
  ADR-0084) gained the same FSM-boundary validation ADR-0067 already
  requires for every other interpolated identifier. A follow-up
  independent audit (`docs/audits/2026-09-12-security-audit.md`)
  confirmed all six land correctly and flagged one residual, disclosed
  risk (`target_address` still lets a caller make managerd dial an
  arbitrary host, just without the credential attached) - the dial-hang
  half of that is now bounded by a 10s timeout regardless of the
  caller's own request deadline; the reachability-oracle half was left
  for a follow-up design decision (below). See
  [ADR-0096](docs/adr/0096-security-audit-follow-up.md).
- **Join-flow hardening: target-address allowlist and a pre-approval
  reachability check** — closes the reachability-oracle finding
  ADR-0096 left open, and a real, separately-hit incident: a new opt-in
  `-known-peer-addresses` flag restricts `target_address` on the three
  unauthenticated join-colony RPCs to an operator-listed set of
  `host:port` values (unconfigured, the default, keeps ADR-0092's
  original accept-any behavior - forcing this on every deployment would
  break a fresh bootstrap with no migration path). Separately,
  `ApproveJoinRequest` now dials the pending request's own
  `raft_bind_address` before ever calling `AddVoter` and refuses to
  approve an unreachable one - approving before a joiner is actually up
  has stranded this project's own raft cluster twice, and `AddVoter`
  commits immediately with no clean way to reverse a bad membership
  change short of wiping raft state entirely. See
  [ADR-0097](docs/adr/0097-join-flow-hardening.md).
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
  **A real, joined multi-node `kubeadm` cluster is done too** - see the
  bullet above under "Implemented and tested." **Still not done**: v1
  has no load-balancer/HA control plane (a single control-plane node's
  own IP is used directly), and `providerID`-to-kubelet wiring has no
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
