# ADR-0153: Native monitoring API for Apiary

## Status

Proposed. The prior ADR-0150 proposal is superseded for the monitoring API decisions recorded here; its historical analysis and unaccepted details remain preserved below.

Supersedes: ADR-0150 monitoring proposal sections on API shape and unresolved monitoring choices
Superseded by: None
Tracking issue: #25

## Context

Apiary already exposes per-node HostStats and cluster-wide ClusterHealth through ManagerService, along with GetLocalNodeHealth. The reports are reads assembled on demand. HostStats describes local physical observations, while ClusterHealth derives a health verdict from independently collected evidence. ClusterHealth is not an atomic snapshot. These existing contracts are preferable foundations to a new monitoring daemon.

The older ADR-0150 proposed a replicated periodic node-vitals report but explicitly left replication, API shape, interval, history, and summary fields open. High-churn physical counters are not replicated through Raft today. A new monitoring feature should not turn measurements into consensus traffic or present observations from different moments as one atomic fact.

The REST shim currently exposes GET /v1/health, backed by ClusterHealth. It is a separate gRPC-to-REST surface. There is no Prometheus exporter or general metrics endpoint. ManagerService read authorization uses the Viewer role; unknown RPCs default to Admin. The product policy distinguishes a read-only core from paid read-write capability, but the product boundary for basic external read-only access has not been decided.

## Decision

Apiary monitoring consists of both native internal monitoring APIs and an external scrape format, delivered in that order. The native ManagerService API is the canonical typed contract. A later REST scrape representation may expose the same read-only observations to external monitoring systems. Neither surface performs monitoring writes or remote control.

Monitoring is pull-based. A caller requests current observations when needed. Apiary does not periodically push reports to a leader or external service, and monitoring observations are not raft-replicated. Each node's physical observations remain local. A cluster report may fan out to peers as ClusterHealth does today, with per-node timestamps and explicit unavailable, stale, or unknown states. It must not imply that rows were sampled atomically.

Add sibling monitoring report RPCs to ManagerService rather than expanding ClusterHealth. ClusterHealth keeps its focused evidence verdict contract; monitoring reports contain measurements and timestamps. This avoids changing the meaning and payload of an established health API. Exact RPC names and proto wire shape are implementation details, but the report must provide per-node and cluster-wide reads.

The stable summary includes node ID, observation timestamp, health status using the existing five-state vocabulary, CPU load averages, memory totals and available memory, per-pool capacity and health, disk health, network byte counters and link state, and relevant raft role and applied-index observations. Fields are optional when the source is unavailable, with explicit availability or freshness metadata. Do not convert missing observations to zero or healthy. New measurements are added compatibly as optional typed fields or a versioned extension map with documented names and units; consumers must ignore unknown extensions.

There is no Apiary reporting interval because sampling is caller-driven. Consumers choose their scrape interval. Each response carries observation timestamps and source freshness. Apiary retains no monitoring history or time series. Historical analysis belongs in the external monitoring system. The REST scrape format uses `GET /v1/monitoring/health` for the cluster verdict and `GET /v1/monitoring/nodes` for per-node summaries, reusing these semantics and timestamps. The REST shim applies the same Viewer authorization as the backing RPCs.

Read methods remain Viewer-authorized, consistent with HostStats and ClusterHealth. The existing per-RPC fail-closed authorization map remains authoritative. The core exposes read-only observations. Any future monitoring operation that changes Apiary state is a write and must follow the paid read-write product policy, an explicit role gate, and audit requirements. This ADR does not decide how external-client read-only access is packaged or licensed.

## Consequences

- Native reads avoid adding high-frequency measurement traffic to the Raft log and work with the existing API authentication model.
- Monitoring data is a set of timestamped observations, not a durable history or an atomic cluster snapshot.
- An external scrape representation is a follow-up implementation of the same native contract; no /metrics endpoint is claimed to exist today.
- The product boundary for basic read-only external access remains open for the owner. Implementation must not assume that MCP, REST, or a connector is core or paid until that decision is made.
- Before implementation, the owner should confirm whether the REST scrape format is required in the first delivery and whether any summary fields beyond the listed existing observations are needed.
