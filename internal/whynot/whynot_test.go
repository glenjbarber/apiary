package whynot

import (
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/invariant"
)

func hasBlockerInvariant(blockers []Blocker, name string) bool {
	for _, b := range blockers {
		if b.Invariant == name {
			return true
		}
	}
	return false
}

func TestAnswerCellMigrate_BlockedWhenNoReplicaConfigured(t *testing.T) {
	ans := AnswerCellMigrate(CellFact{ID: "vm-1", Name: "web-1", Kind: "vm", DesiredState: "running"})
	if ans.Verdict != VerdictBlocked {
		t.Fatalf("expected VerdictBlocked, got %v", ans.Verdict)
	}
	if len(ans.Remedies) != 1 || !ans.Remedies[0].Proven {
		t.Fatalf("expected exactly one Proven remedy, got %+v", ans.Remedies)
	}
}

func TestAnswerCellMigrate_BlockedWhenDeleting(t *testing.T) {
	ans := AnswerCellMigrate(CellFact{ID: "vm-1", Name: "web-1", Kind: "vm", DesiredState: "deleting", ReplicaNodeID: "node-b"})
	if ans.Verdict != VerdictBlocked {
		t.Fatalf("expected VerdictBlocked for a deleting cell, got %v", ans.Verdict)
	}
	if len(ans.Remedies) != 0 {
		t.Errorf("a deleting cell has no proven remedy - expected none, got %+v", ans.Remedies)
	}
}

func TestAnswerCellMigrate_UnknownNotBlockedWhenIncapableDestination(t *testing.T) {
	// This is the key divergence from AnswerCellRecoverable (finding 1):
	// MigrateVM/MigrateJail never check destination capability at all,
	// so a confirmed-incapable replica target must NOT block migrate -
	// it must still resolve Unknown (sync unconfirmed), never Blocked.
	ans := AnswerCellMigrate(CellFact{
		ID: "vm-1", Name: "web-1", Kind: "vm", DesiredState: "running",
		ReplicaNodeID: "node-b", DestinationCapable: invariant.ResultFalse, DestinationCapableDetail: "node-b: bhyve NOT configured",
	})
	if ans.Verdict != VerdictUnknown {
		t.Fatalf("expected VerdictUnknown (real MigrateVM has no capability check) even with an incapable destination, got %v", ans.Verdict)
	}
	if len(ans.Blockers) != 0 {
		t.Errorf("expected no blockers, got %+v", ans.Blockers)
	}
}

func TestAnswerCellRecoverable_BlockedWhenNoReplicaConfigured(t *testing.T) {
	ans := AnswerCellRecoverable(CellFact{ID: "vm-1", Name: "web-1", Kind: "vm"})
	if ans.Verdict != VerdictBlocked {
		t.Fatalf("expected VerdictBlocked (synthesized locally), got %v", ans.Verdict)
	}
	if !hasBlockerInvariant(ans.Blockers, "cell-recoverability") {
		t.Errorf("expected a cell-recoverability blocker, got %+v", ans.Blockers)
	}
}

func TestAnswerCellRecoverable_BlockedWhenDestinationConfirmedIncapable(t *testing.T) {
	ans := AnswerCellRecoverable(CellFact{
		ID: "vm-1", Name: "web-1", Kind: "vm",
		ReplicaNodeID: "node-b", DestinationCapable: invariant.ResultFalse, DestinationCapableDetail: "node-b: bhyve NOT configured",
	})
	if ans.Verdict != VerdictBlocked {
		t.Fatalf("expected VerdictBlocked, got %v", ans.Verdict)
	}
}

func TestAnswerCellRecoverable_NeverClear(t *testing.T) {
	ans := AnswerCellRecoverable(CellFact{
		ID: "vm-1", Name: "web-1", Kind: "vm",
		ReplicaNodeID: "node-b", DestinationCapable: invariant.ResultTrue, DestinationCapableDetail: "node-b: bhyve configured",
	})
	if ans.Verdict == VerdictClear {
		t.Fatalf("cell-recoverability can never resolve Clear/True in v1 (sync is never confirmable) - got %v", ans.Verdict)
	}
	if ans.Verdict != VerdictUnknown {
		t.Fatalf("expected VerdictUnknown, got %v", ans.Verdict)
	}
	if len(ans.Caveats) == 0 {
		t.Errorf("expected the permanent HAST-sync-gap caveat to be present")
	}
}

