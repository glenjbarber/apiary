# ADR-0136: Local CLI for air-gapped operation (`apiaryctl`)

## Status

Proposed

## Context

ADR-0127 ("Sylve.io feature set") names a local root-only CLI and
interactive console as the Phase 4 item that unblocks air-gapped
operation, and sketches it as `apiaryctl` with a root-only Unix socket
at `/var/run/apiary/cli.sock` (mode 0600), thirteen command groups
(`flight-plan`, `cell`, `comb`, `raft`, `vm`, `jail`, `network`,
`storage`, `backup`, `replication`, `cert`, `log`), a TUI status bar,
and a `courier prepare|sync|verify` physical-media workflow.

That sketch is the right problem and, in this codebase, a dangerous
design. The problem is real:

- When `frontend` or `restshimd` is down, there is no way to inspect or
  act on a Colony. The browser is the only management surface, and the
  browser is exactly what is broken.
- An air-gapped deployment has no egress at all, so "use a SaaS console"
  is not an answer to anything.

The danger is structural. A CLI on a management node is, by
construction, a path to Raft writes that bypasses the browser. That path
is one of the most valuable things an attacker who has found a shell
could want, and it is a path the operator installs deliberately, on
purpose, into the trust boundary of every Comb. If the CLI is allowed
general Raft-write access and the restriction is a convention in a
README, the CLI is a rootkit with a friendly man page. The whole design
question is therefore not "what commands should the CLI have" but
**"what may this thing do, and how is that enforced rather than merely
documented."**

Three further facts from the existing code shape the answer:

1. **There is no Flight Plan engine.** `grep -rli
   'flightplan|flight_plan' --include=*.go --include=*.proto` over the
   whole tree returns nothing. No declarer, no scheduler, no reconciler
   for one. ADR-0127's headline CLI command, and the TUI's "active
   Flight Plans" status bar, have no substrate to talk to.
2. **A quorum-less Colony cannot take writes today**, and `managerd`'s
   leader-only RPCs (`ListVMs`, `ListJails`, `ListNetworks`,
   `ListAPIKeys`, ...) return a `leader_hint` on a non-leader rather
   than serving a stale local answer. A CLI that "helpfully" chases that
   hint turns a local tool into a network client, and inherits
   ADR-0113's trust-prompt problem in a context with no browser to
   prompt in.
3. **`raftd`'s socket has exactly one credential**: the
   `-internal-token` from `raftd.json`, checked by
   `raftnode.TokenUnaryInterceptor` / `TokenStreamInterceptor`, and the
   surface behind it includes a general `Apply`
   (`api/internalpb/raftd.proto`). Anything holding that token can write
   the state machine directly, skipping every role map, API key,
   preflight guardrail, and audit trail between the operator and the
   log. The CLI must never be given that token.

## What already exists

The precedents this design follows are not hypothetical; they are in the
tree.

**`raftd -status` / `-status-json` (ADR-0124).** A one-shot, read-only
diagnostic that opens the node's own `raft.db` through BoltDB with
`ReadOnly: true`, reports persisted term / log bounds / membership, and
exits. It starts no server, takes no vote, contacts no peer, writes
nothing, never `MkdirAll`s, uses a bounded `offlineOpenTimeout` of 2s
and reports `ErrOfflineRaftdRunning` rather than blocking, and carries
`*_observed` booleans into its JSON so a script can tell an unread value
from a real zero. It requires no confirmation phrase precisely *because*
it changes nothing — the ADR says so explicitly, and calls out that this
is the property that makes it safe to leave sitting in `rc.conf`'s
`apiary_raftd_args`. This is the model for CLI posture, and its "changes
nothing" argument is the exact contrast the destructive path must draw.

**`raftd -reset` / `-export` / `-restore`.** These are the existing
destructive/offline conventions. `-reset` and `-restore` each require a
typed phrase that must match exactly (`resetConfirmPhrase =
"yes-wipe-raft-state"`, `restoreConfirmPhrase =
"yes-restore-raft-state"`, `cmd/raftd/main.go`) or nothing happens.
`-export` requires a leader and writes an archive (ADR-0051); its
`-restore-dry-run` counterpart validates an archive's format version and
checksum and prints a summary "needs no confirmation phrase, and does
not touch the data directory" — i.e. the preview is a separate, free
subcommand from the mutation.

**`apiaryinstall`.** Described by the `Makefile`'s own `install` target
comment as "a one-shot host-prep CLI meant to be run from this checkout
and never installed permanently", and deliberately excluded from
`INSTALL_SRCS`. It runs as root because the specific operations it
performs (zpool creation, network modification, rc.d installation) each
individually require it. It is a *provisioner*, not an operator tool,
and its privilege is justified per-operation.

