package frontend

import (
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestTracePage_RendersOrderedEvidence(t *testing.T) {
	client := &fakeClient{
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{
			Id: "vm-1", Name: "frontend", NodeId: "hive-a",
		}}},
		traceResp: &rpcpb.TraceCellPathResponse{
			Cell:    &rpcpb.VMDefinition{Id: "vm-1", Name: "frontend", NodeId: "hive-a"},
			Network: &rpcpb.NetworkDefinition{Id: "net-1", Name: "services"},
			Status:  rpcpb.PathTraceStatus_PATH_TRACE_STATUS_BLOCKED,
			Summary: "First blocker: Owner-Hive bridge: bridge is down",
			Steps: []*rpcpb.PathTraceStep{{
				Stage: "Owner-Hive bridge", Status: rpcpb.PathTraceStatus_PATH_TRACE_STATUS_BLOCKED,
				Summary: "Managed-network bridge is down", Evidence: "owner Hive local bridge status",
				Explanation: "apnet-1234 is down",
			}},
			NonAtomic: true,
		},
	}
	s, err := NewServer(client, nil, nil, nil, "", "", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest("GET", "/trace?cell_id=vm-1&destination=10.60.0.20&protocol=tcp&port=443", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Cell Path Trace", "frontend to 10.60.0.20", "blocked", "Owner-Hive bridge", "apnet-1234 is down", "The destination was not contacted"} {
		if !strings.Contains(body, want) {
			t.Errorf("response does not contain %q", want)
		}
	}
	if client.lastTraceReq.GetCellId() != "vm-1" || client.lastTraceReq.GetPort() != 443 {
		t.Fatalf("TraceCellPath request = %+v", client.lastTraceReq)
	}
}

func TestTracePage_InvalidPortDoesNotCallManager(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{}}
	s, err := NewServer(client, nil, nil, nil, "", "", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest("GET", "/trace?cell_id=vm-1&destination=example.com&protocol=tcp&port=70000", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "Destination port must be a number from 1 to 65535") {
		t.Fatalf("response = %s", w.Body.String())
	}
	if client.lastTraceReq != nil {
		t.Fatalf("TraceCellPath called with invalid port: %+v", client.lastTraceReq)
	}
}
