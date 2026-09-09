# ADR-0089: Cross-node jail base-template fetch

## Status

Accepted

## Context

ADR-0084 (jail base images via ZFS clone) explicitly disclosed a real
limitation: a jail's `base_template` is resolved purely by a
node-local naming convention (`templates/<name>@apiary-template`,
checked via `ZFS.SnapshotExists`) - if the named template doesn't
exist on whatever node a jail gets scheduled to, creation just fails,
even if another cluster member has it. VM base images
(`base_image_name`) hit the exact same problem earlier and closed it
in ADR-0041: when a node's own `isostore` lacks a named image, the
reconciler asks every other known node whether it has the file, then
has the first one that does push it over the network automatically.

Asked to choose between mirroring that automatic-fetch pattern in
full (now needing `zfs send`/`receive` instead of a flat-file copy,
since a template is a ZFS dataset) versus a lighter "just tell the
operator where it lives" diagnostic-only fix, the user chose the full
automatic fetch - closing the gap for real, at the cost of genuinely
new plumbing this codebase had never had before (piping a live `zfs
send` stream between two nodes over gRPC).

## Design decisions

- **`internal/zfs.Manager` gains three primitives**: `Send(ctx,
  "dataset@snapshot") (io.ReadCloser, error)` streams a running `zfs
  send`'s stdout live rather than buffering it (unlike every other
  method in this file, which fully buffers a short command's output
  via `runZFS` - wrong for a send stream that could be gigabytes);
  `Receive(ctx, destName, r io.Reader) error` runs `zfs receive
  <dataset>` with its stdin fed from `r`, blocking until `r` is fully
  drained; `ListTemplateNames(ctx) ([]string, error)` lists every name
  under `Base/templates` with an `@apiary-template` snapshot (ADR-0084's
  fixed convention), returning an empty list rather than an error if
  `Base/templates` doesn't exist yet on this node.
- **Three new RPCs on `managerd`, mirroring `ListISONames`/
  `PushISOTo`/`UploadISO` exactly**: `ListJailTemplateNames` (Viewer
  tier, matches `ListISOs` - a read-only local report), `PushJailTemplateTo`
  (Operator tier, matches the peer-forwarding-write convention -
  resolves the target node's address and streams a local `zfs send`
  to its `ReceiveJailTemplate`), and `ReceiveJailTemplate` (Operator
  tier, a client-streaming RPC - the first message must carry
  metadata naming the template, every message after that carries a
  chunk of the `zfs send` stream, piped directly into `zfs receive` as
  it arrives via `io.Pipe`, never buffered in memory).
- **`internal/manager`'s `quotaSetter` interface is extended in
  place**, not given a new field/setter: `Send`/`Receive`/
  `ListTemplateNames` are already methods on the same concrete
  `*zfs.Manager` instance already wired in as `s.zfs`, so no new
  wiring is needed in `cmd/managerd/main.go`.
- **`internal/manager`'s `PeerForwarder` interface gains one method**,
  `PushJailTemplate(ctx, addr, name string, r io.Reader) error` -
  mirrors `UploadISO`'s own streaming-client shape exactly (metadata
  message, then 256KB chunks, `CloseAndRecv`).
- **`internal/cluster`'s separate `peerReporter` interface gains two
  methods**, `ListJailTemplateNames`/`RequestJailTemplatePush` -
  mirrors `ListISONames`/`RequestISOPush` on the same interface
  exactly, keeping this package decoupled from `rpcpb` wire types the
  same way every other method on this interface already is.
- **The actual peer-fetch lives in `internal/cluster/jail.go` as
  `resolveJailTemplate`/`fetchTemplateFromPeer`**, mirroring
  `resolveLocalImagePath`/`fetchImageFromPeer`
  (`internal/cluster/reconciler.go`/`peer.go`) exactly, reusing the
  same already-existing `resolvePeerAddresses`/`peerManagerdPort`
  helpers those functions already use. `resolveJailTemplate` checks
  the snapshot locally first; if missing, it calls
  `fetchTemplateFromPeer`, which asks every other known cluster node's
  `ListJailTemplateNames` and, on the first one reporting the name,
  asks it to push the template here via `RequestJailTemplatePush` -
  same first-match-wins semantics as ISO fetch (no verification a
  peer's template is "the same" beyond name equality, matching that
  accepted trust model), same "every unreachable/erroring peer is just
  skipped" behavior. `ensureJail`'s previous direct
  `SnapshotExists`-then-error block is replaced by a single call to
  `resolveJailTemplate`.

## Consequences

- **New surface area, named directly**: `Send`/`Receive` are the
  first time this codebase pipes a live `zfs send` stream between two
  nodes over the network. Bounded to the `templates/` namespace only
  (never a VM/jail's own live root), but a genuinely new capability,
  not a reuse of something already proven elsewhere.
- ADR-0084's own disclosed limitation is narrowed, not eliminated: an
  operator must still create a template once, somewhere in the
  cluster - this closes only the "must exist on every node a templated
  jail might land on" part, not the manual-creation step itself.
- Full test coverage: `internal/zfs` gained `Send`/`Receive`/
  `ListTemplateNames`, confirmed via `go build`/`gofmt`; `internal/cluster`
  extended the existing `fakePeerReporter` with jail-template
  equivalents of its ISO fields and added coverage mirroring
  `fetchImageFromPeer`'s own tests (found-on-a-peer before cloning,
  not-found-on-any-peer fails without cloning, no-peers-configured
  fails clearly); `internal/manager` gained handler tests for all
  three new RPCs (mirroring `TestServer_UploadISO_*`) plus RBAC tier
  tests, and integration tests for `PushJailTemplateTo` mirroring
  `PushISOTo`'s own coverage (missing fields, no ZFS configured, a
  `Send` error, and a successful push to a resolved target); the two
  `fakeClient` test doubles in `internal/restshim`/`internal/frontend`
  gained stub implementations of the three new `ManagerServiceClient`
  methods to keep satisfying the interface.
