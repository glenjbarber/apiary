package frontend

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/invariant"
)

// invariantEvidenceView is the template-facing shape for one
// invariant.Evidence entry.
type invariantEvidenceView struct {
	Source        string
	Detail        string
	ObservedAt    string // empty means never runtime-observed (a structural guarantee)
	NeverObserved bool
}

// invariantEvaluationView is the template-facing shape for one
// invariant.Evaluation.
type invariantEvaluationView struct {
	Name        string
	Scope       string
	Result      string
	Explanation string
	Evidence    []invariantEvidenceView
}

func fromInvariantEvaluation(e invariant.Evaluation) invariantEvaluationView {
	v := invariantEvaluationView{Name: e.Name, Scope: e.Scope, Result: string(e.Result), Explanation: e.Explanation}
	for _, ev := range e.Evidence {
		iv := invariantEvidenceView{Source: ev.Source, Detail: ev.Detail}
		if ev.ObservedAt.IsZero() {
			iv.NeverObserved = true
		} else {
			iv.ObservedAt = ev.ObservedAt.Format("2006-01-02 15:04:05 MST")
		}
		v.Evidence = append(v.Evidence, iv)
	}
	return v
}

// invariantVoters returns the current raft membership's actual voters
// (Suffrage == "Voter" only - never KnownNodeIds, which mixes in
// VM/jail node_ids unrelated to raft voter status) and the local node
// ID, from the same anchor Status() call the caller already fetched.
func invariantVoters(statusResp *rpcpb.StatusResponse) []string {
	var voters []string
	for _, m := range statusResp.GetMembers() {
		if m.GetSuffrage() == "Voter" {
			voters = append(voters, m.GetNodeId())
		}
	}
	return voters
}

// gatherQuorumTolerance fetches each current voter's real reachability
// (a single bounded, concurrent HostStats fan-out - never a per-voter
// SimulateNodeFailure call, which would cost O(voters^2) reachability
// RPCs concentrated on the leader) and classifies every voter's own
// hypothetical loss locally from that one shared snapshot.
func (s *Server) gatherQuorumTolerance(ctx context.Context, statusResp *rpcpb.StatusResponse, localNodeID string) invariant.Evaluation {
	voterIDs := invariantVoters(statusResp)
	if len(voterIDs) > nodeContextLimit {
		voterIDs = voterIDs[:nodeContextLimit]
	}

	voters := make([]invariant.VoterReachability, len(voterIDs))
	for i, id := range voterIDs {
		voters[i] = invariant.VoterReachability{NodeID: id, Reachability: invariant.ReachabilityReachable}
		if id == localNodeID {
			continue // trivially reachable, no fetch needed
		}
		voters[i].Reachability = invariant.ReachabilityUnknown
	}

	overallCtx, cancel := context.WithTimeout(ctx, nodeContextOverallTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for i, id := range voterIDs {
		if id == localNodeID {
			continue
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			checkCtx, checkCancel := context.WithTimeout(overallCtx, nodeContextTimeout)
			defer checkCancel()
			if s.peers == nil {
				voters[i].Reachability = invariant.ReachabilityUnknown
				return
			}
			if _, err := s.fetchHostStats(checkCtx, id, localNodeID); err != nil {
				voters[i].Reachability = invariant.ReachabilityUnreachable
			} else {
				voters[i].Reachability = invariant.ReachabilityReachable
			}
		}(i, id)
	}
	wg.Wait()

	return invariant.EvaluateQuorumTolerance(voters, statusResp.GetRaftLeaderId())
}

// gatherCellRecoverability enumerates every VM/jail with a configured
// replica and evaluates each. A currentVMs/currentJails fetch failure
// emits one cluster-scoped Unknown evaluation citing it - never a
// silent empty list (an empty list must only ever mean "no protected
// resources exist," never "the fetch failed").
func (s *Server) gatherCellRecoverability(ctx context.Context, localNodeID string, r *http.Request) []invariant.Evaluation {
	vms, vmErr := s.currentVMs(r, "id", "asc")
	jails, jailErr := s.currentJails(r)
	if vmErr != "" || jailErr != "" {
		reason := vmErr
		if reason == "" {
			reason = jailErr
		}
		return []invariant.Evaluation{{
			Name: "cell-recoverability", Scope: "cluster", Result: invariant.ResultUnknown,
			Explanation: "Cell recoverability could not be evaluated - the VM/jail list itself could not be fetched.",
			Evidence:    []invariant.Evidence{{Source: "ListVMs/ListJails", Detail: reason, ObservedAt: time.Now()}},
		}}
	}

	var facts []invariant.ResourceFact
	replicaNodes := map[string]struct{}{}
	for _, vm := range vms {
		if vm.ReplicaNodeID == "" {
			continue
		}
		facts = append(facts, invariant.ResourceFact{ID: vm.ID, Name: vm.Name, Kind: "vm", ReplicaNodeID: vm.ReplicaNodeID})
		replicaNodes[vm.ReplicaNodeID] = struct{}{}
	}
	for _, j := range jails {
		if j.ReplicaNodeID == "" {
			continue
		}
		facts = append(facts, invariant.ResourceFact{ID: j.ID, Name: j.Name, Kind: "jail", ReplicaNodeID: j.ReplicaNodeID})
	}

	// Bounded concurrent HostStats fan-out to each distinct VM
	// replica-target node only - jails get no fetch at all, since no
	// jail-capability signal exists anywhere to check.
	nodeIDs := make([]string, 0, len(replicaNodes))
	for id := range replicaNodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	if len(nodeIDs) > nodeContextLimit {
		nodeIDs = nodeIDs[:nodeContextLimit]
	}

	type capability struct {
		result invariant.Result
		detail string
	}

	capabilities := make(map[string]capability, len(nodeIDs))
	var mu sync.Mutex

	overallCtx, cancel := context.WithTimeout(ctx, nodeContextOverallTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, id := range nodeIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			checkCtx, checkCancel := context.WithTimeout(overallCtx, nodeContextTimeout)
			defer checkCancel()
			resp, err := s.fetchHostStats(checkCtx, id, localNodeID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				capabilities[id] = capability{result: invariant.ResultUnknown, detail: id + ": HostStats fetch failed - " + err.Error()}
			case resp.GetBhyveConfigured():
				capabilities[id] = capability{result: invariant.ResultTrue, detail: id + ": bhyve configured"}
			default:
				capabilities[id] = capability{result: invariant.ResultFalse, detail: id + ": bhyve NOT configured"}
			}
		}(id)
	}
	wg.Wait()

	for i, f := range facts {
		if f.Kind == "jail" {
			facts[i].DestinationCapable = invariant.ResultUnknown
			facts[i].DestinationCapableDetail = "no capability signal exists for jails in this codebase"
			continue
		}
		cap, ok := capabilities[f.ReplicaNodeID]
		if !ok {
			facts[i].DestinationCapable = invariant.ResultUnknown
			facts[i].DestinationCapableDetail = f.ReplicaNodeID + ": not checked (evidence limit reached)"
			continue
		}
		facts[i].DestinationCapable = cap.result
		facts[i].DestinationCapableDetail = cap.detail
	}

	return invariant.EvaluateCellRecoverability(facts)
}