func TestAnswerHiveReboot_BlockedWhenQuorumDoesNotSurvive(t *testing.T) {
	ans := AnswerHiveReboot("node-a", QuorumFact{Survives: false, Note: "removing node-a leaves quorum unreachable"}, nil, nil)
	if ans.Verdict != VerdictBlocked {
		t.Fatalf("expected VerdictBlocked, got %v", ans.Verdict)
	}
	if !hasBlockerInvariant(ans.Blockers, "quorum-tolerance") {
		t.Errorf("expected a quorum-tolerance blocker, got %+v", ans.Blockers)
	}
}

func TestAnswerHiveReboot_SingleNodeExplainsTemporaryOutage(t *testing.T) {
	ans := AnswerHiveReboot("node-a", QuorumFact{
		Survives: false, TotalVoters: 1,
		Note: "this is the cluster's only voter - losing it ends the cluster's ability to reach quorum entirely.",
	}, nil, nil)
	if ans.Verdict != VerdictBlocked {
		t.Fatalf("expected VerdictBlocked, got %v", ans.Verdict)
	}
	if len(ans.Blockers) != 1 {
		t.Fatalf("expected one quorum blocker, got %+v", ans.Blockers)
	}
	detail := ans.Blockers[0].Detail
	for _, want := range []string{"single-node Colony", "temporarily", "does not by itself imply permanent data loss"} {
		if !strings.Contains(detail, want) {
			t.Errorf("single-node blocker does not contain %q: %s", want, detail)
		}
	}
	if strings.Contains(detail, "ends the cluster") {
		t.Errorf("single-node blocker retained the ambiguous permanent-loss wording: %s", detail)
	}
}

func TestAnswerHiveReboot_QuorumBlockerCarriesVoterByVoterEvidence(t *testing.T) {
	ans := AnswerHiveReboot("node-a", QuorumFact{
		Survives:           false,
		TotalVoters:        3,
		QuorumSize:         2,
		RemainingReachable: 0,
		RemainingUnknown:   1,
		Note:               "quorum is LOST - the confirmed-reachable remaining voters do not form a majority.",
		Voters: []VoterFact{
			{NodeID: "node-b", Reachability: "unreachable"},
			{NodeID: "node-c", Reachability: "unknown"},
		},
	}, nil, nil)

	if len(ans.Blockers) != 1 {
		t.Fatalf("expected exactly one blocker, got %+v", ans.Blockers)
	}
	ev := ans.Blockers[0].Evidence
	if len(ev) != 3 {
		t.Fatalf("expected 1 summary + 2 per-voter evidence entries, got %d: %+v", len(ev), ev)
	}
	if !strings.Contains(ev[0].Detail, "3 total voter(s)") || !strings.Contains(ev[0].Detail, "majority requires 2") {
		t.Errorf("summary evidence missing the raw counts: %q", ev[0].Detail)
	}
	if ev[0].ObservedAt.IsZero() {
		t.Errorf("summary evidence must be stamped as freshly observed, not a permanent zero-ObservedAt caveat")
	}
	if ev[1].Detail != "node-b: unreachable" {
		t.Errorf("ev[1].Detail = %q, want %q", ev[1].Detail, "node-b: unreachable")
	}
	if ev[2].Detail != "node-c: unknown" {
		t.Errorf("ev[2].Detail = %q, want %q", ev[2].Detail, "node-c: unknown")
	}
	for i, e := range ev {
		if e.ObservedAt.IsZero() {
			t.Errorf("ev[%d] must be freshly observed (non-zero ObservedAt): %+v", i, e)
		}
	}
}

func TestAnswerHiveReboot_ClearQuorumHasNoQuorumBlockerOrEvidence(t *testing.T) {
	ans := AnswerHiveReboot("node-a", QuorumFact{
		Survives: true, TotalVoters: 3, QuorumSize: 2, RemainingReachable: 2,
		Voters: []VoterFact{{NodeID: "node-b", Reachability: "reachable"}, {NodeID: "node-c", Reachability: "reachable"}},
	}, nil, nil)
	if hasBlockerInvariant(ans.Blockers, "quorum-tolerance") {
		t.Errorf("expected no quorum-tolerance blocker when quorum survives, got %+v", ans.Blockers)
	}
}

