# ADR-0144: Web UI redesign - operator legibility and a real creation wizard

## Status

Accepted and implemented. All six sections (A-F) are in `main`, each
delivered as its own commit. A is the design-system primitives
(`internal/frontend/gauge.go`, `internal/frontend/health_card.go`,
`web/templates/_gauge.html`, `web/templates/_health_card.html`, the
six `--status-*` tokens and the Comb tree in
`web/templates/layout.html`). B is the creation wizard
(`web/templates/create_guided.html`), with `placementUnavailable`
applied as `disabled` on the owner-node `<option>` itself, which is
the gap this ADR was written to close. C is the dashboard
`HealthCard` and per-node gauges (`web/templates/cluster_overview.html`,
`internal/frontend/cluster_overview.go`). D is the image-kind labels
and bridge-health chips (`web/templates/images.html`,
`web/templates/networks.html`). E is the service-status and role chips
(`web/templates/machine.html`, `web/templates/users.html`,
`web/templates/apikeys.html`). F is the verdict chips
(`web/templates/simulate.html`,
`web/templates/assumption_register.html`,
`web/templates/recovery_handbook.html`).

Two points where the code settled differently from the design text
below, each recorded in its own delivery commit rather than here.
Section B's panes are not HTMX round-trips: the Placement pane
computes the server's verdict for both workload kinds once and
serializes it as `data-unavailable-vm` and `data-unavailable-jail`, so
the browser copies a precomputed answer and never derives a rule of
its own. Section C's per-node gauge is a CPU and Memory pair, not a
trio, because `clusterNodeView` carries a boolean pool rollup rather
than a per-node disk-capacity percentage to draw a third arc from.

A third point, about the vocabulary itself. The design text below
specifies seven states, and the tokens for six of them -
`--status-ok`, `--status-warn`, `--status-critical`, `--status-unknown`,
`--status-not-applicable` and `--status-stale` - are all defined in
`web/templates/layout.html:35-37` and specified in
`docs/web-ui-redesign.md:116-121`. The seventh, `contradictory`, has no
token of its own. It is not missing: `internal/health` produces it
(`StatusContradictory`, for a reachable managerd whose own report says
its raft is down, and for a voter marked unreachable),
`internal/frontend/health_card.go` maps it to the `contradictory` state,
and `.badge.contradictory` renders in `web/templates/layout.html:216` -
borrowing `--status-critical` rather than defining its own. So the state
is delivered and visible; what is owed is the token, which is a naming
debt rather than a behaviour one.

The design sections below are unchanged; this status line is the only
edit.

## Context

The user asked for a complete revamp of the web UI, in two explicit
parts:

1. Usability focused on what an administrator/operator needs to know - the state of the system should be obvious.
2. VM and jail creation should be a real Setup Wizard: an answer given
   in one pane must never let a later pane offer an option that answer
   already made invalid.

The second point turned out to have a concrete, already-documented gap
rather than being a hypothetical concern. `internal/frontend/create_guided.go`'s
`placementUnavailable` function carries a doc comment stating it is
"the one piece of placement evidence the wizard turns into a hard
control rather than a line of prose" - but `create_guided.html` only
ever uses that function inside a descriptive `<li>`, never as
`disabled` on the owner-node `<option>` itself. An operator can select
a Comb reporting no bhyve provisioning as a VM's owner node today, and
finds out only from a small text list above the dropdown, or later from
a server-side rejection. Every *other* rule in that same file (image
role mismatches, HAST/clone exclusions, jail-vs-VM field crossover) is
already correctly enforced both server-side and in the rendered
control. This one rule's intent and its wiring simply diverged.

The first point (state should be obvious) is not a gap in the same
sense - this project already has a large, deliberate evidence model
(`internal/health`, ADR-0122's evidence-aware health API, replica
freshness, the September 27 "Command center" aesthetics work) - but it
computes more honest state than the UI currently surfaces at a glance,
and a September 27 UI aesthetics pass already established a token/
component system this redesign should extend rather than replace.

The user's own reference material named two additional constraints
that materially shape the design: no new JS dependency and no build
step (server-rendered Go `html/template` plus HTMX stays the stack),
and a visual direction combining a control-panel/gauge feel with a
tree-style sidebar and a summary health card - left to this ADR's
judgment on how the two combine within the existing yellow/black brand
(ADR-0059), rather than a full re-skin.

