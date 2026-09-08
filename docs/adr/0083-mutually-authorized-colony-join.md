# ADR-0083: Mutually-authorized Colony join

## Status

Accepted

## Context

`raftd -join <socket>` only ever dials its argument as a local Unix
domain socket (`cmd/raftd`'s own `joinCluster`, confirmed by reading the
full call chain - `AddVoter` has exactly one caller in the repo). A
genuine cross-host join today requires a manual, one-shot SSH
local-forward tunnel just to satisfy that UDS requirement
(`docs/bootstrap.md`'s own Step 7 Path B) - real and working, but
awkward, and the opposite of "bring up a machine and have it just
work."

The user's proposal: every fresh Apiary installation already bootstraps
as its own independent single-voter cluster by default (`raftd`'s
existing behavior when `-join` is omitted, unchanged by this ADR).
Joining an existing Colony becomes a *post-bootstrap, UI-driven* action
instead, modeled on how Apple pairs a new device to an Account: the new
side shows a request and a short human-readable code; the existing,
already-trusted side must explicitly approve it, the code giving
mutual, human-visible correlation so an Admin isn't approving a request
they can't actually verify came from the Comb they think it did.

Three research passes across the raft membership code, the
peer-forwarding/RBAC infrastructure, and this codebase's persistence
conventions confirmed: `AddVoter`/`RemoveServer` have zero per-caller
authorization today beyond an optional flat shared `-internal-token`
(one secret for the whole cluster, no per-node identity); no
pairing/approval/OTP pattern exists anywhere in this codebase to
extend - this is genuinely new ground.

## Decision

### The flow

1. Joining Comb's operator, on the Machine page's new "Join a Colony"
   section, enters this Comb's own `node_id`/`raft_bind_address` and
   submits.
2. `managerd` calls a new RPC, `RequestJoinColony`, on the target - the
   address of one existing Colony member the operator names.
   **Deliberately unauthenticated** (mirrors `Status`'s own narrow,
   explicitly-named exemption from the auth interceptor, for a
   different, distinct reason stated in code: a brand-new Comb has no
   Colony API key yet by definition).
3. The receiving `managerd` raft-`Apply`s a new `PendingJoinRequest`
   record - **raft-replicated, not node-local**, so any Admin on any
   current Colony member sees the same request, not just whoever
   happens to be logged into the Comb that received the RPC first.
   Returns `{request_id, code}` - `code` is a 6-digit `crypto/rand`
   value, plaintext, not a secret: its job is human visual correlation.
4. Joining Comb's UI shows "Waiting - your code: 482913" and polls
   `GetJoinRequestStatus(request_id)` - also unauthenticated, scoped to
   exactly one caller-supplied id.
5. The Colony side's landing page gains an Admin-only "Pending join
   requests" panel (raft-replicated state, so the same list regardless
   of which member's UI it's viewed from). The Admin compares the code
   shown there against the one on the joining Comb's own screen, then
   clicks Approve.
6. `ApproveJoinRequest` - Admin-gated, leader-hint-forwarded like every
   other write RPC - calls a new `RaftClient.AddVoter` wrapper against
   this `managerd`'s own local `raftd` (the underlying `Node.AddVoter`/
   `RaftInternal.AddVoter` already existed, just never had a caller on
   the managerd side), then marks the request Approved. If `AddVoter`
   itself fails, the request is left Pending, not marked Approved, so
   a membership change that didn't actually happen is never recorded as
   if it had.
7. `RejectJoinRequest` is the symmetric decline action. Every request
   also carries an `expires_at` (~15 min), checked lazily at read/apply
   time - this codebase's one existing precedent (the Assumption
   Register's `ExpiresAt`) works the same way, and nothing here runs a
   background sweep goroutine either.

### New raft-replicated state

`api/internalpb/state.proto`: `PendingJoinRequest{request_id, node_id,
raft_bind_address, code, requested_at_unix, expires_at_unix, status}`,
plus `Command` variants `CreatePendingJoinRequest`/
`ApprovePendingJoinRequest`/`RejectPendingJoinRequest` (the same
clone-and-touch pattern every other command in `internal/raft/fsm.go`
already uses). Resolved (Approved/Rejected) and expired requests are
never deleted from FSM state, only excluded from the actionable list
(`ListPendingJoinRequests`) - so a joining Comb's own status poll can
still observe a terminal outcome via the separate, always-inclusive
`PendingJoinRequest(id)` accessor.

