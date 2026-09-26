# ADR-0138: A VLAN on a bridge uplink is a Bridge SVI, and is refused rather than half-built

## Status

Accepted (decision). Implemented for the provisioning path
(`internal/vlan`, `internal/cluster`, `internal/jailnet`).
**The live confirmation on `brood`/`drone` is still pending** - see
"Verification" for exactly what has and has not been run.

## Context

`ifconfig: BRDGADD vlan2: Invalid argument (Bridge SVI cannot be added
to a bridge)`, found in the user's own brood notes and reproduced there
(`9b223b9`'s own commit context records it, and
`internal/cluster/network_bridge_leak_test.go` still carries the exact
string as a fixture). The chain, confirmed by reading the code rather
than inferred:

- `brood`'s `managerd.json` sets `"uplink": "bridge0"` - the management
  bridge *is* the configured uplink. That is a legitimate configuration
  here, not a mistake: `internal/assumecheck`'s
  `NAT_UPLINK_DEFAULT_ROUTE` check was specifically fixed to tolerate it
  (`54be431`, `ReasonBridgeMembershipUnknown`), and ADR-0101's
  `uplink_bridged` mode exists precisely because the host's own bridge
  is a legitimate place to put a VM.
- `internal/vlan.EnsureVLAN` runs `ifconfig vlan2 create` then
  `ifconfig vlan2 vlan 2 vlandev bridge0`. **FreeBSD accepts this.** The
  interface comes up, and `ifconfig vlan2` shows a working interface.
- `internal/cluster`'s `ensureNetwork` then creates the network's own
  per-network bridge and calls `EnsureMember(bridge, "vlan2")`, i.e.
  `ifconfig apnet-<hash> addm vlan2`. The kernel refuses it.

The refusal is not incidental. A vlan(4) interface whose vlandev is a
bridge(4) is that bridge's **own L3 switch interface** - a Bridge SVI.
It is not an Ethernet interface enslaved to the bridge; it *is* the
bridge's third layer, the same way the bridge's own `inet` address is.
FreeBSD therefore rejects it as a bridge member, and says so by name.

So the topology Apiary was trying to build does not exist: there is no
`addm` that would make it work. That is the whole finding, and it forces
a decision about what Apiary should do instead - not a bug fix.

## Decision

**Refuse, clearly, before anything is created - and never issue the
`addm` for an SVI in any code path.**

Concretely, in `internal/vlan`:

1. **`EnsureVLAN` refuses a bridge uplink up front.** Before creating or
   probing anything, it observes whether the configured uplink is a
   bridge (via `internal/netif.BridgeMembers`, reused as-is - see
   "Reuse" below). If it is, `EnsureVLAN` returns an error wrapping
   `vlan.ErrBridgeSVI` and creates **nothing**: no `vlanN`, and, because
   the reconciler fails here before `EnsureBridge`, no `apnet-<hash>`
   either. One error per tick instead of an interface created,
   attached-to-nothing, and destroyed again on every pass.

   The refusal is also what an *unreadable* uplink produces. A tagged
   VLAN is the one thing in this package that can leave a
   kernel-level misconfiguration rather than a failed command, so an
   unanswered question ("is this a bridge?") is a refusal, not a guess.
   The next tick re-observes from scratch.

2. **`EnsureMember` returns a state, not a bool, and never issues `addm`
   for an SVI.** `vlan.Membership` is one of four states, kept distinct
   precisely so "not a member" and "could not tell" cannot collapse:

   | state | meaning | `addm` issued |
   |---|---|---|
   | `MembershipPresent` | observed in the bridge's own member list | no |
   | `MembershipAdded` | observed absent, then added successfully | yes |
   | `MembershipSVI` | the interface is a bridge's own L3 | **no, and none is valid** |
   | `MembershipUnknown` | nothing was established | no |

   `MembershipUnknown` is returned - with a non-nil error - when the
   bridge's member list could not be read, when the interface's own
   state could not be read, or when an issued `addm` did not succeed. It
   is a refusal of this pass, never a licence to skip the addm.

   `MembershipSVI` names the owning bridge in `SVIParent`. That makes the
   two cases a caller has to tell apart, one value:

   - **`SVIParent` is a different bridge** (the per-network
     `apnet-<hash>`): a hard refusal, error wrapping `ErrBridgeSVI`. No
     topology makes that addm work.
   - **`SVIParent` is the bridge being asked about**: a success. The
     interface already *is* that bridge's L3, so there is nothing to
     add. It is still a distinct state, because "it is a member" and "it
     is the bridge's own switch interface" are different facts about the
     host, and only a caller that deliberately pointed a network at that
     bridge can ever reach it.

