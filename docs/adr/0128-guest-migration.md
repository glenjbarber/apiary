# ADR-0128: Live guest migration between Combs

## Status

Proposed

## Context

ADR-0127 catalogued Sylve.io's feature set against Apiary and ranked
**guest migration first**, in Phase 1. The problem statement there is
blunt: Apiary cannot move a VM or jail from one Comb to another at all.

What exists today is `ManagerService.MigrateVM` and
`ManagerService.MigrateJail` (`api/rpc/manager.proto`). Both are
deliberately minimal, and their own proto doc comments say so:
`MigrateVM` only succeeds when `target_node_id` is *already* this
guest's `replica_node_id` — that is, when the target is a HAST
secondary already holding a synced copy of the disk (ADR-0026,
ADR-0028). On success, `node_id` and `replica_node_id` are swapped
and the HAST role machinery converges the change like any other role
flip. The handler (`internal/manager/server.go`) enforces exactly
three rejections: unknown guest, `desired_state == *_STATE_DELETING`,
and a target that is not already a synced replica. It emits no phase,
blocks for no time, and produces no progress of its own.

That is a **cold ownership transfer between two nodes that already
hold identical data**, not a migration. It is correct, and it is what
ADR-0028 chose on purpose: without an existing replica, the reconciler
would tear down the real dataset on the old owner and provision a
brand-new empty one on the new owner, silently destroying the guest's
data. Apiary's standing rule is that no default may be silently
destructive.

Three consequences follow, and they are what ADR-0127 was pointing
at:

1. **Maintenance is blocked.** A Comb that needs a kernel update, a
   NIC re-cable, or a pool rebuild cannot drain its guests, because
   the only move available requires a HAST partner that is already in
   sync.
2. **Failure-domain rebalancing is blocked.** A VM that is too large
   for its Comb, or a Comb that is over-committed, cannot be
   re-placed.
3. **Flight Plans are blocked** for any step that requires
   re-placing a guest mid-plan, because there is no verb for "move
   this and keep it running."

ADR-0127 sketched a five-phase design (preflight, bulk transfer,
final sync, cutover, cleanup) and a new `MigrateGuest` RPC. That
sketch is a starting point, not a decision. It does not say what is
raft-replicated and what must never be; it does not say what happens
when the Colony loses quorum halfway through; it does not say whether
the guest keeps its IP; and it does not say what a half-migrated
guest looks like. Those are the questions this ADR answers.

### Vocabulary

- **Comb** — a node running `managerd`. Existing code says "node" and
  `node_id`; this ADR uses Comb for prose and the existing field
  names for anything in the API.
- **Colony** — the raft-replicated control plane. `raftd` is its
  storage and consensus layer (ADR-0001, ADR-0003).
- **Cell** — a workload (a VM or a jail), per ADR-0127's catalog.
- **Flight Plan** — an orchestration sequence.

### What exists to build on

| Capability | Where it lives today | Gap for live migration |
| --- | --- | --- |
| ZFS send/recv streaming | `internal/zfs.Manager.Send` / `Receive`, `internal/zfs/manager.go` (ADR-0089) | `Send` takes only a whole snapshot name; no incremental resume, no `-R`, no raw `send -w` |
| Cross-node stream RPCs | `PushJailTemplateTo` / `ReceiveJailTemplate` in `api/rpc/manager.proto`, `internal/manager/peer.go` | template-shaped, one-shot, no resume, no error phase |
| Reconciler and plan | `internal/cluster/reconciler.go`, `internal/cluster/plan.go` | plans against `node_id` only; no drain or migrate intent |
| Replica freshness | ADR-0119, ADR-0121 | about HAST, not about migration progress |
| Health evidence | `internal/health`, ADR-0056, ADR-0122 | `unknown` is not `healthy`; see below |
| Quorum facts | `internal/recovery/quorum.go` | classifies, does not gate |
| Guest identity | `VMDefinition.ip_address` / `mac_address` (ADR-0044), assigned once in the FSM | need to be *preserved* across a move, not reallocated |

## Decision

Add **live guest migration** as an explicitly multi-phase,
leader-coordinated, raft-journalled operation, gated on the Colony
holding quorum throughout, and gated on ADR-0130 (ZFS replication) for
any target that is not already a HAST replica.

The central design commitment is this:

> **The Colony replicates the *intent* to migrate and the *provenance*
> of the copy. It never replicates guest runtime state.**

### What is raft-replicated, and what is never replicated

Raft-replicated (`api/internalpb/state.proto`):

- The migration record itself: guest id, source Comb, target Comb,
  guest type, phase, timestamps, error text, attempt counter.
- The final ownership swap: one `UpdateVM` / `UpdateJail` command that
  moves `node_id` to the target and, optionally, demotes the source to
  `replica_node_id`. Exactly one such command is ever applied per
  migration, and it is the last thing that happens.
