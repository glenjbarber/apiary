# ADR-0125: Quorum-safe raftd restart workflow

## Status

Proposed

## Context

### Current state (ADR-0116 recap)

ADR-0116 documented a complete design for quorum-safe `apiary_raftd` restarts but explicitly did not implement it. The decision recorded three gaps that made a first implementation riskier than the status quo:

1. **No confirm-on-startup hook for raftd** — `cmd/managerd`'s `confirmPendingRestartOnStartup` clears a managerd restart lease when the new managerd process starts. `cmd/raftd` has no equivalent; getting this wrong would leave a permanently-stuck, un-removable raft-replicated lease on the consensus-critical service.

2. **No live reachability probe for other voters** — The quorum check requires knowing which *other* voters are currently reachable. `internal/raft.Node.Status()` only reports configured membership, not liveness. ADR-0097 established the pattern (a live TCP dial from the leader to each peer's `raft_bind_address`), but it was not wired for restarts.

3. **Leadership transfer never used** — hashicorp/raft's `LeadershipTransfer()` exists but has never been called in this codebase. The ADR-0116 design deferred it (acknowledgment-only), but the gap remains: restarting the current leader triggers an election, which is a real availability impact.

Since ADR-0116, the codebase has accumulated more evidence that the manual-only status quo is itself a risk (operators restarting raftd by hand with no coordination, no visibility into peer health, and no serialization against another operator acting simultaneously). This ADR revisits the design, closes the two implementation gaps with concrete, testable wiring, and keeps the leadership-transfer deferral as a documented future enhancement.

### What changed since ADR-0116

- `cmd/raftd` is now a stable, well-understood entry point (ADR-0124 added `raftd -status`; the process boundary is confirmed).
- The `internal/guardrail` package pattern (pure, no-I/O evaluators over caller-supplied facts) is proven by `EvaluateConcurrentManagerRestart` and `EvaluateJoinReachability`.
- `SimulateNodeFailure` (`internal/cluster/simulate.go`) provides the exact quorum arithmetic we need: `ComputeQuorumImpact` counts `RemainingReachable` voters against `QuorumSize = TotalVoters/2 + 1`, treats `Unknown` as not-surviving, and returns a per-voter breakdown.
- ADR-0103's restart-lease FSM state (`RestartLease`/`RestartRecord`, `AcquireRestartLease`/`RecordRestartCompleted`) is fully generic over `service` string — it already supports a second service key with zero schema changes.
- The restart-guardrail token (`/usr/local/etc/apiary/restart-guardrail-token`) already gates the cross-node lease RPCs for managerd; the same token works for raftd.

## Decision

### 1. New service key: `"apiary_raftd"` (not a new string)

Reuse the existing service name `apiary_raftd` — it is already the rc.d service name, the `apiaryServices` entry in `internal/manager/services.go`, and the natural key operators expect. No new constant.

### 2. Confirm-on-startup hook in `cmd/raftd`

Add a direct equivalent of `cmd/managerd`'s `confirmPendingRestartOnStartup` to `cmd/raftd/main.go`:

- A new `RestartConfirmStore` (same on-disk JSON format, same file location `/var/db/apiary/pending-restart.json` but keyed by service) is loaded at raftd startup.
- If a pending record for `service == "apiary_raftd"` exists, the new raftd process dials its **local managerd** (via the existing Unix socket `rcfg.Socket`) and calls `ConfirmRestartCompletedLocal(ctx, "apiary_raftd", nodeID, leaseID)`.
- Retry logic identical to managerd: 5 attempts, 3s backoff, bounded timeout, giving up loudly (log + leave the lease stuck) rather than retrying forever.
- The local managerd forwards the confirmation to the leader (same path as `ConfirmRestartCompleted` RPC), which applies `RecordRestartCompleted` and releases the lease on exact `lease_id` + `holder_node_id` match.

**Why dial local managerd, not leader directly?** The raftd process has no gRPC client to the leader; it only speaks the internal `RaftInternal` protocol over its Unix socket to managerd. Managerd already has the peer forwarding machinery, the restart-guardrail token, and the `ConfirmRestartCompletedLocal` helper. Reusing that path avoids duplicating credential handling and leader-forwarding logic.

### 3. Reachability probe for other voters (modelled on ADR-0097)

New unexported helper in `internal/manager`:

```go
// probeRaftdVoters dials each configured voter's raft_bind_address
// from the current leader's vantage point, with a bounded timeout.
// Returns a slice aligned with the input voters (never nil).
func probeRaftdVoters(ctx context.Context, voters []*internalpb.ServerInfo, selfNodeID string, dial func(context.Context, string) error) []guardrail.VoterReachability
```

- **Only the leader runs it** — same reasoning as `ApproveJoinRequest`: only the leader's network vantage point matters for a decision that affects quorum.
- **Input**: the voter list from `raft.Status().Servers` (filtered to `Suffrage == "Voter"`), excluding the target node itself.
- **Dial target**: each voter's `Address` field (this is the `raft_bind_address` from the raft configuration).
- **Timeout**: `3 * time.Second` per dial (matching `reachabilityCheckTimeout` used elsewhere), with an overall deadline of `10 * time.Second` for the whole probe set.
- **Result per voter**: `Reachable=true` + empty `ProbeError` on success; `Reachable=false` + `ProbeError` on dial failure (timeout, connection refused, TLS alert, etc.).
- **Probe failure modes**:
  - If the probe *itself* cannot run (e.g., leader cannot read raft status, or context cancelled before any dial), the guardrail returns `Unknown` (fail closed).
  - If individual dials fail, those voters are marked `Unreachable` — the quorum evaluator treats them as down.
  - No retry per voter; a single dial attempt is the signal. Operators can re-preflight after fixing network issues.

### 4. Quorum-safety evaluator: `guardrail.EvaluateRaftdQuorumSafety`

New fact type and pure function in `internal/guardrail`:

```go
type VoterReachability struct {
    NodeID      string
    Reachable   bool
    ProbeError  string
}

type RaftdQuorumFact struct {
    TargetNodeID    string
    IsTargetVoter   bool
    IsTargetLeader  bool
    OtherVoters     []VoterReachability
    ProbeReadOK     bool // false if the probe itself failed to run
}

func EvaluateRaftdQuorumSafety(fact RaftdQuorumFact) Report
```

**Logic** (mirrors `ComputeQuorumImpact` exactly):

- If `!ProbeReadOK` → `Unknown` (fail closed).
- If `!IsTargetVoter` → `Allow` (non-voter restart carries no quorum risk).
- `TotalVoters = 1 (target) + len(OtherVoters)`.
- `QuorumSize = TotalVoters/2 + 1` (integer majority, matching hashicorp/raft).
- `UpCount = count of OtherVoters where Reachable == true`.
- If `UpCount < QuorumSize - 1` → `Block` with `Rule: "raftd-quorum-safety"`, `Detail` explaining the arithmetic (total voters, quorum required, reachable remaining).
- Else → `Allow`.

**Leader-specific finding**: If `IsTargetLeader == true`, additionally emit a `Block`-by-default finding with `Rule: "raftd-leader-restart"`, `Detail: "<node> is the current raft leader - restarting it will trigger a leader election"`. This is overridable by the same `Force` flag as the quorum check (consistent with ADR-0103's `Force` semantics).

### 5. Preflight integration: `PreflightRestartNodeService`

Generalize the existing handler (currently hardcoded to `restartGuardrailService == "apiary_managerd"`):

- Introduce `restartGuardrailServices = map[string]bool{"apiary_managerd": true, "apiary_raftd": true}`.
- When `name == "apiary_raftd"`:
  1. Verify caller is leader (or forward to leader — same pattern as `ReserveRestartLease`).
  2. Read `raft.Status()` → voter list.
  3. Run `probeRaftdVoters` against other voters.
  4. Build `RaftdQuorumFact` and call `EvaluateRaftdQuorumSafety`.
  5. Merge findings with any existing cooldown/lease findings (distinct `Rule` strings so the frontend can distinguish).
- Response shape unchanged: `Verdict` + `Findings[]` (proto `GuardrailFinding` with `Rule`, `Detail`, `Evidence`).

### 6. Real enforcement: `RestartNodeService`

- `apiaryServices` in `internal/manager/services.go` gains `{name: "apiary_raftd", restartable: true}`.
- `RestartNodeService` checks `restartGuardrailServices[name]` — if true, calls `reserveRestartLease` with `Service: "apiary_raftd"` before restarting.
- `reserveRestartLease` already authors `voter_node_ids` from the leader's current `Status()` and submits `AcquireRestartLease` — no changes needed.
- The pending-restart file write (before issuing the restart command) already happens for any service going through the lease path.
- The restart command itself (`service apiary_raftd restart`) is issued in the background goroutine after a 250ms flush delay — same as managerd.

### 7. Timeout/budget constraints

| Step | Timeout | Rationale |
|------|---------|-----------|
| Per-voter dial | 3s | Matches `reachabilityCheckTimeout`; a healthy LAN peer responds in <100ms. |
| Full probe set | 10s | 3 voters × 3s sequential worst case; parallel dials would complete in ~3s. |
| Lease reservation (RPC) | 10s | Existing `ReserveRestartLease` timeout; raft apply is typically <100ms. |
| Confirm-on-startup retry | 5 × 3s | Identical to managerd; bounded, gives up loudly. |
| Restart command | 30s | `restartCommandTimeout` already used; `service restart` on FreeBSD is fast. |

### 8. Probe failure semantics: abort (fail closed), never defer

- If the leader cannot *run* the probe (status read error, context cancelled) → `PreflightRestartNodeService` returns `Verdict: Unknown`, `RestartNodeService` refuses the lease reservation with an error.
- If individual voters are unreachable → they count as down in the quorum arithmetic. The restart is allowed only if the *remaining reachable* voters meet quorum without them.
- No "defer and retry later" logic — the operator re-runs preflight when conditions change. This avoids hidden state machines and matches ADR-0103's explicit `Unknown == Block` philosophy.

### 9. Proto/wire changes

**No new proto messages.** All existing messages are generic over `service` string:

- `RestartLease.Service`, `RestartRecord.Service` — already string.
- `AcquireRestartLease.Service`, `RecordRestartCompleted.Service` — already string.
- `ReserveRestartLeaseRequest.Service`, `ConfirmRestartCompletedRequest.Service` — already string.
- `PreflightRestartNodeServiceRequest.Name` / `RestartNodeServiceRequest.Name` — already string.
- `GuardrailFinding.Rule` — new values `"raftd-quorum-safety"` and `"raftd-leader-restart"` are just strings.

**Snapshot/restore:** `FSMSnapshotState` already includes `map<string, RestartLease>` and `map<string, RestartRecord>` — raftd leases and records persist automatically.

### 10. Frontend wiring

Zero new frontend code. The Machine page's node-services panel (`internal/frontend/machine.go`, `renderNodeServicesPanel`) already:

- Renders a row per service from `ListNodeServices` (which reads `apiaryServices`).
- Shows `Restartable: true` → displays the restart button.
- Calls `PreflightRestartNodeService` on click → shows findings + force checkbox if `Verdict != Allow`.
- Submits `RestartNodeServiceRequest{Force: true}` on acknowledged override.

The template renders `Findings[].Detail` generically; the new rule strings get appropriate copy automatically.

### 11. Sequential maintenance across Combs (no orchestrator)

Same guarantees as ADR-0116's design:

1. **Lease serialization** — only one unconfirmed `apiary_raftd` lease cluster-wide at a time (raft log order).
2. **Quorum gate** — a second voter restart is blocked until the first voter is confirmed reachable (its lease is cleared by its own startup confirm).
3. **Leader acknowledgment** — the `"raftd-leader-restart"` finding nudges operators to restart followers first.
4. **Cooldown** — 600s (10 min) after a confirmed voter restart, same as managerd.

This is sufficient for safe hand-driven rolling maintenance. A fully automated rolling-restart controller remains out of scope.

## Consequences

### Positive

- **Zero new raft schema** — reuses `RestartLease`/`RestartRecord` maps keyed by service string.
- **Single new network primitive** — `probeRaftdVoters` is a direct analog of ADR-0097's `dialReachable`, same timeout, same caller (leader), same swap-for-tests pattern.
- **Confirm-on-startup gap closed** — `cmd/raftd` gets a minimal, well-scoped startup hook that dials local managerd (already running, same process supervision tree) and reuses the existing confirmation RPC path.
- **Pure evaluator** — `EvaluateRaftdQuorumSafety` is a pure function over caller-supplied facts; unit-testable in isolation, same as `EvaluateConcurrentManagerRestart`.
- **Consistent operator experience** — same preflight UI, same force checkbox, same lease/cooldown semantics as managerd restarts.

### Negative / Risks

- **New code in `cmd/raftd` startup path** — a bug here could leave a raftd lease permanently stuck (no TTL, no auto-expiry). Mitigated by: (a) identical retry/logic to managerd's proven hook, (b) the pending-restart file is written *before* the restart command, so a crash before confirm still blocks correctly, (c) `Force` override always available to an operator with the restart-guardrail token.
- **Reachability probe is a moment-in-time snapshot** — a voter could become unreachable between probe and restart. This is inherent to any probe-based guardrail; the design treats `Unknown` as `Block` and requires the operator to re-preflight.
- **Leader restart still triggers election** — the `"raftd-leader-restart"` finding is acknowledgment-only. Automatic `LeadershipTransfer()` remains a future ADR (no RPC/FSM changes needed to add later).
- **Requires restart-guardrail token on all nodes** — same as managerd; an unprovisioned node will reject lease RPCs. This is existing operational requirement, not new.

### Verification

1. **Unit tests** (`internal/guardrail`):
   - `EvaluateRaftdQuorumSafety` table-driven: 3-voter (quorum=2) with 0/1/2 other reachable; 5-voter (quorum=3) combinations; non-voter target; `ProbeReadOK=false`; leader-target with/without force.
   - `probeRaftdVoters` (manager package, swappable dial): happy path, one unreachable, all unreachable, timeout, context cancellation.

2. **FSM tests** (`internal/raft/fsm_test.go`):
   - `AcquireRestartLease` with `service="apiary_raftd"` grants/blocks/forces correctly.
   - `RecordRestartCompleted` releases lease on exact match, writes record unconditionally.
   - Snapshot/restore round-trip includes raftd lease/record.

3. **Manager integration tests** (`internal/manager/integration_test.go`):
   - Two-node fixture: preflight on follower forwards to leader, probe runs from leader, verdict matches quorum arithmetic.
   - Three-node fixture: restart one voter (allow), restart second voter while first unconfirmed (block), restart second after first confirms (allow), restart leader with force (allow + leader finding).
   - Probe failure → `Unknown` verdict blocks lease reservation.

4. **End-to-end `cmd/raftd` confirm test**:
   - Start a 3-node cluster, reserve raftd lease on node A, `service apiary_raftd restart` on node A, verify node A's new raftd process confirms and clears the lease, verify node B can then restart.

5. **Static checks**: `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...`, `buf generate` (no proto changes expected).

6. **Live verification** (operator-run, recorded in this ADR once done):
   - Provision identical `restart-guardrail-token` on all Combs.
   - `PreflightRestartNodeService("apiary_raftd")` on a healthy 3-voter cluster → `Allow`.
   - Kill one voter's raftd, preflight on a second voter → `Block` (quorum-safety).
   - Restart first voter, wait for confirm, preflight on second → `Allow`.
   - Preflight on current leader → `Block` with `raftd-leader-restart` finding; `Force` overrides.

## Implementation Order (for the human implementer)

1. **`internal/guardrail`**: Add `VoterReachability`, `RaftdQuorumFact`, `EvaluateRaftdQuorumSafety` + tests.
2. **`internal/manager`**: Add `probeRaftdVoters` (using existing `dialReachable` / `reachabilityCheck` pattern), generalize `PreflightRestartNodeService` and `RestartNodeService` to consult `restartGuardrailServices` map, add `restartGuardrailServices` constant.
3. **`internal/manager/services.go`**: Set `restartable: true` for `apiary_raftd`.
4. **`cmd/raftd/main.go`**: Add `RestartConfirmStore` load + `confirmPendingRestartOnStartup` equivalent (dial local managerd socket, call `ConfirmRestartCompletedLocal`).
5. **Frontend**: Verify template renders new rule strings correctly (likely no change needed).
6. **Run full verification suite** above.

## References

- ADR-0116 — original design (not implemented), gaps identified here.
- ADR-0103 — restart-lease FSM, `RestartLease`/`RestartRecord`, `AcquireRestartLease`/`RecordRestartCompleted`, `EvaluateConcurrentManagerRestart`, confirm-on-startup pattern.
- ADR-0097 / ADR-0103 — `dialReachable`, `evaluateJoinReachability`, leader-only probe rationale.
- ADR-0052 — `SimulateNodeFailure` / `ComputeQuorumImpact` quorum arithmetic (reused here).
- ADR-0124 — `cmd/raftd` process boundary confirmation (confirms the startup hook location).