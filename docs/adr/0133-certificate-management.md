# ADR-0133: Colony-wide certificate management

## Status

Proposed

**Date:** 2026-09-26
**Author:** Goose (subagent 20260926_31)
**Phase:** 3 of ADR-0127 (`docs/adr/0127-sylve-io-features.md`)

## Context

ADR-0127 catalogued Sylve's feature set and ranked certificate
management as Phase 3. Its framing is that "Apiary manages TLS
manually via config files" and that automated lifecycle "reduces
operational burden", with the adoption note "integrate with Colony
join flow; auto-renew for frontend/restshimd/managerd/raftd
listeners". Its proposed data shape is a "Certificate CRD" with
`type` (`imported`, `self-signed`, `letsencrypt`, `acme`),
`domains[]`, `key_algorithm`, `renewal_window`.

Two of those sketches do not survive contact with the code, and
both corrections matter more than the feature description:

- **`raftd` is not in scope.** ADR-0127 lists auto-renew for
  "frontend/restshimd/managerd/raftd listeners". The raftd peer's
  TLS material is not an operator-facing certificate at all: it is
  the Colony's own consensus trust root, it is read exactly once at
  transport construction, and changing it on one voter is a
  documented quorum hazard. `internal/raftdconfig`'s package
  comment records the 2026-09-15 audit that found there is
  deliberately **no `UpdateRaftdConfig` RPC** because
  "internal_token must match managerd's own separately-configured
  raftd_token for RaftInternal auth to keep working, and raft TLS
  material is cluster-coupled (changing it on one voter and
  restarting can isolate that voter and lose quorum)". An ADR that
  promised automated renewal of the raftd listener would be
  promising automated quorum loss.
- **There is no "Certificate CRD" and there will not be one.**
  `api/internalpb/state.proto`'s `FSMSnapshotState` (line 835) is a
  flat set of `map<string, ...>` fields over workloads, networks,
  jails, API keys, pending join requests, and the restart guardrail
  of ADR-0103. There is no CRD concept, no spec/CRUD pair, and
  nothing that resembles Kubernetes' model. Certificate inventory
  is a handful of non-secret facts per Combs, not a spec object
  with a reconcile loop of its own. The right shape is a new map
  field on `FSMSnapshotState`, not a CRD.

The second open question in ADR-0127 is this ADR's central
decision: "**Certificate authority:** Should Apiary run its own
internal CA (like Smallstep) or only integrate with Let's
Encrypt/external CAs?"

### Scope boundary

This is the first thing to settle, because the word "certificate"
in this codebase already means two unrelated things.

**In scope — operator-facing certificates.** TLS for services the
operator or a guest workload presents to the outside world, or that
a guest workload consumes as a trust anchor:

- `managerd`'s external gRPC listener. `cmd/managerd/main.go:441`
  calls `credentials.NewServerTLSFromFile(cfg.TLSCert, cfg.TLSKey)`;
  `internal/nodeconfig/manager.go:199-200` defines those as
  `tls_cert` / `tls_key` file paths. When both are empty (today's
  default) the listener is plaintext.
- `frontend` and `restshimd`. Both dial `managerd` through
  `internal/tlsdial`, whose package comment calls out exactly this
  case: "useTLS=true with an empty caFile trusts the host's own
  system certificate pool - the expected case for a real, CA-signed
  certificate... caFile, if set, is trusted *instead of* the system
  pool - the expected case for a self-signed certificate". The
  `serverName` parameter exists specifically because "managerd
  stays loopback-only (its `-rpc-addr` is 127.0.0.1, never exposed
  on the network) but its certificate names a real public hostname
  a CA like Let's Encrypt could actually issue for".
- Certificates consumed by guest workloads: a Cell that needs to
  trust a private CA, or to present a publicly-valid certificate.

**Out of scope — the Colony's own internal trust.** The mutual
TLS between Combs is already solved and is not this ADR's to
redesign:

- `internal/raft/tls_transport.go` builds the peer transport.
  `newTLSConfig(certFile, keyFile, caFile)` loads this node's
  identity and one shared CA pool, which serves as both `RootCAs`
  (dialing) and `ClientCAs` (accepting) because "raft members form
  a closed, symmetric peer set fixed by the cluster configuration,
  not a public-facing API". `newTransport` selects
  `newTLSStreamLayer` only when `TLSCert`/`TLSKey`/`TLSCA` are all
  set, and `cfg.withDefaults` rejects any partial set.
- ADR-0078 (raft transport TLS) and ADR-0113 (TLS trust prompt on
  Colony join) established how a joining Comb obtains and confirms
  that trust.
- ADR-0115 (automatic derivation of `peer_tls_hostname_map`)
  established that the hostname map is *derived*, not configured,
  so a renewed peer certificate does not require a matching config
  edit.

**The one coupling this ADR does not walk away from.** This ADR
*reads* the raft peer certificate's expiry and publishes it as
health evidence, and it *constrains* any future rotation of that
material. It never writes it, never renews it, and never
auto-restarts a Comb to apply it. See "The critical safety
property" below.

## What already exists

This is not greenfield. Roughly 70% of the per-Combs issuance
transaction is already written, tested, and shipped, and it must be
reused rather than rebuilt.

### `internal/origincert` — the local issuance transaction

`internal/origincert/store.go` opens with the design constraint
that governs this entire ADR: "keeps Cloudflare Origin CA private
keys and certificate files local to one Hive. It deliberately has
no raft dependency."

What it already provides:

- `NewCSR(hostnames)` — generates an ECDSA P-256 key and CSR
  locally. The key is PEM-encoded "only so the caller can write it
  directly to a root-owned local file after Cloudflare returns a
  matching certificate".
- `WritePair(directory, name, certPEM, keyPEM)` — validates the
  pair with `tls.X509KeyPair` first, `MkdirAll(directory, 0o700)`,
  then stages both files with `writeTemp` (create temp, `Chmod`,
  write, `Sync`, `Close`) and only then renames them into place.
  Its own comment states the invariant that makes renewal safe:
  "Both replacement files are completely written and fsynced
  before either live path changes. Apiary schedules the consuming
  service restart only after this function succeeds, so it never
  reloads a mismatched pair."
- `InventoryEntry` — explicitly "only the non-secret facts needed
  to report an Origin CA certificate's lifetime", with the comment
  "deliberately excludes token values, private keys, CSRs, and
  certificate PEM". This struct is, field for field, the raft-side
  record this ADR needs. It already exists.
