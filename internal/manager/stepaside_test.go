package manager

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// The adapter has to satisfy the seam restartplan actually calls, not a
// lookalike of it. This fails at compile time rather than at the first
// controlled update.
var _ restartplan.StepAside = (*restartStepAsider)(nil)

// fakeStepAsideRaft stands in for this node's own raftd socket. It
// answers Status from a fixed membership and StepAsideForRestartLocal
// from a fixed response or error, and records what it was asked.
type fakeStepAsideRaft struct {
	mu sync.Mutex

	servers   []*internalpb.ServerInfo
	statusErr error

	localResp    *internalpb.StepAsideForRestartResponse
	localErr     error
	localCalls   int
	localTimeout uint64
}

func (f *fakeStepAsideRaft) Status(context.Context) (*internalpb.StatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &internalpb.StatusResponse{Servers: f.servers}, nil
}

func (f *fakeStepAsideRaft) StepAsideForRestartLocal(_ context.Context, timeoutMs uint64) (*internalpb.StepAsideForRestartResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.localCalls++
	f.localTimeout = timeoutMs
	if f.localErr != nil {
		return nil, f.localErr
	}
	return f.localResp, nil
}

func (f *fakeStepAsideRaft) localCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.localCalls
}

func (f *fakeStepAsideRaft) lastLocalTimeout() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.localTimeout
}

// fakeStepAsideForwarder records what a remote hop was asked to do and
// answers from a fixed response or error.
type fakeStepAsideForwarder struct {
	mu sync.Mutex

	resp  *rpcpb.StepAsideForRestartResponse
	err   error
	calls []struct {
		addr string
		req  *rpcpb.StepAsideForRestartRequest
	}
}

func (f *fakeStepAsideForwarder) StepAsideForRestart(_ context.Context, addr string, req *rpcpb.StepAsideForRestartRequest) (*rpcpb.StepAsideForRestartResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct {
		addr string
		req  *rpcpb.StepAsideForRestartRequest
	}{addr: addr, req: req})
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeStepAsideForwarder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeStepAsideForwarder) lastCall() (string, *rpcpb.StepAsideForRestartRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return "", nil
	}
	last := f.calls[len(f.calls)-1]
	return last.addr, last.req
}

func members() []*internalpb.ServerInfo {
	return []*internalpb.ServerInfo{
		{Id: "brood", Address: "10.90.0.94:17600", Suffrage: "Voter"},
		{Id: "drone", Address: "10.90.0.95:17600", Suffrage: "Voter"},
		{Id: "buzz", Address: "10.90.0.96:17600", Suffrage: "Voter"},
	}
}

// TestStepAsiderServesItsOwnCombOverTheLocalSocket proves the local
// case takes the local path. Resolving this node's own address from
// membership and dialing it would work, but only on a node whose
// advertised address is actually routable back to itself, and a
// step-aside that fails whenever a node cannot reach its own address is
// a step-aside that fails on the node most likely to be the leader.
func TestStepAsiderServesItsOwnCombOverTheLocalSocket(t *testing.T) {
	raft := &fakeStepAsideRaft{
		servers: members(),
		localResp: &internalpb.StepAsideForRestartResponse{
			NodeId:        "brood",
			WasLeader:     true,
			Transferred:   true,
			NewLeaderId:   "drone",
			SafeToRestart: true,
			Detail:        "brood handed over leadership to drone and is now a follower",
		},
	}
	peers := &fakeStepAsideForwarder{}
	asider := &restartStepAsider{localNodeID: "brood", raft: raft, peers: peers}

	out, err := asider.StepAsideForRestart(context.Background(), "brood")
	if err != nil {
		t.Fatalf("StepAsideForRestart on the local node: %v", err)
	}
	if !out.SafeToRestart {
		t.Errorf("SafeToRestart = false, want true: a confirmed local handover is the one case that authorizes a restart")
	}
	if !out.WasLeader || !out.Transferred || out.NewLeaderID != "drone" {
		t.Errorf("outcome = %+v, want was-leader + transferred with a new leader of drone", out)
	}
	if raft.localCallCount() != 1 {
		t.Errorf("local raftd was called %d times, want exactly 1", raft.localCallCount())
	}
	if n := peers.callCount(); n != 0 {
		t.Errorf("a local step-aside made %d peer forward(s); it must never dial itself over the network", n)
	}
}

