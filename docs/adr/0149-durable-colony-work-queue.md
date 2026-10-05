# ADR-0149: A durable, Raft-replicated work queue

Status: Unconfirmed
Supersedes: None
Superseded by: None
Affected projects: Apiary
Legacy identifier: ADR-0149 (Apiary)
Loreloom identifier: Pending allocation by Glen

## Status qualifications

**UNCONFIRMED. Not accepted, not implemented, not reviewed.**

This ADR records a design and a set of refusals, and it records the
owner's responses to the questions that were put to them. It is a
starting point for a conversation, not a decision. Two of the questions
put to the owner were answered "I'm not sure", and they are recorded as
open below rather than resolved by the agent who wrote this document.

Nothing in this repository reflects any of it. No proto field, no FSM
command, no package, no page.

The requirement, verbatim from the owner's note:

> message queueing, particularly in Raft. Store the state on the
> leader, state replicates via message queues and raft.

The owner's narrowing of the scope, verbatim:

> 1: a; 2: I'm not sure. 3: I'm not sure. Note this ADR as unconfirmed,
> and commit the change as-is.

Question 1 asked whether the target is (a) durable delayed/retryable
*work* items, (b) a general leader-mediated message bus for all external
operations, or both. The answer was (a). Everything below concerns work
items only.

The two questions the owner did not answer are reproduced in "What the
owner has not decided", with the reasoning that makes each one a real
question rather than an oversight.

## Context

### What already replicates, and what does not

Apiary's replicated state is a log of *decisions* applied by a state
machine on every voter. `internal/raft/fsm.go` applies a typed `Command`
to in-memory maps, and every replica computes identical results from the
identical log. That property is load-bearing and is asserted in the
schema's own comments: IP and MAC allocation happen in `applyCreateVM`
precisely so two Combs reconciling one log entry assign the same
address, and ADR-0148's disk-size floor refuses to lower inside the
apply function rather than in a handler, "so every replica decides 'never
reduced' from the same committed log rather than being told".

Three commands added since - ADR-0103's restart lease, ADR-0145's
colony-wide update single-flight, ADR-0147 Part 4's join window - exist
for the same reason, in nearly the same words: a guarantee that has to
survive a managerd restart and hold across Combs cannot live in one
process's memory.

### The gap: work in progress is not replicated

Every one of those mechanisms records *durable facts about an operation*
- a lease, a step, a window, a pin. None of them records *the work
itself*, and the distinction is not academic. Three subsystems in this
codebase each run a leader-side consumer with its own pending set, and
each has the same two properties:

| Subsystem | Where pending work lives | Lost on leadership change? |
| --- | --- | --- |
| ADR-0134 notifications | leader's in-memory `Engine` pending set, bounded at 512, oldest-first eviction | yes, silently |
| ADR-0131 backups | `BackupPolicy` is raft-replicated; the *due set* is computed by the leader and the outcome recorded after the fact | the due set; a backup due during an election is simply not run |
| ADR-0145 controlled update | `ColonyUpdate.steps` is replicated, as is the single-flight, but nothing names which steps remain | the remainder of a sweep: a replacement coordinator finds the progress and no successor |

The third is the sharpest. `ColonyUpdateStepRecord` deliberately records
outcomes at the ordinal positions a coordinator reached, and
`appendColonyUpdateStep`'s own comment names the consequence: "a gap
means a reader could be shown a sequence with a hole it cannot explain".
A replacement coordinator reading that history cannot tell the difference
between "step 7 was never reached" and "step 7 was reached and its
outcome was never recorded". A queue would make the remaining work a
replicated fact rather than an inference.

The first is the most consequential in the field. ADR-0134 accepted its
loss deliberately and named the cost: "a leader crash loses it and the
missing record is honest about that". That acceptance is correct for v1
and it is the specific tradeoff this ADR would change.

### Why this is not "add a queue to Raft"

Two readings of the requirement are worth separating before designing
anything, because one of them is a much larger project and one of them
is already paid for.

**Reading 1: re-implement message delivery between members.** Rejected.
`hashicorp/raft` is already an ordered, replicated message transport
between members - AppendEntries, heartbeats, and the leader's replication
stream. Its log is a durable, totally ordered queue of state changes. A
second message layer beside it would duplicate guarantees raft already
provides while adding none of the properties this codebase actually
needs, which are about *authority* and *timing*, not delivery.

