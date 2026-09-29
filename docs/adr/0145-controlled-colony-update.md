# ADR-0145: A controlled, one-at-a-time Colony update, with the leader stepping aside

## Status

**Accepted** on 2026-09-27. **Implemented and merged: the backend, and a
page that is served but inert.** The backend is
`ManagerService.ExecuteNodeRestartPlan`, which runs the sequence below for
one named Comb and one service with every effect bound to the real one: the
real `LeadershipTransfer()` over the real raftd socket, real quorum facts
from real raft status and real peer dials, the real raft-replicated restart
lease applied on the real leader, the real pending-restart record, the real
`service apiary_raftd restart`, and the restarted process's own confirmation
read from the durable record rather than self-served. The durable,
raft-replicated colony-wide single-flight is merged with it: `ColonyUpdate`
in the FSM holding the lock and the progress as one object, the four-part
`ColonyUpdateFence` checked exactly, `MutateColonyUpdate` with acquire,
advance, release and handover, and the optional
`AcquireRestartLease.colony_update_fence` that stops a displaced coordinator
from taking the restart leases its own work depends on.

**Accepted but not yet built**, and no code in this repository reflects any
of it:

- **The adapter between the page and the backend.** `GET /colony-update`,
  its panel poll and `POST /colony-update/request` are routed and
  `web/templates/colony_update.html` renders, but `cmd/frontend` never calls
  `SetColonyUpdateController`, so the only `colonyupdate.Controller` in the
  tree is `colonyupdate.Inert`. A request from the page is refused and the
  page says so. This is the Colony view's Update control in everything but
  its ability to act.
- **A sweep.** `ExecuteNodeRestartPlan` takes one `node_id` and one
  `service`. Nothing derives which Comb to touch first or in what order, and
  nothing drives the four Combs back to back.
- **`versioncheck` as the per-step build-identity gate.** Neither
  `internal/manager` nor `internal/restartplan` imports `internal/buildgate`,
  so "what is RUNNING against what is ON DISK" is not checked per step.
- **The ADR-0143 verdicts as the per-step and end-of-sweep gates.** The plan
  imports neither `internal/health` nor reads `ClusterHealth`, so none of the
  three health checks in the table above is wired to anything.
- **A managerd self-restart handoff.** `apiary_managerd` is refused
  outright, unchanged from ADR-0142, so this call can never restart the
  process it lives in.

**Not verified.** None of this has run on a FreeBSD host. The sequence is
exercised against a real raft cluster on loopback with a real gRPC surface,
but `service apiary_raftd restart`, rc.d's stop/start timing and the real
interval between raftd coming back and its startup hook running are macOS
test fakes and nothing more. The design sections below are unchanged, and
the two entries in the "Update 2026-09-27" list at the end that said the
single-flight and the page were unbuilt have been corrected there.

## Context

Updating the Colony today is a manual, four-terminal operation with three
separate ways to get it wrong.

The operator runs `git pull && make update` on every Comb. Per ADR-0141 that
installs all four binaries but restarts only frontend and restshimd, so
managerd and raftd are left running their previous build. The operator then
runs `apiaryctl force-restart` on every Comb to bring those two across. Both steps
look identical from the outside, and the intermediate state is not obviously
wrong: the binaries on disk are new, so anything that checks mtime or a
checksum reports success.

That gap is not hypothetical. On 2026-09-27 the operator pushed `107fdf6` and
ran `make update` on all four Combs. Verification against the running
processes found frontend and restshimd on `107fdf6b414a` while managerd and
raftd were still on `a9879963600c`, installed-but-not-restarted. `versioncheck`
exists precisely to catch this and is not called by the update path; the
Makefile's own closing advice is a hand-written `grep build=` against the log.

Three further problems sit on top:

- **The operator must know who the leader is.** `apiaryctl force-restart` says out
  loud, every time, that restarting the leader can cost the cluster its
  quorum. The only mitigation offered is "never on the leader as part of a
  sweep", which transfers a piece of distributed-systems knowledge onto
  whoever happens to be holding a terminal.
- **A sweep can start from a stale plan.** Leadership moved during the
  2026-09-27 sweep - term advanced to 30 mid-restart - so any ordering
  computed at the start of a sweep is out of date by the time it reaches the
  last Comb.
- **Nothing verifies the result.** `make` exiting zero is the only signal.
  There is no check that the running process took the new build, and none that
  the FSMs still agree afterwards.

