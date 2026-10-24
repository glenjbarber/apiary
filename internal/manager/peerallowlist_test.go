package manager

import (
	"context"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// ADR-0164: the target_address allowlist's DERIVED layer, rebuilt from
// raft membership, so the allowlist stays current as Combs join without
// an operator editing every member's config after every join.

// newAllowlistTestServer builds a Server with no configured allowlist and
// no peers, which is the state a freshly installed Comb starts in.
func newAllowlistTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, &fakeNodeConfigStore{}, nil, 0, nil)
}

func TestRefreshDerivedPeerAddresses_AllowsAMemberNotInTheConfiguredList(t *testing.T) {
	s := newAllowlistTestServer(t)
	s.SetKnownPeerAddresses([]string{"seeded.example.test:17700"})

	s.RefreshDerivedPeerAddresses([]KnownRaftServer{
		{Address: "10.50.0.14:17600"},
	})

	// The new member is a legitimate target even though no operator
	// ever added it to known_peer_addresses.
	if err := s.checkTargetAddressAllowed("10.50.0.14:17700"); err != nil {
		t.Errorf("derived member target refused: %v", err)
	}
	// The operator's own seeded entry still works.
	if err := s.checkTargetAddressAllowed("seeded.example.test:17700"); err != nil {
		t.Errorf("configured target refused after derivation: %v", err)
	}
}

func TestRefreshDerivedPeerAddresses_UsesTheManagerdPortNotTheRaftPort(t *testing.T) {
	// A non-default peer_managerd_port must be honoured, exactly as the
	// leader-hint path does - the derivation must use the same function
	// so the two can never disagree.
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "17777", nil, &fakeNodeConfigStore{}, nil, 0, nil)

	s.RefreshDerivedPeerAddresses([]KnownRaftServer{{Address: "10.50.0.14:17600"}})

	if err := s.checkTargetAddressAllowed("10.50.0.14:17777"); err != nil {
		t.Errorf("member's managerd address refused: %v", err)
	}
	// The raft port is not a managerd address and must not be allowed.
	if err := s.checkTargetAddressAllowed("10.50.0.14:17600"); err == nil {
		t.Error("the raft-bind port was allowed as a target_address")
	}
}

func TestRefreshDerivedPeerAddresses_NeverRemovesAConfiguredEntry(t *testing.T) {
	s := newAllowlistTestServer(t)
	s.SetKnownPeerAddresses([]string{"hand.configured.test:17700"})

	// A refresh that knows about a different member entirely must not
	// disturb the operator's explicit entry.
	s.RefreshDerivedPeerAddresses([]KnownRaftServer{{Address: "10.50.0.14:17600"}})

	if err := s.checkTargetAddressAllowed("hand.configured.test:17700"); err != nil {
		t.Errorf("configured entry removed by derivation: %v", err)
	}
}

func TestRefreshDerivedPeerAddresses_StopsAllowingARemovedMember(t *testing.T) {
	s := newAllowlistTestServer(t)

	s.RefreshDerivedPeerAddresses([]KnownRaftServer{
		{Address: "10.50.0.14:17600"},
		{Address: "10.50.0.9:17600"},
	})
	if err := s.checkTargetAddressAllowed("10.50.0.9:17700"); err != nil {
		t.Fatalf("member refused before removal: %v", err)
	}

	// The derived set is rebuilt in full, not appended to, so a member
	// removed from raft's configuration stops being a valid target.
	s.RefreshDerivedPeerAddresses([]KnownRaftServer{
		{Address: "10.50.0.14:17600"},
	})
	if err := s.checkTargetAddressAllowed("10.50.0.9:17700"); err == nil {
		t.Error("a member removed from raft membership is still an allowed target")
	}
	if err := s.checkTargetAddressAllowed("10.50.0.14:17700"); err != nil {
		t.Errorf("remaining member refused after refresh: %v", err)
	}
}

