# Code audit, 2026-10-24

Revision audited: `main` at `aea1be1` (branch `audit/apiary-codebase-2026-10`).
Method: static analysis (govulncheck, gosec, `go vet`, `gofmt`, `go test`) plus
manual reading of every analyzer hit that could plausibly be real, and a manual
inventory of the ADR set. Nothing was run against a live Colony. No exploit
testing was done. This audit follows the structure of the 2026-09-26 audit and
does not repeat the findings that audit already covered and fixed.

## Summary

| ID  | Severity | Area | Finding |
|-----|----------|------|---------|
| B1  | Medium | frontend | Origin CA certificate lifetime was parsed with `strconv.Atoi` and narrowed to `int32`, so a large request value could truncate onto a lifetime the issuer does accept |
| B2  | Low | docs | `0150-dedicated-vlan90-switch.md` carried the header `ADR-0120`, colliding with a real ADR-0120 |
| B3  | Low | frontend | `GET /cluster-health.json` and `internal/frontend/health_tree.go` have no remaining consumer after PR #122 removed the cluster health panel - **corrected 2026-10-27: the endpoint is deliberately retained for the deferred Evidence tab, not dead code** |
| B4  | Low | docs | ADR numbers are not unique: 0150 is used by four files and 0152 by two - **fixed 2026-10-27: the three non-canonical 0150 files and the non-canonical 0152 file were renumbered** |
| B5  | Info | docs | ADR numbering has gaps at 0126 and 0153 - **partly outdated: 0153 now exists; only the gap at 0126 remains** |
| B6  | Info | tests | `go test ./...` fails in the four host-privilege packages (`internal/cluster`, `internal/hast`, `internal/jail`, `internal/zfs`) because the tests need root-owned paths |
| B7  | Info | hardening | A small set of hardening notes (see the end) |

Analyzer results: govulncheck found 0 vulnerabilities affecting reachable code
(1 in a required module that is not called). gosec reported 186 issues, one
fewer than before this branch because B1 is fixed; the remaining 185 are
reviewed below and are noise, guarded, or accepted. `go vet ./...` is clean.
`gofmt -l .` is clean.

## Fix status

