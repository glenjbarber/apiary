# ADR-0177: The peer trust transition's operator surface

## Status

**Proposed.** This record decides how an operator SEES a Comb's pinned
certificate, how they STAGE a replacement for it, and how they finish
or abandon the change.

Supersedes: None
Superseded by: None
Tracking issue: #26 (the Cloudflare/Let's Encrypt bootstrap task)
Implements: ADR-0170 (the trust transition itself)

## Context

ADR-0170 built the transition: a new FSM command that stages an
incoming certificate beside a Comb's current one, an activation that
promotes it, and an abandonment that drops it. The three manager RPCs
exist and are Admin-tier.

**None of it is reachable by an operator.** The peer trust store is
not rendered anywhere in the web UI - a search for it in
`internal/frontend` returns nothing. So the transition can be driven by
a hand-written RPC, and nothing shows an operator that a transition is
staged, that it is waiting, or that it has lapsed.

That is the gap. A trust transition is a three-step act with a window
in the middle, and the middle is exactly where an operator needs to
look.

## Decision

**The trust store gets its own page: `/machine/trust`.** Not the
Certificates page, and not a panel inside the Machine page's existing
sections.

The Certificates page is the wrong home, and the reason is about scope
rather than space. That page is deliberately READ-ONLY, it is
deliberately ONE Comb's view - its own handler says so, because
`ListOriginCertificates` answers only for the managerd that receives it
and no peer-forwarding path exists. The peer trust store is the
opposite: it is COLONY-WIDE, replicated, and every Comb's view of it
is identical. Putting a colony-wide, writable surface on a page that
has gone out of its way to say it is a single Comb's read-only view
would be two contradictory claims on one page.

The Machine page's "TLS and security" section is the other candidate,
and it is the right KIND of place - it is where TLS is already
configured. But that section is about THIS Comb's configuration: its
certificate paths, its CA, its tokens. The trust store is about the
COLONY's view of its members. A section that configures one Comb and a
page that shows all of them are different scopes, and the page is
clearer.

**The page shows the store, and the store's state in three states.**
Each pinned Comb is listed with its current certificate's fingerprint
and expiry, and - when a transition is staged - the incoming
certificate beside it, with the overlap's end. A staged transition is
the case that matters most: it is the one where the Colony is trusting
TWO certificates for one Comb, and an operator who cannot see that
cannot reason about it.

**The operator STAGES a transition by supplying the incoming
certificate.** The certificate is not chosen from a list, because the
incoming certificate is a file on the Comb that will serve it - the
one produced by a renewal, or by adopting a prepared certificate. So
the operator supplies the path to it, and the server reads it, evaluates
it through the same gate a pin is evaluated through, and stages it.

**The operator ACTIVATES or ABANDONS a staged transition.** Both are
one click, both record a reason, and both are the acts that change
what the Colony trusts.

**Every write is Admin-tier, and every write records a reason.** The
transition's reason is replicated with it, so an incident review can
answer "why did this Comb's identity change" - which is the question
a trust transition actually raises.

**The page shows the overlap's end, and says what happens when it
passes.** A staged certificate that is never activated stops being
trusted when its overlap ends, and an operator should be able to see
that deadline rather than discover it.

## Consequences

- The trust store becomes visible, which it was not.
- A staged transition is visible, which is the case where the Colony
  is trusting two certificates for one Comb.
- The operator can complete or undo a transition without hand-writing
  an RPC, and both actions record why.
- The page is colony-wide and says so, where the Certificates page is
  one Comb's and says so - two pages, two honest scopes.
- The certificate the operator supplies is evaluated by the same gate
  that evaluates a pin, so an invalid certificate is refused before it
  is staged rather than at activation.

## Alternatives and consequences

- **Put the transition on the Certificates page.** Rejected: that page
  has deliberately declared itself read-only and single-Comb, and the
  trust store is neither.
- **Put it in the Machine page's TLS section.** Rejected: that section
  configures THIS Comb; the trust store is the Colony's view of all of
  them.
- **Choose the incoming certificate from a list of the Colony's
  certificates.** Rejected: the incoming certificate is not in the
  Colony's inventory yet - it is a file on the Comb that will serve
  it.
- **Auto-activate when the operator confirms.** Rejected: activation
  drops the old certificate's trust, and it should be a separate,
  separately-reasoned act.