The operator's stated requirement is narrow and should be quoted: the
operator should never need to know who the leader is, and should only need to
know the cluster is healthy.

## Decision

### The operator supplies intent, the system derives the plan

An update is driven from the Colony view. All Combs are listed. The operator
clicks "update" on one. The others are greyed out until that update is
confirmed complete. One at a time, in a controlled UI, is the intended shape.

The operator is never asked to choose an order and never shown a leader as a
choice. Leadership is a derived fact, re-derived at execution time.

### The leader steps aside instead of being scheduled last

`apiaryctl force-restart` gives up the guardrail entirely: no lease, no quorum
preflight, no cross-Comb coordination. That is tolerable only because it is a
named, per-Comb, operator-driven act. An automated sweep that calls it would
surrender every property that made it tolerable and gain none of the
coordination being built.

So the leader is not scheduled last. It **steps aside**.

`hashicorp/raft` v1.8.0 provides `LeadershipTransfer()` and
`LeadershipTransferToServer()`. Neither is referenced anywhere in this
repository today. `internal/raft/node.go` builds its config from
`raft.DefaultConfig()`, which sets `ProtocolVersion: ProtocolVersionMax`, so the
protocol-version-3 gate in the library is satisfied and
`LeadershipTransfer()` will not return `ErrUnsupportedProtocol`. The
defaults - 1000ms heartbeat, 1000ms election, 50ms commit - are not overridden.

`LeadershipTransfer()` with no argument is the right call rather than
`LeadershipTransferToServer`: letting the library choose the target is what
keeps the caller from needing to know the membership or nominate a peer.

The library's own comment is worth recording, given this project's history
with mixed voter builds: *"Using transfer leadership is safe however in a
cluster where not every node has the latest version. If a follower cannot be
promoted, it will fail gracefully."*

Why this beats ordering:

- The decision is local, made by the node that is about to be restarted, at
  the instant of the step. Leadership moving mid-sweep does not invalidate
  anything, because whoever holds it at that moment steps down.
- It fires only when needed. Restarting three followers transfers nothing;
  only the step that actually lands on the leader causes a change. It is not
  churnier than ordering.
- The operator's requirement is met by construction: no plan holds a leader's
  name long enough to be wrong.

### It is an RPC with a confirmation, not a command-line flag

A flag that makes raftd step down and stay down is a lie whenever the restart
does not happen: the node would be silently demoted for no reason, with no
record of why. What is wanted is prepare-and-confirm:

1. raftd checks whether it is currently leader.
2. Not leader - return immediately, having done nothing. This is the common
   case, three steps out of four, so it must be cheap and must not require the
   caller to know which branch was taken.
3. Leader - initiate `LeadershipTransfer()`.
4. Wait until a **different** node is leader and this one is a confirmed
   follower.
5. Only then answer that it is safe to restart.

Step 4 is the entire value. "I asked" and "it is now safe" are different
claims, and only the second justifies killing the process. Without it, a
failed transfer followed by a restart leaves the colony without a leader until
an election times out.

### One at a time is a quorum requirement, not politeness

Stepping aside solves leadership. It does nothing for quorum. With four voters
quorum is three; restarting two Combs at once loses it regardless of who is
leading. The UI greying out the other Combs is therefore a correctness control,
and the server must enforce it independently of the UI. A client that ignores
the greyed-out buttons must still be refused.

Ordering does become merely a politeness question - prefer followers first,
since transferring is unnecessary when the leader is not being touched - but
it is never a permission to go parallel.

### The health gate, evaluated per step

The operator's only stated input is health. The tool still needs a gate, and
it needs one that abstains rather than guesses. ADR-0143's four verdicts map
directly:

| Verdict   | Tool does                                            |
| --------- | ---------------------------------------------------- |
| `match`   | proceed                                               |
| `unsettled` | wait and re-read; the colony is moving              |
| `mismatch` | stop; refuse to roll a diverged colony                |
| `unobserved` | stop; no evidence is not permission                |

`mismatch` is the interesting one. A colony that has already diverged is not
one to roll through four restarts, and the digest is the instrument that knows.
The gate is evaluated before each step, not once at the start, for the same
reason leadership is re-read per step.

### Confirmation, not exit codes

Two post-conditions, at two different times, and they are not the same check:

- **Per step, build identity.** What is RUNNING, compared against what is ON
  DISK, per `cmd/versioncheck`'s existing reasoning. A binary copied over a
  running executable leaves new bytes on disk and old ones resident, so a
  matching mtime proves nothing.
- **End of sweep, FSM agreement.** One `ClusterHealth` read, which already
  carries per-voter digest and applied index from ADR-0143 at no extra RPC
  cost.

Digest agreement must not be used as the update's success criterion. It answers
"do the FSMs agree", not "did the update take". After a legitimate update that
changes state the digest *should* change, and a sample taken mid-sweep is
expected to differ - that is `unsettled`, not divergence.

### Relationship to ADR-0142

ADR-0142 refuses `RestartNodeService` and `PreflightRestartNodeService` for
`apiary_managerd` because the coordinating client did not exist. This is that
client.

The sharper answer may not be "permit managerd once a coordinator exists" but
"permit managerd once there is a preflight that handles the leader case".
Stepping aside is that missing half: the lease path already does acquire,
preflight, restart, confirm, and the leader case is the one step it cannot
currently express.

So the controlled path takes a **real lease** per Comb, which is what gives it
the quorum preflight it needs to answer "is it safe to restart *this* one right
now". `apiaryctl force-restart` keeps its current meaning exactly: the operator's
manual escape hatch, and the path the `lease_id: 0` cooldown receipt was built
for. The two are not merged.

### Leadership will have moved when the sweep ends

A node that transferred, restarted, and came back is a follower. The leader
after the sweep is not the leader before it. That is correct under the stated
principle and should be stated in the UI's own words, so it is not read as a
fault.

## Consequences

- The update path stops being a Makefile convention and becomes an RPC. The
  Makefile targets keep their current meaning and remain the manual path.
- `versioncheck` becomes callable from the update flow rather than being
  documentation.
- The Colony view gains an update control whose availability is driven by
  colony health, not by a per-node reachability flag.
- A transfer that fails must stop the step. The colony is left as it was,
  minus a restart that did not happen - which is the recoverable outcome and
  the reason the confirmation precedes the restart.
- If a transfer fails *and* the process dies anyway, the colony is leaderless
  for roughly one heartbeat plus up to one randomized election timeout, then
  recovers with three voters. Survivable, and the reason step 4 gates the
  restart rather than following it.

## Rejected

- **Schedule the leader last.** Stale the moment leadership moves, which it
  did during the 2026-09-27 sweep, and it still requires the operator or tool
  to know who the leader is.
- **A persistent raftd "step aside" flag.** Silently demotes a healthy leader
  whenever the restart does not follow, with nothing recording why.
- **Reusing `apiaryctl force-restart` for the automated sweep.** Surrenders the
  guardrail and gains no coordination.
- **A new digest RPC.** ADR-0143 deliberately added none; `ClusterHealth`
  already carries what is needed.
- **Nominating a transfer target.** Reintroduces the membership knowledge the
  design is trying to remove.

## Open questions

- Where the per-step update state lives: managerd memory is lost if the Comb
  being updated is managerd, which is the Comb being updated.
- Whether the greying-out is advisory in the UI only or the server enforces a
  colony-wide single-flight. It must be enforced server-side; whether the UI
  also blocks is presentation.
- Whether an update in progress should block a second operator session, and
  how that is coordinated when the two operators are on different Combs.

## Update 2026-09-27: one Comb, end to end

`ManagerService.ExecuteNodeRestartPlan` now runs the sequence above for one
named Comb, with every effect bound to the real one. This section is the
record of what had landed as of this date; the `## Status` line above was
still reading "not yet implemented" when it was written, and was corrected
once the colony-wide single-flight and the update page had merged behind
it.

What is real now, and was previously reachable only from a test with a fake on
every boundary:

- `internal/manager/restartcaller.go` composes the real
  `restartStepAsider` (so the real `LeadershipTransfer()` over the real raftd
  socket), real quorum facts from real raft status and real peer dials, the
  real raft-replicated restart lease applied on the real leader, the real
  pending-restart record, the real `service apiary_raftd restart`, and the
  restarted process's own confirmation.
- `internal/manager/raftdquorum.go` gained `raftdQuorumFact`, the read-and-dial
  split out of `evaluateRaftdQuorumSafety` so there is still exactly one
  implementation of "go and find out the real facts". The preview and the
  enforcement cannot drift because there is one function, not two that happen
  to agree today.