3. **`internal/cluster` acts on the state, not on the error alone.** The
   reconciler still routes every non-success through its existing
   `failOwned` helper (`9b223b9`), which destroys the network's bridge if
   - and only if - *this pass* created it. It also refuses
   `MembershipUnknown` even if some future implementation returned it
   with a nil error, because nothing would have shown the interface was
   on the bridge. A pre-existing bridge is never destroyed, exactly as
   before.

4. **`internal/jailnet`'s epair path is explicitly unaffected.** See
   "Blast radius".

The operator-facing message is one message, shared by both detection
sites, and it names the kernel's own diagnostic verbatim, the configured
interface, and the two things that do work:

> `vlan: Bridge SVI: ...: the configured uplink "bridge0" is a bridge, so
> tagging VLAN 2 onto it would create a Bridge SVI. FreeBSD says so in as
> many words when the addm is attempted: "BRDGADD vlan2: Invalid argument
> (Bridge SVI cannot be added to a bridge)" (confirmed live on brood).
> Apiary will not build a per-network bridge on top of a Bridge SVI:
> point -vlan-uplink at a physical NIC (the interface that is a member of
> that bridge) so tagged networks can be provisioned, or mark the network
> `uplink_bridged` and opt in per node (ADR-0101) to use the host's own
> bridge as-is.`

Note the two options are not equivalent and the message does not pretend
they are. `uplink_bridged` (ADR-0101) puts VMs on the *untagged* host LAN,
which is not "on VLAN 2" - an SVI gives the *host* an address on VLAN 2
and a route to it, while a bhyve tap joining `bridge0` sends untagged
frames. It is offered as what it is: the shared-LAN mode, chosen
deliberately behind its per-node opt-in.

## The FreeBSD semantics this is built on

- A `vlan(4)` interface parented to a `bridge(4)` is a Bridge SVI: the
  bridge's own L3 interface. It cannot be a member of any bridge, and the
  kernel says `Invalid argument (Bridge SVI cannot be added to a bridge)`.
- A `vlan(4)` interface parented to an ordinary interface (`re0`, `em0`,
  another `vlanN`) is an ordinary Ethernet interface and is a perfectly
  good bridge member. **This is the case for every node whose uplink is a
  physical NIC**, i.e. the normal case, and it is the one path that must
  keep working unchanged.
- Bridge membership is exclusive, and re-running `addm` on an existing
  member is an error - so idempotency has to come from reading the
  bridge's own member list first, which is what `EnsureMember` does and
  what makes it safe to call on every tick.

### How "is it an SVI" is decided, and why it is not a guess

Two sources, in order of directness, in `vlan.observeSVI`:

1. **`ifconfig(8)`'s own parent line for the interface** ("Parent
   name: `<iface>`", printed by `if_vlan(4)` when it is available). This
   is a direct observation of the interface's *actual* parent, so it
   wins whenever it is present - including to correct a configured
   uplink that disagrees with a `vlanN` somebody else created by hand.
2. **The configured vlandev (`Manager.Uplink`) plus one observation of
   whether that is a bridge.** This is exact for an interface this
   process just created and tagged, which is the live brood case, and it
   works on an `ifconfig(8)` that does not print a parent line at all.

If neither is available (no parent line *and* no configured uplink), the
answer is `Unknown`, not "not an SVI". Only one exact spelling of the
parent line is recognized; an unrecognized spelling falls through to
source 2 rather than being guessed at.

## Reuse, not reinvention

- **`internal/netif.BridgeMembers`** is the observation primitive, called
  directly (behind a two-method interface so tests can fake it). It
  already returns an error rather than an empty member list when a
  bridge cannot be read, and it already answers "is this a bridge"
  outright from `groups: bridge` - so this ADR inherits its discipline
  instead of re-deriving it.