// gatherNetworkRoute enumerates every managed network and, for each,
// fans GetLocalNetworkBridgeStatus out to the distinct nodes that own
// a VM attached to it (mirroring internal/assumecheck's own established
// pattern) - never ListNetworks's own bridge_status field, which would
// silently mislabel the leader's own bridge state as the answering
// network's (the ADR-0055 bug GetLocalNetworkBridgeStatus's own doc
// comment names).
func (s *Server) gatherNetworkRoute(ctx context.Context, localNodeID string, r *http.Request, vms []vmView, vmsErr string) []invariant.Evaluation {
	networks, netErr := s.currentNetworks(r)
	if netErr != "" {
		return []invariant.Evaluation{{
			Name: "network-route-dns", Scope: "cluster", Result: invariant.ResultUnknown,
			Explanation: "Network route/DNS could not be evaluated - the network list itself could not be fetched.",
			Evidence:    []invariant.Evidence{{Source: "ListNetworks", Detail: netErr, ObservedAt: time.Now()}},
		}}
	}

	ownersByNetwork := map[string]map[string]struct{}{}
	if vmsErr == "" {
		for _, vm := range vms {
			if vm.NetworkID == "" || vm.NodeID == "" {
				continue
			}
			if ownersByNetwork[vm.NetworkID] == nil {
				ownersByNetwork[vm.NetworkID] = map[string]struct{}{}
			}
			ownersByNetwork[vm.NetworkID][vm.NodeID] = struct{}{}
		}
	}

	type obsKey struct{ node, network string }
	type obsResult struct {
		status string
		errStr string
	}
	results := make(map[obsKey]obsResult)
	var mu sync.Mutex

	overallCtx, cancel := context.WithTimeout(ctx, nodeContextOverallTimeout)
	defer cancel()

	var wg sync.WaitGroup
	checked := 0
	for _, net := range networks {
		owners := make([]string, 0, len(ownersByNetwork[net.ID]))
		for id := range ownersByNetwork[net.ID] {
			owners = append(owners, id)
		}
		sort.Strings(owners)
		for _, nodeID := range owners {
			if checked >= nodeContextLimit {
				break
			}
			checked++
			wg.Add(1)
			go func(nodeID, networkID string) {
				defer wg.Done()
				checkCtx, checkCancel := context.WithTimeout(overallCtx, nodeContextTimeout)
				defer checkCancel()
				var resp *rpcpb.GetLocalNetworkBridgeStatusResponse
				var err error
				if nodeID == localNodeID || s.peers == nil {
					resp, err = s.client.GetLocalNetworkBridgeStatus(checkCtx, &rpcpb.GetLocalNetworkBridgeStatusRequest{NetworkId: networkID})
				} else {
					resp, err = s.peers.GetLocalNetworkBridgeStatus(checkCtx, s.peerAddr(nodeID), networkID)
				}
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					results[obsKey{nodeID, networkID}] = obsResult{errStr: err.Error()}
				case resp.GetError() != "":
					results[obsKey{nodeID, networkID}] = obsResult{errStr: resp.GetError()}
				default:
					results[obsKey{nodeID, networkID}] = obsResult{status: resp.GetBridgeStatus()}
				}
			}(nodeID, net.ID)
		}
	}
	wg.Wait()

	facts := make([]invariant.NetworkFact, 0, len(networks))
	for _, net := range networks {
		fact := invariant.NetworkFact{ID: net.ID, Name: net.Name}
		owners := make([]string, 0, len(ownersByNetwork[net.ID]))
		for id := range ownersByNetwork[net.ID] {
			owners = append(owners, id)
		}
		sort.Strings(owners)
		for _, nodeID := range owners {
			r, ok := results[obsKey{nodeID, net.ID}]
			if !ok {
				continue // beyond nodeContextLimit - not included, mirrors the fan-out cap elsewhere
			}
			fact.Observations = append(fact.Observations, invariant.BridgeObservation{NodeID: nodeID, Status: r.status, Err: r.errStr})
		}
		facts = append(facts, fact)
	}

	return invariant.EvaluateNetworkRoute(facts)
}

