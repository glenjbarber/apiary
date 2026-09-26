# ADR-0135: S.M.A.R.T. Disk Health Monitoring

## Status

Proposed

## Context

ADR-0127 lists S.M.A.R.T. disk health in Phase 4 (observability and
operability) and sketches it in one paragraph: a periodic `smartctl -a`,
parsed attributes, threshold alerts, and integration with failure-domain
integrity. That sketch is worth taking seriously and also worth checking
against the tree, because Apiary is not starting from zero here and is
not starting from the place the sketch assumes.

Three things are true today that the sketch does not account for.

**1. Something already polls disks.** `internal/hoststats` has collected
per-disk SMART identity and health for every Comb since ADR-0018. It
shells out to FreeBSD's own `smart(8)`, and it works. The
`/host` page already renders a Disks table. The problem ADR-0127
names — "no disk health visibility" — is therefore not accurate. What
is accurate is narrower and sharper: what exists is a single bit.

**2. That single bit is already built on the exact collapse this Colony
has spent a decade of ADRs refusing.** `internal/hoststats.DiskInfo`
carries `Healthy bool` plus `Error string`, and
`api/rpc/manager.proto`'s `DiskStats` carries the same pair on the
wire. The template `web/templates/host.html` renders three cases off
it. This is a
bool-and-error-string, and a bool is a shape that cannot represent "the
vendor says this disk is degrading but has not yet pre-failed." ADR-0056
established that a Healthy verdict must mean recently *proven by cited
evidence*; `Healthy: false` here means "we got output and the bit was
not OK," and everything else collapses into `Error`, which the page
renders as `unknown`. The three rendered states are accidentally
correct and accidentally under-specified: there is no `degrading`, and
the two negative states are not distinguished in the data model, only in
the template's if/else.

**3. `smartctl` is not the tool that is already in use, and may not be
present at all.** `internal/hoststats` uses `smart(8)` from pkgbase.
ADR-0127's sketch says `smartctl -a`, which is `smartmontools`, a
*different* package with a *different* output format, a *different*
exit-code contract, and — critically — which is not in FreeBSD's base
pkgbase set. The tree contains no reference to `smartctl` or
`smartmontools` anywhere.

That third point is the whole design. A monitoring feature whose
dependency may legitimately be absent, whose output format varies by
vendor, by transport, and by device class, and which can fail in
half-a-dozen ways that are all indistinguishable from each other unless
someone deliberately separates them, is a feature where the difference
between "I could not tell" and "it is fine" is the only thing that
matters. Get that wrong and the feature actively harms the operator:
a page that says `FAILING` for every disk on a host that simply does
not have the tool installed is a page that gets ignored, and once it
is ignored it cannot report a genuinely dying disk either.

This ADR is the spine of that distinction, and the rest of it is
scope discipline around it: which devices, what to do with the output,
what is local and what is shared, and — the loudest question — whether
any of this is ever allowed to *do* something.

The scope of this ADR is observability. It is not a prediction engine,
not a failure-prediction model, not a remediation controller, and not a
ZFS vdev health monitor. Where those boundaries are drawn is stated
explicitly below rather than left to be discovered in review.

## What already exists

### `internal/hoststats` — the real prior art

`internal/hoststats/stats.go` is 300-plus lines whose package comment
already names the design this ADR builds on: each subsystem gathered
independently via FreeBSD's own tools, shelling out and parsing kept
separate so the `parse*` functions can be unit-tested against fixtures
on any OS.

The disk path, in full:

- `gatherDisks` enumerates `sysctl -n kern.disks` and splits on
  whitespace.
- `gatherOneDisk` runs `smart -i /dev/<name>` for model and serial
  (`parseSmartInfo`), then `smart -d /dev/<name>` for the overall bit
  (`parseSmartStatus`).
- Any error from either call sets `info.Error` and returns early.
- If the second call succeeds but no `SMART Status` line is found,
  `info.Error` is set to the literal string
  `"no SMART Status line in smart(8) output"`.

Three properties of this code are load-bearing and are preserved:

- **Per-disk independence.** One disk's failure never blanks the others,
  and never blanks CPU/memory/network — the `Snapshot.Errors` doc
  comment states this is deliberate.
- **Best-effort snapshot semantics.** `Snapshot` is explicitly a
  point-in-time best-effort view, and the per-subsystem error list is
  the mechanism.
- **Node-local, never replicated.** The package comment says it
  outright: "physical, per-node data - never replicated through raft."

The one property this ADR *changes* is the last one's consequence for
the verdict: `gatherOneDisk` today can produce a `DiskInfo` with
`Model` and `Serial` populated, `Healthy: false`, and `Error` empty —
which the template renders as `FAILING` on a device that merely reported
a non-zero status. That is a real and currently-reachable rendering,
not a hypothetical.

### The wire shape

`api/rpc/manager.proto`:

- `rpc HostStats(HostStatsRequest) returns (HostStatsResponse)` (line
  138) — documented as a point-in-time snapshot of *this* node, never
  forwarded through raft.
- `message DiskStats` (line 1175) — `name`, `model`, `serial`,
  `bool healthy`, `string error`. The comment on `error` is the
  important line: "healthy is meaningless in that case, not a false
  'known bad' signal." The intent is already right. The data model
  does not enforce it; it relies on every consumer remembering.
- `HostStatsResponse.disks` is field 5; `errors` is field 7.

### The operator-visible surface today

`web/templates/host.html` (lines 39-60) renders a four-column Disks
table with exactly this precedence:

```
{{if .Error}}      unknown
{{else if .Healthy}}  healthy   (class="success")
{{else}}           FAILING     (class="error")
```

`internal/frontend/convert.go` builds the `diskView` (line ~782) from
`DiskStats` field-for-field, carrying `Healthy bool` and `Error string`
across unchanged.

So: three visually distinct states already exist in the rendered page.
What does not exist is a fourth (`degrading`), a data model that makes
the distinction structural, or any attribute-level detail at all.

### `internal/assumptions` — the pattern this reuses

ADR-0055's `internal/assumptions` is the closest existing analogue to
"a per-node, periodically-recomputed, four-state, never-replicated
observation," and the discipline in `internal/assumecheck/checker.go` is
what a new monitor in this codebase is expected to look like:

- `assumptions.Status` is explicitly four-state and "never collapsed to
  a boolean": `true`, `false`, `unknown`, `not_applicable`. The
  `not_applicable` doc comment insists it is "a real, positive fact
  about scope, not an unresolved measurement," and that it "must never
  be conflated with `StatusUnknown` by any producer or consumer."
- `assumptions.Key` is a struct — `Kind`, `SubjectKind`, `SubjectID`,
  `DependencyID`, `Qualifier`, `ObservedByNodeID` — specifically so that
  "empty" is never confused with "absent."
- Persistence is split on purpose into a current **snapshot** (one
  `Result` per `Key`, `LastObservedAt` bumped every tick) and a
  **journal** (`HistoryEntry` written only on a
  `(ObservedStatus, ReasonCode)` transition or a heartbeat interval
  elapsing) "so '500 entries' reflects genuinely new evidence, not tick
  count."
- A file that fails to parse, or carries an unrecognized
  `schema_version`, is **renamed aside, never overwritten** by
  `Manager.quarantine` (`currentSchemaVersion = 2`). `Manager.Append`
  takes `historyLimit` and `maxAge` as its retention bounds, and
  `Manager.PurgeStale` already exists for age-based pruning.
- `SubjectKind` is a separate namespace because "a VM and a jail can
  share a literal id string." `SubjectKindNode`, `SubjectKindVM` exist;
  `SubjectKindJail` is declared and documented as reserved for a future
  check.
- The package's own comment states the governing principle: physical
  per-node observational data, "never replicated through raft (a
  peer-reachability or NAT-uplink observation is only ever meaningful
  to the node that made it)."

`internal/assumecheck/checker.go` adds the operational discipline:

- A stable, small reason-code vocabulary, "never used for human-readable
  prose (that's `Detail`'s job)," so a history entry's transition
  identity does not depend on free-text error strings "which change even
  when nothing meaningful does."
- One bounded `RunDeadline` per tick, so "one unresponsive peer can't
  stall subsequent ticks."
- Per-tick caching of repeated calls.
- `RunOnce` attempts each check independently: "a failure gathering
  one never discards results already gathered from another," and makes
  exactly one `Store.Append` at the end.

`api/rpc/manager.proto` line 2515's `AssumptionKind` enum currently
runs `ASSUMPTION_KIND_UNSPECIFIED = 0` through
`ASSUMPTION_KIND_REPLICA_NETWORK_BRIDGE_UP = 5`. Adding a kind is
additive and non-breaking; ADR-0129 (sibling, PF firewall management)
added three new `assumptions.Kind` constants and three new
`ASSUMPTION_KIND_*` enum values without disturbing any existing verdict
path, and this ADR follows that precedent exactly.

### `internal/health` — the verdict consumer, and the thing to avoid