// TestStepAsiderForwardsToTheTargetCombOverThePeerTokenPath is the case
// that does not exist at all without this file: asking a Comb other
// than this one to stand down.
func TestStepAsiderForwardsToTheTargetCombOverThePeerTokenPath(t *testing.T) {
	raft := &fakeStepAsideRaft{servers: members()}
	peers := &fakeStepAsideForwarder{
		resp: &rpcpb.StepAsideForRestartResponse{
			NodeId:        "drone",
			WasLeader:     true,
			Transferred:   true,
			NewLeaderId:   "buzz",
			SafeToRestart: true,
			Detail:        "drone handed over leadership to buzz and is now a follower",
		},
	}
	asider := &restartStepAsider{localNodeID: "brood", raft: raft, peers: peers, timeout: 20 * time.Second}

	out, err := asider.StepAsideForRestart(context.Background(), "drone")
	if err != nil {
		t.Fatalf("StepAsideForRestart for a remote target: %v", err)
	}
	if !out.SafeToRestart {
		t.Errorf("SafeToRestart = false, want true")
	}
	if out.NewLeaderID != "buzz" {
		t.Errorf("NewLeaderID = %q, want buzz", out.NewLeaderID)
	}
	if raft.localCallCount() != 0 {
		t.Errorf("a remote step-aside touched the local raftd %d time(s); it must only ever act on the target", raft.localCallCount())
	}
	addr, req := peers.lastCall()
	if addr != "10.90.0.95:17700" {
		t.Errorf("forwarded to %q, want the target's managerd address 10.90.0.95:17700", addr)
	}
	if req.GetTargetNodeId() != "drone" {
		t.Errorf("forwarded request named target %q, want drone - the receiving node must be able to see a misroute", req.GetTargetNodeId())
	}
	if req.GetTimeoutMs() != 20000 {
		t.Errorf("forwarded timeout_ms = %d, want 20000", req.GetTimeoutMs())
	}
}

// TestStepAsiderUsesTheConfiguredPeerManagerdPort proves the resolved
// address is not hard-coded to the default port, which would be wrong
// on any colony that changed it.
func TestStepAsiderUsesTheConfiguredPeerManagerdPort(t *testing.T) {
	peers := &fakeStepAsideForwarder{resp: &rpcpb.StepAsideForRestartResponse{NodeId: "buzz", SafeToRestart: true, Detail: "not the leader"}}
	asider := &restartStepAsider{
		localNodeID:  "brood",
		raft:         &fakeStepAsideRaft{servers: members()},
		peers:        peers,
		managerdPort: "19999",
	}
	if _, err := asider.StepAsideForRestart(context.Background(), "buzz"); err != nil {
		t.Fatalf("StepAsideForRestart: %v", err)
	}
	addr, _ := peers.lastCall()
	if addr != "10.90.0.96:19999" {
		t.Errorf("forwarded to %q, want 10.90.0.96:19999", addr)
	}
}

// TestStepAsiderRefusesUnknownConditions is the fail-closed table. Each
// case is a way the target's answer could be unobtainable, and every
// one of them must produce the same two facts: not safe, and an error
// the coordinator cannot override.
func TestStepAsiderRefusesUnknownConditions(t *testing.T) {
	cases := []struct {
		name       string
		asider     *restartStepAsider
		target     string
		wantDetail string
	}{
		{
			name:       "no target named",
			asider:     &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{servers: members()}},
			target:     "",
			wantDetail: "no target node",
		},
		{
			name:       "no raft client at all",
			asider:     &restartStepAsider{localNodeID: "brood"},
			target:     "drone",
			wantDetail: "no raft client configured",
		},
		{
			name:       "membership unreadable",
			asider:     &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{statusErr: fmt.Errorf("raftd socket gone")}, peers: &fakeStepAsideForwarder{}},
			target:     "drone",
			wantDetail: "cluster membership could not be read",
		},
		{
			name:       "target is not a member this node knows",
			asider:     &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{servers: members()}, peers: &fakeStepAsideForwarder{}},
			target:     "sting",
			wantDetail: "does not list sting as a member",
		},
		{
			name: "member has no reachable address",
			asider: &restartStepAsider{
				localNodeID: "brood",
				raft:        &fakeStepAsideRaft{servers: []*internalpb.ServerInfo{{Id: "drone", Suffrage: "Voter"}}},
				peers:       &fakeStepAsideForwarder{},
			},
			target:     "drone",
			wantDetail: "no raft transport address",
		},
		{
			name:       "no peer forwarder configured",
			asider:     &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{servers: members()}},
			target:     "drone",
			wantDetail: "no peer forwarder configured",
		},
		{
			name:       "target unreachable",
			asider:     &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{servers: members()}, peers: &fakeStepAsideForwarder{err: fmt.Errorf("connection refused")}},
			target:     "drone",
			wantDetail: "connection refused",
		},
		{
			name:       "local raftd unreachable",
			asider:     &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{servers: members(), localErr: fmt.Errorf("socket closed")}},
			target:     "brood",
			wantDetail: "socket closed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.asider.StepAsideForRestart(context.Background(), tc.target)
			if err == nil {
				t.Error("err = nil, want a non-nil error: restartplan treats a nil error as a step that ran, and the whole point is that it did not get that far")
			}
			if out.SafeToRestart {
				t.Error("SafeToRestart = true, want false")
			}
			if !strings.Contains(out.Detail, tc.wantDetail) {
				t.Errorf("Detail = %q, want it to mention %q", out.Detail, tc.wantDetail)
			}
		})
	}
}

