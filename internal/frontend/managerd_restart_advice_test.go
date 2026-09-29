package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestMachineManagerdRow_OffersNoRestartAndNoProse pins the Machine
// page's service row for the one daemon the UI refuses to restart.
//
// This test used to assert the opposite, and the change is the owner's
// decision rather than a repair. The row used to carry a paragraph
// explaining that managerd cannot restart itself, that
// `service apiary_managerd restart` is enough on the node, and that
// `apiaryctl force-restart` also takes raftd down. That text was
// written over two rounds to fix a real defect - it had once named a
// source checkout, which does not exist on a Comb - and the fix was
// correct as advice. It is still correct, and the owner read it and
// judged it worthless in the place it sat: a cell in a status table,
// read at a glance, by someone who had not decided to restart
// anything. Prose nobody asked for is still noise, however true it
// is, and a table cell that renders three sentences of narrative for
// one row and nothing for the others is a table whose column no longer
// means what its header says.
//
// So the advice is gone from the ROW, and what remains here is the part
// that is a real invariant rather than a copy of the manual:
//
//   - the row still offers no restart control, which is a safety
//     property and not an editorial one. A control here would let an
//     operator stop the daemon that is serving them this page, and
//     nothing on this page would bring it back.
//   - the row renders no prose at all, so the deleted text cannot
//     creep back in a reworded form.
//
// The guidance itself is not lost, deliberately. It is still on the
// page, in the Cloudflare Origin CA panel, which is the one place an
// operator is actually told to go and restart managerd, and it is
// still asserted by TestCloudflarePanel_NamesASourceFreeCommand in
// this same file. That test failing is how a deletion that actually
// lost the advice would be caught; this one failing is how a
// reintroduction would be.
func TestMachineManagerdRow_OffersNoRestartAndNoProse(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		listNodeServicesResp: &rpcpb.ListNodeServicesResponse{Services: []*rpcpb.NodeService{
			{Name: "apiary_raftd", Status: "running", Enabled: true, Restartable: true},
			// managerd is the one service RestartNodeService will not
			// accept, which is what makes this the row under test.
			{Name: "apiary_managerd", Status: "running", Enabled: true, Restartable: false},
		}},
	}

	rec := httptest.NewRecorder()
	newTestServer(t, client).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/machine", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Scope to the managerd row itself. The Cloudflare panel on this same
	// page names these same commands, so a whole-page check would pass on
	// the other panel's words and stop testing this row at all - a
	// mutation that deleted the wording from the row and left it in the
	// panel has already survived exactly that once.
	i := strings.Index(body, "<code>apiary_managerd</code>")
	if i < 0 {
		t.Fatalf("machine page has no managerd service row, got: %s", body)
	}
	row := body[i:]
	if j := strings.Index(row, "</tr>"); j >= 0 {
		row = row[:j]
	}

	if strings.Contains(row, `hx-post="/machine/services/apiary_managerd/restart"`) {
		t.Errorf("the managerd row must not offer a restart control; the service is not restartable from inside itself, and acting here would stop the daemon serving this page. Got row: %s", row)
	}
	for _, forbidden := range []string{
		"Cannot be restarted from here",
		"service apiary_managerd restart",
		"apiaryctl force-restart",
		"two-daemon",
		"root shell",
	} {
		if strings.Contains(row, forbidden) {
			t.Errorf("the managerd row carries prose again (%q); the action cell is for controls, and the restart guidance belongs on the one panel that tells an operator to go and do it. Got row: %s", forbidden, row)
		}
	}

	// The row must still be a row. A deletion that took the service
	// name, or the whole table, with it would satisfy every assertion
	// above and leave an operator unable to see that managerd is even
	// running, which is the one thing this table is for.
	for _, want := range []string{"running", "enabled"} {
		if !strings.Contains(row, want) {
			t.Errorf("managerd row lost its %q status badge, got: %s", want, row)
		}
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
