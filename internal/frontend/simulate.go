package frontend

import (
	"net/http"
	"sort"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// quorumImpactView/resourceImpactView/replicaBackedImpactView are the
// template-facing shapes for SimulateNodeFailureResponse's three
// sections (ADR-0052) - translated once here, the same convention
// convert.go already follows for vmView/jailView, rather than exposing
// raw proto getters to templates.
type quorumImpactView struct {
	TargetIsVoter   bool
	TotalVoters     uint32
	RemainingVoters uint32
	Reachable       uint32
	Unknown         uint32
	QuorumSize      uint32
	Survives        bool
	Note            string
}

type resourceImpactView struct {
	ID            string
	Name          string
	Kind          string // "vm" or "jail"
	ReplicaNodeID string
	Verdict       string // "unprotected" or "unverified_replica"
	Explanation   string
}

type replicaBackedImpactView struct {
	ID          string
	Name        string
	Kind        string
	OwnerNodeID string
	Explanation string
}

type networkFailureImpactView struct {
	ID          string
	Name        string
	NodeID      string
	Explanation string
}

func fromRPCResourceKind(k rpcpb.ResourceKind) string {
	if k == rpcpb.ResourceKind_RESOURCE_KIND_JAIL {
		return "jail"
	}
	return "vm"
}

func fromRPCRecoveryVerdict(v rpcpb.RecoveryVerdict) string {
	if v == rpcpb.RecoveryVerdict_RECOVERY_VERDICT_UNVERIFIED_REPLICA {
		return "unverified_replica"
	}
	return "unprotected"
}

func fromRPCQuorumImpact(q *rpcpb.QuorumImpact) quorumImpactView {
	return quorumImpactView{
		TargetIsVoter:   q.GetTargetIsVoter(),
		TotalVoters:     q.GetTotalVoters(),
		RemainingVoters: q.GetRemainingVoters(),
		Reachable:       q.GetRemainingReachableVoters(),
		Unknown:         q.GetRemainingUnknownVoters(),
		QuorumSize:      q.GetQuorumSize(),
		Survives:        q.GetSurvives(),
		Note:            q.GetNote(),
	}
}

func fromRPCOwnedResourceImpact(i *rpcpb.OwnedResourceImpact) resourceImpactView {
	return resourceImpactView{
		ID:            i.GetId(),
		Name:          i.GetName(),
		Kind:          fromRPCResourceKind(i.GetKind()),
		ReplicaNodeID: i.GetReplicaNodeId(),
		Verdict:       fromRPCRecoveryVerdict(i.GetVerdict()),
		Explanation:   i.GetExplanation(),
	}
}

func fromRPCReplicaBackedImpact(i *rpcpb.ReplicaBackedImpact) replicaBackedImpactView {
	return replicaBackedImpactView{
		ID:          i.GetId(),
		Name:        i.GetName(),
		Kind:        fromRPCResourceKind(i.GetKind()),
		OwnerNodeID: i.GetOwnerNodeId(),
		Explanation: i.GetExplanation(),
	}
}

// simulateNodeChoices returns the union of known raft membership and
// every VM/jail's node_id/replica_node_id, deduped and sorted - a node
// can remain a valid, meaningful simulation target after being removed
// from raft entirely (ADR-0025's reassignment-reclaim territory), so
// the picker must not be limited to current raft membership alone.
// Best-effort: a failed ListVMs/ListJails just means a smaller list,
// not a page error, since the picker is a convenience, not the report
// itself.
func (s *Server) simulateNodeChoices(r *http.Request) []string {
	seen := make(map[string]struct{})

	if statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{}); err == nil {
		for _, id := range statusResp.GetKnownNodeIds() {
			seen[id] = struct{}{}
		}
	}
	if vms, errMsg := s.currentVMs(r, "id", "asc"); errMsg == "" {
		for _, vm := range vms {
			if vm.NodeID != "" {
				seen[vm.NodeID] = struct{}{}
			}
			if vm.ReplicaNodeID != "" {
				seen[vm.ReplicaNodeID] = struct{}{}
			}
		}
	}
	if jails, errMsg := s.currentJails(r); errMsg == "" {
		for _, jail := range jails {
			if jail.NodeID != "" {
				seen[jail.NodeID] = struct{}{}
			}
			if jail.ReplicaNodeID != "" {
				seen[jail.ReplicaNodeID] = struct{}{}
			}
		}
	}

	nodes := make([]string, 0, len(seen))
	for id := range seen {
		nodes = append(nodes, id)
	}
	sort.Strings(nodes)
	return nodes
}

// handleSimulatePage serves the Dependency Graph Simulator page
// ("/simulate", ADR-0052): a dropdown of every known Hive - including
// one that's only ever appeared as a Cell's placement, not currently a
// raft member - and, once one is chosen via ?node_id=, a quorum +
// per-Cell recovery report. A plain GET with a query param, not a POST
// form: this is a read-only, idempotent, bookmarkable query, unlike
// every mutating form elsewhere in this package.
func (s *Server) handleSimulatePage(w http.ResponseWriter, r *http.Request) {
	nodes := s.simulateNodeChoices(r)
	networks, _ := s.currentNetworks(r)

	nodeID := r.URL.Query().Get("node_id")
	var (
		quorum        quorumImpactView
		owned         []resourceImpactView
		replicaBacked []replicaBackedImpactView
		simErr        string
	)
	if nodeID != "" {
		resp, err := s.client.SimulateNodeFailure(r.Context(), &rpcpb.SimulateNodeFailureRequest{NodeId: nodeID})
		switch {
		case err != nil:
			simErr = err.Error()
		case resp.GetError() != "":
			simErr = resp.GetError()
		default:
			quorum = fromRPCQuorumImpact(resp.GetQuorum())
			for _, res := range resp.GetOwnedResources() {
				owned = append(owned, fromRPCOwnedResourceImpact(res))
			}
			for _, res := range resp.GetReplicaBackedResources() {
				replicaBacked = append(replicaBacked, fromRPCReplicaBackedImpact(res))
			}
		}
	}

	networkID := r.URL.Query().Get("network_id")
	var (
		network        networkView
		networkImpacts []networkFailureImpactView
		networkNote    string
	)
	if networkID != "" {
		resp, err := s.client.SimulateNetworkFailure(r.Context(), &rpcpb.SimulateNetworkFailureRequest{NetworkId: networkID})
		switch {
		case err != nil:
			simErr = err.Error()
		case resp.GetError() != "":
			simErr = resp.GetError()
		default:
			network = fromRPCNetwork(resp.GetNetwork())
			networkNote = resp.GetNote()
			for _, impact := range resp.GetAffectedResources() {
				networkImpacts = append(networkImpacts, networkFailureImpactView{
					ID: impact.GetId(), Name: impact.GetName(), NodeID: impact.GetNodeId(), Explanation: impact.GetExplanation(),
				})
			}
		}
	}

	s.render(w, "simulate_page", s.withAuthFields(r, pageData{
		SimulateNodes:           nodes,
		SimulateTargetNodeID:    nodeID,
		SimulateError:           simErr,
		SimulateQuorum:          quorum,
		SimulateOwnedResources:  owned,
		SimulateReplicaBacked:   replicaBacked,
		SimulateNetworks:        networks,
		SimulateTargetNetworkID: networkID,
		SimulateNetwork:         network,
		SimulateNetworkImpacts:  networkImpacts,
		SimulateNetworkNote:     networkNote,
		ActivePage:              "simulate",
	}))
}
