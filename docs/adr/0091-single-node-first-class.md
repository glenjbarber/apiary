# ADR-0091: Single-node is first-class, not a lesser bootstrap state

## Status

Accepted

## Context

The user's stated goal: "everything needs to be able to run multi-node,
but a single-node system is completely acceptable." A full audit (an
Explore pass across raftd/raft, `internal/assumecheck`/`internal/health`,
the reconciler's peer-forwarding features, HAST/replication, frontend
copy, docs, and the installer) found the codebase already treats
single-node correctly in nearly every place that matters: raft's default
bootstrap, `internal/assumecheck`'s peer checks, health's
`PeerReachability`, and every peer-forwarding feature (ISO fetch
ADR-0041, jail template fetch ADR-0089, console/serial-log/VM-snapshot
forwarding ADR-0065/ADR-0090) all cleanly no-op or resolve `Unknown`
rather than error or warn when there are no peers configured.
`docs/bootstrap.md` already presents single-node and multi-node as two
co-equal, parallel paths, not a required sequence.

Three real gaps surfaced, all now fixed:

1. `internal/raft/config.go`'s package doc comment described the whole
   package as a "single-node-bootstrap (for now)" consensus node -
   phrasing that directly contradicts "single-node is a legitimate,
   permanent end-state," even though the package has fully supported
   multi-node via join since ADR-0083.
2. `internal/cluster/peer.go`'s `fetchImageFromPeer`/
   `fetchTemplateFromPeer` reported "no peer forwarding is configured on
   this node" whenever `Peers == nil` - true, but every single-node
   deployment hits this path by definition whenever a referenced
   image/template is missing locally, and the wording reads as "your
   setup is incomplete" rather than "this is expected; the file just
   isn't here yet."
3. The Resilience Coverage Map (ADR-0062) always renders the
   `quorum-tolerance` scenario as `StatusUnsafeOrImpossible` (a red
   badge) whenever `EvaluateQuorumTolerance` returns `ResultFalse` - and
   for a single-voter cluster, losing the only voter unconditionally
   loses quorum, so this is **always** false. Every single-node
   deployment therefore permanently shows a failing, alarming-looking
   badge on a coverage page, which reads as "this deployment has a
   problem" purely because it has one node - the clearest actual "single
   -node treated as second-class" finding in the whole audit.

## Decision

### 1. Reword the raft package doc comment

`internal/raft/config.go` now describes the package as bootstrapping "as
a single-voter cluster by default and can grow to a multi-node Colony
via a join... single-node is a fully supported, permanent deployment
shape, not a transitional bootstrap state."

### 2. Reword the two peer-forwarding "not configured" errors

Both `fetchImageFromPeer` and `fetchTemplateFromPeer` in
`internal/cluster/peer.go` now read, e.g.: *"image %q not found locally,
and no peer forwarding is configured on this node to look elsewhere
(expected on a single-node deployment - add the image locally, or join
a peer that already has it)."* The underlying substring
`"no peer forwarding is configured"` is preserved verbatim so the
existing `internal/cluster/jail_test.go` assertion needed no change -
only the framing around it changed, from "this is missing" to "this is
expected, here's what to do about the actual problem (the missing
file)."

The `"not found on any known cluster node"` message (fires only when
`Peers != nil`, i.e. genuinely inside a multi-node topology already) was
left unchanged - it's already accurate and cluster-centric framing is
correct there, since peers are in fact configured.

### 3. Reframe the quorum-tolerance coverage scenario for single-voter clusters - without changing the underlying fact

The user was asked directly how this should be presented and chose:
**keep the same underlying fact, but present it as a neutral/expected
state rather than a failing one**, over leaving it red as-is or making
it operator-configurable.

`internal/coverage.ClassifyQuorumTolerance` gained a `voterCount int`
parameter (the current raft membership's voter count; 0 when raft is
unreachable and the result is unknown regardless). When
`e.Result == invariant.ResultFalse && voterCount != 1`, behavior is
unchanged (`StatusUnsafeOrImpossible` - a genuinely fragile multi-node
cluster, e.g. only one live voter left out of three, is still a real,
actionable hazard worth flagging). When `voterCount == 1`, a `False`
result instead resolves `StatusSimulated` - the same "a real mechanism
ran and confirmed the answer" status every non-hazard result already
gets, since `Status` in this package describes evidence trustworthiness,
not whether the news is good (the existing `coverageBadgeClass` doc
comment already establishes this distinction for `StatusSimulated`
generally).

No new `Status` value was added, deliberately - `coverage.Status` is
documented as "CODEX's own five-word vocabulary," and this reframing
fits inside the existing "a real mechanism ran and confirmed a
definitive answer" category rather than needing a sixth.

`internal/invariant.EvaluateQuorumTolerance`'s own explanation text also
changed for the single-voter case: instead of "The cluster does NOT
tolerate losing at least one current voter - quorum would be lost," it
now reads "This is a single-node deployment: losing its one voter ends
quorum entirely, since there is no other voter to fall back on. This is
an expected property of running one node, not a misconfiguration." The
underlying `Result` stays `ResultFalse` in both the invariant and the
coverage scenario - this is a presentation change, not a change to the
reported fact. The Operational Invariants page (`/invariants`, ADR-0060)
is a raw evidence catalog by design ("continuously evaluated to
true/false/unknown with cited evidence"), not a coverage scorecard, so
it intentionally keeps showing the plain `false` badge there - only its
explanation text improved; only the Resilience Coverage Map's `Status`
classification (which does function as a coverage scorecard) changed.

### Deliberately NOT changed: the "why not reboot this node" blocker

`internal/whynot.AnswerHiveReboot` cites `quorum-tolerance` as a
`Blocker` whenever `!quorum.Survives` - true for a single-node cluster,
since rebooting your only node genuinely does end the cluster's ability
to serve raft requests for the duration. This is real, actionable,
action-specific information ("rebooting this node has this
consequence"), not a standing judgment that the deployment itself is
deficient - it stays exactly as-is.

## Consequences

- `internal/coverage.ClassifyQuorumTolerance`'s signature changed (added
  `voterCount int`); both call sites in
  `internal/frontend/resilience_coverage.go` updated (`0` when raft is
  unreachable, `len(voterReachability)` otherwise).
- New tests: `internal/coverage/scenario_test.go` gained
  `TestClassifyQuorumTolerance_SingleVoterFalseIsNotUnsafeOrImpossible`
  (and renamed the existing multi-voter test to make the distinction
  explicit); `internal/frontend/resilience_coverage_test.go` gained
  `TestHandleCoveragePage_SingleVoterQuorumLostIsNotUnsafeOrImpossible`,
  an end-to-end regression test against a real single-voter `StatusResponse`.
- Everything else the audit checked - raftd bootstrap, `assumecheck`,
  `health`, reconciler peer-forwarding no-ops, HAST, frontend copy,
  docs, and the installer - already treated single-node as a fully
  valid, permanent configuration with no spurious errors or second-class
  UI/log treatment, and needed no changes.
- `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .` all
  clean after these changes.
