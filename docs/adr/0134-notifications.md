# ADR-0134: Colony-wide notifications

## Status

Proposed

## Context

ADR-0127 section 7 ("Notifications", Medium Priority) ranks alerting as
Phase 4, item 1: "No alerting for Flight Plan status, degradations,
Scorecard rehearsals." Its sketch is a `NotificationRule` CRD with an
`event_filter`, a `transports[]` list (in-app, ntfy, SMTP, Discord),
and a `template`, fed by "audit log emits structured events;
notification engine subscribes."

Almost none of that sketch has a counterpart in this repository, and the
parts that do exist contradict it in ways that matter. Apiary is
extremely good at *showing* an operator a condition and very bad at
telling them about it. Every page here is pull-only: an operator has to
be looking at the right screen, on the right Comb, at the right moment.
`internal/hoststats` knows a ZFS pool is `DEGRADED`; nothing tells
anyone. `internal/cluster` knows a configured HAST replica reported
`role: init`; nothing tells anyone. `internal/invariant` knows quorum
would not survive losing an unreachable voter; nothing tells anyone.
During exactly the incident where this matters, the operator is
elsewhere.

The temptation, given that this is a "Phase 4 observability" item, is to
build the general event bus ADR-0127 described. That would be a mistake,
and the reason is not stylistic. Three of the four events ADR-0127 lists
as `event_filter` values — "migration started", "backup failed",
"Flight Plan completed/failed" — have **no producer in this codebase at
all**. The fourth, "replication lag", has a producer but the project has
deliberately and repeatedly refused to compute the number a lag alert
would need. Building a transport framework around events that do not
exist produces a system with zero triggering conditions, which is
indistinguishable in the field from a system that does not work.

The other half of the problem is the one ADR-0127 does not mention at
all: **a notification system is the one component in Apiary that talks
to the outside world, on a timer, with credentials, while the Colony is
in the middle of a failure.** Every existing outbound path in this
codebase is either an operator-initiated action (`internal/freebsdimg`
downloading an image, `internal/cloudflare` running `cloudflared`) or a
base-system scheduler (`internal/deadman`'s `at(8)` jobs). A
notification dispatcher is different: it fires *unprompted*, it fires
*repeatedly*, and it fires *most often when the network is broken*,
because the thing that is broken is frequently the thing that would have
told the operator. The design's central obligation is therefore not
"send alerts" but "never let alerting make anything worse."

This ADR takes the narrowest possible v1 that a real incident would
thank us for, builds it on surfaces that exist, and says plainly what it
refuses to do.

### What ADR-0127 assumed that the repository does not contain

Verified by grep against commit `5dad79f` before drafting. These are
corrections to ADR-0127 section 7, not disagreements with its
prioritisation.

- **There is no Flight Plan engine.** `grep -rl "FlightPlan"` over
  `*.go`, `*.proto` and `web/templates` returns nothing. ADR-0127's
  "Flight Plan completed/failed" filter and its "Flight Plan emits
  lifecycle events" integration have nothing to subscribe to. ADR-0131
  found the same and deferred the same way.
- **There is no Operational Continuity Scorecard.**
  `grep -rl "OperationalContinuity\|Scorecard"` returns nothing.
  "Scorecard rehearsals" is not an event source.
- **There is no audit log.** `grep -rn "audit"` over `internal/*.go`
  returns only prose references to two dated security audits in code
  comments. There is no event stream to "subscribe" to, and no
  append-only record to link a notification to. ADR-0131 §8 was forced
  to invent its own record model for the same reason.
- **There is no replication-lag figure, by deliberate decision.**
  `GetLocalHASTResourceStatus` (ADR-0119) returns role, status,
  replication mode, and `hastctl`'s raw *dirty extent text*. ADR-0119's
  own Decision says: "Apiary does not translate them into seconds,
  bytes-at-risk, or a numeric RPO." A "replication lag exceeded"
  notification would require inventing precisely the number this
  project has three separate ADRs declining to invent. v1 fires on
  ADR-0121's `replica_out_of_sync` verdict (a confirmed-unusable
  replica) and on nothing derived from extent text.
- **There is no notification-shaped code anywhere.** `grep -rn
  "webhook\|Webhook"` over `*.go`, `*.proto` and `web/templates`
  returns **zero** results. `grep -rl "http.Client\|http.Post"` over
  `internal/` returns exactly one file, `internal/freebsdimg/images.go`
  — an operator-initiated download with a caller-injected client. There
  is no outbound-notification plumbing, no retry helper, no queue, no
  template engine, and no channel abstraction to extend.
- **The Cloudflare integration is not an outbound HTTP precedent.** It
  is worth stating because it is the obvious thing to reach for.
  `internal/cloudflare` implements DNS (`dns.go`), an origin-CA client
  (`origin_ca.go`), a sidecar (`sidecar.go`) and a tunnel
  (`tunnel.go`) — and the tunnel path does not open a socket at all: it
  renders a cloudflared YAML config (`RenderConfig`) and shells out to
  `cloudflared` via `internal/cloudflare/exec.go`'s `runCmd`, which is
  the same `exec.CommandContext` helper `internal/bhyve` keeps. Its
  credential is `nodeconfig.CloudflareTokenFile`, a **path** the daemon
  reads. So the useful precedent it offers is a credential-as-path
  convention, not a network-transport one. That convention is the one
  this ADR adopts (§7).
- **There is no `NotificationRule`, `Notification`, or any other
  notification proto message.** `FSMSnapshotState`
  (`api/internalpb/state.proto`) holds `vms` (2), `networks` (3),
  `api_keys` (4), `auth_enabled` (5), `jails` (6),
  `pending_join_requests` (7), `restart_leases` (8) and
  `restart_records` (9). Field 10 is claimed by ADR-0133
  (`certificates`). Everything in this ADR is new.
- **"In-app notification center — persistent, dismissible, linked to
  audit log entries" has nothing to link to.** "Persistent" and
  "dismissible" are also in direct conflict, and the resolution is not
  obvious: an in-app store that an operator can dismiss is an in-app
  store that can silently lose the one notification that mattered. v1's
  record is **append-only and never dismissed**; it is cleared by age,
  or by an explicit "resolve" that leaves the record in place with a
  resolved marker (§5).

### What does exist, and is genuinely close to a notification

- **`internal/deadman` is not a notification system and must not be
  treated as one.** It is named for the dead-man's switch pattern and
  the package is worth reading before writing this ADR, because it is
  easy to assume it owns alerting. It does not. It arms an `at(8)` job
  per `(bridge, tap)` pair to run `ifconfig <bridge> deletem <tap>` if
  nothing confirms bridge health in time (ADR-0101), and cancels it
  via `atrm(1)` from `ConfirmBridgeHealthy`. Its entire "delivery"
  mechanism is the FreeBSD base-system scheduler, its state is one
  small JSON file per tap under `StateDir`, and it has no notion of a
  recipient, a channel, a retry, or a failure it can report. It is
  worth citing for exactly one reason: it is a real, already-shipped
  example in this codebase of a *protective* scheduled action that runs
  **outside** managerd, on the OS scheduler, precisely so that a wedged
  managerd cannot block it. That is the same isolation property this ADR
  wants, from the opposite direction. Notifications must never be
  built into the deadman path, and deadman must never be built into
  the notification path.
- **`internal/assumptions` already has the exact state model a
  notification needs.** `Manager.Append` is called once per checker tick
  and already implements edge-triggered history: "A HistoryEntry is
  additionally appended for a Key only when `(ObservedStatus,
  ReasonCode)` changed from the prior snapshot value for that Key (a
  transition, always recorded regardless of Detail churn), or when
  heartbeatInterval has elapsed." That is the flapping fix, already
  written, already tested, already shipped. Reusing it verbatim as the
  notification trigger's transition rule is the single highest-leverage
  decision in this ADR (§5).
- **`internal/health.ComputeNodeHealth`** (ADR-0056) already produces a
  five-state verdict with `unknown` as a first-class, non-healthy state,
  and is reachable cluster-wide through the existing `ClusterHealth` RPC
  in `internal/manager/server.go`, which already loops over members and
  already calls `s.Status` once for the shared anchor.
