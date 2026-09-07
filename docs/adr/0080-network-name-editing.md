# ADR-0080: Editing a managed network's Name

## Status

Accepted

## Context

ADR-0071 established "replace, never in-place edit" for managed
network definitions: subnet, VLAN ID, bridge, and external gateway are
all node-local physical realizations of replicated intent, so mutating
any of them in place would leave the reconciler unable to tell an old
realization from a new one apart. That ADR's own Decision section names
the future correction path as "a guided replacement action, not a
generic `UpdateNetwork` RPC."

`Name`, however, has no physical realization at all - it is never
rendered into `dnsmasq.conf`, never used to derive a bridge name,
never compared against anything the reconciler tracks. ADR-0071's own
hazard (the reconciler retaining an old VLAN member or NAT anchor while
creating a new one) simply does not apply to a field nothing downstream
of raft ever reads for its physical meaning.

## Decision

A new, narrow `SetNetworkName` command/RPC (mirroring the VM-side
`SetVMFirewallPaused`/`SetVMFirewallRules` pattern exactly: clone the
network, touch only `Name`, apply atomically) lets an operator rename a
network in place. This is **not** a partial relaxation of ADR-0071 -
every other field remains exactly as immutable as that ADR decided,
and still requires the delete-and-recreate workflow ADR-0080's sibling,
[ADR-0081](0081-guided-network-replacement-workflow.md), builds a
guided version of. `Name` is carved out because it was never actually
covered by ADR-0071's own reasoning in the first place, not because
this ADR is loosening it.

The Networks page's Name column becomes a small inline form (an input
plus a Save button, matching this project's own established "inline
editable" pattern - see the Machine Configuration settings-table
precedent) rather than a separate edit page, since renaming is a
single-field, single-action operation with nothing else to configure
alongside it.

## Consequences

- An operator can fix a typo'd or outdated display name without
  deleting and recreating a network, closing the smallest and most
  common real annoyance the "immutable after creation" policy caused
  in practice.
- ADR-0071's actual protection (no silent mutation of anything with a
  physical realization) is completely unchanged - `SetNetworkName`
  cannot touch subnet, VLAN, bridge, or gateway, by construction (the
  command's own message has no field for them).
- No generic `UpdateNetwork` RPC was added - the API surface still
  doesn't gain "a deceptively simple mutable update operation whose
  physical consequences are not atomic," exactly as ADR-0071 wanted to
  avoid.

## Verification

Unit tests: `internal/raft/fsm_test.go` - renaming leaves subnet/VLAN
untouched, and a missing network id is rejected.
`internal/manager/integration_test.go` - a real round trip through
actual raftd confirming the rename persists and every other field
survives. `internal/frontend/server_test.go` - the Networks page
renders an editable Name form pre-filled with the current name, the
form forwards the submitted name correctly, and an RPC error renders
in the network panel. `go build ./...`, `go vet ./...`, `gofmt -l`,
`git diff --check`, and the complete `go test ./...` suite all pass.
