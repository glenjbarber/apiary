# ADR-0034: remote serial console log viewing

## Status

Accepted

## Context

ADR-0032 added serial console log capture but explicitly deferred the remote-viewing half: an operator had to read `internal/bhyve.Manager.SerialLogPath`'s file directly on the owning node. This closes that gap, mirroring ADR-0020's own noVNC console feature shape (RPC + frontend page) as closely as possible, since the two features have near-identical locality constraints.

## Design decisions

- **`GetVMSerialLog` is a plain unary RPC, not a stream.** `GetVMConsole` proxies a live WebSocket because VNC is inherently a continuous framebuffer; a log file has no equivalent need - the frontend page just polls the RPC on a timer (`hx-trigger="every 3s"`, the same pattern `vms.html`'s own VM table already uses), matching the project's existing "poll, don't stream, for anything that isn't truly interactive" posture.
- **Server-side byte cap, regardless of what's requested.** `GetVMSerialLogRequest.max_bytes` is honored but hard-capped at 1MB (`maxSerialLogTailBytes`), with a 64KB default when unset. This is a real, not theoretical, concern - a runaway VM's serial log was observed growing to several megabytes within minutes during this same session's own CAPI tooling-VM debugging, driven by a still-unresolved getty/console-flood bug on that VM. A plain synchronous RPC returning an unbounded tail would let exactly that failure mode turn into an unbounded-response-size problem for `managerd` itself.
- **Same locality limitation as `GetVMConsole`, deliberately not solved here.** `GetVMSerialLog` only ever answers for a VM confirmed running on *this* node - a real multi-node deployment where `internal/frontend` isn't colocated with the VM's owning `managerd` needs node-address discovery this project still doesn't have (the same gap ADR-0020 and ADR-0015 already name).
- **`SerialLogLookup` mirrors `VNCLookup` exactly** - a narrow, locally-defined single-method interface satisfied by `*bhyve.Manager` (already had `SerialLogPath` from ADR-0032), wired through `NewServer`'s existing nil-able-capability parameter pattern. `cmd/managerd` passes the same `bhyveMgr` instance for both `VNCLookup` and `SerialLogLookup` - one bhyve capability, two read-only lookups against it.
- **Content is sanitized with `strings.ToValidUTF8`**, not returned as raw bytes. A serial console can emit non-UTF-8 sequences, and the tail window can start mid-escape-sequence when the log is truncated; a protobuf `string` field must be valid UTF-8 regardless, so invalid bytes are replaced rather than erroring the whole response or switching the API to `bytes`.
- **RBAC: `RoleViewer`**, the same tier as `GetVMConsole`/`ListVMs`/`HostStats` - read-only operational visibility, no write capability implied.
- **Frontend page (`/vms/{id}/serial`) has no `requireRole` wrapper**, matching the read-only console/VM-table pages - any logged-in session can view it, the same posture ADR-0030's role gate only applies to mutating routes.

## Consequences

- Full raft-harness integration test coverage in `internal/manager` (available/unavailable/wrong-node/no-bhyve-configured/unknown-VM/truncation cases), mirroring `GetVMConsole`'s own test suite structure exactly.
- Frontend page and fragment-polling handler both unit-tested (`internal/frontend/serial_log_test.go`), mirroring `console_test.go`'s pattern.
- Not addressed: the same real multi-node locality gap `GetVMConsole` already has, and no download-the-full-log affordance (only ever the bounded tail) - a deliberate v1 scope limit, not an oversight.
