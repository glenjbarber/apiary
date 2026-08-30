# ADR-0019: session-based login for the web UI

## Status

Accepted

## Context

ADR-0014 shipped `cmd/frontend` with an optional single-shared-password
gate implemented as HTTP Basic Auth (`APIARY_UI_USER`/
`APIARY_UI_PASSWORD`, `internal/frontend.BasicAuth`). The user asked for
a real login page instead, with one explicit constraint driving the
whole design: **"I do not want to impede your ability to just go and
click through as you need to."** Basic Auth's browser-native credential
prompt isn't something a page-navigation/form-fill/click automation flow
can drive the same way a real HTML form can — this ADR replaces it with
a session-cookie login instead of keeping both mechanisms in parallel.

## Decisions

### Replace `BasicAuth` entirely, don't keep it alongside the new login page

`internal/frontend/auth.go` and `auth_test.go` were deleted rather than
left in place as an alternative/fallback. Nothing else in the codebase
used `BasicAuth`, and keeping a whole unused file plus its tests around
for a hypothetical future consumer violates the project's own
no-speculative-code guideline. `APIARY_UI_USER`/`APIARY_UI_PASSWORD`
keep their names and both-or-neither semantics — only the mechanism
behind them changed.

### An in-memory `sessionStore`, not a signed/stateless token

`internal/frontend/session.go`'s `sessionStore` is a
`map[token]expiry` guarded by a mutex, matching this project's current
single-developer/local-network stage (same reasoning ADR-0014 already
applied to `BasicAuth` itself). Sessions don't survive a `frontend`
restart — logging in again once after a deploy isn't a real burden here.
A real multi-instance deployment would need a shared/signed session
store instead; not a concern yet.

### Fixed 24-hour TTL, not a sliding window

`sessionTTL` is checked and enforced only at read time (`Valid` sweeps
an expired entry out lazily on next use) rather than refreshed on every
request. Simpler to reason about, and re-logging in once a day is not a
real burden for this project's stage.

### Cookie is `HttpOnly` + `SameSite=Lax`, no `Secure` flag

`HttpOnly` blocks any client-side script (including a compromised HTMX
page load) from reading the session token. `SameSite=Lax` blocks the
cookie being sent on cross-site requests that aren't plain top-level
navigations, closing off the obvious CSRF vector for the login/logout
POSTs without needing a separate CSRF token. `Secure` is deliberately
left off: the actual deployment is plain HTTP on a local network
(matching every other service in this project — none of `raftd`,
`managerd`, or `restshim` have TLS either), and a `Secure` cookie
would simply never be sent at all in that setup.

### Credential comparison uses `crypto/subtle.ConstantTimeCompare`

`handleLogin` compares both username and password with
`subtle.ConstantTimeCompare` rather than `==`, avoiding a timing
side-channel on a single shared credential pair — cheap to do correctly
here, no reason not to.

### Open-redirect protection on `next`

The `next` query/form parameter (where to send the user after a
successful login — normally the page they were originally trying to
reach) is validated by `isSafeRedirectPath`: it must start with `/`, must
not start with `//`, and must not contain `://`. Without this, a
crafted link like `/login?next=https://evil.example` would silently
redirect a freshly-authenticated user off-site. An unsafe `next` falls
back to `/` rather than erroring, since this only ever affects
convenience (where you land after logging in), not whether login itself
succeeds.

### The auth gate is HTMX-aware

`redirectToLogin` checks the incoming request's `HX-Request` header: a
plain navigation gets an ordinary `302`, but an HTMX request (e.g. the
VMs table's own 3s polling fragment, ADR-0016) gets an `HX-Redirect`
response header instead, with the wrapped body otherwise empty. A bare
`302` to an HTMX `hx-get` poll would just swap the login page's HTML
into whatever fragment target issued the request; `HX-Redirect` tells
htmx to navigate the whole browser tab, exactly like a real login
expiry should behave.

### The login page is a plain HTML form, not an HTMX fragment

Unlike every other mutating page in this project (ADR-0014), `/login`
is a full page load on submit (`http.Redirect`, not an HTML fragment
swap). This is what keeps it drivable by ordinary
navigate/fill-field/click automation — the explicit constraint behind
this whole ADR — rather than requiring JS-driven form interception to
follow.

## Consequences

- Verified with 12 new `httptest`-based unit tests
  (`internal/frontend/server_test.go`) covering: unauthenticated access
  redirecting to `/login` (both plain and `HX-Request` variants), wrong
  credentials re-rendering the form with an error and no cookie, correct
  credentials granting a working session and honoring a safe `next`,
  rejecting `//evil.com`/`https://evil.com`-style `next` values, logout
  invalidating the session so a subsequent request is gated again, and
  the nav's "Log out" link only appearing when auth is actually enabled.
  All 25 pre-existing `internal/frontend` tests continue to pass
  unchanged with auth left disabled (the default `newTestServer` helper
  now delegates to `newTestServerWithAuth(t, client, "", "")`).
- Verified live against the real `frontend` binary running on
  `apiarium`: navigating to `/vms` while logged out redirected to
  `/login?next=%2Fvms`; logging in with `APIARY_UI_USER`/
  `APIARY_UI_PASSWORD` redirected straight back to `/vms` (not just
  `/`); clicking "Log out" returned to `/login`; a subsequent direct
  navigation to `/vms` was blocked again, confirming the session was
  actually invalidated server-side and not just hidden client-side.
- Still just one shared username/password, not a real user/role system —
  same limitation ADR-0014 already called out, now just implemented with
  a session instead of Basic Auth. A real deployment needing per-user
  accounts or audit trails needs more than this.
- `raftd`, `managerd`, and `restshim` remain completely unauthenticated —
  this ADR only covers `cmd/frontend`'s own gate in front of itself; it
  does not protect the underlying RPC surface those other components
  expose.