- `RenewalWindow = 30 * 24 * time.Hour` and
  `ExpiryStatus` (`ExpiryOK` / `ExpirySoon` / `ExpiryExpired`) with
  `Expiry(now)`. The comment is careful and correct: "deliberately
  unrelated to `internal/health`'s node-health computations - this
  only ever describes one certificate's own remaining lifetime."
  That separation is the precedent for how expiry evidence must be
  wired into health, and this ADR preserves it.
- `SaveInventory` / `LoadInventory` — atomic `inventory.json`,
  mode 0600, and `LoadInventory` returns `(nil, nil)` when absent
  rather than an error.

`internal/origincert/issue.go` adds `Issue(ctx, issuer, req)`,
which is the whole lifecycle minus scheduling: generate key
locally → call `Issuer.Issue` → reject incomplete metadata
(`ID`, `ExpiresAt`, `PEM`) → `WritePair` → upsert into the local
inventory. Its comment records the intent this ADR generalises:
"Apiary never invokes this from reconciliation or guesses
hostnames from a Cell."

`internal/origincert/renew.go` adds `Renewer.RunOnce(ctx)`, which
is a complete, correct renewal loop:

- `Config func() (directory, tokenFile string, err error)` is
  called fresh on every tick, so "a directory/token-file path
  edited through the Machine Configuration page takes effect on the
  next tick, matching every other node-config-backed capability in
  this codebase."
- Both config values empty means "a silent no-op, not an error" —
  the unconfigured feature is not an error state.
- Per-certificate failures are collected with `errors.Join` and do
  not block the remaining certificates.
- `RestartService` is optional; renewal still happens without it,
  matching a manual renewal whose restart failed.

This is the exact pattern the Colony-wide feature needs. It should
be generalized, not replaced.

### `internal/cloudflare` — the issuer, DDNS, and the tunnel

- `origin_ca.go` implements `origincert.Issuer` as
  `OriginCAIssuer`, calling Cloudflare's Origin CA API and parsing
  `expires_on` as RFC 3339. Its comment states the custody rule:
  the cert PEM "is returned separately so callers can write it
  directly to a root-owned file **without storing it in raft**."
  That sentence is the existing precedent this ADR generalises
  from "Cloudflare Origin CA" to "any CA".
  `ValidOriginCAValidity` restricts validity to
  `{7, 30, 90, 365, 730, 1095, 5475}` days and is enforced "at the
  API boundary as well as the eventual UI boundary, so another
  caller cannot issue an invalid request" — a good precedent for
  where validity policy belongs.
- `dns.go` is Dynamic DNS (ADR-0127's "Dynamic DNS (Cloudflare,
  Namecheap, Sylve.app)"), driven by `internal/nodeconfig`'s
  `CloudflareTokenFile`. DDNS and certificate issuance share a
  chicken-and-egg: the FQDN must resolve before an HTTP-01 or
  DNS-01 challenge can succeed. `dns.go` already exists and is the
  thing that makes issuance possible; this ADR does not touch it
  but does depend on it for the ACME path.
- `sidecar.go` and `tunnel.go` implement ADR-0063, Cloudflare
  Tunnel exposure. A Cloudflare Tunnel terminates TLS at Cloudflare
  and forwards to a local plaintext origin, so a tunneled service
  needs **no certificate on the Comb at all**.

### `internal/tlsdial` — the consumer side

`ManagerDialOption(useTLS bool, caFile, serverName string)` is
already the complete trust configuration for a client: system pool
when `caFile` is empty, one explicit CA file when set, and an
independent `serverName` override. It verifies a server
certificate only and, per its own package comment, "never presents
a client one" — the mirror image of the symmetric raft transport.
No change to this package is required by this ADR; it is cited as
the existing answer to "how does a client trust our own CA".

### The gaps

1. `internal/origincert` is documented as local to one Hive with
   no raft dependency, and the current wiring is local-only.
   There is no cluster-wide view: an operator must visit every
   Comb to learn what certificate it holds.
2. `ListOriginCertificates` and `IssueOriginCertificate`
   (`api/rpc/manager.proto:38-39`, messages at lines 1027-1041,
   handlers at `internal/manager/server.go:621` and `:643`) are
   hard-coded to `Service: "apiary_managerd"`. The issuance
   handler additionally requires
   `cfg.TLSCert == filepath.Join(cfg.OriginCADirectory, name+".crt")`
   and the matching `.key` — issuance is welded to `managerd`'s own
   listener, and cannot issue for `frontend`, `restshimd`, or a
   guest workload.
3. There is no revocation path anywhere. Nothing in
   `internal/origincert` or `internal/cloudflare` deletes a
   certificate, calls a revoke endpoint, or records that a
   certificate was retired.
4. There is no ACME client. The `Issuer` interface has exactly
   one implementation, `cloudflare.OriginCAIssuer`.
5. There is no expiry evidence. `ExpiryStatus` is computed but
   consumed only by the UI; nothing feeds `ClusterHealth`
   (`api/rpc/manager.proto:164`), `internal/deadman`, or
   `internal/assumptions`.
6. ADR-0072's own status line records the same gap in its own
   words: "API/UI activation and renewal are available for local
   `apiary_managerd` only. Expiry health, renewal scheduling,
   revocation, and support for the other Apiary services remain
   future work." ADR-0077 closed expiry health and scheduled
   renewal for the single local case. This ADR closes the rest.

## Decision

### D1. Scope

**In:** issuance, renewal, revocation, and key custody for
operator-facing certificates — `managerd`'s external listener,
`frontend`/`restshimd` when operator-served, and guest workloads.

