package cluster

import (
	"strings"
	"testing"
)

func TestIsKnownTarget(t *testing.T) {
	servers := []ServerSuffrage{{ID: "node-a", Suffrage: "Voter"}, {ID: "node-b", Suffrage: "Voter"}}
	resources := []OwnedResourcePlacement{
		{ID: "vm-1", NodeID: "node-b", ReplicaNodeID: "node-c"},
	}

	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"raft server ID", "node-a", true},
		{"resource owner node_id", "node-b", true},
		{"resource replica_node_id even absent from raft config", "node-c", true},
		{"genuinely unknown", "node-does-not-exist", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsKnownTarget(servers, resources, c.target); got != c.want {
				t.Errorf("IsKnownTarget(%q) = %v, want %v", c.target, got, c.want)
			}
		})
	}
}

func TestComputeQuorumImpact(t *testing.T) {
	cases := []struct {
		name       string
		servers    []ServerSuffrage
		target     string
		wantSurv   bool
		wantTotal  uint32
		wantRemain uint32
		wantReach  uint32
		wantUnk    uint32
	}{
		{
			name: "two-voter cluster losing one fails quorum",
			servers: []ServerSuffrage{
				{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "b", Suffrage: "Voter", Reachability: ReachabilityReachable},
			},
			target: "a", wantSurv: false, wantTotal: 2, wantRemain: 1, wantReach: 1, wantUnk: 0,
		},
		{
			name: "three-voter cluster losing one survives when both remaining are reachable",
			servers: []ServerSuffrage{
				{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "b", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "c", Suffrage: "Voter", Reachability: ReachabilityReachable},
			},
			target: "a", wantSurv: true, wantTotal: 3, wantRemain: 2, wantReach: 2, wantUnk: 0,
		},
		{
			name: "three-voter cluster losing one fails when a remaining voter is already unreachable",
			servers: []ServerSuffrage{
				{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "b", Suffrage: "Voter", Reachability: ReachabilityUnreachable},
				{ID: "c", Suffrage: "Voter", Reachability: ReachabilityReachable},
			},
			target: "a", wantSurv: false, wantTotal: 3, wantRemain: 2, wantReach: 1, wantUnk: 0,
		},
		{
			name: "unknown reachability is never credited toward a failing quorum",
			servers: []ServerSuffrage{
				{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "b", Suffrage: "Voter", Reachability: ReachabilityUnknown},
				{ID: "c", Suffrage: "Voter", Reachability: ReachabilityReachable},
			},
			// 3 voters, quorumSize=2; removing "a" leaves b(unknown)+c(reachable) -
			// only 1 CONFIRMED reachable, short of quorumSize even though 2
			// voters remain on paper. The unknown one must not be credited.
			target: "a", wantSurv: false, wantTotal: 3, wantRemain: 2, wantReach: 1, wantUnk: 1,
		},
		{
			name: "quorum survives on confirmed-reachable voters alone despite a surplus unknown",
			servers: []ServerSuffrage{
				{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "b", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "c", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "d", Suffrage: "Voter", Reachability: ReachabilityUnknown},
				{ID: "e", Suffrage: "Voter", Reachability: ReachabilityReachable},
			},
			// 5 voters, quorumSize=3; removing "a" leaves b,c,e confirmed
			// reachable (3, meeting quorum on their own) plus d unknown - the
			// unknown one is surplus to the answer but still worth flagging.
			target: "a", wantSurv: true, wantTotal: 5, wantRemain: 4, wantReach: 3, wantUnk: 1,
		},
		{
			name:    "single-voter cluster losing itself",
			servers: []ServerSuffrage{{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable}},
			target:  "a", wantSurv: false, wantTotal: 1, wantRemain: 0, wantReach: 0, wantUnk: 0,
		},
		{
			name: "target is a non-voter",
			servers: []ServerSuffrage{
				{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable},
				{ID: "b", Suffrage: "Nonvoter", Reachability: ReachabilityReachable},
			},
			target: "b", wantSurv: true, wantTotal: 1, wantRemain: 1, wantReach: 1, wantUnk: 0,
		},
		{
			name: "target absent from raft config entirely",
			servers: []ServerSuffrage{
				{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable},
			},
			target: "ghost", wantSurv: true, wantTotal: 1, wantRemain: 1, wantReach: 1, wantUnk: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeQuorumImpact(c.servers, c.target)
			if got.Survives != c.wantSurv {
				t.Errorf("Survives = %v, want %v (note: %s)", got.Survives, c.wantSurv, got.Note)
			}
			if got.TotalVoters != c.wantTotal {
				t.Errorf("TotalVoters = %d, want %d", got.TotalVoters, c.wantTotal)
			}
			if got.RemainingVoters != c.wantRemain {
				t.Errorf("RemainingVoters = %d, want %d", got.RemainingVoters, c.wantRemain)
			}
			if got.RemainingReachable != c.wantReach {
				t.Errorf("RemainingReachable = %d, want %d", got.RemainingReachable, c.wantReach)
			}
			if got.RemainingUnknown != c.wantUnk {
				t.Errorf("RemainingUnknown = %d, want %d", got.RemainingUnknown, c.wantUnk)
			}
			if got.Note == "" {
				t.Error("Note is empty, want an explanation")
			}
		})
	}
}

