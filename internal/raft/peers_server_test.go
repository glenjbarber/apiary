package raft

// Wire-level tests for ADR-0147 Part 4's peer trust store, against a
// real three-voter cluster rather than a bare FSM.
//
// The reason is the same as everywhere else in this file's neighbours:
// the removal hook's whole contract is about what the trust store looks
// like after real raft membership changes, including on the followers,
// which are the Combs that write the derived peer-ca.pem. A fake would
// answer whatever the fake was told.

import (
	"context"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

func newPeerTrustServerCluster(t *testing.T) []*Server {
	t.Helper()
	nodes := threeNodeCluster(t)
	servers := make([]*Server, len(nodes))
	for i, n := range nodes {
		servers[i] = NewServer(n)
	}
	return servers
}

func (s *Server) applyTrust(t *testing.T, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	res, err := s.node.Apply(mustMarshalCommand(t, cmd), 10*time.Second)
	if err != nil {
		return &FSMApplyResult{Error: err.Error()}
	}
	return res
}

// everyVoterReportsTheSamePins is the assertion the derived
// peer-ca.pem writer rests on: every Comb in the Colony writes that
// file from its own replicated copy, so a disagreement here is a file
// that differs between hosts, which is exactly the failure the
// replicated store was chosen to prevent.
//
// It is a hard assertion, so it is only safe once replication has
// settled. pinsAgreed below is the eventually-safe form of the same
// check.
func everyVoterReportsTheSamePins(t *testing.T, servers []*Server) []*internalpb.TrustedPeer {
	t.Helper()
	var first []*internalpb.TrustedPeer
	for i, s := range servers {
		resp, err := s.ListTrustedPeersLocal(context.Background(), &internalpb.ListTrustedPeersRequest{})
		if err != nil {
			t.Fatalf("node %d ListTrustedPeersLocal() error: %v", i, err)
		}
		if i == 0 {
			first = resp.GetPeers()
			continue
		}
		if len(resp.GetPeers()) != len(first) {
			t.Fatalf("node %d reports %d pins, node 0 reports %d; the derived peer-ca.pem would differ between these two Combs",
				i, len(resp.GetPeers()), len(first))
		}
		for j := range first {
			if resp.GetPeers()[j].GetFingerprint() != first[j].GetFingerprint() {
				t.Errorf("node %d pin %d = %s, node 0 = %s", i, j, resp.GetPeers()[j].GetFingerprint(), first[j].GetFingerprint())
			}
		}
	}
	return first
}

// pinsAgreed is everyVoterReportsTheSamePins as an eventually
// predicate. A follower's FSM is behind the leader's until the log has
// actually reached it, so a mid-replication disagreement is the
// normal state of the seconds after a write, not a defect - and
// failing the test on it would report a replication lag as a trust
// store bug.
func pinsAgreed(servers []*Server) bool {
	var first []*internalpb.TrustedPeer
	for i, s := range servers {
		resp, err := s.ListTrustedPeersLocal(context.Background(), &internalpb.ListTrustedPeersRequest{})
		if err != nil {
			return false
		}
		if i == 0 {
			first = resp.GetPeers()
			continue
		}
		if len(resp.GetPeers()) != len(first) {
			return false
		}
		for j := range first {
			if resp.GetPeers()[j].GetFingerprint() != first[j].GetFingerprint() ||
				resp.GetPeers()[j].GetIsVoter() != first[j].GetIsVoter() {
				return false
			}
		}
	}
	return true
}

// AddVoter promotes the pin, on every voter.
func TestServerAddVoterPromotesThePin(t *testing.T) {
	servers := newPeerTrustServerCluster(t)
	leader := servers[0]
	eventually(t, 10*time.Second, func() bool { return leader.node.Status().IsLeader })

	peer := newPinTestPeer(t, "n4", "drone")
	if res := leader.applyTrust(t, pinCmd(peer)); res.Error != "" {
		t.Fatalf("pin refused: %s", res.Error)
	}
	// The pin is replicated, but a follower's FSM is behind until the
	// log has actually reached it. Every write below goes through the
	// leader, so waiting for replication rather than sleeping is what
	// keeps this honest.
	eventually(t, 10*time.Second, func() bool { return pinsAgreed(servers) })

	if _, err := leader.AddVoter(context.Background(), &internalpb.AddVoterRequest{Id: "n4", Address: freeLoopbackAddr(t)}); err != nil {
		t.Fatalf("AddVoter() error: %v", err)
	}
	eventually(t, 10*time.Second, func() bool { return pinsAgreed(servers) })

	peers := everyVoterReportsTheSamePins(t, servers)
	if len(peers) != 1 {
		t.Fatalf("every voter reports %d pins, want 1", len(peers))
	}
	if !peers[0].GetIsVoter() {
		t.Error("the pin is not a member pin after AddVoter, want is_voter true")
	}
}

// RemoveServer drops the entry. "Removal is real": a store that can
// only grow accumulates dead certificates until one expires by
// accident.
func TestServerRemoveServerDropsThePin(t *testing.T) {
	servers := newPeerTrustServerCluster(t)
	leader := servers[0]
	eventually(t, 10*time.Second, func() bool { return leader.node.Status().IsLeader })

	// n2 is already a voter here, so the pin is promoted directly
	// rather than by re-adding it. AddVoter's own promotion is covered
	// by the test above, against a node that genuinely was not a member.
	if res := leader.applyTrust(t, pinCmd(newPinTestPeer(t, "n2", "drone"))); res.Error != "" {
		t.Fatalf("pin refused: %s", res.Error)
	}
	if res := leader.applyTrust(t, setVoterCmd("n2", true)); res.Error != "" {
		t.Fatalf("promotion refused: %s", res.Error)
	}
	eventually(t, 10*time.Second, func() bool { return pinsAgreed(servers) })

	if _, err := leader.RemoveServer(context.Background(), &internalpb.RemoveServerRequest{Id: "n2"}); err != nil {
		t.Fatalf("RemoveServer() error: %v", err)
	}

	// The remaining voters, and ONLY the remaining voters. A removed
	// server is no longer replicated to, so its own copy of the store
	// going stale is not a disagreement about trust - it is what
	// RemoveServer means, and the ADR records that a Combs that has
	// been removed is expected to be wiped rather than carried forward.
	remaining := []*Server{servers[0], servers[2]}
	eventually(t, 10*time.Second, func() bool { return pinsAgreed(remaining) })

	if got := len(everyVoterReportsTheSamePins(t, remaining)); got != 0 {
		t.Errorf("the remaining voters hold %d pins after the member was removed, want 0", got)
	}
}

// A member that was never pinned - a Colony upgraded from a
// pre-ADR-0147 build - can still be removed, and its removal is not
// reported as a failure. "Not pinned" is not a failed membership
// change, and UnpinTrustedPeer's own refusal of an unknown node would
// otherwise be surfaced as one.
func TestServerRemoveServerSucceedsForANodeThatWasNeverPinned(t *testing.T) {
	servers := newPeerTrustServerCluster(t)
	leader := servers[0]
	eventually(t, 10*time.Second, func() bool { return leader.node.Status().IsLeader })

	resp, err := leader.RemoveServer(context.Background(), &internalpb.RemoveServerRequest{Id: "n2"})
	if err != nil {
		t.Fatalf("RemoveServer() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Errorf("RemoveServer(n2) = %q, want success - a member that was never pinned has nothing to drop", resp.GetError())
	}
}
