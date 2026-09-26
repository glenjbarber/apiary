package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestColonyLeaderIndicatorRendersOnPages is the end-to-end check that the
// header indicator actually reaches the HTML on real pages, and that it
// carries the observed node ID rather than a hardcoded one. The nodes here
// are deliberately not the cluster the author happened to be looking at -
// if any of these strings could be satisfied by a baked-in leader, the test
// would be worthless.
func TestColonyLeaderIndicatorRendersOnPages(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId:    "brood.lab3.home.arpa",
		RaftReachable:    true,
		RaftState:        "Follower",
		RaftLeaderId:     "sting.lab3.home.arpa",
		RaftNodeId:       "brood.lab3.home.arpa",
		RaftIsLeader:     false,
		RaftLastLogIndex: 126,
		RaftAppliedIndex: 126,
	}}
	s := newTestServer(t, client)

	for _, path := range []string{"/", "/vms", "/images", "/networks", "/jails"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "colony-leader") {
				t.Fatalf("page %s has no Colony leader indicator", path)
			}
			if !strings.Contains(body, "sting.lab3.home.arpa") {
				t.Errorf("page %s does not show the observed leader ID", path)
			}
			// It must say which node produced the reading, since one raftd's
			// view is not a cluster-wide agreement.
			if !strings.Contains(body, "read by brood.lab3.home.arpa") {
				t.Errorf("page %s does not attribute the reading to the node that made it", path)
			}
		})
	}
}

// TestColonyLeaderIndicatorReportsUnknownWhenRaftdUnreachable is the most
// important rendering assertion: when raftd cannot be reached, the header must
// say the leader is unknown. Rendering "No leader" there would state a fact
// nobody observed - and on a page an operator is reading at a glance during an
// incident, that is the difference between a diagnosis and a false alarm.
func TestColonyLeaderIndicatorReportsUnknownWhenRaftdUnreachable(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "brood.lab3.home.arpa",
		RaftReachable: false,
		RaftError:     "raft: connect: connection refused",
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "Leader unknown") {
		t.Errorf("expected 'Leader unknown' when raftd is unreachable, got: %s", body)
	}
	if strings.Contains(body, ">No leader<") {
		t.Error("page claims 'No leader' from an unobserved reading")
	}
	// The underlying reason has to reach the operator, or the indicator is
	// just a shrug.
	if !strings.Contains(body, "connection refused") {
		t.Error("page does not surface managerd's raft_error")
	}
}

// TestColonyLeaderIndicatorSurvivesUnreachableManagerd covers the other
// unobserved path: managerd itself answering with an error. The page must
// still render, still with the indicator, and still saying unknown.
func TestColonyLeaderIndicatorSurvivesUnreachableManagerd(t *testing.T) {
	client := &fakeClient{statusErr: errLeaderTest}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/images", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the page must still render)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Leader unknown") {
		t.Errorf("expected 'Leader unknown' when managerd errors, got: %s", body)
	}
	if !strings.Contains(body, "leaderTestBoom") {
		t.Error("page does not surface the managerd error that caused the unknown verdict")
	}
}

// TestColonyLeaderIndicatorMarksLocalLeader checks the "this Comb" cue and
// that it is withheld when the leader is somewhere else.
func TestColonyLeaderIndicatorMarksLocalLeader(t *testing.T) {
	local := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "brood.lab3.home.arpa",
		RaftReachable: true,
		RaftState:     "Leader",
		RaftIsLeader:  true,
		RaftNodeId:    "brood.lab3.home.arpa",
		RaftLeaderId:  "brood.lab3.home.arpa",
	}}
	rec := httptest.NewRecorder()
	newTestServer(t, local).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/images", nil))
	if !strings.Contains(rec.Body.String(), `class="colony-leader-here"`) {
		t.Error("a local leader does not render the 'this Comb' cue")
	}

	remote := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "brood.lab3.home.arpa",
		RaftReachable: true,
		RaftState:     "Follower",
		RaftIsLeader:  false,
		RaftNodeId:    "brood.lab3.home.arpa",
		RaftLeaderId:  "drone.lab3.home.arpa",
	}}
	rec = httptest.NewRecorder()
	newTestServer(t, remote).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/images", nil))
	// Matched on the cue's own class, not the bare words: the account block
	// also renders "Comb <hostname>", which is a different fact entirely.
	if strings.Contains(rec.Body.String(), `class="colony-leader-here"`) {
		t.Error("a remote leader wrongly renders the 'this Comb' cue")
	}
}

