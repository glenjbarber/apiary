# ADR-0191: Proxmox node discovery and enrollment

## Status

**Proposed.** This record designs Apiary's integration with Proxmox as a
**node discovery and enrollment provider**. It is **not implemented** in
the tree this ADR lands beside: there is no Proxmox client, no Proxmox
configuration, and no Proxmox provider interface in `internal/` today.

Supersedes: None
Superseded by: None
Affected projects: Apiary
Source: Notion record "ADR: Proxmox Node Discovery and Enrollment"
(Proposed, 2026-10-27) and "Todo: Accept Proxmox server requests and join
Proxmox clusters"
Related: ADR-0147 (the two-way peer-authorized Colony join this
integration enrolls into), ADR-0155 (the Colony certificate authority
whose trust model the enrollment inherits), ADR-0164 (deriving the peer
allowlist from Colony membership), ADR-0100 (daemon configuration files,
which is where the Proxmox credentials live)

## Context

### What is being asked for

Apiary needs to discover and manage nodes that belong to Proxmox
environments. The Notion task records two requirements:

- Allow Apiary to accept requests originating from Proxmox servers.
- Support joining Proxmox clusters, including the required discovery,
  authorization, and membership workflow.

Proxmox already provides the mechanisms for managing its own clusters:
membership, configuration, quorum, and node lifecycle. Reimplementing
those inside Apiary would create a second Proxmox-specific control plane,
increase maintenance cost, and risk divergence from upstream behaviour.

The objective is therefore **integration with Proxmox, not
reimplementation of Proxmox**. Apiary needs a narrow integration that
discovers Proxmox clusters and their nodes, establishes node identities,
and enrolls those nodes into Apiary's own management domain — while
keeping the division of responsibility between Proxmox and Apiary
explicit.

### The constraint that shapes everything: Apiary's inventory *is* its raft membership

This is the single most important fact about the design, and it is not
obvious until it is checked against the code.

Apiary has no separate inventory of "known hosts". A Comb is a raft
member. The frontend's cluster overview builds its rows from the
anchor's `Status` response and the member list that comes with it, and
`internal/frontend/cluster_overview.go`'s `knownColonyMember` treats a
node as known **iff** it appears in raft membership
(`status.GetKnownNodeIds()`). The peer allowlist that gates the
unauthenticated join RPCs is itself *derived* from raft membership
(ADR-0164). The per-Comb pages, the monitoring reports, the cell
placement model — all of them address hosts by raft member identity.

So there is no "inventory table" for a Proxmox node to be written into.
A Proxmox node is a **Linux host running QEMU/KVM**, and an Apiary Comb
is a **FreeBSD host running bhyve**. They are not the same kind of thing,
and Apiary cannot manage a Proxmox node as though it were a Comb.

This drives the whole decision below: discovery and enrollment must be
kept **outside** the raft membership, in a separate, explicitly-labelled
record, because putting a Proxmox node into raft membership would be
claiming to be a raft peer and a Colony member.

### What is already true about the Proxmox side

The Proxmox facts this design depends on were verified against upstream
source rather than assumed, because they are load-bearing and because
Proxmox's API has a habit of quietly returning less than it appears to.
Verified against `proxmox/pve-manager` at commit `73ac838`, and
`proxmox/pve-cluster` at commit `a749090`:

**`GET /nodes` is the cluster-wide node index**, and it is
`permissions => { user => 'all' }` — any authenticated user. It returns
one entry per node in `PVE::Cluster::get_nodelist()`, with `node`,
`status` (`unknown`/`online`/`offline`), and optional `cpu`, `maxcpu`,
`mem`, `maxmem`, `uptime`, `level`, and `ssl_fingerprint`.

**`GET /nodes` filters rather than errors.** This is the trap. In
`PVE::API2::Nodes`'s index handler, the per-node stats are extracted
with `PVE::API2Tools::extract_node_stats($node, $members, $rrd,
!$can_audit)` — where `$can_audit` is a check for `Sys.Audit` on
`/nodes/$node`. A user **without** `Sys.Audit` gets the node entry with
its `status` and `type`, but with `cpu`, `maxcpu`, `mem`, `maxmem`,
`uptime` and `maxdisk` **omitted**. The call succeeds. A client that
reads "maxmem missing" as "this host has no memory" is wrong; a client
that reads the absence of a field as an error is also wrong.

