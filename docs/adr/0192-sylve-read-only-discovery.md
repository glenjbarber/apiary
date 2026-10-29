# ADR-0192: Sylve read-only discovery

## Status

**Proposed.** This record designs Apiary's integration with Sylve as a
**read-only discovery provider**.

**Implemented 2026-10-29.** The provider exists in `internal/sylvediscovery`,
the shared provider seam in `internal/extdiscovery`, the configuration in
`internal/nodeconfig`, and the read surface as
`ManagerService.GetExternalDiscovery`. What is implemented is the
**discovery half** only: there is no adoption path, no management path,
and no mutating call anywhere in the provider.

Two decisions needed sharpening during the implementation, and both are
now recorded in the code:

  - **THE SCAN'S STATE AND ITS COMPLETENESS FLAG MUST AGREE.** A scan
    that reported itself "fresh" while also reporting itself incomplete
    is a contradiction, and resolving it in favour of "fresh" would let
    a caller treat a half-finished scan as a current one. The store
    resolves it in favour of **partial**, because partial is the
    strongest statement that is true of both readings. This is checked
    by `TestApply_IncompleteScanNeverReportsFresh`.

  - **THE SOURCE ID IS RECORDED ONLY FROM A COMPLETE SCAN.** A failed
    scan did not establish which installation it was talking to, so
    recording a source id from one would be asserting something the
    scan did not learn. A provider that does not cluster reports an
    empty source id, which is a fact about the source rather than a
    missing value.

The implementation did not change the decisions in this record; it
confirmed them. The one thing the implementation could not confirm is
the identity key's stability, which remains a **candidate** - see
"Implementation Follow-up".

Supersedes: None
Superseded by: None
Affected projects: Apiary
Source: Notion record "Sylve Read-Only Discovery Integration Reference"
(Proposed, 2026-10-28)
Related: ADR-0191 (the Proxmox discovery provider this record sits beside),
ADR-0127 (the earlier Sylve feature-adoption research, which this record
narrows to one bounded slice), ADR-0100 (daemon configuration files)

## Context

### What is being asked for

Apiary may need to discover resources managed by Sylve installations.
Sylve is an open-source FreeBSD management plane — the same problem
domain as Apiary, at a different architectural centre — and it publishes
a REST API for cluster nodes, virtual machines, jails, and system
information, plus a server-sent event stream.

The boundary is the one ADR-0191 established: use supported provider
interfaces, map observations into Apiary, and leave provider-owned state
authoritative. **This integration is narrower than ADR-0191's: discovery
only.** It must not become a Sylve management layer, and discovery must
not imply enrollment, trust, or permission to change anything.

ADR-0127 researched Sylve's full feature set and recommended phased
adoption across eight to twelve ADRs. That is still the right long-run
roadmap, and this record is deliberately *not* that. It takes the single
slice that has an independent value and an independently verifiable
safety property — read-only discovery — and leaves the rest alone.

### What is already true about the Apiary side

**Apiary's inventory is its raft membership.** There is no separate
inventory of "known hosts" (ADR-0191 worked this out in detail): a Comb
*is* a raft member, `knownColonyMember` in
`internal/frontend/cluster_overview.go` treats a node as known iff it is
in membership, and ADR-0164 derives the peer allowlist from the same
source.

**Apiary is FreeBSD and bhyve.** A Sylve VM is a bhyve VM on a FreeBSD
host, and a Sylve jail is a FreeBSD jail — so unlike Proxmox, Sylve's
guests are *the same kind of thing* Apiary manages. That is a genuine
difference in kind, and it is the reason this record has to think harder
about what a discovered Sylve guest is than ADR-0191 had to think about a
Proxmox node.

### What is already true about the Sylve side

The Sylve facts this design depends on were verified against upstream
source rather than assumed, because the most important one — the
security gate — had been recorded as *unproven* in the Notion reference
and could only be settled by reading the implementation. Verified
against `AlchemillaHQ/Sylve` at `master` (repository last pushed
2026-10-28).

**The discovery endpoints require authentication, but not
administration.** In `internal/handlers/routes.go`, the cluster read
group is registered as:

