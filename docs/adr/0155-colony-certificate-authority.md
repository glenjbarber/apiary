# ADR-0155: Colony certificate authority for inter-node TLS

## Status

Accepted (approved by the owner 2026-10-20)

Supersedes: None
Superseded by: None
Tracking issue: #97

## Context

Raft member-to-member traffic is not encrypted, and ADR-0078 names that gap as a committed limitation. ADR-0078 decides that raft uses mutual TLS, so every member both presents a certificate and verifies its peers'. The question left open is where those certificates come from.

Today `internal/hostcert.Generate` issues a self-signed serving certificate per Comb. Its extended key usage is ServerAuth only. A peer that dials out will verify it, but a peer that accepts a connection cannot verify a client certificate for ClientAuth, because the certificate does not carry that usage. Raft's mutual TLS needs ClientAuth in both directions, so these certificates cannot serve it.

Each Comb's self-signed certificate is also a separate trust anchor. A Colony with N Combs has N roots, and every Comb must hold every other Comb's certificate. Adding a node means distributing a pin for it by hand, which is the manual step ADR-0147 Part 4 set out to retire.

## Decision

The first Comb of a new Colony creates a private certificate authority. Every other Comb receives a leaf certificate from that CA and trusts only that CA.

### The trust chain

- **Colony CA.** ECDSA P-256. A self-signed certificate with BasicConstraints `CA:true`, `MaxPathLen 0`, KeyUsage `CertSign|CRLSign`, valid for 10 years. Its private key is `ca.key` (0600) on the Comb that created it. Its certificate is `ca.pem` (0644).
- **Node leaf.** ECDSA P-256, signed by the Colony CA. ExtKeyUsage `{ServerAuth, ClientAuth}`, both required, because raft mutual TLS uses client auth in both directions. KeyUsage `DigitalSignature|KeyEncipherment`. SANs hold the Comb's hostname(s) and IP(s). Valid for about one year.
- **Verification.** A peer accepts a certificate only if it chains to the Colony CA, with the usage the connection needs. The leaf's names must match the address the peer dials.

`internal/colonyca` implements the CA and leaf generation. Nothing outside the package calls it yet.

### Pinning the CA at join

A Comb that joins a Colony does not learn which CA to trust from the network. A join authorization carries the SHA-256 fingerprint of the Colony CA certificate, and the joining Comb accepts a CA only when its fingerprint matches.

The fingerprint is the lowercase hex SHA-256 of the certificate's DER bytes, with no prefix and no separators. This is deliberately a different format from `internal/hostcert.Fingerprint` ("SHA256:" followed by uppercase colon-separated pairs). The two formats must not be compared directly. Step 3 will decide whether join authorizations keep the existing format or move to the new one. Until then, the two must not be mixed.

### Generation refuses to overwrite

`GenerateCA` refuses when `ca.pem` or `ca.key` already exists. The error names every existing path, and neither file is touched. Replacing a CA silently invalidates every leaf the Colony holds, so overwriting it is not a recovery path. Files are published by writing a temporary file, setting its mode, and hard-linking it to the final name. A hard link fails when the target exists, which gives the no-overwrite guarantee that a rename cannot give. A rename would also race between the existence check and the publication.

### Existing certificates are not changed

`internal/hostcert.Generate` is left as it is. It continues to issue the self-signed ServerAuth certificate for the external API and for existing pins. It cannot serve mutual TLS and is superseded for inter-node use by the Colony CA. Its certificates are not migrated by this ADR.

## What this ADR does not cover

- **Public edge certificates.** Certificates for browsers and external clients stay with Cloudflare, as they are today. The Colony CA is private and is never trusted by a browser.
- **The CA key is a single point of trust.** Whoever holds `ca.key` can issue a certificate the whole Colony accepts. The key is root-only and stays on the creating Comb, but this ADR does not add HSM storage, key splitting, or off-host escrow. Losing the key means the Colony cannot issue new leaves and must be rebuilt.
- **CA rotation is not included.** This ADR does not say how the CA is replaced, cross-signed, or retired, or how leaves are renewed. A leaf lasts about a year, and renewal is future work. The expiry of the CA itself, 10 years out, is an open item.
- **Join protocol.** The join flow that carries and checks the fingerprint is step 3 and is not designed here beyond the pin.

## Staged plan

1. **This change.** This ADR, and `internal/colonyca` with `GenerateCA`, `IssueLeaf`, and `Fingerprint`, plus tests. Nothing is wired into apiaryinstall, joins, raftd, or managerd.
2. **Install creates the CA.** The first Comb of a new Colony runs `GenerateCA` during install and leaves the CA in place. Existing Colonies are not touched.
3. **Joins distribute and pin the fingerprint.** A join authorization carries the CA fingerprint. The joining Comb receives the CA certificate and verifies its fingerprint before trusting it.
4. **Raft and peers use it.** Raft and the peer connections use leaves from the Colony CA for mutual TLS, as ADR-0078 describes.

## Consequences

- A Colony gets one trust anchor. Adding a Comb means issuing it a leaf and pinning one fingerprint, not distributing a pin for every other Comb.
- The CA key becomes the most sensitive file in the Colony. Its loss or theft is a Colony-wide event, which is accepted in exchange for a single anchor.
- Step 1 changes no running behavior. Existing certificates and pins stay valid.
- The two fingerprint formats in the codebase now coexist. Step 3 must choose one for join authorizations and document it, so the mismatch does not turn into a pinning bug.

## Verification

- `internal/colonyca/colonyca_test.go` covers: a leaf verifies against the CA for ServerAuth and for ClientAuth; a leaf from a different CA does not verify; `GenerateCA` refuses when either file exists and leaves both byte-identical; the key file is 0600 and the certificate file is 0644; `IssueLeaf` refuses an empty request, a non-CA certificate, an expired CA, and a lifetime longer than the CA; the CA is CA:true with MaxPathLen 0; and the fingerprint is pinned to the value of a fixed fixture, checked independently with openssl.
- `gofmt -l .` is empty, `go vet ./...` is clean, and `make test` passes apart from known host-specific integration failures.