**managerd's external gRPC surface** (`api/rpc/manager.proto`, `service
ManagerService`): the browser's own API. It is role-gated by
`internal/manager/auth.go`, which authenticates a bearer token out of
gRPC `metadata` (`extractBearerToken`) and checks it against
`requiredRoleFor(fullMethod)`. `Role` is a three-tier `viewer < operator
< admin` (`RoleViewer`, `RoleOperator`, `RoleAdmin`, ADR-0030/ADR-0023),
`roleRank` gives an unrecognized role rank 0 so it is never treated as
"no restriction", and `requiredRoleFor` **defaults any RPC absent from
its map to `RoleAdmin`** — a fail-closed default explicitly written so a
newly added RPC can never ship silently under-protected. `Satisfies` is
shared with `internal/frontend`'s role-gated routes, so the browser and
any API client are already judged by one hierarchy. Keys are minted
through `CreateAPIKey` and carry `APIKeyInfo` metadata only, never the
raw key.

**Leader-only vs local reads.** The proto already draws the line the CLI
must respect. `ListVMs` "only succeed[s] against the current leader";
`ListVMsLocal` and `ListJailsLocal` "read the local FSM state without
requiring leadership"; `ListNetworks` is leader-only and forwards, which
is why `GetLocalNetworkBridgeStatus` exists as a never-forwarded local
read. `HostStats` follows the same locality convention. Every
leader-rejecting response carries `leader_hint`.

**Evidence discipline.** ADR-0056 (evidence-aware health) and ADR-0118
(why not a quorum-blocker detail) establish the project rule this CLI
must obey: `replica_unobserved` and health `unknown` mean *no evidence*,
not healthy and not failed. Silence is a real state with its own
verdict.

**Install/update split.** `INSTALL_SRCS` is `raftd managerd frontend
restshimd`; `apiaryinstall` is built by `build` but never installed.
`INSTALL_SRCS_FILTERED` is the subset the `update` target restarts, via
`service apiary_$$S restart` — so the set that is *installed* and the
set that is *restarted on update* are deliberately different.

## Decision

Build `apiaryctl` as a **local, read-only-first client of managerd's
existing external gRPC API, on loopback, authenticated as an ordinary
Colony API key, with a compile-time RPC allowlist and no access to
`raftd` whatsoever.**

The central commitment, stated plainly:

> `apiaryctl` must never be a general Raft-write backdoor, and this is
> guaranteed structurally rather than by convention. It authenticates
> with the same bearer-API-key path and is judged by the same
> `requiredRoleFor` fail-closed role map as a browser session; it holds
> no `raftd -internal-token`; it never opens
> `/var/run/apiary/raftd.sock`; and it does not import `api/internalpb`
> or `internal/raft`, enforced by a build-time import-boundary test. On
> top of that shared server-side gate it adds a second, client-side
> gate: a compile-time allowlist of full gRPC method names with no
> dynamic dispatch, so a command that is not registered cannot be
> invoked at all.

### D1. Posture: read-only-first, three escalation tiers

Every command the CLI can send belongs to exactly one tier, and the tier
is a property of the command, not a flag the operator supplies.

**Tier 0 — observational. No confirmation, no escalation flag, on by
default.** This is the overwhelming majority of the surface and the
entire reason to build the tool: `Status`, `GetLocalNodeHealth`,
`ClusterHealth`, `ListVMsLocal`, `ListJailsLocal`,
`ListAssumptionResults`, `ListAssumptionClaims`, `ListNodeServices`,
`PreflightRestartNodeService`, `GetLocalHASTResourceStatus`,
`GetLocalNetworkBridgeStatus`, `ListISOs`, `ListVMSnapshots`,
`ListJailTemplateNames`, `ListAPIKeys`, `HostStats`.

Note what is *not* in that list. `GetVM` and `GetJail` are leader-only,
exactly like `ListVMs` and `ListJails` — `GetVM`'s own doc comment says
"GetVM and ListVMs only succeed against the current leader". They are
allowed, but allowed *knowingly*: a Tier 0 command may legitimately
require leadership, and what the CLI owes the operator is an accurate
`not-observed (requires leader)` rather than a reclassification of the
call as a write. The tier is about consequence, not about leadership.

**Tier 1 — ordinary lifecycle. Requires a typed confirmation phrase.**
`CreateVM`, `UpdateVM`, `DeleteVM`, `SetVMDesiredState`,
`CreateVMSnapshot`, `RestoreVMSnapshot`, `DeleteVMSnapshot`,
`CreateJail`, `UpdateJail`, `DeleteJail`, `SetJailDesiredState`,
`SetJailHostname`, `CreateNetwork`, `SetNetworkName`, `DeleteNetwork`,
`RestartNodeService`, `SetDatasetQuota`, `SetVMFirewallRules`,
`SetVMFirewallPaused`, `SetVMCloudflareExposure`, `UpdateNodeConfig`,
`UpdateFrontendConfig`, `UpdateRestshimdConfig`,
`UpdateManagerdBindAddress`.

**Tier 2 — irreversible or blast-radius-bearing. Not available in v1 at
all.** `ForcePurgeVM`, `ForcePurgeJail`, `PurgeStaleAssumptionResults`,
`RevokeAPIKey`, `CreateAPIKey`, `ApproveJoinRequest`,
`ConvertStandaloneToJoiner`, `SimulateNodeFailure`,
`SimulateNetworkFailure`, `IssueOriginCertificate`,
`DeleteAssumptionClaim`, `SaveAssumptionClaim`, `MigrateVM`,
`MigrateJail`, and `UploadISO` / `PushISOTo` / `PushJailTemplateTo`
(peer-only bulk transfer paths that should not be driven from a
terminal).

Tier 2 is empty in v1 on purpose. `SimulateNodeFailure` and
`SimulateNetworkFailure` are chaos-injection RPCs with no undo; a
terminal is the *worst* possible place to offer them, because there is
no second tab to notice the mistake. `ForcePurge*` are the purge path
for resources that are already unmanageable, and the operator who
reaches that state has a live Colley's worth of other problems. Being
unable to do these from the CLI is a feature; if they are ever added,
they arrive with the D5 gating below and an ADR amendment.

The default is safe and the dangerous path is loud: no command in this
tool can be made to do something by accident, and there is no `--force`,
no `--yes`, no `-f`, no environment variable that widens a tier's
requirements.

### D2. Transport: managerd over its existing external gRPC API on
loopback

`apiaryctl` dials `managerd`'s **external** `ManagerService` — the same
service, the same socket, the same auth interceptor the browser and
`restshimd` use — not the internal `RaftInternal` service on
`/var/run/apiary/raftd.sock`.

**Local only, and there is no `--remote` in v1.** The dial target is
`managerd.json`'s own configured bind address, which defaults to
`127.0.0.1`, overridable with an explicit `--addr` that is *required* to
resolve to a loopback address; anything else is a hard error, not a
warning. This is the honest answer to ADR-0113 rather than a new trust
mechanism: the CLI never faces a peer whose TLS trust root it cannot
verify, because it never talks to a peer. The browser keeps the trust
prompt where it already works; the CLI defers to it.

**TLS pinning from local state.** When managerd is serving TLS, the CLI
builds its dial options through the existing `internal/tlsdial`
`ManagerDialOption(useTLS bool, caFile, serverName string)`, with
`caFile` taken from the `tls_cert` path in the node's own
`managerd.json` — the node's certificate is the pin, read from local
disk by a process already running as root. This avoids needing any
interactive trust decision: the trust anchor is the filesystem the CLI
already had to read to find its API key. If that file is missing or
unreadable, the CLI fails closed rather than falling back to the system
pool, because a CLI that silently downgrades to a public trust store on
a management node is exactly the failure the air-gap use case must not
have.

### D3. Authentication: an ordinary Colony API key, root-only on disk

The CLI proves it is the operator and not someone who found a shell by
carrying a **normal Colony API key** (`apk_`-prefixed, minted through
`CreateAPIKey`, ADR-0023), presented as a bearer token in gRPC
`authorization` metadata, hashed and validated by `ValidateAPIKeyHash`
through managerd's existing `raftAPIKeyValidator`. The CLI's authority
is therefore *exactly* the union of what an Admin/Operator role would
grant in a browser, and no more. There is no CLI-specific role, no
elevated key class, and no way to be authorized for something a
logged-in Admin could not do through the web UI.

- **Location**: `/usr/local/etc/apiary/cli.key`, mode `0600`, owner
  `root:root`, sitting beside the other `0600` secrets in
  `/usr/local/etc/apiary/` (the `bootstrap` target already `chmod 600`s
  both `raftd.json` and `managerd.json` there, `Makefile:211,213`). The
  key is never printed, never logged, never echoed in a preview, and
  never passed on a command line where it lands in `ps` and shell
  history. `-key-file` exists but defaults to that path and is only
  honored if the file is `0600` and root-owned; a world-readable key is
  refused.
- **Rotation**: mint a replacement with the browser (or, once
  `CreateAPIKey` is untiered, a Tier 1 confirmation), write the new file
  with the same `cp`-to-`.new`-then-`mv` atomic rename the `install`
  target already uses (because it cannot overwrite a file a live process
  has open, and the same reasoning applies to a key being replaced under
  a running shell), then `RevokeAPIKey` the old one. The window where
  both are live is the window the operator chose; there is no way to
  rotate without a window, and pretending otherwise would be the
  convention-based answer again.
- **Revocation actually bites**: because the key is validated against
  the replicated API key store, revoking it disables the CLI exactly as
  it disables every other client, with no local cache to clear.

The residual risk, stated rather than hidden: on a host where the
attacker is root, they can read `cli.key` and use the CLI's Tier 0 and
Tier 1 surface. That is not a new capability — it is the same capability
root already has through the browser's own stored session, and it is
bounded by the same role map. What the design removes is the *extra*
capability that root does not otherwise have: direct `Apply` to the Raft
log with no role, no key, and no record.

### D4. The Raft-write boundary

Every Raft write the CLI can cause is a Raft write the Colony's own
`ManagerService` would accept from an Admin's browser. There is no
second writer, no bypass, and no privileged path. Enforced in three
independent layers, any one of which alone would be sufficient:

1. **Server-side (already exists, shared with the browser).**
   `checkAuth` applies `requiredRoleFor` with a fail-closed `RoleAdmin`
   default. A method the CLI somehow named that it should not have is
   still role-checked by a map the CLI does not own and cannot influence
   at runtime.
2. **Client-side allowlist (new, structural).** `apiaryctl` holds a
   compile-time table of `(fullMethod → tier, required role)`. Command
   dispatch is a switch over registered commands, not a reflective "map
   this string to this method" over the service descriptor. A method
   that is not in the table has no command, therefore no code path,
   therefore no way to be invoked — including by a future contributor
   who adds an RPC to the proto and forgets the CLI. A unit test asserts
   the allowlist is a strict subset of `ManagerService`'s methods, so a
   stale entry fails the build rather than silently widening.
3. **Import boundary (new, structural, and the one that actually answers
   the rootkit question).** `cmd/apiaryctl` must not import
   `api/internalpb` (which contains `RaftInternal` and its `Apply`) or
   `internal/raft`, and must not read `raftd.json`'s `internal_token`. A
   test walks the CLI's transitive import set and fails if either
   package appears. Combined with never naming `raftd.sock` as a dial
   target, this makes "CLI became a Raft-write backdoor" not a review
   question but a **compile/test failure**.

**Against a follower.** Tier 0 local reads (`ListVMsLocal`,
`ListJailsLocal`, `GetLocalNodeHealth`, `PreflightRestartNodeService`)
work and are labelled `source: local-fsm`. Leader-only reads and every
write are rejected by managerd with a `leader_hint`; the CLI does
**not** follow the hint, because following it would mean dialing another
node over TLS with no way to prompt about trust. It prints `not-observed
(requires leader; current leader: <id or unknown>)` and exits non-zero.
"This node cannot answer because it is not the leader" is a different
fact from "there is no leader", and the CLI keeps them distinct.

**Against a quorum-less Colony.** Writes fail at Raft, below managerd.
The CLI distinguishes `no quorum` from `not leader` from `applied` and
renders them as three different states with three different exit codes —
never a generic error, and above all never success. A Cluster-wide
failure and a local refusal to forward are not the same event and a
script must be able to tell them apart. This is the ADR-0118 discipline
applied to exit codes.

### D5. Destructive operations: typed confirmation, real preview, audit

Mirroring the `raftd -reset` / `-restore` convention and ADR-0103's
preflight model, a Tier 1 mutation requires all three of:

- **A typed confirmation phrase that cannot be pre-computed.** The
  required phrase is derived from the operation *and* the content hash
  of the preview the operator must first read, e.g.
  `yes-delete-3-vms-a1b2c3d4`. A script cannot embed it, because it
  cannot know the hash without running the preview — which is the point.
  Typing `yes` is not accepted. This is deliberately stronger than
  `raftd`'s static `yes-wipe-raft-state`, and the reason is that a
  static phrase can be pasted from a runbook while a content-derived one
  forces the operator to have looked at the list.
- **A locally computed preview of exactly what will change**, produced
  by a separate free subcommand the way `raftd -restore-dry-run` is
  (format version, checksum, summary, no changes, no data-directory
  touch). The preview names every affected object by identity, not just
  by count, and separates into `will change`, `will not change`, and
  **`unknown`** — anything the CLI cannot determine locally is listed as
  `unknown` rather than omitted or assumed safe. An operator who cannot
  tell what a delete will take with it does not get a shorter path to
  performing one.
- **An audit record.** The Colony has no general replicated audit log
  today (`grep -rl audit --include=*.go internal/` only reaches
  `internal/frontend`, which is UI-side), so the record is: an
  append-only line to `/var/log/apiary/apiaryctl.log` carrying UTC
  timestamp, invoking user, node ID, full gRPC method, target
  identities, preview hash, and the confirmation phrase; plus, where the
  request type has the field, a populated `change_rationale`
  (`UpdateNodeConfigRequest.change_rationale`,
  `UpdateManagerdBindAddressRequest.change_rationale` — both verified
  fields) so the change is also visible in ADR-0120's replicated
  rationale history and survives the node. Building a real replicated
  audit log is a separate ADR; this ADR does not pretend the local file
  is one, and says in the preview output that the record is local to
  this node.

**There is no bare `--force` and no `-f`.** `--force` is the exact
mechanism by which a confirmation gate degrades into a suggestion inside
a runbook that nobody re-reads.

### D6. Output and evidence discipline

A CLI that prints a confident wrong answer is worse than one that prints
`unknown`, because scripts and tired humans both act on it. Every field
renders in one of exactly three states, and the JSON mirrors ADR-0124's
`*_observed` booleans so a script can tell an unread value from a real
zero:

- `observed` — with a **provenance tag** and an age. `source: local-fsm`
  (this process read the node's own state), `source: managerd` (the node
  asserts it), `source: offline-disk` (read from this node's `raft.db`
  with no server running). The distinction between "I checked" and "the
  node says" is rendered, not implied, because on a quorum-less Colony
  those two disagree and only one of them is a measurement.
- `not-observed` — rendered as `unknown`, never as `false`, `0`, `none`,
  `-`, or an omitted JSON field. Mirrors ADR-0056's `replica_unobserved`
  and ADR-0124's explicit note that leadership cannot be answered from
  disk at all.
- `not-applicable` — an explicit third state, so "this node has no HAST
  role" is never rendered as "HAST is not healthy".

Two rendering rules follow:

- **Absence of evidence is never absence of a fault.** A missing
  observation never contributes a `false` to an aggregate health field
  or a non-zero exit code on its own.
- **`--strict` makes the CLI honest for automation.** Under `--strict`,
  any `unknown` anywhere in the output makes the exit code non-zero, so
  a monitoring script cannot mistake a degraded read for a clean one.
  The default is lenient (an operator wants the table), the flag is
  available, and the flag's absence is stated in `--help`.

The TUI/console mode sketched in ADR-0127 is **deferred**, not adopted.
A full-screen live status bar is the least testable and most expensive
thing in that sketch, it competes for the same terminal an operator
needs for a typed confirmation phrase, and its correctness problem —
rendering a state it cannot observe — is exactly the problem this ADR is
about solving. Text and JSON first.

### D7. Air-gapped operation

"Air-gapped" here means **no egress**: no route to the internet, no DNS
resolution of public names, no certificate transparency, no ACME (Let's
Encrypt and DNS-01-with-public-DNS issuers are unavailable), no
WireGuard rendezvous against a public coordinator, no package mirror. It
does **not** mean "one node" or "no network". A four-Comb Colony on a
private management LAN with zero egress is air-gapped and fully
supported; that is the target deployment, and it is the case ADR-0127's
courier workflow assumes.

Under those conditions `apiaryctl` needs **no external network at all**.
It dials loopback, reads two local files (`managerd.json` for the
address and TLS pin, `cli.key` for the credential), and issues
cluster-internal gRPC. `internal/tlsdial` with a local `caFile` needs no
OCSP, no CRL, and no AIA fetching — a property worth stating, because a
TLS stack that reaches out to a CRL endpoint is a TLS stack that can
hang in an air-gap, and `ManagerDialOption` with an explicit `caFile`
does not.

**What the CLI cannot yet do, stated plainly: it cannot orchestrate.**
There is no Flight Plan engine in this codebase. `apiaryctl flight-plan`
and the TUI's "active Flight Plans" counter from ADR-0127 are
unimplementable today, and this ADR does not ship a stub that prints an
empty list to look complete. A CLI's `flight-plan` group arrives with
the engine ADR, not before. Likewise ADR-0127's `courier
prepare|sync|verify` depends on ADR-0131's backup and the WireGuard
ADR's rendezvous, neither of which is merged; the CLI contributes the
`storage` and `backup` *read* groups now and the courier verbs when
those land.

The CLI's real air-gap value in v1 is therefore narrower and more
durable than the sketch: it is the tool you can still use when
`frontend` and `restshimd` are down, on a node with no egress, to see
what the Colony actually believes, including the parts it does not
believe.

### D8. Packaging and privilege

**Installed, unlike `apiaryinstall`.** `apiaryctl` goes in
`INSTALL_SRCS` and lands at `/usr/local/libexec/apiary/apiaryctl`
through the same `cp -p` → `chmod +x` → `mv` atomic rename the `install`
target already uses. The distinction from `apiaryinstall` is not
promiscuity, it is *availability at the moment of need*: `apiaryinstall`
is a provisioner for a host that is being built, so running it from a
checkout is fine; `apiaryctl` is a diagnostic for a host that is
**already broken**, and a diagnostic that only exists in a git checkout
on the developer's laptop is not a diagnostic. It ships with the daemons
for the same reason `raftd` does.

**Not a service, so not in the update-restart set.** `apiaryctl` is
excluded from `INSTALL_SRCS_FILTERED` because the `update` target
iterates that list with `service apiary_$$S restart`, and the CLI has no
rc.d script, no `apiary_cli_enable` knob, and nothing to restart. This
is precisely the split that commit `89f21e8` made visible by editing
`INSTALL_SRCS_FILTERED` (it *removed `managerd` from the update restart
exclusion list*, `Makefile:35` — it did **not** remove `managerd` from
`INSTALL_SRCS`, the install set, where it still sits). The lesson
generalizes: "installed" and "restarted on update" are different sets,
and a non-daemon binary belongs in the first and not the second.

**Privilege: root, because of one file read, and nothing more.** The CLI
needs root only to read the `0600` `cli.key` and the `0600`
`managerd.json`. It is **not** setuid and gains nothing from being
setuid; it does not require a `wheel`-group bypass, does not need to be
in any supplementary group, and drops to the invoking user for its own
output and log writes. It requests no capability it does not use, and it
does not shell out to `zfs`, `bhyve`, `jail`, or `service` — every
effect it causes is an effect a managerd RPC causes, so there is no
local-privilege escalation surface for a local-root attacker to widen
and no second, unaudited mutation path around the state machine.

## Rejected alternatives

**General Raft-write access, guarded by convention.** Rejected. This is
the tempting one, because the CLI *could* be given `raftd`'s
`-internal-token` and the `RaftInternal.Apply` path and then be
"restricted by a documented list of safe operations". It fails on every
axis. The internal token is a single static bearer secret in
`raftd.json`; anything holding it can write the FSM directly, skipping
`requiredRoleFor`, the API key store, ADR-0103's preflight guardrails,
and the audit trail entirely. There is no per-operation authorization to
write conventions *about* — the convention would be enforced by the
honesty of the code that ships it, and the code that ships it is exactly
what a future contributor, a merge, or an attacker modifies. And the
failure mode is maximally bad: a Raft-write path that bypasses the
browser is indistinguishable from a rootkit, because it *is* one
functionally. `apiaryctl` is installed deliberately by the operator onto
every Comb; that is a strong reason it must be provably weaker than the
thing it sits next to, not stronger. The D4 import-boundary test
converts this from a promise into a build failure.

**The CLI talks directly to `raftd` over its UDS.** Rejected for writes,
and rejected for reads too in v1. `raftd.sock` is `0600` root and its
only credential is the one static token; ADR-0001's process split exists
precisely so that the state machine is reachable by something narrower
than the API server, and a CLI holding the token undoes that separation.
The legitimate use case — "what does this node's disk say when raftd is
stopped and therefore not answering?" — is already served, better, by
`raftd -status` (ADR-0124): read-only, no lock contention, no network,
no token, and honest about the difference. Having a second way to ask
the same question through the privileged socket adds capability and no
information.

**A dedicated `cli.sock` served by a new always-on daemon.** Rejected.
ADR-0127 sketches exactly this. A new root daemon that exists only to
serve a local client which can already reach managerd on loopback is a
permanently mounted, permanently privileged attack surface that buys
nothing — and it directly defeats the tool's stated purpose, because a
second daemon is a second thing that can be down when you need the CLI.
The socket would also have to re-solve authentication, since a UDS has
no bearer-token story of its own.

**The CLI embeds or reimplements `managerd`'s logic.** Rejected. It
would mean two implementations of one state machine, which is how Apiary
ends up with two answers to "what is this VM doing" that differ only
under exactly the conditions where the answer matters. It also
re-invents the preflight guardrails, the leader-forwarding, and the
error mapping that ADR-0103 and ADR-0118 spent ADRs getting right. Reuse
the API; do not fork it.

**Reuse the browser's session/JWT.** Rejected. Sessions are minted by a
login flow that a terminal cannot complete, are stored server-side, and
coupling the CLI to that store creates a path to session forgery that is
strictly worse than a scoped, revocable, role-limited API key.
ADR-0023's key model is the right credential for a machine client and
already exists.

**Ship ADR-0127's full command surface now.** Rejected. `flight-plan`
has no engine. `backup`, `replication`, and `courier` have no merged
ADRs. `cert` depends on ADR-0133, not merged. `cell` and `comb` are
vocabulary without distinct backing RPCs. A CLI that presents thirteen
top-level command groups, eight of which print "not implemented", is
worse than a CLI with five that work, because operators will read
absence of an error as absence of a problem.

## Consequences

### Positive

- **The trust boundary is provable, not asserted.** A reviewer can run
  the import-boundary test and the allowlist test. "Can this tool become
  a Raft-write backdoor?" has a mechanical answer.
- **The CLI is strictly weaker than the browser.** Same key model, same
  role map, same fail-closed default, no additional surface. That is a
  property worth more than any feature on the list.
- **Operators regain a management surface when `frontend` and
  `restshimd` are down**, which is the actual problem, and they regain
  it in the air-gapped case with zero egress.
- **The Raft-write boundary is drawn where the code already draws it.**
  `ListVMsLocal` vs `ListVMs` and the never-forwarded local reads are
  existing, tested distinctions; the CLI reuses them rather than
  inventing a parallel notion of "local".
- **`unknown` is a first-class output.** Scripts built on `--strict`
  cannot mistake a degraded read for a healthy Colony.
- **Destructive operations are auditable after the fact** via a local
  append-only log and, where the field exists, replicated
  `change_rationale`.
- **Tight install discipline.** Shipped with the daemons so it is
  present when needed; not an rc.d service, so `make update` does not
  try to restart it.

### Negative

- **A second authentication surface to get right.** `cli.key` is a
  credential on disk whose compromise yields exactly the role it was
  minted with, and its lifecycle (mint, atomic replace, revoke) is
  manual and outside the browser's own key-management UI for now.
- **A second copy of the authorization table.** The allowlist in
  `apiaryctl` duplicates `requiredRoleFor` in
  `internal/manager/auth.go`. The duplication is intentional and is what
  makes the client-side gate structural, but it is a table that can
  drift — mitigated by the subset test, not eliminated.
- **Limited reach in exactly the case it is built for.** If `managerd`
  is down, the CLI is down too. The fallback is deliberately the
  existing offline surface (`raftd -status`, `raftd -restore-dry-run`),
  not a second mechanism inside the CLI.
- **Terminal-native destructive operations are still risky.** A typed,
  content-derived confirmation is much better than `--force` and still
  worse than a browser dialog that shows a rendered diff next to the
  button. This is inherent to the medium.
- **No orchestration.** Until a Flight Plan engine exists, `apiaryctl`
  reports state and performs explicit single-object operations; it
  cannot express intent and let something else converge on it. That is
  the largest gap against ADR-0127's vision, and it is a missing
  substrate rather than a missing feature.
- **A CLI that can be wrong at 3am.** Terminal output has no diff
  review, no undo affordance, and no second observer. The tiering and
  preview requirements are mitigations, not solutions.

## Implementation notes

### Commands and flags (concrete)

```
apiaryctl [global flags] <group> <verb> [args]

