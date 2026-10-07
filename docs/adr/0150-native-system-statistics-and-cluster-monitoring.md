# ADR-0150: Native system-statistics and cluster-monitoring API

Status: Unconfirmed
Supersedes: "Todo: Integrate bsnmpd with raftd to reduce state tracking" (Notion Todo, Apiary project — not an apiary-local ADR, so there is nothing in `docs/adr/` to mark superseded; this document records the supersession relationship on this side only. Resolving or re-numbering the Notion page is Glen's to do, not this document's.)
Superseded by: None
Affected projects: Apiary
Legacy identifier: ADR-0150 (Apiary)
Loreloom identifier: Pending allocation by Glen — if this design is ever registered cross-project, allocating that number is Glen's to do, not this agent's. This document deliberately does not invent one.

## Status qualifications

**UNCONFIRMED. Not accepted, not implemented, not reviewed.**

This ADR is a proposal, written in response to a Notion Todo ("Design
native Apiary system-statistics and cluster-monitoring capability,
exposed through an API...") that itself superseded an earlier,
cancelled Todo ("Integrate bsnmpd with raftd to reduce state
tracking"). That earlier Todo's three design goals are carried forward
here, verbatim in intent:

- Keep what `raftd` transmits minimal; Apiary reports its own state
  back rather than `raftd` tracking it.
- Two-way monitoring: the leader monitors followers, and followers
  report state back to the leader (similar in spirit to Nagios).
- The monitoring layer must not require a systems administrator to
  hand-configure a file; configuration is managed by Apiary itself.

The task that produced this document was explicit that three things
are **not yet decided**, and this document does not pretend otherwise:

- the wire protocol or data model for the native monitoring layer,
- whether it still speaks SNMP externally or is purely internal,
- how it maps onto `raftd`'s existing message shapes.

Consistent with that, **no code changes accompany this ADR.** This is
a design document only: the research, the options, a recommendation,
and a list of the genuinely open questions for Glen to decide before
any implementation begins. Section "Open questions" below is where
those live; nothing stated as settled anywhere above that section
should be read as more final than the status line says.

## Context

### What exists today, and what the cancelled predecessor left behind

A repository-wide search for `bsnmpd` and `snmp` (case-insensitive,
`*.go` and `*.md`) turns up exactly one hit, in `docs/minimal-build.md`,
listing `FreeBSD-bsnmp`, `FreeBSD-libbegemot`, `FreeBSD-libbsdstat`, and
`FreeBSD-librss` as packages excluded from Apiary's minimal base-system
build. There is no bsnmpd integration code, no SNMP MIB, no `net-snmp`
client, and no prior design document anywhere in this tree. The
cancelled Todo's own "prior investigation", if any happened, left no
trace in the repository — this ADR treats that investigation as
effectively absent and starts from the current architecture instead.

### How raft currently tracks and transmits state (`internal/raft`)

Apiary's replicated state is a log of *decisions* applied by `FSM.Apply`
on every voter (`internal/raft/fsm.go`), via a typed `Command` oneof
carried in `ApplyRequest.payload` (`api/internalpb/raftd.proto`,
`api/internalpb/state.proto`). Every replica computes identical results
from the identical committed log — that property is load-bearing
throughout the FSM (IP/MAC allocation in `applyCreateVM`, the
never-decreases rule in `applySetColonyDiskSize`, etc.), and ADR-0149's
"what already replicates, and what does not" section draws the same
line this proposal has to respect: ephemeral, small, JSON-shaped
coordination facts go through raft; physical, per-node, potentially
large or high-churn data (ZFS datasets, HAST-replicated bytes, and —
by the same reasoning — raw CPU/memory/disk counters) does not.

`Node.Status()` (`internal/raft/node.go`) already reports a
*monitoring-shaped* fact about raft itself: `IsLeader`, `LeaderID`,
`NodeID`, `LastLogIndex`, `AppliedIndex`, `RaftState`, `Servers`
(voter/nonvoter list), `MembershipError`, and — since ADR-0143 — a
canonical `StateDigest` (a lowercase hex SHA-256 of FSM state,
recomputed on every `Apply`, deliberately *not* folding in
`AppliedIndex` so that a snapshot taken mid-replication isn't
misread as divergence). This is the one piece of "cluster health"
state that already flows leader-to-follower-and-back implicitly: every
node computes its own `StateDigest` from its own locally-replayed log,
and a consumer (today, `ClusterHealth`) compares digests across nodes
collected by separate, sequential, non-atomic RPCs — there is no
raft-native push of one node's digest to another.

Crucially, raft's own heartbeat/AppendEntries traffic (hashicorp/raft,
via `raft.DefaultConfig()` in `Node.New`) is **not surfaced** anywhere
in this codebase today, and hashicorp/raft does not expose an
application-level payload hook on its heartbeats — a leader's
heartbeat to a follower carries no room for Apiary-defined fields
without forking the library or layering a second transport. This
matters directly for "whether raft's own mechanism can carry it" in
the Decision section below.

### How the manager already exposes read-only, per-node monitoring (`internal/manager`, `api/rpc`)

`ManagerService` (`api/rpc/manager.proto`, implemented in
`internal/manager/server.go`) already has the closest thing to a
"system statistics" API Apiary has ever built, and it is the right
foundation to extend rather than replace:

- **`HostStats`** (`HostStatsRequest`/`HostStatsResponse`) reports
  *this* node's own CPU (`CPUStats`: cores, three load averages),
  memory (`MemStats`), ZFS pool stats (`PoolStats`, per pool: size,
  alloc, free, capacity %, health), disk SMART health (`DiskStats`),
  network interface counters (`NetIfaceStats`: cumulative rx/tx bytes,
  up/down), pf(8) status (`PFStats`), and a growing set of
  "proves configured, not proves working" booleans and reconciler
  timestamps (`bhyve_configured`, `last_reconcile_success_unix`,
  `last_reconcile_error`, etc.). It is explicitly documented as
  physical, per-node, observational data: gathered locally
  (`internal/hoststats`), never routed through raft, never
  leader-forwarded. A caller wanting the whole cluster's stats fans
  out to every node's managerd itself (as `internal/frontend`'s
  `peerHostStatsClient` already does for the web UI's Combs page).
- **`ClusterHealth`** (`ClusterHealthRequest`/`ClusterHealthResponse`)
  is the one RPC that already answers "the whole cluster's state" in
  one call. It computes, server-side (ADR-0122, moved out of the web
  UI per ADR-0056's original design), an Evidence-Aware Health verdict
  per Comb (`ClusterNodeHealth`: `status` ∈
  {healthy, degraded, unknown, stale, contradictory}, `explanation`,
  `observations`, `dialed`, `raft_state_digest`,
  `raft_applied_index`). It is explicitly documented as *not* an
  atomic snapshot: each node's `HostStats`/`Status` are gathered by
  separate, sequential reads, so different rows can reflect slightly
  different moments. There is no leader concept for this RPC — "it
  answers for whoever receives it", and every node's evidence is
  gathered independently (i.e., answering-node-fans-out-itself, not a
  leader-mediated push).
- **`GetLocalNodeHealth`** is `ClusterHealth`'s single-node,
  no-fan-out sibling, combined with the node-local, non-replicated
  Assumption Register (`internal/assumecheck`, ADR-0055) — a periodic
  per-node pass producing `Result`s for continuously re-evaluated,
  system-observed checks (bridge membership, default-route presence,
  uplink/NAT consistency, peer HostStats reachability), persisted
  locally and never raft-replicated.

None of this is "two-way" in the sense the cancelled bsnmpd Todo meant:
every existing read is *pull*, not *push*. A caller (the web UI, or a
future monitoring client) asks each node in turn; no node today
proactively reports its own state to a leader or to any other node
without being asked. That gap — no leader-initiated poll of followers,
no follower-initiated push to a leader — is exactly what this proposal
has to fill in without inventing a second consensus mechanism to do
it.

### How the frontend surfaces this today (`internal/frontend`)

`internal/frontend/cluster_overview.go` is the Combs page: it fans out
`HostStats` (via `peerHostStatsClient`) to every node named in raft
membership, in parallel, with per-node timeouts, and renders the
result alongside `ClusterHealth`'s verdict and `statedigest`'s compare
view. `internal/frontend/why_not.go` and `invariants_hast_test.go`
back the "why can't I do X" and HAST-replica-freshness invariant pages
(`internal/invariant`, `internal/whynot`), which translate evidence
("last observed at", "never observed") into operator-facing blockers
and remedies — the same evidence vocabulary (`Evidence.Source`,
`.Detail`, `.ObservedAt`, with `ObservedAt.IsZero()` meaning "never
observed" rather than "observed as empty") that `ClusterHealth` and
`HostStats` already use. This is the UI-side proof that "system
statistics" in Apiary already means exactly this family of structures:
per-node observations, each carrying its own freshness, assembled
client-side into a cluster-wide picture, never assumed atomic.

### Authorization model (`internal/manager/auth.go`, `internal/assumecheck/checker.go`)

Apiary's external API auth is a three-tier role hierarchy — Viewer <
Operator < Admin (ADR-0030) — checked by `checkAuth`
(`AuthUnaryInterceptor`) against a `requiredRole` map keyed by full
gRPC method name, with every RPC *absent* from the map defaulting to
Admin-only rather than open (a deliberate fail-closed default enforced
by `TestRequiredRole_CoversEveryRPC`). `HostStats`, `ClusterHealth`,
`GetLocalNodeHealth`, `ListAssumptionResults`,
`GetLocalNetworkBridgeStatus`, `ListNodeServices`, and `HostPackages`
are all already mapped to `RoleViewer` — i.e., Apiary's existing
answer to "who may poll statistics across multiple machines" is
already "any Viewer-tier API key, checked per-RPC, same as every other
read". Credentials are `ApiKey` records (`hashed_key`: SHA-256 hex,
raft-replicated so every node validates identically — see
`Node.ValidateAPIKeyHash`'s own doc comment, explicitly *not*
leader-restricted, for the same reason monitoring reads need to work
on any node) with the raw key shown exactly once at creation
(`CreateAPIKey`). Transport security is TLS end-to-end: managerd's
external gRPC listener, raftd's internal `RaftInternal` Unix-socket
RPC (not network-exposed at all, per `api/internalpb/raftd.proto`),
and — separately — raft's own peer-to-peer transport, which can be
configured with its own TLS (`internal/raft/tls_transport.go`,
`internal/raft/auth.go`) and a replicated peer-trust store
(`TrustedPeer`, ADR-0147 Part 4) feeding a derived `peer-ca.pem` on
every Comb.

A handful of especially dangerous RPCs (the restart-guardrail family:
`ReserveRestartLease`, `StepAsideForRestart`, `MutateColonyUpdate`,
`ExecuteNodeRestartPlan`, `RequestManagerdRestart`,
`IssueManagerdRestart`) are deliberately carved *out* of the
Viewer/Operator/Admin hierarchy entirely and gated instead by a
separate root-owned local token, specifically because no
`CreateAPIKey`-issued credential, however privileged, should be able
to orchestrate a cluster-wide restart. Monitoring/statistics reads are
not in that category — there is nothing in this design that argues for
anything stronger than the existing Viewer-tier API-key model, and the
rest of this document assumes that is also the right answer for the
new surface (see Decision, "What 'secure channel' means here").

## Decision — proposed, not yet accepted

### 1. API surface: extend `ManagerService`, do not create a parallel one

Add a `ClusterStatistics`-shaped read next to `ClusterHealth`, not a
new service. Concretely (illustrative field shapes — the exact proto
is explicitly **not** fixed by this ADR; see Open questions):

- `rpc GetNodeMonitoringReport(GetNodeMonitoringReportRequest) returns (NodeMonitoringReport)` —
  the single-node, no-fan-out report: a superset of what `HostStats`
  already carries (CPU/mem/disk/net/pf/pool) *plus* the two new
  signals this ADR is actually about: (a) this node's own view of
  every *other* node's last-observed state (see §2), and (b) the
  reporting node's own raft role/term/digest, reusing
  `internal/raft.Node.Status()` verbatim rather than re-deriving it.
  Viewer-tier, node-local, never leader-forwarded — same posture as
  `HostStats`/`GetLocalNodeHealth` today.
- `rpc GetClusterMonitoringReport(GetClusterMonitoringReportRequest) returns (ClusterMonitoringReport)` —
  the fan-out convenience RPC, structurally identical to
  `ClusterHealth` today (same non-atomic, sequential-per-node,
  "answers for whoever receives it" posture), but returning the richer
  per-node report instead of only the health verdict. This is very
  likely just `ClusterHealth` with its `ClusterNodeHealth` message
  extended to embed `NodeMonitoringReport`, rather than a genuinely new
  RPC — see Open questions on whether to extend `ClusterHealth` in
  place (versioned additively) or add a sibling RPC.

Both reuse the existing Viewer role gate and the existing API-key auth
model unchanged (`requiredRole` map entries at `RoleViewer`, same as
`HostStats`/`ClusterHealth` today — see §4 below for why no new auth
primitive is proposed).

This explicitly rejects building a standalone monitoring daemon,
dashboard service, or separate RPC port: Apiary already has exactly
one authenticated, TLS-protected external surface
(`ManagerService`/managerd), and every other read-only report in this
codebase (HostStats, ClusterHealth, HostPackages, ListNodeServices,
ListAssumptionResults) lives there. A second surface would duplicate
TLS termination, auth, and the fan-out machinery `internal/frontend`
and `internal/assumecheck` already share via `PeerReporter`-shaped
interfaces, for no offsetting benefit.

### 2. What the two-way leader/follower reporting actually carries

This is the part of the cancelled Todo that genuinely has no existing
analog, so it is where this document is least settled, and
deliberately drafts a mechanism rather than a final wire format.

**Recommendation: build it as a new, narrow raft `Command`, not as a
side channel on raft's heartbeat, and not as a second gossip
protocol.**

Reasoning:

- hashicorp/raft's heartbeat/AppendEntries RPC carries no
  application-level payload slot (confirmed against the version this
  tree vendors — see Context above), so "piggyback on raft's existing
  heartbeat" is not actually available without forking the dependency.
  That rules out the most literal reading of "reuses raft's existing
  heartbeat/state-replication machinery" directly on the heartbeat
  itself.
- What *is* available, and is exactly the mechanism ADR-0103
  (restart lease), ADR-0145 (colony-wide update single-flight), and
  ADR-0147 Part 4 (join window, peer trust store) already use for
  "a fact that must survive a managerd restart and be visible on every
  Comb regardless of who's leading": a small, typed `Command` applied
  through the existing `Apply` path, landing in the same replicated
  FSM map every other ephemeral-state command does. That *is* "raft's
  own state-replication machinery" in every sense that matters for
  this codebase — the log, the apply-identically-everywhere guarantee,
  the survive-a-restart guarantee — even though it does not literally
  ride the heartbeat wire format.
- A proposed `ReportNodeVitals` command, submitted periodically by each
  node's own managerd against its own raftd's `Apply` (not against the
  leader directly — `Apply` already only succeeds against the current
  leader and returns a `leader_hint` otherwise, exactly like every
  other write), carrying a **small, bounded, summary-shaped** payload:
  node_id, observed_at_unix, a coarse health enum (reusing
  `internal/health`'s existing five-state vocabulary:
  healthy/degraded/unknown/stale/contradictory — never inventing a
  second one), and a short fixed set of scalar gauges (load average,
  memory free %, worst pool capacity %, worst disk health) — explicitly
  **not** the full `HostStatsResponse` (SMART details, per-interface
  counters, pf state counts), which stays a pull-only, non-replicated,
  per-node read exactly as it is today.
- This keeps "what raftd transmits" minimal, per the cancelled Todo's
  first carried-forward goal: the summary is small and bounded, one
  entry per node, replacing the previous entry each time (last-write
  wins by node_id, mirroring how `ColonyDiskSize`/`ColonyJoinWindow`
  each hold exactly one current value) — not an unbounded append-only
  journal of every poll.
- "The leader monitors followers, and followers report state back to
  the leader" becomes, concretely: every node (leader included, for
  uniformity) periodically submits its own `ReportNodeVitals` via
  `Apply`; every node (because the resulting map is raft-replicated,
  not leader-held) can read every other node's last-reported vitals
  from its own local FSM copy, the same `*Local`-suffixed,
  no-leader-restriction pattern `ListVMsLocal`/`ColonyUpdateStateLocal`
  already establish for "already-replicated, so any node can answer
  for itself". There is no literal "leader polls follower" RPC in this
  shape — the leader does not need to reach out, because every node's
  self-report already lands in replicated state that the leader (like
  everyone else) can read locally. If Glen's intent is specifically a
  leader-initiated pull (not merely a replicated self-report every node
  can see), that is a materially different design and is flagged as an
  open question below rather than assumed.
- Staleness is the signal that "a follower has stopped reporting" is
  visible without the leader needing to detect it actively:
  `observed_at_unix` on each entry is compared against a local
  interval, exactly as `last_reconcile_success_unix`/
  `reconcile_interval_seconds` already let `internal/health` call a
  stale reconciler "stale" rather than silently "healthy" or
  "unknown". A node that has stopped submitting `ReportNodeVitals`
  shows up as stale from every other node's point of view within one
  interval, with no additional detection logic.

This is explicitly **not** a final data model — only a shape argued to
be consistent with the project's existing replication conventions. The
exact field list, submission interval, and whether `ReportNodeVitals`
should instead be unreplicated-and-gossiped (peer-to-peer, HostStats-
style pull fan-out, never touching raft at all) are open questions
below.

### 3. What "secure channel" means here

**Recommendation: reuse the existing model unchanged — no new auth
primitive.** Concretely:

- External API access (the two new RPCs in §1): the existing
  Viewer-tier, `CreateAPIKey`-issued, SHA-256-hashed, raft-replicated
  API key model (ADR-0023/ADR-0030), checked by the existing
  `checkAuth`/`requiredRole` map, over the existing TLS-terminated
  managerd gRPC listener. This is already apiary's answer for
  "authorized users... over a secure channel" for every other
  statistics-shaped read (`HostStats`, `ClusterHealth`,
  `HostPackages`); nothing about monitoring is more or less sensitive
  than those, so nothing argues for a different tier or a separate
  credential type. It is explicitly **not** in the restart-guardrail
  token's threat category (§ Context, Authorization model) — reading
  statistics cannot orchestrate a restart, so the dedicated root-owned
  token model that family uses would be the wrong fit, over-restrictive
  for an ordinary Viewer-tier read.
- Internal node-to-node traffic (the `ReportNodeVitals` `Apply` calls
  and any peer reads of other nodes' vitals): raft's own existing
  transport security (`internal/raft/tls_transport.go`,
  `internal/raft/auth.go`, the replicated `TrustedPeer` store feeding
  `peer-ca.pem`). This is already how every other raft-replicated
  command travels; `ReportNodeVitals` gets no special treatment and
  needs none.
- No new credential type, no new token, no new transport. The channel
  is "secure" in exactly the sense every other read in this codebase
  already is: TLS in transit, an authenticated, role-checked caller at
  the API boundary, and raft's own peer-TLS/trust-store internally.
  This document does not identify anything about multi-machine
  statistics polling that raises the bar above what `ClusterHealth`
  already clears today.

### 4. The SNMP-external question

**Recommendation: go fully internal/proprietary. Do not keep
bsnmpd-compatible external polling, not even as an optional adapter —
at least not in this pass, and only reconsider it if a concrete
external consumer (e.g., an existing Nagios/Zabbix/Prometheus
deployment Glen already runs) is named.**

Reasoning:

- There is no existing SNMP code in this tree to build on (Context,
  above) — the cancelled Todo's own framing ("replacing bsnmpd") was
  never implemented, so "keep bsnmpd-compatible" would mean building a
  SNMP agent and a MIB from scratch, not preserving anything that
  exists today.
- Every other "statistics" surface in Apiary (`HostStats`,
  `ClusterHealth`, `HostPackages`) is already a typed, authenticated
  gRPC read with no SNMP equivalent anywhere in the system, and the web
  UI and `internal/assumecheck` already consume them that way. Adding
  a second, differently-shaped, differently-authenticated protocol
  (SNMP has its own, much weaker, community-string-style auth model
  that would sit uneasily next to Apiary's API-key/TLS model) for only
  the monitoring surface would make monitoring the one feature with a
  parallel, SNMP-flavored security and data model next to everything
  else's gRPC-flavored one.
- A bsnmpd *adapter* — translating gRPC responses into MIB objects for
  an existing external NMS — is a real, legitimate future option, and
  nothing in this recommendation forecloses it permanently. But it is
  additive scope on top of a design that does not exist yet, best
  evaluated once there is a concrete external consumer naming specific
  OIDs it needs, not sketched speculatively now.
- Apiary's own stance throughout this codebase (CLAUDE.md's
  physical/ephemeral split, the explicit "no automatic scheduling"
  posture in `MigrateVM`'s own doc comment, ADR-0049's rejection of
  convenience fields that risk silent data loss) consistently favors a
  smaller, explicit, internally-consistent surface over a
  compatibility shim with an older ecosystem tool — the same instinct
  applies here.

## Consequences

- No code, proto, or FSM command changes accompany this ADR. Nothing
  in the tree is affected by its acceptance alone.
- If accepted as directionally correct, the next real step is a
  follow-up ADR (or a revision of this one) that actually fixes the
  wire format, answering the open questions below, before any
  `Command` variant, RPC, or FSM apply function is written.
- Accepting the §4 recommendation means no SNMP compatibility work is
  scheduled; a future request for SNMP-external polling would need its
  own justification (a named external consumer) rather than reopening
  this ADR's recommendation by default.

## What this does not claim

- This document does not claim raft's heartbeat can carry an
  application payload — it explicitly cannot, with this tree's vendored
  hashicorp/raft, and the recommendation routes around that rather than
  assuming otherwise.
- This document does not claim "leader monitors followers" is fully
  satisfied by a replicated self-report every node can read locally —
  that is this document's best-effort translation of the requirement
  into mechanisms Apiary already has, not a confirmed match to what
  Glen meant by "the leader monitors followers". See Open questions.
- This document does not claim Viewer-tier API-key auth is definitely
  sufficient for whatever Glen intends "authorized users" to mean here
  — only that nothing in the existing codebase's own precedent argues
  for anything stronger, and that the restart-guardrail token model
  is visibly the wrong fit for a read.

## Open questions

These are the questions this document could not resolve on its own,
and they should block any implementation, not just this ADR's
acceptance:

1. **Push vs. pull, precisely.** Is "the leader monitors followers"
   satisfied by every node submitting a replicated self-report that
   anyone (leader included) can read locally (§2's recommendation), or
   does Glen specifically want the leader to actively dial each
   follower (mirroring `ClusterHealth`'s existing fan-out, but
   leader-only and periodic rather than caller-triggered)? These are
   materially different failure modes: a replicated self-report
   detects "this node stopped talking" from everyone's point of view
   symmetrically; a leader-initiated poll detects it only from the
   leader's point of view, and needs its own handoff story across an
   election.
2. **Should `ReportNodeVitals` go through raft `Apply` at all, or
   should node-vitals exchange be a new, unreplicated peer-to-peer RPC
   (HostStats-style pull, fanned out by whoever asks, never touching
   the raft log)?** §2 recommends the raft-replicated route for
   symmetry with ADR-0103/0145/0147's precedent, but that imposes
   write-throughput cost on the raft log (even a small, bounded,
   last-write-wins entry still has to go through consensus on every
   submission) that a pure pull-only model (like `HostStats` today)
   would avoid entirely. Which failure mode and which cost Glen
   prefers is not something this document can decide.
3. **What exactly belongs in the small summary payload?** §2 proposes
   a short fixed list (coarse health enum + four scalar gauges). The
   actual field list — and whether it should be versioned/extensible
   from day one rather than fixed — is undecided.
4. **Submission/poll interval**, and whether it should be configurable
   per-Colony (like `reconcile_interval_seconds` already is per-node)
   or fixed.
5. **Extend `ClusterHealth` in place, or add a sibling RPC?** §1
   suggests extending `ClusterNodeHealth`/`ClusterHealthResponse`
   additively might be simpler than a new RPC, but that changes an
   existing, already-shipped wire contract rather than adding a new
   one — which carries its own compatibility considerations this
   document has not worked through.
6. **Retention of history.** `HostStats`/`ClusterHealth` are
   point-in-time only today; the cancelled Todo's "Nagios-like" framing
   implies trend data (did this node's load spike an hour ago?) that
   none of this proposal's shapes capture. Whether native monitoring
   is meant to include any time-series retention at all, or stays
   point-in-time like everything else in this codebase, is undecided
   and would significantly change the design if the answer is yes
   (raft-replicated state is a poor fit for a time series of any
   length).
7. **Alerting/notification.** The Todo asks for "monitoring", and
   Nagios implies thresholds and alerts, not just a readable snapshot.
   Whether that is in scope for this capability at all, or is
   explicitly deferred to a separate future design, is undecided.
8. **The bsnmpd-adapter door left open in §4** — if Glen already has,
   or plans to have, an external NMS that expects SNMP, that changes
   the recommendation; this document assumes no such consumer exists
   today because none is named in the Notion task.
