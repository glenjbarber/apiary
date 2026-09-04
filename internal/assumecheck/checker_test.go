package assumecheck

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/assumptions"
)

type fakeRaft struct {
	status    *internalpb.StatusResponse
	statusErr error
	vms       *internalpb.ListVMsResponse
	vmsErr    error
}

func (f *fakeRaft) Status(ctx context.Context) (*internalpb.StatusResponse, error) {
	return f.status, f.statusErr
}
func (f *fakeRaft) ListVMsLocal(ctx context.Context) (*internalpb.ListVMsResponse, error) {
	return f.vms, f.vmsErr
}

type hsResult struct {
	resp *rpcpb.HostStatsResponse
	err  error
}
type brResult struct {
	resp *rpcpb.GetLocalNetworkBridgeStatusResponse
	err  error
}

type fakePeers struct {
	mu              sync.Mutex
	hostStatsCalls  map[string]int
	hostStatsResult map[string]hsResult
	hostStatsDelay  time.Duration

	bridgeCalls  map[string]int
	bridgeResult map[string]brResult
}

func newFakePeers() *fakePeers {
	return &fakePeers{
		hostStatsCalls:  map[string]int{},
		hostStatsResult: map[string]hsResult{},
		bridgeCalls:     map[string]int{},
		bridgeResult:    map[string]brResult{},
	}
}

