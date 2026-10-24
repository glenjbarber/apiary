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
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// ADR-0152's managerd-side identity rename. What matters here is the
// RPC contract: the request shape is validated before anything is
// submitted, the FSM's authoritative outcome comes back (not a bare
// success), the outcome distinguishes a rename this call applied from
// one that had already been applied, and a non-leader node forwards to
// the leader rather than failing.

// renameRaftClient backs a RaftClient with a real FSM, so the handler
// under test exercises the same apply path a managerd does: the command
// is marshalled, applied, and read back.
//
// leaderHint, when set, makes every apply answer with a rejection
// carrying that leader hint - the shape a non-leader raftd replies
// with, and what the forwarding path keys on.
type renameRaftClient struct {
	internalpb.RaftInternalClient
	fsm        *raftnode.FSM
	index      uint64
	leaderHint string
}

func (c *renameRaftClient) Apply(_ context.Context, req *internalpb.ApplyRequest, _ ...grpc.CallOption) (*internalpb.ApplyResponse, error) {
	if c.leaderHint != "" {
		return &internalpb.ApplyResponse{Error: "raft: this node is not the leader", LeaderHint: c.leaderHint}, nil
	}
	c.index++
	res := c.fsm.Apply(&raft.Log{Index: c.index, Data: req.GetPayload()}).(*raftnode.FSMApplyResult)
	if res.Error != "" {
		return &internalpb.ApplyResponse{Error: res.Error}, nil
	}
	var payload proto.Message = res.VM
	if res.RenameResult != nil {
		payload = res.RenameResult
	}
	result, err := proto.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &internalpb.ApplyResponse{Result: result}, nil
}

func (c *renameRaftClient) GetVM(_ context.Context, req *internalpb.GetVMRequest, _ ...grpc.CallOption) (*internalpb.GetVMResponse, error) {
	vm, ok := c.fsm.VM(req.GetId())
	return &internalpb.GetVMResponse{Vm: vm, Found: ok}, nil
}

// newRenameServer builds a managerd whose own FSM already holds the
// given VMs.
func newRenameServer(t *testing.T, nodeID string, peers PeerForwarder, vms ...*internalpb.VMDefinition) (*Server, *raftnode.FSM) {
	t.Helper()
	fsm := raftnode.NewFSM()
	for i, vm := range vms {
		data, err := proto.Marshal(&internalpb.Command{
			Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{Vm: vm}},
		})
		if err != nil {
			t.Fatalf("marshaling the create command: %v", err)
		}
		res := fsm.Apply(&raft.Log{Index: uint64(i + 1), Data: data}).(*raftnode.FSMApplyResult)
		if res.Error != "" {
			t.Fatalf("seeding VM %s: %s", vm.GetId(), res.Error)
		}
	}
	s := &Server{
		raft:   &RaftClient{client: &renameRaftClient{fsm: fsm, index: uint64(len(vms))}},
		nodeID: nodeID,
		peers:  peers,
	}
	_ = fsm
	return s, fsm
}

func TestRenameVMIdentityReturnsTheAuthoritativeVMAndOutcome(t *testing.T) {
	s, fsm := newRenameServer(t, "manager-1", nil, &internalpb.VMDefinition{
		Id: "legacy-id", Name: "legacy", NodeId: "node-a",
		DesiredState: internalpb.VMState_VM_STATE_RUNNING,
	})

	resp, err := s.RenameVMIdentity(context.Background(), &rpcpb.RenameVMIdentityRequest{
		SourceId:      "legacy-id",
		DestinationId: "uid-scoped-id",
	})
	if err != nil {
		t.Fatalf("RenameVMIdentity() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RenameVMIdentity() error = %q, want a successful rename", resp.GetError())
	}
	if resp.GetOutcome() != "renamed" {
		t.Errorf("outcome = %q, want renamed", resp.GetOutcome())
	}
	// The response carries the AUTHORITATIVE record, not an echo of the
	// request: a caller must be able to use it directly.
	if resp.GetVm().GetId() != "uid-scoped-id" {
		t.Errorf("vm.id = %q, want uid-scoped-id", resp.GetVm().GetId())
	}
	if resp.GetVm().GetResourceId() != "legacy-id" {
		t.Errorf("vm.resource_id = %q, want legacy-id: the physical identity must survive the rename", resp.GetVm().GetResourceId())
	}
	// And the FSM really moved the key.
	if _, found := fsm.VM("legacy-id"); found {
		t.Error("the source key is still present after the RPC")
	}
	if _, found := fsm.VM("uid-scoped-id"); !found {
		t.Error("the destination key is missing after the RPC")
	}
}

