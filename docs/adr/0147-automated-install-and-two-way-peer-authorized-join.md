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
is cheap to change now and expensive later. Questions 4, 7, 8, 12, and
13 are answered and each answer is recorded next to its question.
**1, 2, 3, 5, and 6 are still open**, and they are the ones to push
back on: the certificate algorithm, the two code lengths, the
second-PIN lifetime, the reissue policy, and whether an authorization
value may sit in plaintext in replicated state.

It has four parts, decided together. Part 1 is the source-free
`apiaryctl install`. Part 2 is the two-way, peer-authorized join that
replaces `yes-trust-new-comb`. Part 3 is the root-owned
authorization-file mechanism that answers a hostile Colony Admin, which
the owner confirmed is in scope; Part 2 alone does not stop that
adversary and says so. Part 4 makes TLS the default on every Comb
including a single-node one, and adds the Colony-side
`await_colony_join` window that supplies the pre-join trust anchor and
gates the flow.

Three decisions in Part 4 are the owner's, not mine, and are recorded
as decisions rather than proposals: TLS is on by default everywhere;
the Colony side of the join is a deliberate operator toggle with a
predictable expiry; and **approval requires a live window**, not merely
request creation. The last is the stricter of the two options on the
table and was chosen for that reason. The two points I had left open in
Part 4 are answered too: the window length is configurable and defaults
to ten minutes, and the window publishes every member's managerd
fingerprint.

Amends ADR-0113 (removes `yes-trust-new-comb`). Touches ADR-0083,
ADR-0092, ADR-0093, ADR-0096, ADR-0097, ADR-0100, ADR-0105, ADR-0112,
ADR-0115, ADR-0139, and ADR-0141.

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

Four decisions, plus the boundary between them. Parts 1 to 3 are what
was drafted first; Part 4 is the owner's later set, recorded as
decided rather than proposed because the reasoning behind each one is
not in doubt.

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
(ADR-0112), and `setup-quick` does not write it at all today, which is
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

`PendingJoinRequest` also records `window_epoch`, the `opened_at_unix`
of the Colony join window that was live when the request was created
(Part 4). It is not a TTL and it is not a stage; it is the request's
claim on one specific window, and approval checks that the live window
is that window. It is the field that stops a request being parked
through an expiry and finished during the next open window.

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
| `PendingJoinRequest` (Part 3) | gains `authorization_id`, `consumed_at_unix`, and the two approving API key IDs, so consumption and authorship are both in the raft log rather than only in a file |
| `OpenColonyJoinWindow` | Part 4. new. Admin. `{duration_seconds, 0 means configured default}` → the absolute deadline, the applied duration, and the epoch. Leader-only, forwarded like every other admin-side state change |
| `CloseColonyJoinWindow` | Part 4. new. Admin. Ends the window early; expiry converges on the same state |
| `GetColonyJoinWindowResponse` | Part 4. new. Unauthenticated, **only while the window is live**, and returns the public bootstrap fields and nothing else: `colony_name`, **every member's** managerd fingerprint, `opened_at_unix`, `expires_at_unix`, `colony_node_id`, `leader_node_id` |
| `PinPeerCertificate` | Part 4. new. Admin. The target's half of the trust step: evaluate the joiner's leaf, then record the pin. Called automatically by the introduction path, never by a page |
| `UnpinPeerCertificate` | Part 4. new. Admin. Drops a pin held for a request that ended without becoming a voter |
| `internalpb.ColonyJoinWindow` | Part 4. new. The replicated window, with an absolute `expires_at_unix` and `opened_at_unix` doubling as its epoch |
| `internalpb.TrustedPeer` | Part 4. new. The replicated trust store, leaf PEM included |
| `PendingJoinRequest` (Part 4) | gains `window_epoch`, so a request belongs to one window and cannot be finished in the next |
| `internalpb.Command` (Part 4) | gains `OpenColonyJoinWindow`, `CloseColonyJoinWindow`, `PinPeerCertificate`, and `UnpinPeerCertificate` arms |

**Two of these RPCs are deliberately unauthenticated**, and it is worth
being explicit about why, because the instinct on reading the table
will be to authenticate them. `RequestJoinColony` and
`GetColonyJoinWindow` are reachable by a Comb that has no Colony
credentials yet, which is the entire situation they exist to serve.
`GetColonyJoinWindow` is safe to expose because while the window is
live it is publishing what the operator has already decided to
publish, and when the window is not live it returns nothing. The
security of the join does not rest on either RPC; it rests on what the
leader refuses to do without the operator's authorization file.

**The same negative applies to `PinPeerCertificate` as to the
authorization file, in weaker form.** A pin is written by an RPC, so an
Admin can add one, and that is why the trust store is documented as a
mechanism for honest operation rather than as a boundary. It must never
be treated as a substitute for Part 3.

**No RPC is added that can create or modify an authorization entry.**
That is the load-bearing negative of Part 3, and it is stated here
because the RPC table is where a later implementer would add one by
reflex. The only writer is the root-only installed `apiaryctl` binary,
acting on a local file. Any future RPC that touches this file is a
security regression even if it is Admin-tier, and should be treated as
one in review.

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

**Part 3, on the same page.** The panel reports, per pending request,
whether a matching authorization entry exists on this Comb, and the
preflight lists both remaining gates: the authorization entry and the
second distinct API key. It never offers a way to create an entry, and
it never renders the entry list, because the page is served by a Comb a
hostile Admin controls. What the operator is told is *that* a gate is
unmet and what would satisfy it, never the contents of anything that
would satisfy it.

## Part 3: The authorization only the operator can give

Part 2 as written stops unattended, uninformed, and injected approval.
It does not stop a hostile Admin of the target Colony, and the owner
confirmed that adversary is in scope. This part closes it, using the one
authority that separates the operator from that Admin.

### The boundary is file ownership, not a secret

An Admin-tier caller can already do a great deal. `UpdateNodeConfig`,
`UpdateFrontendConfig`, and `UpdateRestshimdConfig` write files as root
(`internal/nodeconfig/manager.go:336`); `RestartNodeService` restarts
services from a fixed allowlist and refuses managerd self-restart
(`internal/manager/server.go:3625`, `internal/manager/services.go:115`);
`ConvertStandaloneToJoiner` can reach `raftd -reset`
(`internal/manager/raftdservice.go:75`).

What an Admin-tier caller cannot do is read or write an arbitrary
root-owned file, or run an arbitrary command. That gap is the entire
basis of this part. It is why the mechanism is a file and not a key: a
file the Admin cannot write is an authority they cannot assume, and it
needs no secret to protect, no rotation story, and no new cryptography.

