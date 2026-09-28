package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestMachineManagerdRow_NamesASourceFreeCommand pins the advice the
// Machine page gives for the one service the UI refuses to restart.
//
// The gap this exists to close was found the hard way, in two rounds.
// The row first said "use make force-restart or service apiary_managerd
// restart on the node" and named neither the directory nor the
// consequence; advice read in a browser, at a distance from any shell,
// that omits both produces a "no rule to make target" on a node that has
// no checkout to have that target in. The second round was the deeper
// one: the honest fix at the time was to name the checkout, and naming a
// checkout only helps if there is one. There is not. force-restart is an
// installed command now (ADR-0136), so the row names that - and the
// assertions below treat checkout language as a defect rather than a
// clarification, because on a Comb every way of naming one is a way of
// sending an operator to a directory that is not there.
func TestMachineManagerdRow_NamesASourceFreeCommand(t *testing.T) {
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
	// the row at all. A mutation that deleted the wording from the row and
	// left it in the panel survived exactly that way once already.
	row := body[strings.Index(body, "Cannot be restarted from here"):]
	if j := strings.Index(row, "</tr>"); j >= 0 {
		row = row[:j]
	}
	for _, want := range []string{
		"service apiary_managerd restart",
		"apiaryctl force-restart",
		// The consequence, which the row must not omit: this is a
		// two-daemon act, not the one the operator asked for.
		"also restarts raftd",
		// And that it needs nothing but a shell, which is what makes the
		// advice actionable on a machine with no checkout on it.
		"needs nothing but a root shell",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("managerd row advice missing %q, got: %s", want, row)
		}
	}

	// The regression this half exists for. Every one of these names a way
	// of running the command that does not exist on a Comb, and every one
	// of them was true of this row at some point.
	for _, forbidden := range []string{
		"make force-restart",
		"source checkout",
		"Makefile",
		"record-forced-restart",
		"scripts/",
	} {
		if strings.Contains(row, forbidden) {
			t.Errorf("managerd row advice contains %q: force-restart is installed on the Comb now, and the row must not send an operator looking for a checkout", forbidden)
		}
	}

	// The narrower command has to come first. This row exists because an
	// operator wants managerd restarted, and force-restart takes raftd
	// down with it, which is a bigger commitment than the one they asked
	// for. Naming the two in the other order invites the larger action.
	plain := strings.Index(row, "service apiary_managerd restart")
	forced := strings.Index(row, "apiaryctl force-restart")
	if plain < 0 || forced < 0 || plain > forced {
		t.Errorf("the managerd-only command must be offered before force-restart, got offsets plain=%d forced=%d", plain, forced)
	}
}

// TestCloudflarePanel_NamesASourceFreeCommand is the same defect on the
// second surface, and it is where a worse one turned up.
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
func TestCloudflarePanel_NamesASourceFreeCommand(t *testing.T) {
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
				"apiaryctl force-restart",
				"also restarts raftd",
				"needs nothing but a root shell",
			} {
				if !strings.Contains(panel, want) {
					t.Errorf("%s Cloudflare advice missing %q, got: %s", path, want, panel)
				}
			}
			// Two regressions this test exists for.
			//
			// A panel that says managerd restarts itself is worse than
			// one that says nothing: the operator stops looking for the
			// step that never happened.
			//
			// And a panel that names a checkout is advice that cannot be
			// followed on the machine it is read on, because a Comb has
			// no checkout. Both were true of this text at some point, and
			// both read as help.
			for _, forbidden := range []string{
				"restarts managerd",
				"will be restarted",
				"automatically",
				"make force-restart",
				"source checkout",
				"Makefile",
				"record-forced-restart",
			} {
				if strings.Contains(panel, forbidden) {
					t.Errorf("%s Cloudflare advice contains %q; nothing restarts managerd on the operator's behalf, and the command it names is installed on the Comb", path, forbidden)
				}
			}
			plain := strings.Index(panel, "service apiary_managerd restart")
			forced := strings.Index(panel, "apiaryctl force-restart")
			if plain < 0 || forced < 0 || plain > forced {
				t.Errorf("%s: the managerd-only command must be offered before force-restart, got offsets plain=%d forced=%d", path, plain, forced)
			}
		})
	}
}
