# ADR-0129: Colony-wide PF Firewall Management

## Status

Proposed

## Context

[ADR-0127](0127-sylve-io-features.md) section 4 ("PF Firewall
Management", High Priority) puts host firewall management in Phase 1
and states the problem as "No host firewall management. Cannot enforce
Cell network isolation or blast-radius boundaries." Phase 1 is
jointly "Guest Migration, PF Firewall".

That framing is accurate about the *host*, and misleading about the
guest. Apiary has managed per-VM `pf(8)` anchors since
[ADR-0022](0022-network-management.md), extended by
[ADR-0075](0075-firewall-rule-priority.md) (rule `priority` and pf's
last-match-wins evaluation), and paused/resumed by ADR-0049
(`SetVMFirewallPaused`). `internal/pf` exists, is 339 lines including
tests, and is exercised every reconcile tick. What does not exist is
anything that is *cluster-wide*, *staged*, or *safe*.

Three facts drive this design.

First, the existing per-VM renderer is structurally incapable of
expressing a host policy. `internal/pf/renderRule` emits a fixed
`from any to any` and has no field for an interface, a source, or a
destination. A rule that restricts *which host* may reach a Cell is
not expressible; a rule that restricts *who the Cell may talk to* is
not expressible either. This is a capability gap, not a missing
feature flag.

Second, and more seriously: the reservation that makes per-VM anchors
work is `anchor "apiary/*" all` in `/etc/pf.conf`
(`internal/pf/exec.go`'s package doc). If that anchor sits where the
host's own traffic is evaluated, then an anchor rule of
`block in from any to any` - which the current renderer will happily
produce from `Action: "block"`, `Direction: "in"`, `Protocol: ""` -
matches the operator's own SSH session as readily as it matches a
guest. The per-VM firewall is, as written, already a host-lockout
vector. This ADR does not assert that is happening on any current host;
`/etc/pf.conf` is a host prerequisite this project deliberately does
not manage, and confirming the real layout is testbed work (see Test
plan). But the hazard is visible from the code alone, and it must be
closed before any host-scope policy is exposed.

Third, [ADR-0114](0114-remove-uplink-takedown.md) removed the uplink
down/up toggle outright rather than build a server-side guardrail for
it, on the stated ground that "a Comb should never be able to remove
its own only path to reach it." A firewall ruleset is the same
capability wearing a different hat: it can sever that path. ADR-0114
withdrew the *action*; this ADR must supply the *guardrail* that makes
an equivalent action safe to keep. It cannot do so by asking nicely -
the withdrawn feature was kept alive for years on an `hx-confirm`
dialog alone.

The failure this ADR exists to make impossible has two variants worth
naming separately, because they have different blast radii:

- **Operator lockout.** The host's management path (SSH, the frontend,
  the API) is blocked. Recovery requires physical access or a console.
  Annoying, recoverable, terrifying.
- **Replication severance.** The blocked path is a quorum voter's raft
  traffic. If the lost link is voter-to-voter and enough of them go at
  once, the Colony has no leader. There is no operator-side recovery,
  because the operator's own connection is likely down the same wire.
  The recovery mechanism *must not require the network it just broke*.

## What exists today

File by file, function by function, quoted from the tree. Nothing in
this section is proposed for replacement; the following section states
precisely why each piece is insufficient for what ADR-0127 asks.

### `internal/pf/exec.go` (42 lines)

Package doc, verbatim in substance: it "applies per-VM firewall rules
(api/internalpb's `FirewallRule`, part of `VMDefinition`) via a
dedicated pf(8) anchor per VM, driven by pfctl(8). See ADR-0022.
Requires pf already enabled on the host with an anchor point reserved
for Apiary in `/etc/pf.conf` (e.g. `anchor "apiary/*"`) - a one-time
host prerequisite this package does not set up itself, the same way
internal/hast doesn't manage the hastd service and internal/dhcpd
doesn't install dnsmasq."

Two unexported helpers, the whole of its API surface:

```go
func runCmdStdin(ctx context.Context, stdin string, name string, args ...string) (string, error)
func runCmd(ctx context.Context, name string, args ...string) (string, error)
```

`runCmdStdin` returns trimmed stdout and, on failure, an error whose
message is `"<name> <args>: <trimmed stderr>"` - or `err.Error()` when
stderr is empty. `runCmd` is `runCmdStdin(ctx, "", ...)`. There is no
dry-run parse, no read-back, no timeout of its own beyond the caller's
context, and no `exec.LookPath` pre-check.

### `internal/pf/rules.go` (90 lines)

```go
type Rule struct {
    Direction string // "in" or "out"
    Action    string // "pass" or "block"
    Protocol  string // "tcp", "udp", "icmp", or "" (any)
    PortRange string // "22", "8000-9000", or "" (any)
}

func RenderRules(rules []Rule) (string, error)
```

`renderRule` validates direction (`in`/`out`) and action
(`pass`/`block`), validates protocol against exactly
`{"", "tcp", "udp", "icmp"}`, then writes, unconditionally:

```go
b.WriteString(" from any to any")
```

A `PortRange` is rejected unless the protocol is `tcp` or `udp`.
`renderPortRange` converts `"22"` to `22` and `"8000-9000"` to
`8000:9000`, rejecting `<= 0`, inverted, or `> 65535`.

`RenderRules` is pure, has no pf tooling dependency, and is the one
genuinely well-factored function in the package. It emits no `quick`,
by design, so pf's last-match-wins ordering applies (ADR-0075).

`Rule` is a deliberate, documented duplicate of
`api/internalpb.FirewallRule` - the comment says so, and gives the
reason: keeping the package's core logic independent of the wire
schema, the same reasoning `internal/cluster`'s local interfaces
already follow.

### `internal/pf/manager.go` (70 lines)

```go
type Manager struct{}

func (m *Manager) Apply(ctx context.Context, anchor string, rules []Rule) error
func (m *Manager) Flush(ctx context.Context, anchor string) error
func (m *Manager) ApplyNAT(ctx context.Context, anchor, subnet, uplink string) error
```

- `Apply` renders and runs `pfctl -a <anchor> -f -`. Full-replace, not
  diff, explicitly "the same full-replace-not-diff convention
  internal/hast's `WriteConfig` already uses for `hast.conf`." Its doc
  comment states the empty-rules convention: "No rules means an empty
  anchor, i.e. everything allowed - matching this project's de facto
  behavior before firewall support existed." That sentence is the
  fail-open default this ADR leans on for last-resort rollback.
- `Flush` runs `pfctl -a <anchor> -F rules` and treats the string
  `"No such anchor"` as success - the idempotent-teardown posture
  `PurgeVM`/`DeleteVM` already applies.
- `ApplyNAT` runs a single `match out on <uplink> from <subnet> to
  any nat-to (<uplink>)` through the same `-f -` path, and rejects
  `uplink`/`subnet` containing `\n` or `\r` as defense in depth,
  because it "has no way to know that validation ran."

`Manager` is a zero-sized struct. It holds no state whatsoever - no
last-applied text, no generation counter, no anchor inventory.

### Tests

`internal/pf/rules_test.go` (110 lines) and
`internal/pf/manager_test.go` (27 lines) cover the renderer and the
error-wrapping. Neither needs pf installed, because `RenderRules` is
pure. That is a genuinely good property and the reason the renderer
can be extended safely.

### The caller: `internal/cluster/reconciler.go`

The reconciler holds a local interface, deliberately narrow:

```go
type pfManager interface {
    Apply(ctx context.Context, anchor string, rules []pf.Rule) error
    Flush(ctx context.Context, anchor string) error
    ApplyNAT(ctx context.Context, anchor, subnet, uplink string) error
}
```

with the comment "`*pf.Manager` satisfies this today." Anchors:

```go
func vmAnchor(id string) string        { return "apiary/vm-" + id }
func natAnchor(networkID string) string { return "apiary/net-" + networkID }
```

`natAnchor`'s comment records that it is a sibling "one flat level
under `apiary/` so it's covered by the same `anchor "apiary/*" all`
reservation `vmAnchor` already relies on, with no separate host
prerequisite needed." `toPFRules` does the ADR-0075 stable sort and
maps to `pf.Rule`; `effectivePFRules` returns `nil` when
`VMDefinition.FirewallPaused` is set (ADR-0049). Apply is called at
reconciler.go:1195 and :1321, Flush at :859, :926, :1011 and :1463,
`ApplyNAT` at :1378. Every tick, full-replace, unconditionally.

### The wire schema: `api/internalpb/state.proto`

`FirewallRule` (line 284) carries exactly five fields: `direction`,
`action`, `protocol`, `port_range`, and `int32 priority = 5` (ADR-0075,
documented in the proto). It is embedded in `VMDefinition` and is the
payload of the `SetVMFirewallRules` command (line 597). There is no
`src`, no `dst`, no interface, no policy object, no firewall field
outside `VMDefinition`.

### The RPC surface: `api/rpc/manager.proto` and `internal/manager`

`service ManagerService` (line 11) exposes, among the firewall surface,
`SetVMFirewallPaused` (line 90) and `SetVMFirewallRules` (line 110).
`message FirewallRule` (line 726) "mirrors api/internalpb's
FirewallRule." `SetVMFirewallRulesRequest` is at line 881.

`internal/manager/server.go` implements both with the house pattern:
submit a narrow `internalpb.Command`, then, on `ErrNotLeader`,
forward to `s.peerManagerdAddr(leaderHint)` and return
`augmentForwardError(appErr, leaderHint, ferr)`. The handler comments
cite the reason this two-step exists: forwarding first would let a
follower mutate state the leader does not have (ADR-0049).

### Adjacent machinery this design must respect

- `internal/raft/fsm.go`: the replicated state is a set of in-memory
  maps keyed by ID - `vms`, `networks`, `apiKeys`, `jails`,
  `pendingJoinRequests`, `restartLeases`, `restartRecords` - plus a
  one-way `authEnabled` flag. `FSMApplyResult` echoes back
  `Index`, `VM`, `Network`, `ApiKey`, `Jail`, `PendingJoinRequest`,
  `RestartLease`, `RestartRecord`, `Error`. There is no KV keyspace
  and no schema versioning per object; the FSM is a state machine over
  typed `Command` messages.
- `internal/raft/node.go`: leader-gated `GetVM`, `ListVMs`,
  `GetNetwork`, `ListNetworks` returning `ErrNotLeader`; ungated
  `ListVMsLocal`, `ListNetworksLocal`,
  `GetPendingJoinRequestLocal`, `RestartLeaseStateLocal` for
  reconcilers, "with NO leadership check"; plus `Apply`, `LeaderHint`,
  `Status`, `ExportState`.
- `internal/assumptions/manager.go`: `Status` is four-state -
  `true`, `false`, `unknown`, and `not_applicable`, with
  `not_applicable` documented as "a real, positive fact about scope,
  not an unresolved measurement. Must never be conflated with
  StatusUnknown by any producer or consumer." `Key` is
  `{Kind, SubjectKind, SubjectID, DependencyID, Qualifier,
  ObservedByNodeID}`; `Result` is `{Key, ObservedStatus, ReasonCode,
  Detail, LastObservedAt}`; `HistoryEntry` is the journal;
  `MaxDetailLen = 500` with `ClampDetail` redacting credential-shaped
  substrings.
- `internal/assumecheck/checker.go`: the sole producer, running one
  pass per tick. `checkNATUplink` (line 286) is the closest existing
  neighbour, emitting `assumptions.KindNATUplinkDefaultRoute` with
  `SubjectKind: assumptions.SubjectKindNode`. Its test
  `TestRunOnce_NATUplinkNotConfigured_IsNotApplicableNotUnknown`
  establishes the precedent this ADR follows: an unconfigured NAT
  uplink is `not_applicable`, *not* `unknown`.
- `internal/netif` exposes `func List() ([]Interface, error)` -
  interface inventory on a node. `internal/vlan` owns
  `EnsureVLAN`/`InterfaceStatus` and the bridge lifecycle.
- `internal/health` (`compute.go`, `health.go`) and
  `internal/coverage` (`scenario.go`) are the health and coverage
  surfaces; [ADR-0122](0122-cluster-evidence-aware-health-api.md) is
  the cluster-wide evidence API this design must feed rather than
  reinvent.
- `api/rpc/manager.proto` line 2515 onward: `AssumptionKind` enum,
  currently `ASSUMPTION_KIND_UNSPECIFIED = 0` through
  `ASSUMPTION_KIND_REPLICA_NETWORK_BRIDGE_UP = 5`.

### What is missing, stated plainly

1. **No cluster-wide object.** Firewall state lives inside
   `VMDefinition`. There is no policy, no Comb-scope ruleset, nothing
   in raft that a firewall operator can read without reading three
   VMs.
2. **No address or interface matching.** `from any to any` is a
   literal, not a default. No `on <iface>`, no `from <cidr>`, no
   `to <cidr>`.
3. **No staged apply, no rollback, no deadline.** `Apply` is
   immediate and total. There is no state in which a ruleset is
   "pending" and could be withdrawn.
4. **No read-back.** `Manager` cannot ask pf what it loaded. It
   writes and assumes.
5. **No memory.** `type Manager struct{}` retains nothing, so there is
   no last-known-good copy to revert to, in memory or on disk.
6. **No idempotence token.** Full-replace every tick makes repeated
   application safe but makes it impossible to tell "this Combs'
   ruleset is the one we asked for" from "this Combs' ruleset is
   whatever it happens to be."
7. **No IPv6 story.** `ApplyNAT`'s single rendered line is IPv4 NAT by
   construction. There is no family concept anywhere in the package.
8. **No anchor inventory.** Nothing knows which `apiary/*` anchors
   exist, so nothing can detect an orphan.
9. **Fail-open, unconditionally.** `Apply` with no rules means
   "everything allowed." Correct as an API convention; dangerous as
   the *only* outcome of a failure, because every error path in the
   reconciler currently leads to "the ruleset on disk is whatever the
   last successful tick wrote."

## Decision

### Scope: both, asymmetrically

v1 ships **both** scopes on one replicated model, with deliberately
unequal safety machinery.

- **Cell (guest) scope** is the existing per-VM anchor path
  (`VMDefinition.firewall_rules` → `toPFRules` → `vmAnchor`),
  extended with the new rule fields. It gets **no** staging and **no**
  rollback, because a guest ruleset cannot sever the operator's
  access - provided, and only provided, the renderer is fixed to
  constrain Cell rules to the guest's own interface. That fix is
  mandatory before this scope is exposed at all.
- **Comb (host) scope** is the new `FirewallPolicy` object, and it is
  where the staged/pending/rollback machinery lives in full. It is
  Admin-only, never default-deny in v1, and always additive over the
  operator's own hand-written host rules.

Why both rather than one: the blast radius differs by orders of
magnitude, and collapsing them into one mechanism would either
over-protect the guest (a rollback timer and a confirm dialog for
changing a jail's rules) or under-protect the host (shipping host
rules on the guest's trust model). One *model*, two *guardrails*.

Why Cell scope is not a new raft object: ADR-0127 says "Firewall
policy attached to Comb or Cell." For Cell, that attachment already
exists as `VMDefinition.firewall_rules`, with ADR-0075 semantics and
ADR-0049 pause. Adding `FirewallPolicy` for Cells too would create two
competing sources of truth for one guest's rules, which is precisely
the class of ambiguity this project has spent ADRs removing. The
`FirewallPolicy` proto reserves a `subject_id` for a future
Cell-attached policy that composes with, rather than replaces, the
per-VM list.

**Divergence from ADR-0127, stated:** ADR-0127 lists "network
objects - reusable IP/CIDR/group definitions (referenced by rules)" and
"live logging - pflog capture." Neither is in v1. Rules take literal
CIDRs. pflog is deferred to a follow-up ADR, because it is a read-only
observation surface with none of the blast-radius problems that
justify doing it now, and because `pflog(4)` needs its own device
setup on a host this project does not configure. Both are recorded as
Open questions, not silently dropped.

### The most important property: a ruleset must never sever
its own recovery path

Four mechanisms, in order of how hard they work.

**1. Static admissibility check, before anything is committed.** A
`FirewallPolicy` is rejected at apply time - in the FSM, so the
rejection is replicated and identical on every voter - if it would
make the Colony unrecoverable. The checker is deny-by-default and
runs over the *rendered* ruleset, not the rule list, because that is
what pf will actually evaluate. Mandatory invariants:

- Every management-path port is explicitly allowed inbound on the
  policy's own interfaces: the managerd gRPC port, the frontend port,
  and the raft port. These ports are read from the Colony's own
  membership and config, not hardcoded, so a changed fixed-listener
  port (ADR-0108) does not silently invalidate the check.
- Every peer-to-peer direction the Colony can legitimately need -
  `any` Combs' raft and managerd ports to `any` Comb - must be allowed
  by at least one `pass` rule that no `block` rule precedes, in at
  least one of the policy's directions. This is a *reachability*
  check, not a pattern match: the checker builds the Colony's own
  communication graph from raft membership and asks whether the policy
  would cut any edge. A single remaining voter-reachable path is
  enough to commit.
- A host policy with `default_deny: true` and no explicit `pass` for
  the management path is rejected outright.
- Reject any rule referencing an interface that does not exist on any
  member node (`internal/netif.List()` on each Comb, evidence-gathered).
  A typo'd `on em9` is a rule pf accepts and silently never matches -
  the worst outcome, because it looks enforced and is not.

A policy that fails admissibility never reaches raft. Failure is
explicit, in the RPC response, with the violated invariant named.

**2. Staged apply with a local deadline.** A committed policy becomes
`PENDING`, not `ACTIVE`, when it is a host-scope change. Each Comb
then:

- writes the new rendered ruleset to
  `/var/db/apiary/firewall/pending.<generation>.rules` and the
  current active one to `/var/db/apiary/firewall/known-good.rules`,
  both `0600`, both fsynced;
- applies the *staged* ruleset to the anchor;
- starts a monotonic deadline - default 120 seconds, capped by
  `net.inet.ip.pf.` - after which it reverts with no further input
  required;
- reports, continuously, whether the management path is intact
  *under the staged ruleset*.

**3. Automatic rollback, performed locally by managerd.** This is the
part that matters most, and its defining constraint is that **it must
not require raft, the leader, or the operator.** A ruleset that severs
replication is precisely the case where the leader may be unreachable
and the operator is at a serial console. So:

- The rollback actor is **managerd on the Comb itself**, driven by its
  own timer. Not the leader. Not raft. Not a peer.
- The rollback source is the Comb's **local** `known-good.rules`
  file, already on disk before the staged apply begins. If that file
  is missing or unreadable, the second source is the previous
  generation's policy from raft; if raft is also unreachable, the
  third and final fallback is `Manager.Flush` on the anchor - the
  empty, everything-allowed state that `Apply`'s own doc comment names
  as "this project's de facto behavior before firewall support
  existed." Fail open, always, and record that it did.
- The **rollback trigger** is: the deadline expires without the Comb
  having recorded a positive `pf_management_path_intact` observation
  within the preceding 15 seconds. The Comb establishes that
  observation itself, from inside, with two probes: a successful
  gRPC call to at least one raft peer, and a successful bind-and-
  accept on its own managerd listener. A Comb that cannot reach any
  peer *and* cannot serve its own listener has demonstrably broken its
  own path, whatever the ruleset says.
- Rollback is itself staged-and-verified in the cheap direction: the
  Comb restores `known-good.rules`, confirms its probes pass, and only
  then reports `ROLLED_BACK` with the generation it fell back from.
  If even the restore fails to restore connectivity, the Comb flushes
  the anchor and reports `ROLLED_BACK_UNVERIFIED` - a loud, honest
  "we tried and we are not sure", which is `unknown` on the operator's
  screen, never `false`.

**4. Operator confirmation to promote, not to apply.** `PENDING`
becomes `ACTIVE` only on an explicit `CommitFirewallPolicy`, or
automatically when the deadline passes with the probes green - the
deadline is a *maximum* time to live, not a mandatory wait. There is
no state in which a host ruleset becomes permanent by accident, and
no state in which an operator's next action is the only thing keeping a
host reachable.

Two deliberate omissions, stated so they are not mistaken for
oversights. There is **no** scheduled "rehearse, wait, confirm"
window beyond the automatic deadline - a longer human-in-the-loop
window means a node that reboots mid-window comes up with a pending
ruleset nobody is watching, so the ruleset must be checkpointed into
`/etc/pf.conf` or it does not survive a reboot, and that checkpoint is
itself a privileged host file this project has decided not to manage
(Open questions). And there is **no** out-of-band recovery channel;
ADR-0114's reasoning applies directly. The recovery path for a
genuinely wedged host is the same one it has always been: physical or
console access, which is a documented requirement, not a feature.

### Raft-replicated representation

**Model.** A `FirewallPolicy` is one Comb's host ruleset.

**Keyspace.** `internal/raft/fsm.go` has no KV keyspace; the FSM is
in-memory maps keyed by ID. So the keyspace is a new map,
`firewallPolicies map[string]*internalpb.FirewallPolicy`, keyed by
`comb_id` - exactly one active policy per Comb, which matches ADR-0127's
"attached to Comb." A second map,
`firewallPolicyHistory map[string]*internalpb.FirewallPolicy`, holds
the single previous generation per Comb, so a Comb that must roll back
can recover the last-good *content* from raft when its local cache is
gone. Two generations, not unbounded history: enough for rollback, not
a ruleset archive.

**Versioning.** Every policy carries a monotonically increasing
`generation` (uint32), assigned by the FSM, never by the client. The
rule is the idempotence key: a Comb applies generation *G* only if
`G > applied_generation`, so a re-delivered, replayed, or
out-of-order command is a no-op rather than a re-render. Rollback
sets `rolled_back_to = <previous generation>` and bumps *nothing* -
the generation counter must never go backwards, or a lagging Comb
would refuse to re-apply the restored ruleset. `known_good_generation`
is written back by the owning Comb (like `UpdateVMPhase` is written
back by the reconciler) and is the only field a Comb ever writes.

**Idempotent application on every Comb.** The existing full-replace-
every-tick discipline is retained - it is genuinely the right call for
a declarative ruleset - but gains the generation guard and a
before/after comparison so a *no-change* tick does not invoke
`pfctl` at all. The Comb computes
`digest = sha256(RenderRules(policy.rules))`. `RenderRules` is pure
and deterministic (rules pre-sorted by ADR-0075's stable
`priority` sort), so the leader and every Comb compute the same digest
from the same bytes without it ever being transmitted. Idempotence is
then explicit: if the observed digest already equals the expected
digest, do nothing.

**Drift detection, with evidence, never silent reconciliation.** Drift
is `expected_digest != observed_digest`, where `observed_digest` comes
from reading the anchor back (`pfctl -a <anchor> -sr`) and
canonicalising the result. Drift is handled as follows, in order:

1. Record a `HistoryEntry` in `internal/assumptions` with the Combs'
   id, the anchor, both digests, and the mismatching rule, timestamped.
   Drift becomes a journal event with a lifetime, not a transient log
   line nobody reads later.
2. Re-apply once, immediately, from the replicated policy. The repair
   is automatic because full-replace makes it safe.
3. Re-read. If the digest now matches, the Comb reports
   `pf_policy_anchor_applied = true` and the journal still shows the
   drift-and-repair. If it does not match, the Comb reports `false`
   with pfctl's stderr clamped to 500 characters, and **stops
   retrying after three consecutive failures**, escalating to
   `pf_reconcile_erroring` rather than hammering `pfctl` on every tick
   forever.
4. A Combs' ruleset that drifts and repairs more than a configurable
   number of times in a window is surfaced as its own health signal.
   A flapping firewall is a distinct failure from a firewall that
   never applied, and collapsing them loses the diagnosis.

The non-negotiable part: **drift is never reconciled silently.** No
path in this design repairs a Combs' anchor without leaving a
timestamped record an operator can find afterwards.

### What is NOT replicated

Not replicated, deliberately:

- **Loaded rules as pf reports them** (`pfctl -a <anchor> -sr`). It is
  an observation of a Comb, gathered live like `GetLocalNodeHealth`,
  not a source of truth.
- **Packet and byte counters**, state-table size, `pfctl -si` and
  `-s rules` statistics, `pflog(4)` output. High-churn, unbounded,
  and physical: two Combs with identical policies will never have
  identical counters, so replicating them creates permanent, meaningless
  divergence and log pressure for zero decision value.
- **Connection state.** A guest's in-flight TCP through a Cell anchor
  is not a coordination fact and is not durable across a `pfctl -f`
  load anyway - reloading an anchor with a changed ruleset flushes
  matching state by pf's own rules.
- **Which anchors exist.** Derived, not authoritative: it is
  `Flush`-able at any time and `Apply`-able lazily, and
  `internal/pf`'s own `Flush` doc already establishes "No such
  anchor" as a success case. The Combs' anchor inventory is a
  *report*, and a Combs' report that disagrees with expectation is
  drift, not a conflict to resolve.
- **The known-good ruleset content** - this is the sharpest call.
  It *is* recoverable from raft (previous generation), but the design
  never *requires* raft to reach it, because the whole point is that
  raft may be the thing that is cut. Local disk is the primary; raft
  is the secondary; empty-anchor flush is the floor.

The line is the one `state.proto`'s own header comment draws:
"ephemeral state is small, JSON-shaped facts [...] as opposed to
physical state (ZFS datasets, HAST-replicated bytes), which never goes
through raft." Firewall *intent* is a fact every node must agree on.
Firewall *effect* is a fact about one kernel.

### Interaction with existing `internal/pf`

Reused verbatim, unchanged:

- `RenderRules(rules []Rule) (string, error)` - the entire pure
  render path, its validation, and `renderPortRange`'s `"8000-9000"`
  → `8000:9000` conversion. ADR-0075's no-`quick` semantics carry
  over untouched, because the priority meaning is a property of
  ordering, not of pf syntax.
- `Manager.Apply` - the `pfctl -a <anchor> -f -` invocation, the
  stdin plumbing, the stderr-in-the-error-message behaviour, the
  full-replace-not-diff convention.
- `Manager.Flush` - including the "No such anchor" success case,
  which is the fail-open floor's implementation.
- `runCmdStdin`'s error format. `Detail` strings in every new
  evidence result depend on it, and it already clamps well via
  `assumptions.ClampDetail`.
- `Rule`'s decoupling from the wire schema, and the reconciler's
  narrow `pfManager` interface discipline. The new work declares its
  own even narrower local interface rather than widening `pfManager`.

Extended:

- `Rule` gains `Source`, `Destination`, `Interface`, and `Comment`.
- `renderRule` gains the `on`/`from`/`to` clauses **and a hard
  rejection** of any non-empty field it cannot render. This rejection
  is the important half. Today's renderer silently ignores fields it
  does not know about, so a future field added to the proto and
  forgotten here would be dropped on the floor and the operator would
  believe traffic was restricted when it was not. Failing loudly is
  the only safe default for a security-relevant renderer.
- New, alongside: `Readback(ctx, anchor) (string, error)` wrapping
  `pfctl -a <anchor> -sr`; `Parse(ctx, body) error` wrapping
  `pfctl -n -f -` (parse-only) for pre-apply validation; and
  `Enable(ctx) (bool, error)` reporting whether the pf instance is
  live, so "pf is off" becomes an observable fact rather than an
  assumption.

Not extended, and why: `ApplyNAT` stays exactly as it is. Its
`match out on ... nat-to (...)` line is IPv4 NAT by construction; a
v6 equivalent needs `rdr`/`map` or a NAT64 translator, which is a
different feature with different evidence. Widening it to claim
family-agnosticism would be a lie in a function signature.

### Quorum, and a degraded Colony

Split cleanly:

- **Deciding requires quorum.** A ruleset change is a raft `Apply`
  against the leader, and raft commits a majority or not at all. There
  is no local, off-ledger path that changes a host ruleset. In a
  degraded Colony with no leader, the operator gets `ErrNotLeader`
  plus a `LeaderHint`, which is the exact existing behaviour of
  `SetVMFirewallRules` and `internal/health`'s membership arithmetic -
  not a special case, and not something this ADR gets to invent.
- **Executing must not.** The moment a policy is committed, every
  member applies it locally, including a member that has since lost
  contact with the leader. A Combs' local apply is gated on its own
  generation counter, never on a fresh quorum check. A half-applied
  Colony is strictly worse than a fully-applied one, because the
  operator cannot tell which Combs are protected.
- **Staging and rollback are raft-independent by construction.**
  Already described; restated here as a quorum property, because it is
  the one that matters: no rollback path in this design consults raft
  before attempting to restore connectivity. The multi-step
  local-file → raft-history → flush chain tries each in order, per
  Comb, without coordination.

`default_deny: true` for host scope is additionally gated on a healthy
Colony in the UI, as a soft guardrail, because turning it on during an
incident is how a recoverable incident becomes a physical-visit
incident. The server-side admissibility check is the real gate; this
is just the surface telling the operator what the check will say.

### The reconcile loop and its failure modes

Every tick, per Comb, in this order:

1. Read the local (ungated, leadership-free) policy. No policy
   assigned → `not_applicable`, and stop. This is the
   `TestRunOnce_NATUplinkNotConfigured_IsNotApplicableNotUnknown`
   precedent, applied.
2. `digest = sha256(RenderRules(sorted rules))`. If the policy is
   `PENDING` and the deadline has not passed, handle the pending
   lifecycle instead (below).
3. `observed, err := PF.Readback(anchor)`. If `err` is a missing
   `pfctl`, report `unknown` with reason `pfctl_missing` - a missing
   tool is absence of evidence, not a failed ruleset. If `err` is a
   pfctl invocation failure, report `false` with the clamped stderr;
   we did observe the failure.
4. If `observed == digest`, done. If not, `Apply`, then re-read and
   confirm; count consecutive failures and stop at three.

Failure modes, each with its honest verdict:

| Failure | Verdict | Why |
|---|---|---|
| `pfctl` binary not on `PATH` | `unknown` / `pfctl_missing` | Cannot determine; must not be `false` |
| pf instance not enabled (no rules loaded, `pf_enable="NO"`) | `false` / `pf_disabled` | We verified pf is off, so the policy is verifiably not in effect. This is the one place `false` is right, and it is right because the check succeeded |
| Anchor absent (never created) | `false` / `anchor_missing` | Same: positively observed absence |
| `pfctl` non-zero exit, stderr captured | `false` / `pfctl_error`, detail clamped to 500 chars | Observed failure |
| Readback succeeds, digest differs | `false` / `drift_detected`, then repaired | Positively observed mismatch |
| Reconcile tick never runs (managerd wedged, or the pending ruleset blocked its own RPC) | `unknown` / `stale` | No observation at all. This is the case that makes lockout survivable and it must never read as healthy |
| Policy is `INET`-only and any Combs' interface holds a global IPv6 address | `false` / `v6_unenforced` | The operator believes they are protected; they are not, and we can prove it from `internal/netif.List()` |

**IPv4 vs IPv6.** `FirewallPolicy.family` is explicit:
`INET` (default), or `INET6`. A host policy of `INET` never implies
`INET6` - they are separate rulesets, loaded by separate `pfctl`
invocations, and pf will happily enforce one while leaving the other
wide open. The v6 row of that table is deliberately a `false` and not
an `unknown`: the v6 gap is verifiable, and an operator who cannot
tell "I did not configure v6" from "v6 is unprotected" will file it as
a bug the day it bites. The IPv6 gap is the single most likely way
this feature ships looking correct and being wrong.

**Pending lifecycle**, when `policy.state == PENDING` and
`deadline_at` is set:

1. Every tick, before the anchor is touched, run the two local probes
   (raft peer gRPC, own-listener bind/accept).
2. All green → `pf_management_path_intact = true`, keep waiting.
3. Any red, for 15 consecutive seconds, *or* the deadline expiring
   without green → roll back, as specified above. The trigger is the
   disjunction; either alone is sufficient, and both are
   conservative in the direction of restoring access.
4. After rollback, re-probe. Green → `ROLLED_BACK`, `false` for
   "policy applied", with `Detail` naming the generation it fell back
   from. Still red → `ROLLED_BACK_UNVERIFIED` and flush the anchor.

## Rejected alternatives

**1. Manage `/etc/pf.conf` itself from the Colony.** Write the whole
file and `pfctl -f /etc/pf.conf`. Rejected: `/etc/pf.conf` is
explicitly a host prerequisite this project does not set up
(`internal/pf/exec.go`'s own package doc, by explicit analogy to
hastd and dnsmasq); it is shared with whatever the operator has
hand-written; and a whole-file load is the single most destructive
action available, applied atomically, with no anchor-scoped blast
radius. Anchors are the only unit small enough to make staging
meaningful.

**2. Ship the staged/rollback machinery later, host scope first with
immediate apply.** Rejected outright. Immediate apply is the exact
mechanism that can cut a quorum voter's replication. The guardrail
is not a follow-on; it is the feature.

**3. Keep the per-VM `firewall_rules` design and add a separate
`FirewallPolicy` object for Cells too.** Rejected: two sources of
truth for one guest's rules, with no defined precedence. ADR-0049
(pause) and ADR-0075 (priority) already give the per-VM list
well-defined semantics; layering a second object over it invites the
"which one is actually in effect" question that this project's
evidence work exists to eliminate. `FirewallPolicy` is Comb-scoped in
v1, with `subject_id` reserved.

**4. Rely on raft for rollback - the leader reverts a Combs' ruleset
when it detects the breakage.** Rejected: it requires the network the
ruleset just broke, it requires an operator or an automated
controller that is itself reachable, and in the voter-to-voter
severance case it requires exactly the thing that is gone. This is
the alternative that turns a recoverable accident into a site visit,
and it is the reason rollback is local-by-construction here.

**5. Replicate live PF state - counters, loaded rules, state tables -
so the Colony has one authoritative view.** Rejected: unbounded,
high-churn, and physically divergent by nature; it would put a
permanent, meaningless diff into raft's log on every tick. It also
inverts the evidence model ADR-0122 established, where the leader
gathers observations from Combs rather than being handed them as
replicated truth.

**6. `default_deny: true` on host scope from day one.** Rejected on
blast radius, per ADR-0114's reasoning: it maximizes the chance that
a mistake is unrecoverable, and the admissibility check, not a
default, is what should decide whether a policy is safe. Deferred to
after the testbed lockout rehearsal has actually been performed.

**7. Reuse the existing `Rule` struct's `from any to any` and express
host policy as a rule ordering trick.** Rejected: there is no
ordering trick that can distinguish guest-bound from host-bound
traffic when the rule cannot name an interface. The renderer has to
change. The alternative - which is today's state - is that a Cell
rule can match the operator's own SSH.

## Consequences

### Positive

- A Comb's host ruleset becomes a first-class, readable, versioned
  raft object rather than a fact scattered across three `VMDefinition`
  messages.
- The existing renderer finally *can* express a real policy, which
  also fixes the pre-existing per-VM host-exposure hazard described in
  Context - a bug found by this design, not a risk it merely manages.
- Lockout becomes a bounded, observable, self-healing event with a
  named trigger and a named actor, instead of an unmitigated operator
  hazard. Every Combs' firewall page can state, at any moment, which
  generation is live, which is pending, when the pending one expires,
  and what the known-good fallback is.
- Drift is a first-class journal event with a repair count, so
  "the firewall on this node is flapping" is a distinguishable
  diagnosis from "the firewall never applied."
- The IPv6 gap becomes an explicit `false` on the health surface
  rather than an absence nobody notices.
- The IPv4/IPv6 split, the missing-pfctl case, and the never-ran case
  are all handled by the existing four-state `assumptions.Status`
  vocabulary without a new concept, and without any path that reports
  `false` for something merely unobserved.

### Negative

- The renderer change breaks the assumption that
  `VMDefinition.firewall_rules` is inert with respect to host traffic.
  Existing per-VM rules that were, by accident, shaping host
  behaviour will behave differently once `on <iface>` is emitted. This
  needs a migration note and a testbed rehearsal, not a silent
  release.
- Fail-closed rejection of unrendered fields means a future proto
  field that lands before its renderer change turns into a hard apply
  failure rather than a silent no-op. That is the intended trade -
  a loud outage beats a quiet security hole - but it will surprise
  someone.
- Three new evidence kinds, three new proto commands, one new FSM
  map plus a history map, six new RPCs, and a per-Combs' state machine
  with a timer. This is a real amount of code for a feature whose
  entire value proposition is that it is not allowed to be simple.
- The design only covers pf. A host with nftables, ipfw, or no packet
  filter at all is out of scope, and the evidence says
  `not_applicable` for anything that is not pf rather than pretending
  the Colony is filtered.
- The last-resort flush to "everything allowed" is a real, deliberate
  security regression. It is the right default - availability of the
  recovery path beats the policy - but it must be loud in the journal
  every time it happens, and it is a standing argument for keeping
  backups of the previous generation.
- No reboot survival. A Combs' pending ruleset does not survive a
  reboot, because checkpointing into `/etc/pf.conf` means managing a
  host file this project has declined to manage. A node that reboots
  mid-pending comes back unfiltered, which is safe but surprising.

## Implementation notes

### Proto: `api/internalpb/state.proto`

Extend the existing rule type rather than adding a parallel one, so
the per-VM path and the host path share one renderer and one set of
priority semantics:

```protobuf
message FirewallRule {
  string direction = 1;
  string action = 2;
  string protocol = 3;
  string port_range = 4;
  int32 priority = 5;
  // New in ADR-0129. Empty preserves the pre-ADR-0129 rendering
  // exactly ("from any to any"), which is why this is additive
  // rather than a breaking change to existing rules.
  string source = 6;        // literal CIDR, e.g. "10.0.0.0/8"
  string destination = 7;   // literal CIDR
  string interface = 8;     // pf(8) interface name, e.g. "em0"
  string comment = 9;       // operator note, never rendered
}
```

`comment` is deliberately not rendered into the ruleset: a
free-text field interpolated into pf syntax is an injection vector,
and `ApplyNAT`'s existing newline rejection is the precedent for
treating that as hostile input rather than a formatting concern.

New policy object and lifecycle:

```protobuf
enum FirewallScope {
  FIREWALL_SCOPE_UNSPECIFIED = 0;
  FIREWALL_SCOPE_COMB = 1;      // the host itself - ADR-0129 v1
  FIREWALL_SCOPE_CELL = 2;      // reserved; Cells use VMDefinition.firewall_rules
}

enum FirewallFamily {
  FIREWALL_FAMILY_UNSPECIFIED = 0;
  FIREWALL_FAMILY_INET = 1;     // IPv4 - the default
  FIREWALL_FAMILY_INET6 = 2;    // IPv6 - never implied by INET
}

enum FirewallPolicyState {
  FIREWALL_POLICY_STATE_UNSPECIFIED = 0;
  FIREWALL_POLICY_STATE_PENDING = 1;   // staged, deadline running
  FIREWALL_POLICY_STATE_ACTIVE = 2;
  FIREWALL_POLICY_STATE_ROLLED_BACK = 3;
  FIREWALL_POLICY_STATE_ROLLED_BACK_UNVERIFIED = 4;
}

message FirewallPolicy {
  string comb_id = 1;             // keyspace key: one policy per Comb
  repeated FirewallRule rules = 2;
  FirewallScope scope = 3;
  FirewallFamily family = 4;
  string anchor = 5;              // default "apiary/host-<comb_id>"
  bool default_deny = 6;          // host scope only; false in v1 by default
  uint32 generation = 7;          // FSM-assigned, monotonic, never decreases
  uint32 known_good_generation = 8;  // written back by the owning Comb only
  uint32 rolled_back_to = 9;      // 0 when not rolled back
  int64 deadline_at_unix = 10;    // pending deadline; 0 when not pending
  repeated string peer_comb_ids = 11;  // membership snapshot used for
                                       // the admissibility check
}
```

Commands, as oneof arms on `Command`: `CreateFirewallPolicy`,
`UpdateFirewallPolicy` (bumps `generation`, sets `PENDING` for host
scope, sets `deadline_at_unix`), `DeleteFirewallPolicy` (applies the
same staged/rollback path - deleting a firewall is as dangerous as
writing one), `ConfirmFirewallPolicy` (`PENDING` → `ACTIVE`),
`RollbackFirewallPolicy` (operator-initiated, same machinery as
automatic), and `ReportFirewallObservation` (the Combs' write-back
of `known_good_generation`, `applied_generation`, and
`observed_digest`).

`DeleteFirewallPolicy` going through staging is the least obvious
decision here and the one most worth arguing about in review. A
delete that immediately empties the anchor is indistinguishable, from
outside, from a lockout. The blast radius of a mistake is identical in
both directions, so the guardrail is not.

### Raft state keys

- `FSM.firewallPolicies map[string]*internalpb.FirewallPolicy`, keyed
  by `comb_id`.
- `FSM.firewallPolicyHistory map[string]*internalpb.FirewallPolicy`,
  keyed by `comb_id`, one previous generation deep.
- `FSMApplyResult` gains `FirewallPolicy *internalpb.FirewallPolicy`,
  following the existing per-type field pattern.
- `internal/raft/node.go` gains `GetFirewallPolicy` and
  `ListFirewallPolicies` (leader-gated, `ErrNotLeader`, matching
  `GetNetwork`) plus `ListFirewallPoliciesLocal` (ungated, for
  reconcilers, matching
  `ListNetworksLocal`). Leader-gating the read and ungating the local
  read is the existing split and this feature does not get a third
  convention.
- `ExportState`'s `FSMSnapshotState` gains the policy map, so a
  restored node rebuilds its firewall intent from the same snapshot as
  everything else.

### `internal/pf` changes

Additive only; no existing signature changes, so
`internal/cluster`'s narrow `pfManager` interface keeps compiling
unchanged and `rules_test.go` keeps passing.

- `Rule` gains `Source`, `Destination`, `Interface`, `Comment`.
- `renderRule` emits `on <interface>`, `from <source>`,
  `to <destination>` when non-empty, and **returns an error** for any
  non-empty field it cannot render.
- New `Readback(ctx, anchor) (string, error)` - `pfctl -a <anchor> -sr`.
- New `Parse(ctx, body) error` - `pfctl -n -f -`, parse-only.
- New `Enabled(ctx) (bool, error)` - distinguishes "pf is off" from
  "pfctl is missing", which the two `unknown`/`false` rows of the
  verdict table both depend on.

Failure modes: `pfctl -sr` output format is not a stable API and will
need canonicalising before digesting - a `pfctl` version bump that
reformats its own output must not read as drift on every Comb. The
canonicaliser therefore normalises whitespace and sorts rules into
render order before hashing, and a canonicaliser change is a
generation-visible event, not a silent one.

### managerd RPC handlers

`api/rpc/manager.proto`, following the `SetVMFirewallRules` shape
exactly (narrow command, `ErrNotLeader` → forward to
`s.peerManagerdAddr(leaderHint)`, `augmentForwardError`):

- `GetFirewallPolicy` / `GetFirewallPolicyResponse` - Viewer.
- `StageFirewallPolicy` (create-or-update, enters `PENDING`) - Admin.
- `ConfirmFirewallPolicy` - Admin.
- `RollbackFirewallPolicy` - Admin.
- `DeleteFirewallPolicy` - Admin, staged.
- `GetFirewallStatus` - Viewer; the per-Combs' evidence snapshot
  (expected digest, observed digest, generation, pending deadline,
  probe results), the same shape ADR-0122's cluster evidence API
  already aggregates.
- `ListFirewallStatus` - Viewer, Colony-wide.

`internal/manager/auth.go`: Viewer for the two reads, Admin for the
four mutations, in the existing `requiredRole` map.

Failure modes: on a leader-less Colony, every mutating RPC returns
`ErrNotLeader` plus a `LeaderHint` and changes nothing - not even the
pending deadline. A staged policy whose deadline expires while the
Colony has no leader is a real case: the Combs' local timer still
fires and still rolls back, because that is deliberate, but the
operator gets no notification until a leader returns. That is
acceptable (fail-open) and must be documented in the UI's degraded
banner rather than left to be discovered.

### Reconcile loop

Per tick, per Comb, in `internal/cluster/reconciler.go`, over a new
local interface narrower than `pfManager`:

```go
type hostFirewall interface {
    Apply(ctx context.Context, anchor string, rules []pf.Rule) error
    Flush(ctx context.Context, anchor string) error
    Readback(ctx context.Context, anchor string) (string, error)
    Parse(ctx context.Context, body string) error
    Enabled(ctx context.Context) (bool, error)
}
```

`hostFirewall` is a **separate** interface from `pfManager` rather than
an extension of it, so a Comb that was never assigned a host policy
depends on neither. The existing per-VM path is not touched by any of
this; it keeps its own anchor, its own interface, its own tick.

Failure modes: as tabulated in the Decision section. The three that
need a named owner are (a) the three-consecutive-failure escalation
(`pf_reconcile_erroring`), (b) the readback canonicaliser drift
described above, and (c) the never-ran case, which surfaces as
`LastObservedAt` going stale - the honest signal, and one
`internal/assumptions` already records per tick without throttling.

### Evidence

Three new `assumptions.Kind` constants in
`internal/assumptions/manager.go`, all with
`SubjectKind: SubjectKindNode` and an empty `SubjectID` (the local
Combs' implicit subject, matching `checkNATUplink`), and three new
`ASSUMPTION_KIND_*` values continuing the enum at
`api/rpc/manager.proto` line 2515 (next free: 6, 7, 8):

- `pf_policy_anchor_applied` - `true` when observed digest equals
  expected digest for the active generation. `false` +
  `drift_detected` / `pfctl_error` / `anchor_missing` / `pf_disabled`
  on positive observation. `not_applicable` when no policy is assigned
  to this Comb, per the `checkNATUplink` precedent. `unknown` +
  `pfctl_missing` or `stale` when there is no observation at all.
- `pf_management_path_intact` - the two local probes, under the
  staged ruleset. `not_applicable` when nothing is pending.
- `pf_policy_v6_enforced` - `false` + `v6_unenforced` when the policy
  is `INET`-only and any local interface holds a global IPv6 address.

Every `Detail` goes through `assumptions.ClampDetail` (500 chars,
credential-shaped substrings redacted) - mandatory, because
`pfctl` stderr is untrusted input that can contain an operator's
typo'd hostname or, in a path, something credential-shaped.

`internal/health`: **no change to the verdict chain in v1.** Folding
firewall state into `ComputeNodeHealth` would silently change existing
health verdicts for every current deployment, which is ADR-0122's
territory and not this ADR's to take. The signal is delivered through
`/assumptions` and the ADR-0122 evidence API, and folded into the
verdict chain only in a follow-up ADR that owns that change
explicitly.

`internal/coverage`: add a scenario asserting the admission check
rejects a policy that would cut quorum, and that a `true` verdict is
never produced from an absent observation - the honest-unknown rule as
an executable assertion rather than a comment.

Frontend: a Firewall page per Comb showing desired vs observed with
the differing rule named, generation, `known_good_generation`, a live
countdown when pending, the last three drift-and-repair events, the
IPv6 posture, and a prerequisite banner when pf is not enabled on that
host. The `default_deny` toggle is disabled with the server-side
verdict rendered inline, so the operator learns what the check will
say before clicking.

## Test plan

### Validatable on macOS (no FreeBSD required)

- `RenderRules` determinism: identical rule slices always produce
  byte-identical output, and therefore identical digests. A property
  test over shuffled equal-priority rules pins ADR-0075's stable-sort
  guarantee, since a sort instability here would present as
  phantom drift on every Comb.
- The renderer's new clauses: `on`/`from`/`to` appear only when the
  field is non-empty; the empty case is byte-identical to today's
  output (regression-pinned against a golden file so the per-VM path
  provably does not change).
- Fail-closed rejection: a rule with a field the renderer cannot
  express returns an error, never a partial render.
- Port-range validation, including the existing `"8000-9000"` →
  `"8000:9000"` cases and the tcp/udp-only restriction.
- The admissibility checker, over synthetic Colony graphs: a policy
  that cuts a voter-to-voter edge is rejected; one that leaves a
  single path is accepted; a `default_deny` policy with no management
  `pass` is rejected; a typo'd interface is rejected against a
  synthetic interface list.
- Generation logic: apply is a no-op at or below `applied_generation`;
  rollback never decrements `generation`; a lagging Comb re-applies the
  restored ruleset rather than refusing it.
- FSM plumbing: create → update → history retention (exactly one
  previous generation) → snapshot export/import → delete, all
  against
  a fake `pfctl` on `PATH`.
- The verdict table, as a table-driven test over the fake `pfctl`:
  missing binary → `unknown`; pf off → `false`; anchor absent →
  `false`; stderr → `false` with clamped detail; no tick → `unknown`
  with stale `LastObservedAt`. The load-bearing assertion is that no
  path produces `false` from an absent observation.
- The three-failure escalation and the drift-repair-count window.

### Only validatable on brood (10.90.0.94) or drone (10.90.0.95)

macOS cannot establish any of the following. PF, HAST, ZFS, jail,
bhyve, and real-network timing are all outside what a macOS test can
claim, and a test run on a laptop that passes is evidence about string
formatting and nothing else.

- **The actual host layout.** Where `anchor "apiary/*"` really sits in
  `/etc/pf.conf` on a real Combs, and therefore whether the Context
  hazard is live today. Nothing about this is inferable from the code.
- **The lockout rehearsal, on the testbed, never on a laptop.** Stage
  a policy that blocks the management path on a *non-voter* test Comb
  first. Verify: the staged apply blocks SSH and the frontend; the
  Combs' own probes go red; the deadline fires; the anchor is restored
  from local `known-good.rules`; the probes go green again; and the
  journal shows the full sequence. Only then repeat on a **voter**,
  with a console on the bench, to prove the replication-severance path.
  This is the single test that justifies the whole design, and it must
  be performed deliberately, by hand, with a person able to reach the
  console - never as part of an automated suite.
- **IPv4/IPv6 split.** That `INET` and `INET6` really are independent
  rulesets, that a v4-only policy leaves v6 open exactly as the
  `v6_unenforced` verdict claims, and that the check fires on a real
  interface with a global v6 address.
- **Real `pfctl` semantics.** `pfctl -a <anchor> -n -f -` really
  parse-checks without loading; `pfctl -a <anchor> -sr` output is
  stable enough to canonicalise across the FreeBSD 16.0-CURRENT
  version in use; reloading an anchor with a changed ruleset really
  does flush matching state as the Context claims.
- **Anchor load behaviour under real load** and interaction with
  `ApplyNAT`'s existing `apiary/net-*` anchors and the per-VM
  `apiary/vm-*` anchors - specifically that adding a host-scope anchor
  does not disturb the other two.
- **Timing.** Whether a 120-second deadline is long enough for a
  legitimate slow network to show green, and whether 15 seconds of red
  is long enough to avoid a false rollback on a transient blip. These
  two numbers are guesses until measured on real hardware, and a false
  rollback during a legitimate change is a worse operator experience
  than a slow apply.
- **Reboot behaviour** of a pending ruleset, confirming the
  unfiltered-after-reboot outcome claimed in Consequences is what
  actually happens.

## Open questions

1. **Reboot survival.** Should a pending or active host ruleset be
   checkpointed into `/etc/pf.conf` so it survives a reboot? Doing so
   means managing a host file this project has explicitly declined to
   manage (`internal/pf/exec.go`'s own doc). v1 accepts the unfiltered
   reboot. Is that acceptable, or is a reboot-safe variant a Phase 1
   requirement?
2. **Is the `anchor "apiary/*"` host-exposure hazard real on a current
   Combs?** The code says it could be. Only a real `/etc/pf.conf` on
   brood or drone settles it. If it is live, fixing it is arguably a
   bug fix that should ship before any host-scope policy feature,
   not inside it.
3. **Default-deny timing.** v1 ships host scope as
   additive-allow. When does it flip to `default_deny: true`, and is
   the gate "after the lockout rehearsal has been performed" or
   something stronger?
4. **Network objects.** ADR-0127 lists reusable IP/CIDR/group
   definitions referenced by rules. v1 uses literal CIDRs. Does the
   operator want named objects, and if so is that a separate ADR?
5. **Cell-scope policy composition.** v1 leaves Cells on
   `VMDefinition.firewall_rules`. If a Cell later needs rules that
   reference a named object or a Colony-wide baseline, `FirewallPolicy`
   needs a real `subject_id` path. Is that composition, or a
   replacement of the per-VM list?
6. **pflog.** Deferred. Is live packet logging a Phase 1 expectation
   from ADR-0127, or genuinely deferrable?
7. **Non-pf hosts.** The evidence says `not_applicable` for anything
   that is not pf. Is that honest enough for a mixed-fleet Colony, or
   should a non-pf Combs be a distinct, more alarming state than
   "no policy assigned"?
8. **Cell isolation claims.** ADR-0127's stated problem is "Cannot
   enforce Cell network isolation." v1's Cell-scope work adds address
   and interface matching to the existing per-VM rules, which is a
   real improvement - but is it the isolation ADR-0127 meant, or was
   that about jails/Cells intercommunicating (ADR-0117) rather than
   host-to-guest?
9. **The default deadline (120s) and red window (15s)** are guesses
   pending testbed measurement. Should they be per-policy operator
   settings, or fixed constants?

## References

- `internal/pf/rules.go`, `internal/pf/manager.go`,
  `internal/pf/exec.go` - the existing implementation this extends
- `internal/cluster/reconciler.go` - `pfManager`, `vmAnchor`,
  `natAnchor`, `toPFRules`, `effectivePFRules`, `r.PF.Apply`,
  `r.PF.Flush`, `r.PF.ApplyNAT`
- `api/internalpb/state.proto` - `FirewallRule` (line 284),
  `VMDefinition` (line 54), `SetVMFirewallRules` (line 597),
  `Command` (line 366)
- `api/rpc/manager.proto` - `service ManagerService` (line 11),
  `SetVMFirewallPaused` (line 90), `SetVMFirewallRules` (line 110),
  `message FirewallRule` (line 726), `AssumptionKind` (line 2515)
- `internal/manager/server.go` - `SetVMFirewallPaused` (line 1654),
  `SetVMFirewallRules` (line 1675)
- `internal/assumptions/manager.go` - `Status`, `Key`, `Result`,
  `HistoryEntry`, `ClampDetail`, `MaxDetailLen`
- `internal/assumecheck/checker.go` - `checkNATUplink` (line 286)
- `internal/raft/fsm.go`, `internal/raft/node.go` - the replicated
  state model and the leader-gated/ungated read split
- [ADR-0022](0022-network-management.md) - network management; the
  origin of the per-VM anchor and the `apiary/*` reservation
- [ADR-0075](0075-firewall-rule-priority.md) - rule priority and
  pf's last-match-wins evaluation
- [ADR-0088](0088-pause-nat-on-uplink-down.md) and
  [ADR-0114](0114-remove-uplink-takedown.md) - the removed uplink
  toggle; the precedent for refusing to rely on a confirm dialog
- [ADR-0122](0122-cluster-evidence-aware-health-api.md) - the
  cluster-wide evidence API this feeds
- [ADR-0127](0127-sylve-io-features.md) section 4 and Phase 1 - the
  originating description, and the two items this ADR defers
  (network objects, pflog)
- `SHARED.md` - Apiary architecture, pillars, and the update-lock
  protocol
