# Apiary Web UI Redesign - Implementation Spec

Companion document to [ADR-0144](adr/0144-web-ui-redesign.md), which
records the decision this spec implements. Read that first for the
"why"; this document is the "how," at the level of detail a build
session needs without re-deriving it from the templates each time.

Status: **Proposed** - first checkpoint (this document) awaiting
review before any code changes. Sections 4 and 6 double as the phased
delivery plan: each lettered section (A-F) is one review checkpoint.

## Ground truth this spec is built from

Everything below is checked against the actual code in this worktree,
not assumed. In particular:

- **`create_guided.html` already has real server-side rule enforcement**
  (`internal/frontend/guided_create.go`) for every field-combination
  rule - image role mismatches, jail-vs-VM field crossover, HAST/clone
  mutual exclusions. Those rules are correct and stay exactly as they
  are; nothing here re-derives or second-guesses them.
- **One real, documented gap justifies the whole "pane 1 must not let
  pane 2 offer an invalid option" ask**: `placementUnavailable()`
  (`internal/frontend/create_guided.go:126`) has a doc comment stating
  it is "the one piece of placement evidence the wizard turns into a
  hard control rather than a line of prose" - but the template
  (`create_guided.html:76`) only ever uses it inside a descriptive
  `<li>`, never as `disabled` on the owner-node `<option>` itself. The
  intent was already there; the wiring was not. Image-role pickers
  (`guidedImageOptions`) *do* wire `Available` to `disabled` correctly
  - so the gap is specifically the placement step, not a systemic
  pattern. This spec's wizard section closes that gap and, by
  construction, prevents its recurrence anywhere else.
- **ADR-0046 is a hard constraint, not a preference**: any HTMX
  fragment that swaps into a polled `<tbody>` (`/vms/rows`,
  `/jails/panel`) must begin with `<tr>`, never a leading `<div>` - htmx's tag-sniffing silently corrupts the swap otherwise. Every new
  polled fragment in this spec is designed around that.
- **ADR-0059 already made the "why not a flat nav" case once**; this
  spec's sidebar change (Section 3) explicitly revisits its Follow-up
  section rather than ignoring it - see Section 6.

---

## 1. Component inventory

Existing components from the September 2026 aesthetics pass
(`8406934`..`a146f9b`) are **kept, not replaced** - this redesign
extends that token/utility set rather than starting over. New or
substantially changed components only:

| Component | Purpose | Fragment / file | Key classes |
|---|---|---|---|
| `HealthCard` | Top-of-page, single-glance Colony state: reachable count, degraded/contradictory count, active guardrail holds, pending join requests. Replaces the KPI strip's "Evidence-backed" / "Preflight" labels (informational-only) with real numbers. | `web/templates/_health_card.html` (new, included by `cluster_overview.html` and `layout.html`'s header on every page) | `.health-card`, `.health-card-stat`, `.health-card-cause` |
| `Gauge` | Radial CPU/Mem/Disk-capacity indicator, inline SVG, no JS. Value and color computed server-side (`internal/frontend/gauge.go`, new) from the same `HostStatsResponse` fields `host.html` already reads. | `web/templates/_gauge.html` (new partial, `{{template "gauge" ...}}`) | `.gauge`, `.gauge-track`, `.gauge-fill`, `.gauge-label` |
| `CombTree` | Sidebar's Colony section becomes a real tree: Colony -> each Comb (linking to the existing `/host/{id}` evidence page, not a dead abstraction) -> that Comb's owned Cells. Answers ADR-0059's Follow-up objection by giving the Comb level a genuine destination. | `layout.html`'s `nav` block | `.sidebar-tree.combs`, `.sidebar-comb`, `.sidebar-comb-state` |
| `WizardShell` | The step container for guided create: a labelled progress rail (not a numbered "Step X of 5" sentence), one pane visible at a time, Back/Next controls. | `web/templates/wizard_*.html` (new, replaces `create_guided.html`) | `.wizard`, `.wizard-rail`, `.wizard-pane`, `.wizard-nav` |
| `OptionCard` | A bigger, screenshot-friendly radio control for mutually-exclusive, few-valued choices (workload kind; disk-seed method: base image / clone / blank). Replaces a bare `<select>` where there are <=4 options and the choice reshapes the rest of the form. | inline in wizard pane templates | `.option-card`, `.option-card input[type=radio]` |
| `StateChip` | Formalizes the existing `.badge` family into a documented vocabulary (below) so every new page uses the same seven states instead of inventing new badge classes. | (CSS-only; no new markup shape) | `.badge.<state>` - already exists, this spec adds the missing states and documents all of them in one place (Section 3) |
| `EvidenceNote` | The "why" line under a stat or state (already used ad hoc as `.cockpit-node-cause`, `.field-help#*-cue`) - same idea, one class, used everywhere a number or state needs its provenance one line below it. | n/a (CSS-only) | `.evidence-note` |

