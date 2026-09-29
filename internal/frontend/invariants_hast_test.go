package frontend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// These tests are about the /invariants page's HAST rows specifically:
// the page is now the production caller of gatherHASTInvariants, so what
// the page renders IS the invariant. Every assertion below is made
// against the real embedded template's output through ServeHTTP, and
// every one is made about a badge on the SAME row as the invariant name
// - a badge elsewhere on the page is a different invariant's verdict.

// hastEvidenceClient answers GetLocalHASTResourceStatus for THIS node
// with a canned view instead of fakeClient's own fixed refusal, so a
// test can make the local end of a HAST pair say something specific.
type hastEvidenceClient struct {
	*fakeClient

	mu    sync.Mutex
	resp  *rpcpb.GetLocalHASTResourceStatusResponse
	err   error
	asked []string
}

func (c *hastEvidenceClient) GetLocalHASTResourceStatus(ctx context.Context, req *rpcpb.GetLocalHASTResourceStatusRequest, _ ...grpc.CallOption) (*rpcpb.GetLocalHASTResourceStatusResponse, error) {
	c.mu.Lock()
	c.asked = append(c.asked, req.GetResourceName())
	resp, err := c.resp, c.err
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *hastEvidenceClient) askedNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

// hastEvidencePeer is a fakePeerHostStatsClient that ALSO answers the
// peer-side GetLocalHASTResourceStatus, so it satisfies
// peerHASTStatusClient and the page can reach a remote end at all.
//
// blockUntil is the timeout path: a peer that never answers, only stops
// answering when its own context expires.
type hastEvidencePeer struct {
	*fakePeerHostStatsClient

	mu         sync.Mutex
	byResource map[string]*rpcpb.GetLocalHASTResourceStatusResponse
	err        error
	blockUntil bool
	asked      []string
}

func (p *hastEvidencePeer) GetLocalHASTResourceStatus(ctx context.Context, addr, resourceName string) (*rpcpb.GetLocalHASTResourceStatusResponse, error) {
	p.mu.Lock()
	p.asked = append(p.asked, addr+"/"+resourceName)
	resp, err, block := p.byResource[resourceName], p.err, p.blockUntil
	p.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *hastEvidencePeer) askedNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

// invariantRowBadge returns the badge class rendered on the row whose
// heading names this invariant - never a badge belonging to some other
// row on the same page. Fails the test if the row is not rendered at
// all, because "the invariant is missing" and "the invariant is
// unknown" are different outcomes and only one of them is a pass.
func invariantRowBadge(t *testing.T, body, name string) string {
	t.Helper()
	idx := strings.Index(body, name+" <span class=\"badge ")
	if idx == -1 {
		t.Fatalf("no %s row on the page at all:\n%s", name, body)
	}
	row := body[idx:]
	if end := strings.Index(row, "</h4>"); end != -1 {
		row = row[:end]
	}
	const marker = `class="badge `
	start := strings.Index(row, marker)
	if start == -1 {
		t.Fatalf("%s row carries no badge: %q", name, row)
	}
	rest := row[start+len(marker):]
	if end := strings.Index(rest, `"`); end != -1 {
		return rest[:end]
	}
	t.Fatalf("%s row has a malformed badge: %q", name, row)
	return ""
}

// invariantsPage renders the real /invariants page through ServeHTTP
// and returns the embedded template's own output. A nil peers is a
// frontend configured with no peer forwarding at all, which is itself a
// case the page has to survive.
func invariantsPage(t *testing.T, client rpcpb.ManagerServiceClient, peers peerHostStatsClient) string {
	t.Helper()
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/invariants", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /invariants = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func hastVM(id, name, nodeID, replica string) *rpcpb.VMDefinition {
	return &rpcpb.VMDefinition{Id: id, Name: name, NodeId: nodeID, ReplicaNodeId: replica}
}

func completeHAST(role string) *rpcpb.GetLocalHASTResourceStatusResponse {
	return &rpcpb.GetLocalHASTResourceStatusResponse{ResourceName: "", Role: role, ResourceStatus: "complete", Replication: "mailsync"}
}