B1 and B2 are fixed and merged to `main` (PR #158, merge `82ed88e`). B3 through
B7 are documented but not changed; see the table for the reason in each case.

**Update 2026-10-27 (branch `docs/audit-b3-b4-b5-closure`):** B3's premise was
outdated and is corrected below - the cluster health endpoint is deliberately
retained, not dead code. B4 is fixed by renumbering four ADRs. B5 is partly
outdated: 0153 now exists, and the remaining gap at 0126 is recorded as an
intentional reserve. B6 and B7 are unchanged.

| ID | Status | Commit | Notes |
|----|--------|--------|-------|
| B1 | Fixed | `c03ee4b` | The requested lifetime is parsed as a 64-bit integer and checked against `internal/cloudflare.ValidOriginCAValidity` before the `int32` narrowing. An out-of-set value is rejected in the UI and never forwarded to managerd. |
| B2 | Fixed | `d7ad9f7` | Header corrected to `ADR-0150`. The filename and header now agree, and the collision with `0120-config-rationale-history.md` is gone. (The file was subsequently renumbered to `0178-dedicated-vlan90-switch.md`; see B4.) |
| B3 | Corrected, not changed | (none) | The finding's premise was outdated when it was written. The endpoint is deliberately retained for the deferred Evidence tab, so no code was removed. Issue #112 is closed, so it is no longer a tracker. The audit record now says so (see the B3 section). |
| B4 | Fixed | (this branch) | The owner asked for the outstanding work to be done. The three non-canonical 0150 files became ADR-0178, ADR-0179 and ADR-0180, and the non-canonical 0152 file became ADR-0181. Every reference was updated in the same commit; `docs/adr/MIGRATION.md` records the new allocation. |
| B5 | Corrected, not changed | (none) | 0153 now exists (`0153-configurable-base-image-disk-expansion.md`), so that half of the finding was outdated. The remaining gap at 0126 is recorded as an intentional reserve pending central allocation; no ADR was invented to fill it. |
| B6 | Documented, not changed | (none) | These are environmental failures, not code defects. |
| B7 | Documented, not changed | (none) | Hardening notes only. |

## Findings

### B1. Origin CA lifetime truncation at the UI boundary (Medium)

`internal/frontend/machine.go` read the `validity_days` form field with
`strconv.Atoi` and passed the result to the managerd RPC, whose field is
`int32`. The conversion from the wider `int` to `int32` was unchecked.

Two consequences, in order of importance:

1. A value that does not fit in `int32` truncates, and a truncated value can
   land on a lifetime Cloudflare's Origin CA does accept. The narrowing is
   modulo 2^32, so `4294967661` (2^32 + 365) truncates to `365`, and a
   request for a nonsense lifetime would have been silently served as a
   one-year certificate. The same holds for every accepted value: 2^32 plus
   7, 30, 90, 365, 730, 1095, or 5475 all truncate onto an accepted
   lifetime.
2. The set of accepted lifetimes is small and closed: 7, 30, 90, 365, 730,
   1095, and 5475 days (`internal/cloudflare.ValidOriginCAValidity`). The
   package comment already notes that the check belongs at the UI boundary too,
   but the UI did not do it.

Impact: an operator-facing form could produce a certificate with a different
lifetime than the one on screen, or a request that managerd rejects after the
fact with an opaque error. No authentication or authorization boundary is
crossed, so the severity is Medium rather than higher.

Fix: parse with `strconv.ParseInt(..., 10, 64)`, validate against
`cloudflare.ValidOriginCAValidity` before the narrowing, and reject with a
message that names the accepted set. The check now runs before any RPC is
forwarded, so an invalid value never leaves the frontend.

Tests: `TestServer_IssueOriginCertificate_RejectsValidityOutsideAcceptedSet`
covers `0`, `-1`, `1`, `31`, `3651`, and the seven values that truncate onto
an accepted lifetime (`4294967303`, `4294967326`, `4294967386`,
`4294967661`, `4294968026`, `4294968391`, `4294972771`), plus `4294967331`
and `2147483800` as out-of-range controls, and asserts that no RPC is
forwarded.
`TestServer_IssueOriginCertificate_ForwardsAcceptedValidity` covers the seven
accepted values and asserts the forwarded `ValidityDays`.

### B2. ADR-0150 carried the ADR-0120 header (Low)

`docs/adr/0150-dedicated-vlan90-switch.md` began with
`# ADR-0120: Dedicated VLAN 90 switch for cluster nodes`. ADR-0120 is a
different, real document (`0120-config-rationale-history.md`), so the file was
claiming an identifier that belongs to someone else.

(This file was itself renumbered to `0178-dedicated-vlan90-switch.md` on
2026-10-27, for the reasons in B4. The B2 fix itself - correcting the
header to `ADR-0150` in commit `d7ad9f7` - is unaffected by the later
renumbering, and the header now reads `ADR-0178`.)

A sweep of all 158 files in `docs/adr` comparing each filename number against
the first `# ADR-NNNN` header found this as the only mismatch. The fix is the
one-line header correction; `0116-quorum-safe-raftd-restart.md` also lacks an
`ADR-NNNN` header, but it uses the `NNNN.` heading form consistently with the
rest of its content and does not collide with another number, so it was left
alone.

### B3. The cluster health endpoint has no consumer (Low)

**CORRECTED 2026-10-27. The finding as originally written was wrong about
what this code is. The endpoint is deliberately retained, and no code was
removed.**

`internal/frontend/server.go` registers `GET /cluster-health.json`, backed by
`internal/frontend/health_tree.go` (about 9.1 KB) and its test file. The
cluster health panel that used it was removed from
`web/templates/cluster_overview.html` in commit `f1e20ac` (PR #122, merged
2026-10-16).

Verified that nothing in `web/` reads it: no template, no JavaScript, and no
CSS mentions `cluster-health`. The only other hit in the tree is a prose
comment in `internal/restshim/server.go:102` that mentions cluster health in
the context of `/v1/health`. `cmd/apiary-mcp` uses the managerd gRPC
`ClusterHealth` RPC, not this HTTP route.

What the original finding missed is that **the removal of the panel and the
retention of the endpoint were separate decisions, taken separately.** The
"Blend" UI redesign, PR #196 (merge `4b306a4`, merged 2026-10-26), replaced
the overview and dropped the health tree from the rendered page, and states
in its own description:

> The old Command Center's panels the handoff did not carry over (the health
> tree, the topology cards) are gone from the overview. The per-guest health
> tree's *data model* and its `/cluster-health.json` endpoint remain, for the
> deferred Evidence tab.

