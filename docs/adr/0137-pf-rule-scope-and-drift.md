# ADR-0137: Rule scope, last-known-good, and pf drift evidence

## Status

Accepted. Implemented in `internal/pf` (scope.go, rules.go,
knowngood.go, drift.go, manager.go) with its two existing call sites
updated. Independent of ADR-0129's status - see *Relationship to
ADR-0129*, which records the one place this ADR and ADR-0129 disagree
and how to resolve it.

## Context

Two defects in `internal/pf`, both of which have been there since
ADR-0022 gave every VM its own pf(8) anchor, and both of which the
package's own doc comments had quietly described as normal.

### 1. Every rule Apiary has ever loaded was unbounded

`internal/pf/rules.go`, before this change, was 90 lines and rendered
one shape. `renderRule` validated direction, action, protocol and
port range, and then wrote, unconditionally:

```go
b.WriteString(" from any to any")
```

There was no field on `Rule` for an interface, a source, or a
destination, so there was nothing else it could write. A `block in
proto tcp port 22` rule - which reads, in the UI, like "do not accept
telnet for this VM" - rendered as `block in proto tcp from any to any
port 22`, which pf evaluates against every packet on every interface
the anchor is evaluated in.

Three things follow, and only the first is about correctness:

- **A rule is not what it says.** The operator's intent is a fact
  about a VM; the rendered rule is a fact about the host.
- **The renderer cannot express a policy that distinguishes
  host-bound from guest-bound traffic.** ADR-0129's Context names
  this as the first of its three driving facts: "there is no ordering
  trick that can distinguish guest-bound from host-bound traffic when
  the rule cannot name an interface."
- **A `block in` with no protocol and no port renders `block in from
  any to any`** - the exact line ADR-0129 identifies as a host-lockout
  vector, reachable today from a rule an operator wrote in a form that
  sounds like it applies to one VM. This ADR does not assert that any
  current host has been locked out by one; `/etc/pf.conf` is a host
  prerequisite this project does not manage, and where the operator has
  placed `anchor "apiary/*"` is not something this code can see.

### 2. No durable record, and no read-back

`Manager` was a zero-sized struct. It held no last-applied text, no
generation counter, no anchor inventory. `Apply` rendered a ruleset,
handed it to `pfctl -a <anchor> -f -`, and returned the error. That
was the whole contract, and it has three holes:

- **A successful `pfctl` is not proof of enforcement.** It is a
  statement about pfctl's parse and load. A load can be refused
  halfway; an anchor can be hand-edited; a host can reboot with a
  ruleset that never survived. `Apply` returns nil in all of those
  cases and the reconciler moves on.
- **There is no baseline to compare against,** so "pf is enforcing
  something we did not write" is not a distinguishable event from "pf
  is enforcing what we wrote." It is not even a detectable one.
- **There is nothing to fall back to,** which matters less for a
  per-VM anchor than it does for a host policy (ADR-0129's rollback
  source) but still matters: the answer to "what did you think you
  were enforcing on this host?" is currently "we have no idea, and
  we never kept the text."

ADR-0129 lists both of these in its *What is missing* section, as
items 4 and 5 ("No read-back", "No memory"). This ADR builds the
underlying capability; ADR-0129 builds the Colony-wide object,
staging, and evidence surface on top of it.

## What already exists

Unchanged by this ADR, and worth naming so the diff is honest about
what it touches:

- **`RenderRules` is pure**, has no pf dependency, is all-or-nothing,
  and emits no `quick` - so ADR-0075's last-match-wins evaluation and
  the priority-by-position convention are untouched. This purity is
  why every new behaviour here is testable on macOS with no pfctl, no
  network and no root, and it was the reason to extend the renderer
  rather than introduce a second ruleset language.
- **`Apply`'s full-replace-not-diff convention**, borrowed from
  `internal/hast`'s `WriteConfig`; **`Flush`'s "No such anchor is
  success"** idempotent-teardown posture; **`runCmdStdin`'s error
  format** (`"<name> <args>: <trimmed stderr>"`), which is what the
  drift detail strings quote.
- **`ApplyNAT`**, whose single `match out on <uplink> from <subnet>
  to any nat-to (<uplink>)` line already names its own scope, and
  whose newline check is the precedent this ADR's validation follows
  ("defense in depth: this function has no way to know that
  validation ran, so it rejects a newline itself").

Two callers build `pf.Rule` values, and both are affected by making
the scope mandatory - see *Affected callers*.

## Decision

### The scoping rule

`Rule` gains four fields. Together they are where the rule applies -
the `on` / `from` / `to` clauses of the rendered rule:

```go
Interface   string // "vtnet0", or "" for no `on` clause
Source      string // "10.60.0.0/24", "10.60.0.7", "table <name>", or ""
Destination string // same shapes as Source
Any         bool   // the explicit opt-in for a from-any-to-any rule
```

plus a `Comment string`, rendered as a pf `#` line above the rule and
validated as a single line (see below).