func (f *fakePeers) HostStats(ctx context.Context, addr string) (*rpcpb.HostStatsResponse, error) {
	f.mu.Lock()
	f.hostStatsCalls[addr]++
	f.mu.Unlock()
	if f.hostStatsDelay > 0 {
		select {
		case <-time.After(f.hostStatsDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r := f.hostStatsResult[addr]
	return r.resp, r.err
}

func (f *fakePeers) GetLocalNetworkBridgeStatus(ctx context.Context, addr, networkID string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error) {
	key := addr + "|" + networkID
	f.mu.Lock()
	f.bridgeCalls[key]++
	f.mu.Unlock()
	r := f.bridgeResult[key]
	return r.resp, r.err
}

type fakeRoute struct {
	iface    string
	hasRoute bool
	err      error
}

func (f *fakeRoute) DefaultRouteInterface(ctx context.Context) (string, bool, error) {
	return f.iface, f.hasRoute, f.err
}

func identityAddr(s string) string { return s }

func newTestChecker(t *testing.T, raft raftReader, peers peerChecker, route routeChecker, store *assumptions.Manager) *Checker {
	t.Helper()
	if store == nil {
		store = &assumptions.Manager{Path: t.TempDir() + "/assumptions.json"}
	}
	return &Checker{
		NodeID:            "node-a",
		Raft:              raft,
		Peers:             peers,
		PeerManagerdAddr:  identityAddr,
		Route:             route,
		Store:             store,
		HeartbeatInterval: time.Hour,
		RunDeadline:       20 * time.Second,
		HistoryLimit:      200,
		HistoryMaxAge:     30 * 24 * time.Hour,
	}
}

func findResult(t *testing.T, snap []assumptions.Result, kind string, subjectID, dependencyID string) assumptions.Result {
	t.Helper()
	for _, r := range snap {
		if r.Key.Kind == kind && r.Key.SubjectID == subjectID && r.Key.DependencyID == dependencyID {
			return r
		}
	}
	t.Fatalf("no result found for kind=%s subject=%s dependency=%s in %+v", kind, subjectID, dependencyID, snap)
	return assumptions.Result{}
}

func TestRunOnce_NATUplinkNotConfigured_IsNotApplicableNotUnknown(t *testing.T) {
	store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
	c := newTestChecker(t, &fakeRaft{status: &internalpb.StatusResponse{NodeId: "node-a"}, vms: &internalpb.ListVMsResponse{}}, nil, &fakeRoute{}, store)
	c.Uplink = ""

	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	snap, _, err := store.Load()
	must(t, err)
	r := findResult(t, snap, assumptions.KindNATUplinkDefaultRoute, "", "")
	if r.ObservedStatus != assumptions.StatusNotApplicable {
		t.Errorf("ObservedStatus = %v, want NotApplicable (never Unknown) when no uplink is configured", r.ObservedStatus)
	}
}

func TestRunOnce_NoDefaultRoute_IsFalse_ExecFailure_IsUnknown(t *testing.T) {
	tests := []struct {
		name       string
		route      *fakeRoute
		wantStatus assumptions.Status
		wantReason string
	}{
		{"no default route at all", &fakeRoute{hasRoute: false, err: nil}, assumptions.StatusFalse, ReasonNoDefaultRoute},
		{"exec failure", &fakeRoute{err: errors.New("route: not found")}, assumptions.StatusUnknown, ReasonRouteCheckFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
			c := newTestChecker(t, &fakeRaft{status: &internalpb.StatusResponse{NodeId: "node-a"}, vms: &internalpb.ListVMsResponse{}}, nil, tt.route, store)
			c.Uplink = "em0"
			must(t, c.RunOnce(context.Background()))
			snap, _, err := store.Load()
			must(t, err)
			r := findResult(t, snap, assumptions.KindNATUplinkDefaultRoute, "", "em0")
			if r.ObservedStatus != tt.wantStatus || r.ReasonCode != tt.wantReason {
				t.Errorf("got status=%v reason=%v, want %v/%v", r.ObservedStatus, r.ReasonCode, tt.wantStatus, tt.wantReason)
			}
		})
	}
}

func TestRunOnce_ListVMsLocalFailure_DoesNotSuppressOtherResults(t *testing.T) {
	store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
	raft := &fakeRaft{
		status: &internalpb.StatusResponse{NodeId: "node-a", Servers: []*internalpb.ServerInfo{
			{Id: "node-a"}, {Id: "node-b", Address: "10.0.0.2:8300"},
		}},
		vmsErr: errors.New("raftd unreachable"),
	}
	peers := newFakePeers()
	peers.hostStatsResult["10.0.0.2:8300"] = hsResult{resp: &rpcpb.HostStatsResponse{}}
	c := newTestChecker(t, raft, peers, &fakeRoute{iface: "em0", hasRoute: true}, store)
	c.Uplink = "em0"

	err := c.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce() error = nil, want the ListVMsLocal failure surfaced")
	}

	snap, _, loadErr := store.Load()
	must(t, loadErr)
	// (a) and (b) results must still be present despite (c)'s failure.
	findResult(t, snap, assumptions.KindPeerManagerRPCSucceeded, "", "node-b")
	findResult(t, snap, assumptions.KindNATUplinkDefaultRoute, "", "em0")
}

func TestRunOnce_PeerSecurityPathAccepted(t *testing.T) {
	baseServers := []*internalpb.ServerInfo{{Id: "node-a"}, {Id: "node-b", Address: "10.0.0.2:8300"}}

	t.Run("not applicable when neither TLS nor API key configured", func(t *testing.T) {
		store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
		peers := newFakePeers()
		peers.hostStatsResult["10.0.0.2:8300"] = hsResult{resp: &rpcpb.HostStatsResponse{}}
		c := newTestChecker(t, &fakeRaft{status: &internalpb.StatusResponse{NodeId: "node-a", Servers: baseServers}, vms: &internalpb.ListVMsResponse{}}, peers, &fakeRoute{}, store)
		must(t, c.RunOnce(context.Background()))
		snap, _, err := store.Load()
		must(t, err)
		r := findResult(t, snap, assumptions.KindPeerSecurityPathAccepted, "", "node-b")
		if r.ObservedStatus != assumptions.StatusNotApplicable {
			t.Errorf("ObservedStatus = %v, want NotApplicable", r.ObservedStatus)
		}
	})

	t.Run("true does not claim the peer enforces it", func(t *testing.T) {
		store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
		peers := newFakePeers()
		peers.hostStatsResult["10.0.0.2:8300"] = hsResult{resp: &rpcpb.HostStatsResponse{}}
		c := newTestChecker(t, &fakeRaft{status: &internalpb.StatusResponse{NodeId: "node-a", Servers: baseServers}, vms: &internalpb.ListVMsResponse{}}, peers, &fakeRoute{}, store)
		c.PeerAuthKeyConfigured = true
		must(t, c.RunOnce(context.Background()))
		snap, _, err := store.Load()
		must(t, err)
		r := findResult(t, snap, assumptions.KindPeerSecurityPathAccepted, "", "node-b")
		if r.ObservedStatus != assumptions.StatusTrue {
			t.Fatalf("ObservedStatus = %v, want True", r.ObservedStatus)
		}
		if !contains(r.Detail, "does not confirm") {
			t.Errorf("Detail = %q, must not claim the peer validated/enforces the security path", r.Detail)
		}
	})

	t.Run("unknown (not false) when the call fails for a non-auth reason", func(t *testing.T) {
		store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
		peers := newFakePeers()
		peers.hostStatsResult["10.0.0.2:8300"] = hsResult{err: status.Error(codes.Unavailable, "connection refused")}
		c := newTestChecker(t, &fakeRaft{status: &internalpb.StatusResponse{NodeId: "node-a", Servers: baseServers}, vms: &internalpb.ListVMsResponse{}}, peers, &fakeRoute{}, store)
		c.PeerAuthKeyConfigured = true
		must(t, c.RunOnce(context.Background()))
		snap, _, err := store.Load()
		must(t, err)
		r := findResult(t, snap, assumptions.KindPeerSecurityPathAccepted, "", "node-b")
		if r.ObservedStatus != assumptions.StatusUnknown {
			t.Errorf("ObservedStatus = %v, want Unknown - a transport failure proves nothing about credentials", r.ObservedStatus)
		}
	})

	t.Run("false when the call fails with an auth-specific code", func(t *testing.T) {
		store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
		peers := newFakePeers()
		peers.hostStatsResult["10.0.0.2:8300"] = hsResult{err: status.Error(codes.PermissionDenied, "role insufficient")}
		c := newTestChecker(t, &fakeRaft{status: &internalpb.StatusResponse{NodeId: "node-a", Servers: baseServers}, vms: &internalpb.ListVMsResponse{}}, peers, &fakeRoute{}, store)
		c.PeerAuthKeyConfigured = true
		must(t, c.RunOnce(context.Background()))
		snap, _, err := store.Load()
		must(t, err)
		r := findResult(t, snap, assumptions.KindPeerSecurityPathAccepted, "", "node-b")
		if r.ObservedStatus != assumptions.StatusFalse {
			t.Errorf("ObservedStatus = %v, want False", r.ObservedStatus)
		}
	})
}

func TestRunOnce_ReplicaBridgeError_IsUnknownNeverFalse(t *testing.T) {
	store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
	raft := &fakeRaft{
		status: &internalpb.StatusResponse{NodeId: "node-a", Servers: []*internalpb.ServerInfo{{Id: "node-a"}, {Id: "node-b", Address: "10.0.0.2:8300"}}},
		vms: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{
			{Id: "vm-1", NodeId: "node-a", ReplicaNodeId: "node-b", NetworkId: "net-1"},
		}},
	}
	peers := newFakePeers()
	peers.hostStatsResult["10.0.0.2:8300"] = hsResult{resp: &rpcpb.HostStatsResponse{BhyveConfigured: true}}
	peers.bridgeResult["10.0.0.2:8300|net-1"] = brResult{resp: &rpcpb.GetLocalNetworkBridgeStatusResponse{Error: "network not found locally"}}
	c := newTestChecker(t, raft, peers, &fakeRoute{}, store)

	must(t, c.RunOnce(context.Background()))
	snap, _, err := store.Load()
	must(t, err)
	r := findResult(t, snap, assumptions.KindReplicaNetworkBridgeUp, "vm-1", "node-b")
	if r.ObservedStatus != assumptions.StatusUnknown {
		t.Errorf("ObservedStatus = %v, want Unknown - an RPC error may reflect replication lag, never a definitive False", r.ObservedStatus)
	}
	if r.Key.SubjectKind != assumptions.SubjectKindVM {
		t.Errorf("SubjectKind = %v, want vm", r.Key.SubjectKind)
	}
}