The tree in the tree agrees, explicitly:

- `internal/frontend/health_tree_render_test.go` opens with a comment
  stating that "the tree's data model, its builder, and its JSON endpoint
  (`/cluster-health.json`) all remain - they are the per-cell health
  evidence the handoff's deferred 'Evidence' tab will need", and that
  `internal/health`'s own five-state vocabulary is still computed per cell.
- That test asserts the endpoint still answers `200` with the per-guest
  tree (`"kind":"cluster"`), so the retention is enforced by a test rather
  than left to drift.
- `internal/frontend/health_tree.go` is still wired to the route at
  `internal/frontend/server.go:1296`, and `buildHealthTree` is still
  covered by `internal/frontend/health_tree_test.go`.

**Why the original finding was written the way it was.** The audit was
performed against `main` at `aea1be1` (2026-10-24), which predates PR #196
by two days. At that revision the endpoint genuinely had no consumer, and
the audit was right to report it. The premise became outdated when PR #196
merged on 2026-10-26 and made the retention explicit.

**Issue #112 is not a tracker for this.** It is closed (closed 2026-10-25),
and it was never a decision ticket. Issue #112 is titled "Feature: Command
Center hierarchical cluster health view" and asked to *implement* the tree,
citing the Notion design page "Apiary Command Center - Hierarchical Cluster
Health Design" and listing eight acceptance criteria (multi-tier
brood/drone/sting hierarchy, relative names, root expanded and hosts
collapsed by default, 10-second polling, a 30-second stale threshold showing
total observation age, explicit UNKNOWN versus DOWN, and expansion state
preserved across refreshes). It was implemented by PR #167 (merge `0aa105c`,
merged 2026-10-24) and closed. The tree builder was the implementation of
that design, and it is retained, so the endpoint is not dead code awaiting
deletion.

**What the endpoint is retained FOR.** It is the per-cell (per-guest) health
evidence surface for the Evidence tab that the "Apiary: UI Redesign"
handoff defers. The tree carries what the Combs section deliberately does
not: a per-resource observation timestamp rather than the host's
reconciliation timestamp, so a stale guest can be told apart from a stale
host; the 30-second stale threshold; the explicit UNKNOWN-versus-DOWN
distinction; and bottom-up aggregation with progressive fault isolation.
The Combs section replaces only the *rendering* of that tree; the evidence
underneath it is still computed per cell.

**What would close it.** The Evidence tab, when it is built, either consumes
`GET /cluster-health.json` as its data source, or supersedes it with its own
read. Until one of those happens, removing the route and the tree builder
would remove evidence the next piece of work is going to need, so the
correct action is to keep it.

**One loose end worth recording: the Evidence tab has no tracker.** There
is no open GitHub issue for it in this repository (every issue in
`glenjbarber/apiary` is currently closed), and ADR-0176 - the record of
the UI redesign's remaining work - does not list it. The Evidence tab is
named only in PR #196's description and in the `health_tree_render_test.go`
comment. That is a gap in the *tracking* of the deferred work, not in the
code, and it is recorded here rather than fixed by inventing an issue
number or an ADR. If the Evidence tab is never built and the
owner decides the endpoint is not public surface, that decision is still
the owner's to make - and it is now a *different* question from the one this
finding asked, because the tree is no longer the only implementation of the
hierarchical health view: `internal/frontend`'s `clusterNodeEvidence` and
`internal/health`'s `AggregateStatus` are the shared evidence path that
ADR-0160's monitoring implementation also uses.