### The file

`/usr/local/etc/apiary/join-authorizations.json`, root-owned, mode 0600,
holding a list of single-use entries:

```
{ "authorizations": [
    { "id": "auth-...", "node_id": "comb-3",
      "fingerprint": "sha256:ab12...", "expires_at_unix": 1789... }
] }
```

Deliberately **not** one of the config files. No RPC writes it, and it
is never a field of any proto, so the three `Update*Config` handlers
cannot reach it. `apiaryctl install` creates it empty, 0600, alongside
the other config files, so a fresh Comb has the store before it needs
it.

The operator creates an entry by reading the fingerprint on the
requester's own page and running, on a Comb:

```
apiaryctl join-authorize --node-id comb-3 --fingerprint sha256:ab12...
```

which prints the pending request it matched against, so the deliberate
comparison still happens in front of the operator, and then writes the
entry atomically. `apiaryctl` is already the established root-only
installed command and needs no checkout (it lives in
`/usr/local/libexec/apiary/`), so this adds a subcommand, not a new
tool.

### What approval now requires

All three, and the first is the one that matters:

1. A **matching, unconsumed, unexpired entry** in the file, for that
   `node_id` and that exact fingerprint. No entry means no voter,
   whatever API key is presented.
2. **Two distinct API keys, from two distinct Combs.** Both are
   recorded on the request, so the Colony's own history says which keys
   authorized which join. This is the same idea as a two-person rule
   and it costs the operator nothing.
3. Everything Part 2 already requires: the first code, the fingerprint
   match, the second PIN, reachability, the join-log guardrail, and the
   existing not-already-a-voter check.

And, ahead of all of them, Part 4's check that the Colony's join window
is live and is the same window the request was created under. The order
is deliberate: the window is the cheapest check, it is the one that
explains the refusal the operator will most often hit, and a request
that is dead on that ground should not also consume a second-PIN
attempt.

A refusal for a missing entry names the entry that is needed and which
Comb to create it on, because "refusing" with no next step is how an
operator ends up disabling the check.

### Single use is replicated, not just local

Only the leader calls `AddVoter`, so only the leader reads the file. If
leadership moves, the new leader's copy of the file is what is checked,
and a stale copy elsewhere in the Colony must not be able to authorize
the same request a second time.

So consumption is recorded in replicated state, not only in the file:
`PendingJoinRequest` gains `authorization_id` and `consumed_at_unix`,
written by the same FSM command that approves. A second Comb holding an
unconsumed copy of the same entry is refused because the replicated
request already records the consumption. The file is the *input*; the
raft log is the *record of use*.

This has an operational consequence worth stating plainly: the file
must exist on every Comb, and an entry must be created on whichever
Comb is currently the leader. That is why the refusal message names it
rather than failing anonymously.

### How the layers compose, and what each one alone misses

- **The two-way PIN exchange alone** stops an uninformed or unattended
  approval and a mis-paste. It does not stop an Admin who reads the PIN
  off the page, or who runs the whole exchange themselves.
- **The distinct-key rule alone** stops an Admin holding exactly one
  key. It does not stop one holding two, and it never involves the
  operator.
- **The authorization file alone** is the only layer that requires the
  operator's deliberate act. It is the layer that closes the hole.

They are kept together because they fail differently, and because the
first two are free.

### What this still does not stop

- **Impersonation.** An Admin can still present themselves as the
  operator everywhere else in the UI. Nothing inside the Colony can fix
  that, because impersonating the operator to the Colony is something
  the Colony is itself asserting.
- **Denial.** An Admin can delete an entry, refuse to forward, or
  withhold a file. Denial is survivable and diagnosable, and it is the
  correct asymmetry: the operator can admit a Comb, the Admin cannot
  quietly admit one.
- **Root on any Comb.** That is Level 2 and it is out of scope. Root on
  the Comb holding an authorization file is root on the Colony's ability
  to admit new members, which is a Level 2 consequence landing on a
  Level 1 design, and is the reason this file is worth excluding from
  backups.

### Why nothing browser-based was used

Recorded because it is the obvious next suggestion and it does not work.
A non-extractable WebCrypto key in the operator's browser cannot be
extracted, but the page is served by the target, and script in that
origin can call `crypto.subtle.sign` with it. A confirm button on the
target's own page is decided by the target. The operator's existing
Admin key is readable through `ListAPIKeys` and mintable through
`CreateAPIKey`. Signing off-band on the operator's own machine was
proposed and **rejected by the owner**: nothing may live on an
administrator's local machine, and everything must be built into Apiary.
The file is what remains once that constraint is applied, and it is
enough because the operator's authority over a Comb is root, which is
exactly the authority an Admin key does not confer.

---

## Part 4: TLS by default, and the window that makes a join possible

The owner asked for a toggle on a managerd of an existing Colony that
sets that Colony joinable, with a predictable timeout. It is worth
being clear that this is more than a convenience switch, because the
design below leans on it for three separate jobs: it is the Colony's
answer to the unauthenticated request-creation surface, it is the only
place a pre-join trust anchor can exist, and it is what makes
`yes-trust-new-comb`'s replacement meaningful rather than ceremonial.

### TLS is on by default, including a single-node Colony

The owner's decision. It removes a compatibility path rather than
adding a default.

- **There is no longer a supported state in which a Comb has no usable
  serving certificate.** The "no TLS certificate presented" fallback
  in the join flow is removed, not deprecated. A Comb with no
  certificate cannot be joined, and Part 1 is what guarantees it has
  one.
- **A single-node Colony is a TLS Colony.** Standalone is not a
  plaintext mode. This costs nothing at one node and it is the case
  that would otherwise discover the gap at the worst moment, when a
  lone Comb is finally asked to join.
- **Existing certificates are preserved, not replaced.** A Comb with a
  working RSA-2048 pair from `setup-tls` keeps it, keeps serving it,
  and joins with it. ECDSA is what the installer *generates*, not what
  is *required*; the evaluation gate below accepts either.

This also answers open question 4, which was written while TLS was
still opt-in and therefore asked the wrong question.

### `await_colony_join` is the Colony side, and it is not `await_join`

Two flags with similar names and opposite meanings. The collision is
close enough to be worth a table.

| | `await_join` | `await_colony_join` |
| --- | --- | --- |
| Lives in | `raftd.json` on the Comb that is joining | managerd, as replicated Colony state |
| Direction | The joining Comb waits for someone | An existing Colony admits |
| Prevents | Self-bootstrapping onto an empty log | Nothing; it is the absence of the normal state |
| Default | `false` | `false` |
| Set by | the installer's join path, or `ConvertStandaloneToJoiner` | an operator, deliberately, on a member |
| Ends | when the join succeeds | at an absolute deadline fixed when it was opened |

