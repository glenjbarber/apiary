# ADR-0086: Remove -role-map; first successful login becomes Admin

## Status

Accepted

## Context

While discussing removing `frontend`'s cgo/PAM native-build requirement
(a separate, larger question not resolved here), we found `-role-map`
(ADR-0030's original CLI flag mapping usernames to Apiary roles) is
already semi-dead: `internal/loginconfig.Manager` persists role
assignments to `/var/db/apiary/frontend-role-map.json` (ADR-0074's
Users-page editor), and once that file exists at all - even holding an
empty map - it wins over the flag forever
(`cmd/frontend/main.go`, previously lines 130-145). So `-role-map` only
ever mattered as the seed for a fresh Comb's very first boot, before
the Users page had ever been used.

This ADR removes the flag entirely and replaces that one remaining
job - seeding the very first Admin account - with a
convention-over-configuration bootstrap: the first successful login on
a Comb with no role-map file yet automatically becomes Admin. This
mirrors this project's existing taste for this kind of bootstrap (e.g.
`raftd`'s own default single-voter-if-no-`-join` behavior) and removes
one more manually-typed, easy-to-get-wrong flag.

## Design decisions

- **The bootstrap signal is "the role map is currently empty," not a
  new field.** `updateRoleMap` already refuses to ever save a role map
  with zero Admins (`roleMapHasAdmin`), and it's the only writer of the
  persisted file. So the role map can only ever be empty in one
  situation: a fresh Comb that has never completed this one-time
  bootstrap. No separate "bootstrap pending" flag/parameter exists
  anywhere - `len(s.roleMap) == 0` at the moment of a successful login
  *is* the signal, self-disabling forever the instant the first Admin
  is created.
- **`bootstrapFirstAdmin` checks and applies under one write-lock
  acquisition**, closing the race between two concurrent first-ever
  login attempts - only one can win; the other falls through to the
  normal no-role-map-entry rejection rather than also becoming Admin.
- **The mechanic is surfaced in the UI, not hidden.** Since this is a
  security-relevant default (see the disclosed risk below), the login
  page shows "No Apiary accounts exist yet on this Comb - the first
  successful login here becomes Admin" whenever the role map is empty,
  via a small `roleMapEmpty()` helper.
- **`updateRoleMap`'s persist-then-apply body was extracted into
  `applyRoleMapLocked`**, shared by both the Users-page edit path and
  the new bootstrap path - exactly one place ever writes
  `internal/loginconfig`.

## The real accepted risk

Whoever successfully authenticates first against a Comb with no
role-map file yet becomes Admin - not necessarily whoever the operator
intended. An operator bringing up a fresh Comb should do one of:
- log in as the intended Admin immediately after starting `frontend`;
- keep the frontend port unreachable/firewalled until they do;
- ensure no other UNIX account with a working password can reach
  `-pam-service`'s PAM stack before then.

This is the same category of "convention over configuration trades
away an explicit safeguard" as `raftd`'s own default bootstrap - named
directly here, not silently assumed acceptable.

## Migration note for already-deployed hosts

`apiarium`/`apiverse` both currently pass
`-role-map "admin:admin;viewer:viewer;operator:ops"`. If
`/var/db/apiary/frontend-role-map.json` does not yet exist on a host
when this deploys, removing the flag means `viewer`/`ops`'s access is
not carried over automatically - only whoever logs in first becomes
Admin, and that Admin must re-add the other accounts through the Users
page afterward. Before deploying, check whether that file already
exists on each host:

```bash
ssh <host> "test -f /var/db/apiary/frontend-role-map.json && echo exists"
```

If it already exists (likely, since ADR-0074's Users page has
presumably been used on both hosts already), this migration concern
doesn't apply at all and the flag removal is a pure no-op for that
host's current accounts.

## Consequences

- `cmd/frontend/main.go` no longer has a `-role-map` flag or
  `parseRoleMap` function; `cmd/frontend/main_test.go` (which existed
  solely to test `parseRoleMap`) was deleted.
- ADR-0030 (original flag) and ADR-0074 (persisted override) are
  superseded for the bootstrap-seeding piece only - the Users-page
  live-editing feature itself, and everything else about PAM-backed
  login, is entirely unchanged.
- Full test coverage: a login against an empty role map grants Admin
  and persists it via the store; a different user's login afterward
  gets the normal rejection, not a second Admin grant; the login
  page's notice appears only when the role map is empty.