Global flags
  --addr string        managerd address; must resolve to loopback (default
                       from managerd.json, which defaults to 127.0.0.1)
  --key-file path      bearer API key (default /usr/local/etc/apiary/cli.key;
                       refused unless mode 0600 and root-owned)
  --json               single JSON object on stdout instead of text tables
  --strict             exit non-zero if any field rendered as unknown
  --timeout duration   per-RPC deadline (default 10s)

Read-only (Tier 0, no confirmation)
  apiaryctl status                     # -> Status
  apiaryctl health                     # -> GetLocalNodeHealth, ClusterHealth
  apiaryctl node list|services         # -> GetLocalNodeHealth, ListNodeServices
  apiaryctl vm list --local            # -> ListVMsLocal  (never ListVMs in v1)
  apiaryctl vm get <name>              # -> GetVM
  apiaryctl vm snapshots <name>        # -> ListVMSnapshots
  apiaryctl jail list --local          # -> ListJailsLocal
  apiaryctl jail get <name>            # -> GetJail
  apiaryctl network bridge             # -> GetLocalNetworkBridgeStatus
  apiaryctl storage hast               # -> GetLocalHASTResourceStatus
  apiaryctl preflight <service>        # -> PreflightRestartNodeService
  apiaryctl log assumpts [results|claims]
  apiaryctl raft status                # offline disk read; see D2 note below

