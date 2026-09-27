# ADR-0141: split `make update` into a safe sweep and a named per-Comb `force-restart`

## Status

Accepted (decision). Implemented in the `Makefile`, with two doc comments
corrected in `cmd/versioncheck` and `internal/buildinfo` that described
the old variable's meaning. No daemon behaviour changes.

## Context

ADR-0125 put a restart guardrail in front of `apiary_raftd` and, in the
same pass, changed the Makefile so that `update` would not restart it.
The variable was `INSTALL_SRCS_FILTERED`, defined as `INSTALL_SRCS` minus
`raftd`, and ADR-0125 stated the trade honestly: `update` "deliberately
updates raftd's binary and leaves the running process alone - so raftd
can go stale relative to everything else, and restarting it is a
separate, deliberate act."

Two costs were accepted there, and only one of them was the subject of
that ADR:

1. **`update` still restarted `managerd` with a bare `service ... restart`**,
   outside the lease, with no preflight. ADR-0125 recorded this as a
   known gap and said the fix was a Makefile client that speaks the
   guarded RPCs and holds an Admin credential.
2. **`raftd` went stale**, because nothing restarted it.

Cost 2 then actually happened, on a live multi-Comb colony, and it is
what prompted this ADR. Three of four raft voters were left running a
build days older than every other daemon on their own Comb, while the
`raftd` binary sitting beside them in `/usr/local/libexec/apiary` had
the current build's mtime. Nothing was broken; all four nodes were
healthy, all four voters agreed on the applied index, and the cluster
behaved correctly throughout. The node simply was not as deployed as it
looked, and the ordinary way of asking - looking at the file - said it
was.

That is the failure mode `internal/buildinfo` was written to prevent, and
it is worth being precise about why the documentation did not: the
documented check was "read the build stamp the daemon logs at startup",
which works, and the *undocumented* assumption was that a freshly
installed binary implies a freshly started process. On a Comb where
`raftd`'s binary is installed and its process is deliberately left alone,
that assumption is false by design.

Bringing the stragglers onto the current build then had to be done by
hand, one guarded RPC at a time, because the only coordinated path
existed. That worked - and worked well enough to find three real defects
in the guardrail on the way - but it is a four-round-trip, cross-node
procedure for what is, mechanically, a service restart. The Makefile
that installs the binary cannot start the process it just installed, and
that is the actual gap.

## Decision

`update` and a new `force-restart` target between them restart every
daemon `install` puts on disk. They do not overlap.

| target | restarts | guardrail |
| --- | --- | --- |
| `update` | `frontend`, `restshimd` | not needed - neither is a voter, neither holds cluster state |
| `force-restart` | `managerd`, then `raftd` | **bypassed entirely** - no lease, no preflight, no cross-node coordination |

The variable is no longer a subtraction. `UPDATE_RESTART_SRCS` and
`FORCE_RESTART_SRCS` are each written out, because "the list minus the
one that is dangerous" stops being the right description the moment
there are two lists, and a reader should not have to reconstruct which
daemons are consensus-critical from a modifier.

Three properties are load-bearing and are each argued below: the
**split**, the **order**, and the **post-restart assertion**.

### The split: this resolves cost 1 by removal, not by building the client

ADR-0125 named the fix for cost 1 as a Makefile client that speaks the
guarded RPCs. This ADR does not build that client, and
`force-restart` does not use the guarded RPCs. It takes a different
route: `update` stops restarting `managerd` at all.

That is a real change of approach and it deserves its own justification
rather than being presented as what ADR-0125 was waiting for. The
argument is that the guarded client is the right answer to a different
question. The question `update` was asking - "I have installed new
binaries on this Comb, start the daemons that hold no cluster state" -
is answered correctly by not touching the two that do. The question
`force-restart` asks - "restart the control plane on this Comb, now" - is
a deliberate, per-Comb, operator-initiated act, and the right instrument
for it is a named target an operator reaches for on purpose, not a step
that happens silently at the end of every deploy.