ADR-0056's `internal/health` is a pure package: `ComputeNodeHealth(s
NodeSignals, now time.Time) NodeHealth`, no I/O, no state, every fact
supplied by the caller. Its `Status` is five-state —
`healthy`, `degraded`, `unknown`, `stale`, `contradictory` — and its
own doc comment insists "Healthy is never the default."

`compute.go` walks a fixed precedence chain, and every branch is a
separately-tested rule. The two that matter most here:

- Step 2: a node that raft counts as a `Voter` but that cannot actually
  be reached right now is `StatusContradictory`.
- Step 3: raft's own uncertainty about a member's suffrage "must never
  be promoted to Healthy, regardless of reachability."

`NodeSignals` is a struct of *control-plane* facts: peer reachability,
heartbeat, membership, suffrage, raft log indices, reconciler
timestamps. The `AppliedIndex`/`LastLogIndex` comment is a precedent
worth citing directly: those fields are "genuinely per-node," and
`ComputeNodeHealth` "deliberately does NOT derive any verdict from
these" because the underlying number is not what it appears to be, so
"any numeric threshold here would be fabricated precision, not
evidence."

A dying disk is exactly that shape of fact. This ADR does not put it in
`NodeSignals`.

### `internal/cluster/simulate.go` — the verdict vocabulary to match

`RecoveryVerdict` is a five-value string type with per-value doc
comments that make the semantics explicit and non-overlapping:
`unprotected`, `unverified_replica`, `replica_in_sync`,
`replica_out_of_sync`, `replica_unobserved`. The `replica_unobserved`
comment is the house statement of this ADR's central property:

> This is a real third state and must never be silently folded into
> either in-sync or out-of-sync - "we could not check" is not "checked
> and fine", and it is not "checked and broken" either.

The neighbouring distinction is worth preserving too:
`unverified_replica` means "no live observation was even attempted";
`replica_unobserved` means "a query WAS attempted and did not return a
usable answer." Being unable to try is a different fact from trying and
failing, and this ADR keeps both.

### `internal/coverage` — the Resilience Coverage Map

ADR-0062's `internal/coverage` is pure classification, importing
`internal/invariant` and `internal/recovery` and never
`internal/cluster` or `internal/manager` (both of which pull in heavy
OS-exec dependencies). Its scenarios are *node-failure* scenarios, and
its own doc comment notes that `StatusPhysicallyRehearsed` and
`StatusStale` are "permanently unreachable in v1."

### Not in the tree

No `smartctl`. No `smartmontools`. No disk-health assumption kind. No
raft state for disks. No self-test scheduling. No prediction. Grep
confirms zero hits for `smartctl` or `smartmontools` outside this
ADR's own discussion of them.

### Whether `smartctl` exists on the testbed

Attempted, from the environment this ADR was written in, and it
**failed**: `ssh root@10.90.0.94` returns
`Permission denied (publickey,keyboard-interactive)`. So whether
`smartmontools` is installed on brood or drone is **unknown**, and this
ADR is written to be correct either way. It is worth being explicit
that this is not a gap in the design — "the interrogator is not present"
is the ADR's central case, and a design that required the tool to be
present to be correct would have no central case at all. Establishing
the answer is the first implementation step, not a precondition for
this decision.

## Decision

S.M.A.R.T. disk health is a **node-local, four-state, per-device
observation**, polled by managerd, recorded in
`internal/assumptions`-style node-local state, exposed through one
new read-only node-local RPC, rendered per Comb and aggregated into a
Colony-wide *view* that is computed per request and never stored.

Nothing about an individual disk is raft-replicated. Nothing in this
feature ever performs a remediation. The default is off.

### The central property: silence is a state with its own verdict

A device is in one of exactly four states, and the fourth is not an
error path — it is a first-class verdict with its own reason codes,
its own colour, its own row in every table, and its own entry in the
journal:

| State | Meaning | Requires |
| --- | --- | --- |
| `passing` | The device was interrogated successfully and reports itself healthy, with no tracked attribute crossing an early-warning threshold. | A parsed observation with a real timestamp. |
| `degrading` | The device was interrogated successfully and is still self-reporting healthy, but at least one tracked attribute has crossed the configured early-warning threshold, strictly *above* the vendor's own failure threshold. | A parsed observation with a real timestamp and at least one named attribute. |
| `failing` | The device reported its own pre-fail: the ATA overall-health bit is not OK, or an NVMe critical-warning bit is set, or `percentage_used` has reached its limit, or a completed self-test reported failure. | A parsed observation, and a specific named cause. |
| `unknown` | The device could not be interrogated. `smartctl` is absent, not permitted, not on `PATH`, or returned a non-zero exit, timed out, or produced output no parser recognizes. | **Nothing is known. Nothing is concluded.** |

The rules that make this a safety property rather than a table:

1. **`unknown` is never `false`.** There is no code path — in the
   collector, the parser, the RPC handler, the frontend converter, or
   the template — that turns a failure to interrogate into `failing`.
   A regression test asserts this directly, in the shape of a test that
   fails if anyone ever writes `if err != nil { state = FAILING }`.
2. **`unknown` is never `passing`.** Absence of a complaint is not
   health. A `passing` verdict requires a successful parse; it is
   never inferred, never defaulted, never carried forward.
3. **Last-known-good does not decay into a current verdict.** If the
   newest poll failed, the state is `unknown` and the previous
   `passing` observation is reported *alongside* it in a separate
   `last_passing_at` field, visibly dated. History is evidence about
   the past, never a substitute for evidence about the present.
4. **Fail-visible, not fail-closed.** A missing `smartctl` produces
   `unknown` and a prominent explanation, not `failing`. This is
   deliberate and is argued under Rejected alternatives, because
   fail-closed is the more defensible-sounding instinct and it is
   wrong here.
5. **The reason code is the payload.** Every `unknown` carries a
   stable reason code from a closed vocabulary, following
   `internal/assumecheck`'s rule that reason codes must not be
   free-text error strings. `Detail` is human prose and is clamped.
6. **Being unable to try is distinct from trying and failing.** This
   is the `unverified_replica` / `replica_unobserved` distinction from
   `internal/cluster/simulate.go`, preserved: "interrogator not
   installed" and "interrogator ran and could not read the device" are
   different facts with different fixes, and an operator needs to tell
   them apart at a glance.

This is not a new idea to this codebase. It is ADR-0056's "Healthy
means proven, never merely unreported," ADR-0118's refusal to
manufacture a quorum blocker from non-quorum evidence, and
`RecoveryVerdictReplicaUnobserved`'s own doc comment, applied to a
subsystem that has a much more obvious opportunity to get it wrong
than any of them did — because a hard drive is a thing people expect
to fail, and it is genuinely tempting to design the feature so that it
does.

### Device scope

**In scope**, enumerated in this order:

1. **ZFS pool member devices.** Read from `zpool status -P` (or
   `zpool list -v`), which yields both the leaf device names and the
   pool name and the vdev role (`data`, `log`, `spare`, `cache`,
   `mirror`, `raidz*`). These are the highest-priority devices: a
   member disk that is degrading is a redundancy fact about a pool that
   holds Cells. This is the only source that attaches a device to a
   pool, and it is the only reason this feature exists in Phase 4
   rather than in a hobby.
2. **Every other block device in `sysctl -n kern.disks`** that is not
   excluded below — standalone data disks, and NVMe namespaces that are
   not in a pool. This is the existing `internal/hoststats`
   enumeration, kept as-is for continuity.
3. **NVMe**, in both of its forms: namespace devices
   (`nvd0`/`nvme0n1`) via `smartctl -d nvme`, and — as a distinct
   question, not part of v1 — the controller itself.

**Explicitly out of scope**, and silently skipped rather than polled
and reported as `unknown`:

- **ZFS zvols** (`/dev/zvol/...`, `nvd0p1`). These are filesystems,
  not devices. Polling them produces noise.
- **Loop devices** (`loop0`), **CD-ROM** (`acd0`), **NVDIMMs** (`nvd*`
  on a memory stick, distinguished from NVMe by transport, not by
  name).
- **USB-attached storage.** Rejected on a specific ground, not a
  general one: USB bridges re-enumerate, device names and even serial
  numbers are not stable across a re-plug, so a per-device journal
  keyed on device identity would accumulate entries for devices that no
  longer exist and could attribute a warning to the wrong physical
  drive. A disk whose identity is not stable cannot be journaled.
- **virtio/blk frontends inside jails**, and **bhyve guest disks** —
  the host's SMART bit says nothing about a guest's virtual disk.
- **iSCSI targets and remote block storage.** They are not local
  devices; the host cannot ask them anything.
- **Hardware-RAID logical volumes** (`md0` and friends) are polled, but
  only as whole-array devices: there are no per-member attributes
  behind the controller, and pretending otherwise would be the same
  class of error as inventing an RPO from HAST dirty extents.
- **S.M.A.R.T. self-test execution.** This ADR reads the self-test log
  if one exists. It never starts a test. See Rejected alternatives.

**What is not in scope for the disk layer, because ZFS already
reports it:** vdev health. `internal/hoststats.PoolInfo.Health` already
carries `zpool list -Hp`'s own health string, and
`web/templates/host.html` already colours it `ONLINE` against an error
class. A `DEGRADED` pool is already visible. This ADR is about the
*device underneath* the member vdev, which ZFS will not tell you about
until the member is already gone. The two layers are adjacent and are
deliberately not merged.

### Attribute normalization

S.M.A.R.T. attribute tables are vendor-specific. There is no single
model that covers ATA and NVMe, and pretending otherwise is the main
thing this section exists to prevent.

**Two layers, and only the first is vendor-normalized.**

*Layer 1 — the vendor's own verdict.* Every ATA device reports an
overall-health self-assessment (the `smartctl -H` / `SMART Status`
bit). Every NVMe device reports a health value in its SMART/Health
Information Log. This is genuinely cross-vendor and it is a
pass/fail boolean. It is authoritative — when it says fail, the disk
says it is failing — but a single bit cannot express "this disk is
getting worse," which is the entire reason to run a poll more than
once. So it is necessary and not sufficient.

*Layer 2 — per-attribute normalization.* Keyed on the attribute
**ID**, not on its name. Names are the vendor's; IDs are assigned in
ACS-3, and every mainstream ATA vendor honours the assigned ones. A
small set is treated as universal because the standard assigns them:

| ID | Meaning | Used for |
| --- | --- | --- |
| 5 | Reallocated Sector Count | Hard `failing` at the vendor threshold; `degrading` on early warning. |
| 9 | Power-On Hours | Age context only; never a verdict input. |
| 187 | Reported Uncorrect | `degrading` on any nonzero growth. |
| 188 | Command Timeout | `degrading` only. Never `failing` on its own — it also rises from controller and cabling problems. |
| 190 / 194 | Airflow / Temperature Celsius | Temperature cap as configured; vendor-dependent which ID appears. |
| 197 | Current Pending Sector | `degrading`; `failing` at threshold. |
| 198 | Offline Uncorrectable | `failing` at threshold. |
| 199 | UDMA CRC Error Count | Advisory only, and explicitly labelled as a cable-or-controller signal, not a disk signal. |
| 177 / 231 | Wear Leveling Count / SSD Life Left | Wear as a percentage; `degrading` on configured remaining-life floor. |

Raw values are **never compared across vendors or across
attributes**. The comparison is always normalized: the attribute's raw
value against *its own row's* `VALUE` and `THRESHOLD` columns,
expressed as remaining life. A `RAW_VALUE` of 3 means nothing on its
own; a `NORMALIZED_VALUE` of 12 out of 100 with a threshold of 10
means a specific, comparable thing.

**The rule for an unknown vendor.** If a device reports an
overall-health bit and we recognize none of the universal IDs in its
table, the verdict is still `passing` — the vendor's own bit is
authoritative and we have no evidence to contradict it — but the
attribute table is recorded as
**not normalized**, `attributes_parsed` is false, and a distinct reason
code `attributes_unrecognized` is emitted. The operator sees that Apiary
is trusting exactly one vendor bit and nothing else. We do not guess
which column means what, we do not match on attribute *name* text
(`grep`ing for a label is how a Chinese-vendor rebrand silently starts
matching the wrong attribute), and we do not degrade the verdict to
`unknown` — that would be reporting our own parsing gap as the disk's
health.

**NVMe is a different shape, not a different vendor.** The NVMe
SMART/Health Information Log's fields *are* standardized
(NVMe 1.3+), so the situation inverts: for NVMe the container and the
contents are both universal, and the open question is not "what does
this column mean" but "does this device populate it." Apiary
therefore has **two parsers, not one, with no shared code path**:

- `critical_warning` — a bitfield: spare below threshold (0x01),
  temperature exceeded (0x02), reliability degraded (0x04), media in
  read-only mode (0x08), volatile memory backup failed (0x10). Any
  nonzero value is `failing`, and the specific bit is named in the
  detail. 0x08 in particular is not a warning, it is the device
  refusing all writes.
- `percentage_used` — `>= 100` is `failing`; `>= 80` is `degrading`
  by default.
- `available_spare` against `available_spare_threshold` — a ratio
  below threshold is `failing`.
- `media_errors` — nonzero is `degrading`; growth across polls is the
  interesting signal, and this is the one place a *delta* is
  meaningful, because the absolute value is often already nonzero on a
  healthy drive.
- `unsafe_shutdowns` — reported as context, never a verdict input.

**If an ATA parser is handed NVMe output, or vice versa, the result is
`unknown` with `unsupported_transport` — never a plausible-looking
garbage verdict.** A parser that guesses is worse than a parser that
declines, because a guess that produces `degrading` is
indistinguishable from a real finding once it reaches a journal.

### What is node-local, what is Colony-replicated, and why

**Node-local, never replicated:**

- Every per-device observation. `da0` on brood and `da0` on drone are
  different physical objects with different serials and different
  failure histories. Replicating them would put per-node disk serials
  into the raft log, into every snapshot and every backup forever, and
  would cause a restored node to come up believing it has another
  node's disks. That is fabrication of exactly the kind ADR-0118 and
  ADR-0116 exist to prevent, committed at the storage layer where it
  would be hardest to notice.
- Every attribute value, self-test entry, and vendor-health bit.
- The poll's own timing, duration, and deferral decisions.
- The per-device history journal, which lives in the node-local
  assumptions-style store.

**Replicated: nothing. This ADR adds zero raft state keys, zero
`state.proto` messages, and zero `FSMApplyResult` fields.** That is a
decision, not an omission, and it is the one place where the
proportionality of this ADR is most clearly visible.

The reason is that the *interesting* thing about disk health is the
aggregate, and an aggregate is a **rendering over per-node answers**,
not a fact that needs to be durable. It is computed fresh on request
from each Comb's own last-observed local state. This is precisely the
shape ADR-0122's evidence API and ADR-0119's
`GetLocalHASTResourceStatus` already have, and it inherits their
properties for free: no leader
involvement, no forwarding, no staleness class of its own beyond what
each node's own `observed_at` already tells us.

**The one thing that looks like it should be replicated, and is not,
in v1:** a durable record that *a specific disk has already failed*,
because that is the one disk fact that must outlive the disk. The
answer is no, and the reason is uncomfortable but clear: the only
writer of that record would be a node that is, by definition, already
failing — and a failing node's write to the raft log is precisely the
write that may not be applied. Recording it in the node-local journal
means the record is lost exactly when it matters most. v1 records it
locally and accepts the loss; if operators want that record, it is a
separate ADR with a separate durability argument, not a field added
here.

**Thresholds are not replicated either.** They are managerd startup
flags — node-local, matching the existing `-reconcile-interval` and
`-hast-enabled` convention — with sane built-in defaults, so a Colony
with zero configuration still gets useful monitoring. Adding a
new raft-replicated configuration surface for a monitoring threshold
would be a disproportionate amount of new replicated state for a value
that is safe to set per-node and stale for hours without harm.

### Feeding the Colony health surface without fabricating quorum
problems

This is the constraint the sibling ADRs have been strictest about, and
it is enforced here by *addition only*.

**`internal/health` is not modified.** No new `NodeSignals` field, no
new branch in `ComputeNodeHealth`, no change to its five-state `Status`
or its precedence chain. A `failing` disk on a Comb never produces
`StatusContradictory`, never changes `StatusHealthy`, and never
appears as a quorum signal — because it is not one. A node with a
dying disk is still a fully participating voter, still reachable, and
still holds its entire raft log. A `contradictory` verdict, by
definition, is about the disagreement between what raft *counts* and
what the network can *reach*, and a disk is in neither category.
Weakening that chain to accommodate a hardware fact would be a
category error with quorum blast radius.

**A separate pure function, a separate vocabulary.** Disk health gets
its own classification in `internal/smart`, called by the frontend
handler alongside `health.ComputeNodeHealth`, never inside it. Its
four-state vocabulary is deliberately *not* `health.Status` and
deliberately *not* `cluster.RecoveryVerdict`. This codebase already
runs three non-interchangeable verdict vocabularies
(`health.Status`, `assumptions.Status`, `RecoveryVerdict`) and has
never had a bug from having several — precisely because none of them
share a type. That convention is preserved: the new state's name,
`degrading`, does not collide with `health`'s `degraded`, and a string
comparison between the two is a compile error rather than a silent
misclassification.

**`internal/coverage` is not modified.** Its scenarios are node-failure
scenarios. A single failed member vdev is a pool-resilience question,
not a Colony-resilience question, and its `StatusPhysicallyRehearsed`
/ `StatusStale` vocabulary would immediately go stale on a per-device
observation. Adding a "pool member degraded" scenario there in v1
would put a hardware fact into a map whose whole job is quorum-shaped
failure analysis. It is named here as an explicit non-goal, not
deferred and forgotten.

**`internal/cluster/simulate.go` is not modified.** No `RecoveryVerdict`
changes. A disk that is `failing` on the Comb owning a replica is
*not* evidence that replication is broken and is not evidence that
quorum is at risk; it is a reason for a human to look. The
"at-risk Cells" integration ADR-0127 sketches is therefore **deferred**,
and specifically: a Cell is not marked at-risk by a disk verdict in
v1, because establishing which Cells depend on which pool member is
real dependency-graph work with its own evidence rules, and guessing it
would put a hardware fact into a Cell-level safety claim. It is listed
under Open questions rather than quietly implemented.

**The `why_not` / blocker surface (ADR-0118) is not modified.** A
failing disk never appears in it.

**What the Colony-wide view actually is:** a card on the cluster
evidence page (ADR-0122) that lists, per Comb, the count of `passing`,
`degrading`, `failing`, and `unknown` devices, and a per-Comb link
down to the device detail. It is a *summary of per-node observations*,
labelled as such, positioned next to the node health verdict and
visually not merged with it. An operator who reads a `failing` count
must be unable to conclude anything about quorum, and the layout's job
is to make that unambiguous.

### Polling cadence and cost

Cost is real and it is not uniform. A `smartctl` attributes query
(`-A`) against a spinning SAS disk can take one to three seconds and
generates genuine device I/O, because SMART attribute reads are real
ATA commands, not cached registers. Twenty-four such disks polled in a
loop is a minute of avoidable load on every pass.

The rules:

- **Default interval: 3600s per device**, jittered ±10% so a Colony's
  Combs do not synchronise into a thundering herd of ATA commands.
- **Off by default.** Enabled by `-smart-poll-interval` on managerd;
  `0` (the default) disables polling, matching this codebase's existing
  convention that a zero or unset flag means "not configured here"
  rather than "configured to zero."
- **Budgeted: at most 8 devices per tick** (configurable), round-robin
  across ticks. A 24-disk Comb completes a full sweep in three hours.
  Freshness is therefore *per device* — every result carries its own
  `observed_at`, and the UI shows per-device age. There is deliberately
  no single "last scan" timestamp, because a per-device fact is being
  reported and a per-sweep timestamp would misrepresent it.
- **Single-flight, one device at a time.** `smartctl` holds a
  pass-through ATA/SATA controller while it runs; concurrent queries
  against the same controller can wedge it, and a wedged controller
  takes every disk behind it offline. One query at a time per Comb, no
  concurrency, with a per-device deadline (default 15s) and a per-tick
  total budget.
- **`-n standby` is NOT used.** It would make a parked disk report
  nothing, which by this ADR's own central property is `unknown` — and
  a monitor that silently skips the disk it most needs to see is the
  same bug as a health check that returns `false` on error. The wake-up
  cost is paid deliberately, hourly, and the cost of paying it is
  recorded rather than hidden.
- **No self-tests are started.** Short and extended tests are read from
  the device's own self-test log when one exists and are otherwise
  absent. See Rejected alternatives.

### Not piling load onto a struggling host

A poll that runs on a node already under stress — because a disk is
retrying, or because the reconciler is failing, or because the machine
is simply loaded — is load applied at the worst time. Three
degradation rules, each of which *records the deferral* rather than
hiding it:

1. **Node already unhealthy.** If this Comb's most recent reconcile
   attempt failed, or its own `health.Status` is anything other than
   `healthy` (the frontend already has both facts), the poller halves
   its budget. Deferrals are journaled with reason
   `poll_deferred_degraded`.
2. **Node loaded.** If the 1-minute load average exceeds a configured
   ceiling (default: 4.0, or 2× core count, whichever is higher), the
   poller skips the tick entirely. Reason `poll_deferred_load`. Load is
   already gathered by `internal/hoststats` and needs no new mechanism.
3. **Device already `failing`.** A device that has reported pre-fail is
   **not polled again** after the transition is recorded. Continuing to
   interrogate a dying disk is how one accelerates its own failure, and
   the operator's next action is to replace it, not to watch it more
   closely. This is a deliberate, visible stop, and the UI says
   `polling stopped — device reported pre-fail` rather than showing a
   stale age with no explanation.

Deferral never silently extends freshness. A device that has been
deferred three times shows an age that reflects the deferral, and after
a configured maximum age with no successful observation it moves to
`unknown` with reason `poll_stale` — because at that point "we have not
checked in a long time" genuinely *is* the honest state, and reporting
the last known verdict as if it were current would be the same
fabrication as carrying forward a `passing`.

### Evidence: three (four) visibly different states

What the operator actually sees, in `web/templates/host.html` and on
the new per-Comb disk page. The requirement is that no two of these
can be confused at a glance, including at a glance in a screenshot
pasted into a ticket.

**A disk that is degrading** — a `warning` class, not a `success` and
not an `error`:

```
da3    WDC WD40EFRX-68N    WD-WCC7K4XXXX    DEGRADING
       Reallocated_Sector_Ct  raw 42  normalized 31/100  threshold 10
       Current_Pending_Sector raw 3   normalized 97/100
       observed 47m ago