- `managerdAddrFromStatus` is the membership-to-address step, shared by the
  step-aside's peer hop and this new forward, so there is one fail-closed
  lookup rather than two.

Three decisions in it are worth recording, because each could reasonably have
gone the other way.

**Execution is target-local.** The pending record is read back by the restarted
raftd on its own next startup and the restart command runs on the machine whose
raftd is stopping, so neither is visible from another node. A request naming
another Comb is therefore forwarded to that member's own managerd, at an
address resolved from this node's own raft membership and never from anything
the caller supplied, and never to the leader.

**The confirmation is not self-served.** ADR-0103's revision note #16 and §2
above both require the restarted process to make it. The plan therefore reads
the durable record `cmd/raftd`'s own startup hook writes rather than calling
`ConfirmRestartCompleted` itself: a plan that confirmed for raftd would be
letting a witness testify to its own resurrection. The lease id must match, so
a stale `confirmed` left by an earlier restart on the same node can never be
credited to this one.

**One at a time is ADR-0103's lease, and nothing more.** A lease already held
for another Comb refuses this call whatever `force` says, because a held lease
is not a known cost an operator can acknowledge - it is evidence that somebody
else's restart is in flight and unconfirmed. ADR-0103's force-override of a
stuck lease stays on `ReserveRestartLease`/`RestartNodeService`, untouched, for
an operator recovering one by hand. That is the whole of the guarantee here,
and it is worth being blunt about its size: it is a per-service lease, not
durable Raft-backed ownership of a Colony update. Two callers racing, or a
caller whose read of the lease state lags the leader, are not closed by it.

The framing from the table above survives: **passing the preflight is
necessary and never sufficient.** The quorum evaluation is step three of
eight, the lease that actually serializes the restart is step four, and the
only thing that releases the lease is a confirmation from the process that was
restarted. A clean `allow` from the preflight means the next step is permitted,
not that the restart will happen or that it worked.

Authorization is the dedicated root-owned restart-guardrail token, exempt from
`checkAuth` like `ReserveRestartLease`, `ConfirmRestartCompleted` and
`StepAsideForRestart`. This call composes all three, so if it were reachable
by an ordinary Admin API key then an ordinary Admin API key could restart a
quorum-critical daemon. There is no UI and no REST route for it, by design.

Still missing, and none of it made smaller by this:

- **Durable update-operation state, and the managerd self-restart handoff.** If
  the managerd running a plan is itself restarted or dies mid-call, the plan is
  over; there is nothing to resume from, and the pending record on disk is the
  only trace. `apiary_managerd` is refused outright, unchanged from ADR-0142,
  so this call can never restart the process it lives in - but the absent piece
  is real, and it is the first open question above. The durable-operation half
  of that bullet landed later the same day, as `ColonyUpdate` step records in
  the FSM; the self-restart handoff has not, and nothing yet acquires that
  state on a plan's behalf, because no coordinator drives one.
- ~~**Colony-wide single-flight**, in progress on another branch.~~ Merged
  on 2026-09-27, in `ColonyUpdate` as raft state, `MutateColonyUpdate`, the
  four-part fence, the cooperative handover, and the optional
  `AcquireRestartLease.colony_update_fence`.
- **The Colony view's Update control**, `versioncheck` as the per-step gate,
  and the ADR-0143 verdicts as the per-step and end-of-sweep gates. The page
  itself landed on 2026-09-27 and is served and linked, but the controller
  behind it is `colonyupdate.Inert`, so it renders and refuses; it is a page
  with no update system attached, not an Update control. None of the three
  health checks in the table above is wired to anything yet.
- **A sweep.** One call, one Comb, one service. Which Comb to touch first, and
  in what order, is not derived by anything here.
- **`apiaryctl force-restart` is untouched** and remains a manual escape hatch. It
  is not wired to this and must not be: the `lease_id: 0` receipt it writes is
  evidence for an un-leased emergency restart, and the automated path must
  never need the weaker record.

What is *not* verified: this has never run on a FreeBSD host. The sequence is
exercised against a real raft cluster on loopback with a real gRPC surface, but
`service apiary_raftd restart`, rc.d's stop/start timing, and the real interval
between raftd coming back and its startup hook running are macOS test fakes and
nothing more. The production confirmation budget is deliberately larger than
`cmd/raftd`'s own (10 attempts 3s apart against 5) for exactly that reason, and
that sizing is reasoned rather than measured.