The two are separated precisely so that the deploy-shaped command is the
safe one. After this change, an operator (or a deploy loop) can run
`make update` across every Comb in the colony in any order, in parallel,
without a quorum preflight, and the worst thing that can happen is two
client daemons bounce.

### raftd is included, deliberately, and this is not a reversal of ADR-0125

ADR-0125 excluded `raftd` because `update` runs `service apiary_raftd
restart` directly, on whichever Comb you run it on, with no coordination
- and running `update` across all four Combs after a deploy, the obvious
thing to do, restarts all four voters at once and costs the cluster its
quorum.

That reasoning is unchanged and is not being overturned. What changes is
that the hazard is no longer attached to the deploy command. `update`
does not restart `raftd`, so a deploy sweep cannot take the cluster down
by accident; restarting `raftd` requires naming `force-restart`, which
prints the hazard on every invocation, on every Comb, in every run. A
mistake that requires typing the name of the dangerous thing is a
different category of mistake from one that follows automatically.

`force-restart` cannot enforce one-Comb-at-a-time - no Makefile knows
what the other Combs are doing - and this ADR does not pretend otherwise.
It states the constraint, prints it, and makes the operator's intent
explicit at the point of use. The coordinated path remains the Machine
page's per-service control (ADR-0125), or `apiaryctl` (ADR-0136), both of
which reserve a real cluster-wide lease.

### The order: `managerd` first, `raftd` second

This is the part most likely to be "tidied" away by a later reader, so
it is worth stating as a rule rather than a preference.

`raftd` confirms its own restart on startup: it dials `managerd` over
TLS and asks it to release the restart lease the new process is holding,
on a bounded five-attempt, three-second budget. A restart lease has no
TTL. So if `raftd` starts while `managerd` is still down, it can exhaust
that budget against a `managerd` that is not listening - and the
consequence is not a degraded node. It is a **blocked cluster**: the
lease stays held with no way to expire, every subsequent `raftd` restart
is refused cluster-wide citing `concurrent-manager-restart`, and nothing
in any node's health shows it. That failure was reached for real during
the verification of ADR-0125, and recovering from it required an
explicit `force`.

Restarting `raftd` while `managerd` is down is therefore the one way
this target could cause the exact failure it exists to help avoid. The
reverse order is harmless: `managerd` tolerates `raftd` being
unavailable and reconnects, so the safe order is also the natural one.

### The post-restart assertion

The loop waits up to fifteen seconds for each service to report running
and **stops the whole target** if one does not, rather than continuing
to the next.

Without it, a failed `managerd` restart would be followed immediately by
a `raftd` restart - the exact ordering hazard above, arrived at by
accident, with the failure swallowed. The assertion converts "managerd
is broken" into a loud stop instead of a silently half-restarted
managerd/raftd pair, and the error message says why stopping matters
rather than just that something failed.

## Consequences

- **`make update` can no longer restart `managerd`.** An operator who
  expected it to will find a stale `managerd` after `update` and must run
  `make force-restart`. The target's closing message says so explicitly,
  on every run, because this is the change most likely to surprise
  someone with an existing muscle memory.
- **`raftd` can still go stale**, exactly as ADR-0125 predicted. This
  ADR reorganises who restarts it; it does not make the staleness go
  away. The mitigation is the same one `internal/buildinfo` and
  `cmd/versioncheck` already provide, and `force-restart`'s closing
  message points at the startup log stamp rather than the file mtime -
  the distinction the whole cost-2 story turns on.
- **A colony-wide `force-restart` sweep remains possible and remains
  dangerous.** Naming the target loudly reduces how easily it happens;
  it does not make it impossible, and nothing here should be read as
  claiming otherwise.