```

Every degrading row names the attributes that crossed the threshold
and shows raw value, normalized value, and the vendor's own threshold
side by side, so the operator can see the margin. It says how long ago
it was observed. It does not say how long it has to live, because
nothing here supports that number.

**A disk that has failed** — an `error` class, with the cause named:

```
da5    WDC WD40EFRX-68N    WD-WCC7K5YYYY    FAILING
       Overall health self-assessment: FAILED
       observed 1h ago — polling stopped
```

The named cause is either the vendor's own bit, an NVMe critical
warning with its specific bit decoded, `percentage_used >= 100`, or a
failed self-test. "Polling stopped" is stated, per the degradation
rule above, so the absence of a recent timestamp is explained rather
than looking like a bug.

**A disk whose health cannot be determined** — a distinct `unknown`
class, and explicitly *not* styled as a warning, because a disk that
cannot be interrogated is not a disk in trouble:

```
da7    <model unknown>     <serial unknown> UNKNOWN
       smartctl not found on PATH
       this Comb has no SMART monitoring available
       observed: never
```

```
da8    Samsung SSD 860    S3Z8NB0K123456   UNKNOWN
       smartctl exited 2: Smartctl open device: /dev/da8 failed:
       Permission denied
       observed 2h ago
```

The distinction between "not found on PATH" and "found but refused" is
the whole reason for the closed reason-code vocabulary: one is fixed
by installing a package, the other is fixed by a permissions or a
device problem, and conflating them wastes the operator's time on the
wrong fix.

**A healthy disk** — `success` class, with its age, and with the
attribute count shown so the reader can tell "checked and fine" from
"checked and only had the vendor bit":

```
da0    WDC WD40EFRX-68N    WD-WCC7K1ZZZZ    healthy
       12 attributes normalized
       observed 12m ago