func TestRunOnce_ReplicaChecks_DedupePerTick(t *testing.T) {
	store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
	raft := &fakeRaft{
		status: &internalpb.StatusResponse{NodeId: "node-a", Servers: []*internalpb.ServerInfo{{Id: "node-a"}, {Id: "node-b", Address: "10.0.0.2:8300"}}},
		vms: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{
			{Id: "vm-1", NodeId: "node-a", ReplicaNodeId: "node-b", NetworkId: "net-1"},
			{Id: "vm-2", NodeId: "node-a", ReplicaNodeId: "node-b", NetworkId: "net-1"},
			{Id: "vm-3", NodeId: "node-a", ReplicaNodeId: "node-b", NetworkId: "net-2"}, // different network - must still get its own call
		}},
	}
	peers := newFakePeers()
	peers.hostStatsResult["10.0.0.2:8300"] = hsResult{resp: &rpcpb.HostStatsResponse{BhyveConfigured: true}}
	peers.bridgeResult["10.0.0.2:8300|net-1"] = brResult{resp: &rpcpb.GetLocalNetworkBridgeStatusResponse{BridgeStatus: "up"}}
	peers.bridgeResult["10.0.0.2:8300|net-2"] = brResult{resp: &rpcpb.GetLocalNetworkBridgeStatusResponse{BridgeStatus: "up"}}
	c := newTestChecker(t, raft, peers, &fakeRoute{}, store)

	must(t, c.RunOnce(context.Background()))

	// 3 VMs share replica node-b: exactly one HostStats call to it, not 3.
	if got := peers.hostStatsCalls["10.0.0.2:8300"]; got != 1 {
		t.Errorf("HostStats calls to node-b = %d, want 1 (deduped per tick)", got)
	}
	// vm-1/vm-2 share (node-b, net-1): one call. vm-3 uses net-2: a
	// separate call. Two distinct (peer, network) pairs.
	if got := peers.bridgeCalls["10.0.0.2:8300|net-1"]; got != 1 {
		t.Errorf("bridge calls for net-1 = %d, want 1 (deduped across vm-1/vm-2)", got)
	}
	if got := peers.bridgeCalls["10.0.0.2:8300|net-2"]; got != 1 {
		t.Errorf("bridge calls for net-2 = %d, want 1 (vm-3 on a different network must still be checked)", got)
	}
}

func TestRunOnce_SlowPeerDoesNotStallTheTick(t *testing.T) {
	store := &assumptions.Manager{Path: t.TempDir() + "/a.json"}
	raft := &fakeRaft{
		status: &internalpb.StatusResponse{NodeId: "node-a", Servers: []*internalpb.ServerInfo{{Id: "node-a"}, {Id: "node-b", Address: "10.0.0.2:8300"}}},
		vms:    &internalpb.ListVMsResponse{},
	}
	peers := newFakePeers()
	peers.hostStatsDelay = 10 * time.Second // well past peerCheckTimeout (3s)
	c := newTestChecker(t, raft, peers, &fakeRoute{iface: "em0", hasRoute: true}, store)
	c.Uplink = "em0"
	c.RunDeadline = 5 * time.Second

	start := time.Now()
	_ = c.RunOnce(context.Background())
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Errorf("RunOnce took %v, want it bounded by peerCheckTimeout (~3s), not the peer's 10s delay", elapsed)
	}

	snap, _, err := store.Load()
	must(t, err)
	// (b) must still have been recorded even though (a)'s peer call was slow.
	findResult(t, snap, assumptions.KindNATUplinkDefaultRoute, "", "em0")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
