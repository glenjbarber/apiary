# ADR-0156: A purpose-built Colony consensus protocol layer above HashiCorp Raft

Status: Proposed
Supersedes: None
Superseded by: None
Affected projects: Apiary
Legacy identifier: ADR-0156 (Apiary)
Loreloom identifier: Pending allocation by Glen

## Status qualifications

Proposed means the wording has not been approved, and no approval date or approver is recorded here.
Nothing in this document is implemented. Every behaviour described under "Decision" is a design proposal, and the staging order in "Consequences" has not been started.
The number 0156 is the next free apiary-local `docs/adr/` number after ADR-0155. Loreloom allocation is Glen's to do; `docs/adr/MIGRATION.md:31-37` requires central allocation for every shared identifier, so nothing here is padded or offset.
The section "Decisions for Glen" lists the choices this proposal makes by default and that Glen may override before any code is written.
One factual disagreement between records is recorded here rather than reconciled: `docs/adr/0147-automated-install-and-two-way-peer-authorized-join.md:17-30` says Parts 2 and 3 of that ADR are "not yet built", but the protos and the FSM now carry an introduction code, a second PIN, two approval attestations, a root-owned authorization ID, single-use consumption, and a trusted-peer pin (`api/rpc/manager.proto:3929`, `api/internalpb/state.proto:1110`, `api/internalpb/state.proto:1164-1165`, `api/internalpb/state.proto:1178`, `internal/raft/peers.go:60`). ADR-0147's status text understates what the code now does. That is noted for accuracy; fixing ADR-0147 is out of scope here.

## Context

### The consensus engine underneath Apiary is HashiCorp Raft, and it is fine at being that

Apiary's replicated state runs on `github.com/hashicorp/raft`. `internal/raft/` is about 6,098 lines of production Go (15,857 lines including the 9,759 lines of test code across 27 `_test.go` files), and `internal/raft/fsm.go` alone is 2,348 lines with 43 command arms spanning `internal/raft/fsm.go:161` through `internal/raft/fsm.go:245`. `FSM.Apply` is at `internal/raft/fsm.go:142`, the digest recompute is deferred inside it at `internal/raft/fsm.go:151`, and `f.lastIndex = log.Index` is at `internal/raft/fsm.go:153`.

That engine provides the things a replicated log must provide: log replication, leader election with pre-vote on by default for the TCP transport (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/api.go:584`), a stable store, snapshots, and leadership transfer.

It provides no colony identity. The `raft.Config` struct has `LeaderLeaseTimeout` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/config.go:205`), `LocalID` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/config.go:210`), and `PreVoteDisabled` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/config.go:236`), and nothing that identifies a cluster, a colony, an epoch, or an incarnation. A search of the non-test Go sources in that module for `clusterid`, `cluster_id`, `epoch`, or `incarnation` returns nothing.

It provides no joint consensus. `raft.Server` has exactly three fields, `Suffrage`, `ID`, and `Address` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/configuration.go:61-67`), and a membership change appends one `LogConfiguration` entry describing one configuration (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/raft.go:1204`, `~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/raft.go:1229-1234`). The exported membership API is `AddVoter` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/api.go:994`), `AddNonvoter` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/api.go:1012`), `RemoveServer` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/api.go:1028`), and `DemoteVoter` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/api.go:1045`). There is no `CheckQuorum` configuration field, so a leader steps down on `LeaderLeaseTimeout` without quorum contact rather than on an explicit no-quorum check.

None of that is a defect in HashiCorp Raft. Raft is a log-replication consensus algorithm and it is being used as one.

### Apiary's own configuration carries no colony identity either

`internal/raft/config.go:16` defines the `Config` struct. It has `NodeID` (`internal/raft/config.go:19`), `DataDir` (`internal/raft/config.go:23`), `BindAddr` (`internal/raft/config.go:28`), `TLSCert`/`TLSKey`/`TLSCA` (`internal/raft/config.go:37-39`), `PreferredLeaderID` (`internal/raft/config.go:52`), and `FastElectionTiming` (`internal/raft/config.go:65`), and it ends at `internal/raft/config.go:66`.

There is no `ColonyID`. There is no epoch. There is no incarnation field. A node therefore knows its own name and its own address, and knows nothing about which colony it belongs to.

### The only `ColonyID` in the tree is caller-supplied backup provenance

`internal/backup/manifest.go:297-298` declares `ColonyID string` with the comment "ColonyID identifies the Colony the node belonged to at capture". `internal/backup/capture.go:137` carries the same field on the capture request, and `internal/backup/capture.go:304` copies it into the manifest.

That is the whole extent of it. A search of the non-test Go sources for `ColonyID` returns exactly those three sites: the manifest field, the request field, and the copy. No production caller sets it, and no production code reads it back. It is a label on a backup file, not an identity anybody enforces.

### The Colony CA fingerprint exists, and is not wired into the control plane

`internal/colonyca` implements a per-colony certificate authority. `Fingerprint` is at `internal/colonyca/colonyca.go:262` and returns the lowercase hex SHA-256 of the DER bytes of the first certificate in the PEM input (`internal/colonyca/colonyca.go:270-271`). `GenerateCA` is at `internal/colonyca/colonyca.go:101`; it writes the CA key with mode 0600 (`internal/colonyca/colonyca.go:149`) and the CA certificate with mode 0644 (`internal/colonyca/colonyca.go:152`), into `ca.key` and `ca.pem` (`internal/colonyca/colonyca.go:64-65`).

ADR-0155 is Accepted. Its own text says "Nothing outside the package calls it yet" (`docs/adr/0155-colony-certificate-authority.md:29`). That sentence is now slightly stale: `cmd/apiaryctl/colonycacmd.go` calls `colonyca.GenerateCA` (`cmd/apiaryctl/colonycacmd.go:128`), `colonyca.IssueLeaf` (`cmd/apiaryctl/colonycacmd.go:204`), and `colonyca.Fingerprint` (`cmd/apiaryctl/colonycacmd.go:212`). What has not changed, and what matters here, is that the CA is not used by `raftd`, by `managerd`, or by the installer. It is a CLI tool and a library, not an identity anchor in the running system.

### The transport authenticates certificates but does not bind them to a colony or to a node

`internal/raft/tls_transport.go:32` is `newTLSConfig`. It loads the local certificate and key, reads the CA file into a single `x509.CertPool`, and returns a `tls.Config` with `RootCAs` and `ClientCAs` set to that pool, `ClientAuth: tls.RequireAndVerifyClientCert`, and `MinVersion: tls.VersionTLS12` (`internal/raft/tls_transport.go:45-51`).

That is ordinary mutual TLS as specified by ADR-0078. There is no `VerifyPeerCertificate` callback, no comparison of a subject or SAN against an expected node ID, and no colony identifier of any kind in the handshake. A peer certificate signed by a CA that happens to be the configured `raft_tls_ca` is accepted, whether or not that peer belongs to this colony.

### The join payload carries no colony identity

`api/rpc/manager.proto:3868` defines `RequestJoinColonyRequest`. Its fields are the joining node's own `node_id` (`api/rpc/manager.proto:3874`) and `raft_bind_address` (`api/rpc/manager.proto:3875`), a `timeout_ms`, the `target_address` (`api/rpc/manager.proto:3889`), a `tls_cert_fingerprint` (`api/rpc/manager.proto:3896`), the joiner's observed log state (`api/rpc/manager.proto:3908-3909`), an `introduction_code` (`api/rpc/manager.proto:3929`), and the joiner's `advertised_fingerprints` (`api/rpc/manager.proto:3948`). A count of `colony_id` occurrences in `api/rpc/manager.proto` is zero.

The replicated side matches. `api/internalpb/state.proto:1090` defines `PendingJoinRequest` with `request_id`, `node_id`, and `raft_bind_address` (`api/internalpb/state.proto:1091-1093`), a `code` (`api/internalpb/state.proto:1110`), timestamps and `status` (`api/internalpb/state.proto:1111-1113`), a `tls_cert_fingerprint` (`api/internalpb/state.proto:1117`), the joiner's log state (`api/internalpb/state.proto:1135-1136`), `window_opened_at_unix` (`api/internalpb/state.proto:1148`), two approval attestations (`api/internalpb/state.proto:1164-1165`), an `authorization_id` (`api/internalpb/state.proto:1178`), and a `consumed_at_unix` (`api/internalpb/state.proto:1179`). No colony ID. No epoch.

### Status reports no colony identity

`api/internalpb/raftd.proto:251` defines `StatusResponse`: `is_leader` (`api/internalpb/raftd.proto:252`), `leader_id` (`api/internalpb/raftd.proto:253`), `node_id` (`api/internalpb/raftd.proto:254`), `last_log_index` (`api/internalpb/raftd.proto:255`), `applied_index` (`api/internalpb/raftd.proto:256`), `raft_state` (`api/internalpb/raftd.proto:259`), `servers` (`api/internalpb/raftd.proto:263`), `state_digest` (`api/internalpb/raftd.proto:279`), and `membership_error` (`api/internalpb/raftd.proto:299`). There is no `colony_id` field and no `epoch` field.

