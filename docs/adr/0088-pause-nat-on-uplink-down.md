# ADR-0088: Pause self-hosted outbound NAT on uplink down

## Status

Accepted

## Context

ADR-0085's uplink admin toggle explicitly disclosed, but didn't fix, a
real gap: bringing the uplink NIC down leaves every self-hosted
network's `nat-to (<uplink>)` pf(8) rule silently in place -
`internal/cluster`'s own `ensureNetwork` unconditionally re-`ApplyNAT`s
it every reconcile tick regardless of interface state
(`internal/pf.ApplyNAT`'s own doc comment: "safe to call
unconditionally every reconcile tick"). Egress for anything relying on
that NAT just stops working, with no signal anywhere that it's because
of the toggle rather than some other failure. This closes that gap:
bringing the uplink down now also immediately neutralizes the
outbound-NAT rule it backs, instead of leaving a rule silently pointing
at a dead interface.

## Design decisions

- **No new "resume" code path.** `ensureNetwork` already re-applies
  `ApplyNAT` for every self-hosted network on every `RunOnce` tick,
  unconditionally, as long as `PF`/`Uplink` are configured and the
  network still has no `ExternalGateway`. Once the uplink comes back
  up, the very next reconcile tick already restores the identical NAT
  rule for free. Only the "down" side needed new code - the same
  eventual-consistency posture every other resource in that file
  already has.
- **The uplink toggle and NAT egress can name different interfaces**
  (ADR-0048's own prior disclosure: `-vlan-uplink` and `-nat-uplink`
  aren't required to match). `SetUplinkState` now compares
  `vlan.Manager.UplinkInterface()` against the reconciler's own
  `NATUplink()` before pausing anything - downing an unrelated
  VLAN-trunk interface must not kill a working, unrelated NAT egress
  path.
- **`Reconciler.PauseOutboundNAT`** flushes the pf(8) anchor for every
  network this node's persisted `NetworkStatePath` records as having
  `OutboundNAT: true` - reusing the exact same artifact-state file
  `NetworkArtifactStatus`/`reconcileNetworkArtifacts` already read/
  write, rather than introducing a second source of truth for "which
  networks have NAT."
- **A NAT-pause failure never fails the `SetUplinkState` RPC.** The
  primary action (bringing the interface down) has already succeeded
  by that point; a pf(8) failure pausing NAT is logged
  (`fmt.Fprintf(os.Stderr, ...)`, matching `RestartNodeService`'s own
  best-effort-background-error style) rather than reported as an
  overall failure that might tempt an operator to retry an action that
  already happened.
- **`SetUplinkStateResponse` gains `nat_paused_networks`**, and the
  Machine Configuration page's success message names them explicitly
  - "uplink brought down (also paused outbound NAT for 2 network(s):
  ...)" - so this is a visible consequence of the toggle, not a silent
  side effect the operator has to infer.

## Consequences

- Full test coverage: `Reconciler.PauseOutboundNAT` flushes exactly
  the networks with `OutboundNAT: true` and ignores others, no-ops
  cleanly with no `PF`/`NetworkStatePath` configured, surfaces a
  `Flush` error; `SetUplinkState` triggers the pause only when the
  toggled interface matches `NATUplink()`, skips it when they differ
  (the ADR-0048 case) or when no `natPauser` is configured, and a pause
  failure never fails the overall RPC; the frontend success message
  includes the paused network IDs when present.
- **Disclosed, not fixed**: restoration on "up" isn't instant - it
  waits for the next reconcile tick, bounded by `-reconcile-interval`,
  consistent with every other resource this reconciler manages. An
  operator bringing the uplink back up should expect a brief window
  (at most one tick) before NAT egress actually resumes.
