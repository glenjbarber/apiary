package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hoststats"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// vmConfigRaft backs a RaftClient with a real FSM, so the handler under
// test exercises the same apply path a managerd does: the command is
// marshalled, applied, and read back, and a rejected command leaves the
// FSM exactly as it was.
type vmConfigRaft struct {
	internalpb.RaftInternalClient
	fsm    *raftnode.FSM
	index  uint64
	nodeID string
}

func (c *vmConfigRaft) Apply(_ context.Context, req *internalpb.ApplyRequest, _ ...grpc.CallOption) (*internalpb.ApplyResponse, error) {
	c.index++
	res := c.fsm.Apply(&raft.Log{Index: c.index, Data: req.GetPayload()}).(*raftnode.FSMApplyResult)
	if res.Error != "" {
		return &internalpb.ApplyResponse{Error: res.Error}, nil
	}
	result, err := proto.Marshal(res.VM)
	if err != nil {
		return nil, err
	}
	return &internalpb.ApplyResponse{Result: result}, nil
}

func (c *vmConfigRaft) ListVMs(_ context.Context, _ *internalpb.ListVMsRequest, _ ...grpc.CallOption) (*internalpb.ListVMsResponse, error) {
	return &internalpb.ListVMsResponse{Vms: c.fsm.ListVMs()}, nil
}

func (c *vmConfigRaft) GetVM(_ context.Context, req *internalpb.GetVMRequest, _ ...grpc.CallOption) (*internalpb.GetVMResponse, error) {
	vm, ok := c.fsm.VM(req.GetId())
	return &internalpb.GetVMResponse{Vm: vm, Found: ok}, nil
}

func (c *vmConfigRaft) Status(_ context.Context, _ *internalpb.StatusRequest, _ ...grpc.CallOption) (*internalpb.StatusResponse, error) {
	return &internalpb.StatusResponse{
		NodeId:  c.nodeID,
		Servers: []*internalpb.ServerInfo{{Id: "node-b", Address: "10.0.0.2:7000"}},
	}, nil
}

// vmConfigPeers answers only HostStats; any other forwarder method panics
// through the nil embedded interface, which is what a test wants if an
// unexpected call is made.
type vmConfigPeers struct {
	PeerForwarder
	hostStats func(addr string) (*rpcpb.HostStatsResponse, error)
}

func (p *vmConfigPeers) HostStats(_ context.Context, addr string) (*rpcpb.HostStatsResponse, error) {
	return p.hostStats(addr)
}

func hostWith(cores int32, totalMB uint64) *rpcpb.HostStatsResponse {
	return &rpcpb.HostStatsResponse{
		Cpu: &rpcpb.CPUStats{Cores: cores},
		Mem: &rpcpb.MemStats{TotalBytes: totalMB << 20},
	}
}

// newVMConfigServer builds a manager for nodeID whose own host reports
// localCores/localMB, with the given VMs already committed to the FSM.
func newVMConfigServer(t *testing.T, nodeID string, localCores int, localMB uint64, peers PeerForwarder, vms ...*internalpb.VMDefinition) (*Server, *raftnode.FSM) {
	t.Helper()
	fsm := raftnode.NewFSM()
	for i, vm := range vms {
		res := fsm.Apply(&raft.Log{Index: uint64(i + 1), Data: mustMarshalVMCreate(t, vm)})
		if e := res.(*raftnode.FSMApplyResult).Error; e != "" {
			t.Fatalf("seeding VM %s: %s", vm.GetId(), e)
		}
	}
	client := &vmConfigRaft{fsm: fsm, index: uint64(len(vms)), nodeID: nodeID}
	s := &Server{
		raft:   &RaftClient{client: client},
		nodeID: nodeID,
		peers:  peers,
		statsGather: func(context.Context) *hoststats.Snapshot {
			return &hoststats.Snapshot{
				CPU: hoststats.CPUInfo{Cores: localCores},
				Mem: hoststats.MemInfo{TotalBytes: localMB << 20},
			}
		},
	}
	return s, fsm
}

func mustMarshalVMCreate(t *testing.T, vm *internalpb.VMDefinition) []byte {
	t.Helper()
	data, err := proto.Marshal(&internalpb.Command{Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{Vm: vm}}})
	if err != nil {
		t.Fatalf("marshal create: %v", err)
	}
	return data
}

func vmOn(id, node string, vcpus uint32, memoryMB uint64) *internalpb.VMDefinition {
	return &internalpb.VMDefinition{
		Id: id, Name: id, NodeId: node, Vcpus: vcpus, MemoryMb: memoryMB,
		DesiredState: internalpb.VMState_VM_STATE_RUNNING,
		IsoName:      "installer.iso", DiskSizeMb: 20480,
	}
}

