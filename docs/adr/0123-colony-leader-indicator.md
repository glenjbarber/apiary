# ADR-0123: Colony Leader Indicator

## Status

Accepted

## Context

An operator looking at any Apiary page has no way to see which Comb is
currently the Colony's Raft leader. That fact decides where a write will be
accepted, so it is the first question during almost any incident: if the
leader is unreachable, or leadership is in flux, "the UI is not updating" and
"the leader is gone" are very different problems with very different fixes.

Two facts constrain the design, and both were established from the code
rather than assumed:

1. **Raft does not persist the current leader.** HashiCorp Raft stores the
   current term, the log, and snapshots, but never the leader. The read-only
   offline status path added for `raftd -status` (see
   `internal/raft/offline.go`) therefore reports leadership as explicitly
   unrecoverable, and says so in its own output. Leadership can only come from
   a **live** source.
2. **An empty leader ID does not mean "no leader".** `Raft.LeaderWithID`'s
   own doc comment in hashicorp/raft v1.8.0 reads: *"It may return empty
   strings if there is no current leader or the leader is unknown."* A follower
   that has lost contact with the leader, and a cluster that genuinely has no
   leader, are indistinguishable from that one field. Any indicator built on
   `leader_id` alone would therefore be unable to tell "nobody is in charge"
   from "I cannot see who is in charge" — which is precisely the
   pass-by-default failure ADR-0056 exists to eliminate, in a new place.

`managerd`'s `Status` RPC already carries everything needed: `raft_reachable`,
`raft_is_leader`, `raft_leader_id`, `raft_node_id`, and `raft_state`, read live
from `raftd` over its internal socket. So this feature adds no RPC, no proto
field, and no new observation path. It renders evidence that already exists and
has so far gone unused by the UI.

## Decision

Add a **Colony leader indicator to the shared application header**, present on
every page, derived from exactly one live `managerd.Status` call per render.

### The five states, and why the split matters

| state | meaning | how it is reached |
| --- | --- | --- |
| `known` | a specific leader was observed | `raft_is_leader`, or a non-empty `raft_leader_id` |
| `unknown` | **no observation was possible** | the `Status` RPC failed, or `raft_reachable` is false |
| `none` | a real, observed absence of any leader | raft read successfully, not leader, `raft_state == "Follower"`, empty leader ID |
| `electing` | a leader is being chosen right now | `raft_state == "Candidate"` |
| `shutdown` | this Comb's raftd is down | `raft_state == "Shutdown"` |

`unknown` and `none` are the two that matter. Collapsing them would let a page
assert "the Colony has no leader" at exactly the moment the page has lost its
ability to find out — turning a diagnosable "raftd is not answering" into a
false alarm. `raft_state` is what separates them, because it is the only
field that distinguishes an unsettled reading from a settled one.

Every non-`known` state carries a `Detail` explaining itself, including
managerd's own `raft_error` verbatim, so an operator can act on the indicator
rather than merely observe it. An unrecognized `raft_state` — a newer raftd
could report a state this build has never seen — is **named verbatim** and
treated as `unknown`, never collapsed into a verdict, per ADR-0056's rule.

### One node's view, labelled as such

The reading comes from **this Comb's own raftd**, not from a cluster-wide
agreement. A follower that has lost the leader can still report the previous
one, so the indicator always names the node that produced the reading
("read by brood.lab3.home.arpa"). It is a point-in-time observation, and
leadership can change on the next election — measured at ~2.8s on this cluster
when brood's raftd was stopped and drone won term 27 — so the page shows the
observation time and says so in the tooltip.

Consulting every node and cross-checking the leader would be the stronger
answer, but it would put a four-node fan-out behind every page render to
answer a question a follower can usually answer. That is the wrong trade for
a header indicator, and `ClusterHealth` already exists (ADR-0122) for an
operator who wants the expensive, evidence-backed version.

### The Combs list

The overview page's Comb cards additionally mark the leader's own card, derived
from the **same single anchor `StatusResponse` every row already shares** — no
extra RPC. When leadership is unknown, electing, or genuinely absent, **no card
is marked**; the list never implies a leader that was not observed.

### Cost, measured

`withAuthFields` is the single funnel every full-page render passes through, so
the indicator is present everywhere without each handler opting in. The call is
deliberately **never cached**: a cached leader would be a leader that changed
up to a cache-interval ago, which is the opposite of what this feature is for.

Measured `Status` RPC counts per page render, with and without the indicator:

| page | before | after |
| --- | --- | --- |
| `/` | 1 | 1 (reuses the page's own `StatusResponse`) |
| `/assumptions` | 1 | 1 (reuses) |
| `/vms`, `/images`, `/jails`, `/networks` | 1–2 | +1 |
| `/simulate`, `/invariants`, `/why-not`, `/recovery-handbook` | 2–3 | +1 |
| `/apikeys`, `/users`, `/trace` | 0 | 1 |

So the indicator costs **zero** extra calls on the two pages that already hold
a `StatusResponse` for membership (via `withAuthFieldsFrom`) and exactly one
local gRPC round trip elsewhere. One extra local read is accepted deliberately:
it is a single call to the colocated managerd, not the ~2.2s four-node fan-out
`ClusterHealth` performs. `TestColonyLeaderIndicatorStatusCallCost` pins these
numbers so the cost cannot quietly grow.

**Fragments are excluded.** Four htmx fragment renders (`iso_rows`,
`iso_panel`, `jail_panel`, `network_panel`) also funnel through
`withAuthFields`, but none of them includes the shared header partial, so
nothing in them can ever display the indicator. They use
`withAuthFieldsForFragment` instead, which fills in the session/role fields
only. Without that split they would each pay a `Status` RPC per swap for a
value silently thrown away — invisible in a latency profile and pure waste in
a list that a user can click through quickly. The split is a named function
rather than a boolean so the choice is visible at the call site.

## Consequences

- **No new RPC, no new proto field, no new observation path.** The evidence
  path is `Status` → `raftd` → `raft.Node.Status()`, all pre-existing.
- **The UI now distinguishes "no leader" from "cannot tell."** This is the
  substantive contribution, and it is available everywhere rather than only on
  a diagnostics page.
- **A leader ID on screen is one node's report, not a quorum fact.** Labelled
  as such. It should not be read as proof that the leader is reachable or that
  writes will be accepted.
- **The offline `raftd -status` path is untouched** and still cannot report
  leadership, which remains correct: leadership is not on disk.
- **One extra local RPC per page render** on most pages, by choice. Revisit if
  a page render is ever latency-sensitive enough to care.
- **Not covered:** whether the leader is actually reachable, whether it is
  keeping up, or whether writes would be accepted right now. Those are
  `ClusterHealth`'s questions (ADR-0122), not this indicator's.

## Alternatives considered

**Extend the offline `raftd -status` reader to report leadership.** Rejected:
leadership is not persisted, so there is nothing to read. This is a property of
Raft, not an omission in the reader, and the reader already says so.

**Add a `leader_id` field to `ClusterHealthResponse`.** Rejected as redundant
for a header: it would couple the expensive cluster-wide RPC to a fact every
page needs cheaply, and every caller of `ClusterHealth` would then have to
decide what to do with a field that is really one node's opinion.

**Cache the leader for a few seconds to save the RPC.** Rejected. It trades a
guaranteed-current fact for a marginally cheaper page, and it would require
carrying an age into the UI to stay honest. The opposite trade is right here.

**Infer the leader from the highest applied index.** Rejected outright: index
order is not leadership, and this is exactly the "looks like a conclusion, is
actually a guess" pattern this codebase already rejects elsewhere.
