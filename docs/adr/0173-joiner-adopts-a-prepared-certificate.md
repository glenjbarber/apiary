# ADR-0173: The joiner adopts a prepared certificate

## Status

**Proposed.** This record decides what the joining Comb does with a
prepared certificate it collects during the join: how it installs it,
what it does with the bootstrap key afterwards, and how the Colony
comes to trust it.

Supersedes: None
Superseded by: None
Tracking issue: #26 (the Cloudflare/Let's Encrypt bootstrap task)
Implements: ADR-0169 (the reservation), ADR-0171 (the delivery),
ADR-0170 (the trust transition)

## Context

The delivery half is built. ADR-0171's `GetJoinRequestStatus` carries
a prepared certificate sealed to the joining Comb's own serving
certificate, and the joiner already polls that endpoint to collect the
Colony's peer credentials.

What is missing is what the joiner does with it. The delivery hands
over a pair whose private key the PREPARING Comb generated, because
ACME never delivers one. The joiner must:

  - install the pair as its own serving certificate, so it serves the
    name the certificate covers, and
  - stop holding a key that two machines hold, by generating one of
    its own and obtaining its own replacement certificate.

And the Colony must come to trust the new identity, which is ADR-0170's
trust transition - a separate, separately-authorized act.

## Decision

**The prepared pair is installed as the joiner's OWN serving
certificate, at the paths this Comb already serves from.** The
`tls_cert` and `tls_key` in this Comb's own configuration name where
managerd, the frontend and the restshimd read their serving
certificate from. Installing the prepared pair there is what makes the
Comb actually serve the name the certificate covers.

**The installation is atomic and through the same discipline every
other certificate install uses.** `internal/origincert.WritePair`
stages, fsyncs and renames, so a failure leaves the previous pair
serving rather than a half-written one. The joiner's certificate is
never briefly mismatched.

**The prepared pair is installed BEFORE the joiner restarts anything,
and the restart happens only after the install succeeds.** The reverse
order would restart a Comb into a certificate it cannot serve.

**The joiner then GENERATES ITS OWN KEY AND OBTAINS ITS OWN
CERTIFICATE.** This is the step that ends the two-holder exposure
ADR-0169 names, and it is the joiner's own act rather than something
the preparing Comb does on its behalf. The joiner needs the
integration configuration - the Cloudflare token and the zone - to do
it, and that configuration is provisioned to it separately.

**The bootstrap key is retired after the replacement is in place, and
the retirement is confirmed.** The prepared key is deleted from the
joiner's disk only once the replacement certificate is installed and
verified. Deleting it earlier would leave the Comb unable to serve if
the replacement failed.

**The joiner reports the ACTIVE certificate's fingerprint after the
transition.** That is the value the Colony's trust transition needs,
and it is the value an operator comparing fingerprints wants to see.

**The trust transition remains the Colony's separate act.** Installing
a file and changing what the Colony trusts are different things. The
joiner installing a certificate it was given does not make the Colony
trust it; ADR-0170's transition does, and it is authorized by the
Colony.

**The joiner refuses to install a prepared certificate whose name
does not match what it is expecting to serve.** The reservation names
the FQDN, and the Comb's own configuration names the certificate
paths. A prepared pair for a different name would install cleanly and
then fail every handshake with a name mismatch, which is a much worse
place to discover it.

### Two decisions the implementation revealed

**The reserved NAME is bound into the envelope's associated data.**
The receiver checks that the certificate covers the reserved name, and
a check whose input the receiver supplied is not a check. The name
therefore joins the reservation, the request, the node and the
fingerprint in the associated data, so a delivery for one name cannot
be opened and believed to be for another. The associated data's version
tag is bumped, because what it authenticates changed: an envelope
sealed under the old rule does not open under the new one, which is the
correct outcome for a change to what the seal covers.

**The bootstrap key is NOT retired by this step, and cannot be.**
Retiring it requires the joiner to hold a REPLACEMENT certificate of
its own, and obtaining one requires the Cloudflare integration
configuration - which is provisioned to the joiner separately and does
not exist there yet. So this step installs the prepared pair and
reports the state honestly: the Comb is serving a working,
publicly-trusted certificate, and the preparing Comb still holds a
copy of the key until the replacement is obtained.

Deleting the bootstrap key here would leave the Comb unable to serve
at all if the replacement then failed, which is strictly worse than
the two-holder window this workflow accepts. The retirement is the
next step's work, and it happens only once a replacement is in place.

## Consequences

- A Comb that joined with a prepared certificate serves the name the
  certificate covers from its first handshake, rather than serving a
  self-signed certificate until an operator intervenes.
- The window in which two machines hold the bootstrap key is bounded
  by the join itself, and ends when the replacement certificate is
  installed.
- The joiner needs the Cloudflare integration configuration to obtain
  its own replacement certificate. That configuration is provisioned
  separately, and its absence leaves the joiner on the prepared
  certificate - which is a working certificate, not a broken one.
- The trust transition is still a separate authorized act, so a
  certificate can be installed on a Comb the Colony has not yet been
  told to trust.
- A failure at any step leaves the Comb with a working certificate:
  either the previous one or the prepared one.

## Alternatives and consequences

- **Keep serving the self-signed certificate and let the operator
  install the prepared one by hand.** Rejected: it is the manual step
  the whole prepared workflow exists to remove.
- **Install the prepared pair and stop there.** Rejected: the bootstrap
  key would remain shared with the preparing Comb indefinitely, and the
  preparing Comb would keep a copy of a key for a machine it does not
  control.
- **Have the preparing Comb issue the replacement and send it too.**
  Rejected: it needs the joiner's own key to do that, which the
  joiner must generate locally - so the replacement is necessarily the
  joiner's own act.
- **Delete the bootstrap key immediately after installing the
  prepared pair.** Rejected: if the replacement then fails, the Comb
  has no key at all, and it was serving a working certificate moments
  before.
