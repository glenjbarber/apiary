# ADR-0172: A guided certificate setup walkthrough

## Status

**Proposed.** This record decides how an operator configures Cloudflare
and obtains a certificate for a Comb, in one guided flow that works on
a headless machine.

Supersedes: None
Superseded by: None
Tracking issue: #26 (the Cloudflare/Let's Encrypt bootstrap task)
Implements: the operator experience confirmed for
`apiary-cloudflare-acme-bootstrap`

## Context

The pieces exist. `internal/cloudflare` speaks Cloudflare's DNS API,
`internal/acme` is a DNS-01 issuer against a real ACME server,
`internal/origincert` owns the issuance-and-install transaction,
`internal/hostcert` owns the evaluation gate, and the Machine page has
an ACME panel that issues a certificate for a name the operator types.

What does not exist is the guided flow the confirmed operator experience
describes:

  1. Configure Cloudflare once on the first Comb, through a menu-style
     walkthrough.
  2. Obtain a Let's Encrypt certificate for that Comb.
  3. Prepare certificates for additional node names or subdomains,
     reusing the same integration.
  4. Reuse the integration when another node joins.

And two capabilities the flow needs, neither of which exists:

  - **TOKEN VALIDATION.** The walkthrough must "validate the token
    against the required operations and zone" and "report missing
    permissions precisely". Nothing in `internal/cloudflare` can do
    that today: there is no call that checks what a token may do, and
    no call that resolves a zone name to a zone ID.

  - **MASKED CREDENTIAL ENTRY.** A Cloudflare token must never reach a
    command line, a URL, a log, or a shell history. The walkthrough
    reads it from a terminal without echoing it.

## Decision

**The walkthrough is a command in `apiaryctl`, not a page.** ADR-0136
says `apiaryctl` exists for exactly this: an operator must be able to
type the thing on a Comb that has no browser and no checkout. A
joining Comb is headless, and the first node of a Colony may be too. A
guided flow that only exists in the web UI is a guided flow the
operator cannot reach in the situation it was designed for.

The Machine page's existing ACME panel stays, and stays a page. It
remains the right surface for a browser at a desk; the walkthrough is
the one that works from a console.

**The walkthrough is MENU-DRIVEN, with a non-interactive form for
automation.** Every step prompts, and every step accepts a flag that
answers it, so the same code path serves both a human at a console and
a script. A command that only works interactively cannot be tested,
and a command that only works with flags is not a walkthrough.

**The credential is read from the terminal without echo, and written to
a file that is 0600 and root-owned.** It is never an argument, never in
an environment variable the operator exported (because `service` runs
with an empty environment, and a token in the operator's shell is a
token in their shell history), and never printed. The walkthrough
reports the token's *permissions*, not the token.

**The token is validated against the zone BEFORE anything is issued.**
Validation is a read: it resolves the zone, and confirms the token may
edit DNS in it. A token that cannot do the thing the walkthrough needs
is refused with the specific missing permission named, because "your
token is wrong" sends the operator to the wrong place and "your token
cannot edit DNS in this zone" does not.

**Issuance is separate from any persistent DNS change.** The walkthrough
may optionally create an A, AAAA or CNAME record for the name, but
only after the operator explicitly selects it and sees exactly what
will be created. It never silently exposes a service, and it never
enables the Cloudflare proxy: a proxied record changes how the world
reaches the name, and that is the operator's decision to make
explicitly.

**The allowed naming suffix is part of the saved integration.** The
integration is what lets a later "prepare another node" reuse the same
token and zone. The suffix is what stops that reuse from issuing a
certificate for a name the operator never meant to give this Colony.

**A prepared certificate for another Comb is recorded as a
reservation, and the reservation is what the join flow binds.** This is
ADR-0169's record and ADR-0171's delivery, and the walkthrough is
where it is created. The walkthrough does not itself deliver
anything: the delivery happens over the join channel, to the Comb that
proves it is the one the reservation was prepared for.

**The walkthrough never prints or logs a credential.** The token, the
ACME account key and the certificate private key are distinct secrets
with distinct custody, and none of them appears in the output.

## Consequences

- An operator can configure Cloudflare and obtain a certificate on a
  headless Comb, from a root shell, with no browser and no checkout.
- The token is validated before an issuance is attempted, so a
  mis-scoped token produces a precise message rather than a failed ACME
  order.
- The saved integration makes "prepare another node" a single command
  rather than a re-entry of the same credential.
- The naming suffix is enforced at preparation time, so a typo in a
  name is refused before a certificate is requested.
- The walkthrough adds a Cloudflare zone-read and a token-verify call
  to `internal/cloudflare`, which are read-only and are the smallest
  addition that makes the flow checkable before it acts.
- `apiaryctl` grows a command group. ADR-0136's own scope rule says a
  command that is present and wrong is worse than one that is absent,
  so this group is deliberately one command with sub-steps rather than
  a family of flags.

## Alternatives and consequences

- **A web page.** Rejected as the primary path: a joining Comb is
  headless, and the first node of a Colony may be too.
- **Flags only, no prompts.** Rejected: that is a configuration
  command, not a walkthrough, and the whole point of the confirmed
  experience is that the operator is carried through the steps.
- **Issue without validating first.** Rejected: the failure mode is an
  ACME order that fails partway, with a TXT record left behind.
- **Store the token in managerd.json.** Rejected: that file is
  replicated-adjacent configuration that gets rendered, copied and
  backed up. A credential belongs in a 0600 file that only the
  issuance path reads.
- **Let the walkthrough create the DNS record by default.** Rejected:
  a persistent DNS change and a proxied record are both visible
  consequences, and both belong to an explicit operator choice.
