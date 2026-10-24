# ADR-0158: Zabbix monitoring integration

## Status

Proposed. This is an Apiary-side design proposal for issue #24; it does not claim that an integration has been implemented or that its product packaging is approved.

Supersedes: None
Superseded by: None
Tracking issue: #24

## Context

Apiary has authenticated read APIs for host statistics and cluster health, and ADR-0157 defines a pull-oriented native monitoring contract. There is no Zabbix client, Zabbix configuration schema, or Zabbix API call in Apiary. The issue requests an integration but leaves operations and connection behavior for design.

A pull design avoids maintaining an outbound Zabbix credential in every Apiary installation and keeps collection independent of leader changes. It also allows Zabbix to own scheduling, retention, history, and trigger evaluation.

## Decision

Zabbix pulls observations from Apiary. Apiary does not push data to Zabbix and does not call the Zabbix JSON-RPC API. The first integration is a read-only Zabbix template and item configuration that calls the Apiary REST monitoring endpoints defined by ADR-0157. It does not provision Zabbix hosts, items, or triggers through Zabbix API calls.

The initial operation set is limited to reading cluster health and per-node monitoring summaries. The mapping uses stable item keys: `apiary.cluster.health.status`, `apiary.node[{#NODEID}].health.status`, `apiary.node[{#NODEID}].cpu.load1`, `apiary.node[{#NODEID}].memory.available_bytes`, `apiary.node[{#NODEID}].pool.worst_capacity_percent`, and `apiary.node[{#NODEID}].disk.worst_health`. The health status item carries the existing healthy, degraded, unknown, stale, or contradictory value. Missing or unreachable data is reported as unavailable, never coerced to a healthy value or zero.

The template supplies discovery from node IDs in the cluster report. Initial triggers are limited to a node status changing to degraded, stale, or contradictory, and an observation becoming unavailable beyond the Zabbix item's configured timeout. Numeric thresholds for load, memory, pool capacity, and disk health are operator-configured in Zabbix. Apiary does not choose site-specific thresholds.

The endpoint is the Apiary REST shim's HTTPS base URL and the ADR-0157 monitoring paths. Zabbix's HTTP agent uses a configured polling interval, initially 60 seconds, adjustable in the template. TLS certificate validation is required. There is no plaintext or insecure-skip-verify mode. The API credential is a dedicated Viewer-scoped Apiary API key held in a Zabbix secret macro or the deployment's equivalent secret store, never in an item key, URL, or log. Rotation means create a replacement Viewer key, update the Zabbix secret, verify reads, then revoke the old key. Apiary stores no Zabbix credential.

The template uses a 10-second HTTP request timeout and leaves retries to Zabbix scheduling; it does not retry inside an item request. Apiary applies bounded backend RPC timeouts, returns a non-success response for unavailable backend data, and does not queue collection work. Zabbix reports connection and authorization failures through its ordinary unsupported-item and trigger mechanisms. Response bodies and logs must not contain credentials.

All initial operations are reads and belong to the read-only core API. The product packaging or price boundary for a Zabbix template and basic external read access is unresolved. No write operation, remediation action, or Zabbix-driven Apiary mutation is permitted by this design. Any future write feature must be a separate decision and follow the paid read-write product policy and Apiary's authorization and audit controls.

The division with orcli#65 is explicit: Apiary owns the ManagerService and REST data contract, Zabbix template, item keys, data mapping, and read authorization. The orcli repository owns its Zabbix connection behavior and user-facing client operations. Both repositories must use the same Apiary read contract, but must not duplicate Apiary-side sampling, health interpretation, or item mapping. Shared operation names and semantics should be documented at the API boundary.

## Consequences

- Zabbix is the scheduler and history store; Apiary remains an authenticated observation source.
- A Viewer API key is stored and rotated on the Zabbix side. Apiary needs no Zabbix endpoint or secret configuration.
- The integration depends on the REST monitoring paths specified by ADR-0157 and cannot ship before that surface exists.
- The owner must confirm whether the Zabbix template and unaffiliated read-only external access are core or paid. Initial thresholds remain local operator choices.
- ADR numbers 0157 and 0158 are verified free in the current origin/main tree, but central ADR number allocation still requires owner confirmation.