The rule is one sentence: **a rule must declare its scope, the
declaration is validated at render time, and the only declaration that
widens to everything is `Any: true`.** Concretely, `scopeClause`
accepts exactly these four shapes and refuses everything else:

| Declared | Renders | |
|---|---|---|
| nothing at all | - | **refused** - the zero value of `Rule` |
| `Interface` only, no `Any` | - | **refused** - `on vtnet0` with no address clause still means "any address", which is the broad rule minus the `on` |
| `Any: true` | `from any to any` | accepted: the opt-in, on its own |
| `Any: true`, `Interface` set | `on <iface> from any to any` | accepted: strictly narrower than the bare opt-in |
| `Source` + `Destination` | `from <src> to <dst>` | accepted, with or without an interface |
| `Source` only, or `Destination` only | - | **refused** - pf scope is a from/to pair and this renderer will not guess the other half |
| `Source: "any"`, `Destination: "any"` | - | **refused in favour of `Any: true`** - a deliberately broad rule is one obvious field, not two strings |
| `Any: true` *and* either address set | - | **refused** - the opt-in means "addresses unconstrained"; a rule that also names an address is not saying that, and rendering the address away would widen it silently |
| one end `any`, the other named | `from any to <dst>` | accepted - the ordinary "anyone may reach this one host" rule, and not a broad one |

Validation of the individual values is a **whitelist, not a
blacklist**, which is what makes this a security property rather than a
tidiness property:

- An interface must be 1-15 characters (IFNAMSIZ-1 on FreeBSD) of
  `[A-Za-z0-9._-]`, and may not be the word `any` (pf has no `on any`).
  A superset of `internal/nodeconfig`'s own `validInterfaceName`, so
  every uplink that node config already accepts still applies.
- An address must be exactly one of: the literal `any`, an IP literal
  or CIDR prefix (`netip.ParseAddr` / `netip.ParsePrefix`), or
  `table <name>` with a pf-table-legal name. A pf macro (`<trusted>`),
  an embedded space, a newline, a second rule after a newline, a
  nonsense string - all refused.
- A comment must be a single line with no `\n`, `\r` or NUL. A comment
  is still a line pfctl reads, so an unvalidated comment is a way to
  write a rule this package did not intend to write. Same reasoning,
  and the same shape of fix, as `ApplyNAT`'s existing newline check.

`RenderRules` remains all-or-nothing: one unrenderable rule produces no
body at all, so a ruleset that is "everything except the broken rule"
can never reach `pfctl -f -`.

**Rule shape is preserved.** The `on` clause is emitted immediately
after the direction, and the `from`/`to` pair lands exactly where
` from any to any` used to, so a rule that carries only the opt-in
renders byte-for-byte what it always has:
`pass in proto tcp from any to any port 22`, and a scoped rule is that
same rule with a few more words in it and nothing else moved. No
`quick`, no reordering, no priority change: ADR-0075's meaning is a
property of position in the slice, not of pf syntax.

The exact clause order the renderer writes is
`<action> <direction> [proto <p>] [on <iface>] from <src> to <dst>
[port <n>]`. That is pf's own documented order with `proto` and `on`
transposed relative to the canonical form, which pf accepts, but
**whether this particular host's pf accepts it is a claim the testbed
must confirm** - it is a parser question, and no test in this
repository can answer it.

### Last-known-good

One file per anchor under `/var/db/apiary/pf/`, written only after a
load has actually succeeded, atomically.