The normal state of a Colony is **closed**. A request arriving at a
closed Colony is refused with a message naming the fix. That inverts
today's default, where `RequestJoinColony` is accepted at any moment by
any member.

### The window is Colony-wide and replicated

Replicated, because otherwise an election would decide whether a join
is possible at all. It is FSM state on the leader:

```proto
message ColonyJoinWindow {
  bool   enabled         = 1;
  string opened_by       = 2;  // node ID of the member it was opened on
  string opened_by_key   = 3;  // API key ID of the caller
  int64  opened_at_unix  = 4;  // also the window's epoch identity
  int64  expires_at_unix = 5;  // absolute, set once, never extended
}
```

with two command arms, `OPEN_COLONY_JOIN_WINDOW` and
`CLOSE_COLONY_JOIN_WINDOW`. Any member may be *asked* to open it; the
call is forwarded to the leader exactly like every other admin-side
state change, and only the leader mutates. A follower answers
`GetColonyJoinWindow` from its replicated copy, so introducing a Comb
to a follower behaves identically to introducing it to the leader.

### Opening it, and what predictable has to mean

`OpenColonyJoinWindow` takes an **optional** duration and is refused
outright while a live window already exists - there is no "or longer"
case, because "or longer" is the extend operation this section exists to
prevent. An operator who wants a different length changes the
configuration or closes the current window and opens a new one, which
are visible acts rather than silent ones.

**The duration is configurable, defaulting to 10 minutes.** The owner's
decision, and the reason it is configurable is worth stating: 10
minutes has to cover a two-human exchange across two machines, which is
comfortable when the exchange is deliberate and tight when it is not,
and the right length depends on the Colony and on the operator. A
Colony where the two machines sit on the same desk and one where the
operator has to walk somewhere are not the same number.

- `managerd.json` gains `colony_join_window_seconds`, absent meaning
  **600**. `apiaryctl install` writes it explicitly when it creates the
  file, so the operator can see the number, and leaves it alone when the
  file already exists, per Part 1's preservation rules.
- The configured value is both the **default and the ceiling**.
  `OpenColonyJoinWindow` may request anything shorter and nothing
  longer, and a longer request is refused with a message naming the
  configured ceiling and the file that holds it. Making the
  configuration the only lever is what stops the RPC from being a way
  around configured policy.
- **The leader's value is authoritative**, because the leader computes
  the deadline. Two Combs configured differently is a legitimate state
  and the answer is not "they disagree, refuse", it is "the leader's
  number wins and the response says which number was used". The
  response carries the applied duration so no operator has to guess.
- A **malformed or non-positive value is a startup error**, not a
  fallback to 600. A window whose length is silently wrong is a window
  nobody can reason about, and this is the same fail-closed rule
  Part 1 applies to the files it writes.

Three further properties, each closing a specific way a window becomes a
permanent capability:

- **The deadline is absolute and fixed once.** It is
  `opened_at + duration` computed on the leader and replicated. A
  leader change does not renew it, a managerd restart does not renew
  it, and there is no extend operation. A member that wants longer
  opens a new window, which is a second deliberate act and shows up as
  a second act in the log.
- **Closing early is allowed**, because an operator who changes their
  mind should not have to wait the window out. Expiry and an explicit
  close converge on the same state; the only difference is who asked
  and what the log records.
- **A reopen invalidates every request created under the old window.**
  This is the subtle one, and without it the window is extendable by
  accident. Each `PendingJoinRequest` records the `opened_at_unix` of
  the window that was live when it was created, and approval requires
  the current window's `opened_at_unix` to equal it. So "let the
  window lapse, reopen, and finish the half-done request from before"
  is not a path, and a request cannot be parked in one window and
  completed in the next.

**Open the window last, not first.** With the epoch rule above, an
expiry part-way through the exchange does not merely expire a PIN, it
kills the request and the pair start again. The ordering that makes a
ten-minute default comfortable is therefore:

1. The joining Comb is installed and shows its own first code and
   fingerprints. Nothing about this needs a window.
2. The operator has **both** pages open and can see both.
3. Only then does the operator open the window on the target, and the
   exchange runs inside it.

This is worth writing down because the failure is not obvious in
advance. An operator who opens the window when they begin setting up
the new Comb spends part of it walking to the other machine, and the
window does not care. Both pages show the deadline and the remaining
time, so the pressure is visible instead of surprising, and the
recovery is stated on the refusal: open the window and start again.

### Both the request and the approval need a live window

The owner's decision, and the stricter of the two options. Gating only
request creation would leave a request created inside a window
approvable at any later moment, which makes the request the long-lived
capability and the window the theatre. So both ends are checked:

- **`RequestJoinColony` is refused when no window is live.** The
  refusal names what to do -- no member of this Colony is currently
  accepting new members, open the join window on a member -- because an
  unauthenticated caller with no next step is a support problem. This
  is the one place where the design takes away an unauthenticated
  capability, and it is a real reduction: no unauthenticated caller can
  create durable replicated state at will any more.
- **Approval is refused when no window is live**, even with a valid
  first code, valid pins, a fresh second PIN, a matching authorization
  entry, and a request still inside its own TTL. The window is checked
  first, before the PINs and before the authorization file, because it
  is the cheapest check and the one the operator most needs explained.
  The request is not deleted; it is dead, and the operator reopens and
  the requester re-creates.
- **The check is at approval time on the leader**, not at page render
  time. A rendered approval form is not a promise, and a window that
  expires while a form sits in a browser must not be approvable on the
  strength of when the form was drawn.

### What the Colony publishes while the window is live

This is the piece that did not exist anywhere before there was a window.
A joining Comb has no Raft membership, so ADR-0115's runtime derivation
has nothing to derive from, and there is no pre-join trust anchor in
the system at all. The window is where the Colony hands one over.

`GetColonyJoinWindow` is unauthenticated, returns only public bootstrap
fields, and refuses once the window is not live:

- `colony_name` -- the resolvable managerd dial name the Colony
  advertises for itself, taken from the serving certificate's SANs.
  Not the LAN address and not a wildcard, per ADR-0139.
- `managerd_fingerprints` -- SHA-256 over the DER of the leader's and
  every member's managerd serving certificate, keyed by node ID.
  Publishing all of them rather than just the leader's alone is what
  lets the joiner check that the member it was handed is a *member*,
  and not a stranger that answered on that address. The owner's
  decision: **every managerd fingerprint**, with the leader not
  privileged among them. Leaders-only was cheaper and left a hole
  exactly where this ADR says behaviour must be identical, since a
  joiner introduced to a follower would have had nothing to check that
  follower against. The cost of the fuller list is that it grows with
  the Colony, and that for the length of a window an operator opened,
  the Colony publishes its own members' certificate digests to anyone
  who asks. Both are accepted.
