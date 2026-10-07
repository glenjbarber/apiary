# ADR-0150: Preferred-Leader Bias

## Status

Accepted

## Context

Issue #20 asks that "when a Colony has a healthy outlook, designate a
configurable Comb to be the leader, helpful for tasks where the leader
needs to be known." Colony and Comb are this codebase's own terms, not
new vocabulary: `internal/raft`'s own comments already use "Colony" for
the raft cluster as a whole (e.g. `colonyjoinwindow.go`'s "this
Colony's configured ceiling") and "Comb" for one member process (e.g.
`cmd/raftd/main.go`'s per-process daemon). "Healthy outlook" maps onto
ADR-0122's cluster-wide evidence-aware health verdict (`ClusterHealth`),
the one place this codebase already computes "is the Colony healthy"
from more than a single node's own view.

Raft's safety guarantees do not allow a node to simply be leader at
all times - if that node is partitioned or down, forcing it to remain
leader (or re-electing it against protocol) would mean either no
leader at all or an unsafe split-brain. A preference can only ever be
a bias applied when it is safe to apply, not an override of normal
election safety.

`hashicorp/raft` (already this codebase's raft library, v1.8.0 per
go.mod) exposes exactly the safe primitive this needs:
`LeadershipTransferToServer`, which moves leadership to a named,
caught-up voter without an election race, and which already refuses
(no-ops, returns an error) if that voter is not a known member or has
not replicated enough to safely receive leadership. `stepaside.go`
already uses this library's plain `LeadershipTransfer()` (no target)
for the "this node must restart, hand off first" case; this feature
is the same primitive, aimed at a specific, configured target instead
of an arbitrary one.

## Decision

**1. A new per-node config field**, `PreferredLeaderID`
(`raftnode.Config.PreferredLeaderID`, surfaced in `raftdconfig.Config`
as `preferred_leader_id`), naming the NodeID that should hold
leadership whenever it safely can. Empty (the default) means no
preference at all, and leaves leadership entirely to raft's own
election process - this feature changes nothing for a Colony that
does not opt in. It is set per-node like every other field in
`raftdconfig.Config` (`node_id`, `raft_bind`, ...); every node in the
Colony is expected to carry the same value, exactly as every node
already carries the same expectations about cluster shape.

**2. `Node.PreferredLeaderTransfer`**, in `internal/raft`, the only
thing in this feature that touches raft directly. Call it from any
periodic loop (the reference caller, `runPreferredLeaderLoop` in
`cmd/raftd`, ticks every 10s - independent of, and shorter than, the
cooldown below, so a transfer happens promptly once due rather than on
some coarser schedule). On every call where this node is not the
leader, or already is the preferred leader, or the preferred ID is not
a current member, it is a correct no-op: nothing is attempted, nothing
is logged as an error. When this node *is* the leader, a different
node is preferred, and the cooldown has elapsed, it calls
`LeadershipTransferToServer` - the raft library's own safe primitive -
and reports what happened. No hand-rolled election bias exists
anywhere in this feature.

**3. Thrash prevention: a one-minute cooldown**, tracked in memory per
`Node` (`PreferredLeaderCooldown`), consulted even on calls that turn
out to be no-ops. Without it, a flapping preferred node (reachable just
long enough to win an election, then unreachable again) would cause
whichever node next notices it is leader to immediately attempt another
transfer, each one its own brief write-unavailability window. A fixed
cooldown rather than exponential backoff was chosen deliberately: the
trigger here is a steady-state human preference, not a failing
operation being retried, so "try again periodically, forever" is the
right shape, not "try less and less often." The cooldown is not
persisted - a restarted `raftd` starts a fresh window, which is the
conservative direction (fewer transfers attempted sooner, never more
thrashing than intended).

**4. What this does NOT gate on, and why.** The issue's own trigger is
"when the Colony has a healthy outlook" - ADR-0122's cluster-wide
verdict. `internal/raft` is deliberately the lowest layer in this
codebase and has no visibility into `internal/health`, managerd's
peer-forwarding, or any multi-node RPC fan-out; importing any of that
here would invert the existing layering. `PreferredLeaderTransfer`'s
own "healthy enough" check is therefore necessarily narrower: raft's
own leader/voter/catch-up state, via `LeadershipTransferToServer`'s own
refusal behavior, which is the one health signal this layer
legitimately owns. Gating the *initiation* of a transfer attempt on
the full Colony-wide `ClusterHealth` verdict - so a degraded Colony
never even tries - is left to a higher-layer caller (managerd, which
already computes that verdict) in a follow-up; `raftd` today exposes
no RPC a caller could use to ask it to transfer, only the in-process
ticker wired up in this change. This is flagged here rather than
guessed at, because it requires a new call path from managerd down
into raftd that does not exist yet, and deciding where health-gated
orchestration for a consensus-critical daemon should live is a design
question, not an implementation detail.

**5. Visibility.** `Node.Status()` now also reports
`PreferredLeaderID` alongside the existing `LeaderID`, so "who is
leading" and "who is supposed to be leading" are visible together on
one snapshot. This change stops at the Go-level `Status()` struct; it
does not extend `internalpb.StatusResponse` or any REST route, since
regenerating this codebase's protobuf bindings is its own exercise
disjoint from this feature's raft-safety concerns, and is left as a
follow-up rather than risking a mismatched generated file here.

## Consequences

- A Colony that sets `preferred_leader_id` on every node gets biased,
  not forced, leadership: the preferred Comb leads whenever it is
  genuinely safe (reachable, caught up, a current voter), and normal
  Raft election still decides everything else, including every
  failure case this feature does not touch.
- No new RPC, no new wire format change; the only externally visible
  addition is a JSON config field and a new field on the in-process
  `Status()` snapshot.
- The cluster-health-gated version of this (asking managerd's
  `ClusterHealth` before ever attempting a transfer) is explicitly
  left open; see Decision 4.
