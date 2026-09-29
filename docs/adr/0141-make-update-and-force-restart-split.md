# ADR-0141: split `make update` into a safe sweep and a named per-Comb `force-restart`

## Status

Accepted (decision). Implemented in the `Makefile`, with two doc comments
corrected in `cmd/versioncheck` and `internal/buildinfo` that described
the old variable's meaning. No daemon behaviour changes.

**Amended 2026-09-27** to close the cooldown gap this ADR left open, and
to state the gap in the Scope boundary below rather than leave it
implied. The amendment added `scripts/record-forced-restart.sh` and one
line in the `force-restart` loop. It still takes no lease and still
changed no daemon code; what it added was the record the cooldown reads.
See "The cooldown must still learn" under Decision.

**Amended 2026-09-28** to move the command off the Makefile. Everything
this ADR decides still holds - the split, the order, the absence of
coordination, the deliberate pending-restart record with `lease_id` 0.
What changed is where the command lives, and the reason is one fact
about the deployment rather than about the design: **`force-restart` is
an operation on a running Comb, so it has to be a file on the Comb, and
a Comb has no source checkout to run a make target from.** It is now
`apiaryctl force-restart` (ADR-0136, `internal/forcerestart`), installed
to `/usr/local/libexec/apiary/apiaryctl` beside the daemons.

**Amended 2026-09-28, later the same day: the make target is deleted,
not retained as a convenience.** The paragraph above is superseded on
its last two sentences. `make force-restart` no longer exists, in this
ADR's text and in the Makefile.

The reasoning for keeping it was that a shim which runs the installed
command cannot mislead anyone. That was wrong in a way the shim's own
output demonstrated. A target named `force-restart` is a name an
operator can read in `make` output, in a build log, or out of habit,
and it cannot be typed on the machine that matters, because the machine
that matters has no Makefile. It does not have to be wrong to be
harmful: it only has to be *present*. Within one merge of being
introduced it was quoted as a fallback in the SHARED.md, in managerd's
own advice to an operator, and on the Colony update page - each time as
the thing to fall back on when the installed command was not yet there.
A shim is a thing to quote; its absence is not.

So the target, and `FORCE_RESTART_SRCS` with it, are gone. Three things
in the body below now describe a Makefile that no longer has them, and
are history rather than instructions: the `force-restart` target in
"Decision" and its table, the two-list argument that follows it, and
`bmake -n force-restart` in "Verification". The split, the order and
the record are unchanged and now live in `internal/forcerestart` -
`DefaultPlan` holds the order, `internal/restartplan` holds the record,
both tested there.

Where the rest of the body says "the target", read "the command",
`apiaryctl force-restart`, unless the sentence is one of the three
listed above. Two are worth naming because they read as claims about
the implementation rather than as history. "No Makefile knows what the
other Combs are doing" is now understated: no `apiaryctl` process does
either, and the conclusion is unchanged. And the closing-message
argument, that the command prints the hazard on every invocation, is
carried by `apiaryctl`'s own banner and timeout diagnostic rather than
by make output.

**Amended 2026-09-29: the command now refuses on the leader, and on
its own inability to tell.** Everything above is unchanged. What is new
is one more preflight, asked once against this Comb's own raftd before
the banner and before the first restart, and a third refusal. It is
recorded at length under "The leader check" below because the reason it
is a refusal and not a warning is the substance, and because the case
that decides the design - the check that could not be answered - is the
one a reader would otherwise assume away.

`TestMakefile_HasNoForceRestartTarget` in `internal/forcerestart` is the
regression, and it is a negative over the whole file rather than a check
on a recipe body. That is deliberate and it is a correction: the test
it replaced sliced out the recipe and asserted the forbidden strings
were absent from it, while the prose in the comment above the target
contained the very command string the test was trying to prove the
target ran. A `Contains` over prose is satisfied by prose.

