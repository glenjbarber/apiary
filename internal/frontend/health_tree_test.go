package frontend

import (
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/health"
)

func TestBuildHealthTreeReportsFreshnessAndAggregatesBottomUp(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	stamp := now.Add(-5 * time.Second).Unix()
	staleStamp := now.Add(-32 * time.Second).Unix()
	index := &combCauseIndex{
		VMs: []vmView{
			{ID: "vm-running", Name: "web", NodeID: "brood", DesiredState: "running", Phase: "ready", ObservedAtUnix: stamp},
			{ID: "vm-down", Name: "db", NodeID: "brood", DesiredState: "running", Phase: "stopped", ObservedAtUnix: stamp},
			{ID: "vm-stale", Name: "cache", NodeID: "drone", DesiredState: "running", Phase: "ready", ObservedAtUnix: staleStamp},
			{ID: "vm-stopped", Name: "backup", NodeID: "sting", DesiredState: "stopped", Phase: "stopped", ObservedAtUnix: stamp},
			{ID: "vm-unknown", Name: "unobserved", NodeID: "unreachable", DesiredState: "running", Phase: "ready"},
		},
		Jails: []jailView{
			{ID: "jail-error", Name: "worker", NodeID: "brood", DesiredState: "running", Phase: "error", ObservedAtUnix: stamp},
		},
	}
	nodes := []clusterNodeView{
		{NodeID: "brood.lab3.apiary.work", Reachable: true, HealthStatus: health.StatusHealthy, ReconcileSuccessUnix: stamp},
		{NodeID: "drone", Reachable: true, HealthStatus: health.StatusHealthy, ReconcileSuccessUnix: staleStamp},
		{NodeID: "sting", Reachable: true, HealthStatus: health.StatusHealthy, ReconcileSuccessUnix: stamp},
		{NodeID: "unreachable", Reachable: false, HealthStatus: health.StatusUnknown},
	}

	got := buildHealthTree(nodes, index, now)
	if got.Label != "lab3.apiary.work" || !got.DefaultExpanded {
		t.Fatalf("root = %+v, want expanded lab3.apiary.work", got)
	}
	if got.State != "DEGRADED" {
		t.Errorf("root state = %q, want DEGRADED due to a down guest", got.State)
	}
	hosts := make(map[string]healthTreeNode, len(got.Children))
	for _, host := range got.Children {
		hosts[host.Label] = host
	}
	if hosts["brood"].State != "DEGRADED" {
		t.Errorf("brood state = %q, want DEGRADED", hosts["brood"].State)
	}
	if hosts["drone"].State != "STALE" || hosts["drone"].Children[0].State != "STALE" || hosts["drone"].Children[0].ObservedAtUnix != staleStamp || hosts["drone"].Children[0].AgeSeconds != 32 {
		t.Errorf("stale host/resource = %+v / %+v", hosts["drone"], hosts["drone"].Children)
	}
	if hosts["brood"].Children[0].Level != 2 || !hosts["brood"].Children[0].Leaf || hosts["brood"].Children[0].Label != "db" {
		t.Errorf("resource metadata = %+v, want relative leaf metadata", hosts["brood"].Children[0])
	}
	if hosts["sting"].Children[0].State != "STOPPED" {
		t.Errorf("intentionally stopped VM state = %q, want STOPPED", hosts["sting"].Children[0].State)
	}
	if hosts["unreachable"].State != "UNKNOWN" || hosts["unreachable"].Children[0].State != "UNKNOWN" {
		t.Errorf("unreachable host/guest = %q / %q, want UNKNOWN for both", hosts["unreachable"].State, hosts["unreachable"].Children[0].State)
	}
	resourceStates := make(map[string]string)
	for _, resource := range hosts["brood"].Children {
		resourceStates[resource.Key] = resource.State
	}
	if resourceStates["vm:vm-running"] != "RUNNING" || resourceStates["vm:vm-down"] != "DOWN" || resourceStates["jail:jail-error"] != "DEGRADED" {
		t.Errorf("brood resource states = %v", resourceStates)
	}
}

func TestRelativeHostNameOnlyRemovesApiaryClusterSuffix(t *testing.T) {
	if got := relativeHostName("drone.lab3.apiary.work"); got != "drone" {
		t.Errorf("relativeHostName() = %q, want drone", got)
	}
	if got := relativeHostName("drone.other.example"); got != "drone.other.example" {
		t.Errorf("relativeHostName() changed unrelated name to %q", got)
	}
}

func TestResourceTreeDoesNotMarkStoppedDesiredGuestDown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	host := clusterNodeView{Reachable: true, HealthStatus: health.StatusHealthy, ReconcileSuccessUnix: now.Unix()}
	got := resourceTreeNode("vm", "stopped", "stopped", "stopped", "stopped", now.Unix(), host, now)
	if got.State != "STOPPED" {
		t.Fatalf("stopped VM state = %q, want STOPPED", got.State)
	}
}

func TestBuildHealthTreeMarksMissingInventoryAndOrphanHostUnknown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	index := &combCauseIndex{
		VMError: "raft list unavailable",
		VMs:     []vmView{{ID: "orphan-vm", Name: "lost", NodeID: "retired-host", DesiredState: "running", Phase: "ready"}},
	}
	got := buildHealthTree([]clusterNodeView{{NodeID: "brood", Reachable: true, HealthStatus: health.StatusHealthy, ReconcileSuccessUnix: now.Unix()}}, index, now)
	if got.State != "UNKNOWN" {
		t.Errorf("root state = %q, want UNKNOWN when inventory is incomplete", got.State)
	}
	hosts := make(map[string]healthTreeNode, len(got.Children))
	for _, host := range got.Children {
		hosts[host.Label] = host
	}
	if hosts["brood"].State != "UNKNOWN" {
		t.Errorf("healthy host with incomplete inventory = %q, want UNKNOWN", hosts["brood"].State)
	}
	if hosts["retired-host"].State != "UNKNOWN" || len(hosts["retired-host"].Children) != 1 || hosts["retired-host"].Children[0].State != "UNKNOWN" {
		t.Errorf("orphan host subtree = %+v, want unknown host and guest", hosts["retired-host"])
	}
}
