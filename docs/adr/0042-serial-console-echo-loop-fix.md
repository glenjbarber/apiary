# ADR-0042: fix the bhyve serial console echo-loop bug

## Status

Accepted

## Context

CLAUDE.md carried a long-standing "known issue, unresolved after
extensive testing, likely host/bhyve-level" bug: a Linux guest VM
(first hit via the CAPI provider's `capi-mgmt-tools` tooling VM on
`apiarium`) would reliably boot into a sustained high host-side CPU
state proportional to its vCPU count, with its serial console flooded
by bare newline bytes. Four specific hypotheses (a corrupt resized GPT
backup header, host vCPU scheduling contention, disk size/I/O
characteristics, a multi-vCPU TSC synchronization bug) had each been
individually disproven by direct experiment, leaving the actual cause
unknown.

This session set out to test the user's hypothesis that it might be
specific to `apiarium`'s own hardware (a Sandy Bridge-era i7-2700K) by
enabling bhyve on a second, genuinely different machine (`apiverse`,
Skylake i7-6700, confirmed real bare metal) and reproducing the exact
same base image/seed combination there. It reproduced identically -
disproving the hardware hypothesis, but not yet explaining the actual
cause.

## Root cause

Reading the full captured serial log from the `apiverse` reproduction
showed something the earlier apiarium-only investigation had missed: a
real, repeating `capi-mgmt-tools login:` / `Password:` / `Login
incorrect` cycle, with no username or password ever visibly typed -
i.e. the console wasn't frozen or panicked, it was receiving a
continuous stream of bare `Enter` keystrokes at whatever prompt
happened to be active. That is the literal, mechanical explanation for
"flooded with bare newline bytes": newlines being received as *input*,
not just observed in output.

