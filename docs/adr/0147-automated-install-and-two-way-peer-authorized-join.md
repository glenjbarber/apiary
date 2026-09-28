# ADR-0147: Automated install, and a two-way peer-authorized Colony join

## Status

**Proposed.** Not implemented. No code in this repository reflects any
of it yet. Two things are being decided together here because they are
the same decision seen from two ends: a Comb that can configure itself
is a Comb that can be joined, and the join has to work without a
source checkout, exactly as the install does.

This ADR is written to be reviewed before implementation. The
"Open questions" section at the end lists the points where a decision
was made on the owner's behalf rather than by the owner, and each one
is cheap to change now and expensive later.

Amends ADR-0113 (removes `yes-trust-new-comb`). Touches ADR-0083,
ADR-0092, ADR-0096, ADR-0097, ADR-0100, ADR-0105, ADR-0111, ADR-0115,
ADR-0139, and ADR-0141.

## Context

Two separate pieces of this system still require a source checkout, and
the last one that was fixed in this way caused a long detour.

### `apiaryctl` had to be moved because a Comb has no checkout

`force-restart` was a Make target. It was moved to an installed binary
because an operation performed *because* a Comb is running has to be a
file on the Comb, and a Comb has no checkout to run a make target from
(ADR-0141). The lesson generalizes, and this ADR is the next place it
applies: **every per-host operation in this system must be reachable
from an installed binary.** Anything that is only reachable from a
checkout is not an operation, it is a build-time script.

### `apiaryinstall` is source-only, on purpose, and that is now the gap