Mutating (Tier 1, typed confirmation required)
  apiaryctl vm create|delete <name> [flags]
  apiaryctl vm power <on|off|cycle> <name>       # -> SetVMDesiredState
  apiaryctl snapshot create|restore|delete ...
  apiaryctl jail create|delete <name> [flags]
  apiaryctl jail power <on|off|cycle> <name>
  apiaryctl network create|delete <name>
  apiaryctl node service restart <service>      # -> RestartNodeService
  apiaryctl config node|frontend|restshimd ... --change-rationale <text>

Not present in v1 (Tier 2, see D1)
  any verb mapping to ForcePurgeVM, ForcePurgeJail,
  PurgeStaleAssumptionResults, SimulateNodeFailure, SimulateNetworkFailure,
  RevokeAPIKey, CreateAPIKey, ApproveJoinRequest, ConvertStandaloneToJoiner,
  IssueOriginCertificate, MigrateVM, MigrateJail
```

`apiaryctl raft status` is the one subcommand that does **not** talk to
managerd. It shells out to the existing `raftd -status` (ADR-0124) —
read-only, stopped node, no token — and passes its `-status-json` output
through with `source: offline-disk` provenance intact. Delegating rather
than reimplementing is the point: the CLI never opens `raft.db` itself,
so it can never be the thing that writes to it.

### RPCs and UDS calls

Every gRPC call goes to `ManagerService` on the loopback address with
`grpc.WithPerRPCCredentials` carrying the bearer key, dialed through
`tlsdial.ManagerDialOption` with the local `caFile`. The CLI makes
**zero** calls on `api/internalpb.RaftInternal` and **zero** connections
to `/var/run/apiary/raftd.sock`. `raftd -status` is invoked as a
subprocess with `-status-json` and no other flag, so a bug in the CLI
cannot construct a `-reset` or `-restore` invocation on its behalf
(those require their own static confirmation phrases, but the CLI does
not pass them and never will).

### The authorization mechanism and how it is enforced

Two tables, one owned by the server, one by the client:

```go
// internal/manager/auth.go (existing, unchanged)
//   requiredRoleFor(fullMethod) — fail-closed RoleAdmin default.
//   Roles: viewer < operator < admin; unrecognized ranks 0.