**`GET /cluster/status` requires `Sys.Audit` on `/`**
(`permissions => { check => ['perm', '/', ['Sys.Audit']] }`), and it is
the endpoint that carries cluster identity: a `type: 'cluster'` entry
with `name`, `nodes`, `version`, and `quorate`, plus one `type: 'node'`
entry per member with `id`, `name`, `nodeid`, `local`, `online`, and
`ip`. It is `protected => 1`, so it is not exposed through the ticket
system the same way the unauthenticated-looking endpoints are.

**On a standalone node there is no cluster entry at all.** The handler's
else-branch returns a single synthetic entry — `type: 'node'`, `local: 1`,
`online: 1`, `nodeid: 0`, and the node's own name — and **no
`type: 'cluster'` row**. So "is this a cluster or a single node" is not
a question the answer can always answer, and a client that assumes a
`cluster` row exists will mis-handle a standalone Proxmox host. This
matters because a bare-metal Proxmox install is the single most common
deployment there is.

**`GET /cluster/resources` is `permissions => { user => 'all' }`** and
returns cluster-wide resources of type `vm`, `storage`, `node`, and
`sdn`. It is a `type`-filtered inventory, not a node listing, and it
carries `status` per resource.

**`GET /nodes/{node}/status` requires `Sys.Audit` on `/nodes/{node}`**
and is `proxyto => 'node'`, so it reaches the named node. It returns
per-node detail including `uptime`, `idle`, `boot-info`, and — notably
— `boot-info.mode` of `efi` or `legacy-bios`.

**`ssl_fingerprint` is a stable per-node identity anchor.**
`PVE::Cluster::get_node_fingerprint($node)` reads
`/etc/pve/nodes/$node/pve-ssl.pem` (or `pveproxy-ssl.pem` where
present) and returns the certificate fingerprint. This is a per-node
value that does not change when the node is renamed in the UI, and it is
already exposed on `GET /nodes`.

**The node list is cluster-wide, not node-local.** `get_nodelist()`
returns the whole cluster's members from the shared cluster filesystem
configuration, so one authenticated call from any member sees every
node.

### What is already true about the Apiary side

**The join path is already built and already strict.** ADR-0147 Part 2
and the work that implemented it give Apiary a two-way, peer-authorized
Colony join: a first code the joiner shows, fingerprint comparison by
hand, a second PIN released only to the requesting Comb, a Colony-wide
join window in replicated FSM state, a fail-closed certificate
evaluation gate, and a pin into the replicated peer trust store.
Approval requires a **live window**, not merely a request.

**The trust model is a Colony CA, not a leaf pin.** ADR-0155 and its
staged implementation (ADR-0186, ADR-0187, ADR-0188, ADR-0184,
ADR-0185) settled the trust anchor: the store records the **issuing CA**,
because a publicly trusted leaf is reissued every 90 days and pinning the
leaf would demand a manual re-pin on every renewal. The CA's own chain
is deliberately not walked.

**Apiary is FreeBSD-only and bhyve-only.** There is no KVM, no QEMU, no
Linux-container path. The README records this twice, in the context of
importing VMs from other hypervisors: no disk-format conversion, and
Apiary is UEFI-only.

### The problem with the task as written

The task's second bullet asks Apiary to "join Proxmox clusters". Read
literally, that would mean an Apiary Comb becoming a member of a Proxmox
cluster — which would require Proxmox to accept it as a node, would mean
Apiary joining corosync, and would be precisely the "Proxmox
cluster-management layer" the ADR's own alternative #1 rejects.

Read as the ADR frame intends, "joining" means the reverse direction:
**discovering** a Proxmox cluster and **enrolling** its nodes into
Apiary's management domain. That reading is consistent with the
title, with the operational model, and with the explicit exclusion of
"Implementing Proxmox cluster membership or quorum" from scope.

This ADR resolves the ambiguity in favour of the discovery/enrollment
reading, and says so, because the two readings are not compatible and
the difference is load-bearing.

## Decision

Apiary will implement Proxmox integration as a **node discovery and
enrollment provider**. Apiary uses existing Proxmox interfaces to
discover clusters and their members, maps discovered nodes into an
Apiary-owned record of *external infrastructure*, and enrolls eligible
nodes into Apiary's management domain under Apiary's own identity,
authentication, and authorization policies.

