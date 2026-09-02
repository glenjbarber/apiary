package manager

import (
	"bytes"
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

	createVMReq  *rpcpb.CreateVMRequest
	createVMResp *rpcpb.CreateVMResponse
	updateVMReq  *rpcpb.UpdateVMRequest
	deleteVMReq  *rpcpb.DeleteVMRequest

	createJailReq  *rpcpb.CreateJailRequest
	createJailResp *rpcpb.CreateJailResponse
	updateJailReq  *rpcpb.UpdateJailRequest
	deleteJailReq  *rpcpb.DeleteJailRequest

	createNetworkReq  *rpcpb.CreateNetworkRequest
	createNetworkResp *rpcpb.CreateNetworkResponse
	deleteNetworkReq  *rpcpb.DeleteNetworkRequest

	createAPIKeyReq  *rpcpb.CreateAPIKeyRequest
	createAPIKeyResp *rpcpb.CreateAPIKeyResponse
	revokeAPIKeyReq  *rpcpb.RevokeAPIKeyRequest
	listAPIKeysResp  *rpcpb.ListAPIKeysResponse

	forcePurgeVMReq    *rpcpb.ForcePurgeVMRequest
	forcePurgeVMResp   *rpcpb.ForcePurgeVMResponse
	migrateVMReq       *rpcpb.MigrateVMRequest
	migrateVMResp      *rpcpb.MigrateVMResponse
	forcePurgeJailReq  *rpcpb.ForcePurgeJailRequest
	forcePurgeJailResp *rpcpb.ForcePurgeJailResponse
	migrateJailReq     *rpcpb.MigrateJailRequest
	migrateJailResp    *rpcpb.MigrateJailResponse

	uploadedISOName string
	uploadedISOHash string
	uploadedISOData []byte
	uploadISOResp   *rpcpb.UploadISOResponse

	pushISOToReq  *rpcpb.PushISOToRequest
	pushISOToResp *rpcpb.PushISOToResponse

	replicateISOReq  *rpcpb.ReplicateISORequest
	replicateISOResp *rpcpb.ReplicateISOResponse
}

func (f *fakePeerServer) UploadISO(stream rpcpb.ManagerService_UploadISOServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	f.uploadedISOName = first.GetMetadata().GetName()
	f.uploadedISOHash = first.GetMetadata().GetExpectedSha256()
	for {
		req, err := stream.Recv()
		if err != nil {
			break
		}
		f.uploadedISOData = append(f.uploadedISOData, req.GetChunk()...)
	}
	if f.uploadISOResp != nil {
		return stream.SendAndClose(f.uploadISOResp)
	}
	return stream.SendAndClose(&rpcpb.UploadISOResponse{})
}

func (f *fakePeerServer) PushISOTo(_ context.Context, req *rpcpb.PushISOToRequest) (*rpcpb.PushISOToResponse, error) {
	f.pushISOToReq = req
	if f.pushISOToResp != nil {
		return f.pushISOToResp, nil
	}
	return &rpcpb.PushISOToResponse{}, nil
}

func (f *fakePeerServer) ReplicateISO(_ context.Context, req *rpcpb.ReplicateISORequest) (*rpcpb.ReplicateISOResponse, error) {
	f.replicateISOReq = req
	if f.replicateISOResp != nil {
		return f.replicateISOResp, nil
	}
	return &rpcpb.ReplicateISOResponse{}, nil
}

func (f *fakePeerServer) ListAPIKeys(context.Context, *rpcpb.ListAPIKeysRequest) (*rpcpb.ListAPIKeysResponse, error) {
	if f.listAPIKeysResp != nil {
		return f.listAPIKeysResp, nil
	}
	return &rpcpb.ListAPIKeysResponse{}, nil
}

