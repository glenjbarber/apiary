# ADR-0188: A restore is a Colony re-creation

## Status

**Accepted.** This record decides what a restore produces.

Supersedes: None
Superseded by: None
Affected projects: Apiary
Implements: ADR-0156 (the Colony Protocol) - its staged plan, stage 3
Extends: ADR-0157 (colony identity, stage 1), ADR-0182 (the Colony
epoch, stage 2)
Tracking issue: #97

## Context

### The hazard

ADR-0156 calls restore "the sharpest edge in this ADR", and says why:

> A restore into an empty node produces a colony with a new Colony ID
> and epoch 1, regardless of what the restored FSM payload says about
> identity, because the restored payload cannot be trusted to carry a
> colony identity that the surrounding world agrees with.

The reason is structural. ADR-0051's restore is a whole-state
point-in-time replacement: it seeds an offline snapshot into an empty
node and preserves the serialized FSM state and **nothing else** — not
the log, not the stable vote keys, not the membership history, not the
leader identity, not the transport, configuration, or TLS material.

So a restored node is a brand new one-node Colony. And because
`ColonyIdentity` and `ColonyEpoch` are both **inside**
`FSMSnapshotState`, the restored payload carries the old Colony's
identity and epoch with it.

Left alone, that is a split-leader generator. The restored node would
present the *same* Colony identity to the original members, which
would see a peer claiming a Colony they are already part of, and the
two would be the same Colony by identity and two instances by state.

Stage 1 and Stage 2 made the identity and the epoch real, and put both
inside the snapshot state so they could not be lost. That is what makes
this hazard reachable: the identity now travels with a restore, and a
restore is precisely the case where it must not.

### What was actually true before this change

`internal/raft/seed.go`'s `SeedSnapshot` copied the caller's state
into the snapshot verbatim. A restored node therefore came up
carrying the original Colony's identity, and the restored node's
leadership watcher found an identity already set and reported it as
its own.

Nothing about the restore path consulted the Colony sidecar either.
`HasExistingState` looks at the bolt file and the snapshot store, so a
node whose raft state had been **wiped** but whose sidecar survived
could be restored into — even though that node belonged to a Colony
that might still be running.

## Decision

### A restore produces a NEW Colony identity and epoch 1

`SeedSnapshot` replaces the restored payload's identity and epoch with
fresh ones: a newly generated Colony ID, and epoch 1.

**The restored state's CONTENTS are preserved as data. Its identity is
not.** The VMs, networks, jails and API keys in the archive are still
there after the restore; what is not there is the claim that this node
is a member of the Colony the archive was taken from.

The epoch is 1 for the reason ADR-0156 gives: there is nothing in the
existing state from which a generation count could be recovered, and 1
is the only consistent starting value. A restored Colony is a
first-generation Colony.

**The caller's payload is not mutated.** The state is copied before
the identity is replaced, because a caller that inspects its own
payload afterwards must not find its identity rewritten.

### Restoring into a directory that belonged to a Colony is refused

ADR-0156 requires it: *"Restoring into a node that is currently a
member of a live colony is refused by default. It is a destructive
operation on a live colony's identity, and it must not be reachable
as a side effect."*

There are two cases, and only the first was covered:

- **The node's raft state is still on disk.** Already refused, by
  `HasExistingState`.
- **The node's raft state was wiped, but its sidecar survived.** This
  is the case that was reachable, and it is now refused too. The
  sidecar records which Colony the directory belonged to, and
  restoring there would form a competing instance of a Colony that
  may still be running.

The sidecar is normally a *hint* rather than the authority on
identity — the replicated state is the authority. But for a **refusal**
it is exactly the right evidence, and a refusal is the safe direction.
The refusal names the Colony the directory belonged to, so an
operator can see what they are about to disturb.

An operator who genuinely wants to restore there moves the whole data
directory aside (`raftd -reset`), which carries the sidecar with it —
the same gesture that declares "this is a new Colony, deliberately".

### The rejoin path is the normal join flow

ADR-0156's third requirement: *"To rejoin the existing colony with
restored data, the operator performs the restore into a separate
directory and then joins the colony through the normal, authorized
join flow, so that the joining node's identity is negotiated rather
than assumed."*

This falls out of the first two decisions rather than needing a new
mechanism. A restored node is a new Colony, with a new identity, and
it is not a member of anything. To bring it into an existing Colony,
the operator joins it — through ADR-0147's authorized join flow, which
negotiates the identity rather than assuming one. The sidecar check
makes the accidental path (restore into the member's own directory)
unreachable, so the deliberate path is the only one left.

**The restored node's own sidecar** is written for its NEW Colony by
the same leadership watcher that writes it for a bootstrapped node —
the sidecar records the Colony this directory now belongs to, which
after a restore is the new one.

## What this ADR does not cover

- **The epoch advance on a reset that preserves identity.** ADR-0182
  provides the `AdvanceColonyEpoch` command and its rules, and wires
  no caller to it. The reset that would want it is a reset that
  PRESERVES the Colony's identity while superseding its generation,
  which is a different operation from the restore this record
  defines. A `raftd -reset` today moves the whole data directory
  aside, sidecar included, so the next start forms a genuinely new
  Colony with a new identity and epoch 1 — which is what this record
  makes explicit.
- **Cross-Colony restore of the archive itself.** The archive is
  portable and its contents survive; only the identity is replaced.
  Whether an operator may restore one Colony's data into another's
  directory is a question about the operator's intent, and the
  sidecar check answers it for the case that matters — a directory
  that already belonged to some Colony.
- **Stage 4 onward.** Membership preconditions, divergence detection
  and fencing tokens are ADR-0156's stages 4, 5 and 6, and are not
  touched here.

## Consequences

- A restore can no longer produce a node that claims to be a member
  of a Colony it is not part of.
- A restored node is unambiguously a new Colony, and the archive's
  contents are available as data to join with.
- The refusal is loud: it names the Colony the directory belonged to
  and the gesture that would clear it.

## Validation

- `internal/raft`: a seeded restore gets a new Colony identity and
  epoch 1 whatever the payload carried; the caller's own state is not
  mutated; a restore into a data dir that already has raft state is
  refused; a restore into a directory that belonged to a Colony is
  refused, and the refusal names the Colony.