func TestSetVMConfig_LocalNodeWithinCapacityIsPersisted(t *testing.T) {
	web := vmOn("web", "node-a", 1, 1024)
	db := vmOn("db", "node-a", 1, 512)
	s, fsm := newVMConfigServer(t, "node-a", 4, 4096, nil, web, db)
	before, _ := fsm.VM("web")

	resp, err := s.SetVMConfig(context.Background(), &rpcpb.SetVMConfigRequest{Id: "web", Vcpus: 2, MemoryMb: 2048})
	if err != nil {
		t.Fatalf("SetVMConfig: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("Error = %q, want empty", resp.GetError())
	}

	got, _ := fsm.VM("web")
	want := proto.Clone(before).(*internalpb.VMDefinition)
	want.Vcpus = 2
	want.MemoryMb = 2048
	if !proto.Equal(got, want) {
		t.Errorf("stored VM:\n got  %v\n want %v", got, want)
	}
}

func TestSetVMConfig_RefusalsLeaveRecordUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		req     *rpcpb.SetVMConfigRequest
		peers   PeerForwarder
		seed    []*internalpb.VMDefinition
		wantErr string
	}{
		{
			name:    "memory exceeds what the node has left after other VMs",
			req:     &rpcpb.SetVMConfigRequest{Id: "web", Vcpus: 1, MemoryMb: 3585},
			seed:    []*internalpb.VMDefinition{vmOn("web", "node-a", 1, 1024), vmOn("db", "node-a", 1, 512)},
			wantErr: "memory_mb 3585 does not fit on node node-a: it has 4096 MB of physical memory and 512 MB is already committed to its other VMs, so at most 3584 MB is available",
		},
		{
			name:    "vcpus exceed the node's logical CPUs",
			req:     &rpcpb.SetVMConfigRequest{Id: "web", Vcpus: 5, MemoryMb: 1024},
			seed:    []*internalpb.VMDefinition{vmOn("web", "node-a", 1, 1024)},
			wantErr: "vcpus 5 exceeds the 4 logical CPUs on node node-a; choose at most 4",
		},
		{
			name: "remote node whose stats cannot be read fails closed",
			req:  &rpcpb.SetVMConfigRequest{Id: "web", Vcpus: 2, MemoryMb: 1024},
			peers: &vmConfigPeers{hostStats: func(string) (*rpcpb.HostStatsResponse, error) {
				return nil, errors.New("connection refused")
			}},
			seed:    []*internalpb.VMDefinition{vmOn("web", "node-b", 1, 1024)},
			wantErr: "cannot verify host capacity on node node-b, so the change was not saved",
		},
		{
			name:    "missing VM is refused by the FSM",
			req:     &rpcpb.SetVMConfigRequest{Id: "ghost", Vcpus: 2, MemoryMb: 1024},
			seed:    []*internalpb.VMDefinition{vmOn("web", "node-a", 1, 1024)},
			wantErr: `id "ghost" does not exist`,
		},
		{
			name:    "unsupported vcpu count is refused by the FSM",
			req:     &rpcpb.SetVMConfigRequest{Id: "web", Vcpus: 0, MemoryMb: 1024},
			seed:    []*internalpb.VMDefinition{vmOn("web", "node-a", 1, 1024)},
			wantErr: "vcpus 0 is outside the supported range 1-16",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, fsm := newVMConfigServer(t, "node-a", 4, 4096, tt.peers, tt.seed...)
			before, _ := fsm.VM(tt.req.GetId())

			resp, err := s.SetVMConfig(context.Background(), tt.req)
			if err != nil {
				t.Fatalf("SetVMConfig: %v", err)
			}
			if !strings.Contains(resp.GetError(), tt.wantErr) {
				t.Fatalf("Error = %q, want it to contain %q", resp.GetError(), tt.wantErr)
			}
			if before != nil {
				after, _ := fsm.VM(tt.req.GetId())
				if !proto.Equal(after, before) {
					t.Errorf("refused edit changed the record:\n got  %v\n want %v", after, before)
				}
			}
		})
	}
}