Impact: none. The route sits behind the same authentication as the rest of
the frontend, it is intentional, and it is covered by a test that asserts it
keeps answering.

### B4. ADR numbers are not unique (Low)

**FIXED 2026-10-27.** The owner asked for the outstanding work to be done,
so the collisions were closed by renumbering. The number was kept with the
record that `main` history treats as canonical, and the other files were
moved to numbers that were free in the tree.

Four files claimed ADR-0150:

- `0150-ai-agent-cell-api.md`
- `0150-dedicated-vlan90-switch.md`
- `0150-native-system-statistics-and-cluster-monitoring.md`
- `0150-preferred-leader-bias.md`

Two files claimed ADR-0152:

- `0152-atomic-vm-identity-rename.md`
- `0152-hastd-fixes-in-freebsd-15-1-release.md`

Every ADR in `docs/adr` has a unique filename, so no file could be renamed
onto another, but references that cite a number rather than a filename were
ambiguous - and there were many of them, in code comments, in other ADRs, in
`README.md`, in `docs/bootstrap.md`, and in the RPC schema and its
generated code.

#### The mapping

| Former number | Record | Now | Why it kept (or did not keep) the number |
| --- | --- | --- | --- |
| 0150 | `0150-ai-agent-cell-api.md` | **ADR-0150** | Keeps 0150. The AI-agent cell API is referenced by the code throughout (`internal/cellrecommend`, the `CreateCell`/`RecommendCellType` RPCs in `api/rpc/manager.proto` and its generated `.pb.go`/`.grpc.pb.go`, the REST shim in `internal/restshim`, `internal/manager`), and is amended by ADR-0151, whose own text names it. |
| 0150 | `0150-dedicated-vlan90-switch.md` | **ADR-0178** | No code and no cross-ADR reference names it. It is a record of a physical build decision (a second Netgear GSS108E dedicated to VLAN 90). |
| 0150 | `0150-native-system-statistics-and-cluster-monitoring.md` | **ADR-0179** | The native system-statistics proposal, superseded by ADR-0160 for the monitoring API design. ADR-0160's own `Supersedes:` line pointed at it, so that line was updated too. |
| 0150 | `0150-preferred-leader-bias.md` | **ADR-0180** | The preferred-leader bias, implemented as `internal/raft`'s `PreferredLeaderTransfer` and referenced by name in `internal/raft/config.go`, `internal/raft/node.go`, `internal/backup/secrets.go`, `internal/raftdconfig/manager.go`, `cmd/raftd/main.go` and ADR-0156. |
| 0152 | `0152-atomic-vm-identity-rename.md` | **ADR-0152** | Keeps 0152. Implemented by PR #173 (merge `93d3420`), tracked by issue #137, and referenced by name throughout `internal/raft`, `internal/cluster`, `internal/manager`, `internal/capacity`, `internal/frontend`, `internal/restshim`, and the RPC and state schemas. |
| 0152 | `0152-hastd-fixes-in-freebsd-15-1-release.md` | **ADR-0181** | The upstream hastd/hastctl fixes in FreeBSD 15.1-RELEASE. Referenced by the install preflight code and by ADR-0008, ADR-0022, ADR-0026 and ADR-0082, which it supersedes in part. |

0178, 0179, 0180 and 0181 were free across every branch in the repository
at the time. 0175 is held by the certificate-renewal-lifecycle work and
0176 and 0177 are already in use, so the new numbers start above them.

#### What was updated

Every reference was updated in the same commit as the moves:

- The four moved files' headers, filenames, `Legacy identifier` lines, and
  a "Merge note" recording the renumbering (the same form ADR-0098 and
  ADR-0055 use for their own renumberings).
