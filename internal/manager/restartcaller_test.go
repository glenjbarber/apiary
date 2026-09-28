package manager

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/guardrail"
	"github.com/glenjbarber/apiary/internal/isostore"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// These tests drive ExecuteNodeRestartPlan - ADR-0145's controlled
// restart - through a real ManagerService entry point, against real
// dependencies wherever a real one exists.
//
// What is REAL here, and it is most of the path:
//
//   - a real raft cluster on loopback (raftnode.Node, Bootstrap,
//     AddVoter), so membership, leadership, the quorum arithmetic and a
//     genuine LeadershipTransfer() are all real;
//   - a real RaftInternal gRPC server over a real Unix socket per node,
//     so the step-aside goes over the wire to the real raftd;
//   - a real managerd Server per node, with a real RaftClient, served on
//     a real gRPC listener behind the real auth interceptors, so the
//     restart-guardrail token check, the leader forward of the lease
//     reservation, and the FSM apply are all real;
//   - the real restartplan.Engine, the real restartStepAsider, the real
//     pending record on disk, the real durable result files.
//
// What is NOT real, and exactly one thing: the peer DIAL.
//
// internal/manager's own peerManagerdAddrOf derives a peer's managerd
// address by taking the HOST out of its raft bind address and substituting
// a single configured managerd port. That is correct in production - one
// managerd per host - and it makes a three-node cluster unreproducible on
// one machine: three loopback raft addresses all reduce to the same
// 127.0.0.1:17700, so three managerds cannot all be addressed.
// planHarnessForwarder below therefore replaces the dial and nothing
// else. It routes to the same node the production path would have
// dialled, found the same way - the leader hint, or the request's own
// target node id - over a real gRPC connection to that node's real
// listener, with the real token attached. The address resolution itself
// is pinned by TestManagerdAddrFromStatus, and the dial by
// TestPeerReporterExecuteNodeRestartPlan_PresentsTheTokenOverARealDial.
//
// The other fake stands in for the rc.d service controller, because
// `service apiary_raftd restart` cannot be run on a test machine. It
// simulates the RESTARTED PROCESS and nothing else: on a successful
// restart it does exactly what cmd/raftd's own startup hook does, which
// is make the confirmation and then write down what it established. It
// never stands in for managerd, which is the whole point of the design -
// the plan waits for the restarted process's own account of itself and
// never confirms on its behalf.

// planHarnessToken is the cluster's restart-guardrail token. It is
// presented by planHarnessForwarder on every forwarded call, so a test
// that reaches a handler through a forward is also a test that the token
// is actually on the wire.
const planHarnessToken = "plan-harness-restart-guardrail-token"

// planHarness is an n-node Apiary cluster: raft plus a managerd on each,
// all real, all on loopback.
type planHarness struct {
	t *testing.T

	ids     []string
	nodes   []*raftnode.Node
	servers []*Server
	// addrs[i] is where nodes[i]'s managerd gRPC server is listening.
	addrs   []string
	clients []rpcpb.ManagerServiceClient
	// controllers[i] is the rc.d stand-in on nodes[i].
	controllers []*planRestartSimulator

	forwarder *planHarnessForwarder
}

// planRestartSimulator stands in for `service <name> restart` and, more
// importantly, for the process that command restarts.
//
// On a successful restart it replays cmd/raftd's own startup hook
// (cmd/raftd/confirm.go): read the pending record, make the real
// confirmation against the real FSM, and only then write down what was
// established. That ordering is the point. It is the ONLY thing in this
// file permitted to make the confirmation, because in production only the
// restarted process may - ADR-0103's revision note #16 and ADR-0125 §2
// both say so, and a plan that confirmed for raftd would be letting a
// witness testify to its own resurrection.
//
// confirmOnRestart=false models a raftd that came back and could not
// confirm - no guardrail token on the node, say - which is the honest
// unobserved case rather than a failure.
type planRestartSimulator struct {
	mu sync.Mutex

	// pendingDir is this node's pending-restart directory, the same one
	// the Engine wrote the record into before calling Restart.
	pendingDir string
	// selfReportDir is where this node's restarted process writes down
	// what it established.
	selfReportDir string

	srv *Server

	restartErr       error
	confirmOnRestart bool

	restarted []string
}

func (s *planRestartSimulator) List(context.Context) ([]*rpcpb.NodeService, error) {
	return nil, nil
}

func (s *planRestartSimulator) Restart(ctx context.Context, name string) error {
	s.mu.Lock()
	s.restarted = append(s.restarted, name)
	err := s.restartErr
	confirm := s.confirmOnRestart
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if !confirm {
		return nil
	}

	// Everything below is cmd/raftd's own startup hook, in its own
	// order. The plan wrote the pending record before calling here, which
	// is what makes it findable.
	pending, found, lerr := restartplan.NewPendingStore(s.pendingDir).Load(name)
	if lerr != nil || !found {
		return nil
	}
	if _, cerr := s.srv.ConfirmRestartCompletedLocal(ctx, pending.Service, pending.NodeID, pending.LeaseID); cerr != nil {
		return nil
	}
	now := time.Now().Unix()
	_ = restartplan.NewResultStore(s.selfReportDir).Save(restartplan.Result{
		Service:        pending.Service,
		NodeID:         pending.NodeID,
		LeaseID:        pending.LeaseID,
		Outcome:        restartplan.OutcomeConfirmed,
		Detail:         "the restarted process reported itself back healthy and the restart lease was released",
		Evidence:       []string{fmt.Sprintf("lease %d for %s on %s confirmed on attempt 1", pending.LeaseID, pending.Service, pending.NodeID)},
		StartedAtUnix:  now,
		FinishedAtUnix: now,
	})
	_ = restartplan.NewPendingStore(s.pendingDir).Clear(name)
	return nil
}

func (s *planRestartSimulator) restartCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.restarted)
}

func (s *planRestartSimulator) restartedNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.restarted...)
}

// setBehaviour reconfigures the simulator for a scenario, under the same
// lock Restart reads it under.
func (s *planRestartSimulator) setBehaviour(confirmOnRestart bool, restartErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmOnRestart = confirmOnRestart
	s.restartErr = restartErr
}

// planHarnessForwarder replaces the peer dial and nothing else.
//
// It embeds the nil PeerForwarder, so any method the plan does not
// actually call panics rather than silently returning a zero value - the
// same discipline fakeISOPeerForwarder and fakeFailingPeerForwarder
// already use in this package. If a future edit routes a new call through
// here, the test fails loudly instead of quietly exercising a stub.
type planHarnessForwarder struct {
	PeerForwarder

	h *planHarness
}

func (f *planHarnessForwarder) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+planHarnessToken)
}

// leaderClient finds the node the production path would have dialled for
// a leader-hint forward: the current leader, reached the same way
// reserveRestartLease reaches it.
func (f *planHarnessForwarder) leaderClient() (rpcpb.ManagerServiceClient, string, error) {
	for i, node := range f.h.nodes {
		if node.Status().IsLeader {
			return f.h.clients[i], f.h.ids[i], nil
		}
	}
	return nil, "", fmt.Errorf("no leader among the harness nodes")
}

