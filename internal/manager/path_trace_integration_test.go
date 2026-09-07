package manager

import (
	"context"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestIntegration_TraceCellPath_OnLinkPathShowsGuestEvidenceGap(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	bridge := resolveBridgeName(&internalpb.NetworkDefinition{Id: "net-1"})
	vlan := &fakeVLANStatus{up: map[string]bool{bridge: true}}
	client := newManagerdRPCClientFull(t, raftdSocket, "raftd-1", nil, nil, vlan, nil, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resp, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{Network: &rpcpb.NetworkDefinition{
		Id: "net-1", Name: "services", Subnet: "10.60.0.0/24",
	}}); err != nil || resp.GetError() != "" {
		t.Fatalf("CreateNetwork() = %+v, err=%v", resp, err)
	}
	if resp, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{Vm: &rpcpb.VMDefinition{
		Id: "vm-1", Name: "frontend", NodeId: "raftd-1", NetworkId: "net-1",
		DesiredState: rpcpb.VMState_VM_STATE_RUNNING,
	}}); err != nil || resp.GetError() != "" {
		t.Fatalf("CreateVM() = %+v, err=%v", resp, err)
	}
	if resp, err := client.ReportVMPhase(ctx, &rpcpb.ReportVMPhaseRequest{
		Id: "vm-1", Phase: rpcpb.VMPhase_VM_PHASE_READY,
	}); err != nil || resp.GetError() != "" {
		t.Fatalf("ReportVMPhase() = %+v, err=%v", resp, err)
	}

	resp, err := client.TraceCellPath(ctx, &rpcpb.TraceCellPathRequest{
		CellId: "vm-1", Destination: "10.60.0.20", Protocol: "tcp", Port: 443,
	})
	if err != nil {
		t.Fatalf("TraceCellPath() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("TraceCellPath() returned error: %s", resp.GetError())
	}
	if resp.GetStatus() != rpcpb.PathTraceStatus_PATH_TRACE_STATUS_UNKNOWN {
		t.Fatalf("TraceCellPath() = %+v, want unknown", resp)
	}
	if resp.GetActiveProbe() {
		t.Fatal("active_probe = true, want false")
	}
	if len(resp.GetSteps()) < 9 {
		t.Fatalf("step count = %d, want a complete ordered trace", len(resp.GetSteps()))
	}
	if resp.GetSummary() != "First evidence gap: Tap attachment: Current tap attachment is not observed" {
		t.Fatalf("summary = %q, want explicit tap evidence gap", resp.GetSummary())
	}
}

func TestIntegration_TraceCellPath_UnknownCellReturnsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.TraceCellPath(ctx, &rpcpb.TraceCellPathRequest{
		CellId: "missing", Destination: "10.60.0.20",
	})
	if err != nil {
		t.Fatalf("TraceCellPath() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("TraceCellPath(missing) error = empty, want explicit rejection")
	}
}
