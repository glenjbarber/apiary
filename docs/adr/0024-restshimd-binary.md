# ADR-0024: `cmd/restshimd` binary and per-request auth forwarding

## Status

Accepted

## Context

`internal/restshim` has existed since ADR-0011 as a fully tested REST/
JSON translation of `managerd`'s external gRPC API, but with no `cmd/`
binary hosting it — every other layer of the stack (`raftd`,
`managerd`, `frontend`) had a real running binary; `restshim` didn't.
This was named repeatedly (ADR-0014, ADR-0019, ADR-0023) as the one
remaining piece blocking the "tabled" Terraform-provider work: a
Terraform provider needs a REST endpoint to talk to, and there wasn't
one running anywhere. With `managerd` now having real API-key auth
(ADR-0023), the other missing piece — a credential a script or CI job
could actually hold — also exists, so this closes the gap: a `cmd/`
binary, plus wiring a caller's own key through to `managerd`.

## `cmd/restshimd` mirrors `cmd/frontend`'s dial pattern, not its auth pattern

`cmd/restshimd/main.go` dials `managerd` the same way `cmd/frontend`
does (`grpc.NewClient` with insecure local-network transport, a
`-manager-addr` flag) and just serves `internal/restshim.Server` over
`-http-addr` (default `127.0.0.1:8081`, one port over from
`frontend`'s `8080`). It deliberately does **not** get a
`RESTSHIMD_MANAGER_API_KEY`-style env var mirroring `frontend`'s
`APIARY_MANAGER_API_KEY`. `frontend` has exactly one identity — the UI
backend process itself, which every browser session shares — so
attaching one static key at dial time is correct there. `restshim` has
no identity of its own: it's meant to sit in front of external tooling
(`curl`, a CI job, a future Terraform provider) where *each caller*
should present *their own* key, the same way they'd present it to any
other REST API. Baking in one shared key would mean every restshim
caller is indistinguishable to `managerd`, defeating the point of
having per-key auth at all.

## Per-request `Authorization` header forwarding, not a static credential

`internal/restshim/server.go` gained `authContext(r *http.Request)
context.Context`: if the incoming HTTP request carries an
`Authorization` header, it's attached to the outgoing gRPC call as
`metadata.AppendToOutgoingContext(ctx, "authorization", ...)` — the
exact metadata key `internal/manager`'s `checkAuth` already reads (see
ADR-0023). Every handler that calls into `s.client` now uses
`authContext(r)` instead of `r.Context()`. A request with no header
forwards nothing, which behaves exactly like calling `managerd`
unauthenticated today — no change for a deployment with no keys yet.

This makes `restshim` a transparent pass-through for auth: whatever
key a REST caller presents is exactly the key `managerd` validates,
with `restshimd` itself never inspecting, storing, or needing to know
about key material at all. A future Terraform provider (or any other
REST client) just sets its own `Authorization: Bearer <key>` header the
same way it would against `managerd` directly.

## Consequences

- Verified with unit tests (`internal/restshim/server_test.go`:
  `TestServer_ForwardsAuthorizationHeaderToManagerd` confirms the
  header reaches `managerd` as gRPC metadata via a fake client capturing
  its context; `TestServer_NoAuthorizationHeaderForwardsNothing`
  confirms no header means no metadata attached) and a real local
  end-to-end run (`raftd` + `managerd` + `restshimd`, all real
  binaries): `GET /v1/status`, `POST /v1/vms`, `GET /v1/vms` worked
  unauthenticated before any key existed; after creating a real key
  directly against `managerd`, the same `GET /v1/vms` through
  `restshimd` correctly rejected a request with no header or the wrong
  key, and succeeded with the right one via a plain `curl -H
  "Authorization: Bearer <key>"` — full parity with `managerd`'s own
  auth behavior, proxied transparently.
- Terraform support is now unblocked at the infrastructure-wiring level
  named in ADR-0023's "tabled" note — a provider can be built against a
  real, running, authenticatable REST endpoint. The provider itself
  (translating Terraform's plan/apply lifecycle to `restshim`'s
  create/read/update/delete calls) is still separate, unstarted work.
- `restshimd` was not deployed to `apiarium` as part of this change —
  local end-to-end verification only. Deploying it live is a small,
  separate step (same `daemon(8)`-launched pattern already used for
  `raftd`/`managerd`/`frontend` there) whenever it's actually needed.
- Everything ADR-0023 already scoped out (`raftd`'s socket, real user/
  role separation, raft's own transport) remains out of scope here too
  — this ADR only adds a binary and a forwarding mechanism, not new
  authorization semantics.