So there is no way to ask a node "which colony are you?" and no way to compare two nodes' answers. The only way to tell that two raftd processes are the same colony today is to infer it from their membership views and their TLS trust.

### The peer trust store is certificate trust, not colony identity

`internal/raft/peers.go:60` is `applyPinTrustedPeer`. It records a pinned peer certificate, keyed by node ID. It deliberately refuses to record voter status at pin time: the error string at `internal/raft/peers.go:83` says "membership is recorded by SetTrustedPeerVoter after AddVoter, never by the act of pinning a certificate".

That is the right design for what it is. It is a trust store for peer certificates, as ADR-0147 requires. It answers "do I trust this certificate", not "is this node in my colony".

### A split-leader mechanism exists, and it is a hypothesis, not a proven cause

`cmd/raftd/main.go:479` is `startupJoinOrBootstrap`. The switch order is: existing state resumes (`cmd/raftd/main.go:481`), a join socket joins an existing cluster (`cmd/raftd/main.go:483`), `-await-join` waits passively (`cmd/raftd/main.go:488`), and the default branch calls `node.Bootstrap()` (`cmd/raftd/main.go:490-491`). `internal/raft/node.go:177` is `Bootstrap`, and it seeds the sole voter through `raft.BootstrapCluster` (`internal/raft/node.go:186`).

`cmd/raftd/main.go:302` is `resetDataDir`. It requires an exact confirmation phrase (`cmd/raftd/main.go:303-305`), renames the existing data directory to `<dataDir>.reset-backup-<unix>` (`cmd/raftd/main.go:307`), recreates an empty directory (`cmd/raftd/main.go:317`), and logs that the next normal start will bootstrap a fresh single-node cluster (`cmd/raftd/main.go:321`).

The mechanism is therefore: an empty data directory with no join flags self-bootstraps a brand new single-voter cluster. Two independently bootstrapped raftd instances are two unrelated clusters, and because no colony identifier exists anywhere in the tree, nothing in the system can notice that they are unrelated. A wiped, reinstalled, or partially converted node can silently form a competing one-node colony and elect itself leader, and that node will believe it is the leader of the whole Colony.