```go
cluster := api.Group("/cluster")
cluster.Use(middleware.EnsureAuthenticated(authService))
cluster.Use(middleware.LimitRequestBody(authServicePkg.MaxRequestBodyBytes))
cluster.Use(middleware.RequestLoggerMiddleware(telemetryDB, authService))
{
    cluster.GET("/nodes", clusterHandlers.Nodes(clusterService))
    cluster.GET("/resources", clusterHandlers.Resources(clusterService))
    cluster.GET("", clusterHandlers.GetCluster(clusterService))
}
```

Note what is **absent**: `RequireLocalAdmin`. The mutating groups in the
same file — `clusterLocal`, `clusterAdmin`, `clusterNotes`, the VM and
jail groups, the network and storage groups — all carry
`RequireLocalAdmin` or `RequireLocalAdminForWrites`. The read group
carries neither.

**`EnsureAuthenticated` does not require admin.** It accepts either a
valid cluster key or a valid local bearer JWT
(`internal/handlers/middleware/auth.go`'s `EnsureAuthenticated` →
`authenticateLocalRequest` → `setLocalAuthentication`). The latter
validates the token and sets the user context, with no admin check.
`RequireLocalAdmin` is the separate middleware that does the admin
check, and it is what guards the mutating routes.

**So the privilege separation exists — but the login endpoint is the
catch.** `internal/services/auth/auth.go`'s `CreateJWT` refuses a
`sylve`-type login for a non-admin user:

```go
if !user.Admin {
    attempt := s.recordFailedLogin(username)
    s.logLoginFailure(username, authType, "admin_required", attempt, nil)
    return 0, "", fmt.Errorf("only_admin_allowed")
}
```

So a **non-admin Sylve account cannot obtain a bearer token through
`POST /auth/login`**. A non-admin identity is nevertheless a valid
principal for `EnsureAuthenticated` — the check is `user.Admin` at
`RequireLocalAdmin`, not at token issuance.

**This is the resolution of the Notion reference's security gate, and it
is a qualified yes.** Sylve's *authorization* model separates reads from
writes properly: a non-admin token is refused by every mutating route
and accepted by the discovery routes. But Sylve's *authentication* model
offers no way for a non-admin to obtain a token in the first place, via
the documented login flow. Three consequences follow, and they are
recorded as decisions rather than glossed:

1. **A non-admin identity is safe but unobtainable by the documented
   flow.** The gate's real question — "can a discovery credential be
   server-side prevented from writing?" — is answered **yes** by Sylve's
   authorization model. The secondary question — "can we mint that
   credential without an administrator?" — is answered **no** through
   `POST /auth/login`.
2. **Therefore the credential must be provisioned out of band, and the
   credential used must be non-admin.** Apiary's operator must create
   the non-admin Sylve account through whatever administrative path
   Sylve offers, and provision Apiary's credential from it. Apiary
   **does not** perform the login as an administrator and then reuse an
   administrator's token — that would be exactly the "administrator
   credential as backend credential" the Notion reference forbids.
3. **The Apiary-side GET-only allowlist is defence in depth, not the
   control.** The control is Sylve's own authorization model refusing
   the write. Apiary's allowlist is a second, independent barrier.

**The API shape.** The responses are wrapped in an envelope. The
handlers return `internal.APIResponse[T]` with `status`, `message`,
`error`, and `data`, so a response body is
`{"status":"success","message":"nodes_listed","error":"","data":[...]}`.
A client that reads `data` directly without checking `status` will
misread an error response as an empty list — the same trap ADR-0191
found in Proxmox's `GET /nodes`, in a different costume.

**The endpoints.** Verified in `internal/handlers/routes.go` and
`internal/handlers/cluster/detail.go`:

