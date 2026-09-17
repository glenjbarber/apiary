# ADR-0105: Standalone-to-Colony joiner conversion

## Status

Accepted

## Context

ADR-0104 left raftd's bind address read-only, deferring "a separate,
future membership-preparation workflow that can establish an empty
joiner state and require an explicit destructive acknowledgement when
resetting an independently bootstrapped node." This is that workflow.

An already-bootstrapped, standalone single-node Comb has no safe path
into an existing Colony today. `raftd`'s `join`/`await_join` config
fields are only ever consulted on a genuinely fresh, empty data
directory (`hadState` wins over both unconditionally at startup), and
`ApproveJoinRequest` has no guard against a joiner that already has its
own committed Raft log - a real, separately tracked gap (see this
project's own "Remaining committed limitations"). Converting such a
Comb into a joiner today means an operator manually stopping raftd,
hand-editing `raftd.json`, and running `raftd -reset` by hand, hoping
they get every step right - exactly the kind of operation this
project's history (a 2026-09-11 stranded-cluster incident, ADR-0103's
guardrails) has already shown needs a guarded workflow, not tribal
knowledge and a manual walkthrough.

## Decision

`ConvertStandaloneToJoiner` is a narrow, single-purpose, Admin-only RPC
and Machine page action - not a generic Raft configuration editor. It
does not expose arbitrary editing of `internal_token`, Raft TLS
material, `data_dir`, or the one-shot recovery flags; the existing
read-only Raft configuration panel (ADR-0102) is unchanged for
everything else.

The action requires an exact, typed confirmation phrase
(`yes-convert-to-joiner`, matching raftd's own `-reset` and
`apiaryinstall -apply-network`'s established exact-match convention)
and fails closed, before any irreversible step, on: a wrong phrase; an
unreadable or malformed local `raftd.json`; an invalid `raft_bind`
(empty, or a host part of `0.0.0.0`/`127.0.0.1`/`localhost` that no
remote Colony peer could ever dial); and an unreachable target Colony
member, checked via the exact same `evaluateJoinReachability` helper
`ApproveJoinRequest` itself uses, so this can never diverge from what
the target's own preflight will find.

On confirmation: stop local `apiary_raftd`; confirm existing raft state
is actually present (failing closed and restarting raftd unchanged if
not - this action is for converting real state, not a substitute for
the ordinary fresh-node join path); reuse raftd's own `-reset` one-shot
mode by invoking the installed binary directly, rather than
reimplementing its rename-to-timestamped-backup logic, so there is
exactly one place that logic lives; rewrite only `raft_bind` and
`await_join` in `raftd.json` (clearing any `join` value, since the two
are mutually exclusive), preserving `node_id`, `socket`, `data_dir`,
`internal_token`, and every Raft TLS field untouched; restart
`apiary_raftd`; and poll it via a plain TCP dial until it is confirmed
listening at the new `raft_bind` before proceeding further. Only then
does it submit an ADR-0083 `RequestJoinColony` call against the
operator-supplied target, using this node's own current, live
identity - never a cached or previously-configured value. The
resulting join request is still fully subject to the existing mutual
authorization flow: a correlation code is returned for the operator to
relay, and an Admin on the target Colony still separately reviews and
approves it through the existing `ApproveJoinRequest` path, reachability
preflight included. This action never merges two divergent Raft
histories on its own - it only prepares this Comb to make a normal,
already-guarded join request.

Every failure after the reset step still reports the timestamped
backup path, so the prior state's location is never lost track of even
if a later stage (saving the rewritten config, restarting raftd,
confirming it is listening) fails. No stage's failure is ever reported
alongside a populated join-request id/code - the response is never
ambiguously "partially ready."

## Consequences

- An operator can convert an already-populated standalone Comb into a
  Colony joiner from the Machine page, without hand-editing
  `raftd.json` or manually invoking `raftd -reset`.
- The action is irreversible in the ordinary sense (the prior
  standalone cluster identity is gone once reset), but the prior raft
  state itself is preserved as a timestamped backup, not deleted.
- raftd's stop/reset/start sequence is a new, dedicated code path,
  separate from `RestartNodeService`'s existing managerd-restart
  guardrail (ADR-0103) - raftd remains deliberately excluded from that
  guardrail's own service-restart allowlist, being consensus-critical
  for a different reason than the "don't restart two managerds at
  once" concern that guardrail exists for.
- The already-known gap that `ApproveJoinRequest`/`AddVoter` has no
  guard against a joiner with a non-empty committed log remains open -
  this ADR's own fail-closed "existing state must be present, and gets
  reset before joining" sequencing is what keeps that gap from being
  reachable through this specific workflow, but it is not a general fix
  to `ApproveJoinRequest` itself.

## Verification

Focused unit tests in `internal/manager` cover every fail-closed
condition (wrong phrase, unreachable target, invalid `raft_bind`, no
existing state, and a failure at each of stop/reset/save/start/listen),
confirm no adapter action runs before the phrase and reachability
checks pass, confirm the backup path is reported on every failure that
occurs after a successful reset, and confirm the submitted join
request uses this call's own current node identity rather than a
cached value - using genuine on-disk raft state (via `internal/raft`'s
own `New`/`Bootstrap`) rather than a mocked filesystem, since
`HasExistingState` reads real BoltDB contents. `internal/frontend`
tests cover the form-to-RPC field mapping and that an error response
never also renders a success result. Live deployment against any real
host is explicitly out of scope for this change and requires separate,
explicit authorization.
