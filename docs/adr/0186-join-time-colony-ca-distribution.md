# ADR-0186: A joining Comb receives the Colony CA, pinned

## Status

**Accepted.** This record decides how a joining Comb comes to trust the
Colony's certificate authority.

Supersedes: None
Superseded by: None
Affected projects: Apiary
Implements: ADR-0155 (the Colony certificate authority) - its staged
plan, step 3
Tracking issue: #97

## Context

### What ADR-0155 built, and what it left

ADR-0155 built the Colony CA: `internal/colonyca` with `GenerateCA`,
`IssueLeaf` and `Fingerprint`, and `apiaryctl colony-ca` to drive them.
A Colony has one trust anchor, every Comb's raft transport certificate
is a leaf from it, and a Comb trusts a peer only if the peer's
certificate chains to that CA.

ADR-0155's staged plan lists four steps. Step 1 (the package) is
done. Step 2 (install creates the CA) is done as far as the operator
path goes: `apiaryctl colony-ca init` creates it, `apiaryctl
colony-ca issue-raft` issues a leaf from it, and `raftd.json`'s
`raft_tls_ca` names it.

**Step 3 is this record: a join distributes and pins the fingerprint.**

Without it, a joining Comb is a Comb with no CA. It cannot issue a
raft transport certificate, and it cannot verify a peer's - so it is
either a member the Colony cannot authenticate, or it is running raft
transport in plaintext, which ADR-0078 names as a committed
limitation.

### The joiner must not learn which CA to trust from the network

ADR-0155 says it directly:

> A Comb that joins a Colony does not learn which CA to trust from the
> network. A join authorization carries the SHA-256 fingerprint of the
> Colony CA certificate, and the joining Comb accepts a CA only when
> its fingerprint matches.

That is the whole security property. The Colony is handing the
joiner a CA. If the joiner simply trusted it, then any party that
could answer a join poll could choose the Colony's trust anchor, and
the anchor would be worth exactly as much as the connection that
delivered it.

So the authority for the CA has to come from the same place the
authority for the Comb's own identity comes from: the **root-owned
join authorization**, whose creation requires root and whose contents
an operator wrote by comparing fingerprints by hand.

### The fingerprint format question ADR-0155 left open

ADR-0155 records that `internal/colonyca.Fingerprint` produces the
lowercase hex SHA-256 of the DER bytes, and that this is deliberately
a different shape from `internal/hostcert.Fingerprint` ("SHA256:"
followed by uppercase colon-separated pairs). It says:

> Step 3 will decide whether join authorizations keep the existing
> format or move to the new one. Until then, the two must not be
> mixed.

**This record decides it: the Colony CA's fingerprint uses colonyca's
own format, and the join authorization's existing Comb fingerprint
keeps hostcert's.**

They are not mixed, and they cannot be mixed by accident:

- the CA fingerprint has its own field, `colony_ca_fingerprint`, with
  its own normalizer, `joinauth.NormalizeCAFingerprint`, which is a
  **different function** from `joinauth.NormalizeFingerprint`;
- the two fields are compared in different places - the Comb's
  fingerprint against the pending join request's
  `tls_cert_fingerprint`, the CA's against the CA certificate the
  bundle carries.

The two formats look similar enough to be confusing and different
enough to be a bug, so a caller that wanted to compare them wrongly
would have to go out of its way - and the compiler would not stop it.
That is why the comparison of the CA fingerprint lives in
`internal/colonyca.VerifyCA` rather than being left to a caller.

## Decision

### The CA travels in the existing sealed bundle, as version 3

The join credential bundle is already an authenticated channel to
this exact Comb, sealed to the certificate the joiner presented. A
separate channel for the CA would have meant a second place for the
joiner's identity to be checked, and a second thing to get subtly
wrong.

`joincredential.Bundle` gains a `ColonyCA` field, and the bundle
version becomes **3**. Versions 1 and 2 are unchanged, and a joiner
that understands only version 1 reads what it understands and ignores
the rest.

A joiner that understands versions 1 and 2 and meets a version 3
bundle **refuses it** rather than joining without the CA it needs.
Joining without it would produce a member the Colony could not
verify, and the failure would surface later and further from its
cause.

The `ColonyCA` carries three values, and they are carried together
because each is useless without the others:

| Field | What it is |
| --- | --- |
| `certificate_pem` | the CA certificate, which is public |
| `fingerprint` | the Colony's own computation of what it is sending |
| `pinned_fingerprint` | the value the operator compared by hand |

**The joiner checks the certificate against `pinned_fingerprint`, not
against `fingerprint`.** Checking the Colony's certificate against
the Colony's own arithmetic would prove nothing at all. The pinned
value is the one a human compared, out of band, in a root-owned file.
`fingerprint` is carried so the two can be required to agree with each
other *and* with the certificate, which catches a bundle whose own
account of itself is wrong.