// A resource with no replica has no HAST pair, so the HAST invariant has
// nothing to say about it - but the page's other results, and the
// structural section in particular, must not depend on live gathering
// having anything to gather. This is the case that would break first if
// anyone ever made the gather a prerequisite for rendering.
func TestHandleInvariantsPage_ResourceWithNoReplicaStillRendersStructuralResults(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
		listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{hastVM("1", "lonely-vm", "node-a", "")}},
	}
	body := invariantsPage(t, client, nil)

	if !strings.Contains(body, "Structural guarantees") {
		t.Fatalf("expected the structural section to render for a colony with nothing to gather:\n%s", body)
	}
	if got := invariantRowBadge(t, body, "ownership-gated-deletion"); got != "true" {
		t.Errorf("ownership-gated-deletion badge = %q, want true", got)
	}
	// A resource with no replica is out of scope for the HAST invariant,
	// not a silent pass on it: it must not appear there at all.
	if strings.Contains(body, "hast-dual-primary <span class=\"badge ") && strings.Contains(body, "(vm-1)</span>") {
		t.Errorf("a replica-less resource must not produce a hast-dual-primary row:\n%s", body)
	}
}

// The gatherer cannot be set up at all when this node does not know its
// own id - a local read could not then be told from a peer one. The
// page must still render, with the structural-only reason, rather than
// dropping the HAST rows or failing the request.
func TestHandleInvariantsPage_HASTGatherCannotBeSetUpFallsBackToNamedUnknownRows(t *testing.T) {
	client := &fakeClient{
		// No ManagerNodeId: this frontend cannot tell a local HAST read
		// from a peer one, so gathering is refused rather than guessed.
		statusResp: &rpcpb.StatusResponse{RaftReachable: true, RaftLeaderId: "node-a"},
		listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{hastVM("1", "web-1", "node-a", "node-b")}},
	}
	body := invariantsPage(t, client, nil)

	if got := invariantRowBadge(t, body, "hast-dual-primary"); got != "unknown" {
		t.Errorf("hast-dual-primary badge = %q, want unknown when the gather could not run", got)
	}
	if !strings.Contains(body, "no live role observation was gathered") {
		t.Errorf("expected the fallback row to state the true reason, got:\n%s", body)
	}
	if got := invariantRowBadge(t, body, "ownership-gated-deletion"); got != "true" {
		t.Errorf("ownership-gated-deletion badge = %q, want true - live HAST gathering must not be a prerequisite for the structural section", got)
	}
}

// Silence is not health. A peer client that cannot answer HAST questions
// at all means the end was NEVER ASKED, and an end that was never asked
// is not a checked end: unknown, with the reason naming that no query
// was made.
func TestHandleInvariantsPage_NeverAskedHASTEndRendersUnknownNotHealthy(t *testing.T) {
	client := &hastEvidenceClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{hastVM("1", "web-1", "node-a", "node-b")}},
			// The local end refused outright - asked, got no view.
		},
		resp: &rpcpb.GetLocalHASTResourceStatusResponse{Error: "hastctl: not configured"},
	}

	// A plain fakePeerHostStatsClient does not implement
	// peerHASTStatusClient at all: peer forwarding for HAST is simply
	// not available here.
	body := invariantsPage(t, client, &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{BhyveConfigured: true}})

	if got := invariantRowBadge(t, body, "hast-dual-primary"); got != "unknown" {
		t.Errorf("hast-dual-primary badge = %q, want unknown - an end nobody asked is not a passed check", got)
	}
	if !strings.Contains(body, "no query was made") {
		t.Errorf("expected the evidence to say no query was made, got:\n%s", body)
	}
	if !strings.Contains(body, "peer forwarding is not configured") {
		t.Errorf("expected the evidence to name the unreachable-end reason, got:\n%s", body)
	}
	if got := invariantRowBadge(t, body, "cell-recoverability"); got == "true" {
		t.Errorf("cell-recoverability badge = %q, want not true - the sync half was never observed", got)
	}
}

// A dead Comb must cost the page its own budget, not its verdict. The
// end that was asked and did not answer is Unknown citing the dial
// error; it is never a failure, because a Comb that cannot be reached
// has said nothing about its own hastd.
func TestHandleInvariantsPage_DeadPeerYieldsUnknownNotFailure(t *testing.T) {
	client := &hastEvidenceClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{hastVM("1", "web-1", "node-a", "node-b")}},
		},
		resp: completeHAST("primary"),
	}

	peer := &hastEvidencePeer{
		fakePeerHostStatsClient: &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{BhyveConfigured: true}},
		err:                     errors.New("dial tcp 10.90.0.96:17700: connect: connection refused"),
	}
	body := invariantsPage(t, client, peer)

	if got := invariantRowBadge(t, body, "hast-dual-primary"); got != "unknown" {
		t.Errorf("hast-dual-primary badge = %q, want unknown - a Comb that could not be reached has not failed", got)
	}
	if !strings.Contains(body, "queried, no usable answer") {
		t.Errorf("expected the evidence to distinguish asked-and-silent from never-asked, got:\n%s", body)
	}
	if !strings.Contains(body, "connection refused") {
		t.Errorf("expected the dial error to be cited as the reason, got:\n%s", body)
	}
	if strings.Contains(body, "TWO writable HAST primaries") {
		t.Errorf("an unreachable end must never be read as a confirmed primary:\n%s", body)
	}
	if got := invariantRowBadge(t, body, "cell-recoverability"); got == "true" {
		t.Errorf("cell-recoverability badge = %q, want not true - the replica's sync state was never observed", got)
	}
}

