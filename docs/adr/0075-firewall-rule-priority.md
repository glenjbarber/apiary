# ADR-0075: Firewall rule priority

## Status

Accepted

## Context

ADR-0022 gave each VM a flat list of firewall rules, applied to its own
`pf(8)` anchor via `internal/pf.RenderRules` with no `quick` keyword -
meaning `pf`'s own default evaluation applies: rules are checked
top-to-bottom, and the *last* matching rule wins. A rule's effective
precedence has therefore always been implicit in its position in the
`FirewallRules` list - correct, but a real, disclosed v1 limitation
(README's own "no Apiary priority model beyond `pf`'s normal
top-to-bottom evaluation") since nothing about the UI or the schema
ever made that ordering visible or intentional. An operator adding a
new rule to an existing VM had no way to say "this one should win"
without understanding `pf`'s own last-match-wins semantics and
carefully re-ordering the whole list to match.

## Decision

Add `int32 priority` (field 5) to `FirewallRule` in both
`api/internalpb/state.proto` (the raft-replicated schema) and
`api/rpc/manager.proto` (the external API) - a higher number is
rendered later in the ruleset and therefore wins when it overlaps a
lower-priority rule, exactly matching `pf`'s existing (unchanged)
evaluation model. Every rule that predates this field defaults to
priority `0`.

`internal/cluster.FirewallRule` (the plain internal mirror type
`VMPlacement` carries) gains the matching `Priority int32` field, and
`toPFRules` now stable-sorts by `Priority` ascending before ever
building `internal/pf.Rule` values:

```go
sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
```

A stable sort specifically so rules sharing a priority (every rule
created before this ADR, all defaulted to `0`) keep their existing
relative order rather than being silently re-ranked by insertion order
becoming comparison order.

## Judgment call: last-match-wins preserved, `quick` deliberately not introduced

The obvious alternative design uses `pf`'s `quick` keyword (first
match wins, evaluation stops there) with priority driving evaluation
order directly - arguably a more intuitive mental model for an
operator used to "priority 1 rules are checked first and settle it."
**Rejected for this pass**: switching to `quick` semantics would
change the *effective behavior* of every firewall rule set that
already exists today, the moment this code deploys, with no way for an
operator to review the change first - a real, live behavior flip
disguised as a schema addition. Keeping `pf`'s existing last-match-wins
evaluation and only adding an explicit way to *express* the ordering
that already implicitly existed is the conservative, zero-behavior-
change-for-existing-rules option. A future `quick`-based mode, if ever
wanted, is a genuinely separate, disclosed design decision - not
bundled into this one.

## What else was threaded through

`Priority` needed updating at every existing `FirewallRule` conversion
site, all pre-dating this change and all mirrored field-by-field
rather than sharing a type (per ADR-0002/0005's internal/external
schema separation):

- `internal/manager/convert.go`'s `toInternalVM`/`fromInternalVM`
  (external `rpcpb.FirewallRule` <-> internal `internalpb.FirewallRule`).
- `internal/cluster/reconciler.go`'s raft-state-to-`VMPlacement`
  conversion.
- `internal/frontend/convert.go`'s `firewallRuleView` (the VM detail
  page's own read-only display type) and its construction site.
- `internal/frontend/server.go`'s `parseFirewallRuleRows` (the
  create-VM form's parallel-slice parsing) - a blank or unparseable
  `fw_priority` value defaults to `0` rather than rejecting the whole
  submission, matching every other optional field in that same parser.

`web/templates/new_vm.html`'s dynamically-added firewall-rule rows
(client-side JS, not `html/template`, since rows are added without a
page reload) gained a `fw_priority` number input defaulting to `0`,
plus explanatory copy. `web/templates/vm.html`'s read-only rule table
gained a Priority column.

## Not addressed

- No editing of an *existing* VM's firewall rules after creation - the
  create-VM form is still the only place rules (and now priorities)
  are set at all; this was already true before this ADR and isn't
  changed by it.
- `internal/restshim`'s REST API doesn't expose firewall rules in
  either direction (a pre-existing gap, confirmed via direct grep) -
  not addressed here either.
- No validation bounds on `Priority` itself (any `int32` is accepted) -
  unlike `direction`/`action`, which are small closed enums `internal/pf`'s
  own `renderRule` already validates, a priority number has no
  "invalid" value to reject.

## Verification

New tests: `internal/cluster/reconciler_test.go`
(`TestToPFRules_OrdersByPriorityAscending`,
`TestToPFRules_StableForEqualPriority`, and the integration-level
`TestReconciler_RunOnce_AppliesFirewallRulesInPriorityOrder` proving
the real `RunOnce` path - not just the pure helper - reorders
correctly); `internal/frontend/server_test.go` (extended
`TestServer_CreateVM_WithNetworkAndFirewallRules` to assert the parsed
priority, plus a new
`TestServer_CreateVM_FirewallRuleBlankPriorityDefaultsToZero`). The
existing `TestServer_VMDetailPage` caught a real bug during this work:
`firewallRuleView` (the VM detail page's own display type) didn't get
a `Priority` field in the same edit as the proto messages, and the
template's new Priority column immediately produced a hard template-
execution error ("can't evaluate field Priority") that silently
truncated the entire rest of the page - caught by the test suite, not
manual inspection, fixed by adding the field.

`go build ./...`, `go vet ./...`, `gofmt -l`, `git diff --check`, and
the full `go test ./...` suite all pass. `buf generate` re-run for
both proto files. FreeBSD cross-compile confirmed for
`managerd`/`raftd`/`restshimd`.
