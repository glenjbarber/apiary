package manager

import (
	"context"
	"fmt"
	"os"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// bytesPerMB converts HostStats' byte counts into the MB unit VM memory
// is recorded in.
const bytesPerMB = 1 << 20

// vmConfigPlacementCheck is the host-capacity half of SetVMConfig (issue
// #111). It runs before the change is committed to raft, because the FSM
// cannot see a node's physical CPU or memory - that is observed state on
// the owning node, not replicated state, and the FSM must stay
// deterministic across replicas.
//
// Only growth is checked. Lowering vcpus or memory_mb can never exceed a
// node's capacity, and refusing a reduction would trap a VM that was
// already overcommitted before this check existed.
//
// It returns a non-empty leaderHint when this manager is not the leader:
// ListVMs is leader-only, so the caller forwards the whole request rather
// than checking against a stale local view. An error means the change must
// not be submitted.
func (s *Server) vmConfigPlacementCheck(ctx context.Context, req *rpcpb.SetVMConfigRequest) (cur *internalpb.VMDefinition, leaderHint string, err error) {
	list, lerr := s.raft.ListVMs(ctx)
	if lerr != nil {
		return nil, "", fmt.Errorf("reading VM placement: %w", lerr)
	}
	if list.GetError() != "" {
		return nil, list.GetLeaderHint(), fmt.Errorf("reading VM placement: %s", list.GetError())
	}

	for _, vm := range list.GetVms() {
		if vm.GetId() == req.GetId() {
			cur = vm
			break
		}
	}
	// A VM that does not exist, or has no owner node, has no host whose
	// capacity could be exceeded. The FSM still refuses a missing VM with
	// its own authoritative error.
	if cur == nil || cur.GetNodeId() == "" {
		return cur, "", nil
	}
	if req.GetVcpus() <= cur.GetVcpus() && req.GetMemoryMb() <= cur.GetMemoryMb() {
		return cur, "", nil
	}

	host, herr := s.vmNodeHostStats(ctx, cur.GetNodeId())
	if herr != nil {
		// Fail closed: an unverifiable capacity check is a refusal, not
		// a silent pass.
		return cur, "", fmt.Errorf("cannot verify host capacity on node %s, so the change was not saved: %w", cur.GetNodeId(), herr)
	}
	return cur, "", checkVMConfigCapacity(cur, list.GetVms(), req.GetVcpus(), req.GetMemoryMb(), host)
}

// vmNodeHostStats fetches nodeID's own HostStats, answering locally when
// nodeID is this manager's node and otherwise asking the peer at that
// node's raft address. Peer calls are bounded by reachabilityCheckTimeout.
func (s *Server) vmNodeHostStats(ctx context.Context, nodeID string) (*rpcpb.HostStatsResponse, error) {
	if nodeID == s.nodeID {
		return s.HostStats(ctx, &rpcpb.HostStatsRequest{})
	}
	if s.peers == nil {
		return nil, fmt.Errorf("node %s is remote and this manager has no peer client", nodeID)
	}
	status, err := s.raft.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("looking up node %s: %w", nodeID, err)
	}
	for _, server := range status.GetServers() {
		if server.GetId() != nodeID {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, reachabilityCheckTimeout)
		defer cancel()
		resp, err := s.peers.HostStats(callCtx, s.peerManagerdAddr(server.GetAddress()))
		if err != nil {
			return nil, fmt.Errorf("reading host stats from node %s: %w", nodeID, err)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("node %s is not a member of this Colony", nodeID)
}

// checkVMConfigCapacity decides whether cur, on its owner node, can grow to
// vcpus/memoryMB given the host's reported capacity and the other VMs
// already assigned to that node.
//
// Two rules, both deliberately simple:
//   - vcpus may not exceed the node's logical CPU count. vCPU overcommit
//     across VMs is allowed, as bhyve allows it, so this is a per-VM ceiling
//     rather than a sum.
//   - memory_mb must fit in the node's physical memory alongside every other
//     VM assigned to that node, stopped ones included: a stopped VM must
//     still be able to start. Desired-deleting VMs are excluded because they
//     are being torn down.
//
// Apiary has no separate scheduling or reservation policy to apply here;
// node_id is set directly by the caller, so these are the only placement
// facts there are.
func checkVMConfigCapacity(cur *internalpb.VMDefinition, all []*internalpb.VMDefinition, vcpus uint32, memoryMB uint64, host *rpcpb.HostStatsResponse) error {
	node := cur.GetNodeId()

	if vcpus > cur.GetVcpus() {
		cores := host.GetCpu().GetCores()
		if cores <= 0 {
			return fmt.Errorf("node %s did not report its CPU count, so vcpus %d cannot be verified", node, vcpus)
		}
		if int64(vcpus) > int64(cores) {
			return fmt.Errorf("vcpus %d exceeds the %d logical CPUs on node %s; choose at most %d", vcpus, cores, node, cores)
		}
	}

	if memoryMB > cur.GetMemoryMb() {
		totalMB := host.GetMem().GetTotalBytes() / bytesPerMB
		if totalMB == 0 {
			return fmt.Errorf("node %s did not report its physical memory, so memory_mb %d cannot be verified", node, memoryMB)
		}
		var committed uint64
		for _, other := range all {
			if other.GetId() == cur.GetId() || other.GetNodeId() != node || other.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING {
				continue
			}
			committed += other.GetMemoryMb()
		}
		if committed >= totalMB || memoryMB > totalMB-committed {
			available := uint64(0)
			if totalMB > committed {
				available = totalMB - committed
			}
			return fmt.Errorf("memory_mb %d does not fit on node %s: it has %d MB of physical memory and %d MB is already committed to its other VMs, so at most %d MB is available to this VM", memoryMB, node, totalMB, committed, available)
		}
	}
	return nil
}

// SetVMConfig implements rpcpb.ManagerServiceServer. See SetVMFirewallRules
// for the narrow-command rationale (ADR-0049). The host-capacity check runs
// first; on a follower the whole request is forwarded to the leader, which
// checks and then commits.
//
// Audit: the change is recorded as one line on this manager's stderr with
// the caller's API key id and the before and after values. Raft's own log
// records the committed command but not its caller.
func (s *Server) SetVMConfig(ctx context.Context, req *rpcpb.SetVMConfigRequest) (*rpcpb.SetVMConfigResponse, error) {
	cur, leaderHint, cerr := s.vmConfigPlacementCheck(ctx, req)
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.SetVMConfigResponse
		var ferr error
		if fwd, ferr = s.peers.SetVMConfig(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
		return &rpcpb.SetVMConfigResponse{Error: augmentForwardError("", leaderHint, ferr), LeaderHint: leaderHint}, nil
	}
	if cerr != nil {
		return &rpcpb.SetVMConfigResponse{Error: cerr.Error(), LeaderHint: leaderHint}, nil
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmConfig{SetVmConfig: &internalpb.SetVMConfig{
			Id: req.GetId(), Vcpus: req.GetVcpus(), MemoryMb: req.GetMemoryMb(),
		}},
	}
	vm, appErr, hint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	if appErr == "" && cur != nil {
		fmt.Fprintf(os.Stderr, "apiary: audit: SetVMConfig vm=%s key=%s vcpus %d->%d memory_mb %d->%d (applies on next launch)\n",
			req.GetId(), callerAPIKeyID(ctx), cur.GetVcpus(), req.GetVcpus(), cur.GetMemoryMb(), req.GetMemoryMb())
	}
	return &rpcpb.SetVMConfigResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: hint}, nil
}
