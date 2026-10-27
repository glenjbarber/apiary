# ADR-0185: The peer trust store records the issuing CA

## Status

**Accepted.** This record decides what the peer trust store holds for a
Colony whose Combs carry certificates issued by a certificate
authority rather than self-signed ones.

Supersedes: None
Superseded by: None
Affected projects: Apiary
Implements: ADR-0147 Part 4 (the replicated peer trust store),
ADR-0184 (accepting a publicly trusted certificate)
Tracking issue: #26

## Context

### What ADR-0184 fixed, and what it left

ADR-0184 made the peer trust store ACCEPT a certificate that a
certificate authority issued, rather than refusing it as "not a Comb's
own self-signed certificate". That unblocked the store on a Colony
whose Combs carry public Let's Encrypt certificates: the store could
be populated at all, `peer-ca.pem` was written, and peer TLS had
anchors.

It recorded the leaf, though, and that is a pin list rather than a
trust set.

### The difference between a pin list and a trust set

The derived `/usr/local/etc/apiary/peer-ca.pem` is what every managerd
verifies a peer's presented certificate against. What goes in it
determines what it can verify.

A **pin list** holds one entry per exact certificate. It verifies the
exact certificate it holds, and nothing else. A peer's certificate
verifies only if it is byte-for-byte the one that was pinned.

A **trust set** holds the CA that signed the certificates. It
verifies EVERY leaf that CA signed.

Those are different properties, and the difference is not academic.
A publicly trusted certificate is reissued every 90 days, and the
reissued certificate is a different certificate with the same name.
Under a pin list, every one of those renewals invalidates every
peer's trust in that Comb, and the repair is a manual re-pin of
every member. Under a trust set, the renewal changes nothing at all,
because the CA that signed the old certificate signed the new one.

This was checked against Go's own verifier rather than assumed, and
the result is recorded in `internal/raft/peers_caissued_test.go`: with
the CA as the anchor, a leaf that was NEVER pinned still verifies
against the derived file, because the CA signed it.

### The store is keyed on identities, and a CA has none

`internal/raft/peers.go` holds `map[string]TrustedPeer`, keyed on
`node_id` - a raft identity. A CA is not a member of the Colony and
has no node_id, so the CA cannot be stored as an entry of its own.
It is a property OF an entry: the anchor under which that Comb's
certificate is trusted.

### The trust transition stages a certificate, not a CA

ADR-0170's transition stages an incoming CERTIFICATE beside a
Comb's current one, and its overlap semantics are about which of the
Comb's own serving certificates is trusted. That is a question about
the leaf, and it stays a question about the leaf. What the record
holds underneath - the CA the leaf is anchored by - travels with it,
and a transition between two leaves of the SAME CA changes no anchor
at all.

## Decision

### The store records the CA, and the derived file holds the CA

`TrustedPeer` gains two fields:

- `ca_pem` - the issuing CA's certificate, normalized PEM. It is the
  TRUST ANCHOR. Empty means the entry is a self-signed certificate
  that anchors itself, which is the shape every entry written before
  this record has.
- `ca_fingerprint` - the digest of `ca_pem`, in the same
  `SHA256:` + colon-separated uppercase hex format as the entry's own
  `fingerprint`, and RE-DERIVED from `ca_pem` rather than believed.

`internal/peerca` renders `ca_pem` in place of the leaf when it is
present, and the leaf when it is not. So a self-signed Colony's
`peer-ca.pem` is byte-for-byte what it always was, and a public-CA
Colony's holds the CA.

### A CA anchor is CHECKED, not accepted

A CA that did not sign the certificate anchors nothing. The derived
file would hold a certificate that verifies no peer's leaf, and the
failure would surface as an opaque TLS handshake error against a
host nobody could explain.

So the CA is checked against the certificate at every point the
certificate is checked, and each check names itself:

- the CA is a CA - it carries `BasicConstraints CA:true`, and it is
  permitted to sign;
- the CA actually SIGNED the certificate, verified with
  `CheckSignatureFrom`;
