# ADR-0140: "pfctl reports enabled" is not evidence that anything is filtered

## Status

Accepted (decision). Implemented in `internal/pf` (`hostbaseline.go`) and
`internal/hoststats` (`stats.go`), with two new fields on
`api/rpc`'s `PFStats`. **The live confirmation was taken by hand on a
real four-node colony; nothing here changes a host.** See "Verification"
for exactly what was run and what was not.

## Context

Every Comb in the live colony reported PF "enabled" for as long as
anyone had been looking. `internal/hoststats`'s `PFInfo` doc comment
explained where that fact came from, and the explanation was the defect
in one sentence:

> PFInfo is a summary of pf(8)'s current status and counters, from
> `pfctl -s info` - confirming the firewall is actually enabled and
> doing something.

Read-only on a live four-node FreeBSD colony, that "actually enabled
and doing something" was one half of a true statement. The other half
was this. `/etc/pf.conf` on a Comb, in its entirety:

```
anchor "apiary/*"
```

One line. `pfctl -sr` printed exactly one rule, `anchor "apiary/*"
all`. The `apiary` anchor existed and contained no rules. There was no
`set block-policy`, no `block` rule anywhere, and no `set skip on lo`.

That is the whole measured posture, and it means **pf was filtering
nothing at all**: pf passes any packet that matches no rule, so on that
host there was no rule, in the main ruleset or in any anchor, that could
drop a packet. Every service the colony depends on - raftd on 17600,
managerd's peer RPC on 17700, the web UI on 8080, restshimd on 8081 -
was reachable from the LAN with no packet filtering in the way.

The exposure was confirmed by probe rather than inferred from the
absence of a rule: an unauthenticated
`GET http://<comb>:8081/v1/vms` returned **HTTP 200 from the LAN on all
four Combs**. That is the real risk this visibility exists to make
visible, and it is worth stating plainly: this was not a near-miss on
paper, it was a reachable unauthenticated VM inventory on every node in
the colony, while every node's own dashboard said the firewall was on.

Three separate things conspired to hide it, and each is worth naming
because each is a design decision somebody could repeat:

1. **`Status: Enabled` is a statement about pfctl, not about
   filtering.** pf loaded a ruleset. Whether that ruleset drops
   anything is a property of the ruleset's *contents*, which
   `pfctl -s info` does not report at all. The single read the codebase
   had was structurally incapable of answering the question the comment
   claimed it answered.
2. **A failed read rendered as the reassuring answer.** `parsePFInfo`
   sets `Enabled = len(fields) >= 2 && fields[1] == "Enabled"`, so
   pfctl missing, not permitted, slow, or printing a status word nobody
   recognised all produced `Enabled: false` - indistinguishable, in a
   bool, from a host with pf genuinely off, and considerably easier to
   read as "not a firewall problem".
3. **`internal/pf` was confined to `apiary/*` and never looked outside
   it.** That confinement is ADR-0137's deliberate, load-bearing design
   decision, not an oversight: every write in the tree is
   `pfctl -a <anchor> -f -` or `pfctl -a <anchor> -F rules`, and the
   only host file it touches is the one-line `anchor "apiary/*"` stanza
   `internal/install/checks.go` verifies. A package that correctly
   refuses to own somebody else's ruleset had consequently never formed
   an opinion about them - and so the drift check it *does* own, "is
   this anchor's text what pf is enforcing?", could be answered
   `InSync` on a host where pf was enforcing nothing whatsoever.

None of the three is a bug in the narrow sense. Each is a true fact
rendered as a claim it does not support, which is the failure mode this
whole codebase's honesty rules exist to prevent elsewhere
(`replica_unobserved` is silence, not health; `health unknown` is not
green; `MembershipUnknown` is not a pass; a failed load is not a
last-known-good).

## Decision

**"pfctl reports enabled" is not evidence that anything is filtered.
Apiary reports what the host's own ruleset can actually do, read-only,
with an explicit `unknown` - and never hardens the host automatically.**