`internal/install` is a real, tiered, tested preflight: ZFS pool and
base dataset, `vmm`/`nmdm` loaded, bhyve firmware and dnsmasq packages,
PF enabled with the `apiary/*` anchor, `/etc/rc.conf` permissions. It
runs as `apiaryinstall`, which the Makefile deliberately does **not**
install (ADR-0141's own record of that decision, and the `install`
target's comment).

That exclusion is correct and this ADR does not reverse it. Host
provisioning needs `pkg install`, needs a compiler for the bhyve
packages, and in practice happens once on a host that has just been
cloned. It is a source-checkout operation and it belongs there.

What is *not* correct is the shape of what comes after it. Everything
that turns a host with binaries into a working Comb is a Make target:

| today | where | what it does |
| --- | --- | --- |
| `setup-tls` | `Makefile` | ten-year self-signed RSA cert, CN=hostname, SANs `IP:127.0.0.1,DNS:<hostname>`, only if absent |
| `setup-quick` | `Makefile` | writes `raftd.json`, `managerd.json`, `frontend.json`, `restshimd.json` **unconditionally** |
| `NODE_RPC_ADDR` | `Makefile` var | `127.0.0.1:17700` by default, with a comment explaining that a multi-Comb colony must override it with a resolvable name |
| `node_id` | `$(hostname)` in a `printf` | written per-file, never into `common.json` |
| `peer_tls_hostname_map` | not written at all | left to ADR-0115's runtime derivation, which cannot help a Comb that has not joined yet |

Two properties of that table are the actual problem.

**It overwrites.** `setup-quick` writes all four config files on every
run, by its own comment, because "these carry real per-host settings
that only setup-quick itself knows how to regenerate correctly." On a
host with a hand-edited `managerd.json` - a corrected `uplink`, a real
certificate path, a `peer_tls_ca` - re-running it destroys the edit.
There is no confirmation, no diff, and no refusal.

**It is prose plus a `printf`.** Every rule about which address is
correct, when loopback is correct, and what the certificate must cover
lives in a comment block, because the tool that writes the config has no
opinion and cannot be tested. ADR-0139 is a whole ADR about a value that
a `printf` put in `rpc_addr` and that no test could catch.

The owner has asked for all of this automated, with the security
anchored on a numeric code.

### The join flow's authorization is a public constant

`ApproveJoinRequest` requires `confirm_phrase == "yes-trust-new-comb"`
(ADR-0113). That string is in this repository, which is public. It is
in `api/rpc/manager.proto`, in `internal/manager/joincolony.go`, in
`web/templates/cluster_overview.html`, and in the ADR you are reading.
It is not a secret and never was intended to be, exactly like the
six-digit `code` beside it, which ADR-0083 documents as "a value in
plaintext, not a secret, whose job is human visual correlation."

The consequence is that the *only* thing standing between an
unauthenticated injected request and a new raft voter is an Admin typing
a string they could have read in the source. The code comparison is
advisory. The fingerprint is displayed but never checked against
anything.

### And the joiner has to name the leader

`RequestJoinColony` takes a `target_address` naming "an existing Colony
member's own managerd address." In practice operators reach for the
leader, because the leadership check inside `ApproveJoinRequest` is
visible in the code and the instinct is to aim at the one that will
accept the write. The leader is a moving target: ADR-0145 records term
advancing from 26 to 30 mid-sweep, and a joiner that named the leader
finds its request rejected with a `leader_hint` at an arbitrary moment.

There is no good reason for this. The request record is raft-replicated,
so every member shows it; the `AddVoter` has to happen on the leader, but
the leader can be found at approve time, and the code already forwards.

## Decision

Two decisions, plus the boundary between them.

---

## Part 1: `apiaryctl install`

A new subcommand of the installed binary. It generates configuration,
certificates, and identity for one Comb. It needs only a root shell and
no checkout.

```
apiaryctl install                                   # report only; changes nothing
apiaryctl install --apply                           # perform RiskSafe writes
apiaryctl install --apply --colony-member host:port # a member exists; join, don't stand alone
apiaryctl install --apply-network yes-modify-network
```

The vocabulary is `apiaryinstall`'s own, deliberately, so the two tools
read as the same family. They are not the same tool and their blast
radii do not overlap:

| | `apiaryinstall` (source-only) | `apiaryctl install` (installed) |
| --- | --- | --- |
| touches | `/etc/rc.conf`, PF, ZFS, kernel modules, packages, bridge topology | `/usr/local/etc/apiary/*.json`, `/usr/local/etc/apiary-tls/*` |
| needs | a checkout, `pkg`, sometimes a compiler | a root shell |
| runs | once, before binaries exist | any time, on a running Comb |

`make setup-quick` keeps running both, in that order, and is the
documented fresh-host path. What changes is that the second half is now
a real, tested program rather than seven `printf` statements.

### Report-only by default

No writes without `--apply`. The report names every file that would be
created, every field that would be filled, every field that is being
left alone **and why**, and every value that could not be derived and
needs a human. This is the same posture `internal/install` already takes
and the same reason: the interesting output of an installer is what it
decided *not* to do.

### The preservation rules

These are the load-bearing part. An installer that overwrites is worse
than no installer, because it converts "I have not configured this" into
"I have configured this wrongly and did not tell you."

1. **Never write to a file that exists and does not parse.** Print the
   path and the parse error. That file may be a hand edit, a partially
   written file from an interrupted run, or something a human is
   mid-way through changing. Refusing is the only safe answer.
2. **Fill absent fields; never replace present ones.** Read through the
   existing `internal/nodeconfig`, `internal/raftdconfig`,
   `internal/frontendconfig`, `internal/restshimdconfig`, and
   `internal/commonconfig` `Manager` types, so the field set and the
   validation are the same code the daemons themselves use rather than a
   second list that can drift.
3. **Never change a `node_id` that is already set.** A different
   `node_id` is a different raft member. Writing one orphans existing
   state and produces a node that can never win an election. If
   `common.json` and a daemon file disagree, report the disagreement and
   change nothing.
4. **Never generate half a TLS pair.** If exactly one of `cert.pem` and
   `key.pem` exists, refuse and say which one is missing. A half-pair is
   an incident to look at, not a gap to fill.
5. **Write through the config packages' own `Save`**, which is already
   atomic and 0600, rather than a `printf` and a `chmod`. A config file
   that is briefly world-readable while being rewritten is a real
   exposure, and the code that avoids it already exists.

### TLS

Generate in Go, not by shelling out to `openssl`. The Makefile's version
is a two-line recipe; in Go it becomes testable on macOS, which is the
only place these tests can run.

- **ECDSA P-256**, not RSA-2048. This is a change from what `setup-tls`
  produces, and it is deliberate: the key is smaller, generation is
  instant, and every consumer here is Go's `crypto/tls`, which has no
  preference.
- **3650 days**, unchanged. A shorter life would create an expiry cliff
  with no renewal path, and `internal/certmgr` already makes an expiry
  *visible* without renewing it. Shortening the life and not building
  renewal is strictly worse than leaving it.
- **SANs: `IP:127.0.0.1`, `DNS:<short hostname>`, `DNS:<FQDN>` when the
  host has one.** Explicitly **not** the LAN address. ADR-0139 is the
  ADR about what happens when a numeric LAN address ends up as a dial
  target against a certificate that does not carry it, and the reason it
  happened is that nobody wrote down that the LAN address must never be
  in a certificate this system relies on.
- Directory 0700, key 0600, certificate 0644.
- If a valid pair already exists, leave it, and report its SANs and its
  expiry so the operator can see what the rest of the tool is about to
  assume.

### Identity

`common.json` is the right home for `node_id` and `hostname`
(ADR-0111), and `setup-quick` does not write it at all today, which is
why every daemon carries its own copy of the same value.

Derivation order, first non-empty wins, and every step is reported:

1. an existing value in `common.json`, then in the daemon's own file;
2. the host's FQDN, if the certificate covers it;
3. the short hostname.

`hostname` follows the same order, falling back to the short hostname.

Then, for `managerd.json` and `raftd.json` specifically: a `node_id` is
written **only** where the file has none. It is never written over an
existing one, and a disagreement between the two files is reported and
left alone.

### Address selection

This is ADR-0139 turned from a comment into code.

**Standalone** (no `--colony-member`, no non-loopback `raft_bind`, no
membership evidence) writes `rpc_addr: "127.0.0.1:17700"`. That is
correct for a one-Comb Apiary: every daemon that dials managerd is on the
same host, the certificate carries `IP:127.0.0.1`, and loopback is the
value `cmd/managerd` would have used with no config file at all.

**Multi-Comb** (`--colony-member` given, or `raftd.json` already carries
a non-loopback `raft_bind`, or the certificate carries a DNS SAN for a
name that is not this host's short name) writes
`rpc_addr: "<name>:17700"` where `<name>` is chosen **from the
certificate's own DNS SANs**. The tool never invents a name. If the
certificate carries no usable name, the install **refuses** and prints
the SANs it found, because writing an address the certificate does not
cover reproduces the ADR-0139 incident exactly, and the whole point is
that it can no longer happen by accident.

The rest of the address block, unchanged and for the same reasons the
Makefile gives today:

- `frontend.json` / `restshimd.json` `manager_addr` is always
  `127.0.0.1:17700` - it is a local dial, and ADR-0139's whole subject
  is a bind address being carried into a dial field.
- `frontend.json` `http_addr` stays `0.0.0.0:8080`. The browser is not
  on the Comb.
- `restshimd.json` `http_addr` stays `127.0.0.1:8081`. It is a full
  read/write control API with no authentication of its own.

### `peer_tls_hostname_map`

ADR-0115 already derives this from raft membership at runtime, refreshed
periodically, with manual entries taking precedence. Duplicating that
derivation at install time would produce a second list that goes stale
the moment membership changes, and ADR-0115's own reasoning is that
raft's committed configuration is the only place a managerd knows every
other voter's identity.

So `apiaryctl install` **does not write a derived map**, and says so in
its report. Two exceptions, both narrow:

1. **It verifies.** If it can see the members it is joining, it checks
   each one's raft-bind hostname against the local certificate's SANs
   and reports a gap by name.
2. **It seeds the bootstrap window.** ADR-0115 cannot help a Comb that
   has not joined yet, because it has no membership to derive from, and
   the first peer it ever dials is the one it is joining. That is the
   join flow in Part 2, and the seeding belongs there.

---

## Part 2: A two-way, peer-authorized Colony join

Replace the fixed phrase and the advisory code with a handshake in which
each side's operator must read the other side's page, in opposite
directions.

### The property being bought

> An attacker who can reach `RequestJoinColony` unauthenticated, and who
> is not the Comb the request names, cannot get a request past stage one,
> because stage one requires a value that is only displayed on the
> requesting Comb's own page, and stage two requires a value that is only
> displayed there in response to one the target generated.

That is the whole claim. It replaces "an Admin types a string published
in a public repository" with a requirement that the attacker control a
machine the target's operator can reach, and be able to answer a
challenge on it.

### The flow

```
REQUESTING COMB                          TARGET COLONY
────────────────────────────────────────────────────────────────
operator submits join request
  generates FIRST CODE locally
  reads its own certificate
  fingerprints
  dials ANY member (not the leader)
                                         record replicated
                                         ┌─ waiting: paste first code
                                         │  + fingerprints
operator reads first code and
  fingerprints on THIS page
  and pastes both into the
  target's page ────────────────────────▶
                                         verify both
                                         generate SECOND PIN
                                         store it, forget the first
operator's page now displays
  the SECOND PIN (via its
  existing status poll) ◀───────────────
operator pastes the second
  PIN into the target's page ──────────▶
                                         consume it
                                         reachability + join-log
                                           guardrails, unchanged
                                         AddVoter
```

### The first code is the requester's, not the target's

This is the inversion that makes the flow work. Today the target
generates the code and both sides display it, so the code is a thing
the two operators compare by eye. Under this ADR the **requester**
generates it, before dialing, and the target checks the pasted value
against what the request carried.

That changes what the code proves. It is no longer "two people are
looking at the same six digits." It is "the operator at the target has
just read this value off the requesting Comb's own page." The
comparison is against the machine, not against another display of the
same server's state.

The joiner generates it with `crypto/rand` over 10^6 and renders it in
6 digits with a copy button, because it is now a value a human
transcribes. **Six digits, unchanged** - it is a correlation value whose
strength comes from the transcription, and a longer string makes the
transcription worse. The second PIN is the one that is
authorization-bearing, and gets 8 (below).

### The fingerprints are the identity check, and it is now fail-closed

The joiner advertises the SHA-256 fingerprint of every certificate it
actually holds: its `tls_cert` always, and its `raft_tls_cert` when Raft
transport TLS is configured. These are read from the certificate files
themselves, not from the config strings that name them.

The target's operator pastes them into the target page alongside the
first code, and the target compares them against what the request
carried. A mismatch is a refusal with both values shown.

**A request carrying no fingerprints cannot be approved.** Today the UI
renders an explicit "no TLS certificate presented" fallback and
approval proceeds. That fallback is the defect ADR-0113 identified and
did not fix, and it is removed here: with a two-way flow, the
fingerprint comparison is the only thing binding a request to a machine,
and a request with nothing to compare is exactly the request this flow
exists to refuse. The installer in Part 1 is what makes this
comfortable - every Comb now has a certificate, so "no fingerprint" is
a real fault rather than a normal state.

### The second PIN, and what it is for

The second PIN is generated by the target, shown **only** on the
requesting Comb's page, and must be pasted back into the target. It is
not rendered on the target's own page. An operator who can see it
everywhere has no reason to perform the comparison, and a target Admin
who can read it can complete the flow alone, which would collapse the
handshake back into one party.

It is delivered over the joiner's existing `GetJoinRequestStatus` poll,
which is already unauthenticated and already keyed on `request_id` (64
bits of `crypto/rand`, unguessable). **The target never dials out to
deliver it.** `RequestJoinColony` is unauthenticated and its
`target_address` is caller-supplied, so a target that dialed back would
be an unauthenticated peer dialling an address an unauthenticated caller
chose. The poll already exists, is already in the requester's hands, and
adds no new outbound surface.

Length: **8 digits.** The first code is a correlation value and six is
right for it. The second PIN is the value whose guessing an attacker
would want, and it gets more entropy for free since it is copy-pasted
rather than read aloud.

### State, durability, and the expiry that now matters

`PendingJoinRequest` gains an explicit stage, so the two numeric values
are not inferred from a status field:

| stage | meaning | terminal |
| --- | --- | --- |
| `JOIN_REQUEST_STAGE_INTRODUCED` | recorded; the target is waiting for the first code and fingerprints | no |
| `JOIN_REQUEST_STAGE_CODE_VERIFIED` | the first code and fingerprints were accepted; the second PIN exists and is waiting to be polled by the requester | no |
| `JOIN_REQUEST_STAGE_AUTHORIZED` | the second PIN was accepted and `AddVoter` succeeded | yes |

`status` keeps its existing meaning - the operator's decision
(pending / approved / rejected / cancelled) - and gains one value,
`JOIN_REQUEST_STATUS_FAILED`, for a request killed by too many failed
authorization attempts. A stage and a status are different axes: a
request can be stage `CODE_VERIFIED` and still be pending, which is
exactly the window where the operator is copying a PIN between two
machines.

Everything lives in raft, not in managerd memory. That is not a
preference, it is what makes the rest work: **leadership can change at
any point in this flow and the handshake continues**, because the new
leader holds the same record, the second PIN is in it, and the requester
is polling by `request_id` rather than by leader. The stage, the
attempt counters, and the consumed flag are all replicated for the same
reason.

`pendingJoinRequestExpired` gains a second TTL for stage two. The
existing 15-minute `defaultJoinRequestTTL` bounds the whole request. The
second PIN gets a shorter one - **5 minutes** - because it is the value
that actually authorizes something, and it only has to survive one
copy-and-paste. Expiry is checked lazily at read and approve time
against the wall clock, never inside the FSM, which must stay
deterministic across replicas and log replays.

### Attempt limits, comparison, and replay

- **5 attempts per stage.** The counter is replicated, so it survives a
  leader change and a managerd restart, and two operators cannot each
  get five guesses by racing across a failover.
- **Exhaustion kills the request.** `status` becomes `FAILED`, terminal,
  and the record must be purged and the join re-issued. It does not
  become "pending again with a fresh counter," because a request that
  has attracted five wrong codes is a request something is guessing at.
- **Constant-time comparison** on the hash of each value, via
  `subtle.ConstantTimeCompare`.
- **The first code is cleared** once it has been accepted. A value that
  has been spent is not left in replicated state for a later reader to
  find.
- **The second PIN is cleared** the moment `ApproveJoinRequest` consumes
  it, before `AddVoter` is called. A failed `AddVoter` does not put it
  back: the operator re-arms with an explicit reissue, which is visible
  and rate-limited to 3.
- **Replay is refused by construction.** Both values are bound to one
  `request_id`, single-use, and expiring. A captured form post is worth
  nothing after the request is approved, rejected, failed, expired, or
  purged, and all five of those are terminal.

### Leader forwarding

The joiner dials **any** member, and the target page is whichever member
it dialed.

- `RequestJoinColony` is already forwarded to the leader when the
  receiving member is not the leader, via the `leader_hint` on a failed
  Apply. That path is kept and its error message is fixed: a joiner that
  named a follower and was forwarded must not see anything that reads
  like a rejection.
- `VerifyJoinIntroduction`, `ReissueJoinSecondPin`, and
  `ApproveJoinRequest` are all state-changing and all forward the same
  way, using the leader's *managerd* address derived from
  `currentLeaderRaftAddress`. The forwarding is authenticated
  (`s.peers`, with the API key attached) - these are Admin operations,
  unlike `RequestJoinColony`, and must never use the unauthenticated
  dial.
- The forward carries the whole request, including the first code, so
  the leader checks exactly the values the operator pasted.
- The receiving member does not become special. It forwards, it returns
  the leader's response verbatim, and it shows the request in its own UI
  because the record is replicated and every member reads it.

One thing changes and it matters: **`tls_cert_fingerprint` and the new
fingerprint list are filled in from the joiner's own certificate before
any forwarding**, on every path, including the path where the request is
recorded locally rather than forwarded. Today the local-record path
reads the caller's field directly and the fingerprint is only filled in
on the forward path. Under this ADR a locally-recorded request must
carry the joiner's real fingerprints, or the operator is comparing
against nothing.

### `yes-trust-new-comb` is removed

The field is `reserved` in the proto rather than deleted, and
`second_pin` takes a new field number. Every call site, template, and
test that used the phrase is updated. ADR-0113 is amended with a dated
section saying what replaced it and why, in the same style as
ADR-0141's own amendments.

What stays: `ConvertStandaloneToJoiner`'s `yes-convert-to-joiner`
(ADR-0105) and `raftd`'s `-reset` phrase. Both guard *local, destructive,
single-machine* actions - wiping this Comb's raft state. The join phrase
guarded a *membership* change and is the only one of the three whose
value was published to strangers. The distinction is the whole basis for
removing one and keeping two, and it is worth writing down so a later
reader does not "consistency-fix" the other two.

### New and changed RPCs

| RPC | change |
| --- | --- |
| `RequestJoinColonyRequest` | gains `introduction_code` and `advertised_fingerprints` |
| `VerifyJoinIntroduction` | new. Admin. `{request_id, introduction_code, fingerprints}` → new stage |
| `ReissueJoinSecondPin` | new. Admin. Regenerates, invalidating the previous PIN, max 3 |
| `ApproveJoinRequestRequest` | `confirm_phrase` reserved; gains `second_pin` |
| `GetJoinRequestStatusResponse` | carries the second PIN, only in stage `CODE_VERIFIED`, only to the holder of the `request_id` |
| `GetLocalJoinIdentity` | new. Admin on the *requesting* Comb. Its own `node_id`, `raft_bind`, first code, fingerprints, stage, and second PIN when it has one |
| `internalpb.PendingJoinRequest` | gains `stage`, `advertised_fingerprints`, `second_pin`, `first_code_attempts`, `second_pin_attempts`, `second_pin_expires_at_unix` |
| `internalpb.Command` | gains `VerifyJoinIntroduction` and `ReissueJoinSecondPin` commands, so both are raft-replicated like every other join transition |

`internalpb/state.proto` change means a state-format concern: new fields
on a raft-replicated message, and the canonical state digest (ADR-0143)
covers them. `internal/statedigest` sorts by field number, so this is
mechanical, and the digest changes on every Comb when the new fields
first appear. That is expected and will read as a digest mismatch during
a mixed-version rollout, which is the same thing that already happens
when any field is added.

### The two pages

**Requesting Comb's own Machine page.** The existing "Join a Colony"
panel becomes a four-step display driven by `GetLocalJoinIdentity`: the
first code with a copy button, the fingerprints with a copy button, the
stage, and - when it exists - the second PIN with a copy button, under
an explicit "copy this into the target Colony's page" instruction. The
page never submits anything to the target on the operator's behalf.
That is the property the design is protecting, and a page that
auto-submitted would destroy it while looking more automated.

**Target Colony's landing page.** The pending-requests panel gains a
first-code-and-fingerprints form per request, then a second-PIN form,
then the existing preflight and Approve. The second PIN is never
rendered there. The `Preflight approval` button stays exactly where it
is and gains a stage-appropriate message.

---

## What this does not claim

Being explicit, because the honest version is more useful than the
impressive one.

- **It does not constrain a hostile Admin of the target Colony.** Such an
  Admin can read the replicated second PIN and approve alone, or skip the
  flow and call `AddVoter` through any other path. The flow raises the
  cost of *unattended, uninformed, or injected* approval. It is not an
  authorization boundary against someone who already holds the authority
  it authorizes.
- **It does not survive an attacker who controls the requesting
  managerd.** If the requesting side is hostile end to end, it displays
  whatever it likes. The fingerprint comparison catches a *third* party
  injecting a request, not the requester lying about itself - which is
  what the join-log guardrail and quorum visibility are for.
- **It is not cryptographic authentication of the requester.** A human
  comparing a fingerprint across two machines is a real check against a
  real class of mistake, and it is still a human check. Nothing here
  replaces the internal-token work that is separately deferred.
- **It does not make the second PIN secret from the target's own
  Admins.** The record is replicated and the Admins are its readers. The
  justification is that this audience is exactly the set of principals
  who could already approve the request outright, so replication widens
  nobody's authority - but the value is not a secret from them, and no
  part of this ADR should be read as claiming it is.

## Rejected alternatives

**Keep `yes-trust-new-comb` alongside the PIN.** The owner's own
objection, recorded because it is the right one and the reasoning is
worth keeping: a fixed phrase in a public repository is not a
confirmation, it is a constant. Adding a second factor next to it does
not remove it, it just makes the weak factor optional-looking.

**Generate both codes on the target.** Simpler, and it would have kept
`RequestJoinColony` unchanged. It is also a one-way flow: the requester
would have no value of its own to display, so the operator at the target
would still be pasting something they read from the *target*, and the
whole binding to the requesting machine disappears.

**Have the target dial the requester to push the second PIN.** Push
looks better than poll. It means an unauthenticated peer dialing an
address an unauthenticated caller supplied, which is a worse version of
the reachability-oracle finding in the 2026-09-12 audit. The poll
already exists.

**Put the second PIN in a managerd in-memory field rather than raft.**
Then a leader change loses it and the operator has to start over. Raft
is what makes the flow survive the thing the owner specifically asked it
not to depend on.

**Re-issue a new first code on every retry instead of limiting attempts.**
A retry that resets the counter is a free unlimited guess. Five, then
`FAILED`.

**Hash the values at rest.** Considered, and it does not buy anything
here: the audience for the replicated record is the same audience that
could approve outright, so confidentiality of the record buys no
additional protection, and 10^8 with five attempts is not the boundary
being defended. The boundary is the transcription.

**Make `apiaryctl install` also do the host provisioning and delete
`apiaryinstall`.** Tempting, and wrong in the direction that matters:
host provisioning genuinely needs `pkg` and a checkout, so folding it in
would mean the *only* installer needs a checkout, which is the property
this whole ADR is removing.

**Write a full `peer_tls_hostname_map` at install time.** Duplicates
ADR-0115's runtime derivation and produces a list that goes stale the
moment membership changes.

**Keep the "no TLS certificate presented" fallback for compatibility.**
Compatibility with what? A Comb with no certificate is a Comb whose
serving identity was never verifiable, and Part 1 makes sure that is no
longer a normal state.

## Scope boundary

This ADR does **not**:

- touch the internal raftd/managerd token project, which stays deferred
  until a coordinated security effort covers secret matching, fail-closed
  mismatch behavior, rollout compatibility, startup validation, no
  leakage, tests, and rotation;
- build a FreeBSD port or package. Binary delivery to a fresh host stays
  exactly as it is today - a checkout, or whatever the owner does;
- remove or deprecate `apiaryinstall`, or move any `/etc`, PF, ZFS,
  kernel-module, package, or network-topology work into `apiaryctl`;
- change the `update` / `apiaryctl force-restart` split from ADR-0141, or
  add anything to `make update`;
- automate anything about `raftd -reset`, the restart guardrail
  (ADR-0103), or the controlled update (ADR-0145, ADR-0146);
- distribute `peer_tls_ca` certificates between Combs. That is a real
  remaining manual step in `docs/add-node-to-colony.md` and it is a
  separate problem - it is a trust-establishment question, not a
  configuration-generation one.

## Relationship to existing decisions

- **ADR-0083** - the join flow and the six-digit code. This ADR keeps
  the flow and inverts who generates the code, which changes what the
  code proves.
- **ADR-0092** - unauthenticated `target_address` forwarding. Kept, and
  extended to the new admin-side RPCs with the authenticated dial.
- **ADR-0096** - never attach the node's own credentials to an
  unauthenticated dial. Unchanged, and now also the reason the second PIN
  is polled rather than pushed.
- **ADR-0097** - the reachability preflight. Unchanged and still the
  last thing before `AddVoter`.
- **ADR-0100** - config files, not flags. This ADR writes those files
  from a program for the first time.
- **ADR-0105** - `ConvertStandaloneToJoiner`. Keeps its own phrase; its
  join submission becomes the same first-code flow.
- **ADR-0111** - `common.json`. Becomes the home `setup-quick` never gave
  it.
- **ADR-0115** - automatic `peer_tls_hostname_map`. Not duplicated. This
  ADR fills the one window it cannot cover: before a Comb has joined, it
  has no membership to derive from.
- **ADR-0139** - `rpc_addr` is a dial target too. This ADR turns its
  comment block into the code that enforces it.
- **ADR-0141** - the installed-binary rule. This ADR is that rule applied
  to the installer, for the same reason and with the same argument.
- **ADR-0143** - canonical state digest. New replicated fields change the
  digest; expected during rollout.
- **ADR-0113** - amended. `yes-trust-new-comb` is removed.

## Verification

Nothing below is claimed as done. This is what implementation would have
to earn.

**Part 1, in Go, runnable on macOS:**

- `internal/hostinstall` (new) holds the plan/apply logic with a
  `Host`-shaped seam, tested against a fixture directory. The cases that
  matter: a file that exists and does not parse is refused with its
  path; a file that exists and parses has its present fields preserved
  byte for byte; a `node_id` is never overwritten; a disagreement
  between `common.json` and a daemon file is reported and changes
  nothing; `cert.pem` without `key.pem` is refused; a valid existing
  pair is not rewritten and its expiry and SANs are reported; standalone
  install writes `127.0.0.1:17700`; `--colony-member` writes a name
  taken from the certificate's SANs; a certificate with no usable name
  is refused with the SANs printed; a generated certificate's SANs are
  asserted to contain `IP:127.0.0.1` and both hostnames and to **not**
  contain the host's LAN address; every written file is 0600 and every
  created directory is 0700; a second run over its own output changes
  nothing at all.
- Generation is checked by parsing what was written: `x509.ParseCertificate`
  on the PEM, an assertion on `DNSNames` and `IPAddresses`, an assertion
  that the public key matches the private key file, and an assertion on
  `NotAfter`.
- No secret is printed: the report for a host with a real certificate
  prints SANs and expiry, never key material, and a test asserts the key
  PEM's body does not appear in the report.

**Part 2, end to end, against real Raft:**

- The full happy path, with a real second Raft node, asserting the
  actual membership change - the same shape as the existing
  `internal/manager/joinlogguard_test.go` allow-path test.
- Introduction to a **non-leader** member, asserting the record is
  replicated, the leader's copy is what authorizes, and `AddVoter` runs
  once.
- A leader change between `CODE_VERIFIED` and approval, asserting the
  second PIN survives and approval still succeeds.
- Wrong first code, wrong fingerprint, wrong second PIN, each refused
  with a message that names what did not match, and each incrementing the
  replicated counter.
- Five wrong attempts, then `FAILED`, then a re-issue is required.
- Expiry of the second PIN at 5 minutes while the request itself is
  still inside its 15.
- Replay: a captured approve form post after the request is approved
  changes nothing.
- The target never dialing out: a test that fails if any outbound dial
  happens during the handshake.
- A request with no advertised fingerprints is refused at stage one, and
  the old "no TLS certificate presented" text is gone from the template.
- The first code is cleared from state once accepted; the second PIN is
  cleared once consumed.
- Comparison is constant-time, asserted by a test that the comparison
  helper is `subtle.ConstantTimeCompare` and not `==`.

**Both parts, then mutation testing** under the rules already used in
this repository: every mutant applies exactly once, changes bytes, fails
on an **assertion** rather than a compile error, and is restored
byte for byte. A mutant that dies at build time is rejected as evidence
and replaced, as was done for the two build-failure kills in the
force-restart work.

## Open questions for the owner

Six points where a decision was made on your behalf. Each is a small
edit here and a much larger one in code.

1. **ECDSA P-256 instead of RSA-2048** for generated serving
   certificates. Every consumer here is Go, and it is a visible change
   to anyone comparing an old and a new certificate. Keep RSA for
   familiarity?
2. **Eight digits for the second PIN, six for the first.** The first is
   a correlation value and six is right for reading aloud; the second is
   the authorization value and is copy-pasted, so longer is free. Both
   the same number?
3. **Five minutes for the second PIN**, against the request's existing
   fifteen. Long enough for a careful copy between two machines, short
   enough that a PIN left on a screen stops being useful.
4. **A request with no fingerprints is refused.** TLS stays opt-in today
   and this makes a Comb with no serving certificate unable to join at
   all until the installer gives it one. Correct, but it is a real
   compatibility change for any host that skipped TLS.
5. **Reissuing the second PIN is allowed, three times.** The alternative
   is that a mistyped PIN kills the request outright, which is safer and
   much more annoying.
6. **The second PIN is stored in plaintext in the replicated record.**
   Justified above by "the audience is already authorized", but it is a
   plaintext authorization value in raft state, and if that is
   uncomfortable the alternative is a short-lived managerd-local copy
   with an explicit reissue after any leader change - which costs the
   property the owner asked for, that the flow survives a leader moving.