// TestSetVMConfig_RemoteNodeCapacityFromPeer checks that a VM owned by
// another Comb is judged against that Comb's own reported capacity, not
// this manager's.
func TestSetVMConfig_RemoteNodeCapacityFromPeer(t *testing.T) {
	asked := ""
	peers := &vmConfigPeers{hostStats: func(addr string) (*rpcpb.HostStatsResponse, error) {
		asked = addr
		return hostWith(8, 8192), nil
	}}
	// Local node is too small for the request; the owner has room.
	s, fsm := newVMConfigServer(t, "node-a", 2, 1024, peers, vmOn("web", "node-b", 1, 1024))

	resp, err := s.SetVMConfig(context.Background(), &rpcpb.SetVMConfigRequest{Id: "web", Vcpus: 6, MemoryMb: 4096})
	if err != nil {
		t.Fatalf("SetVMConfig: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("Error = %q, want empty (owner node-b has room)", resp.GetError())
	}
	if asked == "" {
		t.Fatal("owner's HostStats was never asked")
	}
	got, _ := fsm.VM("web")
	if got.GetVcpus() != 6 || got.GetMemoryMb() != 4096 {
		t.Errorf("stored vcpus/memory = %d/%d, want 6/4096", got.GetVcpus(), got.GetMemoryMb())
	}
}

// TestSetVMConfig_ReductionNeverChecksHost pins that shrinking a VM does
// not consult host capacity at all, so an already-overcommitted node can
// always be relieved.
func TestSetVMConfig_ReductionNeverChecksHost(t *testing.T) {
	peers := &vmConfigPeers{hostStats: func(string) (*rpcpb.HostStatsResponse, error) {
		t.Fatal("reducing vcpus/memory must not query host stats")
		return nil, nil
	}}
	s, fsm := newVMConfigServer(t, "node-a", 2, 1024, peers, vmOn("web", "node-b", 8, 8192))

	resp, err := s.SetVMConfig(context.Background(), &rpcpb.SetVMConfigRequest{Id: "web", Vcpus: 4, MemoryMb: 4096})
	if err != nil {
		t.Fatalf("SetVMConfig: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("Error = %q, want empty", resp.GetError())
	}
	got, _ := fsm.VM("web")
	if got.GetVcpus() != 4 || got.GetMemoryMb() != 4096 {
		t.Errorf("stored vcpus/memory = %d/%d, want 4/4096", got.GetVcpus(), got.GetMemoryMb())
	}
}

func TestCheckVMConfigCapacity_CountsOnlyOtherLiveVMsOnTheNode(t *testing.T) {
	cur := vmOn("web", "node-a", 1, 1024)
	host := hostWith(8, 4096)
	stopped := vmOn("stopped", "node-a", 1, 1000)
	stopped.DesiredState = internalpb.VMState_VM_STATE_STOPPED
	deleting := vmOn("gone", "node-a", 1, 3000)
	deleting.DesiredState = internalpb.VMState_VM_STATE_DELETING
	otherNode := vmOn("elsewhere", "node-b", 1, 3000)

	all := []*internalpb.VMDefinition{cur, stopped, deleting, otherNode}

	// 1000 MB stopped VM counts (stopped VMs must still be able to start);
	// 3000 MB deleting VM and 3000 MB VM on node-b do not. Available to
	// web is 4096-1000 = 3096 MB.
	if err := checkVMConfigCapacity(cur, all, 1, 3096, host); err != nil {
		t.Errorf("3096 MB should fit: %v", err)
	}
	if err := checkVMConfigCapacity(cur, all, 1, 3097, host); err == nil {
		t.Error("3097 MB should not fit alongside the stopped VM")
	}
}

func TestCheckVMConfigCapacity_UnreportedCapacityIsRefused(t *testing.T) {
	cur := vmOn("web", "node-a", 1, 1024)
	if err := checkVMConfigCapacity(cur, []*internalpb.VMDefinition{cur}, 2, 1024, &rpcpb.HostStatsResponse{}); err == nil || !strings.Contains(err.Error(), "did not report its CPU count") {
		t.Errorf("unreported CPU count: err = %v", err)
	}
	if err := checkVMConfigCapacity(cur, []*internalpb.VMDefinition{cur}, 1, 2048, &rpcpb.HostStatsResponse{Cpu: &rpcpb.CPUStats{Cores: 4}}); err == nil || !strings.Contains(err.Error(), "did not report its physical memory") {
		t.Errorf("unreported memory: err = %v", err)
	}
}

// TestSetVMConfig_RequiresOperator pins the authorization tier: like
// CreateVM and the other VM mutations, this is an Operator action.
func TestSetVMConfig_RequiresOperator(t *testing.T) {
	const method = "/apiary.rpc.v1.ManagerService/SetVMConfig"
	if got := requiredRoleFor(method); got != RoleOperator {
		t.Errorf("requiredRoleFor(%q) = %q, want %q", method, got, RoleOperator)
	}
	if got := requiredRoleFor("/apiary.rpc.v1.ManagerService/CreateVM"); got != RoleOperator {
		t.Errorf("CreateVM tier = %q, want %q (the tier SetVMConfig must match)", got, RoleOperator)
	}
}