// cmd/apiaryctl (new)
var allowed = map[string]tier{          // tier 0, 1, or forbidden
    "/apiary.rpc.v1.ManagerService/Status":        tierRead,
    "/apiary.rpc.v1.ManagerService/ListVMsLocal":  tierRead,
    "/apiary.rpc.v1.ManagerService/CreateVM":      tierWrite,
    // ... explicitly enumerated; anything absent is forbidden
}
```

`invoke(method, req)` looks up `allowed[method]`; a miss is a hard
error, not a passthrough. There is no code path that reaches
`conn.Invoke` with a method string the table did not name. Two tests
make this real:

1. **Subset test**: every key in `allowed` exists in `ManagerService`'s
   descriptor, and every `allowed` entry's declared role is ≤ what the
   on-disk key's role can satisfy. A rename in the proto fails the build
   here.
2. **Import-boundary test**: walk `go list -deps ./cmd/apiaryctl` and
   fail on `api/internalpb` or `internal/raft`. A contributor who tries
   to reach `Apply` gets a failing test with a message explaining why,
   not a review comment that arrives after merge.

Third guard, at the config layer: the CLI reads `managerd.json` through
`internal/nodeconfig`'s existing `Manager.Load()` — the same package
`cmd/managerd` and `internal/manager/server.go` use — taking only
`RPCAddr` (`rpc_addr`) and `TLSCert` (`tls_cert`) and dropping the
`Config` on the floor. It never reads `internal_token` from `raftd.json`
at all, and never calls `internal/raftdconfig`'s `Manager.Load()` on the
raftd config, which is the only place `InternalToken` is defined. A test
greps the CLI's config struct for any token field and fails on one.

### Packaging

- `SRCS += apiaryctl` so `make build` produces it.
- `INSTALL_SRCS += apiaryctl` so it lands at
  `/usr/local/libexec/apiary/apiaryctl` via the existing `cp -p`/`mv`
  rename.
- `INSTALL_SRCS_FILTERED` unchanged — the CLI has no rc.d script and
  `make update` must not try to restart it. (For reference, `89f21e8`
  changed this variable by removing `managerd`, which put `managerd`
  back into the update-restart set; it did not change `INSTALL_SRCS`.)
- `etc/apiary/cli.key.sample` is **not** shipped: a key sample invites
  copy-paste of a real key into a world-readable file. Only the path and
  the `0600` requirement are documented.
- No symlink into `/usr/local/bin`; the project keeps daemons under
  `libexec/apiary` and a root-privileged tool in `bin` widens the
  accidental-invocation surface.

### Failure modes

| failure | behavior |
| --- | --- |
| `cli.key` missing or not `0600`/root-owned | refuse to start, say which check failed; never fall back to an unkeyed call |
| `managerd` not running | `connection refused`; point at `raftd -status` and `raftd -restore-dry-run` for offline reads. Do **not** start `managerd` |
| managerd serving TLS, `tls_cert` unreadable | fail closed; never fall back to the system pool |
| `--addr` resolves to a non-loopback address | hard error naming the ADR-0113 reason; v1 has no remote mode |
| key present but rejected | `not-observed (unauthorized)`; never a retry loop |
| follower rejects a leader-only call | print `not-observed (requires leader)` plus the hint; do not follow it; exit non-zero |
| quorum-less Colony | distinguish `no quorum` / `not leader` / `applied` as three outcomes and three exit codes |
| preview cannot determine an object | that object is listed under `unknown`, and the confirmation hash covers it — the operator sees the uncertainty, the hash still binds |
| audit log unwritable | refuse the mutation. A destructive operation with no record does not proceed |
| unknown field anywhere + `--strict` | non-zero exit, with the count and paths of the unknowns |

## Test plan

**On macOS / `go test ./...` (the CI gate is `go vet` on ubuntu-latest;
these run locally and in CI):**

- `allowed` is a strict subset of `ManagerService`, and every entry's
  role requirement is satisfiable by the on-disk key's role.
- Import-boundary test: `go list -deps ./cmd/apiaryctl` contains neither
  `api/internalpb` nor `internal/raft`. Also assert the CLI source
  contains no `raftd.sock` dial target and no `internal_token`
  reference.
- Confirmation-phrase derivation: same operation + same preview content
  → same phrase; any change to the preview (one more VM) → different
  phrase; a wrong phrase, a bare `yes`, and `--force` are all rejected.
  Assert `--force` is not a recognized flag at all.
- Preview correctness against fakes: the preview names exactly the
  identities the mutation would touch, and a `unknown` classification
  flows into the hash and into the `unknown` output section.
- Evidence rendering: every tri-state renders correctly, `unknown` never
  serializes to `false`/`0`/`""`/absent in JSON, `*_observed` booleans
  mirror the text, provenance tags survive `--json`, and `--strict`
  exits non-zero iff at least one `unknown` was rendered.
- Follower/no-leader/no-quorum error mapping produces the three distinct
  exit codes and never exits 0.
- Destructive gate unit tests against a fake `ManagerService`: no Tier 1
  method is invoked without a matching phrase and a prior preview in the
  same process; audit-log write failure blocks the call.

**Only on brood (10.90.0.94) / drone (10.90.0.95) — bare-metal FreeBSD,
where the timing and privilege behavior is real.** macOS cannot
establish any of this; there is no ZFS, no HAST, no PF, no jail, no
bhyve, and no real-network behavior locally.

- Run `apiaryctl` as root on a real Comb with managerd live on
  `127.0.0.1` serving a real self-signed cert from `NODE_TLS_DIR`, and
  confirm the local `caFile` pin works with no egress.
- Confirm `/usr/local/etc/apiary/cli.key` at `0600` root-owned is
  refused when chmod'd `0644` or chown'd to a non-root user.
- `raftd -status` through the CLI's `raft status` with raftd
  **stopped**, and confirm the timeout path returns the offline-running
  error rather than blocking.
- Force quorum loss (stop two of three voters) and confirm the CLI
  reports `no quorum` for writes and still serves Tier 0 local reads
  with `source: local-fsm`, and that `--strict` exits non-zero.
- Call a leader-only RPC through the CLI from a follower and confirm no
  forwarded connection leaves the host (packet capture on the LAN
  interface) — the ADR-0113 deferral, empirically.
- Real ZFS/HAST-visible fields (`apiaryctl storage hast`) with and
  without a role, to confirm `not-applicable` and `unknown` render
  correctly against real evidence.
- Kill `frontend` and `restshimd` and confirm the full Tier 0 surface
  still works — the actual motivating scenario.
- `make install` then confirm `apiaryctl` is present and that `make
  update` did **not** attempt to restart it.

## Open questions

1. **Should the CLI be usable by a non-root operator?** Today it
   requires root only to read `cli.key`. A `wheel`-group-readable key
   (mode `0640`) with the same role limit would let a second admin work
   without `sudo`, at the cost of every `wheel` member being able to use
   the key. My inclination is root-only in v1, matching ADR-0127's
   "root-only" wording, but this is a real operational choice.
2. **Should the audit record become a replicated log?** D5's local file
   is honest about its limits but is lost with the node. A replicated
   audit message in `api/internalpb/state.proto` is a clean fit for the
   existing FSM and would make CLI actions visible cluster-wide; it is
   arguably its own ADR, and this one deliberately does not assume it.
3. **Should `CreateAPIKey`/`RevokeAPIKey` be untiered?** Rotation
   currently requires the browser, which is a real chicken-and-egg when
   the browser is what is broken. Moving them to Tier 1 would let an
   operator rotate the CLI's own credential from the CLI, at the cost of
   a terminal that can mint credentials. I lean yes with the D5 gating;
   the user's call.
4. **Is `apiaryctl raft status` the right shape?** It shells out to
   `raftd -status` and passes JSON through. Alternatively the CLI could
   omit it and the docs could just tell operators to run `raftd -status`
   directly — one fewer subprocess and one fewer thing to test, at the
   cost of a worse experience.
5. **Does Tier 1 need a non-terminal path at all?** If the answer is
   "no, the browser is the place for mutations", the CLI could ship
   read-only and stay permanently safe, at the cost of solving none of
   ADR-0127's recovery cases.
6. **What is the real air-gap courier workflow?** ADR-0127 sketches
   `courier prepare|sync|verify` and ADR-0131 is the backup substrate,
   but whether the CLI's role is to *drive* the courier or merely to
   *verify* the resulting archive (via `raftd -restore-dry-run`) is
   unspecified, and the answer determines whether the CLI ever handles
   bulk data.

## References

- `docs/adr/0001-raftd-process-split-and-uds-protocol.md` — the process
  split and UDS protocol whose separation a CLI Raft-write path would
  undo
- `docs/adr/0023-api-key-authentication.md` — the bearer API key model
  the CLI reuses
- `docs/adr/0030-tiered-rbac-pam-login.md` — viewer/operator/admin tiers
- `docs/adr/0038-tiered-reset-cli.md` — tiered destructive CLI
  convention
- `docs/adr/0051-raftd-config-save-restore.md` — `-export` archive
  format
- `docs/adr/0056-evidence-aware-health-v1.md` — unknown is not false
- `docs/adr/0103-action-preflight-guardrails.md` — preflight before
  consequential action
- `docs/adr/0113-tls-trust-prompt-on-join.md` — the trust-prompt problem
  a CLI cannot prompt its way out of, and why v1 is loopback-only
- `docs/adr/0115-automatic-peer-tls-hostname-map.md` — peer hostname
  resolution
- `docs/adr/0116-quorum-safe-raftd-restart.md` — quorum-safe restart
- `docs/adr/0118-why-not-quorum-blocker-detail.md` — no-quorum is not a
  fault verdict
- `docs/adr/0120-config-rationale-history.md` — replicated
  `change_rationale` as the surviving audit hook
- `docs/adr/0122-cluster-evidence-aware-health-api.md` — evidence-aware
  cluster reads
- `docs/adr/0123-colony-leader-indicator.md` — leader identity
- `docs/adr/0124-raftd-offline-status.md` — the read-only offline
  posture model
- `docs/adr/0127-sylve-io-features.md` — §9 and Phase 4, the origin of
  this ADR
- `cmd/raftd/main.go` — `-status`, `-status-json`, `-reset`, `-export`,
  `-restore`, `-restore-dry-run`, `resetConfirmPhrase`,
  `restoreConfirmPhrase`, `TokenUnaryInterceptor`
- `internal/manager/auth.go` — `Role`, `Satisfies`,
  `extractBearerToken`, `checkAuth`, `requiredRoleFor` fail-closed
  default
- `internal/tlsdial/tlsdial.go` — `ManagerDialOption`
- `internal/raftdconfig/manager.go` — raftd's config load/save,
  permission tightening, and the `InternalToken` field the CLI must
  never read
- `internal/nodeconfig/manager.go` — `managerd.json`'s `Manager.Load()`,
  `RPCAddr`, `TLSCert`
- `internal/manager/server.go` — managerd's external gRPC wiring and
  socket path conventions
- `api/rpc/manager.proto` — `ManagerService`, `leader_hint`,
  `ListVMs`/`ListVMsLocal`, `ListJails`/`ListJailsLocal`,
  `GetLocalNetworkBridgeStatus`, `PreflightRestartNodeService`
- `api/internalpb/raftd.proto` — `RaftInternal`, `Apply` (never
  reachable from the CLI)
- `Makefile` — `SRCS`, `INSTALL_SRCS`, `INSTALL_SRCS_FILTERED`,
  `update`, and the `apiaryinstall` exclusion comment
