# ADR-0182: Colony epoch, Stage 2 of ADR-0156

Status: Accepted
Supersedes: None
Superseded by: None
Affected projects: Apiary
Legacy identifier: ADR-0182 (Apiary)
Loreloom identifier: Pending allocation by Glen

## Status qualifications

Accepted means this record describes behaviour that is implemented in the tree, not a proposal.

This is the second of ADR-0156's six stages. ADR-0156 is the design document and remains the authority on the whole Colony Protocol; this record covers only Stage 2, which ADR-0156 scopes as "Add the replicated epoch, the rules for incrementing it, and the refusal of stale-epoch nodes at admission and on every RPC".

**The "on every RPC" requirement is only partly implemented, and this record says exactly which part.** ADR-0156's own style is to be explicit about its limits, and pretending to a completeness this stage does not have would be a lie in a document whose subject is the detection of lies. The precise statement is in "What is covered, and what is not" below.

ADR-0156's Stages 3 through 6 - sidecar-and-restore semantics, membership preconditions, divergence detection, and fencing tokens - are NOT implemented here. This record does not supersede ADR-0156, and ADR-0156's "Decisions for Glen" defaults that this Stage does not touch remain open.

The number 0182 is the next free apiary-local `docs/adr/` number after ADR-0181. Loreloom allocation is Glen's to do; `docs/adr/MIGRATION.md:31-37` requires central allocation for every shared identifier, so nothing here is padded or offset.

## Context

### A Colony ID cannot distinguish two generations of the same Colony

ADR-0156 records this as its second central finding, and the finding still holds. ADR-0157 (Stage 1) gave every Colony a stable, replicated, never-rewritten identity. That identity answers "which Colony is this?" but not "which generation of this Colony is this?".

A Colony that is wiped, reset and rebuilt has the same identity question and a different answer. A node that comes back from a partition with state from before the rebuild, or from a restored backup taken before the rebuild, holds an identity that matches and a state that does not. Stage 1's identity check cannot see that difference, because Stage 1's check compares an identity that is deliberately invariant across exactly the case that needs detecting.

ADR-0156 names the answer: a second, monotonic value, the **epoch**, replicated alongside the Colony ID. ADR-0156's own words on why HashiCorp Raft's term cannot serve this purpose are worth repeating here, because the temptation to reuse it is the obvious wrong turn: Raft's term is per-cluster, not per-Colony, and counts elections inside one Raft state machine. Two independently bootstrapped clusters both start at term 1 and both increment from there. Their terms are unrelated numbers that only look comparable, and treating them as a shared sequence is precisely the comparison that would falsely conclude "same epoch, no conflict" in the split-leader case.

### The epoch is a value, and a value held in one process's memory is a value two Combs can disagree about

This is the same reasoning that put `ColonyJoinWindow` (ADR-0147) and `ColonyDiskSize` (ADR-0148) in replicated state, and the same reasoning `internal/raft/colonydisksize.go` opens with at length. Four Combs each holding an epoch of 1 are four independent numbers that happen to agree at one moment; the moment one is restarted from an older snapshot, or joins from a config saying 3, the Colony has two epochs and nothing says so. The epoch is therefore in the FSM, committed through the log, and every voter derives it from the same bytes.

### The decision that matters lives at the FSM boundary, not in a handler

Every existing replicated-field precedent in this codebase refuses at the apply function rather than in a handler, and the reasoning is the same in each case: a handler check is advisory. It runs on one process, before the command enters the log, and protects only the path that went through it. The same command can arrive from a peer-forwarded write, from a raftd invoked directly, or from a future client that was not in the handler's mind when the check was written. The apply function is the only place that runs, in the same order, on every replica: it sees the committed log AND the state it is about to change.

That is the whole reason a stale-epoch node is refused "at admission" in a way that is worth anything. The admission decision - "may this node be admitted into this Colony at this epoch?" - is decided identically on every replica from the same log entry.

## Decision

### The epoch is a replicated FSM record, initialised to 1

