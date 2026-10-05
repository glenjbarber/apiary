package frontend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestAssessReplicaFreshness(t *testing.T) {
	complete := []hastObservationView{
		{Role: "primary", Status: "complete", Replication: "memsync", ObservedAt: "2026-09-24T12:00:00Z"},
		{Role: "secondary", Status: "complete", Replication: "memsync", ObservedAt: "2026-09-24T12:00:00Z"},
	}
	tests := []struct {
		name         string
		observations []hastObservationView
		want         string
	}{
		{name: "complete is point-in-time evidence", observations: complete, want: "complete-observed"},
		{name: "missing observation is unknown", observations: complete[:1], want: "unknown"},
		{name: "transport error is unknown", observations: []hastObservationView{{Error: "offline"}, complete[1]}, want: "unknown"},
		{name: "missing timestamp is unknown", observations: []hastObservationView{{Role: "primary", Status: "complete", Replication: "memsync"}, complete[1]}, want: "unknown"},
		{name: "wrong role is degraded", observations: []hastObservationView{{Role: "secondary", Status: "complete", Replication: "memsync", ObservedAt: "now"}, complete[1]}, want: "degraded"},
		{name: "noncomplete is degraded", observations: []hastObservationView{complete[0], {Role: "secondary", Status: "degraded", Replication: "memsync", ObservedAt: "now"}}, want: "degraded"},
		{name: "mode mismatch is unknown", observations: []hastObservationView{complete[0], {Role: "secondary", Status: "complete", Replication: "async", ObservedAt: "now"}}, want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := assessReplicaFreshness(tt.observations)
			if got != tt.want {
				t.Fatalf("assessReplicaFreshness() = %q, want %q", got, tt.want)
			}
		})
	}
}

// freshnessHASTClient answers GetLocalHASTResourceStatus for THIS node
// with an observation the test chooses, including a chosen observation
// time. fakeClient's own default is a fixed refusal, which would make
// every row an error and leave nothing for an age to attach to.
type freshnessHASTClient struct {
	*fakeClient

	resp    *rpcpb.GetLocalHASTResourceStatusResponse
	respErr error
}

func (c *freshnessHASTClient) GetLocalHASTResourceStatus(context.Context, *rpcpb.GetLocalHASTResourceStatusRequest, ...grpc.CallOption) (*rpcpb.GetLocalHASTResourceStatusResponse, error) {
	if c.respErr != nil {
		return nil, c.respErr
	}
	return c.resp, nil
}

// freshnessPeer is a peer that also answers peerHASTStatusClient, so the
// remote end of the HAST pair is reachable at all. It answers per
// resource name, which is how the two ends are told apart.
type freshnessPeer struct {
	*fakePeerHostStatsClient

	byResource map[string]*rpcpb.GetLocalHASTResourceStatusResponse
}

func (p *freshnessPeer) GetLocalHASTResourceStatus(_ context.Context, _ string, resourceName string) (*rpcpb.GetLocalHASTResourceStatusResponse, error) {
	return p.byResource[resourceName], nil
}

