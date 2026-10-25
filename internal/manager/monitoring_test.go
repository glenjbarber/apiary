package manager

import (
	"context"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/health"
	"github.com/glenjbarber/apiary/internal/hoststats"
	"google.golang.org/grpc"
)

// ADR-0160's monitoring reports.
//
// The tests below are about the two properties that matter most to a
// consumer, because both are ways a monitoring system reports a
// healthy cluster during an outage:
//
//  1. A MISSING OBSERVATION IS NOT ZERO.
//  2. A NODE THAT WAS NEVER OBSERVED IS NOT HEALTHY.
//
// They are about the properties rather than the shape of the
// implementation, because the shape is what will change and the
// properties are what a consumer depends on.

// monitoringRaft is a fake raft client that reports a fixed membership.
//
// The monitoring report reads its node set from raft membership, so a
// test needs one. It is a fake rather than a live raftd because the
// thing under test is the REPORT, not the membership - and because a
// live raftd per test is expensive.
type monitoringRaft struct {
	internalpb.RaftInternalClient
	servers []*internalpb.ServerInfo
}

func (c *monitoringRaft) Status(context.Context, *internalpb.StatusRequest, ...grpc.CallOption) (*internalpb.StatusResponse, error) {
	return &internalpb.StatusResponse{
		IsLeader:    true,
		NodeId:      "node-a",
		StateDigest: "digest",
		Servers:     c.servers,
	}, nil
}