## Decision

Adopt the companion spec, [`docs/web-ui-redesign.md`](../web-ui-redesign.md),
as the implementation plan, delivered in six phased sections (A-F),
each its own review checkpoint:

- **A - Design system foundation**: new tokens, a `Gauge` and
  `HealthCard` primitive, and a tree-style Colony sidebar.
- **B - VM/Jail creation wizard**: replaces `create_guided.html` with a
  server-driven, multi-pane wizard. Each pane is rendered from an HTMX
  round-trip against the *same* Go data the final submit validates
  against (`placementHiveView`, `guidedImageOptions`), so an
  incompatible option is never sent to the browser at all - a stronger
  guarantee than a disabled control, and the direct fix for this ADR's
  motivating gap.
- **C - Dashboard / Command Center**: `HealthCard` and per-node `Gauge`
  trios added to the existing cockpit layout from the aesthetics pass.
- **D - Resource pages**: VMs, Jails, Networks, Images get the
  formalized state vocabulary (below); no information-architecture
  change.
- **E - Machine configuration & admin**: visual consistency only; no IA
  change, since ADR-0104/0120's provenance system already solves that
  page's legibility problem.
- **F - Operational/evidence pages**: visual consistency only; these
  pages (Why Not, Invariants, Simulate, Recovery Handbook, etc.) already
  serve exactly the "state should be obvious" goal within their own
  scope.

A single, formalized seven-state vocabulary - `ok`, `warn`, `critical`,
`unknown`, `not-applicable`, `stale`, `contradictory` - replaces the
current ad hoc badge-class lists repeated per template. `unknown` and
`not-applicable` are kept explicitly distinct (a failed check is not
the same fact as an inapplicable one), matching what this codebase's
own evidence model already computes; this ADR only gives it one
documented home.

No RPC, proto, or database-of-record change. The wizard's panes submit
to the existing `/vms` / `/jails` endpoints; `CreateVM`/`CreateJail`
keep their existing `RoleOperator` gate.

### Superseding ADR-0059's Follow-up

ADR-0059's Follow-up section removed Comb from the sidebar, reasoning
that it "adds an abstract layer between Hives and Cells without
providing a separate destination or useful operator action." That was
true when it was written. `/host/{id}` (a per-Comb evidence page) now
exists and gives a Comb entry a genuine destination, so this ADR
reintroduces Comb as a real sidebar tree node linking there. ADR-0059's
other decisions (the shell grid, the responsive drawer, the settings
menu, the theme control) are unaffected and stay as they are.

## Consequences

- Six review checkpoints instead of one large change - slower to land
  in full, but each section is independently useful and independently
  revertible.
- The wizard becomes structurally unable to repeat the placement-pane
  class of bug: a rule that exists in Go and not in the template is no
  longer possible, because the template no longer carries its own copy
  of any gating rule to fall out of sync.
- The existing September 27 aesthetics-pass CSS token set is extended,
  not replaced; none of that work is at risk of regression from this
  ADR.
- `create_guided.html` and its ~260 lines of step-toggle JS are deleted
  in Section B; every validation rule it currently enforces
  (`internal/frontend/guided_create.go`) is kept verbatim and re-used by
  the new wizard panes, not re-derived.
- No new operational surface (no new daemon, no new listener, no new
  config file) - this is templates, one small Go helper (`gauge.go`),
  and CSS.

## References

- Companion spec: [`docs/web-ui-redesign.md`](../web-ui-redesign.md)
- [ADR-0059](0059-hierarchical-sidebar-shell.md) - sidebar shell,
  partially superseded above
- [ADR-0046](0046-vm-table-polling-corruption-from-oob-swap.md) - `<tbody>`-only polling constraint, carried forward unchanged
- [ADR-0104](0104-local-service-endpoint-configuration.md),
  [ADR-0120](0120-config-rationale-history.md) - Machine page provenance
  system, why Section E is visual-only
- [ADR-0127](0127-sylve-io-features.md) - the Sylve feature-adoption
  research this task's visual reference points at; that ADR is about
  backend capabilities (migration, replication, backup, firewall,
  WireGuard) and is explicitly out of scope here, which this ADR notes
  to prevent the two being conflated
- `internal/frontend/create_guided.go`, `internal/frontend/placement.go`
  - the existing rule set this redesign's wizard section reuses
