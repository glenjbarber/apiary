# ADR-0110: Cluster-wide Images page

## Status

Accepted

## Context

The Images page (`/images`) only ever showed the locally-viewed Comb's
own stored ISOs/base archives (`currentISOs`, a thin wrapper around this
node's own `ListISOs`). This was inconsistent with the rest of the
UI's own resource pages: VMs and jails are already visible cluster-wide
(ADR-0106), and the create-VM/create-jail forms' own image pickers had
already solved this exact problem for themselves years earlier
(ADR-0041's `currentClusterISOs`/`isoRowView`, fanning out to every
known Comb's own `ListISOs` and merging by `(name, sha256)`), but the
Images page itself never adopted it - an operator managing images had
to check every Comb's own Machine/Images page individually to know
what was actually stored where.

Unlike VMs/jails, an ISO is a physical per-node file, never
raft-replicated state - there is no single canonical "owner" the way a
VM's `node_id` is. "Cluster-wide" for images means, and has always
meant since ADR-0041, "one row per distinct file, annotated with which
Combs have a copy" - not a single merged record.

## Decision

`handleImagesPage`, `handleListISOs` (the htmx list-refresh fragment),
and `renderISOPanelResult` (the post-upload/delete re-render) now all
use the existing `currentClusterISOs` instead of the local-only
`currentISOs` - no new fetch logic, reusing exactly what the image
pickers already proved out. `isoRowView` gained `Size`/`SHA256Short`
(previously only on the now-removed local-only `isoView`) and a new
`PresentLocally bool`, precomputed in Go rather than checked in the
template (`html/template` has no built-in slice-membership test) -
gates the Delete button, since `DeleteISO` has no remote-node concept
and only ever removes the *locally-viewed* Comb's own copy. A row
present only on other Combs shows no Delete button at all, just its
present/missing node lists, matching the existing `remote-node`
read-only-display convention used elsewhere in this UI rather than
implying an action this page can't actually perform.

The now-fully-unused local-only path (`currentISOs`, `isoView`,
`fromRPCISO`) was deleted outright rather than left as dead code,
following this project's own stated convention.

The upload form gained one line of copy (`Uploads land on this Comb
(<node>) only...`) so it's clear uploads are still node-scoped even
though the list below them is now cluster-wide.

## Consequences

- An operator can now see, from any single Comb's Images page, exactly
  which images exist and where across the whole Colony - matching how
  the create-VM/create-jail pickers already presented this information.
- Upload/Delete semantics are unchanged (still strictly local to the
  viewed Comb) - only the *visibility* became cluster-wide, mirroring
  the same "list is cluster-wide, actions stay node-scoped" pattern
  established for `remote-node`-styled VM/jail rows.
- No new backend RPC or wire-format change - this is entirely a
  frontend reuse of already-existing, already-tested aggregation code.

## Verification

- `internal/frontend`: `TestServer_ImagesPage_ClusterWide` (a genuine
  two-Comb fixture, asserting both Combs' own images are listed, that
  the present-locally row offers Delete and the present-only-remotely
  row does not, and that missing-node text renders); extended
  `TestCurrentClusterISOs_MergesPresentAndMissing` and added
  `TestCurrentClusterISOs_FormatsSizeAndShortensLongHash` (replacing
  the deleted `fromRPCISO`-specific test, now exercised through
  `currentClusterISOs` directly since that's the only remaining
  producer of these display fields); existing `TestServer_ImagesPage`/
  `TestServer_ListISOs_ShowsStoredImages` updated to supply a
  `statusResp` (previously implicit/absent, harmless only because they
  happened to route through the same-node fetch path that `s.peers ==
  nil` already forces regardless of node count).
- Manual visual check: rendered `/images` via the test harness with a
  realistic two-Comb, two-image fixture (one shared, one Comb-specific)
  and confirmed the "Present on"/"missing on" text and Delete-button
  gating render exactly as intended.
- `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), full `go test
  ./...` for the whole repository - all green.