The second half is not a caveat; it is the half that matters most.
Having found that four live Combs were filtering nothing, the
tempting move is to fix it: install `set block-policy drop`, or write
the rules into `/etc/pf.conf`. Both are rejected, and the reasoning is
in "Rejected alternatives" below. The short version: a firewall's main
ruleset is the host operator's, not ours, and the one useful thing this
finding can do for a live colony is *tell the operator*, not
silently reconfigure a running host out from under them.

### Four host states, three anchor states, and no way to collapse them

`internal/pf`'s host assessment is a four-valued verdict, `BaselineState`:

| state | meaning | `Filters()` |
|---|---|---|
| `BaselineFiltering` | the host's own ruleset carries a `set block-policy` **or** at least one `block` rule, so it *can* drop a packet | **true** |
| `BaselineUnfiltered` | pf is enabled, and nothing outside Apiary's anchors can drop a packet: no block policy, no block rule | false |
| `BaselineDisabled` | pf itself is not enabled; nothing is evaluated, Apiary's anchors included | false |
| `BaselineUnknown` | this node could not establish the host's posture at all | false |

`BaselineState.Filters()` returns true for exactly one of the four.
`BaselineUnknown` returning false is the design, not an oversight: "we
did not look" is not "it is fine", the same rule `Drift.InSync()`
already follows. `BaselineUnfiltered` and `BaselineDisabled` are kept
apart because the remedy is different - there is nothing to enable in
the first case - and neither is ever an answer that reads as "the
firewall is working".

Each individual fact is a three-valued `Observation` - `Present`,
`Absent`, `Unknown` - never a bool:

- `PFEnabled` - from `pfctl -s info`'s `Status:` line. `Unknown` when
  pfctl could not be run, printed no status line, or printed a status
  word this package does not recognise. **This is the field that used
  to be `Enabled bool`**, and the three values are the fix for defect
  (2) above.
- `DefaultPolicy` - a `set block-policy ...` directive in the main
  ruleset.
- `BlockRule` - at least one `block` rule. Kept separate from
  `DefaultPolicy` on purpose: a host with one `block return in log`
  rule and no block policy *does* drop the traffic that matches it and
  passes everything else, and calling that either "filtered" or
  "unfiltered" without saying which rule is the whole ambiguity.
- `SkipLoopback` - `set skip on lo` (or `lo0`; both spellings exist in
  the wild). Its absence means loopback traffic walks the entire
  ruleset.
- `ApiaryAnchorReached` - **the anchor half of the same question.** The
  main ruleset contains an anchor rule covering `apiary/*`. `Absent`
  means every rule Apiary has ever loaded is sitting in an anchor
  nothing reaches.

`Absent` and `Unknown` are both `false` through `Observation.Bool()`,
and that is intentional: a caller that must tell them apart reads the
`Observation` itself, not a flag.

### The wildcard is the whole ballgame

`hasApiaryAnchorRule` matches exactly `anchor "apiary/*"`. A bare
`anchor "apiary"` is deliberately **not** counted. pf anchors are a
flat namespace and a rule matches an anchor by its full name, so
`anchor "apiary"` evaluates rules loaded into the anchor literally
named `apiary` and nothing in `apiary/<name>` - i.e. none of the
per-Cell or per-jail anchors `internal/pf` writes. The difference
between the two spellings is a firewall that does nothing and a
firewall that works, so the narrower one is not a judgement call.

### In sync is not enforcing

`Drift.Decisive(b Baseline)` and `Drift.Qualify(b Baseline)` join the
anchor's drift verdict to the host's baseline, because
`Drift.InSync()` is a statement about *text*: "the ruleset pf is
enforcing in this anchor equals the text this node loaded." That is a
real and necessary guarantee, and it is satisfied identically on a host
where the main ruleset never references `apiary/*` and on a host whose
anchor holds only `pass` rules under a permissive default policy. Both
are "in sync". Neither is a firewall.