- `docs/adr/MIGRATION.md`, the canonical index: the numbering migration map
  now covers every record up to ADR-0181, and a new "Local renumbering,
  2026-10-27" section records the mapping, the reasoning, and the fact that
  this is an apiary-local allocation rather than a central one.
- `docs/adr/0156-colony-consensus-protocol.md`, whose "Side note on ADR
  housekeeping" recorded the collisions and whose "Decisions for Glen"
  section listed "Fixing ADR numbering" as deliberately not done. Both now
  record the renumbering and say what remains open (the gap at 0126).
- `docs/adr/0160-native-monitoring-api.md`, whose `Supersedes:` line and
  body referenced the old number of the record it supersedes.
- `docs/adr/0154-frontend-restshimd-passthrough.md`, whose implementation
  notes cited "ADR-0150 and ADR-0153 ... for their route tables". Both
  numbers there were wrong: the "ADR-0150" was the native-monitoring
  record (now ADR-0179), and the "ADR-0153" was the *monitoring* ADR
  before that record took its present number (now ADR-0160), not the
  disk-expansion ADR that later took 0153. Both now point at the right
  records.
- `docs/adr/0008-hast-config-and-lifecycle.md`,
  `docs/adr/0022-network-management.md`,
  `docs/adr/0026-hast-vm-disk-replication.md` and
  `docs/adr/0082-apiary-installer-preflight.md`, which carry
  "Superseded in part by" pointers to the hastd record.
- `internal/install/checks.go` and `internal/install/checks_test.go`, whose
  fix hints name the ADR and its filename.
- `cmd/apiaryinstall/main.go` and `docs/bootstrap.md`.
- `README.md`.
- The `ADR-0180` references in `internal/raft/config.go`,
  `internal/raft/node.go`, `internal/backup/secrets.go`,
  `internal/raftdconfig/manager.go` and `cmd/raftd/main.go`.

#### What could not be updated

Commit messages that named the old numbers are not editable and were left
alone; the renumbering is recorded in the moved records' merge notes
instead, which is the same approach ADR-0098 took. The Notion page
"Todo: apiary repo hygiene - stray worktree and ADR number mismatch"
records the same four-0150 / two-0152 state and names the old filenames;
it was not edited from this branch because it is a Notion record rather
than a repository file, and it now describes a state that has been
superseded.

### B5. ADR numbering gaps (Info)

**PARTLY OUTDATED as of 2026-10-27. One of the two gaps is closed; the other
is confirmed to be an intentional reserve and is left open.**

The audit recorded gaps at 0126 and 0153, with the numbered files running
from 0001 to 0155 at that revision.

- **0153 is no longer a gap.** `0153-configurable-base-image-disk-expansion.md`
  now exists in `main` (added in commit `0938d39`, 2026-10-24, on the
  disk-expansion and VM-identity branch). The gap at 0153 in this finding
  was a snapshot of a branch that had not merged yet, and it is closed.
- **0126 is still absent.** Verified: no file in `docs/adr/` begins with
  `0126`, and the only occurrences of the string `0126` anywhere in the
  repository are in `web/static/novnc/core/input/keysymdef.js` (a
  hexadecimal Unicode codepoint, unrelated), and in this audit record and
  `docs/adr/MIGRATION.md`, which both name it as a gap.

**0126 is left open, deliberately.** Filling it would mean writing an ADR
for a decision that has not been made, and this repository does not do that:
an ADR number with no decision behind it would be a worse record than a
documented gap. The gap is consistent with a reserved-but-unwritten number
rather than a lost file, since `MIGRATION.md` still lists global allocation
as outstanding.

**This is a low-severity bookkeeping item and nothing more.** A gap in the
apiary-local numbering has no runtime effect, no security effect, and no
effect on any reference in the tree, because a number that no record claims
is referenced by nothing. It is recorded here so that the next reader does
not mistake it for a deleted or misplaced file.