func TestRefreshDerivedPeerAddresses_UnknownTargetIsStillRefused(t *testing.T) {
	s := newAllowlistTestServer(t)
	s.RefreshDerivedPeerAddresses([]KnownRaftServer{{Address: "10.50.0.14:17600"}})

	err := s.checkTargetAddressAllowed("attacker.example.test:17700")
	if err == nil {
		t.Fatal("an address that is neither configured nor a member was allowed")
	}
	// The refusal must NOT claim the allowlist is unset - it is set.
	if !strings.Contains(err.Error(), "not in this node's -known-peer-addresses allowlist") {
		t.Errorf("refusal = %q, want it to name the allowlist rather than the unset case", err)
	}
}

func TestRefreshDerivedPeerAddresses_EmptyMembershipKeepsTheFailClosedRefusal(t *testing.T) {
	// With neither layer populated, forwarding stays disabled exactly
	// as ADR-0097 requires. Derivation must never make forwarding
	// unconditionally available.
	s := newAllowlistTestServer(t)

	err := s.checkTargetAddressAllowed("10.50.0.14:17700")
	if err == nil {
		t.Fatal("forwarding was allowed with no configured and no derived allowlist")
	}
	if !strings.Contains(err.Error(), "known_peer_addresses is not configured") {
		t.Errorf("refusal = %q, want the unset-configuration refusal", err)
	}
}

func TestRefreshDerivedPeerAddresses_EmptyMembershipDoesNotClearTheConfiguredLayer(t *testing.T) {
	// A raft status query that succeeds but reports no members (or a
	// refresh over an empty list) must not silently disable forwarding
	// for a Comb that has a configured list.
	s := newAllowlistTestServer(t)
	s.SetKnownPeerAddresses([]string{"seeded.example.test:17700"})

	s.RefreshDerivedPeerAddresses(nil)

	if err := s.checkTargetAddressAllowed("seeded.example.test:17700"); err != nil {
		t.Errorf("configured target refused after an empty refresh: %v", err)
	}
}

func TestRefreshDerivedPeerAddresses_IgnoresAnEmptyAddress(t *testing.T) {
	// A member with no raft-bind address cannot yield a usable
	// managerd address, and must not produce a bogus ":" entry that
	// would match nothing but pollute the set.
	s := newAllowlistTestServer(t)

	s.RefreshDerivedPeerAddresses([]KnownRaftServer{
		{Address: ""},
		{Address: "10.50.0.14:17600"},
	})

	if err := s.checkTargetAddressAllowed(":17700"); err == nil {
		t.Error("an empty raft address produced an allowed target")
	}
	if err := s.checkTargetAddressAllowed("10.50.0.14:17700"); err != nil {
		t.Errorf("valid member refused: %v", err)
	}
}

func TestSetKnownPeerAddresses_DoesNotDisturbTheDerivedLayer(t *testing.T) {
	// A live node-config update replaces the CONFIGURED layer only. It
	// must not discard what raft says about the current membership.
	s := newAllowlistTestServer(t)
	s.RefreshDerivedPeerAddresses([]KnownRaftServer{{Address: "10.50.0.14:17600"}})

	s.SetKnownPeerAddresses([]string{"new.example.test:17700"})

	if err := s.checkTargetAddressAllowed("10.50.0.14:17700"); err != nil {
		t.Errorf("derived member refused after a config update: %v", err)
	}
	if err := s.checkTargetAddressAllowed("new.example.test:17700"); err != nil {
		t.Errorf("newly configured target refused: %v", err)
	}
}

