# ADR-0171: Delivering a prepared certificate over the join channel

## Status

**Proposed.** This record decides how a prepared certificate reaches the
Comb it was prepared for, and how the reservation that authorizes it is
bound to that Comb. It completes the delivery half of ADR-0169's
prepared-certificate workflow.

Supersedes: None
Superseded by: None
Tracking issue: #26 (the Cloudflare/Let's Encrypt bootstrap task)
Implements: ADR-0169 (the reservation), ADR-0147 Part 2 (the join
attestation), ADR-0170 (the trust transition that adopts the result)

## Context

ADR-0169 defines the reservation: a record that a certificate has been
prepared for a Comb that has not joined yet, and the rule that it is
deliverable only to the Comb the join attestation identifies. This
record decides HOW the delivery happens.

The mechanism already exists, in the form ADR-0147 Part 2 built for the
Colony's peer API key and the restart-guardrail token:

  - `GetJoinRequestStatus` is the joining Comb's poll. It is
    unauthenticated, and it carries the joiner's own serving
    certificate as `recipient_tls_certificate_pem`.

  - `sealApprovedJoinCredentials` checks that certificate's fingerprint
    against the fingerprint RECORDED for the join request, requires the
    connection to be TLS, and returns an envelope SEALED TO THAT
    CERTIFICATE. The plaintext bundle carries the peer API key and the
    guardrail token; the envelope carries neither in the clear, and
    neither value is ever in replicated state.

  - `apiaryctl join-credential-install` opens that envelope with the
    joining Comb's own private key, checks the bundle is bound to this
    request and this Comb, and installs the credentials.

A prepared certificate is exactly this shape of thing - a secret that
must travel to the joining Comb over the join channel and to nobody
else - so the delivery REUSES that machinery rather than inventing a
parallel path.

What is missing is the reservation's half: nothing today binds a
reservation to a join request, nothing decides that a reservation
belongs to a given Comb, and nothing carries the prepared pair.

## Decision

**The prepared pair is carried in a SEPARATE field of the join status
response, not in the existing credential bundle.** The existing bundle
is `joincredential.Bundle`, and it is versioned: adding a field to it
would mean every joiner that already understands version 1 would meet a
version 2 it does not. A separate field means the credential bundle's
contract is untouched, and a joiner that does not know about prepared
certificates simply never receives one.

**The delivery is sealed to the joiner's serving certificate, with
associated data that binds it to the reservation.** The seal and the
associated-data discipline are `internal/joincredential`'s existing
ones - there is one implementation of "sealed to a certificate" in this
tree, and a second would be a second thing to get subtly wrong. What
is new is the AD's content: the reservation ID, the join request ID,
the node ID and the certificate's fingerprint. A ciphertext copied
from one reservation's delivery therefore fails to decrypt under
another's, rather than decrypting into the wrong joiner's key.

**The reservation is bound to the join request by the Colony, at
approval time, and the binding is REPLICATED.** ADR-0169 says the
reservation must be bound to the joining Comb before it is
deliverable. The binding happens when the request is approved, because
approval is the moment the Colony has decided which Comb is joining -
and the binding must be replicated, because a reservation that lived
only in one managerd's memory would be lost if that managerd stepped
aside, and the joining Comb would be left holding a reservation it
could never collect.

**The binding is refused if the reservation names a different Comb.**
The reservation is prepared for a FQDN and, once bound, for a
`node_id`. The approval binds it to the request's `node_id`. A
reservation already bound to a different node is refused, and so is a
second binding. A prepared private key has two holders until the
joiner replaces it; making it three would defeat the point.

**The binding rides the APPROVAL's own log entry, and the approval is
REFUSED if it cannot be made.** This is the same reasoning the pin
promotion already uses: "approved" and "the preparation belongs to
this Comb" are one replicated fact, not two commands that could be
half-completed.

The refusal is the important half. An approval that succeeded without
binding the reservation would leave a preparation that nobody can
collect and nobody can clear - the operator would have approved a join
 believing a certificate was on its way, and it would not be.