- A **migration token**: a monotonically increasing integer,
  allocated by the leader in the migration record, that every peer-side
  RPC for this migration must present. It exists so a delayed RPC
  from attempt 1 cannot act on the guest during attempt 2.

Never replicated, and never reconstructed from raft:

- Guest memory state, CPU register state, vCPU scheduling position,
  timers.
- Open file descriptors, sockets, vnet/epair handles, TTY/pty pairs.
- In-flight block IO to the guest's disk, the guest kernel's page
  cache, the FreeBSD host's own VM process state.
- The bhyve or jail process itself. A process cannot be Raft state;
  two hosts starting "the same" process is precisely the split-brain
  failure mode this design exists to prevent.

This is the central safety property, so it is worth being explicit
about the reasoning. Raft's guarantee is that every replica applies
the same *log* in the same order and therefore computes the same
state machine. It is not, and cannot be, a guarantee about two
independent FreeBSD kernels running the same guest. If the guest's
runtime state were replicated, a partition would produce two live VMs
with the same disk, both accepting writes, and no quorum check could
detect it, because both sides would be confidently applying an
identical log. **Exactly one Comb may have a running guest at any
instant, and the only thing that decides which one is a single Raft log
entry, applied once, by one leader, on one node.**

Everything below follows from that.

### The protocol

Six phases. Each is a durable, observable state. The Colony may be
paused between phases; it may never skip a phase or run two
concurrently for one guest.

```
preflight --> freeze --> bulk --> sync --> cutover --> teardown
                        |                 |
                   (resumable)     (terminal, one-way)
```

| Phase | Guest state | On-disk effect | Operator sees |
| --- | --- | --- | --- |
| `preflight` | running, untouched | none | "checking" |
| `freeze` | frozen (paused, not stopped) | none | "pausing" |
| `bulk` | frozen | `zfs send` running | "copying 41%", bytes moved |
| `sync` | frozen | final incremental `zfs send` | "finalising" |
| `cutover` | stopped on source; starting on target | ownership swapped in raft | "starting on comb-b" |
| `teardown` | running on target | source dataset destroyed | "complete" |

**Preflight** validates, and rejects with a specific reason for
each:

- target is a known Colony member and is not the current owner
- target is `healthy` **or** `degraded` per ADR-0122 / ADR-0056. It
  must **not** be `unknown`: unknown means no evidence, and handing a
  guest to a Comb we have not observed is exactly the silence-treated-
  as-health that ADR-0056 exists to forbid
- same `uname -m` architecture as the source
- target has a ZFS pool with free space at least the guest's dataset
  `used`
- no other operation is in flight for this guest (migration, backup,
  HAST role change, lifecycle transition)
- target is not already the owner of a guest with the same id
- for jails: the target has every `base_template` the jail depends on
  (ADR-0084, ADR-0089). The reconciler would otherwise start a jail
  with a missing template, which fails later and less clearly

**Freeze** is a guest pause, not a stop. For bhyve this is the
existing `bhyvectl --vm=<name> --pause`; for a jail, the jail's own
suspend. Pausing rather than powering off keeps the guest's memory
intact on the source for the entire copy, which is what makes the
cutover short and what keeps the freeze window from being a
re-provision.

**Bulk** is `zfs send -R` of the guest's dataset tree into
`zfs receive` on the target, while frozen. The migration record
carries the `from_token` and `to_token` of the last completed
incremental send, so a dropped connection resumes rather than
restarts. The existing `PushJailTemplateTo` / `ReceiveJailTemplate`
stream pair (`internal/manager/peer.go`) is the shape to generalise,
not the guest protocol itself.

**Sync** is a final incremental `zfs send` from the token to the
current snapshot. Short by construction — the guest has not written
since the freeze.

**Cutover** is the only dangerous moment, and it is deliberately
narrow:

1. stop the guest on the source (destroy the bhyve process, or the
   jail)
2. apply **one** raft command moving `node_id` to the target
3. the target's reconciler observes the new `node_id`, provisions the
   guest from the received dataset, and starts it
4. the target's reconciler reports the phase back to the leader
   (the existing peer-report RPC pattern, ADR-0029)
5. verify: the target reports a running guest with the expected
   dataset GUID and the expected IP

The window between (1) and (3) is the guest's downtime. It is
bounded by raft commit plus the target's own provisioning, and it is
**not** zero. This ADR does not claim it is zero, and the frontend
must not display a zero-downtime promise. What it claims is that the
window is bounded, and that the guest is never running in two places.

**Teardown** destroys the source dataset and the migration's
snapshots, and records the outcome. Teardown failure is non-fatal:
the migration is `complete` and the source residue is a reclaim item
for the existing resource-reclaim path (ADR-0025), not an operator
emergency.

