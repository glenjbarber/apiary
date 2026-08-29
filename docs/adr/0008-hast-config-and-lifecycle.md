# ADR-0008: internal/hast config rendering and lifecycle

## Status

Accepted, with one open issue (see below, updated 2026-08-29 with a
finding that narrows it significantly)

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

Next step, not yet attempted: a packet capture (`tcpdump`) on both
sides during a `role primary` attempt, to see what's actually
transmitted (or not) at the point `hastd` claims it never received a
header — this would distinguish "nothing was ever sent" from "something
was sent but arrived corrupted/truncated."

**This package's own tests do not depend on resolving it**: they verify
config rendering, `hastctl create`/`role`/`status` execution, and this
package's output parsing — the actual scope of what `internal/hast` is
responsible for — using a resource pointed at the real peer VM, and only
assert the primary role was set successfully and `Status()` parsed
whatever `hastd` reported (accepting "degraded" as a valid, parseable
outcome). Actually verifying data replication end-to-end (the deeper
promise of HAST) remains blocked on this open issue.

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
