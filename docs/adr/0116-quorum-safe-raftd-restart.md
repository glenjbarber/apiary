# 0116. Quorum-safe raftd restart (design only, not implemented)

## Status

Proposed (design-only). No code in this ADR has been implemented. This
document exists so a human can implement it carefully later, with the
full RPC shape, guardrail algorithm, and UI wiring already worked out.

## Context

### Today's actual raftd restart story

Investigation of `internal/manager/services.go` confirms `apiary_raftd`
is fully excluded from the generic service-restart machinery:

```go
var apiaryServices = []struct {
    name        string
    restartable bool
}{
    {name: "apiary_raftd"},
    {name: "apiary_managerd", restartable: true},
    {name: "apiary_frontend", restartable: true},
    {name: "apiary_restshimd", restartable: true},
}
```

`restartable` defaults to `false` and is never set for `apiary_raftd`.
`RestartNodeService` (`internal/manager/server.go`) checks
`restartableService(name)` before anything else and refuses any request
naming `apiary_raftd` outright. There is no RPC, no preflight, and no
lease path that ever touches raftd today. An operator restarting raftd
must SSH into the Comb directly and run `service apiary_raftd restart`
by hand, with zero cluster awareness: no check of current voter health,
no check of whether the target is the current leader, no serialization
against another operator doing the same thing on a different Comb at
the same time. `UpdateRaftdConfig` (ADR-0102) deliberately never
auto-restarts raftd either, for the same "consensus-critical" reason,
so the exclusion is applied consistently in the two places that could
plausibly trigger it.

### The 2026-09-11 stranded-cluster incident

SHARED.md documents a real accidental two-voter stranded-cluster
incident on 2026-09-11, referenced repeatedly afterward (the
differentiated-product roadmap's item 2 cites it by name as the
motivation for "do not restart both managers within ten minutes", and
item 14, "Safe Raft membership lifecycle", lists mandatory pre-approval
quorum-impact checks as following directly from it). The exact
mechanics of that incident are not fully re-derivable from this
investigation alone, but its consequence is unambiguous and already
shapes this codebase: an uncoordinated raft-membership change put the
cluster in a state that could only be recovered by hand, and every
guardrail added since (ADR-0097's join-reachability check, ADR-0103's
restart-lease guardrail) exists specifically to prevent a repeat. A
raftd restart is a strictly sharper version of the same risk category:
restarting a raft voter process, even briefly, removes it from the
active consensus group for the duration of the restart. Restart enough
voters concurrently (or restart the current leader without warning)
and the cluster can lose quorum or thrash through unnecessary
elections - the same failure shape as 2026-09-11, reachable this time
through routine maintenance rather than a membership-change mistake.

### Existing restart-guardrail pattern (ADR-0103) - what it does and does not give us for free

`internal/manager/server.go`'s `RestartNodeService` already runs
`apiary_managerd` restarts through a cluster-wide, raft-replicated
lease (`ReserveRestartLease` / `ConfirmRestartCompleted`, backed by
`internal/raft`'s `RestartLease` / `RestartRecord` FSM state and the
`AcquireRestartLease` / `RecordRestartCompleted` Command variants). Key
properties, confirmed by reading the code directly:

- The lease is keyed by `Service` (a string), not hardcoded to
  `apiary_managerd` - `RestartCooldownFact.TargetService` and the FSM's
  `AcquireRestartLease.Service` field are already generic. Nothing in
  `internal/raft`'s FSM apply path assumes the service name.
- Only `raftStatus.GetIsLeader()` + `voterNodeIDs(raftStatus)` are read
  to build `AcquireRestartLease.VoterNodeIds` - the leader authors the
  voter snapshot and timestamp itself at Apply time, never accepting
  either from the caller, so a stale follower view or clock skew can
  never influence the decision.
- `EvaluateConcurrentManagerRestart` (`internal/guardrail`) is a pure
  function over `RestartCooldownFact` and enforces exactly two things:
  (1) only one unconfirmed lease can be outstanding for a given service
  at a time (serializes restarts across the whole cluster via raft's
  own log-apply order, not caller-side coordination), and (2) a fixed
  600-second cooldown after the most recent confirmed restart. It does
  **not** reason about voter count, quorum size, or which node
  currently holds the leadership. It answers "is it safe to restart
  *a* manager right now given recent/in-flight restarts", not "would
  restarting *this specific* node break quorum."