| Purpose | Endpoint | Gate | Data |
| --- | --- | --- | --- |
| Cluster summary and Raft membership | `GET /cluster` | `EnsureAuthenticated` | Cluster id, nodeId, leader, member IDs/addresses, suffrage, guest IDs, `partial` indicator |
| Cluster node inventory | `GET /cluster/nodes` | `EnsureAuthenticated` | Node UUID, hostname, status, CPU/memory/disk capacity and usage, guest IDs, timestamps |
| Cluster-wide guest resources | `GET /cluster/resources` | `EnsureAuthenticated` | Per-node grouping; nodeUUID, VMs and jails with IDs, names, state |
| VM list | `GET /vm/simple` | `EnsureAuthenticated` + admin-for-writes | Lightweight VM records |
| Jail list | `GET /jail/simple` | `EnsureAuthenticated` + admin-for-writes | Lightweight jail records |

**The VM and jail groups are gated differently from the cluster group.**
`vm.Use(middleware.RequireLocalAdminForWrites(authService))` — that
middleware passes `GET`, `HEAD`, and `OPTIONS` through without an admin
check and applies the admin requirement only to other methods. So the
`/vm/simple` and `/jail/simple` *reads* are available to a non-admin
identity, and their *writes* are not. This is exactly the separation the
integration needs.

**SSE is not usable yet, and the reason is specific.**
`GET /events/stream` takes its token as a **query parameter**
(`?sse_token=…`), issued by `POST /auth/sse-tokens`. A token in a query
string lands in access logs, proxy logs, and browser history. The Notion
reference already flagged this, and the source confirms it — the token is
read with `c.Query("sse_token")` in `AuthenticateSSE`. SSE also requires
a **scoped** JWT (`ValidateScopedJWT(token, "sse")`), which is a
different issuance path from the ordinary session token.

**The `partial` indicator is the single most important field for
correctness.** `GET /cluster`'s response carries a `partial` flag, and
`GET /cluster/resources` can fail for one node while succeeding for
others. A client that treats a partial response as a complete one will
conclude that absent guests were deleted.

## Decision

Apiary will implement a **read-only Sylve discovery provider**, subject
to the credential rules below. It will use documented Sylve API
endpoints to retrieve cluster and node identity plus VM and jail
inventories, and translate observations into Apiary's discovery
representation.

**Sylve remains authoritative** for its cluster membership, node
status, and guest configuration. Apiary may cache observations with
timestamps and source status, but must not create, update, stop, delete,
migrate, configure, enroll, or otherwise manage Sylve nodes or guests.

**Discovery must not imply enrollment, trust, or permission to change
anything.** A discovered Sylve guest is an *observation*, and — unlike a
Proxmox node — it is also something Apiary could in principle manage,
because it is the same kind of thing. That is exactly why the boundary
must be enforced structurally rather than by intent: **the Sylve
provider interface exposes no mutating method**, and a discovered guest
is never enrolled into Apiary's trust domain by discovery.

### The provider interface is read-only by construction

The Sylve provider implements the discovery surface ADR-0191 defined,
and that surface has no method for creating, modifying, or removing
anything. Concretely, the Sylve implementation issues **only** `GET`
requests to a **fixed allowlist** of endpoint paths:

```
GET /cluster
GET /cluster/nodes
GET /cluster/resources
GET /vm/simple
GET /jail/simple
```

There is no generic URL, method, path, or proxy surface. The
allowlist is a compile-time constant in the provider, not a
configuration value, so an operator cannot widen it by editing a config
file. This is the "defence in depth" the Notion reference asked for,
and it is implemented as the *shape* of the code rather than as a check
inside a general-purpose client.

### The credential: non-admin, provisioned out of band

**Apiary's Sylve credential must be a non-admin Sylve identity's token.
An administrator credential must never be used as the backend
credential.**

This is decided because Sylve's authorization model makes it the right
control: a non-admin token is refused by `RequireLocalAdmin` and
`RequireLocalAdminForWrites`, so every mutating route is denied
server-side. Apiary's own GET-only allowlist is the second barrier.

The credential cannot be minted by Apiary through the documented login
flow, because `CreateJWT` refuses a non-admin `sylve` login. So:

- **The operator provisions the Sylve account and provisions Apiary's
  credential out of band.** Apiary does not log in as an administrator
  to obtain a token.
