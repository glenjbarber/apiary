# ADR-0072: Cloudflare Origin CA certificate lifecycle

## Status

Proposed design. No certificate API client or service mutation is implemented
by this ADR.

## Context

Apiary already supports TLS certificate and key *paths* for `managerd`,
`restshimd`, and `frontend` through Machine Configuration. The operator still
has to create, install, track, and rotate the files outside Apiary.

Cloudflare Tunnel exposure already establishes a narrow, node-local
Cloudflare credential model: a token file is never raft-replicated and the
operator pre-provisions the tunnel. The public side of a proxied Tunnel
hostname is already HTTPS at Cloudflare's edge. A certificate managed by this
feature would protect the separate Cloudflare-to-origin TLS leg.

## Decision

The first managed certificate slice will issue **Cloudflare Origin CA**
certificates, not public ACME certificates and not edge certificates.

This is intentionally limited to a service whose hostname is proxied through
Cloudflare. Origin CA certificates are not trusted by ordinary clients when a
record is DNS-only, Cloudflare is paused, or traffic bypasses Cloudflare. Those
uses must continue to use an externally supplied publicly trusted certificate.

### Local key generation and local secret storage

The Hive generates a private key locally and keeps it local. It submits only a
CSR to Cloudflare's Origin CA API. Apiary writes the returned PEM certificate
and locally generated private key atomically to an explicitly configured,
root-owned directory, with the private key mode `0600`. Neither private key,
raw Cloudflare token, CSR, nor certificate body is stored in raft, shown in a
frontend response, emitted in logs, or included in `-export-host-config`.

The certificate's identifier, requested hostnames, expiry, issuing Hive, and
file paths are node-local inventory metadata. The inventory is sufficient to
report renewal health without becoming a secret store.

### Credential boundary

This uses a dedicated token file with the minimum documented permission:
`Zone > SSL and Certificates > Edit`, restricted to the exact zone. It is not
the existing Tunnel DNS token, and it never uses the deprecated Origin CA user
service key. The Machine page displays only whether the token file is
configured and readable, never its content.

### Explicit issuance and renewal

Issuance is an Admin-only, Hive-local action. It requires an explicit hostname
list, a selected local service, and confirmation that every requested hostname
is Cloudflare-proxied. Apiary validates each hostname before creating a CSR.
It does not guess a hostname from a Cell name or automatically request a
wildcard certificate.

The first implementation must use an explicit **Renew now** operation, not an
unbounded reconciliation-loop API call. It creates a replacement certificate,
validates the PEM/key pair, writes both files atomically, records the new
certificate ID and expiry, then schedules only the selected local service for
restart. If any step fails, the existing files and running service remain in
place. Revoking the retired certificate is a separate confirmed action after
the replacement has been verified.

Expiry is a visible Machine Configuration health item. Cloudflare does not send
Origin CA expiry notifications, so Apiary must derive warning and critical
states from its local inventory. Automatic renewal may be considered only after
the explicit flow has been live-verified and has durable failure evidence.

### Service scope

v1 targets one selected local Apiary service at a time. It reuses the existing
TLS path configuration and the allowlisted local service controller; it does
not edit `rc.conf`, configure arbitrary processes, or distribute one key across
Hives. Each Hive receives its own key and certificate even when the hostname
set overlaps.

## Consequences

- This gives Cloudflare-proxied Apiary services an operator-visible, narrow
  certificate lifecycle without introducing a raft-replicated secret.
- It does not make a service publicly trusted outside Cloudflare.
- The future implementation needs a small Cloudflare Origin CA API client,
  local inventory storage, an Admin-only managerd RPC, a Machine page panel,
  and service-specific post-write verification.
- The future implementation must prove failure safety on a non-production
  Hive before any production token or certificate is used.