- **`internal/cluster/simulate.go`** already produces the exact verdicts
  a redundancy notification needs, with a documented and enforced
  separation between "confirmed broken" and "could not check."
- **`cmd/managerd/main.go` already runs four background loops** in
  exactly the shape this design needs: `runReconcileLoop`,
  `runAssumptionCheckLoop`, and two more, all of the form "immediate
  first run, then one per `time.Ticker` tick, errors logged not fatal."

## Decision

### 1. The central safety property

**A notification must never block, slow, fail, roll back, or extend the
latency of the operation that produced it.** Not "must not usually" —
never, structurally, by construction.

This is stated first because every other decision in this ADR is
downstream of it, and because it is the failure mode a reader should
assume any notification system gets wrong until shown otherwise. The
concrete form of the property:

> If a notification sink is unreachable, misconfigured, hung on a TCP
> connect, returning 500, or not yet configured at all, then an operator
> deleting a Cell, cutting a Cell over, restarting a Comb, or failing a
> quorum check observes **byte-identical** behaviour and timing to
> the same operation performed with notifications removed from the
> binary.

The mechanism, in three rules, each of which is separately checkable:

1. **Trigger producers never call a transport.** A trigger's entire
   contribution to this system is a non-blocking send on a buffered Go
   channel. The send has a `default:` branch. If the buffer is full, the
   event is **dropped and counted**, the function returns, and the
   producer continues. There is no code path from a trigger to a socket,
   because the trigger package does not import `net/http` and must never
   be permitted to.
2. **All delivery happens in one goroutine, on the leader, with a
   bounded context per attempt.** That goroutine is the *only* thing in
   the process that opens an outbound notification socket. It is
   started from `cmd/managerd/main.go` in the same shape as
   `runAssumptionCheckLoop`, and it can be killed mid-attempt by
   cancelling the context without any effect on any other subsystem.
3. **The durable record is written after the fact, best-effort, and its
   failure is not the notification's failure.** What lands in raft is a
   `NotificationRecord` — an append-only log of *attempts and their
   outcomes*, written by the delivery goroutine, never on an
   operation's critical path. If that write cannot reach quorum, the
   attempt still happened, the record is missing, and the gap shows
   up as a dropped counter in `NotificationEngineStatus` and a line
   in managerd's log.

**What the caller observes when delivery is impossible: nothing.**
No error, no warning, no changed status code, no extra latency, no
retry on the operation. The only place the failure is ever visible is
the notification surfaces themselves — the in-app page, the
`NotificationEngineStatus` RPC, and the log. This is deliberate: an
error surfaced on the triggering operation would be a lie, because the
operation succeeded.

#### 1.1 Why the trigger cannot block even in principle

The `select` with `default:` is the whole guarantee, and it is worth
being pedantic about it because a bounded channel with a blocking send
would satisfy every functional test and still violate the property.

```go
// Emit publishes event to the leader's notification engine. It never
// blocks, never returns an error, and never performs I/O. The caller
// is an operation that has already completed (or is completing) and
// must not be delayed by anything that happens here.
func (e *Emitter) Emit(ev Event) {
    select {
    case e.ch <- ev:
    default:
        // The engine is saturated. This is a dropped notification, not
        // a failed operation. Count it and move on; the count is
        // visible in NotificationEngineStatus and must never be
        // silently swallowed.
        atomic.AddUint64(&e.dropped, 1)
    }
}
```

The drop counter is not an implementation detail. A notification system
that silently discards events under load is indistinguishable from one
that has stopped working, and the project rule that `unknown` is a
state with its own verdict applies to notifications as much as to
health. `dropped` is a first-class, operator-visible, never-reset-to-
zero-without-acknowledgement counter.

#### 1.2 The engine is leader-only, and that is load-bearing

The delivery goroutine runs only on the raft leader. Every other Combs
has the emitter (so a local producer can hand an event up cheaply) but
no engine, no channel consumer, and no sockets. This is what makes
deduplication (§6) and rate limiting (§5) a local decision instead of a
distributed protocol, and it is why the design costs one `IsLeader`
check rather than a consensus round.

The corollary is a real hole and it is stated rather than hidden: **a
non-leader Combs cannot deliver a notification, and if the leader is
lost the Colony goes quiet until a new leader is elected.** During a
leader-loss incident — which is one of the incidents most worth
notifying about — notifications stop. This is accepted for v1, for the
same reason every other read-only cluster surface in this codebase is
leader-scoped: a second delivery path that fires independently on
every Combs is precisely the notification-storm failure this ADR is
built to prevent, and the failure mode of a *too quiet* system is an
operator opening a page, while the failure mode of a *too loud*
uncoordinated one is an operator muting the channel.

`StatusResponse.raft_is_leader` (field 4, `api/rpc/manager.proto`) is
the existing signal. The engine checks it every loop iteration, not
once at startup, because a node can and does gain and lose leadership
across its lifetime.

### 2. Delivery semantics: at-least-once, deduplicated, never lost

A notification system that loses a page during an incident is worse
than one that sends a duplicate. A duplicate costs an operator three
seconds. A lost page costs the recovery window.

**At-least-once with bounded retry.** Every delivery attempt gets a
bounded `context.WithTimeout` (5 seconds in v1). On failure the event is
rescheduled with exponential backoff — 10s, 30s, 2m, 10m, 30m — for a
**maximum of 6 attempts spanning roughly 45 minutes**. After the sixth
failure the record is written with `state: undeliverable`, the full
verbatim error is stored, and the engine moves on. The record is never
deleted. The operator sees, on the notifications page, a notification
that says it could not be delivered, with the reason.

The alternative — dropping after N attempts — is rejected and is
listed among rejected alternatives.

**Sink-down-for-hours is a normal condition, not an exception.** If a
webhook endpoint is unreachable for four hours, the engine does not
build a queue of four hours of events. Backoff is per-event and
bounded; the queue is bounded (§5.3); when the sink returns, the engine
resumes and the in-app record has the whole outage's history in order
regardless of whether any of it reached the webhook. The in-app record
is the durable artifact; the webhook is a convenience. This is the
inverted priority that makes the whole thing survivable.

**Ordering is per-dedup-key, and that is the only ordering claimed.**
Events for one condition arrive in order. Events for two unrelated
conditions carry no relative order guarantee and none is claimed. Any
attempt at a global total order would require serialising the engine
behind the slowest sink — which is a latency coupling between an
unreachable webhook and a healthy one, and therefore a direct violation
of the central property. Per-key ordering comes for free: one key, one
in-flight attempt at a time, because two concurrent attempts for the
same key would defeat deduplication anyway.

**Deduplication has two layers**, and they answer different questions:

- *Re-evaluation dedup* (§5): the same condition observed on
  consecutive ticks produces one notification, not one per tick. This
  is the edge-trigger rule.
