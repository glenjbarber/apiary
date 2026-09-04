package frontend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakeISOPeerClient is a fakePeerHostStatsClient with canned per-address
// ListISOs responses, for testing currentClusterISOs's cluster-wide
// merge without a real peer managerd.
type fakeISOPeerClient struct {
	mu sync.Mutex

	listISOsByAddr map[string]*rpcpb.ListISOsResponse
}

func (f *fakeISOPeerClient) HostStats(context.Context, string) (*rpcpb.HostStatsResponse, error) {
	return &rpcpb.HostStatsResponse{}, nil
}

func (f *fakeISOPeerClient) ListAssumptionResults(context.Context, string, *rpcpb.ListAssumptionResultsRequest) (*rpcpb.ListAssumptionResultsResponse, error) {
	return &rpcpb.ListAssumptionResultsResponse{}, nil
}

func (f *fakeISOPeerClient) Status(context.Context, string) (*rpcpb.StatusResponse, error) {
	return &rpcpb.StatusResponse{}, nil
}

func (f *fakeISOPeerClient) GetLocalNetworkBridgeStatus(context.Context, string, string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error) {
	return &rpcpb.GetLocalNetworkBridgeStatusResponse{}, nil
}

func (f *fakeISOPeerClient) ListISOs(_ context.Context, addr string) (*rpcpb.ListISOsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if resp, ok := f.listISOsByAddr[addr]; ok {
		return resp, nil
	}
	return &rpcpb.ListISOsResponse{}, nil
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

	req := httptest.NewRequest(http.MethodGet, "/vms/new", nil)
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

// TestCurrentClusterISOs_PresentEverywhereHasNoMissingNodes confirms a
// file already on every known node renders with an empty MissingNodes
// list, so the create-VM form's cue JS never flags it as needing a
// fetch regardless of which node is selected.
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

	req := httptest.NewRequest(http.MethodGet, "/vms/new", nil)
	rows, errMsg := s.currentClusterISOs(req)
	if errMsg != "" {
		t.Fatalf("currentClusterISOs() error: %s", errMsg)
	}
	if len(rows) != 1 || len(rows[0].MissingNodes) != 0 {
		t.Errorf("expected one row present everywhere with no missing nodes, got: %+v", rows)
	}
}

// TestNewVMPage_EmbedsClusterWideImageListAndMissingCueData is a thin
// end-to-end check that the create-VM form actually renders every
// known node's images (not just the local node's) and embeds the
// missing-node data isoMissingByNode/new_vm.html's own JS needs to show
// a "will be fetched from a peer" cue as the Node ID picker changes.
func TestNewVMPage_EmbedsClusterWideImageListAndMissingCueData(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
		listISOsResp: &rpcpb.ListISOsResponse{Isos: []*rpcpb.ISOInfo{
			{Name: "a.iso", Sha256: "aaa"},
		}},
	}
	peers := &fakeISOPeerClient{listISOsByAddr: map[string]*rpcpb.ListISOsResponse{
		"freebsd-apiary.apiary.work:17700": {Isos: []*rpcpb.ISOInfo{
			{Name: "b.iso", Sha256: "bbb"},
		}},
	}}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/vms/new", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `value="a.iso"`) || !strings.Contains(body, `value="b.iso"`) {
		t.Errorf("expected both a.iso (local) and b.iso (peer-only) as image options, got: %s", body)
	}
	// a.iso only exists on apiarium (missing from freebsd-apiary); b.iso
	// only exists on freebsd-apiary (missing from apiarium) - the
	// embedded cue data must reflect each one's real gap.
	if !strings.Contains(body, `"a.iso":["freebsd-apiary"]`) {
		t.Errorf("expected a.iso's missing-node list to include freebsd-apiary, got: %s", body)
	}
	if !strings.Contains(body, `"b.iso":["apiarium"]`) {
		t.Errorf("expected b.iso's missing-node list to include apiarium, got: %s", body)
	}
}