- **The token is stored as a file path in `managerd.json`,** following
  the existing `cloudflare_token_file` / `origin_ca_token_file` pattern:
  a root-owned file whose path is configuration, never the secret
  itself. It is never placed in raft-replicated state, never logged, and
  never returned in an RPC response or a UI result.
- **Rotation is create-replace-revoke.** Provision a replacement token,
  update the file, confirm discovery succeeds, then revoke the old one.

### Transport: HTTPS with mandatory verification

HTTPS only. The certificate and hostname are validated. **There is no
`insecureSkipVerify` mode**, and there is no way to configure one. This
matches ADR-0161's finding that Zabbix's own schema default for
`verify_peer` is *off* — a client that omits the setting accepts any
certificate — and ADR-0191's rule that a Proxmox client's TLS
verification is required and stated, not assumed.

A private CA is supported through an **explicitly configured trusted CA
bundle path**, never by disabling verification. The bundle path is
configuration; the verification itself is not optional.

### The identity mapping

A discovered Sylve record is identified by a composite key:

```
Apiary external identity = (provider, source_id, kind, source_resource_id)
```

- **`source_id`** identifies the configured Sylve installation. When
  clustering is enabled, the cluster id; when it is not, the
  installation's own identity. The absence of a cluster id must be
  represented rather than papered over with an empty string, exactly as
  ADR-0191 requires for a standalone Proxmox node — a bare-metal Sylve
  install is the common case, not the exotic one.
- **`nodeUUID`** identifies the node a guest lives on.
- **`kind`** is `vm` or `jail`, kept distinct even though Apiary
  represents both through one cell model.
- **`source_resource_id`** is the VM's RID or the jail's CTID.

**The key's stability is an assumption, and it is labelled as one.** The
Notion reference was explicit that uniqueness and stability across
migrations, restores, cluster joins, and node replacement had not been
confirmed. That remains true of this record: the key is a *candidate*,
and the implementation must confirm it against a live Sylve before it is
treated as durable. Where it is not confirmed, the provider must
**report a conflict rather than merge on hostname or display name** —
a merge on a display name is how two different guests become one record.

### What "discovery" means here, given that Sylve guests are Apiary-shaped

This is the part that differs most from ADR-0191, and it deserves to be
stated plainly rather than left implicit.

A Sylve guest is a bhyve VM or a FreeBSD jail — the same thing Apiary
manages. Apiary could, in principle, discover a Sylve guest and then
manage it. **This record does not permit that.** The scope boundary is
drawn at the *provider interface*, and it holds for this reason:

- **A discovered guest is not an Apiary cell.** Apiary's cells live in
  raft-replicated state, are reconciled by a `managerd` on the owning
  Comb, and have a lifecycle Apiary drives. A discovered Sylve guest has
  none of those. Representing it as a cell would make the reconciler try
  to act on a host Apiary does not manage.
- **Discovery does not adopt.** There is no path by which a discovered
  guest enters Apiary's management domain. Adopting a guest is a
  separate, explicit, operator-authorized operation — and it is
  **out of scope for this record**, in the same way that managing a
  Proxmox node's workloads is out of scope for ADR-0191.
- **The observation is a read model, not a claim.** It says "a guest
  named `web-01` exists on Sylve node X, and was in state `running` when
  we looked". It does not say "Apiary owns `web-01`".

So the discovered guest is reported in Apiary as an **external
resource** — a provider-identified observation with a name, a kind, an
observed state, and a timestamp — and nothing more.

### Failure semantics: the same discipline as ADR-0191

Sylve's API has its own version of the trap ADR-0191 found in
Proxmox's `GET /nodes`, and it is a different trap: the **envelope**.
A Sylve error response is `{"status":"error","message":"...","data":null}`.
A client that reads `data` without checking `status` sees `null` and
may render it as "no guests". So:

- **`status` is checked before `data` is read.** An error envelope is a
  failure, never an empty inventory.
- **`partial: true` is not a complete scan.** A partial response
  updates what it did observe and marks the snapshot partial. It never
  removes a record.