**Reading 2: a durable queue of work items, replicated through the
existing log, consumed by one leader.** This is what this ADR is about,
and it is genuinely new: nothing in the tree has ever replicated "what
remains to be done" as state.

The distinction matters because the phrase "message queueing in Raft"
sounds like it is about the first thing and is about the second.

### What "state on the leader" conflicts with, stated plainly

The requirement's "store the state on the leader" is in direct tension
with three things this repository has decided deliberately, and the
tension is not resolved by this document.

1. **Replica determinism.** Leader-held state carries no guarantee that
   two Combs compute the same answer, which is the property the FSM
   exists to provide. A queue whose *content* is leader-only is a second
   source of truth beside the FSM.

2. **Deliberately non-leader-restricted reads.** `ListVMsLocal`,
   `GetColonyDiskSizeLocal`, `ListTrustedPeersLocal` and
   `GetColonyUpdateStateLocal` are all leader-exempt on purpose, so a
   follower answers for itself without a hop, and each says so in its own
   doc comment. A follower that could not read the queue could not answer
   "what is this Colony's backlog", and a design that made that depend on
   the leader would be a regression against four existing decisions.

3. **Leader change is routine, not exceptional.** The sweep in ADR-0145
   re-reads leadership per step *because it moves during a sweep* - term
   30 was observed mid-restart on 2026-09-27. State that only exists on
   the leader is state that vanishes on a schedule the operator does not
   choose.

The resolution this ADR proposes - and the reason it is unconfirmed - is
that only *consumption* is leader-only. The item's content is replicated
FSM state like everything else. Whether that is what the owner meant by
"state on the leader" is question 2 below, and it was not answered.

## What the owner has not decided

### Question 2: is only consumption leader-only, or the content too?

**Answered: "I'm not sure". Recorded as open.**

The proposal is that a queue item is raft-replicated state, exactly like
`ColonyUpdate` or `TrustedPeer`, and that at most one Comb consumes the
queue at a time. That satisfies "state on the leader" operationally - one
Comb does the work, one Comb holds the exclusive claim - while keeping
every replica's answer identical.

It is not literally what was written. A literal reading puts the content
behind the leader, and that reading has the three costs listed above.

This question decides the design, and it is the main reason the ADR is
unconfirmed. Both answers are defensible; they are different systems.

### Question 3: does this replace ADR-0134's in-memory pending set?

**Answered: "I'm not sure". Recorded as open.**

ADR-0134 is `Proposed` and unbuilt, and its pending set is described in
its own Implementation notes as living "only in the leader's in-memory
engine, so a leader crash loses it". Two coherent positions:

- **Keep it as it is.** v1's loss window is bounded and honest, and the
  in-app record is the durable artifact anyway. Replicating the pending
  set adds a raft write on a path whose whole design goal is that it
  never touches raft from the operation's critical path.
- **Replace it.** Then ADR-0134 needs an explicit amendment recording
  that the tradeoff it accepted has changed, and that a
  `NotificationRecord` may now be preceded by a replicated enqueue.

The second option is not free. ADR-0134's central safety property is
that a notification must never block, slow, fail, roll back or extend the
latency of the operation that produced it. A replicated enqueue is a raft
write on the producing operation's path - a quorum round trip - so the
two designs are in genuine tension, not merely different in size.

A shape that might satisfy both is named under "A shape that could
satisfy both". It is not adopted.

## Decision

Provisional, and contingent on the two open questions above.

### 1. A queue item is replicated state, consumed by one Comb at a time

A queue item is raft-replicated FSM state, added to
`FSMSnapshotState` and keyed by item id. It carries:

- an opaque `payload` for the work itself;
- a `kind` drawn from a closed vocabulary, never a free string (§6);
- the item's state: `pending`, `claimed`, `done`, `failed`,
  `abandoned`;
- `not_before_unix`, which is what makes this a queue rather than a set;
- `attempts`, the bounded attempt counter;
- the claim: which Comb, and which of its managerd incarnations.