- The restart itself is gated behind `restartGuardrailService`, a
  single hardcoded constant (`apiary_managerd`) in
  `RestartNodeService` and `PreflightRestartNodeService` - every other
  service, including a hypothetical future `apiary_raftd` entry in
  `apiaryServices`, currently always evaluates to `guardrail.Allow`
  with no lease interaction at all. Reusing this machinery for raftd
  is not automatic; it requires deliberately routing raftd through the
  same gate `restartGuardrailService` currently reserves for managerd
  alone, or introducing a second gated-service constant.
- Confirmation of a restart's success happens from the **restarted
  node's own next process startup** (`cmd/managerd`'s
  `confirmPendingRestartOnStartup`), because the process handling the
  restart RPC cannot observe its own replacement. `apiary_raftd` is a
  different binary from `apiary_managerd`; there is no equivalent
  startup hook in whatever process embeds `internal/raft.Node` today
  for raftd (raftd's own `cmd/` entry point was not audited line by
  line in this pass, but nothing in `internal/manager/server.go`
  suggests one exists). This is a real gap: a lease-based scheme for
  raftd that reuses `ConfirmRestartCompletedLocal`'s pattern needs an
  equivalent "raftd process starting up now, please confirm the
  pending record" call added to raftd's own startup path, which is new
  work, not reuse.