```

**And the aggregate**, which is what prevents the four states above
from being read as four disconnected alarms:

```
brood    2 passing  1 degrading  1 failing  6 unknown
drone    4 passing  0 degrading  0 failing  0 unknown
```

`unknown` is shown as a count, not hidden behind a footnote, because
six unknown devices on a Colony is itself a finding — it means six
devices are not being watched, and that is a different operational
risk from one disk degrading.

### Automatic remediation: no, not in v1

**Decision: S.M.A.R.T. data may never drive a destructive action in
v1.** Not pool replacement, not vdev detach, not Cell evacuation, not
a scrub, not a self-test. The feature may print a recommendation
string and hand the operator the raw `smartctl` output to paste into
their own terminal. It never acts.

The argument, and the blast radius made explicit.

The signal is not trustworthy enough to be irreversible. It is parsed
by code that has never seen this vendor's output, over an attribute
table that is vendor-specific by construction, on hardware in a pool
holding production data, from a tool that may not even be installed.
The central property of this ADR is that we cannot reliably distinguish
"this disk is failing" from "we could not read this disk" — which is
precisely the distinction that must be *perfect* before an automated
system is allowed to destroy a drive on the strength of it. A design
that admits its own most important ambiguity should not be wiring
itself to irreversible consequences.

The failure modes are severe and asymmetric:

- **A false `failing` on the wrong device** triggers replacement of a
  *healthy* disk. That destroys a good drive, extends downtime, and —
  in an already-degraded pool — turns a survivable single fault into a
  double fault. This is the scenario that must never be possible, and
  it is the one a mis-parsed vendor table produces.
- **A vdev-name/da-name confusion** replaces the wrong member. Pool
  names, vdev names, and `daN` names are three namespaces, and `daN`
  assignment is not stable across reboots. Any mapping bug here is
  silent and destructive.
- **A Cell evacuation triggered by a spurious disk signal** tears down
  a running workload mid-flight, and on a Comb whose memory is already
  stressed by a failing disk's retry storm, the evacuation itself can
  fail partway — leaving a Cell in a worse state than the disk that
  triggered it.
- **A degraded-but-not-failed pool plus an automated replace** is a
  common enough operator mistake that automating it makes the mistake
  cluster-wide and simultaneous.

The precedent is established and recent: ADR-0119's Maintenance Wave
Planner is explicitly read-only — "it does not restart services,
migrate workloads, or change Raft membership" — precisely because a
planner that acts on unconfirmed evidence is worse than no planner.
This feature follows the same rule for the same reason. A read-only
planner that is wrong wastes an operator's ten minutes. An automated
remediator that is wrong destroys a pool.

The one thing that can be automated safely is the *recommendation*.
"Pool `tank` has a member reporting pre-fail; redundancy is currently
`DEGRADED`; consider `zpool status tank` and a manual `zpool replace`"
is a sentence, not an action, and it is the whole of v1's
"integration with failure-domain integrity" from ADR-0127.

## Rejected alternatives

### 1. Poll on the leader and forward results to the rest of the Colony

Rejected. The leader cannot see another Comb's disks; forwarding means
asking peers to run privileged commands on their behalf, and the
result is a leader-authoritative view of hardware it never touched. It
also puts disk serials into the raft log, which is the fabrication
problem stated above. This is the shape ADR-0119's HAST status RPC
already rejected, and the reasoning transfers unchanged.

### 2. Reuse `assumptions.Status` unchanged for disk health

Rejected, while still reusing `internal/assumptions` for a different
and better-fitting thing. `assumptions.Status` is a boolean plus
its two escape hatches; disk health needs a *third positive state*
(`degrading`), which a boolean cannot express. `not_applicable` is
also the wrong axis — it means "this question does not apply here,"
whereas an uninterrogatable disk is a question that applies and
cannot be answered.

What is reused instead: a **new `assumptions.Kind` for the monitor
itself**, not for the disk. `disk_health_monitor_available` with
`SubjectKind: assumptions.SubjectKindNode` and the ordinary
true/false/unknown semantics: is a SMART-capable interrogator present
and permitted on this Comb. `false` here is a real affirmative
negative — `PATH` was read successfully and contained no `smartctl` —
which is *not* the same as a disk's health and never touches the disk
verdict. The resolution is elegant because it splits the question in
two: **the monitor is an assumption; the disk is not.** It also means
the existing `/assumptions` page answers "why is every disk on this
Comb unknown?" without any new UI, following the ADR-0129 precedent of
adding assumption kinds without disturbing the verdict chain.

### 3. Add disk signals to `health.NodeSignals`

Rejected. Covered above under "Feeding the Colony health surface." A
`failing` disk cannot legitimately produce `contradictory`, `stale`,
or `degraded` for the *control plane*, and letting it try risks a quorum
claim derived from local hardware. The `AppliedIndex`/`LastLogIndex`
comment is the direct precedent for refusing a threshold on a number
that does not mean what it appears to mean.

### 4. Replicate per-device records through raft

Rejected. Per-device data is physical per-node data, and this codebase
does not replicate it: `internal/hoststats` and `internal/assumptions`
both say so in their own package comments, and `internal/hoststats`'s
rationale ("only ever meaningful to the node that made it") applies
with full force to a
`da0`. The Colony view is a rendering, and renderings are computed per
request. A consistent-replica argument for hardware telemetry is
usually really an argument for a time-series database, which this is
not.

### 5. Predict failure: trend models, time-to-failure, wear curves

Rejected. It would require a corpus of real failure histories that
does not exist in this project or on the testbed, and it would produce
precisely the "fabricated precision" ADR-0056's `NodeSignals` comment
warns against: a number with a confident-looking unit and no evidence
behind it. Deltas between consecutive real observations (media errors
growing, reallocated sectors growing) are evidence and are surfaced.
Extrapolating those to "14 days remaining" is not. The scope of this
ADR is observability, and the task framing agrees.

### 6. Run short and long self-tests automatically

Rejected. A short test takes minutes and a long test takes hours,
during which the disk is under sustained load; an unattended long test
on a pool member is an availability risk, not a health check. It would
also be load applied by a monitor, on a host that may not be able to
afford it, which is the thing the degradation rules exist to prevent.
v1 reads the self-test log and never writes to it. Scheduling tests is
properly a Flight Plan concern — an operator-initiated, reviewed,
scheduled action — and out of scope here.

### 7. Fail closed: report `failing` when `smartctl` is missing

Rejected, and it is the most important rejection in this ADR,
because fail-closed is the more defensible-sounding engineering
instinct and it is wrong here. The argument is not that the
information is worthless when the tool is absent — it is that this
specific UI is the one that will be ignored.

`smartmontools` is not in FreeBSD's base pkgbase set, and this
project's own `internal/hoststats` uses `smart(8)` instead, precisely
because `smart(8)` is. So the default state of a stock, correctly
installed Apiary Comb is "the monitor's preferred tool is not
present." Fail-closed there means a fresh install renders every disk on
every Comb as `FAILING`, in red, on the first page load. Operators
will learn within one incident that this page is always red, and a page
that is always red reports nothing — including the genuine pre-fail of
`da5` on brood, which is the one thing the feature was built to catch.

`unknown` in red-adjacent styling, with a reason code and a one-line
fix, is the correct behaviour. It is loud. It is specific. It is
actionable. It does not lie about a disk to make a point about a
package.

## Consequences

### Positive

- The Colony gains real, per-device, per-node visibility into disk
  condition — the one hardware fact ZFS will not report until the
  member is already gone.
- `unknown` becomes a first-class, countable, colorized state
  everywhere, so "not being watched" is visible as a number rather
  than as an absence. Six unknown devices on a Colony is itself a
  finding.
- `internal/hoststats`'s bool-and-error-string is replaced on the disk
  path with a four-state model that *structurally* prevents the
  `Error`-means-`false` collapse, rather than relying on a doc comment
  and a template's if/else to remember.
- The same reason-code discipline, journal semantics, and
  quarantine-on-corrupt-file behaviour that ADR-0055 established carry
  over unchanged; nothing new is invented.
- A new assumption kind answers "why is nothing being reported here?"
  on the existing `/assumptions` page, with no new UI.
- No new replicated state, no new raft keys, no snapshot-format
  change, and therefore no new class of restore-time divergence.
- Parsers are pure functions over strings, so the whole normalization
  layer is testable on macOS against captured fixtures — following
  `internal/hoststats`'s own "shelling-out and parsing kept separate"
  design.
- The feature degrades safely under every condition: no tool, no
  permission, unparseable output, busy host, unsupported transport, or
  a wedged controller all produce `unknown` with a specific reason,
  and none of them produces a wrong verdict.

### Negative

- A stock FreeBSD Comb with neither `smart(8)`'s status line nor
  `smartmontools` installed will report `unknown` for its disks until
  an operator installs a package. This is a worse first impression
  than a green "healthy" table, and it is correct.
- Two parsers (ATA and NVMe) rather than one, because there is no
  honest single model. That is real maintenance surface.
- The universal-attribute table will be wrong for some vendor, and
  when it is, the fallback is "trust one bit and say so" rather than
  full analysis for that drive. This is a deliberate accuracy-for-
  coverage trade and it will be occasionally unsatisfying.
- Polling costs real I/O on spinning disks, one to three seconds per
  device. The per-tick budget and the degradation rules bound it but
  do not eliminate it.
- Adding a fourth verdict vocabulary to a codebase that already has
  three is a small ongoing tax: every future health-shaped feature has
  to choose, consciously, which of the four it is speaking in.
- The feature is off by default, so an operator who deploys it and
  forgets to set the flag sees nothing and must diagnose why. The flag
  name and the empty-state text on the page both have to say so.
- Per-device staleness means the UI needs per-device ages, which is
  more to render and more to explain than a single "last scan" line.
- ZFS vdev health is explicitly *not* covered. A `DEGRADED` pool from
  a non-disk cause (a lost path, a bad cable on a SAS expander) is
  outside this ADR, and operators looking for one place to answer
  "is my storage okay" will need to read two surfaces.

## Implementation notes

### New package: `internal/smart`

Pure, with shelling out and parsing separated, exactly as
`internal/hoststats` states for itself: "the parse* functions take
command output as a plain string," so parsing is testable on any OS
without a real FreeBSD host.

- `Runner` — the only type that executes anything. `Run(ctx, device)`
  returns `(stdout string, stderr string, err error)` for exactly one
  `smartctl` invocation. Single-flight, per-device deadline, no
  concurrency. A fake `Runner` is injected in tests, matching
  `internal/cluster`'s own `pfManager`/`vlanManager` fake convention.
- `parseATA(out string) (Parsed, error)` — the 20-attribute table and
  the overall-health line.
- `parseNVMe(out string) (Parsed, error)` — the SMART/Health
  Information Log.
- `Detect(out string) Format` — decides which parser applies, from
  the output's own shape. Returns `FormatUnknown` for anything else,
  which is `unknown`, never a guess.
- `Classify(p Parsed, thresholds Thresholds, now time.Time) Verdict` —
  pure. This is where `passing` / `degrading` / `failing` / `unknown`
  is decided, and where the closed reason codes are attached.
- `Enumerate(poolVdevs []Vdev, kernDisks []string) []Device` — pure
  filtering of the exclusion list, so the "what do we poll" decision
  is unit-testable without a `sysctl`.

Failure modes: `smartctl` not found on `PATH` (→ `unknown` /
`smartctl_not_installed`); non-zero exit with empty stdout (→
`unknown` / `smartctl_failed`, with the exit code in `Detail`);
non-zero exit *with* parseable stdout (→ parse what we got, and mark
`errors` non-empty rather than discarding the reading — some
`smartctl` builds exit non-zero for advisory warnings); output
truncated by the deadline (→ `unknown` / `smartctl_timeout`); output
in a shape neither parser recognizes (→ `unknown` /
`unsupported_transport`); device path present in `kern.disks` but gone
by the time we open it (→ `unknown` / `device_absent`, not a failure
of the disk, which may be perfectly fine and merely gone).

### Proto: `api/rpc/manager.proto`

New, appended after the existing `GetLocalHASTResourceStatus*`
messages (currently around line 1258). All additive; no existing field
is renumbered or removed.

```proto
// DiskSmartState is this ADR's four-state verdict. It is deliberately
// a different type from any other verdict in this file: it is NOT
// api/internalpb's node Status, NOT internal/health's five-state
// Status, and NOT internal/cluster's RecoveryVerdict. Do not reuse any
// of them, and do not compare their string values.
enum DiskSmartState {
  DISK_SMART_STATE_UNSPECIFIED = 0;
  DISK_SMART_STATE_PASSING = 1;
  DISK_SMART_STATE_DEGRADING = 2;
  DISK_SMART_STATE_FAILING = 3;
  // UNKNOWN means the device could not be interrogated at all. It is
  // never "failed" and never "fine" - see the ADR's central property.
  DISK_SMART_STATE_UNKNOWN = 4;
}