- the CA is inside its own validity window, which is a separate check
  from the leaf's, because a CA that has expired leaves every leaf
  it signed untrustworthy and the operator needs to be told WHICH one
  expired; and
- the CA's digest matches the one the pin states, re-derived rather
  than believed.

### A CA-issued certificate with no CA is REFUSED

Not accepted on the leaf's own say-so. Accepting it would record a
leaf as its own trust anchor - the pin list this record replaces -
and it would do so without anything ever having checked that the
certificate chains to anything at all.

### The CA's own chain is NOT walked

A publicly trusted CA is a leaf-holder's chain up to a root in the
SYSTEM trust store, and the system store is not replicated state. So
requiring the CA to be verifiable up to a system root here would make
a pin depend on one machine's certificate store rather than on the
Colony's record, and the same pin would be accepted on one host and
refused on another.

The CA certificate in the store IS the root of trust by construction,
exactly as a pinned self-signed certificate is. What is verified here
is that it is the CA that signed this certificate - which is the fact
the store is making a claim about - and not whether that CA is
somebody else's idea of a root.

### The operator supplies the CA, and the tool finds it

`apiaryctl pin-trusted-peers` learns a CA-issued certificate:

- In BUNDLE mode, the bundle may hold the CA alongside the members'
  certificates. The CA is recognised by the certificate itself - it
  carries `BasicConstraints CA:true` - not by the operator naming it,
  and it is NOT pinned as a member: the store is keyed on raft
  identities, and a CA has none. Each member's certificate is
  recorded under the CA that actually signed it, found by a real
  signature check rather than by comparing names.
- In NAMED mode, a `--ca` flag names the CA for the member at the
  same position. It is optional, because a member with a self-signed
  certificate needs no anchor.

The CA travels with the pin on the wire, and managerd and the FSM
both re-derive its digest.

## What is NOT changed

- A self-signed certificate still must verify its own signature, and
  it still anchors itself. A self-signed Colony's derived file is
  byte-for-byte what it was.
- The pin is still create-only, and the trust transition is still
  ADR-0170's. A transition's staged certificate is still a
  certificate.
- The fingerprint and expiry of every pin are still re-derived by
  the FSM rather than believed, and the evaluation is still as of
  `pinned_at_unix` rather than the wall clock - so every replica and
  every replay reaches the identical decision.

## Consequences

- A Colony whose Combs carry publicly trusted certificates can
  populate its peer trust store, and `peer-ca.pem` holds a TRUST SET
  rather than a pin list.
- A certificate renewal changes nothing about the trust: the CA that
  signed the old certificate signed the new one, so the anchor is
  unaffected and no re-pin is needed.
- The store's schema changes: `TrustedPeer` gains two optional
  fields. An older replica ignores them, so a mixed-version Colony
  is not broken by it - but an older replica also renders the LEAF
  rather than the CA, so a mixed-version Colony's derived files
  differ until every member is upgraded. That is a migration
  consideration, and it is why the fields are additive rather than a
  change of meaning to an existing one.
- The operator sees which CA a Comb is trusted through: the trust
  page shows the CA's digest beneath the certificate's.

## Alternatives and consequences

- **Keep recording the leaf (ADR-0184's model).** Rejected: it is a
  pin list, so every 90-day renewal of every member's certificate
  invalidates every peer's trust in it and demands a manual re-pin
  of every member. The store would be a recurring operational
  liability rather than a one-time setup.
- **Store the CA as an entry of its own, keyed on a node_id.**
  Rejected: a CA is not a member and has no raft identity, and the
  store's key is a raft identity. A CA entry would need a
  synthetic identity that nothing else in the system uses, and it
  would be a second thing the store has to keep consistent with the
  entries that reference it.
- **Pin the leaf AND the CA.** Rejected: the leaf adds nothing the CA
  does not already give, and it makes the file's contents ambiguous -
  an operator reading `peer-ca.pem` would have to know which entries
  are anchors and which are leaves.
- **Require the CA to chain to a system root.** Rejected: it makes a
  replicated pin depend on a local machine's certificate store, so
  the same pin would be accepted on one host and refused on another.
