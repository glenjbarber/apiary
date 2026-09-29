# ADR-0146: Let a controlled update restart the managerd that is coordinating it

## Status

Accepted. **The state model was implemented first and is merged.** The
durable `ColonyUpdate` record, the `ColonyUpdateFence` that binds a
restart lease to the operation actually running, and
`HandoverColonyUpdate` as a distinct command over that record are all
in the tree and tested, and that is the whole of section "The gap in the
current state model" below.

**The mechanical half is now implemented too.** All six rules are in
`internal/manager/handoff.go`, reachable only over the dedicated
root-owned restart-guardrail token, with no operator surface:

- **Rule 1** is enforced twice, in
  `Server.RequestManagerdRestart` and again on the target. A managerd
  that names its own Comb as the target is refused by name, and so is a
  target that the presented fence says is the operation's holder. The
  second check is not redundancy: the first is on the other side of a
  network hop, and a check that exists on one side of a hop can be aimed
  around by a caller who reaches the other side directly.
- **Rule 2** is `HandoverColonyUpdate`, reached from the target-side
  handler. A target that is the operation's holder moves the operation
  to a named successor before it arranges its own death, and refuses
  outright when no successor is named rather than inventing one.
- **Rule 3** is two RPCs, `RequestManagerdRestart` on the coordinator
  and `IssueManagerdRestart` on the target, both target-local in
  execution and never leader-forwarded. The self-restart refusal is
  preserved with no `force` field to override it, because there is no
  version of force that turns "the process that would perform the start
  is the process being stopped" into a known cost.
- **Rule 4** is `setsidServiceRestarter`: a child in its own session,
  started from a background context, waiting inside the child and then
  `exec`ing `service apiary_managerd restart`. **It is still asserted
  rather than measured.** The three failure modes the rule names are each
  closed by a specific property - a new session so the child is not in
  managerd's process group, a background context so nothing cancels it
  when the handler returns, the delay inside the child rather than in a
  parent that is about to be killed - and those properties are pinned by
  tests. What no macOS test can supply is the reproduction on a real
  Comb, and that remains outstanding.
- **Rule 5** is `Server.ConfirmManagerdRestartHandoff`, run by
  cmd/managerd on this process's own next startup. It runs the real
  `internal/buildgate` over the real installed binary and the real
  startup log, confirms the restart lease through the existing
  `ConfirmRestartCompletedLocal`, and appends the step outcome to the
  operation's durable history. It never runs for anyone else, which is
  the entire point.
- **Rule 6** is the failure handling of that same path. No marker is an
  ordinary startup and is silent. A marker that exists and cannot be
  read, a marker for a different Comb, a marker with no recorded
  expected build, and a marker whose step could not be recorded are all
  `unobserved`, all keep the record, and none of them is a success. A
  Comb that comes back on a build nobody asked for is `failed`, not
  `unknown`, because that is positive evidence.

**What is still not built is the sweep, and it belongs to ADR-0145.**
Nothing yet decides which Comb to update, in what order, or when to
stop, and `colonyupdate.Inert` is still the only `Controller`
implementation, so no operator request reaches any of this. That is the
same boundary `ExecuteNodeRestartPlan` was delivered under: the guarded
mechanism is real and reachable, the coordinator that drives it is a
later step.

**Two boundaries this implementation deliberately did not cross.** No
UI control, route or template was added: ADR-0142's refusal stands in
`RestartNodeService`, and the Machine page's managerd row is status
only, because the handoff is a step of a controlled update and not a
general "bounce the daemon" control. And the detached restart is not
automated anywhere: `apiaryctl force-restart` remains the manual path,
and the controlled path is the only one that carries the guardrail.

Depends on the durable `ColonyUpdate` record and its `ColonyUpdateFence`,
merged into `main`, which carries `HandoverColonyUpdate` specifically
to close the gap this ADR identified. Section "The gap in the current
state model" records how that was closed.

Amends ADR-0142. Implements the second open question of ADR-0145.

## Context

ADR-0145 sets the invariant: the operator supplies update intent, the
system derives execution order, exactly one Comb is updated at a time,
and every step is server-confirmed. ADR-0145's first open question is
where the per-step state lives, and answers itself by pointing at the
hardest case:

> managerd memory is lost if the Comb being updated is managerd, which is
> the Comb being updated.

ADR-0142 is the reason this is hard, and it is not a style preference.
It records a fault reproduced on two Combs: `RestartNodeService` runs the
restart in a goroutine inside the calling managerd, the stop half of
`service apiary_managerd restart` kills managerd, and the goroutine dies
before the start half runs. The service stops and does not come back. A
manual `service apiary_managerd start` was required each time.

