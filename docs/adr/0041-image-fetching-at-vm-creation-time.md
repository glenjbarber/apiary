# ADR-0041: automatic image fetching at VM/jail creation time

## Status

Accepted - supersedes ADR-0040

## Context

ADR-0040 built a manual, browser-triggered "Copy to `<node>`"/"Copy to
all missing" feature on the Images page so an already-uploaded ISO/base
image could be copied node-to-node instead of re-uploaded. In live use
the feature didn't work reliably (clicking the button produced no
visible result), and separately the user reconsidered the UX itself:
rather than a separate manual pre-copy step before creating a VM, the
image should just be selectable from anywhere in the cluster at
VM-creation time, with the actual fetch happening automatically as part
of provisioning - closer to how VirtualBox's Virtual Media Manager
registers media once and any VM can reference it, with the tool making
it available rather than the user pre-staging it.

The user's own framing: create a VM, and the image dropdown should show
every known image with a cue for the ones that aren't already on the
selected node ("some type of flag/cue... needs to be fetched from
peer") - no separate copy action, no manual button.

## Design

- **`internal/cluster`'s `Reconciler` fetches a missing image
  automatically.** `ensureVM`'s ISO/base-image resolution previously
  just failed with `"ISO %q not found"`/`"base image %q not found"`
  when `r.ISOs.Path(name)` came back empty. A new
  `resolveLocalImagePath` helper wraps that lookup: on a miss, it calls
  `fetchImageFromPeer`, which asks every other known cluster node (via
  `resolvePeerAddresses`, the same raft-membership-derived address list
  `hast.go`'s own HAST role resolution already reuses) whether it has
  the file, and on the first match asks that node to push it here -
  reusing the exact same `PushISOTo`/`UploadISO` peer-to-peer transfer
  ADR-0040 built, just invoked directly by the reconciler instead of by
  a browser click forwarding through an intermediate RPC. Only after a
  successful fetch (or when the file was already local) does
  provisioning proceed.
- **The `ManagerService.ReplicateISO` RPC is removed** (its request/
  response messages too) - it was the browser-facing "pull" trigger for
  ADR-0040's manual copy, and has no caller left once the create-VM/
  create-jail forms stopped needing a pre-copy step. `PushISOTo` (the
  peer-to-peer "push" half) stays exactly as it was; it's still exactly
  what the reconciler's automatic fetch needs on the source side. This
  is a real API removal, not a deprecation - nothing else in this
  project or its sibling repos (`terraform-provider-apiary`,
  `cluster-api-provider-apiary`) ever called it.
- **`internal/manager.PeerReporter` gains `ListISONames`**, a thin
  wrapper around the existing `ListISOs` RPC that returns just
  `[]string` - the reconciler's own `peerReporter` interface
  (`internal/cluster/peer.go`) deliberately stays decoupled from
  `rpcpb` wire types (the same reasoning `ReportVMPhase`'s plain-string
  phase argument already established), so it needs a distinctly-named
  method rather than reusing `PeerReporter.ListISOs`'s own
  `*rpcpb.ListISOsResponse`-typed signature (which the frontend's
  `peerHostStatsClient` interface still uses for its own cluster-wide
  Images-page merge, unaffected by this change).
- **The create-VM form now shows every known node's images, not just
  the local node's**: `handleNewVMPage` fetches `currentClusterISOs`
  (unchanged from ADR-0040, since the cluster-wide merge itself was
  never the broken part) instead of the local-only `currentISOs`. A new
  `base_image_name` picker was added alongside the existing `iso_name`
  one - a real, previously-existing gap (ADR-0031 added
  `VMDefinition.base_image_name` months ago, but the web UI never
  exposed it at all).
- **A vanilla-JS cue, no server round-trip**: `isoMissingByNode`
  (`internal/frontend/iso_replication.go`) renders each image's
  `MissingNodes` list as a small embedded JSON object
  (`{"name": ["node ids missing it"]}`); `new_vm.html`'s own script
  (matching the form's existing firewall-rule-row vanilla-JS pattern)
  relabels each picker's options with "— will be fetched from a peer"
  whenever the currently-selected Node ID is in that image's missing
  list, live as the Node ID selection changes. Purely informational -
  it never blocks submission, since the reconciler now handles the
  fetch regardless.
- **ADR-0040's Images-page manual copy UI is removed outright**:
  `handleReplicateISO`/`handleReplicateISOAll`/`renderClusterISOResult`,
  the `/isos/{name}/replicate/{target_node_id}` and
  `/isos/{name}/replicate-all` routes, and `cluster_iso_rows.html` are
  all deleted rather than fixed - once the real use case (get an image
  onto the node that needs it) is handled automatically at the point it
  actually matters, a separate manual pre-copy action has no remaining
  purpose and would just be a second, redundant path to maintain.
  `currentClusterISOs`/`nodeListISOs`/`isoRowView` are kept (renamed in
  comments, not in code) since the create-VM form's cluster-wide picker
  reuses them as-is.

## Why not fix ADR-0040's button instead

The root cause turned out to be environmental, not a code bug: this
project's `apiverse` node's `managerd` had never been given
`-peer-tls`/`-peer-api-key`/`-peer-tls-hostname-map` at all (see
CLAUDE.md's ADR-0040 bullet, "Deploying this surfaced a real,
pre-existing gap") - the RPC path itself worked once that was fixed,
confirmed via a direct RPC client. But by the time that fix landed, the
user had independently reconsidered the UX and preferred automatic
fetching over a manual copy step regardless of whether the button
itself worked - so the fix was superseded rather than shipped.

## Consequences

- One node knowing about an image is now sufficient to create a VM
  referencing it anywhere in the cluster - no separate "make sure it's
  copied first" step, matching the VirtualBox-style "register once, use
  anywhere" mental model the user described.
- The reconciler tick that provisions a VM with a not-yet-local image
  now blocks on a full file transfer before it can proceed - acceptable
  for this project's image sizes and cluster scale (the same posture
  ADR-0040's synchronous `ReplicateISO` already accepted), but a genuine
  latency cost on that tick worth naming: a multi-gigabyte base image
  fetch happens inline, not in the background.
- Jails have no ISO/base-image field in v1 scope (CLAUDE.md's jail
  orchestration bullet), so this fetch-on-demand behavior only applies
  to VMs for now - nothing to change on the jail side.
- No byte-level fetch progress is surfaced anywhere (same non-goal
  ADR-0040 already named) - an operator watching the VM's phase go from
  `pending`/`creating` to `ready` sees it take longer than usual, with
  no visibility into "why" beyond checking the node's own managerd logs.