- The lease has no TTL and no delete/expire mechanism by design (per
  ADR-0103's own Consequences) - it is un-removable, permanently
  replicated FSM state, cleared only by a matching confirm or an
  operator `force=true` override. This is a correct property for
  managerd (a single relatively cheap process restart) but raises the
  stakes for raftd: getting the raftd confirm-on-startup wiring wrong
  would leave a real raftd-restart lease stuck exactly as
  permanently as a managerd one, on the one service where that state
  most needs to reflect reality.

### What `internal/raft.Node` exposes today

Read `internal/raft/node.go` directly:

```go
type Status struct {
    IsLeader     bool
    LeaderID     string
    NodeID       string
    LastLogIndex uint64
    AppliedIndex uint64
    RaftState    string
    Servers      []ServerInfo
}

type ServerInfo struct {
    ID       string
    Address  string
    Suffrage string
}
```

`Status()` reads `n.raft.GetConfiguration()` for the server list
(with each `Suffrage` already stringified via `suffrageString`) and
`n.raft.State() == raft.Leader` for `IsLeader`. This gives us,
cheaply and already wired through to an RPC (`internal/raft/server.go`
converts it to `internalpb.StatusResponse`, and `Server.raft.Status`
on the manager side is the same interface `PreflightRestartNodeService`
already calls): the full **configured** voter/non-voter list, and
whether the local node is currently leader.

What it does **not** expose, confirmed by reading the whole file (no
other exported method touches per-peer health):

- No wrapping of hashicorp/raft's `LeadershipTransfer()` /
  `LeadershipTransferToServer()` anywhere in this package or
  `internal/manager`. A grep across the repo found no reference to
  either symbol. Leadership transfer is a real hashicorp/raft
  capability but this codebase has never called it.
- No per-voter reachability or last-contact information. `Status()`
  reports the raft library's own view of the *configured* cluster
  (who is a voter, per `GetConfiguration()`), not which of those
  voters are currently reachable. Hashicorp/raft's `Stats()` exposes
  aggregate counters (e.g. `num_peers`) and a leader's own
  `LastContact()` is meaningful only for its immediate followers, not
  a general all-to-all health matrix. This project's own
  join-reachability guardrail (`EvaluateJoinReachability`,
  ADR-0097/ADR-0103) establishes the pattern for how this codebase
  answers "is this node currently reachable" when raft's own state
  does not say: it dials the address directly (a real TCP/gRPC probe
  performed by the current leader, since only the leader's own network
  vantage point matters for the join decision) and records the result
  as an `invariant.Evidence`-backed fact, never inferred from raft
  state alone.

This matters directly for the requested guardrail: "refuse to restart a
raftd voter if doing so would drop the cluster below quorum (e.g.
refuse on a 3-voter cluster if any other voter is already
known-unreachable)" requires knowing which *other* voters are
currently unreachable, and that information does not exist anywhere in
`internal/raft.Node.Status()` today. It would have to be gathered the
same way ADR-0097 gathers join reachability: a live dial from the
current leader (or the requesting node acting as prober) to every
other configured voter's raft or peer-managerd address, at request
time, with a bounded timeout, and the result fed into a new pure
`guardrail.EvaluateXxx` fact struct - not read off existing raft
status fields.

## Decision

**Design only. Do not implement in this pass.**

Reasoning: the honest scope of a *correct* quorum-safe raftd restart
is larger than the phrase "reuse the existing lease" suggests, once
the three gaps above are accounted for:

1. The existing lease/cooldown machinery (`RestartLease`/
   `RestartRecord`, `AcquireRestartLease`) is reusable in shape (it is
   already keyed by service name and gives free serialization across
   the whole cluster), but its confirm-on-restart-startup half is
   managerd-specific by construction - there is no analogous "the
   raftd process just started, please clear the pending lease record"
   hook today, and building one wrong leaves a permanently-stuck,
   un-removable raft-replicated lease with no delete path, on the
   single most consensus-critical service in the project. Getting this
   right needs to be done inside whichever process currently starts
   `internal/raft.Node` for the raftd binary, which this investigation
   did not fully trace end-to-end (the process boundary between
   `apiary_raftd` the rc.d service and whatever Go entry point embeds
   `internal/raft.Node` needs to be confirmed against the real
   `cmd/raftd` source before any startup-confirm hook can be written
   safely).
2. The single most important guardrail requested - refuse if
   restarting this voter would drop the cluster below quorum given
   other voters' *actual current reachability* - cannot be answered
   from any state `internal/raft.Node` exposes today. It requires new
   live-dial logic mirroring ADR-0097's join-reachability probe,
   run against every other configured voter, with its own timeout and
   partial-failure handling (what verdict do you return if the probe
   to one peer times out rather than cleanly refusing?). This is new,
   security/safety-relevant networking code on the exact code path
   this ADR exists to make safer, not a call to something proven.
3. Leadership handling has two honest options and neither is free:
   automatic `LeadershipTransfer()` has literally never been called
   anywhere in this codebase before, so wiring it in for the first time
   on the consensus-critical restart path (rather than on a lower-stakes
   feature first) is exactly the kind of "looks risky to get right in
   the time available" case the task's own instructions call out as a
   reason to prefer the acknowledgment-only path; the
   acknowledgment-required alternative is safe and cheap to build
   (Section "Chosen leader-handling approach" below), but by itself it
   does not fully satisfy "sequential maintenance across Combs" for the
   leader case without an operator manually knowing to restart
   followers before the leader.
4. The task's own test requirement is a *real* multi-node raft
   integration test proving the guardrail actually refuses a
   quorum-breaking restart (not a mock of raft's own quorum logic).
   Writing that test honestly means standing up a real 3-node
   `internal/raft` fixture (this repo already has the pattern -
   `internal/raft/multinode_test.go` - so this part is very doable),
   then wiring a genuinely new RPC and guardrail through it end to end
   on the first attempt, on code that if wrong in a subtle way (e.g. an
   off-by-one in the quorum arithmetic, or a race between the
   reachability probe and a voter actually going down) fails silently
   in the one place - live raft consensus - where a silent failure is
   least acceptable and hardest to detect after the fact in the field.

None of these are individually unsolvable, but together they add up to
materially more new, safety-relevant surface than "extend an existing
lease to a second service name," on the one subsystem in this codebase
where a subtly wrong implementation is worse than no implementation
(a raftd restart done today, by hand, by an operator who is presumably
already being careful, versus a raftd restart done through a new
automated guardrail that has a bug and reports Allow when it should
report Block). Per this task's own explicit escape hatch, this ADR
documents the full design below instead of forcing a merge in this
pass.

## Design

### New raft-replicated state: reuse `RestartLease`/`RestartRecord`, do not add new FSM state

`AcquireRestartLease.Service` should be set to `"apiary_raftd"` for a
raftd restart request. No new Command variant or FSM message is
needed for the lease/cooldown half - `internal/raft`'s existing
`applyAcquireRestartLease`/`applyRecordRestartCompleted` already treat
`Service` as an opaque key. This gives raftd restarts the same free
properties managerd restarts already have: only one outstanding
lease per service cluster-wide, and a cooldown window after the most
recent confirmed restart, both enforced by raft's own log-apply order
rather than caller-side coordination.

### New guardrail fact and pure evaluator: `guardrail.RaftdQuorumFact` / `EvaluateRaftdQuorumSafety`

Add to `internal/guardrail` (mirroring `RestartCooldownFact` /
`EvaluateConcurrentManagerRestart`'s existing shape exactly, so the
convention stays uniform):

```go
// VoterReachability is one other configured voter's current
// reachability, as probed live by the caller - never inferred from
// raft's own GetConfiguration(), which only reports the CONFIGURED
// membership, not which members are actually up right now.
type VoterReachability struct {
    NodeID      string
    Reachable   bool
    ProbeError  string
}

// RaftdQuorumFact is the input to EvaluateRaftdQuorumSafety. Every
// field is already raft-read-derived and live-probed by the caller
// (PreflightRestartRaftd / the real enforcement point) before this
// function ever sees it - this function makes no RPC, dial, or raft
// call of its own, matching every other guardrail in this package.
type RaftdQuorumFact struct {
    TargetNodeID   string
    IsTargetVoter  bool
    IsTargetLeader bool
    OtherVoters    []VoterReachability
    ProbeReadOK    bool // false if the reachability probe itself failed to run at all
}
```

```go
// EvaluateRaftdQuorumSafety is the single source of truth for whether
// restarting apiary_raftd on TargetNodeID is safe right now, called by
// both the real enforcement point and its preview RPC so the two can
// never drift (mirrors EvaluateConcurrentManagerRestart's own
// justification for this split).
//
// A non-voter target always returns Allow: restarting a non-voting
// raftd carries no quorum risk at all, the same reasoning
// EvaluateConcurrentManagerRestart applies to a non-voter manager
// restart.
//
// !ProbeReadOK returns Unknown, never Allow - a guardrail that could
// not determine other voters' real reachability must fail closed.
//
// The quorum check itself: total configured voters N (= 1 target +
// len(OtherVoters)), quorum = N/2 + 1 (integer majority, matching
// hashicorp/raft's own quorum definition). Voters currently counted as
// "up" = OtherVoters where Reachable is true (the target itself is
// about to go down, so it never counts). If up-count < quorum - 1
// (quorum minus the target's own now-planned absence, i.e. the
// remaining cluster after this restart would not itself reach
// majority) -> Block. This is deliberately conservative: it blocks
// even a restart that a live raft election might survive, because a
// probe-based reachability check is inherently a moment-in-time
// snapshot, not a guarantee, and this is exactly the code path where
// erring toward Block is correct (mirrors EvaluateConcurrentManagerRestart's
// stated Unknown-is-Block philosophy).
func EvaluateRaftdQuorumSafety(fact RaftdQuorumFact) Report { ... }
```

Concretely, on a 3-voter cluster (quorum = 2): restarting any one voter
leaves 2 up automatically if the other two are both reachable (2 >= 2,
Allow). If either of the other two is already unreachable, restarting
this one would leave only 1 up (1 < 2, Block) - this is exactly the
task's own worked example ("refuse on a 3-voter cluster if any other
voter is already known-unreachable"). On a 5-voter cluster (quorum =
3), one other voter being down still leaves 3 up after this restart
(Allow); two others being down blocks it. This generalizes correctly
to any voter count without special-casing 3.

### Reachability probing

New unexported helper in `internal/manager` (not `internal/guardrail`,
which stays pure/no-I/O by convention), e.g. `probeRaftdVoters(ctx,
otherVoters []raft.ServerInfo) []guardrail.VoterReachability`, modeled
directly on how `ApproveJoinRequest`/`PreflightApproveJoinRequest`
already dial a claimed `raft_bind_address` for ADR-0097's reachability
gate: a short-timeout TCP dial (or reuse of the peer-managerd gRPC
client already wired for `ReserveRestartLease` forwarding, whichever
this codebase's existing peer-dial helper supports more directly - the
existing `s.peers`/`peerManagerdAddr` machinery in
`internal/manager/server.go` is the natural place to check first
before adding a second dial path). Only the current raft leader should
run this probe, for the same reason `ReserveRestartLease` only
evaluates on the leader: a follower's own network vantage point could
give a different, wrong answer (ADR-0097's own stated reasoning,
directly applicable here). A restart request arriving at a non-leader
node forwards to the leader first, exactly as `reserveRestartLease`
already does via `currentLeaderRaftAddress`/`s.peers.ReserveRestartLease`.

### RPC shape

New RPCs, `PreflightRestartRaftd` and reuse of the existing
`RestartNodeService`/`ReserveRestartLease`/`ConfirmRestartCompleted`
trio with `Service`/`Name` = `"apiary_raftd"`:

- `apiaryServices` in `internal/manager/services.go` gains
  `{name: "apiary_raftd", restartable: true}` - but `restartableService`
  alone is not sufficient gating; `RestartNodeService` needs a second
  named constant alongside `restartGuardrailService`
  (e.g. `restartGuardrailServices = map[string]bool{"apiary_managerd":
  true, "apiary_raftd": true}`) so both services route through the
  lease path, and `PreflightRestartNodeService` needs the same
  generalization (today it special-cases exactly one string).
- `PreflightRestartNodeService`'s existing response shape
  (`Verdict`/`Findings`) is reused unchanged for raftd - no new proto
  message needed there. Internally, when `name == "apiary_raftd"`, the
  handler additionally runs the new quorum probe and merges
  `EvaluateRaftdQuorumSafety`'s findings into the same
  `GuardrailFinding` list the cooldown check already populates, both
  keyed by distinct `Rule` strings (`"concurrent-manager-restart"` stays
  as is; add `"raftd-quorum-safety"` and, for the leader case,
  `"raftd-leader-restart"`) so the frontend can distinguish which
  guardrail fired without string-matching `Detail`.
- `RestartNodeServiceRequest.Force` (already exists) is reused for the
  quorum guardrail's override exactly as it already is for the
  cooldown guardrail - one checkbox, one semantic ("I acknowledge this
  restart may affect cluster availability"), covering both reasons a
  raftd restart could be blocked. This avoids adding a second,
  differently-worded force flag for what is, from the operator's
  perspective, the same kind of acknowledgment.
- Raftd's own startup path needs a new, small confirm-on-startup hook
  analogous to `cmd/managerd`'s `confirmPendingRestartOnStartup`,
  calling `ConfirmRestartCompletedLocal(ctx, "apiary_raftd", nodeID,
  leaseID)` once the raft node has rejoined and is healthy (defined,
  minimally, as: `Node.Status()` returns without error and the node's
  own ID appears in `Servers`). This is the piece flagged above as
  needing the process-boundary confirmation this investigation did not
  complete - whoever implements this must first confirm exactly which
  binary/process embeds `internal/raft.Node` for raftd and where its
  startup sequencing lives.

### Chosen leader-handling approach: acknowledgment required, not automatic leadership transfer

Per the task's own stated preference for judgment based on
effort/risk: this design specifies the acknowledgment-required
approach (option a), not automatic `LeadershipTransfer()` (option b),
for the reasons in the Decision section above (never called anywhere
in this codebase; first use should not be on the consensus-critical
restart path). Concretely: `EvaluateRaftdQuorumSafety` (or a sibling
check folded into the same preflight handler) additionally flags
`IsTargetLeader == true` as a `Block`-by-default finding
(`Rule: "raftd-leader-restart"`, `Detail: "<node> is the current raft
leader - restarting it will trigger a leader election"`), overridable
by the same `Force` flag as the quorum check. This is a legitimate,
well-precedented v1: it is the exact shape ADR-0103 already uses for
managerd's own cooldown/lease Block-by-default-with-force pattern, so
an operator or reviewer already familiar with the managerd restart
control sees a consistent interaction, not a new pattern to learn. A
future ADR can add automatic leadership transfer as a strict
enhancement (transfer first via `LeadershipTransferToServer()`, poll
until `IsLeader` is false, then allow the restart) without changing
this design's RPC shape or FSM state at all - it would only change
what `RestartNodeService` does before calling `s.services.Restart`
when the leader-restart finding fires without `Force`.

### Frontend wiring

Reuse the Machine page's existing node-services panel
(`internal/frontend/machine.go`, `renderNodeServicesPanel`/
`handleRestartNodeService`, `templates` for the "force-acknowledgment
checkbox instead of the plain restart button" pattern already covered
by `machine_test.go`'s `TestMachineNodeServicesPanel...` cases around
line 130-203). No new page or panel: `apiary_raftd`'s row in that same
table gains `Restartable: true` (from the `apiaryServices` change
above) and therefore already flows through the exact same
"blocked -> show finding details + force checkbox -> submit with
`force=1` -> `RestartNodeServiceRequest.Force = true`" path
`handleRestartNodeService` already implements verbatim, with zero new
frontend code beyond whatever template change is needed to render the
new `"raftd-quorum-safety"`/`"raftd-leader-restart"` finding rules with
raftd-appropriate copy (the existing template presumably already
renders `Findings[].Detail` generically - to be confirmed against the
real template file by whoever implements this, since this pass did not
locate and read the exact `.tmpl`). Admin-only gating and the general
confirmation posture are inherited for free from whatever
existing role check already wraps `handleRestartNodeService` (the
route is undifferentiated by service name today, so raftd requires no
new authorization code, only the request-shape/label change above).

### Sequential maintenance across Combs

Explicitly out of scope, as the task allows: no new orchestrator.
What the guardrail above gives an operator doing sequential-by-hand
maintenance is: (1) the lease/cooldown reuse means a second Comb's
raftd restart attempt while the first is still unconfirmed is
automatically blocked cluster-wide, forcing genuine sequencing even if
two operators act at the same time; (2) the quorum-reachability check
means an operator cannot restart a second voter before the first has
actually rejoined (the first restart's own unreachability, or its
still-open unconfirmed lease, both keep the second attempt blocked);
and (3) the leader-restart acknowledgment nudges an operator toward
restarting followers before the leader, which is the correct order for
minimizing election churn, without the software enforcing an order.
Together this is sufficient for safe hand-driven rolling maintenance;
a fully automated rolling-restart controller remains a distinct,
larger feature.

## Consequences

- No code changes ship in this pass. `apiary_raftd` remains
  unrestartable through Apiary; operators continue restarting it by
  hand with no cluster awareness, exactly as today, until this design
  is implemented.
- The design deliberately reuses `RestartLease`/`RestartRecord` FSM
  state and the `RestartNodeServiceRequest.Force` field rather than
  adding new raft-replicated message types or a second force flag,
  keeping the eventual implementation's raft-schema footprint small.
- The design introduces exactly one genuinely new piece of
  network-facing logic (the other-voter reachability probe) and
  documents exactly where it should live and what it should model
  itself on (ADR-0097's join-reachability dial), so an implementer is
  not starting from a blank page.
- Automatic leadership transfer is explicitly deferred, not rejected -
  documented above as a strict future enhancement with no RPC/FSM
  changes required to add later.
- Whoever implements this must first (a) confirm the exact process
  boundary that starts `internal/raft.Node` for `apiary_raftd` and
  wire a startup confirm hook there, and (b) confirm whether
  `internal/manager`'s existing peer-dial machinery
  (`s.peers`/`peerManagerdAddr`) can be reused directly for the
  reachability probe or whether a second, raft-address-specific dial
  path is needed - both are called out above as open items this
  investigation identified but did not resolve.
- A fully automated multi-node rolling-restart orchestrator remains
  explicitly out of scope, per the task's own instructions and Section
  "Sequential maintenance across Combs" above.