// TestStepAsiderRefusesAnAnswerFromTheWrongNode is the honest-answering
// case: a node that replies truthfully about ITSELF when asked about
// somebody else. Forwarding the target's node id is the mistake, and
// the response is the only place it becomes visible.
func TestStepAsiderRefusesAnAnswerFromTheWrongNode(t *testing.T) {
	for _, answering := range []string{"brood", ""} {
		t.Run("answered by "+answering, func(t *testing.T) {
			peers := &fakeStepAsideForwarder{resp: &rpcpb.StepAsideForRestartResponse{
				NodeId:        answering,
				WasLeader:     false,
				SafeToRestart: true,
				Detail:        "I am not the leader",
			}}
			asider := &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{servers: members()}, peers: peers}

			out, err := asider.StepAsideForRestart(context.Background(), "drone")
			if err == nil {
				t.Fatal("err = nil, want a refusal: a different node's answer is not evidence about the target")
			}
			if out.SafeToRestart {
				t.Error("SafeToRestart = true, want false")
			}
			if !strings.Contains(out.Detail, "cannot") {
				t.Errorf("Detail = %q, want it to say the answer cannot be attributed to the target", out.Detail)
			}
		})
	}
}

// TestStepAsiderRefusesALocalAnswerFromTheWrongNode is the same
// invariant one hop closer in: raftd's own response names its node, and
// a managerd that ignored that would be trusting a socket to
// self-identify correctly.
func TestStepAsiderRefusesALocalAnswerFromTheWrongNode(t *testing.T) {
	raft := &fakeStepAsideRaft{
		servers:   members(),
		localResp: &internalpb.StepAsideForRestartResponse{NodeId: "drone", SafeToRestart: true, Detail: "not the leader"},
	}
	asider := &restartStepAsider{localNodeID: "brood", raft: raft}

	out, err := asider.StepAsideForRestart(context.Background(), "brood")
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	if out.SafeToRestart {
		t.Error("SafeToRestart = true, want false")
	}
	if !strings.Contains(out.Detail, "drone") {
		t.Errorf("Detail = %q, want it to name the node that actually answered", out.Detail)
	}
}

// TestStepAsiderCarriesTheSafeFlagInBothDirections is the one that pins
// the flag itself. Every other test here has a node that agrees with the
// answer it expects, so a mapper that simply hard-coded true or false
// would pass all of them. Both directions, both hops, because the
// restartplan Engine's entire behaviour - whether a leader-target needs
// an operator acknowledgment - turns on this one boolean, and it is the
// boolean most worth getting exactly right.
func TestStepAsiderCarriesTheSafeFlagInBothDirections(t *testing.T) {
	cases := []struct {
		name string
		safe bool
	}{
		{name: "confirmed safe", safe: true},
		{name: "confirmed unsafe", safe: false},
	}
	for _, tc := range cases {
		for _, hop := range []string{"local", "remote"} {
			t.Run(tc.name+" over the "+hop+" hop", func(t *testing.T) {
				target := "brood"
				asider := &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{servers: members()}}
				if hop == "local" {
					asider.raft = &fakeStepAsideRaft{
						servers:   members(),
						localResp: &internalpb.StepAsideForRestartResponse{NodeId: "brood", SafeToRestart: tc.safe, Detail: "answered"},
					}
				} else {
					target = "drone"
					asider.peers = &fakeStepAsideForwarder{resp: &rpcpb.StepAsideForRestartResponse{NodeId: "drone", SafeToRestart: tc.safe, Detail: "answered"}}
				}
				out, _ := asider.StepAsideForRestart(context.Background(), target)
				if out.SafeToRestart != tc.safe {
					t.Errorf("SafeToRestart = %v, want %v: the node's own answer must be carried through unchanged, because this boolean is what the restartplan Engine reads to decide whether a leader-target needs an operator acknowledgment", out.SafeToRestart, tc.safe)
				}
			})
		}
	}
}

