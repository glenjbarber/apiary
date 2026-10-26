# ADR-0170: An authorized peer trust transition

## Status

**Proposed.** This record decides how a Comb's pinned serving
certificate is replaced by another one - a rotation, a renewal, or the
adoption of a certificate prepared for it before it joined - and how
the Colony's peers come to recognise the new one without an operator
copying fingerprints to each of them.

Supersedes: None
Superseded by: None
Tracking issue: #26 (the Cloudflare/Let's Encrypt bootstrap task)
Implements: ADR-0169 (the prepared certificate a transition may adopt)

## Context

The replicated peer trust store (`internal/raft/peers.go`) is
CREATE-ONLY. `applyPinTrustedPeer` refuses a `node_id` that is already
pinned, and `api/internalpb/state.proto` records why:

> Rotating a Comb's certificate is UnpinTrustedPeer (with its own
> logged reason) followed by a fresh PinTrustedPeer, so it is two
> visible acts in the log rather than one invisible one.

That reasoning is right about the thing it was guarding against - a
silent replacement of a certificate an operator compared by hand. But
applied literally to a machine-driven rotation, it produces a window
that is worse than the problem it avoids.

**The window.** Between the unpin and the pin, the Comb is trusted by
nobody. `internal/peerca` renders `/usr/local/etc/apiary/peer-ca.pem`
from the store, so that file immediately loses the Comb's entry, and
every peer's next handshake with it fails. If the pin then fails for
any reason - the new certificate does not evaluate, the log entry is
refused, the operator's command is interrupted - the Comb is left with
no trust anchor at all, and the only recovery is the out-of-band
`apiaryctl pin-trusted-peers` backfill, which needs the operator to
compare certificates by hand on every host.

**The second half.** The serving side is already solved:
`internal/tlscache` re-reads the certificate and key on the next TLS
handshake when either file changes, so a replaced pair is picked up
with no restart and no dropped web session. What is missing is the
TRUST side - the store that says which certificate each Comb is
expected to present.

**The prepared case.** ADR-0169 introduces a certificate prepared for
a Comb before it joined, with a private key the preparer generated.
Adopting it is a transition like any other, except that the Comb
arrives holding a certificate the Colony has never seen and never
compared.

Three properties of the existing tree shape the design, and all three
are load-bearing:

  - **The FSM re-derives.** `applyPinTrustedPeer` re-evaluates the
    certificate with `hostcert.Evaluate`, re-derives the fingerprint
    from the PEM, and refuses a pin whose stated fingerprint or expiry
    disagrees with its own certificate. A transition must be evaluated
    the same way, on every replica, deterministically.

  - **The store is keyed by `node_id`.** A Comb has ONE entry. The
    map is `map<string, TrustedPeer>`, so "the old and the new
    certificate at once" is not a shape the store currently has.

  - **`peer-ca.pem` is derived.** It is a cache of the store, and
    `internal/peerca` refuses to publish an empty store over an
    existing file. Whatever the store holds is what the peers trust.

## Decision

A **trust transition** replaces one pinned certificate with another for
the same `node_id`, as ONE visible act in the log, with a bounded
overlap in which both certificates are trusted.

**The transition is a new FSM command, `TransitionTrustedPeer`.** Not
an extension of `PinTrustedPeer`, and not a unpin followed by a pin. A
single command means:

  - There is no window in which the Comb is trusted by nobody. The
    entry is replaced in one log entry, so a replica either has the old
    certificate or the new one, never neither.
  - A refusal leaves the OLD pin exactly as it was. The store is
    untouched, `peer-ca.pem` is untouched, and the Comb keeps serving
    a certificate its peers still trust. A failed rotation degrades to
    "the rotation did not happen", which is recoverable, rather than
    to "this Comb is now untrusted", which is an outage.
  - The log records the transition as one act, with the reason, rather
    than as two acts that an incident review has to correlate.

**Both certificates are held during the overlap.** The store's entry
gains a `next` field carrying the incoming certificate, its
fingerprint and its expiry. While the next certificate is staged, the
rendered `peer-ca.pem` contains BOTH the current and the incoming
certificate, so:

  - the Comb can already serve the new certificate, and
  - its peers can already verify it.

That is what makes the swap safe in either order. Whether the Comb
installs the new pair before or after the transition is committed, there
is a period in which both certificates are trusted and at least one of
them is being served - so every handshake succeeds throughout.

**The overlap is bounded and explicit.** The transition carries a
`next_expires_at_unix`, and the staged entry is dropped once that
moment passes. An unbounded staged entry is a permanent second trust
anchor, and a permanent second trust anchor is a certificate nobody
knows why is still trusted.

**The staged certificate is not a voter.** `is_voter` describes whether
the Comb is a MEMBER, and membership does not change when its
certificate does. The transition carries it forward unchanged, and the
FSM refuses a transition that would alter it.

**The incoming certificate is evaluated exactly like a pin.** The same
`hostcert.Evaluate` gate, the same fail-closed rules, the same
re-derivation of the fingerprint and the expiry. A certificate that
would be refused as a pin is refused as a transition, with the same
reason.

