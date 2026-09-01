package manager

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakePeerServer implements just enough of rpcpb.ManagerServiceServer
// to test PeerReporter's client-side behavior (dialing, request
// shape, auth header) without a real raftd/managerd - the server-side
// RPC handlers themselves (translating a Report* call into a real
// raft Apply) are already covered by internal/manager's own
// integration tests.
type fakePeerServer struct {
	rpcpb.UnimplementedManagerServiceServer

	lastAuth string

	vmPhaseReq   *rpcpb.ReportVMPhaseRequest
	vmPhaseResp  *rpcpb.ReportVMPhaseResponse
	jailPhaseReq *rpcpb.ReportJailPhaseRequest

	listVMsResp      *rpcpb.ListVMsResponse
	getVMReq         *rpcpb.GetVMRequest
	getVMResp        *rpcpb.GetVMResponse
	listJailsResp    *rpcpb.ListJailsResponse
	getJailReq       *rpcpb.GetJailRequest
	getJailResp      *rpcpb.GetJailResponse
	listNetworksResp *rpcpb.ListNetworksResponse
}

func (f *fakePeerServer) ListVMs(context.Context, *rpcpb.ListVMsRequest) (*rpcpb.ListVMsResponse, error) {
	if f.listVMsResp != nil {
		return f.listVMsResp, nil
	}
	return &rpcpb.ListVMsResponse{}, nil
}

func (f *fakePeerServer) GetVM(_ context.Context, req *rpcpb.GetVMRequest) (*rpcpb.GetVMResponse, error) {
	f.getVMReq = req
	if f.getVMResp != nil {
		return f.getVMResp, nil
	}
	return &rpcpb.GetVMResponse{}, nil
}

func (f *fakePeerServer) ListJails(context.Context, *rpcpb.ListJailsRequest) (*rpcpb.ListJailsResponse, error) {
	if f.listJailsResp != nil {
		return f.listJailsResp, nil
	}
	return &rpcpb.ListJailsResponse{}, nil
}

func (f *fakePeerServer) GetJail(_ context.Context, req *rpcpb.GetJailRequest) (*rpcpb.GetJailResponse, error) {
	f.getJailReq = req
	if f.getJailResp != nil {
		return f.getJailResp, nil
	}
	return &rpcpb.GetJailResponse{}, nil
}

func (f *fakePeerServer) ListNetworks(context.Context, *rpcpb.ListNetworksRequest) (*rpcpb.ListNetworksResponse, error) {
	if f.listNetworksResp != nil {
		return f.listNetworksResp, nil
	}
	return &rpcpb.ListNetworksResponse{}, nil
}

func (f *fakePeerServer) ReportVMPhase(ctx context.Context, req *rpcpb.ReportVMPhaseRequest) (*rpcpb.ReportVMPhaseResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("authorization"); len(vals) > 0 {
			f.lastAuth = vals[0]
		}
	}
	f.vmPhaseReq = req
	if f.vmPhaseResp != nil {
		return f.vmPhaseResp, nil
	}
	return &rpcpb.ReportVMPhaseResponse{}, nil
}

func (f *fakePeerServer) ReportVMTeardownComplete(_ context.Context, _ *rpcpb.ReportVMTeardownCompleteRequest) (*rpcpb.ReportVMTeardownCompleteResponse, error) {
	return &rpcpb.ReportVMTeardownCompleteResponse{}, nil
}

func (f *fakePeerServer) ReportJailPhase(_ context.Context, req *rpcpb.ReportJailPhaseRequest) (*rpcpb.ReportJailPhaseResponse, error) {
	f.jailPhaseReq = req
	return &rpcpb.ReportJailPhaseResponse{}, nil
}

func (f *fakePeerServer) ReportJailTeardownComplete(_ context.Context, _ *rpcpb.ReportJailTeardownCompleteRequest) (*rpcpb.ReportJailTeardownCompleteResponse, error) {
	return &rpcpb.ReportJailTeardownCompleteResponse{}, nil
}

func newTestPeerServer(t *testing.T, fake *fakePeerServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := grpc.NewServer()
	rpcpb.RegisterManagerServiceServer(srv, fake)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestPeerReporter_ReportVMPhase_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	if err := p.ReportVMPhase(context.Background(), addr, "vm-1", "ready", ""); err != nil {
		t.Fatalf("ReportVMPhase() error: %v", err)
	}
	if fake.vmPhaseReq.GetId() != "vm-1" || fake.vmPhaseReq.GetPhase() != rpcpb.VMPhase_VM_PHASE_READY {
		t.Errorf("received request = %+v, want id=vm-1 phase=VM_PHASE_READY", fake.vmPhaseReq)
	}
}