- **Two code comments were wrong before this change, independently of
  the Makefile.** Both `cmd/versioncheck` and `internal/buildinfo`
  described `INSTALL_SRCS_FILTERED` as excluding `raftd` "from
  install". It never did - `install` has always installed all four
  binaries, and the filter only ever governed which ones `update`
  *restarted*. The comments were describing the staleness phenomenon
  correctly and its mechanism incorrectly, which is the kind of error
  that survives review precisely because the conclusion is right. Both
  now name the two variables and say "installed but not restarted".

## Rejected alternatives

**Build the lease-aware Makefile client ADR-0125 asked for.** This is the
alternative that most deserves to have been chosen, and the honest
reason it was not is that it is a larger piece of work with its own
credential-handling questions - a Makefile would have to hold an Admin
API key, and ADR-0125 already records that this cluster's API-key auth
is not even enabled. Shipping a target that needs a working Admin
credential before it can restart anything would have made the safe path
*less* available than the escape hatch. If the client is ever built,
this ADR's split is compatible with it: `force-restart` is where a
lease-aware implementation would go.

**Put `raftd` back into `update`.** Rejected for the reason ADR-0125
gave, unchanged. The new information since is that the guarded restart
path works and is well tested, which makes the Machine page *more*
attractive as the coordinated path, not less - it does not make a bare
`service` restart in a deploy loop safe.

**Have `force-restart` also restart `frontend` and `restshimd`.** This
was the explicit instruction's shape to avoid, and there is a real
reason: they are the operator-facing surfaces. Restarting them alongside
`managerd` means the UI and the REST API lose their backend and their
own process in the same instant, so a client reconnect races the backend
coming back rather than merely waiting for it. Isolating them means the
clients stay up and simply retry, which is a recovery the daemons
already handle. It also means `force-restart` cannot be run from a
browser session that the restart itself would drop.

**Name it something less blunt (`unsafe-restart`, `skip-guardrail`).**
Rejected. `force` is the word the RPC itself uses for the same concept -
`RestartNodeService`'s `force` field, and `guardrail_overridden` in its
response - so the Makefile target, the RPC parameter, and the raft
record all say the same thing in the same vocabulary. A target named
`skip-guardrail` would read as a description of mechanism rather than as
the same decision the API already asks an operator to make by name.

## Scope boundary, stated explicitly

This ADR does not make any deploy coordinated, does not add a lease to
`make update`, and does not close ADR-0125's "until `update` has a
client that can" clause. That clause is **not met** by this change, and
the split is a deliberate alternative to meeting it, taken because the
deploy-shaped command can be made safe by subtraction while the
client-shaped work is a larger change with credential questions this
ADR does not answer.

## Relationship to existing decisions

- **ADR-0125** - the guardrail and the original exclusion. This ADR
  keeps the exclusion of `raftd` from `update` and keeps its reasoning
  intact; it restates the filter's stated exit condition as explicitly
  unmet (above) rather than pretending this satisfies it.
- **ADR-0136** (`apiaryctl`) - the local CLI is the other coordinated
  path, and `force-restart`'s own message points at it as the
  alternative to using this target during a sweep.
- **ADR-0097 / ADR-0103** - the reachability probe and the restart-lease
  FSM this target deliberately does not use.

## Verification

- `bmake -n update` expands the restart loop to exactly `frontend` and
  `restshimd`; `bmake -n force-restart` expands it to `managerd` then
  `raftd`, in that order. `bmake` specifically, since that is what the
  Combs run; the `${VAR:Nraftd}` modifier this ADR removes was a BSD
  make extension and the file is now portable to both makes.
- `gofmt -l .` clean, `go build ./...` clean, `go vet ./...` clean,
  `go test ./...` passes across the repository.
- The target was then run for real on one Comb of a live multi-Comb
  colony - a non-leader voter, with the rest of the colony healthy - and
  the result is recorded in `.local/SHARED.md` rather than here, since
  that file is where live-cluster evidence belongs.