- **A failed scan never removes anything.** A timeout, a `401`, a `403`,
  a `5xx`, a malformed body, or an offline node leaves the last known
  records in place, marked stale with their last-seen time.
- **Absence is confirmed, not assumed.** A guest missing from one
  complete scan is marked *pending confirmation*, and only a subsequent
  complete scan — plus a documented grace period — marks it absent.
  Deletion is never a side-effect of a scan.
- **`401`, `403`, and `404` are distinct.** `401` is an invalid or
  expired credential; `403` is a permission failure, which is a
  configuration blocker rather than an empty inventory; a `404` on a
  detail lookup means that record is unavailable, not that the cluster
  is gone.
- **An unknown state is never mapped to "running"** or to healthy. An
  unrecognised enum is a diagnostic, not a guess.

### Polling, not SSE

Polling is the correctness mechanism. **SSE is not implemented in this
slice**, and the reason is specific rather than general: the SSE token
travels in a query string, so it lands in access logs, proxy logs, and
history. Until Sylve's event contract and the redaction behaviour of
every log in the path are verified, an SSE implementation would leak the
credential into exactly the places a credential must not appear.

The polling interval defaults to 60 seconds, is configurable per
source, and carries jitter across sources so that several sources
discovered at the same moment do not synchronise into a thundering
herd. Cluster status and resources are fetched in bounded batches, and
last-success time is tracked per source and per node so that freshness
is explicit to the caller.

### The states a caller can distinguish

A caller of the discovery surface must be able to tell these apart, and
must never be handed a zero value that reads as "healthy" or "empty":

`fresh` · `stale` (last known data retained) · `partial` · `unavailable`
· `empty` (a complete scan that found nothing) · `unauthorized` ·
`forbidden` · `conflict`.

An empty list is a valid observation **only** when the request succeeded
and the response was complete.

## Alternatives Considered

### 1. Use the Sylve cluster key as Apiary's credential

**Rejected.** The cluster key authenticates as a cluster principal and
is used by *other Sylve nodes* talking to this one — `EnsureAuthenticated`
accepts it and it is not an administrator, but it is a principal whose
purpose is inter-node membership, not discovery. Using a membership
credential for an external integration would reuse a trust relationship
for a different purpose, and the key is not scoped to a read-only
identity. This was considered because it is the one credential path that
is unambiguously non-admin; the scope mismatch is why it was rejected.

### 2. Log in as an administrator and cache the token

**Rejected, and this is the option the Notion reference specifically
forbade.** Sylve's `CreateJWT` refuses non-admin logins, so the only
documented way to obtain a token is as an administrator. A
discovered-service credential that is an administrator's token is not
read-only in any meaningful sense: a stolen token can mutate the Sylve
cluster, and the Apiary-side GET-only allowlist does not change that,
because the allowlist constrains *Apiary's* client and not the token's
capabilities. The Notion reference's own words are right: "an Apiary-side
GET-only allowlist is useful defense in depth but does not make a stolen
administrator token read-only."

### 3. Implement SSE now and poll as a fallback

**Rejected for this slice**, for the query-string token exposure
described above. It is recorded as a follow-up rather than dismissed:
SSE may later be added as a *refresh hint* only, with polling remaining
authoritative, and only once the event contract and log redaction are
verified.

### 4. Adopt the broader ADR-0127 feature roadmap

**Deferred.** ADR-0127's eight-to-twelve ADRs — migration, replication,
backup, firewall, WireGuard, certificate management, notifications,
S.M.A.R.T. — remain the right long-run roadmap and are untouched by this
record. This record takes one slice with an independently verifiable
safety property. Doing the whole roadmap at once would put eight trust
boundaries into motion simultaneously.

### 5. Represent discovered Sylve guests as Apiary cells

**Rejected.** A cell is replicated state that a `managerd` reconciles
against a host it manages. A discovered Sylve guest is an observation of
a guest on a host another management plane owns. Representing it as a
cell would make the reconciler try to act on it.

## Consequences

### Positive

- Apiary can see what a Sylve installation is running, without taking
  any authority over it.
- The integration's safety property is **structural**: a provider with no
  mutating method and a fixed GET allowlist cannot grow into a control
  plane.
