# ADR-0120: Managerd configuration rationale history

## Status

Accepted

## Context

Operators need to answer why a setting has its current value, where the
change came from, which resources depend on it, and whether the recorded
rationale still deserves trust. Current managerd configuration files hold
values, but not the event that established them. Existing values cannot be
reliably attributed to a default, operator, migration, incident, or
automation after the fact.

## Decision

The Machine Configuration UI records provenance for changes made through its
managerd-local settings forms. Each changed field gets its own event with the
previous/current values, an operator-selected origin (`default`, `operator`,
`migration`, `incident`, or `automation`), optional rationale, optional
evidence reference, and UTC timestamp. Origins are declarations supplied at
change time, not classifications inferred by Apiary. Empty origins from older
clients are treated as `operator` for compatibility; missing rationales remain
missing rather than receiving invented text.

Events are stored locally beside managerd.json in
`managerd.config-history.json`. They are not replicated through Raft. Saving
settings and their provenance is serialized by `nodeconfig.Manager`; if the
journal write fails, the configuration file is restored before the save
returns an error. The file uses the same atomic 0600 write posture as
managerd.json.

The new Machine detail page presents current value, origin, rationale,
evidence, and chronological changes. A mismatch between the live configured
value and the latest recorded value is flagged as a change outside this
history path. Rationale older than one year is flagged for review, and its
evidence reference is flagged as potentially stale when present. Evidence
references are not fetched or revalidated automatically. Administrators can
add a retrospective attestation to an existing value without changing it;
the entry is explicitly marked as an attestation, and does not claim to
reconstruct the original event.

The dependency view combines documented impact descriptions with direct VM
and jail references that can be matched against the current inventory. It
explicitly reports when the data model cannot prove a local physical
dependency, such as which Comb hosts a Colony-wide managed network. It is not
a complete dependency graph or a safety preflight.

## Scope and limits

- The first slice covers managerd settings saved through Machine's
  `UpdateNodeConfig` forms and the dedicated managerd bind-address form.
- Retrospective attestations are operator-supplied explanations recorded at
  the current time, not recovered historical facts.
- It does not yet cover frontend.json, restshimd.json, raftd.json, direct file
  edits, startup defaults/flags, or values changed by an external script.
- Values that predate the feature, or have no matching event, are labeled
  `origin unknown / not recorded`. Apiary does not reconstruct them from
  current config, Raft logs, shell history, Git blame, or assumptions about
  defaults.
- Secret contents (peer API key and raftd token) are never included in the
  journal or returned by the API. The UI shows only whether each is set.
- A provenance record is an operator-facing record, not tamper-proof audit
  evidence. A host administrator can modify local files, and user identity is
  not yet part of this history event.
- Dependency counts are current-state matches only. Missing, remote, or
  unmodeled dependency edges remain unknown, not absent.

## Verification

Unit tests cover changed-field history, origin validation, rollback on invalid
metadata, and secret redaction. Manager RPC and frontend tests cover history
delivery, provenance form forwarding, dependency presentation, and truthful
unknown-history display. Full repository tests, vet, and build are run for the
change.