`Decisive` requires all four of: the anchor is in sync and non-empty;
pf is confirmed enabled; the main ruleset reaches `apiary/*`; and the
rules can change an outcome (they contain a block rule of their own, or
the host's default policy already drops unmatched traffic and a `pass`
rule is a hole in it). The last condition names the previously
invisible case: an anchor of `pass` rules under a permissive default
policy is *loaded, matching, and irrelevant*.

`Qualify` renders the honest one-line qualification - "this anchor is
empty, so it drops nothing", "these rules are loaded and never
evaluated", "these rules only pass and the host would have permitted
the traffic anyway" - and returns the **empty string** when the two
together are a real guarantee, and also when the drift verdict is
already the worse news and a caveat would only dilute it.

### Read-only, structurally

`AssessHost` issues exactly two commands: `pfctl -s info` and
`pfctl -sr` (no `-a`, so the main ruleset, not an anchor). Both are
reads. **There is no code path in this file that can write to pf, and
the ADR records that as a design property rather than an accident** -
enforced by a test that fails if a future edit adds `-a`, `-f` or
`-F`. `ClassifyHost` is a pure function of two raw strings and two
errors, which is the only reason this is testable on a machine with no
pf, no root and no network. It is not run on every `Apply`: the main
ruleset is a host-level fact that changes on a human's timescale, not
once per reconcile tick per Cell, so `internal/hoststats` asks for it
once per stats gather and the install preflight once per run, and the
verdict is a value the caller keeps.

`Baseline` also carries the raw `MainRuleset` text, because a one-line
`/etc/pf.conf` is the most damning single piece of evidence there is
and an operator should be able to see it rather than be told about it.

## The exposure this exists to make visible

Stated on its own, because it is the actual finding and everything else
is plumbing:

> An unauthenticated `GET :8081/v1/vms` returned **HTTP 200 from the
> LAN on all four Combs** of a live colony in which every Comb reported
> PF "enabled", every Comb's `/etc/pf.conf` was the single line
> `anchor "apiary/*"`, and pf was filtering nothing.

Nothing in this ADR closes that exposure, and it is not this ADR's job
to. Closing it is the host operator's decision about the host's main
ruleset. What this ADR guarantees is that Apiary will now *say so* -
per node, in the same place it already reported "enabled" - instead of
reporting a green shield for a firewall that was not there.

## Consequences

- **The UI must not paint a green shield from `enabled` alone.** This
  is the direct consequence and it is a binding constraint on
  consumers of `PFStats`, not a suggestion. `PFStats.host_firewall`
  (`"filtering"`, `"unfiltered"`, `"disabled"`, `"unknown"`) is the
  field that answers "is anything being filtered?", and
  `PFStats.apiary_anchor_reached` (`"present"`, `"absent"`,
  `"unknown"`) answers "are the rules you wrote even being evaluated?".
  A green shield requires `host_firewall == "filtering"`; anything else
  must render as a distinct state, with `unknown` rendered as
  **unknown** and never as the good case. The existing shield is
  therefore, on a real live host, a bug that this change makes
  visible.
- **`Enabled` keeps its old meaning and its old type.** It is still
  pfctl's `Status:` line, verbatim, and it is still a `bool` in
  `PFStats` because it is genuinely binary when it is successfully read
  - it just is not the field that answers the question anyone was asking
  it. No existing caller breaks.
- **`hoststats` now reports pf twice, in different vocabularies.** A
  reader must learn that `Enabled: true, Baseline.State: unfiltered` is
  a coherent and important combination, not a contradiction. The
  `PFInfo` doc comment says so in as many words, because that comment
  is what was wrong before.
- **A gather that cannot read `pfctl -s info` still reports a
  baseline.** `gatherPF` returns the `PFInfo` alongside the error, and
  `Gather` deliberately does not clear `s.PF` on error, because the one
  gather that could not read pf's counters is exactly the gather a
  reader most needs to hear "we could not ask" from.
- **`internal/hoststats` now depends on `internal/pf`.** Acceptable: it
  already ran `pfctl` itself, `ClassifyHost` is pure and pulls in no
  process spawning, and the alternative is a second, worse parser of
  the same two commands.