enum SmartAttributeSeverity {
  SMART_ATTRIBUTE_SEVERITY_UNSPECIFIED = 0;
  SMART_ATTRIBUTE_SEVERITY_INFO = 1;
  SMART_ATTRIBUTE_SEVERITY_ADVISORY = 2;
  SMART_ATTRIBUTE_SEVERITY_DEGRADING = 3;
  SMART_ATTRIBUTE_SEVERITY_FAILING = 4;
}

message SmartAttribute {
  uint32 id = 1;
  // name is the device's own verbatim label, never synthesized and
  // never matched against - matching on label text is how a vendor
  // rebrand silently starts reporting the wrong attribute.
  string name = 2;
  uint64 raw_value = 3;
  uint64 value_worst = 4;
  uint64 value_threshold = 5;
  uint64 value_typical = 6;
  // normalized_value is the ACS-3 1..253 scale; 0 with
  // normalized_ok false means this attribute could not be normalized,
  // and the verdict then rests on the vendor's own overall-health bit
  // alone.
  uint32 normalized_value = 7;
  bool normalized_ok = 8;
  SmartAttributeSeverity severity = 9;
  string unit = 10;   // "raw", "hours", "Celsius", "percent", ""
}

message SmartNvmeHealth {
  bool critical_warning = 1;
  uint32 critical_warning_flags = 2;   // the raw bitfield, decoded in Detail
  uint32 percentage_used = 3;
  uint32 available_spare = 4;
  uint32 available_spare_threshold = 5;
  uint32 media_errors = 6;
  uint32 num_err_log_entries = 7;
  uint32 unsafe_shutdowns = 8;
  uint32 power_on_hours = 9;
  uint32 power_cycles = 10;
  uint32 temperature_kelvin = 11;       // NVMe reports Kelvin
}