ADR-0142's conclusion is that **no in-process orchestration of managerd's
own death can work**, because the process that would perform the start is
the process the stop terminates. Its chosen answer is to refuse
`apiary_managerd` outright and point the operator at `make
force-restart`, which is deliberately manual and bypasses the guardrail.

That answer is correct for today and is not the answer for a controlled
update. A colony update that cannot update the Comb running it is not a
colony update; it is an update that stops one short, every time, and the
operator is back to logging into nodes and knowing which is the leader,
which is the exact thing the operator asked to eliminate.

## The durable state, and what it already settles

The in-flight single-flight work puts a `ColonyUpdate` record in raft
state, holding `operation_id`, `holder_node_id`, `holder_incarnation`, a
`fence_token` taken from the raft log index of the granting acquire,
`target_node_id`, `step`, the per-step outcome list with evidence, and an
`active` flag. Single-flight is enforced by the FSM's serialized
apply order, the same serialization point `AcquireRestartLease` already
uses for the per-service restart lease.

The load-bearing part for this ADR is not the single-flight. It is the
fence on `AcquireRestartLease`: a restart lease may name a
`ColonyUpdateFence`, and the FSM then requires an active record whose
four fields match exactly, refusing otherwise. **A coordinator that has
been displaced cannot take a new restart lease, no matter how many steps
it still has queued.** That is the fencing, and it is correct.

So the state layer already answers the question ADR-0145 asked. What it
does not answer, and cannot, is the mechanical question: **managerd on
Comb X is being restarted. Which process performs the start?**

## Decision

Six rules. The first two are the architecture, the third is the missing
RPC, the fourth is the part that must be proven before it is trusted, and
the last two are the honesty requirements that make a failure legible.

### 1. The coordinator is never the target of a step

Before any step begins on a Comb, the operation must be held by a
`holder_node_id` that is **not** that Comb. The target is chosen to
satisfy this, at execution time, from the live holder.

This is not the operator's ordering decision and not a cached plan. It is
a constraint the system re-derives per step, from state that is current
because it lives in raft. The operator still never learns who the leader
is, still never chooses an order, and still never maintains a
leader-last plan. The constraint is also trivially satisfiable: with four
voters and one holder there is always a non-holder Comb.

### 2. When the only remaining Comb is the holder's own, hand the operation over first

A colony-wide update names every Comb, so eventually the holder's own
Comb is the only one left. Before that step, ownership moves to a
different Comb, and the new holder runs the step.

Consequence to accept openly: **handover is the normal path at the end of
a colony-wide sweep, not an exceptional event.** The current state model
does not have a first-class way to express that, which is the gap in the
next section.

### 3. A peer managerd issues the restart, over a target-local RPC

The RPC mirrors `StepAsideForRestart`, which already has exactly the
right shape and the right reasoning: target-local, **never
leader-forwarded**, authorized with the dedicated root-owned
restart-guardrail token rather than ordinary API-key auth, refusing any
request that names a different node, and fail-closed on every ambiguity.

Under rule 1 this is the ordinary case. A managerd on Comb Y asks
managerd on Comb X to restart X's own managerd, and X's managerd is the
process that dies, not the process that has to survive it.

A managerd asked to restart **itself** over this RPC must refuse, naming
the reason and naming the alternative, in the same shape and with the
same non-overridable semantics ADR-0142 established. `Force` does not
override it. Overriding a quorum block is acknowledging a known risk;
overriding "this will not work at all" is a different thing and must not
be presented as the same lever.

### 4. The self-restart must be performed by a process that is not managerd, and this must be proven before it is trusted

This is the load-bearing mechanical claim, and it is the one place where
this ADR is asserting something **not yet verified**.

The target-local handler must, in order:

1. perform the durable handover if it is the holder, so the operation
   survives it;
2. write a durable "restart scheduled" marker naming itself, the
   operation, the step, and the expected post-restart build identity;
3. **ACK the peer**;
4. only then arrange the restart, in a process that is reparented away
   from managerd: not in managerd's process group, not holding the
   request context, delayed enough for the ACK to reach the peer, and
   responsible for **both** halves of the service restart.

Performing the restart in managerd's own goroutine, even after a correct
handover, is the ADR-0142 failure. The handover makes the operation
survivable; it does not make the process survive. Those are two separate
problems and only one of them is solved by the state layer.

Why the mechanism is plausible but unproven: `apiaryctl force-restart`
already performs both halves successfully from a process that is not
managerd, verified live across all four Combs (ADR-0141). What has
**never** been done is forking that process out of a live managerd at
exactly the moment managerd is about to be stopped. The realistic failure
modes are that the child is killed with the process group, that it
inherits a context that gets cancelled, or that it dies before reaching
the start half. This must be reproduced on brood and drone on purpose
before the controlled update is trusted with a colony. A design that has
only been argued is not a design that has been shown.

