# ADR-0039: per-role password-change feature for the web UI

## Status

Accepted

## Context

The web UI's login (ADR-0030) authenticates against real local UNIX
accounts via PAM, but Apiary itself has never offered any way to change
one of those accounts' passwords - only by SSHing into the node and
running `pw usermod` directly. The user asked for a real in-app feature
with a specific, asymmetric authorization matrix:

- **Admin** can change its own password, Operator's, and Viewer's.
- **Operator** can change its own password and Viewer's - never Admin's.
- **Viewer** can change no one's password, not even its own.

## Design

The matrix above isn't a plain "role X can act on anything at or below
its own rank, including itself" rule - Viewer is explicitly excluded
from touching even its own password, which a self-inclusive hierarchy
wouldn't produce on its own. The rule that reproduces the matrix exactly,
reusing the existing `manager.Role.Satisfies` rank comparison
(`internal/manager/auth.go`, already used for RPC role-gating) with no
new hierarchy logic at all:

```go
func canChangePassword(actorRole, targetRole manager.Role) bool {
	return actorRole != manager.RoleViewer && actorRole.Satisfies(targetRole)
}
```

Verified against all nine (actor, target) pairs (unit-tested exhaustively
in `password_test.go`) - Viewer is vetoed outright regardless of what
`Satisfies` would otherwise say; Operator and Admin fall out of the
existing rank comparison for free.

- `cmd/frontend` already runs as root (confirmed live via `ps` on
  `apiarium`), so no new privilege-escalation mechanism was needed - a
  new `PasswordSetter` interface's real implementation
  (`UnixPasswordSetter`) shells out to `pw usermod <user> -h 0`, the
  same invocation already used by hand this session to set up the
  project's own `admin`/`operator`/`viewer` test accounts. A fake
  implementation backs the unit tests, matching the existing
  `pam.Authenticator` constructor-injection pattern this same package
  already establishes for login.
- `GET /users` is gated at `RoleViewer` (visible to everyone logged in
  - the same "always show the page, gate the actions" convention
  `/apikeys` already follows) and lists every `roleMap` entry
  (username + role, sorted for deterministic output - `roleMap` is a
  plain Go map). Each row's "Change password" action is only rendered
  when `canChangePassword(session.role, rowRole)` holds - decided once
  in Go and stored on the row (`userView.CanChange`), since a template
  action can't express this rule inline.
- `POST /users/{username}/password` is gated at `RoleOperator` as a
  coarse baseline (Viewer can never reach it at all), but the handler
  itself re-derives the target's role from `roleMap` and re-checks the
  full rule before doing anything - the route gate alone isn't
  sufficient, since Operator is let through but must still be blocked
  from targeting Admin.
- Changing *any* password - even an Admin changing its own - requires
  re-entering the *acting* user's own current password, verified via
  `s.auth.Authenticate` (the exact same check login itself performs).
  This proves it's really them acting, not just an unattended open
  session, before anything is changed - true even for the
  Operator-changes-Viewer case, where the acting Operator's own
  password is what gets re-verified, not the target's.
- A fixed 8-character minimum is enforced as a plain constant, not a
  configurable policy - there's no existing precedent in this codebase
  for tunable password rules, and this project has a handful of local
  test/demo accounts, not a real user base needing configurable policy.

## Consequences

- Full unit coverage: the authorization rule against all nine matrix
  entries, and handler tests (fake `PasswordSetter` + fake
  `pam.Authenticator`, no real `pw`/PAM needed) covering the
  Operator-can/Admin-cannot-be-targeted cases, the route-level Viewer
  block, wrong-current-password, mismatched confirmation, and
  too-short-password rejections.
- Live-verified on `apiverse`: as `ops`, changed `viewer`'s password
  successfully and was correctly refused when targeting `admin`; as
  `viewer`, the change action was both absent from the rendered page
  and rejected server-side when posted directly; the changed password
  was confirmed to actually work via a real subsequent PAM login.
- Not addressed: no password-complexity policy beyond the fixed length
  floor, and no forced re-login/session invalidation when an account
  changes its own password (an existing session, once issued, is
  independent of the underlying UNIX password until its own TTL expires
  - unchanged, pre-existing session behavior from ADR-0019, not a new
  gap introduced here).