Every timestamp on the item is authored by the leader that applied the
command, never accepted from the caller, for exactly the reason
`applyAcquireRestartLease` stamps its own `requested_at_unix`: a replica
replaying the log years later must reach the identical decision.

The claim is `ColonyUpdateFence`'s shape, deliberately - holder node,
holder incarnation, and a fence token minted from the command's own log
index. A displaced claim-holder is fenced out of the *next* transition on
every replica at the same instant, and that is the only mechanism in this
codebase that has ever done it. `AcquireRestartLease`'s optional
`colony_update_fence` field is the existing proof that this composes.

Field numbers are **not** allocated here. Protobuf field numbers are a
permanent wire contract and this repository allocates them centrally
(commit `9c43262`, recorded in SHARED.md). Proposing numbers in an ADR
is not allocation, and there are none proposed.

### 2. Consumption is single-consumer, and a lost claim is a fact

One Comb claims an item; no other Comb may work it. The claim is a raft
command, so two consumers racing for the same item cannot both be granted
- the same argument as `applyAcquireColonyUpdate`.

**A claim that is never released is visible, not silent.** An item
`claimed` by a Comb that has died does not quietly return to `pending`.
It stays `claimed`, is surfaced as `stale_claim`, and is recoverable only
by an explicit, logged `ReclaimQueueItem`, with the same posture as
`AcquireColonyUpdate.takeover`: loud, explicit, never automatic. An item
that silently returned to `pending` would be work executed twice by two
Combs, which for a backup or a notification is worse than an item that
needs an operator to say "this one is stuck".

There is no TTL on a claim. ADR-0103's `RestartLease` and ADR-0145's
`ColonyUpdateFence` both refuse a time-based auto-clear, and that
reasoning transfers verbatim: a claim that lapses on a timer can lapse
while the underlying work is still genuinely mid-flight, which is
exactly the concurrent-execution window the claim exists to close. No
item may ever be worked by two Combs at once.

### 2.1 The attempt counter is bounded, and exhaustion is a named outcome

An item that fails is neither dropped nor retried forever. It advances
`attempts` and becomes `failed` at a bound stated here rather than
clamped: `maxQueueAttempts = 6`, admitted as a guess in the same spirit
as ADR-0134's six-attempt delivery schedule and ADR-0125's five-attempt
confirmation budget. Every attempt's verbatim reason is retained on the
item.

Every attempt's *reason* is retained because ADR-0134's
`NotificationRecord.error` retains one: "the operator needs the reason,
not the category". A failure whose reason was smoothed into a category
is a failure nobody can act on.

### 2.2 Nothing here is a delivery guarantee, and the naming must not imply one

A queue guarantees at-least-once *claim*, not at-least-once
*execution*. An item marked `done` is the consumer's own claim about its
own work; no other replica witnesses it. This is the same shape as
ADR-0125's restart confirmation, and it inherits that ADR's own limit: no
witness exists here either.

The vocabulary must therefore refuse "delivered", "completed",
"performed" and "executed" for an item a consumer marked `done`. The
permitted words are `done` and `unobserved`. An operator reading
"delivered" on a backup item would infer a verified artifact, and for
ADR-0131 an unverified backup "is an assumption, not a backup".

### 3. `not_before_unix` is checked at read and claim time, never in apply

An item is eligible when `not_before_unix <= now`. The wall clock is
consulted by the consumer at claim time, and by any read that reports
eligibility - never inside an apply function, for the determinism reason
`applyResolvePendingJoinRequest` gives in full: it runs on every replica
and again on every replay, so a clock read there would let a replay after
the deadline settle differently from the node that applied the entry
live.

The consequence is worth stating rather than glossing: an item becomes
eligible by the passage of time alone, and time passes identically
everywhere, so *eligibility* is genuinely consistent across replicas. But
*consumption* is not, and cannot be, which is exactly why the claim is a
raft command rather than a local lock.

### 4. The queue is not on the operation's critical path

The operation that produces an item enqueues non-blocking, and the
enqueue never delays it. ADR-0134 established this as a structural
property - the trigger package must not transitively import `net/http`,
asserted by a dependency test - and this ADR inherits it for the enqueue
side unchanged.