This is **consistent with** the October 16 split-leader symptom. It is **not** the proven cause of it. That incident was never root-caused. It could equally have been a defect in HashiCorp Raft, operator error, or a hardware fault in the Sting PSU. This ADR deliberately does not claim otherwise, and nothing in the design below depends on the mechanism being the cause. What matters is that the mechanism is real, it is reachable through documented operations (ADR-0038's tiered reset), and it is currently undetectable.

### There is no split-brain detector anywhere in the tree

There is no component that collects independent leader claims and compares them against a shared term or epoch. There is no fencing token checked by writes across a leadership change. The `ColonyUpdateFence` at `api/internalpb/state.proto:1798` is the closest existing shape: it carries `operation_id`, `holder_node_id`, a `fence_token`, and a `holder_incarnation` (`api/internalpb/state.proto:1799-1802`), and it exists to fence a controlled colony update across leader changes. It fences one operation. It does not identify a colony and does not fence ordinary writes.

### The quorum and health machinery is real, and it is point-in-time rather than consensus

`internal/cluster/simulate.go:391` is `ComputeQuorumImpact`, and it computes `quorumSize := total/2 + 1` (`internal/cluster/simulate.go:421`) with `survives := remainingReachable >= quorumSize` (`internal/cluster/simulate.go:422`). Unknown voters are not credited as reachable (`internal/cluster/simulate.go:382-390`), and there is a single-voter note at `internal/cluster/simulate.go:430-431`.

`internal/restartplan/quorum.go:143` is `EvaluateQuorumSafety`. Its doc comment (`internal/restartplan/quorum.go:113-142`) says it deliberately does not reimplement the arithmetic and instead delegates to `internal/cluster.ComputeQuorumImpact`, so that unknown-reachability handling, non-voter targets, and single-voter clusters apply unchanged. Its precedence order is: a failed probe read is Unknown and is not overridable by `Force` (`internal/restartplan/quorum.go:129`), a non-voter target is Allow, then the quorum arithmetic, then an additional block-by-default on the leader.

`internal/manager/raftdquorum.go:202` is `raftdQuorumFact`. It fails the probe closed when the membership read fails (`internal/manager/raftdquorum.go:231`), when `leader_id` is empty (`internal/manager/raftdquorum.go:244`), when the target is not in the membership (`internal/manager/raftdquorum.go:286`), and when the probe read itself fails (`internal/manager/raftdquorum.go:305`).

All of this is point-in-time probing. None of it compares per-node terms, per-node committed indexes, or per-node FSM digests against each other to decide whether the nodes agree. ADR-0122's rule that missing observations stay Unknown and never healthy, and ADR-0118's per-voter Why Not detail, are the right shapes, but they are evidence collection, not an acceptance protocol.

### ADR-0143 already computes the thing a divergence check needs

`internal/raft/fsm.go:2143` is `StateDigest`. Its doc comment (`internal/raft/fsm.go:2127-2142`) states the contract precisely: two voters whose digests are equal hold identical state, two voters whose digests differ do not, and the digest deliberately does not say which one is wrong. The digest is cached in the FSM (`internal/raft/fsm.go:106-112`) and recomputed on every Apply (`internal/raft/fsm.go:2159-2160`). It is exposed as `state_digest` in `StatusResponse` (`api/internalpb/raftd.proto:279`), where the proto repeats that `applied_index` is deliberately not part of the digest and that a mismatch at equal indexes is a real disagreement while a mismatch at differing indexes may be nothing but a sample taken mid-apply (`api/internalpb/raftd.proto:272-278`).

So the substrate for divergence detection is built and shipped. What is missing is a protocol that says when to compare it, what a mismatch means, and what the system must refuse to do about it.

### The on-disk layout the protocol has to coexist with

`<DataDir>/raft.db` is a single BoltDB file serving as both the log store and the stable store, and it is the file the offline reader opens (`internal/raft/offline.go:170`). Snapshots live under `<DataDir>/snapshots` (`internal/raft/offline.go:35`, `internal/raft/offline.go:350`), with `meta.json` holding the HashiCorp snapshot metadata (`internal/raft/offline.go:39`) and `state.bin` holding the serialized FSM payload (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/file_snapshot.go:28`). `internal/raft/offline.go` reads that BoltDB and those snapshots directly, which is what ADR-0124's read-only offline status is built on; the reader mirrors HashiCorp's own `CurrentTerm` key (`internal/raft/offline.go:27`, `internal/raft/offline.go:199`) and takes an exclusive flock with a timeout (`internal/raft/offline.go:46-51`, `internal/raft/offline.go:177`).

### Restore deliberately produces a fresh single-node colony

`internal/raft/seed.go:54` is `SeedSnapshot`. It refuses a data directory that already has raft state (`internal/raft/seed.go:60-66`), creates an empty BoltDB file purely so it exists on disk (`internal/raft/seed.go:75-82`), and writes a snapshot whose configuration is the local node as sole server (`internal/raft/seed.go:94-101`) at index 1, term 1, configuration index 1 (`internal/raft/seed.go:103`).

It preserves the serialized FSM state and nothing else. It does not preserve the old log, the stable vote keys, the membership history, the leader identity, the snapshot metadata, or the per-node transport, configuration, or TLS material. As ADR-0051 intends, that is a whole-state point-in-time replacement into an empty node. But it also means a restored node is a brand new one-node colony with a brand new identity, which is precisely the hazard described above.

### The internals RPC surface is already divided into leader-only and local-only

`api/internalpb/raftd.proto` declares 25 RPCs. The leader-only ones are `Apply` (`api/internalpb/raftd.proto:17`), `AddVoter` (`api/internalpb/raftd.proto:25`), `RemoveServer` (`api/internalpb/raftd.proto:29`), `GetVM` (`api/internalpb/raftd.proto:36`), `ListVMs` (`api/internalpb/raftd.proto:40`), `GetNetwork` (`api/internalpb/raftd.proto:47`), `ListNetworks` (`api/internalpb/raftd.proto:48`), `ListAPIKeys` (`api/internalpb/raftd.proto:88`), `GetJail` (`api/internalpb/raftd.proto:92`), `ListJails` (`api/internalpb/raftd.proto:93`), `ExportState` (`api/internalpb/raftd.proto:111`), and `GetAuthorizationUse` (`api/internalpb/raftd.proto:179`). The local-only ones are `Status` (`api/internalpb/raftd.proto:20`), `ListVMsLocal` (`api/internalpb/raftd.proto:69`), `ListNetworksLocal` (`api/internalpb/raftd.proto:70`), `ValidateAPIKeyHash` (`api/internalpb/raftd.proto:83`), `ListJailsLocal` (`api/internalpb/raftd.proto:99`), `GetPendingJoinRequestLocal` (`api/internalpb/raftd.proto:120`), `ListPendingJoinRequestsLocal` (`api/internalpb/raftd.proto:121`), `GetRestartLeaseStateLocal` (`api/internalpb/raftd.proto:128`), `GetColonyJoinWindowLocal` (`api/internalpb/raftd.proto:137`), `GetColonyDiskSizeLocal` (`api/internalpb/raftd.proto:147`), `ListTrustedPeersLocal` (`api/internalpb/raftd.proto:164`), `GetColonyUpdateStateLocal` (`api/internalpb/raftd.proto:194`), and `StepAsideForRestartLocal` (`api/internalpb/raftd.proto:223`).

One of those is declared but not implemented. `GetColonyDiskSizeLocal` is in the proto, and the server embeds `internalpb.UnimplementedRaftInternalServer` (`internal/raft/server.go:21`), so the generated fallback at `api/internalpb/raftd_grpc.pb.go:777-778` returns `codes.Unimplemented`. A search for a hand-written handler outside the generated code finds none. That is a loose end in its own right, and the Colony Protocol layer is going to need a status surface, so this ADR records the fact rather than leaving it implicit.

### Leadership movement is already constrained by the right rule

`internal/raft/stepaside.go:82` is `StepAsideForRestart`. It uses the library's untargeted `LeadershipTransfer()` (`internal/raft/stepaside.go:148`), then waits for a different non-empty leader ID (`internal/raft/stepaside.go:119`, `internal/raft/stepaside.go:163`), and if either step fails it reports that the original leader must stay up (`internal/raft/stepaside.go:109-117`, `internal/raft/stepaside.go:120-129`).

`internal/raft/preferredleader.go:38` sets `PreferredLeaderCooldown = 1 * time.Minute`, and `internal/raft/preferredleader.go:132` is `PreferredLeaderTransfer`, which uses the targeted `LeadershipTransferToServer`. The cooldown state is held per `Node` in memory and is deliberately not persisted (`internal/raft/preferredleader.go:96-98`, `internal/raft/preferredleader.go:141`). It is consulted even on the no-op path where this node is not the leader.

The governing rule in both cases is that Raft safety forbids forcing leadership. A leader may transfer, and a peer may decline.


### The node lifecycle has no notion of a colony it belongs to

`internal/raft/node.go:95` is `New`. It fills in defaults, creates the data directory (`internal/raft/node.go:101`), builds the FSM, sets `raftConfig.LocalID` from the node ID (`internal/raft/node.go:108`), opens the BoltDB store at `<DataDir>/raft.db` (`internal/raft/node.go:120`), opens the file snapshot store at the data directory (`internal/raft/node.go:125`), and creates the transport (`internal/raft/node.go:130`). Its own doc comment says plainly that it does not bootstrap a cluster and that `Bootstrap` must be called for that (`internal/raft/node.go:92-94`).

Every one of those steps is a local operation. Nothing in the construction path consults a colony identity, because there is no colony identity to consult. The result is that "which colony is this node in" is answered by the filesystem alone: if `raft.db` holds state, the node resumes whatever was there, and if it does not, the node is free to become the founder of a new one.

### The status surface is the only place a node describes itself

`internal/raft/server.go:134` is `Server.Status`, and it is a straight translation of the node's own status into the proto (`internal/raft/server.go:146-155`), carrying the leader flag, the applied index, and the state digest. There is no field there for a colony, because `internal/raft/node.go`'s status struct has none either.

This matters for the failure matrix below. Every node-to-node comparison the protocol wants to make, and every operator question of the form "are these two processes the same colony", currently has to be answered by inferring from membership views and TLS trust. The protocol's first requirement is that the answer be a field the node can simply state.

### The join path's authorization is real, and it names no colony

The join machinery is thorough. A join request carries an introduction code (`api/rpc/manager.proto:3929`), the replicated pending join record carries a second PIN and two approval attestations (`api/internalpb/state.proto:1164-1165`) plus a root-owned authorization ID (`api/internalpb/state.proto:1178`) and a single-use consumption timestamp (`api/internalpb/state.proto:1179`), and the trusted-peer pin (`internal/raft/peers.go:60`) is established before the voter is added, because the pin is written during the introduction and `AddVoter` promotes it afterwards (`internal/raft/peers.go:18`, `internal/raft/server.go:174`). This is what ADR-0083, ADR-0097, and ADR-0147 asked for, and it is implemented.

What it authorizes is a join into a raft cluster. The request carries `node_id` (`api/rpc/manager.proto:3874`), a bind address (`api/rpc/manager.proto:3875`), a target address (`api/rpc/manager.proto:3889`), and a certificate fingerprint (`api/rpc/manager.proto:3896`), and it carries no colony identity at all. A join is therefore mutually authorized and two-way and fail-closed, and it is authorized into an unnamed colony. Two operators who both hold a valid introduction code for the same target will be admitted, and the protocol as it stands has no way to tell them apart from two operators joining two different colonies.

### There is no place in the tree to record a colony's identity

The three candidate homes for a colony identity were all checked and all three are empty:

- `internal/raft/config.go:16` has no colony field, as described above.
- The replicated state has no colony field. `PendingJoinRequest` (`api/internalpb/state.proto:1090`) and `ColonyUpdateFence` (`api/internalpb/state.proto:1798`) are the two replicated records that would carry one if there were one, and neither has a place for it.
- The on-disk layout has no colony file. `<DataDir>/raft.db` and `<DataDir>/snapshots` are the whole of it, and the offline reader (`internal/raft/offline.go`) reads both directly.

That last point is the reason the protocol's design includes a sidecar. A colony identity has to be readable by a node whose replicated state is empty, because the one moment the identity is needed is the moment the node is about to decide whether it may bootstrap a new colony. At that moment there is no FSM to read, so the value has to live next to the data directory as well.

### Side note on ADR housekeeping

`docs/adr/` held 157 numbered ADRs plus `MIGRATION.md`, 158 Markdown files in total, before this one was added. The highest prefix was 0155, with gaps at 0126 and 0153. The prefix 0150 is used by four files (`0150-ai-agent-cell-api.md`, `0150-dedicated-vlan90-switch.md`, `0150-native-system-statistics-and-cluster-monitoring.md`, `0150-preferred-leader-bias.md`) and 0152 by two. `docs/adr/0150-dedicated-vlan90-switch.md` carried the title identifier `ADR-0120` at the time this ADR was written, which collided with `0120-config-rationale-history.md`; that header was corrected to `ADR-0150` in commit `d7ad9f7`, so the filename prefix collision with `0120-config-rationale-history.md` remains but the title identifier no longer does. None of that is fixed here, because fixing it is not this ADR's business, but this ADR is a proposal, and a proposal that lands into a numbering scheme with four 0150s should say so.

## Decision

Apiary will keep HashiCorp Raft as the log-replication engine and will design an Apiary-specific Colony Protocol layer above it. The protocol being designed is the Colony-level identity, epoch, membership, and quorum-acceptance protocol. It is not a replacement consensus engine.

The verified gap is narrow and specific: there is no colony identity anywhere in configuration, in status, or in the join payload. The Colony Protocol layer exists to close that gap and to add the acceptance rules that depend on it.

### Colony identity

Every Colony has a **Colony ID**: a random UUIDv4, generated once, at first bootstrap.

It is stored in the replicated FSM, so it is identical on every voter, survives a snapshot restore, and travels with the state rather than with the disk. It is also written to a small sidecar file in the local data directory at bootstrap time, so that a node whose data directory is empty can still recognise itself as belonging to a colony it was once part of, and can refuse to act on the assumption that it is the founder of a new one.

The sidecar is a cache and a tripwire, not the source of truth. The FSM copy is authoritative once the node is in a colony; the sidecar exists to answer one question before the FSM is readable: "have I ever been part of a colony, and which one?"

The Colony ID is checked:

1. On every join request, in the payload, in the replicated pending join record, and again at the point the joining node is added as a voter.
2. On every peer TLS handshake, by comparing the value the peer presents against the local value.
3. On every inter-node RPC, in the request envelope, so a mismatched peer is refused even when its certificate is trusted.

The second identity anchor is the ADR-0155 Colony CA fingerprint. The two are independent and they answer different questions:

- The CA fingerprint answers "is this CA ours". It is a property of the trust domain, and it is the natural thing to compare on a TLS handshake, because that is where the CA is already in play.
- The Colony ID answers "is this colony this colony". It is a property of the replicated state, and it is the natural thing to compare on a join, because that is where two operators assert that they are forming one colony.

They are checked together, and disagreement is fatal, not degraded:

- Same Colony ID, different CA fingerprint: the node is a member of this colony but is presenting credentials from another trust domain. Refuse the join, refuse the handshake, and report both values. This is either a misconfiguration or an intrusion.
- Different Colony ID, same CA fingerprint: the CA is shared or misconfigured across colonies, and the peer is in a different colony. Refuse. This is exactly the case the current TLS transport cannot catch, because it compares nothing (`internal/raft/tls_transport.go:45-51`).
- Different Colony ID and different CA fingerprint: two unrelated colonies. Refuse, and say so plainly rather than presenting it as a certificate problem.
- Same Colony ID and same CA fingerprint: this is the normal case, and it is the only case in which the peer proceeds.

Neither anchor is derivable from the other. A Colony ID is a random UUIDv4 and carries no structure. A CA fingerprint is a hash of a certificate and carries no colony membership. Both are needed precisely because both can be wrong independently.

### Epoch and term fencing

A Colony ID alone cannot distinguish two generations of the same colony. A colony that is wiped, reset, and rebuilt has the same identity question but a different answer. So there is a second value: a **monotonic epoch**.

The epoch is replicated in the FSM, alongside the Colony ID. It is incremented by a replicated command, and that command is issued only in two situations: a controlled reset of the colony (ADR-0038's tiered reset, where the reset is already an explicit operator action with a confirmation phrase), and a full rebuild after the state is destroyed.

Every node in a colony carries the same epoch, and a node whose local epoch is lower than the epoch it is told about refuses to serve. Concretely: a node that comes back from a partition with a stale epoch, or from a restored backup with an older epoch, is refused admission and refuses to act as a leader or a voter for that colony.

HashiCorp Raft's own term cannot serve this purpose, and reusing it would be a mistake. Raft's term is per-cluster, not per-colony, and it counts elections within one Raft state machine. Two independently bootstrapped clusters both start at term 1 and both increment from there. Their terms are unrelated numbers that only look comparable. Using a term as a colony-wide epoch would mean treating two unrelated clusters' local counters as if they were a shared sequence, which is exactly the comparison that would falsely conclude "same epoch, no conflict" in the split-leader case.

A fencing token is required for any write that can outlive a leadership change. The failure it prevents is specific: a leader that was partitioned away, continued to believe it was leader for a while, and then completes an operation it started as leader, after a different node has taken over. Raft's log replication prevents two leaders from committing conflicting entries to one log. It does not prevent a partitioned old leader from performing a side effect outside the log that a new leader also performs.

The Colony Protocol's rule is that every operation whose effect is not itself a replicated FSM command carries a fencing token derived from the colony's epoch and the current leadership generation, and the resource being mutated refuses an operation whose token is older than the last token it accepted. The existing `ColonyUpdateFence` (`api/internalpb/state.proto:1798`) is the precedent for this shape, including its `holder_incarnation` field (`api/internalpb/state.proto:1802`), which exists for exactly the same reason: to distinguish one managerd process from the next.


### The quorum rule the protocol will use is already written down

The quorum rule this protocol adopts is not a new invention and it is not borrowed from the library. It is the rule the repository already uses when it decides whether a planned change would break quorum.

`internal/cluster/simulate.go:391` is `ComputeQuorumImpact`, and its doc comment sets out the inputs (`internal/cluster/simulate.go:382-390`): the list of voters, the list of non-voters, the set of members known to be reachable, and the set of members known to be unreachable. A voter that is neither confirmed reachable nor confirmed unreachable is not credited as reachable. The function then computes `quorumSize := total/2 + 1` (`internal/cluster/simulate.go:421`) over the voters, and sets `survives := remainingReachable >= quorumSize` (`internal/cluster/simulate.go:422`). There is a note about the single-voter case at `internal/cluster/simulate.go:430-431`.

The arithmetic is therefore `floor(V/2) + 1` over voters, with unknown reachability never counted in the survivor's favour. That is the same formula Raft itself uses for a majority, expressed here in the codebase's own vocabulary and with the codebase's own treatment of unknowns. The protocol's membership preconditions are stated in terms of it, and it is already the rule the restart planner obeys.

`internal/restartplan/quorum.go:143` is `EvaluateQuorumSafety`, and its doc comment (`internal/restartplan/quorum.go:113-142`) says that it deliberately does not reimplement the arithmetic, delegating to `internal/cluster.ComputeQuorumImpact` instead, so that unknown-reachability handling, non-voter targets, and single-voter clusters apply unchanged. Its precedence order matters for the protocol as well: a failed probe read is Unknown and cannot be overridden by `Force` (`internal/restartplan/quorum.go:129`), a non-voter target is Allow, and only then does the quorum arithmetic apply, with an additional block-by-default on restarting the leader.

`internal/manager/raftdquorum.go:202` is `raftdQuorumFact`, which is how the manager gathers the same facts from a live raftd. It fails the probe closed when the membership read fails (`internal/manager/raftdquorum.go:231`), when `leader_id` is empty (`internal/manager/raftdquorum.go:244`), when the target is not in the membership (`internal/manager/raftdquorum.go:286`), and when the probe read itself fails (`internal/manager/raftdquorum.go:305`).

None of this is a consensus mechanism. It is evidence collection, done carefully, at a point in time, and it decides whether an operator may proceed with a planned change. It never compares two nodes' views of the state to decide whether they agree, and it never compares two nodes' claims to leadership against a shared epoch. The Colony Protocol layer sits above all of this and supplies the missing shared values that make a comparison possible.

### What "acceptance" means in this protocol

Because the colony-level protocol is about acceptance rather than about replication, it is worth being explicit about what it decides.

It decides:

1. Whether a node may be admitted into this colony (Colony ID, CA fingerprint, epoch).
2. Whether a membership change may be committed now (quorum arithmetic against the new configuration, and the joining node's catch-up).
3. Whether a peer may be trusted as a voter at this moment (epoch comparison, digest comparison, quorum reachability).
4. Whether a write that outlives leadership may complete (fencing token).

It does not decide anything about log replication, election, or commit safety. Those remain HashiCorp Raft's job, and HashiCorp Raft does them well. The reason the colony-level protocol needs its own rules is that the colony-level questions are not consensus questions: two independent Raft clusters can each be perfectly internally consistent and still be mutually contradictory, and Raft says nothing about that case because from its point of view they are two different clusters, which they are.

### Membership change with explicit quorum arithmetic

The quorum rule is `floor(V/2) + 1` over voters. This is the same formula the code already uses: `internal/cluster/simulate.go:421` computes `quorumSize := total/2 + 1`, and `internal/cluster/simulate.go:422` decides survival from confirmed-reachable remaining voters only.

The protocol requires that this arithmetic be evaluated at the moment of every membership change, and specifically that it be evaluated against the **new** configuration, not the old one.

**Adding a voter.** The hazard is that the new configuration's quorum may exceed the set that is currently reachable. Consider a two-voter colony A and B, both up. A third voter C is added. Under the old configuration the quorum was 2, so the colony needed both A and B. Under the new configuration the quorum is 2 as well (`floor(3/2)+1 = 2`), so the colony needs two of A, B, C. Now C is added, and C comes up lagging. Nothing is broken yet, but the margin has gone from zero tolerance to one.

The hard case is the one-voter to two-voter transition. A single-voter colony has quorum 1, and it tolerates the loss of its only voter by ending the colony. Add a second voter and the quorum becomes 2, so the new configuration cannot tolerate the loss of either node. The old configuration could tolerate losing nothing, but it could also *keep working alone*; the new one cannot work with either node missing. The transition therefore takes a colony that was self-sufficient and makes it dependent, and the moment of transition is the moment where the reachable set must be rechecked.

The arithmetic for the three transitions the protocol must handle explicitly:

- **1 to 2 voters.** Old quorum 1, new quorum 2. Before the change the colony needs 1 node; after it needs 2. The change is safe only if both nodes are reachable and the new node has caught up to the leader's log before it is counted as a voter. A colony that adds its second voter during a partial outage must refuse the change rather than make itself unreachable.
- **2 to 3 voters.** Old quorum 2, new quorum 2. The quorum count does not change, so the change does not increase the number of nodes that must be simultaneously reachable. What changes is the margin: with 2 voters, losing one is fatal; with 3, losing one leaves exactly quorum. The change is safe if the third node is reachable and caught up.
- **3 to 4 voters.** Old quorum 2, new quorum 3. The quorum increases. A colony of 3 voters tolerates losing one; a colony of 4 voters with quorum 3 tolerates losing one only if the remaining three are all up. This is the first transition where adding a node strictly reduces tolerance.

HashiCorp Raft v1.8.0 does not solve the underlying problem with a joint configuration. `raft.Server` carries only `Suffrage`, `ID`, and `Address` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/configuration.go:61-67`), and a configuration change appends a single `LogConfiguration` entry describing a single configuration (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/raft.go:1204`, `~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/raft.go:1229-1234`). There is no joint consensus in the library, and a search of its non-test sources for `joint` returns nothing.

This is a real hazard and the current code does not guard it. `internal/raft/peers.go:60` deliberately refuses to record voter status at pin time and defers membership to `SetTrustedPeerVoter` after `AddVoter`, and the `raftd.proto` membership RPCs (`api/internalpb/raftd.proto:25`, `api/internalpb/raftd.proto:29`) are thin wrappers over the library's `AddVoter` and `RemoveServer`. Nothing in that path evaluates the new configuration's quorum against the currently reachable set before committing the change.

The Colony Protocol therefore adds an explicit precondition to every membership change, evaluated by the leader before it commits:

1. Compute the prospective configuration and its quorum.
2. Compute the currently reachable set, crediting unknown reachability as not reachable, consistent with `internal/cluster/simulate.go:382-390` and `internal/restartplan/quorum.go:113-142`.
3. Refuse the change if the resulting configuration could not reach quorum with the nodes that are actually up right now, or if the joining node is not caught up.
4. Refuse the change if the change would leave a single-voter colony, which is a teardown and not a membership change (see below).

**Removing a voter.** Removal is two-phase: drain, then remove. The drain phase takes the node out of the data path and stops it accepting writes, but leaves it in the voter set so the quorum arithmetic is unchanged while it is being shut down. The remove phase calls the library's `RemoveServer` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/api.go:1028`) once the node is drained and the remaining voters are confirmed to hold quorum without it. The order matters: removing the voter first and draining afterwards creates a window in which the node still believes it is a voter of a configuration it is no longer part of.

**Single-voter colonies.** A one-voter colony has quorum 1 and is a complete, valid colony. See the next subsection.


### The quorum arithmetic in full

The protocol evaluates the following, at the moment of every membership change, over the voters of the **resulting** configuration:

```
quorum(newConfig) = floor(V_new / 2) + 1
reachable(newConfig) = number of voters confirmed reachable right now
safe_to_commit = reachable(newConfig) >= quorum(newConfig)
```

Unknown reachability is never counted as reachable, which is the rule `internal/cluster/simulate.go:382-390` already sets and `internal/restartplan/quorum.go:113-142` already relies on. The joining node's catch-up is a separate precondition: a node that is reachable but whose log lags the leader is not a usable voter, because counting it as a voter without counting it as a reproducer of the state is how a quorum is satisfied without the state being present.

The worked transitions, using the formula directly:

| Transition | Old quorum | New quorum | Nodes the new configuration needs simultaneously up | Nodes the old configuration needed |
| --- | --- | --- | --- | --- |
| 1 to 2 voters | 1 | 2 | 2 of 2 | 1 of 1 |
| 2 to 3 voters | 2 | 2 | 2 of 3 | 2 of 2 |
| 3 to 4 voters | 2 | 3 | 3 of 4 | 2 of 3 |

Reading the table:

- **1 to 2** is the sharp one. Before the change, the colony needs one node up. After it, it needs both. A colony that adds its second voter while one of the two is unreachable moves from "working" to "not working", and the change is what does it. The precondition must therefore be evaluated against the new configuration, and the change must be refused if the second node is not both reachable and caught up. This is the case that a naive "the old quorum still holds" check would get wrong.
- **2 to 3** does not change the quorum count, so the change does not increase how many nodes must be simultaneously up. It changes the margin: with two voters, losing one is fatal; with three, losing one leaves exactly quorum. The precondition is the same shape, and the new node must be caught up.
- **3 to 4** raises the quorum from 2 to 3. The colony of three voters tolerated losing one; the colony of four voters tolerates losing one only if the remaining three are all up, so the tolerance in absolute terms goes down while the redundancy in relative terms goes up. The precondition is the same, evaluated against the new configuration.

The hazard here is real and the current code does not guard it. `internal/raft/server.go` exposes `AddVoter` and `RemoveServer` as leader-only RPCs (`api/internalpb/raftd.proto:25`, `api/internalpb/raftd.proto:29`), and the quorum arithmetic in `internal/cluster/simulate.go` is used to plan restarts, not to gate membership commits. The protocol's rule is that the arithmetic becomes a precondition on the membership command itself.

There is a further wrinkle that HashiCorp Raft v1.8.0 does not solve for us, and the protocol has to work around rather than around-away. The classic joint-consensus approach changes the configuration through two phases: first to a configuration listing both the old and the new voters, then to the new one. A joint configuration can tolerate a configuration change during a partition, because the quorum is required from both sets. The library's `Server` struct has only `Suffrage`, `ID`, and `Address` (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/configuration.go:61-67`), a membership change appends one `LogConfiguration` describing one configuration (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/raft.go:1204`, `~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/raft.go:1229-1234`), and there is no joint configuration to commit. The library's own approach is to reject a configuration change when it cannot be committed safely in the current configuration, which means a 1 to 2 transition requires the current single voter to be up and to be able to commit.

The protocol's mitigation is to require, before committing any membership change, that the resulting configuration can reach quorum with the nodes confirmed up right now, and that the joining node is caught up. This does not reproduce joint consensus and does not claim to. It converts the risk from "the change is committed and the colony is then unreachable" into "the change is refused and the operator is told why", which is the difference between a self-inflicted outage and a refused action.

### Single-node permanence

ADR-0091 makes single-node a permanent, first-class deployment, not a bootstrap phase. The Colony Protocol honours that literally.

A one-voter colony is valid. It is not a degraded colony, not a colony waiting for a second member, and not misconfiguration. Every rule in this protocol must have a defined answer for the one-voter case, and the answer is "allowed" wherever the colony remains internally consistent.

Concretely:

- Quorum is 1, and one reachable node is quorum (`internal/cluster/simulate.go:430-431` already says that losing the only voter ends the colony's ability to reach quorum, which is a statement about the consequence, not a complaint about the configuration).
- The Colony ID, the epoch, and the fencing token all apply, because a single node can still be a stale restore of an older generation of itself.
- No membership operation may be required or implied for the colony to function.
- Health and Why Not reporting must not present a one-voter colony as a problem to fix.

The one thing a single-node colony cannot do is survive the loss of its only node, and no protocol layer can change that. What the protocol layer can do, and must, is make sure that a single node which comes back with a different Colony ID or a lower epoch does not silently re-form a colony and call itself leader.

### Node removal

Removal of a node from a colony is a two-phase operation.

Phase one, **drain**: the node is removed from the data path. It stops accepting new writes, it is drained of in-flight work, and it is marked as draining in the replicated state so that the leader stops selecting it for placement and the UI reports it as draining rather than as a member. It remains a voter.

Phase two, **remove**: the node is removed from the voter set through the library's `RemoveServer`, and its peer trust record and drain marker are cleared by replicated commands.

The reason for the two phases is quorum. Removing a voter changes the quorum immediately, and the safe condition for the removal depends on the reachable set at that moment. Draining first means the operator can verify the remaining voters are healthy and reachable while the configuration is still the old one, and only then change it.

**Removing the last voter is not a membership change.** Raft cannot safely remove a node from a cluster of size 1: doing so leaves a configuration with no voters, which cannot elect a leader and cannot commit anything. The protocol therefore defines the removal of the last voter as a **colony teardown**, which is a different operation with different semantics and different confirmation. A teardown is irreversible in the consensus sense: it ends the colony, and the state that the colony replicated is gone unless it was exported first (ADR-0051). The protocol requires that a teardown be an explicit, confirmed, single-step operation that no membership flow can reach, and that the confirmation name the fact that the colony ends.

This is not an argument against single-node permanence. It is the distinction between "a one-node colony is a normal, permanent deployment" and "removing the one node is a teardown, not a membership edit."

### Divergence detection through the ADR-0143 state digest

The protocol uses the existing digest rather than inventing a new one.

The rule:

1. Voters compare their digests periodically, and on demand.
2. A comparison at **equal applied_index** with **differing digests** is a real disagreement. The colony is in an inconsistent state.
3. A comparison at **differing applied_index** is inconclusive. It says only that the samples were taken at different points in the log, which is normal during catch-up.
4. A real disagreement **fails closed**. The colony refuses membership changes, refuses to hand leadership to a node whose state disagrees, and surfaces the disagreement to the operator. It is never auto-resolved.

Auto-resolution is refused for a specific reason: the digest does not say which side is wrong. The doc comment on `StateDigest` (`internal/raft/fsm.go:2127-2142`) says so explicitly. A system that picked a winner automatically would be picking a side in a disagreement it cannot adjudicate, and would be doing so without evidence.

One detail constrains the implementation. The digest deliberately excludes `applied_index` (`api/internalpb/raftd.proto:272-278`), so a bare digest comparison carries no index. The protocol therefore pairs the digest with the index in every comparison: either the comparison carries both values, or the digest is extended to a richer form that covers the index. The digest must remain a digest of state, and the index must remain a separate observation that is always compared alongside it. Either approach is acceptable; what is not acceptable is comparing digests alone and calling a mismatch a disagreement.

The comparison result is `Unknown` when a voter's status cannot be read. ADR-0122's rule applies: a missing observation stays Unknown and never becomes healthy.

### Work queue semantics

ADR-0149 is Unconfirmed and unimplemented. There is no proto field, no FSM command, and no package for it. It is therefore not settled precedent, and this ADR does not treat it as one.

If a work queue is built later, it inherits these constraints, and they are constraints on the protocol layer rather than on the queue's implementation:

1. **One consumer at a time.** A queue item is claimed by exactly one consumer for the duration of its attempt.
2. **Bounded attempts.** Each item has a fixed attempt count. There is no unbounded retry.
3. **No I/O inside FSM apply.** The FSM's apply path must not open a socket, must not run a retry timer, and must not perform network I/O of any kind. `internal/raft/fsm.go:142` takes the FSM lock, and `internal/raft/fsm.go:151` recomputes the digest under it; a blocking network call there would stall every other command on the colony and would make the digest a view of a state that is mid-flight.
4. **A failed enqueue is a visible drop, not a silent retry.** If the queue cannot accept an item, the drop is recorded and reported. It is not retried inside apply and it is not swallowed.

The reason for rule 3 in particular is that the FSM is the one place in this system where blocking is structurally unsafe. The digest recompute at `internal/raft/fsm.go:2159-2160` serialises the entire state machine on every applied command, which is a deliberate trade recorded at `internal/raft/fsm.go:2149-2158`. A network call in that path would make a slow peer a colony-wide stall.

### Leader step-aside and preferred leader

The protocol recognizes two existing mechanisms and constrains them.

**Step-aside** is used for controlled operations (ADR-0145, ADR-0146) and for quorum-safe restart (ADR-0125). `internal/raft/stepaside.go:82` performs an untargeted `LeadershipTransfer()` and then verifies that a different node is leading (`internal/raft/stepaside.go:148`, `internal/raft/stepaside.go:119`). If the transfer does not complete, the original leader reports that it is still leading and is not safe to restart (`internal/raft/stepaside.go:109-117`). That refusal is the correct behaviour and the protocol keeps it.

**Preferred leader** is a bias, never an override. ADR-0150 is Accepted and `internal/raft/preferredleader.go:132` is `PreferredLeaderTransfer`, which calls the library's targeted `LeadershipTransferToServer` (`internal/raft/preferredleader.go:218`), and that call declines on its own when the target is not a voter or is not caught up. The protocol's rule is stated plainly: **no mechanism in the Colony Protocol may ever force leadership.** Raft safety forbids it. A protocol layer that could install a leader by fiat would be a protocol layer that could break consensus, and this ADR does not propose one.

One concrete limitation to record. The preferred-leader cooldown is one minute (`internal/raft/preferredleader.go:38`) and its state is held per `Node` process in memory, deliberately not persisted (`internal/raft/preferredleader.go:96-98`). That means the cooldown is not replicated and is not shared across restarts. Two nodes that have been running for different lengths of time hold different cooldowns, and a restart clears the local one. This is the conservative direction, in that it permits an earlier transfer than intended rather than a later one, and the ADR records it rather than proposing a change: making the cooldown replicated would put a wall-clock timer into replicated state, which is exactly the kind of thing that does not survive a partition cleanly.


### Reading the failure matrix

The matrix below is the protocol's contract with the operator. Three things are asserted for every row: what the system can detect, what it refuses to do, and what the operator is required to do about it. The middle column is the important one, because every entry in it is a refusal that the current code does not make.

Two rows deserve a note before the table.

The **network partition** row is the one where the protocol is mostly delegating. Raft's election timeout and leader lease handle the mechanics of a partitioned side losing leadership, and the protocol adds nothing there. What the protocol adds is the refusal: a partitioned node that has lost its lease may not accept writes, and a membership change may not be committed while the reachable set is smaller than the quorum of the resulting configuration.

The **attempted split-brain** row is the one the whole protocol exists for, and it is worth being precise about the detection path. Two raftd instances that were bootstrapped independently have different Colony IDs. They will disagree at the first point where they try to talk to each other, which for the peer transport is the TLS handshake and for the inter-node RPCs is the first request envelope. If they never talk to each other, then the disagreement is not observable by them, and the observable symptom is elsewhere: two nodes both claiming leadership over the same colony name, which the colony-level status surface will show because the Colony ID is a field rather than an inference. Detection in the case where the two instances are fully partitioned from each other therefore depends on a third party comparing them, or on an operator comparing them, which is why the protocol puts the Colony ID in `StatusResponse` (`api/internalpb/raftd.proto:251`) rather than leaving it only in the FSM.

The **disk loss** row is the one with the sharpest operational edge, because the current behaviour on disk loss is exactly the behaviour the protocol forbids: an empty data directory self-bootstraps a fresh single-voter cluster (`cmd/raftd/main.go:479`, `cmd/raftd/main.go:490-491`), and the reset flow does the same thing deliberately (`cmd/raftd/main.go:302`, `cmd/raftd/main.go:321`). The protocol does not forbid an operator from creating a new colony. It forbids a node from creating one silently, and it requires the operator who does it on purpose to say so.

### Failure matrix

Each row states what the protocol detects, what it refuses to do, and what it requires of the operator.

| Situation | Detected by | Refused | Operator must |
| --- | --- | --- | --- |
| Network partition | Raft's own election timeout; the partitioned side loses leadership | Any write accepted by a partitioned node claiming leadership, once the lease lapses; any membership change during a partition | Re-establish connectivity, or accept that the colony is unavailable. Do not "fix" a partition by bootstrapping a second colony. |
| Disk loss on a voter | Missing or unreadable local state; the node returns to an empty data directory | Self-bootstrap into a new colony. The startup guard refuses to bootstrap a fresh single-voter cluster when a sidecar shows the node was previously part of a colony, unless an explicit re-creation is requested | Either rebuild the node by joining the existing colony, or request a colony re-creation deliberately, understanding that it is a new colony. |
| Stale clock | Not directly detectable by this protocol; Raft uses timeouts, not clock agreement | Nothing on clock grounds. The protocol does not treat a stale clock as identity evidence | Keep clocks synchronised. The protocol does not pretend to solve clock skew, and a from-scratch engine would not solve it either without a clock-synchronisation assumption stated up front. |
| Attempted split-brain (two bootstrapped instances) | Colony ID mismatch on the handshake or on any inter-node RPC; epoch mismatch; digest disagreement at equal index | The second instance refuses peers from the first colony; the first refuses the second. Neither will serve as a member of the other's colony | Determine which instance is the real colony, and rejoin the other properly. |
| Partially completed join | The pending join record's status and the presence or absence of a trusted-peer pin (`internal/raft/peers.go:60`, `internal/raft/server.go:174`) | A second attempt at the same join while one is outstanding; adding a voter without the preceding pin | Complete or abandon the outstanding join through the normal flow. Do not hand-edit replicated state. |
| Node rejoins with a stale epoch | Epoch comparison at admission and on every RPC | Admission of the stale node; any leadership claim by it | Rebuild the node from the current colony state. A stale-epoch node is not a member and cannot be made one by restarting it. |

The disk-loss row is the one that requires the most care from the operator, because the current behaviour (self-bootstrap a fresh single-node cluster) is exactly what makes the failure invisible today, and it is exactly what `cmd/raftd/main.go:302` performs for an operator who has used the reset path.

### Migration from the existing on-disk layout

The existing layout is `<DataDir>/raft.db` (one BoltDB file serving as both log store and stable store), `<DataDir>/snapshots/<term>-<index>-<millisecond>/meta.json` (HashiCorp snapshot metadata JSON), and `<DataDir>/snapshots/<id>/state.bin` (the serialized FSM payload with CRC validation, `~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/file_snapshot.go:28`).

**Establishing a Colony ID for a colony that already exists.** A Colony ID cannot be derived from the existing state, because nothing in the existing state encodes one. There are two options:

- **Generate and replicate it.** The leader generates a UUIDv4 and commits it through a replicated membership-class command, so it becomes part of the FSM and therefore part of every voter's state and every snapshot taken afterwards. Nodes that are up adopt it immediately. A node that is down adopts it when it rejoins and receives the state. This is the recommended path.
- **Derive it deterministically from the CA fingerprint.** Deriving the Colony ID as a hash of the Colony CA fingerprint gives a value that is stable across the colony and computable offline on any node that has the CA file, with no replicated command needed. It has a serious drawback: it makes the Colony ID and the CA fingerprint the same fact, so the two-anchor independence described above is lost, and a CA rotation would change the Colony ID. The protocol therefore permits the derivation as a **bootstrap convenience for an existing colony with no CA yet**, and requires that a colony which later adopts a Colony CA be assigned a distinct, randomly generated Colony ID.

**The epoch.** The epoch must be initialised to a value consistent with the existing state, and the consistent value is 1. It is not derived from anything, because there is nothing in the existing state from which a generation count could be recovered. An epoch of 1 for an existing colony means every node that was part of that colony since the beginning shares epoch 1, and any node that later rejoins with an epoch below 1 is refused. There is no way to distinguish "this node has been here since epoch 1" from "this node is a fresh install claiming epoch 1" using the epoch alone; the Colony ID and the sidecar together are what make that distinction, which is one more reason both values exist.

**Restore is a colony re-creation, not a member rejoin.** ADR-0051's restore seeds an offline snapshot into an empty node, single-node for v1, with a whole-state point-in-time replacement (`internal/raft/seed.go:54`, `internal/raft/seed.go:94-101`). It preserves the serialized FSM state and nothing else: not the log, not the stable vote keys, not the membership history, not the leader identity, not the snapshot metadata, not the transport, configuration, or TLS material.

That means a restored node is a brand new one-node colony with a brand new identity, which is exactly the split-leader hazard described in the Context section. The protocol's explicit recommendation is therefore:

**A restore must be recognized as a colony re-creation, not a member rejoin.** Concretely:

1. A restore into an empty node produces a colony with a **new Colony ID** and **epoch 1**, regardless of what the restored FSM payload says about identity, because the restored payload cannot be trusted to carry a colony identity that the surrounding world agrees with.
2. Restoring into a node that is currently a member of a live colony is **refused by default**. It is a destructive operation on a live colony's identity, and it must not be reachable as a side effect.
3. To rejoin the existing colony with restored data, the operator performs the restore into a separate directory and then joins the colony through the normal, authorized join flow, so that the joining node's identity is negotiated rather than assumed.
4. The restored state's own contents are preserved as data. The restored *identity* is not.

This is a change to how ADR-0051 restore behaves in a multi-node world, and it is the sharpest edge in this ADR. It is stated here rather than deferred, because a restore that silently creates a competing colony is the same failure the whole protocol layer exists to prevent.

### Alternatives considered

**(a) Keep HashiCorp Raft and add only a colony-identity guard.** Effort: small. One field in the FSM, one in the status response, one in the join payload, and a startup check. Risk: low. What it buys: the ability to detect that two raftd processes are not the same colony, which is the mechanism behind the split-leader hazard. What it does not fix: it does not distinguish two generations of the same colony, so a stale restore of a colony that once existed still re-forms as a new instance of that colony; it does not give membership changes an explicit quorum precondition; it does not detect state divergence; it does not fence writes across a leadership change. This is the minimum viable fix and it is a strict subset of the full layer.

**(b) Keep HashiCorp Raft and add the full Colony Protocol layer described here. Recommended.** Effort: substantial but staged, and the first stage is the same work as (a). Risk: moderate, concentrated in the membership-precondition logic and the migration path. What it buys: colony identity with two independent anchors, a colony-wide epoch, membership changes gated on quorum arithmetic, explicit divergence detection using a digest that already exists, fencing tokens for writes that outlive leadership, and a clear definition of what a restore is. What it does not fix: it does not make Raft's safety easier to prove, and it does not address clock skew.

**(c) Adopt a different existing library.** Effort: large, and mostly mechanical: the FSM and the RPC surfaces sit above the library, but the storage layout, snapshot format, membership API, and leadership-transfer semantics all sit below it. Risk: high, because every one of those is load-bearing and the repository has live colonies running on the current one. What it buys: nothing that has been asked for in this repository. What it does not fix: the identity gap. A different consensus library would also have no colony identity, because a colony identity is a domain concept, not a consensus concept. Switching libraries and then still having to design an identity layer is strictly worse than designing the identity layer on the library that is already here. The ecosystem options are also limited: the mature Go options are this one and a smaller set of libraries with narrower feature sets, none of which offers a Colony identity concept either.

**(d) Write a purpose-built consensus engine from scratch.** Effort: very large. Risk: very high. What it buys: control over the log format, the snapshot format, and the transport's framing. What it does not fix: the identity gap, which would still need designing, and which is the same amount of work either way.

**What would have to be true to justify (d).** A from-scratch engine carries a safety-proof burden that cannot be discharged by unit tests. It would need, at minimum:

1. A formal or semi-formal safety argument covering the election and log-replication invariants, including the cases where that library does not have to think about: leader lease expiry, pre-vote interaction with membership changes, and snapshot-and-log interaction under partial failure.
2. Jepsen-style linearizability and partition testing. This repo has no such apparatus today. Building it is itself a large project: a history-recording client, an adversarial node controller, a nemesis covering process kill, network partition, clock skew, and disk loss, and a linearizability checker.
3. Fault injection for disk loss, clock skew, and message reordering, on top of the partition testing.

That apparatus does not exist in this repo, and building it would be a large piece of work in its own right, before any consensus code was written.

**The from-scratch option buys nothing that the identity and epoch layer does not buy**, unless the specific thing wanted is control over the log format, the snapshot format, or the transport's framing. None of those has been named as a requirement in this repository.

**Stated plainly, because it is the point of this ADR:** if the goal is to escape HashiCorp Raft's per-node identity model, then the identity layer alone achieves that, without re-proving consensus safety. HashiCorp Raft's per-node identity model is not the problem. The absence of any colony identity is the problem. Replacing the engine does not address the absence of colony identity, and it adds a safety burden that the identity layer does not.

There is one further reason not to choose (d) on the merits of a from-scratch protocol: the design work for a colony protocol has to happen regardless. The identity, the epoch, the membership rules, the divergence rules, the fencing, and the failure matrix are all required whether the layer sits on HashiCorp Raft or on a new engine. Designing them on top of a proven engine and then, if it ever becomes necessary, porting the layer to a different engine is a strictly smaller and better-understood body of work than designing the protocol and the engine at the same time.

## Consequences

### This is a design proposal, and nothing in it is implemented

No code in this repository implements any part of the Colony Protocol layer described above. There is no Colony ID in the FSM, no epoch, no fencing token on ordinary writes, no membership precondition check, no periodic digest comparison, and no startup guard against a stale node self-bootstrapping. Every statement under "Decision" describes work that has to be done, not work that has been done.

The reason it is written this way is that the design work has to be settled before any of it is coded. The membership arithmetic, the two identity anchors, the interaction between the epoch and the local Raft term, and the redefinition of restore as a colony re-creation all have consequences that are awkward to change once they are on disk and in the wire format.

### What gets better

**The split-leader hazard becomes detectable.** A second raftd that bootstraps itself into a colony it does not belong to will fail its first inter-node exchange with the real colony, because it presents a different Colony ID, and it will be refused by the real colony for the same reason. Today the two colonies are mutually invisible and both can claim leadership; after this layer they are mutually exclusive, and the operator is told which instance is which.

**Two generations of the same colony become distinguishable.** A colony that was wiped and rebuilt has a new Colony ID and a new epoch. A stale node from the old generation is refused admission rather than admitted and then argued with.

**Membership changes become safe by construction rather than by luck.** The 1 to 2 voter transition, where the new configuration's quorum exceeds the old one's reachability, is currently unguarded. After this layer it is refused unless the preconditions are met. The arithmetic is not new (`internal/cluster/simulate.go:421` already computes it); using it as a gate on a membership command is new.

**Divergence becomes observable.** The digest already exists (`internal/raft/fsm.go:2143`) and is already exposed (`api/internalpb/raftd.proto:279`). Comparing it across voters at equal applied indexes turns an invisible state disagreement into a reported one, and the protocol refuses to auto-resolve it, because the digest cannot say which side is wrong.

**Writes that outlive a leadership change become fenceable.** The `ColonyUpdateFence` shape (`api/internalpb/state.proto:1798`) already exists for one operation. Extending the same shape to operations that outlive leadership closes a real gap between what Raft's log replication guarantees and what the rest of the system assumes.

**The join flow gains a real identity check.** Today the join payload carries no colony identity at all (`api/rpc/manager.proto:3868`, `api/internalpb/state.proto:1090`), so the mutual-authorization machinery that ADR-0083, ADR-0097, and ADR-0147 built authorizes a join into a colony without ever naming the colony. With the Colony ID in the payload, the authorization is of a specific colony rather than of an unnamed one.

**Restore stops being a trap.** Today restore is a supported operation that produces a competing colony without saying so. After this layer it is explicitly a colony re-creation, it is refused by default against a live colony, and rejoining is done through the normal join flow.

### What gets worse

**More fields, more places to check.** A Colony ID, an epoch, and eventually a fencing token have to appear in the FSM state, in `StatusResponse`, in the join request, in the pending join record, and in the inter-node RPC envelope. Every new field is a new thing that can be wrong, and every check is a new place for a bug.

**The existing estate needs a migration.** Colonies that exist today have no Colony ID and no epoch. Establishing them requires a replicated command and a rollout, and nodes that are down during the rollout need to adopt the values when they return.

**Restore changes meaning.** An operator who used to restore a node and expect it back in its colony will, after this change, have restored a new colony and must join. This is a real behavioural change to an accepted ADR (ADR-0051), and it will surprise someone the first time. It is stated in "Decisions for Glen" below as an overridable default.

**Some operator actions get slower.** A membership change now runs precondition checks before it commits, so the window in which an operator can act is narrower. This is the point, but it is a change in feel.

**The protocol layer adds a place for divergence to hide.** The state the FSM holds is now "the state of colony X at epoch N". A node that fails to update one of those values can produce a mismatch between what it believes and what it is. The design keeps the state minimal and the checks explicit to limit that risk, but it does not eliminate it.


### The staging order, and why it starts where it does

The staging order in "Consequences" is not arbitrary, and the reasoning is worth stating here because it explains the shape of the whole proposal.

Stage 1 is the smallest change that closes the verified gap. The verified gap is that there is no colony identity anywhere in configuration, in status, or in the join payload. Closing it needs exactly four things:

1. A Colony ID in the replicated FSM, so that every voter holds the same value and every snapshot carries it.
2. A Colony ID in `StatusResponse`, so that a node can state which colony it is in without an inference.
3. A Colony ID in the join payload, so that a join is authorized into a named colony rather than into an unnamed one.
4. A startup guard that refuses to self-bootstrap when the node's own data directory says it was previously part of a colony and no join was requested.

The startup guard is the part that has no precedent in the tree and the part that does the most work. It is the piece that turns ADR-0038's tiered reset (`cmd/raftd/main.go:302`) from a silently dangerous operation into an explicitly declared one, because after the guard, a reset is either a declared colony re-creation or it is refused.

The later stages are ordered by dependency. Epoch depends on Colony ID, because the pair is what distinguishes "a different colony" from "another generation of this colony". The sidecar depends on both, because the sidecar is what lets a node with an empty data directory state its identity before it has any replicated state to read. Restore semantics depend on the sidecar, because "is this restore a re-creation or a rejoin" is answered by comparing the restored node's identity against the colony it claims to belong to. Membership preconditions depend on the epoch, because a membership change is an acceptance decision and acceptance needs a generation. Divergence detection depends on nothing new, since the digest already exists, but it is placed late because a false positive in it would be more disruptive than the absence of it. Fencing tokens come last because they touch the largest number of call sites.

### What this ADR does not ask Glen to decide right now

The decision this ADR asks for is the architectural one: keep HashiCorp Raft as the log-replication engine, and design the colony-level protocol as a layer above it, rather than replacing the engine. That decision is what everything else in this document is built on, and it is what the "Alternatives considered" section argues for.

The individual defaults listed under "Decisions for Glen" are recorded so that they are not made implicitly, during implementation, by whoever writes the code. They are reversible now and expensive to reverse later, and each of them has a stated alternative so that the choice is deliberate rather than accidental.

### Implementation cost and staging order

The work is staged, and the first stage is deliberately the smallest change that closes the verified gap.

**Stage 1: Colony ID only.** Add the Colony ID to the FSM as a replicated value, expose it in `StatusResponse`, add it to the join request and to the pending join record, and add a startup guard that refuses to self-bootstrap when the local data directory holds a sidecar showing the node was previously part of a colony and no join was requested. This stage is the same work as alternative (a) above. It closes the split-leader mechanism that is verified to exist, on its own, without touching membership rules or the epoch.

**Stage 2: Epoch.** Add the replicated epoch, the rules for incrementing it, and the refusal of stale-epoch nodes at admission and on every RPC.

**Stage 3: Sidecar and restore semantics.** Write and read the data-directory sidecar, make restore assign a new Colony ID and epoch 1, refuse restore into a live member by default, and make the rejoin path go through the normal join flow.

**Stage 4: Membership preconditions.** Evaluate the quorum arithmetic against the new configuration at every membership change, refuse the 1 to 2 transition when the reachable set cannot meet the new quorum, and implement drain-then-remove.

**Stage 5: Divergence detection.** Compare digests across voters at equal applied indexes, on a schedule and on demand, and fail closed on a real disagreement.

**Stage 6: Fencing tokens.** Apply the `ColonyUpdateFence` shape to operations that can outlive a leadership change.

Each stage is independently shippable and independently valuable. Stage 1 is the one that matters most, because it closes a mechanism that is reachable through a documented operation today.

### What is explicitly out of scope

- **A from-scratch consensus engine.** Not proposed here, and not needed for any of the above. The reasoning and the burden are set out under "Alternatives considered".
- **Joint consensus inside HashiCorp Raft.** The library at v1.8.0 does not implement it (`~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/configuration.go:61-67`, `~/go/pkg/mod/github.com/hashicorp/raft@v1.8.0/raft.go:1204`), and adding it would mean forking or replacing the library, which is alternative (c) or (d) by another name. The quorum precondition rules above are the mitigation.
- **Clock skew.** The protocol does not detect a stale clock and does not treat one as identity evidence. A from-scratch engine would have the same problem unless it stated a clock-synchronisation assumption up front.
- **Implementing the work queue.** ADR-0149 is Unconfirmed and has no code. The constraints above are recorded so that a future implementation inherits them; nothing is being built.
- **Fixing ADR-0147's status text.** The discrepancy is recorded in "Status qualifications" and left alone.
- **Fixing ADR numbering.** The four 0150 files, the two 0152 files, the gaps at 0126 and 0153, and the filename prefix collision between `docs/adr/0150-dedicated-vlan90-switch.md` and `0120-config-rationale-history.md` are recorded as a side note and left alone. The `ADR-0120` title identifier that previously collided in that file was corrected to `ADR-0150` in commit `d7ad9f7`; the filename prefix collision remains.
- **Implementing `GetColonyDiskSizeLocal`.** It is declared (`api/internalpb/raftd.proto:147`) and unimplemented, returning `codes.Unimplemented` through the embedded `internalpb.UnimplementedRaftInternalServer` (`internal/raft/server.go:21`). The Colony Protocol will need a status surface, and this RPC is a candidate for it, but that is implementation work in the staged plan, not part of this ADR.

## Decisions for Glen

Each of the following is a choice this proposal makes by default. Each can be overridden before any code is written, and the override is recorded by amending this ADR.

1. **Colony ID for an existing colony: generate and replicate, not derive.** The default is a random UUIDv4 generated by the leader and committed through a replicated command, so that the Colony ID stays independent of the CA fingerprint. The alternative, deriving it as a hash of the CA fingerprint, is offered only as a bootstrap convenience for a colony that has no CA yet. If Glen prefers derivation as the general rule, the two-anchor independence described above is lost and the CA rotation problem has to be solved some other way.

2. **Restore always produces a new identity.** The default is that a restore yields a new Colony ID and epoch 1, and that restoring into a live member is refused by default. The alternative is to allow restore into a live member with a documented override. The default is safer; the override is more convenient.

3. **Epoch initialised to 1 for existing colonies.** There is nothing in the existing state from which a generation count could be recovered, so 1 is the only consistent starting value. The alternative would be to derive an epoch from a hash of the existing state, which is stable across nodes but not monotonic in any meaningful sense, and would not order a genuine reset against an ordinary restore.

4. **Colony ID and epoch travel in existing messages, not a new envelope.** The default is to add the fields to `StatusResponse`, to the join request, to the pending join record, and to the inter-node RPC envelope as explicit named fields. The alternative is a generic envelope carrying a colony header, which is cleaner to extend but touches every RPC and every caller, including the local-only RPCs that do not leave the machine.

5. **Digest comparison always pairs the index.** The default is to keep the ADR-0143 digest as a pure state digest and to compare it together with the applied index, rather than extending the digest to cover the index. The alternative is a richer digest that folds in the index, which makes a bare digest comparison meaningful but changes the digest's meaning and every consumer of it.

6. **Removal of the last voter is a teardown, named as such.** The default is that the operation is called a teardown, requires its own confirmation, and cannot be reached through a membership flow. The alternative is to expose it as an ordinary removal with a stronger warning, which is easier to reach and easier to get wrong.

7. **Single-voter colonies are reported as normal.** The default is that health and Why Not output does not flag a one-voter colony as a problem to fix, per ADR-0091. The alternative, reporting it as a recommendation, risks training operators to treat the single-node deployment as a misconfiguration, which ADR-0091 says it is not.

8. **The preferred-leader cooldown stays un-replicated.** The default is to leave the one-minute cooldown in memory per process (`internal/raft/preferredleader.go:96-98`) and not to move it into replicated state. The alternative, replicating it, would put a wall-clock timer into the FSM, which does not survive a partition cleanly.

