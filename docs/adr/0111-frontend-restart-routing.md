# ADR-0111: Route action requests to home, not a 405, after a frontend restart invalidates a session

## Status

Accepted

## Context

The user's 2026-09-21 17:19 EDT TODO (recorded in SHARED.md) read only:
"after `apiary_frontend` restarts, route all web requests to `/`." This
had no prior design or filed bug report, so this ADR records the
investigation that resolved the ambiguity before any code changed.

`internal/frontend/server.go` uses `net/http`'s method-aware
`ServeMux` (`http.NewServeMux()`), with routes registered per method,
e.g. `"GET /vms/{id}"` and separately `"POST /vms/{id}/lifecycle"`,
`"DELETE /vms/{id}"`, `"POST /jails/{id}/lifecycle"`, and similar
action-only routes with no GET counterpart at the same path.

`Server.sessions` (a `*sessionStore`) is in-memory only, with no
persistence across process restarts - confirmed by reading its
construction (`newSessionStore()` in `NewServer`) and doc comments.
Separately, `cmd/frontend/main.go` determines whether login is enabled
(`s.auth`) once at process startup via managerd's `Status` RPC and
caches that decision for the process's entire lifetime ("there is no
later recheck", per that file's own comment, and per commit
`56ed7148`, "Document frontend restart after PAM changes"). Both facts
mean an `apiary_frontend` restart - whether from a binary upgrade, a
manual `service apiary_frontend restart`, or the UI's own
"Restart Node Service" action for `apiary_frontend` itself
(`internal/manager/services.go`'s `restartable: true` entry,
served through `handleRestartNodeService` in
`internal/frontend/machine.go`) - unconditionally invalidates every
open browser session the instant the new process starts.

`ServeHTTP`'s existing login gate (server.go, `s.auth != nil` branch)
already handles the general case well: an unauthenticated request is
sent to `/login`, and `redirectToLogin` preserves the original URL as
a `next` parameter so a normal page reload lands back where the user
was, via `isSafeLoginReturnPath` - which already excludes background
fragment and websocket endpoints (`/vms/rows`, `/jails/panel`, `/isos`,
any `.../ws` or `.../content` suffix) precisely because those are not
complete, safely-GET-replayable documents.

That existing exclusion list missed one more category with the same
underlying problem: action routes reached by POST/DELETE, such as
`POST /vms/{id}/lifecycle`, `DELETE /vms/{id}`, or
`POST /jails/{id}/lifecycle`. `redirectToLogin` preserved these paths
as `next` regardless of the original request's method. `handleLogin`'s
own post-authentication redirect (`http.Redirect(w, r, dest, ...)`)
is always effectively a browser GET. GET-replaying a POST-only or
DELETE-only path matches no registered route at that method, so
`net/http`'s `ServeMux` returns a bare, unstyled
"405 Method Not Allowed" - not the confusing dead end from an
error message the user could act on, but a plain-text response with no
navigation back into the app.

This reproduces concretely: a browser tab sitting on a VM detail page
mid-click on a lifecycle action (start/stop/restart), or partway
through submitting a delete confirmation, at the exact moment
`apiary_frontend` restarts (session store cleared, or - when a PAM
change is also in flight per commit `56ed7148` - login newly enabled)
sends that click's POST/DELETE with a cookie the new process has never
seen. The gate correctly detects "no valid session" and redirects to
login, but the preserved `next` doomed the post-login redirect to a
405. This is option (a) from the investigation brief: a real, if
narrow, "stale request after restart gets a confusing error instead of
landing somewhere sane" bug, not a literal misrouting of already-working
requests. No evidence of option (b) (steady-state requests being served
incorrectly) was found anywhere in routing, template, or handler code.

## Decision

Restrict `next`-URL preservation in `redirectToLogin` to GET requests.
Any other method (POST, DELETE, PUT, PATCH, etc.) that hits the
unauthenticated-session gate now redirects to `/login` with no `next`
parameter, so `handleLogin`'s existing default (`dest := "/"` when
`next` is empty or unsafe) takes over and the user lands on the
cluster overview page after logging back in, exactly like the existing
fragment/socket case just above it in the same function.

This is a minimal, targeted extension of an already-correct pattern
(`isSafeLoginReturnPath`) rather than a new subsystem: the underlying
rule in both cases is "only preserve a `next` URL that is safe to
GET-replay after login," and a POST/DELETE-only action route fails that
test exactly like a fragment or websocket path already does.

Steady-state routing (any authenticated request, and any unauthenticated
GET to a real page) is unchanged. GET requests still preserve `next`
through login exactly as before.

## Out of scope

- No attempt to keep an in-flight action request "alive" across a
  restart, replay it automatically, or persist sessions across process
  restarts. The session store's restart-clears-everything behavior is
  unchanged and is not itself considered a bug here - the fix is only
  about where the user's browser ends up next, not about preserving
  the interrupted action.
- No change to the frontend restart-time PAM-detection behavior
  documented in commit `56ed7148` (frontend still reads login-enabled
  status only once, at its own startup).
- No change to `handleRestartNodeService` or the restart RPC path
  itself (`internal/manager/server.go`'s `RestartNodeService`).
- No blanket "redirect everything to `/` right after startup" timer or
  boot-window middleware. Investigation found no evidence of literal
  routing being wrong; the only bounded, reproducible defect was the
  next-URL replay hazard fixed above, so nothing more speculative was
  built.
- No changes for the login-disabled deployment mode (`s.auth == nil`):
  there is no session gate to trigger this failure mode when login is
  off, and none was found.

## Consequences

- A user whose action click (VM lifecycle change, jail delete, etc.)
  lands mid-restart now gets a normal re-login flow ending on the
  cluster overview page (`/`), instead of a bare 405 error page with no
  way back into the app.
- The interrupted action itself is still not retried or preserved -
  the user must repeat it after logging back in, same as they always
  had to for the already-excluded fragment/socket cases.
- `docs/adr/0111-frontend-restart-routing.md` (this file) is the record
  of why "route all web requests to `/`" resolved to this specific,
  narrow fix rather than a broader boot-window redirect: the evidence
  pointed at a concrete, reproducible next-URL replay bug, not a
  general routing problem.

## Testing

`internal/frontend/server_test.go` adds
`TestServer_AuthRestart_ActionRequestDefaultsToHomeAfterLogin`, which:

- Confirms POST/DELETE requests to action-only routes
  (`/vms/{id}/lifecycle`, `/vms/{id}`, `/jails/{id}/lifecycle`) sent
  with no valid session redirect to plain `/login` (no `next`).
- Confirms a GET to a real page (`/vms/{id}`) still preserves `next`
  through the same gate, so steady-state page-reload behavior is
  unaffected.
- Confirms that logging back in after one of the action requests above
  lands on `/`, not an attempted GET-replay of the original path.

The existing `TestServer_AuthEnabled_UnauthenticatedFragmentDefaultsToHomeAfterLogin`,
`TestServer_AuthEnabled_UnauthenticatedRequestRedirectsToLogin`,
`TestServer_Login_RedirectsToSafeNextURL`, and
`TestServer_Login_RejectsOpenRedirectNextURL` tests continue to pass
unmodified, confirming the fix is additive to the existing safe-next-URL
logic rather than a replacement of it.