**The reservation is identified by the operator's own identifier, and
the SEAL is what authorizes the collection.** The reservation ID is
carried by the joining Comb on its status poll, and the Colony checks it
against the reservation's own replicated binding. That binding says
which request and which Comb the preparation is for.

Presenting the identifier is NOT authority to collect anything on its
own. The envelope is sealed to the joining Comb's own serving
certificate, so the private key is recoverable only by the machine
whose identity the Colony already verified, and only from an envelope
produced for that reservation. An attacker who guessed a reservation ID
would get an envelope addressed to the real joiner's certificate, which
they cannot open.

This is deliberately the SAME shape as ADR-0147 Part 2's join
credentials: an identifier that says WHICH record, and a seal that says
WHO may collect it. A separate unguessable delivery TOKEN was
considered and rejected: the joiner already has to present something
unguessable - the join request's own first code - and the seal already
makes possession of the identifier insufficient. A second secret would
be a second thing to manage, rotate, and get wrong, for no additional
property.

**The delivery is refused unless the request is APPROVED.** A
reservation is deliverable to an approved joiner and to nobody else.
Before approval the join request is a proposal, and the Colony has not
yet decided this Comb is a member - so it has not yet decided this
Comb's certificate is a trust anchor either. The existing credential
delivery already has this rule, and the prepared certificate follows
it rather than inventing a second one.

**The joiner INSTALLS the pair atomically, and adopts it as its own
serving certificate.** The prepared pair is a real certificate: the
same `tls_cert`/`tls_key` files the Comb already serves from, written
through the same stage/fsync/rename discipline
`internal/origincert.WritePair` uses, so a failure leaves the previous
pair serving rather than a half-written one.

**Installing the prepared pair is NOT the trust transition.** Installing
a file and changing what the Colony trusts are different acts, and they
are separated on purpose. The trust transition is the Colony's
decision (ADR-0170), and it is a separate, separately-authorized act.
The joiner installing a certificate it was given does not make the
Colony trust it; the Colony's transition does.

**The joiner generates its OWN key afterwards, and obtains its own
replacement certificate.** This is the step that ends the two-holder
exposure ADR-0169 names. It is the joiner's own act, with its own
integration configuration, and it is what makes the eventual
transition (ADR-0170) a rotation rather than an adoption.

## Consequences

- Host 2 can be given a working certificate while it is still offline,
  and collect it during the join, with no manual copying of files and
  no operator typing a fingerprint on each host.
- The prepared private key is readable only by the Comb whose
  certificate the seal names, and only in a join that the Colony
  approved.
- The credential bundle's version 1 contract is unchanged, so a joiner
  built against the existing bundle is unaffected.
- The reservation's binding is replicated, so it survives a leadership
  change and a restart.
- The joiner must still do its own work afterwards: generate a key,
  obtain a replacement certificate, and have the Colony transition to
  it. The prepared mode buys the offline window, not the whole
  lifecycle.
- The delivery is refused for an unapproved request, for a wrong
  recipient, for a replayed or mismatched token, and for a reservation
  already bound elsewhere. Each of those refusals names itself.

## Alternatives and consequences

- **Put the prepared pair in the existing credential bundle.** Rejected:
  it would change a versioned contract that existing joiners already
  understand.
- **Deliver the certificate over the unauthenticated status poll in the
  clear.** Rejected: the poll is unauthenticated by design, and the
  prepared pair carries a private key.
- **Have the joiner ask the Cloudflare API itself for the certificate
  during the join.** Rejected: it requires the joiner to already hold
  the Cloudflare credential, which is the thing the offline
  preparation exists to avoid.
- **Bind the reservation at REQUEST time rather than approval time.**
  Rejected: at request time the Colony has not decided this Comb is
  joining, and a reservation bound to a request that is later rejected
  would be a reservation nobody can use and nobody can clear.
