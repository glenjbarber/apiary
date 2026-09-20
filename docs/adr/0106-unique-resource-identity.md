# ADR-0106: Unique resource identity for VMs, jails, and Colony voters

## Status

Accepted

## Context

VMs and jails are already visible cluster-wide from every Comb, not
scoped to their owning node - `ListVMs`/`ListJails` read the fully
Raft-replicated `FSM.vms`/`FSM.jails` maps unconditionally, and the
`/vms`/`/jails` pages already mark which Comb owns each row. VM and
jail *ids* are already unique cluster-wide as a natural consequence of
using a single shared map, keyed by id, for both. The one place
ambiguous identity could still occur is `JailDefinition.hostname` -
the value passed to `jail(8)` as the jail's own hostname, entirely
independent from `id`/`name` - which had no uniqueness or format check
at all. Two jails sharing a hostname is exactly the kind of ambiguous
identity that matters once every jail is visible from every Comb,
not just its owner.

Separately, while designing this, a second, related identity gap
surfaced: `RequestJoinColony`/`ApproveJoinRequest` never checked
whether a submitted `node_id` already named an existing Raft voter.
`hashicorp/raft`'s own `AddVoter` silently updates that voter's address
in place when called with a `ServerID` already in the configuration,
rather than erroring - useful, and already used once in this project's
own history (see SHARED.md's 2026-09-17 00:17 EDT entry: an ad hoc
one-off Go tool resubmitted a join request against `brood`'s own
already-voting `node_id` to correct a stale recorded address after the
2026-09-17 brood/drone incident), but it means the RPC layer could
never distinguish "a new Comb accidentally colliding with an existing
node_id" from "the same Comb reporting a corrected address" - both look
identical at that layer, and only the latter should ever be allowed.

## Decision

**Jail hostname uniqueness.** `applyCreateJail` and `applySetJailHostname`
(`internal/raft/fsm.go`) now validate `hostname` with the existing
`validHostname` format check (previously applied only to
`SetVMCloudflareExposure`'s unrelated Cloudflare hostname) and reject a
value already used by any other jail, via a new `FSM.jailHostnameConflict`
helper. Comparison is case-insensitive (jail(8) hostnames are DNS names);
an empty hostname never conflicts with another empty hostname, since
empty means "not set," matching the field's existing optional status.
VMs have no equivalent general-purpose hostname field (`cloudflare_hostname`
is a distinct, unrelated public-exposure concept scoped by ADR-0081/
ADR-0093) and are unaffected by this change. Every Raft replica applying
the same log entry reaches the identical accept/reject decision, since
the check reads only already-committed FSM state.

**Colony voter identity.** `RequestJoinColony` now rejects a `node_id`
that already names an existing Raft voter or non-voter outright, via a
new `Server.existingVoter` helper reading `s.raft.Status`'s own
`Servers` list - no `PendingJoinRequest` is created. `ApproveJoinRequest`
re-checks the same condition immediately before calling `AddVoter`,
closing the race window where a second pending request for the same
`node_id` (created before either was a voter, which is still permitted -
see below) is approved after the first. Neither check is pushed into
the FSM itself: Raft's own membership configuration is not FSM-managed
state, so this is deliberately manager-layer logic reading the same
`raft.GetConfiguration()`-backed view every replica's own local `Status`
call already exposes deterministically.

A brand-new `UpdateVoterAddress` RPC (Admin-only, same tier as
`ApproveJoinRequest`) is the only supported way to re-point an existing
voter's address going forward - the first-class replacement for the ad
hoc `RequestJoinColony`/`ApproveJoinRequest`-against-an-existing-node_id
technique used during the brood/drone incident. It requires `node_id`
to already be a known voter (the mirror image of `ApproveJoinRequest`'s
new check - this RPC can never add a new member), applies the same
leadership-first ordering and pre-change reachability guardrail
(ADR-0097) against the *new* address that `ApproveJoinRequest` already
applies against a joiner's address, and calls the same `AddVoter`.

Deliberately **not** applied here: `validateJoinerRaftBind`'s
loopback/format check (ADR-0105). That check exists for
`ConvertStandaloneToJoiner`, where a Comb validates its *own* bind
address and loopback is always a self-inflicted misconfiguration.
`UpdateVoterAddress` plays `ApproveJoinRequest`'s role instead -
accepting *another* Comb's claimed address - and `ApproveJoinRequest`
has never format-checked that address beyond the reachability
guardrail. Adding a stricter check to only one of the two RPCs that
otherwise behave identically would be an inconsistency without a
safety benefit, and would also break this project's own integration
test harness, which legitimately uses loopback addresses throughout.

**What this does not do.** It does not distinguish, automatically, an
accidental `node_id` collision from a legitimate address correction -
that judgment now requires an Admin to explicitly choose
`UpdateVoterAddress` over `RequestJoinColony`, rather than the same
call silently doing either depending on prior state. It does not
enforce any uniqueness concept for a Comb's *hostname* as distinct from
its `node_id` - no such field exists in this codebase's node identity
model today (`node_id` defaults to `os.Hostname()` but is otherwise a
bare identifier), so this stays a `node_id`-only guarantee until a
future feature introduces a real, separate Comb-hostname field. It does
not address VM identity beyond `id` (VMs have no analogous hostname
field to enforce uniqueness on).

## Consequences

- A future re-run of the ad hoc "resubmit a join request to fix a
  stale voter address" technique will now be refused with a clear
  error directing the operator to `UpdateVoterAddress` instead -
  `docs/add-node-to-colony.md` is updated accordingly.
- Two jails can no longer be created, or edited, into sharing a
  hostname, regardless of which Comb owns each - closing the one real
  ambiguous-identity gap the colony-wide visibility work surfaced.
- `UpdateVoterAddress` is a new, real membership-changing capability
  and is Admin-gated identically to `ApproveJoinRequest`; it has no UI
  surface yet (RPC/manager-layer only in this change) - a Colony
  overview action for it is a reasonable follow-up, not required for
  the guardrail itself to be safe to use via direct RPC.

## Verification

- `internal/raft`: `TestFSM_Apply_CreateJailDuplicateHostnameRejected`,
  `TestFSM_Apply_CreateJailDuplicateHostnameCaseInsensitive`,
  `TestFSM_Apply_CreateJailEmptyHostnameNeverConflicts`,
  `TestFSM_Apply_CreateJailInvalidHostnameRejected`,
  `TestFSM_Apply_SetJailHostname_DuplicateRejected`,
  `TestFSM_Apply_SetJailHostname_SameJailReassertingOwnHostname`.
- `internal/manager`: `TestIntegration_RequestJoinColony_DuplicateNodeIDRejected`,
  `TestIntegration_ApproveJoinRequest_DuplicateNodeIDRejected` (the race-window
  case specifically, using two independently pending requests),
  `TestIntegration_UpdateVoterAddress_UpdatesRealRaftVoterAddress`,
  `TestIntegration_UpdateVoterAddress_UnknownNodeIDRejected`,
  `TestIntegration_UpdateVoterAddress_UnreachableAddressRejected`,
  `TestIntegration_UpdateVoterAddress_MissingFieldsRejected` - all using
  real `raftnode.New` instances, matching this file's existing
  `ApproveJoinRequest` integration-test convention rather than a fake.
- `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), full
  `go test ./...` (every existing test, including the full
  `joincolony_test.go`/`integration_test.go` suites, passes unchanged).
- Explicitly not in scope: no live host was touched; `brood`/`drone`
  were not involved.
