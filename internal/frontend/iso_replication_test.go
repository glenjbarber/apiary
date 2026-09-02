package frontend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
)

// fakeISOPeerClient is a fakePeerHostStatsClient for ADR-0040's ISO
// replication tests: canned ListISOs responses per peer address, plus
// call recording for ReplicateISO so a test can assert exactly which
// peer was dialed with which arguments.
type fakeISOPeerClient struct {
	mu sync.Mutex

	listISOsByAddr map[string]*rpcpb.ListISOsResponse

	replicateCalls []struct{ addr, name, sourceNodeID string }
	replicateErr   error
}

func (f *fakeISOPeerClient) HostStats(context.Context, string) (*rpcpb.HostStatsResponse, error) {
	return &rpcpb.HostStatsResponse{}, nil
}

func (f *fakeISOPeerClient) ListISOs(_ context.Context, addr string) (*rpcpb.ListISOsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if resp, ok := f.listISOsByAddr[addr]; ok {
		return resp, nil
	}
	return &rpcpb.ListISOsResponse{}, nil
}

func (f *fakeISOPeerClient) ReplicateISO(_ context.Context, addr, name, sourceNodeID string) (*rpcpb.ReplicateISOResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replicateCalls = append(f.replicateCalls, struct{ addr, name, sourceNodeID string }{addr, name, sourceNodeID})
	if f.replicateErr != nil {
		return nil, f.replicateErr
	}
	return &rpcpb.ReplicateISOResponse{Name: name}, nil
}

// TestCurrentClusterISOs_MergesPresentAndMissing verifies the per-node
// ListISOs fetches (local via s.client, remote via s.peers - mirroring
// nodeHostStats's own dispatch) are merged by (name, sha256) into rows
// tracking exactly which known nodes have and lack each file.
func TestCurrentClusterISOs_MergesPresentAndMissing(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
		listISOsResp: &rpcpb.ListISOsResponse{Isos: []*rpcpb.ISOInfo{
			{Name: "a.iso", Sha256: "aaa", SizeBytes: 100},
			{Name: "b.iso", Sha256: "bbb", SizeBytes: 200},
		}},
	}
	peers := &fakeISOPeerClient{listISOsByAddr: map[string]*rpcpb.ListISOsResponse{
		"freebsd-apiary.apiary.work:17700": {Isos: []*rpcpb.ISOInfo{
			{Name: "b.iso", Sha256: "bbb", SizeBytes: 200},
			{Name: "c.iso", Sha256: "ccc", SizeBytes: 300},
		}},
	}}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/images", nil)
	rows, errMsg := s.currentClusterISOs(req)
	if errMsg != "" {
		t.Fatalf("currentClusterISOs() error: %s", errMsg)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}

	byName := map[string]isoRowView{}
	for _, row := range rows {
		byName[row.Name] = row
	}

	a := byName["a.iso"]
	if !equalStrSlices(a.PresentNodes, []string{"apiarium"}) || !equalStrSlices(a.MissingNodes, []string{"freebsd-apiary"}) {
		t.Errorf("a.iso: present=%v missing=%v, want present=[apiarium] missing=[freebsd-apiary]", a.PresentNodes, a.MissingNodes)
	}
	b := byName["b.iso"]
	if !equalStrSlices(b.PresentNodes, []string{"apiarium", "freebsd-apiary"}) || len(b.MissingNodes) != 0 {
		t.Errorf("b.iso: present=%v missing=%v, want present on both, missing none", b.PresentNodes, b.MissingNodes)
	}
	c := byName["c.iso"]
	if !equalStrSlices(c.PresentNodes, []string{"freebsd-apiary"}) || !equalStrSlices(c.MissingNodes, []string{"apiarium"}) {
		t.Errorf("c.iso: present=%v missing=%v, want present=[freebsd-apiary] missing=[apiarium]", c.PresentNodes, c.MissingNodes)
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandleReplicateISO_LocalTargetUsesLocalClient verifies that
// copying onto the frontend's own colocated node dispatches through
// s.client directly, never touching s.peers - mirroring
// nodeHostStats's own local-vs-peer split.
func TestHandleReplicateISO_LocalTargetUsesLocalClient(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
		listISOsResp: &rpcpb.ListISOsResponse{Isos: []*rpcpb.ISOInfo{
			{Name: "a.iso", Sha256: "aaa"},
		}},
	}
	peers := &fakeISOPeerClient{}
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator}
	s, err := NewServer(client, fakeAuthenticator{user: "ops", pass: "secret"}, roleMap, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/isos/a.iso/replicate/apiarium", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if client.lastReplicateISOReq == nil {
		t.Fatal("expected s.client.ReplicateISO to be called for the local target, but it wasn't")
	}
	if got := client.lastReplicateISOReq.GetName(); got != "a.iso" {
		t.Errorf("replicate request name = %q, want a.iso", got)
	}
	if len(peers.replicateCalls) != 0 {
		t.Errorf("expected no peer dial for a local target, got: %+v", peers.replicateCalls)
	}
}