func (f *planHarnessForwarder) clientForNode(nodeID string) (rpcpb.ManagerServiceClient, error) {
	for i, id := range f.h.ids {
		if id == nodeID {
			return f.h.clients[i], nil
		}
	}
	return nil, fmt.Errorf("the harness has no managerd for node %q", nodeID)
}

func (f *planHarnessForwarder) ReserveRestartLease(ctx context.Context, _ string, req *rpcpb.ReserveRestartLeaseRequest) (*rpcpb.ReserveRestartLeaseResponse, error) {
	c, _, err := f.leaderClient()
	if err != nil {
		return nil, err
	}
	return c.ReserveRestartLease(f.ctx(ctx), req)
}

func (f *planHarnessForwarder) ConfirmRestartCompleted(ctx context.Context, _ string, req *rpcpb.ConfirmRestartCompletedRequest) (*rpcpb.ConfirmRestartCompletedResponse, error) {
	c, _, err := f.leaderClient()
	if err != nil {
		return nil, err
	}
	return c.ConfirmRestartCompleted(f.ctx(ctx), req)
}

func (f *planHarnessForwarder) StepAsideForRestart(ctx context.Context, _ string, req *rpcpb.StepAsideForRestartRequest) (*rpcpb.StepAsideForRestartResponse, error) {
	c, err := f.clientForNode(req.GetTargetNodeId())
	if err != nil {
		return nil, err
	}
	return c.StepAsideForRestart(f.ctx(ctx), req)
}

func (f *planHarnessForwarder) ExecuteNodeRestartPlan(ctx context.Context, _ string, req *rpcpb.ExecuteNodeRestartPlanRequest) (*rpcpb.ExecuteNodeRestartPlanResponse, error) {
	c, err := f.clientForNode(req.GetNodeId())
	if err != nil {
		return nil, err
	}
	return c.ExecuteNodeRestartPlan(f.ctx(ctx), req)
}

// newPlanHarness starts a real n-node Apiary cluster on loopback.
//
// The confirmation budget is turned right down (2 attempts, 20ms apart)
// because these tests drive the confirmation synchronously from the
// restart simulator. The production budget is 10 attempts 3s apart, and
// TestExecuteNodeRestartPlan_ProductionConfirmBudgetIsTheDocumentedOne
// pins that separately, so shrinking it here is not the same as never
// having checked it.
func newPlanHarness(t *testing.T, n int) *planHarness {
	t.Helper()
	h := &planHarness{t: t}

	ids := make([]string, n)
	nodes := make([]*raftnode.Node, n)
	bindAddrs := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("comb-%d", i+1)
		bindAddrs[i] = freeLoopbackAddr(t)
		node, err := raftnode.New(raftnode.Config{NodeID: ids[i], DataDir: t.TempDir(), BindAddr: bindAddrs[i]})
		if err != nil {
			t.Fatalf("raftnode.New(%s): %v", ids[i], err)
		}
		nodes[i] = node
		t.Cleanup(func() { _ = node.Shutdown() })
	}
	if err := nodes[0].Bootstrap(); err != nil {
		t.Fatalf("Bootstrap(): %v", err)
	}
	eventually(t, 10*time.Second, func() bool { return nodes[0].Status().IsLeader })
	for i := range nodes[1:] {
		if err := nodes[0].AddVoter(ids[i+1], bindAddrs[i+1], 0, 5*time.Second); err != nil {
			t.Fatalf("AddVoter(%s): %v", ids[i+1], err)
		}
	}
	eventually(t, 10*time.Second, func() bool { return len(nodes[0].Status().Servers) == n })

	for i := range nodes {
		// A short socket path: t.TempDir()'s test-name-derived path can
		// push a Unix socket over the platform's path length limit.
		socketDir, err := os.MkdirTemp("", "planharness-uds")
		if err != nil {
			t.Fatalf("MkdirTemp(): %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(socketDir) })
		socketPath := filepath.Join(socketDir, "raftd.sock")
		lis, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("Listen(unix): %v", err)
		}
		raftGRPC := grpc.NewServer()
		internalpb.RegisterRaftInternalServer(raftGRPC, raftnode.NewServer(nodes[i]))
		go raftGRPC.Serve(lis)
		t.Cleanup(raftGRPC.GracefulStop)

		raftClient, err := Dial(socketPath, "")
		if err != nil {
			t.Fatalf("Dial(%s): %v", socketPath, err)
		}
		t.Cleanup(func() { _ = raftClient.Close() })

		srv := NewServer(raftClient, ids[i], isostore.New(t.TempDir()), nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
		srv.SetRestartGuardrailToken(planHarnessToken)
		pendingDir := t.TempDir()
		srv.SetRestartConfirmStore(NewRestartConfirmStore(pendingDir))
		coordinatorDir := t.TempDir()
		selfReportDir := t.TempDir()
		srv.SetNodeRestartPlanDirs(coordinatorDir, selfReportDir, 2, 20*time.Millisecond)

		sim := &planRestartSimulator{
			pendingDir:       pendingDir,
			selfReportDir:    selfReportDir,
			srv:              srv,
			confirmOnRestart: true,
		}
		srv.services = sim

		mdLis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen(tcp): %v", err)
		}
		mdGRPC := grpc.NewServer(
			grpc.UnaryInterceptor(srv.AuthUnaryInterceptor),
			grpc.StreamInterceptor(srv.AuthStreamInterceptor),
		)
		rpcpb.RegisterManagerServiceServer(mdGRPC, srv)
		go mdGRPC.Serve(mdLis)
		t.Cleanup(mdGRPC.GracefulStop)

		conn, err := grpc.NewClient(mdLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("grpc.NewClient(): %v", err)
		}
		t.Cleanup(func() { conn.Close() })

		h.ids = append(h.ids, ids[i])
		h.nodes = append(h.nodes, nodes[i])
		h.servers = append(h.servers, srv)
		h.addrs = append(h.addrs, mdLis.Addr().String())
		h.clients = append(h.clients, rpcpb.NewManagerServiceClient(conn))
		h.controllers = append(h.controllers, sim)
	}

	h.forwarder = &planHarnessForwarder{h: h}
	for _, srv := range h.servers {
		srv.peers = h.forwarder
	}
	return h
}

func (h *planHarness) leaderIndex(t *testing.T) int {
	t.Helper()
	for i, node := range h.nodes {
		if node.Status().IsLeader {
			return i
		}
	}
	t.Fatal("no leader among the harness nodes")
	return -1
}

// heldLease reads this node's own FSM copy of the lease state for
// service, so a test asserts what the cluster is actually still blocking
// on rather than trusting a response.
func (h *planHarness) heldLease(t *testing.T, i int, service string) *internalpb.RestartLease {
	t.Helper()
	return heldLeaseOf(t, h.servers[i], service)
}

