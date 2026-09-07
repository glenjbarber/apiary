# ADR-0078: Raft transport TLS

## Status

Accepted

## Context

ADR-0033 gave `raftd`'s own internal UDS/gRPC transport (the protocol
`managerd`/`restshimd` use to talk to their local `raftd`) both a
shared-secret token and real TLS. That ADR's own scope was explicitly
the internal socket, not raft's own member-to-member wire protocol -
and this codebase's "Remaining committed limitations" section has
named that second gap for a long time: "Raftd internal-socket token
auth and application TLS are opt-in, not secure defaults. Raft's
member-to-member TCP transport still lacks TLS."

Confirmed directly before designing anything: `internal/raft.Node`
(`node.go`'s `New`) builds exactly one transport, unconditionally -
`raft.NewTCPTransport(cfg.BindAddr, addr, 3, 10*time.Second,
os.Stderr)`. There is no flag, no config field, no code path that ever
does anything else. So the accurate framing is "this capability does
not exist yet," not "it exists but defaults off" - unlike, say,
`-hast-enabled` or `-jail-enabled`, which are real opt-in switches
around code that's fully there either way.

Two design questions needed answers before implementing:

1. **Does hashicorp/raft support TLS directly?** No - `raft.NewTCPTransport`
   is plain TCP with no TLS option. The library does, however, expose
   `raft.NewNetworkTransport(streamLayer, ...)`, which accepts any
   `raft.StreamLayer` (`net.Listener` plus a `Dial` method) - this is
   the documented extension point for exactly this kind of transport
   customization, not a workaround.
2. **One-directional (server-verified-only) TLS, like managerd's own
   external API and peer-forwarding TLS, or mutual TLS?** Raft members
   are a fixed, closed, symmetric set - every member both dials out to
   and accepts connections from every other member, unlike managerd's
   external API (an operator-facing surface with many possible clients)
   or `internal/tlsdial`'s own managerd-to-managerd peer forwarding
   (a client verifying one specific server it's calling). A closed
   symmetric peer group is exactly the case mutual TLS is for: every
   member should refuse a connection - incoming or outgoing - from
   anything that can't present a certificate signed by the cluster's
   own CA. **Decision: mutual TLS**, both directions, both roles.

## Decision

### `internal/raft/tls_transport.go`: a `raft.StreamLayer` over mutual TLS

`newTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error)`
loads this node's own certificate/key and a CA pool, then builds one
`*tls.Config` used symmetrically: `RootCAs` (verifying a peer when this
node dials out) and `ClientCAs` with `ClientAuth:
RequireAndVerifyClientCert` (verifying a peer when this node accepts a
connection) both point at the same pool, since every peer in a raft
cluster plays both roles. `tlsStreamLayer` wraps a `tls.Listener` for
`Accept`/`Close`/`Addr` (raft.StreamLayer embeds `net.Listener`) and
implements `Dial` via `tls.DialWithDialer` using that same config.

### `Config` gains three optional fields, all-or-nothing

`internal/raft.Config` gains `TLSCert`, `TLSKey`, `TLSCA string`.
`withDefaults` rejects a partially-set trio (matching the "both or
neither" validation this codebase already uses for `-tls-cert`/
`-tls-key` pairs elsewhere) but accepts all three empty exactly as
before. `node.go`'s `New` calls a new `newTransport(cfg)` that builds
today's plain `raft.NewTCPTransport` when all three are empty, or the
TLS `StreamLayer` wrapped in `raft.NewNetworkTransport` when they're
set - no other code in `Node` changes, since both paths satisfy the
same `raft.Transport` interface `raft.NewRaft` expects.

### `cmd/raftd`: three new opt-in flags

`-raft-tls-cert`, `-raft-tls-key`, `-raft-tls-ca` - unset by default,
preserving today's plain-TCP behavior exactly. The startup log line
gains `raft-tls=%v` (mirroring `managerd`'s own `tls=%v`/
`cloudflare-enabled=%v` fields) so it's visible in logs which mode a
running `raftd` is actually in, not just what was intended.

### Not addressed

- **Certificate rotation.** A renewed raft-transport certificate needs
  a `raftd` restart to take effect, same as every other TLS certificate
  in this codebase (managerd's own gRPC TLS, Origin CA-issued
  certificates) - no hot-reload exists anywhere yet.
- **Revocation checking (CRL/OCSP).** `tls.Config.ClientAuth` here only
  ever checks the certificate chains to the trusted CA and hasn't
  expired - a compromised-but-not-yet-expired member certificate can
  still connect until the CA itself is rotated. This matches this
  codebase's existing security posture everywhere else TLS is used
  (`internal/tlsdial`, ADR-0033) - none of it does revocation checking
  either.
- **Automatic default-on.** This stays opt-in, like every other TLS
  capability in the project, because turning it on by default would be
  a breaking change for any already-running cluster with no
  certificates provisioned - the "Remaining committed limitations"
  bullet updated below still correctly says defaults aren't secure by
  themselves; what changed is that the *capability* now exists at all.
- **Certificate provisioning tooling.** Same posture as ADR-0033 and
  Origin CA's own certificate handling: generating and distributing the
  CA and per-node certificates is an external operational
  responsibility, not something Apiary automates.

## Consequences

- A cluster that wants raft's own wire traffic encrypted and
  mutually authenticated between members now can, without any
  hashicorp/raft fork or vendored patch - `raft.NewNetworkTransport`'s
  `StreamLayer` extension point was exactly the right tool.
- Zero behavior change for every existing deployment: `apiarium`/
  `apiverse` run with all three flags unset today, and will keep
  building the identical plain-TCP transport after this change until
  someone explicitly configures certificates.
- This codebase now has its first automated, in-process, genuinely
  multi-node raft test (`TestTwoNodeClusterReplicatesOverMutualTLS`) -
  two real `raft.Node`s on different loopback ports, joined via
  `AddVoter`, with a real log entry replicated and observed on the
  follower. Every prior multi-node claim in this project was verified
  live against `apiarium`/`apiverse` instead, since the existing
  integration test harness only ever runs a single-node `raftd`. This
  test pattern (two `New()` calls, `freeLoopbackAddr`, `AddVoter`,
  `eventually`) is now available for any future raft-level test that
  needs a real second member instead of a live cluster.

## Verification

Unit tests: `internal/raft/tls_transport_test.go` -
`withDefaults` rejects a partially-set TLS trio and accepts both
all-empty and all-set; `newTLSConfig` rejects a missing certificate/key
path and a CA file with no valid certificates.
`TestTwoNodeClusterReplicatesOverMutualTLS` builds a real CA and two
CA-signed leaf certificates, starts two TLS-configured `raft.Node`s,
bootstraps one, adds the other as a voter, applies a `CreateVM`
command on the leader, and polls the follower's own local FSM
(`ListVMsLocal`) until the replicated VM appears - proving election,
membership change, and log replication all work end to end over the
new transport, not just that a TLS handshake succeeds in isolation.
`go build ./...`, `go vet ./...`, `gofmt -l`, `git diff --check`, the
complete `go test ./...` suite, and the FreeBSD cross-compile for
`raftd`/`managerd` all pass. Live verification against a real 2-node
`apiarium`/`apiverse` cluster with real certificates was not performed
as part of this change - the in-process two-node test above is the
verification bar for this ADR, matching how deeply hashicorp/raft's
own transport layer is exercised without touching production.
