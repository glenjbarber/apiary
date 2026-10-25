package restshim

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// ADR-0160's REST scrape endpoints.
//
// The tests are about what a monitoring consumer actually reads, because
// the REST shape is what ADR-0161's Zabbix template consumes.

// TestMonitoringHealthEndpointRendersTheReport checks the cluster
// endpoint's shape.
func TestMonitoringHealthEndpointRendersTheReport(t *testing.T) {
	client := &fakeClient{
		monitoringClusterResp: &rpcpb.GetMonitoringClusterResponse{
			LocalNodeId: "node-a",
			Health: &rpcpb.MonitoringHealthStatus{
				Status: "healthy", ObservedUnix: 1700000000, Available: true,
			},
			Nodes: []*rpcpb.MonitoringNodeReport{{
				NodeId: "node-a",
				Health: &rpcpb.MonitoringHealthStatus{
					Status: "healthy", ObservedUnix: 1700000000, Available: true,
				},
				Mem: &rpcpb.MemStats{TotalBytes: 1024, FreeBytes: 512},
			}},
		},
	}
	s := NewServer(client)

	req := httptest.NewRequest(http.MethodGet, "/v1/monitoring/health", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The known_statuses obligation travels with every response, so a
	// consumer can discharge it mechanically.
	for _, want := range []string{"known_statuses", "healthy", "observed_unix", "local_node_id"} {
		if !strings.Contains(body, want) {
			t.Errorf("the monitoring response does not contain %q: %s", want, body)
		}
	}
}

// TestMonitoringNodesEndpointOmitsAMeasurementThatWasNotTaken is the
// property that matters most to a monitoring consumer.
//
// A measurement that could not be taken is ABSENT from the JSON, not
// present as zero. A monitoring system that reads "memory available:
// 0" during an outage will page an operator about a healthy machine.
func TestMonitoringNodesEndpointOmitsAMeasurementThatWasNotTaken(t *testing.T) {
	client := &fakeClient{
		monitoringNodesResp: &rpcpb.GetMonitoringNodesResponse{
			LocalNodeId: "node-a",
			Nodes: []*rpcpb.MonitoringNodeReport{{
				NodeId: "node-a",
				Health: &rpcpb.MonitoringHealthStatus{
					Status: "unknown", Available: false, Detail: "memory could not be read",
				},
				// A zero-valued MemStats, which is what managerd sends
				// for a memory reading it could not take.
				Mem:         &rpcpb.MemStats{TotalBytes: 0, FreeBytes: 0},
				Unavailable: []string{"memory: could not be read"},
			}},
		},
	}
	s := NewServer(client)

	req := httptest.NewRequest(http.MethodGet, "/v1/monitoring/nodes", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// The REST shape omits the measurement entirely when managerd says
	// the subsystem failed, so the JSON carries no "mem" key at all.
	var decoded struct {
		Nodes []struct {
			NodeID string `json:"node_id"`
			Health struct {
				Status    string `json:"status"`
				Available bool   `json:"available"`
				Detail    string `json:"detail"`
			} `json:"health"`
			Mem         *monitoringMem `json:"mem"`
			Unavailable []string       `json:"unavailable"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(decoded.Nodes))
	}
	node := decoded.Nodes[0]
	if node.Mem != nil {
		t.Errorf("mem = %+v, want it absent - a measurement that was not taken must not be rendered as zero", node.Mem)
	}
	if node.Health.Available {
		t.Error("the row is reported available, but its measurement was not taken")
	}
	if len(node.Unavailable) == 0 {
		t.Error("the failed subsystem is not named in unavailable")
	}
}