message SmartSelfTestEntry {
  string test_type = 1;                 // "short", "extended", vendor label
  uint32 status = 2;                    // verbatim numeric
  string status_text = 3;               // verbatim text, never translated
  int64 completed_at_unix = 4;          // 0 = never observed
}

message DiskSmartReport {
  string device = 1;                    // "da3"
  string pool = 2;                      // "" if not a pool member
  string vdev = 3;                      // "" if not a pool member
  string vdev_role = 4;                 // "data"|"log"|"spare"|"cache"|"standalone"|""
  string model = 5;
  string serial = 6;
  string firmware = 7;
  DiskSmartState state = 8;
  // reason_code is a stable, closed-vocabulary string - never a
  // free-text error message. See internal/assumecheck's own rule.
  string reason_code = 9;
  // detail is clamped human prose and is explicitly not a verdict input.
  string detail = 10;
  int64 observed_at_unix = 11;          // 0 = never observed
  // last_passing_at_unix is HISTORY, never a fallback for state: a
  // failed poll yields state UNKNOWN, not the previous PASSING.
  int64 last_passing_at_unix = 12;
  uint32 poll_duration_ms = 13;
  // vendor_health_bit is the device's own overall self-assessment. Only
  // meaningful when state != UNKNOWN.
  bool vendor_health_bit = 14;
  repeated SmartAttribute attributes = 15;
  // attributes_parsed false means the table was not normalized; the
  // verdict then rests on vendor_health_bit alone and reason_code says
  // so via attributes_unrecognized.
  bool attributes_parsed = 16;
  SmartNvmeHealth nvme = 17;
  repeated SmartSelfTestEntry self_tests = 18;
  // raw_output is the verbatim smartctl text, clamped to 8192 bytes
  // with the tail preserved (the overall-health line is at the top and
  // the self-test log at the bottom, and this ADR keeps both by
  // clamping the middle). Populated only on the local/serving-node RPC;
  // never on the Colony fan-out, which would multiply it by Combs.
  string raw_output = 19;
  bool raw_truncated = 20;
  // poll_stopped is true when polling was deliberately stopped because
  // this device reported pre-fail.
  bool poll_stopped = 21;
}
```

```proto
message GetLocalDiskSmartRequest {
  // refresh forces a poll subject to the degraded-node rules rather
  // than returning the last completed poll's stored result. It never
  // bypasses those rules: a degraded Comb that refreshes gets the same
  // deferral, and the response says so.
  bool refresh = 1;
}

