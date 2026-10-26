# ADR-0176: The UI redesign's remaining work

## Status

**Proposed.** This record states what of the Web UI redesign is
actually in the tree, what is not, and what the remaining work is.

Supersedes: None
Superseded by: None
Tracking issue: none (ADR-0144's redesign, continued)
Amends: ADR-0144 (the redesign's status line), ADR-0059 (the CombTree
sidebar)

## Context

ADR-0144's status line says the redesign is "Accepted and
implemented. All six sections (A-F) are in `main`." The spec document
`docs/web-ui-redesign.md` says something different at its own top:
"**Proposed** - first checkpoint (this document) awaiting review before
any code changes."

Both statements are partly true, and the difference between them is
worth making precise, because a status line that overstates what is
built is exactly the kind of thing that sends the next person to look
for something that was never built.

Checked against the tree, section by section:

| Section | What it promised | State |
| --- | --- | --- |
| A | Design-system primitives, the tokens, and the `CombTree` sidebar | Primitives and tokens are in the tree. **The `CombTree` is not** - the sidebar still renders a flat list of Comb IDs. |
| B | A multi-pane, server-driven creation wizard | **Not in the tree.** `create_guided.html` is still the single-page implementation with its step-toggle JS. |
| C | The dashboard `HealthCard` and per-node gauges | In the tree. |
| D | Image-kind labels and bridge-health chips | In the tree. |
| E | Service-status and role chips | In the tree. |
| F | Verdict chips on the operational pages | In the tree. |

Two things are worth recording about Section B, because they change
what "not done" means.

**The one documented gap in Section B IS closed.**
`internal/frontend/guided_create.go`'s `placementUnavailable` had a doc
comment saying it was "the one piece of placement evidence the wizard
turns into a hard control rather than a line of prose" - while the
template only ever used it inside a descriptive `<li>`. The template
now applies it as `disabled` on the owner-node `<option>` itself
(`create_guided.html:112`), which was the gap. So the rule is enforced
where it matters, even though the wizard's SHAPE is not what the spec
described.

**The wizard's step JS still restates the server's rules.** The
script's own comment says "the server is the authority - if the two
ever disagree, the server wins and says so - but the point of
restating them here is that an operator should never have to submit
something only to be told it was impossible."

That is a reasonable intent, and it is also exactly the shape the spec
says to delete: a second copy of the rules, in a different language, on
a different side of the wire. The spec's argument against it is the
one that matters - "client JS mirroring server rules is exactly how the
placement-pane gap happened".

## Decision

**The status lines are corrected to describe the tree.** ADR-0144's
status line and the spec's own status line are brought into
agreement with each other and with what is built. A redesign that is
"Accepted and implemented" in one document and "awaiting review" in
the next is not a record anyone can rely on.

**The remaining work is TWO pieces, and they are done together.**

**1. The `CombTree` sidebar.** The sidebar's Colony section becomes a
tree: Colony -> each Comb -> that Comb's owned cells, with a
`StateChip` per Comb and a link to `/host/{id}`.

ADR-0059's Follow-up removed Comb-as-sidebar-item because it had "no
separate destination or useful operator action". `/host/{id}` now
exists and did not when that Follow-up was written, so the objection no
longer applies - and ADR-0144 already records this as a superseding
decision. This change is what makes the tree honest rather than
decorative.

The per-Comb health comes from the SAME evidence the Command Center
already gathers - `clusterNodeEvidence` - rather than a second read.
The sidebar and the Command Center must not be able to disagree about
a Comb's state, and two fetches are two moments.

**2. The creation wizard.** `create_guided.html`'s single-page
implementation and its step-toggle JS are replaced by a pane-based
wizard whose panes are rendered from the same Go data the final POST
validates against, so the server is the only place that decides what a
pane may offer.

**The wizard's panes POST to the SAME `/vms` and `/jails` endpoints.**
The spec is explicit that `internal/frontend/create_guided.go`'s routing
is kept verbatim, and the panes are a framing change over an unchanged
submit path. That is what keeps this a redesign rather than a rewrite.

**The rules the JS restated are DELETED, not ported.** The client's
copies of the server's rules go away with the step-toggle JS. Where an
operator needs to know why an option is unavailable, the server says
why - in the option's own `title` - rather than the browser
re-deriving it.

**A Review pane is added.** It renders every answer from every prior
pane in one read-back table before the real submit button. It costs
nothing architecturally and it directly serves "the state of the
system should be obvious": an operator currently commits to `Create VM`
having scrolled past fields that may have changed underneath them.

## Consequences

- The redesign's two status documents agree with each other and with
  the tree.
- The sidebar shows a Comb's state and the cells it owns, so an
  operator can find where a workload lives without leaving the page.
- The wizard offers only valid options, because the server decides -
  the class of bug the placement gap was is structurally impossible
  rather than fixed once.
- The client's copy of the server's rules is gone, so the two cannot
  drift.
- The wizard's framing changes but its submit path does not, so the
  validation rules and their tests are untouched.
- The preview harness remains the way each piece is verified
  visually, which is this project's own convention.

## Alternatives and consequences

- **Declare the redesign done and move on.** Rejected: two named
  deliverables are absent, and the status lines would say otherwise.
- **Port the step JS into the new wizard.** Rejected: it would carry
  the duplicate rules forward, which is the thing the spec identifies
  as the cause of the placement gap.
- **Give the sidebar its own health read.** Rejected: the sidebar and
  the Command Center would then be able to disagree about a Comb's
  state, and two reads are two moments.
