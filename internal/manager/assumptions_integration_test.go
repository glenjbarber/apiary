package manager

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/assumecheck"
	"github.com/glenjbarber/apiary/internal/assumptions"
	"github.com/glenjbarber/apiary/internal/isostore"
)

// fakeRouteChecker always reports iface as the default route interface,
// without shelling out to real route(8) - keeps these tests independent
// of the host they run on.
type fakeRouteChecker struct{ iface string }

func (f fakeRouteChecker) DefaultRouteInterface(context.Context) (string, bool, error) {
	return f.iface, true, nil
}

func TestIntegration_GetLocalNetworkBridgeStatus_ReflectsLocalVLANState(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	bridge := resolveBridgeName(&internalpb.NetworkDefinition{Id: "net-1"})
	vlan := &fakeVLANStatus{up: map[string]bool{bridge: true}}
	client := newManagerdRPCClientFull(t, raftdSocket, "node-a", nil, nil, vlan, nil, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-1", Subnet: "10.60.0.0/24"},
	}); err != nil {
		t.Fatalf("CreateNetwork() error: %v", err)
	}
	if _, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-2", Subnet: "10.61.0.0/24"},
	}); err != nil {
		t.Fatalf("CreateNetwork() (net-2) error: %v", err)
	}

	upResp, err := client.GetLocalNetworkBridgeStatus(ctx, &rpcpb.GetLocalNetworkBridgeStatusRequest{NetworkId: "net-1"})
	if err != nil {
		t.Fatalf("GetLocalNetworkBridgeStatus(net-1) error: %v", err)
	}
	if upResp.GetBridgeStatus() != "up" {
		t.Errorf("net-1 bridge status = %q, want up", upResp.GetBridgeStatus())
	}

	downResp, err := client.GetLocalNetworkBridgeStatus(ctx, &rpcpb.GetLocalNetworkBridgeStatusRequest{NetworkId: "net-2"})
	if err != nil {
		t.Fatalf("GetLocalNetworkBridgeStatus(net-2) error: %v", err)
	}
	if downResp.GetBridgeStatus() != "unknown" {
		t.Errorf("net-2 bridge status = %q, want unknown (bridge doesn't exist on this node)", downResp.GetBridgeStatus())
	}
}

func TestIntegration_GetLocalNetworkBridgeStatus_UnknownNetworkID(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.GetLocalNetworkBridgeStatus(ctx, &rpcpb.GetLocalNetworkBridgeStatusRequest{NetworkId: "ghost"})
	if err != nil {
		t.Fatalf("GetLocalNetworkBridgeStatus() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Error("GetLocalNetworkBridgeStatus() for an unknown network_id must set error, not look like a clean \"unknown\" bridge status")
	}
}

func TestIntegration_ListAssumptionResults_NilStoreReturnsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket) // no assumptions store configured

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.ListAssumptionResults(ctx, &rpcpb.ListAssumptionResultsRequest{})
	if err != nil {
		t.Fatalf("ListAssumptionResults() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Error("ListAssumptionResults() with no store configured must set error, not panic or look empty")
	}
}