### Network identity handoff

**The guest keeps its IP address and its MAC address across a
migration.** Nothing is reallocated.

This is a decision with teeth, so the reasoning is worth stating.
`VMDefinition` assigns `ip_address` and `mac_address` **once, in the
FSM, at `CreateVM` time, deterministically from the committed log, and
never mutates them afterward** (ADR-0044, and the proto's own comment
on those fields). A migration is a move of the guest between Combs,
not the creation of a new guest. If the IP changed, every one of
these would break silently:

- the per-VM `pf(8)` anchor in `internal/pf`, which keys rules on the
  guest's address
- the Cloudflare Tunnel ingress rule (ADR-0063), which proxies
  `cloudflare_hostname` to `ip_address:cloudflare_port` on the owning
  Comb
- any DHCP lease or static mapping tracked against the guest

Preserving identity is also what makes the operation a *migration*
rather than a rebuild. A guest that comes back with a new address has
to re-DHCP, re-learn, and re-register; the operator cannot tell the
difference between "moved" and "replaced", which is precisely the
ambiguity this project has repeatedly refused.

The cost is real and is accepted: the target Comb must be able to host
the guest's address. In practice this means the guest's `network_id`
(ADR-0022) must be reachable from the target. For a bridged network,
any Comb in the Colony can reach the same L2 segment and there is no
problem. For a jail on a **dedicated VNET/epair** (ADR-0117), the
jail's network is private to the Comb it was created on — the epair
is created on the source and its peer lives inside the source's VNET
namespace. A jail on a dedicated VNET therefore **cannot** be
migrated to another Comb in v1 without also moving the VNET, which is
a much larger piece of work (see Rejected alternatives).

**Preflight rejects a dedicated-VNET jail migration outright**, with a
specific error, rather than starting a copy that cannot be started.
A jail on the flat bridge migrates. A jail on a dedicated VNET does
not, in v1. This is a real narrowing of the feature and it is stated
here rather than discovered later.

### Storage handoff and the ADR-0130 boundary

Storage movement is ZFS send/recv. `internal/zfs.Manager` already has
the primitives (ADR-0089): `Send(ctx, snapshot)` streams `zfs send`'s
stdout live, `Receive(ctx, destName, r)` pipes into `zfs receive`. The
gap is incremental resume, which ADR-0127's "final sync" step assumes
and `Send` does not currently provide.

The dependency on **ADR-0130 (ZFS replication)** is a hard boundary,
and it is worth stating precisely what it does and does not gate.

**Implementable before ADR-0130 lands:**

- The whole protocol, all six phases, the raft record, the state
  machine, the managerd handlers, the recovery path, the frontend
  surface.
- Migration **into a target that is already the guest's HAST
  replica** — which is exactly today's `MigrateVM`, lifted into the
  multi-phase machinery so that operator-visible progress, a single
  ownership swap, and half-migration recovery all exist. This is the
  v1 default and needs no new storage machinery at all: the data is
  already there, so the `bulk` phase is a no-op and the interesting
  work is entirely in the control plane.
- Migration **of a guest with no replica**, for guests on the flat
  bridge, using a one-shot `zfs send -R` then incremental `-I` for
  resume. This uses `Send` / `Receive` as they exist, extended with
  resume tokens; it does not need ADR-0130's replication policy,
  scheduling, or fencing.

**Not implementable before ADR-0130 lands:**

- Any target that is not already receiving a copy — because "already
  has the data" is precisely what a replica is. This is the same
  reasoning that already gates today's `MigrateVM`.
- Continuous fencing evidence of the target, and the guarantee that
  the target is actually up to date at cutover.
- Migration *to* a replica that is behind. Without ADR-0130's
  evidence, Apiary cannot prove the target is current, and a cutover
  onto a stale disk is a silent data-loss event.

So: **ADR-0128 defines the protocol and ships the HAST-replica path
first. The arbitrary-target path is gated on ADR-0130.** The design is
chosen so that when 0130 lands it is a matter of enabling a target
class, not rewriting the protocol — which is also why the storage
abstraction point is the `from_token` / `to_token` pair rather than
anything ADR-0127-specific.

### Mid-migration failure and recovery

Failure modes, and the required behaviour of each:

| Failure | Guest running? | Records | Recovery |
| --- | --- | --- | --- |
| Preflight fails | yes, on source | none created | none; nothing happened |
| Freeze fails | yes, on source | record at `preflight` | operator retries; guest never stopped |
| Bulk fails, link drops | frozen, on source | record at `bulk` with token | **auto-resume** from token; guest stays frozen |
| Target dies mid-bulk | frozen, on source | record at `bulk` | resume when target returns |
| Leader dies at any phase | yes, on source | record survives in raft | new leader resumes from recorded phase |
| Quorum lost at or after freeze | frozen, on source | record at last phase | **halt; do not cut over** |
| Crash between stop and commit | no | record at `sync` | see "the dangerous window" below |
| Target fails after cutover, before start | no | record at `cutover` | reconciler retries provisioning |
| Teardown fails | yes, on target | record at `complete` | reclaim, ADR-0025 |