- **`internal/assumecheck`'s bridge-aware NAT assumption** is the
  precedent for the *meaning* of silence: `ReasonBridgeMembershipUnknown`
  ("an unread bridge is silence, not a mismatch, exactly as
  replica_unobserved/health unknown mean 'no evidence'") and
  `classifyUplinkMismatch`'s three-way split. `MembershipUnknown` is the
  same idea applied to a provisioning decision instead of an assumption
  result. No new reason-code vocabulary was invented: the sentinel
  `vlan.ErrBridgeSVI` is an `errors.Is` target, which is the right tool
  for an error rather than for a replicated journal entry.
- **`internal/cluster`'s `failOwned`/`bridgeCreated` ownership tracking**
  is untouched and still the only thing that can destroy a bridge.
- **`internal/jailnet`'s `Runner` seam** is the precedent for the
  nil-able `Runner` this ADR added to `internal/vlan.Manager` (and for
  its `BridgeObserver` sibling): real shell by default, a fake in tests,
  never a silent no-op.

## Blast radius: the epair(4) path is deliberately untouched

`internal/jailnet/reconcile.go` calls `EnsureMember` for an epair(4) host
side (ADR-0117). An epair end is an ordinary Ethernet interface: it has
no vlan(4) parent, so it can never be a Bridge SVI, and
`observeSVI` settles that positively from its own `ifconfig` output
(`sviNotAVLAN`) rather than by failing to look like something else. The
`addm` is issued exactly as before, on every node, bridge uplink or not.

This is checked rather than assumed, in both directions:

- `vlan.EnsureEpair` treats a `MembershipSVI` report as a failure and
  destroys the pair, instead of returning a host side that was never
  joined to anything. Unreachable in reality; present so a caller
  cannot be handed a silently unjoined jail interface.
- `jailnet.Ensure` treats a non-confirmed membership (`MembershipSVI` or
  `MembershipUnknown`) as `VerdictUnknown` with no repair, so the next
  tick re-reads the host instead of recording a join that did not happen.

`bhyve`'s own `createTap` shells out to `ifconfig <bridge> addm <tap>`
directly and never touches this code at all, so VM taps are unaffected
too.

## Rejected alternatives

### 1. Auto-select ADR-0101's `uplink_bridged` path when the uplink is a bridge

This is the obvious "fix" and it is the one that was explicitly
withheld. It would mean attaching VM taps to the host's management
bridge, which ADR-0101 made an explicit per-node opt-in behind the
phrase `"yes-share-uplink-bridge"`, precisely because a VM there shares
a broadcast domain with the host's own management IP. `SHARED.md`
records the same question as "Open decision, not actioned: should a node
whose uplink is a bridge refuse clearly and early, or auto-select the
bridged path? ... Do not change this without the user." It is also
semantically wrong for a tagged network (see above), and it would
silently move VMs onto the management LAN on any node where an operator
had configured a bridge uplink for unrelated reasons. **Rejected; and
the refusal is a finding to report, not something to decide here.**

### 2. Skip the `addm` and carry on - the reading this ADR most nearly follows

Tempting, because the SVI "needs no `addm`". It produces the worst
outcome available: a per-network bridge holding a gateway address, with
VM taps joined to it and **nothing connecting it to its own VLAN**. That
is a network which looks provisioned, appears healthy in the Networks
page, hands out DHCP leases, and carries no traffic. Loudly failing is
strictly better than a silent dead segment, so `EnsureMember` refuses
and reports the state instead.

### 3. Re-tag the VLAN onto the uplink bridge's physical member port

Real and workable in FreeBSD: discover which interfaces are members of
the uplink bridge, pick the port, and create a *new* VLAN on that port
for the network. Rejected here for scope and for ambiguity, not for
correctness: it needs bridge-port discovery (which of several ports? what
if the bridge has none, or is a LAG, or a VLAN itself?), it changes the
VLAN's on-wire identity relative to what the operator asked for, and it
is a new piece of network-topology inference rather than a fix for a
misunderstood one. Worth an ADR of its own if the need is real.

### 4. Fail only at `addm`, keeping today's shape

The status quo. It leaks nothing (`9b223b9` fixed that) but it creates
and destroys `apnet-<hash>` on every reconcile tick, and its error
(`Invalid argument`) tells the operator nothing about why. Refusing at
`EnsureVLAN` costs one extra `ifconfig` read per tick and removes both.

### 5. Make `EnsureMember` silently succeed for SVIs and let the reconciler decide

Rejected: it puts the FreeBSD topology rule in the reconciler, where
there is no observation to support it. The observation lives in
`internal/vlan`, next to the code that would otherwise issue the `addm`,
and travels as a state.

## What is explicitly not decided here

- **The `Parent name:` fallback.** Source 2 (configured uplink plus one
  observation) is what actually runs in the live case; source 1 is a
  refinement for an `ifconfig(8)` that prints a parent. If a future
  FreeBSD changes that line's spelling, source 2 keeps working and
  source 1 silently stops refining - a degradation, not a failure, and
  not worth more machinery until a real ifconfig is observed printing
  something else.
- **Surfacing the refusal in the UI.** A network that cannot be realized
  currently shows up as a reconcile error in the log and in the
  reconciler's status. An assumptions-journal entry or a Networks-page
  warning for "this node's uplink is a bridge, so this network's VLAN
  cannot be built" would be a natural follow-up and is deliberately not
  built here.
- **Per-tick churn is now gone, but not for the `uplink_bridged`
  mismatch case** (a node without `-allow-uplink-bridging` still fails
  in `ensureUplinkBridgedNetwork`, per ADR-0101). Different path,
  different ADR.

## Verification

**Verified here (macOS, no FreeBSD):** the whole decision path is
exercised against a fake host (`internal/vlan/fakehost_test.go`) that
models `ifconfig`/`bridge(4)`/`vlan(4)` and reproduces the kernel's own
SVI refusal, so a regression that re-introduces the `addm` fails with
`Bridge SVI cannot be added to a bridge` rather than passing quietly.
Covered: the plain-NIC path still adds; a bridge uplink is refused before
anything is created; a pre-existing SVI gets no `addm`; an SVI of the
bridge being asked about is a separate, successful state; an unreadable
bridge, an unreadable interface and an unreadable uplink are each
`Unknown` with no `addm`; the parent-line and fallback sources are
tested separately, including the case where the observed parent
*contradicts* the configured uplink; three repeat passes produce exactly
one `create` and one `addm`; the epair path is unaffected with a bridge
uplink, and both guards above are reachable and asserted. The reconciler
side (`internal/cluster/network_svi_test.go`) asserts the no-leak rule on
the SVI refusal, that a pre-existing bridge is left alone, that
`MembershipUnknown` is never treated as joined, and that the normal path
still assigns a gateway. `internal/jailnet` asserts the epair repair path
is unchanged and the two impossible states are `Unknown`, not repaired.
`gofmt`, `go build ./...`, `go vet ./...`, `go test ./...` and
`go test -race` on the touched packages all pass.

**Not verified, and not verifiable on macOS:** that this FreeBSD actually
refuses the `addm` (taken from the live brood error and from
`if_bridge`'s own diagnostic, not re-established here); the exact
`ifconfig` output of a vlan(4) on a bridge, including whether a
`Parent name:` line is printed - which is exactly why the fallback
exists; and anything at all about bhyve, jails, VNET, PF, ZFS, HAST or
real-network timing.

**Pending on the testbed** (`brood` or `drone`, cross-compile with
`GOOS=freebsd GOARCH=amd64 go test -c ./internal/vlan` and run as root):

- `TestIntegration_BridgeSVIIsRefusedByTheKernelAndByEnsureVLAN` -
  creates a disposable bridge, tags a VLAN onto it, and asserts both
  halves directly: the kernel refuses `addm` for it, and `EnsureVLAN`
  refuses the same topology up front. It also logs the kernel's exact
  wording, which is the evidence this ADR's `sviGuidance` quotes.
- `TestIntegration_EpairIsStillAddedToABridgeOnABridgeUplinkNode` - the
  jail path, on a bridge-uplink node, on a real kernel.
- `TestIntegration_PlainNICUplinkStillAddsTheVLAN` - the normal path
  against a real uplink, twice, to prove the refusal did not generalize.

All three are gated to FreeBSD + root, so they cannot run on macOS
(`macOS also has an ifconfig(8)`, which is exactly why the gate is a
`runtime.GOOS` check and not just a root check).