**Apiary will not implement or maintain a separate Proxmox
cluster-management layer.**

### Responsibilities

**Proxmox remains authoritative for:**

- Cluster membership and cluster configuration.
- Cluster quorum and related coordination.
- Proxmox-specific node lifecycle and management operations.
- The authoritative state exposed through supported Proxmox interfaces.

**Apiary is responsible for:**

- Discovering Proxmox clusters and their nodes.
- Mapping discovered nodes to stable Apiary identities.
- Authenticating nodes and authorizing their enrollment into Apiary.
- Tracking connectivity and reconciling discovered state with Apiary's
  record.
- Reporting discovery failures, enrollment failures, and changes in node
  availability.

**The integration is responsible for:**

- Communicating with Proxmox through existing interfaces.
- Translating Proxmox discovery data into Apiary's internal external-node
  representation.
- Coordinating discovery and enrollment without duplicating Proxmox
  cluster-management behaviour.

### The provider interface is the boundary

Proxmox-specific logic lives behind a defined provider interface, so
Apiary's core remains independent of Proxmox internals. The interface
is a discovery interface, not a management interface:

```
Discover(ctx) -> []ExternalNode, error
Authenticate(ctx, node) -> error
Enroll(ctx, node) -> error
```

`ExternalNode` carries, at minimum:

| Field | Source | Note |
| --- | --- | --- |
| `provider` | — | `proxmox` |
| `cluster_key` | `GET /cluster/status`, `type: 'cluster'` | Absent on a standalone node |
| `node_name` | `GET /nodes`, `node` | Proxmox's own node name |
| `fingerprint` | `GET /nodes`, `ssl_fingerprint` | Stable per-node identity anchor |
| `status` | `GET /nodes`, `status` | `unknown` / `online` / `offline` |
| `boot_mode` | `GET /nodes/{node}/status`, `boot-info.mode` | `efi` / `legacy-bios` |
| `observed_at` | Apiary | When Apiary last saw this node |

The interface has no method for *creating*, *modifying*, or *removing*
anything in Proxmox. That absence is the enforcement of the "no parallel
Proxmox implementation" constraint: a provider that cannot mutate
Proxmox cannot become a second control plane, whatever it is later asked
to do.

**The interface is provider-shaped, not Proxmox-shaped.** A second
provider — for a different hypervisor, a different orchestrator, a
bare-metal inventory source — implements the same interface. That is what
makes "Apiary's discovery and enrollment mechanisms can be reused by
future infrastructure providers" true rather than aspirational.

### Identity mapping: the composite key

A discovered Proxmox node is identified to Apiary by a **composite key**,
not by any single Proxmox field:

```
Apiary external-node identity = (provider, cluster_key, node_name)
```

with `fingerprint` carried as the **immutable** attribute and used for
conflict detection.

This is deliberate, and each part earns its place:

- **`cluster_key` is `GET /cluster/status`'s `type: 'cluster'` `name`,
  which is absent on a standalone node.** A standalone Proxmox host
  therefore has a *different kind* of key from a clustered one, and the
  absence must be represented rather than papered over with an empty
  string — an empty `cluster_key` that sorted next to a real cluster name
  would let two different Proxmox environments collide silently.

- **`node_name` is Proxmox's own node name.** It is stable within a
  cluster and is the key Proxmox itself uses, which is what makes the
  mapping reproducible by an operator reading a Proxmox screen.

- **`fingerprint` is the tie-breaker, not the key.** Proxmox node names
  are operator-chosen and are not guaranteed unique across *different*
  Proxmox clusters; they are unique *within* one. Using the fingerprint
  alone as the key would make the identity survive a rename, but it
  would also make it unreadable to an operator and would not group a node
  with its own history. Using the name alone would make a rename look
  like a delete plus an add.

The consequence, and it is the important one: **a rename of a Proxmox
node is an identity change**, and the reconciliation must notice it
rather than silently creating a second record. The fingerprint is what
makes that detectable — a record whose `fingerprint` changed under a
matched `cluster_key`/`node_name` is a rename or a re-keyed node, and
**both are reported as a conflict, not silently accepted**.