func TestPeerReporter_AttachesAPIKey(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("test-key")

	if err := p.ReportVMPhase(context.Background(), addr, "vm-1", "ready", ""); err != nil {
		t.Fatalf("ReportVMPhase() error: %v", err)
	}
	if fake.lastAuth != "Bearer test-key" {
		t.Errorf("received Authorization = %q, want \"Bearer test-key\"", fake.lastAuth)
	}
}

func TestPeerReporter_NoAPIKeyAttachesNothing(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	if err := p.ReportVMPhase(context.Background(), addr, "vm-1", "ready", ""); err != nil {
		t.Fatalf("ReportVMPhase() error: %v", err)
	}
	if fake.lastAuth != "" {
		t.Errorf("received Authorization = %q, want none", fake.lastAuth)
	}
}

func TestPeerReporter_ReportVMPhase_ApplicationErrorIsReturned(t *testing.T) {
	fake := &fakePeerServer{vmPhaseResp: &rpcpb.ReportVMPhaseResponse{Error: "this node is not the leader either"}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	if err := p.ReportVMPhase(context.Background(), addr, "vm-1", "ready", ""); err == nil {
		t.Fatal("ReportVMPhase() error = nil, want the peer's own rejection surfaced")
	}
}

func TestPeerReporter_ReportJailPhase_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	if err := p.ReportJailPhase(context.Background(), addr, "jail-1", "error", "boom"); err != nil {
		t.Fatalf("ReportJailPhase() error: %v", err)
	}
	if fake.jailPhaseReq.GetId() != "jail-1" || fake.jailPhaseReq.GetPhase() != rpcpb.JailPhase_JAIL_PHASE_ERROR || fake.jailPhaseReq.GetPhaseError() != "boom" {
		t.Errorf("received request = %+v, want id=jail-1 phase=JAIL_PHASE_ERROR phase_error=boom", fake.jailPhaseReq)
	}
}

func TestPeerReporter_ListVMs_ReturnsPeerResponse(t *testing.T) {
	fake := &fakePeerServer{listVMsResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1"}}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	resp, err := p.ListVMs(context.Background(), addr)
	if err != nil {
		t.Fatalf("ListVMs() error: %v", err)
	}
	if len(resp.GetVms()) != 1 || resp.GetVms()[0].GetId() != "vm-1" {
		t.Errorf("ListVMs() = %+v, want one VM with id=vm-1", resp)
	}
}

func TestPeerReporter_GetVM_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	resp, err := p.GetVM(context.Background(), addr, "vm-1")
	if err != nil {
		t.Fatalf("GetVM() error: %v", err)
	}
	if fake.getVMReq.GetId() != "vm-1" {
		t.Errorf("received request id = %q, want vm-1", fake.getVMReq.GetId())
	}
	if !resp.GetFound() || resp.GetVm().GetId() != "vm-1" {
		t.Errorf("GetVM() = %+v, want found=true id=vm-1", resp)
	}
}

func TestPeerReporter_ListJails_ReturnsPeerResponse(t *testing.T) {
	fake := &fakePeerServer{listJailsResp: &rpcpb.ListJailsResponse{Jails: []*rpcpb.JailDefinition{{Id: "jail-1"}}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	resp, err := p.ListJails(context.Background(), addr)
	if err != nil {
		t.Fatalf("ListJails() error: %v", err)
	}
	if len(resp.GetJails()) != 1 || resp.GetJails()[0].GetId() != "jail-1" {
		t.Errorf("ListJails() = %+v, want one jail with id=jail-1", resp)
	}
}

func TestPeerReporter_GetJail_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{getJailResp: &rpcpb.GetJailResponse{Found: true, Jail: &rpcpb.JailDefinition{Id: "jail-1"}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	resp, err := p.GetJail(context.Background(), addr, "jail-1")
	if err != nil {
		t.Fatalf("GetJail() error: %v", err)
	}
	if fake.getJailReq.GetId() != "jail-1" {
		t.Errorf("received request id = %q, want jail-1", fake.getJailReq.GetId())
	}
	if !resp.GetFound() || resp.GetJail().GetId() != "jail-1" {
		t.Errorf("GetJail() = %+v, want found=true id=jail-1", resp)
	}
}

func TestPeerReporter_ListNetworks_ReturnsPeerResponse(t *testing.T) {
	fake := &fakePeerServer{listNetworksResp: &rpcpb.ListNetworksResponse{Networks: []*rpcpb.NetworkDefinition{{Id: "net-1"}}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("")

	resp, err := p.ListNetworks(context.Background(), addr)
	if err != nil {
		t.Fatalf("ListNetworks() error: %v", err)
	}
	if len(resp.GetNetworks()) != 1 || resp.GetNetworks()[0].GetId() != "net-1" {
		t.Errorf("ListNetworks() = %+v, want one network with id=net-1", resp)
	}
}