func TestRenameVMIdentityIsIdempotentForACompletedRename(t *testing.T) {
	s, _ := newRenameServer(t, "manager-1", nil, &internalpb.VMDefinition{
		Id: "legacy-id", Name: "legacy", NodeId: "node-a",
		DesiredState: internalpb.VMState_VM_STATE_RUNNING,
	})

	if _, err := s.RenameVMIdentity(context.Background(), &rpcpb.RenameVMIdentityRequest{
		SourceId: "legacy-id", DestinationId: "uid-scoped-id",
	}); err != nil {
		t.Fatalf("the first RenameVMIdentity() error: %v", err)
	}

	resp, err := s.RenameVMIdentity(context.Background(), &rpcpb.RenameVMIdentityRequest{
		SourceId: "legacy-id", DestinationId: "uid-scoped-id",
	})
	if err != nil {
		t.Fatalf("the retry RenameVMIdentity() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("the retry error = %q, want the retry to succeed", resp.GetError())
	}
	if resp.GetOutcome() != "already_renamed" {
		t.Errorf("outcome = %q, want already_renamed: a completed retry must be distinguishable from a fresh rename", resp.GetOutcome())
	}
}

func TestRenameVMIdentityValidatesTheRequestShape(t *testing.T) {
	cases := []struct {
		name string
		req  *rpcpb.RenameVMIdentityRequest
	}{
		{"missing source", &rpcpb.RenameVMIdentityRequest{DestinationId: "uid-scoped-id"}},
		{"missing destination", &rpcpb.RenameVMIdentityRequest{SourceId: "legacy-id"}},
		{"source equals destination", &rpcpb.RenameVMIdentityRequest{SourceId: "legacy-id", DestinationId: "legacy-id"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newRenameServer(t, "manager-1", nil, &internalpb.VMDefinition{
				Id: "legacy-id", Name: "legacy", NodeId: "node-a",
			})
			resp, err := s.RenameVMIdentity(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("RenameVMIdentity() error: %v", err)
			}
			if resp.GetError() == "" {
				t.Error("RenameVMIdentity() with an invalid request shape was accepted, want a refusal")
			}
		})
	}
}

// fakeRenameForwarder records the forwarded request and answers with
// whatever the test wants. It stands in for the leader's own managerd,
// so the forwarding path is exercised without a second real raft.
type fakeRenameForwarder struct {
	PeerForwarder
	calls []string
	resp  *rpcpb.RenameVMIdentityResponse
	err   error
}

func (f *fakeRenameForwarder) RenameVMIdentity(_ context.Context, addr string, req *rpcpb.RenameVMIdentityRequest) (*rpcpb.RenameVMIdentityResponse, error) {
	f.calls = append(f.calls, addr+" "+req.GetSourceId()+" "+req.GetDestinationId())
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestRenameVMIdentityForwardsToTheLeader(t *testing.T) {
	s, _ := newRenameServer(t, "manager-1", nil, &internalpb.VMDefinition{
		Id: "legacy-id", Name: "legacy", NodeId: "node-a",
	})
	// This node's raftd says it is not the leader.
	s.raft = &RaftClient{client: &renameRaftClient{leaderHint: "10.0.0.9:7000"}}

	peers := &fakeRenameForwarder{resp: &rpcpb.RenameVMIdentityResponse{
		Outcome: "renamed",
		Vm:      &rpcpb.VMDefinition{Id: "uid-scoped-id", ResourceId: "legacy-id"},
	}}
	s.peers = peers

	resp, err := s.RenameVMIdentity(context.Background(), &rpcpb.RenameVMIdentityRequest{
		SourceId: "legacy-id", DestinationId: "uid-scoped-id",
	})
	if err != nil {
		t.Fatalf("RenameVMIdentity() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RenameVMIdentity() error = %q, want the forwarded success", resp.GetError())
	}
	if len(peers.calls) != 1 {
		t.Fatalf("forwarded calls = %v, want exactly one", peers.calls)
	}
	// The forwarded address is the LEADER's managerd address, derived
	// from the raft leader hint.
	if !strings.HasPrefix(peers.calls[0], "10.0.0.9:") {
		t.Errorf("forwarded to %q, want the leader hint's host", peers.calls[0])
	}
	// And the leader's authoritative answer comes back, not a bare
	// success: the outcome and the resulting VM both travel.
	if resp.GetOutcome() != "renamed" {
		t.Errorf("outcome = %q, want the leader's outcome", resp.GetOutcome())
	}
	if resp.GetVm().GetId() != "uid-scoped-id" {
		t.Errorf("vm.id = %q, want the leader's authoritative record", resp.GetVm().GetId())
	}
}

func TestRenameVMIdentityReportsAForwardingFailureWithTheLeaderHint(t *testing.T) {
	s, _ := newRenameServer(t, "manager-1", nil, &internalpb.VMDefinition{
		Id: "legacy-id", Name: "legacy", NodeId: "node-a",
	})
	s.raft = &RaftClient{client: &renameRaftClient{leaderHint: "10.0.0.9:7000"}}
	s.peers = &fakeRenameForwarder{err: errors.New("tls: failed to verify certificate")}

	resp, err := s.RenameVMIdentity(context.Background(), &rpcpb.RenameVMIdentityRequest{
		SourceId: "legacy-id", DestinationId: "uid-scoped-id",
	})
	if err != nil {
		t.Fatalf("RenameVMIdentity() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("a failed forward was reported as a success")
	}
	if !strings.Contains(resp.GetError(), "failed to verify certificate") {
		t.Errorf("error = %q, want the real forwarding failure surfaced", resp.GetError())
	}
	if resp.GetLeaderHint() == "" {
		t.Error("leader_hint is empty; a caller cannot re-resolve leadership and retry without it")
	}
}

func TestRenameVMIdentitySurfacesAnFSMRefusal(t *testing.T) {
	s, _ := newRenameServer(t, "manager-1", nil, &internalpb.VMDefinition{
		Id: "legacy-id", Name: "legacy", NodeId: "node-a",
	})

	resp, err := s.RenameVMIdentity(context.Background(), &rpcpb.RenameVMIdentityRequest{
		SourceId: "legacy-id", DestinationId: "uid-scoped-id",
		ExpectedNodeId: "node-z",
	})
	if err != nil {
		t.Fatalf("RenameVMIdentity() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("a rename with a mismatched expected owner was accepted, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "owned by node") {
		t.Errorf("error = %q, want it to name who actually owns the VM", resp.GetError())
	}
}