**Out:** the Colony's internal mTLS trust (`internal/raft/
tls_transport.go`, ADR-0078, ADR-0113, ADR-0115). Apiary will not
issue, renew, revoke, or rotate a raft peer certificate. It may
read that certificate's expiry and report it. Native ZFS
encryption key material is out of scope here; see D7.

### D2. No internal CA. Integrate with external CAs behind the
existing `Issuer` interface.

**Recommendation: do not run an internal CA.** ADR-0127's question
has a clear answer for Apiary, and the answer is the boring one.

`internal/origincert` already defines the right abstraction — a
two-method `Issuer` interface with one implementation. Adding a
`smallstep` implementation is a week of work and should be
declined on the merits, not on effort:

- **Nothing in Apiary needs a private trust anchor.** Every
  consumer of an Apiary certificate is either an Apiary binary
  (which already accepts a CA file via `tlsdial`'s `caFile`) or a
  browser, `curl`, or package manager inside a guest workload
  (none of which will trust a CA Apiary generated on a Combs). An
  internal CA would have to be installed into every guest image to
  be worth anything, and guest images are not Apiary's to modify.
- **The certificate exists to satisfy an outside party.** The
  whole reason `managerd` gets a certificate rather than a
  self-signed blob is that something on the other end of the
  connection is deciding whether to trust it. That party is
  Cloudflare's edge (Origin CA), a browser, or a public CA. A
  private CA optimises for a trust model Apiary does not have.
- **It couples the quorum trust root to a supervised service.**
  The strongest argument, and the one that should decide it. Today
  the peer trust root is three files read once at transport
  construction, and losing or corrupting them costs one Combs
  membership. A CA adds a running, upgradeable, stateful process
  whose loss is felt by every certificate holder at once, and whose
  own key is a strictly higher-value target than any leaf it ever
  signs. That is a new cluster-wide blast radius, bought in
  exchange for a trust model nothing needs.
- **The size mismatch.** ACME's whole reason for existing is
  short-lived, automatically-replaced credentials. A cluster of
  Combs is a handful of stable machines, not a fleet of containers
  with per-pod identity. The problem ACME solves is not the
  problem Apiary has.

**What we do instead:** keep `Issuer`, add a second implementation
for ACME (Let's Encrypt and any RFC 8555 endpoint) alongside
Cloudflare Origin CA, and add an `imported` implementation for
`type: imported` (the operator supplies a PEM pair; Apiary only
manages the file placement, the restart, and the expiry clock) and
`self-signed` (generated locally by the existing `NewCSR` shape,
with `ValidityDays` capped well below the public CAs' limits).
One interface, four issuers, no CA process.

**What would change this answer.** The recommendation is
conditional and these are the triggers, in rough order of
likelihood:

1. **An air-gapped Colony.** No internet egress means no ACME and
   no public CA. Today Origin CA also requires egress. If
   air-gapped operation becomes a supported deployment, a
   step-ca sidecar becomes the only option — and the correct
   deployment is one Comb running it, deliberately *not* a voter,
   with its state backed up out of band.
2. **WireGuard peer certificates** (ADR-0127 Phase 3's other
   half). A mesh with more than a handful of peers, or any
   requirement for short leaf lifetimes, is the classic Smallstep
   use case. This is the most likely near-term trigger.
3. **Native ZFS encryption key wrapping** (ADR-0130). Wrapping a
   dataset key needs a trust anchor the Colony controls. This ADR
   does not build one — see D7 — but if ADR-0130 needs one, the CA
   decision should be revisited *with* that requirement in hand
   rather than ahead of it.
4. **Server-side TLS between Combs and external services** where
   pinning is unacceptable and a public CA cannot be used.

In every one of these cases the CA should be a deployment the
operator chooses to run, never a dependency Apiary assumes exists.

### D3. Lifecycle

**Issuance** is local, explicit, and never automatic. Apiary never
guesses hostnames from a Cell or from a node's IP; the operator
supplies the hostname set, as `IssueRequest` already documents.
The key is generated on the Comb that will serve the certificate
and never leaves it. Issuance for one Comb requires no quorum and
no raft write (see D6).

**Renewal** reuses `Renewer.RunOnce` unchanged in shape, extended
to four issuers and to services beyond `managerd`. The existing
properties are all load-bearing and are kept verbatim: `Config`
read fresh per tick; unconfigured means silent no-op; per-cert
failures joined, never blocking; restart only after a successful
`WritePair`. `RenewalWindow` stays at 30 days but is *per issuer*:
for a 90-day certificate that is a third of its life, for a
5475-day Origin CA certificate it is noise. The window becomes
`min(30 days, 20% of remaining validity)`, floored at 7 days.

**Revocation** is a new, local-first operation, because the
asymmetry matters:

- *Local retirement* is always available and always immediate:
  remove the pair, mark the inventory record revoked with a
  timestamp and a reason, and restart the service if the pair was
  live. This is what Apiary can guarantee.
- *Issuer-side revocation* is best-effort and issuer-dependent.
  Cloudflare Origin CA exposes a revoke endpoint for certificates
  it issued. ACME exposes `revokeCert` with the account key, which
  means revocation of an ACME-issued certificate is only possible
  on a Comb that holds the ACME account key — a fact that pushes
  account-key custody toward a *named* owner rather than a
  directory. `imported` and `self-signed` certificates have no
  issuer-side revocation at all; the inventory must say so rather
  than implying a revocation happened.
- Apiary does **not** build CRL distribution or OCSP stapling. No
  supported issuer offers OCSP for the private-CAs case, and
  publishing CRLs from Combs is a service with its own failure
  modes and no consumer.

**Key custody.** The rule, which the existing code already
follows and this ADR makes Colony-wide:

> A private key is generated on the Comb that serves the
> certificate, written to a root-owned file with mode 0600 in a
> 0700 directory, and never replicated through raft in any form,
> encrypted or otherwise.

The precedent is not merely stylistic. `internal/origincert` is
explicitly "local to one Hive... no raft dependency". The Origin CA
*token* — the credential that can mint a certificate for anything —
is not in raft either: it is a path (`OriginCATokenFile`) in
`internal/nodeconfig`, read per request in the issuance handler
(`os.ReadFile(cfg.OriginCATokenFile)`). The ACME account key gets
the same treatment, and the nodeconfig path is validated by
`validatePathField` (`internal/nodeconfig/manager.go:465`).
`internal/raftdconfig` holds the same line for consensus-critical
material, and warns when a file "is readable by group/other but
contains internal_token".

**Raft therefore replicates only non-secret inventory metadata**: a
`CertificateRecord` carrying identity, hostnames, issuer, validity
window, the local certificate path, and the last observation's
result. It is for display, cross-Combs comparison, and evidence.
It is never authoritative — a Combs's own file is the truth about
that Combs, exactly as `InventoryEntry` is the truth locally.

### D4. Expiry evidence and alerting

`ExpiryStatus` gains a fourth value. Today it has `ok`, `soon`,
`expired`, and the difference between "I checked and it is
expiring" and "I could not check" is unexpressible:

- `CERTIFICATE_EXPIRY_OK`, `..._SOON`, `..._EXPIRED` — observed,
  and the observation is a fact.
- `CERTIFICATE_EXPIRY_UNKNOWN` — the check could not be made:
  the file is unreadable, the pair does not match, the issuer
  returned metadata that would not parse, the node has not yet
  reported at all, or the reporting path itself is broken.

Rules, all of which follow from ADR-0056 (evidence-aware health),
ADR-0118 (why-not / quorum-blocker detail), and ADR-0122
(cluster evidence-aware health API):

- **Unknown is never `false`.** A Combs whose certificate state
  could not be determined is `unknown` in `ClusterHealth`, full
  stop. It is never counted as expiring, never counted as healthy,
  and never contributes a negative to any aggregate.
- **Expiry is not node health.** A serving certificate that is
  about to expire is a fact about a file, not evidence that the
  Combs is degraded. `internal/health`'s `ComputeNodeHealth` and
  its `StatusUnknown` / `ReachabilityUnknown` / `SuffrageUnknown`
  vocabulary stay untouched. `origincert`'s own comment already
  insists on this separation; this ADR ratifies it.
- **Evidence is per-Combs, gathered locally, published upward.**
  The same follower-originated replicated-write pattern as
  `AcquireRestartLease` / `RecordRestartCompleted` in
  `state.proto` is used, so a follower can publish its own
  observation without being leader.
- **Assumption claims are not evidence.** `internal/assumptions`
  (ADR-0055) and the `relevantRegisterClaims` helper in
  `internal/manager/server.go` already establish that a claim is
  an operator statement with an owner, and that it "never" becomes
  evidence for health or recovery calculations. An operator may
  register "the ACME endpoint is down until Tuesday"; Apiary
  continues to report `expired`, and the claim appears beside the
  row as context.
- **Alerting is a presentation concern plus a deadman-shaped
  escalation.** The UI and `ClusterHealth` surface the state. For
  "will silently become an outage" escalation, the correct existing
  primitive is `internal/deadman` (ADR-0101): an OS-scheduled
  `at(8)` job, deliberately independent of managerd, "because
  nothing else in this codebase's own supervision (managerd's
  reconciler loop, daemon(8)'s -r auto-restart) can recover a node
  whose own management bridge is wedged by the very VM traffic
  being protected against". A certificate that will expire in N
  days while every Apiary process is wedged is exactly that
  situation. Notification transports are ADR-0127's separate
  "Notifications" item and are not built here; this ADR makes
  `deadman` the extension point and says so.

### D5. The critical safety property: a certificate problem must
never cost quorum

This is the property the whole design bends around, and it is
stated as a testable invariant:

> No certificate operation — issuance, renewal, revocation,
> expiry reporting, or issuer failure — may cause a Combs to lose
> its raft membership.

Seven mechanisms:

1. **Issuance and renewal never touch raft.** `origincert` is
   "deliberately no raft dependency" today and stays that way.
   Renewal is a local timer reading a local directory. A
   partitioned or leaderless Colony can still renew every
   certificate it holds.
2. **The serving path and the consensus path have disjoint key
   material.** `managerd`'s `TLSCert`/`TLSKey` and raft's
   `TLSCert`/`TLSKey`/`TLSCA` are separate files with separate
   lifecycles. A `managerd` serving certificate that expires
   cannot affect the raft transport, because the raft transport
   never read that file. Blast radius: one Combs's UI and API.
   Not quorum.
3. **Renewal failure is non-destructive by construction.**
   `WritePair` validates with `tls.X509KeyPair` and fsyncs both
   staged files before either live path is renamed, and the
   consuming service is restarted only after it returns
   successfully. A failed renewal — bad token, issuer 5xx, DNS
   failure, disk full — leaves the previous, still-valid pair
   exactly where it was, serving. A renewal failure degrades to a
   countdown, never to an outage.
4. **Long validity by default for Colony services.** Origin CA
   permits up to 5475 days and ACME up to 90. Colony service
   certificates are issued at the longest the issuer allows. The
   longer the validity, the more times the renewal path has to
   succeed before it matters — and the more times it can be tested.
   Short validity is a guest-workload feature, not a
   Colony-service feature.
5. **No automatic peer TLS rotation. Ever, in this ADR.**
   `internal/raftdconfig` documents the audit that removed
   `UpdateRaftdConfig` precisely because "changing [raft TLS
   material] on one voter and restarting can isolate that voter and
   lose quorum". This ADR does not add a rotation workflow, does
   not add an RPC, and does not add a UI affordance that
   approaches one. Raft peer certificates remain operator-
   coordinated, per ADR-0113 and ADR-0115. This ADR's obligation
   stops at *telling the operator early and clearly*.
6. **Distinguishing "my serving certificate is bad" from "I
   cannot reach my peers".** These are different failures with
   different blast radii and they must never be conflated. Two
   probes, deliberately separate, reported as separate fields:
   - *Local self-check* — read the certificate and key from
     local disk, verify they form a valid pair, parse
     `notBefore`/`notAfter`, compare against this Combs's own
     clock. Pure local I/O, no sockets. It produces the same
     verdict whether or not the network is up, which is exactly
     why it is trustworthy: it cannot be confused with a
     network symptom.
   - *Peer reachability* — the existing `GetLocalNodeHealth`
     (`api/rpc/manager.proto:27`) and `ClusterHealth` (`:164`)
     path, with its own `ReachabilityUnknown`.
   A failed self-check with successful reachability is "this
   Combs's certificate is bad, its peers are fine" — an operator
   action on one node. Successful self-check with failed
   reachability is a network problem, and the certificate is not
   implicated. Confusing them in either direction is a bug, and
   the test plan tests both directions.
7. **If a voter's raft certificate does expire or fail to
   renew, the response is operator-coordinated removal and
   replacement, not automation.** Apiary's job is to have made
   that situation visible weeks earlier, through evidence that
   cannot be suppressed by the failure itself.

### D6. Issuance without quorum, and a degraded Colony

- **Single-Combs issuance: always available, quorum or not.** It
  is a local filesystem transaction plus one outbound HTTPS
  request. A leaderless Colony can issue, renew, and revoke.
  `ListOriginCertificates` is a local read and
  `IssueOriginCertificate` is a local write; neither is
  leader-gated today and this ADR keeps them that way.
- **Fleet-wide issuance ("issue this certificate for every
  Combs"): requires quorum, and says so.** This is the one
  genuinely distributed operation: it must apply the same
  hostname set to N Combs, and a partial application is a
  configuration the operator did not ask for. Without quorum the
  RPC returns a plain error and **issues nothing**. It never
  issues to a subset "as far as it got" — a partially-provisioned
  fleet is strictly worse than a clean refusal, and the operator
  cannot see which half succeeded.
- **Observation reporting is best-effort and non-blocking.** A
  Combs that cannot reach the leader records its observation
  locally and retries on the next tick. A stale observation is
  reported as stale — surfaced with its own `observed_at` and
  aged into `unknown` rather than being silently presented as
  current. This is the same discipline `ClusterHealthResponse`
  already documents: `local_node_id` exists because "a consumer
  needs it to interpret per-node evidence, since the answering
  node's own reachability is true by definition rather than by
  observation."
- **Renewal under degradation is the interesting case.** If the
  Colony is degraded but a Combs still has internet egress,
  renewal proceeds normally. If it does not, renewal fails,
  `errors.Join` collects it, the old certificate keeps serving,
  and the Combs reports `expired-soon` with the error attached.
  Nothing retries in a tight loop, and nothing escalates beyond
  the operator.

### D7. Relationship to ADR-0130 (ZFS replication) and key
distribution

ADR-0130's context notes its ADR dependencies as "PF firewall
(for managed rules), certificate management (for key
distribution), Colony membership".

**This ADR does not own ZFS key distribution, and does not
provide it.** Native ZFS encryption needs a dataset key wrapped
under a wrapping key; distributing *that* is a different problem
with different failure semantics — a lost wrapping key loses data
permanently, where a lost certificate is a re-issuance. ADR-0130
may reference this ADR's custody rule (D3) as a constraint —
node-local keys, 0600, never raft plaintext — and must not extend
it to ZFS key material without a design of its own. If ADR-0130
needs a Colony-controlled trust anchor to wrap against, that is
trigger 3 in D2's change-of-answer list, and the right sequence is
to write that design first, then revisit D2. This ADR
**constrains** ADR-0130; it does not **own** that key.

The same answer covers guest migration (ADR-0128), which needs a
target Combs to be able to decrypt a migrated disk.

## Rejected alternatives

**1. Run an internal CA (step-ca) as the Colony's issuer.**
Rejected; see D2. The decisive argument is that it adds a
cluster-wide, stateful, high-value trust anchor to a system whose
dominant engineering constraint is blast-radius containment, in
exchange for a trust model no Apiary consumer has. Retained as a
documented, enumerated trigger list rather than a plan.

**2. Raft-replicate private keys, encrypted under a cluster key.**
Rejected. It is the only option that makes a certificate problem a
quorum problem: the Colony could not serve a certificate it did not
replicate, so TLS availability would inherit raft's availability,
which is precisely the inversion D5 forbids. It also contradicts
the existing, deliberate design of `internal/origincert` ("no raft
dependency") and the audit in `internal/raftdconfig` that removed
config write paths for consensus-critical material. And it buys
nothing: a certificate is served by one Combs, and one Combs
already has the key.

**3. Leader-driven issuance: the leader orchestrates issuance on
every Combs.**
Rejected. It inverts the safety property — a renewal would then
require quorum, so a partitioned Colony could not renew, and an
expired certificate during a partition becomes self-inflicted. It
also contradicts the deliberate design of `Renewer`, whose
`Config` is re-read every tick precisely so it needs no
coordination. A leader-orchestrated *fleet* operation is retained
(D6) for the one case where cross-Combs consistency is the point;
leader-*driven* per-node renewal is not.

**4. Issue guest-workload certificates from the same Origin CA
credential.**
Rejected. Two reasons, one practical and one structural. A Cloudflare
Origin CA certificate is trusted only by Cloudflare's edge; a guest
workload's browser will reject it, so it is the wrong artifact for
the job. And `OriginCATokenFile` is a Colony-wide secret that can
mint for any hostname under the zone — a much broader capability
than a single workload needs. Per-purpose credentials, and for
guest workloads the ACME or `imported` path where the guest's own
domain and key material stay its own.

**5. Model certificate expiry as node health (`false` when
expiring or expired).**
Rejected as a direct violation of ADR-0056, ADR-0118, and
ADR-0122. A Combs whose serving certificate expires will still
replicate, still answer peers, and still hold quorum. Reporting it
as `false` would put a certificate condition into the same field a
consumer reads to decide whether a Combs is safe to remove — and
would make an operator's automated tooling act on a condition that
does not imply what that field means. Expiry is a separate
dimension with its own `unknown`.

**6. Build CRL distribution and OCSP stapling.**
Rejected as premature and unbacked. No supported issuer offers
OCSP for the private-CA case, so half the design would have no
live consumer. Revocation in this ADR is local-first and
issuer-best-effort, recorded honestly in the inventory when the
issuer cannot revoke. Revisit if an issuer that matters starts
offering stapling.

**7. A Kubernetes-style `Certificate` CRD with spec/status and its
own reconcile loop**, as ADR-0127 sketches.
Rejected; see Context. `FSMSnapshotState` is a flat map, not a
CRD substrate, and a spec object with a status field invites
exactly the "status says healthy, reality says otherwise" class of
bug this codebase's evidence rules exist to prevent. The shape
this ADR needs is one more `map<string, ...>` of non-secret facts.

## Consequences

### Positive

- The quorum-safety property is structural rather than
  aspirational: there is no code path from a certificate
  operation to a raft membership change, and the test plan
  asserts it.
- The existing `Issuer` interface is reused rather than replaced.
  Four issuers, one transaction, one inventory format, one renewal
  loop. `internal/origincert`'s tests keep passing unchanged.
- Key custody is already the code's habit; this ADR makes it a
  written invariant and extends it to a new ACME account key that
  would otherwise be the first new secret in the system.
- Operators stop visiting every Combs to find out what
  certificate it holds. The colony-wide inventory is the first
  cluster-scoped view of a per-node fact, and it reuses the
  follower-publishes-own-observation pattern already used by
  ADR-0103's restart leases.
- `unknown` as a first-class expiry state removes the most likely
  source of a false-negative alert in the whole system: a UI that
  shows a green check because a check could not be made.
- Long default validity means the renewal path is exercised many
  times before it is load-bearing, so its failure modes are
  discovered by accident rather than during an incident.

### Negative

- No public PKI for the guest workloads unless the operator runs
  one or reaches an external issuer. A guest needing a
  browser-valid certificate must either have a real domain with
  DNS the Colony controls, or use `imported`. This is a genuine
  capability gap, accepted deliberately.
- Fleet-wide issuance is unavailable exactly when an operator is
  most likely to want it (a degraded Colony). The workaround is to
  issue per-Combs, which is more clicks and is the correct
  trade.
- Revocation is uneven across issuers: strong for Origin CA,
  account-key-dependent for ACME, and nonexistent for `imported`
  and `self-signed`. The inventory must say which case it is in,
  and the UI has to render "revocation is local-only for this
  certificate" without pretending otherwise.
- The ACME account key introduces a new per-Combs secret with its
  own custody questions — who may use it, where it lives, how
  rotation works. RFC 8555's `externalAccountBinding` exists
  precisely because the naive model (a key sitting in a directory)
  is weaker than operators assume. This ADR flags the problem and
  does not solve it.
- Two inventory representations exist — the local
  `inventory.json` (authoritative for its Combs) and the raft
  `CertificateRecord` (a copy, possibly stale) — and every
  consumer must know which it is reading. A stale raft copy
  presented as current would be a correctness bug; the `unknown`
  ageing in D4 is the mitigation, and it needs testing.
- `frontend` and `restshimd` are more work than they look. They
  are clients, not servers; `tlsdial.ManagerDialOption` already
  supports what they need, but wiring `-tls-ca` / `-tls-server-name`
  through `commonconfig` and the frontend's existing flag surface
  is separate plumbing that this ADR names but does not detail.

## Implementation notes

All paths and names below are additions. Nothing here removes or
renames an existing symbol.

### `api/internalpb/state.proto`

`FSMSnapshotState` gains field 10 (5-9 are taken: `auth_enabled`
is 5, `jails` 6, `pending_join_requests` 7, `restart_leases` 8,
`restart_records` 9):

```proto
  // certificates is the Colony-wide view of operator-facing
  // certificate inventory: one non-secret record per (Comb,
  // certificate). Private keys, tokens, CSRs, and certificate PEM
  // are never replicated - a Combs's local inventory.json remains
  // authoritative for that Combs, exactly as it is today
  // (internal/origincert has no raft dependency by design).
  // Included in the snapshot so a raftd -restore (ADR-0051) never
  // silently drops the operator's view of what is deployed where.
  map<string, CertificateRecord> certificates = 10;