The enqueue is itself a raft command, so it costs a quorum round trip,
and this ADR does not pretend otherwise. The claim is narrower: the
*producer* is not blocked by the *consumer*, and an enqueue that cannot
commit is a counted drop rather than a failure of the producing
operation. See question 3 for why this sits in genuine tension with
ADR-0134's design.

### 4.1 An enqueue that cannot commit is a visible drop, never a silent one

An enqueue that times out or loses quorum increments a counted drop
(`QueueStatus.dropped_enqueues`) and is surfaced in the UI. It is never
swallowed. The counter is first-class and never reset to zero without
acknowledgement, for ADR-0134's reason: "a notification system that
silently discards events under load is indistinguishable from one that
has stopped working".

### 4.2 No socket, no retry timer and no network I/O inside `apply`

This is the central refusal, and it is inherited from ADR-0134's
rejected alternatives, which rejected delivering from `Apply` for reasons
that do not depend on notifications at all.

- **No delivery from `apply`.** `apply` runs on every voter and holds the
  FSM's lock. A socket behind it would fan one cluster event out to N
  deliveries and stall every subsequent state change on every Comb. The
  worst blast radius, applied to the component every other component in
  the system depends on.
- **No retry timers in the FSM.** A timer in `apply` is non-deterministic
  by construction, and every timestamp the FSM writes is leader-authored
  rather than derived.
- **The attempt record is written after the fact.** If question 3
  resolves against ADR-0134, the honest way to keep that ADR's property
  is that the attempt record is written by the consumer *after* it
  attempts, strictly after the producing operation has already completed.
  That record is still a raft write, so the concern is real and is
  recorded here rather than dissolved.

### 4.3 A dropped enqueue is not recoverable by re-reading the log

A dropped enqueue means the item does not exist. It is not on the log, so
replaying the log cannot find it. This ADR does not resolve that and
names it as a limitation rather than designing a recovery path it has no
evidence for.

### 5. The item `kind` is a closed vocabulary

An item's `payload` is opaque bytes, so what an item actually *does* is
whatever its consumer makes of it. That is the point of a queue and it is
also its blast radius: whoever can enqueue can ask the Colony to do
something, and a new consumer is a new privileged action.

So `kind` is a closed enumeration, not a free string, for the same
reason `internal/guardrail`'s finding `Rule` is a stable ID rather than
prose - so two Combs render the same item identically, and so the set of
things this Colony can be asked to do is enumerable by reading the
schema. A queue without a closed vocabulary is a queue without a bound,
and an unbounded queue in a component that runs during incidents is a
memory leak with a latency cliff.

### 6. No new per-Comb retry timers