// The other direction: evidence that was actually observed, and that
// actually says the pair is broken, must render as a failure. A page
// that can only ever say "unknown" is as wrong as one that says
// "healthy" on no evidence.
func TestHandleInvariantsPage_ObservedDualPrimaryRendersAsFailure(t *testing.T) {
	client := &hastEvidenceClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{hastVM("1", "web-1", "node-a", "node-b")}},
		},
		resp: completeHAST("primary"),
	}

	peer := &hastEvidencePeer{
		fakePeerHostStatsClient: &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{}},
		// The replica node also says it holds the writable primary.
		byResource: map[string]*rpcpb.GetLocalHASTResourceStatusResponse{"vm-1": completeHAST("primary")},
	}
	body := invariantsPage(t, client, peer)

	if got := invariantRowBadge(t, body, "hast-dual-primary"); got != "false" {
		t.Errorf("hast-dual-primary badge = %q, want false - both ends reported the writable primary role:\n%s", got, body)
	}
	if !strings.Contains(body, "TWO writable HAST primaries") {
		t.Errorf("expected the dual-primary explanation to be rendered, got:\n%s", body)
	}
	// The strict resource name is what goes on the wire, both ends.
	if asked := peer.askedNames(); len(asked) != 1 || !strings.HasSuffix(asked[0], "/vm-1") {
		t.Errorf("peer was asked %v, want exactly one call for the strict resource name vm-1", asked)
	}
	if asked := client.askedNames(); len(asked) != 1 || asked[0] != "vm-1" {
		t.Errorf("local client was asked %v, want exactly one call for vm-1", asked)
	}
	// Bhyve is not configured on the replica target, confirmed by a real
	// HostStats answer: a confirmed-incapable destination is a confirmed
	// failure, and the sync half cannot rescue it.
	if got := invariantRowBadge(t, body, "cell-recoverability"); got != "false" {
		t.Errorf("cell-recoverability badge = %q, want false for a confirmed-incapable destination", got)
	}
}

// The converse of the case above, and the reason the wiring exists: with
// both ends observed, one primary and one secondary, the page reaches a
// real pass - and cell-recoverability's sync half now comes from live
// evidence too, so a capable destination can reach true as well.
func TestHandleInvariantsPage_ObservedHealthyPairRendersTrue(t *testing.T) {
	client := &hastEvidenceClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{hastVM("1", "web-1", "node-a", "node-b")}},
		},
		resp: completeHAST("primary"),
	}

	peer := &hastEvidencePeer{
		fakePeerHostStatsClient: &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{BhyveConfigured: true}},
		byResource:              map[string]*rpcpb.GetLocalHASTResourceStatusResponse{"vm-1": completeHAST("secondary")},
	}
	body := invariantsPage(t, client, peer)

	if got := invariantRowBadge(t, body, "hast-dual-primary"); got != "true" {
		t.Errorf("hast-dual-primary badge = %q, want true - exactly one end reported the primary role:\n%s", got, body)
	}
	if got := invariantRowBadge(t, body, "cell-recoverability"); got != "true" {
		t.Errorf("cell-recoverability badge = %q, want true - both halves of the conjunction were observed and confirmed:\n%s", got, body)
	}
}

// A VM's replica node is a jail? No - the point here is narrower: a
// jail's HAST evidence is gathered and rendered exactly as a VM's, and a
// jail with no capability signal at all still reaches its sync half
// (which is why it can be true for a jail at all).
func TestHandleInvariantsPage_JailHASTEvidenceIsGatheredAndRendered(t *testing.T) {
	client := &hastEvidenceClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			listJailsResp: &rpcpb.ListJailsResponse{Jails: []*rpcpb.JailDefinition{
				{Id: "7", Name: "svc-7", NodeId: "node-a", ReplicaNodeId: "node-b"},
			}},
		},
		resp: completeHAST("primary"),
	}

	peer := &hastEvidencePeer{
		fakePeerHostStatsClient: &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{}},
		byResource:              map[string]*rpcpb.GetLocalHASTResourceStatusResponse{"jail-7": completeHAST("secondary")},
	}
	body := invariantsPage(t, client, peer)

	if !strings.Contains(body, "hast-dual-primary <span class=\"badge true\"") {
		t.Errorf("expected a true hast-dual-primary row for the jail's live evidence, got:\n%s", body)
	}
	if !strings.Contains(body, "(jail-7)</span>") {
		t.Errorf("expected the jail's HAST resource name in its row scope, got:\n%s", body)
	}
	if asked := peer.askedNames(); len(asked) != 1 || !strings.HasSuffix(asked[0], "/jail-7") {
		t.Errorf("peer was asked %v, want exactly one call for jail-7", asked)
	}
}

