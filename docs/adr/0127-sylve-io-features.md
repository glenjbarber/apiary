# ADR-0127: Sylve.io Feature Adoption for Apiary

**Date:** 2026-09-26
**Status:** Proposed
**Author:** Goose (subagent)

## Context

The user recorded a TODO: *"Implement features in place by sylve.io"* (2026-09-17). This ADR researches what Sylve is, catalogs its feature set, and determines which features are relevant and desirable for Apiary.

### What is Sylve?

[Sylve](https://sylve.io) is an **open-source infrastructure management platform built for FreeBSD** (BSD-2-Clause license). It describes itself as a "Management Plane for FreeBSD" — a unified web interface for:

- **Bhyve virtual machines** (VMs)
- **FreeBSD Jails** and **Linux Jails**
- **ZFS storage** (pools, datasets, zvols, snapshots, replication)
- **Networking** (bridges, VLANs, DHCP, routing, PF firewall, NAT, WireGuard, mDNS)
- **File services** (Samba/SMB, iSCSI initiator and target)
- **Clustering** (multi-node with Raft, guest migration, replication policies, backup/restore)
- **Security & services** (authentication, certificates/Let's Encrypt, Dynamic DNS, notifications, auditing)
- **Operations** (dashboards, telemetry, local CLI, interactive console)

**Tech stack:** Go backend, SvelteKit frontend. Installed via `pkg install sylve` on FreeBSD 15.0+. Web UI on port 8181.

**Current release:** v0.3.0 (largest release to date, 500+ commits since v0.2.3).

---

## Sylve Feature Catalog (v0.3.0)

| Area | Features |
|------|----------|
| **Virtualization** | Bhyve VMs (create/start/stop/restart/delete, snapshots, templates, Cloud-Init, PCI passthrough, CPU pinning, VNC/serial console, staged same-cluster migration); FreeBSD Jails (pkgbase bootstrapping, multiple interfaces, VLANs, FSTab presets, DevFS management, console log rotation, Linux Jail support); VM/Jail templates |
| **Storage** | ZFS pool/dataset/zvol management; periodic snapshots; replication (early access, behind feature flag); S.M.A.R.T. diagnostics (scheduled/on-demand self-tests, health monitoring); iSCSI initiator & target (CHAP, portals, ZFS-volume LUNs); Samba/SMB shares (Apple extensions, mDNS, audit logs); disk partitioning/wiping (GPT, 4Kn detection) |
| **Networking** | Switches/bridges (standard & manual); VLANs; DHCP server (dnsmasq); PF firewall (ordered rules, NAT/masquerade, network objects, live logging); WireGuard (server with peer provisioning, QR export; persistent outbound clients); mDNS records; IPv4/IPv6 routing with network-object references |
| **Clustering** | Raft-based multi-node cluster (3+ voters for HA); staged guest migration (recursive ZFS send while online, final incremental sync, cutover); replication policies (ownership leases, fencing, autostart protection, standby recovery); backup/restore (manifest-backed generations, recursive datasets, multi-pool VM roots, in-place & out-of-band restore, encrypted sources) |
| **Security & Services** | Certificate management (import PEM, self-signed, Let's Encrypt, Sylve.app managed); Dynamic DNS (Cloudflare, Namecheap, Sylve.app); Notifications (in-app, ntfy, SMTP, Discord; templates for S.M.A.R.T., ZFS events); Authentication (local users, PAM optional, login rate limiting, JWT sessions, cluster token scopes, audit logging) |
| **Operations** | Dashboards (node, VM, Jail, network, ZFS with historical charts); telemetry (ARC/L2ARC, scrub/resilver, bandwidth/IOPS/latency); workload activity/audit logs; local root-only CLI & interactive TUI console (Unix socket, VM/Jail/network/storage/downloads workflows); localization (CZ, DE, zh-CN) |

---

## Apiary Architecture Summary

Apiary is a **FreeBSD HA infrastructure orchestration system** with different design priorities:

| Concept | Description |
|---------|-------------|
| **Comb** | A single FreeBSD node running Apiary daemons |
| **Colony** | A Raft cluster of Combs (3+ voters for quorum) |
| **Cell** | A logical failure-domain grouping of Combs |
| **Flight Plan** | A declared operational intent with rollback/recovery semantics |
| **Manager (managerd)** | gRPC API daemon (port 17700) for VM/Jail/network/storage control |
| **Frontend (apiary_frontend)** | Web UI (port 8080) served via Go templates |
| **Raft (raftd)** | Consensus layer (port 17600) for replicated state |
| **Restshim (restshimd)** | REST shim (port 8081) for external integrations |

**Apiary's differentiating pillars** (from SHARED.md):
1. **Failure-Domain Integrity** — Model shared power/storage/network/credential dependencies so replicas/Raft voters cannot appear independent merely by having different node IDs
2. **Blast-Radius Budget** — Operator-defined limits on simultaneous exposure (critical cells unprotected, replica pairs degraded, service contracts in reduced mode)
3. **Hidden Dependency Discovery** — Use approved metadata to suggest undeclared dependencies missing from Cell Service Contracts
4. **Operational Continuity Scorecard** — Evaluate operability through specific losses (admin, IdP, management network, site, vendor, automation path)
5. **Air-Gapped Operations Courier** — Physical media workflow for disconnected environments

**Current implemented capabilities:**
- VM lifecycle (bhyve via libvirt) — create, start, stop, restart, delete, consoles
- Jail lifecycle — create, start, stop, restart, delete, VNET networking (ADR-0117)
- ZFS storage — pools, datasets, datastores for VMs/Jails
- Networking — VLANs, bridges, DHCP, static IP allocation (raft-allocated)
- Colony join — guided & manual workflows, TLS trust prompts (ADR-0113), automatic peer TLS hostname map (ADR-0115)
- Images/ISOs — cluster-wide view (ADR-0110)
- Frontend restart routing (ADR-0111), common.json config (ADR-0112)

---

## Feature Mapping: Sylve → Apiary Relevance

### HIGH RELEVANCE — Directly aligns with Apiary's roadmap or fills gaps

| Sylve Feature | Apiary Gap / Alignment | Recommendation |
|---------------|------------------------|----------------|
| **Staged same-cluster VM/Jail migration** | Apiary has no guest migration. Critical for maintenance, failure-domain rebalancing, Flight Plan execution. | **Adopt** — Design as a Flight Plan primitive; reuse Raft for coordination; leverage existing ZFS send/recv infrastructure |
| **ZFS replication (early access)** | Apiary has no dataset replication. Needed for DR, cross-cell sync, blast-radius reduction. | **Adopt** — Integrate with Cell replication policies; respect failure-domain boundaries; gate behind feature flag like Sylve |
| **Manifest-backed backup generations** | Apiary has no backup system. Required for Operational Continuity Scorecard evidence. | **Adopt** — Design as Flight Plan recoverable action; coordinate with replication; support multi-pool VM roots |
| **PF firewall management (rules, NAT, objects, logging)** | Apiary has no host firewall management. Needed for network segmentation, Cell isolation, blast-radius enforcement. | **Adopt** — Integrate with VLAN/switch model; express as declarative network policy per Cell |
| **WireGuard (server + client)** | Apiary has no VPN. Needed for management network redundancy, air-gapped courier sync, cross-site Colony. | **Adopt** — Model as a Cell interconnect; integrate with peer TLS hostname map; support QR/export for courier |
| **Certificate management (Let's Encrypt, self-signed, import)** | Apiary manages TLS manually via config files. Automated cert lifecycle reduces operational burden. | **Adopt** — Integrate with Colony join flow; auto-renew for frontend/restshimd/managerd/raftd listeners |
| **Dynamic DNS** | Apiary has no DDNS. Useful for Colony nodes behind changing IPs, courier rendezvous. | **Adopt** — Low priority; integrate with certificate management for unified FQDN control |
| **Notifications (ntfy, SMTP, Discord, in-app)** | Apiary has no notification subsystem. Needed for Flight Plan status, degradation alerts, Scorecard rehearsals. | **Adopt** — Design as pluggable transport layer; integrate with audit log; support templated rules |
| **S.M.A.R.T. diagnostics & disk health** | Apiary has no disk health monitoring. Critical for Failure-Domain Integrity (shared storage dependencies). | **Adopt** — Feed into dependency discovery; alert on shared-disk degradation; correlate with ZFS pool health |
| **Local CLI & interactive console (Unix socket)** | Apiary has no local CLI. Needed for air-gapped operations, recovery when frontend/restshimd down. | **Adopt** — Root-only Unix socket; expose Flight Plan, Cell, Comb, Raft diagnostics; TUI status bar |

### MEDIUM RELEVANCE — Valuable but lower priority or partially covered

| Sylve Feature | Apiary Gap / Alignment | Recommendation |
|---------------|------------------------|----------------|
| **VM templates & Cloud-Init** | Apiary VM creation is manual. Templates accelerate Fleet Plan rollout. | **Defer** — After migration/backup; integrate with ISO/image cluster-wide view (ADR-0110) |
| **PCI passthrough & CPU pinning** | Apiary VM config lacks hardware passthrough. Needed for specialized workloads (GPU, NIC). | **Defer** — After core migration/backup; requires libvirt XML extension |
| **iSCSI initiator & target** | Apiary has no block storage networking. Useful for shared storage across Cells. | **Defer** — After ZFS replication; model as cross-Cell storage interconnect |
| **Samba/SMB shares** | Apiary has no file sharing. Useful for ISO distribution, backup targets. | **Defer** — Lower priority; can use NFS/rsync externally for now |
| **mDNS discovery** | Apiary uses explicit config (peer_tls_hostname_map). mDNS could simplify join. | **Defer** — Nice-to-have for zero-config discovery; security review needed |
| **Multi-tenant authentication (PAM, groups, rate limiting)** | Apiary has single admin user. Multi-user needed for Operational Continuity (alternate admins). | **Adopt incrementally** — Start with local user management + audit; PAM later |
| **Localization (i18n)** | Apiary is English-only. | **Defer** — Low priority for core operators |

### LOW RELEVANCE — Outside Apiary's scope or philosophy

| Sylve Feature | Reason |
|---------------|--------|
| **Linux Jail support** | Apiary is FreeBSD-native; Linux workloads run in VMs |
| **Sylve.app managed certificates/DDNS** | Tied to Sylve's SaaS; Apiary avoids external dependencies |
| **Browser uploads (Downloader/File Explorer)** | Apiary uses ISO/images cluster-wide view; not a file manager |
| **Sysctl management UI** | Apiary manages sysctls via Flight Plan / host config, not ad-hoc UI |
| **Per-share Apple extensions, guest ACLs** | Too specific; Samba itself is deferred |
| **Demo sandbox (demo.sylve.io)** | Apiary validates on real hardware (brood/drone) |

---

## Design Proposals

### 1. Guest Migration (Highest Priority)

**Problem:** Apiary cannot move VMs/Jails between Combs. This blocks maintenance, failure-domain rebalancing, and Flight Plan execution.

**Design:**
- New RPC: `MigrateGuest` (VM or Jail) with `source_comb`, `target_comb`, `guest_id`, `guest_type`
- Phased execution (mirroring Sylve):
  1. **Preflight** — Validate target capacity, network compatibility, storage accessibility, no conflicting ops (backup/replication/lifecycle)
  2. **Bulk transfer** — Recursive ZFS send while guest online (use `zfs send -R -w` for encrypted)
  3. **Final sync** — Stop guest, incremental ZFS send, transfer ownership in Raft
  4. **Cutover** — Recreate guest on target (libvirt define / jail config), start, verify
  5. **Cleanup** — Destroy source datasets after successful verification
- **Durability:** Each phase recorded in Raft log; recoverable after leader failure; cancellation supported during bulk transfer
- **Integration:** Exposed as Flight Plan step `MigrateGuest` with automatic rollback on failure

**ADR dependencies:** ADR-0117 (jail VNET networking), ZFS datastore model, Raft leadership forwarding

### 2. ZFS Replication (High Priority)

**Problem:** No cross-Comb dataset replication for DR or cross-Cell sync.

**Design:**
- Replication policy CRD: `source_dataset`, `target_comb`, `target_dataset`, `schedule`, `retention`, `encryption`
- Raft-managed ownership leases (like Sylve) — only one active replicator per dataset
- Continuous fencing evidence — target must acknowledge each replication
- Protected autostart — replicated guests auto-start on target after source failure (configurable)
- **Early access gate:** Hidden behind `experimental.replication` feature flag; requires 3+ Raft voters
- **Integration:** Replication policy attached to Cell; respects failure-domain boundaries (source/target in different Cells)

**ADR dependencies:** Colony membership, Raft leadership, ZFS send/recv, encryption key handling

### 3. Backup System (High Priority)

**Problem:** No backup/restore. Operational Continuity Scorecard requires recoverability evidence.

**Design:**
- Backup job CRD: `guest_selector` (VM/Jail/all), `target_comb`, `schedule`, `retention`, `encryption`
- Manifest-backed generations — each backup creates a manifest listing all datasets/zvols for atomic restore
- Recursive dataset support — VM with multiple zvols backed up as coordinated set
- Multi-pool VM roots — handle VMs spanning multiple ZFS pools
- Restore modes: in-place (overwrite), out-of-band (new guest), metadata-only (guest config)
- Encrypted source handling — backup encrypts at rest; restore decrypts with key
- **Integration:** Backup job as Flight Plan step; status visible in Operational Continuity Scorecard

**ADR dependencies:** Guest migration (for restore to alternate Comb), ZFS replication, certificate/key management

### 4. PF Firewall Management (High Priority)

**Problem:** No host firewall management. Cannot enforce Cell network isolation or blast-radius boundaries.

**Design:**
- Firewall policy CRD: ordered rule list (pass/block, in/out, proto, src/dst, port, network-object refs)
- Network objects — reusable IP/CIDR/group definitions (referenced by rules)
- NAT/masquerade rules — separate section with ordered evaluation
- Live logging — pflog capture with filtering, integrated into dashboard
- **Integration:** Firewall policy attached to Comb or Cell; reconciled by managerd on each node
- **Safety:** Default-deny for management interfaces; explicit allow for Colony Raft/managerd ports

**ADR dependencies:** VLAN/switch model (ADR-0117), network objects, Colony join (auto-allow peer ports)

### 5. WireGuard VPN (High Priority)

**Problem:** No secure overlay for cross-site Colony, management network redundancy, courier sync.

**Design:**
- WireGuard interface CRD: `private_key`, `listen_port`, `peers[]` (public_key, allowed_ips, endpoint, keepalive)
- Server mode — peer provisioning with QR/export; managed firewall rules for handshake
- Client mode — persistent outbound with FIB/route controls, MTU, reconnection
- **Integration:** WireGuard mesh as Cell interconnect; auto-provision on Colony join (derive from peer TLS hostname map)
- **Courier sync:** Air-gapped operations courier uses WireGuard endpoint for rendezvous

**ADR dependencies:** PF firewall (for managed rules), certificate management (for key distribution), Colony membership

### 6. Certificate Management (Medium Priority)

**Problem:** TLS certs managed manually in config files; no auto-renewal.

**Design:**
- Certificate CRD: `type` (imported, self-signed, letsencrypt, acme), `domains[]`, `key_algorithm`, `renewal_window`
- ACME integration — HTTP-01 (via frontend), DNS-01 (via Dynamic DNS providers), TLS-ALPN-01
- Auto-renewal — background task renews within window; hot-reloads daemons via SIGHUP
- **Integration:** Colony join auto-requests cert for node FQDN; peer TLS hostname map updated on renewal
- **Storage:** Private keys in Raft-replicated secrets (encrypted at rest)

**ADR dependencies:** Dynamic DNS, Colony join flow, Raft secret replication

### 7. Notifications (Medium Priority)

**Problem:** No alerting for Flight Plan status, degradations, Scorecard rehearsals.

**Design:**
- Notification rule CRD: `event_filter` (backup failed, migration started, replication lag, S.M.A.R.T. alert, ZFS pool degraded, Flight Plan completed/failed), `transports[]` (in-app, ntfy, SMTP, Discord), `template`
- Transport plugins — configurable per transport (ntfy topic, SMTP server, Discord webhook)
- In-app notification center — persistent, dismissible, linked to audit log entries
- **Integration:** Audit log emits structured events; notification engine subscribes; Flight Plan emits lifecycle events

**ADR dependencies:** Audit log, Flight Plan engine, Operational Continuity Scorecard

### 8. S.M.A.R.T. Disk Health (Medium Priority)

**Problem:** No disk health visibility. Shared storage dependencies invisible.

**Design:**
- Disk health CRD: `device`, `schedule` (short/long/offline self-tests), `thresholds` (temp, reallocated, wearout)
- Background monitor — periodic `smartctl -a`, parse health attributes, track trends
- Alert rules — trigger notification on threshold breach or self-test failure
- **Integration:** Feed into Failure-Domain Integrity — if disk in shared pool degrades, mark all dependent Cells as at-risk

**ADR dependencies:** ZFS pool monitoring, notification system, dependency discovery

### 9. Local CLI & Interactive Console (Medium Priority)

**Problem:** No local management when frontend/restshimd down; needed for air-gapped courier.

**Design:**
- Unix socket at `/var/run/apiary/cli.sock` (root-only, mode 0600)
- CLI binary `apiaryctl` — commands: `flight-plan`, `cell`, `comb`, `raft`, `vm`, `jail`, `network`, `storage`, `backup`, `replication`, `cert`, `log`
- TUI mode — live status bar (Raft term/leader, Colony health, active Flight Plans, degradations)
- JSON output for scripting; command history; tab completion
- **Air-gapped courier:** `apiaryctl courier prepare|sync|verify` for physical media workflow

**ADR dependencies:** Raft leadership API, Flight Plan engine, Cell/Comb state

---

## Implementation Sequencing

| Phase | Features | Rationale |
|-------|----------|-----------|
| **1** | Guest Migration, PF Firewall | Migration unblocks maintenance/Flight Plans; Firewall enables Cell isolation |
| **2** | ZFS Replication, Backup System | Replication + Backup = DR foundation; both need migration for restore |
| **3** | WireGuard, Certificate Management | VPN + certs = secure cross-site Colony; enables courier rendezvous |
| **4** | Notifications, S.M.A.R.T., Local CLI | Observability & operability; CLI needed for air-gapped ops |

**Estimated effort:** 8-12 ADRs across 4 phases. Each feature requires:
- CRD definition (protobuf + Go types)
- Raft-replicated state machine
- Managerd RPC handlers
- Frontend UI (Go templates)
- Reconciliation loops
- Integration tests on brood/drone
- Documentation in `docs/adr/`

---

## Open Questions for User

1. **Scope confirmation:** Does "implement features in place by sylve.io" mean adopt the *entire* Sylve feature set, or only the high-relevance subset above?
2. **Priority ordering:** Is guest migration the true highest priority, or does backup/replication rank higher for Operational Continuity?
3. **WireGuard vs. Tailscale/other:** Sylve uses WireGuard; should Apiary standardize on WireGuard or support multiple VPN backends?
4. **Certificate authority:** Should Apiary run its own internal CA (like Smallstep) or only integrate with Let's Encrypt/external CAs?
5. **Multi-tenancy:** Is local user management + PAM sufficient, or is RBAC/teams needed for Operational Continuity (alternate admins)?
6. **Replication early-access gate:** Follow Sylve's model (3+ voters, feature flag, "use alongside independent backups")?

---

## Recommendation

**Proceed with phased adoption of HIGH RELEVANCE features** (Migration, Replication, Backup, Firewall, WireGuard, Certificates, Notifications, S.M.A.R.T., CLI) as they directly enable Apiary's differentiating pillars (Failure-Domain Integrity, Blast-Radius Budget, Operational Continuity).

**Defer MEDIUM/LOW relevance features** until core HA/DR loop is closed.

**Close or defer this TODO** if the user intended full Sylve parity — Apiary and Sylve have different architectural centers (operational orchestration vs. hypervisor management UI) and full convergence is neither necessary nor desirable.

---

## References

- [Sylve website](https://sylve.io)
- [Sylve GitHub](https://github.com/AlchemillaHQ/Sylve)
- [Sylve v0.3.0 Changelog](https://github.com/AlchemillaHQ/Sylve/blob/master/docs/changelogs/v0.3.0.md)
- [Sylve Architecture](https://github.com/AlchemillaHQ/Sylve/blob/master/docs/ARCHITECTURE.md)
- SHARED.md — Apiary architecture, pillars, and roadmap