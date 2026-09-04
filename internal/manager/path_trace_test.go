package manager

import (
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/pathtrace"
)

func natResult(owner, uplink string, status rpcpb.AssumptionStatus, observedAt int64) *rpcpb.AssumptionResult {
	return &rpcpb.AssumptionResult{
		Key: &rpcpb.AssumptionKey{
			Kind:             rpcpb.AssumptionKind_ASSUMPTION_KIND_NAT_UPLINK_DEFAULT_ROUTE,
			ObservedByNodeId: owner, DependencyId: uplink,
		},
		Status: status, LastObservedAtUnix: observedAt,
	}
}

func TestPathTraceNATEvidence_UsesNewestUplinkObservation(t *testing.T) {
	resp := &rpcpb.ListAssumptionResultsResponse{Latest: []*rpcpb.AssumptionResult{
		natResult("hive-a", "em0", rpcpb.AssumptionStatus_ASSUMPTION_STATUS_UNKNOWN, 10),
		natResult("hive-a", "bridge0", rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE, 20),
	}}
	status, _ := pathTraceNATEvidence("hive-a", resp)
	if status != pathtrace.StatusClear {
		t.Fatalf("status = %q, want clear from newest observation", status)
	}
}

func TestPathTraceNATEvidence_EqualTimestampConflictIsUnknown(t *testing.T) {
	resp := &rpcpb.ListAssumptionResultsResponse{Latest: []*rpcpb.AssumptionResult{
		natResult("hive-a", "em0", rpcpb.AssumptionStatus_ASSUMPTION_STATUS_FALSE, 20),
		natResult("hive-a", "bridge0", rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE, 20),
	}}
	status, detail := pathTraceNATEvidence("hive-a", resp)
	if status != pathtrace.StatusUnknown || !strings.Contains(detail, "conflicting") {
		t.Fatalf("status = %q detail = %q, want conflicting unknown", status, detail)
	}
}