- **Node-local, never Raft.** This is the line `state.proto`'s own
  header draws: replicated state is small JSON-shaped facts (VMs,
  networks, intents); physical state - ZFS datasets, HAST bytes, and
  what one kernel's packet filter is enforcing - is per-node
  observation. Putting a pf ruleset in the log would mean a
  permanently divergent value in a replicated state machine and would
  make every node's load-failure a Colony-wide commit. ADR-0129 makes
  the same call for its `known-good.rules`; the two files are
  deliberately different (see *Relationship to ADR-0129*).
- **One file per anchor, named from the anchor**, mirroring the
  anchor's own hierarchy: `/var/db/apiary/pf/apiary/vm-<id>.rules`. It
  looks redundant and is - deliberately. The cost is a nested
  directory; the benefit is that no sanitising scheme of our own
  invention stands between an anchor name and its file, where a
  collision or a path escape could quietly give one anchor another's
  baseline. Anchor names are still validated (`no ".."`, no leading
  `/`, no control characters, no empty element) as defence in depth.
- **Format**, in full:

  ```
  apiary-pf-known-good v1
  anchor apiary/vm-<id>
  recorded 2026-09-26T19:20:00Z
  sha256 <hex digest of the body below>

  <body: one rule per line, possibly empty>
  ```

  The checksum is what makes corruption a *state* rather than a
  guess. `0600` file, `0700` directory.
- **Atomic**: temp file in the same directory -> `write` -> `fsync` ->
  `chmod` -> `rename` -> `fsync` of the containing directory. A
  ruleset file that exists but is missing its last rule is worse than
  no file at all, so the data is durable before the rename is, and
  the directory entry is durable after it.
- **Written only on success.** A `pfctl` failure leaves the previous
  record in place. If it did not, a failed load would silently become
  the new baseline and every later drift check would be comparing
  against a ruleset pf never took.
- **Idempotent.** Apply runs once per anchor per reconcile tick; a
  record whose body is already byte-identical is not rewritten, so
  there is no fsync churn.
- **`Flush` forgets the record.** A flushed anchor's last-known-good
  ruleset *is* the empty one, and the honest encoding of "the baseline
  is the empty ruleset" here is "there is no baseline" - the same state
  a never-loaded anchor is in, and both mean "this anchor is not
  enforcing anything we know about".
- **A failed record write after a successful load is returned as an
  error.** pf has the ruleset; the baseline every later drift check
  depends on does not. Reporting success there would be the worst of
  both: an enforced ruleset nobody can prove, and a silent loss of the
  evidence this ADR exists to create.

Four states, and none of them means "nothing to do":

| State | Wire value | Means |
|---|---|---|
| `KnownGoodLoaded` | `loaded` | a well-formed, non-empty record; the only state that provides a comparison baseline |
| `KnownGoodEmpty` | `empty` | a well-formed record whose body is empty: the last successful load loaded zero rules, which in this package's convention means everything allowed. **Not** the same as absent - an empty record is a positive observation, and comparing against it is meaningful |
| `KnownGoodAbsent` | `absent` | no record: never successfully loaded, or flushed since |
| `KnownGoodCorrupt` | `corrupt` | a record exists but does not parse or does not match its own checksum. A **state, not an error**, precisely so no caller can accidentally treat it as a successful load; it carries a reason |

`corrupt` must never collapse into `absent`. An absent record means we
have no baseline; a corrupt one means we believed we had a baseline and
cannot read it, so the running ruleset is *unexplained*.

### Drift

After a load, read the running ruleset back and compare it.
`Manager.Readback(ctx, anchor) (string, error)` wraps
`pfctl -a <anchor> -sr`; `Manager.CheckDrift(ctx, anchor) Drift` loads
the record, reads back, canonicalises both sides, and returns a value
(never an error, so the verdict cannot be logged and dropped, and never
a "could not tell" dressed up as success).

Four states:

| State | Wire value | Means |
|---|---|---|
| `DriftInSync` | `match` | read back successfully, and equal to the last ruleset this node loaded. **The only state that may be presented as "enforced"** |
| `DriftDetected` | `drift` | read back successfully and **differs**. A positive observation: a hand edit, a partial load, a record that predates a reboot that dropped it. Carries both sides and names the differing line |
| `DriftNotLoaded` | `not_loaded` | no record on this node, so there is no baseline. Says nothing about what pf is enforcing now - only that Apiary on this node never successfully loaded this anchor, or has flushed it since |
| `DriftUnknown` | `unknown` | the comparison could not be made: pfctl missing or failing, the record corrupt, the store unreadable. No evidence either way |

`Drift.InSync()` is true for `DriftInSync` only. `not_loaded` and
`unknown` are both false, deliberately: "we did not check" is not "it
is fine".

**Where the observation goes.** Every `Apply`, `ApplyNAT` and
`CheckDrift` records its verdict, and `Manager.LastDrift(anchor)`
returns it. That is what "observable, not logged and dropped" means
here in practice: the verdict outlives the call that produced it, and
any caller that asks can read it. `Apply` deliberately does **not**
fail the tick on a drift verdict - a transient read-back difference is
not evidence the operator should be told the firewall is down, and the
reconciler can no more usefully act on it than it could on the original
load, since it simply re-applies next tick. Surfacing this in the UI
and in ADR-0122's evidence API is ADR-0129's job, not this ADR's.

**Canonicalisation** (`canonicalRules`) collapses per-line whitespace
runs, and drops blank lines and `#` comments. It deliberately does
*not* sort, and does not try to understand pf's output beyond
whitespace:

- Sorting would hide a real change. Rule order *is* pf's evaluation
  order (ADR-0075, no `quick`, last match wins), so a ruleset whose
  lines were reordered by hand has changed meaning and a
  set-comparison would call it in sync.
- Aggressive normalisation is how "in sync" quietly becomes "we
  normalised both sides until they matched". Anything beyond whitespace
  needs a real pf to test against.

## Affected callers

Two call sites build `pf.Rule` values. Both are named here rather than
fixed quietly, because both are affected in the same way and neither
can be narrowed yet.

**1. `internal/cluster/reconciler.go`'s `toPFRules`.** Converts a VM's
raft-state `FirewallRule` list into `pf.Rule`. It now sets `Any: true`
explicitly, with the reason in the function's own comment.

`api/internalpb.FirewallRule` has exactly five fields - `direction`,
`action`, `protocol`, `port_range`, `priority` - and **no interface or
address field at all**. There is therefore nothing to narrow a per-VM
rule *to* without a proto change, and this ADR does not make one
(that change is ADR-0129's, and its Context already calls the renderer
fix "mandatory before this scope is exposed", precisely because
narrowing every live deployment's rendered rules is a behaviour flip
that needs a testbed rehearsal and a migration note rather than a
quiet release). So the widening is preserved, deliberately and
grep-ably, in one function, instead of being what an undeclared scope
silently meant. A test
(`TestToPFRules_ExplicitlyOptsIntoTheAnyToAnyScope`) pins both halves
of that: the opt-in is present, and the scope fields are empty.

This is the honest cost of the stricter renderer: **until ADR-0129's
proto work lands, every per-VM firewall rule on every live Comb is
still any-to-any.** The difference is that this is now a decision
someone made, at a named place, with a named follow-up - and the
renderer will refuse to produce it for anyone who has not made that
decision.

**2. `internal/jailnet/firewall.go`'s `JailRules`.** Same situation,
same fix, same annotation. `JailRule` is field-for-field
`api/internalpb.FirewallRule`, so it has the same missing scope fields,
and its doc already says the future `JailDefinition.firewall_rules`
field is a one-line conversion when the proto changes.

**Unaffected:** `Manager.Apply`/`Flush`/`ApplyNAT` signatures (so
`cluster.pfManager` and `jailnet.PFLoader` and their test fakes are
untouched), `cmd/managerd`'s `&pf.Manager{}` (the zero value is still
valid and means "the real pfctl, the default record directory,
read-back on"), and every proto, RPC and template.

## Relationship to ADR-0129

Complementary, and deliberately ordered so that ADR-0129 slots on top:

- **Reused verbatim:** the renderer's purity and all-or-nothing
  behaviour, the no-`quick` last-match-wins semantics, `Apply`'s
  full-replace convention, `Flush`'s "No such anchor" success case,
  `runCmdStdin`'s error format, `Rule`'s decoupling from the wire
  schema, and the reconciler's narrow local-interface discipline.