- *Delivery dedup* (§6): one event, one record, one in-flight attempt,
  across every Combs and across every retry cycle. The webhook payload
  carries a stable `dedup_key` so an external receiver (ntfy, Slack,
  a human's own automation) can collapse its own duplicates.

A receiver that is at-least-once and a sender that is at-most-once per
key compose to at-least-once with an idempotency hint. That is the
correct contract for a system whose worst outcome is a duplicate page.

### 3. Trigger sources: what exists, and what is refused

Every trigger in v1 is evaluated **on the leader only**, from data the
leader can already obtain through an existing read. Nothing in this
table requires a new read path, a new RPC, or a new producer package.

| # | Condition | Producer that exists today | Fires on | Explicitly does NOT fire on |
|---|---|---|---|---|
| T1 | A configured HAST replica is confirmed unusable | `cluster.RecoveryVerdict` = `replica_out_of_sync` (`internal/cluster/simulate.go`), reached via `ClusterHealth` | `replica_out_of_sync` | `replica_unobserved` (query failed), `unverified_replica` (never attempted), `unprotected` (fires once, see below) |
| T2 | A Combs's computed node health is bad | `health.ComputeNodeHealth` (ADR-0056) via `ClusterHealth` (`internal/manager/server.go:946`) | `degraded`, `stale`, `contradictory` | `unknown` — ever |
| T3 | A quorum-critical voter is unreachable | `invariant.ClassifyVoterQuorumImpacts` / `EvaluateQuorumTolerance`, already rendered on `/why-not` (ADR-0118) | a voter transitioning reachable → unreachable while quorum tolerance is at risk | a voter that was already unreachable; a `Staging`/`Nonvoter` member (raft `Suffrage`, ADR-0056's `ParseSuffrage`) |
| T4 | An automated assumption check is violated | `assumptions.Result` written by `assumecheck.Checker.RunOnce` | `StatusFalse` | `StatusUnknown`, `StatusNotApplicable` |
| T5 | A ZFS pool is unhealthy or near capacity | `hoststats.PoolInfo.Health` / `.CapacityPct` from `zpool list -Hp` | `Health != "ONLINE"`, or a capacity threshold crossed | pool absent from `zpool list` (an empty list means "no pools", not "all pools gone") |
| T6 | A disk reports a SMART failure | `hoststats.DiskInfo.Healthy` (from `smart(8)`) | `Healthy == false` **and** `Error == ""` | `Error != ""` — that is a *failed query*, which is `unknown` |
| T7 | A recovery assumption is failed | `invariant.EvaluateCellRecoverability` over `ResourceFact`, already on `/invariants` and feeding `/why-not` | a resource newly becoming unrecoverable | an already-known-unrecoverable resource |

**T1 deserves its own note because it is the sharpest available
example of the project's existing discipline.** `simulate.go`'s own
comments separate three states that a naive alert would collapse:
`RecoveryVerdictReplicaOutOfSync` means "queried directly and confirmed
NOT usable"; `RecoveryVerdictReplicaUnobserved` means "queried but could
not be read"; `RecoveryVerdictUnverifiedReplica` means "no live
observation was even attempted." The doc comment is explicit that
"reporting 'we could not check' as 'checked and it is broken' is the
exact inversion this feature exists to prevent." **A notification is
the worst possible place to make that inversion**, because the
remediation differs: one means go fix the replica, the other means go
fix your ability to observe the replica. T1 fires only on the confirmed
verdict. `replica_unobserved` appears in the UI as a visible *condition*
with no notification attached, which is the honest representation.

**`unprotected` is a one-shot, not a flap.** A resource with no
`replica_node_id` set is a fact about configuration that is true on
every single tick forever. T1's edge-trigger rule (§5.1) means it
fires exactly once, on the transition from "has a replica" / "unknown"
to "unprotected", and then never again until it changes. An operator
who wants it re-asserted gets the `/why-not` page, which is already
built to be re-read.

**Every table entry's threshold is a constant in v1, not a rule field.**
The `CapacityPct` threshold in T5 is 90%. It is a Go constant, named
in this ADR, in the same spirit as `deadman.defaultDelay` being a
`Manager.Delay` field with `defaultDelay` as its documented fallback.
Putting a threshold in a raft-replicated rule on day one means an
operator can change alerting by writing to raft, from any Combs, with
no review, in the same plane that holds `VMDefinition` — where a typo
moves workloads. Alerting configuration belongs on a different plane
from workload placement. Revisit in v2, after the rules have been
exercised.

#### 3.1 What is refused, and why

These are named so that a later reader does not "fix" the gap by
bolting on a transport.

- **Flight Plan lifecycle.** No engine exists. There is nothing to
  subscribe to, and designing a `flight_plan_completed` filter against
  an imagined engine produces a rule that can never match — the worst
  possible outcome, because a configured-but-dead rule reads in the UI
  as coverage.
- **Backup failed / backup verified.** ADR-0131 is Proposed; no
  `BackupJob` exists. The event source will exist; the hookup is a
  v2 item, and it is named here so the two ADRs are not merged by
  accident later.
- **Guest migration started / completed / failed.** ADR-0128 is
  Proposed; no migration runtime exists in `internal/`.
- **Replication lag.** See above. Would require a number ADR-0119
  explicitly refuses to compute.
- **S.M.A.R.T. attribute templates.** `hoststats.DiskInfo` carries
  `Name`, `Model`, `Serial`, `Healthy`, `Error` — a boolean, not
  parsed attributes. There is no reallocated-sector count, no
  pending-sector count, no wear level. A "S.M.A.R.T. template" would
  have nothing to render. T6 fires on the boolean; richer triggers wait
  for whatever ADR the Phase 4 S.M.A.R.T. item produces.
- **Certificate expiry.** ADR-0133 defines
  `CertificateExpiryStatus` including `EXPIRY_UNKNOWN`, and that
  design does not exist in the code yet. T-nothing fires on it; the
  hookup is one line when it lands.
- **An operator-defined template DSL.** No template engine exists and
  v1 does not add one. Every payload is a fixed Go struct rendered as
  JSON. A user-authored template is a code-execution surface with
  variable scope, and it is a v3 question at best.
- **A per-rule `transports[]` list.** §4 replaces it with a
  notification-wide channel list plus a per-rule on/off. One fewer
  nested structure, one fewer place for a half-configured rule to live.

### 4. Channels: in-app plus one generic webhook

**v1 has exactly two channels.**

- **In-app.** Append-only, raft-replicated, never dismissed (§5.4).
  This is the channel that must exist, because it is the only one that
  works when the network is broken — which is when it is most needed.
- **One outbound HTTP webhook** (`POST` of a JSON body to an
  operator-configured URL). This is the channel that actually reaches a
  human, and it is why v1 is not in-app-only.

**ADR-0127 lists four channels: in-app, ntfy, SMTP, Discord. Three of
those four are the same code.** ntfy is `POST https://ntfy.sh/topic`
with the message in the body. Discord is `POST <webhook url>` with a
JSON body. A generic webhook with a JSON body *is* both, configured
with a different URL and — this is the only real work — a small
`transform` per well-known destination to reshape the JSON into the
target's expected schema. That is ~15 lines per destination and it
converts ADR-0127's three channels into one implementation with a
lookup table. A receiver that wants something else entirely — a
Slack incoming webhook, a `curl` to a custom endpoint, a bare
`syslog`-ish JSON POST to a SIEM collector — gets it from the same
code with no change at all.

**SMTP is genuinely different and is deferred.** It needs an SMTP
client, a from-address, relay credentials, a second secret store, and
bounce handling, and it is the only channel of the four that Apiary
would have to *write*, rather than configure. Its one real advantage —
reaching a phone that has no push app and no internet — is served
better by the operator pointing the webhook at whatever mail-relay
bridge they already run, which costs them nothing and costs us the
entire subsystem. Deferred explicitly, not forgotten.

**Push is deferred** for a reason specific to this project: a push
channel means Apiary reaches a device it has no other relationship
with, on a vendor's infrastructure, for events this project is
consistently careful not to over-claim. When the notification fires for
a `replica_unobserved`-class event it should say "I could not check,"
not "your replica is broken." A push channel optimized for the
attention economy is a poor fit for a system whose whole value
proposition is not over-claiming.

**One webhook URL, not one per rule.** A single colony-wide webhook
URL is configured once. v1 has no per-rule routing, no per-rule channel
list, and no per-rule endpoint. Rationale: a routing table is a second
configuration plane that must be replicated, and the failure mode of a
misconfigured route ("this condition pages nobody") is silent. One
endpoint, one URL, one delivery attempt path, one thing to get right.

### 5. Rate limiting, flapping, and silencing

#### 5.1 Flapping is prevented by edge-triggering, not by rate limiting

The flapping problem is: a check toggles `true`/``false`/`true` on
consecutive ticks, and a naive subscriber emits three notifications.
Rate limiting papers over this with a window; edge-triggering solves
it at the source.

**The rule is borrowed verbatim from code that already ships.**
`assumptions.Manager.Append`'s doc comment is the specification:

> A HistoryEntry is additionally appended for a Key only when
> `(ObservedStatus, ReasonCode)` changed from the prior snapshot value
> for that Key (a transition, always recorded regardless of Detail
> churn), or when `heartbeatInterval` has elapsed since that Key's last
> HistoryEntry.

A notification is emitted only on a `(Status, ReasonCode)`
**transition** for a given condition key. Toggling produces one
notification on the rising edge. It does not produce three.
`heartbeatInterval`'s role —
"or when N has elapsed" — is preserved as the §5.2 re-notify floor,
not as a per-tick emit.

**ReasonCode must be part of the key, not just Status.** A ZFS pool
that goes `DEGRADED` for a second reason while already `DEGRADED` is
not a transition and does not re-notify; a pool that goes
`DEGRADED` → `FAULTED` is. Without this, a pool oscillating among
several `DEGRADED` sub-states notifies on every tick.

#### 5.2 Three independent brakes, in order of how much they cost

1. **Edge-trigger** (§5.1, free, always on).
2. **Per-key minimum interval** — 15 minutes. A genuine
   fault → resolved → faulted cycle inside 15 minutes produces one
   notification, with the later resolution and recurrence appended as
   history on the same record rather than as separate pages. This is
   what makes a "flapping" network interface quiet without hiding that
   it flapped: the history is right there.
3. **Per-key exponential backoff on repeated failure-to-notify** —
   once a key has notified more than 5 times in 24 hours, its
   notification interval doubles per subsequent notification, up to a
   6-hour ceiling. The record is still written every time; only the
   *notification* is slowed. This is the last-resort brake for a key
   that is genuinely oscillating between two real, distinct states with
   distinct reason codes, where edge-triggering correctly says "this is
   new" every time.

The three compose. The engine's per-key state is a small struct:
`lastStatus`, `lastReasonCode`, `lastNotifiedAt`, `recentCount`.

#### 5.3 The queue is bounded, and overflow is counted

The emitter channel is buffered (256 in v1) and drops on overflow
(§1.1). The engine's internal pending set is separately bounded at
512 events, oldest-first eviction, with the same counted drop. An
unbounded queue in a component that fires during incidents is a memory
leak with a latency cliff, and a bounded queue that drops is a
notification gap that the operator is told about (§5.4). Choose the
second.

#### 5.4 Silencing one thing without silencing everything

Silence is a **per-rule, per-key, time-bounded** mute with a stated
reason and a stated expiry — never a global off switch, and never
permanent.

- A mute is set on a `NotificationRule`, optionally narrowed to one
  condition key (e.g. "stop telling me about `da0` until I replace
  it"), with a `reason` string and a `until_unix`.
- A `until_unix` in the past is a no-op; there is no "mute forever" in
  v1. A permanent mute is a rule deletion plus a decision, and forcing
  that decision at the moment someone mutes a disk is the right
  friction.
- **Muting a rule suppresses the *notification*. It never suppresses the
  *record*.** Every suppressed event still writes a
  `NotificationRecord` with `state: suppressed`, naming the rule and
  the mute's reason. The record is the audit trail; the notification is
  the interruption. Mute the second without destroying the first, or
  the operator has silenced their own memory of what happened.
- Mutes are **not** raft-replicated in v1. They are a local operator
  convenience — "stop shouting at me while I work" — with a local
  expiry, stored in the same per-Combs JSON-with-`warnIfWorldReadable`
  shape as `internal/assumptions` and `internal/assumptionregister`.
  Raft-replicating a mute means any Combs can mute any other Combs's
  alerting, which is an availability tool pointed the wrong way. This
  is a deliberate exception to the "colony-wide state is raft" rule,
  justified because the state is a property of an operator's attention
  rather than of the Colony, and is called out here because the
  project's own convention points the other way.

#### 5.5 "No notification configured" is not a fault

The single most important honesty rule in this ADR, and the one most
likely to be got wrong by a future implementer.

There are four states, and they are four:

| State | Meaning | UI |
|---|---|---|
| `not_configured` | No rule matches this condition. The operator has not asked to be told. | Not an error. A neutral "—", or the condition simply not appearing in the notifications page at all. |
| `delivered` | Attempted, and the sink accepted it. | Normal. |
| `failed` | Attempted, sink rejected it, retries remain. | Error, with the verbatim reason. |
| `undeliverable` | Attempted, retries exhausted. | Error, with all six attempts' reasons. |
| `suppressed` | A rule matched and would have fired, but a mute was active. | Neutral, naming the mute and its reason. |
| `unknown` | The engine could not evaluate the condition at all — its evidence read failed, or the record could not be written to raft. | **Never rendered as healthy. Never rendered as delivered.** |

`not_configured` and `unknown` are the pair that will be confused, and
they are opposites: `not_configured` means *we decided not to look*,
`unknown` means *we tried to look and could not*. Collapsing them is
the same class of error as `replica_unobserved` →
`replica_out_of_sync`, and ADR-0056 (ADR-0118) already established
that silence is a state with its own verdict. The notifications page
therefore shows a per-condition row for every **known** condition with
its notification state, and shows `unknown` — never blank — for one the
engine could not evaluate this tick. A blank cell in a table of
conditions is the single most likely way this system ends up lying.

The same rule applies to the engine itself. `NotificationEngineStatus`
reports `configured` (is there a webhook URL), `last_tick_unix`,
`dropped_since_start`, `in_flight`, and `records_written`. A Colony with
no webhook configured reports `configured: false` and the page says
"no outbound webhook is configured; notifications are recorded in-app
only" — which is a statement about configuration, not a fault, and is
rendered in neutral text, not as an error banner.

### 6. Deduplication across a cluster: one leader, one evaluation

With N Combs observing the same condition, how do we get one
notification?

**Because only the leader evaluates, and the leader's evaluation reads
every Combs once through an RPC it already makes.**
`Server.ClusterHealth` (`internal/manager/server.go:946`) already
anchors on a single `s.Status` call, already reads raft membership once
and reuses it across all nodes, and already fans out to each member.
T1, T2, T3, and T7 are all derived from that one call's result. T5 and
T6 come from `hoststats.Snapshot` per node, which `PeerForwarder`
already forwards. T4 comes from `assumptions`, which
`internal/manager` already reads for `ListAssumptionResults`.

There is no per-Combs subscription, no per-Combs delivery, and no
cross-node dedup protocol, because there is no second evaluator. One
leader, one evaluation per tick, one `NotificationRecord` per transition
per key.

**The consistency cost, stated plainly.** This is a real trade, and it
is not free:

- **Freshness is the leader's tick.** A condition that begins and
  ends entirely between two engine ticks is not notified. v1's tick is
  30 seconds. A 10-second outage of a replica is not an incident and
  not notifying about it is correct; a 5-minute one is caught, because
  the state is re-observed on each tick and the transition is relative
  to the last observed state, not to the last event. The bound is
  "up to 30 seconds of additional detection latency," and it is stated
  in the UI rather than hidden.
- **A Combs that has lost raft connectivity is invisible to the
  leader**, and T1/T2/T3 will report that fact as `replica_unobserved`
  or `unknown` rather than as the underlying fault. The notification
  will be about the unobservability, which is honest, and the
  operator's next move is to fix the network — which is correct.
- **A leader that is itself the failing Combbs may fail to evaluate at
  all.** If the leader's own `zpool list` fails, T5 is `unknown` and no
  notification fires. The engine records `unknown` for that key; the
  operator is told the engine could not evaluate. This is the sharpest
  limitation of the design and the main argument for a future
  non-leader fallback (§9, question 3).
- **A leadership change resets nothing**, because the per-key
  edge-trigger state is in the delivery goroutine's memory. Within one
  tick of a leadership change, the first evaluation after the new
  leader takes over compares against *no* previous state, and so every
  known-bad condition will produce one notification. This is a
  one-notification-per-condition burst on failover. It is accepted:
  it is bounded by the number of distinct condition keys (small), it
  happens at most once per leadership change, and it is *desirable* —
  a new leader that stayed silent about every known-bad condition
  would be worse. The alternative (raft-replicating the edge-trigger
  state) trades a small, useful burst for a replicated write on a path
  that would then need its own failure handling, which is a bad trade.

### 7. Secrets: a URL with a token in it is a path, not a value

A webhook URL is a secret. Discord webhook URLs and ntfy topic URLs
are bearer credentials — anyone holding the URL can post to the
channel, and some of them embed enough to read it. This is not a
theoretical concern; it is why the URL must never enter raft.

**The rule: raft replicates the *path*; the leader reads the *file*.**

This is not a new convention invented here. `internal/cloudflare` —
this project's only existing outbound integration — takes
`nodeconfig.CloudflareTokenFile`, documented in-code as "a path, never
the raw token," and reads it at use time.
`nodeconfig.OriginCATokenFile` is the same shape. ADR-0133 makes the
same call for its ACME account key. The convention is already
established, already reviewed, and already used for exactly this class
of credential.

**Concrete placement, following ADR-0131's finding.** ADR-0131 §2
enumerated four live secrets already stored inline in local config
JSON, and this ADR must not add a fifth:

| File | Field | Status |
|---|---|---|
| `internal/nodeconfig` | `PeerAPIKey` (`peer_api_key`) | live secret, inline, existing |
| `internal/nodeconfig` | `RaftdToken` | live secret, inline, existing |
| `internal/raftdconfig` | `InternalToken` | live secret, inline, existing |
| `internal/frontendconfig` | `ManagerAPIKey` (`manager_api_key`) | live secret, inline, existing |
| **new** | `NotificationWebhookURLFile` | **path only. Never the URL.** |

`NotificationWebhookURLFile` is added to `nodeconfig.Config` as a path
field beside `CloudflareTokenFile`, with the same in-code comment
discipline. The leader reads it fresh on each delivery attempt rather
than caching at startup, so rotating the file takes effect without a
managerd restart — the same reasoning ADR-0133 records for
`Config func() (directory, tokenFile string, err error)` being "called
fresh on every tick."

**Unreadable secret file is `unknown`, not a silent no-op.** If the
path is configured but the file cannot be read, the delivery attempt
fails with the verbatim reason, the record is `failed`, and retries
proceed. The system must never treat "I could not read the webhook
URL" as "there is no webhook configured," because the second reading
means notifications are quietly off and the operator has no way to find
out.

**File permissions.** The established posture across the
secret-bearing config files is *warn, do not refuse*: 
`internal/frontendconfig.warnIfWorldReadable` logs "readable by
group/other but contains manager_api_key - recommend chmod 600" and
starts anyway, and `internal/raftdconfig`'s own comment records the
same reasoning for `InternalToken` — "a hard startup failure over a
permissions nit on a live system would" do more harm than the nit.
`internal/nodeconfig` goes further and actively `os.Chmod`s its own
file to `0o600` on write. The notification webhook file follows
`nodeconfig`, its own config package: written `0o600`, and the
existing 0600 posture of `managerd.json` is documented to be what
protects `PeerAPIKey` already. Escalating any of these to a hard
refusal is a separate decision that would have to apply to all four
existing secrets too; bundling it in here would be a scope
violation.

**Nothing else is secret.** The notification *body* is not — it names
a Comb, a condition, and a reason code, all of which are already
visible in the UI to anyone who can log in. A webhook receiver is by
construction an operator-chosen, operator-trusted destination. The
`template` from ADR-0127 is gone from v1 (§3.1), so there is no
user-authored string interpolation into a payload and therefore no
second-order injection surface.

## Rejected alternatives

- **Deliver inline, on the operation's goroutine.** The obvious design:
  the RPC handler that observes a condition calls the webhook before
  returning. Rejected because it violates the central property by
  construction: an unreachable sink becomes a 10-second stall on every
  matching operation, and an operator who has pointed Apiary at a dead
  webhook has silently made their Colony slower for no benefit. There
  is no configuration of this design that is safe; it is not a
  default-on-rarely design, it is a design where the failure mode and
  the feature are the same thing.

- **Deliver from raft's `Apply`, so the notification is committed
  before the operation returns.** This is the most tempting rejection to
  get wrong, because it sounds like it *guarantees* consistency — the
  notification and the state change land in the same log entry. It
  guarantees the wrong thing. `Apply` runs on every Combs, so it would
  fan a single cluster event out to N deliveries, turning the
  deduplication problem into the hardest part of the design. It would
  also put a network socket behind the FSM's apply lock, so one
  hung webhook stalls every subsequent state change on every Combs —
  the worst possible blast radius, applied to the component every other
  component in the system depends on. The `NotificationRecord` write is
  *also* a raft command, but it is issued by the delivery goroutine
  after the fact, precisely so that raft is a durable log of attempts
  rather than a delivery mechanism.

- **At-most-once delivery: fire and forget, drop on any error.**
  Cheaper to build, and it cannot produce duplicates. Rejected because
  an incident is exactly when the sink is most likely to be
  unreachable, and at-most-once converts "the network blipped" into "the
  operator was never told." The asymmetry is deliberate: a duplicate
  costs an operator three seconds, a lost page costs the recovery
  window. Bounded retry with a 6-attempt / ~45-minute schedule and a
  retained `undeliverable` record gets the useful half of
  at-least-once without unbounded work.

- **Every Combs subscribes and delivers independently.** Maximum
  robustness, no leader dependency, and the design the words
  "Colony-wide" invite. Rejected because it makes deduplication a
  distributed consensus problem (N observers, one notification, with
  network partitions producing either N or zero) and it makes rate
  limiting per-node, so N Combs behind one flapping uplink produce N
  times the notification volume at the far end. The leader-only
  design costs one `raft_is_leader` check and turns both problems into
  local state. The price — silence during a leader loss — is real and
  is recorded in Consequences and as an open question rather than
  glossed over.

- **In-app only for v1, with the webhook deferred.** Strictly smaller,
  and it would have avoided the entire secrets and retry surface. It
  also would not have solved the problem the ADR exists for: an
  in-app-only notification is read when someone opens the dashboard,
  which is the behaviour Apiary already has. The v1 cost is one
  `http.Client` and a path field, both of which are smaller than the
  argument for having them.

- **Build the transport plugin layer ADR-0127 describes** — a
  `transports[]` list per rule, with ntfy, SMTP, Discord, and in-app
  as four implementations, plus a template DSL. Rejected on three
  counts. It is a second configuration plane replicated through raft
  for a feature with a one-page UI. Four of its channels would have
  three of them share one implementation anyway (§4), so the plugin
  boundary buys nothing. And a user-authored template is a
  code-execution surface with a variable scope, in a system whose
  whole identity is not over-claiming. A generic webhook gets the
  operator to the same place with a fraction of the code.

- **Make the notification record dismissible, as ADR-0127 says.** A
  dismissible store is a store that can lose the one notification
  that mattered, and "I dismissed it in a hurry" is not
  distinguishable from "I never saw it" in the record. Rejected in
  favour of append-only with age-based trimming and an explicit
  `resolved_unix` marker. Age is the only thing that should remove
  history here.

- **Fire on `unknown` as well as on confirmed-bad verdicts, so the
  operator hears about observability gaps.** Tempting — an
  unobservable replica is arguably worse than a broken one. Rejected
  because it would invert the exact distinction
  `internal/cluster/simulate.go`'s own doc comment says this feature
  exists to preserve: the remediation for `replica_out_of_sync` is "go
  fix the replica" and for `replica_unobserved` is "go fix your
  ability to see the replica," and paging someone identically for
  both teaches them to ignore the channel. The condition is still fully
  visible; it just does not ring a phone.

- **Put thresholds in `NotificationRule` (raft-replicated operator
  config) rather than in Go constants.** More flexible, and the
  natural instinct. Rejected for v1 because it puts a settings plane
  in the same log as `VMDefinition` and `JailDefinition`, where a typo
  moves workloads, and because there is no way to change an alerting
  threshold without writing to raft from any Combs with no review
  trail beyond this ADR. Revisited in v2, named as open question 4.

## Consequences

### Positive

- **Incidents become visible without an operator watching a screen.**
  Every condition in §3's table is one that Apiary already computes on
  a page nobody is guaranteed to be looking at. T5 in particular — a
  ZFS pool leaving `ONLINE` — is the single highest-value trigger in
  the table and costs one `zpool list` parse.
- **The central property is structural, not procedural.** There is no
  code path from an operation to a socket, so there is no code path
  from a slow webhook to a slow `DeleteVM`. This is the property the
  design exists to guarantee, and it is guaranteed by a `select`
  statement and a package dependency rule, both of which a reviewer
  can check in a minute.
- **Deduplication is free.** Leader-only evaluation means N Combs
  produce one notification without a distributed protocol, a lease, or
  a consensus round — the same reason `ClusterHealth` already reads
  raft membership exactly once.
- **Three of ADR-0127's four channels ship as one implementation.**
  ntfy, Discord, and anything-else-HTTP are one webhook with a
  destination table.
- **The in-app record survives the incident.** A webhook that is
  down — which, during a network incident, it usually is — does not
  cost the operator the history of what happened. The durable artifact
  is local and raft-replicated; the webhook is a convenience.
- **Deferral is cheap.** The refused triggers (§3.1) are all
  one-registration-point hookups when their subsystems land, because
  the engine's input is a `Event`, not a bespoke integration.

### Negative

- **Notifications go quiet during a leader loss**, which is one of the
  incidents most worth notifying about (§6). Accepted in v1; the
  honest fix is a non-leader fallback, which is a real design problem
  and is question 3 below rather than a paragraph here.
- **Only the leader can deliver, so a Colony whose leader is also its
  failing Comb is the worst case**, and the failure is invisible rather
  than loud. `unknown` in the UI is the mitigation; it is a weaker
  mitigation than it sounds, because an operator who never opens the
  page never sees it.
- **No SMTP and no push.** An operator on a phone with no push app and
  a broken network gets nothing until they reach a browser. This is a
  real gap, deliberately taken (§4).
- **Thresholds are Go constants, not configuration.** Changing the
  capacity alert from 90% to 80% is a rebuild and a deploy, not a
  click. Deliberate (§3) and probably wrong within a year.
- **The edge-trigger state is in-memory**, so a managerd restart
  re-notifies every currently-known-bad condition once. Bounded by the
  number of keys, and arguably desirable, but it is a real duplicate
  source.
- **The template DSL is gone**, so a notification's text is fixed. An
  operator who wants a particular phrasing cannot have it without a
  release. Accepted; a template engine is a code-execution surface.
- **v1 has one webhook endpoint for the whole Colony.** An operator
  with two recipients must front it themselves. Deliberate (§4) and
  likely to be the first thing to change.
- **Mutes are not replicated** (§5.4), so an operator's silence does
  not follow them to the frontend they happen to be using.
- **This ADR adds a `map` and three messages to `FSMSnapshotState`**
  and an unbounded-in-principle append-only log to the raft log. The
  history cap (§ implementation notes) is the mitigation and it is a
  *record* bound, not a *raft-log* bound; the FSM snapshot carries the
  capped set, so a long-lived Colony's snapshot does not grow without
  bound.

## Implementation notes

All paths and names below are additions. Nothing here removes or
renames an existing symbol.

#### Raft state (`api/internalpb/state.proto`)

Field 10 is claimed by ADR-0133 (`certificates`); fields 1-9 are taken
as enumerated in "What ADR-0127 assumed" above.

```proto
  // notifications is the Colony-wide, append-only record of
  // notification *attempts* and their outcomes. It is a record of
  // what the engine did, never a queue of what it intends to do - a
  // pending event lives only in the leader's in-memory engine, so a
  // leader crash loses it and the missing record is honest about
  // that. Capped at 2000 records (oldest-first) by the FSM; the cap
  // is a record bound, not a raft-log bound.
  map<string, NotificationRecord> notifications = 11;

  // notification_rules is the Colony-wide set of operator-configured
  // rules. v1 has a small fixed rule table (see
  // NotificationRule) rather than a user-defined expression, so
  // "configuration" is a fixed enum choice, not a program.
  map<string, NotificationRule> notification_rules = 12;
```

```proto
// NotificationChannel is a destination kind. IN_APP is always present
// and needs no configuration - it is the raft-replicated record
// itself. WEBHOOK is one operator-configured HTTP endpoint, whose
// live URL is read from nodeconfig.NotificationWebhookURLFile at
// delivery time and never replicated (ADR-0134 §7).
enum NotificationChannel {
  NOTIFICATION_CHANNEL_UNSPECIFIED = 0;
  NOTIFICATION_CHANNEL_IN_APP = 1;
  NOTIFICATION_CHANNEL_WEBHOOK = 2;
  // ntfy and Discord are not separate channels. Both are
  // NOTIFICATION_CHANNEL_WEBHOOK with a destination transform
  // selected by the URL's host, exactly as ADR-0127's transport list
  // implies but without a second configuration plane.
}

// NotificationState is the outcome of one notification attempt. The
// five values are never collapsed:
//   - DELIVERED: the sink accepted it.
//   - FAILED: a sink error, retries remain.
//   - UNDELIVERABLE: retries exhausted. The record and every
//     attempt's verbatim reason are retained; the event is not lost,
//     it is known-undelivered.
//   - SUPPRESSED: a rule matched and a mute was active. The record
//     still exists - muting silences the interruption, never the
//     memory.
//   - UNKNOWN: the engine could not evaluate the condition, or this
//     record could not be written. Never rendered as healthy and
//     never rendered as delivered.
enum NotificationState {
  NOTIFICATION_STATE_UNSPECIFIED = 0;
  NOTIFICATION_STATE_PENDING = 1;
  NOTIFICATION_STATE_DELIVERED = 2;
  NOTIFICATION_STATE_FAILED = 3;
  NOTIFICATION_STATE_UNDELIVERABLE = 4;
  NOTIFICATION_STATE_SUPPRESSED = 5;
  NOTIFICATION_STATE_UNKNOWN = 6;
}

// NotificationRecord is one attempt (or attempt-group) at telling
// the operator about one condition. Append-only, never overwritten
// except to advance a PENDING record's state once, and never
// deleted by a user action (§5.4).
message NotificationRecord {
  string id = 1;              // stable; see dedup_key
  string dedup_key = 2;       // "<comb_id>/<condition>/<reason_code>"

  // condition is the trigger's stable identifier, e.g.
  // "hast_replica_unusable", "zpool_unhealthy", "assumption_false".
  // It is the thing §5's edge-trigger keys on and the thing the UI
  // groups by. A closed vocabulary in v1 (T1-T7 in the ADR), not a
  // free string.
  string condition = 3;
  string comb_id = 4;         // empty for Colony-wide conditions
  string subject_id = 5;      // pool name, disk name, cell id, ...

  NotificationState state = 6;
  int32 attempts = 7;
  int64 created_unix = 8;
  int64 last_attempt_unix = 9;
  int64 resolved_unix = 10;   // 0 while the condition still holds

  // error is the verbatim last failure reason, never a smoothed-up
  // summary. The same posture internal/assumptions takes with Reason
  // and internal/cluster takes with RecoveryVerdict's Explanation
  // strings: the operator needs the reason, not the category.
  string error = 11;

  // evidence is the reason code and the observation that produced
  // this record, verbatim from the producing package (e.g. hastd's
  // own role/status pair, a zpool health string, a smart(8) error).
  // It is what makes the record reviewable months later, when
  // nobody remembers what "replica_unobserved" meant.
  string reason_code = 12;
  string evidence = 13;
}

// NotificationRule is the fixed v1 rule table. Every field is a
// closed choice; there is no user-authored expression and no
// template (§3.1, §4).
message NotificationRule {
  string id = 1;
  string condition = 2;         // matches NotificationRecord.condition
  string subject_filter = 3;    // "" = any subject on any comb
  bool enabled = 4;

  // min_interval_seconds is the per-key minimum between
  // notifications for this condition (§5.2, brake 2). Default 900.
  uint32 min_interval_seconds = 5;

  // daily_notify_budget is the soft cap before exponential backoff
  // engages (§5.2, brake 3). Default 5.
  uint32 daily_notify_budget = 6;

  NotificationChannel channel = 7;  // WEBHOOK only; IN_APP is implicit
  int64 created_unix = 8;
}
```

**Failure modes.** A `NotificationRecord` write that cannot reach
quorum leaves the *delivery* intact and the *record* missing: the
in-app page cannot show what it cannot read, and the dropped counter in
`NotificationEngineStatus` rises. A `NotificationRule` write that
fails leaves the previous rule set in force — an alerting
misconfiguration never half-applies. Neither failure can roll back the
event that produced the record, because both happen strictly after.

#### Proto RPCs (`api/rpc/manager.proto`)

Added to `service ManagerService`, alongside the existing
`ListAssumptionResults` and `ClusterHealth` read RPCs:

```proto
  rpc ListNotifications(ListNotificationsRequest) returns (ListNotificationsResponse);
  rpc ListNotificationRules(ListNotificationRulesRequest) returns (ListNotificationRulesResponse);
  rpc SetNotificationRule(SetNotificationRuleRequest) returns (SetNotificationRuleResponse);
  rpc DeleteNotificationRule(DeleteNotificationRuleRequest) returns (DeleteNotificationRuleResponse);
  rpc GetNotificationEngineStatus(GetNotificationEngineStatusRequest) returns (GetNotificationEngineStatusResponse);
  rpc TestNotificationWebhook(TestNotificationWebhookRequest) returns (TestNotificationWebhookResponse);
```

`ListNotificationsResponse` carries `repeated NotificationRecord
latest`, `string error`, and — critically — a per-condition row set
including conditions the engine could **not** evaluate, each with
`state: NOTIFICATION_STATE_UNKNOWN`. A response that omits a
condition because it produced no record is the exact failure §5.5
forbids. `GetNotificationEngineStatusResponse` carries `configured`
(false when no webhook URL is configured — neutral, not an error),
`last_tick_unix`, `dropped_since_start`, `in_flight`,
`records_written`, and `is_leader`.

`TestNotificationWebhook` exists because "the operator cannot tell if
their webhook works" is the failure mode that keeps a notification
system unused for a year. It sends one real request through the exact
delivery path and reports the real response. Its result is
**never** written to `notifications` — it is a test, not a
notification, and polluting the record with it would be a lie about
the trigger stream. Failure mode: it is rate-limited to one in flight
per node, and its own failure is reported in the response, never
silently.

#### `internal/notify` (new package)

Pure, no raft import, no `net/http` import at the emit boundary.

- `Event` — `Condition`, `CombID`, `SubjectID`, `ReasonCode`,
  `Evidence`, `ObservedAt`.
- `Emitter.Emit(Event)` — the non-blocking channel send of §1.1.
  **This is the package boundary rule: nothing that holds an `Emitter`
  may transitively import `net/http`.** A dependency-lint test asserts
  it.
- `Engine` — the leader-only consumer. Owns the per-key edge-trigger
  state, the three brakes, the bounded pending set, the retry schedule,
  and the `http.Client` (10s timeout, no redirect following, 8 KiB
  response cap, TLS verification on). Its `Transport` is an interface
  so tests inject a fake with zero sockets, mirroring `deadman`'s
  `atRunner` exactly.
- `WebhookTransport` — `POST` JSON. Destination table maps
  `ntfy.sh` → ntfy body, `discord.com/api/webhooks` → Discord body,
  everything else → the raw `NotificationRecord` JSON. Reads the URL
  from `nodeconfig.NotificationWebhookURLFile` **fresh on each
  attempt** (§7).
- `Flatten` — converts a per-node evaluation into condition keys,
  applying edge-trigger, brakes, and the `not_configured` vs
  `unknown` distinction. Pure function, no I/O, fully unit-testable.

**Failure modes.** A hung `http.Client` is bounded by
`context.WithTimeout(5s)`, and the timeout is per attempt, not per
event, so a hung sink cannot stall the loop. A DNS failure on a
configured-but-wrong hostname is `failed` with the verbatim resolver
error, retried on schedule. A webhook that returns 200 having dropped
the body is `delivered` — the sender cannot distinguish this, and
pretending otherwise would be inventing evidence, so `delivered` means
exactly "the sink returned 2xx" and the ADR says so.

#### managerd wiring (`cmd/managerd/main.go`)

`runNotificationLoop(ctx, engine, interval)` in the exact shape of the
four existing loops — immediate first run, then one per tick, errors
logged not fatal. Two new loop bodies, both already-established
shapes:

- `notificationEvaluateOnce` — leader-gated; on a non-leader it is a
  no-op returning immediately, so a five-Combs Colony does not run
  five evaluations per tick and only one of them does anything.
- `notificationDeliverOnce` — drains the emitter channel, applies the
  brakes, delivers with bounded context, writes records.

The evaluator reads through the **same** `ClusterHealth` code path the
UI already uses, rather than a second gather. A second gather would be
a second set of facts that could disagree with what the page shows,
and two disagreeing views of quorum is the problem ADR-0056 and
ADR-0118 were written to end.

#### Reconcile loop

**There is no reconcile-loop change in v1, deliberately.** The
reconciler (`internal/cluster/reconciler.go`) converges physical state
on Combs; notifications are not physical state and putting them on the
convergence path would make every alert a thing the reconciler retries,
which is how a notification becomes a block. The engine is a separate
loop beside the existing four, on the leader, and the only shared code
is read-only.

#### Frontend surface (`internal/frontend`, `web/templates`)

- `GET /notifications` (`internal/frontend/notifications.go`) — the
  record table: condition, subject, state, attempts, last attempt,
  verbatim error, expandable evidence. Rows exist for **known
  conditions with no notification**, showing `not_configured` or
  `unknown` — never a blank (§5.5).
- `GET /notification-rules` — the fixed rule table (§3's condition
  vocabulary, not an expression editor) and the webhook configuration
  form. The form takes a **file path** (§7) and a "Test" button wired
  to `TestNotificationWebhook`.
- `notification_status` block in the existing sidebar Status section
  (`web/templates/layout.html`, alongside Assumptions, Invariants,
  Why Not): engine state, `configured`, `dropped_since_start`, and the
  count of `undeliverable` records. `dropped_since_start` is shown
  whenever it is non-zero and is never auto-cleared.
- A per-Combs banner on `/` for `undeliverable` and `unknown`
  conditions on that Comb, using the same visual treatment as the
  existing `storage_degraded` banner on the assumptions page — an
  established pattern for "this surface's data is not trustworthy."

**Failure modes.** The page renders from `ListNotifications`; a raft
read failure renders the page with an explicit error and **not** as an
empty list, because an empty notifications page during a raft incident
is indistinguishable from a quiet Colony. `TestNotificationWebhook`
failing shows the sink's real error, never a generic "test failed."

## Test plan

**macOS cannot validate this feature end to end.** Apiary has no ZFS,
no HAST, no `smartctl`, no `bhyve`, no jail(8), and no real network
latency on a developer laptop. Every item below marked *testbed* can
only be established on brood (10.90.0.94) or drone (10.90.0.95), the
bare-metal FreeBSD 16.0-CURRENT testbed. A green `go test ./...` on
macOS proves the state machine, the brakes, the dependency rule, and
the rendering — nothing more. Specifically, **real outbound delivery
cannot be proven on macOS**: a loopback `httptest` server proves the
request is well-formed, not that it survives a real network, a real
DNS failure, a real TLS chain, or a real endpoint being down for
hours.

Unit-testable on macOS, no privileges required:

- **The central property, asserted directly.** A `Flatten`+`Engine`
  wired to a transport whose `Deliver` blocks forever. Assert that
  `Emitter.Emit` returns in under a microsecond, that `Emit` called
  10,000 times without a consumer drops rather than blocking, and
  that `dropped_since_start` equals the overflow.
- **The dependency rule.** A test that walks `go list -deps` for the
  package that owns `Emitter` and fails if `net/http` is in the
  transitive set. This is the property that makes §1 structural
  rather than aspirational, and it must be a test, not a comment.
- **Edge-trigger.** A `Flatten` sequence `ok → bad → ok → bad` over
  four consecutive ticks emits exactly two notifications.
- **Reason-code sensitivity.** `ok → degraded/a → degraded/b` emits
  one notification (reason change is not a status transition within
  the same class only if reason_code differs — see the Open Question
  on this); `ok → degraded/a → degraded/a` emits one.
- **All three brakes.** The 15-minute floor, the daily budget, and
  the doubling-to-6-hours ceiling, each with a synthetic clock. Fake
  clock only — no `time.Sleep` in these tests.
- **Retry schedule.** 6 attempts, backoff sequence 10s/30s/2m/10m/30m,
  terminal state `undeliverable` with all six verbatim reasons
  retained.
- **`not_configured` vs `unknown` vs `suppressed`**, asserted as three
  distinct rendered states, including that `unknown` never renders as
  the same glyph or colour as `delivered`.
- **Bounded history.** 2,500 records through the FSM; the oldest 500
  are evicted and the surviving 2,000 are intact in the snapshot.
- **Snapshot round-trip.** A `FSMSnapshotState` with notifications and
  rules marshals, snapshots, restores, and re-marshals
  byte-identically. Failure mode under test: a `raftd -restore`
  (ADR-0051) that silently dropped the notifications map would lose
  the operator's history.
- **Secret strip.** A fixture `nodeconfig` with
  `NotificationWebhookURLFile` set to a path, and a fixture file
  containing a URL carrying a known token string: the raft-encoded
  `FSMSnapshotState` is searched for that token string and the test
  **fails** if it is found. Unconditional — no `testing.Short()`, no
  env var, no way to skip. This is the same posture ADR-0131 took for
  its own secret strip.
- **Unreadable secret file.** A configured-but-absent file produces
  `failed` with the read error, never `not_configured`.

*Testbed only:*

- **Real outbound delivery** to a real webhook endpoint, and a real
  DNS failure and a real TCP reset mid-body, to confirm the timeout
  and backoff behave as specified under conditions a loopback server
  cannot produce. *testbed*
- **A real `zpool list` returning a non-`ONLINE` health string**, to
  confirm T5's parse against the output this project's FreeBSD 16.0
  build actually produces. The parser is unit-tested against recorded
  fixtures, but the fixture is only trustworthy if someone once ran it
  against a real degraded pool. *testbed*
- **A real `smartctl` failure on one disk**, to confirm T6's
  `Healthy == false && Error == ""` predicate distinguishes a real
  SMART failure from a `smartctl` invocation that errored — the
  distinction the whole trigger rests on. *testbed*
- **A real HAST primary/secondary pair driven to a confirmed-bad
  state**, to confirm T1 fires on `replica_out_of_sync` and stays
  silent on `replica_unobserved`, and that the two produce visibly
  different records. This is the single most important testbed item:
  it is the exact inversion ADR-0121 was written to prevent. *testbed*
- **Real leadership change** on a multi-Combs testbed, confirming the
  documented one-notification-per-condition burst on failover (§6)
  and that only the new leader delivers afterwards. *testbed*
- **A webhook endpoint killed for four hours and restarted**, to
  confirm the queue stays bounded, the in-app record holds the full
  outage's history, and delivery resumes without a thundering herd.
  *testbed*

Not tested by either: a genuinely correct `unknown`-versus-`failed`
distinction under every possible evidence failure. That is a
reasoning property, and the mitigation is the rendering rule plus the
open question about whether the engine should ever guess.

## Open questions

1. **Should `delivered` mean "the sink returned 2xx" or something
   stronger?** v1 says 2xx and nothing more, because a sender cannot
   prove a receiver processed a message. Is that honest enough for the
   word "delivered," or should it be renamed `accepted`?
2. **Is a reason-code change within one status class a transition?**
   §5.1's rule fires on `(Status, ReasonCode)` change, which means a
   pool oscillating between two distinct `DEGRADED` sub-states
   notifies every time, and the daily-budget brake (§5.2 brake 3) is
   the only thing between that and a storm. Should the edge key be
   `Status` alone, with reason recorded as evidence?
3. **Should a non-leader be able to deliver, at all?** The
   leader-only design (§1.2) is what makes dedup free and storms
   impossible, at the cost of going quiet during a leader loss and of
   never evaluating when the leader is itself the failing Comb. A
   non-leader *self-delivery of node-local conditions only* (T5, T6 —
   the ones that need no cluster read) would close the worst gap
   without reintroducing N-fold duplication, because T5/T6 are the only
   conditions with no cross-Combs duplication risk. Worth the
   complexity?
4. **Where do the thresholds live long-term?** §3 puts them in Go
   constants on the right plane and the wrong lifecycle. A
   `NotificationRule`-carried `capacity_threshold_pct` is one raft
   field away, and the objection was "an operator can change alerting
   in the same plane that moves workloads." Is that objection
   strong enough to keep in code for v2?
5. **One webhook for the whole Colony, or per-rule?** v1 says one
   (§4) to avoid a silent "this condition pages nobody" failure. If
   operators want per-Combs or per-severity routing, is the honest
   fix a second endpoint, or a destination header the operator's own
   receiver reads?
6. **Retention.** 2,000 records is a guess. Is that a sensible
   review window for the operator who reads them — a week of busy, or
   a month of quiet? And should retention be a constant or an operator
   setting? (A setting is another raft field; §3's plane objection
   applies.)
7. **Is 30 seconds the right evaluation tick?** It bounds detection
   latency for T1-T3 and is the dominant CPU cost of the loop
   (5 Combs × `ClusterHealth`, though only the leader evaluates). A
   slower tick is cheaper and later; a faster one is earlier and
   noisier. What does the operator actually need?
8. **Mutes and the local-CLI question.** §5.4 makes mutes local and
   unreplicated, which is right for attention and wrong for a second
   operator. The Phase 4 Local CLI item would change the shape here.
   Should mutes be designed for multi-operator now?

## References

- **ADR-0127** — Sylve.io feature survey; section 7 ("Notifications")
  is this ADR's brief and its phase table places it in Phase 4, item
  1. Corrections to its assumptions are listed above.
- **ADR-0056** — Evidence-Aware Health v1. The five-state `Status`
  contract, the `unknown`-is-not-healthy rule, and the deliberate
  on-demand (not background-checked) scoping this ADR inherits for its
  trigger inputs.
- **ADR-0118** — Why Not quorum blocker detail. The precedent that a
  blocked path must show *why*, applied here to the difference between
  `not_configured` and `unknown`.
- **ADR-0122** — Cluster-wide Evidence-Aware Health over the API.
  `ClusterHealth` is the single read this ADR's evaluator reuses; a
  second gather would be a second view that could disagree with the
  page.
- **ADR-0119** — Replica freshness and maintenance wave planner.
  `GetLocalHASTResourceStatus`; and the explicit refusal to translate
  `hastctl`'s dirty-extent text into seconds or a numeric RPO, which
  is why T1 has no lag threshold.
- **ADR-0121** — Dependency-graph HAST sync evidence. The
  `replica_out_of_sync` / `replica_unobserved` distinction T1 depends
  on, and the doc comment in `internal/cluster/simulate.go` that
  makes the inversion unacceptable.
- **ADR-0111** — Frontend restart routing. Not load-bearing for
  delivery, and deliberately **not** cited as a precedent: it is about
  a stale in-memory session after a restart, which is a different
  problem. It is listed because the dispatch named it and the honest
  answer is that it does not bear on notification delivery.
- **ADR-0131** — Manifest-backed backup and restore. The source of the
  four-inline-secrets finding (§7) and the model for an append-only
  record that survives being repeated. Also the source of the "no
  Flight Plan engine" and "no Operational Continuity Scorecard"
  findings, re-verified here.
- **ADR-0133** — Colony-wide certificate management. Claims
  `FSMSnapshotState` field 10, which is why this ADR starts at 11;
  and the same credential-as-path convention.
- **ADR-0101** (via `internal/deadman`) — the dead-man's switch
  pattern. The precedent for a protective action running outside
  managerd, and the boundary this ADR refuses to cross.
- **ADR-0023** — API key authentication. The existing precedent for
  appending a `map` to `FSMSnapshotState` and for a one-way
  `auth_enabled`-style flag.
- **ADR-0051** — `raftd` config save/restore. The reason the
  notification record is in `FSMSnapshotState` at all: a restore must
  not silently drop the operator's history.