func (f *fakePeerServer) ForcePurgeVM(_ context.Context, req *rpcpb.ForcePurgeVMRequest) (*rpcpb.ForcePurgeVMResponse, error) {
	f.forcePurgeVMReq = req
	if f.forcePurgeVMResp != nil {
		return f.forcePurgeVMResp, nil
	}
	return &rpcpb.ForcePurgeVMResponse{}, nil
}

func (f *fakePeerServer) MigrateVM(_ context.Context, req *rpcpb.MigrateVMRequest) (*rpcpb.MigrateVMResponse, error) {
	f.migrateVMReq = req
	if f.migrateVMResp != nil {
		return f.migrateVMResp, nil
	}
	return &rpcpb.MigrateVMResponse{}, nil
}

func (f *fakePeerServer) ForcePurgeJail(_ context.Context, req *rpcpb.ForcePurgeJailRequest) (*rpcpb.ForcePurgeJailResponse, error) {
	f.forcePurgeJailReq = req
	if f.forcePurgeJailResp != nil {
		return f.forcePurgeJailResp, nil
	}
	return &rpcpb.ForcePurgeJailResponse{}, nil
}

func (f *fakePeerServer) MigrateJail(_ context.Context, req *rpcpb.MigrateJailRequest) (*rpcpb.MigrateJailResponse, error) {
	f.migrateJailReq = req
	if f.migrateJailResp != nil {
		return f.migrateJailResp, nil
	}
	return &rpcpb.MigrateJailResponse{}, nil
}

func (f *fakePeerServer) CreateVM(_ context.Context, req *rpcpb.CreateVMRequest) (*rpcpb.CreateVMResponse, error) {
	f.createVMReq = req
	if f.createVMResp != nil {
		return f.createVMResp, nil
	}
	return &rpcpb.CreateVMResponse{}, nil
}

func (f *fakePeerServer) UpdateVM(_ context.Context, req *rpcpb.UpdateVMRequest) (*rpcpb.UpdateVMResponse, error) {
	f.updateVMReq = req
	return &rpcpb.UpdateVMResponse{}, nil
}

func (f *fakePeerServer) DeleteVM(_ context.Context, req *rpcpb.DeleteVMRequest) (*rpcpb.DeleteVMResponse, error) {
	f.deleteVMReq = req
	return &rpcpb.DeleteVMResponse{}, nil
}

func (f *fakePeerServer) CreateJail(_ context.Context, req *rpcpb.CreateJailRequest) (*rpcpb.CreateJailResponse, error) {
	f.createJailReq = req
	if f.createJailResp != nil {
		return f.createJailResp, nil
	}
	return &rpcpb.CreateJailResponse{}, nil
}

func (f *fakePeerServer) UpdateJail(_ context.Context, req *rpcpb.UpdateJailRequest) (*rpcpb.UpdateJailResponse, error) {
	f.updateJailReq = req
	return &rpcpb.UpdateJailResponse{}, nil
}

func (f *fakePeerServer) DeleteJail(_ context.Context, req *rpcpb.DeleteJailRequest) (*rpcpb.DeleteJailResponse, error) {
	f.deleteJailReq = req
	return &rpcpb.DeleteJailResponse{}, nil
}

func (f *fakePeerServer) CreateNetwork(_ context.Context, req *rpcpb.CreateNetworkRequest) (*rpcpb.CreateNetworkResponse, error) {
	f.createNetworkReq = req
	if f.createNetworkResp != nil {
		return f.createNetworkResp, nil
	}
	return &rpcpb.CreateNetworkResponse{}, nil
}

func (f *fakePeerServer) DeleteNetwork(_ context.Context, req *rpcpb.DeleteNetworkRequest) (*rpcpb.DeleteNetworkResponse, error) {
	f.deleteNetworkReq = req
	return &rpcpb.DeleteNetworkResponse{}, nil
}

func (f *fakePeerServer) CreateAPIKey(_ context.Context, req *rpcpb.CreateAPIKeyRequest) (*rpcpb.CreateAPIKeyResponse, error) {
	f.createAPIKeyReq = req
	if f.createAPIKeyResp != nil {
		return f.createAPIKeyResp, nil
	}
	return &rpcpb.CreateAPIKeyResponse{}, nil
}

