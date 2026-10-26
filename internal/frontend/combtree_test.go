package frontend

// The sidebar's Colony tree (ADR-0144's `CombTree`).
//
// What these pin: that the tree carries MEMBERSHIP rather than a
// health verdict, that an unobserved leader is never marked, that the
// cells under a Comb are the ones that Comb owns, and that a tree
// which cannot be built renders nothing rather than something wrong.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// treeStatus builds a status response with the given members.
func treeStatus(members ...*rpcpb.RaftMember) *rpcpb.StatusResponse {
	return &rpcpb.StatusResponse{
		ManagerNodeId: "brood",
		KnownNodeIds:  []string{"brood", "drone"},
		Members:       members,
	}
}

// The tree carries each Comb's suffrage, and the cells it owns.
func TestCombTree_CarriesMembershipAndOwnedCells(t *testing.T) {
	srv := &Server{client: &fakeClient{
		statusResp: treeStatus(
			&rpcpb.RaftMember{NodeId: "brood", Address: "10.0.0.1:17700", Suffrage: "Voter"},
			&rpcpb.RaftMember{NodeId: "drone", Address: "10.0.0.2:17700", Suffrage: "Nonvoter"},
		),
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", NodeId: "brood", Name: "web"},
			{Id: "vm-2", NodeId: "drone"},
		}},
	}}

	view := srv.gatherCombTree(context.Background(), treeStatus(
		&rpcpb.RaftMember{NodeId: "brood", Address: "10.0.0.1:17700", Suffrage: "Voter"},
		&rpcpb.RaftMember{NodeId: "drone", Address: "10.0.0.2:17700", Suffrage: "Nonvoter"},
	))
	if len(view.Combs) != 2 {
		t.Fatalf("the tree holds %d Combs, want 2", len(view.Combs))
	}
	// Sorted by node ID, so the sidebar does not reorder itself.
	if view.Combs[0].NodeID != "brood" || view.Combs[1].NodeID != "drone" {
		t.Errorf("the tree is not sorted by node ID: %q then %q", view.Combs[0].NodeID, view.Combs[1].NodeID)
	}
	if view.Combs[0].Suffrage != "Voter" {
		t.Errorf("brood's suffrage = %q, want Voter", view.Combs[0].Suffrage)
	}
	if view.Combs[1].Suffrage != "Nonvoter" {
		t.Errorf("drone's suffrage = %q, want Nonvoter", view.Combs[1].Suffrage)
	}
	if view.Voters != 1 {
		t.Errorf("the tree counts %d voters, want 1", view.Voters)
	}
	// Each Comb carries the cells IT owns, not everyone else's.
	if len(view.Combs[0].Cells) != 1 || view.Combs[0].Cells[0].ID != "vm-1" {
		t.Errorf("brood's cells = %+v, want just vm-1", view.Combs[0].Cells)
	}
	if len(view.Combs[1].Cells) != 1 || view.Combs[1].Cells[0].ID != "vm-2" {
		t.Errorf("drone's cells = %+v, want just vm-2", view.Combs[1].Cells)
	}
	if view.Combs[0].Cells[0].Href != "/vms/vm-1" {
		t.Errorf("the cell's href = %q, want /vms/vm-1", view.Combs[0].Cells[0].Href)
	}
}

// A leader is marked only when raft's state was ACTUALLY READ.
//
// An unobserved reading must never imply a leader that was not seen -
// that is ADR-0123's rule, and a sidebar that invents a leader would
// send an operator to the wrong Comb.
func TestCombTree_MarksNoLeaderWhenTheLeaderWasNotObserved(t *testing.T) {
	srv := &Server{client: &fakeClient{statusResp: treeStatus(
		&rpcpb.RaftMember{NodeId: "brood", Suffrage: "Voter"},
	)}}
	// A status with NO leader observed.
	status := treeStatus(&rpcpb.RaftMember{NodeId: "brood", Suffrage: "Voter"})
	status.RaftNodeId = ""
	status.RaftState = ""

	view := srv.gatherCombTree(context.Background(), status)
	for _, comb := range view.Combs {
		if comb.IsLeader {
			t.Errorf("comb %q is marked as the leader, but no leader was observed", comb.NodeID)
		}
	}
}