func heldLeaseOf(t *testing.T, srv *Server, service string) *internalpb.RestartLease {
	t.Helper()
	resp, err := srv.raft.GetRestartLeaseStateLocal(context.Background(), service)
	if err != nil {
		t.Fatalf("GetRestartLeaseStateLocal(%s): %v", service, err)
	}
	return resp.GetLease()
}

// tokenCtx is an outgoing context carrying the cluster's guardrail token,
// for calls a test makes directly rather than through a forward.
func tokenCtx() context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+planHarnessToken))
}

func planRequest(nodeID string, force bool) *rpcpb.ExecuteNodeRestartPlanRequest {
	return &rpcpb.ExecuteNodeRestartPlanRequest{NodeId: nodeID, Service: raftdServiceName, Force: force}
}

// eventuallyLeaseHeld waits (bounded) for this node's own replicated state
// to show the lease a run reserved, and returns it.
//
// The wait is not politeness. The plan's target is usually a node that
// just handed over leadership, which makes it a FOLLOWER, and a
// follower's FSM copy trails the leader's by whatever replication costs.
// Reading it the instant the RPC returns can therefore miss a lease that
// was genuinely applied - a flaky test that would eventually be reported
// as a real finding, which is worse than no test.
func eventuallyLeaseHeld(t *testing.T, srv *Server, wantLeaseID uint64) *internalpb.RestartLease {
	t.Helper()
	var seen *internalpb.RestartLease
	eventually(t, 10*time.Second, func() bool {
		seen = heldLeaseOf(t, srv, raftdServiceName)
		return seen != nil
	})
	if wantLeaseID != 0 && seen.GetLeaseId() != wantLeaseID {
		t.Fatalf("the lease this node shows is %d, want %d", seen.GetLeaseId(), wantLeaseID)
	}
	return seen
}

// assertLeaseHeldOrGone is the invariant every run has to keep: a run
// that reserved nothing must not have left a lease behind, and a run that
// reserved one and got no confirmation must still be holding it. A plan
// that quietly released a lease nobody confirmed would let a second Comb
// restart while the first was still unconfirmed, which is the one thing
// this mechanism exists to prevent.
func assertLeaseHeldOrGone(t *testing.T, srv *Server, wantLeaseID uint64, wantHeld bool) {
	t.Helper()
	lease := heldLeaseOf(t, srv, raftdServiceName)
	if wantHeld {
		if lease == nil {
			t.Fatalf("no restart lease is held for %s, but one should be (this run's lease id was %d)", raftdServiceName, wantLeaseID)
		}
		if lease.GetLeaseId() != wantLeaseID {
			t.Fatalf("held lease id = %d, want this run's own %d", lease.GetLeaseId(), wantLeaseID)
		}
		return
	}
	if lease != nil {
		t.Fatalf("a restart lease for %s is held by %q (id %d), but this run should have reserved none",
			raftdServiceName, lease.GetHolderNodeId(), lease.GetLeaseId())
	}
}

// ---------------------------------------------------------------------------
// The token gate, and the refusals that cost nothing to state.
// ---------------------------------------------------------------------------

// TestExecuteNodeRestartPlan_TokenGate proves the call is unreachable by
// any ordinary credential, which is the whole reason it is exempted from
// checkAuth rather than given an Admin tier. An empty configured token
// and an empty presented token must both be refused: a node with no token
// provisioned must never be the one node that accepts everything.
func TestExecuteNodeRestartPlan_TokenGate(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
	srv.SetRestartGuardrailToken(planHarnessToken)
	srv.services = &fakeNodeServiceController{}

	req := planRequest("raftd-1", false)

	if _, err := client.ExecuteNodeRestartPlan(context.Background(), req); err == nil {
		t.Error("ExecuteNodeRestartPlan() with no bearer token = no error, want PermissionDenied")
	} else if status.Code(err) != codes.PermissionDenied {
		t.Errorf("no-token error code = %v, want PermissionDenied (%v)", status.Code(err), err)
	}

	for _, presented := range []string{"", planHarnessToken + "x", "an-admin-api-key-would-go-here"} {
		ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+presented))
		if _, err := client.ExecuteNodeRestartPlan(ctx, req); err == nil {
			t.Errorf("ExecuteNodeRestartPlan() with bearer %q = no error, want PermissionDenied", presented)
		} else if status.Code(err) != codes.PermissionDenied {
			t.Errorf("bearer %q error code = %v, want PermissionDenied", presented, status.Code(err))
		}
	}

	// A freshly-minted Admin API key must not work either. The dedicated
	// token exists precisely so that no CreateAPIKey-issued credential,
	// however privileged, can restart a quorum-critical daemon.
	createResp, err := client.CreateAPIKey(context.Background(), &rpcpb.CreateAPIKeyRequest{Name: "plan-admin", Role: "admin"})
	if err != nil || createResp.GetError() != "" {
		t.Fatalf("CreateAPIKey() = (%+v, %v)", createResp, err)
	}
	adminCtx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+createResp.GetRawKey()))
	if _, err := client.ExecuteNodeRestartPlan(adminCtx, req); err == nil {
		t.Error("ExecuteNodeRestartPlan() with a freshly-minted Admin API key = no error, want PermissionDenied")
	}

	// And the real token does get past the gate. Without this the test
	// would also pass if the handler refused everything.
	realCtx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+planHarnessToken))
	resp, err := client.ExecuteNodeRestartPlan(realCtx, req)
	if err != nil {
		t.Fatalf("ExecuteNodeRestartPlan() with the guardrail token: %v", err)
	}
	if resp.GetError() == "" && resp.GetResult() == nil {
		t.Error("ExecuteNodeRestartPlan() with the real token returned neither a result nor an error; the gate is not the only thing under test")
	}
}

// TestExecuteNodeRestartPlan_RefusesWhatItCannotDo covers the two
// refusals that must never be a silent reinterpretation:
// apiary_managerd, and anything that is not raftd.
//
// ADR-0142's refusal is asserted by identity, not by substring. A test
// that grepped for "managerd" would keep passing if the message drifted
// into something vaguer, and the exact wording is what points an operator
// at the one path that works.
func TestExecuteNodeRestartPlan_RefusesWhatItCannotDo(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
	srv.SetRestartGuardrailToken(planHarnessToken)
	srv.services = &fakeNodeServiceController{}
	ctx := tokenCtx()

	resp, err := client.ExecuteNodeRestartPlan(ctx, &rpcpb.ExecuteNodeRestartPlanRequest{NodeId: "raftd-1", Service: managerdServiceName})
	if err != nil {
		t.Fatalf("ExecuteNodeRestartPlan(managerd): %v", err)
	}
	if resp.GetError() != managerdSelfRestartRefusal {
		t.Errorf("ExecuteNodeRestartPlan(apiary_managerd) error = %q, want ADR-0142's own refusal verbatim (%q)", resp.GetError(), managerdSelfRestartRefusal)
	}
	if resp.GetResult() != nil {
		t.Errorf("ExecuteNodeRestartPlan(apiary_managerd) returned a result as well as an error: %+v; a refusal that never ran must not carry a verdict", resp.GetResult())
	}

	for _, service := range []string{"apiary_frontend", "apiary_restshimd", "sshd", ""} {
		resp, err := client.ExecuteNodeRestartPlan(ctx, &rpcpb.ExecuteNodeRestartPlanRequest{NodeId: "raftd-1", Service: service})
		if err != nil {
			t.Fatalf("ExecuteNodeRestartPlan(%q): %v", service, err)
		}
		if resp.GetError() == "" {
			t.Errorf("ExecuteNodeRestartPlan(%q) was not refused", service)
		}
		if service != "" && !strings.Contains(resp.GetError(), raftdServiceName) {
			t.Errorf("ExecuteNodeRestartPlan(%q) error does not name the one service this plan covers: %q", service, resp.GetError())
		}
	}
}

