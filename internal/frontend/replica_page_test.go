package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
)

// The Storage > Replication page (handoff section 6).
//
// IT IS READ-ONLY. There is no mutating control anywhere on it, and
// the handoff forbids new mutating endpoints in this phase. The test
// pins that, because a page that looked read-only and was not would be
// worse than one that never existed.

// TestReplicaPageIsReadOnly pins that the page carries no form that
// could change anything.
func TestReplicaPageIsReadOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t, &fakeClient{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/replication", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<form") {
		t.Error("the Replication page carries a form; it is meant to be read-only")
	}
	if strings.Contains(body, "method=\"post\"") {
		t.Error("the Replication page carries a POST")
	}
}

// TestReplicaPageIsViewerReadable pins that a Viewer can read it:
// seeing the Colony's replication state is not a privileged act.
func TestReplicaPageIsViewerReadable(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1", NodeId: "brood"}}}}
	s, err := NewServer(client, fakeAuthenticator{user: "viewer", pass: "secret"},
		map[string]manager.Role{"viewer": manager.RoleViewer}, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("viewer", manager.RoleViewer)
	req := httptest.NewRequest(http.MethodGet, "/replication", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - a Viewer may read the replication state", rec.Code)
	}
}

// TestReplicaPageCountsUnreplicatedCells pins the page's honest
// reporting: a cell with no replica is not a replication FAILURE, and
// it is counted and said so rather than being silently dropped.
//
// The page's whole job is to say which cells have redundancy and which
// do not. A page that listed only the paired cells would answer only
// half the question.
func TestReplicaPageCountsUnreplicatedCells(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
		{Id: "vm-1", NodeId: "brood", ReplicaNodeId: "drone"},
		{Id: "vm-2", NodeId: "brood"},
	}}}
	rec := httptest.NewRecorder()
	newTestServer(t, client).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/replication", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "vm-1") {
		t.Error("the page does not list the cell that HAS a replica")
	}
	if !strings.Contains(body, "1 other cell") {
		t.Errorf("the page does not report the one unreplicated cell: %s", body)
	}
}
