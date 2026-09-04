# ADR-0058: Cell Path Trace v1

## Status

Accepted

## Context

Apiary records enough network intent and node-local evidence to explain several
common reasons a VM Cell cannot reach a destination, but operators currently
have to correlate the VM definition, managed network, bridge state, PF state,
firewall rules, and NAT-uplink assumption manually. Priority 7 in `CODEX.md`
calls for Cell Path Trace to identify the first layer where observed state
diverges from intent instead of dumping every layer independently.

Apiary does not yet observe guest routes, guest DNS configuration, live DHCP
leases through RPC, or destination responses. V1 must not turn these missing
observations into a false green result, and it must not inject packets merely
because an operator asked an explanatory question.

## Decision

Add Viewer-readable `ManagerService.TraceCellPath` and a session-protected
`GET /trace` page. V1 accepts a VM Cell ID, destination host or IPv4 address,
and optional protocol and destination port. Jails are excluded because their
current `ip4=inherit` model has no Cell-specific network path in replicated
state.

The RPC reads VM and network intent from the raft leader. Any leader hint
forwards the complete original request so one trace does not combine different
FSM views. It then queries the VM owner Hive directly for node-local evidence:

- `GetLocalNetworkBridgeStatus` for the selected managed network;
- `HostStats` for PF enabled state; and
- `ListAssumptionResults` for the effective, freshness-aware
  `NAT_UPLINK_DEFAULT_ROUTE` result.

The owner query is local when the answering managerd owns the VM and a bounded
peer call otherwise. The three remote evidence reads run concurrently under
separate three-second bounds, so an unreachable owner cannot multiply that
delay across every layer. A missing owner, absent peer route, timeout, RPC
error, or stale assumption becomes `UNKNOWN`, never a negative or positive
guess. If a changed uplink leaves older snapshot keys behind, the newest
owner-Hive NAT observation is used; equally recent conflicting observations
are `UNKNOWN`.

Pure computation belongs in a new `internal/pathtrace` package. It emits an
ordered sequence with four statuses:

- `CLEAR`: the available evidence supports this layer;
- `BLOCKED`: the available evidence identifies a concrete blocker;
- `UNKNOWN`: Apiary lacks sufficient or current evidence;
- `NOT_APPLICABLE`: the layer is irrelevant to this request or intentionally
  not tested.

The stages are Cell desired/reconciled state, virtual interface assignment,
tap attachment, managed-network/VLAN intent, DHCP lease/options, DNS,
owner-Hive bridge, declared outbound firewall policy, route/NAT, and
destination response. Overall status uses the first `BLOCKED` step, otherwise
the first `UNKNOWN`, otherwise `CLEAR`. Tap attachment and guest DHCP state are
explicitly `UNKNOWN` in v1, not omitted or treated as passing. DNS is
`NOT_APPLICABLE` for a literal IPv4 destination and `UNKNOWN` for a hostname.
The destination-response step is always `NOT_APPLICABLE` because the feature
performs no active probe.

Firewall evaluation is intentionally limited to Apiary's actual simple rule
model: ordered `in`/`out` pass/block rules whose last matching rule wins, with
no source/destination selector. If protocol or port information is required to
decide which outbound rule matches but was not supplied, the firewall step is
`UNKNOWN`. Declared rules are only evaluated as enforced when PF is observed
enabled on the owner Hive; unavailable or disabled enforcement is an evidence
gap, not a claimed packet blocker. A paused firewall or no matching outbound
rule preserves Apiary's established allow-all behavior.

For an IPv4 destination inside the managed subnet, routing is on-link and NAT
does not apply. Outside the subnet, an explicit external gateway satisfies the
declared route layer. Otherwise the path requires PF enabled on the owner and a
fresh true NAT-uplink/default-route assumption. A false or not-applicable NAT
assumption is a blocker; unknown or unavailable evidence remains unknown.

## Consequences

- Operators receive one ordered explanation with the first blocker or evidence
  gap identified, while retaining every supporting step for inspection.
- A `CLEAR` result means no blocker was found inside v1's observed scope. It is
  not proof that DNS resolved, the guest accepted its DHCP configuration, or
  the destination responded.
- Because v1 cannot observe current tap attachment or guest DHCP acceptance, a
  complete v1 trace without a known blocker remains `UNKNOWN`. Later evidence
  sources can close those gaps without changing the response shape.
- Hostname destinations remain `UNKNOWN` at DNS because Apiary does not expose
  the DHCP DNS option or guest resolver result through RPC. IPv6 is also
  outside v1 and returns a clear input error rather than being misclassified.
- A Cell configured to be stopped or deleted is a blocker even if its last
  reconciler phase was ready. An unspecified desired state remains unknown.
- Results are sequential and non-atomic. Network or node state may change while
  the trace is assembled.
- Active probes, guest agents, live lease inspection, DNS resolution evidence,
  cross-Hive packet capture, and repair actions remain later slices. Any repair
  belongs in a separate reviewable Flight Plan.
