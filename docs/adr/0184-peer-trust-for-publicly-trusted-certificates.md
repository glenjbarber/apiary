# ADR-0184: The peer trust store accepts a publicly trusted certificate

## Status

**Accepted.** This record decides what the peer trust store holds when
a Colony's Combs carry certificates issued by a certificate authority
rather than self-signed ones.

Supersedes: None
Superseded by: None
Affected projects: Apiary
Implements: ADR-0147 Part 4 (the replicated peer trust store),
ADR-0170 (the authorized peer trust transition), ADR-0133 (the
certificate issuers)
Tracking issue: #26

## Context

### The store could never be populated on a Colony with public certificates

ADR-0147 Part 4 built a replicated peer trust store and a derived
`/usr/local/etc/apiary/peer-ca.pem` written from it. Every managerd
verifies a peer's presented certificate against that file, so it is
the trust anchor for peer TLS.

The store's only writer at the time was the join flow, and the join
flow only ever ran for a Comb that joined *after* the store existed.
`apiaryctl pin-trusted-peers` was built to backfill it for Colonies
that predated it.

On a Colony whose Combs carried **self-signed** certificates, that
command worked. On a Colony whose Combs carry **publicly trusted**
certificates - which is what the Cloudflare/ACME work (ADR-0133,
ADR-0169, ADR-0173) produces - it refused every certificate:

> certificate is issued by `CN=YE2,O=Let's Encrypt,C=US` but its
> subject is `CN=brood.lab3.apiary.work`, so it is not a Comb's own
> self-signed certificate

The refusal came from `internal/hostcert`, which compared the
certificate's raw issuer against its raw subject. A Let's Encrypt
leaf is issued by Let's Encrypt, so it is not self-signed, and the
comparison refused it.

The consequences were all silent, which is what made it worth a
record:

- The store stayed **empty**.
- `peer-ca.pem` was therefore **never written** - `internal/peerca`
  refuses to render an empty store, because writing an empty file
  would atomically replace a good one.
- Peer TLS had **no derived anchors** at all, so peer verification
  fell back to the system pool - which does not contain a Colony's
  own private CA either.
- The operator's only signal was the pin preflight failing, with a
  message about a certificate they could not change.

### The second gate was just as blocking

`internal/hostcert` also requires an `IP:127.0.0.1` subject
alternative name of a Comb's serving certificate, because every
daemon that dials managerd defaults to loopback and ADR-0139's first
live failure was exactly a certificate missing it.

That requirement is right for a Comb's **own** certificate. It is
wrong for a **publicly trusted** one: such a certificate is dialled
*by name* - the DNS SAN is what carries the identity - and a
Let's Encrypt leaf carries no `IP:127.0.0.1` address at all. The
live certificate on this Colony is exactly that: a DNS SAN of
`brood.lab3.apiary.work` and no IP address.

So even with the issuer check relaxed, the loopback requirement
would have refused every publicly trusted certificate.

## Decision

### A Comb's certificate may be self-signed or issued by a CA

`internal/hostcert`'s evaluation gains one option,
`AllowCAIssued`. It is **opt-in**: a caller that needs a Comb's own
self-signed certificate still gets the old refusal, so nothing that
depended on the old gate changed behaviour silently.

With it set, a certificate whose issuer differs from its subject is
accepted **provided**:

- it carries a `BasicConstraints` extension, so it was issued under
  a CA's rules rather than being a root; and
- every other gate still passes.

The issuer's own signature is **not** verified at this gate.
Verifying it means walking the issuer's chain to a root, which is
what the trust store's own verification does when the CA certificate
is pinned. This gate only asks the question that can be answered from
the leaf alone.

### The loopback SAN is required of a self-signed certificate only

A self-signed Comb certificate must still carry `IP:127.0.0.1`; that
is unchanged, and the existing tests that pin it still pass. A
CA-issued certificate is dialled by name and is not required to carry
a loopback address.

### The store still holds the LEAF, and the derived file still verifies the peer

The decision to keep the leaf in the store, rather than recording the
issuing CA, was checked against Go's own verifier rather than
assumed. With `peer-ca.pem` holding each Comb's leaf:

- a peer presenting **that same Comb's** leaf verifies against the
  file; and
- a peer presenting a **different** Comb's leaf does **not** verify -
  the file does not hold the CA that signed it.

With the file holding the **issuing CA** instead, every Comb's leaf
verifies.

So the honest model for a Colony whose members hold public
certificates is the second: **the store should record the issuing
CA, not the leaf.**

That is a larger change than this record makes, and it is recorded
here rather than half-done:

- The `TrustedPeer` proto has no field for a CA anchor, and adding
  one changes replicated state, so it is a schema decision.
- `internal/peerca` renders the store's certificates verbatim, so it
  would need to render the CA rather than the leaf.
- The trust transition (ADR-0170) stages a **certificate** beside the
  current one, and its overlap semantics are about a Comb's own
  serving certificate, not about a CA.

**What this change does is make the store usable at all on such a
Colony.** It is the difference between "the trust store cannot be
populated" and "the trust store can be populated, with the leaf
recorded, and the derived file verifies each Comb's own certificate".

The CA-anchor model is the right end state and is **not done**. This
record says so plainly, because a record that overstates what is
built sends the next person looking for something that was never
built.

### What is NOT changed

- A self-signed certificate still must verify its own signature, and
  the fingerprint and expiry of every pin are still **re-derived**
  from the certificate by the FSM rather than believed.
- The pin is still create-only, and the trust transition is still
  ADR-0170's.
- The raft certificates' own trust is untouched. This Colony's raft
  certificates are issued by a private `CN=Apiary Colony CA`, which
  is not in the system trust store, so raftd's TLS still fails
  verification against the system pool. That is a separate matter
  from the peer trust store, and it is not fixed here.

## Consequences

- A Colony whose Combs carry publicly trusted certificates can
  populate its peer trust store, and `peer-ca.pem` is written.
- The gate remains fail-closed by default: a caller that does not ask
  for a CA-issued certificate is refused one, exactly as before.
- The derived file verifies each Comb's **own** certificate against
  the store. Verifying a peer's certificate that was issued by a
  *different* CA is not supported by this model, and the CA-anchor
  model that would support it is the open work.

## Validation

- `internal/hostcert`: a CA-issued certificate is accepted with
  `AllowCAIssued`, refused without it, and refused for every other
  gate - no DNS SAN, expired, no `BasicConstraints`.
- `internal/raft`: a CA-issued certificate is pinned, recorded, and
  rendered into the derived file, and the derived file verifies the
  pinned certificate with Go's own `x509` verifier.
- A fingerprint that does not match the certificate it carries is
  still refused, CA-issued or not.
