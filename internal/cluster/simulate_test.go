package cluster

import (
	"slices"
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

func TestComputeQuorumImpact_VotersListsEveryOtherVoterSortedByID(t *testing.T) {
	servers := []ServerSuffrage{
		{ID: "c", Suffrage: "Voter", Reachability: ReachabilityUnknown},
		{ID: "a", Suffrage: "Voter", Reachability: ReachabilityUnreachable},
		{ID: "target", Suffrage: "Voter", Reachability: ReachabilityReachable},
		{ID: "b", Suffrage: "Voter", Reachability: ReachabilityReachable},
		{ID: "not-a-voter", Suffrage: "Nonvoter", Reachability: ReachabilityReachable},
	}
	got := ComputeQuorumImpact(servers, "target")

	want := []VoterReachability{
		{ID: "a", Reachability: ReachabilityUnreachable},
		{ID: "b", Reachability: ReachabilityReachable},
		{ID: "c", Reachability: ReachabilityUnknown},
	}
	if len(got.Voters) != len(want) {
		t.Fatalf("len(Voters) = %d, want %d (%+v)", len(got.Voters), len(want), got.Voters)
	}
	for i := range want {
		if got.Voters[i] != want[i] {
			t.Errorf("Voters[%d] = %+v, want %+v", i, got.Voters[i], want[i])
		}
	}
}

func TestComputeOwnedResourceImpacts_NoReplicaIsUnprotected(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", Name: "web-01", NodeID: "target", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, nil, "target")
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
	got := ComputeOwnedResourceImpacts(resources, nil, "target")
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
	got := ComputeOwnedResourceImpacts(resources, nil, "target")
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestComputeOwnedResourceImpacts_SortedByID(t *testing.T) {
	resources := []OwnedResourcePlacement{
		{ID: "vm-2", NodeID: "target"},
		{ID: "vm-1", NodeID: "target"},
	}
	got := ComputeOwnedResourceImpacts(resources, nil, "target")
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
	owned := ComputeOwnedResourceImpacts(resources, nil, "target")
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

	report := SimulateNodeFailure(servers, resources, nil, "target")
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

func TestComputeImageAvailability_ClassifiesRemainingSources(t *testing.T) {
	requirements := []ImageRequirement{
		{ResourceID: "vm-available", ResourceName: "web", ImageName: "ubuntu.raw", Role: ImageRoleBaseImage},
		{ResourceID: "vm-unavailable", ResourceName: "db", ImageName: "rescue.iso", Role: ImageRoleISO},
		{ResourceID: "vm-unknown", ResourceName: "worker", ImageName: "tools.iso", Role: ImageRoleISO},
	}
	inventories := []ImageInventoryObservation{
		{NodeID: "failed", Observed: true, Names: []string{"ubuntu.raw", "rescue.iso", "tools.iso"}},
		{NodeID: "node-a", Observed: true, Names: []string{"ubuntu.raw"}},
		{NodeID: "node-b", Observed: false},
	}

	got := ComputeImageAvailability(requirements, inventories, "failed")
	if len(got) != 3 {
		t.Fatalf("ComputeImageAvailability() = %+v, want 3 impacts", got)
	}
	if got[0].Verdict != ImageAvailabilityAvailable || !slices.Equal(got[0].SourceNodes, []string{"node-a"}) {
		t.Errorf("available impact = %+v", got[0])
	}
	if got[1].Verdict != ImageAvailabilityUnknown || !slices.Equal(got[1].UnknownNodes, []string{"node-b"}) {
		t.Errorf("unavailable-with-unknown impact = %+v", got[1])
	}
	if got[2].Verdict != ImageAvailabilityUnknown {
		t.Errorf("unknown impact = %+v", got[2])
	}
}

func TestComputeImageAvailability_UnavailableRequiresCompleteObservation(t *testing.T) {
	got := ComputeImageAvailability(
		[]ImageRequirement{{ResourceID: "vm-1", ImageName: "missing.raw", Role: ImageRoleBaseImage}},
		[]ImageInventoryObservation{{NodeID: "node-a", Observed: true}},
		"failed",
	)
	if len(got) != 1 || got[0].Verdict != ImageAvailabilityUnavailable {
		t.Fatalf("ComputeImageAvailability() = %+v, want unavailable", got)
	}
}

// --- ADR-0121: live HAST synchronization evidence ---

// replicaObservation builds a successful observation for the tests below.
func replicaObservation(role, status string) ReplicaSyncObservation {
	return ReplicaSyncObservation{
		ResourceID: "vm-1", Kind: ResourceKindVM, ReplicaNodeID: "replica-node",
		Observed: true, Role: role, ResourceStatus: status, Replication: "memsync",
	}
}

func TestComputeOwnedResourceImpacts_SyncObservedIsInSync(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", Name: "web-01", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{replicaObservation("secondary", "complete")}, "target")
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Verdict != RecoveryVerdictReplicaInSync {
		t.Errorf("Verdict = %q, want %q", got[0].Verdict, RecoveryVerdictReplicaInSync)
	}
	if got[0].ReplicaSync == nil {
		t.Fatal("ReplicaSync = nil, want the evidence that produced the verdict")
	}
	if !got[0].ReplicaSync.Observed || got[0].ReplicaSync.Role != "secondary" || got[0].ReplicaSync.ResourceStatus != "complete" {
		t.Errorf("ReplicaSync = %+v, want the verbatim observation", got[0].ReplicaSync)
	}
	// The strongest verdict must still not promise a clean recovery.
	lower := strings.ToLower(got[0].Explanation)
	if strings.Contains(lower, "will recover") || strings.Contains(lower, "guaranteed") || strings.Contains(lower, "safe to lose") {
		t.Errorf("Explanation must not promise recovery: %q", got[0].Explanation)
	}
}

func TestComputeOwnedResourceImpacts_PrimaryRoleIsAlsoInSync(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{replicaObservation("primary", "complete")}, "target")
	if got[0].Verdict != RecoveryVerdictReplicaInSync {
		t.Errorf("Verdict = %q, want %q - a primary role with status complete is healthy", got[0].Verdict, RecoveryVerdictReplicaInSync)
	}
}

func TestComputeOwnedResourceImpacts_InitRoleIsOutOfSync(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{replicaObservation("init", "complete")}, "target")
	if got[0].Verdict != RecoveryVerdictReplicaOutOfSync {
		t.Errorf("Verdict = %q, want %q - role init means never initialized on that node", got[0].Verdict, RecoveryVerdictReplicaOutOfSync)
	}
}

func TestComputeOwnedResourceImpacts_DegradedIsOutOfSync(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{replicaObservation("secondary", "degraded")}, "target")
	if got[0].Verdict != RecoveryVerdictReplicaOutOfSync {
		t.Errorf("Verdict = %q, want %q", got[0].Verdict, RecoveryVerdictReplicaOutOfSync)
	}
}