### Duplicate detection and identity conflicts

Discovery must never create a duplicate Apiary record for the same node,
and repeated discovery must be idempotent. Three cases are distinguished
explicitly, because conflating them is how an integration corrupts a
record:

1. **Same composite key, same fingerprint.** The ordinary case. The
   existing record is updated (status, observation time). Nothing else
   happens. This is what makes repeated discovery safe.

2. **Same composite key, different fingerprint.** A **conflict**. The
   record is *not* updated and *not* duplicated. This is reported
   distinctly, because it means either the node was re-keyed, or two
   different things now share a name. Either way it is a thing an
   operator must look at.

3. **Same fingerprint, different composite key.** Also a **conflict**.
   The node was renamed. It is *not* treated as a new node, because
   that would silently orphan the old record and its history.

Failing closed on both conflicts is the whole point. A record that
cannot be placed unambiguously is worse than a missing record, because a
missing record is visibly missing.

### Enrollment is a separate, explicit act

**Discovery does not, by itself, authorize enrollment.** A node's
appearance in Proxmox cluster membership is evidence of its existence in
that environment, not an unconditional grant of trust to Apiary.

Enrollment therefore reuses Apiary's existing authorized-join machinery
rather than inventing a parallel path. A discovered node is a *candidate*;
enrolling it is an operator-authorized action that:

- runs through the Colony join window (ADR-0147 Part 4), because
  enrollment adds a peer that other Combs must trust;
- requires the same live-window approval, and the same approval phrase,
  that a manually joining Comb requires;
- produces a certificate **from the Colony CA** (ADR-0155), not a
  third-party certificate from the Proxmox environment;
- is recorded as a distinct, visible act in the log, distinguishable
  from a manual join, so an operator can tell afterwards which Combs came
  from Proxmox and which were joined by hand.

**Enrolling a Proxmox node does not make it a raft member.** This is the
constraint from the Context section, and it is load-bearing. An enrolled
external node is a record in Apiary's external-node store; it is not a
Colony member, it holds no raft vote, and it is not a Comb. It has no
`managerd` to reconcile against, no bhyve, no ZFS datasets, no cells.

**The enrolment trust model is therefore explicit about what it does
grant.** An enrolled external node is a *known, authenticated,
operator-approved external host* that Apiary can report on, and that can
be associated with Apiary-side objects. It is not a host Apiary can
manage. Making it manageable would require Apiary to speak QEMU/KVM
management and Linux storage — which is a different product decision,
recorded as explicitly out of scope below.

### Reconciliation

Reconciliation is a periodic, idempotent refresh: query the provider,
compare against the record, and apply only the differences. It runs on an
interval (operator-configured, defaulting to the same cadence as the
existing reconciler), and it is safe to run concurrently with itself and
safe to interrupt.

Reconciliation distinguishes four conditions, and the distinction is
required rather than cosmetic:

| Condition | Meaning | Effect on the record |
| --- | --- | --- |
| **Node disappeared** | Absent from the provider's listing | Marked absent; **not** deleted |
| **Node unreachable** | Listed but reported offline | Marked unreachable |
| **Discovery failed** | The provider call itself failed | **No** record changes; the failure is reported |
| **Partial failure** | Some nodes read, others not | Per-node results; each failure is named |

The "node disappeared" rule matters most. **A node that vanishes from
Proxmox is not deleted from Apiary.** It is marked absent and kept,
because a Proxmox node can be down, rebooting, or temporarily
unreachable from the API endpoint, and deleting a record on a transient
absence would destroy the very history that makes an absence
diagnosable. Deletion is an explicit operator action, never a
side-effect of a failed read.

The "discovery failed" rule is the other half of that. **A failed
discovery call changes nothing.** A provider that returns an empty list
because it could not authenticate is not the same as a provider that
successfully reports zero nodes, and the second must never be
indistinguishable from the first.

### Authentication, and the privilege boundary

The Proxmox credential is an **API token** or a username/password pair
used against `pveproxy`'s HTTPS endpoint. TLS is required and verified:
the Proxmox endpoint's certificate is validated against a configured
CA bundle or the system trust store, and there is **no**
`insecureSkipVerify` mode, consistent with ADR-0161's finding that
Zabbix's own schema default for `verify_peer` is *off* — a template that
omits the setting accepts any certificate, and the same reasoning
applies here.