### New RPCs

| RPC | Auth |
|---|---|
| `RequestJoinColony` | none |
| `GetJoinRequestStatus` | none, scoped to one `request_id` |
| `ListJoinRequests` | `RoleAdmin` |
| `ApproveJoinRequest` | `RoleAdmin` |
| `RejectJoinRequest` | `RoleAdmin` |

The three Admin-gated RPCs sit at the same tier as `CreateAPIKey`/
`RevokeAPIKey`, not the Operator tier ADR-0029's peer-forwarding RPCs
use - approving a request calls `AddVoter` against this node's own raft
cluster, a materially bigger consequence than any Operator-tier write.

### A real, tested finding: approve only once the joining Comb is actually reachable

Building this surfaced a genuine operational hazard, not a hypothetical
one: a first version of the integration test targeted a bogus,
unreachable address for `AddVoter`. The config-change commit itself
succeeded immediately (`AddVoter` only needs the *current*
configuration's quorum, not the new node) - but going from one voter to
two means quorum now requires **both**, and the existing single-node
leader started failing heartbeats to the unreachable address within
~500ms and stepped down entirely, taking the whole Colony's leadership
down with it until the situation was manually resolved.

**This means approving a join before the joining Comb's `raftd` is
actually up and listening at the given `raft_bind_address` can
destabilize the existing Colony's leadership.** The UI's own copy on
the Machine page's "pending" state and the landing page's approval
panel both say so explicitly. The correct operational sequence is: the
joining Comb's `raftd` should already be reachable at the address it
submitted *before* an Admin clicks Approve, not after.

### Disclosed gap, not yet solved: `raftd` has no passive "await join" mode

The safest sequence found via `internal/raft/multinode_test.go`'s own
existing `newUnbootstrappedNode` pattern - construct a `raftnode.Node`
(so its transport is really listening) but never call `Bootstrap()`,
leaving it idle until an external `AddVoter` call arrives - exists at
the library level today, but `cmd/raftd`'s own CLI never exposes it: on
a fresh, empty `-data-dir` with no `-join`, `main()` always calls
`Bootstrap()` unconditionally, self-forming a new single-node cluster.

This is a real, necessary follow-up, not solved here: a genuinely safe
"join a *different* existing Colony" flow for a Comb that already
completed its own standalone bootstrap needs a new `raftd` mode (e.g.
`-await-join`) that skips `Bootstrap()` and simply listens, matching
`newUnbootstrappedNode`'s already-proven-safe shape, so the joining
Comb's operator can wipe its own prior raft state (`raftd -reset
yes-wipe-raft-state`, ADR-0038, unchanged) and restart into that
*passive* mode - never self-forming a second independent single-node
history that would then need reconciling with the real Colony's own log.
Tracked separately; this ADR's own flow and UI copy disclose the
prerequisite (bring `raftd` up and reachable first) without yet
providing the safest way to do so on a previously-bootstrapped Comb.

### Frontend

- **Machine page** - "Join a Colony" section: this Comb's own identity/
  membership, the same framing every other Machine page control
  already has. Admin-gated.
- **Landing page** (`cluster_overview.html`) - "Pending join requests"
  panel: inherently Colony-wide membership state, the same page that
  already aggregates every current member. Admin-gated.

## Consequences

- A fresh host's path into an existing Colony no longer needs an SSH
  tunnel to satisfy a Unix-socket-only RPC - the only cross-host
  requirement left is that `raft_bind_address` be reachable, already a
  pre-existing requirement.
- The riskiest moment in this flow (approving before the joiner is
  reachable) is disclosed in the UI copy itself, not hidden - but is not
  yet mechanically prevented; that's the `raftd -await-join` follow-up's
  job.
- `code` is a human-correlation aid, not a cryptographic secret - an
  attacker with network access to file a `RequestJoinColony` call could
  see it too. The real trust boundary stays Admin RBAC approving, the
  same trust model every other Admin action already rests on.
