package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestMachineManagerdRow_NamesTheCheckoutForForceRestart pins the advice the
// Machine page gives for the one service the UI refuses to restart.
//
// The gap this exists to close was found the hard way. The row said "use
// make force-restart or service apiary_managerd restart on the node" and
// named neither the directory nor the consequence. Two operators then
// differ about what the command does and where it lives: the target needs
// the checkout, because it calls scripts/record-forced-restart.sh, and
// running it restarts raftd as well as managerd. Advice read in a browser,
// at a distance from any shell, that omits both is advice that produces a
// "no rule to make target" on a node that has the target.
func TestMachineManagerdRow_NamesTheCheckoutForForceRestart(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		listNodeServicesResp: &rpcpb.ListNodeServicesResponse{Services: []*rpcpb.NodeService{
			{Name: "apiary_raftd", Status: "running", Enabled: true, Restartable: true},
			// managerd is the one service RestartNodeService will not
			// accept, which is what selects the refusal branch.
			{Name: "apiary_managerd", Status: "running", Enabled: true, Restartable: false},
		}},
	}

	rec := httptest.NewRecorder()
	newTestServer(t, client).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/machine", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Cannot be restarted from here") {
		t.Fatalf("machine page missing the managerd self-restart refusal, got: %s", body)
	}
	if strings.Contains(body, `hx-post="/machine/services/apiary_managerd/restart"`) {
		t.Error("the managerd row must not offer a restart button; the service is not restartable from inside itself")
	}

	// Scope the assertions to the managerd row itself, not the page. The
	// Cloudflare panel on this same page names the same two commands, so a
	// whole-page check passes on the other panel's words and stops testing
	// the row at all. A mutation that deleted the checkout wording from the row and
	// left it in the panel survived exactly that way once already.
	row := body[strings.Index(body, "Cannot be restarted from here"):]
	if j := strings.Index(row, "</tr>"); j >= 0 {
		row = row[:j]
	}
	for _, want := range []string{
		"service apiary_managerd restart",
		"make force-restart",
		"source checkout",
		"Makefile",
		"scripts/record-forced-restart.sh",
		"also restarts raftd",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("managerd row advice missing %q, got: %s", want, row)
		}
	}

	// The narrower command has to come first. This row exists because an
	// operator wants managerd restarted, and make force-restart takes raftd
	// down with it, which is a bigger commitment than the one they asked
	// for. Naming the two in the other order invites the larger action.
	plain := strings.Index(row, "service apiary_managerd restart")
	forced := strings.Index(row, "make force-restart")
	if plain < 0 || forced < 0 || plain > forced {
		t.Errorf("the managerd-only command must be offered before make force-restart, got offsets plain=%d forced=%d", plain, forced)
	}
}

// TestCloudflarePanel_NamesTheCheckoutForForceRestart is the same defect on
// the second surface, and it is where a worse one turned up.
//
// The Origin CA advice exists twice, once in machine.html for the main
// Machine page and once in machine_sections.html for /machine/security, and
// the two had drifted. The main page's copy claimed the panel "restarts
// managerd after a successful replacement". Nothing does that:
// IssueOriginCertificate sets RestartScheduled false on purpose, and the
// comment above it records that the old automatic restart was removed
// because a self-restart kills the process issuing it. So the page an
// operator is most likely to be looking at told them the restart was taken
// care of, the certificate did not take effect, and nothing said why.
//
// The advice is now one named template used by both pages, so the two
// cannot drift again, and this test pins what it says and, just as
// importantly, what it must not say.
func TestCloudflarePanel_NamesTheCheckoutForForceRestart(t *testing.T) {
	for _, path := range []string{"/machine", "/machine/security"} {
		t.Run(path, func(t *testing.T) {
			client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"}}
			rec := httptest.NewRecorder()
			newTestServer(t, client).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()

			anchor := "Cloudflare Origin CA certificates"
			i := strings.Index(body, anchor)
			if i < 0 {
				t.Fatalf("%s missing the %s panel, got: %s", path, anchor, body)
			}
			panel := body[i:]
			if j := strings.Index(panel, "</p>"); j >= 0 {
				panel = panel[:j]
			}
			for _, want := range []string{
				"service apiary_managerd restart",
				"make force-restart",
				"source checkout",
				"Makefile",
				"also restarts raftd",
			} {
				if !strings.Contains(panel, want) {
					t.Errorf("%s Cloudflare advice missing %q, got: %s", path, want, panel)
				}
			}
			// The regression this test exists for. A panel that says
			// managerd restarts itself is worse than one that says
			// nothing: the operator stops looking for the step that
			// never happened.
			for _, forbidden := range []string{
				"restarts managerd",
				"will be restarted",
				"automatically",
			} {
				if strings.Contains(panel, forbidden) {
					t.Errorf("%s Cloudflare advice contains %q; nothing restarts managerd on the operator's behalf", path, forbidden)
				}
			}
			plain := strings.Index(panel, "service apiary_managerd restart")
			forced := strings.Index(panel, "make force-restart")
			if plain < 0 || forced < 0 || plain > forced {
				t.Errorf("%s: the managerd-only command must be offered before make force-restart, got offsets plain=%d forced=%d", path, plain, forced)
			}
		})
	}
}
