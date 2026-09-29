package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/guardrail"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// The join-log guardrail's integration tests. Every one of these drives a
// REAL single-node raft through the real RPCs, and the load-bearing
// assertion in the refusal cases is not the error message but the
// membership afterwards: a refusal that merely failed to call AddVoter by
// accident would look identical from the outside, whereas a node that is
// still absent from the raft configuration proves the irreversible call
// was never made.

// newJoinLogGuardServer brings up a real single-node raft plus a real
// managerd Server on top of it, with the reachability dial stubbed to
// succeed. Reachability is stubbed rather than dialled because these tests
// are about the log guard, and a real dial to a non-existent joiner would
// be refused by ADR-0097's check first, never reaching the check under
// test.
func newJoinLogGuardServer(t *testing.T) (rpcpb.ManagerServiceClient, *Server) {
	t.Helper()
	raftdSocket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
	srv.reachabilityCheck = func(context.Context, string) error { return nil }
	// Only a leader can call AddVoter, and a freshly bootstrapped
	// single-node raft takes an election timeout to get there. Waiting here
	// keeps the tests below about the log guard rather than about raft
	// startup timing.
	eventually(t, 15*time.Second, func() bool {
		status, err := srv.raft.Status(context.Background())
		return err == nil && status.GetIsLeader()
	})
	return client, srv
}

// seedPendingJoinRequest writes a PendingJoinRequest straight into the FSM,
// bypassing RequestJoinColony. This is the only honest way to reach the
// two states that matter for the enforcement point, because
// RequestJoinColony now refuses a known-bad reading when it records one:
//
//   - a record with no evidence at all, which is what an OLDER managerd
//     writes, and
//   - a record whose evidence has since gone bad, which is what the
//     accepted preflight race produces.
//
// Both are real, reachable states, and neither can be produced through the
// RPC once the guard is in place.
func seedPendingJoinRequest(t *testing.T, srv *Server, requestID, nodeID, raftBindAddress string, observed bool, lastIndex uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Stamp the request with the epoch of the window that is live NOW.
	// ADR-0147 Part 4 makes approval require the request and the window
	// to share an opened_at_unix, so a request seeded without one is
	// not approvable at all - and it is refused for exactly that reason
	// before any log-guard rule is reached. That is correct behavior
	// (a request predating the window must not be finished under it),
	// and it means a seeder that omits the field is testing the epoch
	// check while claiming to test the log guard.
	window, live := srv.liveColonyJoinWindow(ctx)
	if !live {
		t.Fatal("seedPendingJoinRequest: no live Colony join window; the request would be created under no window and refused as stale, so the test would pass for the wrong reason")
	}

	create := &internalpb.Command{Op: &internalpb.Command_CreatePendingJoinRequest{
		CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
			Request: &internalpb.PendingJoinRequest{
				RequestId: requestID, NodeId: nodeID, RaftBindAddress: raftBindAddress, Code: "123456",
				RequestedAtUnix: time.Now().Unix(), ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
				Status:                 internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
				JoinerLogStateObserved: observed,
				JoinerLastLogIndex:     lastIndex,
				WindowOpenedAtUnix:     window.GetOpenedAtUnix(),
			},
		},
	}}
	payload, err := proto.Marshal(create)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if resp, err := srv.raft.Apply(ctx, payload, defaultApplyTimeout); err != nil || resp.GetError() != "" {
		t.Fatalf("seeding a pending request: err=%v resp=%q", err, resp.GetError())
	}
}

// isRaftMember reports whether nodeID appears in this raft's own
// configuration. It reads the real membership rather than trusting the
// RPC's error string, because "the RPC said no" and "no membership change
// was committed" are different claims and only the second one is the
// safety property.
func isRaftMember(t *testing.T, srv *Server, nodeID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := srv.raft.Status(ctx)
	if err != nil {
		t.Fatalf("reading raft membership: %v", err)
	}
	for _, s := range status.GetServers() {
		if s.GetId() == nodeID {
			return true
		}
	}
	return false
}