Two consequences for the text below, both recorded here rather than
silently edited in place. The `scripts/record-forced-restart.sh` calls
described in "The cooldown must still learn" are now
`internal/forcerestart`'s own writer through `internal/restartplan` -
same file, same field names, same `lease_id` 0, no clobbering of an
existing pending record. And the node-id resolution is no longer a
`sed`/`hostname` chain in shell: it goes through
`internal/raftdconfig` and `internal/nodeconfig`, which resolves
per-service through the loader the restarted daemon uses for itself,
falling back to ADR-0111's `common.json` and then the hostname. That
change is a small behavioural improvement rather than a port: a `raftd`
restart is now recorded under the identity `raftd` itself would report,
where the shell chain could record it under `managerd`'s.

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

### The leader check: a refusal, asked of this Comb's own raftd

The banner has always said not to restart `raftd` on the current
leader. That was a sentence and an assumption, and the assumption is
the kind this command is not allowed to have: `force-restart` exists to
be run by an operator whose attention is on a daemon that is down,
which is exactly the state in which "and which Comb is the leader" is
the one thing not being held in mind. So the command asks, and refuses
on the answer.

**It asks this Comb's own raftd, over the socket `raftd.json` names.**
Not a peer, not a managerd, not rc.d. "Is some *other* Comb the leader"
is not the question; the question is whether *this* Comb is, and only
this Comb's own raftd can answer it. That also makes it a
measurement rather than an inference, in a package whose `Host`
interface exists precisely because the obvious inference - rc.d's
`service <name> status` - reports "not running" for every apiary daemon
on a Comb where all of them are up and listening.

**An unanswered question refuses too, and that is the half of this
that matters.** "Is this Comb the leader?" and "I could not find out
whether this Comb is the leader" are different answers, and from here a
dead `raftd`, a socket path in `raftd.json` naming some other Comb's
socket, and an internal token that does not match all look like the
second one. Treating any of them as "no" would make the check
decorative, because an unanswerable check passes - and an unanswerable
check is what an incident produces. A Comb whose leadership is not
established is a Comb this command will not restart `raftd` on. The
two refusals print different messages on purpose, because their next
actions are entirely different: one is a leadership problem to wait
out, the other is a broken or absent `raftd` to go and look at.

**There is no flag and no prompt.** A prompt is answered by reflex
under pressure, a flag is passed by a script written once, and a
command whose dangerous mode is a word on a command line will
eventually have that word in a script. The refusal is unconditional,
and the message says so explicitly, so that an operator who has
legitimately concluded they must restart the leader knows this is a
decision to make elsewhere rather than a step they are missing.

**Where the check lives.** In `internal/forcerestart.Run`, as a
preflight beside the existing unknown-port refusal, not in
`cmd/apiaryctl`. The property belongs to the restart rather than to
one way of asking for it, and a check a caller can decline to make is a
check that eventually is not made. `Run`'s zero `Options` therefore
remains a production configuration, which is the property that makes
the safety unconditional.

The dial itself is `internal/localraft`, extracted so that
`apiaryctl force-restart` and `apiaryctl join-authorize` cannot
disagree about which socket this Comb has. Two copies of "read
`raftd.json`, dial that socket, present that token" is two places for
the two to get out of step, and a disagreement there is not a crash -
it is the wrong Colony's answer, read as confidently as the right one.
The extraction also fixed a real inconsistency: `join-authorize`
resolved the socket from its `-raftd-config` flag and the internal
token from the *default* path, so on a Comb with a non-default
`raftd.json` the two could name different files.

This is **not** coordination, and it does not move the Scope boundary
below. It learns one fact about one Comb and stops. It takes no lease,
reserves nothing, waits on no other node, and the two Combs an operator
runs it on are as uncoordinated with respect to each other as they were
before it existed.

### The cooldown must still learn: a record, not a lease

Taking no lease is the point of this target, and it stays that way. But
the ADR-0103 cooldown is not fed by leases. It is fed by
`RestartRecord` entries in the FSM, and the only thing that writes one
is the confirm path - the restarted daemon's own next startup, reading a
pending-restart record. A `force-restart` never wrote that record, so the
guardrail went on believing no restart had happened: an operator could
force-restart `raftd` on one Comb and be granted a coordinated `raftd`
restart on another seconds later, which is the concurrent-restart window
the 600s cooldown exists to close.

