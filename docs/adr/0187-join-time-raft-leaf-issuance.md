# ADR-0187: A joining Comb is issued its own raft transport leaf

## Status

**Accepted.** This record decides how a joining Comb obtains the raft
transport certificate it needs to be a member of the Colony.

Supersedes: None
Superseded by: None
Affected projects: Apiary
Implements: ADR-0155 (the Colony certificate authority) - its staged
plan, step 4; ADR-0186 (the join-time CA distribution)
Tracking issue: #97

## Context

### What ADR-0186 left

ADR-0186 made the Colony CA reach a joining Comb, and made the
joining Comb check it against the fingerprint the operator pinned. It
recorded, as work not done:

> The joiner does not yet get its own raft transport leaf. The CA is
> installed, and `internal/colonycainstall.LeafIsIssuedByCA` verifies
> a leaf against it, but the flow that issues the joiner's
> `raft.pem`/`raft.key` from the installed CA and points its
> `raftd.json` at it is not wired into the join.

This record closes that.

### The constraint that shapes the whole design: the CA KEY does not move

ADR-0155 is explicit that the CA key is a single point of trust:

> Whoever holds `ca.key` can issue a certificate the whole Colony
> accepts. The key is root-only and stays on the creating Comb.

So only the Comb that ran `apiaryctl colony-ca init` can issue a
leaf. **And that Comb is not necessarily the Colony's leader.**

This is not a hypothetical. On the live Colony, the leader is
`drone.lab3.apiary.work` and the CA key is on
`brood.lab3.apiary.work`. The Comb that builds the join credential
bundle is the leader, and the Comb that can issue the leaf is not.

Three designs were available, and the obvious one is wrong:

- **The leader issues the leaf.** This requires the leader to hold
  the CA key, which means moving the Colony's root of trust to
  whichever Comb happens to be leader. Every leadership change would
  move it. That is exactly what ADR-0155 refuses to do, and it would
  turn a single point of trust into a moving one.
- **The CA key is copied to the joining Comb.** Then the joining Comb
  could mint a certificate for any other Comb, and a compromised
  joining Comb would be a compromised Colony.
- **The CA holder issues, and the leader relays.** The key stays put.
  The holder seals the leaf to the joining Comb, so the leaf and its
  key are readable only by the machine whose identity was already
  verified.

The third is what this record does.

### How does the leader know which member holds the key?

It does not, and it must not guess. The key does not move, so which
member holds it is a property of the Colony that the operator knows
and the cluster does not.

So the **CA holder is named in the join authorization**, alongside the
CA fingerprint that ADR-0186 already put there. The operator is
already running `apiaryctl join-authorize` on the leader, already
knows where the Colony's CA key lives, and now says so with
`--colony-ca-holder`.

The holder's node id is then resolved to an address from **raft's own
membership**, never from a caller-supplied one. The join poll is
deliberately unauthenticated and fail-closed, so an address that came
from the request would be an address anyone chose - and this call
makes a Comb sign a certificate.

## Decision

### The CA holder issues; the leader relays

A new RPC, `IssueColonyRaftLeaf`, asks the member holding the CA key
to issue a leaf for a joining Comb. The leader calls it and puts the
sealed leaf into the join credential bundle alongside the CA.

**The call is NOT unauthenticated.** The join RPCs
(`RequestJoinColony`, `GetJoinRequestStatus`, `CancelJoinRequest`)
are deliberately exempt from API-key auth, because a joining Comb has
no key yet. This one is not, and the difference is the point: a call
that makes a Comb sign a certificate has no business being answerable
by whoever dialled it. It is issued by a member of the Colony
forwarding to another member, over the ordinary peer channel, and it
is **Admin**-tier - the same tier as `IssuePreparedCertificate`,
which also issues a credential.

**The CA is re-derived, not believed.** The holder compares its own
CA's fingerprint against the one the caller expected, and refuses a
mismatch. A forwarded request that asked for a leaf from a *different*
CA is refused, so the request cannot be used to make a Comb sign
against a trust anchor the Colony did not pin.

**The leaf's names are the joiner's, not the caller's.** The DNS
name is the joining Comb's `node_id` and the IP comes from its
`raft_bind_address` - both of which the joiner itself declared. A
leaf issued for one Comb therefore cannot carry another Comb's
names.

**The leaf is sealed to the joining Comb's own certificate**, with the
join's associated data binding it to the request and the Comb. So
the leaf and its key are readable only by the machine whose identity
was already verified, and a ciphertext copied from one join's
delivery fails to decrypt under another's.

### The joiner installs the leaf, and only then points raftd at the CA

`internal/colonyleafinstall` is the joiner's half.

- **The leaf is VERIFIED against the CA before it is written.** A
  leaf that does not chain to the Colony CA is a certificate no peer
  can verify, and a joiner that installed it would be a member the
  Colony could not authenticate.
- **The leaf's names are checked against the joiner's own identity.**
  A leaf issued for another Comb is refused.
- **An existing leaf is never replaced.** A Comb that already has a
  raft transport certificate keeps it, because replacing it silently
  would invalidate every leaf the Colony holds for that Comb.
- **The key is 0600 and is written second.** A certificate without
  its key is inert; a key without its certificate is a secret nobody
  can use.

**Pointing raftd at the CA is a SEPARATE step, and it is not
automatic.** Switching a member's raft transport to TLS is an
operator decision, and ADR-0078's own posture is that encryption and
trust have to be in place in that order: a raftd pointed at a CA it
does not trust cannot start. A configuration that already names a
raft transport certificate is left alone.

## What this ADR does not cover

- **CA rotation and leaf renewal.** ADR-0155 records that neither is
  included: a leaf lasts about a year, the CA ten years, and the CA
  key is a single point of trust whose loss means a Colony rebuild.
  A leaf issued here expires in about a year and nothing renews it.
- **The `raftd` restart.** Installing the leaf and pointing `raftd`
  at it do not restart `raftd`. A member whose transport is switched
  to TLS needs `raftd` restarted to pick the new material up, and
  that is an operator step on a live Comb, not something a join can
  do safely for itself.
- **The mixed-version window.** A member running a binary that
  predates this record does not serve `IssueColonyRaftLeaf`, and a
  join that names a CA holder it cannot reach is refused rather than
  half-completed.
- **Distributing the CA to members that joined before this record.**
  A Colony whose members were provisioned by hand still has each of
  them issuing and trusting by hand.

## Consequences

- A joining Comb can be given a raft transport certificate that its
  peers will accept, without the Colony's CA key moving anywhere.
- The Comb that signs the certificate is the one that holds the
  key, and the certificate is opaque to everyone else - including the
  leader that relayed it.
- The CA holder is named by the operator, not discovered, so a
  misconfigured authorization produces a refusal naming the member
  that could not be found rather than a silent join without a leaf.

## Validation

- `internal/colonyleaf`: a leaf issues and chains to the CA; a Comb
  with no CA key cannot issue; a holder whose CA is not the expected
  one refuses; a holder with no expected fingerprint refuses; the leaf
  is readable only by the joiner whose certificate it was sealed to;
  the leaf is bound to its request; the leaf carries the joiner's
  names and not another's.
- `internal/colonyleafinstall`: the leaf and its key are written
  with the right modes; a leaf that does not chain to the CA is
  refused and **nothing is written**; a leaf issued for another Comb
  is refused; an existing leaf is never replaced; raftd's
  configuration is pointed at the CA and the leaf, and an existing
  configuration is left alone.