**A transition is refused for a `node_id` that is not pinned.** There
is nothing to transition from, and silently creating a pin would trust
a certificate nobody compared.

**A transition is refused when one is already staged.** A second
transition for the same `node_id` is refused rather than replacing the
staged certificate. The staged one must be activated or abandoned
first, so the store's state for a `node_id` is always unambiguous.

**Activation and abandonment are separate, visible acts.** Activating
promotes the staged certificate to current and drops the old one.
Abandoning drops the staged certificate and keeps the current one.
Both are their own log entries, and both record a reason. The
reactivation-after-failure case is exactly why abandonment exists: a
staged certificate that turns out to be wrong is dropped, and the Comb
carries on with the certificate its peers already trust.

**The transition is authorized by the Colony, not by the certificate.**
A public CA validates a hostname. It says nothing about whether this
Comb may be a member of this Colony, and a transition must not be a
mechanism by which a publicly valid certificate becomes a trust anchor
here. The transition is issued by an Admin of the Colony, through the
existing role map, and it names the `node_id` it applies to - a raft
identity, which is what the join protocol already establishes.

**The joining Comb's certificate is adopted through the same path.**
ADR-0169's prepared certificate is installed by the joiner and then
transitioned into the store by the Colony. The prepared certificate is
a normal certificate and takes the normal path; nothing about it is
special except that its fingerprint was not compared by hand, which is
why the transition's authorization is the Colony's decision rather
than the preparer's.

### Two pre-existing bugs this work found and fixed

Building the transition against the real trust store turned up two
defects in the EXISTING pin path. Neither is caused by this record,
and both are fixed here because the transition is the same shape of
act and would have inherited both.

  - **`PinPeerCertificate` PANICKED on every call.** The RPC cloned the
    caller's `rpcpb.TrustedPeer` and asserted it as an
    `internalpb.TrustedPeer`. They are DIFFERENT PROTOBUF TYPES, so
    the assertion failed on every call - the RPC could not write the
    trust store at all. The fix is an explicit field-by-field
    conversion, `toInternalTrustedPeer`, which cannot panic and cannot
    silently carry a field across that the two schemas spell
    differently.

  - **A successful pin reported an EMPTY peer.** The raft server's
    apply-result payload chain had no `TrustedPeer` arm, so with no
    arm the payload stayed nil, `proto.Marshal(nil)` yielded zero
    bytes rather than an error, and the managerd side unmarshalled
    those zero bytes into a zero-valued `TrustedPeer`. A pin that
    SUCCEEDED therefore answered with no node_id, no fingerprint and
    no certificate - and a caller checking the response for the pin it
    just made read it as nothing having been pinned.

Both are the same failure the `ColonyJoinWindow` arm's own comment in
`internal/raft/server.go` describes: a missing arm fails OPEN rather
than loudly, and the value the caller receives looks like a refusal
the caller never asked for. The pin is the trust store's only writer,
so the first bug meant the store could not be written through the RPC
at all, and the second meant a caller was told its trust anchor was
not recorded.

## Consequences

- A Comb's certificate can be replaced - by renewal, by rotation, or by
  adopting a prepared certificate - without a window in which its peers
  cannot verify it.
- A failed transition leaves the previous certificate trusted and
  serving. The failure mode is "the rotation did not happen" rather
  than "this Comb is now untrusted".
- `peer-ca.pem` grows by one entry for the length of the overlap. It
  is a pin list, so a second entry during an overlap is exactly what a
  pin list is for; the entry is removed when the overlap ends.
- The store's shape changes: a `TrustedPeer` gains a staged `next`.
  That is a schema change to `api/internalpb/state.proto`, and every
  replica must be able to read the old shape - a staged certificate is
  an optional field, so an older replica ignores it and a newer one
  reading an older entry sees no staged certificate.
- The FSM's create-only rule is narrowed in exactly one place: a
  `node_id` that is pinned may now be TRANSITIONED, which is a visible,
  reasoned, single act. It may not be re-pinned, and the old pin is
  never silently overwritten by a pin.
- The operator's manual path still exists. `apiaryctl
  pin-trusted-peers` remains the way to backfill a Colony that predates
  the store, and it remains a command rather than a page, because the
  thing an operator has to do there is look at certificates.
- The prepared-certificate case inherits this path's authorization
  model: the Colony decides whether to adopt a prepared certificate,
  and the CA's opinion is not consulted for that.

## Alternatives and consequences

- **Unpin then pin, as the proto currently describes.** Rejected: it
  has a window in which the Comb is trusted by nobody, and a failure
  between the two leaves it untrusted permanently.
- **Replace the pin's fields in place.** Rejected: it is exactly the
  "silent replacement" the create-only rule exists to prevent, and it
  leaves no record of what the certificate was before.
- **Let peers accept any publicly valid certificate for a known
  hostname.** Rejected: that is "implicit trust" in the sense ADR-0161
  forbids - it would make a publicly valid certificate a trust anchor
  without a Colony decision.
- **Have the operator copy the new fingerprint to each peer.** Rejected:
  it is the manual distribution step the replicated store was
  introduced to retire, and it does not scale past a handful of Combs.