```

```proto
enum CertificateExpiryStatus {
  CERTIFICATE_EXPIRY_OK = 0;
  CERTIFICATE_EXPIRY_SOON = 1;
  CERTIFICATE_EXPIRY_EXPIRED = 2;
  // "the check could not be made" - unreadable file, mismatched
  // pair, unparseable issuer metadata, or no observation yet. Never
  // coalesce this into EXPIRED or OK (ADR-0056, ADR-0118).
  CERTIFICATE_EXPIRY_UNKNOWN = 3;
}

enum CertificateIssuer {
  CERTIFICATE_ISSUER_UNSPECIFIED = 0;
  CERTIFICATE_ISSUER_CLOUDFLARE_ORIGIN_CA = 1;
  CERTIFICATE_ISSUER_ACME = 2;
  CERTIFICATE_ISSUER_IMPORTED = 3;
  CERTIFICATE_ISSUER_SELF_SIGNED = 4;
}

message CertificateRecord {
  string id = 1;                    // stable: "<comb_id>/<name>"
  string comb_id = 2;
  string service = 3;            // apiary_managerd, apiary_frontend
  repeated string hostnames = 4;
  CertificateIssuer issuer = 5;
  string issuer_certificate_id = 6; // Origin CA id, ACME serial
  int64 not_before_unix = 7;
  int64 expires_at_unix = 8;
  int64 observed_at_unix = 9;       // when THIS Comb last checked
  CertificateExpiryStatus expiry = 10;
  bool auto_renew = 11;
  repeated CertUsage usages = 12;   // web / api / vpn / email
  string local_cert_path = 13;      // path only, never contents
  // local_key_path is deliberately absent: unlike the existing
  // OriginCertificateInfo, the raft copy does not carry it. The
  // key is readable only on its own Comb anyway, and a replicated
  // field invites someone to "helpfully" put key material next to
  // it later.
  bool revoked = 14;
  int64 revoked_at_unix = 15;
  string revoke_reason = 16;
  // issuer_revoked is true only when the CA itself was told.
  // false + revoked=true means local retirement only, which for
  // imported and self-signed certificates is the only kind that
  // exists. The UI must be able to say this.
  bool issuer_revoked = 17;
  // error carries why the last observation failed. It is the
  // evidence for an UNKNOWN row, so it must never be dropped.
  string error = 18;
}
```

**Failure modes.** Field 10 collides with nothing today, but
`ConfigArchive.format_version` ("independent of
`FSMSnapshotState`'s own field additions") means a `-restore` of
an archive written before this change must not be read as "the
Colony has no certificates" and must not be read as "every
certificate was revoked". An absent `certificates` map restores to
"no observation", which ages to `unknown`, which is correct.
Proto3's default for a missing map is an empty map, so the
distinction between "never had certificates" and "restored from an
old archive" is made by `observed_at_unix` being zero, not by the
map's presence — and the frontend must render zero
`observed_at_unix` as `unknown`.

### `api/rpc/manager.proto`

Four new RPCs on the existing `Manager` service:

```proto
  rpc ListColonyCertificates(ListColonyCertificatesRequest)
      returns (ListColonyCertificatesResponse);
  rpc ObserveCertificates(ObserveCertificatesRequest)
      returns (ObserveCertificatesResponse);
  rpc RevokeCertificate(RevokeCertificateRequest)
      returns (RevokeCertificateResponse);
  rpc IssueColonyCertificate(IssueColonyCertificateRequest)
      returns (IssueColonyCertificateResponse);