- `opened_at_unix` and `expires_at_unix`, so a joiner refuses stale
  bootstrap instead of dialing a name it learned from a window that
  closed an hour ago.
- `colony_node_id` and `leader_node_id`, so both pages can name the
  Colony the same way and the operator can see they are looking at the
  same thing.

That is the whole payload: the name, the fingerprints, the window's own
timestamps, and the two node IDs. Not the frontend or raftd
certificates, which are not what peers dial, and not the private keys,
which are not in the Colony to publish.

### The order is trust first, then PINs

The owner's `morethoughts.txt` puts certificate evaluation before PIN
authentication, and the correct reading of that is an **ordering**, not
an extra step. It matters, because a mutual fingerprint exchange is the
thing that makes relay attacks impossible, and a PIN exchange carried
out before the channel is verified hands both numbers to whatever is in
the middle. So the sequence is:

1. The joiner asks the member the operator named for the window, over
   an unauthenticated dial, and receives the name and the fingerprints.
2. The joiner **pins** that advertisement and from then on dials the
   Colony by the advertised name, with `ServerName` set to it and
   verification against the pinned set. A served certificate that
   matches no advertised fingerprint is refused, and the joiner prints
   what it expected.
3. The target **evaluates** the joiner's certificate against the gate
   below, then **pins** it into the replicated trust store. This is
   Part 2's fingerprint handling with the pin kept rather than compared
   and discarded.
4. **Only now** do the first code, the second PIN, and the
   authorization file run, over a channel that both ends have already
   authenticated.

By the time anyone is asked for anything, both ends of the channel are
known to both ends. A PIN flow over an unverified channel is a
man-in-the-middle with two numbers to relay, which is exactly the
attack the mutual exchange is supposed to close, and pins-first is what
makes it closed rather than nearly closed.

### The certificate evaluation gate

Fail-closed, and every check reported to the operator by name on the
target's page with its verdict, the SANs, and the expiry. A failure is
a refusal, not a warning, and the refusal says which check failed.

- the PEM decodes, and to exactly one certificate;
- `x509.ParseCertificate` succeeds;
- `NotBefore <= now < NotAfter`, with both dates printed;
- self-signed as Part 1 generates them: `cert.CheckSignatureFrom(cert)`
  returns nil and the issuer matches the subject. This establishes that
  the certificate is the Comb's own and says nothing about whether that
  Comb is the one the operator meant, which is the fingerprint's job and
  the reason both checks exist rather than one;
- algorithm and strength are acceptable: no MD5 or SHA-1 signatures, no
  RSA below 2048 bits, ECDSA at least P-256, Ed25519 accepted;
- SANs include `IP:127.0.0.1` and at least one `DNS:` name, because a
  certificate carrying only IP SANs is the ADR-0139 failure in
  certificate form;
- for a **local** certificate, the public key matches the private key
  it will be served with.

The last one is local only, and the scope matters enough to say twice.
At install time a Comb confirms its own certificate against the private
key it holds. For a remote joiner's leaf the target can check
structure, dates, algorithm, and SANs, and nothing more: it does not
hold the joiner's private key and must not be asked for one, so the
remote side is recorded by fingerprint and the operator is the one asked
to confirm it. A remote key-pair check written into this list would be
a check nobody performs.

### The replicated peer trust store

A pin written to a page and then forgotten is not a trust store. Pins
are replicated state, so they survive a leader change and a wiped
managerd the same way a join stage does:

```proto
message TrustedPeer {
  string node_id        = 1;
  string comb_name      = 2;
  string fingerprint    = 3;  // "sha256:..." lowercase hex
  string cert_pem       = 4;  // the leaf, so a wiped Comb can rebuild
  int64  not_after_unix = 5;
  int64  pinned_at_unix = 6;
  string pinned_by      = 7;  // node ID of the member that accepted it
  bool   is_voter       = 8;  // false while the pin is held for a request
}
```

- **The leaf is stored, not only its digest.** A digest cannot rebuild
  a PEM bundle, and a Comb that has been reinstalled still has to dial
  the Colony it belongs to.
- **`is_voter` is separate from the pin.** A pin is written when the
  target accepts the certificate, which is *before* authorization, and
  `AddVoter` promotes it. Conflating the two would mean either a
  refused request leaving a permanent pin or a pin implying
  membership. A pin held for a request that reaches a terminal state
  without becoming a voter is dropped with it, so the store does not
  accumulate one entry per Comb that ever asked.
- **Removal is real.** `RemoveServer` drops the entry, and
  `UpdateVoterAddress` replaces the name. A store that can only grow
  accumulates dead certificates until one expires by accident.
- This is replicated state, so it moves the canonical state digest
  (ADR-0143) and is part of the mixed-version rollout already recorded
  below.

### Materializing `/usr/local/etc/apiary/peer-ca.pem`

Every Comb runs managerd, so every Comb writes the file from its
replicated copy, on every change and once at startup. Sorted by node ID
so the bytes are the same everywhere and an operator can diff two
hosts.

- Atomic write, temp file plus rename, **0600, root-owned**, the same
  discipline as every other file here. The directory is 0700.
- **managerd is the only writer**, and the file is a **derived cache**:
  delete it and the next managerd start recreates it correctly, so it
  never has to be in a backup and never has to be restored.
- It becomes the default value of `peer_tls_ca` in all three daemon
  configs and in raftd's replacement-confirmation path. `managerd`,
  `frontend`, and `cmd/raftd/confirm.go` already read that field and
  already use `LoadPeerCAPool`; what changes is that the file now has a
  writer, not that the read path moves.

### `peer_tls_ca` is retired as a manual step, not as a concept

The owner proposed retiring `peer_tls_ca` on the reasoning that TLS is
now on by default everywhere and a CA path is therefore unnecessary.
That reasoning does not hold, and the correction is recorded here
rather than quietly dropped, because the reason is the useful part:

**Enabling TLS everywhere does not create a trust anchor.** A
self-signed serving certificate proves its identity to a party that
already trusts it. Turning TLS on everywhere changes "nothing is
encrypted and nothing is verified" into "everything is encrypted and
nothing is trusted yet", which is a large improvement and a different
thing. The specific gap is step 2 of the sequence above: the joiner's
*first* contact, with the member the operator named, happens before the
joiner has read any advertisement, so the Colony cannot have told it in
advance what to expect. Exactly two things anchor that contact -- the
operator comparing what the joiner displays against what the member
displays, and an operator-supplied `peer_tls_ca` if one is configured.

