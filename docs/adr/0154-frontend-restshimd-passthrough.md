# ADR-0154: Reach restshimd through a frontend path prefix

Status: Accepted, implemented
Supersedes: None
Superseded by: None
Affected projects: Apiary
Legacy identifier: ADR-0154 (Apiary)
Loreloom identifier: Pending allocation by Glen

## Status qualifications

Accepted. Implemented in the tree: `internal/frontend/api_proxy.go` holds the
allow-list and the reverse proxy, `internal/frontend/server.go` gates `/api/`
behind a bearer header, and `cmd/frontend` refuses to start the proxy without
TLS and a manager API key. Covered by `internal/frontend/api_proxy_test.go`
and `cmd/frontend/main_test.go`.

The number 0154 is the next free apiary-local `docs/adr/` number after ADR-0153; Loreloom allocation is Glen's to do.
Dated 2026-10-18.
The section "Decisions for Glen" lists the choices this proposal makes by default and that Glen may override before any code is written.

### What the implementation resolved

Decision 3's first option is what shipped: `/api/` accepts a bearer header
only, never a session cookie, so no CSRF token is needed and the proxy
strips `Cookie` before forwarding. Decision 1's allow-list is the single
table in `internal/frontend/api_proxy.go`, and a test asserts every entry maps
to a ManagerService RPC that carries an explicit role requirement in
`internal/manager/auth.go` - so an allow-listed route can never silently
inherit the fail-closed Admin default, and Status remains the deliberate
auth-exempt exception.

Two of the "Decisions for Glen" items are still open and are noted here
rather than answered by the code:

- Decision 6 (read-only allow-list first) was NOT taken. The allow-list
  covers restshimd's VM, jail, cell, network and ISO routes, all of which
  can write, through managerd's per-RPC role checks.
- Decision 5 was taken as written: `api_proxy_enabled` defaults to `false`,
  so a Colony that does not set it sees no change in behaviour.

## Context

ADR-0139 and ADR-0147 keep restshimd on `127.0.0.1:8081`. The reasons are that restshimd is a full read/write control API, and that it has no authentication of its own. Its only boundary is managerd's per-key auth (ADR-0023), reached by forwarding the caller's `Authorization` header (ADR-0024).

Remote callers therefore use an SSH local forward today. That works, but it is awkward for an automated client such as the Claude plugin in use against this Colony, and it does not scale to a round-robin name for several nodes.

The question is whether a remote caller can reach restshimd through a path on the frontend instead, while restshimd stays on loopback.

### What exists today

- `frontend` listens on `0.0.0.0:8080` for browsers (ADR-0147). It is a `ManagerService` client, not a restshimd client. ADR-0014 deliberately keeps `frontend` and `restshim` as independent siblings.
- No reverse proxy exists anywhere in `internal/frontend` or `cmd/frontend`.
- `frontend`'s `ServeHTTP` (`internal/frontend/server.go`) requires a valid session cookie (`apiary_session`, `internal/frontend/session.go`) for every path except `/login` and `/static/`, when login is enabled. A bearer-token client has no cookie and would be redirected to `/login`.
- Session state is an in-memory cookie store. Its cookie is marked `Secure` only when frontend TLS is on (`cmd/frontend/main.go`, `tlsEnabled`).
- No CSRF token is issued or checked anywhere in `internal/frontend`.
- Frontend TLS is optional. `tls_cert` and `tls_key` must be set together, and when they are unset frontend serves plain HTTP.

## Decisions

### 1. The proxy lives in `frontend`, under `/api/v1/`, and forwards to restshimd on loopback

`frontend` forwards `/api/v1/*` to `127.0.0.1:8081` on the same host, using `httputil.ReverseProxy` or equivalent. The path prefix is stripped so restshimd sees `/v1/*` unchanged. restshimd keeps its loopback bind, so no change to ADR-0139 or ADR-0147 is needed for restshimd itself.

The scheme the proxy dials is restshimd's own to decide, and is read from restshimd's own config file rather than assumed: `cmd/restshimd` serves HTTPS whenever its config sets `tls_cert`/`tls_key` (the shipped `restshimd.json.sample` sets both), so a proxy that assumed plaintext would fail every request against such an installation with "Client sent an HTTP request to an HTTPS server". This is the same posture ADR-0102 already established for the sibling daemons - managerd reads frontend.json, restshimd.json and raftd.json on the same host because they are co-located and each owns exactly one file - applied to the one file that describes the listener this proxy forwards to. The trust anchor is the certificate named in that config, trusted instead of the system pool, mirroring `internal/tlsdial`'s own rules for reaching a sibling daemon whose certificate this project issued itself.