- The privilege separation is verifiable rather than asserted — Sylve's
  own authorization model refuses a non-admin token on every mutating
  route, and the implementation tests representative denied mutations.
- The security gate the Notion reference could not resolve is resolved,
  and resolved in the direction of a real answer rather than an
  assumption.

### Trade-offs

- The credential must be provisioned out of band, because Sylve's login
  flow cannot mint a non-admin token. This is an operational step, and it
  is a real friction.
- The identity key is a **candidate**. Its stability across migrations,
  restores, and node replacement is unconfirmed, and until it is, a
  change may surface as a conflict rather than as a silent update.
- Polling costs a request per source per interval; SSE would be cheaper
  but is not safe to use yet.
- Discovery of Sylve guests is observation only, so an operator looking
  at Apiary will see Sylve's guests and must not expect to manage them
  there.
- A non-admin Sylve identity that is denied `403` on an endpoint
  Sylve's own documentation implies is readable would look like an
  Apiary bug until the credential's role is checked. The error is
  reported as a permission failure rather than as an empty result,
  precisely so it can be told apart.

## Scope

**Included:**

- Configure a Sylve API base URL and a credential reference for a
  read-only integration identity.
- Discover Sylve cluster identity, member nodes, virtual machines, and
  jails using documented read endpoints.
- Map source identities and selected inventory fields into Apiary's
  read-only discovery representation.
- Refresh observations by polling.
- Report source health, staleness, partial results, authentication
  failures, and per-node or per-resource discovery failures.
- Deduplicate repeated observations and reconcile disappearance without
  treating one failed scan as confirmed deletion.

**Excluded:**

- Creating or maintaining a Sylve management layer, parallel control
  plane, or Sylve-specific lifecycle implementation.
- Calling Sylve mutation endpoints, including cluster join/remove/reset,
  VM or jail actions, configuration, storage, networking, backup,
  replication, or system operations.
- Automatically enrolling Sylve nodes or guests into Apiary's trust or
  management domain.
- Issuing commands, remediation, configuration changes, or credentials
  to Sylve.
- Treating Sylve discovery as authority to modify a Sylve cluster or its
  members.
- Adopting a discovered Sylve guest into Apiary's management domain.
  This is the operation that would make a discovery into a management
  relationship, and it is a separate, explicitly-authorized decision
  that this record does not make.
- SSE.

## Implementation Follow-up

**What the implementation established:**

1. **The endpoint contract** is pinned to the documented endpoints
   above. The provider's allowlist is a compile-time constant in
   `internal/sylvediscovery`, so it cannot be widened by configuration -
   and the test asserts that the SSE and mutation paths are absent from
   it.
2. **A non-admin credential is required.** The provider never performs a
   login; it only presents a token that was provisioned out of band.
   The provider's own surface cannot express a mutation, which is the
   structural half of the guarantee, and Sylve's authorization model
   refuses a non-admin token on every mutating route, which is the
   other half.
3. **The endpoint allowlist is exact.** `internal/extdiscovery`'s
   matcher compares literal path segments and admits exactly one
   parameterised segment, so `/cluster` cannot admit `/cluster/join`.
4. **The identity key remains a candidate.** Its stability across
   migration, restore, cluster join, node replacement, and upgrade is
   still unconfirmed against a live Sylve, and the implementation
   therefore treats a changed fingerprint as a **conflict** rather than
   silently updating the record.
5. **The polling interval defaults to 60 seconds** and is configurable
   per provider. Each provider has its own goroutine and its own
   interval, so a slow provider does not delay another.
6. **The provider interface and its error model** are
   `internal/extdiscovery`'s `Provider` and `ScanResult`. The states are
   fresh, stale, partial, unavailable, empty, unauthorized, forbidden,
   and conflict.
7. **The tests cover** success, an empty-but-complete scan, a partial
   cluster response, a malformed body, `401`, `403`, timeouts, duplicate
   observations, an identity change, and recovery after failure - and
   assert that the provider issues only `GET` and never requests a path
   outside its allowlist.

