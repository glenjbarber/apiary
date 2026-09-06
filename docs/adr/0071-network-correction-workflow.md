# ADR-0071: Safe correction of managed network definitions

## Status

Accepted

## Context

A network definition is replicated intent, while bridges, VLAN interfaces,
DHCP scopes, and PF NAT anchors are node-local realizations of that intent.
An operator can discover after creation that a subnet, VLAN ID, or gateway was
wrong. Replacing such a definition by silently mutating it would make the old
local realization ambiguous: the reconciler could retain the old VLAN member
or NAT anchor while creating the new one.

Deleting a network is already rejected while a VM references it. That protects
an active Cell, but an error must also be recoverable when no Cell uses the
network.

## Decision

The first correction workflow is **replace, never in-place edit**:

1. The Networks page identifies definitions as immutable after creation.
2. An Operator who needs to correct a definition first removes or reassigns
   every VM that references it. Apiary continues to reject deletion while any
   reference remains.
3. The Operator deletes the unused definition and waits for each Hive to
   report teardown of Apiary-owned artifacts for that network.
4. Only after teardown converges may the Operator create the replacement,
   normally using a new network ID. Reusing the old ID is permitted only after
   teardown evidence shows no owned artifact remains for the former definition.

Managed-network cleanup owns step 3. It must remove only artifacts that Apiary
recorded as self-created, retain a VLAN still needed by another surviving
network, and flush only the deleted network's NAT anchor. It must never infer
ownership from an interface name alone.

The future UI will expose the correction as a guided replacement action, not a
generic `UpdateNetwork` RPC. Before enabling the final Create action it will
show, per Hive, the old bridge/VLAN/NAT status and whether teardown is complete
or unknown. Unknown is a blocking state, not evidence that cleanup succeeded.

## Consequences

- A mistaken unused network can be corrected without shell cleanup, once the
  cleanup reconciler has converged.
- A network carrying VMs cannot be accidentally rewritten beneath those VMs.
- The API does not gain a deceptively simple mutable update operation whose
  physical consequences are not atomic.
- A future guided UI requires a read-only, per-Hive artifact-status RPC. That
  is deliberately separate from mutation and must not expose arbitrary host
  interface inventory.
- Existing unmanaged or pre-existing interfaces remain untouched. An operator
  must handle them explicitly outside this workflow.