func TestComputeQuorumImpact_UnknownVoterNotedExplicitly(t *testing.T) {
	servers := []ServerSuffrage{
		{ID: "a", Suffrage: "Voter", Reachability: ReachabilityReachable},
		{ID: "b", Suffrage: "Voter", Reachability: ReachabilityReachable},
		{ID: "c", Suffrage: "Voter", Reachability: ReachabilityReachable},
		{ID: "d", Suffrage: "Voter", Reachability: ReachabilityUnknown},
		{ID: "e", Suffrage: "Voter", Reachability: ReachabilityReachable},
	}
	got := ComputeQuorumImpact(servers, "a")
	if !got.Survives {
		t.Fatalf("Survives = false, want true (note: %s)", got.Note)
	}
	if !strings.Contains(got.Note, "unverified reachability") {
		t.Errorf("Note = %q, want it to flag the unverified remaining voter explicitly even though quorum survives without it", got.Note)
	}
}

func TestComputeOwnedResourceImpacts_NoReplicaIsUnprotected(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", Name: "web-01", NodeID: "target", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, "target")
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Verdict != RecoveryVerdictUnprotected {
		t.Errorf("Verdict = %q, want %q", got[0].Verdict, RecoveryVerdictUnprotected)
	}
	if strings.Contains(strings.ToLower(got[0].Explanation), "permanently") || strings.Contains(strings.ToLower(got[0].Explanation), "lost forever") {
		t.Errorf("Explanation must not claim permanent/certain loss: %q", got[0].Explanation)
	}
}

func TestComputeOwnedResourceImpacts_WithReplicaIsUnverified(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", Name: "web-01", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, "target")
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Verdict != RecoveryVerdictUnverifiedReplica {
		t.Errorf("Verdict = %q, want %q", got[0].Verdict, RecoveryVerdictUnverifiedReplica)
	}
	if !strings.Contains(got[0].Explanation, "hastctl status") {
		t.Errorf("Explanation = %q, want it to instruct a manual hastctl status check", got[0].Explanation)
	}
}

func TestComputeOwnedResourceImpacts_ZeroOwnedIsEmpty(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "other-node"}}
	got := ComputeOwnedResourceImpacts(resources, "target")
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestComputeOwnedResourceImpacts_SortedByID(t *testing.T) {
	resources := []OwnedResourcePlacement{
		{ID: "vm-2", NodeID: "target"},
		{ID: "vm-1", NodeID: "target"},
	}
	got := ComputeOwnedResourceImpacts(resources, "target")
	if len(got) != 2 || got[0].ID != "vm-1" || got[1].ID != "vm-2" {
		t.Errorf("got = %+v, want sorted by ID", got)
	}
}