- **Names chosen to match:** `Rule.Source`, `Rule.Destination`,
  `Rule.Interface`, `Rule.Comment` and `Readback(ctx, anchor)` are
  ADR-0129's names, so its proto work can carry the fields through
  without this renderer being rewritten. `Parse(ctx, body)` and
  `Enabled(ctx)` remain unimplemented; they belong to a staged host
  policy, which does not exist yet.
- **Deliberately different:** this ADR's records are one file per
  anchor under `/var/db/apiary/pf/`; ADR-0129's `known-good.rules` is
  one file at `/var/db/apiary/firewall/`. They are not alternatives to
  each other. This one is *evidence* for every anchor this package
  loads, host or guest, and a single shared file would be clobbered by
  every VM's `Apply`. ADR-0129's is a *rollback source* for a staged
  host policy, which is one file because there is one policy.
- **One point of disagreement, stated rather than resolved
  silently.** ADR-0129's Implementation notes plan for the new proto
  fields with the comment: *"Empty preserves the pre-ADR-0129 rendering
  exactly ('from any to any'), which is why this is additive rather
  than a breaking change to existing rules."* **This ADR supersedes
  that.** An empty scope is now a render error, and `from any to any`
  requires an explicit opt-in field. ADR-0129's proto design therefore
  needs one more field than it planned - a `bool any = 9` (or an agreed
  sentinel) - because a rule that arrives over the wire with all three
  scope fields empty would otherwise be refused at render time and
  **every existing VM's firewall would fail to apply**. Whoever
  implements ADR-0129 must carry that field; this ADR's
  `toPFRules` comment is where the coupling is documented in the tree.

## Rejected alternatives

**1. Leave `renderRule` alone and scope rules at the call site** (wrap
each VM's rules in an extra `on <iface>` line, or filter by address
before rendering). Rejected: the renderer is the one place that knows
what pf will actually evaluate, and a scoping decision made by a caller
is a scoping decision every future caller has to remember to make.
ADR-0129 makes the same argument about admissibility - "the checker
runs over the *rendered* ruleset, not the rule list, because that is
what pf will actually evaluate."

**2. Default the scope to "any" and let callers narrow it.** Rejected
outright: that is the defect, renamed. The whole point is that the
broad rule must be *asked for*.

**3. Keep `from any to any` but add an `Interface` field, using the
interface when present and silently falling back to `any` when
absent.** Rejected: the fallback is the accident, and it would fire
exactly when a caller forgot. Every no-scope caller in the tree -
`toPFRules`, `JailRules`, and every future one - would keep matching
the whole host without anyone noticing.

**4. Let `Source: "any", Destination: "any"` count as the opt-in.**
Rejected: it works, and it leaves "I meant this rule to be about
everything" as two strings that happen to say `any`, greppable only by
reading both fields. A single boolean is one reviewable fact, and it is
the fact a security review looks for.

**5. Reject `block` + any-to-any outright, as a renderer-side safety
check.** Rejected, and this is the interesting one. The renderer has
no idea where the anchor is evaluated: whether `anchor "apiary/*"` in
the host's `/etc/pf.conf` sits in a place where an inbound rule can
match the operator's SSH session, or in a place where it cannot, is a
property of a file this project deliberately does not manage
(`internal/pf/exec.go`'s own package doc, and ADR-0129's rejected
alternative 1). A renderer-side check would be a check that cannot
fail honestly - it would either be a no-op that looks like a
guarantee, or it would refuse a legitimate per-VM block. The honest
place for that judgement is an admissibility check over the rendered
ruleset with the host's real configuration in hand, which is
ADR-0129's design, for ADR-0114's reason: the guardrail has to be
real, not a dialog and not a heuristic in a function that cannot see
the thing it is judging.

**6. Keep the last-known-good in memory, or in Raft, or reconstruct it
from the desired state in raft on demand.** All three rejected.
In-memory dies with managerd, which is precisely the moment you want
the record. Raft is the wrong store for node-local observation
(above), and reconstructing from desired state cannot work: it answers
"what did we ask for", not "what took effect", and the difference
between those two is the entire subject of this ADR.

