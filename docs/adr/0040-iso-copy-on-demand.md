# ADR-0040: copy-instead-of-reupload for ISOs already present elsewhere in the cluster

## Status

Accepted

## Context

`internal/isostore` (ADR-0017) stores uploaded installer images/base
disk images per-node - deliberately not raft-replicated, since they're
physical bulk data. Before this change, the Images page only ever
showed and searched the *local* node's own files, so getting the same
image onto a second node meant re-uploading the whole thing through the
browser a second time - slow and wasteful for a multi-gigabyte file,
especially since node-to-node LAN transfer is almost always faster than
re-uploading from the original client machine.

Two designs were considered and rejected before landing on this one:

- **Automatic push-to-every-node-on-every-upload.** This is a different
  feature (eager full-cluster replication) than what was actually
  wanted - it would copy files nobody asked to have copied yet, onto
  nodes that may not need them at all.
- **HAST-backed.** HAST (ADR-0026/0027) only replicates a resource
  between exactly two nodes (an owner and one designated replica),
  which doesn't fit "copy from whichever other node happens to have
  it" - there's no pairwise ownership relationship between arbitrary
  ISO files and arbitrary nodes.

The feature actually wanted, refined directly from the user's own
framing: *"Here is a file I want to upload; there's a list of files
that have been uploaded somewhere; instead of re-uploading, copy it
from another node in the cluster."* - an on-demand, upload-time
convenience triggered from the Images page, not a background sync.

## Design

- **The Images page now shows a cluster-wide list**, not just the
  local node's files: for every node the frontend already knows about
  (the same roster `cluster_overview.go`'s `HostStats` peer-fetching
  already uses, ADR-0036), it fetches that node's `ListISOs` and merges
  the results by `(name, sha256)` into one row per distinct file,
  recording which node IDs actually have it (`PresentNodes`) and which
  don't (`MissingNodes`).
- **Each row shows a "Copy to `<node>`" button per missing node, plus a
  single "Copy to all missing" button** - confirmed directly with the
  user, who wanted both rather than just one or the other.
- **New RPC `ManagerService.ReplicateISO(name, source_node_id)`**,
  called on the *target* node's own managerd (the node that should end
  up with the file). The frontend dispatches this the same
  local-vs-peer way `cluster_overview.go`'s `nodeHostStats` already
  dispatches `HostStats`: directly via `s.client` when the target is
  the node the frontend is colocated with, or via
  `internal/manager.PeerReporter` (dialing the target's managerd
  directly) otherwise.
- **`ReplicateISO` doesn't pull bytes itself - it asks the source to
  push.** It resolves `source_node_id`'s managerd address via
  `s.raft.Status(ctx)` plus the existing `peerManagerdAddr` helper
  (already used for leader-hint resolution, works identically for any
  known node's raft address), then calls a new
  `PeerReporter.RequestISOPush(ctx, sourceAddr, name, targetNodeID)
  error`, which invokes a new peer-only RPC:
  **`ManagerService.PushISOTo(name, target_node_id)`** on the *source*
  node. `PushISOTo`'s handler opens the local file via
  `s.isos.Path(name)` (a small new method added to the local
  `isoManager` interface, already present on `*isostore.Manager`) and
  calls a new `PeerReporter.UploadISO(ctx, targetAddr, name, sha256,
  reader) error`, which drives a real `UploadISO` client stream against
  the target - the exact same metadata-then-chunks-then-`CloseAndRecv`
  shape `internal/frontend/server.go`'s own `uploadISOStream` already
  uses for a browser upload, just with the source's managerd acting as
  the streaming client instead of a browser.
- **Why two RPCs instead of one.** `ReplicateISO` (called by the node
  that needs the file, on itself) and `PushISOTo` (called peer-to-peer,
  by that node asking the source to push) are two different hops of the
  same request, not redundant - `ReplicateISO` is the only one ever
  exposed to a browser/API caller; `PushISOTo` only ever runs
  machine-to-machine, mirroring how `ReportVMPhase`/
  `ReportVMTeardownComplete` (ADR-0029) are already peer-only RPCs
  distinct from the external ones a caller actually invokes. This
  design reuses `UploadISO`'s existing client-streaming direction for
  both legs, avoiding a new download/server-streaming RPC shape
  entirely.
- **Synchronous, matching how uploads already behave.** `ReplicateISO`
  doesn't return to the browser until the copy actually completes or
  fails - the existing upload form is already a long-lived blocking
  call with a progress bar for a multi-gigabyte transfer, so a "copy
  from peer" action being similarly blocking (without byte-level
  progress, which would need new streaming-status plumbing) is
  consistent with what's already there, not a new UX pattern.
- **Role gate: `RoleOperator`**, matching `UploadISO`/`DeleteISO`'s own
  existing requirement - this is a write action with the same blast
  radius as uploading or deleting a file.

## Non-goals

- Byte-level copy progress reporting - out of scope for this pass; the
  browser simply waits for the synchronous `ReplicateISO` call to
  return, same as an upload without a progress bar would.
- Automatic/eager propagation to every node - explicitly rejected, see
  Context above. A file only moves when an operator clicks a button.
- Any interaction with HAST - ISOs remain outside HAST's pairwise
  owner/replica model entirely, as they always have been.

## Consequences

- `internal/manager/server.go`'s `isoManager` interface and
  `PeerForwarder` interface both grew narrowly (`Path`,
  `UploadISO`/`RequestISOPush`/`ReplicateISO`) rather than needing any
  new abstraction - the existing peer-forwarding machinery from
  ADR-0029/ADR-0035/ADR-0036 covers this cleanly.
- The Images page's cluster-wide fetch is O(known nodes) concurrent
  RPCs on every page load/poll, same cost class as the cluster overview
  page's own per-node `HostStats` fetch - acceptable at this project's
  cluster sizes, not something this design attempts to cache.
- A node that fails to answer `ListISOs` at all is simply treated as
  not having anything, the same fail-soft posture `currentVMs`/
  `currentISOs` already follow elsewhere - a transient fetch failure
  looks like "click here to copy it over" rather than blocking the page.
