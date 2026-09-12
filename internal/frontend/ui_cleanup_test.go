package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestNewJailFormSectionsAndReplicaChoices(t *testing.T) {
	for _, known := range []bool{true, false} {
		client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-b"}}
		if known {
			client.statusResp.KnownNodeIds = []string{"node-a", "node-b"}
		}
		rec := httptest.NewRecorder()
		newTestServer(t, client).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jails/new", nil))
		body := rec.Body.String()
		for _, want := range []string{
			"<legend>Identity</legend>", "<legend>Placement</legend>",
			"<legend>Root filesystem</legend>", `name="base_archive_name"`,
			`name="base_template"`, `hx-target="#create-error"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("known=%v: missing %s", known, want)
			}
		}
		if known {
			for _, want := range []string{`<select name="replica_node_id"`, `<option value="node-b" selected>node-b</option>`, `<option value="">None</option>`} {
				if !strings.Contains(body, want) {
					t.Errorf("missing picker content %s", want)
				}
			}
		} else if !strings.Contains(body, `<input type="text" name="replica_node_id"`) {
			t.Error("missing manual replica fallback when membership is unavailable")
		}
	}
}

func TestMachineSectionNavigationPreservesPanels(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t, &fakeClient{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/machine", nil))
	body := rec.Body.String()
	for _, id := range []string{"operations", "networking", "workloads", "cluster", "security", "exposure"} {
		if strings.Count(body, `id="machine-`+id+`"`) != 1 || !strings.Contains(body, `href="#machine-`+id+`"`) {
			t.Errorf("missing or duplicate navigation target %s", id)
		}
	}
	for _, id := range []string{"node-services", "assumption-tuning", "nodeconfig", "uplink", "machine-vms", "resource-scope", "bhyve-config", "jail-provisioning", "hast", "quota", "peer-forwarding", "tls", "origin-ca", "internal-security", "cloudflare-config"} {
		if strings.Count(body, `id="`+id+`-panel"`) != 1 {
			t.Errorf("missing or duplicate HTMX target %s", id)
		}
	}
	if strings.Index(body, "<h3>Apiary services</h3>") > strings.Index(body, "<h3>Network interface</h3>") {
		t.Error("service controls should precede configuration panels")
	}
}

func TestAssumptionScopeSuggestionsKeepManualEntry(t *testing.T) {
	for _, nodes := range [][]string{nil, {"node-a", "node-b"}} {
		rec := httptest.NewRecorder()
		client := &fakeClient{statusResp: &rpcpb.StatusResponse{KnownNodeIds: nodes}}
		newTestServer(t, client).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assumption-register", nil))
		body := rec.Body.String()
		for _, want := range []string{`<input name="scope" list="claim-scopes"`, `<option value="colony">`, `class="form-actions"`} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %s", want)
			}
		}
		for _, node := range nodes {
			if !strings.Contains(body, `<option value="hive:`+node+`">`) {
				t.Errorf("missing scope for %s", node)
			}
		}
	}
}