// TestIntegration_ApproveJoinRequest_NonEmptyJoinerLogRefusedAndNeverAdded
// is the defect this whole change exists for. The joiner reports a
// non-empty raft log - it was previously in a cluster, or bootstrapped on
// its own - and approval must be refused with the node still absent from
// the configuration afterwards.
func TestIntegration_ApproveJoinRequest_NonEmptyJoinerLogRefusedAndNeverAdded(t *testing.T) {
	client, srv := newJoinLogGuardServer(t)
	openColonyJoinWindowForTest(t, client)
	seedPendingJoinRequest(t, srv, "jreq-bad", "node02", "10.62.0.5:17600", true, 4096)

	// ADR-0147 Part 3: the operator's authorization is done FIRST, so the
	// refusal this test is about is the log guard's and not the new
	// gate's. A test that passes by being refused two checks earlier
	// looks green while asserting something else entirely.
	resp, _ := operatorAuthorizedApprovalForTest(t, client, srv, "jreq-bad")
	if resp.GetError() == "" {
		t.Fatal("ApproveJoinRequest() approved a joiner with a non-empty raft log, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "raft log index 4096") {
		t.Errorf("Error = %q, want it to name the observed log index", resp.GetError())
	}
	if resp.GetRequest() != nil {
		t.Errorf("Request = %+v, want nil - nothing should have been approved", resp.GetRequest())
	}
	if isRaftMember(t, srv, "node02") {
		t.Fatal("node02 is a raft voter after a refused approval: AddVoter was reached, which is the data-loss path this guardrail exists to prevent")
	}
}

// TestIntegration_ApproveJoinRequest_UnobservedJoinerLogRefused is the
// fail-closed case, and the one most likely to be broken by a well-meaning
// future edit. A pending request carrying no evidence at all is what an
// older managerd writes. It must be refused, because "this build never
// heard of the check" is not grounds for performing the irreversible
// action the check exists to prevent.
func TestIntegration_ApproveJoinRequest_UnobservedJoinerLogRefused(t *testing.T) {
	client, srv := newJoinLogGuardServer(t)
	openColonyJoinWindowForTest(t, client)
	seedPendingJoinRequest(t, srv, "jreq-old", "node03", "10.62.0.5:17600", false, 0)

	// ADR-0147 Part 3: as in the sibling test above - the operator's
	// authorization is done first, so the refusal under test is the
	// log guard's and not the two-person gate's.
	resp, _ := operatorAuthorizedApprovalForTest(t, client, srv, "jreq-old")
	if resp.GetError() == "" {
		t.Fatal("ApproveJoinRequest() approved a joiner with NO evidence about its log, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "no evidence") {
		t.Errorf("Error = %q, want it to say the evidence was absent rather than that the log was bad", resp.GetError())
	}
	if isRaftMember(t, srv, "node03") {
		t.Fatal("node03 is a raft voter after a refused approval on absent evidence")
	}
}

// TestIntegration_ApproveJoinRequest_ObservedEmptyLogStillApproved is the
// control. Without it, a guardrail that refuses everything would pass every
// test above, and the Colony could never grow again.
func TestIntegration_ApproveJoinRequest_ObservedEmptyLogStillApproved(t *testing.T) {
	client, srv := newJoinLogGuardServer(t)

	// A REAL, unbootstrapped second raft node, mirroring
	// TestIntegration_ApproveJoinRequest_AddsRealRaftVoter's own pattern
	// and for the same reason. Approving a node whose address nothing is
	// listening on takes a single-voter colony to two voters, so quorum
	// becomes 2-of-2, the leader cannot reach the new member, and it steps
	// down within a few hundred milliseconds. That failure would be
	// reported as a refusal by this test, which is exactly the wrong lesson
	// to teach - it is ADR-0097's hazard, not the log guard's.
	joiningAddr := freeLoopbackAddr(t)
	joiningNode, err := raftnode.New(raftnode.Config{NodeID: "node04", DataDir: t.TempDir(), BindAddr: joiningAddr})
	if err != nil {
		t.Fatalf("raftnode.New(node04) error: %v", err)
	}
	t.Cleanup(func() { joiningNode.Shutdown() })

	openColonyJoinWindowForTest(t, client)
	seedPendingJoinRequest(t, srv, "jreq-good", "node04", joiningAddr, true, 0)

	// ADR-0147 Part 3: the operator's authorization, from two Combs with
	// two keys, before the log guard is even consulted.
	resp, _ := operatorAuthorizedApprovalForTest(t, client, srv, "jreq-good")
	if resp.GetError() != "" {
		t.Fatalf("ApproveJoinRequest() refused a joiner whose log is genuinely empty: %s", resp.GetError())
	}
	if !isRaftMember(t, srv, "node04") {
		t.Fatal("node04 was not added as a voter despite an observed empty log and a successful approval")
	}
}

// TestIntegration_RequestJoinColony_NonEmptyLogRefusedAtRequestTime is the
// early, friendlier half of the same rule: a joining Comb that already has
// log entries never becomes a pending request an Admin could approve, so
// the operator is told at the moment they can still act on it.
func TestIntegration_RequestJoinColony_NonEmptyLogRefusedAtRequestTime(t *testing.T) {
	client, _ := newJoinLogGuardServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	resp, err := client.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{
		NodeId: "node05", RaftBindAddress: "10.62.0.5:17600",
		JoinerLogStateObserved: true, JoinerLastLogIndex: 12,
	})
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("RequestJoinColony() recorded a request for a joiner with a non-empty log, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "wipe") {
		t.Errorf("Error = %q, want it to say what to do about it", resp.GetError())
	}
	if resp.GetRequestId() != "" {
		t.Errorf("RequestId = %q, want empty - no request should have been recorded", resp.GetRequestId())
	}
}

// TestIntegration_RequestJoinColony_ObservedEmptyLogRecordsEvidence proves
// the happy path records the evidence, so the Admin's list and the approval
// both have something real to work from rather than an unobserved request
// that can only ever be refused.
func TestIntegration_RequestJoinColony_ObservedEmptyLogRecordsEvidence(t *testing.T) {
	client, _ := newJoinLogGuardServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	resp, err := client.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{
		NodeId: "node06", RaftBindAddress: "10.62.0.5:17600",
		JoinerLogStateObserved: true,
	})
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony() = %q, want a recorded request", resp.GetError())
	}
	status, err := client.GetJoinRequestStatus(ctx, &rpcpb.GetJoinRequestStatusRequest{RequestId: resp.GetRequestId()})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus() error: %v", err)
	}
	if got := status.GetRequest(); !got.GetJoinerLogStateObserved() || got.GetJoinerLastLogIndex() != 0 {
		t.Errorf("recorded evidence = (observed %v, index %d), want (true, 0)", got.GetJoinerLogStateObserved(), got.GetJoinerLastLogIndex())
	}
}

