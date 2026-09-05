# ADR-0065: Authenticated cross-Hive VM console tunnel

## Status

Accepted

## Context

Apiary's noVNC frontend previously worked only when it was colocated with the
VM-owning Hive. `GetVMConsole` intentionally returns `127.0.0.1` because the
VNC listener is local to bhyve and must not be exposed on the network.

## Decision

Add bidirectional `ManagerService.ProxyVMConsole`. Its first frame names a VM.
The receiving managerd independently resolves and validates that VM through
`GetVMConsole`, requiring ownership by that Hive, then dials only the resulting
loopback VNC address. Later frames carry opaque RFB bytes. The frontend uses
the existing authenticated peer managerd client when a VM is owned elsewhere,
and bridges its browser WebSocket into this stream.

The stream is Viewer-authorized, matching the existing read-only console page.
It uses the same optional API-key and peer TLS configuration as other
frontend-to-peer calls. Neither a caller-supplied host nor a caller-supplied
port is accepted.

## Consequences

The raw VNC listener remains loopback-only and noVNC needs no new public
service. A remote-Hive console has one additional authenticated gRPC hop.
Connection lifetime is bounded by the browser WebSocket, peer gRPC stream, and
the VNC connection: a failure on either side closes the relay.

This does not create an interactive serial console. Captured serial-log
viewing remains a separate, bounded unary operation.
