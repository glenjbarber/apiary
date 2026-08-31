# ADR-0032: bhyve serial console log capture

## Status

Accepted

## Context

Found while completing the Kubernetes CAPI provider's (`cluster-api-provider-apiary`) live network verification: a real VM booted, got a real DHCP lease, and was pingable, but SSH never came up and the guest kept its default hostname - meaning cloud-init never consumed the NoCloud seed data. The noVNC console (ADR-0020) showed nothing informative: many cloud/server images (this project's Ubuntu 24.04 base image included) redirect console output to the serial port (`com1`) rather than the VGA/EFI framebuffer VNC shows, so a black screen past firmware handoff is expected either way and proves nothing.

Diagnosing this required actually seeing what the guest printed during boot - cloud-init's own log messages in particular. bhyve has no built-in way to persist `com1` output to a plain file; the real backends are `stdio` (only usable for a foreground process, not this project's always-detached VMs), a host TTY device, or `tcp=ip:port` (an interactive session, not a log). The standard bhyve pattern for capturing serial output from a detached VM is an `nmdm(4)` null-modem pseudo-terminal pair: the guest gets one end, a separate reader process drains the other end continuously.

## Design decisions

- **`bhyve.Config` gains `EnableSerialLog bool`**, mirroring `EnableVNC` exactly - always on when bhyve provisioning is enabled at all (`internal/cluster`'s reconciler), not a separate opt-in flag, for the same reasoning ADR-0020 already gave for VNC: there's no scenario where the tradeoff runs the other way.
- **An `nmdm(4)` pair, not a real TTY device or TCP socket.** The VM's `com1` attaches to one end (`-l com1,/dev/nmdm<N>B`); a small detached reader - started via `daemon(8)`, the same tool `CreateVM` itself already uses to detach bhyve - opens the other end (`/dev/nmdm<N>A`) and `daemon`'s own `-o <logfile>` redirects its `cat`'d output straight into a plain file. No new dependency, no custom log-draining code.
- **Unit allocation mirrors VNC port allocation exactly**: `allocateNmdm` scans `RunDir` for `*.nmdm` sidecar files recording units already in use (same pattern `allocateVNCPort` already established), rather than inventing a second bookkeeping scheme.
- **The log file survives `DestroyVM`; the live-state tracking files don't.** `nmdmfile`/`serialpidfile` are removed on teardown (they only track a *live* allocation, freeing the unit for reuse and stopping the reader process) - but `seriallogfile` is deliberately left behind. A failed VM's boot log is exactly the thing you want to still have *after* it's torn down for diagnosis, not something cleaned away with everything else.
- **`SerialLogPath(name)` is a plain file-path lookup**, mirroring `VNCPort`'s own `(value, ok, err)` shape. No new RPC or web UI page was added in this pass - an operator (or `internal/cluster`'s reconciler, in a future pass) reads the file directly on the node. Building a full remote-viewing feature (matching noVNC's own RPC+proxy+page shape) is real, tracked follow-on work, not attempted here to keep this pass scoped to what was actually needed: diagnosing one specific problem.
- **Requires the `nmdm.ko` kernel module**, which is *not* loaded by default on this project's own FreeBSD hosts (confirmed live - unlike `tap(4)`, which apparently is). A real, disclosed one-time host prerequisite, the same posture as `dnsmasq`/`pf` in ADR-0022: `kld_list="nmdm"` in `/etc/rc.conf` for persistence, `kldload nmdm` once for the current boot. The first live attempt hit this directly - the reader process started and immediately failed (`cat: /dev/nmdm0A: No such file or directory`), silently leaving a 44-byte log file that looked superficially like "nothing happened" rather than an obvious startup failure.

## The real bug this feature immediately found

With the serial log actually working, the root cause of the original problem became visible in seconds: cloud-init's own log showed

```
DataSourceNoCloud.py[WARNING]: device /dev/sr0 with label=cidata not a valid seed.
```

cloud-init found the seed CD-ROM and its `cidata` volume label correctly, but rejected it outright. Inspecting the actual ISO (via `strings`/mounting it) showed why: `internal/nocloud`'s `BuildSeedISO` (the CAPI provider's NoCloud seed builder, ADR-0031) finalized its ISO9660 filesystem with no Rock Ridge extensions - plain ISO9660's 8.3 naming rules silently mangled `user-data`/`meta-data` into `USER_DATA`/`META_DATA` (uppercase, hyphen stripped), which cloud-init's datasource check requires exactly, case-sensitively, hyphen included. Fixed by setting `RockRidge: true` in `go-diskfs`'s `FinalizeOptions` (`cluster-api-provider-apiary`'s own repo, not this one) - confirmed live end-to-end afterward: a real VM booted, took on its seed's custom hostname, ran a custom `runcmd`, and accepted SSH via its seed's injected key, all for the first time.

This is exactly the kind of failure a VNC screenshot could never have caught (the image's console output was never rendered to the framebuffer in the first place) - concrete justification, after the fact, for building this feature at all.

## Consequences

- Full unit test coverage for the new bookkeeping (`allocateNmdm`/`SerialLogPath`), mirroring the existing VNC tests exactly - no real hardware needed, same as those.
- No dedicated real-hardware integration test was added for `EnableSerialLog` itself, matching how `EnableVNC` was never covered by one either (both are live-verified manually, not via `internal/bhyve/integration_test.go`).
- A new host prerequisite (`nmdm.ko` loaded, ideally via `kld_list` in `/etc/rc.conf`) joins the existing list (ZFS pool, `bhyve-firmware`, uplink NIC name, `dnsmasq`, `pf`) - not something Apiary loads or verifies itself.
- Remote viewing of a serial log (a web UI page, an RPC) remains unbuilt - real, disclosed follow-on work, the same posture ADR-0020 took before noVNC's own RPC+page were built.