- **Every Comb in a fresh install will report `unfiltered` until its
  operator writes a block policy.** That is the system working. It is
  also a visible, permanent state in the host stats, and the cost of
  that is paid deliberately rather than by hiding it.

## Rejected alternatives

### 1. Silently install a default-deny policy on the host

The obvious remediation for the finding, and rejected. A default-deny
policy applied to a live host that has SSH, a console, a management LAN
and possibly an out-of-band path is a self-inflicted lockout, and ADR-0129
already records "any to any" as a host-lockout hazard in the other
direction. Apiary does not hold the information needed to write a
*safe* deny policy for a host it did not build - which interfaces are
trusted, whether the operator is connected over a link the proposed
policy would drop, whether console access exists - so any policy it
wrote would be a guess with the authority of a firewall. The operator
writes the host's main ruleset; Apiary tells them what is currently
there.

### 2. Automatically edit `/etc/pf.conf`

Rejected for the same reason with a sharper edge. `/etc/pf.conf` is
the host's file, hand-maintained, and Apiary's own discipline around
it is the `pf-anchor` install check: verify the one-line
`anchor "apiary/*"` stanza is present, and if it is not, say so and
back the file up before touching anything. Rewriting the body of a file
whose every other line is somebody else's policy is categorically
different from adding the one stanza Apiary needs in order to function
at all. It is also not reversible by a colony-wide undo, which is the
property ADR-0137's last-known-good work exists to guarantee for
everything Apiary *does* write.

### 3. Treat a failed `pfctl` read as "no firewall", or as healthy

Both rejected, and they are the two failure directions that make a
two-valued bool unusable here. "No firewall" manufactures a scary host
out of a permission error - it reports a change in the world that did
not happen. "Healthy" is the live bug this ADR fixes, one level down: it
is what `parsePFInfo` did when it answered `Disabled` for output it did
not understand, and it is why `unknown` is a first-class state rather
than a log line. Neither reading is a fact; both are noise dressed as a
verdict.

### 4. Widen `apiary/*` to any-to-any rules to compensate

Rejected outright, and separately from ADR-0137's scope work: making
`internal/pf` write broad host-bound rules to "cover" the host's missing
policy would be Apiary silently acquiring a host firewall, with all the
lockout and drift risk of one, under a name that says "per-Cell
firewall". It would also be invisible - the operator would not know
their host's policy had changed hands. Compensation for a missing host
policy belongs in the host's policy, written by whoever owns it.

### 5. Report `enabled` plus a single boolean `host_firewall`

Rejected: it is the same conflation as (3), one type up. A caller
still cannot tell a pf that is off from a pf that could not be asked, and
"off" and "we don't know" call for different operator responses. The
cost of the richer types is a few string constants in a pure function.

### 6. Do nothing, and file it as an operational note

Rejected. The finding is not about a colony that happens to be
misconfigured; it is that Apiary's own dashboard reported "the firewall
is actually enabled and doing something" for a host where that was
false, and would keep doing so on the next host. The defect is in the
claim, and only a change to the claim fixes it.

## Scope boundary, stated explicitly

**Apiary does not manage the host's main ruleset, and this check only
ever reads.** Not one call in `internal/pf` loads, flushes, or addresses
anything but `apiary/*`; the two new commands are `pfctl -s info` and
`pfctl -sr`, both reads, with no `-a`, no `-f` and no `-F`. The
`TestAssessHost_NeverWritesToPF` test is the enforcement mechanism, and
it is written to fail loudly if a future change breaks the property
rather than to accommodate one. This ADR reports the host's posture; it
does not own it, and it does not intend to start.

## Relationship to existing decisions

- **ADR-0022 (VLAN networks, DHCP, per-VM firewall)** created the
  per-VM pf(8) anchor and the reason this project runs pf at all, and
  `internal/hoststats` was added alongside it for the host stats page.
  ADR-0022 is a guest-boundary decision: it says nothing about the
  host's own filtering, which is why a per-VM firewall could be
  described as "enabled and doing something" while the host beneath it
  filtered nothing.
