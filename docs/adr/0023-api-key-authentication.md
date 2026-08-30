# ADR-0023: API-key authentication for managerd's external API

## Status

Accepted

## Context

Every surface except the web UI's own session login (ADR-0019) was
completely unauthenticated: `raftd`'s internal socket, `managerd`'s
external `ManagerService` gRPC API, and `internal/restshim`'s REST
translation. ADR-0014 and ADR-0019 both flagged this as the next real
gap, and it's what blocked the two "tabled" roadmap items (Terraform,
a hosted `restshim`) from being viable at all — neither can
authenticate without a real credential a script or CI job can hold.
This closes the gap for `managerd`'s external API specifically, the
actual thing outside tooling would hit. Deliberately out of scope:
`raftd`'s own internal Unix socket (already protected by real,
sufficient file permissions — `0o660` plus a `0o700` parent dir,
dialed only by `managerd` locally), a `cmd/restshimd` binary (still
doesn't exist, a separate pre-existing gap), real user/role separation
(this is still one flat set of keys, the same "single-developer/
local-network stage" honesty already applied to the UI password), and
HashiCorp raft's own cross-node TCP transport (unauthenticated at that
layer regardless, unrelated to `ManagerService`).

## New concept: `ApiKey`, ephemeral state like `NetworkDefinition`

A key must validate on *every* node's `managerd`, not just whoever
created it — exactly like `NetworkDefinition`, so it's ephemeral state
replicated through raft: a new `ApiKey` message, `CreateAPIKey`/
`RevokeAPIKey` command variants, `FSM.apiKeys` map, `Snapshot`/
`Restore` extensions. Only a SHA-256 hash of the key is ever stored or
replicated — the raw key is generated once by `managerd`'s
`CreateAPIKey` RPC handler (`crypto/rand`, 32 bytes,
`base64.RawURLEncoding`, an `apk_` prefix for recognizability,
mirroring `internal/frontend/session.go`'s existing token-generation
convention) and returned to the caller exactly once. It is never
persisted or logged in cleartext anywhere, and the external-facing
`APIKeyInfo` type never carries it or its hash back out, even by
accident — `fromInternalAPIKey`/`fromRPCAPIKey` only ever copy
id/name/created time.

`RevokeAPIKey` is a hard delete, no soft-delete tombstone — a key has
no physical resource to reconcile away, the same reasoning already
applied to `DeleteNetwork`.

## Validation deliberately skips the leader-only read convention

`GetVM`/`ListVMs`/`GetNetwork`/`ListNetworks` all require the current
raft leader by design (v1's existing "read consistency" choice). A new
`RaftInternal.ValidateAPIKeyHash` RPC deliberately does **not** follow
that rule — the one exception to the convention in this codebase.
Several `managerd` RPCs need to keep authenticating callers even on a
non-leader node (`HostStats`, `GetVMConsole`, `UploadISO` are all
local, per-node operations that don't otherwise depend on raft
leadership at all); requiring the leader for every single auth check
would make them leader-dependent for no reason. Raft's `Apply` already
replicates FSM state — including `apiKeys` — onto every node, follower
or leader, as they replay the log, so a lookup straight against the
local FSM is just as correct as the leader's, modulo a brief
propagation window right after a create/revoke. `ListAPIKeys` (the
admin-facing key list) is a normal leader-only read, mirroring
`ListNetworks` exactly — only per-request authentication gets the
exception.

## `auth_enabled` is a permanent, one-way flag — not `len(apiKeys) > 0`

The bootstrap rule is: until API-key auth has ever been turned on,
every call (including the very first `CreateAPIKey`) succeeds
unauthenticated, making the feature entirely opt-in and non-breaking
for the already-live `apiarium` deployment. The first implementation
computed this as `len(apiKeys) > 0`, which has a real bug: revoking the
*only* remaining key made that check false again, silently reopening
unauthenticated access to the entire cluster instead of locking it
down. This was caught by the integration test
`TestIntegration_RevokeAPIKey_StopsWorkingImmediately` failing exactly
as designed to catch it — see the parallel bridge-name bug in
ADR-0022 for the same pattern of "self-caught via live/integration
testing, fixed with a regression test" recurring in this project.

The fix: `FSM.authEnabled` is a separate, one-way boolean, persisted
in `FSMSnapshotState.auth_enabled`. `applyCreateAPIKey` sets it `true`
permanently the first time it runs; `applyRevokeAPIKey` never touches
it. `checkAuth` gates on this flag, not on whether any keys currently
exist — so revoking the last key requires a valid key be presented
for every subsequent call, permanently, with **no way back to open
short of restoring an older raft snapshot** (from before the flag was
ever set). This was confirmed for real on `apiarium`: creating a test
key and then revoking it immediately locked out every RPC, including
`CreateAPIKey` itself, requiring a full `raftd` data-directory wipe
(losing that node's ephemeral state — VM/network records, not the
underlying ZFS/bhyve resources) to recover. That lockout is the
feature working exactly as designed, not a bug — but it's a sharp
edge worth this loud a callout, and the `/apikeys` page's own copy
warns about it before the first key is ever created.

## `Status` is the one RPC exempted from the auth check

`checkAuth` runs in `internal/manager/auth.go`'s
`AuthUnaryInterceptor`/`AuthStreamInterceptor`, gating every RPC with
no per-method special-casing — except `Status`. `Status`'s entire
purpose is to report whether `raftd` is reachable, degrading
gracefully (`RaftReachable=false`, `RaftError` set) instead of
erroring when it isn't. But `checkAuth` itself needs to reach `raftd`
(`ValidateAPIKeyHash`) to make its decision — gating `Status` on it
would mean the one call meant to work when raftd is down starts
failing with an opaque "checking API key" error instead of the
diagnostic it exists to provide, masking the very thing it's for. This
was caught the same way as the `auth_enabled` bug: an existing
integration test (`TestIntegration_StatusReportsUnreachableRaftd`)
failed once the interceptor was wired in. `StatusResponse` carries no
secrets (raft reachability/leader info only), so exempting it entirely
is an acceptable, narrow carve-out — not a precedent for adding more
exceptions later.

## `managerd`/`frontend` wiring

- `internal/manager/auth.go` (new file): `apiKeyValidator` local
  interface (mirrors the `VNCLookup`/`VLANStatus`/`isoManager`
  locally-defined-interface convention elsewhere in this package, so
  `checkAuth`'s core logic is unit-testable with a fake, no real raftd
  needed), `hashAPIKey`/`generateAPIKey`/`generateAPIKeyID`,
  `extractBearerToken` (reads the `authorization` gRPC metadata key,
  `Bearer <key>` convention — chosen so a future `restshim` binary
  could forward the same header value largely unchanged from REST
  clients), and the interceptors themselves. This is the project's
  first use of `grpc.UnaryServerInterceptor`/`grpc.StreamServerInterceptor`
  anywhere, wired via `grpc.NewServer(grpc.UnaryInterceptor(...),
  grpc.StreamInterceptor(...))` in `cmd/managerd/main.go`. `UploadISO`
  (the one streaming RPC) is checked once at stream-open, same as
  every unary call.
- `cmd/frontend/main.go`: a new `APIARY_MANAGER_API_KEY` env var
  (matching the existing "credentials come from env, not flags"
  convention already used for `APIARY_UI_USER`/`PASSWORD`), attached
  via `grpc.WithPerRPCCredentials` on the dial to `managerd` (a small
  local `apiKeyCredentials` type). This needed **no changes inside
  `internal/frontend` itself** — attaching credentials at dial time
  means every existing call site keeps working unmodified, and an
  auth failure just arrives as an ordinary `err != nil` that every
  handler already renders as an inline error banner.
- New `/apikeys` page (`web/templates/apikeys.html`,
  `internal/frontend/convert.go`'s `apiKeyView`/`fromRPCAPIKey`):
  create a key (name only), list existing ones (id, name, created
  time — never the key or hash), and a one-time raw-key reveal on
  successful creation with an explicit "you will not see this again"
  warning — the same one-shot-reveal pattern ISO upload's success
  confirmation uses, just for a secret instead of a filename. The
  page's own intro text warns about the permanent, one-way nature of
  enabling auth before the first key is ever created.

## Consequences

- Verified with pure unit tests (`internal/manager/auth_test.go`:
  `hashAPIKey`/`extractBearerToken`/`checkAuth` against a fake
  validator, no raft needed), FSM-level tests (`internal/raft/fsm_test.go`:
  create/revoke CRUD, `ValidateHash`/`AuthEnabled` correctness —
  including that revoking the only key leaves `AuthEnabled()` true,
  not false — and snapshot/restore round-trip), real integration tests
  against a full raft harness (`internal/manager/integration_test.go`:
  zero keys is fully open, a real created key works and a wrong one is
  rejected, revoking a key stops it working immediately, list never
  returns key material), and frontend tests
  (`internal/frontend/server_test.go`: the `/apikeys` page's create/
  list/revoke handlers against the existing fake `ManagerServiceClient`).
- Verified live end-to-end on `apiarium`: deployed the new binaries and
  confirmed the existing, already-running setup kept working completely
  unauthenticated immediately after deploy (zero keys existed yet — the
  critical non-regression check); created a real key through the
  `/apikeys` UI, confirmed it's shown exactly once; confirmed every
  other RPC (`/vms`, etc.) immediately started rejecting calls
  `Unauthenticated`; set `APIARY_MANAGER_API_KEY` on `frontend` and
  restarted it, confirming the UI worked normally again with the key
  attached; revoked the key and confirmed `frontend` (still holding the
  now-revoked key) immediately failed again — which, because it was
  the *only* key, also demonstrated the permanent lockout described
  above for real, requiring a `raftd` data-directory wipe to recover
  (losing `apiarium`'s two in-flight VM-deletion records from ephemeral
  state; their underlying ZFS/bhyve resources were untouched and are
  now orphaned, an existing, already-documented limitation, not a new
  one — see "What's not implemented yet" on resource reclaim).
- `apiarium` was deliberately left with zero keys (fully open,
  matching its state before this work) rather than left with a real
  key configured, since doing so requires the same "configure the key
  everywhere or lock yourself out" coordination described above — left
  for whoever next wants API-key auth actually enabled there to do
  deliberately.
- `raftd`'s internal socket, `restshim`, real user/role separation, and
  raft's own transport remain explicitly out of scope, as listed above.
