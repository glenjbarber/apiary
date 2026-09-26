# ADR-0131: Manifest-backed backup and restore

**Date:** 2026-09-26
**Status:** Proposed
**Author:** Goose (subagent)

## Context

ADR-0127 catalogued Sylve's feature set and ranked **Backup System** as
Phase 2, item 2, alongside ZFS replication (ADR-0130). Sylve describes
its own backup system as "manifest-backed generations" with recursive
dataset support, multi-pool VM roots, three restore modes, and
encrypted-source handling. Apiary has none of it.

The concrete gap is the Operational Continuity pillar. A Comb can be
lost, and today a workload on it is rebuilt from nothing. `zfs send`
(ADR-0089) and HAST (ADR-0026, ADR-0028) both exist but neither is a
backup: HAST is a live block mirror between exactly two nodes and dies
with either one; a `zfs send` stream is only as good as the moment it
ran and leaves nothing behind.

### Raft state backup is already solved and is out of scope

`raftd` has had offline operational formats for a while:
`raftd -export` and `raftd -restore`, implemented in `cmd/raftd/main.go`
against the `ConfigArchive` envelope in `api/internalpb/state.proto`
(ADR-0051), with the read-only offline inspection surface documented in
**ADR-0124** (`raftd -status`). That path already wraps an unmodified
`FSMSnapshotState` payload in a versioned, SHA-256-checksummed envelope
with provenance fields.

This ADR does **not** redesign, replace, extend, or re-verify that. No
Cell restore path in this design may write raft state, and the backup
system does not second-guess the `ConfigArchive` checksum. Where a
backup needs to reason about FSM state it reads it through the existing
`FSM.Persist` snapshot bytes or through the normal read RPCs
(`GetVM`, `GetJail`), never by touching raft's log or snapshot files.

### What ADR-0127 assumed that the repository does not contain

Verified by grep against commit `5dad79f` before drafting. These are
corrections to ADR-0127's section 3, not disagreements with its
prioritisation:

- **There is no Flight Plan engine.** `grep -rl "FlightPlan"` over
  `*.go`, `*.proto` and `web/templates` returns nothing. ADR-0127 lists
  "Backup job as Flight Plan step" as an integration point. There is
  nothing to integrate with; v1 schedules backups on the leader's
  reconcile loop, and the Flight Plan integration is deferred to
  whatever ADR eventually introduces one.
- **There is no Operational Continuity Scorecard.** `grep -rl
  "OperationalContinuity\|Scorecard"` returns nothing. The closest
  existing surfaces are `web/templates/cluster_evidence.html` (ADR-0122)
  and `web/templates/recovery_handbook.html`. v1 reports backup evidence
  there instead of inventing a scorecard.
- **There is no `BackupJob` type, CRD, or protobuf message** anywhere in
  the repository. The raft FSM in `api/internalpb/state.proto` holds
  `VMDefinition`, `JailDefinition`, `NetworkDefinition`, `ApiKey`,
  `PendingJoinRequest`, and the restart-lease state added by ADR-0103.
  Everything below is new.
- **`target_comb` is the wrong field name and the wrong concept.** The
  code's placement identity is `node_id` on `VMDefinition` and
  `JailDefinition` (`api/internalpb/state.proto` fields 5). A backup
  *destination* Comb is a storage location, not a placement decision.
  Conflating them would let a backup job try to move a Cell. The design
  below separates them: placement is raft-owned, destination is a
  target.
- **"Multi-pool VM roots" is not representable today.** `bhyve.Config`
  (`internal/bhyve/manager.go`) has exactly one `DiskPath`, one
  `ISOPath`, and one `InstallDiskPath`. A VM cannot currently span two
  pools, and `zfs.Manager.Send(ctx, snapshot)`
  (`internal/zfs/manager.go`) takes a single dataset name with no
  recursive flag. Recursive, multi-pool, coordinated sets are a v2
  concern and the manifest is designed to carry them without a
  breaking change, but v1 will not pretend to produce them.