func TestComputeReplicaBackedImpacts_OwnershipAndReplicaAreSeparate(t *testing.T) {
	resources := []OwnedResourcePlacement{
		{ID: "vm-1", Name: "web-01", Kind: ResourceKindVM, NodeID: "owner-node", ReplicaNodeID: "target"},
	}

	replicaBacked := ComputeReplicaBackedImpacts(resources, "target")
	if len(replicaBacked) != 1 {
		t.Fatalf("len(replicaBacked) = %d, want 1", len(replicaBacked))
	}
	if replicaBacked[0].OwnerNodeID != "owner-node" {
		t.Errorf("OwnerNodeID = %q, want owner-node", replicaBacked[0].OwnerNodeID)
	}

	// The same resource must NOT also appear as something "target" owns -
	// ownership and replica-backing are genuinely separate consequences.
	owned := ComputeOwnedResourceImpacts(resources, "target")
	if len(owned) != 0 {
		t.Errorf("ComputeOwnedResourceImpacts(target) = %+v, want empty - vm-1 is owned by owner-node, not target", owned)
	}
}

func TestComputeReplicaBackedImpacts_SortedByID(t *testing.T) {
	resources := []OwnedResourcePlacement{
		{ID: "vm-2", NodeID: "owner", ReplicaNodeID: "target"},
		{ID: "vm-1", NodeID: "owner", ReplicaNodeID: "target"},
	}
	got := ComputeReplicaBackedImpacts(resources, "target")
	if len(got) != 2 || got[0].ID != "vm-1" || got[1].ID != "vm-2" {
		t.Errorf("got = %+v, want sorted by ID", got)
	}
}

func TestSimulateNodeFailure_CombinesAllThree(t *testing.T) {
	servers := []ServerSuffrage{
		{ID: "target", Suffrage: "Voter", Reachability: ReachabilityReachable},
		{ID: "other", Suffrage: "Voter", Reachability: ReachabilityReachable},
	}
	resources := []OwnedResourcePlacement{
		{ID: "vm-1", NodeID: "target"},
		{ID: "vm-2", NodeID: "other", ReplicaNodeID: "target"},
	}

	report := SimulateNodeFailure(servers, resources, "target")
	if len(report.OwnedResources) != 1 || report.OwnedResources[0].ID != "vm-1" {
		t.Errorf("OwnedResources = %+v, want just vm-1", report.OwnedResources)
	}
	if len(report.ReplicaBackedResources) != 1 || report.ReplicaBackedResources[0].ID != "vm-2" {
		t.Errorf("ReplicaBackedResources = %+v, want just vm-2", report.ReplicaBackedResources)
	}
	if report.Quorum.TotalVoters != 2 {
		t.Errorf("Quorum.TotalVoters = %d, want 2", report.Quorum.TotalVoters)
	}
}

func TestSimulateNetworkFailure_ReportsOnlyAttachedCells(t *testing.T) {
	target := ManagedNetworkPlacement{ID: "net-1", Name: "services", VLANID: 100, Subnet: "10.60.0.0/24"}
	resources := []NetworkAttachedResourcePlacement{
		{ID: "vm-b", Name: "database", NodeID: "node-b", NetworkID: "net-1"},
		{ID: "vm-other", Name: "unrelated", NodeID: "node-a", NetworkID: "net-2"},
		{ID: "vm-a", Name: "frontend", NodeID: "node-a", NetworkID: "net-1"},
	}

	report := SimulateNetworkFailure(target, resources)
	if report.Network.ID != "net-1" {
		t.Fatalf("Network.ID = %q, want net-1", report.Network.ID)
	}
	if len(report.AffectedResources) != 2 {
		t.Fatalf("AffectedResources = %+v, want 2 entries", report.AffectedResources)
	}
	if report.AffectedResources[0].ID != "vm-a" || report.AffectedResources[1].ID != "vm-b" {
		t.Errorf("AffectedResources order = %+v, want vm-a then vm-b", report.AffectedResources)
	}
	if !strings.Contains(report.AffectedResources[0].Explanation, "does not claim") {
		t.Errorf("Explanation = %q, want conservative scope", report.AffectedResources[0].Explanation)
	}
	if report.Note == "" {
		t.Error("Note is empty, want observation limitations")
	}
}

func TestSimulateNetworkFailure_NoAttachedCellsIsExplicitlyEmpty(t *testing.T) {
	report := SimulateNetworkFailure(ManagedNetworkPlacement{ID: "net-1"}, nil)
	if report.AffectedResources == nil {
		t.Fatal("AffectedResources is nil, want an explicitly empty slice")
	}
	if len(report.AffectedResources) != 0 {
		t.Fatalf("AffectedResources = %+v, want none", report.AffectedResources)
	}
}
