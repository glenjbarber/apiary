# ADR-0181: Upstream hastd and hastctl fixes are in FreeBSD 15.1-RELEASE

Status: Proposed
Supersedes: ADR-0008 (in part: the requirement for a locally built hastd with D57511 or D59521), ADR-0022 (in part: the requirement to patch hast_proto_recv_hdr for FreeBSD bug 298085), ADR-0082 (in part: the statement that the hastd source patch is permanently operator-owned)
Superseded by: None
Affected projects: Apiary
Legacy identifier: ADR-0152 (Apiary), renumbered to ADR-0181 (Apiary)
Loreloom identifier: Pending allocation by Glen

## Status qualifications

This ADR supersedes only the parts of ADR-0008, ADR-0022 and ADR-0082 that depend on running a locally patched hastd or hastctl.
The rest of each record stays in force, including the HAST configuration rendering, lifecycle and replication decisions.
The superseded records remain available as history.
Dated 2026-10-14.
Proposed means the wording has not been approved, and no approval date or approver is recorded here.

## Merge note (2026-10-27)

This record was originally numbered ADR-0152. ADR-0152 was claimed by
two files, so the number was ambiguous and could not identify a
decision. It was renumbered to ADR-0181, which was free in the tree
when the collision was closed. The number in the header, the filename
and every reference to it were changed together; no content changed.

## Context

Apiary replicates VM disks with HAST (ADR-0008, ADR-0026).
Stock FreeBSD hastd and hastctl had two defects that blocked this on the project machines, so every machine ran a hastd and hastctl built from a patched source tree.

- ADR-0008 records a connection-migration defect (FreeBSD bug 292322).
  The first fix, D57511, was superseded by a cleaner review, D59521, and both were built and confirmed working by hand.
- ADR-0022 records a zero-size-message assertion failure in `hast_proto_recv_hdr`, which crashed `hastctl role init all` and therefore every `service hastd onerestart` (FreeBSD bug 298085).
  It was fixed by guarding the assertion and receive behind `hdr.size > 0`, then installed by rebuilding `sbin/hastd` and `sbin/hastctl`.

ADR-0082 then made the patch a permanent operator task: `apiaryinstall` reports a missing patch and never applies it.
The README and `docs/bootstrap.md` repeat that statement.

Glen reports that all of the hastd-related changes Apiary needed are now in FreeBSD 15.1-RELEASE.

## Decision

1. On FreeBSD 15.1-RELEASE and later, Apiary needs no locally built or patched hastd or hastctl. The upstream fix is in every supported FreeBSD release.
2. Apiary documents FreeBSD 15.1-RELEASE or later as its supported floor, and as the baseline for HAST-replicated disks.
3. Apiary still never builds or applies FreeBSD source patches.
   The decision in ADR-0082 that this is operator-owned stays, but it no longer describes a required step on any supported release.
4. The patch instructions in ADR-0008 and ADR-0022 are retained as history for releases that predate the upstream fix.

## Consequences

Documentation to update, none of it done by this ADR:

- `README.md`, near lines 132 and 786, which describe the patched hastd and the permanently report-only patch.
- `docs/bootstrap.md`, near line 1071.
- ADR-0026, which says the nodes run a patched hastd.
- Back-references in ADR-0008, ADR-0022 and ADR-0082, using the existing "Superseded in part by" convention (see ADR-0047).

Code follow-up, done 2026-10-19:

- `internal/install/checks.go` reports ok on FreeBSD 15.1-RELEASE and later, and manual with an upgrade hint on earlier releases. It stays report-only.
- `cmd/apiaryinstall/main.go` help text, `docs/bootstrap.md`, and the README preflight paragraph describe the same floor.

Machines running a release with a patched hastd keep working.

## Open items

These are not decisions.

- Fill in the upstream references: the commits or revisions that carry the D59521 fix (bug 292322) and the bug 298085 fix, and where they landed in 15.1-RELEASE.
  They were not verified when this ADR was written, because FreeBSD Bugzilla was not reachable.
- Decide whether releases before 15.1 stay supported for HAST with the patches, or are dropped.
- Confirm that no other local hastd or hastctl change is still in use that this ADR does not name.

## References

- [FreeBSD bug 292322](https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=292322)
- [D57511](https://reviews.freebsd.org/D57511)
- [D59521](https://reviews.freebsd.org/D59521)
- [FreeBSD bug 298085](https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=298085)
- ADR-0008, ADR-0022, ADR-0026, ADR-0082