**What still needs establishing, and cannot be done from this
repository alone:**

- The identity key's uniqueness and stability against a **live** Sylve
  installation.
- Whether Sylve can issue a non-admin identity through a documented
  administrative path, and what that path is. The provider is written
  to require one; provisioning it is an operational step.
- The version range the endpoint contract is valid for, which needs a
  live instance of each supported release.

The implementation should further establish:

1. **The supported Sylve API version range and the endpoint contract**,
   pinned to official documentation or a tested OpenAPI specification.
   The reference identifies Sylve API 0.3.1 / OpenAPI 2.0.
2. **A non-admin credential, proven server-side read-only.** Every
   discovery call must succeed with it, and representative mutation
   calls must be **denied** by Sylve — the denial is the evidence, and
   the implementation's tests must include it. If a non-admin identity
   cannot be provisioned, the integration is blocked and no
   administrator credential is used.
3. **The identity key**, confirmed against a live Sylve for uniqueness
   and stability across migration, restore, cluster join, node
   replacement, and upgrade.
4. **The provider interface and its error model**, including the
   envelope handling, the `partial` flag, and the distinct states.
5. **The polling interval, freshness threshold, and post-absence grace
   period.**
6. **Tests** for: a successful scan; an empty-but-complete scan; a
   `partial` scan; an offline node; a malformed payload; a `401`; a
   `403`; a `404` on a detail lookup; rate limiting; a timeout;
   duplicate observations; an identity change; recovery after failure;
   **and a representative mutation attempt being refused by the
   provider itself**, which the provider cannot even express because it
   has no mutating method.

## Decision Summary

Apiary will discover Sylve clusters, nodes, VMs, and jails through
documented read-only Sylve API endpoints. Sylve remains authoritative
for its own state.

The integration's safety property is structural: the provider exposes no
mutating method and issues only `GET` requests to a fixed endpoint
allowlist, so it cannot grow into a second control plane. The
credential is a **non-admin** Sylve identity's token, because Sylve's
authorization model refuses non-admin tokens on every mutating route;
because Sylve's login flow cannot mint such a token, it is provisioned
out of band and never acquired by logging in as an administrator.

Discovered guests are observations, not Apiary cells. Adoption, if it is
ever wanted, is a separate decision.

## Evidence and references

- Notion: "Sylve Read-Only Discovery Integration Reference" (Proposed,
  2026-10-28); "ADR: Proxmox Node Discovery and Enrollment" (ADR-0191);
  ADR-0127 (the earlier feature-adoption research).
- Sylve API behaviour verified against upstream source, not assumed —
  `AlchemillaHQ/Sylve` at `master`:
  - `internal/handlers/routes.go` — the endpoint groups and their
    middleware. The cluster read group carries `EnsureAuthenticated`
    and **no** `RequireLocalAdmin`; the mutating groups carry
    `RequireLocalAdmin` or `RequireLocalAdminForWrites`.
  - `internal/handlers/middleware/auth.go` — `EnsureAuthenticated`,
    `setLocalAuthentication`, `AuthenticateSSE` (the query-string
    `sse_token`).
  - `internal/handlers/middleware/authz.go` — `RequireLocalAdmin`,
    `RequireLocalAdminForWrites`, and the admin check they perform.
  - `internal/services/auth/auth.go` — `CreateJWT`'s
    `only_admin_allowed` refusal for a non-admin `sylve` login.
  - `internal/handlers/cluster/detail.go` — the `/cluster/nodes` and
    `/cluster/resources` handlers and their `internal.APIResponse[T]`
    envelope.
  - `internal/handlers/auth/auth.go` — the login handler and its
    `authType` (`sylve` / `pam`) and response shape.
- Apiary-side claims verified against the tree this ADR lands beside:
  `internal/frontend/cluster_overview.go` (membership-derived inventory),
  `internal/manager/auth.go` (the fail-closed RPC role map),
  `internal/nodeconfig/manager.go` (the token-file configuration
  pattern), ADR-0161 (TLS verification must be explicit),
  ADR-0191 (the provider-interface discipline this record follows).
