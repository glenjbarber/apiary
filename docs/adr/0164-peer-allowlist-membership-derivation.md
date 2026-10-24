# ADR-0164: Deriving the peer allowlist from Colony membership

## Status

Proposed. This design has not been approved by the owner and is not implemented.

Supersedes: None
Superseded by: None
Tracking issue: #84
Related: ADR-0097 (the allowlist this ADR derives), ADR-0115 (the membership-derivation pattern this ADR follows)

## Context

ADR-0097 introduced `known_peer_addresses`: a comma-separated `host:port`
allowlist that the three deliberately-unauthenticated join-colony RPCs
(`RequestJoinColony`, `GetJoinRequestStatus`, `CancelJoinRequest`) check
a caller-supplied `target_address` against before dialing it. An empty
allowlist fails closed, so an unconfigured Comb refuses every forwarded
target rather than serving as a network-reachability oracle.

That fail-closed rule is correct, and it created the gap this ADR
addresses. ADR-0097 originally documented the allowlist as an
operator-maintained list, seeded at install time from
`apiaryctl install --colony-member HOST:PORT`. The installer's own
comment states the limitation plainly: the seeded list "is NOT kept
current as further Combs later join this Colony - that needs the join
flow itself to extend it on every member, which is explicitly out of
scope here."

So today a Combs allowlist names exactly the one member it was told
about at install time. When a second Comb joins the Colony, that new
member is a legitimate `target_address` destination, but no existing
member will forward to it, because no existing member's allowlist names
it. The operator's remedy is to hand-edit `known_peer_addresses` on
every Comb, and to repeat that after every join.

This is the same class of problem ADR-0115 already solved for
`peer_tls_hostname_map`, and it is worth being explicit that the two
are not the same problem. `peer_tls_hostname_map` maps a raft-bind
IP to a TLS hostname, so it is derived by resolving each member's raft
address. The allowlist here answers a different question: which
managerd addresses are legitimate forward targets. It is a set of
member addresses, and raft membership already knows every member.

The two differ in one important way that this ADR must get right: the
allowlist holds **managerd** addresses, while raft's committed
configuration holds **raft** addresses. `internal/manager`'s
`peerManagerdAddrOf` already performs exactly this transformation for
leader hints, keeping the host and substituting the configured managerd
port (`peer_managerd_port`, default `17700`). The derivation must use
that same function, not a second copy of it, so the two can never
disagree about what a member's managerd address is.

## Decision

### The allowlist becomes derived, with the operator's list still authoritative

`Server.knownPeerAddresses` becomes two layers:

- The **configured** list, exactly as today: the persisted
  `known_peer_addresses` value, refreshed by `SetKnownPeerAddresses`
  whenever the node config changes. This layer is never rewritten by
  derivation.
- The **derived** list, rebuilt from raft membership on each refresh.

A `target_address` is allowed if it matches either layer. The
configured layer is checked first, so an operator who hand-configured an
entry keeps it exactly, and derivation can only ever widen the set, never
narrow an explicit choice.

The derived layer is rebuilt in full on every refresh rather than being
appended to, so a member removed from raft's configuration stops being a
valid target on the very next refresh instead of leaking a stale entry.

### Deriving the addresses

On each refresh, `managerd` reads raft's own committed configuration
through the `raftClient.Status(ctx)` call it already makes at startup
and from its own background loops. For each member, the raft-bind
address's host part is combined with the managerd port using
`peerManagerdAddrOf`, producing that member's managerd address.

Raft membership is the only source of "every member" this codebase has.
`internal/commonconfig` holds only this node's own identity, not a
roster, so there is no separate peer list to fall back to.

The local node is included like every other member. It is a legitimate
target for the same reason any other member is, and excluding it would
require a separate rule about which member is "this one".

### Refresh timing

A background goroutine, mirroring ADR-0115's
`runPeerHostnameRefreshLoop` exactly: an immediate first run, then one
per tick, until context cancellation, with failures logged and not
fatal. The interval is a fixed 30-second constant, matching ADR-0115's
own interval and for the same reason: a membership change is rare and
operator-driven, and this codebase has no cheap "membership changed"
signal plumbed out of `internal/raft`.

A `raftClient.Status` failure leaves the previously-derived set in place
until the next successful tick. It never clears the allowlist: a
transient raft query failure must not silently disable forwarding for
every member.

### What derivation does NOT change

The fail-closed rule is untouched. An empty configured list **and** an
empty derived set still refuses every forwarded target. Derivation
supplies members when membership is known; it does not make forwarding
unconditionally available.

`checkTargetAddressAllowed`'s error text is preserved. It is what an
operator sees when a target is refused, and its current wording names
both the unset-configuration case and the not-in-allowlist case.

The persisted `known_peer_addresses` config value is **not** rewritten
by derivation. Derivation is a runtime view of membership, not a
config edit: a Comb that derives four members from raft does not write
four addresses into its own `managerd.json`, because that file is
operator-owned, is restored from backups, and is edited through the
Machine page. Writing derived values back into operator-owned config is
how a list goes stale in a new way - it would look authoritative while
being a snapshot of membership at some past moment.

The install-time seeding stays as it is. `--colony-member` still seeds
the configured list, which remains the operator's explicit override.

### Precedence and the explicit-entry case

An operator who adds an address to `known_peer_addresses` by hand is
doing so for a reason - a member not yet in raft's configuration, a
nonstandard managerd port, a debugging entry - and derivation must
never remove it. This mirrors ADR-0115's own "manual always wins over
derived" precedence and ADR-0112's "an explicitly set value always
wins over the auto-filled one".

## Validation

Tests cover:

- a member present in raft membership is allowed as a `target_address`
  even when the configured list does not name it;
- a member's derived address uses the managerd port, not the raft port
  (`peerManagerdAddrOf`'s transformation, including a non-default
  `peer_managerd_port`);
- an address in the configured list is still allowed after derivation
  runs, and derivation never removes it;
- a member removed from raft membership stops being allowed after the
  next refresh;
- a target that is neither configured nor a member is still refused;
- an empty configured list with an empty derived set still refuses
  everything (fail-closed preserved);
- a `raftClient.Status` failure leaves the previous derived set intact
  rather than clearing it;
- all three join-colony RPCs honour the derived set.

## Consequences

- The allowlist stays current as Combs join, without an operator
  editing every member's config after every join.
- The allowlist now tracks membership in both directions: a removed
  member stops being a forward target.
- Raft membership becomes load-bearing for join-flow forwarding. A Comb
  that cannot read raft status keeps whatever it last derived, which is
  the same failure mode ADR-0115 already accepts for hostname
  derivation.
- `known_peer_addresses` gains a second, derived meaning in the config
  file's own terms: it is now the operator's explicit override layer
  rather than the whole allowlist. The Machine page's existing
  description of the field should say so.