func (f *fakePeerServer) RevokeAPIKey(_ context.Context, req *rpcpb.RevokeAPIKeyRequest) (*rpcpb.RevokeAPIKeyResponse, error) {
	f.revokeAPIKeyReq = req
	return &rpcpb.RevokeAPIKeyResponse{}, nil
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
	p := NewPeerReporter("", false, nil)

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
	p := NewPeerReporter("test-key", false, nil)

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
	p := NewPeerReporter("", false, nil)

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
	p := NewPeerReporter("", false, nil)

	if err := p.ReportVMPhase(context.Background(), addr, "vm-1", "ready", ""); err == nil {
		t.Fatal("ReportVMPhase() error = nil, want the peer's own rejection surfaced")
	}
}

func TestPeerReporter_ReportJailPhase_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

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
	p := NewPeerReporter("", false, nil)

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
	p := NewPeerReporter("", false, nil)

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
	p := NewPeerReporter("", false, nil)

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
	p := NewPeerReporter("", false, nil)

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
	p := NewPeerReporter("", false, nil)

	resp, err := p.ListNetworks(context.Background(), addr)
	if err != nil {
		t.Fatalf("ListNetworks() error: %v", err)
	}
	if len(resp.GetNetworks()) != 1 || resp.GetNetworks()[0].GetId() != "net-1" {
		t.Errorf("ListNetworks() = %+v, want one network with id=net-1", resp)
	}
}

// TestPeerReporter_TLSDialFailsAgainstPlaintextServer confirms UseTLS
// actually changes the dial behavior (attempts a real TLS handshake)
// rather than being a no-op - dialing a plain, non-TLS test server
// with UseTLS=true must fail, not silently succeed in plaintext.
func TestPeerReporter_TLSDialFailsAgainstPlaintextServer(t *testing.T) {
	fake := &fakePeerServer{listVMsResp: &rpcpb.ListVMsResponse{}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", true, nil)

	if _, err := p.ListVMs(context.Background(), addr); err == nil {
		t.Fatal("ListVMs() over TLS against a plaintext server = nil error, want a handshake failure")
	}
}

// The following tests cover the write-forwarding methods added for
// ADR-0036's follow-up (extending ADR-0035's read forwarding to the
// external write RPCs) - each just confirms the original request is
// passed through unchanged and the peer's real response comes back.

func TestPeerReporter_CreateVM_SendsCorrectRequestAndReturnsResponse(t *testing.T) {
	fake := &fakePeerServer{createVMResp: &rpcpb.CreateVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.CreateVMRequest{Vm: &rpcpb.VMDefinition{Id: "vm-1", NodeId: "node-a"}}
	resp, err := p.CreateVM(context.Background(), addr, req)
	if err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}
	if fake.createVMReq.GetVm().GetNodeId() != "node-a" {
		t.Errorf("received request node_id = %q, want node-a", fake.createVMReq.GetVm().GetNodeId())
	}
	if resp.GetVm().GetId() != "vm-1" {
		t.Errorf("CreateVM() = %+v, want id=vm-1", resp)
	}
}

func TestPeerReporter_UpdateVM_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.UpdateVMRequest{Vm: &rpcpb.VMDefinition{Id: "vm-1"}}
	if _, err := p.UpdateVM(context.Background(), addr, req); err != nil {
		t.Fatalf("UpdateVM() error: %v", err)
	}
	if fake.updateVMReq.GetVm().GetId() != "vm-1" {
		t.Errorf("received request id = %q, want vm-1", fake.updateVMReq.GetVm().GetId())
	}
}

func TestPeerReporter_DeleteVM_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.DeleteVMRequest{Id: "vm-1"}
	if _, err := p.DeleteVM(context.Background(), addr, req); err != nil {
		t.Fatalf("DeleteVM() error: %v", err)
	}
	if fake.deleteVMReq.GetId() != "vm-1" {
		t.Errorf("received request id = %q, want vm-1", fake.deleteVMReq.GetId())
	}
}