- **ADR-0137 (Rule scope, last-known-good, and pf drift evidence)** is
  the direct parent. It fixed what happens *inside* an anchor -
  unbounded `from any to any` rules, last-known-good, drift evidence -
  and it explicitly left the main ruleset alone, because the confinement
  to `apiary/*` is deliberate. This ADR does not revisit that decision;
  it fills the one gap it created, the posture of the ruleset
  ADR-0137 deliberately does not own, and it inherits ADR-0137's
  discipline unchanged: silence is not health, an unestablished claim is
  not a pass, and every write stays inside the anchor.
- **ADR-0129 (PF firewall management)** covers the Colony-wide
  firewall object. Nothing in this ADR touches it, and in particular
  this ADR is not a back door to it: if a host-level Apiary-managed
  ruleset is ever wanted, that is ADR-0129's subject and its
  `any to any` lockout analysis, not a side effect of a read-only check.

## Verification

**Verified here (macOS, no FreeBSD, no pf, no root):** the whole
decision path is exercised as a pure function over fixture text
(`internal/pf/hostbaseline_test.go`), so a regression that renders
`unknown` as the good case fails rather than passing quietly. Covered:
the measured live node's own ruleset - one `anchor "apiary/*"` line, pf
enabled - classifies as `BaselineUnfiltered`, not `filtering`; a
`set block-policy` and a bare `block` rule each classify as
`BaselineFiltering` for their own stated reason; pf disabled is
`BaselineDisabled`; a missing pfctl and an unparseable ruleset are
`BaselineUnknown` with *every* dependent observation set to `Unknown`
rather than to `Absent`; presence and absence of each of the five facts
individually, including that `anchor "apiary"` alone does **not** reach
`apiary/*` and that a commented-out `# block everything` is not a block
rule; the raw main ruleset is retained as evidence; `Baseline.String`
names the subject and the silence in every state, including
`unfiltered` even when the anchor *is* reached;
`TestAssessHost_NeverWritesToPF` asserts the no-`-a`/`-f`/`-F`
property over the actual invocations; and the drift half asserts that
in-sync-but-empty, in-sync-but-pf-disabled,
in-sync-but-anchor-unreached and in-sync-but-pass-only are all not
decisive, with `Qualify` silent when drift is the worse news and
non-empty otherwise. `gofmt`, `go build ./...`, `go vet ./...` and
`go test ./...` all pass.

**Not verified, and not verifiable on macOS:** that this FreeBSD prints
`set block-policy` and `set skip on lo` inside `pfctl -sr` output at
all. If it does not, a present block policy reads as `Absent` - a
*pessimistic* miss (a host reported less filtered than it is), not a
false "filtering", and deliberately so. Also unverified here: exact
`pfctl -s info` spelling on a live 16.0-CURRENT host, and anything at
all about HAST, ZFS, jails, bhyve or real-network timing.

**Measured by hand on the live colony, read-only:** the
`/etc/pf.conf` contents, `pfctl -sr` output, `pfctl -a apiary -sr`
emptiness, and the `GET :8081/v1/vms -> HTTP 200` result quoted above.
No host configuration was changed to obtain any of it.

**Pending on the testbed** (`brood` or `drone`, cross-compile with
`GOOS=freebsd GOARCH=amd64 go test -c ./internal/pf` and run as root) -
there is no integration test for this file yet, and the two open
questions are the ones above:

- whether `pfctl -sr` on a real host renders `set` directives, which
  decides whether `DefaultPolicy` and `SkipLoopback` can ever be
  observed as `Present`;
- that `ClassifyHost` on a real host with the stock FreeBSD ruleset
  yields `BaselineFiltering` and on one reduced to
  `anchor "apiary/*"` yields `BaselineUnfiltered`, on a real pfctl,
  with the real output captured.

Both are reads and neither changes a host, so they can be run on a
Combs without risk. What is deliberately **not** pending, and will
never be part of this ADR's verification: a test that installs a block
policy.