// A Comb with an UNKNOWN suffrage is not counted as a voter.
//
// "Unknown" is a real, live value - any raft suffrage this build does
// not recognise - and treating it as a Voter would put the Colony's
// membership arithmetic on a value nobody observed.
func TestCombTree_UnknownSuffrageIsNotAVoter(t *testing.T) {
	srv := &Server{client: &fakeClient{statusResp: treeStatus(
		&rpcpb.RaftMember{NodeId: "brood", Suffrage: "Voter"},
		&rpcpb.RaftMember{NodeId: "drone", Suffrage: "Unknown"},
	)}}
	view := srv.gatherCombTree(context.Background(), treeStatus(
		&rpcpb.RaftMember{NodeId: "brood", Suffrage: "Voter"},
		&rpcpb.RaftMember{NodeId: "drone", Suffrage: "Unknown"},
	))
	if view.Voters != 1 {
		t.Errorf("the tree counts %d voters, want 1 - an Unknown suffrage is not a Voter", view.Voters)
	}
}

// No status means NO TREE.
//
// A tree with a Comb in it that cannot be verified is worse than no
// tree, so a managerd that cannot reach raft renders nothing.
func TestCombTree_NoStatusMeansNoTree(t *testing.T) {
	srv := &Server{client: &fakeClient{statusResp: treeStatus()}}
	view := srv.gatherCombTree(context.Background(), nil)
	if len(view.Combs) != 0 {
		t.Errorf("the tree holds %d Combs with no status, want none", len(view.Combs))
	}
}

// A membership view that is unavailable still names the Combs.
//
// The roster in `known_node_ids` is a fact about which Combs exist
// even when the per-member detail is missing, so the Combs are still
// linked to - but with NO suffrage, because a Comb whose suffrage was
// not observed must not be shown as a voter.
func TestCombTree_KnownRosterWithoutMembershipStillLinks(t *testing.T) {
	srv := &Server{client: &fakeClient{statusResp: treeStatus()}}
	status := treeStatus()
	status.Members = nil

	view := srv.gatherCombTree(context.Background(), status)
	if len(view.Combs) != 2 {
		t.Fatalf("the tree holds %d Combs, want the 2 the roster names", len(view.Combs))
	}
	for _, comb := range view.Combs {
		if comb.Suffrage != "" {
			t.Errorf("comb %q shows the suffrage %q, but the membership view was unavailable", comb.NodeID, comb.Suffrage)
		}
	}
	if view.Voters != 0 {
		t.Errorf("the tree counts %d voters with no membership view, want 0", view.Voters)
	}
}

// The cells are read ONCE each, and shared across every Comb.
//
// Two reads would put one Comb's cells from before a change and
// another's from after it, and the sidebar would show a Colony that
// never existed.
func TestCombTree_ReadsTheCellListsOnce(t *testing.T) {
	client := &fakeClient{
		statusResp: treeStatus(&rpcpb.RaftMember{NodeId: "brood", Suffrage: "Voter"}),
		listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1", NodeId: "brood"}}},
	}
	srv := &Server{client: client}
	srv.gatherCombTree(context.Background(), treeStatus(&rpcpb.RaftMember{NodeId: "brood", Suffrage: "Voter"}))
	if client.listCalls != 1 {
		t.Errorf("the VM list was read %d times, want 1", client.listCalls)
	}
}

// The tree RENDERS.
//
// The unit tests above check the data; this one checks that it reaches
// the operator's page, because a tree that is gathered correctly and
// never rendered is a tree nobody sees.
func TestCombTree_RendersInTheSidebar(t *testing.T) {
	client := &fakeClient{
		statusResp: treeStatus(
			&rpcpb.RaftMember{NodeId: "brood", Address: "10.0.0.1:17700", Suffrage: "Voter"},
			&rpcpb.RaftMember{NodeId: "drone", Address: "10.0.0.2:17700", Suffrage: "Nonvoter"},
		),
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", NodeId: "brood", Name: "web"},
		}},
	}
	s := newTestServer(t, client)

	// The tree's facts are rendered in the OVERVIEW's Combs section:
	// the new rail replaced the sidebar's tree, and the Combs section
	// is where a Comb's own row, membership, and cells live now.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The new rail replaced the sidebar's Comb tree, so the tree's
	// facts now live in the Combs section of the overview: each Comb
	// links to its own page, its MEMBERSHIP is shown, and its cells
	// link to their own pages.
	//
	// The tree's data is unchanged - the same combCauseIndex-shaped
	// gather, the same membership - only where it is rendered moved.
	for _, want := range []string{
		"/host/brood", // a Comb links to its own page
		"/host/drone",
		"badge-suffrage", // membership is shown
		"Voter",
		"Nonvoter",
		"/vms/vm-1", // and a cell links to its own page
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the rendered page does not contain %q", want)
		}
	}
}