// TestColonyLeaderIndicatorMarksLeaderOnCombsList checks the Combs list marks
// exactly the leader's own card, and marks none at all when leadership is
// unobserved.
func TestColonyLeaderIndicatorMarksLeaderOnCombsList(t *testing.T) {
	newClient := func(leader string) *fakeClient {
		return &fakeClient{statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "brood.lab3.home.arpa",
			RaftReachable: true,
			RaftState:     "Follower",
			RaftNodeId:    "brood.lab3.home.arpa",
			RaftLeaderId:  leader,
			KnownNodeIds:  []string{"brood.lab3.home.arpa", "drone.lab3.home.arpa", "buzz.lab3.home.arpa", "sting.lab3.home.arpa"},
		}}
	}

	rec := httptest.NewRecorder()
	newTestServer(t, newClient("drone.lab3.home.arpa")).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Colony leader") {
		t.Errorf("Combs list has no leader badge; body: %s", body)
	}
	// Matched inside the class attribute, closing quote included: the page's
	// own <style> block always contains ".is-leader" as a selector, so a
	// looser check would pass even with no card marked. A card that is also
	// unreachable carries "unreachable" between the two, hence the loose
	// prefix - the leader mark is independent of reachability by design.
	if !strings.Contains(body, `is-leader"`) {
		t.Error("Combs list has no leader card marker")
	}

	// Unobserved leadership must mark no card at all, rather than marking an
	// arbitrary one.
	rec = httptest.NewRecorder()
	newTestServer(t, newClient("")).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), `is-leader"`) {
		t.Error("a card is marked as leader from an unobserved reading")
	}
}

// TestColonyLeaderIndicatorReusesExistingStatusCall pins that the indicator
// costs a page no extra RPC on the pages that already hold a StatusResponse.
// The overview page needs a Status call for membership anyway; paying for a
// second one purely for a header badge would be wasteful, and the reuse is
// the reason withAuthFieldsFrom exists.
func TestColonyLeaderIndicatorReusesExistingStatusCall(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "brood.lab3.home.arpa",
		RaftReachable: true,
		RaftState:     "Follower",
		RaftNodeId:    "brood.lab3.home.arpa",
		RaftLeaderId:  "drone.lab3.home.arpa",
		KnownNodeIds:  []string{"brood.lab3.home.arpa"},
	}}
	s := newTestServer(t, client)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if client.statusCalls != 1 {
		t.Errorf("overview page made %d Status calls, want exactly 1 (the indicator must reuse the page's own)", client.statusCalls)
	}
	if !strings.Contains(rec.Body.String(), "drone.lab3.home.arpa") {
		t.Error("reused Status did not reach the indicator")
	}
}