message GetLocalDiskSmartResponse {
  string node_id = 1;
  repeated DiskSmartReport disks = 2;
  // monitor_available mirrors the disk_health_monitor_available
  // assumption on the wire, so one page load needs one call.
  bool monitor_available = 3;
  int64 poll_interval_seconds = 4;
  int64 last_poll_at_unix = 5;
  repeated string errors = 6;            // per-subsystem, like HostStats
}
```

`rpc GetLocalDiskSmart(GetLocalDiskSmartRequest) returns
(GetLocalDiskSmartResponse);` — placed next to `HostStats` (line 138)
and `GetLocalHASTResourceStatus` (line 143).

**Semantics: node-local, never forwarded through raft, never
leader-gated.** It answers for whoever receives it, like `HostStats`
and `GetLocalHASTResourceStatus`. It takes no lock, writes no state
that anyone else reads, and a `raftd` that is down or holding a lock
does not affect it — which is the ADR-0124 property (a diagnostic that
must never be blocked by the thing it is diagnosing) applied to a
second diagnostic.

`refresh` exists and defaults to false so that **a page load can never
trigger twenty-four three-second `smartctl` invocations** against a
busy Comb. The default is the stored last-observed result; the button
is the poll. This is the single most important thing to get right in
the handler.

**Deprecation of `DiskStats`:** `bool healthy` on `message DiskStats`
(line 1175) is deprecated in its comment, not removed. Removing it
would break `HostStats` for every existing consumer, and the field
continues to mean what it always meant — "the device reported a
non-zero status" — which remains correct as a summary. The four-state
truth lives in the new RPC. Deprecating in place, and leaving
`internal/hoststats`'s existing parsers and their tests untouched, is
the proportionate move.

### Raft state keys

**None. This ADR adds no `FSM` field, no `state.proto` message, no
`FSMApplyResult` field, and no `ExportState` change.** There is no
`/keys/disk-health/` to write down, and that absence is the design.

Failure mode of that decision: if the Colony's disk view must survive a
leader change mid-request, it will not — each node's contribution is
read from that node, and a node that is restarting contributes
`unknown` for the duration. That is the correct answer for a
node-local physical observation, and it matches what
`GetLocalHASTResourceStatus` already does.

### `internal/assumptions` additions

Additive, exactly as ADR-0129 did it:

- One new `Kind` in `internal/assumptions/manager.go`:
  `KindDiskHealthMonitorAvailable = "disk_health_monitor_available"`,
  with `SubjectKind: assumptions.SubjectKindNode`. The `Key` is the
  node's own — `SubjectID` left empty, matching how the
  peer-reachability and NAT-uplink checks key themselves.
- `ASSUMPTION_KIND_DISK_HEALTH_MONITOR_AVAILABLE = 6` in
  `api/rpc/manager.proto`'s `AssumptionKind` enum (currently 0-5),
  with the same comment shape as its neighbours.
- The producer is the new poller, **not** `internal/assumecheck` —
  `assumecheck` owns the three ADR-0055 checks and does not gain a
  fourth. A separate `internal/smart` writer calling
  `Store.Append` follows the same contract.
- `SubjectKindDevice` is added to `internal/assumptions`'s
  `SubjectKind` set **only if** per-device journal entries are
  persisted there; since v1 keeps the per-device journal node-local
  under the same store, this is where the namespace distinction goes —
  a `da0` on two Combs must not collide in any future fan-out. It is
  declared for that reason and documented as reserved, mirroring
  `SubjectKindJail`'s existing treatment.

Failure modes: `false` here is a real affirmative negative (`PATH` was
read successfully, `smartctl` was not in it). If `PATH` itself cannot
be read, the answer is `unknown`, not `false` — the third rejection in
this ADR is thereby prevented at the producer rather than at the
renderer. A corrupt assumptions file is quarantined by the existing
`Load` behaviour, which renames it aside rather than overwriting.

### managerd RPC handlers

In `internal/manager/server.go`:

- `GetLocalDiskSmart` — a nil-able handler field, matching the
  existing convention that a node with no `Reconciler` still answers
  other RPCs. When the poller is disabled (`-smart-poll-interval 0`),
  the handler still answers, with `monitor_available` derived from the
  assumption and every device `unknown` / `smart_monitoring_disabled` —
  never an error and never an empty list, because an empty list reads
  as "this Comb has no disks," which is a claim.
- `refresh` runs the poll inline on the caller's goroutine with the
  per-device deadline and the degradation rules applied, and returns
  whatever that produced. It never writes raft and never takes the
  cluster lock.
- `HostStats` is unchanged.

Failure modes: a device that disappears between enumeration and poll
is `unknown` / `device_absent`; a `smartctl` that ignores the context
deadline is bounded by killing the process (`exec.CommandContext`) and
marking `smartctl_timeout`, and the handler must not leave a zombie
holding the controller; a full tick budget must not turn one slow disk
into a skipped tick for the others.

### Reconcile loop

A new background loop in managerd, structurally a sibling of
`internal/assumecheck.Checker.RunOnce` — plain-Go dependency
injection, narrow local interfaces, nil-able fields, a bounded
`RunDeadline`, exactly one `Store.Append` per tick, and independent
per-device handling so one failure never discards another's result.

Per tick: enumerate (from the last known pool/vdev read plus
`kern.disks`), select up to `budget` devices round-robin, apply the
three degradation rules, poll each single-flight with a deadline,
classify, append.

The pool/vdev read is cached with its own, longer interval (default
hourly) and a bounded lifetime. If the pool read fails, the device
list falls back to the previous one with its `pool`/`vdev` fields
marked stale — devices are still polled, they just lose their
membership annotation. **Losing the pool association must never stop
the polling**: a disk with no pool is still a disk, and an outage of
`zpool` must not be allowed to silence the monitor.

Failure modes: a tick that exceeds `RunDeadline` stops taking new
devices and lets the remainder fall to the next tick, recording them
as deferred rather than silently skipped; journal growth is bounded by
the existing `HistoryLimit`/`HistoryMaxAge`; a device in `failing`
is polled once more (to capture the terminal reading) and then stopped.

### Frontend surface

- `web/templates/host.html`'s existing Disks table gains a fifth
  column, **Device state**, carrying the four-state class token, and
  keeps the existing table working for `Stats.Disks` (which still comes
  from `HostStats`). A new per-Comb page, `/host/disks`, renders the
  full per-device evidence: attributes, reason code, per-device age,
  `last_passing_at`, the self-test log, and the clamped raw output.
- `internal/frontend/convert.go` gains a `diskSmartView` alongside the
  existing `diskView`, and **the two must not share a type** — the
  existing `diskView` is a bool and the new one is a four-state, and
  letting them share is how `Error`-means-`false` creeps back in.
- The four classes must be four distinct tokens in
  `web/templates/host.html`: a renderer test asserts that the
  `UNKNOWN` and `FAILING` rows do not share a class attribute, so the
  distinction cannot be lost by a later template edit.
- The `/assumptions` page picks up the new kind with no changes at all
  — which is the point of routing the monitor-availability question
  through `internal/assumptions`.
- The cluster evidence page (ADR-0122) gains a Colony disk summary
  card: per-Comb counts of each of the four states, computed on request
  by fanning out `GetLocalDiskSmart` over the existing peer-forwarding
  path the same way `HostStats` is already fanned out. `unknown` is
  shown as a count, never suppressed. The card is visually and
  structurally separate from the node health verdict, and carries a
  line stating that disk health does not affect quorum — because
  someone will read it next to a health verdict and assume it does.

## Test plan

### Validatable on macOS, with no FreeBSD required

macOS cannot validate ZFS, HAST, PF, jail, bhyve, `smartctl`, or
real-device behaviour, and nothing below should be read as claiming to.

- `gofmt`, `go build ./...`, `go vet ./...`, `go test ./...`. The
  existing `internal/hoststats` `parseSmartInfo`/`parseSmartStatus`
  tests must pass **unchanged** — the migration is additive, and a
  change to their expectations is a signal this ADR went further than
  it meant to.
- **Parser fixtures — and these must be built from real captured
  output, not written from imagination.** This is a blocking
  prerequisite, and it is the first implementation task: capture real
  `smartctl -A -j` (or `-A`) and `smartctl -d nvme` output from a real
  spinning disk, a real SSD, and (if one exists) a real NVMe device,
  check the captured text into
  `internal/smart/testdata/`, and write the parsers against it. A
  parser written against hand-typed fixtures passes every test and
  fails on the first real drive. Capturing fixtures requires brood or
  drone and therefore **cannot** be done in this environment; see
  below.
- Parser unit cases from those fixtures: a well-formed ATA table; an
  ATA table with an unrecognized vendor's column set (→ passing with
  `attributes_unrecognized`); ATA output handed to the NVMe parser and
  vice versa (→ `unknown` / `unsupported_transport`, never a
  plausible-looking verdict); non-zero exit with parseable stdout
  (→ parse it, and set `errors`); non-zero exit with empty stdout;
  empty stdout because the binary is absent; output truncated
  mid-table; a device path that no longer exists; a `SMART Status`
  line that is neither 0 nor a digit.
- **The central invariant, as an explicit regression test:** for every
  fixture and every simulated failure, `state == FAILING` requires a
  named cause — a false vendor health bit, a set NVMe critical-warning
  bit, `percentage_used >= 100`, or a failed self-test. A test of the
  form "if any error path produces FAILING, fail" is the direct
  expression of the ADR's central property and should be written so
  that adding a new error path without updating it breaks the build.
- **Unknown is not false, and unknown is not true:** a table test over
  every `Reason*` constant asserting the state it produces is
  `UNKNOWN`, and a second asserting that `last_passing_at` is preserved
  as a *separate* field while `state` is `UNKNOWN`.
- **Freshness:** a device deferred past `max_stale` moves to
  `unknown` / `poll_stale`, and its previously recorded `degrading` is
  not carried forward.
- **Degradation rules:** with a failing reconcile, a load average over
  the ceiling, or a device already in `failing`, the tick defers and
  records the right reason — and `refresh` on a degraded Comb
  produces the same deferral, proving the request path cannot be used
  to bypass the rules.
- **Enumeration:** `kern.disks` contents naming a zvol, a loop
  device, a CD-ROM, and a `da0` that is a pool member produce exactly
  the right included/excluded set; a `daN` that has disappeared still
  appears with `device_absent`.
- **Proto round-trip:** a `DiskSmartReport` with a 20 KB
  `raw_output` round-trips to 8192 bytes with `raw_truncated` true and
  both the head and the tail of the original preserved; a report with
  `state: UNKNOWN` and no attributes round-trips unchanged.
- **Template:** a render test asserting the `DEGRADING`, `FAILING`, and
  `UNKNOWN` rows carry three distinct class tokens, and that the
  `UNKNOWN` row carries a reason code and does not carry the error
  class.
- **Assumption kind:** `disk_health_monitor_available` yields `true`
  when `PATH` is readable and contains `smartctl`, `false` when
  readable and absent, and **`unknown` when `PATH` itself cannot be
  read** — the last case being the regression test for rejected
  alternative 2.
- **No replication:** a test asserting `state.proto` gains no new
  message and the FSM gains no new field. This is unusual but cheap,
  and it is the only way to keep a later "just replicate it" change
  from landing silently.

### Only validatable on brood (10.90.0.94) or drone (10.90.0.95)

macOS has no ZFS, no HAST, no PF, no jail, no bhyve, no `smartctl`, and
no physical disks. Everything in this section is unverifiable from a
developer machine and must not be reported as verified until it has
been run on real hardware.

- **Establish whether `smartmontools` is installed on either host.**
  Attempted from the environment this ADR was written in and it
  **failed** — `ssh root@10.90.0.94` returns
  `Permission denied (publickey,keyboard-interactive)` — so the
  answer is currently *unknown*. This is the ADR's central case
  rather than a blocker, but it must be established before any
  decision about adding `smartmontools` to the package set.
- **Capture the real fixtures** described above, from real drives, and
  check them in. Until this is done the parsers are unvalidated, and
  the test plan's macOS half proves only that the code does what the
  code does.
- **Real `kern.disks` contents** on a real host, to check the
  exclusion list against what FreeBSD actually reports — including
  whether SAS expanders, NVMe, and the boot pool appear as expected.
- **Real `zpool status -P` output** to validate the vdev→device
  mapping, including mirror and raidz vdevs where a single `daN` is a
  member of a hierarchy rather than a leaf.
- **Actual `smartctl -A` wall time per device class**, to set the
  8-per-tick budget in the cadence section rather than guessing it.
  On a spinning SAS disk this is the number that decides whether the
  feature is affordable at all.
- **The polling-load question:** run a full sweep against a Comb under
  real load and measure whether it is perceptible. The degradation
  thresholds in this ADR are reasoned, not measured, and must be
  calibrated against observation.
- **Re-enumeration behaviour:** what `smartctl` actually returns for a
  zvol, a `md` array, and a CD-ROM, and whether any of them produces
  output that a parser would mistake for a real disk. This is the
  strongest argument for the `unsupported_transport` refusal and it
  should be demonstrated, not assumed.
- **NVMe:** requires an NVMe device. If neither host has one, the NVMe
  parser ships **fixture-only and explicitly unvalidated**, and the
  UI says so for NVMe devices rather than implying a working path.
  Do not ship it as though it were tested.
- **HAST interaction:** a `failing` disk on a Comb that owns a HAST
  primary, observed on both ends, to confirm the finding does **not**
  alter any `RecoveryVerdict` and does not appear in the why-not
  surface. This is the test that proves the central safety property
  holds end to end, and it is the one most worth writing.

## Open questions

1. **Package policy.** Is adding `smartmontools` to the dependency set
   Apiary installs on every Comb acceptable, or does the monitor stay
   strictly optional and a Combs' lack of it remain a permanent
   `unknown` with a fix hint? This is the one question whose answer
   changes the first-run experience, and it is a policy call, not a
   technical one.
2. **Should we use `smart(8)` instead of `smartctl`?** The tree
   already uses `smart(8)`, which *is* in pkgbase, and this ADR was
   written to `smartctl` because ADR-0127's sketch names it and
   because it gives the ATA/NVMe access `smart(8)` does not. If
   avoiding a new package matters more than NVMe coverage, `smart(8)`
   plus `-a` may be enough for v1 — at the cost of dropping the NVMe
   path entirely. Worth an explicit decision rather than a default.
3. **Default thresholds.** Are the proposed defaults acceptable as a
   global — ATA `degrading` at 20% remaining life, NVMe
   `percentage_used >= 80` for `degrading` and `>= 100` for `failing` —
   or is per-device-class configuration needed in v1? This ADR
   assumes no.
4. **Naming.** Is `degrading` the right token, given `degraded` and
   `healthy` already exist in `internal/health`? It is deliberately
   distinct to make accidental cross-vocabulary comparison a compile
   error rather than a silent bug, but the human-facing labels could
   still be aligned.
5. **Deactivating `DiskStats.healthy`.** It is a live field on a live
   RPC. Comment-level deprecation is proportionate; removing it is not.
   Confirm that is the right call, and confirm whether the existing
   `/host` Disks table should be replaced outright by the new page or
   kept alongside it.
6. **Colony-wide placement.** Should the four-state counts appear on
   the cluster evidence page (ADR-0122) at all, given the standing
   risk that a summary next to a health verdict will be read as a
   health input? It is useful and it is also the single most
   misreadable thing this ADR could add.
7. **ZFS vdev health.** Explicitly excluded here on the grounds that
   `internal/hoststats.PoolInfo.Health` already surfaces it. Is that
   the right boundary, or should vdev-level and device-level health be
   presented as one surface?
8. **At-risk Cells.** ADR-0127 proposes marking Cells at-risk when a
   pool member degrades. This ADR declines to, because establishing
   which Cells depend on which pool member is real dependency-graph
   work. Should that become a follow-up ADR, and does it belong to
   ADR-0121's dependency graph rather than here?
9. **Retention.** `Manager.Append` already takes `historyLimit` and
   `maxAge` and `Manager.PurgeStale` already prunes by age; what
   should the per-device journal's bounds be? An hourly poll for a
   year is ~8,700 *observations* per device even with no transitions
   at all, so the bound has to be chosen deliberately rather than
   inherited.
10. **Self-test scheduling.** v1 never starts one. Should an
    operator-initiated short test become a Flight Plan action later,
    or is it out of scope permanently?

## References

ADRs:

- **ADR-0018** — Host stats and multi-page UI. Origin of
  `internal/hoststats` and its per-disk SMART row.
- **ADR-0022** — Network management. Extended `internal/hoststats`
  with interface state and the PF summary.
- **ADR-0026** — HAST VM disk replication. Why Cell-level disk claims
  need their own evidence rules.
- **ADR-0052** / **ADR-0053** — Dependency graph simulator and managed
  network failure simulation. The quorum-simulation model this ADR
  explicitly does not extend.
- **ADR-0055** — Automated Assumption Checks v1. The
  `internal/assumptions` snapshot/journal model, four-state `Status`,
  and `Key` structure reused here.
- **ADR-0056** — Evidence-Aware Health v1. `internal/health`'s
  five-state `Status`, the "Healthy means proven" rule, and the
  "fabricated precision" precedent for refusing a threshold on a number
  that does not mean what it appears to.
- **ADR-0060** — Operational Invariants v1. `internal/invariant`, the
  other pure classification package.
- **ADR-0062** — Resilience Coverage Map v1. `internal/coverage`; why a
  per-device observation is not a node-failure scenario.
- **ADR-0116** — Quorum-safe raftd restart. The precedent against
  writing replicated state from a node that may already be failing.
- **ADR-0118** — Why not a quorum blocker detail. The governing
  constraint on manufacturing quorum claims from non-quorum evidence.
- **ADR-0119** — Replica freshness evidence and maintenance wave
  planner. `GetLocalHASTResourceStatus`'s node-local read pattern, and
  the read-only-planner precedent this ADR follows for remediation.
- **ADR-0121** — Dependency graph HAST sync evidence. Where the
  at-risk-Cells question belongs.
- **ADR-0122** — Cluster evidence-aware health API. The evidence surface
  the disk summary card sits next to.
- **ADR-0124** — Read-only offline raft status. The model for a
  diagnostic that must never take a lock or depend on the service it
  diagnoses.
- **ADR-0127** — Sylve.io feature adoption. The Phase 4 brief this ADR
  implements, and the source of several claims this ADR corrects.
- **ADR-0129** — Colony-wide PF firewall management (sibling
  worktree). The worked example of adding `assumptions.Kind` and
  reason codes additively without disturbing the health verdict chain,
  and of keeping replicated state separate from node-local state.

Code, all paths and symbols verified against commit `5dad79f`:

- `internal/hoststats/stats.go` — package comment, `DiskInfo`
  (`Healthy bool` / `Error string`), `PoolInfo.Health`, `Snapshot`,
  `Snapshot.Errors`, `gatherDisks`, `gatherOneDisk` (`smart -i` then
  `smart -d`), `parseSmartInfo`, `parseSmartStatus`, and the
  `"no SMART Status line in smart(8) output"` literal.
- `internal/assumptions/manager.go` — `Status`
  (`true`/`false`/`unknown`/`not_applicable`), `Kind`, `SubjectKind`
  (`node`/`vm`/`jail` reserved), `Key`, `Result`, `HistoryEntry`,
  `DefaultPath` (`/var/db/apiary/assumptions.json`),
  `currentSchemaVersion = 2`, and the quarantine-on-parse-failure
  behaviour in `Load`.
- `internal/assumecheck/checker.go` — the reason-code vocabulary rule,
  `RunOnce`'s independent-attempts contract, `RunDeadline`,
  `peerCheckTimeout`, per-tick caching, and the
  `NodeID`-must-be-the-raft-node-id comment.
- `internal/health/health.go` — five-state `Status`, `Reachability`,
  `Suffrage`, `NodeSignals` and the `AppliedIndex`/`LastLogIndex`
  "fabricated precision" comment.
- `internal/health/compute.go` — `ComputeNodeHealth`, its fixed
  precedence chain, and the step 2 / step 3 branches.
- `internal/cluster/simulate.go` — `RecoveryVerdict` and its five
  values, in particular `RecoveryVerdictReplicaUnobserved` and the
  `unverified_replica` / `replica_unobserved` distinction.
- `internal/coverage/scenario.go` — package comment on pure
  classification and on `StatusPhysicallyRehearsed` / `StatusStale`
  being permanently unreachable in v1.
- `internal/manager/server.go` — the nil-able-handler-field convention.
- `internal/frontend/convert.go` — `diskView` and `fromRPCStats`.
- `internal/frontend/server.go` line 605 — `ParseFS(web.FS,
  "templates/*.html")`.
- `web/templates/host.html` lines 39-60 — the Disks table and its
  `Error` → `unknown` / `Healthy` → `success` / else → `error`
  precedence, and the pool `ONLINE` colouring above it.
- `api/rpc/manager.proto` — `rpc HostStats` (line 138),
  `rpc GetLocalHASTResourceStatus` (line 143),
  `rpc GetLocalNetworkBridgeStatus` (line 444),
  `message HostStatsRequest` (line 1152), `message DiskStats` (line
  1175) and its `error` comment, `message HostStatsResponse` (line
  1208) with `disks` = 5 and `errors` = 7,
  `GetLocalHASTResourceStatus*` (line 1258), and the `AssumptionKind`
  enum (line 2515, currently 0-5).
- `api/internalpb/state.proto` — the raft state proto this ADR
  deliberately does not extend.
- `internal/commonconfig/manager.go` — `Config` and its
  zero-value-means-unset convention, considered for thresholds and
  rejected here in favour of managerd flags.
- `internal/install/checks.go` — `pkgInstalled` and the
  `Result{ID, Status, Detail, FixHint}` shape, the precedent if an
  install check for `smartmontools` is ever added.

Environment facts, established rather than assumed:

- `smartctl` and `smartmontools` appear nowhere in the tree (grep,
  excluding this ADR's own text).
- ripgrep is not installed; `rg` returns zero results for everything,
  and all findings above were produced with `grep`.
- `ssh root@10.90.0.94` from the environment this ADR was written in
  returns `Permission denied (publickey,keyboard-interactive)`, so no
  live fact about brood or drone — including whether `smartctl` exists
  there — was established. No claim in this ADR depends on one.