**7. One shared last-known-good file for all anchors.** Rejected: every
VM's `Apply` would overwrite the previous VM's baseline, and the result
would be a file that is correct for exactly one anchor and wrong -
silently - for every other.

**8. Delete the record on `Flush` rather than writing an empty one.**
Rejected: the record means "the last ruleset successfully loaded". After
a flush that is the empty ruleset, and re-recording it would make a
flushed anchor indistinguishable from a loaded-empty one - which is the
distinction `KnownGoodEmpty` exists to preserve.

**9. Treat a corrupt record as absent, or as an empty ruleset.** Both
rejected: the first loses the fact that a baseline existed and went
bad; the second is how a node ends up enforcing nothing while
believing it knows exactly what it is enforcing.

**10. Make `Apply` fail the tick on a drift verdict.** Rejected: a
`pfctl -sr` output-format difference would then break every VM's
firewall every tick, and the read-back has not been validated against
a real pf (see *Verification*). Drift is retained and exposed instead,
and a host that wants the stricter behaviour can call `CheckDrift` and
act on it.

**11. Normalise `pfctl -sr` output aggressively** (service names,
`port = N`, flags, sorting) to make the comparison robust. Rejected
for now, and deliberately left as the one obvious place to revisit: it
cannot be written honestly without a real pf to test against, and
guessing at the format is how "in sync" becomes a fiction. The
conservative canonicaliser's failure direction is a *reported* drift,
not a missed one.

**12. Validate `ApplyNAT`'s uplink and subnet through the new
validators.** Rejected as scope creep in this ADR, and recorded as
open: `nodeconfig` already constrains an uplink to
`[A-Za-z0-9-]{1,15}`, which this ADR's interface validator accepts
unchanged, and the subnet shape is a separate question with its own
compatibility surface. The NAT line is recorded and read back like any
other ruleset, which is the part that matters for evidence.

## Consequences

**Positive**

- A rule's blast radius is a declared fact. A caller that forgets to
  declare one gets a loud local error before anything reaches `pfctl`,
  not a rule that quietly matches the host.
- A broad rule is a decision, greppable in one field, in one function
  per caller, with a named follow-up.
- The exact ruleset that was last loaded survives managerd, and is
  available to any operator with `cat`.
- Drift is a first-class, retained, four-state observation with a
  comparison that is a pure function of two strings.
- "We do not know" has its own names, and no code path renders
  "unknown" or "not loaded" as "in sync".

**Negative**

- `Rule` construction is now a compile-visible decision at every call
  site, and two of them have to say "any" out loud. That is the point,
  and it is also a breaking change to anything that builds a `Rule` in
  a way this repository does not know about.
- Every successful `Apply` now does two `pfctl` execs (load, then
  read-back) and one `fsync`ed write. On a node with many VMs this is
  a real per-tick cost. `Manager.DisableReadback` is the valve; it
  costs you the automatic observation, not the record.
- `Manager` is no longer a zero-sized struct, contains a mutex, and
  must be used by pointer.
- Nothing in the UI or the evidence API reads any of this yet. The
  states exist, are retained, and are callable; wiring them into
  ADR-0122/ADR-0129's surfaces is not done.

## What is not addressed

- **Per-VM rules are still any-to-any in practice** (above). This ADR
  makes the over-broadness explicit and blocks *accidents*; it does not
  make per-VM rules narrow. ADR-0129 owns that, with the proto change
  and the testbed rehearsal.
- No staged apply, no rollback, no deadline, no local probes. Those are
  ADR-0129, and they need a host-scope policy object that does not
  exist yet.
- No anchor inventory ("which `apiary/*` anchors exist?"). Nothing
  still knows, so an orphan anchor is still undetectable except by
  listing the host's anchors by hand.
- No IPv6 story. A `fd00::/8` scope renders and is not distinguished
  from an IPv4 one; `ApplyNAT` remains IPv4 NAT by construction.
- No `Parse(ctx, body)` (parse-only pre-flight) and no `Enabled(ctx)`
  (is pf live at all). Both named in ADR-0129, both unimplemented.
- Nothing about the drift states reaches `internal/health`,
  `internal/assumptions` or the frontend.