Only the `/v1/*` routes listed in the implementation's allow-list are forwarded. Any other path under `/api/` returns `404`. This keeps the proxy from exposing future restshimd routes by accident.

### 2. Bearer-token callers bypass the cookie gate, and only for `/api/v1/`

Browser sessions and bearer keys authenticate differently, and the cookie gate cannot serve a bearer client. For `/api/v1/*` only, `ServeHTTP` accepts a request that carries an `Authorization: Bearer` header and skips the cookie check. The header is forwarded unchanged to restshimd, which forwards it to managerd. Managerd's per-key auth (ADR-0023, role checks in ADR-0030) then decides what the caller can do.

A request to `/api/v1/*` without a bearer header is refused with `401`, even when it carries a valid session cookie - see decision 3, whose first option settles this: the cookie gate does not apply on this path at all, because a session cookie is never sufficient here.

The cookie is never forwarded to restshimd. The proxy strips `Cookie` before forwarding.

### 3. Browser-originated writes need CSRF protection before the proxy ships

A browser that is logged in to the frontend keeps its session cookie for the frontend origin. Cross-site requests can cause the browser to send that cookie, and the frontend has no CSRF protection today. Once `/api/v1/*` accepts cookies, a cross-site `POST`, `PUT`, or `DELETE` could reach restshimd's write routes with the victim's session.

Two ways to close this, and this ADR picks the first:

- **Cookies are not accepted on `/api/v1/*`.** Only bearer headers are, so a browser cannot send credentials there implicitly. Browsers can still call the API from a page that holds a key, but that is an explicit act, not an ambient one.
- Issue and check a CSRF token. This is more work and not needed for the first version.

### 4. TLS is required for the proxy's listener

A bearer key sent over plain HTTP crosses the network in cleartext. The proxy's `/api/v1/*` path is refused over plain HTTP, and the frontend fails to start with `/api/` enabled and no `tls_cert`/`tls_key` set. This is a startup check, not a warning.

### 5. Off by default

The proxy is controlled by a new frontend config field, `api_proxy_enabled`, default `false`. A Colony that does not set it sees no change in behavior.

### 6. Read-only option is deferred

ADR-0153 proposes a Viewer-tier route list. This ADR does not limit the proxy to read routes. The proxy forwards whatever the allow-list permits, and managerd enforces role checks per RPC. Restricting the proxy to reads would be a further allow-list change and is left to Glen.

## Consequences

- A remote client can use `https://<frontend-host>:8080/api/v1/...` with a bearer key. Round-robin DNS across nodes works once each node's frontend serves the proxy, because each node proxies to its own loopback restshimd.
- restshimd's bind address and its unauthenticated-transport rule stay as they are.
- The frontend's attack surface grows by the allow-listed `/api/v1/*` routes. Those routes can write, through managerd's role checks, so the API's write surface is reachable from the LAN wherever the frontend is.
- Round-robin DNS does not give failover for writes that go to a node which cannot reach the leader. Write requests are forwarded to the leader by managerd (ADR-0037), so a write sent to any node should still reach the leader.

## Decisions for Glen

1. Resolved: `/api/v1/*` accepts only bearer headers (decision 3, first option). Cookies are not accepted on this path, and no CSRF token is needed for the first version. Glen confirmed this on 2026-10-18.
2. Whether the allow-list covers all of restshimd's routes or only reads at first (decision 6).
3. Whether `api_proxy_enabled` should default to off (decision 5), or be on for Colonies with TLS.
4. Whether to amend ADR-0139 and ADR-0147 to note the proxy as the sanctioned remote path, or leave them as they are and treat the proxy as a separate exception.

## Implementation notes

These are for the implementer, not decisions:

- Add the proxy in `internal/frontend`, with the allow-list in one table so it is testable, as ADR-0150 and ADR-0153 do for their route tables.
- Add a test that every allow-listed route corresponds to an RPC in `requiredRole`.
- Add a test that a request to `/api/v1/*` without a cookie or bearer header is redirected or refused, and that the `Cookie` header never reaches restshimd.
- Add a test that the proxy completes a real request against a TLS-serving upstream, so the scheme is decided by restshimd's config rather than assumed.