The FSM carries a single `ColonyEpoch` record (`api/internalpb/state.proto`), written by exactly two commands:

- `SetColonyEpoch` establishes the Colony's epoch. It is applied at most once per Colony, exactly as `SetColonyIdentity` is, and the first one wins.
- `AdvanceColonyEpoch` moves the Colony to the next epoch. It is the ONLY way the epoch ever changes after it is set.

The epoch is initialised to **1**. This is ADR-0156's Decision 3, implemented rather than merely recorded: there is nothing in the existing state from which a generation count could be recovered, so 1 is the only consistent starting value for an existing Colony. It is not derived from a hash of the state, because a hash is stable across nodes but is not monotonic in any meaningful sense, and would not order a genuine reset against an ordinary restore.

Zero is not a valid epoch. There is exactly one way to express "no epoch recorded" in this design, and it is the absence of any `SetColonyEpoch` entry in the log - the same shape `ColonyDiskSize` and `ColonyIdentity` use. An epoch of 0 would be a second way to say the same thing, and the two could disagree.

The record is inside `FSMSnapshotState`, so it is inside ADR-0143's state digest with no second list to forget - the digest is derived from the message descriptor rather than from a hand-maintained list of sections, so a new state field cannot silently escape it. A raft snapshot restore, or a seeded recovery via `raftd -restore`, therefore cannot drop the epoch.

**ADR-0188 (Stage 3) later changed what a SEEDED recovery does with it.** This record originally recorded that `raftd -restore` carries the epoch through, and the reasoning was that a dropped epoch would leave a node belonging to no generation at all. Stage 3 superseded that: a restore is a COLONY RE-CREATION, not a member rejoin, so the restored node gets a NEW Colony identity and **epoch 1** whatever the payload carried. Carrying the epoch forward would have the restored node present itself to the original members as a member of the same generation - the same identity, a different state, and the split-leader hazard the Colony Protocol exists to prevent. The statement above still holds for a raft SNAPSHOT restore, which is not a re-creation.

### The increment rules are strict, and they live in the apply function

`AdvanceColonyEpoch` names the epoch it is moving TO, and the apply function enforces three rules:

1. **The Colony must already have an epoch.** An advance on a Colony with no epoch is refused, because "advance from nothing" has no defined answer and would let the first advance establish an arbitrary starting point.
2. **The new epoch must be exactly the current epoch plus one.** Not greater - exactly one greater. A jump of two or more is refused, and so is a repeat of the current epoch. The reason is that a gap makes the log lie: an epoch of 7 with no record of 2 through 6 means some of those transitions happened off the log, and a node that is later found holding epoch 5 could not be told from one holding 4.
3. **The epoch never decreases.** There is no command that lowers it, and no command that sets it to an arbitrary value after the first one. The epoch is a monotonic counter, and the only operation on it is "advance by one".

A repeated advance is refused by name, naming both the current epoch and the requested one, so that a caller which lost a race learns that it lost rather than believing it succeeded.

### The epoch is served from replicated state, never from configuration

`internalpb.StatusResponse.colony_epoch` and `rpcpb.StatusResponse.raft_colony_epoch` are served from the replicated record, for exactly the reason ADR-0157 gives for the identity: a node answering from its own config file would be answering a different question - "what did this process believe when it started" - which is exactly the question that a node restored from an older snapshot would answer differently from the Colony it is trying to rejoin.

The zero value is a REAL state, not an error. It means this node has applied no `SetColonyEpoch`: either it predates this record, or it was restored from a snapshot that predates it. It is deliberately not reported as an error, because a node that has genuinely not been told yet can still answer every other question on the response.

### The epoch travels in existing messages, as explicit named fields

ADR-0156's Decision 4 is implemented as written, and the alternative it rejects - a generic envelope carrying a colony header - is not built. The epoch is added as a named field to:

- `internalpb.StatusResponse` (raftd's internal status surface) - `colony_epoch`.
- `rpcpb.StatusResponse` (managerd's external status surface) - `raft_colony_epoch`.
- `rpcpb.RequestJoinColonyRequest` (the join request) - `colony_epoch`.
- `internalpb.PendingJoinRequest` (the replicated pending join record) - `colony_epoch`.
- `rpcpb.PendingJoinRequest` (its external rendering) - `colony_epoch`.
- `rpcpb.ApproveJoinRequestRequest` (the join approval) - `colony_epoch`.
- `rpcpb.GetJoinRequestStatusRequest` (the join status poll) - `colony_epoch`.
- `rpcpb.RejectJoinRequestRequest` (the join rejection) - `colony_epoch`.
- `rpcpb.CancelJoinRequestRequest` (the join withdrawal) - `colony_epoch`.

There is no new envelope type. The internal raftd RPC surface (`api/internalpb/raftd.proto`) is NOT changed to carry the epoch on every request, and the reason is stated in "What is covered, and what is not" below.

**The epoch means two different things on two different sets of messages, and the difference matters.** On the join request it is the JOINER's CLAIM about the Colony's generation - the value the admission decision compares against the Colony's own epoch. On the four inter-node RPCs above it is the CALLING PEER's own epoch - the value the receiving member compares against its own, to decide whether the peer is behind. The join request is the one message in the set whose author is not a Colony member, so on that one the field is a claim to be checked rather than a fact to be compared. `apiaryctl join-introduce` fills it in from the joining Comb's OWN raftd - a Comb that has been wiped and still holds old state states its old generation, which is exactly the stale-epoch case the admission refusal exists to catch, and a brand-new Comb states nothing at all, which is accepted for compatibility.

There is no operator-facing flow that sets the joiner's claim by hand. That is deliberate: the claim is a fact about the joiner's own state, and a hand-typed claim would be a claim about nothing.

### A stale-epoch node is refused at admission, in the FSM

The joining node states the epoch it believes the Colony is at, in the join request. The member recording the request compares it against its own replicated epoch, and the comparison lives in the FSM's `applyCreatePendingJoinRequest`, not in any handler.

The rules are:

- **A join request naming an epoch BELOW the Colony's current epoch is refused.** This is the stale-epoch case ADR-0156's failure matrix names: "Node rejoins with a stale epoch - Epoch comparison at admission and on every RPC - Refused: admission of the stale node".
- **A join request naming an epoch ABOVE the Colony's current epoch is refused.** A node claiming a generation this Colony has not reached is not joining this Colony's generation, and admitting it would put two generations into one log. This is the same "a node that wants a different identity is forming a new one, not joining this one" posture Stage 1 took for the Colony ID.
- **A join request naming NO epoch is accepted, exactly as Stage 1 accepted an absent Colony claim.** Every node built before this record sends no epoch, and refusing those would break every existing join for no safety gain: a member that makes no claim inherits the Colony's epoch from the Colony rather than asserting one.

An absent claim is accepted, but it is NOT recorded as epoch zero, and it is NOT treated as a claim of the current epoch. The recorded request carries whatever the joiner said, and an absent claim is recorded as absent - so the audit trail distinguishes "claimed epoch 1" from "claimed nothing" from "claimed epoch 9".

### A stale-epoch peer is refused on the peer-forwarding path

The managerd peer-forwarding path is where one Colony member calls another. The caller states its own replicated epoch, and the callee compares it against its own.

The comparison lives in a single helper, `Server.checkPeerEpoch`, called at the entry of the peer-forwarding RPCs that this stage covers. The rules are:

- **A peer stating an epoch BELOW this Colony's epoch is refused.** This is the stale-epoch peer case: it is a member of an older generation of this Colony and must not act on this one.
- **A peer stating NO epoch is accepted.** Every peer built before this record sends no epoch, and refusing those would break every existing deployment's inter-node traffic for no safety gain.
- **A peer stating an epoch ABOVE this Colony's epoch is refused**, and the refusal says so explicitly, because that case means the callee is the stale one and the operator needs to know which side to fix.

The refusal is by name, and it names both epochs, so that an operator reading it knows which side is behind.

### What is covered, and what is not

ADR-0156's Stage 2 says "the refusal of stale-epoch nodes at admission and on every RPC". This stage implements the admission refusal fully, and the RPC refusal on the paths that carry the epoch. It does NOT implement the epoch on every RPC in the service, and the limits are these:

- **The internal raftd RPC surface (`api/internalpb/raftd.proto`) does not carry the epoch on its requests.** Its `StatusResponse` DOES carry the epoch, because that response is what a managerd reads to learn the epoch of the raftd it is talking to. But the request messages do not carry it, and the raftd server does not compare an epoch on them. This is a deliberate limit, stated here rather than glossed: raftd's own RPC surface is a unix-socket, root-token-authenticated, same-host surface between managerd and its own raftd. A stale-epoch node presents a different risk there - it would be a stale managerd talking to a current raftd on the same host - and the identity check Stage 1 added to the join payload does not cover it either. Closing that gap properly means putting the epoch on raftd's request messages and comparing it in raftd's own server, which is a real change to a 25-RPC surface and is NOT done here.
- **The managerd peer-forwarding path is covered on the JOIN family's four RPCs, and on nothing else.** Every RPC that a peer managerd calls on another peer managerd is an inter-node RPC in the sense ADR-0156 means, and there are roughly seventy of them. This stage adds the epoch check to the four that are the JOIN family - `ApproveJoinRequest`, `RejectJoinRequest`, `GetJoinRequestStatus` and `CancelJoinRequest` - and to the join path's own `RequestJoinColony`. Those are the ones that matter for the epoch's stated purpose, because they are the ones on which one member acts on another member's call about the Colony's own membership, and because the join path is where a stale-epoch node would try to come back in. The remaining peer RPCs are NOT epoch-checked. The reason is that adding a check to each one is mechanical but not free, and a partially-checked surface is a surface whose coverage an operator has to know by heart; this record names that partial coverage explicitly so it is not mistaken for the whole. It is also worth being blunt about what that leaves open: a stale-epoch member can still call this Colony's `ListVMs`, `HostStats`, `CreateVM` and the rest, and those calls will be served. What the epoch currently prevents is a stale-epoch node joining the Colony, and a stale-epoch member driving the Colony's own membership decisions. It does not yet prevent a stale-epoch member reading or writing ordinary resources.
- **The frontend's and apiaryctl's own RPCs do not carry the epoch.** A human-facing tool is not a peer, and a human is not a stale-epoch node. The one exception is the join path, where the join request DOES carry the epoch, because that is the admission decision.

**What this means, stated plainly:** a node that is behind by a whole epoch is refused admission to the Colony, and a peer that is behind is refused on the RPC paths listed above. A node that is behind is NOT refused on every RPC in the service, and this record does not claim it is. Closing the remaining gap is mechanical work on the rest of the peer surface plus the raftd internal surface; it is not blocked on any decision, and it is not in this stage.

### The epoch is established by the leader, alongside the Colony identity

The epoch is committed by the leader, and leadership is not fixed at startup, for exactly the reason Stage 1's identity has the same property. `internal/raft/node.go` therefore runs a leadership watcher that acts on every leadership acquisition: on becoming leader, the node ensures the Colony has an epoch, establishing epoch 1 if it has none.

The "already set" refusal in the apply function makes every repetition harmless: the second attempt is refused by the FSM, and the watcher reads back the epoch that WON. That read-back is what makes the watcher correct under a race - two leadership acquisitions can both reach the commit point before either has committed, and the loser must return the winner's value, not an error.

### An advance is an explicit operator action, not an automatic one

ADR-0156 says the epoch "is incremented by a replicated command, and that command is issued only in two situations: a controlled reset of the colony (ADR-0038's tiered reset, where the reset is already an explicit operator action with a confirmation phrase), and a full rebuild after the state is destroyed."

This stage provides the COMMAND and the RULES for incrementing. It does NOT wire the increment to ADR-0038's Tier 1 reset, and the reason is worth stating rather than glossed: ADR-0038's Tier 1 reset MOVES THE ENTIRE DATA DIRECTORY ASIDE and the next start self-bootstraps a fresh single-node cluster. That is a new Colony with a new Colony ID, not a new generation of an existing Colony - Stage 1's startup guard explicitly treats it that way, and a deliberate reset carries the sidecar aside with the directory, so the next start is a clean new Colony at epoch 1. Incrementing the epoch of a Colony whose state has just been destroyed would require the destroyed state to still be around to carry the increment, which is exactly what a reset does not leave behind.

So the advance command exists, is safe, and has no caller yet. The reset path that WILL want to call it is a reset that PRESERVES the Colony's identity while superseding its generation - which is a Stage 3 concern, because ADR-0156's Decision 2 makes "a restore always produces a new identity" the default, and a reset that preserves identity and increments epoch is the operator's explicit override of that default. This stage does not implement that override, and does not implement the restore semantics either.

An advance, once one is issued, is only ever issued by a node that is the leader, because only the leader can commit. The advance itself is not restricted to an operator-only path in the sense of a role check: it is a replicated command, and the managerd surface that submits it is a new RPC, `AdvanceColonyEpoch`, which is RoleAdmin-gated like every other consequential write.

`AdvanceColonyEpoch` carries NO target epoch. The only valid target is the Colony's current epoch plus one, and the FSM computes that from the committed log rather than being told a number a caller could have got wrong. A request naming a specific epoch would be a second way to express the same decision, and the two could disagree. The RPC reads the Colony's current epoch to decide what to submit, and the FSM re-checks the same thing against the committed log, so a read that raced a concurrent advance is refused there rather than silently applying a wrong step.

The RPC forwards to the leader on a leader-hint rejection, exactly as every other Apply-backed write in managerd does.

## Consequences

### What gets better

- Two generations of the same Colony become distinguishable. A node that comes back with state from before a generation change is refused admission rather than admitted and then argued with.
- The split-leader hazard's second half is closed. Stage 1 made two independently bootstrapped instances carry different Colony IDs; Stage 2 makes a node that was part of a Colony and has been superseded refuse to be re-admitted to it.
- The refusal decisions are made identically on every replica, from the same committed log entry, rather than asked of whichever process happened to receive the write.

### What gets worse

- The FSM has one more state field, and the state digest covers it, so a Colony whose epoch differs changes the digest. That is the intent: two voters disagreeing about the epoch IS a state disagreement.
- A node that has been running for a long time and has no epoch will acquire one the next time it becomes leader. A node that is never leader reports epoch zero, which is honest - it genuinely does not know.
- The peer-forwarding path has a check on some of its RPCs and not others, and the coverage is documented above rather than uniform. This is the sharpest edge of this stage, and it is why the "What is covered, and what is not" section is as long as it is.

### What this record does NOT do

- It does NOT wire the increment of the epoch to any OPERATOR ACTION. The command, its rules, and the `AdvanceColonyEpoch` RPC that submits it all exist and work; what does not exist is a flow that reaches for it. See "An advance is an explicit operator action, not an automatic one".
- It does NOT implement ADR-0156's restore semantics (Stage 3), which is what a reset-that-preserves-identity would hang off. **ADR-0188 later implemented Stage 3**, and with it the rule that a restore produces a new identity and epoch 1.
- It does NOT put the epoch on raftd's internal RPC request surface, nor on the roughly seventy peer-forwarded managerd RPCs this stage does not name. See "What is covered, and what is not".
- It does NOT implement ADR-0156's membership preconditions (Stage 4), divergence detection (Stage 5), or fencing tokens (Stage 6).
- It does NOT change ADR-0156's other "Decisions for Glen" defaults, each of which remains reversible by amending ADR-0156.
- It does NOT change the join AUTHORIZATION flow. A mismatched epoch is refused; an absent epoch is still admitted under the existing ADR-0147 rules.