// TestExecuteNodeRestartPlan_RefusesBeforeTouchingAnythingWhenMisconfigured
// covers the two preconditions. Both refuse before any step runs, so
// neither can strand a lease or write a record implying an attempt was
// made.
//
// The no-raft-client case is the more interesting one, because
// restartplan's own posture for a nil StepAsider is to SKIP the step and
// let the leader-restart guardrail catch a leader-target behind an
// operator acknowledgment. That degradation is right for a caller who
// wants the old behaviour and wrong here: this entry point exists to
// promise a real confirmed step-aside, and quietly running without one
// would turn the stated guarantee into an unstated fallback.
func TestExecuteNodeRestartPlan_RefusesBeforeTouchingAnythingWhenMisconfigured(t *testing.T) {
	t.Run("no raft client", func(t *testing.T) {
		raftdSocket := newRaftdUDSSocket(t)
		client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
		srv.SetRestartGuardrailToken(planHarnessToken)
		srv.services = &fakeNodeServiceController{}
		srv.raft = nil
		srv.stepAsideRaft = nil

		resp, err := client.ExecuteNodeRestartPlan(tokenCtx(), planRequest("raftd-1", false))
		if err != nil {
			t.Fatalf("ExecuteNodeRestartPlan(): %v", err)
		}
		if resp.GetError() == "" {
			t.Fatal("ExecuteNodeRestartPlan() with no raft client was not refused")
		}
		if !strings.Contains(resp.GetError(), "no raft client") {
			t.Errorf("refusal does not say which dependency is missing: %q", resp.GetError())
		}
		if resp.GetResult() != nil {
			t.Errorf("a refusal that never ran returned a result: %+v", resp.GetResult())
		}
	})

	t.Run("no service controller", func(t *testing.T) {
		raftdSocket := newRaftdUDSSocket(t)
		client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
		srv.SetRestartGuardrailToken(planHarnessToken)
		srv.services = nil

		resp, err := client.ExecuteNodeRestartPlan(tokenCtx(), planRequest("raftd-1", false))
		if err != nil {
			t.Fatalf("ExecuteNodeRestartPlan(): %v", err)
		}
		if resp.GetError() == "" {
			t.Fatal("ExecuteNodeRestartPlan() with no service controller was not refused")
		}
		if !strings.Contains(resp.GetError(), "no lease was reserved") {
			t.Errorf("refusal does not say that nothing was reserved or attempted: %q", resp.GetError())
		}
	})
}

