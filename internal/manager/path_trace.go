package manager

import (
	"context"
	"fmt"
	"strings"
	"sync"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/pathtrace"
)

// TraceCellPath implements ADR-0058's read-only Cell Path Trace. VM and
// network intent comes from leader-only raft reads. Physical evidence
// comes directly from the Cell's owner Hive and is never silently
// substituted with the leader's local state.
func (s *Server) TraceCellPath(ctx context.Context, req *rpcpb.TraceCellPathRequest) (*rpcpb.TraceCellPathResponse, error) {
	cellID := strings.TrimSpace(req.GetCellId())
	if cellID == "" {
		return &rpcpb.TraceCellPathResponse{Error: "cell_id is required"}, nil
	}
	if strings.TrimSpace(req.GetDestination()) == "" {
		return &rpcpb.TraceCellPathResponse{Error: "destination is required"}, nil
	}

	vmsResp, err := s.raft.ListVMs(ctx)
	if err != nil {
		return &rpcpb.TraceCellPathResponse{Error: err.Error()}, nil
	}
	if vmsResp.GetError() != "" {
		if s.peers != nil && vmsResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.TraceCellPath(ctx, s.peerManagerdAddr(vmsResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.TraceCellPathResponse{Error: vmsResp.GetError(), LeaderHint: vmsResp.GetLeaderHint()}, nil
	}

	var cell *internalpb.VMDefinition
	for _, vm := range vmsResp.GetVms() {
		if vm.GetId() == cellID {
			cell = vm
			break
		}
	}
	if cell == nil {
		return &rpcpb.TraceCellPathResponse{Error: fmt.Sprintf("cell_id %q is not recognized", cellID)}, nil
	}

	var network *internalpb.NetworkDefinition
	if cell.GetNetworkId() != "" {
		networksResp, err := s.raft.ListNetworks(ctx)
		if err != nil {
			return &rpcpb.TraceCellPathResponse{Error: err.Error()}, nil
		}
		if networksResp.GetError() != "" {
			if s.peers != nil && networksResp.GetLeaderHint() != "" {
				if fwd, ferr := s.peers.TraceCellPath(ctx, s.peerManagerdAddr(networksResp.GetLeaderHint()), req); ferr == nil {
					return fwd, nil
				}
			}
			return &rpcpb.TraceCellPathResponse{Error: networksResp.GetError(), LeaderHint: networksResp.GetLeaderHint()}, nil
		}
		for _, candidate := range networksResp.GetNetworks() {
			if candidate.GetId() == cell.GetNetworkId() {
				network = candidate
				break
			}
		}
	}

	evidence := pathtrace.Evidence{}
	if network != nil && cell.GetNodeId() != "" {
		evidence = s.cellPathEvidence(ctx, cell.GetNodeId(), network)
	}
	input := pathtrace.Request{
		Cell:        toPathTraceCell(cell),
		Network:     toPathTraceNetwork(network),
		Destination: req.GetDestination(),
		Protocol:    req.GetProtocol(),
		Port:        req.GetPort(),
		Evidence:    evidence,
	}
	trace, err := pathtrace.Compute(input)
	if err != nil {
		return &rpcpb.TraceCellPathResponse{Error: err.Error(), Cell: fromInternalVM(cell), Network: fromInternalNetwork(network)}, nil
	}

	steps := make([]*rpcpb.PathTraceStep, 0, len(trace.Steps))
	for _, step := range trace.Steps {
		steps = append(steps, &rpcpb.PathTraceStep{
			Stage: step.Stage, Status: toRPCPathTraceStatus(step.Status),
			Summary: step.Summary, Evidence: step.Evidence,
			Explanation: step.Explanation,
		})
	}
	return &rpcpb.TraceCellPathResponse{
		Cell: fromInternalVM(cell), Network: fromInternalNetwork(network),
		Status: toRPCPathTraceStatus(trace.Status), Summary: trace.Summary,
		Steps: steps, NonAtomic: trace.NonAtomic, ActiveProbe: trace.ActiveProbe,
	}, nil
}

func (s *Server) cellPathEvidence(ctx context.Context, ownerID string, network *internalpb.NetworkDefinition) pathtrace.Evidence {
	evidence := pathtrace.Evidence{
		BridgeDetail: fmt.Sprintf("No bridge observation was available from owner Hive %s.", ownerID),
		PFDetail:     fmt.Sprintf("No packet-filter observation was available from owner Hive %s.", ownerID),
		NATStatus:    pathtrace.StatusUnknown,
		NATDetail:    fmt.Sprintf("No NAT-uplink observation was available from owner Hive %s.", ownerID),
	}

	var (
		bridgeResp     *rpcpb.GetLocalNetworkBridgeStatusResponse
		bridgeErr      error
		statsResp      *rpcpb.HostStatsResponse
		statsErr       error
		assumptionResp *rpcpb.ListAssumptionResultsResponse
		assumptionErr  error
	)
	localOwner := ownerID == s.nodeID
	address := ""
	if status, err := s.raft.Status(ctx); err == nil {
		localOwner = ownerID == status.GetNodeId()
		if !localOwner {
			for _, server := range status.GetServers() {
				if server.GetId() == ownerID {
					address = s.peerManagerdAddr(server.GetAddress())
					break
				}
			}
		}
	}
	if localOwner {
		bridgeResp, bridgeErr = s.GetLocalNetworkBridgeStatus(ctx, &rpcpb.GetLocalNetworkBridgeStatusRequest{NetworkId: network.GetId()})
		statsResp, statsErr = s.HostStats(ctx, &rpcpb.HostStatsRequest{})
		assumptionResp, assumptionErr = s.ListAssumptionResults(ctx, &rpcpb.ListAssumptionResultsRequest{})
	} else {
		if address == "" || s.peers == nil {
			return evidence
		}

		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			checkCtx, cancel := context.WithTimeout(ctx, reachabilityCheckTimeout)
			defer cancel()
			bridgeResp, bridgeErr = s.peers.GetLocalNetworkBridgeStatus(checkCtx, address, network.GetId())
		}()
		go func() {
			defer wg.Done()
			checkCtx, cancel := context.WithTimeout(ctx, reachabilityCheckTimeout)
			defer cancel()
			statsResp, statsErr = s.peers.HostStats(checkCtx, address)
		}()
		go func() {
			defer wg.Done()
			checkCtx, cancel := context.WithTimeout(ctx, reachabilityCheckTimeout)
			defer cancel()
			assumptionResp, assumptionErr = s.peers.ListAssumptionResults(checkCtx, address, &rpcpb.ListAssumptionResultsRequest{})
		}()
		wg.Wait()
	}

	switch {
	case bridgeErr != nil:
		evidence.BridgeDetail = "Bridge status query failed: " + bridgeErr.Error()
	case bridgeResp != nil:
		if bridgeResp.GetError() != "" {
			evidence.BridgeDetail = bridgeResp.GetError()
		} else {
			evidence.BridgeStatus = bridgeResp.GetBridgeStatus()
			evidence.BridgeDetail = fmt.Sprintf("Owner Hive %s reported %s as %s.", ownerID, resolveBridgeName(network), emptyPathValue(bridgeResp.GetBridgeStatus(), "unknown"))
		}
	}

	switch {
	case statsErr != nil:
		evidence.PFDetail = "HostStats query failed: " + statsErr.Error()
	case statsResp == nil:
	case hasHostStatsError(statsResp, "pf:"):
		evidence.PFDetail, _ = hostStatsError(statsResp, "pf:")
	default:
		evidence.PFObserved = true
		evidence.PFEnabled = statsResp.GetPf().GetEnabled()
		state := "disabled"
		if evidence.PFEnabled {
			state = "enabled"
		}
		evidence.PFDetail = fmt.Sprintf("Owner Hive %s reported PF %s.", ownerID, state)
	}

	if assumptionErr != nil {
		evidence.NATDetail = "Assumption query failed: " + assumptionErr.Error()
	} else {
		evidence.NATStatus, evidence.NATDetail = pathTraceNATEvidence(ownerID, assumptionResp)
	}
	return evidence
}

func hasHostStatsError(resp *rpcpb.HostStatsResponse, prefix string) bool {
	_, ok := hostStatsError(resp, prefix)
	return ok
}

func hostStatsError(resp *rpcpb.HostStatsResponse, prefix string) (string, bool) {
	for _, item := range resp.GetErrors() {
		if strings.HasPrefix(item, prefix) {
			return item, true
		}
	}
	return "", false
}

func pathTraceNATEvidence(ownerID string, resp *rpcpb.ListAssumptionResultsResponse) (pathtrace.Status, string) {
	if resp == nil {
		return pathtrace.StatusUnknown, fmt.Sprintf("Owner Hive %s did not return assumption evidence.", ownerID)
	}
	if resp.GetError() != "" {
		return pathtrace.StatusUnknown, resp.GetError()
	}
	if resp.GetStorageDegraded() {
		return pathtrace.StatusUnknown, "The owner Hive's assumption store is degraded: " + resp.GetStorageDegradedDetail()
	}
	var selected *rpcpb.AssumptionResult
	ambiguous := false
	for _, result := range resp.GetLatest() {
		if result.GetKey().GetKind() != rpcpb.AssumptionKind_ASSUMPTION_KIND_NAT_UPLINK_DEFAULT_ROUTE {
			continue
		}
		observedBy := result.GetKey().GetObservedByNodeId()
		if observedBy != "" && observedBy != ownerID {
			continue
		}
		switch {
		case selected == nil || result.GetLastObservedAtUnix() > selected.GetLastObservedAtUnix():
			selected = result
			ambiguous = false
		case result.GetLastObservedAtUnix() == selected.GetLastObservedAtUnix() &&
			(result.GetStatus() != selected.GetStatus() || result.GetKey().GetDependencyId() != selected.GetKey().GetDependencyId()):
			ambiguous = true
		}
	}
	if selected == nil {
		return pathtrace.StatusUnknown, fmt.Sprintf("Owner Hive %s has no NAT-uplink/default-route observation.", ownerID)
	}
	if ambiguous {
		return pathtrace.StatusUnknown, fmt.Sprintf("Owner Hive %s returned conflicting NAT observations with the same timestamp.", ownerID)
	}
	detail := selected.GetDetail()
	if detail == "" {
		detail = fmt.Sprintf("Owner Hive %s reported a NAT-uplink/default-route observation.", ownerID)
	}
	if selected.GetStale() {
		detail = "The stored observation is stale. " + detail
	}
	switch selected.GetStatus() {
	case rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE:
		return pathtrace.StatusClear, detail
	case rpcpb.AssumptionStatus_ASSUMPTION_STATUS_FALSE:
		return pathtrace.StatusBlocked, detail
	case rpcpb.AssumptionStatus_ASSUMPTION_STATUS_NOT_APPLICABLE:
		return pathtrace.StatusNotApplicable, detail
	default:
		return pathtrace.StatusUnknown, detail
	}
}

func toPathTraceCell(vm *internalpb.VMDefinition) pathtrace.Cell {
	rules := make([]pathtrace.Rule, 0, len(vm.GetFirewallRules()))
	for _, rule := range vm.GetFirewallRules() {
		rules = append(rules, pathtrace.Rule{
			Direction: rule.GetDirection(), Action: rule.GetAction(),
			Protocol: rule.GetProtocol(), PortRange: rule.GetPortRange(),
		})
	}
	return pathtrace.Cell{
		ID: vm.GetId(), Name: vm.GetName(), NodeID: vm.GetNodeId(),
		DesiredState: pathTraceVMState(vm.GetDesiredState()),
		Phase:        pathTraceVMPhase(vm.GetPhase()), PhaseError: vm.GetPhaseError(),
		NetworkID: vm.GetNetworkId(), IPAddress: vm.GetIpAddress(),
		MACAddress: vm.GetMacAddress(), FirewallRules: rules,
		FirewallPaused: vm.GetFirewallPaused(),
	}
}

func pathTraceVMState(state internalpb.VMState) string {
	switch state {
	case internalpb.VMState_VM_STATE_RUNNING:
		return "running"
	case internalpb.VMState_VM_STATE_STOPPED:
		return "stopped"
	case internalpb.VMState_VM_STATE_DELETING:
		return "deleting"
	default:
		return ""
	}
}

func toPathTraceNetwork(network *internalpb.NetworkDefinition) *pathtrace.Network {
	if network == nil {
		return nil
	}
	return &pathtrace.Network{
		ID: network.GetId(), Name: network.GetName(), VLANID: network.GetVlanId(), Subnet: network.GetSubnet(),
		ExternalGateway: network.GetExternalGateway(),
	}
}

func pathTraceVMPhase(phase internalpb.VMPhase) string {
	switch phase {
	case internalpb.VMPhase_VM_PHASE_READY:
		return "ready"
	case internalpb.VMPhase_VM_PHASE_CREATING:
		return "creating"
	case internalpb.VMPhase_VM_PHASE_DELETING:
		return "deleting"
	case internalpb.VMPhase_VM_PHASE_ERROR:
		return "error"
	default:
		return ""
	}
}

func toRPCPathTraceStatus(status pathtrace.Status) rpcpb.PathTraceStatus {
	switch status {
	case pathtrace.StatusClear:
		return rpcpb.PathTraceStatus_PATH_TRACE_STATUS_CLEAR
	case pathtrace.StatusBlocked:
		return rpcpb.PathTraceStatus_PATH_TRACE_STATUS_BLOCKED
	case pathtrace.StatusUnknown:
		return rpcpb.PathTraceStatus_PATH_TRACE_STATUS_UNKNOWN
	case pathtrace.StatusNotApplicable:
		return rpcpb.PathTraceStatus_PATH_TRACE_STATUS_NOT_APPLICABLE
	default:
		return rpcpb.PathTraceStatus_PATH_TRACE_STATUS_UNSPECIFIED
	}
}

func emptyPathValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