```

- `ListColonyCertificates` is a **leader-only read** of the
  replicated `certificates` map, merged with the answering
  Combs's own fresh local observation (its raft copy may be
  stale; its local `inventory.json` is not). It follows the
  `ListAPIKeys` precedent — "leader-only, like `ListAPIKeys` - a
  lightly-loaded cluster" (`api/internalpb/raftd.proto`) — and
  must handle the "answering node is not the leader" case by
  forwarding, not by serving a partial map.
- `ObserveCertificates` is the **follower-originated
  replicated write**, the same shape as `AcquireRestartLease` and
  `RecordRestartCompleted`. It takes a Combs's own non-secret
  observation and applies it. It is *not* leader-gated, because
  the whole point is that a follower publishes what it sees.
  Failure mode to guard: a stale follower overwriting a fresher
  observation. The command must reject a record whose
  `observed_at_unix` is older than the stored one — a
  last-writer-wins field on a periodically-updated map is a
  monotonicity bug waiting to happen.
- `RevokeCertificate` is **local** to the Combs that holds the
  certificate, exactly like `IssueOriginCertificate`. It retires
  the pair, marks the record, attempts issuer-side revocation
  when the issuer supports it, and restarts the service. It never
  requires quorum.
- `IssueColonyCertificate` is the **only quorum-gated**
  operation: leader-only, fan out to each Combs, and **all or
  nothing**. Failure mode: a partial fan-out must roll back or
  refuse to report success. Returning success for a subset is
  worse than returning an error, because the operator's next
  action differs.

`OriginCertificateInfo` (line 1027) stays as-is for backward
compatibility; `ListOriginCertificates` and
`IssueOriginCertificate` keep their current single-Combs,
`managerd`-welded behaviour, and the new RPCs supersede them
functionally rather than deprecating them. A follow-up may
deprecate them; that is out of scope here.

### `internal/certmgr` (new package)

Wraps `origincert`, does not modify it:

- `Observation` — the result of a local self-check, with an
  explicit `Checkable bool` so an unrunnable check produces
  `CERTIFICATE_EXPIRY_UNKNOWN` rather than a default `OK`. The
  self-check reuses `tls.X509KeyPair` and `x509.ParseCertificate`
  only; **it opens no sockets**. This is what makes it separable
  from reachability (D5.6).
- `Renewer` — wraps `origincert.Renewer.RunOnce`, adds the
  per-issuer window, and after each tick publishes the resulting
  observation. Publication failure is swallowed into the
  observation's `error` field; renewal failure is `errors.Join`ed
  and, per the existing `Renewer`, never blocks other
  certificates.
- `Issuers` — registry mapping `CertificateIssuer` to an
  implementation of the existing `origincert.Issuer`, plus the
  `imported` and `self-signed` paths, which do not call any CA.
  `ValidOriginCAValidity` keeps its current role; an equivalent
  bounds check exists for ACME (90-day cap) and self-signed
  (capped, e.g. 397 days, and refused above that so an operator
  cannot accidentally mint a decade-long leaf).
- `Revoke` — local-first, issuer-best-effort, records which of
  the two happened.

**Failure mode.** The renewal tick must be safe to run
concurrently with an operator-triggered issuance. `WritePair`'s
temp-file-then-rename discipline makes the *pair* safe; the
*inventory.json* read-modify-write in `Issue` and `SaveInventory`
is not obviously serialised. If a mutex is added, it belongs in
`origincert` and is a small, separable change.

### managerd handlers

- `ObserveCertificates` is applied by raftd's FSM as a normal
  command; no new leader-only path.
- `ListColonyCertificates` merges and returns. Stale-copy
  handling: any record whose `observed_at_unix` is older than
  `internal/deadman`'s or the inventory's own staleness bound is
  returned with `expiry = UNKNOWN`, not with its last-known value.
- `RevokeCertificate` refuses to revoke a certificate whose
  `service` is currently the raft peer transport's, and refuses
  anything under the raftd config directory. There is no
  legitimate operator request to revoke a consensus trust root
  through this API; the refusal is explicit rather than a
  missing feature.
- All four follow the existing house style in
  `internal/manager/server.go`: return a populated `error` string
  in the response rather than a gRPC error, because these are
  operator-facing and the cause needs to be shown verbatim.

### Frontend

A Certificates page, adjacent to the existing Machine
Configuration Origin CA fields
(`internal/nodeconfig/manager.go:221-225`). Rows are per-Combs
per-certificate, with an expiry chip that has **four** states,
and a `unknown` chip that carries the `error` text as its
tooltip. The rule the component must encode: there is no code path
that renders `unknown` as green, and no path that renders a
missing `observed_at_unix` as anything but `unknown`. Revocation
renders "retired locally" versus "revoked at the issuer"
separately, because they are different guarantees.

### Reconcile loop

The existing tick that calls `Renewer.RunOnce` gains one step:
publish. Order is deliberate — renew first, then observe, then
publish — so the observation describes the state *after* this
tick's renewals, and a node that just renewed does not report
`soon` for one more interval. Nothing in the loop is
quorum-gated, and every failure in it is non-fatal to the loop.

## Test plan

**On macOS, in CI, in unit tests:**

- Self-check classification: valid, inside the window, expired,
  mismatched pair, unreadable file, unparseable PEM, empty
  directory. The last four must all be `UNKNOWN`, never `EXPIRED`
  and never `OK`. This is the single most important table in the
  ADR.
- `CertificateRecord` round-trip through the FSM snapshot, and a
  `ConfigArchive` written by a pre-change binary restoring into a
  post-change one (zero `observed_at_unix` → `unknown`).
- Monotonicity: an `ObserveCertificates` carrying an older
  `observed_at_unix` than the stored record is rejected.
- `IssueColonyCertificate` with one Combs unreachable issues
  nothing and returns an error, not a partial success.
- Per-issuer validity bounds: `ValidOriginCAValidity` still
  rejects 45; the ACME and self-signed bounds reject over-long
  requests.
- Per-issuer renewal window arithmetic, including the floor and
  the case where the window exceeds the certificate's remaining
  life (renew immediately, once, not every tick).
- A `Renewer` tick where one certificate's issuer returns an
  error: the other certificates still renew, the error is joined,
  and the failing certificate's `inventory.json` entry is
  unchanged.
- `RevokeCertificate` against a raft-transport certificate is
  refused.
- Grep-level assertion, as a test: no new package imports
  `internal/raft` for the purpose of writing certificate material,
  and `internal/origincert` still has no raft import. This makes
  D5.1 mechanically enforced rather than a matter of reviewer
  vigilance.

**Only validatable on brood (10.90.0.94) or drone (10.90.0.95),
the bare-metal FreeBSD 16.0-CURRENT testbed:**

- **The quorum-safety property itself.** Configure a multi-Combs
  Colony with a long-lived Origin CA certificate on
  `managerd`; let it expire deliberately; confirm raft keeps
  quorum and the Combs stays a member. This is the assertion
  that cannot be proven on macOS, because it requires a real
  raft transport and real timers.
- **Real clock skew and real expiry.** A Combs whose clock is
  minutes fast reports `soon` early; a Combs whose clock is slow
  reports `expired` late. The self-check uses the local clock and
  this is only observable with real drift.
- **Renewal under real network conditions**: a token that works
  on one tick and not the next, DNS failure, an issuer that is
  slow enough to exceed a tick interval, and disk pressure during
  `WritePair`. `WritePair`'s fsync-then-rename discipline is
  meaningless on a filesystem that never drops pages; only real
  SSD and a real power-loss-adjacent window exercise it.
- **Restart timing.** Confirm that `services.Restart` after a
  successful `WritePair` does not drop in-flight gRPC streams
  on the listener, and that a restart is not scheduled when
  `WritePair` fails.
- **ACME end to end** against a staging endpoint with a real
  domain, including HTTP-01 through the existing `dns.go` path
  and a real `at` renewal. macOS has no `at(8)`/`atd(8)`
  convention worth testing against, and `internal/deadman` is
  built on it.
- **Firewall interaction** (ADR-0127's PF item, which ADR-0130
  also depends on): a certificate renewal that must reach an
  external CA through a PF-filtered egress path, and one that
  must *not* be able to reach the Colony's own network.
- **Genuine expiry-and-rotate of a peer certificate** under
  operator coordination, confirming that ADR-0115's derived
  `peer_tls_hostname_map` needs no edit — and confirming that no
  Apiary code path performed the edit.

**Explicitly not verifiable on macOS:** PF rules, ZFS dataset
properties, HAST, jail, bhyve, TLS handshake timing under real
network latency, and anything about whether a certificate
actually keeps a browser or `curl` on a guest happy. macOS can
verify the classification logic and the custody invariants. It
cannot verify the claim this ADR exists to make.

## Open questions

1. **ACME account custody.** Per-Combs account key (each Combs
   registers independently, rate limits multiply) or one Colony
   account key (single point of failure, and revocation of any
   certificate requires the key on that Combs)? The naive answer
   — a key file in a directory — is weaker than operators assume,
   and RFC 8555's EAB exists for this. Is EAB in scope?
2. **Is the peer raft certificate's expiry even in scope to
   report?** It is genuinely useful and genuinely dangerous to
   surface. My recommendation is to report it as a *dedicated,
   prominently worded* warning with no action button, never as an
   ordinary row. Confirm.
3. **Which services are in the first fleet-wide issuance
   support?** `managerd` and `frontend` are clear;
   `restshimd` is a client and may not need one at all. And does
   any *guest workload* get certificates from the Colony, or is
   that always the guest's own business via `imported`?
4. **Does `frontend` need `-tls-ca` / `-tls-server-name` flags,**
   or does it always use the system pool and a publicly-valid
   certificate? This determines whether a private CA is needed on
   the client side, and my analysis says no, but it is worth an
   explicit decision before the plumbing is written.
5. **Notification transport.** ADR-0127 lists Notifications
   (ntfy, SMTP, Discord) as a separate Phase 3 item. Should a
   soon-to-expire certificate be a first notifier consumer, or
   should this ADR only expose the state and let the notification
   work subscribe to it?
6. **Retention of revoked records.** Forever, or pruned after some
   period? Keeping them forever means a `certificates` map that
   only grows across years of churn. Pruning means losing the
   record that a hostname's key was ever here.

## References

- ADR-0051 — `raftd-config-save-restore`
  (`ConfigArchive`, `FSMSnapshotState` provenance)
- ADR-0055 — automated assumption checks v1
  (`internal/assumptions`)
- ADR-0056 — evidence-aware health v1 (`unknown` vs `false`)
- ADR-0063 — Cloudflare Tunnel exposure v1
  (`internal/cloudflare/sidecar.go`, `tunnel.go`)
- ADR-0072 — Cloudflare Origin CA certificate lifecycle
- ADR-0077 — Origin CA certificate expiry health and renewal
  (`internal/origincert/renew.go`)
- ADR-0078 — raft transport TLS (`internal/raft/tls_transport.go`)
- ADR-0101 — dead-man's switch (`internal/deadman`)
- ADR-0103 — restart preflight guardrail (the
  `AcquireRestartLease` / `RecordRestartCompleted` follower-write
  pattern)
- ADR-0113 — TLS trust prompt on Colony join
- ADR-0115 — automatic derivation of `peer_tls_hostname_map`
- ADR-0118 — why-not / quorum-blocker detail
- ADR-0119 — replica freshness and maintenance wave planner
  (the wave model a future peer-certificate rotation would have to
  respect)
- ADR-0122 — cluster evidence-aware health API
  (`ClusterHealth`)
- ADR-0124 — raftd offline status
- ADR-0127 — Sylve.io features (Phase 3; the Certificate CRD
  sketch and the internal-CA question this ADR answers)
- ADR-0128 — guest migration (a target Combs must be able to
  decrypt)
- ADR-0130 — ZFS dataset replication (constrains this ADR on
  key distribution)
- `internal/origincert/{store,issue,renew}.go`
- `internal/cloudflare/{origin_ca,dns,tunnel,sidecar}.go`
- `internal/tlsdial/tlsdial.go`
- `internal/raft/tls_transport.go`
- `internal/raftdconfig/manager.go` (the 2026-09-15 audit that
  removed `UpdateRaftdConfig`)
- `internal/nodeconfig/manager.go` (`TLSCert`, `TLSKey`,
  `PeerTLSCA`, `OriginCATokenFile`, `OriginCADirectory`)
- `api/internalpb/state.proto` (`FSMSnapshotState`,
  `ConfigArchive`, `ApiKey`, `RestartLease`)
- `api/rpc/manager.proto` (`ListOriginCertificates`,
  `IssueOriginCertificate`, `OriginCertificateInfo`,
  `GetLocalNodeHealth`, `ClusterHealth`)
- `internal/health/health.go` (`StatusUnknown`,
  `ReachabilityUnknown`, `SuffrageUnknown`)
- `cmd/managerd/main.go` (`credentials.NewServerTLSFromFile`)