// TestStepAsiderAnErrorFieldVetoesSafeToRestart is the defence-in-depth
// case. raftd's own server already forces safe_to_restart false when it
// reports an error, so this is checking the second, independent check -
// the one that survives a raftd build that gets it wrong.
func TestStepAsiderAnErrorFieldVetoesSafeToRestart(t *testing.T) {
	local := &restartStepAsider{
		localNodeID: "brood",
		raft: &fakeStepAsideRaft{servers: members(), localResp: &internalpb.StepAsideForRestartResponse{
			NodeId:        "brood",
			SafeToRestart: true,
			Transferred:   true,
			NewLeaderId:   "drone",
			Detail:        "handed over",
			Error:         "leadership transfer did not complete",
		}},
	}
	remote := &restartStepAsider{
		localNodeID: "brood",
		raft:        &fakeStepAsideRaft{servers: members()},
		peers: &fakeStepAsideForwarder{resp: &rpcpb.StepAsideForRestartResponse{
			NodeId:        "drone",
			SafeToRestart: true,
			Detail:        "handed over",
			Error:         "leadership transfer did not complete",
		}},
	}

	for name, asider := range map[string]*restartStepAsider{"local": local, "remote": remote} {
		t.Run(name, func(t *testing.T) {
			target := "brood"
			if name == "remote" {
				target = "drone"
			}
			out, _ := asider.StepAsideForRestart(context.Background(), target)
			if out.SafeToRestart {
				t.Error("SafeToRestart = true, want false: an answer carrying an error cannot authorize a restart, whatever its own boolean says")
			}
			if !strings.Contains(out.Detail, "leadership transfer did not complete") {
				t.Errorf("Detail = %q, want it to carry the node's own reason", out.Detail)
			}
		})
	}
}

// TestStepAsiderDetailIsNeverEmpty proves the evidence a refused or
// accepted restart is recorded with is always readable prose, even if
// the node answered with nothing but booleans.
func TestStepAsiderDetailIsNeverEmpty(t *testing.T) {
	cases := []struct {
		name string
		resp *internalpb.StepAsideForRestartResponse
		want string
	}{
		{
			name: "follower no-op",
			resp: &internalpb.StepAsideForRestartResponse{NodeId: "brood", SafeToRestart: true},
			want: "was not the raft leader",
		},
		{
			name: "transferred",
			resp: &internalpb.StepAsideForRestartResponse{NodeId: "brood", WasLeader: true, Transferred: true, NewLeaderId: "drone", SafeToRestart: true},
			want: "handed over leadership to drone",
		},
		{
			name: "leader with no confirmed handover",
			resp: &internalpb.StepAsideForRestartResponse{NodeId: "brood", WasLeader: true},
			want: "no confirmed handover",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asider := &restartStepAsider{localNodeID: "brood", raft: &fakeStepAsideRaft{servers: members(), localResp: tc.resp}}
			out, _ := asider.StepAsideForRestart(context.Background(), "brood")
			if out.Detail == "" {
				t.Fatal("Detail is empty; a step-aside record with no prose cannot explain a restart later")
			}
			if !strings.Contains(out.Detail, tc.want) {
				t.Errorf("Detail = %q, want it to mention %q", out.Detail, tc.want)
			}
		})
	}
}

// TestServerNewRestartStepAsiderDegradesSafely covers the wiring
// decision. With a raft client there is a real reach; without one there
// is nil, and nil is the safe degraded posture because restartplan then
// skips the step and leaves the leader-restart guardrail in charge.
func TestServerNewRestartStepAsiderDegradesSafely(t *testing.T) {
	withRaft := &Server{nodeID: "brood", stepAsideRaft: &fakeStepAsideRaft{servers: members()}}
	if asider := withRaft.newRestartStepAsider(); asider == nil {
		t.Error("newRestartStepAsider() = nil with a raft client configured; the reach exists and should be wired")
	}
	withoutRaft := &Server{nodeID: "brood"}
	if asider := withoutRaft.newRestartStepAsider(); asider != nil {
		t.Error("newRestartStepAsider() = non-nil with no raft client; a nil StepAsider is the safe fallback, since a non-nil one would panic on use")
	}
}

