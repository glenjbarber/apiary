# ADR-0174: Provisioning the certificate integration to a joining Comb

## Status

**Proposed.** This record decides how the Cloudflare integration
configuration reaches a joining Comb, so that it can obtain and renew
certificates of its own without an operator typing a credential on
every host.

Supersedes: None
Superseded by: None
Tracking issue: #26 (the Cloudflare/Let's Encrypt bootstrap task)
Implements: ADR-0169 (the reservation), ADR-0171 (the delivery),
ADR-0172 (the walkthrough), ADR-0173 (the joiner's adoption)

## Context

ADR-0173 established that the joiner adopts a prepared certificate
and then has to obtain a **replacement** certificate of its own. That
replacement needs the Cloudflare integration: the zone-scoped API
token, the zone, the allowed naming suffix, and the ACME account's
settings.

Without that configuration the joiner is stuck on the prepared
certificate until its expiry, and the bootstrap key it shares with the
preparing Comb stays shared for the whole of that time. So the
configuration has to arrive.

The obvious place for it is the join channel, which already exists,
is already sealed to the joining Comb's own certificate, and already
carries the Colony's peer credentials. Reusing it means one place
where the joiner's identity is checked, and one envelope to get
right.

The credential itself is the hard part, and the reason is worth
stating plainly: **a zone-scoped Cloudflare token is a shared
secret.** A token that can edit DNS for a zone can, in effect, change
what every host in that zone resolves to. Handing it to every Comb
means every Comb is a compromise away from a DNS takeover of the
whole zone.

## Decision

**The integration is provisioned over the join channel, in the
existing sealed credential bundle.** The bundle is already sealed to
the joining Comb's certificate and bound to the request and the node,
so extending it adds no new channel and no new place for the joiner's
identity to be checked.

**The bundle's version is bumped, and a version-1 bundle is still
read.** The bundle is a versioned contract. A version that carries the
integration is version 2; a joiner that only understands version 1
reads what it understands and ignores the rest. That is what the
version field is for.

**The Cloudflare token is carried in the bundle, and it is the FIRST
thing the joiner writes and the FIRST thing it can refuse.** The
bundle arrives sealed; the joiner opens it and writes the token to a
0600 root-owned file before it does anything else with it. If the
joiner's configuration already names a different token, the install
refuses rather than replacing it - a Comb that already has an
integration keeps it.

**A joiner that has no integration provisioned gets NO token, and the
bundle simply carries none.** Most Combs do not issue certificates.
Provisioning a Cloudflare credential to every Comb in a Colony would
multiply the blast radius of a single compromise for no benefit, and
the token would sit unused on most hosts.

**Therefore the integration is provisioned only to a Comb that has
been PREPARED FOR.** The reservation is the request: a Comb that has
a prepared certificate waiting for it is a Comb that is going to need
to renew one. A Comb with no reservation gets no credential, and the
provisioning is a consequence of a preparation the operator already
asked for rather than a new thing to ask for.

**The token is scoped as narrowly as the operator can make it, and the
handbook says so.** A zone-scoped token with only DNS edit is the
narrowest useful scope. The consequence - that compromise of any
recipient can affect that token's authorized zone - is recorded rather
than hidden, and the operator is told it at the point of setup.

**The integration's PUBLIC half travels in the bundle too, and the
SECRET half is written to a file.** The public half - the zone, the
suffix, the ACME directory URL, the contact address - is what the
joiner's configuration needs to reference, and it is not secret. The
secret half is the token, and it is written where the issuance path
reads it.

**The provision is recorded, so an operator can see which Combs hold
the credential.** A credential that has been distributed is a
credential whose distribution is an audit question.

## Consequences

- A joining Comb with a prepared certificate can obtain and renew its
  own certificates without an operator typing a credential on it.
- The bootstrap key's shared window ends at the replacement, not at
  the prepared certificate's expiry.
- The Cloudflare token exists on more than one machine. That is the
  cost of the design, and it is bounded by the operator choosing a
  narrow token and by the provisioning happening only for Combs that
  were prepared for.
- The bundle's version becomes 2, and the joiner must handle both.
- A Combs that was prepared for but whose provision failed is
  detectable: the reservation is delivered and marked, and the Comb
  has no integration.
- The provisioned token can be rotated centrally: the operator updates
  the saved integration on the preparing Comb, and a re-provision
  replaces the token on the Combs that hold it.

## Alternatives and consequences

- **Every Comb gets the token.** Rejected: it multiplies the blast
  radius for the benefit of Combs that will never issue a
  certificate.
- **The operator types the token on each Comb.** Rejected: it is the
  manual step the whole prepared workflow exists to remove.
- **The preparing Comb keeps issuing on every Combs behalf.** Rejected:
  it makes the preparing Comb a certificate authority for the Colony,
  and it cannot produce a certificate for a Combs own ACME account.
- **Replicate the token through raft.** Rejected: a plaintext secret
  in the replicated log is exactly what ADR-0169 forbids, and the
  token is not needed on every Combs.
- **Hand the joiner a Cloudflare API token scoped to one hostname.**
  Rejected as the default: Cloudflare does not enforce a
  per-hostname scope, so a claim of one would be a claim Apiary
  cannot keep.
