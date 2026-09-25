# ADR-0119: Replica freshness evidence and maintenance wave planner

Date: 2026-09-24

Status: Accepted

## Context

Apiary records configured HAST owner/replica placement in Colony state, but
that configuration alone does not prove that either local `hastd` is
available, that both ends agree on their roles, or that replication is
currently complete. The existing node-failure simulator can evaluate each
Comb's individual impact on Raft quorum and identify workloads owned there.
Operators need that evidence in a maintenance workflow without turning the
planner into an unreviewed restart or failover controller.

## Decision

Add a read-only, node-local `GetLocalHASTResourceStatus` RPC. It accepts only
Apiary's fixed `vm-<id>` and `jail-<id>` resource names, queries the local
`hastctl`, and reports the observed role, status, replication mode, dirty
extent text, extent size, and observation time. It never forwards through
Raft. A consumer that needs a pair must independently query owner and replica;
an unavailable endpoint or missing timestamp remains unknown.

VM and jail detail pages show these two local observations. `complete-observed`
means only that both endpoints returned fresh observations with expected
primary/secondary roles, `status: complete`, and matching replication modes.
Other explicit HAST status or role disagreement is degraded; missing or
inconsistent evidence is unknown. Dirty extents are shown as raw `hastctl`
output. Apiary does not translate them into seconds, bytes-at-risk, or a
numeric RPO, nor claim failover readiness from HAST completion.

The Maintenance Wave Planner reuses the existing node-failure simulator for
each candidate Comb, orders confirmed quorum-preserving cases ahead of
unknown and blocked cases, and shows owned workload impact. A wave contains
exactly one Comb. The planner is read-only: it does not restart services,
migrate workloads, or change Raft membership. Confirmed quorum preservation
is not a claim that workloads have replicas or that configured replicas are
fresh. Operators must review each workload's freshness evidence and wait for
fresh healthy evidence after one maintenance event before considering another.

## Consequences

- Physical HAST observations stay local to each Comb and are queried directly.
- Missing observations do not become healthy evidence.
- The planner can inform sequencing but does not coordinate or execute it.
- Numeric RPO and automated maintenance remain future work requiring stronger
  measurements and explicit action guardrails.