So the loop writes that record immediately before each `service`
restart. Since the 2026-09-28 amendment it does so from
`internal/forcerestart`, through `internal/restartplan`, rather than
through a shell script. It writes the same pending-restart record
`RestartNodeService` writes - same directory, same file name, same three
JSON fields, all of which `internal/manager` and `internal/restartplan`
already hold a test asserting are byte-identical - with **`lease_id` 0**.

Zero is the whole mechanism. `applyRecordRestartCompleted` writes the
cooldown record unconditionally, on the stated grounds that a real
restart really did complete regardless of whether the lease below still
matches, and releases a lease only on an exact `lease_id` **and**
`holder_node_id` match. So `lease_id` 0 informs the cooldown and cannot
release anyone's lease, including this node's own real one. Nothing in
the confirm path rejects a zero lease id.

Two cases deliberately write nothing, and both say so loudly on stderr:

- **A pending record already exists.** That is a real lease in flight,
  and `Save` overwrites. Replacing it with `lease_id` 0 would strand
  that lease permanently - the daemon would confirm lease 0, the real
  lease would never match, and ADR-0103's leases have no TTL. The
  operator is told the cooldown will not learn about this restart.
- **The `node_id` cannot be determined at all.** `applyAcquireRestartLease`
  counts a record toward the cooldown only when its holder is a
  currently-known voter, so a record with an empty or wrong `node_id` is
  written and never blocks anything - a silent no-op indistinguishable,
  from the outside, from working. Writing nothing and saying why is the
  honest option.

  "Determined" deliberately means what it means for the daemons, not
  what is convenient here. The script resolves `node_id` in the same
  order `managerd` and `raftd` do - `raftd.json`, then `managerd.json`,
  then ADR-0111's `common.json`, then `os.Hostname()` - because a
  record only counts when its `node_id` equals a raft member id, and
  those ids are the hostnames. Reading only the first two files looks
  correct and is not: a live test on a Comb whose `raftd.json` and
  `managerd.json` both omit `node_id` - which is what any Comb set up
  before that field was written into its config looks like, and is not
  hypothetical - declined to record anything at all, with a correct and
  loud warning that the cooldown would not learn. A warning nobody reads
  is not a substitute for a record.

The script always exits 0. It is bookkeeping on an emergency path, and
the one thing it must never do is stand between an operator and a
restart they need during an incident. Every failure is a message on
stderr instead.

## Consequences

- **`make update` can no longer restart `managerd`.** An operator who
  expected it to will find a stale `managerd` after `update` and must run
  `apiaryctl force-restart`. Both the update target's closing message and
  force-restart's own say so explicitly, on every run, because this is
  the change most likely to surprise someone with an existing muscle
  memory.
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

The 2026-09-27 amendment does not move that boundary. Writing a restart
record is not coordination: nothing is reserved, nothing is checked
against the other Combs, and no node waits on any other node. It makes
this target *tell the truth after the fact* rather than stay silent, and
a record that informs a later decision is not the same as a lease that
constrains this one. ADR-0125's clause remains unmet, and
`force-restart` remains the uncoordinated act it has always been.

The 2026-09-29 leader check does not move that boundary either. It is
one read of one Comb's own raft state, and a refusal: it reserves
nothing, checks nothing about the other Combs, and makes the two
Combs an operator runs it on no more coordinated with respect to each
other than they were. What it changes is that one Comb - the one that
was going to be hurt by the restart, and only that one - now refuses to
be restarted by this command. That is subtraction, like the split
itself, not the client ADR-0125 asks for.

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
  `restshimd`. `bmake` specifically, since that is what the Combs run;
  the `${VAR:Nraftd}` modifier this ADR removes was a BSD make extension
  and the file is now portable to both makes. This was where the
  `managerd` then `raftd` order was pinned; since 2026-09-28 the order
  lives in `internal/forcerestart`'s `DefaultPlan`, and
  `bmake -n force-restart` expands to a message naming it plus
  `apiaryctl force-restart`.