So the field stays, with a changed default:

- `peer_tls_ca` remains in all three configs and in raftd's confirm
  path;
- its **default** becomes the derived
  `/usr/local/etc/apiary/peer-ca.pem`;
- an operator who sets a real path still wins, and that path then
  covers exactly the contact the derived file cannot reach on its own.

What is retired is the **manual distribution step** in
`docs/add-node-to-colony.md`: copying a CA PEM between hosts by hand.
That is now the writer's job. Dial-time precedence is pinned leaf for a
known peer, else operator-supplied CA, else refuse.

### What Part 4 changes in the earlier parts

- Part 1 gains a fail-closed rule: a Comb with no usable serving
  certificate is never reported as ready, in standalone mode too.
- Part 2's fingerprint comparison becomes a pin. Same pages, same
  deliberate operator copy and paste, different lifetime.
- Part 3 is unchanged. The authorization file gates the same approval,
  one precondition further out.

### The two pages, again

- **On any member of a Colony:** a join-window control showing the
  state plainly. Closed by default, with the button that opens it, the
  duration that will be used and where it came from, and the resulting
  deadline rendered as an absolute local time. While it is open the page
  shows a countdown and a "close now" button, because a window the
  operator cannot see the end of is a window they will not remember to
  close. The control is Admin-tier, since it is a state change, and it
  forwards like the rest.
- **On the requester:** the first step of the join is now "fetch the
  Colony's advertised name and fingerprints from the member you were
  given", and that result is displayed before the first code exists, so
  the operator can compare the Colony's advertisement on the two
  screens in the same deliberate pass as the requester's fingerprints.
  The window's deadline is shown here too, in local time, because the
  requester is the operator who is most likely to be mid-copy when it
  runs out.
- **On the target's pending-request panel:** a per-request line saying
  which window created it, whether that window is still live, and a
  refusal that reads "the Colony is not currently accepting joins; open
  the join window and have the requester start again" rather than
  reading as a failed authorization. The distinction matters, because
  the two failures have completely different fixes, and an operator
  told the wrong one goes looking for a second PIN that was never the
  problem.

---

## What this does not claim

Being explicit, because the honest version is more useful than the
impressive one.

- **It does not constrain a hostile Admin of the target Colony.** Such an
  Admin can read the replicated second PIN and approve alone, or skip the
  flow and call `AddVoter` through any other path. The flow raises the
  cost of *unattended, uninformed, or injected* approval. It is not an
  authorization boundary against someone who already holds the authority
  it authorizes. "A hostile Admin" names two principals with two
  different answers; the next section separates them, because which one
  is in scope decides whether any of the remedies are worth building.
  **This bullet describes Part 2 only.** Part 3 is what constrains a
  Level 1 Admin: the operator's deliberate act, recorded in a file no
  Admin-tier RPC can write, becomes a precondition for a voter. What
  survives Part 3 is impersonation and denial, both named there.
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
- **Part 4's trust store is not a boundary against a hostile Admin
  either.** A pin is written by an RPC on a managerd, which means an
  Admin can add one. The replicated store and the derived
  `peer-ca.pem` are what make honest operation automatic and remove
  hand distribution; they are not a credential a hostile Admin lacks,
  and the layer that does require a credential the Admin does not hold
  is still Part 3's file.
- **The window reduces the unauthenticated surface; it does not close
  it.** While a window is live, any unauthenticated caller can create a
  request against it, and a public host's whole job at that moment is to
  be reachable. What the window changes is that the surface exists only
  when an operator opened it, on a deadline they set, and that every
  request created in it dies with the window.
- **The first pre-join contact is still the operator's eyes.** Pinning
  starts after the joiner has read the Colony's advertisement, and
  reading it means an unauthenticated dial to a member the operator
  named. That is narrowed by publishing every member's fingerprint and
  by an operator-supplied `peer_tls_ca`, and it is not eliminated by
  anything in this ADR.

## The hostile-Admin case is two problems, not one

Added on review, when it became clear that the phrase names two
principals with materially different answers. The distinction decides
everything below, so it is made first. Nothing in this section is
decided; it is the analysis behind open questions 7 and 8.

| Adversary | What they hold | Fixable inside the Colony protocol? |
| --- | --- | --- |
| **Level 1: the Colony Admin** | An Admin-tier manager API key, or an operator session. Replicated state, the web UI, and the ability to approve, reject, or purge a request. | Yes. The remedy is a credential this principal does not hold. |
| **Level 2: root on a target Comb** | The host. The Raft log, the binaries, raftd's socket, the ability to serve a forged leader. | No. The trust root would have to live off the machine. |

Level 2 is not fixable by anything in this ADR, and it is worth being
precise about why rather than leaving it as an omission. Raft
signatures do not help, because a compromised leader signs correctly
and any quorum containing it will accept that signature. Replacing the
binary is worse still, because the state digest and the build gate are
then computed by the attacker's own code. The answer for Level 2 is a
trust root that leaves the machine -- TPM-sealed identity, or remote
attestation -- which is a different project at a different cost and is
not proposed here.

### The structural reason Level 1 wins today

It is not that the second PIN is too short, or stored too plainly, or
not bound tightly enough to its request. It is that **the target
generates the second PIN and then verifies it**. A party that generates
a secret and subsequently validates it has demonstrated nothing. The
second PIN is a real human-attention device -- it stops an unattended
approval, a mis-paste, and a request nobody ever read -- and against a
target that has already decided to say yes, it is decorative.

So the limitation is about *who decides*, not about *how many numbers
are typed*. A longer PIN, a more careful store, and a tighter request
binding all leave the decision exactly where the hostile Admin can
reach it.

### Three classes of remedy, none of them selected here

**A. Require a credential the target's Admins do not hold.** A
Colony-wide join capability, created at formation, held by the operator
out of band, and required on every join. The machinery already exists:
`generateAPIKey` in `internal/manager/auth.go` stores only a SHA-256
digest and never writes the raw form anywhere, and `ApiKey` records
already live in the Raft FSM. A join capability checked the same way is
small work. The property it buys is asymmetric on purpose: a hostile
Admin can **deny** joins -- delete the capability, refuse to forward,
withhold it -- but cannot **forge** one. Denial is survivable and
diagnosable. Forgery is the thing that has to be impossible.

