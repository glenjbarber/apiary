package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The panel is now a shared partial rendered by two different pages, so
// "it renders" has to be checked on both. A partial that parses but is
// never reached on the jail page would leave the jail's ages frozen at
// whatever the server rendered, with nothing failing. Both pages are
// asserted for the script and the data attribute together, because
// either one alone is satisfied by a panel that is broken in the other
// respect.
func TestReplicaFreshnessPanelRendersOnBothDetailPages(t *testing.T) {
	observed := time.Now().Add(-3 * time.Minute)
	resp := &rpcpb.GetLocalHASTResourceStatusResponse{
		Role: "primary", ResourceStatus: "complete", Replication: "memsync",
		ObservedAtUnix: observed.Unix(),
	}
	vmClient := &freshnessHASTClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
				Id: "vm-1", Name: "database", NodeId: "node-a", ReplicaNodeId: "node-b",
			}},
		},
		resp: resp,
	}
	jailClient := &freshnessHASTClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			getJailResp: &rpcpb.GetJailResponse{Found: true, Jail: &rpcpb.JailDefinition{
				Id: "jail-1", Name: "web-1", NodeId: "node-a", ReplicaNodeId: "node-b",
			}},
		},
		resp: resp,
	}

	jailDetailPage := func(t *testing.T) string {
		t.Helper()
		s, err := NewServer(jailClient, nil, nil, nil, ".test", "17700", nil, false)
		if err != nil {
			t.Fatalf("NewServer() error: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/jails/jail-1", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /jails/jail-1 = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	pages := map[string]string{
		"vm detail":   vmDetailFreshnessPage(t, vmClient, nil),
		"jail detail": jailDetailPage(t),
	}
	for name, body := range pages {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{"setInterval(tick, REFRESH_MS)", `data-age-of="`} {
				if !strings.Contains(body, want) {
					t.Errorf("the %s page does not render %q - the shared panel is either not reached there, or its tick is missing", name, want)
				}
			}
			if got := freshnessRow(t, body, "node-a")[6]; got != "3m ago" {
				t.Errorf("%s page Last updated = %q, want 3m ago", name, got)
			}
		})
	}
}