The credential is stored **outside** raft-replicated state, in the same
place Apiary already keeps other per-host secrets: a token *file* path
in `managerd.json`, pointing at a root-owned file, following the existing
`cloudflare_token_file` / `origin_ca_token_file` pattern. A Proxmox
credential is a per-host secret and must not become replicated Colony
state.

**The privilege set is the minimum the implemented operations need, and
it is stated rather than assumed:**

- `GET /nodes` needs only an authenticated user.
- `GET /cluster/status` needs `Sys.Audit` on `/`.
- `GET /nodes/{node}/status` needs `Sys.Audit` on `/nodes/{node}`.

The privilege question is genuinely open, and this ADR does not paper
over it. `Sys.Audit` on `/` is a **broad** privilege — it is the
audit-the-whole-datacenter privilege, and granting it to a discovery
credential is heavier than "read the node list" sounds. There are two
reasonable positions, and they are recorded rather than resolved here:

1. Grant `Sys.Audit` on `/` and use `GET /cluster/status` for cluster
   identity, accepting the broad privilege for a simpler and more
   complete identity.
2. Grant only per-node `Sys.Audit` on `/nodes/{node}`, and derive
   cluster identity from the node listing alone — at the cost of losing
   the `quorate` and `cluster_name` facts that only
   `GET /cluster/status` carries.

The choice belongs to the owner and is an explicit input to
implementation. What this ADR decides is that **the credential is
scoped to read-only endpoints on the nodes being discovered, and is never
granted a mutating privilege** — no `Sys.Modify`, no `Sys.Audit` on
`/vms`, no cluster-management rights, no `VM.Allocate`. Discovery must
not be able to modify a Proxmox node or its cluster, and the credential
must be structurally incapable of doing so.

### Failure reporting

Discovery failures, authentication failures, enrollment failures, and
unreachable nodes are four different conditions and are reported as four
different things. Each refusal names itself, following the pattern the
rest of the codebase already uses: a refusal that says "something went
wrong" is a refusal an operator cannot act on.

### What the provider does NOT do

The provider interface deliberately has no method for creating,
modifying, or removing anything in Proxmox. Discovery is read-only by
construction. An enrollment that succeeds changes Apiary's state and
nothing in Proxmox. **Apiary never modifies Proxmox configuration as a
consequence of discovery or enrollment**, and it never assumes it owns a
discovered node.

## Alternatives Considered

### 1. Implement a Proxmox management layer in Apiary

**Rejected.** This would duplicate existing functionality, introduce
unnecessary compatibility work, and create a long-term obligation to
track Proxmox implementation changes. It is also the reading of the task
that the Context section rejected: "support joining Proxmox clusters"
does not mean Apiary Comb becoming corosync members.

### 2. Treat Proxmox clusters as independently managed Apiary clusters

**Rejected.** This risks creating competing sources of truth for
membership and configuration. Apiary should model the nodes it discovers
without taking ownership of Proxmox cluster semantics.

### 3. Model a discovered Proxmox node as a raft member

**Rejected, and this is the alternative the Context section forces.**
It was considered seriously because it would have been the *simple*
answer: `AddVoter` already exists, the overview already renders
membership, and an enrolled node would appear in the same list as
everything else.

It fails on three independent grounds:

- **A raft member is a peer in a consensus group.** Adding a Proxmox
  node to raft membership makes it a voter or non-voter in Apiary's
  consensus, reachable over Apiary's raft transport, expected to speak
  Apiary's internal protocol. A Proxmox node does none of that, and
  cannot be made to without writing and maintaining a Proxmox-side
  agent — which is exactly the "second control plane" this ADR exists to
  avoid.
- **A raft membership change is a quorum event.** It must be replicated
  through raft, which means it must be applied on the leader and
  committed. Making a discovery loop able to trigger membership changes
  puts an external system's polling cadence in the path of Apiary's
  consensus.
- **It would be a lie in the UI.** The overview renders raft membership
  as Combs. A Proxmox node in that list would appear as a Colony member
  that Apiary can manage, place cells on, and reconcile against. It is
  none of those things.

### 4. Store discovered nodes in the existing per-Combs model

