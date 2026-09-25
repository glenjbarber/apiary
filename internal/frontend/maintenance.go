package frontend

import (
	"net/http"
	"sort"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

type maintenanceWaveView struct {
	Wave             int
	NodeID           string
	Verdict          string
	Explanation      string
	TotalVoters      uint32
	QuorumSize       uint32
	ReachableVoters  uint32
	UnknownVoters    uint32
	OwnedResources   []resourceImpactView
	UnprotectedCount int
	ReplicatedCount  int
	Priority         int
}

// handleMaintenancePage builds a read-only, sequential maintenance rehearsal
// from fresh node-failure simulations. It deliberately does not issue restart,
// migration, or membership actions and never groups multiple Combs into a wave.
func (s *Server) handleMaintenancePage(w http.ResponseWriter, r *http.Request) {
	nodes := s.simulateNodeChoices(r)
	waves := make([]maintenanceWaveView, 0, len(nodes))
	for _, nodeID := range nodes {
		wave := maintenanceWaveView{NodeID: nodeID, Verdict: "unknown", Explanation: "The simulation did not provide enough evidence to determine whether quorum is preserved.", Priority: 2}
		resp, err := s.client.SimulateNodeFailure(r.Context(), &rpcpb.SimulateNodeFailureRequest{NodeId: nodeID})
		switch {
		case err != nil:
			wave.Explanation = "Failure simulation unavailable: " + err.Error()
		case resp == nil:
			wave.Explanation = "Failure simulation returned no response."
		case resp.GetError() != "":
			wave.Explanation = "Failure simulation unavailable: " + resp.GetError()
		case resp.GetQuorum() == nil || resp.GetQuorum().GetTotalVoters() == 0 || resp.GetQuorum().GetQuorumSize() == 0:
			wave.Explanation = "Raft voter/quorum evidence is empty or unknown."
		default:
			q := resp.GetQuorum()
			wave.TotalVoters = q.GetTotalVoters()
			wave.QuorumSize = q.GetQuorumSize()
			wave.ReachableVoters = q.GetRemainingReachableVoters()
			wave.UnknownVoters = q.GetRemainingUnknownVoters()
			for _, resource := range resp.GetOwnedResources() {
				view := fromRPCOwnedResourceImpact(resource)
				wave.OwnedResources = append(wave.OwnedResources, view)
				if view.ReplicaNodeID == "" {
					wave.UnprotectedCount++
				} else {
					wave.ReplicatedCount++
				}
			}
			wave.Verdict, wave.Explanation, wave.Priority = classifyMaintenanceQuorum(wave.ReachableVoters, wave.UnknownVoters, wave.QuorumSize)
		}
		waves = append(waves, wave)
	}
	sort.SliceStable(waves, func(i, j int) bool {
		if waves[i].Priority != waves[j].Priority {
			return waves[i].Priority < waves[j].Priority
		}
		return waves[i].NodeID < waves[j].NodeID
	})
	for i := range waves {
		waves[i].Wave = i + 1
	}
	s.render(w, "maintenance_page", s.withAuthFields(r, pageData{MaintenanceWaves: waves, ActivePage: "maintenance"}))
}

func classifyMaintenanceQuorum(reachable, unknown, majority uint32) (verdict, explanation string, priority int) {
	switch {
	case majority == 0:
		return "unknown", "Raft majority evidence is unavailable.", 2
	case reachable >= majority:
		return "quorum-preserved", "The confirmed-reachable voters remaining after this one-Comb outage meet the Raft majority. Workload impact still needs review below.", 0
	case unknown >= majority-reachable:
		return "unknown", "A majority is possible only if one or more unknown voters are reachable. Confirm fresh evidence before maintenance.", 1
	default:
		return "blocked", "The confirmed-reachable remaining voters do not meet the Raft majority.", 3
	}
}