// TestHandleReplicateISO_RemoteTargetUsesPeer verifies that copying
// onto a different node dials that node's managerd via s.peers with
// the resolved source node ID, rather than calling the local client.
func TestHandleReplicateISO_RemoteTargetUsesPeer(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
		listISOsResp: &rpcpb.ListISOsResponse{Isos: []*rpcpb.ISOInfo{
			{Name: "a.iso", Sha256: "aaa"},
		}},
	}
	peers := &fakeISOPeerClient{}
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator}
	s, err := NewServer(client, fakeAuthenticator{user: "ops", pass: "secret"}, roleMap, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/isos/a.iso/replicate/freebsd-apiary", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if len(peers.replicateCalls) != 1 {
		t.Fatalf("expected exactly one peer ReplicateISO call, got: %+v", peers.replicateCalls)
	}
	call := peers.replicateCalls[0]
	if call.addr != "freebsd-apiary.apiary.work:17700" {
		t.Errorf("peer addr = %q, want freebsd-apiary.apiary.work:17700", call.addr)
	}
	if call.name != "a.iso" {
		t.Errorf("peer replicate name = %q, want a.iso", call.name)
	}
	if call.sourceNodeID != "apiarium" {
		t.Errorf("peer replicate source = %q, want apiarium (the only node reporting the file)", call.sourceNodeID)
	}
	if client.lastReplicateISOReq != nil {
		t.Errorf("expected no local client.ReplicateISO call for a remote target, got: %+v", client.lastReplicateISOReq)
	}
}

// TestHandleReplicateISOAll_AttemptsEveryMissingNode verifies "Copy to
// all missing" loops the per-target dispatch over every node the row
// currently lacks the file on, reporting a combined error if any one
// target fails rather than stopping at the first failure.
func TestHandleReplicateISOAll_AttemptsEveryMissingNode(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary", "freebsd-apiary2"}},
		listISOsResp: &rpcpb.ListISOsResponse{Isos: []*rpcpb.ISOInfo{
			{Name: "a.iso", Sha256: "aaa"},
		}},
	}
	peers := &fakeISOPeerClient{}
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator}
	s, err := NewServer(client, fakeAuthenticator{user: "ops", pass: "secret"}, roleMap, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/isos/a.iso/replicate-all", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if len(peers.replicateCalls) != 2 {
		t.Fatalf("expected 2 peer replicate calls (one per missing node), got: %+v", peers.replicateCalls)
	}
	targets := map[string]bool{}
	for _, call := range peers.replicateCalls {
		targets[call.addr] = true
	}
	if !targets["freebsd-apiary.apiary.work:17700"] || !targets["freebsd-apiary2.apiary.work:17700"] {
		t.Errorf("expected calls to both missing nodes, got: %+v", peers.replicateCalls)
	}
}

// TestCurrentClusterISOs_PresentEverywhereHasNoMissingNodes confirms a
// file already on every known node renders with an empty MissingNodes
// list, so images.html's per-row buttons don't appear at all.
func TestCurrentClusterISOs_PresentEverywhereHasNoMissingNodes(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
		listISOsResp: &rpcpb.ListISOsResponse{Isos: []*rpcpb.ISOInfo{
			{Name: "a.iso", Sha256: "aaa"},
		}},
	}
	peers := &fakeISOPeerClient{listISOsByAddr: map[string]*rpcpb.ListISOsResponse{
		"freebsd-apiary.apiary.work:17700": {Isos: []*rpcpb.ISOInfo{
			{Name: "a.iso", Sha256: "aaa"},
		}},
	}}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/images", nil)
	rows, errMsg := s.currentClusterISOs(req)
	if errMsg != "" {
		t.Fatalf("currentClusterISOs() error: %s", errMsg)
	}
	if len(rows) != 1 || len(rows[0].MissingNodes) != 0 {
		t.Errorf("expected one row present everywhere with no missing nodes, got: %+v", rows)
	}
}

// TestImagesPage_RendersCopyButtonsForMissingNodes is a thin
// end-to-end check that the wired-up /images page actually surfaces a
// copy action for a file missing from a peer node.
func TestImagesPage_RendersCopyButtonsForMissingNodes(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
		listISOsResp: &rpcpb.ListISOsResponse{Isos: []*rpcpb.ISOInfo{
			{Name: "a.iso", Sha256: "aaa"},
		}},
	}
	peers := &fakeISOPeerClient{}
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator}
	s, err := NewServer(client, fakeAuthenticator{user: "ops", pass: "secret"}, roleMap, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodGet, "/images", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Copy to freebsd-apiary") {
		t.Errorf("expected a Copy to freebsd-apiary button, got: %s", body)
	}
}
