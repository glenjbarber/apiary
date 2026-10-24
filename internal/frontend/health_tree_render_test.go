package frontend

import (
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestCommandCenterRendersHealthTreeAndBrowserContract(t *testing.T) {
	body := renderClusterOverview(t, &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "brood", KnownNodeIds: []string{"brood", "drone", "sting"}}})
	for _, required := range []string{
		`id="cluster-health-tree" data-poll-interval="10000" data-stale-after="30"`,
		`fetch('/cluster-health.json'`, `window.setInterval(poll, interval)`, `window.setInterval(tick, 1000)`,
		`new Set(['cluster:lab3.apiary.work'])`, `data-state="DOWN"`, `data-state="UNKNOWN"`,
		`UNKNOWN means no valid observation; DOWN means a running guest was observed stopped.`,
		`Refresh failed; showing last valid observation`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("rendered page missing tree contract %q", required)
		}
	}
}