- `gofmt -l .` clean, `go build ./...` clean, `go vet ./...` clean,
  `go test -count=1 ./...` passes across the repository.
- `internal/forcerestart` - the behavioural suite, in Go, so CI runs it
  rather than a developer having to remember. It executes the real
  restart loop with the host commands replaced by a recording stand-in,
  and the real record writer against fixture config files, so the bytes
  asserted on are the bytes a Comb would get. The cases that matter:
  that a restart is confirmed by its own listener port and never by
  `service ... status`; that raftd's port cannot satisfy managerd's wait
  or the reverse; that the wait is a bounded poll of exactly 15 probes;
  that a `sockstat` which cannot be run is an error rather than an
  answer; that a port number without its `host:` delimiter is not a
  match; that a timeout stops before the next service and lists every
  restart already issued; and that a plan entry with no known port is
  refused before the first restart.
- The leader check, since 2026-09-29, in the same suite and the same
  style: it executes `Run` against a fake that answers the leadership
  question, and the cases are the three answers. On yes, the run does
  not complete, `Err` is `*forcerestart.ErrIsLeader`, no restart
  command and no pending-restart record exists afterwards, the *only*
  host call in the whole run is the status query, and the about-to-
  restart banner is absent. On unanswerable, the same, with
  `*forcerestart.ErrLeaderUnknown` and a different message - and a case
  asserting the two messages do not drift into each other, because the
  operator's next action is different for each. On no, the run
  proceeds, and the honest-path case asserts the status query is the
  *first* host call, so a check made after `managerd` is already down
  cannot pass.
  - Those three were checked by mutation rather than by reading them:
    making `IsLeader` not refuse, making an unanswerable check count as
    a no, and moving the check below the restart loop each produce a
    failing test, and the last produces three.
  - `internal/localraft` - the dial, against a real gRPC server on a
    real unix socket with the real token interceptor, not a mocked
    client. It asserts that a leader is reported as one, that a
    follower's knowledge of the leader is carried through rather than
    dropped, that the internal token is read from the *same* config
    that named the socket, and - the case the whole refusal rests on -
    that an unreachable `raftd` and a rejected token both return an
    error with a zero `Status` rather than a `Status` with
    `IsLeader: false` and no error. It also pins that a `raftd.json`
    naming no socket resolves to `raftd`'s own default through
    `raftd`'s own loader, which is what the loader does today and what
    a reader would otherwise have to take on trust.
  - `cmd/apiaryctl` - the operator-facing text, built and run rather
    than matched against the source: `apiaryctl help` and a bare
    `apiaryctl` both name the leader refusal and the unanswered-check
    refusal, and `apiaryctl force-restart <arg>` still reports the
    argument error, which is what proves the subcommand is still
    dispatched.
- `TestForcedRecord_NamesTheVoterTheRestartedDaemonWouldReport` and
  `TestForcedRecord_DoesNotOverwriteAPendingLease` cover the record: that
  it names the identity the restarted daemon would report for itself,
  that the chain reaches the hostname on a Comb whose configs name no
  `node_id` (the case a live multi-Comb colony actually produced, which
  no macOS test on a host whose `raftd.json` carries a `node_id` could
  have caught), that an existing pending record is never overwritten
  (it would strand a real lease, which has no TTL), that a corrupt one
  is left for an operator to look at rather than replaced, and that
  every failure to record is a warning rather than an error - a
  bookkeeping failure must never stop a restart someone needs during an
  incident. The exact JSON bytes and the 0600 mode are pinned too, since
  `internal/restartplan` already holds a test asserting this writer
  agrees with `internal/manager`'s on both.
- The target was then run for real on one Comb of a live multi-Comb
  colony - a non-leader voter, with the rest of the colony healthy - and
  the result is recorded in `.local/SHARED.md` rather than here, since
  that file is where live-cluster evidence belongs.