**The recovery rule is one line: a half-migrated guest must never
silently exist in two places, and must never silently exist in
neither.** Concretely, at every recovery point the Colony must be able
to answer, from raft alone, "which Comb is the owner, and is the
migration still in flight?" A migration record in any phase other than
`complete` or `aborted` is a **fence**: the source reconciler will not
start the guest, the target reconciler will not start the guest, and
only the migration reconciler may act. That is the mechanism, and it
is a raft check performed by the ordinary reconciler, not a special
case in it.

**The dangerous window.** Between "source process stopped" and "raft
commits the new `node_id`", a crash leaves a guest that exists
nowhere in a running state, with ownership still recorded at the
source. This is recoverable and the recovery is boring: the new leader
reads the record at `cutover`, re-applies the single `UpdateVM` — it
is idempotent, and the FSM applies the identical command to the
identical `node_id` — and the target reconciler provisions and starts.
The window is closed in the *safe* direction: the guest is down, not
duplicated. **If instead the source were allowed to resume the guest
while the record still said the source owned it, and the cutover then
also applied, you would have the guest running on the source with the
target provisioning a second copy from the same disk. That is the
failure this design refuses, and the fence is what refuses it.**

**Who may run recovery.** Recovery is automatic and unprivileged: the
migration reconciler resumes any in-flight migration, because only it
can hold the token. An operator may `AbortMigration`, which is the
single destructive action in the feature, and it is `RoleOperator` (or
higher) in `internal/manager/auth.go`, consistent with `MigrateVM`.
Abort semantics are **always** "return the guest to the source and run
it there": resume the source's guest, discard the target's partial
receive, clear the record. Abort is never "give up and delete the
guest".

**Blast radius.** A migration is scoped to exactly one guest. A
failing migration touches no other guest's data, no pool other than
the source and target's, and no raft key other than its own record.
The failure it does carry is a *frozen guest* — which is a Cell with
an outage, and is exactly the failure the operator was trying to
avoid when they started the migration. This is the honest cost, and
the freeze timeout must be bounded and alarmed for it (see Open
questions).

### Destination eligibility

A Comb is a valid destination when **all** of:

- it is a member the Colony currently believes in
- its health is `healthy` or `degraded`, and explicitly **not**
  `unknown` (ADR-0122, ADR-0056: silence is not consent)
- it shares the source's CPU architecture
- it has a ZFS pool with sufficient free space
- it is not already the target of another in-flight migration for
  the same guest, and holds no guest with the same id
- for a VM: it can run bhyve, as already required to own any VM
- for a jail on a dedicated VNET: **it cannot be**, in v1

Note what is deliberately *not* a criterion: **raft voter status.** A
Comb that is a non-voting member, or currently a non-leader, is a
perfectly good destination. Migration is a *data* operation whose
control-plane state is committed by the leader on the leader's own
log; the destination does not need a vote to receive a dataset and
start a guest. Requiring voter status would make maintenance of a
non-voting Comb impossible, which is a large part of why this feature
exists. A Comb that is a voter but *not* a healthy one is a worse
destination than a healthy non-voter, which is the whole point of the
evidence-aware health work.

### Quorum

**Migration may not start without quorum, and may not proceed across
a loss of quorum.** Both are required, and they are the same
requirement seen from two sides.

Starting without quorum is refused because the first thing migration
does after freezing a guest is propose an ownership change. A
proposal that cannot be committed leaves a guest frozen with no
recorded way to unfreeze it — a self-inflicted outage, discovered by
an operator, caused by a feature meant to prevent outages.

Continuing across a loss of quorum is refused because **the
cutover's safety argument is "one leader, one log, one `UpdateVM`,
one Comb."** Lose quorum and that argument is unavailable. A minority
partition holding a frozen guest and a majority partition holding a
`cutover` record could each conclude they own the guest. The fence
helps — the frozen side's record is stale and its fence is still set
— but the fence protects against a *split*, not against a *livelock*,
and the right answer to a guest that cannot be unfrozen because
consensus is gone is to keep it frozen and to say so, not to guess.

The check uses the existing quorum classification in
`internal/recovery/quorum.go` (`ClassifyQuorum`, `ValidQuorumFact`)
rather than a new one, and the API surfaces it through the ADR-0119
maintenance wave planner: a Comb is a valid migration source only if
it is not already in a maintenance wave, and a Comb that is in a wave
is a valid destination only for guests it is not simultaneously
evicting.