**Rejected.** Apiary's "Comb" is a FreeBSD host running bhyve with its
own `managerd`, its own ZFS datasets, and its own cells. A Proxmox
node is a Linux host running KVM. Putting the second in the first's
model would claim a management relationship that does not exist, and the
reconciler would then try to act on it.

### 5. Use `GET /cluster/resources` as the primary discovery source

**Rejected as the primary source**, though it is a reasonable secondary.
It is a `type`-filtered inventory (`vm`, `storage`, `node`, `sdn`) rather
than a node listing, its `node` rows are a different projection of the
same facts, and it requires the broader `/` `Sys.Audit` to be useful for
cluster-wide reads. `GET /nodes` is the endpoint whose semantics are
exactly "list the cluster's nodes", and it is the one with the cheaper
privilege. It may be used later to enumerate what runs on a node.

### 6. Make enrollment automatic on discovery

**Rejected.** It would make a Proxmox environment able to grow Apiary's
trusted surface without an operator deciding anything, which the
"discovery does not authorize enrollment" rule exists to prevent. A
node appearing in a Proxmox cluster is evidence that it exists, not
consent to be managed by Apiary.

## Consequences

### Positive

- Apiary can discover and enroll Proxmox nodes without implementing a
  second cluster-management system.
- Proxmox remains authoritative for its own cluster state.
- The integration has a narrower maintenance and compatibility surface.
- Apiary's discovery and enrollment mechanisms can be reused by future
  infrastructure providers, because the interface is provider-shaped
  rather than Proxmox-shaped.
- Security and enrollment decisions remain under Apiary's control.
- The provider interface is read-only by construction, so the "no
  parallel Proxmox implementation" constraint is enforced by the shape
  of the code rather than by a convention.

### Trade-offs

- The integration depends on the availability and behaviour of the
  Proxmox interfaces it uses.
- Changes in Proxmox APIs may require updates to the provider.
- Discovery data may become stale between reconciliation runs.
- Mapping Proxmox node identities to Apiary identities requires explicit
  rules — the composite key, and the two conflict cases.
- Proxmox discovery credentials and Apiary enrollment credentials must be
  managed separately where their trust boundaries differ.
- A discovered node is observable but not manageable, which will
  disappoint an operator who expected the discovery to imply control. The
  alternative is worse.
- The `Sys.Audit` privilege question is unresolved and is a real
  decision for the owner, not a detail.

## Scope

**Included:**

- Discovery of Proxmox clusters and their member nodes.
- Retrieval of the node information required for Apiary's external-node
  record.
- Identity mapping and duplicate detection.
- Authorized enrollment into Apiary.
- Reconciliation of discovered state.
- Error reporting and recovery behaviour.

**Excluded:**

- Implementing Proxmox cluster membership or quorum.
- Creating or modifying Proxmox clusters.
- Reimplementing Proxmox node management.
- Maintaining a separate Proxmox control plane.
- Automatically modifying Proxmox configuration as a consequence of
  discovery or enrollment.
- **Managing a Proxmox node's workloads.** Apiary does not create, start,
  stop, migrate, or snapshot QEMU/KVM guests on a discovered node, and
  does not manage its storage. Doing so would require a Linux/KVM
  management plane, which is a separate product decision and a separate
  ADR.
- **Importing VMs from Proxmox.** The README already records this as
  absent — no disk-format conversion, and Apiary is UEFI-only. This ADR
  does not change it, and discovery deliberately surfaces no disk-format
  implication.
- Making a discovered node a raft member or a Comb.
- Accepting inbound requests *from* Proxmox servers.

### On the inbound half of the task

The Notion task's first bullet — "allow Apiary to accept requests
originating from Proxmox servers" — is **not covered by this record**.

It is the mirror image of this design and is a genuinely different
problem. This ADR is about Apiary *pulling* from Proxmox over the
Proxmox API. An inbound path means a Proxmox server *pushing* to
Apiary, which requires a request protocol, an authentication and trust
model for Proxmox-originated requests, and a defined answer for
membership — none of which this ADR decides, and all of which the task
lists under "Scope to define".

Deciding it here would have meant designing a second trust boundary in
a record about a first one. It is recorded here as **explicitly not
decided**, so that the omission is visible rather than mistaken for an
oversight. It should be a follow-up record, and it should be written
against whatever the enrollment half actually builds, so that the
inbound path can reuse the same trust model rather than invent a second.