Three subsystems already each hand-roll their own backoff
(ADR-0125's confirmation budget, ADR-0134's delivery schedule, and the
restart plan's own). This ADR does not add a fourth mechanism for the
timer itself; `not_before_unix` and `attempts` are the whole of the
scheduling state, and a consumer that wants a different cadence changes
it in its own code rather than in the queue.

## Rejected alternatives

- **Put the queue's content on the leader.** Rejected *provisionally*,
  and only because it conflicts with the three decisions in "What 'state
  on the leader' conflicts with". This is question 2, and the owner has
  not answered it. If the owner's reading is the literal one, this
  rejection is wrong and the design above is wrong with it.

- **A general leader-mediated message bus for all external operations.**
  Rejected on the owner's own narrowing: the answer to question 1 was
  (a), durable work items. A general bus is a much larger design and is
  not proposed here.

- **A second transport layer beside hashicorp/raft.** Rejected: raft is
  already an ordered replicated message transport between members, and a
  second one duplicates guarantees without adding authority or timing.

- **Automatic claim expiry on a timer.** Rejected for the reason
  `RestartLease` gives in full: a claim that lapses on a timer can lapse
  while the underlying work is still genuinely mid-flight. No item may be
  worked by two Combs at once, and a timer cannot know that.

- **Automatic claim recovery on leadership change.** Rejected for the
  reason ADR-0145 records: routing a planned transfer through a seizure
  flag would set `takeover = true` on the common path and leave a healthy
  sweep indistinguishable in the record from a cluster fight.

- **Deliver from `apply`.** Rejected, inherited from ADR-0134.

- **Silent overflow.** Rejected, inherited from ADR-0134.

## A shape that could satisfy both

If question 3 resolves toward replacement, there is one shape that
preserves ADR-0134's central property *and* replicates the pending set,
and it is offered as a candidate rather than as a decision:

The producer enqueues **asynchronously relative to its own operation** -
it hands the item to a local bounded buffer and returns, and a separate
loop drains that buffer into raft. The operation's latency is then
unaffected by the quorum round trip, because the round trip happens after
the operation has already returned. The buffer's overflow is the counted
drop of §4.1.

The cost is a new failure surface: the buffer is process-local, so an
item handed to it and not yet drained is lost exactly as ADR-0134's
in-memory set is today. That is a strict improvement on the current
design and not a cure, and it is the honest limit of this shape.

## Consequences

### Positive

- **A replacement coordinator knows what remains.** `ColonyUpdate.steps`
  currently records outcomes at positions reached; a queue makes the
  remaining steps a replicated fact. This is the sharpest gain, and it is
  what ADR-0145's own open questions ask for.
- **Work survives a leader change** for three subsystems that each lose
  it today, one of which (ADR-0134) accepted that loss deliberately and
  named it.
- **One home for retry and backoff state.** `not_before_unix` and
  `attempts` replace three separately hand-rolled pending sets.

### Negative

- **Every enqueue is a raft write.** On a trigger's path that is a
  quorum round trip, and it is in genuine tension with ADR-0134's central
  property. Unresolved; see question 3.
- **A stuck claim needs an operator.** By design (§2), no automatic
  recovery, at the cost of a queue that reads `stale_claim` rather than
  one that runs a backup twice.
- **Nothing here is witness-backed** (§2.2). An item marked `done` is
  the consumer's own claim, not a verified artifact.
- **An operator can now enqueue arbitrary work** (§5). That is the
  feature, and it is also a new privileged surface.

## What this does not claim

- **Not verified anywhere.** No code exists. Nothing here has run on
  FreeBSD, in a raft cluster, or at all.
- **No field numbers allocated**, and no proto change made. See §1.
- **No amendment to ADR-0134 has been made**, because question 3 is
  unanswered. This ADR records the tension; it does not resolve it.
- **Not a replacement for raft's own transport**, and not a second
  delivery mechanism between Combs. See "Why this is not 'add a queue to
  Raft'".
- **The attempt bound of 6 is a guess**, stated as one so that a later
  reader does not mistake it for a measurement.
- **The first consumer is unnamed.** Which subsystem adopts this first is
  not decided here, and picking one would prejudge question 3.

## Open questions

1. **Is only consumption leader-only, or the content too?** (§ above.)
   Unanswered. Decides the design.
2. **Does this replace ADR-0134's in-memory pending set?** Unanswered,
   and in tension with that ADR's central safety property.
3. **Which subsystem is the first consumer?** Candidates are ADR-0134's
   delivery goroutine, ADR-0131's due set, and ADR-0145's sweep
   remainder. The sweep is the one with the strongest claim on it and the
   most to lose from being wrong.
4. **Is `payload` opaque bytes, or a typed union?** Opaque is simpler
   and makes the queue a genuine generalisation; a typed union keeps the
   set of legal work enumerable by the schema. §5 argues for a closed
   `kind` regardless, which is most of the benefit of the union without
   the coupling.
5. **Should a `done` item be retained, evicted, or both?** The history
   question. ADR-0134 caps records at 2000 and ADR-0145 caps settled
   operations at a bounded number, so precedent exists for a cap, but
   neither says what the right number is for work items.
6. **Does an abandoned item need a reason, on the item or only in the
   log?** ADR-0146 makes `HandoverColonyUpdate.reason` mandatory for
   exactly this reason. Probably yes; not decided.

## Owner clarification (2026-10-05)

Glen corrected the requirement's terminology to "delivered", not
"replicated", and confirmed that queue contents are delivered to every
member. This resolves the content-recipient part of historical question 2
and open question 1 above. The earlier unanswered response remains history.

This clarification does not decide which members consume work. Leader-only
consumption remains a proposal awaiting confirmation. Delivery to every
member does not by itself specify storage, persistence, transport, ordering,
or a delivery guarantee. The earlier replicated-FSM design remains a
provisional design and must be reviewed against this clarified requirement.
The record remains Unconfirmed; the other open questions remain outstanding.