func TestAnswerHiveReboot_UnprotectedAndUnverifiedReplicaAreEquallySevere(t *testing.T) {
	owned := []OwnedResourceFact{
		{ID: "vm-1", Name: "web-1", Kind: "vm", Verdict: "unprotected"},
		{ID: "vm-2", Name: "web-2", Kind: "vm", ReplicaNodeID: "node-b", Verdict: "unverified_replica"},
	}
	ans := AnswerHiveReboot("node-a", QuorumFact{Survives: true}, owned, nil)
	if ans.Verdict != VerdictBlocked {
		t.Fatalf("expected VerdictBlocked, got %v", ans.Verdict)
	}
	if len(ans.Blockers) != 2 {
		t.Fatalf("expected both unprotected and unverified_replica to independently block, got %d blockers: %+v", len(ans.Blockers), ans.Blockers)
	}
}

func TestAnswerHiveReboot_ReplicaBackedNeverAppearsInBlockers(t *testing.T) {
	replicaBacked := []ReplicaBackedFact{{ID: "vm-9", Name: "db-1", Kind: "vm", OwnerNodeID: "node-c"}}
	ans := AnswerHiveReboot("node-a", QuorumFact{Survives: true}, nil, replicaBacked)
	if ans.Verdict != VerdictClear {
		t.Fatalf("a replica-backed-only resource must not block a reboot - expected VerdictClear, got %v", ans.Verdict)
	}
	if len(ans.Blockers) != 0 {
		t.Fatalf("expected zero blockers, got %+v", ans.Blockers)
	}
	if len(ans.Caveats) != 1 {
		t.Fatalf("expected the replica-backed resource to appear as exactly one caveat, got %+v", ans.Caveats)
	}
}

func TestAnswerHiveReboot_ClearWhenNothingOwnedAndQuorumSurvives(t *testing.T) {
	ans := AnswerHiveReboot("node-a", QuorumFact{Survives: true}, nil, nil)
	if ans.Verdict != VerdictClear {
		t.Fatalf("expected VerdictClear, got %v", ans.Verdict)
	}
}

func TestAnswerNetworkConnectivity_BlockedOnlyFromDownBridge(t *testing.T) {
	fact := invariant.NetworkFact{
		ID: "net-1", Name: "prod",
		Observations: []invariant.BridgeObservation{{NodeID: "node-a", Status: "down"}},
	}
	ans := AnswerNetworkConnectivity(fact)
	if ans.Verdict != VerdictBlocked {
		t.Fatalf("expected VerdictBlocked, got %v", ans.Verdict)
	}
	if !hasBlockerInvariant(ans.Blockers, "network-route-dns") {
		t.Errorf("expected a network-route-dns blocker, got %+v", ans.Blockers)
	}
}

func TestAnswerNetworkConnectivity_FetchFailureIsCaveatNotBlocker(t *testing.T) {
	fact := invariant.NetworkFact{
		ID: "net-1", Name: "prod",
		Observations: []invariant.BridgeObservation{{NodeID: "node-a", Err: "dial tcp: connection refused"}},
	}
	ans := AnswerNetworkConnectivity(fact)
	if ans.Verdict != VerdictUnknown {
		t.Fatalf("a fetch failure must resolve Unknown, not Blocked - got %v", ans.Verdict)
	}
	if len(ans.Blockers) != 0 {
		t.Fatalf("expected zero blockers from a fetch failure, got %+v", ans.Blockers)
	}
}

func TestAnswerNetworkConnectivity_NeverClear(t *testing.T) {
	fact := invariant.NetworkFact{
		ID: "net-1", Name: "prod",
		Observations: []invariant.BridgeObservation{{NodeID: "node-a", Status: "up"}},
	}
	ans := AnswerNetworkConnectivity(fact)
	if ans.Verdict == VerdictClear {
		t.Fatalf("network connectivity can never resolve Clear in v1 (DNS is never verifiable) - got %v", ans.Verdict)
	}
	if ans.Verdict != VerdictUnknown {
		t.Fatalf("expected VerdictUnknown, got %v", ans.Verdict)
	}
}

func TestAnswerNetworkConnectivity_UnknownWithZeroObservations(t *testing.T) {
	ans := AnswerNetworkConnectivity(invariant.NetworkFact{ID: "net-1", Name: "prod"})
	if ans.Verdict != VerdictUnknown {
		t.Fatalf("expected VerdictUnknown for a network with nothing attached, got %v", ans.Verdict)
	}
}