## Implementation Follow-up

The implementation should establish:

1. **The supported Proxmox interfaces and minimum supported versions.**
   The endpoints identified here (`GET /nodes`, `GET /cluster/status`,
   `GET /nodes/{node}/status`) are stable public API; the minimum
   version that supports them must be pinned. `GET /cluster/status`'s
   `protected => 1` marker and the `Sys.Audit` requirement are the
   version-sensitive parts.
2. **The minimum permissions required for discovery.** This is the open
   privilege question above, and it is an owner decision. The
   requirement is `Sys.Audit` on `/` (for `GET /cluster/status`) or
   `Sys.Audit` on `/nodes/{node}` (for per-node status). The mutating
   privileges are excluded unconditionally.
3. **The stable identity keys used to map Proxmox nodes to Apiary
   external-node entries.** This ADR proposes the composite key
   `(provider, cluster_key, node_name)` with `fingerprint` as the
   conflict-detection attribute, and the representation of an absent
   `cluster_key` on a standalone node. It needs an implementation-level
   confirmation that this is representable in the store.
4. **The enrollment authorization and trust model.** The proposal is to
   reuse ADR-0147's join window and ADR-0155's Colony CA, with
   enrollment logged as a distinct visible act. The exact RPC surface
   for "enroll this discovered node" needs designing.
5. **The reconciliation interval and behaviour when nodes disappear or
   become unreachable.** This ADR decides the *behaviour* (mark absent,
   never delete; failed discovery changes nothing) but not the interval.
6. **The provider interface and its error model.** The interface shape
   above is a proposal; the error model needs to distinguish discovery
   failure, authentication failure, conflict, and unreachable node as
   four separate conditions, per the failure-reporting rule.
7. **Tests for duplicate discovery, repeated enrollment, authentication
   failures, stale data, and partial failures.** Specifically: a
   repeated discovery that must be idempotent; the two conflict cases
   (same key/different fingerprint, same fingerprint/different key) that
   must be reported and must not mutate the record; a discovery call that
   fails and must change nothing; a partial read that must report per
   node; a node that disappears and must be marked rather than deleted;
   and a standalone Proxmox node with no `cluster` row that must be
   handled without treating the absence as an error.

These details should be resolved during implementation without expanding
Apiary's responsibilities into Proxmox cluster management.

## Decision Summary

Apiary will discover Proxmox clusters and their nodes through existing
Proxmox interfaces, then enroll eligible nodes into its own management
domain. Proxmox remains authoritative for cluster state; Apiary remains
authoritative for its own inventory and enrollment policy.

A discovered Proxmox node is **not** a raft member and **not** a Comb.
It is an external-node record, identified by a composite key, with
conflicts reported rather than resolved by guessing, and it is enrolled
only through Apiary's existing authorized-join machinery.

The objective is integration with Proxmox, not reimplementation of
Proxmox.

## Evidence and references

- Notion: "ADR: Proxmox Node Discovery and Enrollment" (Proposed,
  2026-10-27); "Todo: Accept Proxmox server requests and join Proxmox
  clusters" (2026-10-27).
- Proxmox API behaviour verified against upstream source, not assumed:
  - `proxmox/pve-manager` at commit `73ac838` — `PVE/API2/Nodes.pm`
    (`GET /nodes` index, `GET /nodes/{node}/status`),
    `PVE/API2/Cluster.pm` (`GET /cluster/status`, `GET /cluster/resources`),
    `PVE/API2Tools.pm` (`extract_node_stats`).
  - `proxmox/pve-cluster` at commit `a749090` — `src/PVE/Cluster.pm`
    (`get_nodelist`, `get_node_fingerprint`).
  - `proxmox/pve-access-control` at commit `dff26b4` — the privilege
    check model.
- Apiary-side claims verified against the tree this ADR lands beside:
  `internal/frontend/cluster_overview.go` (membership-derived inventory,
  `knownColonyMember`), ADR-0147 (the authorized join), ADR-0155 (the
  Colony CA), ADR-0164 (membership-derived peer allowlist), ADR-0161
  (the TLS-verification-must-be-explicit finding), and
  `etc/apiary/managerd.json.sample` (the token-file configuration
  pattern).