// TestColonyLeaderIndicatorStatusCallCost pins the indicator's RPC cost.
//
// The indicator is a live read on every page render, never cached, so its cost
// has to be a deliberate, measured number rather than an assumption. Measured
// against this tree, the indicator adds:
//
//	zero calls on / - the landing page already holds a StatusResponse for
//	  membership and hands it over, so reuse costs it nothing;
//	exactly one call on every other page - the pages that need a
//	  StatusResponse of their own, or that are plain renders.
//
// One extra call is accepted deliberately: it is a single local gRPC round
// trip to the colocated managerd (not the four-node fan-out ClusterHealth
// does, which measures ~2.2s), and caching it would mean the header could
// name a leader that changed up to a cache-interval ago. Trading one cheap
// local read for an indicator that is never stale is the right way round for
// a fact an operator reads at a glance during an incident.
//
// This test exists so the cost cannot quietly grow. If a later change starts
// fetching Status more than once for the indicator, it fails here rather than
// in a latency profile nobody was watching.
func TestColonyLeaderIndicatorStatusCallCost(t *testing.T) {
	newClient := func() *fakeClient {
		return &fakeClient{statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "brood.lab3.home.arpa",
			RaftReachable: true,
			RaftState:     "Follower",
			RaftNodeId:    "brood.lab3.home.arpa",
			RaftLeaderId:  "sting.lab3.home.arpa",
			KnownNodeIds:  []string{"brood.lab3.home.arpa", "sting.lab3.home.arpa"},
		}}
	}

	// Reuse: the landing page's own StatusResponse feeds the indicator.
	reusing := newClient()
	rec := httptest.NewRecorder()
	newTestServer(t, reusing).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if reusing.statusCalls != 1 {
		t.Errorf("landing page made %d Status calls, want 1 (indicator must reuse the page's own)", reusing.statusCalls)
	}
	if !strings.Contains(rec.Body.String(), "sting.lab3.home.arpa") {
		t.Error("reused Status did not reach the indicator")
	}

	// Fresh read: pages with no StatusResponse of their own get exactly one.
	for _, path := range []string{"/apikeys", "/users", "/trace"} {
		fresh := newClient()
		rec := httptest.NewRecorder()
		newTestServer(t, fresh).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if fresh.statusCalls != 1 {
			t.Errorf("%s made %d Status calls, want exactly 1", path, fresh.statusCalls)
		}
		if !strings.Contains(rec.Body.String(), "sting.lab3.home.arpa") {
			t.Errorf("the indicator is missing on %s", path)
		}
	}
}

// TestColonyLeaderIndicatorNotFetchedForFragments guards the other side of the
// cost decision. An htmx fragment does not include the shared header, so
// nothing in it can show the indicator; those call sites use
// withAuthFieldsForFragment and must not pay a Status RPC for a value that is
// thrown away.
//
// Tested directly rather than through a route: the point is which helper a
// fragment call site reaches for, and driving it through an endpoint would
// only prove that one particular route happens to avoid the fetch.
func TestColonyLeaderIndicatorNotFetchedForFragments(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "brood.lab3.home.arpa",
		RaftReachable: true,
		RaftState:     "Follower",
		RaftNodeId:    "brood.lab3.home.arpa",
		RaftLeaderId:  "sting.lab3.home.arpa",
	}}
	s := newTestServer(t, client)
	req := httptest.NewRequest(http.MethodPost, "/networks", nil)

	pd := s.withAuthFieldsForFragment(req, pageData{})

	if client.statusCalls != 0 {
		t.Errorf("withAuthFieldsForFragment made %d Status calls, want 0 - a fragment cannot render the header", client.statusCalls)
	}
	if pd.ColonyLeader.State != "" {
		t.Errorf("ColonyLeader.State = %q, want the zero value; a fragment must not carry an unrendered indicator", pd.ColonyLeader.State)
	}
	// The session/role fields it does exist for must still be filled in -
	// this helper replaces withAuthFields, it does not skip it.
	if pd.HiveID == "" {
		t.Error("withAuthFieldsForFragment did not fill in HiveID")
	}
	if pd.AuthEnabled {
		t.Error("withAuthFieldsForFragment did not fill in AuthEnabled")
	}
}

// TestColonyLeaderIndicatorOnEveryPage guards the one property withAuthFields
// was chosen for: no page can accidentally ship without the indicator, because
// every full-page render funnels through it.
func TestColonyLeaderIndicatorOnEveryPage(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "brood.lab3.home.arpa",
		RaftReachable: true,
		RaftState:     "Follower",
		RaftNodeId:    "brood.lab3.home.arpa",
		RaftLeaderId:  "drone.lab3.home.arpa",
		KnownNodeIds:  []string{"brood.lab3.home.arpa", "drone.lab3.home.arpa"},
	}}
	s := newTestServer(t, client)

	for _, path := range []string{
		"/", "/vms", "/jails", "/images", "/networks", "/trace",
		"/assumptions", "/simulate", "/invariants", "/why-not",
		"/recovery-handbook", "/apikeys", "/users",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "drone.lab3.home.arpa") {
				t.Errorf("page %s does not carry the Colony leader indicator", path)
			}
		})
	}
}

var errLeaderTest = leaderTestError("leaderTestBoom")

type leaderTestError string

func (e leaderTestError) Error() string { return string(e) }