Kept as-is, reused throughout: `.card-grid`, `.detail-grid`,
`.detail-list`, `.confirmation-code`, `.resource-table` /
`.table-scroll`, `.section-nav` (pill tabs), `.panel`, all button/badge
tokens from the aesthetics pass.

---

## 2. Layout system

**Grid** (`ADR-0059`, kept): `.app-layout` stays a 3-region CSS grid - sticky header, sticky sidebar, scrollable content + footer. No change
to the mechanism, only to what the sidebar contains (Section 1,
`CombTree`) and what sits above the fold on `/` (Section 4A).

**Breakpoints** (kept, named as tokens instead of repeated magic
numbers):

```css
--bp-compact: 620px;   /* header collapses, single-column forms  */
--bp-drawer:  900px;   /* sidebar becomes a dismissible drawer   */
--bp-wide:    1200px;  /* new: dashboard goes 3-column above this */
```

**Spacing scale** (new - replaces ad hoc `.35rem`/`.55rem`/`.85rem`
literals sprinkled through every template with a named scale used
everywhere new code is written; existing rules are not mass-edited to
match, to keep this reviewable in the sections below rather than one
enormous whitespace-only diff):

```css
--space-1: .25rem; --space-2: .5rem; --space-3: .75rem;
--space-4: 1rem;   --space-5: 1.5rem; --space-6: 2rem; --space-7: 3rem;
```

**Container widths**: `.content` keeps its existing `max-width:1500px`.
The wizard gets its own narrower measure - `.wizard { max-width: 42rem }` - because a form read top-to-bottom is the one layout in this app that
benefits from a narrow column instead of the wide, dense tables
everywhere else.

---

## 3. Color / design tokens

All existing tokens from `layout.html`'s `:root` / `:root[data-theme="dark"]`
blocks are kept verbatim (`--ink`, `--muted`, `--line`, `--panel`,
`--wash`, `--yellow`, `--danger`, `--success`, `--link`, `--link-hover`,
`--focus`, `--sidebar*`, `--table-head`, `--row-hover`, `--field`,
`--shadow`). New tokens, added to both the light `:root` block and the
`:root[data-theme="dark"]` block (never only one - see the
`artifact-design` skill's own rule about this, which this project's
existing dual-theme CSS already follows correctly):

```css
/* Status vocabulary - seven states, used by StateChip / .badge and by
   Gauge fill color. "not-applicable" and "unknown" are DIFFERENT
   states (see Section 3.1) and must never collapse to the same color. */
--status-ok:            var(--success);
--status-warn:          #a5760a;   /* dark-mode-safe amber, distinct from --yellow (brand accent, reserved for active-nav/CTA) */
--status-critical:      var(--danger);
--status-unknown:       var(--muted);
--status-not-applicable:#6685ef;   /* already used by .badge.not-applicable - promoted to a token */
--status-stale:         var(--muted); /* paired with the existing dashed-border .badge.stale treatment */

--gauge-track: var(--line);
```

### 3.1 The status vocabulary (formalized, not new policy)

This is the exact set the codebase's own evidence model already
computes (`internal/health`, `comb_evidence_cause_test.go`,
`replica_freshness.go`) - this section only gives it one documented
home instead of five ad hoc badge-class lists:

| State | Meaning | Never confuse with |
|---|---|---|
| `ok` / `ready` / `healthy` / `up` | Observed and good. | - |
| `warn` / `degraded` / `soon` / `pending` | Observed, needs attention. | `critical` - this is not yet a failure |
| `critical` / `error` / `blocked` / `expired` / `deleting` | Observed and bad. | `unknown` - this is a real, confirmed problem |
| `unknown` | **The check did not complete.** File missing, probe failed, no signal. | `not-applicable` - unknown is a gap, not a design choice |
| `not-applicable` | **The check does not apply here.** No reconciler configured, no HAST replica requested. | `unknown` - this is not a gap, it's correctly nothing |
| `stale` | Was known, is aging past its freshness window. | `unknown` - stale data is worse than no data, and must look different |
| `contradictory` | Two evidence sources disagree. | any single-state badge - this must always read as its own alarming thing, never as a shade of `warn` |

This table is the answer to "the state of the system should be
obvious": every status surface in this redesign (Gauge fill, StateChip,
HealthCard counts) draws from exactly these seven, and a page that
cannot honestly compute one of them must show `unknown`, never guess.

---

## 4. Page-level specs

Grouped into the six delivery sections. Each page lists: what changes,
data already available vs. new, HTMX shape, and permission gating
(`manager.RoleViewer` / `RoleOperator` / `RoleAdmin`, ADR/RPC-verified
against `internal/manager/auth.go`'s `requiredRole` table, not guessed).

### Section A - Design system foundation

Everything downstream depends on this landing first.

- **`layout.html`**: add the Section 2/3 tokens; add `CombTree` to the
  sidebar's Colony group, replacing the flat "Combs" link. Each Comb
  row shows a `StateChip` (from the same per-node health the Command
  Center already fetches) and links to `/host/{id}` (existing route - ADR-0059's Follow-up removed Comb-as-sidebar-item specifically
  because it had "no separate destination"; `/host/{id}` is a real,
  already-shipped destination, so that objection no longer applies).
  Cells nest under their owning Comb exactly as today.
- **`_gauge.html`**, **`_health_card.html`** (new partials): built and
  unit-rendered against fixture data (Section 7) but not wired into a
  real page yet - that happens in Sections C/D. This keeps Section A
  reviewable as "the primitives," not "the primitives plus their first
  three call sites."
- **No RPC/handler changes.** Pure templates + CSS + one small Go
  helper (`internal/frontend/gauge.go`: `func gaugePercent(used, total uint64) gaugeView`)
  covered by a table-driven unit test (0%, 50%, 100%, `total==0`).

Permission gating: none - the shell renders for every role, exactly as
today.

### Section B - VM/Jail creation wizard

The concrete example from the request. Replaces `create_guided.html`
with a real multi-pane wizard, **server-driven**: see Section 6 for why
this beats client-side JS gating, and Section 5 for the HTMX mechanics.

Panes (renamed from "Step N of 5" to named panes, matching
`WizardShell`'s rail):

1. **Kind** - `OptionCard` x 2 (Virtual Machine / FreeBSD Jail). No
   server round-trip needed; this alone decides the rail's remaining
   labels.
2. **Identity** - Hostname -> ID/Display name derivation
   (`deriveResourceID`, kept verbatim - it already has a pinned Go/JS
   parity test). Unchanged fields, new pane framing.
3. **Placement** - Owner node, Replica node. **This is the pane that
   closes the real gap.** The owner-node control is rendered
   server-side from `currentPlacementHives` with `placementUnavailable`
   applied as `disabled` **on the `<option>` itself**, not only in the
   descriptive list (which stays, for the reachability caveat that a
   disabled control can't express - "unreachable, not evidence either
   way"). The replica-node control's same-as-owner exclusion
   (currently JS-only in `refreshPlacement()`) moves server-side too:
   submitting Placement re-renders Storage with the replica list
   already excluding the chosen owner, so an invalid pairing is never
   offered, not merely caught on submit.
4. **Compute** (VM) / **Root filesystem** (Jail) - kind-specific pane,
   selected by step 1's answer; the other kind's pane is never sent to
   the browser at all (today both are sent and one is `hidden`).
5. **Storage & boot media** (VM only) - image pickers keep
   `guidedImageOptions`' existing `Available`/`disabled` logic
   (correct today) but now also incorporate the Placement pane's owner
   node: an image "available" in the abstract but requiring a
   cross-node fetch the reconciler doesn't do (clone source node
   mismatch - already a validated rule in `validateVMCreateForm`) is
   disabled here too, with the same sentence the validator would give,
   instead of only being caught after submit.
6. **Network** - unchanged rules (VNET needs a network), reframed as
   its own pane instead of "Step 5 of 5."
7. **Review** - new pane, did not exist before. Renders every answer
   from every prior pane in one read-back table before the real submit
   button, using `.detail-list`. This is the one pane that costs
   nothing architecturally (it's just Section 6's accumulated wizard
   state rendered read-only) and directly serves "the state of the
   system should be obvious" - an operator currently commits to
   `Create VM` having scrolled past fields that may have changed
   underneath them.

Data: no new RPCs. Every field already exists in `vmCreateForm` /
`jailCreateForm`; the wizard's job is sequencing and gating, not new
data.

Permission gating: unchanged - `RoleOperator` for `CreateVM`/`CreateJail`
(`internal/manager/auth.go:148,164`). The wizard route itself
(`GET /vms/new`, `GET /jails/new`) stays open to any authenticated role
so a Viewer can see *why* they can't create something, matching this
app's existing "explain, don't hide" convention (e.g. certificates.html's
`unknown` verdict explanation) - the final submit buttons render
disabled with a reason for a Viewer, not absent.

### Section C - Dashboard / Command Center

`cluster_overview.html`'s `cockpit-*` structure (built in the
aesthetics pass, September 27) is kept; this section is additive:

- `HealthCard` replaces the three-item KPI strip
  (`.cockpit-kpis`/`.cockpit-kpi`) with real counts: reachable /
  unreachable Combs, count in each of the seven states (Section 3.1),
  active restart-guardrail holds (`ClusterHealth`'s existing guardrail
  fields), pending join requests (already fetched, `.JoinRequests`).
- Each `cockpit-topology-node` card gains a small `Gauge` trio
  (CPU/Mem/Disk) alongside its existing reachability/health badges - this is the literal "radial gauge, control-panel feel" answer,
  applied where it earns its place: a fleet-overview card is exactly
  where three numbers-at-a-glance beat three more lines of text.
- No change to `/host/{id}`'s own gauges vs. today's plain numbers in
  this section - that's Section D (it's a resource-detail page, not
  the dashboard).

Data: `Gauge` reuses `HostStatsResponse.cpu/mem` already fetched per
node for the topology cards (`fetchHostStats`); no new RPC.

Permission gating: unchanged (dashboard is Viewer-readable; the Create
button and join-request approve/reject forms keep their existing
`CanOperate`/`CanAdmin` guards).

### Section D - Resource pages (VMs, Jails, Networks, Images)

- **`vms.html` / `vm_rows.html`**: table rows get a `StateChip` instead
  of the bare `<span class="badge">`; no structural change - the
  `<tbody>`-only polling contract (ADR-0046) is unchanged, since
  `StateChip` is a CSS class change on cells already inside the row,
  not a new wrapper element.
- **`vm.html`**: replace the plain CPU-load/Memory-used numbers in the
  Status/Compute cards with `Gauge` where a percentage exists
  (memory), keep plain numbers where a gauge would be misleading
  (vCPU count has no "capacity," so it stays a number). Firewall/
  Access/Lifecycle positions (already moved this session, `5463b54`/
  `71e5151`) are unchanged.