### 5. Confirmation comes from the replacement managerd, never from the old one

The dying process must not be the thing that reports the step succeeded.
It is in no position to know.

At startup, a managerd reads the durable operation record. If it finds
itself named as `target_node_id` with a step of "restart issued", it
confirms: run the build-identity gate (running versus on-disk), confirm
the restart lease through the existing `ConfirmRestartCompletedLocal`,
append the step outcome with its evidence, and then either continue the
sweep or hand over again if it is the holder.

The confirmation must be exactly as strict as the ADR-0103 and ADR-0141
rules already in place: exact service, exact holder, exact lease, and a
cooldown driven by the RestartRecord rather than by a timestamp anyone
edits. Lease zero remains the emergency path and remains manual.

What the replacement contributes is the confirmation and nothing else.
Continuing the sweep is the coordinator's work, not this process's: a
replacement managerd is not the coordinator, and under rule 1 it is
almost never the holder either. `ConfirmManagerdRestartHandoff`
therefore records the step and stops. It does not decide what happens
next, and the ADR-0143 question at the end of this section - whether a
replacement whose colony disagrees may continue at all - is exactly the
question a coordinator has to answer, which is another reason the answer
does not belong in the process that just came up.

The fence it records against is read back from its own raftd rather than
taken from the marker, and that does not weaken the fencing. A
replacement has no fence of its own to present - it is not the
coordinator - so what it presents is a claim that "this is the operation
my durable marker names, as this node's raftd currently has it". The
FSM re-checks the same four fields against the active record by exact
match, so a follower's copy lagging the leader by a commit produces a
precise refusal rather than a step record attached to the wrong
operation.

### 6. Absent evidence is `unobserved`, and it stops

If a replacement managerd comes up and cannot find the record it expects,
the operation is `unobserved`. That is neither success nor failure, and
it is never rendered as either.

This matters more here than anywhere else in the workflow, because the
failure mode is silence: the old process is dead, it cannot report its
own death, and nothing is left to notice the gap except a startup read.
A gate that cannot read its evidence must not report success, and an
operation whose evidence is missing must not continue on the assumption
that things are probably fine.

## The gap in the current state model

`AcquireColonyUpdate` has a `takeover` flag, documented as the
explicit, logged override for an operation already in progress, with no
TTL and no silent expiry, and with `takeover = true` recorded on the
resulting grant so an operator reading a stuck update can see that one
happened.

A **planned** handover under rule 2 is not a takeover. It is
cooperative, expected, and part of finishing a colony sweep. Routing it
through the takeover flag would set `takeover = true` on the common path
and leave a properly running colony-wide update indistinguishable, in the
record, from a second coordinator having seized a live operation from an
unresponsive one. That destroys the flag's only purpose.

### Closed: `HandoverColonyUpdate`

The first of the two options above was taken. `HandoverColonyUpdate` is
a distinct command carrying the outgoing holder's fence and the incoming
holder's identity, and the three requirements this section set out are
how it meets them:

- **The outgoing holder's exact fence must still match.** It goes through
  the same `fenceMatchesActive` rule as `AdvanceColonyUpdate` and
  `ReleaseColonyUpdate`, and gets the same named refusal. A coordinator
  that has already been displaced, or whose operation was taken over,
  cannot hand over an operation it no longer holds.
- **The record names both holders and the reason.** Each handover appends
  a `ColonyUpdateHandover` carrying the outgoing node, incarnation and
  fence token, the incoming node and incarnation, the new fence token,
  and the reason, stamped by the applying leader. It is kept in its own
  `ColonyUpdate.handovers` list rather than folded into `steps`, because
  a handover is not a phase of the plan and has no outcome to record.
- **A handover is not a way to escape fencing.** It does not settle the
  record: the operation keeps its id, its step history, its target and
  its progress, and only the holder changes. It mints a higher
  `fence_token` from its own log index, exactly as an acquire does, so
  the incoming holder faces the identical exact-match requirement as any
  other and the outgoing token is stale the instant it commits.

Two details that are easy to get wrong and were not left to chance.
`takeover` is left untouched by a handover and stays sticky, because it
is a record of what happened rather than a description of the current
holder: an operation that was seized and later handed over twice is still
an operation that was seized. And a handover to the identity it already
holds is refused outright, because minting a higher fence token and
appending a record for a change of coordinator that did not happen would
fill the one list an operator reads precisely because it is trustworthy.