**B. Make the fingerprint mean something.** ADR-0113 already records
that the fingerprint is self-reported text from an unauthenticated
requester. If the joining Comb generates a keypair at install and
answers a nonce challenge from the target, the fingerprint names a key
rather than a string, and the operator's comparison becomes a
possession check. That stops replay of an old request and stops an
attacker fabricating a request from a machine they control, which is
the injection case the two-way flow is already aimed at. It does nothing
against a hostile Admin talking to a genuine Comb, and it is additive:
the requester proves possession, and still does not get to skip the
human.

Worth doing regardless of how A and C go, because it is what justifies
open question 4. Requiring a fingerprint is a security control only if
the fingerprint is bound to something the requester has to prove;
otherwise it is a stricter version of a self-reported string.

**C. Move the decision off the target.** The hostile Admin wins
because there is a decision left for them to override. If the operator
authorizes out of band -- an approval signed by a key the Colony
trusts, produced on the operator's own machine, or on a page the target
Colony does not serve -- and the requester presents that signed
approval, the target's only remaining job is to record it. There is no
longer a thing to bypass. This is the structural fix, and the only one
that closes the hole rather than narrowing it.

The cost is real, which is why it is a question and not a decision: the
operator needs a signing key and somewhere to run the signing step,
which is precisely the friction the two-way numeric flow was designed to
remove. Choosing C means accepting that join authorization is a
deliberate operator action with a key, not a copy and paste.

**Superseded. C was proposed, then rejected by the owner, and Part 3
replaces it.** The rejection was a hard constraint rather than a
preference: nothing may live on an administrator's local machine, and
everything must be built into Apiary. C put the signing step on the
operator's own machine, so it is not available. Part 3 reaches the same
place - the operator's act is the thing that cannot be forged - using a
root-owned file instead of a signature, because the operator's authority
over a Comb is root, and root is exactly what an Admin key does not
confer.

One correction to the paragraph above, kept rather than edited out: the
target does **not** reduce to recording a signed approval. It still
validates request binding, expiry, single use, reachability, and the
join-log guardrail. What changes is that it can no longer *manufacture*
authorization, so bypassing the flow requires forging something rather
than holding an API key.

**Also worth considering: quorum authorization for membership change.**
Require k-of-n voters to approve an `AddVoter`. Membership changes are
already single-copy on the leader, so this is comparatively cheap, and
it converts the blast radius from one host to a majority. It is not a
substitute for A, and a hostile leader can still stall, so the residual
damage is availability rather than integrity -- the more tolerable of
the two.

### Sequencing: this and the deferred internal-token work are one problem

A join capability and the second PIN have identical handling
requirements: never stored in plaintext, bound to a request, single use,
expiring, attempt-limited, fail-closed on mismatch, absent from logs and
pages, and rotatable. That is the deferred internal-token project
almost verbatim.

It matters here because open question 6 proposes replicated
*plaintext* storage for the second PIN. Taken on its own, that ships
the weaker of two versions of the same machinery first and then asks
for the stronger one later. Folding the join capability into the token
project, or sequencing the token project ahead of the PIN's storage
decision, avoids building the handling discipline twice. This is a
sequencing recommendation; it does not reverse anything in Part 2.

Narrowed by Part 3, which was written afterwards. The join capability
that was going to be a secret is now a file, so it is not raftd-
reachable and there is no second version of the machinery to build.
What is left here is only the second PIN's storage discipline, which
stays a Part 2 question and is open question 6 below.

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

**Retire `peer_tls_ca` because TLS is on by default.** The owner's own
proposal, recorded with the correction rather than as a rejection of the
intent. TLS being on everywhere is not the same as anything being
trusted, and the first pre-join contact is the case where the Colony
has had no opportunity to say what to expect. What is retired is the
hand-copying of the file between hosts; the field survives as a derived
default and as the operator-supplied anchor for exactly that first
contact.

**Gate only request creation on the window, not approval.** The looser
option, and it was the one on the table until the owner chose the
stricter. A request created inside a window and approved a week later
makes the request the capability and the window the decoration.

**Let a live window be extended on request.** It has to be a new window,
because an extension is invisible in the state and in the log. Making it
a new window with a new epoch is also what makes the "lapse, reopen,
finish the old request" hole impossible.

**Publish only the leader's fingerprint in the window.** Cheaper, and it
fails the case the ADR is built around: a joiner introduced to a follower
would have no way to tell a member from a stranger that answered on that
address.

**Let the joiner dial a numeric LAN address and skip the name
advertisement.** ADR-0139 is the ADR about why that fails. Part 4 is the
code that makes the rule hold at the moment it matters, which is the
first dial.

## Scope boundary

This ADR does **not**:

- touch the internal raftd/managerd token project, which stays deferred
  until a coordinated security effort covers secret matching, fail-closed
  mismatch behavior, rollout compatibility, startup validation, no
  leakage, tests, and rotation. The Part 3 authorization file does not
  enter that project and does not narrow its scope: it is a managerd
  policy input, never replicated, never a raftd credential, and never
  checked by raftd. raftd keeps seeing an ordinary FSM command. What the
  two share is the *handling discipline* - never replicated, never in a
  log line, never on an RPC response, fail-closed on a malformed file -
  not a mechanism, and so not a reason to sequence the two;
- build a FreeBSD port or package. Binary delivery to a fresh host stays
  exactly as it is today - a checkout, or whatever the owner does;
- remove or deprecate `apiaryinstall`, or move any `/etc`, PF, ZFS,
  kernel-module, package, or network-topology work into `apiaryctl`;
- change the `update` / `apiaryctl force-restart` split from ADR-0141, or
  add anything to `make update`;
- automate anything about `raftd -reset`, the restart guardrail
  (ADR-0103), or the controlled update (ADR-0145, ADR-0146);
- distribute `peer_tls_ca` certificates between Combs by hand. That
  is a real remaining manual step in `docs/add-node-to-colony.md`
  today, and Part 4 removes it by giving the file a writer, which is a
  different change from changing a field. What it does **not** do is
  make the first pre-join contact verifiable, and that contact still
  rests on the operator's comparison or on an operator-supplied
  `peer_tls_ca`;
- add renewal for serving certificates. A 3650-day certificate that
  expires is a visible condition, not a silent one, and a renewal path
  belongs in its own ADR rather than inside an installer;

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
- **ADR-0112** - `common.json`. Becomes the home `setup-quick` never gave
  it.
- **ADR-0115** - automatic `peer_tls_hostname_map`. Not duplicated. This
  ADR fills the one window it cannot cover: before a Comb has joined, it
  has no membership to derive from. Part 4 is where that window gets
  filled, with the Colony publishing the name its members can be
  reached at instead of a joining Comb guessing one.