// TestServerStepAsideForRestartRequiresTheGuardrailToken proves the RPC
// is not reachable by an ordinary Colony credential. The token check
// happens before anything else, including before the target check, so a
// caller with no token learns nothing about this node's identity.
func TestServerStepAsideForRestartRequiresTheGuardrailToken(t *testing.T) {
	raft := &fakeStepAsideRaft{localResp: &internalpb.StepAsideForRestartResponse{NodeId: "brood", SafeToRestart: true}}
	s := &Server{nodeID: "brood", stepAsideRaft: raft, restartGuardrailToken: "s3cret"}

	for _, presented := range []string{"", "wrong", "s3cretX"} {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+presented))
		if _, err := s.StepAsideForRestart(ctx, &rpcpb.StepAsideForRestartRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("token %q: err code = %v, want PermissionDenied", presented, status.Code(err))
		}
	}
	if raft.localCallCount() != 0 {
		t.Errorf("an unauthorized caller reached raftd %d time(s)", raft.localCallCount())
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer s3cret"))
	resp, err := s.StepAsideForRestart(ctx, &rpcpb.StepAsideForRestartRequest{})
	if err != nil {
		t.Fatalf("with the correct token: %v", err)
	}
	if !resp.GetSafeToRestart() {
		t.Errorf("SafeToRestart = false, want true with the correct token")
	}
}