- **`jails.html` / `jail.html`**: same `StateChip` treatment; jails have
  no CPU/Mem numbers of their own (they share the Comb's), so no gauge
  is added here - adding one would fabricate a metric the jail does
  not have, which this app's own evidence-first philosophy forbids.
- **`networks.html`**: bridge-health-by-Comb table gets `StateChip`
  instead of the current `unknown`/plain-text cells.
- **`images.html`**: no functional change; gets the `imageKind`
  classification (Section "Ground truth," already computed server-side
  for the wizard) surfaced as a small type label per row, so an
  operator browsing images sees "installer image" / "raw disk image" /
  "userland archive" instead of a bare filename - reusing
  `classifyImageName`, not inventing a second classifier.

Permission gating: unchanged throughout this section.

### Section E - Machine configuration & admin (Users, API keys, Certificates)

`machine.html` / `machine_sections.html` (the largest templates, 975 +
461 lines) get the lightest touch of any section: their own
`.section-nav` pill tabs already shipped in the aesthetics pass. This
section's only change is applying `StateChip`/`EvidenceNote` to the
existing status cells (service running/boot-enabled, TLS cert
presence) for visual consistency with the rest of the redesign - no
information architecture change, because an operator editing Machine
config is doing a deliberate, infrequent task, not scanning for state,
and ADR-0104/0120's existing "why is this set" provenance system
already solves that page's own legibility problem.

`users.html` / `apikeys.html` / `certificates.html`: `StateChip` for
role badges and cert-expiry verdicts (already computed exactly to
Section 3.1's vocabulary - `certificates.go`'s `ok`/`soon`/`expired`/
`unknown`, see the earlier certificates screenshot); no structural
change.

Permission gating: unchanged (`RoleAdmin` for all of Machine/Users/API
keys, per existing `requiredRole` entries).

### Section F - Operational & evidence pages

`why_not.html`, `invariants.html`, `assumptions.html`,
`assumption_register.html`, `simulate.html`, `maintenance.html`,
`recovery_handbook.html`, `resilience_coverage.html`, `trace.html`.

These pages are explicitly **read-only, evidence-backed reference
tools** by their own page copy ("nothing here is ever changed") and
were already given a full pass in the September 27 aesthetics work.
This section is `StateChip` consistency only - no IA change, since
these pages already do exactly what "state should be obvious" asks for
within their own scope (that is their entire purpose). Lowest priority
of the six sections; reasonable to defer past the others if time runs
short.

Permission gating: unchanged (all Viewer-readable; action buttons where
present keep existing `CanOperate` guards).

---

## 5. Interaction patterns

**Loading states**: the app currently has **zero** `hx-indicator` usage - a slow request just leaves the old content on screen with no
feedback. Add one small, reusable pattern: `hx-indicator="#<id>-spinner"`
paired with a `.htmx-indicator` CSS rule (opacity/display toggle,
already htmx's own convention, zero new JS). Applied to: wizard
pane transitions, the Create submit button, and any Admin action that
takes >1s today (restart, snapshot restore) per this app's own
`RestartNodeService`/guardrail latency.

**Error handling**: keep the existing in-DOM `.banner-error`/
`.banner-success` pattern (`role="alert" aria-live="polite"`) rather
than introducing toasts - it is already accessible, already zero-JS,
and this app's own operators are reading one page at a time, not
managing a stream of ephemeral notifications. `HX-Trigger` response
headers are used only for the one thing DOM swaps can't do cleanly:
telling the wizard's rail (a sibling of the swapped pane) which pane
just became active, via a custom event
(`HX-Trigger: apiary-wizard-pane-changed`) a small inline `<script>`
in `WizardShell` listens for, matching the existing pattern of small,
targeted inline scripts already used per-page (`create_guided.html`'s
own step-toggle JS, `layout.html`'s theme/drawer JS) rather than one
global bundle.

**Polling intervals** (formalized from what already exists, not
changed): resource lists (`vm_rows`, `jail panel`) stay at the existing
3s (ADR-0016/0018/0046); nothing in this redesign adds a new poller - the dashboard's per-node data is fetched on page load only, matching
today, since polling four nodes' full `HostStats` every few seconds
from every open dashboard tab is a real cost this redesign should not
introduce silently. If live dashboard refresh is wanted later, it is
its own ADR with its own interval/backoff discussion, not a side effect
of a visual redesign.

**Confirmation dialogs**: keep the existing native
`onsubmit="return confirm('...')"` pattern (zero-dependency, already
used for snapshot restore/delete, join-request purge). Formalize the
wording convention only: name the resource by ID, state the concrete
irreversible consequence, never a bare "Are you sure?" - auditing the
existing confirm strings shows this is already followed everywhere
except nowhere it's missing, so this is documentation, not a code
change.

---

## 6. Migration notes

**Delete outright**: `create_guided.html`'s current single-page
implementation and its ~260 lines of step-toggle JS
(`create_guided.html:280`-`576`), replaced by the `WizardShell` +
per-pane fragments in Section B. `internal/frontend/create_guided.go`'s
routing (`guidedKind`, `createAction`) is **kept** - the wizard's panes
still POST to the same `/vms` / `/jails` endpoints Section 4B commits
to leaving unchanged.

**Kept verbatim** (do not re-derive, do not "improve" during this
redesign - each has its own test coverage this redesign must not
break): `deriveResourceID`, every rule in `validateVMCreateForm` /
`validateJailCreateForm`, `classifyImageName` and the three
`guidedImageRole` definitions, `placementUnavailable`'s *logic* (only
its template wiring changes).

**Why server-driven wizard panes, not richer client-side JS**: the
request's own constraints rule out a JS framework or build step, and
this app's own architecture already has a stronger tool for "pane 1's
answer must gate pane 2's options" than client JS ever provides: an
HTMX round-trip that re-renders pane 2 from the *same* Go data
(`placementHiveView`, `guidedImageOptions`) the final POST validates
against. Client JS mirroring server rules is exactly how the
placement-pane gap happened - a rule existed in one place
(`placementUnavailable`) and the template's own JS never learned about
it. Making the server the only place that decides what pane 2 can show
makes that class of bug structurally impossible, not just fixed once.

**ADR-0059 amendment**: this spec's `CombTree` sidebar (Section 1)
reopens ADR-0059's Follow-up, which removed Comb-as-sidebar-item
specifically because it had "no separate destination or useful
operator action." `/host/{id}` now exists and did not when that
Follow-up was written; ADR-0144 records this as a superseding decision
rather than silently contradicting the Follow-up. See ADR-0144's own
Consequences section.

**ADR-0046 carried forward unchanged**: no new `<tbody>`-polled
fragment in this spec ever prefixes row markup with a wrapper element;
`vm_rows.html`/the jail panel fragment structure is not touched by this
redesign beyond the `StateChip` class swap inside existing `<td>`s.

**CSS**: additive to the aesthetics-pass token set (Sections 2-3), not
a replacement. No token this redesign adds reuses a name already
defined by the September 27 work.

---

## 7. Test fixtures

The pattern already used all session (a throwaway, never-committed
`internal/frontend/zz_preview_test.go` gated on `APIARY_PREVIEW=1`,
serving `fakeClient`-backed data on a local port for screenshot
verification) is the right tool here too and should keep being used
per section, not replaced with a checked-in fixture file - this
project's own convention is that preview harnesses are throwaway.

What each section's fixture set needs to cover, concretely, so a
future preview session doesn't have to rediscover it:

- **Section A/C (dashboard, Gauge, HealthCard)**: four `ClusterNodeView`
  entries spanning all seven Section-3.1 states at once (at minimum:
  one `ok`, one `warn`, one `critical`, one `contradictory`, one
  `unreachable`/`ProbeError`, one `not-applicable` reconciler,
  one `stale`) plus a non-trivial `HostStatsResponse` per node (varied
  CPU load, memory used, at least one pool not `ONLINE`) so Gauge fill
  colors and HealthCard counts are visibly exercised, not all green.
- **Section B (wizard)**: `placementHiveView` set including at least
  one `ProbeError` node (must stay selectable), one `VMCapable:false`
  node (must render `disabled` on the VM path, selectable on the jail
  path if `JailCapable:true`), and one node capable of both - plus an
  image catalog with at least one entry of each `imageKind`
  (`.iso`, `.raw`, `.txz`, and one deliberately unrecognized extension
  to confirm `imageKindUnknown` stays selectable everywhere per its own
  doc comment).
- **Section D (resource pages)**: VM/Jail lists covering every
  `VMPhase`/`JailPhase` value at least once (`READY`, `CREATING`,
  `ERROR` with a real `PhaseError` string, `STOPPED`, `DELETING`), and
  at least one VM with `ReplicaNodeID` set to exercise the
  cell-recoverability path.
- **Section E (admin)**: at least one certificate in each of `ok`/
  `soon`/`expired`/`unknown`, and a role map covering all three roles
  logged in separately (as already done this session via
  `s.sessions.Create` directly, bypassing the login form) to verify
  `CanOperate`/`CanAdmin` gating renders correctly for each.
- **Section F**: reuse Section D's fixture set - these pages read the
  same underlying resources, just presented as evidence rather than as
  editable rows.