// TestExecuteNodeRestartPlan_RefusesAnUnknownNode covers the forward's
// membership lookup. An address must never come from the caller, and a
// node id nobody has heard of must be a named refusal rather than a dial
// to a guessed host.
func TestExecuteNodeRestartPlan_RefusesAnUnknownNode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Server)
		wantSub string
	}{
		{
			name:    "no peer forwarder",
			mutate:  func(s *Server) { s.peers = nil },
			wantSub: "no peer forwarder",
		},
		{
			name: "not a member",
			mutate: func(s *Server) {
				// A forwarder has to be present for the membership lookup
				// to be the thing under test; without one the handler
				// refuses earlier, for a different and equally correct
				// reason, and the assertion below would be testing that
				// instead.
				s.peers = &fakeISOPeerForwarder{}
			},
			wantSub: "does not list",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raftdSocket := newRaftdUDSSocket(t)
			client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
			srv.SetRestartGuardrailToken(planHarnessToken)
			srv.services = &fakeNodeServiceController{}
			tc.mutate(srv)

			resp, err := client.ExecuteNodeRestartPlan(tokenCtx(), planRequest("comb-nobody-has-heard-of", false))
			if err != nil {
				t.Fatalf("ExecuteNodeRestartPlan(): %v", err)
			}
			if resp.GetError() == "" {
				t.Fatal("a request naming an unknown Comb was not refused")
			}
			if !strings.Contains(resp.GetError(), tc.wantSub) {
				t.Errorf("refusal %q does not contain %q", resp.GetError(), tc.wantSub)
			}
			if !strings.Contains(resp.GetError(), "comb-nobody-has-heard-of") {
				t.Errorf("refusal %q does not name the Comb the caller asked about", resp.GetError())
			}
			if resp.GetResult() != nil {
				t.Errorf("a refused forward returned a result: %+v", resp.GetResult())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The whole sequence, on a real cluster.
// ---------------------------------------------------------------------------

// TestExecuteNodeRestartPlan_StepsAsideAndRefusesOnAnUnconfirmedHandover
// is the first real refusal, and the one that matters most.
//
// A single-voter cluster is the sharpest case: the target IS the leader,
// it cannot hand leadership to anybody, and restarting it would cost the
// cluster its only voter. The plan must refuse at the step-aside - before
// the quorum evaluation, before the lease, before the pending record -
// and the refusal must be `blocked` with the step-aside trace attached.
//
// The assertions on what did NOT happen matter as much as the outcome. A
// plan that reserved a lease and then discovered it could not step aside
// would leave a no-TTL cluster-wide block behind for a restart that never
// started, with no TTL to clear it.
func TestExecuteNodeRestartPlan_StepsAsideAndRefusesOnAnUnconfirmedHandover(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
	srv.SetRestartGuardrailToken(planHarnessToken)
	confirmDir := t.TempDir()
	srv.SetRestartConfirmStore(NewRestartConfirmStore(confirmDir))
	controller := &fakeNodeServiceController{}
	srv.services = controller
	srv.SetNodeRestartPlanDirs(t.TempDir(), t.TempDir(), 2, 20*time.Millisecond)

	resp, err := client.ExecuteNodeRestartPlan(tokenCtx(), planRequest("raftd-1", true))
	if err != nil {
		t.Fatalf("ExecuteNodeRestartPlan(): %v", err)
	}
	res := resp.GetResult()
	if res == nil {
		t.Fatalf("no result returned: %+v", resp)
	}
	if res.GetOutcome() != string(restartplan.OutcomeBlocked) {
		t.Errorf("outcome = %q, want %q (a single-voter cluster cannot hand leadership away, so the step-aside must refuse)",
			res.GetOutcome(), restartplan.OutcomeBlocked)
	}
	if res.GetLeaseId() != 0 {
		t.Errorf("lease id = %d, want 0 - a refused step-aside must not reserve anything", res.GetLeaseId())
	}
	if !strings.Contains(res.GetDetail(), "leadership") {
		t.Errorf("detail does not say the handover was the problem: %q", res.GetDetail())
	}
	aside := res.GetStepAside()
	if aside == nil {
		t.Fatal("no step-aside record on a run that stopped at the step-aside; the absence of evidence is not the same as a step-aside that found nothing to do")
	}
	if !aside.GetAttempted() {
		t.Error("step-aside record says attempted = false on a run that attempted one")
	}
	if aside.GetSafeToRestart() {
		t.Error("step-aside record says safe_to_restart = true on a cluster that cannot transfer leadership")
	}
	if strings.TrimSpace(aside.GetDetail()) == "" {
		t.Error("step-aside record has empty detail")
	}
	if got := controller.lastRestartName(); got != "" {
		t.Errorf("the restart command was issued despite a refused step-aside (%q)", got)
	}
	if _, found, _ := restartplan.NewPendingStore(confirmDir).Load(raftdServiceName); found {
		t.Error("a pending-restart record was written despite a refused step-aside; cmd/raftd would find it and try to confirm a lease that was never taken")
	}
	assertLeaseHeldOrGone(t, srv, 0, false)
}

// TestExecuteNodeRestartPlan_ConfirmedEndToEnd is the happy path, and the
// only test here that asserts the whole sequence actually ran.
//
// It is driven from a coordinator on a DIFFERENT Comb than the target,
// so the forward, the target-local execution, the real leadership
// transfer, the real lease, the real pending record, the real restart
// command and the real self-reported confirmation are all exercised. The
// target is whichever node currently leads, because that is the case
// ADR-0145 exists for: a leader-target that transfers leadership and is
// then confirmed safe to restart.
//
// Every effect is checked where it actually landed, not where the
// response claims it did: the restart command on the TARGET's controller
// and not the coordinator's; the lease released in the real FSM; the
// pending record gone from disk; the coordinator's own durable record on
// disk carrying the same outcome it returned.
//
// No force is used, and none is needed: a three-voter cluster whose
// target has stepped aside and whose other two voters answer is exactly
// the case ADR-0103's quorum arithmetic is meant to allow.
func TestExecuteNodeRestartPlan_ConfirmedEndToEnd(t *testing.T) {
	h := newPlanHarness(t, 3)
	leader := h.leaderIndex(t)
	coordinator := (leader + 1) % 3
	targetID := h.ids[leader]

	resp, err := h.clients[coordinator].ExecuteNodeRestartPlan(tokenCtx(), planRequest(targetID, false))
	if err != nil {
		t.Fatalf("ExecuteNodeRestartPlan() transport error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("ExecuteNodeRestartPlan() error: %q (result %+v)", resp.GetError(), resp.GetResult())
	}
	res := resp.GetResult()
	if res == nil {
		t.Fatal("no result returned")
	}
	if res.GetNodeId() != targetID {
		t.Errorf("result node_id = %q, want the target %q - a plan that ran on the wrong Comb is the exact failure this design is about", res.GetNodeId(), targetID)
	}
	if res.GetOutcome() != string(restartplan.OutcomeConfirmed) {
		t.Fatalf("outcome = %q, want %q (detail: %q; evidence: %v)", res.GetOutcome(), restartplan.OutcomeConfirmed, res.GetDetail(), res.GetEvidence())
	}
	if res.GetLeaseId() == 0 {
		t.Error("lease id = 0 on a confirmed run; a real lease was reserved and released, and its id is the evidence")
	}
	if res.GetStepAside() == nil || !res.GetStepAside().GetSafeToRestart() {
		t.Errorf("step-aside trace missing or unsafe on a confirmed run: %+v", res.GetStepAside())
	}
	if res.GetDetail() == "" {
		t.Error("a confirmed run has no detail")
	}

	// The restart command ran on the TARGET and nowhere else.
	if got := h.controllers[leader].restartCount(); got != 1 {
		t.Errorf("target Comb issued %d restart commands, want exactly 1", got)
	}
	if got := h.controllers[coordinator].restartCount(); got != 0 {
		t.Errorf("the COORDINATING Comb issued %d restart commands, want 0 - execution is target-local", got)
	}
	names := h.controllers[leader].restartedNames()
	if len(names) != 1 || names[0] != raftdServiceName {
		t.Errorf("restarted %v, want exactly [%s]", names, raftdServiceName)
	}

	// The lease was really reserved and really released by the restarted
	// process's confirmation, through the real FSM apply.
	//
	// Two waits, in this order, and the order is the point. Waiting only
	// for the lease to be GONE would succeed on the very first read,
	// before the lease had been applied at all, and would prove nothing
	// about the release. So the lease is first waited FOR - on the
	// target's own replicated state - and only then waited out.
	target := h.servers[leader]
	eventuallyLeaseHeld(t, target, res.GetLeaseId())
	eventually(t, 10*time.Second, func() bool {
		return heldLeaseOf(t, target, raftdServiceName) == nil
	})
	assertLeaseHeldOrGone(t, target, 0, false)

	// The pending record was written before the restart and cleared only
	// after the confirmation.
	if _, found, err := restartplan.NewPendingStore(target.restartConfirm.Dir).Load(raftdServiceName); err != nil {
		t.Errorf("reading the target's pending record: %v", err)
	} else if found {
		t.Error("the target's pending record survived a confirmed restart")
	}

	// The coordinator's own durable record exists, carries the same
	// outcome, and is NOT the restarted process's file.
	stored, found, err := restartplan.NewResultStore(target.coordinatorResultDir()).Load(targetID, raftdServiceName)
	if err != nil {
		t.Fatalf("reading the coordinator's durable record: %v", err)
	}
	if !found {
		t.Fatal("no durable record was written for the run")
	}
	if stored.Outcome != restartplan.OutcomeConfirmed {
		t.Errorf("the durable record says %q, but the response said %q; the record an operator reads later must not disagree with the answer the caller got", stored.Outcome, res.GetOutcome())
	}
	if stored.StepAside == nil || !stored.StepAside.SafeToRestart {
		t.Errorf("the durable record lost the step-aside trace: %+v", stored.StepAside)
	}
	if err := stored.Validate(); err != nil {
		t.Errorf("the durable record does not satisfy its own validation: %v", err)
	}
	if target.coordinatorResultDir() == target.selfReportDir() {
		t.Error("the coordinator's record and the restarted process's self-report share a directory; the two writers would overwrite each other")
	}
}

// TestExecuteNodeRestartPlan_FailedRestartCommandIsNotAnUnknown is the
// distinction the whole verdict vocabulary exists for. `service
// apiary_raftd restart` returned a non-zero exit: that is a positive
// observation of breakage, so the outcome is `failed`, and it is not
// `unobserved`.
//
// The lease must still be held afterwards. Only a real confirmation
// releases it, and a restart that failed has produced none - clearing it
// would be exactly the "restart succeeded" lie ADR-0103's no-TTL design
// exists to prevent.
func TestExecuteNodeRestartPlan_FailedRestartCommandIsNotAnUnknown(t *testing.T) {
	h := newPlanHarness(t, 3)
	leader := h.leaderIndex(t)
	h.controllers[leader].setBehaviour(true, fmt.Errorf("service apiary_raftd restart: /usr/local/etc/rc.d/apiary_raftd: WARNING: pidfile could not be verified"))

	resp, err := h.clients[leader].ExecuteNodeRestartPlan(tokenCtx(), planRequest(h.ids[leader], false))
	if err != nil {
		t.Fatalf("ExecuteNodeRestartPlan(): %v", err)
	}
	res := resp.GetResult()
	if res == nil {
		t.Fatalf("no result: %+v", resp)
	}
	if res.GetOutcome() != string(restartplan.OutcomeFailed) {
		t.Errorf("outcome = %q, want %q for a non-zero exit from the restart command (detail %q)", res.GetOutcome(), restartplan.OutcomeFailed, res.GetDetail())
	}
	if res.GetOutcome() == string(restartplan.OutcomeUnobserved) {
		t.Error("a failed restart command was reported as unobserved; a non-zero exit is an observation, not an absence of one")
	}
	if res.GetLeaseId() == 0 {
		t.Error("lease id = 0 on a run that reserved a lease before failing")
	}
	if len(res.GetEvidence()) == 0 {
		t.Error("a `failed` outcome with no evidence: a failure must cite what proved it")
	}
	// The lease the run really took is still held by this Comb. Waiting
	// for it to become visible first is what makes this an assertion
	// about the lease rather than about replication lag.
	stillHeld := eventuallyLeaseHeld(t, h.servers[leader], res.GetLeaseId())
	if stillHeld.GetHolderNodeId() != h.ids[leader] {
		t.Errorf("the held lease names %q, want the Comb this run restarted (%q)", stillHeld.GetHolderNodeId(), h.ids[leader])
	}
	if _, found, _ := restartplan.NewPendingStore(h.servers[leader].restartConfirm.Dir).Load(raftdServiceName); !found {
		t.Error("the pending record was cleared after a failed restart; only a real confirmation may clear it")
	}
}

// TestExecuteNodeRestartPlan_UnobservedWhenNobodyConfirms is the case
// that is easiest to get wrong and the one that most needs saying out
// loud: raftd restarted, the command succeeded, and the restarted process
// never reported itself back. That is not a failed restart and it is not
// a successful one. It is unobserved, and the lease stays held.
//
// A `confirmed` here would be a fabricated success. A `failed` here
// would blame the restart for something it may well have done. The only
// honest answer is the third one, with every attempt recorded.
func TestExecuteNodeRestartPlan_UnobservedWhenNobodyConfirms(t *testing.T) {
	h := newPlanHarness(t, 3)
	leader := h.leaderIndex(t)
	// raftd comes back, and cannot tell anyone: no guardrail token on
	// this node, say. It writes nothing and confirms nothing.
	h.controllers[leader].setBehaviour(false, nil)

	resp, err := h.clients[leader].ExecuteNodeRestartPlan(tokenCtx(), planRequest(h.ids[leader], false))
	if err != nil {
		t.Fatalf("ExecuteNodeRestartPlan(): %v", err)
	}
	res := resp.GetResult()
	if res == nil {
		t.Fatalf("no result: %+v", resp)
	}
	if res.GetOutcome() != string(restartplan.OutcomeUnobserved) {
		t.Fatalf("outcome = %q, want %q (detail %q, evidence %v)", res.GetOutcome(), restartplan.OutcomeUnobserved, res.GetDetail(), res.GetEvidence())
	}
	if len(res.GetEvidence()) == 0 {
		t.Error("an unobserved outcome with no evidence: 'unknown' must be a record of what was tried, not a shrug")
	}
	if !strings.Contains(res.GetDetail(), "lease") {
		t.Errorf("detail does not say the lease is still held: %q", res.GetDetail())
	}
	stillHeld := eventuallyLeaseHeld(t, h.servers[leader], res.GetLeaseId())
	if stillHeld.GetHolderNodeId() != h.ids[leader] {
		t.Errorf("the still-held lease names %q, want the Comb this run restarted (%q)", stillHeld.GetHolderNodeId(), h.ids[leader])
	}
	if _, found, _ := restartplan.NewPendingStore(h.servers[leader].restartConfirm.Dir).Load(raftdServiceName); !found {
		t.Error("the pending record was cleared without a confirmation")
	}
}

// TestExecuteNodeRestartPlan_OneCombAtATime is ADR-0145's correctness
// control, and the only part of "one at a time" this entry point can
// actually enforce.
//
// With four voters quorum is three, so two simultaneous restarts lose it
// regardless of who is leading. The real mechanism available here is
// ADR-0103's cluster-wide restart lease, and the test puts a real lease
// in the real FSM for a DIFFERENT Comb - no fake state, an actual
// AcquireRestartLease applied through the actual leader - and then asks
// the plan to restart this Comb anyway.
//
// Two things are asserted, and the second is the one that is easy to
// leave out. Without force the evaluation refuses. WITH force it must
// still refuse, because a held lease is not a known cost an operator can
// acknowledge - it is evidence that somebody else's restart is in flight
// and unconfirmed. ADR-0103's force-override of a stuck lease remains
// available on ReserveRestartLease/RestartNodeService for an operator
// recovering one by hand; that is a different and deliberate act.
//
// What this does NOT prove, and it is important not to overread it: this
// is a per-service lease, not durable Raft-backed ownership of a whole
// Colony update. Two callers racing, or a caller whose read of the lease
// state lags the leader, are not closed by this. That is the separate
// single-flight work, and it is not in this branch.
func TestExecuteNodeRestartPlan_OneCombAtATime(t *testing.T) {
	h := newPlanHarness(t, 3)
	leader := h.leaderIndex(t)
	targetID := h.ids[leader]
	otherComb := h.ids[(leader+1)%3]

	// A real lease, held by a different Comb, applied through the real
	// leader path. force is needed only because the quorum evaluation
	// runs before the apply and this is a real cluster with a real
	// check; the point of the test is what happens next.
	seed, err := h.clients[leader].ReserveRestartLease(tokenCtx(), &rpcpb.ReserveRestartLeaseRequest{
		Service: raftdServiceName, NodeId: otherComb, Force: true,
	})
	if err != nil {
		t.Fatalf("seeding another Comb's lease: %v", err)
	}
	if !seed.GetGranted() {
		t.Fatalf("could not seed another Comb's lease: %q", seed.GetError())
	}
	held := h.heldLease(t, leader, raftdServiceName)
	if held == nil || held.GetHolderNodeId() != otherComb {
		t.Fatalf("the seeded lease is not held by %q: %+v", otherComb, held)
	}

	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			out, err := h.clients[leader].ExecuteNodeRestartPlan(tokenCtx(), planRequest(targetID, force))
			if err != nil {
				t.Fatalf("ExecuteNodeRestartPlan(): %v", err)
			}
			res := out.GetResult()
			if res == nil {
				t.Fatalf("no result: %+v", out)
			}
			if res.GetOutcome() != string(restartplan.OutcomeBlocked) {
				t.Fatalf("outcome = %q, want %q: another Comb's restart is in flight and unconfirmed (detail %q)",
					res.GetOutcome(), restartplan.OutcomeBlocked, res.GetDetail())
			}
			if res.GetLeaseId() != 0 {
				t.Errorf("lease id = %d, want 0 - this run reserved nothing", res.GetLeaseId())
			}
			// The reason must be specific enough to act on: it has to name
			// who holds the lease. "a lease is held" is not something an
			// operator can do anything with.
			joined := res.GetDetail() + " " + strings.Join(res.GetEvidence(), " ")
			if !strings.Contains(joined, otherComb) {
				t.Errorf("the refusal does not name the Comb already holding the lease (%q); detail %q, evidence %v", otherComb, res.GetDetail(), res.GetEvidence())
			}
			if got := h.controllers[leader].restartCount(); got != 0 {
				t.Errorf("%d restart commands were issued while another Comb held the lease", got)
			}
			if stillHeld := eventuallyLeaseHeld(t, h.servers[leader], 0); stillHeld.GetHolderNodeId() != otherComb {
				t.Errorf("the held lease was disturbed by a refused run: held by %q, want %q", stillHeld.GetHolderNodeId(), otherComb)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The pieces that are cheaper and sharper to pin directly.
// ---------------------------------------------------------------------------

// TestManagerdLeaseReserver_RefusesAHeldLeaseEvenWithForce is the
// authoritative backstop, tested directly against a real raftd.
//
// It exists because the evaluation step alone is not enough, and the
// interaction that proves it is worth stating precisely. Take a target
// that is BOTH the leader AND covered by another Comb's held lease, with
// force set. restartplan.EvaluateQuorumSafety downgrades its OWN findings
// - the leader-election one - into cited caveats and returns Allow, while
// the `concurrent-manager-restart` finding survives in the evidence
// untouched. That report is honest as a record and useless as a gate: it
// reads "allowed" while carrying a reason it is not allowed. So the gate
// lives in the leaser, immediately before the authoritative apply, where
// no override can reach it.
//
// The first half of the test is the evaluation, asserting the Allow that
// documents the hole. The second is the reserver, asserting the refusal
// that closes it. A test of only the second would pass whether or not
// the hole exists; a test of only the first would document a gate that
// does not actually gate.
func TestManagerdLeaseReserver_RefusesAHeldLeaseEvenWithForce(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	_, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
	srv.SetRestartGuardrailToken(planHarnessToken)

	// What the evaluation says in exactly this situation: a leader
	// target, force set, and another Comb's lease already held.
	heldByOther := guardrail.EvaluateConcurrentManagerRestart(guardrail.RestartCooldownFact{
		TargetService:    raftdServiceName,
		IsLocalNodeVoter: true,
		ReadOK:           true,
		ActiveLease:      &guardrail.LeaseInfo{HolderNodeID: "comb-9-elsewhere", RequestedAtUnix: 1},
	})
	if heldByOther.Verdict != guardrail.Block {
		t.Fatalf("precondition: a held lease evaluated = %q, want block", heldByOther.Verdict)
	}
	report := restartplan.EvaluateQuorumSafety(restartplan.QuorumFact{
		Service:        raftdServiceName,
		TargetNodeID:   "raftd-1",
		IsTargetVoter:  true,
		IsTargetLeader: true,
		ProbeReadOK:    true,
		Force:          true,
		ObservedAt:     time.Now(),
		Existing:       &heldByOther,
	})
	if report.Verdict != guardrail.Allow {
		t.Fatalf("precondition: a forced evaluation of a leader target with a held lease = %q, want allow - this test documents the hole the leaser closes", report.Verdict)
	}

	// A real lease, held by a different Comb, in the real FSM. The
	// service is one the quorum evaluation ignores, so the apply is the
	// only thing standing between this and a second lease, which is
	// exactly the position the reserver's own check is in.
	resp, err := srv.reserveRestartLease(context.Background(), &rpcpb.ReserveRestartLeaseRequest{
		Service: "apiary_frontend", NodeId: "comb-9-elsewhere",
	})
	if err != nil {
		t.Fatalf("seeding the lease: %v", err)
	}
	if !resp.GetGranted() {
		t.Fatalf("could not seed a lease: %q", resp.GetError())
	}

	// The reserver is asked about apiary_frontend, the service that now
	// genuinely has somebody else's unconfirmed lease.
	reserver := managerdLeaseReserver{s: srv}
	for _, force := range []bool{false, true} {
		_, err := reserver.ReserveRestartLease(context.Background(), "apiary_frontend", "raftd-1", force)
		if err == nil {
			t.Fatalf("ReserveRestartLease(force=%v) succeeded while another Comb held the lease", force)
		}
		if !strings.Contains(err.Error(), "comb-9-elsewhere") {
			t.Errorf("force=%v refusal does not name the holder: %v", force, err)
		}
		if !strings.Contains(err.Error(), "not overridable by force") {
			t.Errorf("force=%v refusal does not say that force cannot wave it through: %v", force, err)
		}
	}
}

// TestManagerdLeaseReserver_FailsClosedOnAnUnreadableLeaseState is the
// other half of the same property. An unreadable lease state means the
// "is somebody else already restarting this" question has no answer, and
// an unanswered question is not permission.
func TestManagerdLeaseReserver_FailsClosedOnAnUnreadableLeaseState(t *testing.T) {
	reserver := managerdLeaseReserver{s: &Server{}}
	if _, err := reserver.ReserveRestartLease(context.Background(), raftdServiceName, "comb-1", true); err == nil {
		t.Fatal("ReserveRestartLease() on a node with no raft client = no error, want a refusal - 'could not check' must never read as 'nothing to check'")
	}
}

// TestSelfReportedConfirmer_OnlyTheRestartedProcessCanConfirm is the
// honesty property the whole confirmation step rests on, tested where it
// is cheapest to be precise.
//
// The three refusals are each a different mistake being caught:
//
//   - no record at all: raftd has not come back yet, or came back and
//     could not say anything;
//   - a record for a DIFFERENT lease: this is the dangerous one. A
//     `confirmed` left over from an earlier restart on this same node
//     would otherwise be credited to this one, and the result would be a
//     restart reported healthy that never happened. Lease ids are raft
//     log indexes, so they are unique and never reused - which is the
//     only reason this check can be exact;
//   - a record whose outcome is `unobserved`: real information, and
//     emphatically not a confirmation. raftd came up and could not
//     release its own lease.
func TestSelfReportedConfirmer_OnlyTheRestartedProcessCanConfirm(t *testing.T) {
	results := restartplan.NewResultStore(t.TempDir())
	confirmer := selfReportedConfirmer{results: results}
	ctx := context.Background()

	err := confirmer.ConfirmRestartCompleted(ctx, raftdServiceName, "comb-1", 42)
	if err == nil || !strings.Contains(err.Error(), "has not written a restart record") {
		t.Errorf("with no record at all, error = %v, want a refusal naming what is missing", err)
	}

	write := func(leaseID uint64, outcome restartplan.Outcome) {
		t.Helper()
		if err := results.Save(restartplan.Result{
			Service: raftdServiceName, NodeID: "comb-1", LeaseID: leaseID,
			Outcome:  outcome,
			Detail:   "a durable record written by whatever ran last",
			Evidence: []string{"lease confirmed on attempt 1"},
		}); err != nil {
			t.Fatalf("seeding a restart result: %v", err)
		}
	}

	write(41, restartplan.OutcomeConfirmed)
	if err := confirmer.ConfirmRestartCompleted(ctx, raftdServiceName, "comb-1", 42); err == nil {
		t.Error("a `confirmed` record for lease 41 was accepted as a confirmation of lease 42; a stale success must never be credited to a restart that never happened")
	} else if !strings.Contains(err.Error(), "lease 41") {
		t.Errorf("the stale-record refusal does not say which lease the record is actually for: %v", err)
	}

	write(42, restartplan.OutcomeUnobserved)
	if err := confirmer.ConfirmRestartCompleted(ctx, raftdServiceName, "comb-1", 42); err == nil {
		t.Error("an `unobserved` record was accepted as a confirmation; raftd coming up is not raftd saying it is healthy")
	}

	write(42, restartplan.OutcomeConfirmed)
	if err := confirmer.ConfirmRestartCompleted(ctx, raftdServiceName, "comb-1", 42); err != nil {
		t.Errorf("the restarted process's own `confirmed` record for this lease was refused: %v", err)
	}
}

// TestManagerdAddrFromStatus is the forward's address resolution, which
// the harness above cannot exercise for real: three loopback raft
// addresses all reduce to one managerd address on a single host. It is
// the fail-closed half that matters, so it is pinned here directly.
func TestManagerdAddrFromStatus(t *testing.T) {
	members := &internalpb.StatusResponse{Servers: []*internalpb.ServerInfo{
		{Id: "brood", Address: "10.90.0.94:17600", Suffrage: "Voter"},
		{Id: "drone", Address: "10.90.0.95:17600", Suffrage: "Voter"},
		{Id: "addrless", Suffrage: "Voter"},
	}}

	addr, err := managerdAddrFromStatus(members, "drone", "")
	if err != nil {
		t.Fatalf("managerdAddrFromStatus(drone): %v", err)
	}
	if addr != "10.90.0.95:"+defaultPeerManagerdPort {
		t.Errorf("address = %q, want 10.90.0.95:%s", addr, defaultPeerManagerdPort)
	}
	if addr, err := managerdAddrFromStatus(members, "drone", "19999"); err != nil || addr != "10.90.0.95:19999" {
		t.Errorf("an explicitly configured peer port was ignored: (%q, %v)", addr, err)
	}

	for _, tc := range []struct {
		node    string
		wantSub string
	}{
		{"nobody", "does not list"},
		{"addrless", "no raft transport address"},
		{"", "does not list"},
	} {
		addr, err := managerdAddrFromStatus(members, tc.node, "")
		if err == nil {
			t.Errorf("managerdAddrFromStatus(%q) = %q, want a refusal containing %q", tc.node, addr, tc.wantSub)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("managerdAddrFromStatus(%q) error = %q, want it to contain %q", tc.node, err, tc.wantSub)
		}
	}
}

// TestPeerReporterExecuteNodeRestartPlan_PresentsTheTokenOverARealDial
// covers the one thing the harness forwarder stands in for: the dial.
//
// There is a specific, already-paid-for reason this test exists. A
// previous release shipped a credential that existed, was correct, and
// was unit-tested in isolation, but no call site ever put it on the wire
// - so raftd dialled managerd, presented nothing, and every
// confirmation came back PermissionDenied, leaving a lease held and a
// service that could not be restarted. Nothing about that symptom points
// at the dial, so this drives the real PeerReporter against a real
// managerd listener rather than a stub: a stub is exactly what let the
// omission through.
func TestPeerReporterExecuteNodeRestartPlan_PresentsTheTokenOverARealDial(t *testing.T) {
	h := newPlanHarness(t, 1)
	target := h.ids[0]

	// An ordinary Colony credential, and no guardrail token: the far end
	// must refuse both. This is the same class of bug - a reporter
	// dialling with the wrong credential - and it must fail here.
	ordinary := NewPeerReporter("an-ordinary-peer-api-key", false, nil)
	_, err := ordinary.ExecuteNodeRestartPlan(context.Background(), h.addrs[0], planRequest(target, false))
	if err == nil {
		t.Fatal("a reporter with no guardrail token got no error from ExecuteNodeRestartPlan; the token is not on the wire")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("no-token dial error code = %v, want PermissionDenied (%v)", code, err)
	}

	// With the guardrail token, the same dial reaches the handler. The
	// single-voter cluster makes the plan refuse at the step-aside, and
	// that refusal is the proof the call was served rather than
	// refused at the gate.
	reporter := NewPeerReporter("an-ordinary-peer-api-key", false, nil)
	reporter.RestartGuardrailToken = planHarnessToken
	resp, err := reporter.ExecuteNodeRestartPlan(context.Background(), h.addrs[0], planRequest(target, false))
	if err != nil {
		t.Fatalf("a reporter carrying the guardrail token was refused by the far end: %v", err)
	}
	res := resp.GetResult()
	if res == nil {
		t.Fatalf("the call was authenticated but produced no result: %+v", resp)
	}
	if res.GetOutcome() != string(restartplan.OutcomeBlocked) {
		t.Errorf("outcome = %q, want %q on a single-voter cluster", res.GetOutcome(), restartplan.OutcomeBlocked)
	}
}

// TestExecuteNodeRestartPlan_ProductionConfirmBudgetIsTheDocumentedOne
// pins the budget the plan actually ships with. The harness above turns
// it down to 2 attempts so the unobserved case runs in milliseconds, and
// a budget that is only ever exercised turned-down is a budget whose real
// value nobody has checked.
func TestExecuteNodeRestartPlan_ProductionConfirmBudgetIsTheDocumentedOne(t *testing.T) {
	srv := &Server{}
	budget := srv.planConfirm()
	if budget.Attempts != 10 || budget.Backoff != 3*time.Second {
		t.Errorf("default confirmation budget = %d attempts %s apart, want 10 attempts 3s apart", budget.Attempts, budget.Backoff)
	}
	// And it must exceed restartplan's own default, which is the budget
	// cmd/raftd uses to MAKE the confirmation. A coordinator that waits
	// exactly as long as the process it is waiting for takes to start
	// asking will expire first, and report `unobserved` for a restart
	// that completed.
	if budget.Attempts <= restartplan.DefaultConfirmOptions().Attempts {
		t.Errorf("the coordinator's budget (%d attempts) does not exceed cmd/raftd's own confirming budget (%d attempts); it would stop waiting before the restarted process could finish asking",
			budget.Attempts, restartplan.DefaultConfirmOptions().Attempts)
	}
}
