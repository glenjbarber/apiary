# ADR-0033: raftd internal-socket token auth + TLS for managerd/restshimd/frontend

## Status

Accepted

## Context

CLAUDE.md has long named two real, disclosed authentication gaps this
project left open on purpose while it was still single-developer/
local-network-scoped: `raftd`'s internal socket relies on file
permissions alone (`0o660` + a `0o700` parent directory, ADR-0023's own
"judged sufficient for now"), and nothing exposed on the network
speaks TLS - `managerd`'s external gRPC API, `restshimd`'s REST API,
and `frontend`'s web UI are all plaintext HTTP/gRPC. The API-key auth
ADR-0023/ADR-0030 already built means a real credential exists and is
checked, but it travels - and is entered via the web login form -
entirely in the clear.

This ADR closes both gaps, each opt-in and non-breaking, matching
every other security feature this project has shipped so far
(ADR-0023's own "zero keys means unauthenticated" posture, ADR-0030's
role-map).

## `raftd`'s internal socket: a shared-secret token, not a scaled-down ADR-0023

`raftd`'s internal socket has exactly one kind of legitimate caller -
`managerd` on the same node, or a peer `raftd` during `-join` - not a
spectrum of human operators needing different privilege tiers the way
`managerd`'s own external API does. Building a second, parallel
tiered-role system here would be solving a problem that doesn't exist
at this layer. Instead: a single shared secret.

- `cmd/raftd` gains `-internal-token` (a plain flag value, matching
  `-peer-api-key`'s own existing convention rather than introducing a
  new file-based-secret pattern). Empty (the default) preserves
  today's behavior exactly - the socket relies on file permissions
  alone, unchanged.
- `internal/raft.TokenUnaryInterceptor`/`TokenStreamInterceptor` gate
  every `RaftInternal` RPC on this token via `checkToken`, comparing
  the caller's `authorization` gRPC metadata (an optional `"Bearer "`
  prefix is stripped, mirroring `internal/manager`'s own
  `extractBearerToken`) using `crypto/subtle.ConstantTimeCompare` - the
  same timing-attack posture ADR-0019 already applies to session
  credential comparison.
- `internal/raft.TokenCredentials` (a `credentials.PerRPCCredentials`
  implementation, the same shape as `cmd/frontend`'s/
  `internal/manager/peer.go`'s existing `apiKeyCredentials` types)
  attaches the token to outgoing calls. `internal/manager.Dial` gains a
  `token` parameter it forwards into this; `cmd/managerd` gains a
  matching `-raftd-token` flag. `cmd/raftd`'s own `-join` client
  (`joinCluster`) also presents the same token when calling a peer
  `raftd` - a real multi-node deployment is expected to configure the
  same token everywhere, the same assumption `-peer-api-key` already
  makes for `managerd`'s own cross-node calls (ADR-0029).

## TLS for `managerd`, `restshimd`, and `frontend`

An API key (ADR-0023) or a PAM password (ADR-0030) travels in
plaintext the moment any of these are bound to more than loopback -
which ADR-0029 already established as a real deployment requirement
for cross-node peer forwarding to work at all. TLS closes that.

- `cmd/managerd` gains `-tls-cert`/`-tls-key` (both required together,
  rejected otherwise). Set, they load via
  `credentials.NewServerTLSFromFile` and attach as a `grpc.Creds`
  server option alongside the existing auth interceptors. Unset (the
  default), `managerd` serves exactly as it always has - plaintext.
- `cmd/restshimd` and `cmd/frontend` each gain the identical pair for
  their own HTTP listener (`http.ListenAndServeTLS` instead of
  `ListenAndServe` when set) *and* a `-manager-tls`/`-manager-tls-ca`
  pair for dialing `managerd` itself over TLS, since both are
  `managerd` clients first. `internal/tlsdial.ManagerDialOption`
  implements this once, shared by both commands (the same reasoning
  `apiKeyCredentials`'s own duplication comment already gives for why
  small credential-attaching logic sometimes gets copied rather than
  imported between `main` packages - but this one is a genuine, non-
  trivial decision tree with real error handling, unlike a two-line
  type, so a small shared package earns its place instead).
  `-manager-tls-ca`, if set, trusts that PEM file instead of the
  system certificate pool - the expected case for a self-signed
  certificate; left empty, a real CA-signed certificate is assumed.

## Consequences

- All new code is opt-in and defaults to today's exact behavior -
  every existing deployment (including this project's own two live
  nodes) keeps working unchanged until someone deliberately sets a
  token or a cert/key pair.
- Full unit test coverage: `internal/raft`'s token check/credentials
  (`TestCheckToken_*`, `TestTokenCredentials_*`) and
  `internal/tlsdial`'s dial-option construction against both a real,
  freshly-generated self-signed certificate and deliberately broken
  inputs (missing file, malformed PEM).
- Not addressed by this ADR: certificate provisioning/rotation (an
  operator's own responsibility, the same posture this project already
  takes for `/etc/pam.d/<service>` or `hast.conf`), and TLS for `raftd`'s
  own raft-transport TCP connections between cluster members (a
  separate concern from the internal *socket* this ADR covers - raft's
  own inter-node traffic is out of scope here).