// handleInvariantsPage serves the Operational Invariants page
// ("/invariants", ADR-0060): a small, named catalog of safety rules,
// each continuously evaluated to true/false/unknown with cited
// evidence and freshness. No new RPC/proto - every fact comes from
// already-existing RPCs this project's other pages already call.
func (s *Server) handleInvariantsPage(w http.ResponseWriter, r *http.Request) {
	statusResp, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	localNodeID := statusResp.GetManagerNodeId()

	var live []invariantEvaluationView

	if statusErr != nil || !statusResp.GetRaftReachable() || statusResp.GetRaftError() != "" {
		reason := "raft status could not be confirmed"
		switch {
		case statusErr != nil:
			reason = statusErr.Error()
		case statusResp.GetRaftError() != "":
			reason = statusResp.GetRaftError()
		}
		live = append(live, fromInvariantEvaluation(invariant.Evaluation{
			Name: "quorum-tolerance", Scope: "cluster", Result: invariant.ResultUnknown,
			Explanation: "Quorum tolerance could not be evaluated.",
			Evidence:    []invariant.Evidence{{Source: "Status", Detail: reason, ObservedAt: time.Now()}},
		}))
	} else {
		live = append(live, fromInvariantEvaluation(s.gatherQuorumTolerance(r.Context(), statusResp, localNodeID)))
	}

	vms, vmsErr := s.currentVMs(r, "id", "asc")
	jails, jailsErr := s.currentJails(r)

	var resourceIDs []string
	if vmsErr == "" {
		for _, vm := range vms {
			if vm.ReplicaNodeID != "" {
				resourceIDs = append(resourceIDs, vm.ID)
			}
		}
	}
	if jailsErr == "" {
		for _, j := range jails {
			if j.ReplicaNodeID != "" {
				resourceIDs = append(resourceIDs, j.ID)
			}
		}
	}
	for _, e := range invariant.EvaluateHASTDualPrimary(resourceIDs) {
		live = append(live, fromInvariantEvaluation(e))
	}

	for _, e := range s.gatherCellRecoverability(r.Context(), localNodeID, r) {
		live = append(live, fromInvariantEvaluation(e))
	}

	for _, e := range s.gatherNetworkRoute(r.Context(), localNodeID, r, vms, vmsErr) {
		live = append(live, fromInvariantEvaluation(e))
	}

	structural := []invariantEvaluationView{fromInvariantEvaluation(invariant.EvaluateOwnershipGatedDeletion())}

	s.render(w, "invariants_page", s.withAuthFields(r, pageData{
		InvariantEvaluations: live,
		InvariantStructural:  structural,
		ActivePage:           "invariants",
	}))
}