Membership of the receiving Comb is deliberately **not** checked in the
FSM. It has no authoritative view of Colony membership - ADR-0103's own
`AcquireRestartLease` takes its voter list from the calling command for
the same reason - so a check written in the consensus layer could only be
a guess. A half-check there is worse than none, because it looks
load-bearing and can be wrong.

## Open questions

- **Rule 1 and who the acquire names as holder.** A verified tension,
  not a design preference, and it is worth stating precisely because
  rule 1 and one of the rejected alternatives below point opposite
  ways. `AcquireColonyUpdate` is authored by the node that applies it,
  and after the leader forward every acquire performs, that is the raft
  leader - so a freshly acquired operation is always held by the leader,
  whatever Comb asked for it. "Make the coordinator always the Raft
  leader" is rejected in this ADR, precisely because leadership moves
  for reasons of its own. The two are reconciled today only by rule 1's
  check being a constraint the coordinator re-derives per step: the
  leader simply must not be the Comb being updated, and at the end of a
  sweep the operation is handed over (which does accept a non-leader
  holder, and does so explicitly). What that costs is not yet
  established, and whether the acquire should carry the requesting
  Comb's own identity instead of the applying node's is a decision about
  ADR-0145's fence rather than about this handoff. It is recorded here
  rather than changed, because changing it is an architecture decision
  and this implementation is a mechanism, not a redesign.
- **Who may take over.** As designed, `takeover: true` is the only route
  to a second coordinator, and it is deliberate. If any operator session
  can set it, a second browser tab can kill a running colony update. It
  needs its own authorization, and the UI must not offer it while an
  operation is active.
- **Liveness, without a TTL.** The fence has no expiry by design, which
  is correct: a TTL would silently stop an update mid-colony. The cost is
  that a crashed coordinator's operation stays `active` forever. The UI
  needs to show "stalled, takeover required" without guessing, which
  means a liveness **signal** that is explicitly not an authority. Nothing
  may expire the operation on a timer.
- **Is a full-colony update the normal case?** If it is, handover is on
  the common path and deserves a first-class UI state, not a footnote.
- **What happens to a target whose managerd comes back and finds the
  colony in disagreement.** The replacement confirms its own step, but
  whether it may continue the sweep at all is an ADR-0143 verdict
  question, and the answer must be that a mismatched or unobserved colony
  stops the sweep rather than merely annotating it.

## Rejected alternatives

- **Keep ADR-0142's refusal, and let the last Comb be updated by hand.**
  This is what happens by default if this ADR is not implemented. It
  leaves a manual, leader-aware step at the end of every colony update,
  which is the thing the operator asked to remove.
- **Have managerd fork the restart itself, with a delay, in its own
  goroutine.** This is ADR-0142's failure with a longer sleep. The
  process that would perform the start is still the process being
  stopped.
- **Have the operator's browser poll and reissue the restart.** The
  operator must not be the recovery mechanism. If resuming an update
  needs a human with a browser open, it is not resumable.
- **Make the coordinator always the Raft leader.** Leadership moves for
  reasons of its own, including the step-aside that deliberately pushes
  it off a target. Coupling ownership to leadership couples the update to
  the transfer it is trying to survive.
- **A TTL on the fence, to let a stalled operation expire.** A timeout
  that ends a colony update halfway is worse than an operation an
  operator has to take over deliberately.

## Consequences

- **managerd becomes restartable by a peer and still refuses itself.**
  ADR-0142's ordering guarantee survives: the self-refusal happens before
  any lease is taken, so no lease is ever held for a service that cannot
  be restarted, and the invariant test that asserts that ordering keeps
  asserting it.
- **`apiaryctl force-restart` remains manual only.** Nothing here automates
  it. The emergency path and the controlled path stay separate, and the
  controlled path is the one that carries the guardrail.
- **The controlled path gains a failure mode the emergency path does
  not have**: a detached child process, on a real Comb, at the exact
  moment the control plane is going down. That is a genuine new risk,
  bought deliberately, and it is why rule 4 requires reproduction on
  brood and drone before the colony is trusted with it.
- **A restart of managerd is now a normal event during a colony update**,
  which means the replacement-startup path in rule 5 stops being a
  recovery path and becomes a routine one. It has to be boring.

## Related

- ADR-0142 -- why managerd cannot restart itself, and the self-refusal
  this ADR preserves.
- ADR-0145 -- the controlled colony update, and the open question this
  ADR answers.
- ADR-0103 -- no-TTL restart leases, and why the fence here has no TTL
  either.
- ADR-0125 -- the quorum-safe restart sequence a handoff step still has
  to pass.
- ADR-0141 -- the manual `force-restart` path whose mechanism rule 4
  borrows, and whose provenance it must be careful about.
- ADR-0143 -- the digest verdicts that decide whether a replacement
  managerd may continue a sweep.