func TestComputeOwnedResourceImpacts_FailedQueryIsUnobservedNotInSync(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
	observation := ReplicaSyncObservation{
		ResourceID: "vm-1", Kind: ResourceKindVM, ReplicaNodeID: "replica-node",
		Observed: false, Detail: "the replica node did not answer: context deadline exceeded",
	}
	got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{observation}, "target")
	if got[0].Verdict != RecoveryVerdictReplicaUnobserved {
		t.Errorf("Verdict = %q, want %q - a failed query must never read as checked-and-fine", got[0].Verdict, RecoveryVerdictReplicaUnobserved)
	}
	if got[0].ReplicaSync == nil || got[0].ReplicaSync.Observed {
		t.Errorf("ReplicaSync = %+v, want a present, unobserved evidence record", got[0].ReplicaSync)
	}
	if !strings.Contains(got[0].Explanation, "could not check") {
		t.Errorf("Explanation = %q, want it to distinguish 'could not check'", got[0].Explanation)
	}
	if !strings.Contains(got[0].Explanation, "deadline exceeded") {
		t.Errorf("Explanation = %q, want it to cite the actual reason", got[0].Explanation)
	}
}

func TestComputeOwnedResourceImpacts_HastdUnknownStatusIsUnobserved(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{replicaObservation("secondary", "unknown")}, "target")
	if got[0].Verdict != RecoveryVerdictReplicaUnobserved {
		t.Errorf("Verdict = %q, want %q - hastd's own 'unknown' is undeterminable, not a healthy or broken answer", got[0].Verdict, RecoveryVerdictReplicaUnobserved)
	}
}

func TestComputeOwnedResourceImpacts_NoReplicaHasNoEvidence(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "target", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, nil, "target")
	if got[0].Verdict != RecoveryVerdictUnprotected {
		t.Errorf("Verdict = %q, want %q", got[0].Verdict, RecoveryVerdictUnprotected)
	}
	if got[0].ReplicaSync != nil {
		t.Errorf("ReplicaSync = %+v, want nil - an unprotected Cell has no replica to observe", got[0].ReplicaSync)
	}
}

