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
| B3  | Low | frontend | `GET /cluster-health.json` and `internal/frontend/health_tree.go` have no remaining consumer after PR #122 removed the cluster health panel |
| B4  | Low | docs | ADR numbers are not unique: 0150 is used by four files and 0152 by two |
| B5  | Info | docs | ADR numbering has gaps at 0126 and 0153 |
| B6  | Info | tests | `go test ./...` fails in the four host-privilege packages (`internal/cluster`, `internal/hast`, `internal/jail`, `internal/zfs`) because the tests need root-owned paths |
| B7  | Info | hardening | A small set of hardening notes (see the end) |

Analyzer results: govulncheck found 0 vulnerabilities affecting reachable code
(1 in a required module that is not called). gosec reported 186 issues, one
fewer than before this branch because B1 is fixed; the remaining 185 are
reviewed below and are noise, guarded, or accepted. `go vet ./...` is clean.
`gofmt -l .` is clean.

## Fix status

All fixes are on this branch. Nothing is merged or pushed.

| ID | Status | Commit | Notes |
|----|--------|--------|-------|
| B1 | Fixed | `c03ee4b` | The requested lifetime is parsed as a 64-bit integer and checked against `internal/cloudflare.ValidOriginCAValidity` before the `int32` narrowing. An out-of-set value is rejected in the UI and never forwarded to managerd. |
| B2 | Fixed | `d7ad9f7` | Header corrected to `ADR-0150`. The filename and header now agree, and the collision with `0120-config-rationale-history.md` is gone. |
| B3 | Documented, not changed | (none) | Removing the route and the tree builder is a judgment call about the public surface of the frontend, so it is reported rather than done. Issue #112 is still open. |
| B4 | Documented, not changed | (none) | `docs/adr/MIGRATION.md` says global number allocation is "Pending Glen allocation", so the numbers were not reassigned autonomously. |
| B5 | Documented, not changed | (none) | Same reason as B4. |
| B6 | Documented, not changed | (none) | These are environmental failures, not code defects. |
| B7 | Documented, not changed | (none) | Hardening notes only. |

## Findings

### B1. Origin CA lifetime truncation at the UI boundary (Medium)

`internal/frontend/machine.go` read the `validity_days` form field with
`strconv.Atoi` and passed the result to the managerd RPC, whose field is
`int32`. The conversion from the wider `int` to `int32` was unchecked.

Two consequences, in order of importance:

1. A value that does not fit in `int32` truncates, and a truncated value can
   land on a lifetime Cloudflare's Origin CA does accept. `4294967331`
   truncates to `365`, so a request for a nonsense lifetime would have been
   silently served as a one-year certificate.
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
covers `0`, `-1`, `1`, `31`, `3651`, `4294967331`, and `2147483800`, and
asserts that no RPC is forwarded.
`TestServer_IssueOriginCertificate_ForwardsAcceptedValidity` covers the seven
accepted values and asserts the forwarded `ValidityDays`.

### B2. ADR-0150 carried the ADR-0120 header (Low)

`docs/adr/0150-dedicated-vlan90-switch.md` began with
`# ADR-0120: Dedicated VLAN 90 switch for cluster nodes`. ADR-0120 is a
different, real document (`0120-config-rationale-history.md`), so the file was
claiming an identifier that belongs to someone else.

A sweep of all 158 files in `docs/adr` comparing each filename number against
the first `# ADR-NNNN` header found this as the only mismatch. The fix is the
one-line header correction; `0116-quorum-safe-raftd-restart.md` also lacks an
`ADR-NNNN` header, but it uses the `NNNN.` heading form consistently with the
rest of its content and does not collide with another number, so it was left
alone.

### B3. The cluster health endpoint has no consumer (Low)

`internal/frontend/server.go` registers `GET /cluster-health.json`, backed by
`internal/frontend/health_tree.go` (about 6.6 KB) and its test file. The
cluster health panel that used it was removed from
`web/templates/cluster_overview.html` in commit `f1e20ac` (PR #122, merged
2026-10-16).

Verified that nothing else reads it: no template, no JavaScript, and no CSS
in `web/` mentions `cluster-health`. The only other hit in the tree is a prose
comment in `internal/restshim/server.go:102` that mentions cluster health in
the context of `/v1/health`. `cmd/apiary-mcp` uses the managerd gRPC
`ClusterHealth` RPC, not this HTTP route.

Impact: dead code that is still registered on the frontend HTTP mux, so it
still answers requests. It is not a security problem, since the route sits
behind the same authentication as the rest of the frontend.

Recommendation: decide whether the endpoint is a public API surface that
should be documented and kept, or an internal implementation detail that
should be removed along with the tree builder. Issue #112 tracks the broader
cluster health question and is still open, so this is left for the owner
rather than removed unilaterally.

### B4. ADR numbers are not unique (Low)

Four files claim ADR-0150:

- `0150-ai-agent-cell-api.md`
- `0150-dedicated-vlan90-switch.md`
- `0150-native-system-statistics-and-cluster-monitoring.md`
- `0150-preferred-leader-bias.md`

Two files claim ADR-0152:

- `0152-atomic-vm-identity-rename.md`
- `0152-hastd-fixes-in-freebsd-15-1-release.md`

Every ADR in `docs/adr` has a unique filename, so no file can be renamed onto
another, but references that cite a number rather than a filename are
ambiguous. `docs/adr/MIGRATION.md` states that global number allocation is
"Pending Glen allocation", which means the numbers are placeholders rather
than a finished sequence. Renumbering would break existing references across
the docs and in Notion, so the numbers were left as they are and this finding
is recorded instead.

### B5. ADR numbering gaps (Info)

The numbered ADR files run from 0001 to 0155 with two gaps: 0126 and 0153.
Both gaps are consistent with reserved-but-unwritten numbers rather than lost
files, since `MIGRATION.md` still lists allocation as outstanding.

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
- `internal/backup`, `internal/jailnet`, and `internal/migration` still have
  no non-test importers; they were not reviewed in depth.
- The prior audit's findings from 2026-09-26 were not re-verified against
  `aea1be1`; they were treated as closed.
- Notion and the rest of the Apiary documentation set were not audited for
  consistency against the code beyond the ADR inventory.