// monitoringServer builds a Server whose own host statistics are
// whatever the test wants, and whose raft membership is fixed.
func monitoringServer(t *testing.T, nodeID string, snap *hoststats.Snapshot, known []string) *Server {
	t.Helper()
	servers := make([]*internalpb.ServerInfo, 0, len(known))
	for _, id := range known {
		servers = append(servers, &internalpb.ServerInfo{Id: id, Address: "10.0.0.2:17700", Suffrage: "Voter"})
	}
	raft := &RaftClient{client: &monitoringRaft{servers: servers}}
	s := NewServer(raft, nodeID, nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.statsGather = func(context.Context) *hoststats.Snapshot { return snap }
	return s
}

// TestMonitoringClusterReportsAnUnobservedNodeAsNotHealthy is the
// central property.
//
// A node with no peer forwarding configured was never observed. It must
// not be reported as healthy, because a monitoring system that reports
// "healthy" for a node it never reached is worse than one that reports
// nothing.
func TestMonitoringClusterReportsAnUnobservedNodeAsNotHealthy(t *testing.T) {
	s := monitoringServer(t, "node-a", &hoststats.Snapshot{}, []string{"node-a", "node-b"})

	resp, err := s.GetMonitoringCluster(context.Background(), &rpcpb.GetMonitoringClusterRequest{})
	if err != nil {
		t.Fatalf("GetMonitoringCluster: %v", err)
	}

	// The local node IS observed - it is answering now.
	if len(resp.GetNodes()) == 0 {
		t.Fatal("no node rows at all")
	}
	for _, node := range resp.GetNodes() {
		if node.GetNodeId() == "node-a" {
			continue
		}
		if health.Status(node.GetHealth().GetStatus()) == health.StatusHealthy {
			t.Errorf("node %q was never observed but is reported healthy", node.GetNodeId())
		}
		if node.GetHealth().GetAvailable() {
			t.Errorf("node %q was never observed but is reported available", node.GetNodeId())
		}
	}
}

// TestMonitoringClusterDoesNotReportAMissingMeasurementAsZero is the
// other central property.
//
// A measurement that could not be taken must be ABSENT. Rendering it
// as zero would page an operator about a healthy machine during an
// outage.
func TestMonitoringClusterDoesNotReportAMissingMeasurementAsZero(t *testing.T) {
	// A snapshot with no memory figures at all - the subsystem failed
	// to report, which is what hoststats.Snapshot.Errors records.
	snap := &hoststats.Snapshot{Errors: []string{"memory: could not read"}}
	s := monitoringServer(t, "node-a", snap, []string{"node-a", "node-b"})

	resp, err := s.GetMonitoringNodes(context.Background(), &rpcpb.GetMonitoringNodesRequest{})
	if err != nil {
		t.Fatalf("GetMonitoringNodes: %v", err)
	}
	if len(resp.GetNodes()) == 0 {
		t.Fatal("no node rows at all")
	}
	node := resp.GetNodes()[0]
	if node.GetMem() != nil {
		t.Errorf("memory was not observed but is reported as %+v - a missing measurement must be absent, not zero", node.GetMem())
	}
	// And the failure is named, so a consumer can say WHY.
	if len(node.GetUnavailable()) == 0 {
		t.Errorf("the failed subsystem is not named in unavailable: %+v", node)
	}
}

// TestMonitoringNodeCarriesTheObservationTimestamp checks that a row
// says when it was observed.
//
// A consumer polling every sixty seconds must be able to tell a fresh
// reading from a stale one without guessing from its own clock.
func TestMonitoringNodeCarriesTheObservationTimestamp(t *testing.T) {
	s := monitoringServer(t, "node-a", &hoststats.Snapshot{}, []string{"node-a", "node-b"})

	resp, err := s.GetMonitoringNodes(context.Background(), &rpcpb.GetMonitoringNodesRequest{})
	if err != nil {
		t.Fatalf("GetMonitoringNodes: %v", err)
	}
	if len(resp.GetNodes()) == 0 {
		t.Fatal("no node rows at all")
	}
	node := resp.GetNodes()[0]
	if node.GetHealth().GetObservedUnix() <= 0 {
		t.Errorf("an observed node carries observed_unix = %d, want a real timestamp", node.GetHealth().GetObservedUnix())
	}
	if time.Since(time.Unix(node.GetHealth().GetObservedUnix(), 0)) > time.Minute {
		t.Errorf("observed_unix is %d, which is not a recent observation", node.GetHealth().GetObservedUnix())
	}
}

// TestMonitoringClusterAndNodesAgree checks that the two RPCs cannot
// disagree.
//
// They share one gathering path, so a consumer polling the nodes and a
// consumer polling the cluster see the same rows at the same moment.
func TestMonitoringClusterAndNodesAgree(t *testing.T) {
	s := monitoringServer(t, "node-a", &hoststats.Snapshot{}, []string{"node-a", "node-b"})

	cluster, err := s.GetMonitoringCluster(context.Background(), &rpcpb.GetMonitoringClusterRequest{})
	if err != nil {
		t.Fatalf("GetMonitoringCluster: %v", err)
	}
	nodes, err := s.GetMonitoringNodes(context.Background(), &rpcpb.GetMonitoringNodesRequest{})
	if err != nil {
		t.Fatalf("GetMonitoringNodes: %v", err)
	}

	if len(cluster.GetNodes()) != len(nodes.GetNodes()) {
		t.Fatalf("the cluster view has %d nodes and the nodes view has %d; they must agree", len(cluster.GetNodes()), len(nodes.GetNodes()))
	}
	for i, node := range cluster.GetNodes() {
		other := nodes.GetNodes()[i]
		if node.GetNodeId() != other.GetNodeId() {
			t.Fatalf("row %d is %q in the cluster view and %q in the nodes view", i, node.GetNodeId(), other.GetNodeId())
		}
		if node.GetHealth().GetStatus() != other.GetHealth().GetStatus() {
			t.Errorf("node %q is %q in the cluster view and %q in the nodes view", node.GetNodeId(), node.GetHealth().GetStatus(), other.GetHealth().GetStatus())
		}
	}
}

// TestMonitoringClusterVerdictAggregatesTheRows checks that the
// cluster verdict is computed from the rows, by the shared rule.
func TestMonitoringClusterVerdictAggregatesTheRows(t *testing.T) {
	s := monitoringServer(t, "node-a", &hoststats.Snapshot{}, []string{"node-a", "node-b"})

	// node-a is healthy, but node-b was never observed - so the Colony
	// is UNKNOWN, not healthy. This is the property that matters: a
	// Colony containing a Comb nobody could reach is not a healthy
	// Colony, and a monitoring report that said "healthy" here would be
	// reporting a healthy cluster during an outage.
	resp, err := s.GetMonitoringCluster(context.Background(), &rpcpb.GetMonitoringClusterRequest{})
	if err != nil {
		t.Fatalf("GetMonitoringCluster: %v", err)
	}
	if health.Status(resp.GetHealth().GetStatus()) != health.StatusUnknown {
		t.Errorf("cluster health = %q, want unknown - one of the two nodes was never observed", resp.GetHealth().GetStatus())
	}

	// With only the healthy local node in the Colony, it IS healthy.
	only := monitoringServer(t, "node-a", &hoststats.Snapshot{}, []string{"node-a"})
	onlyResp, err := only.GetMonitoringCluster(context.Background(), &rpcpb.GetMonitoringClusterRequest{})
	if err != nil {
		t.Fatalf("GetMonitoringCluster: %v", err)
	}
	if health.Status(onlyResp.GetHealth().GetStatus()) != health.StatusHealthy {
		t.Errorf("cluster health = %q, want healthy with one healthy node", onlyResp.GetHealth().GetStatus())
	}

	// And a verdict is ALWAYS one of the five states - never empty. An
	// empty status would be read as "nothing to say", which is not one
	// of the five.
	if onlyResp.GetHealth().GetStatus() == "" {
		t.Error("the cluster verdict is empty; it must always carry one of the five states")
	}
}