// TestIntegration_PreflightAndApproveAgreeOnTheLogGuard is the drift check
// ADR-0103 asks for. The preview exists so an operator can see the verdict
// BEFORE committing to the action, which is worth nothing if the preview
// says allow and the approval then refuses. Both must return the same
// verdict for the same request.
func TestIntegration_PreflightAndApproveAgreeOnTheLogGuard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed bool
		index    uint64
	}{
		{"empty log", true, 0},
		{"non-empty log", true, 99},
		{"no evidence", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, srv := newJoinLogGuardServer(t)
			// A real reachable address for every case, including the two
			// that never get as far as AddVoter. The allowed case genuinely
			// commits a membership change, and pointing that at a dead
			// address would take the colony to 2-of-2 quorum and drop its
			// leadership - which this test would then read as a disagreement
			// between preflight and approval that has nothing to do with
			// either of them.
			joiningAddr := freeLoopbackAddr(t)
			joiningNode, err := raftnode.New(raftnode.Config{NodeID: "node07", DataDir: t.TempDir(), BindAddr: joiningAddr})
			if err != nil {
				t.Fatalf("raftnode.New(node07) error: %v", err)
			}
			t.Cleanup(func() { joiningNode.Shutdown() })
			openColonyJoinWindowForTest(t, client)
			seedPendingJoinRequest(t, srv, "jreq-pf", "node07", joiningAddr, tc.observed, tc.index)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			pre, err := client.PreflightApproveJoinRequest(ctx, &rpcpb.PreflightApproveJoinRequestRequest{RequestId: "jreq-pf"})
			if err != nil {
				t.Fatalf("PreflightApproveJoinRequest() error: %v", err)
			}
			allowed := pre.GetVerdict() == string(guardrail.Allow)
			if tc.index == 0 && tc.observed && allowed != true {
				t.Errorf("preflight verdict = %q, want allow for an observed empty log", pre.GetVerdict())
			}
			if !allowed && len(pre.GetFindings()) == 0 {
				t.Error("preflight refused but reported no findings, so the operator is told nothing about why")
			}

			// ADR-0147 Part 3: the operator's authorization, so that the
			// agreement this test is about is between preflight and the
			// log-guard check rather than between preflight and a refusal
			// from a gate that did not exist when preflight was written.
			app, _ := operatorAuthorizedApprovalForTest(t, client, srv, "jreq-pf")
			if approved := app.GetError() == ""; approved != allowed {
				t.Errorf("preflight said allowed=%v but approval said approved=%v, want the two to agree", allowed, approved)
			}
		})
	}
}