func TestComputeOwnedResourceImpacts_ObservationForOtherResourceIsNotBorrowed(t *testing.T) {
	resources := []OwnedResourcePlacement{
		{ID: "vm-1", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM},
		{ID: "vm-2", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM},
	}
	// Only vm-1 was observed; vm-2 must fall back to unverified_replica
	// rather than inheriting vm-1's healthy observation.
	observations := []ReplicaSyncObservation{replicaObservation("secondary", "complete")}
	got := ComputeOwnedResourceImpacts(resources, observations, "target")
	byID := map[string]OwnedResourceImpact{}
	for _, i := range got {
		byID[i.ID] = i
	}
	if byID["vm-1"].Verdict != RecoveryVerdictReplicaInSync {
		t.Errorf("vm-1 verdict = %q, want in-sync", byID["vm-1"].Verdict)
	}
	if byID["vm-2"].Verdict != RecoveryVerdictUnverifiedReplica {
		t.Errorf("vm-2 verdict = %q, want unverified-replica - one resource's evidence is never another's", byID["vm-2"].Verdict)
	}
	if byID["vm-2"].ReplicaSync != nil {
		t.Errorf("vm-2 ReplicaSync = %+v, want nil when nothing was observed for it", byID["vm-2"].ReplicaSync)
	}
}

func TestReplicasOnlyEverBackedBySimulatedNodeOrOther(t *testing.T) {
	// The regression guard ADR-0052 correction 5 promised: a resource
	// never appears as both owned and replica-backed for one target.
	resources := []OwnedResourcePlacement{
		{ID: "vm-1", NodeID: "target", ReplicaNodeID: "other"},
	}
	report := SimulateNodeFailure(nil, resources, []ReplicaSyncObservation{replicaObservation("secondary", "complete")}, "target")
	if len(report.OwnedResources) != 1 || report.OwnedResources[0].ID != "vm-1" {
		t.Errorf("OwnedResources = %+v, want just vm-1", report.OwnedResources)
	}
	if len(report.ReplicaBackedResources) != 0 {
		t.Errorf("ReplicaBackedResources = %+v, want none - vm-1's replica is 'other', not the target", report.ReplicaBackedResources)
	}
}

// TestComputeOwnedResourceImpacts_SilenceIsNotConfirmedFailure is the
// regression guard for the ADR-0121 review finding that an absent
// status or an unrecognized role was being reported as
// replica_out_of_sync - i.e. "checked and broken" for evidence that
// actually says only "could not check".
func TestComputeOwnedResourceImpacts_SilenceIsNotConfirmedFailure(t *testing.T) {
	tests := []struct {
		name   string
		role   string
		status string
	}{
		{"absent status with a real role", "primary", ""},
		{"whitespace-only status with a real role", "secondary", "   "},
		{"unrecognized role despite a clean status", "promoted", "complete"},
		{"both role and status unrecognized", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
			got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{replicaObservation(tt.role, tt.status)}, "target")
			if got[0].Verdict != RecoveryVerdictReplicaUnobserved {
				t.Fatalf("Verdict = %q, want %q - missing evidence must never be reported as a confirmed outage", got[0].Verdict, RecoveryVerdictReplicaUnobserved)
			}
			if got[0].ReplicaSync == nil {
				t.Error("ReplicaSync = nil, want the evidence that produced the verdict")
			}
		})
	}
}

// TestComputeOwnedResourceImpacts_PositiveBadnessIsStillOutOfSync
// guards the other direction: a real role with a real, non-complete
// status is a positive statement of badness and must NOT be softened
// into "unobserved".
func TestComputeOwnedResourceImpacts_PositiveBadnessIsStillOutOfSync(t *testing.T) {
	resources := []OwnedResourcePlacement{{ID: "vm-1", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM}}
	got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{replicaObservation("secondary", "degraded")}, "target")
	if got[0].Verdict != RecoveryVerdictReplicaOutOfSync {
		t.Errorf("Verdict = %q, want %q - a real role reporting degraded is confirmed badness", got[0].Verdict, RecoveryVerdictReplicaOutOfSync)
	}
}

// TestComputeOwnedResourceImpacts_EvidenceIsKeyedByKindAndID guards the
// cross-contamination hole: vm-1 and jail-1 are different HAST
// resources, so neither may be shown the other's evidence.
func TestComputeOwnedResourceImpacts_EvidenceIsKeyedByKindAndID(t *testing.T) {
	resources := []OwnedResourcePlacement{
		{ID: "1", Name: "web-01", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindVM},
		{ID: "1", Name: "app-01", NodeID: "target", ReplicaNodeID: "replica-node", Kind: ResourceKindJail},
	}
	vmObservation := ReplicaSyncObservation{
		ResourceID: "1", Kind: ResourceKindVM, ReplicaNodeID: "replica-node",
		Observed: true, Role: "secondary", ResourceStatus: "complete",
	}
	got := ComputeOwnedResourceImpacts(resources, []ReplicaSyncObservation{vmObservation}, "target")
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Kind != ResourceKindVM || got[0].Verdict != RecoveryVerdictReplicaInSync {
		t.Errorf("VM impact = %+v, want the VM's own in-sync evidence", got[0])
	}
	if got[1].Kind != ResourceKindJail || got[1].Verdict != RecoveryVerdictUnverifiedReplica {
		t.Errorf("jail impact = %+v, want unverified_replica - it must not borrow the VM's observation", got[1])
	}
}
