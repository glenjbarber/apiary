# ADR-0142: Refuse to restart managerd from inside managerd

## Status

Accepted. Amends ADR-0125's inventory and ADR-0141's deployment path.

## Context

`RestartNodeService` is a public, Admin-gated RPC. It runs the restart
in a goroutine **inside the calling managerd**, which sleeps 250 ms and
then synchronously shells out to `service <name> restart`:

```go
go func() {
    time.Sleep(250 * time.Millisecond)
    if err := s.services.Restart(restartCtx, name); err != nil { ... }
}(...)
```

For every service except one, that works. `rcServiceController.Restart`
blocks in `exec.CommandContext`, the stop half kills the target, the
start half runs, and the orchestrating managerd is unaffected.

For `apiary_managerd` the target **is** the orchestrating process. The
stop half of `service apiary_managerd restart` kills managerd, and the
goroutine dies with it before the start half executes. The service stops
and does not come back.

This was not inferred from reading the code. It was reproduced on two
Combs: managerd stopped, never returned, and a manual
`service apiary_managerd start` was required each time. The new process
came up healthy and confirmed its guardrail lease cleanly afterwards,
which is how the fault was localised to the restart half rather than to
the guardrail.

There is no ordering trick that avoids this. Any in-process
orchestration of your own death fails the same way: the process that
would perform the start is the process the stop terminates.

`IssueOriginCertificate` had the same defect independently, and worse:
it called `s.services.Restart(ctx, "apiary_managerd")` **synchronously**,
on the request path. Issuing or renewing an Origin CA certificate would
kill managerd mid-RPC — the caller would see a dropped gRPC connection
and a downed control plane rather than any error at all.

## Decision

`apiary_managerd` is removed from the restartable inventory and refused
by both `RestartNodeService` and `PreflightRestartNodeService`, with a
message that names the working alternative:

- `apiaryctl force-restart`, which restarts managerd and then raftd from
  rc.d, in a process that is not managerd (ADR-0141, ADR-0136),
  deliberately bypassing the restart lease and the quorum preflight;
- `service apiary_managerd restart`, if only managerd is needed.

`IssueOriginCertificate` no longer restarts anything. The certificate is
installed, `restart_scheduled` is `false`, and a new
`restart_required_msg` states that a restart is still outstanding and
how to perform one. A caller is never told the certificate is live when
it is not, and is never left with a dead managerd.

`force` does not override the refusal. Overriding a quorum block is
acknowledging a known and understood risk; overriding "this will not
work at all" is not that, and must not be presented as the same lever.

## Consequences

**The guarded lease is reachable only through `apiary_raftd`.** Both
services share one lease implementation, so the machinery is still
covered — it is simply no longer exercised through managerd. The
end-to-end guardrail test was retargeted to raftd rather than deleted.
The single-voter test cluster means the quorum check now blocks a raftd
restart unless forced, so those tests pass `force`; what they assert is
the lease/confirm/cooldown machinery, not the quorum arithmetic, which
has its own tests.

**`managerd` remains guardrailed but not restartable**, which the
existing invariant test had classified as always-a-bug. It is now a
deliberate, asserted exception, and the reason it cannot strand a lease
is ordering, not luck: the refusal happens *before*
`reserveRestartLease` is called, so no lease is ever taken for a service
that can never be restarted. `TestRestartNodeService_RefusesManagerdSelfRestart`
asserts that ordering directly, because if it ever inverts the
exception becomes a real stranded-lease bug.

**A config change to managerd no longer takes effect on save.** This was
already true in practice for `known_peer_addresses`, which
`SetKnownPeerAddresses` populates only at startup, and it is now visible
in the Origin CA flow. The correct general fix is for a supported config
change to refresh the running server, or to say plainly that a restart
is required. Neither is done here.

**`apiaryctl force-restart` is the only supported way to restart
managerd, and it bypasses the guardrail entirely** (ADR-0141). That is a
real cost
of this decision, and it is preferable to a guardrail that can stop the
control plane and cannot start it again. An operator debugging a downed
managerd is worse off than one reading a refusal.

## Related

- ADR-0103 — the concurrent-restart and cooldown guardrail.
- ADR-0125 — the guarded raftd restart, and the quorum-safety
  evaluation that remains the reason raftd is safe to expose.
- ADR-0141 — the `update` / `force-restart` split, which this decision
  makes load-bearing rather than merely convenient.