func TestIntegration_ListAssumptionResults_StaleCollapsesToUnknown(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	store := &assumptions.Manager{Path: t.TempDir() + "/assumptions.json"}
	client := newManagerdRPCClientFull(t, raftdSocket, "node-a", nil, nil, nil, store, time.Minute)

	old := time.Now().Add(-time.Hour)
	key := assumptions.Key{Kind: assumptions.KindNATUplinkDefaultRoute, SubjectKind: assumptions.SubjectKindNode, DependencyID: "em0"}
	if err := store.Append([]assumptions.Result{{Key: key, ObservedStatus: assumptions.StatusTrue, LastObservedAt: old}}, time.Hour, 50, 24*time.Hour); err != nil {
		t.Fatalf("store.Append() error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.ListAssumptionResults(ctx, &rpcpb.ListAssumptionResultsRequest{})
	if err != nil {
		t.Fatalf("ListAssumptionResults() error: %v", err)
	}
	if len(resp.GetLatest()) != 1 {
		t.Fatalf("latest count = %d, want 1", len(resp.GetLatest()))
	}
	r := resp.GetLatest()[0]
	if !r.GetStale() {
		t.Error("Stale = false, want true (last_observed_at is 1h old against a 1m stale-after)")
	}
	if r.GetStatus() != rpcpb.AssumptionStatus_ASSUMPTION_STATUS_UNKNOWN {
		t.Errorf("effective Status = %v, want UNKNOWN despite a stored TRUE value - this is the core staleness-safety regression test", r.GetStatus())
	}
	if r.GetObservedStatus() != rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE {
		t.Errorf("ObservedStatus = %v, want TRUE - the raw stored value must still be visible for diagnosis", r.GetObservedStatus())
	}
}

// TestIntegration_AssumptionChecker_ReplicaChecks_EndToEnd wires a real
// assumecheck.Checker against a genuine raftd + managerd pair - a VM
// owned by this node is self-referentially replicated to the SAME node
// (a real 2-node raft cluster would be needed for a genuinely distinct
// replica target, mirroring this project's own accepted precedent of
// covering that class of scenario via live verification rather than a
// fabricated fake - see ADR-0035's own disclosed gap), but this still
// exercises the entire real pipeline for real: Raft.ListVMsLocal -> a
// real raft server address lookup -> PeerManagerdAddr -> a real
// PeerReporter round trip to a real managerd's HostStats/
// GetLocalNetworkBridgeStatus RPCs -> a real assumptions.Manager file ->
// read back through the real ListAssumptionResults RPC.
func TestIntegration_AssumptionChecker_ReplicaChecks_EndToEnd(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)

	raftClient, err := Dial(raftdSocket, "")
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })

	statusResp, err := raftClient.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	localNodeID := statusResp.GetNodeId()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(tcp) error: %v", err)
	}
	bridge := resolveBridgeName(&internalpb.NetworkDefinition{Id: "net-1"})
	vlan := &fakeVLANStatus{up: map[string]bool{bridge: true}}
	store := &assumptions.Manager{Path: t.TempDir() + "/assumptions.json"}
	peers := NewPeerReporter("", false, nil)

	srv := NewServer(raftClient, "manager-1", isostore.New(t.TempDir()), nil, nil, vlan, peers, "", nil, nil, store, time.Hour, nil)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(srv.AuthUnaryInterceptor),
		grpc.StreamInterceptor(srv.AuthStreamInterceptor),
	)
	rpcpb.RegisterManagerServiceServer(grpcServer, srv)
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.GracefulStop)

	_, managerdPort, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort() error: %v", err)
	}

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient() error: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	client := rpcpb.NewManagerServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-1", Subnet: "10.60.0.0/24"},
	}); err != nil {
		t.Fatalf("CreateNetwork() error: %v", err)
	}
	if _, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1", NodeId: localNodeID, ReplicaNodeId: localNodeID, NetworkId: "net-1"},
	}); err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}

	checker := &assumecheck.Checker{
		NodeID: localNodeID,
		Raft:   raftClient,
		Peers:  peers,
		PeerManagerdAddr: func(raftAddr string) string {
			host, _, err := net.SplitHostPort(raftAddr)
			if err != nil {
				host = raftAddr
			}
			return net.JoinHostPort(host, managerdPort)
		},
		Route:             fakeRouteChecker{iface: "em0"},
		Uplink:            "em0",
		Store:             store,
		HeartbeatInterval: time.Hour,
		RunDeadline:       10 * time.Second,
		HistoryLimit:      50,
		HistoryMaxAge:     24 * time.Hour,
	}
	if err := checker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	resp, err := client.ListAssumptionResults(ctx, &rpcpb.ListAssumptionResultsRequest{})
	if err != nil {
		t.Fatalf("ListAssumptionResults() error: %v", err)
	}
	var bhyveResult, bridgeResult *rpcpb.AssumptionResult
	for _, r := range resp.GetLatest() {
		switch r.GetKey().GetKind() {
		case rpcpb.AssumptionKind_ASSUMPTION_KIND_REPLICA_BHYVE_CONFIGURED:
			bhyveResult = r
		case rpcpb.AssumptionKind_ASSUMPTION_KIND_REPLICA_NETWORK_BRIDGE_UP:
			bridgeResult = r
		}
	}
	if bhyveResult == nil {
		t.Fatal("no REPLICA_BHYVE_CONFIGURED result found in ListAssumptionResults")
	}
	if bhyveResult.GetStatus() != rpcpb.AssumptionStatus_ASSUMPTION_STATUS_FALSE {
		t.Errorf("bhyve result status = %v, want FALSE (this managerd has no bhyve configured - real HostStats round trip)", bhyveResult.GetStatus())
	}
	if bridgeResult == nil {
		t.Fatal("no REPLICA_NETWORK_BRIDGE_UP result found in ListAssumptionResults")
	}
	if bridgeResult.GetStatus() != rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE {
		t.Errorf("bridge result status = %v, want TRUE (net-1's bridge is up per the fake VLANStatus - real GetLocalNetworkBridgeStatus round trip)", bridgeResult.GetStatus())
	}
}

// TestIntegration_AssumptionChecker_JailReplicaProducesNoResults confirms
// a jail with a replica target set produces ZERO assumption-(c) results
// - not NotApplicable, not Unknown, silence - against a real raftd, no
// fakes involved for the raft half of this behavior.
func TestIntegration_AssumptionChecker_JailReplicaProducesNoResults(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)

	raftClient, err := Dial(raftdSocket, "")
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })

	statusResp, err := raftClient.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	localNodeID := statusResp.GetNodeId()

	client := newManagerdRPCClient(t, raftdSocket)
	store := &assumptions.Manager{Path: t.TempDir() + "/assumptions.json"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.CreateJail(ctx, &rpcpb.CreateJailRequest{
		Jail: &rpcpb.JailDefinition{Id: "jail-1", Name: "web-1", Hostname: "web-1.local", NodeId: localNodeID, ReplicaNodeId: localNodeID},
	}); err != nil {
		t.Fatalf("CreateJail() error: %v", err)
	}

	checker := &assumecheck.Checker{
		NodeID:            localNodeID,
		Raft:              raftClient,
		Peers:             nil,
		PeerManagerdAddr:  func(s string) string { return s },
		Route:             fakeRouteChecker{},
		Uplink:            "",
		Store:             store,
		HeartbeatInterval: time.Hour,
		RunDeadline:       10 * time.Second,
		HistoryLimit:      50,
		HistoryMaxAge:     24 * time.Hour,
	}
	if err := checker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	snap, _, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load() error: %v", err)
	}
	for _, r := range snap {
		if r.Key.Kind == assumptions.KindReplicaBhyveConfigured || r.Key.Kind == assumptions.KindReplicaNetworkBridgeUp {
			t.Errorf("a jail with replica_node_id must produce NO assumption-(c) results, got: %+v", r)
		}
	}
}
