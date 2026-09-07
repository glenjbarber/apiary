# ADR-0074: Role-map editing UI

## Status

Accepted

## Context

ADR-0030 gave the web UI real PAM-backed login with tiered Viewer/
Operator/Admin roles, resolved through a `-role-map` startup flag
(`"admin:alice;operator:bob,carol;viewer:dave"`) deliberately kept
independent of any UNIX/AD group. That ADR's own "Deferred" section
named the gap this closes: there was no way to change who has a role,
or what tier they have, without hand-editing `-role-map` in `rc.conf`
and restarting `cmd/frontend` - the same class of gap ADR-0049 closed
for `managerd`'s own startup flags via the Machine Configuration page.

This is scoped narrowly, matching ADR-0030's own explicit boundary:
editing the role map is **not** account management. A role-map entry
grants an already-existing PAM/UNIX account an Apiary role; it never
creates, deletes, or otherwise touches the underlying account. Direct
Kerberos/LDAP client code and self-service account management remain
out of scope here exactly as they were in ADR-0030.

## Decision

A new `internal/loginconfig` package, mirroring `internal/nodeconfig`'s
role for `managerd` exactly, but scoped to just the role map:

- `DefaultPath = "/var/db/apiary/frontend-role-map.json"` - physical,
  per-node state, never routed through raft (who may log in to one
  Hive's web UI is that Hive's own concern).
- `Load()` returns `(cfg, exists, err)` - `exists` is false only when
  the file has never been written, in which case `-role-map`'s parsed
  value remains authoritative. Once the file exists at all - even
  holding an empty map, because every entry was deliberately removed
  through the UI - it wins forever. This mirrors `nodeconfig`'s own
  "the file, once written, wins" posture, and specifically avoids the
  bug an emptied-map/never-written ambiguity would cause.
- `Save()` replaces the whole file, never merges, validates every
  entry's role is one of the three real values before writing anything,
  and writes at `0600` - matching `nodeconfig.Manager.Save`'s own
  contract line for line.

`cmd/frontend/main.go` loads this after parsing `-role-map`, using its
content wholesale when it exists (the same "file overrides flag once
written" pattern ADR-0049 established for `managerd`).

### Server changes

`Server.roleMap` (previously a plain, read-only-after-construction
`map[string]manager.Role`) is now guarded by `roleMapMu
sync.RWMutex`, since it's live-editable through the Users page while
also being read on every login and every Users-page render.
`roleMapStore roleMapPersister` (nil-able) persists an edit; wired via
a new `SetRoleMapStore` **post-construction setter**, not a `NewServer`
parameter - following the exact `SetAssumptionRegister`/`SetJailConsole`
precedent (the latter since removed, ADR-0068) to keep this already-8-
parameter constructor's signature stable across this package's many
focused tests.

Three new Admin-only routes on the existing Users page:

- `POST /users` - add a new username+role entry. Does not create a
  PAM/UNIX account (see Context) - a login attempt for a username with
  no matching real account simply never succeeds, exactly as it always
  has.
- `POST /users/{username}/role` - change an existing entry's role.
- `DELETE /users/{username}` - remove an entry entirely. The
  underlying account is untouched; the user just can't log in until
  re-added.

All three funnel through one `updateRoleMap(target string, role
*manager.Role)` (nil role means remove): it builds the full proposed
map, refuses the edit outright if it would leave **zero** Admin
accounts, then - when `roleMapStore` is configured - persists before
ever touching the in-memory map. This ordering (validate, persist,
apply) mirrors `nodeconfig`'s own update discipline exactly, so a save
failure (e.g. disk full) never leaves the running process holding a
role map that doesn't match what's on disk.

### The no-last-Admin rule, and why it's not ADR-0023's one-way door

ADR-0023's API-key `authEnabled` flag is a deliberate one-way door -
once auth is ever enabled, revoking every key locks the cluster down
permanently, with no in-band recovery. The role map doesn't need that
severity: refusing the *specific edit* that would remove the last
Admin costs nothing (the admin doing the edit just can't make that
particular change) and needs no permanent, unrecoverable-by-design
flag. This is a plain, cheap safety check, not an architectural
one-way door.

### CanAdmin in fragment re-renders

`renderUserPanelResult`/`renderUserPanelSuccess` (the handlers all
three new actions - and the existing password-change action - use to
re-render the panel after a POST) previously built `pageData` directly
rather than through `withAuthFields`, which also sets `HiveID`/
`AuthEnabled` these fragments don't need. That meant `pageData.CanAdmin`
was never set on any post-submission re-render - harmless before this
change since nothing read it, but it would have made every new
Admin-only control silently vanish after the very first successful
edit had it gone unnoticed. Fixed by setting `CanAdmin:
actorRole.Satisfies(manager.RoleAdmin)` explicitly in both helpers.

## Not addressed

- No self-service account management or direct Kerberos/LDAP client -
  ADR-0030's own deferred scope, unchanged.
- No audit log of who changed which role when - the persisted file is
  a snapshot of current state only, not a history.
- An already-logged-in session's role does not change until that user
  logs in again, even if their role-map entry is edited mid-session -
  the session store caches the role at login time (ADR-0019's existing
  design), and this ADR doesn't revisit that.

## Verification

New tests in `internal/frontend/rolemap_test.go` (11, all passing):
adding a user, rejecting an invalid role, rejecting a duplicate
username, rejecting an empty username, Operator forbidden from adding/
removing, changing an existing user's role, removing a user, refusing
to remove the last Admin, refusing to demote the last Admin, and - the
direct regression test for the validate-then-persist-then-apply
ordering - a failing store leaves the in-memory map unchanged. New
tests in `internal/loginconfig/manager_test.go` (7): missing-file
zero-value, save/load round-trip, replace-not-merge, the empty-map-
still-authoritative case, invalid-role/empty-username rejection, and
the `0600` file mode.

`go build ./...`, `go vet ./...`, `gofmt -l`, `git diff --check`, and
the full `go test ./...` suite all pass. FreeBSD cross-compile
confirmed for managerd/raftd/restshimd (unaffected); `cmd/frontend`
itself was verified with a native macOS build (it already can't
cross-compile to FreeBSD from macOS at all, per ADR-0030's own cgo/PAM
constraint) and a live run confirming `/users` renders correctly.
**Not live-verified with a real PAM login** as part of this pass - both
`handleUsersPage` and the three new handlers require an active session,
and testing that live needs real throwaway UNIX accounts the way
ADR-0030's own original work was verified on `apiarium` - deferred to
actual deployment verification rather than this local macOS dev
environment, matching that same precedent.