// A peer that never answers must cost its own budget and no more: the
// page returns inside the node-context deadlines, and every end it
// could not reach is reported as unobserved rather than assumed.
func TestHandleInvariantsPage_PermanentlyBlockingPeerDoesNotHangThePage(t *testing.T) {
	oldTimeout, oldOverall := nodeContextTimeout, nodeContextOverallTimeout
	nodeContextTimeout = 10 * time.Millisecond
	nodeContextOverallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { nodeContextTimeout, nodeContextOverallTimeout = oldTimeout, oldOverall })

	client := &hastEvidenceClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
				hastVM("1", "web-1", "node-a", "node-b"),
				hastVM("2", "web-2", "node-a", "node-b"),
			}},
		},
		resp: completeHAST("primary"),
	}

	peer := &hastEvidencePeer{
		fakePeerHostStatsClient: &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{BhyveConfigured: true}},
		blockUntil:              true,
	}

	done := make(chan string, 1)
	go func() { done <- invariantsPage(t, client, peer) }()

	var body string
	select {
	case body = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handleInvariantsPage did not return within 3s of a permanently-blocking peer - the HAST fan-out is not bounded")
	}
	if got := invariantRowBadge(t, body, "hast-dual-primary"); got != "unknown" {
		t.Errorf("hast-dual-primary badge = %q, want unknown - a timed-out query is silence, not a failure", got)
	}
	if !strings.Contains(body, "context deadline exceeded") {
		t.Errorf("expected the timeout to be cited as the reason, got:\n%s", body)
	}
	if got := invariantRowBadge(t, body, "ownership-gated-deletion"); got != "true" {
		t.Errorf("ownership-gated-deletion badge = %q, want true even when the gather times out", got)
	}
}

// The fan-out is capped, and the cap is not a silent truncation: a
// resource past the bound is still named, as Unknown with the true
// reason, rather than quietly dropped from the page.
func TestHandleInvariantsPage_HASTFanOutIsCappedAndNamesWhatItSkipped(t *testing.T) {
	var vms []*rpcpb.VMDefinition
	for _, id := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"} {
		vms = append(vms, hastVM(id, "web-"+id, "node-a", "node-b"))
	}
	client := &hastEvidenceClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			listResp:   &rpcpb.ListVMsResponse{Vms: vms},
		},
		resp: completeHAST("primary"),
	}

	peer := &hastEvidencePeer{
		fakePeerHostStatsClient: &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{BhyveConfigured: true}},
		byResource: map[string]*rpcpb.GetLocalHASTResourceStatusResponse{
			"vm-1": completeHAST("secondary"),
		},
	}
	body := invariantsPage(t, client, peer)

	rows := strings.Count(body, "hast-dual-primary <span class=\"badge ")
	if rows != len(vms) {
		t.Errorf("rendered %d hast-dual-primary rows for %d replica-backed resources - a resource past the bound must still be named", rows, len(vms))
	}
	if asked := peer.askedNames(); len(asked) > hastGatherLimit {
		t.Errorf("asked %d peers, want at most the hastGatherLimit of %d - one page render must not become a dial storm", len(asked), hastGatherLimit)
	}
	if asked := peer.askedNames(); len(asked) != hastGatherLimit {
		t.Errorf("asked %d peers, want exactly the %d the cap allows", len(asked), hastGatherLimit)
	}
	if !strings.Contains(body, "GetLocalHASTResourceStatus (not queried)") {
		t.Errorf("expected the skipped resources to be reported as never queried, got:\n%s", body)
	}
	// The one resource the peer actually answered resolves from evidence;
	// the rest, which came back with no view, stay unknown.
	if got := invariantRowBadge(t, body, "hast-dual-primary"); got != "true" {
		t.Errorf("hast-dual-primary badge for vm-1 = %q, want true - the only end that reported a role said secondary", got)
	}
}