// TestServerStepAsideForRestartRefusesAMisaddressedTarget proves the
// RPC cannot be used to make some other Comb stand down. It is
// answered - with a named node and a refusal - rather than errored, so
// the coordinator can attribute the answer and read the reason.
func TestServerStepAsideForRestartRefusesAMisaddressedTarget(t *testing.T) {
	raft := &fakeStepAsideRaft{localResp: &internalpb.StepAsideForRestartResponse{NodeId: "brood", SafeToRestart: true, Detail: "not the leader"}}
	s := &Server{nodeID: "brood", stepAsideRaft: raft, restartGuardrailToken: "s3cret"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer s3cret"))

	for _, target := range []string{"drone", "buzz"} {
		resp, err := s.StepAsideForRestart(ctx, &rpcpb.StepAsideForRestartRequest{TargetNodeId: target})
		if err != nil {
			t.Fatalf("target %s: a refusal is a response, not a transport failure: %v", target, err)
		}
		if resp.GetSafeToRestart() {
			t.Errorf("target %s: SafeToRestart = true, want false", target)
		}
		if resp.GetNodeId() != "brood" {
			t.Errorf("target %s: NodeId = %q, want this node's own id so the answer is attributable", target, resp.GetNodeId())
		}
		if !strings.Contains(resp.GetError(), target) {
			t.Errorf("target %s: Error = %q, want it to name the Comb the request actually asked about", target, resp.GetError())
		}
	}
	if raft.localCallCount() != 0 {
		t.Errorf("a misaddressed request reached raftd %d time(s); it must be refused before any leadership is touched", raft.localCallCount())
	}
}

// TestServerStepAsideForRestartAcceptsAnEmptyOrSelfTarget covers the two
// spellings of "this node", since a same-node caller legitimately sends
// either.
func TestServerStepAsideForRestartAcceptsAnEmptyOrSelfTarget(t *testing.T) {
	raft := &fakeStepAsideRaft{localResp: &internalpb.StepAsideForRestartResponse{NodeId: "brood", SafeToRestart: true, Detail: "not the leader"}}
	s := &Server{nodeID: "brood", stepAsideRaft: raft, restartGuardrailToken: "s3cret"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer s3cret"))

	for _, target := range []string{"", "brood"} {
		resp, err := s.StepAsideForRestart(ctx, &rpcpb.StepAsideForRestartRequest{TargetNodeId: target})
		if err != nil {
			t.Fatalf("target %q: %v", target, err)
		}
		if !resp.GetSafeToRestart() || resp.GetError() != "" {
			t.Errorf("target %q: SafeToRestart = %v, Error = %q; want a clean safe answer", target, resp.GetSafeToRestart(), resp.GetError())
		}
	}
}

// TestServerStepAsideForRestartKeepsAFailedHandoverInTheResponse is
// the distinction the internal RPC was built around and this one
// inherits: a node that could not stand down is not the same event as a
// node that could not be reached. The first must arrive as a readable
// response carrying safe_to_restart false and the reason; the second is
// a transport error. Collapsing them would leave a coordinator unable
// to tell a stuck Comb from a dead one.
func TestServerStepAsideForRestartKeepsAFailedHandoverInTheResponse(t *testing.T) {
	raft := &fakeStepAsideRaft{localResp: &internalpb.StepAsideForRestartResponse{
		NodeId:        "brood",
		WasLeader:     true,
		SafeToRestart: false,
		Detail:        "brood is still the leader",
		Error:         "leadership transfer did not complete within the deadline",
	}}
	s := &Server{nodeID: "brood", stepAsideRaft: raft, restartGuardrailToken: "s3cret"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer s3cret"))

	resp, err := s.StepAsideForRestart(ctx, &rpcpb.StepAsideForRestartRequest{})
	if err != nil {
		t.Fatalf("a node that could not hand over must answer, not error: %v", err)
	}
	if resp.GetSafeToRestart() {
		t.Error("SafeToRestart = true, want false")
	}
	if resp.GetError() == "" {
		t.Error("Error is empty; the coordinator would have no way to tell a refusal from a silent success")
	}
	if resp.GetNodeId() != "brood" {
		t.Errorf("NodeId = %q, want brood even on a refusal", resp.GetNodeId())
	}
	if resp.GetDetail() == "" {
		t.Error("Detail is empty on a refusal, so the recorded evidence has nothing to show")
	}
}

// TestServerStepAsideForRestartHonoursACallerTimeout proves a caller can
// bound the wait. A coordinator driving a sweep knows how long it is
// prepared to spend on one Comb, and a per-call deadline is the only way
// to say so.
func TestServerStepAsideForRestartHonoursACallerTimeout(t *testing.T) {
	raft := &fakeStepAsideRaft{localResp: &internalpb.StepAsideForRestartResponse{NodeId: "brood", SafeToRestart: true, Detail: "not the leader"}}
	s := &Server{nodeID: "brood", stepAsideRaft: raft, restartGuardrailToken: "s3cret"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer s3cret"))

	if _, err := s.StepAsideForRestart(ctx, &rpcpb.StepAsideForRestartRequest{TimeoutMs: 5000}); err != nil {
		t.Fatalf("StepAsideForRestart: %v", err)
	}
	if got := raft.lastLocalTimeout(); got != 5000 {
		t.Errorf("raftd was asked to wait %d ms, want the caller's 5000", got)
	}

	if _, err := s.StepAsideForRestart(ctx, &rpcpb.StepAsideForRestartRequest{}); err != nil {
		t.Fatalf("StepAsideForRestart with no timeout: %v", err)
	}
	if got := raft.lastLocalTimeout(); got == 0 {
		t.Error("with no caller timeout the wait must still be bounded, not unbounded")
	}
}

// tokenObservingManagerServer stands in for a peer managerd and records
// the credential it was actually presented with, which is the only way
// to check the forwarding path attaches the right one.
type tokenObservingManagerServer struct {
	rpcpb.UnimplementedManagerServiceServer

	mu        sync.Mutex
	presented string
	calls     int
	gotTarget string
}

func (s *tokenObservingManagerServer) StepAsideForRestart(ctx context.Context, req *rpcpb.StepAsideForRestartRequest) (*rpcpb.StepAsideForRestartResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	auth := ""
	if v := md.Get("authorization"); len(v) > 0 {
		auth = v[0]
	}
	s.mu.Lock()
	s.calls++
	s.presented = auth
	s.gotTarget = req.GetTargetNodeId()
	s.mu.Unlock()
	return &rpcpb.StepAsideForRestartResponse{NodeId: req.GetTargetNodeId(), SafeToRestart: true, Detail: "not the leader"}, nil
}

func (s *tokenObservingManagerServer) snapshot() (presented string, calls int, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.presented, s.calls, s.gotTarget
}

// TestPeerReporterStepAsideForRestartAuthenticatesWithTheGuardrailToken
// is the check the doc comment makes a claim about, made real. The
// receiving handler authorizes by the dedicated token and not by the
// API-key role hierarchy, so a forward that attached p.APIKey would be
// rejected at the far end - which is why this stands up a real gRPC
// server and looks at what arrived, rather than asserting on a field.
func TestPeerReporterStepAsideForRestartAuthenticatesWithTheGuardrailToken(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	srv := grpc.NewServer(grpc.UnaryInterceptor(
		func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			v := md.Get("authorization")
			presented := ""
			if len(v) > 0 {
				presented = v[0]
			}
			if !strings.Contains(presented, "guardrail-token") {
				return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
			}
			return handler(ctx, req)
		}))
	peer := &tokenObservingManagerServer{}
	rpcpb.RegisterManagerServiceServer(srv, peer)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	p := &PeerReporter{APIKey: "colony-api-key", RestartGuardrailToken: "guardrail-token"}
	resp, err := p.StepAsideForRestart(context.Background(), lis.Addr().String(), &rpcpb.StepAsideForRestartRequest{TargetNodeId: "drone"})
	if err != nil {
		t.Fatalf("StepAsideForRestart: %v", err)
	}
	if !resp.GetSafeToRestart() {
		t.Error("SafeToRestart = false, want true")
	}
	presented, calls, target := peer.snapshot()
	if calls != 1 {
		t.Errorf("the peer handler ran %d times, want 1", calls)
	}
	if target != "drone" {
		t.Errorf("the peer saw target %q, want drone - the request's own contents must survive the hop unchanged", target)
	}
	if strings.Contains(presented, "colony-api-key") {
		t.Error("the ordinary peer API key was sent on a step-aside; that credential is not authorized for this RPC and sending it is a needless exposure")
	}
	if !strings.Contains(presented, "guardrail-token") {
		t.Errorf("presented credential = %q, want the restart-guardrail token", presented)
	}
}

// TestPeerReporterStepAsideForRestartIsRefusedWithoutTheGuardrailToken
// is the negative of the same claim, and it is the one that matters
// operationally: a colony with no guardrail token provisioned must get
// a refusal, not a step-aside performed under a weaker credential.
func TestPeerReporterStepAsideForRestartIsRefusedWithoutTheGuardrailToken(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	srv := grpc.NewServer(grpc.UnaryInterceptor(
		func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			v := md.Get("authorization")
			if len(v) == 0 || !strings.Contains(v[0], "guardrail-token") {
				return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
			}
			return handler(ctx, req)
		}))
	peer := &tokenObservingManagerServer{}
	rpcpb.RegisterManagerServiceServer(srv, peer)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	p := &PeerReporter{APIKey: "colony-api-key"}
	if _, err := p.StepAsideForRestart(context.Background(), lis.Addr().String(), &rpcpb.StepAsideForRestartRequest{TargetNodeId: "drone"}); err == nil {
		t.Fatal("err = nil, want PermissionDenied: a step-aside with only an API key must not be accepted")
	}
	if _, calls, _ := peer.snapshot(); calls != 0 {
		t.Errorf("the handler ran %d time(s) despite the interceptor refusing; authorization must happen before any work", calls)
	}
}

// TestStepAsiderDischargesTheLeaderRestartBlockInARealEngine closes the
// loop between the two halves: the seam restartplan calls and the reach
// that now supplies it. internal/restartplan's own composition tests
// supply a fake StepAside, so they cannot see a disagreement at this
// boundary - and a disagreement here would be invisible in both places
// while making a real leader-target restart silently un-reachable.
//
// The Gatherer is the honest version of the colony after a handover: the
// target is still a voter, but leadership has moved, so the fresh fact
// read taken after the step sees a follower. That is the whole reason
// the step has to run before the evaluation.
func TestStepAsiderDischargesTheLeaderRestartBlockInARealEngine(t *testing.T) {
	raft := &fakeStepAsideRaft{
		servers: members(),
		localResp: &internalpb.StepAsideForRestartResponse{
			NodeId:        "brood",
			WasLeader:     true,
			Transferred:   true,
			NewLeaderId:   "drone",
			SafeToRestart: true,
			Detail:        "brood handed over leadership to drone and is now a follower",
		},
	}
	dir := t.TempDir()
	var restarted, leased int
	var leasedWithForce bool
	engine := &restartplan.Engine{
		Service: restartplan.DefaultService,
		NodeID:  "brood",
		StepAsider: &restartStepAsider{
			localNodeID: "brood",
			raft:        raft,
		},
		Gatherer: restartplan.FactGathererFunc(func(_ context.Context, service, nodeID string) restartplan.QuorumFact {
			return restartplan.QuorumFact{
				Service:       service,
				TargetNodeID:  nodeID,
				IsTargetVoter: true,
				// Read AFTER the step-aside ran, so leadership has
				// already moved off the target. A leader here would
				// mean the handover did not take effect.
				IsTargetLeader: false,
				ProbeReadOK:    true,
				OtherVoters: []restartplan.VoterReachability{
					{NodeID: "drone", RaftBindAddress: "10.90.0.95:17600", Reachability: cluster.ReachabilityReachable},
					{NodeID: "buzz", RaftBindAddress: "10.90.0.96:17600", Reachability: cluster.ReachabilityReachable},
				},
			}
		}),
		Leaser: restartplan.LeaserFunc(func(_ context.Context, _ string, _ string, force bool) (restartplan.Lease, error) {
			leased++
			leasedWithForce = force
			return restartplan.Lease{ID: 4321}, nil
		}),
		Restarter: restartplan.RestarterFunc(func(context.Context, string) error {
			restarted++
			return nil
		}),
		Confirmer: restartplan.ConfirmerFunc(func(context.Context, string, string, uint64) error { return nil }),
		Pending:   restartplan.NewPendingStore(dir + "/pending"),
		Results:   restartplan.NewResultStore(dir + "/results"),
	}

	res, err := engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != restartplan.OutcomeConfirmed {
		t.Errorf("Outcome = %q, want %q; a confirmed handover must discharge the leader-restart block without an operator acknowledgment", res.Outcome, restartplan.OutcomeConfirmed)
	}
	if strings.Contains(res.Detail, "leader") && strings.Contains(res.Detail, "election") {
		t.Errorf("Detail = %q, want no leader-restart reason: the step-aside should have discharged that rule", res.Detail)
	}
	if leasedWithForce {
		t.Error("the lease was requested with force=true; nothing set it, and the whole point is that a leader-target no longer needs an operator acknowledgment")
	}
	if restarted != 1 || leased != 1 {
		t.Errorf("restarts = %d, leases = %d; want exactly 1 of each", restarted, leased)
	}
	if res.StepAside == nil {
		t.Fatal("the durable result carries no step-aside record")
	}
	if !res.StepAside.Attempted || !res.StepAside.WasLeader || !res.StepAside.Transferred {
		t.Errorf("StepAside record = %+v, want it to show an attempted handover of leadership", res.StepAside)
	}
	if res.StepAside.NewLeaderID != "drone" {
		t.Errorf("StepAside.NewLeaderID = %q, want drone", res.StepAside.NewLeaderID)
	}
	if !res.StepAside.SafeToRestart || res.StepAside.Detail == "" {
		t.Errorf("StepAside record = %+v, want a confirmed, explained handover", res.StepAside)
	}
}

// TestStepAsiderStopsARealEngineBeforeAnythingElse is the same engine,
// same target, same facts, with only the step-aside's answer changed.
// The colony here is still led by the target when the restart is
// evaluated, so without a confirmed handover the run must stop - and it
// must stop having reserved no lease and written no pending record,
// which is what makes the refusal recoverable rather than leaving
// cluster-wide state behind for a restart that never happened.
func TestStepAsiderStopsARealEngineBeforeAnythingElse(t *testing.T) {
	raft := &fakeStepAsideRaft{
		servers: members(),
		localResp: &internalpb.StepAsideForRestartResponse{
			NodeId:        "brood",
			WasLeader:     true,
			SafeToRestart: false,
			Detail:        "brood is still the raft leader",
			Error:         "leadership transfer did not complete within the deadline",
		},
	}
	dir := t.TempDir()
	var restarted, leased int
	engine := &restartplan.Engine{
		Service:    restartplan.DefaultService,
		NodeID:     "brood",
		StepAsider: &restartStepAsider{localNodeID: "brood", raft: raft},
		Gatherer: restartplan.FactGathererFunc(func(_ context.Context, service, nodeID string) restartplan.QuorumFact {
			return restartplan.QuorumFact{
				Service: service, TargetNodeID: nodeID,
				IsTargetVoter: true, IsTargetLeader: true, ProbeReadOK: true,
				OtherVoters: []restartplan.VoterReachability{
					{NodeID: "drone", RaftBindAddress: "10.90.0.95:17600", Reachability: cluster.ReachabilityReachable},
					{NodeID: "buzz", RaftBindAddress: "10.90.0.96:17600", Reachability: cluster.ReachabilityReachable},
				},
			}
		}),
		Leaser: restartplan.LeaserFunc(func(context.Context, string, string, bool) (restartplan.Lease, error) {
			leased++
			return restartplan.Lease{ID: 4321}, nil
		}),
		Restarter: restartplan.RestarterFunc(func(context.Context, string) error {
			restarted++
			return nil
		}),
		Confirmer: restartplan.ConfirmerFunc(func(context.Context, string, string, uint64) error { return nil }),
		Pending:   restartplan.NewPendingStore(dir + "/pending"),
		Results:   restartplan.NewResultStore(dir + "/results"),
		// Force is the strongest operator signal this engine has. It
		// must not rescue an unconfirmed handover.
		Force: true,
	}

	res, err := engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != restartplan.OutcomeBlocked {
		t.Errorf("Outcome = %q, want %q", res.Outcome, restartplan.OutcomeBlocked)
	}
	if restarted != 0 {
		t.Errorf("restarts = %d, want 0: an unconfirmed handover means the node may still be leader", restarted)
	}
	if leased != 0 {
		t.Errorf("leases = %d, want 0: the step runs first precisely so a refusal strands no cluster-wide lease", leased)
	}
	if res.StepAside == nil || res.StepAside.SafeToRestart {
		t.Errorf("StepAside record = %+v, want an explicit refusal recorded as evidence", res.StepAside)
	}
	if !strings.Contains(res.Detail, "could not be confirmed") {
		t.Errorf("Detail = %q, want it to say the node could not be confirmed out of leadership", res.Detail)
	}
}