`internal/bhyve.Manager.startSerialLogger` starts the read side of the
per-VM `nmdm(4)` pair with a plain `daemon(8)`-wrapped `cat
/dev/nmdm<N>A`. A freshly created `nmdm` endpoint is a completely
ordinary tty and defaults to `icanon`+`echo` on (confirmed live via
`stty -f /dev/nmdmXA -a`) - nothing in `startSerialLogger` ever put it
into raw mode. `nmdm`'s two endpoints are cross-wired like a null
modem: whatever the guest (attached to the "B" side via bhyve's `com1`)
writes arrives as *input* on the "A" side our reader opened. With echo
on, the kernel's own tty layer echoes every byte it receives on "A"
straight back out through "A" - which, because of the null-modem
wiring, lands right back as *input* on "B", i.e. back into the guest,
indistinguishable from real keystrokes.

This was confirmed directly, live, independent of bhyve or any guest
kernel entirely: creating a plain `nmdm` pair and writing a single
`"hello_from_guest\n"` to one end produced an immediate, self-sustaining,
unbounded flood of newlines on the reading end, with zero further input
- pure FreeBSD tty-layer behavior. It explains every previously
mysterious symptom at once:

- **Sustained high CPU, scaling with vCPU count**: the guest's `login`/
  `getty` churns continuously processing an endless stream of bogus
  `Enter` presses.
- **Console flooded with bare newlines**: literally the `\n` half of
  the guest's own boot/prompt output looping back as input.
- **No `watchdog: BUG: soft lockup` kernel message** (present on
  `apiverse`'s reproduction, and apparently not reliably present on
  every `apiarium` run either): the CPU isn't stuck in one context
  without scheduling - it's legitimately, repeatedly context-switching
  through real (if pointless) `getty`/`login` process churn, so the
  soft-lockup detector has no reason to fire.
- **Hardware-independent**: confirmed identical on two completely
  different CPU generations, because the actual mechanism has nothing
  to do with the CPU at all - it's a host-side tty configuration gap,
  introduced the moment anything opens `/dev/nmdm<N>A` without first
  disabling its default line discipline.
- **Only became visible after ADR-0032** (bhyve serial console log
  capture): before that ADR, nothing ever opened the "A" side of any
  VM's `nmdm` pair at all, so there was no reader whose default-echoing
  tty could start the loop. ADR-0032 is what introduced the reader -
  and, with it, unknowingly, this bug.

## Fix

Getting this right took three attempts, each disproven by a real,
direct re-test rather than assumed correct - worth recording, since the
mechanism is subtle enough that a plausible-looking fix can still be
wrong:

1. **First attempt**: run the reader through a shell so `stty -f
   <device> raw` (as its own, separate invocation) could run before
   `cat <device>` (a second, separate invocation) took over. This
   *looked* right but a live re-test showed the device was still
   `icanon`+`echo` on right after boot - the fix hadn't taken effect at
   all.
2. **Second attempt**: reasoned that `CreateVM` launches `bhyve` (which
   starts writing boot output to the "B" side immediately) *before*
   `startSerialLogger` ever runs - so the echo loop could already be
   underway before our `stty` call ever executed. Reordered
   `startSerialLogger` to run first. Still no effect on a live re-test.
3. **Root cause, found by manually reproducing each step by hand**: the
   device's own `cflags` include `hupcl` (hang up on close), and
   `nmdm(4)` genuinely honors it. `stty -f <device> raw` opens the
   device as *its own*, separate file descriptor, sets raw mode, then
   *closes that fd as the process exits* - and that close, given
   `hupcl`, resets the endpoint's termios straight back to defaults
   before `cat`'s own, later, *different* open() ever happens. Setting
   raw mode and reading are two different file descriptors under the
   naive shell-and/&&-two-commands approach, no matter which order they
   run in.

The actual fix has `stty` and `cat` share the exact same open file
description throughout, by redirecting the device onto the shell's own
stdin *once* and having both commands act on inherited stdin rather
than opening the device themselves:

```go
device := fmt.Sprintf("/dev/nmdm%dA", nmdmUnit)
runCmd(ctx, "daemon", "-f", "-p", pidfile, "-o", logfile,
    "/bin/sh", "-c", fmt.Sprintf("{ stty raw && exec cat; } < %s", device))
```

Since the device is opened exactly once (by the shell, for the `{ }`
compound command's redirection) and never closed until the whole reader
exits, there's no intermediate close to trigger a `hupcl` reset between
setting raw mode and reading. `startSerialLogger` was also kept running
before `bhyve` itself launches (from attempt 2 above) - not itself
sufficient on its own, but still the correct order, and now paired with
a proper `stopSerialLogger` helper (extracted from `DestroyVM`'s own
teardown, now shared) so a `CreateVM` failure after the reader already
started cleans it back up rather than leaking it.

No existing unit test asserted the exact command shape here (this
package's convention, per its own integration tests, is real-hardware
verification for anything that actually shells out - `internal/bhyve`
has no exec-mocking test harness at all), so every attempt above,
including the two that turned out wrong, was verified live rather than
assumed correct from reading the code.

## Verification

Live-verified on `apiverse`, end to end, for the exact configuration
that previously reproduced the bug (`base_image_name=ubuntu-cloud.raw`,
`iso_name=tools-seed.iso`, `vcpus=1`): `stty -f /dev/nmdm<N>A -a`
confirmed `-icanon -echo` immediately after boot (previously stuck on
`icanon echo`) and **stayed that way** - no reset. The guest booted
cleanly with a completely quiet console: real package installation
(`docker.io`, `containerd`, etc.) actually running (accounting for
legitimate, expected CPU use during that window, not a stuck loop),
cloud-init generating real SSH host keys and installing the seed's
`authorized_keys` for the `ubuntu` user, taking its NoCloud seed's
`capi-mgmt-tools` hostname, and reaching `cloud-init.target` cleanly at
"Up 39.35 seconds" - after which the serial log stopped growing
entirely (confirmed static across a 5-second recheck), rather than the
previous unbounded newline flood. A literal SSH login wasn't attempted
(no local copy of the seed's matching private key), but every other
piece of the previously-established "real, working boot" bar was
directly confirmed.

Separately noted during this investigation (unrelated to the echo-loop
bug itself, but fixed in the same pass once flagged): `bhyve` was
invoked with no `-H` (yield-the-vCPU-thread-on-guest-HLT) flag anywhere
in `internal/bhyve`, so `top` kept showing a VM's vCPU thread near 100%
even once the guest was genuinely idle post-boot - a real, minor tuning
gap every VM this project created already had, independent of this
bug. Fixed by adding `-H` to `CreateVM`'s bhyve args (a single,
purely-additive flag). Live-verified on `apiverse`: recreating the
identical test VM, the `bhyve` process settled to 1.46% CPU once idle
post-boot, down from ~100% before the flag was added - cloud-init
completing identically either way, confirming the flag has no
guest-visible effect, only host CPU accounting.