func vmDetailFreshnessPage(t *testing.T, client rpcpb.ManagerServiceClient, peers peerHostStatsClient) string {
	t.Helper()
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /vms/vm-1 = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// freshnessRow returns the one replica-freshness table row naming this
// node, as a list of its cell values. Scoping to the row rather than
// counting "unknown" cells across the page matters: the page is full of
// other unknown cells belonging to other panels, so a page-wide count
// passes against a table that renders nothing at all.
func freshnessRow(t *testing.T, body, nodeID string) []string {
	t.Helper()
	idx := strings.Index(body, "<td>"+nodeID+"</td>")
	if idx == -1 {
		t.Fatalf("no freshness row for %s on the page:\n%s", nodeID, body)
	}
	row := body[idx:]
	if end := strings.Index(row, "</tr>"); end != -1 {
		row = row[:end]
	}
	var cells []string
	// Split on the tag NAME rather than a bare "<td>", because the age
	// cell carries a data-age-of attribute and is therefore not a bare
	// tag. Attributes are then stripped so each value is the cell's text
	// and nothing else. The helper stays correct whether or not a cell
	// has attributes, which is the whole reason that one has one.
	for _, chunk := range strings.Split(row, "<td")[1:] {
		if gt := strings.Index(chunk, ">"); gt != -1 {
			chunk = chunk[gt+1:]
		}
		if end := strings.Index(chunk, "</td>"); end != -1 {
			cells = append(cells, chunk[:end])
		}
	}
	return cells
}

// A node that WAS observed must render a relative "Last updated" age. The
// whole point of the column is that an operator reads how stale the
// evidence is without doing the subtraction themselves, so the cell must
// carry the age and not a copy of the absolute timestamp beside it.
func TestReplicaFreshnessView_RendersRelativeAgeForAnObservedNode(t *testing.T) {
	observed := time.Now().Add(-2 * time.Minute)
	client := &freshnessHASTClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
				Id: "vm-1", Name: "database", NodeId: "node-a", ReplicaNodeId: "node-b",
			}},
		},
		resp: &rpcpb.GetLocalHASTResourceStatusResponse{
			Role: "primary", ResourceStatus: "complete", Replication: "memsync",
			ObservedAtUnix: observed.Unix(),
		},
	}
	peer := &freshnessPeer{
		fakePeerHostStatsClient: &fakePeerHostStatsClient{},
		byResource: map[string]*rpcpb.GetLocalHASTResourceStatusResponse{
			// replicaFreshness builds the resource name as resourceType + "-" +
			// the raft ID, so a VM whose ID is "vm-1" is asked about as
			// "vm-vm-1". Keying the fake on the raft ID alone silently
			// returns no observation, which is a test that passes for the
			// wrong reason.
			"vm-vm-1": {
				Role: "secondary", ResourceStatus: "complete", Replication: "memsync",
				ObservedAtUnix: observed.Unix(),
			},
		},
	}
	body := vmDetailFreshnessPage(t, client, peer)

	if !strings.Contains(body, "Last updated") {
		t.Fatalf("freshness table has no Last updated column:\n%s", body)
	}
	for _, nodeID := range []string{"node-a", "node-b"} {
		cells := freshnessRow(t, body, nodeID)
		if len(cells) != 7 {
			t.Fatalf("%s row has %d cells, want 7 (the header gained the age column, so every row must have gained a cell too): %q", nodeID, len(cells), cells)
		}
		if got := cells[6]; got != "2m ago" {
			t.Errorf("%s Last updated = %q, want %q", nodeID, got, "2m ago")
		}
		// The absolute column is unchanged, and the two must not be
		// derived from each other in the template.
		if got, want := cells[5], observed.UTC().Format(time.RFC3339); got != want {
			t.Errorf("%s Observed = %q, want %q", nodeID, got, want)
		}
	}
}

// The age belongs to an observation, not to the column header. A node
// that reported a view with no timestamp has nothing to age, and must
// render the unknown case - never a zero age, which would read as "just
// observed" for a node whose evidence did not say when it was taken.
func TestReplicaFreshnessView_UnobservedNodeHasNoAgeAndNeverReadsAsCurrent(t *testing.T) {
	client := &freshnessHASTClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
				Id: "vm-1", Name: "database", NodeId: "node-a", ReplicaNodeId: "node-b",
			}},
		},
		// No ObservedAtUnix at all. The node reported a real hastctl
		// view, so this is not the error path - it is the subtler one
		// where the view is genuine and only its clock is missing.
		resp: &rpcpb.GetLocalHASTResourceStatusResponse{Role: "primary", ResourceStatus: "complete", Replication: "memsync"},
	}
	body := vmDetailFreshnessPage(t, client, nil)

	cells := freshnessRow(t, body, "node-a")
	if len(cells) != 7 {
		t.Fatalf("node-a row has %d cells, want 7: %q", len(cells), cells)
	}
	if got := cells[5]; got != "unknown" {
		t.Errorf("Observed = %q, want unknown for a response carrying no timestamp", got)
	}
	if got := cells[6]; got != "unknown" {
		t.Errorf("Last updated = %q, want unknown - there is no observation to age, and a zero age would read as current", got)
	}
}

// A node that could not be reached at all is the other half of the same
// rule: it has no observation, so it has no age, and it must not borrow
// one from the node that did answer.
func TestReplicaFreshnessView_UnreachableNodeHasNoAge(t *testing.T) {
	client := &freshnessHASTClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
				Id: "vm-1", Name: "database", NodeId: "node-a", ReplicaNodeId: "node-b",
			}},
		},
		resp: &rpcpb.GetLocalHASTResourceStatusResponse{
			Role: "primary", ResourceStatus: "complete", Replication: "memsync",
			ObservedAtUnix: time.Now().Add(-2 * time.Minute).Unix(),
		},
	}
	// No peers configured at all, so node-b cannot be asked.
	body := vmDetailFreshnessPage(t, client, nil)

	if got := freshnessRow(t, body, "node-a")[6]; got != "2m ago" {
		t.Errorf("the observed end lost its age when its peer failed: %q", got)
	}
	if got := freshnessRow(t, body, "node-b")[6]; got != "unknown" {
		t.Errorf("an unreachable node rendered an age of %q, want unknown - it was never observed, so it has no age", got)
	}
}
