# ADR-0020: a noVNC-based console for running VMs

## Status

Accepted

## Context

Apiary could create and boot bhyve VMs (including from an uploaded
installer image, ADR-0017), but there was no way to actually *see* one -
no console, no way to confirm an install was progressing or even that
firmware had come up at all. This was called out explicitly in CLAUDE.md
as the next planned piece after ISO upload. The user requested it as
item 4 of a batch of UI work, to be built after the session-based login
page (ADR-0019).

noVNC (MPL-2.0 core, BSD-2-Clause HTML/CSS, permissively licensed
throughout - confirmed before starting this work) is the standard
browser-based VNC client and was chosen for the same reason `htmx.min.js`
is vendored rather than CDN-loaded: a management tool for infrastructure
shouldn't have a runtime dependency on an external host being reachable.

## Decisions

### bhyve gets a real VNC framebuffer device, not a placeholder

`internal/bhyve.Config.EnableVNC` attaches a `fbuf` device (PCI slot 29)
plus a USB tablet (slot 30, for absolute mouse positioning - relative
mouse motion over VNC is nearly unusable) to every VM `internal/cluster`'s
Reconciler creates, whenever bhyve provisioning itself is enabled on
that node. This isn't a separate opt-in flag: there's no scenario where
a node that can run bhyve VMs at all shouldn't let their console be
viewed.

### The VNC port is allocated and persisted locally, not through raft

`Manager.allocateVNCPort` scans `RunDir` for `*.vnc` sidecar files (the
same pattern `tapfile` already uses for a VM's tap device name) and
picks the lowest free port in `[5900, 6000)`. This is physical, per-node
data - like a VM's dataset or disk image - not something that belongs in
raft's ephemeral state (see CLAUDE.md's physical/ephemeral distinction).
100 concurrent consoles is far more than this project runs today; a
real deployment needing more would need a wider range or a smarter
allocator.

### A new `GetVMConsole` RPC, following the HostStats/ISO pattern

`ManagerService.GetVMConsole` answers with the VNC host/port for a VM,
gathered from `internal/bhyve.Manager.VNCPort` the same way `HostStats`
gathers from `internal/hoststats` - local, per-node, never through raft.
It only ever answers for a VM confirmed to be running on *this* node
(checked via a `GetVM` call first); a VM on a different node gets an
explicit "query that node directly" error rather than silently failing.

### Host is always `"127.0.0.1"`, never the node's own hostname

The first implementation returned the node's own ID (its hostname) as
`host`, on the assumption that node IDs are resolvable hostnames (true,
apparently, everywhere else in this project). Live testing against
`apiarium` immediately disproved this: `apiarium`'s own `managerd`
couldn't resolve `apiarium` from itself (`dial tcp: lookup apiarium on
127.0.0.1:53: no such host` - no self-entry in `/etc/hosts`, no local
DNS). Since `GetVMConsole` only ever answers for a VM on its own node,
and `internal/frontend` is only ever expected to run alongside that same
`managerd` (matching how `-manager-addr` already defaults to
`127.0.0.1:17700`), loopback is both correct and removes a DNS
dependency the feature has no real reason to need. A genuine multi-node
deployment, where the frontend querying `GetVMConsole` isn't necessarily
colocated with the VM's owning node, would need real node-address
discovery - a gap already called out for scheduling, not something this
ADR solves.

### `internal/frontend` proxies WebSocket-to-TCP itself, no separate websockify

A browser can't open a raw TCP socket, and noVNC speaks the RFB protocol
over a WebSocket bytestream - something has to bridge the two. Rather
than running a separate `websockify` process (the traditional pairing
for noVNC), `internal/frontend/console.go`'s `handleConsoleWS` upgrades
the incoming HTTP connection (via `gorilla/websocket`, this project's
first non-generated third-party Go dependency beyond raft/grpc/boltdb -
BSD-2-Clause, chosen over the stdlib-adjacent `golang.org/x/net/websocket`
for its more complete/robust handling of arbitrary binary frames) and
pumps bytes bidirectionally with the VNC TCP connection. The proxy has
zero understanding of the RFB protocol itself; it forwards binary
WebSocket messages to TCP and vice versa, closing both ends together so
neither goroutine leaks once the other side disconnects.

### The console page is a plain full page load, not an HTMX fragment

Like the login page (ADR-0019), `/vms/{id}/console` is a real page, not
wired into the vm_rows polling/fragment pattern the rest of the VMs page
uses - a persistent WebSocket connection driving a `<canvas>` doesn't fit
that model, and there's no reason to force it to.

### noVNC is vendored as its ES-module core, not its full bundled app

`web/static/novnc/` holds noVNC's `core/` (the `RFB` class and its
dependencies) plus `vendor/pako/` (compression, which `core/inflator.js`/
`deflator.js` import directly) - not noVNC's own `app/` UI, which
duplicates functionality this project's own console page already
provides (connection status, page chrome) via `web/templates/console.html`
importing `RFB` as a native ES module directly. This mirrors
`htmx.min.js`'s vendoring precedent: fetched once, committed, served
from the embedded `web.FS`.

## Consequences

- Verified with unit tests (`internal/bhyve/manager_test.go` for VNC
  port allocation/lookup; `internal/manager/integration_test.go` for
  `GetVMConsole`'s node-matching/availability logic against a real raft
  harness; `internal/frontend/console_test.go` for the console page
  render and the WebSocket-to-TCP proxy itself, using a fake echo TCP
  server standing in for a VNC framebuffer).
- Verified live on `apiarium`: created a VM through the real UI, watched
  its `*.vnc` sidecar file appear after one reconcile tick, opened its
  console page, and watched a live, interactive bhyve framebuffer render
  in the browser via the real WebSocket proxy (confirmed via a raw `curl`
  Upgrade request first, then in an actual browser tab) - the first time
  a running Apiary VM's console has been visible at all.
- The live test also surfaced a second, unrelated bug this ADR's own
  testing made visible for the first time: a VM's console showed nothing
  ever booting from its attached ISO. See ADR-0021 for that fix - the
  console feature is what made it observable at all, which is itself a
  point in favor of building it.
- No credentials/encryption on the VNC connection itself (`fbuf`'s own
  RFB server has neither) - acceptable for now since the WebSocket proxy
  sits behind this project's existing session-based login (ADR-0019),
  and the underlying TCP connection never leaves loopback.
- Console access still only works when `internal/frontend` and the VM's
  owning `managerd` are the same node - see `GetVMConsoleResponse`'s doc
  comment (`api/rpc/manager.proto`) for why, and the same gap already
  accepted for node scheduling.