- **"Cell" means a workload, not a failure-domain grouping.** ADR-0130
  established this from the code (`TraceCellPathRequest.cell_id`
  resolves "as either a VM or a jail"; `internal/coverage` says "this
  comb owns N cell(s)"). ADR-0127's glossary table is wrong on this
  point and this ADR follows the code, as ADR-0130 does.
- **One pre-existing thing already called "backup" is not one.**
  `ConvertStandaloneToJoiner` moves a Comb's existing raft data aside
  to `BackupDataDir` (`web/templates/machine.html`, and the RPC in
  `api/rpc/manager.proto`). That is a local directory rename during a
  destructive reconfiguration. It does not survive the host, it is not
  checksummed, and it is not covered by this ADR. Nothing in the backup
  system may read it or count it as evidence.
- **A Comb's own node configuration is not in the raft snapshot, and
  it holds four live secrets.** `FSMSnapshotState`
  (`api/internalpb/state.proto`) holds `vms`, `networks`, `api_keys`,
  `jails`, `pending_join_requests`, the ADR-0103 restart state, and
  `auth_enabled` — and nothing about the node itself.
  `internal/nodeconfig`, `internal/raftdconfig` and
  `internal/frontendconfig` are local JSON files outside it. So
  `raftd -restore` reconstructs the *Colony* and not the *Comb*, and a
  Comb rebuilt after a disaster comes up with no `raftd_token`, no
  `manager_api_key`, no `internal_token`, and no `peer_api_key` unless
  the operator re-provisions them out of band. That gap is exactly
  where a backup has to reach — and exactly where it must not copy
  secrets verbatim. This ADR therefore treats node configuration as a
  **`Config` artifact with a mandatory strip**, not as a file copy.

## What already exists

Surveyed by reading the packages, not by assumption.

| Package | What it actually provides | How backup composes with it |
|---|---|---|
| `cmd/raftd` + `internalpb.ConfigArchive` | `raftd -export` / `-restore`, versioned SHA-256-checksummed envelope around `FSMSnapshotState` (ADR-0051, ADR-0124) | **Out of scope.** Backed up separately, by the operator, on their own schedule. Backup never writes raft state. |
| `internal/jailarchive` | `Extractor.Extract(ctx, archivePath, destDir)` — unpacks a jail archive into a destination directory. One file, one function, no manifest. | This is the **restore half of a jail backup, already written**. Reuse it as the out-of-band materialiser. Do not build a second archive extractor. It is jail-only and says nothing about VMs. |
| `internal/isostore` | `Manager.Save(name, r, expectedSHA256)` with a hash sidecar, `ParseManifest(r) map[string]string`, `IsISO9660(name)` sniffing a real file, `List`, `Delete`, `Path` | The checksum-and-sidecar idiom already exists and is already content-verified at write time. Reuse the pattern and the `sha256Equal` comparison style; do not invent a new integrity format. |
| `internal/freebsdimg` | `Manager.EnsureImage(name)`, `downloadAndVerify(img, dest)` against `OfficialImage`'s expected SHA-256, `verifyFile`, `decompressXZ`, `sha256Equal` | Base images are **re-fetchable, not backup-worthy**. Record their name and expected digest in the manifest; do not copy the payload. Backing up a 1 GB FreeBSD image that can be re-downloaded and hash-verified wastes the target. |
| `internal/origincert` | `NewCSR`, `WritePair(dir, name, certPEM, keyPEM)`, `SaveInventory` / `LoadInventory`, `ExpiryStatus` | The **key PEM is a live secret** on the same disk as the cert. Back up the inventory (public, non-secret) and record the key's absence. Never copy `keyPEM`. |
| `internal/raftdconfig` | `Manager.Load` / `Save` of `raftd.conf` incl. **`InternalToken`**, documented in-code as "must be root-owned, mode 0600 (Load only warns, see `warnIfWorldReadable`; it does not refuse to start)", plus `RaftTLSCert` / `RaftTLSKey` / `RaftTLSCA` (paths, ADR-0078); `atomicWriteFile`; `Defaults` also backs `cmd/raftd`'s `-reset` / `-export` / `-restore` / `-restore-dry-run` recovery flags | `InternalToken` is a **live secret stored inline in JSON** and must be stripped. `RaftTLSKey` is a path — the path is backable, the key file is not. This is the second of four config files with inline secrets; see §2. |
| `internal/frontendconfig` | `Config.ManagerAPIKey` (`manager_api_key`) in JSON, plus `warnIfWorldReadable(path)`, which prints "readable by group/other but contains manager_api_key - recommend chmod 600" | A **third** inline secret. `warnIfWorldReadable` is the existing precedent for how this project treats a secret-bearing file — warn, don't refuse. Reuse that posture; the backup system must strip the key. |
| `internal/nodeconfig` | `Config` JSON with **two** inline secrets — `PeerAPIKey` (in-code: "a live credential", ADR-0029) and `RaftdToken` — and four *paths* to secrets elsewhere: `TLSCert` / `TLSKey` (in-code: "file paths, not secrets themselves"), `CloudflareTokenFile`, `OriginCATokenFile` | This is the central finding for secret handling. Naively "back up the config files" would write two live credentials into a backup archive. Both must be stripped and their absence recorded; the four paths are recorded as paths. |
| `internal/restshimdconfig` | `Manager` over the restshimd config, same shape as the above | Same treatment; verified secret-free of its own inline token at ADR-0131's writing, but re-checked whenever the file is next edited. |
| `internal/zfs` | `CreateDataset`, `DestroySnapshot`, `CreateSnapshot`, `Send`, `Receive`, `Clone`, `RollbackSnapshot`, `ListSnapshots`, `GetProperty` / `SetProperty` | The snapshot/send/receive primitives a backup is built from. `Send` streams **live** (ADR-0089), so a backup is not blocked by pool contention, but the manifest cannot be finalised until every stream completes. |
| `internal/bhyve` | `CreateVM`, `DestroyVM`, `VMExists`, `ListVMs`, `Config` (one `DiskPath`, one `ISOPath`, one `InstallDiskPath`), `ValidateConfig`, pidfile/tap/VNC/nmdm helpers | Supplies the *configuration* half of a VM artifact. Does not supply the disk — the disk is a ZFS dataset named after the VM, per ADR-0090. |
| `internal/jail` | `CreateJail`, `RemoveJail`, `JailExists`, `JailInfo`, `ListJails`, `Config` (`Path`, `Hostname`, `VNET`, `VNETInterface`, `IPAddress`, `IPPrefixLen`, `Gateway`) | Supplies the configuration half of a jail artifact. `Config.Path` is the dataset root, which is what gets `zfs send`-ed. |
| `internal/manager` (`Server`) | Every subsystem is a `func (s *Server) Xxx(ctx, req) (*rpcpb.XxxResponse, error)` on one `ManagerServiceServer`, with `if s.<dep> == nil { return ...Error: "this node has no X support configured" }` as the standard dependency guard (e.g. `CreateVMSnapshot`, `internal/manager/server.go:3085`) | Backup follows the same shape. This ADR's implementation notes name the handlers in the same style. |

`RestoreVMSnapshot` (`internal/manager/server.go:3120`) is the closest
existing precedent for a restore guard rail, and its own comment is
instructive: it refuses while the VM is desired `RUNNING` because
"ZFS has no notion of coordinating with a process that already has the
file open", and it discloses that the check is best-effort when no raft
client is configured. The backup design reuses that reasoning and
upgrades the disclosure: an unverifiable guard is `unknown`, and
`unknown` blocks, it does not wave through.

## Decision

### 1. A backup is a versioned manifest plus a set of artifacts

A backup generation is a directory (or object prefix) containing one
**manifest** and N **artifacts**. The manifest is the only thing
Apiary reads to know what a backup is; artifacts are opaque payloads
addressed by relative path.

```
<target>/backups/<policy_id>/<manifest_id>/
    manifest.pb            # proto-encoded BackupManifest
    artifacts/
        <artifact_id>.zfs  # zfs send stream
        <artifact_id>.bin  # config proto, image, other payload
```

Rules, in priority order:

1. **The manifest is written last, atomically, and only on full
   success.** A partially-populated directory with no `manifest.pb` is
   not a backup. It is garbage, and the retention sweeper deletes it.
2. **All-or-nothing.** If any artifact of a generation fails to
   capture, the entire generation is discarded. There is no
   best-effort backup. A backup that silently contains 9 of 10
   datasets is worse than no backup, because it will be *believed*.
3. **Unknown fields are preserved and ignored.** Protobuf's own
   forward compatibility is the versioning story; see §1.1.
4. `manifest.pb` is itself checksummed and self-describing. A manifest
   whose checksum fails to verify is rejected outright and reported as
   `unknown`, never skipped.

#### 1.1 Manifest versioning

`BackupManifest.format_version` starts at 1 and is this file's own
schema version, independent of `FSMSnapshotState`'s and independent of
`ConfigArchive`'s — the same discipline `ConfigArchive` already uses
(ADR-0051, `api/internalpb/state.proto`). A reader encountering a
`format_version` it does not know:

- **reads** every field it recognises, and records the unread version
  verbatim in the read result;
- **refuses to restore** if the unknown version changed any field
  marked `restore_critical` in the schema comment;
- **never** guesses. An unknown future version is `unknown`, not
  "probably compatible".

An unknown newer backup on a target is not a health failure and not a
success; it is `unknown`, and the UI says so (ADR-0056, ADR-0118,
ADR-0122).

#### 1.2 What a manifest lists

Every artifact is one of four kinds. A `Config` artifact is metadata
only and carries no secrets; a `Dataset` artifact is a `zfs send`
stream; an `Image` artifact is a content-addressed blob; a
`Reference` artifact is a pointer to something re-fetchable and
verified by digest, with no payload at all.

- **`Config` artifacts** — one marshalled `VMDefinition` or
  `JailDefinition` (from `api/internalpb/state.proto`), plus the
  `bhyve.Config` / `jail.Config` the node actually rendered. Recorded
  so a restore can state *what configuration the data was captured
  under*, and so a schema drift between data and config is visible.
  **Config artifacts are never applied to a live FSM in v1** (see
  §6).
- **`Dataset` artifacts** — a dataset name, the snapshot the stream
  was taken from, the ZFS GUID of the source dataset, byte count, and
  SHA-256 of the stream. A VM's `disk.img` dataset and a jail's root
  dataset are the two v1 cases.
- **`Image` artifacts** — `internal/isostore` entries: name, the
  sidecar's expected digest, observed digest, size. Restoring an image
  means re-uploading or re-fetching it and re-verifying against the
  recorded digest, which is what `isostore.Manager.Save` already does
  with `expectedSHA256`.
- **`Reference` artifacts** — `internal/freebsdimg` base images and
  `internal/zfs` jail templates: name plus expected SHA-256, no
  payload. `freebsdimg.Manager.EnsureImage` re-verifies against the
  expected digest on every call, so a reference artifact is verifiable
  without a copy.

Recursive, multi-pool, cross-dataset coordinated sets are expressible
in this schema (`parent_dataset`, `snapshot_set_id`) and are **not
produced in v1**, because `bhyve.Config` gives a VM one disk and
`zfs.Manager.Send` takes one dataset.

### 2. Secrets are excluded, and their absence is recorded

**A secret never lands in a backup in plaintext. This is not a
default; it is a rule with no v1 exception and no "encrypted at rest"
opt-out.** The reason is not the encryption story, it is the evidence
story: an archive that may contain a credential cannot be safely
copied to a second host, and every additional copy multiplies the blast
radius of the single thing you cannot rotate retroactively.

The v1 exclusion list, each item grounded in a real path found in the
code:

| Secret | Where it actually lives today | v1 handling |
|---|---|---|
| Peer API key | `nodeconfig.Config.PeerAPIKey` (`peer_api_key`) — in-code: "a live credential" (ADR-0029) | **Stripped.** Redacted in the `NodeConfig` artifact; absence recorded. |
| managerd raft token | `nodeconfig.Config.RaftdToken` (`raftd_token`) | **Stripped.** Same treatment. |
| managerd API key | `frontendconfig.Config.ManagerAPIKey` (`manager_api_key`) — the field `warnIfWorldReadable` exists to warn about | **Stripped.** Same treatment. |
| raftd internal token | `raftdconfig.Config.InternalToken` (`internal_token`) — in-code: "must be root-owned, mode 0600" | **Stripped.** Same treatment. |
| TLS / Cloudflare / Origin-CA private material | The *files* `nodeconfig.TLSKey`, `nodeconfig.CloudflareTokenFile`, `nodeconfig.OriginCATokenFile`, and `raftdconfig.RaftTLSKey` **point at**. Every one of these fields is in-code documented as a path, not a secret | **Never copied.** The path is recorded; the content is not. Restore re-provisions via the existing cert and token flows. |
| Origin-certificate private key | `origincert.WritePair(dir, name, certPEM, keyPEM)` writes cert and key side by side | **Never copied.** The public cert and the `InventoryEntry` are recorded; the key is not. |
| API key material | `ApiKey.hashed_key` in `FSMSnapshotState` | **Not duplicated by backup.** It is already covered by the raft export path (ADR-0051), it is one-way hashed, and copying it would be copying a credential-shaped value for no gain. The manifest records only that N keys existed. |
| Guest-internal secrets | Not modelled. Neither `bhyve.Config` nor `jail.Config` carries cloud-init user-data, passwords, or SSH material | Nothing to exclude. **This will change** — when guest credential handling lands, this ADR's rule applies to it unchanged, and the manifest gains a `SecretGap` entry before the first backup runs. |
| `zfs send` stream of a dataset that happens to contain a database | Guest data, not Apiary's to classify | **Unencrypted in v1.** See Rejected alternative 5: this is a real, stated exposure, and it is why §6 requires the operator to own where archives live. |

**Absence is recorded, not implied.** Every manifest carries a
`SecretGap` list: kind, why excluded, where the live copy lives, and
what a restore operator must do to recover it. A restore that
materialises a Cell whose `SecretGap` list is non-empty prints what
must be re-issued. Silence about a missing secret is how a restored
Cell comes up mysteriously unauthenticated.

The strip is enforced in code at the single point where a node config
enters a manifest, and the write is tested: a test that builds a full
manifest from fixture `nodeconfig`, `raftdconfig` and
`frontendconfig` values carrying known token material, greps the
encoded bytes for each fixture string, and fails if any is present. It
also asserts the resulting `SecretGap` list names all four. That test
is not optional and not skippable-by-env-var.

### 3. Consistency: snapshot-consistent, and quiesced when asked

**What "a consistent point in time" means here, precisely.** A ZFS
snapshot is atomic for one dataset. A *set* of datasets snapshotted
one after another is not atomic across the set. And a snapshot of a
dataset that a running bhyve process has open is filesystem-consistent
but not application-consistent: the guest's page cache and its
transaction log are not part of the snapshot, so a database in that
guest may be mid-fsync.

Two declared modes, and only two:

- **`CONSISTENCY_SNAPSHOT`** (default, zero downtime). Every artifact's
  snapshot is taken, then streamed. Honest label: *"filesystem
  consistent, not application consistent."* Nothing in the UI or the
  manifest may imply otherwise. For a jail whose services are
  individually snapshot-safe, and for bulk data, this is the right
  trade and the operator chooses it explicitly per policy.
- **`CONSISTENCY_QUIESCED`** (requires downtime). The Cell is stopped,
  the stop is **observed** through `GetVM` / `GetJail` — not assumed
  from an RPC that returned no error — then snapshots are taken, then
  the Cell is started again if the policy says `restart_after`.

The failure mode this section exists to prevent is a **silent
downgrade**: a policy that says `QUIESCED` and a Cell that cannot be
stopped (a migration in flight, no quorum to change `desired_state`,
`GetVM` unreachable). The job **fails with `unknown`**. It does not
fall back to `SNAPSHOT`. A backup labelled `QUIESCED` that was
actually `SNAPSHOT` is a lie the operator cannot detect, and it is
worse than a missing backup.

Two further rules:

- **The manifest is not written until every send completes.** `Send`
  streams live (ADR-0089), so a stream can fail hours in. A generation
  with a partial stream set is discarded, not published.
- **Inter-artifact skew is recorded, not hidden.** The manifest carries
  `earliest_snapshot_unix` and `latest_snapshot_unix` for the
  generation. A `QUIESCED` backup of a multi-artifact Cell whose
  snapshots span more than the policy's `max_skew` fails; a
  `SNAPSHOT` backup that spans a wide window is fine, because it never
  claimed otherwise.

### 4. Verification: an unverified backup is an assumption, not a backup

Three tiers, and the UI must show which tier a given generation has
actually reached.

1. **Write-time integrity.** SHA-256 per artifact, computed as it is
   written. Same idiom as `isostore.Save`'s `expectedSHA256` sidecar
   and `freebsdimg.verifyFile`. This proves the bytes that landed are
   the bytes that were produced. It proves nothing about restorability.
2. **Read-back verification.** `VerifyBackup` re-reads the manifest,
   re-checks its own checksum, re-stats every artifact, and re-hashes a
   sampled subset in full. Runs on a schedule, and on demand. A target
   that cannot be reached yields `verify_state: unknown` — never
   `verified`, never `failed`. The distinction matters: unreachable is
   not corruption, and conflating them trains operators to ignore the
   alarm.
3. **Restore drill.** The only tier that establishes restorability.
   Restores the generation into an out-of-band Cell on a scratch
   dataset, boots it, and records the result. Until a drill has run for
   a generation, `last_verified_restore` for it is `unknown`, and
   `last_verified_restore` for the Colony as a whole is the timestamp
   of the newest *drilled* generation — not the newest generated one.

**Cadence.** Tier 1 always. Tier 2 every backup plus a daily sweep.
Tier 3 is sampled: by default the newest generation and the oldest
retained generation, weekly, so a slow bit-rot in the oldest archive
is caught. A full drill of every retained generation is not proposed.

**Ownership.** The leader owns scheduling and the policy FSM. The
*Comb* that owns a Cell owns capturing that Cell's artifacts. The
*drill* runs on whichever Comb the ADR-0119 wave planner selects,
because a drill is a workload creation and must respect the same
link and quorum discipline as any other maintenance activity. The
Colony records drill results in raft, so the answer to "when was this
last proven restorable" is the same on every Comb and survives a
leader change.

**v1 ships tiers 1 and 2 and reports tier 3 as `unknown`.** That is the
honest position. A system that reports `last_verified_restore: <now>`
because it hashed a file is lying, and this ADR declines to ship the
lie in a first version. The drill engine lands in v2; the *reporting*
lands in v1 so the gap is visible from day one.

### 5. Scheduling, retention, and the ADR-0119 interlock

A `BackupPolicy` is raft-replicated. It carries: selector (which Cells,
by `id` or node), destination, schedule, retention, consistency mode,
and throttle. The **leader** computes the due set; the **owning Comb**
executes it; the leader records the outcome.

- **One backup at a time, Colony-wide.** A single global slot, held by a
  raft-granted lease with a monotonic generation, exactly the shape
  ADR-0130 uses for replication authority. Two concurrent backups
  would compete for the same link and the same target directory.
- **Yield to replication.** If an ADR-0130 replication stream is
  advancing its generation on the same Comb, the backup pauses its
  send and resumes. Replication is the mechanism that keeps a *running*
  workload safe; a backup is a slower-moving good, and it yields.
- **Yield to the wave planner.** ADR-0119's Maintenance Wave Planner
  sequences quorum-preserving maintenance one Comb per wave. A backup
  is a maintenance activity: it does not start while a wave is in
  progress, and the planner does not schedule a wave that would remove
  a Comb backing an in-flight backup. Neither schedules into the
  other's slot.
- **Throttle.** A backup send is bandwidth-capped well below
  replication's reserved floor. A backup that saturates a link degrades
  live workloads to protect a copy of them, which is backwards.
- **Quorum required to *start*.** Starting a backup is a state change
  (it takes a lease and writes a record). Starting without quorum is
  refused, and refusal is `unknown`, not a silent skip.

**Retention.** The policy declares a count and/or an age. The sweeper
deletes the *oldest* generations first, never the only verified one, and
never a generation that is the target of an in-progress drill. Deletion
appends a `BackupPruned` record to raft; the UI shows *what was
pruned*, because silent disappearance of an archive is how operators
discover they have no backup only when they need one.

### 6. Targets, and the boundary against ADR-0130

**Replication gives you a current copy. Backup gives you a point-in-time
one. Neither is the other, and neither substitutes for the other.**

| | ADR-0130 replication | This ADR's backup |
|---|---|---|
| Point in time | Now, continuously | One instant, on a schedule |
| Survives loss of the replica | No — losing both ends loses the data | Yes, if the target is a third place |
| Survives corruption | No — corruption propagates | Yes, generations are independent |
| Needs quorum to keep running | No | No (once written) |
| Verifiable offline | Only if you can read the peer | Yes, by design |
| RPO | Seconds | One schedule interval |

**v1 targets: local disk only.** One directory on the same Comb, sized
and permissioned by the operator. This is the honest v1 because it is
the only target that needs no new transport, no new credential, and no
new trust boundary — and a backup on the same host as its source is
still enormously better than nothing, because it survives a dataset
destroy, a bad `zfs rollback`, and an operator mistake, which are the
common losses.

**v2: a second Comb**, over the existing managerd mTLS channel — the
same transport `PushJailTemplateTo` / `ReceiveJailTemplate` already
uses for streaming. The only new problem is authority: two Combs must
not write the same generation directory, so the target is leased the
same way the source is.

**v3: off-host** (S3 or an NFS export). This is where the secret
exclusion rule starts to pay for itself, and where encryption at rest
becomes worth arguing about. It is out of v1 because it introduces a
credential for the backup target, which is a credential that must then
be excluded from the backup of the thing it protects.

### 7. Restore semantics and blast radius

**Restore does not touch raft state.** Full stop. Restoring a Cell's
*data* is a filesystem operation. Restoring the *Colony's* control
plane is `raftd -restore` (ADR-0051, ADR-0124), a separate, operator-
initiated, offline action that this design neither performs nor
permits any backup RPC to perform.

**Default is out-of-band.** A restore materialises a *new* Cell at
`<original-id>-restore-<manifest_id>`, in a new dataset, not attached
to the FSM. The operator inspects it, then adopts it. Nothing live is
touched. This is the only mode v1 offers to a single RPC, and it is
the mode the UI's primary button uses.

**In-place restore** requires all of:

1. The Cell is **observed** stopped. `GetVM` / `GetJail` reported not
   running. If the lookup fails, has no raft client, or returns
   nothing, the answer is `unknown` and the restore is **refused** —
   the opposite of `RestoreVMSnapshot`'s best-effort
   skip-the-check-and-hope, for the reason stated in §2: a torn
   dataset is worse than no backup, and a torn *restore* is worse
   than a torn dataset.
2. The current dataset's ZFS GUID equals the manifest's
   `source_dataset_guid`. A mismatch means the dataset was destroyed
   and recreated after the backup; clobbering it would destroy
   post-backup state that the operator may not know exists.
3. An explicit `clobber` flag, set by a human, on a dialog that states
   the blast radius in words before the button is enabled.
4. Quorum. In-place restore writes a `RestoreRecord` to the FSM
   (`phase`, `desired_state`, and an append-only audit entry). That is
   a raft write and it requires quorum. **Out-of-band restore does
   not require quorum**, because it writes nothing; it needs only that
   the Comb is the one that owns the source dataset, and if it cannot
   confirm that, the result is `unknown`.

**Blast radius, stated plainly.** One restore touches **one Cell's
datasets and one Cell's FSM record, and nothing else.** It cannot
touch another Cell, a network, a firewall rule, an API key, or raft
state. The worst outcomes are: (a) a Cell's data is clobbered back to
an older instant and the operator does not notice until a user does;
(b) a restored Cell comes up with a config artifact that no longer
matches the data's actual schema, and boots into a broken state —
mitigated by refusing to *apply* config artifacts in v1 and only ever
showing them; (c) a restore against a stale manifest silently reverts
minutes of legitimate writes, which is why the GUID check exists.

**Restore never prunes and never repairs.** A failed restore leaves
its scratch dataset in place for inspection; deleting the evidence of
a failure to make the failure go away is not a recovery strategy.

### 8. Evidence: what the surfaces report

The project rule is that **missing evidence is `unknown`** — never
`false`, never healthy, never blank. ADR-0056 established it for
health, ADR-0118 for quorum blockers, ADR-0122 for the cluster-wide
surface. This design extends it to backups and does not relax it
anywhere.

- **`BackupStatus` in raft**, one per policy per generation:
  `state` (`pending` / `running` / `succeeded` / `failed` /
  `unknown`), `last_attempt_at`, `last_succeeded_at`,
  `manifest_id`, `artifact_count`, `bytes`, `verify_state`
  (`verified` / `unverified` / `unknown`), and
  `last_verified_restore_at`. Every one of these is a pointer that
  may be **absent**; absence reads as `unknown` in the UI, always.
- **Age, not a verdict.** A 30-day-old successful backup reports
  `last_succeeded_at` and lets the operator judge. Apiary does not
  compute a "backup is stale" boolean against a policy threshold and
  does not let that boolean become a cluster health `false`. Age is
  displayed; a policy's own retention window is displayed; the
  judgement is the operator's.
- **Cluster health (ADR-0122)** — the existing `ClusterHealth` RPC
  (`api/rpc/manager.proto`) gains a backup dimension: the age of the
  newest successful generation per failure domain, and whether any
  drill has ever run. A Comb that has never reported a backup result
  contributes `unknown`, not a green. This follows the shape
  `GetLocalHASTResourceStatus` already established in ADR-0119 — a
  node-local observation that never forwards through raft, so a Comb
  that cannot be reached yields `unknown` from the surface rather than
  an omission.
- **The UI** is `web/templates/backup.html` plus a summary block on
  `web/templates/maintenance.html`, next to the wave planner, because
  that is where the interlock in §5 is legible. Every unknown renders
  as the literal word `unknown` with the reason. There is no
  "✅" without a drill, no green check derived from a checksum, and no
  empty cell that reads as fine.

## Rejected alternatives

**1. Back up the config files and the datasets, and call it a day.**
This is the naive design and it is what ADR-0127's section implies.
Rejected because the local node configuration is not one file but
three, and **all three carry inline live secrets**:
`nodeconfig.Config.PeerAPIKey` ("a live credential", ADR-0029) and
`nodeconfig.Config.RaftdToken`; `frontendconfig.Config.ManagerAPIKey`;
`raftdconfig.Config.InternalToken` ("must be root-owned, mode 0600").
None of these is in `FSMSnapshotState`, so the raft export path does
not already cover them and the operator has no other way to get them
back — which is exactly what makes "back up the config files" so
tempting and exactly what makes it wrong. It is indistinguishable from
writing four live credentials to a second host, and the second host is
somewhere this project does not control. `origincert.WritePair` puts a
private key next to its cert, so the same argument applies there. A
manifest must be built field by field against an explicit exclusion
list, because a whole-file copy has no way to express "everything
except the four fields that matter".

**2. Back up raft state as part of every Cell backup, so one restore
reconstructs everything.** Rejected: `raftd -export` / `-restore` and
`ConfigArchive` already do this, correctly, with their own versioning
and checksum, offline (ADR-0051, ADR-0124). Reimplementing it in the
backup path would create a second, weaker control plane-restoration
mechanism that could be invoked by an RPC while the Colony is live. The
boundary is enforced: no backup RPC writes raft state.

**3. Rely on ZFS snapshots plus `zfs send` to a second Comb, and drop
the manifest.** Tempting, because ADR-0130 is already building the
stream, and a stream plus `zfs list` is nearly enough to restore. But
without a manifest there is no way to answer the only question that
matters after a disaster — *which artifacts belong together, at what
instant, and what did we deliberately leave out* — and no way to
detect a half-finished generation. A `zfs send` that was interrupted
is indistinguishable from a complete one without an external record.
The manifest is that record.

**4. Reuse `internal/jailarchive` as the backup engine.** Rejected as
a *whole-system* answer, while explicitly *adopted* as a component.
`jailarchive.Extractor.Extract` is a single function that unpacks an
archive to a directory: it is the restore half of a jail backup and
nothing else. It has no manifest, no multi-artifact coordination, no
VM support, no verification, and no notion of what was excluded. Build
the manifest and the orchestration around it; call `Extract` for the
jail out-of-band materialisation rather than writing a second
unpacker.

**5. Encrypt every artifact at rest in v1.** Rejected for v1 as a
sequencing choice, not a judgement: encryption requires a key, the key
must be stored somewhere, and "somewhere" is a new secret with its own
backup problem, on top of an archive format that nothing else in the
repository reads. The v1 answer is exclusion (no Apiary secret in the
archive at all) plus an explicit, documented statement that **guest
data inside a `zfs send` stream is not encrypted by this system**, plus
the operator owning the target's location. v3 off-host targets bring
encryption with them. An operator who cannot accept unencrypted guest
data uses a ZFS-encrypted dataset as the target, which is a
filesystem-level answer this design does not need to own.

**6. Make backup a Flight Plan step, as ADR-0127 specifies.** Rejected
for v1 because there is no Flight Plan engine in the repository — grep
for `FlightPlan` across Go, proto and templates returns nothing.
Designing against a subsystem that does not exist produces an ADR that
cannot be implemented and a schema that will be wrong. Scheduling goes
on the leader's reconcile loop; when a Flight Plan engine lands, a
backup becomes a step type then, and this ADR gets amended.

**7. Compute a numeric RPO and a "backup is stale" health boolean.**
Rejected: ADR-0119 already declined to turn HAST dirty extents into a
numeric RPO, for the same reason. A backup's age is a fact; whether it
is acceptable depends on the workload, and Apiary does not know what
the workload is worth. The surface shows age, artifact count, and
verification state and stops there. Health stays `unknown` when the
evidence is missing, per ADR-0056 and ADR-0122.

## Consequences

### Positive

- A backup is a **self-describing object**. The manifest answers what
  was captured, from which instant, on which Comb, at which raft
  applied index, with which digests, and what was deliberately left
  out — without needing the system that produced it to still exist.
- Secrets are structurally excluded rather than excluded by policy.
  A field-by-field manifest cannot leak `PeerAPIKey` the way a
  whole-file copy does, and the negative test proves it.
- A half-finished generation is not a backup, so there is no
  plausible-looking-but-incomplete archive to be misled by. This is
  the single highest-value property in the design.
- The restore path has a **written blast radius of one Cell**, an
  out-of-band default, a GUID check against clobbering newer state, and
  a quorum requirement for the only mode that writes to the FSM.
- v1 reports `last_verified_restore: unknown` rather than pretending.
  The gap is visible from the first release, and closing it is a
  visible, tracked piece of work rather than a hidden one.
- Backup composes with existing machinery rather than replacing it:
  `jailarchive.Extractor` for jail out-of-band materialisation, the
  `isostore` / `freebsdimg` checksum idiom for integrity, the ADR-0119
  wave planner for scheduling discipline, and the ADR-0090
  stop-before-restore reasoning.
- v1 needs no new transport, no new credential, and no new trust
  boundary: local disk.

### Negative

- Local-disk-only v1 means a **host loss takes the backup with it**.
  This is a real and serious limitation, and the ADR says so rather
  than calling local-only "a backup system" without qualification. It
  is a floor, not a destination; v2's second Comb is the fix.
- Guest data inside a `zfs send` stream is **unencrypted in v1**. An
  archive copied to removable media or a second host is readable by
  anyone who can read those bytes.
- `CONSISTENCY_QUIESCED` costs **downtime**, and `CONSISTENCY_SNAPSHOT`
  does not deliver application consistency. There is no free tier, and
  the choice is per policy and permanent for each generation.
- The one-at-a-time, yield-to-replication discipline means a **backup
  can be delayed indefinitely** on a busy Comb. That is the correct
  priority order and it will still annoy someone.
- The manifest is a **schema Apiary must now keep forward-compatible
  forever**, because archives outlive binaries. This is a permanent
  maintenance obligation, the same one `ConfigArchive` already carries.
- `unknown` will be shown a great deal. A first release where the
  honest answer is often "we don't know" is uncomfortable, and it is
  the correct answer.
- Guest-level secret handling is **not yet modelled**. When it lands,
  `SecretGap` grows a category, and every existing archive becomes
  incomplete in a newly-visible way. Cheap now, retroactive later.

## Implementation notes

### Raft state (raft-replicated; no secrets, no key material)

New messages in `api/internalpb/state.proto`, following the existing
`FSMSnapshotState` map-of-record pattern. None of these carry
credentials. None of them are applied by any RPC that a backup job
invokes; the FSM changes only on an operator-initiated write.

- **`BackupPolicy`** — `id`, `name`, `selector` (repeated cell ids,
  empty means "all cells on the nodes named"), `target`
  (`local_dir`, or a `node_id` in v2), `consistency_mode`, `restart_
  after_quiesce`, `max_skew_seconds`, `retention_count`,
  `retention_age_seconds`, `verify_sample_bytes`, `drill_enabled`,
  `created_unix`.
- **`BackupJob`** — `id`, `policy_id`, `generation_id`, `state`
  (`pending`/`running`/`succeeded`/`failed`/`unknown`),
  `started_unix`, `finished_unix`, `error` (verbatim reason, never a
  smoothed-up summary), `cell_ids`.
- **`BackupStatus`** — `policy_id`, `generation_id`, `manifest_id`,
  `artifact_count`, `bytes`, `target`, `earliest_snapshot_unix`,
  `latest_snapshot_unix`, `verify_state`, `last_succeeded_at`.
  One per policy, overwritten on each success; failures append a
  `BackupJob`.
- **`BackupLease`** — `holder_id`, `generation` (monotonic), `expires_
  unix`. Mirrors ADR-0130's replication lease: a stale lease is
  reclaimed on generation bump, never on wall-clock alone.
- **`RestoreRecord`** — append-only. `id`, `cell_id`, `manifest_id`,
  `mode` (`out_of_band` / `in_place`), `source_dataset_guid`,
  `observed_dataset_guid`, `started_unix`, `finished_unix`, `state`,
  `error`. **Append-only, never overwritten** — a restore is the most
  destructive thing this system does, and its history must survive
  being repeated.
- **`BackupPruned`** — append-only. `generation_id`, `pruned_unix`,
  `bytes_reclaimed`, `reason`.

Failure modes: a `BackupStatus` write that cannot reach quorum leaves
the *capture* intact and the *record* missing — the UI shows
`unknown`, and the sweeper must not delete a generation whose status
was never written, because it cannot prove the generation is
unreferenced. A `RestoreRecord` write that fails after a successful
out-of-band restore must not roll back the restore; it becomes a
`BackupJob` with `state: unknown` and an operator-visible orphan
dataset, which is exactly what §7 says to leave in place.

### Manifest schema (`api/internalpb/backup.proto`, new)

A new proto file, not an addition to `manager.proto`, because the
manifest is an **on-disk archive format** with a long life independent
of the RPC surface — the same reason `ConfigArchive` lives in
`internalpb` and not in the service definition.

- `BackupManifest { uint32 format_version = 1; int64 created_unix = 2;
  string comb_id = 3; string colony_id = 4; uint64 applied_index = 5;
  string policy_id = 6; string generation_id = 7;
  ConsistencyMode consistency = 8; int64 earliest_snapshot_unix = 9;
  int64 latest_snapshot_unix = 10;
  repeated BackupArtifact artifacts = 11;
  repeated SecretGap secret_gaps = 12;
  int64 max_skew_seconds = 13; bytes checksum = 14; }`
- `BackupArtifact { string artifact_id = 1;
  oneof payload { DatasetArtifact dataset = 2; ConfigArtifact config =
  3; ImageArtifact image = 4; ReferenceArtifact reference = 5; }
  bytes sha256 = 6; uint64 bytes = 7; }`
- `DatasetArtifact { string dataset = 1; string snapshot = 2;
  string source_guid = 3; bool recursive = 4; string parent_dataset =
  5; }` — `recursive` and `parent_dataset` are v2 fields, reserved
  and unset in v1, present so the schema does not have to change when
  multi-pool sets arrive.
- `ConfigArtifact { string cell_kind = 1; string cell_id = 2;
  bytes definition = 3; bytes rendered_config = 4; }` — both
  marshalled, neither applied in v1.
- `ImageArtifact { string name = 1; string kind = 2; string
  expected_sha256_hex = 3; string observed_sha256_hex = 4; uint64
  bytes = 5; }`
- `ReferenceArtifact { string name = 1; string kind = 2; string
  expected_sha256_hex = 3; }` — no payload; `freebsdimg` and
  `ListTemplateNames` re-verify on demand.
- `SecretGap { string kind = 1; string why = 2; string live_copy_
  location = 3; string restore_procedure = 4; }`
- Fields restoring depends on are marked `restore_critical` in comments;
  a reader seeing an unknown `format_version` refuses rather than
  guesses.

Failure modes: `checksum` mismatch → reject the manifest, report
`unknown`, never fall back to reading artifacts. A `DatasetArtifact`
whose `source_guid` is empty (a non-ZFS dataset, or a v1 bug) makes
in-place restore **ineligible** — the GUID guard in §7 cannot be
evaluated, and absence of a guard is not a passing guard. Out-of-band
restore remains available, because it does not need the GUID.

### managerd RPC handlers (`internal/manager/server.go`)

All on `ManagerServiceServer`, all with the standard
`if s.backup == nil { Error: "this node has no backup support\n  configured" }`
guard, matching `CreateVMSnapshot`. **None of these forwards through
raft except where explicitly noted**, and none of them touches raft's
log or snapshot files.

- `RunBackup(RunBackupRequest) returns (RunBackupResponse)` — node-
  local, executes on the Comb that owns the Cell. Acquire the lease
  (raft write) → capture per §3 → write the manifest atomically →
  report. Failure modes: lease unavailable → an `Error` naming the
  holder, state `unknown`, not a silent skip. Snapshot
  failure on any artifact → discard the generation. Manifest write
  failure → the generation is garbage and the sweeper removes it.
- `VerifyBackup(VerifyBackupRequest) returns (VerifyBackupResponse)` —
  node-local, read-only. Unreachable target → `verify_state:
  "unknown"`, `Error` empty, exit success. This asymmetry is
  deliberate: unreachable is not corruption, and the caller must be
  able to tell them apart.
- `RestoreBackup(RestoreBackupRequest) returns
  (RestoreBackupResponse)` — the guard rails of §7 in code.
  Out-of-band is the default and needs no quorum. In-place requires
  `clobber: true`, an observed-stopped Cell, a GUID match, and
  quorum; each failure returns a distinct `Error` string naming the
  failed guard, because an operator who cannot tell which guard
  stopped them will try again harder.
- `ListBackups` / `ListBackupPolicies` / `GetBackupStatus` —
  read-only, raft-served, follow the ADR-0122 evidence rules.
- `PruneBackups(PruneBackupsRequest)` — raft write; appends
  `BackupPruned`; refuses to touch a drilled generation.

### Reconcile loop (`internal/cluster/backup.go`)

On the leader, on the existing reconcile tick:

1. Read `BackupPolicy` from the FSM. If no policy, do nothing.
2. If the ADR-0119 wave planner has a wave in progress, or quorum is
   absent, **do not start** — and record nothing, because "we
   chose not to run" is not evidence about the backup.
3. Compute the due set by schedule. If nothing is due, return.
4. If the lease is held by another node, return.
5. For each due Cell: check the interlock (no replication stream
   advancing on that Comb), check the consistency mode's preconditions
   — for `QUIESCED`, verify the stop can be *observed*, and if it
   cannot, mark the job `unknown` with the reason and **stop**; never
   downgrade.
6. Dispatch to the owning Comb via `RunBackup`. The leader does not
   execute captures; only the Comb that owns the dataset can read it
   cheaply and consistently.
7. Record the outcome in the FSM.

Failure modes: a leader change mid-backup leaves the lease to expire
and the generation to be garbage; the next leader's sweeper cleans it,
and the job's record says `running` forever, which is `unknown` — the
honest answer, and the sweeper's `running`-past-expiry sweep
converts it to `unknown` rather than to `failed`, because "we lost
track" is not "it broke".

### Frontend surface

- `web/templates/backup.html` — policies, generations, per-generation
  artifact count and size, `verify_state`, `last_verified_restore_at`.
  A restore button whose primary path is out-of-band, with the
  in-place path behind a second confirmation dialog that spells out
  the blast radius in prose before enabling the button.
- `web/templates/maintenance.html` — a backup summary block beside the
  ADR-0119 wave planner, because the interlock is the thing an
  operator most needs to see together.
- `web/templates/cluster_evidence.html` — the ADR-0122 surface gains
  a backup row: age of the newest successful generation per failure
  domain, and `never drilled` as an explicit state, which is
  `unknown`, not a pass.
- The API-key-issuing flow on `web/templates/apikeys.html` gets **no**
  change: backup does not touch keys, and a backup of the Colony does
  not make a revoked key usable again.
- Every unknown renders as the literal word, with its reason. There is
  no icon that means "probably fine".

## Test plan

**macOS cannot validate this feature.** Apiary has no ZFS, no HAST, no
bhyve, no jail(8), and no real network latency on a developer laptop.
Everything below that is marked *testbed* can only be established on
brood (10.90.0.94) or drone (10.90.0.95), the bare-metal FreeBSD
16.0-CURRENT testbed. A green `go test ./...` on macOS proves the
manifest encodes, the strip holds, and the guards fire — nothing more.

Unit-testable on macOS, no privileges required:

- Manifest round-trip: encode → decode → re-encode is byte-identical
  for a fixture with every field populated.
- Forward compatibility: a v2 manifest (synthetic, with an extra
  unknown field and `format_version: 2`) decodes under the v1 reader,
  the unknown version is surfaced, and a `restore_critical` change
  causes a refusal rather than a partial restore.
- Checksum: a single flipped byte in `manifest.pb` or in an artifact
  rejects the generation.
- **The secret strip.** Fixture `nodeconfig` (`PeerAPIKey`,
  `RaftdToken`), `raftdconfig` (`InternalToken`) and `frontendconfig`
  (`ManagerAPIKey`) values carrying known token strings, plus a
  fixture `origincert` key PEM, are run through manifest
  construction; the encoded bytes are searched for every fixture
  string and the test fails if any is found, and the resulting
  `SecretGap` list is asserted to name each one. This test is
  unconditional — no `testing.Short()`, no env var, no way to skip
  it.
- All-or-nothing: a forced failure on the third artifact of five
  leaves no `manifest.pb` anywhere, and `ListBackups` reports nothing.
- Consistency-mode downgrade: a `QUIESCED` job on an unobservable
  Cell records `unknown` with the reason and does **not** produce a
  generation.
- Restore guards, each in isolation: not-observed-stopped → refused;
  GUID mismatch → refused; `clobber: false` → refused; no quorum
  with `in_place` → refused; out-of-band with no quorum → succeeds
  and writes no FSM state.
- Evidence: every RPC that has no answer returns `unknown` and never
  `false`, `healthy`, or empty-with-no-error. Enumerate the states
  explicitly in the test table so a new field cannot quietly default
  to a healthy-looking zero value.

*Testbed only* — brood/drone, and nowhere else:

- Real `zfs send` / `zfs receive` of a jail root and a VM `disk.img`
  through the manifest path, with byte-identical round-trip comparison
  via `zfs diff` against the source snapshot.
- A real interrupted `zfs send`, to confirm the partial generation is
  discarded and never published.
- A real `quiesced` cycle: stop a Cell, observe the stop through
  `GetVM` / `GetJail`, snapshot, stream, restart — and confirm the
  snapshot instant is the one the manifest claims.
- **A real restore drill.** Restore a generation out-of-band on the
  other node, boot the Cell, write a file, read it back. This is the
  only test that can move `last_verified_restore_at` off `unknown`,
  and until someone runs it on the testbed, the honest state of the
  system is "no backup has been proven restorable."
- The in-place restore path against a real dataset, including the
  GUID-mismatch refusal against a dataset that was destroyed and
  recreated.
- Backup under concurrent load, and backup *during* an ADR-0130
  replication stream, to confirm the yield and the throttle actually
  hold and that replication keeps its reserved bandwidth.
- Backup during a wave-planner sequence, to confirm the interlock
  neither starts nor starves.
- Two Combs racing for the same generation directory, to confirm the
  lease holds.

## Open questions

1. **Off-host target choice.** S3-compatible or NFS? S3 gives
   versioned object retention and cheap off-site; NFS gives a
   filesystem the operator can `zfs send` to directly. The choice
   changes the v3 credential model. This is a user decision.
2. **Encryption at rest.** The exclusion rule covers Apiary's own
   secrets, but guest data inside a `zfs send` stream is plaintext in
   v1. Is "tell the operator to use a ZFS-encrypted target dataset"
   an acceptable answer, or must v1 ship stream-level encryption? If
   the latter, the key custody question has to be answered first.
3. **Drill frequency and where drills run.** Default is
   newest-plus-oldest weekly, on whichever Comb the wave planner picks.
   That may be too slow, too fast, or the wrong place.
4. **Retention floor.** Should the sweeper be forbidden from deleting
   any generation newer than the last *drilled* one, or only the exact
   drilled generation? The first is safer and wastes space; the second
   is tighter and has a gap.
5. **Guest secret handling.** The manifest's `SecretGap` model is
   built for secrets that do not exist in the code yet. When guest
   credential management lands, does it integrate with this, or does
   it need its own exclusion channel?
6. **Config artifact drift.** A config artifact is recorded but never
   applied in v1. Should a future version refuse an out-of-band
   restore whose config artifact does not match the restored data's
   actual schema, or warn and proceed?
7. **Local-only floor.** Is a local-disk-only v1 acceptable as a ship
   gate, given that a host loss takes the backup with it? Or should
   the second-Comb target move from v2 into v1?

## References

- **ADR-0124** — Read-Only Offline Raft Status (`raftd -status`); the
  existing offline raft tooling this design sits beside and does not
  duplicate.
- **ADR-0051** — raftd configuration save/restore (`-export` /
  `-restore`); `ConfigArchive` in `api/internalpb/state.proto` and
  `cmd/raftd/main.go`. **Out of scope here by decision.**
- **ADR-0127** — Sylve.io Feature Adoption for Apiary; section 3 and
  the Phase 2 sequencing this ADR implements, with the corrections
  recorded in Context.
- **ADR-0130** — ZFS dataset replication between Combs; the sibling
  Phase 2 item, and the boundary stated in §6.
- **ADR-0119** — Replica freshness evidence and maintenance wave
  planner; the scheduling interlock in §5.
- **ADR-0090** — VM snapshot and restore; the existing
  `CreateVMSnapshot` / `RestoreVMSnapshot` handlers and the
  stop-before-rollback reasoning reused in §3 and §7.
- **ADR-0089** — Cross-node jail base-template fetch; the `zfs send`
  streaming path over managerd mTLS that `Send` / `Receive` and
  `PushJailTemplateTo` already use. Why a capture does not block on
  pool contention, and the transport v2's second-Comb target reuses.
- **ADR-0056** — Evidence-Aware Health v1; the `unknown` rule.
- **ADR-0118** — Why-not quorum blocker detail; `unknown` never
  `false`.
- **ADR-0122** — Cluster-wide Evidence-Aware Health over the API; the
  cluster surface this extends.
- **ADR-0026**, **ADR-0028** — HAST; the live-mirror mechanism backup
  is explicitly not.
- **ADR-0023** — API-key auth; `auth_enabled` never reverting to
  false, the same monotonic-evidence discipline applied to backup
  verification.
- **ADR-0029** — peer API key; the live credential in
  `nodeconfig.Config.PeerAPIKey` that the secret strip exists for.
- **ADR-0103** — action-preflight restart guardrail; the
  `restart_leases` / `restart_records` precedent for lease-shaped
  raft state, and the `unknown`-not-false discipline for evidence
  that may be incomplete.
- `internal/jailarchive`, `internal/isostore`, `internal/freebsdimg`,
  `internal/origincert`, `internal/raftdconfig`,
  `internal/frontendconfig`, `internal/restshimdconfig`,
  `internal/nodeconfig`, `internal/zfs`,
  `internal/bhyve`, `internal/jail` — the surveyed packages in "What
  already exists".
- `internal/manager/server.go` — `ManagerServiceServer`, the standard
  dependency guard, and `RestoreVMSnapshot` at line 3120.
- `api/rpc/manager.proto` — the service definition these handlers
  extend.
- `api/internalpb/state.proto` — the raft FSM and `ConfigArchive`.
- `cmd/raftd/main.go` — `raftd -export` / `-restore`.
- brood (10.90.0.94), drone (10.90.0.95) — the only place any of this
  can actually be validated.
