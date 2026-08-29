# ADR-0014: the web UI (cmd/frontend, internal/frontend, web/)

## Status

Accepted

## Context

This is the last piece of CLAUDE.md's originally described architecture
to get a first slice: "a Go backend serving a JSON API, with HTMX
handling interactive elements server-side (no separate JS SPA build)."
`internal/restshim` (ADR-0011) already covers the JSON API half for
programmatic clients; this slice builds the actual browser-facing HTMX
UI, answering the "can I point a browser at this yet?" question with
yes for the first time.

## Decisions

### `cmd/frontend` is a `ManagerService` client, like `restshim` — not a `restshim` client

`internal/frontend.Server` holds an `rpcpb.ManagerServiceClient` and
dials managerd directly, the same way `internal/restshim` does. It does
not go through `restshim`'s REST/JSON endpoints. This keeps both
`restshim` and `frontend` as sibling, independent translations of the
same underlying `ManagerService` (one to JSON, one to HTML) rather than
layering one on top of the other — HTMX wants HTML fragments back, not
JSON, so routing frontend requests through the REST layer would just
mean parsing JSON back into Go structs to re-render as HTML, with no
benefit.

### Server-rendered HTML fragments, not a JSON API consumed by client-side JS

Every mutating route (`POST /vms`, `DELETE /vms/{id}`) re-renders and
returns the `vm_rows` fragment — the same template used for the initial
full-page table body — rather than returning JSON for client-side
JavaScript to turn into DOM updates. This is what "no separate JS SPA
build" means in practice: HTMX's `hx-target`/`hx-swap` attributes handle
wiring the returned HTML into the page; there is no separate rendering
logic on the client to keep in sync with the server's.

### One shared `vm_rows` fragment for the initial render and every update

`pageData{Error, VMs}` is passed to `vm_rows` from three call sites
(index page load, post-create, post-delete) — there's exactly one place
that knows how to render a table of VMs, used identically whether it's
the first paint or an HTMX swap. This also means an error from a
mutating action still shows the real, freshly-fetched VM list next to
the error message (see next decision), not a stale or empty table.

### Errors render inline in the response, not as HTTP failures

A duplicate-id rejection, a "not the leader" response, or a transport
failure reaching managerd all render as an error message inside the
returned HTML (banner row in the fragment) with a normal `200` status,
rather than a `4xx`/`5xx` HTTP response. This is a deliberate difference
from `internal/restshim`'s approach (ADR-0011), which does map errors to
HTTP status codes — that mapping matters for programmatic REST clients,
but a human looking at a web page just needs to *see* what went wrong
inline, and HTMX's default swap behavior doesn't require special
handling for non-2xx responses this way.

### Templates and static assets are embedded (`go:embed`), not deployed as separate files

`web/assets.go`'s `embed.FS` means the built `frontend` binary is fully
self-contained — no separate step to ship `web/templates`/`web/static`
alongside the binary at deploy time, consistent with every other binary
in this project being a single static artifact.

### `htmx.min.js` is vendored locally, not loaded from a CDN

A management tool for infrastructure shouldn't have a hard runtime
dependency on an external CDN being reachable. `web/static/htmx.min.js`
is HTMX itself (BSD-2-Clause licensed, freely vendorable), fetched once
and committed, served from the embedded FS at `/static/htmx.min.js`.

## Consequences

- Verified with real `httptest` unit tests (mirroring `internal/restshim`'s
  fake-`ManagerServiceClient` pattern) and a real live browser session
  against the full `raftd` → `managerd` → `frontend` stack: create (HTMX
  swap, no reload), delete, and an inline duplicate-id error alongside
  the correct unchanged row were all verified visually, not just via
  `go test`.
- `internal/restshim` remains unwired into any `cmd/` binary — this
  slice didn't need it and didn't add one. A REST API consumer (e.g. a
  CLI tool, or an external integration) would still need its own
  `cmd/restshimd`-style binary, which doesn't exist yet.
- The `hx-confirm` delete-confirmation dialog relies on the browser's
  native `confirm()` — verified indirectly (a sandboxed test browser
  that suppresses `confirm()` was used during development, so the
  delete path itself was confirmed via a direct HTTP request instead;
  worth a real click-through in an unrestricted browser at some point).
- No authentication/authorization exists on any of `raftd`, `managerd`,
  `restshim`, or now `frontend` — anyone who can reach the relevant port
  can do anything. Fine for the current single-developer/local-network
  stage; a real deployment story will need this before it matters.
