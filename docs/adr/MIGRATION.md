# Apiary ADR format migration

## Scope and conversion rule

Inventory at source revision `261e915`: 148 Markdown ADRs, numbered 0001
through 0149 with 0126 absent. The first batch converts ADR-0001 through
ADR-0007. The remaining 141 records retain their existing structure.

This is a structural conversion, not a new technical decision. Existing
filenames, titles, legacy identifiers, links, decision text, rationale,
references, and historical claims remain unchanged. `## Decisions` becomes
`## Decision`; an explicit Accepted status becomes a header with the same
value. Each converted record identifies Apiary and its legacy identifier.
No approval date or approver is invented. Acceptance is preserved from the
source record and does not establish current implementation.

Supersedes and Superseded by are mandatory headers. None means no explicit
record replacement relationship was found for this initial batch. A later
extension or implementation does not automatically replace an earlier record.
Historical prose, including old typography and capability claims, is retained.
No context file is removed or policy migrated to Notion in this batch.

ADR-0008 has historical status notes and amendments; it is deferred for careful
preservation, along with the rest of the inventory. Later batches must retain
status qualifications and distinguish full from partial supersession. They
must report missing Context, Decision, or Consequences content rather than
invent it. Superseded records remain available.

## Numbering migration map

Every destination below requires Glen's central allocation before a global
identifier is assigned. ADR-0000000 and ADR-0000001 are already reserved for
Loreloom coordination policy and Worker exit-code registry. Apiary ADR-0001
is a legacy project identifier, not ADR-0000001. Do not pad existing numbers
or assume any offset. Preserve each old filename as a provenance link when
allocated numbers are introduced, and update reciprocal supersession links
without erasing their historical scope.

The status column is an inventory label from the explicit source status,
not an acceptance or supersession decision. Qualifications remain authoritative
in the linked record. Unresolved means the source needs review before mapping
it to Unconfirmed, Proposed, Accepted, or Superseded. Allocation is outstanding
for all 148 records, including this structurally converted batch.