func TestPeerReporter_CreateJail_SendsCorrectRequestAndReturnsResponse(t *testing.T) {
	fake := &fakePeerServer{createJailResp: &rpcpb.CreateJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1"}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.CreateJailRequest{Jail: &rpcpb.JailDefinition{Id: "jail-1", NodeId: "node-a"}}
	resp, err := p.CreateJail(context.Background(), addr, req)
	if err != nil {
		t.Fatalf("CreateJail() error: %v", err)
	}
	if fake.createJailReq.GetJail().GetNodeId() != "node-a" {
		t.Errorf("received request node_id = %q, want node-a", fake.createJailReq.GetJail().GetNodeId())
	}
	if resp.GetJail().GetId() != "jail-1" {
		t.Errorf("CreateJail() = %+v, want id=jail-1", resp)
	}
}

func TestPeerReporter_UpdateJail_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.UpdateJailRequest{Jail: &rpcpb.JailDefinition{Id: "jail-1"}}
	if _, err := p.UpdateJail(context.Background(), addr, req); err != nil {
		t.Fatalf("UpdateJail() error: %v", err)
	}
	if fake.updateJailReq.GetJail().GetId() != "jail-1" {
		t.Errorf("received request id = %q, want jail-1", fake.updateJailReq.GetJail().GetId())
	}
}

func TestPeerReporter_DeleteJail_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.DeleteJailRequest{Id: "jail-1"}
	if _, err := p.DeleteJail(context.Background(), addr, req); err != nil {
		t.Fatalf("DeleteJail() error: %v", err)
	}
	if fake.deleteJailReq.GetId() != "jail-1" {
		t.Errorf("received request id = %q, want jail-1", fake.deleteJailReq.GetId())
	}
}