**The CA private key never travels.** It is the Colony's root of trust
and it stays on the Comb that created it. A joining Comb receives a
certificate, never a key.

### The pinned fingerprint is recorded in the join authorization

`joinauth.Entry` gains an optional `colony_ca_fingerprint`, and
`apiaryctl join-authorize` gains a `--colony-ca-fingerprint` flag.

It is optional, and empty means "this entry says nothing about the
Colony's CA". That is right for a Colony whose members do not use
raft transport TLS, and it keeps an existing authorization file
readable by a binary that has never heard of a Colony CA.

**An entry that pins no CA fingerprint gets no CA.** A join that
does not say which CA it expects is a join that cannot check the one
it is given, and shipping it anyway would be a CA the joiner has to
take on trust - which is the thing this record exists to prevent.

The fingerprint is read from the authorization file at the moment the
bundle is built, not from replicated state. The authorization file is
per-Comb and physical, exactly as the authorization itself is;
copying it into raft would make a per-Comb operator decision into
replicated Colony state.

### The joiner verifies before it writes, and never replaces

`internal/colonycainstall` is the joiner's half, and it is its own
package for the same reason `internal/certintegration` is: both
sides of a join need it, and putting the installation in the manager
package would have made the joiner depend on the manager, which drags
raft, bbolt and the cluster packages into a binary an operator runs
during an incident.

Its rules, each a refusal rather than a warning:

- **THE PINNED FINGERPRINT IS CHECKED BEFORE ANYTHING IS WRITTEN.**
  The certificate is verified against the operator's pinned value, and
  only a CA that matches it is installed. A CA that satisfies every
  structural check but not the fingerprint is exactly the certificate
  the comparison exists to catch.
- **AN EXISTING CA IS NEVER REPLACED.** A Comb that already holds
  the Colony's CA keeps it; a Comb holding a *different* CA is a
  refusal, because a Colony has one trust anchor and two of them is a
  split. Replacing a CA silently invalidates every leaf the Colony
  holds, and a join must not do that.
- **THE SAME CA IS A SUCCESS, NOT A REFUSAL.** A join retried after a
  partial failure must not be blocked by its own earlier progress.
- **THE CA KEY IS NEVER WRITTEN.** A joining Comb holds the
  certificate - enough to verify every peer and to be verified by them
  - and never the key that could mint a new identity in the Colony.
- **THE CA IS INSTALLED BEFORE THE CREDENTIALS.** The CA is the thing
  that can fail on its own terms, so a refusal to install it is
  reported before anything else has been changed.

## What this ADR does not cover

- **The joiner issuing its own raft transport leaf.** The CA is
  installed, and `internal/colonycainstall.LeafIsIssuedByCA` verifies
  a leaf against it, but the flow that issues the joiner's
  `raft.pem`/`raft.key` from the installed CA and writes them into
  `raftd.json` is **not** wired into the join. A joining Comb that
  installs the CA still has to have its leaf issued and its
  `raftd.json` pointed at the CA.
- **CA rotation and leaf renewal.** ADR-0155 records that neither is
  included, and neither is included here. A leaf lasts about a year,
  the CA ten years, and the CA key is a single point of trust whose
  loss means a Colony rebuild.
- **A mixed-version Colony.** A member running a binary that
  predates this record does not send a CA, and a joiner that
  understands version 3 refuses a version 1 or 2 bundle. A Colony
  mid-upgrade therefore has a join window in which a join is refused
  rather than half-completed.

## Consequences

- A joining Comb can be given the Colony's CA and a leaf it issues
  from it, and the leaf verifies the way a peer would check it.
- The CA a joining Comb trusts is one an operator compared by hand
  and recorded in a root-owned file, not one it was handed.
- The Colony's CA key still never leaves the Comb that created it, and
  a joining Comb never holds it.
- The join authorization's fingerprint format is settled: the Comb's
  own certificate in hostcert's format, the Colony CA in colonyca's,
  in different fields, compared in different places, with different
  normalizers.

## Validation

- `internal/colonycainstall`: a pinned CA is installed; a CA whose
  fingerprint is not the pinned one is refused and **nothing is
  written**; a bundle whose own fingerprint disagrees with its own
  certificate is refused; an existing CA is never replaced and a
  different one is a refusal; the same CA re-installed is a success;
  the CA key is never written; a leaf certificate is refused as an
  anchor.
- End to end: the joiner installs the CA, issues a leaf from it, and
  that leaf verifies against the CA **with Go's own verifier** - not by
  inspection. A leaf from a *different* CA does not verify against it.