| Legacy record | Source status label | Loreloom destination | Structure |
| --- | --- | --- | --- |
| [ADR-0001](0001-raftd-process-split-and-uds-protocol.md) | Accepted | Pending Glen allocation | Converted |
| [ADR-0002](0002-managerd-external-rpc-and-api-rpc-schema.md) | Accepted | Pending Glen allocation | Converted |
| [ADR-0003](0003-raftd-multi-node-clustering.md) | Accepted | Pending Glen allocation | Converted |
| [ADR-0004](0004-ephemeral-state-schema.md) | Accepted | Pending Glen allocation | Converted |
| [ADR-0005](0005-external-vm-crud-rpcs.md) | Accepted | Pending Glen allocation | Converted |
| [ADR-0006](0006-zfs-dataset-lifecycle.md) | Accepted | Pending Glen allocation | Converted |
| [ADR-0007](0007-jail-lifecycle.md) | Accepted | Pending Glen allocation | Converted |
| [ADR-0008](0008-hast-config-and-lifecycle.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0009](0009-vm-read-path.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0010](0010-bhyve-vm-lifecycle.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0011](0011-restshim-rest-translation.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0012](0012-cluster-reconciler.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0013](0013-managerd-reconciler-wiring.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0014](0014-web-ui.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0015](0015-reconciler-bhyve-wiring.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0016](0016-vm-deletion-and-reconciliation-phase.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0017](0017-iso-upload-and-hash-verification.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0018](0018-host-stats-and-multipage-ui.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0019](0019-session-based-login.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0020](0020-novnc-console.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0021](0021-iso9660-sniffing-for-memstick-images.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0022](0022-network-management.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0023](0023-api-key-authentication.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0024](0024-restshimd-binary.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0025](0025-resource-reclaim.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0026](0026-hast-vm-disk-replication.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0027](0027-jail-orchestration.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0028](0028-migrate-vm-and-jail.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0029](0029-cross-node-write-forwarding.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0030](0030-tiered-rbac-pam-login.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0031](0031-vm-base-images.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0032](0032-bhyve-serial-console-log.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0033](0033-internal-transport-security.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0034](0034-remote-serial-log-viewing.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0035](0035-leader-only-read-forwarding.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0036](0036-cluster-overview-and-per-node-host-page.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0037](0037-write-rpc-forwarding-to-leader.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0038](0038-tiered-reset-cli.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0039](0039-per-role-password-change.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0040](0040-iso-copy-on-demand.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0041](0041-image-fetching-at-vm-creation-time.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0042](0042-serial-console-echo-loop-fix.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0043](0043-vmexists-checks-real-process-not-just-vmm-context.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0044](0044-deterministic-mac-for-every-vm.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0045](0045-kubernetes-ready-base-image-and-first-real-kubeadm-init.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0046](0046-vm-table-polling-corruption-from-oob-swap.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0047](0047-external-gateway-networks.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0048](0048-self-hosted-outbound-nat.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0049](0049-machine-configuration-page.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0050](0050-dnsmasq-tag-not-interface-scoping.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0051](0051-raftd-config-save-restore.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0052](0052-dependency-graph-simulator.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0053](0053-managed-network-failure-simulation.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0054](0054-image-availability-in-node-failure-simulation.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0055](0055-automated-assumption-checks-v1.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0056](0056-evidence-aware-health-v1.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0057](0057-offline-recovery-handbook-v1.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0058](0058-cell-path-trace-v1.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0059](0059-hierarchical-sidebar-shell.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0060](0060-operational-invariants-v1.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0061](0061-why-not-engine-v1.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0062](0062-resilience-coverage-map-v1.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0063](0063-cloudflare-tunnel-exposure-v1.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0064](0064-disabled-jail-lifecycle.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0065](0065-cross-hive-console-tunnel.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0066](0066-host-default-egress-contract.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0067](0067-config-injection-hardening.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0068](0068-jexec-jail-console.md) | Unresolved | Pending Glen allocation | Remaining |
| [ADR-0069](0069-host-config-export.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0070](0070-system-settings-expansion.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0071](0071-network-correction-workflow.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0072](0072-cloudflare-origin-ca-certificate-lifecycle.md) | Unresolved | Pending Glen allocation | Remaining |
| [ADR-0073](0073-orphaned-hast-resource-cleanup.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0074](0074-role-map-editing-ui.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0075](0075-firewall-rule-priority.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0076](0076-comb-hierarchy-rename.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0077](0077-origin-ca-expiry-health-and-renewal.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0078](0078-raft-transport-tls.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0079](0079-vm-firewall-rule-editing.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0080](0080-network-name-editing.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0081](0081-guided-network-replacement-workflow.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0082](0082-apiary-installer-preflight.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0083](0083-mutually-authorized-colony-join.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0084](0084-jail-base-images.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0085](0085-uplink-admin-toggle.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0086](0086-first-login-bootstrap-admin.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0087](0087-pam-in-managerd.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0088](0088-pause-nat-on-uplink-down.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0089](0089-jail-template-peer-fetch.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0090](0090-vm-snapshot-restore.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0091](0091-single-node-first-class.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0092](0092-join-colony-target-address.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0093](0093-peer-tls-ca-trust.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0094](0094-boot-time-network-robustness.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0095](0095-create-vm-from-snapshot.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0096](0096-security-audit-follow-up.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0097](0097-join-flow-hardening.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0098](0098-jail-base-archives.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0099](0099-jail-running-check-before-root-ensure.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0100](0100-daemon-config-files.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0101](0101-uplink-bridged-networks.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0102](0102-daemon-config-live-editing.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0103](0103-action-preflight-guardrails.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0104](0104-local-service-endpoint-configuration.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0105](0105-standalone-to-colony-conversion.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0106](0106-unique-resource-identity.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0107](0107-clear-stale-assumption-results.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0108](0108-fixed-listener-ports.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0109](0109-surface-forward-errors.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0110](0110-cluster-wide-images-page.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0111](0111-frontend-restart-routing.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0112](0112-common-config.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0113](0113-tls-trust-prompt-on-join.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0114](0114-remove-uplink-takedown.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0115](0115-automatic-peer-tls-hostname-map.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0116](0116-quorum-safe-raftd-restart.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0117](0117-dedicated-jail-networking.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0118](0118-why-not-quorum-blocker-detail.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0119](0119-replica-freshness-and-maintenance-wave-planner.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0120](0120-config-rationale-history.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0121](0121-dependency-graph-hast-sync-evidence.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0122](0122-cluster-evidence-aware-health-api.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0123](0123-colony-leader-indicator.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0124](0124-raftd-offline-status.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0125](0125-quorum-safe-raftd-restart.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0127](0127-sylve-io-features.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0128](0128-guest-migration.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0129](0129-pf-firewall-management.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0130](0130-zfs-replication.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0131](0131-backup-system.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0132](0132-wireguard-management.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0133](0133-certificate-management.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0134](0134-notifications.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0135](0135-smart-disk-health.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0136](0136-local-cli.md) | Proposed | Pending Glen allocation | Remaining |
| [ADR-0137](0137-pf-rule-scope-and-drift.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0138](0138-bridge-svi-vlan-uplink.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0139](0139-managerd-rpc-addr-is-a-dial-target-too.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0140](0140-pf-enabled-is-not-filtering.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0141](0141-make-update-and-force-restart-split.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0142](0142-refuse-managerd-self-restart.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0143](0143-voter-state-digest-badge.md) | Unresolved | Pending Glen allocation | Remaining |
| [ADR-0144](0144-web-ui-redesign.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0145](0145-controlled-colony-update.md) | Unresolved | Pending Glen allocation | Remaining |
| [ADR-0146](0146-controlled-update-managerd-handoff.md) | Accepted | Pending Glen allocation | Remaining |
| [ADR-0147](0147-automated-install-and-two-way-peer-authorized-join.md) | Unresolved | Pending Glen allocation | Remaining |
| [ADR-0148](0148-colony-disk-size-floor.md) | Unresolved | Pending Glen allocation | Remaining |
| [ADR-0149](0149-durable-colony-work-queue.md) | Unresolved | Pending Glen allocation | Remaining |
