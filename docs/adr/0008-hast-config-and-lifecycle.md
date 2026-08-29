# ADR-0008: internal/hast config rendering and lifecycle

## Status

Accepted. As of 2026-08-29, the replication issue is a known, diagnosed
upstream FreeBSD bug (not this project's environment), and its fix
([D57511](https://reviews.freebsd.org/D57511)) has been independently
built and confirmed to actually work (real data replication verified,
not just status). Not yet merged/released upstream — patched nodes
currently require a manual source build.

## Context

`internal/hast` is the third FreeBSD-specific package. Unlike
`internal/zfs`/`internal/jail`, HAST's control surface isn't just "run a
CLI command" — it's driven by a shared config file (`hast.conf`)
describing both nodes of a replicated resource, plus `hastctl` for
runtime role changes. This package renders that config and drives
`hastctl`; getting it working against the two real project VMs surfaced
real operational behavior worth recording, and one real problem that
isn't resolved yet.

## Decisions

### Config rendering is a pure function, not tied to hastctl

`RenderConfig([]Resource) (string, error)` takes typed `Resource`/`Node`
structs and produces `hast.conf` text with no shelling out at all —
fully unit-testable without any HAST tooling present, unlike everything
else this package does. `Manager.WriteConfig` is a thin wrapper that
renders and writes to `Manager.ConfigPath` (or `DefaultConfigPath`,
`/etc/hast.conf`).

### This package does not manage the `hastd` service

Same boundary `internal/zfs` draws around the ZFS kernel module's load
state: starting/stopping/restarting `hastd` itself is a system-level
(`rc.conf`/`service`) concern, not something `Manager` does. Callers
(and this package's own tests) are responsible for restarting `hastd`
after deploying a config change.

**This boundary surfaced a real gotcha while building this slice**:
`hastd` does not hot-reload `hast.conf` — a resource added to the file
after `hastd` is already running is invisible to it until restarted.
`hastctl create <name>` misleadingly appears to succeed regardless (it
operates on the local provider's on-disk metadata directly, not through
the running daemon), which meant an early debugging session chased a
phantom `hastctl role primary` failure ("Received error 1 from hastd")
that was actually just a stale, un-reloaded config — not a bug in this
package or its command sequence. Documented here so it isn't
rediscovered: **always restart `hastd` after `WriteConfig` and before
`CreateResource`/`SetRole` on a newly added resource.**

### `Status` parses `hastctl list`, not `hastctl status`

`hastctl list` gives verbose `key: value` lines per resource (easy to
parse robustly); `hastctl status` gives an aligned/columnar table
intended for human/quick reading, not something worth writing a
column-width-aware parser for. `parseStatus` extracts only the fields
this package currently needs (`role`, `localpath`, `remoteaddr`,
`status`) and ignores the rest (statistics, extent size, etc.).

### `ResourceStatus` is allowed to be empty

A secondary's `hastctl list` output has no top-level `status:` field at
all (only a primary reports its sync health this way). `Status.ResourceStatus`
is `""` in that case rather than an error — this is normal, not a
parsing failure.

## Open issue: cross-node replication does not reach "complete"

Setting up a real two-node resource between `freebsd-apiary` (10.50.0.11) and
`freebsd-apiary2` (10.50.0.12) — matching `hast.conf` on both, `hastctl
create` on both, `secondary` role on one then `primary` on the other —
consistently leaves the primary's status as **degraded**, with
`hastd`'s own syslog repeating:

```
hastd[PID]: Unable to receive header from tcp://<peer-ip>:<ephemeral-port>: Operation timed out.
```

Ruled out while debugging:
- **Firewall**: `pf` isn't even enabled on these VMs.
- **Basic connectivity**: `nc`/`ping` (including large, 1400-byte
  payloads) between the two VMs work fine, in both directions.
- **Real data transfer**: a plain `nc` listener/sender pair successfully
  exchanged an actual payload — ruling out silent data corruption from a
  broken checksum/segmentation-offload path, a known class of bug in
  some virtualized NIC setups.
- **NIC offload**: disabling `txcsum`/`rxcsum`/`tso`/`lro` on both VMs'
  interfaces made no difference (and was reverted afterward).
- **Role-assignment order**: setting secondary before primary vs. primary
  before secondary made no difference.
- **hastd config-reload gotcha above**: controlled for by always
  restarting `hastd` after config changes in all tests that follow.

Both nodes' `hastd` processes do show sockets connecting to each other
at the OS level (visible in `sockstat`), but the HAST-level protocol
handshake never completes — `hastd` on one side reports it never
received the expected protocol header from the peer. It has **not**
been root-caused.

**Update 2026-08-29 — ruled out a 15.1-specific `hastd` regression.**
A third VM, `freebsd-apiary3`, was upgraded from 15.1-RELEASE to
16.0-CURRENT via `pkg`/pkgbase (a real, verified pkgbase major-version
upgrade — bootstrap the target branch's signing key via `pkg add -f`
on a `FreeBSD-pkg-bootstrap` package, then `pkg upgrade -r
FreeBSD-base` inside a boot environment for safe rollback). Pairing
`freebsd-apiary3` (16.0-CURRENT) with `freebsd-apiary` (15.1-RELEASE)
as a HAST resource reproduced **the exact same failure**: primary
stuck at `degraded`, identical `Unable to receive header ... Operation
timed out.` log line. Since the bug reproduces with one side running a
completely different, much newer `hastd` build, it is **not** a
FreeBSD-15.1-specific `hastd` regression. That leaves something about
this project's specific virtualization/network environment as the far
more likely cause — MAC address collisions between the cloned VMs were
also checked and ruled out (`freebsd-apiary`/`freebsd-apiary2`/
`freebsd-apiary3` all have distinct MACs).

**Update 2026-08-29 (continued) — packet capture and syscall trace point
to a protocol-level deadlock inside `hastd` itself, not the network.**

A `tcpdump` capture on both `freebsd-apiary` and `freebsd-apiary2`
during a fresh `role secondary` / `role primary` sequence shows the
*complete* exchange for the entire ~20-second window before the
connection is torn down:

1. SYN (primary → secondary)
2. SYN-ACK (secondary → primary)
3. ACK (primary → secondary) — three-way handshake completes cleanly
4. **~20 seconds of complete silence — zero data packets in either
   direction**
5. FIN (secondary → primary), ACK (primary → secondary) — secondary's
   `hastd` closes the idle connection once its read timeout expires

So the TCP layer is entirely healthy — this rules out corruption/offload
issues definitively (already suspected ruled out via the earlier plain
`nc` data-transfer test, now confirmed directly on the connection that
actually fails). Neither side ever transmits a single byte of the HAST
protocol handshake after the connection opens.

Restarting the primary's `hastd` with `-d -d -F` (verbose foreground
debug) and re-triggering `role primary` confirms this at the process
level: the debug log shows privilege drop into the capsicum+jail
sandbox, then nothing else for the resource's networking — no
"connecting", no "connected", no error, just periodic unrelated
housekeeping log lines, even while `sockstat` shows the connection to
the peer sitting open. `procstat -t` on the worker process
(`hastd: <resource> (primary)`) shows 8 kernel threads; two of them are
blocked in **`sbwait`** (kernel socket-buffer wait) with no forward
progress. `truss -H` confirms no thread makes any `connect`/`send`/`recv`
syscall during a multi-second trace window — only unrelated periodic
`clock_gettime`/logging activity.

Taken together: the connection opens, and then both sides' worker
threads appear to sit waiting to *receive* something before either one
sends anything — a mutual wait with no timeout-driven retry until the
outer per-connection timeout eventually fires and tears it down. This
reproduces identically on FreeBSD 15.1-RELEASE and 16.0-CURRENT, on a
network already shown to move real data correctly (`nc` test) with no
firewall, MTU, offload, or MAC-collision explanation available. This
now looks like a genuine, reproducible bug or protocol-level deadlock in
`hastd` itself, present across the currently-supported/CURRENT branches
— not an artifact of this project's environment.

**Update 2026-08-29 (final) — this is a known upstream bug, already
diagnosed, with a patch pending review.** Before filing a new report,
searching bugs.freebsd.org (Base System / component `bin`) for existing
`hastd` reports turned up
[bug 292322](https://bugs.freebsd.org/bugzilla/show_bug.cgi?id=292322)
("Hastd failing to g_attach device on FreeBSD 15.0") — an exact match:
same `Unable to receive header ... Operation timed out.` message, same
`g_dev_taste ... failed to g_attach, error=6` kernel log line we also
observed. A community member (Martin Vidovic) root-caused it precisely:
`hastd`'s connection-migration path hands a connected socket from the
parent to its worker over an `AF_UNIX` socketpair, sending the protocol
name via `send(2)` and the descriptor separately via `sendmsg(2)` with
`SCM_RIGHTS`; the receiving `recv(2)` uses `MSG_WAITALL` sized for the
maximum protocol-name length, so on FreeBSD 15+ it blocks waiting for
that full size instead of returning once the (shorter) actual message
arrives — the primary's worker never receives its connection, and so
never sends the HAST header at all. This exactly matches our own
`sbwait`/`truss` findings above.

A patch is up for review: [D57511](https://reviews.freebsd.org/D57511)
("hastd: fix fd passing over socketpair"), sending the protocol name and
descriptor together via a single `sendmsg`/`recvmsg` call. Status as of
2026-08-29: *Needs Review* — a reviewer (`glebius`) confirmed "the fix
looks correct" but asked for a refactor before landing it; not yet
merged. We added a comment to bug 292322 confirming independent
reproduction, including that it still reproduces on 16.0-CURRENT (the
existing report and patch are scoped to "FreeBSD 15"), which is new
information for whoever picks up the fix.

Given a matching report and an already-correct pending patch exist,
filing a new bug would just be noise — the right move is watching
292322/D57511 for the patch to land, not further independent diagnosis.

**Update 2026-08-29 (final) — the patch is confirmed to fix the issue.**
Glen built `hastd` from a source tree with D57511 applied and installed
it on `freebsd-apiary3`. Pairing it as primary against `freebsd-apiary`
(stock, unpatched) as secondary — otherwise the exact same test as
above — reached **`status: complete`** on both sides, matching the
patch's own test plan criteria exactly. A real write through
`/dev/hast/patchtest` confirmed actual data replication, not just a
status flag: the resource's `dirty` counter went `0 → 2.0MB → 0`,
showing the extent was marked dirty, replicated, and cleared normally.
This is real, working replication, not merely an absence of the
degraded symptom.

Only the primary side (`freebsd-apiary3`) needed the patch — consistent
with Martin's diagnosis that the bug is entirely in the primary's own
connection-migration path; the secondary's role never touches the buggy
code. This means once D57511 merges upstream, only nodes acting as a
resource's primary strictly need the fix, though patching all nodes is
still the right long-term move since any node can become primary.

**This package's own tests were verified as still correct once the fix
is available**: the assumption that `hastctl create`/`role`/`status`'s
CLI-driving mechanics are separable from `hastd`'s own wire-level
behavior held up — nothing in `internal/hast` needed to change to work
against a patched `hastd`. The dual-node provisioning guardrail (see
"Decision" section below) should be revisited once the patch is merged
and available via a normal package/release rather than a manually built
binary — `internal/hast`'s replication testing can then move from "not
possible" to "possible with a source build" to eventually "just works
out of the box."

## Consequences

- Anyone revisiting HAST should start from the open issue above rather
  than re-deriving it — the ruled-out causes are the obvious first
  things to check and are already eliminated.
- Any future caller of `WriteConfig` must restart `hastd` afterward;
  this isn't enforced by the package (deliberately — it doesn't manage
  the service), so it's easy to forget. Worth a prominent note wherever
  `internal/hast` eventually gets wired into `managerd` or node
  provisioning.
- Given the open replication issue, ZFS/HAST-backed VM disk provisioning
  (a real future use of this package) can't yet be claimed to work
  end-to-end — only the control-plane half (`internal/zfs` +
  `internal/hast`'s CLI-driving mechanics) is verified.
- **Project-level risk, now bounded**: CLAUDE.md's architecture names
  HAST as the storage-replication mechanism (ADR-0001's physical/
  ephemeral split assumes it works). This was a real risk to that
  decision while the cause was unknown; now that it's a known upstream
  bug with an already-correct patch (D57511) awaiting a minor refactor
  before merge, the risk is bounded to "when does the patch land," not
  "is HAST fundamentally broken." Revisit `internal/hast`'s replication
  testing once D57511 merges and is available on a test system (either
  wait for a release/snapshot that includes it, or build `hastd` from a
  patched source tree sooner if unblocking this matters before then).

## Decision: keep the shim, disable dual-node provisioning for now

Nothing outside this package's own tests calls `internal/hast` yet — no
provisioning flow wires it into `CreateVM` or anything else — so there is
no dual-node code path to disable today. This is a forward-looking
guardrail for when that changes: when VM/dataset provisioning is built
out (`internal/zfs` + eventually `internal/bhyve`), it should provision
**single-node storage only** — do not have that flow call
`internal/hast` to actually configure a live two-node resource pairing
(`WriteConfig` + `CreateResource` + `SetRole` against a real second
node) until D57511 has merged and been verified fixed here. `Manager`'s
API itself is left as-is (no artificial single-node-only restriction
added to the package) since its own tests already exercise it safely
without depending on real replication succeeding — the guardrail belongs
at the call site that will eventually decide *whether* to provision a
replica, not inside this package.

**Update 2026-08-29**: the fix is confirmed working (see above), but
this guardrail stays in place until D57511 actually merges and is
available through a normal package/release rather than a manually built
`hastd` binary on one specific VM (`freebsd-apiary3`). Real dual-node
provisioning depends on every node in the cluster reliably having a
working `hastd`, not on one hand-built binary happening to work in a
test.
