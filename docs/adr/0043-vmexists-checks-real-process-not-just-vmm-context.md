# ADR-0043: `VMExists` checks the real process, not just the kernel context

## Status

Accepted

## Context

`internal/bhyve.Manager.VMExists` - the only signal
`internal/cluster`'s `Reconciler.ensureVM` uses to decide "is this VM
already running, or does it need to be (re)created" - checked only
whether the kernel `vmm(4)` context (`/dev/vmm/<name>`) existed.

`bhyve` itself doesn't destroy that context on every exit: a
guest-requested reboot makes the `bhyve` process exit while
*deliberately* leaving its `vmm(4)` context allocated - the standard
`bhyve` wrapper-script contract is that a caller notices the exit code
and either re-execs `bhyve` against the same still-allocated context
(to actually perform the reboot) or explicitly destroys it (for a real
poweroff). `internal/cluster`'s `Reconciler` has no such wrapper at
all - `CreateVM` runs `bhyve` once, detached via `daemon(8)`, and
nothing ever notices if that process later exits for any reason (a
guest reboot, a crash, anything).

Found live during the ADR-0042 investigation: a test VM's `bhyve`
process exited after its first boot attempt (the underlying cause
wasn't determined - possibly an early guest-side reboot, unrelated to
ADR-0042's own bug). `VMExists` kept reporting it as running
indefinitely, since `/dev/vmm/<name>` was still present, and
`ensureVM` treated that as "nothing to do" every tick from then on.
The VM never actually executed again until the stale context was
destroyed by hand (`bhyvectl --destroy`) to force a fresh relaunch.
This is a real reliability gap independent of any specific bug: *any*
VM whose `bhyve` process exits for *any* reason, without something
external destroying its context first, gets silently and permanently
abandoned by the reconciler while still being reported as healthy.

## Fix

`VMExists` now checks both the kernel context *and* whether the
`bhyve` process `CreateVM` actually launched is still alive, via a new
`processAlive` helper reading `CreateVM`'s own recorded pidfile (the
same `-p` argument already passed to `daemon(8)`) and checking it with
a signal-0 existence probe (`os.FindProcess` + `Process.Signal(syscall.
Signal(0))`) rather than trusting the pidfile's mere presence.
Confirmed live which pid actually ends up in that file: `daemon(8)`
without its own `-r`/`-R` restart flags (unlike `managerd`/`frontend`/
`restshimd`'s own service supervision, which does use them) writes the
pidfile with `bhyve`'s *own* pid directly, not a separate supervisor's
- `ps` still shows a parent `daemon` process hanging around alongside
it, but the recorded pid is `bhyve` itself, so checking it is a direct
check, not a proxy.

When the kernel context exists but the process doesn't, `VMExists` now
tears the whole stale VM down by calling `DestroyVM` itself, rather
than only destroying the `vmm(4)` context - see "Verification" below
for why a narrower, `bhyvectl`-only first attempt at this wasn't
enough.

## Verification

`processAlive` is pure Go (file read + a signal-0 syscall, no exec
involved) and is unit-tested directly, without needing real hardware:
no recorded pidfile, a genuinely running process (the test process's
own pid), an exited process (a real short-lived child run to
completion first, so its pid is guaranteed gone), and unparseable
pidfile content. The kernel-context/`bhyvectl`-destroy half of
`VMExists` is exec-based and follows this package's own established
convention (per its existing integration tests) of real-hardware
verification rather than a mocked test harness.

**First attempt at the live fix was itself caught by testing it for
real**: destroying only the stale `vmm(4)` context (not the whole VM's
state) left the VM's separate detached serial-log reader process still
running from the dead launch - the next `CreateVM` attempt then failed
outright colliding with that reader's own `daemon(8)` pidfile
("process already running"), confirmed live via the exact repeated
failure in `managerd`'s own log every reconcile tick. Fixed by having
`VMExists` call `DestroyVM` itself (which already knows how to tear
down every piece of a VM's state atomically) instead of a narrower,
incomplete `bhyvectl`-only cleanup.

Live-verified end to end on `apiverse` with the corrected fix: created
a real VM, waited for it to boot, then `kill -9`'d its actual `bhyve`
process directly (not the `daemon(8)` supervisor - the real-world
equivalent of a guest-triggered reboot or crash) while leaving the
`vmm(4)` context allocated, reproducing the exact bug scenario found
during the ADR-0042 investigation. Confirmed: the reconciler's next
tick noticed the process was dead, tore down the *entire* stale VM
(context, serial logger, no leftover-reader collision this time), and
relaunched it automatically with no manual `bhyvectl --destroy`
intervention - the guest booted again cleanly, reaching a real
`capi-mgmt-tools` login prompt, with ADR-0042's `-H` flag fix's low
idle CPU intact throughout.
