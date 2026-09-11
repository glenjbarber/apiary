# ADR-0093: Peer-forwarding TLS needs its own CA trust

## Status

Accepted

## Context

Verifying ADR-0092's fix live surfaced a second, distinct bug in the
same feature area. `node01` and `node02` (freshly bootstrapped this
session via `setup-quick`, each with its own self-signed TLS
certificate) both had `-peer-tls` effectively enabled (persisted as
`peer_tls: true` in `/var/db/apiary/node-config.json` on both hosts -
consistent, since each host's own managerd already requires TLS on its
listening socket). Attempting a real `RequestJoinColony` call with
`target_address` between them failed:

```
tls: failed to verify certificate: x509: certificate signed by unknown authority
```

`internal/manager.PeerReporter.dial()` only ever offered two choices
when `UseTLS` is set: trust the system certificate pool (correct for a
real, CA-issued certificate - ADR-0033's expected end state), or don't
use TLS at all. There was no equivalent of `-manager-tls-ca` (already
used by `cmd/frontend`/`cmd/restshimd` to trust managerd's own
self-signed certificate, via `internal/tlsdial.ManagerDialOption`) for
the peer-to-peer direction - so two Combs with self-signed certificates
could never successfully forward a peer RPC to each other at all,
including console/serial-log/VM-snapshot forwarding, ISO/jail-template
fetch, and now ADR-0092's join-colony forwarding. Testing directly
confirmed even a self-dial (a managerd targeting its own address)
failed identically - this affected every peer-forwarding RPC, not
something new to ADR-0092.

By contrast, `apiverse`/`apiarium` (this project's longer-running hosts)
never hit this: they use real, CA-issued certificates
(`fullchain.pem`), which the system pool already trusts - confirmed
live by successfully forwarding a real `RequestJoinColony` call between
them over trusted TLS during ADR-0092's own verification.

## Decision

### `PeerReporter` gains a `CAPool` field

Mirrors `-manager-tls-ca`'s exact semantics for the peer-forwarding
direction: `PeerReporter.CAPool`, a `*x509.CertPool`, is trusted
INSTEAD OF the system pool when `UseTLS` is set and `CAPool != nil`.
`dial()` sets `tls.Config.RootCAs` from it. A new `LoadPeerCAPool(caFile
string) (*x509.CertPool, error)` package function loads one or more
concatenated PEM certificates - typically every known peer's own
self-signed certificate (a self-signed cert works fine as its own trust
anchor for exactly the certificate it signed), or a real shared CA if
one exists. Kept as a small local duplicate of
`internal/tlsdial.ManagerDialOption`'s CA-loading logic rather than a
shared call, since that package returns one fixed `grpc.DialOption`
while `PeerReporter` needs a reusable pool applied fresh per address in
its own `dial()`.

### New `-peer-tls-ca` flag on both `managerd` and `frontend`

Both already have their own `-peer-tls` flag and construct their own
`PeerReporter` (managerd for ADR-0029/ADR-0035 write/read forwarding;
frontend for the cluster overview page's cross-node stats). Both gain
`-peer-tls-ca`, loaded via `LoadPeerCAPool` and assigned to
`peers.CAPool` right after construction - a start-up fatal error if the
file is missing or malformed, matching `-manager-tls-ca`'s own posture
on the client side.

### Machine Configuration UI and persisted node config

`peer_tls_ca` was added everywhere `peer_tls_hostname_map` already
exists, since operators already edit that one live through the "Peer
forwarding" panel: `api/rpc/manager.proto`'s
`GetNodeConfigResponse`/`UpdateNodeConfigRequest` (field 43 in both),
`internal/nodeconfig.Config.PeerTLSCA`, `internal/frontend`'s
`nodeConfigView`/`nodeConfigUpdateRequest`, and a new "Peer TLS CA" row
in `web/templates/machine.html`'s peer-forwarding panel (editable form
and read-only view alike).

## Consequences

- `internal/manager/peer_test.go` gained a real, end-to-end regression
  test using a freshly generated self-signed certificate: dialing it
  with no `CAPool` fails (`TestPeerReporter_TLSWithNoCAPoolFailsAgainstSelfSignedCert`
  - the exact failure a real user hit), and loading that same
  certificate as `CAPool` makes the identical dial succeed
  (`TestPeerReporter_TLSWithCAPoolTrustsSelfSignedCert`). Plus plain
  unit tests for `LoadPeerCAPool`'s missing/malformed-file error paths.
- `internal/nodeconfig`, `internal/manager` (`GetNodeConfig`/
  `UpdateNodeConfig` round-trip), and `internal/frontend` (the peer-
  forwarding panel's form-to-RPC threading) each gained coverage for
  the new field, mirroring `PeerTLSHostnameMap`'s own existing tests.
- This is a real, disclosed operational requirement, not fully
  automated: an operator with self-signed certificates must still
  build and distribute a trust bundle (concatenate every peer's
  `cert.pem` into one file, or generate one real shared CA and reissue
  every node's certificate against it) and point `-peer-tls-ca` at it
  on every node - `-peer-tls-hostname-map` already established this
  same "small, fixed fleet, an operator fills in one file" posture for
  hostname verification, and this follows the identical shape for CA
  trust.
- Full `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .`
  all clean.