func TestPeerReporter_CreateNetwork_SendsCorrectRequestAndReturnsResponse(t *testing.T) {
	fake := &fakePeerServer{createNetworkResp: &rpcpb.CreateNetworkResponse{Network: &rpcpb.NetworkDefinition{Id: "net-1"}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.CreateNetworkRequest{Network: &rpcpb.NetworkDefinition{Id: "net-1", VlanId: 42}}
	resp, err := p.CreateNetwork(context.Background(), addr, req)
	if err != nil {
		t.Fatalf("CreateNetwork() error: %v", err)
	}
	if fake.createNetworkReq.GetNetwork().GetVlanId() != 42 {
		t.Errorf("received request vlan_id = %d, want 42", fake.createNetworkReq.GetNetwork().GetVlanId())
	}
	if resp.GetNetwork().GetId() != "net-1" {
		t.Errorf("CreateNetwork() = %+v, want id=net-1", resp)
	}
}

func TestPeerReporter_DeleteNetwork_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.DeleteNetworkRequest{Id: "net-1"}
	if _, err := p.DeleteNetwork(context.Background(), addr, req); err != nil {
		t.Fatalf("DeleteNetwork() error: %v", err)
	}
	if fake.deleteNetworkReq.GetId() != "net-1" {
		t.Errorf("received request id = %q, want net-1", fake.deleteNetworkReq.GetId())
	}
}

func TestPeerReporter_CreateAPIKey_SendsCorrectRequestAndReturnsResponse(t *testing.T) {
	fake := &fakePeerServer{createAPIKeyResp: &rpcpb.CreateAPIKeyResponse{RawKey: "raw-from-leader"}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.CreateAPIKeyRequest{Name: "ci-key", Role: "viewer"}
	resp, err := p.CreateAPIKey(context.Background(), addr, req)
	if err != nil {
		t.Fatalf("CreateAPIKey() error: %v", err)
	}
	if fake.createAPIKeyReq.GetName() != "ci-key" {
		t.Errorf("received request name = %q, want ci-key", fake.createAPIKeyReq.GetName())
	}
	if resp.GetRawKey() != "raw-from-leader" {
		t.Errorf("CreateAPIKey() = %+v, want the leader's own raw key returned", resp)
	}
}

func TestPeerReporter_RevokeAPIKey_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.RevokeAPIKeyRequest{Id: "key-1"}
	if _, err := p.RevokeAPIKey(context.Background(), addr, req); err != nil {
		t.Fatalf("RevokeAPIKey() error: %v", err)
	}
	if fake.revokeAPIKeyReq.GetId() != "key-1" {
		t.Errorf("received request id = %q, want key-1", fake.revokeAPIKeyReq.GetId())
	}
}

// The following cover the remaining ADR-0037 follow-up methods:
// ListAPIKeys (a read missed from ADR-0035's original set) and
// ForcePurgeVM/MigrateVM/ForcePurgeJail/MigrateJail (forwarding the
// whole original request, since these RPCs read local FSM state up
// front rather than going through the exported, forwarding-enabled
// GetVM/GetJail handlers).

func TestPeerReporter_ListAPIKeys_ReturnsPeerResponse(t *testing.T) {
	fake := &fakePeerServer{listAPIKeysResp: &rpcpb.ListAPIKeysResponse{Keys: []*rpcpb.APIKeyInfo{{Id: "key-1"}}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	resp, err := p.ListAPIKeys(context.Background(), addr)
	if err != nil {
		t.Fatalf("ListAPIKeys() error: %v", err)
	}
	if len(resp.GetKeys()) != 1 || resp.GetKeys()[0].GetId() != "key-1" {
		t.Errorf("ListAPIKeys() = %+v, want one key with id=key-1", resp)
	}
}

func TestPeerReporter_ForcePurgeVM_SendsCorrectRequestAndReturnsResponse(t *testing.T) {
	fake := &fakePeerServer{forcePurgeVMResp: &rpcpb.ForcePurgeVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.ForcePurgeVMRequest{Id: "vm-1"}
	resp, err := p.ForcePurgeVM(context.Background(), addr, req)
	if err != nil {
		t.Fatalf("ForcePurgeVM() error: %v", err)
	}
	if fake.forcePurgeVMReq.GetId() != "vm-1" {
		t.Errorf("received request id = %q, want vm-1", fake.forcePurgeVMReq.GetId())
	}
	if resp.GetVm().GetId() != "vm-1" {
		t.Errorf("ForcePurgeVM() = %+v, want id=vm-1", resp)
	}
}

func TestPeerReporter_MigrateVM_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.MigrateVMRequest{Id: "vm-1", TargetNodeId: "node-b"}
	if _, err := p.MigrateVM(context.Background(), addr, req); err != nil {
		t.Fatalf("MigrateVM() error: %v", err)
	}
	if fake.migrateVMReq.GetTargetNodeId() != "node-b" {
		t.Errorf("received request target_node_id = %q, want node-b", fake.migrateVMReq.GetTargetNodeId())
	}
}

func TestPeerReporter_ForcePurgeJail_SendsCorrectRequestAndReturnsResponse(t *testing.T) {
	fake := &fakePeerServer{forcePurgeJailResp: &rpcpb.ForcePurgeJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1"}}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.ForcePurgeJailRequest{Id: "jail-1"}
	resp, err := p.ForcePurgeJail(context.Background(), addr, req)
	if err != nil {
		t.Fatalf("ForcePurgeJail() error: %v", err)
	}
	if fake.forcePurgeJailReq.GetId() != "jail-1" {
		t.Errorf("received request id = %q, want jail-1", fake.forcePurgeJailReq.GetId())
	}
	if resp.GetJail().GetId() != "jail-1" {
		t.Errorf("ForcePurgeJail() = %+v, want id=jail-1", resp)
	}
}

func TestPeerReporter_MigrateJail_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	req := &rpcpb.MigrateJailRequest{Id: "jail-1", TargetNodeId: "node-b"}
	if _, err := p.MigrateJail(context.Background(), addr, req); err != nil {
		t.Fatalf("MigrateJail() error: %v", err)
	}
	if fake.migrateJailReq.GetTargetNodeId() != "node-b" {
		t.Errorf("received request target_node_id = %q, want node-b", fake.migrateJailReq.GetTargetNodeId())
	}
}

// The following tests cover ADR-0040's copy-on-demand ISO replication:
// PeerReporter.UploadISO (the actual chunked transfer, used by
// PushISOTo's own handler), RequestISOPush (calls PushISOTo on a
// peer), and ReplicateISO (forwards the whole external RPC to a peer,
// the same HostStats-style forwarding pattern).

func TestPeerReporter_UploadISO_SendsMetadataThenChunksInOrder(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	data := []byte("fake iso file contents")
	if err := p.UploadISO(context.Background(), addr, "test.iso", "deadbeef", bytes.NewReader(data)); err != nil {
		t.Fatalf("UploadISO() error: %v", err)
	}
	if fake.uploadedISOName != "test.iso" || fake.uploadedISOHash != "deadbeef" {
		t.Errorf("received metadata = name:%q hash:%q, want name:test.iso hash:deadbeef", fake.uploadedISOName, fake.uploadedISOHash)
	}
	if string(fake.uploadedISOData) != string(data) {
		t.Errorf("received data = %q, want %q", fake.uploadedISOData, data)
	}
}

func TestPeerReporter_UploadISO_PeerRejectionIsReturnedAsError(t *testing.T) {
	fake := &fakePeerServer{uploadISOResp: &rpcpb.UploadISOResponse{Error: "sha256 mismatch"}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	if err := p.UploadISO(context.Background(), addr, "test.iso", "deadbeef", bytes.NewReader([]byte("data"))); err == nil {
		t.Fatal("UploadISO() error = nil, want the peer's own rejection surfaced")
	}
}

func TestPeerReporter_RequestISOPush_SendsCorrectRequest(t *testing.T) {
	fake := &fakePeerServer{}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	if err := p.RequestISOPush(context.Background(), addr, "test.iso", "node-b"); err != nil {
		t.Fatalf("RequestISOPush() error: %v", err)
	}
	if fake.pushISOToReq.GetName() != "test.iso" || fake.pushISOToReq.GetTargetNodeId() != "node-b" {
		t.Errorf("received request = %+v, want name=test.iso target_node_id=node-b", fake.pushISOToReq)
	}
}

func TestPeerReporter_RequestISOPush_ApplicationErrorIsReturned(t *testing.T) {
	fake := &fakePeerServer{pushISOToResp: &rpcpb.PushISOToResponse{Error: "not present on this node"}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	if err := p.RequestISOPush(context.Background(), addr, "test.iso", "node-b"); err == nil {
		t.Fatal("RequestISOPush() error = nil, want the peer's own rejection surfaced")
	}
}

func TestPeerReporter_ReplicateISO_SendsCorrectRequestAndReturnsResponse(t *testing.T) {
	fake := &fakePeerServer{replicateISOResp: &rpcpb.ReplicateISOResponse{Name: "test.iso", Sha256: "deadbeef"}}
	addr := newTestPeerServer(t, fake)
	p := NewPeerReporter("", false, nil)

	resp, err := p.ReplicateISO(context.Background(), addr, "test.iso", "node-a")
	if err != nil {
		t.Fatalf("ReplicateISO() error: %v", err)
	}
	if fake.replicateISOReq.GetName() != "test.iso" || fake.replicateISOReq.GetSourceNodeId() != "node-a" {
		t.Errorf("received request = %+v, want name=test.iso source_node_id=node-a", fake.replicateISOReq)
	}
	if resp.GetSha256() != "deadbeef" {
		t.Errorf("ReplicateISO() = %+v, want sha256=deadbeef", resp)
	}
}