- `pfctl -sr`'s output format is not validated against a real pf; see
  below.

## Verification

**Established here, on macOS, with no pfctl, no network and no root:**

- `RenderRules` is unchanged for every pre-existing rule: all eleven
  original tests in `internal/pf/rules_test.go` still assert the same
  rendered pf text, with the one addition of `Any: true` to the input
  literal - the expected strings are untouched.
- The unscoped zero-value rule is refused
  (`TestRenderRules_UnscopedRuleIsRefused`).
- Every shape in the table above renders or is refused, including the
  half-declared address scope, the two-string broad rule, and the
  opt-in-plus-addresses contradiction.
- Malformed scope input is refused by whitelist: newline injection,
  embedded spaces, a shell-ish interface, an over-long interface, a pf
  macro, a bad prefix length, an illegal table name, a multi-line
  comment. `RenderRules` returns no body alongside any of them.
- Last-known-good round-trips the exact rendered ruleset, records the
  anchor and a timestamp, keeps two anchors apart, and survives a
  second `Apply` (replaced, not appended, with no temp file left
  behind and `0600` on the file).
- All four record states are reachable and distinct, including nine
  distinct corrupt-file shapes (empty file, no header, truncated
  header, no blank line, edited body, truncated tail, unknown header
  key, wrong anchor, bad timestamp), none of which yields a body.
- A failed `pfctl` load records nothing and does not disturb the
  previous record; an unrenderable ruleset never invokes `pfctl` at
  all; a failed record write after a successful load is an error.
- `Flush` forgets the record and stays idempotent, including for an
  anchor that never existed.
- All four drift states are reachable, including a failed read-back
  (three distinct failures), a corrupt baseline (unknown, not drift,
  not in-sync), an empty baseline compared against an anchor that has
  since gained a rule, and a retained `DriftDetected` verdict from a
  load that itself succeeded.
- `canonicalRules` ignores whitespace, blank lines and comments, and
  does **not** ignore rule order.
- `toPFRules` sets the opt-in and leaves the scope fields empty.

**Not established here, and not establishable on macOS:**

- **Anything about pf itself.** No real `pfctl` was run, no anchor was
  loaded, no ruleset was enforced. That the rendered clauses are valid
  pf syntax, that pf accepts them in this order, and that
  the clause order the renderer writes parses the way it is written,
  are all claims about pf(8)'s parser that this
  repository's test environment cannot check. A testbed run against
  brood or drone is required before any of it is a verified fact.
- **`pfctl -sr`'s output format.** Whether pf resolves `port 22` to
  `port = ssh`, prints evaluation flags we did not write, or differs
  between versions, is unknown here. The conservative canonicaliser's
  only guarantee is that a format difference reads as *drift*, not as
  in-sync. **This is the most likely source of a false positive in
  production, and it is deliberately loud rather than quiet.**
- **`pfctl -a <anchor> -sr` on an anchor that does not exist** -
  whether that is empty output or an error, and therefore whether
  `CheckDrift` on a flushed anchor reports `match` or `unknown`.
- **Performance.** Two `pfctl` execs plus an `fsync` per anchor per
  reconcile tick, on real hardware, with real disk latency. macOS and a
  `t.TempDir()` say nothing about a FreeBSD node's per-tick cost.

## Test plan

On brood or drone (bare-metal FreeBSD, real pf, passwordless sudo) -
**not run as part of this ADR**, which changed no live configuration
and ran no `pfctl`:

1. `pfctl -a apiary/test-adr0137 -f -` a body rendered with an
   interface scope, an address scope, a `table` scope, and one
   explicit `Any: true` rule. Confirm all four parse, and that
   `pfctl -a apiary/test-adr0137 -sr` prints something this
   canonicaliser agrees with.
2. Capture that `-sr` output verbatim and add it to a
   `canonicalRules` test as the real-world fixture. Whatever it shows
   about service-name resolution, `port = N`, or flags is the input to
   the canonicaliser's next revision.
3. Load, then edit the anchor out of band by hand
   (`pfctl -a apiary/test-adr0137 -f -` with an extra rule), then
   `CheckDrift` - expect `drift` with a detail that names the line.
4. Make `pfctl` unreadable for one invocation (a bad PATH) and
   `CheckDrift` - expect `unknown`, never `match`.
