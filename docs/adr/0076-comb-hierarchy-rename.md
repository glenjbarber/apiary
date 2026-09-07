# ADR-0076: Retire "Hive", fold the old "Comb" tier into it

## Status

Accepted

## Context

ADR-0059 (and README.md's own "Vocabulary" section) established a
five-tier product hierarchy: `Apiary > Colony > Hive > Comb > Cell`,
where "Hive" named one physical node and "Comb" named the collection of
Cells running on it. In practice this had two problems, both raised by
the user directly rather than discovered independently:

1. **"Hive" reads as a typo of "bhyve"** in ordinary prose - the two
   words differ by one letter and Apiary's entire VM story is built on
   bhyve, so "the Hive is unreachable" and "the bhyve process died" sit
   uncomfortably close together in the same sentences, changelogs, and
   error messages.
2. **The old "Comb" tier never carried real weight.** ADR-0059's own
   Context section already disclaimed it as a UI grouping, "not a new
   persisted API resource," and that ADR's own later "Follow-up"
   section went further and removed Comb from the sidebar entirely
   ("It adds an abstract layer between Hives and Cells without
   providing a separate destination or useful operator action").
   Nothing in `api/`, `internal/`, or `cmd/` has ever modeled a Comb as
   a distinct type, RPC, or resource - only the vocabulary section and
   a handful of template strings ever used the word.

Two smaller alternatives were considered and rejected before landing on
the fix below:

- **Rename only "Hive" to something else, keep five tiers** (e.g.
  `Apiary > Colony > Box > Comb > Cell`). Rejected: "Box" is already
  heavily overloaded in computing generally, and no other beekeeping
  term tried on for this fit better without reading as a stretch.
- **Rename "Colony" to "Swarm" instead of touching "Hive"**. Explored
  first, but rejected once it became clear a Colony is already both
  the physical set of nodes and the logical raft cluster at once (every
  Hive that's a raft member is definitionally part of the same
  Colony) - there is no present distinction between "physical" and
  "logical" topology in Apiary's design for "Swarm" to usefully name
  separately from what "Colony" already means. Introducing that split
  would be a real architectural decision, not a naming pass, and
  wasn't what was actually being asked for.

## Decision

Fold the two problems into one fix: retire "Hive" and let "Comb" take
over its meaning directly, rather than inventing a sixth word.

New hierarchy: **`Apiary > Colony > Comb > Cell`** (four tiers, was
five).

- **Apiary** - the complete system. Unchanged.
- **Colony** - the entire cluster: every Comb and everything running on
  them. Unchanged in meaning; still names the complete cluster, not an
  individual node.
- **Comb** - one physical Apiary node/host. This is the changed
  definition - previously "Hive" named the node and "Comb" named that
  node's own collection of Cells. The old collective meaning is not
  preserved under a new name; it is dropped, matching ADR-0059's own
  conclusion that it never did real work.
- **Cell** - one individual VM or jail. Unchanged.

**Scope of this rename, matching the precedent every naming-only
product-language change in this codebase already follows (README.md's
own "not a code migration" disclaimer predates this ADR and still
applies):**

- Updated: README.md's Vocabulary section, other README prose using
  "Hive" as a descriptive word, and `web/templates/*.html` strings that
  render "Hive"/"Hives" as literal UI text (headings, labels, help
  text, tooltips).
- Left alone, deliberately: every existing Go identifier, struct field,
  proto field, CLI flag, and API resource name that happens to contain
  "Hive" (for example `HiveID`, `PlacementHives`, `WhyNotHiveError`,
  `WhyNotHiveReboot`, the `account-hive` CSS class) - renaming these is
  a real refactor with its own regression surface, not a documentation
  or copy change, and buys nothing the UI-facing rename doesn't already
  deliver. New code written after this ADR should prefer "Comb"-based
  names where a fresh identifier is being chosen anyway; nothing
  existing is being touched to get there.
- Left alone, deliberately: the assumption-register scope-string format
  (`"hive:" + nodeID`, matched literally in
  `internal/manager/server.go`). This is a real, already-persisted data
  format - existing operator-authored claims may already use a
  `hive:<id>` scope value - and changing the accepted string would
  silently break them. The template copy that documents this exact
  scope syntax (`web/templates/assumption_register.html`,
  `web/templates/simulate.html`) keeps showing `hive:<id>` literally
  for the same reason: it must continue to describe what the backend
  actually accepts, not the new product vocabulary.
- Left alone: historical ADR text (including ADR-0059's own Context and
  Follow-up sections quoted above, and ADR-0065's
  "Cross-Hive console tunnel" title and filename) - this project treats
  ADRs as a preserved record of the decision as it was made, not a
  living document to rewrite when terminology moves on later. ADR-0059
  gets a short pointer note to this ADR instead of an in-place rewrite,
  matching the precedent ADR-0068 and ADR-0070 already established for
  a later change invalidating part of an earlier decision.

## Consequences

- "Hive" no longer appears anywhere a user reads product copy, removing
  the bhyve/Hive visual-typo problem at its source.
- The vocabulary is simpler (four tiers instead of five) with no loss
  of expressiveness, since the tier removed was already established
  (ADR-0059) as never having had independent identity in the system.
- No Go code, proto schema, database/JSON field, CLI flag, or API
  surface changes as a result of this ADR - this is exactly as
  low-risk as README.md's own long-standing "not a code migration"
  guarantee promises.
- A reader of ADR-0059 needs this ADR's pointer to know its Context and
  Follow-up sections describe vocabulary that has since changed; the
  underlying sidebar/navigation decisions those sections made are
  otherwise unaffected and remain accurate.
- "Frame" (previously reserved by README.md for a possible future
  subdivision inside a Hive) is no longer reserved for anything - the
  five-tier structure it was defined against no longer exists. A future
  ADR is free to introduce a new subdivision concept under Comb if and
  when a real need for one appears; nothing here reserves a name for
  it in advance.