**For completeness**, the full set of gaps in the apiary-local numbering as
of 2026-10-27 is 0126, 0159, 0165 and 0175. 0159 and 0165 are gaps created
by the renumbering that PR #164's own branch performed (0153/0157/0158
became 0160/0161/0162), and 0175 is held by the certificate-renewal-lifecycle
work and is expected to be filled when that branch merges. Only 0126 is a
genuinely unclaimed reserve.

**What would close it.** A real decision that needs recording, allocated
0126, or the owner's confirmation that 0126 is not needed and the sequence
can close over it. Until one of those happens, the gap stands and is
recorded, and that is the honest state of it.

### B6. Host-privilege test packages fail outside root (Info)

`go test -count=1 ./...` fails in four packages, all for the same reason: the
tests need paths that are owned by root or by the host configuration.

| Package | Reason |
|---------|--------|
| `internal/cluster` | Cannot read or write `/var/db/apiary/cloudflared/cloudflared.pid` and the ZFS datasets it manages. |
| `internal/hast` | Cannot read `/etc/hast.conf`. |
| `internal/jail` | Permission denied on the jail device paths. |
| `internal/zfs` | Permission denied creating and destroying ZFS datasets. |

`internal/manager` passes in this environment. The 2026-09-26 audit noted a
`TestIntegration_CreateJail_ForwardingFailureSurfacedInError` flake in
`internal/manager`; it did not recur on this run.

This is environmental, not a code defect, and the CI workflow already runs
`go test -race ./...` in a container where these paths are available. It is
recorded so that a future reader does not mistake these failures for a
regression.

### B7. Hardening notes (Info)

- `internal/frontend/server.go` has four `template.HTML` conversions that
  gosec flags as G203: `pageHeader` extra (`server.go:790`), `hint`
  (`server.go:812`), `iso_replication.go:148`, and `vm_snapshots.go:208`.
  Every caller passes a template literal written in this repository or the
  output of `json.Marshal`, and `hint` escapes its argument before building
  the fragment. No user-supplied value reaches an unescaped HTML context.
- `internal/manager/services.go:139` runs `service <name> restart` with a name
  taken from the `apiaryServices` table, and the table marks `managerd` as not
  restartable. The risk is bounded by that allowlist.
- `internal/deadman/manager.go:264` passes only numeric arguments to a root
  command.
- gosec's G101 hits are credential field names and generated protobuf, and its
  G103 hits are all inside generated `api/**/*.pb.go` files.
- Apiary contains no Docker integration, no Prometheus or metrics exporter, no
  GitHub API client, and no storage of external provider keys.

## Reviewed and found sound

- The frontend origin CA path: the hostname list is trimmed and empty entries
  are dropped, the managerd RPC is the only caller of the issuer, and the
  accepted lifetime set in `internal/cloudflare` is a single shared source of
  truth now used by both sides.
- The `originca` issuance path still refuses to sign outside the accepted set
  (`internal/cloudflare/origin_ca.go:78`), so B1 was a UI-side gap in a
  defense that already existed on the issuing side.
- `go vet ./...` reports nothing across the tree.
- `gofmt -l .` reports nothing across the tree.
- CI (`.github/workflows/go-ci.yml`) runs the gofmt check, `go vet ./...`,
  `go test -race ./...`, and `go build ./...` on ubuntu-latest with
  `libpam0g-dev`, which covers the classes of defect found here.

## Not covered

- No live testing: no listener, firewall, PAM, deployed configuration, or
  exploit was exercised.
- The 158 ADRs were inventoried, not read in full. Their technical content
  was not re-reviewed.
- The 2026-10-27 closure of B3, B4 and B5 re-read the ADRs it renumbered
  and the ADRs that reference them, but did not re-review the technical
  content of the other 168 records.
- `internal/backup`, `internal/jailnet`, and `internal/migration` still have
  no non-test importers; they were not reviewed in depth.
- The prior audit's findings from 2026-09-26 were not re-verified against
  `aea1be1`; they were treated as closed.
- Notion and the rest of the Apiary documentation set were not audited for
  consistency against the code beyond the ADR inventory.
