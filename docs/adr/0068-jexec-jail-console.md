# ADR-0068: jexec-based interactive jail console

## Status

Accepted

## Context

The user asked for a `jexec`-based console for jails, "similar to the
noVNC console for VMs" (ADR-0020/ADR-0065). Unlike a VM's VNC
framebuffer, there is no existing listener to dial for a jail - the
only way to get an interactive shell inside one is to have the owning
Hive's own managerd spawn `jexec(8)` itself and relay its input/output.

This is genuinely new, higher-privilege attack surface, not a small
addition: `jexec` grants a real root shell that shares the *host's own
kernel* (a jail has no hardware-level isolation boundary the way a VM's
hypervisor does). It is treated with that seriousness throughout this
design, not folded in as a minor variant of the VM console.

## Decision

**Operator-tier, not Viewer-tier.** `ProxyVMConsole` is Viewer-role
because viewing a VM's console is read/write access to that VM's own
guest, isolated from the host by hardware virtualization.
`ProxyJailConsole` requires Operator - one tier stricter - because a
jail's root shell is a direct, kernel-shared host-level actor. This is
a deliberate asymmetry with the VM console, not an oversight.

**A real PTY, not plain pipes.** A jexec session without a PTY would
have no working line editing, job control, or signal handling
(Ctrl-C/Ctrl-D) - genuinely painful to use, the same reasoning ADR-0020
needed a real VNC framebuffer rather than some cheaper approximation.
Added `github.com/creack/pty` (MIT, widely used, real FreeBSD support)
as this project's second-ever third-party runtime dependency beyond
gorilla/websocket - justified the same way that one was: nothing in
the standard library does this portably and correctly.

**The executed command is always a fixed shell (`/bin/sh`), never
caller-influenced.** Accepting an operator-supplied command here would
be a second, redundant way to run arbitrary commands with no
additional capability, and a real place for injection bugs to hide.
`internal/jail.Manager.Attach` hardcodes it.

**No proto-level `GetJailConsole` the way `GetVMConsole` exists for
VMs.** A VM's VNC "availability" is a real fact to check before
proxying (`VNCPort` either has a recorded port or doesn't). A jail has
no equivalent liveness signal cheaper than actually trying `jexec` -
inventing one would mean either a redundant `JailExists` round trip or
a fabricated always-true response. `ProxyJailConsole`'s own ownership
check reuses the already-forwarding-aware `GetJail` RPC (mirroring how
`ProxyVMConsole` delegates to `GetVMConsole` for the same reason), and
`internal/frontend`'s page-load pre-check is a plain existence check
only - the real "is the jail actually running" answer comes from
attempting the WebSocket connection itself, surfaced as a real error if
it fails.

**Structurally mirrors `ProxyVMConsole`/ADR-0065 wherever the shape
actually matches**: same first-frame-opens-the-resource pattern, same
ownership re-validation independent of the caller's claim, same
local-vs-peer routing in `internal/frontend`
(`jailConsoleOwner`/`openJailConsoleTunnel` mirror
`consoleOwner`/`consoleInfoForVM` almost exactly), and the byte-pumping
WebSocket relay (`proxyConsole` in console.go) is reused verbatim - it
has no VNC-specific logic at all, it just pumps opaque bytes between
any `io.ReadWriteCloser` and a WebSocket.

**`SetJailConsole` is a post-construction setter on `manager.Server`,
not a `NewServer` parameter** - following the exact precedent
`SetAssumptionRegister` already established in this same file, to keep
the already-long constructor signature stable across managerd's many
focused tests. Wired unconditionally in `cmd/managerd/main.go`
(independent of `-jail-enabled`), the same "inspection/teardown always
available" posture ADR-0064 established for jail lifecycle operations
- console access to an already-running jail is not "provisioning."

**Frontend terminal: vendored xterm.js (MIT), not a custom widget.**
The user's own comparison to noVNC implied real terminal fidelity was
wanted, not a crude command box that mangles anything using cursor
movement (`top`, `vi`, shell history). Vendored unmodified under
`web/static/xterm/` (JS + CSS + LICENSE), loaded as a plain `<script>`
tag (it's a UMD bundle, not an ES module) - mirrors htmx.min.js's own
vendoring/loading convention exactly, the same way noVNC/pako already
established the pattern for a different kind of widget.

## v1 scope limits (disclosed, not silently absent)

- **No window-resize support.** The PTY is a fixed 80x24 for the whole
  session. A future version could add a resize control-frame type to
  `JailConsoleTunnelFrame` and call `pty.Setsize` server-side.
- **No credentials/encryption on the underlying session beyond the
  existing login gate** - same posture ADR-0020 already discloses for
  the VNC console.
- **One session at a time per jail is not enforced.** Two operators
  attaching simultaneously both get a real, independent `jexec`
  session (FreeBSD itself allows concurrent `jexec` calls) - this can
  be confusing but isn't unsafe; not solved here.
- **PTY read-error-after-exit handling is platform-lenient, not
  platform-verified.** A well-known Linux quirk returns `EIO` (not
  `io.EOF`) once a PTY's slave side closes; this project has not
  independently confirmed FreeBSD's exact behavior. `ProxyJailConsole`
  treats *any* read error as a clean session end rather than guessing
  which platform-specific error to special-case - see its own code
  comment.

## Verification

New tests at every layer: `internal/jail` (`TestProtectedJailRejectedBeforeExec`
extended to cover `Attach`; a real, skippable `TestIntegration_Attach`
that creates a real jail, attaches, runs a real command through the
PTY, and confirms `Close` actually terminates the process - not just
that it returns without error); `internal/manager`
(`TestIntegration_ProxyJailConsole_RelaysOnlyOwnedJail`/`RejectsWrongOwner`/
`NoJailConsoleConfigured`, all against a real raft harness with real
leader election, mirroring the VM console's own integration tests
exactly); `internal/frontend` (`TestServer_JailConsolePage_*` for the
page, and `TestServer_JailConsoleWS_ProxiesBytesBothWays` - a real
WebSocket client through the full local relay path against a fake
echoing `ProxyJailConsole` stream, proving the whole chain rather than
asserting it). `go build`/`go vet`/`go test ./...`/`gofmt -l`/
`git diff --check` all pass, and the FreeBSD cross-compile for
managerd/raftd/restshimd succeeds. Native FreeBSD live verification
(a real `jexec` session through a real browser) was not performed as
part of landing this - see the "Deferred" note below.

## Deferred

Live verification against a real jail on `apiarium`/`apiverse` (a real
browser terminal, real line editing, real Ctrl-C/Ctrl-D, a real
concurrent-session check) has not been performed as part of this
change and should happen before this is treated as fully proven in
production, the same way ADR-0020's own noVNC console was live-verified
separately from its initial implementation.