- **ADR-0139** - `rpc_addr` is a dial target too. This ADR turns its
  comment block into the code that enforces it.
- **ADR-0141** - the installed-binary rule. This ADR is that rule applied
  to the installer, for the same reason and with the same argument.
- **ADR-0143** - canonical state digest. New replicated fields change the
  digest; expected during rollout.
- **ADR-0100, applied to a new file** - `join-authorizations.json` is
  written by a program like every other config file, but it is not
  config: nothing reads it as configuration, no RPC writes it, and it
  carries a decision rather than a setting. It is the one file in the
  project whose authority comes from its ownership rather than its
  contents.
- **ADR-0136 / ADR-0142** - `apiaryctl` as the one root-only installed
  command. `join-authorize` is the same kind of thing as
  `force-restart`: a root-only installed binary acting on the local
  control plane, requiring no checkout, adding no new distribution.
- **ADR-0113** - amended. `yes-trust-new-comb` is removed.
- **ADR-0093** - peer-forwarding TLS needs its own CA trust. Not
  contradicted, and the shape of the answer changes: the CA file
  becomes a derived artifact of replicated pins rather than something
  copied between hosts, while the field stays for the one contact pins
  cannot cover. This ADR is what gives that file a writer.

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

**Part 3, and these are the tests that carry the security claim:**

- An approval with a valid first code, valid fingerprints, and a valid
  second PIN, but **no authorization entry, is refused**, and
  `AddVoter` is never reached. This is the single most important
  assertion in the ADR and it gets its own test with its own name.
- The same, with an entry whose `node_id` matches but whose fingerprint
  does not. Refused.
- The same, with an expired entry. Refused.
- A consumed `authorization_id` presented a second time, against a
  Comb whose local file still lists the entry as unconsumed. Refused on
  the **replicated** consumption, not on the local file.
- Two approvals from the same API key, and two from two keys on the
  same Comb. Both refused. Two keys on two distinct Combs succeed, and
  the response names both key IDs.
- A test that walks the `api/rpc` service descriptor and fails if any
  method name matches the authorization store, so the "no RPC writes
  this file" rule is enforced mechanically rather than by review.
- The file is 0600 and root-owned after install, after an entry is
  added, and after an entry is consumed. A malformed file fails closed
  with its path and refuses every join, rather than falling back to
  "no entries, so nothing is authorized anyway" by accident.
- `apiaryctl join-authorize` with no checkout present, on a fixture
  directory, asserting it writes atomically and prints the matched
  request before writing.
- The refusal message for a missing entry names the Comb that must
  carry it, asserted by substring, so the operator is never left with a
  refusal and no next step.

**Part 4, the window, the trust, and their order:**

- **The window, in state, not in a mock.** A real Raft group. Open on a
  follower, assert the record replicates, assert the deadline is the
  absolute one computed on the leader.
- A leader change with the window open: the new leader answers
  `GetColonyJoinWindow` with the **original** `expires_at_unix`, not a
  fresh one. A test that only checks "the window is still open" passes
  this bug; asserting the exact deadline does not.
- `RequestJoinColony` with no window live is refused, and the message
  names the join window.
- **The central Part 4 assertion, with its own test and its own name:**
  a request with a valid first code, valid pins, a fresh second PIN, a
  matching authorization entry, distinct keys, and a clean join-log
  verdict, whose window has since expired, is refused and `AddVoter` is
  never reached. Every other gate passing is the point of the test.
- The same, where the window was closed early and then **reopened**:
  the request predates the new window and is refused on the epoch
  mismatch. This is the test that proves the window is not extendable by
  lapse-and-reopen.
- A request created inside window A, approved during window B, refused.
  The same test viewed from the other side, kept because the two
  failure messages must not be identical.
- Expiry passes with no leader activity at all: the window closes on
  time because the deadline is absolute, asserted with a shortened
  duration in the test rather than a real ten minutes.
- **The duration, all four ways.** Absent from `managerd.json` gives
  600 seconds, asserted on the actual deadline and not on a constant.
  A configured value is honored. An explicit shorter request is honored.
  A request above the configured value is refused, and the refusal is
  asserted to name both the ceiling and the file that holds it.
- Two Combs configured differently, with the **leader's** value the one
  that applies, and the response's applied duration asserted to be the
  leader's. A test that only checks the window opened would pass a
  mutant that reads a follower's config.
- A zero, negative, or non-integer `colony_join_window_seconds` is a
  **startup error**, asserted against the loader directly. A mutant
  that falls back to 600 is killed here, not in a later test that
  happens to use a valid config.
- `apiaryctl install` writes the field when it creates `managerd.json`
  and does not touch it when the file exists, asserted with a
  hand-edited value in place.
- The published set is **every member**, asserted by count against the
  real voter list: a two-member Colony opened on the follower returns
  two fingerprints, and a mutant that publishes the leader only is
  killed on that count rather than on a substring.
- `GetColonyJoinWindow` after expiry returns no fingerprints, not stale
  ones. Asserted on the absence of the field, not on an error alone.
- **The trust ordering is asserted, not described.** A test in which the
  joiner is pointed at a member whose served certificate matches no
  advertised fingerprint fails at step 2, before any code is requested,
  and no `PendingJoinRequest` is ever created. A test that lets the
  flow proceed to the PINs in that situation fails.
- Every gate check, each with a name and a verdict in the message: a
  truncated PEM, two certificates in one PEM, an expired leaf, a leaf
  not yet valid, a certificate whose issuer differs from its subject, a
  self-signed certificate with a bad self-signature, an RSA-1024 key, a
  SHA-1 signature, a certificate with no `127.0.0.1` SAN, a certificate
  with IP SANs and no DNS name. Each refused by name.
- A locally generated pair whose certificate does not match its private
  key is refused by install, and the remote case is asserted *not* to
  claim that check: a test that a remote leaf is accepted on structure
  alone, with the fingerprint recorded, so the boundary is pinned by a
  test rather than by prose.
- **The derived file, as a file.** Written from a fixture replicated
  set: 0600, root-owned, sorted by node ID, byte-identical on two
  simulated Combs holding the same state. Deleting it and re-running
  the materializer recreates it byte for byte. A test that a hand-edited
  `peer-ca.pem` is replaced on the next start rather than trusted.
- The wire: an operator-set `peer_tls_ca` beats the derived default, and
  the default is used when it is unset. Asserted through the same
  `LoadPeerCAPool` call the daemons make.
- A pin for a request that is rejected is removed; a pin promoted by a
  real `AddVoter` becomes `is_voter`; `RemoveServer` drops the entry.
  All three against real Raft, so the store is asserted to shrink as
  well as grow.
