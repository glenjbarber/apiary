# ADR-0018: Host stats RPC, and splitting the web UI into separate pages

## Status

Accepted

## Context

Two related requests: split the web UI's single scrolling page into
dedicated pages as more sections accumulate (Images, then VMs, then a
create-VM form), and add a host stats page - CPU, memory, ZFS pool
health, per-disk SMART status, and network throughput - as the
default landing page, since a node's own health is what an operator
most wants to see first.

## Decisions

### `internal/hoststats` gathers real host state, shelling out per subsystem

Like `internal/isostore`, this is physical, per-node data - never
routed through raft. Each subsystem (CPU/load, memory, ZFS pools,
per-disk SMART, network interfaces) is gathered independently via
FreeBSD's own tools (`sysctl`, `zpool`, `smart(8)`, `netstat`), with
shelling-out and parsing kept strictly separate: every `parse*`
function takes plain command output as a string and returns a typed
result, so the parsing logic is fully unit-testable with fixture data
captured from a real host, without needing FreeBSD to run the tests -
the same reasoning that keeps `internal/isostore` portable.

`smart(8)` is FreeBSD's own base-system tool (part of the pkgbase set
`apiarium` already has installed), not a third-party dependency - this
closes out the "Someday" item asking for exactly this.

### Best-effort gathering: one subsystem's failure doesn't blank the rest

`Snapshot.Errors` collects any subsystem that failed (e.g. `smart(8)`
missing, or one disk not supporting it) without failing the whole
`Gather()` call. A single unsupported/unhealthy disk shouldn't hide
CPU, memory, and every other disk's data.

### `DiskInfo.Healthy` is only meaningful when `Error` is empty

A SMART query failure isn't "known bad" - it's "unknown." The web UI
renders these three states distinctly (`healthy` / `FAILING` /
`unknown`, the last with the actual error as a tooltip) rather than
collapsing a query failure into a false health claim either way.

### Network figures are cumulative counters, not a computed rate

Computing an actual rate needs two samples over a known interval.
This is a deliberate v1 simplification - the counters are still
useful (confirming an interface is actually passing traffic, spotting
a stuck interface at 0), just not a live "Mbps" figure. Called out
explicitly in the UI itself, not left as a silent gap.

### `HostStats` is a new `ManagerService` RPC, following the ISO precedent exactly

`UploadISO`/`ListISOs`/`DeleteISO` (ADR-0017) already established the
pattern for physical, per-node data: a new RPC answered directly from
managerd's own local knowledge, never touching raft. `HostStats`
follows the identical shape - the frontend is just another RPC client,
consistent with every other page.

### The web UI splits into four pages, ahead of adding a console

`html/template`'s `{{template "name" pipeline}}` requires `name` to be
a literal string, not a dynamic value - there's no way for one shared
"layout" template to wrap a caller-chosen content block picked at
request time. Restructured around two literal-named shared partials
(`head`, `nav`) that each full page template includes directly, rather
than one generic layout dispatching to swappable content. Four
self-contained page templates now exist: `stats.html` (`/`, the new
default), `vms.html` (moved from `/` to `/vms`), `images.html`
(`/images`), and `new_vm.html` (`/vms/new`).

Moving the VMs page off `/` meant its own polling fragment endpoint
(previously `GET /vms`, the same path as the new full page) had to
move too - it's now `GET /vms/rows`, freeing `GET /vms` for the page
itself.

### The create-VM form's redirect target updated to `/vms`, not `/`

`HX-Redirect` (added when the create form moved to its own page,
alongside ADR-0017-era work) originally pointed at `/`, back when that
was the VMs page. With Stats now the default landing page, redirecting
there after creating a VM would hide the very thing the redirect was
meant to show, so it now points at `/vms` explicitly.

## Consequences

- Verified for real on `apiarium`: the Stats page shows real load
  averages, real memory pressure (95%+ used, running the full stack
  plus two bhyve VMs and the Time Machine jail concurrently), both
  real ZFS pools with correct capacity, all five real disks' SMART
  health via `smart(8)`, and real cumulative network counters for
  `re0`/`lo0`.
- Nothing computes a network rate yet - a real feature gap, not
  forgotten, just explicitly out of scope for this slice (see the
  in-page note).
- Multi-node stats aggregation doesn't exist - `HostStats` only
  reports the answering managerd's own node. A cluster-wide stats view
  (all nodes at once) would need its own design, not attempted here.
- This is unrelated to, and doesn't block, the still-planned noVNC
  console work.
