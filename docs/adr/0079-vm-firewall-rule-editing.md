# ADR-0079: Editing an existing VM's firewall rules

## Status

Accepted

## Context

Firewall rules (ADR-0075's `priority` field included) were only ever
settable at `CreateVM` time - correcting a mistake, or adjusting rules
as a service's needs change, required deleting and recreating the VM.
Checked before designing anything: `internal/manager/convert.go`'s
`toInternalVM` already carries `FirewallRules` through the general
`UpdateVM` path - it is not one of the fields excluded there
(`IpAddress`/`MacAddress`/`Phase`/`PhaseError`/`FirewallPaused`/
`CloudflareHostname`/`CloudflarePort`), so at the protocol level
nothing was stopping this. The actual gap was a missing frontend form.

Two implementation shapes were considered:

1. **Fetch the current VM, then call the general `UpdateVM` with
   `FirewallRules` overridden**, since that field already round-trips
   safely through it.
2. **A dedicated `SetVMFirewallRules` command**, mirroring
   `SetVMFirewallPaused`/`SetVMCloudflareExposure`/`SetVMDesiredState`.

Checked which the frontend actually uses today before picking:
`internal/frontend` never calls `UpdateVM` at all - every single
VM mutation it performs (pause/resume firewall enforcement, Cloudflare
exposure, lifecycle state changes) goes through a dedicated, narrow
command, even where the field in question (`DesiredState`, for
example) is *also* safe to carry through `UpdateVM`. This is a
deliberate, total, and current pattern, not just the two `ADR-0049`/
`ADR-0063` hazard cases. **Decision: option 2**, for consistency with
every other frontend-initiated VM mutation, and because a dedicated
command applies atomically inside one FSM `Apply` call with no
read-modify-write race against a concurrent `UpdateVM` changing some
other field in between - the same reasoning `SetVMFirewallPaused`'s own
doc comment already gives for itself.

## Decision

`SetVMFirewallRules` - new internal command
(`api/internalpb/state.proto`), external RPC
(`api/rpc/manager.proto`), FSM apply function
(`internal/raft/fsm.go`), and manager RPC handler
(`internal/manager/server.go`) - mirrors `SetVMFirewallPaused` exactly:
clone the VM, replace only `FirewallRules`, apply atomically, forward
to the leader on a follower's leader-hint like every other write RPC.
No field-level rule validation was added at the FSM layer, matching
`applyCreateVM`'s own existing posture (rule contents aren't validated
there either, so editing isn't held to a stricter standard than
creation).

`toInternalFirewallRules` was factored out of `toInternalVM` into its
own function so both `CreateVM`/`UpdateVM`'s conversion and the new
`SetVMFirewallRules` handler convert the wire `FirewallRule` shape
identically, rather than duplicating the loop.

### Frontend: a dedicated form, reusing the create-VM row editor

`web/templates/vm.html`'s Firewall panel gets a real edit form
(`POST /vms/{id}/firewall-rules`, Operator-gated) alongside the
existing read-only summary (kept for Viewer-tier users, who never see
the edit form at all). The form reuses `parseFirewallRuleRows` -
the exact same repeating-row parser `handleCreateVM` already uses -
since both forms render identically-named fields
(`fw_direction`/`fw_action`/`fw_protocol`/`fw_port`/`fw_priority`).
Existing rules are pre-rendered server-side with the correct option
selected/value filled; the client-side "Add rule" button appends
further blank rows via the same small inline script `new_vm.html`
already uses (duplicated rather than abstracted into a shared
template - this codebase prefers three similar lines over a premature
shared abstraction for something this small). Submitting with every
row removed clears all rules, matching `CreateVM`'s own "no rules
means allow by default" semantics.

`renderVMPage`'s single `formErr` parameter was split into
`cloudflareErr`/`firewallErr` (and `pageData` gained a matching
`VMFirewallFormError` alongside the existing `VMCloudflareFormError`) -
previously every VM-detail-page form's error, including a lifecycle
action failure, rendered under the unrelated "Public exposure" panel.
Splitting this was necessary for the new form's own errors to appear
in the right place; the pre-existing lifecycle-error placement was left
exactly as it was (not part of this ADR's scope) rather than fixed
opportunistically.

## Consequences

- An operator can now correct or evolve a VM's firewall rules without
  deleting and recreating it.
- The change adds one new command/RPC pair rather than reusing
  `UpdateVM`, keeping every frontend-initiated VM mutation on the same
  narrow-command footing - no exception was introduced for this one
  field.
- `VMFirewallFormError` is net-new state; existing callers of
  `renderVMPage` were all updated to pass `""` for it, so no rendering
  behavior changed for any existing form.

## Verification

Unit tests: `internal/raft/fsm_test.go` - `SetVMFirewallRules` replaces
rules while leaving every other field untouched (mirroring
`TestFSM_Apply_SetVMFirewallPaused_TouchesOnlyThatField`'s own
structure), an empty rule list clears all rules, and a missing VM id
is rejected. `internal/manager/integration_test.go` - a real round trip
through actual raftd (UDS, real election, real Apply) replacing rules
and confirming every other field survives, plus the missing-id case.
`internal/frontend/vm_firewall_rules_test.go` - the edit form forwards
parsed rows correctly, an empty submission clears rules, an RPC error
renders under the firewall panel specifically (not the Cloudflare
panel), existing rules pre-populate the edit form's selects/inputs
correctly, and a Viewer-role request never receives the edit form at
all (read-only table only). `go build ./...`, `go vet ./...`,
`gofmt -l`, `git diff --check`, and the complete `go test ./...` suite
all pass.