5. Truncate the record file and `CheckDrift` - expect `unknown` with
   the corruption reason, and confirm the running ruleset is
   untouched.
6. Measure the per-tick cost of the second `pfctl` exec on a node with
   a realistic VM count, and decide whether
   `DisableReadback` is needed anywhere in practice.
7. Rehearse the ADR-0129 question this ADR leaves open: what actually
   happens on a real host when a per-VM rule is narrowed to the guest's
   own interface, with an operator's own SSH session in the path. That
   is the rehearsal ADR-0129 requires before its proto change lands.

## Open questions

1. **Should the `Any` opt-in be a bool on `Rule`, or an enum
   (`scope_mode`)?** A bool is one fact and greppable; an enum extends
   to a third mode later (for instance "same subnet as the guest")
   without a second field. ADR-0129's proto work is the moment to
   settle it, since it has to carry one of them.
2. **Should a `block` rule that resolves to any-to-any be recorded as
   an assumption-result (`internal/assumptions`) rather than left as a
   `pf.Rule` field?** The renderer cannot judge it (see rejected
   alternative 5), but the caller that sets `Any` could emit an
   evidence row saying "this rule is unbounded, on purpose".
3. **Does the record need an `observed_at` separate from
   `recorded_at`?** Today they are the same instant, because a
   successful read-back happens inside `Apply`. If read-back ever
   becomes asynchronous, a stale `recorded_at` is a lie about the
   freshness of the evidence.
4. **What is the right behaviour when the store is full or the disk is
   read-only?** Today every `Apply` fails loudly, which is honest and
   probably right for a firewall - but on a node whose `/var` is
   read-only, that turns a working firewall into a failing tick for
   every VM. Is there a degraded mode that keeps enforcing and reports
   the missing baseline separately, and what would it be called?
5. **Should `CheckDrift` be called on a schedule, or only after a
   load?** A scheduled read-back would catch an out-of-band edit that
   no `Apply` ever notices, which is the case this ADR most wants to
   catch and the one it currently catches only on the next tick's
   apply. ADR-0129's reconcile loop answers this for host scope; a
   per-VM answer probably wants the same shape.

## References

- `internal/pf/scope.go`, `internal/pf/rules.go`,
  `internal/pf/knowngood.go`, `internal/pf/drift.go`,
  `internal/pf/manager.go` - the implementation this ADR describes;
  `internal/pf/exec.go` - `runCmdStdin`, unchanged
- `internal/pf/rules_test.go` (extended), `knowngood_test.go`,
  `drift_test.go`, `manager_test.go` (extended) - the tests
- `internal/cluster/reconciler.go`'s `toPFRules` (the ADR-0075 stable
  sort, now with an explicit scope opt-in) and
  `internal/jailnet/firewall.go`'s `JailRules` - the two call sites
- `api/internalpb/state.proto`'s `FirewallRule` - the five fields that
  leave a per-VM rule nothing to narrow to
- `internal/nodeconfig`'s `validInterfaceName` - the charset this
  ADR's interface validator is a superset of
- [ADR-0022](0022-network-management.md) - the per-VM anchor and the
  `apiary/*` reservation
- [ADR-0048](0048-self-hosted-outbound-nat.md) - the NAT rule `ApplyNAT`
  installs
- [ADR-0075](0075-firewall-rule-priority.md) - priority by position,
  and no `quick`; unchanged here
- [ADR-0079](0079-vm-firewall-rule-editing.md) - how a VM's rules get
  set; the shape this ADR's `Rule` fields are named to match
- [ADR-0071](0071-network-correction-workflow.md) - replicated intent
  versus node-local realisation; the line this ADR's record store
  sits on
- [ADR-0114](0114-remove-uplink-takedown.md) - a guardrail has to be
  real; the reason rejected alternative 5 declines to fake one in the
  renderer
- [ADR-0122](0122-cluster-evidence-aware-health-api.md) - the evidence
  surface these states should eventually reach, and do not yet
- [ADR-0129](0129-pf-firewall-management.md) - the larger design:
  the renderer gap, "No read-back", "No memory", the `Rule` field
  names, `Readback`, and the one point this ADR supersedes