- TLS-by-default: a Comb whose install finds no usable certificate is
  not reported ready, with or without `--colony-member`, and the
  "no TLS certificate presented" path is gone from the templates.

**All four parts, then mutation testing** under the rules already used
in this repository: every mutant applies exactly once, changes bytes,
fails on an **assertion** rather than a compile error, and is restored
byte for byte. A mutant that dies at build time is rejected as evidence
and replaced, as was done for the two build-failure kills in the
force-restart work.

## Open questions for the owner

The first six are points where a decision was made on your behalf. Each
is a small edit here and a much larger one in code. Questions 7 and 8
were scope questions added on review, answered below; question 4 is
retired by Part 4. **Questions 1, 2, 3, 5, and 6 remain open** and are
the ones worth your attention, because each one is a number or a policy
that is cheap in a paragraph and expensive in a schema.

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
4. ~~**A request with no fingerprints is refused.**~~ **Answered, and
   the question is retired.** It was written while TLS was opt-in, so
   it was really asking whether requiring a certificate is a
   compatibility break. The owner made TLS the default on every Comb,
   Part 1 gives every Comb a certificate, and the compatibility path is
   removed rather than deprecated, so a Comb with no certificate is
   now a broken Comb instead of a supported configuration. The
   fingerprints are still mandatory, and now for a reason that is
   structural rather than cautionary.
5. **Reissuing the second PIN is allowed, three times.** The alternative
   is that a mistyped PIN kills the request outright, which is safer and
   much more annoying.
6. **The second PIN is stored in plaintext in the replicated record.**
   Justified above by "the audience is already authorized", but it is a
   plaintext authorization value in raft state, and if that is
   uncomfortable the alternative is a short-lived managerd-local copy
   with an explicit reissue after any leader change - which costs the
   property the owner asked for, that the flow survives a leader moving.
   See the sequencing note above: a join capability would need exactly
   this handling discipline, so 6 and the hostile-Admin remedy are
   easier decided together than apart.
7. **Which adversary level is in scope.** Level 1, a Colony Admin with
   manager API access, and Level 2, root on a target Comb, want two
   different projects. The answer decides whether remedies A and B are
   worth building now or whether the effort belongs in an attestation
   design instead. This ADR as written addresses Level 1.
8. **Whether to move the decision off the target.** If Level 1 is in
   scope, the real choice is between A, a credential the Admins do not
   hold with the target still deciding, and C, the operator signing
   off-band with the target only recording. C is the one that closes
   the hole; A narrows it. If the answer to 7 is that Level 1 does not
   matter and this is an operator-convenience and typo-catch, then the
   Part 2 flow is already the right amount of machinery and both A and C
   should be left out. B is worth having either way, on the reasoning
   given above.

### 7 and 8, now answered

**Level 1 is in scope.** The owner confirmed it: a hostile Colony Admin
holding manager API access is the adversary this ADR must address. Level
2, root on a Comb, stays out of scope and is written down as
unreachable from inside the protocol.

**Neither A nor C, as written.** C was rejected on a hard constraint:
nothing may live on an administrator's local machine, and everything
must be built into Apiary. A was not chosen because a pasted Colony-wide
secret is harvestable by the very Admin it is meant to stop, and because
a capability configured once stops being a per-join human decision. The
owner's decision was option 3, single use: the root-owned authorization
file, plus the two-distinct-keys rule, plus everything Part 2 already
does, all computed under the hood with mutual PIN verification in both
directions. That is Part 3.

9. **Authorization entry expiry.** Proposed at 24 hours. The entry is
   created by the operator at a moment of their choosing, which is not
   the same moment the second PIN is issued, so a five-minute TTL would
   be unusable. The alternative is no expiry at all, which leaves a
   long-lived unused entry sitting in a root-owned file on every Comb.
10. **Two keys on two distinct Combs, or two distinct keys.** Part 3
    writes the stricter rule, because a second key on the *same* Comb is
    two credentials in one place and buys little. The looser rule is
    easier to implement, since it needs no peer coordination and no
    forwarding.
11. **Whether hand-editing the file is supported.** Security-wise a
    root-owned file edit is exactly as safe as the command. It also
    skips the step where `apiaryctl` shows the operator the pending
    request it matched, which is the deliberate comparison. The command
    should be the only documented interface, and a file edit should
    still be honoured, since a root operator who insists is not an
    attacker.

## 12 and 13, now answered

Both were open questions in the Part 4 commit, both are the owner's, and
neither moved anything structural: the window mechanism, the epoch rule,
and the trust ordering were written so that neither answer could change
them.

12. **The window duration is configurable, and defaults to 10 minutes.**
    Not a fixed number, because a ten-minute default has to cover a
    two-human exchange across two machines, and whether that is
    comfortable or tight depends on the Colony and on the operator. A
    Colony whose two machines sit on one desk and one where the operator
    has to walk between them are not the same number, and a value chosen
    once for both would be wrong for at least one of them.

    10 minutes was chosen over the 30 I proposed, and the shorter
    number is the better one here: the epoch rule means an expiry
    mid-exchange costs a restart, so the pressure should be on the
    operator to open the window when they are ready rather than on a
    generous timer. That is why the ordering is written down --
    the joining Comb displays its code first, the operator opens both
    pages, and the window is opened last, immediately before the
    exchange. With that order, ten minutes is a comfortable amount of
    time to copy two numbers and read a fingerprint.

    The configured value is the default **and** the ceiling, and the
    leader's copy is the one that applies. Making the configuration
    the only lever is deliberate: an RPC parameter that could exceed
    the configured value would be a way around the policy the
    configuration states.

13. **Every managerd fingerprint.** Not the leader's alone, and not the
    leader plus a sample. Every current member's, keyed by node ID, with
    the leader not privileged among them.

    The reasoning is the one recorded in the design section and it did
    not survive contact with the question: leaders-only leaves a joiner
    introduced to a **follower** with nothing to check that follower
    against, and follower introduction is a case this ADR exists to make
    work identically. A smaller list would have quietly made the leader
    the only Comb an operator could safely introduce through, which is
    the requirement the owner removed when the joiner was allowed to
    approach any member instead of the leader.

    The cost is accepted: the list grows with the Colony, and for as
    long as a window is open the Colony will hand its own members'
    certificate digests to any caller that asks. That second one is a
    disclosure, and it is a disclosure an operator creates on purpose
    and for a fixed length, which is the property the window was built
    to provide.

**Questions 12 and 13 are answered, and nothing else is answered by
this.** Questions 1, 2, 3, 5, and 6 from the section above are still
open, and the remaining work is those five, then approval, then
implementation.