### Cross-Colony migration is out of scope for v1

**Explicitly out of scope: moving a guest between two different
Colonies, including cross-site.** Rejected for v1 for four reasons:

1. **No shared identity.** The two Colonies have separate raft logs,
   separate member sets, and no established trust relationship (no
   mTLS between Colonies, no cross-Colony leader protocol).
   Everything in this ADR is built on "one log, one leader, one
   `UpdateVM`", which assumes one Colony.
2. **The network handoff is unsolved.** A dedicated-VNET jail's
   address is meaningful only inside its source Comb's VNET. Moving
   it across Colonies means moving the network, which this ADR
   already declines to do within a Colony.
3. **The storage handoff needs a shared key.** ZFS native encryption
   keys (`zfs send -w`, which ADR-0127's design assumes) are
   per-Colony. A cross-Colony move is a key-distribution problem
   wearing a data-migration costume.
4. **The failure story is different.** A cross-Colony move is a DR
   event. ADR-0127 puts the DR foundation (replication, backup) in
   Phase 2 for exactly this reason. Migration is a *planned*
   operation within one failure domain; conflating the two would
   produce a feature whose outage semantics nobody has reasoned
   about.

The Phase 1 follow-on is, in order: ADR-0130 replication, then
backup, then cross-Colony DR as a separate ADR with its own failure
model.

## Rejected alternatives

### 1. Replicate the guest's runtime state through raft

The literal reading of "the Colony is replicated, so migrate by
replicating" would put vCPU state, memory, and device handles in the
raft log. Rejected outright. Raft guarantees identical log
application; it guarantees nothing about two independent FreeBSD
kernels driving the same disk. Under a network partition, two sets of
hosts would each apply the same log and each run the guest, and no
quorum check would catch it, because both sides would be *correct*.
This is the split-brain the whole design is built to prevent, and no
amount of protocol care on top of it makes it safe.

### 2. Live migration with shared storage (HAST as the transport)

Put the guest's disk on a shared HAST device, run the guest on the
source, and hand the HAST primary role to the target mid-flight. This
is how VM live migration is often done, and it would give a genuinely
small freeze window. Rejected because Apiary's HAST is a
*replication* mechanism with a primary/secondary pair and its own role
machinery (ADR-0026, ADR-0028), not a shared clustered volume; a
guest writing to a device whose primary changes under it must be
quiesced anyway, at the block layer, by something that understands
the guest's write cache — which for bhyve means the guest's own
coherence protocol, not ZFS. The complexity buys a smaller downtime
window that ADR-0128 does not promise in the first place. Revisit
only if measured downtime proves operationally unacceptable.

### 3. Copy-then-restart without a raft record (fire-and-forget)

Push the dataset to the target, then hand-write a `node_id` change
from the UI. Rejected because it is exactly the failure mode this
project has refused everywhere else: a partially completed move
leaves two datasets and an ambiguous owner, with no record
distinguishing "moving" from "moved". A raft record is what makes the
operation recoverable, idempotent, and operator-legible. There is no
cheap version of this that is still correct.

### 4. Migrate the guest's network along with the guest

Carry the jail's VNET/epair across Combs so *every* jail is
migratable. Rejected for v1 on cost and on surprise. The epair is a
host-level device with a namespace on one kernel; moving it means a
network migration protocol with its own consistency window, and jails
deliberately choose dedicated VNETs precisely to get *isolation
from* the shared bridge. A half-migrated VNET is a worse incident
than an unmigratable jail. Jails on dedicated VNETs are rejected at
preflight with a clear reason; that is a smaller feature with a
sharper edge.

### 5. Automatic placement and background rebalancing

Have the Colony decide where guests should live and migrate them to
rebalance capacity automatically. Rejected as out of scope, and note
this is not a new position: `VMDefinition.node_id` and
`replica_node_id` are explicitly caller-set because this project
deliberately has no automatic node scheduling. ADR-0128 is the
*manual* verb; the automatic scheduler remains a separate, larger
decision that should not be smuggled in behind a migration feature.
The maintenance wave planner (ADR-0119) may *suggest* a migration; it
does not run one.

### 6. Add migration to the existing `MigrateVM` / `MigrateJail` RPCs

Reusing the existing names and changing their semantics. Rejected:
those RPCs have a shipped, tested, documented contract — "target must
already be a synced HAST replica" — and their error strings tell
operators exactly what to do. Silently widening them would break
that contract for every existing caller and turn a safe, instant,
single-command operation into a long, fallible one with the same
name. New RPCs, new names, old behaviour preserved.

## Consequences

### Positive

- Maintenance of a Comb becomes possible at all. Drain a Comb by
  migrating its guests off, do the work, migrate them back. This was
  impossible before unless every guest happened to have a HAST
  partner.
- Failure-domain rebalancing becomes possible: a guest that no longer
  fits, or sits on Comb hardware that is failing, can be moved to one
  that does.
- Flight Plans gain a re-placement verb, which is what ADR-0127
  identified as a blocker.
- The fence is a *single* mechanism that answers "what is this guest
  doing?" for every failure mode, rather than a per-mode special case.
- Guests keep their IP and MAC, so pf anchors, Cloudflare Tunnel
  ingress, and any DHCP mapping keep working across a move with no
  operator action.
- The storage boundary is drawn so the HAST-replica path, which needs
  no new storage machinery, ships first, and the arbitrary-target path
  is a follow-on rather than a rewrite.

### Negative

- **Downtime is real.** The cutover window is bounded but non-zero,
  and the frontend must say so. Any documentation claiming otherwise
  will be wrong.
- **A frozen guest is an outage.** If bulk transfer stalls, the guest
  is down for the duration. This is the feature's own failure mode
  and needs a bounded freeze timeout with an alarm, not just a retry.
- **The operator has a new dangerous button.** `AbortMigration`
  discards a copy and resumes the source. It is fenced to
  `RoleOperator` and it is destructive in the only way this feature
  is destructive.
- **A migration record is a new raft key with a lifecycle.** It must be
  garbage-collected on `complete` / `aborted` or it grows without
  bound, and a GC bug here is a silent-leak bug.
- **Dedicated-VNET jails are not migratable in v1** (ADR-0117
  interacts here). This is a functional gap operators will notice.
- **Resumable incremental ZFS send is new work** in `internal/zfs`,
  which currently offers only whole-snapshot `Send`.
- **The destination-eligibility rules are opinionated** and will
  reject migrations operators consider reasonable — a Comb whose health
  reads `unknown` is refused even if it is actually fine. That is
  ADR-0056's stance applied consistently, and it is the right
  default, but it will generate support questions.

## Implementation notes

### Proto (`api/rpc/manager.proto`)

New RPCs, alongside — not replacing — `MigrateVM` / `MigrateJail`:

```protobuf
rpc StartGuestMigration(StartGuestMigrationRequest)
    returns (StartGuestMigrationResponse);
rpc GetGuestMigration(GetGuestMigrationRequest)
    returns (GetGuestMigrationResponse);
rpc AbortGuestMigration(AbortGuestMigrationRequest)
    returns (AbortGuestMigrationResponse);
rpc ListGuestMigrations(ListGuestMigrationsRequest)
    returns (ListGuestMigrationsResponse);
rpc ReportMigrationPhase(ReportMigrationPhaseRequest)
    returns (ReportMigrationPhaseResponse);
```

`ReportMigrationPhase` is peer-to-peer, following the existing
`ReportVMPhase` / `ReportJailPhase` pattern (ADR-0029): a target
Comb's reconciler reports progress to the leader's `managerd`.

New messages:

```protobuf
enum GuestType {
  GUEST_TYPE_VM = 0;
  GUEST_TYPE_JAIL = 1;
}
enum MigrationPhase {
  MIGRATION_PHASE_PREFLIGHT = 0;
  MIGRATION_PHASE_FREEZE = 1;
  MIGRATION_PHASE_BULK = 2;
  MIGRATION_PHASE_SYNC = 3;
  MIGRATION_PHASE_CUTOVER = 4;
  MIGRATION_PHASE_TEARDOWN = 5;
  MIGRATION_PHASE_COMPLETE = 6;
  MIGRATION_PHASE_ABORTED = 7;
}
enum MigrationVerdict {
  MIGRATION_IN_FLIGHT = 0;
  MIGRATION_TERMINAL = 1;
}
message GuestMigration {
  string id = 1;
  GuestType guest_type = 2;
  string guest_id = 3;
  string source_node_id = 4;
  string target_node_id = 5;
  MigrationPhase phase = 6;
  string phase_error = 7;
  uint64 token = 8;
  uint64 from_token = 9;
  uint64 to_token = 10;
  uint64 bytes_total = 11;
  uint64 bytes_moved = 12;
  uint64 freeze_started_unix = 13;
  uint32 attempt = 14;
  int64 created_unix = 15;
  int64 updated_unix = 16;
}
```

The fence that the ordinary reconcilers check is **not** carried by
`MigrationPhase` alone. A zero-valued `MigrationPhase` is
`MIGRATION_PHASE_PREFLIGHT`, which is a real, in-flight phase — so a
guest with no migration record at all would read as fenced by default,
and a default that fails *closed* on every existing guest is not
shippable.

Use instead an explicit `bool migration_fenced` on `VMDefinition` /
`JailDefinition`, set only by the start command and cleared only by
the final command. Its default is `false` — no record, not fenced —
which is the correct default for the hundreds of guests that never
migrate. The reconciler checks the bool, not the phase.
`MIGRATION_IN_FLIGHT` / `MIGRATION_TERMINAL` on `MigrationVerdict`
remain useful for the operator view and for the abort path's own
guard, but the safety property rests on the bool.

### Raft state (`api/internalpb/state.proto`)

- New top-level message `GuestMigration`, mirroring the proto above.
- New `Command` oneof arms, matching the existing narrow-command
  style (`UpdateVMPhase`, `SetVMDesiredState`). `Command`'s oneof
  currently ends at `record_restart_completed = 28`, so the new arms
  start at **29**: `start_guest_migration = 29`,
  `update_guest_migration_phase = 30`, `finish_guest_migration = 31`.
- `VMDefinition.migration_fenced = 21` (`hostname` is currently the
  last field, `= 20`) and `JailDefinition.migration_fenced = 14`
  (`vnet` is currently the last, `= 13`) — set by the start command,
  cleared by the single ownership-swap command.
- Raft key layout: `migrations/<migration_id>` holds a
  `GuestMigration`, plus an index
  `migrations/by_guest/<guest_type>/<guest_id>` holding the
  `migration_id`, so "is this guest migrating?" is a single point
  read, not a scan. The reconciler checks that index on every tick.
- The ownership swap itself is the existing `UpdateVM` / `UpdateJail`
  command with `node_id` changed and `migration_fenced` cleared — no
  new command type, so the existing FSM apply path and its tests are
  reused unchanged.

### managerd handlers (`internal/manager/server.go`)

- `StartGuestMigration` — leader-only (forward to leader on
  `leader_hint`, as `MigrateVM` does), runs preflight, and applies
  `StartGuestMigration` with a fresh token. All preflight rejections
  return a specific `error` string and create no record.
- `GetGuestMigration` / `ListGuestMigrations` — leader-forwarded reads.
- `AbortGuestMigration` — leader-only, `RoleOperator` in
  `internal/manager/auth.go`; applies `FinishGuestMigration` with
  `MIGRATION_PHASE_ABORTED` and unfences the source.
- `ReportMigrationPhase` — peer-to-peer; validates the presented token
  against the current record, so a stale attempt's report is rejected.
- Authz: `StartGuestMigration` and `AbortGuestMigration` are
  `RoleOperator`, matching `MigrateVM` / `MigrateJail`; `Get` and
  `List` are `RoleViewer`.

### Reconcile loop (`internal/cluster/reconciler.go`)

The ordinary reconciler gains one check, taken at the top of its
per-guest work:

```
if guest.migration_fenced:
    skip — do not start, do not stop, do not reclaim
```

Everything else about the guest is untouched. A separate **migration
reconciler** on the Comb that owns the source drives the phases. Its
failure modes:

- target unreachable — stay in `bulk`, retry with backoff, keep the
  token
- source guest already gone unexpectedly — abort the migration and
  record why; never "help" it along
- `freeze` older than the configured timeout — abort and unfreeze the
  source, because bounded outage is the guarantee
- token mismatch on resume — restart the transfer from the beginning
  of the current phase rather than trusting a stale token
- teardown failure — record `complete` anyway; reclaim via ADR-0025

### Storage (`internal/zfs/manager.go`)

Extend `Manager.Send` / `Receive` with incremental-resume support:
`SendIncremental(ctx, snapshot, fromToken)` and
`ReceiveIncremental(ctx, destName, r, toToken)`. The existing
whole-snapshot `Send` / `Receive` become the `fromToken == 0` case.
`zfs send -R` is needed for guests whose dataset has children; `-w`
for encrypted datasets, which carries the ADR-0130 key-distribution
dependency.

### Frontend (`internal/frontend`)

A migration view on the Cell page: phase, bytes moved with a progress
bar, freeze start time, elapsed downtime at cutover, and an `Abort`
button. It must show `unknown` health as `unknown` (ADR-0122), and
preflight rejections must be shown as the specific reason, not a
generic failure — the whole point of the enumerated rejection list
is operator legibility.

## Test plan

**macOS cannot validate any of the interesting parts of this feature.**
It has no ZFS, no HAST, no pf, no jails, no bhyve, and no real
network timing. What runs on macOS is only the control-plane logic.

Runnable on macOS (`go test ./...`):

- FSM tests: `StartGuestMigration` creates exactly one record and one
  token; a second start for the same guest is rejected while fenced
- fencing: an in-flight record blocks the reconciler's start path; the
  terminal command clears it
- token validation: a phase report with a stale token is rejected
- quorum gate: start is refused with no quorum, and advance is refused
  on quorum loss, using fakes for `internal/recovery/quorum.go`'s
  `QuorumFact`
- idempotence: re-applying the same `UpdateVM` ownership swap is a
  no-op
- migration-token monotonicity across attempts
- proto round-trip coverage for the new messages

**Requires brood (10.90.0.94) or drone (10.90.0.95):**

- real `zfs send` / `recv` bulk transfer of a multi-GB guest, with
  resume after a deliberately killed connection, confirming the resume
  token is honoured and the final dataset `guid` matches
- real bhyve pause and resume: confirm `bhyvectl --pause` freezes the
  guest, and that the cutover stop-then-start loses no acknowledged
  writes
- real jail migration on the flat bridge, including jail start on the
  target against the preserved IP and MAC
- a dedicated-VNET jail migration attempt, confirming preflight
  **rejects** it with the specific message and changes nothing
- pf anchor survival across the move: the guest's `pf` rules still
  match post-migration because the IP did not change
- a real quorum loss mid-`bulk`: the guest must stay frozen and the
  record must not advance
- a real leader kill mid-`bulk`, then a new leader: resume from the
  token
- the dangerous window: SIGKILL the source between stop and raft
  commit; confirm the guest is down, not duplicated, and that recovery
  converges
- **real-network downtime measurement** for the cutover window across
  guest sizes and Comb architectures. The macOS numbers are fiction;
  the figure quoted to operators must come from brood or drone.
- the HAST-replica-target path: migrate onto an existing synced
  secondary and confirm the `bulk` phase is a genuine no-op

## Open questions

1. **Freeze timeout.** What is the maximum acceptable frozen-guest
   duration before a migration auto-aborts and unfreezes the source?
   A guest is a Cell with a real workload behind it; ten minutes
   frozen is a different incident from ten seconds. This number
   decides whether a slow link is a retry or an outage, and it should
   come from the operator, not from me.
2. **Is a bounded-downtime migration good enough to unblock
   maintenance?** If operators need true zero-downtime, the rejected
   alternative 2 (HAST as transport) has to be reopened, and that is
   a much larger piece of work. Worth confirming before the protocol
   is frozen.
3. **Dedicated-VNET jails.** Accept the v1 gap, or should network
   migration displace something in Phase 1? ADR-0117 users will hit
   this immediately.
4. **Should `StartGuestMigration` be operator-only, or should the
   ADR-0119 maintenance wave planner be permitted to trigger it
   automatically for guests on a Comb entering a wave?** ADR-0119's
   planner is explicitly read-only: it sequences and reports, and does
   not execute. Letting it execute migrations would be a change to
   *that* ADR's stated boundary as much as a change to this one. I
   lean no, and would want explicit consent either way.
5. **Should a `dry_run` preflight be exposed as its own RPC** so an
   operator can ask "could this migrate?" without starting one?
6. **Guest types beyond VM and jail** — whether to use
   `GUEST_TYPE_UNSPECIFIED = 0` or VM-first ordering. Cosmetic, but it
   is a permanent wire value.

## References

- ADR-0001 — raftd process split and UDS protocol
- ADR-0003 — raftd multi-node clustering
- ADR-0022 — network management (`NetworkDefinition`, `network_id`)
- ADR-0025 — resource reclaim (source teardown after cutover)
- ADR-0026 — HAST VM disk replication
- ADR-0028 — migrate VM and jail (the existing, replica-gated
  `MigrateVM`)
- ADR-0029 — cross-node write forwarding (the peer-report RPC pattern)
- ADR-0044 — deterministic MAC for every VM
- ADR-0056 — evidence-aware health v1 (`unknown` is not `healthy`)
- ADR-0063 — Cloudflare exposure (depends on a stable guest IP)
- ADR-0084 — jail base images
- ADR-0089 — jail template peer fetch (the send/recv streaming pattern
  reused for bulk transfer)
- ADR-0117 — dedicated jail networking (VNET/epair; the v1 eligibility
  gap)
- ADR-0119 — replica freshness and the maintenance wave planner
- ADR-0121 — dependency-graph HAST sync evidence
- ADR-0122 — cluster-wide evidence-aware health API
- ADR-0127 — Sylve.io feature adoption (Phase 1, priority 1)
- ADR-0130 — ZFS replication (**parallel sibling, not yet written**;
  the storage dependency boundary named in this ADR)
- `api/rpc/manager.proto` — `MigrateVM`, `MigrateJail`,
  `PushJailTemplateTo`, `ReceiveJailTemplate`
- `api/internalpb/state.proto` — `VMDefinition`, `JailDefinition`,
  `Command`
- `internal/manager/server.go` — the `MigrateVM` handler
- `internal/manager/auth.go` — the RPC-to-role table
- `internal/zfs/manager.go` — `Send` / `Receive`
- `internal/recovery/quorum.go` — `ClassifyQuorum`, `ValidQuorumFact`
- `internal/health/` — evidence-aware health computation
- `internal/cluster/reconciler.go` and `internal/cluster/plan.go`