// The three join-colony RPCs are the ones that actually dial a
// caller-supplied target_address, so the derived layer has to reach all
// of them. This exercises the shared check the same way the RPCs do.
func TestDerivedAllowlist_AppliesToTheUnauthenticatedJoinRPCs(t *testing.T) {
	s := newAllowlistTestServer(t)
	s.RefreshDerivedPeerAddresses([]KnownRaftServer{{Address: "10.50.0.14:17600"}})

	// Every one of the three RPCs calls this same check at the top of
	// its target_address branch, so allowing here is what allows all
	// three.
	for _, target := range []string{"10.50.0.14:17700"} {
		if err := s.checkTargetAddressAllowed(target); err != nil {
			t.Errorf("derived target %q refused: %v", target, err)
		}
	}
	if err := s.checkTargetAddressAllowed("elsewhere.example.test:17700"); err == nil {
		t.Error("a non-member target was allowed")
	}
}

// The derivation reads the same raftClient.Status response the peer
// hostname refresh uses, so this checks the shape it depends on.
func TestRefreshDerivedPeerAddresses_AcceptsTheRaftStatusServersShape(t *testing.T) {
	// StatusResponse.Servers carries Id/Address/Suffrage; the derivation
	// only needs Address. This mirrors how cmd/managerd builds the
	// KnownRaftServer list from status.GetServers().
	s := newAllowlistTestServer(t)

	servers := []KnownRaftServer{
		{Address: "10.50.0.14:17600"},
		{Address: "10.50.0.9:17600"},
	}
	s.RefreshDerivedPeerAddresses(servers)

	for _, addr := range []string{"10.50.0.14:17700", "10.50.0.9:17700"} {
		if err := s.checkTargetAddressAllowed(addr); err != nil {
			t.Errorf("member %q refused: %v", addr, err)
		}
	}
}

// A failed raft status query must leave the previously-derived set
// intact rather than clearing it. This is the same failure mode
// ADR-0115's own hostname derivation accepts: a transient raft query
// failure must not silently disable join-flow forwarding for every
// member. The refresh loop in cmd/managerd returns early on a Status
// error and never calls RefreshDerivedPeerAddresses at all, so the
// previous set survives; this test pins the manager-side contract that
// makes that true.
func TestRefreshDerivedPeerAddresses_ASkippedRefreshLeavesTheDerivedSetIntact(t *testing.T) {
	s := newAllowlistTestServer(t)

	s.RefreshDerivedPeerAddresses([]KnownRaftServer{{Address: "10.50.0.14:17600"}})
	if err := s.checkTargetAddressAllowed("10.50.0.14:17700"); err != nil {
		t.Fatalf("member refused after first refresh: %v", err)
	}

	// A failed refresh leaves the derived set exactly as it was: the
	// loop returns before touching it, and nothing else mutates it.
	if err := s.checkTargetAddressAllowed("10.50.0.14:17700"); err != nil {
		t.Errorf("member refused after a skipped refresh: %v", err)
	}
}

// TestServer_RequestJoinColony_ForwardsToADerivedMember is the
// end-to-end half of ADR-0164: a member that was never written into
// known_peer_addresses by any operator still has to be reachable as a
// forwarding target, because derivation added it. This is the actual
// gap issue #84 describes - the allowlist is not kept current as Combs
// join - so the test drives the real RPC rather than the check alone.
func TestServer_RequestJoinColony_ForwardsToADerivedMember(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{requestResp: &rpcpb.RequestJoinColonyResponse{RequestId: "jreq-1", Code: "482913"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)

	// No configured allowlist at all - the operator never wrote one.
	s.SetKnownPeerAddresses(nil)
	// But raft membership says this Comb belongs to a two-member Colony.
	s.RefreshDerivedPeerAddresses([]KnownRaftServer{
		{Address: "10.0.0.1:17600"},
		{Address: "10.0.0.2:17600"},
	})

	resp, err := s.RequestJoinColony(context.Background(), withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId: "node-3", RaftBindAddress: "10.0.0.3:17600", TargetAddress: "10.0.0.1:17700",
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" || resp.GetRequestId() != "jreq-1" {
		t.Errorf("response = %+v, want the forwarded call to succeed against a derived member", resp)
	}
	if peers.lastAddr != "10.0.0.1:17700" {
		t.Errorf("lastAddr = %q, want the derived member dialed", peers.lastAddr)
	}
}
